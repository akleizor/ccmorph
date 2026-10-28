"""settings-compose: a profile's settings.json = canonical + its layer + saved defaults."""
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest

from support import HANG_TIMEOUT, HELPERS_IMPL, helper_cmd

# Tests that patch the Python module's internals have Go unit-test equivalents in
# settingscompose/compose_test.go; under the Go switch they are skipped here.
python_only = pytest.mark.skipif(HELPERS_IMPL == "go", reason="patches Python internals; see settingscompose/compose_test.go")


HELPER = Path(__file__).resolve().parent.parent / "libexec/settings-compose"
ADVISE = '[ -n "$CCMORPH_ADVISE" ] && printf "%s" "$CCMORPH_ADVISE" || true'
# Nested too deep for any settings file to be held: it fails while the policy is
# decoded, or, on an interpreter that decodes it anyway, while it is copied in.
TOO_DEEP = '{"sandbox":' + "[" * 2000 + "0" + "]" * 2000 + "}"
# Nested past the decoder's own limit, which the interpreter sets from its
# stack: 3.9 refuses a few thousand nestings, 3.14 a hundred thousand, so this
# crashes the decode itself rather than the copy of the policy above.
TOO_DEEP_TO_DECODE = '{"sandbox":' + "[" * 2_000_000 + "0" + "]" * 2_000_000 + "}"


@pytest.fixture
def home(tmp_path):
    canonical = tmp_path / "claude" / "settings.json"
    canonical.parent.mkdir()
    profile = tmp_path / "profile"
    profile.mkdir()
    return canonical, profile


def put(path: Path, doc: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(doc))


def get(path: Path) -> dict:
    return json.loads(path.read_text())


def run(canonical, profile, *extra, saved=None, raw=False):
    """Project, and return settings.json without the advisory hook every projection adds."""
    cmd = [*helper_cmd("settings-compose"), "materialize", str(profile), "--canonical", str(canonical),
           "--saved", json.dumps(saved or {}), *extra]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, result.stderr
    doc = get(profile / "settings.json")
    if raw:
        return doc
    start = doc.get("hooks", {}).get("SessionStart", [])
    kept = [e for e in start if not any(h.get("command") == ADVISE for h in e.get("hooks", []))]
    if kept:
        doc["hooks"]["SessionStart"] = kept
    elif "hooks" in doc:
        doc["hooks"].pop("SessionStart", None)
        if not doc["hooks"]:
            del doc["hooks"]
    return doc


def fold(canonical, profile):
    subprocess.run([*helper_cmd("settings-compose"), "fold", str(profile), "--canonical", str(canonical)],
                   check=True, timeout=HANG_TIMEOUT)


def test_first_capture_drops_inherited_hooks_but_keeps_additions(home):
    canonical, profile = home
    inherited = {"matcher": "Bash", "hooks": [{"type": "command", "command": "guard"}]}
    added = {"matcher": "Bash", "hooks": [{"type": "command", "command": "profile-extra"}]}
    put(canonical, {"hooks": {"PreToolUse": [inherited]}})
    put(profile / "settings.json", {"hooks": {"PreToolUse": [inherited, added]}})
    doc = run(canonical, profile)
    assert doc["hooks"]["PreToolUse"] == [inherited, added]
    assert get(profile / "settings.profile.json")["hooks"]["PreToolUse"] == [added]


def test_profile_hook_tombstones_cannot_remove_canonical_guard(home):
    canonical, profile = home
    inherited = {"matcher": "Bash", "hooks": [{"type": "command", "command": "guard"}]}
    put(canonical, {"hooks": {"PreToolUse": [inherited]}})
    put(profile / "settings.profile.json", {"__unset__": [["hooks"], ["hooks", "PreToolUse"]]})
    assert run(canonical, profile)["hooks"]["PreToolUse"] == [inherited]


def test_existing_layer_cannot_duplicate_inherited_hook(home):
    canonical, profile = home
    inherited = {"matcher": "Bash", "hooks": [{"type": "command", "command": "guard"}]}
    put(canonical, {"hooks": {"PreToolUse": [inherited]}})
    put(profile / "settings.profile.json", {"hooks": {"PreToolUse": [inherited]}})
    assert run(canonical, profile)["hooks"]["PreToolUse"] == [inherited]


def test_first_run_turns_the_old_settings_into_the_layer(home):
    canonical, profile = home
    put(canonical, {"theme": "dark", "hooks": {"Stop": [{"hooks": []}]}})
    put(profile / "settings.json", {"availableModels": ["gw-a"], "env": {"NO_PROXY": "x"}})
    doc = run(canonical, profile)
    assert doc == {"theme": "dark", "hooks": {"Stop": [{"hooks": []}]},
                   "availableModels": ["gw-a"], "env": {"NO_PROXY": "x"}}
    assert get(profile / "settings.profile.json") == {"availableModels": ["gw-a"], "env": {"NO_PROXY": "x"}}


def test_a_canonical_change_reaches_the_profile(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    run(canonical, profile)
    put(canonical, {"theme": "light"})
    assert run(canonical, profile)["theme"] == "light"


def test_claude_edits_stay_with_the_profile_across_canonical_changes(home):
    canonical, profile = home
    put(canonical, {"theme": "dark", "permissions": {"allow": ["Read"]}})
    doc = run(canonical, profile)
    doc["permissions"]["allow"].append("Bash(npm test)")
    doc["outputStyle"] = "Concise"
    put(profile / "settings.json", doc)
    put(canonical, {"theme": "light", "permissions": {"allow": ["Read"]}})
    doc = run(canonical, profile)
    assert doc["theme"] == "light"
    assert doc["permissions"]["allow"] == ["Read", "Bash(npm test)"]
    assert doc["outputStyle"] == "Concise"
    layer = get(profile / "settings.profile.json")
    assert layer["permissions"]["allow"] == ["Read", "Bash(npm test)"]


def test_removing_a_canonical_key_in_the_profile_sticks(home):
    canonical, profile = home
    put(canonical, {"theme": "dark", "statusLine": {"type": "command", "command": "x"}})
    doc = run(canonical, profile)
    del doc["statusLine"]
    put(profile / "settings.json", doc)
    assert "statusLine" not in run(canonical, profile)
    assert "statusLine" not in run(canonical, profile)
    # Setting it again in the profile lifts the removal.
    doc = get(profile / "settings.json")
    doc["statusLine"] = {"type": "command", "command": "y"}
    put(profile / "settings.json", doc)
    assert run(canonical, profile)["statusLine"]["command"] == "y"


def test_in_session_model_and_effort_choices_stay_session_local(home):
    canonical, profile = home
    put(canonical, {})
    saved = {"model": "gw-default", "effortLevel": "medium", "env": {"ANTHROPIC_DEFAULT_OPUS_MODEL": "gw-big"}}
    doc = run(canonical, profile, "--mapping", "ANTHROPIC_DEFAULT_OPUS_MODEL", saved=saved)
    doc["model"] = "picked-in-session"
    doc["effortLevel"] = "max"
    put(profile / "settings.json", doc)
    doc = run(canonical, profile, "--mapping", "ANTHROPIC_DEFAULT_OPUS_MODEL", saved=saved)
    assert doc["model"] == "gw-default" and doc["effortLevel"] == "medium"
    assert "model" not in get(profile / "settings.profile.json")


def test_launch_environment_never_lands_in_the_env_block(home):
    canonical, profile = home
    put(canonical, {"env": {"ENABLE_TOOL_SEARCH": "true", "ANTHROPIC_BASE_URL": "http://stale"}})
    doc = run(canonical, profile, "--strip", "ANTHROPIC_BASE_URL", "--strip", "ANTHROPIC_API_KEY")
    assert doc["env"] == {"ENABLE_TOOL_SEARCH": "true"}


def test_the_advisory_hook_is_added_once_and_never_folded(home):
    canonical, profile = home
    put(canonical, {"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "mine"}]}]}})
    for _ in range(3):
        doc = run(canonical, profile, raw=True)
    commands = [h["command"] for e in doc["hooks"]["SessionStart"] for h in e["hooks"]]
    assert commands == ["mine", ADVISE]
    assert "hooks" not in get(profile / "settings.profile.json")


def test_a_derived_compact_window_fills_a_gap_but_never_overrules_the_user(home):
    canonical, profile = home
    put(canonical, {})
    assert run(canonical, profile, "--acw", "400000")["autoCompactWindow"] == 400000
    assert run(canonical, profile)["autoCompactWindow"] == 400000, "keep reuses the last derived value"
    doc = get(profile / "settings.json")
    doc["autoCompactWindow"] = 250000
    put(profile / "settings.json", doc)
    assert run(canonical, profile, "--acw", "400000")["autoCompactWindow"] == 250000


def test_an_unchanged_projection_is_not_rewritten_and_a_change_is_backed_up(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    run(canonical, profile)
    stamp = (profile / "settings.json").stat().st_mtime_ns
    run(canonical, profile)
    assert (profile / "settings.json").stat().st_mtime_ns == stamp
    put(canonical, {"theme": "light"})
    run(canonical, profile)
    backups = list((profile / "backups").glob("settings.*.json"))
    assert backups and any(get(b).get("theme") == "dark" for b in backups)


def test_symlinked_settings_are_refused(home, tmp_path):
    canonical, profile = home
    put(canonical, {})
    (profile / "settings.json").symlink_to(canonical)
    cmd = [*helper_cmd("settings-compose"), "materialize", str(profile), "--canonical", str(canonical)]
    assert subprocess.run(cmd, capture_output=True, timeout=HANG_TIMEOUT).returncode == 1


def test_a_missing_settings_file_is_regenerated_not_read_as_removals(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    put(profile / "settings.json", {"outputStyle": "Concise"})
    run(canonical, profile)
    (profile / "settings.json").unlink()
    assert run(canonical, profile) == {"theme": "dark", "outputStyle": "Concise"}


def test_a_removed_layer_keeps_only_edits_made_since_the_last_projection(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    put(profile / "settings.json", {"outputStyle": "Concise"})
    doc = run(canonical, profile)
    (profile / "settings.profile.json").unlink()
    doc["verbose"] = True
    put(profile / "settings.json", doc)
    assert run(canonical, profile) == {"theme": "dark", "verbose": True}


def test_fold_makes_pending_edits_visible_before_the_next_projection(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    doc = run(canonical, profile)
    doc["env"] = {"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "70"}
    put(profile / "settings.json", doc)
    fold(canonical, profile)
    assert get(profile / "settings.profile.json")["env"] == {"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "70"}
    # Folding again, or projecting afterwards, must not fold the same edit twice.
    fold(canonical, profile)
    assert run(canonical, profile)["env"] == {"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "70"}


def test_the_first_releases_file_based_hook_is_never_folded(home):
    canonical, profile = home
    put(canonical, {})
    legacy = 'f="$CCMORPH_ADVISE_FILE"; [ -n "$f" ] && [ -f "$f" ] && cat "$f" || true'
    doc = {"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": legacy}]}]}}
    put(profile / "settings.json", doc)
    put(profile / "settings.profile.json", {})
    put(profile / "cache" / "settings.base.json", doc)
    doc = {"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": legacy}]}],
                     "Stop": [{"hooks": []}]}}
    put(profile / "settings.json", doc)
    run(canonical, profile)
    # Hooks never fold from settings.json into the layer at all -- not the legacy
    # advisory, not a genuine edit like the Stop entry added here.
    assert get(profile / "settings.profile.json") == {}


def test_hooks_never_fold_from_settings_json_into_the_layer(home):
    canonical, profile = home
    put(canonical, {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "canon"}]}]}})
    run(canonical, profile)
    doc = get(profile / "settings.json")
    # Claude Code (or anything else) adds a hook entry directly to settings.json.
    doc["hooks"]["Stop"][0]["hooks"].append({"type": "command", "command": "added-by-editor"})
    put(profile / "settings.json", doc)
    fold(canonical, profile)
    assert "hooks" not in get(profile / "settings.profile.json")


def test_a_sync_write_to_canonical_and_relaunch_never_duplicates_hooks(home):
    """Regression: sync used to write a profile's settings.json directly (with
    canonical hooks already merged in), and the next materialize/fold folded that
    copy of canonical's hooks into the layer. The layer then carried its own copy
    of every canonical hook, and merge() appended canonical's hooks again on the
    next build -- doubling, then redoubling, on every subsequent cycle."""
    canonical, profile = home
    put(canonical, {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "canon-a"}]}]}})
    run(canonical, profile)

    def stop_commands(doc):
        return [h["command"] for e in doc.get("hooks", {}).get("Stop", []) for h in e["hooks"]]

    assert stop_commands(run(canonical, profile)) == ["canon-a"]

    # Simulate an external writer (sync) overwriting the profile's settings.json
    # with canonical's current hooks baked in, as if it had re-rendered the file
    # from the profile's own (canonical-inclusive) view rather than through
    # materialize.
    current = get(profile / "settings.json")
    put(profile / "settings.json", current)
    fold(canonical, profile)

    # A second canonical change and relaunch must still show exactly one copy of
    # each hook -- not canonical's hook doubled by a stale fold.
    put(canonical, {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "canon-a"}]}]}})
    for _ in range(3):
        assert stop_commands(run(canonical, profile)) == ["canon-a"]
    assert "hooks" not in get(profile / "settings.profile.json")


def test_a_projection_interrupted_after_writing_settings_is_not_folded_as_an_edit(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    run(canonical, profile, "--acw", "none")
    # Killed after settings.json was replaced but before the saved copy was promoted.
    doc = get(profile / "settings.json")
    doc["autoCompactWindow"] = 300000
    data = (json.dumps(doc, indent=2) + "\n").encode()  # settings-compose's encode()
    (profile / "cache" / "settings.base.pending.json").write_bytes(data)
    (profile / "settings.json").write_bytes(data)
    fold(canonical, profile)
    assert "autoCompactWindow" not in get(profile / "settings.profile.json"), \
        "a budget ccmorph derived must never become the profile's own choice"
    assert (profile / "cache" / "settings.base.json").read_bytes() == data
    assert not (profile / "cache" / "settings.base.pending.json").exists()


def test_a_projection_interrupted_before_writing_settings_is_discarded(home):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    run(canonical, profile)
    before = (profile / "cache" / "settings.base.json").read_bytes()
    (profile / "cache" / "settings.base.pending.json").write_text('{"theme": "light"}\n')
    fold(canonical, profile)
    assert (profile / "cache" / "settings.base.json").read_bytes() == before
    assert not (profile / "cache" / "settings.base.pending.json").exists()
    assert get(profile / "settings.profile.json") == {}


def test_the_saved_copy_is_byte_identical_to_settings_json(home):
    # The launcher skips the fold by comparing these two files' contents.
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    run(canonical, profile)
    base = profile / "cache" / "settings.base.json"
    assert base.read_bytes() == (profile / "settings.json").read_bytes()
    # A file Claude Code rewrote in its own formatting, with no change of content.
    (profile / "settings.json").write_text(json.dumps(get(profile / "settings.json")))
    run(canonical, profile)
    assert base.read_bytes() == (profile / "settings.json").read_bytes()


def test_a_linked_shared_policy_reaches_settings_but_never_the_layer(home, tmp_path):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    shared = tmp_path / "agents" / "sandbox.json"
    put(shared, {"sandbox": {"enabled": True, "filesystem": {"denyRead": ["~/Documents"]}}})
    (profile / "settings.policy.json").symlink_to(shared)
    put(profile / "settings.profile.json", {"effortLevel": "high", "verbose": True})
    assert run(canonical, profile)["sandbox"]["filesystem"]["denyRead"] == ["~/Documents"]
    fold(canonical, profile)
    assert "sandbox" not in get(profile / "settings.profile.json")
    put(shared, {"sandbox": {"enabled": True, "filesystem": {"denyRead": ["~/Documents", "/Volumes"]}}})
    assert run(canonical, profile)["sandbox"]["filesystem"]["denyRead"] == ["~/Documents", "/Volumes"]


@pytest.mark.parametrize("content,diagnosis", [
    pytest.param(None, "missing file", id="dangling-issue-308"),
    pytest.param("{", "Expecting", id="invalid-json-issue-308"),
    pytest.param("[]", "root is not a JSON object", id="non-object-issue-308"),
    pytest.param(b"\xff", "decode", id="invalid-encoding-issue-308"),
    pytest.param(TOO_DEEP, "recursion", id="too-deep-issue-318"),
])
def test_an_unreadable_policy_fails_instead_of_dropping_the_policy(home, tmp_path, content, diagnosis):
    canonical, profile = home
    put(canonical, {})
    shared = tmp_path / "policy.json"
    if content is not None:
        shared.write_bytes(content if isinstance(content, bytes) else content.encode())
    (profile / "settings.policy.json").symlink_to(shared)
    cmd = [*helper_cmd("settings-compose"), "materialize", str(profile), "--canonical", str(canonical)]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 3, result.stderr
    assert "settings.policy.json" in result.stderr and diagnosis in result.stderr
    assert not (profile / "settings.json").exists()


@python_only
@pytest.mark.parametrize("error,replacement", [
    pytest.param(FileNotFoundError, False, id="disappearing-target-issue-308"),
    pytest.param(PermissionError, False, id="unreadable-target-issue-308"),
    pytest.param(FileNotFoundError, True, id="replaced-policy-issue-308"),
    pytest.param(RecursionError, False, id="too-deep-target-issue-318"),
])
def test_a_policy_read_failure_preserves_the_generated_sandbox(home, tmp_path, monkeypatch, capsys, error, replacement):
    canonical, profile = home
    put(canonical, {})
    shared = tmp_path / "policy.json"
    sandbox = {"sandbox": {"enabled": True, "filesystem": {"denyRead": ["~/Documents"]}}}
    put(shared, sandbox)
    policy = profile / "settings.policy.json"
    policy.symlink_to(shared)
    run(canonical, profile)
    before = (profile / "settings.json").read_bytes()
    loader = importlib.machinery.SourceFileLoader("settings_compose", str(HELPER))
    module = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    loader.exec_module(module)
    read_text = Path.read_text

    def failed_read(path, *args, **kwargs):
        if path == policy:
            if replacement:
                shared.replace(policy)
            raise error("policy target unavailable")
        return read_text(path, *args, **kwargs)

    monkeypatch.setattr(Path, "read_text", failed_read)
    assert module.main(["materialize", str(profile), "--canonical", str(canonical)]) == 3
    assert "settings.policy.json" in capsys.readouterr().err
    assert (profile / "settings.json").read_bytes() == before
    assert get(profile / "settings.json")["sandbox"] == sandbox["sandbox"]


def test_a_tombstone_in_the_shared_policy_never_reaches_settings(home, tmp_path):
    canonical, profile = home
    put(canonical, {"theme": "dark"})
    shared = tmp_path / "agents" / "policy.json"
    put(shared, {"$ccmorphUnset": [["theme"]], "verbose": True})
    (profile / "settings.policy.json").symlink_to(shared)
    doc = run(canonical, profile)
    assert "$ccmorphUnset" not in doc and doc["theme"] == "dark" and doc["verbose"] is True


SANDBOX = {"sandbox": {"enabled": True, "filesystem": {"denyRead": ["~/Documents"]}}}
GUARD = {"matcher": "Bash", "hooks": [{"type": "command", "command": "guard"}]}
SANDBOX_AND_HOOK = {**SANDBOX, "hooks": {"PreToolUse": [GUARD]}}


class GoCompose:
    """The Go port behind the same `main(argv)` the in-process tests call: a
    subprocess whose stderr is replayed so capsys sees it."""

    @staticmethod
    def main(argv):
        result = subprocess.run([*helper_cmd("settings-compose"), *argv], capture_output=True, text=True,
                                timeout=HANG_TIMEOUT)
        sys.stderr.write(result.stderr)
        return result.returncode


@pytest.fixture
def compose():
    """settings-compose loaded in-process: a failure path costs no interpreter."""
    if HELPERS_IMPL == "go":
        return GoCompose
    loader = importlib.machinery.SourceFileLoader("settings_compose", str(HELPER))
    module = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    loader.exec_module(module)
    return module


def materialize(compose, canonical, profile) -> int:
    return compose.main(["materialize", str(profile), "--canonical", str(canonical), "--saved", "{}"])


def link_policy(profile: Path, tmp_path: Path, policy: dict) -> None:
    """Link a shared policy into the profile, the way an operator adds one."""
    shared = tmp_path / "shared-policy.json"
    put(shared, policy)
    (profile / "settings.policy.json").symlink_to(shared)


def break_the_lock(profile: Path) -> None:
    """Make the projection unopenable without touching the policy or the settings."""
    lock = profile / "cache" / ".settings.lock"
    lock.unlink(missing_ok=True)
    lock.mkdir(parents=True)


def break_the_projection(profile: Path, canonical: Path, broken: str) -> None:
    """Make the next projection fail the way each parametrized case names --
    an unopenable lock, an unparseable canonical, or a crash main() does not
    expect: a canonical nested past the decoder's limit, a layer whose
    tombstones cannot be parsed (#420)."""
    if broken == "lock":
        break_the_lock(profile)
    elif broken == "deep-canonical":
        canonical.write_text(TOO_DEEP_TO_DECODE)
    elif broken == "malformed-tombstones":
        put(profile / "settings.profile.json", {"$ccmorphUnset": None})
    else:
        canonical.write_text("{")


@pytest.mark.parametrize("broken,policy,named", [
    pytest.param("lock", SANDBOX, "Is a directory", id="lock-unopenable-issue-319"),
    pytest.param("canonical", SANDBOX, "Expecting", id="invalid-canonical-issue-319"),
    pytest.param("lock", SANDBOX_AND_HOOK, "Is a directory", id="lock-unopenable-hooks-issue-319"),
    pytest.param("deep-canonical", SANDBOX, "RecursionError", id="deep-canonical-crash-issue-420"),
    pytest.param("malformed-tombstones", SANDBOX, "TypeError", id="malformed-tombstones-crash-issue-420"),
])
def test_a_failed_projection_refuses_old_settings_without_the_policy(home, tmp_path, compose, capsys, broken, policy, named):
    canonical, profile = home
    put(canonical, {})
    assert materialize(compose, canonical, profile) == 0
    before = (profile / "settings.json").read_bytes()
    link_policy(profile, tmp_path, policy)
    break_the_projection(profile, canonical, broken)
    assert materialize(compose, canonical, profile) == 3
    err = capsys.readouterr().err
    assert "settings.policy.json" in err
    # How the projection failed stays named in what it printed, crash or not.
    assert named in err
    assert (profile / "settings.json").read_bytes() == before


@pytest.mark.parametrize("broken,policy,layer", [
    pytest.param("lock", None, None, id="no-policy-issue-319"),
    pytest.param("lock", SANDBOX, None, id="policy-carried-issue-319"),
    pytest.param("lock", SANDBOX_AND_HOOK, None, id="hooks-carried-issue-319"),
    pytest.param("lock", SANDBOX, {"sandbox": {"enabled": False}}, id="layer-overrides-policy-issue-319"),
    pytest.param("deep-canonical", SANDBOX, None, id="crash-carried-issue-420"),
])
def test_a_failed_projection_keeps_old_settings_that_carry_the_policy(home, tmp_path, compose, capsys, broken, policy, layer):
    canonical, profile = home
    put(canonical, {})
    if layer is not None:
        put(profile / "settings.profile.json", layer)
    if policy is not None:
        link_policy(profile, tmp_path, policy)
    assert materialize(compose, canonical, profile) == 0
    before = (profile / "settings.json").read_bytes()
    # A crash reaches the same decision as any other failure, so a policy the old
    # settings already carry still keeps the fallback (#420).
    break_the_projection(profile, canonical, broken)
    assert materialize(compose, canonical, profile) == 1
    assert "settings.policy.json" not in capsys.readouterr().err
    assert (profile / "settings.json").read_bytes() == before


def test_a_layer_tombstone_added_since_generation_refuses_the_fallback_issue_319(home, tmp_path, compose, capsys):
    """The layer now deletes a key the policy sets, so the settings generated earlier
    hold a key this profile no longer asks for -- carrying them would enforce last
    week's policy, and the tombstone must not hide the key from the comparison."""
    canonical, profile = home
    put(canonical, {})
    link_policy(profile, tmp_path, SANDBOX)
    assert materialize(compose, canonical, profile) == 0
    before = (profile / "settings.json").read_bytes()
    put(profile / "settings.profile.json", {"$ccmorphUnset": [["sandbox", "enabled"]]})
    break_the_lock(profile)
    assert materialize(compose, canonical, profile) == 3
    assert "settings.policy.json" in capsys.readouterr().err
    assert (profile / "settings.json").read_bytes() == before


# Nested deep enough that comparing it with the generated settings cannot finish.
# Written as text: nesting it as a dict would recurse in the test itself. On an
# interpreter that refuses to decode it at all, the policy read refuses first with
# the same outcome.
TOO_DEEP_TO_COMPARE = '{"sandbox":' + '{"a":' * 900 + '1' + '}' * 900 + '}'


@pytest.mark.parametrize("stopper", [
    pytest.param("deep-policy", id="too-deep-to-compare-issue-319"),
    pytest.param("malformed-tombstones", id="malformed-tombstones-issue-319"),
])
def test_a_comparison_that_cannot_finish_is_not_carried_issue_319(home, tmp_path, compose, capsys, stopper):
    canonical, profile = home
    put(canonical, {})
    assert materialize(compose, canonical, profile) == 0
    before = (profile / "settings.json").read_bytes()
    if stopper == "deep-policy":
        deep = tmp_path / "deep-policy.json"
        deep.write_text(TOO_DEEP_TO_COMPARE)
        (profile / "settings.policy.json").symlink_to(deep)
    else:
        link_policy(profile, tmp_path, SANDBOX)
        put(profile / "settings.profile.json", {"$ccmorphUnset": None})
    break_the_lock(profile)
    assert materialize(compose, canonical, profile) == 3
    assert "settings.policy.json" in capsys.readouterr().err
    assert (profile / "settings.json").read_bytes() == before


def test_permission_rules_from_canonical_policy_and_layer_all_apply(home, tmp_path):
    canonical, profile = home
    put(canonical, {"permissions": {"deny": ["Bash(shutdown*)"], "defaultMode": "default"}})
    shared = tmp_path / "agents" / "policy.json"
    put(shared, {"permissions": {"deny": ["Read(~/.hermes/**)"]}})
    (profile / "settings.policy.json").symlink_to(shared)
    put(profile / "settings.profile.json", {"permissions": {"deny": ["WebSearch", "Bash(shutdown*)"]}})
    assert run(canonical, profile)["permissions"]["deny"] == ["Bash(shutdown*)", "Read(~/.hermes/**)", "WebSearch"]


def test_a_profile_cannot_drop_a_canonical_deny_rule(home):
    canonical, profile = home
    put(canonical, {"permissions": {"deny": ["Bash(shutdown*)"]}})
    put(profile / "settings.profile.json", {})
    doc = run(canonical, profile)
    doc["permissions"]["deny"] = ["WebSearch"]
    (profile / "settings.json").write_text(json.dumps(doc))
    fold(canonical, profile)
    assert get(profile / "settings.profile.json")["permissions"]["deny"] == ["WebSearch"]
    assert run(canonical, profile)["permissions"]["deny"] == ["Bash(shutdown*)", "WebSearch"]


@python_only
def test_a_policy_sharing_canonicals_deny_list_still_counts_as_carried(home, tmp_path):
    import importlib.machinery, importlib.util
    canonical, profile = home
    put(canonical, {"permissions": {"deny": ["Bash(shutdown*)"]}})
    shared = tmp_path / "agents" / "policy.json"
    put(shared, {"permissions": {"deny": ["Read(~/.ssh/**)"]}})
    (profile / "settings.policy.json").symlink_to(shared)
    run(canonical, profile)
    spec = importlib.util.spec_from_loader("sc", importlib.machinery.SourceFileLoader("sc", str(HELPER)))
    sc = importlib.util.module_from_spec(spec); spec.loader.exec_module(sc)
    assert sc.carries_policy(profile, get(shared))


@python_only
def test_a_crash_while_deciding_the_fallback_refuses_issue_420(home, tmp_path, compose, monkeypatch, capsys):
    """The refusal decision reads the policy again to compare it with what was
    generated; a failure that read does not wrap must not escape the decision
    -- a traceback there is exit 1, the unguarded fallback one level down. Not
    carried on a guess: settings that would carry the policy still refuse."""
    canonical, profile = home
    put(canonical, {})
    link_policy(profile, tmp_path, SANDBOX)
    assert materialize(compose, canonical, profile) == 0
    before = (profile / "settings.json").read_bytes()
    break_the_lock(profile)

    def unreadable_policy(profile_dir):
        raise MemoryError("cannot hold the policy")

    monkeypatch.setattr(compose, "read_policy", unreadable_policy)
    assert materialize(compose, canonical, profile) == 3
    assert "settings.policy.json" in capsys.readouterr().err
    assert (profile / "settings.json").read_bytes() == before
