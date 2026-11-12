package ctxwindow

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util/argparse"
)

const prog = "context-probe"

// NoProfilesMsg is the one sentence a fresh install hears from every command
// that needs a profile. The cli says it for its own verbs and the probe says it
// for a sweep with nothing to sweep, so it is written once, here.
const NoProfilesMsg = "No profiles yet — run: ccmorph add <name>"

// The full page belongs to the cli, which owns every command's help; a helper
// error keeps only the usage line a script needs and points at that page.
const usageText = `usage: ccmorph probe [profile] [options]
Options and what they do: ccmorph help probe`

// ProbeOptions is every option parseArgs accepts, in the order the help page
// lists them. The page is held to this list so an option cannot ship unhelpful.
var ProbeOptions = []string{"--profile", "--model", "--check", "--jobs", "--deadline", "--probe-timeout",
	"--allow-inference", "--bisect", "--bisect-accepts", "--listing-stdin", "--declared-only", "--probe-all", "--help"}

type args struct {
	profiles, models []string
	check            bool
	deadline         float64
	probeTimeout     float64
	allowInference   bool
	bisect           bool
	bisectAccepts    int
	listingStdin     bool
	declaredOnly     bool
	probeAll         bool
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func positiveInt(name, v string) (int, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(v, "_", "")), 10, 64)
	if err != nil {
		return 0, usageError{fmt.Sprintf("argument %s: invalid positive_int value: '%s'", name, v)}
	}
	if n <= 0 {
		return 0, usageError{fmt.Sprintf("argument %s: must be > 0", name)}
	}
	return int(n), nil
}

func positiveFloat(name, v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, usageError{fmt.Sprintf("argument %s: invalid positive_float value: '%s'", name, v)}
	}
	if !(0 < f && f < math.Inf(1)) {
		return 0, usageError{fmt.Sprintf("argument %s: must be finite and > 0", name)}
	}
	return f, nil
}

// parseArgs is a small argparse: --opt value, --opt=value, unique prefixes.
func parseArgs(argv []string) (*args, error) {
	a := &args{deadline: runTimeout, probeTimeout: probeTimeout, bisectAccepts: BisectMaxAccepts}
	takesValue := map[string]bool{"--profile": true, "--model": true, "--jobs": true, "--deadline": true,
		"--probe-timeout": true, "--bisect-accepts": true}
	var exclusive []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-h" || arg == "--help" {
			return nil, usageError{"help"}
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, usageError{"unrecognized arguments: " + arg}
		}
		name, val, hasVal := strings.Cut(arg, "=")
		match := argparse.Matches(name, ProbeOptions)
		if len(match) > 1 {
			return nil, usageError{fmt.Sprintf("ambiguous option: %s could match %s", name, strings.Join(match, ", "))}
		}
		if len(match) == 0 {
			return nil, usageError{"unrecognized arguments: " + arg}
		}
		name = match[0]
		if takesValue[name] {
			if !hasVal {
				if i+1 >= len(argv) || argparse.OptionLike(argv[i+1]) {
					return nil, usageError{fmt.Sprintf("argument %s: expected one argument", name)}
				}
				i++
				val = argv[i]
			}
		} else if hasVal {
			return nil, usageError{fmt.Sprintf("argument %s: ignored explicit argument '%s'", name, val)}
		}
		var err error
		switch name {
		case "--profile":
			a.profiles = append(a.profiles, val)
		case "--model":
			a.models = append(a.models, val)
		case "--check":
			a.check = true
		case "--jobs":
			_, err = positiveInt(name, val) // compatibility option; discovery is serial
		case "--deadline":
			a.deadline, err = positiveFloat(name, val)
		case "--probe-timeout":
			a.probeTimeout, err = positiveFloat(name, val)
		case "--allow-inference":
			a.allowInference = true
		case "--bisect":
			a.bisect = true
		case "--bisect-accepts":
			a.bisectAccepts, err = positiveInt(name, val)
		case "--listing-stdin":
			a.listingStdin = true
		case "--declared-only", "--probe-all":
			if len(exclusive) > 0 && exclusive[0] != name {
				return nil, usageError{fmt.Sprintf("argument %s: not allowed with argument %s", name, exclusive[0])}
			}
			exclusive = append(exclusive, name)
			a.declaredOnly, a.probeAll = name == "--declared-only", name == "--probe-all"
		}
		if err != nil {
			return nil, err
		}
	}
	return a, nil
}

// ProbeMain runs the helper (argv excludes the program name) and returns its exit code.
func ProbeMain(argv []string, stdin io.Reader, stdout, errw io.Writer) int {
	stderr = errw
	defer func() { stderr = os.Stderr }()
	return run(argv, stdin, stdout, errw)
}

func run(argv []string, stdin io.Reader, stdout, errw io.Writer) int {
	a, err := parseArgs(argv)
	fail := func(msg string) int {
		fmt.Fprintln(errw, usageText)
		fmt.Fprintf(errw, "%s: error: %s\n", prog, msg)
		return 2
	}
	if err != nil {
		if err.Error() == "help" {
			fmt.Fprintln(stdout, usageText)
			return 0
		}
		return fail(err.Error())
	}
	if a.listingStdin {
		if len(a.profiles) != 1 {
			return fail("--listing-stdin requires exactly one --profile")
		}
		body, rerr := io.ReadAll(stdin)
		if rerr != nil {
			fmt.Fprintf(errw, "%s: %s\n", prog, rerr)
			return 1
		}
		return SizeListing(context.Background(), a.profiles[0], string(body),
			os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_API_KEY"), BuildOpener())
	}
	if (a.bisect || a.probeAll) && !a.allowInference {
		return fail("--bisect and --probe-all require --allow-inference (requests can cost money)")
	}
	if a.allowInference && (a.declaredOnly || len(a.profiles) != 1 || len(a.models) != 1) {
		return fail("--allow-inference requires exactly one --profile and one --model, and cannot be combined with --declared-only")
	}
	profiles := a.profiles
	for _, p := range profiles {
		if !ValidProfileName(p) {
			fmt.Fprintf(errw, "%s: invalid profile name\n", prog)
			return 1
		}
	}
	// A run that names no profile sweeps every routed one; when none exists
	// there is nothing to probe, which is not a failure. This is after the
	// arguments are parsed and checked, so an option error still exits 2 and
	// every valid option hears the same answer. `tried` is also what the
	// empty-result report names, so a sweep never lists nothing.
	tried := profiles
	var snaps map[string]*Snapshot
	if tried == nil {
		tried, snaps = routedProfiles()
	}
	if len(tried) == 0 {
		if profiles == nil && len(confPaths(nil)) == 0 {
			fmt.Fprintln(errw, NoProfilesMsg)
		} else {
			fmt.Fprintln(errw, "Nothing to probe.")
		}
		return 0
	}
	mode := "declared-only"
	switch {
	case a.probeAll:
		mode = "probe-all"
	case a.allowInference:
		mode = "auto"
	}
	opts := DefaultOptions()
	opts.Mode, opts.ProbeTimeout, opts.Deadline = mode, a.probeTimeout, a.deadline
	if len(a.models) > 0 {
		opts.OnlySet, opts.Only = true, map[string]bool{}
		for _, m := range a.models {
			opts.Only[m] = true
		}
	}
	if a.bisect {
		opts.Bisect = a.bisectAccepts
	}
	// Ctrl-C ends the run as an interruption, not a crash: ProbeAll's cleanup
	// retires the provider bridge (its process and key-bearing /tmp config),
	// and nothing is written. A launch's --listing-stdin above keeps its own
	// background context: an interrupted launch must not record misses.
	sigctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	started := time.Now()
	opts.Identities = map[string]*Identity{}
	// The selection's read is the one the pinned loop reuses (GH-485).
	opts.Snaps = snaps
	result := ProbeAll(sigctx, tried, opts)
	if sigctx.Err() != nil {
		return 130
	}
	notes := result.Notes
	// The pinned read inside the run already carried each profile's baseline.
	baseline := result.Baseline

	var unknown []string
	seen := map[string]bool{}
	for _, n := range notes {
		if n.Reason == Unresolved && !seen[n.Subject] {
			seen[n.Subject] = true
			unknown = append(unknown, n.Subject)
		}
	}
	sort.Strings(unknown)

	// A profile is sized when it has at least one window to save; one with none
	// still gets its own group in the report.
	sized := map[string]int{}
	for scoped := range result.Values {
		profile, _, _ := strings.Cut(scoped, ":")
		sized[profile]++
	}

	if len(result.Values) == 0 {
		reportProfiles(errw, tried, notes, sized, nil)
		// A model with no size is a result: it runs at Claude Code's default.
		// A note that Failed means the lookup itself broke, and that is exit 1.
		for _, n := range notes {
			if n.Failed {
				return 1
			}
		}
		return 0
	}

	if a.check {
		type drift struct {
			key           string
			before, after string
		}
		var drifts []drift
		changed := map[string]int{}
		for _, k := range slices.Sorted(maps.Keys(result.Values)) {
			v := result.Values[k]
			if b, ok := baseline[k]; !ok || b != v {
				before := "None"
				if ok {
					before = fmt.Sprint(b)
				}
				drifts = append(drifts, drift{k, before, fmt.Sprint(v)})
				profile, _, _ := strings.Cut(k, ":")
				changed[profile]++
			}
		}
		sort.Slice(drifts, func(i, j int) bool { return drifts[i].key < drifts[j].key })
		for _, d := range drifts {
			fmt.Fprintf(stdout, "DRIFT %s: %s -> %s\n", d.key, d.before, d.after)
		}
		if len(drifts) == 0 && len(unknown) == 0 {
			fmt.Fprintln(stdout, "No context-limit changes found.")
		}
		reportProfiles(errw, tried, notes, sized, changed)
		switch {
		case len(drifts) > 0:
			return 2
		case len(unknown) > 0:
			return 1
		}
		return 0
	}

	remaining := a.deadline - time.Since(started).Seconds()
	if remaining <= 0 {
		fmt.Fprintln(errw, "deadline reached before profile write; existing values left unchanged")
		return 1
	}
	ctx, cancel := context.WithTimeout(sigctx, time.Duration(remaining*float64(time.Second)))
	defer cancel()
	committed, cut := Commit(ctx, result.Values, baseline, opts.Identities, result.Sources)
	if sigctx.Err() != nil {
		fmt.Fprintln(errw, "interrupted; completed profile updates were kept")
		return 130
	}
	if cut {
		fmt.Fprintln(errw, "Save deadline reached; completed profile updates were kept, and no partial files were written.")
		return 1
	}
	// What happened, THEN what is still open and how to close it.
	reportProfiles(errw, tried, notes, sized, nil)
	// A run that sized something keeps the exit it had: unsized models are
	// reported, but only a run that found no sizes at all (above) treats them
	// as the result rather than an unfinished lookup. A failed save is a
	// failure either way.
	if committed != 0 || len(unknown) > 0 {
		return 1
	}
	return 0
}
