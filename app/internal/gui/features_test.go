//go:build !android && cgo

package gui

import (
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"clashvless/internal/engine"
	"clashvless/internal/loghub"
	"clashvless/internal/share"
	"clashvless/internal/store"
)

const (
	linkA = "vless://11111111-1111-1111-1111-111111111111@a.example:443?security=reality&sni=a&pbk=k&type=tcp#A"
	linkB = "vless://22222222-2222-2222-2222-222222222222@b.example:443?security=none&type=grpc&serviceName=s#B"
	linkC = "vless://33333333-3333-3333-3333-333333333333@c.example:443?security=tls&sni=c&type=ws&path=%2F#C"
)

func iconButtons(d *dashboard, hint string) []*tooltipIconButton {
	var out []*tooltipIconButton
	for _, obj := range findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*tooltipIconButton)
		return ok && b.Hint == hint
	}) {
		out = append(out, obj.(*tooltipIconButton))
	}
	return out
}

func mainNames(slot store.Slot) string {
	names := make([]string, len(slot.Mains))
	for i, m := range slot.Mains {
		names[i] = m.Name
	}
	return strings.Join(names, ",")
}

func threeExitDashboard(t *testing.T) (*dashboard, func()) {
	d, commands := routesDashboard(t)
	slot := &d.state.Slots[0]
	for _, link := range []string{linkA, linkB, linkC} {
		slot.AddMain(link)
	}
	d.selectedSlot = "first"
	d.render()
	return d, func() { applyRoutePatch(t, d, commands) }
}

func TestMoveExitChangesPriority(t *testing.T) {
	d, nextPatch := threeExitDashboard(t)
	up, down := iconButtons(d, "Raise priority"), iconButtons(d, "Lower priority")
	if len(up) != 3 || len(down) != 3 {
		t.Fatalf("move buttons: %d up, %d down", len(up), len(down))
	}
	if !up[0].Disabled || up[1].Disabled || !down[2].Disabled || down[0].Disabled {
		t.Fatal("only the first exit's raise and the last exit's lower should be disabled")
	}
	down[0].Tapped(nil)
	nextPatch()
	if got := mainNames(d.state.Slots[0]); got != "B,A,C" {
		t.Fatalf("after lowering A: %s", got)
	}
	iconButtons(d, "Raise priority")[2].Tapped(nil)
	nextPatch()
	if got := mainNames(d.state.Slots[0]); got != "B,C,A" {
		t.Fatalf("after raising C: %s", got)
	}
	iconButtons(d, "Raise priority")[0].Tapped(nil) // disabled: no patch
	if got := mainNames(d.state.Slots[0]); got != "B,C,A" {
		t.Fatalf("disabled button changed order: %s", got)
	}
}

func TestDeleteExitFollowsItAcrossConcurrentReorder(t *testing.T) {
	d, nextPatch := threeExitDashboard(t)
	iconButtons(d, "Delete exit")[1].Tapped(nil) // B
	// Another client moves B to the top while the confirmation is open.
	d.state.Slots[0].MoveMain(1, 0)
	tapRouteDialogButton(t, d, "Yes")
	nextPatch()
	if got := mainNames(d.state.Slots[0]); got != "A,C" {
		t.Fatalf("after deleting B: %s", got)
	}
}

func TestRenameExitKeepsURL(t *testing.T) {
	d, nextPatch := threeExitDashboard(t)
	iconButtons(d, "Rename exit")[2].Tapped(nil)
	entry := routeDialogEntry(t, d)
	if entry.Text != "C" {
		t.Fatalf("rename prefilled %q", entry.Text)
	}
	entry.SetText("  Cloud   exit ")
	tapRouteDialogButton(t, d, "Save")
	nextPatch()
	main := d.state.Slots[0].Mains[2]
	if main.Name != "Cloud exit" || main.URL != linkC {
		t.Fatalf("renamed main = %+v", main)
	}
	if got := share.MainLink(main).URL; !strings.HasSuffix(got, "#Cloud%20exit") || !strings.HasPrefix(got, strings.TrimSuffix(linkC, "#C")) {
		t.Fatalf("share link = %s", got)
	}
}

func TestAddSeveralExitsAtOnce(t *testing.T) {
	d, nextPatch := threeExitDashboard(t)
	d.addMain(1)
	entry := routeDialogEntry(t, d)
	entry.SetText(linkA + "\n\n  " + linkB + "  \nnot a link\n" + linkA)
	tapRouteDialogButton(t, d, "Add")
	nextPatch()
	if got := mainNames(d.state.Slots[1]); got != "A,B" {
		t.Fatalf("second slot exits = %s", got)
	}
	if !strings.Contains(d.notice.Text, "Added 2") || !strings.Contains(d.notice.Text, "1 already present") || !strings.Contains(d.notice.Text, "1 invalid") {
		t.Fatalf("notice = %q", d.notice.Text)
	}
}

func TestExportDialogCopiesLinks(t *testing.T) {
	d, _ := threeExitDashboard(t)
	exports := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Text == "Export"
	})
	if len(exports) != 1 {
		t.Fatalf("export buttons = %d", len(exports))
	}
	test.Tap(exports[0].(*widget.Button))
	if !d.dialogOpen {
		t.Fatal("export dialog did not open")
	}
	tapRouteDialogButton(t, d, "Copy all links")
	if got := d.app.Clipboard().Content(); got != strings.Join([]string{linkA, linkB, linkC}, "\n") {
		t.Fatalf("clipboard = %q", got)
	}
	tapRouteDialogButton(t, d, "Copy as subscription")
	decoded, err := base64.StdEncoding.DecodeString(d.app.Clipboard().Content())
	if err != nil || string(decoded) != strings.Join([]string{linkA, linkB, linkC}, "\n") {
		t.Fatalf("subscription blob decodes to %q (%v)", decoded, err)
	}
	tapRouteDialogButton(t, d, "Copy link")
	if got := d.app.Clipboard().Content(); got != linkA {
		t.Fatalf("first link = %q", got)
	}
	tapRouteDialogButton(t, d, "Close")
	if d.dialogOpen {
		t.Fatal("dialog still open")
	}
}

func TestQRCodeScansBackToLink(t *testing.T) {
	zbar, err := exec.LookPath("zbarimg")
	if err != nil {
		t.Skip("zbarimg not installed")
	}
	long := linkC + "&extra=" + strings.Repeat("x", 300)
	for _, link := range []string{linkA, long} {
		img, err := qrImage(link)
		if err != nil {
			t.Fatal(err)
		}
		// Scale up so the scanner sees clean modules.
		path := filepath.Join(t.TempDir(), "qr.png")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, scaled(img, 6)); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out, err := exec.Command(zbar, "--raw", "-q", path).Output()
		if err != nil {
			t.Fatalf("zbarimg: %v", err)
		}
		if got := strings.TrimRight(string(out), "\n"); got != link {
			t.Fatalf("scanned %q, want %q", got, link)
		}
	}
}

func TestSettingsDraftSurvivesRebuild(t *testing.T) {
	d := routeTestDashboard(t)
	d.drafts = map[string]string{}
	d.state.Interval, d.state.Timeout = 12, 6
	d.navigate("settings")
	entry := d.inputs["setting:interval_s"]
	if entry == nil {
		t.Fatal("interval field not registered")
	}
	test.Type(entry, "3") // typed at the start: "312"
	d.render()            // e.g. a state event from another client
	if got := d.inputs["setting:interval_s"].Text; got != "312" {
		t.Fatalf("unsaved interval after rebuild = %q", got)
	}
}

func TestStatusEventsOnlyRedrawWhatThePageShows(t *testing.T) {
	d := routeTestDashboard(t)
	d.navigate("routes")
	rendered := d.lastRender
	time.Sleep(5 * time.Millisecond)
	d.status.Country = []engine.Probe{{Name: "n", OK: true}}
	d.statusChanged()
	if d.lastRender != rendered || d.renderQueued {
		t.Fatal("a pool update redrew the routes page")
	}
	d.lastRender = time.Now().Add(-time.Minute)
	d.status.Slots = []engine.SlotStatus{{Name: "first", Tier: 1, Main: "A"}}
	d.statusChanged()
	if !d.lastRender.After(rendered) {
		t.Fatal("a slot status change did not redraw the routes page")
	}
}

func TestConnectHost(t *testing.T) {
	for in, want := range map[string]string{"0.0.0.0": "127.0.0.1", "::": "127.0.0.1", "127.0.0.1": "127.0.0.1", "192.168.1.5": "192.168.1.5"} {
		if got := connectHost(in); got != want {
			t.Errorf("connectHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func scaled(src image.Image, factor int) image.Image {
	b := src.Bounds()
	dst := image.NewGray(image.Rect(0, 0, b.Dx()*factor, b.Dy()*factor))
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+x/factor, b.Min.Y+y/factor))
		}
	}
	return dst
}

func TestFitRunes(t *testing.T) {
	measure := func(s string) float32 { return float32(len([]rune(s))) }
	cases := []struct {
		value string
		width float32
		want  string
	}{
		{"short", 10, "short"},
		{"Швеция exit", 8, "Швеция…"},
		{"ab · cdef", 6, "ab…"},
		{"abc", 0, "…"},
		{"", 3, ""},
	}
	for _, c := range cases {
		if got := fitRunes(c.value, c.width, measure); got != c.want {
			t.Errorf("fitRunes(%q, %v) = %q, want %q", c.value, c.width, got, c.want)
		}
	}
}

func TestSortProbes(t *testing.T) {
	probes := []engine.Probe{
		{Name: "dead", OK: false, Latency: time.Millisecond},
		{Name: "flaky-fast", OK: true, Samples: 9, Flakiness: 40, SpeedN: 1, Speed: 90, Latency: 20 * time.Millisecond},
		{Name: "steady-slow", OK: true, Samples: 9, Flakiness: 2, SpeedN: 1, Speed: 10, Latency: 90 * time.Millisecond},
		{Name: "steady-fast", OK: true, Samples: 9, Flakiness: 5, SpeedN: 1, Speed: 50, Latency: 300 * time.Millisecond},
		{Name: "steady-untested", OK: true, Samples: 9, Flakiness: 0, Latency: 10 * time.Millisecond},
	}
	names := func(list []engine.Probe) string {
		out := make([]string, len(list))
		for i, p := range list {
			out[i] = p.Name
		}
		return strings.Join(out, ",")
	}
	for mode, want := range map[string]string{
		sortRecommended: "steady-fast,steady-slow,steady-untested,flaky-fast,dead",
		sortLatency:     "steady-untested,flaky-fast,steady-slow,steady-fast,dead",
		sortName:        "dead,flaky-fast,steady-fast,steady-slow,steady-untested",
		sortProvider:    "dead,flaky-fast,steady-slow,steady-fast,steady-untested",
	} {
		if got := names(sortProbes(probes, mode)); got != want {
			t.Errorf("%s: got %s, want %s", mode, got, want)
		}
	}
	if probes[0].Name != "dead" {
		t.Fatal("sortProbes reordered its input")
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.Local)
	for offset, want := range map[time.Duration]string{
		10 * time.Second: "updated just now",
		5 * time.Minute:  "updated 5 min ago",
		3 * time.Hour:    "updated 3 h ago",
		72 * time.Hour:   "updated 09 Nov 2026",
	} {
		if got := ago(now.Add(-offset), now); got != want {
			t.Errorf("ago(-%v) = %q, want %q", offset, got, want)
		}
	}
	if got := ago(time.Time{}, now); got != "never fetched" {
		t.Errorf("zero time = %q", got)
	}
}

func TestEntriesInUse(t *testing.T) {
	used := entriesInUse(engine.Status{Slots: []engine.SlotStatus{
		{Tier: 1, Main: "exit"},
		{Tier: 2, Entry: "de", Main: "exit"},
		{HopOnly: true, Main: "se"},
		{Tier: 0, Err: "down"},
	}})
	if len(used) != 2 || !used["de"] || !used["se"] {
		t.Fatalf("entriesInUse = %v", used)
	}
}

func TestNodeFilterHidesRowsWithoutRebuild(t *testing.T) {
	d := routeTestDashboard(t)
	d.status.Country = []engine.Probe{
		{Name: "Germany", Server: "de.example", OK: true},
		{Name: "Sweden", Server: "se.example", OK: false},
	}
	d.navigate("subscriptions")
	rendered := d.lastRender
	visible := func() []string {
		var names []string
		findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
			if row, ok := obj.(*fyne.Container); ok && !row.Visible() {
				return true // a hidden subtree
			}
			if text, ok := obj.(*fitText); ok && (text.text == "Germany" || text.text == "Sweden") {
				names = append(names, text.text)
			}
			return false
		})
		return names
	}
	if got := visible(); len(got) != 2 {
		t.Fatalf("unfiltered rows = %v", got)
	}
	test.Type(d.inputs["nodes"], "se.ex")
	if got := visible(); len(got) != 1 || got[0] != "Sweden" {
		t.Fatalf("search by server = %v", got)
	}
	if d.lastRender != rendered {
		t.Fatal("typing a search rebuilt the page")
	}
	d.nodeQuery = ""
	d.inputs["nodes"].SetText("")
	d.nodesOnline = true
	d.render()
	if got := visible(); len(got) != 1 || got[0] != "Germany" {
		t.Fatalf("online only = %v", got)
	}
	if d.lastRender == rendered {
		t.Fatal("expected the explicit render above")
	}
}

func TestOverviewAndSubscriptionsStayNarrow(t *testing.T) {
	long := strings.Repeat("Очень длинное имя узла ", 8)
	d := routeTestDashboard(t)
	d.state.Slots[0].Name = long
	d.state.TunSlot = long
	d.state.PinEntry = long
	d.state.Subs = []store.Subscription{{Name: long, URL: "https://x", FetchErr: long}}
	probe := engine.Probe{Name: long, Server: strings.Repeat("sub.", 30) + "example", OK: true, Latency: time.Second, SpeedN: 2, Speed: 0.4, Samples: 9, Flakiness: 50}
	d.status = engine.Status{Tier: 2, Entry: long, Main: long, Note: long, TunErr: long, Country: []engine.Probe{probe}, Bypass: []engine.Probe{probe},
		Sweep: engine.Sweep{Node: long, Total: 3}, Slots: []engine.SlotStatus{{Name: long, Tier: 2, Entry: long, Main: long}}}
	for _, page := range []string{"overview", "subscriptions"} {
		d.navigate(page)
		if width := d.window.Content().MinSize().Width; width > 760 {
			t.Fatalf("%s: long names force window width %.0f", page, width)
		}
	}
}

func visibleFitTexts(root fyne.CanvasObject, names ...string) []string {
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	var shown []string
	findRouteObjects(root, func(obj fyne.CanvasObject) bool {
		if !obj.Visible() {
			return true
		}
		if text, ok := obj.(*fitText); ok && want[text.text] {
			shown = append(shown, text.text)
		}
		return false
	})
	return shown
}

func TestNodesGroupedByProviderAndCollapsible(t *testing.T) {
	d := routeTestDashboard(t)
	d.state.Subs = []store.Subscription{
		{Name: "Main provider", URL: "https://a", Nodes: []store.Node{{Name: "DE"}, {Name: "SE", Whitelist: true}}},
		{Name: "Spare provider", URL: "https://b", Nodes: []store.Node{{Name: "NL"}}},
	}
	d.state.ActiveSub = 0
	d.status.Country = []engine.Probe{{Name: "DE", OK: true}, {Name: "XX", OK: true}}
	d.status.Bypass = []engine.Probe{{Name: "SE", OK: false}}
	d.navigate("subscriptions")

	headers := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool { _, ok := obj.(*clickable); return ok })
	if len(headers) != 3 {
		t.Fatalf("expected Main, Spare and Other groups, got %d headers", len(headers))
	}
	if got := strings.Join(visibleFitTexts(d.content, "DE", "SE", "NL", "XX"), ","); got != "DE,SE,XX" {
		t.Fatalf("inactive provider should start folded, visible = %s", got)
	}
	test.Tap(headers[0].(*clickable))
	test.Tap(headers[1].(*clickable))
	if got := strings.Join(visibleFitTexts(d.content, "DE", "SE", "NL", "XX"), ","); got != "NL,XX" {
		t.Fatalf("after toggling both providers, visible = %s", got)
	}
	d.render()
	if got := strings.Join(visibleFitTexts(d.content, "DE", "SE", "NL", "XX"), ","); got != "NL,XX" {
		t.Fatalf("folding was lost on rebuild, visible = %s", got)
	}
	if !routeHasText(d.content, "Provider not in use") {
		t.Fatal("unchecked nodes of an unused provider should say so")
	}
}

func TestActivityErrorBadgeAndDetails(t *testing.T) {
	d := routeTestDashboard(t)
	d.navigate("overview")
	d.addLog(loghub.Record{Level: loghub.Info, Module: "route", Message: "fine"})
	d.addLog(loghub.Record{Level: loghub.Error, Module: "route", Message: "exit refused the handshake"})
	if !d.errorBadge.box.Visible() || d.errorBadge.text.Text != "1" {
		t.Fatalf("badge after one error: visible=%t text=%q", d.errorBadge.box.Visible(), d.errorBadge.text.Text)
	}
	d.navigate("activity")
	if d.errorBadge.box.Visible() {
		t.Fatal("opening Activity should clear the error badge")
	}
	d.addLog(loghub.Record{Level: loghub.Error, Module: "route", Message: "seen while open"})
	d.flushLogs()
	if d.errorBadge.box.Visible() {
		t.Fatal("errors shown on the open Activity page are not unseen")
	}
	d.logList.Select(1)
	if d.logDetail == nil || d.logDetail.Message != "exit refused the handshake" {
		t.Fatalf("details = %+v", d.logDetail)
	}
	labels := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		label, ok := obj.(*widget.Label)
		return ok && label.Text == "exit refused the handshake"
	})
	if len(labels) != 1 {
		t.Fatal("details strip does not show the full message")
	}
	checks := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		check, ok := obj.(*widget.Check)
		return ok && check.Text == "Errors 2"
	})
	if len(checks) != 1 {
		t.Fatal("severity toggle should count errors")
	}
}

func TestLogArrivalsAreBatched(t *testing.T) {
	d := routeTestDashboard(t)
	scheduled := 0
	d.scheduleFlush = func() { scheduled++ }
	d.navigate("activity")
	for i := 0; i < 50; i++ {
		d.addLog(loghub.Record{Level: loghub.Info, Module: "route", Message: fmt.Sprint("record ", i)})
	}
	if scheduled != 1 {
		t.Fatalf("50 arrivals scheduled %d flushes, want 1", scheduled)
	}
	if d.logList.Length() != 0 {
		t.Fatal("the list redrew before the batched flush")
	}
	d.flushLogs()
	if d.logList.Length() != 50 {
		t.Fatalf("after flush the list shows %d records", d.logList.Length())
	}
	d.addLog(loghub.Record{Level: loghub.Info, Module: "route", Message: "next"})
	if scheduled != 2 {
		t.Fatal("a record after a flush should schedule the next one")
	}
}

func TestCopySubscriptionLink(t *testing.T) {
	d := routeTestDashboard(t)
	d.state.Subs = []store.Subscription{
		{Name: "Alpha sub", URL: "https://alpha.example.com/sub/token123"},
		{Name: "Beta sub", URL: "https://beta.example.com/sub/token456"},
	}
	d.navigate("subscriptions")

	linkBtns := iconButtons(d, "Copy subscription link")
	qrBtns := iconButtons(d, "Export subscription QR code")
	if len(linkBtns) != 2 || len(qrBtns) != 2 {
		t.Fatalf("expected 2 link buttons and 2 qr buttons, got %d and %d", len(linkBtns), len(qrBtns))
	}

	findActions := findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		hbox, ok := obj.(*fyne.Container)
		if !ok {
			return false
		}
		linkIdx, qrIdx := -1, -1
		for i, child := range hbox.Objects {
			if child == linkBtns[0] {
				linkIdx = i
			}
			if child == qrBtns[0] {
				qrIdx = i
			}
		}
		if linkIdx >= 0 && qrIdx >= 0 {
			if linkIdx != qrIdx-1 {
				t.Fatalf("link button index (%d) is not immediately left of qr button (%d)", linkIdx, qrIdx)
			}
			return true
		}
		return false
	})
	if len(findActions) == 0 {
		t.Fatal("could not find container holding both link and qr buttons")
	}

	linkBtns[0].Tapped(nil)
	if got := d.app.Clipboard().Content(); got != "https://alpha.example.com/sub/token123" {
		t.Fatalf("clipboard = %q, want https://alpha.example.com/sub/token123", got)
	}
	if d.notice.Text != "Subscription link copied" {
		t.Fatalf("notice = %q, want 'Subscription link copied'", d.notice.Text)
	}

	linkBtns[1].Tapped(nil)
	if got := d.app.Clipboard().Content(); got != "https://beta.example.com/sub/token456" {
		t.Fatalf("clipboard = %q, want https://beta.example.com/sub/token456", got)
	}
	if d.notice.Text != "Subscription link copied" {
		t.Fatalf("notice = %q, want 'Subscription link copied'", d.notice.Text)
	}

	d.state.Subs = append(d.state.Subs, store.Subscription{Name: "Empty sub", URL: ""})
	d.render()
	linkBtns = iconButtons(d, "Copy subscription link")
	if len(linkBtns) != 3 || !linkBtns[2].Disabled {
		t.Fatal("button for subscription with empty URL should be disabled")
	}
}

func TestSubscriptionQRExportDefaultsToLinkAndAllowsNodes(t *testing.T) {
	d := routeTestDashboard(t)
	d.state.Subs = []store.Subscription{
		{
			Name:  "Test sub",
			URL:   "https://example.com/sub/mytoken",
			Nodes: []store.Node{{Name: "Node A", Raw: linkA}, {Name: "Node B", Raw: linkB}},
		},
	}
	d.navigate("subscriptions")

	qrBtns := iconButtons(d, "Export subscription QR code")
	if len(qrBtns) != 1 {
		t.Fatalf("expected 1 qr button, got %d", len(qrBtns))
	}

	test.Tap(qrBtns[0])
	if !d.dialogOpen {
		t.Fatal("export dialog did not open")
	}

	// 1. By default, it's in Subscription Link mode.
	tapRouteDialogButton(t, d, "Copy link")
	if got := d.app.Clipboard().Content(); got != "https://example.com/sub/mytoken" {
		t.Fatalf("clipboard = %q, want subscription URL", got)
	}
	if d.notice.Text != "Link copied" {
		t.Fatalf("notice = %q, want 'Link copied'", d.notice.Text)
	}

	// 2. Switch to optional Nodes mode.
	tapRouteDialogButton(t, d, "Nodes as VLESS (2)")
	tapRouteDialogButton(t, d, "Copy link")
	if got := d.app.Clipboard().Content(); got != linkA {
		t.Fatalf("clipboard = %q, want node A link", got)
	}

	tapRouteDialogButton(t, d, "Copy all links")
	if got := d.app.Clipboard().Content(); got != linkA+"\n"+linkB {
		t.Fatalf("clipboard = %q, want both links", got)
	}

	// 3. Switch back to Subscription Link mode.
	tapRouteDialogButton(t, d, "Subscription link")
	tapRouteDialogButton(t, d, "Copy link")
	if got := d.app.Clipboard().Content(); got != "https://example.com/sub/mytoken" {
		t.Fatalf("clipboard = %q, want subscription URL after switching back", got)
	}

	tapRouteDialogButton(t, d, "Close")
	if d.dialogOpen {
		t.Fatal("dialog should be closed")
	}
}

func TestExportDialogClosesOnEscape(t *testing.T) {
	d := routeTestDashboard(t)
	d.state.Subs = []store.Subscription{
		{
			Name:  "Test sub",
			URL:   "https://example.com/sub/mytoken",
			Nodes: []store.Node{{Name: "Node A", Raw: linkA}},
		},
	}
	d.navigate("subscriptions")

	qrBtns := iconButtons(d, "Export subscription QR code")
	if len(qrBtns) != 1 {
		t.Fatalf("expected 1 qr button, got %d", len(qrBtns))
	}

	test.Tap(qrBtns[0])
	if !d.dialogOpen || d.activeDialog == nil {
		t.Fatal("export dialog did not open")
	}

	// Pressing Escape should close the subscription export dialog
	handler := d.window.Canvas().OnTypedKey()
	if handler == nil {
		t.Fatal("no OnTypedKey handler registered on canvas")
	}
	handler(&fyne.KeyEvent{Name: fyne.KeyEscape})

	if d.dialogOpen || d.activeDialog != nil {
		t.Fatal("subscription export dialog should close after Escape key press")
	}

	// Verify standard share export window also closes on Escape
	d.showShare("Share test", []share.Link{{Name: "test", URL: "vless://example"}}, 0)
	if !d.dialogOpen || d.activeDialog == nil {
		t.Fatal("share dialog did not open")
	}

	handler(&fyne.KeyEvent{Name: fyne.KeyEscape})
	if d.dialogOpen || d.activeDialog != nil {
		t.Fatal("share dialog should close after Escape key press")
	}

	// Pressing Escape when no dialog is open is safe and a no-op
	handler(&fyne.KeyEvent{Name: fyne.KeyEscape})
	if d.dialogOpen {
		t.Fatal("dialog should remain closed")
	}
}

func TestHWIDQRExport(t *testing.T) {
	d := routeTestDashboard(t)
	d.settingsSection = "Device"
	d.state.Device.HWID = "test-hwid-1234-5678"
	d.navigate("settings")

	var qrBtn *widget.Button
	for _, obj := range findRouteObjects(d.content, func(obj fyne.CanvasObject) bool {
		b, ok := obj.(*widget.Button)
		return ok && b.Icon != nil && strings.Contains(b.Icon.Name(), "qr")
	}) {
		qrBtn = obj.(*widget.Button)
		break
	}
	if qrBtn == nil {
		t.Fatal("HWID QR export button not found in Device settings")
	}

	test.Tap(qrBtn)
	if !d.dialogOpen || d.activeDialog == nil {
		t.Fatal("HWID QR export dialog did not open")
	}

	handler := d.window.Canvas().OnTypedKey()
	handler(&fyne.KeyEvent{Name: fyne.KeyEscape})
	if d.dialogOpen || d.activeDialog != nil {
		t.Fatal("HWID export dialog should close on Escape")
	}
}
