package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"clashvless/internal/store"
	"clashvless/internal/xray"
)

// Probe is a per-node reachability sample shown in the UI.
type Probe struct {
	Name      string
	Server    string
	Whitelist bool
	Latency   time.Duration
	OK        bool
}

// Status is an immutable snapshot of supervisor state handed to the UI.
type Status struct {
	Tier      int    // 1/2/3 = active tier; 0 = down
	Entry     string // entry node name ("" for T1 direct)
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
	entryPort int // first hop exposed here while chained (0 = disabled)

	kick chan struct{}

	mu        sync.Mutex
	live      *Instance
	liveKey   string
	liveTier  int
	liveEntry string
	status    Status
	onChange  func(Status)
	onLog     func(string)

	// hysteresis counters — only touched from the single Run goroutine.
	upStreak   int
	failStreak int

	// fastest-first pool ordering for the current cycle.
	rankedCountry []*store.Node
	rankedBypass  []*store.Node
}

type plan struct {
	tier   int
	entry  *store.Node
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
	return &Supervisor{
		st:        st,
		mainPort:  st.ListenPort,
		entryPort: entryPort,
		kick:      make(chan struct{}, 1),
		onChange:  onChange,
		onLog:     onLog,
	}
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

func (s *Supervisor) cfgBool(get func(*store.State) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.st)
}

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
	for {
		s.cycle(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
		case <-time.After(s.interval()):
		}
	}
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
	// Two exit variants: the direct main (T1, may use Vision) and the chained
	// main (T2/T3, must be non-Vision). They differ only when the user sets a
	// separate MainChainURL (a flow="" user on the same exit node).
	mainURL := s.cfgStr(func(st *store.State) string { return st.MainURL })
	chainURL := s.cfgStr(func(st *store.State) string { return st.MainChainURL })
	if chainURL == "" {
		chainURL = mainURL
	}
	mainDirect, err := xray.VlessToOutbound(mainURL, "main")
	if err != nil {
		s.emit(0, "", 0, err.Error(), "")
		return
	}
	mainChain, err := xray.VlessToOutbound(chainURL, "main")
	if err != nil {
		s.emit(0, "", 0, err.Error(), "")
		return
	}

	// Refresh pool latencies every cycle (cheap TCP) so the dashboard is live.
	s.refreshPools()

	// A pinned node overrides the whole tier cascade (always a chain: entry → main).
	if pin := s.cfgStr(func(st *store.State) string { return st.PinEntry }); pin != "" {
		s.pinnedCycle(ctx, mainChain, pin)
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
			if up, ok := s.bestWorking(ctx, mainDirect, mainChain, lo, curTier-1); ok {
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
		if best, ok := s.bestWorking(ctx, mainDirect, mainChain, lo, hi); ok {
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
		s.emit(0, "", 0, "all tiers unreachable (main down directly and via every entry)", "")
		return
	}
	// Tolerate a transient blip before switching away.
	s.logf("current chain failing (%d/%d)…", s.failStreak, s.downThresh())
	s.emit(curTier, curEntry, 0, "", fmt.Sprintf("current failing (%d/%d)…", s.failStreak, s.downThresh()))
}

// pinnedCycle serves a specific user-pinned entry node (node → main), bypassing
// the tier cascade. Missing or unreachable => DOWN (the pin is an explicit
// choice; unpin in the Subs tab to restore auto).
func (s *Supervisor) pinnedCycle(ctx context.Context, mainOB json.RawMessage, pin string) {
	node, ok := s.findNodeByName(pin)
	if !ok {
		s.emit(0, "", 0, "pinned node "+pin+" not found (refetch, or unpin in Subs)", "")
		return
	}
	lat, up := s.probe(ctx, mainOB, node.Outbound)
	if !up {
		s.logf("pinned %s unreachable", pin)
		s.emit(0, "", 0, "pinned "+pin+" unreachable (unpin for auto)", "")
		return
	}
	tier := 2
	if node.Whitelist {
		tier = 3
	}
	cfg, err := xray.BuildConfig(s.mainPort, mainOB, node.Outbound, s.entryPort, true)
	if err != nil {
		s.emit(0, "", 0, err.Error(), "")
		return
	}
	p := plan{tier: tier, entry: &node, config: cfg, egress: lat, key: "PIN:" + node.Name}
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

// bestWorking probes tiers lo..hi top-down and returns the first plan that
// reaches the internet, using the fastest working entry for its tier.
func (s *Supervisor) bestWorking(ctx context.Context, mainDirect, mainChain json.RawMessage, lo, hi int) (plan, bool) {
	for tier := lo; tier <= hi; tier++ {
		if p, ok := s.probeTier(ctx, mainDirect, mainChain, tier); ok {
			return p, true
		}
	}
	return plan{}, false
}

func (s *Supervisor) probeTier(ctx context.Context, mainDirect, mainChain json.RawMessage, tier int) (plan, bool) {
	if tier == 1 { // direct: use the Vision-capable main
		if lat, ok := s.probe(ctx, mainDirect, nil); ok {
			cfg, err := xray.BuildConfig(s.mainPort, mainDirect, nil, 0, true)
			if err != nil {
				return plan{}, false
			}
			return plan{tier: 1, config: cfg, egress: lat, key: "T1"}, true
		}
		return plan{}, false
	}
	// chained tiers: use the non-Vision main behind an entry
	ranked := s.rankedCountry
	if tier == 3 {
		ranked = s.rankedBypass
	}
	for i, n := range ranked {
		if i >= maxTryPerTier {
			break
		}
		if lat, ok := s.probe(ctx, mainChain, n.Outbound); ok {
			cfg, err := xray.BuildConfig(s.mainPort, mainChain, n.Outbound, s.entryPort, true)
			if err != nil {
				continue
			}
			return plan{tier: tier, entry: n, config: cfg, egress: lat, key: fmt.Sprintf("T%d:%s", tier, n.Name)}, true
		}
	}
	return plan{}, false
}

// --- probing -----------------------------------------------------------------

// refreshPools TCP-probes both pools, stores the fastest-first ordering for this
// cycle, and updates the dashboard latencies.
func (s *Supervisor) refreshPools() {
	s.rankedCountry = s.rankPool(false)
	s.rankedBypass = s.rankPool(true)
}

func (s *Supervisor) rankPool(whitelist bool) []*store.Node {
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
			lat, err := TCPLatency(n.Server, n.Port, 3*time.Second)
			results[i] = res{n: n, lat: lat, ok: err == nil}
		}(i, n)
	}
	wg.Wait()

	probes := make([]Probe, len(results))
	for i, r := range results {
		probes[i] = Probe{Name: r.n.Name, Server: r.n.Server, Whitelist: whitelist, Latency: r.lat, OK: r.ok}
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
func (s *Supervisor) probe(ctx context.Context, mainOB, entryOB json.RawMessage) (time.Duration, bool) {
	// A fresh OS-assigned port per probe: a fixed ListenPort+1 silently hard-failed
	// every probe (→ permanent DOWN) whenever another service already held that port.
	port, err := freePort()
	if err != nil {
		return 0, false
	}
	cfg, err := xray.BuildConfig(port, mainOB, entryOB, 0, true)
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
	lat, err := EgressThroughSOCKS(ctx, port, s.timeout())
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
	s.live, s.liveKey, s.liveTier, s.liveEntry = inst, p.key, p.tier, entry
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) stopLive() {
	s.mu.Lock()
	old := s.live
	s.live, s.liveKey, s.liveTier, s.liveEntry = nil, "", 0, ""
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
