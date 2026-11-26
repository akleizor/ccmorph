"""The suite refuses to run green beside a fork storm, and never becomes one (GH-919).

A Hive turn's test script forked children that never exited until the account
held every slot kern.maxprocperuid (8000) allowed; the first signal was dead RC
sessions (2026-11-25, session 9035fd75). The per-launch containment landed in
the harness since — lib/scratch.py caps RLIMIT_NPROC per launch
(agents-harness#1349). What no check here owned was the live signal itself:
nobody read the account's process count against its cap while a turn ran, so
the storm grew to the table limit unseen. This file reads the real process
table and fails at a share of the cap with the cause named — the account's
process count (a storm from any source, .hive scratch included), and this run's
own descendants (a runaway fork loop in this repo's test code).

The shares: half the cap for the account, a tenth for one run's tree. A storm
that fills the table in minutes passes both long before it fills; a busy-but-
healthy machine sits far under them (the incident Mac idled near a tenth of
its cap with RC sessions open). Both reads are of the live system — the same
outcome-probe stance as temp_hygiene_test.py (the run leaves no temp state)
and loopback_guard_test.py (the run reports what the machine refuses), for the
process table instead of files and sockets.
"""
import os
import resource
import subprocess
import sys

import pytest

from support import HANG_TIMEOUT, wait_until

# The shares of the per-user cap that name a storm; overridable the way the
# shared hang bound is, for a host that legitimately sits higher.
USER_SHARE = float(os.environ.get("CCMORPH_PROC_USER_SHARE", "0.5"))
RUN_SHARE = float(os.environ.get("CCMORPH_PROC_RUN_SHARE", "0.1"))


def _ps(rows, *selectors):
    """`rows` for every live process, trying the repo's two spellings: procps'
    `-A` first, BSD `-ax` if it printed nothing (the same pair the claude stub
    in launch_env_rules_test.py asks). One bounded read per attempt."""
    for flags in (["-A"], ["-ax"]):
        proc = subprocess.run(["ps", *flags, *selectors, "-o", rows],
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)
        if proc.stdout.strip():
            return proc.stdout
    raise RuntimeError("ps printed no process table")


def _pid_rows(text):
    """The pids of `ps -o pid=` output; blank and unparsable lines count as no row."""
    return [int(line) for line in text.split() if line.strip().isdigit()]


def _pair_rows(text):
    """(pid, ppid) for each `ps -o pid=,ppid=` row whose two fields are numbers."""
    pairs = []
    for line in text.splitlines():
        fields = line.split()
        if len(fields) == 2 and all(f.isdigit() for f in fields):
            pairs.append((int(fields[0]), int(fields[1])))
    return pairs


def user_process_count():
    """How many processes this account holds right now, selected by REAL uid.
    The kernel charges the fork — RLIMIT_NPROC and kern.maxprocperuid's refusal
    alike — against the real uid, while `ps -u` selects by EFFECTIVE uid: a
    process whose ids differ would hide from the very limit it spends. `ps -U`
    selects by real uid on procps and BSD alike, asked ALONE: a scope flag
    added to it is a second selection, and ps ORs selections, so
    `ps -A -U <uid>` printed the whole table (measured: 238 rows for 93),
    which here would read every other account's processes as a storm of this
    one's."""
    proc = subprocess.run(["ps", "-U", str(os.getuid()), "-o", "pid="],
                          capture_output=True, text=True, timeout=HANG_TIMEOUT)
    if not proc.stdout.strip():
        raise RuntimeError(f"ps printed no process table: {proc.stderr.strip()}")
    return len(_pid_rows(proc.stdout))


def process_parent_pairs():
    """The live table as (pid, ppid) rows."""
    return _pair_rows(_ps("pid=,ppid="))


def choose_cap(caps):
    """The binding per-user bound: the lowest cap with its source, or None when
    nothing bounds the account. A fork stops at the lower of the kernel limit
    and the shell's RLIMIT_NPROC — which is where the harness cap bites."""
    return min(caps) if caps else None


def per_user_cap():
    """(limit, source) for this account's process count, or None when no finite
    bound applies. On macOS the kernel refuses the fork at kern.maxprocperuid
    and RLIMIT_NPROC mirrors it (or sits lower, when the harness caps it); on
    Linux RLIMIT_NPROC is the per-user bound. Unlimited everywhere means the
    signal has no cap to measure against here."""
    caps = []
    soft = resource.getrlimit(resource.RLIMIT_NPROC)[0]
    if soft != resource.RLIM_INFINITY:
        caps.append((soft, "RLIMIT_NPROC"))
    if sys.platform == "darwin":
        proc = subprocess.run(["sysctl", "-n", "kern.maxprocperuid"],
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)
        value = proc.stdout.strip()
        if proc.returncode == 0 and value.isdigit():
            caps.append((int(value), "kern.maxprocperuid"))
    return choose_cap(caps)


def descendants(pairs, root):
    """Every pid under `root` in the (pid, ppid) rows, as a set. A pid whose
    parent is not in the table hangs off the table's edge, not off the walk,
    and the seen-set makes a cycle cost a skip, never a spin."""
    children = {}
    for pid, ppid in pairs:
        children.setdefault(ppid, []).append(pid)
    seen, stack = set(), [root]
    while stack:
        for kid in children.get(stack.pop(), ()):
            if kid not in seen:
                seen.add(kid)
                stack.append(kid)
    return seen


def over_share(count, limit, share):
    """The one verdict: red once the count passes the share of the cap.
    Exactly at the share is green — a storm is named by passing it, not by
    standing on it."""
    return count > limit * share


def _cap_or_skip():
    cap = per_user_cap()
    if cap is None:
        pytest.skip("no finite per-user process cap here (RLIMIT_NPROC unlimited, "
                    "no kern.maxprocperuid): GH-919's signal needs a cap to pass")
    return cap


def test_the_account_stays_below_a_share_of_its_process_cap():
    """The storm probe proper: while this suite runs on the machine, the
    account's live process count must sit below USER_SHARE of its cap. The
    incident grew the count from idle to 8000/8000 in minutes while this very
    suite was the thing it competed with; sampled here, it goes red at the
    share with the runaway named, before the table fills and other sessions
    die. A forked ps that cannot even run proves the fullness itself: the
    OSError stands."""
    limit, source = _cap_or_skip()
    count = user_process_count()
    assert not over_share(count, limit, USER_SHARE), (
        f"{count} processes held by real uid {os.getuid()} — past {USER_SHARE:.0%} "
        f"of the {source} cap of {limit} (GH-919: a fork storm filled the table "
        f"to 8000 and killed the sessions beside it). Name the runaway: "
        f"ps -U {os.getuid()} -o comm= | sort | uniq -c | sort -rn | head "
        f"— then kill it; a Hive turn's own launches are capped since "
        f"(lib/scratch.py, agents-harness#1349).")


def run_tree_pressure(limit, share, source):
    """The run-tree check's live read, at call time: the failure message when
    this process's OWN descendant tree is past the share of the cap, or None
    while it sits under. The walk is rooted at os.getpid(), so one xdist
    worker never weighs another's tree; a witness can ask this same function
    while its own burst is alive, which is the only moment descendants exist.
    `source` only names the cap in the message (the live cap's source, or an
    injected one)."""
    pairs = process_parent_pairs()
    tree = descendants(pairs, os.getpid())
    if not over_share(len(tree), limit, share):
        return None
    return (f"this run holds {len(tree)} descendant processes — past {share:.0%} "
            f"of {source} of {limit}: a fork loop in the suite is not reaping "
            f"its children (GH-919). Its widest children, with their subtree "
            f"sizes: {_widest_children(pairs)}")


def test_the_run_holds_only_a_share_of_the_process_cap():
    """The attributable half: this run's own descendant tree stays below
    RUN_SHARE of the cap, so a runaway fork loop inside this repo's test code
    is named as itself — by its tree, whatever else the machine holds. Each
    xdist worker asks about its own tree; the others' processes are not this
    run's descendants and never weigh here. Both verdicts of this same live
    read are witnessed against a real live tree in
    test_a_burst_of_children_that_never_exit_is_seen_rising_and_clearing:
    an idle worker measuring itself proves only that an idle worker is idle."""
    limit, source = _cap_or_skip()
    message = run_tree_pressure(limit, RUN_SHARE, f"the {source} cap")
    assert message is None, message


def _widest_children(pairs, top=5):
    """The run's child pids with their subtree sizes, widest first — the pid a
    reader starts `ps -p <pid> -o pid,ppid,etime,args` from."""
    root = os.getpid()
    children = {}
    for pid, ppid in pairs:
        children.setdefault(ppid, []).append(pid)
    widest = sorted(((pid, len(descendants(pairs, pid)) + 1)
                     for pid in children.get(root, ())), key=lambda t: -t[1])
    return ", ".join(f"pid {pid} ({size})" for pid, size in widest[:top]) or "none"


@pytest.mark.parametrize("count, limit, share, red", [
    pytest.param(999, 2000, 0.5, False, id="well-under"),
    pytest.param(1000, 2000, 0.5, False, id="exactly-at-the-share-is-green"),
    pytest.param(1001, 2000, 0.5, True, id="one-past-is-red"),
    pytest.param(8000, 8000, 0.5, True, id="the-incident-was-red"),
    pytest.param(0, 0, 0.5, False, id="no-cap-no-storm"),
])
def test_the_share_verdict_crosses_at_the_share(count, limit, share, red):
    assert over_share(count, limit, share) is red


@pytest.mark.parametrize("caps, expected", [
    pytest.param([], None, id="nothing-bounds-it"),
    pytest.param([(8000, "kern.maxprocperuid")], (8000, "kern.maxprocperuid"),
                 id="kernel-cap-alone"),
    pytest.param([(2000, "RLIMIT_NPROC"), (8000, "kern.maxprocperuid")],
                 (2000, "RLIMIT_NPROC"), id="the-launch-cap-bites-before-the-kernel"),
    pytest.param([(8000, "RLIMIT_NPROC"), (8000, "kern.maxprocperuid")],
                 (8000, "RLIMIT_NPROC"), id="mirrored-limits-agree"),
])
def test_the_cap_is_the_lowest_bound_that_applies(caps, expected):
    assert choose_cap(caps) == expected


@pytest.mark.parametrize("pairs, root, expected", [
    pytest.param([(2, 1)], 1, {2}, id="one-child"),
    pytest.param([(2, 1), (3, 2), (4, 3)], 1, {2, 3, 4}, id="a-chain"),
    pytest.param([(2, 1), (3, 2), (4, 2), (5, 1)], 1, {2, 3, 4, 5}, id="a-branch"),
    pytest.param([(2, 1), (3, 9), (4, 3)], 1, {2},
                 id="a-parent-absent-from-the-table-detaches"),
    pytest.param([(2, 3), (3, 2)], 1, set(), id="a-cycle-away-from-the-root-walks-nothing"),
    pytest.param([(2, 1), (3, 4), (4, 3)], 1, {2},
                 id="a-cycle-below-but-not-under-the-root-walks-nothing"),
])
def test_the_walk_counts_the_tree_and_nothing_else(pairs, root, expected):
    assert descendants(pairs, root) == expected


@pytest.mark.parametrize("text, expected", [
    pytest.param("  12\n456\n\n", [12, 456], id="rows-and-blanks"),
    pytest.param("PID\nabc\n7\n", [7], id="a-header-or-garbage-is-no-row"),
])
def test_the_pid_parser_takes_only_rows(text, expected):
    assert _pid_rows(text) == expected


def test_the_account_count_asks_only_for_the_account():
    """The reader that scopes with `-U` must not add a scope flag: ps ORs
    different selections, and `ps -A -U <uid>` — the shape this file's table
    reader uses for everything — printed the whole table for the account
    (measured 200 rows for 50 processes), reading every other account's
    processes as this one's storm. The pin: count the account's rows by their
    REAL uid column in one full-table read, and the account reader must answer
    that number. The two reads sit back to back and the live table turns over
    between them, so the agreement is to a fifth of the count — the misread
    this pins was 4x the answer, not a turnover."""
    rows = _pair_rows(_ps("pid=,ruid="))
    total = len(rows)
    mine = sum(1 for _, ruid in rows if ruid == os.getuid())
    if mine >= total:
        pytest.skip("every process here belongs to this account: the union "
                    "misread and the true count answer the same number")
    count = user_process_count()
    assert abs(count - mine) * 5 <= max(mine, 1), (
        f"user_process_count said {count}; the table's own real-uid column "
        f"says {mine} of {total} — the reader is counting rows it did not ask "
        f"for")


def test_the_account_count_selects_by_real_uid(monkeypatch):
    """The fork charge (RLIMIT_NPROC / kern.maxprocperuid) is spent against
    the REAL uid, while `ps -u` selects by EFFECTIVE uid — the two counts
    differed on the review host (98 real vs 94 effective in one snapshot), and
    a probe on `-u` passes while the kernel is already refusing the charge.
    The fake table answers `-U` by the real-uid column and `-u` by the
    effective one, the semantics procps and BSD document; ten rows are this
    account's by real uid and only three by effective. A reader on `-u`
    answers 3 and hides the pressure; the count must be 10."""
    from types import SimpleNamespace
    real = os.getuid()
    other = real + 1
    rows = [(1000 + i, real, real if i < 3 else other) for i in range(10)]

    def fake_ps(argv, **_kw):
        columns = argv[argv.index("-o") + 1].replace("=", "").split(",")
        selected = rows
        for flag, col in (("-U", 1), ("-u", 2)):
            if flag in argv:
                selected = [r for r in selected if r[col] == int(argv[argv.index(flag) + 1])]
        field = {"pid": 0, "ruid": 1, "uid": 2, "euid": 2}
        out = "".join(" ".join(str(r[field[c]]) for c in columns) + "\n" for r in selected)
        return SimpleNamespace(returncode=0, stdout=out, stderr="")

    monkeypatch.setattr(subprocess, "run", fake_ps)
    assert user_process_count() == 10, (
        "a reader answering the effective-uid count hides the processes that "
        "spend the real-uid fork charge")


# The detection witness. GH-919's lesson as REVIEW.md reads a "test that cannot
# fail": a counter nobody watched rise proves nothing. These children are real
# processes that do NOT exit on their own — the storm's shape — bounded to
# BURST_CHILDREN and reaped by their own parent, which closes their blocking
# pipe on TERM and waits for each one (the fake_adapter contract: whatever
# makes a child is what puts it away, even over a kill). And the parent notices
# the TEST's death on its own: an xdist timeout's thread method ends a worker
# with os._exit, which runs no finally (tests/conftest.py, #892), so the parent
# blocks on a pipe the worker holds as the only writer — worker gone, EOF
# arrives wherever the test's own code was, and the parent releases and reaps
# its children without being told.
BURST_CHILDREN = 40

BURST = """
import os, signal, sys
n, watch = int(sys.argv[1]), int(sys.argv[2])
r, w = os.pipe()
def stop(*_):
    os.close(w)                       # EOF: every child leaves on its own
    while True:
        try:
            os.wait()
        except ChildProcessError:
            break                     # every child reaped before this parent exits
    os._exit(0)
signal.signal(signal.SIGTERM, stop)   # installed before the forks: no orphan window
kids = []
for _ in range(n):
    pid = os.fork()
    if pid == 0:
        os.close(w)
        os.close(watch)               # hygiene: a child has no use for the watch;
        os.read(r, 1)                 # EOF on it would need the worker's WRITE end
        os._exit(0)                   # gone, which no burst process ever holds
    kids.append(pid)
os.close(r)
print(' '.join([str(os.getpid())] + [str(k) for k in kids]), flush=True)
os.read(watch, 1)                     # the worker's end: EOF when it exits, however abruptly
stop()
"""


QUIET_READS = 4


def _count_reads(pick, reads=QUIET_READS):
    """The account count across consecutive reads, `pick`ed to an extreme.

    One read is a snapshot of a table the rest of the machine keeps churning.
    A full-suite gate run — five concurrent suites and every xdist worker
    spawning shells — showed the witness red once beside healthy churn: a
    lone baseline caught another job's transient spike (or a lone during-read
    a dip), and the burst's margin over the threshold is one process. The
    extreme of a few reads is the closest the live table has to a steady
    number, and on a quiet host it equals the single read. The reads need no
    sleep between them: each is a full-table `ps`, tens of ms on a loaded
    machine — the measuring itself is the gap, no spelled-out budget.
    """
    seen = [user_process_count()]
    for _ in range(reads - 1):
        seen.append(user_process_count())
    return pick(seen)


def test_a_burst_of_children_that_never_exit_is_seen_rising_and_clearing():
    base = descendants(process_parent_pairs(), os.getpid())
    # The bar the burst must clear is the LOWEST quiet read of the account: a
    # spike from a concurrent job must not raise it against us.
    base_user = _count_reads(min)
    # The parent holds the READ end of `watch`; only this worker holds a write
    # end, so its exit — normal, terminated, or an os._exit that skips every
    # cleanup in this file — is EOF, and the burst puts itself away.
    watch_r, watch_w = os.pipe()
    os.set_inheritable(watch_r, True)
    proc = subprocess.Popen([sys.executable, "-c", BURST, str(BURST_CHILDREN), str(watch_r)],
                            stdout=subprocess.PIPE, text=True, pass_fds=(watch_r,))
    os.close(watch_r)                 # only the burst parent may hold it now
    observed = [int(word) for word in proc.stdout.readline().split()]
    try:
        assert observed, (
            "the burst parent died before naming its processes: nothing can be "
            "verified gone")
        wanted = len(base) + len(observed)  # the burst parent plus its children

        def risen():
            return len(descendants(process_parent_pairs(), os.getpid())) >= wanted

        assert wait_until(risen, HANG_TIMEOUT), (
            f"the tree counter never reached {wanted} after forking a bounded "
            f"burst: descendants are not counted and the live probes could never "
            f"see a fork storm")
        during = len(descendants(process_parent_pairs(), os.getpid()))
        # The HIGH read while the burst is provably alive: a dip from the
        # surrounding churn must not hide our own 41 processes.
        during_user = _count_reads(max)

        # The run-tree check's OWN live read, run while the descendants exist —
        # the only moment they ever do. Over an injected cap it must name this
        # tree red and blame the burst parent's own subtree (worker isolation:
        # its widest child is this worker's child, not a sibling worker's);
        # under a far cap the same live read answers None beside the same burst.
        injected = during + 1
        message = run_tree_pressure(injected, RUN_SHARE, "the injected cap")
        assert message and "GH-919" in message and f"pid {proc.pid}" in message, (
            f"the live run-tree read, asked while {during} descendants were "
            f"alive over a threshold of {injected * RUN_SHARE:.1f}, answered: {message!r}")
        assert run_tree_pressure(int(during / RUN_SHARE) + BURST_CHILDREN,
                                 RUN_SHARE, "a far cap") is None, (
            "the same live read, under its share beside the same burst, must "
            "answer None")
    finally:
        if proc.poll() is None:
            proc.terminate()
        proc.wait(timeout=HANG_TIMEOUT)
        os.close(watch_w)

    # Cleanup is verified against the OBSERVED burst pids, not a shrinking
    # tree: a process reparented out of this tree when its parent died leaves
    # the tree quiet while still holding a slot of the table.
    def gone(pid):
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return True
        return False

    assert wait_until(lambda: all(gone(pid) for pid in observed), HANG_TIMEOUT), (
        f"a burst process outlived the burst: {observed} — still alive: "
        f"{[pid for pid in observed if not gone(pid)]} (the orphan shape GH-330 "
        f"and GH-919 are both made of)")

    # The account counter saw the storm, judged only now that both sides are
    # quiet: the burst's HIGH read must beat the LOWEST quiet read either side
    # of it (the table trends while a gate runs, so the after-burst reading
    # belongs in the bar too) by the burst's own size. A counter that misses
    # processes outside its own tree — the GH-919 blindness this probe exists
    # to catch — moves 0 and fails here.
    base_after = _count_reads(min)
    assert during_user >= min(base_user, base_after) + BURST_CHILDREN, (
        f"the account counter moved {during_user - min(base_user, base_after)} "
        f"processes for {len(observed)} provably-alive new ones: it could "
        f"never see the GH-919 storm either")
    message = run_tree_pressure(during + 1, RUN_SHARE, "the burst's own cap")
    assert message is None, message


# The abrupt-death witness, portable to every host that runs the suite. The
# review's repro forced this shape on Linux (a forked worker os._exit-ing over a
# live burst, the burst found through prctl(PR_SET_CHILD_SUBREAPER) and
# /proc/self/task/*/children — both Linux-only, so on the macOS host that
# reviews this the repro command itself dies at its line 3, `dlsym prctl: symbol
# not found`, before it can read this file). The property it pins is not
# Linux-shaped, so the repo pins it without those two Linux handles: a WORKER
# starts the burst, sees every burst process alive, and then os._exit-s holding
# the watch's only write end — the xdist timeout's own verdict (conftest.py,
# #892), skipping every teardown in this file — and the cleanup is verified
# against the observed pids with kill-polls, which see a process however far it
# has been reparented.
ABRUPT_CHILDREN = 4

WORKER = """
import os, subprocess, sys
n, burst_src = int(sys.argv[1]), sys.argv[2]
watch_r, watch_w = os.pipe()                     # watch_w stays only in this worker
os.set_inheritable(watch_r, True)
burst = subprocess.Popen([sys.executable, "-c", burst_src, str(n), str(watch_r)],
                         stdout=subprocess.PIPE, text=True)
os.close(watch_r)                                # the burst is its sole holder now
line = burst.stdout.readline()                   # the burst's own report of its pids
print(line.strip(), flush=True)                  # flushed: os._exit below flushes nothing
pids = [int(word) for word in line.split()]
if not pids:
    os._exit(18)                                 # the burst died before reporting
for pid in pids:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        os._exit(17)                             # a burst process died on its own
os._exit(19)                                     # every burst process alive, mid-burst,
"""                                              # and this worker now dies the timeout's death


def test_the_burst_is_put_away_when_its_worker_dies_abruptly():
    """An xdist timeout ends its worker with os._exit, which runs no finally;
    a burst that only cleaned up in the test's teardown would outlive the
    worker forever (and a shrinking descendant tree could not see it: the
    reparenting takes it out of the tree while it keeps its slot of the
    table). Here the worker dies that death on purpose, over a live burst,
    and every process the burst reported must still be gone."""
    proc = subprocess.Popen([sys.executable, "-c", WORKER, str(ABRUPT_CHILDREN), BURST],
                            stdout=subprocess.PIPE, text=True)
    observed = [int(word) for word in proc.stdout.readline().split()]
    code = proc.wait(timeout=HANG_TIMEOUT)
    assert code == 19, (
        f"the worker left with {code} instead of os._exit over a live burst "
        f"(17: a burst process died on its own — the burst is not the shape "
        f"under test; 18: the burst died before reporting its pids)")
    assert observed, "the burst reported no pids: nothing can be verified gone"

    def gone(pid):
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return True
        return False

    assert wait_until(lambda: all(gone(pid) for pid in observed), HANG_TIMEOUT), (
        f"a burst process outlived its abruptly-dead worker: of the observed "
        f"{observed}, still alive: {[pid for pid in observed if not gone(pid)]} "
        f"— the owner did not notice the worker's exit, or failed to release "
        f"and reap (GH-919)")
