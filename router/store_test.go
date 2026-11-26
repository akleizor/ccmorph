package router

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTheLedgerLockHelperHoldsTheLockForTheParent is not a test in the parent:
// the test binary re-runs it as a helper process that takes ledger.lock and
// holds it until the parent closes its stdin.
func TestTheLedgerLockHelperHoldsTheLockForTheParent(t *testing.T) {
	if os.Getenv("CCMORPH_TEST_LEDGER_LOCK") != "1" {
		return
	}
	f, err := os.OpenFile(os.Getenv("CCMORPH_TEST_LEDGER_LOCK_PATH"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(1)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// holdLock starts the helper above and returns once it holds ledger.lock; the
// caller's cleanup closes stdin, which lets the helper exit and release it.
func holdLock(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(routerDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheLedgerLockHelperHoldsTheLockForTheParent$")
	cmd.Env = append(os.Environ(), "CCMORPH_TEST_LEDGER_LOCK=1",
		"CCMORPH_TEST_LEDGER_LOCK_PATH="+filepath.Join(routerDir(), "ledger.lock"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("the helper did not take the lock: %q, %v", line, err)
	}
}

func TestASpentBudgetRefusesTheLedgerWriteEvenWhenTheLockIsFree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Cleanup(func() { armLock(time.Time{}) })
	armLock(time.Now().Add(-time.Millisecond))
	ran := false
	if withLock(func() { ran = true }) || ran {
		t.Fatal("a spent budget still ran the write under a free lock")
	}
	ledgerAppend(Row{"id": "late"})
	if rows := readRows(ledgerPath()); len(rows) != 0 {
		t.Fatalf("a spent budget wrote %d ledger row(s)", len(rows))
	}
}

func TestAHookPathLedgerWriteReturnsWithinTheHooksBudgetWhileAnotherProcessHoldsTheLock(t *testing.T) {
	hookEnv(t, true)
	quotaSnapshot(t, map[string]float64{"here": 0.1, "there": 0.9}) // a same-profile pick, so the hook reserves
	old := hookBudget
	// PreToolUse's Request.Deadline (16 s in a real hook), short enough to
	// keep the suite quick. The bound below is a measurement of this run's
	// load around the budget, so the budget's length cannot hide a wait that
	// outlives it.
	hookBudget = 500 * time.Millisecond
	t.Cleanup(func() { hookBudget = old })

	// runHook makes one PreToolUse call and reports when it returned.
	runHook := func() time.Duration {
		start := time.Now()
		var out bytes.Buffer
		PreToolUse(strings.NewReader(agentCall), &out)
		return time.Since(start)
	}

	// The allowance over the budget is measured under this run's load, not
	// assumed. It used to be a literal 25 ms — "one 10 ms poll plus a
	// scheduler wake-up" — and under gate load a single wake-up late from the
	// sleep in withLock passed it while the write was being refused at its
	// deadline exactly as designed (#903). A refusal is now owed three
	// measured things: the hook's own path around the wait (taken first, with
	// the lock still free, so the held run repeats it warm), the budget as
	// this run charges it, and one more poll. The sleep below is the second:
	// the length of the budget, started with the write, so it wakes at the
	// instant the write's last poll wakes and is late exactly as late as the
	// scheduler is right now — a single late wake-up cannot outrun an
	// allowance taken from one. A write that waits any real fraction longer
	// than its budget — the refused implementations waited seconds — lands
	// past the sum however late it is woken, and fails.
	unheld := runHook()
	if err := os.WriteFile(ledgerPath(), nil, 0o600); err != nil { // the unheld run's own row is not this test's evidence
		t.Fatal(err)
	}
	holdLock(t)

	returned := make(chan time.Duration, 1)
	wake := time.Now()
	go func() { returned <- runHook() }()
	time.Sleep(hookBudget)
	bound := unheld + time.Since(wake) + lockPoll
	var waited time.Duration
	select {
	case waited = <-returned:
	case <-time.After(bound + 10*hookBudget): // only a write that never returns at all lands here
		t.Fatalf("a held ledger.lock never released the hook: still inside PreToolUse %v after its budget started (budget %v, bound %v)", time.Since(wake), hookBudget, bound)
	}
	if waited > bound {
		t.Fatalf("a held ledger.lock kept the hook past its %v budget (returned after %v, bound %v)", hookBudget, waited, bound)
	}
	if rows := readRows(ledgerPath()); len(rows) != 0 {
		t.Fatalf("a refused write left %d ledger row(s)", len(rows))
	}
}

func TestALockThatCannotBeOpenedRefusesTheWriteInsteadOfRunningItUnlocked(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(routerDir(), "ledger.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	ran := false
	if withLock(func() { ran = true }) {
		t.Fatal("withLock answered success without taking a lock")
	}
	if ran {
		t.Fatal("withLock ran its write unlocked")
	}
}
