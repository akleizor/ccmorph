//go:build !darwin

package util

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// sysctlProcessTable is the darwin reader; elsewhere the ps path is all there is.
func sysctlProcessTable() (map[int]psRow, bool) { return nil, false }

// procStartToken is the kernel's own mark of when pid started: this boot's id
// and the process's start in clock ticks since boot, field 22 of
// /proc/<pid>/stat. Ticks since boot are not the wall clock, so no step moves
// them, and the boot id keeps a later boot's process apart. False where there
// is no /proc, or the pid is gone.
func procStartToken(pid int) (string, bool) {
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

// argvOf is pid's argv with its argument boundaries, from /proc/<pid>/cmdline,
// whose arguments are NUL-ended; false where there is no /proc, the pid is gone
// or the command line is empty.
func argvOf(pid int) ([]string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), true
}
