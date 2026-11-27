package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/paths"
)

// acwString is the integer text of a valid autoCompactWindow, which may be an integer-valued float.
func acwString(v any) string {
	n := v.(json.Number)
	if i, err := n.Int64(); err == nil {
		return fmt.Sprint(i)
	}
	f, _ := n.Float64()
	return fmt.Sprint(int64(f))
}

// loadObj is the provisioner's load(): a missing file is def, anything unreadable
// or not a JSON object is an error.
func loadObj(path string, def *ordjson.Obj) (*ordjson.Obj, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		if os.IsPermission(err) {
			return nil, errors.New(path + ": permission denied")
		}
		return nil, err
	}
	v, perr := ordjson.Parse(b)
	if perr != nil {
		return nil, fmt.Errorf("%s: malformed JSON: %v", path, perr)
	}
	o := asObj(v)
	if o == nil {
		return nil, errors.New(path + ": root is not a JSON object")
	}
	return o, nil
}

func atomicJSON(path string, data *ordjson.Obj) error {
	return AtomicWrite(path, ".ccmorph.", append(ordjson.Marshal(data, false), '\n'), 0o600, true)
}

// baseClaudeJSONs is where the base install keeps its .claude.json: the home
// dir, its .claude subdir the fallback. The pair, in order, is the seed a
// fresh profile is provisioned from.
func baseClaudeJSONs(home string) []string {
	return []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", ".claude.json")}
}

// configEdit applies a profile's auth scoping to config.json's decoded content
// — a missing file is nil — and reports whether the file must be written or
// only have its mode tightened.
func configEdit(kind, appCfg string, existing *ordjson.Obj) (data *ordjson.Obj, write, chmod bool, err error) {
	if kind == "sub" {
		hasToken := existing != nil && (ordjson.Truthy(getKey(existing, "oauth:tokenCache")) || ordjson.Truthy(getKey(existing, "oauth:tokenCacheV2")))
		if hasToken {
			return existing, false, true, nil
		}
		// Best effort, and no longer load-bearing: a subscription credential comes from the profile's own /login.
		seed, err := loadObj(appCfg, nil)
		if err != nil {
			return nil, false, false, err
		}
		if seed == nil {
			return existing, false, false, nil
		}
		return seed, true, false, nil
	}
	data = existing
	if data == nil {
		if data, err = loadObj(appCfg, ordjson.New()); err != nil {
			return nil, false, false, err
		}
	}
	write = existing == nil
	for _, k := range []string{"oauth:tokenCache", "oauth:tokenCacheV2"} {
		if data.Has(k) {
			data.Delete(k)
			write = true
		}
	}
	return data, write, !write, nil
}

// claudeJSONEdit applies a profile's auth scoping to .claude.json's decoded
// content — a missing file is seeded from the base install — and reports
// whether the file must be written.
func claudeJSONEdit(kind string, seeds []string, existing *ordjson.Obj) (*ordjson.Obj, bool, error) {
	fresh := existing == nil
	data := existing
	if fresh {
		var err error
		if data, err = loadObj(seeds[0], nil); err != nil {
			return nil, false, err
		}
		if data == nil {
			if data, err = loadObj(seeds[1], ordjson.New()); err != nil {
				return nil, false, err
			}
		}
	}
	write := fresh
	if fresh || kind != "sub" {
		for _, k := range []string{"oauthAccount", "hasAvailableSubscription", "cachedExtraUsageDisabledReason"} {
			if data.Has(k) {
				data.Delete(k)
				write = true
			}
		}
	}
	return data, write, nil
}

// provisionJSON holds the profile's live lock exclusively across one file's
// read-to-rename window, so the read that decides the write cannot be overtaken
// by a session's save the rename then drops.
func provisionJSON(profiles, name, path string, write func() error) error {
	release, err := liveExclusiveLock(profiles, name, path)
	if err != nil {
		return err
	}
	defer release()
	return write()
}

// ProvisionOK reports whether both auth files already hold what provisioning
// would write: neither is a symlink, both parse, and the edits provisioning
// applies would change nothing. ProfileOK answers for the shared layer; this
// answers for the auth files, so a launch whose earlier provisioning was
// skipped as busy tries again on the next launch from the files' actual state
// — no marker, no retry loop (#938).
func ProvisionOK(profiles, name, kind string) bool {
	dir := filepath.Join(profiles, name)
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	appCfg := filepath.Join(home, "Library/Application Support/Claude/config.json")
	cfg := filepath.Join(dir, "config.json")
	if paths.IsSymlink(cfg) {
		return false
	}
	cfgExisting, err := loadObj(cfg, nil)
	if err != nil {
		return false
	}
	if _, write, _, err := configEdit(kind, appCfg, cfgExisting); err != nil || write {
		return false
	}
	claudeJSON := filepath.Join(dir, ".claude.json")
	if paths.IsSymlink(claudeJSON) {
		return false
	}
	claudeExisting, err := loadObj(claudeJSON, nil)
	if err != nil {
		return false
	}
	if _, write, err := claudeJSONEdit(kind, baseClaudeJSONs(home), claudeExisting); err != nil || write {
		return false
	}
	return true
}

// provisionProfile establishes a profile's auth-scoped config.json and
// .claude.json; nothing else is touched. Each file's read-to-rename window is
// held under the profile's live lock exclusively, so a live session's write is
// never dropped under provisioning's rename. A holder that outlasts the
// bounded wait leaves the files untouched and the error wraps ErrProfileInUse.
// The error carries the reason.
func provisionProfile(pdir, kind string) error {
	pdir = filepath.Clean(pdir)
	profiles, name := filepath.Dir(pdir), filepath.Base(pdir)
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	appCfg := filepath.Join(home, "Library/Application Support/Claude/config.json")
	seeds := baseClaudeJSONs(home)
	if err := os.Chmod(pdir, 0o700); err != nil {
		return err
	}

	cfg := filepath.Join(pdir, "config.json")
	claudeJSON := filepath.Join(pdir, ".claude.json")
	// Both auth files are validated before either write: a launch that cannot
	// take the live lock must still refuse an invalid file — a symlink or
	// unparseable JSON — rather than read the contention as the harmless busy
	// skip and start claude over it (#938).
	if paths.IsSymlink(cfg) {
		return errors.New(cfg + ": symbolic links are not allowed")
	}
	cfgExisting, err := loadObj(cfg, nil)
	if err != nil {
		return err
	}
	if paths.IsSymlink(claudeJSON) {
		return errors.New(claudeJSON + ": symbolic links are not allowed")
	}
	claudeExisting, err := loadObj(claudeJSON, nil)
	if err != nil {
		return err
	}

	_, write, chmod, err := configEdit(kind, appCfg, cfgExisting)
	if err != nil {
		return err
	}
	// Both decisions are made before either write, so a seed the edit cannot
	// read still refuses a launch whose config.json rewrite is skipped as
	// busy, just as the files themselves do above (#938).
	_, claudeWrite, err := claudeJSONEdit(kind, seeds, claudeExisting)
	if err != nil {
		return err
	}
	switch {
	case write:
		err = provisionJSON(profiles, name, cfg, func() error {
			if paths.IsSymlink(cfg) {
				return errors.New(cfg + ": symbolic links are not allowed")
			}
			existing, err := loadObj(cfg, nil)
			if err != nil {
				return err
			}
			profileJSONRead(cfg)
			data, write, chmod, err := configEdit(kind, appCfg, existing)
			if err != nil {
				return err
			}
			if !write {
				if chmod {
					return os.Chmod(cfg, 0o600)
				}
				return nil
			}
			return atomicJSON(cfg, data)
		})
	case chmod:
		err = os.Chmod(cfg, 0o600)
	}
	if err != nil {
		return err
	}

	if !claudeWrite {
		return os.Chmod(claudeJSON, 0o600)
	}
	return provisionJSON(profiles, name, claudeJSON, func() error {
		if paths.IsSymlink(claudeJSON) {
			return errors.New(claudeJSON + ": symbolic links are not allowed")
		}
		existing, err := loadObj(claudeJSON, nil)
		if err != nil {
			return err
		}
		profileJSONRead(claudeJSON)
		data, write, err := claudeJSONEdit(kind, seeds, existing)
		if err != nil {
			return err
		}
		if !write {
			return os.Chmod(claudeJSON, 0o600)
		}
		return atomicJSON(claudeJSON, data)
	})
}
