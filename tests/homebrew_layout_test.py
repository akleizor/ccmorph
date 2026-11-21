"""#674: the paths a Homebrew install's consumers resolve, run as they run them.

akleizor/homebrew-tap's formula moves this repo's bin/, lib/, libexec/ and
share/ under the keg's own libexec/ and writes brew's env script at
<keg>/bin/ccmorph. Two consumers outside this repo depend on that layout:

- a hive landing's settle() execs the core at
  realpath(which ccmorph)/../../libexec/libexec/ccmorph-core
  (agents-harness merge-gate) to record the landing's outcome;
- a dispatched job runs `ccmorph run <profile> --model <m> -- ... -p <prompt>`
  through the installed command (agents-harness scratch.job_argv).

When the keg's layout moved under them (0.5.15), every landing crashed in
settle() with FileNotFoundError and every job sourcing
~/.config/ccmorph/lib/ccmorph.sh died at exit 127. Both consumers now use
paths the package ships from install on; this pins that contract, straight
after `brew install` and straight after `brew upgrade`, with nothing of
ccmorph run before and no ~/.config/ccmorph/lib in the home.
"""
import os
from pathlib import Path
import shutil
import subprocess
import sys

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]

# settle()'s outcome step, spelled as merge-gate runs it.
SETTLE = '''
import pathlib, shutil, subprocess, sys
core = pathlib.Path(shutil.which("ccmorph") or "ccmorph").resolve().parents[1] / "libexec/libexec/ccmorph-core"
subprocess.run([str(core), "shim", "outcome", sys.argv[1], "accepted"], check=True)
'''


def _pour(brew, version):
    """Lay out one keg as the tap's `def install` does, then point opt and bin
    at it the way `brew install` and `brew upgrade` do."""
    keg = brew / "Cellar/ccmorph" / version
    tree = keg / "libexec"
    for part in ("bin", "lib", "share"):
        shutil.copytree(ROOT / part, tree / part, symlinks=True)
    (tree / "libexec").mkdir(parents=True)
    shutil.copy2(ROOT / "libexec/ccmorph-core", tree / "libexec/ccmorph-core")
    (keg / "bin").mkdir()
    wrapper = keg / "bin/ccmorph"
    wrapper.write_text("#!/bin/bash\n"
                       f'CCMORPH_LIB_DIR="{brew}/opt/ccmorph/libexec/lib" '
                       f'exec "{brew}/opt/ccmorph/libexec/bin/ccmorph"  "$@"\n')
    wrapper.chmod(0o555)
    for link, target in ((brew / "opt/ccmorph", f"../Cellar/ccmorph/{version}"),
                         (brew / "bin/ccmorph", f"../Cellar/ccmorph/{version}/bin/ccmorph")):
        link.parent.mkdir(parents=True, exist_ok=True)
        if link.is_symlink():
            link.unlink()
        link.symlink_to(target)


def test_a_landing_and_a_dispatched_job_run_first_after_a_homebrew_install_and_upgrade(tmp_path):
    assert (ROOT / "libexec/ccmorph-core").is_file(), "run `make shim` first"
    brew, home, stubs = tmp_path / "brew", tmp_path / "home", tmp_path / "stubs"
    stubs.mkdir()
    # The stand-in for Claude Code records its argv and answers, as a job must.
    claude = stubs / "claude"
    claude.write_text('#!/bin/sh\n'
                      'printf "%s\\n" "$*" >> "$CCMORPH_TEST_LOG"\n'
                      'printf "answered\\n"\n')
    # The launch's endpoint check answers 200 with no network.
    curl = stubs / "curl"
    curl.write_text('#!/bin/sh\n'
                    'for a do [ "$a" = "-w" ] && { printf "__CCMORPH_STATUS__200"; exit 0; }; done\n'
                    'for a do case "$a" in *messages*) printf "200"; exit 0 ;; esac; done\n'
                    'exit 1\n')
    claude.chmod(0o755)
    curl.chmod(0o755)
    (home / ".config/ccmorph").mkdir(parents=True)
    (home / ".config/ccmorph/native.conf").write_text(
        "\n".join(("type=apikey", "base_url=https://api.anthropic.com",
                   "api_key=" + "fake", "model=claude-sonnet-4-5", "")))
    log = tmp_path / "claude.log"
    env = {"HOME": str(home), "PATH": f"{brew}/bin:{stubs}:/usr/bin:/bin",
           "CCMORPH_TEST_LOG": str(log), "LANG": "C",
           "HTTPS_PROXY": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1"}

    def run(argv):
        return subprocess.run(argv, env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)

    for version, task in (("0.0.1", "ccmorph-6741"), ("0.0.2", "ccmorph-6742")):
        _pour(brew, version)
        if version == "0.0.2":
            # brew's cleanup removes the old keg after the upgrade.
            shutil.rmtree(brew / "Cellar/ccmorph/0.0.1")
        # The landing first: nothing of ccmorph has run since this keg was poured.
        landed = run([sys.executable, "-I", "-c", SETTLE, task])
        assert landed.returncode == 0, landed.stderr
        assert "Traceback" not in landed.stdout + landed.stderr
        # Then a dispatched job, as the harness builds one.
        job = run(["ccmorph", "run", "native", "--model", "claude-sonnet-4-5", "--", "-p", "prompt"])
        assert job.returncode == 0, job.stdout + job.stderr
        assert "answered" in job.stdout
        assert log.read_text().splitlines()[-1].endswith("-p prompt"), log.read_text()

    rows = (home / ".local/state/ccmorph/outcomes.jsonl").read_text()
    for task in ("ccmorph-6741", "ccmorph-6742"):
        assert f'"task":"{task}","outcome":"accepted"' in rows, rows
    # Neither consumer needed the install.sh-shaped library path.
    assert not (home / ".config/ccmorph/lib").exists()
