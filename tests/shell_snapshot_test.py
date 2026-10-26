"""A captured public function must not depend on uncaptured private shell state."""
import os
from pathlib import Path
import subprocess

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def shell(script, env):
    return subprocess.run(
        ['zsh', '-f', '-c', script], env=env,
        capture_output=True, text=True, timeout=HANG_TIMEOUT,
    )


@pytest.fixture
def env(tmp_path):
    state = tmp_path / 'state'
    state.mkdir()
    (state / 'demo.conf').write_text('type=sub\nemail=demo@example.test\n')
    return {
        'HOME': str(tmp_path),
        'XDG_CONFIG_HOME': str(tmp_path / '.config'),
        'CCPROTEUS_STATE_DIR': str(state), 'CCPROTEUS_LIB_DIR': str(ROOT / "lib"),
        'PATH': f'{ROOT / "bin"}{os.pathsep}{os.environ["PATH"]}',
        'CLAUDE_CODE_SESSION_ID': 'snapshot-test', 'CCPROTEUS_RELAY_ID': '123.456.789',
    }


@pytest.mark.parametrize('args', ['--help', 'hop demo', 'hop missing'])
def test_public_function_survives_without_private_helpers(env, args):
    captured = shell('source "$CCPROTEUS_LIB_DIR/ccproteus.sh"; functions ccproteus', env)
    assert captured.returncode == 0, captured.stderr
    # Claude replays public functions, not their private helpers/globals, and
    # disables bare glob qualifiers in its command shell.
    script = captured.stdout + '\nunsetopt bareglobqual\nccproteus ' + args
    result = shell(script, env)
    assert 'command not found' not in result.stderr
    if args == '--help':
        assert result.returncode == 0, result.stderr
        assert 'Claude Code profile switcher' in result.stdout
    elif args == 'hop demo':
        assert result.returncode == 0, result.stderr
        assert 'Switch queued' in result.stdout
        relay = Path(env['CCPROTEUS_STATE_DIR']) / '.reswitch.123.456.789'
        assert relay.read_text() == 'profile=demo\nsession=snapshot-test\n'
    else:
        assert result.returncode == 1
        assert "no profile 'missing'" in result.stderr


def test_sourced_function_restores_options_without_changing_callers(env):
    result = shell('''
source "$CCPROTEUS_LIB_DIR/ccproteus.sh"
unsetopt bareglobqual
ccproteus --help || exit $?
[[ -o bareglobqual ]] && exit 9
exit 0
''', env)
    assert result.returncode == 0, result.stderr
    assert 'Claude Code profile switcher' in result.stdout


def test_state_path_ignores_user_utility_functions_and_aliases(env, tmp_path):
    claude = tmp_path / 'claude'
    claude.write_text('#!/bin/sh\nprintf "Commands:\\n  probe\\n"\n')
    claude.chmod(0o755)
    env = {**env, 'PATH': f'{tmp_path}{os.pathsep}{env["PATH"]}'}
    result = shell('''
rm() { print -u2 poisoned-rm; return 97 }
python3() { print -u2 poisoned-python3; return 97 }
sed() { print -u2 poisoned-sed; return 97 }
tr() { print -u2 poisoned-tr; return 97 }
alias rm='false'
alias python3='false'
alias sed='false'
alias tr='false'
source "$CCPROTEUS_LIB_DIR/ccproteus.sh"
_ccp_claude_verbs
ccproteus rm demo
''', env)
    assert result.returncode == 0, result.stderr
    assert 'probe' in result.stdout
    assert not (Path(env['CCPROTEUS_STATE_DIR']) / 'demo.conf').exists()
    assert 'poisoned-' not in result.stderr
