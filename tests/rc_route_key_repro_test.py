"""The reviewer's SIGTERM repro for #777, kept as a test.

The refusal for this issue carried this script as its repro: a route start whose
wait_listening sends SIGTERM to itself must leave no route-key.* file in STATE.
It is kept verbatim so the fix that the refusal asked for stays pinned. Red on
the launcher before the fix (the key survives the signal), green after.
"""
import os
import subprocess
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parent.parent

REPRO = r'''python3 - <<'PY'
import os, pathlib, runpy, signal, sys, tempfile

with tempfile.TemporaryDirectory() as root:
    pid = os.fork()
    if pid == 0:
        os.environ['CCMORPH_STATE_DIR'] = root
        sys.argv = ['ccmorph-rc', 'test', '--as', 'account', '--edge']
        def stub_start_dependencies(frame, event, arg):
            if event == 'call' and frame.f_code.co_name == 'main' and frame.f_globals.get('__name__') == '__main__':
                sys.settrace(None)
                rc = frame.f_globals
                rc.update(raise_nofile=lambda: None,
                          profile_type=lambda p: 'sub' if p == 'account' else 'apikey',
                          profile_env=lambda p: {'CLAUDE_CONFIG_DIR': root},
                          resolve_route=lambda p: {'ANTHROPIC_API_KEY': 'test-only-key', 'ANTHROPIC_BASE_URL': 'https://example.invalid'},
                          retire_route_with=lambda *a: None, strip_env=tuple,
                          start_adapter=lambda *a: 'http://127.0.0.1:1',
                          wait_listening=lambda *a, **k: os.kill(os.getpid(), signal.SIGTERM))
                rc['shutil'].which = lambda name: '/test/' + name
                rc['subprocess'].Popen = lambda *a, **k: object()
            return stub_start_dependencies
        sys.settrace(stub_start_dependencies)
        runpy.run_path('addons/remote-control/ccmorph-rc', run_name='__main__')
        os._exit(0)
    os.waitpid(pid, 0)
    assert not list(pathlib.Path(root).rglob('route-key.*')), 'SIGTERM during proxy startup leaves the route key on disk'
PY
'''


def test_the_refusals_startup_sigterm_repro_leaves_no_route_key(tmp_path):
    """The script from the refusal runs from the repo root against this tree."""
    env = {**os.environ, "TMPDIR": str(tmp_path)}
    result = subprocess.run(["bash", "-c", REPRO], cwd=ROOT, env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, f"exit={result.returncode}; stderr={result.stderr}"
