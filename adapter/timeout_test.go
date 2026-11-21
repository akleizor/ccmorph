package adapter

import (
	"testing"
	"time"
)

// TestTimeoutEnvKeepsTheDefault pins the adapter's error policy for its two
// timeout knobs (#770): an unusable value, including one whose nanoseconds
// would wrap negative, keeps the documented default rather than stopping a
// launch. A fractional value is accepted like any other.
func TestTimeoutEnvKeepsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 30 * time.Second},
		{"90", 90 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"9223372037", 30 * time.Second},
		{"1e300", 30 * time.Second},
		{"0", 30 * time.Second},
		{"-5", 30 * time.Second},
		{"soon", 30 * time.Second},
	} {
		t.Setenv("CCMORPH_TEST_TIMEOUT", tc.raw)
		if got := timeoutEnv("CCMORPH_TEST_TIMEOUT", 30); got != tc.want {
			t.Errorf("timeoutEnv(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
