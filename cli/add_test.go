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
	"time"
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

// handoffForwardDeadline bounds the wait for the handoff's async forward
// (GH-851). Measured with the whole suite loaded, a forward lands within
// milliseconds of the signal; the one observed loss (2026-11-24) was a
// scheduling stall of ~13 s. The deadline is the wait's bound, never its
// pacing: the pass path reads the moment the child dies, and 60 s still
// sits under the 120 s hang bound the zsh suite shares.
const handoffForwardDeadline = 60 * time.Second

// GH-649: closing the terminal sends SIGHUP/SIGTERM to the whole foreground
// process group. The sign-in child must be the one that dies — the parent has
// to survive long enough to run the caller's rollback. Before the handoff the
// default disposition killed this process outright, which here took the whole
// test binary down with it: reaching the assertions is half the assertion.
//
// GH-851: the handoff is deliberately async, so the test must not let the
// child's own life end the wait. The old child left on its own in ~5 s and a
// machine that stalled the parent's scheduling past that read as a broken
// handoff. The child here outlives the deadline by minutes: only the
// forwarded signal can end this wait, and the deadline — the test's own
// bound, in the zsh suite's hang-bound spirit — names the lost forward.
func TestHandSignInSignalsReachTheChildNotTheParent(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command("sleep", "300")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Exactly one Wait for this child; its outcome waits on the
			// channel, so every path below can take it without hanging.
			gone := make(chan error, 1)
			reaped := make(chan struct{})
			go func() {
				defer close(reaped)
				gone <- cmd.Wait()
			}()
			// Whatever the outcome below, the child is ours to clear — and
			// ours until the Wait returns, so a path that leaves early (the
			// deadline, a failed Kill) kills it and joins that one waiter:
			// the binary never exits ahead of the reap. On the green path
			// the child died of the forward, the kill finds no process and
			// the join has already happened.
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-reaped
			})
			release := handSignInSignals(cmd.Process)
			defer release()
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-gone:
			case <-time.After(handoffForwardDeadline):
				// The child cannot outlive its sleep within the deadline, so
				// a timeout here is the lost forward itself.
				t.Fatalf("the child outlived the forwarded %v: no signal reached it within %v",
					sig, handoffForwardDeadline)
			}
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

func TestShownEndpointNeverPrintsACredential(t *testing.T) {
	const login = "user:" + "canary835"
	const host = "gateway.invalid"
	for _, c := range []struct{ name, url, want string }{
		{"blank is the default host", "", "api.anthropic.com"},
		{"bare host", host, host},
		{"bare host with a port", host + ":8080", host + ":8080"},
		{"scheme URL", "http://" + host, "http://" + host},
		{"scheme URL with a login", "http://" + login + "@" + host, "http://" + host},
		{"scheme URL with a login and a port", "http://" + login + "@" + host + ":8080", "http://" + host + ":8080"},
		{"scheme URL with a key in its query", "http://" + host + "/v1?key=canary835", "http://" + host + "/v1"},
		{"login without a scheme", login + "@" + host, "<redacted>"},
		{"login without a scheme, with a port and a path", login + "@" + host + ":8080/v1", "<redacted>"},
		{"query without a scheme", host + "/v1?key=canary835", "<redacted>"},
		{"fragment without a scheme", host + "#canary835", "<redacted>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := shownEndpoint(c.url)
			if got != c.want {
				t.Errorf("shownEndpoint = %q, want %q", got, c.want)
			}
			if strings.Contains(got, "canary835") {
				t.Errorf("shownEndpoint printed the credential: %q", got)
			}
		})
	}
}

// #852: the credential scan globs the per-provider auth dir under the
// profile root, which is user-settable and must stay literal. A root with
// an unclosed `[` made the joined pattern invalid, the ignored error answered
// nothing, and a completed sign-in was reported as "no credential appeared".
func TestProviderLoginFindsTheCredentialUnderABracketedRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state[6")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake stands in for cliproxyapi's browser login: it takes its auth
	// dir from the -config file and writes the sign-in there, exactly as the
	// real login leaves it on disk.
	script := "#!/bin/sh\n" +
		"dir=$(sed -n 's/^auth-dir: \"\\(.*\\)\"$/\\1/p' \"$3\")\n" +
		"printf '{\"access_token\":\"x\"}' > \"$dir/cred.json\"\n"
	if err := os.WriteFile(filepath.Join(bin, "cliproxyapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake itself shells out to sed, so PATH is the fake first and the
	// tools after, not the fake alone.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l := &launcher{dir: dir}
	if !l.providerLogin("demo", "codex") {
		t.Fatal("a finished sign-in under a root holding `[` was reported as no credential")
	}
	if _, err := os.Stat(filepath.Join(dir, "auth", "demo", "cred.json")); err != nil {
		t.Fatalf("the written credential was not found by the scan: %v", err)
	}
}
