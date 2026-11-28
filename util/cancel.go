package util

import (
	"os"
	"os/exec"
	"sync"
)

// GH-1028: the command-wide signal owner ends the process where it stands, so
// every wait it can interrupt must be able to leave nothing behind. A wait that
// started a child or made state registers here: RunCancelHandlers stops the
// children first (a child writing while its directory is removed is the race
// this order closes) and runs the handlers newest-first, like defers. The
// normal path releases the registration, so a long-lived utility never runs a
// stale handler.
var cancelMu sync.Mutex

type cancelHandler struct {
	fn   func()
	done bool
}

var (
	cancelHandlers []*cancelHandler
	cancelChildren = map[*os.Process]struct{}{}
)

// OnCancel registers fn to run if the command is ended by a signal, and
// returns the release that drops it once the wait has finished on its own.
func OnCancel(fn func()) (release func()) {
	e := &cancelHandler{fn: fn}
	cancelMu.Lock()
	cancelHandlers = append(cancelHandlers, e)
	cancelMu.Unlock()
	return func() {
		cancelMu.Lock()
		e.done = true
		cancelMu.Unlock()
	}
}

// TrackChild registers a spawned child that must not outlive a signal-ended
// command, and returns the release for the normal path.
func TrackChild(p *os.Process) (release func()) {
	if p == nil {
		return func() {}
	}
	cancelMu.Lock()
	cancelChildren[p] = struct{}{}
	cancelMu.Unlock()
	return func() {
		cancelMu.Lock()
		delete(cancelChildren, p)
		cancelMu.Unlock()
	}
}

// RunTracked starts cmd, registers it as a child a signal-ended command must
// stop, and waits. The synchronous start is what makes the registration a fact
// before the wait can be interrupted (a ctx/cancel pair kills asynchronously,
// after the command may already have exited).
func RunTracked(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	release := TrackChild(cmd.Process)
	defer release()
	return cmd.Wait()
}

// RunCancelHandlers stops every registered child and runs every registered
// handler once. It is the last thing a signal-ended command does; a second
// call (there is none today) runs only what the first could not unregister.
func RunCancelHandlers() {
	cancelMu.Lock()
	children := make([]*os.Process, 0, len(cancelChildren))
	for p := range cancelChildren {
		children = append(children, p)
	}
	cancelChildren = map[*os.Process]struct{}{}
	handlers := append([]*cancelHandler(nil), cancelHandlers...)
	cancelMu.Unlock()
	for _, p := range children {
		_ = p.Kill()
	}
	for i := len(handlers) - 1; i >= 0; i-- {
		e := handlers[i]
		cancelMu.Lock()
		if e.done {
			cancelMu.Unlock()
			continue
		}
		e.done = true
		cancelMu.Unlock()
		e.fn()
	}
}
