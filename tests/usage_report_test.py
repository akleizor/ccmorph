#!/usr/bin/env python3
"""gateway-shim report: numbers from fixture usage rows and outcomes, and no identifiers."""

import datetime
import json
import os
import runpy
import subprocess
from unittest import mock
import sys
import tempfile
import unittest
from pathlib import Path

from support import HANG_TIMEOUT

SHIM = Path(__file__).resolve().parent.parent / "libexec/gateway-shim"
NOW = datetime.datetime.now(datetime.timezone.utc)


def stamp(hours_ago):
    return (NOW - datetime.timedelta(hours=hours_ago)).isoformat()


class ReportTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.state = Path(tmp.name)
        (self.state / "ccproteus" / "usage").mkdir(parents=True)
        self.n = 0

    def row(self, task, model, cost, hours=1, attempt=1, **extra):
        self.n += 1
        usage = {"input_tokens": 5} if cost is None else {"input_tokens": 5, "cost": cost}
        path = self.state / "ccproteus" / "usage" / f"{self.n}.json"
        path.write_text(json.dumps({
            "task": task, "attempt": attempt, "served_model": model,
            "response_model": model, "requested_model": model, "timestamp": stamp(hours),
            "usage": usage, "complete": True, **extra}))
        return path

    def outcomes(self, *pairs):
        with open(self.state / "ccproteus" / "outcomes.jsonl", "w") as fh:
            for task, outcome in pairs:
                fh.write(json.dumps({"task": task, "outcome": outcome, "timestamp": stamp(0)}) + "\n")

    def report(self, *args):
        env = {"PATH": os.environ["PATH"],
               "XDG_STATE_HOME": str(self.state)}
        done = subprocess.run([sys.executable, str(SHIM), "report", *args], env=env,
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertEqual(done.returncode, 0, done.stderr)
        return done.stdout

    def test_report(self):
        self.row("secret-task-1", "sonnet-x", 2.0)
        self.row("secret-task-1", "opus-x", 3.0, attempt=2)    # off-route, same task
        self.row("secret-task-2", "sonnet-x", 1.0)
        self.row("secret-task-3", "opus-x", 4.0)
        self.row(None, "opus-x", 10.0)
        self.row("secret-task-2", "sonnet-x", None)            # unknown spend
        self.row("secret-task-old", "sonnet-x", 99.0, hours=72)  # outside window
        self.outcomes(("secret-task-1", "rejected"), ("secret-task-1", "accepted"),
                      ("secret-task-2", "accepted"), ("secret-task-3", "escalated"),
                      ("secret-task-3", "accepted"))
        out = self.report()
        self.assertIn("total spend $20.00", out)
        self.assertIn("unknown spend 1 rows", out)
        for gone in ("role", "hive_model"):
            self.assertNotIn(gone, out)
        self.assertIn("opus-x  $17.00", out)
        # One accepted task has unknown spend: never present the known subtotal as complete.
        self.assertIn("  accepted 3  n/a/task  first-pass 33.3%  escalated 33.3%", out)
        self.assertNotIn("secret", out)
        self.assertNotIn(str(self.state), out)
        self.assertNotIn("99", out)

    def test_scheduled_daily_report_publishes_only_aggregates_issue_268(self):
        # The scheduler runs exactly `report --days 1`; its stdout is what gets published.
        self.row("TASK-SENTINEL-1", "sonnet-x", 2.0, repo="repo-sentinel",
                 prompt="PROMPT-SENTINEL", cwd="/Users/sentinel/x", api_key="sk-sentinel000",
                 response_id="RESP-SENTINEL", turn_id="TURN-SENTINEL", route_id="ROUTE-SENTINEL",
                 profile="PROFILE-SENTINEL", requested_model="REQ-SENTINEL")
        self.row("TASK-SENTINEL-2", "opus-x", 3.0, repo="repo-sentinel")
        self.outcomes(("TASK-SENTINEL-1", "accepted"), ("TASK-SENTINEL-2", "escalated"))
        out = self.report("--days", "1")
        self.assertIn("total spend $5.00", out)
        self.assertIn("opus-x  $3.00", out)
        self.assertIn("sonnet-x  $2.00", out)
        self.assertIn("\n  accepted 1", out)
        for leak in ("SENTINEL", "sentinel", "sk-", "/Users/", str(self.state)):
            self.assertNotIn(leak, out)

    def test_days_widens_window(self):
        self.row("t", "sonnet-x", 5.0, hours=72)
        self.assertIn("total spend $0.00", self.report())
        self.assertIn("total spend $5.00", self.report("--days", "5"))

    def test_empty_state(self):
        self.assertIn("total spend $0.00", self.report())

    def test_atomic_writer_temporary_is_not_a_second_response(self):
        shim = runpy.run_path(str(SHIM))
        sidecar = shim["UsageSidecar"]("fixture", "identity").tagged(
            {"task": "atomic-task", "attempt": 1})
        sidecar.served_model = "sonnet-x"
        record = sidecar._start("response", "sonnet-x", "sonnet-x", {"cost": 5})
        record["complete"] = True
        self.outcomes(("atomic-task", "accepted"))
        replace = os.replace
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            sidecar._write(record)
            def paused_replace(source, destination):
                out = self.report()
                self.assertIn("total spend $5.00", out)
                self.assertIn("  accepted 1  $5.00/task", out)
                replace(source, destination)
            with mock.patch("os.replace", side_effect=paused_replace):
                sidecar._write(record)

    def test_same_timestamp_turns_keep_summed_usage_issue_311(self):
        shim = runpy.run_path(str(SHIM))
        clock = mock.Mock(wraps=datetime.datetime)
        clock.now.return_value = NOW
        records = []
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}), \
                mock.patch.object(shim["datetime"], "datetime", clock):
            for profile, tokens, cost in (("first", 15, 1), ("second", 16, 2)):
                sidecar = shim["UsageSidecar"](profile, profile + "-route")
                record = sidecar.observe({"type": "message_start", "message": {
                    "id": "recycled", "model": "sonnet-x",
                    "usage": {"output_tokens": tokens, "cost": cost}}}, None, "sonnet-x")
                sidecar.observe({"type": "message_stop"}, record, "sonnet-x")
                records.append(record)
        self.assertEqual(records[0]["timestamp"], records[1]["timestamp"])
        rows = [json.loads(path.read_text())
                for path in (self.state / "ccproteus" / "usage").glob("*.json")]
        self.assertEqual(len(rows), 2)
        self.assertEqual(sum(row["usage"]["output_tokens"] for row in rows), 31)
        self.assertIn("total spend $3.00", self.report())

    def test_unknown_accepted_cost_is_not_zero_or_a_known_subtotal(self):
        for known in (None, 2):
            with self.subTest(known=known):
                self.row("unpriced", "sonnet-x", known)
                self.outcomes(("unpriced", "accepted"))
                self.assertIn("  accepted 1  n/a/task", self.report())

    def test_accepted_task_cost_includes_earlier_usage(self):
        self.row("long-task", "sonnet-x", 10, hours=72)
        self.row("long-task", "sonnet-x", 1)
        self.outcomes(("long-task", "accepted"))
        out = self.report()
        self.assertIn("total spend $1.00", out)
        self.assertIn("  accepted 1  $11.00/task", out)

    def test_malformed_outcome_does_not_create_empty_history(self):
        self.row("broken", "sonnet-x", 1)
        (self.state / "ccproteus" / "outcomes.jsonl").write_text(
            json.dumps({"task": "broken", "timestamp": stamp(0)}) + "\n")
        self.assertNotIn("  accepted", self.report())

    def test_non_object_usage_is_ignored(self):
        path = self.row("broken", "sonnet-x", 1)
        row = json.loads(path.read_text())
        row["usage"] = [1]
        path.write_text(json.dumps(row))
        self.assertIn("total spend $0.00", self.report())

    def test_outcome_window_selects_new_verdicts_not_new_usage(self):
        self.row("new-acceptance", "sonnet-x", 10, hours=72)
        self.row("old-acceptance", "opus-x", 1)
        self.outcomes(("new-acceptance", "accepted"))
        with open(self.state / "ccproteus" / "outcomes.jsonl", "a") as fh:
            fh.write(json.dumps({"task": "old-acceptance", "outcome": "accepted",
                                 "timestamp": stamp(72)}) + "\n")
        out = self.report()
        self.assertIn("  accepted 1  $10.00/task", out)

    def test_retry_or_parked_task_is_not_first_pass(self):
        self.row("retry", "sonnet-x", 1)
        self.row("retry", "sonnet-x", 2, attempt=2)
        self.row("parked", "opus-x", 3)
        self.outcomes(("retry", "accepted"), ("parked", "parked"), ("parked", "accepted"))
        out = self.report()
        self.assertIn("  accepted 2  $3.00/task  first-pass 0.0%", out)

    def test_missing_served_identity_is_not_invented_from_request(self):
        path = self.row("missing-model", "sonnet-x", 1)
        row = json.loads(path.read_text())
        row.pop("served_model")
        row.pop("response_model")
        path.write_text(json.dumps(row))
        out = self.report()
        self.assertIn("(unknown)  $1.00", out)
        self.assertNotIn("sonnet-x  $1.00", out)

    def test_model_label_cannot_forge_report_lines(self):
        self.row("forged", "opus-x\nFORGED $999\x1b[2J", 1)
        out = self.report()
        self.assertNotIn("\nFORGED", out)
        self.assertNotIn("\x1b", out)
        self.assertIn("total spend $1.00", out)

    def test_invalid_billed_costs_are_unknown_not_spend(self):
        for value in (-1, float("inf"), float("nan"), True, "2"):
            self.row("bad-cost", "sonnet-x", value)
        self.outcomes(("bad-cost", "accepted"))
        out = self.report()
        self.assertIn("total spend $0.00", out)
        self.assertIn("unknown spend 5 rows", out)
        self.assertIn("  accepted 1  n/a/task", out)

    def test_huge_costs_are_unknown_not_fatal(self):
        self.row("huge-cost", "sonnet-x", 10 ** 400)   # no float holds it, even alone (#313)
        self.row("huge-cost", "sonnet-x", 10 ** 308)   # holds a float alone; the sum does not
        self.row("huge-cost", "sonnet-x", 10 ** 308)
        self.row("huge-cost", "sonnet-x", 1e308)
        self.row("fine-cost", "sonnet-x", 2.0)
        self.outcomes(("fine-cost", "accepted"))
        out = self.report()
        self.assertIn("total spend $2.00", out)
        self.assertIn("unknown spend 4 rows", out)
        self.assertIn("  accepted 1  $2.00/task", out)

    def test_running_total_overflow_is_unknown_not_inf(self):
        # Each cost is finite per row; two near the float ceiling used to
        # carry the running total past the range and print $inf.
        self.row("huge-sum", "sonnet-x", 1e308)
        self.row("huge-sum", "sonnet-x", 1e308)
        self.row("fine-cost", "opus-x", 2.0)
        out = self.report()
        self.assertNotIn("inf", out)
        self.assertIn("total spend $2.00", out)
        self.assertIn("unknown spend 2 rows", out)
        self.assertIn("opus-x  $2.00", out)

    def test_outcome_without_usage_has_unknown_cost(self):
        self.outcomes(("missing-usage", "accepted"))
        self.assertIn("  accepted 1  n/a/task  first-pass 0.0%", self.report())

    def test_incomplete_producer_receipt_keeps_spend_but_not_complete_task_cost(self):
        shim = runpy.run_path(str(SHIM))
        sidecar = shim["UsageSidecar"]("fixture", "identity").tagged(
            {"task": "retry-partial", "attempt": 1})
        sidecar.served_model = "sonnet-x"
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": str(self.state)}):
            sidecar.observe({"type": "message_start", "message": {
                "id": "partial", "model": "sonnet-x", "usage": {"cost": 1}}}, None, "sonnet-x")
            retry = sidecar.tagged({"task": "retry-partial", "attempt": 2})
            record = retry.observe({"type": "message_start", "message": {
                "id": "success", "model": "sonnet-x", "usage": {"cost": 2}}}, None, "sonnet-x")
            retry.observe({"type": "message_stop"}, record, "sonnet-x")
        self.outcomes(("retry-partial", "accepted"))
        out = self.report()
        self.assertIn("total spend $3.00", out)
        self.assertIn("  accepted 1  n/a/task", out)

    def test_invalid_days_are_bounded_cli_errors(self):
        env = {"PATH": os.environ["PATH"], "XDG_STATE_HOME": str(self.state)}
        for days in ("inf", "1000000000", "999999999", "1000000", "nan", "-1", "0"):
            with self.subTest(days=days):
                done = subprocess.run([sys.executable, str(SHIM), "report", "--days", days],
                                      env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
                self.assertNotEqual(done.returncode, 0)
                self.assertNotIn("Traceback", done.stderr)
                self.assertIn("report [--days N]", done.stderr)
        self.assertIn("last 0.5 day(s)", self.report("--days", "0.5"))

    def test_invalid_utf8_outcome_line_does_not_hide_valid_verdicts(self):
        self.row("valid-before", "sonnet-x", 1)
        self.row("valid-after", "sonnet-x", 2)
        self.outcomes(("valid-before", "accepted"))
        with open(self.state / "ccproteus" / "outcomes.jsonl", "ab") as fh:
            fh.write(b"\xff\n")
            fh.write(json.dumps({"task": "valid-after", "outcome": "accepted",
                                 "timestamp": stamp(0)}).encode() + b"\n")
        self.assertIn("  accepted 2  $1.50/task", self.report())

    def test_last_valid_outcome_wins_and_invalid_rows_are_ignored(self):
        self.row("verdict", "sonnet-x", 1)
        self.outcomes(("verdict", "accepted"), ("verdict", "rejected"))
        with open(self.state / "ccproteus" / "outcomes.jsonl", "a") as fh:
            for row in ([], {"task": "verdict", "outcome": "invented", "timestamp": stamp(0)},
                        {"task": "verdict", "outcome": "accepted", "timestamp": "broken"}):
                fh.write(json.dumps(row) + "\n")
        self.assertIn("  accepted 0  n/a/task  first-pass 0.0%", self.report())


if __name__ == "__main__":
    unittest.main()
