package shim

import (
	"strings"
	"testing"
)

// TestListenRefusesUnusableTimeouts pins #770's intentional change: both
// seconds knobs share util.EnvSeconds, so a zero stall (which used to end
// every request at once with "gateway sent nothing for 0s") and an
// out-of-range one (which wraps or saturates depending on the architecture)
// stop the shim at start-up with an error naming the knob; a sub-nanosecond
// stall truncates to that same zero and is refused with them.
func TestListenRefusesUnusableTimeouts(t *testing.T) {
	for _, tc := range []struct{ knob, raw string }{
		{"CCMORPH_STALL_TIMEOUT", "0"},
		{"CCMORPH_STALL_TIMEOUT", "1e300"},
		{"CCMORPH_STALL_TIMEOUT", "1e-10"},
		{"CCMORPH_STALL_TIMEOUT", "9223372036.0000001"},
		{"CCMORPH_RETRY_BASE", "0"},
		{"CCMORPH_RETRY_BASE", "9223372037"},
	} {
		getenv := func(k string) string {
			if k == tc.knob {
				return tc.raw
			}
			return ""
		}
		_, _, err := Listen(Hooks{}, Options{Target: "http://x"}, getenv)
		if err == nil {
			t.Errorf("%s=%q: Listen accepted it", tc.knob, tc.raw)
			continue
		}
		if !strings.Contains(err.Error(), tc.knob) {
			t.Errorf("%s=%q: error %q does not name the knob", tc.knob, tc.raw, err)
		}
	}
}
