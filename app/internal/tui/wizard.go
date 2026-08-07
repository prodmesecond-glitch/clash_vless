package tui

import (
	"fmt"
	"strings"

	"clashvless/internal/control"
	"clashvless/internal/happ"
	"clashvless/internal/store"
	"clashvless/internal/xray"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// First-run setup wizard. It walks a fresh install through: add a subscription →
// fetch (optionally via a proxy) → review nodes → add an exit (main), warning
// loudly if that exit isn't plain (plain is the only kind that hops freely).
const (
	wizOff = iota
	wizWelcome
	wizSub      // text: subscription URL
	wizProxyAsk // choice: fetch via a proxy?
	wizProxyIn  // text: proxy host:port
	wizHwid     // choice: keep / generate / custom HWID
	wizHwidIn   // text: custom HWID
	wizFetch    // waiting for the fetch
	wizReview   // choice: continue / go back
	wizMain     // text: exit (main) URL
	wizWarn     // choice: exit isn't plain — keep / change
	wizDone
)

// maybeStartWizard opens the wizard once, when the daemon first syncs an empty
// config (no subs and no mains).
func (m *model) maybeStartWizard() {
	if m.wizSeen || m.wiz != wizOff || m.st == nil {
		return
	}
	if len(m.st.Subs) == 0 && len(m.st.Mains) == 0 {
		m.wiz, m.wizSeen = wizWelcome, true
	}
}

// wizAdvanceFetch moves from the fetch step to review once the sub we asked for
// has arrived with nodes. A 0-node / error result stays put (the view shows why).
func (m *model) wizAdvanceFetch() {
	if m.wiz != wizFetch || m.st == nil {
		return
	}
	if sb := m.subByURL(m.wizSubURL); sb != nil && len(sb.Nodes) > 0 {
		m.wiz, m.wizErr = wizReview, ""
	}
}

func (m *model) subByURL(u string) *store.Subscription {
	for i := range m.st.Subs {
		if m.st.Subs[i].URL == u {
			return &m.st.Subs[i]
		}
	}
	return nil
}

func (m *model) handleWizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.wiz {
	case wizWelcome:
		switch msg.String() {
		case "y", "Y", "enter":
			m.wiz, m.inputBuf, m.wizErr = wizSub, "", ""
		case "n", "N", "esc":
			m.wiz = wizOff
		}
	case wizSub:
		if done, esc, val := m.wizText(msg); esc {
			m.wiz, m.wizErr = wizWelcome, ""
		} else if done {
			u := sanitizeURL(val)
			if !happ.ValidSubURL(u) {
				m.wizErr = "need an http(s):// subscription URL"
			} else {
				m.wizSubURL, m.wizErr, m.inputBuf = u, "", ""
				m.wiz = wizProxyAsk
			}
		}
	case wizProxyAsk:
		switch msg.String() {
		case "y", "Y":
			m.wiz, m.inputBuf, m.wizErr = wizProxyIn, m.st.FetchProxy, ""
		case "n", "N", "enter":
			m.wiz = wizHwid
		case "esc":
			m.wiz = wizSub
		}
	case wizProxyIn:
		if done, esc, val := m.wizText(msg); esc {
			m.wiz = wizProxyAsk
		} else if done {
			hp := strings.TrimSpace(val)
			m.apply(func(st *store.State) { st.FetchProxy = hp; st.UseFetchProxy = hp != "" })
			m.wiz = wizHwid
		}
	case wizHwid:
		switch msg.String() {
		case "k", "K", "enter":
			m.startWizFetch()
		case "g", "G":
			m.apply(func(st *store.State) { st.Device.HWID = store.NewHWID() })
			m.startWizFetch()
		case "c", "C":
			m.wiz, m.inputBuf, m.wizErr = wizHwidIn, "", "" // start empty — user pastes/types their own
		case "esc":
			m.wiz = wizProxyAsk
		}
	case wizHwidIn:
		if done, esc, val := m.wizText(msg); esc {
			m.wiz, m.wizErr = wizHwid, ""
		} else if done {
			h := strings.TrimSpace(val)
			if !store.ValidHWID(h) {
				m.wizErr = "HWID must be 10–64 chars: letters, digits, = or -"
			} else {
				m.apply(func(st *store.State) { st.Device.HWID = h })
				m.wizErr = ""
				m.startWizFetch()
			}
		}
	case wizFetch:
		switch msg.String() {
		case "r", "R":
			m.startWizFetch()
		case "b", "B", "esc":
			m.wiz = wizProxyAsk
		}
	case wizReview:
		switch msg.String() {
		case "c", "C", "enter":
			m.wiz, m.inputBuf, m.wizErr = wizMain, "", ""
		case "b", "B", "esc":
			m.removeSubByURL(m.wizSubURL)
			m.wiz = wizSub
		}
	case wizMain:
		if done, esc, val := m.wizText(msg); esc {
			m.wiz = wizReview
		} else if done {
			u := sanitizeURL(val)
			if _, err := xray.VlessToOutbound(u, "main"); err != nil {
				m.wizErr = "not a valid vless:// exit URL"
			} else {
				m.wizMainURL, m.wizErr = u, ""
				m.addMain(u)
				if store.IsPlain(u) {
					m.wiz = wizDone
				} else {
					m.wiz = wizWarn
				}
			}
		}
	case wizWarn:
		switch msg.String() {
		case "c", "C", "enter":
			m.wiz = wizDone
		case "b", "B", "esc":
			m.removeMainByURL(m.wizMainURL)
			m.wiz, m.inputBuf, m.wizErr = wizMain, "", ""
		}
	case wizDone:
		m.wiz, m.tab, m.busy = wizOff, tabStatus, ""
	}
	return m, nil
}

// startWizFetch (re)fetches the chosen sub. It drops any prior copy first so a
// retry doesn't stack duplicates.
func (m *model) startWizFetch() {
	m.wiz, m.wizErr = wizFetch, ""
	delete(m.subErr, m.wizSubURL) // clear a prior failure so the view shows "fetching…"
	m.removeSubByURL(m.wizSubURL)
	_ = m.client.Send(control.Command{Cmd: "addsub", URL: m.wizSubURL})
}

// wizText feeds a keypress into inputBuf; returns (submitted, escaped, value).
func (m *model) wizText(msg tea.KeyMsg) (submitted, escaped bool, value string) {
	switch msg.Type {
	case tea.KeyEsc:
		return false, true, ""
	case tea.KeyEnter:
		return true, false, m.inputBuf
	case tea.KeyBackspace, tea.KeyDelete:
		if r := []rune(m.inputBuf); len(r) > 0 {
			m.inputBuf = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			if r >= 0x20 && r != 0x7f {
				m.inputBuf += string(r)
			}
		}
	}
	return false, false, ""
}

func (m *model) removeSubByURL(u string) {
	for i := range m.st.Subs {
		if m.st.Subs[i].URL == u {
			_ = m.client.Send(control.Command{Cmd: "rmsub", Index: i})
			return
		}
	}
}

func (m *model) removeMainByURL(u string) {
	for i := range m.st.Mains {
		if m.st.Mains[i].URL == u {
			m.apply(func(st *store.State) { st.RemoveMain(i) })
			return
		}
	}
}

// --- view --------------------------------------------------------------------

func (m *model) wizardView() string {
	var body string
	switch m.wiz {
	case wizWelcome:
		body = "Welcome to clash_vless.\n\nRun the guided setup? It adds your subscription, fetches\nnodes, and sets your exit — no shell commands needed.\n\n  " + key("y") + " yes    " + key("n") + " skip"
	case wizSub:
		body = step(1) + "Add your subscription (the hop / entry nodes).\n\nPaste the sub URL:\n\n  " + m.inputField() + "\n\n  " + key("Enter") + " add    " + key("Esc") + " back"
	case wizProxyAsk:
		body = "Fetch through a proxy?\n" + dimStyle.Render("(useful if the subscription is geo-blocked where you are)") + "\n\n  " + key("y") + " yes, set one    " + key("n") + " no proxy"
	case wizProxyIn:
		body = "Proxy to fetch through:\n" + dimStyle.Render("socks5://host:port · http://host:port · or host:port (socks5)") + "\n\n  " + m.inputField() + "\n\n  " + key("Enter") + " set    " + key("Esc") + " back"
	case wizHwid:
		body = "Device HWID " + dimStyle.Render("— sent when fetching; occupies one panel device slot.") + "\n" +
			dimStyle.Render("current: ") + hwidShort(m.st.Device.HWID) + "\n\n  " +
			key("k") + " keep    " + key("g") + " generate new    " + key("c") + " enter a custom one"
	case wizHwidIn:
		body = "Enter HWID " + dimStyle.Render("(10–64 chars: a–z A–Z 0–9 = - · paste one from another device to reuse its slot)") + ":\n\n  " + m.inputField() + "\n\n  " + key("Enter") + " set & fetch    " + key("Esc") + " back"
	case wizFetch:
		switch {
		case m.subErr[m.wizSubURL] != "":
			body = badStyle.Render("fetch failed:") + "\n  " + m.subErr[m.wizSubURL] + "\n\n  " + key("r") + " retry    " + key("b") + " back"
		case m.subByURL(m.wizSubURL) != nil: // sub arrived but empty, no logged error
			body = badStyle.Render("fetched 0 nodes — the sub is empty or the URL is wrong") + "\n\n  " + key("r") + " retry    " + key("b") + " back"
		default:
			body = "Fetching nodes…" + dimStyle.Render("  (a few seconds)")
		}
	case wizReview:
		body = step(2) + "Fetched:\n\n" + m.wizSubSummary() + "\n\n  " + key("c") + " continue → add exit    " + key("b") + " change sub"
	case wizMain:
		body = step(3) + "Add your exit — the final " + dimStyle.Render("main") + ".\n\nPaste the " + dimStyle.Render("vless://") + " exit URL:\n\n  " + m.inputField() + "\n\n  " + key("Enter") + " add    " + key("Esc") + " back"
	case wizWarn:
		body = warnStyle.Render("ⓘ  heads up — this exit isn't “plain”") + "\n\n" +
			"Fine to use directly. Just know hopping will be limited:\n" +
			dimStyle.Render("  • a reality / xhttp exit only hops through a non-Vision entry\n  • a Vision exit is direct-only (T1)") + "\n\n" +
			dimStyle.Render("a plain (security=none) exit hops through anything — see the README matrix.") + "\n\n  " +
			key("c") + " keep it    " + key("b") + " use a different exit"
	case wizDone:
		body = okStyle.Render("✓ Setup complete!") + "\n\n" + m.wizSummary() + "\n\n  " + dimStyle.Render("press any key to open the dashboard")
	}
	box := wizBoxStyle.Render(titleStyle.Render(" ⚙  first-time setup ") + "\n\n" + body)
	hint := key("q") + dimStyle.Render(" quit")
	if m.inTextInput() {
		hint = dimStyle.Render("ctrl+c to quit")
	}
	if m.width < 4 || m.height < 4 {
		return "\n" + box + "\n " + hint + "\n"
	}
	// box centered in the screen minus one row; the quit hint sits in that last
	// row at the bottom-left — outside the box.
	return lipgloss.Place(m.width, m.height-1, lipgloss.Center, lipgloss.Center, box) + "\n " + hint
}

func hwidShort(h string) string {
	if len(h) <= 14 {
		return h
	}
	return h[:8] + "…" + h[len(h)-4:]
}

// confirmExitView is the "exit? y/n" prompt, shown over whatever's on screen.
func (m *model) confirmExitView() string {
	note := "\n" + okStyle.Render("  daemon mode — the proxy keeps running after you quit")
	if m.embedded {
		note = "\n" + dimStyle.Render("  embedded mode — quitting also stops the proxy")
	}
	box := wizBoxStyle.Render(titleStyle.Render(" exit? ") + "\n\n  Quit clash_vless?" + note + "\n\n  " + key("y") + " yes    " + key("n") + " no")
	if m.width < 4 || m.height < 4 {
		return "\n" + box + "\n"
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m *model) inputField() string {
	return keyStyle.Render("› ") + m.inputBuf + dimStyle.Render("▌")
}

func (m *model) wizSubSummary() string {
	for i := range m.st.Subs {
		if m.st.Subs[i].URL != m.wizSubURL {
			continue
		}
		sb := &m.st.Subs[i]
		s := okStyle.Render(fmt.Sprintf("  %s", trunc(sb.Name, 32))) + dimStyle.Render(fmt.Sprintf("  (%d nodes)", len(sb.Nodes)))
		for j := range sb.Nodes {
			if j >= 5 {
				s += dimStyle.Render(fmt.Sprintf("\n    …and %d more", len(sb.Nodes)-5))
				break
			}
			s += "\n    • " + trunc(sb.Nodes[j].Name, 34)
		}
		return s
	}
	return dimStyle.Render("  (sub not found)")
}

func (m *model) wizSummary() string {
	nnode := 0
	for i := range m.st.Subs {
		nnode += len(m.st.Subs[i].Nodes)
	}
	exit, tag := "—", ""
	if len(m.st.Mains) > 0 {
		last := m.st.Mains[len(m.st.Mains)-1]
		exit = trunc(last.Name, 28)
		if store.IsPlain(last.URL) {
			tag = okStyle.Render(" (plain ✓)")
		} else {
			tag = badStyle.Render(" (not plain — limited hopping)")
		}
	}
	return fmt.Sprintf("  subs: %d  ·  nodes: %d\n  exit: %s%s", len(m.st.Subs), nnode, exit, tag)
}

func step(n int) string  { return sectionStyle.Render(fmt.Sprintf("Step %d/3 — ", n)) }
func key(k string) string { return keyStyle.Render("[" + k + "]") }
