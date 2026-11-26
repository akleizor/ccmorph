//go:build darwin

package adapter

import (
	"errors"
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
	t.Run("a long burst of interruptions still ends in the drain's answer", func(t *testing.T) {
		const burst = 1000
		calls := 0
		swap(t, &syscallKevent, func(int, []syscall.Kevent_t, []syscall.Kevent_t) (int, error) {
			calls++
			if calls <= burst {
				return 0, syscall.EINTR
			}
			return 3, nil
		})
		n, err := kevent(-1, nil, make([]syscall.Kevent_t, 8))
		if n != 3 || err != nil {
			t.Fatalf("%d EINTRs then a drain: got (%d, %v), want (3, nil): an interruption is never a verdict", burst, n, err)
		}
		if calls != burst+1 {
			t.Errorf("the retry ran %d times, want %d", calls, burst+1)
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
