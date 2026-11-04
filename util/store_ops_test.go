package util

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
)

// Moved from tests/cli_config_store_test.py: the store's own rules, asserted
// there through zsh and a subprocess per call.

const storeConf = "type=apikey\napi_key=test-key\nbase_url=https://old.invalid\ncontext_window.m=10000\n"

type storeEnv struct {
	t                      *testing.T
	conf, settings, shared string
}

func newStoreEnv(t *testing.T) *storeEnv {
	dir := t.TempDir()
	s := &storeEnv{t: t, conf: filepath.Join(dir, "demo.conf"),
		settings: filepath.Join(dir, "profiles", "demo", "settings.json"),
		shared:   filepath.Join(dir, "home", ".claude", "settings.json")}
	os.MkdirAll(filepath.Dir(s.settings), 0o700)
	os.MkdirAll(filepath.Dir(s.shared), 0o700)
	s.write(s.conf, storeConf)
	s.write(s.settings, `{"env":{"ANTHROPIC_MODEL":"old"},"other":true}`)
	return s
}

func (s *storeEnv) write(path, text string) {
	s.t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *storeEnv) read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (s *storeEnv) run(op, key string, ch StoreChange) (out, stderr string, rc int) {
	var errw bytes.Buffer
	out, rc = ConfigStore(StoreArgs{Op: op, Conf: s.conf, Settings: s.settings, Key: key, Shared: s.shared,
		Mapping: profile.MappingEnv, Change: ch}, &errw)
	return out, errw.String(), rc
}

func (s *storeEnv) ok(op, key string, ch StoreChange) string {
	s.t.Helper()
	out, errw, rc := s.run(op, key, ch)
	if rc != 0 {
		s.t.Fatalf("%s %s: rc %d: %s", op, key, rc, errw)
	}
	return out
}

func (s *storeEnv) fields() map[string]string {
	f := map[string]string{}
	for _, line := range strings.Split(s.read(s.conf), "\n") {
		if k, v, ok := strings.Cut(strings.Trim(line, " \t\r"), "="); ok && !strings.HasPrefix(k, "#") {
			f[k] = v
		}
	}
	return f
}

// state is the conf's parsed fields (a save may regroup lines canonically; it
// must never change, add or drop a field) and the settings bytes.
func (s *storeEnv) state() [2]string { return [2]string{toJSON(s.fields()), s.read(s.settings)} }

func (s *storeEnv) jsonOf(text string) map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &v); err != nil {
		s.t.Fatalf("%v: %q", err, text)
	}
	return v
}

func eq(t *testing.T, name string, got, want any) {
	t.Helper()
	if g, w := toJSON(got), toJSON(want); g != w {
		t.Errorf("%s: got %s want %s", name, g, w)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestStoreSetWritesTheConfAndLeavesNativeAlone(t *testing.T) { // test_set_model_..., test_set_effort_...
	s := newStoreEnv(t)
	native := s.read(s.settings)
	s.ok("set", "", StoreChange{Model: "new"})
	f := s.fields()
	if f["model"] != "new" || f["model_defaults"] != "1" || s.read(s.settings) != native {
		t.Fatal(f, s.read(s.settings))
	}
	if got := s.ok("get", "model", StoreChange{}); got != "new\n" {
		t.Fatal(got)
	}
	s.ok("set", "", StoreChange{Effort: "high"})
	if got := s.ok("get", "effort", StoreChange{}); got != "high\n" || s.read(s.settings) != native {
		t.Fatal(got)
	}
}

func TestStoreReadsDoNotWrite(t *testing.T) { // test_first_get_imports_native_value_..., test_model_settings_projection_does_not_write
	s := newStoreEnv(t)
	before := s.state()
	if got := s.ok("get", "model", StoreChange{}); got != "old\n" {
		t.Fatal(got)
	}
	if got := s.jsonOf(s.ok("model-settings", "", StoreChange{})); got["model"] != "old" {
		t.Fatal(got)
	}
	if s.state() != before {
		t.Fatal("a read wrote")
	}
}

func TestStoreFirstSeedImportsEffectiveNativeValues(t *testing.T) { // test_first_seed_imports_effective_native_values_and_leaves_settings_alone
	s := newStoreEnv(t)
	s.write(s.settings, `{"model":"native-model","effortLevel":"high","modelSettings":{"custom-model":{"effortLevel":"low"}},`+
		`"env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"native-opus","UNRELATED":"keep"},"other":true}`)
	native := s.read(s.settings)
	if got := s.ok("seed-model-defaults", "model", StoreChange{}); got != "native-model\n" {
		t.Fatal(got)
	}
	f := s.fields()
	for k, v := range map[string]string{"model": "native-model", "effort": "high", "effort.custom-model": "low",
		"env.ANTHROPIC_DEFAULT_OPUS_MODEL": "native-opus", "model_defaults": "1", "context_window.m": "10000"} {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
	if s.read(s.settings) != native { // including the non-mapping env key, which is not ccmorph's to move
		t.Fatal("native settings changed")
	}
}

func TestStoreSeedIsIdempotent(t *testing.T) { // test_seed_is_idempotent
	s := newStoreEnv(t)
	if got := s.ok("seed-model-defaults", "", StoreChange{}); got != "" {
		t.Fatal(got)
	}
	before := s.state()
	if got := s.ok("seed-model-defaults", "", StoreChange{}); got != "" || s.state() != before {
		t.Fatal("a second seed changed something", got)
	}
}

func TestStoreExplicitConfValuesWinOnFirstSeed(t *testing.T) { // test_explicit_conf_values_win_over_native_on_first_seed
	s := newStoreEnv(t)
	s.write(s.conf, storeConf+"model=conf-model\neffort=low\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=conf-opus\n")
	s.write(s.settings, `{"model":"native-model","effortLevel":"high","env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"native-opus"}}`)
	if s.ok("get", "model", StoreChange{}) != "conf-model\n" || s.ok("get", "effort", StoreChange{}) != "low\n" {
		t.Fatal("the conf did not win a read")
	}
	native := s.read(s.settings)
	if got := s.ok("seed-model-defaults", "model", StoreChange{}); got != "conf-model\n" || s.read(s.settings) != native {
		t.Fatal(got)
	}
	f := s.fields()
	if f["model"] != "conf-model" || f["effort"] != "low" || f["env.ANTHROPIC_DEFAULT_OPUS_MODEL"] != "conf-opus" {
		t.Fatal(f)
	}
}

func TestStoreMigrateNativeIsAnAliasForSeed(t *testing.T) { // test_migrate_native_is_an_alias_for_seed_model_defaults
	s := newStoreEnv(t)
	s.write(s.settings, `{"model":"nm","effortLevel":"high"}`)
	native := s.read(s.settings)
	s.ok("migrate-native", "", StoreChange{})
	migrated := s.read(s.conf)
	s.write(s.conf, storeConf)
	s.ok("seed-model-defaults", "", StoreChange{})
	if s.read(s.settings) != native || s.read(s.conf) != migrated {
		t.Fatal("the two spellings seeded different confs")
	}
}

func TestStoreUnsupportedOperationIsRefusedWithoutWriting(t *testing.T) { // test_unsupported_operation_is_refused_without_writing
	s := newStoreEnv(t)
	before := s.state()
	_, errw, rc := s.run("promote-native", "", StoreChange{})
	if rc == 0 || !strings.Contains(errw, "unsupported configuration operation") || s.state() != before {
		t.Fatal(rc, errw)
	}
}

func TestStoreNativeIsNoLongerAnInputAfterTheSeed(t *testing.T) {
	// test_inline_native_model_and_effort_are_ignored_after_seed
	s := newStoreEnv(t)
	s.write(s.settings, `{"model":"native-model","effortLevel":"high","modelSettings":{"custom-model":{"effortLevel":"low"}}}`)
	s.ok("seed-model-defaults", "", StoreChange{})
	seeded := s.read(s.conf)
	// What an in-session /model and /effort leave behind.
	s.write(s.settings, `{"model":"inline-picked","effortLevel":"xhigh","modelSettings":{"custom-model":{"effortLevel":"low"},"inline-model":{"effortLevel":"max"}}}`)
	s.ok("seed-model-defaults", "", StoreChange{})
	if s.read(s.conf) != seeded || s.ok("get", "model", StoreChange{}) != "native-model\n" || s.ok("get", "effort", StoreChange{}) != "high\n" {
		t.Fatal("inline values leaked into the seeded defaults")
	}
	p := s.jsonOf(s.ok("model-settings", "", StoreChange{}))
	eq(t, "projection", []any{p["model"], p["effortLevel"], p["modelSettings"]},
		[]any{"native-model", "high", map[string]any{"custom-model": map[string]any{"effortLevel": "low"}}})
}

func TestStoreAbsentDefaultsDoNotReviveNativeValues(t *testing.T) { // test_absent_defaults_do_not_revive_native_values
	s := newStoreEnv(t)
	s.write(s.conf, storeConf+"model_defaults=1\n")
	s.write(s.settings, `{"model":"native-model","effortLevel":"high","modelSettings":{"m":{"effortLevel":"low"}},`+
		`"env":{"ANTHROPIC_MODEL":"pin","ANTHROPIC_DEFAULT_OPUS_MODEL":"native-opus"}}`)
	before := s.state()
	if s.ok("get", "model", StoreChange{}) != "\n" || s.ok("get", "effort", StoreChange{}) != "\n" ||
		s.ok("seed-model-defaults", "model", StoreChange{}) != "\n" || s.state() != before {
		t.Fatal("an absent default came back or the seed wrote")
	}
	eq(t, "projection", s.jsonOf(s.ok("model-settings", "", StoreChange{})), map[string]any{"env": map[string]any{}, "modelSettings": map[string]any{}})
	if got := s.ok("get", "env.ANTHROPIC_DEFAULT_OPUS_MODEL", StoreChange{}); got != "\n" {
		t.Fatal(got)
	}
}

func TestStoreRoleMappingIsReadFromTheConf(t *testing.T) { // test_role_mapping_is_read_from_the_conf
	s := newStoreEnv(t)
	s.write(s.conf, storeConf+"env.ANTHROPIC_DEFAULT_OPUS_MODEL=profile-opus\nmodel_defaults=1\n")
	s.write(s.settings, `{"env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"stale-opus"}}`)
	if got := s.ok("get", "env.ANTHROPIC_DEFAULT_OPUS_MODEL", StoreChange{}); got != "profile-opus\n" {
		t.Fatal(got)
	}
}

func TestStorePrepareLaunch(t *testing.T) {
	t.Run("removes only the owned native keys", func(t *testing.T) { // test_prepare_launch_removes_only_owned_native_keys
		s := newStoreEnv(t)
		s.write(s.conf, storeConf+"model_defaults=1\nmodel=configured\neffort=medium\n")
		s.write(s.settings, `{"model":"inline","effortLevel":"high","modelSettings":{"inline":{"effortLevel":"high","thinkingBudget":7}},`+
			`"env":{"ANTHROPIC_MODEL":"pinned","CLAUDE_CODE_EFFORT_LEVEL":"high","ANTHROPIC_DEFAULT_OPUS_MODEL":"stale","CUSTOM":"keep"},`+
			`"enabledPlugins":{"test@example":true},"permissions":{"deny":["Bash(rm *)"]}}`)
		s.ok("prepare-launch", "", StoreChange{})
		eq(t, "settings", s.jsonOf(s.read(s.settings)), map[string]any{
			"modelSettings": map[string]any{"inline": map[string]any{"thinkingBudget": 7}},
			"env":           map[string]any{"CUSTOM": "keep"}, "enabledPlugins": map[string]any{"test@example": true},
			"permissions": map[string]any{"deny": []any{"Bash(rm *)"}}})
		if s.fields()["model"] != "configured" {
			t.Fatal(s.fields())
		}
		before := s.read(s.settings)
		s.ok("prepare-launch", "", StoreChange{})
		if s.read(s.settings) != before {
			t.Fatal("a second prepare rewrote the settings")
		}
	})
	t.Run("keeps intentionally absent defaults absent", func(t *testing.T) { // test_prepare_launch_keeps_intentionally_absent_defaults_absent
		s := newStoreEnv(t)
		s.write(s.conf, storeConf+"model_defaults=1\n")
		s.write(s.settings, `{"model":"inline","effortLevel":"high","modelSettings":{"m":{"effortLevel":"high"}},"env":{"ANTHROPIC_MODEL":"pin"}}`)
		s.ok("prepare-launch", "", StoreChange{})
		eq(t, "projection", s.jsonOf(s.ok("model-settings", "", StoreChange{})), map[string]any{"env": map[string]any{}, "modelSettings": map[string]any{}})
		eq(t, "native", s.jsonOf(s.read(s.settings)), map[string]any{"modelSettings": map[string]any{"m": map[string]any{}}, "env": map[string]any{}})
	})
	t.Run("seeds before clearing and refuses invalid settings", func(t *testing.T) { // test_prepare_launch_seeds_before_clearing_and_refuses_invalid_settings
		s := newStoreEnv(t)
		s.write(s.settings, `{"model":"native","effortLevel":"low"}`)
		s.ok("prepare-launch", "", StoreChange{})
		if s.fields()["effort"] != "low" {
			t.Fatal(s.fields())
		}
		eq(t, "settings", s.jsonOf(s.read(s.settings)), map[string]any{})
		s.write(s.settings, `{"modelSettings": []}`)
		before := s.state()
		if _, _, rc := s.run("prepare-launch", "", StoreChange{}); rc == 0 || s.state() != before {
			t.Fatal("an invalid settings file was accepted or rewritten")
		}
	})
}

func TestStoreSharedNativeSettingsSeedUnderTheProfile(t *testing.T) { // test_shared_native_settings_seed_under_the_profile
	s := newStoreEnv(t)
	s.write(s.shared, `{"effortLevel":"high"}`)
	if got := s.ok("get", "effort", StoreChange{}); got != "high\n" {
		t.Fatal(got)
	}
	s.ok("set", "", StoreChange{Effort: "low"})
	if got := s.ok("get", "effort", StoreChange{}); got != "low\n" {
		t.Fatal(got)
	}
	// One setter seeds every default, so the sibling model resolved from the
	// profile layer is captured too and cannot drift afterwards.
	if s.fields()["model"] != "old" || s.read(s.shared) != `{"effortLevel":"high"}` {
		t.Fatal(s.fields(), s.read(s.shared))
	}
}

func TestStoreRefusesWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		prepare       func(s *storeEnv)
	}{
		{"malformed settings block the save", "could not save/read configuration", func(s *storeEnv) { s.write(s.settings, `{"env": []}`) }}, // test_malformed_settings_block_the_save_and_are_not_overwritten
		{"shared env that is not an object", "shared settings and its env must be objects", func(s *storeEnv) { s.write(s.shared, `{"env": []}`) }},
		{"profile modelSettings that is not an object", "modelSettings must be an object", func(s *storeEnv) { s.write(s.settings, `{"modelSettings": []}`) }},
		{"a native value cannot forge conf fields", "single-line strings", func(s *storeEnv) { s.write(s.settings, `{"model":"a\nmodel_defaults=0"}`) }}, // test_a_native_value_cannot_forge_conf_fields
	} {
		t.Run(tc.name, func(t *testing.T) { // test_malformed_native_layers_block_the_save
			s := newStoreEnv(t)
			tc.prepare(s)
			before := [3]string{s.read(s.conf), s.read(s.settings), s.read(s.shared)}
			for _, op := range []struct {
				op, key string
				ch      StoreChange
			}{{"set", "", StoreChange{Model: "new"}}, {"get", "model", StoreChange{}}} {
				_, errw, rc := s.run(op.op, op.key, op.ch)
				if tc.name == "malformed settings block the save" && op.op == "get" {
					continue
				}
				if rc == 0 || !strings.Contains(errw, tc.message) {
					t.Fatalf("%s: rc %d %q", op.op, rc, errw)
				}
			}
			if after := [3]string{s.read(s.conf), s.read(s.settings), s.read(s.shared)}; after != before {
				t.Fatal("a refused save wrote")
			}
		})
	}
}

func TestStoreSymlinkedSettingsRefuseTheSave(t *testing.T) { // test_symlinked_settings_refuse_the_save (store half)
	s := newStoreEnv(t)
	real := filepath.Join(filepath.Dir(s.conf), "real-settings.json")
	s.write(real, `{"model":"native"}`)
	os.Remove(s.settings)
	os.Symlink(real, s.settings)
	before := [2]string{s.read(s.conf), s.read(real)}
	for _, op := range []struct {
		op, key string
		ch      StoreChange
	}{{"set", "", StoreChange{Model: "new"}}, {"seed-model-defaults", "model", StoreChange{}}} {
		out, errw, rc := s.run(op.op, op.key, op.ch)
		if rc == 0 || out != "" || !strings.Contains(errw, "profile-local files") {
			t.Fatalf("%s: rc %d out %q err %q", op.op, rc, out, errw)
		}
	}
	if [2]string{s.read(s.conf), s.read(real)} != before {
		t.Fatal("a refused save wrote")
	}
}

func TestStoreExternalChangeAfterSnapshotIsPreserved(t *testing.T) { // test_external_change_after_snapshot_is_preserved
	s := newStoreEnv(t)
	snapshot := strings.TrimSpace(s.ok("snapshot", "", StoreChange{}))
	f, _ := os.OpenFile(s.conf, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("# external change\n")
	f.Close()
	settings := s.read(s.settings)
	_, errw, rc := s.run("set", "", StoreChange{Model: "new", Snapshot: snapshot})
	if rc == 0 || !strings.Contains(errw, "changed while configuring") ||
		!strings.HasSuffix(s.read(s.conf), "# external change\n") || strings.Contains(s.read(s.conf), "model=new") || s.read(s.settings) != settings {
		t.Fatal(rc, errw, s.read(s.conf))
	}
}

// The store and ctxwindow must refuse the same model ids: one the store saves
// and the context reader silently drops is a value lost without a word.
func TestStoreAndCtxwindowAgreeOnModelIds(t *testing.T) {
	for r := rune(0); r <= 0x3000; r++ {
		if r == '=' {
			continue
		}
		id := "a" + string(r) + "b"
		if hasSpace(id) == ctxwindow.ValidModel(id) {
			t.Fatalf("rune %U: store rejects=%v ctxwindow accepts=%v", r, hasSpace(id), ctxwindow.ValidModel(id))
		}
	}
	s := newStoreEnv(t)
	before := s.state()
	_, errw, rc := s.run("set", "", StoreChange{Model: "a\x1cb"})
	if rc == 0 || !strings.Contains(errw, "model must be a nonempty model id") {
		t.Fatalf("rc=%d %q", rc, errw)
	}
	if s.state() != before {
		t.Fatal("a refused model wrote")
	}
}
