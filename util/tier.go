package util

// IsTier is whether model is one of the four tier words — opus, sonnet, haiku,
// fable — the aliases a settings layer pins by word
// (ANTHROPIC_DEFAULT_OPUS_MODEL and its kin) and every route resolves without
// a listing: the served probes answer yes for one, a refusal never names one,
// and the sizing and picker walks skip them for the concrete ids they stand
// for. Adding a tier word touches this one definition: the env key and the
// store key are built from the word, and no caller lists the words itself.
// The comparison is exact: callers pass the bare word, lowercased and
// [1m]-stripped where their input could carry either.
func IsTier(model string) bool {
	return model == "opus" || model == "sonnet" || model == "haiku" || model == "fable"
}
