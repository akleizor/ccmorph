package ctxwindow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/paths"
)

// Note is what a run learned about a profile or model. Profile is the profile
// the note belongs to, or "" for a note about the whole run; Failed marks
// something that could not be looked up at all, which is not the same as a
// working profile whose models publish no size.
type Note struct {
	Reason  string
	Subject string
	Profile string
	Failed  bool
}

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
	out, errOut, code, err := runZsh(cctx, `source "$1" && ccmorph _route_env "$2"`, paths.Script(), profile)
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
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

// routeStop retires a route opened by ccmorphEnv. Best effort: a route left up
// is reused by the next caller, so failing to close one must never fail a run
// that already got its numbers. It keeps its own 30-second budget and ignores
// the caller's cancellation, because a Ctrl-C or a deadline arriving mid-stop
// must not abort the one step that retires the bridge process and its
// key-bearing /tmp config — and ProbeAll clears its handle before calling here,
// so nothing retries a stop that was skipped.
var routeStop = func(ctx context.Context, handle string) {
	if handle == "" {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	runZsh(cctx, `source "$1" && ccmorph _route_stop "$2"`, paths.Script(), handle)
	check(ctx)
}

var (
	listModelsFn = ListModels
	probeFn      = Probe
	payloadFn    = payload
)

func confFields(path string) (fields map[string]string, ok bool) {
	_, fields, err := ReadConf(path)
	return fields, err == nil
}

func confPaths(profiles []string) []string {
	var paths []string
	if profiles != nil {
		for _, p := range profiles {
			if path, err := ProfilePath(p); err == nil {
				paths = append(paths, path)
			}
		}
		return paths
	}
	return confScan(StateDir())
}

// priorValues are the stored `profile:model` windows.
var priorValues = func(profiles []string) map[string]int64 {
	values := map[string]int64{}
	for _, path := range confPaths(profiles) {
		profile := stem(path)
		if !ValidProfileName(profile) {
			continue
		}
		fields, ok := confFields(path)
		if !ok {
			continue
		}
		d := DiscoveredValues(fields)
		for _, m := range slices.Sorted(maps.Keys(d)) {
			values[profile+":"+m], _ = d[m]
		}
	}
	return values
}

// priorCatalogueTags maps `profile:model` to its stored provenance tag for every
// catalogue-derived (not verified) baseline entry. It doubles as the "is this
// key catalogue-derived" test and as the exact tag to carry forward when a run
// repeats a prior value, so an unverified guess never reads as verified.
var priorCatalogueTags = func(profiles []string) map[string]obj {
	tags := map[string]obj{}
	for _, path := range confPaths(profiles) {
		profile := stem(path)
		if !ValidProfileName(profile) {
			continue
		}
		fields, ok := confFields(path)
		if !ok {
			continue
		}
		s := DiscoveredSources(fields)
		for _, m := range slices.Sorted(maps.Keys(s)) {
			tags[profile+":"+m], _ = s[m]
		}
	}
	return tags
}

// routedProfiles are the profiles with a routed endpoint, each with the one
// snapshot of it the probe reuses instead of reading again: every routed
// profile has a listing to size. The type never comes from the sidecar, whose
// merge only sets context_discovered.
func routedProfiles() ([]string, map[string]*Snapshot) {
	var names []string
	snaps := map[string]*Snapshot{}
	dir := StateDir()
	for _, path := range confScan(dir) {
		snap, err := snapshotOf(path)
		if err != nil {
			continue
		}
		switch snap.Fields["type"] {
		case "apikey", "proxy", "provider":
			s := stem(path)
			names = append(names, s)
			snaps[s] = snap
		}
	}
	return names, snaps
}

// Run is the outcome of one discovery pass.
type Run struct {
	Values   map[string]int64
	Notes    []Note
	Sources  map[string]obj
	Baseline map[string]int64
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
	// Snaps is the selection's one read of each profile; the pinned loop
	// reuses it instead of reading the conf again (GH-485).
	Snaps map[string]*Snapshot
}

// DefaultOptions are the CLI defaults.
func DefaultOptions() Options {
	return Options{Mode: "declared-only", ProbeTimeout: probeTimeout, Deadline: runTimeout}
}

func closeMatches(word string, models []string) string {
	return ClosestMatch(word, models)
}

func pinnedValue(s string) bool { return ordjson.IsDigits(s) && strings.Trim(s, "0") != "" }

// ProbeAll discovers windows for profiles (all routed ones when nil).
func ProbeAll(parent context.Context, profiles []string, o Options) Run {
	values := map[string]int64{}
	sources := map[string]obj{}
	var notes []Note
	baseline := map[string]int64{}
	catalogueTags := map[string]obj{}
	if o.Identities == nil {
		// The entry sweep reads every profile once up front. The pinned path
		// below reads each profile once itself, so it skips this sweep (GH-485).
		baseline = priorValues(profiles)
		catalogueTags = priorCatalogueTags(profiles)
	}
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
		snaps := o.Snaps
		if profiles == nil {
			profiles, snaps = routedProfiles()
		}
		for _, profile := range profiles {
			fields := map[string]string{}
			if o.Identities != nil {
				// One read of the profile pins its identity and yields the fields,
				// the baseline and the catalogue tags the loop needs: the
				// selection's snapshot when there is one, its own otherwise (GH-485).
				snap := snaps[profile]
				if snap == nil {
					snap, _ = profileSnapshot(profile)
				}
				if snap == nil {
					o.Identities[profile] = nil
				} else {
					o.Identities[profile] = snap.ID
					fields = snap.Fields
					for m, v := range DiscoveredValues(fields) {
						baseline[profile+":"+m] = v
					}
					for m, t := range DiscoveredSources(fields) {
						catalogueTags[profile+":"+m] = t
					}
				}
			}
			env, err := ccmorphEnv(ctx, profile)
			check(ctx)
			if err != nil {
				var ru *RouteUnavailable
				if errors.As(err, &ru) {
					notes = append(notes, Note{Reason: strings.TrimSpace(strings.ReplaceAll(ru.Msg, "ccmorph:", "")), Subject: profile, Profile: profile, Failed: true})
					continue
				}
				panic(err)
			}
			handle = env["CCMORPH_ROUTE_HANDLE"]
			key := env["ANTHROPIC_API_KEY"]
			if key == "" {
				notes = append(notes, Note{Reason: "profile has no api key, skipped", Subject: profile, Profile: profile, Failed: true})
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
				notes = append(notes, Note{Reason: WhyUnreachable(err, base), Subject: profile, Profile: profile, Failed: true})
				if o.Mode == "declared-only" && o.OnlySet && len(o.Only) > 0 {
					// Explicit model selection is enough for a catalogue lookup; an
					// unavailable gateway is not evidence of model absence.
					models = sortedKeys(o.Only)
					declared, listingRows = map[string]int64{}, nil
					fmt.Fprintf(stderr, "[%s] gateway metadata unavailable; trying catalogue for requested models\n", profile)
				} else {
					stopRoute(ctx)
					continue
				}
			}
			if o.Identities == nil {
				path, _ := ProfilePath(profile)
				if f, ok := confFields(path); ok {
					fields = f
				}
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
					notes = append(notes, Note{Reason: fmt.Sprintf("'%s' is not on this route%s", wanted, hint), Subject: profile, Profile: profile, Failed: true})
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
			sized := map[string]int64{}
			for _, m := range slices.Sorted(maps.Keys(declared)) {
				if v := declared[m]; inModels[m] && !pinned[m] {
					sized[m] = v
				}
			}
			raised, pending := ProviderWindows(ctx, sized, listingRows, fields, listTO, cache)
			check(ctx)
			maps.Copy(declared, raised)
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
					notes = append(notes, Note{Reason: "provider catalogue unreachable; size left for the next run", Subject: model, Profile: profile, Failed: true})
					continue
				}
				pin := fields["context_window."+model]
				if pinnedValue(pin) && o.Mode != "probe-all" {
					notes = append(notes, Note{Reason: "manual context override configured; not probed", Subject: model, Profile: profile})
					continue
				}
				scoped := profile + ":" + model
				known, haveKnown := baseline[scoped]
				priorTag := catalogueTags[scoped]
				// A prior catalogue guess is not verified: a fresh declared value this
				// run always supersedes it, unlike a real probed/declared value, which
				// stays pinned.
				if o.Mode != "probe-all" && haveKnown && known != 0 && !(priorTag != nil && declared[model] != 0) {
					values[scoped] = known
					if priorTag != nil {
						// Repeating the same value must not look like a fresh unsourced
						// (therefore "verified") write: carry the tag forward exactly.
						sources[scoped] = priorTag
					}
					if d, ok := declared[model]; ok && d != known {
						reason := fmt.Sprintf("kept saved limit %s; gateway now declares %s", commas(known), commas(d))
						notes = append(notes, Note{Reason: reason, Subject: model, Profile: profile})
					}
					continue
				}
				if d, ok := declared[model]; ok && o.Mode != "probe-all" {
					values[scoped] = d
					continue
				}
				if o.Mode == "declared-only" {
					found := CatalogueLookup(ctx, model, ownedBy[model], cache, catalogueTimeout*time.Second)
					check(ctx)
					if found != nil {
						values[scoped] = found.Value
						sources[scoped] = CatalogueTag(found)
					} else {
						notes = append(notes, Note{Reason: Unresolved, Subject: model, Profile: profile})
					}
					continue
				}
				// The warning is output too: wrap it at the same width as the
				// report, so a narrow terminal never breaks a word in it.
				wrapLine(stderr, fmt.Sprintf("[%s] %s: paid probe enabled.", profile, model), "", "  ")
				wrapLine(stderr, fmt.Sprintf("The first %s-character prompt may be billed in full.",
					commas(OvershootChars)), "  ", "  ")
				wrapLine(stderr, "Confirmation and search requests add cost; rates and tokenization "+
					"depend on the gateway. A timeout does not undo billing.", "  ", "  ")
				if blob == "" {
					blob = payloadFn(OvershootChars)
				}
				res := probeFn(ctx, base, key, model, blob, time.Duration(o.ProbeTimeout*float64(time.Second)), op, o.Bisect)
				check(ctx)
				if (strings.HasSuffix(res.Kind, ":declared") || strings.HasSuffix(res.Kind, ":truncated") || strings.HasSuffix(res.Kind, ":bisected")) && res.Value > 0 {
					values[scoped] = res.Value
				} else {
					why := res.Note
					if why == "" {
						why = res.Kind
					}
					notes = append(notes, Note{Reason: why, Subject: model, Profile: profile, Failed: true})
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
		notes = append(notes, Note{Reason: reason, Subject: "discovery", Failed: true})
		fmt.Fprintln(stderr, notes[len(notes)-1].Reason)
	}
	if handle != "" {
		if runWithDeadline(context.Background(), cleanupBudget, func(ctx context.Context) { stopRoute(ctx) }) {
			fmt.Fprintln(stderr, "route cleanup timed out; a provider bridge may still be running")
		}
	}
	return Run{values, notes, sources, baseline}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
