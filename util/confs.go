package util

import (
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util/paths"
)

// GlobQuote quotes a literal path for use in front of a glob pattern. The zsh
// globbed a quoted "$_CCMORPH_DIR" followed by the pattern's own
// metacharacters, so the directory stayed literal; a joined Go pattern parses
// the whole path as syntax, and a `[` or `*` in it matches the wrong thing.
// The root is user-settable (CCMORPH_STATE_DIR, XDG_CONFIG_HOME), so every
// scan of it must quote first (#848).
func GlobQuote(path string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(path)
}

// ValidConfs is every profile conf under dir, in Glob order: one whose name
// profile's one gate accepts, and not a symlink. The keychain migration and
// the router's profile list shared this rule as two hand-written copies under
// two spellings of the same gate (ctxwindow.ValidProfileName, profile.ValidName);
// one scan holds it now, so the two lists cannot drift (#848). The cli's
// confNames is deliberately unfiltered (zsh parity, cli/verbs.go) and keeps
// its own scan.
func ValidConfs(dir string) []string {
	confs, _ := filepath.Glob(filepath.Join(GlobQuote(dir), "*.conf"))
	var valid []string
	for _, conf := range confs {
		if profile.ValidProfileName(strings.TrimSuffix(filepath.Base(conf), ".conf")) && !paths.IsSymlink(conf) {
			valid = append(valid, conf)
		}
	}
	return valid
}
