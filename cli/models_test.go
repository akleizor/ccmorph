package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListingFailureNamesTheEndpoint(t *testing.T) {
	// Issue #357: HTTP 404 cannot tell a gateway that offers no model list from a
	// wrong endpoint, so both fixes are named: a model id given by hand, and the
	// endpoint check the 404 message has always named.
	msg := listingFailure("gw", "https://gw.test/v1", 404)
	if msg != "'https://gw.test/v1' has no model list (HTTP 404) — a model id must be given by hand: ccmorph set gw model <model-id>, or check the endpoint with: ccmorph config gw" {
		t.Fatalf("404: %s", msg)
	}
	// A rejected key is repaired on the profile, and the failure must keep
	// naming the profile's own endpoint command.
	msg = listingFailure("gw", "https://gw.test/v1", 401)
	if msg != "'https://gw.test/v1' rejected the API key for 'gw' (HTTP 401) — replace it with: ccmorph config gw" {
		t.Fatalf("401: %s", msg)
	}
}

// #562, pinned before noModelList's advice line and the not-served sentences
// merged on listingAdvice: a sub or token profile has no model list from an
// endpoint, and the line that answers it names set's picker — the one command
// that chooses its model. Any other kind has a listing to read, so it goes on
// to the command's own checks.
func TestNoModelListSendsSubAndTokenProfilesToTheSetPicker(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".conf"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("acct", "type=sub\nemail=account@example.test\n")
	write("tok", "type=token\ntoken=t\n")
	write("gw", "type=apikey\napi_key=k\n")
	l := &launcher{dir: dir}
	for _, name := range []string{"acct", "tok"} {
		saved := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		answered := l.noModelList(name)
		w.Close()
		os.Stderr = saved
		note, _ := io.ReadAll(r)
		if !answered {
			t.Errorf("%s: noModelList must answer for its kind", name)
		}
		if msg := string(note); !strings.Contains(msg, "choose its model: ccmorph set "+name+" model -i") {
			t.Errorf("%s: the advice line must name set's picker: %q", name, msg)
		}
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stderr
	os.Stderr = devnull
	answered := l.noModelList("gw")
	os.Stderr = saved
	if answered {
		t.Error("an apikey profile has a model list; noModelList must not answer for it")
	}
}
