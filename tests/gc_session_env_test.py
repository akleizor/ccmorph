"""A concurrent launch must not reap an env file before its helper can read it."""

import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

import pytest

from support import HANG_TIMEOUT


CORE = Path(__file__).resolve().parents[1] / "libexec" / "ccmorph-core"


@pytest.fixture
def sweep_dirs(tmp_path):
    if not CORE.is_file():
        pytest.skip("needs the built core: make shim")
    state = tmp_path / "state[gc"
    run = state / "run"
    tmp = tmp_path / "tmp"
    run.mkdir(parents=True)
    tmp.mkdir()
    # Keep the operator's launch overrides and claude off both processes' paths.
    env = {"HOME": str(tmp_path), "TMPDIR": str(tmp), "_CCMORPH_DIR": str(state),
           "PATH": os.defpath}
    return state, run, tmp, env


def aged(path, data=b""):
    path.touch(mode=0o600)
    path.write_bytes(data)
    past = time.time() - 120
    os.utime(path, (past, past))
    return path


def run_core(env, *args, prefix=()):
    result = subprocess.run([*prefix, CORE, *args], env=env, timeout=HANG_TIMEOUT,
                            text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    return result


@pytest.mark.parametrize("fallback", [False, True], ids=["tmpdir", "state-run"])
def test_delayed_session_env_survives_concurrent_sweep(sweep_dirs, tmp_path, fallback):
    state, run, tmp, env = sweep_dirs
    pid = os.getpid()
    live = aged((run if fallback else tmp) / f"ccmorph-session-env.{pid}.123", b"CANARY=ready\0")
    # Reap the child before asking the sweep to check its pid.
    with subprocess.Popen(["/bin/true"]) as child:
        dead_pid = child.pid
        assert child.wait(timeout=HANG_TIMEOUT) == 0
    stale = [aged(d / f"ccmorph-session-env.{dead_pid}.456") for d in (tmp, run)]
    stale += [aged(tmp / "ccmorph-session-env.123456"), aged(run / "ccmorph-session-env.abc")]
    fresh = tmp / f"ccmorph-session-env.{dead_pid}.789"
    fresh.touch()
    # The same sweep handles relays even under a state root containing '['.
    # The ancient launch clock makes this live PID a recycled launcher.
    stale += [aged(state / f".reswitch.{pid}.1_000000.123"),
              aged(state / f".reswitch.{dead_pid}.legacy.123")]
    retained = [fresh, aged(state / f".reswitch.{pid}.legacy.123"),
                aged(state / ".reswitch.unknown.1_000000.123")]

    bindir = tmp_path / "bin"
    bindir.mkdir()
    claude = bindir / "claude"
    claude.write_text('#!/bin/sh\nprintf "%s\\n" "$CANARY"\n')
    claude.chmod(0o700)
    env["PATH"] = f"{bindir}{os.pathsep}{env['PATH']}"

    run_core(env, "launch", "--cleanup-stale")
    result = run_core(env, "util", "claude-session", "", "", "", "", live, "--")
    assert result.stdout == "ready\n"
    assert not live.exists(), f"consumer left {live.name} behind"
    for path in stale:
        assert not path.exists(), f"stale record survived: {path}"
    for path in retained:
        assert path.exists(), f"live, unknown, or fresh record was reaped: {path}"


@pytest.mark.skipif(sys.platform != "linux", reason="strace fault injection needs Linux")
def test_gc_preserves_records_when_kill_is_denied(sweep_dirs):
    strace = shutil.which("strace")
    if strace is None:
        pytest.skip("needs strace to make kill(pid, 0) return EPERM")
    state, run, tmp, env = sweep_dirs
    pid = os.getpid()  # A real, live holder throughout the sweep.
    records = [aged(d / f"ccmorph-session-env.{pid}.123") for d in (tmp, run)]
    records += [aged(state / f".reswitch.{pid}.{stamp}.123")
                for stamp in ("legacy", "1_000000")]
    # Fail only the sweeper's probe. No signal is sent to the holder.
    result = run_core(env, "launch", "--cleanup-stale", prefix=(
        strace, "-f", "-qq", "-e", "trace=kill", "-e", "inject=kill:error=EPERM",
    ))
    assert any(f"kill({pid}, 0)" in line and "EPERM" in line and "(INJECTED)" in line
               for line in result.stderr.splitlines()), result.stderr
    missing = [str(path.relative_to(state.parent)) for path in records if not path.exists()]
    assert not missing, f"kill returned EPERM for a live holder, but GC reaped {missing}"
