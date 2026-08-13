//go:build linux

package tun

import (
	"fmt"
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
	// the real default stays for the marked table's via-gateway).
	if err := m.run("ip", "route", "add", "0.0.0.0/1", "dev", cfg.Name); err != nil {
		m.osDown()
		return err
	}
	if err := m.run("ip", "route", "add", "128.0.0.0/1", "dev", cfg.Name); err != nil {
		m.osDown()
		return err
	}

	if cfg.DNS != "" {
		m.setDNS(cfg.DNS)
	}
	m.logf("tun: up — default routed through %s (IPv6 is not tunneled; disable it if leaks matter)", cfg.Name)
	return nil
}

func (m *Manager) osDown() error {
	name := m.cfg.Name
	m.runSoft("ip", "route", "del", "0.0.0.0/1", "dev", name)
	m.runSoft("ip", "route", "del", "128.0.0.0/1", "dev", name)
	mark := strconv.Itoa(int(m.cfg.Mark))
	m.runSoft("ip", "rule", "del", "fwmark", mark, "table", fwTable)
	m.runSoft("ip", "route", "flush", "table", fwTable)
	m.restoreDNS()
	m.logf("tun: down — routing and DNS restored")
	return nil
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
