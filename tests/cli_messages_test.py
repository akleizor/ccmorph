"""Human output stays concise without changing machine-readable command output."""
import os
import shutil
from pathlib import Path
import subprocess
import tempfile

import pytest

from support import HANG_TIMEOUT
from fake_adapter import CORE_DISPATCH

ROOT = Path(__file__).resolve().parents[1]


@pytest.mark.parametrize('argv', [
    ['probe', '--help'], ['probe', 'gw', '--help'], ['probe', '-i', '-h'],
    ['mea' + 'sure', '--help'],
], ids=lambda a: ' '.join(a))
def test_probe_help_has_no_progress_banner_and_remains_actionable(argv):
    with tempfile.TemporaryDirectory() as tmp:
        env = {**os.environ, 'HOME': tmp, 'XDG_CONFIG_HOME': str(Path(tmp) / '.config'),
               'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(Path(tmp) / 'state')}
        result = subprocess.run([str(ROOT / "bin/ccmorph"), *argv], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert result.stdout.startswith('usage: ccmorph probe [profile] [options]'), result.stdout
        assert 'Refreshing' not in result.stdout
        assert 'inference probe' not in result.stdout
        assert '--allow-inference' in result.stdout


def test_probe_refuses_an_unknown_option_with_its_usage():
    with tempfile.TemporaryDirectory() as tmp:
        env = {**os.environ, 'HOME': tmp, 'XDG_CONFIG_HOME': str(Path(tmp) / '.config'),
               'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(Path(tmp) / 'state')}
        result = subprocess.run([str(ROOT / "bin/ccmorph"), 'probe', '--bogus'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 2, result.stderr
        assert result.stderr.startswith('usage: ccmorph probe [profile] [options]'), result.stderr
        assert 'unrecognized arguments: --bogus' in result.stderr


def test_adapter_decline_surfaces_actionable_reason():
    with tempfile.TemporaryDirectory() as tmp:
        lib = Path(tmp) / 'lib'
        lib.mkdir()
        (lib.parent / 'libexec').mkdir()
        for f in (ROOT / 'lib').glob('ccmorph-*.zsh'):
            (lib / f.name).write_bytes(f.read_bytes())
        fake = lib.parent / 'libexec/ccmorph-core'
        fake.write_text('#!/bin/sh\n' + CORE_DISPATCH + 'printf "EXIT=set a model and retry\\n"\n')
        fake.chmod(0o755)
        env = {**os.environ, 'HOME': tmp, 'TMPDIR': tmp,
               'CCMORPH_LIB_DIR': str(lib)}
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_core_env; '
                                 'exec env "${reply[@]}" "$2" launch --adapter-start demo',
                                 'test', str(ROOT / 'lib/ccmorph.sh'), str(ROOT / 'libexec/ccmorph-core')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'set a model and retry' in result.stderr


def test_adapter_failure_reports_observation_not_guessed_cause():
    with tempfile.TemporaryDirectory() as tmp:
        lib = Path(tmp) / 'lib'
        lib.mkdir()
        (lib.parent / 'libexec').mkdir()
        for f in (ROOT / 'lib').glob('ccmorph-*.zsh'):
            (lib / f.name).write_bytes(f.read_bytes())
        fake = lib.parent / 'libexec/ccmorph-core'
        fake.write_text('#!/bin/sh\n' + CORE_DISPATCH + 'exit 1\n')
        fake.chmod(0o755)
        env = {**os.environ, 'HOME': tmp, 'CCMORPH_LIB_DIR': str(lib)}
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_core_env; '
                                 'exec env "${reply[@]}" "$2" launch --adapter-start demo known',
                                 'test', str(ROOT / 'lib/ccmorph.sh'), str(ROOT / 'libexec/ccmorph-core')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'adapter exited before readiness' in result.stderr
        assert 'bug or a broken install' not in result.stderr
        assert 'Usually the gateway refused' not in result.stderr
        assert 'ccmorph doctor demo' in result.stderr


def test_help_is_concise_truthful_and_read_only(tmp_path):
    state = tmp_path / 'state'
    state.mkdir()
    marker = tmp_path / 'host-hook-ran'
    (state / 'local.sh').write_text(f'print ran > "{marker}"\n')
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(state)}
    cli = str(ROOT / "bin/ccmorph")
    for args in ([], ['--help'], ['help', 'config'], ['add', '--help'],
                 ['doctor', '--help'], ['update', '--help'], ['uninstall', '--help'],
                 ['help', 'update'], ['--version'],
                 ['rm', 'gw', '--help'], ['cred', 'gw', '-h'], ['use', 'gw', '--help'],
                 ['run', '--help'], ['probe', 'gw', '--help']):
        result = subprocess.run([cli, *args], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert not marker.exists(), 'help must not source executable host overrides'
        assert not (state / 'profiles').exists(), 'help must not provision state'
        assert result.stderr == ''
    result = subprocess.run([cli, '--help'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert len(result.stdout.splitlines()) <= 28
    assert 'ccmorph run -i' in result.stdout
    assert 'ccmorph help <command>' in result.stdout
    for advanced in ('--filtered-json', '--allow-inference', 'oauth_token', 'ccmorph cred', 'ccmorph context',
                     'ccmorph update', 'ccmorph uninstall'):
        assert advanced not in result.stdout
    result = subprocess.run([cli, 'help', 'all'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    for verb in ('run', 'add', 'list', 'use', 'config', 'inspect', 'doctor', 'models',
                 'get', 'set', 'login', 'mv', 'rm', 'hop', 'cred', 'probe', 'context',
                 'update', 'uninstall', 'quota'):
        assert 'ccmorph ' + verb in result.stdout
        detail = subprocess.run([cli, 'help', verb], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert detail.returncode == 0, detail.stderr
        assert detail.stdout.startswith('Usage: ccmorph ' + verb)
        assert '[-a <profile>]' not in detail.stdout
    result = subprocess.run([cli, 'help', 'quota'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    # One line names which profile kinds show plan usage (#270), so a token
    # profile's row is expected to explain rather than show windows.
    flat = ' '.join(result.stdout.split())
    assert 'Plan usage is read for Claude account and Codex profiles' in flat
    assert 'a pasted Claude token cannot read it' in flat
    result = subprocess.run([cli, 'help', 'typo'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 2
    assert 'ccmorph help all' in result.stderr
    assert not marker.exists()


def test_update_and_uninstall_under_homebrew_name_the_brew_command(tmp_path):
    # A brew install keeps the library under the Cellar with no installer beside it.
    root = tmp_path / 'Cellar/ccmorph/0.2.0/libexec'
    (root / 'lib').mkdir(parents=True)
    for f in (ROOT / 'lib').iterdir():
        (root / 'lib' / f.name).write_bytes(f.read_bytes())
    # A brew install ships the core beside the library, as every install does.
    (root / 'libexec').mkdir()
    shutil.copy2(ROOT / 'libexec/ccmorph-core', root / 'libexec/ccmorph-core')
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(root / 'lib'), 'CCMORPH_STATE_DIR': str(tmp_path / 'state')}
    for verb, brew in (('update', 'brew upgrade ccmorph'), ('uninstall', 'brew uninstall ccmorph')):
        result = subprocess.run([str(ROOT / 'bin/ccmorph'), verb], env=env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert brew in result.stderr, result.stderr


def _help_add(tmp_path, bridge):
    """help add with a PATH that has only a fake cliproxyapi, when bridge is given."""
    bin_dir = tmp_path / 'bin'
    bin_dir.mkdir(exist_ok=True)
    if bridge:
        fake = bin_dir / 'cliproxyapi'
        fake.write_text('#!/bin/sh\n'
                        'if [ "$1" = --help ]; then for p in claude antigravity codex; do'
                        ' echo "  -$p-login"; done; exit 0; fi\n'
                        'exit 1\n')
        fake.chmod(0o755)
    env = {**os.environ, 'HOME': str(tmp_path / 'home'), 'PATH': f'{bin_dir}:/usr/bin:/bin',
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(tmp_path / 'state')}
    result = subprocess.run([str(ROOT / "bin/ccmorph"), 'help', 'add'], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, result.stderr
    return result.stdout + result.stderr


def test_add_help_names_provider_logins_only_when_the_bridge_is_installed(tmp_path):
    without = _help_add(tmp_path, bridge=False)
    assert 'cliproxyapi' not in without
    assert 'Provider' not in without
    with_bridge = _help_add(tmp_path, bridge=True)
    assert 'Antigravity, Codex: sign in to that service with your browser (through cliproxyapi).' in with_bridge
    assert 'Claude: sign in to that service' not in with_bridge
