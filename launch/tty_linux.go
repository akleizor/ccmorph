//go:build linux

package launch

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
