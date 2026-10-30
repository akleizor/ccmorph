package util

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// launchFlags is `util launch-flags <routed 0|1> <claude args...>` with the option
// arity table (`<spelling> <flag|value|optional>` lines, from claude-opt-arities)
// on stdin. It reads what the launch needs from Claude Code's own argv and prints
// `key=value` lines: model (an explicit --model), user_model, skip_model,
// ask_missing_model, resume, interactive, relay (--dangerously-skip-permissions,
// once per occurrence) and drop (the 1-based index of a `--model -i` pair to
// remove). Every other option steps over the value it owns, so a value that looks
// like a flag (`--system-prompt --continue`) is never read as one (#334).
func launchFlags(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 1 {
		return 2
	}
	routed := args[0] == "1"
	argv := args[1:]
	arity := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok {
			arity[k] = v
		}
	}
	model, resume := "", ""
	user, skip, ask, interactive := false, false, true, false
	var relay []string
	var drop []int
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next, hasNext := "", i+1 < len(argv)
		if hasNext {
			next = argv[i+1]
		}
		switch {
		case a == "--":
			ask = false
			i = len(argv)
		case a == "--model":
			if !hasNext || next == "" || next == "--" {
				fmt.Fprintln(errw, "--model requires a value")
				return 2
			}
			if next == "-i" || next == "--interactive" {
				interactive = true
				drop = append(drop, i+1)
				i++
				continue
			}
			model, user, skip = next, true, true
			i++
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
			if model == "" {
				fmt.Fprintln(errw, "--model requires a value")
				return 2
			}
			user, skip = true, true
		case a == "--dangerously-skip-permissions":
			// Process launch state Claude Code does not restore from a resumed transcript.
			relay = append(relay, a)
		case a == "-r" || a == "--resume":
			ask = false
			if hasNext && next != "" && !strings.HasPrefix(next, "-") {
				resume = next
			}
			// Claude Code cannot restore a routed model on resume, so a routed
			// profile keeps forcing it.
			skip = skip || !routed
		case strings.HasPrefix(a, "--resume="):
			ask, resume = false, strings.TrimPrefix(a, "--resume=")
			skip = skip || !routed
		case a == "-c" || a == "--continue" || a == "--from-pr":
			ask = false
			skip = skip || !routed
		case a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") || a == "-h" || a == "--help" ||
			a == "-v" || a == "--version" || a == "--bg" || a == "--background" || strings.HasPrefix(a, "--from-pr="):
			ask = false
		case len(a) > 1 && a[0] == '-':
			if optionStep(arity, a, next, hasNext) == 2 {
				i++
			}
		}
	}
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	fmt.Fprintf(out, "model=%s\nuser_model=%d\nskip_model=%d\nask_missing_model=%d\nresume=%s\ninteractive=%d\n",
		model, b(user), b(skip), b(ask), resume, b(interactive))
	for _, r := range relay {
		fmt.Fprintln(out, "relay="+r)
	}
	for j := len(drop) - 1; j >= 0; j-- {
		fmt.Fprintf(out, "drop=%d\n", drop[j])
	}
	return 0
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
