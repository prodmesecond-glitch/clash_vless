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

// State is the persisted storage document.
type State struct {
	Subs       []Subscription `json:"subs"`
	ActiveSub  int            `json:"active_sub"`  // index into Subs (used when !AutoSelect)
	AutoSelect bool           `json:"auto_select"` // true = aggregate every sub's nodes

	Device Device `json:"device"`
	Mains  []Main `json:"mains"` // final-exit candidates: direct (w/o-hop → T1) and/or hopped (T2/T3)

	// engine tuning — editable in the TUI config tab (0 = built-in default).
	ListenPort    int `json:"listen_port"`
	EntryPort     int    `json:"entry_port"`  // first-hop local port while chained (0 = auto: ListenPort+1)
	ListenAddr    string `json:"listen_addr"` // bind address for local inbound(s); "" = 0.0.0.0 (LAN-reachable)
	Interval      int `json:"interval_s"`
	Timeout       int `json:"timeout_s"`
	UpThreshold   int    `json:"up_threshold"`
	DownThreshold int    `json:"down_threshold"`
	PinTier       int    `json:"pin_tier"`
	PinEntry      string `json:"pin_entry"` // pinned entry node name ("" = auto-select)
	ForceHop      bool   `json:"force_hop"` // skip T1 direct — always route through a hop (T2/T3)
	FetchProxy    string `json:"fetch_proxy"`     // host:port SOCKS5 proxy for subscription fetches
	UseFetchProxy bool   `json:"use_fetch_proxy"` // route fetches through FetchProxy
	LogLevel      string `json:"log_level"`       // xray verbosity: none|error|warning|info|debug ("" = warning)

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
const Version = "0.7.0"

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
	return os.Rename(tmp, s.path)
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

// FetchProxyAddr returns the SOCKS5 proxy subscriptions should be fetched
// through, or "" when disabled/unset.
func (s *State) FetchProxyAddr() string {
	if s.UseFetchProxy && s.FetchProxy != "" {
		return s.FetchProxy
	}
	return ""
}

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

// DirectMains returns enabled mains eligible to serve without a hop (T1).
func (s *State) DirectMains() []Main {
	var out []Main
	for _, m := range s.Mains {
		if m.Enabled && m.AllowNoHop {
			out = append(out, m)
		}
	}
	return out
}

// HopMains returns enabled mains that can be dialed through a hop (T2/T3).
// Vision exits are excluded — the chain strips their XTLS flow.
func (s *State) HopMains() []Main {
	var out []Main
	for _, m := range s.Mains {
		if m.Enabled && !IsVision(m.URL) {
			out = append(out, m)
		}
	}
	return out
}

// AddMain appends a main. New mains are enabled; a Vision or REALITY exit defaults
// to direct-capable (w/o-hop on), a plain exit defaults to hop-only (w/o-hop off).
func (s *State) AddMain(u string) {
	s.Mains = append(s.Mains, Main{Name: mainName(u), URL: u, Enabled: true, AllowNoHop: IsVision(u) || IsReality(u)})
}

// RemoveMain deletes the main at index i.
func (s *State) RemoveMain(i int) {
	if i < 0 || i >= len(s.Mains) {
		return
	}
	s.Mains = append(s.Mains[:i], s.Mains[i+1:]...)
}

// IsVision reports whether a vless URL uses XTLS-Vision flow (direct-only).
func IsVision(u string) bool {
	return strings.Contains(u, "xtls-rprx-vision")
}

// IsReality reports whether a vless URL uses REALITY security — DPI-resistant,
// so it works well as a direct exit (T1), not only through a hop.
func IsReality(u string) bool {
	return strings.Contains(u, "security=reality")
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

func (s *State) applyDefaults() (changed bool) {
	set := func(p *int, v int) {
		if *p == 0 {
			*p = v
			changed = true
		}
	}
	set(&s.ListenPort, 2084)
	set(&s.Interval, 12)
	set(&s.Timeout, 6)
	set(&s.UpThreshold, 3)
	set(&s.DownThreshold, 2)
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

// subName derives a readable fallback name from a sub URL (its host).
func subName(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return "subscription"
}
