package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GH-1038: a password in the profile's endpoint URL was hidden in the Added
// line and in doctor but printed in full by inspect, list and add's own key
// prompt — the very lines a person pastes into a bug report or a screenshot.
// Every human-facing rendering of a saved endpoint goes through shownEndpoint
// (profile.RedactURL): the host and path stay, the login never prints. The
// scripted reads (get base_url, get --json) are not renderings: like the
// api_key itself they answer with the stored value, verbatim.
//
// The password is a fragment rather than a literal inside the URL so this
// file holds no credential-shaped string: the assertion is that these bytes
// appear in no output, wherever the endpoint is shown.
const pwCanary = "{{SECRET_fkm3i2d7l6sm}}"
const pwShown = "https://gw.example.invalid/y"

func pwConf() string {
	return "type=apikey\nbase_url=https://bob:" + pwCanary + "@gw.example.invalid/y\napi_key=sk-test\n"
}

// The same login on an escaped route: /team%2Fone is not /team/one, and no
// rendering may rewrite one into the other while hiding the other (GH-1038).
func escConf() string {
	return "type=apikey\nbase_url=https://bob:" + pwCanary + "@gw.example.invalid/team%2Fone\napi_key=sk-test\n"
}

const escShown = "https://gw.example.invalid/team%2Fone"

// list, the profile menu and inspect describe or show the same profile from
// the one walk and the one masking rule: none of them carries the password,
// all of them still name the endpoint.
func TestNoRendererPrintsAPasswordInTheEndpoint(t *testing.T) {
	l := storeTestLauncher(t, "pw", pwConf(), `{"env":{}}`)
	t.Run("list", func(t *testing.T) {
		out, rc := readerOutput(t, func() int { return l.list(nil) })
		if rc != 0 {
			t.Fatalf("list refused the home: rc %d out %q", rc, out)
		}
		if strings.Contains(out, pwCanary) {
			t.Errorf("list printed the endpoint password: %q", out)
		}
		if !strings.Contains(out, pwShown) {
			t.Errorf("list did not name the endpoint it masks: %q", out)
		}
		esc := storeTestLauncher(t, "esc", escConf(), `{"env":{}}`)
		eout, erc := readerOutput(t, func() int { return esc.list(nil) })
		if erc != 0 {
			t.Fatalf("list refused the home: rc %d out %q", erc, eout)
		}
		if strings.Contains(eout, pwCanary) || !strings.Contains(eout, escShown) {
			t.Errorf("list leaked or rewrote the masked endpoint: %q", eout)
		}
	})
	t.Run("profile menu", func(t *testing.T) {
		rows := strings.Join(l.profileRows(l.active(), nil, nil), "\n")
		if strings.Contains(rows, pwCanary) {
			t.Errorf("the profile menu printed the endpoint password: %q", rows)
		}
		if !strings.Contains(rows, pwShown) {
			t.Errorf("the profile menu did not name the endpoint it masks: %q", rows)
		}
		esc := storeTestLauncher(t, "esc", escConf(), `{"env":{}}`)
		erows := strings.Join(esc.profileRows(esc.active(), nil, nil), "\n")
		if strings.Contains(erows, pwCanary) || !strings.Contains(erows, escShown) {
			t.Errorf("the profile menu leaked or rewrote the masked endpoint: %q", erows)
		}
	})
	t.Run("inspect", func(t *testing.T) {
		out, rc := readerOutput(t, func() int { return l.inspect([]string{"pw"}) })
		if rc != 0 {
			t.Fatalf("inspect refused the profile: rc %d out %q", rc, out)
		}
		if strings.Contains(out, pwCanary) {
			t.Errorf("inspect printed the endpoint password: %q", out)
		}
		// inspect has always trimmed the scheme; the masked line keeps the
		// host and path it showed before, minus the login.
		if !strings.Contains(out, "gw.example.invalid/y") {
			t.Errorf("inspect did not name the endpoint it masks: %q", out)
		}
	})
	t.Run("escaped route", func(t *testing.T) {
		// A path's own percent-escapes identify a route: /team%2Fone is a
		// different one than /team/one, so the masked line keeps them
		// verbatim while the login stays hidden (#1038).
		esc := storeTestLauncher(t, "esc", escConf(), `{"env":{}}`)
		out, rc := readerOutput(t, func() int { return esc.inspect([]string{"esc"}) })
		if rc != 0 {
			t.Fatalf("inspect refused the profile: rc %d out %q", rc, out)
		}
		if strings.Contains(out, pwCanary) {
			t.Errorf("inspect printed the endpoint password: %q", out)
		}
		if !strings.Contains(out, "gw.example.invalid/team%2Fone") {
			t.Errorf("inspect rewrote the endpoint's escaped route: %q", out)
		}
	})
}

// The config connection step echoes the endpoint twice — the keep-current
// prompt and the summary line — and both answer through the same rule. The
// four Enter answers keep every answer: the save must store the endpoint it
// was shown, not the masked line.
func TestConfigNeverPrintsAPasswordInTheEndpoint(t *testing.T) {
	t.Setenv("CCMORPH_CONTEXT_LOCKED", "pw")
	l := storeTestLauncher(t, "pw", pwConf(), `{"env":{}}`)
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("\n\n\n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldIn := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = oldIn }()
	out, rc := readerOutput(t, func() int { return l.config([]string{"pw"}) })
	if rc != 0 {
		t.Fatalf("config refused the profile: rc %d out %q", rc, out)
	}
	if strings.Contains(out, pwCanary) {
		t.Errorf("config printed the endpoint password: %q", out)
	}
	if !strings.Contains(out, "Endpoint ["+pwShown+"]") {
		t.Errorf("the endpoint prompt did not name the endpoint it masks: %q", out)
	}
	if !strings.Contains(out, "Endpoint: "+pwShown) {
		t.Errorf("the summary line did not name the endpoint it masks: %q", out)
	}
	// The masked line is what a person reads; the conf keeps what it stores.
	saved, err := os.ReadFile(filepath.Join(l.dir, "pw.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), pwCanary) {
		t.Errorf("the save that masked the endpoint lost it from the conf: %q", saved)
	}
}

// The keep-current prompt names the saved route exactly: masking the login
// must not rewrite /team%2Fone into the different route /team/one (GH-1038
// refusal: inspect's and add's lines had the same flaw, through the same rule).
func TestConfigPromptKeepsTheEscapedRoute(t *testing.T) {
	t.Setenv("CCMORPH_CONTEXT_LOCKED", "esc1")
	l := storeTestLauncher(t, "esc1", escConf(), `{"env":{}}`)
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("\n\n\n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldIn := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = oldIn }()
	out, rc := readerOutput(t, func() int { return l.config([]string{"esc1"}) })
	if rc != 0 {
		t.Fatalf("config refused the profile: rc %d out %q", rc, out)
	}
	if strings.Contains(out, pwCanary) {
		t.Errorf("config printed the endpoint password: %q", out)
	}
	if !strings.Contains(out, "Endpoint ["+escShown+"]") || !strings.Contains(out, "Endpoint: "+escShown) {
		t.Errorf("config rewrote the endpoint's escaped route: %q", out)
	}
}
