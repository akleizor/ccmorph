"""Documented gateway-picker behaviour (docs/guide.md "Exclude unwanted gateway models")
that model_exclusions_test.py does not reach: the real `set ... -i` and `run` entry points."""
import json
import os
from pathlib import Path
import shutil
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
FZF = '#!/usr/bin/env python3\nimport sys\nsys.stdout.write(sys.stdin.read())\n'


def copied_lib(tmp_path):
    """A throwaway install root, so a test can stub one libexec program in place."""
    inst = tmp_path / 'inst'
    shutil.copytree(ROOT, inst, ignore=shutil.ignore_patterns('.git', '.claude', 'tests', '__pycache__'))
    return inst / 'lib'


def stub_sizer(lib, args_file):
    """Replace the networked sizer in a copied lib; it records its arguments instead."""
    stub = lib.parent / 'libexec/refresh-model-registry'
    stub.write_text(f'#!/bin/sh\necho "$@" > "{args_file}"\n')
    stub.chmod(0o755)


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
    return state, {**os.environ, 'HOME': str(tmp_path), 'CCMORPH_STATE_DIR': str(state),
                   'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'PATH': f'{bindir}:{os.environ["PATH"]}',
                   'LISTING': json.dumps({'data': [{'id': m} for m in IDS]})}


IDS = ['blocked', 'allowed', 'future-model']


def claude_calls(tmp_path):
    f = tmp_path / 'claude.ran'
    return f.read_text() if f.exists() else ''


def zsh(script, env):
    return subprocess.run(['zsh', '-fc', 'source "$1"\n' + script, '_',
                           str(Path(env['CCMORPH_LIB_DIR']) / 'ccmorph.sh')],
                          env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT, stdin=subprocess.DEVNULL)


def set_model_i(tmp_path, excluded):
    state, env = setup(tmp_path, excluded)
    # fzf stub echoes every row; the picker returns the first (sorted) one it was offered.
    r = zsh('_ccm_gw_models_json() { REPLY="$LISTING"; }\nccmorph set demo model -i', env)
    assert r.returncode == 0, r.stderr
    return (state / 'demo.conf').read_text()


def test_set_model_interactive_honours_exclusions(tmp_path):
    assert 'model=blocked' in set_model_i(tmp_path, 'allowed')   # `allowed` sorts first but is excluded


def test_set_model_interactive_offers_everything_not_excluded(tmp_path):
    assert 'model=allowed' in set_model_i(tmp_path, 'blocked')


def test_launch_stops_when_exclusions_leave_no_rows(tmp_path):
    state, env = setup(tmp_path, 'blocked,allowed,future-model')
    r = zsh('_ccm_models_listing() { REPLY="$LISTING"; }\nccmorph run demo -p hi', env)
    assert r.returncode != 0
    assert 'no cached or served picker models remain' in r.stderr
    assert '-p hi' not in claude_calls(tmp_path)
    settings = state / 'profiles/demo/settings.json'
    assert not settings.exists() or 'availableModels' not in json.loads(settings.read_text()) \
        or json.loads(settings.read_text())['availableModels']


def test_launch_projects_filtered_rows_and_keeps_discovery_off(tmp_path):
    lib = copied_lib(tmp_path)
    stub_sizer(lib, tmp_path / 'sizer.ran')
    state, env = setup(tmp_path, 'blocked')
    env['CCMORPH_LIB_DIR'] = str(lib)
    r = zsh('_ccm_models_listing() { REPLY="$LISTING"; }\nccmorph run demo -p hi', env)
    assert r.returncode == 0, r.stderr
    # The copied lib's sizer stub writes this; the real one would size the synthetic
    # ids against models.dev and the live endpoint on every launch. A missing marker
    # means the real, networked sizer ran again.
    assert (tmp_path / 'sizer.ran').read_text().split() == ['--listing-stdin', '--profile', 'demo']
    assert '-p hi' in claude_calls(tmp_path)
    assert 'GATEWAY_MODEL_DISCOVERY' not in (tmp_path / 'claude.env').read_text()
    home = state / 'profiles/demo'
    rows = [x['value'] for x in json.loads((home / '.claude.json').read_text())['additionalModelOptionsCache']]
    assert rows == ['allowed', 'future-model']
    assert 'blocked' not in json.loads((home / 'settings.json').read_text())['availableModels']


def test_probe_model_interactive_honours_exclusions(tmp_path):
    # `--model -i` is the probe flag; it must offer the same filtered rows as `set ... -i`.
    lib = copied_lib(tmp_path)
    stub_sizer(lib, tmp_path / 'probe.args')
    state, env = setup(tmp_path, 'allowed')
    env['CCMORPH_LIB_DIR'] = str(lib)
    r = zsh('_ccm_gw_models_json() { REPLY="$LISTING"; }\nccmorph probe demo --model -i', env)
    assert r.returncode == 0, r.stderr
    assert '--model blocked' in (tmp_path / 'probe.args').read_text()
