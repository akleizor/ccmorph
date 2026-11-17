package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkRecordTranscript is one Stop pass over 50 new responses against a
// store the size of a busy machine's: 20,000 rows in each of three day folders.
func BenchmarkRecordTranscript(b *testing.B) {
	t := &testing.T{}
	dir := b.TempDir()
	b.Setenv("XDG_STATE_HOME", dir)
	usage := filepath.Join(dir, "ccmorph", "usage")
	for _, day := range []string{"2026-11-04", "2026-11-05", "2026-11-06"} {
		d := filepath.Join(usage, day)
		os.MkdirAll(d, 0o700)
		for i := 0; i < 20000; i++ {
			os.WriteFile(filepath.Join(d, rowFileName(fmt.Sprintf("old_%s_%d", day, i))), []byte(`{"schema":1}`), 0o600)
		}
	}
	// The real models.dev slice when this machine has one, so a price read costs what it costs.
	if real, err := os.ReadFile(os.ExpandEnv("$HOME/.local/state/ccmorph/prices-owned-anthropic.json")); err == nil {
		os.WriteFile(filepath.Join(dir, "ccmorph", "prices-owned-anthropic.json"), real, 0o600)
	} else {
		seedOwnedPrices(t, usage, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		var lines []string
		for i := 0; i < 50; i++ {
			lines = append(lines, transcriptLine(fmt.Sprintf("u%d", i), fmt.Sprintf("msg_%d_%d", n, i), "claude-opus-5-5",
				"2026-11-05T10:00:00Z", `{"input_tokens":10,"output_tokens":25}`))
		}
		p := filepath.Join(b.TempDir(), "t.jsonl")
		os.WriteFile(p, []byte(joinLines(lines)), 0o600)
		dirNames = map[string][]string{}
		b.StartTimer()
		if got := RecordTranscript("sub", "anthropic", p); got != 50 {
			b.Fatalf("wrote %d", got)
		}
	}
}

func joinLines(ls []string) string {
	s := ""
	for _, l := range ls {
		s += l
	}
	return s
}
