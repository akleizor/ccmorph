package adapter

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// sigRecordSupported: Linux shows each process's blocked and pending signal
// sets in /proc/<pid>/status, so a test can read that a TERM was sent to a
// child that has not run since.
const sigRecordSupported = true

// sigBlockSupported: Linux can start the child with SIGTERM blocked and read
// that block back, so the TERM cannot be consumed before the read.
const sigBlockSupported = true

// startStubborn starts cmd with SIGTERM blocked in the child. Go's fork
// copies the forking thread's signal mask into the child and exec keeps it,
// so a TERM sent to the child waits in the kernel's pending set, where the
// watch reads it, until the child dies: the child never has to run.
func startStubborn(cmd *exec.Cmd) (*proc, error) {
	type started struct {
		p   *proc
		err error
	}
	out := make(chan started, 1)
	go func() {
		// Never unlocked: the thread whose mask this changes ends with the
		// goroutine, so no later fork in the test binary inherits the block.
		runtime.LockOSThread()
		block, old := uint64(1)<<(syscall.SIGTERM-1), uint64(0)
		if _, _, e := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 0 /* SIG_BLOCK */, uintptr(unsafe.Pointer(&block)), uintptr(unsafe.Pointer(&old)), 8, 0, 0); e != 0 {
			out <- started{nil, e}
			return
		}
		p, err := start(cmd)
		syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 2 /* SIG_SETMASK */, uintptr(unsafe.Pointer(&old)), 0, 8, 0, 0)
		out <- started{p, err}
	}()
	s := <-out
	return s.p, s.err
}

// sigWatch reads one child's signal state from /proc: the TERM sent to it is
// recorded there whether or not the child ever runs again.
type sigWatch struct{ pid int }

// watchChildSignals returns the /proc watch for pid.
func watchChildSignals(pid int) (*sigWatch, error) { return &sigWatch{pid: pid}, nil }

// pending is the signals waiting for the child right now.
func (w *sigWatch) pending() (uint64, error) { return sigSet(w.pid, "SigPnd", "ShdPnd") }

// blocked is the child's blocked set.
func (w *sigWatch) blocked() (uint64, error) { return sigSet(w.pid, "SigBlk") }

func (w *sigWatch) close() {}

// sigSet reads pid's signal sets named by fields ("SigBlk"; or "SigPnd" and
// "ShdPnd", the thread's and the process's pending sets) from
// /proc/<pid>/status, ORed together.
func sigSet(pid int, fields ...string) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	var set uint64
	found := 0
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || !slices.Contains(fields, k) {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/%d/status %s: %v", pid, k, err)
		}
		set |= bits
		found++
	}
	if found != len(fields) {
		return 0, fmt.Errorf("/proc/%d/status lacks one of %v", pid, fields)
	}
	return set, nil
}
