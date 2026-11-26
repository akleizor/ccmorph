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

// errLiveLockUnavailable is why a whole-file rewrite did not run when not even
// the live lock could be made: nothing can hold a lock at that path, so nothing
// may be written through it. A verb whose policy is to go on without the lock
// (`add`, `use`) skips the write and leaves it to the next launch that can make
// the lock; a launch is refused earlier, by the holder check (#217, GH-938).
var errLiveLockUnavailable = errors.New("the live lock could not be created")

// profileJSONRead runs inside a whole-file writer's locked window, after its
// read of path and before its write; a test puts a second writer of the same
// file there (#299, #861, GH-938).
var profileJSONRead = func(path string) {}

// holdLiveExclusive takes the profile's live lock exclusively for one
// whole-file writer's read-to-rename window and returns its release. The lock
// is restored afterwards to how this process held it: shared when a launch or
// session of this process holds it, released otherwise — a caller that starts
// no session of the profile, a hop's check, must not keep its lock past the
// call. A holder that outlasts the bounded wait (promise 8) leaves the caller's
// file untouched: the returned error wraps ErrProfileInUse and names path.
func holdLiveExclusive(profiles, name, path string) (func(), error) {
	lock := LiveLockPath(profiles, name)
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
		if errors.Is(why, ErrLiveHeld) {
			return nil, fmt.Errorf("%s: another launch is rewriting the profile's config: %w", path, ErrProfileInUse)
		}
		return nil, fmt.Errorf("%s: %w: %v", lock, errLiveLockUnavailable, why)
	}
	if !flockWithin(live, syscall.LOCK_EX) {
		// The failed conversion can leave this fd unlocked (Linux drops the
		// shared lock first), so the lock the session runs under is taken back
		// or gone; either way this rewrite waits for a caller that holds it.
		restored := flockWithin(live, syscall.LOCK_SH)
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
			// A caller that starts no session of this profile — a hop's check —
			// must not keep its lock past this call.
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
	release, err := holdLiveExclusive(profiles, name, path)
	if err != nil {
		return err
	}
	defer release()
	doc, err = loadObj(path, ordjson.New())
	if err != nil {
		return err
	}
	profileJSONRead(path)
	changed, err := edit(doc)
	if err != nil || !changed {
		return err
	}
	return save(path, doc)
}
