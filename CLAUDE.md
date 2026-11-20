# clash_vless — project guide for Claude

`clashvless` is a single-binary Go CLI/TUI that manages a **Happ-gated Remnawave
subscription** and keeps the best working exit online — as a local SOCKS5 proxy
or a **system-wide TUN** — with automatic tiered failover.

## Layout
- `app/` — the Go module (`module clashvless`, Go 1.26). All source lives here.
  - `main.go` — CLI entrypoint + command dispatch.
  - `branding/` — shared transparent vector/raster logo, icons, and color tokens. The Windows ICO contains
    seven raster sizes; `app/icon_windows_{amd64,386}.syso` embeds it and the `requireAdministrator`
    manifest into Windows x64 and x86 executables (both GUI and CLI request elevation).
  - `internal/store` — on-disk state (subs, cached nodes, **slots** = per-port failover pools of mains,
    stable device identity, tunables).
  - `internal/gui` — native Fyne desktop GUI. `render()` rebuilds the page on every status event, so it restores the
    scroll offset and the focused `textInput`; anything typed must be stored by its `changed` callback or it's lost.
    `connectionSummary` says "Connected" only when `Status.TunUp` is true: a working SOCKS route says nothing about the VPN. Its connection control is a clickable, pointer-cursor switch with the
    same track/thumb shape and state colors as Android, rather than a drag-only slider. Activity (`activity.go`) fills the
    page height (no outer scroll): a toolbar with text/module filters (` & ` AND, ` | ` OR) and severity toggles that
    show per-level counts, then a virtualized 2000-record list drawn in columns (`logRow`: time, level badge, origin,
    message; warnings amber). Arriving records redraw the list in batches (`addLog` → `flushLogs`, at most every `logFlushGap`); one
    redraw per record kept the UI thread busy under traffic (~70% of a core vs ~8% batched, measured with a curl flood).
    Rows never measure text prefixes, which is uncached shaping: the message overflows and the list clips it, and only an
    overflowing origin is ellipsized. `loghub.Filter.Allows` formats a record only when a search query needs it.
    Clicking a record opens a details strip with the full message and Copy. Rows shift as
    records arrive, so the list unselects at once and the strip holds the choice. "Copy shown" copies the filtered
    records. Errors arriving off the Activity page raise a red count badge on its sidebar item (`addLog`,
    `countBadge`). Settings (gear icon) sits at the bottom of the sidebar, above the engine/version footer.
    Exit routes keeps its boxed Routing compatibility note in the fixed header below Recheck,
    with a small danger-colored bordered `!`; the theme hover color is a darker shade of mint.
    Modal forms pause page rebuilding while state/status events continue updating the model;
    closing a form renders the latest state. Adding an exit retains the target slot's name
    across state updates and safely rejects a removed slot. GUI panics are saved to `crash.log`
    beside `state.json`, including fatal background-goroutine panics in windowed builds.
    Single-line ellipsized display labels collapse whitespace, including Unicode paragraph
    separators: Fyne's truncator can otherwise panic when shaping stops at a paragraph boundary.
    Stored exit names and VLESS URLs remain unchanged.
    Exit routes uses ellipsized labels for slot titles, live route summaries, and server addresses,
    so long names or VLESS text cannot increase the window's minimum width after a connection.
    Navigation inside Exit routes uses a scrollable tabs bar placed to the right of "Add slot",
    showing only the selected slot where the first slot used to sit; the VPN slot is highlighted
    with a confirm icon and "· VPN" label, while the active tab uses HighImportance styling, and
    unselected non-VPN tabs sit on a rounded rectangle background. Adding a slot immediately selects
    and opens the new slot without dropping back to the first tab. Slots can be renamed via an edit icon placed to the right of the slot name
    (with hover tooltip and pointer cursor), removing the standalone button; renaming validates for unique,
    non-empty names and keeps `tun_slot` in sync. Slot enable is designed as a slider switch placed right
    after the rename icon (`<rename icon> | <Enabled/Disabled> <slider>`), and a delete icon sits in the top
    right corner of the slot box triggering the slot delete confirmation.
    Icon actions are `dashboard.iconButton` (`tooltipIconButton` with a `Hint`); hints float in one
    `tooltipLayer` stacked over the window. It holds only canvas objects, which are never hit targets, so it
    can't steal clicks or hover, and showing a hint doesn't shift the row. A slot is two cards spaced apart, with no
    separator lines: the slot card (title row, a status strip, actions) and a "Final exits" card where each exit is
    its own bordered card (accent border when in use). Shared `store.MainGroups` groups VLESS links differing only in
    server hostname/IP and fragment into one final-exit card; UUID, port and all query parameters must match.
    Query order/escaping is normalized without resolving DNS. Both addresses remain separate `Slot.Mains`
    candidates for the engine's direct/hopped failover and TUN bypass; existing state needs no migration.
    GUI cards and engine candidates use the same group priority: all eligible addresses of the first group
    are tried before the next group within each tier, even if stored addresses are interleaved. Grouping
    precedes eligibility filtering, so disabling the leading address does not lower its fallback's priority.
    The card lists all addresses, exports every link and applies rename/delete/toggles/priority to the group;
    mixed address flags appear indeterminate until changed. Group edits capture URL occurrence counts,
    update every duplicate once, and reject a changed group membership (including duplicate counts);
    reordering moves the group together, preserving its address order. Text under names is kept to one line of short tags (`labels.go`):
    a slot shows `slotLiveLine` (a colored state tag such as "T1 Direct · 87 ms", "First hop only", "Down", then the
    route or error) with Port/Hop chips on the right, inside a `tonedBox` tinted by the state; an exit shows its name, then chips for "In use", protocol and
    `mainUsage` ("Direct or hop", "Hop only", red "Unused: Vision needs direct"), then the host. Names and hosts
    use `fitText`, which ellipsizes to the available width. Each exit row shows its priority number,
    raise/lower arrows (reorder = priority), share (QR), rename, and delete.
    Exit edits go through `mainRef` (slot name + group URLs), so a confirmation or rename finishing after another client
    reordered the exits still hits the right one. "Add final exits" takes several links, one per line, or a base64
    subscription; `Slot.HasMain` skips exits already present, ignoring the `#fragment`, which is only a name (an
    exported link carries the shown name, so re-importing a renamed exit used to duplicate it). **Export** (`share.go`)
    is Marzban-style: a QR per link (`rsc.io/qr`, drawn black-on-white with a
    quiet zone), Copy link, and for lists Copy all links / Copy as subscription (base64). It exists per exit, per slot,
    per entry node, per provider, and for Hardware ID (in Settings -> Device). Nodes without a `raw` link go through `xray.OutboundToVLESS`, and any setting a
    link can't carry (e.g. `finalmask`) is named in the dialog. Links, QR encoding, and pasted-link parsing live in
    `internal/share`, shared with Android. Status events rebuild the page at most every
    `statusRenderGap`, and only when the part the page shows changed (`statusKey`). A rebuild between press and
    release drops the click. Settings fields keep unsaved text in `drafts` across rebuilds and validate before
    saving (IP, IPv4 CIDR, MTU 576–9000, first-hop port collisions). The "Listen port" row is gone: slot ports are
    edited per slot. Activity has a "Follow new" toggle.
    **Overview** (`overview.go`) is grouped into cards (`section`: title, subtitle, header action). A "Finish setup"
    checklist appears while no usable exit exists. Connection shows the headline, a plain-words hint
    (`connectionHint`), the route line, Latency / Route type / Slots serving, and a VPN error box (`alertBox`).
    Local endpoints lists each enabled slot with its live state and Copy / Copy hop. The copied address is dialable
    (`connectHost`: a wildcard bind becomes `127.0.0.1`). Entry pools are side-by-side cards with an online meter, the
    average failure rate, the fastest node, and the entry in use (`entriesInUse`).
    **Subscriptions** (`subscriptions.go`): provider rows carry an Active/Not used/Fetch failed tag, country/bypass
    counts, online count, and a relative fetch time (`ago`). The node list is grouped by provider (`nodeGroups`; probes matched by
    name and pool, leftovers under "Other nodes"). Each group folds from its header (`clickable`). Unused providers
    start folded and list their nodes unchecked, without Pin. Folding is kept per provider URL in `collapsedSubs`.
    It filters as you type and with "Online only",
    by hiding rows in place without rebuilding the page. It sorts by `sortProbes`: Recommended, which mirrors the
    engine's online → stability band → throughput → ping order, then latency, name, or provider order. Rows show
    In use / Pinned / Testing speed beside the name, then latency / Mbps / failure-rate tags, and a pinned node gets an Unpin banner.
    Names next to tags use `titleWithTags`, which shrinks only the name.
  - `internal/share` — exits and entry nodes as VLESS links (`MainLink` puts the shown name in the fragment,
    `NodeLink`), HWID export, QR codes, base64 subscriptions, and pasted-exit parsing (`ParseExits`). The hidden `share` command
    (handled in `main.go` before `store.Load`, so it never touches state) reads a JSON `Request` on stdin and writes
    links with QR module strings, or parsed exits; the Android app runs it as a child process (`Exports.java`).
  - `internal/loghub` — structured records (timestamp, severity, module, element, message) shared by CLI, TUI,
    desktop, and Android. `control.Event.Record` carries structured fields while `Line` preserves compatibility.
    Both embedded and standalone daemons route xray-core through the same hub; GUI mode no longer discards xray logs.
  - `internal/happ` — fetches a Remnawave sub (Happ 3.x mimic; optional SOCKS5 fetch proxy); parses
    base64 / URI-list / xray-JSON, building a `vless://` outbound for URI-list nodes so they're usable.
  - `internal/xray` — `vless://` → xray outbound, and full-config assembly (incl. entry→main chaining).
    `OutboundToVLESS` (`share.go`) is the inverse for export: vnext or flat vless settings, reality/tls,
    tcp/ws/grpc/xhttp. It returns the settings a link drops. Round-trip tested against `VlessToOutbound`.
  - `internal/engine` — embeds xray-core in-process. `Supervisor` is now a **manager** of one
    `slotRunner` per enabled slot (each = a failover pool on its own port) + the **shared** real-egress
    pool probe, speed loop, and TUN bridge; `publish()` aggregates every runner's `SlotStatus` into `Status`.
  - `internal/control` — daemon↔client IPC over a Unix socket: `run` serves it; `tui`/`status` attach.
    An **elevated daemon** (`sudo … run`, or a **systemd unit**, both needed for TUN) hands the control
    socket **and** the state file back to the human user it serves via `store.ReownToInvoker`, so a
    **rootless `clashvless`** attaches instead of falling back to an embedded engine. Owner resolution is
    `store.invokerIDs` (`reown_unix.go`, no-op stub on Windows): prefer `$SUDO_UID/$SUDO_GID` (set by
    `sudo`), else — under systemd, which sets **no** `SUDO_*` — walk up from the target path to the nearest
    existing ancestor owned by a non-root user (the config lives under the user's home) and chown to it.
    Without this, a systemd-run root daemon left `control.sock` root-owned and every rootless attach got
    `EACCES`. And `runTUI` no longer deletes a socket it only got `EACCES` on (that used to clobber a live
    root daemon) — it errors with guidance. **Bind before dialing** (`control.Listen`, then `Server.Serve(ctx, ln)`):
    on Windows a `connect()` to a missing AF_UNIX path **creates a 0-byte file there**, and `bind()` then fails with
    "address in use", so the old dial-retry racing `Remove`+`Listen` kept the embedded TUI from starting. A client
    dialing with no daemon (e.g. `status`) leaves that file behind; `Listen` clears it.
    A **patch replaces each top-level field it names** (`applyPatch`); a malformed patch is logged and dropped.
    Unmarshalling into the live state used to merge: an array element that omitted an `omitempty` field
    (a node without `raw`, a sub without `fetch_err`) kept whatever the old element at that index had. So
    removing a subscription from the TUI grafted the removed sub's `raw` links onto the next one. `unsubscribe`
    closes the client's channel, because its writer goroutine leaked on every disconnect. `Serve` backs off and
    keeps accepting on transient errors such as EMFILE. CLI edits (`add`, `rm`, `fetch`, `main …`, `slot …`,
    `hwid`, `fetch-proxy`, `loglevel`) go to a running daemon as patches (`commit` in `main.go`). Before this,
    they wrote `state.json` behind the daemon, which overwrote them on its next save.
  - `internal/tui` — Bubble Tea dashboard client (Status / Subs / Main / Log / Config); `wizard.go`
    is the first-run setup wizard (sub → proxy → HWID → fetch → review → exit, with a non-plain warning).
    The **Main** tab is now a **slots** view: each slot shows its port, on/off, TUN marker (◉) and live tier,
    with its mains nested; keys `s` add slot · `a` add main · `p` port · `t` TUN slot · `e` enable · `w` w/o-hop
    · `d` delete. Mains & sub nodes show a proto tag (`store.ProtoTag`/`OutboundProtoTag`:
    plain·reality·vision·grpc·ws·tls·xhttp). The Config tab has a **TUN mode** toggle.
    Outside text input, `handleKey` maps Russian-layout runes to their Latin shortcuts (`latinKey`: `ф` → `a`) and
    drops key bursts (`inKeyBurst`, gaps under `keyBurstGap`). Windows delivers a paste key by key with no paste
    marker, so a `vless://` pasted outside text input used to run as shortcuts: `e` toggled the slot off and `s`
    opened "add slot", naming a junk slot after the rest of the URL. Slot names containing `://` are also rejected.
    In text input, Ctrl+V and Insert read the clipboard (`readClipboard`, Windows only): the classic console delivers
    those keys instead of pasting. Debug records list every destination visited, so the Log tab and `run`'s stdout hide
    them unless `log_level` is `debug`; GUI clients keep them behind their own DEBUG filter. Fetch failures come from
    `Subscription.FetchErr` (`fetch_err`, token-free), set by the daemon and shown by all three clients.
  - `internal/tun` — TUN mode OS layer (Linux `ip`/`resolvectl`, Windows `netsh`/`route`, macOS
    `route`/`networksetup`; stub on other OSes). Moves the default route onto the device and keeps **xray's
    own sockets off the tunnel** so the live exit AND the failover probes reach servers directly — on Linux
    via **fwmark policy routing** (`tun.FwMark`, a private table for marked traffic), on Windows/macOS via
    per-server bypass routes. Sets exit-routed DNS; `Down()` restores everything. Needs root/admin. Device
    name default is OS-specific (`tun.DefaultName()`: `clashvless0` on Linux/Windows, `utunN` on macOS —
    xray requires that form and assigns the utun's address itself). Forwards TCP/UDP only; **ICMP can't
    traverse the SOCKS/VLESS tunnel** — but the tun netstack now answers `ping` locally (see **ICMP ping**
    below), so `ping` works as a device-liveness check. It does NOT prove the exit is reachable — use `curl`
    for end-to-end.
  - `cmd/xhserver` — local test rig: Vision-reality hop (`:9001`) + xhttp-reality exit (`:9002`) +
    plain-reality (non-Vision) hop (`:9003`), to point a client config at (direct T1 / hopped T2).
    Separate `main` (pulls in vless/inbound); not in the app binary.
  - `cmd/xhprobe` — self-contained chaining-mechanism probe: stands up exit+hops and dials them
    via every method (`proxySettings` vs `sockopt.dialerProxy`, xhttp/tcp × Vision/non-Vision),
    printing PASS/FAIL. Proves why chaining uses dialerProxy; re-run after any xray-core bump.
  - `cmd/tunprobe` — root/admin-only lab: starts JUST the TUN bridge, confirms xray creates the device,
    then tears down **without touching routing** (safe). Answers "does the native tun inbound work here?".
- `app/vendor/` — vendored deps (committed; builds are hermetic/offline). To add a dependency without losing the
  patches: `go get`, import it, `go mod vendor`, then `git checkout -- vendor/github.com vendor/gvisor.dev` and
  check that `git status vendor` shows only the new module plus `modules.txt`. Holds **five local patches**
  (grep `clashvless`): (1) gVisor `network/ipv4` echo-reply gate for TUN ping — see **ICMP ping**; (2)
  `transport/internet/grpc/dial.go` **content-keys** the global gRPC client-conn cache. It was keyed by the
  `*MemoryStreamConfig` **pointer**, so each failover probe (a fresh throwaway instance → fresh pointer) missed
  the cache and stored a **new, never-closed `grpc.ClientConn`** (+ its goroutines) into an insert-only global
  map → ~2 GB/day leak with gRPC entry nodes. The patch hashes dest+transport+security+socket settings so all
  probes/live dials to one node share **one bounded** conn. (3) `proxy/tun/udp_fullcone.go` — backport of
  upstream xray-core #5888/#6285/#6814 (landed after our v26.3.27): a TUN UDP flow is keyed by **source**
  addr:port, but only the first packet's destination was kept, so every later packet from that source went to
  the **first** peer. An app reusing a source port for a new peer (NTP's fixed `:123`, STUN, DNS, Moonlight) got
  the old peer's reply under the wrong source and silently timed out, while a fresh-socket SOCKS UDP test of the
  same exit passed. Now each packet carries its own destination (`b.UDP`), a domain `b.UDP` reply falls back to
  the flow target, a finished flow can't evict its successor, and packets queue under the lock (no
  send-on-closed panic). (4) xray `common/log` + `app/log` support an optional
  observer after per-instance severity filtering, so the app receives structured xray records without duplicate
  console output. (5) bubbletea `key_windows.go` emits the char carried on an Alt **key-up**: for a char missing
  from the active layout (`@ & #` on Russian) the Windows console synthesizes Alt+numpad and delivers it only there,
  and bubbletea dropped all key-ups, so a pasted `vless://` lost its `@`/`&` and "add main" failed as an invalid URL.
  The same file maps `VK_INSERT` (unmapped upstream), and `inputreader_windows.go` keeps QuickEdit on so right-click
  paste works in the classic console.
  Re-apply **all five** after any re-vendor. Upstream later replaced gVisor in the tun
  inbound with its own stack (2026-10), so a real xray-core bump must re-check (1) and (3), not just re-apply.
- `android/` — **Android app** (minimal UX, core only). No Gradle: `android/build_apk.py` (plain Python, the same
  on every OS) does go → aapt2 → javac → d8 → zipalign → apksigner into `dist/clashvless.apk`. cgo builds use the NDK's
  per-target wrappers (`<triple>24-clang`, `.cmd` via an 8.3 path on Windows), not `clang --target`: Go probes linker
  flags without the extra argument, which dropped the build-id and exported cgo internals on armeabi-v7a. **minSdk 24** (Android 7; API-26
  calls are version-guarded), target 34. ABIs: **arm64** + **armeabi-v7a** + **x86_64** (for emulator tests), all built
  **with cgo** via the NDK's clang (`$ANDROID_HOME/ndk`, Windows/Linux host toolchains) so Go resolves through bionic
  `getaddrinfo` → netd = the **system resolver** (Private DNS, per-network — the browser's path). Without an NDK only
  a cgo-less arm64 build is produced (Go's own DNS). The Go
  binary ships as `lib/<abi>/libclashvless.so` (`extractNativeLibs=true`) and is **exec'd as a child
  process** — the same CLI, `--config <filesDir>`. **CLI access**: the APK is `debuggable` and the app writes
  `files/cv` (a wrapper with the right `--config`/DNS env), so `adb shell run-as com.clashvless sh files/cv
  <cmd>` runs any command as the app's user against its real state — `status`/`tui` attach to the VPN's
  daemon over `control.sock` (daemon = `VpnSvc`'s `run`; no embedded mode, the tun fd only comes from the
  VpnService). Old bionic prints a harmless `unsupported flags DT_FLAGS_1` linker warning on each exec. `VpnSvc` (Java `VpnService`) establishes the tun
  (`198.18.0.1/30`, `0/0`, DNS `8.8.8.8` via the tunnel, no v6 route → Android blocks v6 = no leak), excludes
  **our own package** from the VPN (so every daemon socket — exits, probes, sub fetches — goes direct; this
  replaces fwmark/bypass routes), spawns `run` with `CLASHVLESS_TUN_SOCK`, and hands it the fd over that unix
  socket (`SCM_RIGHTS`). Go side (`platform_android.go`, stub `platform_other.go`): receives the fd → sets
  `XRAY_TUN_FD` (xray's `proxy/tun/tun_android.go` reads it) → forces `TunEnabled` on; `internal/tun/tun_android.go`
  makes the OS layer a no-op (`tun_linux.go` is now `linux && !android`). `platformInit` (cgo-less builds only, `haveCgo`
  in `cgo_android.go`/`nocgo_android.go`) installs a Go resolver from `CLASHVLESS_DNS` (the app passes the
  underlying network's resolvers — cgo-less Go on Android has no `/etc/resolv.conf`; servers are **rotated per
  query**, IPv4 first). Forcing that plain-UDP resolver on a modern phone made sub fetches hang (`context
  deadline exceeded`) while the browser — on the system resolver — opened the same URL fine and defaults SOCKS to `127.0.0.1`. The app also sets
  `SSL_CERT_DIR` to the Android 14+ conscrypt APEX roots + `/system/etc/security/cacerts` (Go only knows the
  latter). Control-server messages (sub add/refetch results, auto-fetch) go through `Supervisor.Log`, so they
  reach `run`'s stdout = the app's Log tab, not only control clients. **Daemon lifecycle** (`Core`): the daemon
  runs whenever the UI is visible — **idle** (no tun fd → `TunEnabled` off; slots still serve SOCKS + probe, so
  Status shows live tiers before connecting) — and is restarted in **VPN mode** by `VpnSvc` on Connect;
  Disconnect kills it and `Ctl`'s loop brings an idle one back if the UI is visible; leaving the UI with the VPN
  off stops it. Idle start and stop are check-and-act under the daemon lock (`Core.startIdleIfNeeded`, `stopIdle`):
  a separate check let a quick Disconnect → Connect replace the new VPN daemon with an idle one. The log has its own
  lock, because `stopDaemon` waits up to 5 s for the process under the daemon lock and the UI thread reads the log.
  `Ctl`'s loop sleeps while nothing needs the daemon (no UI, VPN or tile listener); each of those calls `Ctl.wake()`.
  `run` prints the route on every check, so `Core.pump` keeps a status line only when it changed (ignoring the
  egress ms). **UI** (`MainActivity`, bare, programmatic views) is a **control client like the TUI** (`Ctl`:
  `LocalSocket` → `control.sock`, JSON lines; `state`/`status` events in, `patch`/`addsub`/`rmsub`/`refetch`/`kick`
  out), so every edit applies **live** to the running daemon, VPN up or not. The footer has **Settings**,
  **Overview**, and **Log**. All three are page switches. Overview mirrors the desktop: a "Finish setup" checklist
  while no exit exists, a Connection card (headline and hint as `connectionSummary`/`connectionHint`, a ROUTE line,
  Latency / Route type / Slots serving, the note, a VPN error box), Local endpoints (VPN chip, address and first
  hop, the slot's `liveState` tag, Copy / Copy hop; `connectHost` turns a wildcard bind into `127.0.0.1`), and
  Entry pools (online count, meter, failure rate, fastest, in use). Log retains 2000 records and provides severity,
  module, and live text filters; records are drawn with a muted time, a colored level and the origin, and redrawn
  at most every 250 ms, following new lines only while scrolled to the bottom.
  An enlarged native switch in the header controls the VPN and displays
  Disconnected/Connecting/No route/Connected in a fixed-width, single-line label that cannot clip the final letters. Settings uses
  a bordered **Settings type** group with an inline accordion list: category choices expand beneath the selector with
  animated height and a right-edge chevron rotates to show state, pushing settings downward without overlapping the
  interface. Every category has a description
  and a clear current state, keeping selection visually separate from the settings themselves. All former controls remain
  (including HWID and slot/main management). In Exit routes, failover slots are presented in a vertical tabs menu
  with an Expand/Collapse toggle: by default (collapsed), only the slot used for VPN is displayed, while expanding
  reveals all slots. Slots can be renamed via a rename icon (`clashvless_edit`, with tooltip and content description)
  placed immediately to the right of the slot name header, removing the standalone button. Renaming validates the new
  name, updates state, and maintains VPN slot tracking. The slot and its exits mirror the desktop as spaced cards:
  the slot card has a `slotStatus` box tinted by state (tag such as "T2 Country hop | 187 ms", "First hop only",
  "Down", the route, Port/Hop chips) and 'Selected for VPN'/'Use for VPN' with 'Edit port'; a "Final exits" card
  has Export and one card per exit (priority badge, share/rename/delete icons, chips for "In use", protocol and
  `mainUsage`, the host, Enabled / Allow direct (T1) checkboxes, raise/lower arrows). Exit edits go through
  `editExit` (slot name + URL against the latest state). "Add final exits" takes several links, a base64
  subscription, or a live QR scan (via "Scan QR" button or inside the add dialog). **Providers & nodes** mirrors desktop Subscriptions: provider rows with Active/Not used/Fetch failed
  tags, counts, `ago`, Use this, export and remove (by URL), plus "Scan QR" to import subscriptions directly from QR codes; entry nodes grouped per provider (`nodeGroups`), folding
  per URL (`collapsedSubs`, unused providers start folded), live search and Online only that hide rows in place,
  `sortProbes` (same order as desktop), a pin banner, and node rows with tags, share and Pin. Hardware ID supports QR code export and QR scanning in Device identity settings.
  **QR Scanning** (`QrScanner.java`, backed by embedded ZXing QR reader): in-app camera viewfinder with autofocus, rotation-aware NV21/YUV frame decoding, reticle overlay, and storage image picker fallback.
  A scan session invalidates queued UI results on dismissal and delivers a successful result once;
  delivery also checks that the dialog and activity are still live. Camera startup and release are serialized.
  Image import reads dimensions first, reopens the stream with power-of-two sampling to a maximum 2048-pixel
  side, and recycles the decoded bitmap. `decodeBitmap` also bounds pixel-copy size for other callers;
  memory exhaustion produces an error message. Image results retain their original import callback.
  `ScanImportTest` runs host-side decoder, cancellation, and sampling checks; it does not exercise a physical camera.
  Export dialogs
  (`showShare`) page through links with a QR code (`Exports.qr`, whole-pixel scaling), Copy link / Copy all links /
  Copy as subscription. QR codes, exported links and parsed exits come from the Go binary's hidden `share` command,
  so Android exports the same text as desktop; a reply arriving after the activity finished is dropped. Status
  events redraw Overview and these two sections (`queueLiveRender`, at most every 1.5 s, keeping scroll and search
  focus) except while a finger is down (`dispatchTouchEvent`: a rebuild between press and release drops the tap),
  the category list is open, or the search text was typed in the last 3 s: a rebuild resets the keyboard's composing
  text. `FlowLayout` wraps chips. Node share uses the node of the group's own provider (`NodeGroup.nodes`), since
  names repeat across providers. The activity handles rotation and similar changes itself (`configChanges`,
  `onConfigurationChanged` → `render`), so the page, filters and open dialogs survive. Network has no "Listen port"
  row (slot ports are per slot); "Allow LAN connections" is a switch that restarts the idle engine, since the
  listen address is read at start. VPN settings are validated (IP resolver, IPv4 network, MTU 0 or 576-9000), and
  ports by `portClash`/the first-hop check, mirroring `SlotPortConflict`/`EntryPortConflict`.
  `VpnSvc` publishes a live notification with the canonical resource-ID overload of `setSmallIcon`, using a dedicated
  white-on-transparent drawable. The application icon is a separate legacy mipmap plus an Android 8+ adaptive icon
  (including monochrome), so OEM notification rows that substitute the application icon do not depend on the small
  icon. There is no large icon. The notification shows the selected route/latency and Disconnect. Built-in Android UI punctuation is ASCII-only, and javac
  explicitly uses UTF-8 so server-provided names and source text are never decoded with the Windows code page.
  `VpnTileService` adds a quick-settings tile that starts/stops the VPN directly after Android's one-time VPN
  consent; consent opens MainActivity. Resource linking runs before javac and generates `R.java`.
  Slot/main edits are done Java-side on the state JSON and sent as a top-level `slots` patch (full objects —
  the daemon's patch is a field replace; a fresh slot's `mains` is JSON `null` — a Go nil slice — so the app
  creates the array). Slot edits go through `editSlotNamed` (by name, on the latest state), like exits. Edit failures
  go to the Log tab + a toast, never swallowed; a failed VPN start toasts too.
  Host-testable without a phone: build with a `go build -overlay` that swaps the `*_android.go` files in
  under a `linux` tag, then hand it a tun fd from `unshare -Urn` (the netns stands in for the VPN).
- `dist/` — prebuilt release binaries (**not committed**; build artifacts).

## Build & run

`python build.py` prints the active Go configuration and builds desktop executables in `dist/`.
General build messages persist; package progress occupies one replacing subline on a terminal,
while compiler diagnostics are retained. Windows outputs both windowed and CLI executables;
Wintun is embedded from the committed signed archive, so builds need no DLL download.
It then builds `dist/clashvless.apk` with `android/build_apk.py` when its tools are present (`missing()`):
Go, an SDK with `platforms;android-34` + `build-tools;35.0.0` (`ANDROID_HOME`, `ANDROID_SDK_ROOT`, or the default
locations), and a JDK 17+ (`JAVA_HOME`, `~/.jdks`, or `PATH`). No bash or `zip`: packaging uses Python's zipfile.
Otherwise it says what is missing and skips; `--apk` makes that an error, `--no-apk` skips it. The APK step drops
`GOOS`/`GOARCH`/`CC`/`CGO_ENABLED` so a desktop cross-build's settings don't reach the per-ABI builds.
```
go -C app build -o ../dist/clashvless .     # build the single binary
go -C app run . [command]                   # run from source
go -C app build ./...                        # compile-check every package
python android/build_apk.py                  # → dist/clashvless.apk only (platforms;android-34 + build-tools;35.0.0; 34.0.0 d8 crashes on MainActivity)
```
The GUI toolkit needs cgo, so cross-compiling for Windows takes mingw:
`GOOS=windows CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ go -C app build -o ../dist/clashvless.exe .`
`internal/gui` builds only with cgo, so `CGO_ENABLED=0` gives a working TUI/CLI binary for any desktop OS (the way to
build macOS off a Mac), and bare `clashvless` then opens the TUI, as it does with no display on Linux; a dead X forward
makes the toolkit panic, which `runGUI` turns into an error. It also needs OpenGL at runtime. `gui.Preflight` (`preflight_windows.go`) runs GLFW's own pixel-format filter before
the daemon starts and shows an error box if nothing qualifies, because the release build has no console and the
toolkit's error would be invisible. A failed query lets the toolkit try anyway, so it can't block a working machine.
To test the GUI in VirtualBox, turn on 3D acceleration. `fitScreen` shrinks the window to the work area: a window taller
than the screen gets clamped by Windows behind the toolkit's back, which cuts off the top and shifts every click.

`run` starts the blocking daemon; `tui`/`status` attach to it as clients — but `tui` now **self-starts
an in-process daemon** if none is running (stops when the TUI exits), so a fresh install needs no shell:
`clashvless tui` → the setup wizard opens on the empty config. `run` no longer requires a main (idles DOWN).
Key commands: `add <url>`, `fetch`, `fetch-proxy [host:port|off]`, `loglevel [level]`, `main add <vless://> [slot]`,
`slot [add|rm|rename|port|tun|enable|disable …]`, `up [entry]`, `run`,
`tun [on|off|status|dns <ip>|dns direct|dns tunnel|lan …|ipv6 …|icmp on|off]`, `whoami`, `hwid [<id>|new]`. See the default-case
help block in `main.go`.
For **TUN mode** the daemon must be elevated: `sudo clashvless run` (then attach the TUI as a client).
Set `CLASHVLESS_PPROF=127.0.0.1:6060` to expose `net/http/pprof` on the daemon (localhost-only, opt-in)
for heap/goroutine leak hunts — see the gRPC probe-leak note under `app/vendor/`.

## Architecture essentials
- **Topology**: per slot, a local SOCKS inbound → `main` outbound (the final exit). Empty `ListenAddr` means
  `127.0.0.1`; LAN exposure requires an explicit `0.0.0.0` setting. Windows GUI builds use `-H=windowsgui`,
  while the separate `clashvless-cli.exe` intentionally keeps the console subsystem.
- **Slots** (`store.Slots`, managed in the TUI **Main** tab, CLI `slot`, desktop and Android GUI): the app now runs
  **multiple independent failover pools**, each a `Slot{Name, Type, Port, Enabled, Mains []Main}` where `Type` is
  either VLESS (`""`/`"vless"`, default) or HTTP/SOCKS5 upstream proxy (`"proxy"`).
  A slot IS the old single-pool model — it keeps its own **best** main online on its **own SOCKS
  port** via the T1→T2/T3 cascade. HTTP/SOCKS5 proxy slots route out through upstream `http://` or `socks5://`
  proxies with optional username/password auth (`xray.ProxyToOutbound`, `proxy/http` + `proxy/socks` registered in
  `engine/runner.go`, `xray.ExitToOutbound` dispatched in `engine/supervisor.go` and `main.go`), supporting both direct
  T1 and hopped T2/T3 through subscription entries (defaults to `AllowNoHop=true`, toggleable per exit). All clients
  (TUI, Desktop GUI, Android, CLI) let users choose the slot type on creation and add proxy exits via either
  **copy-paste** (`http://`, `socks5://`, `socks://`, `socks5h://`, `host:port:user:pass`, `user:pass@host:port`,
  `host:port@user:pass`, `user:pass:host:port`, `host:port`, via `xray.ParseProxy` / `share.ParseProxyExits`) or
  **manual parameter input** (protocol, host, port, username, password, optional name, allow-direct; Android uses
  `share.Request.BuildProxy` and `SlotType`/`DefaultScheme`). `ProtoTag` tags proxy exits as `http`, `http·auth`,
  `socks5`, or `socks5·auth`; `exitURLPattern` and `redactSecrets` redact proxy credentials in error logs and `gen`.
  Slots can be renamed via GUI dialogs, CLI `slot rename <slot> <new>`, or
  `store.RenameSlot()`, which updates `store.TunSlot` automatically if the renamed slot was used for VPN. Every **enabled** slot runs concurrently (one `engine.slotRunner`
  each); point any app at any slot's port. One slot is the **TUN slot** (`store.TunSlot`,
  `TunSlotName()` falls back to the first enabled): the TUN bridge attaches to **that slot's port
  only**. Legacy single-pool state migrates into a default slot `"main"` on `ListenPort`. `main`
  CLI + wizard operate on the **first** slot; the TUI manages all slots.
- **Mains** (`Slot.Mains`, each `{Enabled, AllowNoHop}`): `AllowNoHop` mains are tried **direct**
  (T1); every enabled **non-Vision** main is eligible to be dialed **through a hop** (T2/T3). Vision
  exits are direct-only (chaining strips their XTLS flow) so they never hop. **Order is priority**: within a
  tier the cascade tries mains top to bottom (`Slot.MoveMain`; GUI arrows, CLI `main move <from> <to> [slot]`).
  The cycle follows edits immediately rather than waiting for the live chain to fail. `mainsSignature` notices
  a change to the eligible mains and re-selects; the same main on the same tier is kept, so its entry isn't
  churned. A live main that was deleted, disabled, or demoted, or whose tier a pin or force-hop now excludes,
  is retired at once (`followLiveMain` matches it by outbound, so a rename keeps it serving under the new
  name). A slot with no usable main stops serving. A swap whose new instance fails to start reports the slot
  down (`applyFailed`), not the old tier.
- **Tiers** (auto-cascade with hysteresis, **per slot**; `slotRunner.cycle` in `engine/supervisor.go`):
  - **T1** a w/o-hop `main` used directly (XTLS-Vision OK here).
  - **T2** a non-Vision `main` dialed *through* a country-exit entry node.
  - **T3** a non-Vision `main` dialed *through* an ОБХОД / bypass (whitelist) entry node.
  The entry-node pool + real-egress probe are **shared** across slots (on the manager `Supervisor`);
  each slot picks its best entry independently. `Pin*`/`ForceHop` tunables are **global** (apply to
  every slot's cascade) for now.
  - **Leader = steadiest, then fastest throughput** (`rankPool` sort, `stabilityBucket`): the entry pool is
    ranked by **stability first, then mean throughput, then latency** — a fast entry that drops one probe in
    three is worse to sit behind than a steady slower one. Every pool refresh (~20s, `refreshPools`)
    reachability-probes each node; `recordProbe` keeps the **last hour** of pass/fail per node and
    `flakiness()` turns it into a **flake %** (share that FAILED). Ranking uses 10%-wide stability bands
    (`stabBucketPct`) so only a *clear* stability gap overrides throughput; under-sampled nodes
    (`< minStabSamples`) sit mid-pack (`unknownFlakePct`) so the pick degrades gracefully on a cold start.
    **Within a band the tiebreak is mean throughput** (`meanSpeed`, rolling 1h of the rare speedtest sweep,
    `speedWindow`), higher wins; nodes with a measurement rank ahead of unmeasured ones in the same band
    (`rankKey.less` — comparing speed only when both had it was intransitive). The sweep (`runSpeeds`) measures each node **as an entry chained to the TUN slot's first hop-able
    main** (`speedRefMain`), one test at a time — NOT the node as its own exit: a node can be fast alone yet crawl in
    front of the main (its path to the main's server is throttled; measured 2026-11-05: 🇩🇪 22.8 Mbps alone vs 0.5
    chained). Reachable-but-failed transfers score 0; a non-2xx reference reply (`ErrSpeedRefused`, e.g. Cloudflare
    429 — all chained tests share the main's exit IP) rotates to the next `speedURLs` reference (Cloudflare → Hetzner fsn1 → OVH → Hetzner hel1) and retests the same
    node; the sweep aborts (keeping prior samples) only if every reference refuses. Other failures count against the node
    only once the current reference is *proven* this sweep (some node measured through it); before that the reference is
    rotated instead, so a broken reference can't zero the whole pool. Speed requests send `speedUA` (curl UA) — Hetzner
    closes Go's default UA with EOF. The one-time ping→speed upgrade (`liveBySpeed`) waits for a *completed* sweep with
    measurements (`speedsSwept`) and only swaps to a strictly faster entry. **Escape hatch:** `NoSpeedTest` (`no_speedtest`, Config tab
    "Speedtest sweep", picked up within one pool refresh via the `speedOff` atomic) stops sweeps — mid-sweep too — and makes
    `meanSpeed` report every node unmeasured, so ranking falls straight back to stability + ping and no speed upgrade fires;
    samples are kept and count again when switched back on. Live progress is `Status.Sweep` (set per node by `setSweep`, which also refreshes the pools' speeds so a
    result shows the moment its test ends); the TUI renders a spinner on the node under test + `⚡ sweep N/M → <main>` on
    Subs and Status (spinner ticks only while a sweep runs), and a measured-but-<1 Mbps node (`SpeedN>0`) in red. **Ping (`Latency`) is the fallback only when speed isn't measured for both
    candidates** (e.g. cold start, before the first sweep). The flake % is per-node (Country pool = T2,
    Bypass = T3) and shown in the TUI **Subs** tab: a per-node column plus a per-tier
    `flakiness 1h: T2 …·N  T3 …·N` summary; the Mbps column now shows the **mean**. Direct-egress probes are
    the signal (uniform, one per node per refresh) — not chain probes, which are irregular and bias toward
    the selected entry. Ranking only decides the pick at **selection time**; the cycle is then **sticky** —
    while the live chain stays healthy it does NOT re-rank/re-pick within a tier (it only switches *up* a
    tier, with `upThreshold` hysteresis, or re-picks when the current chain dies) — so among equal-stability
    entries it locks the pick and rides it until it dies, not churning as pings jitter. **One exception**
    (`liveBySpeed`, `speedBacked`): a pick forced onto **ping** because no speedtest had run yet is
    *provisional* — the first cycle after throughput data lands, it does a single swap to the best-mean-speed
    entry in its tier, then locks (deliberately un-clever — "don't optimize everything").
  - **Pin health = warm live socket, honest fallback** (`pinnedCycle`): a pinned entry (`PinEntry`) is
    judged off the **live serving socket** — the same path real traffic takes — like the auto cascade, NOT
    a fresh cold probe each cycle. Two failure modes were conflated into a misleading bare `✖ DOWN`:
    - **(a) pinned chain alive but cold-probe flaky.** A cold 2-hop reality chain routinely misses the
      timeout on its first packet while the live exit carries traffic fine; the old code cold-probed every
      cycle and, on that miss, reported "pinned … unreachable" *without touching the still-serving
      instance* — dashboard flapped to DOWN while `curl` worked. Now, once serving the pin, health rides
      the warm `EgressThroughSOCKS` check (cold probe only gates **initial build** / **real recovery**,
      with `downThreshold` hysteresis) → stays UP.
    - **(b) pinned node genuinely dead, but a pre-pin instance still serving.** Pinning a dead node
      (`PinEntry` unreachable — verified in the wild by a raw-TCP timeout to the node's server, i.e. below
      xray entirely) leaves the *previous* chain carrying traffic (neither old nor new code tears it down).
      The bare "DOWN" then read as a false alarm because the port plainly works. Now, on a failed pin
      cold-probe, if the retained live instance still egresses (warm-checked) we report the **exit that's
      actually serving** — `● T2 <realEntry> → <main>` + note `⚠ pinned X down — serving <realEntry>
      instead` — instead of `✖ DOWN`. Truly `✖ DOWN` only when **nothing** is serving. It does not
      re-cascade to *other* entries (the pin is explicit; it freezes the cascade and auto-recovers to the
      pinned node when it returns) — deliberately un-clever, per "don't optimize everything".
    - **(c) reachable entry, but the CHAIN can't carry this main.** When nothing is serving, the DOWN note
      no longer always says "unreachable" — a raw-TCP probe (`TCPLatency`) to the entry's server separates a
      dead entry (`pinned X unreachable`) from a live entry whose chain just won't carry this slot's main
      (`pinned X ok, but no exit via <main> — try a non-Vision entry`). The canonical case: pinning a
      **Vision** entry onto a slot whose only hop main has its **own reality** — Vision splices/pads the
      stream and mangles the inner handshake (a *plain* main hops through the same Vision entry fine). The
      old "unreachable" lied — the node answered on raw TCP in ~190ms.
  - **Primary slot for the header** (`publish`): the top-line CONNECTION status mirrors the **TUN slot while
    TUN is ON** (that's where system traffic goes), else the **first slot**. Before, it always mirrored the
    TUN-designated slot, so a down TUN slot (e.g. a Vision-pinned slot whose reality main can't hop) painted
    the whole header `✖ DOWN` while the default slot the user actually points apps at (`:2084`) was serving.
- **First-hop port**: **every** slot exposes its entry (first hop) on its **own port + 1** by default
  (`store.HopPort`, also used for port-collision checks: `SlotPortConflict`, `EntryPortConflict`) — so each slot's hop is reachable while a chained
  tier is active. The **default (first) slot** additionally honors the global `store.EntryPort` config
  override (Config-tab **First-hop port**, `0` = auto = `ListenPort+1`); other slots always use `Port+1`.
  Because each slot claims `Port` **and** `Port+1`, slots are **spaced two apart** — `store.freeSlotPort`
  and `applyDefaults` reserve both and require `p` **and** `p+1` free when assigning. Ports: default slot
  `ListenPort` (2084) + hop `+1` (2085), extra slots auto-assigned from `+2` up in steps of two
  (2086+hop 2087, 2088+hop 2089, …); egress probes use a throwaway OS-assigned port (`engine.freePort()`).
  - **Hop stays up when the main is down** (`slotRunner.serveHopOnly`, `plan.hopOnly`/`slotRunner.hopOnly`,
    `SlotStatus.HopOnly`): the hop port was served only *inside* a live chained config (T2/T3), so when no
    main exit was reachable at all the whole instance (hop port included) went dark. Now, after the cascade
    fails on **every** tier, the runner falls back to serving the **best reachable entry node directly** on
    the hop port (`BuildConfig(entryPort, entryOB, nil, …)` — the entry as its own sole outbound), so an app
    pointed at `Port+1` still egresses through the first hop even while the slot's main is dead. Only slots
    that actually chain (a hop-capable, non-Vision main) and expose a distinct hop port qualify. The fallback
    is **sticky** — while the entry port still egresses it isn't rebuilt — and it is **not** a live main
    chain, so it gets no blip tolerance: each cycle still re-attempts a full chain (`bestWorking`) and
    auto-upgrades the moment a main returns. State: `✖ DOWN` → `⚠ HOP-ONLY` (TUI header) / slot row `hop
    <entry>`; truly `✖ DOWN` only when **no** entry reaches the internet either. The hop-only key is prefixed
    `HOP:`.
- **Force-hop** (`store.ForceHop`): skip T1 and always route through a hop (tier bounds → 2..3) —
  a quick "is any hop working?" test that keeps the hop-1 port served. `PinTier`/`PinEntry` still override.
- **Chaining trick** (`xray.BuildConfig`): a chained `main` dials through the entry via outbound
  `proxySettings.tag`, and its XTLS flow is stripped — a hopped main must be `flow=""` (non-Vision).
- **TUN mode** (`store.TunEnabled`, `internal/tun`, `engine.syncTun`/`tunUp`/`tunDown`): opt-in
  system-wide capture. A separate **persistent bridge** instance (`xray.BuildTunBridge`: `tun` inbound →
  `socks` outbound → **the TUN slot's** local port) owns the device, so the exit swaps behind the stable
  SOCKS port **without churning routes**, and `apply()`/probe paths stay untouched. The bridge (and its
  device) is created **once** and kept for the daemon's lifetime, because on macOS a `utun` **cannot be
  recreated in-process**: the kernel refuses `SIOCIFDESTROY` while any fd lingers (`ifconfig destroy` →
  "Invalid argument"), and xray never closes the tun fd on instance close (`AlwaysOnInboundHandler.Close`
  only closes workers/mux, and the tun handler has none), so the device only dies at process exit. So the
  bridge dials a **fixed backend port** and a tiny loopback **relay** (`xray.BuildRelay`,
  `engine.ensureRelay`) forwards that to the selected TUN slot's port. Enabling/disabling TUN toggles only
  OS routing (`tunDownRouting` keeps the bridge alive); switching the TUN slot rebuilds **only the relay**
  (`syncTun`) — no device recreation, no routing churn, just a sub-second blip. `tunShutdown` closes the
  bridge at daemon exit. (`ensureBridge` created it via `engine.freePort`; the device name is still the fixed
  `tun.DefaultName()` — macOS `utun9`, Linux `clashvless0`.) Crucially, **every** slot's
  dials are decorated off-tun (below), not just the TUN slot's — so the non-TUN slots keep serving their
  own ports without looping into the tunnel (the tun path stays a clean `tun → TUN-slot T2/T3 → T1`; a
  non-TUN slot leaking in would make it `T1(other) → T2/T3 → T1` and break). The OS layer moves the default
  route onto the tun (`0/1`+`128/1` halves) and keeps **xray's own connections off the tunnel** — vital,
  since the failover probes must egress-test candidate exits over a non-tun path or every "ping" fails.
  On Linux that's **fwmark policy routing + SO_BINDTODEVICE**: `xray.SetTunMode(mark,dev,hosts)` makes
  `BuildConfig` stamp both `sockopt.mark` **and** `sockopt.interface=<uplink dev>` on every dial (live +
  probes); a rule sends marked traffic to a private table out the real uplink. The **device bind is the
  robust half** — an nftables ruleset (Docker/firewalld) can strip the packet mark (conntrack), after which
  the marked packets fall back to the main table and loop into the tun, killing every probe; SO_BINDTODEVICE
  (`tun.UplinkDevice()`, Linux-only) can't be stripped, so xray's sockets physically leave the uplink
  regardless. Main table stays clean (no per-server `/32`s). Windows/macOS lack both, so they bypass by
  explicit per-server routes instead (`tun.UplinkDevice()` returns `""` there). The exit's own domain
  resolves locally via injected `dns.hosts` (also from `SetTunMode`, needs `app/dns`) so bootstrap doesn't loop.
  - **Bring-up retries** (`tunUpOrLatch`, `tunRetryGap`): a failed bring-up retries every 20s instead of latching until
    toggled, because it can be transient: right after a crash Windows refuses to recreate the Wintun adapter for about a
    minute (`Cannot create a file when that file already exists`). `Status.TunUp` reports real capture while that happens:
    `TunEnabled` is only the wish, and traffic goes out directly until TUN is up, so clients must not show "Connected".
    Embedded daemons wait for `Supervisor.Run` on exit (`startEmbeddedDaemon`'s stop func) so routes and DNS get restored.
  - **Server list refresh** (`engine.tunRefreshServers`, `tun.Manager.SetServerIPs`): the bypass list and static
    hosts were resolved only at bring-up. A refetch or a newly added exit then left new servers without a bypass
    route on Windows/macOS, so their probes went through the very exit they might replace. Each tick compares
    the sorted host list (`tunServerHosts`) with the one last resolved. On a change it re-resolves (a failed
    lookup keeps the previous address), reconciles the per-server routes (keeping the real-net resolver's), and
    re-decorates xray with the new hosts. On Linux, fwmark already covers every socket, so only the hosts change.
  - **Route re-assert watchdog** (`tun.Manager.Reassert`/`osReassert`, `engine.syncTun`→`tunReassert`): a
    network event (DHCP renew, Wi-Fi bounce, sleep/wake) can wipe the `0/1`+`128/1` capture halves out from
    under us — the tun device stays up but the default falls back to the real uplink, so traffic **silently
    leaks** while TUN still reports "on" (the `curl ifconfig.me → real IP with TUN on` symptom). Each
    supervisor cycle (`interval_s`, default **12s**) `syncTun` re-checks capture via `captureIntact()`
    (`route -n get`/`ip route get` a public IP in each half → must egress our device) and, if it went
    missing, re-installs both halves and logs the repair (silent on healthy ticks, so no churn). Same check
    on all three OSes (Windows parses `route print` for presence). Worst-case leak window = one tick; lower
    `interval_s` to tighten it. Handles two modes, cheapest first: **(a) uplink moved to a new network** —
    `GatewayChanged()`/`osGatewayChanged()` compares the current real default (we leave the `0/0` default on
    the real uplink, so `route -n get default` still reports the true gateway even while the `/1` halves own
    traffic) against what `osUp` captured; on a change it does an **in-place re-point** (`Reapply()`/
    `osReapply`), NOT a full re-up: the per-server bypass (mac/win) / fwmark table (Linux), LAN bypass, and
    DNS-direct route all pointed at the dead gateway, so they're torn down and reinstalled against the new
    one **reusing the cached server IPs** (`tunUp` stashes `tunMark`+`tunHosts`) so it needs **no DNS** —
    critical, since right after a Wi-Fi switch DNS isn't ready and a re-resolving re-up fails with `no such
    host`. The device/bridge stay up (no teardown → no `resource busy`, no dropped tunnel); then the engine
    re-decorates xray for the new uplink dev (`SetTunMode(tunMark, UplinkDevice(), tunHosts)` — Linux
    `SO_BINDTODEVICE`, no-op on mac) and rebuilds every slot runner (`restartRunners`) so each exit re-dials
    over the corrected path (the persistent bridge/relay/device are untouched). If the
    new uplink isn't ready yet (`Reapply` → "no uplink yet") it defers and retries next cycle, leaving TUN up.
    `Reapply` re-points **routes only**; **DNS is re-applied separately** by `Manager.ReapplyDNS(staticDNS,
    realNet)` (called right after, per-OS `osReapplyDNS`) because the resolver a new network needs is
    mode-dependent: **static routed** re-asserts the constant exit resolver onto the OS (a network event can
    drop the setting; the query rides the tunnel so it's LAN-independent); **real-net** must adopt the NEW
    network's resolver — the old LAN's is now unreachable — detected clobber-immune via `currentDHCPResolver`
    (macOS `ipconfig getpacket <dev>` DHCP lease; Linux `resolvectl dns <dev>` per-link view; **not** the
    `resolv.conf`/service-DNS we overwrote), then it swaps the off-tun resolver bypass (the `/32` on Linux,
    the folded per-server bypass entry on macOS via `replaceResolverIP` keeping `cfg.ServerIPs` in sync) and
    the system DNS to it. Can't detect one → keeps the old resolver and logs (suggesting `tun dns static`).
    Without this, moving networks in real-net mode left DNS pinned to the old network's resolver →
    `Could not resolve host` while the tunnel itself worked (the `http_proxy` path resolves remotely, so it
    kept working — that's the tell). `setDNS` now captures the pre-TUN resolver **once** (guarded on
    `dnsService`/`savedResolv`) so a re-apply doesn't overwrite the restore value with our own.
    **(b) same network, halves wiped** — just re-adds the halves in place. Both automatic; no manual toggle.
    Bring-up itself is also race-hardened: `startBridge` retries on a transient "resource busy" (macOS utun
    still releasing after a quick off→on), surfacing a clear "another daemon already running?" hint if it
    never clears.
  - **DNS routing** (`tun.ResolverFor(realNet, staticDNS)` picks the resolver, chosen in `tunUp` **before**
    `osUp` rewrites `resolv.conf`): two named modes via **`TunDNSDirect`** (the Config-tab **TUN DNS** row shows
    `real-net` / `static routed`; CLI `tun dns real-net|static`):
    - **`static routed`** (default, `TunDNSDirect=false`): queries ride the tunnel to the exit, resolving at
      **`TunStaticDNS`** (its own field, default **`8.8.8.8`** — public, reachable from the exit; editable in the
      Config tab or `tun dns <ip>`, reset with `tun dns auto`). No leak.
    - **`real-net`** (`TunDNSDirect=true`): pins the resolver **off-tun** (`/32` via the real uplink on Linux;
      folded into the per-server bypass on Win/mac) so **domain-named nodes/exits resolve immediately** rather
      than through a not-yet-up tunnel (the bootstrap deadlock). Has **no public default** — it adopts the
      **real local network's resolver** (`tun.SystemResolver()`), because a public default like `8.8.8.8` is
      often firewalled on a corporate LAN and a LAN resolver is unreachable through the exit. `SystemResolver()`
      is **clobber-immune**: it reads the uplink's **DHCP lease** first (`currentDHCPResolver` — Windows `GetAdaptersAddresses`, macOS `ipconfig
      getpacket <dev>`, Linux `resolvectl dns <dev>`) and only falls back to `/etc/resolv.conf`/`resolvectl`.
      Without this it read the resolv.conf **we already overwrote** with our own injected resolver when the DNS
      mode was toggled while TUN was up — so real-net would re-adopt e.g. `8.8.8.8` and label it "your LAN
      resolver" instead of the actual gateway (`192.168.1.1`). The DHCP lease is the ground truth regardless.
  - **Stale-resolver self-heal** (`tun.HealStaleResolver`, `engine.healStaleDNS`, **macOS-only**): real-net on
    macOS pins a resolver into the persistent **Wi-Fi service DNS** (`networksetup -setdnsservers`). That pin
    **outlives the daemon and the network** — a network switch (macOS keeps the static entry; on the new net it's
    dead) or an **unclean daemon exit** (SIGKILL → `restoreDNS` never runs) strands it, and with **TUN off** the
    watchdog isn't running to fix it → **every lookup fails, all exits DOWN** ("connected to a different network
    and nothing works", while `scutil` shows an old-net resolver). So the supervisor loop calls `healStaleDNS`
    **each cycle while TUN is OFF** (when TUN is up, `tunUp`/`ReapplyDNS` own the resolver): it clears the service
    back to **DHCP-auto** iff a pinned server is a **private/link-local IP the current DHCP lease does NOT offer**
    (`staleLANResolvers`, unit-tested) — a stranded LAN resolver. A **public** resolver the user pinned on purpose
    (e.g. `1.1.1.1`) works on any network, so it's never touched. Linux/Windows are no-ops (per-link resolvectl /
    tun-adapter DNS die with the device — no persistent pin to strand).

    IPv4 only (IPv6 isn't tunneled) — see IPv6 block. Needs root/admin.
  - **IPv6 block** (`store.TunBlockIPv6()`, default **on**; `TunAllowIPv6`=false; toggle `tun ipv6 block|allow`,
    Config-tab **TUN block IPv6**): the tunnel is IPv4-only, so on a dual-stack network an app happy-eyeballs
    straight out over v6 and **leaks the real address** past the exit (the `curl ifconfig.me → 2a03:…` symptom).
    Blocking global-unicast v6 (`tun.IPv6BlockRange` = `2000::/3`) makes the v6 connect **fast-fail** so apps
    fall back to the tunneled v4; link-local (`fe80::/10`) and ULA (`fc00::/7`) are left alone. Mechanism differs
    per OS (like the fwmark-vs-route bypass split): **Linux** adds `ip -6 route add unreachable 2000::/3`;
    **macOS** runs `networksetup -setv6off <uplink service>` (reliable across versions; the reject-inet6-route
    syntax isn't) and restores `-setv6automatic` on `osDown`; **Windows** routes the range into the tun
    (`store=active`, so it can't outlive an unclean exit). `tun allow` leaves v6 alone.
  - **LAN bypass** (`store.TunBypassLAN()`, default **on**; `TunTunnelLAN`=false; toggle `tun lan bypass|tunnel`,
    Config-tab **TUN LAN bypass**): keeps private/local ranges **off the tunnel** so LAN-only resources (corp
    intranet, printers, NAS, a router UI) stay reachable — without it the whole default route goes to the exit,
    which can't reach anything behind your gateway. This is our equivalent of **Throne**'s sing-tun
    `route_exclude_address`; since we install our own OS routes, `osUp` instead punches the ranges back out to
    the real gateway (more specific than the `0/1`+`128/1` halves): `tun.LANBypassRanges` = RFC1918
    (`10/8`,`172.16/12`,`192.168/16`) via the gateway + link-local (`169.254/16`) and multicast (`224/4`)
    on-link. Loopback/broadcast need no route (the kernel's `local` table already keeps them off-tun). `osDown`
    removes them. Set `tun lan tunnel` to capture private ranges too (full tunnel).
  - **ICMP ping** (`store.TunICMP()`, default **on**; `TunNoICMP`=false; toggle `tun icmp on|off`, Config-tab
    **TUN ICMP ping**): makes `ping` work under TUN. ICMP can't cross the SOCKS/VLESS tunnel (freedom outbound
    dials TCP/UDP only), so this is a **local synthetic reply** from the tun netstack — device liveness, NOT
    end-to-end exit reachability (a ping succeeds whenever the tun stack is up, even if the exit is down; use
    `curl` for real end-to-end). Mechanism: xray-core's gVisor tun stack runs promiscuous, so every captured
    remote dst is a *temporary* address, and gVisor's `ipv4/icmp.go` deliberately skips the echo reply for
    temporary addresses (handing it to a custom handler xray never installs). We flip that skip via a **local
    vendored patch**: `gvisor.dev/.../network/ipv4.ReplyToTemporaryEcho` (an `atomic.Bool` in
    `tun_icmp_echo.go`; the one-line gate is in `icmp.go`, grep **`clashvless`**) — set by `engine.tunUp`
    (`ReplyToTemporaryEcho.Store(TunICMP())`) and cleared by `tunDown`. gVisor's own echo-reply path builds the
    reply (spoofing is already on for the source), so no hand-crafted packets. **IPv4 only** — v6 is never
    routed into the tun (blocked or off-tun). ⚠️ Being a vendored patch, it is **wiped by any `go mod vendor`
    / xray-core+gVisor bump — re-apply both files afterward** (like `cmd/xhprobe` re-validation).
  Linux and macOS
  are tested and working, and so is Windows, tested on 10 1909. Windows builds embed the official signed Wintun 0.14.1 archive (`internal/tun/assets`); `tun.Prepare` extracts the architecture-matched DLL beside the executable only if missing, before the bridge starts (also used by `tunprobe`). The executable folder must be writable; distribution and first VPN startup work offline. macOS uses a kernel-named `utunN` device and per-service DNS.
- **What can be hopped** (see README matrix + `cmd/xhprobe` lab): a **plain** main (`security=none`,
  e.g. `plain-444`) hops through **any** entry. A main with **its own REALITY** (xhttp / tcp-reality)
  can only hop through a **non-Vision** entry — a Vision entry splices/pads the stream and mangles the
  main's inner reality handshake (the exit then serves its real camouflage cert →
  `REALITY: received real certificate`). Vision mains are direct-only. (The `dialerProxy` experiment
  of the reverted v0.8.x didn't change this — the blocker is Vision on the entry, not the mechanism.)
- **Device identity** (`store.Device`): one stable HWID + Happ User-Agent is reused for
  every fetch so we occupy exactly one panel device slot. Never mint a fresh HWID per fetch.
  The HWID is user-settable everywhere — wizard, TUI **Config** tab (**Device HWID** row, validated by
  `store.ValidHWID`), Android Config tab, and CLI `hwid [<id>|new]` — so pasting another device's HWID makes
  both share one panel slot.
- **Auto-fetch** (`store.AutoFetch()`/`AutoFetchInterval()`, `control.Server.autoFetchLoop`): the daemon
  periodically refetches every subscription so the node set never goes stale — a stale sub keeps probing
  now-dead servers and misses fresh ones (the "two boxes on one account show different providers / far
  fewer up" symptom, which was just an old fetch snapshot). Lives in the control server (owns `refetch()`
  + the hub), runs in **both** `run` and embedded daemons. A coarse 30-min tick calls `maybeAutoFetch`,
  which refetches all subs only when the **oldest** sub is past the interval (`FetchIntervalH`, default
  **24h**; a never-fetched sub has a zero time so it always qualifies) — cheap, and self-healing since a
  box offline for days refetches shortly after start. Toggle + interval live in the Config tab
  (**Auto-fetch subs** / **Auto-fetch every (h)**); `NoAutoFetch` is inverse so the zero value is ON.
- **Listen address**: new installs write `listen_addr: 127.0.0.1`. An older document with it empty meant LAN-reachable,
  so `Load` rewrites it to `0.0.0.0` on upgrade rather than silently cutting those devices off. On Windows that listener
  raises a one-time firewall prompt per binary; Android pins loopback in `platformInit`.
- **Log secrets**: nothing logged may hold a sub URL or an exit UUID. An untitled sub logs by host (`subHost`), a bad main's
  parse error goes through `vlessURLPattern` and is reported once (`badExitLogged`), not on every cycle.
- **State**: `$XDG_CONFIG_HOME/clash_vless/state.json`, written atomically at 0600 (it holds
  sub tokens + HWID). Lives **outside** the repo — never commit it.

## Conventions
- `engine/runner.go` registers only the xray features actually used (keeps the binary small).
  A config using an unregistered protocol/transport fails at runtime — add the blank import there.
  (TUN mode added `proxy/tun` + `app/dns`; `proxy/socks` also provides the bridge's socks *outbound*.)
- Redact secrets (`id` / `publicKey` / `shortId` / `password`) when printing configs — see
  `redactSecrets` in `main.go`.
- **Inbound sniffing is `routeOnly`** (`xray.go` socksInbound + `BuildTunBridge`): sniffing stays on
  for routing, but must NOT `destOverride` the real destination — without `routeOnly` xray rewrites a
  connection's dest to the sniffed host, which **silently hijacks an app's HTTP `CONNECT`** (e.g. an app
  wiring its own HTTP proxy through our SOCKS/TUN: the `CONNECT proxytarget` is sniffed and rerouted to
  the target, bypassing the proxy → hang). Plain HTTPS is unaffected (TLS sniffs to the same SNI host).
- **SOCKS5 UDP relay addresses are per-connection.** `socksInbound` deliberately leaves `settings.ip`
  unset. Xray then answers UDP ASSOCIATE with the local address of that client's TCP connection:
  loopback clients keep receiving `127.0.0.1`/`::1`, while LAN and container clients receive the
  interface address they connected to. This also works with a wildcard listener; never restore a global
  hard-coded relay address.
- Keep code comments minimal: only a line for a constraint the code itself can't show.

## Doc rule (IMPORTANT)
This `CLAUDE.md` is the single living design doc — tracked, so it travels with the code.
**Update it as part of every commit**: before committing a change, refresh the relevant section
so the guide reflects the new state. (There is no separate `context.md` — it was dropped so the
working notes stay in the tracked file and sync across machines.)

## Version rule (IMPORTANT)
**Every commit MUST bump `store.Version`** (`app/internal/store/store.go`) — it shows in the TUI
header and the `version` command. Claude picks the semver part by the change: **major** = breaking /
incompatible; **minor** = a new feature; **patch** / sub-minor = a fix, tweak, docs, or meta change.
Bump it as part of the commit, alongside the `CLAUDE.md` refresh.

## Scratch rule
Throwaway/runtime files (extra `--config` profiles, logs, scratch) go in the git-ignored `tmp/`
subdir at the repo root — keep them out of `$XDG_CONFIG_HOME` and out of commits.
