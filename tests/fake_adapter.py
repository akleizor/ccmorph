"""The fake adapter the launch tests install in an isolated state dir.

The real adapter is reaped by `--parent-pid`: `check_parent` in
`ccmorph-adapter` polls the pid it was started with and shuts down when it is
gone. That is the only thing that can reap it, because the launcher's own
teardown is a trap, and a launcher killed outright -- SIGKILL runs no trap --
leaves nothing else watching. The body here polls the same pid, for the same
reason: a fake that ignored the flag outlived its test, reparented to PID 1,
forking `sleep` five times a second for hours after pytest had deleted the
temporary home it was serving, and the load landed on every later run on that
host (GH-330).

The poll cannot cover everything, though: a pid is recycled, so the process the
fake watches can be alive again long before the fake notices anything -- on the
mini one launcher's number had been handed to a live simulator process, and
1,124 wrappers were still serving from deleted sandboxes because of it (#347).
So the fake also records its own pid beside itself, and `reap` kills that exact
pid: a teardown does not have to trust the launcher's number to mean what it
did at launch.

One body for the long-lived fake, so that contract is stated once. The stubs
that exit on their own -- `cli_messages_test.py`, `launch_latency_test.py`, the
`rc_bridge_nested_launch_test.py` pair -- keep their own. `fake_adapter_serve.sh`
is that one body: the zsh suite appends the same fragment to its own fakes, so
the loop cannot grow a second copy in either suite (#373).
"""

import os
from pathlib import Path
import signal
import subprocess

# The tail every long-lived fake ends with: read the --parent-pid it was started
# with, then serve until that pid is gone, the way `check_parent` does.
SERVE = Path(__file__).with_name('fake_adapter_serve.sh').read_text()

# The serve fragment's unconditional loop, in the spelling the guard in
# fake_adapter_contract_test.py looks for. That guard checks this needle against
# SERVE, so the loop and the needle cannot drift apart and leave it matching
# nothing.
LOOP = 'while :; do'


def script(ready_line: str) -> str:
    """The body: trap, record itself, announce readiness, then the shared tail.

    Readiness comes last, as it does in the real adapter, where the signals are
    registered before `PORT=` is printed: a reader that sees the ready line can
    then stop the process and get the handler's status rather than dying to the
    default disposition in the window before the trap was installed. The pid
    goes down before it, so a reader that saw the ready line can trust the
    record, and one that never did is not told a pid it cannot act on.
    """
    return ('#!/bin/sh\ntrap "exit 0" TERM INT\n'
            'echo $$ > "$0.pid"\n'
            + ready_line + '\n' + SERVE)


def reap(adapter: Path) -> bool:
    """Kill the fake that recorded itself beside `adapter`; say whether it did.

    Returns whether it signalled anything, so a test can pin the decision
    without racing the signal. A missing, unreadable or unparsable record, a pid
    no process holds, a pid whose command line is not this fake's, and a pid
    that died between the check and the kill all answer False -- a teardown that
    leaks at worst, and never signals a process this suite did not start.

    What is signalled is a pid that was still this sandbox's fake at the instant
    of the check, which is the strongest thing a pid allows: the check and the
    kill are adjacent syscalls, and a pid recycled into the gap between them is
    the one case this cannot see.
    """
    try:
        pid = int(Path(f'{adapter}.pid').read_text().strip())
    except (OSError, ValueError):
        return False
    if pid <= 0:
        return False
    try:
        # One syscall, and only for a pid that is really running: nobody holds
        # the pid once the fake has exited on its own, which is the ordinary
        # case. OverflowError is a record no process can have; OSError is one
        # nobody holds.
        os.kill(pid, 0)
    except (OSError, OverflowError):
        return False
    # A number is not ownership: it may name a stranger's process since the
    # launch, so require the command line the launch gives the fake.
    if f'{adapter} ' not in _command_of(pid):
        return False
    try:
        os.kill(pid, signal.SIGKILL)
    except (OSError, OverflowError):
        return False
    return True


def _command_of(pid: int) -> str:
    """The command line of one pid, or "" when `ps` cannot answer for it.

    A missing ps answers "" like a missing pid: the reap then does nothing,
    which leaks at worst and never signals a process the sandbox does not own.
    """
    try:
        out = subprocess.run(['/bin/ps', '-o', 'command=', '-p', str(pid)],
                             capture_output=True, text=True, errors='replace')
    except OSError:
        return ''
    return out.stdout if out.returncode == 0 else ''
