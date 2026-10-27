package shim

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hooked posts body to a shim with hooks h and returns the headers and body the
// upstream received.
func hooked(t *testing.T, h Hooks, body string, hdr ...string) (http.Header, string) {
	var gotH http.Header
	var gotB []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotH = r.Header.Clone()
		gotB, _ = io.ReadAll(r.Body)
	}))
	defer up.Close()
	s := NewServer(up.URL)
	s.Hooks = h
	post(t, front(t, s), "/v1/messages", body, hdr...)
	return gotH, string(gotB)
}

func TestHookRewritesTheUserAgent(t *testing.T) {
	got, _ := hooked(t, Hooks{UserAgent: func(c string) string { return "hooks-ua[" + c + "]" }}, "{}", "User-Agent", "claude-cli/2.1.300")
	if got.Get("User-Agent") != "hooks-ua[claude-cli/2.1.300]" {
		t.Fatal(got)
	}
}

func TestWithoutAUserAgentHookTheClientsIsForwarded(t *testing.T) {
	got, _ := hooked(t, Hooks{RepairRequest: func([]byte) ([]byte, bool) { return nil, false }}, "{}", "User-Agent", "claude-cli/2.1.300")
	if got.Get("User-Agent") != "claude-cli/2.1.300" {
		t.Fatal(got)
	}
}

func TestHookRepairReachesTheUpstreamBodyAndSessionReachesHeaders(t *testing.T) {
	var sessions []string
	h := Hooks{
		RepairRequest: func(b []byte) ([]byte, bool) { return []byte(`{"hooks":"clean"}`), string(b) == `{"hooks":"dirty"}` },
		OutboundHeaders: func(h http.Header, session string) {
			sessions = append(sessions, session)
			h.Set("X-Affinity", session)
		},
	}
	_, body := hooked(t, h, `{"hooks":"dirty"}`)
	if body != `{"hooks":"clean"}` {
		t.Fatal(body)
	}
	user := `{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"sess-1\"}"}}`
	got, _ := hooked(t, h, user)
	if got.Get("X-Affinity") != "sess-1" {
		t.Fatal("hooks need the session id on every request, with no flag set", got)
	}
	// No session id: the hook is told so with "", not skipped.
	hooked(t, h, "{}")
	if len(sessions) != 3 || sessions[2] != "" {
		t.Fatal(sessions)
	}
}
