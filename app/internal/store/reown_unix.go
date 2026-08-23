//go:build !windows

package store

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// invokerIDs resolves the uid/gid an elevated daemon should hand its created
// files back to. Two sources, in order:
//
//  1. sudo's SUDO_UID/SUDO_GID — set by `sudo clashvless run`.
//  2. the owner of the config tree — a systemd unit runs as root with no SUDO_*
//     env, but the state lives under the user's home; we walk up from refPath to
//     the nearest existing ancestor owned by a non-root user and adopt it.
//
// ok is false when nothing non-root is found (e.g. state genuinely under /root).
func invokerIDs(refPath string) (uid, gid int, ok bool) {
	if u, err := strconv.Atoi(os.Getenv("SUDO_UID")); err == nil && u != 0 {
		g, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
		if err2 != nil {
			g = u
		}
		return u, g, true
	}
	for p := refPath; ; {
		if fi, err := os.Stat(p); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
				return int(st.Uid), int(st.Gid), true
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, 0, false
		}
		p = parent
	}
}
