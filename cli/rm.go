package cli

import (
	"errors"
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
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

// rm is `ccmorph rm <profile>`: conf, private state, keychain item and lock.
func (l *launcher) rm(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	// rm is the one verb that accepts the pre-#373 length: a profile made before
	// the limit can still be addressed and so must still be removable (#449).
	// Its refusal names its own, wider bound, not ValidName's (#551).
	if !profile.RemovableName(name) {
		return l.rmBadName("rm", name)
	}
	// A name that differs only by case is refused before the leftover branch:
	// on a volume that ignores letter case, profiles/GW2 is the real gw2.
	if !l.exists(name) && l.caseTwin(name) != "" {
		l.noProfile(name)
		return 1
	}
	if rc, done := l.locked(name, append([]string{"rm"}, args...)); done {
		return rc
	}
	// What rm offers is what it will remove (GH-837): a leftover from a
	// cancelled sign-in is the profile home and the live lock, never the auth
	// directory the browser logins use, so the offer names only paths on disk.
	pdir, auth := filepath.Join(l.profileDirs, name), filepath.Join(l.dir, "auth", name)
	live := util.LiveLockPath(l.profileDirs, name)
	if !l.exists(name) {
		var leftover []string
		for _, p := range []string{pdir, auth, live} {
			if paths.Lexists(p) {
				leftover = append(leftover, p)
			}
		}
		if len(leftover) == 0 {
			l.noProfile(name)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Profile '%s' has no .conf. Remove leftover private state?\n  %s\nShared conversation history will be kept.\n", name, strings.Join(leftover, "\n  "))
		if rc := uiConfirm("Remove these leftovers?"); rc != 0 {
			// Esc at a terminal is already answered by the picker's own Cancelled
			// line (rc 2), and a second line would make rm the one command that
			// answers a leave twice. Answering No, or having no terminal to show
			// the prompt on, still needs this line to say nothing was removed.
			if rc != 2 || !isTTY(0) {
				l.err("cancelled; nothing removed")
			}
			return 1
		}
	} else {
		// The removal the help page caps with "This cannot be undone" must ask
		// whoever runs rm, not only a terminal (#652): the same question the
		// leftover branch above asks, answered off a terminal by one line of
		// stdin exactly as every other [y/N] prompt is, defaulting to No like
		// the guide says (docs/guide.md). Deleting on an unanswered prompt is
		// not reachable (#724): a piped "n" is an answer of No, and a stdin
		// that closes without one names the ways through instead of removing.
		detail := "  " + l.conf(name)
		if paths.Lexists(pdir) {
			detail += "\n  " + pdir
		}
		if paths.Lexists(auth) {
			detail += "\n  " + auth
		}
		fmt.Fprintf(os.Stderr, "Remove profile '%s', its saved credential and private sign-in state?\n%s\nShared conversation history will be kept. This cannot be undone.\n", name, detail)
		if rc := uiConfirm("Remove this profile?"); rc != 0 {
			switch {
			case rc == 2 && !isTTY(0):
				// The stdin closed without an answer and no terminal was
				// there to show the prompt on: name both ways through (#724).
				l.err("no answer to confirm; nothing removed — confirm in a terminal, or pipe the answer: printf 'y' | ccmorph rm " + name)
			case rc != 2:
				// A leave (rc 2) is already answered by the picker's own
				// Cancelled line, as in the leftover branch; there is no
				// second way to reach here with rc 2 on a terminal.
				l.err("cancelled; nothing removed")
			}
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
	delete(l.confReads, name) // what was removed must not be read back (GH-561)
	util.KeychainDelete(name)
	os.Remove(filepath.Join(l.profileDirs, "..", ".locks", name+".lock"))
	// A saved approval waits for a session that may never end now (#299).
	util.DropApprovals(l.profileDirs, name)
	// The live lock goes with the profile (GH-837), unless a launch still holds
	// it: that lock is its #185 cover, so it stays and rm says so.
	if err := util.RemoveLive(l.profileDirs, name); errors.Is(err, util.ErrLiveHeld) {
		l.err("kept " + live + ": another launch of '" + name + "' still holds it — `lsof " + live + "` names the holder")
	} else if err != nil {
		l.err("kept " + live + ": " + err.Error())
	}
	for _, p := range util.BridgeFiles(filepath.Join(l.dir, "auth"), name) {
		os.Remove(p)
	}
	// The clear reads the active file's own content, not l.active(): that read
	// refuses the legacy-length name a pre-#373 default can spell, and a refused
	// read is an active file left naming a profile that no longer exists (#551).
	active, _ := l.readActiveFile()
	clearedDefault := active == name
	if clearedDefault {
		if err := os.Remove(filepath.Join(l.dir, "active")); err != nil && !os.IsNotExist(err) {
			l.err("could not clear default profile")
			return 1
		}
	}
	fmt.Println(l.accent + "Removed" + l.reset + " " + name)
	if clearedDefault {
		// The default left with the profile; the removal summary is where that
		// belongs, not two steps downstream at the next failed run (#652).
		l.err("default profile removed — set one: ccmorph use <profile>")
	}
	return 0
}

// removeProfileDir is _ccm_remove_profile_dir: history moves to native first.
func (l *launcher) removeProfileDir(name string) bool {
	p := filepath.Join(l.profileDirs, name)
	projects := filepath.Join(p, "projects")
	if fi, err := os.Lstat(projects); err == nil && fi.IsDir() && !paths.IsSymlink(p) {
		if !keepHistory(projects, filepath.Join(os.Getenv("HOME"), ".claude", "projects")) {
			l.err("kept '" + name + "': a session in its history collides with native history")
			return false
		}
	}
	// Transcripts parked beside projects — the divergent duplicates the shared
	// merge kept (#177) and v0.5.0's backups share the premigration name — exist nowhere
	// else; they move to native history like any other, and a collision keeps
	// the profile the same way.
	parked := []string{}
	for _, pat := range []string{"projects.premigration.*"} {
		// #185: a state or home path holding a glob metacharacter made the
		// pattern malformed; the dropped error skipped the rescue and the
		// RemoveAll below deleted the parked transcripts.
		hits, err := filepath.Glob(filepath.Join(globQuote(p), pat))
		if err != nil {
			l.err("kept '" + name + "': could not look for its parked transcripts: " + err.Error())
			return false
		}
		parked = append(parked, hits...)
	}
	for _, d := range parked {
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
			if !keepHistory(d, filepath.Join(os.Getenv("HOME"), ".claude", "projects")) {
				l.err("kept '" + name + "': a session in its history collides with native history")
				return false
			}
		}
	}
	if paths.Lexists(p) && os.RemoveAll(p) != nil {
		return false
	}
	if auth, ok := l.authDirQuiet(name); ok {
		if fi, err := os.Stat(auth); err == nil && fi.IsDir() {
			os.RemoveAll(auth)
		}
	} else {
		l.err("auth directory for '" + name + "' is a symlink, refusing: " + filepath.Join(l.dir, "auth", name))
	}
	return !paths.Lexists(p)
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
	gives := func(dest, f string) bool {
		return paths.IsSymlink(dest) && (paths.SameFile(dest, f) || !paths.Exists(dest))
	}
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
		if paths.Lexists(dest) {
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

// moveBridgeFiles moves a profile's bridge state with the auth directory; a
// leftover under the new name is removed rather than joined. False, with every
// move undone, when a rename fails.
func moveBridgeFiles(root, from, to string) bool {
	src, dst := util.BridgeFiles(root, from), util.BridgeFiles(root, to)
	for i := range src {
		if !paths.Lexists(src[i]) {
			os.Remove(dst[i])
			continue
		}
		if os.Rename(src[i], dst[i]) != nil {
			for j := 0; j < i; j++ {
				if paths.Lexists(dst[j]) {
					os.Rename(dst[j], src[j])
				}
			}
			return false
		}
	}
	return true
}

// mv is `ccmorph mv <old> <new>`, rolled back whole on any failure.
func (l *launcher) mv(args []string) int {
	if len(args) < 2 {
		l.err("usage: " + usageLine("mv"))
		return 1
	}
	for _, n := range args[:2] {
		if !profile.ValidName(n) {
			return l.badName("mv", n)
		}
	}
	from, to := args[0], args[1]
	// A volume that ignores letter case cannot hold both names while the rename
	// runs, so a case-only rename is refused before any lock or write.
	if from != to && strings.EqualFold(from, to) {
		l.err("'" + to + "' differs from '" + from + "' only by letter case; a rename cannot change only the case. Choose another name.")
		return 1
	}
	if rc, done := l.locked(from+","+to, append([]string{"mv"}, args...)); done {
		return rc
	}
	if !l.exists(from) {
		l.noProfile(from)
		return 1
	}
	if l.exists(to) {
		l.err("'" + to + "' already exists")
		return 1
	}
	if twin := l.caseTwin(to); twin != "" {
		l.err(caseClashMsg(to, twin))
		return 1
	}
	oldP, newP := filepath.Join(l.profileDirs, from), filepath.Join(l.profileDirs, to)
	if paths.Lexists(newP) {
		l.err("profile directory '" + to + "' already exists — remove it first")
		return 1
	}
	movedP := false
	if paths.Lexists(oldP) {
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
		if paths.Lexists(newA) {
			l.err("auth directory for '" + to + "' already exists: " + newA + " — remove it first")
			ok = false
		} else if ok = os.Rename(oldA, newA) == nil; ok {
			movedA = true
		}
	}
	authRoot := filepath.Join(l.dir, "auth")
	movedB := false
	if ok {
		if ok = moveBridgeFiles(authRoot, from, to); ok {
			movedB = true
		}
	}
	if ok && l.active() == from {
		ok = atomicWrite(filepath.Join(l.dir, "active"), to+"\n") == nil
	}
	if !ok {
		if paths.Lexists(l.conf(to)) {
			os.Rename(l.conf(to), l.conf(from))
		}
		if movedB {
			moveBridgeFiles(authRoot, to, from)
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
	if util.KeychainRename(l.dir, from, to) != nil {
		l.err("renamed, but '" + to + "' still reads its credential from the keychain item of '" + from + "'")
	}
	// The rename moved the conf and the keychain move may have rewritten it:
	// a read of either name must see what mv left (GH-561).
	delete(l.confReads, from)
	delete(l.confReads, to)
	fmt.Println(l.accent + "Renamed" + l.reset + " " + l.bold + from + l.reset + " → " + l.bold + to + l.reset)
	return 0
}
