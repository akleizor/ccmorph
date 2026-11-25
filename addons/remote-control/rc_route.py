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
import os
import re
from pathlib import Path
from urllib.parse import urlsplit

FIRST_PARTY_HOST = "api.anthropic.com"
INFERENCE_PREFIX = "/v1/messages"          # also covers /v1/messages/count_tokens
AUTH_HEADERS = ("authorization", "x-api-key", "cookie", "proxy-authorization")
# The addon marks every inference request it handed to Anthropic on the session's own
# claude.ai login. The launcher's fail-closed block_list matches api.anthropic.com by
# destination, which is also where an untouched account request goes, so the guard
# exempts a flow carrying this mark: only the addon can set it, and without an addon
# no flow has it.
ACCOUNT_METADATA = "rc_account"


def state_root() -> Path:
    """ccmorph's state root, resolved as the core resolves it: CCMORPH_STATE_DIR,
    else XDG_CONFIG_HOME/ccmorph, else ~/.config/ccmorph. ccmorph-rc and
    ccmorph-rc-hosts both read it here, so the ladder is spelled once (#636)."""
    return Path(os.environ.get("CCMORPH_STATE_DIR") or
                Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "ccmorph")


class BridgeDirs:
    """The bridge's state under a state root: the one spelling ccmorph-rc and
    ccmorph-rc-hosts both read, so the supervisor and the launcher cannot name two
    places for one file (#636)."""

    def __init__(self, root: Path) -> None:
        self.state = root / "profiles" / ".rc-bridge"
        self.logs = self.state / "logs"
        self.edges = self.state / "edges"
        self.inference = self.state / "inference"
        self.busy = self.state / "busy.json"


def project_slug(folder: str) -> str:
    """Claude Code's projects/<slug> name for a folder: the resolved path with
    every character outside [A-Za-z0-9] made '-'. Resolved here, so a folder
    named through a symlink finds the state Claude keeps under its real path;
    ccmorph-rc and ccmorph-rc-hosts both use this one rule (#636)."""
    return re.sub(r"[^A-Za-z0-9]", "-", str(Path(folder).resolve()))


SWITCH_WORD = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*")


def inference_target(switch: str | None) -> str:
    """Where inference goes for this request: "account" sends it to Anthropic on the
    session's own claude.ai login, untouched; "route" (also a missing, unreadable or
    malformed file) to the edge's own route; any other word is a ccmorph profile that
    `ccmorph-rc <profile> --switch` brought up beside it. Read on every inference
    request, so a switch takes effect on the next turn with no restart."""
    try:
        word = open(switch).read().strip() if switch else ""
    except OSError:
        return "route"
    return word if SWITCH_WORD.fullmatch(word) else "route"


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
    pinned = tiers.get(m.group(1) if m else model)
    # `[1m]` is Claude Code's own context marker, stripped before a request is sent;
    # a profile's tier carries it (claude-opus-5-5[1m]) and the backend 404s on it.
    return pinned[:-4] if pinned and pinned.endswith("[1m]") else pinned


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


def _slim_result(node: object, shrink) -> tuple[object, bool]:
    """A tool result's content with long text cut and big screenshots shrunk; (content, changed)."""
    if isinstance(node, str):
        cut = _cut(node)
        return cut, cut != node
    if isinstance(node, list):
        slimmed = [_slim_result(item, shrink) for item in node]
        return [item for item, _ in slimmed], any(changed for _, changed in slimmed)
    if not isinstance(node, dict):
        return node, False
    src = node.get("source")
    if (node.get("type") == "image" and isinstance(src, dict) and src.get("type") == "base64"
            and len(src.get("data") or "") > IMAGE_MIN):
        small = shrink(src["data"], src.get("media_type"))
        if small and len(small[0]) < len(src["data"]):
            return {**node, "source": {**src, "data": small[0], "media_type": small[1]}}, True
        return node, False
    if node.get("type") == "text":
        updated = {**node, "text": _cut(node.get("text"))}
        return updated, updated != node
    return node, False


_MISSING = object()


def _slim(payload: dict, shrink) -> bool:
    """Lighten one event payload in place; True if it changed."""
    changed = False
    kind = payload.get("type")
    listing = (payload.get("response") or {}).get("response")
    if kind == "control_response" and isinstance(listing, dict):
        for key in ("commands", "agents"):
            for item in listing.get(key) or []:
                if isinstance(item, dict) and item.pop("description", _MISSING) is not _MISSING:
                    changed = True
    message = payload.get("message")
    content = message.get("content") if isinstance(message, dict) else None
    if kind == "assistant":
        if payload.pop("wire_tool_inputs", _MISSING) is not _MISSING:
            changed = True
        for block in content if isinstance(content, list) else ():
            if isinstance(block, dict) and block.get("signature"):
                block["signature"] = ""
                changed = True
    if kind == "user":
        for block in content if isinstance(content, list) else ():
            if isinstance(block, dict) and block.get("type") == "tool_result":
                # A tool_result written without `content` has always gained it (as
                # null); a present one is rewritten only when slimming changed it.
                missing = "content" not in block
                slimmed, slim_changed = _slim_result(block.get("content"), shrink)
                if missing or slim_changed:
                    block["content"] = slimmed
                    changed = True
        result = payload.get("tool_use_result")
        if isinstance(result, dict):
            for key in DUPLICATE_OUTPUT:
                if result.pop(key, _MISSING) is not _MISSING:
                    changed = True
        elif isinstance(result, str):
            cut = _cut(result)
            if cut != result:
                payload["tool_use_result"] = cut
                changed = True
    return changed


def encode_json(obj: object, ensure_ascii: bool = False) -> bytes:
    """Compact UTF-8 JSON for a whole body. json.dumps grows one buffer by realloc;
    on macOS each multi-MB body leaves the freed steps cached in the long-lived
    proxy. Joining the encoder's chunks allocates the result once."""
    encoder = json.JSONEncoder(separators=(",", ":"), ensure_ascii=ensure_ascii)
    return "".join(encoder.iterencode(obj)).encode()


def slim_events(body: object, shrink) -> bool:
    """Lighten a worker/events upload in place; True if anything changed.

    `shrink(base64, media_type)` returns a smaller (base64, media_type) or None.
    The change flag comes from the walk itself: encoding the multi-MB body a second
    time just to diff it against the first doubled the cost of every session open.
    """
    events = body.get("events") if isinstance(body, dict) else None
    if not isinstance(events, list):
        return False
    changed = False
    for event in events:
        if isinstance(event, dict) and isinstance(event.get("payload"), dict):
            changed |= _slim(event["payload"], shrink)
    return changed


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
