//go:build darwin

package tun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Supported reports that TUN mode is implemented on macOS (tested + working).
//
// xray creates the utun device and assigns it a link-local point-to-point
// address itself; this backend only adds routes and DNS.
func Supported() bool { return true }

// Privileged reports whether we can create/configure a utun (needs root).
func Privileged() bool { return os.Geteuid() == 0 }

// DefaultName is the utun device name when none is configured. macOS requires a
// "utunN" name (xray rejects anything else); N must be free, so pick a high one.
func DefaultName() string { return "utun9" }

// FwMark is 0 on macOS — no SO_MARK; xray's connections are kept off the tunnel
// with explicit per-server bypass routes instead.
func FwMark() int32 { return 0 }

// UplinkDevice is unused off Linux (bypass is by explicit per-server routes).
func UplinkDevice() string { return "" }

// RemoveDevice is a no-op off Linux (utun devices are kernel-managed).
func RemoveDevice(string) {}

// SystemResolver returns the resolver the host used before TUN (first
// non-loopback IPv4 nameserver in /etc/resolv.conf, which macOS keeps in sync
// with the primary service). DIRECT-mode DNS adopts it so queries keep working
// on the real network. Returns "" if none can be determined.
func SystemResolver() string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[0] == "nameserver" {
			if ip := net.ParseIP(f[1]); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				return f[1]
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

	// xray already brought the utun up with a 169.254.10.2/30 point-to-point
	// address, so we only add routes here (no address assignment).

	// Bypass the active exit's server IP so xray's own connection to it escapes
	// the tunnel (otherwise it loops).
	m.bypass = map[string]bool{}
	m.reconcileBypass(cfg.ServerIPs)

	// Override the default with two /1 routes out the utun interface.
	if err := m.run("route", "-n", "add", "-net", "0.0.0.0/1", "-interface", cfg.Name); err != nil {
		m.osDown()
		return err
	}
	if err := m.run("route", "-n", "add", "-net", "128.0.0.0/1", "-interface", cfg.Name); err != nil {
		m.osDown()
		return err
	}

	// LAN bypass: keep private/link-local/multicast ranges on the real network
	// (like Throne's route_exclude_address) so local-only resources stay reachable.
	if cfg.BypassLAN {
		for _, c := range LANBypassRanges.ViaGateway {
			m.runSoft("route", "-n", "add", "-net", c, gw)
		}
		for _, c := range LANBypassRanges.OnLink {
			m.runSoft("route", "-n", "add", "-net", c, "-interface", dev)
		}
		m.logf("tun: LAN bypass on — private ranges kept off-tun (local-only resources reachable)")
	}

	// Block IPv6: the tunnel is IPv4-only, so a dual-stack app would happy-eyeballs
	// out over v6 and leak the real address. Turn v6 off on the uplink service so
	// everything falls back to the tunneled v4. (setv6off is reliable across macOS
	// versions; a reject inet6 route's syntax is not.)
	if cfg.BlockIPv6 {
		if svc := primaryService(m.origDev); svc != "" {
			m.v6Service = svc
			m.runSoft("networksetup", "-setv6off", svc)
			m.logf("tun: IPv6 blocked on %q — no v6 leak past the IPv4-only tunnel", svc)
		} else {
			m.logf("tun: IPv6 block requested but no primary service found for %s; v6 may leak", m.origDev)
		}
	}

	if cfg.DNS != "" {
		m.setDNS(cfg.DNS)
	}
	m.logf("tun: up — default routed through %s (IPv6 blocked)", cfg.Name)
	return nil
}

func (m *Manager) osDown() error {
	m.runSoft("route", "-n", "delete", "-net", "0.0.0.0/1")
	m.runSoft("route", "-n", "delete", "-net", "128.0.0.0/1")
	if m.cfg.BypassLAN {
		for _, c := range append(append([]string{}, LANBypassRanges.ViaGateway...), LANBypassRanges.OnLink...) {
			m.runSoft("route", "-n", "delete", "-net", c)
		}
	}
	m.reconcileBypass(nil)
	if m.v6Service != "" {
		m.runSoft("networksetup", "-setv6automatic", m.v6Service)
		m.v6Service = ""
	}
	m.restoreDNS()
	m.logf("tun: down — routing and DNS restored")
	return nil
}

// addBypass routes one server IP direct via the original gateway.
func (m *Manager) addBypass(ip string) error {
	return m.run("route", "-n", "add", "-host", ip, m.origGW)
}

func (m *Manager) delBypass(ip string) {
	m.runSoft("route", "-n", "delete", "-host", ip)
}

// defaultRoute returns the current IPv4 default gateway and egress interface via
// `route -n get default`.
func defaultRoute() (gw, dev string, err error) {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return "", "", err
	}
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "gateway:":
			gw = f[1]
		case "interface:":
			dev = f[1]
		}
	}
	if gw == "" || dev == "" {
		return "", "", fmt.Errorf("no IPv4 default route")
	}
	return gw, dev, nil
}

// --- DNS -------------------------------------------------------------------

// setDNS points the primary network service (the one on the original egress
// interface) at server, so its queries then ride the tunnel.
func (m *Manager) setDNS(server string) {
	svc := primaryService(m.origDev)
	if svc == "" {
		m.logf("tun: could not find the primary network service; leaving DNS unchanged")
		return
	}
	m.dnsService = svc
	if out, err := exec.Command("networksetup", "-getdnsservers", svc).Output(); err == nil {
		m.savedResolv = out // may be "There aren't any DNS Servers set…" — handled on restore
	}
	m.runSoft("networksetup", "-setdnsservers", svc, server)
}

func (m *Manager) restoreDNS() {
	if m.dnsService == "" {
		return
	}
	prev := strings.Fields(string(m.savedResolv))
	// networksetup prints a sentence when no servers are set; treat that as "clear".
	if len(prev) == 0 || strings.Contains(string(m.savedResolv), "aren't any") {
		m.runSoft("networksetup", "-setdnsservers", m.dnsService, "Empty")
	} else {
		m.runSoft("networksetup", append([]string{"-setdnsservers", m.dnsService}, prev...)...)
	}
	m.dnsService, m.savedResolv = "", nil
}

// primaryService returns the macOS network service name (e.g. "Wi-Fi") bound to
// the given BSD interface (e.g. "en0"), parsed from networksetup's service order.
func primaryService(iface string) string {
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return ""
	}
	// Blocks look like:
	//   (1) Wi-Fi
	//   (Hardware Port: Wi-Fi, Device: en0)
	name := ""
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "(") && strings.Contains(ln, ") ") && !strings.Contains(ln, "Hardware Port") {
			if i := strings.Index(ln, ") "); i >= 0 {
				name = strings.TrimSpace(ln[i+2:])
			}
			continue
		}
		if strings.Contains(ln, "Device: "+iface+")") {
			return name
		}
	}
	return ""
}
