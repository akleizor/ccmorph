"""GH-1028: Ctrl-C is the escape hatch the product itself advertises.

`help add` says "Ctrl-C cancels setup", docs/guide.md promises "Ctrl+C also
exits cleanly without a traceback", and the sign-in waits print "Ctrl-C to
cancel". The UX walk found none of it true: a ccmorph-core started with SIGINT
ignored — the disposition zsh gives an async job, so every scripted, SSH or
piped call — kept ignoring it (Go honors an inherited ignore), and the add
flow never installed a handler: SIGINT at the sign-in menu, the credential
prompt or the provider wait did nothing (the walked core survived INT and died
only to TERM, exit 143), leaving the user in the bridge's own ~5-minute auth
timeout. These tests start the real core with the walked disposition and send
exactly the signal Ctrl-C sends, to the core's own pid.
"""
import fcntl
import os
import pty
import select
import signal
import struct
import subprocess
import termios
import threading
import time
from pathlib import Path

import pytest

from support import CORE, HANG_TIMEOUT, needs_shim, wait_until

pytestmark = needs_shim

ROOT = Path(__file__).resolve().parents[1]


def add_home(tmp_path, extra_bins=()):
    """A fresh state/home and a PATH only these fakes are on."""
    state, home, bindir, tmp = (tmp_path / n for n in ("state", "home", "bin", "tmp"))
    for d in (state, home, bindir, tmp):
        d.mkdir(parents=True)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH_", "_CCMORPH_", "CLAUDE_", "ANTHROPIC_"))}
    env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"),
               CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"),
               TMPDIR=str(tmp),
               PATH=os.pathsep.join([str(bindir), *(str(b) for b in extra_bins), "/usr/bin", "/bin"]),
               # The apikey flow offers a pasted key to its endpoint: point it
               # at a port nothing owns, as the installed-home suite does.
               HTTP_PROXY="http://127.0.0.1:1", HTTPS_PROXY="http://127.0.0.1:1",
               http_proxy="http://127.0.0.1:1", https_proxy="http://127.0.0.1:1")
    return env, state, bindir


def start_add(env, started_ignored=True):
    """`ccmorph add sigv` on a piped stdin, answered by the test line by line.

    started_ignored reproduces the walked shape: async jobs get SIG_IGN for
    INT, and exec carries it into the core — the disposition every script,
    agent harness and non-interactive shell gives the command.
    """
    def preexec():
        os.setsid()  # a group of one: only the core gets the test's signal
        if started_ignored:
            signal.signal(signal.SIGINT, signal.SIG_IGN)
    p = subprocess.Popen([str(CORE), "cli", "main", "add", "sigv"],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         text=True, env=env, preexec_fn=preexec)
    return p


class _StreamWatch:
    """One reader per stream, from the first wait to the process's end.

    Readline-based waiters cannot see a prompt that has no newline yet, and two
    of them on one stream race each other for its bytes. One byte pump feeds a
    shared buffer, so every wait reads the same text and a prompt is matchable
    the moment its last character arrives.
    """

    def __init__(self, stream):
        self.buf = []
        self.cond = threading.Condition()
        self.closed = threading.Event()
        threading.Thread(target=self._pump, args=(stream,), daemon=True).start()

    def _pump(self, stream):
        while True:
            ch = stream.read(1)
            with self.cond:
                if not ch:
                    self.closed.set()
                    self.cond.notify_all()
                    return
                self.buf.append(ch)
                self.cond.notify_all()

    def text(self):
        # A caller reads after the command exited: wait for EOF so the pump's
        # last bytes are in, never a half-read stream.
        self.closed.wait(HANG_TIMEOUT)
        with self.cond:
            return "".join(self.buf)

    def wait(self, needle, timeout):
        deadline = time.monotonic() + timeout
        with self.cond:
            while True:
                text = "".join(self.buf)
                if needle in text:
                    return text
                remain = deadline - time.monotonic()
                if remain <= 0:
                    return None
                self.cond.wait(remain)


def stream_watch(proc, stream):
    watches = getattr(proc, "_sigint_watches", None)
    if watches is None:
        watches = {}
        proc._sigint_watches = watches
    if stream not in watches:
        watches[stream] = _StreamWatch(getattr(proc, stream))
    return watches[stream]


def stream_text(proc, stream="stderr"):
    return stream_watch(proc, stream).text()


def wait_for_line(proc, needle, timeout=HANG_TIMEOUT, stream="stderr"):
    """True once `needle` has passed on the named stream; prompts print without
    a newline, so the wait is over the stream's own text, not whole lines."""
    text = stream_watch(proc, stream).wait(needle, timeout)
    if text is None:
        proc.kill()
        pytest.fail(f"'{needle}' never reached {stream}:\n{stream_text(proc, stream)}")


def sigint_and_wait(proc, timeout=HANG_TIMEOUT):
    """Ctrl-C itself: SIGINT to the command's own pid, and its exit."""
    proc.send_signal(signal.SIGINT)
    try:
        return proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        pytest.fail("SIGINT cancelled nothing: the command is still waiting "
                    "exactly as in the walked repro")


@pytest.mark.parametrize("started_ignored", [True, False], ids=["started-ignored", "default-disposition"])
def test_sigint_cancels_a_piped_add_prompt(tmp_path, started_ignored):
    """The walked repro's first stop: SIGINT at a menu a pipe is answering
    ends the add, saves nothing, and exits as the signal's default would."""
    env, state, _ = add_home(tmp_path)
    proc = start_add(env, started_ignored=started_ignored)
    try:
        proc.stdin.write("apikey\n")  # the menu answer; the provider menu is the next wait
        proc.stdin.flush()
        wait_for_line(proc, "Provider")  # the second menu's title: the prompt is waiting
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"Ctrl-C must end the add as the default disposition does (130/-2), got {rc}"
        assert not (state / "sigv.conf").exists(), "a cancelled add saved a profile"
        assert not (state / "profiles" / "sigv").exists(), "a cancelled add left a profile dir"
    finally:
        proc.kill()
        proc.wait()


def test_sigint_cancels_the_provider_sign_in_wait(tmp_path):
    """The walked repro's second stop: SIGINT while a browser provider login
    waits ends the wait through the cancel path — the bridge dies of it, the
    half-made auth dir is cleaned, and no profile is saved. Before the fix a
    signal to the core alone was ignored and the walk sat in the bridge's own
    auth timeout (~5 min)."""
    fakebin = tmp_path / "bridge"
    fakebin.mkdir()
    pidfile = tmp_path / "bridge.pid"
    (fakebin / "cliproxyapi").write_text(
        "#!/bin/sh\n"
        'case "$1" in\n'
        "--help) printf '  -antigravity-login  browser login\\n' ;;\n"
        '-antigravity-login) printf "%s\\n" "$$" > \'' + str(pidfile) + '\'; exec sleep 300 ;;\n'
        "esac\n"
        "exit 0\n")
    (fakebin / "cliproxyapi").chmod(0o755)
    env, state, _ = add_home(tmp_path, extra_bins=[fakebin])
    proc = start_add(env)
    try:
        proc.stdin.write("antigravity\n")
        proc.stdin.flush()
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")  # the line that promises the way out
        deadline = time.monotonic() + HANG_TIMEOUT  # until the fake bridge is actually up and waiting
        while not pidfile.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        assert pidfile.exists(), "the fake bridge never started"
        bridge_pid = int(pidfile.read_text().strip())
        rc = sigint_and_wait(proc)
        assert rc in (1, 130), f"a cancelled sign-in answers its failure path or the signal, got {rc}"
        if rc == 1:
            assert "login failed" in stream_text(proc), "the rc 1 path is providerLogin's failure"
        assert not (state / "sigv.conf").exists(), "a cancelled sign-in saved a profile"
        assert not (state / "auth" / "sigv").exists(), "a cancelled sign-in left its auth dir"
    finally:
        try:
            os.kill(bridge_pid, 0)
            os.kill(bridge_pid, signal.SIGKILL)  # the walk's orphan backstop
        except (ProcessLookupError, NameError):
            pass
        proc.kill()
        proc.wait()


def launch_env(tmp_path, conf, binenv=(), **extra):
    """State with one profile and the env a bare `ccmorph-core launch` reads."""
    state = tmp_path / "state"
    (state / "profiles" / "gw").mkdir(parents=True)
    (tmp_path / "tmp").mkdir(exist_ok=True)
    (tmp_path / "home").mkdir(exist_ok=True)
    (state / "gw.conf").write_text(conf)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH_", "_CCMORPH_", "CLAUDE_", "ANTHROPIC_"))}
    env.update(HOME=str(tmp_path / "home"), XDG_CONFIG_HOME=str(tmp_path / "home" / ".config"),
               CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"),
               TMPDIR=str(tmp_path / "tmp"), _CCMORPH_LAUNCH_PROFILE="gw",
               PATH=os.pathsep.join([str(b) for b in binenv] + ["/usr/bin", "/bin"]),
               HTTP_PROXY="http://127.0.0.1:1", HTTPS_PROXY="http://127.0.0.1:1",
               http_proxy="http://127.0.0.1:1", https_proxy="http://127.0.0.1:1")
    env.update(extra)
    return env, state


def test_sigint_cancels_the_launch_endpoint_check(tmp_path):
    """`ccmorph run` prints "Checking 'gw' gateway (up to Ns); Ctrl-C to
    cancel." — and the walked run ignored SIGINT through that whole wait,
    because the launcher swallowed INT for its whole life and the swallow was
    scoped to the handoff to Claude Code only by accident. Ctrl-C before any
    session exists points at the launcher itself."""
    fakebin = tmp_path / "bin"
    fakebin.mkdir()
    (tmp_path / "tmp").mkdir()
    (fakebin / "curl").write_text("#!/bin/sh\nexec sleep 30\n")  # the probe never answers
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin], CCMORPH_REACH_BOUND="25", CCMORPH_REACH_PROGRESS="0.2")
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        wait_for_line(proc, "Checking 'gw' gateway")  # the line that promises the way out
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"Ctrl-C must end the checking launch (130/-2), got {rc}"
    finally:
        proc.kill()
        proc.wait()


def test_sigint_during_a_running_session_is_claudes_own(tmp_path):
    """The half of the promise that must not move: while the session runs,
    INT belongs to Claude Code (it shares the process group). A SIGINT to the
    launcher during a live session ends nothing — the launch is still there to
    report the session's exit."""
    fakebin = tmp_path / "bin"
    fakebin.mkdir()
    up, pidf = tmp_path / "claude.started", tmp_path / "claude.pid"
    (fakebin / "claude").write_text(
        "#!/bin/sh\n"
        'if [ "$1" = --help ]; then printf "Commands:\\n  mcp\\n"; exit 0; fi\n'
        "printf x >> '" + str(up) + "'\n"
        "printf '%s\\n' \"$$\" > '" + str(pidf) + "'\n"
        "trap '' INT\n"  # a real Claude Code lives through Ctrl-C; it handles it
        "while :; do sleep 1; done\n")
    (fakebin / "claude").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\napi_key=k", binenv=[fakebin])
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        deadline = time.monotonic() + HANG_TIMEOUT
        while not pidf.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        assert pidf.exists(), "the session never started"
        claude_pid = int(pidf.read_text().strip())
        proc.send_signal(signal.SIGINT)  # Ctrl-C at the launcher, mid-session
        proc.terminate()  # end the session; the launcher must still be alive to report it
        rc = proc.wait(timeout=HANG_TIMEOUT)
        assert rc not in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            "a mid-session SIGINT ended the launcher itself: INT no longer belongs to the session"
        assert up.read_bytes().count(b"x") == 1, "the session was restarted instead of ended"
    finally:
        try:
            os.kill(claude_pid, signal.SIGKILL)
        except (ProcessLookupError, NameError, UnboundLocalError):
            pass
        proc.kill()
        proc.wait()


# --- GH-1028, the merge gate's contract: every wait and every prompt ---------

def start_core(env, args, stdin=True):
    """The core with a piped stdin/stdout/stderr, in its own process group."""
    return subprocess.Popen([str(CORE), *args],
                            stdin=subprocess.PIPE if stdin else subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                            env=env, preexec_fn=os.setsid)


def wait_for_pidfile(path):
    """The pid a fake child writes before it blocks: the wait has a child."""
    assert wait_until(path.exists, HANG_TIMEOUT), f"{path.name} never appeared"
    return int(path.read_text().strip())


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def wait_gone(pid):
    """A cancelled wait's child must not outlive the command."""
    assert wait_until(lambda: not alive(pid), HANG_TIMEOUT), \
        f"child {pid} outlived the cancelled command"


def bridge_script(pidfile, staged=None, survives_int=False):
    """cliproxyapi: --help lists one browser login; the login blocks, and with
    survives_int it answers the forwarded signal by exiting 0."""
    stage = ""
    if staged is not None:
        stage = "mkdir -p '" + str(staged.parent) + "'\nprintf '{}' > '" + str(staged) + "'\n"
    trap = "trap 'exit 0' INT\n" if survives_int else ""
    return ("#!/bin/sh\n"
            'case "$1" in\n'
            "--help) printf '  -antigravity-login  browser login\\n' ;;\n"
            '-antigravity-login)\n' + stage + trap +
            "printf '%s\\n' \"$$\" > '" + str(pidfile) + "'\n"
            "exec sleep 300 ;;\n"
            "esac\n"
            "exit 0\n")


def install_bridge(tmp_path, state, filename="cliproxyapi", staged=None, survives_int=False):
    fakebin = tmp_path / filename
    fakebin.mkdir()
    pidfile = tmp_path / (filename + ".pid")
    (fakebin / "cliproxyapi").write_text(bridge_script(pidfile, staged, survives_int))
    (fakebin / "cliproxyapi").chmod(0o755)
    return fakebin, pidfile


@pytest.mark.parametrize("wait", [
    "initial-menu", "custom-endpoint", "hidden-api-key", "hidden-token",
    "partial-line", "invalid-answer"], ids=str)
def test_sigint_cancels_every_piped_add_prompt(tmp_path, wait):
    """Each wait the pipe can stop at is its own prompt: the walk's repro
    reached the provider menu, but the initial menu, the custom endpoint, the
    hidden credential reads and a re-prompt all name Ctrl-C too (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    proc = start_add(env)
    try:
        if wait == "initial-menu":
            wait_for_line(proc, "Sign-in method")
        elif wait in ("custom-endpoint", "hidden-api-key", "partial-line"):
            proc.stdin.write("apikey\n")
            proc.stdin.flush()
            wait_for_line(proc, "Provider")
            if wait == "partial-line":
                proc.stdin.write("cust")  # no newline: still reading the line
                proc.stdin.flush()
            else:
                proc.stdin.write("custom\n")
                proc.stdin.flush()
                wait_for_line(proc, "Endpoint (blank")
                if wait == "hidden-api-key":
                    proc.stdin.write("\n")  # blank endpoint: the key prompt is next
                    proc.stdin.flush()
                    wait_for_line(proc, "API key for sigv")
        elif wait == "hidden-token":
            proc.stdin.write("token\n")
            proc.stdin.flush()
            wait_for_line(proc, "Paste token for sigv")
        else:
            proc.stdin.write("bogus\n")
            proc.stdin.flush()
            wait_for_line(proc, "not a sign-in method")  # the re-prompt is waiting
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"Ctrl-C at the {wait} must end the add as the default does, got {rc}"
        assert not (state / "sigv.conf").exists(), "a cancelled add saved a profile"
        assert not (state / "profiles" / "sigv").exists(), "a cancelled add left a profile dir"
        err = stream_text(proc)
        assert "panic:" not in err and "goroutine " not in err, "Ctrl-C printed a traceback"
    finally:
        proc.kill()
        proc.wait()


def test_sigint_at_the_provider_notice_cancels_before_the_bridge_is_up(tmp_path):
    """The notice is the moment a person reads "Ctrl-C to cancel" and presses
    it. The handoff owns the signal before the line is printed, so even that
    first instant reaches the bridge's failure path and cleans up (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    fakebin, pidfile = install_bridge(tmp_path, state, staged=state / "auth" / "sigv" / "staged.json")
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_add(env)
    try:
        proc.stdin.write("antigravity\n")
        proc.stdin.flush()
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        rc = sigint_and_wait(proc)  # the instant the line is readable
        assert rc in (1, *(-int(signal.SIGINT.value), 128 + signal.SIGINT.value)), \
            f"a cancel at the notice is a cancel, got {rc}"
        assert not (state / "sigv.conf").exists(), "a cancelled sign-in saved a profile"
        assert not (state / "auth" / "sigv").exists(), "a cancelled sign-in left its auth dir"
        assert not list(Path(env["TMPDIR"]).glob("ccmorph-login-*")), "the temporary config survived"
        if pidfile.exists():
            wait_gone(int(pidfile.read_text().strip()))
    finally:
        proc.kill()
        proc.wait()


@pytest.mark.parametrize("sig", ["SIGHUP", "SIGTERM", "group-SIGINT"], ids=str)
def test_signals_cancel_the_provider_wait_through_the_handoff(tmp_path, sig):
    """HUP and TERM are what a closing terminal and a dropped SSH send, and a
    foreground Ctrl-C reaches the whole group: each must end the bridge wait
    through the failure path, with the bridge dead and the auth dir gone."""
    env, state, _ = add_home(tmp_path)
    fakebin, pidfile = install_bridge(tmp_path, state)
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_add(env)
    try:
        proc.stdin.write("antigravity\n")
        proc.stdin.flush()
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        bridge_pid = wait_for_pidfile(pidfile)
        if sig == "group-SIGINT":
            os.killpg(proc.pid, signal.SIGINT)
        else:
            proc.send_signal(getattr(signal, sig))
        rc = proc.wait(timeout=HANG_TIMEOUT)
        assert rc in (1, 130), f"{sig} cancels the wait (failure path or the signal), got {rc}"
        if rc == 1:
            assert "login failed" in stream_text(proc)
        wait_gone(bridge_pid)
        assert not (state / "auth" / "sigv").exists(), "the auth dir survived the cancel"
        assert not (state / "sigv.conf").exists(), "a cancelled sign-in saved a profile"
    finally:
        proc.kill()
        proc.wait()


def test_a_cancelled_fresh_provider_add_removes_a_staged_credential(tmp_path):
    """A bridge that stages a credential before it blocks must not leave it
    behind: the fresh add owns everything under auth/<name> until it saves."""
    env, state, _ = add_home(tmp_path)
    fakebin, pidfile = install_bridge(tmp_path, state, staged=state / "auth" / "sigv" / "credential.json")
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_add(env)
    try:
        proc.stdin.write("antigravity\n")
        proc.stdin.flush()
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        bridge_pid = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (1, 130)
        wait_gone(bridge_pid)
        assert not (state / "auth" / "sigv").exists(), "a staged credential outlived the cancel"
        assert not (state / "sigv.conf").exists(), "a cancelled sign-in saved a profile"
    finally:
        proc.kill()
        proc.wait()


def test_cancelling_login_keeps_an_existing_provider_credential(tmp_path):
    """`ccmorph login` of a provider profile renews the sign-in; a cancel must
    keep the stored credential and the original conf, and remove only what the
    attempt made (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    (state / "profiles" / "gw").mkdir(parents=True)
    (state / "gw.conf").write_text("type=provider\nprovider=antigravity\n")
    (state / "auth" / "gw").mkdir(parents=True)
    stored = state / "auth" / "gw" / "credential.json"
    stored.write_text('{"token":"kept"}')
    conf_before = (state / "gw.conf").read_text()
    fakebin, pidfile = install_bridge(tmp_path, state, staged=state / "auth" / "gw" / "new.json")
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_core(env, ["cli", "main", "login", "gw"])
    try:
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        bridge_pid = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (1, 130), f"the login cancel path answers 1 or the signal, got {rc}"
        wait_gone(bridge_pid)
        assert stored.read_text() == '{"token":"kept"}', "the stored credential was lost"
        assert (state / "gw.conf").read_text() == conf_before, "the conf was rewritten"
        assert (state / "auth" / "gw").is_dir(), "the profile's auth dir was removed"
        assert not list(Path(env["TMPDIR"]).glob("ccmorph-login-*")), "the temporary config survived"
    finally:
        proc.kill()
        proc.wait()


def claude_script(pidfile, block, survives_int=False):
    """A fake Claude Code that blocks in exactly one auth subcommand; with
    survives_int it catches the forwarded SIGINT and exits 0."""
    trap = "trap 'exit 0' INT; " if survives_int else ""
    pre = "printf '{\"loggedIn\": true, \"email\": \"kept@example.com\"}\\n'; " if survives_int else ""
    return ("#!/bin/sh\n"
            'case "$1 $2" in\n'
            f'"auth logout") if [ {block!r} = logout ]; then printf \'%s\\n\' "$$" > \'{pidfile}\'; {trap}exec sleep 300; fi ;;\n'
            f'"auth login") if [ {block!r} = login ]; then printf \'%s\\n\' "$$" > \'{pidfile}\'; {trap}exec sleep 300; fi ;;\n'
            f'"auth status") if [ {block!r} = status ]; then {pre}printf \'%s\\n\' "$$" > \'{pidfile}\'; {trap}exec sleep 300; fi; '
            "printf '{\"loggedIn\": true, \"email\": \"kept@example.com\"}\\n' ;;\n"
            "esac\n"
            "exit 0\n")


@pytest.mark.parametrize("block", ["logout", "login", "status"], ids=str)
def test_sigint_cancels_each_claude_account_wait_and_rolls_back(tmp_path, block):
    """The Claude-account add signs in before it saves: logout, the browser
    login and the status readback are all waits it advertises, and a cancel at
    any of them must leave no profile home, live lock or conf — and the same
    name must be usable again without rm (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    pidfile = tmp_path / "claude.pid"
    (tmp_path / "bin" / "claude").write_text(claude_script(pidfile, block))
    (tmp_path / "bin" / "claude").chmod(0o755)
    proc = start_add(env)
    try:
        proc.stdin.write("1\n")
        proc.stdin.flush()
        child = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (1, 130), f"the {block} cancel answers the failure path or the signal, got {rc}"
        wait_gone(child)
        assert not (state / "sigv.conf").exists(), "a cancelled add saved a profile"
        assert not (state / "profiles" / "sigv").exists(), "a cancelled add left its profile home"
        assert not (state / ".locks" / "sigv.live").exists(), "a cancelled add left its live lock"
    finally:
        proc.kill()
        proc.wait()
    # The same name is usable again: the rollback removed what the cancel left.
    (tmp_path / "bin" / "claude").write_text(claude_script(tmp_path / "never.pid", "none"))
    (tmp_path / "bin" / "claude").chmod(0o755)
    again = start_add(env)
    try:
        again.stdin.write("1\n")
        again.stdin.flush()
        out, err = again.communicate(timeout=HANG_TIMEOUT)
        assert again.returncode == 0, f"the name was not reusable after a cancel:\n{out}\n{err}"
        assert (state / "sigv.conf").exists(), "the retried add did not save its profile"
    finally:
        again.kill()
        again.wait()


@pytest.mark.parametrize("flow", ["apikey", "token"], ids=str)
def test_sigint_during_credential_validation_saves_nothing(tmp_path, flow):
    """A credential is offered to its endpoint before it is saved. Ctrl-C while
    curl is still holding the answer must end the add as the signal does, with
    no conf written and the probe curl stopped (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    pidfile = tmp_path / "curl.pid"
    (tmp_path / "bin" / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (tmp_path / "bin" / "curl").chmod(0o755)
    env["PATH"] = os.pathsep.join([str(tmp_path / "bin"), env["PATH"]])
    proc = start_add(env)
    try:
        if flow == "apikey":
            proc.stdin.write("apikey\ncustom\nhttps://10.255.255.1:8080\nsecret\n")
        else:
            proc.stdin.write("token\nsk-ant-oat01-ABCDEFGHIJKLMNOPQRSTUVWX\n")
        proc.stdin.flush()
        child = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"Ctrl-C during validation is the signal's exit, got {rc}"
        wait_gone(child)
        assert not (state / "sigv.conf").exists(), "an unvalidated credential was saved"
        assert not (state / "profiles" / "sigv").exists(), "a cancelled add left a profile dir"
    finally:
        proc.kill()
        proc.wait()


def test_sigint_while_the_bridge_menu_is_still_building_cancels_the_add(tmp_path):
    """The provider rows come from `cliproxyapi --help`; a bridge that hangs
    there must not make the add uncancellable before its first prompt."""
    env, state, _ = add_home(tmp_path)
    fakebin = tmp_path / "bridge"
    fakebin.mkdir()
    pidfile = tmp_path / "help.pid"
    (fakebin / "cliproxyapi").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "cliproxyapi").chmod(0o755)
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_add(env)
    try:
        child = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"Ctrl-C during the bridge menu read is the signal's exit, got {rc}"
        wait_gone(child)
        assert not (state / "sigv.conf").exists(), "a cancelled add saved a profile"
    finally:
        proc.kill()
        proc.wait()


# --- A terminal: actual Ctrl-C bytes, actual restored termios -----------------

def _own_terminal():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)


class Terminal:
    """One process on a pty, with the terminal's input mode on both sides."""

    MODES = termios.ECHO | termios.ICANON | termios.ISIG

    def __init__(self, argv, env):
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 100, 0, 0))
        self.before = termios.tcgetattr(slave)[3] & self.MODES
        self.proc = subprocess.Popen(argv, stdin=slave, stdout=slave, stderr=slave,
                                     env=env, preexec_fn=_own_terminal)
        os.close(slave)
        self.out = bytearray()
        self.deadline = time.monotonic() + HANG_TIMEOUT

    def read_until(self, needle):
        want = needle.encode() if isinstance(needle, str) else needle
        while want not in self.out:
            remain = self.deadline - time.monotonic()
            if remain <= 0:
                pytest.fail(f"'{needle}' never reached the terminal:\n"
                            + self.out.decode("utf-8", "replace"))
            ready, _, _ = select.select([self.master], [], [], remain)
            if not ready:
                continue
            try:
                chunk = os.read(self.master, 4096)
            except OSError:
                chunk = b""
            if not chunk:
                break
            self.out += chunk
        if want not in self.out:
            pytest.fail(f"'{needle}' never reached the terminal:\n"
                        + self.out.decode("utf-8", "replace"))

    def write(self, data):
        os.write(self.master, data.encode() if isinstance(data, str) else data)

    def _drain(self):
        """What the pty buffered between the last read and the exit."""
        while True:
            remain = self.deadline - time.monotonic()
            if remain <= 0:
                return
            ready, _, _ = select.select([self.master], [], [], min(remain, 0.2))
            if not ready:
                return
            try:
                chunk = os.read(self.master, 4096)
            except OSError:
                return
            if not chunk:
                return
            self.out += chunk

    def wait(self):
        try:
            rc = self.proc.wait(timeout=max(0.1, self.deadline - time.monotonic()))
        except subprocess.TimeoutExpired:
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
            except OSError:
                pass
            self.proc.wait(timeout=HANG_TIMEOUT)
            pytest.fail("the terminal process never exited")
        self._drain()
        return rc

    def restored(self):
        return (termios.tcgetattr(self.master)[3] & self.MODES) == self.before

    def close(self):
        try:
            os.killpg(self.proc.pid, signal.SIGKILL)
        except OSError:
            pass
        try:
            self.proc.wait(timeout=HANG_TIMEOUT)
        except subprocess.TimeoutExpired:
            pass
        os.close(self.master)


def terminal_env(tmp_path, extra_bins=(), **extra):
    env, state, _ = add_home(tmp_path, extra_bins=extra_bins)
    env.update(TERM="xterm-256color", NO_COLOR="1")
    env.update(extra)
    return env, state


def test_ctrl_c_cancels_the_terminal_name_prompt(tmp_path):
    """A partial profile name is a raw-mode wait: Ctrl-C is the byte the prompt
    reads, its own Cancelled line is printed, and the terminal comes back in
    the mode it had (GH-1028)."""
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add"], env)
    try:
        t.read_until("New profile name")
        t.write("sigv")
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1, f"the cancelled terminal add answers 1, got {rc}"
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the terminal's echo/canonical/signal modes were not restored"
        assert not (state / "sigv.conf").exists(), "a partial name was saved"
        assert not (state / "profiles" / "sigv").exists(), "a partial name made a profile dir"
    finally:
        t.close()


def test_ctrl_c_cancels_the_terminal_native_menu(tmp_path):
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1, f"the add over a cancelled native menu answers 1, got {rc}"
        assert t.restored(), "the native picker left the terminal raw"
        assert not (state / "sigv.conf").exists()
    finally:
        t.close()


def test_ctrl_c_cancels_the_terminal_numbered_menu(tmp_path):
    """TERM=dumb is the numbered fallback's own screen: the same single Ctrl-C
    leaves through its Cancelled line and restores the terminal."""
    env, state = terminal_env(tmp_path, TERM="dumb")
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the numbered picker left the terminal raw"
        assert not (state / "sigv.conf").exists()
    finally:
        t.close()


@pytest.mark.parametrize("prompt", ["endpoint", "api-key"], ids=str)
def test_ctrl_c_cancels_the_terminal_custom_endpoint_and_credential(tmp_path, prompt):
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x1b[B\x1b[B\r")  # native menu: sub, token, apikey
        t.read_until("Provider — sigv")
        t.write("custom\r")  # the numbered fallback has no fzf here
        t.read_until("Endpoint (blank")
        if prompt == "api-key":
            t.write("\r")
            t.read_until("API key for sigv")
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1, f"the cancelled terminal add answers 1, got {rc}"
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the raw prompt left the terminal raw"
        assert not (state / "sigv.conf").exists()
        assert not (state / "profiles" / "sigv").exists()
    finally:
        t.close()


def test_sigint_during_a_terminal_raw_prompt_restores_the_terminal(tmp_path):
    """A signal to the core, not a Ctrl-C byte: the in-process raw prompt must
    answer it, restore, and cancel (GH-1028)."""
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add"], env)
    try:
        t.read_until("New profile name")
        t.write("sigv")
        t.proc.send_signal(signal.SIGINT)
        rc = t.wait()
        assert rc == 1
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "SIGINT at a raw prompt left the terminal raw"
        assert not (state / "sigv.conf").exists()
    finally:
        t.close()


def test_ctrl_c_cancels_the_terminal_fzf_provider_menu(tmp_path):
    """fzf owns the keys for the provider search. Ctrl-C at the terminal hits
    the whole foreground group; the launcher stands down and fzf's own Esc-like
    exit leaves the picker through Cancelled, with the terminal restored."""
    fakebin = tmp_path / "fzfbin"
    fakebin.mkdir()
    pidfile = tmp_path / "fzf.pid"
    (fakebin / "fzf").write_text(
        "#!/bin/sh\n"
        'if [ "$1" = --version ]; then echo 0.71.0; exit 0; fi\n'
        "printf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "fzf").chmod(0o755)
    env, state = terminal_env(tmp_path, extra_bins=[fakebin])
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x1b[B\x1b[B\r")
        fzf_pid = wait_for_pidfile(pidfile)
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1, f"the fzf picker cancel answers 1, got {rc}"
        wait_gone(fzf_pid)
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the fzf cancel left the terminal raw"
        assert not (state / "sigv.conf").exists()
    finally:
        t.close()


# --- ccmorph run: the public command, its probe, its adapter, its next pass ----

def test_ctrl_c_cancels_the_public_run_at_the_gateway_line(tmp_path):
    """`ccmorph run gw` in a terminal, Ctrl-C at the line that advertises it:
    the shell sees 130, Claude Code never starts, and the probe is gone."""
    fakebin = tmp_path / "bin"
    fakebin.mkdir()
    curl_pidfile = tmp_path / "curl.pid"
    started = tmp_path / "claude.started"
    (fakebin / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$$\" > '" + str(curl_pidfile) + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    (fakebin / "claude").write_text(
        "#!/bin/sh\nprintf x >> '" + str(started) + "'\nexec sleep 300\n")
    (fakebin / "claude").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin], CCMORPH_REACH_BOUND="25", CCMORPH_REACH_PROGRESS="0.2")
    env["TERM"] = "xterm-256color"
    t = Terminal([str(ROOT / "bin/ccmorph"), "run", "gw"], env)
    try:
        t.read_until("Checking 'gw' gateway")
        curl_pid = wait_for_pidfile(curl_pidfile)
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 130, f"the shell sees Ctrl-C as 130, got {rc}"
        wait_gone(curl_pid)
        assert not started.exists(), "Claude Code started after the cancel"
        assert t.restored()
    finally:
        t.close()


def test_sigint_during_the_runs_retry_probe_kills_the_probe(tmp_path):
    """#1021 dials again after a connect timeout; the second dial is still a
    wait the progress line advertises, and its curl dies with the signal."""
    fakebin = tmp_path / "bin"
    fakebin.mkdir()
    counter, pidfile = tmp_path / "curl.count", tmp_path / "curl.pid"
    (fakebin / "curl").write_text(
        "#!/bin/sh\n"
        'n=$(cat \'' + str(counter) + '\' 2>/dev/null || echo 0)\nn=$((n+1))\n'
        "printf '%s' \"$n\" > '" + str(counter) + "'\n"
        'if [ "$n" -eq 1 ]; then exit 28; fi\n'
        "printf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin], CCMORPH_REACH_BOUND="25", CCMORPH_REACH_PROGRESS="0.2")
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        wait_for_line(proc, "Checking 'gw' gateway")
        child = wait_for_pidfile(pidfile)
        assert int(counter.read_text()) == 2, "the probe never dialed its second try"
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"Ctrl-C during the retry is the signal's exit, got {rc}"
        wait_gone(child)
    finally:
        proc.kill()
        proc.wait()


def test_sigint_during_adapter_readiness_stops_the_adapter(tmp_path):
    """The adapter wait says "Ctrl-C to cancel" before PORT=: a cancel must end
    the launch with the signal's status and leave no new, unrecorded adapter."""
    fakebin = tmp_path / "bin"
    fakebin.mkdir()
    adapter_pidfile = tmp_path / "adapter.pid"
    real_core = ROOT / "libexec" / "ccmorph-core"
    (fakebin / "ccmorph-core").write_text(
        "#!/bin/sh\n"
        'if [ "$1" = adapter ]; then\n'
        "  printf '%s\\n' \"$$\" > '" + str(adapter_pidfile) + "'\n"
        "  exec sleep 300\n"
        "fi\n"
        'exec "' + str(real_core) + '" "$@"\n')
    (fakebin / "ccmorph-core").chmod(0o755)
    (fakebin / "curl").write_text(
        "#!/bin/sh\n"
        "printf 'HTTP/1.1 200 OK\\r\\nContent-Type: application/json\\r\\n\\r\\n'\n"
        "printf '{\"data\":[{\"id\":\"claude-sonnet-5-5\"}]}'\n")
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin], _CCMORPH_LIBEXEC_DIR=str(fakebin))
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        wait_for_line(proc, "Ctrl-C to cancel")
        child = wait_for_pidfile(adapter_pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"a cancel at adapter readiness is the signal's exit, got {rc}"
        wait_gone(child)
    finally:
        proc.kill()
        proc.wait()


def test_sigint_on_the_next_pass_after_a_hop_stops_before_the_next_claude(tmp_path):
    """A hop relaunches in the same process. After the first Claude child exits
    its INT ownership is gone; the next pass's gateway wait must cancel, and
    the target Claude process must not start (GH-1028)."""
    fakebin = tmp_path / "bin"
    fakebin.mkdir()
    started = tmp_path / "claude.started"
    (fakebin / "claude").write_text(
        "#!/bin/sh\n"
        "printf x >> '" + str(started) + "'\n"
        "if [ -n \"$CCMORPH_RELAY_ID\" ]; then\n"
        "  printf 'profile=gw2\\nsession=sigint-hop\\n' > \"$CCMORPH_STATE_DIR/.reswitch.$CCMORPH_RELAY_ID\"\n"
        "fi\n"
        "exit 0\n")
    (fakebin / "claude").chmod(0o755)
    (fakebin / "curl").write_text(
        "#!/bin/sh\n"
        'case "$*" in *10.255.255.1*) ;; *) printf \'HTTP/1.1 200 OK\\r\\n\\r\\n\'; exit 0 ;; esac\n'
        "printf '%s\\n' \"$$\" > '" + str(tmp_path / "curl.pid") + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    env, state = launch_env(tmp_path, "type=apikey\napi_key=k", binenv=[fakebin],
                            CCMORPH_REACH_BOUND="25", CCMORPH_REACH_PROGRESS="0.2")
    (state / "profiles" / "gw2").mkdir()
    (state / "gw2.conf").write_text("type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k\n")
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        wait_for_line(proc, "Checking 'gw2' gateway")
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"the hop pass's wait must cancel, got {rc}"
        assert started.read_bytes().count(b"x") == 1, "the target Claude process started after the cancel"
    finally:
        proc.kill()
        proc.wait()


# --- A child may catch the signal and exit 0: that is still a cancel ---------

def test_a_bridge_that_survives_sigint_still_cancels_the_fresh_add(tmp_path):
    """The forwarded SIGINT is the user cancelling. A bridge that catches it and
    exits 0 must not turn that into "login finished": the staged credential is
    removed, no conf or default is saved, and the status is nonzero."""
    env, state, _ = add_home(tmp_path)
    fakebin, pidfile = install_bridge(
        tmp_path, state, staged=state / "auth" / "sigv" / "credential.json", survives_int=True)
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_add(env)
    try:
        proc.stdin.write("antigravity\n")
        proc.stdin.flush()
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        bridge_pid = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc != 0, "a cancelled sign-in returned success"
        wait_gone(bridge_pid)
        assert not (state / "auth" / "sigv").exists(), "the staged credential was kept"
        assert not (state / "sigv.conf").exists(), "a cancelled sign-in saved a profile"
        assert not (state / "active").exists(), "a cancelled add chose a default profile"
        assert not list(Path(env["TMPDIR"]).glob("ccmorph-login-*")), "the temporary config survived"
    finally:
        proc.kill()
        proc.wait()


def test_re_login_removes_only_the_credential_this_attempt_staged(tmp_path):
    """A cancelled re-login drops the file the attempt staged beside the stored
    one; the original credential and conf stay exactly as they were."""
    env, state, _ = add_home(tmp_path)
    (state / "profiles" / "gw").mkdir(parents=True)
    (state / "gw.conf").write_text("type=provider\nprovider=antigravity\n")
    (state / "auth" / "gw").mkdir(parents=True)
    stored = state / "auth" / "gw" / "stored.json"
    stored.write_text('{"token":"kept"}')
    conf_before = (state / "gw.conf").read_text()
    fakebin, pidfile = install_bridge(
        tmp_path, state, staged=state / "auth" / "gw" / "new.json", survives_int=True)
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_core(env, ["cli", "main", "login", "gw"])
    try:
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        bridge_pid = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc != 0
        wait_gone(bridge_pid)
        assert stored.read_text() == '{"token":"kept"}', "the stored credential changed"
        assert not (state / "auth" / "gw" / "new.json").exists(), "the cancelled attempt's credential stayed"
        assert (state / "gw.conf").read_text() == conf_before
    finally:
        proc.kill()
        proc.wait()


def test_re_login_with_an_empty_auth_dir_leaves_no_credential(tmp_path):
    env, state, _ = add_home(tmp_path)
    (state / "profiles" / "gw").mkdir(parents=True)
    (state / "gw.conf").write_text("type=provider\nprovider=antigravity\n")
    (state / "auth" / "gw").mkdir(parents=True)
    fakebin, pidfile = install_bridge(
        tmp_path, state, staged=state / "auth" / "gw" / "new.json", survives_int=True)
    env["PATH"] = os.pathsep.join([str(fakebin), env["PATH"]])
    proc = start_core(env, ["cli", "main", "login", "gw"])
    try:
        wait_for_line(proc, "Ctrl-C to cancel", stream="stdout")
        bridge_pid = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc != 0
        wait_gone(bridge_pid)
        assert not (state / "auth" / "gw" / "new.json").exists()
        assert not (state / "auth" / "gw").exists(), "an empty auth dir was left behind"
    finally:
        proc.kill()
        proc.wait()


@pytest.mark.parametrize("block", ["logout", "login", "status"], ids=str)
def test_a_claude_auth_child_that_survives_sigint_cancels_the_add(tmp_path, block):
    """The auth child can trap the signal and exit 0, and the status readback
    can print a valid email first. None of that is a completed sign-in: the
    add must stop at the cancelled wait and roll back (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    pidfile = tmp_path / "claude.pid"
    (tmp_path / "bin" / "claude").write_text(claude_script(pidfile, block, survives_int=True))
    (tmp_path / "bin" / "claude").chmod(0o755)
    proc = start_add(env)
    try:
        proc.stdin.write("1\n")
        proc.stdin.flush()
        child = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (1, 130), f"the cancelled {block} wait answers the failure path or the signal, got {rc}"
        wait_gone(child)
        assert not (state / "sigv.conf").exists(), "a cancelled add saved a profile"
        assert not (state / "profiles" / "sigv").exists(), "a cancelled add left its profile home"
    finally:
        proc.kill()
        proc.wait()


# --- ccmorph run: probe after headers, authenticated listing, and friends -----

def _run_claude_marker(tmp_path):
    fakebin = tmp_path / "bin"
    fakebin.mkdir(exist_ok=True)
    started = tmp_path / "claude.started"
    (fakebin / "claude").write_text(
        "#!/bin/sh\nprintf x >> '" + str(started) + "'\nexec sleep 300\n")
    (fakebin / "claude").chmod(0o755)
    return fakebin, started


def test_sigint_after_the_probe_headers_kills_the_probe_and_stops_the_launch(tmp_path):
    """curl has begun answering — headers are on stdout — but the transfer is
    blocked. Ctrl-C must end the launch (130), kill curl, and never go on to
    another probe, the adapter or Claude (GH-1028)."""
    fakebin, started = _run_claude_marker(tmp_path)
    pidfile = tmp_path / "curl.pid"
    (fakebin / "curl").write_text(
        "#!/bin/sh\nprintf 'HTTP/1.1 200 OK\\r\\n\\r\\n'\n"
        "printf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin], CCMORPH_REACH_BOUND="25", CCMORPH_REACH_PROGRESS="0.2")
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        wait_for_line(proc, "Checking 'gw' gateway")
        child = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"a cancel mid-transfer is the signal's exit, got {rc}"
        wait_gone(child)
        assert not started.exists(), "Claude Code started after the cancel"
    finally:
        proc.kill()
        proc.wait()


def test_sigint_during_the_native_listing_kills_the_listing_curl(tmp_path):
    """A native API-key profile asks Anthropic about the key before the
    session; that probe blocks and Ctrl-C must take it with the launch."""
    fakebin, started = _run_claude_marker(tmp_path)
    pidfile = tmp_path / "curl.pid"
    (fakebin / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\napi_key=k", binenv=[fakebin])
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        child = wait_for_pidfile(pidfile)
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"a cancel during the listing probe is the signal's exit, got {rc}"
        wait_gone(child)
        assert not started.exists(), "Claude Code started after the cancel"
    finally:
        proc.kill()
        proc.wait()


def test_sigint_during_the_gateway_listing_after_reach_kills_its_curl(tmp_path):
    """The reachability probe answers, then the authenticated listing blocks;
    the second curl is still a wait the launch owns and dies with Ctrl-C."""
    fakebin, started = _run_claude_marker(tmp_path)
    counter, pidfile = tmp_path / "curl.count", tmp_path / "curl.pid"
    (fakebin / "curl").write_text(
        "#!/bin/sh\n"
        'n=$(cat \'' + str(counter) + '\' 2>/dev/null || echo 0)\nn=$((n+1))\n'
        "printf '%s' \"$n\" > '" + str(counter) + "'\n"
        "if [ \"$n\" -eq 1 ]; then printf 'HTTP/1.1 200 OK\\r\\n\\r\\n'; exit 0; fi\n"
        "printf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin])
    proc = subprocess.Popen([str(CORE), "launch"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, preexec_fn=os.setsid)
    try:
        child = wait_for_pidfile(pidfile)
        assert int(counter.read_text()) == 2, "the listing never asked its own question"
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"a cancel during the listing is the signal's exit, got {rc}"
        wait_gone(child)
        assert not started.exists(), "Claude Code started after the cancel"
    finally:
        proc.kill()
        proc.wait()


# --- The re-prompts of the read loops ---------------------------------------

def test_sigint_at_the_wrong_shaped_token_reprompt_saves_nothing(tmp_path):
    env, state, _ = add_home(tmp_path)
    proc = start_add(env)
    try:
        proc.stdin.write("token\nnot-a-token\n")
        proc.stdin.flush()
        wait_for_line(proc, "not a Claude token's shape")
        rc = sigint_and_wait(proc)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value)
        assert not (state / "sigv.conf").exists()
    finally:
        proc.kill()
        proc.wait()


@pytest.mark.parametrize("flow", ["apikey", "token"], ids=str)
def test_sigint_at_the_rejected_credential_reprompt_saves_nothing(tmp_path, flow):
    env, state, _ = add_home(tmp_path)
    (tmp_path / "bin" / "curl").write_text(
        "#!/bin/sh\n"
        'case "$*" in *__CCMORPH_STATUS__*) '
        "printf 'HTTP/1.1 401 Unauthorized\\r\\n\\r\\n'; printf '\\n__CCMORPH_STATUS__401' ;; "
        "*) printf 'HTTP/1.1 401 Unauthorized\\r\\n\\r\\n' ;; esac\n")
    (tmp_path / "bin" / "curl").chmod(0o755)
    env["PATH"] = os.pathsep.join([str(tmp_path / "bin"), env["PATH"]])
    proc = start_add(env)
    try:
        if flow == "apikey":
            proc.stdin.write("apikey\ncustom\nhttps://10.255.255.1:8080\nsecret\n")
            proc.stdin.flush()
            wait_for_line(proc, "rejected the API key")
        else:
            proc.stdin.write("token\nsk-ant-oat01-ABCDEFGHIJKLMNOPQRSTUVWX\n")
            proc.stdin.flush()
            wait_for_line(proc, "Anthropic rejected the token")
        rc = sigint_and_wait(proc)  # the re-prompt is waiting with stdin open
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value)
        assert not (state / "sigv.conf").exists(), "a rejected credential was saved"
    finally:
        proc.kill()
        proc.wait()


def test_group_sigint_during_validation_cancels_even_when_curl_answers(tmp_path):
    """The foreground group's Ctrl-C also reaches curl; a curl that traps it
    and returns a normal 200 is still answering a cancelled wait, never a
    licence to save the key."""
    env, state, _ = add_home(tmp_path)
    pidfile = tmp_path / "curl.pid"
    (tmp_path / "bin" / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$$\" > '" + str(pidfile) + "'\n"
        "trap \"printf 'HTTP/1.1 200 OK\\r\\n\\r\\n'; exit 0\" INT\n"
        "sleep 300\n")
    (tmp_path / "bin" / "curl").chmod(0o755)
    env["PATH"] = os.pathsep.join([str(tmp_path / "bin"), env["PATH"]])
    proc = start_add(env)
    try:
        proc.stdin.write("apikey\ncustom\nhttps://10.255.255.1:8080\nsecret\n")
        proc.stdin.flush()
        child = wait_for_pidfile(pidfile)
        os.killpg(proc.pid, signal.SIGINT)
        rc = proc.wait(timeout=HANG_TIMEOUT)
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"a cancelled validation is the signal's exit, got {rc}"
        wait_gone(child)
        assert not (state / "sigv.conf").exists(), "the returned result was treated as a verdict"
    finally:
        proc.kill()
        proc.wait()


def test_ctrl_c_cancels_a_partial_numbered_provider_picker(tmp_path):
    """Typing part of a provider in the numbered provider picker is not an
    answer; Ctrl-C leaves through Cancelled without accepting the prefix."""
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x1b[B\x1b[B\r")  # API key
        t.read_until("Provider — sigv")
        t.write("antig")  # a prefix, never submitted
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1, f"the partial picker cancel answers 1, got {rc}"
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the numbered provider picker left the terminal raw"
        assert not (state / "sigv.conf").exists(), "a partial provider was accepted"
    finally:
        t.close()


def test_ctrl_c_cancels_a_partially_typed_hidden_token(tmp_path):
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x1b[B\r")  # Claude token
        t.read_until("Paste token for sigv")
        t.write("sk-ant-oat01-partial")
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1
        out = t.out.decode("utf-8", "replace")
        assert "Cancelled" in out
        assert "sk-ant-oat01-partial" not in out, "a hidden token was echoed"
        assert t.restored(), "the secret prompt left the terminal raw"
        assert not (state / "sigv.conf").exists(), "a cancelled token add saved setup"
    finally:
        t.close()


def test_sigint_only_to_the_core_while_fzf_owns_the_menu(tmp_path):
    """No terminal Ctrl-C: the signal goes to the core alone while fzf runs.
    The picker still owns it: fzf is stopped, Cancelled is printed, and the
    terminal comes back (GH-1028)."""
    fakebin = tmp_path / "fzfbin"
    fakebin.mkdir()
    pidfile = tmp_path / "fzf.pid"
    (fakebin / "fzf").write_text(
        "#!/bin/sh\n"
        'if [ "$1" = --version ]; then echo 0.71.0; exit 0; fi\n'
        "printf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "fzf").chmod(0o755)
    env, state = terminal_env(tmp_path, extra_bins=[fakebin])
    t = Terminal([str(CORE), "cli", "main", "add", "sigv"], env)
    try:
        t.read_until("Sign-in method")
        t.write(b"\x1b[B\x1b[B\r")
        fzf_pid = wait_for_pidfile(pidfile)
        t.proc.send_signal(signal.SIGINT)
        rc = t.wait()
        assert rc == 1, f"a core-only cancel at fzf answers 1, got {rc}"
        wait_gone(fzf_pid)
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the core-only signal left the terminal raw"
        assert not (state / "sigv.conf").exists()
    finally:
        t.close()


def test_sigint_cancels_a_prompt_in_an_incomplete_escape_sequence(tmp_path):
    """The raw reader waits 80 ms for the rest of an escape sequence; SIGINT in
    that window still cancels, restores, and saves nothing."""
    env, state = terminal_env(tmp_path)
    t = Terminal([str(CORE), "cli", "main", "add"], env)
    try:
        t.read_until("New profile name")
        t.write("sigv")
        t.write(b"\x1b")  # an introducer whose tail never comes
        t.proc.send_signal(signal.SIGINT)
        rc = t.wait()
        assert rc == 1
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "a signal inside the escape window left the terminal raw"
        assert not (state / "sigv.conf").exists()
    finally:
        t.close()


def test_sigint_to_the_public_login_pid_reaches_the_locked_auth_child(tmp_path):
    """`ccmorph login` for a Claude-account profile runs a lock wrapper between
    the public command and the auth child. A SIGINT to the public pid alone must
    reach the child, release the lock, and leave the profile and email intact."""
    env, state, _ = add_home(tmp_path)
    (state / "profiles" / "gw").mkdir(parents=True)
    (state / "gw.conf").write_text("type=sub\nemail=old@example.com\n")
    pidfile = tmp_path / "claude.pid"
    (tmp_path / "bin" / "claude").write_text(
        "#!/bin/sh\n"
        'case "$1 $2" in\n'
        '"auth status") printf \'{"loggedIn": true, "email": "old@example.com"}\\n\' ;;\n'
        f'"auth login") echo $$ > \'{pidfile}\'; exec sleep 300 ;;\n'
        "esac\n"
        "exit 0\n")
    (tmp_path / "bin" / "claude").chmod(0o755)
    proc = start_core(env, ["cli", "main", "login", "gw"])
    try:
        child = wait_for_pidfile(pidfile)
        proc.send_signal(signal.SIGINT)  # the public command's own pid, alone
        rc = proc.wait(timeout=HANG_TIMEOUT)
        assert rc != 0, "a cancelled login returned success"
        wait_gone(child)
        assert (state / "gw.conf").read_text() == "type=sub\nemail=old@example.com\n", \
            "the cancelled login rewrote the profile"
        lock = state / ".locks" / "gw.lock"
        if lock.exists():
            with open(lock, "a+") as f:
                fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)  # raises if still held
                fcntl.flock(f, fcntl.LOCK_UN)
    finally:
        proc.kill()
        proc.wait()


def test_ctrl_c_cancels_the_sourced_launch_at_the_gateway_line(tmp_path):
    """The sourced entry point `source lib/ccmorph.sh; _ccm_launch -a gw` is
    another caller of the launch; Ctrl-C at its progress line must leave with
    status 130, stop the probe, and start no Claude."""
    fakebin, started = _run_claude_marker(tmp_path)
    pidfile = tmp_path / "curl.pid"
    (fakebin / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$$\" > '" + str(pidfile) + "'\nexec sleep 300\n")
    (fakebin / "curl").chmod(0o755)
    env, _ = launch_env(tmp_path, "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k",
                        binenv=[fakebin], CCMORPH_REACH_BOUND="25", CCMORPH_REACH_PROGRESS="0.2")
    env["TERM"] = "xterm-256color"
    command = 'source "$1"\n_ccm_launch -a gw'
    t = Terminal(["zsh", "-fc", command, "ccmorph-sourced-test", str(ROOT / "lib/ccmorph.sh")], env)
    try:
        t.read_until("Checking 'gw' gateway")
        child = wait_for_pidfile(pidfile)
        t.write(b"\x03")
        rc = t.wait()
        assert rc in (-int(signal.SIGINT.value), 128 + signal.SIGINT.value), \
            f"the sourced launch sees Ctrl-C as the shell's 130, got {rc}"
        wait_gone(child)
        assert not started.exists(), "Claude Code started after the cancel"
        assert t.restored()
    finally:
        t.close()


def test_ctrl_c_cancels_the_model_picker_before_claude(tmp_path):
    """The model picker opens with a profile already chosen. The picker owns
    Ctrl-C: it prints Cancelled, restores the terminal, starts no Claude, and
    leaves the saved model alone (GH-1028)."""
    env, state, _ = add_home(tmp_path)
    (state / "profiles" / "gw").mkdir(parents=True)
    (state / "gw.conf").write_text("type=sub\nemail=a@example.com\n")
    before = (state / "gw.conf").read_text()
    env.update(TERM="dumb")
    t = Terminal([str(CORE), "cli", "_menu", "gw"], env)
    try:
        t.read_until("Model — gw")
        t.write(b"\x03")
        rc = t.wait()
        assert rc == 1, f"a cancelled model picker answers 1, got {rc}"
        assert "Cancelled" in t.out.decode("utf-8", "replace")
        assert t.restored(), "the model picker left the terminal raw"
        assert (state / "gw.conf").read_text() == before, "the cancelled picker changed the saved model"
    finally:
        t.close()
