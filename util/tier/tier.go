// Package tier holds the tier words and the model-id spellings built from
// them. It sits beside util the way util/paths does — a leaf every package
// (util, cli, shim, adapter, profile, ctxwindow) can name without the cycle
// util's own imports would close (#544).
package tier

import (
	"slices"
	"strings"
)

// TierWords are the four tier words, in the picker's order — fable, opus,
// sonnet, haiku — the aliases a settings layer pins by word (through the env
// key TierEnvKey builds) and every route resolves without a listing: the
// served probes answer yes for one, a refusal never names one, and the sizing
// and picker walks skip them for the concrete ids they stand for. Adding or
// renaming a word touches this one list: IsTier reads it, the picker's alias
// rows, the add wizard's env keys and the env-key allowlists are built from
// it, and no other file spells a word — the dedup guard counts the quoted,
// tab-literal and uppercase-key spellings alike.
var TierWords = []string{"fable", "opus", "sonnet", "haiku"}

// IsTier is whether model is one of the tier words. The comparison is exact:
// callers pass the bare word, TierWord(model) — the one spelling of that
// normalization — where their input could carry case or the window marker.
func IsTier(model string) bool {
	return slices.Contains(TierWords, model)
}

// TierWord is the comparison spelling of a model id: the word lowercased with
// its window marker removed. It is the one normalizer the tier question asks
// of an id that could carry either — resolveTier's word, doctor's tier note
// and the served adjudicator's tier test — so a saved model answers the same
// way on every surface (#539).
func TierWord(model string) string {
	return strings.ToLower(TrimWindowMarker(model))
}

// TrimWindowMarker is a model id without its trailing [1m] context-window
// marker: opus[1m] and glm-4.6[1M] are their bare ids on every surface that
// names a model — the resolver, the served adjudicator, the report's model
// column, the launch's wire id and the adapter's own verdict (#539, #544).
// The marker is matched case-insensitively (both spellings have been served)
// and only at the end: an id's own brackets (vendor[beta]) are part of the
// id, and a bracket that is not the marker does not make a tier word of what
// precedes it. Nothing else about the id changes — the case a listing
// compares (MiniMax-M3) is the case it arrived with.
func TrimWindowMarker(model string) string {
	if len(model) >= 4 && strings.EqualFold(model[len(model)-4:], "[1m]") {
		return model[:len(model)-4]
	}
	return model
}

// TierEnvKey is the env key a tier word pins its default model through. The
// alias resolver reads it, the add wizard writes it, the alias store keeps
// it, and profile and ctxwindow build their env-key allowlists from it
// through this leaf (#423, #544); nowhere else may spell it.
func TierEnvKey(tier string) string {
	return "ANTHROPIC_DEFAULT_" + strings.ToUpper(tier) + "_MODEL"
}
