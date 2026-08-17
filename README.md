# clashvless

A single-binary Go **CLI + TUI** that manages a Happ-gated **Remnawave** subscription and keeps a
local **SOCKS5** proxy pointed at the best working exit — with automatic, tiered failover and an
embedded [xray-core](https://github.com/XTLS/Xray-core) (no external `xray` needed).

<p align="center"><img src="docs/tui-status.png" width="640" alt="clashvless status dashboard"></p>

## What it does

- **Embedded xray-core** — the whole thing compiles to one static binary.
- **Managed exits ("mains")** — keep several final exits, each with two toggles: **enable** and
  **allow w/o hop**. Vision exits run direct; plain exits get dialed through a hop.
- **Tiered auto-failover** with hysteresis:
  - **T1** — a w/o-hop main used **directly** (XTLS-Vision).
  - **T2 / T3** — a non-Vision main dialed **through a hop** (country exit / ОБХОД bypass node).
- **First-hop port** — while chained, the entry hop is also served on its own local SOCKS port, so
  you can use or probe hop-1 directly.
- **Force-hop** — one toggle to skip T1 and prove a hop actually works.
- **Happ-mimicking fetch** — one stable device identity (HWID + User-Agent) reused for every fetch,
  so you occupy exactly one panel device slot (never burns a fresh one).
- **Live Bubble Tea dashboard** — Status / Subs / Main / Log / Config, everything editable live.

## Screenshots

| Manage exits — `Main` | Live tunables — `Config` |
|:---:|:---:|
| <img src="docs/tui-main.png" width="440" alt="Main tab"> | <img src="docs/tui-config.png" width="440" alt="Config tab"> |

<p align="center"><img src="docs/tui-subs.png" width="680" alt="Subs tab"></p>

## Build

Requires Go 1.26+. Dependencies are vendored, so builds are hermetic and offline.

```sh
git clone https://github.com/prodmesecond-glitch/clash_vless
cd clash_vless
go -C app build -o ../dist/clashvless .
```

## Quick start

Just open the dashboard — on a fresh install it runs a **guided setup** (add sub → fetch → add exit),
no shell needed:

```sh
clashvless                               # or: clashvless tui
```

Then point your apps at `socks5://127.0.0.1:2084`.

### Two ways to run

The TUI checks whether a daemon's control socket is already up: if so it **attaches** to it; if not it
**starts its own engine in-process** for the session. Pick whichever fits:

**1. Interactive.** The engine lives inside the TUI and stops when you close it — simplest, good for
setup and occasional use. The header shows `· embedded`.
```sh
clashvless                               # TUI + its own engine
```

**2. Persistent daemon.** Keep the proxy serving after you close the dashboard (background / service).
The header shows `· daemon`.
```sh
clashvless run                           # blocking daemon: SOCKS + control socket, auto T1→T2→T3
clashvless tui                           # attaches to it  (or `clashvless status` for a one-shot)
```
`run` idles even with no exit yet — open the TUI to set one up. Prefer the shell? You still can:

```sh
clashvless add '<subscription-url>'      # add a sub (fetches its nodes)
clashvless main add '<vless://…>'         # add a final exit (plain hops freely — see the matrix below)
```

### Commands

| command | what |
|---|---|
| `add <url>` | add a subscription (and fetch it) |
| `subs` · `rm <i>` | list · remove subscriptions |
| `main` · `main add <vless://>` · `main rm <i>` | list · add · remove final exits |
| `fetch` | refetch every subscription |
| `list` | show cached nodes (exit + ОБХОД pools) |
| `up [entry]` | serve one tier once (optionally chained via a node) |
| `gen [entry]` | print the assembled xray config for a tier |
| `run` | headless failover engine |
| `tun [on\|off\|status]` | system-wide TUN capture (needs a root/admin daemon — see below) |
| `tun dns real-net\|static` · `tun dns <ip>\|auto` | DNS mode (resolve on your LAN vs via the exit) · set/reset the static resolver |
| `tun lan bypass\|tunnel` | keep private/LAN ranges off the tunnel (default) or route them through the exit |
| `tui` | the dashboard (default with no args) |
| `whoami` | show this device's panel identity |

Flags: `--config <path>` (alternate state file/dir — e.g. keep real and demo profiles) ·
`--cli` (headless) · `--debug` (mirror engine events to `events.log`).

## TUN mode (system-wide)

By default clashvless serves a **SOCKS5 proxy** you point apps at. TUN mode instead captures **all**
traffic on the machine and routes it through the active exit — no per-app proxy config. It needs the
daemon running **elevated** (creating a network device requires root/Administrator), so run it standalone:

```sh
sudo clashvless run        # daemon as root — TUN can create the device
clashvless tun on          # turn TUN on  (or toggle it in the TUI Config tab)
clashvless tui             # attach the dashboard as your normal user
clashvless tun off         # back to SOCKS-only
```

**How it works.** A small persistent *bridge* (xray's native `tun` inbound → the local SOCKS port) owns
the device, so failover swaps the exit behind the stable SOCKS port **without dropping the tunnel** or
touching routes. The default route is moved onto the TUN; the proxy **server IPs are auto-bypassed**
(routed direct via your real gateway) so the exit connection doesn't loop back into the tunnel; and DNS is
set up per the mode below. The exit's own domain resolves locally, so bootstrap never chicken-and-eggs.

**DNS.** Two modes — the **TUN DNS** row in the Config tab, or `clashvless tun dns real-net|static`:

- **static routed** (default): DNS queries ride the tunnel and exit at the VPN server's network — no leak to
  your ISP's resolver. Resolves at a **static DNS** that defaults to `8.8.8.8` (reachable from the exit); set
  your own in the Config tab's **TUN static DNS** field or with `clashvless tun dns <ip>` (`tun dns auto`
  resets it to `8.8.8.8`).
- **real-net**: DNS resolves on the **real** local network (off-tun). Use this if your exit/nodes are
  **domain-named** and TUN comes up stuck on `all tiers unreachable` — the bootstrap deadlock, where
  resolving those names needs a tunnel that isn't up yet. real-net **auto-adopts the resolver you were
  already using before TUN** (from `/etc/resolv.conf`), so it just works on networks that firewall public
  resolvers like `8.8.8.8` or hand out a private LAN resolver. Trades DNS privacy for bootstrapping.

**LAN bypass** (on by default — the **TUN LAN bypass** toggle, or `clashvless tun lan bypass|tunnel`).
Private/local ranges (`10/8`, `172.16/12`, `192.168/16`, link-local, multicast) are kept **off** the tunnel
so machines reachable **only on your network** — a corp intranet, a NAS, a printer, your router's admin page —
stay reachable. Without it the whole default route goes to the exit, which can't reach anything behind your
gateway (this is the same thing Throne does with sing-box `route_exclude_address`). Set `tun lan tunnel` to
route private ranges through the exit too.

**Notes / limits**
- **`ping` doesn't traverse the tunnel** — xray's TUN forwards **TCP and UDP only, not ICMP**. So
  `ping 8.8.8.8` shows 100% loss even when everything works; test with `curl ifconfig.me` (it should print
  your *exit's* IP) or just browse. This is normal for xray-tun / tun2socks, not a bug.
- xray's own connections — the live exit **and the failover probes** — are kept off the tunnel so egress
  testing keeps working: on **Linux** via `fwmark` policy routing **plus `SO_BINDTODEVICE`** onto the real
  uplink (the device bind survives firewalls — e.g. Docker/firewalld — that strip the packet mark, which
  would otherwise loop xray's own traffic back into the tun; the main route table stays clean — no
  per-server routes), on **Windows/macOS** via explicit per-server bypass routes.
- **Linux, Windows, and macOS.** Linux and macOS are tested and working; **Windows is code-complete but
  not yet runtime-tested — treat it as beta.**
- **Windows** needs `wintun.dll` next to the executable (from [wintun.net](https://www.wintun.net/)).
- **macOS** uses a kernel-named `utunN` device (xray requires the `utunN` form); the default is `utun9` —
  set `tun_name` to another `utunN` if that unit is busy.
- **IPv4 only** — IPv6 is not tunneled; disable IPv6 if leaks matter.
- Sanity-check the device layer on your box *without* touching routing: `sudo go -C app run ./cmd/tunprobe`.
- Tunables (state file / Config tab): `tun_name`, `tun_addr`, `tun_mtu`, `tun_dns`.

## How failover works

The topology is always: **local SOCKS inbound → `main` outbound** (the final exit).

- **T1** tries each **w/o-hop** main **directly**. XTLS-Vision works here.
- If T1 is down, **T2 / T3** dial a **non-Vision** main *through* the fastest working entry node
  (T2 = country-exit pool, T3 = ОБХОД / bypass pool). Vision exits are never hopped — chaining
  strips the XTLS flow — so keep a plain `flow=""` exit around for hops.

Hysteresis prevents flapping: it only switches **up** to a better tier after it holds for a few
cycles, and only switches **down** after the live chain fails a few. `Pin tier`, `Pin entry`, and
`Force hop` override the automatic cascade.

## What can be hopped (connection matrix)

A hopped chain is **you → entry (hop) → main (exit)**. Whether it carries traffic depends on the
**exit's security** and the **entry's flow**. Verified on real hardware unless marked; `cmd/xhprobe`
is a local lab that stands up the whole topology and reads the actual TLS cert each combo yields.

| Exit (final main)                              | Direct (T1) | via **Vision** hop | via **non‑Vision** hop |
|------------------------------------------------|:-----------:|:------------------:|:----------------------:|
| **Plain** — `security=none` (e.g. `plain‑444`) |      ✓      |         ✓          |           ✓            |
| **Reality** — `security=reality` (xhttp / tcp) |      ✓      |         ✗          |    ⚠️ lab ✓, unconfirmed |
| **Reality + XTLS‑Vision**                      |  ✓ (only)   |         ✗          |           ✗            |

**Why** — the entry is dialed *directly*, so it keeps its own reality (and Vision, if any); the
**exit** is what has to survive being relayed through it:

- A **plain** exit carries only ordinary data, so the entry's Vision splices the destination's TLS
  exactly as designed — it rides through **any** hop. This is the everyday case (a plain exit hopped
  through a country node).
- A **reality** exit brings a *second* reality handshake. A **Vision** hop splices/pads that
  handshake and breaks it — the exit treats you as an unauthenticated probe and forwards you to its
  real camouflage site, so you get *that* site's cert (`REALITY: received real certificate`). A
  **non‑Vision** (plain‑reality, e.g. grpc/ws) hop relays it untouched — reality‑over‑reality is fine
  there (shown in the lab; not yet confirmed on live non‑Vision hardware).
- **XTLS‑Vision** can't be relayed at all (it needs a direct link), so a Vision exit is direct‑only.

One line: **a Vision hop can carry only a plain exit; a reality exit needs a non‑Vision hop.**

## State

State lives at `$XDG_CONFIG_HOME/clash_vless/state.json` (outside the repo), written atomically at
`0600` — it holds your subscription tokens and device HWID, so **never commit it**. Pass
`--config <path>` to keep separate profiles.

---

<sub>Screenshots use placeholder servers (`203.0.113.x`, public resolvers) — not real infrastructure.</sub>
