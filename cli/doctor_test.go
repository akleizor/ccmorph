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
// profile's saved model can be a spelling launch translates through model_map
// before classifying it. Doctor asked the adapter about the saved spelling, so
// the adapter row was decided for a model the launch never classifies: the
// listing declared only the mapped id as needing the translator, doctor found
// no verdict for the saved spelling, printed no adapter row, and called the
// profile ready — then the run died at adapter start. The row must be decided
// for the id launch classifies, for a bare saved model, for the `[1m]` spelling
// the map is keyed without, and for a mapped id carrying a bracket launch keeps
// (only the exact `[1m]` marker is ccmorph's own).
func TestDoctorTranslatorVerdictUsesTheModelLaunchClassifies(t *testing.T) {
	for _, tc := range []struct{ name, saved, mapped string }{
		{"saved spelling", "m-spelled", "m-mapped"},
		{"saved window marker", "m-spelled[1m]", "m-mapped"},
		{"mapped id keeps another bracket", "m-spelled", "m-mapped[fast]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			conf := "type=apikey\nbase_url=https://gw.test\napi_key=test-key\n" +
				"model=" + tc.saved + "\nmodel_map={\"m-spelled\":\"" + tc.mapped + "\"}\n"
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
			step := util.ClipInstallStep()
			want := "FAIL adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: " + step
			if !strings.Contains(out, want) {
				t.Fatalf("doctor did not report the adapter row for the mapped model\nwant row: %s\ngot:\n%s", want, out)
			}
			if verdict := "FAIL not ready: install the translator: " + step; !strings.HasSuffix(strings.TrimSpace(out), verdict) {
				t.Fatalf("doctor's verdict does not name the translator\nwant ending: %s\ngot:\n%s", verdict, out)
			}
			if rc == 0 {
				t.Fatalf("doctor exited 0 with a failing adapter row:\n%s", out)
			}
		})
	}
}
