package engine

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"clashvless/internal/store"
	"clashvless/internal/xray"
)

func TestExitAddressesRemainIndependentFailoverCandidates(t *testing.T) {
	slot := store.Slot{Name: "main", Port: 2084, Enabled: true}
	for _, host := range []string{"exit.example", "192.0.2.10"} {
		slot.AddMain("vless://11111111-1111-1111-1111-111111111111@" + host + ":443?security=reality&sni=exit.example&pbk=k&type=tcp#Exit")
	}
	s := &Supervisor{st: &store.State{Slots: []store.Slot{slot}}}
	direct, hop := s.namedOutbounds(slot.DirectMains()), s.namedOutbounds(slot.HopMains())
	if len(direct) != 2 || len(hop) != 2 || bytes.Equal(direct[0].ob, direct[1].ob) {
		t.Fatal("DDNS and IP must produce distinct direct and hop outbounds even with identical names")
	}
	r := &slotRunner{sup: s}
	for i, host := range []string{"exit.example", "192.0.2.10"} {
		for _, tier := range []int{1, 2, 3} {
			if !r.followLiveMain(direct[i].ob, tier, direct, hop) {
				t.Fatalf("%s is not accepted as the live exit at tier %d", host, tier)
			}
		}
		cfg, err := xray.BuildConfig(2084, direct[i].ob, nil, 0, "none", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Outbounds []struct {
				Tag      string `json:"tag"`
				Settings struct {
					Vnext []struct{ Address string } `json:"vnext"`
				} `json:"settings"`
			} `json:"outbounds"`
		}
		if err := json.Unmarshal(cfg, &config); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, outbound := range config.Outbounds {
			if outbound.Tag == "main" && len(outbound.Settings.Vnext) == 1 && outbound.Settings.Vnext[0].Address == host {
				found = true
			}
		}
		if !found {
			t.Fatalf("live configuration lost the selected address %s", host)
		}
	}
	if got := s.tunServerHosts(); !reflect.DeepEqual(got, []string{"192.0.2.10", "exit.example"}) {
		t.Fatalf("TUN bypass lost an address: %v", got)
	}
}
