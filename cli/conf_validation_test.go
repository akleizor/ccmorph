package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two hand-edited confs GH-839 is about: one unknown key, one effort level
// the conf refuses. Raw literals so the fault is the line a person sees.
const (
	unknownKeyConf = `type=apikey
bogus_key=1
api_key={{SECRET_m4kq7v2t9xaa}}`
	invalidEffortConf = `type=apikey
effort=ultra
api_key={{SECRET_c8w3n6r1pzdd}}`
)

// readerOutput runs one reader with both channels captured: a verb's refusal is
// on stderr while doctor's report is on stdout, and a person reads the two as
// one answer.
func readerOutput(t *testing.T, f func() int) (string, int) {
	t.Helper()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	rc := f()
	os.Stdout, os.Stderr = oldOut, oldErr
	wo.Close()
	we.Close()
	ob, _ := io.ReadAll(ro)
	eb, _ := io.ReadAll(re)
	return string(ob) + string(eb), rc
}

// GH-839: one hand-edited conf line used to give each command its own answer —
// list hid the profile, get answered confidently, set/config froze on the
// store's unnamed error. Every reader now prints the one profile.Read refusal.
func TestEveryReaderNamesTheSameConfFault(t *testing.T) {
	for _, tc := range []struct{ name, conf, want string }{
		{"unknown key", unknownKeyConf, "unknown key 'bogus_key' in 'vp'"},
		{"invalid effort", invalidEffortConf,
			"invalid effort 'ultra' in 'vp': effort must be low, medium, high, xhigh or max"},
		{"both faults, key first", "type=apikey\nbogus_key=1\neffort=ultra\n", "unknown key 'bogus_key' in 'vp'"},
		{"both faults, effort first", "type=apikey\neffort=ultra\nbogus_key=1\n",
			"invalid effort 'ultra' in 'vp': effort must be low, medium, high, xhigh or max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CCMORPH_CONTEXT_LOCKED", "vp")
			l := storeTestLauncher(t, "vp", tc.conf, `{"env":{}}`)
			for _, reader := range []struct {
				name string
				run  func() int
			}{
				{"list", func() int { return l.list(nil) }},
				{"get model", func() int { return l.getSet("get", []string{"vp", "model"}) }},
				{"get base_url", func() int { return l.getSet("get", []string{"vp", "base_url"}) }},
				{"get --json", func() int { return l.getSet("get", []string{"vp", "--json"}) }},
				{"get api_key", func() int { return l.getSet("get", []string{"vp", "api_key"}) }},
				{"config", func() int { return l.config([]string{"vp"}) }},
				{"inspect", func() int { return l.inspect([]string{"vp"}) }},
				{"get effort", func() int { return l.getSet("get", []string{"vp", "effort"}) }},
				{"set model", func() int { return l.getSet("set", []string{"vp", "model", "new"}) }},
				{"doctor", func() int { return l.doctor("vp", false) }},
			} {
				out, rc := readerOutput(t, reader.run)
				if !strings.Contains(out, tc.want) {
					t.Errorf("%s did not name the conf's fault %q: rc %d out %q", reader.name, tc.want, rc, out)
				}
				if strings.Contains(out, "invalid saved effort default") {
					t.Errorf("%s still prints the old unnamed store error: %q", reader.name, out)
				}
				if strings.HasPrefix(reader.name, "get") &&
					(strings.Contains(out, "no model saved") || strings.Contains(out, "no effort saved")) {
					t.Errorf("%s answered confidently about an invalid profile: %q", reader.name, out)
				}
				if reader.name != "list" && rc == 0 {
					t.Errorf("%s accepted an invalid profile", reader.name)
				}
			}
		})
	}
}

// GH-839: list keeps skipping a profile it cannot read, but its skip line ends
// with the repair, and the "No profiles yet" summary never claims an empty home
// while a profile is being skipped.
func TestListSkipNamesTheRepairAndNeverClaimsAnEmptyHome(t *testing.T) {
	for _, tc := range []struct{ name, conf, repair string }{
		{"unknown key", unknownKeyConf, "remove the unknown key 'bogus_key'"},
		{"invalid effort", invalidEffortConf, "ccmorph set vp effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := storeTestLauncher(t, "vp", tc.conf, `{"env":{}}`)
			out, rc := readerOutput(t, func() int { return l.list(nil) })
			if rc != 0 {
				t.Fatalf("list refused the home: rc %d out %q", rc, out)
			}
			if !strings.Contains(out, "skipping invalid profile 'vp'") {
				t.Fatalf("list did not skip the invalid profile: %q", out)
			}
			if !strings.Contains(out, tc.repair) || !strings.Contains(out, "ccmorph rm vp") {
				t.Fatalf("the skip line does not name the repair (%q): %q", tc.repair, out)
			}
			if !strings.HasSuffix(strings.TrimSpace(out), "ccmorph rm vp") {
				t.Fatalf("the skip line does not end with the repair: %q", out)
			}
			if strings.Contains(out, "No profiles yet") {
				t.Fatalf("list called a home with a skipped profile empty: %q", out)
			}
		})
	}
	// A valid profile beside the invalid one is still listed, and the summary
	// still does not claim the home is empty.
	l := storeTestLauncher(t, "vp", unknownKeyConf, `{"env":{}}`)
	if err := os.WriteFile(filepath.Join(l.dir, "ok.conf"), []byte("type=sub\nemail=a@b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := readerOutput(t, func() int { return l.list(nil) })
	if rc != 0 || !strings.Contains(out, "ok (Claude account: a@b)") {
		t.Fatalf("list lost the valid profile: rc %d out %q", rc, out)
	}
	if strings.Contains(out, "No profiles yet") {
		t.Fatalf("list called a home with a valid profile empty: %q", out)
	}
}

// GH-839: overwriting the refused value with a legal level is the repair; the
// old pre-read refused the very save it was there to make.
func TestSetEffortOverwritesAnInvalidSavedEffort(t *testing.T) {
	t.Setenv("CCMORPH_CONTEXT_LOCKED", "vp")
	l := storeTestLauncher(t, "vp", invalidEffortConf, `{"env":{}}`)
	out, rc := captureStdout(t, func() int { return l.getSet("set", []string{"vp", "effort", "high"}) })
	if rc != 0 || strings.TrimSpace(out) != "high" {
		t.Fatalf("set effort high: rc %d out %q", rc, out)
	}
	if got, rc := l.configStoreQ("get", "vp", "effort"); rc != 0 || got != "high" {
		t.Fatalf("after the repair, effort = %q (rc %d)", got, rc)
	}
	// A model save does not repair the effort: the same refusal, and no write.
	l = storeTestLauncher(t, "vp", invalidEffortConf, `{"env":{}}`)
	out, rc = readerOutput(t, func() int { return l.getSet("set", []string{"vp", "model", "new"}) })
	if rc == 0 || !strings.Contains(out, "invalid effort 'ultra' in 'vp'") || !strings.Contains(out, "low, medium, high, xhigh or max") {
		t.Fatalf("set model with a bad saved effort: rc %d out %q", rc, out)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "vp.conf")); strings.Contains(string(b), "model=new") {
		t.Fatalf("the refused save wrote a model: %q", b)
	}
	// Nor does naming the refused level again repair it, so that set refuses
	// with the conf's own sentence and writes nothing.
	l = storeTestLauncher(t, "vp", invalidEffortConf, `{"env":{}}`)
	out, rc = readerOutput(t, func() int { return l.getSet("set", []string{"vp", "effort", "ultra"}) })
	if rc == 0 || !strings.Contains(out, "invalid effort 'ultra' in 'vp'") || !strings.Contains(out, "low, medium, high, xhigh or max") {
		t.Fatalf("set effort ultra over a bad saved effort: rc %d out %q", rc, out)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "vp.conf")); string(b) != invalidEffortConf {
		t.Fatalf("the refused set wrote: %q", b)
	}
}

// Doctor reads the profile through the same validator, so it names the same
// fault, and its verdict gives the repair list's skip line gives (GH-839).
func TestDoctorNamesTheConfFaultAndTheSameRepair(t *testing.T) {
	for _, tc := range []struct{ name, conf, fault, repair string }{
		{"unknown key", unknownKeyConf, "unknown key 'bogus_key' in 'vp'",
			"not ready: remove the unknown key 'bogus_key' from vp.conf, or delete it: ccmorph rm vp"},
		{"invalid effort", invalidEffortConf,
			"invalid effort 'ultra' in 'vp': effort must be low, medium, high, xhigh or max",
			"not ready: set a valid level with: ccmorph set vp effort <level>, or delete it: ccmorph rm vp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := storeTestLauncher(t, "vp", tc.conf, `{"env":{}}`)
			out, rc := readerOutput(t, func() int { return l.doctor("vp", false) })
			if rc == 0 || !strings.Contains(out, tc.fault) || !strings.Contains(out, tc.repair) {
				t.Fatalf("doctor: rc %d out %q", rc, out)
			}
		})
	}
}

// GH-839: a set is judged on the conf it leaves behind. Overwriting the bad
// effort does not repair an unknown key beside it: the set names that key and
// writes nothing.
func TestSetEffortNamesTheFaultItLeavesBehind(t *testing.T) {
	t.Setenv("CCMORPH_CONTEXT_LOCKED", "vp")
	conf := "type=apikey\neffort=ultra\nbogus_key=1\n"
	l := storeTestLauncher(t, "vp", conf, `{"env":{}}`)
	out, rc := readerOutput(t, func() int { return l.getSet("set", []string{"vp", "effort", "high"}) })
	if rc == 0 || !strings.Contains(out, "unknown key 'bogus_key' in 'vp'") {
		t.Fatalf("set effort high beside an unknown key: rc %d out %q", rc, out)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "vp.conf")); string(b) != conf {
		t.Fatalf("the refused set wrote: %q", b)
	}
}

// The skip count changes nothing for a home with no conf: it is still empty.
func TestListStillCallsAHomeWithNoConfEmpty(t *testing.T) {
	l := storeTestLauncher(t, "vp", "type=apikey\n", `{"env":{}}`)
	if err := os.Remove(filepath.Join(l.dir, "vp.conf")); err != nil {
		t.Fatal(err)
	}
	out, rc := readerOutput(t, func() int { return l.list(nil) })
	if rc != 0 || !strings.Contains(out, "No profiles yet") || strings.Contains(out, "skipping") {
		t.Fatalf("list on an empty home: rc %d out %q", rc, out)
	}
}
