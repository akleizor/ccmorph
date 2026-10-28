"""Automatic compatibility uses declarations or proven cache, never inference."""
import contextlib
import importlib.machinery
import importlib.util
import io
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from types import SimpleNamespace
from unittest.mock import Mock, patch

from support import HANG_TIMEOUT

ADAPTER = Path(__file__).resolve().parents[1] / 'libexec/ccmorph-adapter'
loader = importlib.machinery.SourceFileLoader('adapter_auto', str(ADAPTER))
spec = importlib.util.spec_from_loader('adapter_auto', loader)
assert spec is not None
ad = importlib.util.module_from_spec(spec)
loader.exec_module(ad)


def row(dialect='anthropic'):
    return {'model': 'm', 'dialect': dialect, 'note': '', 'status': 200,
            'cap': None, 'nothink': False, 'id_recycle': False}


class AutoDetectionTest(unittest.TestCase):
    def test_versioned_base_url_is_not_given_a_second_v1(self):
        self.assertEqual(ad.api_url('https://provider.example/api/v4', '/v1/models'),
                         'https://provider.example/api/v4/models')
        self.assertEqual(ad.api_url('https://provider.example/api/V4', '/v1/models'),
                         'https://provider.example/api/V4/models')
        self.assertEqual(ad.api_url('https://provider.example', '/v1/models'),
                         'https://provider.example/v1/models')

    def test_detection_uses_the_anthropic_clients_path_not_the_openai_path(self):
        base = 'https://provider.example/api/v4'
        calls = []
        def respond(url, body, headers):
            calls.append(url)
            # A messages-shaped endpoint is not proof that the native client,
            # which appends /v1/messages, can reach it.
            return (200, '') if url in (base + '/messages', base + '/chat/completions') else (404, '')
        with patch.object(ad, 'post', side_effect=respond):
            dialect, _, _ = ad.dialect_of(base, 'test-key', 'm', bounded=True)
        self.assertEqual(dialect, 'chat')
        self.assertEqual(calls[0], base + '/v1/messages')
        config = ad.render([row('anthropic')], 'test-key', 1234, base, '/unused')
        self.assertIn('base-url: "' + base + '"', config)

    def test_upstream_proxy_is_rendered_only_when_set(self):
        config = ad.render([row('anthropic')], 'k', 1234, 'https://gw', '/unused',
                           'http://127.0.0.1:18480')
        self.assertIn('\nproxy-url: "http://127.0.0.1:18480"\n', config)
        config = ad.render([row('anthropic')], 'k', 1234, 'https://gw', '/unused')
        self.assertNotIn('proxy-url', config)

    def test_passthrough_filters_translator_block_binding(self):
        payload = ad.render([row('anthropic')], 'k', 1234, 'https://gw', '/unused').split('\npayload:\n')[1]
        self.assertIn('  filter:\n', payload)
        self.assertIn('protocol: "claude"\n      params:\n        - "thinking.block_binding"\n', payload)
        self.assertNotIn('payload:', ad.render([row('chat')], 'k', 1234, 'https://gw', '/unused'))

    def test_passthrough_user_agent_comes_from_the_shim_the_adapter_runs(self):
        # The claude-cli User-Agent restatement is the shim's, from the profile's
        # CCMORPH_USER_AGENT: the adapter asks the shim it will run (stock or a
        # profile's build) and renders what it prints; nothing without one.
        with tempfile.TemporaryDirectory() as tmp:
            shim = Path(tmp) / 'gateway-shim'
            shim.write_text('#!/bin/sh\n[ "$1" = translator-user-agent ] || exit 2\n'
                            '[ -n "$CCMORPH_USER_AGENT" ] && echo "$CCMORPH_USER_AGENT 9.9.9"\n'
                            'exit 0\n')
            shim.chmod(0o755)
            env = dict(os.environ, CCMORPH_USER_AGENT='hook-ua/')
            config = ad.render([row('anthropic')], 'k', 1234, 'https://gw', '/unused',
                               shim_path=str(shim), shim_env=env)
            claude_entry = config.split('claude-api-key:')[1]
            self.assertIn('User-Agent: "hook-ua/ 9.9.9"', claude_entry)
            self.assertNotIn('cloak', claude_entry)  # "never" strips Claude Code's identity on 8.0.34+
            unset = {k: v for k, v in env.items() if k != 'CCMORPH_USER_AGENT'}
            config = ad.render([row('anthropic')], 'k', 1234, 'https://gw', '/unused',
                               shim_path=str(shim), shim_env=unset)
        self.assertNotIn('headers:', config)
        config = ad.render([row('anthropic')], 'k', 1234, 'https://gw', '/unused')
        self.assertNotIn('headers:', config)

    def test_output_cap_uses_versioned_base_without_duplicate_v1(self):
        calls = []
        def fake_post(url, body, headers):
            calls.append(url)
            return 400, "supports at most 8192 completion tokens"
        with patch.object(ad, "post", side_effect=fake_post):
            self.assertEqual(ad.output_cap("https://gateway/api/v4", "k", "m", "chat"), 8192)
        self.assertEqual(calls, ["https://gateway/api/v4/chat/completions"])

    def test_protocol_uses_metadata_or_openai_compatible_default(self):
        for endpoints, expected in [(['/v1/messages'], 'anthropic'),
                                    (['/v1/responses'], 'responses'),
                                    (['/v1/chat/completions'], 'chat'),
                                    (None, 'unusable')]:
            requests = []
            def respond(request, timeout):
                requests.append(request)
                return io.BytesIO(json.dumps({'data': [{'id': 'm', 'supported_endpoints': endpoints}]}).encode())
            with patch.object(ad.OPENER, 'open', side_effect=respond):
                dialect, _, _ = ad.dialect_auto('https://gateway', 'k', 'm')
            self.assertEqual(dialect, expected)
            self.assertTrue(all(r.get_method() == 'GET' and r.data is None for r in requests))

    def test_duplicate_endpoint_declarations_use_first_matching_row(self):
        for first, second, expected in ((['/v1/messages'], ['/v1/chat/completions'], 'anthropic'),
                                         (['/v1/chat/completions'], ['/v1/messages'], 'chat'),
                                         (None, ['/v1/chat/completions'], 'unusable'),
                                         ([], ['/v1/chat/completions'], 'chat'),
                                         (['/v1/messages/count_tokens'], ['/v1/chat/completions'], 'chat'),
                                         (['/unknown'], ['/v1/responses'], 'responses')):
            with self.subTest(first=first, second=second):
                data = {'data': [{'id': 'm', 'supported_endpoints': first},
                                 {'id': 'm', 'supported_endpoints': second}]}
                with patch.object(ad.OPENER, 'open',
                                  return_value=io.BytesIO(json.dumps(data).encode())):
                    dialect, _, _ = ad.dialect_auto('https://gateway', 'k', 'm')
                self.assertEqual(dialect, expected)

    def test_missing_declaration_probes_only_selected_model_once(self):
        with tempfile.TemporaryDirectory() as td:
            cache = Path(td) / 'cache.json'
            with patch.object(ad, 'dialect_auto', return_value=('unusable', 'cannot determine protocol without inference', 0)), \
                 patch.object(ad, 'dialect_of', return_value=('chat', '', 200)) as probe, \
                 patch.object(ad, 'inventory', side_effect=AssertionError('unrelated model scan')):
                ad.classify('https://gateway', 'k', cache, required_model='m', auto=True)
                ad.classify('https://gateway', 'k', cache, required_model='m', auto=True)
                probe.assert_called_once_with('https://gateway', 'k', 'm', bounded=True)

    def test_selected_model_cached_while_waiting_on_lock_skips_inventory(self):
        with tempfile.TemporaryDirectory() as td:
            cache = Path(td) / 'cache.json'
            cache.write_text(json.dumps({
                'endpoint': 'https://gateway',
                'anthropic_path': '/v1/messages',
                'thinking_probe': ad.THINKING_PROBE_VERSION,
                'rows': [row('anthropic')],
            }))
            with patch.object(ad, 'inventory', return_value=['m', 'uncached']), \
                 patch.object(ad, 'dialect_auto', side_effect=AssertionError('probe')), \
                 patch.object(ad, 'dialect_of', side_effect=AssertionError('probe')), \
                 contextlib.redirect_stderr(io.StringIO()):
                rows = ad._classify_locked('https://gateway', 'k', cache, required_model='m', auto=True)
            self.assertEqual([r['model'] for r in rows], ['m'])

    def test_compatibility_requests_have_output_bounds(self):
        requests = []
        def post(url, body, headers):
            requests.append((url, body))
            return (400, 'unsupported')
        with patch.object(ad, 'post', side_effect=post):
            ad.dialect_of('https://gateway', 'k', 'm', bounded=True)
        self.assertEqual(len(requests), 4)
        for url, body in requests:
            self.assertEqual(body['model'], 'm')
            self.assertLessEqual(body.get('max_tokens', body.get('max_output_tokens', 0)), 16)
            self.assertGreater(body.get('max_tokens', body.get('max_output_tokens', 0)), 0)

    def test_failed_compatibility_does_not_cache_transient_result(self):
        with tempfile.TemporaryDirectory() as td:
            cache = Path(td) / 'cache.json'
            with patch.object(ad, 'dialect_auto', return_value=('unusable', 'no metadata', 0)), \
                 patch.object(ad, 'dialect_of', return_value=('unusable', 'timeout', 0)), \
                 self.assertRaises(SystemExit):
                ad.classify('https://gateway', 'k', cache, required_model='m', auto=True)
            self.assertFalse(cache.exists())

    def test_declared_rows_skip_paid_follow_up_probes(self):
        with tempfile.TemporaryDirectory() as td:
            with patch.object(ad, 'dialect_auto', return_value=('chat', 'declared', 200)), \
                 patch.object(ad, 'output_cap', side_effect=AssertionError), \
                 patch.object(ad, 'reasons_by_default', side_effect=AssertionError), \
                 patch.object(ad, 'recycles_tool_ids', side_effect=AssertionError):
                rows = ad.classify('https://gateway', 'k', Path(td) / 'cache.json', required_model='m', auto=True)
            self.assertEqual(rows[0]['dialect'], 'chat')

    def test_old_default_chat_cache_row_is_reclassified(self):
        with tempfile.TemporaryDirectory() as td:
            cache = Path(td) / 'cache.json'
            cache.write_text(json.dumps({
                'endpoint': 'https://gateway',
                'thinking_probe': ad.THINKING_PROBE_VERSION,
                'rows': [{**row('chat'), 'note': 'default OpenAI-compatible protocol'}],
            }))
            with patch.object(ad, 'inventory', side_effect=AssertionError('inventory')), \
                 patch.object(ad, 'dialect_auto', return_value=('unusable', 'no metadata', 0)), \
                 patch.object(ad, 'dialect_of', return_value=('anthropic', '', 200)) as probe:
                rows = ad.classify('https://gateway', 'k', cache, required_model='m', auto=True)
            self.assertEqual(rows[0]['dialect'], 'anthropic')
            probe.assert_called_once_with('https://gateway', 'k', 'm', bounded=True)

    def test_old_versioned_anthropic_verdicts_are_rechecked_once(self):
        with tempfile.TemporaryDirectory() as td:
            cache = Path(td) / 'cache.json'
            blob = {'endpoint': 'https://gateway/api/v4',
                    'thinking_probe': ad.THINKING_PROBE_VERSION,
                    'rows': [row(), {**row('chat'), 'model': 'chat-model'},
                             {**row('unusable'), 'model': 'failed-model', 'status': 404}]}
            cache.write_text(json.dumps(blob))
            # Pre-fix caches (no anthropic_path) are fully re-probed — every
            # verdict from that era was against the wrong URL.
            self.assertEqual(ad._cached_rows(blob['endpoint'], cache), [])
            # Post-fix caches (anthropic_path present) keep all rows.
            blob['anthropic_path'] = '/v1/messages'
            cache.write_text(json.dumps(blob))
            self.assertEqual(len(ad._cached_rows(blob['endpoint'], cache)), 3)

    def test_selected_model_map_is_classified_and_forwarded_to_the_native_edge(self):
        # The second case is the wire rule: Claude Code sends the bare id however
        # the picker row was spelled, and the shim routes on that same bare id, so
        # preparation reads the map on it too -- never on the [1m] spelling, which
        # would prepare a target no request reaches.
        for mapping, expected in (
                ({"claude-sonnet-5-5": "gpt-6.1-sol", "gpt-6.1-sol": "do-not-chain"},
                 "gpt-6.1-sol"),
                ({"claude-sonnet-5-5[1m]": "gpt-6.1-sol", "claude-sonnet-5-5": "gpt-6.1-sol-bare"},
                 "gpt-6.1-sol-bare")):
            with self.subTest(mapping=mapping):
                env = {"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "https://gateway",
                       "CCMORPH_MODEL_MAP": json.dumps(mapping)}
                with patch.object(ad, "profile_env", return_value=env), \
                     patch.object(ad, "classify", return_value=[row()]) as classify, \
                     patch.object(ad, "front_directly", return_value=0) as front, \
                     patch.object(ad.os, "dup2"), \
                     patch.object(ad.sys, "argv", ["ccmorph-adapter", "demo", "--auto", "--model",
                                                    "claude-sonnet-5-5[1m]"]):
                    self.assertEqual(ad.main(), 0)
                self.assertEqual(classify.call_args.args[3], expected)
                self.assertEqual(front.call_args.args[-1], mapping)

    def test_native_launch_rejects_later_mapped_chat_request_before_upstream_post(self):
        self.native_launch_rejects_mapped_chat('')

    def test_versioned_native_launch_rejects_mapped_chat_before_upstream_post(self):
        self.native_launch_rejects_mapped_chat('/api/v4')

    def native_launch_rejects_mapped_chat(self, prefix):
        real_popen = subprocess.Popen
        seen = []

        class Gateway(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def send_json(self, payload):
                body = json.dumps(payload).encode()
                self.send_response(200)
                self.send_header('content-type', 'application/json')
                self.send_header('content-length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                if self.path != prefix + ('/models' if prefix else '/v1/models'):
                    self.send_error(404)
                    return
                self.send_json({'data': [
                    {'id': 'm', 'supported_endpoints': ['/v1/messages']},
                    {'id': 'chat-only', 'supported_endpoints': ['/v1/chat/completions']},
                ]})

            def do_POST(self):
                payload = json.loads(self.rfile.read(int(self.headers['content-length'])))
                seen.append((self.path, payload['model']))
                self.send_json({'id': 'msg_native', 'type': 'message', 'role': 'assistant',
                                'model': payload['model'], 'content': [], 'usage': {}})

        gateway = ThreadingHTTPServer(('127.0.0.1', 0), Gateway)
        upstream = f'http://127.0.0.1:{gateway.server_address[1]}{prefix}'
        mapping = {'claude-sonnet-5-5': 'chat-only[1m]'}
        edge = None  # the real shim's port, read from its banner

        def start_edge(command, *, env, **kwargs):
            nonlocal edge
            route = json.loads(env['CCMORPH_MODEL_ROUTE'])
            self.assertEqual(ad.dialect_auto(upstream, 'k', 'm')[0], 'anthropic')
            self.assertEqual(route['models'], ['m'])
            self.assertEqual(route['model_map'], mapping)
            self.assertNotIn('config', route)
            self.assertIn('--preserve-tool-ids', command)
            # The real shim binary, exactly as the adapter built its argv and env.
            proc = real_popen(command, env=env, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
            self.addCleanup(proc.wait)
            self.addCleanup(proc.kill)
            self.addCleanup(proc.stdout.close)
            edge = int(proc.stdout.readline().split(':')[1].split(' ')[0])
            return Mock(poll=Mock(return_value=None))

        class RequestsComplete(Exception):
            pass

        def exercise_requests(_):
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            def request(model, path):
                payload = {'model': model, 'max_tokens': 1,
                           'messages': [{'role': 'user', 'content': 'hello'}]}
                return urllib.request.Request(
                    f'http://127.0.0.1:{edge}{path}',
                    data=json.dumps(payload).encode(),
                    headers={'content-type': 'application/json', 'x-api-key': 'k'})

            with opener.open(request('m', '/v1/messages'), timeout=HANG_TIMEOUT) as response:
                self.assertEqual(response.status, 200)
                self.assertEqual(json.load(response)['model'], 'm')
            self.assertEqual(seen,
                             [(prefix + '/v1/messages', 'm')])
            for path in ('/v1/messages', '/v1/messages/count_tokens'):
                with self.assertRaises(urllib.error.HTTPError) as caught:
                    with opener.open(request('claude-sonnet-5-5[1m]', path), timeout=HANG_TIMEOUT):
                        pass
                with caught.exception as response:
                    self.assertEqual(response.code, 400)
                    error = json.load(response)['error']
                self.assertEqual(error['type'], 'invalid_request_error')
                self.assertIn('chat-only', error['message'])
                self.assertIn('translation', error['message'])
                self.assertEqual(seen,
                                 [(prefix + '/v1/messages', 'm')], 'rejected request reached upstream')
            raise RequestsComplete

        with tempfile.TemporaryDirectory() as td, \
             patch.dict(ad.os.environ, {'HOME': td, 'XDG_STATE_HOME': td}), \
             patch.object(ad, 'profile_env', return_value={
                 'ANTHROPIC_API_KEY': 'k', 'ANTHROPIC_BASE_URL': upstream,
                 'CCMORPH_MODEL_MAP': json.dumps(mapping)}), \
             patch.object(ad, 'PROFILE_DIRS', Path(td)), \
             patch.object(ad, 'classify', return_value=[row()]) as classify, \
             patch.object(ad, 'render', side_effect=AssertionError('translator config')), \
             patch.object(ad, 'free_port', return_value=0), \
             patch.object(ad.subprocess, 'Popen', side_effect=start_edge) as launch, \
             patch.object(ad, 'tether'), patch.object(ad, 'serving', return_value=True), \
             patch.object(ad.signal, 'signal'), patch.object(ad.os, 'dup2'), \
             patch.object(ad.time, 'sleep', side_effect=exercise_requests), \
             patch.object(ad.sys, 'argv', ['ccmorph-adapter', 'demo', '--auto', '--model', 'm']), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            threading.Thread(target=gateway.serve_forever, daemon=True).start()
            try:
                with self.assertRaises(RequestsComplete):
                    ad.main()
            finally:
                gateway.shutdown()
                gateway.server_close()
        self.assertEqual(classify.call_args.args[3], 'm')
        classify.assert_called_once()
        launch.assert_called_once()

    def test_cached_native_main_needs_no_translator(self):
        with tempfile.TemporaryDirectory() as td:
            cache_dir = Path(td) / '.adapter-cache'
            cache_dir.mkdir()
            cache = cache_dir / (ad.hashlib.sha256(b'https://gateway').hexdigest()[:16] + '.json')
            cache.write_text(json.dumps({'endpoint': 'https://gateway', 'anthropic_path': '/v1/messages', 'thinking_probe': ad.THINKING_PROBE_VERSION, 'rows': [row()]}))
            # A native route is fronted by the gateway edge alone -- no network, no
            # probe, no translator, never a bare route past the edge.
            with patch.dict(ad.os.environ), patch.object(ad, 'OPENER'), \
                 patch.object(ad, 'PROFILE_DIRS', Path(td)), \
                 patch.object(ad, 'profile_env', return_value={'ANTHROPIC_API_KEY': 'k', 'ANTHROPIC_BASE_URL': 'https://gateway'}), \
                 patch.object(ad, 'inventory', side_effect=AssertionError('network')), \
                 patch.object(ad, 'dialect_auto', side_effect=AssertionError('probe')), \
                 patch.object(ad, 'front_directly', return_value=0) as front, \
                 patch.object(ad, 'free_port', side_effect=AssertionError('translator state')), \
                 patch.object(ad.os, 'dup2'), \
                 patch.object(ad.sys, 'argv', ['ccmorph-adapter', 'demo', '--auto', '--model', 'm']):
                self.assertEqual(ad.main(), 0)
            self.assertEqual(front.call_args.args[1], 'https://gateway')

    def test_only_the_direct_route_retries_in_the_shim(self):
        # The direct route has no translator underneath, so the shim owns
        # transport retries. (The translator path passes no such flag: its
        # cliproxyapi already retries, and a second layer would multiply
        # attempts.) Recorded from the real spawn, then abandoned: nothing else
        # of the launch is exercised.
        seen = []

        class Spawned(Exception):
            pass

        def spawn(command, **kwargs):
            seen.append(command)
            raise Spawned

        args = SimpleNamespace(profile='demo', port=None, token=None,
                               reserved=None, inbound_token=False,
                               parent_pid=None)
        with patch.object(ad.subprocess, 'Popen', side_effect=spawn):
            with self.assertRaises(Spawned):
                ad.front_directly(args, 'https://gateway', 'k', 'id', [row()], {})
        self.assertIn('--retry-transport', seen[0])

    def test_the_direct_route_hands_profile_keys_to_the_shim_env(self):
        # Profile request quirks ride in the shim's environment for its hooks
        # module to read, not in new shim flags: every CCMORPH_* key the
        # profile set is ferried through unchanged.
        seen = []

        class Spawned(Exception):
            pass

        def spawn(command, **kwargs):
            seen.append((command, kwargs.get('env') or {}))
            raise Spawned

        args = SimpleNamespace(profile='demo', port=None, token=None,
                               reserved=None, inbound_token=False,
                               parent_pid=None)
        extras = {'CCMORPH_SHIM_HOOKS': '/stable/hooks.py',
                  'CCMORPH_USER_AGENT': 'ua/{version}'}
        with patch.object(ad.subprocess, 'Popen', side_effect=spawn):
            with self.assertRaises(Spawned):
                ad.front_directly(args, 'https://gateway', 'k', 'id', [row()], extras)
        command, env = seen[0]
        self.assertEqual(env.get('CCMORPH_SHIM_HOOKS'), '/stable/hooks.py')
        self.assertEqual(env.get('CCMORPH_USER_AGENT'), 'ua/{version}')
        self.assertNotIn('--repair-tool-references', command)
        self.assertNotIn('--affinity-header', command)


if __name__ == '__main__':
    unittest.main()
