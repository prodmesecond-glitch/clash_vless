// Package tui is the Bubble Tea dashboard over the failover supervisor. It is a
// btop/mihomo-style tabbed interface: Status, Subs, Tiers, Log, Config.
//
// All store mutations happen on the Bubble Tea (Update) goroutine via
// Supervisor.SetConfig; network fetches run in tea.Cmd goroutines and return
// their results as messages, so nothing races with the supervisor.
package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"clashvless/internal/engine"
	"clashvless/internal/happ"
	"clashvless/internal/store"
	"clashvless/internal/xray"
)

type statusMsg engine.Status
type logMsg string

type fetchResult struct {
	url, title string
	nodes      []store.Node
	ok         bool
	errMsg     string
}
type fetchDoneMsg struct {
	results []fetchResult
	total   int
}
type subAddedMsg struct {
	url, name string
	nodes     []store.Node
	err       error
}

const (
	tabStatus = iota
	tabSubs
	tabMain
	tabLog
	tabConfig
	tabCount
)

var tabNames = []string{"Status", "Subs", "Main", "Log", "Config"}

const (
	inputNone = iota
	inputAddSub
	inputAddMain
	inputEditCfg
)

type model struct {
	st        *store.State
	sup       *engine.Supervisor
	status    engine.Status
	logs      []string
	width     int
	height    int
	tab        int
	cfgCursor  int
	mainCursor int          // Main tab: index into st.Mains
	rowCursor  int          // Subs tab: index into the flattened sub/node rows
	rowScroll int
	expanded  map[int]bool // Subs tab: which subs are expanded (dropdown)
	inputMode int
	inputBuf  string
	busy      string
	subErr    map[string]string // per-sub last fetch error (transient), keyed by URL
}

// Run launches the tabbed TUI. When debug is set, supervisor events are also
// mirrored to events.log.
func Run(st *store.State, debug bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := &model{st: st, subErr: map[string]string{}, expanded: map[int]bool{}}
	p := tea.NewProgram(m, tea.WithAltScreen())

	base := func(l string) { p.Send(logMsg(l)) }
	if debug {
		base = engine.EventFileSink(st.EventsLogPath(), base)
	}
	m.sup = engine.NewSupervisor(st, func(s engine.Status) { p.Send(statusMsg(s)) }, base)
	go func() { _ = m.sup.Run(ctx) }()

	_, err := p.Run()
	cancel()
	return err
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case statusMsg:
		m.status = engine.Status(msg)
	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > 500 {
			m.logs = m.logs[len(m.logs)-500:]
		}
	case fetchDoneMsg:
		m.applyFetch(msg)
	case subAddedMsg:
		m.applyAdd(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// --- key handling ------------------------------------------------------------

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inputMode != inputNone {
		return m.handleInput(msg)
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.tab = (m.tab + 1) % tabCount
		return m, nil
	case "shift+tab":
		m.tab = (m.tab - 1 + tabCount) % tabCount
		return m, nil
	case "1", "2", "3", "4", "5":
		m.tab = int(msg.String()[0] - '1')
		return m, nil
	case "r":
		m.busy = "refetching…"
		return m, m.fetchCmd()
	case "f":
		m.sup.Kick()
		m.busy = "re-evaluating…"
		return m, nil
	}
	switch m.tab {
	case tabSubs:
		return m.handleSubsKey(msg)
	case tabMain:
		return m.handleMainKey(msg)
	case tabConfig:
		m.handleConfigKey(msg)
	}
	return m, nil
}

func (m *model) handleInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.inputMode, m.inputBuf, m.busy = inputNone, "", ""
	case tea.KeyEnter:
		mode, raw := m.inputMode, m.inputBuf
		m.inputMode, m.inputBuf = inputNone, ""
		switch mode {
		case inputAddSub:
			u := sanitizeURL(raw)
			if !happ.ValidSubURL(u) {
				m.busy = "invalid URL — need http(s)://…"
				return m, nil
			}
			m.busy = "adding…"
			return m, m.addCmd(u)
		case inputAddMain:
			m.addMain(sanitizeURL(raw))
		case inputEditCfg:
			if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				m.setCfg(m.cfgCursor, v)
			}
		}
	case tea.KeyBackspace, tea.KeyDelete:
		if r := []rune(m.inputBuf); len(r) > 0 {
			m.inputBuf = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			switch {
			case m.inputMode == inputEditCfg:
				if r >= '0' && r <= '9' {
					m.inputBuf += string(r)
				}
			case r >= 0x20 && r != 0x7f: // printable only — drop \r, escapes, etc.
				m.inputBuf += string(r)
			}
		}
	}
	return m, nil
}

// sanitizeURL strips control characters and surrounding whitespace from pasted
// input before it is treated as a URL.
func sanitizeURL(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func (m *model) handleSubsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.subRows()
	var r subRow
	if m.rowCursor < len(rows) {
		r = rows[m.rowCursor]
	}
	switch msg.String() {
	case "up", "k":
		if m.rowCursor > 0 {
			m.rowCursor--
		}
	case "down", "j":
		if m.rowCursor < len(rows)-1 {
			m.rowCursor++
		}
	case "a":
		m.inputMode, m.inputBuf, m.busy = inputAddSub, "", "paste URL, Enter to add"
	case "enter", "right", "l":
		if len(rows) == 0 {
			break
		}
		if r.node < 0 { // sub header → expand/collapse the dropdown
			m.expanded[r.sub] = !m.expanded[r.sub]
		} else { // node → pin/unpin as the forced entry
			m.pinNode(r.sub, r.node)
		}
	case "left", "h":
		if len(rows) > 0 && r.node < 0 {
			m.expanded[r.sub] = false
		}
	case "s": // use this sub as the active one (manual mode)
		if len(rows) > 0 {
			i := r.sub
			m.sup.SetConfig(func(st *store.State) { st.AutoSelect = false; st.ActiveSub = i })
			_ = m.st.Save()
			m.sup.Kick()
			m.busy = "using " + trunc(m.st.Subs[i].Name, 20)
		}
	case "d":
		if len(rows) > 0 && r.node < 0 {
			i := r.sub
			m.sup.SetConfig(func(st *store.State) { st.RemoveSub(i) })
			delete(m.expanded, i)
			_ = m.st.Save()
			m.sup.Kick()
			m.busy = "removed"
		}
	case " ", "space":
		m.sup.SetConfig(func(st *store.State) { st.AutoSelect = !st.AutoSelect })
		_ = m.st.Save()
		m.sup.Kick()
	}
	return m, nil
}

// pinNode toggles a specific node as the forced entry (overrides tier auto-pick).
func (m *model) pinNode(subIdx, nodeIdx int) {
	if subIdx >= len(m.st.Subs) || nodeIdx >= len(m.st.Subs[subIdx].Nodes) {
		return
	}
	name := m.st.Subs[subIdx].Nodes[nodeIdx].Name
	m.sup.SetConfig(func(st *store.State) {
		if st.PinEntry == name {
			st.PinEntry = ""
		} else {
			st.PinEntry = name
		}
	})
	_ = m.st.Save()
	m.sup.Kick()
	if m.st.PinEntry == name {
		m.busy = "pinned " + trunc(name, 20)
	} else {
		m.busy = "unpinned — auto"
	}
}

type subRow struct{ sub, node int } // node < 0 => sub header row

func (m *model) subRows() []subRow {
	var rows []subRow
	for i := range m.st.Subs {
		rows = append(rows, subRow{i, -1})
		if m.expanded[i] {
			for j := range m.st.Subs[i].Nodes {
				rows = append(rows, subRow{i, j})
			}
		}
	}
	return rows
}

func (m *model) handleConfigKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "up", "k":
		if m.cfgCursor > 0 {
			m.cfgCursor--
		}
	case "down", "j":
		if m.cfgCursor < len(cfgFields)-1 {
			m.cfgCursor++
		}
	case "left", "h", "-":
		m.setCfg(m.cfgCursor, cfgFields[m.cfgCursor].get(m.st)-1)
	case "right", "l", "+", "=":
		m.setCfg(m.cfgCursor, cfgFields[m.cfgCursor].get(m.st)+1)
	case "enter":
		m.inputMode, m.inputBuf, m.busy = inputEditCfg, "", "type a value, Enter to save"
	}
}

func (m *model) setCfg(i, v int) {
	f := cfgFields[i]
	v = clamp(v, f.min, f.max)
	m.sup.SetConfig(func(st *store.State) { f.set(st, v) })
	_ = m.st.Save()
	m.busy = "saved ✓"
}

// --- commands (run in tea goroutines; only touch snapshots) ------------------

func (m *model) fetchCmd() tea.Cmd {
	subs := append([]store.Subscription(nil), m.st.Subs...)
	device := m.st.Device
	return func() tea.Msg {
		res := make([]fetchResult, len(subs))
		total := 0
		for i, sb := range subs {
			nodes, title, err := happ.Fetch(device, sb.URL)
			res[i] = fetchResult{url: sb.URL, title: title, nodes: nodes, ok: err == nil, errMsg: fetchErrMsg(err)}
			if err == nil {
				total += len(nodes)
			}
		}
		return fetchDoneMsg{results: res, total: total}
	}
}

func (m *model) addCmd(u string) tea.Cmd {
	device := m.st.Device
	return func() tea.Msg {
		nodes, title, err := happ.Fetch(device, u)
		return subAddedMsg{url: u, name: title, nodes: nodes, err: err}
	}
}

func (m *model) applyFetch(msg fetchDoneMsg) {
	m.sup.SetConfig(func(st *store.State) {
		for _, r := range msg.results {
			if !r.ok {
				continue
			}
			for i := range st.Subs {
				if st.Subs[i].URL == r.url {
					st.Subs[i].Nodes = r.nodes
					st.Subs[i].LastFetch = time.Now()
					if r.title != "" {
						st.Subs[i].Name = r.title
					}
				}
			}
		}
	})
	_ = m.st.Save()
	m.sup.Kick()
	for _, r := range msg.results {
		if r.ok {
			delete(m.subErr, r.url)
		} else {
			m.subErr[r.url] = r.errMsg
		}
	}
	m.busy = fmt.Sprintf("refetched %d nodes", msg.total)
}

func (m *model) applyAdd(msg subAddedMsg) {
	m.sup.SetConfig(func(st *store.State) {
		st.AddSub(msg.name, msg.url)
		if msg.err == nil {
			sb := &st.Subs[len(st.Subs)-1]
			sb.Nodes = msg.nodes
			sb.LastFetch = time.Now()
		}
	})
	_ = m.st.Save()
	m.sup.Kick()
	if msg.err != nil {
		m.subErr[msg.url] = fetchErrMsg(msg.err)
		m.busy = "added — " + fetchErrMsg(msg.err)
	} else {
		delete(m.subErr, msg.url)
		m.busy = fmt.Sprintf("added %d nodes", len(msg.nodes))
	}
}

// --- config fields -----------------------------------------------------------

type cfgField struct {
	label    string
	get      func(*store.State) int
	set      func(*store.State, int)
	min, max int
	show     func(int) string
	note     string
}

func (f cfgField) showVal(st *store.State) string {
	v := f.get(st)
	if f.show != nil {
		return f.show(v)
	}
	return strconv.Itoa(v)
}

var cfgFields = []cfgField{
	{"Probe interval (s)", func(st *store.State) int { return eff(st.Interval, 12) }, func(st *store.State, v int) { st.Interval = v }, 2, 600, nil, ""},
	{"Egress timeout (s)", func(st *store.State) int { return eff(st.Timeout, 6) }, func(st *store.State, v int) { st.Timeout = v }, 1, 60, nil, ""},
	{"Up threshold", func(st *store.State) int { return eff(st.UpThreshold, 3) }, func(st *store.State, v int) { st.UpThreshold = v }, 1, 10, nil, "cycles a better tier must hold"},
	{"Down threshold", func(st *store.State) int { return eff(st.DownThreshold, 2) }, func(st *store.State, v int) { st.DownThreshold = v }, 1, 10, nil, "fails before switching down"},
	{"Pin tier", func(st *store.State) int { return st.PinTier }, func(st *store.State, v int) { st.PinTier = v }, 0, 3, pinLabel, "0 = auto cascade"},
	{"Force hop (skip T1)", func(st *store.State) int { return b2i(st.ForceHop) }, func(st *store.State, v int) { st.ForceHop = v != 0 }, 0, 1, onOff, "always route through a hop — hop test"},
	{"Listen port", func(st *store.State) int { return st.ListenPort }, func(st *store.State, v int) { st.ListenPort = v }, 1024, 65535, nil, "restart to apply"},
	{"Allow LAN (0.0.0.0)", func(st *store.State) int {
		if st.ListenHost() == "127.0.0.1" {
			return 0
		}
		return 1
	}, func(st *store.State, v int) {
		if v == 0 {
			st.ListenAddr = "127.0.0.1"
		} else {
			st.ListenAddr = "0.0.0.0"
		}
	}, 0, 1, onOff, "off = localhost only; restart to apply"},
	{"First-hop port", func(st *store.State) int { return st.EntryPort }, func(st *store.State, v int) { st.EntryPort = v }, 0, 65535, entryPortLabel, "0 = auto (listen+1); restart to apply"},
}

// --- styles ------------------------------------------------------------------

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("213")).Padding(0, 1)
	tabActive    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("81")).Padding(0, 1)
	tabInactive  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	badStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	activeStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	keyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	boxStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

// --- views -------------------------------------------------------------------

func (m *model) View() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n\n")
	switch m.tab {
	case tabStatus:
		b.WriteString(m.statusView())
	case tabSubs:
		b.WriteString(m.subsView())
	case tabMain:
		b.WriteString(m.mainView())
	case tabLog:
		b.WriteString(m.logView())
	case tabConfig:
		b.WriteString(m.configView())
	}
	b.WriteString("\n\n" + m.footer())
	return b.String()
}

func (m *model) header() string {
	parts := []string{titleStyle.Render("clash_vless"), " "}
	for i, n := range tabNames {
		lbl := fmt.Sprintf("%d·%s", i+1, n)
		if i == m.tab {
			parts = append(parts, tabActive.Render(lbl))
		} else {
			parts = append(parts, tabInactive.Render(lbl))
		}
	}
	parts = append(parts, "  "+dimStyle.Render("v"+store.Version))
	return lipgloss.JoinHorizontal(lipgloss.Center, parts...)
}

func (m *model) footer() string {
	if m.inputMode != inputNone {
		return keyStyle.Render("Enter") + " ok  " + keyStyle.Render("Esc") + " cancel" + m.busySuffix()
	}
	var ctx string
	switch m.tab {
	case tabSubs:
		ctx = keyStyle.Render("a") + " add  " + keyStyle.Render("enter") + " expand/pin  " + keyStyle.Render("s") + " use-sub  " + keyStyle.Render("d") + " del  " + keyStyle.Render("space") + " auto  "
	case tabMain:
		ctx = keyStyle.Render("a") + " add  " + keyStyle.Render("e") + " enable  " + keyStyle.Render("w") + " w/o-hop  " + keyStyle.Render("d") + " del  "
	case tabConfig:
		ctx = keyStyle.Render("enter") + " type  " + keyStyle.Render("←→") + " nudge  "
	}
	return ctx + keyStyle.Render("r") + " refetch  " + keyStyle.Render("f") + " re-eval  " +
		keyStyle.Render("Tab") + " switch  " + keyStyle.Render("q") + " quit" + m.busySuffix()
}

func (m *model) busySuffix() string {
	if m.busy == "" {
		return ""
	}
	return "    " + dimStyle.Render(m.busy)
}

func fetchErrMsg(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, happ.ErrDeviceLimit) {
		return "device limit — free a slot & refetch"
	}
	return trunc(err.Error(), 40)
}

func (m *model) statusView() string {
	s := m.status
	conn := sectionStyle.Render("CONNECTION") + "\n" + activeLine(s)
	if m.st.ForceHop {
		conn += "\n" + keyStyle.Render("force-hop on") + dimStyle.Render("  skipping direct T1")
	}
	if s.Note != "" {
		conn += "\n" + dimStyle.Render(s.Note)
	}
	conn += "\n" + dimStyle.Render(fmt.Sprintf("socks5://%s:%d", m.st.ListenHost(), m.st.ListenPort))
	if s.Tier >= 2 && s.Entry != "" {
		conn += "\n" + dimStyle.Render(fmt.Sprintf("first-hop  :%d → %s", m.st.EntryListenPort(), trunc(s.Entry, 22)))
	}

	nodes := m.st.ActiveNodes()
	nExit, nWl := poolCounts(nodes)
	src := "AUTO (all subs)"
	if !m.st.AutoSelect && m.st.ActiveSub >= 0 && m.st.ActiveSub < len(m.st.Subs) {
		src = m.st.Subs[m.st.ActiveSub].Name
	}
	sub := sectionStyle.Render("SUBSCRIPTION") + "\n" +
		fmt.Sprintf("source  %s\nnodes   %d  (%d exit / %d ОБХОД)\n", trunc(src, 30), len(nodes), nExit, nWl) +
		dimStyle.Render(fmt.Sprintf("device  %s / %s\nhwid    %s", m.st.Device.OS, m.st.Device.Model, shortHWID(m.st.Device.HWID)))

	return lipgloss.JoinVertical(lipgloss.Left,
		boxStyle.Width(50).Render(conn), "",
		boxStyle.Width(50).Render(sub))
}

func (m *model) subsView() string {
	var b strings.Builder
	mode := badStyle.Render("MANUAL")
	if m.st.AutoSelect {
		mode = okStyle.Render("AUTO (all subs)")
	}
	head := sectionStyle.Render("SUBSCRIPTIONS") + "   " + dimStyle.Render("mode: ") + mode
	if m.st.PinEntry != "" {
		head += "   " + activeStyle.Render("📌 "+trunc(m.st.PinEntry, 20))
	}
	b.WriteString(head + "\n\n")

	if len(m.st.Subs) == 0 {
		b.WriteString(dimStyle.Render("  no subs yet — press ") + keyStyle.Render("a") + dimStyle.Render(" to add one"))
		return b.String()
	}

	rows := m.subRows()
	lat := m.probeByName()
	m.clampRow(len(rows))
	h := m.subsVisible()
	end := m.rowScroll + h
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.rowScroll; i < end; i++ {
		r := rows[i]
		cur := "  "
		if i == m.rowCursor {
			cur = activeStyle.Render("▸ ")
		}
		if r.node < 0 { // sub header (dropdown)
			sb := &m.st.Subs[r.sub]
			exp := "▸"
			if m.expanded[r.sub] {
				exp = "▾"
			}
			mark := " "
			if !m.st.AutoSelect && r.sub == m.st.ActiveSub {
				mark = okStyle.Render("●")
			}
			b.WriteString(fmt.Sprintf("%s%s %s %-26s %s\n", cur, mark, exp, trunc(sb.Name, 26), m.subStatus(sb)))
		} else { // node under an expanded sub
			nd := &m.st.Subs[r.sub].Nodes[r.node]
			latStr := dimStyle.Render("   —  ")
			if p, ok := lat[nd.Name]; ok {
				if p.OK {
					latStr = fmt.Sprintf("%5dms", p.Latency.Milliseconds())
				} else {
					latStr = badStyle.Render("   ×  ")
				}
			}
			pinMark := "  "
			if m.st.PinEntry == nd.Name {
				pinMark = activeStyle.Render("📌")
			}
			b.WriteString(fmt.Sprintf("%s    %s %-24s %s\n", cur, pinMark, trunc(nd.Name, 24), latStr))
		}
	}
	if len(rows) > h {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  … %d/%d\n", m.rowCursor+1, len(rows))))
	}
	if m.inputMode == inputAddSub {
		b.WriteString("\n" + sectionStyle.Render("add URL: ") + m.inputBuf + "▏")
	}
	return b.String()
}

func (m *model) subStatus(sb *store.Subscription) string {
	switch {
	case len(sb.Nodes) == 0 && m.subErr[sb.URL] != "":
		return badStyle.Render(m.subErr[sb.URL])
	case len(sb.Nodes) == 0:
		return dimStyle.Render("not fetched — press ") + keyStyle.Render("r")
	}
	ok := m.okServers()
	up := 0
	for _, nd := range sb.Nodes {
		if ok[nd.Server] {
			up++
		}
	}
	if up > 0 {
		return dimStyle.Render(fmt.Sprintf("%d nodes · ", len(sb.Nodes))) + okStyle.Render(fmt.Sprintf("%d up", up))
	}
	return dimStyle.Render(fmt.Sprintf("%d nodes", len(sb.Nodes)))
}

func (m *model) probeByName() map[string]engine.Probe {
	p := map[string]engine.Probe{}
	for _, x := range m.status.Country {
		p[x.Name] = x
	}
	for _, x := range m.status.Bypass {
		p[x.Name] = x
	}
	return p
}

func (m *model) mainView() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("MAIN EXITS") + "   " + dimStyle.Render("w/o-hop → T1 direct · others → hop (T2/T3)") + "\n\n")
	if len(m.st.Mains) == 0 {
		b.WriteString(dimStyle.Render("  no mains — press ") + keyStyle.Render("a") + dimStyle.Render(" to add a vless:// exit"))
		return b.String()
	}
	m.clampMainCursor()
	for i := range m.st.Mains {
		mn := &m.st.Mains[i]
		cur := "  "
		if i == m.mainCursor {
			cur = activeStyle.Render("▸ ")
		}
		live := "  "
		if m.status.Tier > 0 && m.status.Main == mn.Name {
			live = okStyle.Render("● ")
		}
		en := badStyle.Render("[ ]")
		if mn.Enabled {
			en = okStyle.Render("[x]")
		}
		hop := "[ ]"
		if mn.AllowNoHop {
			hop = "[x]"
		}
		vis := ""
		if store.IsVision(mn.URL) {
			vis = keyStyle.Render(" vision")
		}
		b.WriteString(fmt.Sprintf("%s%s%-20s %-19s  enable %s  w/o-hop %s%s\n",
			cur, live, trunc(mn.Name, 20), hostOf(mn.URL), en, hop, vis))
	}
	if m.inputMode == inputAddMain {
		b.WriteString("\n" + sectionStyle.Render("add main URL: ") + m.inputBuf + "▏")
	}
	b.WriteString("\n" + dimStyle.Render("  enable = use it · w/o-hop = allow direct (T1); off = hop-only. Vision must stay direct."))
	return b.String()
}

func (m *model) handleMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.st.Mains)
	switch msg.String() {
	case "up", "k":
		if m.mainCursor > 0 {
			m.mainCursor--
		}
	case "down", "j":
		if m.mainCursor < n-1 {
			m.mainCursor++
		}
	case "a":
		m.inputMode, m.inputBuf, m.busy = inputAddMain, "", "paste vless:// URL, Enter to add"
	case "e", "enter":
		if n > 0 {
			m.toggleMain(m.mainCursor, true)
		}
	case " ", "space", "w":
		if n > 0 {
			m.toggleMain(m.mainCursor, false)
		}
	case "d":
		if n > 0 {
			i := m.mainCursor
			m.sup.SetConfig(func(st *store.State) { st.RemoveMain(i) })
			_ = m.st.Save()
			m.sup.Kick()
			m.busy = "removed"
		}
	}
	return m, nil
}

func (m *model) toggleMain(i int, enable bool) {
	m.sup.SetConfig(func(st *store.State) {
		if i < 0 || i >= len(st.Mains) {
			return
		}
		if enable {
			st.Mains[i].Enabled = !st.Mains[i].Enabled
		} else {
			st.Mains[i].AllowNoHop = !st.Mains[i].AllowNoHop
		}
	})
	_ = m.st.Save()
	m.sup.Kick()
	m.busy = "saved ✓"
}

func (m *model) addMain(u string) {
	if _, err := xray.VlessToOutbound(u, "main"); err != nil {
		m.busy = "invalid vless:// URL"
		return
	}
	m.sup.SetConfig(func(st *store.State) { st.AddMain(u) })
	_ = m.st.Save()
	m.sup.Kick()
	m.busy = "main added"
}

func (m *model) clampMainCursor() {
	if m.mainCursor >= len(m.st.Mains) {
		m.mainCursor = len(m.st.Mains) - 1
	}
	if m.mainCursor < 0 {
		m.mainCursor = 0
	}
}

func (m *model) logView() string {
	head := sectionStyle.Render("EVENT LOG") + "\n"
	if len(m.logs) == 0 {
		return head + dimStyle.Render("  (waiting for events…)")
	}
	n := m.height - 8
	if n < 5 {
		n = 5
	}
	start := 0
	if len(m.logs) > n {
		start = len(m.logs) - n
	}
	var lines []string
	for _, l := range m.logs[start:] {
		lines = append(lines, "  "+dimStyle.Render(l))
	}
	return head + strings.Join(lines, "\n")
}

func (m *model) configView() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("ENGINE CONFIG") + dimStyle.Render("  (Enter to type a value, ←→ to nudge)") + "\n\n")
	for i, f := range cfgFields {
		val := f.showVal(m.st)
		if m.inputMode == inputEditCfg && i == m.cfgCursor {
			val = m.inputBuf + "▏"
		}
		label := fmt.Sprintf("%-20s", f.label)
		var line string
		if i == m.cfgCursor {
			line = activeStyle.Render(fmt.Sprintf("▸ %s %8s", label, val))
		} else {
			line = fmt.Sprintf("  %s %8s", label, val)
		}
		if f.note != "" {
			line += dimStyle.Render("   " + f.note)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// --- helpers -----------------------------------------------------------------

func (m *model) okServers() map[string]bool {
	set := map[string]bool{}
	for _, p := range m.status.Country {
		if p.OK {
			set[p.Server] = true
		}
	}
	for _, p := range m.status.Bypass {
		if p.OK {
			set[p.Server] = true
		}
	}
	return set
}

func (m *model) subsVisible() int {
	h := m.height - 10
	if h < 3 {
		h = 3
	}
	return h
}

func (m *model) clampRow(n int) {
	if m.rowCursor >= n {
		m.rowCursor = n - 1
	}
	if m.rowCursor < 0 {
		m.rowCursor = 0
	}
	h := m.subsVisible()
	if m.rowCursor < m.rowScroll {
		m.rowScroll = m.rowCursor
	}
	if m.rowCursor >= m.rowScroll+h {
		m.rowScroll = m.rowCursor - h + 1
	}
	if m.rowScroll < 0 {
		m.rowScroll = 0
	}
}

func activeLine(s engine.Status) string {
	switch s.Tier {
	case 0:
		msg := s.Err
		if msg == "" {
			msg = "starting…"
		}
		return badStyle.Render("✖ DOWN") + "  " + dimStyle.Render(msg)
	case 1:
		return okStyle.Render("● T1") + "  direct → " + mainLabel(s.Main) + egressStr(s.Egress)
	default:
		return okStyle.Render(fmt.Sprintf("● T%d", s.Tier)) + fmt.Sprintf("  %s → %s", s.Entry, mainLabel(s.Main)) + egressStr(s.Egress)
	}
}

func mainLabel(n string) string {
	if n == "" {
		return "main"
	}
	return trunc(n, 18)
}

func egressStr(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return dimStyle.Render(fmt.Sprintf("    egress %dms", d.Milliseconds()))
}

func poolCounts(nodes []store.Node) (exit, wl int) {
	for _, n := range nodes {
		if n.Whitelist {
			wl++
		} else {
			exit++
		}
	}
	return
}

func hostOf(vurl string) string {
	u, err := url.Parse(vurl)
	if err != nil || u.Hostname() == "" {
		return "(main not set)"
	}
	if p := u.Port(); p != "" {
		return u.Hostname() + ":" + p
	}
	return u.Hostname()
}

func shortHWID(h string) string {
	if len(h) <= 10 {
		return h
	}
	return h[:6] + "…" + h[len(h)-4:]
}

func eff(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func pinLabel(t int) string {
	switch t {
	case 1:
		return "T1"
	case 2:
		return "T2"
	case 3:
		return "T3"
	}
	return "auto"
}

func entryPortLabel(v int) string {
	if v == 0 {
		return "auto"
	}
	return strconv.Itoa(v)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func onOff(v int) string {
	if v != 0 {
		return "on"
	}
	return "off"
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
