package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// The cache is the last good reading of one profile, so a later 429 or network
// error shows real numbers with their age instead of a blank row. It holds
// readings only: no token, no header, nothing a credential could be read from.

func cacheDir() string { return filepath.Join(stateDir(), "quota") }

func cachePath(name string) string { return filepath.Join(cacheDir(), name+".json") }

// validName keeps a profile name from escaping the cache directory.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func save(name string, r Reading) {
	if !validName(name) {
		return
	}
	dir := cacheDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".quota-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return
	}
	os.Chmod(tmp, 0o600)
	if os.Rename(tmp, cachePath(name)) != nil {
		os.Remove(tmp)
	}
}

func load(name string) (Reading, bool) {
	var r Reading
	if !validName(name) {
		return r, false
	}
	b, err := os.ReadFile(cachePath(name))
	if err != nil || json.Unmarshal(b, &r) != nil || r.ReadAt == 0 {
		return Reading{}, false
	}
	return r, true
}

// stale is the entry for a read that could not happen: the cached reading with
// its age when one exists, otherwise the plain reason. Auth failures never call
// it — a refused sign-in means the cached numbers belong to a sign-in that is
// gone, so the refusal is what the user needs to see.
func stale(name, kind, plan, reason, neverRead string) Entry {
	e := Entry{Profile: name, Kind: kind, Plan: plan, Windows: []Window{}}
	cached, ok := load(name)
	if !ok {
		e.Error = neverRead
		return e
	}
	readAt := cached.ReadAt
	age := now().Unix() - cached.ReadAt
	if age < 0 {
		age = 0
	}
	e.Plan = cached.Plan
	e.Windows = cached.Windows
	if e.Windows == nil {
		e.Windows = []Window{}
	}
	e.ReadAt = &readAt
	e.AgeSeconds = &age
	e.Stale = true
	e.Error = reason
	return e
}

// fresh stamps an entry read just now.
func fresh(e *Entry) {
	readAt := now().Unix()
	var age int64
	e.ReadAt = &readAt
	e.AgeSeconds = &age
}

// ageText is a reading's age in words: minutes under an hour, hours under two
// days, days after that.
func ageText(seconds int64) string {
	switch {
	case seconds < 3600:
		m := seconds / 60
		if m < 1 {
			m = 1
		}
		return strconv.FormatInt(m, 10) + " min old"
	case seconds < 48*3600:
		return strconv.FormatInt(seconds/3600, 10) + " h old"
	}
	return strconv.FormatInt(seconds/86400, 10) + " d old"
}
