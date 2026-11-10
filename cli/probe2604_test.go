package cli

// TEMPORARY diagnostic for the ubuntu-26.04 route-fixture failures, printed
// from inside the runner; this file is deleted before anything lands. It runs
// the stub's old sleeper start — a symlinked sleep exec'd in the background by
// /bin/sh — beside a Go-started control sleeper, and prints what ours() would
// read for each: aliveness, /proc cmdline, and the ps row.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestProbe2604HowTheOldStubSleeperReads(t *testing.T) {
	p := func(format string, a ...any) { fmt.Printf("PROBE2604 "+format+"\n", a...) }
	p("os=%s argv0=%s", runtime.GOOS, os.Args[0])
	for _, k := range []string{"PATH", "TMPDIR", "COLUMNS", "LINES", "TERM", "LC_ALL", "LANG"} {
		p("env %s=%q", k, os.Getenv(k))
	}
	if b, err := exec.Command("ps", "--version").CombinedOutput(); err == nil {
		p("ps --version: %s", strings.TrimSpace(string(b)))
	}
	if s, err := os.Readlink("/bin/sh"); err == nil {
		p("/bin/sh -> %s", s)
	}
	// The old stub's inner lines, verbatim.
	bridges := filepath.Clean(t.TempDir())
	script := "d=\"$(mktemp -d '" + bridges + "/gh300bridge.XXXXXX')\"\n" +
		"ln -s \"$(command -v sleep)\" \"$d/config.yaml\"\n" +
		"\"$d/config.yaml\" 60 >/dev/null 2>&1 &\n" +
		"p=$!\n" +
		"sleep 0.3\n" +
		"kill -0 \"$p\" 2>/dev/null && a=yes || a=no\n" +
		"printf 'PID=%s\\nTMPDIR=%s\\nALIVE=%s\\nSLEEPAT=%s\\n' \"$p\" \"$d\" \"$a\" \"$(command -v sleep)\"\n"
	b, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	p("old stub run: err=%v out=%q", err, b)
	pid, tmp := 0, ""
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PID="); ok {
			pid, _ = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(l, "TMPDIR="); ok {
			tmp = v
		}
	}
	facts := func(tag string, pid int, tmp string) {
		if err := syscall.Kill(pid, 0); err != nil {
			p("%s pid=%d kill0=%v", tag, pid, err)
		} else {
			p("%s pid=%d kill0=alive", tag, pid)
		}
		if cb, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
			p("%s /proc/cmdline=%q", tag, cb)
		} else {
			p("%s /proc/cmdline err=%v", tag, err)
		}
		if rows, err := exec.Command("ps", "-axo", "pid=,command=").Output(); err == nil {
			for _, r := range strings.Split(string(rows), "\n") {
				if fs := strings.Fields(r); len(fs) > 0 && fs[0] == strconv.Itoa(pid) {
					p("%s ps row=%q", tag, r)
					p("%s ours-verdict=%v", tag, strings.Contains(r, filepath.Join(tmp, "config.yaml")))
				}
			}
		} else {
			p("%s ps err=%v", tag, err)
		}
	}
	facts("old-stub", pid, tmp)
	// Control: a Go-started sleeper, the way util's bridgeProc starts one.
	cdir := t.TempDir()
	cmd := exec.Command("sleep", "60")
	cmd.Args[0] = filepath.Join(cdir, "config.yaml")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { cmd.Process.Kill() })
	facts("go-started", cmd.Process.Pid, cdir)
	t.Errorf("PROBE2604 deliberate failure so the printed facts surface; temporary diagnostic, deleted before landing")
}
