package util

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// The kernel read can answer two transient ways for the pid just started,
	// and production treats both as alive and retries: doubt — the read could
	// not answer at all, which answers() reads as alive (#409) — and the
	// fork/exec publish window, where the pid exists and has not exec'd the
	// bridge yet, so the read answers with the argv the child still carries
	// from its parent, certainly not ours for the microseconds before exec
	// publishes it. Production never reads a bridge it just started inside
	// that window; the fake bridge reads it immediately, so it waits through
	// both under a small deadline. It fails only on an answer that outlasts
	// them: owns() still says certainly-not-ours at the deadline — the one
	// answer that means this fake bridge really is not ours (#867).
	deadline := time.Now().Add(2 * time.Second)
	for {
		yours, known := owns(cmd.Process.Pid, tmp)
		if yours {
			break
		}
		if time.Now().After(deadline) {
			if known {
				t.Fatalf("the fake bridge (pid %d, tmp %s) must read as ours", cmd.Process.Pid, tmp)
			}
			break // doubt throughout: a read that never answered is not fatal here either
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cmd
}

// The fake bridge must not demand a certainty production never asks for.
// The kernel read answers two transient ways for a pid just started: doubt
// (#409) — #409 made owns() answer known=false when the read cannot answer
// for a live pid, the way procImage's two syscalls can leave it after the
// process left in between, and answers() treats doubt as alive and retries —
// and the fork/exec publish window: the pid exists but has not exec'd the
// bridge yet, so the read answers certainly-not-ours with the argv the child
// still carries from the parent. A one-shot check obeys either answer and
// failed this suite under gate load; the wait must outlast both (#867).
func TestBridgeProcWaitsThroughATransientRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(pid int) ([]string, bool)
	}{
		{"doubt", func(int) ([]string, bool) { return nil, false }},
		{"the parent's argv before exec", func(int) ([]string, bool) { return os.Args, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := filepath.Join(t.TempDir(), "ccmorph-bridge-1")
			orig := argvOfPID
			reads := 0
			argvOfPID = func(pid int) ([]string, bool) {
				reads++
				if reads == 1 {
					return tc.read(pid) // transient: the first read cannot answer for the bridge yet
				}
				return orig(pid)
			}
			t.Cleanup(func() { argvOfPID = orig })
			cmd := bridgeProc(t, tmp)
			if reads < 2 {
				t.Fatalf("one transient read must be waited out, not obeyed: reads=%d", reads)
			}
			if yours, known := owns(cmd.Process.Pid, tmp); !known || !yours {
				t.Fatal("the fake bridge must read as ours once the transient read is retried")
			}
		})
	}
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

// A launch's probe is a question about one pid, so it must not pay for every
// process on the machine: answers() asks owns() once per try, and a bridge that
// never answers must not buy three whole-table walks before it is called dead.
func TestAnswersDoesNotWalkTheWholeProcessTable(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "ccmorph-bridge-1")
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

	probes := 0
	stubProbe(t, func(port, key string) bool { probes++; return false })
	orig := sysctlTable
	walks := 0
	sysctlTable = func() (map[int]psRow, bool) { walks++; return orig() }
	t.Cleanup(func() { sysctlTable = orig })

	if answers(strconv.Itoa(cmd.Process.Pid), "4100", "k", tmp, false) {
		t.Fatal("a bridge that never answers must not read as serving")
	}
	if probes != bridgeProbeTries {
		t.Fatalf("the read must see the bounded retry run: %d probes, want %d", probes, bridgeProbeTries)
	}
	if walks != 0 {
		t.Fatalf("one probe of one pid walked the whole process table %d times, want 0", walks)
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

// A process table that cannot answer for the recorded bridge's PID proves
// nothing: the last session out must not read that as death and remove the
// record and config of a live bridge, or the next launch starts a second on
// the same auth dir (#409).
func TestReleaseKeepsALiveBridgeTheProcessTableCannotAnswerFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(pid int) ([]string, bool)
	}{
		// An unreadable table and a row without argv both reach the ownership
		// check as a read that answered nothing.
		{"unreadable table", func(int) ([]string, bool) { return nil, false }},
		{"row without argv", func(int) ([]string, bool) { return []string{}, false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			pid, _ := strconv.Atoi(a.PID)
			// The bridge runs; the table now answers nothing for its PID.
			orig := argvOfPID
			argvOfPID = func(int) ([]string, bool) { return tc.read(pid) }
			t.Cleanup(func() { argvOfPID = orig })
			a.Release(auth)
			if syscall.Kill(pid, 0) != nil {
				return // release stopped it, which also never orphans it
			}
			if _, err := os.Stat(a.TmpDir); err != nil {
				t.Fatal("release removed the config of a live bridge it could not identify")
			}
			if _, err := os.Stat(filepath.Join(root, ".p.bridge")); err != nil {
				t.Fatal("release removed the record of a live bridge it could not identify")
			}
			// The next launch must join the recorded bridge, never start a second.
			b, ok, err := AcquireBridge(root, "p", start)
			if !ok || err != nil || starts != 1 || b.PID != a.PID {
				t.Fatalf("the next launch must join the live bridge: ok=%v err=%v starts=%d pid=%s/%s", ok, err, starts, a.PID, b.PID)
			}
			b.Release(auth)
		})
	}
}

// A recorded PID that is certainly gone is dead, not doubt: the last session
// out still removes the record and config, so they do not outlive the bridge
// they describe (#409).
func TestReleaseCleansUpAfterABridgeThatIsGone(t *testing.T) {
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
	gone := exec.Command("sleep", "60")
	if err := gone.Start(); err != nil {
		t.Fatal(err)
	}
	gone.Process.Kill()
	gone.Wait()
	a.PID = strconv.Itoa(gone.Process.Pid) // the recorded bridge died
	a.Release(auth)
	if _, err := os.Stat(filepath.Join(root, ".p.bridge")); err == nil {
		t.Fatal("a dead bridge's record must not outlive it")
	}
	if _, err := os.Stat(a.TmpDir); err == nil {
		t.Fatal("a dead bridge's config dir must not outlive it")
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
// its argv says, and runs no test. It is also the fake security the keychain
// tests put on PATH under the name "security" (GH-830): that dispatch needs
// both variables, so an operator's env alone runs no test differently.
func TestMain(m *testing.M) {
	if state := os.Getenv(fakeSecurityEnv); state != "" && filepath.Base(os.Args[0]) == "security" {
		os.Exit(fakeSecurity(state, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
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

// defaultTestTimeout is go test's own default -timeout: the bound a run that set none still has.
const defaultTestTimeout = 10 * time.Minute

// holdWaitDeadline is when a holder that has not answered is called stuck: half of what the test
// binary's own -timeout has left (end; defaultTestTimeout from now when it has none). The only bound
// a holder cannot outgrow and the helper does not invent: a starved start or a delayed lock gets the
// whole budget, a stuck holder fails with a verdict before the suite's panic (#998).
func holdWaitDeadline(now, end time.Time, hasEnd bool) time.Time {
	if !hasEnd {
		end = now.Add(defaultTestTimeout)
	}
	return now.Add(end.Sub(now) / 2)
}

// awaitHold reads the holder's one byte from pr until deadline: nil only for "1".
func awaitHold(pr *os.File, deadline time.Time) error {
	if err := pr.SetReadDeadline(deadline); err != nil {
		return err
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(pr, one); err != nil {
		return err
	}
	if one[0] != '1' {
		return fmt.Errorf("the holder answered %q, want \"1\"", one)
	}
	return nil
}

// holdProc runs this test binary from the file exe with argv as its whole argv,
// holding users (TestMain), and returns once the hold has landed.
//
// The wait carries no literal: a start the scheduler starved and a lock delayed
// behind a stalled disk are the machine's, never evidence that the hold failed
// (#935), and a start sample predicts nothing about the delay after it. The
// only bound is one the helper does not invent — the test binary's own
// -timeout, half of what it has left (holdWaitDeadline) — so a holder that
// gets the whole budget still lands, while one that never answers ends the
// test with a verdict before the suite's own panic (#998, #874's rule: bounds
// from conditions, not literals).
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
	end, hasEnd := t.Deadline()
	if err := awaitHold(pr, holdWaitDeadline(time.Now(), end, hasEnd)); err != nil {
		t.Fatalf("the hold never landed: %v", err)
	}
	return cmd
}

// TestHoldWaitDeadline pins the wait's only bound: half of what the test
// binary's own -timeout has left, Go's default when the run set none, and a
// verdict at once for an end already passed (#998).
func TestHoldWaitDeadline(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		end     time.Time
		hasEnd  bool
		want    time.Time
		notLate bool // an end already passed must never push the wait past now
	}{
		{"half of what the run has left", now.Add(600 * time.Second), true, now.Add(300 * time.Second), false},
		{"Go's own default when the run set none", time.Time{}, false, now.Add(5 * time.Minute), false},
		{"an end already passed ends the wait at once", now.Add(-2 * time.Second), true, now.Add(-time.Second), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := holdWaitDeadline(now, c.end, c.hasEnd)
			if !got.Equal(c.want) {
				t.Fatalf("deadline %v, want %v", got, c.want)
			}
			if c.notLate && got.After(now) {
				t.Fatalf("deadline %v is after now; a passed end must end the wait at once", got)
			}
		})
	}
}

// TestAwaitHold pins the read the holder's answer comes through: a deadline
// that ends a silent holder with ErrDeadlineExceeded (never an unbounded
// wait), a late "1" that is still accepted, EOF when the writer is gone, and
// a wrong byte that is not an answer (#998).
func TestAwaitHold(t *testing.T) {
	t.Run("a silent holder is ended by the deadline", func(t *testing.T) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); pw.Close() })
		if err := awaitHold(pr, time.Now().Add(100*time.Millisecond)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("got %v, want the deadline to end a holder that never answers", err)
		}
	})
	t.Run("a late answer is not a failure", func(t *testing.T) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); pw.Close() })
		go func() {
			time.Sleep(300 * time.Millisecond)
			pw.WriteString("1")
		}()
		if err := awaitHold(pr, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("a holder that answers late must land: %v", err)
		}
	})
	t.Run("a closed writer is EOF", func(t *testing.T) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close() })
		pw.Close()
		if err := awaitHold(pr, time.Now().Add(time.Hour)); !errors.Is(err, io.EOF) {
			t.Fatalf("got %v, want EOF from a writer that closed without an answer", err)
		}
	})
	t.Run("a wrong byte is not an answer", func(t *testing.T) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pr.Close(); pw.Close() })
		pw.WriteString("0")
		if err := awaitHold(pr, time.Now().Add(time.Hour)); err == nil {
			t.Fatal("a holder that answered \"0\" must not read as a landed hold")
		}
	})
}

// TestHoldProcWaitsOutAHolderThatLocksLate is the reviewer's pause-before-flock
// case: a healthy holder delayed after it started must still land. The users
// file is held exclusively for a second, so the child's shared lock cannot be
// taken before the release; the assertion is causal — holdProc returned only
// once the release had happened — never a wall-clock bound (#998).
func TestHoldProcWaitsOutAHolderThatLocksLate(t *testing.T) {
	users := BridgeFiles(t.TempDir(), "p")[2]
	f, err := lockFile(users, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(time.Second)
		close(released)
		f.Close()
	}()
	holdProc(t, os.Args[0], users, "argv0")
	select {
	case <-released:
	default:
		t.Fatal("the hold landed while the exclusive lock was held")
	}
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

// fdDir lists this process's open descriptor numbers on Linux (a link to
// /proc/self/fd) and on macOS (fdesc) alike. Only the numbers are read from
// it: macOS's fdesc gives a stat of /dev/fd/N its own device number, so the
// file behind each descriptor is read with fstat on the descriptor itself,
// which names the file's real device and inode on both systems.
const fdDir = "/dev/fd"

// fdsNaming is how many of this process's descriptors are open on the file
// whose fstat is want, matched by device and inode: it is how the test below
// sees Release open the mutex without a hook in Release itself.
func fdsNaming(t *testing.T, want syscall.Stat_t) int {
	t.Helper()
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil && st.Dev == want.Dev && st.Ino == want.Ino {
			n++
		}
	}
	return n
}

// GH-837 (merge gate, c513c37): rm takes a profile's bridge files while a
// session of it still runs. That session's Release opened the old mutex before
// the unlink; a fresh launch then made a new mutex and is starting its bridge.
// The old Release must finish on the generation it owns, the old mutex and its
// own users lock, and never queue on the fresh bridge's mutex and then remove
// the fresh record: a bridge left running with no record lets the next launch
// start a second one on the same auth directory (#250).
func TestAReleaseQueuedOnARemovedMutexLeavesTheNextBridgesRecord(t *testing.T) {
	root := t.TempDir()
	files := BridgeFiles(root, "p")
	old, ok, err := AcquireBridge(root, "p", func() (string, string, string, string, bool) {
		return "0", "4100", "stub", filepath.Join(root, "old"), true
	})
	if err != nil || !ok {
		t.Fatalf("the old bridge must start: ok=%v err=%v", ok, err)
	}
	mu, err := os.OpenFile(files[1], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer mu.Close()
	var muStat syscall.Stat_t
	if err := syscall.Fstat(int(mu.Fd()), &muStat); err != nil {
		t.Fatal(err)
	}
	// AcquireBridge closed its own descriptor on the mutex, so the count sees
	// only this test's one now. Any other count means it could not see
	// Release open the mutex either: that is a broken test, never a skip, so
	// the interleaving below runs on Linux and macOS alike.
	if n := fdsNaming(t, muStat); n != 1 {
		t.Fatalf("%s must show this test's own descriptor on the mutex and no other before Release; it shows %d", fdDir, n)
	}
	if err := syscall.Flock(int(mu.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { old.Release(filepath.Join(root, "p")); close(released) }()
	for until := time.Now().Add(5 * time.Second); fdsNaming(t, muStat) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(until) {
			t.Fatal("the old Release never opened the mutex")
		}
	}
	for _, p := range files { // rm takes the bridge files with the profile
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	type acquired struct {
		ok  bool
		err error
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	fresh := make(chan acquired, 1)
	go func() {
		_, ok, err := AcquireBridge(root, "p", func() (string, string, string, string, bool) {
			close(entered)
			<-resume
			return "0", "4200", "stub", filepath.Join(root, "new"), true
		})
		fresh <- acquired{ok, err}
	}()
	select {
	case <-entered:
	case got := <-fresh:
		t.Fatalf("the fresh acquire ended before its start: ok=%v err=%v", got.ok, got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("the fresh acquire never reached its start")
	}
	if err := syscall.Flock(int(mu.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	releasedFirst := false
	select {
	case <-released:
		releasedFirst = true
	case <-time.After(5 * time.Second):
	}
	close(resume)
	select {
	case got := <-fresh:
		if got.err != nil || !got.ok {
			t.Fatalf("the fresh bridge must start: ok=%v err=%v", got.ok, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fresh acquire never finished")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the old Release never finished")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || !strings.Contains(string(raw), " 4200 ") {
		t.Fatalf("the old Release removed the fresh bridge's record: %q (%v)", raw, err)
	}
	if !releasedFirst {
		t.Fatal("the old Release queued on the fresh bridge's mutex instead of finishing on the one it opened")
	}
}

// GH-837 (merge gate, c9b9a63): the same rm, but the fresh launch finishes and
// records its bridge before the old Release gets the mutex it opened. The old
// Release is still the last user of its own bridge, so it stops that one and
// removes its config; the record at the path is now the fresh bridge's, and it
// stays: a bridge left running with no record lets the next launch start a
// second one on the same auth directory (#250).
func TestAReleaseAfterAFreshBridgeRecordedKeepsThatRecord(t *testing.T) {
	root := t.TempDir()
	files := BridgeFiles(root, "p")
	oldTmp, newTmp := filepath.Join(root, "old"), filepath.Join(root, "new")
	old, ok, err := AcquireBridge(root, "p", func() (string, string, string, string, bool) {
		if err := os.MkdirAll(oldTmp, 0o700); err != nil {
			t.Error(err)
		}
		return "0", "4100", "stub", oldTmp, true
	})
	if err != nil || !ok {
		t.Fatalf("the old bridge must start: ok=%v err=%v", ok, err)
	}
	mu, err := os.OpenFile(files[1], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer mu.Close()
	var muStat syscall.Stat_t
	if err := syscall.Fstat(int(mu.Fd()), &muStat); err != nil {
		t.Fatal(err)
	}
	if n := fdsNaming(t, muStat); n != 1 {
		t.Fatalf("%s must show this test's own descriptor on the mutex and no other before Release; it shows %d", fdDir, n)
	}
	if err := syscall.Flock(int(mu.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { old.Release(filepath.Join(root, "p")); close(released) }()
	for until := time.Now().Add(5 * time.Second); fdsNaming(t, muStat) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(until) {
			t.Fatal("the old Release never opened the mutex")
		}
	}
	for _, p := range files { // rm takes the bridge files with the profile
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	fresh, ok, err := AcquireBridge(root, "p", func() (string, string, string, string, bool) {
		if err := os.MkdirAll(newTmp, 0o700); err != nil {
			t.Error(err)
		}
		return "0", "4200", "stub", newTmp, true
	})
	if err != nil || !ok {
		t.Fatalf("the fresh bridge must start and record itself: ok=%v err=%v", ok, err)
	}
	select {
	case <-released:
		t.Fatal("the old Release finished before the test unlocked the mutex it opened")
	default:
	}
	if err := syscall.Flock(int(mu.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the old Release never finished")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || !strings.Contains(string(raw), " 4200 ") {
		t.Fatalf("the old Release removed the fresh bridge's record: %q (%v)", raw, err)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Fatalf("the old Release must still stop its own bridge and remove its config: %v", err)
	}
	if _, err := os.Stat(newTmp); err != nil {
		t.Fatalf("the old Release must leave the fresh bridge's config: %v", err)
	}
	fresh.Release(filepath.Join(root, "p"))
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatalf("the fresh bridge's own last Release must still remove its record: %v", err)
	}
}

// GH-837: the last session out removes the bridge record only while it names
// that session's bridge, by its TMPDIR, the one field no other bridge shares.
// Its own record goes, with or without the newline a hand-written one ends
// with; another bridge's, a half-written or an unreadable one stays.
func TestTheLastReleaseRemovesOnlyTheRecordOfItsOwnBridge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record func(own string) string
		gone   bool
	}{
		{"its own record", func(own string) string { return "0 4100 stub " + own }, true},
		{"its own record with a newline", func(own string) string { return "0 4100 stub " + own + "\n" }, true},
		{"another bridge's record", func(own string) string {
			return "0 4300 stub " + filepath.Join(filepath.Dir(own), "ccmorph-bridge-other")
		}, false},
		{"a record naming a longer path", func(own string) string { return "0 4100 stub " + own + "-2" }, false},
		{"a half-written record", func(string) string { return "" }, false},
		{"an unreadable record", func(string) string { return "garbage" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := BridgeFiles(root, "p")
			own := filepath.Join(root, "Application Support", "ccmorph-bridge-own")
			a, ok, err := AcquireBridge(root, "p", func() (string, string, string, string, bool) {
				return "0", "4100", "stub", own, true
			})
			if err != nil || !ok {
				t.Fatalf("the bridge must start: ok=%v err=%v", ok, err)
			}
			want := tc.record(own)
			if err := os.WriteFile(files[0], []byte(want), 0o600); err != nil {
				t.Fatal(err)
			}
			a.Release(filepath.Join(root, "p"))
			raw, err := os.ReadFile(files[0])
			if tc.gone {
				if !os.IsNotExist(err) {
					t.Fatalf("the last Release must remove its own bridge's record: %q (%v)", raw, err)
				}
				return
			}
			if err != nil || string(raw) != want {
				t.Fatalf("the last Release must keep a record that does not name its bridge: %q (%v), want %q", raw, err, want)
			}
		})
	}
}
