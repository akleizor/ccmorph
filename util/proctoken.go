package util

// ProcStartToken is the kernel's own mark of when pid started, for a caller
// outside this package that keeps a process's identity in a record (#926: the
// rc-hosts supervisor stamps its pid and start into hosted.json). Linux: the
// boot id and the start tick since boot; macOS: p_starttime as seconds and
// microseconds; the same value procStartToken keeps for the stale sweep. False
// when no process holds pid, or the mark cannot be read: the caller must treat
// that as "not this process", never as a match.
func ProcStartToken(pid int) (string, bool) { return procStartToken(pid) }
