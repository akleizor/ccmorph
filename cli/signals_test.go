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
// decision to end, not the ending itself. Every step below waits on an event
// the handler itself produces — its stand-down report, its end, a child's
// death — bounded by the GH-851 deadline, never on a fixed sleep.
func TestKeepCtrlCAnswersOnlySignalsNobodyElseHolds(t *testing.T) {
	ended := make(chan syscall.Signal, 8)
	saved := endCommand
	endCommand = func(_ *launcher, sig syscall.Signal) { ended <- sig }
	t.Cleanup(func() { endCommand = saved })
	drainSignals()
	release := (&launcher{}).keepCtrlC()
	t.Cleanup(release)

	// A picker holding the keys answers the signal itself; the command-wide
	// owner stands down. The stand-down report is the sync: the handler's own
	// decision has happened before the keys are released.
	held := keysHeld()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if sig := wantSwallowed(t); sig != syscall.SIGINT {
		t.Fatalf("the handler reported %v while the keys were held", sig)
	}
	held()
	// The same signal with nobody holding anything is the command's own.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case sig := <-ended:
		if sig != syscall.SIGINT {
			t.Fatalf("ended with %v, want SIGINT", sig)
		}
	case <-time.After(handoffForwardDeadline):
		t.Fatal("a Ctrl-C nobody held did not end the command")
	}
	// Exactly one stand-down and one end: an extra end would be the held INT
	// answered by the wrong owner.
	select {
	case extra := <-ended:
		t.Fatalf("the INT a picker held also ended the command (%v)", extra)
	default:
	}
	select {
	case extra := <-ctrlCSwallowed:
		t.Fatalf("a second signal was stood down (%v)", extra)
	default:
	}
}

// wantSwallowed waits for the next stand-down report: the decision itself,
// not the signal's arrival at a channel.
func wantSwallowed(t *testing.T) syscall.Signal {
	t.Helper()
	select {
	case s := <-ctrlCSwallowed:
		sig, ok := s.(syscall.Signal)
		if !ok {
			t.Fatalf("stand-down report carried %T", s)
		}
		return sig
	case <-time.After(handoffForwardDeadline):
		t.Fatal("the command-wide owner never answered a signal it held")
		return 0
	}
}

// wantEnded waits for the next recorded end of the command.
func wantEnded(t *testing.T, ended <-chan syscall.Signal) syscall.Signal {
	t.Helper()
	select {
	case sig := <-ended:
		return sig
	case <-time.After(handoffForwardDeadline):
		t.Fatal("the command did not end")
		return 0
	}
}

// drainSignals empties the shared report channel so each test counts only its
// own signals.
func drainSignals() {
	for {
		select {
		case <-ctrlCSwallowed:
		default:
			return
		}
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
	drainSignals()
	release := handSignInSignals(cmd.Process)
	t.Cleanup(release)
	keep := (&launcher{}).keepCtrlC()
	t.Cleanup(keep)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// The handoff's sync (the child's death) and the command-wide owner's sync
	// (its stand-down report) in either order: both prove the signal was
	// answered while the handoff was still registered.
	if sig := wantSwallowed(t); sig != syscall.SIGINT {
		t.Fatalf("the handler reported %v during the handoff", sig)
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

// GH-1028: the launch's own INT/QUIT swallow lasted its whole life, so the
// endpoint check printed "Ctrl-C to cancel" and could not be cancelled. INT and
// QUIT belong to the child only while one runs, and to a picker only while one
// reads keys; with neither owner the launch ends by the signal, and a TERM with
// a running child is still only forwarded to it. Each phase below is bounded by
// an event the loop itself produces (stand-down report, recorded end, child's
// death), so no clock paces it but the channel.
func TestLaunchSignalsAnswerCtrlCOnlyWhenNobodyOwnsIt(t *testing.T) {
	ended := make(chan syscall.Signal, 8)
	saved := endCommand
	endCommand = func(_ *launcher, sig syscall.Signal) { ended <- sig }
	t.Cleanup(func() { endCommand = saved })

	l := &launcher{}
	sigs := make(chan os.Signal, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.signals(sigs)
	}()
	t.Cleanup(func() { close(sigs); <-done })

	// INT while a picker holds the keys is the picker's, not the launch's.
	drainSignals()
	held := keysHeld()
	sigs <- syscall.SIGINT
	if sig := wantSwallowed(t); sig != syscall.SIGINT {
		t.Fatalf("the launch reported %v while the keys were held", sig)
	}
	held()
	// QUIT with no owner at all ends the launch.
	sigs <- syscall.SIGQUIT
	if sig := wantEnded(t, ended); sig != syscall.SIGQUIT {
		t.Fatalf("the launch answered an INT a picker held (%v), want the QUIT after it", sig)
	}

	// INT with a running child belongs to the child; the TERM that follows is
	// forwarded to it (GH: `kill <launcher pid>` still reaches the session),
	// and only after the child is gone does the next INT become the launch's.
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
	l.mu.Lock()
	l.child = cmd.Process
	l.mu.Unlock()
	sigs <- syscall.SIGINT
	if sig := wantSwallowed(t); sig != syscall.SIGINT {
		t.Fatalf("the launch reported %v while the session held it", sig)
	}
	sigs <- syscall.SIGTERM
	select {
	case err := <-gone:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the child did not die of the forwarded TERM: %v", err)
		}
		if ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Fatalf("the child died of %v, want the forwarded TERM", err)
		}
	case <-time.After(handoffForwardDeadline):
		t.Fatal("the TERM was not forwarded to the child")
	}
	l.mu.Lock()
	l.child = nil
	l.mu.Unlock()
	sigs <- syscall.SIGINT
	if sig := wantEnded(t, ended); sig != syscall.SIGINT {
		t.Fatalf("first end recorded after the child was gone: %v", sig)
	}
	select {
	case extra := <-ended:
		t.Fatalf("an owned signal ended the launch too (%v)", extra)
	default:
	}
}
