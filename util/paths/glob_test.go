package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// #852: GlobQuote quotes only the root prefix, so the pattern's own
// metacharacters — appended after the call — stay wildcards. On a literal
// root, every pattern the scans join against a root must still match the
// file it means to match: add's credential scan (*.json, */*.json,
// */*/*.json), GCStale's relay (.reswitch.*) and overlay
// (*/cache/launch-settings.*) sweeps, quota's auth reads, and the settings
// backup prune (settings.*.json).
func TestGlobQuoteKeepsThePatternsOwnWildcards(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct{ dir, pattern, file string }{
		{"auth", "*.json", "auth/cred.json"},
		{"auth", "*/*.json", "auth/sub/cred.json"},
		{"auth", "*/*/*.json", "auth/sub/deep/cred.json"},
		{"state", ".reswitch.*", "state/.reswitch.123.1.2"},
		{"profiles", "*/cache/launch-settings.*", "profiles/demo/cache/launch-settings.abc"},
		{"backups", "settings.*.json", "backups/settings.20261125T000000.000001.json"},
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(c.file)), 0o700); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, filepath.FromSlash(c.file))
		if err := os.WriteFile(want, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := filepath.Glob(filepath.Join(GlobQuote(filepath.Join(root, c.dir)), c.pattern))
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("pattern %q under a literal root %q: got %q (%v), want exactly %q",
				c.pattern, c.dir, got, err, want)
		}
	}
}
