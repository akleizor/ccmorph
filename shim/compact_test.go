package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

var compactNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func seedCompactRow(t *testing.T, base, day, name, raw string, mtime int64) {
	t.Helper()
	dir := filepath.Join(base, day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(0, mtime)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestCompactUsagePreservesBytesMetadataAndOneDayLimit(t *testing.T) {
	base := stateHome(t)
	for _, day := range []string{"2026-11-14", "2026-11-15", "2026-11-16", "2026-11-17"} {
		seedCompactRow(t, base, day, "z.json", ` {"response_id":"z","turn_id":"z","n":1.00,"text":"\\n"} `, 123456789)
		seedCompactRow(t, base, day, "a.json", `{"usage":{"cost":NaN}}`, 987654321)
	}
	if n, err := CompactUsage(base, compactNow.In(time.FixedZone("offset", 12*3600)), 1); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	rows, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl"))
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	want := []paths.UsageArchiveRow{{Name: "a.json", MTimeNS: 987654321, Data: []byte(`{"usage":{"cost":NaN}}`)}, {Name: "z.json", MTimeNS: 123456789, Data: []byte(` {"response_id":"z","turn_id":"z","n":1.00,"text":"\\n"} `)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("raw metadata changed: %#v", rows)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-14")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("folder retained: %v", err)
	}
	for _, day := range []string{"2026-11-15", "2026-11-16", "2026-11-17"} {
		if _, err := os.Stat(filepath.Join(base, day)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CompactUsage(base, compactNow, 0); err != nil || n != 1 {
		t.Fatalf("drain: %d %v", n, err)
	}
	if got := paths.DayNames(base, "2026-11-01", "2026-11-30"); !reflect.DeepEqual(got, []string{"2026-11-14", "2026-11-15", "2026-11-16", "2026-11-17"}) {
		t.Fatal(got)
	}
}

func TestCompactUsageLateMerge(t *testing.T) {
	base := stateHome(t)
	seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t","usage":{"cost":1}}`, 10)
	seedCompactRow(t, base, "2026-11-14", "b.json", `{"usage":{"cost":2}}`, 10)
	if _, err := CompactUsage(base, compactNow, 1); err != nil {
		t.Fatal(err)
	}
	updated := `{"response_id":"a","turn_id":"t","usage":{"cost":3}}`
	seedCompactRow(t, base, "2026-11-14", "a.json", updated, 20)
	seedCompactRow(t, base, "2026-11-14", "c.json", `{"usage":{"cost":4}}`, 20)
	if n, err := CompactUsage(base, compactNow, 1); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	rows, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl"))
	if err != nil || len(rows) != 3 || string(rows[0].Data) != updated || rows[1].Name != "b.json" || rows[2].Name != "c.json" {
		t.Fatalf("%v %v", rows, err)
	}
}

// The compactor compacts exactly what the readers bless: a same-name identity
// conflict stays two turns (the keep-both every reader spells with its archive
// suffix); a row no reader could count never enters the archive and leaves the
// folder for usage-dropped, its bytes kept as the record, so the day closes;
// and a multiline receipt, which readers do count, stays in the folder.
func TestCompactUsageToleratesReaderBlessedStates(t *testing.T) {
	t.Run("identity conflict keeps both", func(t *testing.T) {
		for _, tc := range []struct{ name, raw string }{
			{"conflicting turn", `{"response_id":"a","turn_id":"different"}`},
			{"different no id", `{"usage":{"cost":2}}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				base := stateHome(t)
				seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
				if _, err := CompactUsage(base, compactNow, 1); err != nil {
					t.Fatal(err)
				}
				seedCompactRow(t, base, "2026-11-14", "a.json", tc.raw, 20)
				if n, err := CompactUsage(base, compactNow, 1); err != nil || n != 1 {
					t.Fatalf("%d %v", n, err)
				}
				rows, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				old, live := false, false
				for _, row := range rows {
					if usageOwned(row.Data, "a", "t") {
						old = true
					}
					if string(row.Data) == tc.raw {
						live = true
					}
				}
				if !old || !live {
					t.Fatalf("conflicting identity lost a turn: %v", rows)
				}
				if _, err := os.Stat(filepath.Join(base, "2026-11-14")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("folder retained: %v", err)
				}
			})
		}
	})
	t.Run("uncountable row is dropped with a record", func(t *testing.T) {
		for _, raw := range []string{"", `{"response_id":"tr`, "[]", "not json"} {
			base := stateHome(t)
			seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t","usage":{"cost":1}}`, 10)
			seedCompactRow(t, base, "2026-11-14", "bad.json", raw, 20)
			if n, err := CompactUsage(base, compactNow, 1); err != nil || n != 1 {
				t.Fatalf("%q wedged the day: %d %v", raw, n, err)
			}
			rows, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl"))
			if err != nil || len(rows) != 1 || rows[0].Name != "a.json" {
				t.Fatalf("%q: %v %v", raw, rows, err)
			}
			if _, err := os.Stat(filepath.Join(base, "2026-11-14")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%q kept the closed day open: %v", raw, err)
			}
			kept := filepath.Join(filepath.Dir(base), "usage-dropped", "2026-11-14", "20-bad.json")
			if data, err := os.ReadFile(kept); err != nil || string(data) != raw {
				t.Fatalf("%q dropped without its record: %q %v", raw, data, err)
			}
			if n, err := CompactUsage(base, compactNow, 0); err != nil || n != 0 {
				t.Fatalf("settled day recounted: %d %v", n, err)
			}
		}
	})
	t.Run("multiline overlay keeps its archived twin", func(t *testing.T) {
		// A multiline rewrite of a row the archive already holds under the
		// same name is counted by readers from the folder; compaction must
		// leave both the archive generation and the live bytes exactly as
		// they were (the check TestCompactUsageRefusalRetainsSources made).
		for _, raw := range []string{"{\n\"x\":1}", "{}\n"} {
			base := stateHome(t)
			seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
			if _, err := CompactUsage(base, compactNow, 1); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(base, "2026-11-14.jsonl")
			before, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			beforeIdx, err := os.ReadFile(archive + ".idx")
			if err != nil {
				t.Fatal(err)
			}
			seedCompactRow(t, base, "2026-11-14", "a.json", raw, 20)
			if _, err := CompactUsage(base, compactNow, 1); err != nil {
				t.Fatalf("%q: %v", raw, err)
			}
			after, _ := os.ReadFile(archive)
			afterIdx, _ := os.ReadFile(archive + ".idx")
			if !bytes.Equal(before, after) || !bytes.Equal(beforeIdx, afterIdx) {
				t.Fatalf("%q rewrote the archived twin: %q -> %q", raw, before, after)
			}
			if data, err := os.ReadFile(filepath.Join(base, "2026-11-14", "a.json")); err != nil || string(data) != raw {
				t.Fatalf("%q: the live overlay was lost: %q %v", raw, data, err)
			}
		}
	})
	t.Run("multiline receipt stays in the folder", func(t *testing.T) {
		for _, raw := range []string{"{\n\"x\":1}", "{}\n"} {
			base := stateHome(t)
			seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t","usage":{"cost":1}}`, 10)
			seedCompactRow(t, base, "2026-11-14", "multi.json", raw, 20)
			if n, err := CompactUsage(base, compactNow, 1); err != nil || n != 1 {
				t.Fatalf("%q wedged the day: %d %v", raw, n, err)
			}
			if data, err := os.ReadFile(filepath.Join(base, "2026-11-14", "multi.json")); err != nil || string(data) != raw {
				t.Fatalf("a row readers count left the folder: %q %v", data, err)
			}
			if n, err := CompactUsage(base, compactNow, 0); err != nil || n != 0 {
				t.Fatalf("settled day recounted: %d %v", n, err)
			}
		}
	})
}

// One day this pass cannot rebuild is named and skipped; the days after it
// still compact. The oldest-first walk may never let one damaged day wedge
// the store forever.
func TestCompactUsageBadDayDoesNotBlockNewerDays(t *testing.T) {
	base := stateHome(t)
	seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
	if _, err := CompactUsage(base, compactNow, 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "2026-11-14.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 30)
	if err := os.WriteFile(path, append(before, "corrupt\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	seedCompactRow(t, base, "2026-11-15", "b.json", `{"response_id":"b","turn_id":"u"}`, 10)
	n, err := CompactUsage(base, compactNow, 0)
	if err == nil || !strings.Contains(err.Error(), "2026-11-14") {
		t.Fatalf("damaged day not surfaced: %v", err)
	}
	if n != 1 {
		t.Fatalf("newer day blocked by a bad day: %d (%v)", n, err)
	}
	if _, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-15.jsonl")); err != nil {
		t.Fatalf("newer day not compacted: %v", err)
	}
}

// A kill between the first publication's two renames leaves an index with no
// content; the folder is the day's whole truth, so the first sight of that
// orphan — a reader or the compactor — heals it unconditionally, even after
// later turns landed in the folder.
func TestCompactUsageKilledPublicationHealsOnNextRead(t *testing.T) {
	base := stateHome(t)
	seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
	dir := filepath.Join(base, "2026-11-14")
	folder, err := readUsageFolder(dir, CompactBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeUsageArchiveWithPrevious(filepath.Join(base, "2026-11-14.jsonl"), folder, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(base, "2026-11-14.jsonl")); err != nil {
		t.Fatal(err)
	}
	seedCompactRow(t, base, "2026-11-14", "b.json", `{"response_id":"b","turn_id":"u"}`, 20)
	rows, err := readUsageDay(dir)
	if err != nil || len(rows) != 2 {
		t.Fatalf("orphan index bricked a reader: %v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-14.jsonl.idx")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan index survived first sight: %v", err)
	}
	if n, err := CompactUsage(base, compactNow, 1); err != nil || n != 1 {
		t.Fatalf("healed day did not compact: %d %v", n, err)
	}
	archived, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl"))
	if err != nil || len(archived) != 2 {
		t.Fatalf("recovery %v %v", archived, err)
	}
}

func TestCompactUsageFailedVerificationRetainsFolder(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "replacement"}[existing], func(t *testing.T) {
			base := stateHome(t)
			seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t","usage":{"cost":1}}`, 10)
			if existing {
				if _, err := CompactUsage(base, compactNow, 1); err != nil {
					t.Fatal(err)
				}
			}
			seedCompactRow(t, base, "2026-11-14", "b.json", `{"response_id":"b","turn_id":"u","usage":{"cost":2}}`, 20)
			old := compactPublished
			compactPublished = func(path string) {
				if err := os.WriteFile(path, []byte("corrupt\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { compactPublished = old })
			if _, err := CompactUsage(base, compactNow, 1); err == nil {
				t.Fatal("corruption passed verification")
			}
			if _, err := os.Stat(filepath.Join(base, "2026-11-14", "b.json")); err != nil {
				t.Fatal("source removed", err)
			}
			rows, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl"))
			if err != nil || (existing && (len(rows) != 1 || rows[0].Name != "a.json")) || (!existing && rows != nil) {
				t.Fatalf("rollback %v %v", rows, err)
			}
		})
	}
}

func TestCompactUsageInterruptedPublication(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "replacement"}[existing], func(t *testing.T) {
			base := stateHome(t)
			seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
			path := filepath.Join(base, "2026-11-14.jsonl")
			var oldRows []paths.UsageArchiveRow
			if existing {
				if _, err := CompactUsage(base, compactNow, 1); err != nil {
					t.Fatal(err)
				}
				oldRows, _ = paths.ReadUsageArchive(path)
			}
			seedCompactRow(t, base, "2026-11-14", "b.json", `{"response_id":"b","turn_id":"u"}`, 20)
			folder, err := readUsageFolder(strings.TrimSuffix(path, ".jsonl"), CompactBudget{})
			if err != nil {
				t.Fatal(err)
			}
			pending := append(append([]paths.UsageArchiveRow(nil), oldRows...), folder...)
			if err := writeUsageArchiveWithPrevious(path, pending, oldRows); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := writeUsageArchiveWithPrevious(path, oldRows, nil); err != nil {
					t.Fatal(err)
				}
				// Restore pending metadata only, simulating a kill between the two renames.
				oldData, _ := os.ReadFile(path)
				if err := writeUsageArchiveWithPrevious(path, pending, oldRows); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, oldData, 0o600); err != nil {
					t.Fatal(err)
				}
				if rows, err := paths.ReadUsageArchive(path); err != nil || !reflect.DeepEqual(rows, oldRows) {
					t.Fatalf("previous unreadable: %v %v", rows, err)
				}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := CompactUsage(base, compactNow, 1); err != nil {
				t.Fatal(err)
			}
			rows, err := paths.ReadUsageArchive(path)
			if err != nil || len(rows) != 2 {
				t.Fatalf("recovery %v %v", rows, err)
			}
		})
	}
}

func TestCompactUsageLockAndLateWriter(t *testing.T) {
	base := stateHome(t)
	day := time.Now().UTC().AddDate(0, 0, -3).Format(paths.DayLayout)
	u := newSidecar("sub", "transcript")
	r := turn(u, "late", 1)
	r.timestamp = day + "T10:00:00+00:00"
	if err := u.write(r); err != nil {
		t.Fatal(err)
	}
	lock, err := paths.UsageStoreLock(base, false)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := CompactUsage(base, time.Now(), 1); err != nil || n != 0 {
		t.Fatalf("lock ignored: %d %v", n, err)
	}
	lock.Close()
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, day+".jsonl")
	before, _ := os.ReadFile(path)
	r.usage = usageRow{{"cost", []byte("5")}}
	if err := u.write(r); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("writer modified JSONL")
	}
	rows, err := readUsageDay(filepath.Join(base, day))
	if err != nil || len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"cost":5`) {
		t.Fatalf("late overlay %v %v", rows, err)
	}
}

func TestReportMixedArchiveKeepsDistinctNoIDRows(t *testing.T) {
	base := stateHome(t)
	day := time.Now().UTC().AddDate(0, 0, -3).Format(paths.DayLayout)
	raw := `{"timestamp":"` + day + `T10:00:00+00:00","usage":{"cost":7}}`
	for _, name := range []string{"a.json", "b.json"} {
		seedCompactRow(t, base, day, name, raw, 10)
	}
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	seedCompactRow(t, base, day, "a.json", raw, 20) // A verified copy whose unlink was interrupted.
	seedCompactRow(t, base, day, "c.json", raw, 20) // Equal bytes under a distinct original name stay distinct.
	if out := reportOutput(t, 7); !strings.Contains(out, "total spend $21.00") {
		t.Fatal(out)
	}
}

func TestCompactUsageKeepsHardlinkedLegacyRowCount(t *testing.T) {
	base := stateHome(t)
	stamp := time.Now().UTC().AddDate(0, 0, -3)
	day := stamp.Format(paths.DayLayout)
	raw := `{"timestamp":"` + day + `T10:00:00Z","usage":{"cost":7}}`
	seedCompactRow(t, base, day, "a.json", raw, stamp.UnixNano())
	if err := os.Link(filepath.Join(base, day, "a.json"), filepath.Join(base, "a.json")); err != nil {
		t.Fatal(err)
	}
	if n, err := CompactUsage(base, time.Now(), 1); err != nil || n != 0 {
		t.Fatalf("linked day should stay live: %d %v", n, err)
	}
	if out := reportOutput(t, 7); !strings.Contains(out, "total spend $7.00") {
		t.Fatalf("one legacy receipt counted twice: %s", out)
	}
	// The report finishes the interrupted move before a later compaction.
	if n, err := CompactUsage(base, time.Now(), 1); err != nil || n != 1 {
		t.Fatalf("settled move did not compact: %d %v", n, err)
	}
	if out := reportOutput(t, 7); !strings.Contains(out, "total spend $7.00") {
		t.Fatalf("compaction changed the settled receipt count: %s", out)
	}
}

func TestMoveFlatRowsKeepsDistinctTwinBesideArchive(t *testing.T) {
	base := stateHome(t)
	stamp := time.Now().UTC().AddDate(0, 0, -3)
	day := stamp.Format(paths.DayLayout)
	raw := `{"timestamp":"` + day + `T10:00:00Z","usage":{"cost":7}}`
	seedCompactRow(t, base, day, "a.json", raw, stamp.UnixNano())
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(base, "a.json")
	if err := os.WriteFile(flat, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(flat, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	moveFlatRows()
	if data, err := os.ReadFile(flat); err != nil || string(data) != raw {
		t.Fatalf("distinct flat receipt removed: %s %v", data, err)
	}
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	if out := reportOutput(t, 7); !strings.Contains(out, "total spend $14.00") {
		t.Fatalf("distinct no-id receipts collapsed: %s", out)
	}
}

// A damaged archive in a scanned neighbor is recorded once and treated as no
// redirect: the cursor pins only on this row's own write or lock failure. One
// unreadable archived day may never stop usage recording forever.
func TestTranscriptRecordIgnoresForeignArchiveDamage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files with mode 0")
	}
	base := stateHome(t)
	day := time.Now().UTC().AddDate(0, 0, -3).Format(paths.DayLayout)
	stamp := day + "T10:00:00+00:00"
	seedCompactRow(t, base, day, rowFileName("archived"),
		`{"response_id":"archived","turn_id":"t","timestamp":"`+stamp+`","usage":{"cost":1},"complete":true}`, 10)
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, day+".jsonl")
	if err := os.Chmod(archive, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(archive, 0o600) })
	path := writeTranscript(t, transcriptLine("u1", "resp_new", "claude-opus-5-5", stamp,
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("sub", "", path); n != 1 {
		t.Fatalf("a foreign unreadable archive pinned the transcript cursor: %d rows", n)
	}
	if n := RecordTranscript("sub", "", path); n != 0 {
		t.Fatalf("second pass reread a passed response: %d rows", n)
	}
}

// A receipt still there that this run cannot read — mode 0, or a FIFO left
// under a receipt's name — is skipped and named, and the report goes on with
// the total of every other row, the reading every build before #537 gave: quota
// and the export skip and name the same receipt and give the same total, so
// the three readers agree (#654). The FIFO is refused, never waited on. report
// can exit through die, so it runs in a child of this test binary.
func TestReportNamesAnUnreadableRowFile(t *testing.T) {
	if os.Getenv("CCMORPH_TEST_REPORT_CHILD") == "1" {
		report(7)
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads files with mode 0")
	}
	base := stateHome(t)
	day := time.Now().UTC().Format(paths.DayLayout)
	stamp := day + "T00:00:00+00:00"
	seedCompactRow(t, base, day, "ok.json", `{"timestamp":"`+stamp+`","usage":{"cost":7}}`, 10)
	seedCompactRow(t, base, day, "locked.json", `{"timestamp":"`+stamp+`","usage":{"cost":9}}`, 10)
	path := filepath.Join(base, day, "locked.json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	if err := syscall.Mkfifo(filepath.Join(base, day, "unusable.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReportNamesAnUnreadableRowFile$")
	cmd.Env = append(os.Environ(), "CCMORPH_TEST_REPORT_CHILD=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the report waited on a FIFO under a receipt's name")
	}
	if err != nil {
		t.Fatalf("an unreadable receipt stopped the report: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"locked.json: permission denied", "unusable.json: not a regular file"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("missing %q: %s", want, stderr.String())
		}
	}
	for _, want := range []string{"total spend $7.00", "not readable 2 receipt(s), skipped"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %q: %s", want, stdout.String())
		}
	}
}

// A FIFO under a receipt's name in a closed day never holds the compactor —
// nor the exclusive store lock a quota read's own maintenance takes — on its
// open: the day is named and passed over, and the next day still compacts
// (INTENT 8, #654).
func TestCompactUsageNeverWaitsOnAFIFO(t *testing.T) {
	base := stateHome(t)
	seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
	if err := syscall.Mkfifo(filepath.Join(base, "2026-11-14", "unusable.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedCompactRow(t, base, "2026-11-15", "b.json", `{"response_id":"b","turn_id":"t"}`, 10)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := CompactUsageWithin(base, compactNow, 0, CompactBudget{Deadline: time.Now().Add(time.Second), MaxFiles: 2000, MaxBytes: 2 << 20})
		done <- result{n, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the compactor waited on a FIFO under a receipt's name")
	}
	if got.n != 1 || got.err == nil || !strings.Contains(got.err.Error(), "not a regular file") {
		t.Fatalf("%d %v", got.n, got.err)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-14.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a day holding a FIFO was compacted: %v", err)
	}
	if _, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-15.jsonl")); err != nil {
		t.Fatalf("the day after it did not compact: %v", err)
	}
}

func TestUsageStoreReadLockRefusesExclusiveHolder(t *testing.T) {
	base := stateHome(t)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := paths.UsageStoreLock(base, true)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if lock, err := paths.TryUsageStoreReadLock(base); err == nil || lock != nil {
		t.Fatalf("exclusive holder did not refuse reader: %v %v", lock, err)
	}
	held.Close()
	lock, err := paths.TryUsageStoreReadLock(base)
	if err != nil || lock == nil {
		t.Fatalf("released store unreadable: %v %v", lock, err)
	}
	lock.Close()
}

func TestUsageCompactMain(t *testing.T) {
	stateHome(t)
	var out, err bytes.Buffer
	if code := UsageCompactMain([]string{"extra"}, &out, &err); code != 2 || !strings.Contains(err.String(), "usage:") {
		t.Fatalf("%d %s", code, err.String())
	}
	out.Reset()
	err.Reset()
	if code := UsageCompactMain(nil, &out, &err); code != 0 || out.String() != "compacted 0 usage day(s)\n" {
		t.Fatalf("%d %s %s", code, out.String(), err.String())
	}
}

func TestArchivedTranscriptUpdateKeepsOriginalDayAndSpill(t *testing.T) {
	base := stateHome(t)
	day := time.Now().UTC().AddDate(0, 0, -3).Format(paths.DayLayout)
	stamp := day + "T23:59:59+00:00"
	responseID, turnID := "recycled", "same-turn"
	relay := newSidecar("relay", "adapter")
	primary := turn(relay, responseID, 1)
	primary.timestamp = stamp
	if err := relay.write(primary); err != nil {
		t.Fatal(err)
	}
	if ok, err := writeUsageRow("sub", "", responseID, "m", stamp, "", turnID, usageRow{{"input_tokens", []byte("2")}, {"output_tokens", []byte("40")}}); !ok || err != nil {
		t.Fatalf("initial %v %v", ok, err)
	}
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, day+".jsonl")
	before, _ := os.ReadFile(path)
	nextDay := time.Now().UTC().AddDate(0, 0, -2).Format(paths.DayLayout)
	if ok, err := writeUsageRow("sub", "", responseID, "m", nextDay+"T00:00:01+00:00", "", turnID, usageRow{{"input_tokens", []byte("2")}, {"output_tokens", []byte("5")}}); !ok || err != nil {
		t.Fatalf("update %v %v", ok, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("transcript writer rewrote archive")
	}
	rows, err := readUsageDay(filepath.Join(base, day))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v %v", rows, err)
	}
	found := false
	for _, row := range rows {
		if usageOwned(row.Data, responseID, turnID) {
			found = true
			if !strings.Contains(string(row.Data), `"output_tokens":40`) || row.Name == rowFileName(responseID) {
				t.Fatalf("spill shrank or moved %s", row.Data)
			}
		}
	}
	if !found {
		t.Fatal("archived transcript row lost")
	}
	if got := dayRowFiles(base, time.Time{}, time.Now()); len(got) != 1 || !strings.Contains(got[0], day) {
		t.Fatalf("cross-day duplicate %v", got)
	}
}

func TestArchiveReaderRejectsBadMetadata(t *testing.T) {
	for _, field := range []string{"sha256", "files", "v"} {
		t.Run(field, func(t *testing.T) {
			base := stateHome(t)
			if err := os.MkdirAll(base, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, "2026-11-14.jsonl")
			if err := writeUsageArchiveWithPrevious(path, []paths.UsageArchiveRow{{Name: "a.json", MTimeNS: 10, Data: []byte(`{}`)}}, nil); err != nil {
				t.Fatal(err)
			}
			meta, _ := os.ReadFile(path + ".idx")
			var idx map[string]any
			json.Unmarshal(meta, &idx)
			delete(idx, field)
			meta, _ = json.Marshal(idx)
			os.WriteFile(path+".idx", meta, 0o600)
			if rows, err := paths.ReadUsageArchive(path); err == nil || rows != nil {
				t.Fatalf("partial accepted %v %v", rows, err)
			}
		})
	}
}

// One merge rule, three readers (#654): a live row whose cost Python wrote as
// the NaN token is the same turn as its archived priced sibling. The newer
// row wins and the turn is unknown spend — never the archived row's dollars
// beside a phantom, which is what a reader that decoded the pair with
// encoding/json (which refuses the token) saw instead.
func TestReportNonFiniteOverlayCountsUnknownNotDollars(t *testing.T) {
	base := stateHome(t)
	day := time.Now().UTC().AddDate(0, 0, -3).Format(paths.DayLayout)
	seedCompactRow(t, base, day, "a.json",
		`{"response_id":"r","turn_id":"t","timestamp":"`+day+`T10:00:00Z","usage":{"cost":7,"input_tokens":10},"complete":true}`, 10)
	if _, err := CompactUsage(base, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	seedCompactRow(t, base, day, "a.json",
		`{"response_id":"r","turn_id":"t","timestamp":"`+day+`T10:00:00Z","usage":{"cost":NaN,"input_tokens":10},"complete":true}`, 20)
	if out := reportOutput(t, 7); !strings.Contains(out, "total spend $0.00") {
		t.Fatalf("a damaged price reached the total: %s", out)
	}
}

// A hot path's compaction is held to its budget (INTENT 8, #654): a day over
// the size cap is left whole for usage-compact while a smaller one still
// compacts, and no step starts once the deadline has passed.
func TestCompactUsageWithinLeavesADayOverTheCap(t *testing.T) {
	base := stateHome(t)
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		seedCompactRow(t, base, "2026-11-14", name, `{"response_id":"`+name+`","turn_id":"t"}`, 10)
	}
	seedCompactRow(t, base, "2026-11-15", "d.json", `{"response_id":"d","turn_id":"t"}`, 10)
	if n, err := CompactUsageWithin(base, compactNow, 0, CompactBudget{MaxFiles: 2}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-14.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a day over the cap was compacted: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(base, "2026-11-14")); err != nil || len(entries) != 3 {
		t.Fatalf("a day over the cap was touched: %v %v", entries, err)
	}
	if _, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-15.jsonl")); err != nil {
		t.Fatalf("the day under the cap did not compact: %v", err)
	}
	seedCompactRow(t, base, "2026-11-15", "e.json", `{"response_id":"e","turn_id":"t"}`, 20)
	if n, err := CompactUsageWithin(base, compactNow, 0, CompactBudget{MaxFiles: 2, MaxBytes: 1}); err != nil || n != 0 {
		t.Fatalf("archive cap: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-15", "e.json")); err != nil {
		t.Fatalf("a day whose archive is over the cap was rewritten: %v", err)
	}
	if n, err := CompactUsage(base, compactNow, 0); err != nil || n != 2 {
		t.Fatalf("usage-compact must still drain both: %d %v", n, err)
	}
}

func TestCompactUsageWithinStopsAtItsDeadline(t *testing.T) {
	t.Run("spent before the walk", func(t *testing.T) {
		base := stateHome(t)
		seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
		if n, err := CompactUsageWithin(base, compactNow, 0, CompactBudget{Deadline: time.Now().Add(-time.Second)}); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(base, "2026-11-14.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("published after the deadline: %v", err)
		}
	})
	t.Run("spent during the publication", func(t *testing.T) {
		base := stateHome(t)
		seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
		seedCompactRow(t, base, "2026-11-14", "b.json", `{"response_id":"b","turn_id":"u"}`, 10)
		deadline := time.Now().Add(time.Second)
		old := compactPublished
		compactPublished = func(string) { time.Sleep(time.Until(deadline) + 10*time.Millisecond) }
		t.Cleanup(func() { compactPublished = old })
		n, err := CompactUsageWithin(base, compactNow, 0, CompactBudget{Deadline: deadline})
		if err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
		if entries, err := os.ReadDir(filepath.Join(base, "2026-11-14")); err != nil || len(entries) != 2 {
			t.Fatalf("removed a row after the deadline: %v %v", entries, err)
		}
		if rows, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-14.jsonl")); err != nil || len(rows) != 2 {
			t.Fatalf("the verified publication was lost: %v %v", rows, err)
		}
		compactPublished = old
		if n, err := CompactUsage(base, compactNow, 0); err != nil || n != 1 {
			t.Fatalf("the next pass did not finish the day: %d %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(base, "2026-11-14")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("folder retained: %v", err)
		}
	})
}

// A hot path's budget bounds the bytes a day costs, not only its entry count
// (INTENT 8, #654): a day of few but large receipts is left whole for
// usage-compact, a smaller day still compacts, and usage-compact drains both.
func TestCompactUsageWithinLeavesADayOverItsByteCap(t *testing.T) {
	base := stateHome(t)
	pad := strings.Repeat("x", 100)
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		seedCompactRow(t, base, "2026-11-14", name, `{"response_id":"`+name+`","turn_id":"t","pad":"`+pad+`"}`, 10)
	}
	seedCompactRow(t, base, "2026-11-15", "d.json", `{"response_id":"d","turn_id":"t"}`, 10)
	if n, err := CompactUsageWithin(base, compactNow, 0, CompactBudget{MaxFiles: 2000, MaxBytes: 300}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-14.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a day over the byte cap was compacted: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(base, "2026-11-14")); err != nil || len(entries) != 3 {
		t.Fatalf("a day over the byte cap was touched: %v %v", entries, err)
	}
	if _, err := paths.ReadUsageArchive(filepath.Join(base, "2026-11-15.jsonl")); err != nil {
		t.Fatalf("the day under the byte cap did not compact: %v", err)
	}
	if n, err := CompactUsage(base, compactNow, 0); err != nil || n != 1 {
		t.Fatalf("usage-compact must still drain it: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-11-14")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("folder retained: %v", err)
	}
}

// Under a cap, readUsageFolder never reads past it: a receipt whose stat alone
// is over what the cap has left is not read at all, and a spent deadline reads
// nothing. The zero budget, usage-compact's, still reads every receipt.
func TestReadUsageFolderReadsNoMoreThanItsBudget(t *testing.T) {
	base := stateHome(t)
	seedCompactRow(t, base, "2026-11-14", "a.json", `{"response_id":"a","turn_id":"t"}`, 10)
	dir := filepath.Join(base, "2026-11-14")
	big, err := os.Create(filepath.Join(dir, "big.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: no disk space, but 256 MiB for any read that does not stop.
	if err := big.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	if err := big.Close(); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rows, err := readUsageFolder(dir, CompactBudget{MaxBytes: 2 << 20})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errOverBudget) || rows != nil {
		t.Fatalf("a receipt over the cap was read: %d rows, %v", len(rows), err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Fatalf("a read under a 2 MiB cap allocated %d bytes", grew)
	}
	if rows, err := readUsageFolder(dir, CompactBudget{Deadline: time.Now().Add(-time.Second)}); !errors.Is(err, errOverBudget) || rows != nil {
		t.Fatalf("a spent deadline still read: %d rows, %v", len(rows), err)
	}
	if err := os.Remove(filepath.Join(dir, "big.json")); err != nil {
		t.Fatal(err)
	}
	if rows, err := readUsageFolder(dir, CompactBudget{}); err != nil || len(rows) != 1 {
		t.Fatalf("the zero budget must read every receipt: %v %v", rows, err)
	}
}
