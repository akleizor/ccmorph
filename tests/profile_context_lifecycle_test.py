"""Profile removal/rename must not leak or resurrect context state."""
import importlib.machinery
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from support import HANG_TIMEOUT

SCRIPT = Path(__file__).resolve().parents[1] / "libexec/refresh-model-registry"
spec = importlib.util.spec_from_loader("lifecycle_registry", importlib.machinery.SourceFileLoader("lifecycle_registry", str(SCRIPT)))
assert spec and spec.loader
rmr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rmr)


class ProfileCommands(unittest.TestCase):
    def test_real_cli_removes_and_renames_scoped_context_without_touching_history(self):
        import subprocess
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            state = home / '.config/ccproteus'
            state.mkdir(parents=True)
            profiles = state / 'profiles'
            (profiles / 'gw').mkdir(parents=True)
            shared = home / '.claude/projects'
            shared.mkdir(parents=True)
            (shared / 'history').write_text('keep')
            (profiles / 'gw/projects').symlink_to(shared)
            (state / 'gw.conf').write_text('type=apikey\napi_key=canary\n')
            registry = state / 'model-registry'
            registry.write_text('gw:m=123456\nother:m=222222\n')
            (state / 'other.conf').write_text('type=apikey\napi_key=other\n')
            env = {k: v for k, v in os.environ.items() if not k.startswith(('CCPROTEUS', '_CCPROTEUS', 'CLAUDE_', 'MODEL_WINDOW'))}
            env.update(HOME=tmp, XDG_CONFIG_HOME=str(home / '.config'),
                       CCPROTEUS_STATE_DIR=str(state), CCPROTEUS_LIB_DIR=str(SCRIPT.parent.parent / "lib"))
            def run(*args, answer=None):
                return subprocess.run([str(SCRIPT.parent.parent / 'bin/ccproteus'), *args], env=env,
                                      input=answer, capture_output=True, text=True, timeout=HANG_TIMEOUT)
            renamed = run('mv', 'gw', 'renamed')
            self.assertEqual(renamed.returncode, 0, renamed.stderr)
            self.assertIn('"m":123456', (state / 'renamed.conf').read_text())
            self.assertFalse(registry.exists())
            removed = run('rm', 'renamed')
            self.assertEqual(removed.returncode, 0, removed.stderr)
            self.assertFalse((state / 'renamed.conf').exists())
            self.assertIn('"m":222222', (state / 'other.conf').read_text())
            self.assertEqual((shared / 'history').read_text(), 'keep')
            (profiles / 'orphan').mkdir()
            (profiles / 'orphan/settings.json').write_text('{"model":"private"}')
            refused = run('add', 'orphan')
            self.assertNotEqual(refused.returncode, 0)
            self.assertIn('leftover', refused.stderr)
            self.assertTrue((profiles / 'orphan/settings.json').exists())
            (profiles / 'orphan/projects').symlink_to(shared)
            declined = run('rm', 'orphan', answer='n\n')
            self.assertNotEqual(declined.returncode, 0)
            self.assertTrue((profiles / 'orphan/settings.json').exists())
            cleaned = run('rm', 'orphan', answer='y\n')
            self.assertEqual(cleaned.returncode, 0, cleaned.stderr)
            self.assertFalse((profiles / 'orphan').exists())
            self.assertEqual((shared / 'history').read_text(), 'keep')


class ProfileDiscoveryLifecycle(unittest.TestCase):
    def test_manual_override_skips_paid_probe_without_becoming_discovered_data(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {"CCPROTEUS_STATE_DIR": tmp}):
            conf = Path(tmp, "gw.conf")
            conf.write_text('type=apikey\napi_key=canary\ncontext_window.manual=333333\n')
            with patch.object(rmr, 'ccproteus_env', return_value={'ANTHROPIC_API_KEY': 'k'}), \
                    patch.object(rmr, 'list_models', return_value=(['manual'], {}, [])), \
                    patch.object(rmr, 'probe') as paid:
                values, notes, _ = rmr.probe_all(['gw'], 1, mode='auto', only_models={'manual'})
            paid.assert_not_called()
            self.assertNotIn('gw:manual', values)
            self.assertTrue(any('manual' in reason for reason, _ in notes))
