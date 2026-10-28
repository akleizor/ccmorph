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

import importlib.machinery
import importlib.util
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

from support import HANG_TIMEOUT, HELPERS_IMPL, helper_cmd

SCRIPT = Path(__file__).resolve().parent.parent / "libexec/refresh-model-registry"
spec = importlib.util.spec_from_loader(
    "refresh_model_registry",
    importlib.machinery.SourceFileLoader("refresh_model_registry", str(SCRIPT)))
assert spec and spec.loader
rmr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rmr)
rmr._fetch_json_dict = lambda url, timeout: {}  # offline: no live catalogues in tests

# Tests that import the Python module and patch its internals have Go unit-test
# equivalents in registry/*_test.go; under CCMORPH_TEST_HELPERS=go only the
# subprocess tests run, against `ccmorph-core refresh-model-registry`.
whitebox = unittest.skipIf(HELPERS_IMPL == "go", "white-box; see registry/*_test.go")


@whitebox
class DiagnosticWording(unittest.TestCase):
    def test_access_denial_does_not_diagnose_an_expired_credential(self):
        error = urllib.error.HTTPError("https://gw/v1/models", 403, "Forbidden", {}, None)
        message = rmr.why_unreachable(error, "https://gw")
        self.assertIn("HTTP 403", message)
        self.assertIn("access", message)
        self.assertNotIn("has no usable forward_ip", message)

    def test_a_timeout_names_the_host_not_the_exception_class(self):
        message = rmr.why_unreachable(TimeoutError(), "https://gw")
        self.assertIn("did not answer at gw", message)
        self.assertNotIn("this is an internal", message)
        self.assertLess(len(message), 150)

    def test_partial_probe_failure_prints_its_reason(self):
        import contextlib
        with unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(["known", "failed"], {"known": 300000}, [])), \
                unittest.mock.patch.object(rmr, "probe", return_value=("anthropic:error", None, "gateway timed out")), \
                contextlib.redirect_stderr(io.StringIO()) as output:
            rmr.probe_all(["gw"], 1, mode="auto")
        self.assertIn("gateway timed out", output.getvalue())


@whitebox
class SafeDiscovery(unittest.TestCase):
    def test_default_cli_is_metadata_only_and_bounded(self):
        with unittest.mock.patch.object(sys, "argv", ["probe", "--profile", "gw"]), \
                unittest.mock.patch.object(rmr, "probe_all", return_value=({}, [], {})) as run, \
                unittest.mock.patch.object(rmr, "prior_values", return_value={}):
            rmr.main()
        self.assertEqual(run.call_args.args[2], "declared-only")
        self.assertEqual(run.call_args.args[5], 30)

    def test_paid_modes_require_explicit_consent_and_one_model(self):
        for flags in (["--bisect"], ["--probe-all"], ["--allow-inference"],
                      ["--allow-inference", "--model", "a", "--model", "b"],
                      ["--declared-only", "--allow-inference", "--model", "a"]):
            with self.subTest(flags=flags), \
                    unittest.mock.patch.object(sys, "argv", ["probe", "--profile", "gw", *flags]), \
                    unittest.mock.patch.object(rmr, "ccmorph_env") as route, \
                    self.assertRaises(SystemExit) as raised:
                rmr.main()
            self.assertEqual(raised.exception.code, 2)
            route.assert_not_called()

    def test_direct_discovery_default_never_builds_or_sends_a_prompt(self):
        with unittest.mock.patch.object(rmr, "ccmorph_env", return_value={
                "ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gw"}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(["a", "b"], {"a": 128000}, [])), \
                unittest.mock.patch.object(rmr, "payload") as payload, \
                unittest.mock.patch.object(rmr, "probe") as send:
            values, notes, sources = rmr.probe_all(["gw"], jobs=1)
        payload.assert_not_called()
        send.assert_not_called()
        self.assertEqual(values, {"gw:a": 128000})
        self.assertTrue(any(subject == "b" for _, subject in notes))


@whitebox
class PartialDiscoveryStatus(unittest.TestCase):
    def test_known_value_does_not_hide_missing_gateway_metadata(self):
        import contextlib
        for flags in ([], ["--check"]):
            with self.subTest(flags=flags), \
                    unittest.mock.patch.object(sys, "argv", ["probe", "--profile", "profile", *flags]), \
                    unittest.mock.patch.object(rmr, "prior_values", return_value={"profile:known": 922000}), \
                    unittest.mock.patch.object(rmr, "probe_all", return_value=(
                        {"profile:known": 922000}, [(rmr.UNRESOLVED, "gpt-5.6-sol")], {})), \
                    unittest.mock.patch.object(rmr, "commit", return_value=0), \
                    contextlib.redirect_stdout(io.StringIO()) as out, \
                    contextlib.redirect_stderr(io.StringIO()) as err:
                result = rmr.main()
                self.assertEqual(result, 1)
                self.assertIn("gpt-5.6-sol", err.getvalue())
                self.assertIn("unresolved", err.getvalue())
                # Disclosing it is half the job; the remedy has to come with it.
                self.assertIn("ccmorph context profile gpt-5.6-sol", err.getvalue())
                self.assertNotIn("matches discovery", out.getvalue())

    def test_a_dead_end_names_the_free_fix_not_only_the_paid_one(self):
        """The whole-run failure path must still say what to DO.

        `probe <profile> --model <new-id>` resolves nothing, so `values` is empty
        and main() returns on the `not values` branch. That branch printed the
        reason and what was preserved, and the remedies lived after it -- reachable
        only when some other model in the run resolved. The one shape where the
        reader asked about exactly one model was the one shape with no next step.
        """
        import contextlib
        with unittest.mock.patch.object(sys, "argv", [
                    "probe", "--profile", "gateway", "--model", "deepseek-v4.1-flash"]), \
                unittest.mock.patch.object(rmr, "prior_values", return_value={"gateway:other": 922000}), \
                unittest.mock.patch.object(rmr, "probe_all", return_value=(
                    {}, [(rmr.UNRESOLVED, "deepseek-v4.1-flash")], {})), \
                contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(rmr.main(), 1)
        message = err.getvalue()
        # The free fix, with the profile and model already filled in.
        self.assertIn("ccmorph context gateway deepseek-v4.1-flash", message)
        # The paid one stays disclosed as such, and stays last.
        self.assertIn("--allow-inference", message)
        self.assertLess(message.index("ccmorph context"), message.index("--allow-inference"))
        # And what happens if the reader does nothing.
        self.assertIn("200,000", message)


@whitebox
class DiscoveryDeadline(unittest.TestCase):
    def test_route_setup_listing_cleanup_and_paid_requests_share_one_deadline(self):
        import time

        def blocked(*args, **kwargs):
            # A block only the deadline can end. Whatever breaks the timer,
            # the run goes red, never a pass by luck: an unarmed timer is
            # reported by the suite's 60 s cap, and one that fires without
            # raising runs each phase to the hang bound and fails by name
            # below -- run_deadline's arming replaces the itimer that cap
            # uses, so nothing sooner ends it.
            time.sleep(HANG_TIMEOUT)
            raise AssertionError("the run deadline never interrupted this phase")

        for slow_phase in ("ccmorph_env", "list_models", "route_stop", "probe"):
            with self.subTest(phase=slow_phase), \
                    unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                    unittest.mock.patch.object(rmr, "ccmorph_env", return_value={
                        "ANTHROPIC_API_KEY": "k", "CCMORPH_ROUTE_HANDLE": "handle"}), \
                    unittest.mock.patch.object(rmr, "list_models", return_value=(["a"], {"a": 123000}, [])), \
                    unittest.mock.patch.object(rmr, "route_stop"), \
                    unittest.mock.patch.object(rmr, "probe"), \
                    unittest.mock.patch.object(rmr, slow_phase, side_effect=blocked):
                # No elapsed-time assertion: the itimer fires on schedule, but
                # the signal is delivered when the scheduler next runs this
                # process, so a loaded host can return arbitrarily late while
                # the deadline still works (issue 432). What the deadline
                # promises is interruption, and that is what is asserted.
                _, notes, _ = rmr.probe_all(["gw"], 1, mode="probe-all" if slow_phase == "probe" else "declared-only", deadline=0.05)
                self.assertTrue(any("deadline reached" in reason for reason, _ in notes))

    def test_deadline_during_route_cleanup_preserves_discovered_values(self):
        # The route_stop phase used to carry this claim inside the timed loop,
        # which made it depend on where a loaded host delivered the signal.
        # Raising the deadline from the cleanup call itself pins the same
        # promise with no clock: deadline=30 is far beyond this mocked,
        # microsecond body, so the interrupt always comes from that call --
        # values recorded before cleanup survive, and the operator still
        # gets the deadline note.
        with unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={
                    "ANTHROPIC_API_KEY": "k", "CCMORPH_ROUTE_HANDLE": "handle"}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(["a"], {"a": 123000}, [])), \
                unittest.mock.patch.object(rmr, "route_stop", side_effect=rmr.RunDeadline) as stop:
            values, notes, _ = rmr.probe_all(["gw"], 1, deadline=30)
        stop.assert_called_once_with("handle")
        self.assertEqual(values, {"gw:a": 123000})
        self.assertTrue(any("deadline reached" in reason for reason, _ in notes))

    def test_cached_readings_survive_lower_metadata_and_do_not_spend_again(self):
        for previous in ({"gw:a": 1000000},):
            with self.subTest(previous=previous), \
                    unittest.mock.patch.object(rmr, "prior_values", return_value=previous), \
                    unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                    unittest.mock.patch.object(rmr, "list_models", return_value=(["a"], {"a": 200000}, [])), \
                    unittest.mock.patch.object(rmr, "probe") as probe:
                values, _, _ = rmr.probe_all(["gw"], 1, mode="auto")
                self.assertEqual(values, {"gw:a": 1000000})
                probe.assert_not_called()


@whitebox
class RouteScopedMetadata(unittest.TestCase):
    def test_other_routes_legacy_limit_does_not_become_a_scoped_fact(self):
        with unittest.mock.patch.object(rmr, "prior_values", return_value={"a": 1000000}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(["a"], {"a": 200000}, [])):
            values, _, _ = rmr.probe_all(["other"], 1)
        self.assertEqual(values, {"other:a": 200000})

    def test_deadline_attempts_bounded_route_cleanup(self):
        with unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={
                    "ANTHROPIC_API_KEY": "k", "CCMORPH_ROUTE_HANDLE": "H"}), \
                unittest.mock.patch.object(rmr, "list_models", side_effect=rmr.RunDeadline), \
                unittest.mock.patch.object(rmr, "route_stop") as stop:
            rmr.probe_all(["gw"], 1, deadline=0.1)
        stop.assert_called_once_with("H")


class MetadataCompatibility(unittest.TestCase):
    @whitebox
    def test_ollama_reads_served_num_ctx_without_inference(self):
        calls = []
        class Gateway:
            def open(self, req, **kwargs):
                calls.append(req)
                if req.get_method() == "GET":
                    return io.BytesIO(json.dumps({"data": [
                        {"id": "qwen3-coder:64k", "owned_by": "ollama"},
                        {"id": "known", "owned_by": "ollama", "context_length": 32000},
                        {"id": "opaque", "owned_by": "other"}
                    ]}).encode())
                return io.BytesIO(json.dumps({"parameters": "num_ctx 65536\n",
                    "model_info": {"context_length": 262144}}).encode())
        models, sizes, rows = rmr.list_models("http://local", "k", opener=Gateway())
        self.assertEqual(sizes, {"qwen3-coder:64k": 65536, "known": 32000})
        self.assertEqual(len(calls), 2)
        self.assertTrue(calls[1].full_url.endswith("/api/show"))
        self.assertEqual(json.loads(calls[1].data), {"model": "qwen3-coder:64k"})

    @whitebox
    def test_listing_stores_limits_under_undisguised_ids(self):
        class Gateway:
            def open(self, req, **kwargs):
                return io.BytesIO(json.dumps({"data": [
                    {"id": "claude-fable-5-dd-los-6-tpg", "context_length": 272000},
                    {"id": "claude-opus-5", "context_length": 200000}
                ]}).encode())
        models, sizes, rows = rmr.list_models("http://local", "k", opener=Gateway())
        self.assertEqual(sorted(models), ["claude-opus-5", "gpt-6-sol"])
        self.assertEqual(sizes, {"gpt-6-sol": 272000, "claude-opus-5": 200000})
        self.assertIn("gpt-6-sol", [row["id"] for row in rows])

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

    @whitebox
    def test_cli_reports_unresolved_metadata_offline_without_post(self):
        import contextlib
        # Unlike the subprocess case, this unknown chat model intentionally needs
        # fallback. Exercise the real CLI with only the catalogue I/O seams offline.
        self.rows[-1]["capabilities"].pop("chat_completion")
        with unittest.mock.patch.dict(os.environ, self.env), \
                unittest.mock.patch.object(rmr, "CCMORPH", self.script), \
                unittest.mock.patch.object(sys, "argv", [str(SCRIPT), "--profile", "gw"]), \
                unittest.mock.patch.object(rmr, "fetch_catalogue", return_value={}) as fetch, \
                unittest.mock.patch.object(rmr, "fetch_canonical_catalogue", return_value={}) as canonical, \
                contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(rmr.main(), 1)
        self.assertIn("unresolved", err.getvalue())
        fetch.assert_called_once()
        canonical.assert_called_once()
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

    @whitebox
    def test_issue_259_acquired_headers_do_not_leave_a_body_unbounded(self):
        self.block_body = True
        stored = self.conf.read_text()
        req = urllib.request.Request(self.base + "/v1/models", headers={
            "Authorization": "Bearer canary", "x-api-key": "canary",
            "anthropic-version": "2023-06-01"})
        expected_headers = dict(req.header_items())
        # Acquire the real response headers before arming the unchanged deadline;
        # route startup and host scheduling cannot steal this body's test budget.
        with urllib.request.urlopen(req, timeout=rmr.LIST_TIMEOUT) as response:
            self.assertEqual(response.status, 200)
            fixture = self
            class AcquiredResponse:
                acquired = response
                def open(self, request, **kwargs):
                    fixture.assertIsNotNone(self.acquired, "listing must not reopen the response")
                    fixture.assertEqual(request.full_url, req.full_url)
                    fixture.assertEqual(request.get_method(), "GET")
                    fixture.assertEqual(dict(request.header_items()), expected_headers)
                    fixture.assertEqual(kwargs, {"timeout": rmr.LIST_TIMEOUT})
                    acquired, self.acquired = self.acquired, None
                    return acquired
            with self.assertRaises(rmr.RunDeadline), rmr.run_deadline(0.2):
                rmr.list_models(self.base, "canary", opener=AcquiredResponse())
            self.assertTrue(response.closed)
        self.assertFalse(self.release.is_set())
        self.assertEqual(self.conf.read_text(), stored)
        self.assertFalse((self.root / "registry").exists())
        self.assertEqual(self.requests, [("GET", "/v1/models", "2023-06-01")])


@whitebox
class CCMorphEnv(unittest.TestCase):
    def test_profile_and_packaged_script_are_positional_arguments(self):
        import subprocess
        saved = rmr.subprocess.run
        calls = []

        def fake_run(argv, **kwargs):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, stdout="KEY=value\n", stderr="")

        rmr.subprocess.run = fake_run
        try:
            self.assertEqual(rmr.ccmorph_env("name; touch /tmp/pwned"), {"KEY": "value"})
        finally:
            rmr.subprocess.run = saved
        self.assertEqual(
            calls[0][-3:],
            ["refresh-model-registry", str(rmr.CCMORPH),
             "name; touch /tmp/pwned"])
        self.assertIn('source "$1"', calls[0][2])
        # _route_env, not _env: a provider profile has no endpoint until its
        # bridge runs, and only _route_env starts one (see ccmorph_env).
        self.assertIn('ccmorph _route_env "$2"', calls[0][2])

    def test_routed_profiles_are_chosen_by_conf_type(self):
        # Every routed type has a listing to size, a provider included; a sub or
        # token profile answers natively and is never swept.
        with tempfile.TemporaryDirectory() as tmp, \
                unittest.mock.patch.dict(os.environ, {"CCMORPH_STATE_DIR": tmp}):
            for name, kind in (("gw", "apikey"), ("old", "proxy"), ("codex", "provider"),
                               ("me", "sub"), ("tk", "token")):
                Path(tmp, f"{name}.conf").write_text(f"type={kind}\n")
            self.assertEqual(rmr.routed_profiles(), ["codex", "gw", "old"])

    def test_stop_reason_is_a_refusal_not_an_acceptance(self):
        # glm-5.2's real behaviour: HTTP 200, empty content, usage all zeroes, and the
        # reason stated outright. Claude Code surfaces this as a context-limit message,
        # so the model is safe to offer -- but no ceiling was named, only that the
        # overshoot was above it.
        kind, value, note = rmr.classify_accepted(
            full=(0, "model_context_window_exceeded"), half=None)
        self.assertEqual(kind, "exceeded")
        self.assertIsNone(value)
        self.assertIn("stop_reason", note)

    def test_a_plateau_is_the_ceiling(self):
        # Halving the input did not change what the model reported, so the extra
        # tokens went nowhere: the plateau is the limit, and it is a real probe.
        kind, value, note = rmr.classify_accepted(
            full=(131_072, "end_turn"), half=(131_010, "end_turn"))
        self.assertEqual(kind, "truncated")
        self.assertEqual(value, 131_072)
        self.assertIn("plateau", note)

    def test_tracking_the_input_means_nothing_was_dropped(self):
        # Half the input reported roughly half the tokens, so the whole prompt arrived
        # and the ceiling is merely somewhere above it. A floor must not be recorded as
        # a ceiling: consumers treat these numbers as limits, and overstating one fails
        # late instead of compacting early.
        kind, value, note = rmr.classify_accepted(
            full=(1_100_000, "end_turn"), half=(550_000, "end_turn"))
        self.assertEqual(kind, "intact")
        self.assertIsNone(value)
        self.assertIn("at least", note)

    def test_an_unusable_confirmation_records_nothing(self):
        # Without a second reading there is no way to tell a plateau from a prompt that
        # simply fitted, so neither conclusion may be drawn.
        kind, value, _ = rmr.classify_accepted(full=(900_000, "end_turn"), half=None)
        self.assertEqual(kind, "inconclusive")
        self.assertIsNone(value)


@whitebox
class ClaudeModelsAreProbed(unittest.TestCase):
    """The generator must not skip claude-*.

    The skip was the Opus 5 defect: a newly appended claude-* model was never
    probed and never entered the registry, so it silently launched at the
    200k default while the gateway held 1M. Claude Code "knowing its own
    catalogue" no longer holds -- it budgets 200k for a bare claude-opus-5 id,
    and only the probed entry lets model-window derive the [1m] send_id.
    """

    def _run_probe(self, models):
        import io
        import contextlib
        saved = {k: getattr(rmr, k) for k in
                 ("ccmorph_env", "list_models", "probe")}
        rmr.ccmorph_env = lambda profile: {
            "ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://x"}
        rmr.list_models = lambda base, key, timeout=None, opener=None: (models, {}, [])
        rmr.probe = lambda base, key, model, blob, timeout=None, opener=None, bisect=0: (
            "anthropic:declared", 1_000_000, "")
        import tempfile
        try:
            # An isolated state dir: the operator's real gateway.conf must not leak in.
            with tempfile.TemporaryDirectory() as tmp, \
                    unittest.mock.patch.dict(os.environ, {"CCMORPH_STATE_DIR": tmp}), \
                    contextlib.redirect_stderr(io.StringIO()):
                return rmr.probe_all(["gateway"], jobs=1, mode="probe-all")
        finally:
            for k, v in saved.items():
                setattr(rmr, k, v)

    def test_claude_models_are_probed_not_skipped(self):
        values, notes, _ = self._run_probe(
            ["claude-opus-5", "claude-haiku-4-5", "gpt-5.6-sol"])
        self.assertEqual(values.get("gateway:claude-opus-5"), 1_000_000)
        self.assertEqual(values.get("gateway:claude-haiku-4-5"), 1_000_000)
        self.assertEqual(values.get("gateway:gpt-5.6-sol"), 1_000_000)

    def test_declared_models_are_recorded_without_probing(self):
        # The listing states sizes for two of three models: those two are
        # recorded from the listing and only the third is probed.
        import io
        import contextlib
        probed = []
        saved = {k: getattr(rmr, k) for k in
                 ("ccmorph_env", "list_models", "probe")}
        rmr.ccmorph_env = lambda profile: {
            "ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://x"}
        rmr.list_models = lambda base, key, timeout=None, opener=None: (
            ["a", "b", "c"], {"a": 128_000, "b": 1_000_000}, [])
        def fake_probe(base, key, model, blob, timeout=None, opener=None, bisect=0):
            probed.append(model)
            return ("anthropic:declared", 700_000, "")
        rmr.probe = fake_probe
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                values, notes, _ = rmr.probe_all(["gw"], jobs=1)
        finally:
            for k, v in saved.items():
                setattr(rmr, k, v)
        self.assertEqual(probed, [])
        self.assertEqual(values, {"gw:a": 128_000, "gw:b": 1_000_000})


@whitebox
class OverLimitErrorParsing(unittest.TestCase):
    """The ceiling must be read out of the body the gateway actually sends.

    tencent answers a 400 whose JSON unicode-escapes the comparison:
    {"error":{"message":"prompt is too long: 1552710 tokens > 1000000
    maximum"}} arrives with \\u003e for ">". Searching the raw body for the
    declared-limit patterns finds nothing, and a probed 1M model lands in
    the notes as opaque. The extractor must unwrap error.message first.
    """

    def _run_probe(self, detail_json):
        import io
        import urllib.error
        saved = rmr.urllib.request.urlopen

        def raise_http(req, timeout=None):
            raise urllib.error.HTTPError(
                req.full_url, 400, "Bad Request", None, io.BytesIO(detail_json))
        rmr.urllib.request.urlopen = raise_http
        try:
            return rmr.probe("http://x", "k", "claude-opus-4-6", "blob")
        finally:
            rmr.urllib.request.urlopen = saved

    def test_escaped_error_message_yields_the_ceiling(self):
        detail = (b'{"error":{"message":"prompt is too long: 1552710 tokens '
                  b'\\u003e 1000000 maximum"}}')
        kind, value, _ = self._run_probe(detail)
        self.assertEqual(kind, "anthropic:declared")
        self.assertEqual(value, 1_000_000)

    def test_text_prefixed_gateway_wrap_still_yields_the_ceiling(self):
        # gateway prefixes the upstream body as text: the whole body is not
        # JSON, so no unwrap runs, and the escaped ">" must be normalized
        # before the declared-limit patterns are tried.
        detail = (b'tencent 400 client error {"error":{"message":"prompt is '
                  b'too long: 2277794 tokens \\u003e 1000000 maximum"}}')
        kind, value, _ = self._run_probe(detail)
        self.assertEqual(kind, "anthropic:declared")
        self.assertEqual(value, 1_000_000)

    def test_nested_error_message_is_unwrapped(self):
        detail = b'{"error":{"message":"maximum context length is 131072 tokens"}}'
        kind, value, _ = self._run_probe(detail)
        self.assertEqual(kind, "anthropic:declared")
        self.assertEqual(value, 131_072)


@whitebox
class ProbeOutputStaysOnPoint(unittest.TestCase):
    def _run(self, models, only=None, conf=""):
        import contextlib
        import tempfile
        with tempfile.TemporaryDirectory() as tmp, \
                unittest.mock.patch.dict(os.environ, {"CCMORPH_STATE_DIR": tmp}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "prior_catalogue_tags", return_value={}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(models, {}, [])), \
                unittest.mock.patch.object(rmr, "fetch_canonical_catalogue", return_value={}), \
                contextlib.redirect_stderr(io.StringIO()):
            Path(tmp, "gw.conf").write_text(conf)
            return rmr.probe_all(["gw"], 1, only_models=only)

    def test_excluded_models_are_not_reported_unresolved(self):
        _, notes, _ = self._run(["kept", "hidden"], conf="excluded_models=hidden\n")
        unresolved = {m for r, m in notes if r == rmr.UNRESOLVED}
        self.assertEqual(unresolved, {"kept"})

    def test_a_near_miss_model_id_suggests_the_real_one(self):
        _, notes, _ = self._run(["claude-opus-5-5", "gpt-5"], only={"opus-5-5"})
        self.assertTrue(any("did you mean claude-opus-5-5" in r for r, _ in notes), notes)


if __name__ == "__main__":
    unittest.main(verbosity=2)


@whitebox
class ArgumentValidation(unittest.TestCase):
    def test_non_positive_jobs_is_an_argparse_error(self):
        import contextlib
        import io
        saved_argv = rmr.sys.argv
        try:
            for jobs in ("0", "-1"):
                with self.subTest(jobs=jobs):
                    rmr.sys.argv = ["refresh-model-registry", "--profile", "p",
                                    "--jobs", jobs]
                    err = io.StringIO()
                    with contextlib.redirect_stderr(err), self.assertRaises(SystemExit) as raised:
                        rmr.main()
                    self.assertEqual(raised.exception.code, 2)
                    self.assertIn("--jobs", err.getvalue())
                    self.assertNotIn("Traceback", err.getvalue())
        finally:
            rmr.sys.argv = saved_argv


@whitebox
class DeclaredSizes(unittest.TestCase):
    """A gateway that STATES sizes in its listing is never probed for them.

    Gateways can declare equal limits for several models. Only explicit positive
    integer metadata is accepted; no inference is needed to read it.
    """

    def test_harvests_any_known_field_and_skips_undeclared(self):
        rows = [
            {"id": "a", "context_length": 128000},
            {"id": "b", "max_input_tokens": 200000},
            {"id": "c"},
            {"id": "d", "max_context_length": "not-an-int"},
        ]
        self.assertEqual(rmr.declared_sizes(rows), {"a": 128000, "b": 200000})

    def test_equal_explicit_limits_are_still_metadata(self):
        rows = [{"id": x, "max_input_tokens": 200000} for x in "abc"]
        self.assertEqual(rmr.declared_sizes(rows), {x: 200000 for x in "abc"})

    def test_two_models_agreeing_is_not_flat_when_a_third_differs(self):
        rows = [
            {"id": "a", "context_length": 200000},
            {"id": "b", "context_length": 200000},
            {"id": "c", "context_length": 1000000},
        ]
        self.assertEqual(
            rmr.declared_sizes(rows),
            {"a": 200000, "b": 200000, "c": 1000000})

    def test_matching_declarations_are_not_flat_when_a_model_declares_nothing(self):
        rows = [
            {"id": "a", "context_length": 200000},
            {"id": "b", "context_length": 200000},
            {"id": "c"},
        ]
        self.assertEqual(rmr.declared_sizes(rows),
                         {"a": 200000, "b": 200000})


@whitebox
class ModelScope(unittest.TestCase):
    """--model scopes a run to one model — what a launch needs — instead of the
    whole catalogue. This is the difference between a single probe on first launch
    and an N-model sweep that reads as a hang."""

    CATALOG = ["gpt-5.6-sol", "kimi-k2", "glm-5.2", "qwen-max"]

    def _run(self, only):
        probed = []

        def fake_probe(base, key, m, blob, timeout=None, opener=None, bisect=0):
            probed.append(m)
            return (f"{m}:declared", 123456, "")

        saved_list = rmr.list_models
        saved_env = rmr.ccmorph_env
        saved_probe = rmr.probe
        rmr.list_models = lambda base, key, timeout=None, opener=None: (list(self.CATALOG), {}, [])
        rmr.ccmorph_env = lambda p: {
            "ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://x"}
        rmr.probe = fake_probe
        try:
            with unittest.mock.patch.object(rmr, "prior_values", return_value={}):
                values, _, _ = rmr.probe_all(["pv"], jobs=4, mode="auto", only_models=only)
        finally:
            rmr.list_models = saved_list
            rmr.ccmorph_env = saved_env
            rmr.probe = saved_probe
        return probed, values

    def test_model_scopes_the_probe_to_one_model(self):
        probed, values = self._run({"kimi-k2"})
        self.assertEqual(probed, ["kimi-k2"])
        self.assertEqual(set(values), {"pv:kimi-k2"})

    def test_no_scope_probes_the_whole_catalogue(self):
        probed, _ = self._run(None)
        self.assertEqual(set(probed), set(self.CATALOG))

    def test_requested_model_absent_from_listing_probes_nothing(self):
        probed, values = self._run({"not-in-catalogue"})
        self.assertEqual(probed, [])
        self.assertEqual(values, {})


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(unittest.main())


@whitebox
class ProfileCommitOrdering(unittest.TestCase):
    def test_stale_same_profile_result_is_rejected_and_other_profile_survives(self):
        with tempfile.TemporaryDirectory() as tmp, unittest.mock.patch.dict(os.environ, {"CCMORPH_STATE_DIR": tmp}):
            for name in ("a", "b"):
                Path(tmp, name + ".conf").write_text("type=apikey\napi_key=canary\n")
            identities = {name: rmr.store.profile_identity(name) for name in ("a", "b")}
            self.assertEqual(rmr.commit({"a:m": 500000}, [], identities=identities), 0)
            self.assertEqual(rmr.commit({"a:m": 200000, "b:m": 300000}, [], identities=identities), 1)
            self.assertEqual(rmr.prior_values(), {"a:m": 500000, "b:m": 300000})

    def test_new_model_write_preserves_existing_values_and_manual_pin(self):
        with tempfile.TemporaryDirectory() as tmp, unittest.mock.patch.dict(os.environ, {"CCMORPH_STATE_DIR": tmp}):
            Path(tmp, "a.conf").write_text("type=apikey\napi_key=canary\ncontext_window.manual=100000\n")
            for model, value in (("one", 200000), ("two", 300000)):
                identities = {"a": rmr.store.profile_identity("a")}
                self.assertEqual(rmr.commit({f"a:{model}": value}, [], identities=identities), 0)
            self.assertEqual(rmr.prior_values(), {"a:one": 200000, "a:two": 300000})
            self.assertEqual(rmr.store.context_value("a", "manual"), "100000")


@whitebox
class AmbientProxyIsUsed(unittest.TestCase):
    """A private endpoint is reached through the caller's own proxy.

    ccmorph ships no transport: a bridge exports HTTPS_PROXY and ccmorph passes
    it through. This tool is one of the consumers that must inherit it, and the
    bug this pins is the opposite of inheritance -- an opener that installs its
    own empty ProxyHandler silently BLANKS the operator's bridge, so probing off
    the private network could only ever time out.
    """

    def test_env_asks_ccmorph_for_the_route(self):
        """ccmorph_env goes through _route_env, which starts a provider bridge."""
        import subprocess
        saved = rmr.subprocess.run
        scripts = []

        def fake_run(argv, **kwargs):
            scripts.append(argv[2] if len(argv) > 2 else "")
            return subprocess.CompletedProcess(
                argv, 0, stdout="ANTHROPIC_BASE_URL=https://gw\n"
                                "ANTHROPIC_API_KEY=k\n", stderr="")

        rmr.subprocess.run = fake_run
        try:
            env = rmr.ccmorph_env("gw")
        finally:
            rmr.subprocess.run = saved
        self.assertIn("_route_env", scripts[0],
                      "probe must resolve the route through ccmorph, "
                      "the same seam a launch uses")
        self.assertEqual(env.get("ANTHROPIC_BASE_URL"), "https://gw")

    def test_the_ambient_proxy_reaches_the_opener(self):
        """The invariant is the effect, not the handler.

        urllib drops a ProxyHandler carrying no proxies out of the chain entirely,
        so this cannot be asserted by looking for one. What must hold is that our
        opener carries the ambient HTTPS_PROXY exactly as a default opener does --
        the bridge the operator started is the only route to the endpoint.
        """
        # urllib prefers lowercase variables over uppercase; the host may export
        # either, so clear every proxy variable and restore them afterwards.
        names = ("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
                 "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy")
        saved = {n: os.environ.pop(n) for n in names if n in os.environ}
        os.environ["HTTPS_PROXY"] = "http://ambient.invalid:1"
        try:
            ours = rmr.build_opener()
            default = urllib.request.build_opener()
        finally:
            os.environ.pop("HTTPS_PROXY", None)
            os.environ.update(saved)

        def proxies(opener):
            return [v for h in opener.handlers
                    if isinstance(h, urllib.request.ProxyHandler)
                    for v in h.proxies.values()]
        self.assertIn("http://ambient.invalid:1", proxies(ours),
                      "the caller's proxy was blanked before the request")
        self.assertIn("http://ambient.invalid:1", proxies(default),
                      "control: a default opener is expected to carry it, "
                      "otherwise this test proves nothing")


@whitebox
class FailureIsExplained(unittest.TestCase):
    """Every failure must name the cause and the next action.

    "probed nothing; refusing to overwrite the registry" described the safety
    behaviour and withheld the reason, which the run had already collected in
    `notes`. A reader learned that a file was protected, not that their VPN was
    off.
    """

    def _probe_failing(self, exc):
        import io
        import contextlib
        saved = {k: getattr(rmr, k) for k in ("ccmorph_env", "list_models")}
        rmr.ccmorph_env = lambda profile: {
            "ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gw"}

        def boom(base, key, timeout=None, opener=None):
            raise exc
        rmr.list_models = boom
        try:
            err = io.StringIO()
            with contextlib.redirect_stderr(err):
                values, notes, _ = rmr.probe_all(["gw"], jobs=1)
            return values, notes, err.getvalue()
        finally:
            for k, v in saved.items():
                setattr(rmr, k, v)

    def test_a_timeout_names_the_network(self):
        import socket
        values, notes, _ = self._probe_failing(
            urllib.error.URLError(socket.timeout("timed out")))
        reason = " ".join(r for r, _ in notes)
        self.assertIn("did not answer", reason)
        self.assertIn("check the endpoint and network access", reason,
                      "the reader must be told what to look at")
        self.assertNotIn("URLError", reason,
                         "a python exception class is not an explanation")

    def test_the_empty_result_reports_the_reason_and_what_is_kept(self):
        import io
        import contextlib
        saved = {k: getattr(rmr, k) for k in ("probe_all", "prior_values")}
        rmr.probe_all = lambda *a, **k: ({}, [("did not answer at gw (timed out)",
                                             "gw")], {})
        rmr.prior_values = lambda profiles=None: {"a": 1, "b": 2}
        argv = sys.argv
        sys.argv = ["refresh-model-registry", "--profile", "gw"]
        try:
            err = io.StringIO()
            with contextlib.redirect_stderr(err):
                rc = rmr.main()
            out = err.getvalue()
        finally:
            sys.argv = argv
            for k, v in saved.items():
                setattr(rmr, k, v)
        self.assertEqual(rc, 1)
        self.assertIn("did not answer", out,
                      "the reason was collected and must be printed")
        self.assertIn("gw", out)
        self.assertIn("2", out,
                      "say how many existing values were kept, so 'refusing to "
                      "overwrite' is not the only thing a reader learns")
        self.assertNotIn("refusing to overwrite the registry", out)


@whitebox
class HexWrappedBodies(unittest.TestCase):
    """A ceiling served as hex is still a ceiling.

    One gateway forwards Bedrock's body as `SdkBytes(bytes=0x7b22...)`. Inside
    it is the ordinary "prompt is too long: N tokens > M maximum" the parser already
    understands, so every probe on that route reported `opaque` and probed nothing
    while the answer was in the response the whole time.
    """

    WRAPPED = (
        '{"detailMsg":"bedrock-claude 400 client error SdkBytes(bytes=0x7b226d'
        '657373616765223a2270726f6d707420697320746f6f206c6f6e673a203232373737'
        '393420746f6b656e73203e2031303030303030206d6178696d756d227d)"}'
    )

    def test_the_hex_body_is_decoded(self):
        text = rmr.decode_hex_bodies(self.WRAPPED)
        self.assertIn("prompt is too long: 2277794 tokens > 1000000 maximum", text)

    def test_the_ceiling_is_read_out_of_a_hex_wrapped_body(self):
        detail = rmr.decode_hex_bodies(self.WRAPPED)
        for pat in rmr.DECLARED:
            m = pat.search(detail)
            if m:
                self.assertEqual(int(m.group(1).replace(",", "")), 1_000_000)
                return
        self.fail("no DECLARED pattern matched the decoded body")

    def test_odd_length_and_garbage_blobs_are_left_alone(self):
        # An odd digit count is not valid hex; it must not raise.
        for junk in ("0x7b226", "0xzz", "no hex here", ""):
            self.assertIsInstance(rmr.decode_hex_bodies(junk), str)


@whitebox
class ProbeFailuresAreExplained(unittest.TestCase):
    """A probe's own failures get the same treatment as the listing's.

    `why_unreachable` fixed the listing path while `probe()` still returned
    `repr(exc)` and a 90-char slice, so a real run printed
    "TimeoutError('The read operation timed out')" — the class the code caught, not
    what happened — and cut a wrapped vendor body off exactly where its number
    would have been.
    """

    def _probe_raising(self, exc):
        class Boom:
            def open(self, *a, **k):
                raise exc
        return rmr.probe("https://gw", "k", "m", "blob", 1, Boom())

    def test_a_timeout_says_the_probe_is_oversized_and_what_to_do(self):
        kind, val, note = self._probe_raising(
            TimeoutError("The read operation timed out"))
        self.assertIn("timed out", note)
        self.assertIn("over-limit", note)
        self.assertIn("--probe-timeout", note,
                      "a timeout must name the knob that fixes it")
        self.assertNotIn("TimeoutError(", note,
                         "an exception repr is not an explanation")

    def test_an_unexpected_error_names_itself_without_a_repr(self):
        kind, val, note = self._probe_raising(ValueError("bad json"))
        self.assertEqual(note, "ValueError: bad json")

    def test_an_opaque_body_keeps_enough_to_show_a_missed_number(self):
        """The old 90-char cut discarded the part a reader needs.

        This particular body DOES carry a number the patterns read, so it must be
        probed rather than reported — which is the point: the truncation was
        hiding evidence that the wrapper class was measurable all along.
        """
        body = ('cloudsway 400 client error {"error":{"message":"Anthropic Claude '
                'bad request: [1261][prompt is too long: 2277794 tokens > 204800 '
                'maximum]"}}')

        class Wrapped:
            def open(self, *a, **k):
                raise urllib.error.HTTPError(
                    "u", 400, "Bad Request", {}, io.BytesIO(body.encode()))
        kind, val, note = rmr.probe("https://gw", "k", "m", "blob", 1, Wrapped())
        self.assertTrue(kind.endswith(":declared"), f"got {kind}: {note}")
        self.assertEqual(val, 204_800)


@whitebox
class BisectFindsAnUnstatedCeiling(unittest.TestCase):
    """When the gateway refuses but names no number, find the edge by behaviour.

    Some gateways strip the ceiling out of their own refusal: glm-5.3 on a private
    route answers `[prompt is too long]` with the number removed. The old tool gave
    up there and recorded nothing — but a refusal IS an answer: it says the limit is
    below what was sent. Halve, retry, and the boundary is discoverable without any
    cooperation from the error text.

    Every probe here is a REFUSAL except the final accepted one, and a refusal is
    rejected before inference, so the search is free apart from that last request.
    """

    def _gateway(self, real_limit, *, reports_tokens=True, accept_calls=None):
        """A fake endpoint that refuses anything over `real_limit` tokens."""
        calls = []

        def send(base, key, model, blob, path, hdr, timeout=None, opener=None):
            tokens = len(blob) // 4
            calls.append(tokens)
            if tokens > real_limit:
                return ("refused", None)
            return ("accepted", tokens if reports_tokens else None)

        if accept_calls is not None:
            accept_calls.extend(calls)
        return send, calls

    def test_it_finds_the_boundary_and_reports_the_accepted_size(self):
        send, calls = self._gateway(200_000)
        val, note, spent = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        # The answer must be a size the provider ACTUALLY accepted, never the
        # midpoint of a range we never sent.
        self.assertIsNotNone(val)
        self.assertLessEqual(val, 200_000)
        # And it must be close to the truth, not merely under it: halving from 1.5M
        # to a 200k limit converges within a few percent.
        self.assertGreater(val, 200_000 * 0.9,
                           f"converged too loosely: {val} for a 200,000 limit")

    def test_the_search_is_logarithmic_not_linear(self):
        send, calls = self._gateway(200_000)
        rmr.bisect_ceiling(send, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        self.assertLess(len(calls), 25,
                        f"{len(calls)} requests is not a bisection")

    def test_a_gateway_that_refuses_everything_records_nothing(self):
        send, calls = self._gateway(0)
        val, note, spent = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        self.assertIsNone(val, "no value may be invented when nothing was accepted")
        self.assertIn("floor", note)

    def test_a_provider_reported_count_is_preferred_over_our_estimate(self):
        """The recorded number must be the provider's own, not chars//4."""
        send, calls = self._gateway(200_000, reports_tokens=True)
        val, note, spent = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        self.assertIn("reported", note)

    def test_it_still_answers_when_the_gateway_reports_no_usage(self):
        send, calls = self._gateway(200_000, reports_tokens=False)
        val, note, spent = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        self.assertIsNotNone(val)
        self.assertIn("estimate", note,
                      "an estimated value must say so, so nobody reads it as probed")

    def test_it_counts_what_it_spent(self):
        send, calls = self._gateway(200_000)
        val, note, spent = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        # One accepted request is billed; refusals are not. The caller discloses
        # this, so the number must be real.
        self.assertGreaterEqual(spent, 1)


@whitebox
class BisectSpendsAlmostNothing(unittest.TestCase):
    """The cost property, pinned separately from correctness.

    A refused over-limit request is free: it is rejected before inference, so nothing
    is generated and nothing is billed. An ACCEPTED one is billed for its whole
    payload. So the search must do its work with refusals and accept as few times as
    possible — a symmetric bisection would have converged just as well while buying
    a dozen accepted megabyte payloads, which is the waste these tests forbid.
    """

    def _tracking_gateway(self, real_limit):
        billed_tokens = []

        def send(base, key, model, blob, path, hdr, timeout=None, opener=None):
            tokens = len(blob) // 4
            if tokens > real_limit:
                return ("refused", None)        # free
            billed_tokens.append(tokens)         # billed
            return ("accepted", tokens)
        return send, billed_tokens

    def test_it_bills_at_most_the_accept_budget(self):
        send, billed = self._tracking_gateway(200_000)
        val, note, accepts = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 4_000_000, floor=1_000)
        self.assertLessEqual(len(billed), rmr.BISECT_MAX_ACCEPTS,
                             f"billed {len(billed)} requests: {billed}")
        self.assertEqual(accepts, len(billed), "reported spend must be the real one")

    def test_the_first_billed_request_is_the_cheapest_tried(self):
        """Descent halves while refused, so the first accept is the smallest payload.

        This is what makes the one unavoidable billed request as cheap as it can be.
        """
        send, billed = self._tracking_gateway(200_000)
        rmr.bisect_ceiling(send, ("b", "k", "m", "/p", None), "x" * 4_000_000, floor=1_000)
        self.assertEqual(billed[0], min(billed))
        # And it must be at most the true limit — never a megabyte payload.
        self.assertLessEqual(billed[0], 200_000)

    def test_total_billed_volume_stays_near_one_window(self):
        """Not 'a few requests' but 'a few requests that are each small'."""
        send, billed = self._tracking_gateway(200_000)
        rmr.bisect_ceiling(send, ("b", "k", "m", "/p", None), "x" * 4_000_000, floor=1_000)
        self.assertLess(sum(billed), 200_000 * 3,
                        f"spent {sum(billed):,} tokens across {billed}")

    def test_a_gateway_that_refuses_everything_is_entirely_free(self):
        send, billed = self._tracking_gateway(0)
        val, note, accepts = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 4_000_000, floor=1_000)
        self.assertEqual(billed, [], "a fully-refusing route must cost nothing")
        self.assertEqual(accepts, 0)
        self.assertIsNone(val)

    def test_hitting_the_accept_cap_says_the_answer_understates(self):
        # A gateway with a limit just under the start forces repeated accepts while
        # narrowing; the cap must stop it AND the note must admit the gap.
        send, billed = self._tracking_gateway(999_000)
        val, note, accepts = rmr.bisect_ceiling(
            send, ("b", "k", "m", "/p", None), "x" * 4_000_000, floor=1_000,
            max_accepts=1)
        self.assertEqual(len(billed), 1)
        self.assertIn("understates", note)


@whitebox
class InferenceDisclosesItsCost(unittest.TestCase):
    def test_warning_precedes_first_paid_request(self):
        import contextlib
        err = io.StringIO()
        def send(*args):
            self.assertIn("6,000,000-character", err.getvalue())
            self.assertIn("timeout does not undo billing", err.getvalue())
            return ("anthropic:declared", 200000, "")
        with contextlib.redirect_stderr(err), \
                unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(["m"], {}, [])), \
                unittest.mock.patch.object(rmr, "probe", side_effect=send):
            values, _, _ = rmr.probe_all(["gw"], 1, mode="auto", only_models={"m"})
        self.assertEqual(values, {"gw:m": 200000})


@whitebox
class ATimeoutIsNeverACeiling(unittest.TestCase):
    """A number read off the clock is worse than no number.

    The sender briefly reported a timeout as `refused` so the search could get past a
    slow opening probe. With `--probe-timeout 3` against a real gateway, every request
    timed out, every timeout read as "too long", and glm-5.3 was recorded at 23,051 —
    roughly 40x too low, in a registry that is trusted permanently and that nothing
    downstream can sanity-check.
    """

    def test_a_timeout_records_nothing_and_says_why(self):
        def always_timeout(b, k, m, blob, p, h, timeout=None, opener=None):
            return ("timeout", None)
        val, note, accepts = rmr.bisect_ceiling(
            always_timeout, ("b", "k", "m", "/p", None), "x" * 6_000_000, floor=1_000)
        self.assertIsNone(val, "a clock reading must never become a ceiling")
        self.assertIn("--probe-timeout", note, "it must name the fix")
        self.assertIn("nothing was recorded", note)
        self.assertEqual(accepts, 0)

    def test_a_timeout_after_a_real_accept_keeps_the_probed_value(self):
        """What was paid for and probed is kept; the unconfirmed range is disclosed."""
        state = {"n": 0}

        def accept_then_timeout(b, k, m, blob, p, h, timeout=None, opener=None):
            state["n"] += 1
            tokens = len(blob) // 4
            if state["n"] == 1:
                return ("refused", None)      # free descent works
            if state["n"] == 2:
                return ("accepted", tokens)   # a real probe
            return ("timeout", None)          # then the route goes slow
        val, note, accepts = rmr.bisect_ceiling(
            accept_then_timeout, ("b", "k", "m", "/p", None), "x" * 6_000_000,
            floor=1_000, max_accepts=2)
        self.assertIsNotNone(val, "a paid-for probe must not be thrown away")
        self.assertIn("understates", note,
                      "an unconfirmed range must be disclosed, not implied")


@whitebox
class ProbeErrorBoundaries(unittest.TestCase):
    def test_non_context_errors_and_timeouts_never_start_bisection(self):
        for error in (401, 403, 429, 500, 400, "timeout"):
            calls = []
            class Broken:
                def open(self, req, **kwargs):
                    calls.append(req)
                    if error == "timeout":
                        raise TimeoutError("slow")
                    raise urllib.error.HTTPError(req.full_url, error, "failed", {},
                                                 io.BytesIO(b'{"error":{"message":"unavailable"}}'))
            with self.subTest(error=error):
                kind, value, note = rmr.probe("https://gw", "k", "m", "x" * 16000, 1, Broken(), bisect=1)
                self.assertEqual(len(calls), 1)
                self.assertIsNone(value)


@whitebox
class OneSenderOnePolicy(unittest.TestCase):
    """The timeout policy must have a producer, not just a consumer.

    `bisect_ceiling` checks for a `"timeout"` outcome, and for a while nothing could
    ever return one: a bulk edit rewrote the sender's `("timeout", None)` into
    `("refused", None)`, restoring the exact defect the check exists to prevent (a
    ceiling read off the clock — glm-5.3 recorded at 23,051 under --probe-timeout 3)
    while every test still passed, because the tests faked the sender.
    """

    def test_the_real_sender_reports_a_timeout_as_a_timeout(self):
        class Slow:
            def open(self, *a, **k):
                raise TimeoutError("The read operation timed out")

        captured = {}
        real_bisect = rmr.bisect_ceiling

        def spy(send, request, high, **kw):
            captured["outcome"] = send(*request[:3], "blob", request[3], request[4])
            return (None, "stopped", 0)
        rmr.bisect_ceiling = spy
        try:
            with unittest.mock.patch.object(rmr, "post_sized", side_effect=[
                    urllib.error.HTTPError("u", 400, "too long", {}, io.BytesIO(b"prompt is too long")),
                    TimeoutError("slow")]):
                rmr.probe("https://gw", "k", "m", "blob", 1, Slow(), bisect=1)
        finally:
            rmr.bisect_ceiling = real_bisect
        self.assertEqual(captured.get("outcome"), ("timeout", None),
                         "a timed-out bisection step must not read as 'refused'")

    def test_is_timeout_recognises_the_wrapped_forms(self):
        self.assertTrue(rmr.is_timeout(TimeoutError("x")))
        self.assertTrue(rmr.is_timeout(urllib.error.URLError(TimeoutError("x"))))
        self.assertFalse(rmr.is_timeout(ValueError("x")))

    def test_one_request_builder(self):
        """post_sized is the only place the sized POST is constructed."""
        seen = {}

        class Spy:
            def open(self, req, timeout=None):
                seen["url"] = req.full_url
                seen["body"] = json.loads(req.data)

                class R:
                    def __enter__(s): return s
                    def __exit__(s, *a): return False
                    def read(s): return b'{"usage":{"input_tokens":7}}'
                return R()
        doc = rmr.post_sized("https://gw/", "k", "m", "blob", "/v1/messages",
                             lambda k: {"x-api-key": k}, 5, Spy())
        self.assertEqual(seen["url"], "https://gw/v1/messages")
        self.assertEqual(seen["body"]["max_tokens"], 8)
        self.assertEqual(doc["usage"]["input_tokens"], 7)


@whitebox
class CatalogueFallback(unittest.TestCase):
    """The offline models.dev catalogue fills a gap ONLY the gateway itself left.

    Exact provider+id match wins; a bare id unique across the whole catalogue is
    accepted; a bare id shared by 2+ providers is ambiguous and skipped, never
    maxed or guessed. limit.input, when lower than limit.context, is the real
    operational budget (an input sub-limit can sit below a combined context that
    counts output too) -- never silently dropped in favour of the larger number.
    """

    CATALOGUE = {
        "anthropic": {"models": {
            "claude-x": {"limit": {"context": 400000}},
        }},
        "acme": {"models": {
            "acme/solo": {"limit": {"context": 128000, "input": 100000}},
            "shared-name": {"limit": {"context": 50000}},
            "input-only": {"limit": {"input": 30000}},
            "no-limit-field": {"name": "nope"},
        }},
        "other-co": {"models": {
            "shared-name": {"limit": {"context": 999999}},
        }},
    }

    def test_exact_provider_and_id_match(self):
        found = rmr.catalogue_window(self.CATALOGUE, "claude-x", "anthropic")
        self.assertEqual(found["value"], 400000)
        self.assertEqual(found["source"], "catalogue")

    def test_input_lower_than_context_is_the_operational_value_but_both_kept(self):
        found = rmr.catalogue_window(self.CATALOGUE, "acme/solo", "acme")
        self.assertEqual(found["value"], 100000)
        self.assertEqual(found["context"], 128000)
        self.assertEqual(found["input"], 100000)

    def test_bare_id_unique_across_catalogue_is_accepted(self):
        found = rmr.catalogue_window(self.CATALOGUE, "acme/solo", owned_by=None)
        self.assertEqual(found["value"], 100000)

    def test_bare_id_ambiguous_across_providers_is_skipped(self):
        self.assertIsNone(rmr.catalogue_window(self.CATALOGUE, "shared-name", owned_by=None))
        # A wrong/unrelated owned_by must not force a false exact match either.
        self.assertIsNone(rmr.catalogue_window(self.CATALOGUE, "shared-name", "wrong-co"))

    def test_input_only_entry_without_context_is_still_usable(self):
        found = rmr.catalogue_window(self.CATALOGUE, "input-only", "acme")
        self.assertEqual(found["value"], 30000)
        self.assertIsNone(found["context"])

    def test_entry_with_no_numeric_limit_is_absent(self):
        self.assertIsNone(rmr.catalogue_window(self.CATALOGUE, "no-limit-field", "acme"))

    def test_unknown_model_is_absent(self):
        self.assertIsNone(rmr.catalogue_window(self.CATALOGUE, "nope", "acme"))

    def test_fetch_failure_never_raises(self):
        with unittest.mock.patch.object(rmr.urllib.request, "urlopen", side_effect=OSError("no route")):
            self.assertEqual(rmr.fetch_catalogue(), {})

    def test_declared_only_run_fills_only_the_gap_and_tags_provenance(self):
        # "a" is declared by the gateway itself and must win outright; "b" has no
        # gateway metadata and is filled from the catalogue, tagged as such.
        with unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(
                    ["claude-x", "acme/solo"], {"claude-x": 128000},
                    [{"id": "claude-x", "owned_by": "x"}, {"id": "acme/solo", "owned_by": "acme"}])), \
                unittest.mock.patch.object(rmr, "fetch_catalogue", return_value=self.CATALOGUE):
            values, notes, sources = rmr.probe_all(["gw"], 1, mode="declared-only")
        self.assertEqual(values, {"gw:claude-x": 128000, "gw:acme/solo": 100000})
        self.assertNotIn("gw:claude-x", sources)
        self.assertEqual(sources["gw:acme/solo"]["source"], "catalogue")

    def test_catalogue_fetched_at_most_once_per_run(self):
        rows = [{"id": "a", "owned_by": "acme"}, {"id": "b", "owned_by": "other-co"}]
        with unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                unittest.mock.patch.object(rmr, "list_models", return_value=(["a", "b"], {}, rows)), \
                unittest.mock.patch.object(rmr, "fetch_catalogue",
                                          return_value={"acme": {"models": {"a": {"limit": {"context": 1}}}},
                                                        "other-co": {"models": {"b": {"limit": {"context": 2}}}}}) as fetch:
            rmr.probe_all(["gw"], 1, mode="declared-only")
        fetch.assert_called_once()

    def test_gateway_now_declaring_a_value_supersedes_a_prior_catalogue_guess(self):
        # A catalogue-derived baseline is not verified: this run's real declared
        # value must replace it, unlike a genuinely probed/declared prior value
        # which stays pinned.
        with unittest.mock.patch.dict(os.environ), tempfile.TemporaryDirectory() as tmp:
            os.environ["CCMORPH_STATE_DIR"] = tmp
            conf = Path(tmp, "gw.conf")
            conf.write_text("type=apikey\napi_key=canary\n")
            identity = rmr.store.profile_identity("gw")
            rmr.store.save_discovered("gw", {"m": 50000}, identity,
                                      sources={"m": {"source": "catalogue", "value": 50000,
                                                     "input": None, "context": 50000, "fetched": "2020-01-01"}})
            with unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                    unittest.mock.patch.object(rmr, "list_models", return_value=(
                        ["m"], {"m": 900000}, [{"id": "m", "owned_by": "acme"}])):
                values, _, _ = rmr.probe_all(["gw"], 1, mode="declared-only")
            with unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                    unittest.mock.patch.object(rmr, "list_models", return_value=(
                        ["m"], {"m": 50000}, [{"id": "m", "owned_by": "acme"}])):
                same_values, _, same_sources = rmr.probe_all(["gw"], 1, mode="declared-only")
                self.assertEqual(same_values, {"gw:m": 50000})
                self.assertNotIn("gw:m", same_sources)
        self.assertEqual(values, {"gw:m": 900000})

    def test_a_second_run_that_still_has_no_declared_size_keeps_the_catalogue_tag(self):
        # Regression: carrying a catalogue value forward unchanged must not look
        # like a fresh, unsourced (therefore "verified") write on the next run --
        # that would silently promote an unverified guess to trusted after just one
        # more refresh that changed nothing about it.
        with unittest.mock.patch.dict(os.environ), tempfile.TemporaryDirectory() as tmp:
            os.environ["CCMORPH_STATE_DIR"] = tmp
            conf = Path(tmp, "gw.conf")
            conf.write_text("type=apikey\napi_key=canary\n")
            identity = rmr.store.profile_identity("gw")
            rmr.store.save_discovered("gw", {"acme/solo": 100000}, identity,
                                      sources={"acme/solo": {"source": "catalogue", "value": 100000,
                                                             "input": 100000, "context": 128000,
                                                             "fetched": "2020-01-01"}})
            env_patch = unittest.mock.patch.object(rmr, "ccmorph_env",
                                                   return_value={"ANTHROPIC_API_KEY": "k"})
            listing_patch = unittest.mock.patch.object(rmr, "list_models", return_value=(
                ["acme/solo"], {}, [{"id": "acme/solo", "owned_by": "acme"}]))
            # Still no gateway metadata this run either -- the model should be
            # carried forward from the stored baseline, not refetched.
            with env_patch, listing_patch, \
                    unittest.mock.patch.object(rmr, "fetch_catalogue", side_effect=AssertionError("should not refetch")):
                values, _, sources = rmr.probe_all(["gw"], 1, mode="declared-only")
            self.assertEqual(values, {"gw:acme/solo": 100000})
            self.assertEqual(sources["gw:acme/solo"]["source"], "catalogue")
            rmr.commit(values, [], identities={"gw": rmr.store.profile_identity("gw")}, sources=sources)
            _, fields = rmr.store.read_conf(conf)
            self.assertEqual(rmr.store.discovered_sources(fields)["acme/solo"]["source"], "catalogue")


@whitebox
class CanonicalCatalogueFallback(unittest.TestCase):
    """The flat, provider-independent models.json catalogue resolves what the
    per-provider api.json catalogue can't: a model resold under many gateway
    listings (owned_by naming the wrapper, not the lab) is ambiguous there but has
    exactly one canonical "provider/model-id" leaf here.
    """

    CANONICAL = {
        "swiss-ai/apertus-8b": {"limit": {"context": 65536}},
        "acme/solo": {"limit": {"context": 128000, "input": 100000}},
        "acme/shared-name": {"limit": {"context": 1}},
        "other/shared-name": {"limit": {"context": 2}},
        "acme/widget-v2": {"family": "widget", "limit": {"context": 300000}},
        "acme/widget-v3": {"family": "widget", "limit": {"context": 300000}},
        "acme/conflict-v2": {"family": "conflict", "limit": {"context": 100000}},
        "acme/conflict-v3": {"family": "conflict", "limit": {"context": 200000}},
        "acme/partial-v1": {"family": "partial", "limit": {}},
        "acme/partial-v2": {"family": "partial", "limit": {"context": 300000}},
    }

    def test_exact_canonical_key_match(self):
        found = rmr.canonical_catalogue_window(self.CANONICAL, "apertus-8b", "swiss-ai")
        self.assertEqual(found["value"], 65536)
        self.assertEqual(found["source"], "catalogue")

    def test_bare_leaf_unique_across_catalogue_is_accepted_without_owned_by(self):
        found = rmr.canonical_catalogue_window(self.CANONICAL, "apertus-8b", owned_by=None)
        self.assertEqual(found["value"], 65536)

    def test_wrong_owned_by_falls_back_to_unique_bare_leaf(self):
        # owned_by names a reselling wrapper, not "swiss-ai" -- must not block the match.
        found = rmr.canonical_catalogue_window(self.CANONICAL, "apertus-8b", "cloudsway")
        self.assertEqual(found["value"], 65536)

    def test_bare_leaf_ambiguous_across_canonical_entries_is_skipped(self):
        self.assertIsNone(rmr.canonical_catalogue_window(self.CANONICAL, "shared-name", owned_by=None))
        self.assertIsNone(rmr.canonical_catalogue_window(self.CANONICAL, "shared-name", "wrong-co"))

    def test_probe_urls_match_each_protocols_serving_client(self):
        base = 'https://gateway/api/v4'
        self.assertEqual(rmr.api_url(base, '/v1/messages'), base + '/v1/messages')
        self.assertEqual(rmr.api_url(base, '/v1/chat/completions'), base + '/chat/completions')

    def test_family_cannot_override_ambiguous_exact_model_ids(self):
        catalogue = {**self.CANONICAL,
                     'acme/shared-v2': {'family': 'shared-name', 'limit': {'context': 1000000}}}
        self.assertIsNone(rmr.canonical_catalogue_window(catalogue, 'shared-name', None))
        self.assertIsNone(rmr.canonical_catalogue_window(catalogue, 'shared-name', 'wrong-co'))
        self.assertEqual(rmr.canonical_catalogue_window(catalogue, 'shared-name', 'acme')['value'], 1)

    def test_family_alias_is_accepted_when_every_match_has_one_window(self):
        found = rmr.canonical_catalogue_window(self.CANONICAL, "widget", "acme")
        self.assertEqual(found["value"], 300000)

    def test_family_alias_with_conflicting_windows_is_skipped(self):
        self.assertIsNone(rmr.canonical_catalogue_window(self.CANONICAL, "conflict", "acme"))

    def test_family_alias_ignores_variants_without_a_published_window(self):
        found = rmr.canonical_catalogue_window(self.CANONICAL, "partial", "acme")
        self.assertEqual(found["value"], 300000)

    def test_unknown_model_is_absent(self):
        self.assertIsNone(rmr.canonical_catalogue_window(self.CANONICAL, "nope", "acme"))

    def test_fetch_failure_never_raises(self):
        with unittest.mock.patch.object(rmr.urllib.request, "urlopen", side_effect=OSError("no route")):
            catalogue = rmr.fetch_canonical_catalogue()
            self.assertTrue(rmr.canonical_catalogue_window(catalogue, "gpt-5", None))

    def test_used_only_when_the_provider_catalogue_missed(self):
        # api.json has this model ambiguously spread across two providers (a
        # reselling-gateway shape); models.json has exactly one canonical leaf.
        provider_catalogue = {
            "resell-a": {"models": {"widget": {"limit": {"context": 111}}}},
            "resell-b": {"models": {"widget": {"limit": {"context": 222}}}},
        }
        rows = [{"id": "widget", "owned_by": "cloudsway"}]
        with unittest.mock.patch.dict(os.environ), tempfile.TemporaryDirectory() as tmp:
            os.environ["CCMORPH_STATE_DIR"] = tmp
            Path(tmp, "gw.conf").write_text("type=apikey\napi_key=canary\n")
            with unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                    unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                    unittest.mock.patch.object(rmr, "list_models", return_value=(["widget"], {}, rows)), \
                    unittest.mock.patch.object(rmr, "fetch_catalogue", return_value=provider_catalogue), \
                    unittest.mock.patch.object(rmr, "fetch_canonical_catalogue",
                                              return_value={"real-lab/widget": {"limit": {"context": 333}}}) as canon:
                values, _, sources = rmr.probe_all(["gw"], 1, mode="declared-only")
        self.assertEqual(values, {"gw:widget": 333})
        self.assertEqual(sources["gw:widget"]["source"], "catalogue")
        canon.assert_called_once()

    def test_canonical_catalogue_not_fetched_when_provider_catalogue_already_matched(self):
        rows = [{"id": "claude-x", "owned_by": "anthropic"}]
        provider_catalogue = {"anthropic": {"models": {"claude-x": {"limit": {"context": 400000}}}}}
        with unittest.mock.patch.dict(os.environ), tempfile.TemporaryDirectory() as tmp:
            os.environ["CCMORPH_STATE_DIR"] = tmp
            Path(tmp, "gw.conf").write_text("type=apikey\napi_key=canary\n")
            with unittest.mock.patch.object(rmr, "prior_values", return_value={}), \
                    unittest.mock.patch.object(rmr, "ccmorph_env", return_value={"ANTHROPIC_API_KEY": "k"}), \
                    unittest.mock.patch.object(rmr, "list_models", return_value=(["claude-x"], {}, rows)), \
                    unittest.mock.patch.object(rmr, "fetch_catalogue", return_value=provider_catalogue), \
                    unittest.mock.patch.object(rmr, "fetch_canonical_catalogue",
                                              side_effect=AssertionError("should not fetch")):
                values, _, _ = rmr.probe_all(["gw"], 1, mode="declared-only")
        self.assertEqual(values, {"gw:claude-x": 400000})
