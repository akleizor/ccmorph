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
	holdLock(t)
	old := hookBudget
	// PreToolUse's Request.Deadline (16 s in a real hook). Long enough that a
	// few milliseconds of scheduler wake-up cannot matter next to the 25 ms
	// allowance below, and still quick for the suite.
	hookBudget = 500 * time.Millisecond
	t.Cleanup(func() { hookBudget = old })

	start := time.Now()
	done := make(chan struct{})
	go func() {
		var out bytes.Buffer
		PreToolUse(strings.NewReader(agentCall), &out)
		close(done)
	}()
	// lockSlack is the only allowance over the budget: one 10 ms poll plus a
	// scheduler wake-up. A write that waits any real fraction longer than its
	// budget — the refused implementations waited seconds, or added their own
	// milliseconds to the deadline — lands past this bound and fails.
	const lockSlack = 25 * time.Millisecond
	select {
	case <-done:
	case <-time.After(hookBudget + lockSlack):
		t.Fatalf("a held ledger.lock kept the hook past its %v budget (waited %v)", hookBudget, time.Since(start))
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

// TestSetEnabledOverALiteralNull pins #636: a router.json holding `null`
// decoded to a nil map, and the write panicked.
func TestSetEnabledOverALiteralNull(t *testing.T) {
	for _, body := range []string{"null\n", "", "{}\n", "{\"enabled\":false}\n"} {
		root := t.TempDir()
		t.Setenv("CCMORPH_STATE_DIR", root)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ConfigPath(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := SetEnabled(true); err != nil {
			t.Fatalf("SetEnabled over %q: %v", body, err)
		}
		if c, err := LoadConfig(); err != nil || !c.Enabled {
			t.Fatalf("SetEnabled over %q left enabled=%v, %v", body, c.Enabled, err)
		}
	}
}

// TestLedgerKeepsTheLastThirtyDays pins #636: neither log was ever capped, and
// every settle paid a full-file read.
func TestLedgerKeepsTheLastThirtyDays(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := nowSec()
	appendLine(ledgerPath(), Row{"id": "old", "ts": now - 31*86400})
	appendLine(ledgerPath(), Row{"id": "new", "ts": now})
	appendLine(ledgerPath(), Row{"id": "unreadable"})
	if !ledgerUpdate("new", Row{"outcome": "completed"}) {
		t.Fatal("the new row was not found")
	}
	got := readRows(ledgerPath())
	var ids []string
	for _, r := range got {
		ids = append(ids, fmt.Sprint(r["id"]))
	}
	if strings.Join(ids, ",") != "new,unreadable" {
		t.Fatalf("ledgerUpdate left %v, want [new unreadable]", ids)
	}
}

// TestLedgerRowSkipsOtherLines: complete()'s one-row read must not parse the
// whole ledger, and a miss must answer nil.
func TestLedgerRowSkipsOtherLines(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(routerDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "not json at all\n" +
		`{"id":"route-1","ts":1,"profile":"a"}` + "\n" +
		`{"id":"route-2","ts":2,"profile":"b"}` + "\n"
	if err := os.WriteFile(ledgerPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ledgerRow("route-2"); r == nil || r["profile"] != "b" {
		t.Fatalf("ledgerRow(route-2) = %v", r)
	}
	if r := ledgerRow("route-9"); r != nil {
		t.Fatalf("a miss returned %v", r)
	}
}

// TestOutcomesTrimPastTheCap: the outcomes log is pruned only once it passes
// the cap, and a prune keeps the recent rows.
func TestOutcomesTrimPastTheCap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldTrim := trimBytes
	trimBytes = 200
	t.Cleanup(func() { trimBytes = oldTrim })
	now := nowSec()
	appendLine(filepath.Join(routerDir(), "outcomes.jsonl"), Row{"ts": now - 31*86400, "event": "delegate", "result": "old"})
	for i := 0; i < 5; i++ {
		outcomeAppend(Row{"ts": now, "event": "delegate", "result": fmt.Sprint("new-", i)})
	}
	rows := readRows(filepath.Join(routerDir(), "outcomes.jsonl"))
	if len(rows) != 5 {
		t.Fatalf("a prune left %d rows, want the 5 recent ones", len(rows))
	}
	for _, r := range rows {
		if r["result"] == "old" {
			t.Fatalf("the 31-day-old row survived the prune: %v", rows)
		}
	}
}
