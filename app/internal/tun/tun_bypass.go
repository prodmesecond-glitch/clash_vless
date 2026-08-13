//go:build windows || darwin

package tun

import "net"

// reconcileBypass drives m.bypass toward want via the OS-specific addBypass /
// delBypass. Used on platforms without fwmark policy routing (Windows, macOS),
// where xray's own connections are kept off the tunnel with explicit per-server
// routes instead. Callers must hold m.mu. IPv4 only.
func (m *Manager) reconcileBypass(want []net.IP) {
	desired := map[string]bool{}
	for _, ip := range want {
		if ip.To4() != nil {
			desired[ip.String()] = true
		}
	}
	if m.bypass == nil {
		m.bypass = map[string]bool{}
	}
	for s := range m.bypass {
		if !desired[s] {
			m.delBypass(s)
			delete(m.bypass, s)
		}
	}
	for s := range desired {
		if !m.bypass[s] {
			if err := m.addBypass(s); err != nil {
				m.logf("tun: bypass %s: %v", s, err)
				continue
			}
			m.bypass[s] = true
		}
	}
	m.logf("tun: bypassing %d server IP(s)", len(m.bypass))
}
