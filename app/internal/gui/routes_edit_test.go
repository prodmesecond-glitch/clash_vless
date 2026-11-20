//go:build !android && cgo

package gui

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clashvless/internal/control"
	"clashvless/internal/engine"
	"clashvless/internal/store"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/lang"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func routeTestDashboard(t *testing.T) *dashboard {
	t.Helper()
	a := test.NewApp()
	t.Cleanup(a.Quit)
	w := a.NewWindow("Routes test")
	w.Resize(fyne.NewSize(900, 640))
	d := &dashboard{app: a, window: w, page: "routes", nav: map[string]*widget.Button{}, scheduleFlush: func() {}}
	d.buildShell()
	d.state = &store.State{ListenPort: 2084, Slots: []store.Slot{
		{Name: "first", Port: 2084, Enabled: true},
		{Name: "second", Port: 2086, Enabled: true},
		{Name: "third", Port: 2088, Enabled: true},
	}}
	d.render()
	return d
}

func routesDashboard(t *testing.T) (*dashboard, <-chan control.Command) {
	t.Helper()
	d := routeTestDashboard(t)
	sock := filepath.Join(t.TempDir(), "c.sock")
	ln, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	commands := make(chan control.Command, 16)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		decoder := json.NewDecoder(conn)
		for {
			var cmd control.Command
			if decoder.Decode(&cmd) != nil {
				return
			}
			commands <- cmd
		}
	}()
	d.client, err = control.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.client.Close() })
	return d, commands
}

func routeHasText(root fyne.CanvasObject, value string) bool {
	return len(findRouteObjects(root, func(obj fyne.CanvasObject) bool {
		text, ok := obj.(*canvas.Text)
		return ok && text.Text == value
	})) > 0
}

func findRouteObjects(root fyne.CanvasObject, matches func(fyne.CanvasObject) bool) []fyne.CanvasObject {
	var found []fyne.CanvasObject
	seen := map[fyne.CanvasObject]bool{}
	var walk func(fyne.CanvasObject)
	walk = func(obj fyne.CanvasObject) {
		if obj == nil || seen[obj] {
			return
		}
		seen[obj] = true
		if matches(obj) {
			found = append(found, obj)
			return
		}
		switch obj := obj.(type) {
		case *fyne.Container:
			for _, child := range obj.Objects {
				walk(child)
			}
		case fyne.Widget:
			for _, child := range test.WidgetRenderer(obj).Objects() {
				walk(child)
			}
		}
	}
	walk(root)
	return found
}

func routeDialogEntry(t *testing.T, d *dashboard) *widget.Entry {
	t.Helper()
	entries := findRouteObjects(d.window.Canvas().Overlays().Top(), func(obj fyne.CanvasObject) bool {
		_, ok := obj.(*widget.Entry)
		return ok
	})
	if len(entries) != 1 {
		t.Fatalf("dialog entries = %d", len(entries))
	}
	return entries[0].(*widget.Entry)
}

func tapRouteDialogButton(t *testing.T, d *dashboard, label string) {
	t.Helper()
	if label == "Yes" || label == "No" {
		label = lang.L(label)
	}
	buttons := findRouteObjects(d.window.Canvas().Overlays().Top(), func(obj fyne.CanvasObject) bool {
		button, ok := obj.(*widget.Button)
		return ok && button.Text == label
	})
	if len(buttons) != 1 {
		t.Fatalf("dialog %s buttons = %d", label, len(buttons))
	}
	test.Tap(buttons[0].(*widget.Button))
}

func applyRoutePatch(t *testing.T, d *dashboard, commands <-chan control.Command) {
	t.Helper()
	select {
	case cmd := <-commands:
		if cmd.Cmd != "patch" {
			t.Fatalf("unexpected command: %s", cmd.Cmd)
		}
		if err := json.Unmarshal(cmd.Patch, d.state); err != nil {
			t.Fatal(err)
		}
		d.render()
	case <-time.After(2 * time.Second):
		t.Fatal("no patch received")
	}
}

func TestAddExitToLastThenFirstSlot(t *testing.T) {
	d, commands := routesDashboard(t)
	for _, index := range []int{2, 0, 2, 0} {
		d.addMain(index)
		entry := routeDialogEntry(t, d)
		d.window.Canvas().Focus(entry)
		test.Type(entry, "vless://11111111-1111-1111-1111-111111111111@example.com:443?security=none#Exit")
		content := d.content.Objects[0]
		for update := 0; update < 3; update++ {
			d.render()
		}
		if d.content.Objects[0] != content {
			t.Fatal("status refresh replaced the page beneath the edit dialog")
		}
		tapRouteDialogButton(t, d, "Add")
		if d.dialogOpen || d.window.Canvas().Overlays().Top() != nil {
			t.Fatal("edit dialog was not released after saving")
		}
		applyRoutePatch(t, d, commands)
	}
	if len(d.state.Slots[0].Mains) == 0 || len(d.state.Slots[2].Mains) == 0 {
		t.Fatal("exits were not saved to both slots")
	}
}

func TestAddExitKeepsSlotIdentityDuringUpdates(t *testing.T) {
	d, commands := routesDashboard(t)
	d.addMain(2)
	routeDialogEntry(t, d).SetText("vless://uuid@example.com:443#Exit")
	d.state.Slots[0], d.state.Slots[2] = d.state.Slots[2], d.state.Slots[0]
	d.render()
	tapRouteDialogButton(t, d, "Add")
	applyRoutePatch(t, d, commands)
	if len(d.state.Slots[0].Mains) != 1 || len(d.state.Slots[2].Mains) != 0 {
		t.Fatal("exit was saved to a different slot after the state update")
	}
}

func TestAddExitToRemovedSlot(t *testing.T) {
	d, commands := routesDashboard(t)
	d.addMain(2)
	routeDialogEntry(t, d).SetText("vless://uuid@example.com:443#Exit")
	d.state.Slots = d.state.Slots[:2]
	d.render()
	tapRouteDialogButton(t, d, "Add")
	if d.dialogOpen || d.notice.Text != "This slot was removed while editing" {
		t.Fatal("removed slot was not reported safely")
	}
	select {
	case <-commands:
		t.Fatal("patch sent for a removed slot")
	default:
	}
	d.addMain(2)
	if d.window.Canvas().Overlays().Top() != nil {
		t.Fatal("dialog opened for a stale slot")
	}
}

func TestCancelExitDialogRefreshesLatestState(t *testing.T) {
	d, _ := routesDashboard(t)
	d.addMain(0)
	content := d.content.Objects[0]
	d.state.Slots[0].Enabled = false
	d.render()
	d.addMain(2)
	if len(d.window.Canvas().Overlays().List()) != 1 {
		t.Fatal("a second edit dialog opened over the first")
	}
	tapRouteDialogButton(t, d, "Cancel")
	if d.dialogOpen || d.content.Objects[0] == content {
		t.Fatal("latest state was not rendered after canceling")
	}
	d.addMain(2)
	if d.window.Canvas().Overlays().Top() == nil {
		t.Fatal("next slot dialog could not open")
	}
	tapRouteDialogButton(t, d, "Cancel")
}

func TestRoutesKeepWindowWidthWithLongDetails(t *testing.T) {
	longName := strings.Repeat("vless://uuid@example.com:443?security=reality#Выход ", 12)
	longHost := strings.Repeat("segment.", 25) + "example.com"
	for _, tc := range []struct {
		name, slot, exit, host string
	}{
		{"exit name", "first", longName, "example.com"},
		{"server address", "first", "Example exit", longHost},
		{"slot name", longName, "Example exit", "example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := routeTestDashboard(t)
			d.state.Slots[0].Name = tc.slot
			d.state.Slots[0].Mains = []store.Main{{Name: tc.exit, URL: "vless://uuid@" + tc.host + ":443?security=reality", Enabled: true}}
			for _, connected := range []bool{false, true, false, true} {
				d.status = engine.Status{}
				if connected {
					d.status = engine.Status{Tier: 1, TunUp: true, Slots: []engine.SlotStatus{{Name: tc.slot, Tier: 1, Main: tc.exit, Egress: 30 * time.Millisecond}}}
				}
				d.render()
				if width := d.window.Content().MinSize().Width; width > 760 {
					t.Fatalf("connected=%t: route text forces window width %.0f", connected, width)
				}
				d.window.Resize(fyne.NewSize(760, 640))
				if width := d.window.Canvas().Size().Width; width > 760 {
					t.Fatalf("connected=%t: cannot shrink window below %.0f", connected, width)
				}
			}
		})
	}
}

func TestSlotDisplaysSelectedForVPN(t *testing.T) {
	d, commands := routesDashboard(t)
	d.render()

	if !routeHasText(d.content, "Port 2084") {
		t.Fatal("expected the open slot to show its port 2084")
	}

	selectedBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Selected for VPN"
	})
	if len(selectedBtns) != 1 {
		t.Fatalf("expected 1 'Selected for VPN' button, got %d", len(selectedBtns))
	}
	if !selectedBtns[0].(*widget.Button).Disabled() {
		t.Fatal("expected 'Selected for VPN' button to be disabled")
	}

	// In the tabbed view, the first slot is selected. To see "Use for VPN" for the second slot,
	// select the second slot's tab.
	tabs := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && strings.Contains(b.Text, "second")
	})
	if len(tabs) != 1 {
		t.Fatalf("expected 1 tab for 'second', got %d", len(tabs))
	}
	test.Tap(tabs[0].(*widget.Button))

	useBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Use for VPN"
	})
	if len(useBtns) != 1 {
		t.Fatalf("expected 1 'Use for VPN' button on selected slot, got %d", len(useBtns))
	}

	test.Tap(useBtns[0].(*widget.Button))
	applyRoutePatch(t, d, commands)

	if d.state.TunSlotName() != "second" {
		t.Fatalf("expected tun_slot to be 'second', got %q", d.state.TunSlotName())
	}

	selectedAfter := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Selected for VPN"
	})
	if len(selectedAfter) != 1 || !routeHasText(d.content, "Port 2086") {
		t.Fatalf("expected 'second' slot (Port 2086) to be selected for VPN, got %d buttons", len(selectedAfter))
	}

	d.navigate("overview")
	var vpnRows []string
	findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		row, ok := obj.(*fyne.Container)
		if !ok {
			return false
		}
		if _, ok := row.Layout.(*titleTagsLayout); ok && routeHasText(row, "VPN") {
			vpnRows = append(vpnRows, row.Objects[0].(*fitText).text)
		}
		return false
	})
	if len(vpnRows) != 1 || vpnRows[0] != "second" {
		t.Fatalf("expected overview to mark only 'second' as the VPN endpoint, got %v", vpnRows)
	}

	d.settingsSection = "VPN"
	d.navigate("settings")
	selects := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		s, ok := obj.(*widget.Select)
		return ok && s.Selected == "second"
	})
	if len(selects) != 1 {
		t.Fatalf("expected 1 select widget with 'second' selected in Settings -> VPN, got %d", len(selects))
	}
}

func TestExitRoutesTabsBar(t *testing.T) {
	d := routeTestDashboard(t)
	d.render()

	// 1. Initial state: slot 0 ("first") is selected by default, and it's the default VPN slot.
	if d.selectedSlot != "first" {
		t.Fatalf("expected initial selectedSlot to be 'first', got %q", d.selectedSlot)
	}

	// Verify tab buttons exist for all 3 slots
	tabButtons := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		if !ok {
			return false
		}
		return strings.Contains(b.Text, "first") || strings.Contains(b.Text, "second") || strings.Contains(b.Text, "third")
	})
	if len(tabButtons) != 3 {
		t.Fatalf("expected 3 slot tab buttons, got %d", len(tabButtons))
	}

	// Find the tabs individually
	var tabFirst, tabSecond, tabThird *widget.Button
	for _, obj := range tabButtons {
		b := obj.(*widget.Button)
		if strings.Contains(b.Text, "first") {
			tabFirst = b
		} else if strings.Contains(b.Text, "second") {
			tabSecond = b
		} else if strings.Contains(b.Text, "third") {
			tabThird = b
		}
	}

	if tabFirst == nil || tabSecond == nil || tabThird == nil {
		t.Fatalf("missing one of the tabs: first=%v, second=%v, third=%v", tabFirst, tabSecond, tabThird)
	}

	// Tab "first" is selected (HighImportance) and is VPN slot (icon != nil, contains "VPN")
	if tabFirst.Importance != widget.HighImportance {
		t.Fatalf("expected selected tab 'first' to have HighImportance, got %v", tabFirst.Importance)
	}
	if tabFirst.Icon == nil {
		t.Fatal("expected VPN slot tab 'first' to display an icon")
	}
	if !strings.Contains(tabFirst.Text, "VPN") {
		t.Fatalf("expected tab 'first' text to show VPN, got %q", tabFirst.Text)
	}

	// Tabs "second" and "third" are not selected and not VPN (LowImportance, no icon)
	if tabSecond.Importance != widget.LowImportance {
		t.Fatalf("expected unselected tab 'second' to have LowImportance, got %v", tabSecond.Importance)
	}
	if tabSecond.Icon != nil {
		t.Fatal("expected non-VPN tab 'second' to have no icon")
	}

	// Unselected tabs not used for VPN ("second" and "third") have a rounded rectangle behind them
	tabRects := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		r, ok := obj.(*canvas.Rectangle)
		return ok && r.CornerRadius == 4 && r.FillColor == colorSurface
	})
	if len(tabRects) != 2 {
		t.Fatalf("expected 2 rounded rectangles behind unselected non-VPN tabs, got %d", len(tabRects))
	}

	// Verify only slot "first" panel is displayed
	labels := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		text, ok := obj.(*canvas.Text)
		return ok && text.Text == "Port 2084"
	})
	if len(labels) != 1 {
		t.Fatalf("expected 1 port chip with 'Port 2084', got %d", len(labels))
	}
	labelsSecond := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		text, ok := obj.(*canvas.Text)
		return ok && text.Text == "Port 2086"
	})
	if len(labelsSecond) != 0 {
		t.Fatalf("expected 0 port chips with 'Port 2086' while 'first' is selected, got %d", len(labelsSecond))
	}

	// 2. Switch tab to "second"
	test.Tap(tabSecond)

	if d.selectedSlot != "second" {
		t.Fatalf("expected selectedSlot to be 'second', got %q", d.selectedSlot)
	}

	// Re-query tabs after render
	tabFirst = nil
	tabSecond = nil
	for _, obj := range findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && (strings.Contains(b.Text, "first") || strings.Contains(b.Text, "second"))
	}) {
		b := obj.(*widget.Button)
		if strings.Contains(b.Text, "first") {
			tabFirst = b
		} else if strings.Contains(b.Text, "second") {
			tabSecond = b
		}
	}

	// "second" is now active (HighImportance)
	if tabSecond.Importance != widget.HighImportance {
		t.Fatalf("expected active tab 'second' to have HighImportance, got %v", tabSecond.Importance)
	}
	// "first" is inactive but VPN slot -> highlighted with MediumImportance + icon + text "VPN"
	if tabFirst.Importance != widget.MediumImportance {
		t.Fatalf("expected inactive VPN tab 'first' to have MediumImportance, got %v", tabFirst.Importance)
	}
	if tabFirst.Icon == nil || !strings.Contains(tabFirst.Text, "VPN") {
		t.Fatalf("expected inactive VPN tab 'first' to keep VPN indicator, got icon=%v text=%q", tabFirst.Icon, tabFirst.Text)
	}

	// Verify slot "second" panel is now displayed
	labelsSecond = findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		text, ok := obj.(*canvas.Text)
		return ok && text.Text == "Port 2086"
	})
	if len(labelsSecond) != 1 {
		t.Fatalf("expected 1 port chip with 'Port 2086' after selecting 'second', got %d", len(labelsSecond))
	}

	tabRects = findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		r, ok := obj.(*canvas.Rectangle)
		return ok && r.CornerRadius == 4 && r.FillColor == colorSurface
	})
	if len(tabRects) != 1 {
		t.Fatalf("expected 1 rounded rectangle behind unselected non-VPN tab 'third', got %d", len(tabRects))
	}

	// 3. Status updates keep the selected tab
	d.status = engine.Status{Tier: 1}
	d.render()
	if d.selectedSlot != "second" {
		t.Fatalf("status update reset selectedSlot from 'second' to %q", d.selectedSlot)
	}

	// 4. VPN slot changed to "third"
	d.state.TunSlot = "third"
	d.render()

	tabThirdObj := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && strings.Contains(b.Text, "third")
	})
	if len(tabThirdObj) != 1 {
		t.Fatalf("expected 1 tab for 'third', got %d", len(tabThirdObj))
	}
	tabThird = tabThirdObj[0].(*widget.Button)
	if tabThird.Icon == nil || !strings.Contains(tabThird.Text, "VPN") {
		t.Fatalf("expected tab 'third' to become VPN tab, got icon=%v text=%q", tabThird.Icon, tabThird.Text)
	}
}

func TestTabLabelFormatting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isTun    bool
		expected string
	}{
		{"", false, "Slot"},
		{"", true, "VPN"},
		{"first", false, "first"},
		{"first", true, "first · VPN"},
		{"slot-1", false, "slot-1"},
		{"slot-1", true, "slot-1 · VPN"},
		{"   spaced   name   ", false, "spaced name"},
		{"a_very_long_slot_name_here", false, "a_very_long_slot_…"},
		{"a_very_long_slot_name_here", true, "a_very_long_s… · VPN"},
		{"русское_имя_слота_длинное", false, "русское_имя_слота…"},
	} {
		got := tabLabel(tc.name, tc.isTun)
		if got != tc.expected {
			t.Errorf("tabLabel(%q, %t) = %q, want %q", tc.name, tc.isTun, got, tc.expected)
		}
	}
}

func TestAddSlotOpensNewSlot(t *testing.T) {
	d, commands := routesDashboard(t)
	d.render()

	// Initial slot is "first"
	if d.selectedSlot != "first" {
		t.Fatalf("expected initial selectedSlot to be 'first', got %q", d.selectedSlot)
	}

	// Click "Add slot" button
	addBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Add slot"
	})
	if len(addBtns) != 1 {
		t.Fatalf("expected 1 'Add slot' button, got %d", len(addBtns))
	}
	test.Tap(addBtns[0].(*widget.Button))

	// Enter new slot name "fourth"
	entry := routeDialogEntry(t, d)
	entry.SetText("fourth")
	tapRouteDialogButton(t, d, "Add")

	// Dialog has closed and d.render() ran.
	// The new slot "fourth" must be selected and opened immediately, not dropped to the first tab.
	if d.selectedSlot != "fourth" {
		t.Fatalf("expected selectedSlot to be 'fourth' immediately after add, got %q", d.selectedSlot)
	}

	// Verify "fourth" tab is active (HighImportance)
	fourthTabs := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && strings.Contains(b.Text, "fourth")
	})
	if len(fourthTabs) != 1 {
		t.Fatalf("expected 1 tab for 'fourth', got %d", len(fourthTabs))
	}
	if fourthTabs[0].(*widget.Button).Importance != widget.HighImportance {
		t.Fatalf("expected 'fourth' tab to have HighImportance, got %v", fourthTabs[0].(*widget.Button).Importance)
	}

	// Verify the slot panel displayed is for "fourth" (not "first")
	fourthLabels := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		l, ok := obj.(*widget.Label)
		return ok && strings.Contains(l.Text, "fourth")
	})
	if len(fourthLabels) == 0 {
		t.Fatal("expected 'fourth' slot panel to be opened")
	}

	// Daemon processes patch and sends updated state
	applyRoutePatch(t, d, commands)

	// After state sync, "fourth" remains opened
	if d.selectedSlot != "fourth" {
		t.Fatalf("expected selectedSlot to remain 'fourth' after state patch, got %q", d.selectedSlot)
	}
	fourthTabs = findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && strings.Contains(b.Text, "fourth")
	})
	if len(fourthTabs) != 1 {
		t.Fatalf("expected 1 tab for 'fourth' after patch, got %d", len(fourthTabs))
	}
	if fourthTabs[0].(*widget.Button).Importance != widget.HighImportance {
		t.Fatalf("expected 'fourth' tab to still have HighImportance after patch, got %v", fourthTabs[0].(*widget.Button).Importance)
	}
}

func TestRenameSlot(t *testing.T) {
	d, commands := routesDashboard(t)
	d.selectedSlot = "second"
	d.render()

	// Trigger rename on slot index 1 ("second")
	d.renameSlot(1)
	entry := routeDialogEntry(t, d)
	if entry.Text != "second" {
		t.Fatalf("expected prefilled text 'second', got %q", entry.Text)
	}

	// Submit new name "renamed-second"
	entry.SetText("renamed-second")
	tapRouteDialogButton(t, d, "Save")

	if d.selectedSlot != "renamed-second" {
		t.Fatalf("expected selectedSlot to update to 'renamed-second', got %q", d.selectedSlot)
	}

	applyRoutePatch(t, d, commands)

	if d.state.Slots[1].Name != "renamed-second" {
		t.Fatalf("expected slot 1 to be named 'renamed-second', got %q", d.state.Slots[1].Name)
	}

	// Verify tab label updated
	tabs := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && strings.Contains(b.Text, "renamed-second")
	})
	if len(tabs) != 1 {
		t.Fatalf("expected 1 tab for 'renamed-second', got %d", len(tabs))
	}
}

func TestRenameTunSlotUpdatesTunSlot(t *testing.T) {
	d, commands := routesDashboard(t)
	d.state.TunSlot = "first"
	d.selectedSlot = "first"
	d.render()

	d.renameSlot(0)
	entry := routeDialogEntry(t, d)
	entry.SetText("primary")
	tapRouteDialogButton(t, d, "Save")

	if d.state.TunSlot != "primary" {
		t.Fatalf("expected d.state.TunSlot to update to 'primary', got %q", d.state.TunSlot)
	}

	applyRoutePatch(t, d, commands)

	if d.state.TunSlot != "primary" {
		t.Fatalf("expected TunSlot to remain 'primary' after patch, got %q", d.state.TunSlot)
	}
	if d.state.Slots[0].Name != "primary" {
		t.Fatalf("expected slot 0 name to be 'primary', got %q", d.state.Slots[0].Name)
	}
}

func TestRenameSlotValidation(t *testing.T) {
	d, commands := routesDashboard(t)

	// Duplicate name
	d.renameSlot(0)
	entry := routeDialogEntry(t, d)
	entry.SetText("second")
	tapRouteDialogButton(t, d, "Save")
	if d.notice.Text != "Slot name already exists" {
		t.Fatalf("expected duplicate notification, got %q", d.notice.Text)
	}
	select {
	case <-commands:
		t.Fatal("patch sent for duplicate slot name")
	default:
	}

	// Empty name
	d.renameSlot(0)
	entry = routeDialogEntry(t, d)
	entry.SetText("   ")
	tapRouteDialogButton(t, d, "Save")
	if d.notice.Text != "Enter a valid slot name" {
		t.Fatalf("expected empty name notification, got %q", d.notice.Text)
	}
	select {
	case <-commands:
		t.Fatal("patch sent for empty slot name")
	default:
	}

	// URL-like name containing ://
	d.renameSlot(0)
	entry = routeDialogEntry(t, d)
	entry.SetText("vless://invalid")
	tapRouteDialogButton(t, d, "Save")
	if d.notice.Text != "Slot name cannot contain ://" {
		t.Fatalf("expected URL name notification, got %q", d.notice.Text)
	}
	select {
	case <-commands:
		t.Fatal("patch sent for URL slot name")
	default:
	}
}

func TestRenameIconButtonAndTooltip(t *testing.T) {
	d, _ := routesDashboard(t)
	d.selectedSlot = "second"
	d.render()

	// Verify no standalone "Rename slot" button exists
	standaloneBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Rename slot"
	})
	if len(standaloneBtns) > 0 {
		t.Fatalf("expected no standalone 'Rename slot' button, found %d", len(standaloneBtns))
	}

	// Verify tooltip icon button exists
	renameBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		btn, ok := obj.(*tooltipIconButton)
		return ok && btn.Hint == "Rename slot"
	})
	if len(renameBtns) != 1 {
		t.Fatalf("expected 1 rename tooltipIconButton, got %d", len(renameBtns))
	}
	iconBtn := renameBtns[0].(*tooltipIconButton)

	// The hint floats in the shared tooltip layer, hidden until hovered.
	if d.tips.pill.Visible() {
		t.Fatal("expected tooltip to be initially hidden")
	}
	iconBtn.MouseIn(nil)
	if !d.tips.pill.Visible() || d.tips.text.Text != "Rename slot" {
		t.Fatalf("expected %q tooltip on MouseIn, got visible=%v text=%q", "Rename slot", d.tips.pill.Visible(), d.tips.text.Text)
	}
	iconBtn.MouseOut()
	if d.tips.pill.Visible() {
		t.Fatal("expected tooltip to be hidden on MouseOut")
	}

	// Test tap on icon button triggers rename dialog
	iconBtn.Tapped(nil)
	entry := routeDialogEntry(t, d)
	if entry == nil || entry.Text != "second" {
		t.Fatalf("expected rename dialog with prefilled text 'second', got %v", entry)
	}
}

func TestSlotDeleteIconButtonAndTooltip(t *testing.T) {
	d, commands := routesDashboard(t)
	d.selectedSlot = "second"
	d.render()

	// Verify no standalone "Delete slot" text button exists
	standaloneBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Delete slot"
	})
	if len(standaloneBtns) > 0 {
		t.Fatalf("expected no standalone 'Delete slot' button, found %d", len(standaloneBtns))
	}

	// Verify delete tooltip icon button exists
	delBtns := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		btn, ok := obj.(*tooltipIconButton)
		return ok && btn.Hint == "Delete slot"
	})
	if len(delBtns) != 1 {
		t.Fatalf("expected 1 delete tooltipIconButton, got %d", len(delBtns))
	}
	iconBtn := delBtns[0].(*tooltipIconButton)

	// The hint floats in the shared tooltip layer, hidden until hovered.
	if d.tips.pill.Visible() {
		t.Fatal("expected tooltip to be initially hidden")
	}
	iconBtn.MouseIn(nil)
	if !d.tips.pill.Visible() || d.tips.text.Text != "Delete slot" {
		t.Fatalf("expected %q tooltip on MouseIn, got visible=%v text=%q", "Delete slot", d.tips.pill.Visible(), d.tips.text.Text)
	}
	iconBtn.MouseOut()
	if d.tips.pill.Visible() {
		t.Fatal("expected tooltip to be hidden on MouseOut")
	}

	// Test tap on delete icon button triggers confirm dialog
	iconBtn.Tapped(nil)
	confirmBtns := findRouteObjects(d.window.Canvas().Overlays().Top(), func(obj fyne.CanvasObject) bool {
		btn, ok := obj.(*widget.Button)
		return ok && (btn.Importance == widget.HighImportance || btn.Text == "Yes" || btn.Icon == theme.ConfirmIcon())
	})
	if len(confirmBtns) != 1 {
		t.Fatalf("expected 1 confirm button, got %d", len(confirmBtns))
	}
	test.Tap(confirmBtns[0].(*widget.Button))

	// Verify slot deletion command sent
	applyRoutePatch(t, d, commands)
	if len(d.state.Slots) != 2 {
		t.Fatalf("expected 2 slots after delete, got %d", len(d.state.Slots))
	}
	if d.state.SlotIndexByName("second") >= 0 {
		t.Fatal("expected 'second' slot to be deleted")
	}
}

func TestSlotEnableSlider(t *testing.T) {
	d, commands := routesDashboard(t)
	d.selectedSlot = "second"
	d.render()

	// Find the compact switch for slot enable
	switches := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		sw, ok := obj.(*connectionSwitch)
		return ok && sw.Compact
	})
	if len(switches) != 1 {
		t.Fatalf("expected 1 compact connectionSwitch, got %d", len(switches))
	}
	slotSwitch := switches[0].(*connectionSwitch)
	if !slotSwitch.Checked {
		t.Fatal("expected slot switch to be checked initially")
	}

	// Find the Enabled text
	statusTexts := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		txt, ok := obj.(*canvas.Text)
		return ok && txt.Text == "Enabled"
	})
	if len(statusTexts) != 1 {
		t.Fatalf("expected 1 'Enabled' text, got %d", len(statusTexts))
	}

	// Tap switch to disable slot
	test.Tap(slotSwitch)
	if slotSwitch.Checked {
		t.Fatal("expected slot switch to be unchecked after tap")
	}

	// Check text changed to "Disabled"
	disabledTexts := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		txt, ok := obj.(*canvas.Text)
		return ok && txt.Text == "Disabled"
	})
	if len(disabledTexts) != 1 {
		t.Fatalf("expected 1 'Disabled' text, got %d", len(disabledTexts))
	}

	applyRoutePatch(t, d, commands)
	if d.state.Slots[1].Enabled {
		t.Fatal("expected slot 1 to be disabled in state")
	}
}
func TestSlotTitleDisplaysFullName(t *testing.T) {
	d, _ := routesDashboard(t)
	d.selectedSlot = "second"
	d.render()

	// Verify slot title label exists and contains full name "second"
	labels := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		l, ok := obj.(*widget.Label)
		return ok && l.Text == "second"
	})
	if len(labels) != 1 {
		t.Fatalf("expected 1 label with text 'second', got %d", len(labels))
	}
	titleLabel := labels[0].(*widget.Label)
	if titleLabel.Size().Width < 40 {
		t.Fatalf("expected title label width >= 40px, got %.2f (would render as '...')", titleLabel.Size().Width)
	}
}

func TestAddProxySlotAndExitsCopyPasteAndManual(t *testing.T) {
	d, commands := routesDashboard(t)
	d.render()

	// Add an HTTP/SOCKS5 slot.
	d.addSlot()
	entry := routeDialogEntry(t, d)
	entry.SetText("res-proxy")
	selects := findRouteObjects(d.window.Canvas().Overlays().Top(), func(obj fyne.CanvasObject) bool {
		_, ok := obj.(*widget.Select)
		return ok
	})
	if len(selects) != 1 {
		t.Fatalf("expected 1 slot type select, got %d", len(selects))
	}
	selects[0].(*widget.Select).SetSelected("HTTP / SOCKS5 proxy")
	tapRouteDialogButton(t, d, "Add")
	applyRoutePatch(t, d, commands)

	idx := d.state.SlotIndexByName("res-proxy")
	if idx < 0 || !d.state.Slots[idx].IsProxy() {
		t.Fatalf("proxy slot not created: %+v", d.state.Slots)
	}
	if !routeHasText(d.content, "HTTP/SOCKS") {
		t.Fatal("expected HTTP/SOCKS chip on proxy slot")
	}

	// Add via Copy-paste tab.
	d.addMain(idx)
	entries := findRouteObjects(d.window.Canvas().Overlays().Top(), func(obj fyne.CanvasObject) bool {
		e, ok := obj.(*widget.Entry)
		return ok && e.MultiLine
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 multiline paste entry, got %d", len(entries))
	}
	entries[0].(*widget.Entry).SetText("1.2.3.4:1080:alice:secret\nhttp://bob:pw@5.6.7.8:8080#Web")
	tapRouteDialogButton(t, d, "Add")
	applyRoutePatch(t, d, commands)

	if got := len(d.state.Slots[idx].Mains); got != 2 {
		t.Fatalf("expected 2 pasted proxy exits, got %d", got)
	}
	if d.state.Slots[idx].Mains[0].URL != "socks5://alice:secret@1.2.3.4:1080" || d.state.Slots[idx].Mains[1].URL != "http://bob:pw@5.6.7.8:8080#Web" {
		t.Fatalf("unexpected pasted exits: %+v", d.state.Slots[idx].Mains)
	}

	// Add via Manual tab.
	d.addMain(idx)
	tabContainers := findRouteObjects(d.window.Canvas().Overlays().Top(), func(obj fyne.CanvasObject) bool {
		_, ok := obj.(*container.AppTabs)
		return ok
	})
	if len(tabContainers) != 1 {
		t.Fatalf("expected 1 AppTabs in proxy exit dialog, got %d", len(tabContainers))
	}
	tabs := tabContainers[0].(*container.AppTabs)
	tabs.SelectIndex(1)
	manualEntries := findRouteObjects(tabs.Selected().Content, func(obj fyne.CanvasObject) bool {
		_, ok := obj.(*widget.Entry)
		return ok
	})
	if len(manualEntries) != 5 {
		t.Fatalf("expected 5 manual entries (host, port, user, pass, name), got %d", len(manualEntries))
	}
	manualSelects := findRouteObjects(tabs.Selected().Content, func(obj fyne.CanvasObject) bool {
		_, ok := obj.(*widget.Select)
		return ok
	})
	manualSelects[0].(*widget.Select).SetSelected("HTTP")
	manualEntries[0].(*widget.Entry).SetText("proxy.example")
	manualEntries[1].(*widget.Entry).SetText("3128")
	manualEntries[2].(*widget.Entry).SetText("carol")
	manualEntries[3].(*widget.Entry).SetText("pass123")
	manualEntries[4].(*widget.Entry).SetText("Office")
	tapRouteDialogButton(t, d, "Add")
	applyRoutePatch(t, d, commands)

	if got := len(d.state.Slots[idx].Mains); got != 3 {
		t.Fatalf("expected 3 proxy exits after manual add, got %d", got)
	}
	last := d.state.Slots[idx].Mains[2]
	if last.Name != "Office" || last.URL != "http://carol:pass123@proxy.example:3128#Office" || !last.AllowNoHop {
		t.Fatalf("unexpected manual exit: %+v", last)
	}
	if !routeHasText(d.content, "socks5 · auth") || !routeHasText(d.content, "http · auth") {
		t.Fatal("expected socks5 · auth and http · auth chips on proxy exits")
	}
}
