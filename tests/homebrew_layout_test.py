"""#674: the core path a Homebrew install gives the tools that resolve it.

akleizor/homebrew-tap moves this repo's bin/, lib/, libexec/ and share/ under
the keg's own libexec/ and writes brew's env script at <keg>/bin/ccmorph, so
the core sits at <keg>/libexec/libexec/ccmorph-core. Two consumers outside
this repo depend on the install:

- a hive landing's settle() records its outcome by running
  ${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph/libexec/ccmorph-core shim outcome
  <task> accepted (agents-harness merge-gate), the path install.sh's layout
  has; a Homebrew-only install had nothing there, so the outcome was lost;
- a dispatched job runs `ccmorph run <profile> --model <m> -- ... -p <prompt>`
  through the installed command (agents-harness scratch.job_argv).

Homebrew cannot write $HOME, so ccmorph's first stateful command links that
path to the core behind brew's opt link, which every `brew upgrade` re-points.
"""
import os
from pathlib import Path
import shutil
import subprocess
import sys

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]

# settle()'s outcome step, spelled as merge-gate runs it, without its
# try/except: a missing core must fail here, not pass silently.
SETTLE = '''
import os, pathlib, subprocess, sys
cfg = os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
core = pathlib.Path(cfg) / "ccmorph/libexec/ccmorph-core"
subprocess.run([str(core), "shim", "outcome", sys.argv[1], "accepted"], check=True)
'''

JOB = ["ccmorph", "run", "native", "--model", "claude-sonnet-4-5", "--", "-p", "prompt"]


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


def _machine(tmp_path):
    """A home with one native profile, stub claude and curl, and a runner with
    Homebrew's bin first on PATH, as the consumers run."""
    assert (ROOT / "libexec/ccmorph-core").is_file(), "run `make shim` first"
    base = tmp_path.resolve()
    brew, home, stubs = base / "brew", base / "home", base / "stubs"
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
    log = base / "claude.log"
    env = {"HOME": str(home), "PATH": f"{brew}/bin:{stubs}:/usr/bin:/bin",
           "CCMORPH_TEST_LOG": str(log), "LANG": "C",
           "HTTPS_PROXY": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1"}

    def run(argv, **extra):
        return subprocess.run(argv, env={**env, **extra}, text=True, capture_output=True,
                              timeout=HANG_TIMEOUT)

    return brew, home, log, run


def _job_ran(run, log):
    job = run(JOB)
    assert job.returncode == 0, job.stdout + job.stderr
    assert "answered" in job.stdout
    assert log.read_text().splitlines()[-1].endswith("-p prompt"), log.read_text()


def _landed(run, home, task):
    landed = run([sys.executable, "-I", "-c", SETTLE, task])
    assert landed.returncode == 0, landed.stderr
    assert "Traceback" not in landed.stdout + landed.stderr
    rows = (home / ".local/state/ccmorph/outcomes.jsonl").read_text()
    assert f'"task":"{task}","outcome":"accepted"' in rows, rows


def test_a_homebrew_install_gives_settle_its_core_and_keeps_it_across_an_upgrade(tmp_path):
    brew, home, log, run = _machine(tmp_path)
    core = home / ".config/ccmorph/libexec/ccmorph-core"
    opt_core = str(brew / "opt/ccmorph/libexec/libexec/ccmorph-core")
    _pour(brew, "0.0.1")
    # brew writes nothing under $HOME: the path is ccmorph's to make.
    assert not os.path.lexists(core)
    # The first stateful command, here a dispatched job, publishes it.
    _job_ran(run, log)
    assert core.is_symlink(), "the first job must link the core into ~/.config/ccmorph/libexec"
    assert os.readlink(core) == opt_core, "the link must name brew's opt path, not a Cellar version"
    assert core.parent.stat().st_mode & 0o077 == 0, "the libexec dir must be owner-only"
    _landed(run, home, "ccmorph-6741")

    # brew upgrade: a new keg, opt and bin re-pointed, the old keg removed by
    # its cleanup. Nothing of ccmorph runs before this landing.
    _pour(brew, "0.0.2")
    shutil.rmtree(brew / "Cellar/ccmorph/0.0.1")
    _landed(run, home, "ccmorph-6742")
    _job_ran(run, log)
    assert os.readlink(core) == opt_core
    # Dispatch never needed the install.sh-shaped library path.
    assert not (home / ".config/ccmorph/lib").exists()
    assert not list(core.parent.glob("*.tmp-*")), "no half-made link may stay behind"


def test_a_core_already_there_is_kept_and_a_dangling_link_is_repaired(tmp_path):
    brew, home, log, run = _machine(tmp_path)
    _pour(brew, "0.0.1")
    core = home / ".config/ccmorph/libexec/ccmorph-core"
    core.parent.mkdir()
    # install.sh's own copy is a regular file: never replaced.
    core.write_text("#!/bin/sh\nexit 0\n")
    core.chmod(0o755)
    _job_ran(run, log)
    assert not core.is_symlink() and core.read_text() == "#!/bin/sh\nexit 0\n"
    # A link that still reaches a core is someone's choice: kept.
    core.unlink()
    other = tmp_path.resolve() / "other-core"
    other.write_text("#!/bin/sh\nexit 0\n")
    other.chmod(0o755)
    core.symlink_to(other)
    _job_ran(run, log)
    assert os.readlink(core) == str(other)
    # A link to a core that is gone is repaired.
    core.unlink()
    core.symlink_to(tmp_path.resolve() / "removed/ccmorph-core")
    _job_ran(run, log)
    assert os.readlink(core) == str(brew / "opt/ccmorph/libexec/libexec/ccmorph-core")
    _landed(run, home, "ccmorph-6743")


def test_read_only_commands_and_other_installs_publish_no_core(tmp_path):
    brew, home, log, run = _machine(tmp_path)
    _pour(brew, "0.0.1")
    libexec = home / ".config/ccmorph/libexec"
    for argv in (["ccmorph", "--version"], ["ccmorph", "--help"], ["ccmorph", "doctor"]):
        run(argv)
        assert not os.path.lexists(libexec), f"{argv} must stay read-only"
    # A source checkout runs its own core and has nothing to publish.
    checkout = run([str(ROOT / "bin/ccmorph")] + JOB[1:])
    assert checkout.returncode == 0, checkout.stdout + checkout.stderr
    assert not os.path.lexists(libexec), "a source checkout must not link its core into the config dir"
