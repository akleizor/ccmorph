package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/profile"
)

// The shared harness layer (skills, plugins, hooks, memory, keybindings...) belongs
// to the machine: every profile links these to ~/.claude, so behaviour cannot fork
// per profile. Dirs are linked even while empty; a real profile-local file is a
// deliberate override. Session history is shared the same way (#177): projects/,
// the paste cache and history.jsonl link into ~/.claude too, so /resume, the
// prompt history and pasted text see every profile's sessions.
var (
	sharedDirs  = []string{"skills", "hooks", "plugins", "commands", "output-styles", "agents", "session-env", "file-history", "projects", "paste-cache"}
	sharedFiles = []string{"settings.local.json", "keybindings.json", "CLAUDE.md", ".i-have-adhd-always"}
)

// mergeable stores hold content the profile owns, so a profile-local copy merges
// into the shared store; any other collision belongs to its owning tool.
var mergeable = map[string]bool{"agents": true, "projects": true, "session-env": true, "file-history": true, "paste-cache": true}

// historyShares are the shared stores a live session writes through; they are
// linked only while no session of the profile runs (#177).
var historyShares = map[string]bool{"projects": true, "paste-cache": true}

// resolved is zsh's :A: absolute, every link followed, a missing tail kept as spelled.
func resolved(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r
	}
	if d := filepath.Dir(a); d != a {
		return filepath.Join(resolved(d), filepath.Base(a))
	}
	return a
}

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

// ProfileOK reports whether the profile is fully provisioned (auth files, its
// history shared, the shared layer linked), so a launch needs no lock and no write.
func ProfileOK(profiles, name string) bool {
	if !profile.ValidName(name) {
		return false
	}
	dir := filepath.Join(profiles, name)
	canonical := filepath.Join(os.Getenv("HOME"), ".claude")
	if !isDir(dir) || isLink(dir) || !isFile(dir+"/config.json") || !isFile(dir+"/.claude.json") {
		return false
	}
	for _, item := range sharedDirs {
		link, src := dir+"/"+item, canonical+"/"+item
		if !isDir(src) || !isLink(link) || resolved(link) != resolved(src) {
			return false
		}
	}
	for _, item := range sharedFiles {
		link, src := dir+"/"+item, canonical+"/"+item
		if !exists(src) {
			continue
		}
		if !lexists(link) || (isLink(link) && resolved(link) != resolved(src)) {
			return false
		}
	}
	// Prompt history shares like the dirs, but a real profile-local file is
	// content to merge, not a deliberate override, so only a link at the
	// shared file passes (#177).
	if h := canonical + "/history.jsonl"; isFile(h) {
		if !isLink(dir+"/history.jsonl") || resolved(dir+"/history.jsonl") != resolved(h) {
			return false
		}
	}
	return true
}

// SetupProfile provisions a profile idempotently. Notes go to log, refusals to
// errf (a refusal can follow the line that explains it); false when it failed.
// The shared-store phase runs under the profile's setup lock with the busy
// check inside it (#177): a live session's dirs are never swapped under it, and
// a merge that cannot be completed leaves the profile on its own copy —
// logged, never a failed launch.
func SetupProfile(profiles, name, kind string, log, errf func(string)) bool {
	fail := func(msg string) bool { errf(msg); return false }
	if !profile.ValidName(name) {
		return false
	}
	dir := filepath.Join(profiles, name)
	canonical := filepath.Join(os.Getenv("HOME"), ".claude")
	if isLink(dir) || isLink(canonical) {
		return fail("refusing symlinked profile state for '" + name + "'")
	}
	unlock, serialized := setupSerialize(profiles, name)
	defer unlock()
	pids := liveSessions(dir)
	// A live session's sandbox watches the dirs its config tree resolves
	// through, and its transcript appends reopen by pathname: swapping
	// projects/ or history.jsonl under it quarantines or loses writes. Busy
	// means the launch goes ahead on the profile's own history, unchanged —
	// never a failure, never a half-migrated dir (#177).
	busy := !serialized || len(pids) > 0
	if len(pids) > 0 {
		log("profile '" + name + "' has live session(s) " + strings.Join(pidsText(pids), ",") + "; shared history left unchanged")
	}
	if !serialized {
		log("setup lock for '" + name + "' is held elsewhere; shared history left unchanged")
	}
	for _, d := range append([]string{dir}, prefixed(canonical, sharedDirs)...) {
		if os.MkdirAll(d, 0o777) != nil {
			return fail("cannot create profile '" + name + "'")
		}
	}
	// Best effort: mkdir already set the mode, and a filesystem refusing chmod must
	// not brick provisioning. Only the profile's own dir: ~/.claude is Claude Code's,
	// and its mode is the user's to set.
	_ = os.Chmod(dir, 0o700)
	if err := provisionProfile(dir, kind); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return fail("profile setup failed for '" + name + "'")
	}

	for _, item := range append(append([]string{}, sharedDirs...), sharedFiles...) {
		src, link := canonical+"/"+item, dir+"/"+item
		if !lexists(src) {
			continue
		}
		switch {
		case isLink(link):
			if resolved(link) == resolved(src) {
				continue
			}
			if item != "projects" {
				return fail("'" + item + "' for '" + name + "' points outside the shared store")
			}
			// #177: the profile's history lives on the user's own store,
			// another disk. Its content merges into the shared store; the disk
			// keeps every file and only the profile's link is re-pointed.
			if busy {
				continue
			}
			if !migrateProjects(link, src, log) {
				log("kept profile-local '" + item + "' for profile=" + name + ": the merge into the shared store did not complete; what it left behind stayed put")
				continue
			}
			if os.Symlink(src, link) != nil {
				return fail("could not migrate profile-local '" + item + "' for '" + name + "'")
			}
		case lexists(link):
			if !isDir(link) {
				// A real profile-local FILE (a per-profile CLAUDE.md) is a deliberate override.
				log("OVERRIDE profile-local '" + item + "' kept for profile=" + name)
				continue
			}
			if busy && mergeable[item] {
				// No swap while one of the profile's sessions lives (#177).
				continue
			}
			// Finder drops these the instant a folder opens; they must not defeat the empty-dir heal.
			_ = os.Remove(link + "/.DS_Store")
			_ = os.Remove(link + "/.localized")
			nested := link + "/" + item
			if ents, _ := os.ReadDir(link); len(ents) == 1 && isLink(nested) && resolved(nested) == resolved(src) {
				if os.Remove(nested) != nil {
					return fail("could not remove legacy nested '" + item + "' link for '" + name + "'")
				}
			}
			if os.Remove(link) == nil {
				if os.Symlink(src, link) != nil {
					return fail("could not link '" + item + "' for '" + name + "'")
				}
				continue
			}
			// A non-empty real dir where a shared link belongs: merge it into the
			// store and link. The merge copies and verifies (#177), so it works
			// with the store on another filesystem, and a failure leaves the
			// profile on its own copy — logged, never a failed launch.
			if !mergeable[item] {
				errf("shared '" + item + "' collision at '" + link + "'; nothing moved — reconcile through its owning tool")
				return fail("could not migrate profile-local '" + item + "' for '" + name + "'")
			}
			if !migrateProjects(link, src, log) {
				log("kept profile-local '" + item + "' for profile=" + name + ": the merge into the shared store did not complete; what it left behind stayed put")
				continue
			}
			if os.Symlink(src, link) != nil {
				return fail("could not migrate profile-local '" + item + "' for '" + name + "'")
			}
		default:
			if busy && historyShares[item] {
				continue
			}
			if os.Symlink(src, link) != nil {
				return fail("could not link '" + item + "' for '" + name + "'")
			}
		}
	}

	// Prompt history: the profile's records merge into the shared file,
	// chronologically, and the profile links at it (#177). Never while a
	// session lives — the file it appends to must not change under it.
	hSrc, hLink := canonical+"/history.jsonl", dir+"/history.jsonl"
	if !busy && (isFile(hSrc) || (lexists(hLink) && !isDir(hLink))) {
		switch {
		case isLink(hLink) && resolved(hLink) == resolved(hSrc):
		case isDir(hLink):
			errf("shared 'history.jsonl' collision at '" + hLink + "'; nothing moved — reconcile through its owning tool")
			return fail("could not share history for '" + name + "'")
		case !lexists(hLink):
			if os.Symlink(hSrc, hLink) != nil {
				return fail("could not share history for '" + name + "'")
			}
		default:
			if !mergeHistory(hLink, hSrc) {
				log("kept profile-local 'history.jsonl' for profile=" + name + ": the merge into the shared history did not complete; what it left behind stayed put")
				break
			}
			if os.Symlink(hSrc, hLink) != nil {
				return fail("could not share history for '" + name + "'")
			}
		}
	}

	if isLink(dir + "/settings.json") {
		if os.Remove(dir+"/settings.json") != nil {
			return fail("could not remove legacy settings link for '" + name + "'")
		}
	}
	return true
}

// setupSerialize takes the profile's setup lock — the same flock rm, set and
// config hold through model-window — so two launches cannot both merge into
// the shared store, and returns its release with whether it was taken. A verb
// already running under that lock names the profile's conf in
// CCMORPH_PROFILE_LOCK_HELD and passes straight through. The lock is taken
// without waiting: a holder prompting its user must never hang a launch, so
// contention defers the merge to the next launch instead (#177).
func setupSerialize(profiles, name string) (func(), bool) {
	conf := filepath.Join(filepath.Dir(profiles), name+".conf")
	for _, held := range strings.Split(os.Getenv("CCMORPH_PROFILE_LOCK_HELD"), ":") {
		if held == conf {
			return func() {}, true
		}
	}
	lock := filepath.Join(filepath.Dir(profiles), ".locks", name+".lock")
	if os.MkdirAll(filepath.Dir(lock), 0o700) != nil {
		return func() {}, false
	}
	f, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}

// liveSessions lists the pids of the sessions recorded under the profile's
// config dir that still run; Claude Code writes one sessions/<pid>.json per
// session. A record left behind by a crashed session can name a pid that has
// since been recycled: that reads as busy, which only defers the merge (#177).
// EPERM is an existing process of another user and counts the same way.
func liveSessions(dir string) []int {
	ents, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		return nil
	}
	var live []int
	for _, e := range ents {
		pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || pid <= 0 {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			live = append(live, pid)
		}
	}
	return live
}

func pidsText(pids []int) []string {
	out := make([]string, len(pids))
	for i, p := range pids {
		out[i] = strconv.Itoa(p)
	}
	return out
}

func prefixed(base string, items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = base + "/" + s
	}
	return out
}
