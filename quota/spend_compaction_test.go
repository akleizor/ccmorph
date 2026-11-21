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
	"sync"
	"sync/atomic"
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
	compactUsage = autoCompact
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

// A same-name identity conflict is the keep-both every reader applies, never a
// refusal: the month counts both turns, and both survive the compaction.
func TestQuotaSpendArchiveKeepBothCompactsConflictingNames(t *testing.T) {
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
				t.Fatalf("conflicting same-name receipts collapsed to one turn: %+v", e)
			}
			rows, err := paths.ReadUsageArchive(path)
			if err != nil || len(rows) != 2 {
				t.Fatalf("conflicting same-name receipts did not both survive maintenance: %v %v", rows, err)
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

// A first publication killed between its index and its content leaves an index
// over no content; the folder holds that day whole, so quota reads the folder
// instead of failing the month on an error no data backs.
func TestQuotaSpendIndexOnlyOrphanAnswersFromFolder(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	day := "2026-10-18"
	path := writeSpendArchive(t, usage, day, map[string]string{"a.json": spendArchiveReceipt("a", "turn", day+"T09:00:00Z", 1, 10)})
	writeSpendFixture(t, filepath.Join(usage, day, "a.json"), spendArchiveReceipt("a", "turn", day+"T09:00:00Z", 2, 20))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 2 {
		t.Fatalf("orphan index bricked the month: %+v", e)
	}
}

// The hash-verified archive answers a closed day on its own: a leftover
// folder this run cannot list — the compaction's unlink meeting foreign
// ownership — may not fail the month while report and export still answer.
func TestQuotaSpendUnreadableLeftoverFolderAnswersFromArchive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads folders with mode 0")
	}
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	day := "2026-10-18"
	writeSpendArchive(t, usage, day, map[string]string{"a.json": spendArchiveReceipt("a", "turn", day+"T09:00:00Z", 1, 10)})
	dir := filepath.Join(usage, day)
	writeSpendFixture(t, filepath.Join(dir, "a.json"), spendArchiveReceipt("a", "turn", day+"T09:00:00Z", 2, 20))
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	e := spendEntry("gw", "")
	if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 1 {
		t.Fatalf("unreadable leftover folder failed the month the archive answers: %+v", e)
	}
}

// Auto-compaction is not invisible: a day the compactor refuses is logged, the
// month still answers, and the refusal does not block the days after it.
func TestQuotaSpendAutoCompactionLogsFailure(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	compactUsage = autoCompact
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	stale := writeSpendArchive(t, usage, "2026-09-15", map[string]string{"a.json": spendArchiveReceipt("a", "turn", "2026-09-15T09:00:00Z", 1, 10)})
	if err := os.WriteFile(stale, []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSpendFixture(t, filepath.Join(usage, "2026-09-15", "b.json"), spendArchiveReceipt("b", "turn", "2026-09-15T10:00:00Z", 3, 30))
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "b.json"), spendArchiveReceipt("b", "turn", "2026-10-18T09:00:00Z", 2, 20))
	var logged []string
	oldLog := logf
	logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = oldLog })
	e := spendEntry("gw", "")
	if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 2 {
		t.Fatalf("a September store wedge failed the October read: %+v", e)
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "usage maintenance") || !strings.Contains(joined, "2026-09-15") {
		t.Fatalf("swallowed maintenance failure: %v", logged)
	}
	if _, err := os.Stat(filepath.Join(usage, "2026-10-18.jsonl")); err != nil {
		t.Fatalf("the bad day blocked the newer one: %v", err)
	}
}

// The read path's compaction is the statusline's: a day too large for its
// budget is left whole for usage-compact, a smaller day still compacts, and a
// spent deadline compacts nothing — while the month answers whole every time.
// The one day a pass may compact never holds the read past its budget (#654).
func TestQuotaSpendAutoCompactionStaysWithinItsBudget(t *testing.T) {
	t.Run("a day over the cap", func(t *testing.T) {
		useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
		compactUsage = autoCompact
		old := autoCompactMaxFiles
		autoCompactMaxFiles = 3
		t.Cleanup(func() { autoCompactMaxFiles = old })
		state := t.TempDir()
		t.Setenv("XDG_STATE_HOME", state)
		usage := filepath.Join(state, "ccmorph", "usage")
		for _, id := range []string{"a", "b", "c", "d"} {
			writeSpendFixture(t, filepath.Join(usage, "2026-10-16", id+".json"), spendArchiveReceipt(id, "turn", "2026-10-16T09:00:00Z", 1, 10))
		}
		writeSpendFixture(t, filepath.Join(usage, "2026-10-17", "e.json"), spendArchiveReceipt("e", "turn", "2026-10-17T09:00:00Z", 1, 10))
		if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 5 {
			t.Fatalf("budgeted maintenance changed the month: %+v", e)
		}
		if _, err := os.Stat(filepath.Join(usage, "2026-10-16.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("the read compacted a day over its cap: %v", err)
		}
		if entries, err := os.ReadDir(filepath.Join(usage, "2026-10-16")); err != nil || len(entries) != 4 {
			t.Fatalf("the read touched a day over its cap: %v %v", entries, err)
		}
		if _, err := os.Stat(filepath.Join(usage, "2026-10-17.jsonl")); err != nil {
			t.Fatalf("the day under the cap did not compact: %v", err)
		}
	})
	t.Run("a day over the byte cap", func(t *testing.T) {
		useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
		compactUsage = autoCompact
		old := autoCompactMaxBytes
		autoCompactMaxBytes = 300
		t.Cleanup(func() { autoCompactMaxBytes = old })
		state := t.TempDir()
		t.Setenv("XDG_STATE_HOME", state)
		usage := filepath.Join(state, "ccmorph", "usage")
		for _, id := range []string{"a", "b", "c"} {
			writeSpendFixture(t, filepath.Join(usage, "2026-10-16", id+".json"), spendArchiveReceipt(id, "turn", "2026-10-16T09:00:00Z", 1, 10))
		}
		writeSpendFixture(t, filepath.Join(usage, "2026-10-17", "e.json"), spendArchiveReceipt("e", "turn", "2026-10-17T09:00:00Z", 1, 10))
		if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 4 {
			t.Fatalf("budgeted maintenance changed the month: %+v", e)
		}
		if _, err := os.Stat(filepath.Join(usage, "2026-10-16.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("the read compacted a day over its byte cap: %v", err)
		}
		if entries, err := os.ReadDir(filepath.Join(usage, "2026-10-16")); err != nil || len(entries) != 3 {
			t.Fatalf("the read touched a day over its byte cap: %v %v", entries, err)
		}
		if _, err := os.Stat(filepath.Join(usage, "2026-10-17.jsonl")); err != nil {
			t.Fatalf("the day under the byte cap did not compact: %v", err)
		}
	})
	t.Run("a spent deadline", func(t *testing.T) {
		useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
		compactUsage = autoCompact
		old := autoCompactWait
		autoCompactWait = 0
		t.Cleanup(func() { autoCompactWait = old })
		state := t.TempDir()
		t.Setenv("XDG_STATE_HOME", state)
		usage := filepath.Join(state, "ccmorph", "usage")
		writeSpendFixture(t, filepath.Join(usage, "2026-10-17", "e.json"), spendArchiveReceipt("e", "turn", "2026-10-17T09:00:00Z", 2, 10))
		if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 2 {
			t.Fatalf("a spent budget changed the month: %+v", e)
		}
		if _, err := os.Stat(filepath.Join(usage, "2026-10-17.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("compacted after the deadline: %v", err)
		}
	})
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

// A cost Python wrote as a NaN token is damage in the price, not a second
// turn: the live row carrying it overwrites its priced archived sibling under
// the one merge rule every reader shares, and the month keeps the turn
// unpriced — the turn report counts as unknown spend and the export counts
// unpriced. This reader used to decode the pair with encoding/json directly,
// which refuses the token outright: it kept both rows, then decoded the
// damaged one to nil, so the same store answered three readers with three
// month totals (#654).
func TestQuotaSpendNonFiniteOverlayUnpricesNotOmits(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	compactUsage = shim.CompactUsage
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	day := "2026-10-18"
	path := writeSpendArchive(t, usage, day, map[string]string{
		"a.json": spendArchiveReceipt("r", "t", day+"T09:00:00Z", 1, 10),
	})
	live := filepath.Join(usage, day, "a.json")
	writeSpendFixture(t, live, `{"response_id":"r","turn_id":"t","profile":"gw","timestamp":"`+day+`T09:00:00Z","usage":{"cost":NaN,"input_tokens":10},"complete":true}`)
	stamp := now().Add(time.Second)
	if err := os.Chtimes(live, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	e := spendEntry("gw", "")
	if e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 0 || e.Spend.MonthTurns != 1 || e.Spend.UnpricedTurns != 1 {
		t.Fatalf("a damaged price changed the month instead of only the price: %+v", e)
	}
	rows, err := paths.ReadUsageArchive(path)
	if err != nil || len(rows) != 1 || !strings.Contains(string(rows[0].Data), "NaN") {
		t.Fatalf("maintenance did not merge the damaged row into its twin: %v %v", rows, err)
	}
}

// A forward NTP step lets maintenance fold a day; the correction back makes
// that day open again with its folder gone. One boundary helper at the
// pass's instant means the reader asks the archive the same day it would have
// asked the folder, so the month stays whole instead of losing the day between
// two readings of "open" (#654).
func TestQuotaSpendClockStepForwardThenBackKeepsTheMonthWhole(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	compactUsage = shim.CompactUsage
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	day := "2026-10-18"
	writeSpendFixture(t, filepath.Join(usage, day, "a.json"),
		spendArchiveReceipt("r", "t", day+"T09:00:00Z", 5, 50))
	read := func(at time.Time) *Spend {
		now = func() time.Time { return at }
		tallyMemo.key = ""
		e := spendEntry("gw", "")
		if e.Error != "" || e.Spend == nil {
			t.Fatalf("a clock step bricked the read: %+v", e)
		}
		return e.Spend
	}
	if s := read(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)); s.MonthUSD != 5 || s.MonthTurns != 1 {
		t.Fatalf("before the step %+v", s)
	}
	// The step forward folds the day into its archive.
	if rows, err := paths.ReadUsageArchive(filepath.Join(usage, day+".jsonl")); err != nil || len(rows) != 1 {
		t.Fatalf("maintenance did not archive the closed day: %v %v", rows, err)
	}
	if s := read(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)); s.MonthUSD != 5 || s.MonthTurns != 1 {
		t.Fatalf("after the step forward %+v", s)
	}
	// The correction back reopens the day; its folder is already gone.
	if s := read(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)); s.MonthUSD != 5 || s.MonthTurns != 1 {
		t.Fatalf("the step back lost an archived day classified open: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(usage, day)); !os.IsNotExist(err) {
		t.Fatalf("the day folder came back: %v", err)
	}
}

// A statusline queued behind a compaction's exclusive hold is the hang INTENT 8
// forbids: the read refuses with a named busy error inside the lock's bound and
// the view carries no spend, never an unbounded wait.
func TestQuotaSpendBusyStoreRefusesWithinTheBound(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "a.json"),
		spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10))
	tallyMemo.key = ""
	held, err := paths.UsageStoreLock(usage, true)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	start := time.Now()
	e := spendEntry("gw", "")
	if e.Spend != nil || !strings.Contains(e.Error, "usage store busy") {
		t.Fatalf("a busy store answered %+v", e)
	}
	if elapsed := time.Since(start); elapsed > 3*paths.UsageLockWait {
		t.Fatalf("the refusal took %v, past the lock's bound", elapsed)
	}
}

// The memo's mutex guards the memo, never the store's I/O: a read that holds it
// across a scan makes every second reader — and every refusal — queue behind
// the first (INTENT 8, #654).
func TestQuotaSpendMemoMutexIsFreeDuringTheRead(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "a.json"),
		spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10))
	tallyMemo.key = ""
	beforeFlat = func() {
		if !tallyMemo.TryLock() {
			t.Error("the memo mutex was held across the store read")
			return
		}
		tallyMemo.Unlock()
	}
	t.Cleanup(func() { beforeFlat = nil })
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil {
		t.Fatalf("read failed: %+v", e)
	}
}

// The profiles of one quota call share a month read: a statusline pays one
// scan, not one per profile.
func TestQuotaSpendConcurrentProfilesShareOneRead(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "a.json"),
		spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10))
	tallyMemo.key = ""
	var reads int32
	beforeFlat = func() { atomic.AddInt32(&reads, 1) }
	t.Cleanup(func() { beforeFlat = nil })
	var wg sync.WaitGroup
	for _, name := range []string{"gw", "other"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := spendEntry(name, ""); e.Error != "" || e.Spend == nil {
				t.Errorf("profile %s: %+v", name, e)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&reads); got != 1 {
		t.Fatalf("two profiles read the month %d times, want one shared scan", got)
	}
}

// An unchanged store costs what its marks cost: a cold pass loads each
// archived day once, and a pass whose marks still match loads none (#559,
// #654). A pass that re-hashes every archive on every statusline call is the
// regression this counts.
func TestQuotaSpendReadsEachArchiveOncePerPass(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	for _, day := range []string{"2026-10-17", "2026-10-18"} {
		writeSpendArchive(t, usage, day, map[string]string{
			"a.json": spendArchiveReceipt("a"+day, "turn"+day, day+"T09:00:00Z", 1, 10),
		})
	}
	var loads int32
	archiveReads = func() { atomic.AddInt32(&loads, 1) }
	t.Cleanup(func() { archiveReads = nil })
	tallyMemo.key = ""
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 2 {
		t.Fatalf("cold read: %+v", e)
	}
	if got := atomic.LoadInt32(&loads); got > 2 {
		t.Fatalf("a cold pass loaded %d archive generations for two days, want at most one per day", got)
	}
	atomic.StoreInt32(&loads, 0)
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 2 {
		t.Fatalf("unchanged read: %+v", e)
	}
	if got := atomic.LoadInt32(&loads); got != 0 {
		t.Fatalf("an unchanged pass loaded %d archive generations, want none", got)
	}
}

// An overlay this run cannot read beside an archived day is one receipt the
// month leaves out, never the whole day: the archive and every readable
// overlay still count, the reason names the one skipped, and the run still
// fails — the total report and the export give the same store (#654). The
// skip is not remembered: restoring the mode counts the receipt again (#538).
func TestQuotaSpendSkipsAnUnreadableOverlayAndNamesIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files with mode 0")
	}
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	writeSpendArchive(t, usage, "2026-10-18", map[string]string{"a.json": spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10)})
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "b.json"), spendArchiveReceipt("b", "turn", "2026-10-18T10:00:00Z", 2, 10))
	locked := filepath.Join(usage, "2026-10-18", "c.json")
	writeSpendFixture(t, locked, spendArchiveReceipt("c", "turn", "2026-10-18T11:00:00Z", 4, 10))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o600) })
	tallyMemo.key = ""
	e := spendEntry("gw", "")
	if e.Spend == nil || e.Spend.MonthUSD != 3 || e.Spend.MonthTurns != 2 || e.Spend.UnreadableReceipts != 1 {
		t.Fatalf("the archived day was not answered without its unreadable overlay: %+v %+v", e, e.Spend)
	}
	if !strings.Contains(e.Error, "not readable") || !strings.Contains(e.Error, "c.json") {
		t.Fatalf("the skipped receipt was not named: %q", e.Error)
	}
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	tallyMemo.key = ""
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 7 || e.Spend.UnreadableReceipts != 0 {
		t.Fatalf("a receipt readable again was not counted: %+v %+v", e, e.Spend)
	}
}

// A response the store holds under two names — a flat copy and its day
// folder's — counts once, by the day folder's copy, on both of quota's reads:
// skipping an unrelated receipt this run cannot read sends the month through
// the report's reader (shim.ReadUsageWindow), and that must not change which
// copy counts, so the total of the readable receipts stays what it was (#654).
func TestQuotaSpendSkippingAReceiptKeepsTheDayFolderCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files with mode 0")
	}
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	usage := filepath.Join(state, "ccmorph", "usage")
	writeSpendFixture(t, filepath.Join(usage, "0flat.json"), spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 1, 10))
	writeSpendFixture(t, filepath.Join(usage, "2026-10-18", "a.json"), spendArchiveReceipt("a", "turn", "2026-10-18T09:00:00Z", 9, 10))
	tallyMemo.key = ""
	if e := spendEntry("gw", ""); e.Error != "" || e.Spend == nil || e.Spend.MonthUSD != 9 || e.Spend.MonthTurns != 1 {
		t.Fatalf("the day folder's copy did not count: %+v %+v", e, e.Spend)
	}
	locked := filepath.Join(usage, "2026-10-19", "other.json")
	writeSpendFixture(t, locked, spendArchiveReceipt("other", "turn", "2026-10-19T09:00:00Z", 4, 10))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o600) })
	tallyMemo.key = ""
	e := spendEntry("gw", "")
	if e.Spend == nil || e.Spend.MonthUSD != 9 || e.Spend.MonthTurns != 1 || e.Spend.UnreadableReceipts != 1 {
		t.Fatalf("skipping an unrelated receipt changed which copy counts: %+v %+v", e, e.Spend)
	}
}
