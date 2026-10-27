package shim

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCanonMatchesPythonDumps(t *testing.T) {
	src := `{ "model" : "m", "text":"\u7cb5\u8a9e \u00e9 \u2028 \u007f \ud83d\ude00 \/", "n":[1, 2.5, null, true], "e":{} }`
	for _, tc := range []struct {
		ascii, spaced bool
		want          string
	}{
		{false, false, "{\"model\":\"m\",\"text\":\"粵語 é   \x7f 😀 /\",\"n\":[1,2.5,null,true],\"e\":{}}"},
		{true, false, `{"model":"m","text":"\u7cb5\u8a9e \u00e9 \u2028 \u007f \ud83d\ude00 /","n":[1,2.5,null,true],"e":{}}`},
		{true, true, `{"model": "m", "text": "\u7cb5\u8a9e \u00e9 \u2028 \u007f \ud83d\ude00 /", "n": [1, 2.5, null, true], "e": {}}`},
	} {
		got, ok := (&canon{ascii: tc.ascii, spaced: tc.spaced}).encode([]byte(src), nil)
		if !ok || string(got) != tc.want {
			t.Errorf("ascii=%v spaced=%v\n got %s\nwant %s", tc.ascii, tc.spaced, got, tc.want)
		}
	}
}

func TestLoneSurrogateFallsBackToAscii(t *testing.T) {
	got, ok := encodeBody([]byte(`{"a":"\ud800 é"}`), nil, false)
	if !ok || string(got) != `{"a":"\ud800 \u00e9"}` {
		t.Fatalf("got %s %v", got, ok)
	}
}

func TestDeepBodyIsRefused(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 5000) + "1" + strings.Repeat("}", 5000)
	if _, ok := (&canon{}).encode([]byte(deep), []Edit{{}}); ok {
		t.Fatal("5000-deep body must not be re-encoded")
	}
	s := NewServer("http://x")
	out, changed := s.rewrite([]byte(deep), "", false)
	if changed || string(out) != deep {
		t.Fatal("deep payload must pass through byte-identical")
	}
}

func TestWithModelKeepsKeyOrder(t *testing.T) {
	d := parseDoc([]byte(`{"z":1,"model":"a","a":[{"model":"x"}]}`))
	if got := string(withModel(d, "b[1m]")); got != `{"z":1,"model":"b[1m]","a":[{"model":"x"}]}` {
		t.Fatal(got)
	}
}

func rewritten(t *testing.T, s *Server, in, answerAs string, spaced bool) string {
	t.Helper()
	out, _ := s.rewrite([]byte(in), answerAs, spaced)
	return string(out)
}

var shimID = regexp.MustCompile(`toolu_shim_[0-9a-f]{8}_\d+`)

func TestRewriteIDsUniqueAndNested(t *testing.T) {
	s := NewServer("http://x")
	in := `{"content":[{"type":"tool_use","id":"Bash_0","input":{"x":{"type":"tool_use","id":"in_0"}}},{"type":"text","id":"keep"}]}`
	out := rewritten(t, s, in, "", true)
	ids := shimID.FindAllString(out, -1)
	if len(ids) != 2 || ids[0] == ids[1] || strings.Contains(out, "Bash_0") || strings.Contains(out, "in_0") || !strings.Contains(out, `"id": "keep"`) {
		t.Fatalf("%s", out)
	}
	s.PreserveIDs = true
	if got := rewritten(t, s, in, "", true); got != in {
		t.Fatalf("preserve: %s", got)
	}
}

func TestEmptyThinkingRemovedFromJSON(t *testing.T) {
	s := NewServer("http://x")
	for name, tc := range map[string]struct{ in, want string }{
		"empty dropped":        {`{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"a"}]}`, `{"content":[{"type":"text","text":"a"}]}`},
		"missing text":         {`{"content":[{"type":"thinking"},{"type":"text","text":"a"}]}`, `{"content":[{"type":"text","text":"a"}]}`},
		"only block":           {`{"content":[{"type":"thinking","thinking":"","signature":""}]}`, `{"content":[]}`},
		"signed empty kept":    {`{"content":[{"type":"thinking","thinking":"","signature":"s"}]}`, ""},
		"text kept":            {`{"content":[{"type":"thinking","thinking":"hm"}]}`, ""},
		"tool input untouched": {`{"content":[{"type":"tool_use","id":"","input":{"content":[{"type":"thinking"}]}}]}`, ""},
	} {
		out, changed := s.rewrite([]byte(tc.in), "", false)
		if tc.want == "" {
			if changed && !strings.Contains(tc.in, `"id":""`) {
				t.Errorf("%s: changed %s", name, out)
			}
			continue
		}
		if string(out) != tc.want {
			t.Errorf("%s: got %s want %s", name, out, tc.want)
		}
	}
}

func TestAnswerAs(t *testing.T) {
	s := NewServer("http://x")
	if got := rewritten(t, s, `{"type":"message","model":"Int_X"}`, "claude-opus-5", false); got != `{"type":"message","model":"claude-opus-5"}` {
		t.Fatal(got)
	}
	if got := rewritten(t, s, `{"type":"message_start","message":{"type":"message","model":"Int_X"}}`, "claude-opus-5", false); got != `{"type":"message_start","message":{"type":"message","model":"claude-opus-5"}}` {
		t.Fatal(got)
	}
	in := `{"type":"content_block_delta","model":"Int_X"}`
	if got := rewritten(t, s, in, "claude-opus-5", false); got != in {
		t.Fatal(got)
	}
}

func TestUnwrapGatewayError(t *testing.T) {
	inner := `{"message":"tool_addition refused"}`
	hexs := ""
	for _, b := range []byte(inner) {
		hexs += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15])
	}
	body := `{"code":400,"detailMsg":"bedrock 400 SdkBytes(bytes=0x` + hexs + `)"}`
	want := `{"type": "error", "error": {"type": "invalid_request_error", "message": "tool_addition refused"}}`
	if got := string(unwrapGatewayError([]byte(body))); got != want {
		t.Fatal(got)
	}
	for _, bad := range []string{`{"detailMsg":"nothing"}`, `[1]`, `{"detailMsg":"SdkBytes(bytes=0xabc)"}`, `not json`} {
		if unwrapGatewayError([]byte(bad)) != nil {
			t.Errorf("%s", bad)
		}
	}
}

func TestClaudeVersionAndResolve(t *testing.T) {
	set := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, i := range ids {
			m[i] = true
		}
		return m
	}
	for _, tc := range []struct {
		asked  string
		served map[string]bool
		want   string
	}{
		{"claude-opus-5", nil, "claude-opus-5"},
		{"claude-opus-5", set("claude-opus-5"), "claude-opus-5"},
		{"claude-opus-5", set("claude-opus-5-20260901"), "claude-opus-5-20260901"},
		{"claude-opus-5", set("claude-opus-5-1", "claude-opus-5-20260901"), "claude-opus-5-20260901"},
		{"claude-opus-5", set("claude-opus-5-1", "claude-opus-6"), "claude-opus-6"},
		{"claude-opus-5-1", set("claude-opus-5"), "claude-opus-5-1"},
		{"claude-opus-5-0", set("claude-opus-5-20260901"), "claude-opus-5-20260901"},
		{"claude-opus-5", set("claude-sonnet-9"), "claude-opus-5"},
		{"gpt-x", set("claude-opus-5"), "gpt-x"},
	} {
		if got := resolveModel(tc.asked, tc.served); got != tc.want {
			t.Errorf("%s over %v: got %s want %s", tc.asked, tc.served, got, tc.want)
		}
	}
}

func TestUsageBuckets(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"input_tokens":1,"service_tier":"standard","unknown":9,"output_tokens":true}`, `{"input_tokens":1}`},
		{`{"cost":0.25,"output_tokens":3}`, `{"output_tokens":3,"cost":0.25}`},
		{`{"cost":-1}`, `{}`},
		{`{"cost":1e999}`, `{}`},
		{`{"cost":"2"}`, `{}`},
		{`{"cost":1e300}`, `{}`},
		{`{"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":"x"}}`, `{"cache_creation":{"ephemeral_5m_input_tokens":2}}`},
		{`{"cache_creation":{"ephemeral_1h_input_tokens":"x"}}`, `{}`},
	} {
		b := []byte(tc.in)
		if got := string(bucketsOf(b, Span{0, len(b)}, true).json()); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.in, got, tc.want)
		}
	}
}

func TestTaskTags(t *testing.T) {
	h := http.Header{}
	h.Set("x-ccmorph-task", "issue-42")
	h.Set("x-ccmorph-attempt", "3")
	if g := tagsOf(h); g.task == nil || *g.task != "issue-42" || g.attempt != 3 {
		t.Fatal(g)
	}
	for _, bad := range [][2]string{{"a/b", "0"}, {"x y", "²"}, {strings.Repeat("a", 65), "1000"}, {"", "-1"}} {
		h.Set("x-ccmorph-task", bad[0])
		h.Set("x-ccmorph-attempt", bad[1])
		if g := tagsOf(h); g.task != nil || g.attempt != 1 {
			t.Errorf("%q: %v", bad, g)
		}
	}
}

func TestBreaker(t *testing.T) {
	now := 0.0
	b := &Breaker{clock: func() float64 { return now }}
	for i := 0; i < breakerFailures-1; i++ {
		b.record(false)
	}
	if b.wait() != nil {
		t.Fatal("open too early")
	}
	b.record(false)
	if w := b.wait(); w == nil || *w != breakerOpen {
		t.Fatal("must be open", w)
	}
	now = breakerOpen + 1
	if b.wait() != nil {
		t.Fatal("first request after the hold is the probe")
	}
	if b.wait() == nil {
		t.Fatal("only one probe at a time")
	}
	b.record(false) // probe fails: reopens
	if b.wait() == nil {
		t.Fatal("failed probe must reopen")
	}
	now += breakerOpen + 1
	b.wait()
	b.record(true)
	if b.wait() != nil {
		t.Fatal("success closes")
	}
	// failures outside the window do not add up
	now += breakerWindow + 1
	b.record(false)
	b.record(false)
	now += breakerWindow + 1
	b.record(false)
	if b.wait() != nil {
		t.Fatal("stale failures must expire")
	}
}

func TestDeviceIDSwap(t *testing.T) {
	s := NewServer("http://x")
	s.RandomDeviceID = true
	user := `{"device_id":"` + strings.Repeat("d", 64) + `","account_uuid":"","session_id":"sess-1"}`
	quoted, _ := jsontext.AppendQuote(nil, user)
	body := []byte(`{"model":"m","metadata":{"user_id":` + string(quoted) + `},"x":"粵"}`)
	var session string
	out := s.cleanRequest(body, true, &session)
	if session != "sess-1" || bytes.Equal(out, body) {
		t.Fatalf("%q %s", session, out)
	}
	var v struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		X string `json:"x"`
	}
	if err := jsonv2.Unmarshal(out, &v); err != nil || v.X != "粵" || !strings.Contains(string(out), `"x":"粵"`) {
		t.Fatalf("%v %s", err, out)
	}
	if !strings.HasPrefix(string(out), `{"model":"m","metadata":{"user_id":`) ||
		strings.Contains(v.Metadata.UserID, strings.Repeat("d", 64)) ||
		!strings.Contains(v.Metadata.UserID, `"session_id":"sess-1"`) ||
		!strings.Contains(v.Metadata.UserID, `"device_id":"`+s.deviceID("sess-1")+`"`) {
		t.Fatalf("%s", v.Metadata.UserID)
	}
	if s.deviceID("sess-1") == s.deviceID("sess-2") {
		t.Fatal("one device id per session")
	}
	// no user_id, not an object, wrong types: bytes untouched
	for _, in := range []string{`{"model":"m"}`, `[1]`, `{"metadata":{"user_id":"x"}}`, `{"metadata":{"user_id":"{\"a\":1}"}}`, `nope`} {
		if got := s.cleanRequest([]byte(in), true, new(string)); string(got) != in {
			t.Errorf("%s -> %s", in, got)
		}
	}
}

func TestDeviceIDSwapLoneSurrogate(t *testing.T) {
	s := NewServer("http://x")
	body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"s\\ud800\"}"},"x":"é"}`)
	out := string(s.cleanRequest(body, true, new(string)))
	// The outer body stays raw UTF-8 and valid; the inner surrogate stays an escape.
	if !jsontext.Value(out).IsValid() || !strings.Contains(out, `\\ud800`) || !strings.Contains(out, `"x":"é"`) {
		t.Fatal(out)
	}
}

func TestHookRepairAndSession(t *testing.T) {
	var seen string
	s := NewServer("http://x")
	s.Hooks.RepairRequest = func(b []byte) ([]byte, bool) {
		if strings.Contains(string(b), `"dirty"`) {
			return []byte(strings.Replace(string(b), `"dirty"`, `"clean"`, 1)), true
		}
		return nil, false
	}
	s.Hooks.OutboundHeaders = func(h http.Header, session string) { seen = session }
	if got := s.cleanRequest([]byte(`{"hooks": "dirty"}`), false, new(string)); string(got) != `{"hooks":"clean"}` {
		t.Fatal(string(got))
	}
	raw := `{"hooks":"clean"}`
	if got := s.cleanRequest([]byte(raw), false, new(string)); string(got) != raw {
		t.Fatal(string(got))
	}
	s.Hooks.RepairRequest = func([]byte) ([]byte, bool) { panic("boom") }
	raw = `{"hooks":"dirty"}`
	if got := s.cleanRequest([]byte(raw), false, new(string)); string(got) != raw {
		t.Fatal("a panicking hook must leave the bytes unchanged")
	}
	var session string
	s.cleanRequest([]byte(`{"metadata":{"user_id":"{\"session_id\":\"sess-1\"}"}}`), false, &session)
	if session != "sess-1" {
		t.Fatal("hooks need the session id with no flag set", seen)
	}
}

// --- SSE ------------------------------------------------------------------------------

func streamThrough(t *testing.T, s *Server, upstream string, sidecar *UsageSidecar, answerAs string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(upstream))
	}))
	defer srv.Close()
	s.Target = srv.URL
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return b.String()
}

func TestSSERewriteFramingAndTerminal(t *testing.T) {
	s := NewServer("")
	up := "event:content_block_start\r\ndata:{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"Bash_0\",\"name\":\"Bash\",\"input\":{}}}\r\n\r\n" +
		"event:message_stop\ndata:{\"type\":\"message_stop\"}\n\n" + "data:never\n\n"
	got := streamThrough(t, s, up, nil, "")
	if strings.Contains(got, "Bash_0") || strings.Contains(got, "never") || !strings.HasSuffix(got, "data:{\"type\":\"message_stop\"}\n\n") {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(got, "\"name\":\"Bash\",\"input\":{}}}\r\n\r\n") {
		t.Fatalf("CRLF framing lost: %q", got)
	}
	if !shimID.MatchString(got) {
		t.Fatal(got)
	}
}

func TestSSEErrorEventIsTerminal(t *testing.T) {
	s := NewServer("")
	got := streamThrough(t, s, "event:error\ndata:{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\ndata:x\n\n", nil, "")
	if strings.Contains(got, "data:x") || !strings.HasSuffix(got, "}}\n\n") {
		t.Fatalf("%q", got)
	}
}

func TestEmptyThinkingStream(t *testing.T) {
	s := NewServer("")
	start := func(i int, block string) string {
		return `event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":` + string(rune('0'+i)) + `,"content_block":` + block + `}` + "\n\n"
	}
	stop := func(i int) string {
		return `event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":` + string(rune('0'+i)) + `}` + "\n\n"
	}
	delta := `event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}` + "\n\n"
	// dropped: empty thinking start + stop, later indexes shift down
	up := start(0, `{"type":"thinking","thinking":""}`) + stop(0) + start(1, `{"type":"text","text":""}`) + stop(1)
	want := start(0, `{"type":"text","text":""}`) + stop(0)
	want = strings.ReplaceAll(want, `, `, `,`)
	got := streamThrough(t, s, up, nil, "")
	if strings.ReplaceAll(strings.ReplaceAll(got, ` {"type"`, `{"type"`), `data:`, `data: `) != strings.ReplaceAll(strings.ReplaceAll(want, ` {"type"`, `{"type"`), `data:`, `data: `) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// a delta releases the held block untouched
	up = start(0, `{"type":"thinking","thinking":""}`) + delta + stop(0)
	if got := streamThrough(t, s, up, nil, ""); got != up {
		t.Fatalf("healthy block changed: %q", got)
	}
	// signed empty block is kept
	up = start(0, `{"type":"thinking","thinking":"","signature":"s"}`) + stop(0)
	if got := streamThrough(t, s, up, nil, ""); got != up {
		t.Fatalf("signed block changed: %q", got)
	}
}

func TestStallEndsStreamWithErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data:{\"type\":\"ping\"}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	s := NewServer(srv.URL)
	s.Stall = 200 * time.Millisecond
	s.setClients()
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	want := "event: error\ndata: {\"type\": \"error\", \"error\": {\"type\": \"api_error\", \"message\": \"gateway sent nothing for 0s; ended by ccmorph so the session can retry\"}}\n\n"
	if !strings.HasSuffix(b.String(), want) {
		t.Fatalf("%q", b.String())
	}
}

func TestSilentUpstreamAnswers504(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer srv.Close()
	s := NewServer(srv.URL)
	s.Stall = 200 * time.Millisecond
	s.setClients()
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != 504 {
		t.Fatal(err, resp)
	}
}

// --- relay: retries, framing, redirects ----------------------------------------------

func front(t *testing.T, s *Server) string {
	t.Helper()
	s.Sleep = func(time.Duration) {}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

func post(t *testing.T, url, path, body string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url+path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestUnexpectedEOFIsReplayedOnceOnMessagesOnly(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		n := calls[r.URL.Path]
		mu.Unlock()
		if n == 1 || r.URL.Path == "/v1/other" {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"unexpected EOF"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	url := front(t, NewServer(up.URL))
	if r := post(t, url, "/v1/messages", "{}"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if r := post(t, url, "/v1/other", "{}"); r.StatusCode != 500 || calls["/v1/other"] != 1 {
		t.Fatal("unrelated 500 must not be retried", r.StatusCode, calls)
	}
}

func TestTransportRetryAndBreaker(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
	}))
	defer up.Close()
	s := NewServer(up.URL)
	s.RetryTransport = true
	s.Breaker = NewBreaker()
	var delays []time.Duration
	url := front(t, s)
	s.Sleep = func(d time.Duration) { delays = append(delays, d) }
	for i := 0; i < breakerFailures; i++ {
		if r := post(t, url, "/v1/messages", "{}"); r.StatusCode != 503 {
			t.Fatal(r.StatusCode)
		}
	}
	if calls != breakerFailures*(transportRetries+1) || len(delays) != breakerFailures*transportRetries {
		t.Fatal(calls, delays)
	}
	for i, d := range delays {
		if i%2 == 0 && d > s.RetryBase || i%2 == 1 && d > 2*s.RetryBase {
			t.Fatal("full jitter bound", d)
		}
	}
	before := calls
	r := post(t, url, "/v1/messages", "{}")
	if r.StatusCode != 503 || calls != before {
		t.Fatal("open breaker must not call out")
	}
	var b bytes.Buffer
	b.ReadFrom(r.Body)
	if !strings.Contains(b.String(), "the provider failed 3 requests within 5 min") {
		t.Fatal(b.String())
	}
}

func TestDeadUpstreamIs502WithLength(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	r := post(t, front(t, NewServer(dead.URL)), "/v1/messages", "{}")
	if r.StatusCode != 502 || r.ContentLength != 0 {
		t.Fatal(r.StatusCode, r.ContentLength)
	}
}

func TestInvalidContentLengthRejected(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("up")) }))
	defer up.Close()
	url := strings.TrimPrefix(front(t, NewServer(up.URL)), "http://")
	for _, cl := range []string{"1_3", "-1", "", "\xb2", "abc"} {
		resp := rawRequest(t, url, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: "+cl+"\r\n\r\n{}")
		if !strings.HasPrefix(resp, "HTTP/1.1 400") {
			t.Errorf("Content-Length %q: %q", cl, resp)
		}
	}
}

func TestCrossOriginRedirectNeverCarriesTheKey(t *testing.T) {
	leaked := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked <- r.Header.Get("x-api-key") }))
	defer other.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", 302)
	}))
	defer up.Close()
	r := post(t, front(t, NewServer(up.URL)), "/v1/messages", "{}", "x-api-key", "secret")
	select {
	case k := <-leaked:
		t.Fatalf("key leaked: %q", k)
	default:
	}
	if r.StatusCode != 502 {
		t.Fatal(r.StatusCode)
	}
}

func TestInternalHeadersAndHooksNeverReachUpstream(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer up.Close()
	s := NewServer(up.URL)
	s.Hooks.UserAgent = func(c string) string { return "ua[" + c + "]" }
	s.Hooks.OutboundHeaders = func(h http.Header, session string) { h.Set("X-Affinity", session) }
	post(t, front(t, s), "/v1/messages", `{"metadata":{"user_id":"{\"session_id\":\"s1\"}"}}`,
		"x-ccmorph-task", "t", "X-CCMORPH-Other", "1", "User-Agent", "claude-cli/1", "accept-encoding", "gzip")
	if got.Get("x-ccmorph-task") != "" || got.Get("x-ccmorph-other") != "" ||
		got.Get("User-Agent") != "ua[claude-cli/1]" || got.Get("X-Affinity") != "s1" || got.Get("Accept-Encoding") != "identity" {
		t.Fatal(got)
	}
}

func TestInboundTokenSwap(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer up.Close()
	s := NewServer(up.URL)
	s.Inbound = &inbound{sha256: sha256Hex("token"), key: "real"}
	url := front(t, s)
	if r := post(t, url, "/x", "{}", "x-api-key", "wrong"); r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	post(t, url, "/x", "{}", "Authorization", "Bearer token")
	if got.Get("x-api-key") != "real" || got.Get("Authorization") != "Bearer real" {
		t.Fatal(got)
	}
}

// --- sidecar --------------------------------------------------------------------------

func stateHome(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "ccmorph", "usage")
}

func rows(t *testing.T, dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out = append(out, string(b))
	}
	return out
}

func TestSidecarLifecycleAndModes(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity").tagged(taskTags{nil, 1})
	req := "claude-x"
	rec := u.observe(parseDoc([]byte(`{"type":"message_start","message":{"id":"msg_1","model":"served","usage":{"input_tokens":11,"service_tier":"standard"}}}`)), nil, &req)
	if rec == nil {
		t.Fatal("no record")
	}
	got := rows(t, dir)
	if len(got) != 1 || !strings.Contains(got[0], `"complete":false`) || !strings.Contains(got[0], `"usage":{"input_tokens":11}`) {
		t.Fatal(got)
	}
	u.observe(parseDoc([]byte(`{"type":"message_delta","usage":{"output_tokens":42}}`)), rec, &req)
	u.observe(parseDoc([]byte(`{"type":"message_stop"}`)), rec, &req)
	got = rows(t, dir)
	if len(got) != 1 || !strings.Contains(got[0], `"complete":true`) || !strings.Contains(got[0], `"usage":{"input_tokens":11,"output_tokens":42}`) {
		t.Fatal(got)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatal(st.Mode())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Fatal("temp file left", m)
	}
	for _, secret := range []string{"identity", "standard"} {
		if strings.Contains(got[0], secret) {
			t.Fatal("row leaks", secret)
		}
	}
}

func TestRecycledResponseIDKeepsEveryRow(t *testing.T) {
	dir := stateHome(t)
	start := `{"type":"message_start","message":{"id":"recycled","usage":{"output_tokens":1}}}`
	for i := 0; i < 6; i++ {
		u := newSidecar("p", "id")
		u.observe(parseDoc([]byte(start)), nil, nil)
	}
	// every turn has its own spill names, so recycled ids never cost a row
	if n := len(rows(t, dir)); n != 6 {
		t.Fatalf("%d rows", n)
	}
}

func TestLegacyRowWithoutTurnIDIsNotOwned(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "id")
	rec := u.observe(parseDoc([]byte(`{"type":"message_start","message":{"id":"legacy"}}`)), nil, nil)
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	os.WriteFile(files[0], []byte(`{"response_id":"legacy","usage":{"cost":9}}`), 0o600)
	u.observe(parseDoc([]byte(`{"type":"message_stop"}`)), rec, nil)
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), `"cost":9`) {
		t.Fatal("legacy row overwritten")
	}
	if len(rows(t, dir)) != 2 {
		t.Fatal("the new turn must spill to its own name")
	}
}

func TestConcurrentSameIDWritesEachKeepTheirRow(t *testing.T) {
	dir := stateHome(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			newSidecar("p", "id").observe(parseDoc([]byte(`{"type":"message_start","message":{"id":"race"}}`)), nil, nil)
		}()
	}
	wg.Wait()
	if n := len(rows(t, dir)); n != 4 {
		t.Fatalf("%d rows", n)
	}
}

func TestNonStreamResponseRowAndUnwritableState(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "id")
	u.response([]byte(`{"id":"msg_j","model":"m","usage":{"input_tokens":3,"cost":0.5}}`), nil)
	if r := rows(t, dir); len(r) != 1 || !strings.Contains(r[0], `"complete":true`) || !strings.Contains(r[0], `"cost":0.5`) {
		t.Fatal(r)
	}
	// an unwritable state directory changes nothing and never panics
	t.Setenv("XDG_STATE_HOME", "/dev/null/x")
	u.response([]byte(`{"id":"msg_k"}`), nil)
}

func TestOutcomeAndReport(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	recordOutcome("issue-42", "accepted")
	b, _ := os.ReadFile(filepath.Join(state, "ccmorph", "outcomes.jsonl"))
	if !regexp.MustCompile(`^\{"task":"issue-42","outcome":"accepted","timestamp":"[^"]+\+00:00"\}\n$`).Match(b) {
		t.Fatal(string(b))
	}
	if st, _ := os.Stat(filepath.Join(state, "ccmorph", "outcomes.jsonl")); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	for _, in := range []string{`{"task":"x","outcome":"great","timestamp":"2026-01-01T00:00:00+00:00"}`, `{"task":"a/b"}`} {
		if parseTimeRow(in) {
			t.Fatal(in)
		}
	}
}

func parseTimeRow(line string) bool {
	d := parseDoc([]byte(line))
	if d == nil {
		return false
	}
	task, _ := d.str("task")
	outcome, _ := d.str("outcome")
	return taskRE.MatchString(task) && (outcome == "accepted")
}

func TestParseTime(t *testing.T) {
	for _, s := range []string{"2026-10-27T01:02:03.456789+00:00", "2026-10-27T01:02:03Z", "2026-10-27T01:02:03", "2026-10-27 01:02:03+08:00", "2026-10-27"} {
		if _, ok := parseTime(s); !ok {
			t.Errorf("%s", s)
		}
	}
	if _, ok := parseTime("yesterday"); ok {
		t.Fatal()
	}
}

// --- catalogue --------------------------------------------------------------------------

func catalogueServer(t *testing.T, body string, hits *int) *Catalogue {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	return newCatalogue(up.URL, "", NewServer("").remote)
}

func TestCatalogueEndpointsAndTTL(t *testing.T) {
	hits := 0
	c := catalogueServer(t, `{"data":[{"id":"a","supported_endpoints":["/v1/responses"]},{"id":"a","supported_endpoints":["/v1/messages"]},
		{"id":"b","supported_endpoints":["/v1/messages/count_tokens","/v1/chat/completions"]},{"id":"c"},{"id":5},"x"]}`, &hits)
	served := c.served("k", "a")
	if len(served) != 3 || !served["a"] || !served["b"] || !served["c"] {
		t.Fatal(served)
	}
	if got := c.declared("a"); len(got) != 1 || got[0] != "/v1/responses" {
		// a later duplicate is ignored once the first declaration serves a dialect
		t.Fatal(got)
	}
	if c.declared("c") != nil {
		t.Fatal("no declaration is None")
	}
	c.served("k", "a")
	c.served("k", "b")
	if hits != 1 {
		t.Fatalf("a healthy catalogue is read once within its TTL, got %d", hits)
	}
	c.served("k", "missing") // a miss within MISS_TTL does not re-read either
	if hits != 1 {
		t.Fatal(hits)
	}
	c.readAt = c.readAt.Add(-catalogueMissTTL - time.Second)
	c.served("k", "missing")
	if hits != 2 {
		t.Fatal("a missing id re-reads after the miss TTL", hits)
	}
}

func TestCatalogueFailuresSendAsAsked(t *testing.T) {
	for name, body := range map[string]string{"bad json": `{`, "not object": `[1]`, "data number": `{"data":1}`, "oversize": `{"data":"` + strings.Repeat("x", catalogueMaxBytes) + `"}`} {
		hits := 0
		c := catalogueServer(t, body, &hits)
		if got := c.served("k", "claude-opus-5"); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	hits := 0
	c := catalogueServer(t, `{}`, &hits)
	if len(c.served("wrong", "x")) != 0 {
		t.Fatal("401 is no catalogue")
	}
}

func TestCatalogueTrickleIsBoundedByTheDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 20 s deadline")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		for i := 0; i < 40; i++ {
			w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(time.Second)
		}
	}))
	defer up.Close()
	c := newCatalogue(up.URL, "", NewServer("").remote)
	start := time.Now()
	if len(c.served("k", "m")) != 0 || time.Since(start) > catalogueDeadline+3*time.Second {
		t.Fatal("trickling catalogue must fail at the deadline", time.Since(start))
	}
}

func TestCatalogueUsesTheProxyForRemoteTargets(t *testing.T) {
	// ProxyFromEnvironment reads the environment once per process, so assert the
	// wiring: the catalogue client honours it, the loopback client never does.
	s := NewServer("")
	if s.remote.Transport.(*http.Transport).Proxy == nil || s.loopback.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("remote must honour the proxy environment, loopback must not")
	}
}

func TestLoopbackTargetIgnoresProxy(t *testing.T) {
	proxied := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied = true }))
	defer proxy.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("direct")) }))
	defer up.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	s := NewServer(up.URL)
	r := post(t, front(t, s), "/x", "{}")
	if r.StatusCode != 200 || proxied {
		t.Fatal("a loopback target must bypass http_proxy")
	}
}
