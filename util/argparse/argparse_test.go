package argparse

import (
	"slices"
	"testing"
)

func TestMatches(t *testing.T) {
	opts := []string{"--profile", "--model", "--check", "--probe-all"}
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"--profile", []string{"--profile"}},            // an exact name is its own only match
		{"--pro", []string{"--profile", "--probe-all"}}, // prefixes, in opts order
		{"--model", []string{"--model"}},                // exact wins even beside prefixes
		{"--x", nil},                                    // nothing matches
	} {
		if got := Matches(tc.name, opts); !slices.Equal(got, tc.want) {
			t.Errorf("Matches(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A longer option that starts with name must not survive name's own exact
	// entry, wherever the two sit in opts.
	if got := Matches("--p", []string{"--probe-all", "--p"}); !slices.Equal(got, []string{"--p"}) {
		t.Errorf("Matches exact-beats-prefix = %v, want [--p]", got)
	}
}

func TestOptionLike(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want bool
	}{
		{"-x", true},    // an option
		{"--", true},    // the end-of-options marker is not a value
		{"-5", false},   // a negative number is a value
		{"-1e9", false}, // so is one in exponent form
		{"-", false},    // a lone dash is a value (stdin, say)
		{"x", false},    // a plain word
		{"", false},
	} {
		if got := OptionLike(tc.arg); got != tc.want {
			t.Errorf("OptionLike(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}
