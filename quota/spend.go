package quota

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// costLimit matches shim/sidecar.go: a price at or past it is damage, not a
// bill, and is never counted.
const costLimit = (1 << 53) / 100.0

// tokenLimit is the matching bound for a token count: a count at or past it is
// damage, not use, and is never summed.
const tokenLimit = 1 << 53

type receipt struct {
	Complete  bool         `json:"complete"`
	Profile   string       `json:"profile"`
	Timestamp string       `json:"timestamp"`
	Usage     receiptUsage `json:"usage"`
}

// receiptUsage is what a receipt's usage carries that this view counts: the
// cost a gateway billed (or a list price estimated for it) and the tokens the
// response served, which stay countable when no price was known.
type receiptUsage struct {
	Cost          *float64 `json:"cost"`
	EstimatedCost *float64 `json:"estimated_cost"`
	// Tokens arrive as the shim's whitelisted buckets; a float spelling is as
	// readable as an integer one, so both decode here.
	InputTokens              *float64 `json:"input_tokens"`
	CacheCreationInputTokens *float64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *float64 `json:"cache_read_input_tokens"`
	OutputTokens             *float64 `json:"output_tokens"`
}

// tokens is every token bucket the response carried: the use a profile served,
// visible whether or not any price was known for it. A bucket that is not a
// plain non-negative count below tokenLimit is not use and adds nothing.
func (u receiptUsage) tokens() int64 {
	var n int64
	for _, v := range []*float64{u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, u.OutputTokens} {
		if v != nil && *v >= 0 && *v < tokenLimit {
			n += int64(*v)
		}
	}
	return n
}

func billable(v *float64) bool {
	return v != nil && !math.IsInf(*v, 0) && !math.IsNaN(*v) && *v >= 0 && *v < costLimit
}

// spendEntry is a gateway or budget profile's row. It reads the receipts the
// profile's own turns wrote; it never makes a request, so it costs nothing.
func spendEntry(name, budgetRaw string) Entry {
	e := Entry{Profile: name, Kind: "gateway", Windows: []Window{}}
	s := &Spend{}
	var budget *float64
	if v, err := strconv.ParseFloat(budgetRaw, 64); err == nil && v > 0 && !math.IsInf(v, 0) {
		budget = &v
		s.BudgetUSD = budget
		e.Kind = "budget"
	}
	e.Spend = s

	today := now()
	todayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.Local)
	paths, _ := filepath.Glob(filepath.Join(stateDir(), "usage", "*.json"))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r receipt
		if json.Unmarshal(raw, &r) != nil || !r.Complete || r.Profile != name {
			continue
		}
		ts, err := time.Parse(time.RFC3339, r.Timestamp)
		if err != nil || ts.Before(monthStart) {
			continue
		}
		// Every completed response is a turn the profile served, and its tokens
		// are use a missing price must not hide (#234).
		tokens := r.Usage.tokens()
		todayTurn := !ts.Before(todayStart)
		s.MonthTurns++
		s.MonthTokens += tokens
		if todayTurn {
			s.TodayTurns++
			s.TodayTokens += tokens
		}
		var cost float64
		priced := false
		switch {
		case billable(r.Usage.Cost):
			cost, priced = *r.Usage.Cost, true
		case billable(r.Usage.EstimatedCost):
			cost, priced = *r.Usage.EstimatedCost, true
			s.Estimated = true
		}
		if priced {
			s.MonthUSD += cost
			if todayTurn {
				s.TodayUSD += cost
			}
		} else {
			// No price was known for this turn: its cost is unknown, not zero.
			s.UnpricedTurns++
		}
	}
	if budget != nil {
		resets := time.Date(today.Year(), today.Month()+1, 1, 0, 0, 0, 0, time.Local).Unix()
		used := s.MonthUSD / *budget
		e.Windows = []Window{{ID: "month", Label: "month", Used: used, ResetsAt: &resets}}
	}
	return e
}
