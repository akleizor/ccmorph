package util

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// gc-stale reaps what a killed launch left behind: the relay of a launcher
// that is gone, and a session env file a TERM orphaned in the moment between
// writing it and starting claude (the signal path exits past the backstop
// defer), from TMPDIR and the state run dir alike (#142). What is under a
// minute old, or owned by a live launcher, stays.
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
	GCStale(state, filepath.Join(root, "profiles"), tmp)
	for _, f := range []string{dead, staleTmp, staleRun} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s must go: a killed launch left it behind", f)
		}
	}
	for _, f := range []string{mine, young, fresh} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s must stay (live owner or under a minute old)", f)
		}
	}
}
