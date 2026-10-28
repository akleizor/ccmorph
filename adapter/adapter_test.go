package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers -------------------------------------------------------------------

// The tether re-executes the running binary, which here is the test binary:
// never spawn it from a test.
func TestMain(m *testing.M) {
	tether = func(string, *proc, string) {}
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

var buildOnce sync.Once
var builtShim string

// shimBinary builds the stock gateway-shim once for the tests that need a real one.
func shimBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "adapter-shim-")
		if err != nil {
			return
		}
		out := filepath.Join(dir, "gateway-shim")
		cmd := exec.Command("go", "build", "-o", out, "../cmd/gateway-shim")
		if cmd.Run() == nil {
			builtShim = out
		}
	})
	if builtShim == "" {
		t.Fatal("cannot build gateway-shim")
	}
	return builtShim
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
	cfg := render([]Row{row("anthropic")}, "test-key", 1234, base, "/unused", "", "", nil)
	if !strings.Contains(cfg, `base-url: "`+base+`"`) {
		t.Fatal("passthrough base-url is not the bare upstream")
	}
}

func TestRenderUpstreamProxyOnlyWhenSet(t *testing.T) { // test_upstream_proxy_is_rendered_only_when_set
	cfg := render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "http://127.0.0.1:18480", "", nil)
	if !strings.Contains(cfg, "\nproxy-url: \"http://127.0.0.1:18480\"\n") {
		t.Fatal("proxy-url missing")
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil), "proxy-url") {
		t.Fatal("proxy-url rendered without a proxy")
	}
}

func TestRenderFiltersBlockBinding(t *testing.T) { // test_passthrough_filters_translator_block_binding
	payload := strings.SplitN(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil), "\npayload:\n", 2)[1]
	if !strings.Contains(payload, "  filter:\n") ||
		!strings.Contains(payload, "protocol: \"claude\"\n      params:\n        - \"thinking.block_binding\"\n") {
		t.Fatalf("filter missing:\n%s", payload)
	}
	if strings.Contains(render([]Row{row("chat")}, "k", 1234, "https://gw", "/unused", "", "", nil), "payload:") {
		t.Fatal("chat-only config carries a payload block")
	}
}

func TestRenderUserAgentFromTheShimItRuns(t *testing.T) { // test_passthrough_user_agent_comes_from_the_shim_the_adapter_runs
	shimPath := filepath.Join(t.TempDir(), "gateway-shim")
	writeFile(t, shimPath, "#!/bin/sh\n[ \"$1\" = translator-user-agent ] || exit 2\n"+
		"[ -n \"$CCMORPH_USER_AGENT\" ] && echo \"$CCMORPH_USER_AGENT 9.9.9\"\nexit 0\n", 0o755)
	env := append(os.Environ(), "CCMORPH_USER_AGENT=hook-ua/")
	cfg := render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", shimPath, env)
	entry := strings.SplitN(cfg, "claude-api-key:", 2)[1]
	if !strings.Contains(entry, `User-Agent: "hook-ua/ 9.9.9"`) || !strings.Contains(entry, "cloak:\n      mode: \"never\"\n") {
		t.Fatalf("entry:\n%s", entry)
	}
	var unset []string
	for _, e := range env {
		if !strings.HasPrefix(e, "CCMORPH_USER_AGENT=") {
			unset = append(unset, e)
		}
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", shimPath, unset), "headers:") {
		t.Fatal("headers rendered without a profile User-Agent")
	}
	if strings.Contains(render([]Row{row("anthropic")}, "k", 1234, "https://gw", "/unused", "", "", nil), "headers:") {
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
			swap(t, &gatewayShim, func() string { return shimBinary(t) })
			edge, _ := freePort()
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

func TestOnlyTheDirectRouteRetriesInTheShim(t *testing.T) { // test_only_the_direct_route_retries_in_the_shim
	cmd := spawnedDirect(t, map[string]string{})
	if !contains(cmd.Args, "--retry-transport") {
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
		if contains(cmd.Args, flag) {
			t.Errorf("new shim flag %s", flag)
		}
	}
	if cmd.Path != "/stable/hooks.py" {
		t.Errorf("ran %s, not the profile's hooks build", cmd.Path)
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
	if !reflect.DeepEqual(argv[n-3:], []string{"ccmorph-adapter", ccmorphScript(), "p; nope"}) ||
		!strings.Contains(argv[2], `source "$1"`) || !strings.Contains(argv[2], `ccmorph _env "$2"`) {
		t.Fatalf("argv %q", argv)
	}
}

func TestLibDirDefaults(t *testing.T) { // the CCMORPH path half of test_profile_env_sources_the_packaged_ccmorph_script
	t.Setenv("CCMORPH_LIB_DIR", "/somewhere/lib")
	if got := defaultLibDir(); got != "/somewhere/lib" {
		t.Errorf("override ignored: %s", got)
	}
	t.Setenv("CCMORPH_LIB_DIR", "")
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if want := filepath.Join(filepath.Dir(filepath.Dir(exe)), "lib"); defaultLibDir() != want {
		t.Errorf("default %s, want %s", defaultLibDir(), want)
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
