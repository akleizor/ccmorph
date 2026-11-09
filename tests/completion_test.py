"""Tab completion for ccmorph in zsh (issue #281).

Before it, Tab after `ccmorph run` offered the files of the current folder and a
half-typed command completed to nothing. The completion function is run in a
plain `zsh -f` with `compadd` and `_files` replaced by stand-ins that print what
they were given, so no terminal is needed: the words on the line go in, the
candidates the function offers come out.
"""
import os
import re
import subprocess
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
SITE_FUNCTIONS = ROOT / "share/zsh/site-functions"

HARNESS = r'''
compadd() { [[ "$1" == -- ]] && shift; print -rl -- "$@" }
_files() { print -r -- "<files>" }
fpath=("$1" $fpath)
autoload -Uz _ccmorph
shift
words=("$@")
CURRENT=$#
_ccmorph
'''

PROFILES = ["Ops", "home", "work"]


def offered(env, line):
    """The candidates Tab offers after `line`; a trailing space starts a new word."""
    words = line.split(" ")
    proc = subprocess.run(
        ["zsh", "-f", "-c", HARNESS, "zsh", str(SITE_FUNCTIONS), *words],
        env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert proc.returncode == 0, proc.stderr
    return sorted(proc.stdout.split())


@pytest.fixture
def home(tmp_path):
    state = tmp_path / ".config/ccmorph"
    state.mkdir(parents=True)
    for name in PROFILES:
        (state / f"{name}.conf").write_text("type=token\n")
    (state / "notes.txt").write_text("not a profile\n")
    return {"HOME": str(tmp_path), "PATH": os.environ["PATH"]}


def help_all_verbs(env):
    proc = subprocess.run(
        ["zsh", str(ROOT / "bin/ccmorph"), "help", "all"],
        env={**env, "CCMORPH_LIB_DIR": str(ROOT / "lib")},
        capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert proc.returncode == 0, proc.stderr
    return sorted(set(re.findall(r"\bccmorph ([a-z]+)", proc.stdout)))


def test_tab_after_ccmorph_offers_every_command_help_all_lists(home):
    assert offered(home, "ccmorph ") == help_all_verbs(home)


@pytest.mark.parametrize("verb", ["run", "use", "rm", "mv", "config", "inspect", "quota",
                                  "set", "get", "models", "login", "cred", "hop", "probe",
                                  "context"])
def test_tab_after_a_profile_verb_offers_the_profile_names(home, verb):
    assert offered(home, f"ccmorph {verb} ") == PROFILES


@pytest.mark.parametrize("line, want", [
    ("ccmorph doctor ", sorted(["--verbose", *PROFILES])),
    ("ccmorph doctor --verbose ", PROFILES),
    ("ccmorph set work ", ["effort", "model"]),
    ("ccmorph run work --add-dir ", ["<files>"]),
])
def test_tab_completes_the_later_words(home, line, want):
    assert offered(home, line) == want


def test_profile_names_come_from_the_redirected_state_dir(home, tmp_path):
    other = tmp_path / "elsewhere"
    other.mkdir()
    (other / "lab.conf").write_text("type=token\n")
    assert offered({**home, "CCMORPH_STATE_DIR": str(other)}, "ccmorph run ") == ["lab"]


def test_compinit_binds_the_function_to_the_ccmorph_command(home):
    # The real consumer is `compinit`, which scans fpath and binds a file whose
    # first line is `#compdef ccmorph`. -D writes no compdump; HOME is the
    # fixture's tmp dir, so nothing outside the test dir is touched.
    proc = subprocess.run(
        ["zsh", "-f", "-c",
         'fpath=("$1" $fpath); autoload -Uz compinit; compinit -u -D; '
         'print -r -- "${_comps[ccmorph]-unbound}"',
         "zsh", str(SITE_FUNCTIONS)],
        env=home, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout.strip() == "_ccmorph"