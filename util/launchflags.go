package util

import (
	"errors"
	"strings"
)

// LaunchFlags is what the launch needs from Claude Code's own argv: model (an
// explicit --model), UserModel, SkipModel, AskMissingModel, Resume, Interactive,
// Relay (--dangerously-skip-permissions, once per occurrence) and Drop (the 1-based
// indexes of `--model -i` pairs to remove, highest first).
type LaunchFlags struct {
	Model, Resume                                      string
	UserModel, SkipModel, AskMissingModel, Interactive bool
	Relay                                              []string
	Drop                                               []int
}

// ParseLaunchFlags reads argv with the option arity table (spelling to flag, value
// or optional). Every other option steps over the value it owns, so a value that
// looks like a flag (`--system-prompt --continue`) is never read as one (#334).
func ParseLaunchFlags(routed bool, argv []string, arity map[string]string) (LaunchFlags, error) {
	f := LaunchFlags{AskMissingModel: true}
	var drop []int
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next, hasNext := "", i+1 < len(argv)
		if hasNext {
			next = argv[i+1]
		}
		switch {
		case a == "--":
			f.AskMissingModel = false
			i = len(argv)
		case a == "--model":
			if !hasNext || next == "" || next == "--" {
				return f, errors.New("--model requires a value")
			}
			if next == "-i" || next == "--interactive" {
				f.Interactive = true
				drop = append(drop, i+1)
				i++
				continue
			}
			f.Model, f.UserModel, f.SkipModel = next, true, true
			i++
		case strings.HasPrefix(a, "--model="):
			f.Model = strings.TrimPrefix(a, "--model=")
			if f.Model == "" {
				return f, errors.New("--model requires a value")
			}
			f.UserModel, f.SkipModel = true, true
		case a == "--dangerously-skip-permissions":
			// Process launch state Claude Code does not restore from a resumed transcript.
			f.Relay = append(f.Relay, a)
		case a == "-r" || a == "--resume":
			f.AskMissingModel = false
			if hasNext && next != "" && !strings.HasPrefix(next, "-") {
				f.Resume = next
			}
			// Claude Code cannot restore a routed model on resume, so a routed
			// profile keeps forcing it.
			f.SkipModel = f.SkipModel || !routed
		case strings.HasPrefix(a, "--resume="):
			f.AskMissingModel, f.Resume = false, strings.TrimPrefix(a, "--resume=")
			f.SkipModel = f.SkipModel || !routed
		case a == "-c" || a == "--continue" || a == "--from-pr":
			f.AskMissingModel = false
			f.SkipModel = f.SkipModel || !routed
		case a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") || a == "-h" || a == "--help" ||
			a == "-v" || a == "--version" || a == "--bg" || a == "--background" || strings.HasPrefix(a, "--from-pr="):
			f.AskMissingModel = false
		case len(a) > 1 && a[0] == '-':
			if optionStep(arity, a, next, hasNext) == 2 {
				i++
			}
		}
	}
	for j := len(drop) - 1; j >= 0; j-- {
		f.Drop = append(f.Drop, drop[j])
	}
	return f, nil
}

// optionStep is how many tokens a native option owns: 0 unknown, 1 itself, 2 with
// its value. A --spelling=value carries its value inside the token.
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
		return 2
	case "optional":
		if next != "" && !strings.HasPrefix(next, "-") {
			return 2
		}
	}
	return 1
}
