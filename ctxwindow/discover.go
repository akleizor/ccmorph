package ctxwindow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/pycompat"
)

// Note is (reason, subject): what a run learned about a profile or model.
type Note struct{ Reason, Subject string }

// RouteUnavailable means ccmorph could not produce a reachable endpoint for a profile.
type RouteUnavailable struct{ Msg string }

func (e *RouteUnavailable) Error() string { return e.Msg }

// runDeadline escapes nested request handlers without treating a clock as a
// model limit; it is raised by panic and recovered where the deadline is armed.
type runDeadline struct{}

func check(ctx context.Context) {
	if ctx.Err() != nil {
		panic(runDeadline{})
	}
}

// runWithDeadline runs fn under a deadline and reports whether it was cut short.
func runWithDeadline(parent context.Context, seconds float64, fn func(ctx context.Context)) (expired bool) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds*float64(time.Second)))
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runDeadline); !ok {
				panic(r)
			}
			expired = true
		}
	}()
	fn(ctx)
	return false
}

// runZsh runs a zsh snippet that sources lib/ccmorph.sh; the whole process
// group dies with the context, so a route that hangs cannot outlive its deadline.
func runZsh(ctx context.Context, script string, args ...string) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, "zsh", append([]string{"-c", script, "context-probe"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errb.String(), ee.ExitCode(), nil
	}
	return out.String(), errb.String(), 0, err
}

// ccmorphEnv asks ccmorph, the only definition, for a profile's endpoint and
// credential. It uses _route_env rather than _env: a provider profile has no
// endpoint until its bridge runs, and only _route_env starts one. It hands back
// a handle to retire with routeStop.
var ccmorphEnv = func(ctx context.Context, profile string) (map[string]string, error) {
	// Starting a provider bridge is not a local read.
	cctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	out, errOut, code, err := runZsh(cctx, `source "$1" && ccmorph _route_env "$2"`, ccmorphScript(), profile)
	check(ctx)
	if err != nil || cctx.Err() != nil {
		if cctx.Err() != nil {
			return nil, &RouteUnavailable{"route setup timed out after 180 seconds"}
		}
		return nil, &RouteUnavailable{err.Error()}
	}
	if code != 0 {
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = "ccmorph could not resolve a route for " + profile
		}
		return nil, &RouteUnavailable{msg}
	}
	env := map[string]string{}
	for _, line := range pycompat.SplitLines(out) {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

// routeStop retires a route opened by ccmorphEnv. Best effort: a route left up
// is reused by the next caller, so failing to close one must never fail a run
// that already got its numbers.
var routeStop = func(ctx context.Context, handle string) {
	if handle == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	runZsh(cctx, `source "$1" && ccmorph _route_stop "$2"`, ccmorphScript(), handle)
	check(ctx)
}

var (
	listModelsFn = ListModels
	probeFn      = Probe
	payloadFn    = payload
)

func confFields(path string) (fields map[string]string, ok bool) {
	e := pycompat.Catch(func() { _, fields = ReadConf(path) })
	return fields, e == nil
}

func confPaths(profiles []string) []string {
	var paths []string
	if profiles != nil {
		for _, p := range profiles {
			paths = append(paths, ProfilePath(p))
		}
		return paths
	}
	dir := StateDir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".conf") {
			paths = append(paths, pycompat.Join(dir, e.Name()))
		}
	}
	return paths
}

func stemOf(path string) string {
	return strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".conf")
}

// priorValues are the stored `profile:model` windows.
var priorValues = func(profiles []string) map[string]int64 {
	values := map[string]int64{}
	for _, path := range confPaths(profiles) {
		stem := stemOf(path)
		if !ValidProfileName(stem) {
			continue
		}
		fields, ok := confFields(path)
		if !ok {
			continue
		}
		d := DiscoveredValues(fields)
		for _, m := range d.Keys() {
			values[stem+":"+m], _ = d.Get(m)
		}
	}
	return values
}

// priorCatalogueTags maps `profile:model` to its stored provenance tag for every
// catalogue-derived (not verified) baseline entry. It doubles as the "is this
// key catalogue-derived" test and as the exact tag to carry forward when a run
// repeats a prior value, so an unverified guess never reads as verified.
var priorCatalogueTags = func(profiles []string) map[string]*obj {
	tags := map[string]*obj{}
	for _, path := range confPaths(profiles) {
		stem := stemOf(path)
		if !ValidProfileName(stem) {
			continue
		}
		fields, ok := confFields(path)
		if !ok {
			continue
		}
		s := DiscoveredSources(fields)
		for _, m := range s.Keys() {
			tags[stem+":"+m], _ = s.Get(m)
		}
	}
	return tags
}

// routedProfiles are the profiles with a routed endpoint: every one has a listing to size.
func routedProfiles() []string {
	var names []string
	dir := StateDir()
	entries, _ := os.ReadDir(dir)
	var confs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".conf") {
			confs = append(confs, e.Name())
		}
	}
	sort.Strings(confs)
	for _, name := range confs {
		fields, ok := confFields(pycompat.Join(dir, name))
		if !ok {
			continue
		}
		switch fields["type"] {
		case "apikey", "proxy", "provider":
			names = append(names, strings.TrimSuffix(name, ".conf"))
		}
	}
	return names
}

// Run is the outcome of one discovery pass.
type Run struct {
	Values  *pycompat.OMap[int64]
	Notes   []Note
	Sources map[string]*obj
}

// Options configures ProbeAll.
type Options struct {
	Mode         string // "declared-only", "auto" or "probe-all"
	Only         map[string]bool
	OnlySet      bool
	ProbeTimeout float64
	Deadline     float64
	Bisect       int
	Identities   map[string]*Identity
}

// DefaultOptions are the CLI defaults.
func DefaultOptions() Options {
	return Options{Mode: "declared-only", ProbeTimeout: probeTimeout, Deadline: runTimeout}
}

func closeMatches(word string, models []string) string {
	return ClosestMatch(word, models)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func pinnedValue(s string) bool { return isDigits(s) && strings.Trim(s, "0") != "" }

// ProbeAll discovers windows for profiles (all routed ones when nil).
func ProbeAll(parent context.Context, profiles []string, o Options) Run {
	values := pycompat.NewOMap[int64]()
	sources := map[string]*obj{}
	var notes []Note
	baseline := priorValues(profiles)
	catalogueTags := priorCatalogueTags(profiles)
	cache := NewCache(false)
	var blob string
	handle := ""
	cleanupBudget := o.Deadline / 10
	if cleanupBudget > 1 {
		cleanupBudget = 1
	}

	stopRoute := func(ctx context.Context) {
		closing := handle
		handle = ""
		routeStop(ctx, closing)
	}

	expired := runWithDeadline(parent, o.Deadline-cleanupBudget, func(ctx context.Context) {
		if profiles == nil {
			profiles = routedProfiles()
		}
		for _, profile := range profiles {
			if o.Identities != nil {
				o.Identities[profile] = ProfileIdentity(profile)
				baseline = priorValues([]string{profile})
				catalogueTags = priorCatalogueTags([]string{profile})
			}
			env, err := ccmorphEnv(ctx, profile)
			check(ctx)
			if err != nil {
				var ru *RouteUnavailable
				if errors.As(err, &ru) {
					notes = append(notes, Note{strings.TrimSpace(strings.ReplaceAll(ru.Msg, "ccmorph:", "")), profile})
					continue
				}
				panic(err)
			}
			handle = env["CCMORPH_ROUTE_HANDLE"]
			key := env["ANTHROPIC_API_KEY"]
			if key == "" {
				notes = append(notes, Note{"profile has no api key, skipped", profile})
				stopRoute(ctx)
				continue
			}
			base, ok := env["ANTHROPIC_BASE_URL"]
			if !ok {
				base = "https://api.anthropic.com"
			}
			op := BuildOpener()
			lt := o.ProbeTimeout
			if lt > listTimeout {
				lt = listTimeout
			}
			listTO := time.Duration(lt * float64(time.Second))
			models, declared, listingRows, err := listModelsFn(ctx, base, key, listTO, op)
			check(ctx)
			if err != nil {
				notes = append(notes, Note{WhyUnreachable(err, base), profile})
				if o.Mode == "declared-only" && o.OnlySet && len(o.Only) > 0 {
					// Explicit model selection is enough for a catalogue lookup; an
					// unavailable gateway is not evidence of model absence.
					models = sortedKeys(o.Only)
					declared, listingRows = pycompat.NewOMap[int64](), nil
					fmt.Fprintf(stderr, "[%s] gateway metadata unavailable; trying catalogue for requested models\n", profile)
				} else {
					stopRoute(ctx)
					continue
				}
			}
			fields, ok := confFields(ProfilePath(profile))
			if !ok {
				fields = map[string]string{}
			}
			if o.OnlySet {
				have := map[string]bool{}
				for _, m := range models {
					have[m] = true
				}
				var wantedMissing []string
				for w := range o.Only {
					if !have[w] {
						wantedMissing = append(wantedMissing, w)
					}
				}
				sort.Strings(wantedMissing)
				for _, wanted := range wantedMissing {
					var near string
					for _, m := range models {
						if strings.Contains(strings.ToLower(m), strings.ToLower(wanted)) {
							near = m
							break
						}
					}
					if near == "" {
						near = closeMatches(wanted, models)
					}
					hint := ""
					if near != "" {
						hint = "; did you mean " + near + "?"
					}
					notes = append(notes, Note{fmt.Sprintf("'%s' is not on this route%s", wanted, hint), profile})
				}
				var kept []string
				for _, m := range models {
					if o.Only[m] {
						kept = append(kept, m)
					}
				}
				models = kept
			} else {
				// The operator already hid these from the picker; a model nobody can
				// select needs no window, and listing it as unresolved is noise.
				excluded := map[string]bool{}
				for _, m := range strings.Split(fields["excluded_models"], ",") {
					excluded[m] = true
				}
				var kept []string
				for _, m := range models {
					if !excluded[m] {
						kept = append(kept, m)
					}
				}
				models = kept
			}
			inModels := map[string]bool{}
			pinned := map[string]bool{}
			for _, m := range models {
				inModels[m] = true
				if o.Mode != "probe-all" && pinnedValue(fields["context_window."+m]) {
					pinned[m] = true
				}
			}
			sized := pycompat.NewOMap[int64]()
			for _, m := range declared.Keys() {
				if v, _ := declared.Get(m); inModels[m] && !pinned[m] {
					sized.Set(m, v)
				}
			}
			raised, pending := ProviderWindows(ctx, sized, listingRows, fields, listTO, cache)
			check(ctx)
			declared = pycompat.Merge(declared, raised)
			ownedBy := map[string]string{}
			for _, row := range listingRows {
				if ob, ok := asStr(getv(row, "owned_by")); ok {
					id, _ := asStr(getv(row, "id"))
					ownedBy[id] = ob
				}
			}
			for _, model := range models {
				check(ctx)
				if pending[model] {
					notes = append(notes, Note{"provider catalogue unreachable; size left for the next run", model})
					continue
				}
				pin := fields["context_window."+model]
				if pinnedValue(pin) && o.Mode != "probe-all" {
					notes = append(notes, Note{"manual context override configured; not probed", model})
					continue
				}
				scoped := profile + ":" + model
				known, haveKnown := baseline[scoped]
				priorTag := catalogueTags[scoped]
				// A prior catalogue guess is not verified: a fresh declared value this
				// run always supersedes it, unlike a real probed/declared value, which
				// stays pinned.
				if o.Mode != "probe-all" && haveKnown && known != 0 && !(priorTag != nil && declared.Has(model)) {
					values.Set(scoped, known)
					if priorTag != nil {
						// Repeating the same value must not look like a fresh unsourced
						// (therefore "verified") write: carry the tag forward exactly.
						sources[scoped] = priorTag
					}
					if d, ok := declared.Get(model); ok && d != known {
						reason := fmt.Sprintf("kept saved limit %s; gateway now declares %s", commas(known), commas(d))
						notes = append(notes, Note{reason, model})
						fmt.Fprintf(stderr, "  %s: %s\n", model, reason)
					}
					continue
				}
				if d, ok := declared.Get(model); ok && o.Mode != "probe-all" {
					values.Set(scoped, d)
					continue
				}
				if o.Mode == "declared-only" {
					found := CatalogueLookup(ctx, model, ownedBy[model], cache, catalogueTimeout*time.Second)
					check(ctx)
					if found != nil {
						values.Set(scoped, found.Value)
						sources[scoped] = CatalogueTag(found)
					} else {
						notes = append(notes, Note{Unresolved, model})
					}
					continue
				}
				fmt.Fprintf(stderr, "[%s] %s: paid probe enabled. The first %s-character prompt may be billed in full.\n"+
					"  Confirmation and search requests add cost; rates and tokenization "+
					"depend on the gateway. A timeout does not undo billing.\n", profile, model, commas(OvershootChars))
				if blob == "" {
					blob = payloadFn(OvershootChars)
				}
				res := probeFn(ctx, base, key, model, blob, time.Duration(o.ProbeTimeout*float64(time.Second)), op, o.Bisect)
				check(ctx)
				if (strings.HasSuffix(res.Kind, ":declared") || strings.HasSuffix(res.Kind, ":truncated") || strings.HasSuffix(res.Kind, ":bisected")) && res.Value > 0 {
					values.Set(scoped, res.Value)
				} else {
					why := res.Note
					if why == "" {
						why = res.Kind
					}
					notes = append(notes, Note{why, model})
				}
				shown := "-"
				if res.Value != 0 {
					shown = fmt.Sprint(res.Value)
				}
				fmt.Fprintf(stderr, "  %s %s %s\n", pad(model, 30), pad(res.Kind, 12), shown)
				if res.Note != "" {
					fmt.Fprintf(stderr, "    %s\n", res.Note)
				}
			}
			stopRoute(ctx)
		}
	})
	if expired {
		reason := fmt.Sprintf("%s discovery deadline reached; existing values are preserved", gFormat(o.Deadline))
		if parent.Err() != nil {
			reason = "interrupted; existing values are preserved"
		}
		notes = append(notes, Note{reason, "discovery"})
		fmt.Fprintln(stderr, notes[len(notes)-1].Reason)
	}
	if handle != "" {
		if runWithDeadline(context.Background(), cleanupBudget, func(ctx context.Context) { stopRoute(ctx) }) {
			fmt.Fprintln(stderr, "route cleanup timed out; a provider bridge may still be running")
		}
	}
	return Run{values, notes, sources}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
