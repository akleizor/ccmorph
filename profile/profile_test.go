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
		"type=sub\nemail=a\nusage_route=direct\n":                     "sub",
		"type=token\ntoken=t\nusage_route=direct\n":                   "token",
		"type=provider\nprovider=codex\nusage_route=direct\n":         "provider",
	} {
		f, _, err := read(t, conf)
		if err != nil || f["type"] != typ {
			t.Fatalf("%q: %v %v", conf, f, err)
		}
	}
}

// Every level the template documents loads in a conf: the reader is one of the
// surfaces that must name the same list of levels (#264).
func TestReadAcceptsEveryEffortLevel(t *testing.T) {
	for _, lvl := range Efforts {
		if _, _, err := read(t, "type=apikey\napi_key=k\neffort="+lvl+"\n"); err != nil {
			t.Fatalf("effort=%s: %v", lvl, err)
		}
	}
}

// usage_route is a key every launch of that type may carry, and only "direct"
// is a route: Read accepts the key, Validate refuses any other value.
func TestValidateRefusesAnUnknownUsageRoute(t *testing.T) {
	f, _, err := read(t, "type=sub\nemail=a\nusage_route=edge\n")
	if err != nil {
		t.Fatalf("the key must load so the value can be refused: %v", err)
	}
	if err := Validate("p", f); err == nil || !strings.Contains(err.Error(), "usage_route") {
		t.Fatalf("Validate accepted usage_route=edge: %v", err)
	}
}

func TestReadRefusesWithTheMessageZshPrinted(t *testing.T) {
	for conf, want := range map[string]string{
		"type=sub\n":                                                "has no email",
		"type=apikey\nnoequals\n":                                   "malformed config",
		"type=apikey\napi_key=a\r\n":                                "malformed config",
		"type=apikey\nbad key=1\n":                                  "invalid key",
		"type=apikey\napi_key=a\napi_key=b\n":                       "duplicate key 'api_key'",
		"type=apikey\nwhat=1\n":                                     "unknown key 'what'",
		"type=apikey\nenv.PATH=x\n":                                 "disallowed environment key 'env.PATH'",
		"type=sub\nemail=a\nenv.CCMORPH_MASK=1\n":                   "key 'env.CCMORPH_MASK' is invalid for sub",
		"type=sub\nemail=a\napi_key=k\n":                            "key 'api_key' is invalid for sub",
		"type=apikey\neffort=huge\n":                                "invalid effort setting 'effort'",
		"type=apikey\ncontext_window.m=0\n":                         "invalid context pin",
		"type=apikey\nexcluded_models=a,,b\n":                       "excluded_models must be",
		"type=apikey\nmodel_map={\"a[1m]\":\"b\"}\n":                "model_map must be",
		"type=apikey\nmonthly_budget_usd=0.0\n":                     "monthly_budget_usd must be",
		"type=odd\n":                                                "invalid or missing type",
		"type=apikey\napi_key=k\nmonthly_budget_usd=lots\n":         "monthly_budget_usd must be",
		"type=apikey\napi_key=k\n" + "endpoint" + "_side=outside\n": "unknown key 'endpoint_side'",
		"type=token\ntoken=t\nadapter=true\n":                       "key 'adapter' is invalid for token",
		"type=proxy\napi_key=k\n":                                   "proxy 'p' has no base_url",
	} {
		if _, _, err := read(t, conf); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: got %v, want %q", conf, err, want)
		}
	}
}

// monthly_budget_usd belongs to a sibling tool's shared conf: a number loads, and
// an apikey or proxy profile may declare adapter=true, which survives parsing.
func TestReadLoadsTheBudgetAndKeepsTheAdapterDeclaration(t *testing.T) {
	for _, conf := range []string{
		"type=apikey\napi_key=k\nbase_url=https://g\nmonthly_budget_usd=500\n",
		"type=apikey\napi_key=k\nbase_url=https://g\nadapter=true\n",
		"type=proxy\nbase_url=http://127.0.0.1:11434\napi_key=none\nmodel=local-model\nadapter=true\n",
	} {
		f, _, err := read(t, conf)
		if err != nil || f["type"] != "apikey" {
			t.Fatalf("%q: %v %v", conf, f, err)
		}
		if strings.Contains(conf, "adapter=true") && f["adapter"] != "true" {
			t.Fatalf("%q: adapter=true must survive parsing, got %q", conf, f["adapter"])
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

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		f    map[string]string
		want string
	}{
		{map[string]string{"type": "sub"}, ""},
		{map[string]string{"type": "apikey", "api_key": "k"}, ""},
		{map[string]string{"type": "apikey"}, "API key profile 'p' has no api_key"},
		{map[string]string{"type": "apikey", "api_key": "a b"}, "API key profile 'p' api_key contains whitespace"},
		{map[string]string{"type": "apikey", "api_key": "k", "base_url": "gateway.invalid"}, "API key profile 'p' endpoint must use http:// or https://"},
		{map[string]string{"type": "apikey", "api_key": "k", "base_url": "ftp://x"}, "API key profile 'p' endpoint must use http:// or https://"},
		{map[string]string{"type": "apikey", "api_key": "k", "base_url": "https://x y"}, "API key profile 'p' endpoint contains whitespace"},
		{map[string]string{"type": "apikey", "api_key": "k", "adapter": "yes"}, "API key profile 'p' adapter must be 'true', 'false', or absent"},
		{map[string]string{"type": "apikey", "api_key": "k", "adapter": "true"}, "API key profile 'p' has adapter=true but no endpoint to front"},
		{map[string]string{"type": "apikey", "api_key": "k", "adapter": "true", "base_url": "https://g"}, ""},
	} {
		got := ""
		if err := Validate("p", c.f); err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%v: got %q want %q", c.f, got, c.want)
		}
	}
}

func TestEnv(t *testing.T) {
	for _, c := range []struct {
		f    map[string]string
		want string
	}{
		{map[string]string{"type": "sub"}, "CLAUDE_CONFIG_DIR=/p/n CCMORPH_PROFILE=n"},
		{map[string]string{"type": "apikey", "api_key": "k"}, "CLAUDE_CONFIG_DIR=/p/n CCMORPH_PROFILE=n ANTHROPIC_API_KEY=k"},
		{map[string]string{"type": "apikey", "api_key": "k", "base_url": "https://g", "private": "1", "env.B": "2", "env.A": "1", "model_map": "x=y"},
			"CLAUDE_CONFIG_DIR=/p/n CCMORPH_PROFILE=n CCMORPH_PRIVATE=1 ANTHROPIC_BASE_URL=https://g ANTHROPIC_API_KEY=k CCMORPH_MODEL_MAP=x=y A=1 B=2"},
		{map[string]string{"type": "token", "token": "t"}, "CLAUDE_CONFIG_DIR=/p/n CCMORPH_PROFILE=n CLAUDE_CODE_OAUTH_TOKEN=t"},
		{map[string]string{"type": "provider", "env.X": "1"}, "CLAUDE_CONFIG_DIR=/p/n CCMORPH_PROFILE=n X=1"},
	} {
		env, err := Env("n", "/p", c.f)
		if got := strings.Join(env, " "); err != nil || got != c.want {
			t.Errorf("%v: got %q err %v", c.f, got, err)
		}
	}
	if _, err := Env("n", "/p", map[string]string{"type": "quantum"}); err == nil || err.Error() != "profile 'n' has unsupported type 'quantum'" {
		t.Fatalf("unsupported type: %v", err)
	}
}

func TestValidModelMap(t *testing.T) {
	for in, want := range map[string]bool{
		`{"a":"b"}`: true, ` {"a":"b"}`: true, `{}`: true, `[]`: false, `{"a":"b","a":"c"}`: false, `{"a[1m]":"b"}`: false,
		`{"a":"b c"}`: false, `{"a":1}`: false, `nope`: false, `{"a":{"b":"c"},"d":"e"}`: false, `{"A[1M]":"b"}`: false,
	} {
		if got := ValidModelMap(in); got != want {
			t.Errorf("ValidModelMap(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestKindLabelNamesEveryProfileKindForPeople(t *testing.T) {
	for _, c := range []struct{ typ, provider, want string }{
		{"sub", "", "Claude account"},
		{"token", "", "Claude token"},
		{"apikey", "", "API key"},
		{"proxy", "", "API key"},
		{"provider", "codex", "Codex"},
		{"provider", "xai", "xAI"},
		// #366: the two Kimi logins carry the provider list's names — the
		// region tells them apart, not only the domain in the row's text.
		{"provider", "kimi-ai", "Kimi For Coding (global)"},
		{"provider", "kimi", "Kimi For Coding (China)"},
		{"provider", "", "Provider"},
		{"quantum", "", "quantum"},
	} {
		if got := KindLabel(c.typ, c.provider); got != c.want {
			t.Errorf("KindLabel(%q, %q) = %q, want %q", c.typ, c.provider, got, c.want)
		}
	}
}
