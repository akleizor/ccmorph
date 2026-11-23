"""Every CI job ends inside merge-gate's 30 minutes, so a hung step fails and is retried (#827)."""
import re
from pathlib import Path

CI = Path(__file__).resolve().parent.parent / ".github/workflows/ci.yml"


def job_timeouts(text):
    """{job: timeout-minutes or None}; jobs are the two-space keys under `jobs:`."""
    jobs, job = {}, None
    for line in text.split("jobs:\n", 1)[1].splitlines():
        if m := re.match(r"  ([\w-]+):\s*$", line):
            job = m.group(1)
            jobs[job] = None
        elif job and (m := re.match(r"    timeout-minutes:\s*(\d+)", line)):
            jobs[job] = int(m.group(1))
    return jobs


def test_every_job_ends_inside_merge_gates_30_minutes():
    jobs = job_timeouts(CI.read_text())
    assert {"test", "python-floor", "install-check"} <= jobs.keys()
    late = {j: t for j, t in jobs.items() if t is None or t > 30}
    assert not late, f"set timeout-minutes <= 30 on {late}: a hung step otherwise holds CI for GitHub's 6 h default"


def test_apt_gives_up_on_a_dead_mirror():
    for line in CI.read_text().splitlines():
        if "apt-get update" in line:
            assert "Acquire::http::Timeout" in line, line
