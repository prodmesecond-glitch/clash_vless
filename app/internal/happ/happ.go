// Package happ fetches a Remnawave subscription by presenting the same request
// signature the Happ client uses, and parses the result into nodes.
//
// The panel gates real nodes behind a Happ 3.x User-Agent plus a REGISTERED
// X-Hwid. We always fetch with the single stable device identity from the store
// (never a fresh HWID), so we occupy exactly one device slot.
package happ

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"clashvless/internal/store"
	"clashvless/internal/xray"
)

// ErrDeviceLimit means the panel rejected our HWID because the account's device
// limit is full. Free a slot in the panel/bot and retry with the SAME hwid.
var ErrDeviceLimit = errors.New("device limit reached: free a slot in the panel/bot, then retry with the same hwid")

// Fetch pulls a subscription URL using the given device identity and returns the
// parsed nodes plus the panel's profile title (used to name the subscription).
// proxyAddr (host:port), when non-empty, routes the request through that SOCKS5
// proxy — useful when the panel host itself is censored.
func Fetch(device store.Device, subURL, proxyAddr string) ([]store.Node, string, error) {
	subURL = strings.TrimSpace(subURL)
	if subURL == "" {
		return nil, "", errors.New("empty subscription URL")
	}

	req, err := http.NewRequest(http.MethodGet, subURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", firstNonEmpty(device.UA, store.DefaultUA))
	req.Header.Set("X-Hwid", device.HWID)
	req.Header.Set("X-Device-Os", device.OS)
	req.Header.Set("X-Ver-Os", device.OSVer)
	req.Header.Set("X-Device-Model", device.Model)
	req.Header.Set("X-Device-Locale", device.Locale)

	tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxyAddr = strings.TrimSpace(proxyAddr); proxyAddr != "" {
		d, err := proxy.SOCKS5("tcp", proxyAddr, nil, &net.Dialer{Timeout: 15 * time.Second})
		if err != nil {
			return nil, "", fmt.Errorf("fetch proxy %s: %w", proxyAddr, err)
		}
		tr = &http.Transport{DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return d.Dial(network, addr)
		}}
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	title := decodeTitle(resp.Header.Get("profile-title"))

	if strings.EqualFold(resp.Header.Get("x-hwid-max-devices-reached"), "true") ||
		strings.EqualFold(resp.Header.Get("x-hwid-limit"), "true") {
		return nil, title, ErrDeviceLimit
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, title, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, title, fmt.Errorf("panel returned HTTP %d: %s", resp.StatusCode, snippet(body))
	}

	nodes := Parse(body)
	if isPlaceholderOnly(nodes) {
		// No real servers => "App not supported" / device-limit placeholder.
		return nil, title, ErrDeviceLimit
	}
	return nodes, title, nil
}

// ValidSubURL reports whether s is a usable subscription URL (http/https with a
// host and no embedded whitespace/control chars — guards against pasted junk).
func ValidSubURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// decodeTitle decodes a Remnawave `profile-title: base64:<...>` header.
func decodeTitle(h string) string {
	h = strings.TrimPrefix(strings.TrimSpace(h), "base64:")
	if h == "" {
		return ""
	}
	if dec, err := base64.StdEncoding.DecodeString(h); err == nil {
		return strings.TrimSpace(string(dec))
	}
	return ""
}

// Parse turns a raw subscription body into nodes. It accepts the shapes this
// panel emits: a base64 (or plain) newline list of proxy URIs, or the Happ
// xray-JSON array of profiles whose "remarks" are the friendly node names.
func Parse(body []byte) []store.Node {
	if nodes := parseURIList(decodeMaybeBase64(body)); len(nodes) > 0 {
		return nodes
	}
	if nodes := parseURIList(body); len(nodes) > 0 {
		return nodes
	}
	if nodes := parseXrayProfiles(body); len(nodes) > 0 {
		return nodes
	}
	return nil
}

func parseURIList(data []byte) []store.Node {
	var out []store.Node
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if n, ok := parseURI(line); ok {
			out = append(out, n)
		}
	}
	return out
}

func parseURI(uri string) (store.Node, bool) {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return store.Node{}, false
	}
	scheme := strings.ToLower(uri[:i])
	switch scheme {
	case "vless", "trojan", "ss":
		u, err := url.Parse(uri)
		if err != nil {
			return store.Node{}, false
		}
		host := u.Hostname()
		if host == "" || host == "0.0.0.0" {
			return store.Node{}, false
		}
		port, _ := strconv.Atoi(u.Port())
		name, _ := url.QueryUnescape(u.Fragment)
		name = strings.TrimSpace(name)
		n := store.Node{
			Raw:       uri,
			Name:      name,
			Protocol:  scheme,
			Server:    host,
			Port:      port,
			Whitelist: IsBypass(name),
		}
		// Build the xray outbound so URI-list nodes are actually usable (probed
		// and chainable), not just listed. The embedded core only speaks vless.
		if scheme == "vless" {
			if ob, err := xray.VlessToOutbound(uri, "proxy"); err == nil {
				n.Outbound = ob
			}
		}
		return n, true
	default:
		return store.Node{}, false
	}
}

// parseXrayProfiles parses the Happ xray subscription: a JSON array where each
// element is a full xray config for ONE node, carrying a "remarks" friendly name
// and a primary "proxy" outbound. Auto-select meta-profiles (which bundle many
// proxy-N outbounds) are skipped — we do our own selection.
func parseXrayProfiles(body []byte) []store.Node {
	type lightOutbound struct {
		Tag      string `json:"tag"`
		Protocol string `json:"protocol"`
		Settings struct {
			Vnext []struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
			} `json:"vnext"`
		} `json:"settings"`
	}
	type profile struct {
		Remarks   string            `json:"remarks"`
		Outbounds []json.RawMessage `json:"outbounds"`
	}

	var profs []profile
	if err := json.Unmarshal(body, &profs); err != nil {
		var one profile
		if err := json.Unmarshal(body, &one); err != nil {
			return nil
		}
		profs = []profile{one}
	}

	isProxyProto := func(p string) bool {
		switch p {
		case "vless", "trojan", "vmess", "shadowsocks":
			return true
		}
		return false
	}

	var out []store.Node
	for _, p := range profs {
		name := strings.TrimSpace(p.Remarks)
		if IsAuto(name) {
			continue // auto-select group, not a single node
		}
		// Pick the primary proxy outbound (prefer tag "proxy"); count real
		// proxy outbounds to detect bundled/meta profiles.
		var primary json.RawMessage
		var chosen lightOutbound
		proxyCount := 0
		for _, raw := range p.Outbounds {
			var lo lightOutbound
			if err := json.Unmarshal(raw, &lo); err != nil {
				continue
			}
			if !isProxyProto(lo.Protocol) {
				continue
			}
			proxyCount++
			if primary == nil || lo.Tag == "proxy" {
				primary = raw
				chosen = lo
			}
		}
		if primary == nil || proxyCount > 1 {
			continue
		}
		var addr string
		var port int
		if len(chosen.Settings.Vnext) > 0 {
			addr = chosen.Settings.Vnext[0].Address
			port = chosen.Settings.Vnext[0].Port
		}
		if addr == "" || addr == "0.0.0.0" {
			continue
		}
		out = append(out, store.Node{
			Name:      name,
			Protocol:  chosen.Protocol,
			Server:    addr,
			Port:      port,
			Whitelist: IsBypass(name),
			Outbound:  primary,
		})
	}
	return out
}

// IsBypass reports whether a node name belongs to the whitelist/bypass pool
// (Remnawave "ОБХОД" nodes). IsAuto reports auto-select meta groups.
func IsBypass(name string) bool {
	u := strings.ToUpper(name)
	return !IsAuto(name) && (strings.Contains(u, "ОБХОД") || strings.Contains(name, "🐾"))
}

func IsAuto(name string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(name)), "АВТО")
}

// decodeMaybeBase64 returns the base64-decoded bytes if body looks like a base64
// blob wrapping proxy URIs; otherwise it returns body unchanged.
func decodeMaybeBase64(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if dec, err := enc.DecodeString(s); err == nil && strings.Contains(string(dec), "://") {
			return dec
		}
	}
	return b
}

func isPlaceholderOnly(nodes []store.Node) bool {
	if len(nodes) == 0 {
		return true
	}
	for _, n := range nodes {
		if n.Server != "0.0.0.0" && n.Server != "" {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
