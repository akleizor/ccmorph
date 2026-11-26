package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util/paths"
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

// linkTargetIs reports whether the link at p spells path; a resolution would
// follow a dangling store link to nowhere, so the spelled target is compared
// (#185).
func linkTargetIs(p, path string) bool {
	t, err := os.Readlink(p)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(p), t)
	}
	return filepath.Clean(t) == filepath.Clean(path)
}

// ProfileOK reports whether the profile is fully provisioned (auth files, its
// history shared, the shared layer linked), so a launch needs no lock and no write.
func ProfileOK(profiles, name string) bool {
	if !profile.ValidName(name) {
		return false
	}
	dir := filepath.Join(profiles, name)
	canonical := filepath.Join(os.Getenv("HOME"), ".claude")
	if !paths.IsDir(dir) || paths.IsSymlink(dir) || !paths.IsFile(dir+"/config.json") || !paths.IsFile(dir+"/.claude.json") {
		return false
	}
	for _, item := range sharedDirs {
		link, src := dir+"/"+item, canonical+"/"+item
		if !paths.IsDir(src) || !paths.IsSymlink(link) || resolved(link) != resolved(src) {
			return false
		}
	}
	for _, item := range sharedFiles {
		link, src := dir+"/"+item, canonical+"/"+item
		if !paths.Exists(src) {
			continue
		}
		if !paths.Lexists(link) || (paths.IsSymlink(link) && resolved(link) != resolved(src)) {
			return false
		}
	}
	// Prompt history shares like the dirs, but a real profile-local file is
	// content to merge, not a deliberate override, so only a link at the
	// shared file passes (#177, #185: a missing shared file is not provisioned
	// either — setup creates it, and skipping the check left the history
	// per-profile forever on a home claude only ever runs through ccmorph).
	if h := canonical + "/history.jsonl"; !paths.IsSymlink(dir+"/history.jsonl") || resolved(dir+"/history.jsonl") != resolved(h) {
		return false
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
	if paths.IsSymlink(dir) || paths.IsSymlink(canonical) {
		return fail("refusing symlinked profile state for '" + name + "'")
	}
	unlock, serialized := setupSerialize(profiles, name)
	defer unlock()
	live, _ := HoldLive(profiles, name, log)
	// #185: the merge runs only while this launch is the sole holder of the
	// profile's live lock. Another launch may have registered after the pid
	// sample below, or not yet at all; the lock covers its whole session.
	exclusive := false
	if serialized && live != nil {
		lock := LiveLockPath(profiles, name)
		if syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			exclusive = true
			liveMu.Lock()
			liveEx[lock]++
			liveMu.Unlock()
			// The exclusive hold covers the whole setup, history merge included.
			// A whole-file writer's nested hold counts itself in liveEx and
			// leaves the descriptor exclusive; only the outermost release goes
			// back to the shared lock a live session holds (#938).
			defer func() {
				liveMu.Lock()
				liveEx[lock]--
				rem := liveEx[lock]
				if rem <= 0 {
					delete(liveEx, lock)
				}
				_, still := liveFDs[lock]
				liveMu.Unlock()
				if rem <= 0 && still {
					syscall.Flock(int(live.Fd()), syscall.LOCK_SH) // back to live, for claude
				}
			}()
		} else if !flockWithin(live, syscall.LOCK_SH) {
			// #205: Linux flock(2) drops the existing shared lock before it tries
			// a conversion, so a failed upgrade leaves this launch holding
			// nothing; take the shared lock back. Only a stuck exclusive holder
			// in that window defeats it, and then the launch cannot be covered.
			log("lost the live lock for '" + name + "' after a failed upgrade; shared history left unchanged")
			ReleaseLive(profiles, name)
			live = nil
		}
	}
	pids := liveSessions(dir)
	// A live session's sandbox watches the dirs its config tree resolves
	// through, and its transcript appends reopen by pathname: swapping
	// projects/ or history.jsonl under it quarantines or loses writes. Busy
	// means the launch goes ahead on the profile's own history, unchanged —
	// never a failure, never a half-migrated dir (#177).
	busy := !serialized || !exclusive || len(pids) > 0
	if serialized && !exclusive {
		log("another launch of '" + name + "' is live or starting; shared history left unchanged")
	}
	if len(pids) > 0 {
		log("profile '" + name + "' has live session(s) " + strings.Join(pidsText(pids), ",") + "; shared history left unchanged")
	}
	if !serialized {
		log("setup lock for '" + name + "' is held elsewhere; shared history left unchanged")
	}
	if os.MkdirAll(dir, 0o777) != nil {
		return fail("cannot create profile '" + name + "'")
	}
	// #185: a shared history store may sit on a volume that is not mounted —
	// ~/.claude/projects as a link to another disk. Its absence is logged and
	// the profile keeps its own history; only the rest of the shared layer is
	// required to provision.
	unreachable := map[string]bool{}
	for _, d := range sharedDirs {
		src := canonical + "/" + d
		if os.MkdirAll(src, 0o777) != nil {
			if !historyShares[d] {
				return fail("cannot create profile '" + name + "'")
			}
			unreachable[d] = true
			log("shared '" + d + "' at '" + src + "' cannot be reached; profile '" + name + "' stays on its own '" + d + "'")
		}
	}
	// Best effort: mkdir already set the mode, and a filesystem refusing chmod must
	// not brick provisioning. Only the profile's own dir: ~/.claude is Claude Code's,
	// and its mode is the user's to set.
	_ = os.Chmod(dir, 0o700)
	if err := provisionProfile(dir, kind); err != nil {
		if errors.Is(err, ErrProfileInUse) || errors.Is(err, errLiveLockUnavailable) {
			// The profile's live lock is held by a session or another launch,
			// or it cannot be made at all, so the auth files must not be
			// rewritten now: they stay as they are and the next launch that
			// can take the lock provisions them. A caller whose policy is to
			// go on without the lock (`add`, `use`) proceeds; a launch was
			// already refused before this point. No state records the skip.
			log("provisioning of '" + name + "' skipped: " + err.Error() + "; the next launch retries it")
		} else {
			fmt.Fprintln(os.Stderr, err)
			return fail("profile setup failed for '" + name + "'")
		}
	}

	for _, item := range append(append([]string{}, sharedDirs...), sharedFiles...) {
		src, link := canonical+"/"+item, dir+"/"+item
		if unreachable[item] {
			// #185: the store cannot be reached, so the profile keeps a real
			// dir of its own; a link into the missing store is replaced.
			if paths.IsSymlink(link) && linkTargetIs(link, src) {
				if busy {
					continue
				}
				if os.Remove(link) != nil {
					return fail("could not unlink '" + item + "' for '" + name + "'")
				}
			}
			if !paths.Lexists(link) && os.MkdirAll(link, 0o777) != nil {
				return fail("cannot create profile '" + name + "'")
			}
			continue
		}
		if !paths.Lexists(src) {
			continue
		}
		switch {
		case paths.IsSymlink(link):
			if resolved(link) == resolved(src) {
				continue
			}
			if !historyShares[item] {
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
		case paths.Lexists(link):
			if !paths.IsDir(link) {
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
			if ents, _ := os.ReadDir(link); len(ents) == 1 && paths.IsSymlink(nested) && resolved(nested) == resolved(src) {
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

	// #185: transcripts parked beside the profile (a v0.5.0 launch, an earlier
	// failed merge) exist nowhere else; merge them into the shared store or
	// report them, so their sessions return to /resume.
	if !busy && !unreachable["projects"] {
		mergeParks(dir, canonical+"/projects", log)
	}

	// Prompt history: the profile's records merge into the shared file,
	// chronologically, and the profile links at it (#177). Never while a
	// session lives — the file it appends to must not change under it.
	hSrc, hLink := canonical+"/history.jsonl", dir+"/history.jsonl"
	if !busy && !paths.Lexists(hSrc) {
		// #185: nothing creates the shared file on a home claude only ever runs
		// through ccmorph, and the link below is only made when it exists.
		if f, err := os.OpenFile(hSrc, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			f.Close()
		}
	}
	if !busy && (paths.IsFile(hSrc) || (paths.Lexists(hLink) && !paths.IsDir(hLink))) {
		switch {
		case paths.IsSymlink(hLink) && resolved(hLink) == resolved(hSrc):
		case paths.IsDir(hLink):
			errf("shared 'history.jsonl' collision at '" + hLink + "'; nothing moved — reconcile through its owning tool")
			return fail("could not share history for '" + name + "'")
		case !paths.Lexists(hLink):
			if os.Symlink(hSrc, hLink) != nil {
				return fail("could not share history for '" + name + "'")
			}
		case sameTarget(hLink, hSrc):
			// #185: the shared store links at this profile's own real file.
			// Merging it would delete the link's target and leave a loop, so
			// the store path becomes the real file (copied, so another disk is
			// fine) and the profile links at it. #205: the copy is verified at
			// a temp beside the store and renamed over the store's link, so the
			// shared path always names a file; a failed copy leaves both where
			// they were — logged, never a failed launch.
			tmp := hSrc + ".ccm-repair." + strconv.Itoa(os.Getpid())
			_ = os.Remove(tmp)
			if copyVerified(hLink, tmp) != nil || os.Rename(tmp, hSrc) != nil {
				_ = os.Remove(tmp)
				log("kept profile-local 'history.jsonl' for profile=" + name + ": the shared history links at it and the copy into its place did not complete; nothing moved")
				break
			}
			if os.Remove(hLink) != nil {
				log("kept profile-local 'history.jsonl' for profile=" + name + ": the shared history links at it and its link could not be removed; nothing moved")
				break
			}
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

	if paths.IsSymlink(dir + "/settings.json") {
		if os.Remove(dir+"/settings.json") != nil {
			return fail("could not remove legacy settings link for '" + name + "'")
		}
	}
	return true
}

// lockWait bounds every lock the launch path takes: a holder stuck on a
// stalled volume defers the work to the next launch instead of hanging the
// session, which INTENT promise 8 forbids (#185). Tests shorten it.
var lockWait = 2 * time.Second

// SetLockWait replaces how long the launch path waits for a lock, for tests in
// other packages; the returned func restores the previous value.
func SetLockWait(d time.Duration) (restore func()) {
	old := lockWait
	lockWait = d
	return func() { lockWait = old }
}

// flockWithin takes how on f, retrying without blocking until lockWait
// expires; false means the lock is held by someone else and the caller must
// go on without it.
func flockWithin(f *os.File, how int) bool {
	deadline := time.Now().Add(lockWait)
	for {
		if syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB) == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// liveLaunches holds one shared-lock descriptor per profile live lock path for
// the life of the process (#185). liveEx counts this process's nested exclusive
// holds of each path: SetupProfile keeps one over its whole run, and a
// whole-file writer's window nests inside it, so the descriptor stays
// exclusive until the outer hold ends (#938).
var (
	liveMu  sync.Mutex
	liveFDs = map[string]*os.File{}
	liveEx  = map[string]int{}
)

// LiveLockPath names a profile's live lock: a launch holds it shared while its
// session runs, and a setup takes it exclusive to merge (#185, #205). Every
// taker goes through HoldLive or RemoveLive, which check after their flock
// that the descriptor still names the file at the path: an rm may unlink the
// file (GH-837), and a raw open-then-flock that skipped the check could lock
// an inode the path no longer names. A symlink at the path is never the lock:
// both open it with O_NOFOLLOW, so the link rm removes is one no launch holds.
func LiveLockPath(profiles, name string) string {
	return filepath.Join(filepath.Dir(profiles), ".locks", name+".live")
}

// ReleaseLive drops the live lock this process holds for the profile, so a
// hopped-away profile does not read as live for the rest of the process's life
// (#205). Releasing one this process does not hold is a no-op.
func ReleaseLive(profiles, name string) {
	lock := LiveLockPath(profiles, name)
	liveMu.Lock()
	defer liveMu.Unlock()
	if f, ok := liveFDs[lock]; ok {
		f.Close()
		delete(liveFDs, lock)
	}
	delete(liveEx, lock)
}

// LiveFile is the descriptor this process holds for the profile's live lock,
// or nil when it holds none. A whole-file writer run in this process — the
// project picker's rewrite, the settings fold — upgrades that same descriptor
// instead of opening a second one: flock locks the open file description, so a
// fresh descriptor would wait on this process's own shared lock (#939).
func LiveFile(profiles, name string) *os.File {
	liveMu.Lock()
	defer liveMu.Unlock()
	return liveFDs[LiveLockPath(profiles, name)]
}

// RemoveLive takes a profile's live lock file away with the profile (GH-837).
// It drops this process's own hold first, then removes the file only when no
// other process holds it: a running launch's lock is its cover (#185), and
// unlinking it would let the next launch make a second lock beside the one
// still held. Locking a file the path no longer names is not a hold on the
// path, so the descriptor is checked against the path before the unlink and
// the take starts again on whatever the path names now. nil when the file is
// gone (removed now, or never there); ErrLiveHeld when another process holds
// it; the open error otherwise. A symlink at the path is removed without being
// followed: HoldLive never takes a lock through one, so it covers no launch.
// Removals take turns under one guard (liveRemovalGuardPath), so what one saw
// at the path is still there when it unlinks it (GH-837).
func RemoveLive(profiles, name string) error {
	ReleaseLive(profiles, name)
	lock := LiveLockPath(profiles, name)
	if !paths.Lexists(lock) {
		return nil // never there: nothing to remove, and nothing made to learn it
	}
	unguard, err := guardLiveRemoval(profiles)
	if err != nil {
		return err
	}
	defer unguard()
	for attempt := 0; attempt < liveLockTries; attempt++ {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
		if os.IsNotExist(err) {
			return nil // removed, or never there
		}
		if isLinkErr(err) {
			// A link, dangling or not, is never a live lock (GH-837): it goes
			// with the profile, or rm would offer it again on every run.
			if liveBeforeLinkRemove != nil {
				liveBeforeLinkRemove()
			}
			if rerr := os.Remove(lock); rerr != nil && !os.IsNotExist(rerr) {
				return rerr
			}
			return nil
		}
		if err != nil {
			return err
		}
		if liveAfterOpen != nil {
			liveAfterOpen()
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			f.Close()
			return ErrLiveHeld
		}
		if !sameLockInode(f, lock) {
			// What this descriptor locks is no longer what the path names:
			// unlinking the path would take away someone else's lock (#837).
			f.Close()
			continue
		}
		err = os.Remove(lock)
		f.Close()
		return err
	}
	return errLiveReplaced
}

// liveRemovalGuardPath names the one lock every RemoveLive holds from its first
// look at a live lock path to its unlink (GH-837). Only RemoveLive removes what
// sits at a live lock path, and a launch makes a lock only where nothing sits,
// so with removals taken one at a time what a removal saw is still there when
// it unlinks: two rm runs that both saw a link can no longer let the second
// clear it, a launch make a lock there, and the first unlink that lock. It is
// one file for the whole state dir and is never removed, since unlinking a
// held lock file is the race it closes. The leading dot keeps it apart from
// every <name>.live and <name>.lock: a profile name never starts with one.
func liveRemovalGuardPath(profiles string) string {
	return filepath.Join(filepath.Dir(profiles), ".locks", ".live-removal")
}

// guardLiveRemoval takes the removal guard, waiting for a removal under way to
// finish, and returns its release. A removal holds it only for an open, a
// non-blocking flock and an unlink, so the wait is that long.
func guardLiveRemoval(profiles string) (func(), error) {
	f, err := os.OpenFile(liveRemovalGuardPath(profiles), os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// ErrLiveHeld is why HoldLive returned no lock: another launch holds the
// profile's live lock exclusively and outlasted the wait (#205). Any other
// error means the lock file itself could not be made or opened (#217), so a
// caller's message can tell a stuck holder from that.
var ErrLiveHeld = errors.New("another launch is holding the live lock")

// liveLockTries bounds the open-lock-check loop that keeps a take and a
// removal off an inode the path no longer names (#837). Each round only meets
// a removal that ran inside its own open-and-lock window, so five is slack.
const liveLockTries = 5

// errLiveReplaced means the file at the live lock path was removed or
// replaced on every attempt to take it: the path never settled long enough to
// name one inode, and a lock on an inode the path no longer names covers
// nothing (#837).
var errLiveReplaced = errors.New("the live lock file was replaced while it was being taken")

// liveAfterOpen is called between the open of the live lock and the flock that
// locks it. Tests set it to land the unlink a concurrent rm performs in that
// window; it is nil everywhere else (#837).
var liveAfterOpen func()

// liveBeforeLinkRemove is called between RemoveLive finding a link at the lock
// path and unlinking it. Tests set it to run a launch in that window; it is nil
// everywhere else (GH-837).
var liveBeforeLinkRemove func()

// errLiveIsLink is why HoldLive takes no lock when a symlink sits at the live
// lock path: a lock taken through a link is one an rm can unlink from under
// the launch, leaving the path free for a second lock (GH-837).
var errLiveIsLink = errors.New("a symlink is there, and a live lock is never taken through one")

// isLinkErr reports whether an O_NOFOLLOW open failed because the path is a
// symlink: ELOOP on Linux and macOS.
func isLinkErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

// sameLockInode reports whether f still names what path names. A lock is only
// a lock on the profile while the path and the descriptor are one file: an rm
// that unlinks between a launch's open and its flock would otherwise leave the
// launch holding an inode no other process can see, and the next Holder would
// make and lock a second file at the same path (#837). The path is not
// followed: a link put there since the open is not the file the lock is on.
func sameLockInode(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return os.SameFile(fi, pi)
}

// HoldLive takes the profile's live lock shared and keeps it for the life of
// the process. The launcher calls it before a session starts, so the lock covers
// the session from before claude writes sessions/<pid>.json until claude exits:
// a setup that samples the pid list in that window cannot see it and must not
// merge under it. A launch that finds the lock held exclusively waits, with a
// line in the log, for that merge to finish — a merge is file I/O only and
// never a prompt, so the wait is brief. The descriptor is cached per lock path,
// so repeated calls in one process share it (an exclusive take is an upgrade of
// this same descriptor). A nil file means the lock could not be taken, and the
// returned error names why — ErrLiveHeld for a holder that outlasted the wait,
// the failure of making or opening the lock file otherwise — so the caller that
// must refuse to start the session rather than run it through the window #185
// covers (#205) can say which of the two it was (#217).
func HoldLive(profiles, name string, log func(string)) (*os.File, error) {
	lock := LiveLockPath(profiles, name)
	liveMu.Lock()
	defer liveMu.Unlock()
	if f, ok := liveFDs[lock]; ok {
		return f, nil
	}
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < liveLockTries; attempt++ {
		f, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if isLinkErr(err) {
			return nil, errLiveIsLink // never lock through a link rm may unlink (GH-837)
		}
		if err != nil {
			return nil, err
		}
		if liveAfterOpen != nil {
			liveAfterOpen()
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
			if log != nil {
				log("waiting for another launch of '" + name + "' to finish linking its shared history")
			}
			if !flockWithin(f, syscall.LOCK_SH) {
				f.Close()
				return nil, ErrLiveHeld // a stuck holder: the caller must not start a session (#205)
			}
		}
		if !sameLockInode(f, lock) {
			// A removal ran inside this open-and-lock window: the file this
			// descriptor locks is not the one the path names any more, so the
			// lock covers nothing and the take starts again (#837).
			f.Close()
			continue
		}
		liveFDs[lock] = f
		return f, nil
	}
	return nil, errLiveReplaced
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
