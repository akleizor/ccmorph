//go:build !darwin

package util

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// sysctlProcessTable is the darwin reader; elsewhere the ps path is all there is.
func sysctlProcessTable() (map[int]psRow, bool) { return nil, false }

// ProcStartToken is the kernel's own mark of when pid started: this boot's id
// and the process's start in clock ticks since boot, field 22 of
// /proc/<pid>/stat. Ticks since boot are not the wall clock, so no step moves
// them, and the boot id keeps a later boot's process apart. False where there
// is no /proc, or the pid is gone.
// Exported for a caller that keeps a process's identity in a record (#926:
// the rc-hosts supervisor stamps its pid and start into hosted.json), which
// must treat false as "not this process", never as a match.
func ProcStartToken(pid int) (string, bool) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", false
	}
	return strings.TrimSpace(string(boot)) + ":" + f[19], true
}

// imageOf is pid's executable path and argv, from /proc/<pid>/exe and
// /proc/<pid>/cmdline; false when the command line cannot be read, an
// unreadable executable link leaving the path empty.
func imageOf(pid int) (exe string, argv []string, ok bool) {
	argv, ok = argvOf(pid)
	if !ok {
		return "", nil, false
	}
	if p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		exe = p
	}
	return exe, argv, true
}

// A just-exec'd process reaches /proc without its command line for a moment:
// the kernel closes the exec-status pipe Start waits on before it publishes
// the new argv, so the parent that started the pid can read the file still
// empty. Read it again over that moment, briefly, so a launch does not read a
// bridge it just started as dead and start a second.
const (
	argvOfReads = 20
	argvOfPause = time.Millisecond
)

// argvOf is pid's argv with its argument boundaries, from /proc/<pid>/cmdline,
// whose arguments are NUL-ended; false where there is no /proc, the pid is gone
// or the command line is empty.
func argvOf(pid int) ([]string, bool) {
	path := "/proc/" + strconv.Itoa(pid) + "/cmdline"
	for i := 0; i < argvOfReads; i++ {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		if len(b) > 0 {
			return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), true
		}
		if i == argvOfReads-1 || zombie(pid) {
			break
		}
		time.Sleep(argvOfPause)
	}
	return nil, false
}

// zombie is whether pid has exited and waits for its parent to reap it: its
// command line is empty and will not come back, so there is nothing to wait for.
func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}
