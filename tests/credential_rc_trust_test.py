"""Remote Control trust seeding keeps .claude.json private (issue #11)."""
import os
import stat
import tempfile
import unittest
from pathlib import Path

from support import load_script

ROOT = Path(__file__).resolve().parents[1]


class RemoteControlTrustSeed(unittest.TestCase):
    def test_remote_control_trust_seed_keeps_claude_json_private(self):
        rc = load_script(ROOT / "addons/remote-control/ccmorph-rc", "rc_cred")
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
