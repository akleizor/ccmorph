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
func psThatCannotList(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\necho 'ps: unrecognized option: x' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return calls
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
	calls := psThatCannotList(t)

	// A live same-user process now holds a dead launcher's number, and its
	// command line names claude: only its start can tell that it is not the
	// launcher the relay names (#189, #206).
	recycled := holdProc(t, os.Args[0], filepath.Join(t.TempDir(), "users"),
		"grep", "-r", "claude", "/notes")
	gone := exec.Command("/bin/sh", "-c", ":")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "tmp")
	os.MkdirAll(filepath.Join(state, "run"), 0o700)
	os.MkdirAll(tmp, 0o700)
	past := time.Now().Add(-2 * time.Minute)
	// The name's clock says the launch happened at the epoch; the process
	// holding the number started since, so it is not the launcher.
	recycledRelay := filepath.Join(state, ".reswitch."+strconv.Itoa(recycled.Process.Pid)+".1_000000.2")
	// A number nobody holds has no owner, whichever table is read.
	dead := filepath.Join(state, ".reswitch."+strconv.Itoa(gone.Process.Pid)+".1.2")
	// This process's own relay names a launch that just happened: its holder
	// started before the name's clock and must keep it.
	mine := filepath.Join(state, ".reswitch."+strconv.Itoa(os.Getpid())+"."+
		strconv.FormatInt(time.Now().Unix(), 10)+"_000000.2")
	for _, f := range []string{recycledRelay, dead, mine} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, past, past); err != nil {
			t.Fatal(err)
		}
	}

	GCStale(state, filepath.Join(root, "profiles"), tmp)

	if _, err := os.Stat(recycledRelay); !os.IsNotExist(err) {
		t.Errorf("%s must go: the launcher it names is dead and its number is now a process that started since (stat: %v)", recycledRelay, err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("%s must go: no process holds its launcher pid (stat: %v)", dead, err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("%s must stay: this live launcher queued it (stat: %v)", mine, err)
	}

	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Errorf("the sweep must not ask ps while /proc answers (log stat: %v)", err)
	}

	// Losing the table, or just this holder's start, must not turn doubt
	// into proof. Put the recycled relay back and run both cannot-tell cases.
	for _, c := range []struct {
		name   string
		read   func() (map[int]psRow, bool)
		wantPs bool
	}{
		{"unreadable table", func() (map[int]psRow, bool) { return nil, false }, true},
		{"unreadable start", func() (map[int]psRow, bool) {
			return map[int]psRow{recycled.Process.Pid: {}}, true
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			original := sysctlTable
			t.Cleanup(func() { sysctlTable = original })
			if err := os.WriteFile(recycledRelay, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(recycledRelay, past, past); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(calls); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			sysctlTable = c.read
			GCStale(state, filepath.Join(root, "profiles"), tmp)
			if _, err := os.Stat(recycledRelay); err != nil {
				t.Errorf("the relay must stay while the holder's start cannot be read: %v", err)
			}
			log, err := os.ReadFile(calls)
			if c.wantPs {
				if err != nil || len(log) == 0 {
					t.Errorf("the unreadable table must reach the ps fallback (log: %q, error: %v)", log, err)
				}
			} else if !os.IsNotExist(err) {
				t.Errorf("the readable table must not reach the ps fallback (log: %q, error: %v)", log, err)
			}
		})
	}
}

// GH-1051: on Linux a row's start is the kernel's own field 22 of
// /proc/<pid>/stat, ticks since boot, anchored by /proc/uptime — never ps's
// lstart, whose words are the locale's and whose pass busybox rejects. The ps
// on PATH cannot answer at all, and the child still reads with its parent,
// comm and argv and a start at the spawn, not a cannot-tell zero.
func TestProcessTableReadsStartsFromProcWithoutPs(t *testing.T) {
	calls := psThatCannotList(t)

	core := coreCopy(t)
	before := time.Now()
	// holdProc waits for a byte from the child after exec. Start alone can
	// return before Linux publishes the child's comm and argv.
	cmd := holdProc(t, core, filepath.Join(t.TempDir(), "users"), "test-holder", "two words", "last")
	ready := time.Now()
	tab := processTable()
	r, ok := tab[cmd.Process.Pid]
	if !ok || r.ppid != os.Getpid() || r.comm != "ccmorph-core" || r.args != "test-holder two words last" {
		t.Errorf("the child must be read from /proc with its parent, comm and argv, got %+v", r)
	}
	if r.start.IsZero() || r.start.Before(before.Add(-time.Second)) || r.start.After(ready.Add(time.Second)) {
		t.Errorf("the child must be read with the time it started, got %v (spawn %v to %v)", r.start, before, ready)
	}
	if self, ok := tab[os.Getpid()]; !ok || self.ppid != os.Getppid() || self.start.IsZero() {
		t.Errorf("this process must sit under its parent with a start, got %+v", self)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Errorf("the table must not ask ps while /proc answers (log stat: %v)", err)
	}
}

// stat brackets comm in parens of its own, so the fields are read past the last ')'.
func TestProcessTableReadsACommWithSpacesAndParens(t *testing.T) {
	psThatCannotList(t)
	exe := namedCopy(t, "a) (b c")
	before := time.Now()
	cmd := holdProc(t, exe, filepath.Join(t.TempDir(), "users"), "holder")
	ready := time.Now()

	r, ok := processTable()[cmd.Process.Pid]
	if !ok || r.comm != "a) (b c" || r.ppid != os.Getpid() {
		t.Fatalf("the child's comm and parent must be read from /proc, got %+v", r)
	}
	if r.start.IsZero() || r.start.Before(before.Add(-time.Second)) || r.start.After(ready.Add(time.Second)) {
		t.Errorf("the child must be read with the time it started, got %v (spawn %v to %v)", r.start, before, ready)
	}
}
