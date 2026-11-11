package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/util/paths"
)

func mergedView(paths []string) *ordjson.Obj { return settingscompose.MergedView(paths) }

func acwValid(v any) bool { return settingscompose.ACWValid(v) }

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

// provisionProfile establishes a profile's auth-scoped config.json and
// .claude.json; nothing else is touched. The error carries the reason.
func provisionProfile(pdir, kind string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	appCfg := filepath.Join(home, "Library/Application Support/Claude/config.json")
	srcJSON := filepath.Join(home, ".claude.json")
	fallbackJSON := filepath.Join(home, ".claude", ".claude.json")
	if err := os.Chmod(pdir, 0o700); err != nil {
		return err
	}

	cfg := filepath.Join(pdir, "config.json")
	if paths.IsSymlink(cfg) {
		return errors.New(cfg + ": symbolic links are not allowed")
	}
	existing, err := loadObj(cfg, nil)
	if err != nil {
		return err
	}
	if kind == "sub" {
		hasToken := existing != nil && (truthy(getKey(existing, "oauth:tokenCache")) || truthy(getKey(existing, "oauth:tokenCacheV2")))
		// Best effort, and no longer load-bearing: a subscription credential comes from the profile's own /login.
		if !hasToken {
			seed, err := loadObj(appCfg, nil)
			if err != nil {
				return err
			}
			if seed != nil {
				if err := atomicJSON(cfg, seed); err != nil {
					return err
				}
			}
		} else if err := os.Chmod(cfg, 0o600); err != nil {
			return err
		}
	} else {
		data := existing
		if data == nil {
			if data, err = loadObj(appCfg, ordjson.New()); err != nil {
				return err
			}
		}
		dirty := existing == nil
		for _, k := range []string{"oauth:tokenCache", "oauth:tokenCacheV2"} {
			if data.Has(k) {
				data.Delete(k)
				dirty = true
			}
		}
		if dirty {
			err = atomicJSON(cfg, data)
		} else {
			err = os.Chmod(cfg, 0o600)
		}
		if err != nil {
			return err
		}
	}

	claudeJSON := filepath.Join(pdir, ".claude.json")
	if paths.IsSymlink(claudeJSON) {
		return errors.New(claudeJSON + ": symbolic links are not allowed")
	}
	data, err := loadObj(claudeJSON, nil)
	if err != nil {
		return err
	}
	fresh := data == nil
	if fresh {
		if data, err = loadObj(srcJSON, nil); err != nil {
			return err
		}
		if data == nil {
			if data, err = loadObj(fallbackJSON, ordjson.New()); err != nil {
				return err
			}
		}
	}
	dirty := fresh
	if fresh || kind != "sub" {
		for _, k := range []string{"oauthAccount", "hasAvailableSubscription", "cachedExtraUsageDisabledReason"} {
			if data.Has(k) {
				data.Delete(k)
				dirty = true
			}
		}
	}
	if dirty {
		return atomicJSON(claudeJSON, data)
	}
	return os.Chmod(claudeJSON, 0o600)
}
