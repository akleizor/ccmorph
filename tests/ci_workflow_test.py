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
    apt_updates = [line for line in CI.read_text().splitlines()
                   if re.search(r"\bapt-get\b.*\bupdate\b", line)]
    assert apt_updates, "no apt-get update line matched in ci.yml: the matcher drifted, so this guard enforced nothing"
    for line in apt_updates:
        assert "Acquire::http::Timeout" in line, line
        assert "Acquire::Retries" in line, line


def job_block(text, name):
    """One job's lines under `jobs:`, comment lines dropped: from `  name:` to the next two-space key."""
    out, inside = [], False
    for line in text.split("jobs:\n", 1)[1].splitlines():
        if re.match(r"  [\w-]+:\s*$", line):
            inside = line.strip() == f"{name}:"
        elif inside and not line.lstrip().startswith("#"):
            out.append(line)
    return "\n".join(out)


def test_util_suite_runs_in_both_go_images_as_a_user():
    """#1051: a Linux without GNU ps belongs to CI's green. The util suite runs in
    golang:1.27 (GNU ps) and golang:1.27-alpine (BusyBox ps) as the runner's uid
    (root writes through the 0o500/0o555 dirs the refusal tests rely on), under an
    init that reaps orphans, and a leg whose image's ps is not its kind fails."""
    job = job_block(CI.read_text(), "util-linux")
    assert job, "ci.yml has no util-linux job"
    assert "runs-on: ubuntu-24.04" in job, job
    assert re.search(r"image: ['\"]?golang:1\.27['\"]?\s*\n\s*ps: gnu\b", job), job
    assert re.search(r"image: ['\"]?golang:1\.27-alpine['\"]?\s*\n\s*ps: busybox\b", job), job
    for want in ("--init", '--user "$(id -u):$(id -g)"', "HOME=/tmp", "GOPATH=/tmp/go",
                 "GOCACHE=/tmp/go-cache", "readonly", "go test -count=1 ./util/", "ps -axo pid="):
        assert want in job, f"util-linux job lacks {want}"
    for banned in ("apk ", "apt-get", "procps"):
        assert banned not in job, f"util-linux job names {banned!r}: the alpine leg must test BusyBox ps, not an installed one"
