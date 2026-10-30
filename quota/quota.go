// Package quota is `ccmorph quota`: what each Claude subscription profile has
// left of its plan windows (5-hour and weekly), read from claude.ai's usage
// endpoint, the one Claude Code's /usage reads. A GET with the profile's own
// sign-in: no inference, nothing billed. A sign-in whose access token has
// expired is reported, never refreshed here: refreshing would rotate the token
// under the Claude Code sessions that hold it.
package quota

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// usageURL and readKeychain are swapped by tests.
var (
	usageURL     = "https://api.anthropic.com/api/oauth/usage"
	readKeychain = func(service, account string) ([]byte, error) {
		return exec.Command("/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w").Output()
	}
	now = time.Now
)

// Main reads one profile per stdin line, "name<TAB>type<TAB>config dir<TAB>token"
// (the token only for type token), and prints one line each.
func Main(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) != 0 {
		fmt.Fprintln(stderr, "usage: ccmorph-core quota < profiles")
		return 2
	}
	client := httpClient()
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	seen := false
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		seen = true
		token := ""
		if len(f) > 3 {
			token = f[3]
		}
		fmt.Fprintf(stdout, "%-12s %s\n", f[0], report(client, f[1], f[2], token))
	}
	if !seen {
		fmt.Fprintln(stdout, "No subscription or token profiles.")
	}
	return 0
}

func report(client *http.Client, typ, dir, token string) string {
	if typ == "sub" {
		var err error
		if token, err = subscriptionToken(dir); err != nil {
			return err.Error()
		}
	}
	if token == "" {
		return "no sign-in"
	}
	req, _ := http.NewRequest("GET", usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := client.Do(req)
	if err != nil {
		return "unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		if typ == "token" {
			return "not available: this token cannot read plan usage"
		}
		return "sign-in refused; open a session on this profile to sign in again"
	case resp.StatusCode != 200:
		return fmt.Sprintf("usage endpoint answered %d", resp.StatusCode)
	}
	return windows(body)
}

type window struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
}

// windows is the reply's limits, "5h 12% (19:10)  week 40% (Tue 09:00)".
func windows(body []byte) string {
	var u struct {
		Limits   []window `json:"limits"`
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day"`
	}
	if json.Unmarshal(body, &u) != nil {
		return "unreadable usage reply"
	}
	ws := u.Limits
	if len(ws) == 0 {
		if u.FiveHour != nil {
			ws = append(ws, window{"session", u.FiveHour.Utilization, u.FiveHour.ResetsAt})
		}
		if u.SevenDay != nil {
			ws = append(ws, window{"weekly_all", u.SevenDay.Utilization, u.SevenDay.ResetsAt})
		}
	}
	var parts []string
	for _, w := range ws {
		if w.Percent == nil {
			continue
		}
		p := fmt.Sprintf("%s %.0f%%", label(w.Kind), *w.Percent)
		if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			layout := "15:04"
			if t.Sub(now()) > 20*time.Hour {
				layout = "Mon 15:04"
			}
			p += " (" + t.Local().Format(layout) + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no plan windows reported"
	}
	return strings.Join(parts, "  ")
}

func label(kind string) string {
	switch kind {
	case "session":
		return "5h"
	case "weekly_all":
		return "week"
	}
	if rest, ok := strings.CutPrefix(kind, "weekly_"); ok {
		return "week " + rest
	}
	return kind
}

// keychainAccount is the account name of the user's keychain item. The service
// name is shared with an MCP-only item under another account, so the lookup
// names the user's account, as addons/remote-control/ccmorph-rc-hosts does.
func keychainAccount() string {
	for _, k := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// subscriptionToken is the profile's claude.ai access token, from the keychain
// item Claude Code keeps for that config dir (macOS) or its .credentials.json.
func subscriptionToken(dir string) (string, error) {
	var raw []byte
	var err error
	if runtime.GOOS == "darwin" {
		sum := sha256.Sum256([]byte(dir))
		raw, err = readKeychain("Claude Code-credentials-"+hex.EncodeToString(sum[:])[:8], keychainAccount())
	} else {
		raw, err = os.ReadFile(filepath.Join(dir, ".credentials.json"))
	}
	if err != nil {
		return "", errors.New("not signed in")
	}
	var c struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &c) != nil || c.OAuth.AccessToken == "" {
		return "", errors.New("not signed in")
	}
	if c.OAuth.ExpiresAt > 0 && now().UnixMilli() >= c.OAuth.ExpiresAt {
		return "", errors.New("sign-in expired; open a session on this profile to refresh it")
	}
	return c.OAuth.AccessToken, nil
}

// httpClient honours the proxy environment, and the extra CA a Claude Code
// session trusts (NODE_EXTRA_CA_CERTS), so it also works inside one.
func httpClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if path := os.Getenv("NODE_EXTRA_CA_CERTS"); path != "" {
		if pem, err := os.ReadFile(path); err == nil {
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			if pool.AppendCertsFromPEM(pem) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}
