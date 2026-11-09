package cli

import (
	"path/filepath"

	"github.com/akleizor/ccmorph/util"
)

// keyQuestionHint is what a person is told when ccmorph could not record the
// answer for them: Claude Code's first-launch question defaults to No, which
// turns the profile's key off for good.
const keyQuestionHint = `Claude Code will ask "Do you want to use this API key?": choose Yes — the highlighted No turns this profile's key off.`

// keyApproval makes sure Claude Code will use the profile's key before it
// starts, so a first launch never reaches the question whose default turns the
// key off. A key the person refused earlier is not overridden behind their
// back: the launch stops and names the one command that records a new answer.
func (l *launcher) keyApproval(name, key string) bool {
	home := filepath.Join(l.profileDirs, name)
	switch util.KeyResponse(home, key) {
	case util.KeyRefused:
		l.err("Claude Code was told not to use the API key of '" + name + "' (the No answer to \"Do you want to use this API key?\"), so it would open its login menu instead; nothing started. Fix: ccmorph login " + name)
		return false
	case util.KeyApproved:
		return true
	}
	if err := util.ApproveKey(l.profileDirs, name, key); err != nil {
		l.err(keyQuestionHint)
	}
	return true
}
