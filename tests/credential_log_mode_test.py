"""Diagnostic log mode (issue #12)."""
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(os.environ.get("CCPROTEUS_LIB_DIR") or Path(__file__).resolve().parents[1])




def home_env(tmp, **extra):
    home = Path(tmp)
    state = home / ".config/ccproteus"
    state.mkdir(parents=True, exist_ok=True)
    (home / ".claude/projects").mkdir(parents=True, exist_ok=True)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCPROTEUS", "_CCPROTEUS", "CLAUDE_", "ANTHROPIC_", "AWS_"))}
    env.update(HOME=tmp, XDG_CONFIG_HOME=str(home / ".config"), TMPDIR=tmp,
               CCPROTEUS_STATE_DIR=str(state), CCPROTEUS_LIB_DIR=str(ROOT),
               LANG="C.UTF-8", LC_ALL="C.UTF-8", **extra)
    return env, state


class LogMode(unittest.TestCase):
    def test_diagnostic_log_is_created_private(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.chmod(tmp, 0o755)
            env, _ = home_env(tmp)
            subprocess.run(["zsh", "-c", "umask 022; source \"$1\"; _ccs_log hello",
                            "t", str(ROOT / "ccproteus.sh")], env=env, check=True, timeout=HANG_TIMEOUT)
            log = Path(tmp, f"ccproteus-{os.getuid()}.log")
            self.assertEqual(stat.S_IMODE(log.stat().st_mode) & 0o077, 0,
                             "ccproteus log is group/world readable")

    def _run_log(self, env):
        subprocess.run(["zsh", "-c", "umask 022; source \"$1\"; _ccs_log hello",
                        "t", str(ROOT / "ccproteus.sh")], env=env, check=True, timeout=HANG_TIMEOUT)

    def test_existing_loose_log_is_tightened(self):
        with tempfile.TemporaryDirectory() as tmp:
            env, _ = home_env(tmp)
            log = Path(tmp, f"ccproteus-{os.getuid()}.log")
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
            Path(tmp, f"ccproteus-{os.getuid()}.log").symlink_to(target)
            self._run_log(env)
            self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o644)


if __name__ == "__main__":
    unittest.main()
