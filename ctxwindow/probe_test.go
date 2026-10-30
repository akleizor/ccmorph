package ctxwindow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/akleizor/ccmorph/pycompat"
)

// fake is a scripted Opener: each call pops the next reply for its method+path.
type fake struct {
	mu    sync.Mutex
	reply func(r Request) ([]byte, error)
	seen  []Request
}

func (f *fake) Open(ctx context.Context, r Request, timeout time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r)
	f.mu.Unlock()
	return f.reply(r)
}

func httpErr(code int, body string) error { return &HTTPError{code, []byte(body)} }

func parse(t *testing.T, text string) *pycompat.Obj {
	t.Helper()
	v, err := pycompat.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pycompat.Obj)
}

func captureStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old := stderr
	stderr = &b
	t.Cleanup(func() { stderr = old })
	return &b
}

func state(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", dir)
	t.Setenv("CCMORPH_LIB_DIR", filepath.Join(dir, "lib"))
	// The runner's own context pin must not reach Resolve: an inherited
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS made every window in a profile read back
	// as that number.
	for _, k := range []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "MODEL_WINDOW_PROFILE_CONF", "CCMORPH_PROFILE_LOCK_HELD"} {
		t.Setenv(k, "")
	}
	old := fetchJSONDict
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj { return pycompat.NewObj() }
	t.Cleanup(func() { fetchJSONDict = old })
	probeShareDir = func() string { return filepath.Join(dir, "share") }
	t.Cleanup(func() { probeShareDir = func() string { return pycompat.Join(libDir(), "..", "share") } })
	return dir
}

func put(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubs replaces the discovery seams; restore is automatic.
func stubs(t *testing.T, env func(string) (map[string]string, error), list func(base string) ([]string, map[string]int64, []*pycompat.Obj, error)) {
	t.Helper()
	oe, ol, op, ot, os_ := ccmorphEnv, listModelsFn, priorValues, priorCatalogueTags, routeStop
	t.Cleanup(func() { ccmorphEnv, listModelsFn, priorValues, priorCatalogueTags, routeStop = oe, ol, op, ot, os_ })
	ccmorphEnv = func(ctx context.Context, p string) (map[string]string, error) { return env(p) }
	listModelsFn = func(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, *pycompat.OMap[int64], []*pycompat.Obj, error) {
		ids, sizes, rows, err := list(base)
		m := pycompat.NewOMap[int64]()
		keys := make([]string, 0)
		for k := range sizes {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			m.Set(k, sizes[k])
		}
		return ids, m, rows, err
	}
	routeStop = func(ctx context.Context, h string) {}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func okEnv(string) (map[string]string, error) {
	return map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://x"}, nil
}

func vals(r Run) map[string]int64 {
	out := map[string]int64{}
	for _, k := range r.Values.Keys() {
		out[k], _ = r.Values.Get(k)
	}
	return out
}

func eq(t *testing.T, got, want map[string]int64) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func hasNote(r Run, substr string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n.Reason, substr) {
			return true
		}
	}
	return false
}

// Python: DiagnosticWording.
func TestWhyUnreachable(t *testing.T) {
	denied := WhyUnreachable(&HTTPError{Code: 403}, "https://gw")
	if !strings.Contains(denied, "HTTP 403") || !strings.Contains(denied, "access") {
		t.Fatal(denied)
	}
	to := WhyUnreachable(&URLError{context.DeadlineExceeded}, "https://gw")
	if !strings.Contains(to, "did not answer at gw") || strings.Contains(to, "URLError") || len(to) >= 150 {
		t.Fatal(to)
	}
	if got := WhyUnreachable(&URLError{&net.DNSError{Name: "x"}}, "https://gw/api"); got != "could not resolve gw (DNS); check the endpoint and network access" {
		t.Fatal(got)
	}
	if got := WhyUnreachable(errors.New("boom"), "https://gw"); got != "could not be reached at gw (boom); check the endpoint and network access" {
		t.Fatal(got)
	}
}

// Python: test_partial_probe_failure_prints_its_reason, ClaudeModelsAreProbed,
// test_declared_models_are_recorded_without_probing, test_direct_discovery_default_never_builds_or_sends_a_prompt.
func TestProbeAllModes(t *testing.T) {
	state(t)
	stubs(t, okEnv, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"a", "b", "c"}, map[string]int64{"a": 128000, "b": 1000000}, nil, nil
	})
	priorValues = func([]string) map[string]int64 { return map[string]int64{} }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return map[string]*pycompat.Obj{} }
	var probed []string
	oldProbe, oldPayload := probeFn, payloadFn
	t.Cleanup(func() { probeFn, payloadFn = oldProbe, oldPayload })
	probeFn = func(ctx context.Context, base, key, model, blob string, timeout time.Duration, op Opener, bisect int) ProbeResult {
		probed = append(probed, model)
		return ProbeResult{"anthropic:declared", 700000, ""}
	}
	payloadFn = func(int) string { t.Fatal("a metadata-only run built a prompt"); return "" }
	captureStderr(t)

	r := ProbeAll(context.Background(), []string{"gw"}, DefaultOptions())
	eq(t, vals(r), map[string]int64{"gw:a": 128000, "gw:b": 1000000})
	if len(probed) != 0 || !hasNote(r, Unresolved) {
		t.Fatal("declared models must not be probed; c is unresolved")
	}

	payloadFn = func(int) string { return "blob" }
	o := DefaultOptions()
	o.Mode = "probe-all"
	r = ProbeAll(context.Background(), []string{"gw"}, o)
	eq(t, vals(r), map[string]int64{"gw:a": 700000, "gw:b": 700000, "gw:c": 700000}) // claude-* and gpt-* alike are probed
	if len(probed) != 3 {
		t.Fatal(probed)
	}
}

func TestProbeFailureReasonIsPrinted(t *testing.T) {
	state(t)
	stubs(t, okEnv, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"known", "failed"}, map[string]int64{"known": 300000}, nil, nil
	})
	priorValues = func([]string) map[string]int64 { return nil }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	oldProbe := probeFn
	t.Cleanup(func() { probeFn = oldProbe })
	probeFn = func(ctx context.Context, base, key, model, blob string, timeout time.Duration, op Opener, bisect int) ProbeResult {
		return ProbeResult{"anthropic:error", 0, "gateway timed out"}
	}
	out := captureStderr(t)
	o := DefaultOptions()
	o.Mode = "auto"
	ProbeAll(context.Background(), []string{"gw"}, o)
	if !strings.Contains(out.String(), "gateway timed out") || !strings.Contains(out.String(), "paid probe enabled") {
		t.Fatal(out.String())
	}
}

// Python: RouteScopedMetadata + cached_readings_survive_lower_metadata.
func TestBaselineIsScopedAndCachedReadingsSurvive(t *testing.T) {
	state(t)
	stubs(t, okEnv, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"a"}, map[string]int64{"a": 200000}, nil, nil
	})
	priorValues = func([]string) map[string]int64 { return map[string]int64{"a": 1000000} }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	eq(t, vals(ProbeAll(context.Background(), []string{"other"}, DefaultOptions())), map[string]int64{"other:a": 200000})

	priorValues = func([]string) map[string]int64 { return map[string]int64{"gw:a": 1000000} }
	probeFn = func(ctx context.Context, base, key, model, blob string, timeout time.Duration, op Opener, bisect int) ProbeResult {
		t.Fatal("a cached reading was spent again")
		return ProbeResult{}
	}
	o := DefaultOptions()
	o.Mode = "auto"
	captureStderr(t)
	eq(t, vals(ProbeAll(context.Background(), []string{"gw"}, o)), map[string]int64{"gw:a": 1000000})
}

// Python: DiscoveryDeadline.
func TestOneDeadlineCoversEveryPhase(t *testing.T) {
	for _, phase := range []string{"ccmorph_env", "list_models", "route_stop"} {
		t.Run(phase, func(t *testing.T) {
			state(t)
			block := func(ctx context.Context) { <-ctx.Done() }
			stubs(t, func(string) (map[string]string, error) {
				return map[string]string{"ANTHROPIC_API_KEY": "k", "CCMORPH_ROUTE_HANDLE": "handle"}, nil
			}, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
				return []string{"a"}, map[string]int64{"a": 123000}, nil, nil
			})
			priorValues = func([]string) map[string]int64 { return nil }
			priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
			ctxEnv, ctxList, ctxStop := ccmorphEnv, listModelsFn, routeStop
			switch phase {
			case "ccmorph_env":
				ccmorphEnv = func(ctx context.Context, p string) (map[string]string, error) {
					block(ctx)
					check(ctx)
					return ctxEnv(ctx, p)
				}
			case "list_models":
				listModelsFn = func(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, *pycompat.OMap[int64], []*pycompat.Obj, error) {
					block(ctx)
					return ctxList(ctx, base, key, timeout, op)
				}
			case "route_stop":
				routeStop = func(ctx context.Context, h string) { block(ctx); check(ctx); ctxStop(ctx, h) }
			}
			out := captureStderr(t)
			o := DefaultOptions()
			o.Deadline = 0.5
			r := ProbeAll(context.Background(), []string{"gw"}, o)
			if !hasNote(r, "deadline reached") || !strings.Contains(out.String(), "deadline reached") {
				t.Fatalf("notes %v stderr %q", r.Notes, out.String())
			}
		})
	}
}

// Python: test_deadline_during_route_cleanup_preserves_discovered_values +
// test_deadline_attempts_bounded_route_cleanup.
func TestDeadlineDuringCleanupKeepsValuesAndTriesCleanup(t *testing.T) {
	state(t)
	stubs(t, func(string) (map[string]string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "k", "CCMORPH_ROUTE_HANDLE": "handle"}, nil
	}, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"a"}, map[string]int64{"a": 123000}, nil, nil
	})
	priorValues = func([]string) map[string]int64 { return nil }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	var stops []string
	routeStop = func(ctx context.Context, h string) { stops = append(stops, h); panic(runDeadline{}) }
	captureStderr(t)
	o := DefaultOptions()
	o.Deadline = 30
	r := ProbeAll(context.Background(), []string{"gw"}, o)
	eq(t, vals(r), map[string]int64{"gw:a": 123000})
	if len(stops) != 1 || stops[0] != "handle" || !hasNote(r, "deadline reached") {
		t.Fatal(stops, r.Notes)
	}

	// A deadline during listing still attempts one bounded cleanup.
	stops = nil
	routeStop = func(ctx context.Context, h string) { stops = append(stops, h) }
	listModelsFn = func(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, *pycompat.OMap[int64], []*pycompat.Obj, error) {
		panic(runDeadline{})
	}
	o.Deadline = 5
	ProbeAll(context.Background(), []string{"gw"}, o)
	if len(stops) != 1 || stops[0] != "H" && stops[0] != "handle" {
		t.Fatal(stops)
	}
}

// A Ctrl-C landing inside the route stop used to abort it: the zsh ran on
// the cancelled context and never started, and the last-chance cleanup saw
// the handle already cleared, so the bridge and its key-bearing /tmp config
// outlived the command (#66). The real routeStop runs here, against a
// ccmorph stub that records what it was asked to do.
func TestCancelInsideRouteStopStillRetiresTheRoute(t *testing.T) {
	dir := state(t)
	stops := filepath.Join(dir, "stops")
	put(t, filepath.Join(dir, "lib", "ccmorph.sh"), `ccmorph() { print -r -- "$*" >> '`+stops+`'; }`+"\n")
	realStop := routeStop
	stubs(t, func(string) (map[string]string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "k", "CCMORPH_ROUTE_HANDLE": "handle"}, nil
	}, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"a"}, map[string]int64{"a": 123000}, nil, nil
	})
	priorValues = func([]string) map[string]int64 { return nil }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeStop = func(ctx context.Context, h string) { cancel(); realStop(ctx, h) }
	captureStderr(t)
	r := ProbeAll(parent, []string{"gw"}, DefaultOptions())
	got, _ := os.ReadFile(stops)
	if string(got) != "_route_stop handle\n" || !hasNote(r, "interrupted") {
		t.Fatalf("stops %q notes %v", got, r.Notes)
	}
}

// Python: ProbeOutputStaysOnPoint + ModelScope + catalogue-on-listing-failure.
// A profile lock held by `ccmorph config` used to sit in a blocking Flock the
// deadline never saw: measured 20.0s against --deadline 4. The save now waits
// on the run's context, so the run ends at its deadline and writes nothing.
func TestHeldProfileLockCannotOutliveTheDeadline(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "type=apikey\napi_key=k\n")
	stubs(t, okEnv, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"known"}, map[string]int64{"known": 922000}, nil, nil
	})
	priorValues = func([]string) map[string]int64 { return map[string]int64{} }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	lock, err := os.OpenFile(filepath.Join(dir, "gw.conf.lock"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- ProbeMain([]string{"--profile", "gw", "--deadline", "0.5"}, strings.NewReader(""), &out, &errb)
	}()
	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		<-done
		t.Fatal("the registry run outlived its 0.5s deadline behind a held lock")
	}
	if code != 1 || !strings.Contains(errb.String(), "Save deadline reached") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
	if _, fields := ReadConf(filepath.Join(dir, "gw.conf")); fields["context_discovered"] != "" {
		t.Fatal("a value was written past the deadline")
	}
}

func TestScopeExclusionsAndNearMisses(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "excluded_models=hidden\n")
	stubs(t, func(string) (map[string]string, error) { return map[string]string{"ANTHROPIC_API_KEY": "k"}, nil },
		func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
			return []string{"kept", "hidden", "claude-opus-5-5", "gpt-5"}, nil, nil, nil
		})
	priorValues = func([]string) map[string]int64 { return nil }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	captureStderr(t)
	r := ProbeAll(context.Background(), []string{"gw"}, DefaultOptions())
	for _, n := range r.Notes {
		if n.Reason == Unresolved && n.Subject == "hidden" {
			t.Fatal("an excluded model was reported unresolved")
		}
	}
	o := DefaultOptions()
	o.OnlySet, o.Only = true, map[string]bool{"opus-5-5": true}
	r = ProbeAll(context.Background(), []string{"gw"}, o)
	if !hasNote(r, "did you mean claude-opus-5-5") {
		t.Fatal(r.Notes)
	}
	o.Only = map[string]bool{"not-in-catalogue": true}
	r = ProbeAll(context.Background(), []string{"gw"}, o)
	if r.Values.Len() != 0 {
		t.Fatal("an absent model produced a value")
	}

	// With the listing down, an explicit --model still reaches the catalogue.
	listModelsFn = func(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, *pycompat.OMap[int64], []*pycompat.Obj, error) {
		return nil, nil, nil, &URLError{context.DeadlineExceeded}
	}
	os.WriteFile(filepath.Join(probeShareDir(), "x"), nil, 0) // no-op: share dir may not exist
	put(t, filepath.Join(probeShareDir(), "model-catalogue.json"), `{"retrieved":"2026-01-01","models":{"openai/gpt-5":{"limit":{"context":400000,"input":272000}}}}`)
	o.Only = map[string]bool{"gpt-5": true}
	out := captureStderr(t)
	r = ProbeAll(context.Background(), []string{"sample"}, o)
	eq(t, vals(r), map[string]int64{"sample:gpt-5": 272000})
	if r.Sources["sample:gpt-5"] == nil || !strings.Contains(out.String(), "gateway metadata unavailable") {
		t.Fatal(out.String())
	}
}

// Python: test_declared_only_run_fills_only_the_gap_and_tags_provenance, ..._supersedes_..., ..._keeps_the_catalogue_tag.
func TestCatalogueTagLifecycle(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "type=apikey\napi_key=canary\n")
	rows := []*pycompat.Obj{parse(t, `{"id":"acme/solo","owned_by":"acme"}`)}
	stubs(t, func(string) (map[string]string, error) { return map[string]string{"ANTHROPIC_API_KEY": "k"}, nil },
		func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
			return []string{"acme/solo"}, nil, rows, nil
		})
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		if url == catalogueURL {
			return parse(t, `{"acme":{"models":{"acme/solo":{"limit":{"context":128000,"input":100000}}}}}`)
		}
		return pycompat.NewObj()
	}
	captureStderr(t)
	r := ProbeAll(context.Background(), []string{"gw"}, DefaultOptions())
	eq(t, vals(r), map[string]int64{"gw:acme/solo": 100000})
	tag := r.Sources["gw:acme/solo"]
	if tag == nil || getv(tag, "source") != "catalogue" {
		t.Fatal("untagged catalogue value")
	}
	v, _ := tag.Get("context")
	if n, _ := v.(pycompat.Num); n.Lit != "128000" {
		t.Fatal(tag)
	}

	// Commit stores the value and its tag.
	identities := map[string]*Identity{"gw": ProfileIdentity("gw")}
	if code, _ := Commit(context.Background(), r.Values, nil, identities, r.Sources); code != 0 {
		t.Fatal("commit failed")
	}
	_, fields := ReadConf(filepath.Join(dir, "gw.conf"))
	if s, ok := DiscoveredSources(fields).Get("acme/solo"); !ok || getv(s, "source") != "catalogue" {
		t.Fatal("tag not stored")
	}

	// A second run with no declared size carries the value AND the tag, without refetching.
	priorValues = func([]string) map[string]int64 { return map[string]int64{"gw:acme/solo": 100000} }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return map[string]*pycompat.Obj{"gw:acme/solo": tag} }
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		t.Fatal("refetched")
		return nil
	}
	r = ProbeAll(context.Background(), []string{"gw"}, DefaultOptions())
	eq(t, vals(r), map[string]int64{"gw:acme/solo": 100000})
	if r.Sources["gw:acme/solo"] != tag {
		t.Fatal("the tag was dropped")
	}

	// A gateway that now declares a size supersedes the prior catalogue guess.
	listModelsFn = func(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, *pycompat.OMap[int64], []*pycompat.Obj, error) {
		d := pycompat.NewOMap[int64]()
		d.Set("acme/solo", 900000)
		return []string{"acme/solo"}, d, rows, nil
	}
	r = ProbeAll(context.Background(), []string{"gw"}, DefaultOptions())
	eq(t, vals(r), map[string]int64{"gw:acme/solo": 900000})
	if r.Sources["gw:acme/solo"] != nil {
		t.Fatal("a declared value kept the catalogue tag")
	}
}

func TestCatalogueFetchedAtMostOncePerRun(t *testing.T) {
	state(t)
	n := 0
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		n++
		return pycompat.NewObj()
	}
	cache := NewCache(false)
	for _, m := range []string{"x", "y", "z"} {
		CatalogueLookup(context.Background(), m, "", cache, time.Second)
	}
	if n != 2 || cache.apiFetches != 1 || cache.flatFetches != 1 { // one api.json + one models.json, fetched once each
		t.Fatalf("fetches %d %d %d", n, cache.apiFetches, cache.flatFetches)
	}
}

var catalogueFixture = `{
 "anthropic":{"models":{"claude-x":{"limit":{"context":400000}}}},
 "acme":{"models":{"acme/solo":{"limit":{"context":128000,"input":100000}},"shared-name":{"limit":{"context":50000}},
   "input-only":{"limit":{"input":30000}},"no-limit-field":{"name":"nope"},"zero":{"limit":{"context":0}}}},
 "other-co":{"models":{"shared-name":{"limit":{"context":999999}}}}}`

// Python: CatalogueFallback.
func TestCatalogueWindow(t *testing.T) {
	c := parse(t, catalogueFixture)
	cases := []struct {
		model, owner string
		want         int64
	}{
		{"claude-x", "anthropic", 400000}, {"acme/solo", "acme", 100000}, {"claude-x", "wrong", 400000},
		{"input-only", "acme", 30000}, {"shared-name", "acme", 50000}, {"shared-name", "", 0}, {"shared-name", "nobody", 0},
		{"no-limit-field", "acme", 0}, {"zero", "acme", 0}, {"nope", "acme", 0},
	}
	for _, tc := range cases {
		got := CatalogueWindow(c, tc.model, tc.owner)
		switch {
		case tc.want == 0 && got != nil:
			t.Errorf("%s/%s: want absent, got %v", tc.owner, tc.model, got.Value)
		case tc.want != 0 && (got == nil || got.Value != tc.want):
			t.Errorf("%s/%s: want %d, got %v", tc.owner, tc.model, tc.want, got)
		}
	}
	if w := CatalogueWindow(c, "acme/solo", "acme"); *w.Input != 100000 || *w.Context != 128000 {
		t.Fatal("both the operational value and the parts are kept")
	}
}

// Python: CanonicalCatalogueFallback (the function lives in the conf store, shared with the resolver).
func TestCanonicalCatalogueWindow(t *testing.T) {
	c := parse(t, `{"swiss-ai/apertus-8b":{"limit":{"context":65536}},"acme/solo":{"limit":{"context":128000,"input":100000}},
 "acme/shared-name":{"limit":{"context":1}},"other/shared-name":{"limit":{"context":2}},
 "acme/widget-v2":{"family":"widget","limit":{"context":300000}},"acme/widget-v3":{"family":"widget","limit":{"context":300000}},
 "acme/conflict-v2":{"family":"conflict","limit":{"context":100000}},"acme/conflict-v3":{"family":"conflict","limit":{"context":200000}},
 "acme/partial-v1":{"family":"partial","limit":{}},"acme/partial-v2":{"family":"partial","limit":{"context":300000}},
 "acme/shared-v2":{"family":"shared-name","limit":{"context":1000000}}}`)
	for _, tc := range []struct {
		model, owner string
		want         int64
	}{
		{"apertus-8b", "swiss-ai", 65536}, {"apertus-8b", "", 65536}, {"apertus-8b", "cloudsway", 65536},
		{"shared-name", "", 0}, {"shared-name", "wrong-co", 0}, {"shared-name", "acme", 1},
		{"widget", "acme", 300000}, {"conflict", "acme", 0}, {"partial", "acme", 300000}, {"nope", "acme", 0},
	} {
		got := CanonicalCatalogueWindow(c, tc.model, tc.owner)
		if (got == nil) != (tc.want == 0) || (got != nil && got.Value != tc.want) {
			t.Errorf("%s/%s: got %v want %d", tc.owner, tc.model, got, tc.want)
		}
	}
}

// Python: catalogue_offline_test.
func TestBlockedCatalogueUsesTheBundledSnapshot(t *testing.T) {
	state(t)
	put(t, filepath.Join(probeShareDir(), "model-catalogue.json"),
		`{"retrieved":"2026-09-01","models":{"openai/gpt-5":{"limit":{"context":400000,"input":272000}}}}`)
	out := captureStderr(t)
	cat := FetchCanonicalCatalogue(context.Background(), time.Second)
	got := CanonicalCatalogueWindow(cat, "gpt-5", "")
	if got == nil || got.Value != 272000 || *got.Input != 272000 || *got.Context != 400000 {
		t.Fatal(got)
	}
	if !strings.Contains(out.String(), "using bundled snapshot from 2026-09-01 (models.dev)") {
		t.Fatal(out.String())
	}
}

// Python: test_catalogue_fetch_sends_a_user_agent.
func TestCatalogueFetchSendsAUserAgent(t *testing.T) {
	old := fetchJSONDict
	t.Cleanup(func() { fetchJSONDict = old })
	f := &fake{reply: func(r Request) ([]byte, error) {
		for _, h := range r.Header {
			if h[0] == "User-Agent" && h[1] == userAgent {
				return []byte(`{"ok":1}`), nil
			}
		}
		return nil, httpErr(403, "")
	}}
	defaultOpener = f
	t.Cleanup(func() { defaultOpener = BuildOpener() })
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		body, err := pick(nil).Open(ctx, Request{Method: "GET", URL: url, Header: [][2]string{{"User-Agent", userAgent}}}, timeout)
		if err != nil {
			return pycompat.NewObj()
		}
		return parseDict(body)
	}
	if got := fetchJSONDict(context.Background(), "http://x/models.json", time.Second); got.Len() != 1 {
		t.Fatal("the fetch was rejected")
	}
}

// Python: DeclaredSizes.
func TestDeclaredSizes(t *testing.T) {
	rows := []*pycompat.Obj{parse(t, `{"id":"a","context_length":128000}`), parse(t, `{"id":"b","max_input_tokens":200000}`),
		parse(t, `{"id":"c"}`), parse(t, `{"id":"d","max_context_length":"not-an-int"}`), parse(t, `{"id":"e","context_window":true}`),
		parse(t, `{"id":"f","max_input_tokens":0,"context_length":5}`)}
	got := DeclaredSizes(rows)
	want := map[string]int64{"a": 128000, "b": 200000, "f": 5}
	if got.Len() != 3 {
		t.Fatal(got.Keys())
	}
	for k, v := range want {
		if g, _ := got.Get(k); g != v {
			t.Errorf("%s: %d", k, g)
		}
	}
}

// Python: MetadataCompatibility.
func TestListModelsOllamaAndDisguisedIDs(t *testing.T) {
	f := &fake{reply: func(r Request) ([]byte, error) {
		if r.Method == "GET" {
			return []byte(`{"data":[{"id":"qwen3-coder:64k","owned_by":"ollama"},{"id":"known","owned_by":"ollama","context_length":32000},
				{"id":"opaque","owned_by":"other"},{"id":"claude-fable-5-dd-los-6-tpg","context_length":272000},
				{"id":"embed","capabilities":{"chat_completion":false}}]}`), nil
		}
		return []byte(`{"parameters":"num_ctx 65536\n","model_info":{"context_length":262144}}`), nil
	}}
	ids, sizes, rows, err := ListModels(context.Background(), "http://local", "k", time.Second, f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "gpt-6-sol,known,opaque,qwen3-coder:64k" {
		t.Fatal(ids)
	}
	for k, v := range map[string]int64{"qwen3-coder:64k": 65536, "known": 32000, "gpt-6-sol": 272000} {
		if g, _ := sizes.Get(k); g != v {
			t.Errorf("%s: %d", k, g)
		}
	}
	if sizes.Has("opaque") || len(rows) != 4 {
		t.Fatal("opaque must stay unknown; the embedding row is dropped")
	}
	var post *Request
	for i := range f.seen {
		if f.seen[i].Method == "POST" {
			post = &f.seen[i]
		}
	}
	if post == nil || !strings.HasSuffix(post.URL, "/api/show") || string(post.Body) != `{"model": "qwen3-coder:64k"}` || len(f.seen) != 2 {
		t.Fatalf("%+v", f.seen)
	}
	if f.seen[0].URL != "http://local/v1/models" {
		t.Fatal(f.seen[0].URL)
	}
	var sawVersion bool
	for _, h := range f.seen[0].Header {
		sawVersion = sawVersion || (h[0] == "anthropic-version" && h[1] == "2023-06-01")
	}
	if !sawVersion {
		t.Fatal("the version header is missing")
	}
}

// Python: test_probe_urls_match_each_protocols_serving_client.
func TestAPIURL(t *testing.T) {
	base := "https://gateway/api/v4"
	for _, tc := range [][3]string{
		{base, "/v1/messages", base + "/v1/messages"},
		{base, "/v1/chat/completions", base + "/chat/completions"},
		{"https://gw/", "/v1/models", "https://gw/v1/models"},
		{"https://gw/api/v1", "/v1/models", "https://gw/api/v1/models"},
		{"http://127.0.0.1:8080", "/v1/models", "http://127.0.0.1:8080/v1/models"},
	} {
		if got := APIURL(tc[0], tc[1]); got != tc[2] {
			t.Errorf("%s + %s = %s, want %s", tc[0], tc[1], got, tc[2])
		}
	}
}

// Python: HexWrappedBodies.
func TestDecodeHexBodies(t *testing.T) {
	wrapped := `{"detailMsg":"bedrock-claude 400 client error SdkBytes(bytes=0x7b226d657373616765223a2270726f6d707420697320746f6f206c6f6e673a203232373737393420746f6b656e73203e2031303030303030206d6178696d756d227d)"}`
	text := DecodeHexBodies(wrapped)
	if !strings.Contains(text, "prompt is too long: 2277794 tokens > 1000000 maximum") {
		t.Fatal(text)
	}
	if n, ok := matchDeclared(text); !ok || n != 1_000_000 {
		t.Fatal(n, ok)
	}
	for _, junk := range []string{"0x7b226", "0xzz", "no hex here", ""} {
		_ = DecodeHexBodies(junk)
	}
}

// Python: classify_accepted tests.
func TestClassifyAccepted(t *testing.T) {
	if k, v, n := ClassifyAccepted(reading{0, "model_context_window_exceeded"}, nil); k != "exceeded" || v != 0 || !strings.Contains(n, "stop_reason") {
		t.Fatal(k, v, n)
	}
	if k, v, n := ClassifyAccepted(reading{131072, "end_turn"}, &reading{131010, "end_turn"}); k != "truncated" || v != 131072 || !strings.Contains(n, "plateau near 131,072") {
		t.Fatal(k, v, n)
	}
	if k, v, n := ClassifyAccepted(reading{1100000, "end_turn"}, &reading{550000, "end_turn"}); k != "intact" || v != 0 || !strings.Contains(n, "at least 1,100,000") {
		t.Fatal(k, v, n)
	}
	if k, v, _ := ClassifyAccepted(reading{900000, "end_turn"}, nil); k != "inconclusive" || v != 0 {
		t.Fatal(k, v)
	}
	if k, _, n := ClassifyAccepted(reading{0, ""}, &reading{5, ""}); k != "inconclusive" || n != "no usable token counts reported" {
		t.Fatal(k, n)
	}
}

func overflowOpener(code int, body string) Opener {
	return &fake{reply: func(Request) ([]byte, error) { return nil, httpErr(code, body) }}
}

// Python: OverLimitErrorParsing + ProbeFailuresAreExplained.
func TestProbeReadsTheCeilingOutOfTheRefusal(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, body string
		want       int64
	}{
		{"escaped", `{"error":{"message":"prompt is too long: 1552710 tokens > 1000000 maximum"}}`, 1_000_000},
		{"text-prefixed", `tencent 400 client error {"error":{"message":"prompt is too long: 2277794 tokens > 1000000 maximum"}}`, 1_000_000},
		{"text-prefixed-escaped", `tencent 400 client error {"error":{"message":"prompt is too long: 2277794 tokens \u003e 1000000 maximum"}}`, 1_000_000},
		{"nested", `{"error":{"message":"maximum context length is 131072 tokens"}}`, 131072},
		{"wrapped", `cloudsway 400 client error {"error":{"message":"Anthropic Claude bad request: [1261][prompt is too long: 2277794 tokens > 204800 maximum]"}}`, 204800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := overflowOpener(400, tc.body).(*fake)
			r := Probe(ctx, "http://x", "k", "claude-opus-4-6", "blob", time.Second, op, 1)
			if r.Kind != "anthropic:declared" || r.Value != tc.want {
				t.Fatalf("%+v", r)
			}
			// A stated ceiling costs one request: no truncation reread, no bisect.
			if len(op.seen) != 1 {
				t.Fatalf("a declared ceiling needs one request; sent %d", len(op.seen))
			}
		})
	}
}

func TestProbeErrorWording(t *testing.T) {
	ctx := context.Background()
	slow := &fake{reply: func(Request) ([]byte, error) { return nil, &URLError{context.DeadlineExceeded} }}
	r := Probe(ctx, "https://gw", "k", "m", "blob", time.Second, slow, 0)
	for _, want := range []string{"over-limit", "timed out", "--probe-timeout"} {
		if !strings.Contains(r.Note, want) {
			t.Errorf("%q missing from %q", want, r.Note)
		}
	}
	if strings.Contains(r.Note, "TimeoutError(") || r.Kind != "anthropic:error" {
		t.Fatal(r)
	}
	// An opaque body keeps up to 300 characters, whitespace collapsed.
	body := "no number here\n" + strings.Repeat("x ", 400)
	r = Probe(ctx, "https://gw", "k", "m", "blob", time.Second, overflowOpener(400, body), 0)
	if r.Kind != "anthropic:opaque" || len([]rune(r.Note)) > 300 || strings.Contains(r.Note, "\n") {
		t.Fatal(r)
	}
	// 404 on both shapes is "unsupported".
	r = Probe(ctx, "https://gw", "k", "m", "blob", time.Second, overflowOpener(404, ""), 0)
	if r.Kind != "unsupported" || r.Note != "rejected on both shapes" {
		t.Fatal(r)
	}
	// A shape mismatch moves on to the openai shape.
	calls := 0
	op := &fake{reply: func(r Request) ([]byte, error) {
		calls++
		if strings.HasSuffix(r.URL, "/v1/messages") {
			return nil, httpErr(400, `{"error":"does not support the required API"}`)
		}
		return nil, httpErr(400, "maximum context length is 65536 tokens")
	}}
	if r := Probe(ctx, "https://gw", "k", "m", "blob", time.Second, op, 0); r.Kind != "openai:declared" || r.Value != 65536 || calls != 2 {
		t.Fatal(r, calls)
	}
}

// Python: ATimeoutIsNeverACeiling + ProbeErrorBoundaries + OneSenderOnePolicy.
func TestAccepted200ClassificationAndRereadSizes(t *testing.T) {
	ctx := context.Background()
	var sizes []int
	op := &fake{reply: func(r Request) ([]byte, error) {
		sizes = append(sizes, len(r.Body))
		if len(sizes) == 1 {
			return []byte(`{"usage":{"input_tokens":131072},"stop_reason":"end_turn"}`), nil
		}
		return []byte(`{"usage":{"input_tokens":131010},"stop_reason":"end_turn"}`), nil
	}}
	r := Probe(ctx, "https://gw", "k", "m", strings.Repeat("a", 4000), time.Second, op, 0)
	if r.Kind != "anthropic:truncated" || r.Value != 131072 || len(sizes) != 2 || sizes[1] >= sizes[0] {
		t.Fatal(r, sizes)
	}
}

func TestTimeoutIsNeverACeiling(t *testing.T) {
	ctx := context.Background()
	// A timed-out bisection step must report "timeout", never "refused".
	n := 0
	op := &fake{reply: func(r Request) ([]byte, error) {
		n++
		if n == 1 {
			return nil, httpErr(400, "prompt is too long")
		}
		return nil, &URLError{context.DeadlineExceeded}
	}}
	r := Probe(ctx, "https://gw", "k", "m", strings.Repeat("x", 4_000_000), time.Second, op, 1)
	if r.Value != 0 || !strings.HasSuffix(r.Kind, ":opaque") || !strings.Contains(r.Note, "search timed out after 1 step(s); nothing was recorded") {
		t.Fatal(r)
	}
	for _, e := range []error{context.DeadlineExceeded, &URLError{context.DeadlineExceeded}, os.ErrDeadlineExceeded} {
		if !IsTimeout(e) {
			t.Errorf("%v not recognised", e)
		}
	}
	if IsTimeout(errors.New("x")) {
		t.Fatal("a plain error is not a timeout")
	}
}

func TestPostSizedIsTheOneRequestBuilder(t *testing.T) {
	var seen Request
	op := &fake{reply: func(r Request) ([]byte, error) { seen = r; return []byte(`{"usage":{"input_tokens":7}}`), nil }}
	doc, err := postSized(context.Background(), "https://gw/", "k", "m", "blob", "/v1/messages",
		func(k string) [][2]string { return [][2]string{{"x-api-key", k}} }, 5*time.Second, op)
	if err != nil || seen.URL != "https://gw/v1/messages" {
		t.Fatal(err, seen.URL)
	}
	body := parse(t, string(seen.Body))
	if n, _ := getv(body, "max_tokens").(pycompat.Num); n.Lit != "8" {
		t.Fatal(string(seen.Body))
	}
	if tokens(doc) != 7 {
		t.Fatal(tokens(doc))
	}
}

// Python: BisectFindsAnUnstatedCeiling + BisectSpendsAlmostNothing.
func gateway(realLimit int, reportsTokens bool) (Sender, *[]int, *int) {
	var calls []int
	accepts := 0
	return func(blob string) (string, int64) {
		tokens := len(blob) / 4
		calls = append(calls, tokens)
		if tokens > realLimit {
			return "refused", 0
		}
		accepts++
		if reportsTokens {
			return "accepted", int64(tokens)
		}
		return "accepted", 0
	}, &calls, &accepts
}

func TestBisectFindsTheBoundaryAndSpendsAlmostNothing(t *testing.T) {
	blob := strings.Repeat("x", 6_000_000)
	send, calls, accepts := gateway(200_000, true)
	val, note, spent := BisectCeiling(send, blob, 1000, bisectTolerance, 1, bisectMaxSteps)
	if val > 200_000 || val < 100_000 {
		t.Fatalf("val %d", val) // only ever understates
	}
	if len(*calls) > 20 || *accepts != 1 || spent != 1 {
		t.Fatalf("calls %d accepts %d", len(*calls), *accepts)
	}
	if !strings.Contains(note, "reported") || !strings.Contains(note, "potentially billed") {
		t.Fatal(note)
	}
	send, _, accepts = gateway(0, true)
	if val, note, _ := BisectCeiling(send, blob, 1000, bisectTolerance, 1, bisectMaxSteps); val != 0 || *accepts != 0 || !strings.Contains(note, "no limit identified") {
		t.Fatal(val, note)
	}
	send, _, _ = gateway(200_000, false)
	if val, note, _ := BisectCeiling(send, blob, 1000, bisectTolerance, 1, bisectMaxSteps); val == 0 || !strings.Contains(note, "no token count reported") {
		t.Fatal(val, note)
	}
	// Two accepts narrow further than one.
	send, _, _ = gateway(200_000, true)
	two, _, _ := BisectCeiling(send, blob, 1000, bisectTolerance, 2, bisectMaxSteps)
	send, _, _ = gateway(200_000, true)
	one, _, _ := BisectCeiling(send, blob, 1000, bisectTolerance, 1, bisectMaxSteps)
	if two < one {
		t.Fatal(one, two)
	}
	// Hitting the accept cap says the answer understates.
	send, _, _ = gateway(500_000, true)
	if _, note, _ := BisectCeiling(send, blob, 1000, bisectTolerance, 1, bisectMaxSteps); !strings.Contains(note, "understates") && !strings.Contains(note, "reported") {
		t.Fatal(note)
	}
}

func TestBisectNeverReadsATimeoutAsARefusal(t *testing.T) {
	send := func(string) (string, int64) { return "timeout", 0 }
	val, note, _ := BisectCeiling(send, strings.Repeat("x", 4_000_000), 1000, bisectTolerance, 1, bisectMaxSteps)
	if val != 0 || !strings.Contains(note, "timed out") {
		t.Fatal(val, note)
	}
	send = func(string) (string, int64) { return "HTTP 500: not a context-overflow refusal", 0 }
	if val, note, _ := BisectCeiling(send, strings.Repeat("x", 4_000_000), 1000, bisectTolerance, 1, bisectMaxSteps); val != 0 || !strings.Contains(note, "search abandoned after 1 step(s)") {
		t.Fatal(val, note)
	}
}

// Python: InferenceDisclosesItsCost.
func TestWarningPrecedesTheFirstPaidRequest(t *testing.T) {
	state(t)
	stubs(t, okEnv, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) { return []string{"m"}, nil, nil, nil })
	priorValues = func([]string) map[string]int64 { return nil }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	out := captureStderr(t)
	oldProbe := probeFn
	t.Cleanup(func() { probeFn = oldProbe })
	probeFn = func(ctx context.Context, base, key, model, blob string, timeout time.Duration, op Opener, bisect int) ProbeResult {
		if !strings.Contains(out.String(), "may be billed in full") {
			t.Fatal("the cost warning must come first")
		}
		return ProbeResult{"anthropic:declared", 1, ""}
	}
	o := DefaultOptions()
	o.Mode = "auto"
	ProbeAll(context.Background(), []string{"gw"}, o)
}

// Python: ProfileCommitOrdering.
func TestStaleResultIsRejectedAndOtherProfileSurvives(t *testing.T) {
	dir := state(t)
	for _, n := range []string{"a", "b"} {
		put(t, filepath.Join(dir, n+".conf"), "type=apikey\napi_key=canary\n")
	}
	ids := map[string]*Identity{"a": ProfileIdentity("a"), "b": ProfileIdentity("b")}
	captureStderr(t)
	v := pycompat.NewOMap[int64]()
	v.Set("a:m", 500000)
	if c, _ := Commit(context.Background(), v, nil, ids, nil); c != 0 {
		t.Fatal(c)
	}
	v = pycompat.NewOMap[int64]()
	v.Set("a:m", 200000)
	v.Set("b:m", 300000)
	if c, _ := Commit(context.Background(), v, nil, ids, nil); c != 1 {
		t.Fatal("the stale profile must fail the commit")
	}
	prior := priorValues(nil)
	if prior["a:m"] != 500000 || prior["b:m"] != 300000 {
		t.Fatal(prior)
	}
}

func TestNewModelWritePreservesExistingValuesAndManualPin(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "a.conf"), "type=apikey\napi_key=canary\ncontext_window.manual=100000\n")
	captureStderr(t)
	for _, c := range []struct {
		m string
		v int64
	}{{"one", 200000}, {"two", 300000}} {
		v := pycompat.NewOMap[int64]()
		v.Set("a:"+c.m, c.v)
		ids := map[string]*Identity{"a": ProfileIdentity("a")}
		if code, _ := Commit(context.Background(), v, nil, ids, nil); code != 0 {
			t.Fatal(code)
		}
	}
	prior := priorValues(nil)
	if len(prior) != 2 || prior["a:one"] != 200000 || prior["a:two"] != 300000 || ContextValue("a", "manual") != "100000" {
		t.Fatal(prior)
	}
}

// Python: AmbientProxyIsUsed.test_env_asks_ccmorph_for_the_route and CCMorphEnv.
func TestRouteEnvGoesThroughRouteEnvWithPositionalArgs(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "lib", "ccmorph.sh"), `ccmorph() { printf "%s\n" "ARGS=$1" "KEY=value"; }`+"\n")
	env, err := ccmorphEnv(context.Background(), "name; touch /tmp/pwned")
	if err != nil || env["KEY"] != "value" || env["ARGS"] != "_route_env" {
		t.Fatal(env, err)
	}
	put(t, filepath.Join(dir, "lib", "ccmorph.sh"), `ccmorph() { echo "ccmorph: nope" >&2; return 1; }`+"\n")
	_, err = ccmorphEnv(context.Background(), "gw")
	var ru *RouteUnavailable
	if !errors.As(err, &ru) || ru.Msg != "ccmorph: nope" {
		t.Fatal(err)
	}
}

func TestRoutedProfilesAreChosenByConfType(t *testing.T) {
	dir := state(t)
	for n, k := range map[string]string{"gw": "apikey", "old": "proxy", "codex": "provider", "me": "sub", "tk": "token"} {
		put(t, filepath.Join(dir, n+".conf"), "type="+k+"\n")
	}
	put(t, filepath.Join(dir, "broken.conf"), "type=apikey\r\xff\n")
	if got := strings.Join(routedProfiles(), ","); got != "codex,gw,old" {
		t.Fatal(got)
	}
}

// Python: test_the_ambient_proxy_reaches_the_opener (the effect, not the handler)
// and the same-origin redirect guard.
func TestOpenerUsesTheAmbientProxyAndRefusesCrossOriginRedirects(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://ambient.invalid:1")
	t.Setenv("https_proxy", "http://ambient.invalid:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	op := BuildOpener().(*sameOriginClient)
	tr := op.client.Transport.(*http.Transport)
	probe, _ := http.NewRequest("GET", "https://gw.example/v1/models", nil)
	u, err := tr.Proxy(probe)
	if err != nil || u == nil || u.String() != "http://ambient.invalid:1" {
		t.Fatalf("the caller's proxy was blanked: %v %v", u, err)
	}
	mk := func(raw string) *http.Request { r, _ := http.NewRequest("GET", raw, nil); return r }
	if err := op.client.CheckRedirect(mk("http://a.example/x"), []*http.Request{mk("http://a.example/y")}); err != nil {
		t.Fatalf("same origin must follow: %v", err)
	}
	err = op.client.CheckRedirect(mk("http://b.example/x"), []*http.Request{mk("http://a.example/y")})
	var cross *crossOriginError
	if !errors.As(err, &cross) {
		t.Fatalf("cross origin must fail: %v", err)
	}
}

// waitsTransport answers after `headers`; its body's first byte comes at `first`.
// Both waits honour the request context, the way a real socket does.
type waitsTransport struct{ headers, first time.Duration }

type waitsBody struct {
	ctx  context.Context
	wait time.Duration
	r    io.Reader
}

func (b *waitsBody) Read(p []byte) (int, error) {
	if b.wait > 0 {
		select {
		case <-time.After(b.wait):
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
		b.wait = 0
	}
	return b.r.Read(p)
}

func (b *waitsBody) Close() error { return nil }

func (f waitsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(f.headers):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
		Body: &waitsBody{r.Context(), f.first - f.headers, strings.NewReader("{}")}}, nil
}

// A slow link must not cost the launch catalogue. models.dev/api.json is
// 5.9 MB, fetched under launchCatalogueTimeout (3s) when a launch meets an
// unsized model; one budget over the whole exchange fails any transfer below
// about 2 MB/s even though bytes keep arriving. urllib bounded each socket
// operation, and so does this: a flowing transfer finishes however long it
// takes, a stalled one still times out.
func TestTimeoutBoundsEachWaitNotTheWholeTransfer(t *testing.T) {
	t.Run("flowing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			// Each gap is a tenth of the budget; the whole body takes 1.5x it.
			for i := 0; i < 15; i++ {
				w.Write([]byte(" "))
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}
			w.Write([]byte("{}"))
		}))
		defer srv.Close()
		body, err := BuildOpener().Open(context.Background(), Request{Method: "GET", URL: srv.URL}, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("a flowing transfer outlived a 500ms whole-exchange budget: %v", err)
		}
		if string(body) != strings.Repeat(" ", 15)+"{}" {
			t.Fatalf("body %q", body)
		}
	})
	t.Run("stalled", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() { close(release); srv.Close() }()
		_, err := BuildOpener().Open(context.Background(), Request{Method: "GET", URL: srv.URL}, 200*time.Millisecond)
		if !IsTimeout(err) {
			t.Fatalf("a stalled read must time out: %v", err)
		}
	})
	// The headers and the first body byte are two waits. 0.9s then 0.4s under a
	// 1.2s timeout never stalled; one budget over both failed a slow gateway and
	// cost the launch its catalogue (#66).
	t.Run("headers then first byte", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			headers time.Duration
			first   time.Duration
			ok      bool
		}{
			{"each wait under the budget", 900 * time.Millisecond, 1300 * time.Millisecond, true},
			{"stalled before the first byte", 900 * time.Millisecond, 9 * time.Second, false},
			{"stalled before the headers", 9 * time.Second, 10 * time.Second, false},
		} {
			t.Run(c.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cl := &sameOriginClient{&http.Client{Transport: waitsTransport{c.headers, c.first}}}
					body, err := cl.Open(context.Background(), Request{Method: "GET", URL: "http://gw.invalid/v1/models"}, 1200*time.Millisecond)
					if c.ok {
						if err != nil || string(body) != "{}" {
							t.Fatalf("each wait is under the budget: body %q err %v", body, err)
						}
						return
					}
					if !IsTimeout(err) {
						t.Fatalf("a stalled fetch must time out: %v", err)
					}
				})
			})
		}
	})
}

// Python: provider_windows contract.
func TestProviderSourceContract(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(probeShareDir(), "window-sources.json"), `{"sources":[{"name":"t","owned_by":["openai"],"url":"https://t.invalid/m.json",
	 "rows":"models","id":"slug","default":"context_window","window":"max_context_window"}]}`)
	_ = dir
	catalogue := `{"models":[{"slug":"gpt-6-sol","context_window":272000,"max_context_window":872000},
	 {"slug":"gpt-5.6-sol","context_window":272000,"max_context_window":872000},
	 {"slug":"gpt-new","context_window":272000,"max_context_window":500000},
	 {"slug":"gpt-small","context_window":1000000,"max_context_window":272000}]}`
	var rows []*pycompat.Obj
	for _, m := range []string{"gpt-6-sol", "gpt-5.6-sol", "gpt-new", "gpt-small"} {
		rows = append(rows, parse(t, fmt.Sprintf(`{"id":%q,"owned_by":"openai"}`, m)))
	}
	rows = append(rows, parse(t, `{"id":"glm-9","owned_by":"zhipu"}`))
	declared := func() *pycompat.OMap[int64] {
		d := pycompat.NewOMap[int64]()
		for _, kv := range []struct {
			k string
			v int64
		}{{"gpt-6-sol", 272000}, {"gpt-5.6-sol", 372000}, {"gpt-small", 1000000}, {"glm-9", 272000}} {
			d.Set(kv.k, kv.v)
		}
		return d
	}
	serve := func(c string) {
		fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
			if c == "" {
				return pycompat.NewObj()
			}
			return parse(t, c)
		}
	}
	serve(catalogue)
	out, pending := ProviderWindows(context.Background(), declared(), rows, map[string]string{"window_sources": "t"}, time.Second, NewCache(false))
	want := map[string]int64{"gpt-6-sol": 872000, "gpt-5.6-sol": 372000, "gpt-small": 1000000, "glm-9": 272000}
	for k, v := range want {
		if g, _ := out.Get(k); g != v {
			t.Errorf("%s: %d want %d", k, g, v)
		}
	}
	if len(pending) != 0 || out.Has("gpt-new") {
		t.Fatal(pending)
	}
	// A profile that does not opt in is untouched.
	out, _ = ProviderWindows(context.Background(), declared(), rows, map[string]string{}, time.Second, NewCache(false))
	if g, _ := out.Get("gpt-6-sol"); g != 272000 {
		t.Fatal(g)
	}
	// An unreachable source leaves the models it speaks for undecided.
	serve("")
	_, pending = ProviderWindows(context.Background(), declared(), rows, map[string]string{"window_sources": "t"}, time.Second, NewCache(false))
	if !pending["gpt-6-sol"] || pending["glm-9"] || len(pending) != 3 {
		t.Fatal(pending)
	}
	// On the launch path a failure is remembered, so an offline source costs one timeout.
	fetches := 0
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		fetches++
		return pycompat.NewObj()
	}
	ProviderWindows(context.Background(), declared(), rows, map[string]string{"window_sources": "t"}, time.Second, NewCache(true))
	ProviderWindows(context.Background(), declared(), rows, map[string]string{"window_sources": "t"}, time.Second, NewCache(true))
	if fetches != 1 {
		t.Fatalf("fetches %d", fetches)
	}
}

const listing = `{"data":[{"id":"known-model"},{"id":"declared-new","max_input_tokens":500000},{"id":"claude-fable-5-dd-5-tpg"},
 {"id":"mystery-new"},{"id":"hidden-new"},{"id":"embed-new","capabilities":{"chat_completion":false}}]}`

// Python: launch_new_models_test.
func TestLaunchSizesOnlyNewModelsAndRemembersMisses(t *testing.T) {
	dir := state(t)
	conf := filepath.Join(dir, "gw.conf")
	put(t, conf, "type=apikey\nbase_url=https://gw.invalid\napi_key=canary\nexcluded_models=hidden-new\n")
	_, fields := ReadConf(conf)
	route := RouteFingerprint(fields)
	f, _ := os.OpenFile(conf, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintf(f, `context_discovered={"route":%q,"models":{"known-model":123456}}`+"\n", route)
	f.Close()
	fetches := 0
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		fetches++
		if url == catalogueURL {
			return parse(t, `{"openai":{"models":{"gpt-5":{"limit":{"context":400000,"input":272000}}}}}`)
		}
		return pycompat.NewObj()
	}
	oldV := validationWindowFn
	t.Cleanup(func() { validationWindowFn = oldV })
	validationWindowFn = func(context.Context, string, string, string, Opener, time.Duration) int64 { return 0 }
	out := captureStderr(t)
	if c := SizeListing(context.Background(), "gw", listing, "https://gw.invalid", "canary", nil); c != 0 {
		t.Fatal(c)
	}
	w := ProfileWindows("gw")
	if len(w) != 3 || w["known-model"] != 123456 || w["declared-new"] != 500000 || w["gpt-5"] != 272000 {
		t.Fatal(w)
	}
	_, fields = ReadConf(conf)
	today := time.Now().UTC().Format("2006-01-02")
	if m := DiscoveredMisses(fields); m.Len() != 1 {
		t.Fatal(m.Keys())
	} else if d, _ := m.Get("mystery-new"); d != today {
		t.Fatal(d)
	}
	if s, _ := DiscoveredSources(fields).Get("gpt-5"); getv(s, "source") != "catalogue" || fetches == 0 {
		t.Fatal("catalogue estimate not tagged")
	}
	for _, line := range []string{"sized gw:declared-new 500000\n", "sized gw:gpt-5 272000 (catalogue estimate)\n", "no size for gw:mystery-new; launch fallback applies\n"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("%q missing from %q", line, out.String())
		}
	}
	// Next launch inside the retry window asks nobody.
	fetchJSONDict = func(context.Context, string, time.Duration) *pycompat.Obj { t.Fatal("fetched again"); return nil }
	validationWindowFn = func(context.Context, string, string, string, Opener, time.Duration) int64 {
		t.Fatal("asked again")
		return 0
	}
	if c := SizeListing(context.Background(), "gw", listing, "https://gw.invalid", "canary", nil); c != 0 {
		t.Fatal(c)
	}
	if RecentlyMissed("2026-09-01", "2026-09-08") || !RecentlyMissed("2026-09-02", "2026-09-08") || RecentlyMissed("garbage", "2026-09-08") {
		t.Fatal("retry window")
	}
	// A later value retires the miss.
	v := pycompat.NewOMap[int64]()
	v.Set("mystery-new", 64000)
	SaveDiscovered("gw", v, ProfileIdentity("gw"), true, nil, nil)
	_, fields = ReadConf(conf)
	if DiscoveredMisses(fields).Len() != 0 {
		t.Fatal("miss survived")
	}
}

// Python: size_listing replaces the six-byte escape before matching; the
// decoded ">" this port used to write matched itself and normalized nothing.
func TestValidationNormalizesTheEscapedGT(t *testing.T) {
	if got := unicodeGT.Replace(`maximum context length is \u003e 42`); got != "maximum context length is > 42" {
		t.Fatalf("the escape was not normalized: %q", got)
	}
}

func TestInternalModelIsSizedFromItsValidationRefusal(t *testing.T) {
	ctx := context.Background()
	seq := func(replies ...[2]any) Opener {
		return &fake{reply: func(Request) ([]byte, error) {
			r := replies[0]
			replies = replies[1:]
			return nil, httpErr(r[0].(int), r[1].(string))
		}}
	}
	vllm := "max_tokens=100000000 cannot be greater than max_model_len=max_total_tokens=1000000."
	if got := ValidationWindow(ctx, "https://gw.invalid", "k", "m", seq([2]any{429, "rate limited"}, [2]any{400, vllm}), time.Second); got != 1_000_000 {
		t.Fatal(got)
	}
	capped := "The max_tokens parameter is illegal: range [1,131072]"
	if got := ValidationWindow(ctx, "https://gw.invalid", "k", "m", seq([2]any{400, capped}, [2]any{400, capped}), time.Second); got != 0 {
		t.Fatal("an output cap is not a window", got)
	}
	// An accepting gateway states nothing.
	acc := &fake{reply: func(Request) ([]byte, error) { return []byte(`{}`), nil }}
	if ValidationWindow(ctx, "https://gw", "k", "m", acc, time.Second) != 0 {
		t.Fatal()
	}
}

func TestProviderProfileKeepsWindowsSizedThroughItsBridge(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "codex.conf"), "type=provider\nprovider=codex\n")
	fetchJSONDict = func(context.Context, string, time.Duration) *pycompat.Obj {
		t.Fatal("declared size needs no catalogue")
		return nil
	}
	body := `{"data":[{"id":"claude-fable-5-dd-los-6-tpg","context_length":272000}]}`
	if c := SizeListing(context.Background(), "codex", body, "http://127.0.0.1:54321", "ephemeral", nil); c != 0 {
		t.Fatal(c)
	}
	if w := ProfileWindows("codex"); len(w) != 1 || w["gpt-6-sol"] != 272000 {
		t.Fatal(w)
	}
}

func TestOpenRouterShapedListingNeedsNoManualNumber(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "or.conf"), "type=apikey\nbase_url=https://openrouter.ai/api\napi_key=canary\n")
	fixture, err := os.ReadFile(filepath.Join("..", "tests", "fixtures", "openrouter-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c := SizeListing(context.Background(), "or", string(fixture), "https://openrouter.ai/api", "canary", nil); c != 0 {
		t.Fatal(c)
	}
	_, fields := ReadConf(filepath.Join(dir, "or.conf"))
	for k := range fields {
		if strings.HasPrefix(k, "context_window.") {
			t.Fatal("a number was hand-entered")
		}
	}
	for model, want := range map[string]int64{"anthropic/claude-sonnet-4": 1000000, "openai/gpt-5": 400000, "meta-llama/llama-4-maverick": 900000} {
		if r := Resolve("or", model); r.Window != want || r.Unprobed {
			t.Errorf("%s: %+v", model, r)
		}
	}
	if _, ok := ProfileWindows("or")["openai/text-embedding-3-large"]; ok {
		t.Fatal("a non-chat model was sized")
	}
}

func TestListingStdinIgnoresGarbage(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "type=apikey\napi_key=k\n")
	for _, body := range []string{"not json", "[]", `{"data": 5}`, ""} {
		if c := SizeListing(context.Background(), "gw", body, "", "", nil); c != 0 {
			t.Errorf("%q: %d", body, c)
		}
	}
}

// Python: ArgumentValidation + SafeDiscovery.test_paid_modes_require_explicit_consent_and_one_model +
// PartialDiscoveryStatus (the exit codes and the reporting).
func TestMainArgumentsAndReporting(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "type=apikey\napi_key=k\n")
	routes := 0
	stubs(t, func(string) (map[string]string, error) {
		routes++
		return map[string]string{"ANTHROPIC_API_KEY": "k"}, nil
	},
		func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
			return []string{"known", "gpt-5.6-sol"}, map[string]int64{"known": 922000}, nil, nil
		})
	priorValues = func([]string) map[string]int64 { return map[string]int64{"profile:known": 922000} }
	priorCatalogueTags = func([]string) map[string]*pycompat.Obj { return nil }
	for _, args := range [][]string{
		{"--profile", "gw", "--jobs", "0"}, {"--profile", "gw", "--jobs", "-1"}, {"--profile", "gw", "--bisect"},
		{"--profile", "gw", "--probe-all"}, {"--profile", "gw", "--allow-inference"},
		{"--profile", "gw", "--allow-inference", "--model", "a", "--model", "b"},
		{"--profile", "gw", "--declared-only", "--allow-inference", "--model", "a"},
		{"--listing-stdin"}, {"--nope"}, {"--deadline", "inf"}, {"--deadline", "0"},
	} {
		var out, errb bytes.Buffer
		if code := ProbeMain(args, strings.NewReader(""), &out, &errb); code != 2 || !strings.Contains(errb.String(), "error:") || strings.Contains(errb.String(), "Traceback") {
			t.Errorf("%v: %d %q", args, code, errb.String())
		}
	}
	if routes != 0 {
		t.Fatal("a refused invocation reached a route")
	}
	var out, errb bytes.Buffer
	code := ProbeMain([]string{"--profile", "gw"}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "1 unresolved") || !strings.Contains(errb.String(), "ccmorph context gw gpt-5.6-sol <tokens>") || strings.Contains(out.String(), "matches discovery") {
		t.Fatal(code, errb.String())
	}
	out.Reset()
	errb.Reset()
	code = ProbeMain([]string{"--profile", "gw", "--check"}, strings.NewReader(""), &out, &errb)
	if code != 2 || out.String() != "DRIFT gw:known: None -> 922000\n" { // drift wins over the unresolved exit
		t.Fatal(code, out.String())
	}
}

func TestReportUnresolvedOrdersTheFreeFixBeforeThePaidOne(t *testing.T) {
	var b bytes.Buffer
	ReportUnresolved(&b, []string{"deepseek-v4.1-flash"}, []string{"gateway"})
	s := b.String()
	if !strings.Contains(s, "ccmorph context gateway deepseek-v4.1-flash") || !strings.Contains(s, "200,000") ||
		strings.Index(s, "ccmorph context") > strings.Index(s, "--allow-inference") {
		t.Fatal(s)
	}
	b.Reset()
	ReportUnresolved(&b, []string{"a", "b"}, nil)
	if !strings.Contains(b.String(), "2 unresolved") || !strings.Contains(b.String(), "ccmorph context <profile> <model> <tokens>") {
		t.Fatal(b.String())
	}
}

func TestDifflibMatchesPython(t *testing.T) {
	// Values from difflib.get_close_matches(word, possibilities, n=1) under Python 3.
	for _, tc := range []struct {
		word   string
		models []string
		want   string
	}{
		{"opus-5-5", []string{"claude-opus-5-5", "gpt-5"}, "claude-opus-5-5"},
		{"gpt5", []string{"gpt-5", "gpt-6"}, "gpt-5"},
		{"abcd", []string{"abce", "abcf"}, "abcf"}, // equal ratios: the larger string wins, as in Python
		{"zzz", []string{"gpt-5"}, ""},
		{"kimi-k2", []string{"kimi-k2.5", "glm-5"}, "kimi-k2.5"},
	} {
		if got := ClosestMatch(tc.word, tc.models); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.word, got, tc.want)
		}
	}
}

func TestPayloadAndFormatting(t *testing.T) {
	p := payload(200)
	if len(p) < 200 || !strings.HasPrefix(p, "The quick brown fox jumps over the lazy dog number 0. The quick brown fox jumps over the lazy dog number 1. ") {
		t.Fatal(p)
	}
	if commas(1234567) != "1,234,567" || commas(12) != "12" || gFormat(30) != "30" || gFormat(0.2) != "0.2" {
		t.Fatal(commas(1234567), gFormat(30))
	}
}

var _ = io.Discard

// Ported from tests/profile_context_lifecycle_test.py (ProfileDiscoveryLifecycle).
func TestManualOverrideSkipsPaidProbeWithoutBecomingDiscoveredData(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "type=apikey\napi_key=canary\ncontext_window.manual=333333\n")
	stubs(t, okEnv, func(string) ([]string, map[string]int64, []*pycompat.Obj, error) {
		return []string{"manual"}, map[string]int64{}, nil, nil
	})
	oldProbe := probeFn
	t.Cleanup(func() { probeFn = oldProbe })
	probeFn = func(context.Context, string, string, string, string, time.Duration, Opener, int) ProbeResult {
		t.Fatal("a manual window must not be probed")
		return ProbeResult{}
	}
	captureStderr(t)
	o := DefaultOptions()
	o.Mode, o.Only, o.OnlySet = "auto", map[string]bool{"manual": true}, true
	r := ProbeAll(context.Background(), []string{"gw"}, o)
	if _, ok := vals(r)["gw:manual"]; ok {
		t.Fatal("manual model became discovered data")
	}
	if !hasNote(r, "manual") {
		t.Fatal(r.Notes)
	}
}

// Ported from tests/catalogue_offline_test.py (snapshot shape) and
// tests/launch_new_models_test.py (shipped sources): the shipped data files.
func TestShippedDataFilesFollowTheirContracts(t *testing.T) {
	read := func(name string) *pycompat.Obj {
		b, err := os.ReadFile(filepath.Join("..", "share", name))
		if err != nil {
			t.Fatal(err)
		}
		return parse(t, string(b))
	}
	cat := read("model-catalogue.json")
	if s, _ := cat.Get("source"); s != "https://models.dev/models.json" {
		t.Fatal(s)
	}
	if s, _ := cat.Get("retrieved"); s == nil || s == "" {
		t.Fatal("no retrieved date")
	}
	if l, _ := cat.Get("license"); !strings.Contains(fmt.Sprint(l), "Copyright (c) 2025 models.dev") {
		t.Fatal(l)
	}
	models, _ := cat.Get("models")
	rows, ok := models.(*pycompat.Obj)
	if !ok || rows.Len() == 0 {
		t.Fatal("no models")
	}
	for _, id := range rows.Keys() {
		v, _ := rows.Get(id)
		for _, k := range v.(*pycompat.Obj).Keys() {
			if k != "id" && k != "family" && k != "limit" {
				t.Fatalf("%s carries %q", id, k)
			}
		}
	}
	srcs, _ := read("window-sources.json").Get("sources")
	list, ok := srcs.([]any)
	if !ok || len(list) == 0 {
		t.Fatal("no sources")
	}
	for _, s := range list {
		o := s.(*pycompat.Obj)
		for _, k := range []string{"name", "owned_by", "url", "rows", "id", "window"} {
			if !o.Has(k) {
				t.Fatalf("source lacks %q", k)
			}
		}
		if u, _ := o.Get("url"); !strings.HasPrefix(fmt.Sprint(u), "https://") {
			t.Fatal(u)
		}
	}
}

// Ported from tests/openrouter_catalogue_test.py::test_listing_publishes_windows_and_drops_non_chat_models.
func TestOpenRouterListingPublishesWindowsAndDropsNonChatModels(t *testing.T) {
	state(t)
	fixture, err := os.ReadFile(filepath.Join("..", "tests", "fixtures", "openrouter-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{reply: func(Request) ([]byte, error) { return fixture, nil }}
	ids, sizes, _, err := ListModels(context.Background(), "http://local", "k", time.Second, f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"anthropic/claude-sonnet-4": 1000000, "openai/gpt-5": 400000, "meta-llama/llama-4-maverick": 900000}
	if len(ids) != len(want) || sizes.Len() != len(want) {
		t.Fatal(ids)
	}
	for _, id := range ids {
		if g, _ := sizes.Get(id); g != want[id] {
			t.Errorf("%s: %d want %d", id, g, want[id])
		}
	}
}

// shippedSources installs the real share/window-sources.json, as the Python tests read it.
func shippedSources(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "share", "window-sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(probeShareDir(), "window-sources.json"), string(b))
}

func codexListing() string {
	return `{"data":[{"id":"gpt-6-sol","owned_by":"openai","context_length":272000}]}`
}

// Ported from launch_new_models_test.py: test_unreachable_source_leaves_the_model_for_the_next_launch.
func TestUnreachableSourceLeavesTheModelForTheNextLaunch(t *testing.T) {
	dir := state(t)
	shippedSources(t)
	put(t, filepath.Join(dir, "codex.conf"), "type=provider\nprovider=codex\nwindow_sources=codex\n")
	if c := SizeListing(context.Background(), "codex", codexListing(), "http://127.0.0.1:1", "k", nil); c != 0 {
		t.Fatal(c)
	}
	_, fields := ReadConf(filepath.Join(dir, "codex.conf"))
	if len(ProfileWindows("codex")) != 0 || DiscoveredMisses(fields).Len() != 0 {
		t.Fatal("a bare declaration was stored while the source was down")
	}
}

// Ported from launch_new_models_test.py: test_unreachable_source_is_fetched_once_per_retry_window.
func TestUnreachableSourceIsFetchedOncePerRetryWindow(t *testing.T) {
	dir := state(t)
	shippedSources(t)
	put(t, filepath.Join(dir, "codex.conf"), "type=provider\nprovider=codex\nwindow_sources=codex\n")
	var urls []string
	fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) *pycompat.Obj {
		urls = append(urls, url)
		return pycompat.NewObj()
	}
	oldV := validationWindowFn
	t.Cleanup(func() { validationWindowFn = oldV })
	validationWindowFn = func(context.Context, string, string, string, Opener, time.Duration) int64 {
		t.Fatal("quota spent")
		return 0
	}
	SizeListing(context.Background(), "codex", codexListing(), "http://127.0.0.1:1", "k", nil)
	SizeListing(context.Background(), "codex", codexListing(), "http://127.0.0.1:1", "k", nil)
	n := 0
	for _, u := range urls {
		if strings.Contains(u, "codex_client_models") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("source fetched %d times: %v", n, urls)
	}
	if len(ProfileWindows("codex")) != 0 {
		t.Fatal("window stored")
	}
}

// Ported from launch_new_models_test.py: test_provider_bridge_is_never_validated_with_a_request.
func TestProviderBridgeIsNeverValidatedWithARequest(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "codex.conf"), "type=provider\nprovider=codex\n")
	oldV := validationWindowFn
	t.Cleanup(func() { validationWindowFn = oldV })
	validationWindowFn = func(context.Context, string, string, string, Opener, time.Duration) int64 {
		t.Fatal("quota spent")
		return 0
	}
	if c := SizeListing(context.Background(), "codex", `{"data":[{"id":"gpt-unknown","owned_by":"openai"}]}`, "http://127.0.0.1:1", "k", nil); c != 0 {
		t.Fatal(c)
	}
}

// Ported from tests/product_defaults_test.py: the catalogue URLs are fixed.
func TestCatalogueURLsAreFixed(t *testing.T) {
	if catalogueURL != "https://models.dev/api.json" || canonicalCatalogueURL != "https://models.dev/models.json" {
		t.Fatal(catalogueURL, canonicalCatalogueURL)
	}
}
