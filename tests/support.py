"""The suite's shared hang-detection bound; imported by the tests that wait on a real process."""
import os

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


def shim_cmd(path):
    """The argv prefix that starts gateway-shim: the Go binary named by CCMORPH_SHIM_BIN
    when set, else the Python script at `path`. Lets the black-box shim tests run both ways."""
    import sys
    binary = os.environ.get("CCMORPH_SHIM_BIN")
    return [binary] if binary else [sys.executable, str(path)]
