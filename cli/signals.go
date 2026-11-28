package cli

import (
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// GH-1028: `help add` says "Ctrl-C cancels setup", docs/guide.md promises
// "Ctrl+C also exits cleanly without a traceback", and every wait the product
// prints names Ctrl-C as its way out. For the command itself to answer Ctrl-C
// it must own the signal — but zsh gives an async job SIG_IGN for INT, Go keeps
// an inherited ignore (os/signal doc: "started with SIGHUP or SIGINT ignored,
// they will remain ignored"), and the `cli` verbs never installed a handler, so
// every scripted, SSH or piped `ccmorph add` ignored the one escape it
// advertises. These three pieces make Ctrl-C live wherever no one else is
// holding the terminal's keys: a command-wide owner (`keepCtrlC`), the two
// owner counts it consults, and the one way every signal ends this process.

// keyOwner counts the picker loops and picker children currently holding the
// terminal's keys: while one does, SIGINT is its answer (a Cancelled line and
// its own restore, raw mode included), and no command-wide handler may exit
// over the top of it.
var keyOwner atomic.Int32

func keysReading() bool { return keyOwner.Load() > 0 }

// keysHeld marks the current region as one holding the terminal's keys — a
// picker's loop or a picker's own child — until the returned release runs; use
// it as `defer keysHeld()()`.
func keysHeld() func() {
	keyOwner.Add(1)
	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			keyOwner.Add(-1)
		}
	}
}

// handoffs counts the child waits that hold the signals (handSignInSignals):
// the child owns such a wait, dies of the signal, and the parent survives to
// run the caller's failure path (GH-649, GH-837).
var handoffs atomic.Int32

// endCommand ends this command the way the signal's own default disposition
// would have: the helpers stop first, then the process dies of the signal, so
// the caller sees 128+signal and no traceback is printed. Reset comes first, so
// a signal Go was holding can still land as a death; where Reset cannot undo a
// started-ignored disposition (the signal then re-ignores), a brief wait and an
// equal exit code keep the promise anyway. A package var so the tests of who
// owns a signal can watch it end without ending the test binary.
var endCommand = func(l *launcher, sig syscall.Signal) {
	l.stopHelpers()
	signal.Reset(sig)
	syscall.Kill(os.Getpid(), sig)
	time.Sleep(time.Second)
	os.Exit(128 + int(sig))
}

// ctrlCSwallowed reports each signal a command-wide owner declined because a
// picker or a handed-off child holds it. The send never blocks and drops when
// the buffer is full, so production pays a compare and the tests get a sync
// point: "the owner saw it and stood down", proven rather than raced.
var ctrlCSwallowed = make(chan os.Signal, 16)

func swallowed(s os.Signal) {
	select {
	case ctrlCSwallowed <- s:
	default:
	}
}

// keepCtrlC is `add`'s and `login`'s promise that Ctrl-C cancels for the whole
// command, not only while a picker has the keys. SIGINT that belongs to no
// picker and no handed-off child ends the command where it stands, saving
// nothing: every wait that can leave state behind ends through a picker's rc or
// a handed-off child's failure path, and both already roll back (GH-1028). The
// Notify is what makes Ctrl-C live at all when the process started with SIGINT
// ignored — os/signal installs a handler for a Notify'd signal regardless.
func (l *launcher) keepCtrlC() (release func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		for s := range sigs {
			if keysReading() || handoffs.Load() > 0 {
				swallowed(s) // a picker or the child it waits on owns this one
				continue
			}
			if sig, ok := s.(syscall.Signal); ok {
				endCommand(l, sig)
			}
		}
	}()
	return func() {
		signal.Stop(sigs) // returns once no more signals can arrive
		close(sigs)
	}
}
