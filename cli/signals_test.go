package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// GH-1028: `add` advertises "Ctrl-C cancels setup" at every prompt and wait,
// but a process started with SIGINT ignored — the shape zsh gives an async job,
// and every scripted, SSH or piped call is one — kept ignoring it: no handler
// was ever installed, and Go honors an inherited ignore. keepCtrlC is the
// command-wide owner; these tests pin who a signal belongs to when it arrives.
//
// endCommand is swapped for a recorder here: the production endCommand ends the
// process, which would take the test binary with it, and the owner answer is the
// decision to end, not the ending itself. The recorder sends to a buffered
// channel; the handler loop processes signals one at a time, so a signal's
// absence from the channel once a later signal's entry is read is proven, not
// waited for — no clock paces any of this but the channel itself.
func TestKeepCtrlCAnswersOnlySignalsNobodyElseHolds(t *testing.T) {
	ended := make(chan syscall.Signal, 8)
	saved := endCommand
	endCommand = func(_ *launcher, sig syscall.Signal) { ended <- sig }
	t.Cleanup(func() { endCommand = saved })
	release := (&launcher{}).keepCtrlC()
	t.Cleanup(release)

	// A picker holding the keys answers the signal itself; the command-wide
	// owner stands down.
	held := keysHeld()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	held()
	// The same signal with nobody holding anything is the command's own.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	var first syscall.Signal
	select {
	case first = <-ended:
	case <-time.After(handoffForwardDeadline):
		t.Fatal("a Ctrl-C nobody held did not end the command")
	}
	if first != syscall.SIGINT {
		t.Fatalf("ended with %v, want SIGINT", first)
	}
	// The handler had processed the held INT before this one (one loop, in
	// order): a second entry would be that held INT ending the command.
	select {
	case extra := <-ended:
		t.Fatalf("the INT a picker held also ended the command (%v)", extra)
	default:
	}
}

// The sign-in handoff (GH-649, GH-837) owns the signal while a child holds the
// wait; keepCtrlC must not end the parent over the top of it, or the rollback
// that makes a cancelled sign-in clean up never runs.
func TestKeepCtrlCStandsDownWhileAHandedChildHoldsTheWait(t *testing.T) {
	ended := make(chan syscall.Signal, 8)
	saved := endCommand
	endCommand = func(_ *launcher, sig syscall.Signal) { ended <- sig }
	t.Cleanup(func() { endCommand = saved })

	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := make(chan error, 1)
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		gone <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	release := handSignInSignals(cmd.Process)
	t.Cleanup(release)
	keep := (&launcher{}).keepCtrlC()
	t.Cleanup(keep)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-gone:
	case <-time.After(handoffForwardDeadline):
		t.Fatal("the child outlived the forwarded SIGINT")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the child did not die of the forwarded SIGINT: %v", err)
	}
	if ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("the child died of %v, want the forwarded SIGINT", err)
	}
	// The handoff stays registered through this check, so a handler still
	// behind sees a handoff waiting and stands down: the command-wide owner
	// never ends a command whose child is mid-handoff.
	select {
	case sig := <-ended:
		t.Fatalf("keepCtrlC ended the command while the child held the wait (%v)", sig)
	default:
	}
}

// keysHeld nests (a picker may run inside a region that already holds the keys)
// and its release runs exactly once, however often it is called.
func TestKeysHeldCountsOwnersAndReleasesOnce(t *testing.T) {
	if keysReading() {
		t.Fatal("no picker runs at the start of this test")
	}
	outer := keysHeld()
	inner := keysHeld()
	if !keysReading() {
		t.Fatal("a held region must read as held")
	}
	inner()
	inner() // a second release of one hold must not steal the outer one
	if !keysReading() {
		t.Fatal("a double release of the inner hold ended the outer one")
	}
	outer()
	if keysReading() {
		t.Fatal("the keys must be free after every hold is released")
	}
}
