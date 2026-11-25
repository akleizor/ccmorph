package util

import (
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util/paths"
)

// GlobQuote is paths.GlobQuote under the name #848's callers already use.
func GlobQuote(path string) string {
	return paths.GlobQuote(path)
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
