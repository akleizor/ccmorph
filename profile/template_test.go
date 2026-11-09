package profile

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// share/profile.conf.example is the documented contract for a hand-edited profile.
// It must load as-is, and its key list must stay the parser's list: a key the reader
// accepts but the template never names is undocumented, and one the template names
// but the reader rejects is a lie.
func TestTheProfileTemplateLoadsAndDocumentsEveryAcceptedKey(t *testing.T) {
	tpl, err := os.ReadFile("../share/profile.conf.example")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tpl.conf"), tpl, 0o600)
	f, err := Read(dir, "tpl", true, Defaults(), func(string) {})
	if err == nil {
		err = Validate("tpl", f)
	}
	if err != nil {
		t.Fatalf("the profile template must load and validate unchanged: %v", err)
	}

	// The reader's accepted keys are checkKey's case labels and the per-type lists.
	// Read both from the source, so a key added later is checked too.
	srcBytes, _ := os.ReadFile("profile.go")
	src := string(srcBytes)
	fn := src[strings.Index(src, "func checkKey"):]
	fn = fn[:strings.Index(fn, "\n}\n")]
	var accepted []string
	for _, m := range regexp.MustCompile(`(?:k == |HasPrefix\(k, )"([a-z_.]+)"`).FindAllStringSubmatch(fn, -1) {
		accepted = append(accepted, regexp.MustCompile(`\.$`).ReplaceAllString(m[1], ".*"))
	}
	pt := src[strings.Index(src, "perType  = map"):]
	pt = pt[:strings.Index(pt, "\n\t}")]
	for _, m := range regexp.MustCompile(`"([a-z_.*]+)"`).FindAllStringSubmatch(pt, -1) {
		if !slices.Contains([]string{"sub", "token", "apikey", "provider"}, m[1]) {
			accepted = append(accepted, m[1])
		}
	}
	if len(accepted) <= 20 || !slices.Contains(accepted, "model_map") || !slices.Contains(accepted, "env.*") {
		t.Fatalf("could not find the reader key list: %v", accepted)
	}

	doc := string(tpl)
	for _, k := range accepted {
		// A key is documented by its literal line, by a comment naming it, or by the
		// `# pattern.<arg>` comment a pattern key uses (`effort.<model>`). The trailing
		// class stops `# model` from being satisfied by `# model_defaults`.
		var ok bool
		if k == "env.*" {
			ok = regexp.MustCompile(`#\s+env\.`).MatchString(doc)
		} else {
			bare := strings.TrimSuffix(k, "*")
			if i := strings.Index(bare, "."); i >= 0 {
				bare = bare[:i]
			}
			q := regexp.QuoteMeta(bare)
			ok = strings.Contains(doc, bare+"=") || regexp.MustCompile(`# `+q+`[^a-zA-Z0-9_]`).MatchString(doc) ||
				regexp.MustCompile(`# `+q+`\.[^a-zA-Z0-9_]`).MatchString(doc)
		}
		if !ok {
			t.Errorf("profile.conf.example does not document the accepted key %q", k)
		}
	}
}

// The template's effort line is the list of levels a hand-edit may write, so it
// names exactly the levels the reader accepts (#264).
func TestTheTemplatesEffortLineNamesTheAcceptedLevels(t *testing.T) {
	tpl, err := os.ReadFile("../share/profile.conf.example")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^# effort\s+([a-z]+(?: \| [a-z]+)*)$`).FindStringSubmatch(string(tpl))
	if m == nil {
		t.Fatal("profile.conf.example names no effort levels")
	}
	if levels := strings.Split(m[1], " | "); !slices.Equal(levels, Efforts) {
		t.Errorf("the template's effort levels %v are not the accepted levels %v", levels, Efforts)
	}
}
