"""Issue #13: the models error must not echo URL userinfo or query."""
import os
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


class UrlRedaction(unittest.TestCase):
    def test_unreachable_endpoint_error_hides_url_userinfo_and_query(self):
        with tempfile.TemporaryDirectory() as tmp:
            env, state = home_env(tmp)
            (state / "gw.conf").write_text(
                "type=apikey\napi_key=sk-x\nbase_url=http://user:HUNTER2pw@127.0.0.1:1/?token=QTOK\n")
            result = subprocess.run([str(ROOT / "ccproteus"), "models", "gw"], env=env,
                                    capture_output=True, text=True, timeout=HANG_TIMEOUT)
            log = Path(tmp, f"ccproteus-{os.getuid()}.log")
            text = result.stdout + result.stderr + (log.read_text() if log.exists() else "")
            self.assertIn("could not reach", text)
            self.assertIn("http://127.0.0.1:1", text)
            self.assertNotIn("HUNTER2pw", text)
            self.assertNotIn("QTOK", text)


if __name__ == "__main__":
    unittest.main()
