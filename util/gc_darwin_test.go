//go:build darwin

package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A sweep asks whether each private view's claude still runs, and each answer
// re-validates the kinfo layout this process reads by: a question whose answer
// cannot change while the process lives, so a sweep over several stale views
// must pay for it once, not once per view.
func TestGCSweepValidatesTheKinfoLayoutOnce(t *testing.T) {
	orig := sysctlRaw
	selfReads := 0
	sysctlRaw = func(mib []int32, buf []byte) ([]byte, bool) {
		if len(mib) == 4 && mib[0] == 1 && mib[1] == 14 && mib[2] == 1 && mib[3] == int32(os.Getpid()) {
			selfReads++
		}
		return sysctlRead(mib, buf)
	}
	t.Cleanup(func() { sysctlRaw = orig })

	// A live process whose start mark the views' records disagree with: the
	// sweep reads the kernel's mark once per view, which is what asks for the
	// layout self-check.
	holder := exec.Command("/bin/sleep", "30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { holder.Process.Kill(); holder.Wait() }()

	root := t.TempDir()
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "tmp")
	if err := os.MkdirAll(filepath.Join(state, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Minute)
	var views []string
	for _, name := range []string{"ccmorph-private.1", "ccmorph-private.2", "ccmorph-private.3"} {
		d := filepath.Join(tmp, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, privateOwner), []byte(strconv.Itoa(holder.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, privateOwnerStart), []byte("1.000000"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatal(err)
		}
		views = append(views, d)
	}

	selfReads = 0
	GCStale(state, filepath.Join(root, "profiles"), tmp)

	for _, d := range views {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s must go: its record names another process's start, so the claude that ran from it is gone", d)
		}
	}
	if selfReads > 1 {
		t.Fatalf("a sweep over %d stale views validated the kinfo layout %d times, want at most 1", len(views), selfReads)
	}
}
