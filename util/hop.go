package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// processTable is one read of the process table: pid -> parent, executable,
// argv and start time; sysctl on darwin, ps elsewhere or when that read fails
// its self-check.
func processTable() map[int]psRow {
	if t, ok := sysctlProcessTable(); ok {
		return t
	}
	return psProcessTable()
}

// psProcessTable is processTable through ps. The start times come from a read
// of their own: a ps without lstart then costs the times and not the rows hop
// needs, and a line that does not parse leaves its row at the cannot-tell zero.
func psProcessTable() map[int]psRow {
	raw, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
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
	if raw, err := exec.Command("ps", "-axo", "pid=,command=").Output(); err == nil {
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
	lstart := exec.Command("ps", "-axo", "pid=,lstart=")
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

// HopTarget is the Claude Code process to end for a hop: the first process below
// the relay in pid's ancestry that is neither a shell nor the core launcher (the
// shells between are the relay's forwarding subshells and the one Claude Code runs
// the command in). 0 when there is none.
func HopTarget(relay, pid int) int { return findHopTarget(processTable(), relay, pid) }

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
// from its argv; "" when there is none.
func SessionAdapter(launcher int) string { return findSessionAdapter(processTable(), launcher) }
