// Package store is the on-disk storage for the clash_vless TUI: a list of
// subscriptions (each with its own cached nodes), a single stable device
// identity presented to every panel, the main outbound, and engine tunables.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Node is a parsed proxy node from a subscription.
type Node struct {
	Raw       string          `json:"raw,omitempty"`
	Name      string          `json:"name"`
	Protocol  string          `json:"protocol"`
	Server    string          `json:"server"`
	Port      int             `json:"port"`
	Whitelist bool            `json:"whitelist"` // ОБХОД/bypass pool vs country exit pool
	Outbound  json.RawMessage `json:"outbound,omitempty"`
}

// Device is the identity presented to every panel (a Happ-mimic). One stable
// HWID is reused across all subscriptions so we occupy a single device slot each.
type Device struct {
	HWID   string `json:"hwid"`
	OS     string `json:"os"`
	OSVer  string `json:"os_ver"`
	Model  string `json:"model"`
	Locale string `json:"locale"`
	UA     string `json:"ua"`
}

// Subscription is one profile: a URL plus its cached nodes.
type Subscription struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Nodes     []Node    `json:"nodes"`
	LastFetch time.Time `json:"last_fetch"`
}

// Main is a final-exit candidate. AllowNoHop lets it serve directly (T1);
// otherwise it is used only through a hop (T2/T3). Vision exits can never be
// hopped (chaining strips the XTLS flow), so they must run direct.
type Main struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Enabled    bool   `json:"enabled"`
	AllowNoHop bool   `json:"allow_no_hop"`
}

// Slot is a failover pool of mains served on its own local SOCKS port. Its
// supervisor keeps the best working main of the pool online exactly like the
// single-pool model — a slot IS that model, just one of several. Every enabled
// slot runs at once, each on its Port; one slot is selected for TUN (see
// State.TunSlot) and the TUN bridge attaches to that slot's port only. The other
// slots are independent local proxies — their dials are kept off-tun (like every
// xray socket in TUN mode) so they never loop into the tunnel.
type Slot struct {
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Enabled bool   `json:"enabled"`
	Mains   []Main `json:"mains"`
}

// DirectMains returns the slot's enabled mains eligible to serve without a hop (T1).
func (sl *Slot) DirectMains() []Main {
	var out []Main
	for _, m := range sl.Mains {
		if m.Enabled && m.AllowNoHop {
			out = append(out, m)
		}
	}
	return out
}

// HopMains returns the slot's enabled mains that can be dialed through a hop
// (T2/T3). Vision exits are excluded — the chain strips their XTLS flow.
func (sl *Slot) HopMains() []Main {
	var out []Main
	for _, m := range sl.Mains {
		if m.Enabled && !IsVision(m.URL) {
			out = append(out, m)
		}
	}
	return out
}

// AddMain appends a main to the slot (same defaults as State.AddMain).
func (sl *Slot) AddMain(u string) {
	sl.Mains = append(sl.Mains, Main{Name: mainName(u), URL: u, Enabled: true, AllowNoHop: IsVision(u) || IsReality(u)})
}

// RemoveMain deletes the main at index i within the slot.
func (sl *Slot) RemoveMain(i int) {
	if i < 0 || i >= len(sl.Mains) {
		return
	}
	sl.Mains = append(sl.Mains[:i], sl.Mains[i+1:]...)
}

// State is the persisted storage document.
type State struct {
	Subs       []Subscription `json:"subs"`
	ActiveSub  int            `json:"active_sub"`  // index into Subs (used when !AutoSelect)
	AutoSelect bool           `json:"auto_select"` // true = aggregate every sub's nodes

	Device  Device `json:"device"`
	Slots   []Slot `json:"slots"`    // failover pools, each on its own port; one is the TUN slot
	TunSlot string `json:"tun_slot"` // name of the slot the TUN bridge attaches to ("" = first enabled)

	Mains []Main `json:"mains,omitempty"` // legacy single pool — migrated into Slots[0] on load

	// engine tuning — editable in the TUI config tab (0 = built-in default).
	ListenPort    int    `json:"listen_port"`
	EntryPort     int    `json:"entry_port"`  // first-hop local port while chained (0 = auto: ListenPort+1)
	ListenAddr    string `json:"listen_addr"` // bind address for local inbound(s); "" = 0.0.0.0 (LAN-reachable)
	Interval      int    `json:"interval_s"`
	Timeout       int    `json:"timeout_s"`
	UpThreshold   int    `json:"up_threshold"`
	DownThreshold int    `json:"down_threshold"`
	PinTier       int    `json:"pin_tier"`
	PinEntry      string `json:"pin_entry"`       // pinned entry node name ("" = auto-select)
	ForceHop      bool   `json:"force_hop"`       // skip T1 direct — always route through a hop (T2/T3)
	FetchProxy    string `json:"fetch_proxy"`     // proxy for subscription fetches (socks5://, http://, or bare host:port=socks5)
	UseFetchProxy bool   `json:"use_fetch_proxy"` // route fetches through FetchProxy
	LogLevel      string `json:"log_level"`       // xray verbosity: none|error|warning|info|debug ("" = warning)

	// TUN mode (system-wide capture; Linux + Windows). A separate persistent
	// "bridge" xray instance (tun inbound → local SOCKS) owns the device, so
	// failover swaps the exit behind the SOCKS port without churning the TUN.
	// Requires root (Linux) / admin (Windows). See internal/tun.
	TunEnabled bool   `json:"tun_enabled"`
	TunName    string `json:"tun_name"` // device name ("" = clashvless0)
	TunAddr    string `json:"tun_addr"` // interface CIDR ("" = 198.18.0.1/30)
	TunMTU     int    `json:"tun_mtu"`  // 0 = 1500
	// TunDNSDirect selects the DNS mode. true = "real-net": resolve off-tun on the
	// real local network (a /32 bypass out the uplink) using the host's pre-TUN
	// resolver — breaks the domain-node bootstrap deadlock, no public default a
	// corp LAN might firewall. false (default) = "static routed": route DNS through
	// the exit to TunStaticDNS (no leak). See tun.ResolverFor.
	TunDNSDirect bool   `json:"tun_dns_direct"`
	TunStaticDNS string `json:"tun_static_dns"` // resolver for "static routed" mode ("" = 8.8.8.8)
	// TunTunnelLAN routes private/LAN ranges THROUGH the tunnel. Default false =
	// LAN bypass ON: RFC1918 + link-local + multicast are kept on the real network
	// (like Throne's route_exclude_address) so corp intranet / printers / NAS stay
	// reachable. true = capture them too (full tunnel, LAN unreachable via the exit).
	TunTunnelLAN bool `json:"tun_tunnel_lan"`
	// TunAllowIPv6 lets IPv6 escape while TUN is up. Default false = IPv6 block ON:
	// the tunnel is IPv4-only, so on a dual-stack network apps would happy-eyeballs
	// straight out over v6 and leak the real address (bypassing the exit). Blocking
	// global-unicast v6 forces fallback to the tunneled v4. true = leave v6 alone.
	TunAllowIPv6 bool `json:"tun_allow_ipv6"`
	// TunNoICMP disables answering ICMP echo under TUN. Default false = ICMP ping
	// ON: the tun netstack replies to echo requests locally so `ping` works (the
	// SOCKS/VLESS tunnel can't carry ICMP, so this is device liveness, not
	// end-to-end exit reachability — use curl for that). true = leave ping broken.
	TunNoICMP bool `json:"tun_no_icmp"`

	// legacy fields, migrated into Subs / Mains on load.
	LegacyURL       string    `json:"subscription_url,omitempty"`
	LegacyNodes     []Node    `json:"nodes,omitempty"`
	LegacyLastFetch time.Time `json:"last_fetch,omitempty"`
	MainURL         string    `json:"main_url,omitempty"`
	MainChainURL    string    `json:"main_chain_url,omitempty"`

	path string
}

// DefaultUA is the client signature the panel expects (Happ 3.x).
const DefaultUA = "Happ/3.13.0"

// Version is the app version, shown in the TUI header and `version` command.
const Version = "0.18.3"

func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "clash_vless"), nil
}

func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "state.json"), nil
}

// Load reads state from disk, creating a fresh document (with a new stable HWID)
// if none exists, and migrating legacy documents. If path is empty the default
// location is used; if path is an existing directory, its state.json is used.
func Load(path string) (*State, error) {
	if path == "" {
		p, err := Path()
		if err != nil {
			return nil, err
		}
		path = p
	} else if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "state.json")
	}
	s := &State{path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.Device = defaultDevice()
		s.applyDefaults()
		return s, s.Save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.path = path

	changed := false
	if s.migrate() {
		changed = true
	}
	if s.Device.HWID == "" {
		s.Device = defaultDevice()
		changed = true
	}
	if s.applyDefaults() {
		changed = true
	}
	if changed {
		if err := s.Save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// migrate folds pre-multi-sub and single-main documents into Subs / Mains.
func (s *State) migrate() bool {
	changed := false
	if len(s.Subs) == 0 && s.LegacyURL != "" {
		s.Subs = []Subscription{{
			Name:      subName(s.LegacyURL),
			URL:       s.LegacyURL,
			Nodes:     s.LegacyNodes,
			LastFetch: s.LegacyLastFetch,
		}}
		s.ActiveSub = 0
	}
	if s.LegacyURL != "" || s.LegacyNodes != nil {
		s.LegacyURL, s.LegacyNodes, s.LegacyLastFetch = "", nil, time.Time{}
		changed = true
	}
	if len(s.Mains) == 0 {
		if s.MainURL != "" {
			s.Mains = append(s.Mains, Main{Name: mainName(s.MainURL), URL: s.MainURL, Enabled: true, AllowNoHop: true})
		}
		if s.MainChainURL != "" && s.MainChainURL != s.MainURL {
			s.Mains = append(s.Mains, Main{Name: mainName(s.MainChainURL), URL: s.MainChainURL, Enabled: true, AllowNoHop: false})
		}
	}
	if s.MainURL != "" || s.MainChainURL != "" {
		s.MainURL, s.MainChainURL = "", ""
		changed = true
	}
	// Fold the legacy single main pool into the default slot (multi-slot upgrade).
	if len(s.Mains) > 0 && len(s.Slots) == 0 {
		port := s.ListenPort
		if port == 0 {
			port = defaultListenPort
		}
		s.Slots = []Slot{{Name: "main", Port: port, Enabled: true, Mains: s.Mains}}
		s.Mains = nil
		changed = true
	}
	return changed
}

// Save writes the document atomically with 0600 perms (it holds sub tokens).
func (s *State) Save() error {
	if s.path == "" {
		p, err := Path()
		if err != nil {
			return err
		}
		s.path = p
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// An elevated daemon (sudo, for TUN) would otherwise leave state root-owned,
	// locking out the rootless TUI that reads it. Hand it back to the invoker.
	ReownToInvoker(filepath.Dir(s.path))
	ReownToInvoker(s.path)
	return nil
}

// ReownToInvoker best-effort hands a file/dir the elevated daemon created back to
// the human user it serves, so a rootless `clashvless` can read the state and
// connect to the control socket. Under `sudo` that's SUDO_UID/SUDO_GID; under a
// systemd unit (no SUDO_* env) it falls back to the owner of the config tree —
// see invokerIDs. No-op unless running as root with a non-root target found.
// Errors are ignored (best-effort).
func ReownToInvoker(path string) {
	if os.Geteuid() != 0 {
		return
	}
	uid, gid, ok := invokerIDs(path)
	if !ok || uid == 0 {
		return
	}
	_ = os.Chown(path, uid, gid)
}

// FilePath returns the on-disk state file currently in use.
func (s *State) FilePath() string { return s.path }

func (s *State) RawPath() string {
	return filepath.Join(filepath.Dir(s.path), "last_sub.raw")
}

// EventsLogPath is where --debug mirrors the supervisor event log.
func (s *State) EventsLogPath() string {
	return filepath.Join(filepath.Dir(s.path), "events.log")
}

// ControlSocketPath is the daemon's Unix control socket.
func (s *State) ControlSocketPath() string {
	return filepath.Join(filepath.Dir(s.path), "control.sock")
}

// Loglevel returns the xray log verbosity for served instances (default warning).
func (s *State) Loglevel() string {
	switch s.LogLevel {
	case "none", "error", "warning", "info", "debug":
		return s.LogLevel
	default:
		return "warning"
	}
}

// FetchProxyAddr returns the proxy subscriptions should be fetched through
// (socks5://, http://, or bare host:port), or "" when disabled/unset.
func (s *State) FetchProxyAddr() string {
	if s.UseFetchProxy && s.FetchProxy != "" {
		return s.FetchProxy
	}
	return ""
}

// TunAddress is the CIDR assigned to the TUN interface (config or default). The
// gVisor stack is promiscuous, so the exact address is cosmetic — it only needs
// to be a small private subnet the routes can attach to.
func (s *State) TunAddress() string {
	if s.TunAddr != "" {
		return s.TunAddr
	}
	return "198.18.0.1/30"
}

// TunMTUOr is the TUN MTU (config or 1500).
func (s *State) TunMTUOr() int {
	if s.TunMTU > 0 {
		return s.TunMTU
	}
	return 1500
}

// TunStaticResolver is the resolver used in "static routed" DNS mode (queries
// ride the tunnel to the exit). Config or the 8.8.8.8 default.
func (s *State) TunStaticResolver() string {
	if s.TunStaticDNS != "" {
		return s.TunStaticDNS
	}
	return "8.8.8.8"
}

// TunBypassLAN reports whether private/LAN ranges are kept off the tunnel
// (default true). Inverse of TunTunnelLAN so the zero value bypasses.
func (s *State) TunBypassLAN() bool { return !s.TunTunnelLAN }

// TunBlockIPv6 reports whether global-unicast IPv6 is blocked while TUN is up
// (default true). Inverse of TunAllowIPv6 so the zero value blocks the v6 leak.
func (s *State) TunBlockIPv6() bool { return !s.TunAllowIPv6 }

// TunICMP reports whether the tun netstack answers ICMP echo so `ping` works
// (default true). Inverse of TunNoICMP so the zero value enables it.
func (s *State) TunICMP() bool { return !s.TunNoICMP }

// EntryListenPort is the local SOCKS port the current first hop (entry node) is
// exposed on while a chained tier (T2/T3) is active. Config 0 = auto (ListenPort+1).
func (s *State) EntryListenPort() int {
	if s.EntryPort > 0 {
		return s.EntryPort
	}
	return s.ListenPort + 1
}

// ListenHost is the bind address for the local SOCKS inbound(s). Empty config
// means 0.0.0.0 (reachable from the LAN, not just localhost).
func (s *State) ListenHost() string {
	if s.ListenAddr != "" {
		return s.ListenAddr
	}
	return "0.0.0.0"
}

// ActiveNodes returns the node set the engine should draw entries from: every
// sub aggregated (AutoSelect), or just the selected sub. Always a fresh copy.
func (s *State) ActiveNodes() []Node {
	var out []Node
	if s.AutoSelect || s.ActiveSub < 0 || s.ActiveSub >= len(s.Subs) {
		for i := range s.Subs {
			out = append(out, s.Subs[i].Nodes...)
		}
		return out
	}
	return append(out, s.Subs[s.ActiveSub].Nodes...)
}

// FindNodeByName returns the first node (across all subs) with the given name
// that has a usable outbound, or nil.
func (s *State) FindNodeByName(name string) *Node {
	for i := range s.Subs {
		for j := range s.Subs[i].Nodes {
			n := &s.Subs[i].Nodes[j]
			if n.Name == name && len(n.Outbound) > 0 {
				return n
			}
		}
	}
	return nil
}

// DefaultSlot returns the first slot (creating one if the list is somehow
// empty). Used by the wizard, the standalone up/gen commands, and the legacy
// State.AddMain/RemoveMain wrappers — all of which operate on one pool.
func (s *State) DefaultSlot() *Slot {
	if len(s.Slots) == 0 {
		s.Slots = []Slot{{Name: "main", Port: s.ListenPort, Enabled: true}}
	}
	return &s.Slots[0]
}

// SlotIndexByName returns the index of the slot named name, or -1.
func (s *State) SlotIndexByName(name string) int {
	for i := range s.Slots {
		if s.Slots[i].Name == name {
			return i
		}
	}
	return -1
}

// SlotByName returns the slot named name, or nil.
func (s *State) SlotByName(name string) *Slot {
	if i := s.SlotIndexByName(name); i >= 0 {
		return &s.Slots[i]
	}
	return nil
}

func (s *State) firstEnabledSlotName() string {
	for i := range s.Slots {
		if s.Slots[i].Enabled {
			return s.Slots[i].Name
		}
	}
	return ""
}

// TunSlotName is the slot the TUN bridge attaches to: the configured TunSlot if
// it names an enabled slot, else the first enabled slot ("" if none).
func (s *State) TunSlotName() string {
	if i := s.SlotIndexByName(s.TunSlot); i >= 0 && s.Slots[i].Enabled {
		return s.TunSlot
	}
	return s.firstEnabledSlotName()
}

// AddSlot appends a new enabled, empty slot with a unique name + free port.
func (s *State) AddSlot(name string) *Slot {
	name = strings.TrimSpace(name)
	if name == "" || s.SlotIndexByName(name) >= 0 {
		for i := 1; ; i++ {
			cand := fmt.Sprintf("slot-%d", i)
			if s.SlotIndexByName(cand) < 0 {
				name = cand
				break
			}
		}
	}
	s.Slots = append(s.Slots, Slot{Name: name, Port: s.freeSlotPort(), Enabled: true})
	return &s.Slots[len(s.Slots)-1]
}

// RemoveSlot deletes the slot at index i (always keeps at least one slot).
func (s *State) RemoveSlot(i int) {
	if i < 0 || i >= len(s.Slots) || len(s.Slots) <= 1 {
		return
	}
	s.Slots = append(s.Slots[:i], s.Slots[i+1:]...)
}

// freeSlotPort returns the lowest local port not already claimed by the main
// port, the entry-hop port, or another slot.
func (s *State) freeSlotPort() int {
	used := map[int]bool{s.ListenPort: true, s.ListenPort + 1: true}
	if s.EntryPort > 0 {
		used[s.EntryPort] = true
	}
	for i := range s.Slots {
		used[s.Slots[i].Port] = true
	}
	for p := s.ListenPort + 2; p < 65535; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

// AnyUsableMain reports whether any enabled slot has at least one enabled main.
func (s *State) AnyUsableMain() bool {
	for i := range s.Slots {
		if !s.Slots[i].Enabled {
			continue
		}
		if len(s.Slots[i].DirectMains()) > 0 || len(s.Slots[i].HopMains()) > 0 {
			return true
		}
	}
	return false
}

// DirectMains / HopMains / AddMain / RemoveMain operate on the default slot, for
// the wizard and the one-pool standalone commands. Per-slot failover uses the
// Slot methods directly.
func (s *State) DirectMains() []Main { return s.DefaultSlot().DirectMains() }
func (s *State) HopMains() []Main    { return s.DefaultSlot().HopMains() }
func (s *State) AddMain(u string)    { s.DefaultSlot().AddMain(u) }
func (s *State) RemoveMain(i int)    { s.DefaultSlot().RemoveMain(i) }

// IsVision reports whether a vless URL uses XTLS-Vision flow (direct-only).
func IsVision(u string) bool {
	return strings.Contains(u, "xtls-rprx-vision")
}

// IsReality reports whether a vless URL uses REALITY security — DPI-resistant,
// so it works well as a direct exit (T1), not only through a hop.
func IsReality(u string) bool {
	return strings.Contains(u, "security=reality")
}

// IsPlain reports whether a vless URL is a bare transport — no REALITY/TLS
// security and no XTLS-Vision flow. Only a plain main hops through ANY entry;
// a reality/vision main is limited (reality needs a non-Vision hop, Vision is
// direct-only). See the README connection matrix.
func IsPlain(u string) bool {
	return !IsReality(u) && !strings.Contains(u, "security=tls") && !IsVision(u)
}

// protoLabel formats a compact security·transport tag: "vision", "reality",
// "reality·grpc", "plain", "tls·ws", … (tcp transport is left implicit).
func protoLabel(network, security, flow string) string {
	sec := security
	switch {
	case strings.Contains(flow, "xtls-rprx-vision"):
		sec = "vision"
	case security == "" || security == "none":
		sec = "plain"
	}
	if network == "" || network == "tcp" {
		return sec
	}
	return sec + "·" + network
}

// ProtoTag returns the compact proto tag for a vless:// URL (used for mains).
func ProtoTag(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	q := p.Query()
	return protoLabel(q.Get("type"), q.Get("security"), q.Get("flow"))
}

// OutboundProtoTag returns the compact proto tag for a resolved xray outbound
// (used for cached sub nodes, which store the built outbound, not a URL).
func OutboundProtoTag(ob json.RawMessage) string {
	var o struct {
		Settings struct {
			Vnext []struct {
				Users []struct {
					Flow string `json:"flow"`
				} `json:"users"`
			} `json:"vnext"`
		} `json:"settings"`
		StreamSettings struct {
			Network  string `json:"network"`
			Security string `json:"security"`
		} `json:"streamSettings"`
	}
	if json.Unmarshal(ob, &o) != nil {
		return ""
	}
	flow := ""
	if len(o.Settings.Vnext) > 0 && len(o.Settings.Vnext[0].Users) > 0 {
		flow = o.Settings.Vnext[0].Users[0].Flow
	}
	return protoLabel(o.StreamSettings.Network, o.StreamSettings.Security, flow)
}

// mainName derives a readable label for a main from its URL fragment or host.
func mainName(u string) string {
	if p, err := url.Parse(u); err == nil {
		if f := strings.TrimSpace(p.Fragment); f != "" {
			if dec, e := url.QueryUnescape(f); e == nil {
				return strings.TrimSpace(dec)
			}
			return f
		}
		if p.Host != "" {
			return p.Host
		}
	}
	return "main"
}

// AddSub appends a subscription (activating it if it's the first).
func (s *State) AddSub(name, u string) {
	if name == "" {
		name = subName(u)
	}
	s.Subs = append(s.Subs, Subscription{Name: name, URL: u})
	if len(s.Subs) == 1 {
		s.ActiveSub = 0
	}
}

// RemoveSub deletes the subscription at index i, keeping ActiveSub in range.
func (s *State) RemoveSub(i int) {
	if i < 0 || i >= len(s.Subs) {
		return
	}
	s.Subs = append(s.Subs[:i], s.Subs[i+1:]...)
	if s.ActiveSub >= len(s.Subs) {
		s.ActiveSub = len(s.Subs) - 1
	}
	if s.ActiveSub < 0 {
		s.ActiveSub = 0
	}
}

const defaultListenPort = 2084

func (s *State) applyDefaults() (changed bool) {
	set := func(p *int, v int) {
		if *p == 0 {
			*p = v
			changed = true
		}
	}
	set(&s.ListenPort, defaultListenPort)
	set(&s.Interval, 12)
	set(&s.Timeout, 6)
	set(&s.UpThreshold, 3)
	set(&s.DownThreshold, 2)

	// Slots: ensure at least one exists, each has a name + non-colliding port,
	// and TunSlot names an existing (preferably enabled) slot.
	if len(s.Slots) == 0 {
		s.Slots = []Slot{{Name: "main", Port: s.ListenPort, Enabled: true}}
		changed = true
	}
	used := map[int]bool{s.ListenPort: true, s.ListenPort + 1: true}
	if s.EntryPort > 0 {
		used[s.EntryPort] = true
	}
	for i := range s.Slots {
		if s.Slots[i].Port > 0 {
			used[s.Slots[i].Port] = true
		}
	}
	for i := range s.Slots {
		if s.Slots[i].Name == "" {
			s.Slots[i].Name = fmt.Sprintf("slot-%d", i+1)
			changed = true
		}
		if s.Slots[i].Port == 0 {
			p := s.ListenPort + 2
			for used[p] {
				p++
			}
			s.Slots[i].Port, used[p] = p, true
			changed = true
		}
	}
	if s.SlotIndexByName(s.TunSlot) < 0 {
		want := s.firstEnabledSlotName()
		if want == "" {
			want = s.Slots[0].Name
		}
		if s.TunSlot != want {
			s.TunSlot = want
			changed = true
		}
	}
	return
}

func defaultDevice() Device {
	return Device{
		HWID:   newHWID(),
		OS:     "Android",
		OSVer:  "14",
		Model:  "clash-vless-tui",
		Locale: "en",
		UA:     DefaultUA,
	}
}

// newHWID returns a 32-hex-char stable device id (matches /^[a-zA-Z0-9=-]{10,64}$/).
func newHWID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("clash_vless: cannot read crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewHWID mints a fresh stable device id (a new panel device slot).
func NewHWID() string { return newHWID() }

// ValidHWID reports whether s is an acceptable panel HWID — matches the panel's
// /^[a-zA-Z0-9=-]{10,64}$/, so a HWID copied from another device is accepted.
func ValidHWID(s string) bool {
	if len(s) < 10 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '=' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// subName derives a readable fallback name from a sub URL (its host).
func subName(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return "subscription"
}
