package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBannerReadLimit pins the bound ClipBanner's read keeps: five seconds
// unless CCMORPH_BRIDGE_BANNER_TIMEOUT names a usable one. Anything unusable
// keeps the default rather than arming a deadline already past (#770).
func TestBannerReadLimit(t *testing.T) {
	t.Setenv("CCMORPH_BRIDGE_BANNER_TIMEOUT", "")
	if got := bannerReadLimit(); got != 5*time.Second {
		t.Fatalf("default limit %v, want 5s", got)
	}
	// A caller on a starved host may wait longer; anything unusable keeps the
	// default rather than arming a zero or negative deadline — including a
	// count whose nanoseconds would not fit a duration (9223372036 = 292
	// years), which would wrap negative and abort the read at once.
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"9223372036", 9223372036 * time.Second},
		{"9223372037", 5 * time.Second},
		{"99999999999999999999999", 5 * time.Second},
		{"0", 5 * time.Second},
		{"-5", 5 * time.Second},
		{"soon", 5 * time.Second},
		{"", 5 * time.Second},
	} {
		t.Setenv("CCMORPH_BRIDGE_BANNER_TIMEOUT", tc.env)
		if got := bannerReadLimit(); got != tc.want {
			t.Errorf("env %q: limit %v, want %v", tc.env, got, tc.want)
		}
	}
}

// TestClipBannerReadsUnderItsEnvBound is the seam the version-gate test raises:
// a `-h` slower than the bound comes back empty (an unparseable banner counts
// as new), and the same binary read under a raised bound returns the banner
// that keeps the below-floor verdict truthful.
func TestClipBannerReadsUnderItsEnvBound(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "cliproxyapi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.4\nprintf 'CLIProxyAPI Version: 8.0.45, Commit: x\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCMORPH_BRIDGE_BANNER_TIMEOUT", "0.05")
	if got := ClipBanner(bin); got != "" {
		t.Fatalf("read %q before a 0.05 s bound elapsed, want an empty banner", got)
	}
	t.Setenv("CCMORPH_BRIDGE_BANNER_TIMEOUT", "600")
	if got := ClipBanner(bin); !strings.Contains(got, "8.0.45") {
		t.Fatalf("read %q under a raised bound, want the banner", got)
	}
}
