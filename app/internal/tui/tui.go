// Package tui is the Bubble Tea dashboard over the failover supervisor. It is a
// btop/mihomo-style tabbed interface: Status, Subs, Tiers, Log, Config.
//
// All store mutations happen on the Bubble Tea (Update) goroutine via
// Supervisor.SetConfig; network fetches run in tea.Cmd goroutines and return
// their results as messages, so nothing races with the supervisor.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"clashvless/internal/control"
	"clashvless/internal/engine"
	"clashvless/internal/happ"
	"clashvless/internal/loghub"
	"clashvless/internal/store"
	"clashvless/internal/xray"
)

type statusMsg engine.Status
type logMsg struct {
	line  string
	debug bool
}
type stateMsg json.RawMessage
type daemonGoneMsg struct{}
type spinTickMsg struct{}

// spinFrames animate the Subs row of the node a speed sweep is testing.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinTick drives the spinner; it's only scheduled while a sweep runs.
func spinTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })
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
	inputSlotType
	inputAddSlot
	inputProxyMode
	inputAddProxyPaste
	inputAddProxyManual
	inputSlotPort
	inputEditCfg
	inputEditStr
)

const (
	pfProto = iota
	pfHost
	pfPort
	pfUser
	pfPass
	pfName
	pfDirect
	pfCount
)

type model struct {
	st          *store.State
	client      *control.Client
	embedded    bool // true = this TUI started its own in-process daemon (no standalone `run`)
	status      engine.Status
	logs        []string
	width       int
	height      int
	tab         int
	cfgCursor   int
	mainCursor  int // Main tab: index into the flattened slot/main rows
	addMainSlot int // Main tab: slot index a new main is added to
	lastKey     time.Time
	portSlot    int // Main tab: slot index whose port is being edited
	rowCursor   int // Subs tab: index into the flattened sub/node rows
	rowScroll   int
	expanded    map[int]bool // Subs tab: which subs are expanded (dropdown)
	inputMode   int
	inputBuf    string
	busy        string

	// slot creation & HTTP/SOCKS5 proxy exit input
	slotTypeCur     int    // 0 = VLESS, 1 = HTTP/SOCKS5
	newSlotType     string // store.SlotTypeVLESS or store.SlotTypeProxy
	proxyModeCur    int    // 0 = Copy-paste, 1 = Manual parameters
	proxyPasteProto string // "socks5" or "http" default for scheme-less copy-paste
	proxyFormField  int
	proxyFormProto  string
	proxyFormHost   string
	proxyFormPort   string
	proxyFormUser   string
	proxyFormPass   string
	proxyFormName   string
	proxyFormDirect bool
	proxyFormShowPw bool
	proxyFormErr    string

	// first-run setup wizard (see wizard.go)
	wiz        int
	wizSeen    bool
	wizSubURL  string
	wizMainURL string
	wizErr     string

	confirmExit bool // showing the "exit? y/n" prompt

	spinFrame int  // current spinner frame while a speed sweep runs
	spinning  bool // a spinTick is scheduled (one chain at a time)
}

// RunClient attaches the TUI to a running daemon over the control socket. It
// renders state/status/log pushed by the daemon and sends commands back. embedded
// records whether the caller started the daemon in-process (vs a standalone `run`).
func RunClient(client *control.Client, embedded bool) error {
	m := &model{client: client, embedded: embedded, expanded: map[int]bool{}}
	p := tea.NewProgram(m, tea.WithAltScreen())
	go func() {
		for e := range client.Events() {
			switch e.Type {
			case "status":
				if e.Status != nil {
					p.Send(statusMsg(*e.Status))
				}
			case "log":
				p.Send(logMsg{e.Line, e.Record != nil && e.Record.Level == loghub.Debug})
			case "state":
				p.Send(stateMsg(e.State))
			}
		}
		p.Send(daemonGoneMsg{})
	}()
	_, err := p.Run()
	return err
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case statusMsg:
		m.status = engine.Status(msg)
		m.clearProgress()
		if m.status.Sweep.Node != "" && !m.spinning {
			m.spinning = true
			return m, spinTick()
		}
	case spinTickMsg:
		if m.status.Sweep.Node == "" {
			m.spinning = false
			return m, nil
		}
		m.spinFrame++
		return m, spinTick()
	case logMsg:
		// Debug records list every destination visited: opt-in only.
		if msg.debug && (m.st == nil || m.st.Loglevel() != "debug") {
			return m, nil
		}
		m.logs = append(m.logs, msg.line)
		if len(m.logs) > 500 {
			m.logs = m.logs[len(m.logs)-500:]
		}
	case stateMsg:
		var st store.State
		if json.Unmarshal([]byte(msg), &st) == nil {
			m.st = &st
			m.maybeStartWizard()
			m.wizAdvanceFetch()
			m.clearProgress()
		}
	case daemonGoneMsg:
		return m, tea.Quit
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// --- key handling ------------------------------------------------------------

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case !m.inTextInput():
		if m.inKeyBurst(msg, time.Now()) {
			m.busy = "pasted text ignored — press a to add a main first"
			return m, nil
		}
		msg = latinKey(msg)
	case msg.Type == tea.KeyCtrlV || (msg.Type == tea.KeyInsert && !msg.Alt):
		// The classic Windows console delivers these keys instead of pasting.
		text := []rune(sanitizeURL(readClipboard()))
		if len(text) == 0 {
			return m, nil
		}
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: text}
	}
	// "exit? y/n" prompt takes over the whole UI once armed.
	if m.confirmExit {
		switch msg.String() {
		case "y", "Y", "enter":
			return m, tea.Quit
		default:
			m.confirmExit = false
		}
		return m, nil
	}
	if msg.String() == "ctrl+c" { // hard exit, always
		return m, tea.Quit
	}
	// `q` is always live to quit — except while typing (where it's a literal char).
	if msg.String() == "q" && !m.inTextInput() {
		m.confirmExit = true
		return m, nil
	}
	if m.wiz != wizOff {
		return m.handleWizardKey(msg)
	}
	if m.inputMode != inputNone {
		return m.handleInput(msg)
	}
	if m.st == nil { // not synced from the daemon yet
		return m, nil
	}
	switch msg.String() {
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
		_ = m.client.Send(control.Command{Cmd: "refetch"})
		return m, nil
	case "f":
		_ = m.client.Send(control.Command{Cmd: "kick"})
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
	switch m.inputMode {
	case inputSlotType:
		return m.handleSlotTypeInput(msg)
	case inputProxyMode:
		return m.handleProxyModeInput(msg)
	case inputAddProxyPaste:
		return m.handleProxyPasteInput(msg)
	case inputAddProxyManual:
		return m.handleProxyManualInput(msg)
	}
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
			_ = m.client.Send(control.Command{Cmd: "addsub", URL: u})
		case inputAddMain:
			m.addMain(sanitizeURL(raw))
		case inputAddSlot:
			name := strings.TrimSpace(raw)
			if strings.Contains(name, "://") {
				m.busy = "that's a URL, not a slot name — press a to add a main"
				return m, nil
			}
			kind := m.newSlotType
			m.apply(func(st *store.State) { st.AddSlotOfType(name, kind) })
			if kind == store.SlotTypeProxy {
				m.busy = "http/socks slot added"
			} else {
				m.busy = "slot added"
			}
		case inputSlotPort:
			if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && v >= 1024 && v <= 65534 {
				s := m.portSlot
				m.apply(func(st *store.State) {
					if s >= 0 && s < len(st.Slots) {
						st.Slots[s].Port = v
					}
				})
				m.busy = "port set — restart the daemon to apply"
			} else {
				m.busy = "invalid port (1024–65534)"
			}
		case inputEditCfg:
			if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				m.setCfg(m.cfgCursor, v)
			}
		case inputEditStr:
			if idx := m.cfgCursor - len(cfgFields); idx >= 0 && idx < len(cfgStrFields) {
				val := strings.TrimSpace(raw)
				if f := cfgStrFields[idx]; f.check != nil && !f.check(val) {
					m.busy = f.bad
					break
				}
				m.apply(func(st *store.State) { cfgStrFields[idx].set(st, val) })
				m.busy = "saved ✓"
			}
		}
	case tea.KeyBackspace, tea.KeyDelete:
		if r := []rune(m.inputBuf); len(r) > 0 {
			m.inputBuf = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			switch {
			case m.inputMode == inputEditCfg || m.inputMode == inputSlotPort:
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

func (m *model) handleSlotTypeInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.inputMode, m.busy = inputNone, ""
	case "up", "down", "left", "right", "tab", "shift+tab", "k", "j", "h", "l":
		m.slotTypeCur = 1 - m.slotTypeCur
	case "1":
		m.slotTypeCur = 0
		m.advanceSlotName()
	case "2":
		m.slotTypeCur = 1
		m.advanceSlotName()
	case "enter":
		m.advanceSlotName()
	}
	return m, nil
}

func (m *model) advanceSlotName() {
	if m.slotTypeCur == 1 {
		m.newSlotType = store.SlotTypeProxy
	} else {
		m.newSlotType = store.SlotTypeVLESS
	}
	m.inputMode, m.inputBuf, m.busy = inputAddSlot, "", "slot name (Enter; blank = auto)"
}

func (m *model) handleProxyModeInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.inputMode, m.busy = inputNone, ""
	case "up", "down", "left", "right", "tab", "shift+tab", "k", "j", "h", "l":
		m.proxyModeCur = 1 - m.proxyModeCur
	case "1":
		m.proxyModeCur = 0
		m.startProxyPaste()
	case "2":
		m.proxyModeCur = 1
		m.startProxyManual()
	case "enter":
		if m.proxyModeCur == 1 {
			m.startProxyManual()
		} else {
			m.startProxyPaste()
		}
	}
	return m, nil
}

func (m *model) startProxyPaste() {
	if m.proxyPasteProto == "" {
		m.proxyPasteProto = "socks5"
	}
	m.inputMode = inputAddProxyPaste
	m.inputBuf = ""
	m.proxyFormErr = ""
	m.busy = "paste proxy, Tab switches default proto, Enter to add"
}

func (m *model) startProxyManual() {
	m.inputMode = inputAddProxyManual
	m.proxyFormField = pfProto
	m.proxyFormProto = "socks5"
	m.proxyFormHost = ""
	m.proxyFormPort = ""
	m.proxyFormUser = ""
	m.proxyFormPass = ""
	m.proxyFormName = ""
	m.proxyFormDirect = true
	m.proxyFormShowPw = false
	m.proxyFormErr = ""
	m.busy = "fill parameters, Enter/Ctrl+S to save"
}

func (m *model) handleProxyPasteInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.inputMode, m.inputBuf, m.proxyFormErr, m.busy = inputNone, "", "", ""
	case tea.KeyTab, tea.KeyShiftTab:
		if m.proxyPasteProto == "http" {
			m.proxyPasteProto = "socks5"
		} else {
			m.proxyPasteProto = "http"
		}
	case tea.KeyEnter:
		raw := sanitizeURL(m.inputBuf)
		spec, err := xray.ParseProxy(raw, m.proxyPasteProto)
		if err != nil {
			m.proxyFormErr = err.Error()
			return m, nil
		}
		u, err := xray.FormatProxyURL(spec)
		if err != nil {
			m.proxyFormErr = err.Error()
			return m, nil
		}
		m.addProxyMain(u, true)
		m.inputMode, m.inputBuf, m.proxyFormErr = inputNone, "", ""
	case tea.KeyBackspace, tea.KeyDelete:
		m.proxyFormErr = ""
		if r := []rune(m.inputBuf); len(r) > 0 {
			m.inputBuf = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		m.proxyFormErr = ""
		for _, r := range msg.Runes {
			if r >= 0x20 && r != 0x7f {
				m.inputBuf += string(r)
			}
		}
	}
	return m, nil
}

func (m *model) handleProxyManualInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.inputMode, m.proxyFormErr, m.busy = inputNone, "", ""
		return m, nil
	case tea.KeyCtrlO:
		m.proxyFormShowPw = !m.proxyFormShowPw
		return m, nil
	case tea.KeyCtrlS:
		m.submitProxyManual()
		return m, nil
	case tea.KeyUp, tea.KeyShiftTab:
		m.proxyFormField = (m.proxyFormField + pfCount - 1) % pfCount
		return m, nil
	case tea.KeyDown, tea.KeyTab:
		m.proxyFormField = (m.proxyFormField + 1) % pfCount
		return m, nil
	case tea.KeyLeft, tea.KeyRight:
		switch m.proxyFormField {
		case pfProto:
			if m.proxyFormProto == "http" {
				m.proxyFormProto = "socks5"
			} else {
				m.proxyFormProto = "http"
			}
		case pfDirect:
			m.proxyFormDirect = !m.proxyFormDirect
		}
		return m, nil
	case tea.KeyEnter:
		if m.proxyFormField < pfCount-1 {
			m.proxyFormField++
		} else {
			m.submitProxyManual()
		}
		return m, nil
	case tea.KeyBackspace, tea.KeyDelete:
		m.proxyFormErr = ""
		if p := m.proxyFormTextPtr(); p != nil {
			if r := []rune(*p); len(r) > 0 {
				*p = string(r[:len(r)-1])
			}
		}
		return m, nil
	case tea.KeySpace:
		switch m.proxyFormField {
		case pfProto:
			if m.proxyFormProto == "http" {
				m.proxyFormProto = "socks5"
			} else {
				m.proxyFormProto = "http"
			}
			return m, nil
		case pfDirect:
			m.proxyFormDirect = !m.proxyFormDirect
			return m, nil
		}
		if p := m.proxyFormTextPtr(); p != nil && m.proxyFormField != pfPort {
			m.proxyFormErr = ""
			*p += " "
		}
		return m, nil
	case tea.KeyRunes:
		if m.proxyFormField == pfProto || m.proxyFormField == pfDirect {
			if len(msg.Runes) == 1 && msg.Runes[0] == ' ' {
				if m.proxyFormField == pfProto {
					if m.proxyFormProto == "http" {
						m.proxyFormProto = "socks5"
					} else {
						m.proxyFormProto = "http"
					}
				} else {
					m.proxyFormDirect = !m.proxyFormDirect
				}
			}
			return m, nil
		}
		if p := m.proxyFormTextPtr(); p != nil {
			m.proxyFormErr = ""
			for _, r := range msg.Runes {
				if m.proxyFormField == pfPort {
					if r >= '0' && r <= '9' {
						*p += string(r)
					}
				} else if r >= 0x20 && r != 0x7f {
					*p += string(r)
				}
			}
		}
	}
	return m, nil
}

func (m *model) proxyFormTextPtr() *string {
	switch m.proxyFormField {
	case pfHost:
		return &m.proxyFormHost
	case pfPort:
		return &m.proxyFormPort
	case pfUser:
		return &m.proxyFormUser
	case pfPass:
		return &m.proxyFormPass
	case pfName:
		return &m.proxyFormName
	default:
		return nil
	}
}

func (m *model) submitProxyManual() {
	host := strings.TrimSpace(m.proxyFormHost)
	if host == "" {
		m.proxyFormErr = "host is required"
		m.proxyFormField = pfHost
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(m.proxyFormPort))
	if err != nil || port < 1 || port > 65535 {
		m.proxyFormErr = "port must be 1–65535"
		m.proxyFormField = pfPort
		return
	}
	u, err := xray.FormatProxyURL(xray.ProxySpec{
		Scheme: m.proxyFormProto,
		Host:   host,
		Port:   port,
		User:   m.proxyFormUser,
		Pass:   m.proxyFormPass,
		Name:   strings.TrimSpace(m.proxyFormName),
	})
	if err != nil {
		m.proxyFormErr = err.Error()
		return
	}
	m.addProxyMain(u, m.proxyFormDirect)
	m.inputMode, m.proxyFormErr = inputNone, ""
}

func (m *model) addProxyMain(u string, allowDirect bool) {
	s := m.addMainSlot
	dup := false
	m.apply(func(st *store.State) {
		var sl *store.Slot
		if s >= 0 && s < len(st.Slots) {
			sl = &st.Slots[s]
		} else {
			sl = st.DefaultSlot()
		}
		if sl.HasMain(u) {
			dup = true
			return
		}
		sl.AddMain(u)
		sl.Mains[len(sl.Mains)-1].AllowNoHop = allowDirect
	})
	if dup {
		m.busy = "proxy already in this slot"
	} else {
		m.busy = "proxy exit added"
	}
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

// keyBurstGap is faster than key repeat or typing. Windows delivers a paste key by
// key with no paste marker, so a burst is the only way to tell it from shortcuts.
const keyBurstGap = 10 * time.Millisecond

// inKeyBurst reports whether a key outside text input belongs to a paste.
func (m *model) inKeyBurst(msg tea.KeyMsg, now time.Time) bool {
	if msg.Type != tea.KeyRunes && msg.Type != tea.KeyEnter {
		return false
	}
	burst := now.Sub(m.lastKey) < keyBurstGap
	m.lastKey = now
	return burst
}

var ruToLatin = map[rune]rune{
	'й': 'q', 'ц': 'w', 'у': 'e', 'к': 'r', 'е': 't', 'н': 'y', 'г': 'u', 'ш': 'i', 'щ': 'o', 'з': 'p',
	'ф': 'a', 'ы': 's', 'в': 'd', 'а': 'f', 'п': 'g', 'р': 'h', 'о': 'j', 'л': 'k', 'д': 'l',
	'я': 'z', 'ч': 'x', 'с': 'c', 'м': 'v', 'и': 'b', 'т': 'n', 'ь': 'm',
}

// latinKey makes shortcuts work on a Russian layout, where the a key sends ф.
func latinKey(msg tea.KeyMsg) tea.KeyMsg {
	if msg.Type != tea.KeyRunes || len(msg.Runes) != 1 {
		return msg
	}
	r := msg.Runes[0]
	l, ok := ruToLatin[unicode.ToLower(r)]
	if !ok {
		return msg
	}
	if unicode.IsUpper(r) {
		l = unicode.ToUpper(l)
	}
	msg.Runes = []rune{l}
	return msg
}

// inTextInput reports whether a keypress should be treated as text (so `q` is a
// literal char, not the quit shortcut).
func (m *model) inTextInput() bool {
	if m.inputMode != inputNone {
		return true
	}
	switch m.wiz {
	case wizSub, wizProxyIn, wizMain, wizHwidIn:
		return true
	}
	return false
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
			m.apply(func(st *store.State) { st.AutoSelect = false; st.ActiveSub = i })
			m.busy = "using " + trunc(m.st.Subs[i].Name, 20)
		}
	case "d":
		if len(rows) > 0 && r.node < 0 {
			i := r.sub
			_ = m.client.Send(control.Command{Cmd: "rmsub", Index: i})
			delete(m.expanded, i)
			m.busy = "removed"
		}
	case " ", "space":
		m.apply(func(st *store.State) { st.AutoSelect = !st.AutoSelect })
	}
	return m, nil
}

// pinNode toggles a specific node as the forced entry (overrides tier auto-pick).
func (m *model) pinNode(subIdx, nodeIdx int) {
	if subIdx >= len(m.st.Subs) || nodeIdx >= len(m.st.Subs[subIdx].Nodes) {
		return
	}
	name := m.st.Subs[subIdx].Nodes[nodeIdx].Name
	willPin := m.st.PinEntry != name
	m.apply(func(st *store.State) {
		if st.PinEntry == name {
			st.PinEntry = ""
		} else {
			st.PinEntry = name
		}
	})
	if willPin {
		m.busy = "using " + trunc(name, 20) + " as hop"
	} else {
		m.busy = "auto hop (unpinned)"
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
	firstStr := len(cfgFields) // free-text rows live just past the int fields
	maxRow := firstStr + len(cfgStrFields) - 1
	switch msg.String() {
	case "up", "k":
		if m.cfgCursor > 0 {
			m.cfgCursor--
		}
	case "down", "j":
		if m.cfgCursor < maxRow {
			m.cfgCursor++
		}
	case "left", "h", "-":
		if m.cfgCursor < firstStr {
			m.setCfg(m.cfgCursor, cfgFields[m.cfgCursor].get(m.st)-1)
		}
	case "right", "l", "+", "=":
		if m.cfgCursor < firstStr {
			m.setCfg(m.cfgCursor, cfgFields[m.cfgCursor].get(m.st)+1)
		}
	case "enter":
		if m.cfgCursor >= firstStr {
			f := cfgStrFields[m.cfgCursor-firstStr]
			m.inputMode, m.inputBuf, m.busy = inputEditStr, f.get(m.st), f.hint
		} else {
			m.inputMode, m.inputBuf, m.busy = inputEditCfg, "", "type a value, Enter to save"
		}
	}
}

func (m *model) setCfg(i, v int) {
	f := cfgFields[i]
	v = clamp(v, f.min, f.max)
	m.apply(func(st *store.State) { f.set(st, v) })
	m.busy = "saved ✓"
}

// --- daemon commands ---------------------------------------------------------

// apply computes the delta from mut and sends it to the daemon as a patch — only
// the changed top-level fields, so it never clobbers daemon-fetched nodes.
func (m *model) apply(mut func(*store.State)) {
	if m.st == nil {
		return
	}
	var cp store.State
	b, _ := json.Marshal(m.st)
	_ = json.Unmarshal(b, &cp)
	mut(&cp)
	_ = m.client.SendPatch(statePatch(m.st, &cp))
}

func statePatch(a, b *store.State) json.RawMessage {
	fa, fb := stateFields(a), stateFields(b)
	out := map[string]json.RawMessage{}
	for k, vb := range fb {
		if !bytes.Equal(fa[k], vb) {
			out[k] = vb
		}
	}
	j, _ := json.Marshal(out)
	return j
}

func stateFields(s *store.State) map[string]json.RawMessage {
	b, _ := json.Marshal(s)
	var mm map[string]json.RawMessage
	_ = json.Unmarshal(b, &mm)
	return mm
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
	{"TUN mode (all traffic)", func(st *store.State) int { return b2i(st.TunEnabled) }, func(st *store.State, v int) { st.TunEnabled = v != 0 }, 0, 1, onOff, "system-wide capture; daemon must be root/admin"},
	{"TUN DNS", func(st *store.State) int { return b2i(st.TunDNSDirect) }, func(st *store.State, v int) { st.TunDNSDirect = v != 0 }, 0, 1, dnsModeLabel, "real-net = your LAN resolver off-tun · static routed = via the exit"},
	{"TUN LAN bypass", func(st *store.State) int { return b2i(st.TunBypassLAN()) }, func(st *store.State, v int) { st.TunTunnelLAN = v == 0 }, 0, 1, onOff, "keep private/LAN IPs off the tunnel — local-only resources (intranet, NAS) stay reachable"},
	{"TUN block IPv6", func(st *store.State) int { return b2i(st.TunBlockIPv6()) }, func(st *store.State, v int) { st.TunAllowIPv6 = v == 0 }, 0, 1, onOff, "block global IPv6 so it can't leak past the IPv4-only tunnel (apps fall back to v4)"},
	{"TUN ICMP ping", func(st *store.State) int { return b2i(st.TunICMP()) }, func(st *store.State, v int) { st.TunNoICMP = v == 0 }, 0, 1, onOff, "answer ping locally so ping works under TUN — device liveness only (ICMP can't cross the tunnel; use curl for the exit)"},
	{"Listen port", func(st *store.State) int { return st.ListenPort }, func(st *store.State, v int) { st.ListenPort = v }, 1024, 65534, nil, "for `up`/`gen` only — slot ports are set in the Main tab (p)"},
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
	{"First-hop port", func(st *store.State) int { return st.EntryPort }, func(st *store.State, v int) { st.EntryPort = v }, 0, 65535, entryPortLabel, "default slot only; 0 = auto (every slot exposes its hop on port+1); restart to apply"},
	{"Log level", func(st *store.State) int { return logLevelIdx(st.Loglevel()) }, func(st *store.State, v int) { st.LogLevel = logLevels[clamp(v, 0, len(logLevels)-1)] }, 0, 4, logLevelLabel, "xray verbosity; restart daemon to apply"},
	{"Use fetch proxy", func(st *store.State) int { return b2i(st.UseFetchProxy) }, func(st *store.State, v int) { st.UseFetchProxy = v != 0 }, 0, 1, onOff, "fetch subs via the proxy below"},
	{"Auto-fetch subs", func(st *store.State) int { return b2i(st.AutoFetch()) }, func(st *store.State, v int) { st.NoAutoFetch = v == 0 }, 0, 1, onOff, "daemon periodically refetches subs so the node set never goes stale"},
	{"Speedtest sweep", func(st *store.State) int { return b2i(st.SpeedTest()) }, func(st *store.State, v int) { st.NoSpeedTest = v == 0 }, 0, 1, onOff, "rank hops by measured hop→main speed; off = ignore speeds, rank by stability + ping"},
	{"Auto-fetch every (h)", func(st *store.State) int { return eff(st.FetchIntervalH, 24) }, func(st *store.State, v int) { st.FetchIntervalH = v }, 1, 168, nil, "hours between auto-fetches"},
}

// cfgStrFields are the free-text (string) rows, rendered just past the numeric
// cfgFields in the Config tab. Enter edits them inline.
type strField struct {
	label string
	get   func(*store.State) string
	set   func(*store.State, string)
	empty string // shown when the value is unset
	note  string
	hint  string            // status-line prompt while editing
	check func(string) bool // optional validator; on failure the status line shows bad
	bad   string
}

var cfgStrFields = []strField{
	{"Fetch proxy", func(st *store.State) string { return st.FetchProxy }, func(st *store.State, v string) { st.FetchProxy = v }, "(unset)", "host:port (SOCKS5)", "type host:port, Enter to save", nil, ""},
	{"TUN static DNS", func(st *store.State) string { return st.TunStaticDNS }, func(st *store.State, v string) { st.TunStaticDNS = v }, "8.8.8.8 (default)", "resolver when TUN DNS = static routed", "type an IP, Enter to save", nil, ""},
	{"Device HWID", func(st *store.State) string { return st.Device.HWID }, func(st *store.State, v string) { st.Device.HWID = v }, "(unset)", "sent on fetch = one panel device slot; paste another device's to share its slot", "type/paste a HWID, Enter to save",
		store.ValidHWID, "HWID must be 10–64 chars: a–z A–Z 0–9 = -"},
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
	warnStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")) // calm amber heads-up
	amberStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))            // proto tag: vision
	wizBoxStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("213")).Padding(1, 3)
)

// protoStyle colours a proto tag by security/protocol kind: plain/socks5 = green,
// vision = amber (direct-only), reality/tls/http = cyan, else dim.
func protoStyle(tag string) lipgloss.Style {
	switch {
	case strings.HasPrefix(tag, "plain"), strings.HasPrefix(tag, "socks"):
		return okStyle
	case strings.HasPrefix(tag, "vision"):
		return amberStyle
	case strings.HasPrefix(tag, "reality"), strings.HasPrefix(tag, "tls"), strings.HasPrefix(tag, "http"):
		return keyStyle
	default:
		return dimStyle
	}
}

// --- views -------------------------------------------------------------------

func (m *model) View() string {
	if m.confirmExit {
		return m.confirmExitView()
	}
	if m.st == nil {
		return "\n  connecting to daemon…  (start it with: clashvless run)\n"
	}
	if m.wiz != wizOff {
		return m.wizardView()
	}
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
	mode := "· daemon"
	if m.embedded {
		mode = "· embedded"
	}
	parts = append(parts, "  "+dimStyle.Render("v"+store.Version+" "+mode))
	return lipgloss.JoinHorizontal(lipgloss.Center, parts...)
}

func (m *model) footer() string {
	switch m.inputMode {
	case inputSlotType, inputProxyMode:
		return keyStyle.Render("↑↓/1-2") + " choose  " + keyStyle.Render("Enter") + " select  " + keyStyle.Render("Esc") + " cancel" + m.busySuffix()
	case inputAddProxyPaste:
		return keyStyle.Render("Tab") + " default proto  " + keyStyle.Render("Enter") + " add  " + keyStyle.Render("Esc") + " cancel" + m.busySuffix()
	case inputAddProxyManual:
		return keyStyle.Render("↑↓/Tab") + " field  " + keyStyle.Render("←→/Space") + " toggle  " + keyStyle.Render("Ctrl+O") + " show pass  " + keyStyle.Render("Ctrl+S/Enter") + " save  " + keyStyle.Render("Esc") + " cancel" + m.busySuffix()
	case inputNone:
	default:
		return keyStyle.Render("Enter") + " ok  " + keyStyle.Render("Esc") + " cancel" + m.busySuffix()
	}
	var ctx string
	switch m.tab {
	case tabSubs:
		ctx = keyStyle.Render("a") + " add  " + keyStyle.Render("enter") + " expand / use-as-hop  " + keyStyle.Render("s") + " use-sub  " + keyStyle.Render("d") + " del  " + keyStyle.Render("space") + " auto  "
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

// clearProgress drops an in-progress indicator (a "…" message like "refetching…")
// once a fresh status/state update lands. Confirmations (no ellipsis) persist.
func (m *model) clearProgress() {
	if strings.HasSuffix(m.busy, "…") {
		m.busy = ""
	}
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
	if sw := m.sweepLabel(); sw != "" {
		conn += "\n" + sw + dimStyle.Render("  (link busy)")
	}
	if s.TunErr != "" {
		conn += "\n" + badStyle.Render("VPN isn't running, traffic is NOT protected. Retrying. "+s.TunErr)
	}
	// One line per enabled slot: its local SOCKS port + live tier (◉ = TUN slot).
	ssByName := map[string]engine.SlotStatus{}
	for _, x := range s.Slots {
		ssByName[x.Name] = x
	}
	tunName := m.st.TunSlotName()
	for i := range m.st.Slots {
		sl := &m.st.Slots[i]
		if !sl.Enabled {
			continue
		}
		mark := "  "
		if sl.Name == tunName {
			mark = okStyle.Render("◉ ")
		}
		line := mark + dimStyle.Render(fmt.Sprintf("socks5://%s:%-5d %s", m.st.ListenHost(), sl.Port, trunc(sl.Name, 12)))
		if st, ok := ssByName[sl.Name]; ok {
			line += "  " + slotLiveLabel(st)
		}
		conn += "\n" + line
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
	b.WriteString(head + "\n")
	t2pct, t2n := poolFlake(m.status.Country)
	t3pct, t3n := poolFlake(m.status.Bypass)
	tail := dimStyle.Render("   — leader = steadiest, then fastest")
	if sw := m.sweepLabel(); sw != "" {
		tail = "   " + sw
	}
	b.WriteString("  " + dimStyle.Render("flakiness 1h: ") + flakeLabel("T2", t2pct, t2n) + dimStyle.Render("  ") + flakeLabel("T3", t3pct, t3n) +
		tail + "\n\n")

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
			flkStr := dimStyle.Render("      ") // 6-wide, matches "   NNN%"
			spdStr := ""
			if p, ok := lat[nd.Name]; ok {
				if p.OK {
					latStr = fmt.Sprintf("%5dms", p.Latency.Milliseconds())
				} else {
					latStr = badStyle.Render("   ×  ")
				}
				if p.Samples > 0 {
					fs := fmt.Sprintf("  %3.0f%%", p.Flakiness)
					switch {
					case p.Flakiness >= 30:
						flkStr = badStyle.Render(fs)
					case p.Flakiness >= 10:
						flkStr = warnStyle.Render(fs)
					default:
						flkStr = okStyle.Render(fs)
					}
				}
				if p.SpeedN > 0 {
					ss := fmt.Sprintf("  %4.0f Mbps", p.Speed)
					if p.Speed < 1 { // measured, but the chain can't carry traffic
						spdStr = badStyle.Render(ss)
					} else {
						spdStr = dimStyle.Render(ss)
					}
				}
			}
			if m.status.Sweep.Node == nd.Name {
				spdStr = warnStyle.Render("  " + m.spinner() + " testing")
			}
			pinMark := "  "
			if m.st.PinEntry == nd.Name {
				pinMark = activeStyle.Render("📌")
			}
			raw := store.OutboundProtoTag(nd.Outbound)
			tag := protoStyle(raw).Render(fmt.Sprintf("%-13s", raw))
			b.WriteString(fmt.Sprintf("%s    %s %-24s %s %s%s%s\n", cur, pinMark, trunc(nd.Name, 24), tag, latStr, flkStr, spdStr))
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

// spinner is the current sweep spinner frame.
func (m *model) spinner() string { return spinFrames[m.spinFrame%len(spinFrames)] }

// sweepLabel renders a running speed sweep's progress ("" when idle).
func (m *model) sweepLabel() string {
	sw := m.status.Sweep
	if sw.Node == "" {
		return ""
	}
	via := "node-direct"
	if sw.Via != "" {
		via = "→ " + sw.Via
	}
	return warnStyle.Render(fmt.Sprintf("⚡ %s sweep %d/%d", m.spinner(), sw.Done+1, sw.Total)) + dimStyle.Render(" "+via+" · "+sw.Ref)
}

func (m *model) subStatus(sb *store.Subscription) string {
	switch {
	case sb.FetchErr != "":
		return badStyle.Render("fetch failed: " + sb.FetchErr)
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

// poolFlake averages the last-hour flakiness across a tier's nodes that have
// enough samples to count, and returns how many nodes that was over.
func poolFlake(ps []engine.Probe) (pct float64, n int) {
	var sum float64
	for _, p := range ps {
		if p.Samples > 0 {
			sum += p.Flakiness
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// flakeLabel renders a "T2 8%·12" style tier summary, colored by how flaky it is.
func flakeLabel(tier string, pct float64, n int) string {
	if n == 0 {
		return dimStyle.Render(tier + " —")
	}
	body := fmt.Sprintf("%s %.0f%%·%d", tier, pct, n)
	switch {
	case pct >= 30:
		return badStyle.Render(body)
	case pct >= 10:
		return warnStyle.Render(body)
	default:
		return okStyle.Render(body)
	}
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

// mainRow addresses one navigable line in the Main tab: a slot header
// (main == -1) or a main within a slot.
type mainRow struct {
	slot int
	main int
}

func (m *model) mainRows() []mainRow {
	var rows []mainRow
	for si := range m.st.Slots {
		rows = append(rows, mainRow{slot: si, main: -1})
		for mi := range m.st.Slots[si].Mains {
			rows = append(rows, mainRow{slot: si, main: mi})
		}
	}
	return rows
}

func (m *model) mainView() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("MAIN SLOTS") + "   " + dimStyle.Render("each slot = a failover pool on its own port · ◉ = TUN slot") + "\n\n")
	if len(m.st.Slots) == 0 {
		b.WriteString(dimStyle.Render("  no slots — press ") + keyStyle.Render("s") + dimStyle.Render(" to add one"))
		return b.String()
	}
	m.clampMainCursor()
	ss := map[string]engine.SlotStatus{}
	for _, s := range m.status.Slots {
		ss[s.Name] = s
	}
	tunName := m.st.TunSlotName()
	ri := 0
	for si := range m.st.Slots {
		sl := &m.st.Slots[si]
		cur := "  "
		if ri == m.mainCursor {
			cur = activeStyle.Render("▸ ")
		}
		ri++
		tunMark := "  "
		if sl.Name == tunName {
			tunMark = okStyle.Render("◉ ")
		}
		en := badStyle.Render("off")
		if sl.Enabled {
			en = okStyle.Render("on ")
		}
		live := dimStyle.Render("idle")
		if st, ok := ss[sl.Name]; ok {
			live = slotLiveLabel(st)
		}
		kindTag := dimStyle.Render("[vless]     ")
		if sl.IsProxy() {
			kindTag = keyStyle.Render("[http/socks]")
		}
		b.WriteString(fmt.Sprintf("%s%s%-14s :%-5d %s %s  %s\n", cur, tunMark, trunc(sl.Name, 14), sl.Port, kindTag, en, live))
		for mi := range sl.Mains {
			mn := &sl.Mains[mi]
			c2 := "     "
			if ri == m.mainCursor {
				c2 = "   " + activeStyle.Render("▸ ")
			}
			ri++
			enb := badStyle.Render("[ ]")
			if mn.Enabled {
				enb = okStyle.Render("[x]")
			}
			hop := "[ ]"
			if mn.AllowNoHop {
				hop = "[x]"
			}
			tag := ""
			if raw := store.ProtoTag(mn.URL); raw != "" {
				tag = "  " + protoStyle(raw).Render(raw)
			}
			b.WriteString(fmt.Sprintf("%s%s %-16s %-17s w/o-hop %s%s\n", c2, enb, trunc(mn.Name, 16), hostOf(mn.URL), hop, tag))
		}
	}
	switch m.inputMode {
	case inputSlotType:
		b.WriteString("\n" + m.slotTypeView())
	case inputAddMain:
		b.WriteString("\n" + sectionStyle.Render("add vless:// URL: ") + m.inputBuf + "▏")
	case inputAddSlot:
		kind := "VLESS"
		if m.newSlotType == store.SlotTypeProxy {
			kind = "HTTP/SOCKS"
		}
		b.WriteString("\n" + sectionStyle.Render("new "+kind+" slot name (blank = auto): ") + m.inputBuf + "▏")
	case inputProxyMode:
		b.WriteString("\n" + m.proxyModeView())
	case inputAddProxyPaste:
		b.WriteString("\n" + m.proxyPasteView())
	case inputAddProxyManual:
		b.WriteString("\n" + m.proxyManualView())
	case inputSlotPort:
		b.WriteString("\n" + sectionStyle.Render("slot SOCKS port: ") + m.inputBuf + "▏")
	}
	b.WriteString("\n" + dimStyle.Render("  ") + keyStyle.Render("s") + dimStyle.Render(" add slot · ") + keyStyle.Render("a") + dimStyle.Render(" add exit · ") + keyStyle.Render("p") + dimStyle.Render(" port · ") + keyStyle.Render("t") + dimStyle.Render(" TUN slot · ") + keyStyle.Render("e") + dimStyle.Render(" enable · ") + keyStyle.Render("w") + dimStyle.Render(" w/o-hop · ") + keyStyle.Render("d") + dimStyle.Render(" delete"))
	return b.String()
}

func (m *model) slotTypeView() string {
	opt := func(i int, num, title, desc string) string {
		cur := "  "
		lbl := fmt.Sprintf("[%s] %-16s", num, title)
		if m.slotTypeCur == i {
			cur = activeStyle.Render("▸ ")
			lbl = activeStyle.Render(lbl)
		}
		return cur + lbl + " " + dimStyle.Render(desc)
	}
	content := sectionStyle.Render("NEW SLOT TYPE") + "\n" +
		opt(0, "1", "VLESS", "vless:// exits (direct T1 or hopped T2/T3)") + "\n" +
		opt(1, "2", "HTTP / SOCKS5", "upstream HTTP/SOCKS5 proxies with optional auth")
	return boxStyle.Render(content)
}

func (m *model) proxyModeView() string {
	slotName := ""
	if m.addMainSlot >= 0 && m.addMainSlot < len(m.st.Slots) {
		slotName = m.st.Slots[m.addMainSlot].Name
	}
	opt := func(i int, num, title, desc string) string {
		cur := "  "
		lbl := fmt.Sprintf("[%s] %-18s", num, title)
		if m.proxyModeCur == i {
			cur = activeStyle.Render("▸ ")
			lbl = activeStyle.Render(lbl)
		}
		return cur + lbl + " " + dimStyle.Render(desc)
	}
	content := sectionStyle.Render("ADD PROXY EXIT · "+slotName) + "\n" +
		opt(0, "1", "Copy-paste", "http://, socks5://, host:port:user:pass, user:pass@host:port") + "\n" +
		opt(1, "2", "Manual parameters", "type protocol, host, port, username, password step by step")
	return boxStyle.Render(content)
}

func (m *model) proxyPasteView() string {
	proto := m.proxyPasteProto
	if proto == "" {
		proto = "socks5"
	}
	var lines []string
	lines = append(lines, sectionStyle.Render("COPY-PASTE PROXY")+"   "+dimStyle.Render("default if no scheme: ")+keyStyle.Render(proto)+dimStyle.Render(" (Tab to switch)"))
	lines = append(lines, keyStyle.Render("› ")+m.inputBuf+"▏")
	lines = append(lines, dimStyle.Render("  examples: socks5://user:pass@1.2.3.4:1080  ·  http://1.2.3.4:8080  ·  1.2.3.4:1080:user:pass"))
	if m.proxyFormErr != "" {
		lines = append(lines, badStyle.Render("  ✖ "+m.proxyFormErr))
	}
	return boxStyle.Render(strings.Join(lines, "\n"))
}

func (m *model) proxyManualView() string {
	passVal := strings.Repeat("•", len([]rune(m.proxyFormPass)))
	if m.proxyFormShowPw {
		passVal = m.proxyFormPass
	}
	directVal := "[ ] no (hop only)"
	if m.proxyFormDirect {
		directVal = "[x] yes (T1 direct + T2/T3 hop)"
	}
	protoVal := "◀ " + strings.ToUpper(m.proxyFormProto) + " ▶"
	rows := []struct {
		label string
		val   string
		empty string
		text  bool
	}{
		{"Protocol", protoVal, "", false},
		{"Host", m.proxyFormHost, "1.2.3.4 or proxy.example.com", true},
		{"Port", m.proxyFormPort, "1080", true},
		{"Username", m.proxyFormUser, "(optional)", true},
		{"Password", passVal, "(optional · Ctrl+O show)", true},
		{"Name", m.proxyFormName, "(optional · defaults to host:port)", true},
		{"Allow direct", directVal, "", false},
	}
	var lines []string
	lines = append(lines, sectionStyle.Render("MANUAL PROXY PARAMETERS"))
	for i, r := range rows {
		cur := "  "
		lbl := fmt.Sprintf("%-13s", r.label)
		val := r.val
		if i == m.proxyFormField {
			cur = activeStyle.Render("▸ ")
			lbl = activeStyle.Render(lbl)
			if r.text {
				val = val + "▏"
			} else {
				val = keyStyle.Render(val)
			}
		} else if val == "" {
			val = dimStyle.Render(r.empty)
		}
		lines = append(lines, cur+lbl+" "+val)
	}
	if m.proxyFormErr != "" {
		lines = append(lines, badStyle.Render("  ✖ "+m.proxyFormErr))
	}
	return boxStyle.Render(strings.Join(lines, "\n"))
}

// slotLiveLabel renders a slot's current failover state compactly.
func slotLiveLabel(s engine.SlotStatus) string {
	switch {
	case s.Tier == 1:
		return okStyle.Render("T1") + " " + trunc(s.Main, 14)
	case s.Tier >= 2:
		return okStyle.Render(fmt.Sprintf("T%d", s.Tier)) + " " + trunc(s.Entry, 10) + "→" + trunc(s.Main, 12)
	case s.HopOnly:
		return warnStyle.Render("hop") + " " + trunc(s.Main, 12)
	case s.Err != "":
		return badStyle.Render("down")
	default:
		return dimStyle.Render("…")
	}
}

func (m *model) handleMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.mainRows()
	n := len(rows)
	m.clampMainCursor()
	var cur mainRow
	cur.main = -1
	if m.mainCursor >= 0 && m.mainCursor < n {
		cur = rows[m.mainCursor]
	}
	switch msg.String() {
	case "up", "k":
		if m.mainCursor > 0 {
			m.mainCursor--
		}
	case "down", "j":
		if m.mainCursor < n-1 {
			m.mainCursor++
		}
	case "s":
		m.slotTypeCur = 0
		m.inputMode, m.inputBuf, m.busy = inputSlotType, "", "choose slot type"
	case "a":
		m.addMainSlot = cur.slot
		if cur.slot >= 0 && cur.slot < len(m.st.Slots) && m.st.Slots[cur.slot].IsProxy() {
			m.proxyModeCur = 0
			m.inputMode, m.inputBuf, m.busy = inputProxyMode, "", "choose input method"
		} else {
			m.inputMode, m.inputBuf, m.busy = inputAddMain, "", "paste vless:// URL, Enter to add"
		}
	case "p":
		m.portSlot = cur.slot
		m.inputMode, m.inputBuf, m.busy = inputSlotPort, "", "slot SOCKS port (Enter)"
	case "t":
		if cur.slot < len(m.st.Slots) {
			name := m.st.Slots[cur.slot].Name
			m.apply(func(st *store.State) { st.TunSlot = name })
			m.busy = "TUN slot → " + name
		}
	case "e", "enter":
		if cur.main >= 0 {
			m.toggleMain(cur.slot, cur.main, true)
		} else {
			m.toggleSlotEnabled(cur.slot)
		}
	case " ", "space", "w":
		if cur.main >= 0 {
			m.toggleMain(cur.slot, cur.main, false)
		}
	case "d":
		if cur.main >= 0 {
			s, i := cur.slot, cur.main
			m.apply(func(st *store.State) {
				if s < len(st.Slots) {
					st.Slots[s].RemoveMain(i)
				}
			})
			m.busy = "removed"
		} else if len(m.st.Slots) > 1 {
			s := cur.slot
			m.apply(func(st *store.State) { st.RemoveSlot(s) })
			m.busy = "slot removed"
		} else {
			m.busy = "can't remove the last slot"
		}
	}
	return m, nil
}

func (m *model) toggleMain(slotIdx, mainIdx int, enable bool) {
	m.apply(func(st *store.State) {
		if slotIdx < 0 || slotIdx >= len(st.Slots) {
			return
		}
		sl := &st.Slots[slotIdx]
		if mainIdx < 0 || mainIdx >= len(sl.Mains) {
			return
		}
		if enable {
			sl.Mains[mainIdx].Enabled = !sl.Mains[mainIdx].Enabled
		} else {
			sl.Mains[mainIdx].AllowNoHop = !sl.Mains[mainIdx].AllowNoHop
		}
	})
	m.busy = "saved ✓"
}

func (m *model) toggleSlotEnabled(slotIdx int) {
	m.apply(func(st *store.State) {
		if slotIdx >= 0 && slotIdx < len(st.Slots) {
			st.Slots[slotIdx].Enabled = !st.Slots[slotIdx].Enabled
		}
	})
	m.busy = "saved ✓"
}

func (m *model) addMain(u string) {
	if _, err := xray.VlessToOutbound(u, "main"); err != nil {
		m.busy = "invalid vless:// URL"
		return
	}
	s := m.addMainSlot
	m.apply(func(st *store.State) {
		if s >= 0 && s < len(st.Slots) {
			st.Slots[s].AddMain(u)
		} else {
			st.AddMain(u)
		}
	})
	m.busy = "main added"
}

func (m *model) clampMainCursor() {
	n := len(m.mainRows())
	if m.mainCursor >= n {
		m.mainCursor = n - 1
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
	// Free-text (string) settings, rendered past the numeric fields.
	firstStr := len(cfgFields)
	for j, f := range cfgStrFields {
		idx := firstStr + j
		shown := f.get(m.st)
		if m.inputMode == inputEditStr && m.cfgCursor == idx {
			shown = m.inputBuf + "▏"
		} else if shown == "" {
			shown = f.empty
		}
		label := fmt.Sprintf("%-20s", f.label)
		if m.cfgCursor == idx {
			b.WriteString(activeStyle.Render(fmt.Sprintf("▸ %s %s", label, shown)) + dimStyle.Render("   "+f.note) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %s ", label) + dimStyle.Render(shown+"   "+f.note) + "\n")
		}
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
		if s.HopOnly {
			return warnStyle.Render("⚠ HOP-ONLY") + "  " + dimStyle.Render("main down — first hop "+mainLabel(s.Main)+" still up")
		}
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

func dnsModeLabel(v int) string {
	if v != 0 {
		return "real-net"
	}
	return "static routed"
}

var logLevels = []string{"none", "error", "warning", "info", "debug"}

func logLevelIdx(l string) int {
	for i, x := range logLevels {
		if x == l {
			return i
		}
	}
	return 2 // warning
}

func logLevelLabel(v int) string {
	if v >= 0 && v < len(logLevels) {
		return logLevels[v]
	}
	return "warning"
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
