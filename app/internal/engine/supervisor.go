package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	gvisoripv4 "gvisor.dev/gvisor/pkg/tcpip/network/ipv4" // TUN ICMP-echo toggle (vendored gVisor patch)

	"clashvless/internal/store"
	"clashvless/internal/tun"
	"clashvless/internal/xray"
)

// Probe is a per-node reachability sample shown in the UI.
type Probe struct {
	Name      string
	Server    string
	Whitelist bool
	Latency   time.Duration
	OK        bool
	Speed     float64 // Mbps from the last speedtest (0 = not yet tested)
}

// SlotStatus is the live failover state of one slot (its own port + best main).
type SlotStatus struct {
	Name   string
	Port   int
	Tier   int    // 1/2/3 = active tier; 0 = down
	Entry  string // entry node name ("" for T1 direct)
	Main   string // active final-exit main name
	Egress time.Duration
	Err    string
	Note   string
	IsTun  bool // this slot is the one the TUN bridge attaches to
}

// Status is an immutable snapshot of supervisor state handed to the UI. The
// top-level Tier/Entry/Main/Egress/Err/Note mirror the TUN slot (the primary),
// so single-slot views keep working; Slots carries every slot's state.
type Status struct {
	Tier      int    // 1/2/3 = active tier; 0 = down (TUN slot)
	Entry     string // entry node name ("" for T1 direct)
	Main      string // active final-exit main name
	Egress    time.Duration
	Err       string
	Note      string // hysteresis hint, e.g. "↑ T1 available (2/3)"
	TunErr    string // TUN bring-up error, if any
	Slots     []SlotStatus
	Country   []Probe
	Bypass    []Probe
	UpdatedAt time.Time
}

// Supervisor keeps the single local port served by the best available tier and
// re-evaluates on an interval. Hysteresis prevents flapping: it only switches
// UP after a better tier holds for UpThreshold cycles, and only switches DOWN
// after the current chain fails DownThreshold cycles.
type Supervisor struct {
	st     *store.State
	listen string // bind address for the LIVE local inbound(s)
	quiet  bool   // force xray silent (TUI-embedded daemon)

	kick chan struct{} // kicks the manage loop

	mu       sync.Mutex // guards st, status (aggregate), pools, tun fields
	status   Status
	onChange func(Status)
	onLog    func(string)

	// fastest-first pool ordering, shared by every slot runner.
	rankedCountry []*store.Node
	rankedBypass  []*store.Node

	speedMu sync.Mutex
	speeds  map[string]float64 // Mbps by node name, from the rare speed loop

	runnersMu sync.Mutex
	runners   map[string]*slotRunner // one failover runner per enabled slot, keyed by name

	// TUN mode (touched only from the manage goroutine). The bridge (tun → local
	// SOCKS on a fixed backend port) is created ONCE and never recreated in-process
	// — macOS can't recreate a utun (the kernel won't destroy it while any fd
	// lingers, and xray leaks that fd until exit). Switching the TUN slot swaps only
	// the cheap relay behind it, never the device.
	bridge         *Instance
	relay          *Instance // socks backend-port → current TUN slot's port
	relayTarget    int       // the slot port the relay currently forwards to
	tunBackendPort int       // fixed local port the bridge dials / the relay serves
	tunMgr         *tun.Manager
	tunOn          bool // OS routing currently applied (bridge may outlive this)
	tunErr         bool // last bring-up failed; don't respin until toggled off
	tunErrMsg      string
	tunMark        int32             // xray's off-tun socket mark (cached for the gateway-change re-point)
	tunHosts       map[string]string // exit domain→IP map (cached so the re-point re-decorates without DNS)
	tunSlotName    string            // the slot the relay currently targets (for live re-point)
	tunDevName     string            // the device name the bridge created
}

// slotRunner keeps one slot's best main online on the slot's own port. It is the
// former single-pool supervisor, now one of several — each with its own live
// instance, tier state, and hysteresis. Shared resources (config, node pool,
// probing, TUN) live on the parent Supervisor.
type slotRunner struct {
	sup       *Supervisor
	name      string
	mainPort  int
	entryPort int // first hop exposed here while chained (0 = disabled)
	cancel    context.CancelFunc
	kick      chan struct{}

	mu        sync.Mutex // guards the live/status fields below
	live      *Instance
	liveKey   string
	liveTier  int
	liveEntry string
	liveMain  string
	ss        SlotStatus

	// hysteresis counters — only touched from this runner's goroutine.
	upStreak   int
	failStreak int
}

type plan struct {
	tier   int
	entry  *store.Node
	main   string
	config []byte
	egress time.Duration
	key    string
}

// NewSupervisor builds a supervisor. onChange (may be nil) receives a fresh
// Status snapshot on every state change; onLog (may be nil) receives one-line
// activity events for a log view.
func NewSupervisor(st *store.State, onChange func(Status), onLog func(string)) *Supervisor {
	s := &Supervisor{
		st:       st,
		listen:   st.ListenHost(),
		kick:     make(chan struct{}, 1),
		onChange: onChange,
		onLog:    onLog,
		speeds:   map[string]float64{},
		runners:  map[string]*slotRunner{},
	}
	s.tunMgr = tun.New(s.logf)
	return s
}

func (s *Supervisor) logf(format string, a ...any) {
	if s.onLog != nil {
		s.onLog(fmt.Sprintf(format, a...))
	}
}

// --- dynamic config (read from the store under lock, so the TUI config tab
// takes effect live) ---------------------------------------------------------

func (s *Supervisor) cfgInt(get func(*store.State) int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.st)
}

// SetConfig mutates the stored config under lock (used by the TUI config tab).
func (s *Supervisor) SetConfig(mutate func(*store.State)) {
	s.mu.Lock()
	mutate(s.st)
	s.mu.Unlock()
}

func (s *Supervisor) cfgStr(get func(*store.State) string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.st)
}

// Snapshot returns the current state as JSON (for pushing to control clients).
func (s *Supervisor) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.st)
	return b
}

// Save persists the current state to disk under lock.
func (s *Supervisor) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Save()
}

// CurrentStatus returns the latest status snapshot.
func (s *Supervisor) CurrentStatus() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Supervisor) cfgBool(get func(*store.State) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.st)
}

// loglevel is the configured xray verbosity for the live (served) instance.
// When quiet (embedded in a TUI) it's forced silent — xray writes to the terminal,
// which would corrupt the alt-screen.
func (s *Supervisor) loglevel() string {
	if s.quiet {
		return "none"
	}
	return s.cfgStr(func(st *store.State) string { return st.Loglevel() })
}

// SetQuiet silences the served xray instance's logging (call before Run). Used
// when the daemon runs in-process under the TUI.
func (s *Supervisor) SetQuiet(q bool) { s.quiet = q }

func (s *Supervisor) interval() time.Duration {
	if v := s.cfgInt(func(st *store.State) int { return st.Interval }); v > 0 {
		return time.Duration(v) * time.Second
	}
	return 12 * time.Second
}

func (s *Supervisor) timeout() time.Duration {
	if v := s.cfgInt(func(st *store.State) int { return st.Timeout }); v > 0 {
		return time.Duration(v) * time.Second
	}
	return 6 * time.Second
}

func (s *Supervisor) upThresh() int {
	if v := s.cfgInt(func(st *store.State) int { return st.UpThreshold }); v > 0 {
		return v
	}
	return 3
}

func (s *Supervisor) downThresh() int {
	if v := s.cfgInt(func(st *store.State) int { return st.DownThreshold }); v > 0 {
		return v
	}
	return 2
}

const maxTryPerTier = 3

// tierBounds honours a pinned tier (PinTier 1/2/3); else, with ForceHop, skips the
// direct T1 and forces a hop (2..3); otherwise the full 1..3 cascade.
func (s *Supervisor) tierBounds() (lo, hi int) {
	if p := s.cfgInt(func(st *store.State) int { return st.PinTier }); p >= 1 && p <= 3 {
		return p, p
	}
	if s.cfgBool(func(st *store.State) bool { return st.ForceHop }) {
		return 2, 3
	}
	return 1, 3
}

// --- lifecycle ---------------------------------------------------------------

// Run drives the failover loop until ctx is cancelled, then stops serving.
func (s *Supervisor) Run(ctx context.Context) error {
	defer s.stopAllRunners()
	defer s.tunShutdown() // restore routing/DNS + close the bridge (device) on shutdown
	s.refreshPools(ctx)   // initial pool health so the first cycle has data
	go s.poolLoop(ctx)    // keep pool health fresh in the background
	go s.speedLoop(ctx)   // rare auto-speedtest of each node's throughput
	for {
		s.syncRunners(ctx) // one failover runner per enabled slot, on its own port
		s.syncTun(ctx)     // bring TUN up/down to match config (attaches to the TUN slot)
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
		case <-time.After(s.interval()):
		}
	}
}

// syncRunners starts a runner for each enabled slot and stops runners whose slot
// was removed, disabled, or re-ported. Each runner failovers independently on its
// slot's port; the TUN bridge later attaches to just the TUN slot's port.
func (s *Supervisor) syncRunners(ctx context.Context) {
	type want struct{ port, entry int }
	s.mu.Lock()
	desired := map[string]want{}
	for i := range s.st.Slots {
		sl := &s.st.Slots[i]
		if sl.Enabled && sl.Port > 0 {
			desired[sl.Name] = want{port: sl.Port, entry: s.slotEntryPortLocked(sl)}
		}
	}
	s.mu.Unlock()

	changed := false
	s.runnersMu.Lock()
	for name, r := range s.runners {
		if d, ok := desired[name]; !ok || d.port != r.mainPort || d.entry != r.entryPort {
			r.cancel() // its goroutine stops its live instance on the way out
			delete(s.runners, name)
			changed = true
		}
	}
	for name, d := range desired {
		if _, ok := s.runners[name]; ok {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		r := &slotRunner{sup: s, name: name, mainPort: d.port, entryPort: d.entry, cancel: cancel, kick: make(chan struct{}, 1)}
		s.runners[name] = r
		go r.run(rctx)
		changed = true
	}
	s.runnersMu.Unlock()
	if changed {
		s.publish()
	}
}

// slotEntryPortLocked returns the local port a slot exposes its first hop on
// while chained. Only the default (first) slot exposes it (EntryListenPort), so
// extra slots never collide on the entry port. Call with s.mu held.
func (s *Supervisor) slotEntryPortLocked(sl *store.Slot) int {
	if len(s.st.Slots) > 0 && s.st.Slots[0].Name == sl.Name {
		if ep := s.st.EntryListenPort(); ep != sl.Port {
			return ep
		}
	}
	return 0
}

// stopAllRunners cancels every runner (each closes its live instance on exit).
func (s *Supervisor) stopAllRunners() {
	s.runnersMu.Lock()
	for name, r := range s.runners {
		r.cancel()
		delete(s.runners, name)
	}
	s.runnersMu.Unlock()
}

// restartRunners cancels and recreates every runner so its live instance is
// rebuilt with the current TUN decoration (SO_MARK/bind on/off). Call after
// SetTunMode changes so served instances match probe instances.
func (s *Supervisor) restartRunners(ctx context.Context) {
	s.stopAllRunners()
	s.syncRunners(ctx)
}

// run is the per-slot failover loop: re-evaluate on the interval (or a kick).
func (r *slotRunner) run(ctx context.Context) {
	defer r.stopLive()
	for {
		r.cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		case <-time.After(r.sup.interval()):
		}
	}
}

// poolLoop refreshes pool health on a slower cadence than the selection cycle,
// so egress-probing every node doesn't stretch failover response time.
func (s *Supervisor) poolLoop(ctx context.Context) {
	t := time.NewTicker(poolRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshPools(ctx)
		}
	}
}

// speedLoop runs a rare, automatic throughput test of every node and caches the
// result (Mbps by name) for display. Fully decoupled from failover selection.
func (s *Supervisor) speedLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second): // let startup settle before the first sweep
	}
	s.runSpeeds(ctx)
	t := time.NewTicker(speedInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runSpeeds(ctx)
		}
	}
}

// runSpeeds speedtests only the nodes that currently *ping* — the reachable set
// the pool probe already found (rankedCountry ∪ rankedBypass). Testing a node
// that can't even egress a 204 just wastes a throwaway xray on a guaranteed miss.
func (s *Supervisor) runSpeeds(ctx context.Context) {
	s.mu.Lock()
	nodes := append(append([]*store.Node(nil), s.rankedCountry...), s.rankedBypass...)
	s.mu.Unlock()
	if len(nodes) == 0 {
		return
	}
	s.logf("⚡ speedtest sweep (%d reachable)…", len(nodes))
	sem := make(chan struct{}, speedConcurrency)
	var wg sync.WaitGroup
	for _, n := range nodes {
		if len(n.Outbound) == 0 {
			continue
		}
		wg.Add(1)
		go func(n *store.Node) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if mbps, ok := s.speedProbe(ctx, n.Outbound); ok {
				s.setSpeed(n.Name, mbps)
				s.logf("⚡ %s  %.0f Mbps", n.Name, mbps)
			}
		}(n)
	}
	wg.Wait()
}

// speedProbe spins a throwaway node-direct xray and measures its throughput.
func (s *Supervisor) speedProbe(ctx context.Context, ob json.RawMessage) (float64, bool) {
	port, err := freePort()
	if err != nil {
		return 0, false
	}
	cfg, err := xray.BuildConfig(port, ob, nil, 0, "none", "127.0.0.1")
	if err != nil {
		return 0, false
	}
	inst, err := Start(cfg)
	if err != nil {
		return 0, false
	}
	defer inst.Close()
	if !waitPort(port, 1500*time.Millisecond) {
		return 0, false
	}
	return SpeedThroughSOCKS(ctx, port, speedBudget)
}

func (s *Supervisor) setSpeed(name string, mbps float64) {
	s.speedMu.Lock()
	s.speeds[name] = mbps
	s.speedMu.Unlock()
}

func (s *Supervisor) speedOf(name string) float64 {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	return s.speeds[name]
}

// Kick forces an immediate re-evaluation, on top of the interval.
func (s *Supervisor) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
	s.runnersMu.Lock()
	for _, r := range s.runners {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
	s.runnersMu.Unlock()
}

// --- the cycle ---------------------------------------------------------------

func (r *slotRunner) logf(format string, a ...any) {
	r.sup.logf("["+r.name+"] "+format, a...)
}

func (r *slotRunner) cycle(ctx context.Context) {
	// Snapshot this slot's mains, then resolve outbounds: direct candidates
	// (w/o-hop, may be Vision) and hop candidates (non-Vision — chaining strips
	// the XTLS flow, so Vision exits can only run direct).
	r.sup.mu.Lock()
	var directMains, hopMains []store.Main
	if sl := r.sup.st.SlotByName(r.name); sl != nil {
		directMains = sl.DirectMains()
		hopMains = sl.HopMains()
	}
	pin := r.sup.st.PinEntry
	r.sup.mu.Unlock()

	directOB := namedOutbounds(directMains)
	hopOB := namedOutbounds(hopMains)
	if len(directOB) == 0 && len(hopOB) == 0 {
		r.emit(0, "", 0, "no usable main — add/enable one in this slot", "")
		return
	}

	// A pinned node overrides the whole cascade (always a chain: entry → hop main).
	if pin != "" {
		r.pinnedCycle(ctx, hopOB, pin)
		return
	}

	lo, hi := r.sup.tierBounds()

	r.mu.Lock()
	curTier, curEntry, hasLive := r.liveTier, r.liveEntry, r.live != nil
	r.mu.Unlock()

	// Is the current live chain still reaching the internet?
	var liveLat time.Duration
	healthy := false
	if hasLive {
		if lat, e := EgressThroughSOCKS(ctx, r.mainPort, r.sup.timeout()); e == nil {
			healthy, liveLat = true, lat
		}
	}

	if hasLive && healthy {
		r.failStreak = 0
		// Only consider switching UP to a strictly better (lower) tier.
		if curTier > lo {
			if up, ok := r.bestWorking(ctx, directOB, hopOB, lo, curTier-1); ok {
				r.upStreak++
				if r.upStreak >= r.sup.upThresh() {
					if r.apply(up) == nil {
						r.upStreak = 0
						r.logf("↑ recovered → %s  egress %dms", up.key, up.egress.Milliseconds())
						r.emitPlan(up, "↑ recovered to a better tier")
						return
					}
				}
				r.logf("↑ %s available (%d/%d)", up.key, r.upStreak, r.sup.upThresh())
				r.emit(curTier, curEntry, liveLat, "", fmt.Sprintf("↑ %s available (%d/%d)", up.key, r.upStreak, r.sup.upThresh()))
				return
			}
		}
		r.upStreak = 0
		r.emit(curTier, curEntry, liveLat, "", "")
		return
	}

	// Current chain is down (or nothing running yet).
	r.upStreak = 0
	r.failStreak++
	if !hasLive || r.failStreak >= r.sup.downThresh() {
		if best, ok := r.bestWorking(ctx, directOB, hopOB, lo, hi); ok {
			if e := r.apply(best); e != nil {
				r.logf("apply %s failed: %v", best.key, e)
				r.emit(0, "", 0, e.Error(), "")
				return
			}
			r.failStreak = 0
			r.logf("→ %s  egress %dms", best.key, best.egress.Milliseconds())
			r.emitPlan(best, "")
			return
		}
		r.logf("✖ all tiers unreachable")
		r.emit(0, "", 0, "no main reachable — direct nor via any hop", "")
		return
	}
	// Tolerate a transient blip before switching away.
	r.logf("current chain failing (%d/%d)…", r.failStreak, r.sup.downThresh())
	r.emit(curTier, curEntry, 0, "", fmt.Sprintf("current failing (%d/%d)…", r.failStreak, r.sup.downThresh()))
}

// pinnedCycle serves a specific user-pinned entry node (node → main), bypassing
// the tier cascade. Missing or unreachable => DOWN (the pin is an explicit
// choice; unpin in the Subs tab to restore auto).
func (r *slotRunner) pinnedCycle(ctx context.Context, hopOB []namedOB, pin string) {
	if len(hopOB) == 0 {
		r.emit(0, "", 0, "pinned "+pin+" needs a hop-capable (non-Vision) main — enable one", "")
		return
	}
	node, ok := r.sup.findNodeByName(pin)
	if !ok {
		r.emit(0, "", 0, "pinned node "+pin+" not found (refetch, or unpin in Subs)", "")
		return
	}
	m := hopOB[0]
	lat, up := r.sup.probe(ctx, m.ob, node.Outbound, r.sup.timeout())
	if !up {
		r.logf("pinned %s unreachable", pin)
		r.emit(0, "", 0, "pinned "+pin+" unreachable (unpin for auto)", "")
		return
	}
	tier := 2
	if node.Whitelist {
		tier = 3
	}
	cfg, err := xray.BuildConfig(r.mainPort, m.ob, node.Outbound, r.entryPort, r.sup.loglevel(), r.sup.listen)
	if err != nil {
		r.emit(0, "", 0, err.Error(), "")
		return
	}
	p := plan{tier: tier, entry: &node, main: m.name, config: cfg, egress: lat, key: "PIN:" + node.Name}
	r.mu.Lock()
	same := p.key == r.liveKey
	r.mu.Unlock()
	if !same {
		if e := r.apply(p); e != nil {
			r.emit(0, "", 0, e.Error(), "")
			return
		}
		r.logf("→ pinned %s  egress %dms", node.Name, lat.Milliseconds())
	}
	r.emit(tier, node.Name, lat, "", "📌 pinned")
}

func (s *Supervisor) findNodeByName(name string) (store.Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.st.FindNodeByName(name); n != nil {
		return *n, true
	}
	return store.Node{}, false
}

// namedOB pairs a main's label with its resolved xray outbound JSON.
type namedOB struct {
	name string
	ob   json.RawMessage
}

func namedOutbounds(mains []store.Main) []namedOB {
	var out []namedOB
	for _, m := range mains {
		ob, err := xray.VlessToOutbound(m.URL, "main")
		if err != nil {
			continue
		}
		out = append(out, namedOB{name: m.Name, ob: ob})
	}
	return out
}

// bestWorking probes tiers lo..hi top-down and returns the first plan that
// reaches the internet, using the fastest working main/entry for its tier.
func (r *slotRunner) bestWorking(ctx context.Context, directOB, hopOB []namedOB, lo, hi int) (plan, bool) {
	for tier := lo; tier <= hi; tier++ {
		if p, ok := r.probeTier(ctx, directOB, hopOB, tier); ok {
			return p, true
		}
	}
	return plan{}, false
}

func (r *slotRunner) probeTier(ctx context.Context, directOB, hopOB []namedOB, tier int) (plan, bool) {
	if tier == 1 { // direct: each w/o-hop main (Vision OK), first that egresses
		for _, m := range directOB {
			if lat, ok := r.sup.probe(ctx, m.ob, nil, r.sup.timeout()); ok {
				cfg, err := xray.BuildConfig(r.mainPort, m.ob, nil, 0, r.sup.loglevel(), r.sup.listen)
				if err != nil {
					continue
				}
				return plan{tier: 1, main: m.name, config: cfg, egress: lat, key: "T1:" + m.name}, true
			}
		}
		return plan{}, false
	}
	// chained tiers: each hop main behind the fastest working entry of the pool
	r.sup.mu.Lock()
	ranked := r.sup.rankedCountry
	if tier == 3 {
		ranked = r.sup.rankedBypass
	}
	r.sup.mu.Unlock()
	for _, m := range hopOB {
		for i, n := range ranked {
			if i >= maxTryPerTier {
				break
			}
			if lat, ok := r.sup.probe(ctx, m.ob, n.Outbound, r.sup.timeout()); ok {
				cfg, err := xray.BuildConfig(r.mainPort, m.ob, n.Outbound, r.entryPort, r.sup.loglevel(), r.sup.listen)
				if err != nil {
					continue
				}
				return plan{tier: tier, entry: n, main: m.name, config: cfg, egress: lat, key: fmt.Sprintf("T%d:%s:%s", tier, m.name, n.Name)}, true
			}
		}
	}
	return plan{}, false
}

// --- probing -----------------------------------------------------------------

const (
	poolProbeConcurrency = 10               // max simultaneous pool egress probes
	poolProbeTimeout     = 8 * time.Second  // match the speed budget: "reachable" == "usable", not "fast"
	poolRefreshInterval  = 20 * time.Second // background pool-health cadence

	speedInterval    = 15 * time.Minute // rare auto-speedtest cadence
	speedConcurrency = 4                // max simultaneous speedtests (heavy)
	speedBudget      = 8 * time.Second  // per-node download window (past slow-start)
)

// refreshPools egress-probes both pools (a real 204 straight through each node)
// concurrently, records fastest-first ordering, and updates the dashboard. The
// latency is honest — a raw TCP connect just measured reaching the node's edge.
func (s *Supervisor) refreshPools(ctx context.Context) {
	sem := make(chan struct{}, poolProbeConcurrency)
	var c, b []*store.Node
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c = s.rankPool(ctx, false, sem) }()
	go func() { defer wg.Done(); b = s.rankPool(ctx, true, sem) }()
	wg.Wait()
	s.mu.Lock()
	s.rankedCountry, s.rankedBypass = c, b
	s.mu.Unlock()
	s.publish()
}

func (s *Supervisor) rankPool(ctx context.Context, whitelist bool, sem chan struct{}) []*store.Node {
	s.mu.Lock()
	all := s.st.ActiveNodes()
	s.mu.Unlock()

	var nodes []*store.Node
	for i := range all {
		n := &all[i]
		if n.Whitelist == whitelist && len(n.Outbound) > 0 {
			nodes = append(nodes, n)
		}
	}

	type res struct {
		n   *store.Node
		lat time.Duration
		ok  bool
	}
	results := make([]res, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n *store.Node) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			lat, ok := s.probe(ctx, n.Outbound, nil, poolProbeTimeout) // real 204 through the node
			results[i] = res{n: n, lat: lat, ok: ok}
		}(i, n)
	}
	wg.Wait()

	probes := make([]Probe, len(results))
	for i, r := range results {
		probes[i] = Probe{Name: r.n.Name, Server: r.n.Server, Whitelist: whitelist, Latency: r.lat, OK: r.ok, Speed: s.speedOf(r.n.Name)}
	}
	s.setPool(whitelist, probes)

	sort.SliceStable(results, func(a, b int) bool {
		if results[a].ok != results[b].ok {
			return results[a].ok
		}
		return results[a].lat < results[b].lat
	})
	var out []*store.Node
	for _, r := range results {
		if r.ok {
			out = append(out, r.n)
		}
	}
	return out
}

// probe spins a throwaway xray on the probe port for a candidate (entry nil =
// T1 direct) and returns whether it reaches the internet + the egress latency.
func (s *Supervisor) probe(ctx context.Context, mainOB, entryOB json.RawMessage, timeout time.Duration) (time.Duration, bool) {
	// A fresh OS-assigned port per probe: a fixed ListenPort+1 silently hard-failed
	// every probe (→ permanent DOWN) whenever another service already held that port.
	port, err := freePort()
	if err != nil {
		return 0, false
	}
	cfg, err := xray.BuildConfig(port, mainOB, entryOB, 0, "none", "127.0.0.1") // probes stay local + silent
	if err != nil {
		return 0, false
	}
	inst, err := Start(cfg)
	if err != nil {
		return 0, false
	}
	defer inst.Close()
	if !waitPort(port, 1500*time.Millisecond) {
		return 0, false
	}
	lat, err := EgressThroughSOCKS(ctx, port, timeout)
	return lat, err == nil
}

// --- applying / state --------------------------------------------------------

// apply hot-swaps the runner's live instance on its slot port to the given plan.
// The old instance is closed first (same port), so there's a sub-second gap.
func (r *slotRunner) apply(p plan) error {
	r.mu.Lock()
	old := r.live
	r.live = nil
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	// In TUN mode p.config is already decorated by BuildConfig (SO_MARK on every
	// dial + static hosts), so no special handling is needed here.
	var inst *Instance
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if inst, err = Start(p.config); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond) // let the port fully release
	}
	if err != nil {
		return err
	}

	entry := ""
	if p.entry != nil {
		entry = p.entry.Name
	}
	r.mu.Lock()
	r.live, r.liveKey, r.liveTier, r.liveEntry, r.liveMain = inst, p.key, p.tier, entry, p.main
	r.mu.Unlock()
	return nil
}

func (r *slotRunner) stopLive() {
	r.mu.Lock()
	old := r.live
	r.live, r.liveKey, r.liveTier, r.liveEntry, r.liveMain = nil, "", 0, "", ""
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (s *Supervisor) setPool(whitelist bool, probes []Probe) {
	s.mu.Lock()
	if whitelist {
		s.status.Bypass = probes
	} else {
		s.status.Country = probes
	}
	s.mu.Unlock()
}

func (r *slotRunner) emitPlan(p plan, note string) {
	entry := ""
	if p.entry != nil {
		entry = p.entry.Name
	}
	r.emit(p.tier, entry, p.egress, "", note)
}

// emit records this runner's latest slot status, then republishes the aggregate.
func (r *slotRunner) emit(tier int, entry string, egress time.Duration, errStr, note string) {
	r.mu.Lock()
	r.ss = SlotStatus{Name: r.name, Port: r.mainPort, Tier: tier, Entry: entry, Main: r.liveMain, Egress: egress, Err: errStr, Note: note}
	r.mu.Unlock()
	r.sup.publish()
}

// snapshot returns the runner's last-emitted slot status (name/port always set).
func (r *slotRunner) snapshot() SlotStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	ss := r.ss
	ss.Name, ss.Port = r.name, r.mainPort
	return ss
}

// publish rebuilds the aggregate Status from every runner + the shared pools and
// fires onChange. The top-level tier/main mirror the TUN slot (the primary).
func (s *Supervisor) publish() {
	s.runnersMu.Lock()
	runners := make([]*slotRunner, 0, len(s.runners))
	for _, r := range s.runners {
		runners = append(runners, r)
	}
	s.runnersMu.Unlock()

	s.mu.Lock()
	tunName := s.st.TunSlotName()
	order := map[string]int{}
	for i := range s.st.Slots {
		order[s.st.Slots[i].Name] = i
	}
	s.mu.Unlock()

	slots := make([]SlotStatus, 0, len(runners))
	for _, r := range runners {
		ss := r.snapshot()
		ss.IsTun = ss.Name == tunName
		slots = append(slots, ss)
	}
	sort.SliceStable(slots, func(i, j int) bool { return order[slots[i].Name] < order[slots[j].Name] })

	var primary SlotStatus
	for _, ss := range slots {
		if ss.IsTun {
			primary = ss
			break
		}
	}
	if primary.Name == "" && len(slots) > 0 {
		primary = slots[0]
	}

	s.mu.Lock()
	s.status.Tier = primary.Tier
	s.status.Entry = primary.Entry
	s.status.Main = primary.Main
	s.status.Egress = primary.Egress
	s.status.Err = primary.Err
	s.status.Note = primary.Note
	s.status.TunErr = s.tunErrMsg
	s.status.Slots = slots
	s.status.UpdatedAt = time.Now()
	snap := s.status
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

// --- TUN mode ----------------------------------------------------------------

// syncTun reconciles TUN mode with the stored config each manage-loop pass. The
// bridge (and its device) is created once and kept for the daemon's lifetime:
// enabling/disabling TUN toggles only OS routing, and switching the TUN slot swaps
// only the local relay — the device is never recreated (macOS can't in-process).
func (s *Supervisor) syncTun(ctx context.Context) {
	want := s.cfgBool(func(st *store.State) bool { return st.TunEnabled })
	s.mu.Lock()
	wantSlot := s.st.TunSlotName()
	wantPort := 0
	if sl := s.st.SlotByName(wantSlot); sl != nil {
		wantPort = sl.Port
	}
	s.mu.Unlock()

	switch {
	case want && !s.tunOn && !s.tunErr:
		s.tunUpOrLatch(ctx, wantSlot, wantPort)
	case !want && (s.tunOn || s.tunErr):
		s.tunDownRouting()
	case want && s.tunOn:
		// Already up. If the selected TUN slot (or its port) changed, swap ONLY the
		// cheap local relay behind the persistent bridge (no device churn). Otherwise
		// self-heal the OS routing against network events.
		if wantPort > 0 && (wantSlot != s.tunSlotName || wantPort != s.relayTarget) {
			if err := s.ensureRelay(wantPort); err != nil {
				s.logf("TUN: re-point to %q failed: %v", wantSlot, err)
			} else {
				s.tunSlotName = wantSlot
				s.logf("TUN: slot → %q (:%d) — relay swapped, device unchanged", wantSlot, wantPort)
				s.publish()
			}
		} else {
			s.tunReassert(ctx)
		}
	}
}

// tunReassert keeps TUN routing correct against network events while it's up, so
// the tunnel doesn't silently become a no-op. Runs each cycle; logs only when it
// actually acts. Two failure modes, cheapest first:
//   - uplink moved to a NEW network (gateway/dev changed): the bypass routes,
//     DNS, and xray's own egress binding all point at the dead uplink, so a
//     routes-only fix won't do — Reapply re-points them in place, then we
//     re-decorate for the new uplink dev and rebuild every runner so each exit
//     re-dials over the corrected path (the persistent bridge/device stays put).
//   - same network, but the default-capture halves got wiped (DHCP renew, Wi-Fi
//     bounce, sleep/wake): just re-install them in place.
func (s *Supervisor) tunReassert(ctx context.Context) {
	if changed, desc := s.tunMgr.GatewayChanged(); changed {
		s.logf("TUN: uplink changed to %s — re-pointing routes/egress in place (tunnel stays up)", desc)
		if err := s.tunMgr.Reapply(); err != nil {
			// Usually the new uplink isn't fully ready yet (no default route) —
			// leave TUN up and retry next cycle rather than tearing anything down.
			s.logf("TUN: re-point deferred: %v", err)
			return
		}
		// Refresh xray's egress decoration for the new uplink dev (Linux
		// SO_BINDTODEVICE; no-op on macOS) with the cached hosts — no DNS — then
		// rebuild every runner so the exits re-dial over the corrected path.
		xray.SetTunMode(s.tunMark, tun.UplinkDevice(), s.tunHosts)
		s.restartRunners(ctx)
		s.logf("TUN: re-pointed to the new uplink — exits reconnecting")
		return
	}
	repaired, err := s.tunMgr.Reassert()
	if err != nil {
		s.logf("TUN: re-assert routes: %v", err)
		return
	}
	if repaired {
		s.logf("TUN: default-capture routes were missing (a network change wiped them) — reinstalled; traffic back on the tunnel")
	}
}

// tunUpOrLatch brings TUN up, latching + surfacing the error on failure.
func (s *Supervisor) tunUpOrLatch(ctx context.Context, slot string, port int) {
	if err := s.tunUp(ctx, slot, port); err != nil {
		s.tunErr = true
		s.logf("TUN: %v", err)
		s.mu.Lock()
		s.tunErrMsg = "TUN: " + err.Error()
		s.mu.Unlock()
		s.publish()
	}
}

// ensureBridge creates the persistent tun bridge (device + tun→socks→backendPort)
// exactly once. It is never recreated in-process: on macOS the kernel won't
// destroy a utun while any fd lingers and xray leaks that fd until the process
// exits, so a recreate fails with "resource busy". A stale device from an unclean
// prior exit is cleared + waited-out first.
func (s *Supervisor) ensureBridge() error {
	if s.bridge != nil {
		return nil
	}
	name := s.cfgStr(func(st *store.State) string { return st.TunName })
	if name == "" {
		name = tun.DefaultName()
	}
	mtu := s.cfgInt(func(st *store.State) int { return st.TunMTUOr() })
	p, err := freePort()
	if err != nil {
		return fmt.Errorf("pick tun backend port: %w", err)
	}
	cfg, err := xray.BuildTunBridge(p, name, mtu, s.loglevel())
	if err != nil {
		return fmt.Errorf("build bridge config: %w", err)
	}
	tun.RemoveDevice(name)
	tun.WaitForDeviceGone(name, 3*time.Second)
	inst, err := s.startBridge(cfg, name)
	if err != nil {
		return err
	}
	s.bridge = inst
	s.tunBackendPort = p
	s.tunDevName = name
	return nil
}

// ensureRelay points the loopback relay (backendPort → targetPort) at the current
// TUN slot's port, rebuilding it only if the target changed. Cheap: no device.
func (s *Supervisor) ensureRelay(targetPort int) error {
	if s.relay != nil && s.relayTarget == targetPort {
		return nil
	}
	old := s.relay
	s.relay = nil
	if old != nil {
		_ = old.Close() // frees the backend port for the new relay to bind
	}
	cfg, err := xray.BuildRelay(s.tunBackendPort, targetPort, s.loglevel())
	if err != nil {
		return err
	}
	var inst *Instance
	for attempt := 0; attempt < 5; attempt++ {
		if inst, err = Start(cfg); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond) // let the backend port release from the old relay
	}
	if err != nil {
		return fmt.Errorf("start tun relay: %w", err)
	}
	s.relay = inst
	s.relayTarget = targetPort
	return nil
}

// tunUp ensures the persistent bridge + the relay(slot) exist, decorates every
// dial off-tun, and applies the OS routing/DNS. The bridge forwards captured
// traffic to a fixed backend port; the relay forwards that to the selected TUN
// slot's port, so only that slot's exit carries system-wide traffic and the
// others keep serving off-tun. Reused verbatim on a later re-enable.
func (s *Supervisor) tunUp(ctx context.Context, tunSlot string, slotPort int) error {
	if !tun.Supported() {
		return fmt.Errorf("TUN mode is only supported on Linux, Windows, and macOS")
	}
	if !tun.Privileged() {
		return fmt.Errorf("needs root/Administrator — run the daemon elevated (e.g. `sudo clashvless run`)")
	}
	if tunSlot == "" || slotPort == 0 {
		return fmt.Errorf("no enabled slot to attach TUN to — enable a slot in the Main tab")
	}

	name := s.cfgStr(func(st *store.State) string { return st.TunName })
	if name == "" {
		name = tun.DefaultName()
	}
	mtu := s.cfgInt(func(st *store.State) int { return st.TunMTUOr() })
	addr := s.cfgStr(func(st *store.State) string { return st.TunAddress() })
	// realNet ("real-net" DNS) resolves off-tun; "static routed" rides the exit.
	realNet := s.cfgBool(func(st *store.State) bool { return st.TunDNSDirect })
	// Pick the resolver BEFORE osUp rewrites /etc/resolv.conf: real-net adopts the
	// host's pre-TUN resolver (no public default — a corp LAN may firewall 8.8.8.8,
	// and a LAN resolver is unreachable through the exit); static routed uses
	// TunStaticDNS (default 8.8.8.8, rides the exit).
	dns := tun.ResolverFor(realNet, s.cfgStr(func(st *store.State) string { return st.TunStaticDNS }))
	dnsDirect := realNet
	if dns == "" && realNet {
		s.logf("TUN: DNS mode real-net but no system resolver was detected — set one with `tun dns <ip>` or switch to static routed, else DNS will fail")
	}

	// Resolve every slot's server host on the real network (before the tunnel
	// captures the default route): IPs for the Windows/macOS bypass list, and a
	// domain→IP map for static hosts (so an exit's own domain resolves locally).
	ips, hosts := s.tunResolveAll()
	mark := tun.FwMark()
	dev := tun.UplinkDevice()
	bypassLAN := s.cfgBool(func(st *store.State) bool { return st.TunBypassLAN() })
	blockIPv6 := s.cfgBool(func(st *store.State) bool { return st.TunBlockIPv6() })
	// The tun netstack answers ICMP echo locally so `ping` works (device liveness
	// only — ICMP can't traverse the SOCKS/VLESS tunnel; curl tests end-to-end).
	icmp := s.cfgBool(func(st *store.State) bool { return st.TunICMP() })
	gvisoripv4.ReplyToTemporaryEcho.Store(icmp)

	// DNS-direct: reach the resolver off-tun. Linux pins a /32 (see osUp); on
	// Windows/macOS fold the resolver IP into the per-server bypass list.
	if dnsDirect {
		if ip := net.ParseIP(dns); ip != nil {
			ips = append(ips, ip)
		}
	}

	// Decorate every dialing config (live + probes) off-tun BEFORE (re)building the
	// bridge/relay/runners: SO_MARK + SO_BINDTODEVICE onto the real uplink (Linux),
	// plus static hosts. The device bind survives an nftables ruleset stripping the
	// fwmark (Docker/firewalld), where marked packets would loop back into the tun.
	xray.SetTunMode(mark, dev, hosts)
	s.tunMark, s.tunHosts = mark, hosts // cached for the gateway-change re-point (no re-resolve)

	// Create the device once (never recreated in-process), then point the relay at
	// the selected slot's port.
	if err := s.ensureBridge(); err != nil {
		xray.SetTunMode(0, "", nil)
		return err
	}
	if err := s.ensureRelay(slotPort); err != nil {
		xray.SetTunMode(0, "", nil)
		return err
	}

	if err := s.tunMgr.Up(tun.Config{Name: name, Addr: addr, MTU: mtu, DNS: dns, DNSDirect: dnsDirect, Mark: mark, ServerIPs: ips, BypassLAN: bypassLAN, BlockIPv6: blockIPv6}); err != nil {
		xray.SetTunMode(0, "", nil)
		return err
	}

	s.tunOn = true
	s.tunSlotName = tunSlot
	s.mu.Lock()
	s.tunErrMsg = ""
	s.mu.Unlock()
	icmpWord := "on"
	if !icmp {
		icmpWord = "off"
	}
	if mark != 0 {
		bind := dev
		if bind == "" {
			bind = "(uplink unknown — fwmark only)"
		}
		s.logf("TUN up — all traffic via %s → slot %q (:%d) → exit; xray bound to %s + marked 0x%x to stay off-tun (ping %s)", name, tunSlot, slotPort, bind, mark, icmpWord)
	} else {
		s.logf("TUN up — all traffic via %s → slot %q (:%d) → exit (%d server IP(s) bypassed; ping %s)", name, tunSlot, slotPort, len(ips), icmpWord)
	}

	// Rebuild every runner so their live instances are re-created WITH the off-tun
	// decoration — pre-TUN sockets aren't marked and would loop into the tunnel.
	s.restartRunners(ctx)
	return nil
}

// startBridge starts the bridge (TUN-owning) instance, retrying on a transient
// "resource busy": after a close the OS may still be releasing the device, so
// xray's (re)create fails until it's freed. On Linux/Windows RemoveDevice clears
// a leftover; on macOS the utun is kernel-managed and only the owner's close
// frees it, so we back off and retry while the kernel catches up. A busy that
// never clears (another clashvless daemon owns the device) surfaces with a hint.
func (s *Supervisor) startBridge(cfg []byte, name string) (*Instance, error) {
	const attempts = 8
	var err error
	for i := 0; i < attempts; i++ {
		var inst *Instance
		if inst, err = Start(cfg); err == nil {
			return inst, nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "busy") {
			return nil, err // not the device race — fail fast
		}
		if i == 0 {
			s.logf("TUN: device %s still releasing — retrying bridge start", name)
		}
		tun.RemoveDevice(name)
		time.Sleep(350 * time.Millisecond)
	}
	return nil, fmt.Errorf("%w — is another clashvless daemon already running with TUN on? (device %s stayed busy)", err, name)
}

// tunDownRouting removes the OS routing/DNS and stops the relay, but KEEPS the
// bridge (and its device) alive so a later enable can reuse it — the device
// can't be recreated in-process on macOS. Idempotent.
func (s *Supervisor) tunDownRouting() {
	if !s.tunOn && !s.tunErr {
		return
	}
	xray.SetTunMode(0, "", nil)                  // stop decorating configs
	gvisoripv4.ReplyToTemporaryEcho.Store(false) // stop answering ICMP echo
	_ = s.tunMgr.Down()
	if s.relay != nil {
		_ = s.relay.Close()
		s.relay = nil
		s.relayTarget = 0
	}
	s.tunOn = false
	s.tunErr = false
	s.tunSlotName = ""
	s.mu.Lock()
	s.tunErrMsg = ""
	s.mu.Unlock()
	s.logf("TUN down — routing and DNS restored (device kept until exit; macOS can't recreate a utun)")
	s.publish()
}

// tunShutdown tears TUN fully down on daemon exit: routing off, relay + bridge
// closed. Closing the bridge releases the device on Linux; on macOS the utun goes
// when the process exits and its leaked fd closes.
func (s *Supervisor) tunShutdown() {
	s.tunDownRouting()
	if s.bridge != nil {
		_ = s.bridge.Close()
		s.bridge = nil
		if s.tunDevName != "" {
			tun.RemoveDevice(s.tunDevName)
		}
	}
}

// tunResolveAll resolves every configured main + node server host on the real
// network (before the tunnel captures the default route). Returns the IPs to
// keep off the tunnel (the Windows/macOS bypass list — Linux uses fwmark) and a
// domain→IP map for the live config's static hosts (so an exit's own domain
// resolves locally instead of looping through the tunnel it bootstraps).
func (s *Supervisor) tunResolveAll() (ips []net.IP, hosts map[string]string) {
	s.mu.Lock()
	var srvHosts []string
	for i := range s.st.Slots { // every slot's exit must resolve/bypass off-tun
		for _, m := range s.st.Slots[i].Mains {
			srvHosts = append(srvHosts, hostOf(m.URL))
		}
	}
	for _, n := range s.st.ActiveNodes() {
		srvHosts = append(srvHosts, n.Server)
	}
	s.mu.Unlock()

	hosts = map[string]string{}
	seen := map[string]bool{}
	for _, h := range srvHosts {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		addrs, err := net.LookupIP(h) // real network — tunnel isn't up yet
		if err != nil {
			s.logf("TUN: resolve %s: %v", h, err)
			continue
		}
		for _, a := range addrs {
			if a.To4() != nil {
				ips = append(ips, a)
				if hosts[h] == "" {
					hosts[h] = a.String()
				}
			}
		}
	}
	return ips, hosts
}

// hostOf extracts the host from a vless:// (or any) URL.
func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}
