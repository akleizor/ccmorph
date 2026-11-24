"""The reviewers' repros for #777, kept as tests.

The first refusal for this issue carried a script as its repro: a route start whose
wait_listening sends SIGTERM to itself must leave no route-key.* file in STATE.
It is kept verbatim so the fix that the refusal asked for stays pinned. Red on
the launcher before the fix (the key survives the signal), green after. The
second refusal's concurrent-switch repro is kept the same way, at the bottom.
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


# The second refusal (review of ed2a422): a switch whose handoff rename fails must
# not delete the temp a concurrent switch to the same profile staged. Kept verbatim.
# Red on ed2a422 (the shared switch-<profile>.tmp was unlinked under the second
# switch, which then failed with FileNotFoundError), green with one mkstemp temp
# per switch.
SWITCH_REPRO = r'''python3 -c '
import os, runpy, tempfile, threading
from pathlib import Path
with tempfile.TemporaryDirectory() as state:
    os.environ["CCMORPH_STATE_DIR"] = state
    rc = runpy.run_path("addons/remote-control/ccmorph-rc")["switch"].__globals__
    rc["EDGES"].mkdir(parents=True)
    (rc["EDGES"] / "main.json").touch()
    rc.update(live_edge=lambda p: {"pid": 42} if p == "main" else None,
              profile_type=lambda p: "apikey", adapter_env=lambda: {},
              resolve_route=lambda p: {"ANTHROPIC_API_KEY": "test-only-key"},
              retire_route_with=lambda *a: None, start_adapter=lambda *a: "http://127.0.0.1:1")
    real_replace, real_path_replace = os.replace, Path.replace
    staged, resume = threading.Event(), threading.Event()
    errors = []
    def switch():
        rc["switch"]("gw", "account")
    def run_second():
        try:
            switch()
        except Exception as exc:
            errors.append(exc)
    second = threading.Thread(target=run_second)
    def replace(src, dst, *args, **kw):
        if Path(dst).name != "switch-gw.json":
            return real_replace(src, dst, *args, **kw)
        if threading.current_thread() is second:
            staged.set()
            assert resume.wait(15)
            return real_replace(src, dst, *args, **kw)
        second.start()
        staged.wait(4)
        raise OSError("first rename refused")
    def path_replace(src, dst):
        replace(src, dst)
        return Path(dst)
    os.replace, Path.replace = replace, path_replace
    try:
        try:
            switch()
        except OSError as exc:
            assert str(exc) == "first rename refused", exc
    finally:
        resume.set()
        if second.ident is not None:
            second.join(15)
            assert not second.is_alive()
        os.replace, Path.replace = real_replace, real_path_replace
    assert not errors, f"the first switch removed the second switch handoff: {errors}"
    assert (rc["STATE"] / "inference").read_text() == "gw\n"
'
'''


def test_the_refusals_concurrent_switch_repro_keeps_the_other_switchs_handoff(tmp_path):
    """The script from the second refusal runs from the repo root against this tree."""
    env = {**os.environ, "TMPDIR": str(tmp_path)}
    result = subprocess.run(["bash", "-c", SWITCH_REPRO], cwd=ROOT, env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, f"exit={result.returncode}; stderr={result.stderr}"
