package ctxwindow

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/pycompat"
)

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// globConf lists state/*.conf (sorted), as paths.
func globConf(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".conf") {
			out = append(out, pycompat.Join(dir, e.Name()))
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
	legacy := pycompat.Join(state, "model-registry")
	if !exists(legacy) && exists(pycompat.Join(state, "model-registry.conf")) {
		legacy = pycompat.Join(state, "model-registry.conf")
	}
	return legacy
}

func cacheRoot() string {
	root := pycompat.Join(StateDir(), "profiles")
	if !exists(root) {
		home, _ := os.UserHomeDir()
		root = pycompat.Join(home, ".claude-profiles") // pre-migration cache recovery
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

func archive(path string) {
	backup := path + ".migrated"
	for n := 1; exists(backup); n++ {
		backup = fmt.Sprintf("%s.migrated.%d", path, n)
	}
	if err := os.Rename(path, backup); err != nil {
		pycompat.RaiseOS(err)
	}
}

func readLegacy(legacy string) []cacheEntry {
	if isSymlink(legacy) {
		pycompat.RaiseOSMsg("legacy registry is a symlink; migration refused")
	}
	var out []cacheEntry
	for _, line := range pycompat.SplitLines(pycompat.ReadText(legacy)) {
		key, value, ok := strings.Cut(pycompat.Strip(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		if n, ok := Positive(value); ok {
			out = append(out, cacheEntry{key, n})
		}
	}
	return out
}

// MigrationPreview is `--migrate --check`: what a migration would attribute.
func MigrationPreview() string {
	state := StateDir()
	legacy := legacyPath(state)
	type counts struct{ scoped, cached int }
	profiles := map[string]*counts{}
	var names []string
	for _, p := range globConf(state) {
		if s := stem(p); !isSymlink(p) && ValidProfileName(s) {
			profiles[s] = &counts{}
			names = append(names, s)
		}
	}
	attributed := map[string]bool{}
	root := cacheRoot()
	for _, profile := range names {
		cache := pycompat.Join(root, profile, "cache/context-window")
		if isFile(cache) && !isSymlink(cache) {
			for _, e := range cacheEntries(pycompat.SplitLines(pycompat.ReadText(cache))) {
				profiles[profile].cached++
				attributed[e.model] = true
			}
		}
	}
	unresolved := 0
	if exists(legacy) {
		for _, e := range readLegacy(legacy) {
			scope, _, _ := strings.Cut(e.model, ":")
			if _, ok := profiles[scope]; ok && strings.HasPrefix(e.model, scope+":") {
				profiles[scope].scoped++
			} else if !attributed[e.model] {
				unresolved++
			}
		}
	}
	out := pycompat.NewObj()
	out.Set("source", legacy)
	ps := pycompat.NewObj()
	for _, n := range names {
		c := pycompat.NewObj()
		c.Set("scoped", pycompat.Int(int64(profiles[n].scoped)))
		c.Set("cached", pycompat.Int(int64(profiles[n].cached)))
		ps.Set(n, c)
	}
	out.Set("profiles", ps)
	out.Set("unattributed_entries", pycompat.Int(int64(unresolved)))
	return pycompat.Marshal(out, pycompat.Format{SortKeys: true, ASCII: true})
}

// Migrate folds the legacy registry and per-profile caches into each profile's conf.
func Migrate(stderr io.Writer) int {
	state := StateDir()
	if fi, err := os.Stat(state); err != nil || !fi.IsDir() {
		return 0
	}
	lock, err := os.OpenFile(pycompat.Join(state, ".context-migration.lock"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		pycompat.RaiseOS(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		pycompat.RaiseOS(err)
	}
	return migrateLocked(state, stderr)
}

func migrateLocked(state string, stderr io.Writer) int {
	legacy := legacyPath(state)
	root := cacheRoot()
	var caches []string
	if dirs, err := os.ReadDir(root); err == nil {
		for _, d := range dirs {
			c := pycompat.Join(root, d.Name(), "cache/context-window")
			if _, err := os.Lstat(c); err == nil {
				caches = append(caches, c)
			}
		}
	}
	if !exists(legacy) && len(caches) == 0 {
		return 0
	}
	var entries []cacheEntry
	if exists(legacy) {
		entries = readLegacy(legacy)
	}
	var profiles []string
	for _, conf := range globConf(state) {
		if conf[strings.LastIndex(conf, "/")+1:] == "model-registry.conf" || isSymlink(conf) || !ValidProfileName(stem(conf)) {
			continue
		}
		_, fields := ReadConf(conf)
		switch fields["type"] {
		case "apikey", "proxy", "provider", "token", "sub":
			profiles = append(profiles, stem(conf))
		}
	}
	var handled []string
	attributed := map[string]bool{}
	for _, c := range caches {
		if isSymlink(c) {
			pycompat.RaiseOSMsg("legacy context cache is a symlink; migration refused: %s", c)
		}
	}
	for _, profile := range profiles {
		expected := ProfileIdentity(profile)
		cache := pycompat.Join(root, profile, "cache/context-window")
		values := pycompat.NewOMap[int64]()
		if isFile(cache) && !isSymlink(cache) {
			for _, e := range cacheEntries(pycompat.SplitLines(pycompat.ReadText(cache))) {
				values.Set(e.model, e.value)
				attributed[e.model] = true
			}
			handled = append(handled, cache)
		}
		// A legacy bare row has no route provenance. Only the profile's own cache
		// can attribute it; scoped rows supersede that profile-local fallback.
		for _, e := range entries {
			if strings.HasPrefix(e.model, profile+":") {
				values.Set(e.model[len(profile)+1:], e.value)
				attributed[e.model] = true
			}
		}
		if values.Len() > 0 && !SaveDiscovered(profile, values, expected, false, nil, nil) {
			fmt.Fprintf(stderr, "Migration paused: profile '%s' changed. Legacy files retained; retry migration.\n", profile)
			return 1
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
		archive(c)
	}
	if exists(legacy) {
		archive(legacy)
	}
	fmt.Fprintf(stderr, "Context migration complete: %d profile(s) checked. Originals retained as .migrated backups.\n", len(profiles))
	return 0
}

// RecoverCacheBackups is explicit recovery only: old backups never become an automatic live source.
func RecoverCacheBackups(stderr io.Writer) int {
	root := cacheRoot()
	for _, conf := range globConf(StateDir()) {
		s := stem(conf)
		if isSymlink(conf) || !ValidProfileName(s) {
			continue
		}
		cache := pycompat.Join(root, s, "cache/context-window.migrated")
		if !isFile(cache) || isSymlink(cache) {
			continue
		}
		values := pycompat.NewOMap[int64]()
		for _, e := range cacheEntries(strings.Split(pycompat.ReadText(cache), "\n")) {
			values.Set(e.model, e.value)
		}
		if values.Len() > 0 && !SaveDiscovered(s, values, ProfileIdentity(s), false, nil, nil) {
			return 1
		}
		fmt.Fprintf(stderr, "%s: processed %d backup entries; existing limits and backup retained.\n", s, values.Len())
	}
	return 0
}
