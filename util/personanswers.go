package util

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util/paths"
)

// PersonGaps names the first-run questions Claude Code will still ask after
// SeedPersonAnswers ran: the onboarding screens (the theme list and the
// security notes), and the folder-trust dialog whose highlighted answer ends
// the session.
type PersonGaps struct {
	Onboarding bool
	Trust      bool
}

// SeedPersonAnswers carries the answers a person gave Claude Code's first-run
// questions — the onboarding acknowledgment, the theme, and this folder's
// trust — into profile name's .claude.json, filling only what is missing, so
// the next profile started in the folder opens straight to the prompt (#258).
// The answers are read from the other profiles' .claude.json files and the base
// install's own (the seed a fresh profile is provisioned from); a source that
// cannot be read is skipped, not fatal. The return names the questions that
// will still be asked, so the caller can say which answer is the safe one
// before claude starts. A file that cannot be safely rewritten — symlinked,
// unparsable, or holding a projects tree of another shape — is refused, not
// guessed at, and the gaps read as unanswered.
func SeedPersonAnswers(profiles, name, folder string) (PersonGaps, error) {
	if !profile.ValidName(name) {
		return PersonGaps{Onboarding: true, Trust: true}, errors.New("invalid profile name: " + name)
	}
	if folder == "" {
		return PersonGaps{Onboarding: true, Trust: true}, nil
	}
	folder = resolved(folder)
	path := filepath.Join(profiles, name, ".claude.json")
	if paths.IsSymlink(path) {
		return PersonGaps{Onboarding: true, Trust: true}, errors.New(path + ": symbolic links are not allowed")
	}
	doc, err := loadObj(path, ordjson.New())
	if err != nil {
		return PersonGaps{Onboarding: true, Trust: true}, err
	}
	before := personGaps(doc, folder)
	if !before.Onboarding && !before.Trust {
		return before, nil
	}
	dirty, err := seedPerson(doc, personDocs(profiles, name), folder)
	if err != nil || !dirty {
		return before, err
	}
	// The rewrite runs only while this launch is the sole holder of the
	// profile's live lock: a live session of the same profile writes its own
	// answers, and a whole-file swap must not race it (#185, #205). With no lock
	// to hold, the answers wait for a launch that can hold it, and the caller
	// re-takes the live lock before claude starts.
	live, _ := HoldLive(profiles, name, nil)
	if live == nil {
		return before, nil
	}
	if syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		// The failed conversion can leave this fd unlocked (Linux drops the
		// shared lock first), so the lock the session runs under is taken back or
		// gone; either way this rewrite waits for a launch that holds it.
		if !flockWithin(live, syscall.LOCK_SH) {
			ReleaseLive(profiles, name)
		}
		return before, nil
	}
	defer syscall.Flock(int(live.Fd()), syscall.LOCK_SH)
	if err := atomicJSON(path, doc); err != nil {
		return before, err
	}
	return personGaps(doc, folder), nil
}

// personGaps reads which first-run questions the doc still leaves Claude Code
// asking. A projects tree it cannot read counts as untrusted.
func personGaps(doc *ordjson.Obj, folder string) PersonGaps {
	gaps := PersonGaps{Onboarding: true, Trust: true}
	if truthy(getKey(doc, "hasCompletedOnboarding")) {
		gaps.Onboarding = false
	}
	if entry, err := folderEntry(doc, folder); err == nil && entry != nil && truthy(getKey(entry, "hasTrustDialogAccepted")) {
		gaps.Trust = false
	}
	return gaps
}

// folderEntry is the doc's projects entry for folder, or nil when it is absent.
// A projects or entry that is present but not a JSON object is an error: the
// caller must not guess and clobber what Claude Code wrote there.
func folderEntry(doc *ordjson.Obj, folder string) (*ordjson.Obj, error) {
	v, ok := doc.Get("projects")
	if !ok || v == nil {
		return nil, nil
	}
	projects := asObj(v)
	if projects == nil {
		return nil, errors.New("projects is not a JSON object")
	}
	entry, ok := projects.Get(folder)
	if !ok || entry == nil {
		return nil, nil
	}
	o := asObj(entry)
	if o == nil {
		return nil, errors.New("the entry for " + folder + " is not a JSON object")
	}
	return o, nil
}

// personDoc is one place the person's answers may live: a decoded .claude.json
// and the time its file was last written.
type personDoc struct {
	doc   *ordjson.Obj
	mtime time.Time
}

// personDocs gathers the sources to fill from: every other profile's
// .claude.json, then the base install's own — $HOME/.claude.json, falling back
// to $HOME/.claude/.claude.json, the seed a fresh profile is provisioned from.
// A file that is missing, unreadable or unparsable is skipped: the answers that
// can be read still carry.
func personDocs(profiles, name string) []personDoc {
	var docs []personDoc
	if ents, err := os.ReadDir(profiles); err == nil {
		for _, e := range ents {
			if !e.IsDir() || e.Name() == name || !profile.ValidName(e.Name()) {
				continue
			}
			if d, ok := readPersonDoc(filepath.Join(profiles, e.Name(), ".claude.json")); ok {
				docs = append(docs, d)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, p := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", ".claude.json")} {
			if d, ok := readPersonDoc(p); ok {
				docs = append(docs, d)
				break
			}
		}
	}
	return docs
}

func readPersonDoc(path string) (personDoc, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return personDoc{}, false
	}
	doc, err := loadObj(path, nil)
	if err != nil || doc == nil {
		return personDoc{}, false
	}
	return personDoc{doc: doc, mtime: fi.ModTime()}, true
}

// seedPerson fills the doc's missing first-run answers from the sources and
// reports whether it changed anything. A default fills a gap, it never
// overrules a choice: the doc's own theme keeps its value, a boolean only goes
// missing-or-false to true — the state answering the question records — and
// every other key and projects entry stays as it is.
func seedPerson(doc *ordjson.Obj, docs []personDoc, folder string) (bool, error) {
	dirty := false
	if !truthy(getKey(doc, "hasCompletedOnboarding")) {
		for _, d := range docs {
			if truthy(getKey(d.doc, "hasCompletedOnboarding")) {
				doc.Set("hasCompletedOnboarding", true)
				dirty = true
				break
			}
		}
	}
	if t, ok := getKey(doc, "theme").(string); !ok || t == "" {
		// Two profiles can carry two themes; the person's choice is the one they
		// answered last, which the newest-written source holds.
		var newest *personDoc
		for i := range docs {
			if s, ok := getKey(docs[i].doc, "theme").(string); !ok || s == "" {
				continue
			}
			if newest == nil || docs[i].mtime.After(newest.mtime) {
				newest = &docs[i]
			}
		}
		if newest != nil {
			doc.Set("theme", getKey(newest.doc, "theme"))
			dirty = true
		}
	}
	trustDirty, err := seedTrust(doc, docs, folder)
	return dirty || trustDirty, err
}

// seedTrust fills the folder's trust answer — hasTrustDialogAccepted, and the
// hasCompletedProjectOnboarding Claude Code records with it — into the folder's
// projects entry, each only when the doc has not recorded it.
func seedTrust(doc *ordjson.Obj, docs []personDoc, folder string) (bool, error) {
	entry, err := folderEntry(doc, folder)
	if err != nil {
		return false, err
	}
	if entry == nil {
		entry = ordjson.New()
	}
	dirty := false
	for _, k := range []string{"hasTrustDialogAccepted", "hasCompletedProjectOnboarding"} {
		if truthy(getKey(entry, k)) {
			continue
		}
		for _, d := range docs {
			if e, err := folderEntry(d.doc, folder); err == nil && e != nil && truthy(getKey(e, k)) {
				entry.Set(k, true)
				dirty = true
				break
			}
		}
	}
	if !dirty {
		return false, nil
	}
	projects := asObj(getKey(doc, "projects"))
	if projects == nil {
		projects = ordjson.New()
		doc.Set("projects", projects)
	}
	projects.Set(folder, entry)
	return true, nil
}
