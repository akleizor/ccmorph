"""The suite's shared hang-detection bound; imported by the tests that wait on a real process."""
import os
import random
import shlex
import shutil
import socket
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


def anthropic_accepts_the_key(bindir):
    """Write bindir/curl for a test that launches a native apikey profile.

    Such a launch asks Anthropic whether its key works before the session opens
    (#255). A test's key is a placeholder Anthropic would reject, and a test must
    not reach the network to find that out: this curl answers a request to
    api.anthropic.com as Anthropic answers a working key (HTTP 200, an empty
    model list) and hands every other request to the real curl, so a test's own
    gateway or loopback server is reached exactly as before."""
    bindir = Path(bindir)
    elsewhere = os.pathsep.join(p for p in os.environ.get("PATH", "").split(os.pathsep)
                                if p and Path(p).resolve() != bindir.resolve())
    real = shutil.which("curl", path=elsewhere) or "/usr/bin/curl"
    curl = bindir / "curl"
    curl.write_text(
        "#!/bin/sh\n"
        "for a do case \"$a\" in https://api.anthropic.com/*)\n"
        "  printf '%s' '{\"data\":[]}'\n"
        "  for b do case \"$b\" in *http_code*) printf '\\n__CCMORPH_STATUS__200' ;; esac; done\n"
        "  exit 0 ;; esac; done\n"
        f"exec {shlex.quote(real)} \"$@\"\n")
    curl.chmod(0o755)


def port_outside_the_ephemeral_pool() -> int:
    """A loopback port number for a test whose subject binds a port the test
    chose — the adapter's --port, a shim's listen port, a route that must stay
    dead — where no parallel process can be handed the number while the test
    waits for that bind. A number drawn by binding port 0 comes from the OS's
    ephemeral range (49152-65535 on macOS, 32768-60999 on Linux), the same
    system-wide pool every parallel gate process draws from, so between the
    draw and the bind another worker's listener, shim or outbound source port
    can take it and a full gate fails a test that is green alone (#176, #188).
    A number below every range's floor cannot be drawn by a bind-to-zero at
    all; the probe skips the fixed services that do live down there, and a
    bind that loses its number draws again."""
    for _ in range(64):
        p = random.randrange(1024, 32768)
        s = socket.socket()
        try:
            s.bind(("127.0.0.1", p))
        except OSError:
            continue                       # a fixed service holds the number
        finally:
            s.close()
        return p
    raise RuntimeError("no port below the ephemeral ranges is free on 127.0.0.1")


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
