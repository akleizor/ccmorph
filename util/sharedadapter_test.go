package util

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
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

// The proof is the adapter command itself, argument by argument: a process
// that merely carries `--shared <base>` among its operands — the words a
// `sh -c` script receives as junk — is provably not this key's adapter, is
// never signalled, and the stale key's files still go (#1020).
func TestSweepNeverSignalsWhatOnlyCarriesTheAdapterArguments(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	base := AdapterBase(root, "gw", AdapterKey("gw", "old", conf, core))
	carrier := exec.Command("/bin/sh", "-c", "echo ready; read gone", "unrelated-command", "--shared", base)
	in, err := carrier.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := carrier.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := carrier.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil { // the carrier is up and waiting on stdin
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = carrier.Wait(); close(done) }()
	defer func() { in.Close(); <-done }()
	files := map[string]string{base: fmt.Sprintf("%d 4242", carrier.Process.Pid), base + ".lock": "", base + ".users": "", base + ".log": "old log"}
	for f, content := range files {
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := acquire(t, core, root, "m", conf) // this launch sweeps the stale key
	defer a.Release()
	if !alive(carrier.Process.Pid) {
		t.Fatal("the sweep signalled a process that only carries the adapter arguments")
	}
	for f := range files {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Fatalf("a provably stale key left %s behind: %v", filepath.Base(f), err)
		}
	}
}

// A prefix match whose key part is no key's shape — not the lowercase hex
// AdapterKey makes — was never written by a key, however well its lock opens,
// and the sweep leaves it (#1020).
func TestSweepLeavesMatchesThatAreNotKeys(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	dead := acquire(t, core, root, "m2", conf)
	deadBase := AdapterBase(root, "gw", AdapterKey("gw", "m2", conf, core))
	dead.Release()
	waitExit(t, dead.PID)
	stray := filepath.Join(root, ".gw.adapter.not-a-key.log")
	os.WriteFile(filepath.Join(root, ".gw.adapter.not-a-key.lock"), nil, 0o600)
	os.WriteFile(stray, []byte("keep"), 0o600)

	a := acquire(t, core, root, "m1", conf) // the sweep runs on this launch
	defer a.Release()

	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("a glob match that is no key's file set was removed: %v", err)
	}
	if _, err := os.Stat(deadBase + ".lock"); err == nil {
		t.Fatal("a key of the real shape must still be swept")
	}
}

// The sweep scans the auth root it was given, literally: the root is
// user-settable, and a pattern that parses it as syntax deletes whatever
// sibling a metacharacter happens to match instead (#848, #1020).
func TestSweepScansTheAuthRootLiterally(t *testing.T) {
	core, parent := sharedCore(t), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", filepath.Join(parent, "state[1]"))
	root := filepath.Join(paths.ProfileRoot(), "auth")
	sibling := filepath.Join(parent, "state1", "auth") // what an unquoted `[1]` matches
	conf := filepath.Join(parent, "gw.conf")
	os.WriteFile(conf, []byte("base_url=x\n"), 0o600)
	old := AdapterKey("gw", "old", conf, core)
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{".lock", ".users", ".log"} {
			if err := os.WriteFile(AdapterBase(dir, "gw", old)+s, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	a := acquire(t, core, root, "m", conf) // the sweep runs on this launch
	defer a.Release()

	for _, s := range []string{".lock", ".users", ".log"} {
		if _, err := os.Lstat(AdapterBase(root, "gw", old) + s); !os.IsNotExist(err) {
			t.Errorf("the selected root's stale %s was not swept: %v", s, err)
		}
		if b, err := os.ReadFile(AdapterBase(sibling, "gw", old) + s); err != nil || string(b) != s {
			t.Errorf("a sibling root's %s was touched: %q %v", s, b, err)
		}
	}
}
