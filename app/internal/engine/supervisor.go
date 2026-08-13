package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"sync"
	"time"

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

// Status is an immutable snapshot of supervisor state handed to the UI.
type Status struct {
	Tier      int    // 1/2/3 = active tier; 0 = down
	Entry     string // entry node name ("" for T1 direct)
	Main      string // active final-exit main name
	Egress    time.Duration
	Err       string
	Note      string // hysteresis hint, e.g. "↑ T1 available (2/3)"
	Country   []Probe
	Bypass    []Probe
	UpdatedAt time.Time
}

// Supervisor keeps the single local port served by the best available tier and
// re-evaluates on an interval. Hysteresis prevents flapping: it only switches
// UP after a better tier holds for UpThreshold cycles, and only switches DOWN
// after the current chain fails DownThreshold cycles.
type Supervisor struct {
	st        *store.State
	mainPort  int
	entryPort int    // first hop exposed here while chained (0 = disabled)
	listen    string // bind address for the LIVE local inbound(s)
	quiet     bool   // force xray silent (TUI-embedded daemon)

	kick chan struct{}

	mu        sync.Mutex
	live      *Instance
	liveKey   string
	liveTier  int
	liveEntry string
	liveMain  string
	status    Status
	onChange  func(Status)
	onLog     func(string)

	// hysteresis counters — only touched from the single Run goroutine.
	upStreak   int
	failStreak int

	// fastest-first pool ordering for the current cycle.
	rankedCountry []*store.Node
	rankedBypass  []*store.Node

	speedMu sync.Mutex
	speeds  map[string]float64 // Mbps by node name, from the rare speed loop

	// TUN mode (touched only from the single Run goroutine). The bridge is a
	// persistent tun→local-SOCKS instance so failover never churns the device.
	bridge *Instance
	tunMgr *tun.Manager
	tunOn  bool
	tunErr bool // last bring-up failed; don't respin until toggled off
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
	entryPort := st.EntryListenPort()
	if entryPort == st.ListenPort {
		entryPort = 0 // would collide with the main port — don't expose
	}
	s := &Supervisor{
		st:        st,
		mainPort:  st.ListenPort,
		entryPort: entryPort,
		listen:    st.ListenHost(),
		kick:      make(chan struct{}, 1),
		onChange:  onChange,
		onLog:     onLog,
		speeds:    map[string]float64{},
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
	defer s.stopLive()
	defer s.tunDown()   // restore routing/DNS + stop the bridge on shutdown
	s.refreshPools(ctx) // initial pool health so the first cycle has data
	go s.poolLoop(ctx)  // keep pool health fresh in the background
	go s.speedLoop(ctx) // rare auto-speedtest of each node's throughput
	for {
		s.syncTun(ctx) // bring TUN mode up/down to match config
		s.cycle(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
		case <-time.After(s.interval()):
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
}

// --- the cycle ---------------------------------------------------------------

func (s *Supervisor) cycle(ctx context.Context) {
	// Snapshot the mains, then resolve outbounds: direct candidates (w/o-hop,
	// may be Vision) and hop candidates (non-Vision — chaining strips the XTLS
	// flow, so Vision exits can only run direct).
	s.mu.Lock()
	directMains := s.st.DirectMains()
	hopMains := s.st.HopMains()
	pin := s.st.PinEntry
	s.mu.Unlock()

	directOB := namedOutbounds(directMains)
	hopOB := namedOutbounds(hopMains)
	if len(directOB) == 0 && len(hopOB) == 0 {
		s.emit(0, "", 0, "no usable main — add/enable one in the Main tab", "")
		return
	}

	// A pinned node overrides the whole cascade (always a chain: entry → hop main).
	if pin != "" {
		s.pinnedCycle(ctx, hopOB, pin)
		return
	}

	lo, hi := s.tierBounds()

	s.mu.Lock()
	curTier, curEntry, hasLive := s.liveTier, s.liveEntry, s.live != nil
	s.mu.Unlock()

	// Is the current live chain still reaching the internet?
	var liveLat time.Duration
	healthy := false
	if hasLive {
		if lat, e := EgressThroughSOCKS(ctx, s.mainPort, s.timeout()); e == nil {
			healthy, liveLat = true, lat
		}
	}

	if hasLive && healthy {
		s.failStreak = 0
		// Only consider switching UP to a strictly better (lower) tier.
		if curTier > lo {
			if up, ok := s.bestWorking(ctx, directOB, hopOB, lo, curTier-1); ok {
				s.upStreak++
				if s.upStreak >= s.upThresh() {
					if s.apply(up) == nil {
						s.upStreak = 0
						s.logf("↑ recovered → %s  egress %dms", up.key, up.egress.Milliseconds())
						s.emitPlan(up, "↑ recovered to a better tier")
						return
					}
				}
				s.logf("↑ %s available (%d/%d)", up.key, s.upStreak, s.upThresh())
				s.emit(curTier, curEntry, liveLat, "", fmt.Sprintf("↑ %s available (%d/%d)", up.key, s.upStreak, s.upThresh()))
				return
			}
		}
		s.upStreak = 0
		s.emit(curTier, curEntry, liveLat, "", "")
		return
	}

	// Current chain is down (or nothing running yet).
	s.upStreak = 0
	s.failStreak++
	if !hasLive || s.failStreak >= s.downThresh() {
		if best, ok := s.bestWorking(ctx, directOB, hopOB, lo, hi); ok {
			if e := s.apply(best); e != nil {
				s.logf("apply %s failed: %v", best.key, e)
				s.emit(0, "", 0, e.Error(), "")
				return
			}
			s.failStreak = 0
			s.logf("→ %s  egress %dms", best.key, best.egress.Milliseconds())
			s.emitPlan(best, "")
			return
		}
		s.logf("✖ all tiers unreachable")
		s.emit(0, "", 0, "no main reachable — direct nor via any hop", "")
		return
	}
	// Tolerate a transient blip before switching away.
	s.logf("current chain failing (%d/%d)…", s.failStreak, s.downThresh())
	s.emit(curTier, curEntry, 0, "", fmt.Sprintf("current failing (%d/%d)…", s.failStreak, s.downThresh()))
}

// pinnedCycle serves a specific user-pinned entry node (node → main), bypassing
// the tier cascade. Missing or unreachable => DOWN (the pin is an explicit
// choice; unpin in the Subs tab to restore auto).
func (s *Supervisor) pinnedCycle(ctx context.Context, hopOB []namedOB, pin string) {
	if len(hopOB) == 0 {
		s.emit(0, "", 0, "pinned "+pin+" needs a hop-capable (non-Vision) main — enable one", "")
		return
	}
	node, ok := s.findNodeByName(pin)
	if !ok {
		s.emit(0, "", 0, "pinned node "+pin+" not found (refetch, or unpin in Subs)", "")
		return
	}
	m := hopOB[0]
	lat, up := s.probe(ctx, m.ob, node.Outbound, s.timeout())
	if !up {
		s.logf("pinned %s unreachable", pin)
		s.emit(0, "", 0, "pinned "+pin+" unreachable (unpin for auto)", "")
		return
	}
	tier := 2
	if node.Whitelist {
		tier = 3
	}
	cfg, err := xray.BuildConfig(s.mainPort, m.ob, node.Outbound, s.entryPort, s.loglevel(), s.listen)
	if err != nil {
		s.emit(0, "", 0, err.Error(), "")
		return
	}
	p := plan{tier: tier, entry: &node, main: m.name, config: cfg, egress: lat, key: "PIN:" + node.Name}
	if p.key != s.liveKey {
		if e := s.apply(p); e != nil {
			s.emit(0, "", 0, e.Error(), "")
			return
		}
		s.logf("→ pinned %s  egress %dms", node.Name, lat.Milliseconds())
	}
	s.emit(tier, node.Name, lat, "", "📌 pinned")
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
func (s *Supervisor) bestWorking(ctx context.Context, directOB, hopOB []namedOB, lo, hi int) (plan, bool) {
	for tier := lo; tier <= hi; tier++ {
		if p, ok := s.probeTier(ctx, directOB, hopOB, tier); ok {
			return p, true
		}
	}
	return plan{}, false
}

func (s *Supervisor) probeTier(ctx context.Context, directOB, hopOB []namedOB, tier int) (plan, bool) {
	if tier == 1 { // direct: each w/o-hop main (Vision OK), first that egresses
		for _, m := range directOB {
			if lat, ok := s.probe(ctx, m.ob, nil, s.timeout()); ok {
				cfg, err := xray.BuildConfig(s.mainPort, m.ob, nil, 0, s.loglevel(), s.listen)
				if err != nil {
					continue
				}
				return plan{tier: 1, main: m.name, config: cfg, egress: lat, key: "T1:" + m.name}, true
			}
		}
		return plan{}, false
	}
	// chained tiers: each hop main behind the fastest working entry of the pool
	s.mu.Lock()
	ranked := s.rankedCountry
	if tier == 3 {
		ranked = s.rankedBypass
	}
	s.mu.Unlock()
	for _, m := range hopOB {
		for i, n := range ranked {
			if i >= maxTryPerTier {
				break
			}
			if lat, ok := s.probe(ctx, m.ob, n.Outbound, s.timeout()); ok {
				cfg, err := xray.BuildConfig(s.mainPort, m.ob, n.Outbound, s.entryPort, s.loglevel(), s.listen)
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

// apply hot-swaps the live instance on the main port to the given plan. The old
// instance is closed first (same port), so there's a sub-second reconnect gap.
func (s *Supervisor) apply(p plan) error {
	s.mu.Lock()
	old := s.live
	s.live = nil
	s.mu.Unlock()
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
	s.mu.Lock()
	s.live, s.liveKey, s.liveTier, s.liveEntry, s.liveMain = inst, p.key, p.tier, entry, p.main
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) stopLive() {
	s.mu.Lock()
	old := s.live
	s.live, s.liveKey, s.liveTier, s.liveEntry, s.liveMain = nil, "", 0, "", ""
	s.mu.Unlock()
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

func (s *Supervisor) emitPlan(p plan, note string) {
	entry := ""
	if p.entry != nil {
		entry = p.entry.Name
	}
	s.emit(p.tier, entry, p.egress, "", note)
}

func (s *Supervisor) emit(tier int, entry string, egress time.Duration, errStr, note string) {
	s.mu.Lock()
	s.status.Tier = tier
	s.status.Entry = entry
	s.status.Main = s.liveMain
	s.status.Egress = egress
	s.status.Err = errStr
	s.status.Note = note
	s.status.UpdatedAt = time.Now()
	snap := s.status
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

// --- TUN mode ----------------------------------------------------------------

// syncTun brings TUN mode up or down to match the stored TunEnabled flag. Called
// once per cycle from the single Run goroutine, so bridge/tun state needs no lock.
// A failed bring-up latches (tunErr) so it doesn't respin until toggled off.
func (s *Supervisor) syncTun(ctx context.Context) {
	want := s.cfgBool(func(st *store.State) bool { return st.TunEnabled })
	switch {
	case want && !s.tunOn && !s.tunErr:
		if err := s.tunUp(); err != nil {
			s.tunErr = true
			s.logf("TUN: %v", err)
			cur := s.CurrentStatus()
			s.emit(cur.Tier, cur.Entry, cur.Egress, "TUN: "+err.Error(), cur.Note)
		}
	case !want && (s.tunOn || s.tunErr):
		s.tunDown()
	}
}

// tunUp starts the persistent bridge instance (which owns the TUN device) and
// applies the OS-level routing/DNS around it.
func (s *Supervisor) tunUp() error {
	if !tun.Supported() {
		return fmt.Errorf("TUN mode is only supported on Linux, Windows, and macOS")
	}
	if !tun.Privileged() {
		return fmt.Errorf("needs root/Administrator — run the daemon elevated (e.g. `sudo clashvless run`)")
	}

	name := s.cfgStr(func(st *store.State) string { return st.TunName })
	if name == "" {
		name = tun.DefaultName()
	}
	mtu := s.cfgInt(func(st *store.State) int { return st.TunMTUOr() })
	addr := s.cfgStr(func(st *store.State) string { return st.TunAddress() })
	dns := s.cfgStr(func(st *store.State) string { return st.TunResolver() })

	// Resolve every server host on the real network (before the tunnel captures
	// the default route): the IPs for the Windows/macOS bypass list, and a
	// domain→IP map for static hosts (so an exit's own domain resolves locally).
	ips, hosts := s.tunResolveAll()
	mark := tun.FwMark()

	// From now on BuildConfig decorates every config (live + probes): SO_MARK so
	// xray's own connections route off-tun (Linux), and static hosts.
	xray.SetTunMode(mark, hosts)

	cfg, err := xray.BuildTunBridge(s.mainPort, name, mtu, s.loglevel())
	if err != nil {
		xray.SetTunMode(0, nil)
		return fmt.Errorf("build bridge config: %w", err)
	}
	inst, err := Start(cfg)
	if err != nil {
		xray.SetTunMode(0, nil)
		return fmt.Errorf("start bridge instance (Windows needs wintun.dll next to the exe): %w", err)
	}

	if err := s.tunMgr.Up(tun.Config{Name: name, Addr: addr, MTU: mtu, DNS: dns, Mark: mark, ServerIPs: ips}); err != nil {
		_ = inst.Close()
		xray.SetTunMode(0, nil)
		return err
	}

	s.bridge = inst
	s.tunOn = true
	if mark != 0 {
		s.logf("TUN up — all traffic via %s → exit; xray marked 0x%x to route off-tun", name, mark)
	} else {
		s.logf("TUN up — all traffic via %s → exit (%d server IP(s) bypassed)", name, len(ips))
	}

	// Rebuild the live instance so it's re-created WITH the decoration — its
	// pre-TUN sockets aren't marked and would loop. The next cycle re-applies.
	s.stopLive()
	return nil
}

// tunDown restores the OS routing/DNS and stops the bridge instance. Idempotent.
func (s *Supervisor) tunDown() {
	if !s.tunOn && s.bridge == nil {
		s.tunErr = false
		return
	}
	xray.SetTunMode(0, nil) // stop decorating configs
	_ = s.tunMgr.Down()
	if s.bridge != nil {
		_ = s.bridge.Close()
		s.bridge = nil
	}
	s.tunOn = false
	s.tunErr = false
	s.logf("TUN down — routing and DNS restored")
}

// tunResolveAll resolves every configured main + node server host on the real
// network (before the tunnel captures the default route). Returns the IPs to
// keep off the tunnel (the Windows/macOS bypass list — Linux uses fwmark) and a
// domain→IP map for the live config's static hosts (so an exit's own domain
// resolves locally instead of looping through the tunnel it bootstraps).
func (s *Supervisor) tunResolveAll() (ips []net.IP, hosts map[string]string) {
	s.mu.Lock()
	var srvHosts []string
	for _, m := range s.st.Mains {
		srvHosts = append(srvHosts, hostOf(m.URL))
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
