// Command clashvless is the manager for a Happ-gated Remnawave subscription.
//
// It includes subscription storage, a Happ-mimicking fetcher, an embedded xray
// runner, tiered failover, and graphical/terminal dashboards over one control API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on DefaultServeMux (gated by CLASHVLESS_PPROF)
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"clashvless/internal/control"
	"clashvless/internal/crashlog"
	"clashvless/internal/engine"
	appgui "clashvless/internal/gui"
	"clashvless/internal/happ"
	"clashvless/internal/loghub"
	"clashvless/internal/share"
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

	if len(args) > 0 && args[0] == "share" { // hidden: the Android app's export and link parser, JSON on stdin
		return share.Serve(os.Stdin, os.Stdout)
	}

	st, err := store.Load(configPath)
	if err != nil {
		return err
	}
	platformInit(st)

	cmd := "gui" // bare `clashvless` launches the graphical dashboard
	if len(args) > 0 {
		cmd = args[0]
	}
	if cli && (cmd == "gui" || cmd == "tui" || cmd == "") {
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
		if err := commit(st, "subs", "active_sub"); err != nil {
			return err
		}
		return cmdSubs(st)

	case "main":
		return cmdMain(st, args[1:])

	case "slot", "slots":
		return cmdSlot(st, args[1:])

	case "whoami":
		printDevice(st)
		return nil

	case "hwid":
		return cmdHWID(st, args[1:])

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

	case "gui", "":
		return runGUI(st, len(args) == 0)

	case "tui":
		return runTUI(st)

	default:
		fmt.Println("clashvless — Happ-gated subscription manager")
		fmt.Println("commands:")
		fmt.Println("  add <url>        add a subscription (fetches it)")
		fmt.Println("  subs             list subscriptions")
		fmt.Println("  rm <index>       remove a subscription")
		fmt.Println("  main                  list slots + their final-exit mains")
		fmt.Println("  main add <url|proxy> [slot] [--proto http|socks5]  add an exit (VLESS or HTTP/SOCKS5)")
		fmt.Println("  main rm <index> [slot]       remove a main")
		fmt.Println("  main move <from> <to> [slot] reorder mains (index 0 = tried first)")
		fmt.Println("  slot                  list slots (failover pools, each on its own port)")
		fmt.Println("  slot add [name] [--type vless|proxy] · rename <name> <new> · rm <name> · port <name> <port> · tun <name> · enable|disable <name>")
		fmt.Println("  fetch            refetch all subscriptions")
		fmt.Println("  fetch-proxy [addr|off]  fetch subs through a proxy: socks5://h:p, http://h:p, or h:p (socks5)")
		fmt.Println("  loglevel [none|error|warning|info|debug]  xray log verbosity")
		fmt.Println("  tun [on|off|status]   system-wide TUN capture (Linux/Windows; daemon must be root/admin)")
		fmt.Println("  tun dns real-net|static|<ip>|auto   DNS mode (real-net = your LAN resolver off-tun · static routed = via the exit) / set the static resolver")
		fmt.Println("  tun lan bypass|tunnel   keep private/LAN ranges off the tunnel (default, local-only reachable) or capture them")
		fmt.Println("  tun ipv6 block|allow    block global IPv6 (default — no v6 leak past the IPv4-only tunnel) or let it escape")
		fmt.Println("  tun icmp on|off         answer ping locally under TUN (default on; device liveness, not end-to-end)")
		fmt.Println("  list             show active nodes (two pools)")
		fmt.Println("  gen [entry]      print the xray config for main, optionally chained via a cached node")
		fmt.Println("  up [entry]       start xray in-process (chained via [entry] also exposes hop-1 on its own port)")
		fmt.Println("  run              run the failover engine headless (auto T1→T2→T3, keep-alive)")
		fmt.Println("  gui              launch the graphical dashboard (default with no args)")
		fmt.Println("  tui              launch the terminal dashboard")
		fmt.Println("  whoami           show this device's identity (what the panel sees)")
		fmt.Println("  hwid [<id>|new]  set the device HWID (paste another device's to share its panel slot) or mint a new one")
		fmt.Println("flags: --config <path> (alt state file/dir)   --cli (headless)   --debug (events.log)")
		return nil
	}
}

func cmdHWID(st *store.State, args []string) error {
	if len(args) == 0 {
		fmt.Println(st.Device.HWID)
		return nil
	}
	h := strings.TrimSpace(args[0])
	if h == "new" {
		h = store.NewHWID()
	} else if !store.ValidHWID(h) {
		return errors.New("HWID must be 10–64 chars: a–z A–Z 0–9 = -")
	}
	st.Device.HWID = h
	if err := commit(st, "device"); err != nil {
		return err
	}
	printDevice(st)
	return nil
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
	if err := platformTunFd(st); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Opt-in heap/goroutine profiling for leak hunts. Bind localhost only.
	if addr := os.Getenv("CLASHVLESS_PPROF"); addr != "" {
		go func() {
			fmt.Println("pprof: http://" + addr + "/debug/pprof/")
			if err := http.ListenAndServe(addr, nil); err != nil {
				fmt.Fprintln(os.Stderr, "pprof server:", err)
			}
		}()
	}

	hub := control.NewHub()
	// Debug records list every destination visited, so stdout (often a journal) skips
	// them unless asked for; clients still receive them for their own opt-in filter.
	showDebug := st.Loglevel() == "debug"
	sink := func(record loghub.Record) {
		if record.Level != loghub.Debug || showDebug {
			fmt.Println(record.String())
		}
	}
	if debug {
		sink = engine.EventFileSink(st.EventsLogPath(), sink)
		fmt.Println("debug: mirroring events →", st.EventsLogPath())
	}
	onLog := func(record loghub.Record) { sink(record); hub.Broadcast(control.LogEvent(record)) }
	onStatus := func(s engine.Status) { printStatus(s); hub.Broadcast(control.Event{Type: "status", Status: &s}) }

	sup := engine.NewSupervisor(st, onStatus, onLog)
	engine.SetLogSink(onLog)
	if !st.AnyUsableMain() {
		sup.Logf(loghub.Warning, "config", "routes", "no usable exit; add or enable one in Exit routes")
	}
	if len(st.ActiveNodes()) == 0 {
		sup.Logf(loghub.Warning, "config", "subscriptions", "no cached entry nodes; only direct exits are possible")
	}
	sock := st.ControlSocketPath()
	if ln, err := control.Listen(sock); err != nil {
		sup.Logf(loghub.Error, "control", "server", "%v", err)
	} else {
		go control.NewServer(sup, hub).Serve(ctx, ln)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		sup.Logf(loghub.Info, "daemon", "lifecycle", "stopping")
		cancel()
	}()

	sup.Logf(loghub.Info, "daemon", "lifecycle", "started; SOCKS5 %s port(s) %s; control %s", st.ListenHost(), slotPorts(st), sock)
	return sup.Run(ctx)
}

// runGUI opens the dashboard. A bare launch (no command) falls back to the TUI where
// no GUI can run; an explicit `gui` reports why instead.
func runGUI(st *store.State, bare bool) (err error) {
	crashPath := filepath.Join(filepath.Dir(st.FilePath()), "crash.log")
	if stop, err := crashlog.Enable(crashPath); err == nil {
		defer stop()
	}
	if err := appgui.Preflight(); err != nil {
		if bare && errors.Is(err, appgui.ErrUnavailable) && hasConsole() {
			return runTUI(st)
		}
		return err
	}
	defer func() {
		// The toolkit panics when it can't open a display (e.g. a dead X forward).
		if r := recover(); r != nil {
			crashlog.Recovered(crashPath, r)
			err = fmt.Errorf("could not open the GUI (%v) — use `clashvless tui`", r)
		}
	}()
	sock := st.ControlSocketPath()
	embedded := false
	client, err := control.Dial(sock)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("a daemon is already running on %s but owned by another user (root?) — attach as that user or restart the daemon", sock)
		}
		stop, err := startEmbeddedDaemon(st)
		if err != nil {
			return fmt.Errorf("could not start an in-process daemon on %s: %w", sock, err)
		}
		defer stop()
		embedded = true
		client, err = control.Dial(sock)
		if err != nil {
			return fmt.Errorf("could not attach to the in-process daemon on %s: %w", sock, err)
		}
	}
	defer client.Close()
	return appgui.RunClient(client, embedded)
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
	} else if errors.Is(err, os.ErrPermission) {
		// A daemon IS running but its socket is owned by another user (a root/TUN
		// daemon started without sudo's SUDO_UID). Do NOT fall through to the
		// embedded path — it would delete a live daemon's socket. Guide instead.
		return fmt.Errorf("a daemon is already running on %s but owned by another user (root?) —\n"+
			"  attach it as that user (`sudo clashvless`), or restart the daemon so it hands the\n"+
			"  socket back to you (running it via `sudo clashvless run …` does this from v0.14.1)", sock)
	}
	stop, err := startEmbeddedDaemon(st)
	if err != nil {
		return fmt.Errorf("could not start an in-process daemon on %s: %w", sock, err)
	}
	defer stop()
	client, err := control.Dial(sock)
	if err != nil {
		return fmt.Errorf("could not attach to the in-process daemon on %s: %w", sock, err)
	}
	defer client.Close()
	return tui.RunClient(client, true)
}

// startEmbeddedDaemon routes all engine and xray records to the attached UI,
// keeping them away from stdout even when the host is a terminal dashboard. The
// socket is bound before it returns, so the caller can dial straight away.
func startEmbeddedDaemon(st *store.State) (func(), error) {
	ln, err := control.Listen(st.ControlSocketPath())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	hub := control.NewHub()
	onLog := func(record loghub.Record) { hub.Broadcast(control.LogEvent(record)) }
	onStatus := func(s engine.Status) { hub.Broadcast(control.Event{Type: "status", Status: &s}) }
	sup := engine.NewSupervisor(st, onStatus, onLog)
	engine.SetLogSink(onLog)
	go control.NewServer(sup, hub).Serve(ctx, ln)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := sup.Run(ctx); err != nil {
			sup.Logf(loghub.Error, "daemon", "lifecycle", "%v", err)
		}
	}()
	sup.Logf(loghub.Info, "daemon", "lifecycle", "embedded engine started; SOCKS5 %s port(s) %s", st.ListenHost(), slotPorts(st))
	return func() {
		cancel()
		// Exiting before Run returns would leave TUN routes and DNS behind.
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
		}
	}, nil
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
	mainOB, err = xray.ExitToOutbound(m.URL, "main")
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
	if e := commit(st, "subs", "active_sub"); e != nil {
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
	return commit(st, "subs")
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
	if err := commit(st, "log_level"); err != nil {
		return err
	}
	fmt.Println("log level:", st.Loglevel(), "— applies as each route is rebuilt (or re-run `up`)")
	return nil
}

// slotPorts lists the SOCKS ports of the enabled slots, which is what the daemon serves.
func slotPorts(st *store.State) string {
	var ports []string
	for _, sl := range st.Slots {
		if sl.Enabled {
			ports = append(ports, strconv.Itoa(sl.Port))
		}
	}
	if len(ports) == 0 {
		return "none (no enabled slot)"
	}
	return strings.Join(ports, ", ")
}

// commit saves a CLI edit. A running daemon keeps its own copy of the state and
// would overwrite a direct file write on its next save, so the named fields go
// to it as a patch instead.
func commit(st *store.State, keys ...string) error {
	client, err := control.Dial(st.ControlSocketPath())
	if err != nil {
		return st.Save()
	}
	defer client.Close()
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	patch := map[string]json.RawMessage{}
	for _, key := range keys {
		value, ok := fields[key]
		if !ok {
			value = json.RawMessage("null") // an omitted empty field
		}
		patch[key] = value
	}
	data, _ = json.Marshal(patch)
	return client.SendAndWait(control.Command{Cmd: "patch", Patch: data}, 5*time.Second)
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
			if err := client.SendAndWait(control.Command{Cmd: "patch", Patch: b}, 5*time.Second); err != nil {
				return err
			}
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
			if err := client.SendAndWait(control.Command{Cmd: "patch", Patch: patch}, 5*time.Second); err != nil {
				return err
			}
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
		// tun dns real-net|static  — DNS mode (resolve on the real net vs routed through the exit)
		// tun dns <ip>             — set the static-routed resolver
		// tun dns auto             — reset it to 8.8.8.8
		if len(args) < 2 {
			fmt.Printf("TUN DNS: %s\n", tunDNSDisplay(st))
			fmt.Println("usage: clashvless tun dns real-net | static | <ip> | auto")
			return nil
		}
		switch v := strings.ToLower(strings.TrimSpace(args[1])); v {
		case "real-net", "realnet", "real", "direct", "off-tun", "lan":
			fmt.Println("TUN DNS → real-net (resolves off-tun on your real network — breaks the domain-node deadlock)")
			return apply(map[string]any{"tun_dns_direct": true}, func(s *store.State) { s.TunDNSDirect = true })
		case "static", "static-routed", "routed", "tunnel", "through":
			fmt.Println("TUN DNS → static routed (through the exit to the static resolver; no DNS leak)")
			return apply(map[string]any{"tun_dns_direct": false}, func(s *store.State) { s.TunDNSDirect = false })
		case "auto", "default":
			fmt.Println("TUN static-routed resolver → 8.8.8.8 (default)")
			return apply(map[string]any{"tun_static_dns": ""}, func(s *store.State) { s.TunStaticDNS = "" })
		default:
			if net.ParseIP(v) == nil {
				return fmt.Errorf("not an IP or mode: %q — use `tun dns real-net|static`, `tun dns <ip>`, or `tun dns auto`", args[1])
			}
			fmt.Printf("TUN static-routed resolver → %s\n", v)
			return apply(map[string]any{"tun_static_dns": v}, func(s *store.State) { s.TunStaticDNS = v })
		}
	case "lan":
		// tun lan bypass|tunnel — keep private/LAN ranges off the tunnel (default) or capture them
		if len(args) < 2 {
			fmt.Printf("TUN LAN: %s\n", tunLANLabel(st.TunBypassLAN()))
			fmt.Println("usage: clashvless tun lan bypass | tunnel")
			return nil
		}
		switch v := strings.ToLower(strings.TrimSpace(args[1])); v {
		case "bypass", "direct", "off-tun", "on":
			fmt.Println("TUN LAN → BYPASS (private/LAN ranges kept off-tun — local-only resources reachable)")
			return apply(map[string]any{"tun_tunnel_lan": false}, func(s *store.State) { s.TunTunnelLAN = false })
		case "tunnel", "capture", "through", "off":
			fmt.Println("TUN LAN → TUNNEL (private ranges routed through the exit too — LAN unreachable via it)")
			return apply(map[string]any{"tun_tunnel_lan": true}, func(s *store.State) { s.TunTunnelLAN = true })
		default:
			return fmt.Errorf("usage: clashvless tun lan bypass|tunnel")
		}
	case "ipv6", "ip6", "v6":
		// tun ipv6 block|allow — block global-unicast IPv6 (default) or let it escape
		if len(args) < 2 {
			fmt.Printf("TUN IPv6: %s\n", tunIPv6Label(st.TunBlockIPv6()))
			fmt.Println("usage: clashvless tun ipv6 block | allow")
			return nil
		}
		switch v := strings.ToLower(strings.TrimSpace(args[1])); v {
		case "block", "off", "on", "no":
			fmt.Println("TUN IPv6 → BLOCK (global v6 fast-fails so apps fall back to the tunneled v4 — no leak)")
			return apply(map[string]any{"tun_allow_ipv6": false}, func(s *store.State) { s.TunAllowIPv6 = false })
		case "allow", "on-net", "leak", "yes":
			fmt.Println("TUN IPv6 → ALLOW (v6 escapes on the real network — the IPv4-only tunnel does NOT cover it)")
			return apply(map[string]any{"tun_allow_ipv6": true}, func(s *store.State) { s.TunAllowIPv6 = true })
		default:
			return fmt.Errorf("usage: clashvless tun ipv6 block|allow")
		}
	case "icmp", "ping":
		// tun icmp on|off — answer ICMP echo locally (default) so `ping` works
		if len(args) < 2 {
			fmt.Printf("TUN ICMP: %s\n", tunICMPLabel(st.TunICMP()))
			fmt.Println("usage: clashvless tun icmp on | off")
			return nil
		}
		switch v := strings.ToLower(strings.TrimSpace(args[1])); v {
		case "on", "yes", "reply", "enable":
			fmt.Println("TUN ICMP → ON (tun stack answers ping locally — device liveness, not end-to-end; use curl for the exit)")
			return apply(map[string]any{"tun_no_icmp": false}, func(s *store.State) { s.TunNoICMP = false })
		case "off", "no", "drop", "disable":
			fmt.Println("TUN ICMP → OFF (ping left unanswered under TUN, as before)")
			return apply(map[string]any{"tun_no_icmp": true}, func(s *store.State) { s.TunNoICMP = true })
		default:
			return fmt.Errorf("usage: clashvless tun icmp on|off")
		}
	case "status", "":
		name := st.TunName
		if name == "" {
			name = tun.DefaultName()
		}
		fmt.Printf("TUN mode: %s\n", onOff(st.TunEnabled))
		fmt.Printf("  os support: %v   this process privileged: %v\n", tun.Supported(), tun.Privileged())
		if ts := st.TunSlotName(); ts != "" {
			port := 0
			if sl := st.SlotByName(ts); sl != nil {
				port = sl.Port
			}
			fmt.Printf("  slot %q (:%d)   — the exit TUN routes through\n", ts, port)
		} else {
			fmt.Printf("  slot (none — enable a slot in the Main tab)\n")
		}
		fmt.Printf("  device %s   addr %s   mtu %d\n", name, st.TunAddress(), st.TunMTUOr())
		fmt.Printf("  dns %s\n", tunDNSDisplay(st))
		fmt.Printf("  lan %s\n", tunLANLabel(st.TunBypassLAN()))
		fmt.Printf("  ipv6 %s\n", tunIPv6Label(st.TunBlockIPv6()))
		fmt.Printf("  icmp %s\n", tunICMPLabel(st.TunICMP()))
		return nil
	default:
		return fmt.Errorf("usage: clashvless tun [on|off|status|dns real-net|static|<ip>|auto|lan bypass|tunnel|ipv6 block|allow|icmp on|off]")
	}
}

// tunIPv6Label describes whether global-unicast IPv6 is blocked while TUN is up.
func tunIPv6Label(block bool) string {
	if block {
		return "block (global v6 fast-fails → apps fall back to the tunneled v4; no leak)"
	}
	return "allow (v6 escapes on the real network — NOT covered by the IPv4-only tunnel)"
}

// tunICMPLabel describes whether the tun stack answers ICMP echo (ping).
func tunICMPLabel(on bool) string {
	if on {
		return "on (tun stack answers ping locally — device liveness only, not end-to-end; use curl for the exit)"
	}
	return "off (ping unanswered under TUN — ICMP can't traverse the SOCKS/VLESS tunnel)"
}

// tunLANLabel describes whether private/LAN ranges bypass the tunnel.
func tunLANLabel(bypass bool) string {
	if bypass {
		return "bypass (private/LAN kept off-tun — local-only reachable)"
	}
	return "tunnel (private ranges captured — LAN unreachable via exit)"
}

// tunDNSDisplay is the DNS mode + effective resolver, for `tun status` and
// `tun dns`. Mirrors tun.ResolverFor's choice.
func tunDNSDisplay(st *store.State) string {
	if st.TunDNSDirect {
		r := tun.ResolverFor(true, "")
		if r == "" {
			return "real-net (no system resolver detected — set `tun dns <ip>` or use static)"
		}
		return "real-net → " + r + " (your current resolver, off-tun)"
	}
	return "static routed → " + st.TunStaticResolver() + " (via the exit)"
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
	if err := commit(st, "fetch_proxy", "use_fetch_proxy"); err != nil {
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

// cmdMain lists mains across slots, or adds/removes a main in a slot (default =
// first slot; `main add <url> [slot]` targets a named slot).
func cmdMain(st *store.State, args []string) error {
	if len(args) == 0 {
		return listMains(st)
	}
	switch args[0] {
	case "add":
		var pos []string
		proto := ""
		for i := 1; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--proto" || a == "-p":
				if i+1 >= len(args) {
					return errors.New("--proto requires http or socks5")
				}
				i++
				proto = args[i]
			case strings.HasPrefix(a, "--proto="):
				proto = strings.TrimPrefix(a, "--proto=")
			default:
				pos = append(pos, a)
			}
		}
		if len(pos) < 1 {
			return errors.New("usage: clashvless main add <vless://...|proxy> [slot] [--proto http|socks5]")
		}
		return addMainCLI(st, pos[0], argOr(pos, 1, ""), proto)
	case "rm", "remove", "del":
		if len(args) < 2 {
			return errors.New("usage: clashvless main rm <index> [slot]")
		}
		i, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid index %q", args[1])
		}
		sl, err := slotArg(st, argOr(args, 2, ""))
		if err != nil {
			return err
		}
		if i < 0 || i >= len(sl.Mains) {
			return fmt.Errorf("no main %d in slot %q", i, sl.Name)
		}
		sl.RemoveMain(i)
		if err := commit(st, "slots"); err != nil {
			return err
		}
		return listMains(st)
	case "move", "mv":
		if len(args) < 3 {
			return errors.New("usage: clashvless main move <from> <to> [slot]   (index 0 = highest priority)")
		}
		from, err1 := strconv.Atoi(args[1])
		to, err2 := strconv.Atoi(args[2])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("invalid index in %q %q", args[1], args[2])
		}
		sl, err := slotArg(st, argOr(args, 3, ""))
		if err != nil {
			return err
		}
		if !sl.MoveMain(from, to) {
			return fmt.Errorf("can't move main %d to %d in slot %q (it has %d)", from, to, sl.Name, len(sl.Mains))
		}
		if err := commit(st, "slots"); err != nil {
			return err
		}
		return listMains(st)
	default:
		return addMainCLI(st, args[0], "", "") // bare `main <url>` = add to first slot
	}
}

func addMainCLI(st *store.State, raw, slot, defaultProto string) error {
	raw = strings.TrimSpace(raw)
	sl, err := slotArg(st, slot)
	if err != nil {
		return err
	}
	u := raw
	if sl.IsProxy() {
		if strings.HasPrefix(strings.ToLower(raw), "vless://") {
			return fmt.Errorf("slot %q is an HTTP/SOCKS proxy slot (cannot add a vless:// exit)", sl.Name)
		}
		spec, err := xray.ParseProxy(raw, defaultProto)
		if err != nil {
			return fmt.Errorf("invalid proxy exit: %w", err)
		}
		formatted, err := xray.FormatProxyURL(spec)
		if err != nil {
			return err
		}
		u = formatted
	} else {
		if store.IsProxyURL(raw) {
			return fmt.Errorf("slot %q is a VLESS slot — create an HTTP/SOCKS slot first: clashvless slot add --type proxy", sl.Name)
		}
		if _, err := xray.VlessToOutbound(raw, "main"); err != nil {
			return fmt.Errorf("invalid vless url: %w", err)
		}
	}
	if sl.HasMain(u) {
		return fmt.Errorf("exit already present in slot %q", sl.Name)
	}
	sl.AddMain(u)
	if err := commit(st, "slots"); err != nil {
		return err
	}
	fmt.Printf("✓ added to slot %q — %s\n", sl.Name, mainModeHint(u))
	if !sl.IsProxy() && looksCutOff(u) {
		fmt.Println("⚠ no type= or security= in this URL, so it was saved as plain TCP. If your shell cut it at an unquoted &, it also lost the slot argument: remove it and re-add with the URL in quotes.")
	}
	return listMains(st)
}

// looksCutOff spots the stub left when a shell splits an unquoted URL at its first &.
func looksCutOff(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	q := p.Query()
	return !q.Has("type") && !q.Has("security")
}

func argOr(args []string, i int, def string) string {
	if i < len(args) {
		return args[i]
	}
	return def
}

// slotArg resolves a slot by name or index; "" selects the first slot.
func slotArg(st *store.State, s string) (*store.Slot, error) {
	if len(st.Slots) == 0 {
		st.DefaultSlot()
	}
	if s == "" {
		return &st.Slots[0], nil
	}
	if i := st.SlotIndexByName(s); i >= 0 {
		return &st.Slots[i], nil
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n < len(st.Slots) {
		return &st.Slots[n], nil
	}
	return nil, fmt.Errorf("no slot %q (see `clashvless slot`)", s)
}

// cmdSlot manages slots — failover pools, each served on its own SOCKS port.
func cmdSlot(st *store.State, args []string) error {
	if len(args) == 0 {
		return listMains(st)
	}
	switch args[0] {
	case "add":
		var name, slotType string
		for i := 1; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--type" || a == "-t":
				if i+1 >= len(args) {
					return errors.New("--type requires vless or proxy")
				}
				i++
				slotType = args[i]
			case strings.HasPrefix(a, "--type="):
				slotType = strings.TrimPrefix(a, "--type=")
			case name == "":
				name = a
			}
		}
		kind, ok := store.NormalizeSlotType(slotType)
		if !ok {
			return fmt.Errorf("invalid slot type %q (use vless or proxy)", slotType)
		}
		if strings.Contains(name, "://") {
			return errors.New("slot name cannot contain ://")
		}
		sl := st.AddSlotOfType(name, kind)
		if err := commit(st, "slots", "tun_slot"); err != nil {
			return err
		}
		if sl.IsProxy() {
			fmt.Printf("✓ slot %q (http/socks) added on :%d\n", sl.Name, sl.Port)
		} else {
			fmt.Printf("✓ slot %q added on :%d\n", sl.Name, sl.Port)
		}
		return listMains(st)
	case "rename":
		if len(args) < 3 {
			return errors.New("usage: clashvless slot rename <name|index> <new-name>")
		}
		i, err := slotIndexArg(st, args[1])
		if err != nil {
			return err
		}
		oldName := st.Slots[i].Name
		newName := args[2]
		if err := st.RenameSlot(i, newName); err != nil {
			return err
		}
		if err := commit(st, "slots", "tun_slot"); err != nil {
			return err
		}
		fmt.Printf("✓ slot %q renamed to %q\n", oldName, newName)
		return listMains(st)
	case "rm", "remove", "del":
		if len(args) < 2 {
			return errors.New("usage: clashvless slot rm <name|index>")
		}
		if len(st.Slots) <= 1 {
			return errors.New("can't remove the last slot")
		}
		i, err := slotIndexArg(st, args[1])
		if err != nil {
			return err
		}
		name := st.Slots[i].Name
		st.RemoveSlot(i)
		if err := commit(st, "slots", "tun_slot"); err != nil {
			return err
		}
		fmt.Printf("✓ slot %q removed\n", name)
		return listMains(st)
	case "port":
		if len(args) < 3 {
			return errors.New("usage: clashvless slot port <name> <port>")
		}
		sl, err := slotArg(st, args[1])
		if err != nil {
			return err
		}
		p, err := strconv.Atoi(args[2])
		if err != nil || p < 1024 || p > 65534 {
			return fmt.Errorf("invalid port %q (1024–65534)", args[2])
		}
		if st.SlotPortConflict(st.SlotIndexByName(sl.Name), p) {
			return fmt.Errorf("port %d or its first-hop port %d is already used by another slot", p, p+1)
		}
		sl.Port = p
		if err := commit(st, "slots", "tun_slot"); err != nil {
			return err
		}
		fmt.Printf("✓ slot %q → :%d\n", sl.Name, p)
		return listMains(st)
	case "tun":
		if len(args) < 2 {
			return errors.New("usage: clashvless slot tun <name>")
		}
		sl, err := slotArg(st, args[1])
		if err != nil {
			return err
		}
		st.TunSlot = sl.Name
		if err := commit(st, "slots", "tun_slot"); err != nil {
			return err
		}
		fmt.Printf("✓ TUN slot → %q (:%d)\n", sl.Name, sl.Port)
		return listMains(st)
	case "enable", "disable":
		if len(args) < 2 {
			return fmt.Errorf("usage: clashvless slot %s <name>", args[0])
		}
		sl, err := slotArg(st, args[1])
		if err != nil {
			return err
		}
		sl.Enabled = args[0] == "enable"
		if err := commit(st, "slots", "tun_slot"); err != nil {
			return err
		}
		return listMains(st)
	default:
		return fmt.Errorf("usage: clashvless slot [add|rm|port|tun|enable|disable] …")
	}
}

func slotIndexArg(st *store.State, s string) (int, error) {
	if i := st.SlotIndexByName(s); i >= 0 {
		return i, nil
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n < len(st.Slots) {
		return n, nil
	}
	return -1, fmt.Errorf("no slot %q", s)
}

func mainModeHint(u string) string {
	switch {
	case store.IsProxyURL(u):
		return "HTTP/SOCKS5 → direct/T1 + hop/T2-T3 (w/o-hop on)"
	case store.IsVision(u):
		return "Vision → direct/T1 (w/o-hop on)"
	case store.IsReality(u):
		return "REALITY → direct/T1 (w/o-hop on)"
	default:
		return "plain → hop/T2 by default (toggle w/o-hop in the Main tab to allow direct)"
	}
}

func listMains(st *store.State) error {
	tun := st.TunSlotName()
	fmt.Println("slots (failover pools · ◉ = TUN slot):")
	for si := range st.Slots {
		sl := &st.Slots[si]
		mark := " "
		if sl.Name == tun {
			mark = "◉"
		}
		state := "off"
		if sl.Enabled {
			state = "on"
		}
		kind := "vless"
		if sl.IsProxy() {
			kind = "http/socks"
		}
		fmt.Printf("%s slot %q  :%d  [%s]  [%s]\n", mark, sl.Name, sl.Port, kind, state)
		if len(sl.Mains) == 0 {
			if sl.IsProxy() {
				fmt.Printf("    (no exits — clashvless main add <socks5://user:pass@host:port|host:port:user:pass> %s)\n", sl.Name)
			} else {
				fmt.Printf("    (no mains — clashvless main add <vless://...> %s)\n", sl.Name)
			}
			continue
		}
		for i := range sl.Mains {
			m := sl.Mains[i]
			en := "✗"
			if m.Enabled {
				en = "✓"
			}
			mode := "hop-only"
			if m.AllowNoHop {
				mode = "direct-ok"
			}
			tag := ""
			if p := store.ProtoTag(m.URL); p != "" {
				tag = "  [" + p + "]"
			}
			fmt.Printf("    [%d] %s %-26s  %s%s\n", i, en, trunc(m.Name, 26), mode, tag)
		}
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

var secretRe = regexp.MustCompile(`("(?:id|publicKey|shortId|password|pass)"\s*:\s*")[^"]*`)

func redactSecrets(s string) string {
	return secretRe.ReplaceAllString(s, `${1}<redacted>`)
}
