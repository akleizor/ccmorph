package util

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util/paths"
)

// ErrProfileInUse is why a rewrite of a profile's config did not run: a session
// or launch of the profile holds it, so a whole-file rewrite now would race that
// holder's own save and could lose it (#299, #861). The file is untouched.
var ErrProfileInUse = errors.New("a session or launch of the profile is open")

// errLiveLockUnavailable is why a whole-file rewrite did not run when not even
// the live lock could be made: nothing can hold a lock at that path, so nothing
// may be written through it. Only SetupProfileWithoutLive, the setup of a verb
// that goes on without the lock (`add`, `use`; #217), reads it as a skip; every
// other caller fails on it, and a launch is refused before setup by the holder
// check (GH-938).
var errLiveLockUnavailable = errors.New("the live lock could not be created")

// profileJSONRead runs inside a whole-file writer's locked window, after its
// read of path and before its write; a test puts a second writer of the same
// file there (#299, #861, GH-938).
var profileJSONRead = func(path string) {}

// LiveExclusive runs fn holding profile name's live lock exclusively, across a
// whole-file read and its rename. A process that already holds the lock shared
// — a launch, a config verb — upgrades that same descriptor for the window and
// hands it back shared; a process that holds none takes one and releases it. It
// reports false when a holder outlasted the bounded wait, leaving fn unrun and
// the file untouched, with an error wrapping ErrProfileInUse; any other error
// is the lock file that could not be made (#861, #939).
func LiveExclusive(profiles, name string, fn func()) (bool, error) {
	release, err := liveExclusiveLock(profiles, name, "")
	if release == nil {
		return false, err
	}
	defer release()
	fn()
	return true, nil
}

// liveExclusiveLock is LiveExclusive's acquisition half: its release, or nil
// when the lock could not be taken. A caller that cannot hold it must not
// rename the file it guards (#939). path, when it names the file about to be
// rewritten, is carried in the contention error so a launch's skip log names
// that file; empty names the lock instead.
func liveExclusiveLock(profiles, name, path string) (func(), error) {
	lock := paths.LiveLockPath(profiles, name)
	if path == "" {
		path = lock
	}
	liveMu.Lock()
	_, held := liveFDs[lock]
	if liveEx[lock] > 0 && held {
		// SetupProfile holds this profile's lock exclusively for its whole run
		// (#185); a writer's window nests inside it. Count the hold and leave
		// the descriptor exclusive — a downgrade here would open the history
		// merge to a second launch's setup (#938).
		liveEx[lock]++
		liveMu.Unlock()
		return func() {
			liveMu.Lock()
			liveEx[lock]--
			if liveEx[lock] <= 0 {
				delete(liveEx, lock)
			}
			liveMu.Unlock()
		}, nil
	}
	liveMu.Unlock()
	live, why := HoldLive(profiles, name, nil)
	if live == nil {
		if errors.Is(why, paths.ErrLiveHeld) {
			return nil, fmt.Errorf("%s: another launch is rewriting the profile's config: %w", path, ErrProfileInUse)
		}
		return nil, fmt.Errorf("%s: %w: %v", lock, errLiveLockUnavailable, why)
	}
	if !paths.FlockWithin(live, syscall.LOCK_EX) {
		// The failed conversion can leave this fd unlocked (Linux drops the
		// shared lock first), so the lock the session runs under is taken back
		// or gone; either way this writer waits for a caller that holds it.
		restored := paths.FlockWithin(live, syscall.LOCK_SH)
		if !held || !restored {
			ReleaseLive(profiles, name)
		}
		return nil, fmt.Errorf("%s: %w", path, ErrProfileInUse)
	}
	liveMu.Lock()
	liveEx[lock]++
	liveMu.Unlock()
	return func() {
		liveMu.Lock()
		liveEx[lock]--
		rem := liveEx[lock]
		if rem <= 0 {
			delete(liveEx, lock)
		}
		_, still := liveFDs[lock]
		liveMu.Unlock()
		if rem > 0 || !still {
			return
		}
		if !held {
			// A caller that starts no session of this profile — a hop's check,
			// a config verb — must not keep its lock past the write.
			ReleaseLive(profiles, name)
			return
		}
		syscall.Flock(int(live.Fd()), syscall.LOCK_SH)
	}, nil
}

// rewriteProfileJSON is the one read-modify-write of a JSON object file a
// session of profile name may also write — its .claude.json (ApproveKey, the
// project picker) and its settings layer (the project picker). edit changes
// the decoded document in place and reports whether it changed anything; save
// writes the changed document to path. An edit that changes nothing takes no
// lock and writes nothing. Otherwise the profile's live lock is held
// exclusively from the read to the rename: every ccmorph session holds it
// shared for its life and every whole-file writer takes it exclusively — this
// one, ApproveKey, the project picker, SeedPersonAnswers and a launch's
// provisioning — so no session, launch or second writer can write the file
// while this runs, and none of theirs is lost under it. A holder that outlasts
// the bounded wait (promise 8) leaves the file untouched and the error wraps
// ErrProfileInUse. A file that is unreadable or not a JSON object is an error,
// never guessed at.
func rewriteProfileJSON(profiles, name, path string, edit func(doc *ordjson.Obj) (bool, error), save func(path string, doc *ordjson.Obj) error) error {
	if !profile.ValidName(name) {
		return errors.New("invalid profile name: " + name)
	}
	doc, err := loadObj(path, ordjson.New())
	if err != nil {
		return err
	}
	if changed, err := edit(doc); err != nil || !changed {
		return err
	}
	var saveErr error
	ok, err := LiveExclusive(profiles, name, func() {
		doc, err := loadObj(path, ordjson.New())
		if err != nil {
			saveErr = err
			return
		}
		profileJSONRead(path)
		changed, err := edit(doc)
		if err != nil || !changed {
			saveErr = err
			return
		}
		saveErr = save(path, doc)
	})
	if !ok {
		return err
	}
	return saveErr
}
