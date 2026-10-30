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

// processTable is one `ps` read: pid -> parent, executable and argv.
func processTable() map[int]psRow {
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

// hopTarget is `util hop-target <relay pid> <pid>`: the Claude Code process to end
// for a hop -- the first non-shell process below the relay in <pid>'s ancestry
// (the shells between are the relay's forwarding subshells and the one Claude Code
// runs the command in).
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
				if !shellComm.MatchString(filepath.Base(t[chain[i]].comm)) {
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
	for _, r := range processTable() {
		if r.ppid != parent {
			continue
		}
		if m := adapterArgv.FindStringSubmatch(r.args); m != nil {
			fmt.Fprintln(out, m[1])
			return 0
		}
	}
	return 1
}
