package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A run/agent-proxy.<pid> dir (cli/agentproxy.go) a kill -9 left behind goes
// once its launcher is gone and it is a minute old; a live launcher's and a
// fresh one's stay.
func TestGCReapsAnOrphanedAgentProxyRunDir(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "tmp")
	os.MkdirAll(tmp, 0o700)
	mk := func(pid int, old bool) string {
		d := filepath.Join(state, "run", "agent-proxy."+strconv.Itoa(pid))
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "ca-bundle.pem"), []byte("x"), 0o600)
		if old {
			past := time.Now().Add(-2 * time.Minute)
			os.Chtimes(d, past, past)
		}
		return d
	}
	orphan, fresh, live := mk(dead.Process.Pid, true), mk(dead.Process.Pid+100000, false), mk(os.Getpid(), true)
	GCStale(state, filepath.Join(root, "profiles"), tmp)
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("the dead launcher's run dir must go")
	}
	for _, d := range []string{fresh, live} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s must stay", d)
		}
	}
}
