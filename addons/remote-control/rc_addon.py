"""mitmproxy addon: the part of the Remote Control proxy that lives as long as the
proxy. Loaded only by ccmorph-rc. It holds the route key and every endpoint handed
to it; what each request does is rc_flow.py (with rc_route.py), which it reloads
when either file changes, so a routing change needs no proxy restart and no
session is cut off. A change to this file still needs one.

Configuration arrives through the environment, never argv. The route key itself
arrives as a 0600 file that is deleted on read, so it is in neither the process
table nor any environment:
  RC_UPSTREAM     route endpoint (the adapter's loopback URL or the profile's base_url)
  RC_KEY_FILE     route credential file, removed once read
  RC_PARENT_PID   pid to follow; the proxy shuts down when it disappears
  RC_PARENT_STARTED  that process's start (rc_route.proc_started): which edge a handoff is for
  RC_LOG          optional path; one line per request, method and path only
  RC_TIERS        optional JSON {alias: model}: tiers the config pinned (rc-hosts)
  RC_SOURCE       optional folder rc_flow.py and rc_route.py are reloaded from (the install's)
  RC_INFERENCE    optional path; "account", "route" or a profile name, read on every turn.
                  A profile's endpoint arrives as <dir>/switch-<profile>.json (0600), taken
                  within a second and deleted on read, by the proxy of the edge it names
                  only. What this proxy holds is <dir>/held.<parent pid>.json ({profile:
                  handoff id}, no key): --switch lands its word only once that names its
                  handoff, and a restarted proxy starts it empty, so rc-hosts re-hands the
                  live profile instead of leaving the phone on the route (issue #595).
                  <dir>/sessions/<session id> holds one
                  session's own word (--switch --session), <dir>/account-tiers.json the
                  account's {alias: model} for it
"""
from __future__ import annotations

import importlib.util
import json
import os
import shutil
import signal
import sys
import tempfile
import threading
import time
from pathlib import Path
from types import SimpleNamespace

HERE = Path(__file__).resolve().parent
UPSTREAM = os.environ["RC_UPSTREAM"]
_key_file = os.environ.pop("RC_KEY_FILE")
KEY = Path(_key_file).read_text().strip()
os.unlink(_key_file)
LOG = os.environ.get("RC_LOG")
PARENT = int(os.environ.get("RC_PARENT_PID") or 0)
PARENT_STARTED = os.environ.get("RC_PARENT_STARTED") or None
TIERS = json.loads(os.environ.get("RC_TIERS") or "{}")
INFERENCE = os.environ.get("RC_INFERENCE")
SOURCE = os.environ.get("RC_SOURCE")
SWITCHED: dict[str, tuple[str, str, dict]] = {}   # profile -> (upstream, key, tiers), handed over by --switch
HELD: dict[str, str] = {}                          # profile -> the id of the handoff SWITCHED holds
_TAKE = threading.Lock()                           # the poller and a request both take handoffs
MODULES = ("rc_route", "rc_flow")                  # in import order: rc_flow imports rc_route
RECHECK = 1.0                                      # seconds between looks at the source files

_LOG = open(LOG, "a", buffering=1) if LOG else None   # line-buffered: one write per request


def _log(line: str) -> None:
    if _LOG:
        _LOG.write(f"{time.strftime('%H:%M:%S')} {line}\n")


def _own_tag() -> str:
    """This proxy's own start as a file-name part (rc_route.start_tag), read once at
    load. The name of a handoff it is taking carries it, so the sweep can tell this
    proxy, killed mid-take, from a later process the OS gives its pid (issue #595
    review). rc_route is loaded from beside this file as _fresh loads it, never
    through sys.modules; a start that cannot be read is "unknown", which the sweep
    judges by the pid alone."""
    try:
        spec = importlib.util.spec_from_file_location("_rc_route_own", HERE / "rc_route.py")
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return mod.start_tag(mod.proc_started(os.getpid()))
    except Exception:
        return "unknown"


OWN_TAG = _own_tag()


def _held_path() -> Path:
    return Path(INFERENCE).parent / f"held.{PARENT}.json"


def _publish_held() -> None:
    """Say which handoffs this proxy holds (no key): held.<parent pid>.json, replaced
    whole. --switch waits for its handoff's id here, and rc-hosts reads it to see a
    proxy that restarted and holds nothing (issue #595)."""
    if not (INFERENCE and PARENT):
        return
    path = _held_path()
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as fh:
            json.dump({"edge": PARENT, "edge_started": PARENT_STARTED, "profiles": dict(HELD)}, fh)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _mine(h: dict) -> bool:
    """Whether handoff `h` is for this proxy's own edge: that pid and that start."""
    return not PARENT or (h.get("edge", PARENT) == PARENT
                          and h.get("edge_started", PARENT_STARTED) == PARENT_STARTED)


def _endpoint(h) -> tuple[str, str, dict] | None:
    """The endpoint a parsed handoff carries, or None. Without upstream and key
    there is nothing to route to: such a file holds a credential for nothing, and
    the take removes it without recording (issue #900)."""
    try:
        return (h["upstream"], h["key"], h.get("tiers") or {})
    except (TypeError, KeyError):                # not a dict, or missing a field
        return None


_GONE = object()                                 # a table entry this profile never had


def _unrecord(profile: str, was, held_was) -> None:
    """Put back what a take wrote before it when the take did not end up consuming
    its own handoff: a handoff still standing — or standing again — behind a
    switched endpoint is issue #900 read backwards. No other take of this profile
    runs beside it (_TAKE), so restoring these two entries restores the record."""
    for table, prev in ((SWITCHED, was), (HELD, held_was)):
        if prev is _GONE:
            table.pop(profile, None)
        else:
            table[profile] = prev


class _LastLook:
    """The take's rename destination, recording the handoff as the syscall
    resolves its arguments. os.rename converts both paths before it makes the
    system call: this object's __fspath__ runs after every moment a writer could
    have replaced the handoff and before the kernel moves it, so the record
    covers the publication the rename actually takes, not only the one the take
    read first (#900 review). It records nothing it cannot route and nothing
    another edge's: the checks at the taken name settle those, as ever. The look
    is idempotent — a take no writer raced re-records what it already read."""

    def __init__(self, path: Path, hand: Path, profile: str) -> None:
        self.path = path
        self.hand = hand
        self.profile = profile

    def __fspath__(self) -> str:
        try:
            h = json.loads(self.hand.read_text())
        except (OSError, ValueError):
            return str(self.path)               # gone or garbage: the taken-name checks decide
        entry = _endpoint(h)
        if entry is not None and _mine(h):
            SWITCHED[self.profile] = entry
            HELD[self.profile] = h.get("id")
        return str(self.path)


def _switched(profile: str) -> tuple[str, str, dict] | None:
    """A profile's endpoint: a new handoff file replaces the one in memory (it was
    brought up again). Only a handoff for this proxy's own edge is taken: another
    edge's proxy is the one --switch waits for.

    The take is a rename of the handoff to taken.<this pid>.<OWN_TAG>.<profile>.json,
    so what this proxy reads, holds and removes is the one publication it took
    (issue #595 review). OWN_TAG is this proxy's own start, so a sweep can tell it,
    killed mid-take, from a later process given its pid.

    The record of the handoff it read lands before the rename makes that handoff
    gone, so "gone" never runs ahead of the record (issue #900, CI run 45562448374).
    The rename moves whatever stands on the name by the time the kernel runs, so
    the take looks once more where the syscall resolves its destination
    (_LastLook): after any writer's replace and before the move. That last look
    records the publication being taken, and the moved file decides anyway: its
    endpoint is recorded before held.<pid>.json names it and before this returns,
    which is the "taken" --switch waits for. A rename that fails, or moves another
    edge's publication (put back) or something that routes nothing, restores the
    record as it was before this take.

    A handoff that routes nothing (does not parse, or lacks upstream or key) is
    moved aside and removed with no record, whichever edge it claims: its key must
    not linger to be re-polled every half second. A held.<pid>.json that cannot be
    written never cuts the take short (#900 review)."""
    if not INFERENCE:
        return SWITCHED.get(profile)
    hand = Path(INFERENCE).parent / f"switch-{profile}.json"
    taken = hand.with_name(f"taken.{os.getpid()}.{OWN_TAG}.{profile}.json")
    with _TAKE:                  # one take at a time, so held.<pid>.json never loses one
        try:
            h = json.loads(hand.read_text())
        except OSError:
            return SWITCHED.get(profile)
        except ValueError:
            h = None                            # garbage: removed below, never recorded
        entry = _endpoint(h)
        if entry is not None and not _mine(h):
            return SWITCHED.get(profile)        # another edge's: never touched, not even moved
        was = SWITCHED.get(profile, _GONE)
        held_was = HELD.get(profile, _GONE)
        if entry is not None:
            SWITCHED[profile] = entry           # recorded while the handoff still stands
            HELD[profile] = h.get("id")
        try:
            os.rename(hand, _LastLook(taken, hand, profile))   # the last look, inside the
        except OSError:                                        # syscall's path resolution
            _unrecord(profile, was, held_was)   # nothing taken: the state as it was
            return SWITCHED.get(profile)
        try:
            try:
                moved = json.loads(taken.read_text())
            except (OSError, ValueError):
                moved = None
            me = _endpoint(moved)               # what the rename actually moved decides
            if me is None:
                _unrecord(profile, was, held_was)       # routes nothing: removed, unrecorded
            elif not _mine(moved):
                try:
                    os.link(taken, hand)        # another edge's: put back unless a newer stands
                except OSError:
                    pass
                _unrecord(profile, was, held_was)
            else:
                SWITCHED[profile] = me          # the moved one, recorded before any word names it
                HELD[profile] = moved.get("id")
                try:
                    _publish_held()
                except OSError:
                    pass                        # an unwritten word never loses the take
        finally:
            try:
                taken.unlink()                  # the key lives in memory only now
            except OSError:
                pass
    return SWITCHED.get(profile)


# What rc_flow reads; the key and endpoints never leave this module's lifetime.
S = SimpleNamespace(upstream=UPSTREAM, key=KEY, tiers=TIERS, inference=INFERENCE,
                    switched=_switched, log=_log, logs=_LOG is not None, parent=PARENT,
                    parent_started=PARENT_STARTED)

_flow = None
_stamp: tuple | None = None
_checked = 0.0


def _fresh(folder: Path):
    """A new rc_flow from `folder`, bound to a new rc_route from the same folder.
    sys.modules holds them only while they load, so nothing else sees a half load."""
    before = {n: sys.modules.get(n) for n in MODULES}
    try:
        for name in MODULES:
            spec = importlib.util.spec_from_file_location(name, folder / f"{name}.py")
            mod = importlib.util.module_from_spec(spec)
            sys.modules[name] = mod      # rc_flow's `from rc_route import` takes this copy
            spec.loader.exec_module(mod)
        return mod
    finally:
        for name, old in before.items():
            if old is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = old


def _load():
    """rc_flow as its source holds it now. A change is loaded on the next request
    (at most RECHECK late). A file that does not load (a checkout half written)
    keeps the code already running and is tried again on its next change."""
    global _flow, _stamp, _checked
    now = time.monotonic()
    if _flow is not None and now - _checked < RECHECK:
        return _flow
    _checked = now
    folder = Path(SOURCE) if SOURCE and (Path(SOURCE) / "rc_flow.py").is_file() else HERE
    try:
        stamp = (str(folder), *(os.stat(folder / f"{n}.py").st_mtime_ns for n in MODULES))
    except OSError:
        stamp = None
    if _flow is not None and (stamp is None or stamp == _stamp):
        return _flow
    try:
        mod = _fresh(folder)
    except Exception as e:
        if _flow is None:
            if folder == HERE:
                raise
            folder, stamp = HERE, None    # the start's own copy, which the launcher just wrote
            mod = _fresh(folder)
            _log(f"reload    {SOURCE} failed ({type(e).__name__}: {e}); started on the launch's copy")
            _flow, _stamp = mod, None
            return _flow
        _stamp = stamp
        _log(f"reload    {folder} failed ({type(e).__name__}: {e}); the running code stays")
        return _flow
    if _flow is not None:
        _log(f"reload    {folder}")
    _flow, _stamp = mod, stamp
    return _flow


def _take_handoffs() -> None:
    """Take every handoff within a second of its writing, so the key never waits on
    disk for a request (one session's profile may be idle for days); --switch waits
    for this and removes a handoff no proxy took."""
    folder = Path(INFERENCE).parent
    while True:
        time.sleep(0.5)
        for hand in folder.glob("switch-*.json"):
            _switched(hand.name[len("switch-"):-len(".json")])


def _watch_parent() -> None:
    while True:
        time.sleep(2)
        try:
            os.kill(PARENT, 0)
        except ProcessLookupError:
            # A signal, not ctx.master.shutdown(): this is not the event-loop thread.
            os.kill(os.getpid(), signal.SIGTERM)
            return
        except PermissionError:
            pass


def running() -> None:
    _load()                      # a module that does not load fails the start, not a request
    _publish_held()              # this proxy holds nothing yet, whatever one before it held
    if PARENT:
        threading.Thread(target=_watch_parent, daemon=True).start()
    if INFERENCE:
        threading.Thread(target=_take_handoffs, daemon=True).start()


def done() -> None:
    # ccmorph-rc runs a private per-launch copy of this addon (addon.* under its
    # state dir); remove it with the proxy. Never touch the source directory.
    if HERE.name.startswith("addon."):
        shutil.rmtree(HERE, ignore_errors=True)
    if INFERENCE and PARENT:
        _forget_held()


def _ours(path: Path) -> bool:
    """Whether the held record at `path` is this proxy's: it names this edge's start."""
    try:
        return json.loads(path.read_text()).get("edge_started") == PARENT_STARTED
    except (OSError, ValueError, AttributeError):
        return False


def _forget_held() -> None:
    """What this proxy holds goes with it -- but only its own record. held.<pid>.json
    is named by the edge's pid, which the OS may have given to a replacement edge
    whose proxy writes its own record there at any moment. So the record is moved
    aside first and checked where it lies: what is checked is what is removed, and a
    record that is not ours goes back, unless a newer one stands there already
    (issue #595 review: a read, then an unlink by name, removed the replacement's)."""
    held = _held_path()
    if not _ours(held):
        return
    aside = held.with_name(f"{held.name}.{os.getpid()}.gone")
    try:
        os.rename(held, aside)
    except OSError:
        return
    try:
        if not _ours(aside):
            try:
                os.link(aside, held)
            except OSError:
                pass
    finally:
        try:
            aside.unlink()
        except OSError:
            pass


# Destination is decided in requestheaders, the earliest hook: if request bodies
# were ever streamed, mitmproxy would open the upstream connection right after the
# headers, and a reroute in `request` would arrive after the call had already gone
# to api.anthropic.com on the account's credential. Request bodies are buffered
# (the launcher sets no stream_large_bodies) so `request` can still normalize them.
def requestheaders(flow) -> None:
    _load().requestheaders(flow, S)


async def request(flow) -> None:
    await _load().request(flow, S)


def responseheaders(flow) -> None:
    _load().responseheaders(flow, S)
