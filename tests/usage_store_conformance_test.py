#!/usr/bin/env python3
"""One damaged store, four readers, one answer (#654, #804).

The store's rules — which rows merge, which rows are skipped, which damage is
a refusal — were hand-spelled per reader, and the #642 landing let them
disagree: one store answered three month totals, or died on a day another
reader reported. This fixture carries every kind of damage the writers create
(a zero-byte row, a truncated row, a NaN-cost overlay pair, an orphan index
beside its folder, and unreadable files: the root-owned store lock one sudo'd
compaction leaves, which every reader opens, and a stray file in a day folder
that is no receipt), runs each reader on its own copy,
and requires the same priced total and the same unpriced count from all
three. It is the kind-catch for the next store-format change: a reader that
gains a private rule fails here instead of in a person's month.

A receipt that is still there but cannot be read — mode 0, or a FIFO left
under a receipt's name — is the damage no reader can price, so the second test
adds one to the same store and asks all three for the same answer: each skips
it, names it, and totals every other receipt — $9.00 and one unpriced turn,
the same as without it — never a wait on the FIFO's open. Each keeps its own
exit: the report is a reading and goes on (exit 0, the pre-#537 reading); quota
(#545) and the export (#583) still fail the run, quota carrying the total of
the rest beside its reason and the export writing nothing over the export
already there. A receipt gone between listing and read is the store's own
relocation, which all three skip.

The compactor is the fourth reader of the same fixture (#804): it reaches the
store through its own paths — a recovery read wrapped around the archive
reader — so it gains damage under the archive names, a FIFO at a closed day's
`<day>.jsonl.idx` that no windowed reader's window holds, and a closed archived
day whose folder holds a receipt as a bare JSON scalar. It must not hang, must
not lose a row, and every reader must still answer the fixture's one answer.
"""
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys

import pytest

from support import CORE, HANG_TIMEOUT, needs_shim

pytestmark = needs_shim

EXPORTER = pathlib.Path(__file__).resolve().parent.parent / "share/usage-export"

# One instant for every fixture row, so every reader's window (quota's month,
# report's seven days, the export's whole history) holds the same rows on any
# calendar day the test runs.
NOW = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
STAMP = NOW.strftime("%Y-%m-%dT%H:%M:%SZ")
TODAY = NOW.strftime("%Y-%m-%d")
TOMORROW = (NOW + dt.timedelta(days=1)).strftime("%Y-%m-%d")


def receipt(response_id, turn, cost, stamp=STAMP):
    """One row every reader counts. `cost` is written as given, so a float NaN
    becomes the bare token the Go readers normalize and the exporter parses."""
    return {
        "schema": 1, "response_id": response_id, "turn_id": turn,
        "response_model": "claude-opus-5-5", "profile": "ledger",
        "timestamp": stamp, "complete": True,
        "usage": {"input_tokens": 10, "output_tokens": 5, "cost": cost},
    }


def write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(data, dict):
        data = json.dumps(data)
    path.write_text(data if isinstance(data, str) else data.decode())


def archive(root, day, rows):
    """Publish day's archive exactly as shim.writeUsageArchiveWithPrevious does:
    one JSON line per row, names sorted, and the hash that binds the index."""
    rows = sorted(rows, key=lambda row: row[0])
    data = "".join(json.dumps(row, allow_nan=True) + "\n" for _, row in rows)
    write(root / (day + ".jsonl"), data)
    write(root / (day + ".jsonl.idx"), json.dumps({
        "v": 1,
        "sha256": hashlib.sha256(data.encode()).hexdigest(),
        # An older stamp than any live overlay, so the overlay is the newer row.
        "files": [{"name": name, "mtime_ns": 1} for name, _ in rows],
    }))


def build_store(root):
    """The fixture store under root/ccmorph/usage, one copy per reader (quota's
    maintenance may rewrite the store it reads). Returns the usage directory."""
    usage = root / "ccmorph" / "usage"
    day = usage / TODAY
    day.mkdir(parents=True)
    write(day / "good1.json", receipt("good1", "t-good1", 5))
    write(day / "zero.json", "")
    write(day / "trunc.json", '{"response_id":"truncated"')
    write(day / "nan.json", receipt("nan", "t-nan", float("nan")))
    archive(usage, TODAY, [
        ("good2.json", receipt("good2", "t-good2", 4)),
        ("nan.json", receipt("nan", "t-nan", 1)),
    ])
    # An index over no content beside its folder: the first publication died
    # between the two writes. The folder holds the day whole, so every reader
    # answers from it. A future day so no windowed reader stops on its absence
    # of rows; the export, which owns every day, still walks it.
    future = usage / TOMORROW
    future.mkdir(parents=True)
    write(usage / (TOMORROW + ".jsonl.idx"), json.dumps({
        "v": 1, "sha256": hashlib.sha256(b"").hexdigest(), "files": []}))
    # A store lock this run cannot open: the root-owned .move.lock one sudo'd
    # compaction leaves. Every reader opens it, so each must clear or bypass
    # it and still answer the same total (#654). Root reads mode 0, so there
    # the file is merely present.
    lock = usage / ".move.lock"
    lock.write_text("")
    lock.chmod(0)
    # An unreadable file in a day folder that is no receipt (a root-owned note
    # or editor leftover): no reader counts it, so none may trip on it.
    stray = day / "notes.txt"
    stray.write_text("not a receipt")
    stray.chmod(0)
    return usage


def env_for(root):
    return {**os.environ, "XDG_STATE_HOME": str(root),
            "HOME": str(root / "home")}


def run(cmd, root, stdin=None):
    done = subprocess.run(cmd, input=stdin, env=env_for(root),
                          capture_output=True, text=True, timeout=HANG_TIMEOUT)
    output = done.stdout + done.stderr
    assert "Traceback" not in output, output
    assert "panic:" not in output, output
    return done


def quota(root):
    done = run([str(CORE), "quota", "--json"], root, stdin="ledger\tgateway\t\t\n")
    payload = json.loads(done.stdout or "{}")
    entry = next(e for e in payload["profiles"] if e["profile"] == "ledger")
    return done, entry


def report(root):
    return run([str(CORE), "shim", "report", "--days", "7"], root)


def export(tmp_path, root):
    usage = root / "ccmorph" / "usage"
    return run([sys.executable, str(EXPORTER), "--source", str(usage),
                "--output", str(tmp_path / "out")], root)


def compact(root):
    return run([str(CORE), "usage-compact"], root)


def add_fifo_archive_index(usage):
    """A closed day named only by a FIFO at `<day>.jsonl.idx`: the folder is
    there and empty, no `<day>.jsonl` beside it. The day is
    min(this month's first - 2, today - 8): quota names its month window from
    a local midnight stepped as a UTC day (east of UTC that is the previous
    month's last day) and the report opens its window's first day too, so the
    day stands one clear day outside each; the compactor still folds it, as it
    folds every closed day, and the export — which owns every day — answers
    rows={} from its empty folder without opening the index. Every guarded
    store read answers `not a regular file` for it; the recovery read 8b74a030
    kept raw opens the name directly, which is the hang (#804). Returns the
    day's name."""
    today = NOW.date()
    day = min(today.replace(day=1) - dt.timedelta(days=2), today - dt.timedelta(days=8))
    (usage / day.isoformat()).mkdir()
    os.mkfifo(usage / (day.isoformat() + ".jsonl.idx"), 0o600)
    return day.isoformat()


def test_all_three_readers_agree_on_a_damaged_store(tmp_path):
    """Zero-byte and truncated rows are skipped, the NaN overlay is the one turn
    its archived twin names (unpriced, not two turns), the orphan index answers
    from its folder, and the lock no reader can open is cleared or bypassed:
    $9.00 priced and one unpriced turn everywhere."""
    roots = {name: tmp_path / name for name in ("quota", "report", "export")}
    for root in roots.values():
        usage = build_store(root)
        if os.geteuid() != 0:
            assert not os.access(usage / ".move.lock", os.R_OK)
            assert not os.access(usage / TODAY / "notes.txt", os.R_OK)

    done, entry = quota(roots["quota"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert (entry["spend"]["month_usd"], entry["spend"]["month_turns"],
            entry["spend"]["unpriced_turns"]) == (9, 3, 1), entry

    done = report(roots["report"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $9.00" in done.stdout, done.stdout
    assert "unknown spend 1 rows" in done.stdout, done.stdout

    done = export(tmp_path, roots["export"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "Exported 2 turns" in done.stdout, done.stdout
    assert "the gateway billed, $9.00" in done.stdout, done.stdout
    assert "1 turn unpriced" in done.stdout, done.stdout


@pytest.mark.parametrize("damage", ["mode-0", "fifo"])
def test_all_three_readers_agree_on_an_unreadable_receipt(tmp_path, damage):
    """The same damaged store plus one receipt this run cannot read: quota,
    report and the export each skip it, name it, and give the same total of
    every other receipt — $9.00 and one unpriced turn — as plain words, never a
    traceback, never a hang."""
    if damage == "mode-0" and os.geteuid() == 0:
        pytest.skip("root reads mode-0 receipts")
    roots = {name: tmp_path / name for name in ("quota", "report", "export")}
    for root in roots.values():
        usage = build_store(root)
        unreadable = usage / TODAY / "unreadable.json"
        if damage == "fifo":
            os.mkfifo(unreadable, 0o600)
        else:
            write(unreadable, receipt("unreadable", "t-unreadable", 7))
            unreadable.chmod(0)

    done, entry = quota(roots["quota"])
    assert done.returncode == 1, done.stdout + done.stderr
    assert (entry["spend"]["month_usd"], entry["spend"]["month_turns"],
            entry["spend"]["unpriced_turns"], entry["spend"]["unreadable_receipts"]) == (9, 3, 1, 1), entry
    assert re.search("not readable", entry["error"]) and "unreadable.json" in entry["error"], entry

    done = report(roots["report"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $9.00" in done.stdout, done.stdout
    assert "unknown spend 1 rows" in done.stdout, done.stdout
    assert "not readable 1 receipt(s), skipped" in done.stdout, done.stdout
    assert "unreadable.json: " in done.stderr, done.stderr

    done = export(tmp_path, roots["export"])
    assert done.returncode == 1, done.stdout + done.stderr
    assert "the gateway billed, $9.00" in done.stdout, done.stdout
    assert "1 turn unpriced" in done.stdout, done.stdout
    assert "Nothing written" in done.stdout, done.stdout
    assert "unreadable.json: " in done.stderr, done.stderr
    assert "[Errno" not in done.stderr, done.stderr


@pytest.mark.parametrize("damage", ["none", "mode-0"])
def test_a_response_under_two_names_counts_by_its_day_copy_in_every_reader(tmp_path, damage):
    """A response the store holds under two names — a flat copy a move left and
    its day folder's — counts once, by the day folder's copy, in quota, the
    report and the export alike. Skipping an unrelated receipt this run cannot
    read must not change which copy counts: the readable total stays $9.00 and
    one unpriced turn, the same as without it (#654). The flat name sorts
    before any day name, so a reader that counts in name order is caught."""
    if damage == "mode-0" and os.geteuid() == 0:
        pytest.skip("root reads mode-0 receipts")
    roots = {name: tmp_path / name for name in ("quota", "report", "export")}
    for root in roots.values():
        usage = build_store(root)
        # The same response and turn as the day folder's good1.json ($5), at $1.
        write(usage / "0flat.json", receipt("good1", "t-good1", 1))
        if damage == "mode-0":
            unreadable = usage / TODAY / "unreadable.json"
            write(unreadable, receipt("unreadable", "t-unreadable", 7))
            unreadable.chmod(0)
    failed = 1 if damage == "mode-0" else 0

    done, entry = quota(roots["quota"])
    assert done.returncode == failed, done.stdout + done.stderr
    assert (entry["spend"]["month_usd"], entry["spend"]["month_turns"],
            entry["spend"]["unpriced_turns"]) == (9, 3, 1), entry

    done = report(roots["report"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $9.00" in done.stdout, done.stdout
    assert "unknown spend 1 rows" in done.stdout, done.stdout

    done = export(tmp_path, roots["export"])
    assert done.returncode == failed, done.stdout + done.stderr
    assert "the gateway billed, $9.00" in done.stdout, done.stdout
    assert "1 turn unpriced" in done.stdout, done.stdout


def test_all_four_readers_agree_on_a_fifo_archive_index(tmp_path):
    """The same damaged store plus a damage under the archive names: a FIFO at a
    closed day's `<day>.jsonl.idx`, no archive, an empty folder. The day is
    outside every reader's window, so quota, the report and the export answer
    the fixture's $9.00 and one unpriced turn exactly as without it; the
    compactor — the store reader this fixture has never run — reaches it on its
    every-closed-day walk, and must name it as damage and leave it whole rather
    than block in the recovery read while it holds the store's exclusive lock
    (#804). The compacted store still answers $9.00: the compaction lost no row.
    """
    roots = {name: tmp_path / name for name in ("quota", "report", "export", "compact")}
    for name, root in roots.items():
        usage = build_store(root)
        day = add_fifo_archive_index(usage)  # same instant for every copy: same day
        if name == "compact":
            # usage-compact is the store's exclusive writer: a lock it cannot
            # open ends its run before its walk starts (the no-lock reading
            # that lets the three readers go on is not a compaction), so the
            # compactor's copy is the one whose lock it can open. Every other
            # byte of the fixture stands.
            (usage / ".move.lock").chmod(0o600)

    done, entry = quota(roots["quota"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert (entry["spend"]["month_usd"], entry["spend"]["month_turns"],
            entry["spend"]["unpriced_turns"]) == (9, 3, 1), entry

    done = report(roots["report"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $9.00" in done.stdout, done.stdout
    assert "unknown spend 1 rows" in done.stdout, done.stdout

    done = export(tmp_path, roots["export"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the gateway billed, $9.00" in done.stdout, done.stdout
    assert "1 turn unpriced" in done.stdout, done.stdout

    usage = roots["compact"] / "ccmorph" / "usage"
    done = compact(roots["compact"])
    assert done.returncode == 1, (done.stdout, done.stderr)
    assert f"day {day}" in done.stderr, done.stderr  # the damage is named as every other day's is
    assert "not a regular file" in done.stderr, done.stderr
    assert "compacted 0 usage day(s)" in done.stdout, done.stdout
    assert (usage / str(day)).is_dir() and (usage / f"{day}.jsonl.idx").exists(), "the day was not left whole"
    assert not (usage / f"{day}.jsonl").exists(), "a damaged day was published over"
    done = report(roots["compact"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $9.00" in done.stdout, done.stdout  # no row lost to the walk


def test_all_four_readers_agree_on_a_scalar_archived_row(tmp_path):
    """A closed archived day whose folder still holds `scalar.json` as a bare
    JSON `true` while its archive names it: the Go readers answer the pair "not
    one turn — keep both", count the archived row, and file the unreadable one
    under usage-dropped/; the export mirrored the merge by hand and called
    `.get()` on the scalar (#804). All four readers answer the one archived
    turn — $4.00 — with no traceback, whether or not a compaction ran first."""
    day = (NOW - dt.timedelta(days=3)).strftime("%Y-%m-%d")
    roots = {name: tmp_path / name for name in ("quota", "report", "export", "compact")}
    for root in roots.values():
        usage = root / "ccmorph" / "usage"
        folder = usage / day
        folder.mkdir(parents=True)
        write(folder / "scalar.json", "true")
        archive(usage, day, [("scalar.json", receipt("good2", "t-good2", 4))])

    if NOW.day >= 4:  # else the closed day is last month's and quota's month never reads it
        done, entry = quota(roots["quota"])
        assert done.returncode == 0, done.stdout + done.stderr
        assert (entry["spend"]["month_usd"], entry["spend"]["month_turns"],
                entry["spend"]["unpriced_turns"]) == (4, 1, 0), entry

    done = report(roots["report"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $4.00" in done.stdout, done.stdout

    done = export(tmp_path, roots["export"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the gateway billed, $4.00" in done.stdout, done.stdout
    assert "0 turns unpriced" in done.stdout, done.stdout

    usage = roots["compact"] / "ccmorph" / "usage"
    done = compact(roots["compact"])
    assert done.returncode == 0, (done.stdout, done.stderr)
    assert "compacted 1 usage day(s)" in done.stdout, done.stdout
    kept = list((roots["compact"] / "ccmorph" / "usage-dropped" / day).glob("*-scalar.json"))
    assert kept and kept[0].read_text().strip() == "true", "the unreadable row's bytes were not kept"
    done = report(roots["compact"])
    assert done.returncode == 0, done.stdout + done.stderr
    assert "total spend $4.00" in done.stdout, done.stdout  # the archived row survived its day