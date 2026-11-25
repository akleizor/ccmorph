package cli

import (
	"fmt"
	"os"

	"github.com/akleizor/ccmorph/profile"
)

// notInSession is what profile and hop answer outside a session ccmorph
// launched: one sentence, naming where both work (#636).
const notInSession = "not inside a ccmorph session; this works inside a Claude Code session started with: ccmorph run <profile>"

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
		name = l.configDirProfile()
	}
	if !profile.ValidName(name) {
		l.err(notInSession)
		return 1
	}
	fmt.Println(name)
	return 0
}
