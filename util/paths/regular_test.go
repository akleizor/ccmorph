package paths

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO under a receipt's name is refused at once, never waited on: a plain
// open would block until a writer appeared, holding whatever lock the reader
// holds (INTENT 8, #654). A regular file still reads whole.
func TestReadRegularFileRefusesAFIFOWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "unusable.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadRegularFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("a FIFO was not refused as not regular: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO under a receipt's name hung")
	}
	plain := filepath.Join(dir, "a.json")
	if err := os.WriteFile(plain, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadRegularFile(plain); err != nil || string(data) != `{"a":1}` {
		t.Fatalf("a regular receipt did not read: %q %v", data, err)
	}
	if _, err := ReadRegularFile(filepath.Join(dir, "gone.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing name must stay ErrNotExist for the readers' skip: %v", err)
	}
}
