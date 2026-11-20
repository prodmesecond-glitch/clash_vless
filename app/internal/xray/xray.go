// Package xray converts proxy links into xray outbounds and assembles the final
// xray config for a given tier of the failover state machine.
//
// The topology is always: a single local SOCKS inbound, and "main" as the final
// outbound. When main is not directly reachable we insert an entry hop and have
// main DIAL THROUGH it via outbound proxySettings.tag — which keeps main's
// Reality/TLS handshake end-to-end (a plain SOCKS hop would break it). Only a
// plain (security=none) main hops cleanly; a main with its own REALITY needs a
// non-Vision entry (a Vision entry mangles the inner reality handshake) — see the
// README connection matrix and cmd/xhprobe.
package xray

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// VlessToOutbound converts a vless:// share link into an xray outbound object
// (as raw JSON) tagged with tag. It handles reality/tls security and
// tcp/ws/grpc transports — enough for the nodes this subscription and the
// user's own servers use.
func VlessToOutbound(uri, tag string) (json.RawMessage, error) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return nil, fmt.Errorf("parse vless url: %w", err)
	}
	if u.Scheme != "vless" {
		return nil, fmt.Errorf("not a vless:// url (scheme %q)", u.Scheme)
	}
	uuid := u.User.Username()
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if uuid == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("vless url missing uuid/host/port")
	}
	q := u.Query()

	network := def(q.Get("type"), "tcp")
	security := def(q.Get("security"), "none")

	user := map[string]any{"id": uuid, "encryption": def(q.Get("encryption"), "none")}
	if flow := q.Get("flow"); flow != "" {
		user["flow"] = flow
	}

	stream := map[string]any{"network": network, "security": security}
	switch security {
	case "reality":
		rs := map[string]any{}
		put(rs, "serverName", q.Get("sni"))
		put(rs, "publicKey", q.Get("pbk"))
		put(rs, "shortId", q.Get("sid"))
		put(rs, "fingerprint", q.Get("fp"))
		put(rs, "spiderX", q.Get("spx"))
		stream["realitySettings"] = rs
	case "tls":
		ts := map[string]any{}
		put(ts, "serverName", q.Get("sni"))
		put(ts, "fingerprint", q.Get("fp"))
		if v := q.Get("alpn"); v != "" {
			ts["alpn"] = strings.Split(v, ",")
		}
		if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
			ts["allowInsecure"] = true
		}
		stream["tlsSettings"] = ts
	}

	switch network {
	case "ws":
		ws := map[string]any{}
		put(ws, "path", q.Get("path"))
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		stream["wsSettings"] = ws
	case "grpc":
		g := map[string]any{}
		put(g, "serviceName", q.Get("serviceName"))
		stream["grpcSettings"] = g
	case "xhttp", "splithttp":
		xh := map[string]any{}
		put(xh, "path", q.Get("path"))
		put(xh, "host", q.Get("host"))
		put(xh, "mode", q.Get("mode"))
		if e := q.Get("extra"); e != "" && json.Valid([]byte(e)) {
			xh["extra"] = json.RawMessage(e) // merged server-side (padding, xmux, …)
		}
		stream["xhttpSettings"] = xh
	}

	ob := map[string]any{
		"tag":      tag,
		"protocol": "vless",
		"settings": map[string]any{
			"vnext": []any{map[string]any{
				"address": host,
				"port":    port,
				"users":   []any{user},
			}},
		},
		"streamSettings": stream,
	}
	return json.Marshal(ob)
}

// ProxySpec describes an HTTP or SOCKS5 upstream exit proxy with optional auth.
type ProxySpec struct {
	Scheme string `json:"scheme"` // "http" or "socks5"
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user,omitempty"`
	Pass   string `json:"pass,omitempty"`
	Name   string `json:"name,omitempty"`
}

func normalizeProxyScheme(scheme string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http":
		return "http", true
	case "socks", "socks5", "socks5h":
		return "socks5", true
	default:
		return "", false
	}
}

func validProxyHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '@' || r == '?' || r == '#' {
			return false
		}
	}
	return true
}

func parsePort(s string) (int, bool) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

// FormatProxyURL validates spec and encodes it as a canonical http:// or
// socks5:// URL (with optional user:pass and #name fragment).
func FormatProxyURL(spec ProxySpec) (string, error) {
	scheme, ok := normalizeProxyScheme(spec.Scheme)
	if !ok {
		return "", fmt.Errorf("unsupported proxy protocol %q (use http or socks5)", spec.Scheme)
	}
	host := strings.TrimSpace(spec.Host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if !validProxyHost(host) {
		return "", fmt.Errorf("invalid proxy host %q", spec.Host)
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return "", fmt.Errorf("proxy port must be 1–65535")
	}
	u := url.URL{
		Scheme:   scheme,
		Host:     net.JoinHostPort(host, strconv.Itoa(spec.Port)),
		Fragment: strings.TrimSpace(spec.Name),
	}
	if spec.User != "" || spec.Pass != "" {
		if spec.Pass != "" {
			u.User = url.UserPassword(spec.User, spec.Pass)
		} else {
			u.User = url.User(spec.User)
		}
	}
	return u.String(), nil
}

// ParseProxy parses an HTTP or SOCKS5 upstream proxy from either a standard URL
// (http://[user:pass@]host:port[#name], socks5://...) or a common proxy-list
// format (host:port:user:pass, user:pass@host:port, host:port@user:pass,
// user:pass:host:port, or host:port). When the input omits a scheme,
// defaultScheme is used (defaulting to "socks5").
func ParseProxy(raw, defaultScheme string) (ProxySpec, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ProxySpec{}, fmt.Errorf("empty proxy")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ProxySpec{}, fmt.Errorf("parse proxy url: %w", err)
		}
		scheme, ok := normalizeProxyScheme(u.Scheme)
		if !ok {
			return ProxySpec{}, fmt.Errorf("unsupported proxy scheme %q (use http or socks5)", u.Scheme)
		}
		host := u.Hostname()
		if !validProxyHost(host) {
			return ProxySpec{}, fmt.Errorf("proxy url missing valid host")
		}
		port, ok := parsePort(u.Port())
		if !ok {
			return ProxySpec{}, fmt.Errorf("proxy url missing valid port")
		}
		var user, pass string
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		return ProxySpec{
			Scheme: scheme,
			Host:   host,
			Port:   port,
			User:   user,
			Pass:   pass,
			Name:   strings.TrimSpace(u.Fragment),
		}, nil
	}

	scheme := "socks5"
	if defaultScheme != "" {
		var ok bool
		scheme, ok = normalizeProxyScheme(defaultScheme)
		if !ok {
			return ProxySpec{}, fmt.Errorf("unsupported proxy protocol %q (use http or socks5)", defaultScheme)
		}
	}

	// Format with @: user[:pass]@host:port (or host:port@user[:pass]).
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		left, right := s[:at], s[at+1:]
		if host, port, ok := splitHostPortStrict(right); ok {
			user, pass := splitUserPass(left)
			return ProxySpec{Scheme: scheme, Host: host, Port: port, User: user, Pass: pass}, nil
		}
		if atFirst := strings.IndexByte(s, '@'); atFirst >= 0 {
			l2, r2 := s[:atFirst], s[atFirst+1:]
			if host, port, ok := splitHostPortStrict(l2); ok {
				user, pass := splitUserPass(r2)
				return ProxySpec{Scheme: scheme, Host: host, Port: port, User: user, Pass: pass}, nil
			}
		}
		return ProxySpec{}, fmt.Errorf("invalid proxy format (expected user:pass@host:port)")
	}

	// Bare host:port (including [ipv6]:port).
	if host, port, ok := splitHostPortStrict(s); ok {
		return ProxySpec{Scheme: scheme, Host: host, Port: port}, nil
	}

	// Bracketed IPv6 with auth: [ipv6]:port:user:pass
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]:"); end > 1 {
			host := s[1:end]
			rest := strings.SplitN(s[end+2:], ":", 3)
			if len(rest) == 3 {
				if port, ok := parsePort(rest[0]); ok && validProxyHost(host) {
					return ProxySpec{Scheme: scheme, Host: host, Port: port, User: rest[1], Pass: rest[2]}, nil
				}
			}
		}
		return ProxySpec{}, fmt.Errorf("invalid IPv6 proxy format")
	}

	// Colon-separated: host:port:user:pass (pass may contain :) or user:pass:host:port.
	parts := strings.Split(s, ":")
	if len(parts) >= 4 {
		if port, ok := parsePort(parts[1]); ok && validProxyHost(parts[0]) {
			return ProxySpec{
				Scheme: scheme,
				Host:   parts[0],
				Port:   port,
				User:   parts[2],
				Pass:   strings.Join(parts[3:], ":"),
			}, nil
		}
		if len(parts) == 4 {
			if port, ok := parsePort(parts[3]); ok && validProxyHost(parts[2]) {
				return ProxySpec{
					Scheme: scheme,
					Host:   parts[2],
					Port:   port,
					User:   parts[0],
					Pass:   parts[1],
				}, nil
			}
		}
	}
	return ProxySpec{}, fmt.Errorf("invalid proxy format (use http://, socks5://, host:port:user:pass, user:pass@host:port, or host:port)")
}

func splitHostPortStrict(s string) (string, int, bool) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, false
	}
	port, ok := parsePort(portStr)
	if !ok || !validProxyHost(host) {
		return "", 0, false
	}
	return host, port, true
}

func splitUserPass(s string) (user, pass string) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// ProxyToOutbound converts a canonical http:// or socks5:// proxy URL into an
// xray outbound JSON object tagged with tag.
func ProxyToOutbound(uri, tag string) (json.RawMessage, error) {
	s := strings.TrimSpace(uri)
	if !strings.Contains(s, "://") {
		return nil, fmt.Errorf("proxy url missing scheme (http:// or socks5://)")
	}
	spec, err := ParseProxy(s, "")
	if err != nil {
		return nil, err
	}
	proto := "socks"
	if spec.Scheme == "http" {
		proto = "http"
	}
	server := map[string]any{
		"address": spec.Host,
		"port":    spec.Port,
	}
	if spec.User != "" || spec.Pass != "" {
		server["users"] = []any{map[string]any{
			"user": spec.User,
			"pass": spec.Pass,
		}}
	}
	ob := map[string]any{
		"tag":      tag,
		"protocol": proto,
		"settings": map[string]any{
			"servers": []any{server},
		},
	}
	return json.Marshal(ob)
}

// ExitToOutbound converts any supported final-exit URL (vless://, http://, or
// socks5://) into an xray outbound object tagged with tag.
func ExitToOutbound(uri, tag string) (json.RawMessage, error) {
	s := strings.TrimSpace(uri)
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse exit url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "vless":
		return VlessToOutbound(s, tag)
	case "http", "socks", "socks5", "socks5h":
		return ProxyToOutbound(s, tag)
	default:
		return nil, fmt.Errorf("unsupported exit scheme %q", u.Scheme)
	}
}

// BuildConfig assembles a full xray config: a single SOCKS inbound on port, and
// main as the final outbound. If entry is non-nil, main is dialed through it
// (entry → main); otherwise main is used directly (T1).
func BuildConfig(port int, main, entry json.RawMessage, entryPort int, logLevel, listen string) (json.RawMessage, error) {
	var mainM map[string]any
	if err := json.Unmarshal(main, &mainM); err != nil {
		return nil, fmt.Errorf("main outbound: %w", err)
	}
	mainM["tag"] = "main"

	// Expose the first hop (entry) on its own local inbound when we're chaining
	// and a distinct port is given — lets you use/probe the entry hop directly.
	exposeEntry := entry != nil && entryPort > 0 && entryPort != port

	outbounds := []any{}
	if entry != nil {
		// Chain main THROUGH entry. Outbound-level proxySettings carries the
		// entry's Reality correctly; sockopt.dialerProxy fell back to plain TLS
		// on the entry (x509 cert failure). This matches the old VLESS setup.
		mainM["proxySettings"] = map[string]any{"tag": "entry"}
		stripFlow(mainM) // XTLS-Vision only works direct; the outer hop must be plain Reality/TLS

		var entryM map[string]any
		if err := json.Unmarshal(entry, &entryM); err != nil {
			return nil, fmt.Errorf("entry outbound: %w", err)
		}
		entryM["tag"] = "entry"
		outbounds = append(outbounds, mainM, entryM)
	} else {
		outbounds = append(outbounds, mainM)
	}
	outbounds = append(outbounds,
		map[string]any{"tag": "direct", "protocol": "freedom"},
		map[string]any{"tag": "block", "protocol": "blackhole"},
	)

	inbounds := []any{socksInbound("in", port, listen)}
	rules := []any{map[string]any{"type": "field", "network": "tcp,udp", "outboundTag": "main"}}
	if exposeEntry {
		inbounds = append(inbounds, socksInbound("in-entry", entryPort, listen))
		rules = []any{
			map[string]any{"type": "field", "inboundTag": []any{"in"}, "outboundTag": "main"},
			map[string]any{"type": "field", "inboundTag": []any{"in-entry"}, "outboundTag": "entry"},
		}
	}

	ll := logLevel // "" defaults to warning; probes pass "none" to stay silent
	if ll == "" {
		ll = "warning"
	}
	cfg := map[string]any{
		"log":       map[string]any{"loglevel": ll},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"routing":   map[string]any{"rules": rules},
	}
	applyTunMode(cfg) // TUN mode: SO_MARK every dial + static hosts (no-op otherwise)
	return json.MarshalIndent(cfg, "", "  ")
}

// --- TUN-mode config decoration --------------------------------------------
//
// When TUN mode is active, BuildConfig marks every dialing outbound's socket
// with SO_MARK so Linux policy routing can steer xray's OWN connections AROUND
// the tunnel — the live exit and, crucially, the failover probes, which must
// reach candidate exits over a non-TUN path or every failover "ping" fails. It
// also injects static hosts so an exit's own domain resolves locally instead of
// looping through the tunnel it bootstraps.

var (
	tunMu    sync.RWMutex
	tunMark  int32
	tunDev   string
	tunHosts map[string]string
)

// SetTunMode configures the decoration BuildConfig applies to every config it
// produces (live and probe alike). mark=0, dev="" & hosts=nil disables it. The
// supervisor sets it when TUN comes up and clears it when TUN goes down.
//
// dev (Linux uplink device, e.g. "wlan0") makes every dial SO_BINDTODEVICE onto
// the real uplink — the robust way to keep xray's own sockets off the tunnel:
// unlike the SO_MARK fwmark trick, a device bind can't be stripped by an
// nftables ruleset (Docker/firewalld), so it survives on hosts where the mark
// gets cleared and the marked packets would otherwise loop back into the tun.
func SetTunMode(mark int32, dev string, hosts map[string]string) {
	tunMu.Lock()
	tunMark, tunDev, tunHosts = mark, dev, hosts
	tunMu.Unlock()
}

func applyTunMode(cfg map[string]any) {
	tunMu.RLock()
	mark, dev, hosts := tunMark, tunDev, tunHosts
	tunMu.RUnlock()
	if mark == 0 && dev == "" && len(hosts) == 0 {
		return
	}
	if len(hosts) > 0 {
		h := map[string]any{}
		for host, ip := range hosts {
			h[host] = ip
		}
		cfg["dns"] = map[string]any{"hosts": h}
	}
	if mark == 0 && dev == "" { // non-Linux: bypass is done by explicit routes
		return
	}
	obs, _ := cfg["outbounds"].([]any)
	for _, o := range obs {
		ob, ok := o.(map[string]any)
		if !ok || ob["protocol"] == "blackhole" { // "block" never dials
			continue
		}
		ss, _ := ob["streamSettings"].(map[string]any)
		if ss == nil {
			ss = map[string]any{}
			ob["streamSettings"] = ss
		}
		so, _ := ss["sockopt"].(map[string]any)
		if so == nil {
			so = map[string]any{}
			ss["sockopt"] = so
		}
		if mark != 0 {
			so["mark"] = mark
		}
		if dev != "" {
			so["interface"] = dev // SO_BINDTODEVICE — survives nftables mark-stripping
		}
	}
}

// BuildTunBridge assembles the config for the persistent TUN "bridge" instance:
// a `tun` inbound whose traffic is forwarded to the local SOCKS port (socksPort)
// that the failover instance serves. Isolating the TUN in its own instance lets
// the exit be swapped behind the SOCKS port without recreating the TUN device,
// so failover never churns system routes. The OS-level interface/route/DNS setup
// is done separately by internal/tun.
func BuildTunBridge(socksPort int, tunName string, mtu int, logLevel string) (json.RawMessage, error) {
	if mtu <= 0 {
		mtu = 1500
	}
	if tunName == "" {
		tunName = "clashvless0"
	}
	ll := logLevel
	if ll == "" {
		ll = "warning"
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": ll},
		"inbounds": []any{map[string]any{
			"tag":      "tun-in",
			"protocol": "tun",
			"settings": map[string]any{"name": tunName, "MTU": mtu},
			"sniffing": map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}, "routeOnly": true},
		}},
		"outbounds": []any{map[string]any{
			"tag":      "to-socks",
			"protocol": "socks",
			"settings": map[string]any{"servers": []any{map[string]any{
				"address": "127.0.0.1",
				"port":    socksPort,
			}}},
		}},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []any{"tun-in"}, "outboundTag": "to-socks"},
		}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// BuildRelay assembles a tiny local SOCKS→SOCKS forwarder: a socks inbound on
// 127.0.0.1:listenPort → a socks outbound to 127.0.0.1:targetPort. The persistent
// TUN bridge dials the STABLE listenPort; the relay forwards to whichever slot is
// currently selected for TUN. Switching the TUN slot then only rebuilds this
// cheap relay — never the tun device, which macOS cannot recreate in-process (the
// kernel won't destroy a utun while any fd lingers, and xray leaks that fd until
// the process exits). Loopback-only, so it stays off the tunnel automatically.
func BuildRelay(listenPort, targetPort int, logLevel string) (json.RawMessage, error) {
	ll := logLevel
	if ll == "" {
		ll = "warning"
	}
	cfg := map[string]any{
		"log":      map[string]any{"loglevel": ll},
		"inbounds": []any{socksInbound("relay-in", listenPort, "127.0.0.1")},
		"outbounds": []any{map[string]any{
			"tag":      "to-slot",
			"protocol": "socks",
			"settings": map[string]any{"servers": []any{map[string]any{
				"address": "127.0.0.1",
				"port":    targetPort,
			}}},
		}},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []any{"relay-in"}, "outboundTag": "to-slot"},
		}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// socksInbound builds a local SOCKS5 inbound on 127.0.0.1:port with the given tag.
func socksInbound(tag string, port int, listen string) map[string]any {
	if listen == "" {
		listen = "127.0.0.1"
	}
	return map[string]any{
		"tag":      tag,
		"listen":   listen,
		"port":     port,
		"protocol": "socks",
		"settings": map[string]any{"auth": "noauth", "udp": true},
		"sniffing": map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}, "routeOnly": true},
	}
}

// stripFlow removes the XTLS flow from a vless outbound's first user. The outer
// hop of a chain must be plain Reality/TLS (flow=""), because XTLS-Vision only
// works on a direct connection. The main SERVER must therefore accept a
// flow-less (non-Vision) user for chaining to succeed.
func stripFlow(ob map[string]any) {
	settings, _ := ob["settings"].(map[string]any)
	if settings == nil {
		return
	}
	vnext, _ := settings["vnext"].([]any)
	if len(vnext) == 0 {
		return
	}
	v0, _ := vnext[0].(map[string]any)
	if v0 == nil {
		return
	}
	users, _ := v0["users"].([]any)
	if len(users) == 0 {
		return
	}
	if u0, ok := users[0].(map[string]any); ok {
		delete(u0, "flow")
	}
}

func def(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func put(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}
