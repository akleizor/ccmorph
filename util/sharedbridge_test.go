package util

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
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

// RetireRoute signals the bridge its handle names only while the process
// table shows that PID running it: a number the system reused belongs to
// someone else, and the record may name a bridge another session still uses
// (#408).
func TestRetireRouteNeverSignalsAPidNotRunningTheBridge(t *testing.T) {
	root := t.TempDir()
	auth := filepath.Join(root, "p")
	tmp := filepath.Join(root, "ccmorph-bridge-1")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	record := BridgeFiles(root, "p")[0]
	sleep := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd
	}
	t.Run("the record names the handle", func(t *testing.T) {
		other := sleep(t)
		want := strconv.Itoa(other.Process.Pid) + " 4100 k " + tmp
		if err := os.WriteFile(record, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		RetireRoute(auth, strconv.Itoa(other.Process.Pid), tmp, "")
		if syscall.Kill(other.Process.Pid, 0) != nil {
			t.Fatal("retirement must never signal a PID that does not run this bridge")
		}
		if raw, err := os.ReadFile(record); err != nil || string(raw) != want {
			t.Fatalf("the record must stay for the bridge it names: %v %q", err, raw)
		}
		if _, err := os.Stat(tmp); err != nil {
			t.Fatal("the config dir of a bridge that may still run must stay")
		}
	})
	t.Run("no auth directory", func(t *testing.T) {
		other := sleep(t)
		RetireRoute("", strconv.Itoa(other.Process.Pid), tmp, "")
		if syscall.Kill(other.Process.Pid, 0) != nil {
			t.Fatal("retirement must never signal a PID that does not run this bridge")
		}
	})
	t.Run("a PID that is gone", func(t *testing.T) {
		gone := exec.Command("sleep", "60")
		if err := gone.Start(); err != nil {
			t.Fatal(err)
		}
		gone.Process.Kill()
		gone.Wait()
		want := strconv.Itoa(gone.Process.Pid) + " 4100 k " + tmp
		if err := os.WriteFile(record, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		RetireRoute(auth, strconv.Itoa(gone.Process.Pid), tmp, "")
		if _, err := os.Stat(record); err == nil {
			t.Fatal("a dead bridge's record must not outlive it")
		}
		if _, err := os.Stat(tmp); err == nil {
			t.Fatal("a dead bridge's config dir must not outlive it")
		}
	})
}

// TestMain lets a test run this binary as a route hold: with
// CCMORPH_TEST_HOLD_USERS set it takes routeHold's hold on that file, whatever
// its argv says, and runs no test.
func TestMain(m *testing.M) {
	if users := os.Getenv("CCMORPH_TEST_HOLD_USERS"); users != "" {
		os.Exit(routeHold([]string{users}, nil, os.Stdout, io.Discard))
	}
	os.Exit(m.Run())
}

// coreCopy is this test binary under the name ccmorph-core: a hard link, or a
// copy where the temp dir is on another volume. Never a symlink: darwin names a
// process after the file a symlink resolves to, so a linked holder would read
// as util.test, never as the core (#408).
func coreCopy(t *testing.T) string {
	t.Helper()
	core := filepath.Join(t.TempDir(), "ccmorph-core")
	if os.Link(os.Args[0], core) == nil {
		return core
	}
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(core, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return core
}

// holdProc runs this test binary from the file exe with argv as its whole argv,
// holding users (TestMain), and returns once the hold has landed.
func holdProc(t *testing.T, exe, users string, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(exe)
	cmd.Args = argv
	cmd.Env = append(os.Environ(), "CCMORPH_TEST_HOLD_USERS="+users)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	go cmd.Wait()
	t.Cleanup(func() { cmd.Process.Kill() })
	defer pr.Close()
	pr.SetReadDeadline(time.Now().Add(10 * time.Second))
	one := make([]byte, 1)
	if _, err := io.ReadFull(pr, one); err != nil || one[0] != '1' {
		t.Fatalf("the hold never landed: %v %q", err, one)
	}
	return cmd
}

// argvOf keeps the argument boundaries the joined command line loses: one
// argument holding spaces is one element (#408). The process it reads stays
// running until it is read (holdProc), because a process that exits on its own
// arguments has no command line left to read on Linux.
func TestArgvOfKeepsArgumentBoundaries(t *testing.T) {
	users := BridgeFiles(t.TempDir(), "p")[2]
	p := holdProc(t, os.Args[0], users, "argv0", "60", "a b", "c")
	argv, ok := argvOf(p.Process.Pid)
	if !ok || len(argv) != 4 || argv[0] != "argv0" || argv[1] != "60" || argv[2] != "a b" || argv[3] != "c" {
		t.Fatalf("argv must read one element per argument, got %q %v", argv, ok)
	}
}

// endHold signals a holder PID only while the process table shows the core's
// route hold on the users file, proven by the executable (comm ccmorph-core)
// and the exact operands: a number the system reused, or a program whose argv
// carries the same words, may be another session's process, whose hold — and
// the bridge that hold keeps alive — would end with it (#408).
func TestEndRouteHoldSignalsOnlyTheRouteHold(t *testing.T) {
	users := BridgeFiles(t.TempDir(), "p")[2]
	t.Run("a PID the system reused", func(t *testing.T) {
		other := exec.Command("sleep", "60")
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
		go other.Wait()
		t.Cleanup(func() { other.Process.Kill() })
		EndRouteHold(strconv.Itoa(other.Process.Pid), users)
		// A signalled sleep turns zombie until reaped, and Kill alone reads a
		// zombie as alive, so the check must reap: waitGone polls Wait4.
		if waitGone(other.Process.Pid, 1e9) {
			t.Fatal("endHold must never signal a PID that does not run the route's hold")
		}
	})
	core, self := coreCopy(t), os.Args[0]
	survives := func(name string, start func(t *testing.T) *exec.Cmd) {
		t.Run(name, func(t *testing.T) {
			p := start(t)
			EndRouteHold(strconv.Itoa(p.Process.Pid), users)
			if waitGone(p.Process.Pid, 1e9) {
				t.Fatal("endHold must never signal a process that is not the route's own hold")
			}
		})
	}
	survives("another program with the words in one argument", func(t *testing.T) *exec.Cmd {
		return holdProc(t, self, users, self, "-c", "import time; time.sleep(60)", "util route-hold "+users)
	})
	survives("another program under the name ccmorph-core", func(t *testing.T) *exec.Cmd {
		return holdProc(t, self, users, core, "util", "route-hold", users)
	})
	survives("ccmorph-core holding another users file", func(t *testing.T) *exec.Cmd {
		return holdProc(t, core, users+"x", core, "util", "route-hold", users+"x")
	})
	survives("ccmorph-core with an extra operand", func(t *testing.T) *exec.Cmd {
		return holdProc(t, core, users, core, "util", "route-hold", users, "more")
	})
	// The joined command line cannot tell this one operand from the words it
	// spells: the process holds another file whose name ends the way a route's
	// argv does.
	survives("ccmorph-core holding a file named like the route's argv", func(t *testing.T) *exec.Cmd {
		other := t.TempDir() + "/ccmorph-core util route-hold " + users
		if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
			t.Fatal(err)
		}
		return holdProc(t, core, other, core, "util", "route-hold", other)
	})
	t.Run("the route's own holder", func(t *testing.T) {
		h := holdProc(t, core, users, core, "util", "route-hold", users)
		EndRouteHold(strconv.Itoa(h.Process.Pid), users)
		if !waitGone(h.Process.Pid, 3e9) {
			t.Fatal("the route's own holder must still be ended")
		}
	})
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
