"""On-selection preparation is bounded and never replays an inference request.

Black-box: the compiled shim runs against a fake gateway, with a fake
adapter script standing in for `ccmorph-adapter --model`, so the argv the shim
hands the adapter and the bytes the gateway receives are the whole observation.
"""
import hashlib
import json
import os
import subprocess
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from support import HANG_TIMEOUT, SHIM, needs_shim

pytestmark = needs_shim

# The shim runs `<adapter> <profile> --ensure-config ... --model <id>`; this one
# logs <id> and succeeds only for the model named in $FAKE_ADAPTER_OK.
ADAPTER = '#!/bin/sh\nfor last; do :; done\necho "$last" >> "$FAKE_ADAPTER_LOG"\n[ "$last" = "$FAKE_ADAPTER_OK" ]\n'


class Gateway(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.server.catalogue_reads += 1
        body = json.dumps({"data": [{"id": "claude-opus-5-6"}, {"id": "claude-opus-5"}]}).encode()
        self.send_response(200)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        self.server.received.append((self.path, self.headers["x-api-key"],
                                     self.rfile.read(int(self.headers["content-length"]))))
        self.send_response(200)
        self.send_header("content-length", "2")
        self.end_headers()
        self.wfile.write(b"OK")


class Rig:
    """One shim in front of one fake gateway, with a fake adapter."""

    def __init__(self, tmp, models=(), model_map=None, ok_model=None):
        self.state = Path(tmp) / "state"
        self.log = Path(tmp) / "adapter.log"
        adapter = Path(tmp) / "adapter"
        adapter.write_text(ADAPTER)
        adapter.chmod(0o755)
        self.gateway = ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        self.gateway.received, self.gateway.catalogue_reads = [], 0
        threading.Thread(target=self.gateway.serve_forever, daemon=True).start()
        upstream = f"http://127.0.0.1:{self.gateway.server_port}"
        spec = {"adapter": str(adapter), "profile": "p", "config": str(Path(tmp) / "config"), "port": 1,
                "models": list(models), "timeout": 5, "identity": "test-route",
                "key_sha256": hashlib.sha256(b"key").hexdigest(), "upstream": upstream,
                "model_map": model_map or {}}
        env = dict(os.environ, CCMORPH_MODEL_ROUTE=json.dumps(spec), XDG_STATE_HOME=str(self.state),
                   FAKE_ADAPTER_LOG=str(self.log), FAKE_ADAPTER_OK=ok_model or "")
        self.proc = subprocess.Popen([*SHIM, "0", upstream, "--preserve-tool-ids",
                                      f"--parent-pid={os.getpid()}"],
                                     stdout=subprocess.PIPE, text=True, env=env)
        self.port = int(self.proc.stdout.readline().split(":")[1].split(" ")[0])
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def close(self):
        self.proc.terminate()
        self.proc.wait(HANG_TIMEOUT)
        self.proc.stdout.close()
        self.gateway.shutdown()
        self.gateway.server_close()

    def post(self, path, body, key="key"):
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", data=body,
                                     headers={"x-api-key": key})
        try:
            response = self.opener.open(req, timeout=HANG_TIMEOUT)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            response.read()
            return response.status

    @property
    def prepared(self):
        return self.log.read_text().split() if self.log.exists() else []


@pytest.fixture
def rig(tmp_path):
    made = []

    def make(**kwargs):
        made.append(Rig(tmp_path, **kwargs))
        return made[-1]
    yield make
    for r in made:
        r.close()


def test_explicit_map_prepares_its_target_once_without_family_substitution(rig):
    r = rig(model_map={"claude-opus-5-5": "claude-opus-5"}, ok_model="claude-opus-5")
    body = b'{"model":"claude-opus-5-5"}'
    # The gateway serves a newer Opus, but an explicit map is final.
    assert r.post("/v1/messages", body) == 200
    assert r.post("/v1/messages", body) == 200
    assert [json.loads(b)["model"] for _, _, b in r.gateway.received] == ["claude-opus-5"] * 2
    assert r.prepared == ["claude-opus-5"]
    # A doubled window marker is not the map's key (issue #250): the marker
    # is stripped once, so the request is no map hit and keeps its own model:
    # the shim tries to prepare that model, not the target.
    assert r.post("/v1/messages", b'{"model":"claude-opus-5-5[1m][1m]"}') == 400
    assert r.prepared[-1] == "claude-opus-5-5[1m]" and len(r.gateway.received) == 2


@pytest.mark.parametrize('body,key,status,target', [
    (b'{ "model": "claude-opus-5", "messages": [] }', 'key', 200, None),
    (b'{ "model": "no-map", "messages": [] }', 'key', 200, None),
    (b'{ "model": "claude-opus-5[1m]" }', 'wrong', 200, None),
    (b'{"model":"unrelated"}', 'key', 200, None),
    (b'{"model":"null-map"}', 'key', 200, None),
    (b'{broken', 'wrong', 200, None),
    (b'{"messages":[]}', 'wrong', 200, None),
    (b'{"model":null}', 'wrong', 200, None),
    (b'[]', 'wrong', 200, None),
    (b'{"model":"claude-sonnet-5-5"}', 'key', 200, 'gpt-6.1-sol'),
    (b'{"model":"claude-sonnet-5-5[1m]"}', 'key', 200, 'gpt-6.1-sol'),
    (b'{"model":"bare-only[1m]"}', 'key', 200, 'bare-target'),
    (b'{"model":"claude-sonnet-5-5"}', 'wrong', 401, None),
])
def test_count_tokens_only_prepares_explicit_map_hits(rig, body, key, status, target):
    r = rig(model_map={'claude-sonnet-5-5': 'gpt-6.1-sol', 'bare-only': 'bare-target[1m]',
                       'null-map': None, 'gpt-6.1-sol': 'do-not-chain'},
            ok_model=target)
    assert r.post('/v1/messages/count_tokens?beta=true', body, key) == status
    assert r.gateway.catalogue_reads == 0
    assert not (r.state / 'ccmorph' / 'usage').exists()      # count_tokens is never a usage row
    assert r.prepared == ([target] if target else [])
    if status == 401:
        assert r.gateway.received == []
    else:
        path, forwarded_key, forwarded_body = r.gateway.received[0]
        assert (path, forwarded_key) == ('/v1/messages/count_tokens?beta=true', key)
        if target:
            assert json.loads(forwarded_body)['model'] == target
        else:
            assert forwarded_body == body


def test_unknown_model_prepared_once_before_forwarding(rig):
    r = rig(models=['known'], ok_model='new')
    def request(model, key='key'):
        body = json.dumps({'model': model, 'messages': []}).encode()
        return r.post('/v1/messages', body, key), body
    for model in ('known', 'new', 'new'):
        code, body = request(model)
        assert code == 200 and r.gateway.received[-1][2] == body
    assert r.prepared == ['new'] and len(r.gateway.received) == 3
    assert request('unauthorized', 'wrong')[0] == 401
    assert r.prepared == ['new']
    assert request('bad')[0] == 400
    assert request('bad')[0] == 400  # failures can recover on a later request
    assert r.prepared == ['new', 'bad', 'bad'] and len(r.gateway.received) == 3
