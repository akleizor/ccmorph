"""The #611 skip: a sandbox that refuses loopback binds must read as one loud
SKIP line, not as a suite of failures every Handoff re-discovers.

These cases hold the probe's contract — a permission refusal is the process's
one answer, a success costs exactly one bind, and anything that is not a
permission refusal propagates visibly and caches nothing (a transient
resource failure must never become a skipped suite on a capable runner) —
and the marker's wording, because the one loud line IS the promise to
whoever reads `pytest -q`.
"""

import pytest

import support


class _RefusedSocket:
    """The sandbox's answer: the bind itself is refused on permission."""

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, *args):
        raise PermissionError(1, "Operation not permitted")

    def close(self):
        pass


class _AllowedSocket:
    created = 0

    def __init__(self, *args, **kwargs):
        type(self).created += 1

    def bind(self, *args):
        pass

    def close(self):
        pass


class _TransientCreate:
    """Not the sandbox refusing: the fd table is momentarily full, so
    socket creation itself fails."""

    def __init__(self, *args, **kwargs):
        raise OSError(24, "Too many open files")


class _TransientBind:
    """The socket exists; the bind fails on a resource error, not on
    permission."""

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, *args):
        raise OSError(105, "No buffer space available")

    def close(self):
        pass


def _unprobed(monkeypatch):
    monkeypatch.setattr(support, "_LOOPBACK_BIND", None)


def test_a_permission_refusal_stands_for_the_whole_process(monkeypatch):
    _unprobed(monkeypatch)
    monkeypatch.setattr(support.socket, "socket", _RefusedSocket)
    assert support.can_bind_loopback() is False
    # The refusal is the sandbox's policy for this process's whole life, so
    # it stays the answer even after patching the probe's socket away.
    monkeypatch.setattr(support.socket, "socket", _AllowedSocket)
    assert support.can_bind_loopback() is False


def test_the_probe_reports_true_where_bind_is_allowed(monkeypatch):
    _unprobed(monkeypatch)
    monkeypatch.setattr(support.socket, "socket", _AllowedSocket)
    assert support.can_bind_loopback() is True


def test_a_allowed_bind_costs_one_socket_per_process(monkeypatch):
    _unprobed(monkeypatch)

    class _Counting(_AllowedSocket):
        created = 0

    monkeypatch.setattr(support.socket, "socket", _Counting)
    assert support.can_bind_loopback() is True
    assert support.can_bind_loopback() is True
    assert _Counting.created == 1


@pytest.mark.parametrize("transient", [_TransientCreate, _TransientBind],
                         ids=["socket-creation", "bind"])
def test_a_transient_error_stays_visible_and_caches_nothing(transient, monkeypatch):
    _unprobed(monkeypatch)
    monkeypatch.setattr(support.socket, "socket", transient)
    with pytest.raises(OSError):
        support.can_bind_loopback()
    assert support._LOOPBACK_BIND is None
    # Capability was never established, only waited on: once the resource
    # comes back the next probe runs and answers on its own.
    monkeypatch.setattr(support.socket, "socket", _AllowedSocket)
    assert support.can_bind_loopback() is True


def test_the_marker_carries_the_one_loud_skip_line():
    mark = support.needs_loopback
    assert mark.name == "skipif"
    assert mark.kwargs["reason"] == support.LOOPBACK_SKIP
    assert support.LOOPBACK_SKIP.startswith("SKIP:")
    assert "runs in CI" in support.LOOPBACK_SKIP


# GH-877: the pool probe releases the number it hands back, and a test binds
# it only seconds later in a child. On one shared draw space that gap belongs
# to every parallel worker: a sibling drew the same number and came up with
# its proxy on the route another test expects to be dead. xdist names every
# worker and its count in the environment, so each worker may draw only its
# own slice of the pool — the gap then holds a number no sibling drew.


def _draws_the_bottom_of_the_range(monkeypatch):
    """Pin every draw to its range's floor: on a shared pool all workers meet
    on one number (the old red), and the ranges the pool asks for are visible."""
    asked = []

    def randrange(lo, hi):
        asked.append((lo, hi))
        return lo

    monkeypatch.setattr(support.random, "randrange", randrange)
    return asked


def _holder(monkeypatch):
    """Record the numbers the probe tried to bind, succeeding always."""

    class _Recording(_AllowedSocket):
        def bind(self, addr):
            _Recording.held.add(addr[1])

    _Recording.held = set()
    monkeypatch.setattr(support.socket, "socket", _Recording)
    return _Recording


def test_two_parallel_workers_never_draw_the_same_number(monkeypatch):
    _draws_the_bottom_of_the_range(monkeypatch)
    holder = _holder(monkeypatch)
    drawn = []
    for wid in range(8):
        holder.held = set()
        monkeypatch.setenv("PYTEST_XDIST_WORKER", f"gw{wid}")
        monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "8")
        for _ in range(4):
            p = support.port_outside_the_ephemeral_pool()
            assert p in holder.held, "the draw never probed the number it returned"
        drawn.append(set(holder.held))
    for a in range(len(drawn)):
        for b in range(a + 1, len(drawn)):
            assert not drawn[a] & drawn[b], (
                f"xdist workers {a} and {b} hold the same port: {drawn[a] & drawn[b]}")


@pytest.mark.parametrize("worker_env", [
    {},                                                  # no xdist: CI's serial path
    {"PYTEST_XDIST_WORKER": "gw-x", "PYTEST_XDIST_WORKER_COUNT": "lots"},  # malformed
    {"PYTEST_XDIST_WORKER": "gw-1", "PYTEST_XDIST_WORKER_COUNT": "4"},     # index outside the run
    {"PYTEST_XDIST_WORKER": "gw65536", "PYTEST_XDIST_WORKER_COUNT": "4"},  # index outside the run
    {"PYTEST_XDIST_WORKER": "gw0", "PYTEST_XDIST_WORKER_COUNT": "99999"},  # more workers than numbers
    {"PYTEST_XDIST_WORKER": "gw3", "PYTEST_XDIST_WORKER_COUNT": "99999"},  # same, a nonzero worker
])
def test_the_pool_falls_back_to_the_whole_range(worker_env, monkeypatch):
    """Without a usable worker identity — no xdist at all (-n0, CI's serial
    path), or an env the pool cannot slice by — the pool draws its whole
    range, exactly as before, and never an empty or crashed one. The
    oversized-count cases hold the fallback all-or-none: gw0 and gw3 of the
    same run must not split one number between a slice and the full pool.
    """
    asked = _draws_the_bottom_of_the_range(monkeypatch)
    _holder(monkeypatch)
    for name in ("PYTEST_XDIST_WORKER", "PYTEST_XDIST_WORKER_COUNT"):
        if name in worker_env:
            monkeypatch.setenv(name, worker_env[name])
        else:
            monkeypatch.delenv(name, raising=False)
    support.port_outside_the_ephemeral_pool()
    assert asked == [(1024, 32768)]
