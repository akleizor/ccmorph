package util

import (
	"errors"
	"strings"
)

// LaunchFlags is what the launch needs from Claude Code's own argv: model (an
// explicit --model), UserModel, SkipModel, AskMissingModel, Resume, Interactive,
// Relay (--dangerously-skip-permissions, once per occurrence), Effort (an
// explicit --effort, UserEffort its bare presence), Resuming (any spelling that
// resumes a conversation instead of starting one) and Drop (the 1-based indexes
// of `--model -i` pairs to remove, highest first).
type LaunchFlags struct {
	Model, Resume, Effort                              string
	UserModel, SkipModel, AskMissingModel, Interactive bool
	UserEffort, Resuming                               bool
	Relay                                              []string
	Drop                                               []int
}

// launchStep is one step of the launch flag grammar: the spelling the launch
// parses itself ("" when the token is a positional or an option only the arity
// table knows), the value the step owns — carried inside the token (inside) or
// the token after it — and how many tokens the step covers. No step owns the
// `--` separator: it ends the scan and the tokens after it are positionals
// (#405).
type launchStep struct {
	opt, value string
	inside     bool
	wide       int
}

// stepLaunch is the launch flag grammar's one walk, the reader behind every
// launch-side scan of Claude Code's argv (#490): what the token at argv[at] is
// and how far the step reaches. The spellings the launch parses itself carry
// their own rule; every other option follows the arity table (spelling to flag,
// value or optional), so a value that looks like a flag (`--system-prompt
// --continue`) is never read as one (#334).
func stepLaunch(argv []string, arity map[string]string, at int) launchStep {
	a := argv[at]
	next, hasNext := "", at+1 < len(argv)
	if hasNext {
		next = argv[at+1]
	}
	switch {
	case a == "--":
		return launchStep{opt: "--", wide: 1}
	case a == "--model" || a == "--effort":
		// #356: the value is the person's own decision, so anything that is
		// neither empty nor the separator is the value, flag-shaped or not.
		if hasNext && next != "" && next != "--" {
			return launchStep{opt: a, value: next, wide: 2}
		}
		return launchStep{opt: a, wide: 1}
	case strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "--effort=") || strings.HasPrefix(a, "--resume=") ||
		strings.HasPrefix(a, "--from-pr=") || strings.HasPrefix(a, "--print="):
		opt, value, _ := strings.Cut(a, "=")
		return launchStep{opt: opt, value: value, inside: true, wide: 1}
	case a == "-r" || a == "--resume":
		// An optional value never takes a flag-shaped token.
		if hasNext && next != "" && !strings.HasPrefix(next, "-") {
			return launchStep{opt: a, value: next, wide: 2}
		}
		return launchStep{opt: a, wide: 1}
	case a == "--dangerously-skip-permissions" || a == "-c" || a == "--continue" || a == "--from-pr" ||
		a == "-p" || a == "--print" || a == "-h" || a == "--help" || a == "-v" || a == "--version" ||
		a == "--bg" || a == "--background":
		return launchStep{opt: a, wide: 1}
	case len(a) > 1 && a[0] == '-':
		if optionStep(arity, a, next, hasNext) == 2 {
			return launchStep{wide: 2}
		}
	}
	return launchStep{wide: 1}
}

// ParseLaunchFlags reads argv with the launch flag grammar (stepLaunch): the
// spellings the launch parses itself plus the option arity table. Every option
// steps over the value it owns, so a value that looks like a flag
// (`--system-prompt --continue`) is never read as one (#334).
func ParseLaunchFlags(routed bool, argv []string, arity map[string]string) (LaunchFlags, error) {
	f := LaunchFlags{AskMissingModel: true}
	var drop []int
	for i := 0; i < len(argv); i++ {
		s := stepLaunch(argv, arity, i)
		switch {
		case s.opt == "--":
			f.AskMissingModel = false
			i = len(argv)
		case s.opt == "--model":
			// The = spelling carries its value inside the token, empty or not;
			// the bare spelling has none to carry.
			if s.inside {
				f.Model = s.value
			}
			if s.value == "" {
				return f, errors.New("--model requires a value")
			}
			// `--model -i` is the interactive pick of a model: the pair leaves
			// the argv (Drop names the --model) and the picker asks.
			if !s.inside && (s.value == "-i" || s.value == "--interactive") {
				f.Interactive = true
				drop = append(drop, i+1)
				break
			}
			f.Model, f.UserModel, f.SkipModel = s.value, true, true
		case s.opt == "--effort":
			// #356: an effort the person typed decides over any saved default.
			// A bare --effort stays on the argv for Claude Code to judge — its
			// levels are its enum, not ccmorph's to validate here.
			f.UserEffort = true
			if s.value != "" || s.inside {
				f.Effort = s.value
			}
		case s.opt == "--dangerously-skip-permissions":
			// Process launch state Claude Code does not restore from a resumed transcript.
			f.Relay = append(f.Relay, s.opt)
		case s.opt == "-r" || s.opt == "--resume":
			f.AskMissingModel, f.Resuming = false, true
			// The = spelling carries its value inside the token, empty or not:
			// the last selector wins, so a final --resume= clears the session
			// named before it (a routed launch reads Resume to pick its model).
			if s.value != "" || s.inside {
				f.Resume = s.value
			}
			// Claude Code cannot restore a routed model on resume, so a routed
			// profile keeps forcing it.
			f.SkipModel = f.SkipModel || !routed
		case s.opt == "-c" || s.opt == "--continue" || (s.opt == "--from-pr" && !s.inside):
			f.AskMissingModel, f.Resuming = false, true
			f.SkipModel = f.SkipModel || !routed
		case s.opt == "--from-pr":
			// Its = spelling marks the launch resuming without forcing the
			// model (pinned by TestLaunchFlags).
			f.AskMissingModel, f.Resuming = false, true
		case s.opt == "-p" || s.opt == "--print" || s.opt == "-h" || s.opt == "--help" ||
			s.opt == "-v" || s.opt == "--version" || s.opt == "--bg" || s.opt == "--background":
			f.AskMissingModel = false
		}
		i += s.wide - 1
	}
	for j := len(drop) - 1; j >= 0; j-- {
		f.Drop = append(f.Drop, drop[j])
	}
	return f, nil
}

// OpensNoSession reports an argv whose own options make Claude Code print and
// exit — its version or help — instead of opening a session: the exemption the
// native key check grants a launch with no session to kill (#255). The walk is
// stepLaunch's: every option steps over the value it owns, so a flag-shaped
// value (`--system-prompt --version`) is a value, never a version request
// (#334). A nil arity table covers the spellings the launch parses itself.
func OpensNoSession(argv []string, arity map[string]string) bool {
	for i := 0; i < len(argv); i++ {
		s := stepLaunch(argv, arity, i)
		switch {
		case s.opt == "--":
			return false
		case s.opt == "-h" || s.opt == "--help" || s.opt == "-v" || s.opt == "--version":
			return true
		}
		i += s.wide - 1
	}
	return false
}

// optionStep is how many tokens a native option owns: 0 unknown, 1 itself, 2 with
// its value. A --spelling=value carries its value inside the token, and the `--`
// separator is never a value — it ends the scan (#405).
func optionStep(arity map[string]string, opt, next string, hasNext bool) int {
	name, embedded := opt, false
	if strings.HasPrefix(opt, "--") {
		if k, _, ok := strings.Cut(opt, "="); ok {
			name, embedded = k, true
		}
	}
	kind := arity[name]
	if kind == "" {
		return 0
	}
	if embedded || !hasNext {
		return 1
	}
	switch kind {
	case "value":
		if next != "--" {
			return 2
		}
	case "optional":
		if next != "" && !strings.HasPrefix(next, "-") {
			return 2
		}
	}
	return 1
}
