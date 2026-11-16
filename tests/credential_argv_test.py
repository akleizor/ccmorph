"""Issue #9: the API key must never appear on a process command line."""
import http.server
import os
import re
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
# How many times the scan must catch the child in flight before the held request
# answers. Three is enough to cover a key handed to a child a few execs downstream
# and still costs a few hundred milliseconds instead of a fixed hold.
SIGHTINGS = 3


def serve(handler):
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    # Set by the test when it has looked often enough; the handler answers on it.
    server.held = threading.Event()
    server.hits = 0
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


class Slow(http.server.BaseHTTPRequestHandler):
    """Answers only when the sampler has let it: the child stays in flight for as
    long as the test is looking at it, and no longer. A fixed hold had to be sized
    for the slowest machine, and on a quick one it was pure waiting."""

    def do_GET(self):
        self.server.hits += 1
        self.server.held.wait(HANG_TIMEOUT)
        body = b'{"data":[{"id":"m1"}]}'
        self.send_response(200)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def home_env(tmp, **extra):
    home = Path(tmp)
    state = home / ".config/ccmorph"
    state.mkdir(parents=True, exist_ok=True)
    (home / ".claude/projects").mkdir(parents=True, exist_ok=True)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH", "_CCMORPH", "CLAUDE_", "ANTHROPIC_", "AWS_"))}
    env.update(HOME=tmp, XDG_CONFIG_HOME=str(home / ".config"), TMPDIR=tmp,
               CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"),
               LANG="C.UTF-8", LC_ALL="C.UTF-8", **extra)
    return env, state


class CredentialArgv(unittest.TestCase):
    def test_key_never_on_a_command_line(self):
        """.github/SECURITY.md: 'No credential on a command line ... not in ps output.'"""
        server = serve(Slow)
        key = "sk-ARGV-CANARY-123456"
        with tempfile.TemporaryDirectory() as tmp:
            env, state = home_env(tmp)
            (state / "gw.conf").write_text(
                f"type=apikey\napi_key={key}\nbase_url=http://127.0.0.1:{server.server_address[1]}\n")
            for verb in (["models", "gw"], ["doctor", "gw"]):
                server.held.clear()
                server.hits = 0
                proc = subprocess.Popen([str(ROOT / "bin/ccmorph"), *verb], env=env,
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                seen = False
                sightings = 0
                deadline = time.monotonic() + HANG_TIMEOUT
                try:
                    while proc.poll() is None and time.monotonic() < deadline:
                        ps = subprocess.run(["ps", "-axo", "pid=,args="], capture_output=True,
                                            text=True, timeout=HANG_TIMEOUT).stdout
                        seen = seen or key in ps
                        if any(line.split(None, 1)[:1] == [str(proc.pid)] for line in ps.splitlines()):
                            sightings += 1
                            # Looked while it ran, so let the held request answer.
                            if sightings >= SIGHTINGS:
                                server.held.set()
                finally:
                    server.held.set()          # never hold a child past its own scan
                if proc.poll() is None:
                    proc.kill()
                    proc.wait(timeout=HANG_TIMEOUT)
                    self.fail(f"`ccmorph {verb[0]}` did not exit within the hang bound")
                self.assertFalse(seen, f"`ccmorph {verb[0]}` put the API key on a process command line")
                # The scan is only evidence if it caught the child in flight. A verb
                # that never reached the gateway had nothing in flight to catch.
                self.assertTrue(sightings >= SIGHTINGS or server.hits == 0,
                                f"`ccmorph {verb[0]}` sent {server.hits} request(s) but the "
                                f"scan caught it running {sightings} time(s): the scan proved nothing")
        server.shutdown()


if __name__ == "__main__":
    unittest.main()
