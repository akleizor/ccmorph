"""Routing decision for the Remote Control bridge. No mitmproxy import, so it is
testable with a plain interpreter.

The Remote Control session authenticates to Anthropic with the account's claude.ai
OAuth token. Only model inference is moved to the route profile's endpoint; every
other request (bridge, OAuth, telemetry, MCP registry) passes through unchanged.

A rerouted request must never carry the claude.ai credential: every auth-bearing
header is dropped before the route key is set, and OAuth-only beta flags go with it.
"""
from __future__ import annotations

import json
import re
from urllib.parse import urlsplit

FIRST_PARTY_HOST = "api.anthropic.com"
INFERENCE_PREFIX = "/v1/messages"          # also covers /v1/messages/count_tokens
AUTH_HEADERS = ("authorization", "x-api-key", "cookie", "proxy-authorization")


def is_inference(host: str, path: str) -> bool:
    if host.lower().rstrip(".") != FIRST_PARTY_HOST:
        return False
    route = path.split("?", 1)[0]
    return route == INFERENCE_PREFIX or route.startswith(INFERENCE_PREFIX + "/")


# The session believes it talks to api.anthropic.com, so it sends request fields only
# first-party accepts. Each entry maps one such value to its nearest portable form.
# thinking.display="updates" (streamed thinking for Remote Control viewers) is refused
# by gateways with "display: Input should be 'summarized', 'omitted'". "omitted", not
# "summarized": a gateway that buffers thinking releases nothing until the summary is
# written, and on the measured gateway that summary step alone added ~1.5-2.5s of dead
# air before the first token, for text the phone only shows collapsed anyway.
FIRST_PARTY_ONLY_DISPLAY = {"updates": "omitted"}


# A first-party login makes Claude Code put how far into the session this request is
# inside the billing-header text block at system[0]: cch, cc_prev_req, cc_prompt_id,
# cc_turn_origin, cc_prompt_index, cc_turn_index. They change every turn, and a route
# caches on the request prefix, so they rewrote the whole prompt (#345: the tools block
# read back as cache_read=31028 while cache_creation=353451; upstream said
# "prefix_binding_mismatch" from messages.1 on). A gateway launch never sends them.
BILLING_HEADER = "x-anthropic-billing-header:"
# One field is a whitespace-separated token ending in ";", so the match never crosses
# whitespace: a field written without its ";" cannot swallow the next one, and it takes
# the separator a field was written with, whatever that is (the client writes a space).
FIRST_PARTY_TURN_FIELDS = re.compile(
    r"\s*\b(?:cch|cc_prev_req|cc_prompt_id|cc_turn_origin|cc_prompt_index|cc_turn_index)=[^;\s]*;?")


def _drop_turn_fields(system: object) -> bool:
    changed = False
    for block in system if isinstance(system, list) else ():
        text = block.get("text") if isinstance(block, dict) else None
        if isinstance(text, str) and text.startswith(BILLING_HEADER):
            stable = FIRST_PARTY_TURN_FIELDS.sub("", text)
            if stable != text:
                block["text"] = stable
                changed = True
    return changed


# cache_control.scope is sent only to first party on a claude.ai login; gateways
# refuse it ("system.1.cache_control.scope: Extra inputs are not permitted"). The
# breakpoint itself stays, so the route still caches at the same place.
CACHE_SCOPED = ("system", "messages", "tools")


def _drop_cache_scope(node: object) -> bool:
    changed = False
    if isinstance(node, list):
        for item in node:
            changed |= _drop_cache_scope(item)
    elif isinstance(node, dict):
        cc = node.get("cache_control")
        if isinstance(cc, dict) and "scope" in cc:
            del cc["scope"]
            changed = True
        content = node.get("content")
        if isinstance(content, list):
            changed |= _drop_cache_scope(content)
    return changed


# A pinned tier (`tier.fable=gpt-6-astra` in rc-hosts) reaches the session as
# ANTHROPIC_DEFAULT_FABLE_MODEL, which only covers the bare alias. The phone's model
# picker sends the full id (claude-fable-5-1), so the proxy pins the whole family.
CLAUDE_FAMILY = re.compile(r"^claude-(fable|opus|sonnet|haiku)-\d")


def pinned_model(model: object, tiers: dict[str, str]) -> str | None:
    """The model a pinned tier sends `model` to, else None."""
    if not isinstance(model, str):
        return None
    m = CLAUDE_FAMILY.match(model)
    return tiers.get(m.group(1) if m else model)


def normalize_body(payload: object, tiers: dict[str, str] | None = None) -> bool:
    """Rewrite first-party-only fields in place; True if anything changed."""
    if not isinstance(payload, dict):
        return False
    changed = False
    if (pinned := pinned_model(payload.get("model"), tiers or {})) and pinned != payload["model"]:
        payload["model"] = pinned
        changed = True
    thinking = payload.get("thinking")
    if isinstance(thinking, dict) and thinking.get("display") in FIRST_PARTY_ONLY_DISPLAY:
        thinking["display"] = FIRST_PARTY_ONLY_DISPLAY[thinking["display"]]
        changed = True
    for field in CACHE_SCOPED:
        changed |= _drop_cache_scope(payload.get(field))
    changed |= _drop_turn_fields(payload.get("system"))
    return changed


# The phone opens a session by downloading every event the host ever uploaded, so
# event weight is open time (one session: 139 MB, 80 s). The app only renders this
# copy; the model reads its local transcript, which this never touches. Measured
# 2026-10-07, what can go without changing what the phone shows:
#   - each app open re-uploads the full command and agent list (~55 KB, 927 times);
#     names stay, descriptions go
#   - thinking signatures: only the model API checks them, from the local copy
#   - tool_use_result's copies of the output (stdout, file text, the whole file
#     before an edit), already in the message; its diffs stay
#   - wire_tool_inputs: a second copy of the tool inputs already in the message
#   - tool output past TEXT_MAX and full-size screenshots: unreadable on a phone
EVENTS_PATH = re.compile(r"^/v1/code/sessions/[A-Za-z0-9_]+/worker/events$")
TEXT_MAX = 4000              # chars of tool output kept per block
IMAGE_MIN = 100_000          # base64 chars; smaller images are left as they are
DUPLICATE_OUTPUT = ("originalFile", "stdout", "stderr", "file", "content")
CUT = "\n… [trimmed for Remote Control; full output is on the Mac]"


def is_event_upload(host: str, path: str) -> bool:
    return (host.lower().rstrip(".") == FIRST_PARTY_HOST
            and bool(EVENTS_PATH.match(path.split("?", 1)[0])))


def _cut(text: object) -> object:
    return text[:TEXT_MAX] + CUT if isinstance(text, str) and len(text) > TEXT_MAX + len(CUT) else text


def _slim_result(node: object, shrink) -> object:
    """A tool result's content with long text cut and big screenshots shrunk."""
    if isinstance(node, str):
        return _cut(node)
    if isinstance(node, list):
        return [_slim_result(item, shrink) for item in node]
    if not isinstance(node, dict):
        return node
    src = node.get("source")
    if (node.get("type") == "image" and isinstance(src, dict) and src.get("type") == "base64"
            and len(src.get("data") or "") > IMAGE_MIN):
        small = shrink(src["data"], src.get("media_type"))
        if small and len(small[0]) < len(src["data"]):
            return {**node, "source": {**src, "data": small[0], "media_type": small[1]}}
        return node
    if node.get("type") == "text":
        return {**node, "text": _cut(node.get("text"))}
    return node


def _slim(payload: dict, shrink) -> None:
    kind = payload.get("type")
    listing = (payload.get("response") or {}).get("response")
    if kind == "control_response" and isinstance(listing, dict):
        for key in ("commands", "agents"):
            for item in listing.get(key) or []:
                if isinstance(item, dict):
                    item.pop("description", None)
    message = payload.get("message")
    content = message.get("content") if isinstance(message, dict) else None
    if kind == "assistant":
        payload.pop("wire_tool_inputs", None)
        for block in content if isinstance(content, list) else ():
            if isinstance(block, dict) and block.get("signature"):
                block["signature"] = ""
    if kind == "user":
        for block in content if isinstance(content, list) else ():
            if isinstance(block, dict) and block.get("type") == "tool_result":
                block["content"] = _slim_result(block.get("content"), shrink)
        result = payload.get("tool_use_result")
        if isinstance(result, dict):
            for key in DUPLICATE_OUTPUT:
                result.pop(key, None)
        elif isinstance(result, str):
            payload["tool_use_result"] = _cut(result)


def slim_events(body: object, shrink) -> bool:
    """Lighten a worker/events upload in place; True if anything changed.

    `shrink(base64, media_type)` returns a smaller (base64, media_type) or None.
    """
    events = body.get("events") if isinstance(body, dict) else None
    if not isinstance(events, list):
        return False
    before = json.dumps(events, sort_keys=True)
    for event in events:
        if isinstance(event, dict) and isinstance(event.get("payload"), dict):
            _slim(event["payload"], shrink)
    return json.dumps(events, sort_keys=True) != before


def reroute(headers: dict[str, str], upstream: str, key: str) -> tuple[dict[str, str], tuple[str, str, int]]:
    """Rewrite headers for the route endpoint; return (headers, (scheme, host, port)).

    `headers` keys are matched case-insensitively and returned lower-cased.
    """
    target = urlsplit(upstream)
    if target.scheme not in ("http", "https") or not target.hostname:
        raise ValueError(f"route upstream is not an http(s) URL: {upstream!r}")
    port = target.port or (443 if target.scheme == "https" else 80)

    out = {k.lower(): v for k, v in headers.items() if k.lower() not in AUTH_HEADERS}
    betas = [b.strip() for b in out.pop("anthropic-beta", "").split(",")
             if b.strip() and not b.strip().startswith("oauth-")]
    if betas:
        out["anthropic-beta"] = ",".join(betas)
    out["host"] = target.netloc
    out["x-api-key"] = key
    out["authorization"] = f"Bearer {key}"
    return out, (target.scheme, target.hostname, port)
