package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var lists = Lists{
	ProfileEnv: map[string]bool{"ANTHROPIC_DEFAULT_OPUS_MODEL": true, "CCMORPH_MASK": true},
	MappingEnv: map[string]bool{"ANTHROPIC_DEFAULT_OPUS_MODEL": true},
}

func read(t *testing.T, conf string) (map[string]string, []string, error) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "p.conf"), []byte(conf), 0o644)
	var warned []string
	f, err := Read(dir, "p", true, lists, func(s string) { warned = append(warned, s) })
	if fi, _ := os.Stat(filepath.Join(dir, "p.conf")); fi.Mode().Perm() != 0o600 {
		t.Fatal("not secured to 600")
	}
	return f, warned, err
}

func TestReadAcceptsEachTypeAndFoldsProxy(t *testing.T) {
	for conf, typ := range map[string]string{
		"type=sub\nemail=a@b\n":                                       "sub",
		"type=token\ntoken=t\n":                                       "token",
		"# c\ntype=apikey\napi_key=k\nenv.CCMORPH_MASK=1\ntag=\n":     "apikey",
		"type=provider\nprovider=codex\n":                             "provider",
		"type=proxy\nbase_url=https://x\napi_key=k\n":                 "apikey",
		"type=sub\nemail=a\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=m\n":     "sub",
		"type=apikey\napi_key=k\ncontext_window.m=1000\neffort.m=max": "apikey",
	} {
		f, _, err := read(t, conf)
		if err != nil || f["type"] != typ {
			t.Fatalf("%q: %v %v", conf, f, err)
		}
	}
}

func TestReadRefusesWithTheMessageZshPrinted(t *testing.T) {
	for conf, want := range map[string]string{
		"type=sub\n":                                 "has no email",
		"type=apikey\nnoequals\n":                    "malformed config",
		"type=apikey\napi_key=a\r\n":                 "malformed config",
		"type=apikey\nbad key=1\n":                   "invalid key",
		"type=apikey\napi_key=a\napi_key=b\n":        "duplicate key 'api_key'",
		"type=apikey\nwhat=1\n":                      "unknown key 'what'",
		"type=apikey\nenv.PATH=x\n":                  "disallowed environment key 'env.PATH'",
		"type=sub\nemail=a\nenv.CCMORPH_MASK=1\n":    "key 'env.CCMORPH_MASK' is invalid for sub",
		"type=sub\nemail=a\napi_key=k\n":             "key 'api_key' is invalid for sub",
		"type=apikey\neffort=huge\n":                 "invalid effort setting 'effort'",
		"type=apikey\ncontext_window.m=0\n":          "invalid context pin",
		"type=apikey\nexcluded_models=a,,b\n":        "excluded_models must be",
		"type=apikey\nmodel_map={\"a[1m]\":\"b\"}\n": "model_map must be",
		"type=apikey\nmonthly_budget_usd=0.0\n":      "monthly_budget_usd must be",
		"type=odd\n":                                 "invalid or missing type",
		"type=proxy\napi_key=k\n":                    "proxy 'p' has no base_url",
	} {
		if _, _, err := read(t, conf); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: got %v, want %q", conf, err, want)
		}
	}
}

func TestRemovedKeysWarnAndAreDropped(t *testing.T) {
	f, warned, err := read(t, "type=apikey\napi_key=k\ntransport=x\n")
	if err != nil || len(warned) != 1 || !strings.Contains(warned[0], "transport= was removed") || f["transport"] != "" {
		t.Fatal(f, warned, err)
	}
}

func TestReadRefusesBadNamesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "real.conf"), []byte("type=token\ntoken=t\n"), 0o600)
	os.Symlink(filepath.Join(dir, "real.conf"), filepath.Join(dir, "link.conf"))
	for _, name := range []string{"link", "Active", "a-b", ""} {
		if _, err := Read(dir, name, false, lists, func(string) {}); err == nil {
			t.Fatal(name)
		}
	}
}
