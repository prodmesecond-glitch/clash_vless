//go:build !android && cgo

package gui

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"clashvless/internal/engine"
	"clashvless/internal/store"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
)

var fallbackA = strings.Replace(strings.Replace(linkA, "a.example", "192.0.2.10", 1), "#A", "#Fallback", 1)

func TestGroupMainsOnlyIgnoresServerAndName(t *testing.T) {
	for name, link := range map[string]string{
		"address":     fallbackA,
		"IPv6":        strings.Replace(linkA, "a.example", "[2001:db8::1]", 1),
		"query order": "vless://11111111-1111-1111-1111-111111111111@192.0.2.10:443?type=tcp&pbk=k&sni=%61&security=reality#Fallback",
	} {
		t.Run(name, func(t *testing.T) {
			if mainGroupKey(link) != mainGroupKey(linkA) {
				t.Fatal("the same exit's addresses were split")
			}
		})
	}
	for name, link := range map[string]string{
		"UUID":              strings.Replace(linkA, "11111111-", "22222222-", 1),
		"port":              strings.Replace(linkA, ":443", ":8443", 1),
		"SNI":               strings.Replace(linkA, "sni=a", "sni=b", 1),
		"key":               strings.Replace(linkA, "pbk=k", "pbk=other", 1),
		"security":          strings.Replace(linkA, "security=reality", "security=tls", 1),
		"transport":         strings.Replace(linkA, "type=tcp", "type=ws", 1),
		"flow":              strings.Replace(linkA, "#A", "&flow=xtls-rprx-vision#A", 1),
		"short ID":          strings.Replace(linkA, "#A", "&sid=abcd#A", 1),
		"path":              strings.Replace(linkA, "#A", "&path=%2Fother#A", 1),
		"unknown parameter": strings.Replace(linkA, "#A", "&extra=value#A", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if mainGroupKey(link) == mainGroupKey(linkA) {
				t.Fatal("different configurations were merged")
			}
		})
	}
}

func groupedExitDashboard(t *testing.T) (*dashboard, func()) {
	t.Helper()
	d, nextPatch := threeExitDashboard(t)
	d.state.Slots[0].AddMain(fallbackA) // deliberately separated from A by B and C
	d.render()
	return d, nextPatch
}

func TestImportGroupedAddresses(t *testing.T) {
	d, commands := routesDashboard(t)
	d.addMain(0)
	links := linkA + "\n" + fallbackA + "\n" + linkA
	routeDialogEntry(t, d).SetText(base64.StdEncoding.EncodeToString([]byte(links)))
	tapRouteDialogButton(t, d, "Add")
	applyRoutePatch(t, d, commands)
	slot := d.state.Slots[0]
	if len(slot.Mains) != 2 || slot.Mains[0].URL != linkA || slot.Mains[1].URL != fallbackA {
		t.Fatal("import lost an address or kept a duplicate")
	}
	if len(slot.DirectMains()) != 2 || len(slot.HopMains()) != 2 {
		t.Fatal("both addresses must remain available to direct and hopped failover")
	}
	if len(iconButtons(d, "Delete exit")) != 1 || !routeHasText(d.content, "1 in priority order: the top working exit is used") {
		t.Fatal("addresses should appear as one final exit")
	}
	if got := visibleFitTexts(d.content, "a.example:443 · 192.0.2.10:443"); len(got) != 1 {
		t.Fatal("the card must show both server addresses")
	}
	if width := d.window.Content().MinSize().Width; width > 760 {
		t.Fatalf("grouped addresses force the window too wide: %.0f", width)
	}
	// A working fallback may have a different fragment/name from the DDNS link.
	d.status.Slots = []engine.SlotStatus{{Name: "first", Tier: 1, Main: "Fallback"}}
	d.render()
	if !routeHasText(d.content, "In use · T1") {
		t.Fatal("the group is not marked in use when its fallback serves")
	}
	iconButtons(d, "Share link / QR code")[0].Tapped(nil)
	tapRouteDialogButton(t, d, "Copy all links")
	if got := d.app.Clipboard().Content(); got != linkA+"\n"+fallbackA {
		t.Fatal("group export must preserve both links")
	}
}

func TestGroupedExitActionsPreserveAddresses(t *testing.T) {
	d, nextPatch := groupedExitDashboard(t)
	iconButtons(d, "Lower priority")[0].Tapped(nil)
	nextPatch()
	if got := mainNames(d.state.Slots[0]); got != "B,A,Fallback,C" {
		t.Fatalf("group priority did not move both addresses: %s", got)
	}
	checks := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		check, ok := obj.(*widget.Check)
		return ok && check.Text == "Enabled"
	})
	checks[1].(*widget.Check).SetChecked(false)
	nextPatch()
	for _, i := range []int{1, 2} {
		if d.state.Slots[0].Mains[i].Enabled {
			t.Fatal("disabling the group left an address enabled")
		}
	}
	d.setMainFlag(0, 1, func(m *store.Main) { m.Enabled = true; m.AllowNoHop = false })
	nextPatch()
	if got := len(d.state.Slots[0].DirectMains()); got != 0 {
		t.Fatalf("direct disabled for the group, but %d direct candidates remain", got)
	}
	if got := len(d.state.Slots[0].HopMains()); got != 4 {
		t.Fatalf("hop candidates lost after changing flags: %d", got)
	}
	iconButtons(d, "Rename exit")[1].Tapped(nil)
	// Confirm against a newer order, using the captured URLs rather than indices.
	d.state.Slots[0].MoveMain(2, 0)
	routeDialogEntry(t, d).SetText("Unified exit")
	tapRouteDialogButton(t, d, "Save")
	nextPatch()
	var urls []string
	for _, m := range d.state.Slots[0].Mains {
		if m.Name == "Unified exit" {
			urls = append(urls, m.URL)
		}
	}
	if !reflect.DeepEqual(urls, []string{fallbackA, linkA}) {
		t.Fatal("rename lost or rewrote an address")
	}
	iconButtons(d, "Delete exit")[0].Tapped(nil)
	d.state.Slots[0].MoveMain(0, 3)
	tapRouteDialogButton(t, d, "Yes")
	nextPatch()
	if got := mainNames(d.state.Slots[0]); got != "B,C" {
		t.Fatalf("group deletion touched the wrong exits: %s", got)
	}
}

func TestGroupedExitEditRejectsRemovedAddress(t *testing.T) {
	d, _ := groupedExitDashboard(t)
	iconButtons(d, "Rename exit")[0].Tapped(nil)
	d.state.Slots[0].RemoveMain(3)
	routeDialogEntry(t, d).SetText("Changed")
	tapRouteDialogButton(t, d, "Save")
	if d.state.Slots[0].Mains[0].Name != "A" || !strings.Contains(d.notice.Text, "changed elsewhere") {
		t.Fatal("a stale group edit should be rejected")
	}
}

func TestGroupingPreservesMixedFlags(t *testing.T) {
	d, nextPatch := groupedExitDashboard(t)
	d.state.Slots[0].Mains[3].Enabled = false
	d.state.Slots[0].Mains[3].AllowNoHop = false
	d.render()
	checks := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		check, ok := obj.(*widget.Check)
		return ok && check.Partial
	})
	if len(checks) != 2 || !routeHasText(d.content, "Mixed settings") {
		t.Fatal("the group must show differing flags without changing them")
	}
	if d.state.Slots[0].Mains[3].Enabled || d.state.Slots[0].Mains[3].AllowNoHop {
		t.Fatal("rendering a group changed address flags")
	}
	checks[0].(*widget.Check).SetChecked(true)
	nextPatch()
	if !d.state.Slots[0].Mains[0].Enabled || !d.state.Slots[0].Mains[3].Enabled {
		t.Fatal("enabling a mixed group must enable both addresses")
	}
}

func TestGroupedExitActionsIncludeDuplicateURLs(t *testing.T) {
	for _, action := range []string{"disable", "hop only", "rename", "delete"} {
		t.Run(action, func(t *testing.T) {
			d, commands := routesDashboard(t)
			for _, link := range []string{linkA, linkB, linkA, fallbackA} {
				d.state.Slots[0].AddMain(link)
			}
			d.render()
			switch action {
			case "disable":
				d.setMainFlag(0, 0, func(m *store.Main) { m.Enabled = false })
			case "hop only":
				d.setMainFlag(0, 0, func(m *store.Main) { m.AllowNoHop = false })
			case "rename":
				d.renameMain(0, 0)
				d.state.Slots[0].MoveMain(3, 0)
				routeDialogEntry(t, d).SetText("Unified exit")
				tapRouteDialogButton(t, d, "Save")
			case "delete":
				d.removeMain(0, 0)
				d.state.Slots[0].MoveMain(3, 0)
				tapRouteDialogButton(t, d, "Yes")
			}
			applyRoutePatch(t, d, commands)
			var members int
			for _, m := range d.state.Slots[0].Mains {
				if mainGroupKey(m.URL) != mainGroupKey(linkA) {
					if m.URL != linkB || !m.Enabled || m.Name != "B" {
						t.Fatal("editing the group changed another exit")
					}
					continue
				}
				members++
				if action == "disable" && m.Enabled || action == "hop only" && m.AllowNoHop || action == "rename" && m.Name != "Unified exit" {
					t.Fatalf("%s left a duplicate address unchanged", action)
				}
			}
			want := 3
			if action == "delete" {
				want = 0
			}
			if members != want {
				t.Fatalf("%s left %d group members, want %d", action, members, want)
			}
		})
	}
}

func TestGroupedExitEditRejectsChangedDuplicateCount(t *testing.T) {
	for _, add := range []bool{false, true} {
		d, _ := groupedExitDashboard(t)
		d.state.Slots[0].AddMain(linkA)
		ref, _ := d.mainRef(0, 0)
		if add {
			d.state.Slots[0].AddMain(linkA)
		} else {
			d.state.Slots[0].RemoveMain(4)
		}
		called := false
		if d.updateMain(ref, func(*store.Slot, []int) { called = true }) || called {
			t.Fatal("a changed duplicate count must invalidate the pending group edit")
		}
	}
}

func TestDisplayedPriorityMatchesFailover(t *testing.T) {
	d, _ := groupedExitDashboard(t)
	slot := &d.state.Slots[0]
	for i := range slot.Mains {
		slot.Mains[i].AllowNoHop = true
	}
	var shown []store.Main
	for _, group := range groupMains(slot.Mains) {
		shown = append(shown, group.mains...)
	}
	if !reflect.DeepEqual(slot.DirectMains(), shown) || !reflect.DeepEqual(slot.HopMains(), shown) {
		t.Fatal("the engine must try every address of a displayed group before the next group")
	}
}
