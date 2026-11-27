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
import shlex
import shutil
import subprocess
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
HOOKS = ROOT / "share" / "claude-plugin" / "hooks" / "hooks.json"
PLUGIN = HOOKS.parent.parent


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
        "CLAUDE_PLUGIN_ROOT": str(PLUGIN),
        "CLAUDE_CODE_SESSION_ATTENDED": attended,
        "CLAUDE_CODE_SESSION_ID": "sid-285",
        "CLAUDE_PID": "4194304",
    }
    if config_dir:
        env["CLAUDE_CONFIG_DIR"] = config_dir
    ran = subprocess.run(["/bin/sh", "-c", _session_end_command()], env=env, timeout=HANG_TIMEOUT)
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    assert _recorded(tmp_path, 4194304) == expected


@pytest.mark.parametrize("suffix", ["", ".json"])
def test_the_hook_never_writes_through_a_planted_link_issue_410(suffix, tmp_path):
    # The record names are predictable in a shared TMPDIR, so another account
    # can plant a link at one before the hook runs. The hook must not follow it:
    # the file the link names stays as it was (#410).
    victim = tmp_path / "victim"
    victim.write_text("keep me", encoding="utf-8")
    (tmp_path / f"ccmorph-session-end.4194304{suffix}").symlink_to(victim)
    env = {
        "TMPDIR": str(tmp_path),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "CLAUDE_PLUGIN_ROOT": str(PLUGIN),
        "CLAUDE_CODE_SESSION_ATTENDED": "1",
        "CLAUDE_CODE_SESSION_ID": "sid-410",
        "CLAUDE_CONFIG_DIR": "/profiles/work",
        "CLAUDE_PID": "4194304",
    }
    ran = subprocess.run(
        ["/bin/sh", "-c", _session_end_command()],
        env=env,
        input='{"session_id":"sid-410","transcript_path":"/p/sid-410.jsonl"}',
        text=True,
        timeout=HANG_TIMEOUT,
    )
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    assert victim.read_text(encoding="utf-8") == "keep me", "the hook wrote through a planted link"


@pytest.mark.parametrize("suffix", ["", ".json"])
@pytest.mark.parametrize("kind", ["link to a directory", "directory"])
def test_the_hook_never_creates_a_file_through_a_planted_directory_issue_410(kind, suffix, tmp_path):
    # `ln SRC DEST` puts the link inside DEST when DEST is a directory, or a link
    # to one, so a directory planted at a record name would take a file of the
    # attacker's choosing. The hook writes with `link`, the bare link(2) call,
    # which creates exactly the record name or nothing (#410).
    victim = tmp_path / "victim"
    victim.mkdir()
    planted = tmp_path / f"ccmorph-session-end.4194304{suffix}"
    if kind == "directory":
        planted.mkdir()
        victim = planted
    else:
        planted.symlink_to(victim, target_is_directory=True)
    env = {
        "TMPDIR": str(tmp_path),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "CLAUDE_PLUGIN_ROOT": str(PLUGIN),
        "CLAUDE_CODE_SESSION_ATTENDED": "1",
        "CLAUDE_CODE_SESSION_ID": "sid-410",
        "CLAUDE_CONFIG_DIR": "/profiles/work",
        "CLAUDE_PID": "4194304",
    }
    ran = subprocess.run(
        ["/bin/sh", "-c", _session_end_command()],
        env=env,
        input='{"session_id":"sid-410","transcript_path":"/p/sid-410.jsonl"}',
        text=True,
        timeout=HANG_TIMEOUT,
    )
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    assert [e.name for e in victim.iterdir()] == [], "the hook created a file through a planted directory"
    leftovers = sorted(e.name for e in tmp_path.iterdir() if e.name.startswith("ccmorph-session-end.") and e != planted)
    other = ".json" if suffix == "" else ""
    assert leftovers == [f"ccmorph-session-end.4194304{other}"], "the hook left a temporary file behind"


def test_the_hook_does_not_wait_on_a_planted_fifo_issue_410(tmp_path):
    # A FIFO at the record's name would leave the hook's own write waiting for
    # a reader that never comes, so the session that is ending never ends (#410).
    os.mkfifo(tmp_path / "ccmorph-session-end.4194304")
    env = {
        "TMPDIR": str(tmp_path),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "CLAUDE_PLUGIN_ROOT": str(PLUGIN),
        "CLAUDE_CODE_SESSION_ATTENDED": "1",
        "CLAUDE_CODE_SESSION_ID": "sid-410",
        "CLAUDE_CONFIG_DIR": "/profiles/work",
        "CLAUDE_PID": "4194304",
    }
    ran = subprocess.run(
        ["/bin/sh", "-c", _session_end_command()],
        env=env,
        input='{"session_id":"sid-410","transcript_path":"/p/sid-410.jsonl"}',
        text=True,
        timeout=HANG_TIMEOUT,
    )
    assert ran.returncode == 0, "no outcome may fail the hook at session end"


@pytest.mark.parametrize("attended", ["1", "0"])
def test_the_hook_keeps_claude_codes_input_beside_the_record(attended, tmp_path):
    # Claude Code hands its SessionEnd hook a JSON input on stdin; its
    # transcript_path names where the session's conversation is, or would be had a
    # message been sent. The launcher reads it to tell a session that spoke from one
    # closed before its first message (#359), so an attended session keeps it
    # beside the id record and everything else keeps nothing.
    given = (
        '{"session_id":"sid-359","transcript_path":"/profiles/work/projects/p/sid-359.jsonl",'
        '"hook_event_name":"SessionEnd","reason":"prompt_input_exit"}'
    )
    env = {
        "TMPDIR": str(tmp_path),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "CLAUDE_PLUGIN_ROOT": str(PLUGIN),
        "CLAUDE_CODE_SESSION_ATTENDED": attended,
        "CLAUDE_CODE_SESSION_ID": "sid-359",
        "CLAUDE_CONFIG_DIR": "/profiles/work",
        "CLAUDE_PID": "4194304",
    }
    ran = subprocess.run(
        ["/bin/sh", "-c", _session_end_command()], env=env, input=given, text=True, timeout=HANG_TIMEOUT
    )
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    kept = tmp_path / "ccmorph-session-end.4194304.json"
    if attended == "1":
        assert kept.read_text(encoding="utf-8") == given
        assert _recorded(tmp_path, 4194304) == "sid-359"
    else:
        assert not kept.exists()
        assert _recorded(tmp_path, 4194304) is None


@pytest.mark.parametrize("suffix", ["", ".json"])
def test_a_directory_that_appears_after_any_check_takes_no_file_issue_410(suffix, tmp_path):
    # A directory can appear at a record name between any check the hook makes
    # and its link call, so only a link that names its destination exactly is
    # safe. The shims plant the directory at the last moment, right before the
    # real ln or link runs: nothing may land inside it (#410).
    name = f"ccmorph-session-end.4194304{suffix}"
    shims = tmp_path / "shims"
    shims.mkdir()
    for tool in ("ln", "link"):
        real = shutil.which(tool)
        if real is None:
            continue
        shim = shims / tool
        shim.write_text(
            "#!/bin/sh\n"
            "for last; do :; done\n"
            f'case "$last" in */{name}) mkdir "$last" ;; esac\n'
            f'exec {shlex.quote(real)} "$@"\n',
            encoding="utf-8",
        )
        shim.chmod(0o755)
    env = {
        "TMPDIR": str(tmp_path),
        "PATH": f"{shims}:{os.environ.get('PATH', '/usr/bin:/bin')}",
        "CLAUDE_PLUGIN_ROOT": str(PLUGIN),
        "CLAUDE_CODE_SESSION_ATTENDED": "1",
        "CLAUDE_CODE_SESSION_ID": "sid-410",
        "CLAUDE_CONFIG_DIR": "/profiles/work",
        "CLAUDE_PID": "4194304",
    }
    given = '{"session_id":"sid-410","transcript_path":"/p/sid-410.jsonl"}'
    ran = subprocess.run(
        ["/bin/sh", "-c", _session_end_command()], env=env, input=given, text=True, timeout=HANG_TIMEOUT
    )
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    planted = tmp_path / name
    assert planted.is_dir(), "the shim never ran: the hook did not reach its link call"
    assert [e.name for e in planted.iterdir()] == [], "the hook created a file inside a directory that appeared late"
    other = tmp_path / ("ccmorph-session-end.4194304.json" if suffix == "" else "ccmorph-session-end.4194304")
    assert other.read_text(encoding="utf-8") == (given if suffix == "" else "sid-410"), "the other record was not written"
    leftovers = sorted(
        e.name for e in tmp_path.iterdir() if e.name.startswith("ccmorph-session-end.") and e not in (planted, other)
    )
    assert leftovers == [], "the hook left a temporary file behind"

def test_the_command_claude_code_prints_names_the_script_not_its_source_issue_969():
    # The line Claude Code prints when it cancels or fails a hook carries the
    # hook's command. One call of a file names the script; the shell source
    # that used to sit there told the person nothing (#969).
    command = _session_end_command()
    assert command == 'sh "${CLAUDE_PLUGIN_ROOT}/hooks/session-end.sh"'
    assert (PLUGIN / "hooks/session-end.sh").is_file()
    for source in (";", "mktemp", "link ", "CLAUDE_CODE_SESSION_ATTENDED", "printf"):
        assert source not in command, f"the hook command still carries its source: {source!r}"


@pytest.mark.parametrize("how", ["env", "substituted"])
def test_the_hook_runs_from_a_plugin_root_with_a_space_issue_969(how, tmp_path):
    # Claude Code may hand the plugin root over as an environment variable or
    # substitute it into the command text; either way a path with a space must
    # reach the script unharmed, and the records must land as before (#969).
    root = tmp_path / "Application Support" / "claude-plugin"
    shutil.copytree(PLUGIN, root)
    records = tmp_path / "tmp"
    records.mkdir()
    command = _session_end_command()
    env = {
        "TMPDIR": str(records),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "CLAUDE_CODE_SESSION_ATTENDED": "1",
        "CLAUDE_CODE_SESSION_ID": "sid-969",
        "CLAUDE_CONFIG_DIR": "/profiles/work",
        "CLAUDE_PID": "4194304",
    }
    if how == "env":
        env["CLAUDE_PLUGIN_ROOT"] = str(root)
    else:
        command = command.replace("${CLAUDE_PLUGIN_ROOT}", str(root))
    given = '{"session_id":"sid-969","transcript_path":"/p/sid-969.jsonl"}'
    ran = subprocess.run(["/bin/sh", "-c", command], env=env, input=given, text=True, timeout=HANG_TIMEOUT)
    assert ran.returncode == 0, "no outcome may fail the hook at session end"
    assert _recorded(records, 4194304) == "sid-969"
    assert (records / "ccmorph-session-end.4194304.json").read_text(encoding="utf-8") == given
