package util

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type psRow struct {
	ppid  int
	comm  string
	args  string
	start time.Time // when the process started; zero when the table cannot tell
}

// sysctlTable is sysctlProcessTable. A variable so a test can reach the ps
// fallback on darwin, where the kernel's read never fails and no PATH double
// reaches it.
var sysctlTable = sysctlProcessTable

// procfsTable is procfsProcessTable, a variable so a test can reach the ps
// fallback where the kernel's /proc answers, as sysctlTable is there for the
// read's self-check.
var procfsTable = procfsProcessTable

// processTable is one read of the process table: pid -> parent, executable,
// argv and start time; sysctl on darwin, /proc elsewhere where the kernel
// keeps one, and ps where neither read answers. Its ps fallback reads without
// a deadline: a caller that needs one uses processTableWithin.
func processTable() map[int]psRow { return processTableWithin(context.Background()) }

// processTableWithin is processTable with the ps fallback bounded by ctx: a
// caller that has given up on the read leaves no ps of its own running. The
// kernel reads come first because ps is itself an exec, and under a fork
// storm the exec is the part that queues — which is how a bounded reader
// lost the table while the kernel's own copy sat readable (#991).
func processTableWithin(ctx context.Context) map[int]psRow {
	if t, ok := sysctlTable(); ok {
		return t
	}
	if t := procfsTable(); t != nil {
		return t
	}
	return psProcessTable(ctx)
}

// procfsClkTck is the clock tick /proc counts start times in (USER_HZ): 100 on
// every Linux this repository runs on. A host that ticks differently gets its
// start times zeroed by the sanity check below, which the stale sweep holds
// on (#189) — never a reaping on a wrong time.
const procfsClkTck = 100

// procfsRoot is the kernel's process table. A test points it at a fixture it
// controls — the vanish-between-reads state cannot be caught in the live one
// — while every other reader walks "/proc" itself.
var procfsRoot = "/proc"

// procfsGone is the kernel's word for a pid that is no longer there: its
// directory was listed and is gone by the read. That absence is proven, so
// the row simply is not there. Any other read error is not a proof of
// anything — it voids the whole table (nil), and the caller's next reader
// answers, because a row lost mid-walk would otherwise read as the process
// never was, which is the silence #991 is about.
func procfsGone(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// procfsProcessTable is the process table read from /proc directly, with no
// exec: ps reads the very same files and adds a process to pay for it, so a
// PATH ps stalled by the run queue bounds doctor's adapter line and rc-hosts'
// session_pid() alike while the kernel waits unasked (#991). Rows carry what
// every other read carries: parent, comm, the joined command line — ps joins
// argv with spaces and so does this, from the NULs the kernel keeps — and the
// start time. A pid proven gone between the listing and its read is simply
// absent; a read that merely failed gives up the whole table rather than one
// row (procfsGone). A start that reads after now means the ticks or the boot
// time do not describe this machine, and the row keeps the cannot-tell zero
// the sweep holds on. nil where /proc is absent or cannot be trusted, so the
// caller falls back to ps.
func procfsProcessTable() map[int]psRow {
	dir, err := os.Open(procfsRoot)
	if err != nil {
		return nil
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return nil
	}
	boot, haveBoot := procfsBootTime()
	now := time.Now()
	rows := map[int]psRow{}
	for _, name := range names {
		pid, e := strconv.Atoi(name)
		if e != nil || pid <= 0 {
			continue
		}
		st, err := os.ReadFile(procfsRoot + "/" + name + "/stat")
		if err != nil {
			if procfsGone(err) {
				continue
			}
			return nil
		}
		ppid, comm, ticks, ok := parseProcStat(st)
		if !ok {
			continue
		}
		args, err := procfsCmdline(pid)
		if err != nil {
			if procfsGone(err) {
				continue // gone after the stat: it has no row, not an old one
			}
			return nil
		}
		r := psRow{ppid: ppid, comm: comm, args: args}
		if haveBoot {
			if start, ok := procfsStart(boot, ticks, now); ok {
				r.start = start
			}
		}
		rows[pid] = r
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// parseProcStat is the parent pid, the comm and the start ticks of one
// /proc/<pid>/stat line. comm is parenthesized and may itself hold spaces or
// parens, so the fields are counted from the last ')': state is 0, the parent
// 1, and starttime — field 22 of the line — the twentieth after it, the same
// field procStartToken reads.
func parseProcStat(b []byte) (ppid int, comm string, ticks int64, ok bool) {
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return 0, "", 0, false
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 {
		return 0, "", 0, false
	}
	p, e1 := strconv.Atoi(f[1])
	t, e2 := strconv.ParseInt(f[19], 10, 64)
	if e1 != nil || e2 != nil {
		return 0, "", 0, false
	}
	return p, string(b[open+1 : end]), t, true
}

// procfsStart is the wall-clock start of a process from this boot's start and
// the process's start ticks: false when the answer is later than now — wrong
// ticks or a wrong anchor, which the caller leaves at the cannot-tell zero
// the stale sweep holds on (#189).
func procfsStart(boot time.Time, ticks int64, now time.Time) (time.Time, bool) {
	start := boot.Add(time.Duration(ticks/procfsClkTck)*time.Second +
		time.Duration(ticks%procfsClkTck)*10*time.Millisecond)
	if start.After(now) {
		return time.Time{}, false
	}
	return start, true
}

// procfsCmdline is pid's command line as ps prints it: the arguments the
// kernel keeps, NUL between them, joined with one space. An empty answer with
// no error is the kernel's own: a zombie or kernel thread names no command
// line here at all. An error is the kernel's too, and the caller tells a
// vanished pid from a lost read with procfsGone.
func procfsCmdline(pid int) (string, error) {
	b, err := os.ReadFile(procfsRoot + "/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", nil // a kernel thread, or a zombie: ps prints no argv either
	}
	b = bytes.TrimSuffix(b, []byte{0})
	return strings.Join(strings.Split(string(b), "\x00"), " "), nil
}

// procfsBootTime is this boot's wall-clock start, /proc/stat's btime: the
// anchor for starttime's ticks since boot.
func procfsBootTime() (time.Time, bool) {
	b, err := os.ReadFile(procfsRoot + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if f := strings.Fields(string(line)); len(f) == 2 && f[0] == "btime" {
			if unix, e := strconv.ParseInt(f[1], 10, 64); e == nil && unix > 0 {
				return time.Unix(unix, 0), true
			}
		}
	}
	return time.Time{}, false
}

// psPipeGrace is how long a ps pass may keep its output pipe after the pass
// itself has exited: a subprocess that inherited the pipe must not hold the
// read to its own end (#449). The pass exits with the kernel's table, so the
// flush the grace pays for is the pipe's own.
const psPipeGrace = 100 * time.Millisecond

// psCmd is one ps pass under ctx. WaitDelay closes the pass's pipe at
// psPipeGrace when the pass exits but something it started still holds the
// other end, so an orphan cannot outlive the bound; a pass that pays the grace
// returns ErrWaitDelay and its output is discarded with the error.
func psCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ps", args...)
	cmd.WaitDelay = psPipeGrace
	return cmd
}

// psProcessTable is processTable through ps. The start times come from a read
// of their own: a ps without lstart then costs the times and not the rows hop
// needs, and a line that does not parse leaves its row at the cannot-tell zero.
// Every pass runs under ctx, so a stalled ps is killed with the caller's
// deadline rather than outliving the read.
func psProcessTable(ctx context.Context) map[int]psRow {
	raw, err := psCmd(ctx, "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}
	rows := map[int]psRow{}
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil {
			rows[pid] = psRow{ppid: ppid, comm: strings.Join(f[2:], " ")}
		}
	}
	if raw, err := psCmd(ctx, "-axo", "pid=,command=").Output(); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			l = strings.TrimSpace(l)
			p, rest, _ := strings.Cut(l, " ")
			if pid, err := strconv.Atoi(p); err == nil {
				r := rows[pid]
				r.args = strings.TrimSpace(rest)
				rows[pid] = r
			}
		}
	}
	// lstart is "Mon Jan _2 15:04:05 2006" in the locale's words, so the read
	// pins LC_ALL and the fields are joined back for the one layout parsed. A
	// ps that lacks lstart prints an error and exits non-zero: no times.
	lstart := psCmd(ctx, "-axo", "pid=,lstart=")
	lstart.Env = append(os.Environ(), "LC_ALL=C")
	if raw, err := lstart.Output(); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			f := strings.Fields(l)
			if len(f) < 6 {
				continue
			}
			pid, err := strconv.Atoi(f[0])
			if err != nil {
				continue
			}
			start, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[1:6], " "), time.Local)
			if err != nil {
				continue
			}
			if r, ok := rows[pid]; ok {
				r.start = start
				rows[pid] = r
			}
		}
	}
	return rows
}

var shellComm = regexp.MustCompile(`^-?(zsh|bash|sh|dash)$`)

// launcherComm is the Go launcher. claude runs as its child (the core's
// claude-session execs into it), so it stands between the relay and Claude Code
// and must never be the hop's target: TERM to it is read as a signal this launch
// forwarded, which discards the queued switch.
const launcherComm = "ccmorph-core"

// ProcTable is one read of the process table, for a command that asks more
// than one question of the same machine: a hop asks which profile this session
// runs on and which Claude Code process to end, moments apart, and one read
// answers both. The zero value is an empty table.
type ProcTable map[int]psRow

// ProcessTable is one read of the process table.
func ProcessTable() ProcTable { return processTable() }

// HopTarget is the Claude Code process to end for a hop: the first process below
// the relay in pid's ancestry that is neither a shell nor the core launcher (the
// shells between are the relay's forwarding subshells and the one Claude Code runs
// the command in). 0 when there is none. t is the read the command already made
// for its first question.
func HopTarget(t ProcTable, relay, pid int) int { return findHopTarget(t, relay, pid) }

func findHopTarget(t map[int]psRow, relay, pid int) int {
	var chain []int
	for n := 0; pid > 1 && n < 32; n++ {
		r, ok := t[pid]
		if !ok {
			return 0
		}
		chain = append(chain, pid)
		if r.ppid == relay {
			// Nearest the relay first.
			for i := len(chain) - 1; i >= 0; i-- {
				base := filepath.Base(t[chain[i]].comm)
				if !shellComm.MatchString(base) && base != launcherComm {
					return chain[i]
				}
			}
			return 0
		}
		pid = r.ppid
	}
	return 0
}

// underClaude says whether a Claude Code process is an ancestor of pid.
func underClaude(t map[int]psRow, pid int) bool {
	for n := 0; pid > 1 && n < 64; n++ {
		r, ok := t[pid]
		if !ok {
			return false
		}
		argv := append(strings.Fields(r.args), "", "")
		// native binary, a versioned install, an interpreted script, or npm's cli.js
		if filepath.Base(r.comm) == "claude" || filepath.Base(argv[0]) == "claude" || filepath.Base(argv[1]) == "claude" ||
			strings.Contains(r.args, "/claude/versions/") || strings.Contains(r.args, "@anthropic-ai/claude-code/") {
			return true
		}
		pid = r.ppid
	}
	return false
}

var adapterArgv = regexp.MustCompile(`ccmorph-(?:core )?adapter\s+(\S+)`)

// findSessionAdapter is the profile of the adapter <launcher> started. A session
// keeps one adapter alive across passes, but `util adapter-start` exits once it is
// ready, so the adapter is reparented and its --parent-pid argument names the
// launcher where its ppid no longer can. The pid must match whole, so 10 is never
// taken for 100.
func findSessionAdapter(t map[int]psRow, launcher int) string {
	parent := regexp.MustCompile(`(?:^|\s)--parent-pid[= ]` + strconv.Itoa(launcher) + `(?:\s|$)`)
	for _, r := range t {
		if r.ppid != launcher && !parent.MatchString(r.args) {
			continue
		}
		if m := adapterArgv.FindStringSubmatch(r.args); m != nil {
			return m[1]
		}
	}
	return ""
}

// SessionAdapter is the profile of the adapter the launcher pid started, read
// from its argv in t; "" when there is none.
func SessionAdapter(t ProcTable, launcher int) string { return findSessionAdapter(t, launcher) }

// adapterProfile is the profile an adapter's argv names: the core runs
// `ccmorph-core adapter <profile>`, the older build `ccmorph-adapter <profile>`,
// either possibly behind the python host a wrapped install runs it through.
func adapterProfile(a []string) (string, bool) {
	i := execIndex(a)
	if i < 0 || i+1 >= len(a) {
		return "", false
	}
	if filepath.Base(a[i]) == "ccmorph-core" && a[i+1] == "adapter" {
		if i+2 < len(a) {
			return a[i+2], true
		}
		return "", false
	}
	if filepath.Base(a[i]) == "ccmorph-adapter" {
		return a[i+1], true
	}
	return "", false
}

// findAdapter is the pid of the adapter serving profile, 0 when there is none.
// The table's command lines are joined text — they cannot say where one
// argument ends — so the match is the kernel's argv, read per pid: the joined
// line only cuts the walk to the pids that can match. Pids ascend, the order
// ps itself prints, so the earliest match is the one the table's own order
// always gave.
func findAdapter(t map[int]psRow, profile string) int {
	for _, pid := range sortedPids(t) {
		if adapterArgv.FindString(t[pid].args) == "" {
			continue
		}
		if argv, ok := argvOf(pid); ok {
			if name, ok := adapterProfile(argv); ok && name == profile {
				return pid
			}
		}
	}
	return 0
}

// sortedPids is the table's pids ascending, the order ps itself prints them.
func sortedPids(t map[int]psRow) []int {
	pids := make([]int, 0, len(t))
	for pid := range t {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}
