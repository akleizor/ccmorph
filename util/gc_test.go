package util

import (
	"os"
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
// session's view lives as long as its claude runs, however old it is.
func TestGCReapsWhatAKilledLaunchLeaves(t *testing.T) {
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
	// from it. A dead owner's view goes once a minute old, from either root; a
	// live owner's stays however old the view is; one with no owner record —
	// a session from a build older than the record, or one whose record
	// failed to write — is never reaped on age alone: its transcripts age
	// while claude idles, so age cannot tell it from a dead session's.
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
	liveView := view(tmp, "ccmorph-private.3", strconv.Itoa(os.Getpid()))
	youngView := view(filepath.Join(state, "run"), "ccmorph-private.4", "999995")
	noOwner := view(filepath.Join(state, "run"), "ccmorph-private.5", "")
	for _, d := range []string{deadViewTmp, deadViewRun, liveView} {
		os.Chtimes(d, past, past)
	}
	ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(noOwner, ancient, ancient)
	GCStale(state, filepath.Join(root, "profiles"), tmp)
	for _, f := range []string{dead, staleTmp, staleRun} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s must go: a killed launch left it behind", f)
		}
	}
	for _, d := range []string{deadViewTmp, deadViewRun} {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s must go: the claude that ran from it is gone", d)
		}
	}
	for _, f := range []string{mine, young, fresh, liveView, youngView, noOwner} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s must stay (live or unknown owner, or under a minute old)", f)
		}
	}
}
