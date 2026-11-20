//go:build !android && cgo

package gui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"clashvless/branding"
	"clashvless/internal/control"
	"clashvless/internal/engine"
	"clashvless/internal/loghub"
	"clashvless/internal/share"
	"clashvless/internal/store"
	"clashvless/internal/xray"
)

type dashboard struct {
	dialogOpen      bool
	logRefresh      func() // re-filters the Activity list; only valid while that page is shown
	pauseLog        bool
	scroll          *container.Scroll
	scrollPage      string
	tabsScroll      *container.Scroll
	inputs          map[string]*widget.Entry
	drafts          map[string]string // unsaved settings text, kept across rebuilds
	tips            *tooltipLayer
	renderedStatus  string
	lastRender      time.Time
	renderQueued    bool
	app             fyne.App
	window          fyne.Window
	client          *control.Client
	embedded        bool
	state           *store.State
	status          engine.Status
	logs            []loghub.Record
	logFilter       loghub.Filter
	logList         *widget.List
	page            string
	selectedSlot    string
	settingsSection string
	nodeQuery       string
	nodeSort        string
	nodesOnline     bool
	collapsedSubs   map[string]bool // provider URL → node group folded by the user
	errorBadge      *countBadge
	unseenErrors    int
	logDetail       *loghub.Record
	logFlushQueued  bool
	scheduleFlush   func() // tests replace the timed flush
	content         *fyne.Container
	notice          *widget.Label
	nav             map[string]*widget.Button
}

func RunClient(client *control.Client, embedded bool) error {
	a := app.NewWithID("com.clashvless")
	a.Settings().SetTheme(nativeTheme{})
	a.SetIcon(logoResource())
	w := a.NewWindow("Clashvless")
	w.SetIcon(logoResource())
	w.SetPadded(false)
	w.Resize(fitScreen(fyne.NewSize(900, 640)))
	w.CenterOnScreen()
	d := &dashboard{
		app: a, window: w, client: client, embedded: embedded,
		page: "overview", settingsSection: "Network & failover", nav: map[string]*widget.Button{},
		logFilter: loghub.DefaultFilter(), inputs: map[string]*widget.Entry{}, drafts: map[string]string{},
	}
	d.buildShell()
	go d.readEvents()
	w.ShowAndRun()
	return nil
}

func (d *dashboard) readEvents() {
	for event := range d.client.Events() {
		event := event
		fyne.Do(func() {
			switch event.Type {
			case "state":
				var state store.State
				if json.Unmarshal(event.State, &state) == nil {
					d.state = &state
					d.render()
				}
			case "status":
				if event.Status != nil {
					d.status = *event.Status
					d.statusChanged()
				}
			case "log":
				record := loghub.ParseLine(event.Line)
				if event.Record != nil {
					record = *event.Record
				}
				d.addLog(record)
			}
		})
	}
	fyne.Do(func() { d.notice.SetText("Daemon disconnected") })
}

func (d *dashboard) buildShell() {
	logo := canvas.NewImageFromResource(logoResource())
	logo.FillMode = canvas.ImageFillContain
	logo.SetMinSize(fyne.NewSize(30, 30))
	brand := container.NewHBox(logo, headingText(branding.Name, 16))
	navButton := func(page, name string) *widget.Button {
		button := widget.NewButtonWithIcon(name, brandIcon(page), func() { d.navigate(page) })
		button.Alignment = widget.ButtonAlignLeading
		button.Importance = widget.LowImportance
		d.nav[page] = button
		return button
	}
	d.errorBadge = newCountBadge()
	items := []fyne.CanvasObject{
		navButton("overview", "Overview"),
		navButton("subscriptions", "Subscriptions"),
		navButton("routes", "Exit routes"),
		container.NewStack(navButton("activity", "Activity"), inset(container.NewHBox(layout.NewSpacer(), container.NewCenter(d.errorBadge.box)), 0, 0, 0, 8)),
	}
	mode := "Attached to daemon"
	if d.embedded {
		mode = "Local engine"
	}
	footer := container.NewVBox(
		navButton("settings", "Settings"),
		widget.NewSeparator(),
		inset(container.NewVBox(muted(mode), muted("Version "+store.Version)), 0, 0, 4, 4),
	)
	sidebarContent := container.NewBorder(inset(brand, 18, 14, 12, 12), inset(footer, 8, 12, 8, 8), nil, nil,
		inset(container.NewVBox(items...), 8, 8, 8, 8))
	background := canvas.NewRectangle(colorSidebar)
	sidebar := container.New(&widthLayout{width: 154}, container.NewStack(background, sidebarContent))

	d.notice = widget.NewLabel("Ready")
	d.notice.Truncation = fyne.TextTruncateEllipsis
	d.notice.SizeName = theme.SizeNameCaptionText
	d.content = container.NewStack(container.NewCenter(widget.NewLabel("Starting local engine…")))
	statusBar := container.NewStack(
		canvas.NewRectangle(colorSidebar),
		inset(d.notice, 4, 12, 12, 12),
	)
	main := container.NewBorder(nil, statusBar, nil, nil, d.content)
	d.tips = newTooltipLayer()
	d.window.SetContent(container.NewStack(container.NewBorder(nil, nil, sidebar, nil, main), d.tips.layer))
}

func (d *dashboard) addLog(record loghub.Record) {
	d.logs = append(d.logs, record)
	if len(d.logs) > 2000 {
		d.logs = d.logs[len(d.logs)-2000:]
	}
	if record.Level == loghub.Error && d.page != "activity" {
		d.unseenErrors++
		d.errorBadge.set(d.unseenErrors)
	}
	if d.page == "activity" && d.logRefresh != nil && !d.logFlushQueued {
		d.logFlushQueued = true
		if d.scheduleFlush != nil {
			d.scheduleFlush()
		} else {
			time.AfterFunc(logFlushGap, func() { fyne.Do(d.flushLogs) })
		}
	}
}

// logFlushGap batches arriving records: redrawing the list per record kept the
// UI thread busy whenever traffic produced records faster than it could paint.
const logFlushGap = 250 * time.Millisecond

func (d *dashboard) flushLogs() {
	d.logFlushQueued = false
	if d.page != "activity" || d.logRefresh == nil {
		return
	}
	d.logRefresh()
	if !d.pauseLog {
		d.logList.ScrollToBottom()
	}
}

// statusRenderGap bounds how often status events rebuild the page: a rebuild
// between press and release swaps the widget under the pointer and drops the click.
const statusRenderGap = 1500 * time.Millisecond

func (d *dashboard) statusChanged() {
	if d.state == nil || d.renderedStatus == d.statusKey() {
		return
	}
	if wait := statusRenderGap - time.Since(d.lastRender); wait > 0 {
		if !d.renderQueued {
			d.renderQueued = true
			time.AfterFunc(wait, func() {
				fyne.Do(func() {
					d.renderQueued = false
					if d.renderedStatus != d.statusKey() {
						d.render()
					}
				})
			})
		}
		return
	}
	d.render()
}

// statusKey is the part of the status the current page shows; an unchanged key
// means a status event has nothing new to draw.
func (d *dashboard) statusKey() string {
	var shown any
	switch d.page {
	case "settings", "activity":
		return d.renderedStatus
	case "subscriptions":
		shown = []any{d.status.Country, d.status.Bypass, d.status.Sweep.Node, entriesInUse(d.status)}
	case "routes":
		shown = d.status.Slots
	default:
		shown = d.status
	}
	data, _ := json.Marshal(shown)
	return string(data)
}

func (d *dashboard) navigate(page string) {
	d.page = page
	if page == "activity" {
		d.unseenErrors = 0
		d.errorBadge.set(0)
	}
	d.render()
}

func (d *dashboard) render() {
	if d.state == nil || d.dialogOpen {
		return
	}
	prevScroll, prevPage, prevTabs := d.scroll, d.scrollPage, d.tabsScroll
	d.tabsScroll = nil
	d.tips.hide(nil)
	var focused *widget.Entry
	focusedKey := ""
	for key, entry := range d.inputs {
		if d.window.Canvas().Focused() == entry {
			focused, focusedKey = entry, key
		}
	}
	d.inputs = map[string]*widget.Entry{}
	for name, button := range d.nav {
		button.Importance = widget.LowImportance
		if name == d.page {
			button.Importance = widget.HighImportance
		}
		button.Refresh()
	}
	titles := map[string][2]string{
		"overview":      {"Overview", "Connection and local proxy"},
		"subscriptions": {"Subscriptions", "Providers and entry nodes"},
		"routes":        {"Exit routes", "Failover slots and exits"},
		"activity":      {"Activity", "Live engine events"},
		"settings":      {"Settings", "Application preferences"},
	}
	var body fyne.CanvasObject
	switch d.page {
	case "subscriptions":
		body = d.subscriptionsView()
	case "routes":
		body = d.routesView()
	case "activity":
		body = d.activityView()
	case "settings":
		body = d.settingsView()
	default:
		body = d.overviewView()
	}
	recheck := widget.NewButtonWithIcon("Recheck", theme.ViewRefreshIcon(), func() {
		d.command(control.Command{Cmd: "kick"}, "Route check started")
	})
	title := container.NewVBox(headingText(titles[d.page][0], 20), muted(titles[d.page][1]))
	headerContent := container.NewVBox(container.NewBorder(nil, nil, title, recheck))
	headerBottomPadding := float32(16)
	if d.page == "routes" {
		headerContent.Add(inset(routingCompatibilityPanel(), 6, 0, 0, 0))
		headerBottomPadding = 8
	}
	header := container.NewStack(
		canvas.NewRectangle(colorSidebar),
		inset(headerContent, 10, headerBottomPadding, 16, 16),
	)
	var scroll *container.Scroll
	area := inset(body, 12, 16, 16, 16)
	if d.page != "activity" { // the log list scrolls itself and fills the page
		scroll = container.NewVScroll(area)
		area = container.NewStack(scroll)
	}
	d.content.Objects = []fyne.CanvasObject{container.NewBorder(header, nil, nil, nil, area)}
	d.content.Refresh()
	if d.page == prevPage && prevScroll != nil && scroll != nil {
		scroll.ScrollToOffset(prevScroll.Offset)
	}
	if d.page == prevPage && prevTabs != nil && d.tabsScroll != nil {
		d.tabsScroll.ScrollToOffset(prevTabs.Offset)
	}
	d.scroll, d.scrollPage = scroll, d.page
	d.lastRender = time.Now()
	d.renderedStatus = ""
	d.renderedStatus = d.statusKey()
	if entry := d.inputs[focusedKey]; entry != nil && focused != nil {
		entry.CursorRow, entry.CursorColumn = focused.CursorRow, focused.CursorColumn
		d.window.Canvas().Focus(entry)
	}
}

// iconButton is a compact icon action whose hint floats in the tooltip layer.
func (d *dashboard) iconButton(icon fyne.Resource, hint string, tapped func()) *tooltipIconButton {
	button := newTooltipIconButton(icon, tapped, nil)
	button.Hint = hint
	button.OnHover = func(in bool) {
		if in {
			d.tips.show(button, hint)
		} else {
			d.tips.hide(button)
		}
	}
	return button
}

// textInput is an entry that survives render(), which rebuilds the page on every
// status event: changed must store the text so the next build can restore it.
func (d *dashboard) textInput(key, placeholder, text string, changed func(string)) *widget.Entry {
	entry := widget.NewEntry()
	entry.SetPlaceHolder(placeholder)
	entry.SetText(text)
	entry.OnChanged = changed
	d.inputs[key] = entry
	return entry
}

func (d *dashboard) command(cmd control.Command, message string) {
	if err := d.client.Send(cmd); err != nil {
		d.notify("Command failed: " + err.Error())
		return
	}
	d.notify(message)
}

func (d *dashboard) patch(values map[string]any) {
	data, err := json.Marshal(values)
	if err != nil {
		d.notify("Could not save: " + err.Error())
		return
	}
	if err := d.client.SendPatch(data); err != nil {
		d.notify("Could not save: " + err.Error())
		return
	}
	d.notify("Saved")
}

func (d *dashboard) notify(message string) {
	d.notice.SetText(message)
}

func (d *dashboard) updateSlots(mutate func([]store.Slot)) {
	if d.state == nil {
		return
	}
	data, _ := json.Marshal(d.state.Slots)
	var slots []store.Slot
	_ = json.Unmarshal(data, &slots)
	mutate(slots)
	d.state.Slots = slots
	d.patch(map[string]any{"slots": slots})
}

func (d *dashboard) ask(title, label, initial, confirm string, apply func(string)) {
	if d.dialogOpen {
		return
	}
	entry := widget.NewEntry()
	entry.SetText(initial)
	form := dialog.NewForm(title, confirm, "Cancel", []*widget.FormItem{widget.NewFormItem(label, entry)}, func(ok bool) {
		if ok {
			apply(strings.TrimSpace(entry.Text))
		}
	}, d.window)
	form.Resize(fyne.NewSize(520, 150))
	d.showDialog(form)
}

func (d *dashboard) confirm(title, message string, apply func()) {
	if d.dialogOpen {
		return
	}
	form := dialog.NewConfirm(title, message, func(ok bool) {
		if ok {
			apply()
		}
	}, d.window)
	d.showDialog(form)
}

func (d *dashboard) showDialog(form dialog.Dialog) {
	// Keep the underlying widget tree stable until the modal releases focus.
	d.dialogOpen = true
	d.tips.hide(nil) // the modal covers its button, so MouseOut never comes
	form.SetOnClosed(func() {
		d.dialogOpen = false
		d.render()
	})
	form.Show()
}

func (d *dashboard) addSubscription() {
	d.ask("Add subscription", "Subscription URL", "", "Add", func(value string) {
		if !strings.HasPrefix(strings.ToLower(value), "http://") && !strings.HasPrefix(strings.ToLower(value), "https://") {
			d.notify("Enter a valid http(s) subscription URL")
			return
		}
		d.command(control.Command{Cmd: "addsub", URL: value}, "Subscription added; fetching nodes…")
	})
}

func (d *dashboard) askMultiline(title, label, placeholder, confirm string, apply func(string)) {
	if d.dialogOpen {
		return
	}
	entry := widget.NewMultiLineEntry()
	entry.SetPlaceHolder(placeholder)
	entry.Wrapping = fyne.TextWrapBreak
	entry.SetMinRowsVisible(6)
	form := dialog.NewForm(title, confirm, "Cancel", []*widget.FormItem{widget.NewFormItem(label, entry)}, func(ok bool) {
		if ok {
			apply(entry.Text)
		}
	}, d.window)
	form.Resize(fitScreen(fyne.NewSize(620, 320)))
	d.showDialog(form)
}

func (d *dashboard) addMain(slotIndex int) {
	if d.state == nil || slotIndex < 0 || slotIndex >= len(d.state.Slots) {
		d.notify("This slot no longer exists")
		return
	}
	slot := d.state.Slots[slotIndex]
	slotName := slot.Name
	if slot.IsProxy() {
		d.addProxyExits(slotName)
		return
	}
	d.askMultiline("Add final exits", "VLESS links", "vless://… — one per line, or a base64 subscription", "Add", func(value string) {
		links, invalid := share.ParseExits(value)
		if len(links) == 0 {
			d.notify("Enter at least one valid vless:// link")
			return
		}
		if d.state.SlotIndexByName(slotName) < 0 {
			d.notify("This slot was removed while editing")
			return
		}
		added, duplicate := 0, 0
		d.updateSlots(func(slots []store.Slot) {
			slot := &slots[d.state.SlotIndexByName(slotName)]
			for _, link := range links {
				if slot.HasMain(link) {
					duplicate++
					continue
				}
				slot.AddMain(link)
				added++
			}
		})
		message := fmt.Sprintf("Added %d exit(s) to %s", added, slotName)
		if duplicate > 0 {
			message += fmt.Sprintf(" · %d already present", duplicate)
		}
		if invalid > 0 {
			message += fmt.Sprintf(" · %d invalid line(s) skipped", invalid)
		}
		d.notify(message)
	})
}

func (d *dashboard) addProxyExits(slotName string) {
	if d.dialogOpen {
		return
	}
	// Tab 0: copy-paste (URLs or host:port:user:pass / user:pass@host:port / host:port).
	pasteProto := widget.NewSelect([]string{"SOCKS5", "HTTP"}, nil)
	pasteProto.SetSelected("SOCKS5")
	pasteEntry := widget.NewMultiLineEntry()
	pasteEntry.SetPlaceHolder("socks5://user:pass@host:port\nhttp://user:pass@host:port\nhost:port:user:pass\nuser:pass@host:port")
	pasteEntry.Wrapping = fyne.TextWrapBreak
	pasteEntry.SetMinRowsVisible(6)
	pasteTab := container.NewVBox(
		widget.NewForm(widget.NewFormItem("Default protocol", pasteProto)),
		muted("Used when pasted lines omit http:// or socks5://"),
		pasteEntry,
	)

	// Tab 1: manual parameter fields.
	manualProto := widget.NewSelect([]string{"SOCKS5", "HTTP"}, nil)
	manualProto.SetSelected("SOCKS5")
	hostEntry := widget.NewEntry()
	hostEntry.SetPlaceHolder("1.2.3.4 or proxy.example.com")
	portEntry := widget.NewEntry()
	portEntry.SetPlaceHolder("1080")
	userEntry := widget.NewEntry()
	userEntry.SetPlaceHolder("Optional")
	passEntry := widget.NewPasswordEntry()
	passEntry.SetPlaceHolder("Optional")
	nameEntry := widget.NewEntry()
	nameEntry.SetPlaceHolder("Optional (defaults to host:port)")
	allowDirect := widget.NewCheck("Allow direct (T1)", nil)
	allowDirect.SetChecked(true)
	manualTab := container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("Protocol", manualProto),
			widget.NewFormItem("Host", hostEntry),
			widget.NewFormItem("Port", portEntry),
			widget.NewFormItem("Username", userEntry),
			widget.NewFormItem("Password", passEntry),
			widget.NewFormItem("Name", nameEntry),
		),
		allowDirect,
	)

	tabs := container.NewAppTabs(
		container.NewTabItem("Copy-paste", pasteTab),
		container.NewTabItem("Manual", manualTab),
	)

	schemeFor := func(label string) string {
		if strings.EqualFold(label, "HTTP") {
			return "http"
		}
		return "socks5"
	}

	form := dialog.NewCustomConfirm("Add HTTP / SOCKS5 exits", "Add", "Cancel", tabs, func(ok bool) {
		if !ok {
			return
		}
		if d.state == nil || d.state.SlotIndexByName(slotName) < 0 {
			d.notify("This slot was removed while editing")
			return
		}
		if tabs.SelectedIndex() == 0 {
			links, invalid := share.ParseProxyExits(pasteEntry.Text, schemeFor(pasteProto.Selected))
			if len(links) == 0 {
				d.notify("Enter at least one valid HTTP/SOCKS5 proxy")
				return
			}
			added, duplicate := 0, 0
			d.updateSlots(func(slots []store.Slot) {
				slot := &slots[d.state.SlotIndexByName(slotName)]
				for _, link := range links {
					if slot.HasMain(link) {
						duplicate++
						continue
					}
					slot.AddMain(link)
					added++
				}
			})
			message := fmt.Sprintf("Added %d proxy exit(s) to %s", added, slotName)
			if duplicate > 0 {
				message += fmt.Sprintf(" · %d already present", duplicate)
			}
			if invalid > 0 {
				message += fmt.Sprintf(" · %d invalid line(s) skipped", invalid)
			}
			d.notify(message)
			return
		}

		port, err := strconv.Atoi(strings.TrimSpace(portEntry.Text))
		if err != nil || port < 1 || port > 65535 {
			d.notify("Enter a valid port (1–65535)")
			return
		}
		link, err := xray.FormatProxyURL(xray.ProxySpec{
			Scheme: schemeFor(manualProto.Selected),
			Host:   strings.TrimSpace(hostEntry.Text),
			Port:   port,
			User:   userEntry.Text,
			Pass:   passEntry.Text,
			Name:   strings.TrimSpace(nameEntry.Text),
		})
		if err != nil {
			d.notify(err.Error())
			return
		}
		dup := false
		d.updateSlots(func(slots []store.Slot) {
			slot := &slots[d.state.SlotIndexByName(slotName)]
			if slot.HasMain(link) {
				dup = true
				return
			}
			slot.AddMain(link)
			slot.Mains[len(slot.Mains)-1].AllowNoHop = allowDirect.Checked
		})
		if dup {
			d.notify("This proxy is already in " + slotName)
		} else {
			d.notify("Added proxy exit to " + slotName)
		}
	}, d.window)
	form.Resize(fitScreen(fyne.NewSize(560, 420)))
	d.showDialog(form)
}

func (d *dashboard) addSlot() {
	if d.state == nil || d.dialogOpen {
		return
	}
	nameEntry := widget.NewEntry()
	nameEntry.SetText(fmt.Sprintf("slot-%d", len(d.state.Slots)+1))
	typeSelect := widget.NewSelect([]string{"VLESS", "HTTP / SOCKS5 proxy"}, nil)
	typeSelect.SetSelected("VLESS")
	form := dialog.NewForm("Add slot", "Add", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Slot name", nameEntry),
		widget.NewFormItem("Slot type", typeSelect),
	}, func(ok bool) {
		if !ok {
			return
		}
		name := strings.TrimSpace(nameEntry.Text)
		if d.state == nil || name == "" || d.state.SlotIndexByName(name) >= 0 {
			d.notify("Enter a unique slot name")
			return
		}
		if strings.Contains(name, "://") {
			d.notify("Slot name cannot contain ://")
			return
		}
		slotType := ""
		if typeSelect.Selected == "HTTP / SOCKS5 proxy" {
			slotType = store.SlotTypeProxy
		}
		data, _ := json.Marshal(d.state.Slots)
		var slots []store.Slot
		_ = json.Unmarshal(data, &slots)
		newSlot := store.Slot{Name: name, Type: slotType, Port: d.state.FreeSlotPort(), Enabled: true}
		slots = append(slots, newSlot)
		d.selectedSlot = name
		d.state.Slots = slots
		d.patch(map[string]any{"slots": slots})
	}, d.window)
	form.Resize(fyne.NewSize(520, 200))
	d.showDialog(form)
}

func (d *dashboard) renameSlot(slotIndex int) {
	if d.state == nil || slotIndex < 0 || slotIndex >= len(d.state.Slots) {
		d.notify("This slot no longer exists")
		return
	}
	oldName := d.state.Slots[slotIndex].Name
	d.ask("Rename slot", "Slot name", oldName, "Save", func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			d.notify("Enter a valid slot name")
			return
		}
		if strings.Contains(name, "://") {
			d.notify("Slot name cannot contain ://")
			return
		}
		if name == oldName {
			return
		}
		for index, slot := range d.state.Slots {
			if index != slotIndex && slot.Name == name {
				d.notify("Slot name already exists")
				return
			}
		}
		data, _ := json.Marshal(d.state.Slots)
		var slots []store.Slot
		_ = json.Unmarshal(data, &slots)
		if slotIndex >= len(slots) || slots[slotIndex].Name != oldName {
			return
		}
		slots[slotIndex].Name = name
		patch := map[string]any{"slots": slots}
		if d.state.TunSlot == oldName {
			patch["tun_slot"] = name
			d.state.TunSlot = name
		}
		if d.selectedSlot == oldName {
			d.selectedSlot = name
		}
		d.state.Slots = slots
		d.patch(patch)
	})
}

// mainRef names an exit by slot name and URL, so an action confirmed after
// another client reordered or removed exits still edits the intended one.
type mainRef struct {
	slot string
	url  string
}

func (d *dashboard) mainRef(slotIndex, mainIndex int) (mainRef, bool) {
	if d.state == nil || slotIndex < 0 || slotIndex >= len(d.state.Slots) {
		return mainRef{}, false
	}
	mains := d.state.Slots[slotIndex].Mains
	if mainIndex < 0 || mainIndex >= len(mains) {
		return mainRef{}, false
	}
	return mainRef{slot: d.state.Slots[slotIndex].Name, url: mains[mainIndex].URL}, true
}

// updateMain applies mutate to the referenced exit; false if it no longer exists.
func (d *dashboard) updateMain(ref mainRef, mutate func(slot *store.Slot, index int)) bool {
	slotIndex := d.state.SlotIndexByName(ref.slot)
	if slotIndex < 0 || d.state.Slots[slotIndex].MainIndex(ref.url) < 0 {
		d.notify("This exit was changed elsewhere; nothing was edited")
		return false
	}
	d.updateSlots(func(slots []store.Slot) {
		mutate(&slots[slotIndex], slots[slotIndex].MainIndex(ref.url))
	})
	return true
}

func (d *dashboard) removeMain(slotIndex, mainIndex int) {
	ref, ok := d.mainRef(slotIndex, mainIndex)
	if !ok {
		return
	}
	name := d.state.Slots[slotIndex].Mains[mainIndex].Name
	d.confirm("Delete exit", "Remove "+name+" from this failover slot?", func() {
		d.updateMain(ref, func(slot *store.Slot, index int) { slot.RemoveMain(index) })
	})
}

func (d *dashboard) moveMain(slotIndex, mainIndex, delta int) {
	ref, ok := d.mainRef(slotIndex, mainIndex)
	if !ok {
		return
	}
	if d.updateMain(ref, func(slot *store.Slot, index int) { slot.MoveMain(index, index+delta) }) {
		d.notify("Priority updated")
	}
}

func (d *dashboard) renameMain(slotIndex, mainIndex int) {
	ref, ok := d.mainRef(slotIndex, mainIndex)
	if !ok {
		return
	}
	d.ask("Rename exit", "Exit name", d.state.Slots[slotIndex].Mains[mainIndex].Name, "Save", func(name string) {
		name = strings.Join(strings.Fields(name), " ")
		if name == "" {
			d.notify("Enter an exit name")
			return
		}
		d.updateMain(ref, func(slot *store.Slot, index int) { slot.Mains[index].Name = name })
	})
}

func (d *dashboard) setMainFlag(slotIndex, mainIndex int, set func(*store.Main)) {
	if ref, ok := d.mainRef(slotIndex, mainIndex); ok {
		d.updateMain(ref, func(slot *store.Slot, index int) { set(&slot.Mains[index]) })
	}
}

func logoResource() fyne.Resource {
	data, _ := branding.Logo()
	return fyne.NewStaticResource("clashvless-logo.svg", data)
}

func brandIcon(name string) fyne.Resource {
	data, _ := branding.Icon(name)
	return theme.NewThemedResource(fyne.NewStaticResource("clashvless-"+name+".svg", data))
}

func headingText(value string, size float32) *canvas.Text {
	text := canvas.NewText(value, colorText)
	text.TextSize = size
	text.TextStyle = fyne.TextStyle{Bold: true}
	return text
}

func strong(value string) *canvas.Text {
	return headingText(value, 14)
}

func muted(value string) *canvas.Text {
	text := canvas.NewText(value, colorMuted)
	text.TextSize = 11
	return text
}

func wrappingMuted(value string) *widget.Label {
	label := widget.NewLabel(value)
	label.Wrapping = fyne.TextWrapWord
	label.SizeName = theme.SizeNameCaptionText
	return label
}

func ellipsized(value string) *widget.Label {
	// Paragraph separators can make Fyne's single-line truncator exhaust its glyph runs.
	label := widget.NewLabel(strings.Join(strings.Fields(value), " "))
	label.Truncation = fyne.TextTruncateEllipsis
	return label
}

func mutedLine(value string) *widget.Label {
	label := ellipsized(value)
	label.Importance = widget.LowImportance
	label.SizeName = theme.SizeNameCaptionText
	return label
}

func statusText(value string, ok bool) *canvas.Text {
	c := colorDanger
	if ok {
		c = colorAccent
	}
	text := canvas.NewText(value, c)
	text.TextSize = 20
	text.TextStyle = fyne.TextStyle{Bold: true}
	return text
}

func panel(title, subtitle string, objects ...fyne.CanvasObject) fyne.CanvasObject {
	items := make([]fyne.CanvasObject, 0, len(objects)+2)
	if title != "" {
		items = append(items, strong(title))
	}
	if subtitle != "" {
		items = append(items, wrappingMuted(subtitle))
	}
	items = append(items, objects...)
	content := inset(container.NewVBox(items...), 11, 11, 12, 12)
	background := canvas.NewRectangle(colorSurface)
	background.CornerRadius = 8
	background.StrokeColor = colorBorder
	background.StrokeWidth = 1
	return container.NewStack(background, content)
}

// routingCompatibilityPanel keeps the warning visible above the scrollable routes.
func routingCompatibilityPanel() fyne.CanvasObject {
	border := canvas.NewRectangle(colorSurface)
	border.StrokeColor = colorDanger
	border.StrokeWidth = 1
	border.CornerRadius = 3
	mark := canvas.NewText("!", colorDanger)
	mark.TextStyle = fyne.TextStyle{Bold: true}
	mark.TextSize = 13
	icon := container.NewGridWrap(fyne.NewSize(20, 20), container.NewStack(border, container.NewCenter(mark)))
	title := container.NewBorder(nil, nil, nil, icon, strong("Routing compatibility"))
	return panel("", "", title, wrappingMuted("Vision exits are direct-only. REALITY exits require a non-Vision hop; plain VLESS and HTTP/SOCKS5 exits support any hop."))
}

func metric(name, value string) fyne.CanvasObject {
	return container.NewVBox(muted(name), headingText(value, 17))
}

func inset(object fyne.CanvasObject, top, bottom, left, right float32) *fyne.Container {
	return container.New(layout.NewCustomPaddedLayout(top, bottom, left, right), object)
}

func flexRow(primary, accessory fyne.CanvasObject) *fyne.Container {
	return container.New(&flexLayout{}, primary, accessory)
}

type flexLayout struct{}

func (l *flexLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	if len(objects) != 2 {
		return
	}
	accessorySize := objects[1].MinSize()
	primaryWidth := max(0, size.Width-accessorySize.Width-10)
	primarySize := fyne.NewSize(primaryWidth, objects[0].MinSize().Height)
	objects[0].Resize(primarySize)
	objects[0].Move(fyne.NewPos(0, (size.Height-primarySize.Height)/2))
	objects[1].Resize(accessorySize)
	objects[1].Move(fyne.NewPos(size.Width-accessorySize.Width, (size.Height-accessorySize.Height)/2))
}

func (l *flexLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	if len(objects) != 2 {
		return fyne.Size{}
	}
	first, second := objects[0].MinSize(), objects[1].MinSize()
	return fyne.NewSize(first.Width+second.Width+10, max(first.Height, second.Height))
}

type widthLayout struct {
	width float32
}

func (l *widthLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	for _, object := range objects {
		object.Resize(size)
	}
}

func (l *widthLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	height := float32(0)
	for _, object := range objects {
		height = max(height, object.MinSize().Height)
	}
	return fyne.NewSize(l.width, height)
}

func milliseconds(duration time.Duration) string {
	if duration <= 0 {
		return "—"
	}
	return strconv.FormatInt(duration.Milliseconds(), 10) + " ms"
}

func mainHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "Invalid exit URL"
	}
	return parsed.Host
}
