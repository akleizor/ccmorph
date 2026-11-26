package util

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/ordjson"
)

// holdLiveLock takes the profile's live lock the way a whole-file writer or a
// live session holds it, on a descriptor apart from the package's own, and
// returns its release. flock locks belong to the open file description, so two
// descriptors of one process conflict exactly as two processes do; the GH-938
// probe held its lock in the same process for this reason.
func holdLiveLock(t *testing.T, profiles, name string, how int) func() {
	t.Helper()
	lock := LiveLockPath(profiles, name)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return func() {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// GH-938: provisioning is the whole-file writer of a profile's config.json and
// .claude.json that the live lock premise missed, so a launch rewrote both
// under a live session and a read that happened before the rename was silently
// lost. Each file's read-to-rename window now holds the live lock exclusively;
// a holder that outlasts the bounded wait leaves both files untouched and the
// error wraps ErrProfileInUse.
func TestProvisionProfileRespectsTheLiveLock(t *testing.T) {
	const cfgSeed = `{"oauth:tokenCache":{"accessToken":"t"},"numStartups":3}`
	const claudeSeed = `{"oauthAccount":{"emailAddress":"someone@example"}}`
	cases := []struct {
		name    string
		how     int
		cfg     string
		claude  string
		trailer string
	}{
		{
			name:   "both files would be rewritten under an exclusive holder",
			how:    syscall.LOCK_EX,
			cfg:    cfgSeed,
			claude: claudeSeed,
		},
		{
			name:   "both files would be rewritten under a live session's shared lock",
			how:    syscall.LOCK_SH,
			cfg:    cfgSeed,
			claude: claudeSeed,
		},
		{
			name:   "only .claude.json would be rewritten",
			how:    syscall.LOCK_EX,
			cfg:    `{"numStartups":3}`,
			claude: claudeSeed,
		},
		{
			name: "a missing .claude.json would be seeded",
			how:  syscall.LOCK_EX,
			cfg:  `{"numStartups":3}`,
		},
		{
			name:    "a profile dir spelled with a trailing separator",
			how:     syscall.LOCK_EX,
			cfg:     cfgSeed,
			claude:  claudeSeed,
			trailer: string(filepath.Separator),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			profiles := filepath.Join(home, "profiles")
			dir := filepath.Join(profiles, "probe")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfg, []byte(tc.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			claudeJSON := filepath.Join(dir, ".claude.json")
			if tc.claude != "" {
				if err := os.WriteFile(claudeJSON, []byte(tc.claude), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			release := holdLiveLock(t, profiles, "probe", tc.how)
			restore := SetLockWait(0)
			defer restore()

			err := provisionProfile(dir+tc.trailer, "gateway")
			if !errors.Is(err, ErrProfileInUse) {
				t.Fatalf("provisioning under a held live lock must wrap ErrProfileInUse (got %v)", err)
			}
			if got := mustRead(t, cfg); got != tc.cfg {
				t.Fatalf("config.json was rewritten under the live lock: %s", got)
			}
			if tc.claude == "" {
				if _, err := os.Lstat(claudeJSON); !os.IsNotExist(err) {
					t.Fatalf(".claude.json was seeded under the live lock (err %v)", err)
				}
			} else if got := mustRead(t, claudeJSON); got != tc.claude {
				t.Fatalf(".claude.json was rewritten under the live lock: %s", got)
			}

			// Convergence: the skip leaves nothing to retry by hand; the next
			// launch, after the holder ends, provisions as usual.
			release()
			if err := provisionProfile(dir, "gateway"); err != nil {
				t.Fatalf("the next launch must provision once the holder is gone: %v", err)
			}
			doc, err := loadObj(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if doc == nil || doc.Has("oauth:tokenCache") || doc.Has("oauth:tokenCacheV2") {
				t.Fatalf("config.json was not auth-scoped after the holder ended: %s", mustRead(t, cfg))
			}
			doc, err = loadObj(claudeJSON, nil)
			if err != nil {
				t.Fatal(err)
			}
			if doc == nil || doc.Has("oauthAccount") {
				t.Fatalf(".claude.json was not auth-scoped after the holder ended: %s", mustRead(t, claudeJSON))
			}
		})
	}
}

// GH-938: the lock is not taken around the whole provision, but over each
// file's read-to-rename window. At that seam — after the locked read, before
// the rename — a second writer must find the lock held exclusively, and a
// field already in the file must survive the auth scoping it is not about.
func TestProvisionProfileHoldsTheLiveLockAcrossReadAndRename(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, "profiles")
	dir := filepath.Join(profiles, "probe")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	claudeJSON := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(cfg, []byte(`{"oauth:tokenCache":{"accessToken":"t"},"numStartups":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeJSON, []byte(`{"oauthAccount":{"emailAddress":"someone@example"},"numStartups":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := profileJSONRead
	defer func() { profileJSONRead = old }()
	var seen []string
	profileJSONRead = func(path string) {
		seen = append(seen, filepath.Base(path))
		probe, err := os.OpenFile(LiveLockPath(profiles, "probe"), os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("the live lock must exist at the seam for %s: %v", path, err)
		}
		defer probe.Close()
		if syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
			t.Fatalf("the live lock was not held exclusively at the read-to-rename seam for %s", path)
		}
	}

	if err := provisionProfile(dir, "gateway"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"config.json", ".claude.json"}; len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("provisioning wrote through an unlocked window: seams seen %v, want %v", seen, want)
	}
	doc, err := loadObj(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Has("oauth:tokenCache") || !ordjson.Truthy(getKey(doc, "numStartups")) {
		t.Fatalf("config.json must lose the auth key and keep the field provisioning is not about: %s", mustRead(t, cfg))
	}
	doc, err = loadObj(claudeJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Has("oauthAccount") || !ordjson.Truthy(getKey(doc, "numStartups")) {
		t.Fatalf(".claude.json must lose the auth key and keep the field provisioning is not about: %s", mustRead(t, claudeJSON))
	}
}

// GH-938: SetupProfile's own exclusive live lock (#185) covers the shared
// history merge that runs after provisioning. A provisioning window nested
// inside it must leave the descriptor exclusive, or the merge runs under a
// lock another launch can share.
func TestSetupProfileKeepsExclusiveLiveLockAcrossProvisioning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	dir := filepath.Join(profiles, "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A real profile-local CLAUDE.md is an override, logged in the shared-store
	// loop after provisioning: a seam the test opens across the merge.
	if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("canonical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	claudeJSON := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(cfg, []byte(`{"oauth:tokenCache":{"accessToken":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeJSON, []byte(`{"oauthAccount":{"emailAddress":"someone@example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ReleaseLive(profiles, "p") })
	restore := SetLockWait(0)
	defer restore()

	checked := false
	if !SetupProfile(profiles, "p", "gateway",
		func(s string) {
			if !strings.Contains(s, "OVERRIDE profile-local 'CLAUDE.md'") {
				return
			}
			checked = true
			probe, err := os.OpenFile(LiveLockPath(profiles, "p"), os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("the live lock must exist before the merge: %v", err)
			}
			defer probe.Close()
			if syscall.Flock(int(probe.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
				syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
				t.Fatal("provisioning downgraded SetupProfile's exclusive live lock before the history merge")
			}
		},
		func(s string) { t.Fatalf("setup refused: %s", s) }) {
		t.Fatal("setup must succeed")
	}
	if !checked {
		t.Fatal("the shared-store seam never ran; the exclusive hold was not checked")
	}
}

// GH-938: a launch finding the profile busy must still start. SetupProfile
// logs the skipped provisioning, naming the file and the reason, and goes on;
// any other provisioning error still fails the launch as before.
func TestSetupProfileSkipsProvisioningWhileTheLiveLockIsHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	dir := filepath.Join(profiles, "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const cfgSeed = `{"oauth:tokenCache":{"accessToken":"t"}}`
	const claudeSeed = `{"oauthAccount":{"emailAddress":"someone@example"}}`
	cfg := filepath.Join(dir, "config.json")
	claudeJSON := filepath.Join(dir, ".claude.json")
	for path, body := range map[string]string{cfg: cfgSeed, claudeJSON: claudeSeed} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release := holdLiveLock(t, profiles, "p", syscall.LOCK_EX)
	restore := SetLockWait(0)
	defer restore()

	var logs, errs []string
	if !SetupProfile(profiles, "p", "gateway",
		func(s string) { logs = append(logs, s) },
		func(s string) { errs = append(errs, s) }) {
		t.Fatalf("a busy launch must continue, not fail: %v", errs)
	}
	if len(errs) != 0 {
		t.Fatalf("a busy launch must report no refusal: %v", errs)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "provisioning of 'p' skipped") {
		t.Fatalf("the skipped provisioning must be logged: %v", logs)
	}
	if !strings.Contains(joined, "config.json") {
		t.Fatalf("the skip must name the file it left alone: %v", logs)
	}
	if got := mustRead(t, cfg); got != cfgSeed {
		t.Fatalf("config.json was rewritten while the live lock was held: %s", got)
	}
	if got := mustRead(t, claudeJSON); got != claudeSeed {
		t.Fatalf(".claude.json was rewritten while the live lock was held: %s", got)
	}
	// Convergence is the next launch, not a marker: no state records the skip.
	entries, err := os.ReadDir(filepath.Join(home, ".config/ccmorph/.locks"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "p.live" || names[1] != "p.lock" {
		t.Fatalf("the skip must leave no retry state behind: .locks holds %v", names)
	}
	release()

	// Every other provisioning error still fails the launch.
	broken := filepath.Join(profiles, "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "config.json"), []byte(`[1]`), 0o600); err != nil {
		t.Fatal(err)
	}
	errs = nil
	if SetupProfile(profiles, "broken", "gateway",
		func(string) {},
		func(s string) { errs = append(errs, s) }) {
		t.Fatal("a real provisioning error must still fail the launch")
	}
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "profile setup failed for 'broken'") {
		t.Fatalf("a real provisioning error must be reported: %v", errs)
	}

	// A symlinked .claude.json is refused the same way, not skipped.
	linked := filepath.Join(profiles, "linked")
	if err := os.MkdirAll(linked, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	errs = nil
	if SetupProfile(profiles, "linked", "gateway",
		func(string) {},
		func(s string) { errs = append(errs, s) }) {
		t.Fatal("a symlinked .claude.json must fail the launch")
	}
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "profile setup failed for 'linked'") {
		t.Fatalf("a symlinked .claude.json must be reported: %v", errs)
	}
}

// GH-938: contention skips the rewrite, never the validation. A busy launch
// must still refuse an invalid auth file — a symlink or unparseable JSON —
// instead of reading the whole provisioning as the harmless skip and starting
// claude over it. Both files are validated before either lock is taken, so a
// config.json that cannot be rewritten no longer hides a bad .claude.json.
func TestSetupProfileRefusesInvalidAuthFilesWhileTheLiveLockIsHeld(t *testing.T) {
	cases := []struct {
		name    string
		invalid func(t *testing.T, home, dir string)
		want    string
	}{
		{
			name: "a symlinked .claude.json",
			invalid: func(t *testing.T, home, dir string) {
				target := filepath.Join(t.TempDir(), "elsewhere.json")
				if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, ".claude.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "symbolic links are not allowed",
		},
		{
			name: "an unparseable .claude.json",
			invalid: func(t *testing.T, home, dir string) {
				if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`[1]`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "root is not a JSON object",
		},
		{
			name: "an unparseable base .claude.json the profile would be seeded from",
			invalid: func(t *testing.T, home, dir string) {
				if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`[1]`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "root is not a JSON object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			profiles := filepath.Join(home, ".config/ccmorph/profiles")
			dir := filepath.Join(profiles, "p")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// config.json needs a rewrite, so the first file's lock is the one
			// contention loses; .claude.json is never reached without the
			// upfront validation.
			const cfgSeed = `{"oauth:tokenCache":{"accessToken":"t"}}`
			cfg := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfg, []byte(cfgSeed), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.invalid(t, home, dir)
			release := holdLiveLock(t, profiles, "p", syscall.LOCK_SH)
			restore := SetLockWait(0)
			defer restore()

			err := provisionProfile(dir, "gateway")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("contention must not hide an invalid auth file: want %q, got %v", tc.want, err)
			}
			if got := mustRead(t, cfg); got != cfgSeed {
				t.Fatalf("config.json was rewritten while refusing the invalid .claude.json: %s", got)
			}

			var errs []string
			if SetupProfile(profiles, "p", "gateway",
				func(string) {},
				func(s string) { errs = append(errs, s) }) {
				t.Fatalf("a busy launch must not start over an invalid auth file: %v", errs)
			}
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "profile setup failed for 'p'") {
				t.Fatalf("the refusal must be reported: %v", errs)
			}
			release()
		})
	}
}

// GH-938: a busy launch's skipped rewrite must be retried by the next launch
// from the auth files' actual state. ProfileOK's existence checks cannot see
// that the files still hold the fields provisioning deletes; ProvisionOK can,
// and setupDir asks it before it skips setup.
func TestProvisionOKSeesAuthFilesStillNeedingProvisioning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	dir := filepath.Join(profiles, "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ReleaseLive(profiles, "p") })
	restore := SetLockWait(0)
	defer restore()

	var logs, errs []string
	logf := func(s string) { logs = append(logs, s) }
	errf := func(s string) { errs = append(errs, s) }
	if !SetupProfile(profiles, "p", "gateway", logf, errf) {
		t.Fatalf("the first launch must succeed: %v", errs)
	}
	if !ProfileOK(profiles, "p") {
		t.Fatal("a completed setup must read as ProfileOK")
	}
	if !ProvisionOK(profiles, "p", "gateway") {
		t.Fatal("provisioned auth files must read as ProvisionOK")
	}

	cfg := filepath.Join(dir, "config.json")
	claudeJSON := filepath.Join(dir, ".claude.json")
	const cfgSeed = `{"oauth:tokenCache":{"accessToken":"t"}}`
	const claudeSeed = `{"oauthAccount":{"emailAddress":"someone@example"}}`
	for path, body := range map[string]string{cfg: cfgSeed, claudeJSON: claudeSeed} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A missing shared link makes this setup run and repair the shared layer
	// while the live lock is held: whether the auth files are retried must not
	// depend on the shared layer still needing work.
	if err := os.Remove(filepath.Join(dir, "skills")); err != nil {
		t.Fatal(err)
	}
	release := holdLiveLock(t, profiles, "p", syscall.LOCK_SH)
	logs = nil
	if !SetupProfile(profiles, "p", "gateway", logf, errf) {
		t.Fatalf("a busy launch must continue: %v", errs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "provisioning of 'p' skipped") {
		t.Fatalf("the busy setup must log the skipped provisioning: %v", logs)
	}
	release()

	if !ProfileOK(profiles, "p") {
		t.Fatal("the busy setup repaired the shared layer; ProfileOK must be true")
	}
	if ProvisionOK(profiles, "p", "gateway") {
		t.Fatal("the auth files still hold oauth fields; the next launch must not skip setup")
	}

	logs, errs = nil, nil
	if !SetupProfile(profiles, "p", "gateway", logf, errf) {
		t.Fatalf("the next uncontended launch must succeed: %v", errs)
	}
	if !ProvisionOK(profiles, "p", "gateway") {
		t.Fatal("the next launch must have provisioned the auth files")
	}
	if doc, err := loadObj(cfg, nil); err != nil || doc == nil || doc.Has("oauth:tokenCache") {
		t.Fatalf("config.json was not auth-scoped by the retry (err %v): %s", err, mustRead(t, cfg))
	}
	if doc, err := loadObj(claudeJSON, nil); err != nil || doc == nil || doc.Has("oauthAccount") {
		t.Fatalf(".claude.json was not auth-scoped by the retry (err %v): %s", err, mustRead(t, claudeJSON))
	}
}

// GH-938: `add` and `use` go on without the live lock by policy (#217), so a
// lock that cannot be made at all skips the write with its real cause named,
// never turns into a failed verb and never blames "another launch".
func TestSetupProfileSkipsProvisioningWhenTheLiveLockCannotBeMade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	if err := os.MkdirAll(filepath.Join(profiles, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/ccmorph/.locks"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := SetLockWait(0)
	defer restore()

	var logs, errs []string
	if !SetupProfile(profiles, "p", "gateway",
		func(s string) { logs = append(logs, s) },
		func(s string) { errs = append(errs, s) }) {
		t.Fatalf("a verb that goes on without the lock must not fail: %v", errs)
	}
	if len(errs) != 0 {
		t.Fatalf("a skipped provisioning must report no refusal: %v", errs)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"provisioning of 'p' skipped", "could not be created"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the skip must name the lock it could not make (%q missing): %v", want, logs)
		}
	}
	if strings.Contains(joined, "another launch") {
		t.Fatalf("a lock that cannot be made is not another launch: %v", logs)
	}
	for _, name := range []string{"config.json", ".claude.json"} {
		if _, err := os.Lstat(filepath.Join(profiles, "p", name)); !os.IsNotExist(err) {
			t.Fatalf("nothing may be provisioned without the live lock (%s exists, err %v)", name, err)
		}
	}
}
