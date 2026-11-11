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

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parent
# The argv `_ccm_adapter_start` builds for a selected launch, minus the parent pid.
LAUNCH_ARGV = ['gw', '--auto', '--model', 'claude-sonnet-4-5']
SUPPORT = ('fake_adapter.py', 'fake_adapter_serve.sh', Path(__file__).name)


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


def test_the_fake_ends_when_its_own_launcher_is_gone_gh_434(tmp_path):
    """An orphaned fake has nothing left to serve, whatever the pid it names.

    GH-434: a launcher killed outright runs no trap, so the only reap is the
    --parent-pid poll -- and a recycled pid names a live process long after the
    launcher it belonged to is gone. Orphaned to PID 1 under a parent that
    never dies, the fake served forever, holding the launcher's fifo write end
    open so the suite's readiness read waited for an EOF at zero CPU and the
    whole suite hung. The starter here exits at once, orphaning the fake, and
    the bystander is the recycled pid the fake keeps trusting.
    """
    bystander = subprocess.Popen(['/bin/sleep', '300'])
    try:
        starter = subprocess.Popen(
            ['/bin/sh', '-c', '"$1" gw --auto --model claude-sonnet-4-5 --parent-pid "$2" &',
             '_', _install(tmp_path), str(bystander.pid)])
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
    finally:
        fake_adapter.reap(tmp_path / 'ccmorph-adapter')
        _reap(bystander, starter)


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
