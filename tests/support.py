"""The suite's shared hang-detection bound; imported by the tests that wait on a real process."""
import os
from pathlib import Path


# A hang detector, not a performance bound: the longest any test waits on a
# spawned process, sized so only a real hang reaches it. It sits above pytest.ini's
# 60 s per-test cap so that no site is stricter than the literal it replaced: the
# cap reports a single hung wait first, with a thread dump, while this bound holds
# the waits the cap cannot see — a test that polls a spawned process several times
# over, where the cap would fire in the middle of the polling.
# Override with CCMORPH_TEST_HANG_TIMEOUT.
HANG_TIMEOUT = float(os.environ.get("CCMORPH_TEST_HANG_TIMEOUT", "120"))
# Source for a fake HTTP server the tests write out and run as its own process,
# placed after its http.server import. HTTPServer.server_bind resolves 127.0.0.1
# with socket.getfqdn, a reverse DNS lookup that can stall for tens of seconds on
# macOS; conftest.py patches that away for in-process servers, and a subprocess
# needs the same line itself. No fake here uses the name it would find.
FAKE_SERVER_NO_DNS = "import socket\nsocket.getfqdn = lambda name='': name\n"


# The compiled core (`make shim`): adapter, shim and helper verbs are its subcommands.
CORE = Path(__file__).resolve().parent.parent / "libexec/ccmorph-core"
# argv prefix of the stock shim.
SHIM = [str(CORE), "shim"]
# Module-level `pytestmark = needs_shim` skips a file that spawns the compiled shim.
# pytest is imported here, not at the top: the zsh suite imports this module
# for HANG_TIMEOUT under a python3 that has no pytest.
def adapter_cmd(binary=None):
    """argv prefix that runs the adapter under test, optionally from a copied `binary`."""
    return [str(binary or CORE), "adapter"]


def helper_cmd(name, binary=None):
    """argv prefix that runs the helper `name` under test, optionally from a copied `binary`."""
    return [str(binary or CORE), name]


def __getattr__(name):
    if name == "needs_shim":
        import pytest
        return pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
    raise AttributeError(name)
