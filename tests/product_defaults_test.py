"""Product defaults do not require environment tuning."""
import ast
import os
from pathlib import Path
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def test_provider_uses_path_not_private_override(tmp_path):
    binary = tmp_path / 'cliproxyapi'
    binary.write_text('#!/bin/sh\nexit 0\n')
    binary.chmod(0o755)
    env = {
        **os.environ,
        'HOME': str(tmp_path),
        'CCMORPH_STATE_DIR': str(tmp_path / 'state'),
        'CCMORPH_LIB_DIR': str(ROOT / "lib"),
        'CCMORPH_PROVIDER_BIN': '/nonexistent/ignored-override',
        'PATH': f'{tmp_path}:/usr/bin:/bin',
    }
    result = subprocess.run(
        ['/bin/zsh', '-f', '-c',
         'source "$1"; _ccm_provider_bin || exit; print -r -- "$REPLY"',
         'test', str(ROOT / 'lib/ccmorph.sh')],
        env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT,
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == str(binary)
