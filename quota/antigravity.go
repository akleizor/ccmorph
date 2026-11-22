package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

// antigravityModelsURL is Antigravity's own model list, the one its IDE and
// cliproxyapi call; it carries each model's quota. A var so tests can watch it.
var antigravityModelsURL = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

// readAntigravity reads one Antigravity (Google AI plan) profile's quota pools.
func readAntigravity(client *http.Client, name, dir string) Entry {
	e := Entry{Profile: name, Kind: "antigravity", Windows: []Window{}}
	a, err := clipAuthFile(dir, "antigravity")
	if errors.Is(err, errSignInExpired) {
		return stale(name, e.Kind, "", "sign-in expired", "sign-in expired; run `ccmorph login "+name+"` to sign in again")
	}
	if err != nil {
		e.Error = err.Error()
		return e
	}
	payload, _ := json.Marshal(map[string]string{"project": a.ProjectID})
	req, _ := http.NewRequest("POST", antigravityModelsURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity")
	body, status, err := fetch(client, req)
	if v, ok := usageVerdict(e, status, err); !ok {
		return v
	}
	ws, perr := antigravityReading(body)
	if perr != nil {
		e.Error = perr.Error()
		return e
	}
	e.Windows = ws
	save(name, Reading{Windows: ws, ReadAt: now().Unix()})
	fresh(&e)
	return e
}

// antigravityReading turns the per-model quota into windows. Models that share
// a pool report the same remaining fraction and reset, so each distinct pair
// is one window carrying its models. The reply has no window length: a pool
// resetting within five hours is the plan's 5h pool, a later one its weekly
// pool. A model without a reset time (tab completion) has no plan limit.
func antigravityReading(body []byte) ([]Window, error) {
	var u struct {
		Models map[string]struct {
			QuotaInfo *struct {
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &u) != nil {
		return nil, errors.New("unreadable usage reply")
	}
	type pool struct {
		left   float64
		resets int64
	}
	pools := map[pool][]string{}
	for model, m := range u.Models {
		q := m.QuotaInfo
		if q == nil || q.RemainingFraction == nil {
			continue
		}
		at := epoch(q.ResetTime)
		if at == nil {
			continue
		}
		p := pool{*q.RemainingFraction, *at}
		pools[p] = append(pools[p], model)
	}
	ws := []Window{}
	for p, models := range pools {
		sort.Strings(models)
		id := "7d"
		if time.Unix(p.resets, 0).Sub(now()) <= 5*time.Hour {
			id = "5h"
		}
		resets := p.resets
		// The label names the pool by its first model's family ("7d claude"), so
		// the text view tells the pools apart.
		family, _, _ := strings.Cut(models[0], "-")
		used := 1 - p.left
		ws = append(ws, Window{ID: id, Label: id + " " + family, Used: &used, ResetsAt: &resets, Models: models})
	}
	if len(ws) == 0 {
		return nil, errors.New("no plan windows reported")
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].ID != ws[j].ID {
			return ws[i].ID < ws[j].ID
		}
		return ws[i].Models[0] < ws[j].Models[0]
	})
	return ws, nil
}
