package util

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sharedCore is a stand-in `core adapter`: it answers PORT=<its pid> and runs
// until it is signalled, keeping its argv (the --shared marker) as the real
// adapter does.
func sharedCore(t *testing.T) string {
	t.Helper()
	return fakeCore(t, `trap 'kill $!; exit 0' TERM; echo PORT=$$; sleep 60 & wait`)
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
