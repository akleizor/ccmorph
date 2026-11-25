"""The ccusage export over a store sharded by UTC day folder (#227): a ranged run
opens only the folders its range can reach, beside the flat rows still at the top."""
import fcntl
import hashlib
import json
import os
import pathlib
import sys
import time

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
# The export suite's helpers; the zone pin is conftest's hong_kong (#636).
from usage_export_test import local, read_day, receipt, ue, write  # noqa: E402

pytestmark = pytest.mark.usefixtures("hong_kong")


def test_the_shard_suite_runs_on_hong_kong_days():
    assert time.strftime("%z") == "+0800"


def test_receipts_sharded_by_day_folders_are_read_and_old_folders_skipped(tmp_path):
    """A ranged run reads the day folders at or after its first day beside the
    flat rows still at the top level, and never opens an older folder — the
    export fails on a malformed receipt it reads, so a run that succeeds with one
    sitting in an old folder proves that folder was never opened, while a run
    with no range, which owns every day, reads it and refuses."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    (rows / "2026-10-16").mkdir(parents=True)
    write(rows / "2026-10-16", "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00Z", cost=2))
    write(rows, "flat.json", receipt("f", "claude-opus-5-5", "2026-10-16T03:00:00Z", cost=1))
    (rows / "2026-10-01").mkdir()
    write(rows / "2026-10-01", "bad.json",
          {**receipt("b", "claude-opus-5-5", "2026-10-01T01:00:00Z"), "schema": 2})

    stats = ue.export(rows, output, local(2026, 10, 16))

    assert (stats["rows"], stats["usd"]) == (2, 3)
    assert len(read_day(output, "2026-10-16")) == 2

    with pytest.raises(ValueError, match="bad.json"):
        ue.export(rows, output)


def test_a_ranged_export_opens_no_folder_before_its_first_utc_day(tmp_path, monkeypatch):
    """West of UTC a local midnight is the same UTC day, so the first folder a
    receipt in range can sit in is that day — not the local date minus one. A
    malformed receipt one day earlier is out of range and must not be opened:
    the old bound read it and failed the whole export."""
    monkeypatch.setenv("TZ", "America/Los_Angeles")
    time.tzset()
    rows, output = tmp_path / "rows", tmp_path / "out"
    (rows / "2026-10-16").mkdir(parents=True)
    write(rows / "2026-10-16", "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T07:00:00Z", cost=2))
    # 2026-10-16 00:00 in Los Angeles is 07:00Z, so 2026-10-16 is the first UTC
    # day in range; a receipt under 2026-10-15 is older than the range.
    (rows / "2026-10-15").mkdir()
    write(rows / "2026-10-15", "bad.json",
          {**receipt("b", "claude-opus-5-5", "2026-10-15T23:59:59Z"), "schema": 2})

    stats = ue.export(rows, output, local(2026, 10, 16))

    assert (stats["rows"], stats["usd"]) == (1, 2)


def archive(root, day, rows, mtime_ns=0):
    data = b"".join(json.dumps(row).encode() + b"\n" for _, row in rows)
    path = root / (day + ".jsonl")
    path.write_bytes(data)
    path.with_suffix(".jsonl.idx").write_text(json.dumps({
        "v": 1, "sha256": hashlib.sha256(data).hexdigest(),
        "files": [{"name": name, "mtime_ns": mtime_ns} for name, _ in rows],
    }))


def test_mixed_archive_and_live_rows_keep_export_totals(tmp_path):
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    a = receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=0.1)
    b = receipt("b", "claude-opus-5-5", day + "T02:00:00Z", cost=0.2)
    archive(rows, day, [("a.json", a), ("b.json", b)])
    write(rows / day, "b.json", b)
    write(rows, "flat.json", receipt("f", "claude-opus-5-5", day + "T03:00:00Z", cost=0.3))

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"]) == (3, sum([0.1, 0.2, 0.3]))
    assert len(read_day(output, day)) == 3


@pytest.mark.parametrize("response_id, archived_time, expected", [
    ("a", 0, (1, 2)), ("b", 0, (2, 3)), ("a", 4102444800000000000, (1, 1)),
])
def test_late_live_row_replaces_only_its_archived_identity(tmp_path, response_id, archived_time, expected):
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    old = receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1)
    archive(rows, day, [("a.json", old)], archived_time)
    write(rows / day, "a.json", {**old, "response_id": response_id, "usage": {**old["usage"], "cost": 2}})

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"]) == expected


def test_missing_and_empty_legacy_turn_select_newest_receipt(tmp_path):
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    old = receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1)
    old.pop("turn_id", None)
    archive(rows, day, [("a.json", old)])
    write(rows / day, "a.json", {**old, "turn_id": "", "usage": {**old["usage"], "cost": 2}})

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"]) == (1, 2)


def test_interrupted_index_publication_keeps_prior_archive_readable(tmp_path):
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    rows.mkdir()
    archive(rows, day, [("a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1))])
    path = rows / (day + ".jsonl.idx")
    old = json.loads(path.read_text())
    path.write_text(json.dumps({"v": 1, "sha256": "0" * 64, "files": [], "previous": old}))

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"]) == (1, 1)


@pytest.mark.parametrize("damage", ["hash", "missing-jsonl"])
def test_invalid_archive_preserves_existing_export(tmp_path, damage):
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    rows.mkdir()
    archive(rows, day, [("a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1))])
    path = rows / (day + ".jsonl")
    if damage == "hash":
        path.write_bytes(b"{}\n")
    else:
        path.unlink()
    output.mkdir()
    canary = output / "canary"
    canary.write_text("unchanged")

    with pytest.raises(ValueError, match="archive"):
        ue.export(rows, output)

    assert canary.read_text() == "unchanged"
    assert not (output / "projects").exists()


def test_index_only_orphan_answers_from_the_folder(tmp_path):
    """A first publication killed between its index and its content leaves an
    index over no content; the folder holds that day whole, so the export reads
    the folder — the same heal the Go readers apply — instead of raising."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    write(rows / day, "a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=2))
    archive(rows, day, [("a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1))])
    (rows / (day + ".jsonl")).unlink()

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"]) == (1, 2)


@pytest.mark.skipif(os.geteuid() == 0, reason="root opens mode-0 files")
def test_export_reads_past_an_unopenable_move_lock_and_keeps_it(tmp_path):
    """A root-owned .move.lock left by a sudo'd compaction must not poison
    every export forever, and is never unlinked: a live compactor may hold that
    inode, and a replacement would stop the store's lock excluding anyone."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    write(rows / day, "a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1))
    lock = rows / ".move.lock"
    lock.write_bytes(b"")
    lock.chmod(0)
    before = lock.stat()
    stats = ue.export(rows, output)
    assert stats["rows"] == 1
    assert os.path.samestat(before, lock.stat())
    lock.chmod(0o600)


@pytest.mark.skipif(os.geteuid() == 0, reason="root opens mode-0 files")
def test_export_reads_without_a_lock_when_the_lock_is_stuck(tmp_path):
    """With the store directory read-only as well, the export still answers
    without a lock rather than failing every run forever."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    write(rows / day, "a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1))
    lock = rows / ".move.lock"
    lock.write_bytes(b"")
    lock.chmod(0)
    rows.chmod(0o500)
    try:
        stats = ue.export(rows, output)
    finally:
        rows.chmod(0o700)
    assert stats["rows"] == 1


def test_nonfinite_overlay_is_one_turn_priced_as_written(tmp_path):
    """One merge rule for all three readers (#654): a live row whose cost
    Python wrote as the NaN token is the same turn as its archived priced
    sibling — the newer row wins, and every reader gives that turn what its
    damaged-price rows already get: no dollars, counted unpriced. A reader
    that refuses to parse the token kept the pair as two rows and then
    dropped the damaged one, which is how one store came to answer three
    readers with three month totals."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    archive(rows, day, [("a.json", receipt("r", "claude-opus-5-5", f"{day}T01:00:00Z", cost=1, input_tokens=10))], mtime_ns=1)
    write(rows / day, "a.json", receipt("r", "claude-opus-5-5", f"{day}T01:00:00Z", cost=float("nan"), input_tokens=10))

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"], stats["unpriced"]) == (0, 0, 1)
    assert not (output / "projects" / "ledger" / f"{day}.jsonl").exists()


def test_a_truncated_overlay_skips_and_the_rest_of_the_year_exports(tmp_path):
    """One damaged row is not a gate on the export (#654): a live overlay whose
    JSON stops mid-token must skip — its archived sibling still answers, the
    next day's receipts still export — where the merge's unguarded pair of
    json.loads runs inside receipt_rows, so it raised at the consumer's `for`,
    outside the per-receipt handler, and a full-year run emitted nothing."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day, next_day = "2026-10-16", "2026-10-17"
    (rows / day).mkdir(parents=True)
    good = receipt("a", "claude-opus-5-5", f"{day}T01:00:00Z", cost=1)
    archive(rows, day, [("a.json", good)])
    (rows / day / "a.json").write_text('{"response_id":"a","turn_id":')
    (rows / next_day).mkdir()
    write(rows / next_day, "b.json", receipt("b", "claude-opus-5-5", f"{next_day}T01:00:00Z", cost=2))

    stats = ue.export(rows, output)

    assert (stats["rows"], stats["usd"]) == (2, 3)
    assert read_day(output, day)[0]["costUSD"] == 1
    assert read_day(output, next_day)[0]["costUSD"] == 2


def test_a_held_move_lock_refuses_the_export_instead_of_waiting(tmp_path):
    """An export queued behind compaction's exclusive hold is the hang INTENT 8
    forbids (#654): the shared lock is taken with LOCK_NB and a bounded retry,
    so a busy store is a named refusal inside the bound, never a wait. The
    pytest cap stands in for the bound here: an unbounded flock holds the test
    until it is killed."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = "2026-10-16"
    (rows / day).mkdir(parents=True)
    write(rows / day, "a.json", receipt("a", "claude-opus-5-5", day + "T01:00:00Z", cost=1))
    with open(rows / ".move.lock", "w") as held:
        fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
        with pytest.raises(ValueError, match="usage store busy"):
            ue.export(rows, output)
    assert ue.export(rows, output)["rows"] == 1
