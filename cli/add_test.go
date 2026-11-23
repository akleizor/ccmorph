package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Issue #283: every browser-login row names the product or plan it signs in to,
// and the two Kimi logins (kimi.ai and kimi.com) never share their text.
func TestProviderDetailNamesEachBrowserLogin(t *testing.T) {
	want := map[string]string{
		"antigravity": "Browser sign-in to Google Antigravity, through cliproxyapi",
		"codex":       "Browser sign-in to ChatGPT (Codex), through cliproxyapi",
		"devin":       "Browser sign-in to Devin, through cliproxyapi",
		"kimi-ai":     "Browser sign-in to a kimi.ai account, through cliproxyapi",
		"kimi":        "Browser sign-in to a kimi.com account, through cliproxyapi",
		"meta":        "Browser sign-in to Meta AI, through cliproxyapi",
		"xai":         "Browser sign-in to xAI (Grok), through cliproxyapi",
	}
	seen := map[string]string{}
	for provider, detail := range want {
		if got := providerDetail(provider); got != detail {
			t.Errorf("providerDetail(%q) = %q, want %q", provider, got, detail)
		}
		if other, dup := seen[detail]; dup {
			t.Errorf("%q and %q share the row text %q", other, provider, detail)
		}
		seen[detail] = provider
	}
	if got := providerDetail("future-provider"); got != "Sign in to Future-provider with your browser, through cliproxyapi" {
		t.Errorf("an unknown provider keeps the bridge note and its own name, got %q", got)
	}
}

// GH-456: the bridge's own flags can look like a provider login. A row like
// "-login-proxy string" carries the -login marker, but the leading dash the
// parse strips is that same marker's dash, so the position came back -1 and
// the slice panicked — on every `ccmorph add` with the bridge installed. It is
// a bridge flag, not a sign-in: the row is skipped, and the login rows around
// it still parse.
func TestProviderListSkipsABridgeFlagThatOnlyLooksLikeALogin(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '  -login-proxy string' '  -kimi-login' '  -codex-login'\n"
	if err := os.WriteFile(filepath.Join(dir, "cliproxyapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, ok := providerList()
	if !ok {
		t.Fatal("a bridge on PATH was reported missing")
	}
	if want := []string{"kimi", "codex"}; !slices.Equal(got, want) {
		t.Errorf("providerList() = %q, want %q", got, want)
	}
}

// #449: the preset's three tier columns are opus, sonnet and haiku by name.
// The wizard zips them onto the words after fable, so reordering util.TierWords
// would silently write a column under another tier's key; this pins the keys
// the columns land on. Fable has no column: it is left to `ccmorph config set`.
func TestAddWizardTierColumnsArePinnedByName(t *testing.T) {
	got := tierColumns([]string{"m-opus", "m-sonnet", "m-haiku"})
	want := []string{
		"env.ANTHROPIC_DEFAULT_OPUS_MODEL=m-opus",
		"env.ANTHROPIC_DEFAULT_SONNET_MODEL=m-sonnet",
		"env.ANTHROPIC_DEFAULT_HAIKU_MODEL=m-haiku",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tier columns = %q, want %q", got, want)
	}
	for _, line := range got {
		if strings.Contains(line, "FABLE") {
			t.Fatalf("a preset column wrote fable's key: %q", line)
		}
	}
}

// GH-649, GH-837: closing the terminal sends SIGHUP/SIGTERM to the whole
// foreground process group, and Ctrl-C sends SIGINT. The sign-in child must be
// the one that dies — the parent has to survive long enough to run the caller's
// rollback. Before the handoff the default disposition killed this process
// outright, which here took the whole test binary down with it: reaching the
// assertions is half the assertion.
func TestHandSignInSignalsReachTheChildNotTheParent(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			// Bounded on its own: if the handoff ever stops delivering, the
			// child still leaves in a few seconds and the test fails on its
			// assertions instead of hanging the suite.
			cmd := exec.Command("sh", "-c",
				"i=0; while [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			release := handSignInSignals(cmd.Process)
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				release()
				t.Fatal(err)
			}
			err := cmd.Wait()
			release()
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("the child outlived the forwarded %v: %v", sig, err)
			}
			ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != sig {
				t.Fatalf("the child did not die of the forwarded %v: %v", sig, err)
			}
		})
	}
}
