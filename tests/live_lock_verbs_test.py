"""The live lock gates sessions, not the verbs that start no session (#217).

The refusal #205 wired into setupDir ran in every caller, so `use` failed with
a message about sessions it never starts and `add` rolled the profile it had
just written back over a lock it could not even make. The lock is the session's
cover: only the paths that start claude refuse without it.
"""
import fcntl
import json
import os
from pathlib import Path
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def run(home, state, verb, input=""):
    """Run one ccmorph verb through the sourced library, as an operator does."""
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH_", "_CCMORPH_", "CLAUDE_", "ANTHROPIC_"))}
    env.update(HOME=str(home), ZDOTDIR=str(home),
               CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"))
    # `add` offers a pasted credential to its endpoint before saving it; point
    # that fetch at a port nothing owns so the box stays offline and the answer
    # is always "not checked" (setup_home does the same for the zsh suite).
    env.update(HTTP_PROXY='http://127.0.0.1:1', HTTPS_PROXY='http://127.0.0.1:1',
               http_proxy='http://127.0.0.1:1', https_proxy='http://127.0.0.1:1')
    env.pop('NO_PROXY', None)
    env.pop('no_proxy', None)
    return subprocess.run(["zsh", "-fc", f'. "$1"; ccmorph {verb}', "_", str(ROOT / "lib/ccmorph.sh")],
                          env=env, input=input, text=True, capture_output=True, timeout=HANG_TIMEOUT)


def fresh_state(tmp_path, conf=False):
    """An isolated state dir; with conf, the profile exists for verbs that
    need one (`add` creates its own and must find no leftover)."""
    home = tmp_path / "home"
    state = home / ".config/ccmorph"
    (state / "profiles").mkdir(parents=True)
    if conf:
        (state / "gw.conf").write_text("type=sub\nemail=me@example.invalid\n")
    return home, state


def exclusive_holder(state, name="gw"):
    """Hold the profile's live lock the way a launch's merge does: exclusively,
    past the launch path's wait."""
    lock = state / ".locks" / f"{name}.live"
    lock.parent.mkdir(parents=True, exist_ok=True)
    holder = open(lock, "a+")
    fcntl.flock(holder, fcntl.LOCK_EX)
    return holder


def shared_holder(state, name="gw"):
    """Hold the profile's live lock the way a running session does: shared."""
    lock = state / ".locks" / f"{name}.live"
    lock.parent.mkdir(parents=True, exist_ok=True)
    holder = open(lock, "a+")
    fcntl.flock(holder, fcntl.LOCK_SH)
    return holder


def test_use_sets_the_default_while_another_launch_holds_the_live_lock(tmp_path):
    # `use` only sets state; before 2026-11-06 it proceeded while another
    # launch linked the profile's shared history.
    home, state = fresh_state(tmp_path, conf=True)
    holder = exclusive_holder(state)
    try:
        result = run(home, state, "use gw")
    finally:
        holder.close()
    assert result.returncode == 0, result.stderr
    assert (state / "active").read_text().strip() == "gw"
    # Says nothing about sessions: no refusal, no launch talk.
    assert result.stderr == "", result.stderr


def test_add_keeps_its_conf_while_another_launch_holds_the_live_lock(tmp_path):
    # The conf and the profile dir `add` just wrote stay; the message names the
    # real cause — a stuck holder — instead of refusing over it.
    home, state = fresh_state(tmp_path)
    holder = exclusive_holder(state)
    try:
        result = run(home, state, "add gw", "token\nsk-ant-oat01-live-lock-fixture-not-real-00000000000000000")
    finally:
        holder.close()
    assert result.returncode == 0, result.stderr
    assert (state / "gw.conf").read_text() == "type=token\ntoken=sk-ant-oat01-live-lock-fixture-not-real-00000000000000000\n"
    assert (state / "profiles" / "gw").is_dir()
    assert "another launch" in result.stderr
    assert "nothing started" not in result.stderr


def test_add_keeps_its_conf_when_the_lock_dir_cannot_be_made(tmp_path):
    # A regular file at <state>/.locks: the lock cannot be made at all, and the
    # message names that, not a holder.
    home, state = fresh_state(tmp_path)
    (state / ".locks").write_text("not a directory\n")
    result = run(home, state, "add gw", "token\nsk-ant-oat01-live-lock-fixture-not-real-00000000000000000")
    assert result.returncode == 0, result.stderr
    assert (state / "gw.conf").read_text() == "type=token\ntoken=sk-ant-oat01-live-lock-fixture-not-real-00000000000000000\n"
    assert (state / "profiles" / "gw").is_dir()
    assert "could not be created" in result.stderr
    assert "another launch" not in result.stderr


def test_a_launch_still_refuses_without_its_live_lock(tmp_path):
    # #205 kept: a session must not start claude through the window the live
    # lock covers, and the 2026-11-06 message still says so.
    home, state = fresh_state(tmp_path, conf=True)
    holder = exclusive_holder(state)
    try:
        result = run(home, state, "_env gw")
    finally:
        holder.close()
    assert result.returncode == 1
    assert "another launch of 'gw' is holding its live lock" in result.stderr
    assert "nothing started" in result.stderr


def test_a_launch_refusal_names_a_lock_file_that_cannot_be_made(tmp_path):
    # The refusal no longer blames a launch for a lock file that could not be
    # created: the reason is plain and truthful (promise 8).
    home, state = fresh_state(tmp_path, conf=True)
    (state / ".locks").write_text("not a directory\n")
    result = run(home, state, "_env gw")
    assert result.returncode == 1
    assert "could not be created" in result.stderr
    assert "nothing started" in result.stderr
    assert "another launch" not in result.stderr


def test_login_while_a_session_runs_saves_the_approval_and_leaves_the_file_alone(tmp_path):
    # #299: a session holds the live lock shared and rewrites .claude.json
    # itself, so the approval must not rewrite the file under it. login saves
    # the answer for the next launch to record; the file is left alone.
    home, state = fresh_state(tmp_path)
    key = "sk-issue-299-test-key-0001"
    (state / "gw.conf").write_text(f"type=apikey\napi_key={key}\n")
    (state / "profiles" / "gw").mkdir(parents=True)
    claude_json = state / "profiles" / "gw" / ".claude.json"
    body = json.dumps({"numStartups": 3,
                       "customApiKeyResponses": {"approved": [], "rejected": [key[-20:]]}})
    claude_json.write_text(body)

    holder = shared_holder(state)
    try:
        result = run(home, state, "login gw")
    finally:
        holder.close()

    assert result.returncode == 0, result.stderr
    assert "next launch" in result.stdout, result.stdout
    assert result.stderr == "", result.stderr
    assert claude_json.read_text() == body
    markers = list((state / ".locks").glob("gw.approve-*"))
    assert len(markers) == 1, markers
    marker = markers[0]
    assert key not in marker.name and key[-20:] not in marker.name, marker.name
    assert marker.stat().st_size == 0, marker.name
    assert marker.stat().st_mode & 0o777 == 0o600, marker.name
