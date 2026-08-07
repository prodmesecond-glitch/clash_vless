# clash_vless — project guide for Claude

`clashvless` is a single-binary Go CLI/TUI that manages a **Happ-gated Remnawave
subscription** and keeps a local SOCKS5 proxy pointed at the best working exit,
with automatic tiered failover.

## Layout
- `app/` — the Go module (`module clashvless`, Go 1.26). All source lives here.
  - `main.go` — CLI entrypoint + command dispatch.
  - `internal/store` — on-disk state (subs, cached nodes, stable device identity, tunables).
  - `internal/happ` — fetches a Remnawave sub (Happ 3.x mimic; optional SOCKS5 fetch proxy); parses
    base64 / URI-list / xray-JSON, building a `vless://` outbound for URI-list nodes so they're usable.
  - `internal/xray` — `vless://` → xray outbound, and full-config assembly (incl. entry→main chaining).
  - `internal/engine` — embeds xray-core in-process; the failover supervisor + real-egress pool probe.
  - `internal/control` — daemon↔client IPC over a Unix socket: `run` serves it; `tui`/`status` attach.
  - `internal/tui` — Bubble Tea dashboard client (Status / Subs / Main / Log / Config).
- `app/vendor/` — vendored deps (committed; builds are hermetic/offline).
- `dist/` — prebuilt release binaries (**not committed**; build artifacts).

## Build & run
```
go -C app build -o ../dist/clashvless .     # build the single binary
go -C app run . [command]                   # run from source
go -C app build ./...                        # compile-check every package
```
`run` starts the blocking daemon; `tui`/`status` attach to it as clients. Key commands: `add <url>`,
`fetch`, `fetch-proxy [host:port|off]`, `main add <vless://>`, `up [entry]`, `run`, `whoami`.
See the default-case help block in `main.go` for the full list.

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
- **Chaining trick** (`xray.BuildConfig`): a chained `main` dials through the entry via
  outbound `proxySettings.tag`, and its XTLS flow is stripped — Vision only works on a
  direct hop, so a hopped main must be a `flow=""` (non-Vision) user.
- **Device identity** (`store.Device`): one stable HWID + Happ User-Agent is reused for
  every fetch so we occupy exactly one panel device slot. Never mint a fresh HWID per fetch.
- **State**: `$XDG_CONFIG_HOME/clash_vless/state.json`, written atomically at 0600 (it holds
  sub tokens + HWID). Lives **outside** the repo — never commit it.

## Conventions
- `engine/runner.go` registers only the xray features actually used (keeps the binary small).
  A config using an unregistered protocol/transport fails at runtime — add the blank import there.
- Redact secrets (`id` / `publicKey` / `shortId` / `password`) when printing configs — see
  `redactSecrets` in `main.go`.
- Keep code comments minimal: only a line for a constraint the code itself can't show.

## Context rule (IMPORTANT)
`context.md` at the repo root is the living design / working-notes doc. It is **local and
untracked** (git-ignored). **Update `context.md` as part of every commit**: before committing
a change, refresh `context.md` so it reflects the new state. Never stage or commit `context.md`.
