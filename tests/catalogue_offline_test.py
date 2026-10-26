"""A blocked public catalogue must still resolve known models offline."""
import importlib.util
import importlib.machinery
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_loader('offline_probe', importlib.machinery.SourceFileLoader('offline_probe', str(ROOT/'libexec/refresh-model-registry')))
assert spec and spec.loader
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def test_blocked_catalogue_uses_bundled_canonical_snapshot():
    with patch.object(probe, '_fetch_json_dict', return_value={}):
        catalogue = probe.fetch_canonical_catalogue()
    result = probe.canonical_catalogue_window(catalogue, 'gpt-5', None)
    assert result and result['value'] == 272000
    assert result['input'] == 272000 and result['context'] == 400000


def test_requested_model_can_use_catalogue_when_gateway_listing_times_out():
    with patch.object(probe, 'ccmorph_env', return_value={'ANTHROPIC_API_KEY': 'canary'}), \
            patch.object(probe, 'list_models', side_effect=TimeoutError('slow')), \
            patch.object(probe, 'prior_values', return_value={}), \
            patch.object(probe, 'prior_catalogue_tags', return_value={}), \
            patch.object(probe, '_fetch_json_dict', return_value={}):
        values, notes, sources = probe.probe_all(['sample'], 1, only_models={'gpt-5'})
    assert values == {'sample:gpt-5': 272000}
    assert sources['sample:gpt-5']['source'] == 'catalogue'


def test_first_launch_resolves_bundled_model_locally_without_writing():
    import os
    import tempfile
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCMORPH_STATE_DIR': tmp}, clear=True):
        conf = Path(tmp) / 'sample.conf'
        conf.write_text('type=apikey\napi_key=canary\n')
        before = conf.read_text()
        result = probe.store.resolve('sample', 'gpt-5')
        assert result['window'] == 272000
        assert result['source'] == 'catalogue-bundled'
        assert not result.get('unprobed')
        assert conf.read_text() == before


def test_snapshot_has_source_date_license_limits_and_families_only():
    import json
    data = json.loads((ROOT/'share/model-catalogue.json').read_text())
    assert data['source'] == 'https://models.dev/models.json'
    assert data['retrieved']
    assert 'Copyright (c) 2025 models.dev' in data['license']
    assert data['models']
    assert all(set(row) <= {'id', 'family', 'limit'} for row in data['models'].values())


def test_catalogue_fetch_sends_a_user_agent():
    """models.dev 403s urllib's default UA, silently stranding the live catalogue."""
    import http.server
    import json
    import threading

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            agent = self.headers.get('User-Agent', '')
            if not agent or agent.startswith('Python-urllib'):
                self.send_error(403)
                return
            body = json.dumps({'ok': 1}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        url = f'http://127.0.0.1:{server.server_port}/models.json'
        assert probe._fetch_json_dict(url, 5) == {'ok': 1}
    finally:
        server.shutdown()
