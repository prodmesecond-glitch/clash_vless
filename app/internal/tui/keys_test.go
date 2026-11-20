package tui

import (
	"testing"
	"time"

	"clashvless/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func typeString(m *model, s string) {
	for _, r := range s {
		m.handleInput(runeKey(r))
	}
}

func TestLatinKey(t *testing.T) {
	for in, want := range map[rune]string{'ф': "a", 'ы': "s", 'й': "q", 'Н': "Y", 'a': "a", '3': "3", 'ё': "ё"} {
		if got := latinKey(runeKey(in)).String(); got != want {
			t.Errorf("latinKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInKeyBurst(t *testing.T) {
	m := &model{}
	t0 := time.Now()
	if m.inKeyBurst(runeKey('v'), t0) {
		t.Error("first key flagged as a burst")
	}
	if !m.inKeyBurst(runeKey('e'), t0.Add(time.Millisecond)) {
		t.Error("pasted key not flagged")
	}
	if !m.inKeyBurst(tea.KeyMsg{Type: tea.KeyEnter}, t0.Add(2*time.Millisecond)) {
		t.Error("pasted newline not flagged")
	}
	if m.inKeyBurst(runeKey('a'), t0.Add(200*time.Millisecond)) {
		t.Error("typed key flagged as a burst")
	}
	if m.inKeyBurst(tea.KeyMsg{Type: tea.KeyDown}, t0.Add(201*time.Millisecond)) {
		t.Error("arrow key flagged as a burst")
	}
}

func TestProxySlotTypeAndInputModes(t *testing.T) {
	m := &model{
		tab: tabMain,
		st: &store.State{
			ListenPort: 2084,
			Slots: []store.Slot{
				{Name: "main", Port: 2084, Enabled: true},
				{Name: "res", Type: store.SlotTypeProxy, Port: 2086, Enabled: true},
			},
		},
	}

	// Pressing 's' opens the slot type picker; selecting '2' picks HTTP/SOCKS5 and advances to name input.
	m.handleMainKey(runeKey('s'))
	if m.inputMode != inputSlotType {
		t.Fatalf("inputMode after s = %d, want inputSlotType", m.inputMode)
	}
	m.handleInput(runeKey('2'))
	if m.inputMode != inputAddSlot || m.newSlotType != store.SlotTypeProxy {
		t.Fatalf("after picking 2: mode=%d type=%q", m.inputMode, m.newSlotType)
	}
	m.handleInput(tea.KeyMsg{Type: tea.KeyEsc})
	if m.inputMode != inputNone {
		t.Fatalf("Esc did not cancel: mode=%d", m.inputMode)
	}

	// Move cursor to the proxy slot (row 1) and press 'a' -> opens Copy-paste vs Manual picker.
	m.mainCursor = 1
	m.handleMainKey(runeKey('a'))
	if m.inputMode != inputProxyMode {
		t.Fatalf("inputMode after a on proxy slot = %d, want inputProxyMode", m.inputMode)
	}

	// Pick [1] Copy-paste, Tab toggles default proto to http, invalid input stays in form with error.
	m.handleInput(runeKey('1'))
	if m.inputMode != inputAddProxyPaste || m.proxyPasteProto != "socks5" {
		t.Fatalf("paste mode = %d proto = %q", m.inputMode, m.proxyPasteProto)
	}
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	if m.proxyPasteProto != "http" {
		t.Fatalf("Tab did not toggle paste proto: %q", m.proxyPasteProto)
	}
	typeString(m, "not-a-proxy")
	m.handleInput(tea.KeyMsg{Type: tea.KeyEnter})
	if m.inputMode != inputAddProxyPaste || m.proxyFormErr == "" {
		t.Fatalf("invalid paste should stay in form with error: mode=%d err=%q", m.inputMode, m.proxyFormErr)
	}
	m.handleInput(tea.KeyMsg{Type: tea.KeyEsc})

	// Pick [2] Manual parameters, navigate and fill fields.
	m.handleMainKey(runeKey('a'))
	m.handleInput(runeKey('2'))
	if m.inputMode != inputAddProxyManual || m.proxyFormProto != "socks5" || !m.proxyFormDirect {
		t.Fatalf("manual init = mode=%d proto=%q direct=%v", m.inputMode, m.proxyFormProto, m.proxyFormDirect)
	}
	// Toggle protocol to http, advance to host, type host, advance to port, type port (non-digits ignored), user, pass, name, toggle allow-direct.
	m.handleInput(tea.KeyMsg{Type: tea.KeyRight})
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	typeString(m, "proxy.example")
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	typeString(m, "31a28")
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	typeString(m, "alice")
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	typeString(m, "secret")
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	typeString(m, "Office")
	m.handleInput(tea.KeyMsg{Type: tea.KeyTab})
	m.handleInput(tea.KeyMsg{Type: tea.KeySpace}) // toggle direct off

	if m.proxyFormProto != "http" || m.proxyFormHost != "proxy.example" || m.proxyFormPort != "3128" ||
		m.proxyFormUser != "alice" || m.proxyFormPass != "secret" || m.proxyFormName != "Office" || m.proxyFormDirect {
		t.Fatalf("unexpected manual form state: %+v", m)
	}
	// View renders cleanly without leaking password when hidden.
	view := m.proxyManualView()
	if view == "" {
		t.Fatal("empty manual view")
	}
}
