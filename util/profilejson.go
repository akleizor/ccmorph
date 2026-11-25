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
	lock := LiveLockPath(profiles, name)
	liveMu.Lock()
	_, held := liveFDs[lock]
	liveMu.Unlock()
	live, why := HoldLive(profiles, name, nil)
	if live == nil {
		if errors.Is(why, ErrLiveHeld) {
			return fmt.Errorf("%s: another launch is rewriting the profile's config: %w", path, ErrProfileInUse)
		}
		return errors.New(lock + ": the live lock could not be created: " + why.Error())
	}
	if !held {
		// A caller that starts no session of this profile — a hop's check —
		// must not keep its lock past this call.
		defer ReleaseLive(profiles, name)
	}
	if !flockWithin(live, syscall.LOCK_EX) {
		// The failed conversion can leave this fd unlocked (Linux drops the
		// shared lock first), so the lock the session runs under is taken back
		// or gone; either way this rewrite waits for a caller that holds it.
		if !flockWithin(live, syscall.LOCK_SH) {
			ReleaseLive(profiles, name)
		}
		return fmt.Errorf("%s: %w", path, ErrProfileInUse)
	}
	defer syscall.Flock(int(live.Fd()), syscall.LOCK_SH)
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
