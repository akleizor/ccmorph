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

	"github.com/akleizor/ccmorph/ordjson"
)

type verb func(args []string, in io.Reader, out, errw io.Writer) int

var verbs map[string]verb

func init() {
	verbs = map[string]verb{
		"bridge-start":           bridgeStart,
		"route-hold":             routeHold,
		"adapter-start":          adapterStart,
		"launch-env":             launchEnv,
		"claude-session":         claudeSession,
		"profile-read":           profileRead,
		"profile-env":            profileEnv,
		"normalize-plugin-index": normalizePluginIndexVerb,
	}
	// The shapes a v0.4.6 launcher still execs on the on-disk core (#162).
	for name, v := range v046Verbs {
		verbs[name] = v
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

// jsonOut is the compact JSON a shell caller parses.
func jsonOut(v any) string { return string(ordjson.Marshal(v, false)) }

func readAll(r io.Reader) []byte { b, _ := io.ReadAll(r); return b }

func asObj(v any) *ordjson.Obj { o, _ := v.(*ordjson.Obj); return o }

func str(v any) (string, bool) { s, ok := v.(string); return s, ok }

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
func loadJSON(path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ordjson.Parse(b)
}

// readConf is a profile conf's fields, read the one way ctxwindow does.
func readConf(path string) (map[string]string, error) {
	_, fields, err := ctxwindow.ReadConf(path)
	return fields, err
}
