package router

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// GH-848: the profile root is literal text, not a pattern — the zsh globbed a
// quoted "$_CCMORPH_DIR" so a `[` in it matched only that directory name. The
// router's own scan joined the root to "*.conf" unescaped, so on a root
// carrying a glob metacharacter filepath.Glob parses the whole path as syntax
// (an unclosed `[` is a bad pattern, and the ignored error answers nothing)
// while every cli verb lists the right files. The list must hold exactly the
// valid, non-symlinked confs whatever the root's spelling.
func TestProfileNamesSeesEveryConfUnderABracketedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "confs[cd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "bad-name"} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"), []byte("type=apikey\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "a.conf"), filepath.Join(root, "c.conf")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCMORPH_STATE_DIR", root)
	if names := profileNames(); !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("profileNames() under a root holding `[` = %v, want exactly [a b]", names)
	}
}
