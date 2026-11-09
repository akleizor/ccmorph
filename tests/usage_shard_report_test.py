#!/usr/bin/env python3
"""shim report across the one-time move of flat usage rows into UTC day folders (#227)."""

import datetime
import fcntl
import os
import unittest

import usage_report_test as base
from support import needs_shim

pytestmark = needs_shim


class ShardReportTest(unittest.TestCase):
    # The fixture helpers of usage_report_test.ReportTest, borrowed rather than
    # inherited so its tests are not collected a second time here.
    setUp = base.ReportTest.setUp
    row = base.ReportTest.row
    outcomes = base.ReportTest.outcomes
    report = base.ReportTest.report

    def age(self, path, hours):
        """Pin a row's mtime to its stamp's age, so the move's mtime day is the
        row's own day and the window alone decides what a read counts."""
        when = (base.NOW - datetime.timedelta(hours=hours)).timestamp()
        os.utime(path, (when, when))

    def test_totals_survive_the_move_into_day_folders(self):
        # The report reads the flat store, prints its totals, then moves each
        # flat row into the day folder of its mtime; the next report reads the
        # day folders instead. The move must not change a total.
        # A row older than the window still counts for its task: whole-task cost.
        for task, model, cost, hours, attempt in (
            ("moved-task", "sonnet-x", 2.0, 1, 1),
            ("moved-task", "opus-x", 3.0, 2, 2),
            (None, "sonnet-x", 0.5, 3, 1),
            ("moved-task", "sonnet-x", 10.0, 72, 1),  # before the window
            ("old-task", "sonnet-x", 99.0, 72, 1),    # outside it, no verdict
        ):
            # At least two hours old: the move leaves a fresher flat row for
            # the writer an upgrade may still have running.
            self.age(self.row(task, model, cost, hours=hours, attempt=attempt), max(hours, 2))
        self.outcomes(("moved-task", "accepted"))
        before = self.report()
        after = self.report()
        self.assertEqual(before, after)
        self.assertIn("total spend $5.50", after)
        self.assertIn("  accepted 1  $15.00/task", after)
        usage = self.state / "ccmorph" / "usage"
        self.assertEqual(list(usage.glob("*.json")), [])
        self.assertEqual(len(list(usage.glob("*/*.json"))), 5)
        self.assertTrue(any(folder.name < (base.NOW - datetime.timedelta(hours=48)).strftime("%Y-%m-%d")
                            for folder in usage.iterdir() if folder.is_dir()), list(usage.iterdir()))

    def test_a_held_move_lock_leaves_the_store_to_its_holder(self):
        # Two reports can run at once. A run that cannot take the store's move
        # lock leaves the move to its holder, so one run's rollback can never
        # meet another run's remove and take a row's last name (#298).
        usage = self.state / "ccmorph" / "usage"
        path = self.row(None, "sonnet-x", 1.0, hours=2)
        self.age(path, 2)
        with open(usage / ".move.lock", "w") as held:
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.report()
            self.assertTrue(path.exists(), "a move ran beside its lock's holder")
            fcntl.flock(held, fcntl.LOCK_UN)
        self.report()
        self.assertFalse(path.exists(), "the move did not run once the lock was free")
        self.assertEqual(len(list(usage.glob("*/*.json"))), 1)


if __name__ == "__main__":
    unittest.main()
