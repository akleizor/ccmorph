//go:build linux

package util

import (
	"syscall"
	"unsafe"
)

// IsTerminal reports whether fd is a terminal: the one answer every reader
// that must not read a terminal asks, so the packages below the cli (the
// router) can tell a person's keyboard from a pipe.
func IsTerminal(fd int) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return e == 0
}
