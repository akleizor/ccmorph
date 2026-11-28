package util

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/akleizor/ccmorph/util/paths"
)

// plantIndex plants a marketplace index under home whose one installLocation is
// an absolute path under profiles, alongside the shared-plugins target the repair
// rewrites it to; it reports the index path and the body as planted.
func plantIndex(t *testing.T, home, profiles string) (index, body string) {
	t.Helper()
	plugins := filepath.Join(home, ".claude", "plugins")
	if err := os.MkdirAll(filepath.Join(plugins, "marketplaces", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	index = filepath.Join(plugins, "known_marketplaces.json")
	body = `{"foo":{"installLocation":"` +
		filepath.ToSlash(filepath.Join(profiles, "p1", "plugins", "marketplaces", "foo")) + `"}}` + "\n"
	mustWrite(t, index, body)
	return index, body
}

// GH-1065: the repair joined ".claude" and ".claude-profiles" on the home it was
// handed, and install.sh's uninstall hands it "$HOME" — empty when HOME is unset
// — so it read and rewrote .claude/plugins/known_marketplaces.json in the
// installer's cwd, replacing an absolute installLocation with a path relative to
// that cwd. A home that does not name one resolves through paths.Home, never
// against the cwd; an absolute home stays the caller's own, which is how an
// isolated test home or a redirected install gets its index repaired.
func TestNormalizePluginIndexHomeIsNeverTheCwd(t *testing.T) {
	root := t.TempDir()
	acct, cwd, profiles := filepath.Join(root, "acct"), filepath.Join(root, "cwd"), filepath.Join(root, "profiles")
	for _, d := range []string{acct, cwd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	old := paths.AccountHome
	paths.AccountHome = func() string { return acct }
	t.Cleanup(func() { paths.AccountHome = old })
	t.Chdir(cwd)

	index, planted := plantIndex(t, cwd, profiles)
	if !NormalizePluginIndex("", profiles) {
		t.Fatal("the repair reported a failure it cannot have made: there is no index under the account home")
	}
	if got, err := os.ReadFile(index); err != nil || string(got) != planted {
		t.Fatalf("the cwd's plugin index was rewritten:\n got %q (%v)\nwant %q", got, err, planted)
	}

	given := filepath.Join(root, "given")
	index, _ = plantIndex(t, given, profiles)
	if !NormalizePluginIndex(given, profiles) {
		t.Fatal("the repair failed on an absolute home it was handed")
	}
	rec := map[string]struct {
		InstallLocation string `json:"installLocation"`
	}{}
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash(filepath.Join(given, ".claude", "plugins", "marketplaces", "foo"))
	if rec["foo"].InstallLocation != want {
		t.Fatalf("an absolute home is the caller's own:\n got %q\nwant %q", rec["foo"].InstallLocation, want)
	}
}
