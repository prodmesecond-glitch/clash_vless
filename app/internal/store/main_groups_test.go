package store

import (
	"reflect"
	"testing"
)

func TestGroupedFailoverPriority(t *testing.T) {
	const primary = "vless://a@exit.example:443?security=reality&sni=exit.example&pbk=k&type=tcp#A"
	const fallback = "vless://a@192.0.2.10:443?type=tcp&pbk=k&sni=exit.example&security=reality#Fallback"
	const other = "vless://b@other.example:443?security=reality&sni=other.example&pbk=k&type=tcp#B"
	for _, tc := range []struct {
		name   string
		change func(*Slot)
		direct []string
		hop    []string
	}{
		{"all addresses", func(*Slot) {}, []string{primary, fallback, other}, []string{primary, fallback, other}},
		{"disabled leader", func(s *Slot) { s.Mains[0].Enabled = false }, []string{fallback, other}, []string{fallback, other}},
		{"hop-only leader", func(s *Slot) { s.Mains[0].AllowNoHop = false }, []string{fallback, other}, []string{primary, fallback, other}},
		{"disabled fallback", func(s *Slot) { s.Mains[2].Enabled = false }, []string{primary, other}, []string{primary, other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slot := Slot{}
			for _, link := range []string{primary, other, fallback} {
				slot.AddMain(link)
			}
			tc.change(&slot)
			before := append([]Main(nil), slot.Mains...)
			for tier, mains := range map[string][]Main{"direct": slot.DirectMains(), "hop": slot.HopMains()} {
				var got []string
				for _, m := range mains {
					got = append(got, m.URL)
				}
				want := tc.direct
				if tier == "hop" {
					want = tc.hop
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s failover order = %v, want %v", tier, got, want)
				}
			}
			if !reflect.DeepEqual(slot.Mains, before) {
				t.Fatal("selecting candidates rewrote stored addresses or flags")
			}
		})
	}
}
