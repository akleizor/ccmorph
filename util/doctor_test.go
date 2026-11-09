package util

import (
	"strings"
	"testing"
)

// The adapter line's wording is the contract GH-272 fixed: it says whether this
// endpoint needs a translator and whether cliproxyapi is installed, and is
// absent when nothing decides the need. The
// not-installed verdict cannot be produced end to end on a machine that has
// cliproxyapi installed (discovery finds it at a fixed path), so the wording is pinned here, where the verdict is an input. The empty key
// sends no request, so the test needs no curl; the profile name matches no
// real process, so no running session is reported either.
func TestDoctorRouteAdapterLine(t *testing.T) {
	cases := []struct {
		name string
		need TranslatorNeed
		kind string
		msg  string
	}{
		{"needed and installed", TranslatorNeed{Known: true, Needed: true, Installed: true}, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"needed and not installed", TranslatorNeed{Known: true, Needed: true}, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: brew install cliproxyapi"},
		{"not needed", TranslatorNeed{Known: true}, "OK",
			"adapter: this endpoint needs no translator"},
		{"unknown, translator installed", TranslatorNeed{Installed: true}, "", ""},
		{"unknown, translator not installed", TranslatorNeed{}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
				wantFix = "install the translator: brew install cliproxyapi"
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
		kind, msg               string
	}{
		{"needed, installed", "", "", "", TranslatorNeed{Known: true, Needed: true, Installed: true}, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"needed, not installed", "", "", "", TranslatorNeed{Known: true, Needed: true}, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: brew install cliproxyapi"},
		{"needed, installed, in use", "", "", translatorSession, TranslatorNeed{Known: true, Needed: true, Installed: true}, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed; an open session on this profile is using one now"},
		{"not needed", "", "", "", TranslatorNeed{Known: true}, "OK",
			"adapter: this endpoint needs no translator"},
		{"session running a translator", "", "", translatorSession, TranslatorNeed{}, "OK",
			"adapter: this profile needs a translator now: an open session on it is running cliproxyapi"},
		{"session with the hooks shim and a translator", "", "", "4321\t4322:cliproxyapi,4323:hooks-shim", TranslatorNeed{}, "OK",
			"adapter: this profile needs a translator now: an open session on it is running cliproxyapi"},
		{"session without a translator", "", "", "4321\t4321:shim", TranslatorNeed{}, "OK",
			"adapter: this profile needs no translator now: its open session runs without one"},
		{"no model, installed", "", list, "", TranslatorNeed{Installed: true}, "INFO",
			"adapter: this profile needs no translator until a model is chosen (none is set, and the endpoint's model list names no protocol); cliproxyapi is installed; choose one: ccmorph set demo model <model-id>"},
		{"no model, not installed", "", list, "", TranslatorNeed{}, "INFO",
			"adapter: this profile needs no translator until a model is chosen (none is set, and the endpoint's model list names no protocol); cliproxyapi is not installed; choose one: ccmorph set demo model <model-id>"},
		{"no model, process listing unreadable", "", list, "__UNKNOWN__", TranslatorNeed{}, "INFO",
			"adapter: this profile needs no translator until a model is chosen (none is set, and the endpoint's model list names no protocol); cliproxyapi is not installed; choose one: ccmorph set demo model <model-id>"},
		{"saved model nothing classified", "glm-4.6", list, "", TranslatorNeed{Installed: true}, "", ""},
		{"no model, no listing", "", "", "", TranslatorNeed{}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, msg := adapterLine("demo", c.model, c.listing, c.running, c.need)
			if kind != c.kind || msg != c.msg {
				t.Fatalf("got %q %q, want %q %q", kind, msg, c.kind, c.msg)
			}
		})
	}
}
