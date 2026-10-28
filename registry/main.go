package registry

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/modelwindow"
	"github.com/akleizor/ccmorph/pycompat"
)

const prog = "refresh-model-registry"

const usageText = `usage: refresh-model-registry [-h] [--profile PROFILES] [--model MODELS]
                              [--check] [--jobs JOBS] [--deadline SECONDS]
                              [--probe-timeout SECONDS] [--allow-inference]
                              [--bisect] [--bisect-accepts N] [--listing-stdin]
                              [--declared-only | --probe-all]`

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
	flags := []string{"--profile", "--model", "--check", "--jobs", "--deadline", "--probe-timeout",
		"--allow-inference", "--bisect", "--bisect-accepts", "--listing-stdin", "--declared-only", "--probe-all", "--help"}
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
		var match []string
		for _, f := range flags {
			if f == name {
				match = []string{f}
				break
			}
			if strings.HasPrefix(f, name) {
				match = append(match, f)
			}
		}
		if len(match) > 1 {
			return nil, usageError{fmt.Sprintf("ambiguous option: %s could match %s", name, strings.Join(match, ", "))}
		}
		if len(match) == 0 {
			return nil, usageError{"unrecognized arguments: " + arg}
		}
		name = match[0]
		if takesValue[name] {
			if !hasVal {
				if i+1 >= len(argv) || (strings.HasPrefix(argv[i+1], "-") && len(argv[i+1]) > 1 && !isNumber(argv[i+1])) {
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

func isNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

// Main runs the helper (argv excludes the program name) and returns its exit code.
func Main(argv []string, stdin io.Reader, stdout, errw io.Writer) (code int) {
	stderr = errw
	defer func() { stderr = os.Stderr }()
	if e := pycompat.Catch(func() { code = run(argv, stdin, stdout, errw) }); e != nil {
		if e.Kind == pycompat.Other {
			fmt.Fprintf(errw, "%s: %s: %s\n", prog, e.Class, e.Msg)
		} else {
			fmt.Fprintf(errw, "%s: %s\n", prog, e.Msg)
		}
		return 1
	}
	return code
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
			pycompat.RaiseOS(rerr)
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
	baseline := priorValues(profiles)
	started := time.Now()
	opts.Identities = map[string]*modelwindow.Identity{}
	result := ProbeAll(context.Background(), profiles, opts)
	notes := result.Notes

	var unknown []string
	seen := map[string]bool{}
	for _, n := range notes {
		if n.Reason == Unresolved && !seen[n.Subject] {
			seen[n.Subject] = true
			unknown = append(unknown, n.Subject)
		}
	}
	sort.Strings(unknown)

	if result.Values.Len() == 0 {
		// What went wrong, then what was protected, then the fix: one line each.
		var order []string
		groups := map[string][]string{}
		for _, n := range notes {
			if n.Reason == Unresolved {
				continue
			}
			if _, ok := groups[n.Reason]; !ok {
				order = append(order, n.Reason)
			}
			groups[n.Reason] = append(groups[n.Reason], n.Subject)
		}
		fmt.Fprintln(errw, "Nothing discovered.")
		for _, reason := range order {
			subjects := groups[reason]
			// Grouped by reason: 46 models failing the same way is one line, not 46.
			var line string
			if len(subjects) == 1 {
				line = reason
				if !strings.Contains(reason, subjects[0]) {
					line = subjects[0] + ": " + reason
				}
			} else {
				sorted := append([]string(nil), subjects...)
				sort.Strings(sorted)
				names := strings.Join(sorted[:min(4, len(sorted))], ", ")
				if len(subjects) > 4 {
					names += fmt.Sprintf(", and %d more", len(subjects)-4)
				}
				line = fmt.Sprintf("%s (%d): %s", reason, len(subjects), names)
			}
			fmt.Fprintf(errw, "  %s\n", line)
		}
		if len(notes) == 0 {
			fmt.Fprintf(errw, "  no route produced a model listing (profiles tried: %s)\n", strings.Join(profiles, ", "))
		}
		if len(baseline) > 0 {
			fmt.Fprintf(errw, "%d saved limit(s) untouched.\n", len(baseline))
		}
		if len(unknown) > 0 {
			ReportUnresolved(errw, unknown, profiles)
		}
		return 1
	}

	if a.check {
		type drift struct {
			key           string
			before, after string
		}
		var drifts []drift
		for _, k := range result.Values.Keys() {
			v, _ := result.Values.Get(k)
			if b, ok := baseline[k]; !ok || b != v {
				before := "None"
				if ok {
					before = fmt.Sprint(b)
				}
				drifts = append(drifts, drift{k, before, fmt.Sprint(v)})
			}
		}
		sort.Slice(drifts, func(i, j int) bool { return drifts[i].key < drifts[j].key })
		for _, d := range drifts {
			fmt.Fprintf(stdout, "DRIFT %s: %s -> %s\n", d.key, d.before, d.after)
		}
		if len(drifts) == 0 && len(unknown) == 0 {
			fmt.Fprintln(stdout, "No context-limit changes found.")
		}
		if len(unknown) > 0 {
			ReportUnresolved(errw, unknown, profiles)
		}
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(remaining*float64(time.Second)))
	defer cancel()
	committed, cut := Commit(ctx, result.Values, baseline, opts.Identities, result.Sources)
	if cut {
		fmt.Fprintln(errw, "Save deadline reached; completed profile updates were kept, and no partial files were written.")
		return 1
	}
	// What happened, THEN what is still open and how to close it.
	if len(unknown) > 0 {
		ReportUnresolved(errw, unknown, profiles)
	}
	if committed != 0 || len(unknown) > 0 {
		return 1
	}
	return 0
}
