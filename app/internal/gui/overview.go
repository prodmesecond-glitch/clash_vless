//go:build !android && cgo

package gui

import (
	"fmt"
	"image/color"
	"net"
	"strconv"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"clashvless/internal/engine"
)

// connectionSummary never claims "Connected" unless system traffic is really captured:
// a working SOCKS route says nothing about the VPN.
func connectionSummary(vpnWanted bool, s engine.Status) (headline string, good bool, vpnLabel string) {
	online := s.Tier > 0
	switch {
	case vpnWanted && s.TunErr != "":
		return "Not protected", false, "VPN failed"
	case vpnWanted && !s.TunUp:
		return "Connecting", false, "VPN on · Connecting"
	case vpnWanted && online:
		return "Connected", true, "VPN on · Connected"
	case vpnWanted:
		return "No route", false, "VPN on · No route"
	case online:
		return "Proxy ready", true, "VPN off"
	}
	return "Disconnected", false, "VPN off"
}

// connectionHint explains the headline in plain words.
func connectionHint(vpnWanted bool, s engine.Status) string {
	switch {
	case vpnWanted && s.TunErr != "":
		return "The VPN could not start, so system traffic goes out directly."
	case vpnWanted && !s.TunUp:
		return "Starting the VPN; system traffic goes out directly until it is up."
	case vpnWanted && s.Tier > 0:
		return "All system traffic goes through the route below."
	case vpnWanted:
		return "The VPN is up, but no exit works yet. Retrying automatically."
	case s.Tier > 0:
		return "Apps set to use a local SOCKS5 address are routed. Turn the VPN on to route everything."
	case s.HopOnly:
		return "No final exit works; only the first-hop port is served."
	}
	return "No exit works yet. Retrying automatically."
}

// section is a panel whose header carries an optional action on the right.
func section(title, subtitle string, action fyne.CanvasObject, items ...fyne.CanvasObject) fyne.CanvasObject {
	head := fyne.CanvasObject(strong(title))
	if subtitle != "" {
		head = container.New(layout.NewCustomPaddedVBoxLayout(2), strong(title), muted(subtitle))
	}
	if action != nil {
		head = flexRow(head, action)
	}
	return panel("", "", append([]fyne.CanvasObject{head}, items...)...)
}

func (d *dashboard) overviewView() fyne.CanvasObject {
	var items []fyne.CanvasObject
	if !d.state.AnyUsableMain() {
		items = append(items, d.setupPanel())
	}
	items = append(items, d.connectionPanel(), d.endpointsPanel(), d.poolsPanel())
	return container.NewVBox(items...)
}

func (d *dashboard) setupPanel() fyne.CanvasObject {
	step := func(done bool, title, hint string, action fyne.CanvasObject) fyne.CanvasObject {
		mark := tintChip("To do", colorWarning)
		if done {
			mark = tintChip("Done", colorAccent)
		}
		text := container.New(layout.NewCustomPaddedVBoxLayout(2), newFitText(title, colorText, 13, true), newFitText(hint, colorMuted, 12, false))
		row := container.NewBorder(nil, nil, inset(mark, 0, 0, 0, 8), nil, text)
		if done || action == nil {
			return row
		}
		return flexRow(row, action)
	}
	return section("Finish setup", "Two steps before traffic can be routed", nil,
		step(len(d.state.Subs) > 0, "Add a subscription", "Provides the entry nodes used as hops (T2/T3)",
			widget.NewButtonWithIcon("Add subscription", theme.ContentAddIcon(), d.addSubscription)),
		widget.NewSeparator(),
		step(false, "Add a final exit", "The server or proxy your traffic finally leaves from",
			widget.NewButtonWithIcon("Add exit", theme.ContentAddIcon(), func() { d.addMain(0) })),
	)
}

func (d *dashboard) connectionPanel() fyne.CanvasObject {
	status := d.status
	headline, good, vpnLabel := connectionSummary(d.state.TunEnabled, status)
	vpnSwitch := newConnectionSwitch(d.state.TunEnabled, func(on bool) {
		d.patch(map[string]any{"tun_enabled": on})
	})
	vpnControl := container.NewVBox(headingText(vpnLabel, 12), container.NewCenter(vpnSwitch))
	top := container.NewBorder(nil, nil, nil, vpnControl,
		container.NewVBox(statusText(headline, good), newFitText(connectionHint(d.state.TunEnabled, status), colorMuted, 12, false)))

	route, routeColor := "No working route yet", colorMuted
	switch {
	case status.Tier > 0:
		route, routeColor = "This device  →  "+status.Main, colorText
		if status.Entry != "" {
			route = "This device  →  " + status.Entry + "  →  " + status.Main
		}
	case status.HopOnly:
		route, routeColor = "This device  →  "+status.Main+"  (first hop only)", colorWarning
	case status.Err != "":
		route, routeColor = status.Err, colorDanger
	}
	routeLine := container.NewBorder(nil, nil, inset(muted("ROUTE"), 0, 0, 0, 8), nil, newFitText(route, routeColor, 13, true))

	tier := "—"
	if status.Tier > 0 {
		tier = fmt.Sprintf("T%d %s", status.Tier, tierName(status.Tier))
	}
	serving, enabled := 0, 0
	for _, slot := range d.state.Slots {
		if slot.Enabled {
			enabled++
		}
	}
	for _, slot := range status.Slots {
		if slot.Tier > 0 {
			serving++
		}
	}
	items := []fyne.CanvasObject{
		top,
		inset(routeLine, 4, 0, 0, 0),
		widget.NewSeparator(),
		container.NewGridWithColumns(3,
			metric("Latency", milliseconds(status.Egress)),
			metric("Route type", tier),
			metric("Slots serving", fmt.Sprintf("%d / %d", serving, enabled)),
		),
	}
	if status.Note != "" {
		items = append(items, newFitText(status.Note, colorMuted, 12, false))
	}
	if status.TunErr != "" {
		items = append(items, alertBox("VPN isn't running: traffic is NOT going through the tunnel. Retrying automatically.", status.TunErr, colorDanger))
	}
	return section("Connection", "", nil, items...)
}

// alertBox is a tinted, bordered note inside a panel.
func alertBox(title, detail string, tone color.NRGBA) fyne.CanvasObject {
	bg := tone
	bg.A = 28
	box := canvas.NewRectangle(bg)
	box.CornerRadius = 6
	box.StrokeColor = tone
	box.StrokeWidth = 1
	heading := widget.NewLabel(title)
	heading.Wrapping = fyne.TextWrapWord
	heading.TextStyle = fyne.TextStyle{Bold: true}
	items := []fyne.CanvasObject{heading}
	if detail != "" {
		items = append(items, wrappingMuted(detail))
	}
	return container.NewStack(box, inset(container.New(layout.NewCustomPaddedVBoxLayout(0), items...), 2, 2, 4, 4))
}

func (d *dashboard) endpointsPanel() fyne.CanvasObject {
	live := map[string]engine.SlotStatus{}
	for _, slot := range d.status.Slots {
		live[slot.Name] = slot
	}
	tunSlot := d.state.TunSlotName()
	host := connectHost(d.state.ListenHost())
	address := func(port int) string { return "socks5://" + net.JoinHostPort(host, strconv.Itoa(port)) }
	copyButton := func(label, value string, importance widget.Importance) *widget.Button {
		button := widget.NewButtonWithIcon(label, theme.ContentCopyIcon(), func() {
			d.app.Clipboard().SetContent(value)
			d.notify("Copied " + value)
		})
		button.Importance = importance
		return button
	}
	var rows []fyne.CanvasObject
	disabled := 0
	for index, slot := range d.state.Slots {
		if !slot.Enabled {
			disabled++
			continue
		}
		var tags []fyne.CanvasObject
		if slot.IsProxy() {
			tags = append(tags, neutralChip("HTTP/SOCKS"))
		}
		if tunSlot != "" && slot.Name == tunSlot {
			tags = append(tags, tintChip("VPN", colorAccent))
		}
		detail := address(slot.Port)
		buttons := []fyne.CanvasObject{}
		if hop := d.state.HopPort(index); hop > 0 {
			detail += fmt.Sprintf("   ·   first hop :%d", hop)
			buttons = append(buttons, copyButton("Copy hop", address(hop), widget.LowImportance))
		}
		buttons = append(buttons, copyButton("Copy", address(slot.Port), widget.MediumImportance))
		liveLine, _ := slotLiveLine(true, live[slot.Name])
		info := container.New(layout.NewCustomPaddedVBoxLayout(2),
			titleWithTags(newFitText(slot.Name, colorText, 14, true), tags...),
			newFitText(detail, colorText, 13, false),
			liveLine,
		)
		if len(rows) > 0 {
			rows = append(rows, widget.NewSeparator())
		}
		rows = append(rows, flexRow(info, container.NewHBox(buttons...)))
	}
	if len(rows) == 0 {
		rows = append(rows, muted("No enabled slots. Enable one in Exit routes."))
	}
	if disabled > 0 {
		rows = append(rows, muted(fmt.Sprintf("%d disabled slot(s) not shown", disabled)))
	}
	manage := widget.NewButtonWithIcon("Exit routes", theme.NavigateNextIcon(), func() { d.navigate("routes") })
	manage.Importance = widget.LowImportance
	return section("Local endpoints", "SOCKS5 addresses for your applications", manage, rows...)
}

func (d *dashboard) poolsPanel() fyne.CanvasObject {
	inUse := entriesInUse(d.status)
	pools := container.NewGridWithColumns(2,
		d.poolCard("Country exits", 2, d.status.Country, inUse),
		d.poolCard("Bypass nodes", 3, d.status.Bypass, inUse),
	)
	items := []fyne.CanvasObject{pools}
	if sweep := d.status.Sweep; sweep.Node != "" {
		items = append(items, newFitText(fmt.Sprintf("Speed test %d / %d · testing %s", sweep.Done+1, sweep.Total, sweep.Node), colorMuted, 12, false))
	}
	view := widget.NewButtonWithIcon("All nodes", theme.NavigateNextIcon(), func() { d.navigate("subscriptions") })
	view.Importance = widget.LowImportance
	return section("Entry pools", "Hop nodes from your subscriptions", view, items...)
}

func (d *dashboard) poolCard(name string, tier int, probes []engine.Probe, inUse map[string]bool) fyne.CanvasObject {
	up, measured := 0, 0
	flaky := 0.0
	var fastest *engine.Probe
	using := ""
	for i := range probes {
		probe := &probes[i]
		if probe.OK {
			up++
			if fastest == nil || probe.Latency < fastest.Latency {
				fastest = probe
			}
		}
		if probe.Samples > 0 {
			measured++
			flaky += probe.Flakiness
		}
		if inUse[probe.Name] && using == "" {
			using = probe.Name
		}
	}
	ratio := 0.0
	if len(probes) > 0 {
		ratio = float64(up) / float64(len(probes))
	}
	barColor := colorAccent
	if ratio < 0.5 {
		barColor = colorWarning
	}
	if up == 0 {
		barColor = colorDanger
	}
	count := headingText(fmt.Sprintf("%d / %d", up, len(probes)), 18)
	head := container.NewBorder(nil, nil, container.NewHBox(strong(name), neutralChip(fmt.Sprintf("T%d", tier))), nil)
	lines := []fyne.CanvasObject{head, container.NewHBox(count, container.NewCenter(muted("online")))}
	if len(probes) == 0 {
		lines = append(lines, newFitText("No nodes in your subscriptions", colorMuted, 12, false))
	} else {
		lines = append(lines, meter(ratio, barColor))
		stability := "Stability: measuring…"
		if measured > 0 {
			avg := flaky / float64(measured)
			stability = fmt.Sprintf("Avg failure rate %.0f%% · last hour", avg)
		}
		lines = append(lines, newFitText(stability, colorMuted, 12, false))
		if fastest != nil {
			lines = append(lines, newFitText("Fastest: "+fastest.Name+" · "+milliseconds(fastest.Latency), colorMuted, 12, false))
		}
		if using != "" {
			lines = append(lines, newFitText("In use: "+using, colorAccent, 12, true))
		}
	}
	bg := canvas.NewRectangle(colorBackground)
	bg.CornerRadius = 6
	return container.NewStack(bg, inset(container.New(layout.NewCustomPaddedVBoxLayout(4), lines...), 8, 8, 10, 10))
}

// entriesInUse names the entry nodes some slot is routing through right now.
func entriesInUse(status engine.Status) map[string]bool {
	used := map[string]bool{}
	for _, slot := range status.Slots {
		switch {
		case slot.HopOnly:
			used[slot.Main] = true
		case slot.Tier > 1 && slot.Entry != "":
			used[slot.Entry] = true
		}
	}
	return used
}

// meter is a thin horizontal bar filled to ratio.
func meter(ratio float64, c color.Color) fyne.CanvasObject {
	track := canvas.NewRectangle(colorControl)
	track.CornerRadius = 3
	fill := canvas.NewRectangle(c)
	fill.CornerRadius = 3
	return container.New(&meterLayout{ratio: float32(min(1, max(0, ratio)))}, track, fill)
}

type meterLayout struct{ ratio float32 }

func (l *meterLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	objects[0].Resize(size)
	objects[1].Resize(fyne.NewSize(size.Width*l.ratio, size.Height))
}

func (l *meterLayout) MinSize([]fyne.CanvasObject) fyne.Size { return fyne.NewSize(40, 6) }
