"""The 60 s ceiling is a per-test verdict, never a session killer (#892).

pytest-timeout's signal method calls `pytest.fail()` wherever the worker's
main thread happens to be when SIGALRM lands. `call_and_report` builds the
report (`TestReport.from_item_and_call`) outside the runner's exception
protection, so a test that finishes just under the ceiling gets the failure
raised *inside report construction*: it escapes the xdist worker's protocol
loop instead of becoming a FAILED line, the worker ends its session without
`runtest_protocol_complete`, and the controller asserts on the orphaned crash
item (xdist's `worker_workerfinished` → `assert not crashitem`). The gate then
ends with counts and a traceback but no named failure — the exact collapse the
2026-11-25 night gate recorded as `142 failed ... in 1718.28s` with not one
FAILED line.

These probes are cross-the-ceiling by design and are not collected unless
`CCMORPH_TIMEOUT_REPRO` names them (tests/conftest.py), because the suite must
never pay a minute for them. Run against the landed pytest.ini ceiling:

    CCMORPH_TIMEOUT_REPRO=1 uv run python -m pytest tests/timeout_verdict_test.py -q -k mid_report

The first probe is the regression: with the signal method on an xdist worker
this run collapses with an INTERNALERROR and no named failure; with the
enforcement conftest.py selects for workers (the thread method — the timer
thread ends the worker, and xdist names the crashed test and replaces the
worker) the same run ends with exactly one named FAILED line — the probe's own
nodeid — and a surviving session summary. The second probe walks a timeout the
ordinary way, plainly inside the test body, and is named under either method;
it pins that the chosen enforcement still delivers a per-test verdict.
"""
import time

from _pytest import reports

_from_item_and_call = reports.TestReport.from_item_and_call.__func__

# The probe returns 1.5 s before the ceiling and then holds the worker inside
# report construction for 4 s, so the alarm — armed at protocol start — lands
# mid-report no matter how few milliseconds the fixture setup consumed.
WAKE_BEFORE_CEILING = 1.5
WIDEN_REPORT_WINDOW = 4.0


def test_a_timeout_mid_report_names_the_test_and_keeps_the_session(request, monkeypatch):
    ceiling = float(request.config.getini("timeout"))
    me = request.node.nodeid

    def widened(cls, item, call):
        if item.nodeid == me and call.when == "call":
            time.sleep(WIDEN_REPORT_WINDOW)
        return _from_item_and_call(cls, item, call)

    monkeypatch.setattr(reports.TestReport, "from_item_and_call", classmethod(widened))
    time.sleep(max(0.0, ceiling - WAKE_BEFORE_CEILING))


def test_a_timeout_in_the_test_body_names_the_test_and_keeps_the_session(request):
    time.sleep(float(request.config.getini("timeout")) + 5)
