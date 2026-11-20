package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPlain(t *testing.T) {
	cases := []struct {
		url   string
		plain bool
	}{
		{"vless://x@45.1.1.1:444?type=tcp&security=none", true},
		{"vless://x@45.1.1.1:444?type=tcp", true}, // no security param → none
		{"vless://x@1.1.1.1:443?type=xhttp&security=reality&pbk=k", false},
		{"vless://x@1.1.1.1:443?type=tcp&security=reality&flow=xtls-rprx-vision", false},
		{"vless://x@1.1.1.1:443?type=tcp&security=tls", false},
		{"vless://x@1.1.1.1:443?flow=xtls-rprx-vision", false},
	}
	for _, c := range cases {
		if got := IsPlain(c.url); got != c.plain {
			t.Errorf("IsPlain(%q) = %v, want %v", c.url, got, c.plain)
		}
	}
}

func TestProtoTag(t *testing.T) {
	url := map[string]string{
		"vless://a@h:444?type=tcp&security=none":                          "plain",
		"vless://a@h:443?type=xhttp&security=reality":                     "reality·xhttp",
		"vless://a@h:443?type=tcp&security=reality&flow=xtls-rprx-vision": "vision",
		"vless://a@h:443?type=grpc&security=reality":                      "reality·grpc",
		"vless://a@h:443?type=ws&security=tls":                            "tls·ws",
	}
	for u, want := range url {
		if got := ProtoTag(u); got != want {
			t.Errorf("ProtoTag(%q) = %q, want %q", u, got, want)
		}
	}
	ob := map[string]string{
		`{"settings":{"vnext":[{"users":[{"flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"tcp","security":"reality"}}`: "vision",
		`{"settings":{"vnext":[{"users":[{}]}]},"streamSettings":{"network":"grpc","security":"reality"}}`:                         "reality·grpc",
		`{"settings":{"vnext":[{"users":[{}]}]},"streamSettings":{"network":"ws","security":"tls"}}`:                               "tls·ws",
	}
	for j, want := range ob {
		if got := OutboundProtoTag([]byte(j)); got != want {
			t.Errorf("OutboundProtoTag = %q, want %q", got, want)
		}
	}
}

func TestListenHost(t *testing.T) {
	state := &State{}
	if got := state.ListenHost(); got != "127.0.0.1" {
		t.Fatalf("default ListenHost() = %q", got)
	}
	state.ListenAddr = "0.0.0.0"
	if got := state.ListenHost(); got != "0.0.0.0" {
		t.Fatalf("explicit LAN ListenHost() = %q", got)
	}
}

func TestListenAddrUpgrade(t *testing.T) {
	dir := t.TempDir()
	fresh, err := Load(filepath.Join(dir, "new.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.ListenHost(); got != "127.0.0.1" {
		t.Fatalf("new install ListenHost() = %q", got)
	}
	old := filepath.Join(dir, "old.json")
	if err := os.WriteFile(old, []byte(`{"listen_port":2084}`), 0o600); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Load(old)
	if err != nil {
		t.Fatal(err)
	}
	if got := upgraded.ListenHost(); got != "0.0.0.0" {
		t.Fatalf("upgraded ListenHost() = %q, want the LAN reach it had before", got)
	}
}

func TestRenameSlot(t *testing.T) {
	state := &State{
		Slots: []Slot{
			{Name: "slot1", Port: 2084, Enabled: true},
			{Name: "slot2", Port: 2086, Enabled: true},
		},
		TunSlot: "slot1",
	}
	// Valid rename of slot1 to primary
	if err := state.RenameSlot(0, "primary"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Slots[0].Name != "primary" {
		t.Fatalf("slot name = %q, want 'primary'", state.Slots[0].Name)
	}
	if state.TunSlot != "primary" {
		t.Fatalf("TunSlot = %q, want 'primary'", state.TunSlot)
	}

	// Duplicate name
	if err := state.RenameSlot(0, "slot2"); err == nil {
		t.Fatal("expected error on duplicate slot name")
	}

	// Empty name
	if err := state.RenameSlot(0, "   "); err == nil {
		t.Fatal("expected error on empty slot name")
	}

	// Name containing ://
	if err := state.RenameSlot(0, "vless://invalid"); err == nil {
		t.Fatal("expected error on URL slot name")
	}
}

func TestMoveMain(t *testing.T) {
	sl := Slot{Mains: []Main{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}}
	names := func() string {
		var out string
		for _, m := range sl.Mains {
			out += m.Name
		}
		return out
	}
	for _, step := range []struct {
		from, to int
		ok       bool
		want     string
	}{
		{0, 2, true, "bcad"},
		{3, 0, true, "dbca"},
		{1, 1, false, "dbca"},
		{-1, 0, false, "dbca"},
		{0, 4, false, "dbca"},
	} {
		if ok := sl.MoveMain(step.from, step.to); ok != step.ok || names() != step.want {
			t.Fatalf("MoveMain(%d, %d) = %v, order %s; want %v, %s", step.from, step.to, ok, names(), step.ok, step.want)
		}
	}
	if sl.MainIndex("") != 0 || sl.HasMain("missing") {
		t.Fatal("MainIndex/HasMain disagree with the slot")
	}
}

func TestRemoveSubKeepsActiveSelection(t *testing.T) {
	s := &State{Subs: []Subscription{{Name: "A"}, {Name: "B"}, {Name: "C"}}, ActiveSub: 1}
	s.RemoveSub(0)
	if s.Subs[s.ActiveSub].Name != "B" {
		t.Fatalf("active sub became %s", s.Subs[s.ActiveSub].Name)
	}
	s.RemoveSub(0) // the active one: fall back to what is now first
	if s.ActiveSub != 0 || s.Subs[0].Name != "C" {
		t.Fatalf("active = %d over %v", s.ActiveSub, s.Subs)
	}
}

func TestSlotPortConflicts(t *testing.T) {
	s := &State{ListenPort: 2084, Slots: []Slot{{Name: "a", Port: 2084}, {Name: "b", Port: 2086}}}
	for _, c := range []struct {
		slot, port int
		clash      bool
	}{
		{1, 2084, true},  // a's SOCKS port
		{1, 2083, true},  // its hop would be a's port
		{1, 2085, true},  // a's hop
		{1, 2088, false}, // free pair
		{0, 2087, true},  // b's hop
		{0, 2090, false},
	} {
		if got := s.SlotPortConflict(c.slot, c.port); got != c.clash {
			t.Errorf("SlotPortConflict(%d, %d) = %v", c.slot, c.port, got)
		}
	}
	s.EntryPort = 3000 // the first slot's hop moves off Port+1
	if s.HopPort(0) != 3000 || s.SlotPortConflict(1, 2085) {
		t.Errorf("hop override ignored: HopPort(0) = %d", s.HopPort(0))
	}
	if !s.SlotPortConflict(1, 2999) || !s.SlotPortConflict(1, 3000) {
		t.Error("a slot may not take the overridden first-hop port")
	}
	for port, clash := range map[int]bool{0: false, 2084: true, 2086: true, 2087: true, 3001: false} {
		if got := s.EntryPortConflict(port); got != clash {
			t.Errorf("EntryPortConflict(%d) = %v", port, got)
		}
	}
}

func TestMainNameDecodesOnce(t *testing.T) {
	for raw, want := range map[string]string{
		"vless://u@h:1#DE%20100%25": "DE 100%",
		"vless://u@h:1#A+B":         "A+B",
		"vless://u@h:1":             "h:1",
	} {
		if got := mainName(raw); got != want {
			t.Errorf("mainName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLoadAcceptsBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf{\"listen_port\": 2500, \"listen_addr\": \"127.0.0.1\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil || st.ListenPort != 2500 {
		t.Fatalf("Load = %v, %v", st, err)
	}
}

func TestHasMainIgnoresName(t *testing.T) {
	sl := Slot{Mains: []Main{{Name: "Renamed", URL: "vless://u@a.example:443?security=none#old"}}}
	for link, want := range map[string]bool{
		"vless://u@a.example:443?security=none#Renamed": true,
		"vless://u@a.example:443?security=none":         true,
		"vless://u@a.example:444?security=none#old":     false,
	} {
		if got := sl.HasMain(link); got != want {
			t.Errorf("HasMain(%s) = %v", link, got)
		}
	}
}

func TestProxySlotAndProtoTags(t *testing.T) {
	st := &State{ListenPort: 2084, Slots: []Slot{{Name: "main", Port: 2084, Enabled: true}}}
	p := st.AddSlotOfType("res", SlotTypeProxy)
	if !p.IsProxy() || p.Kind() != SlotTypeProxy || p.Type != SlotTypeProxy {
		t.Fatalf("proxy slot = %+v", p)
	}
	v := st.AddSlot("v2")
	if v.IsProxy() || v.Kind() != SlotTypeVLESS || v.Type != "" {
		t.Fatalf("vless slot = %+v", v)
	}
	for in, want := range map[string]string{
		"":           SlotTypeVLESS,
		"vless":      SlotTypeVLESS,
		"proxy":      SlotTypeProxy,
		"http":       SlotTypeProxy,
		"socks5":     SlotTypeProxy,
		"http/socks": SlotTypeProxy,
	} {
		got, ok := NormalizeSlotType(in)
		if !ok || got != want {
			t.Errorf("NormalizeSlotType(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
	if _, ok := NormalizeSlotType("wireguard"); ok {
		t.Error("expected unknown slot type to fail")
	}

	p.AddMain("socks5://alice:secret@1.2.3.4:1080#Office")
	p.AddMain("http://5.6.7.8:8080")
	if len(p.DirectMains()) != 2 || len(p.HopMains()) != 2 {
		t.Fatalf("proxy mains should be eligible for both direct and hop: direct=%d hop=%d", len(p.DirectMains()), len(p.HopMains()))
	}
	if p.Mains[0].Name != "Office" || p.Mains[1].Name != "5.6.7.8:8080" {
		t.Fatalf("main names = %q, %q", p.Mains[0].Name, p.Mains[1].Name)
	}
	for u, want := range map[string]string{
		"socks5://alice:secret@1.2.3.4:1080#Office": "socks5·auth",
		"socks5://1.2.3.4:1080":                     "socks5",
		"http://u:p@1.2.3.4:8080":                   "http·auth",
		"http://1.2.3.4:8080":                       "http",
	} {
		if got := ProtoTag(u); got != want {
			t.Errorf("ProtoTag(%q) = %q, want %q", u, got, want)
		}
		if !IsProxyURL(u) || !IsPlain(u) {
			t.Errorf("IsProxyURL/IsPlain(%q) expected true", u)
		}
	}
}
