"""Run a command under a pty and print what the terminal saw.

`run_under_a_terminal` in `tests/ccmorph_test.zsh` calls this. A test cannot
fake `-t`, so a terminal is the only way to reproduce the launcher's own
condition for asking a model-less routed route to pick a model. The pty is
drained (the launch can out-talk the terminal buffer) and the child is killed at
the deadline, so a fixture that waits on input cannot hang the suite again. The
deadline is spent in idle-machine seconds, so a machine running the whole fleet
slows it instead of failing the case (#365).

A test that has to answer a menu, or end the terminal's input while the child
still runs, adds the flags below before the command (#1030): stdin then gets a
pty of its own and stdout/stderr their own, so the input can be closed the way a
closed terminal closes it while the output is still read. The marker waits spend
their budget on the same load-aware clock.
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


class Script:
    """What the scripted mode does while the child runs: answer each marker in
    order, then close the input at the last one when the test asks for it."""

    def __init__(self, answers, end_input_at):
        self.answers = answers  # [(marker, text)], in the order they appear
        self.end_input_at = end_input_at


def parse_script(argv):
    # The runner's own flags stand before the command; no flags is the plain
    # mode every other caller uses, and the command is passed through whole.
    answers = []
    end_input_at = ""
    i = 0
    while i < len(argv):
        if argv[i] == "--answer" and i + 2 < len(argv):
            answers.append((argv[i + 1], argv[i + 2]))
            i += 3
        elif argv[i] == "--end-input-at" and i + 1 < len(argv):
            end_input_at = argv[i + 1]
            i += 2
        else:
            break
    if not answers and not end_input_at:
        return None, argv
    return Script(answers, end_input_at), argv[i:]


def wait_for_output(p, chunks, marker, budget=45.0, tick=0.1):
    # A marker wait is a wait like any other: the budget is idle-machine
    # seconds, and a child that exits before its marker answers at once.
    needle = marker.encode()
    spent = 0.0
    while spent < budget:
        if needle in b"".join(chunks):
            return True
        try:
            p.wait(timeout=tick)
            return needle in b"".join(chunks)
        except subprocess.TimeoutExpired:
            spent += tick / machine_busy()
    return False


def stop(p):
    try:
        os.killpg(os.getpgid(p.pid), signal.SIGKILL)
    except OSError:
        p.kill()
    p.wait()


def run_scripted(script, command):
    in_m, in_s = pty.openpty()
    out_m, out_s = pty.openpty()
    chunks = []
    p = None
    in_open = True
    try:
        # Its own session, so the deadline below can take the whole process
        # group, and two ptys: closing the input must end the child's stdin
        # without ending the output the test reads.
        p = subprocess.Popen(command, stdin=in_s, stdout=out_s, stderr=out_s,
                             start_new_session=True)
        os.close(in_s)
        in_s = -1
        os.close(out_s)
        out_s = -1
        reader = threading.Thread(target=drain, args=(out_m, chunks), daemon=True)
        reader.start()
        rc = 0
        for marker, text in script.answers:
            if not wait_for_output(p, chunks, marker):
                rc = 124
                break
            # Answers are ASCII with Python escapes for the keys and newlines a
            # menu reads: "3\n" and "\x1b".
            os.write(in_m, text.encode("ascii").decode("unicode_escape").encode("latin-1"))
        if rc == 0 and in_open and script.end_input_at:
            if wait_for_output(p, chunks, script.end_input_at):
                os.close(in_m)
                in_open = False
            else:
                rc = 124
        if rc == 0:
            rc = wait_for(p)
            if rc is None:
                rc = 124
        if rc == 124:
            stop(p)
        reader.join(timeout=2)
    finally:
        # Both pty ends belong to this process, and a child still standing here is
        # ours to end, so neither outlives the call. A failure is not swallowed by
        # any of it: it leaves with its traceback and its exit status.
        if p is not None and p.poll() is None:
            p.kill()
            p.wait()
        if in_s != -1:
            os.close(in_s)
        if out_s != -1:
            os.close(out_s)
        if in_open:
            os.close(in_m)
        os.close(out_m)
    sys.stdout.write(b"".join(chunks).decode("utf-8", "replace"))
    sys.stdout.flush()
    return rc


def run_plain(command):
    m, s = pty.openpty()
    chunks = []
    p = None
    try:
        # Its own session, so the deadline below can take the whole process group: a
        # killed launch must not leave a stubbed adapter looping behind it.
        p = subprocess.Popen(command, stdin=s, stdout=s, stderr=s, start_new_session=True)
        os.close(s)
        s = -1
        reader = threading.Thread(target=drain, args=(m, chunks), daemon=True)
        reader.start()
        rc = wait_for(p)
        if rc is None:
            stop(p)
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


def main(argv):
    script, command = parse_script(argv)
    if script is not None:
        return run_scripted(script, command)
    return run_plain(command)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))