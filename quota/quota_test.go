package quota

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQuota(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.Header.Get("Authorization") {
		case "Bearer live":
			w.Write([]byte(`{"five_hour":{"utilization":3.0},"limits":[` +
				`{"kind":"session","percent":12,"resets_at":"2026-10-29T19:10:00.788+00:00"},` +
				`{"kind":"weekly_all","percent":40,"resets_at":"2026-11-03T01:00:00+00:00"},` +
				`{"kind":"weekly_opus","percent":null}]}`))
		default:
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	usageURL = srv.URL
	now = func() time.Time { return time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC) }
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })

	home := t.TempDir()
	creds := map[string]string{
		"live":    `{"claudeAiOauth":{"accessToken":"live","expiresAt":1893456000000}}`,
		"expired": `{"claudeAiOauth":{"accessToken":"old","expiresAt":1000}}`,
	}
	for name, c := range creds {
		dir := filepath.Join(home, name)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(c), 0o600)
	}
	if runtime.GOOS == "darwin" {
		readKeychain = func(service string) ([]byte, error) {
			for name, c := range creds {
				if service == "Claude Code-credentials-"+hashOf(filepath.Join(home, name)) {
					return []byte(c), nil
				}
			}
			return nil, os.ErrNotExist
		}
	}
	in := strings.Join([]string{
		"live\tsub\t" + filepath.Join(home, "live"),
		"expired\tsub\t" + filepath.Join(home, "expired"),
		"none\tsub\t" + filepath.Join(home, "none"),
		"tk\ttoken\t" + home + "\tsk-ant-oat-setup",
	}, "\n")
	var out, errw bytes.Buffer
	if code := Main(nil, strings.NewReader(in), &out, &errw); code != 0 {
		t.Fatal(errw.String())
	}
	want := "live         5h 12% (19:10)  week 40% (Tue 01:00)\n" +
		"expired      sign-in expired; open a session on this profile to refresh it\n" +
		"none         not signed in\n" +
		"tk           not available: this token cannot read plan usage\n"
	if out.String() != want {
		t.Fatalf("\n got %q\nwant %q", out.String(), want)
	}
	// An expired or missing sign-in is never sent.
	if strings.Join(auth, ",") != "Bearer live,Bearer sk-ant-oat-setup" {
		t.Fatalf("sent %v", auth)
	}
}

func hashOf(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:])[:8]
}
