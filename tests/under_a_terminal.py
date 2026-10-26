"""Run a command under a pty and print what the terminal saw.

`run_under_a_terminal` in `tests/ccproteus_test.zsh` calls this. A test cannot
fake `-t`, so a terminal is the only way to reproduce the launcher's own
condition for asking a model-less routed route to pick a model. The pty is
drained (the launch can out-talk the terminal buffer) and the child is killed at
the deadline, so a fixture that waits on input cannot hang the suite again. The
deadline is spent in idle-machine seconds, so a machine running the whole fleet
slows it instead of failing the case (#365).
"""
import os
import pty
import signal
import subprocess
import sys
import threading


def drain(m, chunks):
    while True:
        try:
            data = os.read(m, 4096)
        except OSError:
            return
        if not data:
            return
        chunks.append(data)


def machine_busy():
    # Runnable processes per core over the last minute; 1 on an idle machine.
    try:
        return max(1.0, os.getloadavg()[0] / (os.cpu_count() or 1))
    except OSError:
        return 1.0


def wait_for(p, budget=45.0, tick=1.0):
    # The budget is idle-machine seconds: on a machine with N runnable processes
    # per core a second of waiting spends 1/N of it, re-read every tick, so a busy
    # fleet slows this clock instead of failing the case (#365), and a hung child
    # on an idle machine is still stopped at about `budget`.
    spent = 0.0
    while spent < budget:
        try:
            return p.wait(timeout=tick)
        except subprocess.TimeoutExpired:
            spent += tick / machine_busy()
    return None


def main(argv):
    m, s = pty.openpty()
    chunks = []
    p = None
    try:
        # Its own session, so the deadline below can take the whole process group: a
        # killed launch must not leave a stubbed adapter looping behind it.
        p = subprocess.Popen(argv, stdin=s, stdout=s, stderr=s, start_new_session=True)
        os.close(s)
        s = -1
        reader = threading.Thread(target=drain, args=(m, chunks), daemon=True)
        reader.start()
        rc = wait_for(p)
        if rc is None:
            try:
                os.killpg(os.getpgid(p.pid), signal.SIGKILL)
            except OSError:
                p.kill()
            p.wait()
            rc = 124
        reader.join(timeout=2)
    finally:
        # Both pty ends belong to this process, and a child still standing here is
        # ours to end, so neither outlives the call. A failure is not swallowed by
        # any of it: it leaves with its traceback and its exit status.
        if p is not None and p.poll() is None:
            p.kill()
            p.wait()
        if s != -1:
            os.close(s)
        os.close(m)
    sys.stdout.write(b"".join(chunks).decode("utf-8", "replace"))
    sys.stdout.flush()
    return rc


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
