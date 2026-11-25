package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/akleizor/ccmorph/util/paths"
)

// BenchmarkRecordTranscript is one Stop pass over 50 new responses against a
// store the size of a busy machine's: 20,000 rows in each of three days, in
// the live-folder shape and in the compacted archive shape. archive-loads/op
// is the #559 contract: a settled generation is loaded from disk once, and
// every lookup after that is a stat, not a byte (#654).
func BenchmarkRecordTranscript(b *testing.B) {
	for _, form := range []string{"folders", "archives"} {
		b.Run(form, func(b *testing.B) {
			settle := paths.ArchiveSettle
			paths.ArchiveSettle = 0 // a fixture written now is settled for the benchmark
			b.Cleanup(func() { paths.ArchiveSettle = settle })
			var loads int64
			loaded := paths.UsageArchiveLoaded
			paths.UsageArchiveLoaded = func(string) { atomic.AddInt64(&loads, 1) }
			b.Cleanup(func() { paths.UsageArchiveLoaded = loaded })

			dir := b.TempDir()
			b.Setenv("XDG_STATE_HOME", dir)
			usage := filepath.Join(dir, "ccmorph", "usage")
			for _, day := range []string{"2026-11-04", "2026-11-05", "2026-11-06"} {
				d := filepath.Join(usage, day)
				if form == "archives" {
					rows := make([]paths.UsageArchiveRow, 0, 20000)
					for i := 0; i < 20000; i++ {
						rows = append(rows, paths.UsageArchiveRow{
							Name:    rowFileName(fmt.Sprintf("old_%s_%d", day, i)),
							MTimeNS: int64(i), Data: []byte(`{"schema":1}`)})
					}
					sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
					if err := os.MkdirAll(usage, 0o700); err != nil {
						b.Fatal(err)
					}
					if err := writeUsageArchiveWithPrevious(d+".jsonl", rows, nil); err != nil {
						b.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(d, 0o700); err != nil {
						b.Fatal(err)
					}
					for i := 0; i < 20000; i++ {
						if err := os.WriteFile(filepath.Join(d, rowFileName(fmt.Sprintf("old_%s_%d", day, i))), []byte(`{"schema":1}`), 0o600); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
			// A seeded cache, never the operator's HOME: the measurement must be
			// the same on every machine (INTENT.md: suites never touch it).
			seedOwnedPrices(b, usage, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
			pass := func(n int) string {
				var lines []string
				for i := 0; i < 50; i++ {
					lines = append(lines, transcriptLine(fmt.Sprintf("u%d", i), fmt.Sprintf("msg_%d_%d", n, i), "claude-opus-5-5",
						"2026-11-05T10:00:00Z", `{"input_tokens":10,"output_tokens":25}`))
				}
				p := writeTranscript(b, lines...)
				if got := RecordTranscript("sub", "anthropic", p); got != 50 {
					b.Fatalf("wrote %d", got)
				}
				return p
			}
			pass(-1) // settle the archives in the memo outside the timed region
			atomic.StoreInt64(&loads, 0)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				b.StopTimer()
				var lines []string
				for i := 0; i < 50; i++ {
					lines = append(lines, transcriptLine(fmt.Sprintf("u%d", i), fmt.Sprintf("msg_%d_%d", n, i), "claude-opus-5-5",
						"2026-11-05T10:00:00Z", `{"input_tokens":10,"output_tokens":25}`))
				}
				p := writeTranscript(b, lines...)
				b.StartTimer()
				if got := RecordTranscript("sub", "anthropic", p); got != 50 {
					b.Fatalf("wrote %d", got)
				}
			}
			b.ReportMetric(float64(atomic.LoadInt64(&loads))/float64(b.N), "archive-loads/op")
		})
	}
}
