"""The addon, loaded by a real mitmdump, must fail closed and keep the key off disk.

Skipped when mitmdump is not installed. Uses a dead loopback port as the route, so
no network or credential is involved.
"""
import os
import shutil
import socket
import subprocess
import urllib.request
from pathlib import Path

import pytest

from support import HANG_TIMEOUT, load_script, needs_loopback, port_outside_the_ephemeral_pool, wait_until

ADDON = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "rc_addon.py"
MITMDUMP = shutil.which("mitmdump")


@needs_loopback  # the proxy and upstream are real listeners: #611
@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_dead_route_fails_closed_and_key_file_is_consumed(tmp_path):
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    key_file.chmod(0o600)
    port = port_outside_the_ephemeral_pool()
    dead = port_outside_the_ephemeral_pool()
    while dead == port:                      # two draws of one worker's slice
        dead = port_outside_the_ephemeral_pool()   # can meet; the route must stay dead
    env = dict(os.environ, RC_UPSTREAM=f"http://127.0.0.1:{dead}", RC_KEY_FILE=str(key_file),
               RC_PARENT_PID=str(os.getpid()))
    proxy = subprocess.Popen(
        [MITMDUMP, "-q", "--listen-host", "127.0.0.1", "--listen-port", str(port),
         "--set", f"confdir={tmp_path / 'mitm'}",
         "--allow-hosts", r"^api\.anthropic\.com(:443)?$", "-s", str(ADDON)],
        env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        # A listening socket alone does not mean mitmproxy has loaded the addon.
        def loaded():
            assert proxy.poll() is None, "proxy exited before loading the addon"
            with socket.socket() as s:
                return not key_file.exists() and s.connect_ex(("127.0.0.1", port)) == 0
        assert wait_until(loaded, HANG_TIMEOUT), (
            f"proxy did not load the addon and listen within {HANG_TIMEOUT:.0f} seconds")
        assert not key_file.exists(), "addon must delete the key file once read"

        # Plain-HTTP absolute-form request through the proxy: exercises the same
        # hook without needing TLS trust in the test process.
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": f"http://127.0.0.1:{port}"}))
        req = urllib.request.Request("http://api.anthropic.com/v1/messages",
                                     data=b"{}", method="POST",
                                     headers={"authorization": "Bearer ACCOUNT-SECRET"})
        with pytest.raises(urllib.error.HTTPError) as err:
            opener.open(req, timeout=HANG_TIMEOUT)
        assert err.value.code == 502, "a dead route must be an error, never a fallback"
        assert b"anthropic-organization-id" not in bytes(str(err.value.headers), "utf-8")
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)


def _launcher():
    return load_script(ADDON.parent / "ccmorph-rc", "ccmorph_rc_fc")


@needs_loopback  # the proxy and upstream are real listeners: #611
@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_launcher_proxy_never_falls_back_to_anthropic(tmp_path):
    """The launcher's own proxy: a routed call works, edits to the source addon
    never reach the running proxy, and a proxy that lost its addon answers 502
    instead of forwarding inference to api.anthropic.com."""
    import http.server
    import threading

    class Route(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers.get("content-length") or 0))
            self.send_response(200)
            self.send_header("content-length", "2")
            self.end_headers()
            self.wfile.write(b"ok")

    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Route)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    rc = _launcher()
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    script = rc.private_addon(tmp_path)
    assert script.parent != ADDON.parent and script.read_bytes() == ADDON.read_bytes()
    port = port_outside_the_ephemeral_pool()
    env = dict(os.environ, RC_UPSTREAM=f"http://127.0.0.1:{upstream.server_address[1]}",
               RC_KEY_FILE=str(key_file), RC_PARENT_PID=str(os.getpid()))
    proxy = subprocess.Popen(rc.proxy_command(MITMDUMP, port, tmp_path / "mitm", script),
                             env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": f"http://127.0.0.1:{port}"}))

    def post():
        req = urllib.request.Request("http://api.anthropic.com/v1/messages", data=b"{}",
                                     method="POST", headers={"authorization": "Bearer ACCOUNT"})
        try:
            with opener.open(req, timeout=HANG_TIMEOUT) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code

    try:
        def listening():
            with socket.socket() as s:
                return s.connect_ex(("127.0.0.1", port)) == 0
        assert wait_until(listening, HANG_TIMEOUT), "the proxy never listened"
        assert post() == 200, "a rerouted call must not match the fail-closed guard"
        # A broken addon, as a failed hot reload leaves it: the proxy stays up with
        # no addon, and the guard alone must stop the call.
        script.write_text("raise RuntimeError('reload failed')\n")

        def failing_closed():
            assert proxy.poll() is None, "the proxy exited instead of failing closed"
            return post() == 502
        assert wait_until(failing_closed, HANG_TIMEOUT), (
            "without the addon, inference must fail, never reach Anthropic")
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)


@needs_loopback  # the proxy and upstream are real listeners: #611
@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_the_account_word_passes_the_fail_closed_guard(tmp_path):
    """GH-594: `--switch account` leaves the call addressed to api.anthropic.com,
    the very destination the fail-closed block_list matches. The addon marks the flow
    it handed over and the guard exempts that mark; with the addon gone, neither
    word's call may reach the upstream."""
    import http.server
    import threading

    seen = []

    class Route(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            seen.append((self.path, self.headers.get("authorization")))
            self.rfile.read(int(self.headers.get("content-length") or 0))
            self.send_response(200)
            self.send_header("content-length", "2")
            self.end_headers()
            self.wfile.write(b"ok")

    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Route)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    rc = _launcher()
    switch = tmp_path / "inference"
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    script = rc.private_addon(tmp_path)
    port = port_outside_the_ephemeral_pool()
    # The account word keeps the request's own destination; map_remote stands in for
    # Anthropic, so this test never reaches the network and the upstream sees the
    # session's own authorization header.
    upstream_port = upstream.server_address[1]
    map_remote = (rf"|~d ^api\.anthropic\.com$|^http://api\.anthropic\.com"
                  rf"|http://127.0.0.1:{upstream_port}")
    env = dict(os.environ, RC_UPSTREAM=f"http://127.0.0.1:{upstream_port}",
               RC_KEY_FILE=str(key_file), RC_PARENT_PID=str(os.getpid()),
               RC_INFERENCE=str(switch))
    proxy = subprocess.Popen(rc.proxy_command(MITMDUMP, port, tmp_path / "mitm", script)
                             + ["--set", f"map_remote={map_remote}"],
                             env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": f"http://127.0.0.1:{port}"}))

    def post():
        seen.clear()
        req = urllib.request.Request("http://api.anthropic.com/v1/messages", data=b"{}",
                                     method="POST", headers={"authorization": "Bearer ACCOUNT"})
        try:
            with opener.open(req, timeout=HANG_TIMEOUT) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code

    try:
        def listening():
            with socket.socket() as s:
                return s.connect_ex(("127.0.0.1", port)) == 0
        assert wait_until(listening, HANG_TIMEOUT), "the proxy never listened"
        switch.write_text("route\n")
        assert post() == 200, "a rerouted call must not match the fail-closed guard"
        switch.write_text("account\n")
        assert post() == 200, "an account call must not match the fail-closed guard"
        assert seen == [("/v1/messages", "Bearer ACCOUNT")], \
            "the account call must reach the upstream on the session's own credential"
        # A broken addon, as a failed hot reload leaves it: the proxy stays up with
        # no addon, and the guard alone must stop the call -- on either word.
        script.write_text("raise RuntimeError('reload failed')\n")

        def failing_closed(word):
            assert proxy.poll() is None, "the proxy exited instead of failing closed"
            switch.write_text(word + "\n")
            return post() == 502
        for word in ("route", "account"):
            assert wait_until(lambda w=word: failing_closed(w), HANG_TIMEOUT), (
                f"without the addon, {word} inference must fail, never reach Anthropic")
            assert not seen, "without the addon, no call may reach the upstream"
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)
        upstream.shutdown()


@needs_loopback  # the proxy and upstream are real listeners: #611
@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_addon_dated_epoch_still_loads(tmp_path):
    """mitmproxy loads a -s script only when its mtime is newer than its last load
    (initially 0). A sync that delivered the tree with 1970 timestamps left every
    private copy unloadable: the key file was never consumed and ccmorph-rc refused
    to start (2026-09-29). The private copy must carry a fresh mtime."""
    rc = _launcher()
    src = tmp_path / "src"
    src.mkdir()
    for name in ("rc_addon.py", "rc_flow.py", "rc_route.py"):
        shutil.copy(ADDON.parent / name, src / name)
        os.utime(src / name, (0, 0))
    rc.HERE = src
    script = rc.private_addon(tmp_path)
    assert script.stat().st_mtime > 0
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    port = port_outside_the_ephemeral_pool()
    env = dict(os.environ, RC_UPSTREAM="http://127.0.0.1:9", RC_KEY_FILE=str(key_file),
               RC_PARENT_PID=str(os.getpid()))
    proxy = subprocess.Popen(rc.proxy_command(MITMDUMP, port, tmp_path / "mitm", script),
                             env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        # Not a boot-speed assertion: mitmdump takes as long as the host gives
        # it, and a key file still there after HANG_TIMEOUT is a real hang.
        assert wait_until(lambda: not key_file.exists(), HANG_TIMEOUT), (
            "addon never loaded: route key not consumed")
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)


@needs_loopback  # the proxy and upstreams are real listeners: #611
@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_a_picked_subagent_is_served_by_its_pick_through_a_real_proxy(tmp_path):
    """The router left a pick for one subagent's prompt: under the account word, the
    main thread still reaches the account while that subagent's requests reach the
    route, on the route's key and the pick's model. Its first request is parked
    until the addon reads its body; a later one routes from its headers."""
    import http.server
    import json
    import threading
    import hashlib

    def stub(name, seen):
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("content-length") or 0)))
                seen.append((name, self.headers.get("x-api-key") or self.headers.get("authorization"),
                             body.get("model")))
                self.send_response(200)
                self.send_header("content-length", "2")
                self.end_headers()
                self.wfile.write(b"ok")
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        return server

    seen = []
    account, route = stub("account", seen), stub("route", seen)
    rc = _launcher()
    (tmp_path / "inference").write_text("account\n")
    pending = tmp_path / "agents" / "pending"
    pending.mkdir(parents=True)
    (pending / f"{hashlib.sha256(b'Fix parse()').hexdigest()}.json").write_text('{"word": "route", "model": "kimi-k3"}')
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    script = rc.private_addon(tmp_path)
    port = port_outside_the_ephemeral_pool()
    map_remote = (rf"|~d ^api\.anthropic\.com$|^http://api\.anthropic\.com"
                  rf"|http://127.0.0.1:{account.server_address[1]}")
    env = dict(os.environ, RC_UPSTREAM=f"http://127.0.0.1:{route.server_address[1]}",
               RC_KEY_FILE=str(key_file), RC_PARENT_PID=str(os.getpid()),
               RC_INFERENCE=str(tmp_path / "inference"))
    proxy = subprocess.Popen(rc.proxy_command(MITMDUMP, port, tmp_path / "mitm", script)
                             + ["--set", f"map_remote={map_remote}"],
                             env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": f"http://127.0.0.1:{port}"}))

    def post(agent, text):
        headers = {"authorization": "Bearer ACCOUNT", "content-type": "application/json"}
        if agent:
            headers["x-claude-code-agent-id"] = agent
        body = {"model": "claude-opus-5-5", "messages": [{"role": "user", "content": [
            {"type": "text", "text": "<system-reminder>x</system-reminder>"}, {"type": "text", "text": text}]}]}
        req = urllib.request.Request("http://api.anthropic.com/v1/messages", method="POST",
                                     data=json.dumps(body).encode(), headers=headers)
        with opener.open(req, timeout=HANG_TIMEOUT) as r:
            return r.status

    try:
        def listening():
            with socket.socket() as s:
                return s.connect_ex(("127.0.0.1", port)) == 0
        assert wait_until(listening, HANG_TIMEOUT), "the proxy never listened"
        assert wait_until(lambda: not key_file.exists(), HANG_TIMEOUT), "the addon never loaded"
        assert post(None, "Fix parse()") == 200          # the main thread: its word, never the pick
        assert post("a1", "Fix parse()") == 200          # the subagent: parked, claimed, routed
        assert post("a1", "go on") == 200                # its later turn: bound
        assert post("a2", "other") == 200                # another subagent: its word
        assert seen == [("account", "Bearer ACCOUNT", "claude-opus-5-5"),
                        ("route", "route-key", "kimi-k3"),
                        ("route", "route-key", "kimi-k3"),
                        ("account", "Bearer ACCOUNT", "claude-opus-5-5")]
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)
        account.shutdown()
        route.shutdown()
