"""The rules ccmorph-rc and ccmorph-rc-hosts share about the shared edges: how an edge
is named, whether one can serve, which one a --switch hands a profile to. The request
routing itself is in the Go edge proxy (agentproxy/edge*.go; `ccmorph-core edge-proxy`).
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import socket
import sys
from pathlib import Path

SWITCH_WORD = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*")


def safe_part(value: str) -> str:
    """Any token as one file-name part: every run of characters a path segment
    cannot hold becomes a single dash. Folder log names have always cleaned this
    way; the edge's names did not, so `route=gw/eu` opened its log inside a
    directory nothing creates and its FileNotFoundError escaped the supervisor's
    tick, which launchd restarted into the same crash (#846)."""
    return re.sub(r"[^A-Za-z0-9]+", "-", value).strip("-")


# The shape of a valid profile name, and so of the only route `ccmorph _route_env`
# accepts (cli/main.go routeEnv -> profile.ValidName; profile/profile.go nameRE,
# MaxNameLen). It is a copy of the owner's rule only in the sense a reader repeats
# a fact: the launcher still decides, this only says what an existing edge row may
# be named.
PROFILE_NAME = re.compile(r"[A-Za-z0-9_]{1,200}")


def edge_name(route: str) -> str:
    """The route as the file-name part of its edge's log and state file -- the
    one rule both scripts name edges by.

    A valid profile name keeps it WHOLE: it is the only route whose edge can
    ever have run (the launcher refuses every other word before a record is
    written), and `gw_eu.json` is the name a live edge wrote before this rule
    existed -- renaming rows already on disk leaves a restarted supervisor
    blind to its own edge and starting a second one against the same fixed port
    (REVIEW.md, the gate's round-2 refusal).

    EVERY other route is cleaned by `safe_part` and carries a sha256 digest of
    the whole route: cleaning alone merges `gw/eu`, `gw-eu` and `gw.eu` onto one
    log and one record, and a checksum an operator can steer onto another name
    (CRC32, the first attempt) builds that collision on purpose.

    Every emitted name is ASCII and short, so two names are one FILE on the
    volumes ccmorph runs on -- case-insensitive, normalisation-insensitive --
    exactly when they are equal after `.lower()` (the supervisor compares that,
    round 4), and `edge-<name>.log` stays inside NAME_MAX. Whole names never
    contain `-` and cleaned names always do, so a route typed as another route's
    cleaned name gets a digest of its own and never that name (round 3).

    Lives in this pure module, beside SWITCH_WORD, so neither script must load
    the other -- or the launcher -- to name a file during a reload (the #845
    class: a partial install must not crash one)."""
    if PROFILE_NAME.fullmatch(route):
        return route
    part = safe_part(route)[:64].strip("-") or "route"
    return f"{part}-{hashlib.sha256(route.encode('utf-8', 'surrogatepass')).hexdigest()[:32]}"


def proc_started(pid: int) -> str | None:
    """When the process at `pid` started, exactly enough that no later process the
    OS gives the same number can share it (issue #595); None when no process runs
    there or this OS has no exact start. Linux: the boot id and the start tick
    (/proc/<pid>/stat field 22). macOS: the kernel's p_starttime to the microsecond
    (sysctl kern.proc.pid). Never `ps`: it prints whole seconds."""
    try:
        stat = Path(f"/proc/{pid}/stat").read_bytes()
        ticks = stat[stat.rindex(b")") + 1:].split()[19].decode()   # field 22: starttime
        boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
        return f"{boot}:{ticks}"
    except (OSError, ValueError, IndexError, UnicodeDecodeError):
        pass
    if sys.platform != "darwin" or pid <= 0:
        return None
    import ctypes
    import struct
    try:
        sysctl = ctypes.CDLL(None, use_errno=True).sysctl
    except (OSError, AttributeError):
        return None
    sysctl.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_uint, ctypes.c_void_p,
                       ctypes.POINTER(ctypes.c_size_t), ctypes.c_void_p, ctypes.c_size_t]
    sysctl.restype = ctypes.c_int
    mib = (ctypes.c_int * 4)(1, 14, 1, pid)         # CTL_KERN, KERN_PROC, KERN_PROC_PID
    buf = ctypes.create_string_buffer(1024)         # struct kinfo_proc: 648 bytes on 64-bit macOS
    size = ctypes.c_size_t(len(buf))
    if sysctl(mib, 4, buf, ctypes.byref(size), None, 0) != 0 or size.value < 16:
        return None                                  # size 0: no process holds the pid
    sec, usec = struct.unpack_from("=qi", buf.raw, 0)   # kp_proc.p_starttime (struct timeval)
    return f"darwin:{sec}.{usec:06d}"


def start_tag(started: str | None) -> str:
    """A process start (proc_started) as one file-name part: the first 16 hex digits
    of its sha256, or "unknown" when the start could not be read. A start itself
    holds ':' and '.', and a name read by splitting on '.' must keep its pid where
    it was (issue #595: taken.<proxy pid>.<start tag>.<profile>.json)."""
    return hashlib.sha256(started.encode()).hexdigest()[:16] if started else "unknown"


def edge_serves(record: Path, route: str | None = None) -> dict | None:
    """The shared edge `record` describes if it can serve right now: its process is
    alive, its proxy answers on loopback and its CA file is still there. The one
    rule: ccmorph-rc's live_edge (the count --switch refuses on, and rc-hosts'
    edge_ready) and the fallback note both call it, so the note can never count an
    edge --switch would refuse (issue #595: the note skipped the CA).

    With `route`, the record must also be that route's (#846): one naming another
    route is a case twin's on a case-insensitive volume, and one naming none (from
    before the field existed) counts only when its directory lists it spelled
    exactly as `record.name` (the GH-259 rule)."""
    try:
        edge = json.loads(record.read_text())
        if route is not None:
            if edge.get("route", route) != route:
                return None
            if "route" not in edge and record.name not in os.listdir(record.parent):
                return None
        os.kill(int(edge["pid"]), 0)
        with socket.create_connection(("127.0.0.1", int(edge["port"])), timeout=1):
            pass
        return edge if Path(edge["ca"]).exists() else None
    except (OSError, ValueError, KeyError, TypeError, AttributeError):
        return None


def route_edge(edges: Path, route: str) -> dict | None:
    """`route`'s shared edge under `edges` if it can serve now: its record at
    <edge_name(route)>.json, read by edge_serves with the #846 ownership check.
    ccmorph-rc's live_edge is this, and so are the note's and --switch's counts."""
    return edge_serves(edges / f"{edge_name(route)}.json", route)


def switch_edge(edges: Path, serves=None) -> tuple[int, dict | None]:
    """The shared edge `ccmorph-rc <profile> --switch` hands a profile to: (how many
    edges under `edges` serve, that edge's record with its "started" added -- or None
    unless exactly one serves and its start can be read). --switch, the supervisor
    and the fallback note all ask this, so the note names the command only where it
    lands (issue #595). `serves` stands in for route_edge (ccmorph-rc passes its
    live_edge, the supervisor its edge_ready)."""
    serves = serves or (lambda f: route_edge(edges, f.stem))
    live = [e for f in (sorted(edges.glob("*.json")) if edges.is_dir() else ()) if (e := serves(f))]
    if len(live) != 1:
        return len(live), None
    started = proc_started(int(live[0]["pid"]))
    return 1, (dict(live[0], started=started) if started else None)


# A session id names a file under sessions/: only a plain id, no path.
SESSION_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}")
