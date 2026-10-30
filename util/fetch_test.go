package util

import (
	"strings"
	"testing"
)

func TestAPIURLAddsV1OnlyToAnUnversionedBase(t *testing.T) {
	for base, want := range map[string]string{
		"https://provider.invalid/api/v4": "https://provider.invalid/api/v4/models",
		"https://provider.invalid/api/V4": "https://provider.invalid/api/V4/models",
		"https://provider.invalid/":       "https://provider.invalid/v1/models",
	} {
		if got := apiURL(base, "/v1/models"); got != want {
			t.Errorf("apiURL(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestListingsAreUndisguisedOnEntry(t *testing.T) {
	got := undisguise(`{"data":[{"id":"claude-fable-5-dd-los-6-tpg","display_name":"x"},{"id":"claude-opus-5"}]}`)
	for _, want := range []string{`"id":"gpt-6-sol"`, `"id":"claude-opus-5"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "-dd-") {
		t.Errorf("a disguise survived: %s", got)
	}
}

func TestModelServedRulesBeforeAnyRequest(t *testing.T) {
	for _, c := range []struct {
		want, typ, base string
		rc              int
	}{
		{"opus", "apikey", "https://gw.invalid", 0},
		{"claude-x[1M]", "sub", "", 0},
		{"gpt-6", "sub", "", 1},
		{"claude-x", "provider", "", 1},
		{"gpt-6", "provider", "", 2},
		{"gpt-6", "apikey", "https://gw.invalid", 2}, // no key: cannot tell
	} {
		if rc := modelServed([]string{c.want, c.typ, c.base, ""}, strings.NewReader("\n"), nil, nil); rc != c.rc {
			t.Errorf("%+v: rc %d", c, rc)
		}
	}
}
