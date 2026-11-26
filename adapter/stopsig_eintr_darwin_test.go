//go:build darwin

package adapter

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
)

// TestKeventRetriesInterruptedDrain pins the witness read's answer to an
// interruption: EINTR is a retry, not a verdict. On 34aa8ee2 one EINTR was
// terminal — CI run 45747952377 died on pending's "interrupted system call"
// while its pending mask held exactly the value the assertion wanted.
func TestKeventRetriesInterruptedDrain(t *testing.T) {
	t.Run("one interruption then a clean drain answers (0, nil)", func(t *testing.T) {
		calls := 0
		swap(t, &syscallKevent, func(int, []syscall.Kevent_t, []syscall.Kevent_t) (int, error) {
			calls++
			if calls == 1 {
				return 0, syscall.EINTR
			}
			return 0, nil
		})
		n, err := kevent(-1, nil, make([]syscall.Kevent_t, 8))
		if n != 0 || err != nil {
			t.Fatalf("one EINTR then a clean drain: got (%d, %v), want (0, nil)", n, err)
		}
		if calls != 2 {
			t.Errorf("one EINTR took %d calls, want the retry to be the second", calls)
		}
	})
	t.Run("a call that never stops interrupting ends as an error", func(t *testing.T) {
		calls := 0
		swap(t, &syscallKevent, func(int, []syscall.Kevent_t, []syscall.Kevent_t) (int, error) {
			calls++
			return 0, syscall.EINTR
		})
		_, err := kevent(-1, nil, make([]syscall.Kevent_t, 8))
		if !errors.Is(err, syscall.EINTR) {
			t.Fatalf("a permanently interrupted call: got %v, want the EINTR as the answer", err)
		}
		if calls != maxKeventEINTR {
			t.Errorf("the retry ran %d times, want it bounded at %d, not to hang the suite", calls, maxKeventEINTR)
		}
	})
	t.Run("any other error answers at once", func(t *testing.T) {
		calls := 0
		swap(t, &syscallKevent, func(int, []syscall.Kevent_t, []syscall.Kevent_t) (int, error) {
			calls++
			return 0, syscall.EBADF
		})
		_, err := kevent(-1, nil, make([]syscall.Kevent_t, 8))
		if !errors.Is(err, syscall.EBADF) {
			t.Fatalf("EBADF: got %v, want it returned unchanged", err)
		}
		if calls != 1 {
			t.Errorf("EBADF took %d calls, want one: only EINTR is an interruption", calls)
		}
	})
}

// TestPendingAnswersThroughSelfSignals drives the exact race CI hit: a signal
// landing on the test process while pending() is inside kevent. Each caught
// SIGWINCH can interrupt the in-flight call; the read must keep answering
// (0, nil) instead of handing the interruption back as an error.
func TestPendingAnswersThroughSelfSignals(t *testing.T) {
	child, err := start(exec.Command("sleep", "60"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killIfLive(child) })
	watch, err := watchChildSignals(child.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watch.close)
	// Caught but ignored: a handler is installed and the channel is never
	// drained, so each delivery interrupts pending's kevent and stops there.
	// The signals go to this process, not the watched child, so the child's
	// witness must stay empty.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	t.Cleanup(func() { signal.Stop(winch) })
	for i := 0; i < 200; i++ {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
			t.Fatalf("sending the test process a SIGWINCH: %v", err)
		}
		if p, err := watch.pending(); err != nil || p != 0 {
			t.Fatalf("iteration %d: pending %#x, %v — an interruption answered as a verdict", i, p, err)
		}
	}
}
