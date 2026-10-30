package quota

// Window is one plan window as both the text view and `--json` carry it: Used
// is the fraction of the window spent (0.12 is 12%), ResetsAt the Unix second
// the provider says it resets, nil when the provider did not say.
type Window struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Used     float64 `json:"used"`
	ResetsAt *int64  `json:"resets_at"`
}

// Reading is what one usage endpoint answered, and when. It is also what the
// cache holds, so a later rate limit can show the last good numbers.
type Reading struct {
	Plan    string   `json:"plan,omitempty"`
	Windows []Window `json:"windows"`
	ReadAt  int64    `json:"read_at"`
}

// Spend is a gateway or budget profile's billed total from its receipts.
// Estimated is true when any counted row carried only a list-price estimate.
type Spend struct {
	TodayUSD  float64  `json:"today_usd"`
	MonthUSD  float64  `json:"month_usd"`
	Estimated bool     `json:"estimated"`
	BudgetUSD *float64 `json:"budget_usd,omitempty"`
}

// Entry is one profile's row. Kind is "claude", "codex", "budget" or "gateway";
// budget and gateway entries carry Spend and never a network reading.
type Entry struct {
	Profile    string   `json:"profile"`
	Kind       string   `json:"kind"`
	Plan       string   `json:"plan,omitempty"`
	Windows    []Window `json:"windows"`
	ReadAt     *int64   `json:"read_at"`
	AgeSeconds *int64   `json:"age_seconds"`
	Stale      bool     `json:"stale"`
	Error      string   `json:"error,omitempty"`
	Spend      *Spend   `json:"spend,omitempty"`
	// Balance is what is left on the account behind an API key, "¥40.75", where
	// the provider publishes it (balance.go); empty otherwise.
	Balance string `json:"balance,omitempty"`
}
