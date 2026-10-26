"""The metadata-only ccusage export: one record per receipt, one folder per profile, damaged costs reported."""
import datetime as dt
import importlib.machinery
import importlib.util
import json
import os
import pathlib
import sys
import time

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from gateway_shim_test import load_shim_module  # noqa: E402
from rc_hosts_test import is_python_helper  # noqa: E402

SPEC = importlib.util.spec_from_loader("usage_export", importlib.machinery.SourceFileLoader(
    "usage_export", str(pathlib.Path(__file__).parent.parent / "scripts/usage-export")))
ue = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ue)

ABSENT = object()


@pytest.fixture(autouse=True)
def hong_kong(monkeypatch):
    """Days are local days; pin the zone so the day boundaries below do not move with the host."""
    monkeypatch.setenv("TZ", "Asia/Hong_Kong")
    time.tzset()
    yield
    monkeypatch.undo()
    time.tzset()


def local(year, month, day, hour=0):
    return dt.datetime(year, month, day, hour).astimezone()


def receipt(rid, model, stamp, cost=None, complete=True,
            requested=ABSENT, served=ABSENT, input_tokens=1_000_000):
    """One schema-1 receipt. `model` is the identity the client is answered with;
    `requested` and `served` are the shim's recorded ids, absent by default — the
    row shape a receipt had before served_model existed. Pass None for a null id."""
    usage = {"input_tokens": input_tokens}
    if cost is not None:
        usage["cost"] = cost
    row = {"schema": 1, "response_id": rid, "response_model": model, "timestamp": stamp,
           "task": None, "attempt": 1, "profile": "ledger", "complete": complete, "usage": usage}
    if requested is not ABSENT:
        row["requested_model"] = requested
    if served is not ABSENT:
        row["served_model"] = served
    return row


def mapped_receipt(rid, stamp, cost=None):
    """The exact row gateway-shim writes for a profile model map: the picker alias was
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
    path = output / "projects" / "ccmorph-ledger" / f"{day}.jsonl"
    return [json.loads(l) for l in path.read_text().splitlines() if l]


@pytest.mark.parametrize("damage", [
    pytest.param({"cost": 10 ** 400}, id="float-overflowing-cost-issue-368"),
    pytest.param({"cost": 1e14}, id="over-limit-gateway-cost-issue-413"),
])
def test_a_receipt_with_a_damaged_cost_is_counted_not_fatal(tmp_path, damage):
    """A billed cost too large for a float, or past the shim's COST_LIMIT — a finite
    number no turn could ever be billed, so a stored one is damage after the fact —
    is counted unpriced next to a good row, never exported and never fatal."""
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
    """The exporter's bound is the writer's own: gateway-shim's bucket filter keeps
    a receipt's cost only below its COST_LIMIT, and this reader must never bill a
    value the writer would drop. The boundary is pinned from both sides — a cost the
    shim keeps still bills as a gateway cost, the limit itself (the first value it
    refuses) exports unpriced — and the two literals are one number, so neither
    script's bound can drift from the other's."""
    shim = load_shim_module()
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "kept.json", receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00", cost=1e13))
    write(rows, "refused.json", receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00",
                                        cost=shim.COST_LIMIT))

    stats = ue.export(rows, output)

    day = read_day(output, "2026-10-16")
    assert [r["costUSD"] for r in day] == [1e13] and stats["usd"] == 1e13
    assert stats["unpriced"] == 1 and stats["gateway_rows"] == 1
    assert ue.COST_LIMIT == shim.COST_LIMIT


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
    ledger = output / "projects" / "ccmorph-ledger"
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


def test_a_since_rerun_reconciles_only_its_range(tmp_path):
    """--since rewrites its own range and nothing else: the day it has no receipt for
    goes, an earlier day stays, and every file the exporter does not own is left alone."""
    rows, output = tmp_path / "rows", tmp_path / "out"
    ledger = output / "projects" / "ccmorph-ledger"
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

    ledger = output / "projects" / "ccmorph-ledger"
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
    rows, output = tmp_path / "rows", tmp_path / "out"
    rows.mkdir()
    write(rows, "a.json", {**receipt("a", "claude-opus-5-5", "2026-10-16T01:00:00+00:00"), "profile": "work"})
    write(rows, "b.json", {**receipt("b", "claude-opus-5-5", "2026-10-16T02:00:00+00:00"), "profile": "a/b"})
    write(rows, "c.json", {**receipt("c", "claude-opus-5-5", "2026-10-16T03:00:00+00:00"), "profile": None})

    ue.export(rows, output)

    folders = sorted(p.name for p in (output / "projects").iterdir())
    assert folders == ["ccmorph-a_b", "ccmorph-unknown", "ccmorph-work"]
    assert len(read_day_in(output, "ccmorph-work", "2026-10-16")) == 1


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
    assert is_python_helper(pathlib.Path(SPEC.origin).read_text())
