package paths

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// writeArchiveTestDay publishes one archive generation under dir/<day>.jsonl
// and returns its path. Rows are written sorted by name, the archive's order.
func writeArchiveTestDay(t *testing.T, dir, day string, rows map[string]string) string {
	t.Helper()
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	var data []byte
	files := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, rows[name]...)
		data = append(data, '\n')
		files = append(files, map[string]any{"name": name, "mtime_ns": 5})
	}
	sum := sha256.Sum256(data)
	meta, err := json.Marshal(map[string]any{"v": 1, "sha256": hex.EncodeToString(sum[:]), "files": files})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, day+".jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".idx", meta, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// settleTestArchive makes a fixture generation immediately memorable; the
// production settle would otherwise keep a just-written pair out of the memo.
func settleTestArchive(t *testing.T) {
	t.Helper()
	old := ArchiveSettle
	ArchiveSettle = 0
	t.Cleanup(func() { ArchiveSettle = old })
}

func countArchiveLoads(t *testing.T) *int32 {
	t.Helper()
	var n int32
	old := UsageArchiveLoaded
	UsageArchiveLoaded = func(string) { atomic.AddInt32(&n, 1) }
	t.Cleanup(func() { UsageArchiveLoaded = old })
	return &n
}

// An unchanged generation is answered from the memo: the second read stats the
// pair and opens neither file (#559).
func TestReadUsageArchiveRemembersASettledGeneration(t *testing.T) {
	settleTestArchive(t)
	path := writeArchiveTestDay(t, t.TempDir(), "2026-10-18", map[string]string{"a.json": `{"x":1}`})
	loads := countArchiveLoads(t)
	first, err := ReadUsageArchive(path)
	if err != nil || len(first) != 1 || string(first[0].Data) != `{"x":1}` {
		t.Fatalf("first read: %v %v", first, err)
	}
	second, err := ReadUsageArchive(path)
	if err != nil || len(second) != 1 || string(second[0].Data) != `{"x":1}` {
		t.Fatalf("memo read: %v %v", second, err)
	}
	if got := atomic.LoadInt32(loads); got != 1 {
		t.Fatalf("an unchanged generation loaded %d times, want one", got)
	}
}

// A rewrite that keeps the bytes' size and puts the mtime back is still a new
// generation: the identity's ctime moves, so the memo re-reads (#635).
func TestReadUsageArchiveRereadsASameSizeRewrite(t *testing.T) {
	settleTestArchive(t)
	dir, day := t.TempDir(), "2026-10-18"
	path := writeArchiveTestDay(t, dir, day, map[string]string{"a.json": `{"x":1}`})
	if _, err := ReadUsageArchive(path); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeArchiveTestDay(t, dir, day, map[string]string{"a.json": `{"x":2}`})
	if err := os.Chtimes(path, old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadUsageArchive(path)
	if err != nil || len(rows) != 1 || string(rows[0].Data) != `{"x":2}` {
		t.Fatalf("a same-size rewrite was answered from the memo: %v %v", rows, err)
	}
}

// A failed read is never remembered: fixing the index makes the next read
// answer, instead of replaying the failure from a memo.
func TestReadUsageArchiveNeverRemembersAFailure(t *testing.T) {
	settleTestArchive(t)
	path := writeArchiveTestDay(t, t.TempDir(), "2026-10-18", map[string]string{"a.json": `{"x":1}`})
	if err := os.WriteFile(path+".idx", []byte("{not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	loads := countArchiveLoads(t)
	if _, err := ReadUsageArchive(path); err == nil {
		t.Fatal("a corrupt index read without an error")
	}
	writeArchiveTestDay(t, filepath.Dir(path), "2026-10-18", map[string]string{"a.json": `{"x":1}`})
	rows, err := ReadUsageArchive(path)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the repaired generation was not read: %v %v", rows, err)
	}
	if got := atomic.LoadInt32(loads); got != 2 {
		t.Fatalf("failures were remembered: %d loads, want 2 (one failed, one repaired)", got)
	}
}

// The memo hands every caller its own rows: naming or extending one read must
// not reach the next.
func TestReadUsageArchiveRowsBelongToTheCaller(t *testing.T) {
	settleTestArchive(t)
	path := writeArchiveTestDay(t, t.TempDir(), "2026-10-18", map[string]string{"a.json": `{"x":1}`})
	if _, err := ReadUsageArchive(path); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadUsageArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Name = "changed.json"
	rows[0].Data = append(rows[0].Data, 'x')
	rows = append(rows, UsageArchiveRow{Name: "extra.json"})
	again, err := ReadUsageArchive(path)
	if err != nil || len(again) != 1 || again[0].Name != "a.json" || !bytes.Equal(again[0].Data, []byte(`{"x":1}`)) {
		t.Fatalf("a caller's edit reached the memo: %v %v", again, err)
	}
}

// A generation read while unsettled is not remembered: the writer that just
// published it may still replace it, so the next read loads it again.
func TestReadUsageArchiveForgetsNothingUnsettled(t *testing.T) {
	old := ArchiveSettle
	ArchiveSettle = time.Hour
	t.Cleanup(func() { ArchiveSettle = old })
	path := writeArchiveTestDay(t, t.TempDir(), "2026-10-18", map[string]string{"a.json": `{"x":1}`})
	loads := countArchiveLoads(t)
	for i := 0; i < 2; i++ {
		if _, err := ReadUsageArchive(path); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(loads); got != 2 {
		t.Fatalf("an unsettled generation was remembered: %d loads, want 2", got)
	}
	ArchiveSettle = 0
	if _, err := ReadUsageArchive(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUsageArchive(path); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(loads); got != 3 {
		t.Fatalf("a settled generation was not remembered: %d loads, want 3", got)
	}
}
