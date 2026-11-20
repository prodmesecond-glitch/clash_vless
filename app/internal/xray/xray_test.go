package xray

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/infra/conf/serial"
)

func TestSOCKSInboundUsesConnectionUDPAddress(t *testing.T) {
	for _, listen := range []string{"", "127.0.0.1", "::1", "172.30.0.4", "0.0.0.0", "::"} {
		inbound := socksInbound("in", 2084, listen)
		settings, ok := inbound["settings"].(map[string]any)
		if !ok {
			t.Fatalf("listen %q: settings has unexpected type", listen)
		}
		if settings["udp"] != true {
			t.Errorf("listen %q: UDP is not enabled", listen)
		}
		if ip, exists := settings["ip"]; exists {
			t.Errorf("listen %q: UDP relay address is forced to %v", listen, ip)
		}
	}
}

func TestBuildConfigLeavesAllSOCKSUDPAddressesAutomatic(t *testing.T) {
	main := json.RawMessage(`{"protocol":"freedom"}`)
	entry := json.RawMessage(`{"protocol":"freedom"}`)
	cfg, err := BuildConfig(2084, main, entry, 2085, "none", "0.0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Inbounds []struct {
			Tag      string         `json:"tag"`
			Settings map[string]any `json:"settings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Inbounds) != 2 {
		t.Fatalf("got %d inbounds, want primary and entry", len(decoded.Inbounds))
	}
	for _, inbound := range decoded.Inbounds {
		if _, exists := inbound.Settings["ip"]; exists {
			t.Errorf("%s overrides the per-connection UDP relay address", inbound.Tag)
		}
	}
	if _, err := serial.LoadJSONConfig(bytes.NewReader(cfg)); err != nil {
		t.Fatalf("xray rejected config without a SOCKS UDP address override: %v", err)
	}
}

func TestRelayLeavesSOCKSUDPAddressAutomatic(t *testing.T) {
	cfg, err := BuildRelay(2086, 2084, "none")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Inbounds []struct {
			Settings map[string]any `json:"settings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Inbounds) != 1 {
		t.Fatalf("got %d relay inbounds, want 1", len(decoded.Inbounds))
	}
	if _, exists := decoded.Inbounds[0].Settings["ip"]; exists {
		t.Fatal("loopback relay overrides the UDP relay address")
	}
	if _, err := serial.LoadJSONConfig(bytes.NewReader(cfg)); err != nil {
		t.Fatalf("xray rejected relay config without a SOCKS UDP address override: %v", err)
	}
}

func TestParseProxyFormats(t *testing.T) {
	cases := []struct {
		raw, def string
		want     ProxySpec
	}{
		{"socks5://alice:s3cr3t@1.2.3.4:1080#Office", "", ProxySpec{Scheme: "socks5", Host: "1.2.3.4", Port: 1080, User: "alice", Pass: "s3cr3t", Name: "Office"}},
		{"http://user:pass@proxy.example:8080", "", ProxySpec{Scheme: "http", Host: "proxy.example", Port: 8080, User: "user", Pass: "pass"}},
		{"socks://1.2.3.4:1080", "", ProxySpec{Scheme: "socks5", Host: "1.2.3.4", Port: 1080}},
		{"socks5h://u:p@1.2.3.4:1080", "", ProxySpec{Scheme: "socks5", Host: "1.2.3.4", Port: 1080, User: "u", Pass: "p"}},
		{"1.2.3.4:1080:user:pa:ss", "http", ProxySpec{Scheme: "http", Host: "1.2.3.4", Port: 1080, User: "user", Pass: "pa:ss"}},
		{"user:pa@ss@1.2.3.4:1080", "socks5", ProxySpec{Scheme: "socks5", Host: "1.2.3.4", Port: 1080, User: "user", Pass: "pa@ss"}},
		{"1.2.3.4:1080@user:pass", "http", ProxySpec{Scheme: "http", Host: "1.2.3.4", Port: 1080, User: "user", Pass: "pass"}},
		{"user:pass:1.2.3.4:8080", "http", ProxySpec{Scheme: "http", Host: "1.2.3.4", Port: 8080, User: "user", Pass: "pass"}},
		{"1.2.3.4:1080", "", ProxySpec{Scheme: "socks5", Host: "1.2.3.4", Port: 1080}},
		{"[2001:db8::1]:1080:u:p", "socks5", ProxySpec{Scheme: "socks5", Host: "2001:db8::1", Port: 1080, User: "u", Pass: "p"}},
	}
	for _, tc := range cases {
		got, err := ParseProxy(tc.raw, tc.def)
		if err != nil {
			t.Fatalf("ParseProxy(%q, %q) error: %v", tc.raw, tc.def, err)
		}
		if got != tc.want {
			t.Errorf("ParseProxy(%q, %q) = %+v, want %+v", tc.raw, tc.def, got, tc.want)
		}
		formatted, err := FormatProxyURL(got)
		if err != nil {
			t.Fatalf("FormatProxyURL(%+v) error: %v", got, err)
		}
		roundtrip, err := ParseProxy(formatted, "")
		if err != nil || roundtrip != got {
			t.Errorf("roundtrip(%q) = %+v (%v), want %+v", formatted, roundtrip, err, got)
		}
	}

	for _, bad := range []string{"", "vless://u@h:443", "http://1.2.3.4", "http://1.2.3.4:70000", "not-a-proxy", "1.2.3.4:0"} {
		if _, err := ParseProxy(bad, "socks5"); err == nil {
			t.Errorf("ParseProxy(%q) expected error", bad)
		}
	}
}

func TestProxyOutboundsLoadInXray(t *testing.T) {
	entry, err := VlessToOutbound("vless://11111111-1111-1111-1111-111111111111@entry.example:443?security=none&type=tcp", "entry")
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"http://alice:secret@proxy.example:8080#HTTP",
		"http://proxy.example:8080",
		"socks5://bob:pw@proxy.example:1080#SOCKS",
		"socks5://proxy.example:1080",
	} {
		ob, err := ExitToOutbound(uri, "main")
		if err != nil {
			t.Fatalf("ExitToOutbound(%q): %v", uri, err)
		}
		// Direct (T1)
		cfg, err := BuildConfig(2084, ob, nil, 0, "none", "127.0.0.1")
		if err != nil {
			t.Fatalf("BuildConfig direct(%q): %v", uri, err)
		}
		if _, err := serial.LoadJSONConfig(bytes.NewReader(cfg)); err != nil {
			t.Fatalf("xray rejected direct proxy config for %q: %v", uri, err)
		}
		// Chained through entry hop (T2/T3)
		cfgChained, err := BuildConfig(2084, ob, entry, 2085, "none", "127.0.0.1")
		if err != nil {
			t.Fatalf("BuildConfig chained(%q): %v", uri, err)
		}
		if _, err := serial.LoadJSONConfig(bytes.NewReader(cfgChained)); err != nil {
			t.Fatalf("xray rejected chained proxy config for %q: %v", uri, err)
		}
	}
}
