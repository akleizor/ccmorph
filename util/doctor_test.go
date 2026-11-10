package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stepPATH points PATH at a directory holding a brew stub, or one holding
// nothing: the install step the adapter wording names comes from the machine's
// PATH (#369), so each case chooses the machine it runs on.
func stepPATH(t *testing.T, brew bool) {
	t.Helper()
	dir := t.TempDir()
	if brew {
		if err := os.WriteFile(filepath.Join(dir, "brew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// The adapter line's wording is the contract GH-272 fixed: it says whether this
// endpoint needs a translator and whether cliproxyapi is installed, and is
// absent when nothing decides the need. The
// not-installed verdict cannot be produced end to end on a machine that has
// cliproxyapi installed (discovery finds it at a fixed path), so the wording is pinned here, where the verdict is an input. The empty key
// sends no request, so the test needs no curl; the profile name matches no
// real process, so no running session is reported either. The install step the
// FAIL wording names is the machine's own (#369): brew's command where brew is
// on PATH, the project page the installer gives where it is not.
func TestDoctorRouteAdapterLine(t *testing.T) {
	cases := []struct {
		name string
		need TranslatorNeed
		brew bool
		kind string
		msg  string
	}{
		{"needed and installed", TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"needed and not installed, brew on PATH", TranslatorNeed{Known: true, Needed: true}, true, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: brew install cliproxyapi"},
		{"needed and not installed, no brew", TranslatorNeed{Known: true, Needed: true}, false, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: https://github.com/router-for-me/CLIProxyAPI"},
		{"not needed", TranslatorNeed{Known: true}, true, "OK",
			"adapter: this endpoint needs no translator"},
		{"unknown, translator installed", TranslatorNeed{Installed: true}, true, "", ""},
		{"unknown, translator not installed", TranslatorNeed{}, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stepPATH(t, c.brew)
			rows, failed, _ := DoctorRoute("doctor-route-verdict", "apikey",
				"https://example.test", "", "", func(string) TranslatorNeed { return c.need }, "", "", false)
			var line *DoctorRow
			for i := range rows {
				if strings.HasPrefix(rows[i].Msg, "adapter:") {
					if line != nil {
						t.Fatalf("two adapter lines: %+v", rows)
					}
					line = &rows[i]
				}
			}
			if c.kind == "" {
				// Nothing is cached and no listing was read (the empty key sends
				// no request): nothing honest to say, so no line.
				if line != nil {
					t.Fatalf("unexpected adapter line: %+v", *line)
				}
				return
			}
			if line == nil {
				t.Fatalf("no adapter line; rows: %+v", rows)
			}
			if line.Kind != c.kind || line.Msg != c.msg {
				t.Fatalf("got %s %q, want %s %q", line.Kind, line.Msg, c.kind, c.msg)
			}
			wantFix := ""
			if c.kind == "FAIL" {
				step := "https://github.com/router-for-me/CLIProxyAPI"
				if c.brew {
					step = "brew install cliproxyapi"
				}
				wantFix = "install the translator: " + step
			}
			if line.Fix != wantFix {
				t.Fatalf("fix %q, want %q", line.Fix, wantFix)
			}
			if failed != (c.kind == "FAIL") {
				t.Fatalf("failed=%v, want %v", failed, c.kind == "FAIL")
			}
		})
	}
}

// adapterLine's answer for every branch, exactly as the reader sees it.
func TestAdapterLine(t *testing.T) {
	const list = `{"data":[]}`
	translatorSession := "4321\t4322:cliproxyapi,4321:shim"
	cases := []struct {
		name                    string
		model, listing, running string
		need                    TranslatorNeed
		brew                    bool
		kind, msg               string
	}{
		{"needed, installed", "", "", "", TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"needed, not installed, brew on PATH", "", "", "", TranslatorNeed{Known: true, Needed: true}, true, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: brew install cliproxyapi"},
		{"needed, not installed, no brew", "", "", "", TranslatorNeed{Known: true, Needed: true}, false, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: https://github.com/router-for-me/CLIProxyAPI"},
		{"needed, installed, in use", "", "", translatorSession, TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed; an open session on this profile is using one now"},
		{"not needed", "", "", "", TranslatorNeed{Known: true}, true, "OK",
			"adapter: this endpoint needs no translator"},
		{"session running a translator", "", "", translatorSession, TranslatorNeed{}, true, "OK",
			"adapter: this profile needs a translator now: an open session on it is running cliproxyapi"},
		{"session with the hooks shim and a translator", "", "", "4321\t4322:cliproxyapi,4323:hooks-shim", TranslatorNeed{}, true, "OK",
			"adapter: this profile needs a translator now: an open session on it is running cliproxyapi"},
		{"session without a translator", "", "", "4321\t4321:shim", TranslatorNeed{}, true, "OK",
			"adapter: this profile needs no translator now: its open session runs without one"},
		{"no model, installed", "", list, "", TranslatorNeed{Installed: true}, true, "INFO",
			"adapter: this profile needs no translator until a model is chosen, and a hop here will need a saved model (none is set, and none can be picked from the endpoint's model list); cliproxyapi is installed; list them: ccmorph models demo, then save one: ccmorph set demo model <model-id>"},
		{"no model, not installed", "", list, "", TranslatorNeed{}, false, "INFO",
			"adapter: this profile needs no translator until a model is chosen, and a hop here will need a saved model (none is set, and none can be picked from the endpoint's model list); cliproxyapi is not installed; list them: ccmorph models demo, then save one: ccmorph set demo model <model-id>"},
		{"no model, process listing unreadable", "", list, "__UNKNOWN__", TranslatorNeed{}, false, "INFO",
			"adapter: this profile needs no translator until a model is chosen, and a hop here will need a saved model (none is set, and none can be picked from the endpoint's model list); cliproxyapi is not installed; list them: ccmorph models demo, then save one: ccmorph set demo model <model-id>"},
		{"saved model nothing classified", "glm-4.6", list, "", TranslatorNeed{Installed: true}, true, "", ""},
		{"no model, no listing", "", "", "", TranslatorNeed{}, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stepPATH(t, c.brew)
			kind, msg := adapterLine("demo", c.model, c.listing, c.running, c.need)
			if kind != c.kind || msg != c.msg {
				t.Fatalf("got %q %q, want %q %q", kind, msg, c.kind, c.msg)
			}
		})
	}
}
