"""The addon, loaded by a real mitmdump, must fail closed and keep the key off disk.

Skipped when mitmdump is not installed. Uses a dead loopback port as the route, so
no network or credential is involved.
"""
import os
import shutil
import socket
import subprocess
import time
import urllib.request
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ADDON = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "rc_addon.py"
MITMDUMP = shutil.which("mitmdump")


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_dead_route_fails_closed_and_key_file_is_consumed(tmp_path):
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    key_file.chmod(0o600)
    port, dead = _free_port(), _free_port()
    env = dict(os.environ, RC_UPSTREAM=f"http://127.0.0.1:{dead}", RC_KEY_FILE=str(key_file),
               RC_PARENT_PID=str(os.getpid()))
    proxy = subprocess.Popen(
        [MITMDUMP, "-q", "--listen-host", "127.0.0.1", "--listen-port", str(port),
         "--set", f"confdir={tmp_path / 'mitm'}",
         "--allow-hosts", r"^api\.anthropic\.com(:443)?$", "-s", str(ADDON)],
        env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        # A listening socket alone does not mean mitmproxy has loaded the addon.
        deadline = time.monotonic() + HANG_TIMEOUT
        while time.monotonic() < deadline:
            assert proxy.poll() is None, "proxy exited before loading the addon"
            with socket.socket() as s:
                if not key_file.exists() and s.connect_ex(("127.0.0.1", port)) == 0:
                    break
            time.sleep(0.1)
        else:
            pytest.fail(
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
    import importlib.machinery
    import importlib.util
    path = ADDON.parent / "ccproteus-rc"
    loader = importlib.machinery.SourceFileLoader("ccproteus_rc_fc", str(path))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


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
    port = _free_port()
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
        deadline = time.monotonic() + HANG_TIMEOUT
        while time.monotonic() < deadline:
            with socket.socket() as s:
                if s.connect_ex(("127.0.0.1", port)) == 0:
                    break
            time.sleep(0.1)
        assert post() == 200, "a rerouted call must not match the fail-closed guard"
        # A broken addon, as a failed hot reload leaves it: the proxy stays up with
        # no addon, and the guard alone must stop the call.
        script.write_text("raise RuntimeError('reload failed')\n")
        time.sleep(3)
        assert proxy.poll() is None
        assert post() == 502, "without the addon, inference must fail, never reach Anthropic"
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)
        upstream.shutdown()


@pytest.mark.skipif(not MITMDUMP, reason="mitmdump not installed")
def test_addon_dated_epoch_still_loads(tmp_path):
    """mitmproxy loads a -s script only when its mtime is newer than its last load
    (initially 0). A sync that delivered the tree with 1970 timestamps left every
    private copy unloadable: the key file was never consumed and ccproteus-rc refused
    to start (2026-09-29). The private copy must carry a fresh mtime."""
    rc = _launcher()
    src = tmp_path / "src"
    src.mkdir()
    for name in ("rc_addon.py", "rc_route.py"):
        shutil.copy(ADDON.parent / name, src / name)
        os.utime(src / name, (0, 0))
    rc.HERE = src
    script = rc.private_addon(tmp_path)
    assert script.stat().st_mtime > 0
    key_file = tmp_path / "route-key"
    key_file.write_text("route-key")
    port = _free_port()
    env = dict(os.environ, RC_UPSTREAM="http://127.0.0.1:9", RC_KEY_FILE=str(key_file),
               RC_PARENT_PID=str(os.getpid()))
    proxy = subprocess.Popen(rc.proxy_command(MITMDUMP, port, tmp_path / "mitm", script),
                             env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        # Not a boot-speed assertion: mitmdump takes as long as the host gives
        # it, and a key file still there after HANG_TIMEOUT is a real hang.
        deadline = time.monotonic() + HANG_TIMEOUT
        while key_file.exists() and time.monotonic() < deadline:
            time.sleep(0.1)
        assert not key_file.exists(), "addon never loaded: route key not consumed"
    finally:
        proxy.terminate()
        proxy.wait(timeout=HANG_TIMEOUT)
