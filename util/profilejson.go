package util

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/profile"
)

// ErrProfileInUse is why a rewrite of a profile's config did not run: a session
// or launch of the profile holds it, so a whole-file rewrite now would race that
// holder's own save and could lose it (#299, #861). The file is untouched.
var ErrProfileInUse = errors.New("a session or launch of the profile is open")

// profileJSONRead runs between rewriteProfileJSON's locked read of path and its
// write; a test puts a second writer of the same file there (#299, #861).
var profileJSONRead = func(path string) {}

// LiveExclusive runs fn holding profile name's live lock exclusively, across a
// whole-file read and its rename. A process that already holds the lock shared
// — a launch, a config verb — upgrades that same descriptor for the window and
// hands it back shared; a process that holds none takes one and releases it. It
// reports false when a holder outlasted the bounded wait, leaving fn unrun and
// the file untouched, with an error wrapping ErrProfileInUse; any other error
// is the lock file that could not be made (#861, #939).
func LiveExclusive(profiles, name string, fn func()) (bool, error) {
	release, err := liveExclusiveLock(profiles, name)
	if release == nil {
		return false, err
	}
	defer release()
	fn()
	return true, nil
}

// liveExclusiveLock is LiveExclusive's acquisition half: its release, or nil
// when the lock could not be taken. A caller that cannot hold it must not
// rename the file it guards (#939).
func liveExclusiveLock(profiles, name string) (func(), error) {
	lock := LiveLockPath(profiles, name)
	liveMu.Lock()
	_, held := liveFDs[lock]
	liveMu.Unlock()
	live, why := HoldLive(profiles, name, nil)
	if live == nil {
		if errors.Is(why, ErrLiveHeld) {
			return nil, fmt.Errorf("%s: another launch is rewriting the profile's config: %w", lock, ErrProfileInUse)
		}
		return nil, errors.New(lock + ": the live lock could not be created: " + why.Error())
	}
	if !flockWithin(live, syscall.LOCK_EX) {
		// The failed conversion can leave this fd unlocked (Linux drops the
		// shared lock first), so the lock the session runs under is taken back
		// or gone; either way this writer waits for a caller that holds it.
		if !flockWithin(live, syscall.LOCK_SH) {
			ReleaseLive(profiles, name)
		}
		return nil, fmt.Errorf("%s: %w", lock, ErrProfileInUse)
	}
	return func() {
		syscall.Flock(int(live.Fd()), syscall.LOCK_SH)
		if !held {
			// A caller that starts no session of this profile — a hop's check,
			// a config verb — must not keep its lock past the write.
			ReleaseLive(profiles, name)
		}
	}, nil
}

// rewriteProfileJSON is the one read-modify-write of a JSON object file a
// session of profile name may also write — its .claude.json (ApproveKey, the
// project picker) and its settings layer (the project picker). edit changes
// the decoded document in place and reports whether it changed anything; save
// writes the changed document to path. An edit that changes nothing takes no
// lock and writes nothing. Otherwise the profile's live lock is held
// exclusively from the read to the rename: every ccmorph session holds it
// shared for its life and every whole-file writer takes it exclusively, so no
// session, launch or second writer can write the file while this runs, and
// none of theirs is lost under it. A holder that outlasts the bounded wait
// (promise 8) leaves the file untouched and the error wraps ErrProfileInUse.
// A file that is unreadable or not a JSON object is an error, never guessed at.
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
