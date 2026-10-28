package util

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/pycompat"
)

func TestEngineInt(t *testing.T) {
	for in, want := range map[string]string{
		"100000": "100000", "1e5": "100000", "2E4": "20000", "100_000": "100000", "100,000": "100000",
		"100 000": "100000", "100000junk": "100000", "junk100000": "", "0": "", "-5": "", "1e999": "",
		"-1e999": "", "1e309": "", "1e21": "1000000000000000000", "\u00a0100000": "100000",
		"\u0085100000": "", "1.5e0": "", "100_00": "100", "100,000_000": "100",
	} {
		var out bytes.Buffer
		engineIntVerb([]string{in}, nil, &out, nil)
		if got := strings.TrimSpace(out.String()); got != want {
			t.Errorf("engine-int %q = %q, want %q", in, got, want)
		}
	}
}

func TestEnginePct(t *testing.T) {
	for in, want := range map[string]float64{"80": 80, "90junk": 90, "\xef\xbb\xbf70": 70, "1e1": 10, ".5": 0.5} {
		if got, ok := enginePct(in); !ok || got != want {
			t.Errorf("enginePct(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "101", "x", "-5", "nan"} {
		if _, ok := enginePct(in); ok {
			t.Errorf("enginePct(%q) must be invalid", in)
		}
	}
}

func TestEngineTriggerDividesBeforeMultiplying(t *testing.T) {
	pct := 70.0
	// 700,000 * (70/100): multiplying first lands one token high.
	if got := engineTrigger(680_000, &pct); got != 475_999 && got != 476_000 {
		t.Fatalf("trigger = %d", got)
	}
	if got := engineTrigger(100_000, nil); got != 87_000 {
		t.Fatalf("no-pct trigger = %d", got)
	}
}

func TestHazardMessage(t *testing.T) {
	rows := []string{"135000", "40000\tkimi-k3"}
	if got := hazardMessage(rows, false); !strings.HasPrefix(got, "compacts at 135k; lower model limits: kimi-k3 limit 40k.") {
		t.Fatal(got)
	}
	if got := hazardMessage(rows, true); !strings.Contains(got, "hidden from /model: kimi-k3 limit 40k.") {
		t.Fatal(got)
	}
	many := append([]string{"100000"}, "5000\ta", "4000\tb", "3000\tc", "2000\td", "1000\te")
	if got := hazardMessage(many, false); !strings.Contains(got, "a limit 5k, b limit 4k, c limit 3k +2 smaller.") {
		t.Fatal(got)
	}
	if hazardMessage([]string{"1"}, false) != "" {
		t.Fatal("a trigger alone says nothing")
	}
}

func TestValidModelMap(t *testing.T) {
	for in, want := range map[string]int{
		`{"a":"b"}`: 0, ` {"a":"b"}`: 0, `{}`: 0, `[]`: 1, `{"a":"b","a":"c"}`: 1, `{"a[1m]":"b"}`: 1,
		`{"a":"b c"}`: 1, `{"a":1}`: 1, `nope`: 1, `{"a":{"b":"c"},"d":"e"}`: 1, `{"A[1M]":"b"}`: 1,
	} {
		if got := validModelMap([]string{in}, nil, nil, nil); got != want {
			t.Errorf("valid-model-map %s = %d, want %d", in, got, want)
		}
	}
}

func TestUnwireStatuslineKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	os.WriteFile(path, []byte(`{"statusLine":{"command":"/mine/statusline"},"x":1}`), 0o600)
	t.Setenv("_CCM_SL_OWNED", "/mine/statusline")
	unwireStatusline([]string{path}, nil, nil, nil)
	if b, _ := os.ReadFile(path); string(b) != "{\n  \"x\": 1\n}\n" {
		t.Fatalf("%q", b)
	}
	os.WriteFile(path, []byte(`{"statusLine":{"command":"/theirs"}}`), 0o600)
	unwireStatusline([]string{path}, nil, nil, nil)
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "/theirs") {
		t.Fatal("someone else's statusline must stay")
	}
}

func migrateFixture(t *testing.T) (legacy, target string) {
	root := t.TempDir()
	legacy = filepath.Join(root, ".claude-profiles")
	target = filepath.Join(root, ".config", "ccmorph", "profiles")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(legacy, "fixture"), []byte("untouched"), 0o600)
	return
}

// .claude.json is the user's file: a value Python round-tripped unchanged
// must come back byte-for-byte. An unpaired \ud83d escape used to be decoded
// to a surrogate and written out as U+FFFD, silently mutating the file.
func TestPickerRewriteKeepsALoneSurrogateByteForByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	want := "{\n  \"history\": [\n    \"a b\\ud83d c\"\n  ]\n}"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteJSON(path, func(*pycompat.Obj) bool { return true })
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("the rewrite changed the file:\n got %q\nwant %q", got, want)
	}
}

func TestMigrateRefusesACrossDeviceMoveWithoutCopying(t *testing.T) {
	legacy, target := migrateFixture(t)
	old := rename
	rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	defer func() { rename = old }()
	var errw bytes.Buffer
	if rc := migrateProfileHome([]string{legacy, target}, nil, nil, &errw); rc != 1 {
		t.Fatalf("rc=%d %s", rc, errw.String())
	}
	if b, _ := os.ReadFile(filepath.Join(legacy, "fixture")); string(b) != "untouched" {
		t.Fatal("legacy home must stay")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("nothing may be copied")
	}
}

func TestMigrateRestoresTheOriginalWhenTheAliasFails(t *testing.T) {
	legacy, target := migrateFixture(t)
	old := symlink
	symlink = func(string, string) error { return errors.New("simulated alias failure") }
	defer func() { symlink = old }()
	if rc := migrateProfileHome([]string{legacy, target}, nil, nil, &bytes.Buffer{}); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	fi, err := os.Lstat(legacy)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the original directory must be back, not a link")
	}
	if b, _ := os.ReadFile(filepath.Join(legacy, "fixture")); string(b) != "untouched" {
		t.Fatal("content lost")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the moved copy must be gone")
	}
}

func storeFixture(t *testing.T) (conf, settings string) {
	dir := t.TempDir()
	conf = filepath.Join(dir, "demo.conf")
	settings = filepath.Join(dir, "profiles", "demo", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o700)
	os.WriteFile(conf, []byte("type=apikey\napi_key=k\nbase_url=https://old.invalid\n"), 0o600)
	os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"old"},"other":true}`), 0o600)
	return
}

func leftovers(t *testing.T, dirs ...string) {
	for _, d := range dirs {
		if m, _ := filepath.Glob(filepath.Join(d, ".configure.*")); len(m) > 0 {
			t.Fatalf("staging files left in %s: %v", d, m)
		}
	}
}

func TestConfigStoreFailedConfReplacementLeavesBothFilesAlone(t *testing.T) {
	conf, settings := storeFixture(t)
	beforeConf, _ := os.ReadFile(conf)
	beforeSettings, _ := os.ReadFile(settings)
	old := replaceFile
	replaceFile = func(string, string) error { return errors.New("simulated replace failure") }
	defer func() { replaceFile = old }()
	t.Setenv("_CCM_CONFIG_MODEL", "new")
	var errw bytes.Buffer
	if rc := configStore([]string{"set", conf, settings, "", filepath.Join(filepath.Dir(conf), "shared.json")}, nil, &bytes.Buffer{}, &errw); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(errw.String(), "simulated replace failure") {
		t.Fatal(errw.String())
	}
	if a, _ := os.ReadFile(conf); string(a) != string(beforeConf) {
		t.Fatalf("conf changed: %q", a)
	}
	if a, _ := os.ReadFile(settings); string(a) != string(beforeSettings) {
		t.Fatalf("settings changed: %q", a)
	}
	leftovers(t, filepath.Dir(conf), filepath.Dir(settings))
}

func TestConfigStoreRollsTheConfBackWhenTheSettingsReplacementFails(t *testing.T) {
	conf, settings := storeFixture(t)
	beforeConf, _ := os.ReadFile(conf)
	beforeSettings, _ := os.ReadFile(settings)
	old := replaceFile
	replaceFile = func(from, to string) error {
		if to == settings {
			return errors.New("simulated replace failure")
		}
		return os.Rename(from, to)
	}
	defer func() { replaceFile = old }()
	var errw bytes.Buffer
	if rc := configStore([]string{"prepare-launch", conf, settings, "", filepath.Join(filepath.Dir(conf), "shared.json")}, nil, &bytes.Buffer{}, &errw); rc != 1 {
		t.Fatalf("rc=%d %s", rc, errw.String())
	}
	if a, _ := os.ReadFile(conf); string(a) != string(beforeConf) {
		t.Fatalf("the seed must be rolled back: %q", a)
	}
	if a, _ := os.ReadFile(settings); string(a) != string(beforeSettings) {
		t.Fatalf("native settings changed: %q", a)
	}
	leftovers(t, filepath.Dir(conf), filepath.Dir(settings))
}

func TestLaunchDiagnosticsKeepsTheWarningsWhenALayerCannotBeMerged(t *testing.T) {
	// A null $ccmorphUnset makes the merge raise TypeError; Python dropped only the
	// env answers and still printed the warnings that read settings.json.
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	layer := filepath.Join(dir, "settings.profile.json")
	if err := os.WriteFile(settings, []byte(`{"model":"claude-x-dd-ved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layer, []byte(`{"$ccmorphUnset": null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("_CCM_SETTINGS", settings)
	t.Setenv("_CCM_PCT_IN_ENV", "1")
	var out bytes.Buffer
	if rc := launchDiagnostics([]string{"--", layer}, nil, &out, &bytes.Buffer{}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if got := out.String(); got != "disguised\tdev\x00" {
		t.Fatalf("records = %q", got)
	}
}
