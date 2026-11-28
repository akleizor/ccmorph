package modelid

import "testing"

// #544: the marker is stripped in either case and only at the end; an id's own
// brackets are part of the id.
func TestTrimWindowMarkerOnlyTheTrailingMarker(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5[1m]": "claude-opus-5",
		"glm-4.6[1M]":       "glm-4.6",
		"vendor[beta]":      "vendor[beta]",
		"vendor[beta][1m]":  "vendor[beta]",
		"opus":              "opus",
		"[1m]":              "",
		"1m]":               "1m]",
	} {
		if got := TrimWindowMarker(in); got != want {
			t.Errorf("TrimWindowMarker(%q) = %q, want %q", in, got, want)
		}
		if got := HasWindowMarker(in); got != (want != in) {
			t.Errorf("HasWindowMarker(%q) = %v", in, got)
		}
	}
}

func TestTierWordAndEnvKey(t *testing.T) {
	if got := TierWord("Opus[1M]"); got != "opus" || !IsTier(got) {
		t.Fatalf("TierWord(Opus[1M]) = %q", got)
	}
	if got := TierEnvKey("sonnet"); got != "ANTHROPIC_DEFAULT_SONNET_MODEL" {
		t.Fatalf("TierEnvKey = %q", got)
	}
}

// The engine mirrors cut at the first marker anywhere, as Claude Code reads it.
func TestCutWindowMarkerAnywhere(t *testing.T) {
	for in, want := range map[string]string{
		"claude-x[1m]-eu":  "claude-x",
		"claude-x[1M]":     "claude-x",
		"vendor[beta]":     "vendor[beta]",
		"vendor[beta][1m]": "vendor[beta]",
		"plain":            "plain",
	} {
		if got := CutWindowMarker(in); got != want {
			t.Errorf("CutWindowMarker(%q) = %q, want %q", in, got, want)
		}
	}
}
