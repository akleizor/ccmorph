package adapter

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A shared adapter ends when no session holds its users lock — the case a
// last session killed without releasing leaves behind — and not before.
func TestSharedAdapterEndsOnceNoSessionHoldsIt(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".gw.adapter.k")
	os.WriteFile(base+".lock", nil, 0o600)
	holder, err := os.OpenFile(base+".users", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	a := &cliArgs{shared: base}
	ended := -1
	a.checkParent(func(code int) { ended = code })
	if ended != -1 {
		t.Fatal("a held adapter shut down")
	}
	holder.Close() // SIGKILL: the kernel closes the lock
	a.checkParent(func(code int) { ended = code })
	if ended != 0 {
		t.Fatalf("an adapter nobody holds must shut down cleanly, got %d", ended)
	}
}
