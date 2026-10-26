"""Which environment a launch inherits from the calling shell (#53, #14).

Backend, account and credential keys are ALWAYS cleared; client tuning keys pass
through when the profile does not declare them; a nested launch clears the tuning
keys an outer ccmorph session set. Real `ccmorph` against an isolated HOME with a
stub `claude` that prints its environment.
"""
import importlib.machinery
import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))
from behaviour_coverage_test import Box, ROOT  # noqa: E402
from support import HANG_TIMEOUT  # noqa: E402

TUNING = ("MAX_THINKING_TOKENS", "CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC")
# Pinned literally (#106): deriving this from the launcher would let a name dropped from
# _CCMORPH_BACKEND_ENV vanish from the leak checks too. Every name Claude Code reads that it must list (#88).
BACKEND = (
    'CLAUDE_CODE_OAUTH_TOKEN',
    'CLAUDE_CODE_OAUTH_REFRESH_TOKEN',
    'ANTHROPIC_API_KEY',
    'ANTHROPIC_AUTH_TOKEN',
    'ANTHROPIC_BASE_URL',
    'ANTHROPIC_UNIX_SOCKET',
    'ANTHROPIC_CUSTOM_HEADERS',
    'CLAUDE_CODE_USE_BEDROCK',
    'CLAUDE_CODE_USE_VERTEX',
    'CLAUDE_CODE_USE_FOUNDRY',
    'CLAUDE_CODE_USE_GATEWAY',
    'CLAUDE_CODE_USE_MANTLE',
    'CLAUDE_CODE_USE_ANTHROPIC_AWS',
    'CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD',
    'AWS_BEARER_TOKEN_BEDROCK',
    'ANTHROPIC_AWS_API_KEY',
    'ANTHROPIC_FOUNDRY_API_KEY',
    'ANTHROPIC_FOUNDRY_AUTH_TOKEN',
    'ANTHROPIC_AWS_BASE_URL',
    'ANTHROPIC_BEDROCK_BASE_URL',
    'ANTHROPIC_BEDROCK_MANTLE_BASE_URL',
    'ANTHROPIC_VERTEX_BASE_URL',
    'CLAUDE_CODE_API_BASE_URL',
    'ANTHROPIC_ORGANIZATION_ID',
    'ANTHROPIC_WORKSPACE_ID',
    'ANTHROPIC_PROFILE',
    'ANTHROPIC_FEDERATION_RULE_ID',
    'CLAUDE_CODE_ACCOUNT_UUID',
    'CLAUDE_CODE_ORGANIZATION_UUID',
    'CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR',
    'CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR',
    'CLAUDE_CODE_SESSION_ACCESS_TOKEN',
    'CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR',
    'ANTHROPIC_FOUNDRY_BASE_URL',
    'ANTHROPIC_GOOGLE_CLOUD_BASE_URL',
    'ANTHROPIC_IDENTITY_TOKEN',
    'ANTHROPIC_IDENTITY_TOKEN_FILE',
)


@pytest.fixture
def box(tmp_path):
    b = Box(tmp_path)
    (tmp_path / 'bin/claude').write_text(
        '#!/bin/sh\n[ "$1" = --help ] && exit 0\n'
        'env | grep -E "^(MAX_THINKING|CLAUDE_CODE_|AWS_|ANTHROPIC_|CCMORPH_SET)" | sort >> "$HOME/env.log"\n'
        'echo "--" >> "$HOME/env.log"\n')
    b.conf('plain', 'type=sub\nemail=me@example.invalid\n')
    b.conf('tuned', 'type=apikey\napi_key=k\nenv.MAX_THINKING_TOKENS=0\n')
    return b


def seen(box):
    p = box.home / 'env.log'
    return p.read_text().split('--\n') if p.exists() else []


def test_an_ambient_thinking_budget_reaches_a_profile_that_does_not_set_one(box):
    box.env.update(MAX_THINKING_TOKENS='4096', CLAUDE_CODE_SIMPLE='1',
                   CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC='1')
    assert box.run('run', 'plain').returncode == 0
    out = seen(box)[0]
    for line in ('MAX_THINKING_TOKENS=4096', 'CLAUDE_CODE_SIMPLE=1',
                 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1'):
        assert line in out.splitlines(), out
    assert 'CCMORPH_SET_ENV' not in out


def test_a_profile_that_declares_a_tuning_key_wins_over_the_shell(box):
    box.env['MAX_THINKING_TOKENS'] = '4096'
    assert box.run('run', 'tuned').returncode == 0
    out = seen(box)[0].splitlines()
    assert 'MAX_THINKING_TOKENS=0' in out and 'MAX_THINKING_TOKENS=4096' not in out
    assert 'CCMORPH_SET_ENV=MAX_THINKING_TOKENS' in out


def test_a_nested_launch_does_not_inherit_the_outer_sessions_tuning(box):
    # Outer launch exports MAX_THINKING_TOKENS=0 from its profile; the stub then
    # launches `ccmorph run plain` from inside that environment.
    (box.home / 'bin/claude').write_text(
        '#!/bin/sh\n[ "$1" = --help ] && exit 0\n'
        'if [ -z "$NESTED" ]; then NESTED=1 "' + str(ROOT / "bin/ccmorph") + '" run plain; exit; fi\n'
        'echo "MTT=${MAX_THINKING_TOKENS-<unset>} SET=${CCMORPH_SET_ENV-<unset>}" >> "$HOME/env.log"\n'
        'echo "--" >> "$HOME/env.log"\n')
    assert box.run('run', 'tuned').returncode == 0
    assert seen(box)[0].strip() == 'MTT=<unset> SET=<unset>'


def _simple_stub(box, profile='plain'):
    # Outer stub plays `claude --bare`: it sets SIMPLE and the child-session marker itself, then
    # launches `ccmorph run plain`; the nested stub records what it inherited.
    (box.home / 'bin/claude').write_text(
        '#!/bin/sh\n[ "$1" = --help ] && exit 0\n'
        'if [ -z "$NESTED" ]; then NESTED=1 CLAUDE_CODE_CHILD_SESSION=1 CLAUDE_CODE_SIMPLE=1 "' + str(ROOT / "bin/ccmorph") + '" run ' + profile + '; exit; fi\n'
        'echo "SIMPLE=${CLAUDE_CODE_SIMPLE-<unset>}" >> "$HOME/env.log"\n'
        'echo "--" >> "$HOME/env.log"\n')


def test_a_nested_launch_does_not_inherit_bare_mode(box):
    _simple_stub(box)
    assert box.run('run', 'plain').returncode == 0
    assert seen(box)[0].strip() == 'SIMPLE=<unset>'


def test_a_nested_launch_still_sets_bare_mode_a_profile_declares(box):
    box.conf('declared', 'type=apikey\napi_key=k\nenv.CLAUDE_CODE_SIMPLE=1\n')
    _simple_stub(box, 'declared')
    assert box.run('run', 'declared').returncode == 0
    assert seen(box)[0].strip() == 'SIMPLE=1'


def test_a_top_level_shell_keeps_its_own_bare_mode(box):
    box.env['CLAUDE_CODE_SIMPLE'] = '1'
    assert 'CLAUDE_CODE_CHILD_SESSION' not in box.env
    assert box.run('run', 'plain').returncode == 0
    assert 'CLAUDE_CODE_SIMPLE=1' in seen(box)[0].splitlines()


def test_claudecode_alone_keeps_the_shells_bare_mode(box):
    # CLAUDECODE=1 is set in any shell Claude Code spawns, so it is not a nesting signal.
    box.env.update(CLAUDE_CODE_SIMPLE='1', CLAUDECODE='1')
    assert box.run('run', 'plain').returncode == 0
    assert 'CLAUDE_CODE_SIMPLE=1' in seen(box)[0].splitlines()


@pytest.mark.parametrize('name', BACKEND)
def test_launch_does_not_inherit_other_backends_credentials(box, name):
    box.env[name] = 'leak'
    assert box.run('run', 'plain').returncode == 0
    assert name not in seen(box)[0], seen(box)[0]


def test_the_launcher_lists_every_pinned_backend_name(tmp_path):
    listed = set(Box(tmp_path).run('_backend_env').stdout.split())
    assert set(BACKEND) <= listed, sorted(set(BACKEND) - listed)


def _rc():
    loader = importlib.machinery.SourceFileLoader("rc_env_rules", str(ROOT / "addons/remote-control/ccmorph-rc"))
    mod = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    loader.exec_module(mod)
    return mod


def test_the_addon_strips_exactly_the_launchers_backend_list_plus_proxies(tmp_path, monkeypatch):
    monkeypatch.setenv('HOME', str(tmp_path))
    monkeypatch.delenv('CCMORPH_STATE_DIR', raising=False)
    rc = _rc()
    listed = subprocess.run(['zsh', '-c', 'source "$1" && ccmorph _backend_env', 'x', str(ROOT / 'lib/ccmorph.sh')],
                            capture_output=True, text=True, check=True, timeout=HANG_TIMEOUT).stdout.split()
    assert listed and set(listed) >= set(BACKEND)
    assert set(rc.strip_env()) == set(listed) | set(rc.PROXY_ENV)


def test_the_seam_leaves_home_alone_and_ignores_local_sh_output(tmp_path, monkeypatch):
    monkeypatch.setenv('HOME', str(tmp_path))
    monkeypatch.delenv('CCMORPH_STATE_DIR', raising=False)
    monkeypatch.delenv('XDG_CONFIG_HOME', raising=False)
    state = tmp_path / '.config/ccmorph'
    state.mkdir(parents=True)
    (state / 'local.sh').write_text('print "PATH HOME ready"\n')
    before = sorted(p.relative_to(tmp_path) for p in tmp_path.rglob('*'))
    names = _rc().strip_env()
    assert not {'PATH', 'HOME', 'ready'} & set(names)
    assert 'CLAUDE_CODE_USE_BEDROCK' in names
    assert sorted(p.relative_to(tmp_path) for p in tmp_path.rglob('*')) == before


def _nested_stub(box, apply_settings):
    (box.home / 'bin/claude').write_text(
        '#!/usr/bin/env python3\nimport json, os, sys\n'
        "if sys.argv[1:2] == ['--help']: sys.exit(0)\n"
        + ("cfg = os.environ.get('CLAUDE_CONFIG_DIR', '')\n"
           "try: os.environ.update(json.load(open(cfg + '/settings.json')).get('env', {}))\n"
           'except Exception: pass\n' if apply_settings else '')
        + "if not os.environ.get('NESTED'):\n    os.environ['NESTED'] = '1'\n"
        f"    os.execv('{ROOT}/bin/ccmorph', ['ccmorph', 'run', 'plain'])\n"
        "open(os.environ['HOME'] + '/env.log', 'a').write('MTT=' + os.environ.get('MAX_THINKING_TOKENS', '<unset>') + '\\n--\\n')\n")


def test_a_tuning_key_from_the_outer_profiles_settings_layer_is_not_inherited(box):
    import json
    # No conf env. line: the value comes only from the settings layer (as in the probe).
    box.conf('tuned', 'type=apikey\napi_key=k\n')
    (box.state / 'profiles/tuned').mkdir(parents=True, exist_ok=True)
    (box.state / 'profiles/tuned/settings.profile.json').write_text(
        json.dumps({'env': {'MAX_THINKING_TOKENS': '0'}}))
    _nested_stub(box, True)
    assert box.run('run', 'tuned').returncode == 0
    assert seen(box)[0].strip() == 'MTT=<unset>'


@pytest.mark.parametrize('junk', ['PATH', 'bad-name', 'PATH MAX_THINKING_TOKENS'])
def test_an_inherited_set_env_only_unsets_tuning_keys(box, junk):
    box.env['CCMORPH_SET_ENV'] = junk
    assert box.run('run', 'plain').returncode == 0
    assert seen(box) != []
