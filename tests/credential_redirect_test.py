"""Cross-origin redirects must not carry the API key (issue #10)."""
import contextlib
import http.server
import importlib.machinery
import importlib.util
import os
import socket
import subprocess
import sys
import threading
import time
import unittest
import urllib.request
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(os.environ.get("CCPROTEUS_LIB_DIR") or Path(__file__).resolve().parents[1])


def load(path, name):
    loader = importlib.machinery.SourceFileLoader(name, str(path))
    spec = importlib.util.spec_from_loader(name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


PROXY_VARS = ("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
              "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy")


def clean_env():
    """Environment with every proxy variable removed, so loopback fixtures are reached directly."""
    return {k: v for k, v in os.environ.items() if k not in PROXY_VARS}


def serve(handler):
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


@contextlib.contextmanager
def socket_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    yield port


def wait_port(port):
    deadline = time.monotonic() + HANG_TIMEOUT
    while time.monotonic() < deadline:
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.1)


class CredentialRedirect(unittest.TestCase):
    def _redirect_fixture(self):
        seen = []
        self.origin_hits = []
        origin_hits = self.origin_hits

        class Sink(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append({k.lower(): v for k, v in self.headers.items()})
                self.send_response(200)
                self.send_header("content-length", "11")
                self.end_headers()
                self.wfile.write(b'{"data":[]}')

            def log_message(self, *args):
                pass

        sink = serve(Sink)

        class Redirect(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                origin_hits.append(self.path)
                self.send_response(302)
                self.send_header("location", f"http://127.0.0.1:{sink.server_address[1]}/v1/models")
                self.send_header("content-length", "0")
                self.end_headers()

            def log_message(self, *args):
                pass

        origin = serve(Redirect)
        return seen, f"http://127.0.0.1:{origin.server_address[1]}"

    def test_registry_refuses_cross_origin_redirect_with_key(self):
        seen, base = self._redirect_fixture()
        # The module builds its opener at import, so clear proxies first.
        saved = {k: os.environ.pop(k) for k in PROXY_VARS if k in os.environ}
        self.addCleanup(os.environ.update, saved)
        registry = load(ROOT / "refresh-model-registry", "rmr_redirect")
        try:
            registry.list_models(base, "sk-REDIRECT-CANARY")
        except Exception:
            pass
        self.assertTrue(self.origin_hits, "the request never reached the fixture")
        self.assertEqual([h for h in seen if "sk-REDIRECT-CANARY" in str(h)], [],
                         "refresh-model-registry forwarded the key to another origin")

    def test_shim_refuses_cross_origin_redirect_with_key(self):
        seen, base = self._redirect_fixture()
        with socket_port() as port:
            shim = subprocess.Popen([sys.executable, str(ROOT / "gateway-shim"), str(port), base],
                                    stdout=subprocess.DEVNULL, env=clean_env())
            try:
                wait_port(port)
                req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/models",
                                             headers={"x-api-key": "sk-REDIRECT-CANARY"})
                try:
                    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                    opener.open(req, timeout=HANG_TIMEOUT).read()
                except Exception:
                    pass
            finally:
                shim.kill()
        self.assertTrue(self.origin_hits, "the request never reached the fixture")
        self.assertEqual([h for h in seen if "sk-REDIRECT-CANARY" in str(h)], [],
                         "gateway-shim forwarded the key to another origin")


if __name__ == "__main__":
    unittest.main()
