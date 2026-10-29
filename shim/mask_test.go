package shim

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	} {
		if got := maskOne(t, m, in); strings.Contains(got, gone) {
			t.Errorf("%q kept %q: %q", in, gone, got)
		}
	}
	for _, plain := range []string{
		"password = self.password", "token: ${GITHUB_TOKEN}", "max_tokens: 4096",
		"api_key = os.environ.get_key", "password: <your password>", "pwd=abc",
		"secret_key_base = Rails.application.secret", "the word password alone",
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
		`{"type":"tool_use","id":"t","input":{"cmd":["login","` + ph + `"]}},{"type":"text","text":"{{PASSWORD_aaaaaaaaaaaa}}"}]}`
	got := string(m.restoreBody([]byte(in), false))
	want := `{"content":[{"type":"thinking","thinking":"` + ph + `"},{"type":"text","text":"a hunter2pass b"},` +
		`{"type":"tool_use","id":"t","input":{"cmd":["login","hunter2pass"]}},{"type":"text","text":"{{PASSWORD_aaaaaaaaaaaa}}"}]}`
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
