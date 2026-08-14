// Package tun brings clashvless's TUN mode up and down at the OS level. xray's
// tun inbound (living in the separate "bridge" instance) creates and owns the
// network device; this package does everything around it: assign the interface
// address, move the default route onto it, bypass the proxy server IPs (so xray's
// own connection to the exit doesn't loop back into the tunnel), and point DNS at
// a resolver whose queries ride the tunnel. Down() restores the original routing
// and DNS.
//
// Platforms: Linux (iproute2) and Windows (netsh/route). Other OSes get a stub
// so the app still builds everywhere.
package tun

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// Config describes a TUN bring-up.
type Config struct {
	Name      string   // device name (created by xray's tun inbound)
	Addr      string   // interface address CIDR, e.g. "198.18.0.1/30"
	MTU       int      // informational on most OSes (xray sets it)
	DNS       string   // resolver to set system-wide
	DNSDirect bool     // reach DNS off-tun (bypass route) instead of through the tunnel
	Mark      int32    // SO_MARK xray stamps on its sockets; Linux steers marked traffic off-tun (0 = none)
	ServerIPs []net.IP // Windows/macOS: server IPs to route direct (Linux uses Mark instead)
}

// Manager owns one TUN bring-up and restores the OS on Down. Methods are
// serialized, so it is safe to call from the supervisor goroutine and a toggle.
type Manager struct {
	logf func(string, ...any)

	mu  sync.Mutex
	up  bool
	cfg Config

	// captured for teardown (which fields matter varies by OS)
	origGW      string          // original default gateway
	origDev     string          // original egress device (Linux/macOS)
	savedResolv []byte          // Linux: original /etc/resolv.conf (plain file) · macOS: prior DNS servers
	resolvLink  bool            // Linux: systemd-resolved path taken
	dnsService  string          // macOS: network service whose DNS we changed
	bypass      map[string]bool // server IPs we routed around the tunnel
}

// New builds a Manager. logf (may be nil) receives progress lines.
func New(logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{logf: logf, bypass: map[string]bool{}}
}

// IsUp reports whether TUN mode is currently applied.
func (m *Manager) IsUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.up
}

// Up applies TUN mode: waits for the device, assigns the address, routes the
// default through it, bypasses the server IPs, and sets DNS. Idempotent.
func (m *Manager) Up(cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.up {
		return nil
	}
	if err := m.osUp(cfg); err != nil {
		return err
	}
	m.up = true
	return nil
}

// Down restores routes and DNS. Safe to call when not up.
func (m *Manager) Down() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.up {
		return nil
	}
	err := m.osDown()
	m.up = false
	return err
}

// cidrIPMask splits "198.18.0.1/30" into a host IP ("198.18.0.1") and a dotted
// netmask ("255.255.255.252") — the form Windows' netsh/route want.
func cidrIPMask(cidr string) (ip, mask string, err error) {
	i, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", err
	}
	return i.String(), net.IP(n.Mask).String(), nil
}

// waitForInterface blocks until an interface named name exists or timeout
// elapses. xray's tun inbound creates the device asynchronously as its instance
// starts, so callers race it.
func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := net.InterfaceByName(name); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tun device %q did not appear within %s", name, timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}
