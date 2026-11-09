"""The SessionEnd hook ccmorph's plugin ships (#285).

Claude Code's own exit line suggests a bare `claude --resume <id>`, which
reopens the default config folder instead of the profile the session ran on;
the launcher answers with `ccmorph run <profile> --resume <id>`, and for that
it must learn the id of the session that just ended. The hook is the recorder:
it runs in Claude Code's own environment at session end, and its contract here
is exactly what the launcher's reader depends on — an attended session on a
profile records its id in a pid-keyed TMPDIR file, everything else records
nothing, and no outcome fails the hook.
"""
import json
import os
import subprocess
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
HOOKS = ROOT / "share" / "claude-plugin" / "hooks" / "hooks.json"


def _session_end_command():
    hooks = json.loads(HOOKS.read_text(encoding="utf-8"))
    (group,) = hooks["hooks"]["SessionEnd"]
    (hook,) = group["hooks"]
    assert hook["type"] == "command"
    return hook["command"]


def _recorded(tmp, pid):
    f = tmp / f"ccmorph-session-end.{pid}"
    if not f.exists():
        return None
    body = f.read_text(encoding="utf-8")
    f.unlink()
    return body


@pytest.mark.parametrize(
    "attended,config_dir,expected",
    [
        ("1", "/profiles/work", "sid-285"),  # an attended profile session records its id
        ("0", "/profiles/work", None),  # a print or background run records nothing
        ("1", "", None),  # a launch with no profile records nothing
    ],
)
def test_the_hook_records_attended_profile_sessions(attended, config_dir, expected, tmp_path):
    env = {
        "TMPDIR": str(tmp_path),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "CLAUDE_CODE_SESSION_ATTENDED": attended,
        "CLAUDE_CODE_SESSION_ID": "sid-285",
        "CLAUDE_PID": "4194304",
    }
    if config_dir:
        env["CLAUDE_CONFIG_DIR"] = config_dir
    ran = subprocess.run(["/bin/sh", "-c", _session_end_command()], env=env, timeout=HANG_TIMEOUT)
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    assert _recorded(tmp_path, 4194304) == expected
