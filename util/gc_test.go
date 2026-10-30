package util

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestGCReapsOnlyDeadOldRelays(t *testing.T) {
	dir := t.TempDir()
	past := time.Now().Add(-2 * time.Minute)
	dead := filepath.Join(dir, ".reswitch.999999.1.2")
	mine := filepath.Join(dir, ".reswitch."+strconv.Itoa(os.Getpid())+".1.2")
	young := filepath.Join(dir, ".reswitch.999998.1.2")
	for _, f := range []string{dead, mine, young} {
		os.WriteFile(f, nil, 0o600)
	}
	os.Chtimes(dead, past, past)
	os.Chtimes(mine, past, past)
	gcStale([]string{dir, filepath.Join(dir, "profiles"), dir}, nil, nil, nil)
	if _, err := os.Stat(dead); err == nil {
		t.Fatal("an old relay of a dead launcher must go")
	}
	for _, f := range []string{mine, young} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s must stay (live owner or under a minute old)", f)
		}
	}
}
