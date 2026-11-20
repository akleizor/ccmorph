package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestCompactUsageRefusalRetainsSources(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"conflicting turn", `{"response_id":"a","turn_id":"different"}`},
		{"different no id", `{"usage":{"cost":2}}`},
		{"literal newline", "{\n\"x\":1}"},
		{"trailing newline", "{}\n"},
		{"invalid object", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := stateHome(t)
			old := `{"response_id":"a","turn_id":"t"}`
			seedCompactRow(t, base, "2026-11-14", "a.json", old, 10)
			if _, err := CompactUsage(base, compactNow, 1); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(base, "2026-11-14.jsonl")
			before, _ := os.ReadFile(archive)
			seedCompactRow(t, base, "2026-11-14", "a.json", tc.raw, 20)
			if _, err := CompactUsage(base, compactNow, 1); err == nil {
				t.Fatal("conflicting/invalid receipt compacted")
			}
			after, _ := os.ReadFile(archive)
			data, err := os.ReadFile(filepath.Join(base, "2026-11-14", "a.json"))
			if err != nil || string(data) != tc.raw || !bytes.Equal(before, after) {
				t.Fatalf("source lost: %s %v", data, err)
			}
		})
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
			folder, err := readUsageFolder(strings.TrimSuffix(path, ".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			pending := append(append([]paths.UsageArchiveRow(nil), oldRows...), folder...)
			if err := writeUsageArchiveWithPrevious(path, pending, oldRows); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := writeUsageArchive(path, oldRows); err != nil {
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
			if err := writeUsageArchive(path, []paths.UsageArchiveRow{{Name: "a.json", MTimeNS: 10, Data: []byte(`{}`)}}); err != nil {
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
