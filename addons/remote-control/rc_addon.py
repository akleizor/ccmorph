"""mitmproxy addon: thin shell around rc_route. Loaded only by ccmorph-rc.

Configuration arrives through the environment, never argv. The route key itself
arrives as a 0600 file that is deleted on read, so it is in neither the process
table nor any environment:
  RC_UPSTREAM     route endpoint (the adapter's loopback URL or the profile's base_url)
  RC_KEY_FILE     route credential file, removed once read
  RC_PARENT_PID   pid to follow; the proxy shuts down when it disappears
  RC_LOG          optional path; one line per request, method and path only
  RC_TIERS        optional JSON {alias: model}: tiers the config pinned (rc-hosts)
"""
import json
import os
import base64
import shutil
import signal
import subprocess
import tempfile
import sys
import threading
import time
from pathlib import Path

from mitmproxy import http

sys.path.insert(0, str(Path(__file__).resolve().parent))
from rc_route import encode_json, is_event_upload, is_inference, normalize_body, reroute, slim_events  # noqa: E402

UPSTREAM = os.environ["RC_UPSTREAM"]
_key_file = os.environ.pop("RC_KEY_FILE")
KEY = Path(_key_file).read_text().strip()
os.unlink(_key_file)
LOG = os.environ.get("RC_LOG")
PARENT = int(os.environ.get("RC_PARENT_PID") or 0)
TIERS = json.loads(os.environ.get("RC_TIERS") or "{}")


_LOG = open(LOG, "a", buffering=1) if LOG else None   # line-buffered: one write per request


def _log(line: str) -> None:
    if _LOG:
        _LOG.write(f"{time.strftime('%H:%M:%S')} {line}\n")


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
    if PARENT:
        threading.Thread(target=_watch_parent, daemon=True).start()


def done() -> None:
    # ccmorph-rc runs a private per-launch copy of this addon (addon.* under its
    # state dir); remove it with the proxy. Never touch the source directory.
    here = Path(__file__).resolve().parent
    if here.name.startswith("addon."):
        shutil.rmtree(here, ignore_errors=True)


# Destination is decided in requestheaders, the earliest hook: if request bodies
# were ever streamed, mitmproxy would open the upstream connection right after the
# headers, and a reroute in `request` would arrive after the call had already gone
# to api.anthropic.com on the account's credential. Request bodies are buffered
# (the launcher sets no stream_large_bodies) so `request` can still normalize them.
def requestheaders(flow: http.HTTPFlow) -> None:
    req = flow.request
    route = req.path.split("?", 1)[0]
    if not is_inference(req.pretty_host, req.path):
        flow.metadata["rc_events"] = is_event_upload(req.pretty_host, req.path)
        _log(f"anthropic {req.method} {req.pretty_host}{route}")
        return
    headers, (scheme, host, port) = reroute(dict(req.headers.items()), UPSTREAM, KEY)
    req.headers.clear()
    for k, v in headers.items():
        req.headers[k] = v
    req.scheme, req.host, req.port = scheme, host, port
    flow.metadata["rc_route"] = True
    _log(f"route     {req.method} {route}")


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


def request(flow: http.HTTPFlow) -> None:
    routed, events = flow.metadata.get("rc_route"), flow.metadata.get("rc_events")
    if not (routed or events) or not flow.request.content:
        return
    try:
        payload = json.loads(flow.request.content)
    except ValueError:
        return
    if normalize_body(payload, TIERS) if routed else slim_events(payload, _shrink):
        before = len(flow.request.content)
        flow.request.content = encode_json(payload)
        if events:
            _log(f"events    {before} -> {len(flow.request.content)} bytes")


def responseheaders(flow: http.HTTPFlow) -> None:
    flow.response.stream = True          # SSE and bridge long-polls pass through live
    if not flow.metadata.get("rc_route"):
        return
    # Anthropic stamps its organization on every response; a routed call must not
    # carry it. This is the proof the call left first-party, not an assumption.
    first_party = "anthropic-organization-id" in flow.response.headers
    served = "ANTHROPIC (not routed!)" if first_party else f"{flow.request.host}:{flow.request.port}"
    _log(f"route     <- {flow.response.status_code} {flow.request.path.split('?', 1)[0]} served-by={served}")
