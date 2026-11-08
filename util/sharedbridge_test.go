package util

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// Two sessions of one provider profile share one bridge; the last one out stops it (#250).
// The bridge is a sleep process plus an in-process listing: bridgeStart has its own test.
func TestTwoSessionsShareOneBridge(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	starts := 0
	var procs []*exec.Cmd
	start := func() (pid, port, key, tmp string, ok bool) {
		starts++
		cmd := exec.Command("sleep", "60")
		if cmd.Start() != nil {
			return "", "", "", "", false
		}
		go cmd.Wait()
		procs = append(procs, cmd)
		tmp = filepath.Join(root, "ccmorph-bridge-"+strconv.Itoa(starts))
		os.Mkdir(tmp, 0o700)
		return strconv.Itoa(cmd.Process.Pid), u.Port(), "k", tmp, true
	}
	t.Cleanup(func() {
		for _, c := range procs {
			c.Process.Kill()
		}
	})
	auth := filepath.Join(root, "p")
	a, ok1 := AcquireBridge(root, "p", start)
	b, ok2 := AcquireBridge(root, "p", start)
	if !ok1 || !ok2 || starts != 1 || a.PID != b.PID {
		t.Fatalf("want one bridge for two sessions: ok=%v,%v starts=%d", ok1, ok2, starts)
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
	c, _ := AcquireBridge(root, "p", start)
	defer c.Release(auth)
	if starts != 2 {
		t.Fatal("a launch after the last release must start a new bridge")
	}
}
