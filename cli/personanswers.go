package cli

import (
	"os"

	"github.com/akleizor/ccmorph/util"
)

// trustQuestionHint is what a person is told when this folder still has to be
// trusted on the profile claude is about to start: the dialog's highlighted
// answer ends the session, so pressing Enter out of habit must be an informed
// choice, not a silent return to the shell (#258).
const trustQuestionHint = `Claude Code will ask "Is this a project you created or one you trust?": choose Yes, I trust this folder — the highlighted No, exit ends the session.`

// onboardingQuestionHint is what a person is told when the first-run screens —
// the theme list and the security notes — still have to be answered on the
// profile claude is about to start.
const onboardingQuestionHint = `Claude Code will show its first-run theme and security questions before the prompt.`

// personAnswers carries the answers a person gave Claude Code's first-run
// questions to this profile before claude starts, so a hop or a second profile
// in the folder lands on the prompt or the resumed conversation, not on the
// questions again (#258). warn marks a hop onto another profile's config — the
// one place a question can still meet the person mid-session — and there, and
// only there, whatever could not be filled gets one line naming the safe
// answer, printed before claude starts. An ordinary launch stays as quiet as it
// was: its questions are Claude Code's normal first run, not an interruption.
// The live lock is required again after seeding: its rewrite takes the lock
// exclusively, and a failed upgrade can leave the launch holding nothing (Linux
// drops the shared lock first). Claude must not start through the window a
// merge opens (#185, #205); false means it must not start at all.
func (l *launcher) personAnswers(name string, warn bool) bool {
	folder, err := os.Getwd()
	if err != nil {
		folder = ""
	}
	gaps, err := util.SeedPersonAnswers(l.profileDirs, name, folder)
	if err != nil {
		l.log("person answers: " + err.Error())
	}
	if !l.holdLive(name, liveRefuse) {
		return false
	}
	if warn {
		switch {
		case gaps.Trust:
			l.err(trustQuestionHint)
		case gaps.Onboarding:
			l.err(onboardingQuestionHint)
		}
	}
	return true
}
