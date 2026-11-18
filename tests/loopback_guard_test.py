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
