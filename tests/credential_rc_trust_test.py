"""Remote Control trust seeding keeps .claude.json private (issue #11)."""
import importlib.machinery
import importlib.util
import os
import stat
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def load(path, name):
    loader = importlib.machinery.SourceFileLoader(name, str(path))
    spec = importlib.util.spec_from_loader(name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class RemoteControlTrustSeed(unittest.TestCase):
    def test_remote_control_trust_seed_keeps_claude_json_private(self):
        rc = load(ROOT / "addons/remote-control/ccproteus-rc", "rc_cred")
        with tempfile.TemporaryDirectory() as tmp:
            cfg = Path(tmp, ".claude.json")
            cfg.write_text('{"oauthAccount":{"emailAddress":"a@example.com"}}')
            cfg.chmod(0o600)
            Path(tmp, "folder").mkdir()
            old = os.umask(0o022)
            try:
                rc.seed_trust(tmp, str(Path(tmp, "folder")))
            finally:
                os.umask(old)
            self.assertEqual(stat.S_IMODE(cfg.stat().st_mode), 0o600)


if __name__ == "__main__":
    unittest.main()
