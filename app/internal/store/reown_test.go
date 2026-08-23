//go:build !windows

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInvokerIDsSudo: SUDO_UID/GID win when present (the `sudo clashvless run` path).
func TestInvokerIDsSudo(t *testing.T) {
	t.Setenv("SUDO_UID", "1234")
	t.Setenv("SUDO_GID", "5678")
	uid, gid, ok := invokerIDs("/root/whatever")
	if !ok || uid != 1234 || gid != 5678 {
		t.Fatalf("invokerIDs sudo = (%d,%d,%v), want (1234,5678,true)", uid, gid, ok)
	}
}

// TestInvokerIDsConfigOwner: with no SUDO_* (the systemd path), fall back to the
// owner of the nearest existing ancestor — here the test's own dir, owned by us.
func TestInvokerIDsConfigOwner(t *testing.T) {
	os.Unsetenv("SUDO_UID")
	os.Unsetenv("SUDO_GID")
	dir := t.TempDir()
	deep := filepath.Join(dir, "clash_vless", "state.json") // does not exist yet
	uid, gid, ok := invokerIDs(deep)
	if !ok {
		t.Fatal("invokerIDs found no non-root owner for a temp path under our home")
	}
	if want := os.Getuid(); uid != want {
		t.Fatalf("invokerIDs uid = %d, want our uid %d", uid, want)
	}
	if want := os.Getgid(); gid != want {
		t.Fatalf("invokerIDs gid = %d, want our gid %d", gid, want)
	}
}
