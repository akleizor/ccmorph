"""Documented gateway-picker behaviour (docs/guide.md "Exclude unwanted gateway models")
that model_exclusions_test.py does not reach: the real `set ... -i` and `run` entry points."""
import json
import os
from pathlib import Path
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
FZF = '#!/usr/bin/env python3\nimport sys\nsys.stdout.write(sys.stdin.read())\n'


def setup(tmp_path, excluded):
    state = tmp_path / 'state'
    (state / 'profiles/demo').mkdir(parents=True)
    (state / 'demo.conf').write_text(
        'type=apikey\napi_key=synthetic\nbase_url=https://api.anthropic.com\n'
        f'excluded_models={excluded}\n')
    bindir = tmp_path / 'bin'
    bindir.mkdir()
    (bindir / 'fzf').write_text(FZF)
    (bindir / 'claude').write_text(f'#!/bin/sh\necho "$@" >> "{tmp_path}/claude.ran"\n'
                                  f'env > "{tmp_path}/claude.env"\n')
    for f in bindir.iterdir():
        f.chmod(0o755)
    return state, {**os.environ, 'HOME': str(tmp_path), 'CCPROTEUS_STATE_DIR': str(state),
                   'CCPROTEUS_LIB_DIR': str(ROOT), 'PATH': f'{bindir}:{os.environ["PATH"]}',
                   'LISTING': json.dumps({'data': [{'id': m} for m in IDS]})}


IDS = ['blocked', 'allowed', 'future-model']


def claude_calls(tmp_path):
    f = tmp_path / 'claude.ran'
    return f.read_text() if f.exists() else ''


def zsh(script, env):
    return subprocess.run(['zsh', '-fc', 'source "$1"\n' + script, '_', str(ROOT / 'ccproteus.sh')],
                          env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT, stdin=subprocess.DEVNULL)


def set_model_i(tmp_path, excluded):
    state, env = setup(tmp_path, excluded)
    # fzf stub echoes every row; the picker returns the first (sorted) one it was offered.
    r = zsh('_ccs_gw_models_json() { REPLY="$LISTING"; }\nccproteus set demo model -i', env)
    assert r.returncode == 0, r.stderr
    return (state / 'demo.conf').read_text()


def test_set_model_interactive_honours_exclusions(tmp_path):
    assert 'model=blocked' in set_model_i(tmp_path, 'allowed')   # `allowed` sorts first but is excluded


def test_set_model_interactive_offers_everything_not_excluded(tmp_path):
    assert 'model=allowed' in set_model_i(tmp_path, 'blocked')


def test_launch_stops_when_exclusions_leave_no_rows(tmp_path):
    state, env = setup(tmp_path, 'blocked,allowed,future-model')
    r = zsh('_ccs_models_listing() { REPLY="$LISTING"; }\nccproteus run demo -p hi', env)
    assert r.returncode != 0
    assert 'no cached or served picker models remain' in r.stderr
    assert '-p hi' not in claude_calls(tmp_path)
    settings = state / 'profiles/demo/settings.json'
    assert not settings.exists() or 'availableModels' not in json.loads(settings.read_text()) \
        or json.loads(settings.read_text())['availableModels']


def test_launch_projects_filtered_rows_and_keeps_discovery_off(tmp_path):
    state, env = setup(tmp_path, 'blocked')
    r = zsh('_ccs_models_listing() { REPLY="$LISTING"; }\nccproteus run demo -p hi', env)
    assert r.returncode == 0, r.stderr
    assert '-p hi' in claude_calls(tmp_path)
    assert 'GATEWAY_MODEL_DISCOVERY' not in (tmp_path / 'claude.env').read_text()
    home = state / 'profiles/demo'
    rows = [x['value'] for x in json.loads((home / '.claude.json').read_text())['additionalModelOptionsCache']]
    assert rows == ['allowed', 'future-model']
    assert 'blocked' not in json.loads((home / 'settings.json').read_text())['availableModels']


def test_probe_model_interactive_honours_exclusions(tmp_path):
    # `--model -i` is the probe flag; it must offer the same filtered rows as `set ... -i`.
    import shutil
    lib = tmp_path / 'lib'
    shutil.copytree(ROOT, lib, ignore=shutil.ignore_patterns('.git', '.claude', 'tests', '__pycache__'))
    stub = lib / 'refresh-model-registry'
    stub.write_text(f'#!/bin/sh\necho "$@" > "{tmp_path}/probe.args"\n')
    stub.chmod(0o755)
    state, env = setup(tmp_path, 'allowed')
    env['CCPROTEUS_LIB_DIR'] = str(lib)
    r = subprocess.run(['zsh', '-fc', 'source "$1"\n_ccs_gw_models_json() { REPLY="$LISTING"; }\nccproteus probe demo --model -i',
                        '_', str(lib / 'ccproteus.sh')], env=env, text=True, capture_output=True,
                       timeout=HANG_TIMEOUT, stdin=subprocess.DEVNULL)
    assert r.returncode == 0, r.stderr
    assert '--model blocked' in (tmp_path / 'probe.args').read_text()
