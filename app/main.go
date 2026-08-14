// Command clashvless is the manager for a Happ-gated Remnawave subscription.
//
// This build implements the foundation: on-disk subscription STORAGE (stable
// device identity + cached nodes + a configurable main outbound), a
// Happ-mimicking FETCH, xray config assembly, and an in-process xray runner
// (embedded → single binary). The Bubble Tea TUI, live health probing and the
// failover supervisor land on top.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"clashvless/internal/control"
	"clashvless/internal/engine"
	"clashvless/internal/happ"
	"clashvless/internal/store"
	"clashvless/internal/tui"
	"clashvless/internal/tun"
	"clashvless/internal/xray"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var debug, cli bool
	var configPath string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--debug":
			debug = true
		case a == "--cli":
			cli = true
		case a == "--config":
			if i+1 >= len(args) {
				return errors.New("--config needs a path")
			}
			i++
			configPath = args[i]
		case strings.HasPrefix(a, "--config="):
			configPath = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	args = rest
	if len(configPath) >= 2 && configPath[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			configPath = home + configPath[1:]
		}
	}

	st, err := store.Load(configPath)
	if err != nil {
		return err
	}

	cmd := "tui" // bare `clashvless` launches the dashboard
	if len(args) > 0 {
		cmd = args[0]
	}
	if cli && (cmd == "tui" || cmd == "") {
		cmd = "run" // --cli forces headless mode
	}

	switch cmd {
	case "init", "add":
		if len(args) < 2 {
			return errors.New("usage: clashvless add <subscription-url>")
		}
		return cmdAdd(st, strings.TrimSpace(args[1]))

	case "subs":
		return cmdSubs(st)

	case "rm":
		if len(args) < 2 {
			return errors.New("usage: clashvless rm <sub-index>")
		}
		i, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid index %q", args[1])
		}
		st.RemoveSub(i)
		if err := st.Save(); err != nil {
			return err
		}
		return cmdSubs(st)

	case "main":
		return cmdMain(st, args[1:])

	case "whoami":
		printDevice(st)
		return nil

	case "fetch":
		return cmdFetch(st)

	case "fetch-proxy":
		return cmdFetchProxy(st, args[1:])

	case "loglevel", "log-level":
		return cmdLoglevel(st, args[1:])

	case "tun":
		return cmdTun(st, args[1:])

	case "list":
		nodes := st.ActiveNodes()
		if len(nodes) == 0 {
			fmt.Println("no cached nodes — add a sub and run `clashvless fetch`.")
			return nil
		}
		fmt.Printf("%d nodes across %d sub(s)\n", len(nodes), len(st.Subs))
		printNodes(nodes)
		return nil

	case "gen":
		return cmdGen(st, args[1:])

	case "up":
		return cmdUp(st, args[1:])

	case "run":
		return cmdRun(st, debug)

	case "version", "--version", "-v":
		fmt.Println("clashvless " + store.Version)
		return nil

	case "status":
		return cmdStatus(st)

	case "tui", "":
		return runTUI(st)

	default:
		fmt.Println("clashvless — Happ-gated subscription manager")
		fmt.Println("commands:")
		fmt.Println("  add <url>        add a subscription (fetches it)")
		fmt.Println("  subs             list subscriptions")
		fmt.Println("  rm <index>       remove a subscription")
		fmt.Println("  main                  list final-exit mains")
		fmt.Println("  main add <vless://>   add a main (Vision → direct/T1, plain → hop/T2)")
		fmt.Println("  main rm <index>       remove a main")
		fmt.Println("  fetch            refetch all subscriptions")
		fmt.Println("  fetch-proxy [addr|off]  fetch subs through a proxy: socks5://h:p, http://h:p, or h:p (socks5)")
		fmt.Println("  loglevel [none|error|warning|info|debug]  xray log verbosity")
		fmt.Println("  tun [on|off|status]   system-wide TUN capture (Linux/Windows; daemon must be root/admin)")
		fmt.Println("  tun dns <ip>|direct|tunnel   set the TUN resolver / route it off-tun or through the exit")
		fmt.Println("  list             show active nodes (two pools)")
		fmt.Println("  gen [entry]      print the xray config for main, optionally chained via a cached node")
		fmt.Println("  up [entry]       start xray in-process (chained via [entry] also exposes hop-1 on its own port)")
		fmt.Println("  run              run the failover engine headless (auto T1→T2→T3, keep-alive)")
		fmt.Println("  tui              launch the live dashboard (default with no args)")
		fmt.Println("  whoami           show this device's identity (what the panel sees)")
		fmt.Println("flags: --config <path> (alt state file/dir)   --cli (headless)   --debug (events.log)")
		return nil
	}
}

// cmdGen prints the xray config the engine would run for a tier: main alone
// (T1) or main dialed through a chosen entry node (T2/T3).
func cmdGen(st *store.State, args []string) error {
	mainOB, entryOB, label, err := tierConfig(st, args)
	if err != nil {
		return err
	}
	cfg, err := xray.BuildConfig(st.ListenPort, mainOB, entryOB, st.EntryListenPort(), st.Loglevel(), st.ListenHost())
	if err != nil {
		return err
	}
	fmt.Printf("# app → socks5://127.0.0.1:%d → [%s] → main\n", st.ListenPort, label)
	if entryOB != nil {
		fmt.Printf("# first hop → socks5://127.0.0.1:%d → [%s] (exits at the entry)\n", st.EntryListenPort(), label)
	}
	fmt.Println(redactSecrets(string(cfg)))
	return nil
}

// cmdUp starts xray in-process for the chosen tier and serves until interrupted.
func cmdUp(st *store.State, args []string) error {
	mainOB, entryOB, label, err := tierConfig(st, args)
	if err != nil {
		return err
	}
	cfg, err := xray.BuildConfig(st.ListenPort, mainOB, entryOB, st.EntryListenPort(), st.Loglevel(), st.ListenHost())
	if err != nil {
		return err
	}
	inst, err := engine.Start(cfg)
	if err != nil {
		return err
	}
	defer inst.Close()

	fmt.Printf("▶ xray up (in-process): socks5://127.0.0.1:%d → [%s] → main\n", st.ListenPort, label)
	if entryOB != nil {
		fmt.Printf("  first hop:              socks5://127.0.0.1:%d → [%s]\n", st.EntryListenPort(), label)
	}
	fmt.Println("  Ctrl-C to stop.")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\n■ stopped.")
	return nil
}

// cmdRun runs the failover supervisor: it keeps the single local port served by
// the best available tier and re-evaluates continuously (failover + recovery).
func cmdRun(st *store.State, debug bool) error {
	if len(st.DirectMains()) == 0 && len(st.HopMains()) == 0 {
		fmt.Println("note: no main yet — the daemon will idle (DOWN). Open the TUI to run setup: clashvless tui")
	}
	if len(st.ActiveNodes()) == 0 {
		fmt.Println("note: no cached nodes — only T1 (direct main) is possible until you add/fetch a sub.")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := control.NewHub()
	sink := func(l string) { fmt.Println("  ·", l) }
	if debug {
		sink = engine.EventFileSink(st.EventsLogPath(), sink)
		fmt.Println("debug: mirroring events →", st.EventsLogPath())
	}
	onLog := func(l string) { sink(l); hub.Broadcast(control.Event{Type: "log", Line: l}) }
	onStatus := func(s engine.Status) { printStatus(s); hub.Broadcast(control.Event{Type: "status", Status: &s}) }

	sup := engine.NewSupervisor(st, onStatus, onLog)
	sock := st.ControlSocketPath()
	go func() {
		if err := control.NewServer(sup, hub).Serve(ctx, sock); err != nil {
			fmt.Fprintln(os.Stderr, "control server:", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\n■ stopping.")
		cancel()
	}()

	fmt.Printf("▶ daemon: socks5://%s:%d · control %s — Ctrl-C to stop\n", st.ListenHost(), st.ListenPort, sock)
	return sup.Run(ctx)
}

// runTUI attaches the TUI to a running daemon. If none is running, it starts an
// in-process one for this session (so the TUI + its setup wizard work on a fresh
// install); that daemon stops when the TUI exits. For a persistent proxy, run the
// daemon separately with `clashvless run`.
func runTUI(st *store.State) error {
	sock := st.ControlSocketPath()
	// Attach to a standalone `run` daemon if its control socket is up; otherwise
	// start an in-process one for this session.
	if client, err := control.Dial(sock); err == nil {
		defer client.Close()
		return tui.RunClient(client, false)
	}
	stop := startEmbeddedDaemon(st)
	defer stop()
	client := dialRetry(sock, 5*time.Second)
	if client == nil {
		return fmt.Errorf("could not start an in-process daemon on %s", sock)
	}
	defer client.Close()
	return tui.RunClient(client, true)
}

// startEmbeddedDaemon runs the supervisor + control server in-process with silent
// sinks (output goes to the TUI over the socket, not stdout). Returns a stop func.
func startEmbeddedDaemon(st *store.State) func() {
	engine.SetLogSink(func(string) {}) // xray-core must not write to the terminal (TUI owns it)
	ctx, cancel := context.WithCancel(context.Background())
	hub := control.NewHub()
	onLog := func(l string) { hub.Broadcast(control.Event{Type: "log", Line: l}) }
	onStatus := func(s engine.Status) { hub.Broadcast(control.Event{Type: "status", Status: &s}) }
	sup := engine.NewSupervisor(st, onStatus, onLog)
	sup.SetQuiet(true)                    // xray must not write to the terminal — the TUI owns the screen
	_ = os.Remove(st.ControlSocketPath()) // clear any stale socket (no live daemon — Dial failed)
	go func() { _ = control.NewServer(sup, hub).Serve(ctx, st.ControlSocketPath()) }()
	go func() { _ = sup.Run(ctx) }()
	return cancel
}

// dialRetry polls the control socket until it accepts or the deadline passes.
func dialRetry(sock string, d time.Duration) *control.Client {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := control.Dial(sock); err == nil {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// cmdStatus prints one status snapshot from the running daemon.
func cmdStatus(st *store.State) error {
	client, err := control.Dial(st.ControlSocketPath())
	if err != nil {
		return fmt.Errorf("no daemon running — start it first: clashvless run")
	}
	defer client.Close()
	for e := range client.Events() {
		if e.Type == "status" && e.Status != nil {
			printStatus(*e.Status)
			return nil
		}
	}
	return nil
}

func printStatus(s engine.Status) {
	ts := s.UpdatedAt.Format("15:04:05")
	note := ""
	if s.Note != "" {
		note = "   " + s.Note
	}
	main := s.Main
	if main == "" {
		main = "main"
	}
	switch s.Tier {
	case 0:
		fmt.Printf("[%s] ✖ DOWN — %s%s\n", ts, s.Err, note)
	case 1:
		fmt.Printf("[%s] ● T1  direct → %-16s egress %dms%s\n", ts, trunc(main, 16), s.Egress.Milliseconds(), note)
	default:
		fmt.Printf("[%s] ● T%d  %s → %s   egress %dms%s\n", ts, s.Tier, s.Entry, trunc(main, 16), s.Egress.Milliseconds(), note)
	}
}

// tierConfig resolves a main outbound and (optionally) an entry outbound from the
// first arg (a cached-node name substring). No arg = direct via the first enabled
// w/o-hop main; with an entry = the first enabled hop-capable (non-Vision) main.
func tierConfig(st *store.State, args []string) (mainOB, entryOB json.RawMessage, label string, err error) {
	chained := len(args) >= 1 && strings.TrimSpace(args[0]) != ""
	mains := st.DirectMains()
	if chained {
		mains = st.HopMains()
	}
	if len(mains) == 0 {
		if chained {
			return nil, nil, "", errors.New("no enabled hop-capable (non-Vision) main — add one: clashvless main add <vless://...>")
		}
		return nil, nil, "", errors.New("no enabled direct (w/o-hop) main — add one: clashvless main add <vless://...>")
	}
	m := mains[0]
	label = "direct (T1) · " + m.Name
	if chained {
		n := findNode(st, args[0])
		if n == nil {
			return nil, nil, "", fmt.Errorf("no cached node matches %q (fetch first, or try a country/ОБХОД substring)", args[0])
		}
		entryOB = n.Outbound
		label = n.Name + " → " + m.Name
	}
	mainOB, err = xray.VlessToOutbound(m.URL, "main")
	if err != nil {
		return nil, nil, "", err
	}
	return mainOB, entryOB, label, nil
}

func findNode(st *store.State, q string) *store.Node {
	want := strings.ToLower(strings.TrimSpace(q))
	nodes := st.ActiveNodes()
	for i := range nodes {
		n := &nodes[i]
		if len(n.Outbound) > 0 && strings.Contains(strings.ToLower(n.Name), want) {
			return n
		}
	}
	return nil
}

// cmdAdd fetches a subscription URL and stores it as a new profile.
func cmdAdd(st *store.State, u string) error {
	if !happ.ValidSubURL(u) {
		return fmt.Errorf("invalid subscription URL (need http(s)://…): %q", u)
	}
	nodes, title, err := happ.Fetch(st.Device, u, st.FetchProxyAddr())
	st.AddSub(title, u)
	sub := &st.Subs[len(st.Subs)-1]
	if err == nil {
		sub.Nodes = nodes
		sub.LastFetch = time.Now()
	}
	if e := st.Save(); e != nil {
		return e
	}
	if err != nil {
		fmt.Printf("added %q (fetch failed: %v)\n", sub.Name, err)
	} else {
		fmt.Printf("added %q — %d nodes\n", sub.Name, len(nodes))
	}
	return nil
}

// cmdFetch refetches every subscription's nodes.
func cmdFetch(st *store.State) error {
	if len(st.Subs) == 0 {
		return errors.New("no subs — add one: clashvless add <url>")
	}
	for i := range st.Subs {
		nodes, title, err := happ.Fetch(st.Device, st.Subs[i].URL, st.FetchProxyAddr())
		if err != nil {
			fmt.Printf("  %-26s %v\n", st.Subs[i].Name, err)
			continue
		}
		st.Subs[i].Nodes = nodes
		st.Subs[i].LastFetch = time.Now()
		if title != "" {
			st.Subs[i].Name = title
		}
		fmt.Printf("  %-26s %d nodes\n", st.Subs[i].Name, len(nodes))
	}
	return st.Save()
}

// cmdLoglevel shows or sets the embedded xray log verbosity.
func cmdLoglevel(st *store.State, args []string) error {
	if len(args) == 0 {
		fmt.Println("log level:", st.Loglevel(), "  (none|error|warning|info|debug)")
		return nil
	}
	switch lvl := strings.ToLower(strings.TrimSpace(args[0])); lvl {
	case "none", "error", "warning", "info", "debug":
		st.LogLevel = lvl
	default:
		return fmt.Errorf("invalid level %q (use: none|error|warning|info|debug)", args[0])
	}
	if err := st.Save(); err != nil {
		return err
	}
	fmt.Println("log level:", st.Loglevel(), "— restart the daemon (or re-run `up`) to apply")
	return nil
}

// cmdFetchProxy shows or sets the SOCKS5 proxy used for subscription fetches.
// cmdTun toggles system-wide TUN capture. With a running daemon it patches it
// live (the supervisor brings the tunnel up/down next cycle); otherwise it saves
// the flag for the next run. TUN needs the daemon running as root/Administrator.
func cmdTun(st *store.State, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(strings.TrimSpace(args[0]))
	}
	// apply a config patch to the running daemon if one is up, else persist to disk.
	apply := func(patch map[string]any, set func(*store.State)) error {
		if client, err := control.Dial(st.ControlSocketPath()); err == nil {
			defer client.Close()
			b, _ := json.Marshal(patch)
			if err := client.SendPatch(b); err != nil {
				return err
			}
			_ = client.Send(control.Command{Cmd: "kick"})
			fmt.Println("  applied to the running daemon (toggle TUN off/on for it to take effect)")
			return nil
		}
		set(st)
		if err := st.Save(); err != nil {
			return err
		}
		fmt.Println("  saved; applies when the daemon starts")
		return nil
	}
	switch action {
	case "on", "enable", "1", "off", "disable", "0":
		want := action == "on" || action == "enable" || action == "1"
		if client, err := control.Dial(st.ControlSocketPath()); err == nil {
			defer client.Close()
			patch, _ := json.Marshal(map[string]any{"tun_enabled": want})
			if err := client.SendPatch(patch); err != nil {
				return err
			}
			_ = client.Send(control.Command{Cmd: "kick"})
			fmt.Printf("TUN %s — sent to the running daemon\n", onOff(want))
		} else {
			st.TunEnabled = want
			if err := st.Save(); err != nil {
				return err
			}
			fmt.Printf("TUN %s — saved; applies when the daemon starts\n", onOff(want))
		}
		if want {
			fmt.Println("  the daemon must run as root/Administrator (e.g. `sudo clashvless run`).")
			if !tun.Supported() {
				fmt.Println("  note: this OS build does not support TUN.")
			}
		}
		return nil
	case "dns":
		// tun dns <ip>            — set the resolver
		// tun dns direct|tunnel   — route it off-tun (breaks the bootstrap deadlock) or through the exit
		if len(args) < 2 {
			fmt.Printf("TUN DNS: %s  (%s)\n", st.TunResolver(), tunDNSModeLabel(st.TunDNSDirect))
			fmt.Println("usage: clashvless tun dns <ip>  |  tun dns direct|tunnel")
			return nil
		}
		switch v := strings.ToLower(strings.TrimSpace(args[1])); v {
		case "direct", "off-tun":
			fmt.Println("TUN DNS → DIRECT (resolves off-tun on the real network — breaks the domain-node deadlock)")
			return apply(map[string]any{"tun_dns_direct": true}, func(s *store.State) { s.TunDNSDirect = true })
		case "tunnel", "through", "via-tunnel":
			fmt.Println("TUN DNS → THROUGH TUNNEL (default; no DNS leak, needs the exit up)")
			return apply(map[string]any{"tun_dns_direct": false}, func(s *store.State) { s.TunDNSDirect = false })
		default:
			if net.ParseIP(v) == nil {
				return fmt.Errorf("not an IP: %q — use `tun dns <ip>` or `tun dns direct|tunnel`", args[1])
			}
			fmt.Printf("TUN DNS resolver → %s\n", v)
			return apply(map[string]any{"tun_dns": v}, func(s *store.State) { s.TunDNS = v })
		}
	case "status", "":
		name := st.TunName
		if name == "" {
			name = tun.DefaultName()
		}
		fmt.Printf("TUN mode: %s\n", onOff(st.TunEnabled))
		fmt.Printf("  os support: %v   this process privileged: %v\n", tun.Supported(), tun.Privileged())
		fmt.Printf("  device %s   addr %s   mtu %d\n", name, st.TunAddress(), st.TunMTUOr())
		fmt.Printf("  dns %s  (%s)\n", st.TunResolver(), tunDNSModeLabel(st.TunDNSDirect))
		return nil
	default:
		return fmt.Errorf("usage: clashvless tun [on|off|status|dns <ip>|dns direct|dns tunnel]")
	}
}

// tunDNSModeLabel describes how TUN DNS is routed.
func tunDNSModeLabel(direct bool) string {
	if direct {
		return "direct / off-tun"
	}
	return "through tunnel"
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func cmdFetchProxy(st *store.State, args []string) error {
	if len(args) == 0 {
		if st.FetchProxy == "" {
			fmt.Println("fetch proxy: (unset) — set with: clashvless fetch-proxy 127.0.0.1:2084")
			return nil
		}
		state := "off"
		if st.UseFetchProxy {
			state = "on"
		}
		fmt.Printf("fetch proxy: %s (%s)\n", st.FetchProxy, state)
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "off", "disable", "0":
		st.UseFetchProxy = false
	case "on", "enable", "1":
		st.UseFetchProxy = true
	default:
		st.FetchProxy = strings.TrimSpace(args[0])
		st.UseFetchProxy = true
	}
	if err := st.Save(); err != nil {
		return err
	}
	return cmdFetchProxy(st, nil)
}

// cmdSubs lists subscriptions and the current selection mode.
func cmdSubs(st *store.State) error {
	if len(st.Subs) == 0 {
		fmt.Println("no subscriptions. add one: clashvless add <url>")
		return nil
	}
	mode := "manual"
	if st.AutoSelect {
		mode = "auto (all subs aggregated)"
	}
	fmt.Printf("subscriptions (mode: %s):\n", mode)
	for i := range st.Subs {
		active := " "
		if !st.AutoSelect && i == st.ActiveSub {
			active = "►"
		}
		fmt.Printf("  %s [%d] %-26s %d nodes\n", active, i, st.Subs[i].Name, len(st.Subs[i].Nodes))
	}
	return nil
}

// cmdMain lists, adds, or removes final-exit mains.
func cmdMain(st *store.State, args []string) error {
	if len(args) == 0 {
		return listMains(st)
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New("usage: clashvless main add <vless://...>")
		}
		return addMainCLI(st, args[1])
	case "rm", "remove", "del":
		if len(args) < 2 {
			return errors.New("usage: clashvless main rm <index>")
		}
		i, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid index %q", args[1])
		}
		st.RemoveMain(i)
		if err := st.Save(); err != nil {
			return err
		}
		return listMains(st)
	default:
		return addMainCLI(st, args[0]) // bare `main <vless://>` = add
	}
}

func addMainCLI(st *store.State, u string) error {
	u = strings.TrimSpace(u)
	if _, err := xray.VlessToOutbound(u, "main"); err != nil {
		return fmt.Errorf("invalid vless url: %w", err)
	}
	st.AddMain(u)
	if err := st.Save(); err != nil {
		return err
	}
	fmt.Printf("✓ added — %s\n", mainModeHint(u))
	return listMains(st)
}

func mainModeHint(u string) string {
	switch {
	case store.IsVision(u):
		return "Vision → direct/T1 (w/o-hop on)"
	case store.IsReality(u):
		return "REALITY → direct/T1 (w/o-hop on)"
	default:
		return "plain → hop/T2 by default (toggle w/o-hop in the Main tab to allow direct)"
	}
}

func listMains(st *store.State) error {
	if len(st.Mains) == 0 {
		fmt.Println("no mains — add one: clashvless main add <vless://...>")
		return nil
	}
	fmt.Println("mains (final exits):")
	for i := range st.Mains {
		m := st.Mains[i]
		en := "✗"
		if m.Enabled {
			en = "✓"
		}
		mode := "hop-only"
		if m.AllowNoHop {
			mode = "direct-ok"
		}
		vis := ""
		if store.IsVision(m.URL) {
			vis = "  [vision]"
		}
		fmt.Printf("  [%d] %s enabled  %-26s  %s%s\n", i, en, trunc(m.Name, 26), mode, vis)
	}
	return nil
}

func printDevice(st *store.State) {
	d := st.Device
	fmt.Println("device identity (this is the single slot we occupy):")
	fmt.Printf("  label (panel)  : %s / %s\n", d.OS, d.Model)
	fmt.Printf("  user-agent     : %s\n", d.UA)
	fmt.Printf("  hwid           : %s\n", d.HWID)
	fmt.Printf("  stored at      : %s\n", st.FilePath())
}

// printNodes lists nodes split into the two pools this subscription uses:
// regular country exits vs ОБХОД/bypass (whitelist) nodes.
func printNodes(nodes []store.Node) {
	var reg, wl []store.Node
	for _, n := range nodes {
		if n.Whitelist {
			wl = append(wl, n)
		} else {
			reg = append(reg, n)
		}
	}
	fmt.Printf("\n── regular / exit pool (non-whitelist): %d ──\n", len(reg))
	for _, n := range reg {
		fmt.Printf("  • %-34s %s:%d\n", trunc(n.Name, 34), n.Server, n.Port)
	}
	fmt.Printf("\n── bypass pool (ОБХОД / whitelist): %d ──\n", len(wl))
	for _, n := range wl {
		fmt.Printf("  • %-34s %s:%d\n", trunc(n.Name, 34), n.Server, n.Port)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

var secretRe = regexp.MustCompile(`("(?:id|publicKey|shortId|password)"\s*:\s*")[^"]*`)

func redactSecrets(s string) string {
	return secretRe.ReplaceAllString(s, `${1}<redacted>`)
}
