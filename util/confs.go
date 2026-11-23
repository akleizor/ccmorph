package util

import (
	"strings"
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
