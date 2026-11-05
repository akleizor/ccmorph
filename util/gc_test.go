package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// gc-stale reaps what a killed launch left behind: the relay of a launcher
// that is gone, a session env file a TERM orphaned in the moment between
// writing it and starting claude (the signal path exits past the backstop
// defer), and a private view whose claude died without its helper's defer —
// a SIGKILL skips that defer, and the view holds the session's real
// transcripts, from TMPDIR and the state run dir alike (#142, #166). What is
// under a minute old, or owned by a live process, stays: a live private
// session's view lives as long as its claude runs, however old it is — and
// the owner pid must still be that claude, not any number that answers kill:
// the OS recycles a dead pid's number to a later process, which answers
// kill(pid, 0) just the same (#189).
func TestGCReapsWhatAKilledLaunchLeaves(t *testing.T) {
	// The recycled pid is a live same-user process that now holds the dead
	// claude's number: a plain sleep stands in, its command naming no claude.
	// The living claude is the launch's own spawn shape — privateSession runs
	// claude with argv[0] "claude" — on another sleep, so the two stand-ins
	// differ in nothing but the name the process table reads (#189).
	recycled := exec.Command("/bin/sleep", "30")
	if recycled.Start() != nil {
		t.Fatal("could not start the recycled-pid stand-in")
	}
	defer func() { recycled.Process.Kill(); recycled.Wait() }()
	claude := exec.Command("/bin/sleep", "30")
	claude.Args[0] = "claude"
	if claude.Start() != nil {
		t.Fatal("could not start the claude stand-in")
	}
	defer func() { claude.Process.Kill(); claude.Wait() }()

	root := t.TempDir()
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "tmp")
	os.MkdirAll(filepath.Join(state, "run"), 0o700)
	os.MkdirAll(tmp, 0o700)
	past := time.Now().Add(-2 * time.Minute)
	dead := filepath.Join(state, ".reswitch.999999.1.2")
	mine := filepath.Join(state, ".reswitch."+strconv.Itoa(os.Getpid())+".1.2")
	young := filepath.Join(state, ".reswitch.999998.1.2")
	staleTmp := filepath.Join(tmp, "ccmorph-session-env.999999")
	staleRun := filepath.Join(state, "run", "ccmorph-session-env.999998")
	fresh := filepath.Join(tmp, "ccmorph-session-env.42")
	for _, f := range []string{dead, mine, young, staleTmp, staleRun, fresh} {
		os.WriteFile(f, nil, 0o600)
	}
	for _, f := range []string{dead, mine, staleTmp, staleRun} {
		os.Chtimes(f, past, past)
	}
	// A private view is a dir whose owner record names the claude that ran
	// from it. A dead owner's view goes once a minute old, from either root,
	// and so does one whose owner pid another process now holds — kill alone
	// cannot tell a recycled number from the claude it replaced; a live
	// claude's stays however old the view is; one with no owner record — a
	// session from a build older than the record, or one whose record failed
	// to write — is never reaped on age alone: its transcripts age while
	// claude idles, so age cannot tell it from a dead session's.
	view := func(dir, name, owner string) string {
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0o700)
		if owner != "" {
			os.WriteFile(filepath.Join(d, privateOwner), []byte(owner), 0o600)
		}
		return d
	}
	deadViewTmp := view(tmp, "ccmorph-private.1", "999997")
	deadViewRun := view(filepath.Join(state, "run"), "ccmorph-private.2", "999996")
	recycledView := view(tmp, "ccmorph-private.3", strconv.Itoa(recycled.Process.Pid))
	claudeView := view(tmp, "ccmorph-private.4", strconv.Itoa(claude.Process.Pid))
	youngView := view(filepath.Join(state, "run"), "ccmorph-private.5", "999995")
	noOwner := view(filepath.Join(state, "run"), "ccmorph-private.6", "")
	for _, d := range []string{deadViewTmp, deadViewRun, recycledView} {
		os.Chtimes(d, past, past)
	}
	ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(claudeView, ancient, ancient)
	os.Chtimes(noOwner, ancient, ancient)
	GCStale(state, filepath.Join(root, "profiles"), tmp)
	for _, f := range []string{dead, staleTmp, staleRun} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s must go: a killed launch left it behind", f)
		}
	}
	for _, d := range []string{deadViewTmp, deadViewRun, recycledView} {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s must go: the claude that ran from it is gone, its pid dead or held by another process", d)
		}
	}
	for _, f := range []string{mine, young, fresh, claudeView, youngView, noOwner} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s must stay (live or unknown owner, or under a minute old)", f)
		}
	}
}
