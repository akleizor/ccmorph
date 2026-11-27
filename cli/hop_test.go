package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/util"
)

// hopFixture is the state a joined gateway session shows `ccmorph hop`: gw is
// the profile the session runs on, other is the profile its own
// env.CLAUDE_CONFIG_DIR points at — the override that makes the fallback answer
// wrong when the shared adapter's marker is missed (#1022).
func hopFixture(t *testing.T) (*launcher, string) {
	t.Helper()
	state, profiles := gatewayProfile(t, "gw", "type=apikey\nbase_url=https://gw.test\napi_key=k")
	if err := os.MkdirAll(filepath.Join(profiles, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "other.conf"),
		[]byte("type=apikey\nbase_url=https://other.test\napi_key=k"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "hop-session")
	t.Setenv("CCMORPH_RELAY_ID", strconv.Itoa(os.Getpid())+".0_000000.0")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(profiles, "other"))
	return &launcher{dir: state, profileDirs: profiles}, state
}

// startJoinedAdapterHold is this test binary under the name ccmorph-core
// running the core's route hold on a shared adapter's users file — the child
// AcquireAdapter spawns for a session that joined an adapter. The process is a
// child of this test, which stands in for the session's launcher.
func startJoinedAdapterHold(t *testing.T, users string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(users), 0o700); err != nil {
		t.Fatal(err)
	}
	core := coreCopy(t)
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(core, "util", "route-hold", users, "--exit-on-stdin-eof")
	cmd.Env = append(os.Environ(), "CCMORPH_TEST_HOLD_USERS="+users)
	cmd.Stdin, cmd.Stdout = inR, readyW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inR.Close()
	readyW.Close()
	go cmd.Wait()
	t.Cleanup(func() {
		inW.Close()
		cmd.Process.Kill()
	})
	end, ok := t.Deadline()
	if !ok {
		end = time.Now().Add(10 * time.Minute)
	}
	readyR.SetReadDeadline(time.Now().Add(end.Sub(time.Now()) / 2))
	one := make([]byte, 1)
	if _, err := io.ReadFull(readyR, one); err != nil || one[0] != '1' {
		t.Fatalf("the session hold never landed: %v %q", err, one)
	}
	readyR.Close()
}

// hopRun runs one hop with stdout captured, as the Claude Code prompt would.
func hopRun(t *testing.T, l *launcher, args ...string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	rc := l.hop(args)
	w.Close()
	os.Stdout = saved
	out, _ := io.ReadAll(r)
	r.Close()
	return rc, string(out)
}

// TestHopNamesAJoinedSessionsProfile is #1022: a session that joined a shared
// adapter has no adapter of its own, and its profile sets env.CLAUDE_CONFIG_DIR,
// so only the adapter's session hold can name the profile it runs on.
func TestHopNamesAJoinedSessionsProfile(t *testing.T) {
	l, state := hopFixture(t)
	users := util.AdapterBase(filepath.Join(state, "auth"), "gw", "0123456789abcdef") + ".users"
	startJoinedAdapterHold(t, users)

	// The no-profile answer marks the profile the session is on.
	rc, out := hopRun(t, l)
	if rc != 0 {
		t.Fatalf("hop with no profile: rc %d:\n%s", rc, out)
	}
	if !strings.Contains(out, "(now on gw)") {
		t.Fatalf("hop must mark the joined session's profile, got:\n%s", out)
	}
	if strings.Contains(out, "now on other") {
		t.Fatalf("the CLAUDE_CONFIG_DIR fallback must not name the profile, got:\n%s", out)
	}
}

// TestHopToAJoinedSessionsProfileChangesNothing is #257 against a joined shared
// adapter: hopping to the profile the session already runs on must answer
// `Already on` and end nothing — the guard the missing marker used to lose.
func TestHopToAJoinedSessionsProfileChangesNothing(t *testing.T) {
	l, state := hopFixture(t)
	users := util.AdapterBase(filepath.Join(state, "auth"), "gw", "0123456789abcdef") + ".users"
	startJoinedAdapterHold(t, users)

	rc, out := hopRun(t, l, "gw")
	if rc != 0 {
		t.Fatalf("hop to the current profile: rc %d:\n%s", rc, out)
	}
	if !strings.Contains(out, "Already on gw — nothing changed") {
		t.Fatalf("hop must answer that nothing changed, got:\n%s", out)
	}
	if left, _ := filepath.Glob(filepath.Join(state, ".reswitch.*")); len(left) != 0 {
		t.Fatalf("a hop that changes nothing must write no relay file: %v", left)
	}
}

// TestHopToAnotherProfileFromAJoinedSessionIsNotAlreadyOn is the other side of
// #257: the marker must not over-block. A hop to another profile proceeds to
// its normal path — here there is no Claude Code process under the relay to end,
// so it answers with that error and rc 1, never "Already on" (#1022).
func TestHopToAnotherProfileFromAJoinedSessionIsNotAlreadyOn(t *testing.T) {
	l, state := hopFixture(t)
	users := util.AdapterBase(filepath.Join(state, "auth"), "gw", "0123456789abcdef") + ".users"
	startJoinedAdapterHold(t, users)

	rc, out := hopRun(t, l, "other")
	if strings.Contains(out, "Already on") {
		t.Fatalf("a hop to another profile must not answer Already on, got:\n%s", out)
	}
	if rc != 1 {
		t.Fatalf("hop to another profile must take its normal path (rc 1, no process to end): rc %d:\n%s", rc, out)
	}
}

// TestHopWithoutAHoldFallsBackToTheConfigDir pins the old path unchanged: a
// session with no adapter hold still takes its profile from CLAUDE_CONFIG_DIR.
func TestHopWithoutAHoldFallsBackToTheConfigDir(t *testing.T) {
	l, _ := hopFixture(t)

	rc, out := hopRun(t, l)
	if rc != 0 {
		t.Fatalf("hop with no profile: rc %d:\n%s", rc, out)
	}
	if !strings.Contains(out, "(now on other)") {
		t.Fatalf("a session with no hold must fall back to CLAUDE_CONFIG_DIR, got:\n%s", out)
	}
}
