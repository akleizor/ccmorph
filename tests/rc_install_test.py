"""The rc-hosts LaunchAgent plist, as install.sh generates it, and the install
claims the add-on's page makes about that installer."""
import os
import re
from pathlib import Path
import plistlib
import subprocess
import tempfile

import pytest

from support import HANG_TIMEOUT

INSTALL = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "install.sh"
README = INSTALL.parent / "README.md"

# A `ccmorph-rc` invocation on the page: the command, then its first argument.
# `ccmorph-rc-hosts` never matches (no whitespace after `ccmorph-rc`), and a
# mention in prose carries a backtick before the space, not whitespace.
INVOCATION = re.compile(r"(?<![\w-])ccmorph-rc\s+(\S+)")


def test_supervisor_plist_is_interactive_and_kept_alive():
    # A stub launchctl: bootstrap must not reach the operator's real launchd.
    with tempfile.TemporaryDirectory() as td:
        home = Path(td)
        (home / ".config/ccmorph").mkdir(parents=True)  # the core installer's dir
        stub = home / "bin"
        stub.mkdir()
        (stub / "launchctl").write_text("#!/bin/sh\nexit 0\n")
        (stub / "launchctl").chmod(0o755)
        env = {k: v for k, v in os.environ.items() if not k.startswith(("CCMORPH_", "_CCMORPH_"))}
        env.update(HOME=td, ZDOTDIR=td, XDG_CONFIG_HOME=str(home / ".config"),
                   PATH=f"{stub}:/usr/bin:/bin")
        done = subprocess.run(["zsh", str(INSTALL)], env=env, text=True,
                              capture_output=True, timeout=HANG_TIMEOUT)
        assert done.returncode == 0, done.stdout + done.stderr
        plist = plistlib.loads((home / "Library/LaunchAgents/com.ccmorph.rc-hosts.plist").read_bytes())
        # Interactive lifts launchd's CPU/IO throttling, so the poll loop keeps
        # its bridges online on a heavily loaded Mac.
        assert plist["ProcessType"] == "Interactive"
        assert plist["KeepAlive"] is True
        assert plist["ThrottleInterval"] == 2


def unprofiled_invocations(text):
    """The page's `ccmorph-rc` example lines that name no profile placeholder.
    Only the `--switch` form may spell its argument `<profile>` (#593): that
    argument is the target profile — the edge's route, the session's own account,
    a profile brought up beside the edge — and an account is not a route. Every
    other form keeps #276's `<route-profile>`; no form carries a real name."""
    bad = []
    for line in text.splitlines():
        m = INVOCATION.search(line)
        if not m:
            continue
        arg = m.group(1)
        if arg == "<route-profile>" or (arg == "<profile>" and "--switch" in line):
            continue
        bad.append(line.strip())
    return bad


def test_every_example_writes_the_profile_where_a_profile_goes():
    """Issue #276: the page's first examples read `ccmorph-rc gateway` as though
    `gateway` were a subcommand, when it is the operator's profile name — the
    script's own usage line spells the argument `<route-profile>`, and so must
    every invocation on the page (the `--switch` form dials a target, not a route,
    and spells it `<profile>`; #593)."""
    text = README.read_text(encoding="utf-8")
    bad = unprofiled_invocations(text)
    assert not bad, f"ccmorph-rc examples not naming the route as a profile: {bad}"
    assert "ccmorph-rc <route-profile>" in text, "the page no longer shows the invocation"


@pytest.mark.parametrize("line,names_a_profile", [
    pytest.param("ccmorph-rc <route-profile> --probe", True, id="route-form-276"),
    pytest.param("ccmorph-rc <profile> --switch", True, id="switch-form-593"),
    pytest.param("ccmorph-rc <profile> --probe", False, id="placeholder-off-switch-593"),
    pytest.param("ccmorph-rc oli --switch", False, id="real-name-on-switch-593"),
    pytest.param("ccmorph-rc gateway", False, id="bare-subcommand-276"),
])
def test_only_the_switch_form_may_name_a_bare_profile(line, names_a_profile):
    assert (line not in unprofiled_invocations(line)) is names_a_profile


def test_the_page_opens_with_requirements_and_reach_before_the_first_command():
    """Issue #276: the page opened with `addons/remote-control/install.sh`, a
    path that exists only inside a source checkout, although the main README
    installs ccmorph three ways — and Homebrew ships no addons folder, and the
    one-line installer deletes its download afterwards, so neither reader can
    run that line. It also never said what the add-on needs. Before the first
    `ccmorph-rc` invocation, the page must name its requirements and every
    install route, and carry the commands that get a working clone."""
    text = README.read_text(encoding="utf-8")
    opening = text.split("ccmorph-rc <route-profile>", 1)[0]
    for word in ("macOS", "Python", "mitmproxy", "Homebrew", "one-line", "clone"):
        assert word in opening, f"the opening no longer names {word}"
    assert "git clone" in opening and "addons/remote-control/install.sh" in opening
