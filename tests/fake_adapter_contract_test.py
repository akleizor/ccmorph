"""The contract every fake adapter in the launch tests must keep.

A launcher killed outright -- a pytest-timeout, an xdist worker restart -- runs
no trap, so the fake can only be reaped the way the real adapter is: by noticing
that the `--parent-pid` it was started with is gone. Without that the fake is
reparented to PID 1 and keeps forking `sleep` for hours after pytest has deleted
the temporary home it was serving (GH-330, which found two such orphans alive
for more than four hours).

The same defect through a real launch is
`adapter_route_defaults_test.py::test_a_killed_launch_leaves_no_adapter_stub_behind`,
which #385/#386 landed. That one costs a whole routed launch; this file pins the
contract on the fake itself, where a spelling the launcher does not currently
use is still cheap to check.

GH-850 tells the serve tail's two shapes apart by ancestry, not by output: a
double the launcher started names one of its own ancestors as `--parent-pid`
(the launcher, which `util adapter-start` sits under until the ready line), so
being orphaned at the handoff is not its death -- it serves while that same
process lives and leaves on TERM. A double whose named parent is a stranger
keeps GH-434's orphan exit, whatever its output is. Both shapes, and a
launcher pid that is recycled, are pinned here and in `ccmorph_test.zsh`.

The second half is the guard: the body has one home, so a launch test cannot
re-introduce a private unbounded loop beside it. Both of GH-330's orphans came
from a private copy.
"""
import os
from pathlib import Path
import signal
import subprocess
import sys

import fake_adapter
import pytest

from support import HANG_TIMEOUT, wait_until

ROOT = Path(__file__).resolve().parent
# The argv `_ccm_adapter_start` builds for a selected launch, minus the parent pid.
LAUNCH_ARGV = ['gw', '--auto', '--model', 'claude-sonnet-4-5']
SUPPORT = ('fake_adapter.py', 'fake_adapter_serve.sh', 'fake_adapter_claim.sh', Path(__file__).name)


def _install(tmp_path: Path) -> str:
    """The fake where the launcher looks for one: a routed profile's state dir."""
    path = tmp_path / 'ccmorph-adapter'
    path.write_text(fake_adapter.script('echo PORT=45999'))
    path.chmod(0o755)
    return str(path)


def _start(tmp_path: Path, *argv: str) -> subprocess.Popen:
    return subprocess.Popen([_install(tmp_path), *argv],
                            stdout=subprocess.PIPE, text=True)


def _reap(*processes: subprocess.Popen) -> None:
    for process in processes:
        if process.poll() is None:
            process.kill()
            process.wait()


@pytest.mark.parametrize('parent_flag', ['--parent-pid {}', '--parent-pid={}'],
                         ids=['separate-argument', 'equals-argument'])
def test_the_fake_exits_when_the_launch_that_started_it_is_killed(tmp_path, parent_flag):
    """The launcher runs no trap when it is SIGKILLed: the pid is the only reap.

    Both spellings, because the real adapter parses with argparse and would take
    either -- a fake that quietly stopped reaping on one of them is GH-330 again,
    with every test still green.
    """
    launch = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(300)'])
    fake = _start(tmp_path, *LAUNCH_ARGV, *parent_flag.format(launch.pid).split())
    try:
        assert fake.stdout.readline() == 'PORT=45999\n'
        launch.kill()
        launch.wait()  # reaped, not a zombie: kill -0 still finds a zombie
        assert fake.wait(timeout=HANG_TIMEOUT) == 0
    finally:
        _reap(fake, launch)


def test_the_fake_still_serves_when_it_was_started_without_a_parent(tmp_path):
    """`--parent-pid` is optional in the real adapter; its absence is not a death."""
    fake = _start(tmp_path, *LAUNCH_ARGV)
    try:
        assert fake.stdout.readline() == 'PORT=45999\n'
        assert fake.poll() is None
        fake.terminate()
        assert fake.wait(timeout=HANG_TIMEOUT) == 0
    finally:
        _reap(fake)


def test_the_fake_records_the_pid_its_teardown_reaps(tmp_path):
    """The record is the handle a teardown can kill by; a detached fake has none.

    Nothing holds the Popen: the launcher starts the fake itself, from inside
    `ccmorph`, so a test that wants to stop it needs the pid written where the
    teardown looks -- the adapter's own path plus `.pid`.
    """
    fake = _start(tmp_path)
    try:
        assert fake.stdout.readline() == 'PORT=45999\n'
        assert (tmp_path / 'ccmorph-adapter.pid').read_text().strip() == str(fake.pid)
    finally:
        _reap(fake)


def test_reap_ends_a_fake_whose_parent_never_dies_issue_347(tmp_path):
    """The watchdog cannot cover this one, and this is what stranded the mini.

    A pid is recycled on a busy host, so `--parent-pid` can name a live process
    long after the launcher it belonged to is gone: the fake's poll then never
    fires and it serves forever. The parent here is this very test, which stays
    alive for the whole case -- a recycled pid that nobody can outwait.
    """
    fake = _start(tmp_path, *LAUNCH_ARGV, '--parent-pid', str(os.getpid()))
    try:
        assert fake.stdout.readline() == 'PORT=45999\n'
        assert fake_adapter.reap(tmp_path / 'ccmorph-adapter') is True
        assert fake.wait(timeout=HANG_TIMEOUT) == -signal.SIGKILL
    finally:
        _reap(fake)


def test_reap_never_signals_a_pid_it_did_not_start_issue_347(tmp_path):
    """A pid is not ownership: the record could name any live process.

    A recycled number is the realistic way this happens, and the cost of
    believing it is killing someone else's process on a shared machine. The
    record is checked against the command line before anything is signalled.
    """
    bystander = subprocess.Popen(['/bin/sleep', '300'])
    try:
        (tmp_path / 'ccmorph-adapter.pid').write_text(f'{bystander.pid}\n')
        assert fake_adapter.reap(tmp_path / 'ccmorph-adapter') is False
        assert bystander.poll() is None
    finally:
        _reap(bystander)


def test_reap_without_a_record_does_nothing(tmp_path):
    """No record is the ordinary end of a healthy test: nothing to do, no error."""
    _install(tmp_path)
    assert fake_adapter.reap(tmp_path / 'ccmorph-adapter') is False


@pytest.mark.parametrize('output', ['pipe', 'file'])
def test_the_fake_ends_when_its_own_launcher_is_gone_gh_434(tmp_path, output):
    """An orphan no launcher started leaves on its own, whatever its output is.

    GH-434: the `--parent-pid` here names a live bystander that never started
    the fake, the shape a recycled pid takes. The starter exits at once,
    orphaning the fake, and nobody will send it TERM -- so the serve tail must
    end it, and close the output it inherited. Both output shapes: a pipe (the
    readiness reader GH-434 hung on, read to EOF here) and a regular file (the
    shape the launcher also uses, which must not buy a stranger's orphan a
    pass).
    """
    bystander = subprocess.Popen(['/bin/sleep', '300'])
    out = tmp_path / 'out'
    try:
        if output == 'pipe':
            starter = subprocess.Popen(
                ['/bin/sh', '-c', '"$1" gw --auto --model claude-sonnet-4-5 --parent-pid "$2" &',
                 '_', _install(tmp_path), str(bystander.pid)],
                stdout=subprocess.PIPE, text=True)
        else:
            starter = subprocess.Popen(
                ['/bin/sh', '-c',
                 '"$1" gw --auto --model claude-sonnet-4-5 --parent-pid "$2" >"$3" 2>/dev/null &',
                 '_', _install(tmp_path), str(bystander.pid), str(out)])
        assert starter.wait(timeout=HANG_TIMEOUT) == 0
        # The record may not exist the instant the starter exits: wait on the
        # event itself, bounded by the shared bound.
        subprocess.run(['/bin/sh', '-c', 'while [ ! -s "$1" ]; do sleep 0.05; done', '_',
                        str(tmp_path / 'ccmorph-adapter.pid')], timeout=HANG_TIMEOUT)
        fake_pid = int((tmp_path / 'ccmorph-adapter.pid').read_text().strip())
        watcher = subprocess.run(
            ['/bin/sh', '-c', 'while kill -0 "$1" 2>/dev/null; do sleep 0.2; done', '_',
             str(fake_pid)], timeout=HANG_TIMEOUT)
        assert watcher.returncode == 0, 'the orphaned fake never left'
        if output == 'pipe':
            # Death is only half of GH-434: the orphan must have let go of the
            # inherited output, the EOF the readiness reader was waiting for.
            assert 'PORT=45999' in starter.stdout.read()
        else:
            assert 'PORT=45999' in out.read_text()
    finally:
        fake_adapter.reap(tmp_path / 'ccmorph-adapter')
        _reap(bystander, starter)


def _start_like_the_launcher(tmp_path: Path, env: dict) -> subprocess.Popen:
    """Start the fake the way `util adapter-start` does (util/adapterstart.go).

    The named parent is this test process, an ancestor of the fake, as the
    launcher is. The starter sends the fake's stdout to a ready file, waits for
    the ready line in it -- so it is alive while the fake's head looks up its
    ancestors -- and then exits, orphaning the fake: the launch handoff.
    """
    return subprocess.Popen(
        ['/bin/sh', '-c',
         '"$1" gw --auto --model claude-sonnet-4-5 --parent-pid "$2" >"$3" 2>/dev/null &\n'
         'n=0\n'
         'until grep -q "^PORT=" "$3" 2>/dev/null; do\n'
         '  n=$((n + 1)); [ "$n" -le "$4" ] || exit 1; /bin/sleep 0.05\n'
         'done',
         '_', _install(tmp_path), str(os.getpid()), str(tmp_path / 'ready.out'),
         str(int(HANG_TIMEOUT * 20))],
        env=env)


def _fake_pid(tmp_path: Path) -> int:
    subprocess.run(['/bin/sh', '-c', 'while [ ! -s "$1" ]; do sleep 0.05; done', '_',
                    str(tmp_path / 'ccmorph-adapter.pid')], timeout=HANG_TIMEOUT)
    return int((tmp_path / 'ccmorph-adapter.pid').read_text().strip())


def test_the_fake_keeps_serving_after_the_launch_handoff_gh_850(tmp_path):
    """Orphaning by the starter is the launch's handoff, not a death.

    The launcher's `util adapter-start` exits once the adapter announces its
    port, so every launched adapter is reparented the moment it serves
    (util/hop.go: "util adapter-start exits once it is ready, so the adapter is
    reparented"). GH-850: the serve tail's GH-434 orphan exit read that
    handoff as a stray and ended the double on its tenth pass, before the
    launcher's stop TERM reached it, so the stop record never appeared. The
    double the launcher started names an ancestor as its parent; it must serve
    while that process lives and leave on TERM, as `check_parent` does.

    The witness is the double's own serve passes, not the wall clock: a PATH
    `sleep` counts them, so clearing the tenth pass the old exit died on is an
    event, and the shared bound is the only budget.
    """
    serve_bin = tmp_path / 'servebin'
    serve_bin.mkdir()
    counter = serve_bin / 'sleep'
    counter.write_text('#!/bin/sh\necho x >> "$FAKE_SERVE_PASSES"\nexec /bin/sleep "$@"\n')
    counter.chmod(0o755)
    passes = tmp_path / 'passes'
    starter = _start_like_the_launcher(
        tmp_path, {**os.environ, 'PATH': f'{serve_bin}:{os.environ["PATH"]}',
                   'FAKE_SERVE_PASSES': str(passes)})
    try:
        assert starter.wait(timeout=HANG_TIMEOUT) == 0, 'the fake never printed its ready line'
        fake_pid = _fake_pid(tmp_path)

        def state():
            """'dead', 'serving' past the tenth pass, or None while still earlier."""
            try:
                os.kill(fake_pid, 0)
            except OSError:
                return 'dead'
            try:
                counted = len(passes.read_text().splitlines())
            except OSError:
                counted = 0
            return 'serving' if counted >= 11 else None

        seen = None

        def observe():
            nonlocal seen
            seen = state()
            return seen is not None

        wait_until(observe, HANG_TIMEOUT)
        assert seen == 'serving', (
            'the fake left while the launch it watches was alive: a serve loop '
            'that ends on its own timer, however long, is GH-850 again')

        # And it still answers the launcher's stop: the TERM runs the trap and
        # the fake leaves, the half the zsh suite's stopped-when-the-session-
        # ends test reads from its record.
        ended = subprocess.run(
            ['/bin/sh', '-c',
             'kill -TERM "$1"; while kill -0 "$1" 2>/dev/null; do sleep 0.2; done',
             '_', str(fake_pid)], timeout=HANG_TIMEOUT)
        assert ended.returncode == 0, 'the fake did not leave on TERM after the handoff'
    finally:
        fake_adapter.reap(tmp_path / 'ccmorph-adapter')
        _reap(starter)


def test_the_fake_ends_when_its_launchers_pid_is_recycled_gh_434(tmp_path):
    """A launched double leaves once its parent pid names another process.

    GH-434's cause: a launcher killed outright runs no trap, and its pid is
    handed to a new process that never dies, so `kill -0` keeps answering. The
    head recorded the launcher's start time; once the pid answers with another
    start time, the tail must leave without TERM. A PATH `ps` stands in for
    the recycle: after the flag file appears it reports a different start
    time for every pid.
    """
    serve_bin = tmp_path / 'servebin'
    serve_bin.mkdir()
    shim = serve_bin / 'ps'
    shim.write_text('#!/bin/sh\n'
                    'if [ -e "$FAKE_PS_RECYCLED" ]; then\n'
                    '  for a do [ "$a" = lstart= ] && { echo "Thu Jan  1 00:00:00 1970"; exit 0; }; done\n'
                    'fi\n'
                    'exec /bin/ps "$@"\n')
    shim.chmod(0o755)
    recycled = tmp_path / 'recycled'
    starter = _start_like_the_launcher(
        tmp_path, {**os.environ, 'PATH': f'{serve_bin}:{os.environ["PATH"]}',
                   'FAKE_PS_RECYCLED': str(recycled)})
    try:
        assert starter.wait(timeout=HANG_TIMEOUT) == 0, 'the fake never printed its ready line'
        fake_pid = _fake_pid(tmp_path)
        os.kill(fake_pid, 0)  # serving after the handoff, before the recycle
        recycled.touch()
        watcher = subprocess.run(
            ['/bin/sh', '-c', 'while kill -0 "$1" 2>/dev/null; do sleep 0.2; done', '_',
             str(fake_pid)], timeout=HANG_TIMEOUT)
        assert watcher.returncode == 0, 'the fake kept serving a recycled parent pid'
    finally:
        fake_adapter.reap(tmp_path / 'ccmorph-adapter')
        _reap(starter)


def _logical_lines(text: str):
    """`(first physical line number, the whole command)` for each command.

    A command written across `\\` continuations is one line to the shell, and so
    one line here: a fake assembled by `printf` keeps its path and its loop at
    opposite ends of a single command, and neither end means anything alone.
    """
    lines = []
    for number, line in enumerate(text.splitlines(), 1):
        if lines and lines[-1][1].endswith('\\'):
            lines[-1] = (lines[-1][0], lines[-1][1][:-1] + line)  # the shell reads on
        else:
            lines.append((number, line))
    return lines


def test_no_test_file_ships_its_own_unbounded_adapter_fake():
    """One body, so a long-lived adapter fake cannot grow a private copy again.

    A tripwire, not a proof: it catches the copy-paste that produced both of
    GH-330's orphans, in the body's own spelling of the loop. Fakes that exit on
    their own are none of its business.

    A `.py` fake spans lines, so there the file is the unit, and the needle is
    the path the launch looks for. A `.zsh` fake is a `printf` of quoted lines
    the shell reads as one command, so there the unit is a logical line -- the
    four #373 copies were each one command whose path and loop never shared a
    physical line -- and the needle is the word `adapter`, because the path may
    sit in a helper (`write_fake_adapter`) rather than on the line that spells
    the loop.
    """
    assert fake_adapter.LOOP in fake_adapter.SERVE, 'the needle no longer spells the body'
    offenders = []
    for path in sorted([*ROOT.glob('*.py'), *ROOT.glob('*.zsh')]):
        if path.name in SUPPORT:
            continue
        text = path.read_text()
        if path.suffix == '.zsh':
            offenders += [f'{path.name}:{number}' for number, line in _logical_lines(text)
                          if 'adapter' in line and fake_adapter.LOOP in line]
        elif 'ccmorph-adapter' in text and fake_adapter.LOOP in text:
            offenders.append(path.name)
    assert offenders == []
