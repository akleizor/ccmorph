"""Adapter startup: the wire model id it is handed, and the wait it must not add."""
import os
from pathlib import Path
import subprocess
import tempfile
import time

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def _adapter_start_run(home, selected, delay):
    """Start a fake adapter through the real shell helper; build state, never assert."""
    lib = Path(home) / 'lib'
    lib.mkdir()
    (lib.parent / 'libexec').mkdir()
    for f in (ROOT / 'lib').glob('ccmorph-*.zsh'):
        (lib / f.name).write_bytes(f.read_bytes())
    log = Path(home) / 'args'
    adapter = lib.parent / 'libexec/ccmorph-adapter'
    adapter.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARG_LOG"\n'
                       f'sleep {delay}\nprintf "PORT=12345\\n"\n')
    adapter.chmod(0o755)
    env = {**os.environ, 'HOME': home, 'CCMORPH_LIB_DIR': str(lib), 'ARG_LOG': str(log)}
    script = 'source "$1"; _ccm_adapter_start gw "$2"'
    start = time.monotonic()
    result = subprocess.run(['zsh', '-c', script, 'test', str(ROOT / 'lib/ccmorph.sh'), selected],
                            env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    # A launch that failed before the adapter ran leaves no argv log; reading it
    # unconditionally would mask the launcher's own message with FileNotFound,
    # so hand back empty argv and let the caller's returncode assertion speak.
    argv = log.read_text() if log.exists() else ''
    return result, time.monotonic() - start, argv


@pytest.mark.parametrize(('selected', 'wire'), [
    pytest.param('gpt-6-astra', 'gpt-6-astra', id='bare-id'),
    pytest.param('claude-sonnet-5-5[1m]', 'claude-sonnet-5-5',
                 id='window-marker-stripped-issue-232'),
])
def test_adapter_receives_the_wire_model_id(selected, wire):
    # The [1m] window marker is ccmorph's own tag: the shell strips it before
    # handing the adapter the model to prepare (#232).
    with tempfile.TemporaryDirectory() as tmp:
        result, _, argv = _adapter_start_run(tmp, selected, 0)
        assert result.returncode == 0, result.stderr
        assert f'--model\n{wire}\n' in argv


def test_adapter_start_reports_the_wait():
    with tempfile.TemporaryDirectory() as tmp:
        result, elapsed, _ = _adapter_start_run(tmp, 'gpt-6-astra', 2.2)
        assert result.returncode == 0, result.stderr
        assert elapsed < 6
        assert 'Starting' in result.stderr and 'gw' in result.stderr


def test_adapter_alias_uses_profile_settings_before_startup():
    import json
    with tempfile.TemporaryDirectory() as tmp:
        home = Path(tmp)
        state = home / 'state'
        state.mkdir()
        (state / 'gw.conf').write_text('type=apikey\napi_key=synthetic\n')
        settings = state / 'profiles/gw/settings.json'
        settings.parent.mkdir(parents=True)
        settings.write_text(json.dumps({'env': {'ANTHROPIC_DEFAULT_OPUS_MODEL': 'gpt-6-astra'}}))
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_settings_model_alias gw opus',
                                 'test', str(ROOT / 'lib/ccmorph.sh')],
                                env={**os.environ, 'HOME': tmp, 'CCMORPH_STATE_DIR': str(state)}, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert result.stdout.strip() == 'gpt-6-astra'
