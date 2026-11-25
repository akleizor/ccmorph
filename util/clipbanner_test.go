package util

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClipBannerIsBoundedWhenAGrandchildHoldsThePipe pins #636: a killed
// child that left the pipe open must not hold ClipBanner past its bound.
func TestClipBannerIsBoundedWhenAGrandchildHoldsThePipe(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hang")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ClipBanner(script)
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("ClipBanner held %s: the bound is not real", elapsed)
	}
}
