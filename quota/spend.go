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

type receipt struct {
	Complete  bool   `json:"complete"`
	Profile   string `json:"profile"`
	Timestamp string `json:"timestamp"`
	Usage     struct {
		Cost          *float64 `json:"cost"`
		EstimatedCost *float64 `json:"estimated_cost"`
	} `json:"usage"`
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
		var cost float64
		switch {
		case billable(r.Usage.Cost):
			cost = *r.Usage.Cost
		case billable(r.Usage.EstimatedCost):
			cost = *r.Usage.EstimatedCost
			s.Estimated = true
		default:
			continue
		}
		s.MonthUSD += cost
		if !ts.Before(todayStart) {
			s.TodayUSD += cost
		}
	}
	if budget != nil {
		resets := time.Date(today.Year(), today.Month()+1, 1, 0, 0, 0, 0, time.Local).Unix()
		used := s.MonthUSD / *budget
		e.Windows = []Window{{ID: "month", Label: "month", Used: used, ResetsAt: &resets}}
	}
	return e
}
