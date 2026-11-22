//go:build linux

package paths

import "syscall"

// archiveCtime is the inode change time of a file, in Unix nanoseconds: the
// identity member a rewrite moves even when size and mtime are put back.
func archiveCtime(st *syscall.Stat_t) int64 {
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
