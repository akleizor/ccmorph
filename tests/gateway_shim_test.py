#!/usr/bin/env python3
"""Tests for shim against a stub upstream.

The upstream stub reproduces the observed gateway misbehavior —
deterministic tool_use ids (Bash_0 on every response, SSE and plain JSON)
— and the tests assert the shim rewrites them unique while passing
everything else byte-identical.

The sidecar tests at the end pin the metadata-only usage rows: exact hashed
response ids, whitelisted buckets, the incomplete-then-complete stream
lifecycle, and that neither an unwritable state directory nor a dead client
changes a single relayed byte.
"""

import datetime
import gzip
import hashlib
import http.client
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

from support import HANG_TIMEOUT, SHIM, needs_shim

pytestmark = needs_shim

REPO = Path(__file__).resolve().parent.parent

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
        [*SHIM, "0", target, *flags, f"--parent-pid={os.getpid()}"],
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


class ShimParentDeathTest(unittest.TestCase):
    def test_shim_dies_with_the_process_that_launched_it(self):
        """--parent-pid must hold when the launcher is killed outright: six
        shims were found reparented to launchd, 12-17 h old, each left by a
        suite run that was SIGKILLed mid-test."""
        parent = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(0.5)"])
        self.addCleanup(parent.wait)
        proc = subprocess.Popen(
            [*SHIM, "0", f"http://127.0.0.1:{dead_port()}",
             f"--parent-pid={parent.pid}"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.addCleanup(reap_shim, proc)
        self.assertTrue(proc.stdout.readline().startswith("shim"))
        parent.wait(timeout=HANG_TIMEOUT)
        try:
            proc.wait(timeout=HANG_TIMEOUT)
        except subprocess.TimeoutExpired:
            self.fail("the shim outlived the process that launched it")


class ShimStartupTest(unittest.TestCase):
    def test_startup_prints_its_banner_with_the_bound_port(self):
        """The shim binds loopback only and resolves no name for it (a reverse
        lookup stalled startup for tens of seconds on macOS): the banner is the
        adapter's readiness signal and carries the port the OS chose."""
        proc, port = start_shim(self, f"http://127.0.0.1:{dead_port()}")
        self.assertGreater(port, 0)


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
            [*SHIM, "0", f"http://127.0.0.1:{cls.uport}",
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
    ctype = "application/json"

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.bodies[self.path]
        self.send_response(200)
        self.send_header("content-type", self.ctype)
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

    @staticmethod
    def sse(events):
        return "".join(
            f"event:{e['type']}\ndata:{json.dumps(e, separators=(',', ':'))}\n\n"
            for e in events).encode()

    @classmethod
    def reply(cls, thinking):
        """A streamed reply: the thinking fixture at index 0 (or no thinking
        block at all) and then a Read tool_use, like a real turn."""
        events = [{"type": "message_start",
                   "message": {"id": "msg_1", "type": "message",
                               "role": "assistant", "model": "unknown-test",
                               "content": [], "stop_reason": None,
                               "stop_sequence": None,
                               "usage": {"input_tokens": 500,
                                         "output_tokens": 0}}}]
        if thinking is not None:
            events.append({"type": "content_block_start", "index": 0,
                           "content_block": {"type": "thinking",
                                             "thinking": ""}})
            events.extend(thinking)
            events.append({"type": "content_block_stop", "index": 0})
        index = 1 if thinking is not None else 0
        events += [
            {"type": "content_block_start", "index": index,
             "content_block": {"type": "tool_use", "id": "toolu_1",
                               "name": "Read", "input": {}}},
            {"type": "content_block_delta", "index": index,
             "delta": {"type": "input_json_delta", "partial_json": "{}"}},
            {"type": "content_block_stop", "index": index},
            {"type": "message_delta",
             "delta": {"stop_reason": "tool_use", "stop_sequence": None},
             "usage": {"input_tokens": 500, "output_tokens": 20}},
            {"type": "message_stop"},
        ]
        return cls.sse(events)

    @staticmethod
    def frames(raw):
        """Every SSE frame, checked to be one event: line plus its data."""
        out = []
        for frame in raw.decode().split("\n\n"):
            if not frame:
                continue
            lines = frame.splitlines()
            assert len(lines) == 2, frame
            assert lines[0].startswith("event:"), frame
            assert lines[1].startswith("data:"), frame
            data = json.loads(lines[1][5:].lstrip())
            assert lines[0][6:] == data["type"], frame
            out.append(data)
        return out

    def post(self, bodies, flags=(), ctype="application/json"):
        srv = serve(type("Fixture", (FixtureUpstream,),
                         {"bodies": bodies, "ctype": ctype}))
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

    def test_unsigned_empty_streamed_thinking_is_dropped_and_later_blocks_reindexed(self):
        """#12 — Claude Code replays a streamed unsigned empty thinking block
        exactly as it does a JSON one, so the SSE path must drop it and shift
        later blocks down: the client addresses content by index."""
        for flags in ((), ("--preserve-tool-ids",)):
            with self.subTest(flags=flags):
                out = self.post({"/fixture": self.reply([])}, flags=flags,
                                ctype="text/event-stream")
                frames = self.frames(out)
                self.assertFalse(
                    [f for f in frames
                     if f.get("content_block", {}).get("type") == "thinking"])
                self.assertEqual({f["index"] for f in frames if "index" in f}, {0})
                self.assertEqual(
                    [f["type"] for f in frames],
                    ["message_start", "content_block_start",
                     "content_block_delta", "content_block_stop",
                     "message_delta", "message_stop"])
                if flags:
                    self.assertEqual(out, self.reply(None))

    def test_streamed_thinking_controls_pass_through_byte_identical(self):
        """#12 — signed omitted-display, healthy initially empty, an already
        signed start and a no-thinking stream are all relayed unchanged."""
        signature = {"type": "content_block_delta", "index": 0,
                     "delta": {"type": "signature_delta",
                               "signature": "sig-fixture-omitted-display"}}
        healthy = [{"type": "content_block_delta", "index": 0,
                    "delta": {"type": "thinking_delta", "thinking": "plan"}},
                   signature]
        pre_signed = self.sse([
            {"type": "message_start",
             "message": {"id": "msg_1", "type": "message", "role": "assistant",
                         "model": "unknown-test", "content": [],
                         "stop_reason": None, "stop_sequence": None,
                         "usage": {"input_tokens": 500, "output_tokens": 0}}},
            {"type": "content_block_start", "index": 0,
             "content_block": self.SIGNED},
            {"type": "content_block_stop", "index": 0},
            {"type": "message_stop"},
        ])
        cases = {"signed": self.reply([signature]),
                 "healthy": self.reply(healthy),
                 "pre-signed": pre_signed,
                 "no-thinking": self.reply(None)}
        for name, raw in cases.items():
            with self.subTest(case=name):
                self.assertEqual(
                    self.post({"/fixture": raw}, flags=["--preserve-tool-ids"],
                              ctype="text/event-stream"), raw)


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
    """--retry-transport end to end. The Go suite (shim/retry_test.go) pins the
    jitter bounds, the breaker clock and the stall rule with injected time; here
    the binary's own flags and CCMORPH_RETRY_BASE=0 (no waits) show what a
    provider and a client see."""

    def setUp(self):
        BlockedUpstream.failures_left = 0
        BlockedUpstream.bodies = []
        BlockedUpstream.sse = False
        self.upstream = serve(BlockedUpstream)
        self.addCleanup(self.upstream.shutdown)

    def shim(self, target=None, flags=("--retry-transport",)):
        _, port = start_shim(self, target or f"http://127.0.0.1:{self.upstream.server_address[1]}",
                             env={"CCMORPH_RETRY_BASE": "0"}, flags=flags)
        return port

    def post(self, port):
        return keepalive_post(self, port, "/v1/messages", body=REQUEST)

    def test_blocked_provider_is_retried_twice_on_the_same_bytes(self):
        BlockedUpstream.failures_left = 10 ** 6
        self.assertEqual(self.post(self.shim()).status, 503)
        self.assertEqual(BlockedUpstream.bodies, [REQUEST] * 3)

    def test_provider_that_recovers_answers_after_one_retry(self):
        BlockedUpstream.failures_left = 1
        resp = self.post(self.shim())
        self.assertEqual(resp.status, 200)
        self.assertEqual(json.loads(resp.read())["id"], "msg_ok")
        self.assertEqual(len(BlockedUpstream.bodies), 2)

    def test_an_sse_error_status_is_retried_too(self):
        BlockedUpstream.sse = True
        BlockedUpstream.failures_left = 10 ** 6
        self.assertEqual(self.post(self.shim()).status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 3)

    def test_refused_connection_is_retried_then_502(self):
        self.assertEqual(self.post(self.shim(f"http://127.0.0.1:{dead_port()}")).status, 502)

    def test_without_the_flag_nothing_is_retried(self):
        BlockedUpstream.failures_left = 10 ** 6
        self.assertEqual(self.post(self.shim(flags=())).status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 1)

    def test_three_failed_requests_open_the_breaker_and_it_fails_readably(self):
        BlockedUpstream.failures_left = 10 ** 6
        port = self.shim()
        for _ in range(3):
            self.assertEqual(self.post(port).status, 503)
        self.assertEqual(len(BlockedUpstream.bodies), 9)
        held = self.post(port)
        self.assertEqual(held.status, 503)
        message = json.loads(held.read())["error"]["message"]
        self.assertIn("ccmorph", message)
        self.assertIn("then tries one", message)
        # Held means held: nothing went upstream, and nothing went elsewhere.
        self.assertEqual(BlockedUpstream.bodies, [REQUEST] * 9)


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
    """The request-side flags, end to end. The shipped shim rewrites only a
    tool_reference its request does not declare; every other body and every
    identifier go upstream as the client sent them."""

    def echoed(self, tool_name):
        body = json.dumps({
            "model": "m", "tools": [{"name": "Bash"}],
            "messages": [{"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": [
                    {"type": "tool_reference", "tool_name": tool_name}]}]}],
        }).encode()
        srv = serve(EchoUpstream)
        self.addCleanup(srv.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{srv.server_address[1]}")
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/v1/messages", data=body,
            headers={"content-type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            return body, r.read()

    def test_request_body_is_byte_identical_when_nothing_dangles(self):
        body, got = self.echoed("Bash")
        self.assertEqual(got, body)

    def test_a_dangling_tool_reference_goes_upstream_as_text(self):
        _, got = self.echoed("Monitor")
        inner = json.loads(got)["messages"][0]["content"][0]["content"]
        self.assertEqual(inner, [{"type": "text", "text": "[tool_reference: Monitor]"}])

    def test_removed_repair_and_affinity_flags_are_rejected(self):
        # Issue 438: those rewrites left the core shim, so their flags are now
        # unknown; a caller still passing one fails loud at startup instead of
        # silently losing the rewrite.
        for flag in ("--repair-tool-references", "--affinity-header=X-Header"):
            with self.subTest(flag=flag):
                out = subprocess.run([*SHIM, "0", "http://x",
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
        proc = subprocess.run([*SHIM, "0", "http://x",
                               "--nope"], capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("usage", proc.stdout + proc.stderr)


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

    def test_recycled_response_id_through_the_shim_keeps_both_rows(self):
        port = self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})
        for _ in range(2):
            with self.post(port) as r:
                r.read()
        deadline = time.monotonic() + HANG_TIMEOUT
        while len(self.rows()) < 2 and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertEqual(len(self.rows()), 2)
        self.assertEqual(len({r["turn_id"] for r in self.rows().values()}), 2)
        self.assertIn(self.row_path("msg_capture_json").name, self.rows())

    def test_truncated_stream_leaves_an_incomplete_row(self):
        CaptureUpstream.mode = "truncated"
        with self.post(self.shim(env={"CCMORPH_MODEL_ROUTE": ROUTE_SPEC})) as r:
            r.read()
        row = self.wait_for(self.row_for("msg_capture_sse"))
        self.assertEqual(len(self.rows()), 1)
        self.assertEqual(row["response_id"], "msg_capture_sse")
        self.assertIs(row["complete"], False)
        self.assertEqual(row["usage"], {"input_tokens": 11, "cache_read_input_tokens": 2})


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
            subprocess.run([*SHIM, "outcome", "issue-42", result], env=env, check=True, timeout=HANG_TIMEOUT)
        bad = subprocess.run([*SHIM, "outcome", "/etc/x", "accepted"], env=env,
                             capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(bad.returncode, 0)
        bad = subprocess.run([*SHIM, "outcome", "t", "great"], env=env, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(bad.returncode, 0)
        # #210: "$" also matches before a trailing newline, so this used to append a row.
        bad = subprocess.run([*SHIM, "outcome", "x\n", "accepted"], env=env,
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
            [*SHIM, "0", f"http://127.0.0.1:{upstream.server_address[1]}",
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



class ThinkingResendTest(unittest.TestCase):
    """The stock shim resends once without thinking blocks after a 400."""

    BODY = {"model": "m", "messages": [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": [{"type": "thinking", "thinking": "t", "signature": "s"},
                                          {"type": "text", "text": "a"}]},
        {"role": "user", "content": "more"}]}

    def test_a_400_is_resent_once_without_thinking(self):
        bodies = []

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                bodies.append(self.rfile.read(int(self.headers["content-length"])))
                status = 400 if len(bodies) == 1 else 200
                out = b'{"type":"message","content":[]}'
                self.send_response(status)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

        server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        _, port = start_shim(self, f"http://127.0.0.1:{server.server_address[1]}")
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/v1/messages", data=json.dumps(self.BODY).encode(),
            headers={"content-type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            self.assertEqual(r.status, 200)
        self.assertEqual(len(bodies), 2)
        self.assertIn(b'"thinking"', bodies[0])
        self.assertNotIn(b"thinking", bodies[1])
        self.assertEqual(json.loads(bodies[1])["messages"][1]["content"],
                         [{"type": "text", "text": "a"}])


class TurnRouterTest(unittest.TestCase):
    """env.CCMORPH_TURN_ROUTER (#76): the shim asks a local command which model
    answers each user turn and keeps that answer for the turn's tool loop. The
    switch is announced before the answer it applies to; a router failure of any
    kind leaves the request as the client sent it."""

    TURN_SSE = (
        'event:message_start\n'
        'data:{"type":"message_start","message":{"id":"msg_turn","model":"m-served",'
        '"usage":{"input_tokens":3}}}\n\n'
        'event:content_block_start\n'
        'data:{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n'
        'event:content_block_delta\n'
        'data:{"type":"content_block_delta","index":0,'
        '"delta":{"type":"text_delta","text":"answer"}}\n\n'
        'event:content_block_stop\n'
        'data:{"type":"content_block_stop","index":0}\n\n'
        'event:message_delta\n'
        'data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},'
        '"usage":{"output_tokens":2}}\n\n'
        'event:message_stop\n'
        'data:{"type":"message_stop"}\n\n'
    )

    def setUp(self):
        CaptureUpstream.mode = "sse"
        CaptureUpstream.sse_body = self.TURN_SSE
        CaptureUpstream.on_body = None
        CaptureUpstream.on_headers = None
        self.upstream = serve(CaptureUpstream)
        self.addCleanup(self.upstream.shutdown)
        self.target = f"http://127.0.0.1:{self.upstream.server_address[1]}"
        self.seen = []
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, str(self.dir), True)
        self.asks = self.dir / "asks.jsonl"
        self.spec = json.dumps({"upstream": self.target, "models": ["m1", "m2"], "timeout": 1,
                                "key_sha256": hashlib.sha256(b"k").hexdigest()})

    def router(self, answer='{"model":"m2"}'):
        """A router script that appends its stdin to $1 and answers with `answer`."""
        path = self.dir / "router"
        path.write_text('#!/bin/sh\ncat >> "$1"\nprintf \'\\n\' >> "$1"\n'
                        f"printf '%s' '{answer}'\n")
        path.chmod(0o700)
        return path

    def shim(self, answer='{"model":"m2"}'):
        router = self.router(answer)
        _, port = start_shim(self, self.target, env={
            "CCMORPH_MODEL_ROUTE": self.spec,
            "CCMORPH_TURN_ROUTER": f"{router} {self.asks}",
        })
        return port

    def body(self, prompt, first="first"):
        return json.dumps({
            "model": "m1",
            "tools": [{"name": "Bash", "input_schema": {"type": "object"}}],
            "messages": [{"role": "user", "content": first},
                         {"role": "user", "content": prompt}],
        }).encode()

    def follow_up(self, prompt, first="first"):
        return json.dumps({
            "model": "m1",
            "tools": [{"name": "Bash", "input_schema": {"type": "object"}}],
            "messages": [
                {"role": "user", "content": first},
                {"role": "user", "content": prompt},
                {"role": "assistant", "content": [{"type": "tool_use", "id": "t", "name": "Bash",
                                                   "input": {}}]},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t",
                                              "content": "ok"}]},
            ],
        }).encode()

    def post(self, port, body):
        request = urllib.request.Request(
            f"http://127.0.0.1:{port}/v1/messages", data=body, method="POST",
            headers={"content-type": "application/json", "x-api-key": "k",
                     "x-claude-code-session-id": "sess"})
        with urllib.request.urlopen(request, timeout=HANG_TIMEOUT) as r:
            return r.read().decode()

    def sent_models(self):
        return [json.loads(b)["model"] for b in self.seen]

    def test_a_routed_turn_sticks_through_its_tool_loop_and_is_announced(self):
        CaptureUpstream.on_body = self.seen.append
        port = self.shim()
        out = self.post(port, self.body("second"))
        self.assertEqual(self.sent_models()[-1], "m2")
        # The notice names both models and comes before the answer it applies to.
        self.assertIn("answered by m2", out)
        self.assertIn("you asked for m1", out)
        self.assertLess(out.index("answered by m2"), out.index('"text":"answer"'))

        out = self.post(port, self.follow_up("second"))
        self.assertEqual(self.sent_models()[-1], "m2")
        self.assertNotIn("Turn router", out)

        # A request with no tools is a side call: never routed.
        self.post(port, json.dumps({"model": "m1", "messages": [
            {"role": "user", "content": "name this chat"}]}).encode())
        self.assertEqual(self.sent_models()[-1], "m1")

        # The next human turn asks again.
        self.post(port, self.body("third"))
        self.assertEqual(self.sent_models()[-1], "m2")
        lines = self.asks.read_text().splitlines()
        self.assertEqual(len(lines), 2, lines)
        self.assertEqual(json.loads(lines[0])["prompt"], "second")
        self.assertEqual(json.loads(lines[1])["prompt"], "third")

    def test_a_failing_router_leaves_the_request_as_sent(self):
        CaptureUpstream.on_body = self.seen.append
        port = self.shim(answer="not json")
        out = self.post(port, self.body("second"))
        self.assertEqual(self.sent_models()[-1], "m1")
        self.assertNotIn("Turn router", out)
