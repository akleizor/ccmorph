"""Diagnostic log mode (issue #12)."""
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

from support import HANG_TIMEOUT, home_env

ROOT = Path(__file__).resolve().parents[1]




class LogMode(unittest.TestCase):
    def test_diagnostic_log_is_created_private(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.chmod(tmp, 0o755)
            env, _ = home_env(tmp)
            subprocess.run(["zsh", "-c", "umask 022; source \"$1\"; _ccm_launch_core --log hello",
                            "t", str(ROOT / "lib/ccmorph.sh")], env=env, check=True, timeout=HANG_TIMEOUT)
            log = Path(tmp, f"ccmorph-{os.getuid()}.log")
            self.assertEqual(stat.S_IMODE(log.stat().st_mode) & 0o077, 0,
                             "ccmorph log is group/world readable")

    def _run_log(self, env):
        subprocess.run(["zsh", "-c", "umask 022; source \"$1\"; _ccm_launch_core --log hello",
                        "t", str(ROOT / "lib/ccmorph.sh")], env=env, check=True, timeout=HANG_TIMEOUT)

    def test_existing_loose_log_is_tightened(self):
        with tempfile.TemporaryDirectory() as tmp:
            env, _ = home_env(tmp)
            log = Path(tmp, f"ccmorph-{os.getuid()}.log")
            log.write_text("old\n")
            os.chmod(log, 0o644)
            self._run_log(env)
            self.assertEqual(stat.S_IMODE(log.stat().st_mode), 0o600)

    def test_symlinked_log_is_not_chmodded_through(self):
        with tempfile.TemporaryDirectory() as tmp:
            env, _ = home_env(tmp)
            target = Path(tmp, "victim")
            target.write_text("x")
            os.chmod(target, 0o644)
            Path(tmp, f"ccmorph-{os.getuid()}.log").symlink_to(target)
            self._run_log(env)
            self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o644)


if __name__ == "__main__":
    unittest.main()
