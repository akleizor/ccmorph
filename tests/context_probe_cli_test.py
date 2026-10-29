"""context-probe as a black box: argv, env, stdin, files, stdout, stderr, exit code.

Runs against whichever implementation CCMORPH_TEST_HELPERS selects (the Python
script, or `ccmorph-core context-probe`), so the two are held to the
same contract. Every gateway here is a local fake; no test reaches models.dev
(an unreachable proxy makes the catalogue fetches fail at once).
"""
import json
import os
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

from support import FAKE_SERVER_NO_DNS, HANG_TIMEOUT, helper_cmd  # noqa: F401


def serve(handler_cls):
    server = HTTPServer(("127.0.0.1", 0), handler_cls)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


class Fixture:
    def __init__(self, tmp_path, listing, post=None):
        self.tmp = tmp_path
        self.requests = []
        fx = self

        class Gateway(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                fx.requests.append((self.command, self.path))
                body = json.dumps({"data": fx.listing}).encode()
                self.send_response(200)
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                size = int(self.headers.get("content-length") or 0)
                data = self.rfile.read(size)
                fx.requests.append((self.command, self.path, len(data)))
                status, doc = (fx.post or (lambda path: (500, {})))(self.path)
                body = json.dumps(doc).encode()
                self.send_response(status)
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.listing = listing
        self.post = post
        self.server = serve(Gateway)
        self.base = f"http://127.0.0.1:{self.server.server_port}"
        self.state = tmp_path / "state"
        self.state.mkdir()
        self.lib = tmp_path / "lib"
        self.lib.mkdir()
        (self.lib / "ccmorph.sh").write_text(
            'ccmorph() { printf "%s\\n" "ANTHROPIC_API_KEY=canary" '
            f'"ANTHROPIC_BASE_URL={self.base}"; }}\n')
        self.conf = self.state / "gw.conf"
        self.conf.write_text("type=apikey\napi_key=canary\n")
        self.env = {k: v for k, v in os.environ.items() if not k.upper().endswith("_PROXY")}
        self.env.update(CCMORPH_LIB_DIR=str(self.lib), CCMORPH_STATE_DIR=str(self.state), HOME=str(tmp_path),
                        HTTPS_PROXY="http://127.0.0.1:9")  # models.dev is unreachable, quickly

    def run(self, *args, stdin=None, env=None):
        return subprocess.run([*helper_cmd("context-probe"), *args], input=stdin,
                              env={**self.env, **(env or {})}, capture_output=True, text=True,
                              timeout=HANG_TIMEOUT)

    def stored(self):
        for line in self.conf.read_text().splitlines():
            if line.startswith("context_discovered="):
                return json.loads(line.partition("=")[2])
        return None

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def gateway(tmp_path):
    made = []

    def make(listing, post=None):
        fx = Fixture(tmp_path, listing, post)
        made.append(fx)
        return fx
    yield make
    for fx in made:
        fx.close()


def test_default_run_writes_declared_limits_and_says_what_changed(gateway):
    fx = gateway([{"id": "a", "max_input_tokens": 128000}, {"id": "b", "context_length": 64000}])
    result = fx.run("--profile", "gw")
    assert result.returncode == 0, result.stderr
    assert result.stdout == ""
    assert result.stderr == ("gw: 2 limit(s) updated, applies to new sessions.\n"
                             "  a                                  128,000\n"
                             "  b                                   64,000\n")
    stored = fx.stored()
    assert stored["models"] == {"a": 128000, "b": 64000} and "route" in stored
    assert fx.requests == [("GET", "/v1/models")]
    again = fx.run("--profile", "gw")
    assert again.returncode == 0 and again.stderr == "gw: 2 model(s) sized, nothing changed.\n"


def test_check_reports_drift_without_writing(gateway):
    fx = gateway([{"id": "a", "max_input_tokens": 128000}, {"id": "b", "max_input_tokens": 64000}])
    assert fx.run("--profile", "gw").returncode == 0
    before = fx.conf.read_text()
    fx.listing[:] = [{"id": "a", "max_input_tokens": 256000}, {"id": "c", "max_input_tokens": 32000}]
    result = fx.run("--profile", "gw", "--check")
    assert result.returncode == 2
    assert result.stdout == "DRIFT gw:c: None -> 32000\n"
    assert "  a: kept saved limit 128,000; gateway now declares 256,000\n" in result.stderr
    assert fx.conf.read_text() == before


def test_a_saved_limit_is_kept_when_the_gateway_changes_its_mind(gateway):
    fx = gateway([{"id": "a", "max_input_tokens": 128000}])
    fx.run("--profile", "gw")
    fx.listing[:] = [{"id": "a", "max_input_tokens": 999000}]
    result = fx.run("--profile", "gw")
    assert result.returncode == 0
    assert fx.stored()["models"] == {"a": 128000}


def test_a_manual_pin_is_not_probed_or_overwritten(gateway):
    fx = gateway([{"id": "a", "max_input_tokens": 128000}, {"id": "b", "max_input_tokens": 64000}])
    fx.conf.write_text("type=apikey\napi_key=canary\ncontext_window.a=777000\n")
    result = fx.run("--profile", "gw")
    assert result.returncode == 0, result.stderr
    assert fx.stored()["models"] == {"b": 64000}


def test_a_missing_model_suggests_the_real_id_and_the_free_fix(gateway):
    fx = gateway([{"id": "claude-opus-5-5", "max_input_tokens": 1}, {"id": "gpt-5"}])
    result = fx.run("--profile", "gw", "--model", "opus-5-5")
    assert result.returncode == 1
    assert "Nothing discovered.\n" in result.stderr
    assert "'opus-5-5' is not on this route; did you mean claude-opus-5-5?" in result.stderr
    assert "Set it:" not in result.stderr  # nothing unresolved: the model was never on the route
    assert fx.stored() is None


def test_an_unsized_model_names_the_remedies_and_exits_one(gateway):
    fx = gateway([{"id": "a", "max_input_tokens": 128000}, {"id": "mystery"}])
    result = fx.run("--profile", "gw")
    assert result.returncode == 1
    assert fx.stored()["models"] == {"a": 128000}
    assert ("\n1 unresolved, running at Claude Code's 200,000-token default:\n  mystery\n"
            "Set it:      ccmorph context gw mystery <tokens>\n"
            "Measure it:  ccmorph probe gw --model mystery --allow-inference"
            "   (one large prompt, may be billed)\n") in result.stderr
    # --check says so too, and still names the fix.
    checked = fx.run("--profile", "gw", "--check")
    assert checked.returncode == 1 and checked.stdout == ""
    assert "Set it:      ccmorph context gw mystery <tokens>" in checked.stderr


def test_excluded_models_are_not_reported_unresolved(gateway):
    fx = gateway([{"id": "kept", "max_input_tokens": 5}, {"id": "hidden"}])
    fx.conf.write_text("type=apikey\napi_key=canary\nexcluded_models=hidden\n")
    result = fx.run("--profile", "gw")
    assert result.returncode == 0 and "hidden" not in result.stderr


def test_a_gateway_that_is_down_is_explained(gateway):
    import socket
    fx = gateway([])
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    (fx.lib / "ccmorph.sh").write_text(
        'ccmorph() { printf "%s\\n" "ANTHROPIC_API_KEY=k" "ANTHROPIC_BASE_URL=http://127.0.0.1:' + str(port) + '"; }\n')
    result = fx.run("--profile", "gw")
    assert result.returncode == 1
    assert "Nothing discovered." in result.stderr
    assert f"did not answer at 127.0.0.1:{port} (" in result.stderr
    assert "check the endpoint and network access" in result.stderr


def test_paid_probe_reads_the_ceiling_from_the_refusal_and_warns_first(gateway):
    def refuse(path):
        return 400, {"error": {"message": "prompt is too long: 2000000 tokens > 500000 maximum"}}
    fx = gateway([{"id": "m"}], post=refuse)
    result = fx.run("--profile", "gw", "--model", "m", "--allow-inference", "--probe-timeout", "60", "--deadline", "120")
    assert result.returncode == 0, result.stderr
    assert fx.stored()["models"] == {"m": 500000}
    posts = [r for r in fx.requests if r[0] == "POST"]
    assert len(posts) == 1 and posts[0][1] == "/v1/messages" and posts[0][2] > 5_900_000
    assert result.stderr.startswith("[gw] m: paid probe enabled. The first 6,000,000-character prompt may be billed in full.\n"
                                    "  Confirmation and search requests add cost; rates and tokenization "
                                    "depend on the gateway. A timeout does not undo billing.\n")
    assert "  m                              anthropic:declared 500000\n" in result.stderr


@pytest.mark.parametrize("args,needle", [
    (["--profile", "p", "--jobs", "0"], "--jobs"),
    (["--profile", "p", "--bisect"], "--allow-inference"),
    (["--profile", "p", "--probe-all"], "--allow-inference"),
    (["--profile", "p", "--allow-inference"], "exactly one --profile and one --model"),
    (["--profile", "p", "--model", "a", "--model", "b", "--allow-inference"], "exactly one --profile and one --model"),
    (["--declared-only", "--probe-all"], "not allowed with argument"),
    (["--deadline", "nan"], "--deadline"),
    (["--listing-stdin"], "requires exactly one --profile"),
])
def test_argument_errors_exit_two_and_never_reach_a_route(gateway, args, needle):
    fx = gateway([])
    result = fx.run(*args)
    assert result.returncode == 2 and needle in result.stderr
    assert "Traceback" not in result.stderr and fx.requests == []


def test_listing_stdin_sizes_only_what_the_listing_declares(gateway):
    fx = gateway([])
    fx.conf.write_text("type=apikey\napi_key=canary\nexcluded_models=hidden\n")
    listing = json.dumps({"data": [{"id": "new", "max_input_tokens": 500000},
                                   {"id": "hidden", "max_input_tokens": 1},
                                   {"id": "embed", "capabilities": {"chat_completion": False}}]})
    result = fx.run("--profile", "gw", "--listing-stdin", stdin=listing, env={"ANTHROPIC_BASE_URL": "", "ANTHROPIC_API_KEY": ""})
    assert result.returncode == 0 and result.stdout == ""
    assert result.stderr == "sized gw:new 500000\n"
    assert fx.stored()["models"] == {"new": 500000}
    again = fx.run("--profile", "gw", "--listing-stdin", stdin=listing, env={"ANTHROPIC_BASE_URL": "", "ANTHROPIC_API_KEY": ""})
    assert again.returncode == 0 and again.stderr == ""


def test_listing_stdin_remembers_a_miss_and_asks_the_gateway_only_once(gateway):
    fx = gateway([], post=lambda path: (400, {"error": {"message": "max_model_len is 98304"}}) if path == "/v1/messages" else (404, {}))
    listing = json.dumps({"data": [{"id": "stated"}]})
    env = {"ANTHROPIC_BASE_URL": fx.base, "ANTHROPIC_API_KEY": "canary"}
    result = fx.run("--profile", "gw", "--listing-stdin", stdin=listing, env=env)
    assert result.returncode == 0 and result.stderr == "sized gw:stated 98304\n"
    assert fx.stored()["models"] == {"stated": 98304}
    assert sum(1 for r in fx.requests if r[0] == "POST") == 1


def test_listing_stdin_garbage_is_ignored(gateway):
    fx = gateway([])
    for body in ("not json", "[]", '{"data": 5}'):
        result = fx.run("--profile", "gw", "--listing-stdin", stdin=body)
        assert result.returncode == 0 and result.stderr == "", body


def test_a_cross_origin_redirect_never_carries_the_key(tmp_path, gateway):
    seen = []

    class Elsewhere(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            seen.append(dict(self.headers))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"data": [{"id": "stolen", "max_input_tokens": 1}]}')

    other = serve(Elsewhere)
    try:
        fx = gateway([])
        fx.base_redirect = f"http://127.0.0.1:{other.server_port}/v1/models"

        class Redirect(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(302)
                self.send_header("location", fx.base_redirect)
                self.end_headers()

        bounce = serve(Redirect)
        (fx.lib / "ccmorph.sh").write_text(
            'ccmorph() { printf "%s\\n" "ANTHROPIC_API_KEY=sk-REDIRECT-CANARY" '
            f'"ANTHROPIC_BASE_URL=http://127.0.0.1:{bounce.server_port}"; }}\n')
        result = fx.run("--profile", "gw")
        bounce.shutdown()
        assert result.returncode == 1 and "Nothing discovered." in result.stderr
        assert "cross-origin redirect refused" in result.stderr
        assert [h for h in seen if "sk-REDIRECT-CANARY" in str(h)] == [] and seen == []
        assert fx.stored() is None
    finally:
        other.shutdown()


def test_the_ambient_proxy_is_used_not_blanked(tmp_path, gateway):
    seen = []

    class Proxy(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            seen.append(self.path)
            body = b'{"data": [{"id": "via-proxy", "max_input_tokens": 4242}]}'
            self.send_response(200)
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    proxy = serve(Proxy)
    try:
        fx = gateway([])
        (fx.lib / "ccmorph.sh").write_text(
            'ccmorph() { printf "%s\\n" "ANTHROPIC_API_KEY=k" "ANTHROPIC_BASE_URL=http://gw.invalid:81"; }\n')
        result = fx.run("--profile", "gw", env={"HTTP_PROXY": f"http://127.0.0.1:{proxy.server_port}"})
        assert result.returncode == 0, result.stderr
        assert seen == ["http://gw.invalid:81/v1/models"]
        assert fx.stored()["models"] == {"via-proxy": 4242}
    finally:
        proxy.shutdown()


def test_a_route_that_fails_is_a_note_not_a_crash(gateway):
    fx = gateway([])
    (fx.lib / "ccmorph.sh").write_text('ccmorph() { echo "ccmorph: profile gw has no endpoint" >&2; return 1; }\n')
    result = fx.run("--profile", "gw")
    assert result.returncode == 1
    assert "Nothing discovered.\n  profile gw has no endpoint\n" in result.stderr


def test_a_route_without_a_key_is_skipped(gateway):
    fx = gateway([])
    (fx.lib / "ccmorph.sh").write_text('ccmorph() { echo "ANTHROPIC_BASE_URL=http://x"; }\n')
    result = fx.run("--profile", "gw")
    assert result.returncode == 1 and "profile has no api key, skipped" in result.stderr


def test_every_routed_profile_is_swept_when_none_is_named(gateway):
    fx = gateway([{"id": "m", "max_input_tokens": 9000}])
    (fx.state / "native.conf").write_text("type=sub\n")
    (fx.state / "other.conf").write_text("type=proxy\napi_key=k\n")
    result = fx.run()
    assert result.returncode == 0, result.stderr
    assert "gw: 1 limit(s) updated" in result.stderr and "other: 1 limit(s) updated" in result.stderr
    assert "native" not in result.stderr
