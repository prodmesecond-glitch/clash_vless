//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// Supported reports that TUN mode is implemented on Windows.
//
// NOTE: the Windows backend is code-complete but has not been runtime-tested by
// the author. It needs wintun.dll next to the executable (xray's tun inbound
// loads it to create the Wintun adapter) and an elevated (Administrator) process.
func Supported() bool { return true }

// DefaultName is the TUN device name when none is configured. Wintun accepts any.
func DefaultName() string { return "clashvless0" }

// FwMark is 0 on Windows — no SO_MARK; xray's connections are kept off the tunnel
// with explicit per-server bypass routes instead.
func FwMark() int32 { return 0 }

// UplinkDevice is unused off Linux (bypass is by explicit per-server routes).
func UplinkDevice() string { return "" }

// SystemResolver has no /etc/resolv.conf equivalent to read on Windows, so
// DIRECT-mode DNS can't auto-adopt the system resolver here — the user sets one
// explicitly with `tun dns <ip>`. Returns "".
func SystemResolver() string { return "" }

// RemoveDevice is a no-op off Linux (wintun adapters are managed by xray).
func RemoveDevice(string) {}

// Privileged reports whether the process is running elevated (Administrators).
func Privileged() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY, 2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0, &sid)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)
	member, err := windows.GetCurrentProcessToken().IsMember(sid)
	return err == nil && member
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
	if err := waitForInterface(cfg.Name, 10*time.Second); err != nil {
		return err
	}
	iface, err := ifaceByName(cfg.Name)
	if err != nil {
		return err
	}
	tunIdx := strconv.Itoa(iface)
	tunIP, mask, err := cidrIPMask(cfg.Addr)
	if err != nil {
		return err
	}
	gw, err := defaultGateway()
	if err != nil {
		return fmt.Errorf("capture default gateway: %w (need an active uplink before enabling TUN)", err)
	}
	m.origGW = gw
	m.logf("tun: original default gateway %s; tun ifindex %s", gw, tunIdx)

	// interface address
	if err := m.run("netsh", "interface", "ip", "set", "address",
		"name="+cfg.Name, "static", tunIP, mask); err != nil {
		return err
	}

	// bypass the active exit's server IP via the real gateway (avoid the loop)
	m.bypass = map[string]bool{}
	m.reconcileBypass(cfg.ServerIPs)

	// default through the tun via two /1 halves (win by specificity). Delete any
	// leftover halves first (from a prior daemon that died without clean teardown)
	// so `add` isn't rejected as a duplicate.
	m.runSoft("route", "delete", "0.0.0.0", "mask", "128.0.0.0")
	m.runSoft("route", "delete", "128.0.0.0", "mask", "128.0.0.0")
	if err := m.run("route", "add", "0.0.0.0", "mask", "128.0.0.0", tunIP, "if", tunIdx, "metric", "1"); err != nil {
		m.osDown()
		return err
	}
	if err := m.run("route", "add", "128.0.0.0", "mask", "128.0.0.0", tunIP, "if", tunIdx, "metric", "1"); err != nil {
		m.osDown()
		return err
	}

	// LAN bypass: keep private/link-local/multicast ranges on the real network via
	// the gateway (like Throne's route_exclude_address) so local-only resources
	// stay reachable.
	if cfg.BypassLAN {
		for _, c := range append(append([]string{}, LANBypassRanges.ViaGateway...), LANBypassRanges.OnLink...) {
			if netIP, mask, err := cidrIPMask(c); err == nil {
				m.runSoft("route", "add", netIP, "mask", mask, m.origGW, "metric", "1")
			}
		}
		m.logf("tun: LAN bypass on — private ranges kept off-tun (local-only resources reachable)")
	}

	// Block IPv6: the tunnel is IPv4-only, so a dual-stack app would leak out over
	// v6. Route global-unicast v6 into the tun (best-effort — untested on Windows)
	// so it can't escape on the real network.
	if cfg.BlockIPv6 {
		m.runSoft("netsh", "interface", "ipv6", "add", "route", IPv6BlockRange, "interface="+cfg.Name)
		m.logf("tun: IPv6 routed into %s (best-effort v6 leak block)", cfg.Name)
	}

	if cfg.DNS != "" {
		m.runSoft("netsh", "interface", "ip", "set", "dns", "name="+cfg.Name, "static", cfg.DNS)
	}
	m.logf("tun: up — default routed through %s (IPv6 blocked)", cfg.Name)
	return nil
}

func (m *Manager) osDown() error {
	m.runSoft("route", "delete", "0.0.0.0", "mask", "128.0.0.0")
	m.runSoft("route", "delete", "128.0.0.0", "mask", "128.0.0.0")
	if m.cfg.BypassLAN {
		for _, c := range append(append([]string{}, LANBypassRanges.ViaGateway...), LANBypassRanges.OnLink...) {
			if netIP, _, err := cidrIPMask(c); err == nil {
				m.runSoft("route", "delete", netIP)
			}
		}
	}
	m.reconcileBypass(nil)
	if m.cfg.BlockIPv6 {
		m.runSoft("netsh", "interface", "ipv6", "delete", "route", IPv6BlockRange, "interface="+m.cfg.Name)
	}
	if m.cfg.DNS != "" {
		m.runSoft("netsh", "interface", "ip", "set", "dns", "name="+m.cfg.Name, "dhcp")
	}
	m.logf("tun: down — routing and DNS restored")
	return nil
}

// osReassert re-installs the default-capture halves if a network event wiped
// them (the /1 routes vanish but the adapter stays up → a silent leak out the
// real uplink). Best-effort/untested like the rest of this backend: it checks
// `route print` for both halves and re-adds any that are gone. Returns true only
// when it repaired something, so the caller logs just the repair.
func (m *Manager) osReassert() (bool, error) {
	if m.captureIntact() {
		return false, nil
	}
	iface, err := ifaceByName(m.cfg.Name)
	if err != nil {
		return false, err
	}
	tunIdx := strconv.Itoa(iface)
	tunIP, _, err := cidrIPMask(m.cfg.Addr)
	if err != nil {
		return false, err
	}
	m.runSoft("route", "delete", "0.0.0.0", "mask", "128.0.0.0")
	m.runSoft("route", "delete", "128.0.0.0", "mask", "128.0.0.0")
	if err := m.run("route", "add", "0.0.0.0", "mask", "128.0.0.0", tunIP, "if", tunIdx, "metric", "1"); err != nil {
		return true, err
	}
	if err := m.run("route", "add", "128.0.0.0", "mask", "128.0.0.0", tunIP, "if", tunIdx, "metric", "1"); err != nil {
		return true, err
	}
	return true, nil
}

// osGatewayChanged compares the current real default gateway to what osUp
// captured; a difference means the uplink moved to a new network. (This backend
// tracks only the gateway, not the egress ifindex.)
func (m *Manager) osGatewayChanged() (bool, string) {
	gw, err := defaultGateway()
	if err != nil || gw == "" {
		return false, "" // mid-transition / no uplink — don't act
	}
	if gw == m.origGW {
		return false, ""
	}
	return true, fmt.Sprintf("%s (was %s)", gw, m.origGW)
}

// osReapply re-points the gateway-dependent routes at the current uplink after a
// network change (per-server bypass + LAN ranges all pointed at the old gateway).
// Reuses cached cfg.ServerIPs (no DNS), keeps the adapter up, re-asserts the
// capture halves. Best-effort/untested like the rest of this backend.
func (m *Manager) osReapply() error {
	gw, err := defaultGateway()
	if err != nil || gw == "" {
		return fmt.Errorf("no uplink yet")
	}
	if m.cfg.BypassLAN {
		for _, c := range append(append([]string{}, LANBypassRanges.ViaGateway...), LANBypassRanges.OnLink...) {
			if netIP, _, err := cidrIPMask(c); err == nil {
				m.runSoft("route", "delete", netIP)
			}
		}
	}
	for s := range m.bypass { // per-server bypass points at the old gateway
		m.delBypass(s)
		delete(m.bypass, s)
	}
	m.origGW = gw
	m.reconcileBypass(m.cfg.ServerIPs)
	if m.cfg.BypassLAN {
		for _, c := range append(append([]string{}, LANBypassRanges.ViaGateway...), LANBypassRanges.OnLink...) {
			if netIP, mask, err := cidrIPMask(c); err == nil {
				m.runSoft("route", "add", netIP, "mask", mask, m.origGW, "metric", "1")
			}
		}
	}
	_, err = m.osReassert() // re-assert the capture halves if the transition dropped them
	return err
}

// captureIntact reports whether both default-capture halves are present in the
// routing table (`route print` shows a 0.0.0.0 and a 128.0.0.0 dest, each with a
// 128.0.0.0 mask).
func (m *Manager) captureIntact() bool {
	out, err := exec.Command("route", "print", "-4").Output()
	if err != nil {
		return true // can't tell — assume intact rather than churn the table
	}
	have0, have128 := false, false
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 || f[1] != "128.0.0.0" {
			continue
		}
		switch f[0] {
		case "0.0.0.0":
			have0 = true
		case "128.0.0.0":
			have128 = true
		}
	}
	return have0 && have128
}

// addBypass routes one server IP direct via the original gateway.
func (m *Manager) addBypass(ip string) error {
	return m.run("route", "add", ip, "mask", "255.255.255.255", m.origGW, "metric", "1")
}

func (m *Manager) delBypass(ip string) {
	m.runSoft("route", "delete", ip)
}

// ifaceByName returns the interface index for the named adapter.
func ifaceByName(name string) (int, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return ifi.Index, nil
}

// defaultGateway parses `route print -4` for the active 0.0.0.0/0 gateway with
// the lowest metric.
func defaultGateway() (string, error) {
	out, err := exec.Command("route", "print", "-4", "0.0.0.0").Output()
	if err != nil {
		return "", err
	}
	best, bestMetric := "", 1<<31-1
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 5 || f[0] != "0.0.0.0" || f[1] != "0.0.0.0" {
			continue
		}
		gw := f[2]
		metric, err := strconv.Atoi(f[len(f)-1])
		if err != nil {
			continue
		}
		if metric < bestMetric {
			best, bestMetric = gw, metric
		}
	}
	if best == "" {
		return "", fmt.Errorf("no IPv4 default route")
	}
	return best, nil
}
