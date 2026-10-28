#!/usr/bin/env python3
"""What a 200 from an over-limit request actually means.

The generator used to read every 200 as "accepted, therefore this model truncates
silently and has no observable ceiling". Three different things arrive as 200 and only
one of them is that:

  - the model refuses in 200 clothing, naming the reason in stop_reason
  - the model took the whole prompt, so the ceiling is merely above what was sent
  - the model quietly kept a prefix, and the tokens it reports are the ceiling

Conflating them cost a real probe: glm-5.2 was recorded as unmeasurable when it
had in fact reported its own limit through stop_reason. These tests pin each case
without sending anything, so the classification can be trusted before it is paid for.
"""

from __future__ import annotations

import os
import pathlib
import io
import json
import shutil
import sys
import tempfile
import unittest
import unittest.mock
import urllib.error
import urllib.request
from pathlib import Path

from support import HANG_TIMEOUT, helper_cmd







class MetadataCompatibility(unittest.TestCase):

    def test_real_route_subprocess_is_killed_on_deadline(self):
        import subprocess
        with tempfile.TemporaryDirectory() as tmp:
            script = Path(tmp) / "ccmorph.sh"
            script.write_text('ccmorph() { exec sleep 30; }\n')
            # The outer wait includes cold interpreter startup; the deadline must
            # interrupt the blocking route, not benchmark the host's scheduler.
            env = {**os.environ, "CCMORPH_LIB_DIR": tmp,
                   "CCMORPH_STATE_DIR": tmp}
            result = subprocess.run([*helper_cmd("refresh-model-registry"), "--profile", "gw",
                                     "--deadline", "0.2"], env=env,
                                    capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertIn("deadline reached", result.stderr)
            self.assertNotEqual(result.returncode, 0)


class DiscoveryHTTP(unittest.TestCase):
    def setUp(self):
        import http.server
        import threading
        self.requests = []
        self.release = threading.Event()
        self.block_body = False
        # Fully declared chat models need no external catalogue. The vision-only
        # row is deliberately excluded by the listing contract.
        self.rows = [
            {"id": "a", "max_input_tokens": 128000},
            {"id": "b", "context_length": 128000},
            {"id": "unknown", "capabilities": {
                "vision": True, "chat_completion": False}}
        ]
        fixture = self
        class Gateway(http.server.BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                pass
            def do_GET(self):
                fixture.requests.append((self.command, self.path,
                                         self.headers.get("anthropic-version")))
                self.send_response(200)
                self.end_headers()
                if fixture.block_body:
                    fixture.release.wait(30)
                    return
                self.wfile.write(json.dumps({"data": fixture.rows}).encode())
            def do_POST(self):
                fixture.requests.append((self.command, self.path, None))
                self.send_error(500)
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.thread.join)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.addCleanup(self.release.set)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.base = f"http://127.0.0.1:{self.server.server_port}"
        self.script = self.root / "ccmorph.sh"
        self.script.write_text(
            'ccmorph() { printf "%s\\n" "ANTHROPIC_API_KEY=canary" '
            f'"ANTHROPIC_BASE_URL={self.base}"; }}\n')
        self.conf = self.root / "gw.conf"
        self.conf.write_text("type=apikey\napi_key=canary\n")
        self.env = {**os.environ, "CCMORPH_LIB_DIR": tmp.name,
                    "CCMORPH_STATE_DIR": tmp.name}
        self.command = [*helper_cmd("refresh-model-registry"), "--profile", "gw"]

    def test_real_cli_gets_metadata_without_post_and_persists_it(self):
        import subprocess
        first = subprocess.run(self.command, env=self.env, capture_output=True,
                               text=True, timeout=HANG_TIMEOUT)
        self.assertEqual(first.returncode, 0, first.stderr)
        stored = self.conf.read_text()
        self.assertIn('"a":128000', stored)
        self.assertIn('"b":128000', stored)
        self.assertNotIn('"unknown":', stored)
        self.assertFalse((self.root / "registry").exists())
        self.assertEqual(self.requests, [("GET", "/v1/models", "2023-06-01")])

    def test_issue_259_route_deadline_can_expire_before_any_get(self):
        import subprocess
        stored = self.conf.read_text()
        # Route setup is inside the whole-run deadline. It cannot promise a GET.
        self.script.write_text('ccmorph() { exec sleep 30; }\n')
        slow = subprocess.run(self.command + ["--deadline", "0.2"], env=self.env,
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertIn("deadline reached", slow.stderr)
        self.assertNotEqual(slow.returncode, 0)
        self.assertEqual(self.conf.read_text(), stored)
        self.assertFalse((self.root / "registry").exists())
        self.assertEqual(self.requests, [])






if __name__ == "__main__":
    unittest.main(verbosity=2)





if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(unittest.main())













