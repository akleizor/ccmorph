"""The pty deadline must not turn a busy machine into a suite failure (#365).

`run_under_a_terminal` (tests/ccproteus_test.zsh) runs a whole suite under a pty
and kills it at a deadline. On a loaded machine that child needs minutes, so a
fixed wall-clock deadline kills a healthy run and the parent test fails with
output that stops mid-line. These cases drive the deadline's `wait_for` through a
child on fake time, so nothing here sleeps or waits on a clock.
"""
import math
import os
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import under_a_terminal  # noqa: E402


class Child:
    """A child process on fake time: it exits once it has waited `needs` seconds."""

    def __init__(self, needs):
        self.needs = needs
        self.waited = 0.0

    # The contract of subprocess.Popen.wait(timeout=...): the exit status when the
    # child finishes inside the timeout, and a TimeoutExpired when it does not.
    def wait(self, timeout=None):
        if self.waited + timeout >= self.needs:
            self.waited = self.needs
            return 0
        self.waited += timeout
        raise subprocess.TimeoutExpired("child", timeout)


@pytest.mark.parametrize("busy", [
    pytest.param(lambda child: 10.0, id="busy-throughout-issue-365"),
    pytest.param(lambda child: 1.0 if child.waited < 20 else 20.0, id="busy-after-20s-issue-365"),
])
def test_a_busy_machine_does_not_spend_the_deadline(monkeypatch, busy):
    # A child needing 120 s of waiting finishes while the machine is busy: the
    # deadline is idle-machine seconds, so heavy load slows the clock, not the
    # child. `raising=False` keeps this an assertion failure rather than an error
    # on a runner without the load check.
    child = Child(needs=120)
    monkeypatch.setattr(under_a_terminal, "machine_busy", lambda: busy(child), raising=False)
    assert under_a_terminal.wait_for(child) == 0


def test_a_hung_child_on_an_idle_machine_is_still_stopped(monkeypatch):
    # The #222 guard: a fixture waiting on terminal input must still be killed,
    # at about the budget, so a hang cannot hold the suite open.
    monkeypatch.setattr(under_a_terminal, "machine_busy", lambda: 1.0, raising=False)
    child = Child(needs=math.inf)
    assert under_a_terminal.wait_for(child) is None
    assert child.waited == pytest.approx(45, abs=1)


@pytest.mark.parametrize("loadavg, cores, busy", [
    pytest.param(0.5, 18, 1.0, id="idle-machine-issue-365"),
    pytest.param(4.0, 4, 1.0, id="one-runnable-per-core-issue-365"),
    pytest.param(137.0, 18, 137.0 / 18, id="a-loaded-fleet-issue-365"),
    pytest.param(2.0, None, 2.0, id="a-host-that-cannot-count-cores-issue-365"),
])
def test_the_load_reading_is_runnable_processes_per_core(monkeypatch, loadavg, cores, busy):
    # What the budget is charged against. The cases above hold this reading still;
    # this one is the reading itself, so a change to it cannot pass unnoticed:
    # the three windows differ, so reading the five- or fifteen-minute average
    # instead of the one-minute one is a failure here.
    monkeypatch.setattr(os, "getloadavg", lambda: (loadavg, loadavg * 10, loadavg * 100))
    monkeypatch.setattr(os, "cpu_count", lambda: cores)
    assert under_a_terminal.machine_busy() == pytest.approx(busy)


def test_an_unreadable_load_keeps_the_strict_deadline(monkeypatch):
    # A host with no load average must not break the runner: the clock stays at
    # 1, which is the fixed deadline this replaces.
    def unreadable():
        raise OSError("no load average here")

    monkeypatch.setattr(os, "getloadavg", unreadable)
    assert under_a_terminal.machine_busy() == 1.0


def test_the_launcher_suite_has_one_pty_runner():
    # A pty launch in the launcher suite goes through run_under_a_terminal, so
    # it gets this file's load-aware deadline, the drain and the process-group
    # kill. An inline runner keeps a fixed wall-clock kill and fails a healthy
    # launch on a busy machine (#421).
    suite = (Path(__file__).resolve().parent / "ccproteus_test.zsh").read_text()
    assert "subprocess.Popen" not in suite
