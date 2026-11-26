//go:build darwin

package adapter

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// sigRecordSupported: macOS's kernel posts NOTE_SIGNAL to an EVFILT_PROC
// kqueue watcher as a signal is sent, before the child's disposition is
// consulted, so a test can read that a TERM reached a child that has not run
// since.
const sigRecordSupported = true

// sigBlockSupported: macOS exposes no other process's blocked set, and the
// kqueue witness records the ask without the child blocking the signal.
const sigBlockSupported = false

// startStubborn starts cmd: the child's own ignored-TERM trap keeps it stubborn,
// and the kernel's NOTE_SIGNAL witness needs no signal blocked.
func startStubborn(cmd *exec.Cmd) (*proc, error) { return start(cmd) }

// sigWatch drains the NOTE_SIGNAL events the kernel queues for one child.
type sigWatch struct {
	kq  int
	pid int
}

// watchChildSignals attaches a kqueue to pid for NOTE_SIGNAL. The kernel posts the
// event when the signal is sent (kern_sig.c, before the ignored-signal check),
// so the ask is witnessed however loaded the machine is.
func watchChildSignals(pid int) (*sigWatch, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	ch := syscall.Kevent_t{Ident: uint64(pid), Filter: syscall.EVFILT_PROC,
		Flags: syscall.EV_ADD | syscall.EV_ENABLE, Fflags: syscall.NOTE_SIGNAL}
	events := make([]syscall.Kevent_t, 1)
	// A zero timeout: kevent processes the change and returns at once. A nil
	// timeout would wait for the first event, which is the TERM this watch
	// exists to catch: the attach would block the test before the stop.
	n, err := kevent(kq, []syscall.Kevent_t{ch}, events)
	if err == nil && n > 0 && events[0].Flags&syscall.EV_ERROR != 0 {
		err = fmt.Errorf("EVFILT_PROC %d: %v", pid, syscall.Errno(events[0].Data))
	}
	if err != nil {
		syscall.Close(kq)
		return nil, err
	}
	return &sigWatch{kq: kq, pid: pid}, nil
}

// pending drains the NOTE_SIGNAL events queued so far as a signal mask. The
// event names the signal's arrival but not its number; the first stop sends
// this child only TERM before recA reads, so a queued NOTE_SIGNAL is that TERM.
func (w *sigWatch) pending() (uint64, error) {
	events := make([]syscall.Kevent_t, 8)
	n, err := kevent(w.kq, nil, events)
	if err != nil {
		return 0, err
	}
	var set uint64
	for i := 0; i < n; i++ {
		if events[i].Flags&syscall.EV_ERROR != 0 {
			return 0, fmt.Errorf("EVFILT_PROC %d: %v", w.pid, syscall.Errno(events[i].Data))
		}
		if events[i].Fflags&syscall.NOTE_SIGNAL != 0 {
			set |= sigBit(syscall.SIGTERM)
		}
	}
	return set, nil
}

// blocked is not readable on macOS; the witness does not need it.
func (w *sigWatch) blocked() (uint64, error) {
	return 0, errors.New("no other process's blocked set on macOS")
}

// syscallKevent is the one interruptible call the witness makes, kept as a
// package variable so the retry below can be checked with a stubbed drain.
var syscallKevent = func(kq int, changes, events []syscall.Kevent_t) (int, error) {
	return syscall.Kevent(kq, changes, events, &syscall.Timespec{})
}

// maxKeventEINTR bounds the retries: a zero-timeout kevent is idempotent —
// draining again is the same answer — so a retry costs the test microseconds,
// but a persistent interruption must end as an error, not hang the suite.
const maxKeventEINTR = 5

// kevent is a zero-timeout kevent retried up to maxKeventEINTR times on EINTR:
// Go's runtime preempts a busy test with SIGURG, which interrupts the call
// before it reads the queue. Re-adding an EV_ADD filter is idempotent, so a
// retried attach is safe. Any other error, and an exhausted retry budget, are
// answers, not interruptions.
func kevent(kq int, changes, events []syscall.Kevent_t) (int, error) {
	var err error
	for i := 0; i < maxKeventEINTR; i++ {
		var n int
		n, err = syscallKevent(kq, changes, events)
		if !errors.Is(err, syscall.EINTR) {
			return n, err
		}
	}
	return 0, err
}

func (w *sigWatch) close() { syscall.Close(w.kq) }
