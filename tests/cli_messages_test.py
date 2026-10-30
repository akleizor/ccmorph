"""Human output stays concise without changing machine-readable command output."""
import os
from pathlib import Path
import subprocess
import tempfile

from support import HANG_TIMEOUT
from fake_adapter import CORE_DISPATCH

ROOT = Path(__file__).resolve().parents[1]


def test_probe_help_has_no_progress_banner_and_remains_actionable():
    with tempfile.TemporaryDirectory() as tmp:
        env = {**os.environ, 'HOME': tmp, 'XDG_CONFIG_HOME': str(Path(tmp) / '.config'),
               'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(Path(tmp) / 'state')}
        result = subprocess.run([str(ROOT / "bin/ccmorph"), 'probe', '--help'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert 'Refreshing' not in result.stdout
        assert 'inference probe' not in result.stdout
        assert '--allow-inference' in result.stdout


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
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_adapter_start demo',
                                 'test', str(ROOT / 'lib/ccmorph.sh')], env=env,
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
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_adapter_start demo known',
                                 'test', str(ROOT / 'lib/ccmorph.sh')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'adapter exited before readiness' in result.stderr
        assert 'bug or a broken install' not in result.stderr
        assert 'Usually the gateway refused' not in result.stderr
        assert 'ccmorph inspect demo' in result.stderr


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
                 ['help', 'update'], ['--version']):
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
                 'update', 'uninstall'):
        assert 'ccmorph ' + verb in result.stdout
        detail = subprocess.run([cli, 'help', verb], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert detail.returncode == 0, detail.stderr
        assert detail.stdout.startswith('Usage: ccmorph ' + verb)
        assert '[-a <profile>]' not in detail.stdout
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
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(root / 'lib'), 'CCMORPH_STATE_DIR': str(tmp_path / 'state')}
    for verb, brew in (('update', 'brew upgrade ccmorph'), ('uninstall', 'brew uninstall ccmorph')):
        result = subprocess.run([str(ROOT / 'bin/ccmorph'), verb], env=env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert brew in result.stderr, result.stderr
