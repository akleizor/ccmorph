//go:build darwin

package util

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// One kinfo_proc record, as the kernel lays it out: 648 bytes on arm64 and
// amd64 alike, the pid at 40, the 17-byte comm at 243 and e_ppid at 560 (the
// offsets of golang.org/x/sys's KinfoProc, which this file deliberately does
// not depend on: the standard library is enough, and the repository ships no
// go.sum for a module to grow behind).
const (
	kinfoSize    = 648
	kinfoPID     = 40
	kinfoComm    = 243
	kinfoCommLen = 17
	kinfoPPID    = 560
)

// sysctlRaw reads one sysctl into buf, sizing it first when buf is nil: the
// table, or false. The size pass and the read are two calls, so a table that
// grew in between is retried on a fresh size rather than read short.
func sysctlRaw(mib []int32, buf []byte) ([]byte, bool) {
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
		if out, ok := sysctlRaw(mib, make([]byte, n+n/8)); ok {
			return out, true
		}
	}
	return nil, false
}

// procArgs is a process's argv from kern.procargs2, space-joined the way ps's
// command= column reads; "" when it cannot be read (another user's process, a
// process that left between the two reads).
func procArgs(pid int, buf []byte) string {
	b, ok := sysctlRaw([]int32{1, 49, int32(pid)}, buf)
	if !ok || len(b) < 4 {
		return ""
	}
	argc := int(binary.LittleEndian.Uint32(b))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0) // the exec path, then NUL padding
	if i < 0 {
		return ""
	}
	rest = bytes.TrimLeft(rest[i:], "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		a, r, _ := bytes.Cut(rest, []byte{0})
		args = append(args, string(a))
		rest = r
	}
	return strings.Join(args, " ")
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
		rows[pid] = psRow{ppid: ppid, comm: comm, args: procArgs(pid, argBuf)}
	}
	if r, ok := rows[os.Getpid()]; !ok || r.ppid != os.Getppid() {
		return nil, false
	}
	return rows, true
}
