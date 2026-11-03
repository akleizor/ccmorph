// Package util holds the small shell-facing helpers behind `ccmorph-core util <verb>`:
// the JSON, context-window and process readers the zsh library used to run through
// python3. Each verb keeps the stdout, stderr and exit status its python program had.
package util

import (
	"fmt"
	"github.com/akleizor/ccmorph/ctxwindow"
	"io"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/pycompat"
)

type verb func(args []string, in io.Reader, out, errw io.Writer) int

var verbs map[string]verb

func init() {
	verbs = map[string]verb{
		"bridge-start":           bridgeStart,
		"adapter-start":          adapterStart,
		"launch-env":             launchEnv,
		"claude-session":         claudeSession,
		"profile-read":           profileRead,
		"profile-env":            profileEnv,
		"normalize-plugin-index": normalizePluginIndexVerb,
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

// AtomicWrite replaces path through a same-dir temp file, renamed into place.
func AtomicWrite(path, prefix string, data []byte, mode os.FileMode, sync bool) error {
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

// readConf is a profile conf's fields, read the one way ctxwindow does.
func readConf(path string) map[string]string {
	_, fields := ctxwindow.ReadConf(path)
	return fields
}
