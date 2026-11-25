package ctxwindow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPinnedValueSharesPositivesLengthRule pins #636: an 11-digit pin skipped
// probing while Positive refused it, so it could never resolve.
func TestPinnedValueSharesPositivesLengthRule(t *testing.T) {
	for in, want := range map[string]bool{
		"1234567890":  true,
		"12345678901": false,
		"0":           false,
		"":            false,
		"0000000001":  true,
	} {
		if got := pinnedValue(in); got != want {
			t.Errorf("pinnedValue(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestReadConfRefusesADuplicateKey pins #636: the conf reader follows the
// profile package's one strict line rule.
func TestReadConfRefusesADuplicateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "work.conf")
	if err := os.WriteFile(path, []byte("type=sub\nemail=a\nemail=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadConf(path); err == nil || !strings.Contains(err.Error(), "duplicate key 'email'") {
		t.Fatalf("ReadConf = %v, want a duplicate-key refusal", err)
	}
}
