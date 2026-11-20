package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gvisoripv4 "gvisor.dev/gvisor/pkg/tcpip/network/ipv4" // TUN ICMP-echo toggle (vendored gVisor patch)

	"clashvless/internal/loghub"
	"clashvless/internal/store"
	"clashvless/internal/tun"
	"clashvless/internal/xray"
	"regexp"
)

// Probe is a per-node reachability sample shown in the UI.
type Probe struct {
	Name      string
	Server    string
	Whitelist bool
	Latency   time.Duration
	OK        bool
	Speed     float64 // mean Mbps over the speed window (see SpeedN for whether it's measured)
	SpeedN    int     // speed samples behind Speed (0 = not yet tested; >0 with Speed 0 = failed in chain)
	Flakiness float64 // % of the last hour's reachability probes that FAILED (0 = rock-solid)
	Samples   int     // probes behind Flakiness in the window (0 = not enough evidence yet)
}

// SlotStatus is the live failover state of one slot (its own port + best main).
type SlotStatus struct {
	Name    string
	Port    int
	Tier    int    // 1/2/3 = active tier; 0 = down
	Entry   string // entry node name ("" for T1 direct)
	Main    string // active final-exit main name
	Egress  time.Duration
	Err     string
	Note    string
	IsTun   bool // this slot is the one the TUN bridge attaches to
	HopOnly bool // main is down; only the first-hop entry port is still served
}

// Status is an immutable snapshot of supervisor state handed to the UI. The
// top-level Tier/Entry/Main/Egress/Err/Note mirror the primary slot — the TUN
// slot while TUN is ON, else the first slot — so single-slot views keep working
// without a down TUN-designated slot painting the header DOWN while another slot
// serves; Slots carries every slot's state.
type Status struct {
	Tier      int    // 1/2/3 = active tier; 0 = down (TUN slot)
	Entry     string // entry node name ("" for T1 direct)
	Main      string // active final-exit main name
	Egress    time.Duration
	Err       string
	Note      string // hysteresis hint, e.g. "↑ T1 available (2/3)"
	HopOnly   bool   // primary slot's main is down but its first-hop port still serves
	TunErr    string // TUN bring-up error, if any
	TunUp     bool   // system traffic is actually captured (TunEnabled alone is only the wish)
	Slots     []SlotStatus
	Country   []Probe
	Bypass    []Probe
	Sweep     Sweep // live speedtest sweep progress (zero = idle)
	UpdatedAt time.Time
}

// Sweep is the progress of a running speedtest sweep, for the UI's spinner.
type Sweep struct {
	Node  string // node under test now ("" = no sweep running)
	Done  int    // nodes finished so far
	Total int    // nodes in this sweep
	Via   string // main each node is chained to ("" = node-direct)
	Ref   string // host of the speed reference in use
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
	onLog    func(loghub.Record)

	// fastest-first pool ordering, shared by every slot runner.
	rankedCountry []*store.Node
	rankedBypass  []*store.Node

	speedMu     sync.Mutex
	speeds      map[string][]speedSample // rolling 1h throughput samples by node name (rare speed loop)
	speedsReady bool                     // a sweep has completed with real measurements (gates the one-time upgrade)
	speedOff    atomic.Bool              // mirror of State.NoSpeedTest (read under speedMu, so it can't take mu)

	flakeMu sync.Mutex
	flakes  map[string][]flakeSample // rolling 1h reachability history by node name (the stability signal)

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
	tunErr         bool // last bring-up failed; retried from tunRetryAt
	tunRetryAt     time.Time
	tunActive      atomic.Bool // tunOn for publish, which runs on other goroutines
	badExitLogged  sync.Map    // invalid exits already reported: a runner re-parses them every cycle
	tunErrMsg      string
	tunMark        int32             // xray's off-tun socket mark (cached for the gateway-change re-point)
	tunHosts       map[string]string // exit domain→IP map (cached so the re-point re-decorates without DNS)
	tunServerSig   string            // the server hosts tunHosts and the bypass were built from
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
	liveOB    json.RawMessage // the live main's outbound, to recognise it after edits
	ss        SlotStatus

	mainsSig string // the slot's eligible mains as of the last cycle
	haveSig  bool

	// hysteresis counters — only touched from this runner's goroutine.
	upStreak    int
	failStreak  int
	liveBySpeed bool // current auto pick is throughput-backed (else a cold-start ping pick, upgraded once speed lands)
	hopOnly     bool // live instance serves only the first-hop entry port (main down)
}

type plan struct {
	tier    int
	entry   *store.Node
	main    string
	mainOB  json.RawMessage
	config  []byte
	egress  time.Duration
	key     string
	hopOnly bool // serves only the first-hop entry port (main down) — not a full chain
}

// NewSupervisor builds a supervisor. onChange (may be nil) receives a fresh
// Status snapshot on every state change; onLog receives structured activity events.
func NewSupervisor(st *store.State, onChange func(Status), onLog func(loghub.Record)) *Supervisor {
	s := &Supervisor{
		st:       st,
		listen:   st.ListenHost(),
		kick:     make(chan struct{}, 1),
		onChange: onChange,
		onLog:    onLog,
		speeds:   map[string][]speedSample{},
		runners:  map[string]*slotRunner{},
	}
	s.tunMgr = tun.New(s.logf)
	return s
}

// Log emits an informational engine record for compatibility with existing callers.
func (s *Supervisor) Log(line string) { s.Logf(loghub.Info, "engine", "", "%s", line) }

func (s *Supervisor) Logf(level loghub.Level, module, element, format string, a ...any) {
	if s.onLog != nil {
		s.onLog(loghub.New(level, module, element, format, a...))
	}
}

func (s *Supervisor) logf(format string, a ...any) {
	s.Logf(loghub.Info, "engine", "", format, a...)
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
		s.healStaleDNS()   // clear a stale LAN resolver stranded by a prior network/TUN session
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
		case <-time.After(s.interval()):
		}
	}
}

// healStaleDNS clears a stale LAN resolver left pinned in the OS network config (a
// real-net TUN session on a PREVIOUS network, or an unclean daemon exit that skipped
// the DNS restore) so a network change can't strand every lookup — the "connected to
// a different network and nothing resolves, tunnel/proxy DOWN" symptom. Runs each
// cycle ONLY while TUN is OFF; when TUN is up, tunUp/ReapplyDNS own the resolver, so
// touching it here would fight them. macOS-only in effect (see tun.HealStaleResolver).
func (s *Supervisor) healStaleDNS() {
	if s.tunOn {
		return
	}
	if healed, desc := tun.HealStaleResolver(); healed {
		s.logf("DNS: cleared a stale resolver (%s) left from a network change or TUN session — reverted to DHCP", desc)
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
			desired[sl.Name] = want{port: sl.Port, entry: s.st.HopPort(i)}
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

// speedLoop runs a rare, automatic throughput test of every reachable node. The
// mean of those samples ranks the entry pools (see rankPool), so it decides which
// hop a chained tier picks.
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

// speedRefMain is the hop main every entry is speedtested in front of: the first
// hop-able (non-Vision) main of the TUN slot (else the first enabled slot). Nodes
// are only ever used as ENTRIES chained to such a main, and a node that is fast as
// its own exit can crawl in front of the main (its path to the main's server gets
// throttled), so the chain — not the node alone — is what must be measured. nil
// (no hop-able main) falls back to a node-direct test.
func (s *Supervisor) speedRefMain() (namedOB, bool) {
	s.mu.Lock()
	sl := s.st.SlotByName(s.st.TunSlotName())
	var mains []store.Main
	if sl != nil {
		mains = sl.HopMains()
	}
	s.mu.Unlock()
	if obs := s.namedOutbounds(mains); len(obs) > 0 {
		return obs[0], true
	}
	return namedOB{}, false
}

// runSpeeds speedtests only the nodes that currently *ping* — the reachable set
// the pool probe already found (rankedCountry ∪ rankedBypass). Testing a node
// that can't even egress a 204 just wastes a throwaway xray on a guaranteed miss.
// Each node is tested as hop → speedRefMain (the real chain). Tests run one at a
// time: they share the uplink and, chained, the main's server too, so parallel
// tests would split the bandwidth and skew the ranking. A reachable node whose
// transfer fails scores 0 (it can't carry traffic) rather than staying
// "unmeasured" and ranking mid-pack by ping. A refused reference (HTTP 429 etc.)
// says nothing about the node: the sweep moves to the next speedURLs entry and
// retests the same node, and only aborts (recording nothing more) once every
// reference refuses. Any other failure is only blamed on the node once the
// current reference is PROVEN (some node measured through it this sweep) — before
// that, a broken reference (e.g. one that rejects our client) would otherwise
// score every node 0, so the sweep rotates references instead; a node that fails
// on every reference scores 0.
func (s *Supervisor) runSpeeds(ctx context.Context) {
	s.mu.Lock()
	nodes := append(append([]*store.Node(nil), s.rankedCountry...), s.rankedBypass...)
	s.speedOff.Store(s.st.NoSpeedTest)
	s.mu.Unlock()
	if len(nodes) == 0 || s.speedOff.Load() {
		return
	}
	ref, chained := s.speedRefMain()
	defer s.setSweep(Sweep{}) // idle again however the sweep ends
	if chained {
		s.logf("⚡ speedtest sweep (%d reachable, each → %s)…", len(nodes), ref.name)
	} else {
		s.logf("⚡ speedtest sweep (%d reachable, node-direct: no hop main)…", len(nodes))
	}
	ui := 0         // index into speedURLs; sticks for the rest of the sweep once moved
	proven := false // a node has measured through speedURLs[ui] this sweep
	measured := 0
	for i, n := range nodes {
		if ctx.Err() != nil {
			return
		}
		if s.speedOff.Load() { // switched off mid-sweep
			s.logf("⚡ speedtest switched off — sweep stopped")
			return
		}
		if len(n.Outbound) == 0 {
			continue
		}
		mainOB, entryOB := n.Outbound, json.RawMessage(nil)
		if chained {
			mainOB, entryOB = ref.ob, n.Outbound
		}
		var mbps float64
		var err error
		allRefused := true
		for tried := 1; ; tried++ {
			s.setSweep(Sweep{Node: n.Name, Done: i, Total: len(nodes), Via: ref.name, Ref: urlHost(speedURLs[ui])})
			mbps, err = s.speedProbe(ctx, mainOB, entryOB, speedURLs[ui])
			refused := errors.Is(err, ErrSpeedRefused)
			allRefused = allRefused && refused
			if err == nil {
				proven = true
				break
			}
			if errors.Is(err, errSpeedSetup) || ctx.Err() != nil || (!refused && proven) || tried >= len(speedURLs) {
				break // setup/cancel, a real node failure on a proven reference, or every reference tried
			}
			ui, proven = (ui+1)%len(speedURLs), false
			s.logf("⚡ %s via %s: %v — switching speed reference to %s", n.Name, urlHost(speedURLs[(ui+len(speedURLs)-1)%len(speedURLs)]), err, urlHost(speedURLs[ui]))
		}
		switch {
		case errors.Is(err, errSpeedSetup) || ctx.Err() != nil:
			continue // our side failed to stand the test up — not the node's fault
		case err != nil && allRefused:
			s.logf("⚡ sweep aborted at %s: every speed reference refused — keeping previous samples", n.Name)
			return
		case err != nil:
			s.recordSpeed(n.Name, 0)
			s.logf("⚡ %s  0 Mbps (%v)", n.Name, err)
		default:
			measured++
			s.recordSpeed(n.Name, mbps)
			s.logf("⚡ %s  %.0f Mbps", n.Name, mbps)
		}
	}
	if measured > 0 {
		s.speedMu.Lock()
		s.speedsReady = true
		s.speedMu.Unlock()
		s.Kick() // let slots take the one-time best-speed upgrade now, not a cycle later
	}
	s.logf("⚡ sweep done: %d/%d measured", measured, len(nodes))
}

// speedsSwept reports whether a sweep has finished with real measurements, so a
// throughput ranking covers the pool rather than the first node or two.
func (s *Supervisor) speedsSwept() bool {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	return s.speedsReady && !s.speedOff.Load()
}

// setSweep publishes sweep progress and refreshes every listed node's speed from
// the samples so far, so the UI shows a node's new number the moment its test
// ends instead of at the next pool refresh.
func (s *Supervisor) setSweep(sw Sweep) {
	s.mu.Lock()
	s.status.Sweep = sw
	s.status.Country = s.withSpeeds(s.status.Country)
	s.status.Bypass = s.withSpeeds(s.status.Bypass)
	s.mu.Unlock()
	s.publish()
}

// withSpeeds returns a copy of pool with each node's speed re-read from the
// samples (a copy: earlier snapshots handed to the UI share the old backing array).
func (s *Supervisor) withSpeeds(pool []Probe) []Probe {
	out := append([]Probe(nil), pool...)
	for i := range out {
		out[i].Speed, out[i].SpeedN = s.meanSpeed(out[i].Name)
	}
	return out
}

// errSpeedSetup marks a speed probe that never got to test the path (no free
// port, bad config, xray didn't start).
var errSpeedSetup = errors.New("speed probe setup failed")

// urlHost is a speed reference's host, for logs and the UI.
func urlHost(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// speedProbe spins a throwaway xray (mainOB, dialed through entryOB when non-nil)
// and measures its throughput downloading refURL.
func (s *Supervisor) speedProbe(ctx context.Context, mainOB, entryOB json.RawMessage, refURL string) (float64, error) {
	port, err := freePort()
	if err != nil {
		return 0, errSpeedSetup
	}
	cfg, err := xray.BuildConfig(port, mainOB, entryOB, 0, "none", "127.0.0.1")
	if err != nil {
		return 0, errSpeedSetup
	}
	inst, err := Start(cfg)
	if err != nil {
		return 0, errSpeedSetup
	}
	defer inst.Close()
	if !waitPort(port, 1500*time.Millisecond) {
		return 0, errSpeedSetup
	}
	return SpeedThroughSOCKS(ctx, port, refURL, speedBudget)
}

type speedSample struct {
	t    time.Time
	mbps float64
}

// recordSpeed appends one throughput sample for a node and prunes the window.
func (s *Supervisor) recordSpeed(name string, mbps float64) {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	if s.speeds == nil {
		s.speeds = map[string][]speedSample{}
	}
	now := time.Now()
	cut := now.Add(-speedWindow)
	hist := append(s.speeds[name], speedSample{t: now, mbps: mbps})
	i := 0
	for i < len(hist) && hist[i].t.Before(cut) {
		i++
	}
	s.speeds[name] = hist[i:]
}

// meanSpeed averages a node's last-hour throughput samples (Mbps) and returns how
// many samples that's over (0 = not yet measured). It's the within-band leader
// tiebreak — steadiest first, then FASTEST by mean throughput, ping only as the
// fallback when speed isn't measured for both candidates.
func (s *Supervisor) meanSpeed(name string) (mbps float64, samples int) {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	if s.speedOff.Load() { // speedtest off: every node reads as unmeasured → ping ranking
		return 0, 0
	}
	cut := time.Now().Add(-speedWindow)
	var sum float64
	var n int
	for _, smp := range s.speeds[name] {
		if smp.t.Before(cut) {
			continue
		}
		sum += smp.mbps
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
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
	r.sup.Logf(loghub.Info, "route", r.name, format, a...)
}

func (r *slotRunner) warnf(format string, a ...any) {
	r.sup.Logf(loghub.Warning, "route", r.name, format, a...)
}

func (r *slotRunner) errorf(format string, a ...any) {
	r.sup.Logf(loghub.Error, "route", r.name, format, a...)
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

	directOB := r.sup.namedOutbounds(directMains)
	hopOB := r.sup.namedOutbounds(hopMains)
	sig := mainsSignature(directOB, hopOB)
	mainsChanged := r.haveSig && sig != r.mainsSig
	r.mainsSig, r.haveSig = sig, true
	if len(directOB) == 0 && len(hopOB) == 0 {
		r.stopLive() // a removed or disabled exit must not keep carrying traffic
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
	liveOB, servingHopOnly, liveKey := r.liveOB, r.hopOnly, r.liveKey
	r.mu.Unlock()

	// The live main must still be allowed at its tier: deleting, disabling or
	// demoting it, or a tier pin / force-hop that excludes its tier, retires it now.
	stale := hasLive && !servingHopOnly && !(curTier >= lo && curTier <= hi && r.followLiveMain(liveOB, curTier, directOB, hopOB))

	// Is the current live chain still reaching the internet?
	var liveLat time.Duration
	healthy := false
	if hasLive && !stale {
		if lat, e := EgressThroughSOCKS(ctx, r.mainPort, r.sup.timeout()); e == nil {
			healthy, liveLat = true, lat
		}
	}

	if hasLive && healthy {
		r.failStreak = 0

		// An edit to the slot's exits (e.g. a new priority order) applies now
		// instead of waiting for the live chain to fail. The same main on the same
		// tier stays put, so its entry isn't churned.
		if mainsChanged {
			if best, ok := r.bestWorking(ctx, directOB, hopOB, lo, hi); ok && (best.tier != curTier || !bytes.Equal(best.mainOB, liveOB)) {
				if e := r.apply(best); e != nil {
					r.applyFailed(best, e)
					return
				}
				r.upStreak = 0
				r.liveBySpeed = r.speedBacked(best)
				r.logf("→ %s  egress %dms (exits changed)", best.key, best.egress.Milliseconds())
				r.emitPlan(best, "exits changed — re-selected by priority")
				return
			}
		}

		// One-time throughput upgrade: a pick made ping-only (cold start, before any
		// speedtest) swaps to the best-throughput entry in its tier the moment speed
		// data lands — then it's sticky again (never chasing ping/speed jitter).
		if !r.liveBySpeed && curTier >= 2 {
			if r.sup.speedsSwept() { // a full sweep is in — not just the first node or two
				if best, ok := r.bestWorking(ctx, directOB, hopOB, curTier, curTier); ok {
					var bestSpd float64
					if best.entry != nil {
						bestSpd, _ = r.sup.meanSpeed(best.entry.Name)
					}
					curSpd, _ := r.sup.meanSpeed(curEntry)
					r.liveBySpeed = true // swap at most once; the current entry may already be best
					if best.entry != nil && best.key != liveKey && bestSpd > curSpd {
						if e := r.apply(best); e != nil {
							r.applyFailed(best, e)
							return
						}
						r.logf("⚡ upgraded to best-speed %s (%.0f Mbps)", best.entry.Name, bestSpd)
						r.emitPlan(best, "⚡ upgraded to best throughput")
						return
					}
				}
			}
		}

		// Only consider switching UP to a strictly better (lower) tier.
		if curTier > lo {
			if up, ok := r.bestWorking(ctx, directOB, hopOB, lo, curTier-1); ok {
				r.upStreak++
				if r.upStreak >= r.sup.upThresh() {
					if e := r.apply(up); e != nil {
						r.upStreak = 0
						r.applyFailed(up, e)
						return
					}
					r.upStreak = 0
					r.liveBySpeed = r.speedBacked(up)
					r.logf("↑ recovered → %s  egress %dms", up.key, up.egress.Milliseconds())
					r.emitPlan(up, "↑ recovered to a better tier")
					return
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

	// Current chain is down, retired by an edit, or nothing is running yet.
	r.upStreak = 0
	if stale {
		r.logf("current exit is no longer allowed by this slot's settings — re-selecting")
	}

	// An entry-only fallback (main down, first hop still serving) is not a live
	// MAIN chain, so it gets no blip tolerance — we immediately re-attempt a full
	// chain. A real live chain tolerates DownThreshold failures before switching.
	if hasLive && !servingHopOnly && !stale {
		r.failStreak++
		if r.failStreak < r.sup.downThresh() {
			// Tolerate a transient blip before switching away.
			r.warnf("current chain failing (%d/%d)…", r.failStreak, r.sup.downThresh())
			r.emit(curTier, curEntry, 0, "", fmt.Sprintf("current failing (%d/%d)…", r.failStreak, r.sup.downThresh()))
			return
		}
	}

	if best, ok := r.bestWorking(ctx, directOB, hopOB, lo, hi); ok {
		if e := r.apply(best); e != nil {
			r.applyFailed(best, e)
			return
		}
		r.failStreak = 0
		r.liveBySpeed = r.speedBacked(best)
		r.logf("→ %s  egress %dms", best.key, best.egress.Milliseconds())
		r.emitPlan(best, "")
		return
	}
	if stale {
		r.stopLive()
		liveKey = ""
	}

	// No main exit works via any tier. Keep the first-hop port usable anyway:
	// serve the best reachable entry node directly on the hop port, so an app
	// pointed at it still gets out even while the slot's main is down.
	if r.serveHopOnly(ctx, hopOB, liveKey) {
		return
	}

	r.logf("✖ all tiers unreachable")
	r.emit(0, "", 0, "no main reachable — direct nor via any hop", "")
}

// followLiveMain reports whether the live main is still a candidate for its
// tier. A renamed main keeps serving under its new name.
func (r *slotRunner) followLiveMain(ob json.RawMessage, tier int, directOB, hopOB []namedOB) bool {
	candidates := hopOB
	if tier == 1 {
		candidates = directOB
	}
	for _, m := range candidates {
		if bytes.Equal(m.ob, ob) {
			r.mu.Lock()
			r.liveMain = m.name
			r.mu.Unlock()
			return true
		}
	}
	return false
}

// applyFailed reports a swap whose new instance didn't start. apply already
// closed the old one, so the slot is down until the next cycle rebuilds it.
func (r *slotRunner) applyFailed(p plan, err error) {
	r.errorf("apply %s failed: %v", p.key, err)
	r.emit(0, "", 0, err.Error(), "")
}

// mainsSignature captures which mains are eligible per tier, in priority order.
func mainsSignature(directOB, hopOB []namedOB) string {
	var b strings.Builder
	for _, m := range directOB {
		b.WriteString("d")
		b.Write(m.ob)
		b.WriteByte(0)
	}
	for _, m := range hopOB {
		b.WriteString("h")
		b.Write(m.ob)
		b.WriteByte(0)
	}
	return b.String()
}

const hopOnlyKeyPrefix = "HOP:"

// serveHopOnly keeps a slot's first-hop port alive when no full main chain can
// be built: it serves the best reachable entry node directly on the entry port,
// so traffic pointed at the hop still egresses through the entry even though the
// slot's own main exit is down. Only slots that actually chain (have a hop-capable
// main) and expose a distinct hop port qualify. Sticky: if it's already serving
// an entry whose hop port still egresses, it leaves the instance untouched.
func (r *slotRunner) serveHopOnly(ctx context.Context, hopOB []namedOB, liveKey string) bool {
	if r.entryPort == 0 || r.entryPort == r.mainPort || len(hopOB) == 0 {
		return false
	}

	// Already serving a healthy entry-only fallback? Don't churn the instance.
	if strings.HasPrefix(liveKey, hopOnlyKeyPrefix) {
		if lat, e := EgressThroughSOCKS(ctx, r.entryPort, r.sup.timeout()); e == nil {
			name := strings.TrimPrefix(liveKey, hopOnlyKeyPrefix)
			r.emit(0, "", lat, "", fmt.Sprintf("⚠ main down — first hop %s still up on :%d", name, r.entryPort))
			return true
		}
	}

	// Best-ranked entries first: country pool (T2), then bypass (T3).
	r.sup.mu.Lock()
	ranked := append(append([]*store.Node(nil), r.sup.rankedCountry...), r.sup.rankedBypass...)
	r.sup.mu.Unlock()

	for _, n := range ranked {
		if len(n.Outbound) == 0 {
			continue
		}
		if lat, ok := r.sup.probe(ctx, n.Outbound, nil, r.sup.timeout()); ok {
			cfg, err := xray.BuildConfig(r.entryPort, n.Outbound, nil, 0, r.sup.loglevel(), r.sup.listen)
			if err != nil {
				continue
			}
			p := plan{tier: 0, main: n.Name, config: cfg, egress: lat, key: hopOnlyKeyPrefix + n.Name, hopOnly: true}
			if e := r.apply(p); e != nil {
				continue
			}
			r.failStreak = 0
			r.logf("⚠ main down — serving first hop %s only on :%d", n.Name, r.entryPort)
			r.emit(0, "", lat, "", fmt.Sprintf("⚠ main down — first hop %s still up on :%d", n.Name, r.entryPort))
			return true
		}
	}
	return false
}

// pinnedCycle serves a specific user-pinned entry node (node → main), bypassing
// the tier cascade. Missing or unreachable => DOWN (the pin is an explicit
// choice; unpin in the Subs tab to restore auto).
//
// Health is judged off the WARM live socket — the exact path real traffic takes —
// exactly like the auto cascade does, NOT a cold throwaway probe. A cold 2-hop
// reality chain routinely misses the timeout on its first packet while the live
// exit is carrying traffic perfectly; the old code cold-probed every cycle and,
// on that miss, reported DOWN without ever touching the (still-serving) instance
// — so the dashboard flapped to "unreachable" while curl through the port worked.
// Now a cold probe only gates the initial build / a real recovery, with the same
// downThreshold hysteresis as auto.
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
	tier := 2
	if node.Whitelist {
		tier = 3
	}
	wantKey := "PIN:" + node.Name + ":" + m.name // the top-priority hop main, so a reorder rebuilds

	// Already serving this exact pin? Trust the live socket; re-probe it warm.
	r.mu.Lock()
	liveMatches := r.live != nil && r.liveKey == wantKey
	r.mu.Unlock()
	if liveMatches {
		if lat, e := EgressThroughSOCKS(ctx, r.mainPort, r.sup.timeout()); e == nil {
			r.failStreak = 0
			r.emit(tier, node.Name, lat, "", "📌 pinned")
			return
		}
		r.failStreak++
		if r.failStreak < r.sup.downThresh() { // tolerate a transient blip
			r.logf("pinned %s failing (%d/%d)…", pin, r.failStreak, r.sup.downThresh())
			r.emit(tier, node.Name, 0, "", fmt.Sprintf("📌 pinned failing (%d/%d)…", r.failStreak, r.sup.downThresh()))
			return
		}
	}

	// Fresh pin, or the live chain has failed past the threshold: cold-probe and
	// (re)build the serving instance.
	lat, up := r.sup.probe(ctx, m.ob, node.Outbound, r.sup.timeout())
	if !up {
		// The pinned chain can't be built — but if a previously-applied instance is
		// still carrying traffic (a fallback from before the pin), we are NOT offline.
		// Report the exit that's ACTUALLY serving, honestly, and keep it — a bare
		// "DOWN" while curl through the port plainly works is what read as a false
		// alarm. Truly DOWN only when nothing is serving.
		r.mu.Lock()
		haveLive, liveEntry, liveMain, liveTier, prevNote := r.live != nil, r.liveEntry, r.liveMain, r.liveTier, r.ss.Note
		r.mu.Unlock()
		if haveLive {
			if wlat, e := EgressThroughSOCKS(ctx, r.mainPort, r.sup.timeout()); e == nil {
				via := liveEntry
				if via == "" {
					via = liveMain // T1 fallback has no entry; name the main
				}
				note := "⚠ pinned " + pin + " down — serving " + via + " instead"
				if note != prevNote { // log the transition once, not every tick
					r.logf("pinned %s unreachable — still online via %s", pin, via)
				}
				r.emit(liveTier, liveEntry, wlat, "", note)
				return
			}
		}
		// Distinguish a genuinely dead entry from a reachable entry whose chain just
		// won't carry THIS main (e.g. a Vision entry mangles a reality main's inner
		// handshake — see README hop matrix). The old bare "unreachable" lied when the
		// node answered fine on raw TCP, which is exactly what confused the pin.
		note := "pinned " + pin + " unreachable (unpin for auto)"
		if _, e := TCPLatency(node.Server, node.Port, 4*time.Second); e == nil {
			note = "pinned " + pin + " ok, but no exit via " + m.name + " — try a non-Vision entry (unpin for auto)"
		}
		if note != prevNote {
			r.logf("%s", note)
		}
		r.emit(0, "", 0, note, "")
		return
	}
	r.failStreak = 0
	cfg, err := xray.BuildConfig(r.mainPort, m.ob, node.Outbound, r.entryPort, r.sup.loglevel(), r.sup.listen)
	if err != nil {
		r.emit(0, "", 0, err.Error(), "")
		return
	}
	p := plan{tier: tier, entry: &node, main: m.name, mainOB: m.ob, config: cfg, egress: lat, key: wantKey}
	if e := r.apply(p); e != nil {
		r.emit(0, "", 0, e.Error(), "")
		return
	}
	r.logf("→ pinned %s  egress %dms", node.Name, lat.Milliseconds())
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

// exitURLPattern finds a URL quoted in a parse error: it carries the exit's UUID or proxy credentials.
var exitURLPattern = regexp.MustCompile(`(?i)(?:vless|http|socks5h?|socks)://[^\s"]*`)

func (s *Supervisor) namedOutbounds(mains []store.Main) []namedOB {
	var out []namedOB
	for _, m := range mains {
		ob, err := xray.ExitToOutbound(m.URL, "main")
		if err != nil {
			msg := exitURLPattern.ReplaceAllString(err.Error(), "<redacted-url>")
			if _, seen := s.badExitLogged.LoadOrStore(m.Name+"\x00"+msg, true); !seen {
				s.Logf(loghub.Error, "route", m.Name, "invalid exit %q: %s", m.Name, msg)
			}
			continue
		}
		out = append(out, namedOB{name: m.Name, ob: ob})
	}
	return out
}

// speedBacked reports whether a plan's entry pick already has throughput data
// (so it was ranked by speed, not just ping). A T1 direct plan has no entry pool
// to upgrade against, so it counts as backed.
func (r *slotRunner) speedBacked(p plan) bool {
	if p.entry == nil {
		return true
	}
	_, n := r.sup.meanSpeed(p.entry.Name)
	return n > 0
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
				return plan{tier: 1, main: m.name, mainOB: m.ob, config: cfg, egress: lat, key: "T1:" + m.name}, true
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
				return plan{tier: tier, entry: n, main: m.name, mainOB: m.ob, config: cfg, egress: lat, key: fmt.Sprintf("T%d:%s:%s", tier, m.name, n.Name)}, true
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

	speedInterval = 15 * time.Minute // rare auto-speedtest cadence
	speedBudget   = 8 * time.Second  // per-node download window (past slow-start)
	speedWindow   = time.Hour        // rolling window behind the mean throughput
)

// --- flakiness / stability ---------------------------------------------------
//
// Every pool refresh (~20s) reachability-probes each entry node; we keep the last
// hour of those pass/fail samples per node and turn them into a flakiness % (share
// that FAILED). It's the leader signal: the cascade now prefers the *most stable*
// entry over the merely-fastest, because a 40ms node that drops one probe in three
// is worse to sit behind than a steady 120ms one. Direct-egress probes are the
// signal (uniform across the pool, one per node per refresh) — not chain probes,
// which are irregular and would bias toward whatever's currently selected.
const (
	flakeWindow     = time.Hour // rolling window behind the flakiness %
	minStabSamples  = 5         // below this the % is too noisy to rank on → treated as "unknown"
	stabBucketPct   = 10.0      // rank by 10%-wide stability bands; finer diffs defer to latency
	unknownFlakePct = 25.0      // stand-in flakiness for under-sampled nodes when ranking (mid-pack)
)

type flakeSample struct {
	t  time.Time
	ok bool
}

// recordProbe appends one reachability outcome for a node and prunes the window.
func (s *Supervisor) recordProbe(name string, ok bool) {
	s.flakeMu.Lock()
	defer s.flakeMu.Unlock()
	if s.flakes == nil {
		s.flakes = map[string][]flakeSample{}
	}
	now := time.Now()
	cut := now.Add(-flakeWindow)
	hist := append(s.flakes[name], flakeSample{t: now, ok: ok})
	i := 0
	for i < len(hist) && hist[i].t.Before(cut) {
		i++
	}
	s.flakes[name] = hist[i:]
}

// flakiness returns the % of the last hour's probes that failed for a node, and
// how many samples that's over (0 samples => 0%, treated as "unknown" by callers).
func (s *Supervisor) flakiness(name string) (pct float64, samples int) {
	s.flakeMu.Lock()
	defer s.flakeMu.Unlock()
	cut := time.Now().Add(-flakeWindow)
	var total, fails int
	for _, smp := range s.flakes[name] {
		if smp.t.Before(cut) {
			continue
		}
		total++
		if !smp.ok {
			fails++
		}
	}
	if total == 0 {
		return 0, 0
	}
	return 100 * float64(fails) / float64(total), total
}

// stabilityBucket is the leader-ranking key (lower = steadier). Under-sampled
// nodes land mid-pack so a proven-steady node beats them but they beat a proven-
// flaky one; ties inside a band fall through to latency.
func (s *Supervisor) stabilityBucket(name string) int {
	pct, n := s.flakiness(name)
	if n < minStabSamples {
		pct = unknownFlakePct
	}
	return int(pct / stabBucketPct)
}

// refreshPools egress-probes both pools (a real 204 straight through each node)
// concurrently, records fastest-first ordering, and updates the dashboard. The
// latency is honest — a raw TCP connect just measured reaching the node's edge.
func (s *Supervisor) refreshPools(ctx context.Context) {
	s.mu.Lock()
	s.speedOff.Store(s.st.NoSpeedTest) // pick up a Config-tab toggle within one refresh
	s.mu.Unlock()
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
			s.recordProbe(n.Name, ok)                                  // feeds the rolling flakiness %
			results[i] = res{n: n, lat: lat, ok: ok}
		}(i, n)
	}
	wg.Wait()

	probes := make([]Probe, len(results))
	for i, r := range results {
		pct, ns := s.flakiness(r.n.Name)
		spd, sn := s.meanSpeed(r.n.Name)
		probes[i] = Probe{Name: r.n.Name, Server: r.n.Server, Whitelist: whitelist, Latency: r.lat, OK: r.ok, Speed: spd, SpeedN: sn, Flakiness: pct, Samples: ns}
	}
	s.setPool(whitelist, probes)

	// Leader order: reachable first, then steadiest (stability band), then FASTEST by
	// mean throughput — ping is only the tiebreak when speed isn't measured for both.
	// Keys are read once so concurrent probes can't change them mid-sort.
	keys := make(map[*store.Node]rankKey, len(results))
	for _, r := range results {
		spd, n := s.meanSpeed(r.n.Name)
		keys[r.n] = rankKey{ok: r.ok, bucket: s.stabilityBucket(r.n.Name), measured: n > 0, speed: spd, lat: r.lat}
	}
	sort.SliceStable(results, func(a, b int) bool { return keys[results[a].n].less(keys[results[b].n]) })
	var out []*store.Node
	for _, r := range results {
		if r.ok {
			out = append(out, r.n)
		}
	}
	return out
}

type rankKey struct {
	ok       bool
	bucket   int
	measured bool
	speed    float64
	lat      time.Duration
}

// less orders entries for leadership. Within a stability band, nodes with a
// measured speed come first, ranked by speed; the rest by ping. Comparing speed
// only when both sides have it would not be transitive, so a slow measured node
// could end up ahead of a fast one depending on input order.
func (a rankKey) less(b rankKey) bool {
	if a.ok != b.ok {
		return a.ok
	}
	if a.bucket != b.bucket {
		return a.bucket < b.bucket
	}
	if a.measured != b.measured {
		return a.measured
	}
	if a.measured && a.speed != b.speed {
		return a.speed > b.speed
	}
	return a.lat < b.lat
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
	r.live, r.liveKey, r.liveTier, r.liveEntry, r.liveMain, r.liveOB = inst, p.key, p.tier, entry, p.main, p.mainOB
	r.hopOnly = p.hopOnly
	r.mu.Unlock()
	return nil
}

func (r *slotRunner) stopLive() {
	r.mu.Lock()
	old := r.live
	r.live, r.liveKey, r.liveTier, r.liveEntry, r.liveMain, r.liveOB = nil, "", 0, "", "", nil
	r.hopOnly = false
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
	r.ss = SlotStatus{Name: r.name, Port: r.mainPort, Tier: tier, Entry: entry, Main: r.liveMain, Egress: egress, Err: errStr, Note: note, HopOnly: r.hopOnly}
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
// fires onChange. The top-level tier/main mirror the primary slot: the TUN slot
// while TUN is ON (that's where system traffic goes), else the first slot — so a
// down TUN-designated slot doesn't paint the whole header ✖ DOWN while the slot
// the user is actually pointing an app at (e.g. the default :2084) is serving fine.
func (s *Supervisor) publish() {
	s.runnersMu.Lock()
	runners := make([]*slotRunner, 0, len(s.runners))
	for _, r := range s.runners {
		runners = append(runners, r)
	}
	s.runnersMu.Unlock()

	s.mu.Lock()
	tunName := s.st.TunSlotName()
	tunEnabled := s.st.TunEnabled
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
	if tunEnabled { // TUN carries system traffic through its slot — mirror that one
		for _, ss := range slots {
			if ss.IsTun {
				primary = ss
				break
			}
		}
	}
	if primary.Name == "" && len(slots) > 0 {
		primary = slots[0] // TUN off (or no tun slot): the first slot is canonical
	}

	s.mu.Lock()
	s.status.Tier = primary.Tier
	s.status.Entry = primary.Entry
	s.status.Main = primary.Main
	s.status.Egress = primary.Egress
	s.status.Err = primary.Err
	s.status.Note = primary.Note
	s.status.HopOnly = primary.HopOnly
	s.status.TunErr = s.tunErrMsg
	s.status.TunUp = s.tunActive.Load()
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
	case want && !s.tunOn && (!s.tunErr || !time.Now().Before(s.tunRetryAt)):
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
			s.tunRefreshServers()
		}
	}
}

// tunRefreshServers follows exits and nodes added or removed while TUN is up, so
// a new server gets its bypass route (Windows/macOS) and static hosts entry and
// probing it doesn't depend on the very exit it may have to replace.
func (s *Supervisor) tunRefreshServers() {
	hosts := s.tunServerHosts()
	sig := strings.Join(hosts, "\n")
	if sig == s.tunServerSig {
		return
	}
	ips, hostMap := s.resolveServers(hosts, s.tunHosts)
	s.tunServerSig, s.tunHosts = sig, hostMap
	s.tunMgr.SetServerIPs(ips)
	xray.SetTunMode(s.tunMark, tun.UplinkDevice(), hostMap)
	s.logf("TUN: server list changed — %d host(s) kept off the tunnel", len(hosts))
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
		// Re-pick + re-apply DNS for the new network: static routed re-asserts the
		// constant exit resolver; real-net adopts the new network's resolver (the old
		// one is now unreachable). Reapply left DNS alone on purpose (mode-dependent).
		realNet := s.cfgBool(func(st *store.State) bool { return st.TunDNSDirect })
		staticDNS := tun.ResolverFor(false, s.cfgStr(func(st *store.State) string { return st.TunStaticDNS }))
		if err := s.tunMgr.ReapplyDNS(staticDNS, realNet); err != nil {
			s.logf("TUN: re-apply DNS after uplink change: %v", err)
		}
		// Refresh xray's egress decoration for the new uplink dev (Linux
		// SO_BINDTODEVICE; no-op on macOS) with the cached hosts, then rebuild every
		// runner so the exits re-dial over the corrected path.
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

// tunRetryGap paces bring-up retries. A failure can be transient: Windows refuses to
// recreate a Wintun adapter for about a minute after a crash left the old one behind.
const tunRetryGap = 20 * time.Second

// tunUpOrLatch brings TUN up, surfacing a failure and scheduling a retry.
func (s *Supervisor) tunUpOrLatch(ctx context.Context, slot string, port int) {
	err := s.tunUp(ctx, slot, port)
	if err == nil {
		return
	}
	msg := "TUN: " + err.Error()
	s.tunErr = true
	s.tunRetryAt = time.Now().Add(tunRetryGap)
	s.mu.Lock()
	changed := s.tunErrMsg != msg
	s.tunErrMsg = msg
	s.mu.Unlock()
	if changed {
		s.Logf(loghub.Error, "engine", "tun", "%s", msg)
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
	srvHosts := s.tunServerHosts()
	ips, hosts := s.resolveServers(srvHosts, nil) // real network — the tunnel isn't up yet
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
	s.tunServerSig = strings.Join(srvHosts, "\n")

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
	s.tunActive.Store(true)
	s.tunErr = false
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
	if err := tun.Prepare(s.logf); err != nil {
		return nil, fmt.Errorf("prepare TUN support: %w", err)
	}
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
	s.tunActive.Store(false)
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

// tunServerHosts lists every slot's exit host and every active node's server,
// sorted and unique.
func (s *Supervisor) tunServerHosts() []string {
	s.mu.Lock()
	var hosts []string
	for i := range s.st.Slots { // every slot's exit must resolve/bypass off-tun
		for _, m := range s.st.Slots[i].Mains {
			hosts = append(hosts, hostOf(m.URL))
		}
	}
	for _, n := range s.st.ActiveNodes() {
		hosts = append(hosts, n.Server)
	}
	s.mu.Unlock()
	sort.Strings(hosts)
	out := hosts[:0]
	for i, h := range hosts {
		if h != "" && (i == 0 || h != hosts[i-1]) {
			out = append(out, h)
		}
	}
	return out
}

// resolveServers returns the IPs to keep off the tunnel (the Windows/macOS bypass
// list — Linux uses fwmark) and a domain→IP map for the live config's static
// hosts (so an exit's own domain resolves locally instead of looping through the
// tunnel it bootstraps). A lookup that fails keeps the address in prev, if any.
func (s *Supervisor) resolveServers(srvHosts []string, prev map[string]string) (ips []net.IP, hosts map[string]string) {
	hosts = map[string]string{}
	for _, h := range srvHosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		addrs, err := net.LookupIP(h)
		if err != nil {
			s.logf("TUN: resolve %s: %v", h, err)
			if ip := net.ParseIP(prev[h]); ip != nil {
				ips = append(ips, ip)
				hosts[h] = prev[h]
			}
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
