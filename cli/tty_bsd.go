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

// pushInput puts bytes back on the terminal's input queue, in order, as if they
// had just been typed: the way a probe returns what it read but did not own.
// Best effort — a kernel that refuses TIOCSTI drops them.
func pushInput(fd int, b []byte) {
	for i := range b {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSTI, uintptr(unsafe.Pointer(&b[i]))); e != 0 {
			return
		}
	}
}
