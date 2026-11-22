package util

import (
	"strings"
	"testing"
	"time"
)

// TestEnvSecondsBoundaries pins the one owner of the seconds-to-duration rule
// (#770): 9223372036 s is the most a time.Duration holds, and one more wraps
// negative; zero reads as an elapsed deadline; a non-number is unusable. The
// old copies differed on every one of these, so this table fails against
// either of them and passes against EnvSeconds. A float64 near the bound
// cannot tell 9223372036.0000001 from 9223372036, and a value under one
// nanosecond truncates to the zero this issue removes, so both are judged on
// the exact decimal.
func TestEnvSecondsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"9223372036", 9223372036 * time.Second, false},
		{"9223372037", 0, true},
		{"9223372036.5", 0, true},
		{"1e300", 0, true},
		{"0", 0, true},
		{"-5", 0, true},
		{"soon", 0, true},
		{"NaN", 0, true},
		{"0.5", 500 * time.Millisecond, false},
		{"9223372036.0000001", 0, true},
		{"9.2233720360000001e9", 0, true},
		{"9223372036.000", 9223372036 * time.Second, false},
		{"9.223372036e9", 9223372036 * time.Second, false},
		{"9223372035.9999999", 9223372035999999900, false},
		{"0.000000001", time.Nanosecond, false},
		{"0.0000000001", 0, true},
		{"1e-10", 0, true},
		{"1e308", 0, true},
		{"1e400", 0, true},
	} {
		name := "CCMORPH_TEST_SECONDS"
		got, err := EnvSeconds(func(k string) string {
			if k == name {
				return tc.raw
			}
			return ""
		}, name, 7)
		if tc.wantErr {
			if err == nil {
				t.Errorf("EnvSeconds(%q) = %v, want an error", tc.raw, got)
			} else if !strings.Contains(err.Error(), name) {
				t.Errorf("EnvSeconds(%q) error %q, want it to name %s", tc.raw, err, name)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("EnvSeconds(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
}

func TestEnvSecondsUnsetKeepsTheDefault(t *testing.T) {
	got, err := EnvSeconds(func(string) string { return "" }, "CCMORPH_TEST_SECONDS", 0.5)
	if err != nil || got != 500*time.Millisecond {
		t.Fatalf("unset: %v, %v; want 500ms", got, err)
	}
}
