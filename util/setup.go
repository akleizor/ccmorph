package util

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/profile"
)

// The shared harness layer (skills, plugins, hooks, memory, keybindings...) belongs
// to the machine: every profile links these to ~/.claude, so behaviour cannot fork
// per profile. Dirs are linked even while empty; a real profile-local file is a
// deliberate override.
var (
	sharedDirs  = []string{"skills", "hooks", "plugins", "commands", "output-styles", "agents", "session-env", "file-history"}
	sharedFiles = []string{"settings.local.json", "keybindings.json", "CLAUDE.md", ".i-have-adhd-always"}
)

// mergeable stores hold content the profile owns, so a profile-local copy merges
// into the shared store; any other collision belongs to its owning tool.
var mergeable = map[string]bool{"agents": true, "projects": true, "session-env": true, "file-history": true}

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

// profileOK is `util profile-ok <profiles dir> <name>`: 0 when the profile is fully
// provisioned (auth files, its own history, the shared layer linked), so a launch
// needs no lock and no write.
func profileOK(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 2 || !dirOK(args[0], args[1]) {
		return 1
	}
	return 0
}

func dirOK(profiles, name string) bool {
	if !profile.ValidName(name) {
		return false
	}
	dir := filepath.Join(profiles, name)
	canonical := filepath.Join(os.Getenv("HOME"), ".claude")
	if !isDir(dir) || isLink(dir) || !isFile(dir+"/config.json") || !isFile(dir+"/.claude.json") {
		return false
	}
	if !isDir(dir+"/projects") || isLink(dir+"/projects") {
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
	return true
}

// setupProfile is `util setup-profile <profiles dir> <name> <type>`: provision a
// profile idempotently. Errors print to stdout as `err <msg>` and notes as
// `log <msg>`, so the shell reports them through its own logger.
func setupProfile(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) != 3 {
		return 2
	}
	profiles, name, kind := args[0], args[1], args[2]
	fail := func(msg string) int { fmt.Fprintln(out, "err "+msg); return 1 }
	if !profile.ValidName(name) {
		return 1
	}
	dir := filepath.Join(profiles, name)
	canonical := filepath.Join(os.Getenv("HOME"), ".claude")
	if isLink(dir) || isLink(canonical) {
		return fail("refusing symlinked profile state for '" + name + "'")
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
	if provisionProfile([]string{dir, kind}, in, out, errw) != 0 {
		return fail("profile setup failed for '" + name + "'")
	}

	for _, item := range append(append([]string{}, sharedDirs...), sharedFiles...) {
		src, link := canonical+"/"+item, dir+"/"+item
		if !lexists(src) {
			continue
		}
		switch {
		case isLink(link):
			if resolved(link) != resolved(src) {
				return fail("'" + item + "' for '" + name + "' points outside the shared store")
			}
		case lexists(link):
			if !isDir(link) {
				// A real profile-local FILE (a per-profile CLAUDE.md) is a deliberate override.
				fmt.Fprintln(out, "log OVERRIDE profile-local '"+item+"' kept for profile="+name)
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
			// store (rename keeps files open in live sessions valid) and link.
			if !mergeable[item] {
				fmt.Fprintln(out, "err shared '"+item+"' collision at '"+link+"'; nothing moved — reconcile through its owning tool")
				return fail("could not migrate profile-local '" + item + "' for '" + name + "'")
			}
			if migrateProjects([]string{link, src}, in, out, errw) != 0 || os.Symlink(src, link) != nil {
				return fail("could not migrate profile-local '" + item + "' for '" + name + "'")
			}
		default:
			if os.Symlink(src, link) != nil {
				return fail("could not link '" + item + "' for '" + name + "'")
			}
		}
	}

	// History is the profile's own: drop only a shared-history link, never its target.
	if isLink(dir + "/projects") {
		if os.Remove(dir+"/projects") != nil {
			return fail("could not unlink shared history for '" + name + "'")
		}
	}
	if os.MkdirAll(dir+"/projects", 0o700) != nil {
		return fail("could not create history for '" + name + "'")
	}
	_ = os.Chmod(dir+"/projects", 0o700)
	if isLink(dir + "/settings.json") {
		if os.Remove(dir+"/settings.json") != nil {
			return fail("could not remove legacy settings link for '" + name + "'")
		}
	}
	return 0
}

func prefixed(base string, items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = base + "/" + s
	}
	return out
}
