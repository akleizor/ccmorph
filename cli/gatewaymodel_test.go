package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// gw862 is one gateway profile, gw862, on a fake route listing $LISTING
// (model-a and model-b unless a test overrides it), a stub claude, and a core
// that logs every call and refuses the adapter; HOME holds shared as
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
			"if [ \"$1\" = adapter ]; then\n"+
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
		if c := calls(); adapterStarted(c) {
			t.Fatalf("terminal=%v: the adapter started after a refusal:\n%s", terminal, c)
		}
	}
}

// readBack is a file's bytes, for "a read changed the file" checks.
func readBack(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// GH-862: doctor's FAIL model row says where the alias came from, in the same
// words run prints, and names the one command that picks a served model; the
// profile and the shared file keep their bytes.
func TestDoctorNamesWhereAnUnservedAliasCameFromIssue862(t *testing.T) {
	type tc struct {
		shared, extra, origin string
		ownLayer              bool
	}
	for _, c := range []tc{
		{shared: `{"model":"opus"}`, origin: "from ~/.claude/settings.json"},
		{shared: `{"env":{"ANTHROPIC_MODEL":"opus"}}`, origin: "from ~/.claude/settings.json"},
		{shared: `{"model":"opus[1m]"}`, origin: "from ~/.claude/settings.json"},
		{extra: "\nmodel=opus\nmodel_source=claude-settings\nmodel_defaults=1\n", origin: "from ~/.claude/settings.json"},
		{extra: "\nmodel=opus\n", origin: "saved in this profile"},
		{shared: `{"model":"sonnet"}`, ownLayer: true, origin: "saved in this profile"},
	} {
		t.Run(c.origin+c.shared+c.extra, func(t *testing.T) {
			state, profiles, _, _ := gw862(t, c.extra, c.shared)
			conf := filepath.Join(state, "gw862.conf")
			sharedPath := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
			if c.ownLayer {
				os.WriteFile(filepath.Join(profiles, "gw862", "settings.json"), []byte(`{"model":"opus"}`), 0o600)
			}
			confBefore, sharedBefore := readBack(t, conf), readBack(t, sharedPath)
			out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
			want := "FAIL model: " + util.UnservedTier("gw862", "opus", c.origin, nil)
			if !strings.Contains(out, want+"\n") {
				t.Fatalf("doctor did not print the row\nwant: %s\ngot:\n%s", want, out)
			}
			if end := "FAIL not ready: " + util.UnservedTierFix("gw862"); !strings.HasSuffix(strings.TrimSpace(out), end) {
				t.Fatalf("doctor's verdict\ngot: %s\nwant ending: %s", out, end)
			}
			if rc != 1 {
				t.Fatalf("rc %d, want 1:\n%s", rc, out)
			}
			if got := readBack(t, conf); got != confBefore {
				t.Fatalf("doctor changed the conf:\n%s\n->\n%s", confBefore, got)
			}
			if got := readBack(t, sharedPath); got != sharedBefore {
				t.Fatalf("doctor changed ~/.claude/settings.json:\n%s\n->\n%s", sharedBefore, got)
			}
		})
	}
	t.Run("the profile maps the tier", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=model-a\n", `{"model":"opus"}`)
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "FAIL model:") {
			t.Fatalf("a mapped tier is no fault:\n%s", out)
		}
		if end := "OK ready: start it with ccmorph run gw862"; !strings.HasSuffix(strings.TrimSpace(out), end) || rc != 0 {
			t.Fatalf("want rc 0 ending %q, got rc %d:\n%s", end, rc, out)
		}
	})
}

// GH-862: an excluded_models that removes every served model fails doctor with
// the same refusal run prints; one model left stays ready; a body that is not a
// listing reports no exclusions row.
func TestDoctorFailsWhenExcludedModelsLeaveNoModelIssue862(t *testing.T) {
	t.Run("every model excluded", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "\nexcluded_models=model-a,model-b\n", "")
		conf := filepath.Join(state, "gw862.conf")
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		want := "FAIL model: " + util.ExclusionsRefusal("gw862", conf)
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("doctor did not print the row\nwant: %s\ngot:\n%s", want, out)
		}
		end := "FAIL not ready: " + util.ExclusionsFix("gw862", conf)
		if !strings.HasSuffix(strings.TrimSpace(out), end) || rc != 1 {
			t.Fatalf("want rc 1 ending %q, got rc %d:\n%s", end, rc, out)
		}
	})
	t.Run("one model left", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "\nexcluded_models=model-a\n", "")
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "FAIL model:") {
			t.Fatalf("one model left is startable (the picker offers it):\n%s", out)
		}
		if end := "OK ready: start it with ccmorph run gw862"; !strings.HasSuffix(strings.TrimSpace(out), end) || rc != 0 {
			t.Fatalf("want rc 0 ending %q, got rc %d:\n%s", end, rc, out)
		}
	})
	t.Run("empty listing", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "\nexcluded_models=model-a,model-b\n", "")
		t.Setenv("LISTING", `{"data":[]}`)
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if !strings.Contains(out, "FAIL model: "+exclusionsRefusal(state)) || rc != 1 {
			t.Fatalf("want the exclusions FAIL, got rc %d:\n%s", rc, out)
		}
	})
	t.Run("a body that is not a listing", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "\nexcluded_models=model-a,model-b\n", "")
		t.Setenv("LISTING", `{"error":"x"}`)
		out, _ := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "excluded_models") {
			t.Fatalf("a body that is not a listing cannot judge the exclusions:\n%s", out)
		}
	})
}

func exclusionsRefusal(state string) string {
	return util.ExclusionsRefusal("gw862", filepath.Join(state, "gw862.conf"))
}

// GH-862: a gateway profile with no saved model and no known protocol stays
// ready — from a terminal, run asks first — but says so, in the adapter's own
// refusal sentence.
func TestDoctorNotesAGatewayWithNoSavedModelIssue862(t *testing.T) {
	t.Run("plain listing", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "", "")
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		want := "INFO model: none saved; in a terminal, ccmorph run asks which model first, and without one it stops: " +
			util.NoModelReason("gw862")
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("doctor did not note the stop\nwant: %s\ngot:\n%s", want, out)
		}
		if end := "OK ready: start it with ccmorph run gw862"; !strings.HasSuffix(strings.TrimSpace(out), end) || rc != 0 {
			t.Fatalf("want rc 0 ending %q, got rc %d:\n%s", end, rc, out)
		}
	})
	t.Run("the listing declares a protocol", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "", "")
		t.Setenv("LISTING", `{"data":[{"id":"model-a","supported_endpoints":["/v1/messages"]},{"id":"model-b"}]}`)
		out, _ := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "INFO model:") {
			t.Fatalf("a declared protocol decides the first launch:\n%s", out)
		}
	})
	t.Run("the adapter cache knows a model", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "", "")
		sum := sha256.Sum256([]byte("https://gw.test"))
		dir := filepath.Join(profiles, ".adapter-cache")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cache := `{"endpoint":"https://gw.test","anthropic_path":"/v1/messages","thinking_probe":2,` +
			`"rows":[{"model":"model-a","dialect":"anthropic","note":"","cap":null,"nothink":false,"id_recycle":false,"status":200}]}`
		if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+".json"), []byte(cache), 0o600); err != nil {
			t.Fatal(err)
		}
		out, _ := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "INFO model:") {
			t.Fatalf("a cached protocol decides the first launch:\n%s", out)
		}
	})
	t.Run("a saved model", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "\nmodel=model-a\n", "")
		out, _ := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "INFO model:") {
			t.Fatalf("a saved model is not the no-model case:\n%s", out)
		}
	})
	t.Run("a body that is not a listing", func(t *testing.T) {
		state, profiles, _, _ := gw862(t, "", "")
		t.Setenv("LISTING", `{"error":"x"}`)
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if strings.Contains(out, "INFO model:") {
			t.Fatalf("a body that is not a listing cannot judge the first launch:\n%s", out)
		}
		if !strings.Contains(out, "FAIL gateway: malformed model listing (HTTP 200)\n") || rc != 1 {
			t.Fatalf("doctor's malformed-listing FAIL changed, rc %d:\n%s", rc, out)
		}
	})
}

// GH-862: without a terminal the launch refuses an unmapped alias with the
// alias, its source, the fix and a few served ids, before any adapter starts.
func TestRunWithoutATerminalNamesAnUnservedAliasIssue862(t *testing.T) {
	for _, tc := range []struct {
		name, extra, shared, origin string
		argv                        []string
	}{
		{"saved alias", "", `{"model":"opus"}`, "from ~/.claude/settings.json", nil},
		{"typed alias", "", `{"model":"opus"}`, "given with --model", []string{"--model", "opus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, profiles, libexec, calls := gw862(t, tc.extra, tc.shared)
			rc, out := launchAs(t, false, state, profiles, libexec, "gw862", tc.argv)
			want := "ccmorph: " + util.UnservedTier("gw862", "opus", tc.origin, []string{"model-a", "model-b"})
			if !strings.Contains(out, want) {
				t.Fatalf("stderr does not name the alias, its source, the fix and the served ids\nwant: %s\ngot:\n%s", want, out)
			}
			if rc != 1 {
				t.Fatalf("rc %d, want 1:\n%s", rc, out)
			}
			if c := calls(); adapterStarted(c) {
				t.Fatalf("the adapter started after the refusal:\n%s", c)
			}
		})
	}
	t.Run("the profile maps the tier", func(t *testing.T) {
		state, profiles, libexec, calls := gw862(t, "\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=model-a\n", `{"model":"opus"}`)
		_, out := launchAs(t, false, state, profiles, libexec, "gw862", nil)
		if strings.Contains(out, "is a Claude model alias") {
			t.Fatalf("a mapped tier starts the session it names:\n%s", out)
		}
		c := calls()
		if !adapterStarted(c) || !strings.Contains(c, "model-a") {
			t.Fatalf("the launch did not start the adapter with the mapped id:\n%s", c)
		}
	})
}

// fakeFzf862 puts first on PATH an fzf that writes its arguments to argsFile
// and its input rows to sawFile, then cancels (exit 130). GH-862.
func fakeFzf862(t *testing.T) (argsFile, sawFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, sawFile = filepath.Join(dir, "args"), filepath.Join(dir, "saw")
	body := "#!/bin/sh\n" +
		"[ \"$1\" = --version ] && { echo 0.50.0; exit 0; }\n" +
		"printf '%s\\n' \"$@\" > \"$FZF_ARGS\"\n" +
		"cat > \"$FZF_SAW\"\n" +
		"exit 130\n"
	if err := os.WriteFile(filepath.Join(dir, "fzf"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FZF_ARGS", argsFile)
	t.Setenv("FZF_SAW", sawFile)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return argsFile, sawFile
}

// GH-862: in a terminal the launch says why the saved alias cannot start, then
// opens the picker on the served ids — no Keep row for the alias, the heading
// naming it — and the pick, or its cancel, changes nothing on disk.
func TestRunInATerminalOffersThePickerForAnUnservedAliasIssue862(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		state, profiles, libexec, calls := gw862(t, "", `{"model":"opus"}`)
		t.Setenv("TERM", "xterm-256color")
		sharedPath := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
		sharedBefore := readBack(t, sharedPath)
		argsFile, sawFile := fakeFzf862(t)
		rc, out := launchAs(t, true, state, profiles, libexec, "gw862", nil)
		if rc != 1 {
			t.Fatalf("rc %d, want 1:\n%s", rc, out)
		}
		want := "ccmorph: " + util.UnservedTier("gw862", "opus", "from ~/.claude/settings.json", nil)
		if !strings.Contains(out, want) {
			t.Fatalf("stderr does not say why before the picker\nwant: %s\ngot:\n%s", want, out)
		}
		saw := readBack(t, sawFile)
		if !strings.Contains(saw, "model-a") || !strings.Contains(saw, "model-b") {
			t.Fatalf("the picker did not open on the served ids:\n%s", saw)
		}
		if strings.Contains(saw, "@keep") {
			t.Fatalf("the picker offers the alias that cannot start:\n%s", saw)
		}
		if args := readBack(t, argsFile); !strings.Contains(args, "Model — gw862 (opus is not served here)") {
			t.Fatalf("the picker heading does not name the alias:\n%s", args)
		}
		if c := calls(); adapterStarted(c) {
			t.Fatalf("the adapter started after a cancelled picker:\n%s", c)
		}
		f := map[string]string{}
		for _, line := range strings.Split(readBack(t, filepath.Join(state, "gw862.conf")), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				f[k] = v
			}
		}
		if f["model"] != "opus" || f["model_source"] != "claude-settings" {
			t.Fatalf("the launch changed the saved model or lost its source: %v", f)
		}
		if got := readBack(t, sharedPath); got != sharedBefore {
			t.Fatalf("~/.claude/settings.json changed:\n%s\n->\n%s", sharedBefore, got)
		}
	})
	t.Run("a served saved model opens no picker", func(t *testing.T) {
		state, profiles, libexec, _ := gw862(t, "\nmodel=model-a\nmodel_defaults=1\n", "")
		t.Setenv("TERM", "xterm-256color")
		_, sawFile := fakeFzf862(t)
		launchAs(t, true, state, profiles, libexec, "gw862", nil)
		if paths.Exists(sawFile) {
			t.Fatal("the picker opened although the saved model is served")
		}
	})
	t.Run("a mapped tier opens no picker", func(t *testing.T) {
		state, profiles, libexec, calls := gw862(t, "\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=model-a\n", `{"model":"opus"}`)
		t.Setenv("TERM", "xterm-256color")
		_, sawFile := fakeFzf862(t)
		_, out := launchAs(t, true, state, profiles, libexec, "gw862", nil)
		if paths.Exists(sawFile) {
			t.Fatal("the picker opened although the profile maps opus")
		}
		if strings.Contains(out, "is a Claude model alias") {
			t.Fatalf("a mapped tier was refused:\n%s", out)
		}
		if c := calls(); !adapterStarted(c) || !strings.Contains(c, "model-a") {
			t.Fatalf("the launch did not start the adapter with the mapped id:\n%s", c)
		}
	})
}

// countingCurl puts first on PATH a curl that answers like gw862's and logs one
// line per call; the func it returns counts the calls so far. GH-862.
func countingCurl(t *testing.T) func() int {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "curl.log")
	body := "#!/bin/sh\necho call >> '" + log + "'\n" +
		"for a do case $a in -K) cat >/dev/null ;; esac; done\n" +
		"printf '%s' \"$LISTING\"\n" +
		"for a do case $a in -w) printf '\\n__CCMORPH_STATUS__200' ;; esac; done\n" +
		"printf '\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "call\n")
	}
}

// GH-862: the served ids come from the listing the launch already holds. A body
// that is not a listing, or an empty one, names no ids, and the refusal never
// sends a request of its own.
func TestTheAliasRefusalSendsNoRequestOfItsOwnIssue862(t *testing.T) {
	state, profiles, libexec, _ := gw862(t, "", `{"model":"opus"}`)
	count := countingCurl(t)
	_, out := launchAs(t, false, state, profiles, libexec, "gw862", nil)
	if !strings.Contains(out, "(it serves model-a, model-b)") {
		t.Fatalf("a good listing names no served ids:\n%s", out)
	}
	good := count()
	for _, listing := range []string{`{"error":"x"}`, `{"data":[]}`, "garbage"} {
		t.Run(listing, func(t *testing.T) {
			state, profiles, libexec, calls := gw862(t, "", `{"model":"opus"}`)
			t.Setenv("LISTING", listing)
			count := countingCurl(t)
			rc, out := launchAs(t, false, state, profiles, libexec, "gw862", nil)
			want := "ccmorph: " + util.UnservedTier("gw862", "opus", "from ~/.claude/settings.json", nil)
			if rc != 1 || !strings.Contains(out, want) {
				t.Fatalf("rc %d, want 1 with %q:\n%s", rc, want, out)
			}
			if strings.Contains(out, "(it serves") {
				t.Fatalf("ids were named from a listing that gives none:\n%s", out)
			}
			if got := count(); got != good {
				t.Fatalf("the refusal sent its own request: %d curl calls, %d with a good listing", got, good)
			}
			if adapterStarted(calls()) {
				t.Fatalf("the adapter started after the refusal:\n%s", calls())
			}
		})
	}
}

// GH-862: one model left after excluded_models is no refusal, with or without a
// terminal; an empty listing with exclusions is refused in both, in one sentence.
func TestRunWithExclusionsThatLeaveAModelOrMeetAnEmptyListingIssue862(t *testing.T) {
	t.Run("one model left", func(t *testing.T) {
		state, profiles, libexec, calls := gw862(t, "\nexcluded_models=model-a\n", "")
		_, out := launchAs(t, false, state, profiles, libexec, "gw862", nil)
		if strings.Contains(out, "no cached or served picker models remain") {
			t.Fatalf("no terminal: one model left was refused:\n%s", out)
		}
		if !adapterStarted(calls()) {
			t.Fatalf("no terminal: the launch stopped before the adapter:\n%s\n%s", out, calls())
		}
		t.Setenv("TERM", "xterm-256color")
		_, sawFile := fakeFzf862(t)
		_, out = launchAs(t, true, state, profiles, libexec, "gw862", nil)
		if strings.Contains(out, "no cached or served picker models remain") {
			t.Fatalf("terminal: one model left was refused:\n%s", out)
		}
		if !paths.Exists(sawFile) {
			t.Fatalf("terminal: the picker did not open on the model left:\n%s", out)
		}
	})
	t.Run("an empty listing", func(t *testing.T) {
		state, profiles, libexec, calls := gw862(t, "\nexcluded_models=model-a,model-b\n", "")
		t.Setenv("LISTING", `{"data":[]}`)
		for _, terminal := range []bool{false, true} {
			if terminal {
				t.Setenv("TERM", "xterm-256color")
			}
			rc, out := launchAs(t, terminal, state, profiles, libexec, "gw862", nil)
			if rc != 1 || !strings.Contains(out, "ccmorph: "+exclusionsRefusal(state)) {
				t.Fatalf("terminal=%v: rc %d, want 1 with the exclusions refusal:\n%s", terminal, rc, out)
			}
		}
		if adapterStarted(calls()) {
			t.Fatalf("the adapter started after a refusal:\n%s", calls())
		}
	})
}

// GH-862: wherever run stops before a session, doctor fails with the same cause
// and the same fix, with and without a terminal, and nothing starts an adapter.
func TestDoctorAndRunNameTheSameCauseAndFixIssue862(t *testing.T) {
	for _, tc := range []struct {
		name, extra, shared, cause string
		fix                        func(conf string) string
	}{
		{"alias", "", `{"model":"opus"}`,
			"opus, from ~/.claude/settings.json, is a Claude model alias this gateway does not serve",
			func(string) string { return util.UnservedTierFix("gw862") }},
		{"exclusions", "\nexcluded_models=model-a,model-b\n", "",
			"no cached or served picker models remain after excluded_models, which removes every model this gateway serves",
			func(conf string) string { return util.ExclusionsFix("gw862", conf) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, profiles, libexec, calls := gw862(t, tc.extra, tc.shared)
			fix := tc.fix(filepath.Join(state, "gw862.conf"))
			out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
			row := ""
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "FAIL model: ") {
					row = line
				}
			}
			if !strings.Contains(row, tc.cause) || !strings.Contains(row, fix) {
				t.Fatalf("doctor's FAIL model row %q lacks the cause %q or the fix %q:\n%s", row, tc.cause, fix, out)
			}
			if !strings.HasSuffix(strings.TrimSpace(out), "\nFAIL not ready: "+fix) || rc != 1 {
				t.Fatalf("doctor rc %d, want 1 ending %q:\n%s", rc, "FAIL not ready: "+fix, out)
			}
			for _, terminal := range []bool{false, true} {
				if terminal {
					t.Setenv("TERM", "xterm-256color")
					fakeFzf862(t)
				}
				rc, stderr := launchAs(t, terminal, state, profiles, libexec, "gw862", nil)
				if rc != 1 || !strings.Contains(stderr, tc.cause) || !strings.Contains(stderr, fix) {
					t.Fatalf("terminal=%v: run rc %d, want 1 with the cause %q and the fix %q:\n%s", terminal, rc, tc.cause, fix, stderr)
				}
			}
			if c := calls(); adapterStarted(c) {
				t.Fatalf("a launch started the adapter:\n%s", c)
			}
		})
	}
}

// GH-862 review: a key Claude Code was told to refuse is the launch's first
// stop. The automatic alias picker must not ask the gateway with that key or
// present model advice in place of keyApproval's login repair.
func TestRunRefusedKeyDoesNotOpenTheAliasPickerIssue862(t *testing.T) {
	state, profiles, libexec, calls := gw862(t, "", `{"model":"opus"}`)
	l := &launcher{dir: state, profileDirs: profiles}
	f, ok := l.readRaw("gw862")
	if !ok {
		t.Fatal("the fixture conf could not be read")
	}
	tail := f["api_key"]
	if len(tail) > 20 {
		tail = tail[len(tail)-20:]
	}
	if err := os.WriteFile(filepath.Join(profiles, "gw862", ".claude.json"),
		[]byte(`{"customApiKeyResponses":{"approved":[],"rejected":["`+tail+`"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "xterm-256color")
	_, sawFile := fakeFzf862(t)
	count := countingCurl(t)
	rc, out := launchAs(t, true, state, profiles, libexec, "gw862", nil)
	if rc != 1 {
		t.Fatalf("rc %d, want 1:\n%s", rc, out)
	}
	if want := keyRefusedMsg("gw862"); !strings.Contains(out, want) {
		t.Fatalf("the launch lost the login repair\nwant: %s\ngot:\n%s", want, out)
	}
	if strings.Contains(out, "is a Claude model alias") {
		t.Fatalf("the picker's model advice buried the login repair:\n%s", out)
	}
	if paths.Exists(sawFile) {
		t.Fatal("the alias picker opened for a key the person refused")
	}
	if got := count(); got != 0 {
		t.Fatalf("the launch sent %d listing request(s) with a refused key", got)
	}
	if c := calls(); adapterStarted(c) {
		t.Fatalf("an adapter started:\n%s", c)
	}
}

// GH-862 review: the launch stops at the picker before an adapter starts, so
// doctor's verdict names the exclusions repair, not a translator install that
// cannot clear that stop. The adapter row still reports its own fault.
func TestDoctorExclusionsOutrankTheAdapterRowIssue862(t *testing.T) {
	state, profiles, _, _ := gw862(t, "\nexcluded_models=model-a,model-b\n", "")
	t.Setenv("LISTING", `{"data":[{"id":"model-a","supported_endpoints":["/v1/chat/completions"]},{"id":"model-b","supported_endpoints":["/v1/chat/completions"]}]}`)
	old := translatorInstalled
	translatorInstalled = func() bool { return false }
	t.Cleanup(func() { translatorInstalled = old })
	conf := filepath.Join(state, "gw862.conf")
	out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
	exRow := "FAIL model: " + util.ExclusionsRefusal("gw862", conf)
	adRow := "FAIL adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: " + util.ClipInstallStep()
	if !strings.Contains(out, exRow+"\n") || !strings.Contains(out, adRow+"\n") {
		t.Fatalf("doctor lost a row\nwant model row: %s\nwant adapter row: %s\ngot:\n%s", exRow, adRow, out)
	}
	if strings.Index(out, exRow) > strings.Index(out, adRow) {
		t.Fatalf("the adapter row came before the launch's first stop:\n%s", out)
	}
	if rc != 1 || !strings.HasSuffix(strings.TrimSpace(out), "FAIL not ready: "+util.ExclusionsFix("gw862", conf)) {
		t.Fatalf("doctor rc %d, want 1 ending with the exclusions repair:\n%s", rc, out)
	}
}

// GH-862 review: a listing that names no chat model, for a profile with no
// saved model, stops every terminal launch at the picker, and with nothing
// cached the launch without a terminal too. Doctor fails with run's cause and
// repair, and no launch that stops opens a picker or starts an adapter.
func TestDoctorAndRunStopOnAListingWithNoModelToPickIssue862(t *testing.T) {
	for _, listing := range []string{
		`{"data":[]}`,
		`{"data":[{"id":"emb","capabilities":{"chat_completion":false}}]}`,
	} {
		t.Run(listing, func(t *testing.T) {
			state, profiles, libexec, calls := gw862(t, "", "")
			t.Setenv("LISTING", listing)
			conf := filepath.Join(state, "gw862.conf")
			before := readBack(t, conf)
			out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
			if !strings.Contains(out, "FAIL model: "+util.NoUsableModel("gw862")+"\n") || strings.Contains(out, "INFO model:") {
				t.Fatalf("doctor does not name the empty choice in run's words:\n%s", out)
			}
			if rc != 1 || !strings.HasSuffix(strings.TrimSpace(out), "\nFAIL not ready: "+util.NoUsableModelFix("gw862")) {
				t.Fatalf("doctor rc %d, want 1 ending %q:\n%s", rc, "FAIL not ready: "+util.NoUsableModelFix("gw862"), out)
			}
			if got := readBack(t, conf); got != before {
				t.Fatalf("doctor changed the conf:\n%s\n->\n%s", before, got)
			}
			for _, terminal := range []bool{false, true} {
				if terminal {
					t.Setenv("TERM", "xterm-256color")
				}
				_, sawFile := fakeFzf862(t)
				runRC, stderr := launchAs(t, terminal, state, profiles, libexec, "gw862", nil)
				if runRC != 1 || !strings.Contains(stderr, "ccmorph: "+util.NoUsableModel("gw862")) {
					t.Fatalf("terminal=%v: run rc %d, want 1 with %q:\n%s", terminal, runRC, util.NoUsableModel("gw862"), stderr)
				}
				if paths.Exists(sawFile) {
					t.Fatalf("terminal=%v: a picker opened without a row to pick", terminal)
				}
			}
			if c := calls(); adapterStarted(c) {
				t.Fatalf("an adapter started with no model to serve:\n%s", c)
			}
		})
	}
	t.Run("a cached model", func(t *testing.T) {
		// The cache lets a launch without a terminal start, but the terminal
		// launch still stops at the empty picker: doctor is not ready.
		state, profiles, libexec, calls := gw862(t, "", "")
		t.Setenv("LISTING", `{"data":[]}`)
		sum := sha256.Sum256([]byte("https://gw.test"))
		cache := filepath.Join(profiles, ".adapter-cache", hex.EncodeToString(sum[:])[:16]+".json")
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cache, []byte(`{"endpoint":"https://gw.test","anthropic_path":"/v1/messages","thinking_probe":2,"rows":[{"model":"model-a","dialect":"anthropic","note":"","cap":null,"nothink":false,"id_recycle":false,"status":200}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		out, rc := doctorOutput(t, &launcher{dir: state, profileDirs: profiles}, "gw862")
		if rc != 1 || !strings.Contains(out, "FAIL model: "+util.NoUsableModel("gw862")+"\n") ||
			!strings.HasSuffix(strings.TrimSpace(out), "\nFAIL not ready: "+util.NoUsableModelFix("gw862")) {
			t.Fatalf("doctor rc %d; a terminal launch stops at the empty picker:\n%s", rc, out)
		}
		_, stderr := launchAs(t, false, state, profiles, libexec, "gw862", nil)
		if strings.Contains(stderr, util.NoUsableModel("gw862")) || !adapterStarted(calls()) {
			t.Fatalf("no terminal: a cached model was refused before the adapter:\n%s\n%s", stderr, calls())
		}
		t.Setenv("TERM", "xterm-256color")
		_, sawFile := fakeFzf862(t)
		runRC, stderr := launchAs(t, true, state, profiles, libexec, "gw862", nil)
		if runRC != 1 || !strings.Contains(stderr, "ccmorph: "+util.NoUsableModel("gw862")) || paths.Exists(sawFile) {
			t.Fatalf("terminal: run rc %d, want 1 with %q and no picker:\n%s", runRC, util.NoUsableModel("gw862"), stderr)
		}
	})
}

// adapterStarted is whether a stub core's call log shows a launch starting the
// adapter: a call whose first argument is `adapter`.
func adapterStarted(calls string) bool {
	return strings.HasPrefix(calls, "adapter\n") || strings.Contains(calls, ".\nadapter\n")
}
