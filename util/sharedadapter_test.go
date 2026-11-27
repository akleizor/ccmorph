package util

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sharedCore is a stand-in `core adapter`: it answers PORT=<its pid> and runs
// until it is signalled, keeping its argv (the --shared marker) as the real
// adapter does. Every other verb is the real core's: AcquireAdapter spawns the
// session's route-hold through this same path, and only a core's own
// `util route-hold` is the marker a hop reads (#1022).
func sharedCore(t *testing.T) string {
	t.Helper()
	core := coreCopy(t)
	return fakeCore(t, `if [ "$1" = util ]; then exec '`+core+`' "$@"; fi
trap 'kill $!; exit 0' TERM; echo PORT=$$; sleep 60 & wait`)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitExit(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if !alive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid %d still runs", pid)
}

func acquire(t *testing.T, core, root, model, conf string) *SharedAdapter {
	t.Helper()
	a, ok := AcquireAdapter(core, root, "gw", model, conf, io.Discard)
	if !ok {
		t.Fatal("adapter did not start")
	}
	return a
}

func TestSessionsWithOneKeyShareOneAdapter(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	a := acquire(t, core, root, "m1", conf)
	b := acquire(t, core, root, "m1[1m]", conf) // the context marker is no model
	if a.PID != b.PID || a.Port != b.Port || a.PID == 0 {
		t.Fatalf("two sessions, one key: pids %d and %d", a.PID, b.PID)
	}
	// A different model, and a rotated conf, are other adapters.
	c := acquire(t, core, root, "m2", conf)
	os.WriteFile(conf, []byte("base_url=y\n"), 0o600)
	d := acquire(t, core, root, "m1", conf)
	if c.PID == a.PID || d.PID == a.PID || c.PID == d.PID {
		t.Fatalf("pids %d %d %d must differ", a.PID, c.PID, d.PID)
	}
	for _, s := range []*SharedAdapter{a, b, c, d} {
		s.Release()
	}
	for _, s := range []*SharedAdapter{a, c, d} {
		waitExit(t, s.PID)
	}
}

func TestTheLastReleaseStopsTheAdapterAndNoLauncherExitKillsOneInUse(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	a := acquire(t, core, root, "m", conf)
	b := acquire(t, core, root, "m", conf)
	base := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core))
	a.Release() // the session that started it ends first
	time.Sleep(200 * time.Millisecond)
	if !alive(a.PID) {
		t.Fatal("the starter's release killed an adapter another session holds")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatal("the record must stay while a session holds the adapter")
	}
	b.Release()
	waitExit(t, a.PID)
	if _, err := os.Stat(base); err == nil {
		t.Fatal("the last release must remove the record")
	}
	// The next session starts a fresh one.
	c := acquire(t, core, root, "m", conf)
	defer c.Release()
	if c.PID == a.PID || !alive(c.PID) {
		t.Fatalf("pid %d after %d", c.PID, a.PID)
	}
}

func TestAnAdapterWhoseSessionsAllVanishedEndsByItself(t *testing.T) {
	root := t.TempDir()
	base := AdapterBase(root, "gw", "k")
	os.WriteFile(base+".lock", nil, 0o600)
	os.WriteFile(base, []byte("4242 9"), 0o600)
	// A holder, as a live session: the adapter is not idle.
	holder, err := lockFile(base+".users", syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	if SharedAdapterIdle(base, 4242) {
		t.Fatal("a held adapter reads as idle")
	}
	// The holder is SIGKILLed: the kernel closes its lock.
	holder.Close()
	if SharedAdapterIdle(base, 1) {
		// the record names another adapter: it goes unremoved
		if _, err := os.Stat(base); err != nil {
			t.Fatal("a record naming another adapter must stay")
		}
	}
	if !SharedAdapterIdle(base, 4242) {
		t.Fatal("an adapter nobody holds must read as idle")
	}
	if _, err := os.Stat(base); err == nil {
		t.Fatal("an idle adapter removes its own record")
	}
}

func TestAnAdapterWhoseFilesAreGoneEnds(t *testing.T) {
	if !SharedAdapterIdle(filepath.Join(t.TempDir(), ".gone"), 1) {
		t.Fatal("rm took the files: nothing is left to wait for")
	}
}

// A session that outlives its profile's rm puts none of the adapter files back:
// the session hold's end watches the hold's pid, never the users file, so a
// path rm removed is not re-created by the release (#1022).
func TestReleaseAfterRmLeavesNoAdapterFile(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	a := acquire(t, core, root, "m", conf)
	for _, p := range AdapterFiles(root, "gw") {
		if err := os.Remove(p); err != nil {
			t.Fatalf("rm removes %s: %v", p, err)
		}
	}
	a.Release()
	if left, _ := filepath.Glob(filepath.Join(root, ".gw.adapter.*")); len(left) != 0 {
		t.Fatalf("release recreated adapter state after rm: %v", left)
	}
}

// TestAdapterProfileFromUsers pins the parse that turns a session hold's users
// file back into a profile name: AdapterBase's `.<name>.adapter.<key>` shape
// and nothing else, so a provider route's users file names no profile.
func TestAdapterProfileFromUsers(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/r/.gw.adapter.0123456789abcdef.users", "gw"},
		{"/r/.gw.adapter.k.users", "gw"},
		{"/r/gw.adapter.k.users", ""},    // no leading dot
		{"/r/.gw.users", ""},             // no .adapter. segment
		{"/r/.gw.adapter..users", ""},    // empty key
		{"/r/..adapter.k.users", ""},     // empty name
		{"/r/.gw.adapter.k.lock", ""},    // not a users file
		{"/r/.gw.adapter.k", ""},         // not a users file
		{"/r/.a\\b.adapter.k.users", ""}, // a name holds no slash
		{"", ""},
	} {
		if got := adapterProfileFromUsers(tc.path); got != tc.want {
			t.Errorf("adapterProfileFromUsers(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestEndSessionHoldSignalsOnlyTheSessionsHold pins the ownership proof before
// the TERM: a reused PID, another program spelling the same words, a core hold
// on another file or with an extra operand, and no pid at all are left running;
// only the one process whose executable and exact operands are the session's
// hold is ended (#1022, #408).
func TestEndSessionHoldSignalsOnlyTheSessionsHold(t *testing.T) {
	users := AdapterBase(t.TempDir(), "gw", "0123456789abcdef") + ".users"
	t.Run("a PID the system reused", func(t *testing.T) {
		other := exec.Command("sleep", "60")
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
		go other.Wait()
		t.Cleanup(func() { other.Process.Kill() })
		endSessionHold(other.Process.Pid, users)
		if waitGone(other.Process.Pid, 1e9) {
			t.Fatal("endSessionHold must never signal a PID that does not run the session's hold")
		}
	})
	core, self := coreCopy(t), os.Args[0]
	survives := func(name string, start func(t *testing.T) *exec.Cmd) {
		t.Run(name, func(t *testing.T) {
			p := start(t)
			endSessionHold(p.Process.Pid, users)
			if waitGone(p.Process.Pid, 1e9) {
				t.Fatal("endSessionHold must never signal a process that is not the session's own hold")
			}
		})
	}
	survives("another program with the words in one argument", func(t *testing.T) *exec.Cmd {
		return holdProc(t, self, users, self, "-c", "x", "util route-hold "+users)
	})
	survives("another program under the name ccmorph-core", func(t *testing.T) *exec.Cmd {
		return holdProc(t, self, users, core, "util", "route-hold", users, routeHoldExitStdin)
	})
	survives("ccmorph-core holding another users file", func(t *testing.T) *exec.Cmd {
		return holdProc(t, core, users+"x", core, "util", "route-hold", users+"x", routeHoldExitStdin)
	})
	survives("ccmorph-core with an extra operand", func(t *testing.T) *exec.Cmd {
		return holdProc(t, core, users, core, "util", "route-hold", users, routeHoldExitStdin, "more")
	})
	for _, pid := range []int{0, -1} {
		endSessionHold(pid, users) // no pid, no proof, no signal
	}
	sleep := exec.Command("sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	go sleep.Wait()
	t.Cleanup(func() { sleep.Process.Kill() })
	endSessionHold(sleep.Process.Pid, "") // no users file, no proof
	if waitGone(sleep.Process.Pid, 1e9) {
		t.Fatal("endSessionHold with no users file must signal nothing")
	}
	t.Run("the session's own hold", func(t *testing.T) {
		h := holdProc(t, core, users, core, "util", "route-hold", users, routeHoldExitStdin)
		endSessionHold(h.Process.Pid, users)
		if !waitGone(h.Process.Pid, 3e9) {
			t.Fatal("the session's own hold must still be ended")
		}
	})
}

// TestAHoldThatCannotLandRefusesTheAcquisition pins the refusal: a session
// without its hold is exactly the session `ccmorph hop` can no longer name, so
// an acquisition whose hold does not land is refused rather than going on
// without #257 — and the adapter it started is stopped and unrecorded (#1022).
func TestAHoldThatCannotLandRefusesTheAcquisition(t *testing.T) {
	root := t.TempDir()
	real := coreCopy(t)
	flag := filepath.Join(t.TempDir(), "nohold")
	core := fakeCore(t, `if [ "$1" = util ]; then [ -e '`+flag+`' ] && exit 1; exec '`+real+`' "$@"; fi
trap 'kill $!; exit 0' TERM; echo PORT=$$; sleep 60 & wait`)
	conf := filepath.Join(t.TempDir(), "gw.conf")
	if err := os.WriteFile(conf, []byte("base_url=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core))
	t.Run("starter", func(t *testing.T) {
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(flag) })
		var errw bytes.Buffer
		a, ok := AcquireAdapter(core, root, "gw", "m", conf, &errw)
		if ok || a == nil || a.PID == 0 {
			t.Fatalf("an acquisition without a hold must be refused, got ok=%v pid=%d", ok, a.PID)
		}
		if !strings.Contains(errw.String(), "could not keep a hold on the shared adapter for 'gw'") {
			t.Fatalf("the refusal must say why, got %q", errw.String())
		}
		waitExit(t, a.PID) // the adapter it had just started is stopped
		if _, err := os.Stat(base); err == nil {
			t.Fatal("a refused acquisition must leave no record")
		}
	})
	t.Run("joiner", func(t *testing.T) {
		first := acquire(t, core, root, "m", conf)
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			first.Release()
			t.Fatal(err)
		}
		var errw bytes.Buffer
		_, ok := AcquireAdapter(core, root, "gw", "m", conf, &errw)
		if ok {
			first.Release()
			t.Fatal("a joining acquisition without a hold must be refused")
		}
		if !strings.Contains(errw.String(), "could not keep a hold on the shared adapter for 'gw'") {
			first.Release()
			t.Fatalf("the refusal must say why, got %q", errw.String())
		}
		if !alive(first.PID) {
			first.Release()
			t.Fatal("the failed joiner must not end the adapter another session holds")
		}
		raw, err := os.ReadFile(base)
		if pid, _, ok := parseAdapterRecord(string(raw)); err != nil || !ok || pid != first.PID {
			first.Release()
			t.Fatalf("the record must still name the first session's adapter, got %q (%v)", raw, err)
		}
		first.Release()
		waitExit(t, first.PID) // no lock left behind by the joiner: the last user out stops it
		if _, err := os.Stat(base); err == nil {
			t.Fatal("the last session's release must remove the record")
		}
	})
}
