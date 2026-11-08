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
// Turns and Tokens count every completed turn the profile served in the window
// and the tokens those turns carried, priced or not, and UnpricedTurns is how
// many of the month's turns carried no cost at all — their price was unknown,
// so they add to the counts and never to the money.
type Spend struct {
	TodayUSD  float64  `json:"today_usd"`
	MonthUSD  float64  `json:"month_usd"`
	Estimated bool     `json:"estimated"`
	BudgetUSD *float64 `json:"budget_usd,omitempty"`
	// TodayTurns and MonthTurns are the completed responses the profile served
	// in the window; TodayTokens and MonthTokens the tokens they carried.
	TodayTurns    int   `json:"today_turns"`
	MonthTurns    int   `json:"month_turns"`
	TodayTokens   int64 `json:"today_tokens"`
	MonthTokens   int64 `json:"month_tokens"`
	UnpricedTurns int   `json:"unpriced_turns"`
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
