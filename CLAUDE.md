# clash_vless — project guide for Claude

`clashvless` is a single-binary Go CLI/TUI that manages a **Happ-gated Remnawave
subscription** and keeps the best working exit online — as a local SOCKS5 proxy
or a **system-wide TUN** — with automatic tiered failover.

## Layout
- `app/` — the Go module (`module clashvless`, Go 1.26). All source lives here.
  - `main.go` — CLI entrypoint + command dispatch.
  - `internal/store` — on-disk state (subs, cached nodes, **slots** = per-port failover pools of mains,
    stable device identity, tunables).
  - `internal/happ` — fetches a Remnawave sub (Happ 3.x mimic; optional SOCKS5 fetch proxy); parses
    base64 / URI-list / xray-JSON, building a `vless://` outbound for URI-list nodes so they're usable.
  - `internal/xray` — `vless://` → xray outbound, and full-config assembly (incl. entry→main chaining).
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
    root daemon) — it errors with guidance.
  - `internal/tui` — Bubble Tea dashboard client (Status / Subs / Main / Log / Config); `wizard.go`
    is the first-run setup wizard (sub → proxy → HWID → fetch → review → exit, with a non-plain warning).
    The **Main** tab is now a **slots** view: each slot shows its port, on/off, TUN marker (◉) and live tier,
    with its mains nested; keys `s` add slot · `a` add main · `p` port · `t` TUN slot · `e` enable · `w` w/o-hop
    · `d` delete. Mains & sub nodes show a proto tag (`store.ProtoTag`/`OutboundProtoTag`:
    plain·reality·vision·grpc·ws·tls·xhttp). The Config tab has a **TUN mode** toggle.
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
- `app/vendor/` — vendored deps (committed; builds are hermetic/offline). Holds **two local patches**
  (grep `clashvless`): (1) gVisor `network/ipv4` echo-reply gate for TUN ping — see **ICMP ping**; (2)
  `transport/internet/grpc/dial.go` **content-keys** the global gRPC client-conn cache. It was keyed by the
  `*MemoryStreamConfig` **pointer**, so each failover probe (a fresh throwaway instance → fresh pointer) missed
  the cache and stored a **new, never-closed `grpc.ClientConn`** (+ its goroutines) into an insert-only global
  map → ~2 GB/day leak with gRPC entry nodes. The patch hashes dest+transport+security+socket settings so all
  probes/live dials to one node share **one bounded** conn. Re-apply **both** after any re-vendor.
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
Key commands: `add <url>`, `fetch`, `fetch-proxy [host:port|off]`, `loglevel [level]`, `main add <vless://> [slot]`,
`slot [add|rm|port|tun|enable|disable …]`, `up [entry]`, `run`,
`tun [on|off|status|dns <ip>|dns direct|dns tunnel|lan …|ipv6 …|icmp on|off]`, `whoami`. See the default-case
help block in `main.go`.
For **TUN mode** the daemon must be elevated: `sudo clashvless run` (then attach the TUI as a client).
Set `CLASHVLESS_PPROF=127.0.0.1:6060` to expose `net/http/pprof` on the daemon (localhost-only, opt-in)
for heap/goroutine leak hunts — see the gRPC probe-leak note under `app/vendor/`.

## Architecture essentials
- **Topology**: per slot, a local SOCKS inbound → `main` outbound (the final exit).
- **Slots** (`store.Slots`, managed in the TUI **Main** tab, CLI `slot`): the app now runs
  **multiple independent failover pools**, each a `Slot{Name, Port, Enabled, Mains []Main}`.
  A slot IS the old single-pool model — it keeps its own **best** main online on its **own SOCKS
  port** via the T1→T2/T3 cascade. Every **enabled** slot runs concurrently (one `engine.slotRunner`
  each); point any app at any slot's port. One slot is the **TUN slot** (`store.TunSlot`,
  `TunSlotName()` falls back to the first enabled): the TUN bridge attaches to **that slot's port
  only**. Legacy single-pool state migrates into a default slot `"main"` on `ListenPort`. `main`
  CLI + wizard operate on the **first** slot; the TUI manages all slots.
- **Mains** (`Slot.Mains`, each `{Enabled, AllowNoHop}`): `AllowNoHop` mains are tried **direct**
  (T1); every enabled **non-Vision** main is eligible to be dialed **through a hop** (T2/T3). Vision
  exits are direct-only (chaining strips their XTLS flow) so they never hop.
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
    `speedWindow`), higher wins; **ping (`Latency`) is the fallback only when speed isn't measured for both
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
  (`slotEntryPortLocked` in `engine/supervisor.go`) — so each slot's hop is reachable while a chained
  tier is active. The **default (first) slot** additionally honors the global `store.EntryPort` config
  override (Config-tab **First-hop port**, `0` = auto = `ListenPort+1`); other slots always use `Port+1`.
  Because each slot claims `Port` **and** `Port+1`, slots are **spaced two apart** — `store.freeSlotPort`
  and `applyDefaults` reserve both and require `p` **and** `p+1` free when assigning. Ports: default slot
  `ListenPort` (2084) + hop `+1` (2085), extra slots auto-assigned from `+2` up in steps of two
  (2086+hop 2087, 2088+hop 2089, …); egress probes use a throwaway OS-assigned port (`engine.freePort()`).
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
