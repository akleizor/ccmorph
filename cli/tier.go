package cli

import "strings"

// The tier words and their one resolution (#517). A saved model may be a tier
// word — opus, sonnet, haiku, fable — which is not a gateway id: every reader
// that classifies the model a launch runs resolves it first. The rule lived in
// three hand-maintained spellings (adapterModel, contextEnv, doctor) that had
// already drifted on one edge; it is spelled here once.

// isTier reports whether s is one of the tier words a saved model may be.
// Adding a tier word touches this one place: the env key and the store key are
// built from the word, and no caller lists the words itself.
func isTier(s string) bool { return s == "opus" || s == "sonnet" || s == "haiku" || s == "fable" }

// resolveTier is the one spelling of "a tier word resolves to a gateway id":
// the profile env's ANTHROPIC_DEFAULT_<TIER>_MODEL, else the saved tier alias
// (the store's env key, which the settings layer can also carry). env reads a
// launch-env key, alias a saved tier word; a model that is not a tier word is
// the id itself, and no lookup is made. A value the env maps to that is still
// the same tier word leaves the alias its say, as the env named no id of its
// own. An empty value from either source is no mapping, never the empty id a
// launch would serve: the alias store already reads its own empty that way
// (tierAlias), and the env value agrees — pinned by test, after the three
// spellings answered this edge three ways (#517). Not-found (ok=false) is the
// caller's to report in its own words: adapterModel refuses the launch, doctor
// notes the missing tier, contextEnv keeps the word.
func resolveTier(model string, env func(key string) (string, bool), alias func(tier string) (string, bool)) (string, bool) {
	bare := strings.SplitN(model, "[", 2)[0]
	if !isTier(bare) {
		return model, true
	}
	if v, ok := env("ANTHROPIC_DEFAULT_" + strings.ToUpper(bare) + "_MODEL"); ok && v != "" {
		model = v
	}
	if strings.SplitN(model, "[", 2)[0] == bare {
		v, ok := alias(bare)
		if !ok {
			return "", false
		}
		model = v
	}
	return model, true
}
