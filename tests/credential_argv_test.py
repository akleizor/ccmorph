"""Issue #9: the API key must never appear on a process command line."""
import http.server
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path

from support import HANG_TIMEOUT, home_env, needs_loopback, wait_until

ROOT = Path(__file__).resolve().parents[1]


def serve(handler):
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


class Slow(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        time.sleep(2.5)
        body = b'{"data":[{"id":"m1"}]}'
        self.send_response(200)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


@needs_loopback  # the fake gateway is a real listener: #611
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
                proc = subprocess.Popen([str(ROOT / "bin/ccmorph"), *verb], env=env,
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                seen, samples = False, 0

                def finished(proc=proc):
                    nonlocal seen, samples
                    if proc.poll() is not None:
                        return True
                    ps = subprocess.run(["ps", "-axo", "args="], capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout
                    # A sample counts only if the child was still alive when ps
                    # returned: one taken after it exited proves nothing (#636).
                    if proc.poll() is None:
                        samples += 1
                    seen = seen or key in ps
                    return False
                if not wait_until(finished, HANG_TIMEOUT):
                    proc.kill()
                    proc.wait(timeout=HANG_TIMEOUT)
                    self.fail(f"`ccmorph {verb[0]}` did not exit within the hang bound")
                self.assertGreater(samples, 0, f"`ccmorph {verb[0]}` exited before ps sampled it")
                self.assertFalse(seen, f"`ccmorph {verb[0]}` put the API key on a process command line")
        server.shutdown()


if __name__ == "__main__":
    unittest.main()
