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
// goes once the claude named inside it is gone — a pid another process now
// holds is not that claude, however lively (#189) — and it is a minute old; a
// view naming no claude is left alone, however old (#166). Launch-settings
// overlays and provider bridge dirs carry credentials, so they go after 12h
// unless a live process still names them; the process table is read only when
// something is old, at most once for a view's owner pid and once for the
// overlays' and bridges' scan.
func GCStale(state, profiles, tmp string) {
	now := time.Now()
	old := func(p string, age time.Duration) bool {
		fi, err := os.Lstat(p)
		return err == nil && fi.Mode()&os.ModeSymlink == 0 && now.Sub(fi.ModTime()) >= age
	}

	relays, _ := filepath.Glob(filepath.Join(state, ".reswitch.*"))
	for _, f := range relays {
		owner, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(f), ".reswitch."), ".")
		pid, err := strconv.Atoi(owner)
		if err != nil || !isFile(f) || syscall.Kill(pid, 0) == nil || !old(f, time.Minute) {
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
	// A private view goes once the claude named inside it is gone and it is a
	// minute old, and stays however old it is while that claude lives: the
	// view holds a live session's transcripts. Gone is identity, not the
	// number alone: the OS hands a dead pid's number to any later process,
	// and a same-user one answers kill(pid, 0) just the same, so the pid's
	// own command line must still name claude — the launch resolves claude
	// by that name and runs it with argv[0] "claude", so each form it takes
	// (a native binary, or a script whose claude-named path the kernel keeps
	// in argv) names it. A command the table cannot read — the table itself
	// is unreadable, or the process left between the two checks — reads as
	// claude: the sweep never takes a live session's transcripts on a guess
	// (#189). A view naming no claude is never reaped — this build writes
	// the record before claude writes anything into the view, so a
	// record-less view either predates the record or failed to write it, and
	// either can still be a live session whose transcripts age while claude
	// idles (#166).
	var tab map[int]psRow
	read := false
	runsClaude := func(pid int) bool {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		if !read {
			tab, read = processTable(), true
		}
		r, ok := tab[pid]
		return !ok || r.args == "" || strings.Contains(r.args, "claude")
	}
	for _, d := range views {
		if !isDir(d) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, privateOwner))
		if err != nil {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr != nil || pid <= 0 || !old(d, time.Minute) || runsClaude(pid) {
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
