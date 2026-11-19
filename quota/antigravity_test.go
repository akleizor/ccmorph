package quota

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reply shape read live from a Google AI Pro plan on 2026-11-19: a Gemini
// pool resetting within five hours, a weekly pool shared by Claude and
// gpt-oss, and tab models with no reset (no plan limit).
const antigravityReply = `{"models":{
 "gemini-3-flash":{"quotaInfo":{"remainingFraction":1,"resetTime":"2026-11-19T13:44:39Z"}},
 "gemini-2.5-pro":{"quotaInfo":{"remainingFraction":1,"resetTime":"2026-11-19T13:44:39Z"}},
 "claude-sonnet-4-6":{"quotaInfo":{"remainingFraction":0.412578,"resetTime":"2026-11-24T06:29:33Z"}},
 "gpt-oss-120b-medium":{"quotaInfo":{"remainingFraction":0.412578,"resetTime":"2026-11-24T06:29:33Z"}},
 "tab_flash_lite_preview":{"quotaInfo":{"remainingFraction":1}},
 "chat_20706":{}}}`

func TestAntigravityReadsEachPoolWithItsModels(t *testing.T) {
	useClock(t, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.String() != antigravityModelsURL || r.Header.Get("Authorization") != "Bearer live" ||
			!strings.Contains(string(body), `"project":"p-1"`) {
			return reply(401, ""), nil
		}
		return reply(200, antigravityReply), nil
	})
	dir := t.TempDir()
	cred := `{"type":"antigravity","access_token":"live","project_id":"p-1","expired":"2026-11-19T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "a@example.com.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	e := readAntigravity(httpClient(), "gp", dir)
	if e.Error != "" {
		t.Fatalf("error %q", e.Error)
	}
	got, _ := json.Marshal(e.Windows)
	want := `[{"id":"5h","label":"5h gemini","used":0,"resets_at":1795095879,"models":["gemini-2.5-pro","gemini-3-flash"]},` +
		`{"id":"7d","label":"7d claude","used":0.587422,"resets_at":1795501773,"models":["claude-sonnet-4-6","gpt-oss-120b-medium"]}]`
	if string(got) != want {
		t.Fatalf("windows\n got %s\nwant %s", got, want)
	}
}

func TestAntigravityExpiredSignInIsReportedWithoutACall(t *testing.T) {
	useClock(t, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Fatalf("an expired sign-in must not be sent: %s", r.URL)
		return nil, nil
	})
	dir := t.TempDir()
	cred := `{"type":"antigravity","access_token":"old","project_id":"p-1","expired":"2026-11-19T08:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	useRefresh(t, func(string) error { return errors.New("no cliproxyapi") })
	if e := readAntigravity(httpClient(), "gp", dir); !strings.Contains(e.Error, "sign-in expired") {
		t.Fatalf("error %q", e.Error)
	}
}

func TestAntigravityExpiredSignInIsRefreshedThenRead(t *testing.T) {
	useClock(t, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Fatalf("sent the stale token: %s", r.Header.Get("Authorization"))
		}
		return reply(200, antigravityReply), nil
	})
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")
	old := `{"type":"antigravity","access_token":"old","project_id":"p-1","expired":"2026-11-19T08:00:00Z"}`
	if err := os.WriteFile(file, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	useRefresh(t, func(d string) error {
		fresh := `{"type":"antigravity","access_token":"new","project_id":"p-1","expired":"2026-11-19T10:00:00Z"}`
		return os.WriteFile(filepath.Join(d, "a.json"), []byte(fresh), 0o600)
	})
	if e := readAntigravity(httpClient(), "gp", dir); e.Error != "" || len(e.Windows) != 2 {
		t.Fatalf("entry %+v", e)
	}
}

func useRefresh(t *testing.T, f func(string) error) {
	prev := refreshAntigravity
	refreshAntigravity = f
	t.Cleanup(func() { refreshAntigravity = prev })
}
