package quota

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// readKeychain is swapped by tests.
var readKeychain = func(service, account string) ([]byte, error) {
	return exec.Command("/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w").Output()
}

// readClaude reads one Claude subscription or setup-token profile.
func readClaude(client *http.Client, name, token, plan string, tokenOnly bool) Entry {
	e := Entry{Profile: name, Kind: "claude", Plan: plan, Windows: []Window{}}
	req, _ := http.NewRequest("GET", usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	body, status, err := fetch(client, req)
	switch {
	case err != nil:
		return stale(name, e.Kind, plan, "unreachable", "not read yet: usage endpoint unreachable")
	case status == 401 || status == 403:
		if tokenOnly {
			e.Error = "this token cannot read plan usage"
		} else {
			e.Error = "sign-in refused; start a session on it to sign in"
		}
		return e
	case status == 429:
		return stale(name, e.Kind, plan, "rate-limited", "not read yet: usage endpoint is rate-limiting")
	case status != 200:
		if status >= 500 {
			return stale(name, e.Kind, plan, "endpoint error", fmt.Sprintf("not read yet: usage endpoint answered %d", status))
		}
		e.Error = fmt.Sprintf("not read yet: usage endpoint answered %d", status)
		return e
	}
	ws, werr := claudeWindows(body)
	if werr != nil {
		e.Error = werr.Error()
		return e
	}
	e.Windows = ws
	save(name, Reading{Plan: plan, Windows: ws, ReadAt: now().Unix()})
	fresh(&e)
	return e
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// claudeWindows reads the usage reply's plan windows. The reply carries either
// a `limits` array or the older five_hour/seven_day members; both are read, and
// a window the provider reported no percent for is dropped rather than guessed.
func claudeWindows(body []byte) ([]Window, error) {
	var u struct {
		Limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
		} `json:"limits"`
		FiveHour     *claudeWindow `json:"five_hour"`
		SevenDay     *claudeWindow `json:"seven_day"`
		SevenDayOpus *claudeWindow `json:"seven_day_opus"`
	}
	if json.Unmarshal(body, &u) != nil {
		return nil, errors.New("unreadable usage reply")
	}
	ws := []Window{}
	for _, l := range u.Limits {
		if l.Percent == nil {
			continue
		}
		id := windowID(l.Kind)
		ws = append(ws, Window{ID: id, Label: windowLabel(id), Used: *l.Percent / 100, ResetsAt: epoch(l.ResetsAt)})
	}
	if len(ws) == 0 {
		add := func(id string, w *claudeWindow) {
			if w == nil || w.Utilization == nil {
				return
			}
			ws = append(ws, Window{ID: id, Label: windowLabel(id), Used: *w.Utilization / 100, ResetsAt: epoch(w.ResetsAt)})
		}
		add("5h", u.FiveHour)
		add("7d", u.SevenDay)
		add("7d_opus", u.SevenDayOpus)
	}
	if len(ws) == 0 {
		return nil, errors.New("no plan windows reported")
	}
	return ws, nil
}

// windowID is the stable id `--json` carries; the text label is derived from it.
func windowID(kind string) string {
	switch kind {
	case "", "session":
		return "5h"
	case "weekly_all":
		return "7d"
	}
	if rest, ok := strings.CutPrefix(kind, "weekly_"); ok {
		return "7d_" + rest
	}
	return kind
}

func windowLabel(id string) string {
	label := strings.ReplaceAll(id, "_", " ")
	if i := strings.LastIndexByte(label, ' '); i >= 0 && label[i+1:] == "opus" {
		label = label[:i+1] + "Opus"
	}
	return label
}

// epoch is a reply's RFC3339 reset time as a Unix second, nil when absent.
func epoch(s string) *int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	v := t.Unix()
	return &v
}

// claudePlan names the tier Claude Code's own token reply recorded. The tier is
// read, never inferred from usage: a profile with no recorded tier shows none.
func claudePlan(subscriptionType, rateLimitTier string) string {
	switch strings.ToLower(subscriptionType) {
	case "pro":
		return "Pro"
	case "max":
		tier := strings.ToLower(rateLimitTier)
		switch {
		case strings.Contains(tier, "20x"):
			return "Max 20x"
		case strings.Contains(tier, "5x"):
			return "Max 5x"
		}
		return "Max"
	case "team":
		return "Team"
	case "enterprise":
		return "Enterprise"
	}
	return ""
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

// subscriptionToken is the profile's claude.ai access token and the plan tier
// recorded beside it, from the keychain item Claude Code keeps for that config
// dir (macOS) or its .credentials.json.
func subscriptionToken(dir string) (string, string, error) {
	var raw []byte
	var err error
	if runtime.GOOS == "darwin" {
		sum := sha256.Sum256([]byte(dir))
		raw, err = readKeychain("Claude Code-credentials-"+hex.EncodeToString(sum[:])[:8], keychainAccount())
	} else {
		raw, err = os.ReadFile(filepath.Join(dir, ".credentials.json"))
	}
	if err != nil {
		return "", "", errors.New("not signed in")
	}
	var c struct {
		OAuth struct {
			AccessToken      string `json:"accessToken"`
			ExpiresAt        int64  `json:"expiresAt"`
			SubscriptionType string `json:"subscriptionType"`
			RateLimitTier    string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &c) != nil || c.OAuth.AccessToken == "" {
		return "", "", errors.New("not signed in")
	}
	if c.OAuth.ExpiresAt > 0 && now().UnixMilli() >= c.OAuth.ExpiresAt {
		// The recorded plan still names the account the user has; only the read
		// is refused, so the row keeps its tier beside the refusal.
		return "", claudePlan(c.OAuth.SubscriptionType, c.OAuth.RateLimitTier),
			errors.New("sign-in expired; start a session on it to refresh")
	}
	return c.OAuth.AccessToken, claudePlan(c.OAuth.SubscriptionType, c.OAuth.RateLimitTier), nil
}

// httpClient honours the proxy environment, and the extra CA a Claude Code
// session trusts (NODE_EXTRA_CA_CERTS), so it also works inside one.
func httpClient() *http.Client {
	if transport != nil {
		return &http.Client{Transport: transport, Timeout: 15 * time.Second}
	}
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
