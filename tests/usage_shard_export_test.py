"""The ccusage export over a store sharded by UTC day folder (#227): a ranged run
opens only the folders its range can reach, beside the flat rows still at the top."""
import hashlib
import json
import pathlib
import sys
import time

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from usage_export_test import local, read_day, receipt, ue, write  # noqa: E402


@pytest.fixture(autouse=True)
def hong_kong(monkeypatch):
    """Days are local days; pin the zone so the day boundaries below do not move with the host."""
    monkeypatch.setenv("TZ", "Asia/Hong_Kong")
    time.tzset()
    yield
    monkeypatch.undo()
    time.tzset()


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
