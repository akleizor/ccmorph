"""The ccusage export over a store sharded by UTC day folder (#227): a ranged run
opens only the folders its range can reach, beside the flat rows still at the top."""
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
