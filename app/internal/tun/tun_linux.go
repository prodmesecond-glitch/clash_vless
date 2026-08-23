//go:build linux

package tun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	fwTable    = "51820" // private routing table for marked (xray) traffic
	fwRulePrio = "100"   // ip-rule priority: after local(0), before main(32766)
)

// Supported reports that TUN mode is implemented on this OS.
func Supported() bool { return true }

// Privileged reports whether we can create/configure a TUN (needs root / CAP_NET_ADMIN).
func Privileged() bool { return os.Geteuid() == 0 }

// DefaultName is the TUN device name when none is configured. Linux accepts any.
func DefaultName() string { return "clashvless0" }

// FwMark is the SO_MARK xray stamps on its sockets so policy routing can steer
// them around the tunnel. Linux-only; other OSes return 0 (they bypass by route).
func FwMark() int32 { return 0x1a2b }

// UplinkDevice is the real egress interface (the default route's device). xray
// SO_BINDTODEVICEs its own sockets onto it so they stay off the tunnel even when
// an nftables ruleset (Docker/firewalld) strips the fwmark and the marked
// packets would otherwise loop back into the tun. "" if undeterminable.
func UplinkDevice() string { _, dev, _ := defaultRoute(); return dev }

// RemoveDevice deletes a leftover TUN device by name (idempotent) so a fresh
// bring-up isn't blocked by "device or resource busy" when the kernel is still
// releasing the old one after a quick off→on. No-op if the device is absent.
func RemoveDevice(name string) {
	if name != "" {
		_ = exec.Command("ip", "link", "del", name).Run()
	}
}

// SystemResolver returns the resolver the host used before TUN (the first
// non-loopback IPv4 nameserver in /etc/resolv.conf). This is what DIRECT-mode
// DNS adopts so queries keep working on the real network — no public default,
// which a corporate LAN often firewalls. Falls back to systemd-resolved's real
// upstream (via resolvectl) when resolv.conf is just the 127.0.0.53 stub.
// Returns "" if none can be determined.
func SystemResolver() string {
	if b, err := os.ReadFile(resolvConf); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(ln)
			if len(f) >= 2 && f[0] == "nameserver" {
				if ip := net.ParseIP(f[1]); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
					return f[1]
				}
			}
		}
	}
	// resolv.conf was the loopback stub — ask systemd-resolved for the upstream.
	if out, err := exec.Command("resolvectl", "dns").Output(); err == nil {
		for _, tok := range strings.Fields(string(out)) {
			if ip := net.ParseIP(strings.TrimRight(tok, ",")); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return ""
}

func (m *Manager) run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runSoft runs a command and only logs failure (used for idempotent teardown /
// "route may already exist" cases).
func (m *Manager) runSoft(name string, args ...string) {
	if err := m.run(name, args...); err != nil {
		m.logf("tun: %v", err)
	}
}

func (m *Manager) osUp(cfg Config) error {
	m.cfg = cfg
	if err := waitForInterface(cfg.Name, 6*time.Second); err != nil {
		return err
	}
	gw, dev, err := defaultRoute()
	if err != nil {
		return fmt.Errorf("capture default route: %w (need an active uplink before enabling TUN)", err)
	}
	m.origGW, m.origDev = gw, dev
	m.logf("tun: original default via %s dev %s", gw, dev)

	// interface address + link up (xray already brought it up; make sure)
	m.runSoft("ip", "addr", "add", cfg.Addr, "dev", cfg.Name)
	if err := m.run("ip", "link", "set", "dev", cfg.Name, "up"); err != nil {
		return err
	}

	// Policy routing: xray marks its own sockets (cfg.Mark), so its connections
	// — the live exit AND every failover probe — get steered out the real uplink
	// via a private table, escaping the tunnel. This replaces per-server /32
	// bypass routes entirely, so the main table stays clean and probes still work.
	mark := strconv.Itoa(int(cfg.Mark))
	m.runSoft("ip", "rule", "del", "fwmark", mark, "table", fwTable) // clear any stale rule
	m.runSoft("ip", "route", "flush", "table", fwTable)
	if err := m.run("ip", "route", "add", "default", "via", gw, "dev", dev, "table", fwTable); err != nil {
		m.osDown()
		return err
	}
	if err := m.run("ip", "rule", "add", "fwmark", mark, "table", fwTable, "priority", fwRulePrio); err != nil {
		m.osDown()
		return err
	}

	// Override the default with two /1 routes through the tun (win by specificity;
	// the real default stays for the marked table's via-gateway). `replace` (not
	// `add`) so a leftover half from a prior daemon that died without clean
	// teardown — possibly pointing at a now-dead device — doesn't block bring-up.
	if err := m.run("ip", "route", "replace", "0.0.0.0/1", "dev", cfg.Name); err != nil {
		m.osDown()
		return err
	}
	if err := m.run("ip", "route", "replace", "128.0.0.0/1", "dev", cfg.Name); err != nil {
		m.osDown()
		return err
	}

	// LAN bypass: punch the private/link-local/multicast ranges back out to the
	// real network (more specific than the /1 halves win), so local-only resources
	// stay reachable. Our equivalent of Throne's sing-tun route_exclude_address.
	if cfg.BypassLAN {
		for _, c := range LANBypassRanges.ViaGateway {
			m.runSoft("ip", "route", "add", c, "via", gw, "dev", dev)
		}
		for _, c := range LANBypassRanges.OnLink {
			m.runSoft("ip", "route", "add", c, "dev", dev, "scope", "link")
		}
		m.logf("tun: LAN bypass on — private ranges kept off-tun (local-only resources reachable)")
	}

	// Block global-unicast IPv6: the tunnel is IPv4-only, so without this a
	// dual-stack app happy-eyeballs out over v6 and leaks the real address. An
	// "unreachable" route fails the v6 connect fast so it falls back to the
	// tunneled v4 (link-local/ULA are untouched).
	if cfg.BlockIPv6 {
		m.runSoft("ip", "-6", "route", "add", "unreachable", IPv6BlockRange)
		m.logf("tun: IPv6 blocked (%s unreachable) — no v6 leak past the IPv4-only tunnel", IPv6BlockRange)
	}

	if cfg.DNS != "" {
		if cfg.DNSDirect {
			// Pin the resolver off the tunnel (its own /32 out the real uplink) so
			// DNS answers during bootstrap instead of looping into a not-yet-up
			// exit — breaks the domain-named-node deadlock. Trades DNS privacy.
			m.runSoft("ip", "route", "add", cfg.DNS+"/32", "via", gw, "dev", dev)
			m.logf("tun: DNS %s → DIRECT (off-tun via %s) — resolves on the real network", cfg.DNS, dev)
		}
		m.setDNS(cfg.DNS)
	}
	m.logf("tun: up — default routed through %s (IPv6 blocked)", cfg.Name)
	return nil
}

func (m *Manager) osDown() error {
	name := m.cfg.Name
	m.runSoft("ip", "route", "del", "0.0.0.0/1", "dev", name)
	m.runSoft("ip", "route", "del", "128.0.0.0/1", "dev", name)
	if m.cfg.BypassLAN {
		for _, c := range LANBypassRanges.ViaGateway {
			m.runSoft("ip", "route", "del", c, "via", m.origGW, "dev", m.origDev)
		}
		for _, c := range LANBypassRanges.OnLink {
			m.runSoft("ip", "route", "del", c, "dev", m.origDev, "scope", "link")
		}
	}
	if m.cfg.BlockIPv6 {
		m.runSoft("ip", "-6", "route", "del", "unreachable", IPv6BlockRange)
	}
	mark := strconv.Itoa(int(m.cfg.Mark))
	m.runSoft("ip", "rule", "del", "fwmark", mark, "table", fwTable)
	m.runSoft("ip", "route", "flush", "table", fwTable)
	if m.cfg.DNSDirect && m.cfg.DNS != "" {
		m.runSoft("ip", "route", "del", m.cfg.DNS+"/32", "via", m.origGW, "dev", m.origDev)
	}
	m.restoreDNS()
	m.logf("tun: down — routing and DNS restored")
	return nil
}

// osReassert re-installs the default-capture halves if a network event wiped
// them (the /1 routes vanish but the tun device stays up → a silent leak out the
// real uplink). It checks whether traffic to a public address in each half still
// egresses via our device; if not, it re-adds both halves. Returns true only
// when it actually repaired the capture, so the caller logs just the repair.
func (m *Manager) osReassert() (bool, error) {
	if m.captureIntact() {
		return false, nil
	}
	// replace so a stale/partial entry doesn't make `add` fail with "File exists"
	m.runSoft("ip", "route", "del", "0.0.0.0/1", "dev", m.cfg.Name)
	m.runSoft("ip", "route", "del", "128.0.0.0/1", "dev", m.cfg.Name)
	if err := m.run("ip", "route", "add", "0.0.0.0/1", "dev", m.cfg.Name); err != nil {
		return true, err
	}
	if err := m.run("ip", "route", "add", "128.0.0.0/1", "dev", m.cfg.Name); err != nil {
		return true, err
	}
	return true, nil
}

// osGatewayChanged compares the current real default (gateway + egress device)
// to what osUp captured; a difference means the uplink moved to a new network.
func (m *Manager) osGatewayChanged() (bool, string) {
	gw, dev, err := defaultRoute()
	if err != nil || gw == "" {
		return false, "" // mid-transition / no uplink — don't act
	}
	if gw == m.origGW && dev == m.origDev {
		return false, ""
	}
	return true, fmt.Sprintf("%s dev %s (was %s dev %s)", gw, dev, m.origGW, m.origDev)
}

// osReapply re-points the gateway-dependent routes at the current uplink after a
// network change: the fwmark table's default (which steers xray's own marked
// sockets off-tun), the LAN bypass ranges, and the DNS-direct /32 all pointed at
// the old gateway/device. No per-server bypass on Linux (that's the fwmark
// table's job). Keeps the device up and re-asserts the capture halves. The
// caller re-decorates xray (SO_BINDTODEVICE to the new dev) and rebuilds live.
func (m *Manager) osReapply() error {
	gw, dev, err := defaultRoute()
	if err != nil || gw == "" {
		return fmt.Errorf("no uplink yet")
	}
	if m.cfg.BypassLAN {
		for _, c := range LANBypassRanges.ViaGateway {
			m.runSoft("ip", "route", "del", c, "via", m.origGW, "dev", m.origDev)
		}
		for _, c := range LANBypassRanges.OnLink {
			m.runSoft("ip", "route", "del", c, "dev", m.origDev, "scope", "link")
		}
	}
	if m.cfg.DNSDirect && m.cfg.DNS != "" {
		m.runSoft("ip", "route", "del", m.cfg.DNS+"/32", "via", m.origGW, "dev", m.origDev)
	}
	m.origGW, m.origDev = gw, dev
	// re-point the fwmark table default (xray's off-tun path) at the new uplink
	m.runSoft("ip", "route", "replace", "default", "via", gw, "dev", dev, "table", fwTable)
	if m.cfg.BypassLAN {
		for _, c := range LANBypassRanges.ViaGateway {
			m.runSoft("ip", "route", "add", c, "via", gw, "dev", dev)
		}
		for _, c := range LANBypassRanges.OnLink {
			m.runSoft("ip", "route", "add", c, "dev", dev, "scope", "link")
		}
	}
	if m.cfg.DNSDirect && m.cfg.DNS != "" {
		m.runSoft("ip", "route", "add", m.cfg.DNS+"/32", "via", gw, "dev", dev)
	}
	_, err = m.osReassert() // re-assert the capture halves if the transition dropped them
	return err
}

// captureIntact reports whether the default-capture halves still steer traffic
// into the tun — probes one public address in each /1 half and checks the kernel
// would egress both via our device.
func (m *Manager) captureIntact() bool {
	return routeDev("1.1.1.1") == m.cfg.Name && routeDev("200.0.0.1") == m.cfg.Name
}

// routeDev returns the egress device the kernel would use for dst via
// `ip route get`; "" on error.
func routeDev(dst string) string {
	out, err := exec.Command("ip", "route", "get", dst).Output()
	if err != nil {
		return ""
	}
	// "1.1.1.1 dev clashvless0 src ... " — take the token after "dev".
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

// defaultRoute returns the current IPv4 default gateway and egress device.
func defaultRoute() (gw, dev string, err error) {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return "", "", err
	}
	// "default via 192.168.1.1 dev eth0 proto static ..." — take the first line.
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "via":
			gw = f[i+1]
		case "dev":
			dev = f[i+1]
		}
	}
	if gw == "" || dev == "" {
		return "", "", fmt.Errorf("no IPv4 default route")
	}
	return gw, dev, nil
}

// --- DNS -------------------------------------------------------------------

const resolvConf = "/etc/resolv.conf"

// setDNS points the system at server, whose queries then ride the tunnel. It
// handles both a plain /etc/resolv.conf and a systemd-resolved symlink.
func (m *Manager) setDNS(server string) {
	if fi, err := os.Lstat(resolvConf); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		m.resolvLink = true
		m.runSoft("resolvectl", "dns", m.cfg.Name, server)
		m.runSoft("resolvectl", "domain", m.cfg.Name, "~.")
		return
	}
	if b, err := os.ReadFile(resolvConf); err == nil {
		m.savedResolv = b
	}
	if err := os.WriteFile(resolvConf, []byte("# clashvless TUN\nnameserver "+server+"\n"), 0644); err != nil {
		m.logf("tun: set DNS: %v", err)
	}
}

func (m *Manager) restoreDNS() {
	if m.resolvLink {
		m.runSoft("resolvectl", "revert", m.cfg.Name)
		m.resolvLink = false
		return
	}
	if m.savedResolv != nil {
		if err := os.WriteFile(resolvConf, m.savedResolv, 0644); err != nil {
			m.logf("tun: restore DNS: %v", err)
		}
		m.savedResolv = nil
	}
}
