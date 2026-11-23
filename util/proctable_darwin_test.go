//go:build darwin

package util

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestSysctlProcessTableSeesAChild(t *testing.T) {
	before := time.Now()
	cmd := exec.Command("/bin/sleep", "30")
	if cmd.Start() != nil {
		t.Fatal("could not start a child")
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	tab, ok := sysctlProcessTable()
	if !ok {
		t.Fatal("the sysctl process table must be readable on darwin")
	}
	if r, ok := tab[os.Getpid()]; !ok || r.ppid != os.Getppid() {
		t.Fatalf("this process must sit under its parent, got %+v", r)
	}
	r, ok := tab[cmd.Process.Pid]
	if !ok || r.ppid != os.Getpid() || r.comm != "sleep" || r.args != "/bin/sleep 30" {
		t.Fatalf("the child must be read with its parent, comm and argv, got %+v", r)
	}
	if r.start.Before(before.Add(-time.Second)) || r.start.After(time.Now()) {
		t.Fatalf("the child must be read with the time it started, got %v, no earlier than %v", r.start, before)
	}
	if self := tab[os.Getpid()]; self.start.IsZero() || self.start.After(before) {
		t.Fatalf("this process must be read with the time it started, got %v, no later than %v", self.start, before)
	}
	mine, ok := procStartToken(os.Getpid())
	again, _ := procStartToken(os.Getpid())
	child, cok := procStartToken(cmd.Process.Pid)
	if !ok || !cok || mine == "" || mine != again || mine == child {
		t.Fatalf("each process must read one mark of its own, got self %q/%q, child %q", mine, again, child)
	}
	if m, ok := procStartToken(999999); ok {
		t.Fatalf("a pid nobody holds must read no mark, got %q", m)
	}
}

// The native reader's own silent shape, diagnosed rather than assumed: a
// kernel that answers kern.proc.all but refuses kern.procargs2 — a sandbox
// profile can do exactly this — passes the pid/ppid self-check and returns a
// table whose every command line is empty. That table never reaches ps, so no
// ps fix can cover it: what hid the faked adapter on a darwin machine may be
// this, and it must decline rather than confirm an absence (GH-819). The same
// refusal with only this process and the adapter readable is an ordinary
// other-users' process denial and still answers.
func TestRunningAdapterDeclinesWhenTheKernelRefusesCommandLines(t *testing.T) {
	profile := "kr" + strconv.Itoa(os.Getpid())
	adapter := holdProc(t, os.Args[0], BridgeFiles(t.TempDir(), "p")[2],
		"/x/ccmorph-core", "adapter", profile)
	want := fmt.Sprintf("%d\t%d:shim", adapter.Process.Pid, adapter.Process.Pid)
	orig := sysctlRaw
	t.Cleanup(func() { sysctlRaw = orig })
	refuse := func(readable ...int) {
		sysctlRaw = func(mib []int32, buf []byte) ([]byte, bool) {
			if len(mib) == 3 && mib[0] == 1 && mib[1] == 49 && !slices.Contains(readable, int(mib[2])) {
				return nil, false
			}
			return sysctlRead(mib, buf)
		}
	}

	refuse()
	tab, ok := sysctlProcessTable()
	if !ok {
		t.Fatal("a kernel answering kern.proc.all must pass the self-check: this route never reaches ps")
	}
	if r := tab[adapter.Process.Pid]; r.args != "" {
		t.Fatalf("every refused command line must read empty, got %q", r.args)
	}
	if got := runningAdapter(profile); got != "__UNKNOWN__" {
		t.Fatalf("argv the kernel refused is not a confirmed absence, got %q", got)
	}

	refuse(os.Getpid(), adapter.Process.Pid)
	if got := runningAdapter(profile); got != want {
		t.Fatalf("a refusal limited to other users' processes must still answer, got %q want %q", got, want)
	}
}
