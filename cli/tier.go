package cli

import (
	"github.com/akleizor/ccmorph/util"
)

// A saved model may be a tier word — opus, sonnet, haiku, fable — which is
// not a gateway id: every reader that classifies the model a launch runs
// resolves it first. The words are util.TierWords, the one set across util
// and cli (#520); the resolution is spelled here once (#517), where it lived
// in three hand-maintained spellings (adapterModel, contextEnv, doctor) that
// had already drifted on one edge.

// resolveTier is the one spelling of "a tier word resolves to a gateway id":
// the profile env's tier default (the key util.TierEnvKey builds), else the
// saved tier alias (the store's env key, which the settings layer can also
// carry). env reads a launch-env key, alias a saved tier word; a model that
// is not a tier word is the id itself, and no lookup is made — the word is
// util.TierWord's spelling of the model, lowercased with its window marker
// stripped, so the marker resolves in either case and a bracket that is not
// the marker leaves the id alone (#539). A value the env maps to that is
// still the same tier word leaves the alias its say, as the env named no id
// of its own. An empty value from either source is no mapping, never the
// empty id a launch would serve: the alias store already reads its own empty
// that way (tierAlias), and the env value agrees — pinned by test, after the
// three spellings answered this edge three ways (#517). Not-found (ok=false)
// is the caller's to report in its own words: adapterModel refuses the
// launch, doctor notes the missing tier, contextEnv keeps the word.
func resolveTier(model string, env func(key string) (string, bool), alias func(tier string) (string, bool)) (string, bool) {
	bare := util.TierWord(model)
	if !util.IsTier(bare) {
		return model, true
	}
	if v, ok := env(util.TierEnvKey(bare)); ok && v != "" {
		model = v
	}
	if util.TierWord(model) == bare {
		v, ok := alias(bare)
		if !ok {
			return "", false
		}
		model = v
	}
	return model, true
}
