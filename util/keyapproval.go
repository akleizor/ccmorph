package util

import (
	"errors"
	"os"
	"path/filepath"

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
// rewrite is a whole-file swap that a running session, SeedPersonAnswers or a
// launch's merge may race, it runs while this process holds the profile's live
// lock shared — the mode a running session holds, so an open session never
// stops the approval being recorded, and the exclusive whole-file writers wait
// for it. A symlinked or unparsable file, or a customApiKeyResponses that is
// not an object, is an error: the caller must not guess and clobber what
// Claude Code wrote.
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
	// sessions and SeedPersonAnswers also write. It holds the profile's live
	// lock shared across the read and the rename — the mode a running session
	// holds — so the repo's writers that replace the whole file, which take it
	// exclusively (seeding a profile, a launch's merge), cannot swap it under
	// the approval. A running session must not block the approval: an approval
	// dropped because a session is open is exactly the loss this guards
	// against (#299). An exclusive holder is waited for, bounded by lockWait,
	// never outlasted (promise 8).
	lock := LiveLockPath(profiles, name)
	liveMu.Lock()
	_, held := liveFDs[lock]
	liveMu.Unlock()
	live, why := HoldLive(profiles, name, nil)
	if live == nil {
		if errors.Is(why, ErrLiveHeld) {
			return errors.New(path + ": another launch is rewriting the profile's config; try again once it finishes")
		}
		return errors.New(lock + ": the live lock could not be created: " + why.Error())
	}
	if !held {
		// A caller that starts no session of this profile — a hop's check —
		// must not keep its lock past this call.
		defer ReleaseLive(profiles, name)
	}
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
