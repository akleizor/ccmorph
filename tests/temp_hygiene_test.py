#!/usr/bin/env python3
"""Temp state is removed by whatever made it (GH-196).

Every temp folder a suite run made used to stay in TMPDIR for good (about
3,800 were found across repos on 2026-11-05). Each witness here runs the real
maker — the demo sandbox, a slice of the suite — and asserts the folder is
gone once the run has ended, which is what a full run's temp dir says.
"""

from __future__ import annotations

import os
import socket
import subprocess
import sys
from pathlib import Path

import pytest

from support import HANG_TIMEOUT, needs_shim

REPO = Path(__file__).resolve().parent.parent


def _can_bind_loopback() -> bool:
    """The suite witnesses run tests that bind loopback servers; a runner that
    refuses the bind (a sandbox) cannot run them. The landing gate runs this
    file outside any sandbox."""
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", 0))
    except OSError:
        return False
    s.close()
    return True


@pytest.mark.parametrize(("ending", "expected_rc"), [
    # The tape's shell ends normally when the recording does; a recording
    # killed mid-tape ends it with a signal instead.
    pytest.param("", 0, id="natural-end-issue-196"),
    pytest.param("kill -TERM $$", 143, id="term-killed-issue-196"),
])
def test_the_demo_sandbox_home_dies_with_the_shell(ending, expected_rc, tmp_path):
    """assets/demo/setup.sh builds a throwaway HOME — fresh state dir, fake
    profiles, a scripted `claude` — and the shell that sources it owns putting
    it away, however it ends. A bare `mktemp -d` lands where the platform puts
    it (the user's temp dir on macOS, $TMPDIR on Linux), so the witness reads
    the sandbox's own path back and checks the folder, not one directory."""
    recorded = tmp_path / "sandbox-home"
    env = dict(os.environ, CCMORPH_TEST_SANDBOX_OUT=str(recorded))
    script = ("source assets/demo/setup.sh\n"
              'printf %s "$HOME" > "$CCMORPH_TEST_SANDBOX_OUT"\n'
              'test -s "$CCMORPH_TEST_SANDBOX_OUT" || exit 9\n') + ending
    proc = subprocess.run(["bash", "-c", script], cwd=REPO, env=env,
                          capture_output=True, text=True, timeout=HANG_TIMEOUT)
    # 9 means the sandbox was never built, so the check below would pass
    # vacuously. 0 is the natural ending; 143 is the TERM one, where an
    # untrapped shell instead dies of the signal (the parent sees -15).
    assert proc.returncode == expected_rc, proc.stderr[-500:]
    home = Path(recorded.read_text())
    inherited = env.get("HOME")
    assert not inherited or home != Path(inherited), \
        "setup.sh did not build its own HOME"
    assert not home.exists(), f"the demo sandbox outlived its shell: {home}"


@needs_shim
@pytest.mark.parametrize(("file", "one_test"), [
    # One test from each file the issue names as leaking: both leaked per
    # test — a fresh HOME per AdapterTest.setUp, a fresh XDG_STATE_HOME per
    # GatewayEdgeTest request — so one run of one test shows the leak and
    # its fix.
    pytest.param("tests/adapter_test.py",
                 "test_no_x_forwarded_for_header_is_ever_added",
                 id="adapter-home-issue-196"),
    pytest.param("tests/gateway_shim_test.py",
                 "test_non_claude_model_is_left_alone",
                 id="gateway-edge-state-issue-196"),
])
def test_a_suite_run_removes_the_temp_state_it_makes(file, one_test, tmp_path):
    """The witness run happens inside a private TMPDIR — Python's tempfile
    honours the variable on every platform — and must leave it exactly as it
    found it."""
    if not _can_bind_loopback():
        pytest.skip("this runner refuses loopback binds; the landing gate "
                    "runs this witness outside any sandbox")
    scratch = tmp_path / "tmpdir"
    scratch.mkdir()
    env = dict(os.environ, TMPDIR=str(scratch))
    env.pop("PYTEST_ADDOPTS", None)  # the witness runs its own argv
    argv = [sys.executable, "-m", "pytest", file, "-q", "-k", one_test,
            "-p", "no:cacheprovider", "-p", "no:xdist"]
    proc = subprocess.run(argv, cwd=REPO, env=env, capture_output=True,
                          text=True, timeout=HANG_TIMEOUT)
    # A witness that errored or was skipped everywhere must not pass silently.
    assert proc.returncode == 0, proc.stdout[-2000:] + proc.stderr[-2000:]
    left = sorted(p.name for p in scratch.iterdir())
    assert left == [], f"{file} left temp state behind: {left}"
