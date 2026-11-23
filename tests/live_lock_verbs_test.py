"""The live lock gates sessions, not the verbs that start no session (#217).

The refusal #205 wired into setupDir ran in every caller, so `use` failed with
a message about sessions it never starts and `add` rolled the profile it had
just written back over a lock it could not even make. The lock is the session's
cover: only the paths that start claude refuse without it.
"""
import fcntl
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


def test_rm_of_a_leftover_names_only_paths_that_exist_and_takes_the_live_lock(tmp_path):
    """GH-837: Ctrl-C at a sign-in left profiles/<name> and .locks/<name>.live.
    rm offered auth/<name> too, which the Claude-account sign-in never makes,
    and left the lock behind; the next add then still found the name held.
    What rm offers is what it removes, and the lock is one of those pieces."""
    home, state = fresh_state(tmp_path)
    leftover = state / "profiles" / "gw"
    leftover.mkdir(parents=True)
    lock = state / ".locks" / "gw.live"
    lock.parent.mkdir(parents=True)
    lock.write_text("")
    result = run(home, state, "rm gw", "y\n")
    assert result.returncode == 0, result.stderr
    assert "Removed gw" in result.stdout
    assert str(leftover) in result.stderr
    assert str(lock) in result.stderr
    assert not leftover.exists()
    assert not lock.exists()
    assert str(state / "auth" / "gw") not in result.stderr


def test_rm_of_a_complete_profile_takes_its_live_lock_too(tmp_path):
    # The lock is the profile's, so the removal the help caps with "This cannot
    # be undone" leaves none of it behind (GH-837).
    home, state = fresh_state(tmp_path, conf=True)
    lock = state / ".locks" / "gw.live"
    lock.parent.mkdir(parents=True)
    lock.write_text("")
    result = run(home, state, "rm gw", "y\n")
    assert result.returncode == 0, result.stderr
    assert not (state / "gw.conf").exists()
    assert not lock.exists()


def test_rm_of_a_leftover_auth_dir_names_it_and_not_a_missing_home(tmp_path):
    # A provider sign-in leaves auth/<name> and no profile home; the offer
    # follows the disk in this direction too (GH-837).
    home, state = fresh_state(tmp_path)
    auth = state / "auth" / "gw"
    auth.mkdir(parents=True)
    result = run(home, state, "rm gw", "y\n")
    assert result.returncode == 0, result.stderr
    assert str(auth) in result.stderr
    assert str(state / "profiles" / "gw") not in result.stderr
    assert not auth.exists()


def test_rm_of_a_leftover_lock_alone_offers_and_takes_it(tmp_path):
    # The probe counts the live lock as leftover state, so rm can clean up
    # what an older build left behind (GH-837).
    home, state = fresh_state(tmp_path)
    lock = state / ".locks" / "gw.live"
    lock.parent.mkdir(parents=True)
    lock.write_text("")
    result = run(home, state, "rm gw", "y\n")
    assert result.returncode == 0, result.stderr
    assert str(lock) in result.stderr
    assert not lock.exists()


def test_rm_answered_no_keeps_the_leftover_and_its_lock(tmp_path):
    home, state = fresh_state(tmp_path)
    leftover = state / "profiles" / "gw"
    leftover.mkdir(parents=True)
    lock = state / ".locks" / "gw.live"
    lock.parent.mkdir(parents=True)
    lock.write_text("")
    result = run(home, state, "rm gw", "n\n")
    assert result.returncode == 1
    assert "cancelled; nothing removed" in result.stderr
    assert leftover.exists()
    assert lock.exists()


def test_rm_keeps_a_live_lock_a_running_launch_holds(tmp_path):
    # GH-837's removal must not undercut #185: a lock another launch holds is
    # that session's cover, so rm leaves the file and says why.
    home, state = fresh_state(tmp_path, conf=True)
    lock = state / ".locks" / "gw.live"
    lock.parent.mkdir(parents=True)
    lock.write_text("")
    holder = open(lock, "a+")
    fcntl.flock(holder, fcntl.LOCK_SH)
    try:
        result = run(home, state, "rm gw", "y\n")
    finally:
        holder.close()
    assert result.returncode == 0, result.stderr
    assert not (state / "gw.conf").exists()
    assert lock.exists()
    assert f"kept {lock}" in result.stderr
    assert "still holds it" in result.stderr
