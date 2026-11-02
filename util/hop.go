package util

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type psRow struct {
	ppid int
	comm string
	args string
}

// processTable is one read of the process table: pid -> parent, executable and
// argv; sysctl on darwin, ps elsewhere or when that read fails its self-check.
func processTable() map[int]psRow {
	if t, ok := sysctlProcessTable(); ok {
		return t
	}
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
	return rows
}

var shellComm = regexp.MustCompile(`^-?(zsh|bash|sh|dash)$`)

// launcherComm is the Go launcher. claude runs as its child (the core's
// claude-session execs into it), so it stands between the relay and Claude Code
// and must never be the hop's target: TERM to it is read as a signal this launch
// forwarded, which discards the queued switch.
const launcherComm = "ccmorph-core"

// hopTarget is `util hop-target <relay pid> <pid>`: the Claude Code process to end
// for a hop -- the first process below the relay in <pid>'s ancestry that is
// neither a shell nor the core launcher (the shells between are the relay's
// forwarding subshells and the one Claude Code runs the command in).
func hopTarget(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	relay, e1 := strconv.Atoi(args[0])
	pid, e2 := strconv.Atoi(args[1])
	if e1 != nil || e2 != nil {
		return 1
	}
	if c := findHopTarget(processTable(), relay, pid); c > 0 {
		fmt.Fprintln(out, c)
		return 0
	}
	return 1
}

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

// sessionAdapter is `util session-adapter <launcher pid>`: the profile of the
// adapter that launcher started, read from its argv.
func sessionAdapter(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	parent, err := strconv.Atoi(args[0])
	if err != nil {
		return 1
	}
	if p := findSessionAdapter(processTable(), parent); p != "" {
		fmt.Fprintln(out, p)
		return 0
	}
	return 1
}
