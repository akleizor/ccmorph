//go:build darwin

package util

import (
	"os"
	"os/exec"
	"testing"
)

func TestSysctlProcessTableSeesAChild(t *testing.T) {
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
}
