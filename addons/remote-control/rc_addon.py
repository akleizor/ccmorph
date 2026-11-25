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
  RC_LOG          optional path; one line per request, method and path only
  RC_TIERS        optional JSON {alias: model}: tiers the config pinned (rc-hosts)
  RC_SOURCE       optional folder rc_flow.py and rc_route.py are reloaded from (the install's)
  RC_INFERENCE    optional path; "account", "route" or a profile name, read on every turn.
                  A profile's endpoint arrives as <dir>/switch-<profile>.json (0600), taken
                  within a second and deleted on read. <dir>/sessions/<session id> holds one
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
TIERS = json.loads(os.environ.get("RC_TIERS") or "{}")
INFERENCE = os.environ.get("RC_INFERENCE")
SOURCE = os.environ.get("RC_SOURCE")
SWITCHED: dict[str, tuple[str, str, dict]] = {}   # profile -> (upstream, key, tiers), handed over by --switch
MODULES = ("rc_route", "rc_flow")                  # in import order: rc_flow imports rc_route
RECHECK = 1.0                                      # seconds between looks at the source files

_LOG = open(LOG, "a", buffering=1) if LOG else None   # line-buffered: one write per request


def _log(line: str) -> None:
    if _LOG:
        _LOG.write(f"{time.strftime('%H:%M:%S')} {line}\n")


def _switched(profile: str) -> tuple[str, str, dict] | None:
    """A profile's endpoint: a new handoff file replaces the one in memory (it was brought up again)."""
    if not INFERENCE:
        return SWITCHED.get(profile)
    hand = Path(INFERENCE).parent / f"switch-{profile}.json"
    try:
        h = json.loads(hand.read_text())
        hand.unlink()
        SWITCHED[profile] = (h["upstream"], h["key"], h.get("tiers") or {})
    except (OSError, ValueError, KeyError):
        pass
    return SWITCHED.get(profile)


# What rc_flow reads; the key and endpoints never leave this module's lifetime.
S = SimpleNamespace(upstream=UPSTREAM, key=KEY, tiers=TIERS, inference=INFERENCE,
                    switched=_switched, log=_log)

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
    if PARENT:
        threading.Thread(target=_watch_parent, daemon=True).start()
    if INFERENCE:
        threading.Thread(target=_take_handoffs, daemon=True).start()


def done() -> None:
    # ccmorph-rc runs a private per-launch copy of this addon (addon.* under its
    # state dir); remove it with the proxy. Never touch the source directory.
    if HERE.name.startswith("addon."):
        shutil.rmtree(HERE, ignore_errors=True)


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
