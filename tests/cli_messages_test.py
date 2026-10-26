"""Human output stays concise without changing machine-readable command output."""
import os
from pathlib import Path
import subprocess
import tempfile

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def test_probe_help_has_no_progress_banner_and_remains_actionable():
    with tempfile.TemporaryDirectory() as tmp:
        env = {**os.environ, 'HOME': tmp, 'XDG_CONFIG_HOME': str(Path(tmp) / '.config'),
               'CCPROTEUS_LIB_DIR': str(ROOT), 'CCPROTEUS_STATE_DIR': str(Path(tmp) / 'state')}
        result = subprocess.run([str(ROOT / 'ccproteus'), 'probe', '--help'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert 'Refreshing' not in result.stdout
        assert 'inference probe' not in result.stdout
        assert '--allow-inference' in result.stdout


def test_adapter_decline_surfaces_actionable_reason():
    with tempfile.TemporaryDirectory() as tmp:
        lib = Path(tmp) / 'lib'
        lib.mkdir()
        for f in ROOT.glob('ccproteus-*.zsh'):
            (lib / f.name).write_bytes(f.read_bytes())
        fake = lib / 'ccproteus-adapter'
        fake.write_text('#!/bin/sh\nprintf "EXIT=set a model and retry\\n"\n')
        fake.chmod(0o755)
        env = {**os.environ, 'HOME': tmp, 'TMPDIR': tmp,
               'CCPROTEUS_LIB_DIR': str(lib)}
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccs_adapter_start demo',
                                 'test', str(ROOT / 'ccproteus.sh')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'set a model and retry' in result.stderr


def test_adapter_failure_reports_observation_not_guessed_cause():
    with tempfile.TemporaryDirectory() as tmp:
        lib = Path(tmp) / 'lib'
        lib.mkdir()
        for f in ROOT.glob('ccproteus-*.zsh'):
            (lib / f.name).write_bytes(f.read_bytes())
        fake = lib / 'ccproteus-adapter'
        fake.write_text('#!/bin/sh\nexit 1\n')
        fake.chmod(0o755)
        env = {**os.environ, 'HOME': tmp, 'CCPROTEUS_LIB_DIR': str(lib)}
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccs_adapter_start demo known',
                                 'test', str(ROOT / 'ccproteus.sh')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'adapter exited before readiness' in result.stderr
        assert 'bug or a broken install' not in result.stderr
        assert 'Usually the gateway refused' not in result.stderr
        assert 'ccproteus inspect demo' in result.stderr


def test_help_is_concise_truthful_and_read_only(tmp_path):
    state = tmp_path / 'state'
    state.mkdir()
    marker = tmp_path / 'host-hook-ran'
    (state / 'local.sh').write_text(f'print ran > "{marker}"\n')
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCPROTEUS_LIB_DIR': str(ROOT), 'CCPROTEUS_STATE_DIR': str(state)}
    cli = str(ROOT / 'ccproteus')
    for args in ([], ['--help'], ['help', 'config'], ['add', '--help'],
                 ['doctor', '--help'], ['--version']):
        result = subprocess.run([cli, *args], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert not marker.exists(), 'help must not source executable host overrides'
        assert not (state / 'profiles').exists(), 'help must not provision state'
        assert result.stderr == ''
    result = subprocess.run([cli, '--help'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert len(result.stdout.splitlines()) <= 28
    assert 'ccproteus run -i' in result.stdout
    assert 'ccproteus help <command>' in result.stdout
    for advanced in ('--filtered-json', '--allow-inference', 'oauth_token', 'ccproteus cred', 'ccproteus context'):
        assert advanced not in result.stdout
    result = subprocess.run([cli, 'help', 'all'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    for verb in ('run', 'add', 'list', 'use', 'config', 'inspect', 'doctor', 'models',
                 'get', 'set', 'login', 'mv', 'rm', 'hop', 'cred', 'probe', 'context'):
        assert 'ccproteus ' + verb in result.stdout
        detail = subprocess.run([cli, 'help', verb], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert detail.returncode == 0, detail.stderr
        assert detail.stdout.startswith('Usage: ccproteus ' + verb)
        assert '[-a <profile>]' not in detail.stdout
    result = subprocess.run([cli, 'help', 'typo'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 2
    assert 'ccproteus help all' in result.stderr
    assert not marker.exists()
