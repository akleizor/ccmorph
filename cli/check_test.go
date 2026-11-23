package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkOutput runs `ccmorph check` and returns its stdout, stderr and status.
func checkOutput(t *testing.T, l *launcher, args ...string) (string, string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	out, rc := captureStdout(t, func() int { return l.verb(append([]string{"check"}, args...)) })
	os.Stderr = old
	w.Close()
	errText, _ := io.ReadAll(r)
	return out, string(errText), rc
}

func TestCheckPrintsTheContractAndNeverTheKey(t *testing.T) {
	const key = "sk-check-0123456789abcdefghijklmnop"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"message":"key `+r.Header.Get("x-api-key")+r.Header.Get("Authorization")+` may not use this model"}}`)
	}))
	defer srv.Close()
	state, profiles := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "gw.conf"), []byte("type=apikey\napi_key="+key+"\nbase_url="+srv.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &launcher{dir: state, profileDirs: profiles}

	out, errText, rc := checkOutput(t, l, "gw", "some-model", "--json")
	if rc != 1 {
		t.Fatalf("rc %d, want 1 for a refusal; stderr %s", rc, errText)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	if len(got) != 5 || got["ok"] != false || got["route"] != "chat" || got["status"] != float64(403) {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["ms"].(float64); !ok || !strings.HasSuffix(got["note"].(string), "may not use this model") {
		t.Fatalf("got %v", got)
	}
	if hits != 3 {
		t.Fatalf("%d requests, want one per route", hits)
	}
	if strings.Contains(out+errText, key) {
		t.Fatalf("the key was printed:\n%s%s", out, errText)
	}

	out, errText, rc = checkOutput(t, l, "gw", "some-model")
	if rc != 1 || !strings.HasPrefix(out, "gw some-model: refused on chat (HTTP 403, ") || strings.Contains(out+errText, key) {
		t.Fatalf("rc %d, plain line %q, stderr %q", rc, out, errText)
	}
}

func TestCheckRefusesAnUnknownProfileAsUsage(t *testing.T) {
	l := &launcher{dir: t.TempDir(), profileDirs: t.TempDir()}
	for _, args := range [][]string{{"nope", "m"}, {"only-one"}} {
		if out, _, rc := checkOutput(t, l, args...); rc != 2 || out != "" {
			t.Fatalf("%v: rc %d, stdout %q", args, rc, out)
		}
	}
}
