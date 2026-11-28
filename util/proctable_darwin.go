//go:build darwin

package util

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// One kinfo_proc record, as the kernel lays it out: 648 bytes on arm64 and
// amd64 alike, p_starttime (a Timeval: tv_sec int64, tv_usec int32) at 0, the
// pid at 40, the 17-byte comm at 243 and e_ppid at 560 (the offsets of
// golang.org/x/sys's KinfoProc, which this file deliberately does not depend
// on: the standard library is enough, and the repository ships no go.sum for a
// module to grow behind).
const (
	kinfoSize    = 648
	kinfoStart   = 0
	kinfoPID     = 40
	kinfoComm    = 243
	kinfoCommLen = 17
	kinfoPPID    = 560
)

// startTime is when the record's process started, from p_starttime; zero when
// the record carries no time.
func startTime(rec []byte) time.Time {
	sec := int64(binary.LittleEndian.Uint64(rec[kinfoStart:]))
	usec := int32(binary.LittleEndian.Uint32(rec[kinfoStart+8:]))
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, int64(usec)*1000)
}

// sysctlRaw is sysctlRead. A variable so a test can count the reads a launch or
// a sweep makes, as with sysctlTable.
var sysctlRaw = sysctlRead

// sysctlRead reads one sysctl into buf, sizing it first when buf is nil: the
// table, or false. The size pass and the read are two calls, so a table that
// grew in between is retried on a fresh size rather than read short.
func sysctlRead(mib []int32, buf []byte) ([]byte, bool) {
	if buf != nil {
		n := uintptr(len(buf))
		if _, _, e := syscall.Syscall6(syscall.SYS___SYSCTL,
			uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0); e != 0 {
			return nil, false
		}
		return buf[:n], true
	}
	for range 3 {
		n := uintptr(0)
		if _, _, e := syscall.Syscall6(syscall.SYS___SYSCTL,
			uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
			0, uintptr(unsafe.Pointer(&n)), 0, 0); e != 0 || n == 0 {
			return nil, false
		}
		if out, ok := sysctlRead(mib, make([]byte, n+n/8)); ok {
			return out, true
		}
	}
	return nil, false
}

// procImage is pid's executable path and argv from one kern.procargs2 read: the
// record carries argc, the path of the image the kernel ran, then the arguments
// as the process was started with them. False when it cannot be read (another
// user's process, a process that left between the two reads).
func procImage(pid int, buf []byte) (exe string, argv []string, ok bool) {
	b, ok := sysctlRaw([]int32{1, 49, int32(pid)}, buf)
	if !ok || len(b) < 4 {
		return "", nil, false
	}
	argc := int(binary.LittleEndian.Uint32(b))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0) // the exec path, then NUL padding
	if i < 0 {
		return "", nil, false
	}
	exe = string(rest[:i])
	rest = bytes.TrimLeft(rest[i:], "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		a, r, _ := bytes.Cut(rest, []byte{0})
		args = append(args, string(a))
		rest = r
	}
	return exe, args, true
}

// procArgv is a process's argv from kern.procargs2, one element per argument as
// the process was started with them; nil when it cannot be read (another
// user's process, a process that left between the two reads).
func procArgv(pid int, buf []byte) []string {
	_, argv, _ := procImage(pid, buf)
	return argv
}

// imageOf is pid's executable path and argv, from one read of the kernel's own
// record: the executable is what pid ran, not argv[0], which the program
// itself chooses.
func imageOf(pid int) (string, []string, bool) { return procImage(pid, nil) }

// procArgs is procArgv space-joined the way ps's command= column reads; ""
// when it cannot be read.
func procArgs(pid int, buf []byte) string { return strings.Join(procArgv(pid, buf), " ") }

// argvOf is pid's argv with its argument boundaries, read from the kernel; false
// when it cannot be read.
func argvOf(pid int) ([]string, bool) {
	argv := procArgv(pid, nil)
	return argv, len(argv) > 0
}

// sysctlProcessTable is processTable without ps, which a sandboxed Claude Code
// session may not exec even though the kernel's own process list is readable.
// False unless it places this process under its real parent, so a kinfo_proc
// layout these offsets do not describe falls back to ps instead of naming a
// wrong pid to kill.
func sysctlProcessTable() (map[int]psRow, bool) {
	raw, ok := sysctlRaw([]int32{1, 14, 0}, nil) // kern.proc.all
	if !ok || len(raw)%kinfoSize != 0 {
		return nil, false
	}
	argBuf := make([]byte, 1<<20) // kern.argmax; one buffer, reused per process
	rows := map[int]psRow{}
	for off := 0; off+kinfoSize <= len(raw); off += kinfoSize {
		rec := raw[off : off+kinfoSize]
		pid := int(int32(binary.LittleEndian.Uint32(rec[kinfoPID:])))
		ppid := int(int32(binary.LittleEndian.Uint32(rec[kinfoPPID:])))
		comm, _, _ := strings.Cut(string(rec[kinfoComm:kinfoComm+kinfoCommLen]), "\x00")
		rows[pid] = psRow{ppid: ppid, comm: comm, args: procArgs(pid, argBuf), start: startTime(rec)}
	}
	if r, ok := rows[os.Getpid()]; !ok || r.ppid != os.Getppid() {
		return nil, false
	}
	// A start time read from offsets this kernel does not use would be a wrong
	// time, and the stale sweep reaps on it. This process started in the past
	// and before now, so a self that reads as never started or as starting
	// later means the layout is not this one: keep the rows' identity and drop
	// every time to the cannot-tell zero the sweep keeps.
	if self := rows[os.Getpid()]; self.start.IsZero() || self.start.After(time.Now()) {
		for pid, r := range rows {
			r.start = time.Time{}
			rows[pid] = r
		}
	}
	return rows, true
}

// kinfoLayoutOK answers once per process whether this kernel's kinfo_proc is
// the layout these offsets describe: this process must read under its real
// parent with a start in the past. Its own record cannot move while it lives —
// a reparented process's ppid changes, but the layout the answer is about does
// not — so a sweep that reads many marks pays for the check once, not once per
// mark.
var kinfoLayoutOK = sync.OnceValue(func() bool {
	self, ok := sysctlRaw([]int32{1, 14, 1, int32(os.Getpid())}, nil)
	if !ok || len(self) != kinfoSize ||
		int(int32(binary.LittleEndian.Uint32(self[kinfoPID:]))) != os.Getpid() ||
		int(int32(binary.LittleEndian.Uint32(self[kinfoPPID:]))) != os.Getppid() {
		return false
	}
	s := startTime(self)
	return !s.IsZero() && !s.After(time.Now())
})

// ProcStartToken is the kernel's own mark of when pid started: p_starttime as
// seconds.microseconds, from kern.proc.pid. The kernel stores it at fork and no
// wall-clock step moves it, so the same mark read twice is the same process.
// False when the pid is gone, or when this kernel's kinfo_proc is not the
// layout these offsets describe (kinfoLayoutOK).
// Exported for a caller that keeps a process's identity in a record (#926:
// the rc-hosts supervisor stamps its pid and start into hosted.json), which
// must treat false as "not this process", never as a match.
func ProcStartToken(pid int) (string, bool) {
	if !kinfoLayoutOK() {
		return "", false
	}
	rec, ok := sysctlRaw([]int32{1, 14, 1, int32(pid)}, nil)
	if !ok || len(rec) != kinfoSize || int(int32(binary.LittleEndian.Uint32(rec[kinfoPID:]))) != pid {
		return "", false
	}
	s := startTime(rec)
	if s.IsZero() {
		return "", false
	}
	return fmt.Sprintf("%d.%06d", s.Unix(), s.Nanosecond()/1000), true
}
