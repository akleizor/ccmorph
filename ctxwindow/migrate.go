package ctxwindow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/util/paths"
)

// globConf lists state/*.conf (sorted), as paths.
func globConf(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".conf") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func stem(path string) string {
	name := path[strings.LastIndex(path, "/")+1:]
	return strings.TrimSuffix(name, ".conf")
}

func legacyPath(state string) string {
	legacy := filepath.Join(state, "model-registry")
	if !paths.Exists(legacy) && paths.Exists(filepath.Join(state, "model-registry.conf")) {
		legacy = filepath.Join(state, "model-registry.conf")
	}
	return legacy
}

func cacheRoot() string {
	root := filepath.Join(StateDir(), "profiles")
	home, _ := os.UserHomeDir()
	// Pre-migration cache recovery: an unmoved legacy home (a real directory,
	// not the link the move leaves) still holds the caches, even once a
	// models.json has created the new root.
	if legacy := filepath.Join(home, ".claude-profiles"); !paths.Exists(root) || (paths.IsDir(legacy) && !paths.IsSymlink(legacy)) {
		root = legacy
	}
	return root
}

type cacheEntry struct {
	model string
	value int64
}

func cacheEntries(lines []string) []cacheEntry {
	var out []cacheEntry
	for _, line := range lines {
		model, value, ok := strings.Cut(line, "\t")
		if !ok || !ValidModel(model) {
			continue
		}
		if n, ok := Positive(value); ok {
			out = append(out, cacheEntry{strings.TrimSuffix(model, "[1m]"), n})
		}
	}
	return out
}

func archive(path string) error {
	backup := path + ".migrated"
	for n := 1; paths.Exists(backup); n++ {
		backup = fmt.Sprintf("%s.migrated.%d", path, n)
	}
	return os.Rename(path, backup)
}

func readLegacy(legacy string) ([]cacheEntry, error) {
	if paths.IsSymlink(legacy) {
		return nil, errors.New("legacy registry is a symlink; migration refused")
	}
	text, err := readText(legacy)
	if err != nil {
		return nil, err
	}
	var out []cacheEntry
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		if n, ok := Positive(value); ok {
			out = append(out, cacheEntry{key, n})
		}
	}
	return out, nil
}

// MigrationPreview is `--migrate --check`: what a migration would attribute.
func MigrationPreview() (string, error) {
	state := StateDir()
	legacy := legacyPath(state)
	type counts struct{ scoped, cached int }
	profiles := map[string]*counts{}
	var names []string
	for _, p := range globConf(state) {
		if s := stem(p); !paths.IsSymlink(p) && ValidProfileName(s) {
			profiles[s] = &counts{}
			names = append(names, s)
		}
	}
	attributed := map[string]bool{}
	root := cacheRoot()
	for _, profile := range names {
		cache := filepath.Join(root, profile, "cache/context-window")
		if paths.IsFile(cache) && !paths.IsSymlink(cache) {
			text, err := readText(cache)
			if err != nil {
				return "", err
			}
			for _, e := range cacheEntries(strings.Split(text, "\n")) {
				profiles[profile].cached++
				attributed[e.model] = true
			}
		}
	}
	unresolved := 0
	if paths.Exists(legacy) {
		entries, err := readLegacy(legacy)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			scope, _, _ := strings.Cut(e.model, ":")
			if _, ok := profiles[scope]; ok && strings.HasPrefix(e.model, scope+":") {
				profiles[scope].scoped++
			} else if !attributed[e.model] {
				unresolved++
			}
		}
	}
	ps := map[string]any{}
	for _, n := range names {
		ps[n] = map[string]int{"scoped": profiles[n].scoped, "cached": profiles[n].cached}
	}
	return jsonString(map[string]any{"source": legacy, "profiles": ps, "unattributed_entries": unresolved}), nil
}

// Migrate folds the legacy registry and per-profile caches into each profile's conf.
func Migrate(stderr io.Writer) (int, error) {
	state := StateDir()
	if fi, err := os.Stat(state); err != nil || !fi.IsDir() {
		return 0, nil
	}
	lock, err := os.OpenFile(filepath.Join(state, ".context-migration.lock"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	return migrateLocked(state, stderr)
}

func migrateLocked(state string, stderr io.Writer) (int, error) {
	legacy := legacyPath(state)
	root := cacheRoot()
	var caches []string
	if dirs, err := os.ReadDir(root); err == nil {
		for _, d := range dirs {
			c := filepath.Join(root, d.Name(), "cache/context-window")
			if _, err := os.Lstat(c); err == nil {
				caches = append(caches, c)
			}
		}
	}
	moved, err := moveInlineDiscovered(state)
	if err != nil {
		return 0, err
	}
	if !paths.Exists(legacy) && len(caches) == 0 {
		if moved > 0 {
			fmt.Fprintf(stderr, "Moved discovered context limits of %d profile(s) to profiles/<name>/models.json.\n", moved)
		}
		return 0, nil
	}
	var entries []cacheEntry
	if paths.Exists(legacy) {
		if entries, err = readLegacy(legacy); err != nil {
			return 0, err
		}
	}
	var profiles []string
	for _, conf := range globConf(state) {
		if conf[strings.LastIndex(conf, "/")+1:] == "model-registry.conf" || paths.IsSymlink(conf) || !ValidProfileName(stem(conf)) {
			continue
		}
		_, fields, err := ReadConf(conf)
		if err != nil {
			return 0, err
		}
		switch fields["type"] {
		case "apikey", "proxy", "provider", "token", "sub":
			profiles = append(profiles, stem(conf))
		}
	}
	var handled []string
	attributed := map[string]bool{}
	for _, c := range caches {
		if paths.IsSymlink(c) {
			return 0, fmt.Errorf("legacy context cache is a symlink; migration refused: %s", c)
		}
	}
	for _, profile := range profiles {
		expected := ProfileIdentity(profile)
		cache := filepath.Join(root, profile, "cache/context-window")
		values := map[string]int64{}
		if paths.IsFile(cache) && !paths.IsSymlink(cache) {
			text, err := readText(cache)
			if err != nil {
				return 0, err
			}
			for _, e := range cacheEntries(strings.Split(text, "\n")) {
				values[e.model] = e.value
				attributed[e.model] = true
			}
			handled = append(handled, cache)
		}
		// A legacy bare row has no route provenance. Only the profile's own cache
		// can attribute it; scoped rows supersede that profile-local fallback.
		for _, e := range entries {
			if strings.HasPrefix(e.model, profile+":") {
				values[e.model[len(profile)+1:]] = e.value
				attributed[e.model] = true
			}
		}
		if len(values) > 0 {
			saved, err := SaveDiscovered(profile, values, expected, false, nil, nil)
			if err != nil {
				return 0, err
			}
			if !saved {
				fmt.Fprintf(stderr, "Migration paused: profile '%s' changed. Legacy files retained; retry migration.\n", profile)
				return 1, nil
			}
		}
	}
	unresolved := 0
	for _, e := range entries {
		if !attributed[e.model] {
			unresolved++
		}
	}
	if unresolved > 0 {
		fmt.Fprintf(stderr, "%d legacy context entries have no profile attribution; retained in backup, not applied.\n", unresolved)
	}
	for _, c := range handled {
		if err := archive(c); err != nil {
			return 0, err
		}
	}
	if paths.Exists(legacy) {
		if err := archive(legacy); err != nil {
			return 0, err
		}
	}
	fmt.Fprintf(stderr, "Context migration complete: %d profile(s) checked. Originals retained as .migrated backups.\n", len(profiles))
	return 0, nil
}

// RecoverCacheBackups is explicit recovery only: old backups never become an automatic live source.
func RecoverCacheBackups(stderr io.Writer) (int, error) {
	root := cacheRoot()
	for _, conf := range globConf(StateDir()) {
		s := stem(conf)
		if paths.IsSymlink(conf) || !ValidProfileName(s) {
			continue
		}
		cache := filepath.Join(root, s, "cache/context-window.migrated")
		if !paths.IsFile(cache) || paths.IsSymlink(cache) {
			continue
		}
		text, err := readText(cache)
		if err != nil {
			return 0, err
		}
		values := map[string]int64{}
		for _, e := range cacheEntries(strings.Split(text, "\n")) {
			values[e.model] = e.value
		}
		if len(values) > 0 {
			saved, err := SaveDiscovered(s, values, ProfileIdentity(s), false, nil, nil)
			if err != nil {
				return 0, err
			}
			if !saved {
				return 1, nil
			}
		}
		fmt.Fprintf(stderr, "%s: processed %d backup entries; existing limits and backup retained.\n", s, len(values))
	}
	return 0, nil
}

// moveInlineDiscovered moves each conf's inline context_discovered line to its
// models.json and removes the retired <name>.conf.lock files. It returns how
// many profiles moved.
func moveInlineDiscovered(state string) (int, error) {
	moved := 0
	for _, conf := range globConf(state) {
		s := stem(conf)
		if paths.IsSymlink(conf) || !ValidProfileName(s) {
			continue
		}
		os.Remove(conf + ".lock")
		text, fields, err := ReadConf(conf)
		if err != nil {
			return moved, err
		}
		if !strings.Contains(text, "context_discovered=") {
			continue
		}
		err = ProfileLock(s, func() error {
			expected := ProfileIdentity(s)
			if expected == nil {
				return nil
			}
			if doc := discoveredDoc(fields); doc != nil {
				ok, err := writeDiscovered(conf, doc, expected)
				if ok {
					moved++
				}
				return err
			}
			// Bound to a route this profile no longer has: every read already ignores it.
			_, err := ReplaceFields(conf, []Field{{Key: "context_discovered"}}, expected)
			return err
		})
		if err != nil {
			return moved, err
		}
	}
	return moved, nil
}
