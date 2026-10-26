"""Product defaults do not require environment tuning."""
import ast
import os
from pathlib import Path
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def test_fixed_defaults_are_not_environment_overrides():
    expected = {
        'ccproteus-adapter': {'LOCK_WAIT': 120, 'JOBS': 8},
        'refresh-model-registry': {
            'CATALOGUE_URL': 'https://models.dev/api.json',
            'CANONICAL_CATALOGUE_URL': 'https://models.dev/models.json',
        },
    }
    for filename, values in expected.items():
        assignments = {
            target.id: node.value
            for node in ast.parse((ROOT / 'libexec' / filename).read_text()).body
            if isinstance(node, ast.Assign)
            for target in node.targets if isinstance(target, ast.Name)
        }
        for name, value in values.items():
            assert ast.literal_eval(assignments[name]) == value


def test_provider_uses_path_not_private_override(tmp_path):
    binary = tmp_path / 'cliproxyapi'
    binary.write_text('#!/bin/sh\nexit 0\n')
    binary.chmod(0o755)
    env = {
        **os.environ,
        'HOME': str(tmp_path),
        'CCPROTEUS_STATE_DIR': str(tmp_path / 'state'),
        'CCPROTEUS_LIB_DIR': str(ROOT / "lib"),
        'CCPROTEUS_PROVIDER_BIN': '/nonexistent/ignored-override',
        'PATH': f'{tmp_path}:/usr/bin:/bin',
    }
    result = subprocess.run(
        ['/bin/zsh', '-f', '-c',
         'source "$1"; _ccp_provider_bin || exit; print -r -- "$REPLY"',
         'test', str(ROOT / 'lib/ccproteus.sh')],
        env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT,
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == str(binary)
