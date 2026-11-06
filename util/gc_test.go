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
// the process holding the owner pid must still be that claude, not any number
// that answers kill: the OS recycles a dead pid's number to a later process,
// which answers kill(pid, 0) just the same (#189), and a word in its command
// line cannot tell the two apart, since a grep, an editor or another Claude
// Code session all name claude (#206). Only when it started can.
func TestGCReapsWhatAKilledLaunchLeaves(t *testing.T) {
	// The recycled pid is a live same-user process that now holds the dead
	// claude's number, and its command line names claude: the #189 check
	// spared every such process, so the stand-in is the one that fooled it —
	// a grep over notes, with sleep reading only its count from argv (#206).
	recycled := exec.Command("/bin/sleep", "30")
	recycled.Args = []string{"grep -r claude /Users/someone/notes", "30"}
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
	// A living claude the sweep must not know by its command line: the launch
	// resolves claude by a path and argv may spell it any way, so this one
	// names no claude at all. Only the record, written the moment it started,
	// says it is the claude that runs from its view (#206).
	script := exec.Command("/bin/sleep", "30")
	script.Args = []string{"node /opt/tools/cc/cli.js", "30"}
	if script.Start() != nil {
		t.Fatal("could not start the script-claude stand-in")
	}
	defer func() { script.Process.Kill(); script.Wait() }()

	root := t.TempDir()
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "tmp")
	os.MkdirAll(filepath.Join(state, "run"), 0o700)
	os.MkdirAll(tmp, 0o700)
	past := time.Now().Add(-2 * time.Minute)
	dead := filepath.Join(state, ".reswitch.999999.1.2")
	// This process's own relay names a launch that just happened: a launcher
	// runs before the launch it hands off, so its start is older than the
	// name's clock, and its queued switch stays.
	mine := filepath.Join(state, ".reswitch."+strconv.Itoa(os.Getpid())+"."+strconv.FormatInt(time.Now().Unix(), 10)+"_000000.2")
	// A relay under a recycled launcher pid: the name says the launch happened
	// at the epoch and the sleep holding the number started since, so the pid
	// answers kill while it is not the launcher (#206).
	recycledRelay := filepath.Join(state, ".reswitch."+strconv.Itoa(recycled.Process.Pid)+".1_000000.2")
	young := filepath.Join(state, ".reswitch.999998.1.2")
	staleTmp := filepath.Join(tmp, "ccmorph-session-env.999999")
	staleRun := filepath.Join(state, "run", "ccmorph-session-env.999998")
	fresh := filepath.Join(tmp, "ccmorph-session-env.42")
	for _, f := range []string{dead, mine, recycledRelay, young, staleTmp, staleRun, fresh} {
		os.WriteFile(f, nil, 0o600)
	}
	for _, f := range []string{dead, mine, recycledRelay, staleTmp, staleRun} {
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
	ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	deadViewTmp := view(tmp, "ccmorph-private.1", "999997")
	deadViewRun := view(filepath.Join(state, "run"), "ccmorph-private.2", "999996")
	recycledView := view(tmp, "ccmorph-private.3", strconv.Itoa(recycled.Process.Pid))
	claudeView := view(tmp, "ccmorph-private.4", strconv.Itoa(claude.Process.Pid))
	youngView := view(filepath.Join(state, "run"), "ccmorph-private.5", "999995")
	noOwner := view(filepath.Join(state, "run"), "ccmorph-private.6", "")
	scriptView := view(tmp, "ccmorph-private.7", strconv.Itoa(script.Process.Pid))
	for _, d := range []string{deadViewTmp, deadViewRun, recycledView} {
		os.Chtimes(d, past, past)
	}
	// The recycled view's record predates the process that now holds the
	// number, as every record does for a pid the OS recycled: it was written
	// the moment the dead claude started.
	os.Chtimes(filepath.Join(recycledView, privateOwner), ancient, ancient)
	os.Chtimes(claudeView, ancient, ancient)
	os.Chtimes(scriptView, ancient, ancient)
	os.Chtimes(noOwner, ancient, ancient)
	GCStale(state, filepath.Join(root, "profiles"), tmp)
	for _, f := range []string{dead, recycledRelay, staleTmp, staleRun} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s must go: a killed launch left it behind", f)
		}
	}
	for _, d := range []string{deadViewTmp, deadViewRun, recycledView} {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s must go: the claude that ran from it is gone, its pid dead or held by another process", d)
		}
	}
	for _, f := range []string{mine, young, fresh, claudeView, scriptView, youngView, noOwner} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s must stay (live or unknown owner, or under a minute old)", f)
		}
	}
}
