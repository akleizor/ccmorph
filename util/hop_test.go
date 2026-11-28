package util

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A hop asks two questions of one machine — the profile this session runs on
// and the Claude Code process to end — moments apart, and one read of the
// process table answers both: walking every process once each is the whole
// machine paid for twice. A helper that reads its own table again shows up as
// a second walk.
func TestOneHopReadsTheProcessTableOnce(t *testing.T) {
	orig := sysctlTable
	walks := 0
	sysctlTable = func() (map[int]psRow, bool) { walks++; return orig() }
	t.Cleanup(func() { sysctlTable = orig })

	tab := ProcessTable()
	_ = SessionAdapter(tab, os.Getppid())
	_ = HopTarget(tab, os.Getppid(), os.Getpid())
	if walks != 1 {
		t.Fatalf("one hop read the process table %d times, want 1", walks)
	}
}

func TestHopTargetIsTheFirstNonShellBelowTheRelay(t *testing.T) {
	// relay 100 -> zsh 101 (forwarder) -> claude 102 -> zsh 103 (command shell) -> zsh 104 ($$)
	tab := map[int]psRow{
		101: {ppid: 100, comm: "/bin/zsh"},
		102: {ppid: 101, comm: "/opt/bin/claude"},
		103: {ppid: 102, comm: "-zsh"},
		104: {ppid: 103, comm: "zsh"},
	}
	if got := HopTarget(tab, 100, 104); got != 102 {
		t.Fatalf("got %d, want 102", got)
	}
	if got := HopTarget(tab, 999, 104); got != 0 {
		t.Fatalf("an unrelated relay must find nothing, got %d", got)
	}
	tab[101] = psRow{ppid: 100, comm: "node"}
	if got := HopTarget(tab, 100, 104); got != 101 {
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
	if got := HopTarget(tab, 200, 204); got != 202 {
		t.Fatalf("the hop must end claude (202), not the core launcher, got %d", got)
	}
	tab[202] = psRow{ppid: 201, comm: "claude"}
	if got := HopTarget(tab, 200, 204); got != 202 {
		t.Fatalf("a bare comm name must be found too, got %d", got)
	}
	// A core with nothing but shells below it has no Claude Code to end.
	tab[202] = psRow{ppid: 201, comm: "sh"}
	if got := HopTarget(tab, 200, 204); got != 0 {
		t.Fatalf("shells below the core are not a hop target, got %d", got)
	}
}

// TestSessionAdapterFindsA covers the three shapes a session's adapter takes:
// spawned (the adapter is still the launcher's child), reparented away (its
// --parent-pid is the only witness left), and a joined shared adapter, which is
// found through the session hold the joiner spawned (#1022).
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
		{300, "sub"}, // spawned: still the launcher's child
		{10, ""},
		{400, ""},
	} {
		if got := SessionAdapter(tab, c.launcher); got != c.want {
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
	if got := SessionAdapter(tab, launcher); got != "gw" {
		t.Fatalf("a reparented adapter must be found by its --parent-pid, got %q (row %+v)", got, r)
	}
}

// TestSessionAdapterFindsAJoinedAdaptersHold is the joined shape through the
// real process table: the session holds no adapter of its own, only the
// route-hold child that names the adapter's users file (#1022).
func TestSessionAdapterFindsAJoinedAdaptersHold(t *testing.T) {
	users := AdapterBase(t.TempDir(), "gw", "0123456789abcdef") + ".users"
	core := coreCopy(t)
	hold := holdProc(t, core, users, core, "util", "route-hold", users, routeHoldExitStdin)

	tab := processTable()
	if r := tab[hold.Process.Pid]; r.ppid != os.Getpid() {
		t.Fatalf("the fixture hold must be this launcher's child, got %+v", r)
	}
	if got := SessionAdapter(tab, os.Getpid()); got != "gw" {
		t.Fatalf("a joined adapter must be named by its session hold, got %q", got)
	}
}

// TestAnAdapterInArgvWinsOverAHold pins the precedence: this session has both a
// real hold and an adapter's argv, and the adapter answers whichever side of the
// hold the walk meets first — the pids ascend, so map iteration cannot decide.
func TestAnAdapterInArgvWinsOverAHold(t *testing.T) {
	users := AdapterBase(t.TempDir(), "gw", "0123456789abcdef") + ".users"
	core := coreCopy(t)
	holdProc(t, core, users, core, "util", "route-hold", users, routeHoldExitStdin)
	for _, pid := range []int{2, 1 << 30} {
		tab := processTable()
		tab[pid] = psRow{ppid: os.Getpid(), args: "/x/libexec/ccmorph-core adapter eq --auto"}
		if got := SessionAdapter(tab, os.Getpid()); got != "eq" {
			t.Fatalf("adapter at pid %d: got %q, want the adapter's profile", pid, got)
		}
	}
}

// TestASubagentHoldNamesNoProfile is #1022's second half: the router acquires
// shared adapters for subagent picks from the session launcher itself, so its
// hold is a child of the launcher. It must never answer for the session, and a
// session's own hold must win when both are present (#1022).
func TestASubagentHoldNamesNoProfile(t *testing.T) {
	dir := t.TempDir()
	core := coreCopy(t)
	gw := AdapterBase(dir, "gw", "0123456789abcdef") + ".users"
	eq := AdapterBase(dir, "eq", "fedcba9876543210") + ".users"
	// The subagent's hold is the older child: the ascending walk meets it first.
	holdProc(t, core, eq, core, "util", "route-hold", eq, routeHoldExitStdin, routeHoldSubagent)
	if got := SessionAdapter(processTable(), os.Getpid()); got != "" {
		t.Fatalf("a subagent pick's hold must name no session profile, got %q", got)
	}
	holdProc(t, core, gw, core, "util", "route-hold", gw, routeHoldExitStdin)
	if got := SessionAdapter(processTable(), os.Getpid()); got != "gw" {
		t.Fatalf("the session's own hold must win over a subagent's, got %q", got)
	}
}

// TestARouteHoldIsNotASessionMarker pins the shape: a provider route's hold
// names the bridge's users file with the four-word argv, and a flagged hold on
// a path that is not an adapter's names nothing either.
func TestARouteHoldIsNotASessionMarker(t *testing.T) {
	dir := t.TempDir()
	core := coreCopy(t)
	users := AdapterBase(dir, "gw", "0123456789abcdef") + ".users"
	holdProc(t, core, users, core, "util", "route-hold", users)
	if got := SessionAdapter(processTable(), os.Getpid()); got != "" {
		t.Fatalf("a route hold is not a session's adapter, got %q", got)
	}
	other := filepath.Join(dir, "route.users")
	holdProc(t, core, other, core, "util", "route-hold", other, routeHoldExitStdin)
	if got := SessionAdapter(processTable(), os.Getpid()); got != "" {
		t.Fatalf("a hold on a file no adapter owns names no profile, got %q", got)
	}
	// A subagent's flagged hold on an adapter-shaped path is still not this
	// session's profile.
	agent := AdapterBase(dir, "eq", "0123456789abcdef") + ".users"
	holdProc(t, core, agent, core, "util", "route-hold", agent, routeHoldExitStdin, routeHoldSubagent)
	if got := SessionAdapter(processTable(), os.Getpid()); got != "" {
		t.Fatalf("a subagent's hold is not a session marker, got %q", got)
	}
}

// TestSharedAdapterLauncherChild is one launcher process for the two-process
// tests below: it takes this session's use of the shared adapter for gw,
// reports the adapter it got, and waits until the parent closes its stdin. A
// launcher is a real process there because the hop reads the process table —
// the joiner's own children are the whole evidence. Run on its own it does
// nothing.
func TestSharedAdapterLauncherChild(t *testing.T) {
	core := os.Getenv("CCMORPH_TEST_ADAPTER_CORE")
	if core == "" {
		return
	}
	name := os.Getenv("CCMORPH_TEST_ADAPTER_NAME")
	if name == "" {
		name = "gw"
	}
	a, ok := AcquireAdapter(core, os.Getenv("CCMORPH_TEST_ADAPTER_ROOT"), name, "m",
		os.Getenv("CCMORPH_TEST_ADAPTER_CONF"), io.Discard)
	if !ok {
		fmt.Println("FAILED")
		os.Exit(1)
	}
	fmt.Printf("ACQUIRED %d\n", a.PID)
	io.Copy(io.Discard, os.Stdin)
	a.Release()
}

// adapterLauncher is one launcher process of the tests below.
type adapterLauncher struct {
	cmd     *exec.Cmd
	in      *os.File // its stdin: closing it ends the launcher, which releases
	adapter int      // the shared adapter it reported
	waited  bool
}

// end lets the launcher release and exit, as a session ending normally does.
func (l *adapterLauncher) end() {
	l.in.Close()
	if !l.waited {
		l.waited = true
		l.cmd.Wait()
	}
}

// kill drops the launcher with no chance to release, as a crash or SIGKILL does.
func (l *adapterLauncher) kill() {
	l.in.Close()
	l.cmd.Process.Kill()
	if !l.waited {
		l.waited = true
		l.cmd.Wait()
	}
}

// launcherLine reads the launcher's one report, bounded by half of what this
// run has left: a start the scheduler starved still lands, and one that never
// reports ends the test with a verdict (#998).
func launcherLine(t *testing.T, r *os.File) string {
	t.Helper()
	end, ok := t.Deadline()
	if !ok {
		end = time.Now().Add(defaultTestTimeout)
	}
	r.SetReadDeadline(time.Now().Add(end.Sub(time.Now()) / 2))
	var line []byte
	one := make([]byte, 1)
	for {
		if _, err := r.Read(one); err != nil {
			t.Fatalf("the launcher never reported its adapter: %v (%q)", err, line)
		}
		if one[0] == '\n' {
			return string(line)
		}
		line = append(line, one[0])
	}
}

// startAdapterLauncher runs this test binary as one more launcher for root's
// name adapter and returns once it has acquired its use.
func startAdapterLauncher(t *testing.T, core, root, name, conf string) *adapterLauncher {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedAdapterLauncherChild$")
	cmd.Env = append(os.Environ(),
		"CCMORPH_TEST_ADAPTER_CORE="+core,
		"CCMORPH_TEST_ADAPTER_ROOT="+root,
		"CCMORPH_TEST_ADAPTER_NAME="+name,
		"CCMORPH_TEST_ADAPTER_CONF="+conf)
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inR.Close()
	outW.Close()
	line := launcherLine(t, outR)
	outR.Close()
	l := &adapterLauncher{cmd: cmd, in: inW}
	if !strings.HasPrefix(line, "ACQUIRED ") {
		l.kill()
		t.Fatalf("the launcher did not acquire the adapter: %q", line)
	}
	if l.adapter, err = strconv.Atoi(strings.TrimPrefix(line, "ACQUIRED ")); err != nil {
		l.kill()
		t.Fatalf("the launcher reported no adapter pid: %q", line)
	}
	t.Cleanup(l.kill)
	return l
}

// TestAJoiningLauncherCanNameItsSharedAdapter is #1022's regression: two
// launchers acquire the same key, one starts the adapter and one joins it, and
// the joining launcher's SessionAdapter names the profile. Before the session
// hold, the joiner started no process of its own and answered "" — which is how
// `ccmorph hop` lost the `now on` mark and the #257 no-change guard.
func TestAJoiningLauncherCanNameItsSharedAdapter(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	if err := os.WriteFile(conf, []byte("base_url=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := startAdapterLauncher(t, core, root, "gw", conf)
	second := startAdapterLauncher(t, core, root, "gw", conf)
	if first.adapter != second.adapter {
		t.Fatalf("the second launcher must join the first's adapter: pids %d and %d", first.adapter, second.adapter)
	}
	// Each launcher keeps exactly one hold, on the one adapter's users file.
	users := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core)) + ".users"
	sessionHoldOf(t, first.cmd.Process.Pid, users)
	sessionHoldOf(t, second.cmd.Process.Pid, users)
	tab := ProcessTable()
	if got := SessionAdapter(tab, second.cmd.Process.Pid); got != "gw" {
		t.Fatalf("the joining launcher's session must name its profile, got %q", got)
	}
	if got := SessionAdapter(tab, first.cmd.Process.Pid); got != "gw" {
		t.Fatalf("the starting launcher's session must name its profile, got %q", got)
	}
	if got := SessionAdapter(tab, os.Getpid()); got != "" {
		t.Fatalf("this test process holds no session use of the adapter, got %q", got)
	}
	second.end()
	// A session that ends takes its own hold and nothing else: the starter still
	// names its profile, and the adapter it started is still up.
	firstTab := ProcessTable()
	if got := SessionAdapter(firstTab, first.cmd.Process.Pid); got != "gw" {
		t.Fatalf("the starter must still name its profile after the joiner ends, got %q", got)
	}
	if !alive(first.adapter) {
		t.Fatal("the joiner's normal end must not stop the adapter the starter holds")
	}
	first.end()
}

// TestTwoSessionsNameTheirOwnProfiles pins that a hold is read for its own
// launcher alone: two launchers on two profiles each names only their own.
func TestTwoSessionsNameTheirOwnProfiles(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	if err := os.WriteFile(conf, []byte("base_url=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gw := startAdapterLauncher(t, core, root, "gw", conf)
	eq := startAdapterLauncher(t, core, root, "eq", conf)
	if gw.adapter == eq.adapter {
		t.Fatalf("two profiles are two adapters, got pid %d for both", gw.adapter)
	}
	tab := ProcessTable()
	if got := SessionAdapter(tab, gw.cmd.Process.Pid); got != "gw" {
		t.Fatalf("launcher A must name gw, got %q", got)
	}
	if got := SessionAdapter(tab, eq.cmd.Process.Pid); got != "eq" {
		t.Fatalf("launcher B must name eq, got %q", got)
	}
	gw.end()
	eq.end()
}

// TestASessionHoldDiesWithItsLauncher is #1022's lifetime half: the hold that
// names a joined adapter belongs to its launcher alone, so a launcher killed
// without releasing leaves no hold behind to keep the adapter (and its users
// file) alive; the last session out still stops it.
func TestASessionHoldDiesWithItsLauncher(t *testing.T) {
	core, root := sharedCore(t), t.TempDir()
	conf := filepath.Join(t.TempDir(), "gw.conf")
	if err := os.WriteFile(conf, []byte("base_url=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := startAdapterLauncher(t, core, root, "gw", conf)
	second := startAdapterLauncher(t, core, root, "gw", conf)
	users := AdapterBase(root, "gw", AdapterKey("gw", "m", conf, core)) + ".users"
	hold := sessionHoldOf(t, second.cmd.Process.Pid, users)

	second.kill() // no release runs: the pipe is the only thing that can end the hold
	waitExit(t, hold)
	if !alive(first.adapter) {
		t.Fatal("the starter's adapter must keep running while its session lives")
	}
	first.end()
	waitExit(t, first.adapter)
}

// sessionHoldOf is the launcher's child that holds users as its session marker,
// and proves it is the only one: a second marker would outlive nothing useful
// and the hop must have exactly one child to read.
func sessionHoldOf(t *testing.T, launcher int, users string) int {
	t.Helper()
	found, hold := 0, 0
	for pid, r := range processTable() {
		if r.ppid != launcher {
			continue
		}
		if got, role, ok := routeHoldUsers(pid); ok && role == holdSession && got == users {
			found, hold = found+1, pid
		}
	}
	if found != 1 {
		t.Fatalf("launcher %d has %d session holds on %s, want exactly one", launcher, found, users)
	}
	return hold
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

// A ps pass that exits but whose stdout a subprocess still holds must not hold
// the read until that subprocess ends (GH-449): the pass's pipes are the read's
// own, and WaitDelay closes them at the grace instead of reading to EOF. The
// fake ps hands its stdout to a background sleep and exits, so the pipe outlives
// it by seconds; without the grace, Output waits for the sleep's own end.
func TestPsProcessTableDoesNotWaitOnAGrandchildHoldingThePipe(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	script := "#!/bin/sh\n/bin/sleep 5 &\necho $! > '" + pidFile + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := psProcessTable(ctx); got != nil {
		t.Fatalf("a pass whose pipe is closed at the grace must yield no table, got %d rows", len(got))
	}
	// The sleep outlives the bound by seconds: an answer at the bound can only
	// come from the pipe's grace, not from the subprocess ending.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a grandchild holding the pipe kept the read for %v; WaitDelay must close it", elapsed)
	}
}
