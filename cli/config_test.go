package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

// #358: the fold erases every row the value line took, and no more.
func TestValueRowsCountsEveryRowTheValueTook(t *testing.T) {
	for _, c := range []struct {
		value   string
		columns int
		want    int
	}{
		{"high", 100, 1},
		{"", 80, 1},
		{strings.Repeat("a", 100), 100, 1},
		{strings.Repeat("a", 101), 100, 2},
		{strings.Repeat("a", 200), 100, 2},
		{strings.Repeat("a", 201), 100, 3},
		{"picker-alpha", 10, 2},
	} {
		if got := valueRows(c.value, c.columns); got != c.want {
			t.Errorf("valueRows(%d cells, %d columns) = %d, want %d", len(c.value), c.columns, got, c.want)
		}
	}
}

// GH-791: the save decided whether to ask the endpoint's listing through the
// validating reader, so `set keyless model sonnet` printed
// `API key profile 'keyless' has no api_key` to stderr even though the save
// happened and exited 0. A successful save emits no error for the stored key:
// the guard reads quietly, and only a key profile.Validate accepts is ever
// offered to the endpoint.
func TestSetModelOnAKeylessProfileSavesQuietly(t *testing.T) {
	name := "keyless"
	l := storeTestLauncher(t, name, "type=apikey\nbase_url=https://gw.test\n", "")
	t.Setenv("CCMORPH_CONTEXT_LOCKED", name) // run the verb here, not through a re-exec
	note := ""
	out, rc := captureStdout(t, func() int {
		saved := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w
		code := l.getSet("set", []string{name, "model", "sonnet"})
		os.Stderr = saved
		w.Close()
		b, _ := io.ReadAll(r)
		note = string(b)
		return code
	})
	if rc != 0 || out != "sonnet\n" {
		t.Fatalf("the save must stand: rc %d, stdout %q, stderr %q", rc, out, note)
	}
	if strings.Contains(note, "has no api_key") {
		t.Fatalf("a successful save printed an error for the stored key: %q", note)
	}
	if got, rc := l.configStoreQ("get", name, "model"); rc != 0 || got != "sonnet" {
		t.Fatalf("get after the save: %q, rc %d", got, rc)
	}

	// A key that fails Validate is no key to ask an endpoint with: the guard
	// must still decline the listing, now without the printing read.
	ws := "wskey"
	l2 := storeTestLauncher(t, ws, "type=apikey\nbase_url=https://gw.test\napi_key=a b\n", "")
	t.Setenv("CCMORPH_CONTEXT_LOCKED", ws)
	note = ""
	_, rc = captureStdout(t, func() int {
		saved := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w
		code := l2.getSet("set", []string{ws, "model", "sonnet"})
		os.Stderr = saved
		w.Close()
		b, _ := io.ReadAll(r)
		note = string(b)
		return code
	})
	if rc != 0 {
		t.Fatalf("the save must stand for a key the endpoint is not asked about: rc %d, stderr %q", rc, note)
	}
}
