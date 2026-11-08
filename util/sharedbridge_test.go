package util

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Two sessions of one provider profile share one bridge; the last one out stops it (#250).
func TestTwoSessionsShareOneBridge(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 needed for the fake bridge")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	bin := filepath.Join(dir, "cliproxyapi")
	os.WriteFile(bin, []byte(fakeBridge), 0o755)
	auth := filepath.Join(dir, "auth", "p")
	os.MkdirAll(auth, 0o700)
	starts := 0
	start := func() (pid, port, key, tmp string, ok bool) {
		starts++
		var out, errw bytes.Buffer
		if bridgeStart([]string{"p", bin, auth, filepath.Join(dir, "log")}, nil, &out, &errw) != 0 {
			log, _ := os.ReadFile(filepath.Join(dir, "log"))
			t.Logf("bridge start failed: %s; bridge log: %s", errw.String(), log)
			return "", "", "", "", false
		}
		got := map[string]string{}
		for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			k, v, _ := strings.Cut(l, "=")
			got[k] = v
		}
		return got["PID"], got["PORT"], got["KEY"], got["TMPDIR"], true
	}
	root := filepath.Dir(auth)
	a, ok1 := AcquireBridge(root, "p", start)
	b, ok2 := AcquireBridge(root, "p", start)
	if !ok1 || !ok2 || starts != 1 || a.Port != b.Port {
		t.Fatalf("want one bridge for two sessions: ok=%v,%v starts=%d", ok1, ok2, starts)
	}
	pid, _ := strconv.Atoi(a.PID)
	t.Cleanup(func() { BridgeStop(pid, a.TmpDir, "") })
	a.Release(auth)
	if syscall.Kill(pid, 0) != nil || !listsModel(b.Port, b.Key) {
		t.Fatal("the bridge must keep serving while a session still uses it")
	}
	b.Release(auth)
	if syscall.Kill(pid, 0) == nil {
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
