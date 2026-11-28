package util

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// GCStale is `util gc-stale <state dir> <profiles dir> <tmp dir>`: best-effort reaping of
// state a killed launch left behind. Relay signals go once their owning launcher is
// dead and they are a minute old. Session env files carry credentials and live
// until their reader gets its turn — under a fork/exec storm that can be past a
// minute — so a minute-old one in TMPDIR or the state run dir goes only once the
// launcher its name carries is gone (#142, #1047). A
// session-end record — the id of a session that just ended, read once by its
// launcher for the resume hint — goes the same way (#285). A
// private session's view — its real transcripts, history and shell snapshots —
// goes once the claude that ran from it is gone and it is a minute old; a view
// naming no claude is left alone, however old (#166). Gone is identity, not
// the number alone: the OS hands a dead pid's number to any later process, and
// a same-user one answers kill(pid, 0) just the same. So the holder is never
// asked what its command line says: a grep over notes, an editor on a
// claude.md and another Claude Code session that recycled the number all name
// claude (#206). It is asked when it started. A relay's name carries the
// launch's own clock, so a launcher that started more than a minute after it
// is not the one it names; a session env file's name carries the launcher's
// pid alone, so it is spared while that pid lives (#1047). A view keeps the
// kernel's own mark of its claude's
// start beside the record, so a holder whose mark differs is not that claude;
// no wall-clock step or filesystem clock moves that mark, while a record's
// mtime is a filesystem's clock read against the kernel's (#218). A time the
// table cannot read reads as the owner, and a view with no readable mark or a
// relay name with no readable clock keeps kill alone: the sweep never takes
// live state on a guess (#189).
// Launch-settings overlays and provider bridge dirs carry
// credentials, so they go after 12h unless a live process still names them;
// the process table is read only when something is old, at most once for the
// view and relay sweeps and once for the overlays' and bridges' scan.
// gcScanLimit bounds the process scan GCStale makes on the launch path, the
// bound runningAdapter's table read has (GH-467).
const gcScanLimit = time.Second

func GCStale(state, profiles, tmp string) {
	now := time.Now()
	old := func(p string, age time.Duration) bool {
		fi, err := os.Lstat(p)
		return err == nil && fi.Mode()&os.ModeSymlink == 0 && now.Sub(fi.ModTime()) >= age
	}

	// reap is the one stale-record decision both sweeps read (#1047): a
	// regular file a minute old may go only once the holder its name carries
	// is gone. Names with no pid keep age alone. A relay's launch clock can
	// also prove a recycled pid by the start the table gives it (#189, #206).
	var tab map[int]psRow
	read := false
	reap := func(f string, pid int, hasPid bool, since time.Time) bool {
		if !paths.IsFile(f) || !old(f, time.Minute) {
			return false
		}
		if !hasPid {
			return true
		}
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return true
		}
		// EPERM and other probe failures are not proof the holder is gone.
		if err != nil || since.IsZero() {
			return false
		}
		if !read {
			tab, read = processTable(), true
		}
		r, ok := tab[pid]
		return ok && !r.start.IsZero() && r.start.After(since.Add(time.Minute))
	}

	relays, _ := filepath.Glob(filepath.Join(paths.GlobQuote(state), ".reswitch.*"))
	for _, f := range relays {
		// The name is <launcher pid>.<launch seconds>_<microseconds>.<random>
		// (cli/run.go): the launch's own clock, so the launcher that queued it
		// started no later than that. A name with no readable clock — an older
		// build's, or a stray file — keeps the kill check alone.
		owner, rest, _ := strings.Cut(strings.TrimPrefix(filepath.Base(f), ".reswitch."), ".")
		pid, err := strconv.Atoi(owner)
		if err != nil {
			continue // no launcher pid: never age-only reaped
		}
		stamp, _, _ := strings.Cut(rest, ".")
		secS, usecS, hasUsec := strings.Cut(stamp, "_")
		sec, serr := strconv.ParseInt(secS, 10, 64)
		usec := 0
		if hasUsec {
			usec, _ = strconv.Atoi(usecS)
		}
		var since time.Time
		if serr == nil {
			since = time.Unix(sec, int64(usec)*1000)
		}
		if reap(f, pid, true, since) {
			_ = os.Remove(f)
		}
	}

	overlays, _ := filepath.Glob(filepath.Join(paths.GlobQuote(profiles), "*", "cache", "launch-settings.*"))
	var bridges, envs, ends, views, agentRuns []string
	ents, _ := os.ReadDir(tmp)
	bridgePrefix := "ccmorph-bridge-" + strconv.Itoa(os.Getuid()) + "-"
	for _, e := range ents {
		switch n := e.Name(); {
		case strings.HasPrefix(n, "ccmorph-launch-settings."):
			overlays = append(overlays, filepath.Join(tmp, n))
		case strings.HasPrefix(n, bridgePrefix):
			bridges = append(bridges, filepath.Join(tmp, n))
		case strings.HasPrefix(n, "ccmorph-session-env."):
			envs = append(envs, filepath.Join(tmp, n))
		case strings.HasPrefix(n, "ccmorph-session-end."):
			ends = append(ends, filepath.Join(tmp, n))
		case strings.HasPrefix(n, "ccmorph-private."):
			views = append(views, filepath.Join(tmp, n))
		}
	}
	// The run dir is read by name, never globbed: a state dir can hold glob
	// metacharacters (#113), and the env file and the private view land there
	// when TMPDIR cannot be written (#140).
	if run, err := os.ReadDir(filepath.Join(state, "run")); err == nil {
		for _, e := range run {
			switch n := e.Name(); {
			case strings.HasPrefix(n, "ccmorph-session-env."):
				envs = append(envs, filepath.Join(state, "run", n))
			case strings.HasPrefix(n, "ccmorph-private."):
				views = append(views, filepath.Join(state, "run", n))
			case strings.HasPrefix(n, "agent-proxy."):
				agentRuns = append(agentRuns, filepath.Join(state, "run", n))
			}
		}
	}
	// An env file's name carries the launcher that wrote it (#1047): while that
	// pid lives, the file is a launch whose helper has yet to get its turn, and
	// stays however old it is. A name with no pid — an older build's, or a
	// stray one — keeps age alone.
	for _, f := range envs {
		pid, hasPid := envHolder(filepath.Base(f))
		if reap(f, pid, hasPid, time.Time{}) {
			_ = os.Remove(f)
		}
	}
	// A session-end record is consumed the instant its launcher's claude exits;
	// a minute-old one belongs to a launcher that died before it could read it.
	for _, f := range ends {
		if paths.IsFile(f) && old(f, time.Minute) {
			_ = os.Remove(f)
		}
	}
	// A private view goes once the claude that ran from it is gone and it is a
	// minute old, and stays however old it is while that claude lives: the
	// view holds a live session's transcripts. The holder is known by the
	// kernel's own mark of when it started, never by a word in its command
	// line (#206) and never by a clock: the helper reads that mark the moment
	// claude starts and keeps it beside the record, and the sweep reads it
	// again for the pid now. It is the same kernel field, which no wall-clock
	// step and no filesystem's clock can move, so the same mark is the same
	// claude and another mark is a later process that took the number. The
	// record's mtime is a filesystem's clock against the kernel's, two clocks
	// nothing keeps in step, so it names no one (#218). A mark the sweep
	// cannot read now, and a record with no mark beside it (written by a build
	// older than the mark, or one whose mark failed to write), is cannot-tell,
	// which holds while the pid lives: the sweep never takes live state on a
	// guess, and such a view goes once the number is free (#189). A view with
	// no record is never reaped: this build writes the record before claude
	// writes anything into the view, so a record-less view either predates the
	// record or failed to write it, and either can still be a live session
	// whose transcripts age while claude idles (#166).
	for _, d := range views {
		if !paths.IsDir(d) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, privateOwner))
		if err != nil {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr != nil || pid <= 0 || !old(d, time.Minute) {
			continue // unreadable owner, or under a minute
		}
		if syscall.Kill(pid, 0) == nil {
			want, merr := os.ReadFile(filepath.Join(d, privateOwnerStart))
			cur, ok := ProcStartToken(pid)
			if merr != nil || strings.TrimSpace(string(want)) == "" || !ok || cur == strings.TrimSpace(string(want)) {
				continue // the claude that runs from it lives, or cannot-tell
			}
		}
		_ = os.RemoveAll(d)
	}
	// An agent proxy's run dir (run/agent-proxy.<launcher pid>, cli/agentproxy.go)
	// holds the trust bundle and the Bash env file; its launcher removes it on
	// the way out, so one a minute old whose launcher is gone was left by a kill.
	// The launcher's pid is the only owner mark, so a number that answers kill
	// keeps the dir: the sweep never takes live state on a guess (#189).
	for _, d := range agentRuns {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "agent-proxy."))
		if err == nil && pid > 0 && paths.IsDir(d) && old(d, time.Minute) && syscall.Kill(pid, 0) == syscall.ESRCH {
			_ = os.RemoveAll(d)
		}
	}
	var live *string
	unknown := false // the one scan cannot be told apart from no scan: it stalled, or its table was discarded
	alive := func(needle string) bool {
		if unknown {
			return true
		}
		if live == nil {
			// A scan whose ps exited non-zero is a failed scan: it names
			// nothing, so every old candidate is reaped. A scan past
			// gcScanLimit cannot tell, so it keeps them all for the next
			// launch: the launch never waits on a stalled ps (#185). A
			// pass that exits 0 but pays the pipe grace — a grandchild
			// inherited the pipe, psCmd's own #449 — cannot tell either:
			// exec returns ErrWaitDelay and discards the complete table
			// with it, and reaping on information thrown away would take
			// a live session's overlay and bridge dir (#1064).
			ctx, cancel := context.WithTimeout(context.Background(), gcScanLimit)
			raw, err := psCmd(ctx, "-axo", "command").Output()
			unknown = ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay)
			cancel()
			if unknown {
				return true
			}
			s := ""
			if err == nil {
				s = string(raw)
			}
			live = &s
		}
		return *live != "" && strings.Contains(*live, needle)
	}
	for _, f := range overlays {
		if paths.IsFile(f) && old(f, 12*time.Hour) && !alive(f) {
			_ = os.Remove(f)
		}
	}
	for _, d := range bridges {
		if paths.IsDir(d) && old(d, 12*time.Hour) && !alive(d+"/config.yaml") {
			_ = os.RemoveAll(d)
		}
	}
}

// envHolder reads the launcher pid a session env file's name carries: the
// launcher writes ccmorph-session-env.<pid>.<random> (#1047). An older build's
// name is ccmorph-session-env.<random>, with no pid, and a stray or planted
// name may carry none either; those keep the age-only reaping they always had.
func envHolder(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "ccmorph-session-env.")
	if !ok {
		return 0, false
	}
	owner, random, ok := strings.Cut(rest, ".")
	if !ok || random == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(owner)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
