package util

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCurl puts a curl on PATH that records its arguments and stdin config, prints
// the listing body and exits with rc, so ModelServed meets a catalogue without a
// network. A complaint, when given, is curl's stderr. It returns the directory the
// request log lands in.
func fakeCurl(t *testing.T, body string, rc int, complaint ...string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "body"), []byte(body), 0o600)
	script := "#!/bin/sh\n" +
		"printf 'PROXY=%s\\n' \"$HTTPS_PROXY\" > '" + dir + "/request'\n" +
		"for a do printf 'ARG=%s\\n' \"$a\"; done >> '" + dir + "/request'\n" +
		"cat >> '" + dir + "/request'\n" +
		"cat '" + dir + "/body'\n"
	if len(complaint) > 0 {
		script += "printf '%s\\n' '" + strings.ReplaceAll(complaint[0], "'", `'\''`) + "' >&2\n"
	}
	script += "exit " + string(rune('0'+rc/10)) + string(rune('0'+rc%10)) + "\n"
	os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

const catalogue = `{"data":[{"id":"served-target"},{"id":"literal-id"},{"id":"embed-only","capabilities":{"chat_completion":false}},{"id":"listed-alias"}]}`

// A mapped alias is served when the shim would map it onto a listed id; a native
// endpoint ignores the map, as its launch runs no adapter.
func TestModelServedMatchesTheShimsMappingScope(t *testing.T) {
	fakeCurl(t, catalogue, 0)
	const mapJSON = `{"mapped-alias":"served-target","literal-id":"absent-target","window-target":"served-target[1m]","nonchat-alias":"embed-only","listed-alias":"absent-target","chain-alias":"mapped-alias"}`
	mapped := map[string]Served{"mapped-alias": ServedYes, "mapped-alias[1M]": ServedYes, "mapped-alias[1m]": ServedYes, "window-target": ServedYes}
	native := map[string]Served{"literal-id": ServedYes, "literal-id[1M]": ServedYes, "listed-alias": ServedYes}
	for _, base := range []string{"https://api.anthropic.com", "https://api.anthropic.com/", "http://mapped.invalid"} {
		want := native
		if strings.HasPrefix(base, "http://") {
			want = mapped
		}
		for _, req := range []string{"mapped-alias", "mapped-alias[1M]", "mapped-alias[1m]", "window-target", "literal-id", "literal-id[1M]", "nonchat-alias", "listed-alias", "chain-alias", "unmapped-alias"} {
			exp, ok := want[req]
			if !ok {
				exp = ServedNo
			}
			if got := ModelServed(req, "apikey", base, mapJSON, "test-key"); got != exp {
				t.Errorf("%s %s: got %d, want %d", base, req, got, exp)
			}
		}
	}
}

// A catalogue that cannot prove absence leaves a carried model alone (2); an
// explicit chat denial is a definite no, undeclared capabilities stay carryable,
// and only a terminal window suffix is stripped before comparing.
func TestModelServedWordsUncertaintyAndAbsence(t *testing.T) {
	for _, c := range []struct {
		name, body string
		rc         int
		model      string
		want       Served
	}{
		{"has_more", `{"has_more":true,"data":[{"id":"other-model"}]}`, 0, "absent-chat", ServedUnknown},
		{"has_more still serves a listed id", `{"has_more":true,"data":[{"id":"alpha-chat"}]}`, 0, "alpha-chat", ServedYes},
		{"non-string id", `{"data":[{"id":[]},{"id":"other-model"}]}`, 0, "absent-chat", ServedUnknown},
		{"http error", `{"error":{"message":"rate limited"}}`, 22, "alpha-chat", ServedUnknown},
		{"object data", `{"data":{"alpha-chat":{}}}`, 0, "alpha-chat", ServedUnknown},
		{"string data", `{"data":"alpha-chat"}`, 0, "alpha-chat", ServedUnknown},
		{"bare array", `[{"id":"alpha-chat"}]`, 0, "alpha-chat", ServedUnknown},
		{"explicit non-chat", `{"data":[{"id":[]},{"id":"image-only","capabilities":{"chat_completion":false}}]}`, 0, "image-only", ServedNo},
		{"undeclared chat", `{"data":[{"id":"undeclared-chat","capabilities":{"embeddings":true}}]}`, 0, "undeclared-chat", ServedYes},
		{"bracketed id intact", `{"data":[{"id":"vendor[beta]"}]}`, 0, "vendor[beta]", ServedYes},
		{"terminal 1m stripped", `{"data":[{"id":"vendor[beta]"}]}`, 0, "vendor[beta][1m]", ServedYes},
		{"terminal 1M stripped", `{"data":[{"id":"vendor[beta]"}]}`, 0, "vendor[beta][1M]", ServedYes},
	} {
		fakeCurl(t, c.body, c.rc)
		if got := ModelServed(c.model, "proxy", "http://proxy.invalid", "", "test-key"); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	fakeCurl(t, "", 22)
	if got := ModelServed("mapped-alias", "apikey", "http://mapped.invalid", `{"mapped-alias":"absent-target"}`, "test-key"); got != ServedUnknown {
		t.Errorf("a failed endpoint must preserve a mapped model, got %d", got)
	}
}

// The listing request asks for the largest page, keeps the ambient proxy, and
// authenticates through the stdin config like the adapter and launch probes.
func TestModelsListingRequestShape(t *testing.T) {
	dir := fakeCurl(t, `{"has_more":true,"data":[{"id":"alpha-chat"}]}`, 0)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47999")
	body, ok := ModelsListing("http://proxy.invalid", "test-key", io.Discard)
	if !ok || !strings.Contains(body, "alpha-chat") {
		t.Fatalf("the first catalogue page must stay usable: %q %v", body, ok)
	}
	req, _ := os.ReadFile(filepath.Join(dir, "request"))
	for _, want := range []string{"PROXY=http://127.0.0.1:47999", "ARG=http://proxy.invalid/v1/models?limit=1000", "Authorization: Bearer test-key"} {
		if !strings.Contains(string(req), want) {
			t.Errorf("request lacks %q:\n%s", want, req)
		}
	}
	if rows, ok := GwModels("", false, body); !ok || len(rows) != 1 || rows[0].ID != "alpha-chat" {
		t.Errorf("the first page must list its chat model, got %v %v", rows, ok)
	}
}

// An endpoint that answered says so with a status, so a caller can tell a refused
// key from a network that never carried the request; a listing that worked keeps
// its body and status 0.
func TestModelsAnswerReadsTheRefusedStatus(t *testing.T) {
	for _, c := range []struct {
		name, stderr string
		rc           int
		wantStatus   int
		wantOK       bool
	}{
		{"a rejected key", "curl: (56) The requested URL returned error: 401", 56, 401, false},
		{"a forbidden key", "curl: (22) The requested URL returned error: 403", 22, 403, false},
		{"an older curl's wrong path", "curl: (22) The requested URL returned error: 404 Not Found", 22, 404, false},
		{"a server fault", "curl: (22) The requested URL returned error: 500", 22, 500, false},
		{"nothing answered", "curl: (7) Failed to connect to 127.0.0.1 port 1: Connection refused", 7, 0, false},
		{"a served listing", "", 0, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeCurl(t, catalogue, c.rc, c.stderr)
			body, status, ok := ModelsAnswer("http://proxy.invalid", "test-key")
			if ok != c.wantOK || status != c.wantStatus {
				t.Fatalf("got ok=%v status=%d, want ok=%v status=%d", ok, status, c.wantOK, c.wantStatus)
			}
			if c.wantOK && !strings.Contains(body, "served-target") {
				t.Errorf("a served listing must keep its body, got %q", body)
			}
		})
	}
}
