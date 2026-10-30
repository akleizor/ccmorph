"""Profile removal/rename must not leak or resurrect context state."""
import os
from pathlib import Path
import tempfile
import unittest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


class ProfileCommands(unittest.TestCase):
    def test_real_cli_removes_and_renames_scoped_context_without_touching_history(self):
        import subprocess
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            state = home / '.config/ccmorph'
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
            env = {k: v for k, v in os.environ.items() if not k.startswith(('CCMORPH', '_CCMORPH', 'CLAUDE_', 'MODEL_WINDOW'))}
            env.update(HOME=tmp, XDG_CONFIG_HOME=str(home / '.config'),
                       CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"))
            def run(*args, answer=None):
                return subprocess.run([str(ROOT / 'bin/ccmorph'), *args], env=env,
                                      input=answer, capture_output=True, text=True, timeout=HANG_TIMEOUT)
            renamed = run('mv', 'gw', 'renamed')
            self.assertEqual(renamed.returncode, 0, renamed.stderr)
            self.assertIn('"m": 123456', (profiles / 'renamed/models.json').read_text())
            self.assertFalse(registry.exists())
            removed = run('rm', 'renamed')
            self.assertEqual(removed.returncode, 0, removed.stderr)
            self.assertFalse((state / 'renamed.conf').exists())
            self.assertFalse((profiles / 'renamed').exists())
            self.assertIn('"m": 222222', (profiles / 'other/models.json').read_text())
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
