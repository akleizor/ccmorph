package shim

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadTurn(t *testing.T) {
	const tools = `"tools":[{"name":"Bash","input_schema":{"type":"object"}}],`
	body := func(msgs string, withTools bool) []byte {
		tl := ""
		if withTools {
			tl = tools
		}
		return []byte(`{` + tl + `"model":"m1","messages":` + msgs + `}`)
	}
	human := `[{"role":"user","content":"first"},{"role":"user","content":"second"}]`
	cases := []struct {
		name   string
		body   []byte
		sess   string
		ok     bool
		prompt string
	}{
		{"human string", body(human, true), "s", true, "second"},
		{"text beside a reminder", body(`[{"role":"user","content":"first"},{"role":"user","content":[`+
			`{"type":"text","text":"second"},{"type":"text","text":"<system-reminder>ctx</system-reminder>"}]}]`, true),
			"s", true, "second"},
		{"tool result only", body(`[{"role":"user","content":"first"},{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"t","content":"ok"}]}]`, true), "s", true, ""},
		{"tool result and a reminder", body(`[{"role":"user","content":"first"},{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"t","content":"ok"},`+
			`{"type":"text","text":"<system-reminder>x</system-reminder>"}]}]`, true), "s", true, ""},
		{"task notification", body(`[{"role":"user","content":"first"},{"role":"user","content":`+
			`"<task-notification>done</task-notification>"}]`, true), "s", true, ""},
		{"compact continuation", body(`[{"role":"user","content":"first"},{"role":"user","content":`+
			`"This session is being continued from a previous conversation that ran out of context."}]`, true), "s", true, ""},
		{"compact instruction", body(`[{"role":"user","content":"first"},{"role":"user","content":`+
			`"Your task is to create a detailed summary of the conversation so far"}]`, true), "s", true, ""},
		{"assistant last", body(`[{"role":"user","content":"first"},{"role":"assistant","content":"ok"}]`,
			true), "s", true, ""},
		{"no tools", body(human, false), "s", false, ""},
		{"not json", []byte(`{`), "s", false, ""},
		{"no messages", body(`[]`, true), "s", false, ""},
	}
	for _, c := range cases {
		got, ok := readTurn(c.body, c.sess)
		if ok != c.ok || got.prompt != c.prompt {
			t.Errorf("%s: ok=%v prompt=%q, want ok=%v prompt=%q", c.name, ok, got.prompt, c.ok, c.prompt)
		}
		if ok && got.thread == "" {
			t.Errorf("%s: empty thread", c.name)
		}
	}
	// The thread is the session plus the conversation's opening text: a
	// subagent (another first message) never inherits the main loop's decision.
	a, _ := readTurn(body(human, true), "s")
	b, _ := readTurn(body(`[{"role":"user","content":"first"},{"role":"user","content":"other"}]`, true), "s")
	c, _ := readTurn(body(human, true), "t")
	if a.thread != b.thread {
		t.Error("the same opening message and session must share a thread")
	}
	if a.thread == c.thread {
		t.Error("another session must be another thread")
	}
}

// routerTest builds a router whose command is a stub; asks collects its stdin.
func routerTest(t *testing.T, answer string, run func(ctx context.Context, argv []string, stdin []byte) ([]byte, error)) (*TurnRouter, *[]string, *[]string) {
	t.Helper()
	var asks, logs []string
	if run == nil {
		run = func(context.Context, []string, []byte) ([]byte, error) { return []byte(answer), nil }
	}
	r := &TurnRouter{
		argv: []string{"/router"}, timeout: time.Second, run: func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
			asks = append(asks, string(stdin))
			return run(ctx, argv, stdin)
		},
		logf:    func(format string, args ...any) { logs = append(logs, format) },
		threads: map[string]*turnState{},
	}
	return r, &asks, &logs
}

func turnJSON(first, last string) []byte {
	return []byte(`{"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"model":"m1",` +
		`"messages":[{"role":"user","content":"` + first + `"},{"role":"user","content":"` + last + `"}]}`)
}

func followUpJSON(t *testing.T, body []byte) []byte {
	t.Helper()
	var v map[string]any
	if err := jsonv2.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	v["messages"] = append(v["messages"].([]any), map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "ok"}}})
	out, err := jsonv2.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTurnRouterAsksOncePerTurn(t *testing.T) {
	r, asks, _ := routerTest(t, `{"model":"m2"}`, nil)
	models := func() []string { return []string{"m1", "m2"} }
	first := turnJSON("first", "second")
	tr, ok := readTurn(first, "s")
	if !ok {
		t.Fatal("a human turn with tools must be routable")
	}
	model, notice, thread := r.route(tr, "s", "m1", 100, models)
	if model != "m2" || !strings.Contains(notice, "answered by m2") || !strings.Contains(notice, "you asked for m1") {
		t.Fatalf("route = %q %q", model, notice)
	}
	if len(*asks) != 1 {
		t.Fatalf("%d asks", len(*asks))
	}
	var got struct {
		Session       string   `json:"session"`
		CurrentModel  string   `json:"current_model"`
		Prompt        string   `json:"prompt"`
		ContextTokens int      `json:"context_tokens"`
		Models        []string `json:"models"`
	}
	if err := jsonv2.Unmarshal([]byte((*asks)[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Session != "s" || got.CurrentModel != "m1" || got.Prompt != "second" ||
		got.ContextTokens != 100 || strings.Join(got.Models, ",") != "m1,m2" {
		t.Fatalf("ask = %+v", got)
	}
	// The rest of the turn reuses the decision, and carries no second notice.
	r.noticed(thread)
	tool, _ := readTurn(followUpJSON(t, first), "s")
	if m, n, _ := r.route(tool, "s", "m1", 100, models); m != "m2" || n != "" {
		t.Fatalf("follow-up = %q %q", m, n)
	}
	// A retry of the same human message is the same ask.
	if m, _, _ := r.route(tr, "s", "m1", 100, models); m != "m2" || len(*asks) != 1 {
		t.Fatalf("retry re-asked: %d", len(*asks))
	}
	// The same words again, in a longer conversation, are a new turn: only a
	// retry of the identical request reuses the decision.
	repeat := []byte(`{"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"model":"m1",` +
		`"messages":[{"role":"user","content":"first"},{"role":"user","content":"second"},` +
		`{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`)
	tr3, _ := readTurn(repeat, "s")
	if m, _, _ := r.route(tr3, "s", "m1", 100, models); m != "m2" || len(*asks) != 2 {
		t.Fatalf("a repeated message must ask again: %q asks %d", m, len(*asks))
	}
	// The next human turn asks again, with its own text.
	next := turnJSON("first", "third")
	tr2, _ := readTurn(next, "s")
	if m, _, _ := r.route(tr2, "s", "m1", 100, models); m != "m2" || len(*asks) != 3 {
		t.Fatalf("next turn = %q, asks %d", m, len(*asks))
	}
	if !strings.Contains((*asks)[2], `"prompt":"third"`) {
		t.Fatalf("third ask = %s", (*asks)[2])
	}
}

func TestTurnRouterFailsOpen(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		run    func(ctx context.Context, argv []string, stdin []byte) ([]byte, error)
		reason string
	}{
		{"timeout", "", func(ctx context.Context, _ []string, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, "timed out"},
		{"crash", "", func(context.Context, []string, []byte) ([]byte, error) {
			return nil, errors.New("exit status 3")
		}, "failed"},
		{"garbage", "not json", nil, "not a JSON object"},
		{"wrong type", `{"model":5}`, nil, "not a JSON object"},
		{"unknown model", `{"model":"m9"}`, nil, "not served"},
		{"empty answer", `{}`, nil, ""},
		{"null model", `{"model":null}`, nil, ""},
		{"same model", `{"model":"m1"}`, nil, ""},
	}
	for _, c := range cases {
		r, _, logs := routerTest(t, c.answer, c.run)
		r.timeout = 5 * time.Millisecond
		tr, _ := readTurn(turnJSON("first", "second"), "s")
		model, notice, _ := r.route(tr, "s", "m1", 10, func() []string { return []string{"m1", "m2"} })
		if model != "" || notice != "" {
			t.Errorf("%s: model=%q notice=%q, want the asked model", c.name, model, notice)
		}
		switch {
		case c.reason == "" && len(*logs) != 0:
			t.Errorf("%s: unexpected log %v", c.name, *logs)
		case c.reason != "" && (len(*logs) == 0 || !strings.Contains((*logs)[0], c.reason)):
			t.Errorf("%s: logs %v, want %q", c.name, *logs, c.reason)
		}
	}
}

func TestNewTurnRouterSpec(t *testing.T) {
	if r, err := NewTurnRouter("  "); r != nil || err != nil {
		t.Fatalf("empty: %v %v", r, err)
	}
	if _, err := NewTurnRouter("relative/router"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative: %v", err)
	}
	if _, err := NewTurnRouter(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing router must fail the launch")
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTurnRouter(plain); err == nil {
		t.Fatal("a non-executable router must fail the launch")
	}
	script := filepath.Join(t.TempDir(), "router")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := NewTurnRouter(script + " --flag value")
	if err != nil || strings.Join(r.argv, " ") != script+" --flag value" || r.timeout != 3*time.Second {
		t.Fatalf("router = %+v %v", r, err)
	}
}

// The router is a local program the user configured; the shim's credentials are
// not its business (promise 2).
func TestRunRouterGetsNoCredentials(t *testing.T) {
	dir := t.TempDir()
	envPath, inPath := filepath.Join(dir, "env"), filepath.Join(dir, "in")
	script := filepath.Join(dir, "router")
	body := "#!/bin/sh\nenv > \"" + envPath + "\"\ncat > \"" + inPath + "\"\nprintf '%s' '{\"model\":\"m2\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCMORPH_INBOUND", "sekrit")
	t.Setenv("ANTHROPIC_API_KEY", "sekrit")
	out, err := runRouter(context.Background(), []string{script}, []byte(`{"session":"s"}`))
	if err != nil || string(out) != `{"model":"m2"}` {
		t.Fatalf("run = %s %v", out, err)
	}
	env, _ := os.ReadFile(envPath)
	for _, leak := range []string{"CCMORPH_INBOUND", "ANTHROPIC_API_KEY", "sekrit"} {
		if strings.Contains(string(env), leak) {
			t.Fatalf("the router's environment carries %s:\n%s", leak, env)
		}
	}
	if in, _ := os.ReadFile(inPath); string(in) != `{"session":"s"}` {
		t.Fatalf("stdin = %s", in)
	}
}

// --- the relay ------------------------------------------------------------------

func routerServer(t *testing.T, model string) (*Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	const reply = `{"id":"msg_r","type":"message","model":"m-served","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":1}}`
	s := NewServer("http://up.invalid")
	s.remote = &http.Client{Transport: roundTrip(func(r *http.Request) *http.Response {
		b := ""
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			b = string(raw)
		}
		mu.Lock()
		sent = append(sent, b)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Status: "200 OK",
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          nopCloser{strings.NewReader(reply)},
			ContentLength: int64(len(reply))}
	})}
	spec := `{"upstream":"http://up.invalid","models":["m1","m2"],"timeout":1,"key_sha256":"` +
		sha256Hex("k") + `"}`
	routes, err := newModelRoutes(spec, s.remote)
	if err != nil {
		t.Fatal(err)
	}
	s.Routes = routes
	if model != "" {
		r, _, _ := routerTest(t, `{"model":"`+model+`"}`, nil)
		s.TurnRouter = r
	}
	return s, &sent
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func sentModel(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		Model string `json:"model"`
	}
	if err := jsonv2.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v.Model
}

func TestTurnRouterSticksThroughTheToolLoop(t *testing.T) {
	s, sent := routerServer(t, "m2")
	first := turnJSON("first", "second")
	out := askWithSession(t, s, first, "sess")
	if got := sentModel(t, (*sent)[len(*sent)-1]); got != "m2" {
		t.Fatalf("first request went as %q", got)
	}
	if !strings.Contains(out, "answered by m2") {
		t.Fatalf("no notice: %s", out)
	}
	follow := followUpJSON(t, first)
	out = askWithSession(t, s, follow, "sess")
	if got := sentModel(t, (*sent)[len(*sent)-1]); got != "m2" {
		t.Fatalf("tool follow-up went as %q", got)
	}
	if strings.Contains(out, "answered by m2") {
		t.Fatalf("the notice repeated: %s", out)
	}
	// A side call (no tools) keeps its model and never asks.
	side := []byte(`{"model":"m1","messages":[{"role":"user","content":"title?"}]}`)
	askWithSession(t, s, side, "sess")
	if got := sentModel(t, (*sent)[len(*sent)-1]); got != "m1" {
		t.Fatalf("a request without tools went as %q", got)
	}
	// The next human turn asks again.
	askWithSession(t, s, turnJSON("first", "third"), "sess")
	if got := sentModel(t, (*sent)[len(*sent)-1]); got != "m2" {
		t.Fatalf("the next turn went as %q", got)
	}
}

func askWithSession(t *testing.T, s *Server, body []byte, session string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("x-api-key", "k")
	req.Header.Set("x-claude-code-session-id", session)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestWithoutATurnRouterTheBodyIsUntouched(t *testing.T) {
	s, sent := routerServer(t, "")
	body := turnJSON("first", "second")
	askWithSession(t, s, body, "sess")
	if (*sent)[len(*sent)-1] != string(body) {
		t.Fatalf("sent %s, want the body as the client wrote it", (*sent)[len(*sent)-1])
	}
}

func TestTurnRouterUnknownModelKeepsTheRequested(t *testing.T) {
	s, sent := routerServer(t, "m9")
	out := askWithSession(t, s, turnJSON("first", "second"), "sess")
	if got := sentModel(t, (*sent)[len(*sent)-1]); got != "m1" {
		t.Fatalf("sent %q", got)
	}
	if strings.Contains(out, "Turn router") {
		t.Fatalf("no switch must mean no notice: %s", out)
	}
}

func TestListenRefusesAnUnusableTurnRouter(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if _, _, err := Listen(Hooks{}, Options{Port: 0, Target: "http://127.0.0.1:1"},
		env(map[string]string{"CCMORPH_TURN_ROUTER": "relative"})); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative: %v", err)
	}
	script := filepath.Join(t.TempDir(), "router")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Listen(Hooks{}, Options{Port: 0, Target: "http://127.0.0.1:1"},
		env(map[string]string{"CCMORPH_TURN_ROUTER": script})); err == nil ||
		!strings.Contains(err.Error(), "model route") {
		t.Fatalf("without a route: %v", err)
	}
}

// The notice must reach a real client through the stream relay, not only the
// unit under noticeStream: this drives ServeHTTP with an SSE upstream.
func TestRoutedStreamCarriesTheNotice(t *testing.T) {
	const stream = `event:content_block_start
data:{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event:content_block_delta
data:{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}

event:content_block_stop
data:{"type":"content_block_stop","index":0}

event:message_stop
data:{"type":"message_stop"}

`
	s := NewServer("http://up.invalid")
	s.remote = &http.Client{Transport: roundTrip(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Status: "200 OK",
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   nopCloser{strings.NewReader(stream)}}
	})}
	spec := `{"upstream":"http://up.invalid","models":["m1","m2"],"timeout":1,"key_sha256":"` + sha256Hex("k") + `"}`
	routes, err := newModelRoutes(spec, s.remote)
	if err != nil {
		t.Fatal(err)
	}
	s.Routes = routes
	r, _, _ := routerTest(t, `{"model":"m2"}`, nil)
	s.TurnRouter = r
	out := askWithSession(t, s, turnJSON("first", "second"), "sess")
	noticeAt := strings.Index(out, "answered by m2")
	answerAt := strings.Index(out, `"text":"answer"`)
	if noticeAt < 0 || answerAt < 0 || noticeAt > answerAt {
		t.Fatalf("notice missing or after the answer:\n%s", out)
	}
	if !strings.Contains(out, `"index":1,"delta":{"type":"text_delta","text":"answer"}`) {
		t.Fatalf("the answer did not shift behind the notice:\n%s", out)
	}
}
