package shim

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testMasker(t *testing.T, commands string) *masker {
	t.Helper()
	m, err := newMasker(bytes.Repeat([]byte{7}, 32), commands)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func maskOne(t *testing.T, m *masker, text string) string {
	t.Helper()
	out, _, ok := m.maskRequest([]byte(`{"messages":[{"role":"user","content":` + string(Quote(text)) + `}]}`))
	if !ok {
		t.Fatal("unreadable")
	}
	d := parseDoc(out)
	msgs, _ := d.last("messages")
	turns, _ := Array(out, msgs.Start)
	ms, _ := Object(out, turns[0].Start)
	c, _ := lastMember(ms, "content")
	got, _ := str(out, c)
	return got
}

func TestMaskRules(t *testing.T) {
	m := testMasker(t, "")
	for _, secret := range []string{
		"sk-ant-api03-" + strings.Repeat("a1B2", 10),
		"ghp_" + strings.Repeat("x9", 18),
		"AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----",
	} {
		if got := maskOne(t, m, "see "+secret+" here"); strings.Contains(got, secret) || !placeholderRE.MatchString(got) {
			t.Errorf("not masked: %q", got)
		}
	}
	for in, gone := range map[string]string{
		"postgres://app:S3cr3tPw@db:5432/x":             "S3cr3tPw",
		`DB_PASSWORD="hunter2hunter"`:                   "hunter2hunter",
		"export OPENAI_API_KEY=abc123def456ghi":         "abc123def456ghi",
		"Authorization: Bearer abcdefghij0123456789xyz": "abcdefghij0123456789xyz",
		// A labeled password of letters (or letters and dashes) is a value,
		// not a code identifier.
		"DB_PASSWORD=changeme":                   "changeme",
		"PASSWORD=correcthorsebatterystaple":     "correcthorsebatterystaple",
		"password: correct-horse-battery-staple": "correct-horse-battery-staple",
	} {
		if got := maskOne(t, m, in); strings.Contains(got, gone) {
			t.Errorf("%q kept %q: %q", in, gone, got)
		}
	}
	for _, plain := range []string{
		"password = self.password", "token: ${GITHUB_TOKEN}", "max_tokens: 4096",
		"api_key = os.environ.get_key", "password: <your password>", "pwd=abc",
		"secret_key_base = Rails.application.secret", "the word password alone",
		"password: string", "password = Password",
	} {
		if got := maskOne(t, m, plain); got != plain {
			t.Errorf("masked a non-secret: %q -> %q", plain, got)
		}
	}
}

const opRead = "toolu_01"

// history is a request in which the agent fetched a password and then typed it.
func history(typed string) string {
	return `{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"pw is {{PASSWORD_x}} hunter2pass","signature":"sig"},` +
		`{"type":"tool_use","id":"` + opRead + `","name":"Bash","input":{"command":"op read op://Private/site/password"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + opRead + `","content":"hunter2pass\n"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"fill","input":{"text":` + string(Quote(typed)) + `}}]}]}`
}

func TestMaskedRoundTrip(t *testing.T) {
	var seen []string
	reply := ""
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(reply))
	}))
	defer up.Close()
	s := NewServer(up.URL)
	s.PreserveIDs = true
	s.mask = testMasker(t, "")
	url := front(t, s)

	// The fetched password is masked, everywhere, outside thinking.
	resp := post(t, url, "/v1/messages", history("hunter2pass"))
	resp.Body.Close()
	sent := seen[0]
	if strings.Count(sent, "hunter2pass") != 1 || !strings.Contains(sent, `pw is {{PASSWORD_x}} hunter2pass`) {
		t.Fatalf("only thinking may keep the value: %s", sent)
	}
	ph := placeholderRE.FindString(sent)
	if ph == "" || !strings.Contains(sent, `"content":"`+ph+`\n"`) || !strings.Contains(sent, `"text":"`+ph+`"`) {
		t.Fatalf("placeholder missing: %s", sent)
	}

	// The reply puts it back, split across deltas and inside tool input; thinking keeps it.
	half := len(ph) / 2
	ev := func(index int, delta string) string {
		return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":" + string(rune('0'+index)) +
			",\"delta\":" + delta + "}\n\n"
	}
	reply = ev(0, `{"type":"thinking_delta","thinking":"`+ph+`"}`) +
		ev(1, `{"type":"text_delta","text":"use `+ph[:half]+`"}`) +
		ev(1, `{"type":"text_delta","text":"`+ph[half:]+` now {"}`) +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		ev(2, `{"type":"input_json_delta","partial_json":"{\"text\": \"`+ph+`\"}"}`) +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp = post(t, url, "/v1/messages", history("hunter2pass"))
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(got)
	for _, want := range []string{`"thinking":"` + ph + `"`, `"text":"use hunter2pass now {"`,
		`"partial_json":"{\"text\": \"hunter2pass\"}"`} {
		if !strings.Contains(out, want) {
			t.Errorf("reply lacks %s:\n%s", want, out)
		}
	}
	// the split delta is joined to the next one; the held "{" is let go at the stop
	if strings.Count(out, "event: content_block_delta") != 3 || !strings.Contains(out, "now {\"}}\n\nevent: content_block_stop") {
		t.Errorf("events out of order:\n%s", out)
	}
	// Same value, same placeholder: the history reads the same every turn.
	if seen[1] != sent {
		t.Fatal("masking is not deterministic")
	}

	// A password command in a pipeline prints something else: left alone.
	piped := strings.Replace(history("x"), "op read op://Private/site/password", "op read op://x | wc -c", 1)
	m2 := testMasker(t, "")
	b, _, _ := m2.maskRequest([]byte(piped))
	if !strings.Contains(string(b), `"content":"hunter2pass\n"`) {
		t.Fatalf("piped output masked: %s", b)
	}
	failed := strings.Replace(history("x"), `"tool_use_id":"`+opRead+`",`, `"tool_use_id":"`+opRead+`","is_error":true,`, 1)
	if b, _, _ := testMasker(t, "").maskRequest([]byte(failed)); !strings.Contains(string(b), `"content":"hunter2pass\n"`) {
		t.Fatalf("a failed fetch's error was masked: %s", b)
	}
	// A profile names its own password command.
	own := strings.Replace(history("x"), "op read op://Private/site/password", "secrets get site", 1)
	b, masked, _ := testMasker(t, `^secrets get\b`).maskRequest([]byte(own))
	if strings.Contains(string(b), `"content":"hunter2pass\n"`) || masked["PASSWORD"] != 1 || maskedJSON(masked) != `{"PASSWORD":1}` {
		t.Fatalf("own command: %s %v", b, masked)
	}
}

func TestMaskedJSONReply(t *testing.T) {
	m := testMasker(t, "")
	m.learn("hunter2pass", "PASSWORD")
	ph := m.known["hunter2pass"]
	in := `{"content":[{"type":"thinking","thinking":"` + ph + `"},{"type":"text","text":"a ` + ph + ` b"},` +
		`{"type":"tool_use","id":"t","input":{"cmd":["login","` + ph + `"]}},` +
		`{"type":"tool_use","id":"u","input":{"steps":[{"type":"thinking","text":"use ` + ph + ` here"}]}},` +
		`{"type":"text","text":"{{PASSWORD_aaaaaaaaaaaa}}"}]}`
	got := string(m.restoreBody([]byte(in), false))
	want := `{"content":[{"type":"thinking","thinking":"` + ph + `"},{"type":"text","text":"a hunter2pass b"},` +
		`{"type":"tool_use","id":"t","input":{"cmd":["login","hunter2pass"]}},` +
		`{"type":"tool_use","id":"u","input":{"steps":[{"type":"thinking","text":"use hunter2pass here"}]}},` +
		`{"type":"text","text":"{{PASSWORD_aaaaaaaaaaaa}}"}]}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestMaskSetting(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "CCMORPH_MASK" {
				return v
			}
			return ""
		}
	}
	if _, _, err := Listen(Hooks{}, Options{Target: "http://x"}, env("yes")); err == nil {
		t.Fatal("a bad CCMORPH_MASK must stop the shim")
	}
	srv, ln, err := Listen(Hooks{}, Options{Target: "http://x"}, env("1"))
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if srv.Handler.(*Server).mask == nil {
		t.Fatal("masking off")
	}
	path := filepath.Join(stateDir(), "mask.key")
	k1, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	k2, _ := maskKey()
	if len(k1) != 32 || !bytes.Equal(k1, k2) || st.Mode().Perm() != 0o600 {
		t.Fatalf("key not kept: %d %v", len(k1), st.Mode())
	}
}

// The floor is characters, as the guide says ("shorter than six characters"):
// 密码 is two characters, six UTF-8 bytes, and is not a value.
func TestMaskFloorCountsCharacters(t *testing.T) {
	for _, tc := range []struct {
		value  string
		masked bool
	}{
		{"密码", false},
		{"密码密码密码", true},
		{"12345", false},
		{"abcdef", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			m := testMasker(t, "")
			in := strings.ReplaceAll(history("x"), "hunter2pass", tc.value)
			if _, _, ok := m.maskRequest([]byte(in)); !ok {
				t.Fatal("unreadable")
			}
			prose := "我把" + tc.value + "记在纸上"
			if got := maskOne(t, m, prose); (got != prose) != tc.masked {
				t.Fatalf("masked=%v, want %v: %q", got != prose, tc.masked, got)
			}
		})
	}
}

// unmask is a whole SSE reply with its placeholders put back.
func unmask(t *testing.T, m *masker, sse string) string {
	t.Helper()
	u := &unmaskStream{src: &thinkingStream{r: bufio.NewReader(strings.NewReader(sse))}, m: m}
	var out []byte
	for {
		line, err := u.readline()
		if err != nil {
			break
		}
		out = append(out, line...)
	}
	return string(out)
}

// deltaEvent is one content_block_delta event, closing blank line included.
func deltaEvent(index int, delta string) string {
	return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":" +
		string(rune('0'+index)) + ",\"delta\":" + delta + "}\n\n"
}

func TestStreamRestoreEdges(t *testing.T) {
	m := testMasker(t, "")
	m.learn("hunter2pass", "PASSWORD")
	ph := m.known["hunter2pass"]

	t.Run("cut after a data line", func(t *testing.T) {
		// No closing blank line: the stream ends right after the data line.
		sse := "event: content_block_delta\ndata: " +
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the password is ` + ph + `"}}` + "\n"
		out := unmask(t, m, sse)
		if !strings.Contains(out, `"text":"the password is hunter2pass"`) || strings.Contains(out, ph) {
			t.Fatalf("unrestored: %q", out)
		}
	})

	// joinedThrough puts a placeholder's two halves in two deltas with between
	// them, which must not end the join.
	joinedThrough := func(t *testing.T, between string) string {
		t.Helper()
		half := len(ph) / 2
		sse := deltaEvent(0, `{"type":"text_delta","text":"pw: `+ph[:half]+`"}`) + between +
			deltaEvent(0, `{"type":"text_delta","text":"`+ph[half:]+` done"}`) +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		out := unmask(t, m, sse)
		if !strings.Contains(out, `"text":"pw: hunter2pass done"`) || strings.Contains(out, ph) {
			t.Fatalf("join broken by %q: %q", between, out)
		}
		return out
	}

	t.Run("ping between halves", func(t *testing.T) {
		out := joinedThrough(t, "event: ping\ndata: {\"type\": \"ping\"}\n\n")
		if !strings.Contains(out, "event: ping") {
			t.Fatalf("ping dropped: %q", out)
		}
	})
	t.Run("comment between halves", func(t *testing.T) {
		joinedThrough(t, ": keep-alive\n\n")
	})
	t.Run("unknown event between halves", func(t *testing.T) {
		joinedThrough(t, "event: x\ndata: {\"type\":\"x\"}\n\n")
	})
}

// A key file holds the whole key or nothing: an empty file is replaced, and
// two launches starting together both get the same key.
func TestMaskKeyIsWholeOrAbsent(t *testing.T) {
	keyDir := func(t *testing.T) string {
		root := t.TempDir()
		t.Setenv("XDG_STATE_HOME", root)
		dir := filepath.Join(root, "ccmorph")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("empty file is replaced", func(t *testing.T) {
		dir := keyDir(t)
		path := filepath.Join(dir, "mask.key")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		k1, err := maskKey()
		if err != nil || len(k1) != 32 {
			t.Fatalf("empty key file refused: %d %v", len(k1), err)
		}
		if k2, _ := maskKey(); !bytes.Equal(k1, k2) {
			t.Fatal("a second call rotated the key")
		}
	})

	t.Run("wrong size is still refused", func(t *testing.T) {
		dir := keyDir(t)
		if err := os.WriteFile(filepath.Join(dir, "mask.key"), []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		if k, err := maskKey(); err == nil {
			t.Fatalf("kept a %d-byte key", len(k))
		}
	})

	t.Run("concurrent first launches agree", func(t *testing.T) {
		dir := keyDir(t)
		const n = 8
		var wg sync.WaitGroup
		keys := make([][]byte, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				keys[i], errs[i] = maskKey()
			}(i)
		}
		wg.Wait()
		for i := 1; i < n; i++ {
			if errs[i] != nil || errs[0] != nil || len(keys[i]) != 32 || !bytes.Equal(keys[i], keys[0]) {
				t.Fatalf("launch %d: %d %v", i, len(keys[i]), errs[i])
			}
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "mask.key.*")); len(left) != 0 {
			t.Fatalf("temp file left: %v", left)
		}
	})

	t.Run("without hard links", func(t *testing.T) {
		withoutHardlinks(t)
		dir := keyDir(t)
		k, err := maskKey()
		if err != nil || len(k) != 32 {
			t.Fatalf("%d %v", len(k), err)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "mask.key.*")); len(left) != 0 {
			t.Fatalf("temp file left: %v", left)
		}
	})
}

// The usage receipt's `masked` member (docs/ARCHITECTURE.md, [test]): a
// request that masked something carries the counts, one that masked nothing
// carries no member at all.
func TestMaskedCountReachesTheReceipt(t *testing.T) {
	dir := stateHome(t)
	s := NewServer("http://up.invalid")
	// No listener: the upstream answers in-process, and the client request
	// goes through the handler directly.
	s.remote = &http.Client{Transport: roundTrip(func(*http.Request) *http.Response {
		const body = `{"id":"msg_mask","model":"m","usage":{"input_tokens":1}}`
		return &http.Response{StatusCode: 200, Status: "200 OK",
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body))}
	})}
	sum := sha256.Sum256([]byte("k"))
	spec := `{"profile":"p","identity":"i","upstream":"http://up.invalid","timeout":1,"key_sha256":"` + hex.EncodeToString(sum[:]) + `"}`
	routes, err := newModelRoutes(spec, s.remote)
	if err != nil {
		t.Fatal(err)
	}
	routes.usage.prices = nil // no price book: this test writes rows only
	s.Routes = routes
	s.mask = testMasker(t, "")

	ask := func(content string) {
		req := httptest.NewRequest("POST", "/v1/messages",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+content+`"}]}`))
		req.Header.Set("x-api-key", "k")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", content, rec.Code, rec.Body.String())
		}
	}
	ask("DB_PASSWORD=changeme")
	ask("hello")
	got := rows(t, dir)
	if len(got) != 2 {
		t.Fatalf("%d rows: %v", len(got), got)
	}
	masked := 0
	for _, row := range got {
		if strings.Contains(row, `"masked":`) {
			masked++
			if !strings.Contains(row, `"masked":{"PASSWORD":1}`) {
				t.Fatalf("wrong counts: %s", row)
			}
		}
	}
	if masked != 1 {
		t.Fatalf("%d rows carry masked: %v", masked, got)
	}
}

// roundTrip is a transport that answers without a socket.
type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }
