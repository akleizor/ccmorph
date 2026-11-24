"""rm takes a pre-#373 name through its lock, as a person runs it.

GH-551: the Go tests set CCMORPH_CONTEXT_LOCKED to test the removal itself, but
the normal command re-execs under `model-window --locked <name>` first, and that
key was validated at ValidName: a legacy name was refused before the widened
removal rule could run, and a legacy default's `active` was never cleared. These
run the real core, with no lock bypass.
"""
import os
import subprocess
import tempfile
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT / 'libexec' / 'ccmorph-core'


def _state_and_env(tmp):
    """An isolated state dir and env, with a no-op keychain for macOS's rm."""
    home, state = Path(tmp) / 'home', Path(tmp) / 'state'
    home.mkdir()
    state.mkdir()
    stub = Path(tmp) / 'security'
    stub.write_text('#!/bin/sh\nexit 1\n')
    stub.chmod(0o700)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(('CCMORPH_', '_CCMORPH_', 'MODEL_WINDOW_'))}
    env.update(HOME=str(home), CCMORPH_STATE_DIR=str(state),
               PATH=str(Path(tmp)) + os.pathsep + env.get('PATH', ''))
    return state, env


def _rm(env, name):
    return subprocess.run([str(CORE), 'cli', 'rm', name], input='y\n', text=True,
                          env=env, capture_output=True, timeout=HANG_TIMEOUT)


def test_rm_removes_a_legacy_name_through_its_lock_gh_551():
    with tempfile.TemporaryDirectory() as tmp:
        state, env = _state_and_env(tmp)
        for length in (201, 240, 241, 245, 250):
            name = 'n' * length
            conf = state / (name + '.conf')
            conf.write_text('type=apikey\napi_key=k\n')
            result = _rm(env, name)
            assert result.returncode == 0, (length, result.stderr)
            assert not conf.exists(), (length, result.stderr)


def test_rm_clears_a_legacy_default_through_its_lock_gh_551():
    with tempfile.TemporaryDirectory() as tmp:
        state, env = _state_and_env(tmp)
        name = 'w' * 201
        conf, active = state / (name + '.conf'), state / 'active'
        conf.write_text('type=apikey\napi_key=k\n')
        active.write_text(name + '\n')
        result = _rm(env, name)
        assert result.returncode == 0, result.stderr
        assert not conf.exists(), result.stderr
        assert not active.exists(), result.stderr