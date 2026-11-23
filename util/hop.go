package util

import (
	"context"
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

// processTable is one read of the process table: pid -> parent, executable,
// argv and start time; sysctl on darwin, ps elsewhere or when that read fails
// its self-check. Its ps fallback reads without a deadline: a caller that needs
// one uses processTableWithin.
func processTable() map[int]psRow { return processTableWithin(context.Background()) }

// processTableWithin is processTable with the ps fallback bounded by ctx: a
// caller that has given up on the read leaves no ps of its own running.
func processTableWithin(ctx context.Context) map[int]psRow {
	if t, ok := sysctlTable(); ok {
		return t
	}
	return psProcessTable(ctx)
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
// deadline rather than outliving the read. The rows without their command are
// no table: a pass that cannot read it declines the read, because a half-read
// table answers argv questions — and the adapter question — with silence.
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
	// ww: an unbounded command column. ps sizes that column to COLUMNS (or the
	// caller's terminal) when the output is a pipe, and a caller that exports
	// COLUMNS — a pytest-xdist worker does, GH-819 — cut a live session's argv
	// right through the middle, so the adapter hunt read
	// "/Users/…/.venv/bin/python /private/var/…" and stopped: the session was
	// there and the read claimed there was none. The reader's question is
	// never the caller's terminal's width, so ask for the whole line.
	cmdRaw, err := psCmd(ctx, "-axwwo", "pid=,command=").Output()
	if err != nil {
		// Without the command column no argv question can be answered: rows
		// of comm alone let findAdapter miss the adapter it hunts and say
		// there is none. A pass killed at the bound leaves exactly that half
		// table standing as a success — the half-reported answer GH-467
		// promised never to give. Decline with the rest of the read.
		return nil
	}
	for _, l := range strings.Split(string(cmdRaw), "\n") {
		l = strings.TrimSpace(l)
		p, rest, _ := strings.Cut(l, " ")
		if pid, err := strconv.Atoi(p); err == nil {
			r := rows[pid]
			r.args = strings.TrimSpace(rest)
			rows[pid] = r
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
