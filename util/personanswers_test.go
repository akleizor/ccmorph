package util

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// answeredDoc is the .claude.json of a profile the person answered Claude
// Code's first-run questions on, in folder: onboarding done, a theme chosen,
// the folder trusted. extra is spliced in at the top level.
func answeredDoc(t *testing.T, folder, theme, extra string) string {
	t.Helper()
	return `{"hasCompletedOnboarding":true,"theme":"` + theme + `","projects":{"` + folder +
		`":{"hasTrustDialogAccepted":true,"hasCompletedProjectOnboarding":true}}` + extra + `}`
}

// checkAnswers asserts the doc at path holds the person's answers for folder —
// and nothing an account carries: the answers a person gives cross profiles,
// their login never does.
func checkAnswers(t *testing.T, path, folder, theme string) {
	t.Helper()
	doc, err := loadObj(path, nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if !truthy(getKey(doc, "hasCompletedOnboarding")) {
		t.Fatalf("onboarding not carried: %s", mustRead(t, path))
	}
	if got, _ := getKey(doc, "theme").(string); got != theme {
		t.Fatalf("theme = %q, want %q: %s", got, theme, mustRead(t, path))
	}
	entry, err := folderEntry(doc, folder)
	if err != nil || entry == nil {
		t.Fatalf("no trust entry for %s: %s (%v)", folder, mustRead(t, path), err)
	}
	for _, k := range []string{"hasTrustDialogAccepted", "hasCompletedProjectOnboarding"} {
		if !truthy(getKey(entry, k)) {
			t.Fatalf("%s not carried: %s", k, mustRead(t, path))
		}
	}
	for _, k := range []string{"oauthAccount", "hasAvailableSubscription"} {
		if doc.Has(k) {
			t.Fatalf("%s crossed from a source: %s", k, mustRead(t, path))
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSeedPersonAnswersFillsEveryGapFromAnySource(t *testing.T) {
	cases := []struct {
		name   string
		source func(t *testing.T, home, profiles, folder string) string
	}{
		{
			name: "another profile",
			source: func(t *testing.T, home, profiles, folder string) string {
				return writeClaudeJSON(t, filepath.Join(profiles, "sibling"),
					answeredDoc(t, folder, "dark", `,"oauthAccount":{"email":"person@example.invalid"}`))
			},
		},
		{
			name: "the base install",
			source: func(t *testing.T, home, profiles, folder string) string {
				return writeClaudeJSON(t, home, answeredDoc(t, folder, "dark", ""))
			},
		},
		{
			name: "the base install's fallback path",
			source: func(t *testing.T, home, profiles, folder string) string {
				return writeClaudeJSON(t, filepath.Join(home, ".claude"), answeredDoc(t, folder, "dark", ""))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			profiles := filepath.Join(t.TempDir(), "profiles")
			target := writeClaudeJSON(t, filepath.Join(profiles, "target"), `{"numStartups":3}`)
			folder := resolved(filepath.Join(t.TempDir(), "work"))
			source := tc.source(t, home, profiles, folder)
			sourceBefore := mustRead(t, source)

			gaps, err := SeedPersonAnswers(profiles, "target", folder)

			if err != nil {
				t.Fatalf("seeding failed: %v", err)
			}
			if gaps.Onboarding || gaps.Trust {
				t.Fatalf("a filled profile still reports gaps: %+v in %s", gaps, mustRead(t, target))
			}
			checkAnswers(t, target, folder, "dark")
			if got := mustRead(t, target); !strings.Contains(got, `"numStartups":3`) {
				t.Fatalf("the target's own keys did not survive: %s", got)
			}
			if after := mustRead(t, source); after != sourceBefore {
				t.Fatalf("the source was rewritten: %s", after)
			}
		})
	}
}

func TestSeedPersonAnswersAnswersAProfileWithNoFileYet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	writeClaudeJSON(t, filepath.Join(profiles, "sibling"), answeredDoc(t, folder, "dark", ""))
	target := filepath.Join(profiles, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}

	gaps, err := SeedPersonAnswers(profiles, "target", folder)

	if err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	if gaps.Onboarding || gaps.Trust {
		t.Fatalf("a filled profile still reports gaps: %+v", gaps)
	}
	checkAnswers(t, filepath.Join(target, ".claude.json"), folder, "dark")
}

func TestSeedPersonAnswersKeepsThePersonsOwnChoices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	sibling := writeClaudeJSON(t, filepath.Join(profiles, "sibling"), answeredDoc(t, folder, "dark", ""))
	siblingBefore := mustRead(t, sibling)

	t.Run("an answered profile is not rewritten", func(t *testing.T) {
		body := `{"hasCompletedOnboarding":true,"theme":"light","numStartups":3,"projects":{"` + folder +
			`":{"hasTrustDialogAccepted":true,"allowedTools":["Bash"]}}}`
		target := writeClaudeJSON(t, filepath.Join(profiles, "target"), body)
		gaps, err := SeedPersonAnswers(profiles, "target", folder)
		if err != nil {
			t.Fatal(err)
		}
		if gaps.Onboarding || gaps.Trust {
			t.Fatalf("an answered profile reports gaps: %+v", gaps)
		}
		if after := mustRead(t, target); after != body {
			t.Fatalf("an answered profile was rewritten: %s", after)
		}
	})

	t.Run("a theme chosen on the profile keeps its value", func(t *testing.T) {
		target := writeClaudeJSON(t, filepath.Join(profiles, "target"), `{"hasCompletedOnboarding":true,"theme":"light"}`)
		gaps, err := SeedPersonAnswers(profiles, "target", folder)
		if err != nil {
			t.Fatal(err)
		}
		if gaps.Onboarding || gaps.Trust {
			t.Fatalf("a filled profile reports gaps: %+v in %s", gaps, mustRead(t, target))
		}
		checkAnswers(t, target, folder, "light")
	})

	t.Run("a false answer is the unanswered state, not a choice", func(t *testing.T) {
		target := writeClaudeJSON(t, filepath.Join(profiles, "target"),
			`{"hasCompletedOnboarding":true,"projects":{"`+folder+`":{"hasTrustDialogAccepted":false,"allowedTools":["Bash"]}}}`)
		gaps, err := SeedPersonAnswers(profiles, "target", folder)
		if err != nil {
			t.Fatal(err)
		}
		if gaps.Onboarding || gaps.Trust {
			t.Fatalf("a filled profile reports gaps: %+v in %s", gaps, mustRead(t, target))
		}
		doc, err := loadObj(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := folderEntry(doc, folder)
		if err != nil || entry == nil {
			t.Fatalf("no trust entry: %s (%v)", mustRead(t, target), err)
		}
		if !truthy(getKey(entry, "hasTrustDialogAccepted")) || !truthy(getKey(entry, "hasCompletedProjectOnboarding")) {
			t.Fatalf("the trust answer was not carried: %s", mustRead(t, target))
		}
		if tools, ok := getKey(entry, "allowedTools").([]any); !ok || len(tools) != 1 {
			t.Fatalf("the entry's own keys did not survive: %s", mustRead(t, target))
		}
	})

	if after := mustRead(t, sibling); after != siblingBefore {
		t.Fatalf("the source profile was rewritten: %s", after)
	}
}

func TestSeedPersonAnswersPicksTheNewestTheme(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	older := writeClaudeJSON(t, filepath.Join(profiles, "aaa"), answeredDoc(t, folder, "old-theme", ""))
	newer := writeClaudeJSON(t, filepath.Join(profiles, "zzz"), answeredDoc(t, folder, "new-theme", ""))
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	target := writeClaudeJSON(t, filepath.Join(profiles, "target"), `{}`)

	gaps, err := SeedPersonAnswers(profiles, "target", folder)

	if err != nil {
		t.Fatal(err)
	}
	if gaps.Onboarding || gaps.Trust {
		t.Fatalf("a filled profile reports gaps: %+v", gaps)
	}
	checkAnswers(t, target, folder, "new-theme")
	if got := mustRead(t, older); !strings.Contains(got, "old-theme") {
		t.Fatalf("the older source lost its theme: %s", got)
	}
	if got := mustRead(t, newer); !strings.Contains(got, "new-theme") {
		t.Fatalf("the newer source lost its theme: %s", got)
	}
}

func TestSeedPersonAnswersPrefersTheBaseInstallsOwnFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	writeClaudeJSON(t, filepath.Join(home, ".claude"), answeredDoc(t, folder, "fallback-theme", ""))
	writeClaudeJSON(t, home, answeredDoc(t, folder, "base-theme", ""))
	target := writeClaudeJSON(t, filepath.Join(profiles, "target"), `{}`)

	gaps, err := SeedPersonAnswers(profiles, "target", folder)

	if err != nil {
		t.Fatal(err)
	}
	if gaps.Onboarding || gaps.Trust {
		t.Fatalf("a filled profile reports gaps: %+v", gaps)
	}
	checkAnswers(t, target, folder, "base-theme")
}

func TestSeedPersonAnswersRefusesWhatItCannotSafelyRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	writeClaudeJSON(t, filepath.Join(profiles, "sibling"), answeredDoc(t, folder, "dark", ""))
	cases := []struct{ name, body string }{
		{"a non-object projects tree", `{"projects":"not-an-object"}`},
		{"a non-object folder entry", `{"projects":{"` + folder + `":"not-an-object"}}`},
		{"unparsable JSON", `{"theme":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := writeClaudeJSON(t, filepath.Join(profiles, "target"), tc.body)
			gaps, err := SeedPersonAnswers(profiles, "target", folder)
			if err == nil {
				t.Fatal("a file that cannot be safely rewritten must be refused")
			}
			if !gaps.Onboarding || !gaps.Trust {
				t.Fatalf("a refused file must read as unanswered: %+v", gaps)
			}
			if after := mustRead(t, target); after != tc.body {
				t.Fatalf("the file was rewritten: %s", after)
			}
		})
	}
	// A symlinked .claude.json is never followed into a rewrite.
	symlinked := filepath.Join(t.TempDir(), "profiles")
	if err := os.MkdirAll(filepath.Join(symlinked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(writeClaudeJSON(t, t.TempDir(), `{}`), filepath.Join(symlinked, "x", ".claude.json")); err != nil {
		t.Skip("no symlink support")
	}
	if gaps, err := SeedPersonAnswers(symlinked, "x", folder); err == nil || !gaps.Onboarding || !gaps.Trust {
		t.Fatalf("a symlinked .claude.json must be refused with the gaps unread: %+v %v", gaps, err)
	}
	// A name that is not a profile name names no profile to seed.
	if gaps, err := SeedPersonAnswers(profiles, "active", folder); err == nil || !gaps.Onboarding || !gaps.Trust {
		t.Fatalf("an invalid profile name must be refused with the gaps unread: %+v %v", gaps, err)
	}
}

func TestSeedPersonAnswersDefersWhileALiveSessionHoldsTheLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	writeClaudeJSON(t, filepath.Join(profiles, "sibling"), answeredDoc(t, folder, "dark", ""))
	body := `{"hasCompletedOnboarding":true}`
	target := writeClaudeJSON(t, filepath.Join(profiles, "target"), body)

	// A live session of the same profile holds the live lock shared; it may be
	// writing its own answers, so the seeding defers to the next launch.
	lock := LiveLockPath(profiles, "target")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	gaps, err := SeedPersonAnswers(profiles, "target", folder)

	if err != nil {
		t.Fatalf("contention must defer, not fail: %v", err)
	}
	if gaps.Onboarding || !gaps.Trust {
		t.Fatalf("a deferred seeding must report the gaps it left: %+v", gaps)
	}
	if after := mustRead(t, target); after != body {
		t.Fatalf("a deferred seeding rewrote the file: %s", after)
	}
}

// #185, #205: the rewrite runs only holding the profile's live lock. When the
// lock cannot be taken at all — another launch's merge holds it exclusively and
// outlasts the wait — the answers wait for the next launch; seeding must not
// rewrite the file unlocked.
func TestSeedPersonAnswersWithoutTheLiveLockLeavesTheAnswersForLater(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	writeClaudeJSON(t, filepath.Join(profiles, "sibling"), answeredDoc(t, folder, "dark", ""))
	body := `{"hasCompletedOnboarding":true}`
	target := writeClaudeJSON(t, filepath.Join(profiles, "target"), body)

	// Another launch holds the live lock exclusively, as its merge does; the
	// wait expires while it still holds it.
	lock := LiveLockPath(profiles, "target")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	restore := SetLockWait(0)
	defer restore()

	gaps, err := SeedPersonAnswers(profiles, "target", folder)

	if err != nil {
		t.Fatalf("a lock that cannot be taken must defer, not fail: %v", err)
	}
	if gaps.Onboarding || !gaps.Trust {
		t.Fatalf("an unlocked seeding must report the gaps it left: %+v", gaps)
	}
	if after := mustRead(t, target); after != body {
		t.Fatalf("a seeding with no live lock rewrote the file: %s", after)
	}
}

func TestSeedPersonAnswersWithoutSourcesLeavesTheQuestions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(t.TempDir(), "profiles")
	folder := resolved(filepath.Join(t.TempDir(), "work"))
	target := writeClaudeJSON(t, filepath.Join(profiles, "target"), `{}`)

	gaps, err := SeedPersonAnswers(profiles, "target", folder)

	if err != nil {
		t.Fatalf("no sources is not a failure: %v", err)
	}
	if !gaps.Onboarding || !gaps.Trust {
		t.Fatalf("the questions must still be named: %+v", gaps)
	}
	if after := mustRead(t, target); after != `{}` {
		t.Fatalf("the file was rewritten from nothing: %s", after)
	}
}
