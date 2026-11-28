// Package modelid owns the spellings of a model id every package compares: the
// four tier words, the trailing context-window marker, and the env key a tier
// word pins through. It is a leaf (standard library only), so profile,
// ctxwindow, shim, adapter, router and util all name the one rule instead of
// spelling it again (#544).
package modelid

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
// rows and the add wizard's env keys are built from it, and no other file
// spells a word — the dedup guard counts the quoted, tab-literal and
// uppercase-key spellings alike.
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

// WindowMarker is ccmorph's own context-window marker as it is written: a
// suffix on a model id, never part of the id a route serves. Readers strip it
// with TrimWindowMarker, which matches either case.
const WindowMarker = "[1m]"

// HasWindowMarker is whether model ends in the window marker, in either case.
func HasWindowMarker(model string) bool {
	return TrimWindowMarker(model) != model
}

// TrimWindowMarker is a model id without its trailing [1m] context-window
// marker: opus[1m] and glm-4.6[1M] are their bare ids on every surface that
// names a model — the alias resolver, the served adjudicator, the report's
// model column (#539). The marker is matched case-insensitively (both
// spellings have been served) and only at the end: an id's own brackets
// (vendor[beta]) are part of the id, and a bracket that is not the marker
// does not make a tier word of what precedes it. Nothing else about the id
// changes — the case a listing compares (MiniMax-M3) is the case it arrived
// with.
func TrimWindowMarker(model string) string {
	if len(model) >= len(WindowMarker) && strings.EqualFold(model[len(model)-len(WindowMarker):], WindowMarker) {
		return model[:len(model)-len(WindowMarker)]
	}
	return model
}

// CutWindowMarker is the id Claude Code's engine sizes: everything before the
// first window marker, wherever it sits, in either case. Claude Code reads the
// tag anywhere in an id (claude-x[1m]-eu is claude-x at 1M), so the engine
// mirrors (the reserve, the window registry walk, the route ceiling) cut there;
// every other surface strips only a trailing marker with TrimWindowMarker. An
// id's own brackets (vendor[beta]) are kept either way.
func CutWindowMarker(model string) string {
	for i := 0; i+len(WindowMarker) <= len(model); i++ {
		if strings.EqualFold(model[i:i+len(WindowMarker)], WindowMarker) {
			return model[:i]
		}
	}
	return model
}

// TierEnvKey is the env key a tier word pins its default model through. The
// alias resolver reads it, the add wizard writes it and the alias store keeps
// it; profile and ctxwindow build their allowlists from it too, so nothing else
// spells the key.
func TierEnvKey(tier string) string {
	return "ANTHROPIC_DEFAULT_" + strings.ToUpper(tier) + "_MODEL"
}
