package util

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestHopTargetIsTheFirstNonShellBelowTheRelay(t *testing.T) {
	// relay 100 -> zsh 101 (forwarder) -> claude 102 -> zsh 103 (command shell) -> zsh 104 ($$)
	tab := map[int]psRow{
		101: {ppid: 100, comm: "/bin/zsh"},
		102: {ppid: 101, comm: "/opt/bin/claude"},
		103: {ppid: 102, comm: "-zsh"},
		104: {ppid: 103, comm: "zsh"},
	}
	if got := findHopTarget(tab, 100, 104); got != 102 {
		t.Fatalf("got %d, want 102", got)
	}
	if got := findHopTarget(tab, 999, 104); got != 0 {
		t.Fatalf("an unrelated relay must find nothing, got %d", got)
	}
	tab[101] = psRow{ppid: 100, comm: "node"}
	if got := findHopTarget(tab, 100, 104); got != 101 {
		t.Fatalf("nearest the relay wins, got %d", got)
	}
}

func TestHopTargetSkipsTheCoreLauncher(t *testing.T) {
	// The Go launcher runs claude as its child, so the process between the relay
	// and Claude Code is the core itself: relay 200 -> core 201 -> claude 202 ->
	// zsh 203 (command shell) -> zsh 204 ($$). Ending the core forwards TERM to
	// claude as a signal the launch sent, which discards the queued switch.
	tab := map[int]psRow{
		201: {ppid: 200, comm: "/Users/u/.config/ccmorph/libexec/ccmorph-core"},
		202: {ppid: 201, comm: "/opt/bin/claude"},
		203: {ppid: 202, comm: "zsh"},
		204: {ppid: 203, comm: "-zsh"},
	}
	if got := findHopTarget(tab, 200, 204); got != 202 {
		t.Fatalf("the hop must end claude (202), not the core launcher, got %d", got)
	}
	tab[202] = psRow{ppid: 201, comm: "claude"}
	if got := findHopTarget(tab, 200, 204); got != 202 {
		t.Fatalf("a bare comm name must be found too, got %d", got)
	}
	// A core with nothing but shells below it has no Claude Code to end.
	tab[202] = psRow{ppid: 201, comm: "sh"}
	if got := findHopTarget(tab, 200, 204); got != 0 {
		t.Fatalf("shells below the core are not a hop target, got %d", got)
	}
}

func TestSessionAdapterFindsAReparentedAdapter(t *testing.T) {
	// util adapter-start exits once the adapter is ready, so the running adapter
	// is reparented to launchd: its --parent-pid argument is the only witness left.
	tab := map[int]psRow{
		50: {ppid: 1, args: "/x/libexec/ccmorph-core adapter gw --auto --model m --parent-pid 100"},
		51: {ppid: 300, args: "/x/libexec/ccmorph-core adapter sub --auto"},
		52: {ppid: 1, args: "/x/libexec/ccmorph-core adapter eq --auto --parent-pid=200"},
		53: {ppid: 1, args: "/x/libexec/ccmorph-core adapter near --auto --parent-pid 1000"},
		54: {ppid: 1, args: "/bin/other --parent-pid 100"},
	}
	for _, c := range []struct {
		launcher int
		want     string
	}{
		{100, "gw"},
		{200, "eq"},
		{300, "sub"},
		{10, ""},
		{400, ""},
	} {
		if got := findSessionAdapter(tab, c.launcher); got != c.want {
			t.Fatalf("launcher %d: got %q, want %q", c.launcher, got, c.want)
		}
	}
}

func TestSessionAdapterFindsAReparentedProcessInTheRealTable(t *testing.T) {
	// adapter-start exits at readiness, so a running adapter's ppid is not the
	// launcher's: only its --parent-pid argument can name it. A real process with
	// that argv, read back through the real process table, whose parent is this
	// test and not the launcher it names.
	const launcher = 424242
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Args = []string{"/x/libexec/ccmorph-core adapter gw --auto --parent-pid 424242", "30"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	tab := processTable()
	r, ok := tab[cmd.Process.Pid]
	if !ok || r.ppid == launcher {
		t.Fatalf("the process must be in the table under a parent that is not the launcher, got %+v", r)
	}
	if got := findSessionAdapter(tab, launcher); got != "gw" {
		t.Fatalf("a reparented adapter must be found by its --parent-pid, got %q (row %+v)", got, r)
	}
}

func TestAdapterProfileReadsTheArgvItIsGiven(t *testing.T) {
	// The shapes an adapter's argv takes: the core's own `ccmorph-core adapter
	// <profile>`, the older build's `ccmorph-adapter <profile>`, either behind
	// the python host a wrapped install runs it through — and the lookalikes a
	// joined command line cannot tell apart.
	for _, c := range []struct {
		argv    []string
		profile string
		found   bool
	}{
		{[]string{"/usr/local/libexec/ccmorph-core", "adapter", "gw"}, "gw", true},
		{[]string{"ccmorph-adapter", "gw"}, "gw", true},
		{[]string{"python3", "/x/libexec/ccmorph-core", "adapter", "gw", "--auto"}, "gw", true},
		{[]string{"/x/my adapter/ccmorph-core", "adapter", "gw"}, "gw", true},
		{[]string{"/bin/echo", "ccmorph-core", "adapter", "gw"}, "", false},
		{[]string{"/x/libexec/ccmorph-core", "adapterx", "gw"}, "", false},
		{[]string{"/x/libexec/ccmorph-core", "adapter"}, "", false},
		{[]string{"python3"}, "", false},
	} {
		if name, ok := adapterProfile(c.argv); ok != c.found || name != c.profile {
			t.Fatalf("argv %v: got (%q, %v), want (%q, %v)", c.argv, name, ok, c.profile, c.found)
		}
	}
}

func TestFindAdapterMatchesTheKernelArgvNotTheJoinedLine(t *testing.T) {
	// A process table's command lines are joined text, so a space in the exec
	// path would blur where the path ends and the arguments begin — and the
	// decoy's joined line names an adapter it is not. findAdapter matches the
	// kernel's argv, the boundaries a launch itself trusts, read back through
	// the real table.
	profile := "fw" + strconv.Itoa(os.Getpid())
	holdProc(t, os.Args[0], BridgeFiles(t.TempDir(), "p")[2],
		"/bin/echo", "ccmorph-core", "adapter", profile)
	adapter := holdProc(t, os.Args[0], BridgeFiles(t.TempDir(), "p")[2],
		"/x/my adapter/ccmorph-core", "adapter", profile)

	tab := processTable()
	if got := findAdapter(tab, profile); got != adapter.Process.Pid {
		t.Fatalf("the adapter must be found by its argv, got %d want %d (row %+v)",
			got, adapter.Process.Pid, tab[got])
	}
	if got := findAdapter(tab, "no-such-profile"); got != 0 {
		t.Fatalf("a profile nothing serves must find nothing, got %d", got)
	}
}

func TestPsProcessTableReadsStartTimes(t *testing.T) {
	// The ps path is the whole table off darwin, and on darwin the fallback, so
	// its start times are what the stale sweep reads there. lstart carries whole
	// seconds, and on Linux it is boot time plus clock ticks, which can read a
	// second early: the floor is two seconds under the test's own clock, far
	// inside the minute the sweep allows.
	if err := exec.Command("ps", "-o", "pid=", "-p", "1").Run(); err != nil {
		t.Skipf("ps cannot run here: %v", err)
	}
	before := time.Now().Add(-2 * time.Second)
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	tab := psProcessTable(context.Background())
	r, ok := tab[cmd.Process.Pid]
	if !ok || r.ppid != os.Getpid() || r.args != "/bin/sleep 30" {
		t.Fatalf("the child must be read with its parent and argv, got %+v", r)
	}
	if r.start.Before(before) || r.start.After(time.Now().Add(time.Second)) {
		t.Fatalf("the child must be read with the time it started, got %v, no earlier than %v", r.start, before)
	}
}
