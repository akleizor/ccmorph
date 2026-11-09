package cli

import (
	"errors"
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
// back: the launch stops and names the one command that records a new answer —
// unless that answer is already saved, waiting for the profile's open session
// or launch to end (#299), in which case it is recorded here, before Claude
// Code starts. A saved approval that cannot be recorded — the profile is in
// use, or the file refuses a safe rewrite — stays saved whatever the file
// holds for the key, and the launch stops rather than ask the person the
// question ccmorph already holds the answer to.
func (l *launcher) keyApproval(name, key string) bool {
	home := filepath.Join(l.profileDirs, name)
	saved := util.ApprovalSaved(l.profileDirs, name, key)
	state := util.KeyResponse(home, key)
	if state == util.KeyRefused && !saved {
		l.err("Claude Code was told not to use the API key of '" + name + "' (the No answer to \"Do you want to use this API key?\"), so it would open its login menu instead; nothing started. Fix: ccmorph login " + name)
		return false
	}
	if state == util.KeyApproved {
		util.DropApproval(l.profileDirs, name, key)
		return true
	}
	err := util.ApproveKey(l.profileDirs, name, key)
	if err != nil && saved {
		// The saved approval could not be recorded, whether the file holds a
		// refusal or no answer yet. It stays saved for the launch that follows,
		// and nothing starts under it (#299).
		if errors.Is(err, util.ErrProfileInUse) {
			would := "open its login menu"
			if state == util.KeyUnanswered {
				would = "ask \"Do you want to use this API key?\" — the highlighted No turns the key off"
			}
			l.err("the approval `ccmorph login " + name + "` saved for the API key of '" + name + "' waits for the open session or launch of '" + name + "' to end; until it is recorded Claude Code would " + would + ", so nothing started. Close that session, then: ccmorph run " + name)
		} else {
			l.err("could not record the approval `ccmorph login " + name + "` saved for the API key of '" + name + "': " + err.Error() + "; nothing started.")
		}
		return false
	}
	// Recorded, or Claude Code will ask and the person's own answer is final;
	// either way nothing is left waiting (#299).
	util.DropApproval(l.profileDirs, name, key)
	if err != nil {
		l.err(keyQuestionHint)
	}
	return true
}
