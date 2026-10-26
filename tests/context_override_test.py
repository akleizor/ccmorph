"""Exercise manual context overrides through the shipped command."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


class ContextOverride(unittest.TestCase):
    def test_exact_set_get_reset_and_validation_preserve_other_state(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            state = home / '.config/ccmorph'
            state.mkdir(parents=True)
            conf = state / 'gw.conf'
            conf.write_text('type=apikey\napi_key=secret-context_window.a.b=keep\ncontext_window.axb=654321\n')
            cache = state / 'profiles/gw/cache/context-window'
            cache.parent.mkdir(parents=True)
            cache.write_text('a.b\t200000\nother\t300000\n')
            env = {k: v for k, v in os.environ.items() if not k.startswith(('CCMORPH', '_CCMORPH', 'CLAUDE_', 'MODEL_WINDOW'))}
            env.update(HOME=tmp, XDG_CONFIG_HOME=str(home / '.config'), CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"))
            def cli(*args):
                return subprocess.run([str(ROOT / "bin/ccmorph"), *args], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
            result = cli('set', 'gw', 'context', 'a.b', '131072')
            self.assertNotEqual(result.returncode, 0)
            result = subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'a.b', '131072'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('api_key=secret-context_window.a.b=keep', conf.read_text())
            self.assertIn('context_window.axb=654321', conf.read_text())
            self.assertEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'a.b'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout.strip(), '131072')
            import json
            resolved = subprocess.run([str(ROOT / 'libexec/model-window'), 'gw', 'a.b'], env=env,
                                      capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(json.loads(resolved.stdout)['window'], 131072)
            validated = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_read gw', 'test', str(ROOT / 'lib/ccmorph.sh')],
                                       env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(validated.returncode, 0, validated.stderr)
            self.assertEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'claude-opus-5', '1000000'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode, 0)
            resolved = subprocess.run([str(ROOT / 'libexec/model-window'), 'gw', 'claude-opus-5'], env=env,
                                      capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(json.loads(resolved.stdout)['send_id'], 'claude-opus-5[1m]')
            self.assertFalse(cache.exists())
            self.assertIn('other\t300000', cache.with_name('context-window.migrated').read_text())
            self.assertIn('"other":300000', conf.read_text())
            self.assertEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'a.b', 'auto'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode, 0)
            self.assertEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'a.b'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout.strip(), 'auto')
            resolved = subprocess.run([str(ROOT / 'libexec/model-window'), 'gw', 'a.b'], env=env,
                                      capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(json.loads(resolved.stdout)['window'], 200000)
            self.assertEqual(json.loads(resolved.stdout)['source'], 'conf-discovered')
            baseline = conf.read_text()
            for value in ('0', '-1', '1e6', 'x', '999999999999999999999999999999'):
                self.assertNotEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'a.b', value], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode, 0)
                self.assertEqual(conf.read_text(), baseline)
            self.assertNotEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'bad\nkey', '123456'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode, 0)
            self.assertNotEqual(subprocess.run([str(ROOT / 'libexec/model-window'), '--context', 'gw', 'a.b', '123456', 'ignored'], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode, 0)
