package util

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// #177: the shared history's inode is never replaced — a writer holding it
// open by pathname across the merge (claude appends that way, through the very
// links setup made) must not lose its line to it.
func TestMergeHistoryKeepsAConcurrentAppend(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared/history.jsonl")
	mustWrite(t, shared, hrec(100, "a"))
	appender, err := os.OpenFile(shared, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer appender.Close()
	own := filepath.Join(root, "own/history.jsonl")
	mustWrite(t, own, hrec(200, "b"))
	if !mergeHistory(own, shared) {
		t.Fatal("merge failed")
	}
	if _, err := appender.WriteString(hrec(300, "c")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if want := hrec(100, "a") + hrec(200, "b") + hrec(300, "c"); string(b) != want {
		t.Errorf("a writer holding the shared history open lost its line: got %q, want %q", b, want)
	}
}

// #177: two profiles sharing their history at the same time — the normal case
// when several sessions launch at once — must both land in the shared file;
// each rewriting what it read would drop the other's whole history.
func TestMergeHistorySerializesTwoConcurrentMerges(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared/history.jsonl")
	mustWrite(t, shared, hrec(100, "base"))
	a := filepath.Join(root, "a/history.jsonl")
	b := filepath.Join(root, "b/history.jsonl")
	mustWrite(t, a, hrec(200, "from-a"))
	mustWrite(t, b, hrec(300, "from-b"))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, own := range []string{a, b} {
		wg.Add(1)
		go func(own string) {
			defer wg.Done()
			<-start
			mergeHistory(own, shared)
		}(own)
	}
	close(start)
	wg.Wait()
	got, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{hrec(100, "base"), hrec(200, "from-a"), hrec(300, "from-b")} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the shared history must keep %q: got %q", want, got)
		}
	}
	if strings.Count(string(got), "\"from-a\"") != 1 || strings.Count(string(got), "\"from-b\"") != 1 {
		t.Errorf("neither profile's records may be duplicated or dropped: %q", got)
	}
}

// #177: a file with no final newline must not glue two records on one line.
func TestMergeHistoryKeepsRecordsApartWithoutAFinalNewline(t *testing.T) {
	root := t.TempDir()
	shared, own := filepath.Join(root, "s.jsonl"), filepath.Join(root, "o.jsonl")
	mustWrite(t, shared, strings.TrimSuffix(hrec(100, "a"), "\n"))
	mustWrite(t, own, hrec(150, "m")+strings.TrimSuffix(hrec(200, "b"), "\n"))
	if !mergeHistory(own, shared) {
		t.Fatal("merge failed")
	}
	b, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if want := hrec(100, "a") + hrec(150, "m") + hrec(200, "b"); string(b) != want {
		t.Errorf("got %q, want %q", b, want)
	}
}

// #185: mergeHistory read the profile's file before taking the lock and
// removed it after verifying only that earlier read. A record a concurrent
// session appended in between was deleted with the file. The read is now
// checked unchanged before the removal, and the late record survives.
func TestMergeHistoryKeepsARecordAppendedAfterItsRead(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared/history.jsonl")
	mustWrite(t, shared, hrec(100, "a"))
	own := filepath.Join(root, "own/history.jsonl")
	mustWrite(t, own, hrec(200, "b"))
	late := false
	historyReadHook = func() {
		if late {
			return
		}
		late = true
		f, err := os.OpenFile(own, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(hrec(300, "late")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { historyReadHook = nil })
	if mergeHistory(own, shared) {
		t.Fatal("a file that grew after its read must not be consumed")
	}
	got := ""
	for _, p := range []string{own, shared} {
		if b, err := os.ReadFile(p); err == nil {
			got += string(b)
		}
	}
	if !strings.Contains(got, "late") {
		t.Fatalf("a record appended during the merge was lost: own=%q shared=%q", got, func() string { b, _ := os.ReadFile(shared); return string(b) }())
	}
	if b, err := os.ReadFile(own); err != nil || !strings.Contains(string(b), "late") {
		t.Fatalf("the profile's file must keep the late record: %q %v", b, err)
	}
}

// #185: a merge that cannot take the history lock within its deadline leaves
// the profile on its own file instead of hanging the launch on it.
func TestMergeHistoryGivesUpOnAStuckLock(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared/history.jsonl")
	mustWrite(t, shared, hrec(100, "a"))
	own := filepath.Join(root, "own/history.jsonl")
	mustWrite(t, own, hrec(200, "b"))
	holder, err := os.OpenFile(filepath.Join(root, "shared/.ccm-history.lock"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	lockWait = 0
	t.Cleanup(func() { lockWait = 2 * time.Second })
	if mergeHistory(own, shared) {
		t.Fatal("a merge that could not take the lock must report failure")
	}
	if b, err := os.ReadFile(own); err != nil || !strings.Contains(string(b), "b") {
		t.Fatalf("the profile's history must stay whole: %q %v", b, err)
	}
	if b, _ := os.ReadFile(shared); string(b) != hrec(100, "a") {
		t.Fatalf("the shared file must not be rewritten without the lock: %q", b)
	}
}
