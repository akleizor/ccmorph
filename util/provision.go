package util

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/pycompat"
	"github.com/akleizor/ccmorph/settingscompose"
)

func mergedView(paths []string) *pycompat.Obj { return settingscompose.MergedView(paths) }

func acwValid(v pycompat.Value) bool { return settingscompose.ACWValid(v) }

// acwString is str(int(v)) for a valid autoCompactWindow, which may be an integer-valued float.
func acwString(v pycompat.Value) string {
	n := v.(pycompat.Num)
	if i, ok := n.Int64(); ok {
		return fmt.Sprint(i)
	}
	return fmt.Sprint(int64(n.F64()))
}

type exitMsg string

// loadObj is the provisioner's load(): a missing file is def, anything unreadable
// or not a JSON object ends the run with a message.
func loadObj(path string, def *pycompat.Obj) *pycompat.Obj {
	b, err := os.ReadFile(path)
	if err != nil {
		if pycompat.IsNotExist(err) {
			return def
		}
		if os.IsPermission(err) {
			panic(exitMsg(path + ": permission denied"))
		}
		pycompat.RaiseOS(err)
	}
	v, perr := pycompat.Parse(b)
	if perr != nil {
		panic(exitMsg(fmt.Sprintf("%s: malformed JSON: %s", path, perr.Error())))
	}
	o := asObj(v)
	if o == nil {
		panic(exitMsg(path + ": root is not a JSON object"))
	}
	return o
}

func atomicJSON(path string, data *pycompat.Obj) {
	b := []byte(pycompat.Marshal(data, pycompat.Format{Compact: true, ASCII: true}) + "\n")
	if err := atomicWrite(path, ".ccmorph.", b, 0o600, true); err != nil {
		pycompat.RaiseOS(err)
	}
}

func chmod600(path string) {
	if err := os.Chmod(path, 0o600); err != nil {
		pycompat.RaiseOS(err)
	}
}

// provisionProfile: provision-profile <profile dir> <type>. Establishes a profile's
// auth-scoped config.json and .claude.json; nothing else is touched.
func provisionProfile(args []string, _ io.Reader, _, errw io.Writer) (rc int) {
	if len(args) != 2 {
		return 2
	}
	defer func() {
		switch r := recover().(type) {
		case nil:
		case exitMsg:
			fmt.Fprintln(errw, string(r))
			rc = 1
		case *pycompat.Exc:
			fmt.Fprintln(errw, r.Msg)
			rc = 1
		default:
			panic(r)
		}
	}()
	pdir, kind := args[0], args[1]
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	appCfg := filepath.Join(home, "Library/Application Support/Claude/config.json")
	srcJSON := filepath.Join(home, ".claude.json")
	fallbackJSON := filepath.Join(home, ".claude", ".claude.json")
	if err := os.Chmod(pdir, 0o700); err != nil {
		pycompat.RaiseOS(err)
	}

	cfg := filepath.Join(pdir, "config.json")
	if isSymlink(cfg) {
		panic(exitMsg(cfg + ": symbolic links are not allowed"))
	}
	existing := loadObj(cfg, nil)
	if kind == "sub" {
		hasToken := existing != nil && (truthy(getKey(existing, "oauth:tokenCache")) || truthy(getKey(existing, "oauth:tokenCacheV2")))
		// Best effort, and no longer load-bearing: a subscription credential comes from the profile's own /login.
		if !hasToken {
			if seed := loadObj(appCfg, nil); seed != nil {
				atomicJSON(cfg, seed)
			}
		} else {
			chmod600(cfg)
		}
	} else {
		data := existing
		if data == nil {
			data = loadObj(appCfg, pycompat.NewObj())
		}
		dirty := existing == nil
		for _, k := range []string{"oauth:tokenCache", "oauth:tokenCacheV2"} {
			if data.Has(k) {
				data.Delete(k)
				dirty = true
			}
		}
		if dirty {
			atomicJSON(cfg, data)
		} else {
			chmod600(cfg)
		}
	}

	claudeJSON := filepath.Join(pdir, ".claude.json")
	if isSymlink(claudeJSON) {
		panic(exitMsg(claudeJSON + ": symbolic links are not allowed"))
	}
	data := loadObj(claudeJSON, nil)
	fresh := data == nil
	if fresh {
		data = loadObj(srcJSON, nil)
		if data == nil {
			data = loadObj(fallbackJSON, pycompat.NewObj())
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
		atomicJSON(claudeJSON, data)
	} else {
		chmod600(claudeJSON)
	}
	return 0
}
