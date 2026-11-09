package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/profile"
)

// profileVerb is `ccmorph profile`: name the profile this session runs on
// (#275). A session this ccmorph launched carries CCMORPH_PROFILE; one an older
// ccmorph launched still points CLAUDE_CONFIG_DIR at its profile dir, so both
// spell the name.
func (l *launcher) profileVerb(args []string) int {
	if len(args) > 0 {
		l.err("usage: ccmorph profile")
		return 2
	}
	name := os.Getenv("CCMORPH_PROFILE")
	if name == "" {
		if cfg := os.Getenv("CLAUDE_CONFIG_DIR"); cfg != "" {
			// Like zsh's :A, resolve what exists: the profile dir itself may not yet.
			parent, dirs := resolvePath(filepath.Dir(cfg)), resolvePath(l.profileDirs)
			if parent != "" && parent == dirs {
				name = filepath.Base(cfg)
			}
		}
	}
	if !profile.ValidName(name) {
		l.err("not inside a ccmorph session")
		return 1
	}
	fmt.Println(name)
	return 0
}
