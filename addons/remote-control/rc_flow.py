"""What the Remote Control proxy does with each request: where it goes, and how its
body is changed. Loaded by rc_addon.py, which holds the key and the endpoints (`s`)
and reloads this module and rc_route when either file changes, so a routing fix
reaches the running proxy with no restart. Keep every lasting state in rc_addon.
"""
from __future__ import annotations

import asyncio
import base64
import ipaddress
import json
import subprocess
import tempfile
from pathlib import Path

import time

from rc_route import (ACCOUNT_METADATA, AGENT_HEADER, SESSION_HEADER, agent_target, claim_agent,
                      encode_json, has_pending, inference_target, is_event_upload, is_inference,
                      lost_endpoint_note, normalize_body, reroute, session_target, set_model,
                      slim_events)


def _sessions(s) -> str | None:
    """<dir>/sessions: one session's own word, written by --switch --session."""
    return str(Path(s.inference).parent / "sessions") if s.inference else None


def _agents(s) -> str | None:
    """<dir>/agents: the router's picks for single subagents (rc_route.claim_agent)."""
    return str(Path(s.inference).parent / "agents") if s.inference else None


# A new subagent's request waits here, on a port nothing answers, until `request` has
# read its body: a route decided from the body alone must still fail closed if the
# addon dies before deciding (the block_list matches api.anthropic.com only).
PARKED = ("http", "127.0.0.1", 9)


def _account_tiers(s) -> dict:
    """The account profile's {alias: model}, written beside the switch by --switch; {} when absent."""
    if not s.inference:
        return {}
    try:
        tiers = json.loads((Path(s.inference).parent / "account-tiers.json").read_text())
    except (OSError, ValueError, TypeError):
        return {}
    return tiers if isinstance(tiers, dict) else {}


# Destination is decided in requestheaders, the earliest hook: if request bodies
# were ever streamed, mitmproxy would open the upstream connection right after the
# headers, and a reroute in `request` would arrive after the call had already gone
# to api.anthropic.com on the account's credential. Request bodies are buffered
# (the launcher sets no stream_large_bodies) so `request` can still normalize them.
def requestheaders(flow, s) -> None:
    req = flow.request
    route = req.path.split("?", 1)[0]
    if not is_inference(req.pretty_host, req.path):
        flow.metadata["rc_events"] = is_event_upload(req.pretty_host, req.path)
        s.log(f"anthropic {req.method} {req.pretty_host}{route}")
        return
    agent = req.headers.get(AGENT_HEADER)
    if pick := agent_target(_agents(s), agent):
        return _route(flow, s, pick["word"], " (agent)", pick["model"])
    if agent and has_pending(_agents(s), req.headers.get(SESSION_HEADER)):
        # Its prompt, in the body, may name a pick: decided in `request`.
        flow.metadata["rc_parked"] = (req.scheme, req.host, req.port)
        req.scheme, req.host, req.port = PARKED
        return
    _route(flow, s)


def _route(flow, s, target: str | None = None, via: str = "", model: str | None = None) -> None:
    """Send an inference request where its word says: the agent's pick when given,
    else its session's own word, else the switch every host shares."""
    req = flow.request
    route = req.path.split("?", 1)[0]
    if target is None:
        own = session_target(_sessions(s), req.headers.get(SESSION_HEADER))
        target = own or inference_target(s.inference)
        via = " (session)" if own else ""
    flow.metadata["rc_model"] = model
    if target == "account":
        # A legitimate first-party call: leave it untouched, but mark it so the
        # launcher's fail-closed block_list tells it from one no addon ever saw.
        # Only its model may change: a tier the route pinned (fable -> the route's
        # model) is read back to the account's own model for that alias.
        flow.metadata[ACCOUNT_METADATA] = True
        flow.metadata["rc_account_tiers"] = _account_tiers(s)
        flow.metadata["rc_word"] = "account"      # the word, kept apart from what serves it
        flow.metadata["rc_via"] = "account"
        s.log(f"account   {req.method} {route}{via}")
        return
    flow.metadata["rc_word"] = target             # the word Stark set, before any fallback
    upstream, key, tiers = s.upstream, s.key, s.tiers
    if target != "route":
        if alt := s.switched(target):
            upstream, key, tiers = alt
        else:
            # issue #595: which repair is true depends on the state dir, so rc_route decides
            # what this line may name (a session's own word is repaired with --session).
            if s.inference and getattr(s, "logs", True):
                # getattr: a proxy started before these existed reloads this module too
                s.log(lost_endpoint_note(target, Path(s.inference).parent, getattr(s, "parent", 0),
                                         req.headers.get(SESSION_HEADER) if via == " (session)" else None,
                                         getattr(s, "parent_started", None)))
            target = "route"
    headers, (scheme, host, port) = reroute(dict(req.headers.items()), upstream, key)
    flow.metadata["rc_tiers"] = tiers
    req.headers.clear()
    for k, v in headers.items():
        req.headers[k] = v
    req.scheme, req.host, req.port = scheme, host, port
    flow.metadata["rc_route"] = True
    flow.metadata["rc_via"] = target              # what routed it: the route after a fallback
    s.log(f"route     {req.method} {route}" + ("" if target == "route" else f" via {target}") + via)


def _claim(flow, s) -> None:
    """A parked subagent request: bind the agent to the pick its prompt names, then
    route it as requestheaders would have."""
    req = flow.request
    req.scheme, req.host, req.port = flow.metadata.pop("rc_parked")
    try:
        payload = json.loads(req.content or b"null")
    except ValueError:
        payload = None
    if pick := claim_agent(_agents(s), req.headers.get(AGENT_HEADER), payload, time.time(),
                           req.headers.get(SESSION_HEADER)):
        return _route(flow, s, pick["word"], " (agent)", pick["model"])
    _route(flow, s)


def _shrink(data: str, media_type: str | None) -> tuple[str, str] | None:
    """A 1000 px JPEG of a base64 image, via macOS sips; None if it fails."""
    with tempfile.TemporaryDirectory() as tmp:
        src, dst = Path(tmp, "in"), Path(tmp, "out.jpg")
        src.write_bytes(base64.b64decode(data))
        done = subprocess.run(["sips", "-s", "format", "jpeg", "-s", "formatOptions", "70",
                               "-Z", "1000", str(src), "--out", str(dst)], capture_output=True)
        if done.returncode or not dst.exists():
            return None
        return base64.b64encode(dst.read_bytes()).decode(), "image/jpeg"


def _slim_body(content: bytes, routed: bool, tiers: dict, route_tiers: dict,
               account: dict | None = None, model: str | None = None) -> bytes | None:
    """Parse, lighten and re-encode a request body; None if nothing changed. An
    account call (`account`: its tiers) changes nothing but its model; an agent's
    pick (`model`) names the model outright."""
    try:
        payload = json.loads(content)
    except ValueError:
        return None
    if account is not None:
        changed = set_model(payload, account, route_tiers, family=False)
    elif routed:
        changed = normalize_body(payload, tiers, route_tiers)
    else:
        changed = slim_events(payload, _shrink)
    if model and isinstance(payload, dict) and payload.get("model") != model:
        payload["model"] = model
        changed = True
    return encode_json(payload) if changed else None


async def request(flow, s) -> None:
    if "rc_parked" in flow.metadata:
        await asyncio.get_running_loop().run_in_executor(None, _claim, flow, s)
    routed, events = flow.metadata.get("rc_route"), flow.metadata.get("rc_events")
    account = flow.metadata.get("rc_account_tiers")
    if not (routed or events or account or flow.metadata.get("rc_model")) or not flow.request.content:
        return
    content = flow.request.content
    # sips and multi-MB JSON would stall every other flow served by the loop:
    # parse, slim and encode on a worker thread and wait for it here.
    slim = await asyncio.get_running_loop().run_in_executor(
        None, _slim_body, content, routed, flow.metadata.get("rc_tiers", s.tiers), s.tiers, account,
        flow.metadata.get("rc_model"))
    if slim is not None:
        flow.request.content = slim
        if events:
            s.log(f"events    {len(content)} -> {len(slim)} bytes")


def _answered(flow) -> tuple[str, tuple[str, int] | None]:
    """Where the reply came from: the connection that answered, named as served-by,
    with its (host, port); None for the address when the hook cannot see the
    connection (a replay, an older flow), and the request's destination stands in."""
    addr = getattr(getattr(flow, "server_conn", None), "address", None)
    try:
        host, port = str(addr[0]), int(addr[1])
    except (TypeError, IndexError, KeyError, ValueError):
        return f"{flow.request.host}:{flow.request.port}", None
    return f"{host}:{port}", (host, port)


def _host(name: str):
    """A host as compared: an IP literal by its address (::1 in any spelling), a name
    as written, case aside."""
    try:
        return ipaddress.ip_address(name)
    except ValueError:
        return name.lower()


def _reached(host: str, port: int, answered: tuple[str, int] | None) -> bool:
    """Whether the answering connection is the endpoint the request was sent to: the
    same host and the same port. mitmproxy's Server.address names the host the
    request left for (a hostname stays a hostname; the IP it resolved to is
    Server.peername, never read here), so a named upstream is compared by its name
    and a literal (a loopback adapter, an IP upstream) by its address. An unreadable
    connection cannot prove a mismatch and leaves the destination as the evidence."""
    if answered is None:
        return True
    return int(port) == answered[1] and _host(str(host)) == _host(answered[0])


def responseheaders(flow, s) -> None:
    flow.response.stream = True          # SSE and bridge long-polls pass through live
    asked = flow.metadata.get("rc_word")
    if asked is None:                    # never an inference turn: nothing to answer for
        return
    # Anthropic stamps its organization on every response; that is the proof of which
    # upstream answered, matched against the word the turn went out under (#920:
    # requested-vs-served, per session turn — the profile Stark set must be the one
    # that replies, and a turn answered by anything else says so, never a quiet
    # success). A routed call must not carry it; an account call must.
    first_party = "anthropic-organization-id" in flow.response.headers
    account = bool(flow.metadata.get(ACCOUNT_METADATA))
    served, answered = _answered(flow)
    # The word the pick set is `asked`; `via` is what routed the turn. A profile the
    # proxy holds no endpoint for falls back to the edge's route (issue #595), and
    # the reply is then the route's, not the pick's: a miss, with the fallback's own
    # warning above it. A routed reply is the pick's only when it left for that
    # endpoint, came back from that same endpoint, and carries no Anthropic signature.
    via = flow.metadata.get("rc_via", asked)
    if account:
        extra, ok = "", first_party
        where = "ANTHROPIC" if first_party else f"{served} (no Anthropic signature)"
    else:
        ok = via == asked and not first_party and _reached(
            str(flow.request.host), flow.request.port, answered)
        extra = f" via={via}" if via != asked else ""
        where = "ANTHROPIC (not routed!)" if first_party else served
    s.log(("miss      " if not ok else "account   " if account else "route     ")
          + f"<- {flow.response.status_code} {flow.request.path.split('?', 1)[0]} "
            f"asked={asked}{extra} served-by={where}")
