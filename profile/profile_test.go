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

// GH-839: a saved effort the conf refuses is named by value, and the refusal
// lists every level the conf does accept, so the reader knows what to type.
func TestReadNamesAnInvalidEffortValueAndTheLevels(t *testing.T) {
	conf := `type=apikey
api_key=k
effort=ultra
`
	_, _, err := read(t, conf)
	if err == nil {
		t.Fatal("effort=ultra loaded")
	}
	for _, want := range []string{"invalid effort 'ultra'", "in 'p'", "low, medium, high, xhigh or max"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	if got := Repair(err, "p"); !strings.Contains(got, "ccmorph set p effort") || !strings.Contains(got, "ccmorph rm p") {
		t.Fatalf("repair %q names neither the set nor the removal", got)
	}
}

// The skip line a listing appends is the repair, and an unknown key (the fault
// a hand edit most often leaves) names the line to remove before the removal
// that always works (GH-839).
func TestRepairNamesTheUnknownKeyAndTheRemoval(t *testing.T) {
	_, _, err := read(t, "type=apikey\nbogus_key=1\n")
	if err == nil {
		t.Fatal("bogus_key loaded")
	}
	for _, want := range []string{"remove the unknown key 'bogus_key'", "ccmorph rm p"} {
		if got := Repair(err, "p"); !strings.Contains(got, want) {
			t.Fatalf("repair %q does not name %q", got, want)
		}
	}
}

// A refusal that already names its own repair keeps it: a half-made sign-in
// sends a person to `login`, never to the removal that throws the profile
// away (GH-649), and the same holds for a credential the keychain lost
// (GH-839).
func TestRepairKeepsTheRepairTheRefusalAlreadyNames(t *testing.T) {
	_, _, err := read(t, "type=sub\n")
	if got := Repair(err, "p"); got != "sign in again: ccmorph login p" {
		t.Fatalf("half-made sign-in repair %q", got)
	}
	// No `security` on PATH, so the keychain lookup fails on every host.
	t.Setenv("PATH", t.TempDir())
	ref := "keychain" + ":" + "absent-839"
	conf := "type=apikey\n" + "api_key" + "=" + ref + "\n"
	_, _, err = read(t, conf)
	if got := Repair(err, "p"); got != "set it again with: ccmorph config p" {
		t.Fatalf("keychain repair %q (err %v)", got, err)
	}
	if got := RepairOf(err); got != "set it again with: ccmorph config p" {
		t.Fatalf("RepairOf %q", got)
	}
	_, _, err = read(t, "type=token\n")
	if got := RepairOf(err); got != "" {
		t.Fatalf("RepairOf on a refusal with no repair %q", got)
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
		"type=apikey\neffort=huge\n":                                "invalid effort 'huge' in 'p'",
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

func TestValidNameCapsTheLength(t *testing.T) {
	if !ValidName(strings.Repeat("y", MaxNameLen)) {
		t.Fatal("a name at the limit must be valid")
	}
	if ValidName(strings.Repeat("y", MaxNameLen+1)) {
		t.Fatal("a name one past the limit must be refused")
	}
}

// GH-449: rm must still address a profile made before #373's length limit.
// Every profile makes "<name>.conf" and ".locks/<name>.lock" (+5), so 250 is
// the last name inside NAME_MAX's 255. The bridge file (".<name>.bridge.users",
// +14 → 241) bounds only a file a bridge-bearing provider makes: a name between
// the two simply has no bridge file to remove (#551).
func TestRemovableNameAcceptsTheLegacyRange(t *testing.T) {
	// Every length in the legacy range, not one pin: a rule that rejects any
	// single one of them (240 and 241 are the old test's accepted lengths) must
	// fail here (#551).
	for length := MaxNameLen + 1; length <= LegacyMaxNameLen; length++ {
		if !RemovableName(strings.Repeat("y", length)) {
			t.Fatalf("a %d-byte name inside the legacy range must be removable", length)
		}
	}
	if RemovableName(strings.Repeat("y", LegacyMaxNameLen+1)) {
		t.Fatal("a name whose conf would exceed NAME_MAX must stay refused")
	}
	for _, name := range []string{"Active", "ccmorph", "a-b", "a.b", ""} {
		if RemovableName(name) {
			t.Fatalf("%q must stay invalid", name)
		}
	}
}

// RemovableBadNameMsg is the refusal rm gives: the same one line, with the
// length reason naming the bound rm applies, not ValidName's (#551).
func TestRemovableBadNameMsgNamesRmsOwnBound(t *testing.T) {
	long := strings.Repeat("y", 300)
	got := RemovableBadNameMsg(long)
	want := "invalid profile name '" + strings.Repeat("y", 32) +
		"…': too long; at most 250 of A-Z, a-z, 0-9 and _"
	if got != want {
		t.Fatalf("RemovableBadNameMsg = %q, want %q", got, want)
	}
	if got == BadNameMsg(long) {
		t.Fatal("the removable refusal must name rm's own bound, not ValidName's 200")
	}
	if RemovableBadNameMsg("my-work") != BadNameMsg("my-work") {
		t.Fatal("a name refused for its shape hears the same reason either way")
	}
}

// BadNameMsg is the one reply every command gives a name that can never be a
// profile (#373): the reason, the name shown short, and never a second line.
func TestBadNameMsg(t *testing.T) {
	const chars = "a name may hold only A-Z, a-z, 0-9 and _ (no hyphen, dot, slash or space)"
	for _, c := range []struct{ name, want string }{
		{"my-work", "invalid profile name 'my-work': " + chars},
		{"a/b", "invalid profile name 'a/b': " + chars},
		{"bad name", "invalid profile name 'bad name': " + chars},
		{"a.b", "invalid profile name 'a.b': " + chars},
		{"日本語", "invalid profile name '日本語': " + chars},
		{"a\nb", "invalid profile name 'a?b': " + chars},
		{strings.Repeat("y", MaxNameLen+100), "invalid profile name '" + strings.Repeat("y", 32) +
			"…': too long; at most 200 of A-Z, a-z, 0-9 and _"},
		{"Active", "invalid profile name 'Active': that word is reserved; choose another"},
		{"ccmorph", "invalid profile name 'ccmorph': that word is reserved; choose another"},
	} {
		got := BadNameMsg(c.name)
		if got != c.want {
			t.Fatalf("BadNameMsg(%q) = %q, want %q", c.name, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Fatalf("BadNameMsg(%q) is not one line: %q", c.name, got)
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

// GH-790: three sentences read "is a API key profile" because the article was
// hardcoded before KindLabel. The article now belongs with the label: the
// refusal says "an API key profile", and the kinds that took "a" still do.
func TestKindPhraseGivesEveryKindLabelItsArticle(t *testing.T) {
	for _, c := range []struct{ typ, provider, want string }{
		{"sub", "", "a Claude account"},
		{"token", "", "a Claude token"},
		{"apikey", "", "an API key"},
		{"proxy", "", "an API key"},
		{"provider", "codex", "a Codex"},
		{"provider", "xai", "a xAI"},
		{"provider", "", "a Provider"},
		{"quantum", "", "a quantum"},
	} {
		if got := KindPhrase(c.typ, c.provider); got != c.want {
			t.Errorf("KindPhrase(%q, %q) = %q, want %q", c.typ, c.provider, got, c.want)
		}
	}
}
