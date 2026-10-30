package util

import "testing"

// The engine's env boolean: ECMAScript trim (U+FEFF included, NEL not), then 1/true/yes/on.
func TestEngineBoolReadsLikeTheEngine(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on", "TRUE", "On", " on ", "yes ", "\ufeffon", "\ufeff1"} {
		if !engineBool(v) {
			t.Errorf("%q must read as true", v)
		}
	}
	for _, v := range []string{"off", "no", "false", "0", "2", "onx", "", "\ufeffoff", "\u3080on", "\u0085on"} {
		if engineBool(v) {
			t.Errorf("%q must read as false", v)
		}
	}
}
