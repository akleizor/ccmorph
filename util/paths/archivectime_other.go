//go:build !linux && !darwin

package paths

import "syscall"

// archiveCtime has no portable field outside linux and darwin; the identity
// still carries the inode, size and mtime, so a rewrite that preserves those
// is the one case this platform cannot see.
func archiveCtime(st *syscall.Stat_t) int64 {
	return 0
}
