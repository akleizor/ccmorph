package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

// locked re-runs this verb under the profile's context lock unless already
// inside it, as the zsh did through `model-window --locked`.
func (l *launcher) locked(key string, args []string) (int, bool) {
	if os.Getenv("CCMORPH_CONTEXT_LOCKED") == key {
		return 0, false
	}
	core := filepath.Join(l.libexec, "ccmorph-core")
	cmd := exec.Command(core, append([]string{"model-window", "--locked", key, core, "cli"}, args...)...)
	cmd.Env = append(append(os.Environ(), l.handoff()...), "CCMORPH_STATE_DIR="+l.dir, "CCMORPH_CONTEXT_LOCKED="+key)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return runRC(cmd), true
}

func present(p string) bool { _, err := os.Lstat(p); return err == nil }

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// rm is `ccmorph rm <profile>`: conf, private state, keychain item and lock.
func (l *launcher) rm(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if !profile.ValidName(name) {
		l.err("usage: ccmorph rm <name>")
		return 1
	}
	if rc, done := l.locked(name, append([]string{"rm"}, args...)); done {
		return rc
	}
	pdir, auth := filepath.Join(l.profileDirs, name), filepath.Join(l.dir, "auth", name)
	if !l.exists(name) {
		if !present(pdir) && !present(auth) {
			l.err("no profile '" + name + "'")
			return 1
		}
		fmt.Fprintf(os.Stderr, "Profile '%s' has no .conf. Remove leftover private state?\n  %s\n  %s\nShared conversation history will be kept.\n", name, pdir, auth)
		if uiConfirm("Remove these leftovers?") != 0 {
			l.err("cancelled; nothing removed")
			return 1
		}
	}
	if !l.removeProfileDir(name) {
		l.err("could not remove credential directory for '" + name + "'")
		return 1
	}
	if err := os.Remove(l.conf(name)); err != nil && !os.IsNotExist(err) {
		l.err("could not remove profile '" + name + "'")
		return 1
	}
	l.core(nil, "", "util", "keychain-delete", name)
	os.Remove(filepath.Join(l.profileDirs, "..", ".locks", name+".lock"))
	if l.active() == name {
		if err := os.Remove(filepath.Join(l.dir, "active")); err != nil && !os.IsNotExist(err) {
			l.err("could not clear default profile")
			return 1
		}
	}
	fmt.Println(l.accent + "Removed" + l.reset + " " + name)
	return 0
}

// removeProfileDir is _ccm_remove_profile_dir: history moves to native first.
func (l *launcher) removeProfileDir(name string) bool {
	p := filepath.Join(l.profileDirs, name)
	projects := filepath.Join(p, "projects")
	if fi, err := os.Lstat(projects); err == nil && fi.IsDir() && !isLink(p) {
		if !keepHistory(projects, filepath.Join(os.Getenv("HOME"), ".claude", "projects")) {
			l.err("kept '" + name + "': a session in its history collides with native history")
			return false
		}
	}
	if present(p) && os.RemoveAll(p) != nil {
		return false
	}
	if auth, ok := l.authDirQuiet(name); ok {
		if fi, err := os.Stat(auth); err == nil && fi.IsDir() {
			os.RemoveAll(auth)
		}
	} else {
		l.err("auth directory for '" + name + "' is a symlink, refusing: " + filepath.Join(l.dir, "auth", name))
	}
	return !present(p)
}

// keepHistory is _ccm_keep_history: regular files move into native history; a
// native link to the same file (or a dangling one) gives way, and a linked
// parent dir becomes a real one first (#466). False while any file stayed.
func keepHistory(from, to string) bool {
	var dirs, files []string
	filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == from {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		} else if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	gives := func(dest, f string) bool { return isLink(dest) && (sameFile(dest, f) || !fileExists(dest)) }
	for _, f := range dirs {
		dest := filepath.Join(to, strings.TrimPrefix(f, from+"/"))
		if gives(dest, f) && os.Remove(dest) == nil {
			os.MkdirAll(dest, 0o755)
		}
	}
	for _, f := range files {
		dest := filepath.Join(to, strings.TrimPrefix(f, from+"/"))
		if gives(dest, f) {
			os.Remove(dest)
		}
		if present(dest) {
			continue
		}
		if os.MkdirAll(filepath.Dir(dest), 0o755) == nil {
			if os.Rename(f, dest) != nil {
				exec.Command("mv", f, dest).Run() // across volumes
			}
		}
	}
	left := false
	filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			left = true
			return fs.SkipAll
		}
		return nil
	})
	return !left
}

// mv is `ccmorph mv <old> <new>`, rolled back whole on any failure.
func (l *launcher) mv(args []string) int {
	if len(args) < 2 || !profile.ValidName(args[0]) || !profile.ValidName(args[1]) {
		l.err("usage: ccmorph mv <old> <new>")
		return 1
	}
	from, to := args[0], args[1]
	if rc, done := l.locked(from+","+to, append([]string{"mv"}, args...)); done {
		return rc
	}
	if !l.exists(from) {
		l.err("no profile '" + from + "'")
		return 1
	}
	if l.exists(to) {
		l.err("'" + to + "' already exists")
		return 1
	}
	oldP, newP := filepath.Join(l.profileDirs, from), filepath.Join(l.profileDirs, to)
	if present(newP) {
		l.err("profile directory '" + to + "' already exists — remove it first")
		return 1
	}
	movedP := false
	if present(oldP) {
		if os.Rename(oldP, newP) != nil {
			l.err("could not rename profile directory")
			return 1
		}
		movedP = true
	}
	ok := os.Rename(l.conf(from), l.conf(to)) == nil
	oldA, newA := filepath.Join(l.dir, "auth", from), filepath.Join(l.dir, "auth", to)
	movedA := false
	if fi, err := os.Stat(oldA); ok && err == nil && fi.IsDir() {
		if present(newA) {
			l.err("auth directory for '" + to + "' already exists: " + newA + " — remove it first")
			ok = false
		} else if ok = os.Rename(oldA, newA) == nil; ok {
			movedA = true
		}
	}
	if ok && l.active() == from {
		ok = atomicWrite(filepath.Join(l.dir, "active"), to+"\n") == nil
	}
	if !ok {
		if present(l.conf(to)) {
			os.Rename(l.conf(to), l.conf(from))
		}
		if movedA {
			os.Rename(newA, oldA)
		}
		if movedP {
			os.Rename(newP, oldP)
		}
		l.err("could not rename profile")
		return 1
	}
	if _, _, rc := l.core(nil, "", "util", "keychain-rename", l.dir, from, to); rc != 0 {
		l.err("renamed, but '" + to + "' still reads its credential from the keychain item of '" + from + "'")
	}
	fmt.Println(l.accent + "Renamed" + l.reset + " " + l.bold + from + l.reset + " → " + l.bold + to + l.reset)
	return 0
}
