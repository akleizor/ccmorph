package util

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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

// rebuildCore writes a new build of the core at the same path: a different
// size, so a different key, with the same shared-adapter manners.
func rebuildCore(t *testing.T, core string, n int) {
	t.Helper()
	body := "#!/bin/sh\ntrap 'kill $!; exit 0' TERM; echo PORT=$$; sleep 60 & wait #" + strings.Repeat("x", n) + "\n"
	if err := os.WriteFile(core, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// GH-1020: a core rebuild or a conf edit makes a new key, and the dead key's
// empty lock and users files and its last log must not pile up across
// upgrades — the issue's check: after two rebuilds and a launch, only the
// live key's files are left.
func TestALaunchTakesADeadKeysFilesWithIt(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	for i := 1; i <= 3; i++ {
		if i > 1 {
			rebuildCore(t, core, i) // an upgrade makes a new key
		}
		a := acquire(t, core, root, "m", conf)
		a.Release()
		waitExit(t, a.PID)
	}
	live := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core))
	for _, f := range AdapterFiles(root, "gw") {
		if f != live && !strings.HasPrefix(f, live+".") {
			t.Fatalf("a dead key's file survived the launch: %s", f)
		}
	}
	for _, s := range []string{".lock", ".users", ".log"} {
		if _, err := os.Stat(live + s); err != nil {
			t.Fatalf("the live key's own %s must stay: %v", s, err)
		}
	}
}

// A sweep takes only dead keys: a live session's key, one whose mutex a launch
// is on, one whose users lock a session holds, and another profile's files all
// stay where they are.
func TestSweepLeavesKeysThatSomeoneStillUses(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	live := acquire(t, core, root, "m1", conf) // this session stays on the key
	defer live.Release()
	dead := acquire(t, core, root, "m2", conf)
	deadBase := AdapterBase(root, "gw", AdapterKey("gw", "m2", conf, core))
	dead.Release() // the last m2 session ends: its files are now the sweep's to take
	waitExit(t, dead.PID)
	// A launch in flight on a third key: it holds that key's mutex.
	busy := AdapterBase(root, "gw", "aabbccddeeff0011")
	mu, err := lockFile(busy+".lock", syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer mu.Close()
	// A fourth key a session is using: its mutex is free, its users lock held.
	held := AdapterBase(root, "gw", "11223344556677ff")
	os.WriteFile(held+".lock", nil, 0o600)
	os.WriteFile(held+".log", []byte("x"), 0o600)
	users, err := lockFile(held+".users", syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	defer users.Close()
	// Another profile's files are nobody's to sweep.
	other := filepath.Join(root, ".other.adapter.aabbccddeeff0011.lock")
	os.WriteFile(other, nil, 0o600)

	joiner := acquire(t, core, root, "m1", conf) // the sweep runs on this launch
	defer joiner.Release()

	for _, f := range []string{deadBase, deadBase + ".lock", deadBase + ".users", deadBase + ".log"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("a dead key's %s survived the sweep", filepath.Base(f))
		}
	}
	liveBase := AdapterBase(root, "gw", AdapterKey("gw", "m1", conf, core))
	for _, f := range []string{liveBase + ".lock", liveBase + ".users", liveBase + ".log", busy + ".lock", held + ".users", held + ".log", other} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s is not the sweep's to take", filepath.Base(f))
		}
	}
}

// Sessions SIGKILLed without releasing leave their adapter running with
// nobody holding the users lock; the next launch stops it and takes the whole
// key with it.
func TestSweepStopsTheAdapterItsDeadSessionsLeftBehind(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	a := acquire(t, core, root, "m", conf)
	old := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core))
	a.users.Close() // the session dies without releasing: the kernel lifts the lock
	a.users = nil
	if !alive(a.PID) {
		t.Fatal("the test needs the adapter to still run here")
	}
	rebuildCore(t, core, 9) // an upgrade makes a new key
	b := acquire(t, core, root, "m", conf)
	defer b.Release()
	waitExit(t, a.PID) // the launch stopped the adapter nobody uses
	for _, f := range AdapterFiles(root, "gw") {
		if b := filepath.Base(f); b == filepath.Base(old) || strings.HasPrefix(b, filepath.Base(old)+".") {
			t.Fatalf("the swept key left %s behind", f)
		}
	}
}

// A pid the process table cannot answer for is alive, and a pid that runs
// something else is never stopped: doubt leaves a swept key whole for the
// next launch, while a pid proven not to run it takes the key's files without
// a signal.
func TestSweepNeverSignalsWhatTheTableCannotProve(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	a := acquire(t, core, root, "m", conf)
	old := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core))
	a.users.Close() // the session dies without releasing
	a.users = nil
	rebuildCore(t, core, 3)
	orig := argvOfPID
	defer func() { argvOfPID = orig }()
	argvOfPID = func(int) ([]string, bool) { return nil, false } // a table that reads no argv
	b := acquire(t, core, root, "m", conf)
	defer b.Release()
	if !alive(a.PID) {
		t.Fatal("an adapter the table cannot answer for was stopped")
	}
	if _, err := os.Stat(old + ".lock"); err != nil {
		t.Fatal("a key the table cannot answer for must stay whole")
	}
	rebuildCore(t, core, 30)
	argvOfPID = func(int) ([]string, bool) { return []string{"something", "else"}, true } // a reused pid
	c := acquire(t, core, root, "m", conf)
	argvOfPID = orig // the releases below must see the real table
	defer c.Release()
	if _, err := os.Stat(old + ".lock"); err == nil {
		t.Fatal("a record whose pid provably runs something else is dead: the key's files go")
	}
	if !alive(a.PID) {
		t.Fatal("a pid that does not run this key's adapter must never be signalled")
	}
	stopProcess(a.PID) // the orphan the test kept alive on purpose
}
