"""On-selection preparation is bounded and never replays an inference request."""
import hashlib
import json
import runpy
from pathlib import Path
import subprocess
import threading
import urllib.request
import urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from support import HANG_TIMEOUT

SHIM = Path(__file__).resolve().parents[1] / 'libexec/gateway-shim'


def test_explicit_map_prepares_its_target_once_without_family_substitution(tmp_path, monkeypatch):
    module = runpy.run_path(str(SHIM))
    calls = []
    def prepare(argv, **kwargs):
        calls.append(argv[-1])
        return subprocess.CompletedProcess(argv, 0)
    monkeypatch.setattr(subprocess, 'run', prepare)
    router = module['ModelRoutes']({'adapter': 'adapter', 'profile': 'p',
        'config': str(tmp_path / 'config'), 'port': 1, 'models': [], 'timeout': 1,
        'identity': 'test-route', 'key_sha256': hashlib.sha256(b'key').hexdigest(),
        'model_map': {'claude-opus-5-5': 'claude-opus-5'}})
    monkeypatch.setattr(router.catalogue, 'served', lambda *args: {'claude-opus-5-6'})
    handler = module['IdShim'].__new__(module['IdShim'])
    handler.model_routes = router
    body, answer_as = handler._resolve(b'{"model":"claude-opus-5-5"}', 'key')
    assert json.loads(body)['model'] == 'claude-opus-5'
    assert answer_as == 'claude-opus-5-5'
    assert router.ensure(body) is None
    assert router.ensure(body) is None
    assert calls == ['claude-opus-5']
    # A doubled window marker is not the map's key (issue #250): the marker
    # is stripped once, as ensure strips it, so the request keeps its model.
    body, answer_as = handler._resolve(b'{"model":"claude-opus-5-5[1m][1m]"}', 'key')
    assert json.loads(body)['model'] == 'claude-opus-5-5[1m][1m]'
    assert answer_as is None


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
def test_count_tokens_only_prepares_explicit_map_hits(tmp_path, monkeypatch,
                                                     body, key, status, target):
    module = runpy.run_path(str(SHIM))
    prepared, discovered, received = [], [], []
    def prepare(argv, **kwargs):
        prepared.append(argv[-1])
        return subprocess.CompletedProcess(argv, 0 if argv[-1] == target else 1)
    monkeypatch.setattr(subprocess, 'run', prepare)
    router = module['ModelRoutes']({'adapter': 'adapter', 'profile': 'p',
        'config': str(tmp_path / 'config'), 'port': 1, 'models': [], 'timeout': 1,
        'identity': 'test-route', 'key_sha256': hashlib.sha256(b'key').hexdigest(),
        'model_map': {'claude-sonnet-5-5': 'gpt-6.1-sol',
                      'bare-only': 'bare-target[1m]', 'null-map': None,
                      'gpt-6.1-sol': 'do-not-chain'}})
    if b'no-map' in body:
        router.model_map = {}
        def unexpected_parse(*args):
            pytest.fail('an empty map must not parse the count request')
        monkeypatch.setitem(module['IdShim']._relay.__globals__,
                            'requested_model', unexpected_parse)
    def served(*args):
        discovered.append(args)
        return {'claude-opus-5-5'}
    monkeypatch.setattr(router.catalogue, 'served', served)
    usage = []
    monkeypatch.setattr(router.usage, 'tagged', lambda *args: usage.append(args))
    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_POST(self):
            received.append((self.path, self.headers['x-api-key'],
                             self.rfile.read(int(self.headers['content-length']))))
            self.send_response(200)
            self.send_header('content-length', '2')
            self.end_headers()
            self.wfile.write(b'OK')
    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    handler = type('Handler', (module['IdShim'],), {
        'model_routes': router, 'preserve_ids': True,
        'target': f'http://127.0.0.1:{upstream.server_port}'})
    server = ThreadingHTTPServer(('127.0.0.1', 0), handler)
    for s in (upstream, server):
        threading.Thread(target=s.serve_forever, daemon=True).start()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        req = urllib.request.Request(
            f'http://127.0.0.1:{server.server_port}/v1/messages/count_tokens?beta=true',
            data=body, headers={'x-api-key': key})
        try:
            response = opener.open(req, timeout=HANG_TIMEOUT)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            assert response.status == status
            response.read()
        assert discovered == []
        assert usage == []
        assert prepared == ([target] if target else [])
        if status == 401:
            assert received == []
        else:
            path, forwarded_key, forwarded_body = received[0]
            assert path == '/v1/messages/count_tokens?beta=true'
            assert forwarded_key == key
            if target:
                assert json.loads(forwarded_body)['model'] == target
            else:
                assert forwarded_body == body
    finally:
        for s in (server, upstream):
            s.shutdown()
            s.server_close()


def test_unknown_model_prepared_once_before_forwarding(tmp_path, monkeypatch):
    module = runpy.run_path(str(SHIM))
    calls = []
    def prepare(argv, **kwargs):
        calls.append(argv[-1])
        return subprocess.CompletedProcess(argv, 1 if argv[-1] == 'bad' else 0)
    monkeypatch.setattr(subprocess, 'run', prepare)
    router = module['ModelRoutes']({'adapter': 'adapter', 'profile': 'p',
        'config': str(tmp_path/'config'), 'port': 1, 'models': ['known'],
        'timeout': 1, 'identity': 'test-route', 'key_sha256': hashlib.sha256(b'key').hexdigest()})
    received = []
    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_POST(self):
            received.append(self.rfile.read(int(self.headers['content-length'])))
            self.send_response(200)
            self.send_header('content-length', '2')
            self.end_headers()
            self.wfile.write(b'OK')
    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    handler = type('Handler', (module['IdShim'],), {
        'model_routes': router, 'preserve_ids': True,
        'target': f'http://127.0.0.1:{upstream.server_port}'})
    server = ThreadingHTTPServer(('127.0.0.1', 0), handler)
    for s in (upstream, server):
        threading.Thread(target=s.serve_forever, daemon=True).start()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def request(model, key='key'):
        body = json.dumps({'model': model, 'messages': []}).encode()
        req = urllib.request.Request(f'http://127.0.0.1:{server.server_port}/v1/messages',
                                     data=body, headers={'x-api-key': key})
        try:
            response = opener.open(req, timeout=HANG_TIMEOUT)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, body
    try:
        for model in ('known', 'new', 'new'):
            code, body = request(model)
            assert code == 200 and received[-1] == body
        assert calls == ['new'] and len(received) == 3
        assert request('unauthorized', 'wrong')[0] == 401
        assert calls == ['new']
        assert request('bad')[0] == 400
        assert request('bad')[0] == 400  # failures can recover on a later request
        assert calls == ['new', 'bad', 'bad'] and len(received) == 3
    finally:
        for s in (server, upstream):
            s.shutdown()
            s.server_close()
