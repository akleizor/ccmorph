package cli

import (
	"fmt"
	"slices"
	"strings"
)

func rc(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

// pad is a, extended with empty strings to at least n.
func pad(a []string, n int) []string {
	for len(a) < n {
		a = append(a, "")
	}
	return a
}

// stages are the launcher's single-stage entry points: --run-claude and
// --project-picker serve lib/ccmorph-launch.zsh, the rest the zsh and Python tests.
// Each gets the whole argv, args[0] being the stage's own name.
var stages = map[string]func(l *launcher, args []string) int{
	"--run-claude": func(l *launcher, args []string) int { // <env...> -- <claude args...>
		i := slices.Index(args, "--")
		if i < 0 {
			return 2
		}
		return l.runClaude(args[1:i], args[i+1:])
	},
	"--project-picker": func(l *launcher, args []string) int { // <profile> <base> <key> [own] [tag] [launch]
		a := pad(args[1:], 6)
		return rc(l.projectPicker(a[0], a[1], a[2], a[3], a[4], a[5]))
	},
	"--project": func(l *launcher, args []string) int { // <one|pair|model-settings> <profile> [opts...] -- [penv...]
		i := slices.Index(args, "--")
		if i < 3 {
			return 2
		}
		name, opts, penv, op := args[2], args[3:i], args[i+1:], args[1]
		switch op {
		case "one":
			return l.prepareAndProject(name, opts, penv)
		case "pair": // the GH-433 reference: save, then project, as two steps
			op = "prepare-launch"
		}
		saved, code := l.configStore(op, name, "")
		if code != 0 {
			return 10
		}
		return l.projectSettings(name, saved, opts, penv)
	},
	"--adapter-start": func(l *launcher, args []string) int { // <profile> [model]
		a := pad(args[1:], 2)
		return rc(l.adapterStart(a[0], a[1]))
	},
	"--invariant": func(l *launcher, args []string) int { // <profile> [pct] [max-output] [no-1m] [budget] [pct-in-env]
		a := pad(args[1:], 6)
		if a[5] == "" {
			a[5] = "1"
		}
		l.reportInvariant(a[0], strings.Split(l.diagnostics(a[0], a[5], a[1], a[2], a[3], a[4], "", "", "", "", nil), "\x00"))
		return 0
	},
	"--launch-window": func(l *launcher, args []string) int { // <profile> [model]
		a := pad(args[1:], 2)
		fmt.Println(strings.Join(l.launchWindow(a[0], a[1]), "\t"))
		return 0
	},
	"--fold": func(l *launcher, args []string) int { // <profile>: prints "fold" when one ran
		if l.foldSettings(args[1]) {
			fmt.Println("fold")
		}
		return 0
	},
	"--hazard-set": func(l *launcher, args []string) int { // <profile> <budget> <ceiling> <settings> -- [penv...]
		i := slices.Index(args, "--")
		if i < 5 {
			return 2
		}
		if rows := l.hazardSet(args[1], args[2], args[3], args[4], args[i+1:]); rows != "" {
			fmt.Println(rows)
		}
		return 0
	},
	"--migrate-state": func(l *launcher, _ []string) int { return rc(l.migrateState()) },
	"--model-window": func(l *launcher, args []string) int { // <profile> <model>
		if !l.migrateState() {
			return 1
		}
		a := pad(args[1:], 2)
		out := l.coreNoCeiling(l.windowEnv(a[0]), "model-window", a[0], a[1])
		if out == "" {
			return 1
		}
		fmt.Println(out)
		return 0
	},
	"--prelaunch":      func(l *launcher, args []string) int { l.prelaunch(args[1]); return 0 },
	"--remove-profile": func(l *launcher, args []string) int { return rc(l.removeProfileDir(args[1])) },
	// <id> <dst>: rc 1 when nothing was carried
	"--carry-session": func(l *launcher, args []string) int { return rc(l.carrySession(args[1], args[2])) },
	"--models-json": func(l *launcher, args []string) int { // <profile> [base] [key]: the /v1/models listing
		out, ok := l.modelsJSON(args[1], args[2:]...)
		if ok {
			fmt.Println(out)
		}
		return rc(ok)
	},
	// <word>: rc 0 when the installed claude names it
	"--claude-subcommand": func(l *launcher, args []string) int { return rc(l.isClaudeSubcommand(args[1])) },
	"--listing": func(l *launcher, args []string) int { // <base> <key> [<base> <key>...]: one launch's memo
		for i := 1; i+1 < len(args); i += 2 {
			body, ok := l.listing(args[i], args[i+1])
			if !ok {
				return 1
			}
			fmt.Println(body)
		}
		return 0
	},
	"--resume-model": func(l *launcher, args []string) int { // <session id>: the model it last ran
		m, ok := l.resumeModel(args[1])
		if ok {
			fmt.Println(m)
		}
		return rc(ok)
	},
	"--trigger-env": func(l *launcher, args []string) int { // <KEY=VALUE>...: pct, output reserve, 1M switch
		pct, maxout, no1m := l.triggerEnv(args[1:])
		fmt.Printf("%s\n%s\n%s\n", pct, maxout, no1m)
		return 0
	},
	"--probe-nudge": func(l *launcher, args []string) int { l.probeNudge(args[1]); return 0 },
	"--log":         func(l *launcher, args []string) int { l.log(strings.Join(args[1:], " ")); return 0 },
}
