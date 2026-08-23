//go:build !linux && !windows && !darwin

package tun

import "errors"

// Supported reports that TUN mode is not implemented on this OS.
func Supported() bool { return false }

// Privileged is meaningless here; TUN is unsupported.
func Privileged() bool { return false }

// DefaultName is unused on unsupported OSes.
func DefaultName() string { return "clashvless0" }

// FwMark is unused on unsupported OSes.
func FwMark() int32 { return 0 }

// UplinkDevice is unused off Linux (bypass is by explicit per-server routes).
func UplinkDevice() string { return "" }

// SystemResolver is unused on unsupported OSes.
func SystemResolver() string { return "" }

// RemoveDevice is unused on unsupported OSes.
func RemoveDevice(string) {}

func (m *Manager) osUp(cfg Config) error {
	return errors.New("TUN mode is only supported on Linux, Windows, and macOS")
}

func (m *Manager) osDown() error { return nil }

func (m *Manager) osReassert() (bool, error) { return false, nil }

func (m *Manager) osGatewayChanged() (bool, string) { return false, "" }

func (m *Manager) osReapply() error { return nil }
