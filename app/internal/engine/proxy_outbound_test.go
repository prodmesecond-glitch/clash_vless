package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"clashvless/internal/loghub"
	"clashvless/internal/store"
	"clashvless/internal/xray"
)

func TestProxyExitsNamedOutboundsAndRedaction(t *testing.T) {
	var logs []loghub.Record
	sup := NewSupervisor(&store.State{
		Slots: []store.Slot{{
			Name:    "res",
			Type:    store.SlotTypeProxy,
			Port:    2086,
			Enabled: true,
			Mains: []store.Main{
				{Name: "HTTP", URL: "http://alice:secret@proxy1.example:8080#HTTP", Enabled: true, AllowNoHop: true},
				{Name: "SOCKS", URL: "socks5://bob:pw@proxy2.example:1080#SOCKS", Enabled: true, AllowNoHop: true},
				{Name: "Bad", URL: "http://carol:topsecret@bad.example", Enabled: true, AllowNoHop: true},
			},
		}},
	}, nil, func(r loghub.Record) { logs = append(logs, r) })

	obs := sup.namedOutbounds(sup.st.Slots[0].Mains)
	if len(obs) != 2 {
		t.Fatalf("namedOutbounds len = %d, want 2", len(obs))
	}
	var ob0, ob1 struct {
		Protocol string `json:"protocol"`
	}
	_ = json.Unmarshal(obs[0].ob, &ob0)
	_ = json.Unmarshal(obs[1].ob, &ob1)
	if ob0.Protocol != "http" || ob1.Protocol != "socks" {
		t.Fatalf("protocols = %q, %q; want http, socks", ob0.Protocol, ob1.Protocol)
	}
	for _, r := range logs {
		if strings.Contains(r.Message, "topsecret") || strings.Contains(r.Message, "carol") {
			t.Fatalf("log leaked proxy credentials: %q", r.Message)
		}
	}

	hosts := sup.tunServerHosts()
	joined := strings.Join(hosts, ",")
	if !strings.Contains(joined, "proxy1.example") || !strings.Contains(joined, "proxy2.example") {
		t.Fatalf("tunServerHosts = %v, missing proxy hosts", hosts)
	}
}

func TestProxyOutboundsStartInEngine(t *testing.T) {
	entry, err := xray.VlessToOutbound("vless://11111111-1111-1111-1111-111111111111@entry.example:443?security=none&type=tcp", "entry")
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"http://alice:secret@127.0.0.1:18080",
		"socks5://bob:pw@127.0.0.1:11080",
	} {
		ob, err := xray.ExitToOutbound(uri, "main")
		if err != nil {
			t.Fatal(err)
		}
		for _, hop := range []json.RawMessage{nil, entry} {
			port, err := freePort()
			if err != nil {
				t.Skipf("cannot allocate local port in this environment: %v", err)
			}
			cfg, err := xray.BuildConfig(port, ob, hop, 0, "none", "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			inst, err := Start(cfg)
			if err != nil {
				t.Fatalf("Start(%q, hop=%v) failed: %v", uri, hop != nil, err)
			}
			_ = inst.Close()
		}
	}
}
