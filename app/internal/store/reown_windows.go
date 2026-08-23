//go:build windows

package store

// invokerIDs is a no-op on Windows: there is no sudo/systemd reown model, and
// os.Chown is a no-op there anyway. ReownToInvoker never acts.
func invokerIDs(refPath string) (uid, gid int, ok bool) { return 0, 0, false }
