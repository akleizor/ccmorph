"""The suite's shared hang-detection bound; imported by the tests that wait on a real process."""
import atexit
import os
import random
import shlex
import shutil
import socket
import tempfile
from pathlib import Path


# A hang detector, not a performance bound: the longest any test waits on a
# spawned process, sized so only a real hang reaches it. It sits above pytest.ini's
# 60 s per-test cap so that no site is stricter than the literal it replaced: the
# cap reports a single hung wait first, with a thread dump, while this bound holds
# the waits the cap cannot see — a test that polls a spawned process several times
# over, where the cap would fire in the middle of the polling.
# Override with CCMORPH_TEST_HANG_TIMEOUT.
HANG_TIMEOUT = float(os.environ.get("CCMORPH_TEST_HANG_TIMEOUT", "120"))
# The one sentence, with the one link, for a claude no PATH search can find
# (#282). The installer, `ccmorph run` and `ccmorph doctor` print these same
# bytes, so a missing Claude Code meets a person with one answer wherever it
# finds them; the install suite ties the installer's copy to the core binary.
CLAUDE_MISSING = ("Claude Code is not installed or not on the PATH — install it: "
                  "https://docs.anthropic.com/en/docs/claude-code/setup")

# Source for a fake HTTP server the tests write out and run as its own process,
# placed after its http.server import. HTTPServer.server_bind resolves 127.0.0.1
# with socket.getfqdn, a reverse DNS lookup that can stall for tens of seconds on
# macOS; conftest.py patches that away for in-process servers, and a subprocess
# needs the same line itself. No fake here uses the name it would find.
FAKE_SERVER_NO_DNS = "import socket\nsocket.getfqdn = lambda name='': name\n"

# Tools whose real copy on the host must not answer a test that seeded neither:
# the picker falls back to fzf when it finds one, and the add menu reads its
# provider rows from cliproxyapi on PATH, so a machine that carries either
# decides rows and widths the test never asked for (#414). A test that needs
# one writes its own stub, in a directory in front of this PATH.
HERMETIC_TOOLS = ("fzf", "cliproxyapi")

# One mirror directory per (real directory, hidden names), made once per process:
# every test env that hides the same tools from the same directory reuses it.
_MIRRORED_PATHS = {}


def _mirror_without(entry, hidden):
    """A directory linking every executable in `entry` except the `hidden` names."""
    try:
        real = Path(entry).resolve()
    except OSError:
        return None
    key = (str(real), frozenset(hidden))
    if key in _MIRRORED_PATHS:
        return _MIRRORED_PATHS[key]
    try:
        names = os.listdir(real)
    except OSError:
        return None
    target = Path(tempfile.mkdtemp(prefix="ccmorph-path-"))
    atexit.register(shutil.rmtree, str(target), True)
    for name in names:
        if name in hidden:
            continue
        source = real / name
        try:
            if source.is_file() and os.access(source, os.X_OK):
                (target / name).symlink_to(source)
        except OSError:
            continue
    _MIRRORED_PATHS[key] = str(target)
    return str(target)


def path_without(*tools, path=None):
    """PATH minus `tools` (default: HERMETIC_TOOLS), keeping every other tool.

    An entry carrying one of `tools` is replaced by a directory of links to its
    other executables: dropping the entry whole would also drop zsh and the
    coreutils a test's shell needs, and a merged-/usr Linux keeps both fzf and
    zsh in /usr/bin (#414). The caller puts its own stub directory first, in
    front of this result."""
    entries = (os.environ.get("PATH", "") if path is None else path).split(os.pathsep)
    hidden = tools or HERMETIC_TOOLS
    kept, seen = [], set()
    for entry in entries:
        if not entry:
            continue
        if any((Path(entry) / tool).exists() for tool in hidden):
            # A relative entry cannot be mirrored from a test's directory and
            # then mean the same thing to the child; hiding wins.
            if not os.path.isabs(entry):
                continue
            entry = _mirror_without(entry, hidden)
            if entry is None:
                continue
        if entry in seen:
            continue
        seen.add(entry)
        kept.append(entry)
    return os.pathsep.join(kept)


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
