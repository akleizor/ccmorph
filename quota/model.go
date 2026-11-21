package quota

// Window is one plan window as both the text view and `--json` carry it: Used
// is the fraction of the window spent (0.12 is 12%), ResetsAt the Unix second
// the provider says it resets, nil when the provider did not say.
type Window struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Used     float64 `json:"used"`
	ResetsAt *int64  `json:"resets_at"`
	// Models names the models that draw on this window when the plan keeps
	// separate pools (Antigravity); empty means every model on the profile.
	Models []string `json:"models,omitempty"`
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

// AllUnpriced reports whether the profile served turns this month and not one
// of them carried a price: the money lines then say the cost is unknown (#234),
// and a budget bar has no share to show, so it says so too instead of 0% (#301).
// It is the exported owner of that answer: quota's own lines and the router's
// month bar consult it, and no reader re-derives it from the counts (#776).
// A nil spend — a plan window's entry, never a spend's — is never all-unpriced.
func (s *Spend) AllUnpriced() bool {
	return s != nil && s.MonthTurns > 0 && s.UnpricedTurns == s.MonthTurns
}

// Entry is one profile's row. Kind is "claude", "codex", "antigravity", "budget" or "gateway";
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
	// Checked answers, for a gateway or budget row, whether the endpoint
	// answered this run. false says it answered nothing: the spend beside it
	// comes from local receipts alone, the text row says so, and `--json`
	// carries the same honesty as checked:false — the row's zero is not a
	// reading that found no spend (#726). true says the endpoint answered (any
	// answer, even a refusal, proves it is there) while the receipts stay the
	// only spend. nil, and absent from the JSON, says the row has no endpoint
	// to ask: no base URL, no key, Anthropic's own base, or a plan row, which
	// carries its own reading and its own Stale.
	Checked *bool `json:"checked,omitempty"`
}
