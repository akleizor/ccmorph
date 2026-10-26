"""Launch sizes models the gateway added since the last launch, once, metadata only."""
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import tempfile
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_loader('launch_probe', importlib.machinery.SourceFileLoader('launch_probe', str(ROOT/'libexec/refresh-model-registry')))
assert spec and spec.loader
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
probe._fetch_json_dict = lambda url, timeout: {}  # offline: no live catalogues in tests

LISTING = json.dumps({"data": [
    {"id": "known-model"},
    {"id": "declared-new", "max_input_tokens": 500000},
    {"id": "claude-fable-5-dd-5-tpg"},          # disguised gpt-5: catalogue knows it
    {"id": "mystery-new"},                       # nobody knows it
    {"id": "hidden-new"},                        # excluded by the operator
    {"id": "embed-new", "capabilities": {"chat_completion": False}},
]})
CATALOGUE = {"openai": {"models": {"gpt-5": {"limit": {"context": 400000, "input": 272000}}}}}


def test_launch_sizes_only_new_models_and_remembers_misses():
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCPROTEUS_STATE_DIR': tmp}, clear=True):
        conf = Path(tmp) / 'gw.conf'
        conf.write_text('type=apikey\nbase_url=https://gw.invalid\napi_key=canary\nexcluded_models=hidden-new\n')
        _, fields = probe.store.read_conf(conf)
        route = probe.store.route_fingerprint(fields)
        conf.write_text(conf.read_text() + 'context_discovered=' + json.dumps(
            {"route": route, "models": {"known-model": 123456}}) + '\n')

        fetches = []
        def fetch(url, timeout):
            fetches.append(url)
            return CATALOGUE if url == probe.CATALOGUE_URL else {}

        with patch.object(probe, '_fetch_json_dict', side_effect=fetch), \
                patch.object(probe, 'validation_window', return_value=None):
            assert probe.size_listing('gw', LISTING, 'https://gw.invalid', 'canary') == 0
        windows = probe.store.profile_windows('gw')
        assert windows == {"known-model": 123456, "declared-new": 500000, "gpt-5": 272000}
        _, fields = probe.store.read_conf(conf)
        assert probe.store.discovered_misses(fields) == {"mystery-new": probe.time.strftime("%Y-%m-%d", probe.time.gmtime())}
        assert probe.store.discovered_sources(fields)["gpt-5"]["source"] == "catalogue"
        assert fetches, "a new unknown model must reach the catalogue"

        # Next launch within the retry window: every model is known or a recent miss.
        with patch.object(probe, '_fetch_json_dict', side_effect=AssertionError('fetched again')), \
                patch.object(probe, 'validation_window', side_effect=AssertionError('asked again')):
            assert probe.size_listing('gw', LISTING, 'https://gw.invalid', 'canary') == 0
        assert not probe.recently_missed('2026-09-01', '2026-09-08')
        assert probe.recently_missed('2026-09-02', '2026-09-08')

        # A later manual value retires the miss.
        probe.store.save_discovered('gw', {"mystery-new": 64000}, probe.store.profile_identity('gw'))
        _, fields = probe.store.read_conf(conf)
        assert probe.store.discovered_misses(fields) == {}


def test_internal_model_is_sized_from_its_validation_refusal():
    import io
    import urllib.error

    class Opener:
        def __init__(self, bodies):
            self.bodies = list(bodies)

        def open(self, req, timeout):
            status, text = self.bodies.pop(0)
            raise urllib.error.HTTPError(req.full_url, status, "x", {}, io.BytesIO(text.encode()))

    vllm = 'max_tokens=100000000 cannot be greater than max_model_len=max_total_tokens=1000000.'
    assert probe.validation_window('https://gw.invalid', 'k', 'm',
                                   Opener([(429, 'rate limited'), (400, vllm)])) == 1000000
    # An output-cap range is not a context window.
    capped = 'The max_tokens parameter is illegal: range [1,131072]'
    assert probe.validation_window('https://gw.invalid', 'k', 'm',
                                   Opener([(400, capped), (400, capped)])) is None


def test_provider_profile_keeps_windows_sized_through_its_bridge():
    # A provider conf stores no endpoint or key; launch passes the bridge's own.
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCPROTEUS_STATE_DIR': tmp}, clear=True):
        (Path(tmp) / 'codex.conf').write_text('type=provider\nprovider=codex\n')
        listing = json.dumps({"data": [{"id": "claude-fable-5-dd-los-6-tpg", "context_length": 272000}]})
        with patch.object(probe, '_fetch_json_dict', side_effect=AssertionError('declared size needs no catalogue')):
            assert probe.size_listing('codex', listing, 'http://127.0.0.1:54321', 'ephemeral') == 0
        assert probe.store.profile_windows('codex') == {"gpt-6-sol": 272000}


def test_provider_source_contract():
    source = {"name": "t", "owned_by": ["openai"], "url": "https://t.invalid/m.json",
              "rows": "models", "id": "slug", "default": "context_window", "window": "max_context_window"}
    catalogue = {"models": [
        {"slug": "gpt-6-sol", "context_window": 272000, "max_context_window": 872000},
        {"slug": "gpt-5.6-sol", "context_window": 272000, "max_context_window": 872000},
        {"slug": "gpt-new", "context_window": 272000, "max_context_window": 500000},
        {"slug": "gpt-small", "context_window": 1000000, "max_context_window": 272000},
    ]}
    rows = [{"id": m, "owned_by": "openai"} for m in ("gpt-6-sol", "gpt-5.6-sol", "gpt-new", "gpt-small")]
    rows.append({"id": "glm-9", "owned_by": "zhipu"})
    declared = {"gpt-6-sol": 272000, "gpt-5.6-sol": 372000, "gpt-small": 1000000, "glm-9": 272000}
    opted = {"window_sources": "t"}
    with tempfile.TemporaryDirectory() as tmp:
        (Path(tmp) / "window-sources.json").write_text(json.dumps({"sources": [source]}))

        def windows(fetch, rows, fields):
            with patch.object(probe, 'SHARE_DIR', Path(tmp)), patch.object(probe, '_fetch_json_dict', **fetch):
                return probe.provider_windows(declared, rows, fields, 1, {})

        assert windows({"return_value": catalogue}, rows, opted) == ({
            "gpt-6-sol": 872000,      # declared the provider default: raised
            "gpt-5.6-sol": 372000,    # gateway's own figure: kept
            "gpt-small": 1000000,     # a source never lowers a declaration
            "glm-9": 272000}, set())  # no source speaks for its owner; gpt-new not invented
        # A profile that names no source is never re-sized, whatever its rows say.
        fetched = {"side_effect": AssertionError('fetched')}
        assert windows(fetched, rows, {}) == (declared, set())
        assert windows(fetched, [{"id": "glm-9", "owned_by": "zhipu"}], opted) == (declared, set())
        # Unreachable source: nothing changes, and its models stay undecided.
        assert windows({"return_value": {}}, rows, opted) == (
            declared, {"gpt-6-sol", "gpt-5.6-sol", "gpt-small"})


def test_shipped_sources_follow_the_contract():
    sources = json.loads((ROOT / "share/window-sources.json").read_text())["sources"]
    assert sources
    for source in sources:
        assert {"name", "owned_by", "url", "rows", "id", "window"} <= set(source), source
        assert isinstance(source["owned_by"], list) and source["url"].startswith("https://")


def test_unreachable_source_leaves_the_model_for_the_next_launch():
    # Storing the bare declaration while the source is down would pin it forever.
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCPROTEUS_STATE_DIR': tmp}, clear=True):
        (Path(tmp) / 'codex.conf').write_text('type=provider\nprovider=codex\nwindow_sources=codex\n')
        listing = json.dumps({"data": [{"id": "gpt-6-sol", "owned_by": "openai", "context_length": 272000}]})
        with patch.object(probe, '_fetch_json_dict', return_value={}):
            assert probe.size_listing('codex', listing, 'http://127.0.0.1:1', 'k') == 0
        _, fields = probe.store.read_conf(Path(tmp) / 'codex.conf')
        assert probe.store.profile_windows('codex') == {}
        assert probe.store.discovered_misses(fields) == {}


def test_unreachable_source_is_fetched_once_per_retry_window():
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCPROTEUS_STATE_DIR': tmp}, clear=True):
        (Path(tmp) / 'codex.conf').write_text('type=provider\nprovider=codex\nwindow_sources=codex\n')
        listing = json.dumps({"data": [{"id": "gpt-6-sol", "owned_by": "openai", "context_length": 272000}]})
        calls = []

        def down(url, timeout):
            calls.append(url)
            return {}

        with patch.object(probe, '_fetch_json_dict', side_effect=down), \
                patch.object(probe, 'validation_window', side_effect=AssertionError('quota spent')):
            probe.size_listing('codex', listing, 'http://127.0.0.1:1', 'k')
            probe.size_listing('codex', listing, 'http://127.0.0.1:1', 'k')
        source_calls = [u for u in calls if "codex_client_models" in u]
        assert len(source_calls) == 1, calls   # the second launch trusts the marker
        assert probe.store.profile_windows('codex') == {}


def test_provider_bridge_is_never_validated_with_a_request():
    with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'CCPROTEUS_STATE_DIR': tmp}, clear=True):
        (Path(tmp) / 'codex.conf').write_text('type=provider\nprovider=codex\n')
        listing = json.dumps({"data": [{"id": "gpt-unknown", "owned_by": "openai"}]})
        with patch.object(probe, 'validation_window', side_effect=AssertionError('quota spent')):
            assert probe.size_listing('codex', listing, 'http://127.0.0.1:1', 'k') == 0
