package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/util"
)

// doctorOutput runs doctor for one profile and returns what it printed with its
// exit status; doctor's report is stdout, so the caller can read the rows the
// same way a person does.
func doctorOutput(t *testing.T, l *launcher, name string) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	rc := l.doctor(name, false)
	w.Close()
	os.Stdout = old
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), rc
}

// TestDoctorTranslatorVerdictUsesTheModelLaunchClassifies is GH-412: a gateway
// profile's saved model can be a spelling launch resolves or translates before
// classifying it. Doctor asked the adapter about the saved spelling, so the
// adapter row was decided for a model the launch never classifies: the listing
// declared only the mapped id as needing the translator, doctor found no
// verdict for the saved spelling, printed no adapter row, and called the
// profile ready — then the run died at adapter start. The row must be decided
// for the id launch classifies: a bare saved model, the `[1m]` spelling the map
// is keyed without, a mapped id carrying a bracket launch keeps (only the exact
// `[1m]` marker is ccmorph's own), and a tier word launch resolves through the
// profile's alias (#475). A tier word with no mapping anywhere is launch's own
// refusal, and must be a failing row, never an unresolved model's silence.
func TestDoctorTranslatorVerdictUsesTheModelLaunchClassifies(t *testing.T) {
	for _, tc := range []struct {
		name, saved, mapped, mapping, alias string
		tierMissing                         bool
		// emptyMapping writes the env mapping set but with no value, the edge
		// the three spellings of the resolution answered three ways (#517).
		emptyMapping bool
	}{
		{"saved spelling", "m-spelled", "m-mapped", `"m-spelled":"m-mapped"`, "", false, false},
		{"saved window marker", "m-spelled[1m]", "m-mapped", `"m-spelled":"m-mapped"`, "", false, false},
		{"mapped id keeps another bracket", "m-spelled", "m-mapped[fast]", `"m-spelled":"m-mapped[fast]"`, "", false, false},
		{"tier word resolved through the profile alias", "opus", "m-mapped", "", "m-mapped", false, false},
		{"tier word with no mapping", "opus", "m-other", "", "", true, false},
		{"tier word whose env mapping is set but empty", "opus", "m-other", "", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			conf := "type=apikey\nbase_url=https://gw.test\napi_key=test-key\n" +
				"model=" + tc.saved + "\n"
			if tc.mapping != "" {
				conf += "model_map={" + tc.mapping + "}\n"
			}
			if tc.alias != "" || tc.emptyMapping {
				conf += "env.ANTHROPIC_DEFAULT_OPUS_MODEL=" + tc.alias + "\n"
			}
			if err := os.WriteFile(filepath.Join(state, "gw412.conf"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			// The gateway lists only the mapped id, and declares it as speaking
			// the chat dialect, which no native route serves: only the mapped id
			// can decide that a translator is needed.
			bin := t.TempDir()
			curl := "#!/bin/sh\nprintf '%s\\n' '" +
				`{"data":[{"id":"` + tc.mapped + `","supported_endpoints":["/v1/chat/completions"]}]}` +
				"' '__CCMORPH_STATUS__200'\n"
			for _, exe := range []struct{ name, body string }{
				{"curl", curl},
				{"claude", "#!/bin/sh\nexit 0\n"},
			} {
				if err := os.WriteFile(filepath.Join(bin, exe.name), []byte(exe.body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// No brew either, so the install step the row names is the same one
			// the report computed.
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			t.Setenv("HOME", t.TempDir())
			old := translatorInstalled
			translatorInstalled = func() bool { return false }
			t.Cleanup(func() { translatorInstalled = old })

			l := &launcher{dir: state, profileDirs: filepath.Join(t.TempDir(), "profiles")}
			out, rc := doctorOutput(t, l, "gw412")
			if tc.alias != "" {
				// The effective model is the id a launch resolves the tier word
				// to, so the reader is told what the profile really runs (#475).
				if want := "effective model: " + tc.alias + " (profile default)"; !strings.Contains(out, want) {
					t.Fatalf("doctor did not name the resolved model\nwant row: %s\ngot:\n%s", want, out)
				}
			}
			step := util.ClipInstallStep()
			want := "FAIL adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: " + step
			verdict := "FAIL not ready: install the translator: " + step
			if tc.tierMissing {
				want = "FAIL model: no gateway model is mapped to tier 'opus'; a launch would stop at adapter start"
				verdict = "FAIL not ready: choose an explicit model: ccmorph set gw412 model <model-id>"
			}
			if !strings.Contains(out, want) {
				t.Fatalf("doctor did not report the verdict for the model launch runs\nwant row: %s\ngot:\n%s", want, out)
			}
			if !strings.HasSuffix(strings.TrimSpace(out), verdict) {
				t.Fatalf("doctor's verdict does not name the model launch runs\nwant ending: %s\ngot:\n%s", verdict, out)
			}
			if rc == 0 {
				t.Fatalf("doctor exited 0 with a failing adapter row:\n%s", out)
			}
		})
	}
}

// GH-449: the per-model effort a new session starts at is keyed by the model
// Claude Code sees, and a 1M claude model is sent under its [1m] send id.
// Doctor asked the store for the saved spelling, missed the override and
// reported the profile's default instead, where the launch — which asks by the
// send id — passes the override. A native route is sent without ccmorph's send
// id, so the launch keys that lookup by the saved spelling and the override
// must not apply there (#449).
func TestDoctorShowsTheEffortSavedForTheModelsSendID(t *testing.T) {
	for _, tc := range []struct {
		name, base, want string
	}{
		{"gateway uses the send id", "https://gw.test", "effective effort: high (profile override for claude-sonnet-4-5[1m])"},
		{"native route keeps the saved spelling", "https://api.anthropic.com", "effective effort: low (profile default)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			conf := "type=apikey\nbase_url=" + tc.base + "\napi_key=test-key\n" +
				"model=claude-sonnet-4-5\n" +
				"context_window.claude-sonnet-4-5=1000000\n" +
				"effort=low\n" +
				"effort.claude-sonnet-4-5[1m]=high\n"
			if err := os.WriteFile(filepath.Join(state, "gw449.conf"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			for _, exe := range []struct{ name, body string }{
				{"curl", "#!/bin/sh\nprintf '%s\\n' '{\"data\":[]}' '__CCMORPH_STATUS__200'\n"},
				{"claude", "#!/bin/sh\nexit 0\n"},
			} {
				if err := os.WriteFile(filepath.Join(bin, exe.name), []byte(exe.body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			t.Setenv("HOME", t.TempDir())
			l := &launcher{dir: state, profileDirs: filepath.Join(t.TempDir(), "profiles")}
			out, _ := doctorOutput(t, l, "gw449")
			if !strings.Contains(out, tc.want) {
				t.Fatalf("doctor did not report the effort a launch passes\nwant row: %s\ngot:\n%s", tc.want, out)
			}
		})
	}
}

// GH-556: doctor answers the effort row from the state a launch would have
// migrated first. A profile still carrying its discovered window in the legacy
// cache read a bare send id, so doctor missed the [1m] override and reported
// the profile default where `ccmorph context` — which migrates before it reads
// — and the launch resolve the [1m] id. The row must name the same send id the
// migrated state answers.
func TestDoctorEffortRowMigratesLegacyCacheIssue556(t *testing.T) {
	state := t.TempDir()
	profiles := filepath.Join(state, "profiles")
	cache := filepath.Join(profiles, "gw556", "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	// The pre-models.json layout kept the discovered window in this cache; only
	// migration moves it to profiles/gw556/models.json, where the read sees it.
	if err := os.WriteFile(filepath.Join(cache, "context-window"), []byte("claude-sonnet-4-5\t1000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := "type=apikey\nbase_url=https://gw.test\napi_key=test-key\n" +
		"model=claude-sonnet-4-5\n" +
		"effort=low\n" +
		"effort.claude-sonnet-4-5[1m]=high\n"
	if err := os.WriteFile(filepath.Join(state, "gw556.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	securityLog := filepath.Join(t.TempDir(), "security-calls")
	for _, exe := range []struct{ name, body string }{
		{"curl", "#!/bin/sh\nprintf '%s\\n' '{\"data\":[]}' '__CCMORPH_STATUS__200'\n"},
		{"claude", "#!/bin/sh\nexit 0\n"},
		// A working Keychain double: if doctor ran the credential migration, it
		// would answer both calls and leave a trace here.
		{"security", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SECURITY_LOG\"\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(bin, exe.name), []byte(exe.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	core := coreCopy(t)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SECURITY_LOG", securityLog)
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "")
	l := &launcher{dir: state, profileDirs: profiles, libexec: filepath.Dir(core)}
	out, _ := doctorOutput(t, l, "gw556")
	if want := "effective effort: high (profile override for claude-sonnet-4-5[1m])"; !strings.Contains(out, want) {
		t.Fatalf("doctor did not migrate the legacy window before keying the effort row\nwant row: %s\ngot:\n%s", want, out)
	}
	// Doctor's only migration is the context one: the plaintext credential stays
	// where it was, so no other profile's conf is rewritten and the Keychain is
	// never touched (#556).
	if confText, err := os.ReadFile(filepath.Join(state, "gw556.conf")); err != nil || !strings.Contains(string(confText), "api_key=test-key") {
		t.Fatalf("doctor rewrote the conf outside the context migration: %q, %v", confText, err)
	}
	if _, err := os.Stat(securityLog); !os.IsNotExist(err) {
		t.Fatalf("doctor ran the credential migration: %v", err)
	}
	// The state doctor answered from is the one ccmorph context reads: the same
	// migrated send id the row above named.
	record, errs, rc := l.core(l.windowEnv("gw556"), "", "model-window", "--context", "gw556", "claude-sonnet-4-5")
	if rc != 0 {
		t.Fatalf("context read failed (rc %d): %s", rc, errs)
	}
	if !strings.Contains(record, `"send_id":"claude-sonnet-4-5[1m]"`) {
		t.Fatalf("context read %q, want the send id the doctor row named", record)
	}
}

// A doctor that names no profile and finds none selected still reports the
// environment a fresh install can check: an empty default is "no profile
// selected" to doctor, its own reading of the shared read — never the
// launch's refusal of an invalid active file.
func TestDoctorReportsTheEnvironmentWhenTheDefaultIsEmpty(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	l := &launcher{dir: state, profileDirs: filepath.Join(t.TempDir(), "profiles"), libDir: filepath.Join(t.TempDir(), "lib")}
	out, rc := doctorOutput(t, l, "")
	if rc != 0 {
		t.Fatalf("an empty default with no profiles must report the environment, rc=%d:\n%s", rc, out)
	}
	if !strings.Contains(out, "claude executable") {
		t.Fatalf("the environment report must still check claude:\n%s", out)
	}
	if strings.Contains(out, "invalid active profile") {
		t.Fatalf("an empty default is no profile selected, never a refusal:\n%s", out)
	}
}

// doctor is read-only: it reads the default through the same read the launch
// stands on, but the re-tighten to 0600 is readActive's write, not its own —
// a doctor that tightened what it only read would break the promise
// ARCHITECTURE.md makes for it.
func TestDoctorLeavesTheActiveFilesModeAlone(t *testing.T) {
	state := t.TempDir()
	active := filepath.Join(state, "active")
	if err := os.WriteFile(active, []byte("ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile filters its mode through the process umask, so a 077 host
	// would create this 0600 and the assertion below could never pass. Chmod
	// pins the mode the fixture means to start from, whatever the host's umask.
	if err := os.Chmod(active, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	l := &launcher{dir: state, profileDirs: filepath.Join(t.TempDir(), "profiles")}
	out, rc := doctorOutput(t, l, "")
	if rc == 0 || !strings.Contains(out, "ghost") {
		t.Fatalf("a default naming no profile must keep its missing-profile diagnostic:\n%s", out)
	}
	fi, err := os.Lstat(active)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("doctor must leave the active file's mode alone, got %v", fi.Mode().Perm())
	}
}

// An invalid default is refused as written by doctor's own badName: the case
// twin rescue never reads the Kelvin sign (U+212A) as the profile k.
func TestDoctorRefusesAnInvalidDefaultBeforeAnyCaseRescue(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "k.conf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "active"), []byte("\u212A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	l := &launcher{dir: state, profileDirs: filepath.Join(t.TempDir(), "profiles")}
	out, rc := doctorOutput(t, l, "")
	if rc != 1 || out != "" {
		t.Fatalf("an invalid default must be refused before any check row, rc=%d:\n%s", rc, out)
	}
}
