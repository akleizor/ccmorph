package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// --- helpers -------------------------------------------------------------------

// The tether re-executes the running binary, which here is the test binary:
// never spawn it from a test.
func TestMain(m *testing.M) {
	tether = func(string, *proc, string) {}
	cloakNeverFn = func() bool { return false } // never run the machine's real translator
	os.Exit(m.Run())
}

type exited int

// exits runs fn with the process exit turned into a recoverable event.
func exits(t *testing.T, fn func()) (code int, did bool) {
	t.Helper()
	old := exitFn
	exitFn = func(c int) { panic(exited(c)) }
	exiting.Store(false)
	defer func() {
		exitFn = old
		exiting.Store(false)
		if r := recover(); r != nil {
			e, ok := r.(exited)
			if !ok {
				panic(r)
			}
			code, did = int(e), true
		}
	}()
	fn()
	return 0, false
}

// swap sets a package variable for the test and restores it after.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func row(dialect string) Row {
	return Row{Model: "m", Dialect: dialect, Status: 200}
}

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
}

func writeCacheJSON(t *testing.T, path string, blob map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(blob)
	writeFile(t, path, string(raw), 0o600)
}

func rowJSON(r Row) map[string]any {
	return map[string]any{"model": r.Model, "dialect": r.Dialect, "note": r.Note, "status": r.Status,
		"cap": nil, "nothink": r.Nothink, "id_recycle": r.IDRecycle}
}

func jsonReply(w http.ResponseWriter, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("content-type", "application/json")
	w.Write(body)
}

// portOutsideTheEphemeralPool is a loopback port number for a test whose
// subject binds a port the test chose, where no parallel process can be
// handed the number while the test waits for that bind. A 127.0.0.1:0 draw
// comes from the OS's ephemeral range (49152-65535 on macOS, 32768-60999 on
// Linux), the pool every parallel gate process draws from, so between the
// draw and the bind another worker can take it and a full gate fails a test
// that is green alone (#176, #188). A number below every range's floor cannot
// be drawn by a bind-to-zero; the probe skips the fixed services that live
// down there, and a lost bind draws again.
func portOutsideTheEphemeralPool(t *testing.T) int {
	t.Helper()
	for range 64 {
		p := 1024 + rand.IntN(32768-1024)
		l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue // a fixed service holds the number
		}
		l.Close()
		return p
	}
	t.Fatal("no port below the ephemeral ranges is free on 127.0.0.1")
	return 0
}

// decoy is a listener standing in for an ambient http_proxy; it counts hits.
func decoy(t *testing.T) (url string, hits func() int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\ncontent-length: 0\r\n\r\n"))
			c.Close()
		}
	}()
	return "http://" + l.Addr().String(), func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// --- ports of tests/adapter_auto_test.py ----------------------------------------

func TestAPIURLVersionedBaseGetsNoSecondV1(t *testing.T) { // test_versioned_base_url_is_not_given_a_second_v1
	for base, want := range map[string]string{
		"https://provider.example/api/v4": "https://provider.example/api/v4/models",
		"https://provider.example/api/V4": "https://provider.example/api/V4/models",
		"https://provider.example":        "https://provider.example/v1/models",
	} {
		if got := apiURL(base, "/v1/models"); got != want {
			t.Errorf("apiURL(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestDetectionUsesAnthropicClientsPath(t *testing.T) { // test_detection_uses_the_anthropic_clients_path_not_the_openai_path
	base := "https://provider.example/api/v4"
	var calls []string
	swap(t, &post, func(u string, _ map[string]any, _ map[string]string) (int, string) {
		calls = append(calls, u)
		// A messages-shaped endpoint is not proof that the native client, which
		// appends /v1/messages, can reach it.
		if u == base+"/messages" || u == base+"/chat/completions" {
			return 200, ""
		}
		return 404, ""
	})
	dialect, _, _ := dialectOf(base, "test-key", "m", true)
	if dialect != "chat" || calls[0] != base+"/v1/messages" {
		t.Fatalf("dialect %q, first call %q", dialect, calls[0])
	}
	cfg := render([]Row{row("anthropic")}, "test-key", 1234, base, "/unused", "", "", nil, false)
	if !strings.Contains(cfg, `base-url: "`+base+`"`) {
		t.Fatal("passthrough base-url is not the bare upstream")
	}
}

func TestRenderUpstreamProxyOnlyWhenSet(t *testing.T) { // test_upstream_proxy_is_rendered_only_when_set
	cfg := render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "http://127.0.0.1:18480", "", nil, false)
	if !strings.Contains(cfg, "\nproxy-url: \"http://127.0.0.1:18480\"\n") {
		t.Fatal("proxy-url missing")
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil, false), "proxy-url") {
		t.Fatal("proxy-url rendered without a proxy")
	}
}

func TestRenderFiltersBlockBinding(t *testing.T) { // test_passthrough_filters_translator_block_binding
	payload := strings.SplitN(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil, false), "\npayload:\n", 2)[1]
	if !strings.Contains(payload, "  filter:\n") ||
		!strings.Contains(payload, "protocol: \"claude\"\n      params:\n        - \"thinking.block_binding\"\n") {
		t.Fatalf("filter missing:\n%s", payload)
	}
	if strings.Contains(render([]Row{row("chat")}, "k", 1234, "https://gw", "/unused", "", "", nil, false), "payload:") {
		t.Fatal("chat-only config carries a payload block")
	}
}

func TestRenderCloakNeverOnlyForOldTranslators(t *testing.T) {
	const never = "    cloak:\n      mode: \"never\"\n"
	if !strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil, true), never) {
		t.Fatal("old translator: cloak never missing")
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil, false), "cloak:") {
		t.Fatal("new translator: cloak rendered")
	}
}

func TestCloakNeverForBanner(t *testing.T) {
	for banner, want := range map[string]bool{
		"CLIProxyAPI Version: 7.2.110, Commit: x, BuiltAt: y\n": true,
		"CLIProxyAPI Version: 8.0.33, Commit: x, BuiltAt: y\n":  true,
		"CLIProxyAPI Version: 8.0.34, Commit: x, BuiltAt: y\n":  false,
		"CLIProxyAPI Version: 8.0.45, Commit: Homebrew\nUsage":  false,
		"CLIProxyAPI Version: v8.1.0, Commit: x\n":              false,
		"CLIProxyAPI Version: 9.0.0, Commit: x\n":               false,
		"CLIProxyAPI Version: dev, Commit: x\n":                 false,
		"":                                                      false,
	} {
		if got := cloakNeverForBanner(banner); got != want {
			t.Errorf("%q: got %v want %v", banner, got, want)
		}
	}
}

func TestCloakNeverForRunsTheBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "cliproxyapi")
	writeFile(t, bin, "#!/bin/sh\n[ \"$1\" = -h ] || exit 2\necho 'CLIProxyAPI Version: 7.2.110, Commit: x'\n", 0o755)
	if !cloakNeverFor(bin) {
		t.Fatal("7.2.110 binary not detected as old")
	}
	if cloakNeverFor(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("missing binary must count as new")
	}
}

func TestRenderUserAgentFromTheShimItRuns(t *testing.T) { // test_passthrough_user_agent_comes_from_the_shim_the_adapter_runs
	shimPath := filepath.Join(t.TempDir(), "shim")
	writeFile(t, shimPath, "#!/bin/sh\n[ \"$1\" = translator-user-agent ] || exit 2\n"+
		"[ -n \"$CCMORPH_USER_AGENT\" ] && echo \"$CCMORPH_USER_AGENT 9.9.9\"\nexit 0\n", 0o755)
	env := append(os.Environ(), "CCMORPH_USER_AGENT=hook-ua/")
	cfg := render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", shimPath, env, false)
	entry := strings.SplitN(cfg, "claude-api-key:", 2)[1]
	if !strings.Contains(entry, `User-Agent: "hook-ua/ 9.9.9"`) || strings.Contains(entry, "cloak:") { // cloak "never" strips Claude Code's identity on 8.0.34+
		t.Fatalf("entry:\n%s", entry)
	}
	var unset []string
	for _, e := range env {
		if !strings.HasPrefix(e, "CCMORPH_USER_AGENT=") {
			unset = append(unset, e)
		}
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", shimPath, unset, false), "headers:") {
		t.Fatal("headers rendered without a profile User-Agent")
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil, false), "headers:") {
		t.Fatal("headers rendered without a shim")
	}
}

func TestOutputCapUsesVersionedBase(t *testing.T) { // test_output_cap_uses_versioned_base_without_duplicate_v1
	var calls []string
	swap(t, &post, func(u string, _ map[string]any, _ map[string]string) (int, string) {
		calls = append(calls, u)
		return 400, "supports at most 8192 completion tokens"
	})
	got := outputCap("https://gateway/api/v4", "k", "m", "chat")
	if got == nil || *got != 8192 || !reflect.DeepEqual(calls, []string{"https://gateway/api/v4/chat/completions"}) {
		t.Fatalf("cap %v, calls %v", got, calls)
	}
}

func modelsServer(t *testing.T, data func() any) (string, *[]string) {
	t.Helper()
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		jsonReply(w, map[string]any{"data": data()})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &methods
}

func TestDialectAutoFollowsMetadata(t *testing.T) { // test_protocol_uses_metadata_or_openai_compatible_default
	for _, c := range []struct {
		endpoints any
		want      string
	}{{[]any{"/v1/messages"}, "anthropic"}, {[]any{"/v1/responses"}, "responses"},
		{[]any{"/v1/chat/completions"}, "chat"}, {nil, "unusable"}} {
		base, methods := modelsServer(t, func() any {
			return []any{map[string]any{"id": "m", "supported_endpoints": c.endpoints}}
		})
		if got, _, _ := dialectAuto(base, "k", "m"); got != c.want {
			t.Errorf("%v: got %q, want %q", c.endpoints, got, c.want)
		}
		for _, m := range *methods {
			if m != "GET" {
				t.Errorf("auto detection sent %s", m)
			}
		}
	}
}

func TestDialectAutoDuplicateRowsUseFirstMatch(t *testing.T) { // test_duplicate_endpoint_declarations_use_first_matching_row
	for _, c := range []struct {
		first, second any
		want          string
	}{
		{[]any{"/v1/messages"}, []any{"/v1/chat/completions"}, "anthropic"},
		{[]any{"/v1/chat/completions"}, []any{"/v1/messages"}, "chat"},
		{nil, []any{"/v1/chat/completions"}, "unusable"},
		{[]any{}, []any{"/v1/chat/completions"}, "chat"},
		{[]any{"/v1/messages/count_tokens"}, []any{"/v1/chat/completions"}, "chat"},
		{[]any{"/unknown"}, []any{"/v1/responses"}, "responses"},
	} {
		base, _ := modelsServer(t, func() any {
			return []any{map[string]any{"id": "m", "supported_endpoints": c.first},
				map[string]any{"id": "m", "supported_endpoints": c.second}}
		})
		if got, _, _ := dialectAuto(base, "k", "m"); got != c.want {
			t.Errorf("%v / %v: got %q, want %q", c.first, c.second, got, c.want)
		}
	}
}

// The listing walk is the one read of a /v1/models listing (#479): the id
// list's filters and the per-model endpoint lookup were each spelled twice —
// inventory/listedModels and dialectAuto/declaredDialect — and the pairs
// could drift. Pinned here once, over one listing holding every row shape,
// including the asymmetry the walk must keep: a capabilities denial drops a
// row from the id list but hides nothing from the endpoint lookup, and an
// unsafe id is still findable by its raw spelling.
func TestTheListingWalkReadsEveryRowShapeOnceIssue479(t *testing.T) {
	data := []any{
		"not an object",
		map[string]any{"id": 7},
		map[string]any{"id": ""},
		map[string]any{"id": "m-evil ok:"},
		map[string]any{"id": "m-denied", "capabilities": map[string]any{"chat_completion": false},
			"supported_endpoints": []any{"/v1/chat/completions"}},
		map[string]any{"id": "m-a"},
		map[string]any{"id": "m-dup"},
		map[string]any{"id": "m-dup", "supported_endpoints": []any{"/v1/messages"}},
		map[string]any{"id": "m-scan", "supported_endpoints": []any{}},
		map[string]any{"id": "m-scan", "supported_endpoints": []any{"/v1/responses", "/v1/chat/completions"}},
		map[string]any{"id": "m-notarray", "supported_endpoints": "no"},
	}
	want := []string{"m-a", "m-dup", "m-dup", "m-notarray", "m-scan", "m-scan"}
	if got := listedModels(data, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("listedModels = %v, want %v", got, want)
	}
	for _, tc := range []struct{ model, want string }{
		{"m-denied", "chat"},    // a capabilities denial hides no declaration
		{"m-evil ok:", ""},      // an unsafe id is still matched as written
		{"m-a", ""},             // no supported_endpoints: declares nothing
		{"m-notarray", ""},      // supported_endpoints that is not an array declares nothing
		{"m-dup", ""},           // the first row naming the model ends the search
		{"m-scan", "responses"}, // an empty declaration is read past, and responses outranks chat
	} {
		if got := declaredDialect(data, tc.model); got != tc.want {
			t.Errorf("declaredDialect(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
	// The unsafe-id log is the walk's own, so inventory and the quiet readers
	// share one filter chain.
	output := captureStdio(t)
	walkListing(data, true)
	if text := output(); !strings.Contains(text, "unsafe characters") || !strings.Contains(text, "m-evil ok:") {
		t.Fatalf("the walk did not log the unsafe id: %s", text)
	}
}

// usableNative is the one fold over classified rows that doctor's verdict and
// serve's fast path read (#479); it was spelled in both places before, and a
// new Row field remembered in only one of them is how doctor and launch
// disagreed in #412 and #475. Every term is pinned here once.
func TestUsableNativeFoldsEveryRowTermOnceIssue479(t *testing.T) {
	cap8192 := 8192
	cases := []struct {
		name        string
		rows        []Row
		usable      []string
		native      bool
		recyclesIDs bool
	}{
		{"no rows at all", nil, nil, true, false},
		{"an unusable row serves nothing", []Row{row("unusable")}, nil, true, false},
		{"an unusable row contributes nothing, even its flags", []Row{{Model: "m", Dialect: "unusable", IDRecycle: true}}, nil, true, false},
		{"plain anthropic is native", []Row{row("anthropic")}, []string{"m"}, true, false},
		{"another dialect needs the translator", []Row{row("responses")}, []string{"m"}, false, false},
		{"a capped row needs the translator", []Row{{Model: "m", Dialect: "anthropic", Status: 200, Cap: &cap8192}}, []string{"m"}, false, false},
		{"a reason-by-default row needs the translator", []Row{{Model: "m", Dialect: "anthropic", Status: 200, Nothink: true}}, []string{"m"}, false, false},
		{"an id-recycling row needs the translator and fresh ids", []Row{{Model: "m", Dialect: "anthropic", Status: 200, IDRecycle: true}}, []string{"m"}, false, true},
		{"one workaround row among native ones breaks native", []Row{{Model: "m-plain", Dialect: "anthropic", Status: 200}, {Model: "m-think", Dialect: "anthropic", Status: 200, Nothink: true}}, []string{"m-plain", "m-think"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usable, native, recyclesIDs := usableNative(tc.rows)
			var models []string
			for _, r := range usable {
				models = append(models, r.Model)
			}
			if !reflect.DeepEqual(models, tc.usable) || native != tc.native || recyclesIDs != tc.recyclesIDs {
				t.Fatalf("usableNative = (%v, native=%v, recyclesIDs=%v), want (%v, %v, %v)",
					models, native, recyclesIDs, tc.usable, tc.native, tc.recyclesIDs)
			}
		})
	}
}

func TestClassifyProbesOnlySelectedModelOnce(t *testing.T) { // test_missing_declaration_probes_only_selected_model_once
	cache := filepath.Join(t.TempDir(), "cache.json")
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { return "unusable", noInference, 0 })
	calls := 0
	swap(t, &dialectOfFn, func(_, _, _ string, bounded bool) (string, string, int) {
		if !bounded {
			t.Error("selected-model probe is not bounded")
		}
		calls++
		return "chat", "", 200
	})
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) { panic("unrelated model scan") })
	for range 2 {
		if _, err := classify("https://gateway", "k", cache, "m", true); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("probed %d times, want 1", calls)
	}
}

func seededCache(t *testing.T, endpoint string, withPath bool, rows ...map[string]any) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "cache.json")
	blob := map[string]any{"endpoint": endpoint, "thinking_probe": thinkingProbeVersion, "rows": rows}
	if withPath {
		blob["anthropic_path"] = "/v1/messages"
	}
	writeCacheJSON(t, cache, blob)
	return cache
}

func TestClassifyLockedSkipsInventoryWhenSelectedModelCached(t *testing.T) { // test_selected_model_cached_while_waiting_on_lock_skips_inventory
	cache := seededCache(t, "https://gateway", true, rowJSON(row("anthropic")))
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) { return []string{"m", "uncached"}, nil })
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { panic("probe") })
	swap(t, &dialectOfFn, func(_, _, _ string, _ bool) (string, string, int) { panic("probe") })
	rows, err := classifyLocked("https://gateway", "k", cache, "m", true)
	if err != nil || len(rows) != 1 || rows[0].Model != "m" {
		t.Fatalf("rows %v err %v", rows, err)
	}
}

func TestBoundedCompatibilityRequestsHaveOutputBounds(t *testing.T) { // test_compatibility_requests_have_output_bounds
	var bodies []map[string]any
	swap(t, &post, func(_ string, b map[string]any, _ map[string]string) (int, string) {
		bodies = append(bodies, b)
		return 400, "unsupported"
	})
	dialectOf("https://gateway", "k", "m", true)
	if len(bodies) != 4 {
		t.Fatalf("%d requests, want 4", len(bodies))
	}
	for _, b := range bodies {
		n, ok := b["max_tokens"].(int)
		if !ok {
			n, ok = b["max_output_tokens"].(int)
		}
		if b["model"] != "m" || !ok || n <= 0 || n > 16 {
			t.Errorf("unbounded request: %v", b)
		}
	}
}

func TestTransientCompatibilityFailureIsNotCached(t *testing.T) { // test_failed_compatibility_does_not_cache_transient_result
	cache := filepath.Join(t.TempDir(), "cache.json")
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { return "unusable", "no metadata", 0 })
	swap(t, &dialectOfFn, func(_, _, _ string, _ bool) (string, string, int) { return "unusable", "timeout", 0 })
	code, did := exits(t, func() { classify("https://gateway", "k", cache, "m", true) })
	if !did || code != 1 {
		t.Fatalf("exit %d did %v, want a failing exit", code, did)
	}
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("a transient result was cached")
	}
}

func TestDeclaredRowsSkipPaidFollowUpProbes(t *testing.T) { // test_declared_rows_skip_paid_follow_up_probes
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { return "chat", "declared", 200 })
	swap(t, &outputCapFn, func(_, _, _, _ string) *int { panic("paid probe") })
	swap(t, &reasonsFn, func(_, _, _ string) bool { panic("paid probe") })
	swap(t, &recyclesFn, func(_, _, _ string) bool { panic("paid probe") })
	rows, err := classify("https://gateway", "k", filepath.Join(t.TempDir(), "cache.json"), "m", true)
	if err != nil || rows[0].Dialect != "chat" {
		t.Fatalf("rows %v err %v", rows, err)
	}
}

func TestOldDefaultChatCacheRowIsReclassified(t *testing.T) { // test_old_default_chat_cache_row_is_reclassified
	chat := rowJSON(row("chat"))
	chat["note"] = "default OpenAI-compatible protocol"
	cache := seededCache(t, "https://gateway", false, chat)
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) { panic("inventory") })
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { return "unusable", "no metadata", 0 })
	calls := 0
	swap(t, &dialectOfFn, func(_, _, _ string, bounded bool) (string, string, int) {
		calls++
		return "anthropic", "", 200
	})
	rows, err := classify("https://gateway", "k", cache, "m", true)
	if err != nil || rows[0].Dialect != "anthropic" || calls != 1 {
		t.Fatalf("rows %v err %v calls %d", rows, err, calls)
	}
}

func TestPreFixCacheIsDroppedPostFixKept(t *testing.T) { // test_old_versioned_anthropic_verdicts_are_rechecked_once
	chat, failed := rowJSON(row("chat")), rowJSON(row("unusable"))
	chat["model"], failed["model"], failed["status"] = "chat-model", "failed-model", 404
	endpoint := "https://gateway/api/v4"
	cache := seededCache(t, endpoint, false, rowJSON(row("anthropic")), chat, failed)
	if got := cachedRows(endpoint, cache); len(got) != 0 {
		t.Fatalf("pre-fix cache served %d rows", len(got))
	}
	cache = seededCache(t, endpoint, true, rowJSON(row("anthropic")), chat, failed)
	if got := cachedRows(endpoint, cache); len(got) != 3 {
		t.Fatalf("post-fix cache served %d rows, want 3", len(got))
	}
}

func stubProfile(t *testing.T, env map[string]string) {
	t.Helper()
	swap(t, &profileEnvFn, func(string) map[string]string { return env })
	swap(t, &detachStdin, false)
}

func TestSelectedModelMapIsClassifiedAndForwarded(t *testing.T) { // test_selected_model_map_is_classified_and_forwarded_to_the_native_edge
	// The wire rule: Claude Code sends the bare id however the picker row was
	// spelled, and the shim routes on that same bare id, so preparation reads the
	// map on it too -- never on the [1m] spelling.
	for _, c := range []struct {
		mapping  map[string]any
		expected string
	}{
		{map[string]any{"claude-sonnet-5-5": "gpt-6.1-sol", "gpt-6.1-sol": "do-not-chain"}, "gpt-6.1-sol"},
		{map[string]any{"claude-sonnet-5-5[1m]": "gpt-6.1-sol", "claude-sonnet-5-5": "gpt-6.1-sol-bare"}, "gpt-6.1-sol-bare"},
	} {
		raw, _ := json.Marshal(c.mapping)
		stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway", "CCMORPH_MODEL_MAP": string(raw)})
		var required string
		swap(t, &classifyFn, func(_, _, _, req string, _ bool) ([]Row, error) { required = req; return []Row{row("anthropic")}, nil })
		var gotMap map[string]any
		swap(t, &frontDirectlyFn, func(_ *cliArgs, _, _, _ string, _ []Row, _ map[string]string, _ []string, m map[string]any) int {
			gotMap = m
			return 0
		})
		if got := Main([]string{"demo", "--auto", "--model", "claude-sonnet-5-5[1m]"}); got != 0 {
			t.Fatalf("Main = %d", got)
		}
		if required != c.expected || !reflect.DeepEqual(gotMap, c.mapping) {
			t.Errorf("required %q (want %q), map %v", required, c.expected, gotMap)
		}
	}
}

func TestNativeLaunchRejectsMappedChatBeforeUpstreamPost(t *testing.T) { // test_native_launch_rejects_later_mapped_chat_request_before_upstream_post + the versioned variant
	for _, prefix := range []string{"", "/api/v4"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					want := prefix + "/v1/models"
					if prefix != "" {
						want = prefix + "/models"
					}
					// the shim relays its own readiness probe to <prefix>/v1/models
					if r.URL.Path != want && r.URL.Path != prefix+"/v1/models" {
						http.NotFound(w, r)
						return
					}
					jsonReply(w, map[string]any{"data": []any{
						map[string]any{"id": "m", "supported_endpoints": []any{"/v1/messages"}},
						map[string]any{"id": "chat-only", "supported_endpoints": []any{"/v1/chat/completions"}}}})
					return
				}
				var payload map[string]any
				json.NewDecoder(r.Body).Decode(&payload)
				mu.Lock()
				seen = append(seen, r.URL.Path+" "+payload["model"].(string))
				mu.Unlock()
				jsonReply(w, map[string]any{"id": "msg_native", "type": "message", "role": "assistant",
					"model": payload["model"], "content": []any{}, "usage": map[string]any{}})
			}))
			defer gateway.Close()
			upstream := gateway.URL + prefix
			mapping := map[string]any{"claude-sonnet-5-5": "chat-only[1m]"}
			raw, _ := json.Marshal(mapping)
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_STATE_HOME", dir)
			stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": upstream, "CCMORPH_MODEL_MAP": string(raw)})
			swap(t, &profileDirs, dir)
			var required string
			swap(t, &classifyFn, func(_, _, _, req string, _ bool) ([]Row, error) { required = req; return []Row{row("anthropic")}, nil })
			edge := portOutsideTheEphemeralPool(t)
			swap(t, &freePortFn, func() (int, error) { return edge, nil })
			swap(t, &exitFn, func(int) { runtime.Goexit() })
			exiting.Store(false)
			swap(t, &parentPoll, 100*time.Millisecond)
			parent := exec.Command("sleep", "120")
			parent.Start()
			defer parent.Process.Kill()
			done := make(chan struct{})
			go func() {
				defer close(done)
				Main([]string{"demo", "--auto", "--model", "m", "--parent-pid", strconv.Itoa(parent.Process.Pid)})
			}()
			// A cold macOS runner can take well over 10 s to start the shim; the
			// adapter itself allows readyTimeout (90 s).
			ready := false
			for end := time.Now().Add(60 * time.Second); !ready && time.Now().Before(end); {
				ready = serving(edge, "k")
				time.Sleep(50 * time.Millisecond)
			}
			if !ready {
				t.Fatal("the native edge never served")
			}
			if d, _, _ := dialectAuto(upstream, "k", "m"); d != "anthropic" {
				t.Fatalf("dialect %q", d)
			}
			send := func(model, path string) (int, map[string]any) {
				body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1,
					"messages": []any{map[string]any{"role": "user", "content": "hello"}}})
				req, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(edge)+path, bytes.NewReader(body))
				req.Header.Set("content-type", "application/json")
				req.Header.Set("x-api-key", "k")
				resp, err := loopback.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var out map[string]any
				json.NewDecoder(resp.Body).Decode(&out)
				return resp.StatusCode, out
			}
			if code, out := send("m", "/v1/messages"); code != 200 || out["model"] != "m" {
				t.Fatalf("native request: %d %v", code, out)
			}
			want := []string{prefix + "/v1/messages m"}
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
				code, out := send("claude-sonnet-5-5[1m]", path)
				e, _ := out["error"].(map[string]any)
				msg, _ := e["message"].(string)
				if code != 400 || e["type"] != "invalid_request_error" || !strings.Contains(msg, "chat-only") || !strings.Contains(msg, "translation") {
					t.Fatalf("%s: %d %v", path, code, out)
				}
				mu.Lock()
				if !reflect.DeepEqual(seen, want) {
					t.Fatalf("rejected request reached upstream: %v", seen)
				}
				mu.Unlock()
			}
			parent.Process.Kill() // the parent dies: the adapter shuts the shim down and exits
			parent.Wait()         // a zombie still answers kill(pid, 0)
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("adapter did not stop with its parent")
			}
			if required != "m" {
				t.Errorf("classified %q, want m", required)
			}
		})
	}
}

func TestCachedNativeMainNeedsNoTranslator(t *testing.T) { // test_cached_native_main_needs_no_translator
	dir := t.TempDir()
	cache := filepath.Join(dir, ".adapter-cache", hashHex("https://gateway")[:16]+".json")
	os.MkdirAll(filepath.Dir(cache), 0o755)
	writeCacheJSON(t, cache, map[string]any{"endpoint": "https://gateway", "anthropic_path": "/v1/messages",
		"thinking_probe": thinkingProbeVersion, "rows": []any{rowJSON(row("anthropic"))}})
	swap(t, &profileDirs, dir)
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway"})
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) { panic("network") })
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { panic("probe") })
	swap(t, &freePortFn, func() (int, error) { panic("translator state") })
	upstream := ""
	swap(t, &frontDirectlyFn, func(_ *cliArgs, up, _, _ string, _ []Row, _ map[string]string, _ []string, _ map[string]any) int {
		upstream = up
		return 0
	})
	if got := Main([]string{"demo", "--auto", "--model", "m"}); got != 0 || upstream != "https://gateway" {
		t.Fatalf("Main = %d, upstream %q", got, upstream)
	}
}

func TestAModelMustBeNamedWhenTheGatewayOffersNoList(t *testing.T) { // issue #357
	// The headless launch can discover no model without a listing, so it must
	// refuse — and the refusal must carry the cause and the one step to the
	// ready-file reader, not end as a bare process death.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".adapter-cache"), 0o755)
	swap(t, &profileDirs, dir)
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway"})
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) {
		return nil, refusingStatus{404}
	})
	swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) { panic("no probe can name a model") })
	swap(t, &frontDirectlyFn, func(_ *cliArgs, _, _, _ string, _ []Row, _ map[string]string, _ []string, _ map[string]any) int {
		panic("nothing is served")
	})
	stdio := captureStdio(t)
	var status int
	exits(t, func() { status = Main([]string{"demo", "--auto"}) })
	if status != 1 {
		t.Fatalf("Main = %d, want 1 (a launch without a model on a listless gateway must refuse)", status)
	}
	out := stdio()
	if !strings.Contains(out, "EXIT=") || !strings.Contains(out, "offers no model list (HTTP 404)") {
		t.Fatalf("the refusal must say why in plain words, got: %s", out)
	}
	if !strings.Contains(out, "ccmorph set demo model <model-id>") {
		t.Fatalf("the refusal must name the step, got: %s", out)
	}
}

func TestARefusedListingNamesItsTrueCause(t *testing.T) { // issues #357, #374
	// A rejected key is not an absent model list: the refusal must name the
	// credential and its repair, never a model-setting step that cannot fix it —
	// on whichever path the launch met the listing, and without spending
	// inference requests a rejected key answers the same way (#374).
	for _, tc := range []struct {
		name  string
		argv  []string
		setup func(t *testing.T) func() int
	}{
		{"no model named", []string{"demo", "--auto"}, func(t *testing.T) func() int {
			swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) {
				return nil, refusingStatus{401}
			})
			return func() int { return 0 }
		}},
		{"a model is named", []string{"demo", "--auto", "--model", "m"}, func(t *testing.T) func() int {
			swap(t, &dialectAutoFn, func(_, _, _ string) (string, string, int) {
				return "unusable", noInference + ": HTTP Error 401", 401
			})
			calls := 0
			swap(t, &dialectOfFn, func(_, _, _ string, _ bool) (string, string, int) {
				calls++
				return "unusable", "HTTP Error 401", 401
			})
			return func() int { return calls }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, ".adapter-cache"), 0o755)
			swap(t, &profileDirs, dir)
			stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway"})
			inferences := tc.setup(t)
			swap(t, &frontDirectlyFn, func(_ *cliArgs, _, _, _ string, _ []Row, _ map[string]string, _ []string, _ map[string]any) int {
				panic("nothing is served")
			})
			stdio := captureStdio(t)
			// The refusal must be Main's own return, never a death on the way:
			// exits discards the intercepted code, so a die leaves status 0.
			var status int
			exits(t, func() { status = Main(tc.argv) })
			if status != 1 {
				t.Fatalf("Main = %d, want 1", status)
			}
			out := stdio()
			if !strings.Contains(out, "EXIT=") || !strings.Contains(out, "rejected the API key (HTTP 401)") || !strings.Contains(out, "replace it: ccmorph config demo") {
				t.Fatalf("the refusal must be the structured EXIT= line naming the rejected key and its repair, got: %s", out)
			}
			if strings.Contains(out, "offers no model list") || strings.Contains(out, "set demo model") {
				t.Fatalf("a rejected key is not an absent model list, so the refusal must not say it is; got: %s", out)
			}
			if strings.Contains(out, "model m is unusable") {
				t.Fatalf("a rejected key is not a verdict about the model, so the refusal must not say it is; got: %s", out)
			}
			if n := inferences(); n != 0 {
				t.Fatalf("%d inference request(s) spent on a key the endpoint had already rejected", n)
			}
		})
	}
}

func TestAWarmCacheIsNotAModelChoice(t *testing.T) { // issue #357
	// Cached rows are endpoint knowledge, not the profile's model choice: with
	// no model named and no listing offered, the launch must refuse and name
	// the step, not front a session that never picked a model.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".adapter-cache"), 0o755)
	swap(t, &profileDirs, dir)
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway"})
	cache := filepath.Join(dir, ".adapter-cache", hashHex("https://gateway")[:16]+".json")
	os.WriteFile(cache, []byte(`{"endpoint":"https://gateway","anthropic_path":"/v1/messages","thinking_probe":2,`+
		`"rows":[{"model":"delta-1","dialect":"anthropic","note":"","cap":null,"nothink":false,"id_recycle":false,"status":200}]}`), 0o600)
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]string, error) {
		return nil, refusingStatus{404}
	})
	served := false
	swap(t, &frontDirectlyFn, func(_ *cliArgs, _, _, _ string, _ []Row, _ map[string]string, _ []string, _ map[string]any) int {
		served = true
		return 0
	})
	stdio := captureStdio(t)
	var status int
	exits(t, func() { status = Main([]string{"demo", "--auto"}) })
	if served {
		t.Fatal("a warm cache fronted a session with no model chosen")
	}
	if status != 1 {
		t.Fatalf("Main = %d, want 1", status)
	}
	out := stdio()
	if !strings.Contains(out, "EXIT=") || !strings.Contains(out, "offers no model list (HTTP 404)") {
		t.Fatalf("the refusal must say why in plain words, got: %s", out)
	}
	if !strings.Contains(out, "ccmorph set demo model <model-id>") {
		t.Fatalf("the refusal must name the step, got: %s", out)
	}
}

// refusingTransport is the issue's stand-in gateway as an HTTP transport: it
// answers the model list (HTTP 200) but refuses every /v1/messages request with
// the given status and body, and answers every other message path with the 404
// of a route it does not implement. Swapped into the adapter's client, the
// whole launch path runs for real over it with no socket.
type refusingTransport struct {
	status int
	body   string
	calls  []string
}

func (f *refusingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	code, body := 200, `{"data":[{"id":"eta-opus-1","object":"model"}]}`
	if r.Method == "POST" {
		if r.URL.Path == "/v1/messages" {
			code, body = f.status, f.body
		} else {
			code, body = 404, `{"error":"no route"}`
		}
	}
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Body:   io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// refuseMessages stands the refusing gateway up for the test and guards the
// launch: a refused message must never front a session.
func refuseMessages(t *testing.T, status int, body string) *refusingTransport {
	t.Helper()
	gw := &refusingTransport{status: status, body: body}
	swap(t, &client, &http.Client{Transport: gw})
	swap(t, &frontDirectlyFn, func(_ *cliArgs, _, _, _ string, _ []Row, _ map[string]string, _ []string, _ map[string]any) int {
		panic("a refused message must not front a session")
	})
	return gw
}

func TestARefusedMessageEndsTheLaunchWithItsStatusAndText(t *testing.T) { // issue #375
	// The issue's three stand-ins: a gateway that lists its models (HTTP 200)
	// but refuses every message — an empty balance, a rate limit, a broken
	// upstream. The refusal is the launch's true cause: it must reach the
	// ready-file reader with the status and the gateway's own words, never as
	// an adapter death, and one refused answer must be enough to end it. The
	// launcher passes a REFUSED line through untouched, so the repair its
	// siblings carry has to be on this line too (#448): a balance or a rate
	// limit is the provider's own account and names the endpoint this profile
	// reaches, and any other refusal is the gateway's own failure, where
	// doctor is the one command that checks the route.
	for _, standIn := range []struct {
		name        string
		status      int
		body        string
		explanation string
		repair      string
	}{
		{"eta-billing", 402,
			`{"type":"error","error":{"type":"billing_error","message":"Insufficient balance: top up your account to continue"}}`,
			"Insufficient balance: top up your account to continue",
			"top up the provider account, then retry; or change the route: ccmorph config demo"},
		{"theta-rate-limit", 429,
			`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit reached: 3 requests per minute"}}`,
			"Rate limit reached: 3 requests per minute",
			"wait for the provider's rate limit to reset, then retry; or change the route: ccmorph config demo"},
		{"eps-server-error", 500,
			`{"error":{"message":"mock gateway exploded"}}`,
			"mock gateway exploded",
			"retry, or check the route: ccmorph doctor demo"},
	} {
		t.Run(standIn.name, func(t *testing.T) {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, ".adapter-cache"), 0o755)
			swap(t, &profileDirs, dir)
			stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://standin"})
			gw := refuseMessages(t, standIn.status, standIn.body)
			stdio := captureStdio(t)
			var status int
			exits(t, func() { status = Main([]string{"demo", "--auto", "--model", "eta-opus-1"}) })
			if status != 1 {
				t.Fatalf("Main = %d, want 1 (a refused message must not start a session)", status)
			}
			out := stdio()
			for _, want := range []string{"EXIT=", fmt.Sprintf("HTTP %d", standIn.status), standIn.explanation, standIn.repair} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal must say %q, got: %s", want, out)
				}
			}
			// Not an adapter problem, and not a different error than the one the
			// gateway answered: the old wording called the model unusable and
			// carried the 404 of a path the stand-in does not implement.
			for _, stale := range []string{"unusable", "no route", "exited before readiness"} {
				if strings.Contains(out, stale) {
					t.Errorf("the refusal must not say %q, got: %s", stale, out)
				}
			}
			// One refused answer ends the launch: the remaining dialect probes
			// would ask the same gateway the same refused question.
			if want := []string{"GET /v1/models", "POST /v1/messages"}; !slices.Equal(gw.calls, want) {
				t.Errorf("the gateway was asked %v, want exactly %v", gw.calls, want)
			}
		})
	}
}

func TestARefusedMessageIsNotSettledAgainstTheNextLaunch(t *testing.T) { // issue #375
	// A refusal the gateway answered is account or gateway state, not a
	// property of the model: no launch may cache it as settled knowledge, or
	// the next one would die on the stale verdict instead of reporting the
	// gateway's fresh answer.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".adapter-cache"), 0o755)
	swap(t, &profileDirs, dir)
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://standin"})
	refuseMessages(t, 402, `{"error":{"message":"Insufficient balance: top up your account to continue"}}`)
	for launch := 1; launch <= 2; launch++ {
		stdio := captureStdio(t)
		var status int
		exits(t, func() { status = Main([]string{"demo", "--auto", "--model", "eta-opus-1"}) })
		out := stdio()
		if status != 1 || !strings.Contains(out, "EXIT=") || !strings.Contains(out, "HTTP 402") ||
			!strings.Contains(out, "Insufficient balance: top up your account to continue") ||
			!strings.Contains(out, "or change the route: ccmorph config demo") {
			t.Fatalf("launch %d: the refusal must reach the reader with its status, text and repair (status %d): %s", launch, status, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".adapter-cache", hashHex("https://standin")[:16]+".json")); !os.IsNotExist(err) {
		t.Fatalf("a refused message must not be cached as settled knowledge: %v", err)
	}
}

func TestALegacyCachedRefusalIsReaskedNotServed(t *testing.T) { // issue #375
	// A version before the fix settled a refused message as a cached unusable
	// verdict — and the row it actually wrote carried the LAST probe's 404 and
	// its "no route" note, with status 404, because the refusal's own 402 was
	// overwritten by the later probes. The cache must not stand between the
	// person and the gateway's fresh answer.
	dir := t.TempDir()
	cache := filepath.Join(dir, ".adapter-cache", hashHex("https://standin")[:16]+".json")
	os.MkdirAll(filepath.Dir(cache), 0o755)
	writeCacheJSON(t, cache, map[string]any{"endpoint": "https://standin", "anthropic_path": "/v1/messages",
		"thinking_probe": thinkingProbeVersion,
		"rows": []any{rowJSON(Row{Model: "eta-opus-1", Dialect: "unusable",
			Note: `{"error":"no route"}`, Status: 404})}})
	swap(t, &profileDirs, dir)
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://standin"})
	gw := refuseMessages(t, 402, `{"error":{"message":"Insufficient balance: top up your account to continue"}}`)
	stdio := captureStdio(t)
	var status int
	exits(t, func() { status = Main([]string{"demo", "--auto", "--model", "eta-opus-1"}) })
	out := stdio()
	if status != 1 || !strings.Contains(out, "EXIT=") || !strings.Contains(out, "HTTP 402") ||
		!strings.Contains(out, "Insufficient balance: top up your account to continue") {
		t.Fatalf("the fresh refusal must reach the reader (status %d): %s", status, out)
	}
	if strings.Contains(out, "cached as unusable") || strings.Contains(out, "no route") {
		t.Fatalf("a cached refusal must not be served as knowledge: %s", out)
	}
	if want := []string{"GET /v1/models", "POST /v1/messages"}; !slices.Equal(gw.calls, want) {
		t.Errorf("the gateway was asked %v, want exactly %v", gw.calls, want)
	}
}

// chatOnlyTransport is a chat-only gateway as an HTTP transport: it lists its
// models, answers the Messages API with a 501 that says to use chat
// completions, has no /v1/responses route, and serves /v1/chat/completions.
type chatOnlyTransport struct{ calls []string }

func (f *chatOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	code, body := 200, `{"data":[{"id":"chat-only-1","object":"model"}]}`
	if r.Method == "POST" {
		switch r.URL.Path {
		case "/v1/messages":
			code, body = 501, `{"error":{"message":"Messages API not implemented; use chat completions"}}`
		case "/v1/responses":
			code, body = 404, `{"error":"no route"}`
		case "/v1/chat/completions":
			code, body = 200, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`
		}
	}
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Body:   io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

func TestAnUnimplementedPathIsNotARefusalOfEveryDialect(t *testing.T) { // issue #375
	// The merge gate's finding: a 501 answers "this gateway does not implement
	// that operation" — a property of the path, not a refusal of the model. A
	// chat-only gateway must classify through its working dialect instead of
	// dying on the Messages API's 501 (INTENT: any provider's model can run).
	gw := &chatOnlyTransport{}
	swap(t, &client, &http.Client{Transport: gw})
	dir := t.TempDir()
	rows, err := classify("https://standin", "k", filepath.Join(dir, "cache.json"), "chat-only-1", true)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "chat-only-1" || rows[0].Dialect != "chat" {
		t.Fatalf("rows %v, want chat-only-1 speaking chat", rows)
	}
	// The sweep walked past the 501 to the working dialect.
	if want := []string{"GET /v1/models", "POST /v1/messages", "POST /v1/responses",
		"POST /v1/chat/completions"}; !slices.Equal(gw.calls, want) {
		t.Errorf("the gateway was asked %v, want %v", gw.calls, want)
	}
}

func TestOnlyTheDirectRouteRetriesInTheShim(t *testing.T) { // test_only_the_direct_route_retries_in_the_shim
	// The in-process shim takes the same Options; the spawned hooks build shows the argv.
	cmd := spawnedDirect(t, map[string]string{"CCMORPH_SHIM_HOOKS": "/stable/hooks.py"})
	if !slices.Contains(cmd.Args, "--retry-transport") {
		t.Fatalf("argv %v", cmd.Args)
	}
}

func TestDirectRouteHandsProfileKeysToTheShimEnv(t *testing.T) { // test_the_direct_route_hands_profile_keys_to_the_shim_env
	cmd := spawnedDirect(t, map[string]string{"CCMORPH_SHIM_HOOKS": "/stable/hooks.py", "CCMORPH_USER_AGENT": "ua/{version}"})
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"CCMORPH_SHIM_HOOKS=/stable/hooks.py", "CCMORPH_USER_AGENT=ua/{version}"} {
		if !strings.Contains(env, want) {
			t.Errorf("shim env lacks %s", want)
		}
	}
	for _, flag := range []string{"--repair-tool-references", "--affinity-header"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("new shim flag %s", flag)
		}
	}
	if cmd.Path != "/stable/hooks.py" {
		t.Errorf("ran %s, not the profile's hooks build", cmd.Path)
	}
}

func TestDirectRouteReadinessNeedsNoModelList(t *testing.T) { // issue #357
	// A gateway whose /v1/models answers 404 still fronts directly: readiness
	// asks the shim itself and never the gateway's model list, so the wait
	// sends the gateway nothing and cannot spin on an answer that never comes.
	swap(t, &readyTimeout, 2*time.Second) // the refusal path must fail fast, not in 90s
	done := make(chan struct{})
	var once sync.Once
	shut := func() { once.Do(func() { close(done) }) }
	swap(t, &freePortFn, func() (int, error) { return 45999, nil })
	swap(t, &spawnShim, func(c *exec.Cmd) (*proc, error) {
		return &proc{done: done, stop: shut}, nil // a shim that stays up until told
	})
	swap(t, &servingFn, func(port int, key string) bool {
		t.Error("readiness asked the gateway's model list (a relayed GET /v1/models)")
		return false
	})
	probes := 0
	swap(t, &answeredFn, func(port int) bool { probes++; shut(); return true })
	stdio := captureStdio(t)
	code, did := exits(t, func() {
		frontDirectly(&cliArgs{profile: "demo", reserved: -1}, "https://gateway", "k", "id",
			[]Row{row("anthropic")}, map[string]string{"CCMORPH_SHIM_HOOKS": "/stable/hooks.py"}, nil, nil)
	})
	if !did || code != 1 { // the fake shim's exit must be what stops the loop
		t.Fatalf("frontDirectly = (%d, exited=%v); the fake shim's exit must end it", code, did)
	}
	if out := stdio(); !strings.Contains(out, "PORT=45999") {
		t.Fatalf("no PORT= line for a gateway without a model list; stdio:\n%s", out)
	}
	if probes != 1 {
		t.Errorf("readiness probed %d time(s); the first answer must end the wait", probes)
	}
}

// spawnedDirect records the command frontDirectly would run, then abandons it.
func spawnedDirect(t *testing.T, extras map[string]string) *exec.Cmd {
	t.Helper()
	var got *exec.Cmd
	swap(t, &spawnShim, func(c *exec.Cmd) (*proc, error) { got = c; return nil, fmt.Errorf("abandoned") })
	swap(t, &freePortFn, func() (int, error) { return 45999, nil })
	exits(t, func() {
		frontDirectly(&cliArgs{profile: "demo", reserved: -1}, "https://gateway", "k", "id", []Row{row("anthropic")}, extras, nil, nil)
	})
	if got == nil {
		t.Fatal("the shim was never spawned")
	}
	return got
}

// --- ports of the white-box tests in tests/adapter_test.py -----------------------

func TestCredentialIsNeverForwardedAcrossARedirect(t *testing.T) { // test_credential_is_never_forwarded_across_a_redirect
	var mu sync.Mutex
	var seen []http.Header
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		jsonReply(w, map[string]any{"data": []any{}})
	}))
	defer sink.Close()
	// A different port is a different origin; "localhost" also makes the host differ.
	target := "http://localhost:" + sink.URL[strings.LastIndex(sink.URL, ":")+1:] + "/v1/models"
	bounce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, target, http.StatusTemporaryRedirect) // 307 preserves method and body
	}))
	defer bounce.Close()

	if _, err := inventory(bounce.URL, "SECRET-KEY", nil); err == nil {
		t.Error("a cross-origin redirect was followed")
	}
	// A refused redirect reads as a transport error: indeterminate, never cached.
	status, _ := postImpl(bounce.URL+"/v1/messages", map[string]any{}, map[string]string{"x-api-key": "SECRET-KEY"})
	if status != 0 || !indeterminate(status) {
		t.Errorf("status %d", status)
	}
	if postJSON(bounce.URL+"/v1/messages", map[string]any{}, map[string]string{"x-api-key": "SECRET-KEY"}) != nil {
		t.Error("postJSON followed the redirect")
	}
	mu.Lock()
	if len(seen) != 0 {
		t.Fatalf("a credential-bearing request reached another origin: %v", seen)
	}
	mu.Unlock()

	// A same-origin redirect is still followed.
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "/moved", http.StatusFound)
			return
		}
		jsonReply(w, map[string]any{"data": []any{map[string]any{"id": "same-origin-ok"}}})
	}))
	defer same.Close()
	if ids, err := inventory(same.URL, "k", nil); err != nil || !reflect.DeepEqual(ids, []string{"same-origin-ok"}) {
		t.Fatalf("same-origin redirect: %v %v", ids, err)
	}
}

func TestStopProcessesWaitsThenKillsAStubbornChild(t *testing.T) { // test_shutdown_waits_then_kills_a_stubborn_child
	dir := t.TempDir()
	ready, term := filepath.Join(dir, "ready"), filepath.Join(dir, "term")
	cmd := exec.Command("sh", "-c", fmt.Sprintf(`trap 'echo got >> %s' TERM; : > %s; while :; do sleep 0.1; done`, term, ready))
	p, err := start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	swap(t, &stopGrace, 400*time.Millisecond)
	began := time.Now()
	stopProcesses(p)
	if !p.exited() {
		t.Fatal("the stubborn child outlived stopProcesses")
	}
	if time.Since(began) < 400*time.Millisecond {
		t.Error("SIGKILL came before the grace period ended")
	}
	if b, _ := os.ReadFile(term); !strings.Contains(string(b), "got") {
		t.Error("the child was never asked to terminate first")
	}
}

func TestTetherWatchesEveryChildOfOneAdapter(t *testing.T) { // test_sigkilled_adapter_does_not_leak_its_children
	// One tether per adapter (#29): the first child starts it with the state
	// dir, and every later child arrives as one more pid on the same stdin.
	// Two starts here would mean two processes watching one adapter, the
	// per-child cost this test exists to keep down.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tether")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \"$TETHER_OUT.argv\"\n"+
		"cat >> \"$TETHER_OUT.stdin\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_OUT", filepath.Join(dir, "out"))
	tetherMu.Lock()
	prev := tetherIn
	tetherIn = nil
	tetherMu.Unlock()
	t.Cleanup(func() {
		tetherMu.Lock()
		if tetherIn != nil {
			tetherIn.Close()
		}
		tetherIn = prev
		tetherMu.Unlock()
	})

	var kids []*proc
	startSleep := func() *proc {
		p, err := start(exec.Command("sleep", "30"))
		if err != nil {
			t.Fatal(err)
		}
		kids = append(kids, p)
		return p
	}
	first, second := startSleep(), startSleep()
	t.Cleanup(func() {
		for _, p := range kids {
			if !p.exited() {
				p.kill()
			}
		}
	})
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	tetherImpl(script, first, state)
	tetherImpl(script, second, "")

	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, "out."+name))
		return string(b)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && strings.Count(read("stdin"), "\n") < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := strings.Split(strings.TrimSpace(read("argv")), "\n"); len(got) != 1 {
		t.Fatalf("one tether per adapter: started %d: %q", len(got), read("argv"))
	} else if want := fmt.Sprintf("tether %d %s", os.Getpid(), state); got[0] != want {
		t.Fatalf("tether argv %q, want %q", got[0], want)
	}
	want := []string{strconv.Itoa(first.cmd.Process.Pid), strconv.Itoa(second.cmd.Process.Pid)}
	if got := strings.Fields(read("stdin")); !reflect.DeepEqual(got, want) {
		t.Fatalf("the tether was handed %v, want every child %v in order", got, want)
	}
}

func TestProfileEnvSourcesThePackagedScript(t *testing.T) { // test_profile_env_sources_the_packaged_ccmorph_script
	var argv []string
	swap(t, &runCommand, func(_ time.Duration, name string, args ...string) (string, string, int) {
		argv = append([]string{name}, args...)
		return "ANTHROPIC_API_KEY=k\n", "", 0
	})
	got := profileEnv("p; nope")
	if !reflect.DeepEqual(got, map[string]string{"ANTHROPIC_API_KEY": "k"}) {
		t.Fatalf("env %v", got)
	}
	n := len(argv)
	// The profile is a positional argument, never interpolated into the script.
	if !reflect.DeepEqual(argv[n-3:], []string{"ccmorph-adapter", paths.Script(), "p; nope"}) ||
		!strings.Contains(argv[2], `source "$1"`) || !strings.Contains(argv[2], `ccmorph _env "$2"`) {
		t.Fatalf("argv %q", argv)
	}
}

func TestServingBypassesAmbientProxy(t *testing.T) { // ServingNoProxyTest.test_serving_bypasses_ambient_proxy
	proxy, hits := decoy(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "k" {
			return
		}
		w.WriteHeader(401)
	}))
	defer fixture.Close()
	t.Setenv("http_proxy", proxy)
	t.Setenv("HTTP_PROXY", proxy)
	port, _ := strconv.Atoi(fixture.URL[strings.LastIndex(fixture.URL, ":")+1:])
	if !serving(port, "k") || serving(port, "wrong") {
		t.Fatal("serving did not reflect the endpoint's answer")
	}
	if hits() != 0 {
		t.Fatal("serving went through the ambient proxy")
	}
}

func inventoryFixture(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			w.WriteHeader(401)
			return
		}
		jsonReply(w, map[string]any{"data": []any{map[string]any{"id": "m1"}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestLoopbackInventoryBypassesAmbientProxy(t *testing.T) { // InventoryLoopbackNoProxyTest.test_loopback_inventory_bypasses_ambient_proxy
	proxy, hits := decoy(t)
	t.Setenv("http_proxy", proxy)
	t.Setenv("HTTP_PROXY", proxy)
	ids, err := inventory(inventoryFixture(t), "k", loopback)
	if err != nil || !reflect.DeepEqual(ids, []string{"m1"}) || hits() != 0 {
		t.Fatalf("ids %v err %v proxy hits %d", ids, err, hits())
	}
}

func TestEnsureConfigPollBypassesAmbientProxy(t *testing.T) { // InventoryLoopbackNoProxyTest.test_ensure_config_poll_bypasses_ambient_proxy
	proxy, hits := decoy(t)
	t.Setenv("http_proxy", proxy)
	t.Setenv("HTTP_PROXY", proxy)
	translator := inventoryFixture(t)
	conf := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, conf, "old", 0o600)
	up, key := "http://gateway.invalid", "k"
	realInventory := inventory
	swap(t, &inventoryFn, func(b, k string, c *http.Client) ([]string, error) {
		if b == up {
			return []string{"m1"}, nil
		}
		return realInventory(b, k, c)
	})
	swap(t, &classifyFn, func(_, _, _, _ string, _ bool) ([]Row, error) { return nil, nil })
	port, _ := strconv.Atoi(translator[strings.LastIndex(translator, ":")+1:])
	identity := hashHex("[" + q(up) + ", " + q(key) + "]")
	a := &cliArgs{profile: "p", ensureConfig: conf, ensurePort: port, ensureIdentity: identity}
	if got := ensureConfig(a, "m1", up, key, identity, "", "", "", map[string]string{}); got != 0 {
		t.Fatalf("ensureConfig = %d", got)
	}
	if b, _ := os.ReadFile(conf); string(b) == "old" || !strings.Contains(string(b), "port: "+strconv.Itoa(port)) {
		t.Fatalf("config not replaced: %q", b)
	}
	if hits() != 0 {
		t.Fatal("the --ensure-config poll went through the ambient proxy")
	}
}

func TestLoopbackInventoryRefusesCrossOriginRedirect(t *testing.T) { // InventoryLoopbackNoProxyTest.test_loopback_inventory_refuses_cross_origin_redirect
	var mu sync.Mutex
	var hits []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Header.Get("x-api-key"))
		mu.Unlock()
		w.Write([]byte("{}"))
	}))
	defer other.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/models", http.StatusFound)
	}))
	defer redir.Close()
	if _, err := inventory(redir.URL, "k", loopback); err == nil {
		t.Error("LOOPBACK followed a cross-origin redirect")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 0 {
		t.Fatalf("the key reached another origin: %v", hits)
	}
}

func TestFindTranslator(t *testing.T) { // TranslatorDiscoveryTest (4 tests)
	executableFile := func(path string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		writeFile(t, path, "#!/bin/sh\n", 0o755)
	}
	t.Run("on PATH", func(t *testing.T) { // test_a_translator_on_path_is_found
		dir := t.TempDir()
		executableFile(filepath.Join(dir, "cliproxyapi"))
		t.Setenv("PATH", dir)
		if got := findTranslator(); got != filepath.Join(dir, "cliproxyapi") {
			t.Errorf("got %s", got)
		}
	})
	t.Run("per-user install honours HOME", func(t *testing.T) { // test_a_non_homebrew_install_location_is_found + test_a_per_user_install_is_a_candidate_at_all
		dir := t.TempDir()
		want := filepath.Join(dir, ".local/bin/cliproxyapi")
		executableFile(want)
		t.Setenv("PATH", "/nonexistent")
		t.Setenv("HOME", dir)
		got := findTranslator()
		// A host that has a system copy wins over the scratch install; either way
		// the answer is something executable.
		if !executable(got) {
			t.Errorf("not executable: %s", got)
		}
		if _, err := os.Stat("/opt/homebrew/opt/cliproxyapi/bin/cliproxyapi"); err != nil && got != want {
			if !executable("/usr/local/opt/cliproxyapi/bin/cliproxyapi") && !executable("/usr/local/bin/cliproxyapi") && !executable("/usr/bin/cliproxyapi") {
				t.Errorf("per-user install not found: %s", got)
			}
		}
	})
	t.Run("nothing found is actionable", func(t *testing.T) { // test_nothing_found_yields_an_actionable_path_not_a_crash
		t.Setenv("PATH", "/nonexistent")
		t.Setenv("HOME", "/nonexistent")
		if got := findTranslator(); !strings.HasSuffix(got, "cliproxyapi") {
			t.Errorf("got %q", got)
		}
	})
}

// --- Go-only checks of what the port had to reimplement --------------------------

func TestQMatchesPythonJSONDumps(t *testing.T) {
	got := q("a\"b\\c\n\té\U0001F600\x7f")
	want := "\"a\\\"b\\\\c\\n\\t\\u00e9\\ud83d\\ude00\\u007f\""
	if got != want {
		t.Errorf("q = %s, want %s", got, want)
	}
}

func TestRouteIdentityMatchesPython(t *testing.T) {
	// sha256(json.dumps(["http://x", "ké"])) -- the shim echoes this back, so
	// both implementations must produce the same bytes.
	if got, want := hashHex("["+q("http://x")+", "+q("k\u00e9")+"]"), hashHex("[\"http://x\", \"k\\u00e9\"]"); got != want {
		t.Errorf("identity %s != %s", got, want)
	}
}

// Ported from tests/product_defaults_test.py: the fixed defaults are constants, not tuning knobs.
func TestFixedDefaults(t *testing.T) {
	if lockWait != 120*time.Second || jobs != 8 {
		t.Fatalf("lockWait %v jobs %d", lockWait, jobs)
	}
}

// --- GH-47: the Go port's lost deadline, redaction and tool-id panic -----------

// captureStdio points os.Stdout and os.Stderr at one temp file; the returned
// func reads what either wrote so far.
func captureStdio(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdio-")
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = f, f
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		f.Close()
	})
	return func() string {
		raw, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
}

func TestAutoLaunchWithoutModelIsBoundedBySelectDeadline(t *testing.T) {
	// The launcher always passes --auto and adds --model only for a picked model,
	// so both forms must fail fast like the Python's `required_model or args.auto`.
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway"})
	swap(t, &profileDirs, t.TempDir())
	swap(t, &selectTimeout, 50*time.Millisecond)
	stalled := make(chan struct{}) // never closed: the gateway that never answers
	swap(t, &classifyFn, func(_, _, _, _ string, _ bool) ([]Row, error) {
		<-stalled
		return nil, nil
	})
	output := captureStdio(t)
	code := make(chan int, 1)
	swap(t, &exitFn, func(c int) { code <- c; runtime.Goexit() })
	t.Cleanup(func() { exiting.Store(false) })
	exiting.Store(false)

	go Main([]string{"demo", "--auto"})
	select {
	case got := <-code:
		if got != 1 {
			t.Fatalf("exit %d, want 1", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("--auto without --model armed no select deadline")
	}
	if text := output(); !strings.Contains(text, "classifying") || !strings.Contains(text, "exceeded") {
		t.Fatalf("no deadline message on stderr: %q", text)
	}
}

func TestPrintConfigNeverPrintsTheProxyPassword(t *testing.T) {
	// Go's url.Parse refuses these passwords where Python's urlsplit took them;
	// the fallback must still cut the userinfo rather than echo the raw URL.
	// A /, ? or # inside the password ends the authority mid-password (GH-65),
	// so the @ that closes the userinfo sits past the split's netloc and the
	// password's head would print as the host.
	// A digit or empty head before the first /?# reads as a port, so url.Parse
	// accepts the URL and the success path must not print the secret either.
	for name, secret := range map[string]string{"caret": "p^ss", "percent": "50%off", "space": "p ss", "slash": "pa/ss", "question": "pa?ss", "hash": "pa#ss",
		"digit-slash": "12/ss", "digit-question": "12?ss", "empty-head-slash": "/ss"} {
		t.Run(name, func(t *testing.T) {
			stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway",
				"CCMORPH_UPSTREAM_PROXY": "http://alice:" + secret + "@proxy:3128"})
			swap(t, &profileDirs, t.TempDir())
			// Neutral state dir: t.TempDir() puts the subtest name in the path,
			// and a secret spelled "p^ss" must not be excused for that.
			state, err := os.MkdirTemp("", "ccmorph-print-config-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(state) })
			t.Setenv("TMPDIR", state)
			swap(t, &classifyFn, func(_, _, _, _ string, _ bool) ([]Row, error) { return []Row{row("anthropic")}, nil })
			swap(t, &freePortFn, func() (int, error) { return 1234, nil })
			output := captureStdio(t)
			if got := Main([]string{"demo", "--auto", "--print-config"}); got != 0 {
				t.Fatalf("Main = %d", got)
			}
			text := output()
			if strings.Contains(text, secret) || strings.Contains(text, "alice") {
				t.Fatalf("--print-config printed the proxy credentials: %q", text)
			}
			if !strings.Contains(text, `proxy-url: "http://proxy:3128"`) {
				t.Fatalf("proxy-url not redacted to its host: %q", text)
			}
		})
	}
}

// GH-65: a password holding /, ? or # ends the authority inside the password,
// or leaves a digit or empty head that Go reads as a port. Whether url.Parse
// refuses the URL or accepts it, the output must carry no part of the userinfo:
// the host behind the last @, or <redacted> when nothing host-shaped survives.

func TestRecyclesToolIDsNonStringIDsAnswerFalse(t *testing.T) {
	// A tool_use id decoded as a JSON object or array is uncomparable with ==
	// and used to panic the classify goroutine; the probe's contract is that a
	// shaky reading answers false.
	for _, id := range []any{map[string]any{"k": "v"}, []any{1.0}} {
		t.Run(fmt.Sprintf("%T", id), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, map[string]any{"content": []any{
					map[string]any{"type": "tool_use", "id": id, "name": "probe_tool", "input": map[string]any{}},
				}})
			}))
			t.Cleanup(srv.Close)
			if recyclesToolIDs(srv.URL, "k", "m") {
				t.Fatal("a non-string tool id must answer false")
			}
		})
	}
}
