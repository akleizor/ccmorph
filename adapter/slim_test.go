package adapter

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Moved from tests/adapter_test.py: the rendering and input-validation rules
// below were asserted there through a subprocess and a stub gateway.

func TestRenderPlacesEachModelInItsDialectBlock(t *testing.T) { // test_config_places_each_model_in_its_dialect_block
	cap8192 := 8192
	capped := Row{Model: "m-capped", Dialect: "chat", Status: 200, Cap: &cap8192}
	cfg := render([]Row{
		{Model: "m-anthropic", Dialect: "anthropic", Status: 200},
		{Model: "m-responses", Dialect: "responses", Status: 200},
		{Model: "m-noreason", Dialect: "chat-noreason", Status: 200},
		{Model: "m-dead", Dialect: "unusable", Status: 400},
		capped,
	}, "stub-key", 1234, "https://gw", "/unused", "", "", nil, false)
	for _, want := range []string{"claude-api-key:", "codex-api-key:", "openai-compatibility:", `"reasoning_effort"`, `"max_tokens": 8192`, "request-retry: 3"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	if strings.Contains(cfg, "m-dead") { // a dead model is never offered
		t.Error("an unusable model was rendered")
	}
}

func overrideParams(cfg, model string) string {
	m := regexp.MustCompile(`- name: "` + regexp.QuoteMeta(model) + `"\n      params:\n((?:        .+\n)+)`).FindStringSubmatch(cfg)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestRenderThinkingOverride(t *testing.T) {
	// test_reason_by_default_model_gets_thinking_disabled (the rendering half),
	// test_thinking_override_never_replaces_the_system_prompt,
	// test_thinking_override_is_only_rendered_for_anthropic_dialect.
	cfg := render([]Row{
		{Model: "m-reasoner", Dialect: "anthropic", Status: 200, Nothink: true},
		{Model: "m-anthropic", Dialect: "anthropic", Status: 200},
		{Model: "m-responses", Dialect: "responses", Status: 200, Nothink: true},
		{Model: "m-noreason", Dialect: "chat-noreason", Status: 200, Nothink: true},
	}, "k", 1234, "https://gw", "/unused", "", "", nil, false)
	if !strings.Contains(overrideParams(cfg, "m-reasoner"), `"thinking": {"type": "disabled"}`) {
		t.Error("a reason-by-default model carries no thinking override")
	}
	for _, m := range []string{"m-anthropic", "m-responses", "m-noreason"} {
		if strings.Contains(overrideParams(cfg, m), `"thinking"`) {
			t.Errorf("%s must carry no thinking override", m)
		}
	}
	// Params lines only: a comment may name the field to warn against it.
	for _, line := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(line, `        "`) && strings.Contains(line, `"system"`) {
			t.Errorf("the override must set no system field: %q", line)
		}
	}
	if strings.Contains(cfg, "Think briefly") {
		t.Error("a system prompt override leaked back in")
	}
}

func TestRenderQuotesSafeIDsAndSkipsUnsafeOnes(t *testing.T) { // test_model_ids_are_escaped_and_unsafe_ones_skipped
	rows := []Row{row("anthropic"), {Model: "m-evil\"\nport: 1\n", Dialect: "anthropic", Status: 200},
		{Model: "m-evil ok:", Dialect: "anthropic", Status: 200}, {Model: "m-evil.safe-1", Dialect: "anthropic", Status: 200}}
	cfg := render(rows, "k", 1234, "https://gw", "/unused", "", "", nil, false)
	if !strings.Contains(cfg, `"m-evil.safe-1"`) || strings.Contains(cfg, "m-evil ok:") || strings.Contains(cfg, "port: 1\n") {
		t.Fatalf("unsafe ids reached the config:\n%s", cfg)
	}
	ports := 0
	for _, line := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(line, "port:") {
			ports++
		}
		if m := regexp.MustCompile(`^\s+(?:- )?(?:name|alias): (.+)$`).FindStringSubmatch(line); m != nil && !(strings.HasPrefix(m[1], `"`) && strings.HasSuffix(m[1], `"`)) {
			t.Errorf("unquoted scalar: %q", line)
		}
	}
	if ports != 1 {
		t.Errorf("%d port lines, want the one the adapter chose", ports)
	}
}

func TestInventorySkipsUnsafeIDs(t *testing.T) { // the inventory half of the same Python test
	base, _ := modelsServer(t, func() any {
		return []any{map[string]any{"id": "m-evil\"\nport: 1\n"}, map[string]any{"id": "m-evil ok:"}, map[string]any{"id": "m-evil.safe-1"}}
	})
	output := captureStdio(t)
	got, err := inventory(base, "k", http.DefaultClient)
	if err != nil || len(got) != 1 || got[0] != "m-evil.safe-1" || !strings.Contains(output(), "unsafe characters") {
		t.Fatal(got, err, output())
	}
}

func TestPrintConfigRedactsAKeyNeedingEscaping(t *testing.T) { // test_a_key_needing_escaping_is_still_fully_redacted
	key := `sk-a"b\c-live`
	stubProfile(t, map[string]string{"ANTHROPIC_API_KEY": key, "ANTHROPIC_BASE_URL": "https://gateway"})
	swap(t, &profileDirs, t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	swap(t, &classifyFn, func(_, _, _, _ string, _ bool) ([]Row, error) { return []Row{row("anthropic")}, nil })
	swap(t, &freePortFn, func() (int, error) { return 1234, nil })
	output := captureStdio(t)
	if got := Main([]string{"demo", "--auto", "--print-config"}); got != 0 {
		t.Fatalf("Main = %d", got)
	}
	text := output()
	for _, spelling := range []string{key, `sk-a\"b\\c-live`} {
		if strings.Contains(text, spelling) {
			t.Errorf("the credential printed unredacted as %q", spelling)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Error("no redaction marker")
	}
}

// test_long_lived_endpoint_refuses_bad_inputs_before_starting: every input is
// judged before anything starts, nothing is contacted, and the token never
// reaches an output stream.
func TestLongLivedEndpointRefusesBadInputsBeforeStarting(t *testing.T) {
	swap(t, &classifyFn, func(_, _, _, _ string, _ bool) ([]Row, error) { t.Error("something was contacted"); return nil, nil })
	dir := t.TempDir()
	tokenFile := func(name, text string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		writeFile(t, p, text, mode)
		return p
	}
	good := tokenFile("good", strings.Repeat("t", 40), 0o600)
	linked := filepath.Join(dir, "link")
	os.Symlink(good, linked)
	directory := filepath.Join(dir, "dir")
	os.Mkdir(directory, 0o700)
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	// A wildcard listener receives 127.0.0.1 traffic too; the check must see it.
	wildcard, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer wildcard.Close()
	port := func(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }
	long := strings.Repeat("t", 20) + "\n" + strings.Repeat("t", 19)
	for _, tc := range []struct {
		name, file string
		port       int
		phrase     string
		secret     string
	}{
		{"mode-0644", tokenFile("broad", strings.Repeat("t", 40), 0o644), 41001, "must have mode 0600", strings.Repeat("t", 40)},
		{"symlink-to-valid-file", linked, 41001, "inbound token file " + linked, strings.Repeat("t", 40)},
		{"directory", directory, 41001, "inbound token file " + directory, ""},
		{"too-short", tokenFile("short", strings.Repeat("t", 31), 0o600), 41001, "holds fewer than 32 characters", strings.Repeat("t", 31)},
		{"missing-path", filepath.Join(dir, "missing"), 41001, "inbound token file", ""},
		{"empty-path-supplied", "", 41001, "inbound token file", ""},
		{"interior-newline", tokenFile("nl", long, 0o600), 41001, "holds control characters", long},
		{"oversized", tokenFile("big", strings.Repeat("t", 5000), 0o600), 41001, "holds more than 4096 characters", strings.Repeat("t", 5000)},
		{"port-held-by-another-socket", good, port(held), "--port " + strconv.Itoa(port(held)), strings.Repeat("t", 40)},
		{"port-held-by-a-wildcard-listener", good, port(wildcard), "--port " + strconv.Itoa(port(wildcard)), strings.Repeat("t", 40)},
		{"port-out-of-range", good, 80, "--port 80", strings.Repeat("t", 40)},
		{"port-zero", good, 0, "--port 0", strings.Repeat("t", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := captureStdio(t)
			code, did := exits(t, func() {
				Main([]string{"demo", "--auto", "--port", strconv.Itoa(tc.port), "--inbound-token-file", tc.file})
			})
			text := output()
			if !did || code == 0 {
				t.Fatalf("started: exit %d did %v", code, did)
			}
			if strings.Contains(text, "PORT=") || strings.Contains(text, "TOKEN=") ||
				(tc.secret != "" && strings.Contains(text, tc.secret)) || !strings.Contains(text, tc.phrase) {
				t.Fatalf("want %q, no secret: %q", tc.phrase, text)
			}
		})
	}
}
