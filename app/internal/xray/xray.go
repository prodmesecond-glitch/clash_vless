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
	"net/url"
	"strconv"
	"strings"
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
		"settings": map[string]any{"auth": "noauth", "udp": true, "ip": "127.0.0.1"},
		"sniffing": map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}},
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
