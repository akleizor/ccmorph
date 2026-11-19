package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// codexUsageURL is Codex's own usage read, the one its /status shows:
// codex-rs/backend-client/src/client/rate_limit_resets.rs builds
// `<base>/wham/usage` for ChatGPT backends. A var so tests can watch it.
var codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
}

// readCodex reads one Codex subscription's windows and plan.
func readCodex(client *http.Client, name, dir string) Entry {
	e := Entry{Profile: name, Kind: "codex", Windows: []Window{}}
	a, err := clipAuthFile(dir, "codex")
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
	if v, ok := usageVerdict(e, status, err); !ok {
		return v
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
