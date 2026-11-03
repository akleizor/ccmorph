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
// or the state run dir was orphaned by a killed launcher and goes (#142).
// Launch-settings overlays and provider bridge dirs carry credentials, so they go
// after 12h unless a live process still names them; the process table is read at
// most once, and only when something is old.
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
	var bridges, envs []string
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
		}
	}
	// The run dir is read by name, never globbed: a state dir can hold glob
	// metacharacters (#113), and the env file lands there when TMPDIR cannot
	// be written.
	if run, err := os.ReadDir(filepath.Join(state, "run")); err == nil {
		for _, e := range run {
			if strings.HasPrefix(e.Name(), "ccmorph-session-env.") {
				envs = append(envs, filepath.Join(state, "run", e.Name()))
			}
		}
	}
	for _, f := range envs {
		if isFile(f) && old(f, time.Minute) {
			_ = os.Remove(f)
		}
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
