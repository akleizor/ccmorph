package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// GCStale is `util gc-stale <state dir> <profiles dir> <tmp dir>`: best-effort reaping of
// state a killed launch left behind. Relay signals go once their owning launcher is
// dead and they are a minute old. Session env files carry credentials and live
// milliseconds (their reader removes them on sight), so a minute-old one in TMPDIR
// or the state run dir was orphaned by a killed launcher and goes (#142). A
// private session's view — its real transcripts, history and shell snapshots —
// goes once the claude that ran from it is gone and it is a minute old; a view
// naming no claude is left alone, however old (#166). Gone is identity, not
// the number alone: the OS hands a dead pid's number to any later process, and
// a same-user one answers kill(pid, 0) just the same. So the holder is never
// asked what its command line says: a grep over notes, an editor on a
// claude.md and another Claude Code session that recycled the number all name
// claude (#206). It is asked when it started: a view's record is written the
// moment claude starts, a relay's name carries the launch's own clock, and a
// holder that started more than a minute after that is not the process it
// names. A time the table cannot read reads as the owner, and a relay name
// with no readable clock keeps kill alone: the sweep never takes live state on
// a guess (#189). Launch-settings overlays and provider bridge dirs carry
// credentials, so they go after 12h unless a live process still names them;
// the process table is read only when something is old, at most once for the
// view and relay sweeps and once for the overlays' and bridges' scan.
func GCStale(state, profiles, tmp string) {
	now := time.Now()
	old := func(p string, age time.Duration) bool {
		fi, err := os.Lstat(p)
		return err == nil && fi.Mode()&os.ModeSymlink == 0 && now.Sub(fi.ModTime()) >= age
	}

	// holds reports whether pid still runs the process a record's time names:
	// alive, and started no later than a minute after that time. A pid the
	// table cannot place, or one whose start it cannot read, holds: the sweep
	// never takes live state on a guess (#189).
	var tab map[int]psRow
	read := false
	holds := func(pid int, since time.Time) bool {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		if !read {
			tab, read = processTable(), true
		}
		r, ok := tab[pid]
		return !ok || r.start.IsZero() || !r.start.After(since.Add(time.Minute))
	}

	relays, _ := filepath.Glob(filepath.Join(state, ".reswitch.*"))
	for _, f := range relays {
		// The name is <launcher pid>.<launch seconds>_<microseconds>.<random>
		// (cli/run.go): the launch's own clock, so the launcher that queued it
		// started no later than that. A name with no readable clock — an older
		// build's, or a stray file — keeps the kill check alone.
		owner, rest, _ := strings.Cut(strings.TrimPrefix(filepath.Base(f), ".reswitch."), ".")
		pid, err := strconv.Atoi(owner)
		stamp, _, _ := strings.Cut(rest, ".")
		secS, usecS, hasUsec := strings.Cut(stamp, "_")
		sec, serr := strconv.ParseInt(secS, 10, 64)
		usec := 0
		if hasUsec {
			usec, _ = strconv.Atoi(usecS)
		}
		if err != nil || !isFile(f) || !old(f, time.Minute) {
			continue
		}
		if serr != nil {
			if syscall.Kill(pid, 0) == nil {
				continue
			}
		} else if holds(pid, time.Unix(sec, int64(usec)*1000)) {
			continue
		}
		_ = os.Remove(f)
	}

	overlays, _ := filepath.Glob(filepath.Join(profiles, "*", "cache", "launch-settings.*"))
	var bridges, envs, views []string
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
			}
		}
	}
	for _, f := range envs {
		if isFile(f) && old(f, time.Minute) {
			_ = os.Remove(f)
		}
	}
	// A private view goes once the claude that ran from it is gone and it is a
	// minute old, and stays however old it is while that claude lives: the
	// view holds a live session's transcripts. The holder is known by when it
	// started, never by a word in its command line (#206): the record is
	// written the moment claude starts, so a process that started more than a
	// minute after it is not that claude, whoever it is. A holder the table
	// cannot place or time reads as claude, and a view with no record is never
	// reaped — this build writes the record before claude writes anything into
	// the view, so a record-less view either predates the record or failed to
	// write it, and either can still be a live session whose transcripts age
	// while claude idles (#166, #189).
	for _, d := range views {
		if !isDir(d) {
			continue
		}
		rec := filepath.Join(d, privateOwner)
		b, err := os.ReadFile(rec)
		fi, serr := os.Stat(rec)
		if err != nil || serr != nil {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr != nil || pid <= 0 || !old(d, time.Minute) || holds(pid, fi.ModTime()) {
			continue // unreadable owner, under a minute, or the claude that runs from it lives
		}
		_ = os.RemoveAll(d)
	}
	var live *string
	alive := func(needle string) bool {
		if live == nil {
			// A failed scan names nothing, so every old candidate is reaped.
			s := ""
			if raw, err := exec.Command("ps", "-axo", "command").Output(); err == nil {
				s = string(raw)
			}
			live = &s
		}
		return *live != "" && strings.Contains(*live, needle)
	}
	for _, f := range overlays {
		if isFile(f) && old(f, 12*time.Hour) && !alive(f) {
			_ = os.Remove(f)
		}
	}
	for _, d := range bridges {
		if isDir(d) && old(d, 12*time.Hour) && !alive(d+"/config.yaml") {
			_ = os.RemoveAll(d)
		}
	}
}
