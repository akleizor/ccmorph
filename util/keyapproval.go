package util

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// the file is rewritten only when the answer actually changes. The rewrite is
// a whole-file read-modify-write of a file a running session, SeedPersonAnswers
// or a launch's merge also writes, so it runs while this process is the sole
// holder of the profile's live lock — exclusively from the read to the rename,
// the mode those siblings take — and no write can be lost under it (#299).
// When the lock cannot be taken that way within the bounded wait, nothing is
// written and the error wraps ErrProfileInUse, so the caller can keep the
// approval for the next launch with SaveApproval. A symlinked or unparsable
// file, or a customApiKeyResponses that is not an object, is an error: the
// caller must not guess and clobber what Claude Code wrote.
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
	// The profile's live lock is held exclusively across the read and the
	// rename: every ccmorph session holds it shared for its life and every
	// whole-file writer takes it exclusively, so while this rewrite runs no
	// session, launch or second approval can write the file — and none of
	// theirs can be lost under it either (#299). A holder that outlasts the
	// bounded wait (promise 8) leaves the file untouched; the caller can save
	// the approval for the next launch instead of dropping it.
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

// ErrProfileInUse is why an approval could not be recorded: a session or
// launch of the profile holds its config, so a whole-file rewrite now would
// race that holder's own save and could lose it (#299). The caller can keep
// the approval for the next launch with SaveApproval instead of dropping it.
var ErrProfileInUse = errors.New("a session or launch of the profile is open")

// approvalMarker names the file an approval waiting for an open session lives
// in: beside the profile's other locks, never inside the profile's own dir
// (a session's private view symlinks every entry of it). It holds no key
// material — its name is a digest of the key's last 20 characters, the part
// Claude Code itself stores in plain text (#299, promise 5).
func approvalMarker(profiles, name, key string) string {
	sum := sha256.Sum256([]byte(keyTail(key)))
	return filepath.Join(filepath.Dir(profiles), ".locks", name+".approve-"+hex.EncodeToString(sum[:8]))
}

// SaveApproval keeps an approval that cannot be recorded now — the profile's
// config is held by an open session or launch — for the next launch to record
// before Claude Code starts (#299). The marker is empty and owner-only.
func SaveApproval(profiles, name, key string) error {
	if !profile.ValidName(name) {
		return errors.New("invalid profile name: " + name)
	}
	p := approvalMarker(profiles, name, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// ApprovalSaved reports whether an approval is saved for key, waiting for the
// profile's open session or launch to end (#299).
func ApprovalSaved(profiles, name, key string) bool {
	if !profile.ValidName(name) {
		return false
	}
	fi, err := os.Lstat(approvalMarker(profiles, name, key))
	return err == nil && fi.Mode().IsRegular()
}

// DropApproval forgets a saved approval: it has been recorded, or Claude Code
// holds the person's own answer (#299).
func DropApproval(profiles, name, key string) {
	if !profile.ValidName(name) {
		return
	}
	os.Remove(approvalMarker(profiles, name, key))
}

// DropApprovals removes every saved approval marker of the profile, whatever
// key each was saved for — rm's cleanup, which cannot know the key (#299).
func DropApprovals(profiles, name string) {
	if !profile.ValidName(name) {
		return
	}
	dir := filepath.Join(filepath.Dir(profiles), ".locks")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// ReadDir, not Glob: the state path may hold glob metacharacters.
	prefix := name + ".approve-"
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
