package adapter

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeUpstream answers each path with its fixed status and body, counting the
// requests and keeping each body it was sent.
func fakeUpstream(t *testing.T, answers map[string][2]string) (*httptest.Server, map[string]int, map[string]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	hits, bodies := map[string]int{}, map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		hits[r.URL.Path]++
		bodies[r.URL.Path] = body
		mu.Unlock()
		a, ok := answers[r.URL.Path]
		if !ok {
			a = [2]string{"404", `{"error":{"message":"no route"}}`}
		}
		code := map[string]int{"200": 200, "400": 400, "403": 403, "404": 404, "429": 429}[a[0]]
		// An upstream that echoes the credential it was sent, as some do.
		w.WriteHeader(code)
		io.WriteString(w, strings.ReplaceAll(a[1], "$KEY", r.Header.Get("x-api-key")+r.Header.Get("Authorization")))
	}))
	t.Cleanup(srv.Close)
	return srv, hits, bodies
}

func TestCheckResultJSONIsTheContract(t *testing.T) {
	raw, _ := json.Marshal(CheckResult{OK: false, Route: "chat", Status: 403, MS: 12, Note: "denied"})
	if want := `{"ok":false,"route":"chat","status":403,"ms":12,"note":"denied"}`; string(raw) != want {
		t.Fatalf("got %s\nwant %s", raw, want)
	}
}

func TestCheckAsksEachRouteAtMostOnceAndStopsAtTheAnswer(t *testing.T) {
	srv, hits, bodies := fakeUpstream(t, map[string][2]string{
		"/v1/messages":         {"403", `{"error":{"message":"not on this route"}}`},
		"/v1/chat/completions": {"200", `{"choices":[]}`},
	})
	r := Check(srv.URL, "sk-test", "m", "", false, Routes)
	if !r.OK || r.Route != "chat" || r.Status != 200 || r.Note != "" {
		t.Fatalf("got %+v", r)
	}
	for _, p := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		if hits[p] != 1 {
			t.Fatalf("%s asked %d times, want once: %v", p, hits[p], hits)
		}
	}
	chat := bodies["/v1/chat/completions"]
	if chat["max_tokens"] != float64(1) || chat["tools"] != nil {
		t.Fatalf("chat body is not the smallest request: %v", chat)
	}
	if bodies["/v1/responses"]["max_output_tokens"] != float64(16) {
		t.Fatalf("responses body: %v", bodies["/v1/responses"])
	}
}

func TestCheckStopsAtARefusalForEveryRoute(t *testing.T) {
	srv, hits, _ := fakeUpstream(t, map[string][2]string{"/v1/messages": {"429", `{"error":{"message":"slow down"}}`}})
	r := Check(srv.URL, "sk-test", "m", "", false, Routes)
	if r.OK || r.Status != 429 || r.Note != "slow down" || len(hits) != 1 {
		t.Fatalf("got %+v after %v", r, hits)
	}
}

func TestCheckReportsTheRelayedReasonOfTheRouteThatExists(t *testing.T) {
	srv, hits, _ := fakeUpstream(t, map[string][2]string{
		"/v1/messages": {"400", `{"error":{"message":"upstream said: {\"error\":{\"message\":\"model is retired\"}}"}}`},
	})
	r := Check(srv.URL, "sk-test", "m", "", false, Routes)
	if r.OK || r.Route != "anthropic" || r.Status != 400 || r.Note != "model is retired" {
		t.Fatalf("got %+v", r)
	}
	if hits["/v1/messages"] != 1 || hits["/v1/responses"] != 1 || hits["/v1/chat/completions"] != 1 {
		t.Fatalf("hits %v", hits)
	}
}

func TestCheckNeverPrintsTheSecret(t *testing.T) {
	const key = "sk-secret-0123456789abcdefghijklmnopqrstuvwxyz"
	// The key straddles the note's 150-rune cut: no prefix of it may survive.
	srv, _, _ := fakeUpstream(t, map[string][2]string{
		"/v1/messages": {"403", `{"error":{"message":"` + strings.Repeat("x", 140) + ` bad key $KEY"}}`},
	})
	for _, oauth := range []bool{false, true} {
		r := Check(srv.URL, key, "m", "", oauth, []string{"anthropic"})
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), key[:12]) {
			t.Fatalf("oauth=%v: the note carries the secret: %s", oauth, raw)
		}
	}
}

func TestCheckOAuthSendsTheSubscriptionHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	if r := Check(srv.URL, "tok", "claude-x", "", true, []string{"anthropic"}); !r.OK {
		t.Fatalf("got %+v", r)
	}
	if got.Get("Authorization") != "Bearer tok" || got.Get("x-api-key") != "" || got.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Fatalf("headers %v", got)
	}
}

func TestCachedRouteReadsTheClassificationCache(t *testing.T) {
	dir, base := t.TempDir(), "https://gw.example"
	cap0 := 0
	rows := []Row{{Model: "g", Dialect: "chat-noreason", Cap: &cap0}, {Model: "c", Dialect: "anthropic", Cap: &cap0},
		{Model: "u", Dialect: "unusable", Cap: &cap0}}
	if err := writeCache(cachePath(dir, base), base, rows); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"g": "chat", "c": "anthropic", "u": "", "missing": ""} {
		if got := CachedRoute(base+"/", dir, model); got != want {
			t.Fatalf("%s: got %q, want %q", model, got, want)
		}
	}
}

// A check run inside a Remote Control session reaches its upstream through a
// proxy that signs with the session's extra CA: the check must trust it, as
// quota's reads do.
func TestCheckTrustsTheSessionsExtraCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }))
	defer srv.Close()
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	if r := Check(srv.URL, "k", "m", "", false, []string{"anthropic"}); r.OK || r.Status != 0 {
		t.Fatalf("an untrusted certificate answered: %+v", r)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", ca)
	if r := Check(srv.URL, "k", "m", "", false, []string{"anthropic"}); !r.OK {
		t.Fatalf("the session's CA was not trusted: %+v", r)
	}
}

// #636: Check is exported, and with no route it indexed tried[-1] and panicked.
func TestCheckWithNoRoutesAnswersWithANote(t *testing.T) {
	r := Check("http://gw.invalid", "k", "m", "", false, nil)
	if r.OK || r.Status != 0 || r.Note == "" {
		t.Fatalf("Check with no routes = %+v, want a note and no verdict", r)
	}
}
