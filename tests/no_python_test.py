"""The core runs on no interpreter (#36 step 4).

Everything but the Remote Control add-on (addons/remote-control, a mitmproxy add-on)
is zsh and the compiled `ccmorph-core`. This pins that: no core file names python3,
and nothing in the core path runs the Python helper files that step 5 deletes.
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CORE_FILES = sorted([*ROOT.glob("lib/*.zsh"), *ROOT.glob("lib/*.sh"), *ROOT.glob("bin/*"),
                     ROOT / "install.sh"])
PYTHON_HELPERS = re.compile(r"LIBEXEC_DIR/(model-window|settings-compose|ccmorph-adapter|refresh-model-registry)\b")


def test_the_core_files_exist():
    assert len(CORE_FILES) > 10


def test_no_core_file_mentions_python3():
    hits = [f"{f.relative_to(ROOT)}:{n}" for f in CORE_FILES
            for n, line in enumerate(f.read_text(encoding="utf-8").splitlines(), 1) if "python3" in line]
    assert not hits, f"python3 in the core (only addons/remote-control may use it): {hits}"


def test_no_core_file_runs_a_python_helper():
    hits = [f"{f.relative_to(ROOT)}:{n}" for f in CORE_FILES
            for n, line in enumerate(f.read_text(encoding="utf-8").splitlines(), 1)
            if PYTHON_HELPERS.search(line) and not line.lstrip().startswith("#")]
    assert not hits, f"the core runs a Python helper; use `ccmorph-core <verb>`: {hits}"
