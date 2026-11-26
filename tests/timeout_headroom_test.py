"""GH-932: the 60 s ceiling is a verdict about the host, not a hang.

A night `make gate` on a saturated Mac killed nine spawn-bound tests that each
run in under 3 s alone. Each death surfaced as `[gwN] node down: Not properly
terminated` followed by `worker 'gwN' crashed while running '<id>'` — the
thread-method shape tests/conftest.py selects for xdist workers (#892) — with
no traceback and no timeout text. The children had finished or were still
completing; the scheduler had starved them past the per-test ceiling.

This file pins both halves of the fix: the global ceiling in pytest.ini stays
60 s, and a per-test `@pytest.mark.timeout(300)` keeps its own budget on an
xdist worker even when the command line sets a lower one — for a plain function
and for a `unittest.TestCase` method, the two shapes the marked tests use.
"""
import os
import re
import shutil
import subprocess
import sys
import time
import unittest
from pathlib import Path

import pytest

from support import HANG_TIMEOUT


def test_the_suite_ceiling_stays_sixty_seconds(request):
    assert float(request.config.getini("timeout")) == 60


# GH-932: the probe spawns a pytest session and an xdist worker, so it is
# bound by process starts under the gate's load; its children are the pytest
# interpreter and the worker it starts.
@pytest.mark.timeout(300)
def test_a_marked_test_keeps_its_budget_where_the_ceiling_kills_an_unmarked_one(tmp_path):
    pytest.importorskip("xdist")
    # The real conftest is what selects the thread method on a worker; copy it
    # so the probe reproduces the night shape and not a bare pytest.
    shutil.copy(Path(__file__).parent / "conftest.py", tmp_path / "conftest.py")
    (tmp_path / "pytest.ini").write_text("[pytest]\ntimeout = 60\n")
    (tmp_path / "probe_test.py").write_text(
        "import time\n"
        "import unittest\n"
        "\n"
        "import pytest\n"
        "\n"
        "\n"
        "def test_unmarked_past_the_ceiling():\n"
        "    time.sleep(30)\n"
        "\n"
        "\n"
        "@pytest.mark.timeout(30)\n"
        "def test_marked_past_the_ceiling():\n"
        "    time.sleep(2)\n"
        "\n"
        "\n"
        "class MarkedCase(unittest.TestCase):\n"
        "    @pytest.mark.timeout(30)\n"
        "    def test_marked_method_past_the_ceiling(self):\n"
        "        time.sleep(2)\n"
    )
    # A parent pytest's own env would steer the child (`PYTEST_ADDOPTS` from
    # the gate, `PYTEST_CURRENT_TEST` from this test); the probe owns its run.
    env = {k: v for k, v in os.environ.items() if not k.startswith("PYTEST_")}
    proc = subprocess.run(
        [sys.executable, "-m", "pytest", "-n1", "--timeout=1",
         "-p", "no:cacheprovider", "-rfEp", "probe_test.py"],
        cwd=tmp_path, env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    out = proc.stdout + proc.stderr
    assert proc.returncode == 1, out
    assert re.search(r"\[gw\d+\] node down: Not properly terminated", out), out
    assert re.search(
        r"worker 'gw\d+' crashed while running "
        r"'probe_test\.py::test_unmarked_past_the_ceiling'", out), out
    assert "PASSED probe_test.py::test_marked_past_the_ceiling" in out, out
    assert "PASSED probe_test.py::MarkedCase::test_marked_method_past_the_ceiling" in out, out
    assert "1 failed, 2 passed" in out, out
