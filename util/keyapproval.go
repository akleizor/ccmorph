package util

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/profile"
)

// Claude Code asks "Do you want to use this API key?" on a profile's first
// launch and stores the answer in the profile config dir's .claude.json under
// customApiKeyResponses. It records the key's last 20 characters, never the
// key itself (the shape the issue's evidence shows: a rejected entry
// "ateway-test-key-0001" for a 21-character key).
const keyTailLen = 20

// keyTail is the form Claude Code compares a key against its stored answers in.
func keyTail(key string) string {
	if len(key) > keyTailLen {
		return key[len(key)-keyTailLen:]
	}
	return key
}

// KeyState is Claude Code's stored answer for one profile's API key.
type KeyState int

const (
	// KeyUnanswered: no answer is stored (or the file cannot be read). Claude
	// Code would ask the question, with No highlighted.
	KeyUnanswered KeyState = iota
	// KeyApproved: Claude Code will use the key without asking.
	KeyApproved
	// KeyRefused: Claude Code was told No and opens its login menu instead.
	KeyRefused
)

func stringList(o *ordjson.Obj, k string) ([]any, bool) {
	v, ok := o.Get(k)
	if !ok {
		return nil, true
	}
	if v == nil {
		return nil, true
	}
	list, ok := v.([]any)
	return list, ok
}

func hasString(list []any, s string) bool {
	for _, e := range list {
		if v, ok := e.(string); ok && v == s {
			return true
		}
	}
	return false
}

// keyResponses returns the customApiKeyResponses object of a decoded
// .claude.json, or nil when it is absent or not an object.
func keyResponses(doc *ordjson.Obj) *ordjson.Obj {
	if doc == nil {
		return nil
	}
	v, ok := doc.Get("customApiKeyResponses")
	if !ok {
		return nil
	}
	return asObj(v)
}

// KeyResponse reads the answer Claude Code stored for key in the profile home's
// .claude.json. It never writes, so doctor can call it.
func KeyResponse(home, key string) KeyState {
	path := filepath.Join(home, ".claude.json")
	if isSymlink(path) {
		return KeyUnanswered
	}
	doc, err := loadObj(path, nil)
	if err != nil || doc == nil {
		return KeyUnanswered
	}
	responses := keyResponses(doc)
	if responses == nil {
		return KeyUnanswered
	}
	tail := keyTail(key)
	approved, okA := stringList(responses, "approved")
	rejected, okR := stringList(responses, "rejected")
	// A refused key wins: it is the answer that sends Claude Code to its login menu.
	if okR && hasString(rejected, tail) {
		return KeyRefused
	}
	if okA && hasString(approved, tail) {
		return KeyApproved
	}
	return KeyUnanswered
}

// approveKeyRead runs between ApproveKey's read of .claude.json and its write;
// a test puts a second writer of the same file there (#299).
var approveKeyRead = func() {}

// ApproveKey records key as approved in the profile's .claude.json, so Claude
// Code never asks the first-launch question. An earlier refusal for the same
// key is cleared. Every other key in the file keeps its place and value, and
// the file is rewritten only when the answer actually changes. Because the
// rewrite is a whole-file swap that a running session or SeedPersonAnswers may
// race, it runs only while this process holds the profile's live lock
// exclusively; while another holder has it the call changes nothing and names
// the running session. A symlinked or unparsable file, or a
// customApiKeyResponses that is not an object, is an error: the caller must not
// guess and clobber what Claude Code wrote.
func ApproveKey(profiles, name, key string) error {
	if !profile.ValidName(name) {
		return errors.New("invalid profile name: " + name)
	}
	path := filepath.Join(profiles, name, ".claude.json")
	if isSymlink(path) {
		return errors.New(path + ": symbolic links are not allowed")
	}
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return errors.New(path + ": not a regular file")
	}
	// This is a whole-file read-modify-write of a file the profile's live
	// sessions and SeedPersonAnswers also write, so it runs only while this
	// process holds the profile's live lock exclusively — the lock every writer
	// of the file takes (#299). Two approvals serialize; a running session or
	// another launch keeps the file to itself and this call changes nothing.
	lock := LiveLockPath(profiles, name)
	liveMu.Lock()
	_, held := liveFDs[lock]
	liveMu.Unlock()
	live, why := HoldLive(profiles, name, nil)
	if live == nil {
		if errors.Is(why, ErrLiveHeld) {
			return errors.New(path + ": a running session or launch of '" + name + "' is using it; try again once it ends")
		}
		return errors.New(lock + ": the live lock could not be created: " + why.Error())
	}
	if !held {
		// A caller that starts no session of this profile — a hop's check —
		// must not keep its lock past this call.
		defer ReleaseLive(profiles, name)
	}
	if syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		// The failed conversion can leave this fd unlocked (Linux drops the
		// shared lock first), so take the lock back or drop it before refusing.
		if !flockWithin(live, syscall.LOCK_SH) {
			ReleaseLive(profiles, name)
		}
		return errors.New(path + ": a running session or launch of '" + name + "' is using it; try again once it ends")
	}
	defer syscall.Flock(int(live.Fd()), syscall.LOCK_SH)
	doc, err := loadObj(path, ordjson.New())
	if err != nil {
		return err
	}
	approveKeyRead()
	responses := keyResponses(doc)
	created := false
	if responses == nil {
		if _, ok := doc.Get("customApiKeyResponses"); ok {
			if v, _ := doc.Get("customApiKeyResponses"); v != nil {
				return errors.New(path + ": customApiKeyResponses is not a JSON object")
			}
		}
		responses = ordjson.New()
		doc.Set("customApiKeyResponses", responses)
		created = true
	}
	tail := keyTail(key)
	approved, okA := stringList(responses, "approved")
	if !okA {
		return errors.New(path + ": customApiKeyResponses.approved is not a JSON array")
	}
	rejected, okR := stringList(responses, "rejected")
	if !okR {
		return errors.New(path + ": customApiKeyResponses.rejected is not a JSON array")
	}
	changed := created
	if !hasString(approved, tail) {
		responses.Set("approved", append(append([]any{}, approved...), tail))
		changed = true
	}
	if hasString(rejected, tail) {
		kept := make([]any, 0, len(rejected))
		for _, e := range rejected {
			if v, ok := e.(string); ok && v == tail {
				continue
			}
			kept = append(kept, e)
		}
		responses.Set("rejected", kept)
		changed = true
	}
	if !responses.Has("rejected") {
		responses.Set("rejected", []any{})
		changed = true
	}
	if !changed {
		return nil
	}
	return atomicJSON(path, doc)
}
