import getpass
import http.server
import os
import socketserver

import pytest

# Fake gateways take the adapter's 8 concurrent probes; the stdlib default backlog
# of 5 makes macOS reset the overflow, which the adapter reads as a transport
# error and the tests saw as a randomly missing model.
socketserver.TCPServer.request_queue_size = 128

# #892: the ceiling probes in timeout_verdict_test.py sleep past the 60 s
# ceiling by design, so the suite never even collects them — they run only
# when CCMORPH_TIMEOUT_REPRO names them.
collect_ignore = [] if os.environ.get("CCMORPH_TIMEOUT_REPRO") else ["timeout_verdict_test.py"]


# HTTPServer.server_bind resolves its own address with socket.getfqdn, a reverse
# DNS lookup of 127.0.0.1 that can stall for tens of seconds on macOS; every fake
# gateway here binds loopback and never uses the name. Subprocess fakes the tests
# write out cannot see this file, so each of those carries FAKE_SERVER_NO_DNS.
def _bind_without_reverse_dns(self):
    socketserver.TCPServer.server_bind(self)
    self.server_name, self.server_port = self.server_address[:2]


http.server.HTTPServer.server_bind = _bind_without_reverse_dns


# Identity is ambient env, and per #774 env is a tool too: a subject that reads
# the machine's username asserts what that machine happens to carry. On the rig
# CPU worker nothing carries one — USER and LOGNAME are unset and the uid has no
# name — so getpass.getuser raises its no-username error, the first member of
# rc-hosts' TRANSIENT, so every reader's LOGIN_ERRORS guard swallowed it as a
# routine login failure: the rc-hosts suite was red there and green on the gate,
# where a username resolves (GH-809). The suite names itself one fixed name.
# Scripts that load_script runs consult this module attribute at call time, so
# the pin reaches them; a test that needs another name sets it after this fixture.
@pytest.fixture(autouse=True)
def fixed_identity(monkeypatch):
    monkeypatch.setattr(getpass, "getuser", lambda: "ccmorph-tester")


@pytest.hookimpl(tryfirst=True)
def pytest_cmdline_main(config):
    # Every core by default when pytest-xdist is installed; `-n0` or a plain
    # environment without xdist runs serially, as before.
    if (config.pluginmanager.hasplugin("xdist") and not hasattr(config, "workerinput")
            and config.option.numprocesses is None):
        config.option.numprocesses = "auto"
        config.option.dist = "worksteal"


@pytest.hookimpl(trylast=True)  # after pytest-timeout's own pytest_configure
def pytest_configure(config):
    # #892: pytest-timeout's signal method calls pytest.fail() wherever the
    # main thread happens to be. A test finishing just under the ceiling gets
    # it raised inside report construction (`TestReport.from_item_and_call`,
    # outside the runner's exception protection); the failure then escapes the
    # worker's protocol loop instead of becoming a FAILED line, the worker
    # never reports `runtest_protocol_complete`, and the controller's
    # bookkeeping asserts (`worker_workerfinished` → `assert not crashitem`):
    # the whole xdist session dies with counts and a traceback but no named
    # failure. On a worker the thread method is the safe verdict — its
    # os._exit surfaces as one FAILED naming the crashed test, and xdist
    # replaces the worker and finishes the session. The controller and serial
    # runs (CI's no-xdist install, `-n0`) keep signal: there the handler lands
    # as an ordinary per-test FAILED and there is no dsession to collapse.
    # Workers rebuild their config from the command line, so the choice has
    # to be made inside each worker, on the attribute `_get_item_settings`
    # reads (pytest-timeout exposes no public per-run method hook).
    if hasattr(config, "workerinput"):
        config._env_timeout_method = "thread"
