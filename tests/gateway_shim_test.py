#!/usr/bin/env python3
"""Tests for gateway-shim against a stub upstream.

The upstream stub reproduces the observed gateway misbehavior —
deterministic tool_use ids (Bash_0 on every response, SSE and plain JSON)
— and the tests assert the shim rewrites them unique while passing
everything else byte-identical.

The sidecar tests at the end pin the metadata-only usage rows: exact hashed
response ids, whitelisted buckets, the incomplete-then-complete stream
lifecycle, and that neither an unwritable state directory nor a dead client
changes a single relayed byte.
"""

import contextlib
import datetime
import gzip
import hashlib
import http.client
import importlib.machinery
import importlib.util
import io
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

from support import HANG_TIMEOUT

REPO = Path(__file__).resolve().parent.parent
SHIM = REPO / "libexec/gateway-shim"

SSE_BODY = (
    'event:content_block_start\n'
    'data:{"type":"content_block_start","index":0,"content_block":'
    '{"type":"tool_use","id":"Bash_0","name":"Bash","input":{}}}\n\n'
    'event:content_block_delta\n'
    'data:{"type":"content_block_delta","index":0,"delta":'
    '{"type":"input_json_delta","partial_json":"{\\"command\\":\\"ls\\"}"}}\n\n'
    'event:message_stop\n'
    'data:{"type":"message_stop"}\n\n'
)

JSON_BODY = json.dumps({
    "id": "msg_1", "type": "message", "role": "assistant",
    "content": [{"type": "tool_use", "id": "Edit_115", "name": "Edit",
                 "input": {"file_path": "/f"}}],
    "stop_reason": "tool_use",
}).encode()


ERROR_SSE = (
    'event:content_block_start\n'
    'data:{"type":"content_block_start","index":0,"content_block":'
    '{"type":"tool_use","id":"Bash_0","name":"Bash","input":{}}}\n\n'
    'event:error\n'
    'data:{"type":"error","error":{"type":"overloaded_error",'
    '"message":"Overloaded"}}\n\n'
)

# Deeper than the interpreter recursion limit: _rewrite_ids (or the json
# scanner) must give up and pass the payload through, not drop the response.
DEEP_JSON = (b'{"a":' * 5000) + b'1' + (b'}' * 5000)

SHIM_ID_RE = re.compile(r"toolu_shim_[0-9a-f]{8}_\d+")


def reap_shim(proc):
    """Stop a shim and close its stdout pipe. Left to Popen's __del__, every
    test reported "subprocess still running" and an unclosed file."""
    proc.terminate()
    try:
        proc.wait(timeout=HANG_TIMEOUT)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=HANG_TIMEOUT)
    proc.stdout.close()


def start_shim(case, target, env=None, flags=()):
    """Launch a shim process against `target`; return its port."""
    # --parent-pid: a run killed outright (SIGKILL skips every cleanup) must
    # not leave the shim listening forever.
    proc = subprocess.Popen(
        [sys.executable, str(SHIM), "0", target, *flags, f"--parent-pid={os.getpid()}"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        env=(dict(os.environ, **env) if env else None))
    case.addCleanup(reap_shim, proc)
    banner = proc.stdout.readline().strip()
    return proc, int(banner.split(":")[1].split(" ")[0])


def dead_port():
    """A port with nothing listening on it."""
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def write_hooks(case, text):
    """Write a hooks module to scratch space and return its path (issue 438).
    The hooks seam is module-shaped, so the core tests build their own tiny
    module instead of naming a real one."""
    d = tempfile.mkdtemp(prefix="ccmorph-hooks-")
    case.addCleanup(shutil.rmtree, d, ignore_errors=True)
    path = Path(d) / "hooks.py"
    path.write_text(text)
    return path


class ShimParentDeathTest(unittest.TestCase):
    def test_shim_dies_with_the_process_that_launched_it(self):
        """--parent-pid must hold when the launcher is killed outright: six
        shims were found reparented to launchd, 12-17 h old, each left by a
        suite run that was SIGKILLed mid-test."""
        parent = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(0.5)"])
        self.addCleanup(parent.wait)
        proc = subprocess.Popen(
            [sys.executable, str(SHIM), "0", f"http://127.0.0.1:{dead_port()}",
             f"--parent-pid={parent.pid}"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.addCleanup(reap_shim, proc)
        self.assertTrue(proc.stdout.readline().startswith("gateway-shim"))
        parent.wait(timeout=HANG_TIMEOUT)
        try:
            proc.wait(timeout=HANG_TIMEOUT)
        except subprocess.TimeoutExpired:
            self.fail("the shim outlived the process that launched it")


class ShimStartupTest(unittest.TestCase):
    def test_startup_does_no_reverse_dns_lookup(self):
        """http.server's server_bind resolves its own address with getfqdn, a
        reverse lookup of 127.0.0.1 that can stall for tens of seconds on macOS
        (17 shim and adapter tests timed out on the CI runner waiting for this
        banner). The shim binds loopback only and needs no name for it, so make
        that lookup an error and require the banner anyway."""
        site = Path(tempfile.mkdtemp(prefix="ccmorph-nodns-"))
        self.addCleanup(shutil.rmtree, site, ignore_errors=True)
        (site / "sitecustomize.py").write_text(
            "import socket\n"
            "def _refuse(*args, **kwargs):\n"
            "    raise RuntimeError('reverse DNS lookup at shim startup')\n"
            "socket.getfqdn = _refuse\n"
            "socket.gethostbyaddr = _refuse\n")
        proc = subprocess.Popen(
            [sys.executable, str(SHIM), "0", f"http://127.0.0.1:{dead_port()}",
             f"--parent-pid={os.getpid()}"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
            env=dict(os.environ, PYTHONPATH=str(site)))
        self.addCleanup(reap_shim, proc)
        banner = proc.stdout.readline().strip()
        self.assertTrue(banner.startswith("gateway-shim :"), banner)


class Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    seen_accept_encoding = []
    seen_xff = []

    def log_message(self, *args):
        pass

    def do_POST(self):
        Upstream.seen_xff.append(self.headers.get("X-Forwarded-For"))
        length = int(self.headers.get("content-length") or 0)
        self.rfile.read(length)
        if self.path == "/gzip":
            # Compress only if the requester offered it. The shim must NOT
            # offer it, however eagerly the real client does.
            offered = self.headers.get("accept-encoding", "")
            Upstream.seen_accept_encoding.append(offered)
            body, enc = JSON_BODY, None
            if "gzip" in offered:
                body, enc = gzip.compress(JSON_BODY), "gzip"
            self.send_response(200)
            self.send_header("content-type", "application/json")
            if enc:
                self.send_header("content-encoding", enc)
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/thinking":
            body = json.dumps({"content": [
                {"type": "thinking", "thinking": ""},
                {"type": "thinking", "thinking": "plan"},
                {"type": "text", "text": "hi"}]}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/deep":
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(DEEP_JSON)))
            self.end_headers()
            self.wfile.write(DEEP_JSON)
        elif self.path == "/sse":
            body = SSE_BODY.encode()
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/json":
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(JSON_BODY)))
            self.end_headers()
            self.wfile.write(JSON_BODY)
        elif self.path == "/fail":
            self.send_response(429)
            data = b'{"error":{"message":"slow down"}}'
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        else:
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()


def serve(handler):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class ShimTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.upstream = serve(Upstream)
        cls.uport = cls.upstream.server_address[1]
        cls.proc = subprocess.Popen(
            [sys.executable, str(SHIM), "0", f"http://127.0.0.1:{cls.uport}",
             f"--parent-pid={os.getpid()}"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        cls.banner = cls.proc.stdout.readline().strip()
        cls.port = int(cls.banner.split(":")[1].split(" ")[0])

    @classmethod
    def tearDownClass(cls):
        reap_shim(cls.proc)
        cls.upstream.shutdown()

    def post(self, path):
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}", data=b"{}",
            headers={"content-type": "application/json"}, method="POST")
        return urllib.request.urlopen(req, timeout=HANG_TIMEOUT)

    def test_sse_tool_use_ids_are_rewritten_unique(self):
        ids = set()
        for _ in range(3):
            with self.post("/sse") as r:
                body = r.read().decode()
            # "data:" or "data: " — the shim preserves whichever form the
            # upstream used (FIX 6), and this stub sends the unspaced one
            start = json.loads(
                next(l.split(":", 1)[1] for l in body.splitlines()
                     if l.startswith("data:") and "content_block_start" in l))
            ids.add(start["content_block"]["id"])
        self.assertEqual(len(ids), 3, f"ids not unique: {ids}")
        for i in ids:
            self.assertRegex(i, SHIM_ID_RE)

    def test_sse_non_tool_events_pass_verbatim(self):
        with self.post("/sse") as r:
            body = r.read().decode()
        self.assertIn("input_json_delta", body)
        self.assertIn('"message_stop"', body)
        self.assertNotIn("Bash_0", body)

    def test_json_response_ids_are_rewritten(self):
        seen = set()
        for _ in range(2):
            with self.post("/json") as r:
                payload = json.loads(r.read())
            seen.add(payload["content"][0]["id"])
        self.assertEqual(len(seen), 2)
        self.assertNotIn("Edit_115", seen)

    def test_error_status_and_body_relayed(self):
        try:
            self.post("/fail")
            self.fail("expected HTTPError")
        except urllib.error.HTTPError as e:
            self.assertEqual(e.code, 429)
            self.assertIn(b"slow down", e.read())

    def test_404_passthrough(self):
        try:
            self.post("/nope")
            self.fail("expected HTTPError")
        except urllib.error.HTTPError as e:
            self.assertEqual(e.code, 404)

    def test_no_x_forwarded_for_header_is_ever_added(self):
        # The shim injects nothing of its own: Claude Code sends no
        # X-Forwarded-For and none appears on the upstream request either.
        target = f"http://127.0.0.1:{self.uport}"

        Upstream.seen_xff.clear()
        proc, port = start_shim(self, target)
        urllib.request.urlopen(urllib.request.Request(
            f"http://127.0.0.1:{port}/deep", data=b"{}",
            headers={"content-type": "application/json"}, method="POST"), timeout=HANG_TIMEOUT)
        self.assertIsNone(Upstream.seen_xff[-1],
                          "the shim added an X-Forwarded-For header on its own")

    def test_gzip_offer_still_yields_rewritten_ids(self):
        """FIX 2 — the shim forwarded the client's accept-encoding, so a
        gzipped response failed json.loads and was passed through
        UNREWRITTEN and silently: the shim's whole job skipped whenever the
        client offered compression."""
        Upstream.seen_accept_encoding.clear()
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}/gzip", data=b"{}",
            headers={"content-type": "application/json",
                     "accept-encoding": "gzip, deflate"}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            raw = r.read()
            self.assertNotEqual(r.headers.get("content-encoding"), "gzip")
        payload = json.loads(raw)
        self.assertRegex(payload["content"][0]["id"], SHIM_ID_RE)
        self.assertEqual(Upstream.seen_accept_encoding, ["identity"],
                         "shim must not offer compression upstream")

    def test_malformed_content_length_is_rejected(self):
        """FIX 7 — int() accepts "1_3" (parses as 13, desyncing the body and
        poisoning the next request on the reused connection) and "-1" (blocks
        in rfile.read)."""
        # NB: " 12" is deliberately absent — the header parser strips leading
        # OWS, so it is a legitimate length of 12, not a malformed one.
        for bad in ("1_3", "-1", "+5", "0x10", "abc", "1 2", ""):
            with self.subTest(content_length=bad):
                conn = http.client.HTTPConnection(
                    "127.0.0.1", self.port, timeout=HANG_TIMEOUT)
                self.addCleanup(conn.close)
                conn.putrequest("POST", "/json", skip_accept_encoding=True)
                conn.putheader("content-type", "application/json")
                conn.putheader("content-length", bad)
                conn.endheaders()
                conn.send(b"{}")
                self.assertEqual(conn.getresponse().status, 400)

    def test_empty_thinking_blocks_are_dropped_even_with_preserved_ids(self):
        """An empty thinking block is replayed by the client and rejected by
        the API; only non-empty thinking and other blocks survive."""
        for flags in ((), ("--preserve-tool-ids",)):
            upstream = serve(Upstream)
            self.addCleanup(upstream.shutdown)
            _, port = start_shim(self, f"http://127.0.0.1:{upstream.server_port}", flags=flags)
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/thinking", data=b"{}",
                headers={"content-type": "application/json"}, method="POST")
            with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
                self.assertEqual(json.loads(r.read())["content"], [
                    {"type": "thinking", "thinking": "plan"},
                    {"type": "text", "text": "hi"}])

    def test_deep_nesting_passes_through_unchanged(self):
        """FIX 8 — unbounded _rewrite_ids recursion raised RecursionError and
        dropped the response with no status at all. Byte-identical
        passthrough beats a bare disconnect."""
        with self.post("/deep") as r:
            self.assertEqual(r.status, 200)
            self.assertEqual(r.read(), DEEP_JSON)


class FixtureUpstream(BaseHTTPRequestHandler):
    """Answers every POST with the raw body the test put in `bodies` for that
    path. The sanitizer fixtures are data, so each test declares its own
    subclass instead of growing the shared Upstream stub."""

    protocol_version = "HTTP/1.1"
    bodies = {}

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.bodies[self.path]
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class ThinkingSanitationTest(unittest.TestCase):
    """#223/#224 — response sanitation must remove only MALFORMED empty
    thinking (no signature) and must never walk into `tool_use.input`.

    A signed empty-text thinking block is a valid omitted-display block: the
    client replays it unchanged, so every field on it must survive. A
    thinking-shaped record under a tool input is application data the tool
    asked for, not API content, so it is opaque here. The signatures below are
    SYNTHETIC fixture strings — no provider replay and no cryptographically
    valid signature was exercised, and no live call was made.
    """

    SIGNED = {"type": "thinking", "thinking": "",
              "signature": "sig-fixture-omitted-display",
              "extra_opaque_field": "keep-me"}
    MALFORMED = {"type": "thinking", "thinking": ""}
    TOOL_USE = {"type": "tool_use", "id": "Edit_115", "name": "Edit",
                "input": {"file_path": "/f"}}
    TOOL_INPUT = {"content": [{"type": "thinking", "thinking": ""},
                              {"type": "thinking", "thinking": "draft"},
                              {"type": "text", "text": "keep"}]}
    NESTED_TOOL_USE = {"type": "tool_use", "id": "Edit_115", "name": "Edit",
                       "input": TOOL_INPUT}

    @staticmethod
    def body(content):
        return json.dumps({"id": "msg_1", "type": "message",
                           "role": "assistant", "content": content,
                           "stop_reason": "tool_use"}).encode()

    def post(self, bodies, flags=()):
        srv = serve(type("Fixture", (FixtureUpstream,), {"bodies": bodies}))
        # shutdown() stops the serving loop but leaves the listening socket
        # open, so close that too; addCleanup is LIFO, hence this order.
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        proc, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}",
                                flags=flags)
        self.addCleanup(reap_shim, proc)
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/fixture", data=b"{}",
            headers={"content-type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            return r.read()

    def test_signed_empty_thinking_survives_with_preserved_ids(self):
        """#223 — with nothing else to change, the response is the upstream
        bytes: the signed omitted-display block is not rebuilt or reordered."""
        raw = self.body([self.SIGNED, self.TOOL_USE])
        self.assertEqual(
            self.post({"/fixture": raw}, flags=["--preserve-tool-ids"]), raw)

    def test_signed_empty_thinking_survives_with_rewritten_ids(self):
        """#223 — sanitation still runs when ids are rewritten; the signed
        block is simply not eligible for removal."""
        raw = self.body([self.SIGNED, self.TOOL_USE])
        content = json.loads(self.post({"/fixture": raw}))["content"]
        self.assertEqual(content[0], self.SIGNED)
        self.assertRegex(content[1]["id"], SHIM_ID_RE)

    def test_unsigned_empty_thinking_is_still_dropped(self):
        """#223 — the malformed-block behavior is the contract; a signature is
        the only thing that exempts an empty block, so both stay side by side."""
        raw = self.body([self.MALFORMED, self.SIGNED,
                         {"type": "text", "text": "hi"}])
        for flags in ((), ("--preserve-tool-ids",)):
            with self.subTest(flags=flags):
                content = json.loads(self.post({"/fixture": raw},
                                               flags=flags))["content"]
                self.assertEqual(content, [self.SIGNED,
                                           {"type": "text", "text": "hi"}])

    def test_tool_input_thinking_shaped_records_survive_with_preserved_ids(self):
        """#224 — the tool's own JSON is not API content: byte-identical."""
        raw = self.body([self.NESTED_TOOL_USE])
        self.assertEqual(
            self.post({"/fixture": raw}, flags=["--preserve-tool-ids"]), raw)

    def test_tool_input_thinking_shaped_records_survive_with_rewritten_ids(self):
        """#224 — the outer id is still rewritten; the input is untouched."""
        raw = self.body([self.NESTED_TOOL_USE])
        content = json.loads(self.post({"/fixture": raw}))["content"]
        self.assertRegex(content[0]["id"], SHIM_ID_RE)
        self.assertEqual(content[0]["input"], self.TOOL_INPUT)

    def test_sanitizer_keeps_protocol_content_and_tool_input_apart(self):
        """#223+#224 together — one response carrying malformed protocol
        thinking, a signed omitted-display block, and a tool whose input
        contains thinking-shaped application records."""
        raw = self.body([self.MALFORMED, self.SIGNED, self.NESTED_TOOL_USE])
        for flags in ((), ("--preserve-tool-ids",)):
            with self.subTest(flags=flags):
                content = json.loads(self.post({"/fixture": raw},
                                               flags=flags))["content"]
                self.assertEqual(len(content), 2)
                self.assertEqual(content[0], self.SIGNED)
                self.assertEqual(content[1]["input"], self.TOOL_INPUT)


class ProcessRestartTest(unittest.TestCase):
    """FIX 1 (CRITICAL) — the counter restarts at 1 in every process, but a
    Claude Code transcript OUTLIVES the shim: every relaunch, profile hop or
    crash-resume re-minted toolu_shim_1 onto ids already in the transcript,
    manufacturing the duplicate-id corruption the shim exists to prevent
    (found live in 21 transcripts). Two sequential PROCESSES must mint
    disjoint id sets."""

    def _ids_from_one_process(self, uport, n=3):
        _, port = start_shim(self, f"http://127.0.0.1:{uport}")
        ids = set()
        for _ in range(n):
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/json", data=b"{}",
                headers={"content-type": "application/json"}, method="POST")
            with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
                ids.add(json.loads(r.read())["content"][0]["id"])
        self.assertEqual(len(ids), n)
        return ids

    def test_sequential_processes_mint_disjoint_ids(self):
        upstream = serve(Upstream)
        self.addCleanup(upstream.shutdown)
        uport = upstream.server_address[1]
        first = self._ids_from_one_process(uport)
        second = self._ids_from_one_process(uport)
        self.assertEqual(first & second, set(),
                         f"restarted shim reused ids: {first & second}")
        for i in first | second:
            self.assertRegex(i, SHIM_ID_RE)


class UnexpectedEOFUpstream(BaseHTTPRequestHandler):
    """cliproxyapi's production failure shape: a complete HTTP 500 response
    whose body says the upstream POST ended with unexpected EOF."""
    protocol_version = "HTTP/1.1"
    hits = 0
    always_fail = False
    unrelated = False

    def log_message(self, *args):
        pass

    def do_POST(self):
        type(self).hits += 1
        length = int(self.headers.get("content-length") or 0)
        self.rfile.read(length)
        if self.unrelated or self.always_fail or type(self).hits == 1:
            message = "internal failure" if self.unrelated else (
                'Post "https://gateway.example.com/v1/messages": unexpected EOF')
            data = json.dumps({"error": {"message": message}}).encode()
            self.send_response(500)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        body = SSE_BODY.encode()
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class UnexpectedEOFRetryTest(unittest.TestCase):
    def setUp(self):
        UnexpectedEOFUpstream.hits = 0
        UnexpectedEOFUpstream.always_fail = False
        UnexpectedEOFUpstream.unrelated = False
        self.upstream = serve(UnexpectedEOFUpstream)
        self.addCleanup(self.upstream.shutdown)
        _, self.port = start_shim(
            self, f"http://127.0.0.1:{self.upstream.server_address[1]}")

    def post(self):
        return keepalive_post(self, self.port, "/v1/messages")

    def test_exact_unexpected_eof_500_is_retried_once_then_succeeds(self):
        resp = self.post()
        self.assertEqual(resp.status, 200)
        body = resp.read()
        self.assertIn(b"message_stop", body)
        self.assertRegex(body.decode(), SHIM_ID_RE)
        self.assertEqual(UnexpectedEOFUpstream.hits, 2)

    def test_repeated_unexpected_eof_is_bounded_and_final_500_is_relayed(self):
        UnexpectedEOFUpstream.always_fail = True
        resp = self.post()
        self.assertEqual(resp.status, 500)
        self.assertIn(b"unexpected EOF", resp.read())
        self.assertEqual(UnexpectedEOFUpstream.hits, 2)

    def test_unrelated_500_is_not_retried(self):
        UnexpectedEOFUpstream.unrelated = True
        resp = self.post()
        self.assertEqual(resp.status, 500)
        self.assertIn(b"internal failure", resp.read())
        self.assertEqual(UnexpectedEOFUpstream.hits, 1)


# The direct route's request: one model, and that is the only model any
# upstream request may ever name (issue #175 — no cross-model fallback).
REQUEST = json.dumps({"model": "claude-opus-5-5", "max_tokens": 1,
                      "messages": []}).encode()


class BlockedUpstream(BaseHTTPRequestHandler):
    """A provider that answers 503 while `failures_left` is positive, then a
    healthy JSON message. Records every body it received, so a test can prove
    what the shim actually put on the wire — and to how many requests."""

    protocol_version = "HTTP/1.1"
    failures_left = 0
    bodies = []
    sse = False                            # failures answer as text/event-stream

    def log_message(self, *args):
        pass

    def _send(self, status, data, ctype="application/json"):
        self.send_response(status)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        type(self).bodies.append(
            self.rfile.read(int(self.headers.get("content-length") or 0)))
        if type(self).failures_left > 0:
            type(self).failures_left -= 1
            return self._send(503, json.dumps(
                {"type": "error", "error": {"type": "overloaded_error",
                                            "message": "blocked"}}).encode(),
                "text/event-stream" if type(self).sse else "application/json")
        self._send(200, json.dumps(
            {"id": "msg_ok", "type": "message", "model": "claude-opus-5-5",
             "content": [], "usage": {"input_tokens": 1, "output_tokens": 1}}).encode())


class TransportRetryTest(unittest.TestCase):
    """Retrying a transport failure, and the breaker it feeds, are driven
    in-process: the retry waits (a jittered sleep) and the breaker's clock are
    both injected, so neither costs the suite real time and neither can flake.
    One shim per launch fronts one provider, so the state is class state."""

    def setUp(self):
        BlockedUpstream.failures_left = 0
        BlockedUpstream.bodies = []
        BlockedUpstream.sse = False
        self.upstream = serve(BlockedUpstream)
        self.addCleanup(self.upstream.shutdown)
        self.shim = load_shim_module()
        self.delays = []
        self.now = [0.0]
        self.shim.IdShim.target = f"http://127.0.0.1:{self.upstream.server_address[1]}"
        self.shim.IdShim.retry_transport = True
        self.shim.IdShim.breaker = self.shim.Breaker(clock=lambda: self.now[0])
        self.shim.IdShim.sleep = staticmethod(self.delays.append)
        server = serve(self.shim.IdShim)
        self.addCleanup(server.shutdown)
        self.port = server.server_address[1]

    def post(self):
        return keepalive_post(self, self.port, "/v1/messages", body=REQUEST)

    def test_blocked_provider_is_retried_twice_on_the_same_model(self):
        BlockedUpstream.failures_left = 10 ** 6
        self.assertEqual(self.post().status, 503)
        # Three upstream requests, all the same bytes: same model, same target.
        self.assertEqual(len(BlockedUpstream.bodies), 3)
        self.assertTrue(all(body == REQUEST for body in BlockedUpstream.bodies))
        self.assertEqual(len(self.delays), 2)

    def test_the_retry_waits_a_jittered_backoff_not_a_fixed_one(self):
        BlockedUpstream.failures_left = 10 ** 6
        self.post()
        self.assertLessEqual(0, self.delays[0])
        self.assertLessEqual(self.delays[0], self.shim.RETRY_BASE)
        self.assertLessEqual(self.delays[1], 2 * self.shim.RETRY_BASE)
        self.assertGreater(len({self.shim.retry_delay(2) for _ in range(20)}), 1)

    def test_provider_that_recovers_answers_after_one_retry(self):
        BlockedUpstream.failures_left = 1
        resp = self.post()
        self.assertEqual(resp.status, 200)
        self.assertEqual(json.loads(resp.read())["id"], "msg_ok")
        self.assertEqual(len(BlockedUpstream.bodies), 2)
        self.assertEqual(len(self.delays), 1)

    def test_refused_connection_is_retried_then_502(self):
        self.shim.IdShim.target = f"http://127.0.0.1:{dead_port()}"
        self.assertEqual(self.post().status, 502)
        self.assertEqual(len(self.delays), 2)

    def test_an_sse_error_status_is_retried_too(self):
        # A gateway that streams its errors must not lose the retry: no byte has
        # gone downstream, so the status alone decides — as it does for a JSON
        # reply — and the client sees the last one if every attempt fails.
        BlockedUpstream.sse = True
        BlockedUpstream.failures_left = 10 ** 6
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 3)
        self.assertEqual(len(self.delays), 2)

    def test_a_stall_is_not_retried_even_when_urllib_wraps_it(self):
        # urllib delivers the connect phase's timeout inside URLError, so the
        # stall rule has to unwrap: retrying it would stack STALL_TIMEOUT three
        # times before the client heard anything, and the three failures would
        # fall outside the breaker's window.
        def stall(_body):
            raise urllib.error.URLError(TimeoutError("connect timed out"))

        self.shim.IdShim._proxy = staticmethod(stall)
        self.assertEqual(self.post().status, 502)
        self.assertEqual(self.delays, [])
        self.assertEqual(BlockedUpstream.bodies, [])

    def test_the_stall_rule_names_the_system_pythons_socket_timeout(self):
        # On 3.9 — the macOS system python the live shim runs on — socket.timeout
        # is not TimeoutError. Naming both keeps the no-retry rule there too.
        self.assertIn(socket.timeout, self.shim.STALL_ERRORS)
        self.assertTrue(self.shim.wrapped_timeout(urllib.error.URLError(socket.timeout())))

    def test_without_the_flag_nothing_is_retried(self):
        self.shim.IdShim.retry_transport = False
        BlockedUpstream.failures_left = 10 ** 6
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 1)
        self.assertEqual(self.delays, [])

    def open_breaker(self):
        """Three requests that each still fail after their retries, a minute
        apart: the breaker's threshold inside its 5-minute window. Each request
        cost three upstream calls, so the wire shows nine."""
        BlockedUpstream.failures_left = 10 ** 6
        for _ in range(2):
            self.assertEqual(self.post().status, 503)
            self.now[0] += 60
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 9)

    def test_breaker_opens_after_three_failed_requests_and_one_probe_closes_it(self):
        self.open_breaker()
        # Held: the client gets the breaker's own 503 and nothing goes upstream.
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 9)
        # A second short of the 10 minutes it is still held...
        self.now[0] += 599
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 9)
        # ...and the next real request past it probes the provider: this one
        # succeeds, so the breaker closes and the following request is normal.
        self.now[0] += 2
        BlockedUpstream.failures_left = 0
        self.assertEqual(self.post().status, 200)
        self.assertEqual(len(BlockedUpstream.bodies), 10)
        self.assertEqual(self.post().status, 200)
        self.assertEqual(len(BlockedUpstream.bodies), 11)

    def test_failed_probe_reopens_the_breaker(self):
        self.open_breaker()
        self.now[0] += 601
        # The probe reaches the still-blocked provider: one more request with
        # its retries, and the breaker is open again.
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 12)
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 12)

    def test_failures_spread_over_more_than_five_minutes_do_not_open_it(self):
        BlockedUpstream.failures_left = 10 ** 6
        for _ in range(3):
            self.assertEqual(self.post().status, 503)
            self.now[0] += 200
        # Three failures, but never three inside the window: the next request
        # still goes upstream (three more calls) rather than being held.
        self.assertEqual(self.post().status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 12)

    def test_blocked_provider_fails_readably_and_never_goes_elsewhere(self):
        self.open_breaker()
        resp = self.post()
        self.assertEqual(resp.status, 503)
        message = json.loads(resp.read())["error"]["message"]
        self.assertIn("ccmorph", message)
        self.assertIn("then tries one", message)
        # Every call the shim ever made went to the one target, with the one
        # model: an open breaker is never a reason to try anywhere else.
        self.assertTrue(all(body == REQUEST for body in BlockedUpstream.bodies))


class DeadUpstreamTest(unittest.TestCase):
    """FIX 3 — the 502 path sent no content-length and did not close, so an
    HTTP/1.1 keep-alive client waited forever for a body that never came."""

    def test_502_terminates_and_does_not_hang(self):
        _, port = start_shim(self, f"http://127.0.0.1:{dead_port()}")
        resp = keepalive_post(self, port, "/json")
        self.assertEqual(resp.status, 502)
        start = time.monotonic()
        # The hang is HERE: with neither content-length nor a close, a
        # keep-alive client blocks in read() waiting for a body forever.
        self.assertEqual(resp.read(), b"")
        elapsed = time.monotonic() - start
        self.assertLess(elapsed, 10, f"502 hung the client {elapsed:.1f}s")


class SlowUpstream(BaseHTTPRequestHandler):
    """The REAL cliproxyapi behavior the original stub missed: keep-alive —
    the stream body completes with message_stop but the connection stays
    open and NO more bytes ever arrive. The shim must terminate the client
    response at message_stop, not wait for bytes that never come (the
    harness:4.2 production hang, 2026-08-16)."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        self.rfile.read(length)
        body = SSE_BODY.encode()
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        # no content-length, no close: keep-alive hangs open
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()
        time.sleep(30)   # hold the connection open like cliproxyapi does


class KeepAliveHangTest(unittest.TestCase):
    def test_stream_completes_at_message_stop_under_keepalive(self):
        body, elapsed = read_stream(self, SlowUpstream)
        self.assertLess(elapsed, 10,
                        f"stream hung {elapsed:.1f}s despite message_stop "
                        f"— the keep-alive regression")
        self.assertRegex(body.decode(), SHIM_ID_RE)


class TruncatedUpstream(BaseHTTPRequestHandler):
    """Upstream dies mid-stream: a tool_use event arrives, then EOF with no
    message_stop (gateway crash, connection reset, 21-transcript scenario)."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        self.rfile.read(length)
        body = SSE_BODY.encode().split(b"event:message_stop")[0]
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()
        self.close_connection = True   # EOF with no message_stop


class ErrorEventUpstream(BaseHTTPRequestHandler):
    """Stream terminating on `event: error` (overloaded_error), then
    keep-alive silence — never a message_stop."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        self.wfile.write(ERROR_SSE.encode())
        self.wfile.flush()
        time.sleep(30)   # hold open like cliproxyapi does


def keepalive_post(case, port, path, body=b"{}"):
    """POST over a genuine keep-alive connection and return the response.

    Deliberately raw http.client, NOT urllib: urllib.request.do_open
    OVERWRITES the Connection header with "close" (it cannot reuse sockets),
    which makes the shim close the connection no matter what its own logic
    decided — masking exactly the hangs fixes 3/4/5 address. Claude Code's
    client sends keep-alive, so the tests must too.
    """
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=HANG_TIMEOUT)
    case.addCleanup(conn.close)
    conn.putrequest("POST", path, skip_accept_encoding=True)
    conn.putheader("content-type", "application/json")
    conn.putheader("connection", "keep-alive")
    conn.putheader("content-length", str(len(body)))
    conn.endheaders()
    conn.send(body)
    resp = conn.getresponse()
    resp.connection = conn      # let callers hang up hard, mid-stream
    return resp


def read_stream(case, upstream_cls):
    """Drive one keep-alive SSE request through a fresh shim; return
    (body, elapsed). Raises TimeoutError if the shim hangs the client."""
    upstream = serve(upstream_cls)
    case.addCleanup(upstream.shutdown)
    _, port = start_shim(case, f"http://127.0.0.1:{upstream.server_address[1]}")
    resp = keepalive_post(case, port, "/sse")
    start = time.monotonic()
    body = resp.read()
    return body, time.monotonic() - start


class StreamTerminationTest(unittest.TestCase):
    def test_upstream_eof_midstream_closes_the_client(self):
        """FIX 4 — the bare `return` on upstream EOF left close_connection
        False, so the client (no content-length on an SSE response) waited
        forever for a completion signal that never came."""
        body, elapsed = read_stream(self, TruncatedUpstream)
        self.assertLess(elapsed, 10, f"EOF hung the client {elapsed:.1f}s")
        self.assertRegex(body.decode(), SHIM_ID_RE)
        self.assertNotIn(b"message_stop", body)

    def test_error_event_stream_terminates(self):
        """FIX 5 — only message_stop was terminal, so a stream ending on
        `event: error` (overloaded_error) hung the client until timeout."""
        body, elapsed = read_stream(self, ErrorEventUpstream)
        self.assertLess(elapsed, 10,
                        f"error-event stream hung {elapsed:.1f}s")
        self.assertIn(b"overloaded_error", body)
        self.assertRegex(body.decode(), SHIM_ID_RE)


class FramingTest(unittest.TestCase):
    def test_rewritten_event_framing_is_byte_exact(self):
        """FIX 6 — the rewritten data line was re-emitted with a hardcoded
        `data: ` prefix and a single CRLF, dropping the blank line that
        closes the SSE event and mixing CRLF into an LF stream."""
        body, _ = read_stream(self, SlowUpstream)
        expected = SSE_BODY.encode()
        # The only permitted difference is the id inside the rewritten event
        ids = SHIM_ID_RE.findall(body.decode())
        self.assertEqual(len(ids), 1, f"expected one minted id, got {ids}")
        self.assertEqual(body.replace(ids[0].encode(), b"Bash_0"), expected,
                         "framing differs beyond the rewritten id")
        self.assertNotIn(b"\r\n", body, "CRLF mixed into an LF stream")


def load_shim_module():
    """Import gateway-shim as a module. It has no .py extension, so it
    needs an explicit SourceFileLoader."""
    loader = importlib.machinery.SourceFileLoader("ccmorph_gateway_shim", str(SHIM))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class DeadSocket:
    """A wfile whose writes fail the way a hung-up client's does — every one at
    survive=0 (the default), or after `survive` of them have been let through.
    Flush always passes: the shim writes before it flushes, so the first failing
    call is a write either way, and counting writes keeps a test's "one event
    delivered, then the client vanishes" independent of flush bookkeeping."""

    def __init__(self, exc, survive=0):
        self.exc = exc
        self.survive = survive

    def write(self, _data):
        if self.survive:
            self.survive -= 1
            return
        raise self.exc

    def flush(self):
        pass


class ClientDisconnectTest(unittest.TestCase):
    """FIX 9 — a cancelled turn (client closes mid-stream) let
    BrokenPipeError escape _stream_sse, and ThreadingHTTPServer's
    handle_error dumped a traceback into the shared adapter log.

    Driven at the unit level on purpose: end-to-end, whether the kernel has
    yet noticed the peer is gone depends on socket buffering and timing, so
    an integration test for this passes or fails by luck (probed: no
    traceback at 1/2/3/4/6s waits even on the unfixed shim). Injecting the
    failing wfile makes the assertion deterministic.
    """

    def _run_stream(self, exc):
        shim = load_shim_module()
        handler = shim.IdShim.__new__(shim.IdShim)      # no socket setup
        handler.wfile = DeadSocket(exc)
        handler.close_connection = False
        handler._stream_sse(io.BytesIO(SSE_BODY.encode()))
        return handler

    def test_broken_pipe_midstream_is_swallowed(self):
        handler = self._run_stream(BrokenPipeError(32, "Broken pipe"))
        self.assertTrue(handler.close_connection)

    def test_connection_reset_midstream_is_swallowed(self):
        handler = self._run_stream(ConnectionResetError(54, "reset by peer"))
        self.assertTrue(handler.close_connection)


class EchoUpstream(BaseHTTPRequestHandler):
    """Answers with the exact request body it received, so a test can assert
    what the shim put on the upstream wire."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length") or 0))
        self.send_response(200)
        self.send_header("content-type", "text/plain")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class ShimFlagTest(unittest.TestCase):
    """The request-side flags, end to end. Without a profile's hooks module
    the shipped shim rewrites nothing in a request: the body and every
    identifier go upstream as the client sent them (issue 438: the
    gateway-specific rewrites moved out of the core shim)."""

    def test_request_body_is_byte_identical_without_hooks(self):
        body = json.dumps({
            "model": "m", "tools": [{"name": "Bash"}],
            "messages": [{"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": [
                    {"type": "tool_reference", "tool_name": "Monitor"}]}]}],
        }).encode()
        srv = serve(EchoUpstream)
        self.addCleanup(srv.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}")
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/v1/messages", data=body,
            headers={"content-type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            self.assertEqual(r.read(), body)

    def test_removed_repair_and_affinity_flags_are_rejected(self):
        # Issue 438: those rewrites left the core shim, so their flags are now
        # unknown; a caller still passing one fails loud at startup instead of
        # silently losing the rewrite.
        for flag in ("--repair-tool-references", "--affinity-header=X-Header"):
            with self.subTest(flag=flag):
                out = subprocess.run([sys.executable, str(SHIM), "0", "http://x",
                                      flag], capture_output=True, text=True,
                                     timeout=HANG_TIMEOUT)
                self.assertNotEqual(out.returncode, 0)
                self.assertIn("usage", out.stdout + out.stderr)

    def test_preserve_tool_ids_stops_response_id_rewriting(self):
        srv = serve(Upstream)
        self.addCleanup(srv.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}",
                             flags=["--preserve-tool-ids"])
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/json", data=b"{}",
            headers={"content-type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            self.assertEqual(r.read(), JSON_BODY)

    def test_unknown_flag_is_rejected(self):
        proc = subprocess.run([sys.executable, str(SHIM), "0", "http://x",
                               "--nope"], capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("usage", proc.stdout + proc.stderr)


HOOKS_MODULE = '''\
"""A stand-in for a profile's hooks module: neutral names, all three hooks."""


def user_agent(client_ua):
    return "hooks-ua[" + client_ua + "]"


def repair_request(payload):
    if payload.get("hooks") == "dirty":
        payload["hooks"] = "clean"
        return True
    return False


def outbound_headers(headers, session):
    if session is None:
        headers["x-hooks-no-session"] = "1"
    else:
        headers["x-hooks-affinity"] = session
'''


class ShimHooksTest(unittest.TestCase):
    """The CCMORPH_SHIM_HOOKS seam (issue 438): a profile points the shim at
    a module whose functions rewrite its outbound requests — the User-Agent,
    the body, or the headers. Everything gateway-specific lives in such a
    module; the shim itself stays generic. These tests build the module in
    scratch space with neutral names rather than naming a real one."""

    def request(self, hooks=None, body=b"{}", headers=None, flags=()):
        RecordingUpstream.seen = []
        srv = serve(RecordingUpstream)
        self.addCleanup(srv.shutdown)
        env = {"CCMORPH_SHIM_HOOKS": str(hooks)} if hooks is not None else None
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}",
                             env=env, flags=flags)
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/v1/messages", data=body,
            headers=headers or {"content-type": "application/json"})
        urllib.request.urlopen(req, timeout=HANG_TIMEOUT).read()
        return RecordingUpstream.seen[-1]

    def test_hooks_module_rewrites_the_user_agent(self):
        got, _ = self.request(write_hooks(self, HOOKS_MODULE), headers={
            "content-type": "application/json",
            "User-Agent": "claude-cli/2.1.300 (external, sdk-ts)"})
        self.assertEqual(got["user-agent"],
                         "hooks-ua[claude-cli/2.1.300 (external, sdk-ts)]")

    def test_a_hooks_module_without_user_agent_forwards_the_client_one(self):
        got, _ = self.request(
            write_hooks(self, "def repair_request(payload):\n    return False\n"),
            headers={"content-type": "application/json",
                     "User-Agent": "claude-cli/2.1.300 (external, sdk-ts)"})
        self.assertEqual(got["user-agent"], "claude-cli/2.1.300 (external, sdk-ts)")

    def test_hooks_repair_reaches_the_upstream_body(self):
        _, sent = self.request(write_hooks(self, HOOKS_MODULE),
                               body=json.dumps({"hooks": "dirty"}).encode())
        # COMPACT separators: the shim re-serializes only what the hook changed.
        self.assertEqual(sent, b'{"hooks":"clean"}')

    def test_hooks_repair_that_changes_nothing_keeps_the_bytes_identical(self):
        raw = b'{"hooks":"clean"}'
        _, sent = self.request(write_hooks(self, HOOKS_MODULE), body=raw)
        self.assertEqual(sent, raw)

    def test_a_raising_hook_leaves_the_request_bytes_unchanged(self):
        hooks = write_hooks(self,
                            "def repair_request(payload):\n    raise ValueError('boom')\n")
        raw = b'{"hooks":"dirty"}'
        _, sent = self.request(hooks, body=raw)
        self.assertEqual(sent, raw)

    def test_hooks_outbound_headers_receive_the_session_id(self):
        # Issue 438: the session capture used to happen only inside the
        # --random-device-id swap; hooks need it on every request, with no
        # flag set at all.
        user_id = json.dumps({"device_id": "d" * 64, "session_id": "sess-1"})
        body = json.dumps({"metadata": {"user_id": user_id}}).encode()
        got, _ = self.request(write_hooks(self, HOOKS_MODULE), body=body)
        self.assertEqual(got["x-hooks-affinity"], "sess-1")
        self.assertNotIn("x-hooks-no-session", got)

    def test_hooks_outbound_headers_get_none_without_a_session_id(self):
        got, _ = self.request(write_hooks(self, HOOKS_MODULE), body=b"{}")
        self.assertEqual(got["x-hooks-no-session"], "1")
        self.assertNotIn("x-hooks-affinity", got)

    def test_a_missing_hooks_module_refuses_to_start(self):
        # A profile pointing at a module that is not there must fail loud at
        # startup, never serve with its rewrites silently missing.
        out = subprocess.run([sys.executable, str(SHIM), "0", "http://127.0.0.1:1"],
                             capture_output=True, text=True, timeout=HANG_TIMEOUT,
                             env=dict(os.environ, CCMORPH_SHIM_HOOKS="/nonexistent/hooks.py"))
        self.assertNotEqual(out.returncode, 0)
        self.assertIn("CCMORPH_SHIM_HOOKS", out.stdout + out.stderr)


# --- usage sidecar ----------------------------------------------------------
IDENTITY = "route-identity-digest-from-the-adapter"
ROUTE_SPEC = json.dumps({
    "adapter": "unused", "profile": "test-profile", "config": "/nowhere/gateway.conf",
    "port": 1, "identity": IDENTITY, "models": ["m"], "timeout": 1,
    "key_sha256": hashlib.sha256(b"k").hexdigest()})
REQUEST_BODY = json.dumps({"model": "m", "messages": []}).encode()

# The response id is the join key, so it must survive capture untouched — the
# tool_use id beside it is the one id the shim DOES rewrite.
CAPTURE_JSON = json.dumps({
    "id": "msg_capture_json", "type": "message", "model": "m-served",
    "content": [{"type": "tool_use", "id": "Bash_0", "name": "Bash", "input": {}}],
    "usage": {"input_tokens": 100, "output_tokens": 7,
              "cache_read_input_tokens": 3, "cache_creation_input_tokens": 0,
              "cache_creation": {"ephemeral_5m_input_tokens": 2,
                                 "ephemeral_1h_input_tokens": "not-a-number"},
              "service_tier": "standard", "unknown_bucket": 9},
}).encode()

CAPTURE_SSE = (
    'event:message_start\n'
    'data:{"type":"message_start","message":{"id":"msg_capture_sse","model":"m-served",'
    '"usage":{"input_tokens":11,"cache_read_input_tokens":2,"service_tier":"standard"}}}\n\n'
    'event:content_block_start\n'
    'data:{"type":"content_block_start","index":0,"content_block":'
    '{"type":"tool_use","id":"Bash_0","name":"Bash","input":{}}}\n\n'
    'event:message_delta\n'
    'data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},'
    '"usage":{"output_tokens":42}}\n\n'
    'event:message_stop\n'
    'data:{"type":"message_stop"}\n\n'
)


class RecordingUpstream(BaseHTTPRequestHandler):
    """Records the headers and body of the last POST it was sent."""
    protocol_version = "HTTP/1.1"
    seen = []

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length") or 0))
        RecordingUpstream.seen.append(
            ({k.lower(): v for k, v in self.headers.items()}, body))
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", "2")
        self.end_headers()
        self.wfile.write(b"{}")


class InboundTokenTest(unittest.TestCase):
    """With CCMORPH_INBOUND the client holds only a token; the real key goes upstream."""

    def call(self, token):
        RecordingUpstream.seen = []
        srv = serve(RecordingUpstream)
        self.addCleanup(srv.shutdown)
        env = {"CCMORPH_INBOUND": json.dumps(
            {"sha256": hashlib.sha256(b"tok").hexdigest(), "key": "real"})}
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}", env)
        req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/models", data=b"{}",
                                     headers={"Authorization": f"Bearer {token}"})
        try:
            urllib.request.urlopen(req, timeout=HANG_TIMEOUT).read()
        except urllib.error.HTTPError as e:
            return e.code, None
        return 200, RecordingUpstream.seen[-1][0]

    def test_token_swapped_for_real_key(self):
        status, got = self.call("tok")
        self.assertEqual(status, 200)
        self.assertEqual(got["x-api-key"], "real")
        self.assertEqual(got["authorization"], "Bearer real")

    def test_wrong_token_refused(self):
        self.assertEqual(self.call("real")[0], 401)


class IdentifierTest(unittest.TestCase):
    """The client's schema goes upstream as sent; identifiers change only on a
    profile's opt-in (--random-device-id)."""

    REAL_DEVICE = "d" * 64

    def post(self, path, body, headers, flags=()):
        RecordingUpstream.seen = []
        srv = serve(RecordingUpstream)
        self.addCleanup(srv.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}", flags=flags)
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}{path}", data=body, headers=headers)
        urllib.request.urlopen(req, timeout=HANG_TIMEOUT).read()
        return RecordingUpstream.seen[-1]

    def request(self, session="sess-1", device=None, **kw):
        """A Claude Code request: metadata.user_id is a JSON string."""
        user_id = json.dumps({"device_id": device or self.REAL_DEVICE, "account_uuid": "acct",
                              "session_id": session}, separators=(",", ":"))
        body = json.dumps({"model": "m", "messages": [{"role": "user", "content": "hi"}],
                           "metadata": {"user_id": user_id}}, separators=(",", ":")).encode()
        headers = {"content-type": "application/json", "X-Claude-Code-Session-Id": session,
                   "X-Stainless-OS": "MacOS", "X-Stainless-Arch": "arm64",
                   "User-Agent": "claude-cli/9.9.9 (external, cli)", "x-api-key": "k"}
        got, sent = self.post("/v1/messages", body, headers, **kw)
        return body, got, sent

    def test_flags_off_body_and_identifier_headers_pass_as_sent(self):
        body, got, sent = self.request()
        self.assertEqual(sent, body)
        self.assertEqual(got["x-claude-code-session-id"], "sess-1")
        self.assertEqual(got["x-stainless-os"], "MacOS")
        self.assertEqual(got["x-stainless-arch"], "arm64")
        # Issue 438: the User-Agent rewrite was one gateway's quirk; without
        # hooks the UA goes upstream exactly as the client sent it.
        self.assertEqual(got["user-agent"], "claude-cli/9.9.9 (external, cli)")

    def test_random_device_id_changes_only_the_device_id(self):
        body, got, sent = self.request(flags=["--random-device-id"])
        before, after = json.loads(body), json.loads(sent)
        ids_before = json.loads(before["metadata"]["user_id"])
        ids_after = json.loads(after["metadata"]["user_id"])
        self.assertNotEqual(ids_after["device_id"], self.REAL_DEVICE)
        self.assertRegex(ids_after["device_id"], r"^[0-9a-f]{64}$")
        ids_after["device_id"] = self.REAL_DEVICE
        self.assertEqual(ids_after, ids_before)
        for doc in (before, after):
            doc["metadata"]["user_id"] = None
        self.assertEqual(before, after)
        self.assertNotIn(self.REAL_DEVICE.encode(), sent)
        self.assertEqual(got["x-claude-code-session-id"], "sess-1")
        self.assertEqual(got["x-stainless-os"], "MacOS")

    def test_device_id_is_stable_within_a_session_and_differs_across_sessions(self):
        RecordingUpstream.seen = []
        srv = serve(RecordingUpstream)
        self.addCleanup(srv.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}",
                             flags=["--random-device-id"])

        def device(session):
            user_id = json.dumps({"device_id": self.REAL_DEVICE, "account_uuid": "",
                                  "session_id": session})
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/v1/messages",
                data=json.dumps({"metadata": {"user_id": user_id}}).encode(),
                headers={"content-type": "application/json",
                         "X-Claude-Code-Session-Id": session})
            urllib.request.urlopen(req, timeout=HANG_TIMEOUT).read()
            body = RecordingUpstream.seen[-1][1]
            return json.loads(json.loads(body)["metadata"]["user_id"])["device_id"]

        first, again, other = device("s1"), device("s1"), device("s2")
        self.assertEqual(first, again)
        self.assertNotEqual(first, other)

    def test_user_agent_is_forwarded_as_sent(self):
        # Issue 438: the claude-cli rewrite existed because one gateway gated
        # models on the CLI version; the shim now forwards whatever the client
        # sent, claude-cli or not, and a profile that needs a rewrite opts in
        # through its hooks module.
        for ua in ("claude-cli/2.1.300 (external, sdk-ts, agent-sdk/0.1)",
                   "Python-urllib/3.14", "Anthropic/Python 0.69.0"):
            with self.subTest(ua=ua):
                got, _ = self.post("/v1/messages", b"{}", {
                    "content-type": "application/json", "User-Agent": ua})
                self.assertEqual(got["user-agent"], ua)

    def test_count_tokens_body_gets_the_random_device_id_too(self):
        _, sent = self.post("/v1/messages/count_tokens", json.dumps(
            {"metadata": {"user_id": json.dumps({"device_id": self.REAL_DEVICE})}}).encode(),
            {"content-type": "application/json"}, ["--random-device-id"])
        self.assertNotIn(self.REAL_DEVICE.encode(), sent)

    def test_cjk_body_keeps_utf8_when_the_device_id_is_swapped(self):
        text = "你好，世界" * 20
        raw = json.dumps({"model": "m", "messages": [{"role": "user", "content": text}],
                          "metadata": {"user_id": json.dumps({"device_id": self.REAL_DEVICE})}},
                         ensure_ascii=False, separators=(",", ":")).encode()
        _, sent = self.post("/v1/messages", raw, {"content-type": "application/json"},
                            ["--random-device-id"])
        self.assertIn(text.encode(), sent)

    def test_lone_surrogate_body_still_swaps_the_device_id(self):
        # A lone \ud83d escape parses in Python's json but cannot UTF-8 encode
        # after an ensure_ascii=False dump (#182); the swap must survive it.
        raw = json.dumps({"model": "m",
                          "messages": [{"role": "user", "content": "x \ud83d y"}],
                          "metadata": {"user_id": json.dumps({"device_id": self.REAL_DEVICE})}},
                         separators=(",", ":")).encode()
        _, sent = self.post("/v1/messages", raw, {"content-type": "application/json"},
                            ["--random-device-id"])
        doc = json.loads(sent.decode("utf-8"))
        self.assertEqual(doc["messages"][0]["content"], "x \ud83d y")
        self.assertNotIn(self.REAL_DEVICE.encode(), sent)
        self.assertRegex(json.loads(doc["metadata"]["user_id"])["device_id"],
                         r"^[0-9a-f]{64}$")

    def test_nested_surrogate_user_id_keeps_valid_outer_json_and_inner_escapes(self):
        for extra in ("x \ud83d y", "x \udc00 y", "你好 😀"):
            with self.subTest(extra=repr(extra)):
                ids = {"device_id": self.REAL_DEVICE, "account_uuid": "account",
                       "session_id": "nested", "extra": extra}
                # Inner surrogate escapes are ordinary backslash characters in
                # the outer JSON string, which contains no unpaired surrogates.
                raw = json.dumps({"model": "m", "messages": [{"role": "user", "content": "你好"}],
                                  "metadata": {"user_id": json.dumps(ids)}},
                                 ensure_ascii=False).encode()
                _, sent = self.post("/v1/messages", raw, {"content-type": "application/json"},
                                    ["--random-device-id"])
                doc = json.loads(sent.decode("utf-8"))
                user_id = doc["metadata"]["user_id"]
                # A strict outer parser rejects unpaired surrogate escapes.
                # Its decoded JSON strings must therefore be UTF-8 encodable.
                user_id.encode("utf-8", errors="strict")
                actual = json.loads(user_id)
                self.assertEqual(actual["extra"], extra)
                self.assertEqual(actual["account_uuid"], ids["account_uuid"])
                self.assertEqual(actual["session_id"], ids["session_id"])
                self.assertRegex(actual["device_id"], r"^[0-9a-f]{64}$")
                self.assertNotEqual(actual["device_id"], self.REAL_DEVICE)
                self.assertIn("你好".encode(), sent)
                if extra == "你好 😀":
                    self.assertIn(extra.encode(), sent)
                else:
                    self.assertIn("\\u", user_id)

    def test_bodies_it_cannot_swap_go_upstream_byte_identical(self):
        flags = ["--random-device-id"]
        for raw in (b"not json \xff metadata",
                    b'{ "model": "m",  "messages": [] }',
                    b'{"metadata":{"user_id":"device_abc_session_def"}}',
                    b'{"metadata":{"user_id":"{\\"session_id\\":\\"s\\"}"}}'):
            _, sent = self.post("/v1/messages", raw, {"content-type": "application/json"}, flags)
            self.assertEqual(sent, raw)


class CaptureUpstream(BaseHTTPRequestHandler):
    """One stub, four shapes: a complete JSON message, a complete stream, a
    stream truncated by EOF, and a stream a test can hold open mid-response."""
    protocol_version = "HTTP/1.1"
    mode = "json"
    gate = None
    json_body = None                       # per-test override of CAPTURE_JSON
    sse_body = None                        # per-test override of CAPTURE_SSE
    fail_first = 0                         # that many POSTs answer 503 before the mode applies
    eof_first = 0                          # that many POSTs answer the replayable 500 first

    def log_message(self, *args):
        pass

    def _send(self, body, ctype, status=200):
        self.send_response(status)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    on_body = None                         # per-test hook: the request body received
    on_headers = None                      # per-test hook: the request headers received

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length") or 0))
        if self.on_body:
            self.on_body(body)
        if self.on_headers:
            self.on_headers(dict(self.headers.items()))
        if type(self).fail_first:
            type(self).fail_first -= 1
            return self._send(json.dumps(
                {"type": "error", "error": {"type": "overloaded_error",
                                            "message": "blocked"}}).encode(),
                "application/json", 503)
        if type(self).eof_first:
            type(self).eof_first -= 1
            return self._send(b'{"type":"error","error":{"message":"unexpected EOF"}}',
                              "application/json", 500)
        if self.mode == "json":
            return self._send(self.json_body or CAPTURE_JSON, "application/json")
        if self.mode == "sse":
            return self._send((self.sse_body or CAPTURE_SSE).encode(), "text/event-stream")
        if self.mode == "truncated":
            # Upstream dies after message_start: EOF with no message_stop.
            head = CAPTURE_SSE.encode().split(b"event:message_delta")[0]
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.end_headers()
            self.wfile.write(head)
            self.wfile.flush()
            self.close_connection = True
            return
        head, rest = CAPTURE_SSE.encode().split(b"event:content_block_start", 1)
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        self.wfile.write(head)
        self.wfile.flush()
        CaptureUpstream.gate.wait(30)      # held until the test has the row
        self.wfile.write(b"event:content_block_start" + rest)
        self.wfile.flush()
        time.sleep(30)                     # keep-alive silence after message_stop


def iso_utc(timestamp):
    parsed = datetime.datetime.fromisoformat(timestamp)
    return parsed.utcoffset() == datetime.timedelta(0)


def upstream_bytes(case, body):
    """`body` as the upstream sent it: the one difference the shim may make to
    a response is the tool_use id it mints, which capture must not disturb."""
    minted = SHIM_ID_RE.findall(body.decode())
    case.assertEqual(len(minted), 1)
    return body.replace(minted[0].encode(), b"Bash_0")


class UsageSidecarTest(unittest.TestCase):
    def setUp(self):
        CaptureUpstream.mode = "json"
        CaptureUpstream.gate = threading.Event()
        CaptureUpstream.json_body = CaptureUpstream.sse_body = None
        CaptureUpstream.fail_first = 0
        CaptureUpstream.eof_first = 0
        self.state = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, str(self.state), True)
        self.upstream = serve(CaptureUpstream)
        self.addCleanup(self.upstream.shutdown)
        self.target = f"http://127.0.0.1:{self.upstream.server_address[1]}"

    def shim(self, env=None, flags=()):
        """A fresh shim process against the stub upstream, with the run's own
        state directory so no test can touch the operator's."""
        base = dict(env or {}, XDG_STATE_HOME=str(self.state))
        _, port = start_shim(self, self.target, env=base, flags=flags)
        return port

    def row_path(self, response_id):
        # The primary row is named sha256(response_id).json — the name a
        # consumer computes from a transcript's message id. Only a recycled id
        # (same id, different turn) spills to sha256(id.turn_id).json.
        return (self.state / "ccmorph" / "usage"
                / (hashlib.sha256(response_id.encode()).hexdigest() + ".json"))

    def row_for(self, response_id):
        """Find the single row file for a response id, regardless of the
        turn-id suffix in the filename. Polls until the row appears."""
        deadline = time.monotonic() + HANG_TIMEOUT
        while time.monotonic() < deadline:
            rows = [p for p in (self.state / "ccmorph" / "usage").glob("*.json")
                    if json.loads(p.read_text()).get("response_id") == response_id]
            if len(rows) == 1:
                return rows[0]
            time.sleep(0.05)
        self.fail(f"expected 1 row for {response_id}, got {len(rows)}")

    def rows(self):
        return {p.name: json.loads(p.read_text())
                for p in (self.state / "ccmorph" / "usage").glob("*.json")}

    def wait_for(self, path, predicate=bool):
        """The shim writes the row after the client has its bytes, so every row
        assertion polls rather than racing the client's last read."""
        deadline = time.monotonic() + HANG_TIMEOUT
        while time.monotonic() < deadline:
            if path.exists():
                row = json.loads(path.read_text())
                if predicate(row):
                    return row
            time.sleep(0.05)
        self.fail(f"no row at {path} within 5s")

    def post(self, port, path="/v1/messages", body=REQUEST_BODY, headers=None):
        request = urllib.request.Request(
            f"http://127.0.0.1:{port}{path}", data=body, method="POST",
            headers={"content-type": "application/json", "x-api-key": "k",
                     **(headers or {})})
        return urllib.request.urlopen(request, timeout=HANG_TIMEOUT)

    def test_nonstream_message_is_one_complete_row(self):
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            self.assertEqual(upstream_bytes(self, r.read()), CAPTURE_JSON)
        # The row sits at exactly sha256(response_id).json, the consumer's join key.
        row_path = self.row_for("msg_capture_json")
        self.assertEqual(row_path, self.row_path("msg_capture_json"))
        row = self.wait_for(row_path)
        self.assertEqual(len(list(self.rows())), 1)
        self.assertEqual((row["schema"], row["response_id"]), (1, "msg_capture_json"))
        self.assertEqual(row["profile"], "test-profile")
        self.assertEqual(row["requested_model"], "m")
        self.assertEqual(row["response_model"], "m-served")
        self.assertIs(row["complete"], True)
        # No transport retry happened: the flag is off on this shim.
        self.assertEqual(row["retries"], 0)
        self.assertTrue(iso_utc(row["timestamp"]))
        # Whitelisted numeric buckets only: the string tier, the unknown key and
        # the non-numeric nested entry are all absent.
        self.assertEqual(row["usage"], {
            "input_tokens": 100, "output_tokens": 7, "cache_read_input_tokens": 3,
            "cache_creation_input_tokens": 0,
            "cache_creation": {"ephemeral_5m_input_tokens": 2}})
        # Private by mode, and nothing secret-derived is in the row: not the
        # identity digest, not the key, not its hash, not the rendered config path.
        self.assertEqual((self.state / "ccmorph" / "usage").stat().st_mode & 0o077, 0)
        self.assertEqual(row_path.stat().st_mode & 0o077, 0)
        text = json.dumps(row)
        for secret in (IDENTITY, hashlib.sha256(b"k").hexdigest(), "/nowhere/gateway.conf"):
            self.assertNotIn(secret, text)

    def test_transport_retries_are_counted_in_the_row(self):
        # Two upstream 503s, then the real message: the row records both retries,
        # and `attempt` is still the launcher's tag, not the shim's count.
        CaptureUpstream.fail_first = 2
        port = self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC, "CCMORPH_RETRY_BASE": "0"},
                         flags=("--retry-transport",))
        with self.post(port) as r:
            self.assertEqual(upstream_bytes(self, r.read()), CAPTURE_JSON)
        row = self.wait_for(self.row_path("msg_capture_json"))
        self.assertEqual(row["retries"], 2)
        self.assertEqual(row["attempt"], 1)

    def test_the_unexpected_eof_replay_is_counted_in_the_row_too(self):
        # Flag off: the shim still re-sends once, for the exact unexpected-EOF
        # 500, and the row says so in the same field.
        CaptureUpstream.eof_first = 1
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            self.assertEqual(upstream_bytes(self, r.read()), CAPTURE_JSON)
        row = self.wait_for(self.row_path("msg_capture_json"))
        self.assertEqual(row["retries"], 1)

    def test_explicit_model_map_records_request_and_target_but_keeps_picker_identity(self):
        for mode in ("json", "sse"):
            with self.subTest(mode=mode):
                CaptureUpstream.mode = mode
                seen = []
                CaptureUpstream.on_body = seen.append
                self.addCleanup(setattr, CaptureUpstream, "on_body", None)
                CaptureUpstream.sse_body = CAPTURE_SSE.replace('"message":{"id"', '"message":{"type":"message","id"')
                spec = dict(json.loads(ROUTE_SPEC), config=None,
                            model_map={"claude-sonnet-5-5": "gpt-6.1-sol"})
                asked = "claude-sonnet-5-5[1m]"
                with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": json.dumps(spec)}),
                               body=json.dumps({"model": asked, "messages": []}).encode()) as r:
                    data = r.read()
                self.assertEqual(json.loads(seen[0])["model"], "gpt-6.1-sol")
                self.assertIn(b'claude-sonnet-5-5', data)
                response_id = "msg_capture_json" if mode == "json" else "msg_capture_sse"
                row = self.wait_for(self.row_path(response_id), lambda r: r["complete"])
                self.assertEqual((row["requested_model"], row["served_model"], row["response_model"]),
                                 (asked, "gpt-6.1-sol", "claude-sonnet-5-5"))

    def test_model_map_is_exact_single_hop_and_unknown_ids_pass_through(self):
        # Keys are wire model ids; the context suffix is invisible to matching.
        # The suffixed key is in the spec on purpose: a map that still carried one
        # must not decide a route the launch never prepared, so both spellings
        # resolve through the bare key and an unknown id is untouched.
        spec = dict(json.loads(ROUTE_SPEC), config=None, model_map={
            "m": "mapped", "mapped": "do-not-chain", "m[1m]": "never-prepared"})
        seen = []
        CaptureUpstream.on_body = seen.append
        self.addCleanup(setattr, CaptureUpstream, "on_body", None)
        port = self.shim(env={"CCMORPH_MODEL_ROUTE": json.dumps(spec)})
        for asked, expected in (("m", "mapped"), ("m[1m]", "mapped"), ("unknown", "unknown")):
            with self.post(port, body=json.dumps({"model": asked}).encode()) as r:
                r.read()
            self.assertEqual(json.loads(seen[-1])["model"], expected)

    def test_mapping_does_not_leak_to_another_profile_and_absent_is_byte_identical(self):
        seen = []
        CaptureUpstream.on_body = seen.append
        self.addCleanup(setattr, CaptureUpstream, "on_body", None)
        for profile, mapping, expected in (("mapped", {"m": "target"}, "target"),
                                           ("unmapped", None, "m")):
            spec = dict(json.loads(ROUTE_SPEC), config=None, profile=profile)
            if mapping is not None:
                spec["model_map"] = mapping
            with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": json.dumps(spec)})) as r:
                r.read()
            self.assertEqual(json.loads(seen[-1])["model"], expected)
        self.assertEqual(seen[-1], REQUEST_BODY)

    def test_count_tokens_uses_the_same_explicit_map_without_a_usage_row(self):
        seen = []
        CaptureUpstream.on_body = seen.append
        self.addCleanup(setattr, CaptureUpstream, "on_body", None)
        spec = dict(json.loads(ROUTE_SPEC), config=None, model_map={"m": "target"})
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": json.dumps(spec)}),
                       path="/v1/messages/count_tokens") as r:
            r.read()
        self.assertEqual(json.loads(seen[0])["model"], "target")
        self.assertEqual(self.rows(), {})

    def test_nonstream_cost_is_kept_at_the_consumer_join_path(self):
        body = json.loads(CAPTURE_JSON)
        body["usage"]["cost"] = 0.072112
        CaptureUpstream.json_body = json.dumps(body).encode()
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            r.read()
        row = self.wait_for(self.row_path("msg_capture_json"))
        self.assertEqual(row["usage"]["cost"], 0.072112)

    def test_sse_billed_cost_is_kept_from_each_usage_event(self):
        sidecar = load_shim_module().UsageSidecar("p", "identity")
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            for kind in ("message_start", "message_delta", "message_stop"):
                with self.subTest(kind=kind):
                    record = None
                    for event in (json.loads(line[5:]) for line in CAPTURE_SSE.splitlines()
                                  if line.startswith("data:")):
                        if event["type"] == kind:
                            usage = event["message"]["usage"] if kind == "message_start" else event.setdefault("usage", {})
                            usage.update(cost=0.5, private="not receipt data")
                        record = sidecar.observe(event, record, "m")
                    rows = [r for r in self.rows().values() if r["turn_id"] == record["turn_id"]]
                    self.assertEqual(len(rows), 1)
                    self.assertTrue(rows[0]["complete"])
                    self.assertEqual(rows[0]["usage"]["cost"], 0.5)
                    self.assertEqual(rows[0]["usage"]["output_tokens"], 42)
                    self.assertNotIn("private", rows[0]["usage"])

    def test_unusable_costs_stay_out_of_the_row(self):
        shim = load_shim_module()
        usage_buckets = shim.usage_buckets
        for bad in (-0.01, float("nan"), float("inf"), True, "0.1", None, [1],
                    10 ** 308, 1e308, 9.1e13, shim.COST_LIMIT):
            self.assertNotIn("cost", usage_buckets({"output_tokens": 1, "cost": bad}), bad)
        self.assertEqual(usage_buckets({"cost": 0}), {"cost": 0})
        self.assertEqual(usage_buckets({"cost": 1.5}), {"cost": 1.5})
        self.assertEqual(usage_buckets({"cost": 1e13}), {"cost": 1e13})

    def test_recycled_response_id_does_not_overwrite_the_earlier_turn(self):
        sidecar = load_shim_module().UsageSidecar("p", "identity")
        environment = mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)})
        environment.start()
        self.addCleanup(environment.stop)
        first = sidecar._start("msg_dup", "m", "m", {"output_tokens": 1})
        first["timestamp"] = "2026-10-15T00:00:00+00:00"
        second = sidecar._start("msg_dup", "m", "m", {"output_tokens": 2})
        second["timestamp"] = "2026-10-15T00:00:05+00:00"
        sidecar._write(first)
        sidecar._write(second)
        first["complete"] = True            # same-record rewrite keeps its path
        sidecar._write(first)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual(json.loads(self.row_path("msg_dup").read_text()), first)
        spill = hashlib.sha256(
            ("msg_dup." + second["turn_id"]).encode()).hexdigest() + ".json"
        self.assertEqual(self.rows()[spill], second)

    def test_legacy_row_without_turn_id_is_not_owned_by_a_new_turn(self):
        sidecar = load_shim_module().UsageSidecar("p", "identity")
        legacy = sidecar._start("msg_legacy", "m", "m", {"output_tokens": 1})
        legacy.pop("turn_id")
        path = self.row_path("msg_legacy")
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps(legacy))
        record = sidecar._start("msg_legacy", "m", "m", {"output_tokens": 2})
        record["timestamp"] = legacy["timestamp"]
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            sidecar._write(record)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual(json.loads(path.read_text()), legacy)
        self.assertIn(record, self.rows().values())

    def test_recycled_response_id_keeps_rows_without_hardlink_support(self):
        """The non-link fallback must preserve sequential recycled-id rows,
        including when separate sidecar instances own the turns."""
        import errno

        shim = load_shim_module()
        first_writer = shim.UsageSidecar("p", "identity")
        second_writer = shim.UsageSidecar("p", "identity")
        first = first_writer._start("msg_no_link", "m", "m", {"output_tokens": 1})
        first["timestamp"] = "2026-10-15T00:00:00+00:00"
        second = second_writer._start("msg_no_link", "m", "m", {"output_tokens": 2})
        second["timestamp"] = first["timestamp"]
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}), \
                mock.patch.object(shim.os, "link", side_effect=OSError(errno.ENOTSUP, "unsupported")):
            first_writer._write(first)
            second_writer._write(second)
            first["complete"] = True
            first_writer._write(first)
            second["complete"] = True
            second_writer._write(second)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual(json.loads(self.row_path("msg_no_link").read_text()), first)
        spill = hashlib.sha256(
            ("msg_no_link." + second["turn_id"]).encode()).hexdigest() + ".json"
        self.assertEqual(self.rows()[spill], second)

    def test_concurrent_same_id_writes_each_keep_their_row(self):
        """#199: writers racing on one response id (each its own turn, so its
        own timestamp) must not overwrite each other's rows — the path is
        claimed atomically, not picked with an exists() check. #444: the claim
        must stay atomic without hardlinks too — an exclusive create, so the
        racing loser lands in its spill, not on the winner's row."""
        import errno

        shim = load_shim_module()
        sidecar = shim.UsageSidecar("p", "identity")
        for link in (None, OSError(errno.ENOTSUP, "unsupported")):
            with self.subTest(hardlinks=link is None):
                shutil.rmtree(self.state / "ccmorph", ignore_errors=True)
                records = []
                for i in range(8):
                    record = sidecar._start("msg_race", "m", "m", {"output_tokens": i})
                    record["timestamp"] = "2026-10-15T00:00:{:02d}+00:00".format(i)
                    records.append(record)
                barrier = threading.Barrier(len(records), timeout=3)
                errors = []

                def write(record):
                    try:
                        barrier.wait()
                        sidecar._write(record)
                    except Exception as error:
                        errors.append(error)

                threads = [threading.Thread(target=write, args=(r,)) for r in records]
                started = []
                patches = [mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)})]
                if link is not None:
                    patches.append(mock.patch.object(shim.os, "link", side_effect=link))
                with contextlib.ExitStack() as stack:
                    for patch in patches:
                        stack.enter_context(patch)
                    try:
                        for thread in threads:
                            thread.start()
                            started.append(thread)
                        for thread in started:
                            thread.join(timeout=5)
                    finally:
                        barrier.abort()
                        for thread in started:
                            thread.join(timeout=5)
                    self.assertFalse(errors)
                    self.assertFalse(any(t.is_alive() for t in started))
                    rows = self.rows()
                    self.assertEqual(len(rows), len(records))
                    self.assertEqual({r["usage"]["output_tokens"] for r in rows.values()},
                                     set(range(len(records))))
                    # A record's later rewrite (message_delta / message_stop) replaces the
                    # file it owns, never another turn's row.
                    mine = records[3]
                    mine["usage"]["output_tokens"] = 99
                    mine["complete"] = True
                    sidecar._write(mine)
                    rows = self.rows()
                    self.assertEqual(len(rows), len(records))
                    updated = [r for r in rows.values() if r["timestamp"] == mine["timestamp"]]
                    self.assertEqual(len(updated), 1)
                    self.assertIs(updated[0]["complete"], True)
                    self.assertEqual(updated[0]["usage"]["output_tokens"], 99)

    def test_failed_temp_unlink_never_truncates_a_claimed_row(self):
        shim = load_shim_module()
        sidecar = shim.UsageSidecar("p", "identity")
        first = sidecar._start("msg_alias", "m", "m", {"output_tokens": 1})
        second = sidecar._start("msg_alias", "m", "m", {"output_tokens": 2})
        first["timestamp"], second["timestamp"] = "first", "second"
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            with mock.patch.object(shim.os, "unlink", side_effect=PermissionError("fixture unlink")):
                try:
                    sidecar._write(first)
                except OSError:
                    pass
            sidecar._write(second)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual(json.loads(self.row_path("msg_alias").read_text()), first)

    def test_fallback_race_keeps_both_rows_from_the_first_write_issue_444(self):
        """#444: two turns that both observe a free primary — exactly what a
        race opens in the no-hardlink fallback's exists() check — must not
        both take it. The claim is an exclusive create, so the loser lands in
        its spill on that first write: a stream cancelled, or a process
        killed, before its next usage update keeps its row. Updates afterwards
        still recheck the on-disk owner and keep each row where it is (the
        recovery this test pinned before the claim itself stopped displacing
        anyone)."""
        import errno

        shim = load_shim_module()
        writers = [shim.UsageSidecar("p", "identity") for _ in range(2)]
        records = [w._start("msg_fallback_race", "m", "m", {"output_tokens": i})
                   for i, w in enumerate(writers)]
        for i, record in enumerate(records):
            record["timestamp"] = str(i)
        primary = str(self.row_path("msg_fallback_race"))
        real_exists = shim.os.path.exists

        def exists(path):
            # Hold the race window open: every check reports the primary
            # free, exactly what a writer that checked before the other's
            # replace saw.
            return False if path == primary else real_exists(path)

        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            with mock.patch.object(shim.os, "link", side_effect=OSError(errno.ENOTSUP, "unsupported")), \
                    mock.patch.object(shim.os.path, "exists", side_effect=exists):
                for writer, record in zip(writers, records):
                    writer._write(record)
            self.assertEqual(len(self.rows()), 2)
            for writer, record in zip(writers, records):
                record["usage"]["output_tokens"] += 10
                record["complete"] = True
                writer._write(record)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual({r["timestamp"]: r for r in self.rows().values()},
                         {r["timestamp"]: r for r in records})

    def test_removed_claim_reclaimed_by_another_turn_is_not_overwritten(self):
        shim = load_shim_module()
        first_writer, second_writer = [shim.UsageSidecar("p", "identity") for _ in range(2)]
        first = first_writer._start("msg_reclaimed", "m", "m", {"output_tokens": 1})
        second = second_writer._start("msg_reclaimed", "m", "m", {"output_tokens": 2})
        first["timestamp"], second["timestamp"] = "first", "second"
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            first_writer._write(first)
            self.row_path("msg_reclaimed").unlink()
            second_writer._write(second)
            first["complete"] = True
            first_writer._write(first)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual(json.loads(self.row_path("msg_reclaimed").read_text()), second)

    def test_spill_rewrite_keeps_its_path_when_primary_is_removed(self):
        sidecar = load_shim_module().UsageSidecar("p", "identity")
        first = sidecar._start("msg_spill_stable", "m", "m", {"output_tokens": 1})
        second = sidecar._start("msg_spill_stable", "m", "m", {"output_tokens": 2})
        first["timestamp"], second["timestamp"] = "first", "second"
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            sidecar._write(first)
            sidecar._write(second)
            spill = next(p for p in (self.state / "ccmorph" / "usage").glob("*.json")
                         if p != self.row_path("msg_spill_stable"))
            self.row_path("msg_spill_stable").unlink()
            second["usage"]["output_tokens"] = 9
            sidecar._write(second)
            second["complete"] = True
            sidecar._write(second)
        self.assertEqual(self.rows(), {spill.name: second})

    def test_spill_aliasing_another_primary_keeps_every_row(self):
        """#320: both names of a turn can belong to other turns — the primary to
        a recycled id, the spill to a row whose own primary is that spill name.
        The write must move on to a further name, never replace either row."""
        import errno

        shim = load_shim_module()
        sidecar = shim.UsageSidecar("p", "identity")
        for link in (None, OSError(errno.ENOTSUP, "unsupported")):
            with self.subTest(hardlinks=link is None):
                shutil.rmtree(self.state / "ccmorph", ignore_errors=True)
                unrelated = sidecar._start("msg_a", "m", "m", {"cost": 1})
                aliased = sidecar._start("msg_a", "m", "m", {"cost": 2})
                other = sidecar._start("msg_a." + aliased["turn_id"], "m", "m", {"cost": 3})
                patches = [mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)})]
                if link is not None:
                    patches.append(mock.patch.object(shim.os, "link", side_effect=link))
                with contextlib.ExitStack() as stack:
                    for patch in patches:
                        stack.enter_context(patch)
                    for record in (unrelated, other, aliased):
                        sidecar._write(record)
                    rows = list(self.rows().values())
                    self.assertCountEqual(rows, [unrelated, aliased, other])
                    self.assertEqual(sum(r["usage"]["cost"] for r in rows), 6)
                    for record in (aliased, other):    # rewrites keep their own rows
                        record["complete"] = True
                        sidecar._write(record)
                    rows = list(self.rows().values())
                    self.assertCountEqual(rows, [unrelated, aliased, other])
                    self.assertEqual(sum(r["usage"]["cost"] for r in rows), 6)
                    # Every one of its five names held by other turns: this turn
                    # loses its own row and leaves all theirs untouched.
                    blocked = sidecar._start("msg_a", "m", "m", {"cost": 4})
                    for key in ["msg_a." + blocked["turn_id"]] + [
                            "{}.{}".format("msg_a." + blocked["turn_id"], n)
                            for n in range(1, 4)]:
                        sidecar._write(sidecar._start(key, "m", "m", {"cost": 5}))
                    before = self.rows()
                    self.assertEqual(len(before), 7)
                    with self.assertRaises(FileExistsError):
                        sidecar._write(blocked)
                    self.assertEqual(self.rows(), before)
                    usage = self.state / "ccmorph" / "usage"
                    self.assertEqual(list(usage.glob("*.tmp")), [])

    def test_stream_exits_do_not_retain_shared_claims(self):
        shim = load_shim_module()
        sidecar = shim.UsageSidecar("p", "identity")
        start = CAPTURE_SSE.split("event:message_delta")[0].encode()
        error = b'event: error\ndata: {"type":"error"}\n\n'

        class FailingRead(io.BytesIO):
            def readline(self):
                line = super().readline()
                if not line:
                    raise TimeoutError("fixture stall")
                return line

        cases = ((io.BytesIO(start), io.BytesIO()),
                 (io.BytesIO(start + error), io.BytesIO()),
                 (io.BytesIO(CAPTURE_SSE.encode()),
                  DeadSocket(BrokenPipeError(32, "fixture disconnect"), survive=2)),
                 (FailingRead(start), io.BytesIO()))
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            for response, output in cases:
                handler = shim.IdShim.__new__(shim.IdShim)
                handler.wfile = output
                handler._stream_sse(response, sidecar, "m")
                self.assertFalse(getattr(sidecar, "_claimed", {}))
        self.assertEqual(len(self.rows()), len(cases))
        self.assertTrue(all(not row["complete"] for row in self.rows().values()))

    def test_concurrent_fixture_restores_inherited_state_home(self):
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": "fixture-inherited"}):
            case = UsageSidecarTest("test_concurrent_same_id_writes_each_keep_their_row")
            case.state = self.state
            try:
                case.test_concurrent_same_id_writes_each_keep_their_row()
            finally:
                case.doCleanups()
            self.assertEqual(os.environ.get("XDG_STATE_HOME"), "fixture-inherited")

    def test_concurrent_fixture_partial_thread_start_failure_exits(self):
        # A start failure mid-fixture must leave no worker parked at the
        # barrier: liveness is read straight after the fixture raises, before
        # the fixture's 3 s barrier timeout could rescue a leaked worker.
        case = UsageSidecarTest("test_concurrent_same_id_writes_each_keep_their_row")
        case.state = self.state
        original = threading.Thread.start
        started = []

        def start(thread):
            if started:
                raise RuntimeError("fixture thread start")
            started.append(thread)
            return original(thread)

        try:
            with mock.patch.object(threading.Thread, "start", start):
                with self.assertRaisesRegex(RuntimeError, "fixture thread start"):
                    case.test_concurrent_same_id_writes_each_keep_their_row()
        finally:
            case.doCleanups()
        self.assertEqual(len(started), 1)
        self.assertFalse(started[0].is_alive())

    def test_route_id_is_stable_across_processes_and_never_the_identity(self):
        route_ids = set()
        for _ in range(2):                 # two shim PROCESSES, one route spec
            shutil.rmtree(self.state / "ccmorph" / "usage", ignore_errors=True)
            with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
                r.read()
            route_ids.add(self.wait_for(self.row_for("msg_capture_json"))["route_id"])
        self.assertEqual(len(route_ids), 1, "route_id changed between shim processes")
        route_id = route_ids.pop()
        self.assertRegex(route_id, r"^[0-9a-f]{64}$")
        self.assertNotIn(route_id, (IDENTITY, hashlib.sha256(IDENTITY.encode()).hexdigest()))

    def test_capture_leaves_the_response_bytes_alone(self):
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            body = r.read()
        self.assertEqual(upstream_bytes(self, body), CAPTURE_JSON)

    def test_sse_row_saved_at_message_start_and_finalized_at_message_stop(self):
        CaptureUpstream.mode = "sse"
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            body = r.read()
        row_path = self.row_for("msg_capture_sse")
        row = self.wait_for(row_path, lambda r: r["complete"])
        self.assertEqual(len(list(self.rows())), 1)
        self.assertIs(row["complete"], True)
        self.assertEqual(row["response_model"], "m-served")
        # message_start's buckets merged with the message_delta update.
        self.assertEqual(row["usage"], {"input_tokens": 11,
                                        "cache_read_input_tokens": 2,
                                        "output_tokens": 42})
        self.assertEqual(upstream_bytes(self, body), CAPTURE_SSE.encode())

    def test_partial_row_exists_before_the_stream_finishes(self):
        """The row is durable from message_start on: kill the shim or the
        process mid-turn and the incomplete row is still there."""
        CaptureUpstream.mode = "gated"
        port = self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=HANG_TIMEOUT)
        self.addCleanup(conn.close)
        conn.putrequest("POST", "/v1/messages", skip_accept_encoding=True)
        conn.putheader("content-type", "application/json")
        conn.putheader("x-api-key", "k")
        conn.putheader("content-length", str(len(REQUEST_BODY)))
        conn.endheaders()
        conn.send(REQUEST_BODY)
        resp = conn.getresponse()          # headers first, then events
        started = False
        while True:
            line = resp.fp.readline()
            if not line:
                self.fail("stream ended before the row check")
            if line.startswith(b"data:") and b"message_start" in line:
                started = True
            elif started and line in (b"\n", b"\r\n"):
                break
        path = self.row_for("msg_capture_sse")
        row = self.wait_for(path, lambda r: r["usage"] == {"input_tokens": 11,
                                                          "cache_read_input_tokens": 2})
        self.assertIs(row["complete"], False)
        CaptureUpstream.gate.set()
        resp.read()
        row = self.wait_for(path, lambda r: r["complete"])
        self.assertEqual(row["usage"], {"input_tokens": 11, "cache_read_input_tokens": 2,
                                        "output_tokens": 42})

    def test_truncated_stream_leaves_an_incomplete_row(self):
        CaptureUpstream.mode = "truncated"
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            r.read()
        row = self.wait_for(self.row_for("msg_capture_sse"))
        self.assertEqual(len(self.rows()), 1)
        self.assertEqual(row["response_id"], "msg_capture_sse")
        self.assertIs(row["complete"], False)
        self.assertEqual(row["usage"], {"input_tokens": 11, "cache_read_input_tokens": 2})

    def test_client_disconnect_keeps_the_partial_row_and_is_swallowed(self):
        """Unit-level, like ClientDisconnectTest: a wfile that dies after the
        first event must not lose the row already written, and the disconnect
        must still be swallowed rather than reach the shared adapter log."""
        shim = load_shim_module()
        sidecar = shim.UsageSidecar("test-profile", IDENTITY)
        handler = shim.IdShim.__new__(shim.IdShim)
        # Two writes get through — the message_start event's two lines, which is
        # where the row is written — and the blank line after it is the last the
        # client's socket ever hears.
        handler.wfile = DeadSocket(BrokenPipeError(32, "Broken pipe"), survive=2)
        handler.close_connection = False
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            handler._stream_sse(io.BytesIO(CAPTURE_SSE.encode()), sidecar, "m")
        self.assertTrue(handler.close_connection)
        row = next(iter(self.rows().values()))
        self.assertEqual(row["response_id"], "msg_capture_sse")
        self.assertIs(row["complete"], False)

    def test_unrelated_endpoints_are_never_captured(self):
        port = self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})
        with self.post(port, path="/json") as r:
            self.assertEqual(r.status, 200)
        try:
            urllib.request.urlopen(urllib.request.Request(
                f"http://127.0.0.1:{port}/v1/messages", method="GET"), timeout=HANG_TIMEOUT).read()
        except urllib.error.HTTPError:
            pass                            # the stub answers POST only
        self.assertFalse((self.state / "ccmorph" / "usage").exists())

    def test_no_route_spec_writes_nothing(self):
        port = self.shim(env={"CCMORPH_MODEL_ROUTE": ""})
        with self.post(port) as r:
            self.assertEqual(upstream_bytes(self, r.read()), CAPTURE_JSON)
        self.assertFalse((self.state / "ccmorph" / "usage").exists())

    def test_unwritable_state_directory_never_changes_the_response(self):
        # A file where the sidecar wants its directory: every write fails.
        (self.state / "ccmorph").write_text("not a directory")
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            body = r.read()
        self.assertEqual(upstream_bytes(self, body), CAPTURE_JSON)



    # #173: the task headers only tag the row; the shim never changes the requested model.
    def tag_shim(self):
        self.seen = []
        CaptureUpstream.on_body = self.seen.append
        self.addCleanup(setattr, CaptureUpstream, "on_body", None)
        return self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})

    def row(self):
        return self.wait_for(self.row_path("msg_capture_json"))

    def test_task_tags_the_row_and_never_changes_the_model(self):
        h = {"x-ccmorph-task": "issue-42", "x-ccmorph-attempt": "2"}
        port = self.tag_shim()
        with self.post(port, headers=h) as r:
            r.read()
        self.assertEqual(self.seen[0], REQUEST_BODY)
        row = self.row()
        self.assertEqual((row["task"], row["attempt"]), ("issue-42", 2))
        self.assertNotIn("role", row)
        self.assertEqual((row["requested_model"], row["response_model"]), ("m", "m-served"))

    def test_no_tags_leave_the_request_unchanged(self):
        port = self.tag_shim()
        with self.post(port) as r:
            r.read()
        self.assertEqual(self.seen[0], REQUEST_BODY)
        row = self.row()
        self.assertEqual((row["task"], row["attempt"]), (None, 1))

    def test_malformed_values_are_not_trusted(self):
        port = self.tag_shim()
        for task in ("ok", "/etc/passwd", "a b"):
            self.seen.clear()
            with self.post(port, headers={"x-ccmorph-task": task}) as r:
                r.read()
            self.assertEqual(self.seen[0], REQUEST_BODY, task)
        # Every request answers with the same response id, so each spills to its own row.
        deadline = time.monotonic() + HANG_TIMEOUT
        while len(self.rows()) < 3 and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertEqual(sorted(str(r["task"]) for r in self.rows().values()), ["None", "None", "ok"])

    # #210: tags are ASCII-only full matches — "²" satisfies isdigit() but raises
    # in int(), and "$" also matches before a trailing newline. Header values are
    # strip()ed first, so a trailing newline tags as its clean ASCII core; the
    # raw regexes must refuse one outright (record_outcome takes argv, no strip).
    def test_tags_are_ascii_full_matches(self):
        shim = load_shim_module()
        self.assertEqual(shim.task_tags({"x-ccmorph-attempt": "\xb2"})["attempt"], 1)
        self.assertEqual(shim.task_tags({"x-ccmorph-task": "x\n"})["task"], "x")
        self.assertIsNone(shim.TASK_RE.fullmatch("x\n"))

    def test_ccmorph_headers_never_reach_the_upstream(self):
        port = self.tag_shim()
        CaptureUpstream.on_headers = self.seen.append
        self.addCleanup(setattr, CaptureUpstream, "on_headers", None)
        # #387: the retired role tag is the header a session started before #348
        # still carries. Any x-ccmorph-* name ends here, listed or not.
        h = {"x-ccmorph-task": "t1", "x-ccmorph-attempt": "1", "x-ccmorph-role": "build"}
        with self.post(port, headers=h) as r:
            r.read()
        self.assertFalse([k for k in self.seen[-1] if k.lower().startswith("x-ccmorph")])

    def test_outcome_appends_a_row_keyed_by_task(self):
        env = dict(os.environ, XDG_STATE_HOME=str(self.state))
        for result in ("accepted", "parked"):
            subprocess.run([sys.executable, str(SHIM), "outcome", "issue-42", result], env=env, check=True, timeout=HANG_TIMEOUT)
        bad = subprocess.run([sys.executable, str(SHIM), "outcome", "/etc/x", "accepted"], env=env,
                             capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(bad.returncode, 0)
        bad = subprocess.run([sys.executable, str(SHIM), "outcome", "t", "great"], env=env, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(bad.returncode, 0)
        # #210: "$" also matches before a trailing newline, so this used to append a row.
        bad = subprocess.run([sys.executable, str(SHIM), "outcome", "x\n", "accepted"], env=env,
                             capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(bad.returncode, 0)
        self.assertIn(b"usage:", bad.stderr)
        lines = [json.loads(x) for x in (self.state / "ccmorph" / "outcomes.jsonl").read_text().splitlines()]
        self.assertEqual([(x["task"], x["outcome"]) for x in lines],
                         [("issue-42", "accepted"), ("issue-42", "parked")])


if __name__ == "__main__":
    unittest.main()


class GatewayEdgeUpstream(BaseHTTPRequestHandler):
    """A gateway with the three habits the shim normalizes: it answers under an
    internal model name, its catalogue has moved past the asked Opus, and it
    wraps errors in hex."""
    protocol_version = "HTTP/1.1"
    catalogue = ["claude-opus-5-6", "claude-opus-5", "claude-sonnet-5", "deepseek-v4-pro"]
    seen_models = []
    endpoints = {}

    def log_message(self, *args):
        pass

    def _send(self, status, ctype, body):
        self.send_response(status)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._send(200, "application/json",
                   json.dumps({"data": [{"id": m, **({"supported_endpoints": self.endpoints[m]}
                                                if m in self.endpoints else {})}
                                         for m in self.catalogue]}).encode())

    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers.get("content-length") or 0)))
        GatewayEdgeUpstream.seen_models.append(payload["model"])
        if payload.get("fail"):
            inner = json.dumps({"message": "messages.1.content.1: Input tag 'tool_addition' found using 'type'"})
            body = {"code": 400, "msg": "client_error",
                    "detailMsg": f"bedrock-claude 400 client error SdkBytes(bytes=0x{inner.encode().hex()})"}
            return self._send(400, "application/json", json.dumps(body).encode())
        message = {"id": "msg_1", "type": "message", "role": "assistant",
                   "model": "Int_Cl_Opus_5.6_ULT", "content": [], "usage": {}}
        if payload.get("stream"):
            sse = ("event:message_start\n"
                   f"data:{json.dumps({'type': 'message_start', 'message': message})}\n\n"
                   "event:message_stop\ndata:{\"type\":\"message_stop\"}\n\n")
            return self._send(200, "text/event-stream", sse.encode())
        self._send(200, "application/json", json.dumps(message).encode())


class GatewayEdgeTest(unittest.TestCase):
    """Anthropic ids in and out, whatever the gateway calls its models."""

    KEY = "edge-key"

    @classmethod
    def setUpClass(cls):
        cls.shim = load_shim_module()

    def test_resolution_keeps_served_ids_and_never_crosses_families(self):
        served = {"claude-opus-5-6", "claude-opus-5", "claude-sonnet-5", "kimi-k3"}
        resolve = self.shim.resolve_model
        self.assertEqual(resolve("claude-sonnet-5", served), "claude-sonnet-5")
        self.assertEqual(resolve("claude-opus-5-5", served), "claude-opus-5-6")
        self.assertEqual(resolve("claude-haiku-4-5", served), "claude-haiku-4-5")
        self.assertEqual(resolve("kimi-k3", served), "kimi-k3")
        self.assertEqual(resolve("claude-opus-5-5", set()), "claude-opus-5-5")
        self.assertEqual(resolve("claude-opus-5-5", served | {"claude-opus-5-5-20260101"}),
                         "claude-opus-5-5-20260101")
        self.assertEqual(self.shim.claude_version("claude-opus-4-5-20251101"), ("opus", (4, 5)))

    def test_hex_wrapped_error_is_unwrapped_and_others_pass(self):
        self.assertIsNone(self.shim.unwrap_gateway_error(b'{"type":"error","error":{"message":"x"}}'))
        self.assertIsNone(self.shim.unwrap_gateway_error(b"not json"))

    def post(self, body, mapping=None, path="/v1/messages"):
        GatewayEdgeUpstream.seen_models.clear()
        srv = serve(GatewayEdgeUpstream)
        self.addCleanup(srv.shutdown)
        upstream = f"http://127.0.0.1:{srv.server_address[1]}"
        spec = {"profile": "edge", "identity": "i", "upstream": upstream, "models": [],
                "model_map": mapping or {},
                "timeout": 5, "key_sha256": hashlib.sha256(self.KEY.encode()).hexdigest()}
        self.proc, port = start_shim(self, upstream, env={"CCMORPH_MODEL_ROUTE": json.dumps(spec),
                                                          "XDG_STATE_HOME": tempfile.mkdtemp()},
                                     flags=["--preserve-tool-ids"])
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}{path}", data=json.dumps(body).encode(),
            headers={"content-type": "application/json", "x-api-key": self.KEY}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def test_native_edge_rejects_declared_chat_map_before_forwarding(self):
        with mock.patch.object(GatewayEdgeUpstream, "endpoints",
                          {"deepseek-v4-pro": ["/v1/chat/completions"]}):
            for path in ("/v1/messages", "/v1/messages/count_tokens"):
                with self.subTest(path=path):
                    status, data = self.post({"model": "claude-sonnet-5[1m]", "messages": []},
                                             {"claude-sonnet-5": "deepseek-v4-pro[1m]"}, path)
                    self.assertEqual(status, 400)
                    self.assertEqual(GatewayEdgeUpstream.seen_models, [])
                    error = json.loads(data)["error"]
                    self.assertEqual(error["type"], "invalid_request_error")
                    self.assertIn("deepseek-v4-pro", error["message"])
                    self.assertIn("translation", error["message"])

    def test_native_known_mapped_target_uses_current_endpoint_declaration(self):
        spec = dict(json.loads(ROUTE_SPEC), config=None, models=['chat-only'],
                    model_map={'claude-sonnet-5': 'chat-only'})
        routes = self.shim.ModelRoutes(spec)
        routes.catalogue.endpoints = {'chat-only': ['/v1/chat/completions']}
        with mock.patch.object(routes.catalogue, 'served') as served:
            error = routes.ensure(b'{"model":"chat-only"}', 'k', 'claude-sonnet-5')
        self.assertIsNotNone(error)
        self.assertIn('translation', error)
        served.assert_called_once_with('k', 'chat-only')

    def test_native_edge_does_not_treat_a_suffixed_key_as_a_map(self):
        # A key carrying the [1m] suffix never matches a wire id and is refused
        # where the profile is read, so a spec that still carries one decides
        # nothing: the request is not a mapped one, and the guard must not go to
        # the gateway -- or answer 400 -- over a map no lookup applied.
        spec = dict(json.loads(ROUTE_SPEC), config=None, models=[],
                    model_map={'claude-sonnet-5[1m]': 'chat-only'})
        routes = self.shim.ModelRoutes(spec)
        routes.catalogue.endpoints = {'chat-only': ['/v1/chat/completions']}
        with mock.patch.object(routes.catalogue, 'served') as served:
            error = routes.ensure(b'{"model":"claude-sonnet-5"}', 'k', 'claude-sonnet-5[1m]')
        self.assertIsNone(error)
        served.assert_not_called()

    def test_native_map_allows_declared_count_tokens_but_rejects_messages(self):
        with mock.patch.object(GatewayEdgeUpstream, 'endpoints',
                               {'deepseek-v4-pro': ['/v1/messages/count_tokens',
                                                    '/v1/chat/completions']}):
            for path, expected in (('/v1/messages/count_tokens?beta=1', 200),
                                   ('/v1/messages', 400)):
                with self.subTest(path=path):
                    status, _ = self.post({'model': 'claude-sonnet-5', 'messages': []},
                                          {'claude-sonnet-5': 'deepseek-v4-pro'}, path)
                    self.assertEqual(status, expected)
                    self.assertEqual(GatewayEdgeUpstream.seen_models,
                                     ['deepseek-v4-pro'] if expected == 200 else [])

    def test_native_catalogue_uses_first_duplicate_row_even_if_malformed(self):
        count_tokens = '/v1/messages/count_tokens'
        for first, second, path, rejected, expected in (
                (['/v1/messages'], ['/v1/chat/completions'], '/v1/messages', False, ['/v1/messages']),
                (['/v1/chat/completions'], ['/v1/messages'], '/v1/messages', True, ['/v1/chat/completions']),
                (None, ['/v1/chat/completions'], '/v1/messages', False, None),
                ([], ['/v1/chat/completions'], '/v1/messages', True, ['/v1/chat/completions']),
                ([count_tokens], ['/v1/chat/completions'], '/v1/messages', True, ['/v1/chat/completions', count_tokens]),
                ([count_tokens], ['/v1/chat/completions'], count_tokens, False, ['/v1/chat/completions', count_tokens]),
                (['/v1/chat/completions'], [count_tokens], count_tokens, False, ['/v1/chat/completions', count_tokens]),
                (['/unknown'], ['/v1/responses'], '/v1/messages', True, ['/v1/responses'])):
            with self.subTest(issue=233, first=first, second=second, path=path):
                spec = dict(json.loads(ROUTE_SPEC), config=None, models=[],
                            upstream='https://gateway', model_map={'alias': 'm'})
                routes = self.shim.ModelRoutes(spec)
                data = {'data': [{'id': 'm', 'supported_endpoints': first},
                                 {'id': 'm', 'supported_endpoints': second}]}
                with mock.patch.object(self.shim.OPENER, 'open',
                                       return_value=io.BytesIO(json.dumps(data).encode())), \
                     mock.patch.object(self.shim.time, 'monotonic', return_value=0.25):
                    error = routes.ensure(b'{"model":"m"}', 'k', 'alias', path)
                self.assertEqual(error is not None, rejected)
                self.assertEqual(routes.catalogue.endpoints.get('m'), expected)

    def test_native_catalogue_refresh_keeps_previous_declarations_visible(self):
        spec = dict(json.loads(ROUTE_SPEC), config=None, models=[],
                    upstream='https://gateway', model_map={'alias': 'm'})
        routes = self.shim.ModelRoutes(spec)
        routes.catalogue.endpoints = {'m': ['/v1/chat/completions']}
        observed = []

        class Row(dict):
            def get(row, key, default=None):
                if key == 'supported_endpoints':
                    with mock.patch.object(routes.catalogue, 'served', return_value={'m'}):
                        observed.append(routes.ensure(b'{"model":"m"}', 'k', 'alias'))
                return super().get(key, default)

        data = {'data': [Row(id='m', supported_endpoints=['/v1/chat/completions'])]}
        parse = self.shim.json.loads
        with mock.patch.object(self.shim.OPENER, 'open', return_value=io.BytesIO(b'catalogue')), \
             mock.patch.object(self.shim.json, 'loads', side_effect=lambda body:
                               data if body == b'catalogue' else parse(body)):
            routes.catalogue.served('k', 'm')
        self.assertEqual(len(observed), 1)
        self.assertIn('translation', observed[0])
        self.assertEqual(routes.catalogue.endpoints, {'m': ['/v1/chat/completions']})

    def test_native_expired_declaration_does_not_veto_after_failed_refresh(self):
        spec = dict(json.loads(ROUTE_SPEC), config=None, models=[],
                    upstream='https://gateway', model_map={'alias': 'm'})
        routes = self.shim.ModelRoutes(spec)
        routes.catalogue.ids = {'m'}
        routes.catalogue.endpoints = {'m': ['/v1/chat/completions']}
        routes.catalogue.read_at = 1.0
        with mock.patch.object(self.shim.time, 'monotonic', return_value=10000.0), \
             mock.patch.object(self.shim.OPENER, 'open', side_effect=OSError('unavailable')):
            self.assertIsNone(routes.ensure(b'{"model":"m"}', 'k', 'alias'))
        self.assertEqual(routes.catalogue.ids, set())
        self.assertEqual(routes.catalogue.endpoints, {})

    def test_translated_map_uses_existing_preparer_not_native_guard(self):
        spec = dict(json.loads(ROUTE_SPEC), models=[],
                    model_map={"claude-sonnet-5": "chat-only"})
        routes = self.shim.ModelRoutes(spec)
        with mock.patch.object(routes.catalogue, "served", side_effect=AssertionError("native guard")), \
             mock.patch.object(self.shim.subprocess, "run") as prepare:
            prepare.return_value.returncode = 0
            body = b'{"model":"chat-only"}'
            self.assertIsNone(routes.ensure(body, "k", "claude-sonnet-5"))
            self.assertEqual(prepare.call_args.args[0][-2:], ["--model", "chat-only"])
            self.assertIsNone(routes.ensure(body, "k", "claude-sonnet-5"))
            prepare.assert_called_once()

    def test_native_map_preserves_messages_and_undeclared_targets(self):
        for endpoints in (["/v1/messages"], ["/v1/messages", "/v1/chat/completions"], [], None):
            with self.subTest(endpoints=endpoints), mock.patch.object(
                    GatewayEdgeUpstream, "endpoints",
                    {"deepseek-v4-pro": endpoints} if endpoints else {}):
                status, data = self.post({"model": "claude-sonnet-5", "messages": []},
                                         {"claude-sonnet-5": "deepseek-v4-pro"})
                self.assertEqual(status, 200)
                self.assertEqual(GatewayEdgeUpstream.seen_models, ["deepseek-v4-pro"])
                self.assertEqual(json.loads(data)["model"], "claude-sonnet-5")

    def test_native_unmapped_target_is_not_subject_to_map_guard(self):
        with mock.patch.object(GatewayEdgeUpstream, "endpoints",
                          {"deepseek-v4-pro": ["/v1/chat/completions"]}):
            status, _ = self.post({"model": "deepseek-v4-pro", "messages": []},
                                  {"claude-sonnet-5": "deepseek-v4-pro"})
        self.assertEqual(status, 200)
        self.assertEqual(GatewayEdgeUpstream.seen_models, ["deepseek-v4-pro"])

    def test_retired_opus_is_sent_as_newest_opus_and_answers_under_it(self):
        status, data = self.post({"model": "claude-opus-5-5[1m]", "messages": []})
        self.assertEqual(status, 200)
        self.assertEqual(GatewayEdgeUpstream.seen_models, ["claude-opus-5-6[1m]"])
        self.assertEqual(json.loads(data)["model"], "claude-opus-5-6")

    def test_never_falls_back_to_an_older_model(self):
        # Served: opus 5-6 and 5. Asking for 5-7 has nothing >= it: sent unchanged, one log line.
        self.post({"model": "claude-opus-5-7", "messages": []})
        self.assertEqual(GatewayEdgeUpstream.seen_models, ["claude-opus-5-7"])
        self.proc.terminate()
        self.assertEqual(self.proc.stdout.read().count("sending it unchanged"), 1)

    def test_resolution_never_downgrades(self):
        resolve = self.shim.resolve_model
        older = {"claude-opus-5", "claude-opus-5-4-20260101", "claude-sonnet-5-5"}
        self.assertEqual(resolve("claude-opus-5-5", older), "claude-opus-5-5")
        self.assertEqual(resolve("claude-opus-5-5", older | {"claude-opus-5-5-20260301"}),
                         "claude-opus-5-5-20260301")
        self.assertEqual(resolve("claude-opus-5-1", older | {"claude-opus-5-6"}), "claude-opus-5-6")

    def test_zero_padded_versions_match_the_unsuffixed_served_id(self):
        resolve = self.shim.resolve_model
        self.assertEqual(resolve("claude-opus-4-0", {"claude-opus-4"}), "claude-opus-4")
        self.assertEqual(resolve("claude-sonnet-4-0", {"claude-sonnet-4"}), "claude-sonnet-4")
        self.assertEqual(resolve("claude-opus-5-0", {"claude-opus-5"}), "claude-opus-5")
        # An older served version is still refused, zero-padded or not.
        self.assertEqual(resolve("claude-opus-5-0", {"claude-opus-4"}), "claude-opus-5-0")

    def test_stream_answers_under_the_anthropic_id(self):
        _, data = self.post({"model": "claude-sonnet-5", "stream": True, "messages": []})
        self.assertEqual(GatewayEdgeUpstream.seen_models, ["claude-sonnet-5"])
        self.assertIn(b'"model":"claude-sonnet-5"', data)
        self.assertNotIn(b"Int_", data)

    def test_non_claude_model_is_left_alone(self):
        _, data = self.post({"model": "deepseek-v4-pro", "messages": []})
        self.assertEqual(GatewayEdgeUpstream.seen_models, ["deepseek-v4-pro"])
        self.assertEqual(json.loads(data)["model"], "Int_Cl_Opus_5.6_ULT")

    def test_gateway_error_reaches_the_client_readable(self):
        status, data = self.post({"model": "claude-sonnet-5", "fail": True, "messages": []})
        self.assertEqual(status, 400)
        self.assertEqual(json.loads(data)["error"]["type"], "invalid_request_error")
        self.assertIn("Input tag 'tool_addition'", json.loads(data)["error"]["message"])


class CatalogueRedirectTest(unittest.TestCase):
    """The catalogue fetch must not carry the API key to a cross-origin redirect target."""

    CANARY = "sk-canary-redirect-test"

    @classmethod
    def setUpClass(cls):
        # The shim builds its opener at import; a proxy in the environment would
        # route the loopback fixtures away, so load it without one.
        saved = {k: os.environ.pop(k) for k in ("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
                                                "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy")
                 if k in os.environ}
        cls.addClassCleanup(os.environ.update, saved)
        cls.shim = load_shim_module()

    def test_cross_origin_redirect_does_not_leak_key(self):
        """The canary key must never reach the redirect destination on another origin."""
        received_headers = {}
        origin_hits = []

        class TargetHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_GET(self):
                received_headers["x-api-key"] = self.headers.get("x-api-key", "")
                received_headers["authorization"] = self.headers.get("authorization", "")
                body = json.dumps({"data": [{"id": "target-model"}]}).encode()
                self.send_response(200)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        target = ThreadingHTTPServer(("127.0.0.1", 0), TargetHandler)
        target.daemon_threads = True
        threading.Thread(target=target.serve_forever, daemon=True).start()
        self.addCleanup(target.shutdown)
        target_url = f"http://127.0.0.1:{target.server_address[1]}"

        class RedirectHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_GET(self):
                origin_hits.append(self.path)
                self.send_response(302)
                self.send_header("location", f"{target_url}/v1/models")
                self.send_header("content-length", "0")
                self.end_headers()

        redirect_srv = ThreadingHTTPServer(("127.0.0.1", 0), RedirectHandler)
        redirect_srv.daemon_threads = True
        threading.Thread(target=redirect_srv.serve_forever, daemon=True).start()
        self.addCleanup(redirect_srv.shutdown)

        cat = self.shim.Catalogue(f"http://127.0.0.1:{redirect_srv.server_address[1]}")
        # A refused redirect is a transport error the fetch swallows: no ids, no
        # exception, so the caller sends the model exactly as asked.
        self.assertEqual(cat.served(self.CANARY, "some-model"), set())
        self.assertTrue(origin_hits, "the request never reached the fixture")

        self.assertEqual(received_headers.get("x-api-key", ""), "",
                         f"x-api-key leaked to redirect target: {received_headers.get('x-api-key')!r}")
        self.assertEqual(received_headers.get("authorization", ""), "",
                         f"Authorization leaked to redirect target: {received_headers.get('authorization')!r}")

    def test_same_origin_redirect_returns_models(self):
        """A same-origin 302 still fetches the catalogue successfully."""
        class SameOriginServer(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_GET(self):
                port = self.server.server_address[1]
                if self.path == "/v1/models":
                    self.send_response(302)
                    self.send_header("location", f"http://127.0.0.1:{port}/models")
                    self.send_header("content-length", "0")
                    self.end_headers()
                else:
                    body = json.dumps({"data": [{"id": "same-origin-model"}]}).encode()
                    self.send_response(200)
                    self.send_header("content-type", "application/json")
                    self.send_header("content-length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)

        srv = ThreadingHTTPServer(("127.0.0.1", 0), SameOriginServer)
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)

        cat = self.shim.Catalogue(f"http://127.0.0.1:{srv.server_address[1]}")
        ids = cat.served(self.CANARY, "same-origin-model")
        self.assertIn("same-origin-model", ids)


class CatalogueDeadlineTest(unittest.TestCase):
    """Synthetic byte arrival and thread waiting keep deadline tests off the clock."""

    def test_trickling_catalogue_is_unreadable_issue_296(self):
        for phase in ("headers", "body"):
            with self.subTest(phase=phase):
                shim = load_shim_module()
                now = [1000.0]
                payload = b'{"data":[{"id":"claude-opus-5-6"}]}'
                threads = []
                waits = []

                class Reader:
                    def __init__(self, target, args, daemon):
                        self.target, self.args = target, args
                        self.pending = True
                        threads.append(self)
                        self.daemon = daemon

                    def start(self):
                        pass

                    def join(self, timeout):
                        # The upstream keeps sending, but its completion is after
                        # the caller's wait expires. No real thread or clock waits.
                        waits.append(timeout)
                        started = now[0]
                        self.target(*self.args)
                        self.pending = now[0] > started + timeout

                    def is_alive(self):
                        return self.pending

                class Response(io.BytesIO):
                    def read1(self, *args):
                        if phase == "body":
                            now[0] += len(payload)
                        return super().read1(*args)

                def open_catalogue(req, timeout):
                    self.assertEqual(req.full_url, "http://upstream.invalid/v1/models")
                    if phase == "headers":
                        now[0] += len(payload)
                    return Response(payload)

                cat = shim.Catalogue("http://upstream.invalid")
                with mock.patch.object(shim.time, "monotonic", side_effect=lambda: now[0]), \
                     mock.patch.object(shim.threading, "Thread", Reader), \
                     mock.patch.object(shim.OPENER, "open", side_effect=open_catalogue):
                    ids = cat.served("canary", "claude-opus-5")
                    self.assertEqual(ids, set())
                    self.assertEqual(shim.resolve_model("claude-opus-5", ids), "claude-opus-5")
                    self.assertFalse(cat.lock.locked())
                    self.assertEqual(waits, [shim.CATALOGUE_DEADLINE])
                    # Even once the retry TTL has expired, an unfinished read
                    # cannot accumulate another worker or publish its late ids.
                    now[0] += shim.CATALOGUE_TTL + 1
                    self.assertEqual(cat.served("canary", "claude-opus-5"), set())
                    self.assertEqual(len(threads), 1)
                    self.assertTrue(threads[0].daemon)
                    threads[0].pending = False
                    self.assertEqual(cat.ids, set(), "late results must not update the cache")
                    # After completion, a healthy refresh can populate the cache.
                    with mock.patch.object(shim.OPENER, "open", side_effect=lambda *a, **kw:
                                           io.BytesIO(payload)):
                        self.assertEqual(cat.served("canary", "claude-opus-5"),
                                         {"claude-opus-5-6"})

    def test_loopback_trickle_reader_recovers_issue_310(self):
        shim = load_shim_module()
        now = [1000.0]
        body_started = threading.Event()
        timer_started = threading.Event()
        release = threading.Event()
        connections = []
        timers = []

        class DeadlineTimer:
            def __init__(self, interval, function):
                self.function = function
                timers.append(self)

            def start(self):
                timer_started.set()

            def cancel(self):
                pass

        class TrickleServer(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_GET(self):
                connections.append(self.connection)
                self.send_response(200)
                if not release.is_set():
                    self.send_header("Transfer-Encoding", "chunked")
                    self.end_headers()
                    # Leave real HTTP chunk framing unfinished, not just JSON.
                    # A bounded read1 also blocks here until transport shutdown.
                    self.wfile.write(b"1000\r\n{\"data\":[ ")
                    self.wfile.flush()
                    body_started.set()
                    release.wait()
                else:
                    body = b'{"data":[{"id":"claude-opus-5-6"}]}'
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)

        class Reader(threading.Thread):
            def join(self, timeout=None):
                # One second is the issue's reader-termination contract, not
                # an elapsed-time assertion; expiry itself uses no real clock.
                if not release.is_set():
                    armed = timer_started.wait(1)
                    if not body_started.wait(1):
                        raise AssertionError("the loopback body never arrived")
                    now[0] += shim.CATALOGUE_DEADLINE + 1
                    if armed:
                        timers[-1].function()
                super().join(1)

        server = serve(TrickleServer)
        cat = shim.Catalogue(f"http://127.0.0.1:{server.server_address[1]}")
        try:
            with mock.patch.object(shim, "OPENER", shim.LOOPBACK), \
                 mock.patch.object(shim.time, "monotonic", side_effect=lambda: now[0]), \
                 mock.patch.object(shim.threading, "Thread", Reader), \
                 mock.patch.object(shim.threading, "Timer", DeadlineTimer):
                self.assertEqual(cat.served("canary", "claude-opus-5"), set())
                self.assertFalse(cat.reader.is_alive(), "expired reader still owns the refresh slot")
                release.set()
                now[0] += shim.CATALOGUE_TTL + 1
                ids = cat.served("canary", "claude-opus-5")
                self.assertEqual(ids, {"claude-opus-5-6"})
                self.assertEqual(shim.resolve_model("claude-opus-5", ids), "claude-opus-5-6")
        finally:
            release.set()
            for connection in connections:
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            if cat.reader:
                cat.reader.join(1)
            server.shutdown()
            server.server_close()

    def test_catalogue_body_size_limit_issue_310(self):
        shim = load_shim_module()
        limit = 1024 * 1024
        payload = b'{"data":[{"id":"served"}]}'
        for size, declared, accepted in ((limit, limit, True),
                                          (limit + 1, limit + 1, False),
                                          (len(payload), 500, False)):
            with self.subTest(size=size, declared=declared):
                wire = (f"HTTP/1.1 200 OK\r\nContent-Length: {declared}\r\n\r\n".encode()
                        + payload + b" " * (size - len(payload)))
                peer = mock.Mock()
                peer.makefile.return_value = io.BytesIO(wire)
                response = http.client.HTTPResponse(peer)
                response.begin()
                cat = shim.Catalogue("http://upstream.invalid")
                with mock.patch.object(shim.OPENER, "open", return_value=response):
                    self.assertEqual(cat.served("canary", "served"),
                                     {"served"} if accepted else set())
                self.assertTrue(response.closed)

    def test_reader_failures_send_as_asked_issues_296_309(self):
        for failure, cached in ((failure, cached)
                                for failure in ("start", "late", "http", "json", "connection")
                                for cached in (set(), {"claude-opus-5-6"})):
            with self.subTest(failure=failure, cached=cached):
                shim = load_shim_module()
                now = [1000.0]
                reader = mock.Mock()
                reader.is_alive.return_value = False

                def make_reader(target, args, daemon):
                    if failure == "start":
                        reader.start.side_effect = RuntimeError("cannot start new thread")
                    else:
                        def finish(timeout):
                            if failure == "late":
                                now[0] += timeout + 1
                            target(*args)
                        reader.join.side_effect = finish
                    return reader

                def open_catalogue(req, timeout):
                    if failure == "http":
                        raise shim.urllib.error.HTTPError(req.full_url, 503, "Unavailable", {}, None)
                    if failure == "connection":
                        raise shim.urllib.error.URLError("connection refused")
                    return io.BytesIO(b'not JSON' if failure == "json" else
                                      b'{"data":[{"id":"late"}]}')

                cat = shim.Catalogue("http://upstream.invalid")
                cat.ids = cached
                cat.endpoints = {model: ["/v1/messages"] for model in cached}
                cat.read_at = now[0] - shim.CATALOGUE_TTL - 1
                with mock.patch.object(shim.time, "monotonic", side_effect=lambda: now[0]), \
                     mock.patch.object(shim.threading, "Thread", side_effect=make_reader), \
                     mock.patch.object(shim.OPENER, "open", side_effect=open_catalogue):
                    ids = cat.served("canary", "claude-opus-5-5")
                    self.assertEqual(ids, set())
                    self.assertEqual(shim.resolve_model("claude-opus-5-5", ids), "claude-opus-5-5")
                    self.assertEqual(cat.endpoints, {})
                    self.assertFalse(cat.lock.locked())

    def test_healthy_catalogue_ttl(self):
        for model, age, refresh in (("served", 60, False), ("missing", 60, False),
                                    ("missing", 61, True), ("served", 600, False),
                                    ("served", 601, True)):
            with self.subTest(model=model, age=age):
                shim = load_shim_module()
                cat = shim.Catalogue("http://upstream.invalid")
                with mock.patch.object(shim.time, "monotonic", return_value=1000.0), \
                     mock.patch.object(shim.OPENER, "open", side_effect=lambda *a, **kw:
                                       io.BytesIO(b'{"data":[{"id":"served"}]}')) as upstream:
                    self.assertEqual(cat.served("canary", model), {"served"})
                    shim.time.monotonic.return_value += age
                    self.assertEqual(cat.served("canary", model), {"served"})
                    self.assertEqual(upstream.call_count, 2 if refresh else 1)


class FakeProxy:
    """A listener that counts connections and answers 502, standing in for an ambient http_proxy."""

    def __init__(self):
        self.hits = 0
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(8)
        self.port = self.sock.getsockname()[1]
        threading.Thread(target=self._run, daemon=True).start()

    def _run(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            self.hits += 1
            conn.sendall(b"HTTP/1.1 502 Bad Gateway\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
            conn.close()

    def env(self):
        url = f"http://127.0.0.1:{self.port}"
        env = {k: v for k, v in os.environ.items()
               if k.lower() not in ("http_proxy", "https_proxy", "all_proxy", "no_proxy")}
        env.update(http_proxy=url, HTTP_PROXY=url)
        return env


class LoopbackNoProxyTest(unittest.TestCase):
    """Loopback hops never go through an ambient http_proxy (issue #83); the catalogue's upstream still does."""

    def setUp(self):
        self.proxy = FakeProxy()
        self.addCleanup(self.proxy.sock.close)

    def test_shim_forward_to_loopback_target_bypasses_proxy(self):
        upstream = serve(Upstream)
        self.addCleanup(upstream.shutdown)
        proc = subprocess.Popen(
            [sys.executable, str(SHIM), "0", f"http://127.0.0.1:{upstream.server_address[1]}",
             f"--parent-pid={os.getpid()}"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=self.proxy.env())
        self.addCleanup(reap_shim, proc)
        port = int(proc.stdout.readline().strip().split(":")[1].split(" ")[0])
        req = urllib.request.Request(f"http://127.0.0.1:{port}/json", data=b"{}", method="POST",
                                     headers={"content-type": "application/json"})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(req, timeout=HANG_TIMEOUT) as r:
            self.assertEqual(r.status, 200)
        self.assertEqual(self.proxy.hits, 0, "loopback forward went through the ambient proxy")

    def test_catalogue_upstream_still_uses_proxy(self):
        saved = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(saved)))
        os.environ.update(self.proxy.env())
        shim = load_shim_module()
        shim.Catalogue("http://upstream.invalid").served("k", "m")
        self.assertGreaterEqual(self.proxy.hits, 1, "catalogue fetch no longer honours the proxy")


class StallUpstream(BaseHTTPRequestHandler):
    """/before: never answers. /during: starts a stream, then goes silent."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("content-length") or 0))
        if self.path == "/v1/messages" and b"during" in self.headers.get("x-case", "").encode():
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.end_headers()
            self.wfile.write(b'event:message_start\ndata:{"type":"message_start","message":{"id":"m","type":"message","model":"x","content":[],"usage":{}}}\n\n')
            self.wfile.flush()
        time.sleep(20)


class StallDeadlineTest(unittest.TestCase):
    """A silent gateway ends the turn with an error Claude Code can read and retry,
    never a reply that never comes."""

    def post(self, case):
        srv = serve(StallUpstream)
        self.addCleanup(srv.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}",
                             env={"CCMORPH_STALL_TIMEOUT": "1"})
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/v1/messages", data=b'{"model":"m"}', method="POST",
            headers={"content-type": "application/json", "x-case": case})
        start = time.monotonic()
        try:
            with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
                status, data = r.status, r.read()
        except urllib.error.HTTPError as e:
            status, data = e.code, e.read()
        return status, data, time.monotonic() - start

    def test_silent_before_reply_is_a_504_error(self):
        status, data, elapsed = self.post("before")
        self.assertEqual(status, 504)
        self.assertIn("gateway sent nothing", json.loads(data)["error"]["message"])
        self.assertLess(elapsed, 10)

    def test_silent_mid_stream_ends_with_an_error_event(self):
        status, data, elapsed = self.post("during")
        self.assertEqual(status, 200)
        self.assertIn(b"message_start", data)
        self.assertIn(b'"type": "error"', data)
        self.assertLess(elapsed, 10)

    def test_every_stall_error_ends_a_stream_with_an_error_event(self):
        """The same mid-stream contract, driven in-process so it holds on every
        Python: on 3.9 socket.timeout is not TimeoutError, and the suite's own
        interpreter is 3.13+, where the two are the same class. Patching
        STALL_ERRORS with a stand-in that is neither fails here if the
        mid-stream arm stops naming the tuple."""
        shim = load_shim_module()

        class SocketTimeout(OSError):
            """What /usr/bin/python3 3.9 raises for a socket timeout."""

        shim.STALL_ERRORS = (TimeoutError, SocketTimeout)

        class SilentMidStream:
            """message_start, then silence: the upstream holds the connection
            open and never writes again."""

            lines = iter([b'event:message_start\n',
                          b'data:{"type":"message_start","message":{"id":"m"}}\n',
                          b"\n"])

            def readline(self):
                line = next(self.lines, None)
                if line is None:
                    raise SocketTimeout("timed out")
                return line

        handler = shim.IdShim.__new__(shim.IdShim)     # no socket setup
        handler.wfile = io.BytesIO()
        handler.close_connection = False
        handler._stream_sse(SilentMidStream())
        body = handler.wfile.getvalue()
        self.assertIn(b"message_start", body)
        self.assertIn(b'"type": "error"', body)
        self.assertIn(b"gateway sent nothing", body)
        self.assertTrue(handler.close_connection)
