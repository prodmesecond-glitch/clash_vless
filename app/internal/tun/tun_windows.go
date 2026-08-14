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

	// default through the tun via two /1 halves (win by specificity)
	if err := m.run("route", "add", "0.0.0.0", "mask", "128.0.0.0", tunIP, "if", tunIdx, "metric", "1"); err != nil {
		m.osDown()
		return err
	}
	if err := m.run("route", "add", "128.0.0.0", "mask", "128.0.0.0", tunIP, "if", tunIdx, "metric", "1"); err != nil {
		m.osDown()
		return err
	}

	if cfg.DNS != "" {
		m.runSoft("netsh", "interface", "ip", "set", "dns", "name="+cfg.Name, "static", cfg.DNS)
	}
	m.logf("tun: up — default routed through %s (IPv6 is not tunneled)", cfg.Name)
	return nil
}

func (m *Manager) osDown() error {
	m.runSoft("route", "delete", "0.0.0.0", "mask", "128.0.0.0")
	m.runSoft("route", "delete", "128.0.0.0", "mask", "128.0.0.0")
	m.reconcileBypass(nil)
	if m.cfg.DNS != "" {
		m.runSoft("netsh", "interface", "ip", "set", "dns", "name="+m.cfg.Name, "dhcp")
	}
	m.logf("tun: down — routing and DNS restored")
	return nil
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
