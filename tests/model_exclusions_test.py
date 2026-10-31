"""Explicit exclusions use native picker settings without freezing gateway inventory."""
import json
import os
from pathlib import Path
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def project(tmp_path, served, excluded='blocked', *, offline=False, cached=None, current=None):
    state = tmp_path / 'state'
    home = state / 'profiles/demo'
    home.mkdir(parents=True, exist_ok=True)
    (state / 'demo.conf').write_text(
        'type=apikey\napi_key=synthetic\nbase_url=https://example.invalid\n'
        'env.ANTHROPIC_DEFAULT_OPUS_MODEL=blocked\n'
        f'excluded_models={excluded}\n')
    rows = cached if cached is not None else [{'value': 'blocked'}, {'value': 'allowed'}]
    (home / '.claude.json').write_text(json.dumps({'additionalModelOptionsCache': rows, 'other': True}))
    settings = {'model': 'blocked', 'availableModels': current or ['blocked', 'allowed', 'opus'],
                'permissions': {'deny': ['Bash(rm *)']}}
    (home / 'settings.json').write_text(json.dumps(settings))
    env = {**os.environ, 'HOME': str(tmp_path), 'CCMORPH_STATE_DIR': str(state),
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'LISTING': json.dumps({'data': [{'id': m} for m in served]}),
           'OFFLINE': '1' if offline else '0'}
    script = '''source "$1"
mkdir -p "$HOME/fakebin" && printf '%s\\n' '#!/bin/sh' '[ "$OFFLINE" = 1 ] && exit 22' 'printf "%s" "$LISTING"' > "$HOME/fakebin/curl" && chmod +x "$HOME/fakebin/curl" && export PATH="$HOME/fakebin:$PATH"
_ccm_read demo || exit
_ccm_project_picker demo https://example.invalid synthetic blocked Test
'''
    result = subprocess.run(['zsh', '-fc', script, '_', str(ROOT / 'lib/ccmorph.sh')],
                            env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    return result, json.loads((home / '.claude.json').read_text()), json.loads((home / 'settings.json').read_text())


def test_exclusions_remove_saved_model_alias_and_derived_id(tmp_path):
    result, cache, settings = project(tmp_path, ['blocked', 'blocked[1m]', 'allowed', 'new-model'])
    assert result.returncode == 0, result.stderr
    assert [row['value'] for row in cache['additionalModelOptionsCache']] == ['allowed', 'new-model']
    assert settings['availableModels'] == ['allowed', 'new-model']
    assert settings['model'] == 'blocked'  # filtering never silently changes the user's default
    assert settings['permissions'] == {'deny': ['Bash(rm *)']}
    assert cache['other'] is True


def test_new_gateway_model_appears_without_editing_exclusions(tmp_path):
    result, _, settings = project(tmp_path, ['allowed', 'blocked', 'future-model'])
    assert result.returncode == 0, result.stderr
    assert 'future-model' in settings['availableModels']
    assert 'blocked' not in settings['availableModels']


def test_offline_refresh_filters_cached_rows_and_preserves_other_choices(tmp_path):
    result, cache, settings = project(tmp_path, [], offline=True,
                                     current=['blocked', 'allowed', 'other', 'opus'])
    assert result.returncode == 0, result.stderr
    assert [r['value'] for r in cache['additionalModelOptionsCache']] == ['allowed']
    assert settings['availableModels'] == ['allowed', 'other']


def test_empty_result_refuses_instead_of_enabling_native_defaults(tmp_path):
    result, cache, settings = project(tmp_path, ['blocked'])
    assert result.returncode != 0
    assert 'no cached or served picker models remain' in result.stderr
    assert settings['availableModels'] == ['blocked', 'allowed', 'opus']


def test_exclusions_are_exact_case_sensitive_ids(tmp_path):
    result, _, settings = project(tmp_path, ['blocked', 'blocked-new', 'Blocked'])
    assert result.returncode == 0
    assert 'blocked-new' in settings['availableModels'] and 'Blocked' in settings['availableModels']


def test_cli_pickers_filter_but_catalogue_stays_unfiltered(tmp_path):
    ids = ['blocked', 'blocked[1m]', 'opus', 'allowed', 'future-model']
    result, cache, _ = project(tmp_path, ids)
    assert result.returncode == 0
    bindir = tmp_path / 'bin'
    bindir.mkdir()
    fzf = bindir / 'fzf'
    fzf.write_text('#!/usr/bin/env python3\nimport sys\nsys.stdout.write(sys.stdin.read())\n')
    fzf.chmod(0o755)
    env = {**os.environ, 'HOME': str(tmp_path), 'CCMORPH_STATE_DIR': str(tmp_path / 'state'),
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'PATH': str(bindir) + ':' + os.environ['PATH'],
           'LISTING': json.dumps({'data': [{'id': m, 'owned_by': 'test'} for m in ids]})}
    script = '''source "$1"
_ccm_gw_models_json() { REPLY="$LISTING"; }
_ccm_pick_value model demo
ccmorph _menu demo
ccmorph models demo
'''
    r = subprocess.run(['zsh', '-fc', script, '_', str(ROOT / 'lib/ccmorph.sh')],
                       env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
    assert r.returncode == 0, r.stderr
    # The fzf fixture returns every input line; picker functions return its first ID.
    assert r.stdout.splitlines()[:2] == ['allowed', 'allowed']
    catalogue = [line.split('\t')[0] for line in r.stdout.splitlines()[2:]]
    assert set(catalogue) == set(ids)
    filtered = subprocess.run(
        ['zsh', '-fc', 'source "$1"; _ccm_gw_models_json() { REPLY="$LISTING"; }; ccmorph models demo --filtered',
         '_', str(ROOT / 'lib/ccmorph.sh')],
        env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
    assert filtered.returncode == 0, filtered.stderr
    assert {line.split('\t')[0] for line in filtered.stdout.splitlines()} == {'allowed', 'future-model'}
    filtered_json = subprocess.run(
        ['zsh', '-fc', 'source "$1"; _ccm_gw_models_json() { REPLY="$LISTING"; }; ccmorph models demo --filtered-json',
         '_', str(ROOT / 'lib/ccmorph.sh')],
        env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
    assert filtered_json.returncode == 0, filtered_json.stderr
    assert {row['id'] for row in json.loads(filtered_json.stdout)['data']} == {'allowed', 'future-model'}
    # Both selection surfaces derive the same visible IDs, unlike the raw catalogue.
    r = subprocess.run(['zsh', '-fc', 'source "$1"; _ccm_gw_models_json() { REPLY="$LISTING"; }; _ccm_gw_models demo picker',
                        '_', str(ROOT / 'lib/ccmorph.sh')], env=env, text=True,
                       capture_output=True, timeout=HANG_TIMEOUT)
    assert r.returncode == 0, r.stderr
    assert {line.split('\t')[0] for line in r.stdout.splitlines()} == {row['value'] for row in cache['additionalModelOptionsCache']}


def test_malformed_exclusion_list_is_rejected(tmp_path):
    result, _, _ = project(tmp_path, ['allowed'], excluded='blocked,,other')
    assert result.returncode != 0
    assert 'comma-separated exact model IDs' in result.stderr
