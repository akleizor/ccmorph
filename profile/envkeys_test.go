package profile

import "testing"

// TestProfileEnvComposesEveryTuningKey pins #636: a tuning key is spelled in
// TuningEnv alone, and ProfileEnv carries it without a second copy.
func TestProfileEnvComposesEveryTuningKey(t *testing.T) {
	have := map[string]bool{}
	for _, k := range ProfileEnv {
		have[k] = true
	}
	for _, k := range MappingEnv {
		if !have[k] {
			t.Errorf("ProfileEnv lost the mapping key %s", k)
		}
	}
	for _, k := range TuningEnv {
		if !have[k] {
			t.Errorf("ProfileEnv is missing the tuning key %s", k)
		}
	}
}
