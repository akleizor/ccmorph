package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"io"
	"os"
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
		l.reportInvariant(a[0], l.diagnostics(a[0], a[5] == "1", a[1], a[2], a[3], a[4], "", "", false, "", nil))
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
		if rows := l.hazardSet(args[1], args[2], args[3], args[4], args[i+1:]); len(rows) > 0 {
			fmt.Println(strings.Join(rows, "\n"))
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
	"--model-known":   func(l *launcher, args []string) int { return l.modelKnown(args[1], args[2]) }, // <profile> <model>: 0 served, 1 not, 2 cannot tell
	"--profile-ok":    func(l *launcher, args []string) int { return rc(util.ProfileOK(l.profileDirs, args[1])) },
	"--cleanup-stale": func(l *launcher, _ []string) int { util.GCStale(l.dir, l.profileDirs, tmpDir()); return 0 },
	// <op> <profile> [key]: the store's text; the change and materialize argv come
	// from _CCM_CONFIG_{MODEL,EFFORT,SNAPSHOT,TYPE,ENDPOINT,CREDENTIAL} and _CCM_MATERIALIZE
	"--config-store": func(l *launcher, args []string) int {
		a := l.storeArgs(args[1], args[2], pad(args[3:], 1)[0])
		a.Materialize = os.Getenv("_CCM_MATERIALIZE")
		a.Change = util.StoreChange{Model: os.Getenv("_CCM_CONFIG_MODEL"), Effort: os.Getenv("_CCM_CONFIG_EFFORT"),
			Snapshot: os.Getenv("_CCM_CONFIG_SNAPSHOT"), Type: os.Getenv("_CCM_CONFIG_TYPE"),
			Endpoint: os.Getenv("_CCM_CONFIG_ENDPOINT"), Credential: os.Getenv("_CCM_CONFIG_CREDENTIAL")}
		out, rc := util.ConfigStore(a, os.Stderr)
		fmt.Print(out)
		return rc
	},
	"--listing-has-unsized": func(l *launcher, args []string) int { return rc(util.ListingHasUnsized(l.conf(args[1]), args[2])) }, // <profile> <listing json>
	"--gw-models": func(l *launcher, args []string) int { // <profile> <picker|catalogue>, the listing on stdin: its chat models
		body, _ := io.ReadAll(os.Stdin)
		rows, ok := util.GwModels(l.conf(args[1]), args[2] == "picker", string(body))
		if ok {
			fmt.Println(modelRows(rows))
		}
		return rc(ok)
	},
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

// stageOperands is each stage's least argv, its own name included, and the rest of
// its usage line. A stage called short refuses with that line instead of indexing
// past its argv (#162).
var stageOperands = map[string]struct {
	min   int
	usage string
}{
	"--run-claude":          {1, "[env...] -- [claude args...]"},
	"--project-picker":      {3, "<profile> <base> <key> [own] [tag] [launch]"},
	"--project":             {3, "<one|pair|model-settings> <profile> [opts...] -- [penv...]"},
	"--adapter-start":       {1, "<profile> [model]"},
	"--invariant":           {1, "<profile> [pct] [max-output] [no-1m] [budget] [pct-in-env]"},
	"--launch-window":       {1, "<profile> [model]"},
	"--fold":                {1, "<profile>"},
	"--hazard-set":          {5, "<profile> <budget> <ceiling> <settings> -- [penv...]"},
	"--migrate-state":       {0, ""},
	"--model-window":        {2, "<profile> <model>"},
	"--prelaunch":           {1, "<profile>"},
	"--remove-profile":      {1, "<profile>"},
	"--carry-session":       {2, "<id> <dst>"},
	"--models-json":         {1, "<profile> [base] [key]"},
	"--model-known":         {2, "<profile> <model>"},
	"--profile-ok":          {1, "<profile>"},
	"--cleanup-stale":       {0, ""},
	"--config-store":        {2, "<op> <profile> [key]"},
	"--listing-has-unsized": {2, "<profile> <listing json>"},
	"--gw-models":           {2, "<profile> <picker|catalogue>"},
	"--claude-subcommand":   {1, "<word>"},
	"--listing":             {0, "[<base> <key>...]"},
	"--resume-model":        {1, "<session id>"},
	"--trigger-env":         {0, "[KEY=VALUE...]"},
	"--probe-nudge":         {1, "<profile>"},
	"--log":                 {0, "[message...]"},
}

// stageShort is the usage line for a stage invoked without its operands.
func stageShort(args []string) (string, bool) {
	op := stageOperands[args[0]]
	if len(args)-1 >= op.min {
		return "", false
	}
	return strings.TrimSpace("usage: ccmorph-core launch " + args[0] + " " + op.usage), true
}
