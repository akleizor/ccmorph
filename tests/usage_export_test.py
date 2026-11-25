"""The metadata-only ccusage export: one record per receipt, one folder per profile, damaged costs reported."""
import contextlib
import datetime as dt
import errno
import json
import os
import pathlib
import shlex
import shutil
import subprocess
import sys
import tempfile

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from rc_hosts_test import is_python_helper  # noqa: E402
from support import HANG_TIMEOUT, load_script  # noqa: E402

pytestmark = pytest.mark.usefixtures("hong_kong")

# The shim's own bound (costLimit in shim/sidecar.go, 2**53/100); shim/sidecar_test.go
# asserts the Go constant equals this literal, and the test below that the exporter's matches.
SHIM_COST_LIMIT = 2 ** 53 / 100

EXPORTER = pathlib.Path(__file__).parent.parent / "share/usage-export"
ue = load_script(EXPORTER, "usage_export")

ABSENT = object()


def local(year, month, day, hour=0):
    return dt.datetime(year, month, day, hour).astimezone()


def receipt(rid, model, stamp, cost=None, complete=True,
            requested=ABSENT, served=ABSENT, input_tokens=1_000_000, estimate=None):
    """One schema-1 receipt. `model` is the identity the client is answered with;
    `requested` and `served` are the shim's recorded ids, absent by default — the
    row shape a receipt had before served_model existed. Pass None for a null id."""
    usage = {"input_tokens": input_tokens}
    if cost is not None:
        usage["cost"] = cost
    if estimate is not None:
        usage["estimated_cost"] = estimate
    row = {"schema": 1, "response_id": rid, "response_model": model, "timestamp": stamp,
           "task": None, "attempt": 1, "profile": "ledger", "complete": complete, "usage": usage}
    if requested is not ABSENT:
        row["requested_model"] = requested
    if served is not ABSENT:
        row["served_model"] = served
    return row


def mapped_receipt(rid, stamp, cost=None):
    """The exact row shim writes for a profile model map: the picker alias was
    asked for with its [1m] marker, the gateway target answered, and the alias is the
    identity the client is told (asserted in gateway_shim_test.py)."""
    return receipt(rid, "claude-sonnet-5-5", stamp, cost=cost,
                   requested="claude-sonnet-5-5[1m]", served="gpt-6.1-sol")


def write(root, name, body, mtime=None):
    """Write one receipt. `mtime` pins the file's modification time, so a test of the
    receipt-time cutoff is not decided by when the suite happens to run."""
    path = root / name
    path.write_text(json.dumps(body))
    if mtime is not None:
        os.utime(path, (mtime.timestamp(), mtime.timestamp()))


def read_day_in(output, folder, day):
    path = output / "projects" / folder / f"{day}.jsonl"
    return [json.loads(l) for l in path.read_text().splitlines() if l]


def read_day(output, day):
    path = output / "projects" / "ledger" / f"{day}.jsonl"
    return [json.loads(l) for l in path.read_text().splitlines() if l]


def ccusage_stub(tmp_path):
    """A bin directory holding one executable named ccusage: the report asks PATH
    whether ccusage is installed (GH-843), so a test that pins the whole report
    pins this too — never the host's PATH, which decides the tail's spelling."""
    bin_dir = tmp_path / "ccusage-bin"
    bin_dir.mkdir(exist_ok=True)
    stub = bin_dir / "ccusage"
    if not stub.exists():
        stub.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(stub, 0o755)
    return bin_dir


@pytest.mark.parametrize("damage", [
    pytest.param({"cost": 10 ** 400}, id="float-overflowing-cost-issue-368"),
    pytest.param({"cost": 1e14}, id="over-limit-gateway-cost-issue-413"),
    pytest.param({"estimate": 10 ** 400}, id="float-overflowing-estimate-issue-68"),
    pytest.param({"estimate": 1e14}, id="over-limit-estimate-issue-68"),
    pytest.param({"cost": "12.50"}, id="non-numeric-cost-issue-68"),
    pytest.param({"estimate": "12.50"}, id="non-numeric-estimate-issue-68"),
    pytest.param({"cost": -0.01}, id="negative-cost-issue-68"),
    pytest.param({"estimate": -0.01}, id="negative-estimate-issue-68"),
])
def test_a_receipt_with_a_damaged_cost_is_counted_not_fatal(tmp_path, damage):
    """A stored price no writer would store — too large for a float, past the shim's
    COST_LIMIT, not a number at all, or negative — is damage after the fact, and the
    same damage takes the same outcome in either field: counted unpriced next to a
    good row, never exported and never fatal. #68: the guards used to sit on the
    billed cost alone, so the same damage in estimated_cost, the field #51 added,
    aborted the whole export."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00"))
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00", **damage))

    stats = ue.export(rows, output)

    assert stats["unpriced"] == 1 and stats["rows"] == 1
    assert [r["message"]["model"] for r in read_day(output, "2026-10-16")] == ["claude-opus-5-5"]


def test_two_over_limit_costs_total_a_finite_number_not_infinity(tmp_path):
    """Issue #413's total-side repro: two receipts whose costs are each individually
    fine as a float used to pass the per-row checks and push the exported total past
    the float range, so the summary's usd printed Infinity — a token no JSON parser
    accepts. Each over-limit cost is unpriced, so the total stays a finite number
    and a fine row still carries it."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "huge-a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1e308))
    write(rows, "huge-b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00", cost=1e308))
    write(rows, "fine.json", receipt("c", "claude-opus-5-5", "2026-10-16T03:00:00+00:00", cost=2.0))

    stats = ue.export(rows, output)

    assert stats["unpriced"] == 2 and stats["rows"] == 1
    assert stats["usd"] == 2.0


def test_the_exporter_refuses_exactly_the_costs_the_shim_refuses(tmp_path):
    """The exporter's bound is the writer's own: shim's bucket filter keeps
    a receipt's cost only below its COST_LIMIT, and this reader must never bill a
    value the writer would drop. The boundary is pinned from both sides — a cost the
    shim keeps still bills as a gateway cost, the limit itself (the first value it
    refuses) exports unpriced — and the two literals are one number, so neither
    script's bound can drift from the other's."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "kept.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1e13))
    write(rows, "refused.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00",
                                        cost=SHIM_COST_LIMIT))

    stats = ue.export(rows, output)

    day = read_day(output, "2026-10-16")
    assert [r["costUSD"] for r in day] == [1e13] and stats["usd"] == 1e13
    assert stats["unpriced"] == 1 and stats["gateway_rows"] == 1
    assert ue.COST_LIMIT == SHIM_COST_LIMIT


def test_a_billed_and_an_estimated_cost_are_told_apart(tmp_path):
    """One gateway-billed row and one row the shim priced itself: the export names
    each kind, and the billed total never absorbs the list-price estimate. This is
    the seam that read both as `gateway` before #51."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "billed.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=2))
    write(rows, "estimated.json", receipt("b", "deepseek-flash", "2026-10-16T02:00:00+00:00",
                                          estimate=0.75))

    stats = ue.export(rows, output)

    day = read_day(output, "2026-10-16")
    sources = {r["message"]["model"]: r["ccmorph"]["costSource"] for r in day}
    assert sources == {"claude-opus-5-5": "gateway", "deepseek-flash": "estimate"}
    assert {r["message"]["model"]: r["costUSD"] for r in day} == {"claude-opus-5-5": 2, "deepseek-flash": 0.75}
    assert stats["usd"] == 2
    assert stats["estimated_usd"] == 0.75
    assert stats["gateway_rows"] == 1 and stats["estimated_rows"] == 1
    assert stats["unpriced"] == 0 and stats["rows"] == 2


def test_unpriced_model_with_a_gateway_cost_still_exports(tmp_path):
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "Int_Kimi_K3_20260717", "2026-10-16T01:00:00+00:00", cost=2.5))

    stats = ue.export(rows, output)

    assert stats["unpriced"] == 0 and stats["usd"] == 2.5
    record = read_day(output, "2026-10-16")[0]
    assert record["ccmorph"]["costSource"] == "gateway"


@pytest.mark.parametrize("tokens, costs, expected_rows, expected_usd", [
    (("first", "second"), (1, 2), 2, 3),
    (("first", "first"), (1, 1), 1, 1),
    ((None, None), (1, 1), 1, 1),
], ids=["distinct-turns-issue-311", "duplicate-turn", "legacy-duplicate"])
def test_export_deduplicates_turns_not_timestamps(tmp_path, tokens, costs, expected_rows, expected_usd):
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    for name, token, cost in zip(("a.json", "b.json"), tokens, costs):
        row = receipt("recycled", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=cost)
        if token is not None:
            row["turn_id"] = token
        write(rows, name, row)

    stats = ue.export(rows, output)

    records = read_day(output, "2026-10-16")
    assert stats["rows"] == expected_rows
    assert len({record["requestId"] for record in records}) == expected_rows
    assert stats["usd"] == expected_usd


@pytest.mark.parametrize("damage", ["schema", "huge-token-count-issue-368"])
@pytest.mark.parametrize("since", [None, "20261016"], ids=["full-issue-246", "since-midnight-issue-246"])
def test_a_malformed_receipt_still_fails_the_export(tmp_path, since, damage):
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00"))
    damaged = ({**receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00Z"), "schema": 2}
               if damage == "schema"
               else {**receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00Z"),
                     "usage": {"input_tokens": 10 ** 400}})
    write(rows, "b.json", damaged)
    ledger = output / "projects" / "ccmorph-ledger"
    ledger.mkdir(parents=True)
    prior = ledger / "2026-10-16.jsonl"
    prior.write_text("previous export\n")
    stale = ledger / "2026-10-17.jsonl"  # a day this run has no receipt for at all
    stale.write_text("stale export\n")
    (output / "ccusage.json").write_text("previous config\n")
    boundary = dt.datetime.strptime(since, "%Y%m%d").astimezone() if since else None

    try:
        ue.export(rows, output, boundary)
    except ValueError as exc:
        assert "b.json" in str(exc)
    else:
        raise AssertionError("a malformed receipt must refuse the export")
    assert prior.read_text() == "previous export\n"
    assert stale.read_text() == "stale export\n"  # unreadable input must not reconcile anything
    assert (output / "ccusage.json").read_text() == "previous config\n"


@pytest.mark.parametrize("gone", ["removed", "damaged", "incomplete"],
                         ids=["receipt-removed-issue-246", "receipt-unpriced-issue-246",
                              "receipt-incomplete-issue-246"])
def test_a_rerun_drops_days_whose_last_usable_receipt_is_gone(tmp_path, gone):
    """A rerun is a snapshot of the whole range, not an addition to it: once no usable
    receipt supports a day, its exported file must leave the consumer's input too, or
    the summary reports a cost the consumer still shows."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    ledger = output / "projects" / "ledger"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-15T01:00:00+00:00"))
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1.25))

    ue.export(rows, output)
    assert [r["costUSD"] for r in read_day(output, "2026-10-16")] == [1.25]
    kept = read_day(output, "2026-10-15")

    if gone == "removed":
        (rows / "b.json").unlink()
    elif gone == "damaged":
        write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1e14))
    else:
        write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T01:00:00+00:00",
                                      cost=1.25, complete=False))

    stats = ue.export(rows, output)

    assert not (ledger / "2026-10-16.jsonl").exists()
    assert read_day(output, "2026-10-15") == kept
    exported = [r for path in ledger.glob("*.jsonl") for r in read_day(output, path.stem)]
    assert stats["usd"] == sum(r.get("costUSD", 0) for r in exported)  # summary and consumer input agree


@pytest.mark.parametrize("overlap", ["rewritten", "removed"],
                         ids=["rewritten-issue-246", "removed-issue-246"])
def test_another_run_touching_a_stale_day_is_never_undone(tmp_path, monkeypatch, overlap):
    """Two exports can overlap: a day the other one wrote while this one reads receipts is
    that run's snapshot, not ours to remove, and a day it removed first is already the state
    this run wanted, so this run still finishes."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    ledger = output / "projects" / "ccmorph-ledger"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-15T01:00:00+00:00"))
    ledger.mkdir(parents=True)
    stale = ledger / "2026-10-16.jsonl"
    stale.write_text("previous export\n")
    earlier = local(2026, 10, 17, 3)
    os.utime(stale, (earlier.timestamp(), earlier.timestamp()))

    raced = []
    if overlap == "rewritten":
        convert = ue.convert

        def other_run(row):
            answer = convert(row)
            if not raced:
                raced.append(1)
                stale.write_text("the other run's export\n")
                later = local(2026, 10, 18, 3)
                os.utime(stale, (later.timestamp(), later.timestamp()))
            return answer

        monkeypatch.setattr(ue, "convert", other_run)
    else:
        owned = ue.owned_days

        def other_run(ledger_dir):
            listed = owned(ledger_dir)
            if not raced:
                raced.append(1)
                stale.unlink()
            return listed

        monkeypatch.setattr(ue, "owned_days", other_run)

    stats = ue.export(rows, output)

    assert raced == [1], "the overlapping run must have acted while this export was running"
    assert stats["rows"] == 1
    assert stale.read_text() == "the other run's export\n" if overlap == "rewritten" else not stale.exists()
    assert (output / "ccusage.json").exists()  # the run finished either way


def test_a_listed_receipt_the_move_took_mid_read_is_skipped_issue_583(tmp_path, monkeypatch):
    """The pre-shard move links each flat receipt into its day folder and then removes
    the flat name (#513), and this export lists first and opens later, so the mover can
    empty a listed path between the two. A vanished path is the move's relocation, not a
    damaged store: the run skips it whole, finishes, and its summary names the count —
    the skip #552 installed in the Go readers, and the one owned_days here already
    makes two functions away (#583)."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1))
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00", cost=2))
    gone = rows / "b.json"

    reader = ue.receipt_rows
    raced = []

    def mover(root, since):
        # receipt_rows lists the whole store before it yields its first row, so a
        # name removed after that first row is one the listing already holds.
        listed = reader(root, since)
        first = next(listed)
        raced.append(first[0].name)
        gone.unlink()  # the move removes the flat name after the listing, before its read
        yield first
        yield from listed

    monkeypatch.setattr(ue, "receipt_rows", mover)

    stats = ue.export(rows, output)

    assert raced == ["a.json"], "the move must land between the listing and the read of b.json"
    assert stats["rows"] == 1 and stats["moved"] == 1 and stats["usd"] == 1
    assert [r["costUSD"] for r in read_day(output, "2026-10-16")] == [1]
    assert "1 receipt moved by the store mid-run, skipped" in ue.report(stats, output, rows).splitlines()


def test_a_day_folder_receipt_gone_between_listing_and_read_is_skipped_issue_583(tmp_path, monkeypatch):
    """A day folder is listed whole before its receipts are read, and compaction (#642)
    removes a folder's names once their archive is published, so the same race reaches
    a day folder: a name its listing holds and its read cannot open is skipped and
    counted, the way the Go readers' receiptMarks skips it (#552, #583)."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    day = rows / "2026-10-16"
    day.mkdir(parents=True)
    write(day, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1))
    write(day, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00", cost=2))
    gone = day / "b.json"
    real_scandir = os.scandir
    raced = []

    def scandir(path):
        if os.fspath(path) != os.fspath(day):
            return real_scandir(path)
        with real_scandir(path) as entries:
            listed = sorted(entries, key=lambda e: e.name)
        for entry in listed:
            entry.is_file(follow_symlinks=False)  # the listing's own view, taken before the race
        raced.append([e.name for e in listed])
        gone.unlink()  # the name leaves after the folder's listing, before its read
        return contextlib.nullcontext(iter(listed))

    monkeypatch.setattr(ue.os, "scandir", scandir)

    stats = ue.export(rows, output)

    assert raced == [["a.json", "b.json"]], "the folder must be listed before b.json goes"
    assert stats["rows"] == 1 and stats["moved"] == 1 and stats["usd"] == 1
    assert [r["costUSD"] for r in read_day(output, "2026-10-16")] == [1]


def test_a_receipt_still_there_but_unreadable_still_fails_the_export_issue_583(tmp_path):
    """The moved-by-the-mover skip reads only a path that is gone: a receipt that is
    still there and cannot be read is damage or permissions, and the run still fails
    — so the race skip never quiets a real unreadable store (#583). It fails naming
    the receipt after counting every other one, the total quota and the report give
    the same store (#654), and it writes nothing: a day file is rewritten whole, so
    a partial run would drop the unread turn from the export already there."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    if os.geteuid() == 0:
        pytest.skip("root reads a mode-000 file")
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1))
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00", cost=2))
    ue.export(rows, output)
    day = output / "projects" / "ledger" / "2026-10-16.jsonl"
    before = day.read_bytes()
    (rows / "b.json").chmod(0o000)

    stats = ue.export(rows, output)
    assert stats["unreadable"] == [f"{rows / 'b.json'}: Permission denied"]
    assert (stats["rows"], stats["gateway_rows"], stats["usd"]) == (1, 1, 1)
    assert day.read_bytes() == before, "a run that could not read every receipt rewrote the export"

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert ran.returncode == 1, ran.stdout + ran.stderr
    assert f"usage-export: {rows / 'b.json'}: Permission denied" in ran.stderr
    assert "Nothing written" in ran.stdout and "the gateway billed, $1.00" in ran.stdout
    assert "[Errno" not in ran.stderr and "Traceback" not in ran.stderr
    assert day.read_bytes() == before


def test_a_since_rerun_reconciles_only_its_range(tmp_path):
    """--since rewrites its own range and nothing else: the day it has no receipt for
    goes, an earlier day stays, and every file the exporter does not own is left alone."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    ledger = output / "projects" / "ledger"
    rows.mkdir()
    for rid, day in [("a", "2026-10-14"), ("b", "2026-10-16"), ("c", "2026-10-17")]:
        write(rows, f"{rid}.json", receipt(rid, "claude-opus-5-5", f"{day}T01:00:00+00:00"))

    ue.export(rows, output)

    (ledger / "notes.txt").write_text("hand-placed notes\n")
    (ledger / "2026-10-16.jsonl.bak").write_text("backup\n")
    (ledger / "2026-13-45.jsonl").write_text("not a calendar date\n")
    other = output / "projects" / "other" / "2026-10-17.jsonl"
    other.parent.mkdir(parents=True)
    other.write_text("another project's day\n")
    elsewhere = tmp_path / "elsewhere.jsonl"
    elsewhere.write_text("symlink target\n")
    (ledger / "2026-10-18.jsonl").symlink_to(elsewhere)

    (rows / "a.json").unlink()
    (rows / "c.json").unlink()

    stats = ue.export(rows, output, local(2026, 10, 16))

    assert stats["days"] == 1 and stats["rows"] == 1
    assert [r["timestamp"][:10] for r in read_day(output, "2026-10-14")] == ["2026-10-14"]
    assert not (ledger / "2026-10-17.jsonl").exists()
    assert [r["timestamp"][:10] for r in read_day(output, "2026-10-16")] == ["2026-10-16"]
    assert (ledger / "notes.txt").read_text() == "hand-placed notes\n"
    assert (ledger / "2026-10-16.jsonl.bak").read_text() == "backup\n"
    assert (ledger / "2026-13-45.jsonl").read_text() == "not a calendar date\n"
    assert other.read_text() == "another project's day\n"
    assert (ledger / "2026-10-18.jsonl").is_symlink()
    assert elsewhere.read_text() == "symlink target\n"


def test_since_exports_an_in_range_receipt_with_an_old_file_mtime(tmp_path):
    """The receipt's own timestamp decides the range, never the file's mtime: a receipt
    restored from a backup with an old mtime still exports, so no run can drop its day."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00Z"),
          mtime=local(2026, 10, 1))

    stats = ue.export(rows, output, local(2026, 10, 16))

    assert stats["rows"] == 1
    assert [r["timestamp"][:10] for r in read_day(output, "2026-10-16")] == ["2026-10-16"]


@pytest.mark.parametrize("since, expected_days, expected_unpriced", [
    (None, {"1941-06-16": ["1941-06-15T15:30:00.000Z"],
            "2026-10-15": ["2026-10-15T15:59:59.000Z"],
            "2026-10-16": ["2026-10-15T16:00:00.000Z"]}, 2),
    ("20261016", {"2026-10-16": ["2026-10-15T16:00:00.000Z"]}, 1),
], ids=["historical-boundaries-issue-248", "since-midnight-issue-248"])
def test_since_filters_by_local_day(tmp_path, since, expected_days, expected_unpriced):
    """The receipt's own timestamp decides a row's day, never the file's mtime, and the
    same cutoff governs the unpriced count: a damaged receipt from the day before the
    request is not counted even though its file was written after the cutoff (#253)."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    fresh = local(2026, 10, 16, 9)  # a file mtime past the cutoff
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "1941-06-15T15:30:00Z"), mtime=fresh)
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-15T15:59:59Z"), mtime=fresh)
    write(rows, "c.json", receipt("c", "claude-opus-5-5", "2026-10-15T16:00:00Z"), mtime=fresh)
    # The damaged pair mirrors the good pair at the same two instants, one second apart.
    write(rows, "d.json", receipt("d", "claude-opus-5-5", "2026-10-15T15:59:59Z", cost=1e14), mtime=fresh)
    write(rows, "e.json", receipt("e", "claude-opus-5-5", "2026-10-15T16:00:00Z", cost=1e14), mtime=fresh)
    boundary = dt.datetime.strptime(since, "%Y%m%d").astimezone() if since else None

    stats = ue.export(rows, output, boundary)

    ledger = output / "projects" / "ledger"
    assert {p.stem: [r["timestamp"] for r in read_day(output, p.stem)]
            for p in ledger.glob("*.jsonl")} == expected_days
    assert stats["rows"] == sum(len(records) for records in expected_days.values())
    assert stats["unpriced"] == expected_unpriced  # only the receipt's instant, never its file's mtime, counts it


def test_a_mapped_receipt_is_grouped_by_the_served_model(tmp_path):
    """The alias keeps the client's identity, but the recorded target answered:
    the model grouping (and so ccusage's price) follows the served model, not the
    alias, and every recorded identity stays in the row's metadata."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", mapped_receipt("a", "2026-10-16T01:00:00+00:00"))

    stats = ue.export(rows, output)

    day = read_day(output, "2026-10-16")
    assert [r["message"]["model"] for r in day] == ["gpt-6.1-sol"]
    record = day[0]
    assert "costUSD" not in record and record["ccmorph"]["costSource"] == "list"
    assert stats["usd"] == 0 and stats["rows"] == 1 and stats["unpriced"] == 0 and stats["gateway_rows"] == 0
    assert record["ccmorph"]["requestedModel"] == "claude-sonnet-5-5[1m]"
    assert record["ccmorph"]["servedModel"] == "gpt-6.1-sol"
    assert record["ccmorph"]["responseModel"] == "claude-sonnet-5-5"


def test_a_doubled_marker_is_stripped_only_once(tmp_path):
    """Exactly one trailing [1m] is dropped, never a whole run of them: a doubled
    marker still leaves the served id different from the requested one, so the row
    is a mapping and the grouping keeps the marker that single strip left behind."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-sonnet-5-5", "2026-10-16T01:00:00+00:00",
                                  requested="gpt-6.1-sol[1m]", served="gpt-6.1-sol[1m][1m]"))

    ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["message"]["model"] == "gpt-6.1-sol[1m]" and "costUSD" not in record
    assert record["ccmorph"]["requestedModel"] == "gpt-6.1-sol[1m]"
    assert record["ccmorph"]["servedModel"] == "gpt-6.1-sol[1m][1m]"


def test_a_mapped_gateway_cost_stays_authoritative(tmp_path):
    """A gateway-reported cost is used as billed; the served model still carries
    the grouping and the recorded identities."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", mapped_receipt("a", "2026-10-16T01:00:00+00:00", cost=7))

    stats = ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["costUSD"] == 7 and record["ccmorph"]["costSource"] == "gateway"
    assert record["message"]["model"] == "gpt-6.1-sol"
    assert stats["usd"] == 7 and stats["gateway_rows"] == 1 and stats["unpriced"] == 0


def test_a_served_marker_is_stripped_once_for_grouping_and_kept_in_metadata(tmp_path):
    """The exporter does not assume the shim already stripped a target's marker:
    one trailing [1m] is dropped for grouping, while the recorded
    served id is copied into the metadata verbatim."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-sonnet-5-5", "2026-10-16T01:00:00+00:00",
                                  requested="claude-sonnet-5-5[1m]", served="gpt-6.1-sol[1m]"))

    ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["message"]["model"] == "gpt-6.1-sol"
    assert record["ccmorph"]["servedModel"] == "gpt-6.1-sol[1m]"


def test_an_unmapped_1m_row_keeps_its_response_identity(tmp_path):
    """No map and no substitution: the shim records the same [1m] id the client asked
    for, and that row exports with exactly the identity it always did."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5[1m]", "2026-10-16T01:00:00+00:00",
                                  requested="claude-opus-5-5[1m]", served="claude-opus-5-5[1m]"))

    ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["message"]["model"] == "claude-opus-5-5[1m]"
    assert record["ccmorph"]["requestedModel"] == "claude-opus-5-5[1m]"
    assert record["ccmorph"]["servedModel"] == "claude-opus-5-5[1m]"
    assert record["ccmorph"]["responseModel"] == "claude-opus-5-5[1m]"


def test_an_unmapped_row_keeps_the_response_identity_when_it_differs_from_served(tmp_path):
    """Requested and served match once one [1m] is stripped, so this is not a
    mapping: the served id must not replace the identity the client was answered
    with, even though the two strings differ."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00",
                                  requested="claude-opus-5-5[1m]", served="claude-opus-5-5[1m]"))

    ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["message"]["model"] == "claude-opus-5-5"
    assert record["ccmorph"]["servedModel"] == "claude-opus-5-5[1m]"
    assert record["ccmorph"]["responseModel"] == "claude-opus-5-5"


def test_a_non_claude_row_asked_for_and_served_the_same_keeps_the_response_identity(tmp_path):
    """A gateway may answer under its own name; requested == served is not a map, so
    the response identity exports."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "kimi-k3-0717", "2026-10-16T01:00:00+00:00",
                                  requested="Int_Kimi_K3_20260717", served="Int_Kimi_K3_20260717"))

    ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["message"]["model"] == "kimi-k3-0717"
    assert record["ccmorph"]["requestedModel"] == "Int_Kimi_K3_20260717"
    assert record["ccmorph"]["servedModel"] == "Int_Kimi_K3_20260717"
    assert record["ccmorph"]["responseModel"] == "kimi-k3-0717"


def test_a_served_id_without_a_requested_id_is_not_treated_as_a_map(tmp_path):
    """A map is only detected by the two recorded ids differing, so a row that
    records no requested id keeps the response identity it has always used."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-sonnet-5-5", "2026-10-16T01:00:00+00:00",
                                  requested=None, served="gpt-6.1-sol"))

    ue.export(rows, output)

    record = read_day(output, "2026-10-16")[0]
    assert record["message"]["model"] == "claude-sonnet-5-5"
    assert record["ccmorph"]["requestedModel"] is None
    assert record["ccmorph"]["servedModel"] == "gpt-6.1-sol"


def test_a_legacy_row_without_served_model_exports_unchanged(tmp_path):
    """Rows written before served_model existed keep the old response/requested
    choice, and the added metadata reports a missing identity as null."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00"))
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00",
                                  requested="claude-opus-4-5"))
    write(rows, "c.json", receipt("c", "claude-opus-5-5", "2026-10-16T03:00:00+00:00",
                                  requested="claude-opus-4-5", served=None))

    stats = ue.export(rows, output)

    day = read_day(output, "2026-10-16")
    assert [r["message"]["model"] for r in day] == ["claude-opus-5-5"] * 3
    assert all("costUSD" not in r and r["ccmorph"]["costSource"] == "list" for r in day)
    assert day[0]["ccmorph"]["requestedModel"] is None and day[0]["ccmorph"]["servedModel"] is None
    assert day[0]["ccmorph"]["responseModel"] == "claude-opus-5-5"
    assert day[1]["ccmorph"]["requestedModel"] == "claude-opus-4-5"
    assert day[1]["ccmorph"]["servedModel"] is None
    assert day[2]["ccmorph"]["servedModel"] is None
    assert stats["unpriced"] == 0 and stats["rows"] == 3


def test_receipts_of_two_profiles_land_in_two_profile_folders(tmp_path):
    """One folder per profile, named with the profile alone (#383): the label ccusage
    prints for a block is the folder name, cut to its head at a narrow terminal, so a
    label that began with the constant `ccmorph-` prefix lost exactly the part that
    differs — every profile read `ccmorph…`. A folder also carries the marker file
    that says whose it is, the name alone no longer can; an unsafe character in a
    profile is replaced, a profile that is no folder (only damage writes one — a name
    is letters, digits and underscores) lands with the profile-less, and so does a
    row with no profile."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", {**receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00"), "profile": "work"})
    write(rows, "b.json", {**receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00"), "profile": "a/b"})
    write(rows, "c.json", {**receipt("c", "claude-opus-5-5", "2026-10-16T03:00:00+00:00"), "profile": None})
    write(rows, "d.json", {**receipt("d", "claude-opus-5-5", "2026-10-16T04:00:00+00:00"), "profile": ".."})

    ue.export(rows, output)

    folders = sorted(p.name for p in (output / "projects").iterdir())
    assert folders == ["a_b", "unknown", "work"]
    assert len(read_day_in(output, "work", "2026-10-16")) == 1
    assert len(read_day_in(output, "unknown", "2026-10-16")) == 2
    assert all((output / "projects" / f / ue.MARKER).is_file() for f in folders)


def test_a_rerun_reconciles_an_export_from_before_the_profile_named_folders(tmp_path):
    """#383: an export written before the change holds `ccmorph-<profile>` folders. A
    full rerun moves its days to the profile-named folders and removes the legacy day
    files — one profile stays one project, not two — while the emptied legacy folder
    itself stays, and a folder the exporter never marked is still not ours to touch."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1.25))
    legacy = output / "projects" / "ccmorph-ledger"
    legacy.mkdir(parents=True)
    (legacy / "2026-10-16.jsonl").write_text("previous export\n")
    foreign = output / "projects" / "other"
    foreign.mkdir(parents=True)
    (foreign / "2026-10-16.jsonl").write_text("someone else's day\n")

    stats = ue.export(rows, output)

    assert [r["costUSD"] for r in read_day(output, "2026-10-16")] == [1.25]
    assert not (legacy / "2026-10-16.jsonl").exists()
    assert legacy.is_dir()  # day files are reconciled, folders are never removed
    assert (foreign / "2026-10-16.jsonl").read_text() == "someone else's day\n"
    assert stats["rows"] == 1


def test_a_folder_the_exporter_does_not_own_is_never_taken_over(tmp_path):
    """#383: a folder in the output that carries no marker — a live Claude Code's own
    project folder in the config dir, or anything a hand placed there — keeps its name
    even when a profile's destination is exactly that folder: taking it over would mark
    it ours, overwrite its matching day file and let a later run remove its other days.
    The whole export fails before anything is written, naming the folder, and every
    file in it stays as it was."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", {**receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1.25),
                           "profile": "work"})
    foreign = output / "projects" / "work"
    foreign.mkdir(parents=True)
    (foreign / "2026-10-16.jsonl").write_text("someone else's day\n")
    (foreign / "2026-10-17.jsonl").write_text("someone else's next day\n")

    with pytest.raises(ValueError, match="projects/work"):
        ue.export(rows, output)

    assert (foreign / "2026-10-16.jsonl").read_text() == "someone else's day\n"
    assert (foreign / "2026-10-17.jsonl").read_text() == "someone else's next day\n"
    assert not (foreign / ue.MARKER).exists()
    assert not (output / "ccusage.json").exists()


def test_a_profile_that_is_not_a_name_fails_the_export_cleanly(tmp_path):
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    bad = receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00")
    bad["profile"] = 5
    write(rows, "a.json", bad)
    with pytest.raises(ValueError, match="a.json"):
        ue.export(rows, output)


def test_an_empty_range_still_gives_ccusage_a_projects_dir(tmp_path):
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    ue.export(rows, output)
    assert (output / "projects").is_dir() and (output / "ccusage.json").is_file()


def test_the_exported_timestamp_is_utc_with_milliseconds_and_z(tmp_path):
    """ccusage drops any other spelling of the stamp."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-22T09:48:09.023400+08:00"))

    ue.export(rows, output)

    assert read_day(output, "2026-10-22")[0]["timestamp"] == "2026-10-22T01:48:09.023Z"


def test_the_python_floor_scan_covers_the_exporter():
    """Issue #248: the exporter stays in the Python floor scan."""
    assert is_python_helper(str(EXPORTER), EXPORTER.read_text())


@pytest.mark.parametrize("via", ["path", "symlink"], ids=["direct-issue-261", "through-a-symlink-issue-416"])
def test_the_checkout_launcher_runs_the_shipped_exporter(tmp_path, via):
    """#261: the exporter ships as share/usage-export; scripts/usage-export is
    the checkout's pre-existing spelling and must still reach it. #416: the
    wrapper is reached through a link in another directory, so its neighbour is
    named beside the script, never beside the link. #382: what the
    launcher answers is the report a person reads, so the check is the whole
    report, exactly — not a substring another line could also satisfy — with the
    zero-row count it stands on."""
    (tmp_path / "rows").mkdir()
    output = tmp_path / "out"
    script = pathlib.Path(str(EXPORTER)).parent.parent / "scripts/usage-export"
    launcher = script
    if via == "symlink":
        launcher = tmp_path / "usage-export"
        launcher.symlink_to(script)
    ran = subprocess.run(
        [str(launcher), "--output", str(output), "--source", str(tmp_path / "rows")],
        capture_output=True, text=True, timeout=HANG_TIMEOUT,
        env=dict(os.environ, PATH=f"{ccusage_stub(tmp_path)}{os.pathsep}{os.environ['PATH']}"))
    assert ran.returncode == 0, ran.stderr
    root = os.path.abspath(output)
    assert ran.stdout == "\n".join([
        f"Exported 0 turns over 0 days from {os.path.realpath(tmp_path / 'rows')} to {root}",
        "one folder per profile under projects/, ccusage.json beside it",
        "0 turns the gateway billed, $0.00",
        "0 turns ccmorph estimated, $0.00",
        "0 turns left for ccusage to price at its list price",
        "0 turns unpriced (a damaged cost, not exported)",
        "0 receipts moved by the store mid-run, skipped",
        "0 receipts not parseable, skipped",
        "Then, with ccusage installed:",
        f"CLAUDE_CONFIG_DIR={root} ccusage claude daily --instances "
        f"--config {root}/ccusage.json",
    ]) + "\n"
    assert (tmp_path / "out/ccusage.json").is_file()


@pytest.mark.parametrize("state", ["home", "xdg"],
                         ids=["default-state-home-issue-647", "xdg-state-home-issue-647"])
def test_a_fresh_install_exports_zeroes_before_the_first_session_issue_647(tmp_path, state):
    """#647: right after an install, before any launched session, the receipt
    store folder does not exist at all. The README's command died with
    "usage-export: [Errno 2] No such file or directory: .../usage/.move.lock" and
    exit 1. A never-created store is an empty one: the zero report, exit 0, and
    the reader still creates no store folder."""
    home, output = tmp_path / "home", tmp_path / "out"
    home.mkdir()
    env = {k: v for k, v in os.environ.items() if k != "XDG_STATE_HOME"}
    env["HOME"] = str(home)
    state_root = home / ".local/state"
    if state == "xdg":
        state_root = tmp_path / "state"
        env["XDG_STATE_HOME"] = str(state_root)

    ran = subprocess.run([sys.executable, str(EXPORTER), "--output", str(output)],
                         capture_output=True, text=True, timeout=HANG_TIMEOUT, env=env)

    assert ran.returncode == 0, ran.stderr
    assert ran.stdout.splitlines()[0] == (
        f"Exported 0 turns over 0 days from "
        f"{os.path.realpath(state_root / 'ccmorph' / 'usage')} to {os.path.abspath(output)}")
    assert "[Errno" not in ran.stdout + ran.stderr
    assert ".move.lock" not in ran.stdout + ran.stderr
    assert not (state_root / "ccmorph").exists()
    assert (output / "projects").is_dir() and (output / "ccusage.json").is_file()


@pytest.mark.parametrize("kind", ["missing", "file"],
                         ids=["source-folder-missing-issue-843", "source-is-a-file-issue-843"])
def test_a_named_source_that_is_not_a_folder_refuses_naming_the_path_issue_843(tmp_path, kind):
    """GH-843: a wrong --source — a typo, or receipts under the other state root the
    README describes — printed the confident "Exported 0 turns over 0 days",
    byte-identical to a genuinely empty store: silence that reads as zero. A source
    named on the command line that is not a folder refuses, names the path the way
    every other refusal here names one (#647), and writes nothing."""
    rows, output = tmp_path / "no-such-receipts", tmp_path / "out"
    if kind == "file":
        rows.write_text("not a folder\n")

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 1
    assert str(rows) in ran.stderr, ran.stderr
    assert "[Errno" not in ran.stderr and "Traceback" not in ran.stderr
    assert not output.exists(), "the refusal wrote an output tree beside the error"


def test_an_unreadable_named_source_exits_nonzero_naming_the_folder_issue_843(tmp_path):
    """GH-843's other half: the folder is there but this run cannot read it. The run
    still refuses with the folder's own name, never Python's errno spelling."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    if os.geteuid() == 0:
        pytest.skip("root reads a mode-000 folder")
    rows.mkdir()
    rows.chmod(0o000)
    try:
        ran = subprocess.run(
            [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    finally:
        rows.chmod(0o700)  # let tmp_path clean up

    assert ran.returncode == 1
    assert str(rows) in ran.stderr, ran.stderr
    assert "[Errno" not in ran.stderr and "Traceback" not in ran.stderr


def test_a_source_behind_an_unsearchable_parent_refuses_naming_the_folder_issue_843():
    """GH-843 review: the existence probe must not raise the errno it stands guard
    against. When a parent of the named --source cannot be searched, stat'ing the
    source itself fails, and the run still answers with a plain refusal naming the
    folder — never the Python traceback with the [Errno 13] inside it."""
    with tempfile.TemporaryDirectory() as d:
        root = pathlib.Path(d)
        parent, source = root / "private", root / "private" / "receipts"
        source.mkdir(parents=True)
        script = root / "exporter"
        shutil.copyfile(EXPORTER, script)
        who = {}
        if os.geteuid() == 0:
            root.chmod(0o755)  # let an unprivileged reader reach its own paths
            script.chmod(0o644)
            # Popen's user= is Python 3.9+; on the 3.8 floor the child drops to
            # nobody itself before it runs the exporter (GH-843 review).
            who["preexec_fn"] = lambda: os.setuid(65534)
        source.chmod(0o000)  # the leaf first: once the parent is sealed, nothing inside reaches it
        parent.chmod(0o000)
        try:
            ran = subprocess.run(
                [sys.executable, str(script), "--output", str(root / "out"), "--source", str(source)],
                capture_output=True, text=True, timeout=HANG_TIMEOUT, **who)
        finally:
            parent.chmod(0o700)
            source.chmod(0o700)

    assert ran.returncode == 1
    assert str(source) in ran.stderr, ran.stderr
    assert "[Errno" not in ran.stderr and "Traceback" not in ran.stderr


def test_the_unsearchable_parent_case_drops_root_the_way_python_38_can_issue_843(monkeypatch):
    """GH-843 review: Popen's user=, group=, extra_groups= and umask= are Python 3.9+
    (process_group= 3.11, pipesize= 3.10); on the 3.8 floor any of them raises
    TypeError before the exporter runs. The case above takes its root branch only on
    a root host, which CI is not, so this takes that branch on any host and any
    interpreter, through a subprocess.run that accepts only what 3.8 accepts. The
    privilege drop it is handed is never called here: run in this process it would
    drop the whole suite's privileges."""
    calls = []

    def run_on_the_38_floor(args, **kwargs):
        newer = {"user", "group", "extra_groups", "umask", "process_group", "pipesize"} & set(kwargs)
        if newer:
            raise TypeError(f"Python 3.8 Popen takes no {sorted(newer)}")
        calls.append(kwargs)
        source = args[args.index("--source") + 1]
        return subprocess.CompletedProcess(args, 1, "", f"usage-export: {source}: Permission denied\n")

    monkeypatch.setattr(os, "geteuid", lambda: 0)
    monkeypatch.setattr(subprocess, "run", run_on_the_38_floor)
    test_a_source_behind_an_unsearchable_parent_refuses_naming_the_folder_issue_843()
    assert len(calls) == 1 and callable(calls[0].get("preexec_fn")), calls


def test_an_existing_empty_source_still_exits_zero_and_prints_the_folder_issue_843(tmp_path):
    """#647's zero report must survive this: a store folder that exists and holds no
    receipt is genuinely empty, exports nothing and exits 0. What tells it apart from
    the refused typo is the head line, which now names the folder the run read — a
    reader who sees a folder they did not name knows where the zeros came from."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 0, ran.stderr
    assert ran.stdout.splitlines()[0] == (
        f"Exported 0 turns over 0 days from {os.path.realpath(rows)} "
        f"to {os.path.abspath(output)}")


@pytest.mark.parametrize("installed", [True, False],
                         ids=["ccusage-on-path", "ccusage-missing-issue-843"])
def test_the_summary_names_a_missing_ccusage_instead_of_printing_a_dead_command_issue_843(
        tmp_path, installed):
    """GH-843: every run ended on a ccusage command, but no ccmorph install route
    provides ccusage (README, Requirements), so that final step dead-ended in
    "ccusage: command not found" with nothing saying it was missing. Absent from
    PATH, the summary says so and points at the README's note; present, the exact
    command prints exactly as before."""
    rows, output, bin_dir = tmp_path / "rows", tmp_path / "out", tmp_path / "bin"
    rows.mkdir()
    bin_dir.mkdir()
    env = dict(os.environ, PATH=str(tmp_path / "emptybin"))  # a PATH nothing can be found on
    (tmp_path / "emptybin").mkdir()
    if installed:
        (bin_dir / "ccusage").write_text("#!/bin/sh\nexit 0\n")
        os.chmod(bin_dir / "ccusage", 0o755)
        env["PATH"] = f"{bin_dir}{os.pathsep}{env['PATH']}"

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT, env=env)

    assert ran.returncode == 0, ran.stderr
    if installed:
        assert "Then, with ccusage installed:" in ran.stdout
        assert any(l.startswith("CLAUDE_CONFIG_DIR=") for l in ran.stdout.splitlines())
    else:
        assert "ccusage is not installed" in ran.stdout
        assert "README" in ran.stdout
        assert "CLAUDE_CONFIG_DIR=" not in ran.stdout, "the summary still prints a dead command"


def test_the_printed_source_reaches_the_folder_the_run_read_issue_843(tmp_path):
    """GH-843 review: a spelling of the source is only a name for the folder the
    run read if following it goes where the run went. The exporter walks a
    `..` through the symlink before it, while a lexical collapse of the same `..`
    names a folder that is not there — so the printed path must be traversable
    to the very store the export read."""
    (tmp_path / "physical/child").mkdir(parents=True)
    actual = tmp_path / "physical" / "ccmorph" / "usage"
    actual.mkdir(parents=True)
    link = tmp_path / "link"
    link.symlink_to(tmp_path / "physical" / "child", target_is_directory=True)
    env = {k: v for k, v in os.environ.items() if k != "XDG_STATE_HOME"}
    env["XDG_STATE_HOME"] = str(link / "..")  # walked: link -> physical/child, .. -> physical

    ran = subprocess.run([sys.executable, str(EXPORTER), "--output", str(tmp_path / "out")],
                         env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 0, ran.stderr
    assert (actual / ".move.lock").exists(), "the exporter read some other store"
    reported = pathlib.Path(ran.stdout.splitlines()[0].split(" from ", 1)[1].split(" to ", 1)[0])
    assert reported.exists() and reported.samefile(actual), (
        f"the summary names a folder the run did not read: {ran.stdout.splitlines()[0]}")


@pytest.mark.parametrize("missing", [".jsonl", ".jsonl.idx"],
                         ids=["archive-data-missing-issue-647", "archive-index-missing-issue-647"])
def test_an_archive_missing_a_piece_names_the_file_not_an_errno_issue_647(tmp_path, missing):
    """#647: no missing-state path may reach the person as a raw Python error
    string. A day archive whose data or index file is not there refuses the
    export naming the file that is missing, never "[Errno 2] No such file or
    directory"."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    present = ".jsonl.idx" if missing == ".jsonl" else ".jsonl"
    (rows / f"2026-10-16{present}").write_text("{}\n")

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 1
    assert f"2026-10-16{missing} is missing" in ran.stderr
    assert "[Errno" not in ran.stderr and "Traceback" not in ran.stderr


@pytest.mark.parametrize("error, told", [
    (errno.ENOENT, "{path} is missing"),
    (errno.EACCES, "{path}: Permission denied"),
], ids=["missing-issue-647", "unreadable-issue-647"])
def test_an_os_error_reaching_the_command_is_told_without_an_errno_issue_647(
        tmp_path, monkeypatch, capsys, error, told):
    """#647: whatever file a run finds missing or unreadable — a day folder the
    store compacted away mid-run, an output folder it may not write — the
    command's last word names the path and what is wrong with it, never Python's
    own "[Errno N] ..." spelling."""
    path = tmp_path / "rows/2026-10-16"

    def failing(*_args):
        raise OSError(error, os.strerror(error), str(path))

    monkeypatch.setattr(ue, "export", failing)
    monkeypatch.setattr(sys, "argv", ["usage-export", "--output", str(tmp_path / "out")])
    with pytest.raises(SystemExit) as stopped:
        ue.main()

    assert stopped.value.code == 1
    assert capsys.readouterr().err == "usage-export: " + told.format(path=path) + "\n"


def test_a_blocked_destination_is_named_not_the_temporary_file_issue_647(tmp_path):
    """#647: an OSError carrying two paths — the writer's temporary file and the
    destination it was moving onto — must name the destination, the one the
    person can act on. A ccusage.json that is a directory made the export say a
    .usage-* temporary file "Is a directory" and then delete that file, which
    names nothing the reader owns; the blocking destination is the answer."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    (output / "ccusage.json").mkdir(parents=True)

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 1
    assert str(output / "ccusage.json") in ran.stderr
    assert "Is a directory" in ran.stderr
    assert "[Errno" not in ran.stderr and "Traceback" not in ran.stderr


@pytest.mark.parametrize("installed", [True, False],
                         ids=["ccusage-installed", "ccusage-missing-issue-843"])
def test_the_summary_names_where_the_files_went_and_the_command_to_run_next_issue_382(tmp_path,
                                                                                       installed):
    """#382: the export used to end on one raw JSON line — long floating-point
    money, no output folder, no next command — and its counts (13 gateway rows
    plus 5 estimated rows is not 33 rows) left the third kind, the turns ccusage
    prices itself, unnamed. The end of an export is for the person who ran it:
    the folder the files went to, every count by a name that adds up, money at
    cents, and the exact ccusage command for that folder on its own line. GH-843
    made the last lines depend on whether ccusage is on PATH; the counts must be
    pinned exactly under both spellings, so this one store reports under each."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=0.1))
    write(rows, "b.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00", cost=0.0599))
    write(rows, "c.json", receipt("c", "deepseek-flash", "2026-10-16T03:00:00+00:00",
                                  estimate=0.05333333333333333))
    write(rows, "d.json", receipt("d", "claude-opus-5-5", "2026-10-16T04:00:00+00:00"))
    write(rows, "e.json", receipt("e", "claude-opus-5-5", "2026-10-16T05:00:00+00:00", cost=1e14))

    if installed:
        env = dict(os.environ, PATH=f"{ccusage_stub(tmp_path)}{os.pathsep}{os.environ['PATH']}")
        tail = ["Then, with ccusage installed:",
                f"CLAUDE_CONFIG_DIR={os.path.abspath(output)} ccusage claude daily --instances "
                f"--config {os.path.abspath(output)}/ccusage.json"]
    else:
        emptybin = tmp_path / "emptybin"  # a PATH nothing, least of all ccusage, is found on
        emptybin.mkdir()
        env = dict(os.environ, PATH=str(emptybin))
        tail = ["ccusage is not installed: no ccmorph install provides it; see the "
                "README's cost-reporting note, and --help prints the command to run"]
    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows),
         "--since", "20261016"],
        capture_output=True, text=True, timeout=HANG_TIMEOUT, env=env)

    assert ran.returncode == 0, ran.stderr
    # The whole report, exactly — nothing extra, nothing conflicting — because a
    # substring check would pass beside output that disagrees with it.
    root = os.path.abspath(output)
    assert ran.stdout == "\n".join([
        f"Exported 4 turns over 1 day from {os.path.realpath(rows)} to {root}",  # both ends named
        "one folder per profile under projects/, ccusage.json beside it",
        # 0.1 + 0.0599 is 0.15990000000000001 and the estimate 0.05333333333333333:
        # both print at cents, never as the float's own spelling. The three priced
        # kinds are the whole 4, so a reader can reconcile the counts.
        "2 turns the gateway billed, $0.16",
        "1 turn ccmorph estimated, $0.05",
        "1 turn left for ccusage to price at its list price",
        "1 turn unpriced (a damaged cost, not exported)",
        "0 receipts moved by the store mid-run, skipped",
        "0 receipts not parseable, skipped",
    ] + tail) + "\n"


def test_the_printed_command_runs_as_printed_for_a_folder_with_spaces_issue_382(tmp_path):
    """#382: the printed command interpolated both paths bare, so an export to a
    folder named 'out with spaces' printed a command the shell tore apart —
    '/bin/sh: with: command not found', exit 127. The command a person copies
    must run exactly as printed, so this runs the printed line itself against a
    ccusage stub and asks the stub what it was handed."""
    rows, output = tmp_path / "rows", tmp_path / "out with spaces"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=0.1))

    # A ccusage stub on PATH records the config dir it ran under and every
    # argument it was called with, so the assertion reads the command's effect —
    # and the export, asked whether ccusage is installed (GH-843), finds it and
    # prints the command at all.
    stub_dir, seen = tmp_path / "bin", tmp_path / "seen"
    stub_dir.mkdir()
    (stub_dir / "ccusage").write_text(
        '#!/bin/sh\nprintf \'%s\\n\' "$CLAUDE_CONFIG_DIR" "$@" > "$CCUSAGE_STUB_SEEN"\n')
    os.chmod(stub_dir / "ccusage", 0o755)
    stub_env = dict(os.environ, PATH=f"{stub_dir}{os.pathsep}{os.environ['PATH']}")

    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
        capture_output=True, text=True, timeout=HANG_TIMEOUT, env=stub_env)
    assert ran.returncode == 0, ran.stderr
    command = next(l for l in ran.stdout.splitlines() if l.startswith("CLAUDE_CONFIG_DIR="))

    ran_command = subprocess.run(
        ["/bin/sh", "-c", command],
        env=dict(stub_env, CCUSAGE_STUB_SEEN=str(seen)),
        capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran_command.returncode == 0, ran_command.stderr
    assert seen.read_text().splitlines() == [
        str(output), "claude", "daily", "--instances", "--config",
        str(output / "ccusage.json")]


def test_help_prints_the_ccusage_command_with_the_export_paths_filled_in(tmp_path):
    """#261: no install provides ccusage, so the README prints no ccusage
    command; it points at the exporter's --help, which must give it. #382: the
    parser reflowed the description into one paragraph, gluing the command to
    the sentence after it so it could not be copied as one line. #636: the line
    then carried a ROOT placeholder the reader had to substitute twice, while
    the end of a run printed both paths resolved — --help now resolves them the
    same way, for the --output on this command line or the export's default."""
    home = tmp_path / "home"
    home.mkdir()
    output = tmp_path / "out with spaces"
    cases = [(["--help"], home / ".cache" / "ccmorph-usage"),
             (["--output", str(output), "--help"], output),
             # #636: argparse's own rules, not a hand scan of the line.
             ([f"--output={output}", "--help"], output),
             (["--out", str(output), "--help"], output),
             (["--output", str(tmp_path / "first"), "--output", str(output), "--help"], output),
             (["--help", "--output", str(output)], output),
             (["-h"], home / ".cache" / "ccmorph-usage")]
    for args, root in cases:
        ran = subprocess.run(
            [sys.executable, str(EXPORTER), *args],
            capture_output=True, text=True, timeout=HANG_TIMEOUT,
            env=dict(os.environ, HOME=str(home), XDG_CACHE_HOME=""))
        assert ran.returncode == 0, ran.stderr
        assert (f"CLAUDE_CONFIG_DIR={shlex.quote(str(root))} ccusage claude daily "
                f"--instances --config {shlex.quote(str(root / 'ccusage.json'))}"
                ) in ran.stdout.splitlines()
        assert "ROOT ccusage" not in ran.stdout
    for flag in ("--output", "--source", "--since"):
        line = next((l for l in ran.stdout.splitlines() if l.lstrip().startswith(flag)), "")
        assert len(line.split()) > 2, f"{flag} is missing or says nothing: {line!r}"


@pytest.mark.parametrize("state", ["home", "xdg"],
                         ids=["default-state-home-issue-731", "xdg-state-home-issue-731"])
def test_help_shows_the_source_default_as_a_real_path_issue_731(tmp_path, state):
    """#731: --help described the --source default as "the state root's usage/"
    — a sentence naming no folder, which read as the CCMORPH_STATE_DIR root and
    sent that reader to a path the exporter never reads. The default is an XDG
    state path, so --help prints the folder it resolves to under both roots."""
    home = tmp_path / "home"
    home.mkdir()
    env = {k: v for k, v in os.environ.items() if k != "XDG_STATE_HOME"}
    env["HOME"] = str(home)
    default = home / ".local/state/ccmorph/usage"
    if state == "xdg":
        default = tmp_path / "state/ccmorph/usage"
        env["XDG_STATE_HOME"] = str(tmp_path / "state")

    ran = subprocess.run([sys.executable, str(EXPORTER), "--help"], env=env,
                         capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 0, ran.stderr
    # The path sits on its own line, the way the ccusage command does (#382):
    # in wrapped option help argparse breaks a long path mid-word, and a path
    # that cannot be copied names no folder.
    assert f"Default --source: {default}" in ran.stdout.splitlines(), (
        f"--help never names the default store as one copyable line: {ran.stdout}")
    assert "state root's" not in ran.stdout, "--help still points at a root it does not print"


@pytest.mark.parametrize("since", ["2026-11-10", "2026111", ""])
def test_a_date_in_the_wrong_format_names_the_format_and_an_example_issue_382(tmp_path, since):
    """#382: --since 2026-11-10 answered with Python's own parse error — time
    data '2026-11-10' does not match format '%Y%m%d' — which names a strptime
    format, not the format the person is asked for. And the format asked for has
    to be enforced as stated: strptime reads unpadded fields (2026111 as
    2026-11-01), and an explicitly empty value slipped past a truthiness check
    into a full-history export, so both must reach the same answer."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    ran = subprocess.run(
        [sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows),
         "--since", since],
        capture_output=True, text=True, timeout=HANG_TIMEOUT)

    assert ran.returncode == 1
    assert since in ran.stderr  # the person's own input, named back
    assert "YYYYMMDD" in ran.stderr and "20261110" in ran.stderr


def test_an_unparseable_receipt_is_counted_and_named(tmp_path):
    """#636: a truncated or non-object receipt was skipped with a bare continue,
    no count and no name, while every other skip is both: a person could not tell
    one damaged row from none."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1))
    (rows / "cut.json").write_text('{"ts":')
    (rows / "scalar.json").write_text("7")
    stats = ue.export(rows, output)
    assert stats["rows"] == 1
    assert sorted(stats["unparsed"]) == ["cut.json", "scalar.json"]
    ran = subprocess.run([sys.executable, str(EXPORTER), "--output", str(output), "--source", str(rows)],
                         capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert ran.returncode == 0, ran.stderr
    assert "2 receipts not parseable, skipped: " in ran.stdout, ran.stdout
    assert "cut.json" in ran.stdout and "scalar.json" in ran.stdout
