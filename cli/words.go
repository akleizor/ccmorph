package cli

import "github.com/akleizor/ccmorph/util"

// The words for a profile with nothing saved, shared by every screen that
// names the state (#381): config, inspect, get and doctor once each worded it
// their own way, and none of the five wordings said what a launch does. Each
// screen keeps its own framing — get's "no model saved — ", the picker's
// "Keep none — ", doctor's "(no saved model)" — around the same words.

// unsavedModelWords is what a launch does when no model is saved: a gateway
// (an API key on another endpoint) is asked which model at every launch, a
// plain run too (#360); everywhere else no --model is passed, so Claude Code
// picks the model itself.
func unsavedModelWords(typ, base string) string {
	if util.Maskable(typ, base) {
		return "ccmorph asks which model at each launch"
	}
	return "Claude Code picks the model"
}

// unsavedEffortWords is what a launch does when no effort is saved: no level
// is passed, so the session starts at Claude Code's own default.
const unsavedEffortWords = "Claude Code's own default"

// unsavedLabel is the unsaved state as a picker shows it: the none it keeps,
// then what that means at launch.
func unsavedLabel(slot, typ, base string) string {
	if slot == "model" {
		return "none — " + unsavedModelWords(typ, base)
	}
	return "none — " + unsavedEffortWords
}
