//go:build darwin || freebsd

package cli

import (
	"syscall"
	"unsafe"
)

func ioctlTermios(fd int) (syscall.Termios, error) {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	if e != 0 {
		return t, e
	}
	return t, nil
}

func setTermios(fd int, t *syscall.Termios) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(t)))
	if e != 0 {
		return e
	}
	return nil
}

// winsize is the terminal's rows and columns, 0 0 when unknown.
func winsize(fd int) (int, int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if e != 0 {
		return 0, 0
	}
	return int(ws.Row), int(ws.Col)
}

// fionread is FIONREAD, which Go's syscall package does not name on macOS or
// FreeBSD (the value is the same on both).
const fionread = 0x4004667f

// pushInput puts b on the terminal's input queue, in order, as if it had just
// been typed (TIOCSTI). It stops at the first byte the kernel refuses and
// reports why: Linux with dev.tty.legacy_tiocsti=0 refuses every byte, and
// macOS and BSD refuse a terminal that is not the caller's controlling one.
func pushInput(fd int, b []byte) error {
	for i := range b {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSTI, uintptr(unsafe.Pointer(&b[i]))); e != 0 {
			return e
		}
	}
	return nil
}

// inputPending is how many bytes wait on fd's input queue, -1 when unknown.
func inputPending(fd int) int {
	var n int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fionread, uintptr(unsafe.Pointer(&n))); e != 0 {
		return -1
	}
	return int(n)
}

// foregroundTTY is whether fd is this process's controlling terminal and this
// process's group owns it: only then may the probe change its mode or read it
// without the kernel stopping the process (SIGTTOU, SIGTTIN).
func foregroundTTY(fd int) bool {
	var pgrp int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp))); e != 0 {
		return false
	}
	return int(pgrp) == syscall.Getpgrp()
}
