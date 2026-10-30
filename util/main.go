// Package util holds the small shell-facing helpers behind `ccmorph-core util <verb>`:
// the JSON, context-window and process readers the zsh library used to run through
// python3. Each verb keeps the stdout, stderr and exit status its python program had.
package util

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/pycompat"
)

type verb func(args []string, in io.Reader, out, errw io.Writer) int

var verbs map[string]verb

func init() {
	verbs = map[string]verb{
		"migrate-profile-home":   migrateProfileHome,
		"profile-read":           profileRead,
		"listing-has-unsized":    listingHasUnsized,
		"keychain-migrate":       keychainMigrate,
		"keychain-rename":        keychainRename,
		"keychain-delete":        keychainDelete,
		"normalize-plugin-index": normalizePluginIndex,
		"valid-model-map":        validModelMap,
		"launch-diagnostics":     launchDiagnostics,
		"engine-int":             engineIntVerb,
		"hazard-set":             hazardSetVerb,
		"hazard-msg":             hazardMsgVerb,
		"withhold-fatal":         withholdFatal,
		"policy-pct":             policyPctVerb,
		"effective-pct":          effectivePctVerb,
		"config-store":           configStore,
		"settings-valid":         settingsValid,
		"models-count":           modelsCount,
		"running-adapter":        runningAdapter,
		"listener-of":            listenerOf,
		"models-filtered-json":   modelsFilteredJSON,
		"window-label":           windowLabel,
		"effort-rows":            effortRows,
		"saved-provider-models":  savedProviderModels,
		"provision-profile":      provisionProfile,
		"unwire-statusline":      unwireStatusline,
		"resume-model":           resumeModel,
		"session-state":          sessionState,
		"launch-window":          launchWindow,
		"redact-url":             redactURL,
		"gw-models":              gwModels,
		"model-known":            modelKnown,
		"project-picker":         projectPicker,
		"advise-json":            adviseJSON,
		"rc-interpreter":         rcInterpreter,
	}
}

// Main runs `util <verb> args...` and returns its exit status.
func Main(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 0 || verbs[args[0]] == nil {
		fmt.Fprintln(errw, "usage: ccmorph-core util <verb> ...")
		return 2
	}
	return verbs[args[0]](args[1:], in, out, errw)
}

// jsonOut is json.dumps with its default separators and ensure_ascii.
func jsonOut(v pycompat.Value) string { return pycompat.Marshal(v, pycompat.Format{ASCII: true}) }

func readAll(r io.Reader) []byte { b, _ := io.ReadAll(r); return b }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && fi.Mode().IsRegular() }

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func asObj(v pycompat.Value) *pycompat.Obj { o, _ := v.(*pycompat.Obj); return o }

func str(v pycompat.Value) (string, bool) { s, ok := v.(string); return s, ok }

// truthy is Python's bool() over a decoded JSON value.
func truthy(v pycompat.Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case pycompat.Num:
		return t.F64() != 0
	case []any:
		return len(t) > 0
	case *pycompat.Obj:
		return t.Len() > 0
	}
	return true
}

// atomicWrite is the mkstemp, write, chmod, replace sequence the helpers shared.
func atomicWrite(path, prefix string, data []byte, mode os.FileMode, sync bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), prefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	bad := func(err error) error { f.Close(); os.Remove(tmp); return err }
	if _, err := f.Write(data); err != nil {
		return bad(err)
	}
	if sync {
		if err := f.Sync(); err != nil {
			return bad(err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// loadJSON reads and decodes a file like json.load(open(path)).
func loadJSON(path string) (pycompat.Value, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pycompat.Parse(b)
}

// confFields is the lenient key=value reader the shell helpers used: a line is
// split once on "=", "#" lines are skipped, and nothing is trimmed.
func confFields(path string) map[string]string {
	fields := map[string]string{}
	for _, line := range splitUniversal(pycompat.ReadText(path)) {
		if k, v, ok := cut(line); ok {
			fields[k] = v
		}
	}
	return fields
}

func splitUniversal(s string) []string { return strings.Split(s, "\n") }

func cut(line string) (string, string, bool) {
	if len(line) > 0 && line[0] == '#' {
		return "", "", false
	}
	for i := 0; i < len(line); i++ {
		if line[i] == '=' {
			return line[:i], line[i+1:], true
		}
	}
	return "", "", false
}
