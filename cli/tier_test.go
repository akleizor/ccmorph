package cli

import "testing"

// The tier resolution, pinned once for the three readers that share it (#517):
// adapterModel, contextEnv and doctor each spelled the rule by hand, and the
// spellings had already drifted on the empty-value edge. The lookups stand in
// for the two sources — the profile env and the saved alias — and answer only
// the opus key and word, so a resolver that builds the wrong key, or hands the
// alias the model's marker spelling, finds nothing and fails the case.
func TestATierWordResolvesThroughTheEnvThenTheSavedAlias(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		envValue   string // the env mapping's value; envSet false leaves it unset
		envSet     bool
		aliasValue string // the saved alias's value; aliasSet false leaves nothing saved
		aliasSet   bool
		want       string
		wantOK     bool
	}{
		{"a plain id is the id itself", "m-id", "", false, "", false, "m-id", true},
		{"a tier word with nothing mapped is not found", "opus", "", false, "", false, "", false},
		{"a tier word resolves through the saved alias", "opus", "", false, "m-alias", true, "m-alias", true},
		{"a tier word resolves through the env mapping first", "opus", "m-env", true, "m-alias", true, "m-env", true},
		{"the marker spelling of a tier word resolves too", "opus[1m]", "", false, "m-alias", true, "m-alias", true},
		{"an env mapping set but empty is no mapping and the alias answers (#517)", "opus", "", true, "m-alias", true, "m-alias", true},
		{"an env mapping set but empty with no alias is not found (#517)", "opus", "", true, "", false, "", false},
		{"an env mapping naming the same tier word leaves the alias its say", "opus", "opus[1m]", true, "m-store", true, "m-store", true},
		{"an env mapping naming another tier word is that word", "opus", "sonnet", true, "", false, "sonnet", true},
		{"an empty model is no tier and stays empty", "", "", false, "", false, "", true},
		// #539: the word is util.TierWord's spelling — lowercased, marker
		// stripped — so these three answer the same way the served adjudicator
		// does, where the old split-at-the-first-bracket spelling answered
		// "opus[foo]" as the tier word opus and never saw "OPUS" at all.
		{"an uppercase word is the word", "OPUS", "", false, "m-alias", true, "m-alias", true},
		{"the marker's other case resolves too", "opus[1M]", "", false, "m-alias", true, "m-alias", true},
		{"a bracket that is not the marker is the id itself", "opus[foo]", "", false, "", false, "opus[foo]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveTier(tc.model,
				func(key string) (string, bool) {
					if key != "ANTHROPIC_DEFAULT_OPUS_MODEL" {
						return "", false
					}
					return tc.envValue, tc.envSet
				},
				func(tier string) (string, bool) {
					if tier != "opus" {
						return "", false
					}
					return tc.aliasValue, tc.aliasSet
				})
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("resolveTier(%q) = (%q, %v), want (%q, %v)", tc.model, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
