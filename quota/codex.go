package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexUsageURL is Codex's own usage read, the one its /status shows:
// codex-rs/backend-client/src/client/rate_limit_resets.rs builds
// `<base>/wham/usage` for ChatGPT backends. A var so tests can watch it.
var codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexAuth struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	Expired     string `json:"expired"`
}

// codexAuthFile finds the cliproxyapi Codex sign-in under the profile's auth
// dir. Symlinks are not followed: a credential outside the 0700 auth root is
// not this profile's.
func codexAuthFile(dir string) (codexAuth, error) {
	var found codexAuth
	paths := []string{}
	for _, pattern := range []string{filepath.Join(dir, "*.json"), filepath.Join(dir, "*", "*.json")} {
		m, _ := filepath.Glob(pattern)
		paths = append(paths, m...)
	}
	for _, p := range paths {
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a codexAuth
		if json.Unmarshal(raw, &a) == nil && a.Type == "codex" && a.AccessToken != "" {
			found = a
			break
		}
	}
	if found.AccessToken == "" {
		return found, errors.New("not signed in")
	}
	if found.Expired != "" {
		if t, err := time.Parse(time.RFC3339, found.Expired); err == nil && now().After(t) {
			return found, errors.New("sign-in expired; start a session on it to refresh")
		}
	}
	return found, nil
}

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
}

// readCodex reads one Codex subscription's windows and plan.
func readCodex(client *http.Client, name, dir string) Entry {
	e := Entry{Profile: name, Kind: "codex", Windows: []Window{}}
	a, err := codexAuthFile(dir)
	if err != nil {
		e.Error = err.Error()
		return e
	}
	req, _ := http.NewRequest("GET", codexUsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("User-Agent", "codex-cli")
	if a.AccountID != "" {
		req.Header.Set("chatgpt-account-id", a.AccountID)
	}
	body, status, err := fetch(client, req)
	switch {
	case err != nil:
		return stale(name, e.Kind, "", "unreachable", "not read yet: usage endpoint unreachable")
	case status == 401 || status == 403:
		e.Error = "sign-in refused; start a session on it to sign in"
		return e
	case status == 429:
		return stale(name, e.Kind, "", "rate-limited", "not read yet: usage endpoint is rate-limiting")
	case status != 200:
		if status >= 500 {
			return stale(name, e.Kind, "", "endpoint error", fmt.Sprintf("not read yet: usage endpoint answered %d", status))
		}
		e.Error = fmt.Sprintf("not read yet: usage endpoint answered %d", status)
		return e
	}
	plan, ws, perr := codexReading(body)
	if perr != nil {
		e.Error = perr.Error()
		return e
	}
	e.Plan = plan
	e.Windows = ws
	save(name, Reading{Plan: plan, Windows: ws, ReadAt: now().Unix()})
	fresh(&e)
	return e
}

// codexReading reads the usage reply. Every field is optional: a window the
// provider did not describe is dropped, never guessed.
func codexReading(body []byte) (string, []Window, error) {
	var u struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			PrimaryWindow   *codexWindow `json:"primary_window"`
			SecondaryWindow *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(body, &u) != nil {
		return "", nil, errors.New("unreadable usage reply")
	}
	ws := []Window{}
	add := func(w *codexWindow) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		id := codexWindowID(w.LimitWindowSeconds)
		resets := int64(0)
		switch {
		case w.ResetAt > 0:
			resets = w.ResetAt
		case w.ResetAfterSeconds > 0:
			resets = now().Unix() + w.ResetAfterSeconds
		}
		var at *int64
		if resets > 0 {
			at = &resets
		}
		ws = append(ws, Window{ID: id, Label: windowLabel(id), Used: *w.UsedPercent / 100, ResetsAt: at})
	}
	if u.RateLimit != nil {
		add(u.RateLimit.PrimaryWindow)
		add(u.RateLimit.SecondaryWindow)
	}
	if len(ws) == 0 {
		return "", nil, errors.New("no plan windows reported")
	}
	return codexPlan(u.PlanType), ws, nil
}

func codexWindowID(seconds int64) string {
	switch seconds {
	case 18000:
		return "5h"
	case 604800:
		return "7d"
	}
	if seconds > 0 && seconds%3600 == 0 && seconds < 86400 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	if seconds > 0 {
		return fmt.Sprintf("%dd", seconds/86400)
	}
	return "window"
}

func codexPlan(planType string) string {
	switch strings.ToLower(planType) {
	case "":
		return ""
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "team":
		return "Team"
	case "business":
		return "Business"
	case "enterprise":
		return "Enterprise"
	case "edu":
		return "Edu"
	case "free":
		return "Free"
	case "go":
		return "Go"
	}
	return planType
}
