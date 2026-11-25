package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/util"
)

// gw862 is one gateway profile, gw862, on a fake route listing $LISTING
// (model-a and model-b unless a test overrides it), a stub claude, and a core
// that logs every call and refuses adapter-start; HOME holds shared as
// ~/.claude/settings.json ("" for none). GH-862.
func gw862(t *testing.T, extraConf, shared string) (state, profiles, libexec string, calls func() string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CCMORPH_SUBCOMMAND_LAUNCH", "0")
	t.Setenv("LISTING", `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
	if shared != "" {
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(shared), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, profiles = gatewayProfile(t, "gw862", "type=apikey\nbase_url=https://gw.test\napi_key=test-key\ncontext_window.model-a=200000\ncontext_window.model-b=200000\n"+extraConf)
	bin := t.TempDir()
	for _, exe := range []struct{ name, body string }{
		{"claude", "#!/bin/sh\nexit 0\n"},
		{"curl", "#!/bin/sh\n" +
			"for a do case $a in -K) cat >/dev/null ;; esac; done\n" +
			"printf '%s' \"$LISTING\"\n" +
			"for a do case $a in -w) printf '\\n__CCMORPH_STATUS__200' ;; esac; done\n" +
			"printf '\\n'\n"},
	} {
		if err := os.WriteFile(filepath.Join(bin, exe.name), []byte(exe.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	libexec = t.TempDir()
	log := filepath.Join(libexec, "calls.log")
	if err := os.WriteFile(filepath.Join(libexec, "ccmorph-core"), []byte(
		"#!/bin/sh\n{ for a do printf '%s\\n' \"$a\"; done; printf '.\\n'; } >> '"+log+"'\n"+
			"if [ \"$1\" = util ] && [ \"$2\" = adapter-start ]; then\n"+
			"  echo \"ccmorph: 'gw862': adapter exited before readiness; session not started.\" >&2\n"+
			"  exit 1\nfi\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls = func() string {
		b, _ := os.ReadFile(log)
		return string(b)
	}
	old := translatorInstalled
	translatorInstalled = func() bool { return true }
	t.Cleanup(func() { translatorInstalled = old })
	return state, profiles, libexec, calls
}

// GH-862: an excluded_models that removes every model the endpoint serves stops
// the launch. Both passes name the list and its fix; neither blames the
// endpoint, which answered, and neither starts an adapter.
func TestRunNamesExcludedModelsThatLeaveNothingIssue862(t *testing.T) {
	state, profiles, libexec, calls := gw862(t, "\nexcluded_models=model-a,model-b\n", "")
	conf := filepath.Join(state, "gw862.conf")
	want := "ccmorph: " + util.ExclusionsRefusal("gw862", conf)
	for _, terminal := range []bool{false, true} {
		if terminal {
			t.Setenv("TERM", "xterm-256color")
		}
		rc, out := launchAs(t, terminal, state, profiles, libexec, "gw862", nil)
		if rc != 1 {
			t.Fatalf("terminal=%v: rc %d, want 1:\n%s", terminal, rc, out)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("terminal=%v: stderr does not name the exclusions and their fix\nwant: %s\ngot:\n%s", terminal, want, out)
		}
		for _, never := range []string{"restore gateway access", "endpoint returned no usable model list"} {
			if strings.Contains(out, never) {
				t.Fatalf("terminal=%v: the gateway answered; the refusal still says %q:\n%s", terminal, never, out)
			}
		}
		if c := calls(); strings.Contains(c, "adapter-start") {
			t.Fatalf("terminal=%v: the adapter started after a refusal:\n%s", terminal, c)
		}
	}
}
