"""Under `pipefail`, a pipe into `grep -q` is a race (#964).

`grep -q` exits at its first match; a writer still writing then dies of
SIGPIPE and the pipeline fails on a true match. It only shows when the input
outgrows the pipe buffer, so a suite passes alone and fails under load. A
suite that sets pipefail hands grep its input directly: a file, or a
here-string (`grep -q pat <<< "$text"`).
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PIPE_INTO_GREP_Q = re.compile(r"\|\s*grep\s+(?:-[A-Za-z]*q|--quiet)")


def test_no_pipe_into_grep_q_under_pipefail_issue_964():
    found = []
    for path in sorted((ROOT / "tests").glob("*.zsh")):
        lines = path.read_text().splitlines()
        if not any(re.match(r"\s*setopt\b.*\bpipefail\b", l) for l in lines):
            continue
        for n, line in enumerate(lines, 1):
            if not line.lstrip().startswith("#") and PIPE_INTO_GREP_Q.search(line):
                found.append(f"{path.relative_to(ROOT)}:{n}: {line.strip()}")
    assert not found, "pipe into grep -q under pipefail (use a here-string):\n" + "\n".join(found)
