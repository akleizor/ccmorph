import functools
import getpass
import http.server
import os
import signal
import socketserver
import subprocess
import sys
import threading

import pytest

from support import SPAWN_TEST_BUDGET

if sys.version_info < (3, 11):  # pytest's own dependency below 3.11, as its runner uses it
    from exceptiongroup import BaseExceptionGroup

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


# GH-976: the suite's own load starved tests that wait on a real process past
# pytest.ini's 60 s ceiling (#932, #976). Which test is that is decided by the
# process, not by a static walk: the wrappers below note every real start
# (subprocess.Popen, the os spawn forms) while a test is on the clock, and the
# first one extends that test's own pytest-timeout timer to SPAWN_TEST_BUDGET.
# A Popen monkeypatched to a stub never reaches the wrapper (no process starts);
# a wrapper that delegates to the saved real Popen does — so a stubbed-away
# sidecar keeps main's 60 s and a delegating start gets the chain.
#
# An explicit marker is never raised: one below the budget becomes a verdict at
# teardown naming the test and the marker, so a stale literal cannot ride in
# under the grant.
_SPAWN = {"item": None, "granted": False, "verdict": None}
_SPAWN_LOCK = threading.RLock()
_SPAWN_COUNT = [0]
# (fixture name, node) pairs whose non-function-scoped setup started a child:
# every later test that requests that fixture waits on the same child.
_HOLDERS = set()


def _grant(item):
    """Extend this test's timer; answer a callable that puts it back when the
    call it was granted for turns out to start nothing."""
    marker = item.get_closest_marker("timeout")
    if marker is not None:
        value = marker.args[0] if marker.args else marker.kwargs.get("timeout")
        if value is not None and float(value) < SPAWN_TEST_BUDGET:
            _SPAWN["verdict"] = (
                f"{item.nodeid} starts a process under an explicit {float(value):g}s timeout "
                f"marker, below the spawn budget {SPAWN_TEST_BUDGET:g}s "
                f"(HANG_TIMEOUT + SPAWN_SLACK)")
        return None
    try:
        import pytest_timeout
    except ImportError:
        return None
    settings = pytest_timeout._get_item_settings(item)
    if settings.func_only or not settings.timeout or settings.timeout >= SPAWN_TEST_BUDGET:
        return None
    if settings.method == "signal" and threading.current_thread() is not threading.main_thread():
        # The only timer call safe off the main thread: the handler the session
        # installed stays, only the deadline moves.
        signal.setitimer(signal.ITIMER_REAL, SPAWN_TEST_BUDGET)
        return lambda: signal.setitimer(signal.ITIMER_REAL, settings.timeout)
    hook = item.config.pluginmanager.hook
    hook.pytest_timeout_cancel_timer(item=item)
    hook.pytest_timeout_set_timer(item=item, settings=settings._replace(timeout=SPAWN_TEST_BUDGET))

    def restore():
        hook.pytest_timeout_cancel_timer(item=item)
        hook.pytest_timeout_set_timer(item=item, settings=settings)
    return restore


def _grant_once():
    """Grant once per test; answer a callable that un-grants an attempt which
    did not start a child, so a later real start can still grant."""
    item = _SPAWN["item"]
    if item is None:
        return None
    with _SPAWN_LOCK:
        if _SPAWN["granted"]:
            return None
        _SPAWN["granted"] = True
        before = _SPAWN["verdict"]
        try:
            restore = _grant(item)
        except Exception as exc:
            # A grant that cannot extend becomes a named verdict, never an
            # exception thrown into the test's own spawn call.
            _SPAWN["verdict"] = f"{item.nodeid}: the spawn grant could not extend the timeout: {exc!r}"
            restore = None

        def undo():
            with _SPAWN_LOCK:
                try:
                    if restore is not None:
                        restore()
                finally:
                    _SPAWN["verdict"] = before
                    _SPAWN["granted"] = False
        return undo


def _spawn_before():
    _SPAWN_COUNT[0] += 1
    undo_grant = _grant_once()

    def undo():
        _SPAWN_COUNT[0] -= 1
        if undo_grant is not None:
            undo_grant()
    return undo


def _spawn_after():
    _SPAWN_COUNT[0] += 1
    _grant_once()


def _through_before(real):
    """For a spawner that blocks until its child ends (os.system): the timer has
    to move before the wait, and a call that raises before any child exists is
    put back."""
    @functools.wraps(real)
    def wrapper(*args, **kwargs):
        undo = _spawn_before()
        try:
            return real(*args, **kwargs)
        except BaseException:
            undo()
            raise
    wrapper._ccm_spawn_wrapped = True
    return wrapper


def _through_after(real):
    """For a spawner that returns once its child exists: Popen validates its
    arguments inside __init__, so a call it rejects never reaches this count and
    is not a start."""
    @functools.wraps(real)
    def wrapper(*args, **kwargs):
        result = real(*args, **kwargs)
        _spawn_after()
        return result
    wrapper._ccm_spawn_wrapped = True
    return wrapper


# A Popen monkeypatched to a stub never reaches the wrapper (no process starts);
# a wrapper that delegates to the saved real Popen does, so it is a start.
if not getattr(subprocess.Popen.__init__, "_ccm_spawn_wrapped", False):
    subprocess.Popen.__init__ = _through_after(subprocess.Popen.__init__)
    for _name in ("fork", "forkpty", "posix_spawn", "posix_spawnp",
                  "spawnl", "spawnle", "spawnlp", "spawnlpe", "spawnv", "spawnve", "spawnvp", "spawnvpe"):
        if hasattr(os, _name):
            setattr(os, _name, _through_after(getattr(os, _name)))
    if hasattr(os, "system"):
        os.system = _through_before(os.system)


@pytest.hookimpl(wrapper=True)
def pytest_fixture_setup(fixturedef, request):
    """Record a wider-than-function fixture whose setup started a child: a test
    that waits on that child through the fixture gets the budget too, even
    though it starts nothing itself (gateway_shim_test's setUpClass shim)."""
    before = _SPAWN_COUNT[0]
    try:
        return (yield)
    finally:
        if fixturedef.scope != "function" and _SPAWN_COUNT[0] != before:
            _HOLDERS.add((fixturedef.argname, request.node))


# GH-919: every worker's OWN descendant tree is read while each test runs, by a
# watcher process outside that tree (proc_pressure_test.RunTreeWatch); a test
# whose tree passes RUN_SHARE of the per-user cap fails at its teardown, named.
# The window is opened by a tryfirst setup hook and closed by a teardown hook
# wrapper, not by a fixture: a fixture opens it after every higher-scoped
# fixture (a module's or the session's) has been built and closes it before they
# are torn down, so what those hold would belong to no test.
_RUN_TREE = {}


def _run_tree_watch():
    """This process's watch, started by its first test (None: no finite cap,
    where the in-file probes skip and say why, or a copy of this file run away
    from tests/)."""
    if "watch" not in _RUN_TREE:
        try:
            import proc_pressure_test as probe
        except ModuleNotFoundError as missing:
            # A copy of this conftest in an inner run away from tests/
            # (timeout_headroom_test's probe): that run is a descendant of the
            # outer worker, whose own watch already reads its tree. Beside the
            # module, a failed import is a broken suite and stays loud.
            beside = os.path.join(os.path.dirname(__file__), "proc_pressure_test.py")
            if missing.name != "proc_pressure_test" or os.path.exists(beside):
                raise
            _RUN_TREE["watch"] = None
            return None
        cap = probe.per_user_cap()
        _RUN_TREE["watch"] = None if cap is None else probe.RunTreeWatch(
            cap[0], probe.RUN_SHARE, f"the {cap[1]} cap").start()
    return _RUN_TREE["watch"]


@pytest.hookimpl(tryfirst=True)
def pytest_runtest_setup(item):
    watch = _run_tree_watch()
    if watch is not None:
        watch.begin(item.nodeid)
    # After the watcher: its own Popen (a worker's first test) must not grant
    # this test, which has not started anything.
    _SPAWN.update(item=item, granted=False, verdict=None)
    if any((name, node) in _HOLDERS for name in item.fixturenames for node in item.listchain()):
        _grant_once()


def _run_tree_verdict():
    """Close this test's window: the GH-919 text when its tree passed the share
    or the watch could not read the table, else None."""
    watch = _RUN_TREE.get("watch")
    return watch.end() if watch is not None else None


def _teardown_verdict():
    """Close both windows: the GH-919 text and the spawn grant's, joined."""
    verdict = _SPAWN["verdict"]
    _SPAWN.update(item=None, verdict=None)
    return "; and ".join(m for m in (_run_tree_verdict(), verdict) if m) or None


@pytest.hookimpl(wrapper=True)
def pytest_runtest_teardown(item, nextitem):
    # A wrapper, not a trylast impl: pytest tears every fixture down inside the
    # yield, and when one of them raises or skips the hook chain stops there, so
    # an impl ordered after it never ran - the window stayed open, the next
    # test's begin() discarded its samples, and a tree past the share passed
    # green (#hive-repro-38948e07). The code after the yield runs however the
    # teardown ended, and keeps that ending beside the verdict, the way pytest
    # itself reports two finalizers that both failed.
    try:
        result = yield
    except BaseException as ended:
        message = _teardown_verdict()
        if message is None or isinstance(ended, KeyboardInterrupt):
            raise  # ^C: the run stops as interrupted (exit 2), never green
        if isinstance(ended, pytest.exit.Exception):  # a stop pytest must still see
            raise pytest.exit.Exception(f"{ended.msg}; and {message}",
                                        returncode=ended.returncode) from None
        raise BaseExceptionGroup("errors during test teardown", [
            ended, pytest.fail.Exception(message, pytrace=False)]) from None
    message = _teardown_verdict()
    if message:
        pytest.fail(message, pytrace=False)
    return result


def pytest_sessionfinish(session):
    watch = _RUN_TREE.pop("watch", None)
    if watch is not None:
        watch.stop()


@pytest.fixture(scope="session")
def run_tree_watch():
    return _run_tree_watch()
