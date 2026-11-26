"""What the Remote Control proxy does with each request: where it goes, and how its
body is changed. Loaded by rc_addon.py, which holds the key and the endpoints (`s`)
and reloads this module and rc_route when either file changes, so a routing fix
reaches the running proxy with no restart. Keep every lasting state in rc_addon.
"""
from __future__ import annotations

import asyncio
import base64
import json
import subprocess
import tempfile
from pathlib import Path

from rc_route import (ACCOUNT_METADATA, SESSION_HEADER, encode_json, inference_target,
                      is_event_upload, is_inference, lost_endpoint_note, normalize_body, reroute,
                      session_target, set_model, slim_events)


def _sessions(s) -> str | None:
    """<dir>/sessions: one session's own word, written by --switch --session."""
    return str(Path(s.inference).parent / "sessions") if s.inference else None


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
    own = session_target(_sessions(s), req.headers.get(SESSION_HEADER))
    target = own or inference_target(s.inference)
    via = " (session)" if own else ""
    if target == "account":
        # A legitimate first-party call: leave it untouched, but mark it so the
        # launcher's fail-closed block_list tells it from one no addon ever saw.
        # Only its model may change: a tier the route pinned (fable -> the route's
        # model) is read back to the account's own model for that alias.
        flow.metadata[ACCOUNT_METADATA] = True
        flow.metadata["rc_account_tiers"] = _account_tiers(s)
        flow.metadata["rc_word"] = "account"
        s.log(f"account   {req.method} {route}{via}")
        return
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
                                         req.headers.get(SESSION_HEADER) if own else None,
                                         getattr(s, "parent_started", None)))
            target = "route"
    headers, (scheme, host, port) = reroute(dict(req.headers.items()), upstream, key)
    flow.metadata["rc_tiers"] = tiers
    req.headers.clear()
    for k, v in headers.items():
        req.headers[k] = v
    req.scheme, req.host, req.port = scheme, host, port
    flow.metadata["rc_route"] = True
    flow.metadata["rc_word"] = target
    s.log(f"route     {req.method} {route}" + ("" if target == "route" else f" via {target}") + via)


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
               account: dict | None = None) -> bytes | None:
    """Parse, lighten and re-encode a request body; None if nothing changed. An
    account call (`account`: its tiers) changes nothing but its model."""
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
    return encode_json(payload) if changed else None


async def request(flow, s) -> None:
    routed, events = flow.metadata.get("rc_route"), flow.metadata.get("rc_events")
    account = flow.metadata.get("rc_account_tiers")
    if not (routed or events or account) or not flow.request.content:
        return
    content = flow.request.content
    # sips and multi-MB JSON would stall every other flow served by the loop:
    # parse, slim and encode on a worker thread and wait for it here.
    slim = await asyncio.get_running_loop().run_in_executor(
        None, _slim_body, content, routed, flow.metadata.get("rc_tiers", s.tiers), s.tiers, account)
    if slim is not None:
        flow.request.content = slim
        if events:
            s.log(f"events    {len(content)} -> {len(slim)} bytes")


def responseheaders(flow, s) -> None:
    flow.response.stream = True          # SSE and bridge long-polls pass through live
    word = flow.metadata.get("rc_word")
    if word is None:                     # never an inference turn: nothing to answer for
        return
    # Anthropic stamps its organization on every response; that is the proof of which
    # upstream answered, matched against the word the turn went out under (#920:
    # requested-vs-served, per session turn — the profile Stark set must be the one
    # that replies, and a turn answered by anything else says so, never a quiet
    # success). A routed call must not carry it; an account call must.
    first_party = "anthropic-organization-id" in flow.response.headers
    account = bool(flow.metadata.get(ACCOUNT_METADATA))
    if account:
        ok, served = first_party, ("ANTHROPIC" if first_party else
                                   f"{flow.request.host}:{flow.request.port} (no Anthropic signature)")
    else:
        ok = not first_party
        served = "ANTHROPIC (not routed!)" if first_party else f"{flow.request.host}:{flow.request.port}"
    s.log(("miss      " if not ok else "account   " if account else "route     ")
          + f"<- {flow.response.status_code} {flow.request.path.split('?', 1)[0]} "
            f"asked={word} served-by={served}")
