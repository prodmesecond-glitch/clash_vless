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

// State is the persisted storage document.
type State struct {
	Subs       []Subscription `json:"subs"`
	ActiveSub  int            `json:"active_sub"`  // index into Subs (used when !AutoSelect)
	AutoSelect bool           `json:"auto_select"` // true = aggregate every sub's nodes

	Device       Device `json:"device"`
	MainURL      string `json:"main_url"`       // direct exit (T1) — may use XTLS-Vision
	MainChainURL string `json:"main_chain_url"` // chained exit (T2/T3) — must be flow="" (non-Vision); falls back to MainURL

	// engine tuning — editable in the TUI config tab (0 = built-in default).
	ListenPort    int `json:"listen_port"`
	Interval      int `json:"interval_s"`
	Timeout       int `json:"timeout_s"`
	UpThreshold   int    `json:"up_threshold"`
	DownThreshold int    `json:"down_threshold"`
	PinTier       int    `json:"pin_tier"`
	PinEntry      string `json:"pin_entry"` // pinned entry node name ("" = auto-select)

	// legacy single-sub fields, migrated into Subs on load.
	LegacyURL       string    `json:"subscription_url,omitempty"`
	LegacyNodes     []Node    `json:"nodes,omitempty"`
	LegacyLastFetch time.Time `json:"last_fetch,omitempty"`

	path string
}

// DefaultUA is the client signature the panel expects (Happ 3.x).
const DefaultUA = "Happ/3.13.0"

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
// if none exists, and migrating any legacy single-subscription document.
func Load() (*State, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	s := &State{path: p}

	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		s.Device = defaultDevice()
		s.applyDefaults()
		return s, s.Save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	s.path = p

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

// migrate folds a pre-multi-sub document into the Subs list.
func (s *State) migrate() bool {
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
		return true
	}
	return false
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

func (s *State) RawPath() string {
	return filepath.Join(filepath.Dir(s.path), "last_sub.raw")
}

// EventsLogPath is where --debug mirrors the supervisor event log.
func (s *State) EventsLogPath() string {
	return filepath.Join(filepath.Dir(s.path), "events.log")
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
	set(&s.ListenPort, 2080)
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
