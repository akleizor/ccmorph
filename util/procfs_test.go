//go:build !darwin

package util

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// procfsFixtureRoot aims the kernel-table read at a /proc the test writes, and
// restores the real one afterwards. The window where a pid leaves between the
// listing and its argv read cannot be caught in the live table; its outcome —
// an argv that answers ENOENT — is what the fixture states directly.
func procfsFixtureRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	anchor := time.Now().Add(-2 * time.Hour).Unix()
	if err := os.WriteFile(dir+"/stat", []byte("btime "+strconv.FormatInt(anchor, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := procfsRoot
	procfsRoot = dir
	t.Cleanup(func() { procfsRoot = restore })
	return dir
}

// procfsFixturePid writes one pid's stat, and its cmdline unless it is nil —
// no file is the kernel's own word for a pid gone before the read.
func procfsFixturePid(t *testing.T, root string, pid int, cmdline []byte) {
	t.Helper()
	p := root + "/" + strconv.Itoa(pid)
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := strconv.Itoa(pid) + " (sleep) S 7 " + strconv.Itoa(pid) + " 41 0 -1 4194560 100 0 0 0 5 6 0 0 20 0 1 0 360000 999\n"
	if err := os.WriteFile(p+"/stat", []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if cmdline != nil {
		if err := os.WriteFile(p+"/cmdline", cmdline, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcfsTableNamesAChildWithoutAnExec(t *testing.T) {
	// #991: the table read must not itself be an exec — ps is a reader of
	// these same files plus an exec, and the exec is what queues behind a
	// fork storm, past the bounds doctor and rc-hosts put on the answer. The
	// kernel's own copy names a live child whole: parent, comm, the joined
	// argv with its spaces kept, and a start this machine agrees with.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Args = []string{"a marker with spaces", "30"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	tab := procfsProcessTable()
	if tab == nil {
		t.Fatal("no process table where /proc is the kernel's own")
	}
	r, ok := tab[cmd.Process.Pid]
	if !ok {
		t.Fatalf("the child is not in the kernel's table (read %d rows)", len(tab))
	}
	if r.ppid != os.Getpid() {
		t.Fatalf("parent is %d, want this test's %d", r.ppid, os.Getpid())
	}
	if r.comm != "sleep" {
		t.Fatalf("comm is %q, want sleep", r.comm)
	}
	if r.args != "a marker with spaces 30" {
		t.Fatalf("args joined is %q, want the argv whole and space-joined", r.args)
	}
	if r.start.IsZero() || r.start.Before(time.Now().Add(-time.Hour)) || r.start.After(time.Now()) {
		t.Fatalf("start %v is not this minute's own", r.start)
	}

	cmd.Process.Kill()
	cmd.Wait() // reaped: the pid is gone from the kernel's table, not a zombie row
	if _, ok := procfsProcessTable()[cmd.Process.Pid]; ok {
		t.Fatal("a reaped pid still holds a row")
	}
}

func TestParseProcStatReadsPastACommHoldingParens(t *testing.T) {
	// comm is parenthesized and may hold spaces and parens of its own; the
	// fields are counted from the last one: state, parent, ..., starttime.
	line := []byte("42 (weird ) comm) S 7 42 41 0 -1 4194560 100 0 0 0 5 6 0 0 20 0 1 0 12345 999")
	ppid, comm, ticks, ok := parseProcStat(line)
	if !ok || ppid != 7 || comm != "weird ) comm" || ticks != 12345 {
		t.Fatalf("got ppid=%d comm=%q ticks=%d ok=%v", ppid, comm, ticks, ok)
	}
	for _, bad := range []string{"", "42 x S 7", "42 (unterminated S 7",
		"42 (c) S 7 42 41 0 -1 1 1 0 0 0 5 6 0 0 20 0 1 0"} { // nineteen fields: no starttime
		if _, _, _, ok := parseProcStat([]byte(bad)); ok {
			t.Fatalf("a cut-short stat line parsed: %q", bad)
		}
	}
}

func TestProcfsStartRefusesTheFuture(t *testing.T) {
	// A start computed after now is ticks or an anchor that does not describe
	// this machine: the cannot-tell zero the stale sweep holds on, never a
	// reaping on a wrong time (#189).
	boot := time.Now().Add(-time.Hour)
	if got, ok := procfsStart(boot, 300*procfsClkTck, time.Now()); !ok ||
		!got.Equal(boot.Add(300*time.Second)) {
		t.Fatalf("half-past-the-boot start reads %v ok=%v", got, ok)
	}
	if _, ok := procfsStart(boot, 48*3600*procfsClkTck, time.Now()); ok {
		t.Fatal("a start tomorrow answered ok: the wrong clock must read as cannot-tell")
	}
	// Fractional ticks keep their tenth of a second, and long uptides do not
	// overflow the duration: a year of ticks lands a year after the boot.
	year := int64(365 * 24 * 3600 * procfsClkTck)
	if got, ok := procfsStart(boot, year, boot.AddDate(1, 0, 1)); !ok ||
		got.Before(boot.AddDate(1, 0, 0)) || got.Sub(boot) > 366*24*time.Hour {
		t.Fatalf("a year of ticks reads %v ok=%v", got, ok)
	}
}

func TestRunningAdapterAnswersFromTheKernelTableUnderTheBound(t *testing.T) {
	// #991: doctor's one-second bound used to bound three ps execs, which a
	// loaded machine could blow without the table existing any problem. With
	// the kernel read first the bound bounds an open, not an exec: a full
	// table read answers inside it on this machine, however busy the run
	// queue. (The PATH ps stub the doctor suite installs no longer reaches
	// the read here: that is the regression.)
	start := time.Now()
	tab := processTableWithin(context.Background())
	if tab == nil {
		t.Fatal("processTableWithin read nothing")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the kernel table read took %v: past doctor's bound", d)
	}
	r, ok := tab[os.Getpid()]
	if !ok || r.comm == "" {
		t.Fatalf("this test is not in the table it read (row %+v)", r)
	}
}

func TestProcfsTableDropsAPidGoneBeforeItsArgv(t *testing.T) {
	// #991: a pid listed, statted, and gone by the argv read must leave no
	// row with the old parent, comm and start — the vanished pid has no row,
	// and its absence says nothing about the rows still to be read.
	dir := procfsFixtureRoot(t)
	procfsFixturePid(t, dir, 43, []byte("/bin/sleep\x0030\x00"))
	procfsFixturePid(t, dir, 42, nil) // gone between the listing and its argv
	tab := procfsProcessTable()
	if tab == nil {
		t.Fatal("a vanished row voided a table whose other rows all read")
	}
	if _, ok := tab[42]; ok {
		t.Fatal("a pid gone by its argv read still holds a row")
	}
	r, ok := tab[43]
	if !ok || r.args != "/bin/sleep 30" {
		t.Fatalf("the complete pid lost its row or its argv: %+v ok=%v", r, ok)
	}
}

func TestProcfsTableGivesUpOnALostArgvRead(t *testing.T) {
	// The same walk, but the argv read fails for a reason that proves nothing
	// about the process (here: the path is no file at all — EISDIR even for
	// root). One row silently missing would read as "no such adapter", so the
	// whole table is given up and the caller's next reader answers.
	dir := procfsFixtureRoot(t)
	procfsFixturePid(t, dir, 43, []byte("/bin/sleep\x0030\x00"))
	procfsFixturePid(t, dir, 42, nil)
	if err := os.Mkdir(dir+"/42/cmdline", 0o755); err != nil { // unreadable: not a file
		t.Fatal(err)
	}
	if tab := procfsProcessTable(); tab != nil {
		t.Fatalf("a lost argv read answered %d rows; it must void the table", len(tab))
	}
}
