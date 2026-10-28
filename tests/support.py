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


SHIM = Path(__file__).resolve().parent.parent / "libexec/gateway-shim"
# The adapter under test: the Python script (default) or the Go port,
# `CCMORPH_TEST_ADAPTER=go` (needs `make shim`). The same black-box tests run
# against both; ADAPTER_CMD is the argv prefix, followed by the profile.
CORE = Path(__file__).resolve().parent.parent / "libexec/ccmorph-core"
ADAPTER_IMPL = os.environ.get("CCMORPH_TEST_ADAPTER", "python")
ADAPTER_PY = Path(__file__).resolve().parent.parent / "libexec/ccmorph-adapter"
# Module-level `pytestmark = needs_shim` skips a file that spawns the compiled shim.
# pytest is imported here, not at the top: the zsh suite imports this module
# for HANG_TIMEOUT under a python3 that has no pytest.
def adapter_cmd(binary=None):
    """argv prefix that runs the adapter under test, optionally from a copied `binary`."""
    import sys
    if ADAPTER_IMPL == "go":
        return [str(binary or CORE), "adapter"]
    return [sys.executable, str(binary or ADAPTER_PY)]


def __getattr__(name):
    if name == "needs_shim":
        import pytest
        return pytest.mark.skipif(not SHIM.exists(), reason=f"{SHIM} is not built: run `make shim` first")
    raise AttributeError(name)
