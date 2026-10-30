#!/usr/bin/env python3
"""Tests for ccmorph-adapter against a stub endpoint.

No network and no real translator: a stdlib HTTP server stands in for the gateway
and a shell script stands in for cliproxyapi, so the tests assert the adapter's own
behaviour rather than a vendor's.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

from support import CORE, FAKE_SERVER_NO_DNS, HANG_TIMEOUT, adapter_cmd

REPO = Path(__file__).resolve().parent.parent

# One model per dialect the adapter must distinguish, one stably dead, and one
# whose failure looks transient (an indeterminate verdict that must not be cached).
MODELS = ["m-anthropic", "m-responses", "m-noreason", "m-dead", "m-shaky",
          "m-reasoner", "m-always-reasoner"]


class Gateway(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    extra_models: list[str] = []
    # Set when a stalled probe's client goes away, freeing this single-threaded
    # server for the next request. `test_selected_launch_has_its_own_global_deadline`
    # reads it instead of racing the stall with a client timeout.
    stall_released = threading.Event()

    def log_message(self, format, *args):  # noqa: A002
        pass

    def _json(self, code, obj):
        payload = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        XFF_SEEN.append(self.headers.get("X-Forwarded-For"))
        ids = list(MODELS) + list(Gateway.extra_models)
        self._json(200, {"data": [{"id": i, "object": "model"} for i in ids]})

    def do_POST(self):
        XFF_SEEN.append(self.headers.get("X-Forwarded-For"))
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        # What the upstream really received — path, headers (lowercased), raw
        # body — so a test can assert on the wire the shim produced.
        UPSTREAM.append((self.path, {k.lower(): v for k, v in self.headers.items()}, raw))
        body = json.loads(raw or b"{}")
        model = body.get("model", "")
        PROBES.append((self.path.split("?")[0], model))

        def ok():
            self._json(200, {"type": "message", "role": "assistant",
                             "model": model, "content": [], "usage": {}})

        # A model whose probe never returns inside a reasonable time, so the
        # selected-model launch deadline has something real to fire against.
        if model == "m-stall":
            # Send no response, but stop waiting as soon as the deadline kills
            # the client. A fixed sleep blocks this shared server's next test.
            self.connection.settimeout(30)
            try:
                self.connection.recv(1)
            except TimeoutError:
                pass
            Gateway.stall_released.set()
            return

        # An id carrying YAML metacharacters, answering Anthropic cleanly: without
        # id validation it would be rendered into the config and reshape it.
        if model.startswith("m-evil"):
            ok()
            return

        # A model whose every probe returns 503 looks transient, not dead: its
        # verdict is indeterminate and must not be cached, but it must not poison
        # the verdicts of the models that answered cleanly either.
        if model == "m-shaky":
            # A REAL 5xx body, with no status number anywhere in the text: what a
            # gateway actually sends. The adapter must call this indeterminate from
            # the numeric status alone. An earlier version matched "^5\d\d" against
            # the note, which no real 5xx body starts with, so genuinely transient
            # models were cached as permanently unusable.
            self._json(503, {"error_message": "no healthy upstream"})
            return

        if self.path.startswith("/v1/messages"):
            # The cap probe sends a large max_tokens; answer it the way a capped
            # model does so the adapter can read the number back.
            if body.get("max_tokens", 0) > 20000 and model == "m-anthropic":
                self._json(400, {"error": {"message":
                    "max_tokens is too large: 32000. This model supports at "
                    "most 8192 completion tokens."}})
                return
            # m-anthropic recycles tool_use ids (observed on a real gateway):
            # every tools-bearing probe gets the same deterministic id.
            if model == "m-anthropic" and body.get("tools"):
                self._json(200, {"type": "message", "role": "assistant",
                                 "model": model,
                                 "content": [{"type": "tool_use", "id": "Bash_0",
                                              "name": "probe_tool",
                                              "input": {"x": "1"}}],
                                 "usage": {}})
                return
            # m-reasoner thinks when omitted but accepts an explicit disabled
            # control. m-always-reasoner rejects disabled, so the adapter must
            # leave its native always-thinking behavior alone.
            if model in ("m-reasoner", "m-always-reasoner"):
                if (model == "m-always-reasoner"
                        and body.get("thinking", {}).get("type") == "disabled"):
                    self._json(400, {"error": {"message":
                        "This model always engages in thinking and cannot be disabled"}})
                    return
                self._json(200, {"type": "message", "role": "assistant",
                                 "model": model,
                                 "content": [{"type": "thinking",
                                              "thinking": "hmm"},
                                             {"type": "text", "text": "hi"}],
                                 "usage": {}})
                return
            ok() if model == "m-anthropic" else self._json(
                400, {"error": {"message": "MESSAGES_API not supported"}})
            return
        if self.path.startswith("/v1/responses"):
            ok() if model == "m-responses" else self._json(
                400, {"error": {"message": "no"}})
            return
        if self.path.startswith("/v1/chat/completions"):
            # m-noreason works only when reasoning_effort is absent.
            if model == "m-noreason" and "reasoning_effort" not in body:
                ok()
            else:
                self._json(400, {"error": {"message":
                    "Unrecognized request argument supplied: reasoning_effort"}})
            return
        self._json(404, {"error": "no route"})


PROBES: list[tuple[str, str]] = []
XFF_SEEN: list[str | None] = []
UPSTREAM: list[tuple[str, dict, bytes]] = []


def free_port() -> int:
    """A loopback port that is free right now: bind, read it, close."""
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class AdapterTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), Gateway)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def setUp(self):
        PROBES.clear()
        XFF_SEEN.clear()
        UPSTREAM.clear()
        Gateway.extra_models = []
        self.home = Path(tempfile.mkdtemp())
        # A fake ccmorph whose _env reports the stub endpoint, so the adapter's
        # "credential comes from the profile" path is exercised for real.
        cc = self.home / ".config/ccmorph"
        (cc / "lib").mkdir(parents=True)
        (cc / "lib/ccmorph.sh").write_text(
            "ccmorph() {\n"
            '  [[ "$1" == _env ]] || return 1\n'
            f'  print "ANTHROPIC_BASE_URL={self.base}"\n'
            '  print "ANTHROPIC_API_KEY=stub-key"\n'
            "}\n")
        self.profiles = self.home / "profiles"
        self.profiles.mkdir()
        self.path_bin = self.home / "bin"
        self.path_bin.mkdir()
        self.adapter_env = {"PATH": f"{self.path_bin}:{os.environ.get('PATH', '')}"}

    def _install_fake_translator(self, source=None):
        target = self.path_bin / "cliproxyapi"
        if source is None:
            source = self.home / "stub-translator"
        if Path(source).exists():
            shutil.copy2(source, target)
        target.chmod(0o755)
        return target

    def cache_file(self):
        """The shared endpoint-keyed classification cache the adapter writes —
        keyed by base_url hash, not per-profile, so profiles on one gateway share
        one classification. Mirrors the keying in ccmorph-adapter main()."""
        import hashlib
        key = hashlib.sha256(self.base.encode()).hexdigest()[:16]
        return self.profiles / ".adapter-cache" / f"{key}.json"

    def write_ccmorph(self, key: str) -> None:
        """Repoint the stub ccmorph at a different credential."""
        (self.home / ".config/ccmorph/lib/ccmorph.sh").write_text(
            "ccmorph() {\n"
            '  [[ "$1" == _env ]] || return 1\n'
            f'  print "ANTHROPIC_BASE_URL={self.base}"\n'
            f"  print 'ANTHROPIC_API_KEY={key}'\n"
            "}\n")

    def _launch_env(self, **extra) -> dict:
        """The launch context every adapter invocation in these tests shares:
        a fake HOME, the stub ccmorph, the test state root, and the PATH
        holding any stub translator. Extras win over the shared keys, so a
        caller can still override any of them."""
        env = dict(os.environ, **self.adapter_env)
        env.update(HOME=str(self.home),
                   CCMORPH_LIB_DIR=str(self.home / ".config/ccmorph/lib"),
                   CCMORPH_STATE_DIR=str(self.home))
        env.update(extra)
        return env

    def run_adapter(self, *args, **kw):
        constants = kw.pop("_constants", {})
        shim = kw.pop("_shim", None)
        cmd = adapter_cmd()
        env = self._launch_env()
        if constants or shim:
            fixture = self.home / "adapter-fixture"
            fixture.mkdir(exist_ok=True)
            # The adapter takes its two timings from the environment.
            binary = fixture / "ccmorph-core"
            binary.unlink(missing_ok=True)  # a new inode: macOS kills a signed binary rewritten in place
            shutil.copy2(CORE, binary)
            env.update({f"CCMORPH_ADAPTER_{name}": str(value) for name, value in constants.items()})
            cmd = adapter_cmd(binary)
        env.update(kw.pop("env", {}))
        return subprocess.run([*cmd, "p", *args],
                              capture_output=True, text=True, env=env,
                              timeout=HANG_TIMEOUT, **kw)

    def classification(self):
        return {r["model"]: r["dialect"] for r in json.loads(
            self.cache_file().read_text())["rows"]}

    def token_file(self, text="t" * 40, mode=0o600) -> Path:
        """A fresh token file under the test home; one path per call, so the
        refusal cases can hold a symlink, a directory and a missing path at
        the same time."""
        self._token_n = getattr(self, "_token_n", 0) + 1
        f = self.home / f"inbound-token-{self._token_n}"
        f.write_text(text)
        f.chmod(mode)
        return f

    def seeded_cache(self, id_recycle: bool) -> Path:
        """A one-model classification cache. id_recycle decides the serve path
        a selected-model launch takes: True forces the translator, False
        leaves the native fast path."""
        cache = self.cache_file()
        cache.parent.mkdir(parents=True, exist_ok=True)
        cache.write_text(json.dumps({
            "endpoint": self.base, "anthropic_path": "/v1/messages",
            "thinking_probe": 2,
            "rows": [{"model": "m-anthropic", "dialect": "anthropic",
                      "note": "", "status": 200, "cap": None,
                      "nothink": False, "id_recycle": id_recycle}]}))
        return cache

    def install_serving_stub_translator(self):
        """A stub translator that really serves /v1/models, so both health
        checks pass and the adapter reaches the shim."""
        stub = self.home / "stub-translator"
        stub.write_text(
            "#!/usr/bin/env python3\n"
            "import sys, json, re\n"
            "from http.server import BaseHTTPRequestHandler, HTTPServer\n" + FAKE_SERVER_NO_DNS +
            "conf = open(sys.argv[2]).read()\n"
            "port = int(re.search(r'^port: (\\d+)', conf, re.M).group(1))\n"
            "class H(BaseHTTPRequestHandler):\n"
            "    protocol_version = 'HTTP/1.1'\n"
            "    def log_message(self, *a): pass\n"
            "    def do_GET(self):\n"
            "        d = json.dumps({'data': []}).encode()\n"
            "        self.send_response(200)\n"
            "        self.send_header('content-type', 'application/json')\n"
            "        self.send_header('content-length', str(len(d)))\n"
            "        self.end_headers()\n"
            "        self.wfile.write(d)\n"
            "HTTPServer(('127.0.0.1', port), H).serve_forever()\n")
        stub.chmod(0o755)
        self._install_fake_translator(stub)

    def start_endpoint(self, *args) -> tuple[subprocess.Popen, str]:
        """Run the adapter as a real serving process; return it with its first
        stdout line (the PORT= contract). Cleanup terminates it and drains both
        pipes — communicate(), never a bare wait(), so a full pipe cannot hang
        the test. XDG_STATE_HOME keeps shim usage rows inside the test home."""
        proc = subprocess.Popen(
            [*adapter_cmd(), "p", *args],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            env=self._launch_env(XDG_STATE_HOME=str(self.home / ".local/state")))

        def stop():
            if proc.poll() is None:
                proc.terminate()
            proc.communicate(timeout=HANG_TIMEOUT)

        self.addCleanup(stop)
        return proc, proc.stdout.readline().strip()

    def test_each_dialect_is_identified_by_probing(self):
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        got = self.classification()
        self.assertEqual(got["m-anthropic"], "anthropic")
        self.assertEqual(got["m-responses"], "responses")
        self.assertEqual(got["m-noreason"], "chat-noreason")
        self.assertEqual(got["m-dead"], "unusable")

    def test_config_places_each_model_in_its_dialect_block(self):
        out = self.run_adapter("--print-config")
        cfg = out.stdout
        self.assertIn("claude-api-key:", cfg)
        self.assertIn("codex-api-key:", cfg)
        self.assertIn("openai-compatibility:", cfg)
        # A dead model must never be offered.
        self.assertNotIn("m-dead", cfg)
        # reasoning_effort is stripped for the model that rejects it, and the
        # probed max_tokens cap is clamped rather than the model being dropped.
        self.assertIn("reasoning_effort", cfg)
        self.assertIn('"max_tokens": 8192', cfg)
        # The credential never appears in rendered output.
        self.assertNotIn("stub-key", cfg)

    def test_no_x_forwarded_for_header_is_ever_added(self):
        # The adapter forwards nothing of its own: no header on probes, no headers
        # block in the rendered config. There is no profile-driven path left that
        # can add a spoofable X-Forwarded-For.
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertTrue(XFF_SEEN, "no upstream probe was made")
        self.assertTrue(all(x is None for x in XFF_SEEN),
                        f"a probe carried an unexpected X-Forwarded-For: {XFF_SEEN}")
        self.assertNotIn("X-Forwarded-For", out.stdout)

    def test_a_key_needing_escaping_is_still_fully_redacted(self):
        # The key reaches the config as a YAML scalar, so one containing a quote or
        # backslash is on disk in escaped form. --print-config must redact that
        # spelling too, or the credential prints in full.
        self.write_ccmorph('sk-a"b\\c-live')
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        for spelling in ('sk-a"b\\c-live', 'sk-a\\"b\\\\c-live'):
            self.assertNotIn(spelling, out.stdout,
                             "the credential printed unredacted")
        self.assertIn("<redacted>", out.stdout)

    def test_print_config_redacts_upstream_proxy_credentials(self):
        proxy = "http://user:s3cretpw@127.0.0.1:8118/p?token=qq"
        stub = self.home / ".config/ccmorph/lib/ccmorph.sh"
        stub.write_text(stub.read_text().rstrip().rstrip("}")
                        + f"  print 'CCMORPH_UPSTREAM_PROXY={proxy}'\n}}\n")
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        for leak in ("s3cretpw", "user:", "token=qq"):
            self.assertNotIn(leak, out.stdout)
        self.assertIn("proxy-url:", out.stdout)
        self.assertIn("127.0.0.1:8118/p", out.stdout)

    def test_config_enables_request_retry(self):
        # Transient upstream drops (unexpected EOF, 5xx) are re-sent by the proxy
        # before Claude Code sees them, cutting visible error noise.
        cfg = self.run_adapter("--print-config").stdout
        self.assertIn("request-retry:", cfg)

    def test_reason_by_default_model_gets_thinking_disabled(self):
        # kimi-k3 pathology (2026-08-16): the gateway reasons even when
        # thinking is absent, so CC's effort knob cannot bound it — 36-minute
        # thinking streams and thinking-only empty turns. The adapter must
        # force "thinking": {"type": "disabled"} for such models; models that
        # only think when asked keep the knob.
        cfg = self.run_adapter("--print-config").stdout
        import re
        m = re.search(r'- name: "m-reasoner"\n      params:\n((?:        .+\n)+)',
                      cfg)
        self.assertIsNotNone(m, "m-reasoner must carry a payload override")
        self.assertIn('"thinking": {"type": "disabled"}', m.group(1))
        m2 = re.search(r'- name: "m-anthropic"\n      params:\n((?:        .+\n)+)',
                       cfg)
        self.assertNotIn('"thinking"', m2.group(1) if m2 else "")
        m3 = re.search(r'- name: "m-always-reasoner"\n      params:\n((?:        .+\n)+)',
                       cfg)
        self.assertNotIn('"thinking"', m3.group(1) if m3 else "",
                         "a model that rejects disabled must retain native thinking")

    def test_legacy_thinking_verdict_is_reprobed_once(self):
        self.run_adapter("--print-config")
        cache = self.cache_file()
        blob = json.loads(cache.read_text())
        blob.pop("thinking_probe", None)
        for row in blob["rows"]:
            if row["model"] == "m-always-reasoner":
                row["nothink"] = True
        cache.write_text(json.dumps(blob))
        PROBES.clear()

        cfg = self.run_adapter("--print-config").stdout
        import re
        m = re.search(r'- name: "m-always-reasoner"\n      params:\n((?:        .+\n)+)',
                      cfg)
        self.assertNotIn('"thinking"', m.group(1) if m else "")
        probed = {model for _, model in PROBES}
        self.assertEqual(probed,
                         {"m-reasoner", "m-always-reasoner", "m-shaky"},
                         "only legacy force-disabled and unsettled models should re-probe")
        self.assertEqual(json.loads(cache.read_text())["thinking_probe"], 2)

    def test_thinking_override_never_replaces_the_system_prompt(self):
        # cliproxyapi's payload override REPLACES the system field rather than
        # appending to it, so a "system" param here deletes Claude Code's entire
        # system prompt — tools, environment, policy — for exactly the models most
        # likely to need it. Reasoning is bounded through the thinking field alone.
        cfg = self.run_adapter("--print-config").stdout
        self.assertIn('"thinking": {"type": "disabled"}', cfg)
        self.assertNotIn("Think briefly", cfg)
        # Assert on the params bodies, not the whole document: a comment may name
        # the field precisely to warn against it.
        params = [l for l in cfg.splitlines()
                  if l.startswith('        "')]
        self.assertTrue(params, "no override params rendered")
        for line in params:
            self.assertNotIn('"system"', line,
                             f"the override must set no system field: {line!r}")

    def test_thinking_override_is_only_rendered_for_anthropic_dialect(self):
        # nothink is an Anthropic-dialect workaround. A row of any other dialect
        # carrying the flag (an older cache, a hand edit) must not produce an
        # override: the translator would turn it into a parameter the upstream
        # rejects, and a merely-unbounded model would become a broken one.
        self.run_adapter("--print-config")
        cache = self.cache_file()
        blob = json.loads(cache.read_text())
        for row in blob["rows"]:
            if row["model"] in ("m-responses", "m-noreason"):
                row["nothink"] = True
        cache.write_text(json.dumps(blob))

        cfg = self.run_adapter("--print-config").stdout
        import re
        for model in ("m-responses", "m-noreason"):
            m = re.search(rf'- name: "{model}"\n      params:\n((?:        .+\n)+)',
                          cfg)
            self.assertNotIn('"thinking"', m.group(1) if m else "",
                             f"{model} is not Anthropic dialect and must not "
                             f"carry a thinking override")

    def test_model_ids_are_escaped_and_unsafe_ones_skipped(self):
        # Ids reach the config as YAML scalars. An id carrying a quote or a newline
        # would reshape the document — reassigning port, api-keys, or base-url — so
        # ids outside the safe set are dropped, and everything rendered is quoted.
        Gateway.extra_models = ['m-evil"\nport: 1\n', "m-evil ok:", "m-evil.safe-1"]
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        cfg = out.stdout

        # The safe id is offered; the two metacharacter-bearing ones are not.
        self.assertIn('"m-evil.safe-1"', cfg)
        self.assertNotIn("m-evil ok:", cfg)
        self.assertNotIn("port: 1", cfg)
        self.assertIn("unsafe characters", out.stderr)

        # Nothing injected a second top-level key: the document still has exactly
        # one port line, and it is the one the adapter chose.
        self.assertEqual(len([l for l in cfg.splitlines()
                              if l.startswith("port:")]), 1)
        # And every rendered model name is a quoted scalar.
        import re
        for line in cfg.splitlines():
            m = re.match(r"\s+(?:- )?(?:name|alias): (.+)$", line)
            if m:
                self.assertTrue(m.group(1).startswith('"')
                                and m.group(1).endswith('"'),
                                f"unquoted scalar: {line!r}")

    def test_real_5xx_body_is_indeterminate_by_status_not_by_prose(self):
        # m-shaky answers 503 with a body that contains no status number at all —
        # what a gateway really sends. Classification must key on the numeric
        # status: the old note regex ("^5\d\d") never matched such a body, so a
        # transiently-down model was cached as permanently unusable and never
        # re-probed.
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        rows = json.loads(
            self.cache_file().read_text())["rows"]
        by_model = {r["model"]: r for r in rows}
        self.assertNotIn("m-shaky", by_model,
                         "a real 5xx body must be read as indeterminate and not "
                         "cached, whatever its prose says")
        # A 4xx refusal is a settled verdict and IS cached, so the two are really
        # being told apart rather than everything being called shaky.
        self.assertEqual(by_model["m-dead"]["dialect"], "unusable")
        self.assertEqual(by_model["m-dead"]["status"], 400)

    def test_cached_rows_from_an_older_schema_are_dropped_not_read(self):
        # A row written before a field existed used to reach sort/render and raise
        # KeyError mid-launch. Such rows are dropped at load and re-probed instead.
        self.run_adapter("--print-config")
        cache = self.cache_file()
        blob = json.loads(cache.read_text())
        for row in blob["rows"]:
            if row["model"] == "m-anthropic":
                del row["cap"]
            if row["model"] == "m-responses":
                del row["dialect"]
        cache.write_text(json.dumps(blob))

        PROBES.clear()
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        reprobed = {m for _p, m in PROBES}
        self.assertIn("m-anthropic", reprobed)
        self.assertIn("m-responses", reprobed)
        # Intact rows still come from cache — the drop is per row, not per file.
        self.assertNotIn("m-noreason", reprobed)
        # And the re-probe restored a complete row.
        rows = json.loads(cache.read_text())["rows"]
        by_model = {r["model"]: r for r in rows}
        self.assertEqual(by_model["m-anthropic"]["cap"], 8192)
        self.assertEqual(by_model["m-responses"]["dialect"], "responses")

    def test_an_unwritable_cache_dir_degrades_instead_of_killing_the_launch(self):
        # The lock is an optimisation, not a precondition. Opening it is a write,
        # and a write can be refused for reasons unrelated to contention: a
        # sandboxed launch context (observed as "[Errno 1] Operation not permitted"
        # on a path an ordinary shell can write), a read-only volume, a
        # full disk. That used to raise out of classify() and kill the adapter one
        # second into a launch — "adapter for 'gateway' did not become ready" —
        # even though the cache beside the lock held everything needed to serve.
        self.run_adapter("--print-config")
        cache = self.cache_file()
        self.assertTrue(cache.exists(), "first run must populate the cache")
        expected = len(json.loads(cache.read_text())["rows"])

        # Remove the sidecar before locking the directory: an EXISTING lock file
        # can still be opened for writing in a non-writable directory, so leaving
        # it in place would not exercise the refusal at all. The real sandbox failure
        # was on first contact in that context, with no lock yet.
        lock = cache.with_name(cache.name + ".lock")
        lock.unlink(missing_ok=True)
        mode = cache.parent.stat().st_mode
        os.chmod(cache.parent, 0o500)          # readable, not writable
        try:
            PROBES.clear()
            out = self.run_adapter("--print-config")
            self.assertEqual(out.returncode, 0,
                             f"an unwritable cache dir must not fail the launch:\n{out.stderr}")
            self.assertIn("lock unavailable", out.stderr,
                          "the degradation must be stated, not silent")
            # Served from the cache, without re-probing: PROBES is the real oracle
            # here. (Counting "- name:" in the config would be wrong — it appears in
            # the passthrough allowlist AND in each per-model params/addon block.)
            reprobed = {m for _p, m in PROBES}
            self.assertEqual(reprobed, {"m-shaky"},
                             "settled cached rows must not be re-probed just "
                             "because the lock could not be taken (m-shaky is "
                             "never cached by design — its verdict is "
                             "indeterminate — so it alone re-probes)")
            # And the config it rendered is a real one, naming a cached model.
            self.assertIn("m-anthropic", out.stdout,
                          "the rendered config must still carry the cached models")
        finally:
            os.chmod(cache.parent, mode)

    def test_id_recycling_upstream_is_classified_for_the_shim_addon(self):
        # Two probes, same deterministic id both times = recycling upstream;
        # the shim add-on is interposed only for those (m-reasoner answers
        # without a tool call at all, so it is not flagged).
        self.run_adapter("--print-config")
        rows = json.loads(
            self.cache_file().read_text())["rows"]
        by_model = {r["model"]: r for r in rows}
        self.assertTrue(by_model["m-anthropic"]["id_recycle"])
        self.assertFalse(by_model["m-reasoner"]["id_recycle"])

    def test_runtime_state_uses_temp_but_classification_cache_persists(self):
        runtime_root = self.home / "runtime"
        runtime_root.mkdir()
        out = self.run_adapter("--print-config", env={"TMPDIR": str(runtime_root)})
        self.assertEqual(out.returncode, 0, out.stderr)

        # Classification persists in the shared endpoint-keyed cache; runtime
        # state (config.yaml, auth) lives only under TMPDIR and never beside it.
        persistent = self.cache_file()
        self.assertTrue(persistent.exists())
        self.assertFalse((persistent.parent / "config.yaml").exists())
        self.assertFalse((persistent.parent / "auth").exists())
        self.assertIn(str(runtime_root), out.stdout)
        self.assertNotIn(str(persistent), out.stdout)

    def test_second_start_probes_only_models_it_has_not_seen(self):
        self.run_adapter("--print-config")
        first = len(PROBES)
        self.assertGreater(first, 0)

        PROBES.clear()
        Gateway.extra_models = ["m-new"]
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        probed = {m for _p, m in PROBES}
        self.assertIn("m-new", probed)
        self.assertNotIn("m-anthropic", probed,
                         "a known model must not be re-probed when a new one appears")

    def test_indeterminate_verdict_does_not_block_caching_the_rest(self):
        # m-shaky returns 503 on every probe: an indeterminate verdict. The old
        # behaviour refused to cache the whole sweep, so all 5 models were re-probed
        # every launch. The settled four must be cached; only the shaky one is not.
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        cache = self.cache_file()
        self.assertTrue(cache.exists(),
                        "a lone indeterminate model must not suppress the cache")
        cached = {r["model"] for r in json.loads(cache.read_text())["rows"]}
        for settled in ("m-anthropic", "m-responses", "m-noreason", "m-dead"):
            self.assertIn(settled, cached,
                          f"{settled} settled and must be cached")
        self.assertNotIn("m-shaky", cached,
                         "an indeterminate verdict must not be cached")

    def test_next_launch_reprobes_only_the_shaky_model(self):
        self.run_adapter("--print-config")
        PROBES.clear()
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        reprobed = {m for _p, m in PROBES}
        # The four settled models come from cache; only the uncached shaky one is
        # probed again. This is what turns a 42-model re-sweep into a 1-model one.
        self.assertIn("m-shaky", reprobed)
        for settled in ("m-anthropic", "m-responses", "m-noreason", "m-dead"):
            self.assertNotIn(settled, reprobed,
                             f"{settled} was cached and must not be re-probed")

    def test_selected_model_already_cached_bypasses_inventory_and_probing(self):
        # Live regression: a launch waited out a full sweep of models it never
        # selected because ONE other model on the endpoint was new. Once
        # m-anthropic is cached, selecting it must do no inventory GET and no
        # probes at all -- the cached row is returned before either runs.
        self.run_adapter("--print-config")  # first contact: populates the cache
        PROBES.clear()
        XFF_SEEN.clear()
        out = self.run_adapter("--print-config", "--model", "m-anthropic")
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual(PROBES, [], "a cached selected model must not be probed")
        self.assertEqual(XFF_SEEN, [], "a cached selected model must not hit "
                         "the gateway's inventory endpoint either")
        # And every other previously-cached row is still on disk and selectable
        # (m-dead is correctly absent from the rendered config -- it is
        # unusable -- so the cache, not stdout, is the oracle here).
        cached = {r["model"] for r in
                  json.loads(self.cache_file().read_text())["rows"]}
        for m in ("m-responses", "m-noreason", "m-dead"):
            self.assertIn(m, cached)
        self.assertIn("m-responses", out.stdout)

    def test_selected_model_absent_from_cache_probes_only_itself(self):
        # No prior run: the cache is empty, so m-anthropic is not in it. The
        # adapter must classify only m-anthropic -- no /v1/models inventory call,
        # no probing of the other six stub models.
        out = self.run_adapter("--print-config", "--model", "m-anthropic")
        self.assertEqual(out.returncode, 0, out.stderr)
        probed = {m for _p, m in PROBES}
        self.assertEqual(probed, {"m-anthropic"},
                         f"only the selected model may be probed, got {probed}")
        self.assertEqual(len(XFF_SEEN), len(PROBES),
                         "no GET /v1/models (inventory) call must have been made")
        self.assertIn("m-anthropic", out.stdout)

    def test_selected_model_probe_preserves_other_cached_rows(self):
        # Cache m-responses and m-noreason via a full sweep, then start a second
        # run selecting a model NOT yet cached. Its own probe must not evict or
        # ignore the rows already on disk. "m-evil-brand-new" answers cleanly
        # (matches the stub's m-evil* passthrough) so this exercises preservation
        # on a USABLE new model, not the separate unusable-requested-model path.
        self.run_adapter("--print-config")
        PROBES.clear()
        Gateway.extra_models = ["m-evil-brand-new"]
        out = self.run_adapter("--print-config", "--model", "m-evil-brand-new")
        self.assertEqual(out.returncode, 0, out.stderr)
        probed = {m for _p, m in PROBES}
        self.assertEqual(probed, {"m-evil-brand-new"})
        rows = json.loads(self.cache_file().read_text())["rows"]
        cached = {r["model"] for r in rows}
        self.assertIn("m-evil-brand-new", cached)
        for kept in ("m-anthropic", "m-responses", "m-noreason", "m-dead"):
            self.assertIn(kept, cached, f"{kept} must survive a selected-model probe")

    def test_selected_model_cached_unusable_raises_a_precise_failure(self):
        # m-dead is cached as unusable dialect. Selecting it must fail loudly and
        # namedly, not silently launch against a config lacking it or, worse,
        # fall through to whichever other models happen to be cached.
        self.run_adapter("--print-config")
        out = self.run_adapter("--print-config", "--model", "m-dead")
        self.assertNotEqual(out.returncode, 0,
                            "an unusable requested model must fail the launch")
        self.assertIn("m-dead", out.stderr)
        self.assertIn("unusable", out.stderr)

    def test_selected_model_that_probes_unusable_raises_not_silently_starts(self):
        # m-dead is not yet cached at all: its own probe classifies it unusable,
        # and that must still be a named failure, not a launch on other models.
        out = self.run_adapter("--print-config", "--model", "m-dead")
        self.assertNotEqual(out.returncode, 0)
        self.assertIn("m-dead", out.stderr)

    def test_model_flag_strips_only_the_1m_suffix(self):
        # ccmorph's own [1m] context-window marker is a shell-layer annotation,
        # not a real id on the gateway -- classify() must see the bare id.
        out = self.run_adapter("--print-config", "--model", "m-anthropic[1m]")
        self.assertEqual(out.returncode, 0, out.stderr)
        probed = {m for _p, m in PROBES}
        self.assertEqual(probed, {"m-anthropic"})

    def test_no_model_flag_keeps_the_full_diagnostic_sweep(self):
        # Omitting --model is the existing warmup/diagnostic path: every model
        # on the endpoint is still probed, unchanged from before this feature.
        out = self.run_adapter("--print-config")
        self.assertEqual(out.returncode, 0, out.stderr)
        probed = {m for _p, m in PROBES}
        for m in MODELS:
            self.assertIn(m, probed, f"{m} must still be probed with no --model")

    def test_selected_launch_has_its_own_global_deadline(self):
        # A cold translator's probe can stall well past any single request's own
        # PROBE_TIMEOUT (60s) because the CHAIN -- lock wait + several probes --
        # has no cap of its own otherwise. The selected-model deadline bounds the
        # whole classification, so a stalled probe fails the
        # launch in seconds, not minutes.
        Gateway.extra_models = ["m-stall"]
        Gateway.stall_released.clear()
        out = self.run_adapter("--print-config", "--model", "m-stall",
                               _constants={"SELECT_DEADLINE": 2})
        self.assertNotEqual(out.returncode, 0,
                            "a stalled selected-model probe must not hang past "
                            "its own deadline")
        self.assertIn("m-stall", out.stderr)
        # Timing out the client must not leave the shared, single-threaded test
        # gateway holding the stall for the next test's inventory or
        # selected-model probe: the probe's own deadline drops the connection,
        # which is what frees the server. A client timeout would only race the
        # stall's 30 s backstop, so ask the stall itself.
        import urllib.request
        self.assertTrue(Gateway.stall_released.wait(HANG_TIMEOUT),
                        "the stalled probe never released the shared gateway")
        with urllib.request.urlopen(f"{self.base}/v1/models", timeout=HANG_TIMEOUT) as response:
            self.assertEqual(response.status, 200)

    def test_parent_death_stops_the_translator_and_removes_the_config(self):
        stub = self.home / "stub-translator"
        stub.write_text("#!/bin/sh\nexec sleep 300\n")
        stub.chmod(0o755)

        self._install_fake_translator(stub)

        # A parent that exits immediately: the adapter must notice and clean up.
        parent = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(0.5)"])
        proc = subprocess.Popen(
            [*adapter_cmd(), "p", "--parent-pid", str(parent.pid)],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            env=dict(os.environ, HOME=str(self.home),
                     CCMORPH_LIB_DIR=str(self.home / ".config/ccmorph/lib"),
                     CCMORPH_STATE_DIR=str(self.home),
                     PATH=f"{self.path_bin}:{os.environ.get('PATH', '')}"))
        parent.wait(timeout=HANG_TIMEOUT)
        try:
            proc.wait(timeout=HANG_TIMEOUT)
        except subprocess.TimeoutExpired:
            proc.kill()
            self.fail("adapter did not exit after its parent died")
        self.assertFalse((self.profiles / "p/adapter/config.yaml").exists(),
                         "rendered config holding the credential was left behind")

    @staticmethod
    def _pid_alive(pid):
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        return True

    def _tethers(self, adapter_pid):
        """The `ccmorph-core tether <adapter_pid> ...` processes running now."""
        out = subprocess.run(["ps", "-A", "-o", "pid=,command="],
                             capture_output=True, text=True, check=True,
                             timeout=HANG_TIMEOUT).stdout
        pattern = re.compile(rf"\stether {adapter_pid}(\s|$)")
        return {int(line.split(None, 1)[0]) for line in out.splitlines()
                if pattern.search(line)}

    def test_sigkilled_adapter_does_not_leak_its_children(self):
        # SIGKILL skips every handler and atexit in the adapter, which is how 40
        # translator+shim pairs were found reparented to launchd on 2026-10-02.
        # One tether per adapter now watches every child it starts (#29), so a
        # profile's own hooks build is covered by the same guarantee and the
        # whole session costs one tether process, not one per child.
        cache = self.cache_file()
        cache.parent.mkdir(parents=True, exist_ok=True)
        cache.write_text(json.dumps({
            "endpoint": self.base,
            "rows": [{"model": "m-anthropic", "dialect": "anthropic",
                      "note": "", "cap": 0, "nothink": False,
                      "id_recycle": True, "status": 200}]}))
        script = self.home / ".config/ccmorph/lib/ccmorph.sh"
        base_script = script.read_text()

        def stub(path, marker, port_line):
            """A translator/shim stand-in: serves /v1/models, records its pid."""
            path.write_text(
                f"#!{sys.executable}\n"
                "import json, os, re, sys\n"
                "if len(sys.argv) > 1 and sys.argv[1] == '-h':\n"
                "    sys.exit(0)\n"
                "from http.server import BaseHTTPRequestHandler, HTTPServer\n"
                + FAKE_SERVER_NO_DNS +
                f"open({str(marker)!r}, 'w').write(str(os.getpid()))\n"
                "class H(BaseHTTPRequestHandler):\n"
                "    def log_message(self, *a): pass\n"
                "    def do_GET(self):\n"
                "        body = json.dumps({'data': [{'id': 'm-anthropic'}]}).encode()\n"
                "        self.send_response(200)\n"
                "        self.send_header('content-length', str(len(body)))\n"
                "        self.end_headers()\n"
                "        self.wfile.write(body)\n"
                + port_line +
                "HTTPServer(('127.0.0.1', port), H).serve_forever()\n")
            path.chmod(0o755)

        translator_port = ("port = int(re.search(r'^port: (\\d+)', "
                           "open(sys.argv[2]).read(), re.M).group(1))\n")
        for hooks in (False, True):
            with self.subTest(hooks=hooks):
                tmp = self.home / f"tmp-hooks-{hooks}"
                tmp.mkdir()
                tpid = self.home / f"translator-{hooks}.pid"
                spid = self.home / f"shim-{hooks}.pid"
                stub(self.home / f"stub-translator-{hooks}", tpid, translator_port)
                self._install_fake_translator(self.home / f"stub-translator-{hooks}")
                if hooks:
                    hooks_shim = self.home / f"hooks-shim-{hooks}"
                    stub(hooks_shim, spid, "port = int(sys.argv[1])\n")
                    script.write_text(base_script.replace(
                        "}\n", f'  print "CCMORPH_SHIM_HOOKS={hooks_shim}"\n}}\n'))
                else:
                    script.write_text(base_script)
                out = tmp / "adapter.out"
                with open(out, "w") as log:
                    proc = subprocess.Popen(
                        [*adapter_cmd(), "p"], stdout=log, stderr=subprocess.DEVNULL,
                        env=dict(os.environ, HOME=str(self.home), TMPDIR=str(tmp),
                                 CCMORPH_LIB_DIR=str(self.home / ".config/ccmorph/lib"),
                                 CCMORPH_STATE_DIR=str(self.home),
                                 PATH=f"{self.path_bin}:{os.environ.get('PATH', '')}"))
                child_pids, seen = [], set()
                try:
                    deadline = time.monotonic() + HANG_TIMEOUT
                    while True:
                        # PORT= follows both children's readiness, so every pid
                        # has reached the tether by then.
                        seen |= self._tethers(proc.pid)
                        ready = (tpid.exists() and (not hooks or spid.exists())
                                 and "PORT=" in out.read_text())
                        if ready and seen:
                            break
                        if proc.poll() is not None:
                            self.fail(f"hooks={hooks}: adapter exited "
                                      f"{proc.returncode}: {out.read_text()}")
                        self.assertLess(time.monotonic(), deadline,
                                        f"hooks={hooks}: adapter never served: "
                                        f"{out.read_text()}")
                        time.sleep(0.1)
                    child_pids = [int(tpid.read_text())]
                    if hooks:
                        child_pids.append(int(spid.read_text()))
                    self.assertEqual(len(seen), 1,
                                     f"one tether per adapter, saw {sorted(seen)}")
                    proc.kill()
                    proc.wait(timeout=HANG_TIMEOUT)
                    deadline = time.monotonic() + HANG_TIMEOUT
                    while True:
                        seen |= self._tethers(proc.pid)
                        survivors = [p for p in child_pids if self._pid_alive(p)]
                        tethers = [p for p in seen if self._pid_alive(p)]
                        state = list(tmp.glob("ccmorph-adapter-*"))
                        if not survivors and not tethers and not state:
                            break
                        self.assertLess(time.monotonic(), deadline,
                                        f"hooks={hooks}: children alive {survivors}, "
                                        f"tethers alive {tethers}, state left {state}")
                        time.sleep(0.2)
                finally:
                    if proc.poll() is None:
                        proc.kill()
                        proc.wait(timeout=HANG_TIMEOUT)
                    for pid in child_pids + sorted(seen):
                        if self._pid_alive(pid):
                            os.kill(pid, signal.SIGKILL)

    def test_ready_timeout_exits_non_zero(self):
        # The contract with ccmorph: anything short of "printed PORT and served"
        # exits non-zero, so the caller aborts the launch. A translator that starts
        # but never serves used to exit 0, which read as success and left Claude
        # Code pointed at a port nothing listened on.
        stub = self.home / "stub-translator"
        stub.write_text("#!/bin/sh\nexec sleep 300\n")  # alive, never serves
        stub.chmod(0o755)
        self._install_fake_translator(stub)
        out = self.run_adapter(_constants={"READY_TIMEOUT": 3})
        self.assertNotEqual(out.returncode, 0,
                            "a ready-timeout must not look like success")
        self.assertNotIn("PORT=", out.stdout)

    def test_shim_readiness_failure_stops_the_translator(self):
        marker = self.home / "translator.pid"
        translator = self.home / "stub-translator"
        translator.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, sys\n"
            "from http.server import BaseHTTPRequestHandler, HTTPServer\n" + FAKE_SERVER_NO_DNS +
            f"open({str(marker)!r}, 'w').write(str(os.getpid()))\n"
            "class H(BaseHTTPRequestHandler):\n"
            "    def log_message(self, *a): pass\n"
            "    def do_GET(self):\n"
            "        body = json.dumps({'data': [{'id': 'm-anthropic'}]}).encode()\n"
            "        self.send_response(200)\n"
            "        self.send_header('content-length', str(len(body)))\n"
            "        self.end_headers()\n"
            "        self.wfile.write(body)\n"
            "conf = open(sys.argv[2]).read()\n"
            "import re\n"
            "port = int(re.search(r'^port: (\\d+)', conf, re.M).group(1))\n"
            "HTTPServer(('127.0.0.1', port), H).serve_forever()\n")
        translator.chmod(0o755)
        cache = self.cache_file()
        cache.parent.mkdir(parents=True, exist_ok=True)
        cache.write_text(json.dumps({
            "endpoint": self.base,
            "rows": [{"model": "m-anthropic", "dialect": "anthropic",
                      "note": "", "cap": 0, "nothink": False,
                      "id_recycle": True, "status": 200}]}))
        dead_shim = self.home / "dead-shim"
        dead_shim.write_text("#!/bin/sh\nexit 1\n")
        dead_shim.chmod(0o755)
        self._install_fake_translator(translator)

        # The stock shim runs inside the adapter; a profile's hooks build is the
        # spawned one, and this one dies before it serves.
        script = self.home / ".config/ccmorph/lib/ccmorph.sh"
        script.write_text(script.read_text().replace(
            "}\n", f'  print "CCMORPH_SHIM_HOOKS={dead_shim}"\n}}\n'))
        out = self.run_adapter(env={
            "PATH": f"{self.path_bin}:{os.environ.get('PATH', '')}"})
        self.assertNotEqual(out.returncode, 0)
        self.assertTrue(marker.exists())
        pid = int(marker.read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_profile_keys_ferry_into_the_shim_env_not_new_flags(self):
        shim = self.home / "argv-shim"
        argv_file = self.home / "shim.argv"
        env_file = self.home / "shim.env"
        shim.write_text(f"#!{sys.executable}\n"
                        "import json, os, sys\n"
                        f"open({str(argv_file)!r}, 'w').write(' '.join(sys.argv[1:]))\n"
                        f"open({str(env_file)!r}, 'w').write(json.dumps("
                        "{k: v for k, v in os.environ.items() if k.startswith('CCMORPH_')}))\n"
                        "sys.exit(1)\n")
        shim.chmod(0o755)
        # A hooks build is a whole shim: with CCMORPH_SHIM_HOOKS set the adapter
        # runs that binary in place of the stock one, so the recorder stands in.
        hooks = shim
        translator = self.home / "stub-translator"
        translator.write_text(
            "#!/usr/bin/env python3\n"
            "import json, re, sys\n"
            "from http.server import BaseHTTPRequestHandler, HTTPServer\n" + FAKE_SERVER_NO_DNS +
            "class H(BaseHTTPRequestHandler):\n"
            "    def log_message(self, *a): pass\n"
            "    def do_GET(self):\n"
            "        body = json.dumps({'data': [{'id': 'm-anthropic'}]}).encode()\n"
            "        self.send_response(200)\n"
            "        self.send_header('content-length', str(len(body)))\n"
            "        self.end_headers()\n"
            "        self.wfile.write(body)\n"
            "port = int(re.search(r'^port: (\\d+)', open(sys.argv[2]).read(), re.M).group(1))\n"
            "HTTPServer(('127.0.0.1', port), H).serve_forever()\n")
        translator.chmod(0o755)
        self._install_fake_translator(translator)
        cache = self.cache_file()
        cache.parent.mkdir(parents=True, exist_ok=True)
        cache.write_text(json.dumps({"endpoint": self.base, "rows": [
            {"model": "m-anthropic", "dialect": "anthropic", "note": "", "cap": 0,
             "nothink": False, "id_recycle": True, "status": 200}]}))
        script = self.home / ".config/ccmorph/lib/ccmorph.sh"
        base = script.read_text()
        for extra in ("", '  print "CCMORPH_RANDOM_DEVICE_ID=1"\n'
                           '  print "CCMORPH_USER_AGENT=ua/{version}"\n'
                           '  print "CCMORPH_AFFINITY_HEADER=X-Session-Affinity"\n'
                           f'  print "CCMORPH_SHIM_HOOKS={hooks}"\n'):
            script.write_text(base.replace("}\n", extra + "}\n") if extra else base)
            argv_file.unlink(missing_ok=True)
            env_file.unlink(missing_ok=True)
            if not extra:
                continue  # the stock shim runs inside the adapter: no process to record
            self.run_adapter(env={"PATH": f"{self.path_bin}:{os.environ.get('PATH', '')}"},
                             _shim=shim)
            seen = argv_file.read_text()
            self.assertEqual("--random-device-id" in seen, bool(extra), seen)
            self.assertNotIn("--affinity-header", seen)
            self.assertNotIn("--repair-tool-references", seen)
            ferried = json.loads(env_file.read_text())
            for k, v in (("CCMORPH_RANDOM_DEVICE_ID", "1"),
                         ("CCMORPH_USER_AGENT", "ua/{version}"),
                         ("CCMORPH_AFFINITY_HEADER", "X-Session-Affinity"),
                         ("CCMORPH_SHIM_HOOKS", str(hooks))):
                if extra:
                    self.assertEqual(ferried.get(k), v, ferried)
                else:
                    self.assertNotIn(k, ferried)

    def test_a_profile_pointing_at_missing_hooks_refuses_to_start(self):
        # The hooks path is validated before anything is spawned, so a stale
        # profile fails loudly at launch instead of through the shim later.
        script = self.home / ".config/ccmorph/lib/ccmorph.sh"
        base = script.read_text()
        not_executable = self.home / "hooks-shim"
        not_executable.write_text("#!/bin/sh\n")
        for path in ("/nonexistent/hooks-shim", str(not_executable)):
            script.write_text(base.replace(
                "}\n", f'  print "CCMORPH_SHIM_HOOKS={path}"\n}}\n'))
            out = self.run_adapter()
            self.assertNotEqual(out.returncode, 0, out.stdout + out.stderr)
            self.assertIn("CCMORPH_SHIM_HOOKS", out.stdout + out.stderr)

    def test_profile_name_is_not_executed_as_shell(self):
        # The profile name arrives on a command line and is handed to `zsh -c`.
        # Interpolating it into the script would execute anything it contains; it
        # is passed as a positional argument instead.
        canary = self.home / "canary"
        env = dict(os.environ,
                   HOME=str(self.home),
                   CCMORPH_LIB_DIR=str(self.home / ".config/ccmorph/lib"),
                   CCMORPH_STATE_DIR=str(self.home))
        out = subprocess.run(
            [*adapter_cmd(),
             f'p; touch "{canary}"', "--print-config"],
            capture_output=True, text=True, env=env, timeout=HANG_TIMEOUT)
        self.assertFalse(canary.exists(),
                         "the profile name was executed by the shell")
        # The stub ccmorph answers any _env call, so the launch itself may well
        # succeed; what matters is that the injected command never ran.
        self.assertNotIn("Traceback", out.stderr)

    def test_claude_code_faces_the_gateway_shim_not_the_translator(self):
        # A stub translator that actually serves /v1/models, so both health
        # checks pass and the adapter starts the shim. Claude Code must be
        # handed the shim's port, and that port must proxy to the translator.
        import urllib.request
        self.install_serving_stub_translator()

        parent = subprocess.Popen([sys.executable, "-c",
                                   "import time; time.sleep(30)"])
        proc, line = self.start_endpoint("--parent-pid", str(parent.pid))
        try:
            self.assertTrue(line.startswith("PORT="),
                            f"no PORT contract line: {line!r}")
            shim_port = int(line[5:])
            with urllib.request.urlopen(
                    f"http://127.0.0.1:{shim_port}/v1/models",
                    timeout=HANG_TIMEOUT) as r:
                self.assertEqual(json.loads(r.read()), {"data": []})
        finally:
            parent.terminate()
            parent.wait(timeout=HANG_TIMEOUT)
            proc.wait(timeout=HANG_TIMEOUT)

    def test_fixed_port_and_token_file_serve_the_translator_path(self):
        # The long-lived endpoint on the translator path (Hermes, #176): the
        # shim serves on exactly --port, the file's token is the only accepted
        # credential — the profile key itself is refused inbound — and the
        # token is never printed. The cached row carries id_recycle, which is
        # what forces the translator path; the stub translator serving is what
        # proves it ran.
        import urllib.error
        import urllib.request
        self.install_serving_stub_translator()
        self.seeded_cache(id_recycle=True)
        p = free_port()
        token = "h" * 40
        f = self.token_file(token)
        proc, line = self.start_endpoint("--auto", "--model", "m-anthropic",
                                         "--port", str(p),
                                         "--inbound-token-file", str(f))
        self.assertEqual(line, f"PORT={p}")

        def get(headers):
            req = urllib.request.Request(f"http://127.0.0.1:{p}/v1/models",
                                         headers=headers)
            try:
                with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
                    return r.status
            except urllib.error.HTTPError as e:
                return e.code

        self.assertEqual(get({"Authorization": f"Bearer {token}"}), 200)
        self.assertEqual(get({"x-api-key": "stub-key"}), 401)
        proc.terminate()
        out, err = proc.communicate(timeout=HANG_TIMEOUT)
        self.assertNotIn(token, line)
        self.assertNotIn(token, out)
        self.assertNotIn(token, err)
        self.assertFalse([l for l in out.splitlines() if l.startswith("TOKEN=")])

    def test_hermes_request_through_the_native_endpoint_reaches_upstream_filtered(self):
        # What a Hermes-shaped request (Anthropic SDK User-Agent, a task tag,
        # its own token, a device id) meets at the long-lived native endpoint:
        # exactly what every other profile's traffic meets. The upstream sees
        # the client's own User-Agent unchanged (issue 438: the claude-cli
        # rewrite left the core shim), no task tag, a swapped device_id,
        # and the profile key — never the client's token.
        import urllib.request
        fixture = json.loads(
            (REPO / "tests/fixtures/hermes_messages_request.json").read_text())
        self.seeded_cache(id_recycle=False)
        script = self.home / ".config/ccmorph/lib/ccmorph.sh"
        script.write_text(script.read_text().replace(
            "}\n", '  print "CCMORPH_RANDOM_DEVICE_ID=1"\n}\n'))
        p = free_port()
        token = "h" * 40
        f = self.token_file(token)
        # No translator installed: reaching PORT= at all proves the native
        # path (front_directly) served this endpoint.
        proc, line = self.start_endpoint("--auto", "--model", "m-anthropic",
                                         "--port", str(p),
                                         "--inbound-token-file", str(f))
        self.assertEqual(line, f"PORT={p}")
        req = urllib.request.Request(
            f"http://127.0.0.1:{p}{fixture['path']}",
            data=json.dumps(fixture["body"]).encode(),
            headers={"x-api-key": token, **fixture["headers"]}, method="POST")
        with urllib.request.urlopen(req, timeout=HANG_TIMEOUT) as r:
            self.assertEqual(r.status, 200)
        _, headers, raw = [e for e in UPSTREAM
                           if e[0].split("?")[0] == "/v1/messages"][-1]
        self.assertEqual(headers["user-agent"], "Anthropic/Python 0.69.0")
        self.assertNotIn("x-ccmorph-task", headers)
        self.assertEqual(headers["x-api-key"], "stub-key")
        self.assertTrue(all(token not in v for v in headers.values()))
        ids = json.loads(json.loads(raw)["metadata"]["user_id"])
        self.assertRegex(ids["device_id"], r"^[0-9a-f]{64}$")
        self.assertNotEqual(ids["device_id"], "d" * 64)
        self.assertEqual(ids["session_id"], "hermes-session-1")
        proc.terminate()
        out, err = proc.communicate(timeout=HANG_TIMEOUT)
        self.assertNotIn(token, out)
        self.assertNotIn(token, err)

    def test_the_fixed_port_stays_reserved_while_models_are_probed(self):
        # The whole classification phase runs between validating --port and
        # the shim's bind. The port must stay reserved through it: released
        # after a mere check, it would sit free for all of that, and whoever
        # grabbed it would collect the readiness probe's credential and
        # could answer 200 for a PORT= that points at itself.
        p = free_port()
        f = self.token_file()
        proc = subprocess.Popen(
            [*adapter_cmd(), "p", "--port", str(p),
             "--inbound-token-file", str(f)],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            env=self._launch_env(XDG_STATE_HOME=str(self.home / ".local/state")))

        def stop():
            if proc.poll() is None:
                proc.terminate()
            proc.communicate(timeout=HANG_TIMEOUT)

        self.addCleanup(stop)
        # No cache and no translator: the adapter sweeps the stub gateway's
        # models first — exactly the window the reservation must cover — and
        # dies at the translator check afterwards, which the assertion has
        # long outlived by then.
        for line in proc.stderr:
            if "new model(s)" in line:
                break
        else:
            self.fail("classification never started")
        squatter = socket.socket()
        with self.assertRaises(OSError):
            squatter.bind(("127.0.0.1", p))
        squatter.close()

    def test_a_stopped_fixed_port_restarts_over_time_wait(self):
        # A listener that closes a live connection first leaves TIME_WAIT on
        # its own port, and a bare bind refuses it. Restarting the long-lived
        # endpoint on such a port must still work: the reservation and the
        # shim both bind with SO_REUSEADDR.
        srv = socket.socket()
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        srv.bind(("127.0.0.1", 0))
        srv.listen(1)
        p = srv.getsockname()[1]
        client = socket.create_connection(("127.0.0.1", p), timeout=HANG_TIMEOUT)
        conn, _ = srv.accept()
        conn.close()   # the server closes first: TIME_WAIT lands on :p
        client.close()
        srv.close()
        self.install_serving_stub_translator()
        self.seeded_cache(id_recycle=True)
        f = self.token_file()
        proc, line = self.start_endpoint("--auto", "--model", "m-anthropic",
                                         "--port", str(p),
                                         "--inbound-token-file", str(f))
        self.assertEqual(line, f"PORT={p}")

    def test_a_listener_on_another_address_does_not_block_the_endpoint(self):
        # The endpoint is 127.0.0.1:<port> and nothing else, so a service on
        # another address of this machine must not make --port refuse it. On
        # Linux a wildcard bind probe conflicts with such a listener even
        # though the loopback endpoint is free; the occupancy check asks the
        # endpoint address directly instead.
        finder = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            finder.connect(("10.255.255.255", 1))   # picks the route; sends nothing
            lan = finder.getsockname()[0]
        finally:
            finder.close()
        if lan in ("0.0.0.0", "127.0.0.1"):
            self.skipTest("no second address on this machine")
        other = socket.socket()
        other.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        other.bind((lan, 0))
        other.listen(8)
        self.addCleanup(other.close)
        p = other.getsockname()[1]
        self.seeded_cache(id_recycle=False)
        f = self.token_file()
        proc, line = self.start_endpoint("--auto", "--model", "m-anthropic",
                                         "--port", str(p),
                                         "--inbound-token-file", str(f))
        self.assertEqual(line, f"PORT={p}")

    def test_long_lived_endpoint_refuses_bad_inputs_before_starting(self):
        # The long-lived endpoint form (a fixed --port plus an
        # --inbound-token-file, for a non-Claude client such as Hermes, #176):
        # every input is checked before anything starts, so a service is never
        # left half-configured with its credential half-checked, nothing is
        # contacted, and the token never reaches an output stream.
        held = socket.socket()
        held.bind(("127.0.0.1", 0))
        held.listen(8)
        self.addCleanup(held.close)
        held_p = held.getsockname()[1]
        # A wildcard listener receives 127.0.0.1 traffic too, and on macOS a
        # specific REUSEADDR bind succeeds right over it — so the check must
        # probe the wildcard form as well, or validation would pass a port
        # another service occupies and hand it the readiness probe's token.
        wildcard = socket.socket()
        wildcard.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        wildcard.bind(("", 0))
        wildcard.listen(8)
        self.addCleanup(wildcard.close)
        wildcard_p = wildcard.getsockname()[1]

        good = self.token_file()
        broad = self.token_file(mode=0o644)
        linked = self.home / "token-link"
        linked.symlink_to(good)
        directory = self.home / "token-dir"
        directory.mkdir()
        cases = [
            ("mode-0644", broad, free_port(),
             f"inbound token file {broad}", "t" * 40),
            ("symlink-to-valid-file", linked, free_port(),
             f"inbound token file {linked}", "t" * 40),
            ("directory", directory, free_port(),
             f"inbound token file {directory}", None),
            ("too-short", self.token_file("t" * 31), free_port(),
             "holds fewer than 32 characters", "t" * 31),
            ("missing-path", self.home / "token-missing", free_port(),
             "inbound token file", None),
            ("empty-path-supplied", "", free_port(),
             "inbound token file", None),
            ("interior-newline", self.token_file("t" * 20 + "\n" + "t" * 19),
             free_port(), "holds control characters", "t" * 20 + "\n" + "t" * 19),
            ("oversized", self.token_file("t" * 5000), free_port(),
             "holds more than 4096 characters", "t" * 5000),
            ("port-held-by-another-socket", good, held_p,
             f"--port {held_p}", "t" * 40),
            ("port-held-by-a-wildcard-listener", good, wildcard_p,
             f"--port {wildcard_p}", "t" * 40),
            ("port-out-of-range", good, 80, "--port 80", "t" * 40),
            ("port-zero", good, 0, "--port 0", "t" * 40),
        ]
        for label, f, p, phrase, secret in cases:
            with self.subTest(label):
                out = self.run_adapter("--auto", "--port", str(p),
                                       "--inbound-token-file", str(f))
                self.assertNotEqual(out.returncode, 0)
                self.assertNotIn("PORT=", out.stdout)
                self.assertNotIn("TOKEN=", out.stdout)
                self.assertEqual(PROBES, [], "something was contacted")
                if secret:
                    self.assertNotIn(secret, out.stdout)
                    self.assertNotIn(secret, out.stderr)
                self.assertIn(phrase, out.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
