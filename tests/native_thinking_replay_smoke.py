#!/usr/bin/env python3
"""Native empty-thinking replay canary — not auto-run by pytest.

Launches the real Claude Code binary against a fake loopback /v1/messages
(no network, no spend), directly and through `libexec/gateway-shim
--preserve-tool-ids`, and reads the SECOND main request's assistant content.
That is the client's own assembly of the first reply, not a handmade SSE
reader, so it shows whether the client retains and replays a streamed
thinking block.

The signatures are SYNTHETIC fixture strings: this shows client assembly and
replay only, not provider acceptance of a signature.

Binary resolution: --claude-bin, else $CLAUDE_BIN, else `which claude`.

Usage:
    python3 tests/native_thinking_replay_smoke.py [--claude-bin PATH] [--timeout 90]
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from http.server import BaseHTTPRequestHandler
from pathlib import Path

from native_compaction_smoke import make_sse, resolve_claude_bin, serve
from support import SHIM

REPO = Path(__file__).resolve().parent.parent

SIGNED_SIGNATURE = "sig-fixture-12-omitted"
HEALTHY_SIGNATURE = "sig-fixture-12-healthy"
HEALTHY_TEXT = "synthetic plan"


def _delta(delta):
    return {"type": "content_block_delta", "index": 0, "delta": delta}


FIXTURES = {
    # The start is empty and no delta ever follows: the malformed block.
    "unsigned": [],
    # A valid omitted-display block: empty text, real signature, no text delta.
    "signed": [_delta({"type": "signature_delta",
                       "signature": SIGNED_SIGNATURE})],
    # Healthy: starts empty, then gains text and its signature.
    "healthy": [_delta({"type": "thinking_delta", "thinking": HEALTHY_TEXT}),
                _delta({"type": "signature_delta",
                        "signature": HEALTHY_SIGNATURE})],
    # No thinking block at all.
    "baseline": None,
}


def make_reply(thinking, canary_file):
    """First reply: the thinking fixture at index 0, then a Read tool_use."""
    events = [{"type": "message_start",
               "message": {"id": "msg", "type": "message", "role": "assistant",
                           "model": "unknown-test", "content": [],
                           "stop_reason": None, "stop_sequence": None,
                           "usage": {"input_tokens": 500, "output_tokens": 0}}}]
    if thinking is not None:
        events.append({"type": "content_block_start", "index": 0,
                       "content_block": {"type": "thinking", "thinking": ""}})
        events.extend(thinking)
        events.append({"type": "content_block_stop", "index": 0})
    index = 1 if thinking is not None else 0
    events += [
        {"type": "content_block_start", "index": index,
         "content_block": {"type": "tool_use", "id": "toolu_1",
                           "name": "Read", "input": {}}},
        {"type": "content_block_delta", "index": index,
         "delta": {"type": "input_json_delta",
                   "partial_json": json.dumps({"file_path": canary_file})}},
        {"type": "content_block_stop", "index": index},
        {"type": "message_delta",
         "delta": {"stop_reason": "tool_use", "stop_sequence": None},
         "usage": {"input_tokens": 500, "output_tokens": 20}},
        {"type": "message_stop"},
    ]
    return "".join(f"event:{e['type']}\ndata:{json.dumps(e)}\n\n"
                   for e in events).encode()


def make_final():
    return make_sse(500, 20, text="done")


class ThinkingUpstream(BaseHTTPRequestHandler):
    """The fixture reply first, then a plain text reply so the turn ends."""

    protocol_version = "HTTP/1.1"
    fixture = None
    canary_file = ""
    requests = []
    main_calls = 0

    def log_message(self, *a):
        pass

    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except Exception:
            body = {"_unparsed": raw.decode(errors="replace")}
        ThinkingUpstream.requests.append({"path": self.path, "body": body})

        if self.path.split("?", 1)[0] != "/v1/messages":
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
            return

        if "naming a coding session" in json.dumps(body):
            payload = make_final()
        else:
            ThinkingUpstream.main_calls += 1
            if ThinkingUpstream.main_calls == 1:
                payload = make_reply(ThinkingUpstream.fixture,
                                     ThinkingUpstream.canary_file)
            else:
                payload = make_final()

        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def main_requests():
    """Every /v1/messages body that is not the background naming call."""
    return [r["body"] for r in ThinkingUpstream.requests
            if r["path"].split("?", 1)[0] == "/v1/messages"
            and "naming a coding session" not in json.dumps(r["body"])]


def replayed_assistant_blocks(body):
    """The assistant content the client sent back on that request, in order."""
    blocks = []
    for message in body.get("messages", []) if isinstance(body, dict) else []:
        if isinstance(message, dict) and message.get("role") == "assistant":
            content = message.get("content")
            if isinstance(content, list):
                blocks.extend(b for b in content if isinstance(b, dict))
    return blocks


def run_case(claude_bin, name, via_shim, timeout):
    tmp = tempfile.mkdtemp(prefix="ccmorph-thinking-")
    srv = shim = None
    try:
        home, config_dir, cwd = (Path(tmp, n) for n in ("home", "claude-config", "cwd"))
        for d in (home, config_dir, cwd):
            d.mkdir(parents=True, exist_ok=True)
        canary_file = cwd / "canary.txt"
        canary_file.write_text("canary payload\n")

        ThinkingUpstream.requests = []
        ThinkingUpstream.main_calls = 0
        ThinkingUpstream.fixture = FIXTURES[name]
        ThinkingUpstream.canary_file = str(canary_file)
        srv = serve(ThinkingUpstream)
        base = f"http://127.0.0.1:{srv.server_address[1]}"

        if via_shim:
            shim = subprocess.Popen(
                [str(SHIM), "0", base, "--preserve-tool-ids",
                 f"--parent-pid={os.getpid()}"],
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            banner = shim.stdout.readline().strip()
            base = f"http://127.0.0.1:{int(banner.split(':')[1].split(' ')[0])}"

        env = {k: v for k, v in os.environ.items()
               if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC")
                       or k.endswith("_PROXY") or k in ("no_proxy", "NO_PROXY"))}
        env.update({
            "HOME": str(home),
            "CLAUDE_CONFIG_DIR": str(config_dir),
            "ANTHROPIC_BASE_URL": base,
            "ANTHROPIC_API_KEY": "canary",
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "no_proxy": "*", "NO_PROXY": "*",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        })

        stream_in = json.dumps({
            "type": "user",
            "message": {"role": "user", "content": "read the canary file"},
        }) + "\n"

        argv = [
            str(claude_bin), "-p",
            "--input-format", "stream-json",
            "--output-format", "stream-json", "--verbose",
            "--model", "unknown-test",
            "--setting-sources", "",
            "--strict-mcp-config",
            "--dangerously-skip-permissions",
            "--tools", "Read",
            "--no-session-persistence",
        ]

        proc = subprocess.run(argv, cwd=str(cwd), env=env, input=stream_in,
                              capture_output=True, text=True, timeout=timeout)
        calls = main_requests()
        replayed = replayed_assistant_blocks(calls[1]) if len(calls) >= 2 else []
        return {"exit": proc.returncode, "main_calls": ThinkingUpstream.main_calls,
                "replayed": replayed, "stderr": proc.stderr}
    finally:
        if shim is not None:
            shim.terminate()
            try:
                shim.wait(timeout=10)
            except subprocess.TimeoutExpired:
                shim.kill()
                shim.wait(timeout=10)
            shim.stdout.close()
        if srv is not None:
            srv.shutdown()
            srv.server_close()
        shutil.rmtree(tmp, ignore_errors=True)


def case_ok(name, route, result):
    if result["exit"] != 0 or result["main_calls"] != 2:
        return False
    blocks = result["replayed"]
    thinking = [b for b in blocks if b.get("type") == "thinking"]
    tool = [b for b in blocks if b.get("type") == "tool_use"]
    if name == "unsigned" and route == "direct":
        return (bool(thinking) and thinking[0].get("thinking") == ""
                and not thinking[0].get("signature"))
    if name == "unsigned":
        return not thinking and any(t.get("id") == "toolu_1" for t in tool)
    if name == "signed":
        return (bool(thinking) and thinking[0].get("thinking") == ""
                and thinking[0].get("signature") == SIGNED_SIGNATURE)
    if name == "healthy":
        return (bool(thinking) and thinking[0].get("thinking") == HEALTHY_TEXT
                and thinking[0].get("signature") == HEALTHY_SIGNATURE)
    return not thinking and bool(tool)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--timeout", type=int, default=90)
    ap.add_argument("--claude-bin")
    args = ap.parse_args()

    claude_bin = resolve_claude_bin(args.claude_bin)
    if not claude_bin or not claude_bin.exists():
        print("LIMIT: no claude binary found (--claude-bin / $CLAUDE_BIN / PATH) — cannot run.")
        return 1

    version = subprocess.run([str(claude_bin), "--version"], capture_output=True,
                             text=True).stdout.strip()
    print(f"claude {version}; fake loopback upstream, no network, no spend")

    ok = True
    observed_replay = None
    for name in ("unsigned", "signed", "healthy", "baseline"):
        for route, via_shim in (("direct", False), ("shim", True)):
            result = run_case(claude_bin, name, via_shim, args.timeout)
            shown = [{k: b.get(k) for k in ("type", "thinking", "signature", "id")}
                     for b in result["replayed"]]
            passed = case_ok(name, route, result)
            ok = ok and passed
            print(f"{'PASS' if passed else 'FAIL'} {name}/{route}: "
                  f"exit={result['exit']} main_calls={result['main_calls']} "
                  f"replayed={json.dumps(shown)}")
            if not passed and result["stderr"].strip():
                for line in result["stderr"].strip().splitlines()[-5:]:
                    print(f"    stderr: {line}")
            if name == "unsigned" and route == "direct":
                observed_replay = bool(result["replayed"]
                                       and any(b.get("type") == "thinking"
                                               for b in result["replayed"]))

    print(f"OBSERVED: client replays unsigned empty thinking = {observed_replay}")
    print(f"ASSERTION {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())