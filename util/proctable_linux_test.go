//go:build linux

package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// psThatCannotList puts a ps on PATH that rejects the table read the way a
// busybox ps does (probe 2026-11-28, busybox v1.37: `ps -axo pid=,lstart=`
// answers "unrecognized option: x"), with nothing after it: a reader that needs
// ps has no table at all there.
func psThatCannotList(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'ps: unrecognized option: x' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// GH-1051: the identity sweep must not need a GNU ps. On a busybox Linux the
// ps table was unreadable, so gc-stale's holder check found no row and answered
// "holds" for every live pid: the relay of a recycled launcher — the very
// accumulation #189/#206/#218 exist to reap — stayed forever. The holder's
// start is the kernel's own field in /proc/<pid>/stat, so the relay goes even
// when the ps on PATH cannot list a process, while the live launcher's own
// relay stays: a reader that cannot tell must never turn into a sweep that
// takes live state.
func TestGCReapsARecycledRelayWithoutGNUPs(t *testing.T) {
	psThatCannotList(t)

	// A live same-user process now holds a dead launcher's number, and its
	// command line names claude: only its start can tell that it is not the
	// launcher the relay names (#189, #206).
	recycled := exec.Command("/bin/sleep", "30")
	recycled.Args = []string{"grep -r claude /Users/someone/notes", "30"}
	if recycled.Start() != nil {
		t.Fatal("could not start the recycled-pid stand-in")
	}
	defer func() { recycled.Process.Kill(); recycled.Wait() }()

	root := t.TempDir()
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "tmp")
	os.MkdirAll(filepath.Join(state, "run"), 0o700)
	os.MkdirAll(tmp, 0o700)
	past := time.Now().Add(-2 * time.Minute)
	// The name's clock says the launch happened at the epoch; the process
	// holding the number started since, so it is not the launcher.
	recycledRelay := filepath.Join(state, ".reswitch."+strconv.Itoa(recycled.Process.Pid)+".1_000000.2")
	// A number nobody holds has no owner, whichever table is read.
	dead := filepath.Join(state, ".reswitch.999999.1.2")
	// This process's own relay names a launch that just happened: its holder
	// started before the name's clock and must keep it.
	mine := filepath.Join(state, ".reswitch."+strconv.Itoa(os.Getpid())+"."+
		strconv.FormatInt(time.Now().Unix(), 10)+"_000000.2")
	for _, f := range []string{recycledRelay, dead, mine} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(f, past, past)
	}

	GCStale(state, filepath.Join(root, "profiles"), tmp)

	if _, err := os.Stat(recycledRelay); err == nil {
		t.Fatalf("%s must go: the launcher it names is dead and its number is now a process that started since", recycledRelay)
	}
	if _, err := os.Stat(dead); err == nil {
		t.Fatalf("%s must go: no process holds its launcher pid", dead)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("%s must stay: this live launcher queued it", mine)
	}
}

// GH-1051: on Linux a row's start is the kernel's own field 22 of
// /proc/<pid>/stat, ticks since boot, anchored by /proc/uptime — never ps's
// lstart, whose words are the locale's and whose pass busybox rejects. The ps
// on PATH cannot answer at all, and the child still reads with its parent,
// comm and argv and a start at the spawn, not a cannot-tell zero.
func TestProcessTableReadsStartsFromProcWithoutPs(t *testing.T) {
	psThatCannotList(t)

	before := time.Now().Add(-time.Second)
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	tab := processTable()
	r, ok := tab[cmd.Process.Pid]
	if !ok || r.ppid != os.Getpid() || r.comm != "sleep" || r.args != "/bin/sleep 30" {
		t.Fatalf("the child must be read from /proc with its parent, comm and argv, got %+v", r)
	}
	if r.start.IsZero() || r.start.Before(before) || r.start.After(time.Now().Add(time.Second)) {
		t.Fatalf("the child must be read with the time it started, got %v, no earlier than %v", r.start, before)
	}
	if self, ok := tab[os.Getpid()]; !ok || self.ppid != os.Getppid() || self.start.IsZero() {
		t.Fatalf("this process must sit under its parent with a start, got %+v", self)
	}
}
