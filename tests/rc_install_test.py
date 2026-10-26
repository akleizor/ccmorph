"""The rc-hosts LaunchAgent plist, as install.sh generates it."""
import os
from pathlib import Path
import plistlib
import subprocess
import tempfile

from support import HANG_TIMEOUT

INSTALL = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "install.sh"


def test_supervisor_plist_is_interactive_and_kept_alive():
    # A stub launchctl: bootstrap must not reach the operator's real launchd.
    with tempfile.TemporaryDirectory() as td:
        home = Path(td)
        (home / ".config/ccproteus").mkdir(parents=True)  # the core installer's dir
        stub = home / "bin"
        stub.mkdir()
        (stub / "launchctl").write_text("#!/bin/sh\nexit 0\n")
        (stub / "launchctl").chmod(0o755)
        env = {k: v for k, v in os.environ.items() if not k.startswith(("CCPROTEUS_", "_CCPROTEUS_"))}
        env.update(HOME=td, ZDOTDIR=td, XDG_CONFIG_HOME=str(home / ".config"),
                   PATH=f"{stub}:/usr/bin:/bin")
        done = subprocess.run(["zsh", str(INSTALL)], env=env, text=True,
                              capture_output=True, timeout=HANG_TIMEOUT)
        assert done.returncode == 0, done.stdout + done.stderr
        plist = plistlib.loads((home / "Library/LaunchAgents/com.ccproteus.rc-hosts.plist").read_bytes())
        # Interactive lifts launchd's CPU/IO throttling, so the poll loop keeps
        # its bridges online on a heavily loaded Mac.
        assert plist["ProcessType"] == "Interactive"
        assert plist["KeepAlive"] is True
        assert plist["ThrottleInterval"] == 2
