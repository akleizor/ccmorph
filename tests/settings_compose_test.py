"""settings-compose through the built helper: a profile's settings.json = canonical + its layer + saved defaults.

The composition rules are asserted in settingscompose/compose_port_test.go; these two keep the
binary entry point and the on-disk layout honest."""
import json
from pathlib import Path
import subprocess

import pytest

from support import HANG_TIMEOUT, helper_cmd

ADVISE = '[ -n "$CCMORPH_ADVISE" ] && printf "%s" "$CCMORPH_ADVISE" || true'


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


def test_first_run_turns_the_old_settings_into_the_layer(home):
    canonical, profile = home
    put(canonical, {"theme": "dark", "hooks": {"Stop": [{"hooks": []}]}})
    put(profile / "settings.json", {"availableModels": ["gw-a"], "env": {"NO_PROXY": "x"}})
    doc = run(canonical, profile)
    assert doc == {"theme": "dark", "hooks": {"Stop": [{"hooks": []}]},
                   "availableModels": ["gw-a"], "env": {"NO_PROXY": "x"}}
    assert get(profile / "settings.profile.json") == {"availableModels": ["gw-a"], "env": {"NO_PROXY": "x"}}


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
