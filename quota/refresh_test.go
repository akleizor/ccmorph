package quota

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexExpiredSignInIsRefreshedThenRead(t *testing.T) {
	useClock(t, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sent := ""
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Get("Authorization")
		return reply(200, `{}`), nil
	})
	dir := t.TempDir()
	write := func(token, expired string) error {
		cred := `{"type":"codex","access_token":"` + token + `","expired":"` + expired + `"}`
		return os.WriteFile(filepath.Join(dir, "a.json"), []byte(cred), 0o600)
	}
	if err := write("old", "2026-11-19T08:00:00Z"); err != nil {
		t.Fatal(err)
	}
	refreshed := false
	useRefresh(t, func(string) error { refreshed = true; return write("new", "2026-11-26T08:00:00Z") })
	readCodex(httpClient(), "cx", dir)
	if !refreshed || sent != "Bearer new" {
		t.Fatalf("refreshed=%v sent %q", refreshed, sent)
	}
}
