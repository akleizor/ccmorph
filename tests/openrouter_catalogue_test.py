"""An OpenRouter-shaped gateway sizes every chat model with no hand-entered number.

The listing comes from tests/fixtures/openrouter-models.json (trimmed to the fields
that matter; the max_input_tokens row is synthetic, to pin field precedence) and is
served from a local server: nothing here reaches the network.
"""
import http.server
import importlib.machinery
import importlib.util
import json
import os
import tempfile
import threading
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = (ROOT / 'tests' / 'fixtures' / 'openrouter-models.json').read_bytes()
spec = importlib.util.spec_from_loader('openrouter_probe', importlib.machinery.SourceFileLoader('openrouter_probe', str(ROOT/'libexec/refresh-model-registry')))
assert spec and spec.loader
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
probe._fetch_json_dict = lambda url, timeout: {}  # offline: no live catalogues

WINDOWS = {
    'anthropic/claude-sonnet-4': 1000000,       # context_length only
    'openai/gpt-5': 400000,                     # context_length only, not the 272000 catalogue input cap
    'meta-llama/llama-4-maverick': 900000,      # max_input_tokens beats context_length
}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(FIXTURE)))
        self.end_headers()
        self.wfile.write(FIXTURE)

    def log_message(self, *args):
        pass


def serve():
    server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def test_listing_publishes_windows_and_drops_non_chat_models():
    server = serve()
    try:
        env = {'NO_PROXY': '127.0.0.1', 'no_proxy': '127.0.0.1'}
        with patch.dict(os.environ, env):
            ids, sizes, _ = probe.list_models(f'http://127.0.0.1:{server.server_port}', 'k')
    finally:
        server.shutdown()
    assert ids == sorted(WINDOWS)
    assert sizes == WINDOWS


def test_new_openrouter_profile_resolves_every_model_without_a_manual_number():
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCPROTEUS_STATE_DIR': tmp}, clear=True):
        conf = Path(tmp) / 'or.conf'
        conf.write_text('type=apikey\nbase_url=https://openrouter.ai/api\napi_key=canary\n')
        assert probe.size_listing('or', FIXTURE.decode(), 'https://openrouter.ai/api', 'canary') == 0
        _, fields = probe.store.read_conf(conf)
        assert not any(k.startswith('context_window.') for k in fields)  # nothing hand-entered
        for model, window in WINDOWS.items():
            result = probe.store.resolve('or', model)
            assert result['window'] == window, model
            assert not result.get('unprobed'), model
        assert 'openai/text-embedding-3-large' not in probe.store.profile_windows('or')
