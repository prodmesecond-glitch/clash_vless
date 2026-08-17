# clash_vless — project guide for Claude

`clashvless` is a single-binary Go CLI/TUI that manages a **Happ-gated Remnawave
subscription** and keeps the best working exit online — as a local SOCKS5 proxy
or a **system-wide TUN** — with automatic tiered failover.

## Layout
- `app/` — the Go module (`module clashvless`, Go 1.26). All source lives here.
  - `main.go` — CLI entrypoint + command dispatch.
  - `internal/store` — on-disk state (subs, cached nodes, stable device identity, tunables).
  - `internal/happ` — fetches a Remnawave sub (Happ 3.x mimic; optional SOCKS5 fetch proxy); parses
    base64 / URI-list / xray-JSON, building a `vless://` outbound for URI-list nodes so they're usable.
  - `internal/xray` — `vless://` → xray outbound, and full-config assembly (incl. entry→main chaining).
  - `internal/engine` — embeds xray-core in-process; the failover supervisor + real-egress pool probe.
  - `internal/control` — daemon↔client IPC over a Unix socket: `run` serves it; `tui`/`status` attach.
  - `internal/tui` — Bubble Tea dashboard client (Status / Subs / Main / Log / Config); `wizard.go`
    is the first-run setup wizard (sub → proxy → HWID → fetch → review → exit, with a non-plain warning).
    Mains & sub nodes show a proto tag (`store.ProtoTag`/`OutboundProtoTag`: plain·reality·vision·grpc·ws·tls·xhttp).
    The Config tab has a **TUN mode** toggle.
  - `internal/tun` — TUN mode OS layer (Linux `ip`/`resolvectl`, Windows `netsh`/`route`, macOS
    `route`/`networksetup`; stub on other OSes). Moves the default route onto the device and keeps **xray's
    own sockets off the tunnel** so the live exit AND the failover probes reach servers directly — on Linux
    via **fwmark policy routing** (`tun.FwMark`, a private table for marked traffic), on Windows/macOS via
    per-server bypass routes. Sets exit-routed DNS; `Down()` restores everything. Needs root/admin. Device
    name default is OS-specific (`tun.DefaultName()`: `clashvless0` on Linux/Windows, `utunN` on macOS —
    xray requires that form and assigns the utun's address itself). Forwards TCP/UDP only — **ICMP/`ping`
    does not traverse the tunnel**; test with `curl`, not `ping`.
  - `cmd/xhserver` — local test rig: Vision-reality hop (`:9001`) + xhttp-reality exit (`:9002`) +
    plain-reality (non-Vision) hop (`:9003`), to point a client config at (direct T1 / hopped T2).
    Separate `main` (pulls in vless/inbound); not in the app binary.
  - `cmd/xhprobe` — self-contained chaining-mechanism probe: stands up exit+hops and dials them
    via every method (`proxySettings` vs `sockopt.dialerProxy`, xhttp/tcp × Vision/non-Vision),
    printing PASS/FAIL. Proves why chaining uses dialerProxy; re-run after any xray-core bump.
  - `cmd/tunprobe` — root/admin-only lab: starts JUST the TUN bridge, confirms xray creates the device,
    then tears down **without touching routing** (safe). Answers "does the native tun inbound work here?".
- `app/vendor/` — vendored deps (committed; builds are hermetic/offline).
- `dist/` — prebuilt release binaries (**not committed**; build artifacts).

## Build & run
```
go -C app build -o ../dist/clashvless .     # build the single binary
go -C app run . [command]                   # run from source
go -C app build ./...                        # compile-check every package
```
`run` starts the blocking daemon; `tui`/`status` attach to it as clients — but `tui` now **self-starts
an in-process daemon** if none is running (stops when the TUI exits), so a fresh install needs no shell:
`clashvless tui` → the setup wizard opens on the empty config. `run` no longer requires a main (idles DOWN).
Key commands: `add <url>`, `fetch`, `fetch-proxy [host:port|off]`, `loglevel [level]`, `main add <vless://>`,
`up [entry]`, `run`, `tun [on|off|status|dns <ip>|dns direct|dns tunnel]`, `whoami`. See the default-case
help block in `main.go`.
For **TUN mode** the daemon must be elevated: `sudo clashvless run` (then attach the TUI as a client).

## Architecture essentials
- **Topology**: local SOCKS inbound → `main` outbound (the final exit, always).
- **Mains** (`store.Mains`, managed in the TUI **Main** tab): a list of final-exit
  candidates, each `{Enabled, AllowNoHop}`. `AllowNoHop` mains are tried **direct** (T1);
  every enabled **non-Vision** main is eligible to be dialed **through a hop** (T2/T3).
  Vision exits are direct-only (chaining strips their XTLS flow) so they never hop.
- **Tiers** (auto-cascade with hysteresis; see `engine/supervisor.go`):
  - **T1** a w/o-hop `main` used directly (XTLS-Vision OK here).
  - **T2** a non-Vision `main` dialed *through* a country-exit entry node.
  - **T3** a non-Vision `main` dialed *through* an ОБХОД / bypass (whitelist) entry node.
- **First-hop port**: while chained (T2/T3), the entry (first hop) is also served on its own
  local SOCKS port — `store.EntryPort`, default `ListenPort+1` — routed straight out via the
  `entry` outbound, so you can use/probe hop-1 directly. Ports: main `ListenPort` (default 2084), hop-1 `+1`; egress
  probes use a throwaway OS-assigned port (`engine.freePort()`), never a fixed one.
- **Force-hop** (`store.ForceHop`): skip T1 and always route through a hop (tier bounds → 2..3) —
  a quick "is any hop working?" test that keeps the hop-1 port served. `PinTier`/`PinEntry` still override.
- **Chaining trick** (`xray.BuildConfig`): a chained `main` dials through the entry via outbound
  `proxySettings.tag`, and its XTLS flow is stripped — a hopped main must be `flow=""` (non-Vision).
- **TUN mode** (`store.TunEnabled`, `internal/tun`, `engine.syncTun`/`tunUp`/`tunDown`): opt-in
  system-wide capture. A separate **persistent bridge** instance (`xray.BuildTunBridge`: `tun` inbound →
  `socks` outbound → local `ListenPort`) owns the device, so the exit swaps behind the stable SOCKS port
  **without churning routes**, and `apply()`/probe paths stay untouched. The OS layer moves the default
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
  - **DNS routing** (`tun.ResolverFor(realNet, staticDNS)` picks the resolver, chosen in `tunUp` **before**
    `osUp` rewrites `resolv.conf`): two named modes via **`TunDNSDirect`** (the Config-tab **TUN DNS** row shows
    `real-net` / `static routed`; CLI `tun dns real-net|static`):
    - **`static routed`** (default, `TunDNSDirect=false`): queries ride the tunnel to the exit, resolving at
      **`TunStaticDNS`** (its own field, default **`8.8.8.8`** — public, reachable from the exit; editable in the
      Config tab or `tun dns <ip>`, reset with `tun dns auto`). No leak.
    - **`real-net`** (`TunDNSDirect=true`): pins the resolver **off-tun** (`/32` via the real uplink on Linux;
      folded into the per-server bypass on Win/mac) so **domain-named nodes/exits resolve immediately** rather
      than through a not-yet-up tunnel (the bootstrap deadlock). Has **no public default** — it adopts the host's
      **pre-TUN system resolver** (`tun.SystemResolver()`: first non-loopback IPv4 in `/etc/resolv.conf`,
      `resolvectl` fallback for the systemd stub; Linux/macOS only), because a public default like `8.8.8.8` is
      often firewalled on a corporate LAN and a LAN resolver is unreachable through the exit.

    IPv4 only (IPv6 isn't tunneled) — see IPv6 block. Needs root/admin.
  - **IPv6 block** (`store.TunBlockIPv6()`, default **on**; `TunAllowIPv6`=false; toggle `tun ipv6 block|allow`,
    Config-tab **TUN block IPv6**): the tunnel is IPv4-only, so on a dual-stack network an app happy-eyeballs
    straight out over v6 and **leaks the real address** past the exit (the `curl ifconfig.me → 2a03:…` symptom).
    Blocking global-unicast v6 (`tun.IPv6BlockRange` = `2000::/3`) makes the v6 connect **fast-fail** so apps
    fall back to the tunneled v4; link-local (`fe80::/10`) and ULA (`fc00::/7`) are left alone. Mechanism differs
    per OS (like the fwmark-vs-route bypass split): **Linux** adds `ip -6 route add unreachable 2000::/3`;
    **macOS** runs `networksetup -setv6off <uplink service>` (reliable across versions; the reject-inet6-route
    syntax isn't) and restores `-setv6automatic` on `osDown`; **Windows** routes the range into the tun
    (best-effort, untested). `tun allow` leaves v6 alone.
  - **LAN bypass** (`store.TunBypassLAN()`, default **on**; `TunTunnelLAN`=false; toggle `tun lan bypass|tunnel`,
    Config-tab **TUN LAN bypass**): keeps private/local ranges **off the tunnel** so LAN-only resources (corp
    intranet, printers, NAS, a router UI) stay reachable — without it the whole default route goes to the exit,
    which can't reach anything behind your gateway. This is our equivalent of **Throne**'s sing-tun
    `route_exclude_address`; since we install our own OS routes, `osUp` instead punches the ranges back out to
    the real gateway (more specific than the `0/1`+`128/1` halves): `tun.LANBypassRanges` = RFC1918
    (`10/8`,`172.16/12`,`192.168/16`) via the gateway + link-local (`169.254/16`) and multicast (`224/4`)
    on-link. Loopback/broadcast need no route (the kernel's `local` table already keeps them off-tun). `osDown`
    removes them. Set `tun lan tunnel` to capture private ranges too (full tunnel).
  Linux and macOS
  are tested and working; **Windows is code-complete but author-untested** (needs `wintun.dll` beside the
  exe). macOS uses a kernel-named `utunN` device and per-service DNS.
- **What can be hopped** (see README matrix + `cmd/xhprobe` lab): a **plain** main (`security=none`,
  e.g. `plain-444`) hops through **any** entry. A main with **its own REALITY** (xhttp / tcp-reality)
  can only hop through a **non-Vision** entry — a Vision entry splices/pads the stream and mangles the
  main's inner reality handshake (the exit then serves its real camouflage cert →
  `REALITY: received real certificate`). Vision mains are direct-only. (The `dialerProxy` experiment
  of the reverted v0.8.x didn't change this — the blocker is Vision on the entry, not the mechanism.)
- **Device identity** (`store.Device`): one stable HWID + Happ User-Agent is reused for
  every fetch so we occupy exactly one panel device slot. Never mint a fresh HWID per fetch.
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
