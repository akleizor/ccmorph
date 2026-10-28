package modelwindow

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akleizor/ccmorph/pycompat"
)

type env struct {
	root, state, a, b string
}

// setup mirrors tests/profile_context_store_test.py's setUp: two routed profiles
// in an isolated state dir.
func setup(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{root: root, state: filepath.Join(root, "state")}
	for _, k := range []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "MODEL_WINDOW_PROFILE_CONF", "MODEL_WINDOW_ENDPOINT_JSON", "CCMORPH_PROFILE_LOCK_HELD", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", root)
	t.Setenv("CCMORPH_STATE_DIR", e.state)
	if err := os.MkdirAll(e.state, 0o755); err != nil {
		t.Fatal(err)
	}
	e.a, e.b = filepath.Join(e.state, "a.conf"), filepath.Join(e.state, "b.conf")
	write(t, e.a, "# keep\ntype=apikey\nbase_url=https://a.invalid\napi_key=canary-a\n")
	if err := os.Chmod(e.a, 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, e.b, "type=apikey\nbase_url=https://b.invalid\napi_key=canary-b\n")
	ShareDir = func() string { return filepath.Join(root, "share") }
	t.Cleanup(func() { ShareDir = func() string { return filepath.Join(ExecDir(), "..", "share") } })
	return e
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func one(m string, v int64) *pycompat.OMap[int64] {
	o := pycompat.NewOMap[int64]()
	o.Set(m, v)
	return o
}

func save(profile string, values *pycompat.OMap[int64]) bool {
	return SaveDiscovered(profile, values, ProfileIdentity(profile), true, nil, nil)
}

func window(profile, model string) Record { return Resolve(profile, model) }

func raises(t *testing.T, fn func()) *pycompat.Exc {
	t.Helper()
	e := pycompat.Catch(fn)
	if e == nil {
		t.Fatal("expected an exception")
	}
	return e
}

// Python: test_discovery_and_manual_values_share_conf_and_stay_profile_scoped.
func TestDiscoveryAndManualValuesShareConfAndStayProfileScoped(t *testing.T) {
	e := setup(t)
	if !save("a", one("m", 123000)) {
		t.Fatal("save failed")
	}
	if got := window("a", "m").Window; got != 123000 {
		t.Fatal(got)
	}
	if !window("b", "m").Unprobed {
		t.Fatal("b must not see a's window")
	}
	SetContext("a", "m", "654321")
	if got := window("a", "m").Window; got != 654321 {
		t.Fatal(got)
	}
	SetContext("a", "m", "auto")
	if got := window("a", "m").Window; got != 123000 {
		t.Fatal(got)
	}
	text := read(t, e.a)
	if !strings.Contains(text, "# keep\n") || !strings.Contains(text, "api_key=canary-a\n") {
		t.Fatal(text)
	}
	if fi, _ := os.Stat(e.a); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(e.state, "model-registry")); err == nil {
		t.Fatal("a registry file appeared")
	}
}

// Python: test_unicode_line_separators_in_unrelated_values_are_preserved.
func TestUnicodeLineSeparatorsInUnrelatedValuesArePreserved(t *testing.T) {
	e := setup(t)
	write(t, e.a, "type=apikey\napi_key=before\u0085context_window.m=keep after\nenv.ANTHROPIC_AUTH_TOKEN=first\u0085second\n")
	_, fields := ReadConf(e.a)
	if fields["api_key"] != "before\u0085context_window.m=keep after" || fields["env.ANTHROPIC_AUTH_TOKEN"] != "first\u0085second" {
		t.Fatal(fields)
	}
	SetContext("a", "m", "300000")
	save("a", one("other", 400000))
	_, after := ReadConf(e.a)
	if after["api_key"] != "before\u0085context_window.m=keep after" || after["env.ANTHROPIC_AUTH_TOKEN"] != "first\u0085second" || after["context_window.m"] != "300000" {
		t.Fatal(after)
	}
}

// Python: test_route_change_invalidates_discovery_but_not_manual_pin.
func TestRouteChangeInvalidatesDiscoveryButNotManualPin(t *testing.T) {
	e := setup(t)
	save("a", one("m", 123000))
	SetContext("a", "manual", "300000")
	write(t, e.a, strings.ReplaceAll(read(t, e.a), "https://a.invalid", "https://other.invalid"))
	if !window("a", "m").Unprobed || window("a", "manual").Window != 300000 {
		t.Fatal("route change must drop discovery and keep the pin")
	}
}

// Python: test_deleted_recreated_renamed_or_edited_profile_rejects_stale_commit.
func TestDeletedRecreatedRenamedOrEditedProfileRejectsStaleCommit(t *testing.T) {
	for _, mutation := range []string{"delete", "recreate", "rename", "manual"} {
		t.Run(mutation, func(t *testing.T) {
			e := setup(t)
			write(t, e.a, "type=apikey\napi_key=canary\n")
			identity := ProfileIdentity("a")
			switch mutation {
			case "manual":
				SetContext("a", "m", "400000")
			case "rename":
				os.Rename(e.a, filepath.Join(e.state, "renamed.conf"))
			default:
				os.Remove(e.a)
				if mutation == "recreate" {
					write(t, e.a, "type=apikey\napi_key=canary\n")
				}
			}
			if SaveDiscovered("a", one("m", 900000), identity, true, nil, nil) {
				t.Fatal("stale commit accepted")
			}
			if b, err := os.ReadFile(e.a); err == nil && strings.Contains(string(b), "900000") {
				t.Fatal("stale value written")
			}
		})
	}
}

// Python: test_exact_manual_keys_preserve_credentials_and_neighbours.
func TestExactManualKeysPreserveCredentialsAndNeighbours(t *testing.T) {
	e := setup(t)
	write(t, e.a, "type=apikey\napi_key=context_window.a.b=keep\ncontext_window.axb=500000\n")
	SetContext("a", "a.b", "131072")
	if !strings.Contains(read(t, e.a), "api_key=context_window.a.b=keep") {
		t.Fatal(read(t, e.a))
	}
	if window("a", "axb").Window != 500000 || ContextValue("a", "a.b") != "131072" {
		t.Fatal("neighbour or value lost")
	}
	for _, c := range [][2]string{{"bad\nkey", "100000"}, {"a=b", "100000"}, {"m", "0"}, {"m", "-1"}} {
		if x := raises(t, func() { SetContext("a", c[0], c[1]) }); x.Kind != pycompat.ValueError {
			t.Fatalf("%v: %v", c, x)
		}
	}
}

func discard() *bytes.Buffer { return &bytes.Buffer{} }

// Python: test_migration_keeps_scoped_data_and_only_attributed_bare_data.
func TestMigrationKeepsScopedDataAndOnlyAttributedBareData(t *testing.T) {
	e := setup(t)
	legacy := filepath.Join(e.state, "model-registry")
	write(t, legacy, "a:scoped=300000\na:wild*=350000\nb:scoped=400000\nbare=500000\nunknown=600000\n")
	cache := filepath.Join(e.root, ".claude-profiles/a/cache/context-window")
	write(t, cache, "bare\t500000\nlocal:tag\t65536\n")
	SetContext("a", "scoped", "250000")
	if Migrate(discard()) != 0 {
		t.Fatal("migrate failed")
	}
	for _, c := range []struct {
		profile, model string
		want           int64
	}{{"a", "scoped", 250000}, {"b", "scoped", 400000}, {"a", "wild-id", 350000}, {"a", "bare", 500000}, {"a", "local:tag", 65536}} {
		if got := window(c.profile, c.model).Window; got != c.want {
			t.Errorf("%s %s: %d, want %d", c.profile, c.model, got, c.want)
		}
	}
	if !window("b", "bare").Unprobed || !window("a", "unknown").Unprobed {
		t.Fatal("unattributed bare rows must not apply")
	}
	for _, gone := range []string{legacy, cache} {
		if _, err := os.Stat(gone); err == nil {
			t.Fatalf("%s should be archived", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(e.state, "model-registry.migrated")); err != nil {
		t.Fatal(err)
	}
	before := read(t, e.a)
	if Migrate(discard()) != 0 || read(t, e.a) != before {
		t.Fatal("a second migration changed the conf")
	}
}

// Python: test_migration_preview_reports_counts_without_writing.
func TestMigrationPreviewReportsCountsWithoutWriting(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.state, "model-registry"), "a:m=321000\nunknown=500000\n")
	snapshot := func() string {
		var b strings.Builder
		entries, _ := os.ReadDir(e.state)
		for _, en := range entries {
			b.WriteString(en.Name() + "=" + read(t, filepath.Join(e.state, en.Name())) + "\n")
		}
		return b.String()
	}
	before := snapshot()
	preview := MigrationPreview()
	if snapshot() != before {
		t.Fatal("preview wrote")
	}
	if !strings.Contains(preview, `"a": {"cached": 0, "scoped": 1}`) || !strings.Contains(preview, `"unattributed_entries": 1`) || strings.Contains(preview, "canary") {
		t.Fatal(preview)
	}
}

// Python: test_concurrent_migrations_archive_once_and_preserve_values.
func TestConcurrentMigrationsArchiveOnceAndPreserveValues(t *testing.T) {
	e := setup(t)
	legacy := filepath.Join(e.state, "model-registry")
	write(t, legacy, "a:m=321000\n")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = Migrate(discard())
		}()
	}
	wg.Wait()
	if codes[0] != 0 || codes[1] != 0 || window("a", "m").Window != 321000 {
		t.Fatal(codes)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy + ".migrated.1"); err == nil {
		t.Fatal("archived twice")
	}
}

// Python: test_symlink_cache_refuses_migration_without_archiving_registry.
func TestSymlinkCacheRefusesMigrationWithoutArchivingRegistry(t *testing.T) {
	e := setup(t)
	legacy := filepath.Join(e.state, "model-registry")
	write(t, legacy, "a:m=321000\n")
	cache := filepath.Join(e.root, ".claude-profiles/a/cache/context-window")
	os.MkdirAll(filepath.Dir(cache), 0o755)
	os.Symlink(e.b, cache)
	if x := raises(t, func() { Migrate(discard()) }); x.Kind != pycompat.OSError {
		t.Fatal(x)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("registry was archived")
	}
	if strings.Contains(read(t, e.a), "context_discovered") {
		t.Fatal("a value was applied")
	}
}

// Python: test_resolver_never_falls_back_to_legacy_registry_or_profile_cache.
func TestResolverNeverFallsBackToLegacyRegistryOrProfileCache(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.state, "model-registry"), "a:m=900000\nm=900000\n")
	if !window("a", "m").Unprobed {
		t.Fatal("legacy registry was read")
	}
}

// Python: test_migration_never_archives_cache_without_owning_profile.
func TestMigrationNeverArchivesCacheWithoutOwningProfile(t *testing.T) {
	e := setup(t)
	cache := filepath.Join(e.root, ".claude-profiles/unrelated/cache/context-window")
	write(t, cache, "m\t300000\n")
	Migrate(discard())
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("cache without a profile was archived")
	}
}

// Python: test_explicit_recovery_imports_profile_backups_without_overwriting_current.
func TestExplicitRecoveryImportsProfileBackupsWithoutOverwritingCurrent(t *testing.T) {
	e := setup(t)
	cache := filepath.Join(e.root, ".claude-profiles/a/cache/context-window.migrated")
	write(t, cache, "m\t300000\nknown\t400000\n")
	save("a", one("known", 500000))
	RecoverCacheBackups(discard())
	if window("a", "m").Window != 300000 || window("a", "known").Window != 500000 {
		t.Fatal("recovery overwrote or skipped")
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("backup removed")
	}
}

// Python: test_cache_entry_rules_match_preview_migration_and_recovery.
func TestCacheEntryRulesMatchPreviewMigrationAndRecovery(t *testing.T) {
	e := setup(t)
	rows := "tag:variant[1m]\t300000\ntag:variant\t400000\nbad model\t100000\nzero\t0\nnegative\t-1\nextra\t200000\textra\nmissing-tab\n"
	write(t, filepath.Join(e.root, ".claude-profiles/a/cache/context-window"), rows)
	if !strings.Contains(MigrationPreview(), `"a": {"cached": 2, "scoped": 0}`) {
		t.Fatal(MigrationPreview())
	}
	Migrate(discard())
	migrated := ProfileWindows("a")
	if len(migrated) != 1 || migrated["tag:variant"] != 400000 {
		t.Fatal(migrated)
	}
	backup := filepath.Join(e.root, ".claude-profiles/b/cache/context-window.migrated")
	write(t, backup, rows)
	RecoverCacheBackups(discard())
	got := ProfileWindows("b")
	if len(got) != 1 || got["tag:variant"] != 400000 || read(t, backup) != rows {
		t.Fatal(got)
	}
}

// Python: test_discovered_json_does_not_contain_credentials.
func TestDiscoveredJSONDoesNotContainCredentials(t *testing.T) {
	e := setup(t)
	save("a", one("m", 123000))
	var line string
	for _, l := range strings.Split(read(t, e.a), "\n") {
		if strings.HasPrefix(l, "context_discovered=") {
			line = l
		}
	}
	v, err := pycompat.Parse([]byte(strings.TrimPrefix(line, "context_discovered=")))
	if err != nil {
		t.Fatal(err)
	}
	models, _ := v.(*pycompat.Obj).Get("models")
	if got := pycompat.Marshal(models, pycompat.Format{Compact: true}); got != `{"m":123000}` {
		t.Fatal(got)
	}
	if strings.Contains(line, "canary-a") || strings.Contains(line, "https://a.invalid") {
		t.Fatal("route leaked into the record")
	}
}

// Python: model_window_test.test_model_choice_does_not_change_the_route.
func TestModelChoiceDoesNotChangeTheRoute(t *testing.T) {
	base := map[string]string{"type": "apikey", "base_url": "https://gw.invalid", "api_key": "k"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	mapped := with(map[string]string{"env.ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5[1m]", "env.ANTHROPIC_SMALL_FAST_MODEL": "gpt-6-luna", "env.ANTHROPIC_MODEL": "x"})
	if RouteFingerprint(mapped) != RouteFingerprint(base) {
		t.Fatal("model choice changed the route")
	}
	if RouteFingerprint(with(map[string]string{"env.ANTHROPIC_CUSTOM_HEADERS": "x-route: eu"})) == RouteFingerprint(base) {
		t.Fatal("headers must change the route")
	}
}

// Python: model_window_test.test_every_shell_mapping_key_is_excluded_from_the_route.
func TestEveryShellMappingKeyIsExcludedFromTheRoute(t *testing.T) {
	root := filepath.Join("..", "lib")
	files, _ := filepath.Glob(filepath.Join(root, "ccmorph*.zsh"))
	files = append(files, filepath.Join(root, "ccmorph.sh"))
	var shell strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		shell.Write(b)
	}
	_, rest, ok := strings.Cut(shell.String(), "_CCMORPH_MODEL_MAPPING_ENV=(\n")
	if !ok {
		t.Fatal("mapping list not found")
	}
	block, _, _ := strings.Cut(rest, "\n)")
	n := 0
	for _, l := range strings.Split(block, "\n") {
		if key := strings.TrimSpace(l); strings.HasPrefix(key, "ANTHROPIC_") {
			n++
			if !modelChoiceEnv.MatchString("env." + key) {
				t.Errorf("%s is not excluded from the route", key)
			}
		}
	}
	if n == 0 {
		t.Fatal("no keys found")
	}
}

// Python: model_window_test.test_real_id_and_exclusion_share_one_rule.
func TestRealIDAndExclusionShareOneRule(t *testing.T) {
	if RealID("claude-fable-5-dd-los-6-tpg") != "gpt-6-sol" || RealID("claude-opus-5") != "claude-opus-5" {
		t.Fatal("RealID")
	}
	fields := map[string]string{"excluded_models": "gpt-6-sol,claude-opus-5", "env.ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5[1m]"}
	if !IsExcluded(fields, "claude-fable-5-dd-los-6-tpg") || !IsExcluded(fields, "opus[1m]") || IsExcluded(fields, "gpt-6-luna") {
		t.Fatal("IsExcluded")
	}
}

// Python: model_window_test.ConfLayout.
func TestCanonicalConfGroupsKeysKeepsCommentsAndValues(t *testing.T) {
	messy := "context_discovered={}\n# why the shim flag\nenv.CCMORPH_RANDOM_DEVICE_ID=1\nmodel=m\napi_key=k\nmodel_defaults=1\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=o\ntype=apikey\ncontext_window.m=1000\neffort=high\n\nbase_url=https://g.invalid\n"
	tidy := CanonicalConf(messy)
	want := "type=apikey\nbase_url=https://g.invalid\napi_key=k\n\nmodel=m\neffort=high\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=o\n\n# why the shim flag\nenv.CCMORPH_RANDOM_DEVICE_ID=1\n\ncontext_window.m=1000\n\nmodel_defaults=1\ncontext_discovered={}\n"
	if tidy != want {
		t.Fatalf("got %q", tidy)
	}
	if CanonicalConf(tidy) != tidy {
		t.Fatal("not idempotent")
	}
}

func TestARemovedRoleKeyIsNotLaidOutWithTheModelKeys(t *testing.T) {
	if got := CanonicalConf("model=m\nrole=r\neffort=high\n"); got != "model=m\neffort=high\n\nrole=r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestAContextWriteLandsTheFileInCanonicalLayout(t *testing.T) {
	e := setup(t)
	conf := filepath.Join(e.root, "p.conf")
	write(t, conf, "model=m\ntype=apikey\napi_key=k\n")
	v := "5000"
	if !ReplaceFields(conf, []Field{{"context_window.m", &v}}, nil) {
		t.Fatal("replace failed")
	}
	if got := read(t, conf); got != "type=apikey\napi_key=k\n\nmodel=m\n\ncontext_window.m=5000\n" {
		t.Fatalf("got %q", got)
	}
}

func TestFnmatchFollowsPython(t *testing.T) {
	for _, c := range []struct {
		name, pat string
		want      bool
	}{{"glm-5.2", "glm-*", true}, {"glm", "glm-*", false}, {"za", "z[ab]", true}, {"zc", "z[!ab]", true}, {"zb", "z[!ab]", false}, {"q1", "q?", true}, {"a[", "a[", true}, {"x", "[]", false}} {
		if got := pycompat.Fnmatch(c.name, c.pat); got != c.want {
			t.Errorf("fnmatch(%q,%q)=%v", c.name, c.pat, got)
		}
	}
}

func TestBundledCatalogueSizesAnUnknownRoutedModelByFamily(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.root, "share/model-catalogue.json"),
		`{"models":{"x/fam-1":{"family":"fam","limit":{"context":300000}},"y/fam-2":{"family":"fam","limit":{"context":300000,"input":250000}}}}`)
	_ = e
	// Two family members with different values are ambiguous; equal values resolve.
	if got := Resolve("a", "fam"); !got.Unprobed {
		t.Fatalf("ambiguous family resolved: %+v", got)
	}
	write(t, filepath.Join(e.root, "share/model-catalogue.json"),
		`{"models":{"x/fam-1":{"family":"fam","limit":{"context":300000}},"y/fam-2":{"family":"fam","limit":{"context":300000}}}}`)
	if got := Resolve("a", "fam"); got.Window != 300000 || got.Source != "catalogue-bundled" {
		t.Fatalf("got %+v", got)
	}
}
