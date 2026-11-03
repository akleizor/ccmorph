package settingscompose

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Ported from tests/settings_compose_test.py, where each case ran the helper
// binary in a subprocess; here they run Main in-process on the same files.

const advise = `[ -n "$CCMORPH_ADVISE" ] && printf "%s" "$CCMORPH_ADVISE" || true`

var bom = []byte{0xef, 0xbb, 0xbf}

type home struct {
	t                  *testing.T
	canonical, profile string
	dir                string
}

func newHome(t *testing.T) home {
	dir := t.TempDir()
	h := home{t, filepath.Join(dir, "claude", "settings.json"), filepath.Join(dir, "profile"), dir}
	os.MkdirAll(filepath.Dir(h.canonical), 0o755)
	os.MkdirAll(h.profile, 0o755)
	return h
}

func (h home) p(parts ...string) string {
	return filepath.Join(append([]string{h.profile}, parts...)...)
}

func (h home) put(path string, doc string) {
	h.t.Helper()
	write(h.t, path, doc)
}

func (h home) bytes(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func parseJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %q", err, b)
	}
	return m
}

func (h home) get(path string) map[string]any { return parseJSON(h.t, h.bytes(path)) }

func same(t *testing.T, name string, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	g, _ := json.Marshal(got)
	var gv any
	json.Unmarshal(g, &gv)
	if !reflect.DeepEqual(gv, w) {
		t.Errorf("%s: got %s want %s", name, g, want)
	}
}

func (h home) main(argv ...string) (int, string) {
	var stderr bytes.Buffer
	return Main(argv, &stderr), stderr.String()
}

func (h home) materialize(saved string, extra ...string) (int, string) {
	return h.main(append([]string{"materialize", h.profile, "--canonical", h.canonical, "--saved", saved}, extra...)...)
}

// run projects and returns settings.json without the advisory hook every projection adds.
func (h home) run(extra ...string) map[string]any { return h.runSaved("{}", extra...) }

func (h home) runSaved(saved string, extra ...string) map[string]any {
	h.t.Helper()
	if rc, stderr := h.materialize(saved, extra...); rc != 0 {
		h.t.Fatalf("materialize: %d %s", rc, stderr)
	}
	doc := h.get(h.p("settings.json"))
	hooks, _ := doc["hooks"].(map[string]any)
	var kept []any
	for _, e := range listOf(hooks["SessionStart"]) {
		isAdvise := false
		for _, hk := range listOf(e.(map[string]any)["hooks"]) {
			if hk.(map[string]any)["command"] == advise {
				isAdvise = true
			}
		}
		if !isAdvise {
			kept = append(kept, e)
		}
	}
	if hooks != nil {
		if len(kept) > 0 {
			hooks["SessionStart"] = kept
		} else {
			delete(hooks, "SessionStart")
			if len(hooks) == 0 {
				delete(doc, "hooks")
			}
		}
	}
	return doc
}

func listOf(v any) []any { l, _ := v.([]any); return l }

func (h home) fold() {
	h.t.Helper()
	if rc, stderr := h.main("fold", h.profile, "--canonical", h.canonical); rc != 0 {
		h.t.Fatalf("fold: %d %s", rc, stderr)
	}
}

// edit rewrites a JSON file through fn, the way Claude Code edits settings.json.
func (h home) edit(path string, fn func(m map[string]any)) {
	m := h.get(path)
	fn(m)
	b, _ := json.Marshal(m)
	h.put(path, string(b))
}

func (h home) linkPolicy(doc string) string {
	shared := filepath.Join(h.dir, "agents", "policy.json")
	h.put(shared, doc)
	if err := os.Symlink(shared, h.p("settings.policy.json")); err != nil {
		h.t.Fatal(err)
	}
	return shared
}

func (h home) breakTheLock() {
	lock := h.p("cache", ".settings.lock")
	os.Remove(lock)
	os.MkdirAll(lock, 0o755)
}

const guard = `{"matcher":"Bash","hooks":[{"type":"command","command":"guard"}]}`

func TestComposeHooks(t *testing.T) {
	t.Run("first capture drops inherited hooks but keeps additions", func(t *testing.T) { // test_first_capture_drops_inherited_hooks_but_keeps_additions
		h := newHome(t)
		added := `{"matcher":"Bash","hooks":[{"type":"command","command":"profile-extra"}]}`
		h.put(h.canonical, `{"hooks":{"PreToolUse":[`+guard+`]}}`)
		h.put(h.p("settings.json"), `{"hooks":{"PreToolUse":[`+guard+`,`+added+`]}}`)
		same(t, "settings", h.run()["hooks"], `{"PreToolUse":[`+guard+`,`+added+`]}`)
		same(t, "layer", h.get(h.p("settings.profile.json"))["hooks"], `{"PreToolUse":[`+added+`]}`)
	})
	t.Run("profile tombstones cannot remove the canonical guard", func(t *testing.T) { // test_profile_hook_tombstones_cannot_remove_canonical_guard
		h := newHome(t)
		h.put(h.canonical, `{"hooks":{"PreToolUse":[`+guard+`]}}`)
		h.put(h.p("settings.profile.json"), `{"__unset__":[["hooks"],["hooks","PreToolUse"]]}`)
		same(t, "settings", h.run()["hooks"], `{"PreToolUse":[`+guard+`]}`)
	})
	t.Run("an existing layer cannot duplicate an inherited hook", func(t *testing.T) { // test_existing_layer_cannot_duplicate_inherited_hook
		h := newHome(t)
		h.put(h.canonical, `{"hooks":{"PreToolUse":[`+guard+`]}}`)
		h.put(h.p("settings.profile.json"), `{"hooks":{"PreToolUse":[`+guard+`]}}`)
		same(t, "settings", h.run()["hooks"], `{"PreToolUse":[`+guard+`]}`)
	})
	t.Run("the advisory hook is added once and never folded", func(t *testing.T) { // test_the_advisory_hook_is_added_once_and_never_folded
		h := newHome(t)
		h.put(h.canonical, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"mine"}]}]}}`)
		for range 3 {
			if rc, stderr := h.materialize("{}"); rc != 0 {
				t.Fatal(rc, stderr)
			}
		}
		var commands []any
		for _, e := range listOf(h.get(h.p("settings.json"))["hooks"].(map[string]any)["SessionStart"]) {
			for _, hk := range listOf(e.(map[string]any)["hooks"]) {
				commands = append(commands, hk.(map[string]any)["command"])
			}
		}
		same(t, "commands", commands, `["mine",`+quote(advise)+`]`)
		if _, ok := h.get(h.p("settings.profile.json"))["hooks"]; ok {
			t.Fatal("the advisory hook was folded into the layer")
		}
	})
	t.Run("the first release's file-based hook is never folded", func(t *testing.T) { // test_the_first_releases_file_based_hook_is_never_folded
		h := newHome(t)
		h.put(h.canonical, `{}`)
		legacy := quote(`f="$CCMORPH_ADVISE_FILE"; [ -n "$f" ] && [ -f "$f" ] && cat "$f" || true`)
		old := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":` + legacy + `}]}]}}`
		h.put(h.p("settings.json"), old)
		h.put(h.p("settings.profile.json"), `{}`)
		h.put(h.p("cache", "settings.base.json"), old)
		h.put(h.p("settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":`+legacy+`}]}],"Stop":[{"hooks":[]}]}}`)
		h.run()
		// Hooks never fold from settings.json into the layer at all -- not the
		// legacy advisory, not a genuine edit like the Stop entry added here.
		same(t, "layer", h.get(h.p("settings.profile.json")), `{}`)
	})
	t.Run("hooks never fold from settings.json into the layer", func(t *testing.T) { // test_hooks_never_fold_from_settings_json_into_the_layer
		h := newHome(t)
		h.put(h.canonical, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"canon"}]}]}}`)
		h.run()
		h.put(h.p("settings.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"canon"},{"type":"command","command":"added-by-editor"}]}]}}`)
		h.fold()
		if _, ok := h.get(h.p("settings.profile.json"))["hooks"]; ok {
			t.Fatal("a hook added to settings.json was folded")
		}
	})
	t.Run("a sync write to canonical and relaunch never duplicates hooks", func(t *testing.T) { // test_a_sync_write_to_canonical_and_relaunch_never_duplicates_hooks
		h := newHome(t)
		canon := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"canon-a"}]}]}}`
		stop := func(doc map[string]any) []any {
			var out []any
			hooks, _ := doc["hooks"].(map[string]any)
			for _, e := range listOf(hooks["Stop"]) {
				for _, hk := range listOf(e.(map[string]any)["hooks"]) {
					out = append(out, hk.(map[string]any)["command"])
				}
			}
			return out
		}
		h.put(h.canonical, canon)
		h.run()
		same(t, "first", stop(h.run()), `["canon-a"]`)
		// An external writer rewrites settings.json with canonical's hooks baked in.
		h.put(h.p("settings.json"), string(h.bytes(h.p("settings.json"))))
		h.fold()
		h.put(h.canonical, canon)
		for range 3 {
			same(t, "relaunch", stop(h.run()), `["canon-a"]`)
		}
		if _, ok := h.get(h.p("settings.profile.json"))["hooks"]; ok {
			t.Fatal("canonical's hooks were folded into the layer")
		}
	})
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestComposeLayering(t *testing.T) {
	t.Run("a canonical change reaches the profile", func(t *testing.T) { // test_a_canonical_change_reaches_the_profile
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.run()
		h.put(h.canonical, `{"theme":"light"}`)
		same(t, "theme", h.run()["theme"], `"light"`)
	})
	t.Run("claude edits stay with the profile across canonical changes", func(t *testing.T) { // test_claude_edits_stay_with_the_profile_across_canonical_changes
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark","permissions":{"allow":["Read"]}}`)
		h.run()
		h.edit(h.p("settings.json"), func(m map[string]any) {
			p := m["permissions"].(map[string]any)
			p["allow"] = append(p["allow"].([]any), "Bash(npm test)")
			m["outputStyle"] = "Concise"
		})
		h.put(h.canonical, `{"theme":"light","permissions":{"allow":["Read"]}}`)
		doc := h.run()
		same(t, "theme", doc["theme"], `"light"`)
		same(t, "allow", doc["permissions"].(map[string]any)["allow"], `["Read","Bash(npm test)"]`)
		same(t, "style", doc["outputStyle"], `"Concise"`)
		same(t, "layer", h.get(h.p("settings.profile.json"))["permissions"].(map[string]any)["allow"], `["Read","Bash(npm test)"]`)
	})
	t.Run("removing a canonical key in the profile sticks", func(t *testing.T) { // test_removing_a_canonical_key_in_the_profile_sticks
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark","statusLine":{"type":"command","command":"x"}}`)
		h.run()
		h.edit(h.p("settings.json"), func(m map[string]any) { delete(m, "statusLine") })
		for range 2 {
			if _, ok := h.run()["statusLine"]; ok {
				t.Fatal("a removed key came back")
			}
		}
		h.edit(h.p("settings.json"), func(m map[string]any) { m["statusLine"] = map[string]any{"type": "command", "command": "y"} })
		same(t, "statusLine", h.run()["statusLine"].(map[string]any)["command"], `"y"`)
	})
	t.Run("in-session model and effort choices stay session-local", func(t *testing.T) { // test_in_session_model_and_effort_choices_stay_session_local
		h := newHome(t)
		h.put(h.canonical, `{}`)
		saved := `{"model":"gw-default","effortLevel":"medium","env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"gw-big"}}`
		h.runSaved(saved, "--mapping", "ANTHROPIC_DEFAULT_OPUS_MODEL")
		h.edit(h.p("settings.json"), func(m map[string]any) { m["model"], m["effortLevel"] = "picked-in-session", "max" })
		doc := h.runSaved(saved, "--mapping", "ANTHROPIC_DEFAULT_OPUS_MODEL")
		if doc["model"] != "gw-default" || doc["effortLevel"] != "medium" {
			t.Fatal(doc)
		}
		if _, ok := h.get(h.p("settings.profile.json"))["model"]; ok {
			t.Fatal("the session's model reached the layer")
		}
	})
	t.Run("launch environment never lands in the env block", func(t *testing.T) { // test_launch_environment_never_lands_in_the_env_block
		h := newHome(t)
		h.put(h.canonical, `{"env":{"ENABLE_TOOL_SEARCH":"true","ANTHROPIC_BASE_URL":"http://stale"}}`)
		same(t, "env", h.run("--strip", "ANTHROPIC_BASE_URL", "--strip", "ANTHROPIC_API_KEY")["env"], `{"ENABLE_TOOL_SEARCH":"true"}`)
	})
	t.Run("a derived compact window fills a gap but never overrules the user", func(t *testing.T) { // test_a_derived_compact_window_fills_a_gap_but_never_overrules_the_user
		h := newHome(t)
		h.put(h.canonical, `{}`)
		same(t, "derived", h.run("--acw", "400000")["autoCompactWindow"], `400000`)
		same(t, "kept", h.run()["autoCompactWindow"], `400000`) // keep reuses the last derived value
		h.edit(h.p("settings.json"), func(m map[string]any) { m["autoCompactWindow"] = 250000 })
		same(t, "user's", h.run("--acw", "400000")["autoCompactWindow"], `250000`)
	})
	t.Run("an unchanged projection is not rewritten and a change is backed up", func(t *testing.T) { // test_an_unchanged_projection_is_not_rewritten_and_a_change_is_backed_up
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.run()
		stamp, _ := os.Stat(h.p("settings.json"))
		h.run()
		if again, _ := os.Stat(h.p("settings.json")); !again.ModTime().Equal(stamp.ModTime()) {
			t.Fatal("an unchanged projection was rewritten")
		}
		h.put(h.canonical, `{"theme":"light"}`)
		h.run()
		backups, _ := filepath.Glob(h.p("backups", "settings.*.json"))
		found := false
		for _, b := range backups {
			found = found || h.get(b)["theme"] == "dark"
		}
		if !found {
			t.Fatal("the replaced settings were not backed up", backups)
		}
	})
	t.Run("a missing settings file is regenerated, not read as removals", func(t *testing.T) { // test_a_missing_settings_file_is_regenerated_not_read_as_removals
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.put(h.p("settings.json"), `{"outputStyle":"Concise"}`)
		h.run()
		os.Remove(h.p("settings.json"))
		same(t, "regenerated", h.run(), `{"theme":"dark","outputStyle":"Concise"}`)
	})
	t.Run("a removed layer keeps only edits made since the last projection", func(t *testing.T) { // test_a_removed_layer_keeps_only_edits_made_since_the_last_projection
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.put(h.p("settings.json"), `{"outputStyle":"Concise"}`)
		h.run()
		os.Remove(h.p("settings.profile.json"))
		h.edit(h.p("settings.json"), func(m map[string]any) { m["verbose"] = true })
		same(t, "settings", h.run(), `{"theme":"dark","verbose":true}`)
	})
	t.Run("permission rules from canonical, policy and layer all apply", func(t *testing.T) { // test_permission_rules_from_canonical_policy_and_layer_all_apply
		h := newHome(t)
		h.put(h.canonical, `{"permissions":{"deny":["Bash(shutdown*)"],"defaultMode":"default"}}`)
		h.linkPolicy(`{"permissions":{"deny":["Read(~/.hermes/**)"]}}`)
		h.put(h.p("settings.profile.json"), `{"permissions":{"deny":["WebSearch","Bash(shutdown*)"]}}`)
		same(t, "deny", h.run()["permissions"].(map[string]any)["deny"], `["Bash(shutdown*)","Read(~/.hermes/**)","WebSearch"]`)
	})
	t.Run("a profile cannot drop a canonical deny rule", func(t *testing.T) { // test_a_profile_cannot_drop_a_canonical_deny_rule
		h := newHome(t)
		h.put(h.canonical, `{"permissions":{"deny":["Bash(shutdown*)"]}}`)
		h.put(h.p("settings.profile.json"), `{}`)
		h.run()
		h.edit(h.p("settings.json"), func(m map[string]any) { m["permissions"].(map[string]any)["deny"] = []any{"WebSearch"} })
		h.fold()
		same(t, "layer", h.get(h.p("settings.profile.json"))["permissions"].(map[string]any)["deny"], `["WebSearch"]`)
		same(t, "settings", h.run()["permissions"].(map[string]any)["deny"], `["Bash(shutdown*)","WebSearch"]`)
	})
}

func TestComposeSavedCopy(t *testing.T) {
	t.Run("a projection interrupted after writing settings is not folded as an edit", func(t *testing.T) { // test_a_projection_interrupted_after_writing_settings_is_not_folded_as_an_edit
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.run("--acw", "none")
		// Killed after settings.json was replaced but before the saved copy was promoted.
		doc := h.get(h.p("settings.json"))
		doc["autoCompactWindow"] = 300000
		data, _ := json.MarshalIndent(doc, "", "  ")
		data = append(data, '\n')
		os.WriteFile(h.p("cache", "settings.base.pending.json"), data, 0o644)
		os.WriteFile(h.p("settings.json"), data, 0o644)
		h.fold()
		if _, ok := h.get(h.p("settings.profile.json"))["autoCompactWindow"]; ok {
			t.Fatal("a budget ccmorph derived became the profile's own choice")
		}
		if !bytes.Equal(h.bytes(h.p("cache", "settings.base.json")), data) {
			t.Fatal("the saved copy was not promoted")
		}
		if _, err := os.Stat(h.p("cache", "settings.base.pending.json")); err == nil {
			t.Fatal("the pending copy was left behind")
		}
	})
	t.Run("a projection interrupted before writing settings is discarded", func(t *testing.T) { // test_a_projection_interrupted_before_writing_settings_is_discarded
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.run()
		before := h.bytes(h.p("cache", "settings.base.json"))
		h.put(h.p("cache", "settings.base.pending.json"), `{"theme": "light"}`+"\n")
		h.fold()
		if !bytes.Equal(h.bytes(h.p("cache", "settings.base.json")), before) {
			t.Fatal("the saved copy changed")
		}
		if _, err := os.Stat(h.p("cache", "settings.base.pending.json")); err == nil {
			t.Fatal("the pending copy was left behind")
		}
		same(t, "layer", h.get(h.p("settings.profile.json")), `{}`)
	})
	t.Run("the saved copy is byte-identical to settings.json", func(t *testing.T) { // test_the_saved_copy_is_byte_identical_to_settings_json
		h := newHome(t) // the launcher skips the fold by comparing these two files' contents
		h.put(h.canonical, `{"theme":"dark"}`)
		h.run()
		base := h.p("cache", "settings.base.json")
		if !bytes.Equal(h.bytes(base), h.bytes(h.p("settings.json"))) {
			t.Fatal("the copy differs")
		}
		// A file Claude Code rewrote in its own formatting, with no change of content.
		h.put(h.p("settings.json"), string(mustCompact(h.bytes(h.p("settings.json")))))
		h.run()
		if !bytes.Equal(h.bytes(base), h.bytes(h.p("settings.json"))) {
			t.Fatal("the copy differs after a reformat")
		}
	})
	t.Run("a BOM in settings.json never poisons the saved copy", func(t *testing.T) { // test_a_bom_in_settings_json_never_poisons_the_saved_copy_issue_67
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		settings, base := h.p("settings.json"), h.p("cache", "settings.base.json")
		same(t, "first", h.run(), `{"theme":"dark"}`)
		os.WriteFile(settings, append(append([]byte{}, bom...), h.bytes(settings)...), 0o644)
		if rc, stderr := h.materialize("{}"); rc != 0 {
			t.Fatal(rc, stderr)
		}
		if bytes.HasPrefix(h.bytes(base), bom) {
			t.Fatal("the saved copy carries the BOM")
		}
		if rc, stderr := h.materialize("{}"); rc != 0 {
			t.Fatal(rc, stderr)
		}
		os.WriteFile(settings, bytes.TrimPrefix(h.bytes(settings), bom), 0o644)
		same(t, "after", h.run(), `{"theme":"dark"}`)
	})
	t.Run("a saved copy poisoned by an older build is repaired", func(t *testing.T) { // test_a_saved_copy_poisoned_by_an_older_build_is_repaired_issue_67
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		base := h.p("cache", "settings.base.json")
		h.run()
		os.WriteFile(base, append(append([]byte{}, bom...), h.bytes(base)...), 0o644)
		same(t, "poisoned", h.run(), `{"theme":"dark"}`)
		if bytes.HasPrefix(h.bytes(base), bom) {
			t.Fatal("the projection left the BOM in place")
		}
	})
}

func mustCompact(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestComposePolicy(t *testing.T) {
	t.Run("a linked shared policy reaches settings but never the layer", func(t *testing.T) { // test_a_linked_shared_policy_reaches_settings_but_never_the_layer
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		shared := h.linkPolicy(`{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/Documents"]}}}`)
		h.put(h.p("settings.profile.json"), `{"effortLevel":"high","verbose":true}`)
		deny := func() any {
			return h.run()["sandbox"].(map[string]any)["filesystem"].(map[string]any)["denyRead"]
		}
		same(t, "denyRead", deny(), `["~/Documents"]`)
		h.fold()
		if _, ok := h.get(h.p("settings.profile.json"))["sandbox"]; ok {
			t.Fatal("the policy was folded into the layer")
		}
		h.put(shared, `{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/Documents","/Volumes"]}}}`)
		same(t, "denyRead", deny(), `["~/Documents","/Volumes"]`)
	})
	for _, which := range []string{"canonical", "policy"} {
		t.Run("a BOM in the "+which+" is read like settings.json", func(t *testing.T) { // test_a_bom_in_the_composed_files_is_read_like_settings_json_issue_67
			h := newHome(t)
			h.put(h.canonical, `{"theme":"dark"}`)
			shared := h.linkPolicy(`{"verbose":true}`)
			target := map[string]string{"canonical": h.canonical, "policy": shared}[which]
			os.WriteFile(target, append(append([]byte{}, bom...), h.bytes(target)...), 0o644)
			same(t, "settings", h.run(), `{"theme":"dark","verbose":true}`)
		})
	}
	for _, c := range []struct {
		name      string
		content   []byte // nil: a dangling link
		diagnosis string
	}{
		{"dangling", nil, "missing file"},
		{"invalid json", []byte("{"), "invalid JSON"},
		{"non-object", []byte("[]"), "root is not a JSON object"},
		{"invalid encoding", []byte{0xff}, "invalid JSON"},
	} {
		t.Run("an unreadable policy fails instead of dropping the policy: "+c.name, func(t *testing.T) { // test_an_unreadable_policy_fails_instead_of_dropping_the_policy
			h := newHome(t)
			h.put(h.canonical, `{}`)
			shared := filepath.Join(h.dir, "policy.json")
			if c.content != nil {
				os.WriteFile(shared, c.content, 0o644)
			}
			os.Symlink(shared, h.p("settings.policy.json"))
			rc, stderr := h.main("materialize", h.profile, "--canonical", h.canonical)
			if rc != 3 || !strings.Contains(stderr, "settings.policy.json") || !strings.Contains(stderr, c.diagnosis) {
				t.Fatalf("rc %d: %s", rc, stderr)
			}
			if _, err := os.Stat(h.p("settings.json")); err == nil {
				t.Fatal("settings.json was written")
			}
		})
	}
	t.Run("a tombstone in the shared policy never reaches settings", func(t *testing.T) { // test_a_tombstone_in_the_shared_policy_never_reaches_settings
		h := newHome(t)
		h.put(h.canonical, `{"theme":"dark"}`)
		h.linkPolicy(`{"$ccmorphUnset":[["theme"]],"verbose":true}`)
		doc := h.run()
		if _, ok := doc["$ccmorphUnset"]; ok || doc["theme"] != "dark" || doc["verbose"] != true {
			t.Fatal(doc)
		}
	})
	t.Run("symlinked settings are refused", func(t *testing.T) { // test_symlinked_settings_are_refused
		h := newHome(t)
		h.put(h.canonical, `{}`)
		os.Symlink(h.canonical, h.p("settings.json"))
		if rc, _ := h.main("materialize", h.profile, "--canonical", h.canonical); rc != 1 {
			t.Fatalf("rc %d", rc)
		}
	})
}

const (
	sandboxPolicy  = `{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/Documents"]}}}`
	sandboxAndHook = `{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/Documents"]}},"hooks":{"PreToolUse":[` + guard + `]}}`
)

func tooDeepToDecode() string {
	return `{"sandbox":` + strings.Repeat("[", 2_000_000) + "0" + strings.Repeat("]", 2_000_000) + "}"
}

func (h home) breakTheProjection(broken string) {
	switch broken {
	case "lock":
		h.breakTheLock()
	case "deep-canonical":
		h.put(h.canonical, tooDeepToDecode())
	case "malformed-tombstones":
		h.put(h.p("settings.profile.json"), `{"$ccmorphUnset": null}`)
	default:
		h.put(h.canonical, "{")
	}
}

func TestComposeAFailedProjection(t *testing.T) {
	for _, c := range []struct{ name, broken, policy, named string }{
		{"lock-unopenable-issue-319", "lock", sandboxPolicy, "is a directory"},
		{"invalid-canonical-issue-319", "canonical", sandboxPolicy, "invalid JSON"},
		{"lock-unopenable-hooks-issue-319", "lock", sandboxAndHook, "is a directory"},
		{"deep-canonical-crash-issue-420", "deep-canonical", sandboxPolicy, "max depth"},
		{"malformed-tombstones-crash-issue-420", "malformed-tombstones", sandboxPolicy, "$ccmorphUnset"},
	} {
		t.Run("refuses old settings without the policy: "+c.name, func(t *testing.T) { // test_a_failed_projection_refuses_old_settings_without_the_policy
			h := newHome(t)
			h.put(h.canonical, `{}`)
			if rc, stderr := h.materialize("{}"); rc != 0 {
				t.Fatal(rc, stderr)
			}
			before := h.bytes(h.p("settings.json"))
			h.linkPolicy(c.policy)
			h.breakTheProjection(c.broken)
			rc, stderr := h.materialize("{}")
			if rc != 3 || !strings.Contains(stderr, "settings.policy.json") || !strings.Contains(stderr, c.named) { // how it failed stays named, crash or not
				t.Fatalf("rc %d: %s", rc, stderr)
			}
			if !bytes.Equal(h.bytes(h.p("settings.json")), before) {
				t.Fatal("settings.json changed")
			}
		})
	}
	for _, c := range []struct{ name, broken, policy, layer string }{
		{"no-policy-issue-319", "lock", "", ""},
		{"policy-carried-issue-319", "lock", sandboxPolicy, ""},
		{"hooks-carried-issue-319", "lock", sandboxAndHook, ""},
		{"layer-overrides-policy-issue-319", "lock", sandboxPolicy, `{"sandbox":{"enabled":false}}`},
		{"crash-carried-issue-420", "deep-canonical", sandboxPolicy, ""},
	} {
		t.Run("keeps old settings that carry the policy: "+c.name, func(t *testing.T) { // test_a_failed_projection_keeps_old_settings_that_carry_the_policy
			h := newHome(t)
			h.put(h.canonical, `{}`)
			if c.layer != "" {
				h.put(h.p("settings.profile.json"), c.layer)
			}
			if c.policy != "" {
				h.linkPolicy(c.policy)
			}
			if rc, stderr := h.materialize("{}"); rc != 0 {
				t.Fatal(rc, stderr)
			}
			before := h.bytes(h.p("settings.json"))
			h.breakTheProjection(c.broken)
			rc, stderr := h.materialize("{}")
			if rc != 1 || strings.Contains(stderr, "settings.policy.json") {
				t.Fatalf("rc %d: %s", rc, stderr)
			}
			if !bytes.Equal(h.bytes(h.p("settings.json")), before) {
				t.Fatal("settings.json changed")
			}
		})
	}
	t.Run("a layer tombstone added since generation refuses the fallback", func(t *testing.T) { // test_a_layer_tombstone_added_since_generation_refuses_the_fallback_issue_319
		h := newHome(t)
		h.put(h.canonical, `{}`)
		h.linkPolicy(sandboxPolicy)
		if rc, stderr := h.materialize("{}"); rc != 0 {
			t.Fatal(rc, stderr)
		}
		before := h.bytes(h.p("settings.json"))
		h.put(h.p("settings.profile.json"), `{"$ccmorphUnset":[["sandbox","enabled"]]}`)
		h.breakTheLock()
		rc, stderr := h.materialize("{}")
		if rc != 3 || !strings.Contains(stderr, "settings.policy.json") || !bytes.Equal(h.bytes(h.p("settings.json")), before) {
			t.Fatalf("rc %d: %s", rc, stderr)
		}
	})
	for _, stopper := range []string{"deep-policy", "malformed-tombstones"} {
		t.Run("a comparison that cannot finish is not carried: "+stopper, func(t *testing.T) { // test_a_comparison_that_cannot_finish_is_not_carried_issue_319
			h := newHome(t)
			h.put(h.canonical, `{}`)
			if rc, stderr := h.materialize("{}"); rc != 0 {
				t.Fatal(rc, stderr)
			}
			before := h.bytes(h.p("settings.json"))
			if stopper == "deep-policy" {
				deep := filepath.Join(h.dir, "deep-policy.json")
				os.WriteFile(deep, []byte(`{"sandbox":`+strings.Repeat(`{"a":`, 900)+`1`+strings.Repeat("}", 900)+`}`), 0o644)
				os.Symlink(deep, h.p("settings.policy.json"))
			} else {
				h.linkPolicy(sandboxPolicy)
				h.put(h.p("settings.profile.json"), `{"$ccmorphUnset": null}`)
			}
			h.breakTheLock()
			rc, stderr := h.materialize("{}")
			if rc != 3 || !strings.Contains(stderr, "settings.policy.json") || !bytes.Equal(h.bytes(h.p("settings.json")), before) {
				t.Fatalf("rc %d: %s", rc, stderr)
			}
		})
	}
}
