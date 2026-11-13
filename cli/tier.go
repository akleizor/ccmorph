package cli

import (
	"github.com/akleizor/ccmorph/util"
	"strings"
)

// A saved model may be a tier word — opus, sonnet, haiku, fable — which is
// not a gateway id: every reader that classifies the model a launch runs
// resolves it first. The words are util.IsTier's, the one set across util and
// cli (#520); the resolution is spelled here once (#517), where it lived in
// three hand-maintained spellings (adapterModel, contextEnv, doctor) that had
// already drifted on one edge.

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
	if !util.IsTier(bare) {
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
