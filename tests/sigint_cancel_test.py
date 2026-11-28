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
import os
import signal
import subprocess
import threading
import time
from pathlib import Path

import pytest

from support import CORE, HANG_TIMEOUT, needs_shim

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


def wait_for_line(proc, needle, timeout=HANG_TIMEOUT, stream="stderr"):
    """True once `needle` has passed on the named stream; the prompts and the
    sign-in notes print whole lines, so a line read is the deterministic point
    the flow has reached."""
    seen, done = [], threading.Event()

    def pump():
        for line in iter(getattr(proc, stream).readline, ""):
            seen.append(line)
            if needle in line:
                done.set()
                return

    threading.Thread(target=pump, daemon=True).start()
    reached = done.wait(timeout)
    if not reached:
        proc.kill()
        pytest.fail(f"'{needle}' never reached {stream}:\n{''.join(seen)}")


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
        assert rc == 1, f"the cancel path should end the add with rc 1, got {rc}"
        assert "login failed" in proc.stderr.read(), "the wait must end through providerLogin's failure path"
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
