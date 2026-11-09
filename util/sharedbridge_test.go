package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// bridgeProc starts a stand-in for a bridge: a sleeper whose argv carries the
// rendered config path, as cliproxyapi's -config argument does, so the
// ownership check reads it as ours.
func bridgeProc(t *testing.T, tmp string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.Args[0] = filepath.Join(tmp, "config.yaml")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { cmd.Process.Kill() })
	if !ours(cmd.Process.Pid, tmp) {
		t.Fatalf("the fake bridge (pid %d, tmp %s) must read as ours", cmd.Process.Pid, tmp)
	}
	return cmd
}

// stubProbe makes every launch this test starts ask f instead of a real port.
func stubProbe(t *testing.T, f func(port, key string) bool) {
	t.Helper()
	probeBridge = f
	t.Cleanup(func() { probeBridge = listsModel })
}

// Two sessions of one provider profile share one bridge; the last one out stops it (#250).
// The bridge is a sleep process plus an in-process listing: bridgeStart has its own test.
//
// Same check as on main (util/sharedbridge_test.go:17 there): one start for two
// sessions, the first release leaves it running, the last stops it and removes
// the handle, and a later launch starts a new one. Two things moved: the
// listing is a stub because a sandbox may refuse loopback ports, and the fake
// bridge carries <tmp>/config.yaml in its argv because Release now re-reads
// the process table before it signals a PID (#294).
func TestTwoSessionsShareOneBridge(t *testing.T) {
	for _, parent := range []string{"tmp", "Application Support"} {
		t.Run(parent, func(t *testing.T) {
			root := t.TempDir()
			stubProbe(t, func(port, key string) bool { return port == "4100" && key == "k" })
			starts := 0
			var procs []*exec.Cmd
			start := func() (pid, port, key, tmp string, ok bool) {
				starts++
				tmp = filepath.Join(root, parent, "ccmorph-bridge-"+strconv.Itoa(starts))
				procs = append(procs, bridgeProc(t, tmp))
				return strconv.Itoa(procs[len(procs)-1].Process.Pid), "4100", "k", tmp, true
			}
			t.Cleanup(func() {
				for _, c := range procs {
					c.Process.Kill()
				}
			})
			auth := filepath.Join(root, "p")
			a, ok1, err1 := AcquireBridge(root, "p", start)
			b, ok2, err2 := AcquireBridge(root, "p", start)
			if !ok1 || !ok2 || err1 != nil || err2 != nil || starts != 1 || a.PID != b.PID {
				t.Fatalf("want one bridge for two sessions: ok=%v,%v err=%v,%v starts=%d", ok1, ok2, err1, err2, starts)
			}
			pid, _ := strconv.Atoi(a.PID)
			a.Release(auth)
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("the bridge must keep running while a session still uses it")
			}
			b.Release(auth)
			if !waitGone(pid, 3e9) {
				t.Fatal("the last session out must stop the bridge")
			}
			if _, err := os.Stat(filepath.Join(root, ".p.bridge")); err == nil {
				t.Fatal("the handle must be removed with the bridge")
			}
			c, _, _ := AcquireBridge(root, "p", start)
			defer c.Release(auth)
			if starts != 2 {
				t.Fatal("a launch after the last release must start a new bridge")
			}
		})
	}
}

// A recorded bridge that does not answer the probe while another session holds
// it is joined, not killed and doubled (#294).
func TestASlowBridgeAnotherSessionHoldsIsJoinedNotKilled(t *testing.T) {
	root := t.TempDir()
	stubProbe(t, func(port, key string) bool { return true })
	starts := 0
	start := func() (pid, port, key, tmp string, ok bool) {
		starts++
		tmp = filepath.Join(root, "ccmorph-bridge-"+strconv.Itoa(starts))
		cmd := bridgeProc(t, tmp)
		return strconv.Itoa(cmd.Process.Pid), "4100", "k", tmp, true
	}
	auth := filepath.Join(root, "p")
	a, ok, err := AcquireBridge(root, "p", start)
	if !ok || err != nil {
		t.Fatalf("first launch: ok=%v err=%v", ok, err)
	}
	stubProbe(t, func(port, key string) bool { return false }) // the bridge goes quiet
	b, ok2, err2 := AcquireBridge(root, "p", start)
	if !ok2 || err2 != nil || starts != 1 || b.PID != a.PID {
		t.Fatalf("a slow bridge another session holds must be joined: ok=%v err=%v starts=%d pid=%s/%s", ok2, err2, starts, a.PID, b.PID)
	}
	pid, _ := strconv.Atoi(a.PID)
	a.Release(auth)
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the slow bridge must keep running while a session still uses it")
	}
	b.Release(auth)
	if !waitGone(pid, 3e9) {
		t.Fatal("the last session out must stop the slow bridge")
	}
}

// A recorded bridge that never answers and that no session holds is replaced
// after a bounded retry, not joined.
func TestAnUnansweringBridgeNobodyHoldsIsReplaced(t *testing.T) {
	root := t.TempDir()
	stubProbe(t, func(port, key string) bool { return true })
	starts := 0
	start := func() (pid, port, key, tmp string, ok bool) {
		starts++
		tmp = filepath.Join(root, "ccmorph-bridge-"+strconv.Itoa(starts))
		cmd := bridgeProc(t, tmp)
		return strconv.Itoa(cmd.Process.Pid), "4100", "k", tmp, true
	}
	auth := filepath.Join(root, "p")
	a, ok, err := AcquireBridge(root, "p", start)
	if !ok || err != nil {
		t.Fatalf("first launch: ok=%v err=%v", ok, err)
	}
	old, _ := strconv.Atoi(a.PID)
	a.users.Close() // the first session ends without Release
	a.users = nil
	probes := 0
	stubProbe(t, func(port, key string) bool { probes++; return false })
	b, ok2, err2 := AcquireBridge(root, "p", start)
	if !ok2 || err2 != nil || starts != 2 || b.PID == a.PID {
		t.Fatalf("a dead bridge nobody holds must be replaced: ok=%v err=%v starts=%d", ok2, err2, starts)
	}
	if !waitGone(old, 3e9) {
		t.Fatal("the unanswering bridge must be stopped once no session holds it")
	}
	if probes != bridgeProbeTries {
		t.Fatalf("want %d bounded probe tries, got %d", bridgeProbeTries, probes)
	}
	b.Release(auth)
}

// The last session out signals a PID only while it still runs this bridge;
// a PID the system reused belongs to someone else (#294).
func TestReleaseNeverSignalsAPidNotRunningTheBridge(t *testing.T) {
	root := t.TempDir()
	stubProbe(t, func(port, key string) bool { return true })
	start := func() (pid, port, key, tmp string, ok bool) {
		tmp = filepath.Join(root, "ccmorph-bridge-1")
		cmd := bridgeProc(t, tmp)
		return strconv.Itoa(cmd.Process.Pid), "4100", "k", tmp, true
	}
	auth := filepath.Join(root, "p")
	a, ok, err := AcquireBridge(root, "p", start)
	if !ok || err != nil {
		t.Fatalf("first launch: ok=%v err=%v", ok, err)
	}
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	go other.Wait()
	t.Cleanup(func() { other.Process.Kill() })
	a.PID = strconv.Itoa(other.Process.Pid) // the bridge died and the PID came back
	a.Release(auth)
	if syscall.Kill(other.Process.Pid, 0) != nil {
		t.Fatal("release must never signal a PID that does not run this bridge")
	}
	if _, err := os.Stat(a.TmpDir); err == nil {
		t.Fatal("release still removes the bridge's config dir")
	}
}

// A session that outlives its profile's rm puts none of the bridge files back.
func TestReleaseAfterRmLeavesNoBridgeFile(t *testing.T) {
	root := t.TempDir()
	stubProbe(t, func(port, key string) bool { return true })
	start := func() (pid, port, key, tmp string, ok bool) {
		tmp = filepath.Join(root, "ccmorph-bridge-1")
		cmd := bridgeProc(t, tmp)
		return strconv.Itoa(cmd.Process.Pid), "4100", "k", tmp, true
	}
	auth := filepath.Join(root, "p")
	a, ok, err := AcquireBridge(root, "p", start)
	if !ok || err != nil {
		t.Fatalf("first launch: ok=%v err=%v", ok, err)
	}
	for _, p := range BridgeFiles(root, "p") {
		if os.Remove(p) != nil {
			t.Fatalf("rm removes %s", p)
		}
	}
	a.Release(auth)
	if left, _ := filepath.Glob(filepath.Join(root, ".p.bridge*")); len(left) != 0 {
		t.Fatalf("release recreated bridge state after rm: %v", left)
	}
}

// A bridge whose handle could not be written is stopped before the launch
// reports, so the next launch never finds two bridges on one auth dir (#294).
func TestAnUnrecordedBridgeIsStopped(t *testing.T) {
	root := t.TempDir()
	stubProbe(t, func(port, key string) bool { return true })
	started := 0
	start := func() (pid, port, key, tmp string, ok bool) {
		tmp = filepath.Join(root, "ccmorph-bridge-1")
		cmd := bridgeProc(t, tmp)
		started = cmd.Process.Pid
		return strconv.Itoa(started), "4100", "k", tmp, true
	}
	// The handle path is a non-empty directory, so reading and writing it fail.
	if err := os.MkdirAll(filepath.Join(root, ".p.bridge", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, ok, err := AcquireBridge(root, "p", start)
	if ok || err == nil || err == ErrBridgeNotStarted {
		t.Fatalf("a handle that cannot be written must fail the launch: ok=%v err=%v", ok, err)
	}
	if started == 0 || !waitGone(started, 3e9) {
		t.Fatal("the unrecorded bridge must be stopped")
	}
}

// A handle nobody can read while another session holds the bridge is refused
// loudly: a second bridge on the same auth dir would sign the profile out.
func TestAnUnreadableRecordWhileHeldIsRefusedLoudly(t *testing.T) {
	root := t.TempDir()
	stubProbe(t, func(port, key string) bool { return true })
	starts := 0
	start := func() (pid, port, key, tmp string, ok bool) {
		starts++
		cmd := bridgeProc(t, filepath.Join(root, "ccmorph-bridge-"+strconv.Itoa(starts)))
		return strconv.Itoa(cmd.Process.Pid), "4100", "k", filepath.Dir(cmd.Args[0]), true
	}
	auth := filepath.Join(root, "p")
	a, ok, err := AcquireBridge(root, "p", start)
	if !ok || err != nil {
		t.Fatalf("first launch: ok=%v err=%v", ok, err)
	}
	pid, _ := strconv.Atoi(a.PID)
	if os.WriteFile(filepath.Join(root, ".p.bridge"), []byte("garbage"), 0o600) != nil {
		t.Fatal("could not write the garbage handle")
	}
	_, ok2, err2 := AcquireBridge(root, "p", start)
	if ok2 || err2 == nil || err2 == ErrBridgeNotStarted {
		t.Fatalf("an unreadable record while held must be refused: ok=%v err=%v", ok2, err2)
	}
	if starts != 1 {
		t.Fatalf("no second bridge may start: starts=%d", starts)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the bridge another session holds must keep running")
	}
	a.Release(auth)
}
