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
	}{
		{"saved spelling", "m-spelled", "m-mapped", `"m-spelled":"m-mapped"`, "", false},
		{"saved window marker", "m-spelled[1m]", "m-mapped", `"m-spelled":"m-mapped"`, "", false},
		{"mapped id keeps another bracket", "m-spelled", "m-mapped[fast]", `"m-spelled":"m-mapped[fast]"`, "", false},
		{"tier word resolved through the profile alias", "opus", "m-mapped", "", "m-mapped", false},
		{"tier word with no mapping", "opus", "m-other", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			conf := "type=apikey\nbase_url=https://gw.test\napi_key=test-key\n" +
				"model=" + tc.saved + "\n"
			if tc.mapping != "" {
				conf += "model_map={" + tc.mapping + "}\n"
			}
			if tc.alias != "" {
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
