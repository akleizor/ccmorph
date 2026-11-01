//go:build linux

package cli

import (
	"syscall"
	"unsafe"
)

func ioctlTermios(fd int) (syscall.Termios, error) {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	if e != 0 {
		return t, e
	}
	return t, nil
}

func setTermios(fd int, t *syscall.Termios) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(t)))
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
