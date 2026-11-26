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
    """How many processes this account holds right now. `ps -u <uid>` selects
    the account's processes on procps and BSD alike, terminals or none — and
    asks it ALONE: a scope flag added to it is a second selection, and ps ORs
    selections, so `ps -A -u <uid>` printed the whole table (measured: 238 rows
    for 93 processes), which here would read every other account's processes as
    a storm of this one's."""
    proc = subprocess.run(["ps", "-u", str(os.getuid()), "-o", "pid="],
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
        f"{count} processes held by uid {os.getuid()} — past {USER_SHARE:.0%} of "
        f"the {source} cap of {limit} (GH-919: a fork storm filled the table to "
        f"8000 and killed the sessions beside it). Name the runaway: "
        f"ps -u {os.getuid()} -o comm= | sort | uniq -c | sort -rn | head "
        f"— then kill it; a Hive turn's own launches are capped since "
        f"(lib/scratch.py, agents-harness#1349).")


def test_the_run_holds_only_a_share_of_the_process_cap():
    """The attributable half: this run's own descendant tree stays below
    RUN_SHARE of the cap, so a runaway fork loop inside this repo's test code
    is named as itself — by its tree, whatever else the machine holds. Each
    xdist worker asks about its own tree; the others' processes are not this
    run's descendants and never weigh here."""
    limit, source = _cap_or_skip()
    pairs = process_parent_pairs()
    tree = descendants(pairs, os.getpid())
    assert not over_share(len(tree), limit, RUN_SHARE), (
        f"this run holds {len(tree)} descendant processes — past {RUN_SHARE:.0%} "
        f"of the {source} cap of {limit}: a fork loop in the suite is not "
        f"reaping its children (GH-919). Its widest children, with their subtree "
        f"sizes: {_widest_children(pairs)}")


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
    """The reader that scopes with `-u` must not add a scope flag: ps ORs
    different selections, and `ps -A -u <uid>` — the shape this file's table
    reader uses for everything — printed the whole table for the account
    (measured 238 rows for 93 processes), reading every other account's
    processes as this one's storm. The pin: count the account's rows by uid
    in one full-table read, and the account reader must answer that number.
    The two reads sit back to back and the live table turns over between
    them, so the agreement is to a fifth of the count — the misread this
    pins was 2.5x the answer, not a turnover."""
    uids = _pid_rows(_ps("uid="))
    total = len(uids)
    mine = sum(1 for uid in uids if uid == os.getuid())
    if mine >= total:
        pytest.skip("every process here belongs to this account: the union "
                    "misread and the true count answer the same number")
    count = user_process_count()
    assert abs(count - mine) * 5 <= max(mine, 1), (
        f"user_process_count said {count}; the table's own uid column says "
        f"{mine} of {total} — the reader is counting rows it did not ask for")


# The detection witness. GH-919's lesson as REVIEW.md reads a "test that cannot
# fail": a counter nobody watched rise proves nothing. These children are real
# processes that do NOT exit on their own — the storm's shape — bounded to
# BURST_CHILDREN and reaped by their own parent, which closes their blocking
# pipe on TERM and waits for each one (the fake_adapter contract: whatever
# makes a child is what puts it away, even over a kill).
BURST_CHILDREN = 40

BURST = """
import os, signal, sys
n = int(sys.argv[1])
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
for _ in range(n):
    if os.fork() == 0:
        os.close(w)
        os.read(r, 1)                 # block until the parent closes the pipe
        os._exit(0)
os.close(r)
signal.pause()
"""


def test_a_burst_of_children_that_never_exit_is_seen_rising_and_clearing():
    base = descendants(process_parent_pairs(), os.getpid())
    base_user = user_process_count()
    proc = subprocess.Popen([sys.executable, "-c", BURST, str(BURST_CHILDREN)])
    try:
        wanted = len(base) + BURST_CHILDREN + 1  # the burst parent plus its children

        def risen():
            return len(descendants(process_parent_pairs(), os.getpid())) >= wanted

        assert wait_until(risen, HANG_TIMEOUT), (
            f"the tree counter never reached {wanted} after forking a bounded "
            f"burst: descendants are not counted and the live probes could never "
            f"see a fork storm")
        during = len(descendants(process_parent_pairs(), os.getpid()))
        assert user_process_count() >= base_user + BURST_CHILDREN, (
            "the account counter never moved for 40 new processes: it could "
            "never see the GH-919 storm either")

        # The two counters, verdicts included, cross exactly where the share says:
        # under before the burst, over it after, on the same injected cap.
        injected = during + 1
        assert not over_share(len(base), injected, RUN_SHARE)
        assert over_share(during, injected, RUN_SHARE)
    finally:
        if proc.poll() is None:
            proc.terminate()
        proc.wait(timeout=HANG_TIMEOUT)

    assert wait_until(lambda: len(descendants(process_parent_pairs(), os.getpid())) <= len(base),
                      HANG_TIMEOUT), (
        "a burst child outlived the burst: its parent died or failed to reap it — "
        "the orphan shape GH-330 and GH-919 are both made of")
    after = len(descendants(process_parent_pairs(), os.getpid()))
    assert not over_share(after, during + 1, RUN_SHARE)
