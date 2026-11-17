package quota

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/shim"
	"github.com/akleizor/ccmorph/util/paths"
)

func writeSpendFixture(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSpendArchive(t *testing.T, usage, day string, files map[string]string) string {
	t.Helper()
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var data bytes.Buffer
	index := struct {
		V      int    `json:"v"`
		SHA256 string `json:"sha256"`
		Files  []struct {
			Name    string `json:"name"`
			MTimeNS int64  `json:"mtime_ns"`
		} `json:"files"`
	}{V: 1}
	for _, name := range names {
		data.WriteString(files[name])
		data.WriteByte('\n')
		index.Files = append(index.Files, struct {
			Name    string `json:"name"`
			MTimeNS int64  `json:"mtime_ns"`
		}{name, now().UnixNano()})
	}
	sum := sha256.Sum256(data.Bytes())
	index.SHA256 = hex.EncodeToString(sum[:])
	idx, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(usage, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(usage, day+".jsonl")
	for p, raw := range map[string][]byte{path: data.Bytes(), path + ".idx": idx} {
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func spendArchiveReceipt(id, turn, stamp string, cost, tokens int) string {
	return fmt.Sprintf(`{"response_id":%q,"turn_id":%q,"profile":"gw","timestamp":%q,"usage":{"cost":%d,"input_tokens":%d},"complete":true}`,
		id, turn, stamp, cost, tokens)
}

func TestQuotaSpendArchiveMixedStoreTotals(t *testing.T) {
	for _, form := range []string{"folder", "compacted", "archive", "mixed"} {
		t.Run(form, func(t *testing.T) {
			useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			usage := filepath.Join(state, "ccmorph", "usage")
			day := "2026-10-18"
			stamp := day + "T09:00:00Z"
			files := map[string]string{
				"a.json": spendArchiveReceipt("a", "turn_a", stamp, 4, 40),
				"b.json": spendArchiveReceipt("", "turn_b", stamp, 2, 20),
				"c.json": spendArchiveReceipt("", "turn_c", stamp, 3, 30),
			}
			if form == "folder" || form == "compacted" {
				for name, raw := range files {
					writeSpendFixture(t, filepath.Join(usage, day, name), raw)
				}
			} else {
				if form == "mixed" {
					files["a.json"] = spendArchiveReceipt("a", "turn_a", stamp, 1, 10)
					writeSpendFixture(t, filepath.Join(usage, day, "a.json"), spendArchiveReceipt("a", "turn_a", stamp, 4, 40))
					newer := now().Add(time.Hour)
					if err := os.Chtimes(filepath.Join(usage, day, "a.json"), newer, newer); err != nil {
						t.Fatal(err)
					}
				}
				writeSpendArchive(t, usage, day, files)
			}
			writeSpendFixture(t, filepath.Join(usage, "b.json"), files["b.json"])
			writeSpendFixture(t, filepath.Join(usage, "flat.json"), spendArchiveReceipt("flat", "turn_flat", stamp, 8, 80))
			writeSpendFixture(t, filepath.Join(usage, "2026-10-20", "today.json"), spendArchiveReceipt("today", "turn_today", "2026-10-20T09:00:00Z", 16, 160))
			writeSpendFixture(t, filepath.Join(usage, "2026-10-20", "unpriced.json"), `{"response_id":"unpriced","profile":"gw","timestamp":"2026-10-20T10:00:00Z","usage":{"output_tokens":5},"complete":true}`)
			if form == "compacted" {
				if n, err := shim.CompactUsage(usage, now(), 0); err != nil || n != 1 {
					t.Fatalf("compact closed fixture: %d days, %v", n, err)
				}
				if _, err := os.Stat(filepath.Join(usage, day)); !os.IsNotExist(err) {
					t.Fatalf("compacted day must be indexed from jsonl, not its folder: %v", err)
				}
			}
			e := spendEntry("gw", "")
			want := Spend{MonthUSD: 33, TodayUSD: 16, MonthTurns: 6, TodayTurns: 2, MonthTokens: 335, TodayTokens: 165, UnpricedTurns: 1}
			if e.Error != "" || e.Spend == nil || *e.Spend != want {
				t.Fatalf("%s store totals: %+v, want %+v (error %q)", form, e.Spend, want, e.Error)
			}
		})
	}
}

func TestQuotaSpendAutomaticallyCompactsOneClosedDay(t *testing.T) {
	useClock(t, time.Date(2026, 10, 21, 0, 30, 0, 0, time.FixedZone("east", 8*60*60)))
	compactUsage = shim.CompactUsage
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	for _, day := range []string{"2026-10-16", "2026-10-17", "2026-10-18", "2026-10-19", "2026-10-20"} {
		writeSpendFixture(t, filepath.Join(usage, day, "a.json"), spendArchiveReceipt(day, "turn", day+"T09:00:00Z", 1, 10))
	}
	for call := 1; call <= 4; call++ {
		tallyMemo.key = ""
		out, _, code := run(t, nil, "gw\tgateway\t\t\nother\tgateway\t\t\n")
		if code != 0 || !strings.Contains(out, "month $5.00") {
			t.Fatalf("call %d lost receipts after maintenance (exit %d): %s", call, code, out)
		}
		if other := spendEntry("other", ""); other.Error != "" || other.Spend == nil {
			t.Fatalf("second profile read failed: %+v", other)
		}
		archives, err := filepath.Glob(filepath.Join(usage, "*.jsonl"))
		want := min(call, 3)
		if err != nil || len(archives) != want {
			t.Fatalf("call %d archived %d days, want %d: %v", call, len(archives), want, err)
		}
		for _, day := range []string{"2026-10-19", "2026-10-20"} {
			if _, err := os.Stat(filepath.Join(usage, day, "a.json")); err != nil {
				t.Fatalf("call %d compacted yesterday or today (%s): %v", call, day, err)
			}
		}
	}
}

func TestQuotaSpendArchiveRefusalKeepsReadableRows(t *testing.T) {
	for _, collision := range []struct {
		name, archivedID, liveID, archivedTurn, liveTurn string
	}{
		{"same-id-different-turn", "a", "a", "old", "new"},
		{"different-id", "a", "b", "turn", "turn"},
		{"no-id", "", "", "turn", "turn"},
	} {
		t.Run(collision.name, func(t *testing.T) {
			useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
			compactUsage = shim.CompactUsage
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			usage := filepath.Join(state, "ccmorph", "usage")
			day := "2026-10-18"
			path := writeSpendArchive(t, usage, day, map[string]string{
				"a.json": spendArchiveReceipt(collision.archivedID, collision.archivedTurn, day+"T09:00:00Z", 1, 10),
			})
			writeSpendFixture(t, filepath.Join(usage, day, "a.json"), spendArchiveReceipt(collision.liveID, collision.liveTurn, day+"T09:00:00Z", 2, 20))
			e := spendEntry("gw", "")
			if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 3 || e.Spend.MonthTurns != 2 || e.Spend.MonthTokens != 30 {
				t.Fatalf("compaction refusal dropped distinct readable receipts: %+v", e)
			}
			if _, err := os.Stat(filepath.Join(usage, day, "a.json")); err != nil {
				t.Fatalf("conflicting live receipt removed: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("conflicting archive removed: %v", err)
			}
		})
	}
}

func TestQuotaSpendArchiveInvalidFailsEveryCache(t *testing.T) {
	for _, invalid := range []string{"hash", "count", "missing-index", "index-only"} {
		for _, cache := range []string{"process", "month", "base", "day", "cold"} {
			t.Run(invalid+"/"+cache, func(t *testing.T) {
				useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
				state := t.TempDir()
				t.Setenv("XDG_STATE_HOME", state)
				usage := filepath.Join(state, "ccmorph", "usage")
				path := writeSpendArchive(t, usage, "2026-10-18", map[string]string{"a.json": spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10)})
				if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
					t.Fatalf("warm archived read: %+v", e)
				}
				switch cache {
				case "month":
					tallyMemo.key = ""
				case "base":
					tallyMemo.key = ""
					os.Remove(monthMemoPath())
				case "day":
					tallyMemo.key = ""
					os.Remove(monthMemoPath())
					os.Remove(monthBasePath())
				case "cold":
					tallyMemo.key = ""
					os.RemoveAll(spendCacheDir())
				}
				switch invalid {
				case "hash":
					writeSpendFixture(t, path, spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 9, 90)+"\n")
				case "count":
					meta, err := os.ReadFile(path + ".idx")
					if err != nil {
						t.Fatal(err)
					}
					var index map[string]any
					if err := json.Unmarshal(meta, &index); err != nil {
						t.Fatal(err)
					}
					index["files"] = []any{}
					writeAtomic(path+".idx", index)
				case "missing-index":
					if err := os.Remove(path + ".idx"); err != nil {
						t.Fatal(err)
					}
				case "index-only":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				e := spendEntry("gw", "")
				if e.Error == "" || e.Spend != nil {
					t.Fatalf("invalid archive answered as readable under %s cache: %+v", cache, e)
				}
			})
		}
	}
}

func TestQuotaSpendArchiveBusyCompactorDoesNotFailRead(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "a.json"), spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10))
	lock, err := paths.UsageStoreLock(usage, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	compactUsage = func(base string, at time.Time, limit int) (int, error) {
		n, err := shim.CompactUsage(base, at, limit)
		if n != 0 || err != nil {
			t.Fatalf("busy maintenance changed store: %d, %v", n, err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		return n, err
	}
	e := spendEntry("gw", "")
	if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
		t.Fatalf("busy compaction failed a readable store: %+v", e)
	}
}

func TestQuotaSpendArchiveRecreatedFolderInvalidatesCaches(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	day := "2026-10-18"
	dir := filepath.Join(usage, day)
	stamp := day + "T09:00:00Z"
	writeSpendArchive(t, usage, day, map[string]string{"a.json": spendArchiveReceipt("a", "turn", stamp, 1, 10)})
	path := filepath.Join(dir, "a.json")
	writeSpendFixture(t, path, spendArchiveReceipt("a", "turn", stamp, 2, 20))
	mtime := now().Add(time.Hour)
	for _, p := range []string{path, dir} {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 2 {
		t.Fatalf("warm mixed read: %+v", e)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	writeSpendFixture(t, path, spendArchiveReceipt("a", "turn", stamp, 9, 90))
	for _, p := range []string{path, dir} {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 9 || e.Spend.MonthTokens != 90 {
		t.Fatalf("recreated folder with unchanged mtimes served stale totals: %+v", e)
	}
}

func TestQuotaSpendArchiveNewestNamedReceipt(t *testing.T) {
	for _, liveAge := range []time.Duration{-time.Hour, 0, time.Hour} {
		t.Run(liveAge.String(), func(t *testing.T) {
			useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			usage := filepath.Join(state, "ccmorph", "usage")
			day := "2026-10-18"
			stamp := day + "T09:00:00Z"
			writeSpendArchive(t, usage, day, map[string]string{"a.json": spendArchiveReceipt("a", "turn", stamp, 1, 10)})
			path := filepath.Join(usage, day, "a.json")
			writeSpendFixture(t, path, spendArchiveReceipt("a", "turn", stamp, 2, 20))
			mtime := now().Add(liveAge)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			want := float64(2)
			if liveAge < 0 {
				want = 1
			}
			e := spendEntry("gw", "")
			if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != want || e.Spend.MonthTurns != 1 {
				t.Fatalf("newest named receipt (live age %s): %+v, want cost %g", liveAge, e, want)
			}
		})
	}
}

func TestQuotaSpendArchiveReadPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files with mode 0")
	}
	for _, suffix := range []string{".jsonl", ".jsonl.idx"} {
		t.Run(suffix, func(t *testing.T) {
			useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			usage := filepath.Join(state, "ccmorph", "usage")
			writeSpendArchive(t, usage, "2026-10-18", map[string]string{"a.json": spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10)})
			if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
				t.Fatalf("warm archive read: %+v", e)
			}
			path := filepath.Join(usage, "2026-10-18"+suffix)
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(path, 0o600) })
			if e := spendEntry("gw", ""); e.Error == "" || e.Spend != nil {
				t.Fatalf("unreadable archive served cached spend: %+v", e)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
				t.Fatalf("readable archive did not recover: %+v", e)
			}
		})
	}
}

func TestQuotaSpendArchiveReadOnlyStoreIsReadable(t *testing.T) {
	for _, lockPresent := range []bool{false, true} {
		t.Run(fmt.Sprintf("lock=%t", lockPresent), func(t *testing.T) {
			useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
			compactUsage = shim.CompactUsage
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			usage := filepath.Join(state, "ccmorph", "usage")
			writeSpendArchive(t, usage, "2026-10-18", map[string]string{"a.json": spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10)})
			if lockPresent {
				if err := os.WriteFile(filepath.Join(usage, ".move.lock"), nil, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(usage, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(usage, 0o700) })
			e := spendEntry("gw", "")
			if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
				t.Fatalf("read-only store failed read: %+v", e)
			}
		})
	}
}

func TestQuotaSpendArchiveRewritesInvalidateCaches(t *testing.T) {
	for _, day := range []string{"2026-10-18", "2026-10-20"} {
		t.Run(day, func(t *testing.T) {
			useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			usage := filepath.Join(state, "ccmorph", "usage")
			files := map[string]string{"a.json": spendArchiveReceipt("a", "turn_a", day+"T09:00:00Z", 1, 10)}
			path := writeSpendArchive(t, usage, day, files)
			if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
				t.Fatalf("warm archived read: %+v", e)
			}
			archiveInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			indexInfo, err := os.Stat(path + ".idx")
			if err != nil {
				t.Fatal(err)
			}
			files["a.json"] = spendArchiveReceipt("a", "turn_a", day+"T09:00:00Z", 9, 90)
			writeSpendArchive(t, usage, day, files)
			for p, info := range map[string]os.FileInfo{path: archiveInfo, path + ".idx": indexInfo} {
				if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			// The process memo and every disk cache must see a new archive generation,
			// even when both files retain their previous sizes and mtimes.
			if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 9 || e.Spend.MonthTokens != 90 {
				t.Fatalf("archive rewrite served stale totals: %+v", e)
			}
		})
	}
}
