#!/usr/bin/env python3
"""Native compaction canary — not auto-run by pytest.

Launches the real Claude Code binary against a fake local /v1/messages
(no network, no spend) and reads the CLI's own stream-json status events
to see whether it actually compacts once fabricated usage crosses the
configured threshold. Ground truth is the CLI's status event, never a
text-substring guess.

Binary resolution: --claude-bin, else $CLAUDE_BIN, else `which claude`.

Usage:
    python3 native_compaction_smoke.py [--window 100000] [--pct 80]
    python3 native_compaction_smoke.py --selfcheck
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def resolve_claude_bin(cli_arg):
    if cli_arg:
        return Path(cli_arg)
    if os.environ.get("CLAUDE_BIN"):
        return Path(os.environ["CLAUDE_BIN"])
    found = shutil.which("claude")
    return Path(found) if found else None


def make_sse(usage_input, usage_output, tool_call=None, text="ok"):
    """One fake streamed Messages response with fabricated usage."""
    events = [("message_start", {
        "type": "message_start",
        "message": {"id": "msg", "type": "message", "role": "assistant",
                     "model": "unknown-test", "content": [],
                     "stop_reason": None, "stop_sequence": None,
                     "usage": {"input_tokens": usage_input, "output_tokens": 0}},
    })]
    if tool_call:
        events += [
            ("content_block_start", {"type": "content_block_start", "index": 0,
             "content_block": {"type": "tool_use", "id": tool_call["id"],
                                "name": tool_call["name"], "input": {}}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
             "delta": {"type": "input_json_delta",
                       "partial_json": json.dumps(tool_call["input"])}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ]
        stop_reason = "tool_use"
    else:
        events += [
            ("content_block_start", {"type": "content_block_start", "index": 0,
             "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
             "delta": {"type": "text_delta", "text": text}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ]
        stop_reason = "end_turn"
    events += [
        ("message_delta", {"type": "message_delta",
         "delta": {"stop_reason": stop_reason, "stop_sequence": None},
         "usage": {"input_tokens": usage_input, "output_tokens": usage_output}}),
        ("message_stop", {"type": "message_stop"}),
    ]
    body = "".join(f"event:{e}\ndata:{json.dumps(d)}\n\n" for e, d in events)
    return body.encode()


class FakeAnthropic(BaseHTTPRequestHandler):
    """Turn 1: tool_use reporting fabricated usage. Every following turn
    keeps reporting the same fabricated usage and re-issuing a tool_use, up
    to MAX_TURNS, so the CLI has repeated chances to compact. A hard cap
    stops a runaway loop if the CLI never stops calling the tool."""

    protocol_version = "HTTP/1.1"
    requests = []
    call_count = 0
    over_threshold_input = 90000
    canary_file = ""
    MAX_TURNS = 8

    def log_message(self, *a):
        pass

    def do_POST(self):
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except Exception:
            body = {"_unparsed": raw.decode(errors="replace")}
        FakeAnthropic.requests.append({"path": self.path, "body": body})

        if self.path.split("?", 1)[0] != "/v1/messages":
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
            return

        blob = json.dumps(body)
        if "naming a coding session" in blob:
            payload = make_sse(50, 5)
        else:
            FakeAnthropic.call_count += 1
            n = FakeAnthropic.call_count
            if n == 1:
                payload = make_sse(500, 20, tool_call={
                    "id": "toolu_1", "name": "Read",
                    "input": {"file_path": FakeAnthropic.canary_file}})
            elif n < FakeAnthropic.MAX_TURNS:
                payload = make_sse(FakeAnthropic.over_threshold_input, 20,
                                    tool_call={"id": f"toolu_{n}", "name": "Read",
                                               "input": {"file_path": FakeAnthropic.canary_file}})
            else:
                # Cap reached: stop offering a tool so the CLI's turn ends
                # cleanly instead of looping forever against a fake server.
                payload = make_sse(FakeAnthropic.over_threshold_input, 20,
                                    text="done reading")

        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def serve(handler):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def compact_status_events(stdout):
    """Ground truth from the CLI's own stream-json output."""
    out = []
    for line in stdout.splitlines():
        try:
            d = json.loads(line)
        except Exception:
            continue
        if d.get("type") == "system" and d.get("subtype") == "status":
            if d.get("status") == "compacting" or "compact_result" in d:
                out.append(d)
    return out


def run_case(claude_bin, label, *, window, pct, force_turns, timeout):
    tmp = tempfile.mkdtemp(prefix="ccproteus-canary-")
    srv = None
    try:
        home, config_dir, cwd = (Path(tmp, n) for n in ("home", "claude-config", "cwd"))
        for d in (home, config_dir, cwd):
            d.mkdir(parents=True, exist_ok=True)
        canary_file = cwd / "canary.txt"
        canary_file.write_text("canary payload\n")

        FakeAnthropic.requests = []
        FakeAnthropic.call_count = 0
        FakeAnthropic.canary_file = str(canary_file)
        srv = serve(FakeAnthropic)
        port = srv.server_address[1]

        env = {k: v for k, v in os.environ.items()
               if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC")
                       or k.endswith("_PROXY") or k == "no_proxy")}
        env.update({
            "HOME": str(home),
            "CLAUDE_CONFIG_DIR": str(config_dir),
            "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{port}",
            "ANTHROPIC_API_KEY": "canary",
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "no_proxy": "*", "NO_PROXY": "*",
            "CLAUDE_CODE_MAX_CONTEXT_TOKENS": str(window),
            "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": str(pct),
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        })

        stream_in = json.dumps({
            "type": "user",
            "message": {"role": "user", "content": "read the canary file"},
        }) + "\n"

        # ccproteus never issues --autocompact on the CLI; the overlay is the
        # native `autoCompactWindow` settings key. Match that here.
        argv = [
            str(claude_bin), "-p",
            "--input-format", "stream-json",
            "--output-format", "stream-json",
            "--include-partial-messages", "--verbose",
            "--model", "unknown-test",
            "--settings", json.dumps({"autoCompactWindow": window}),
            "--setting-sources", "",
            "--strict-mcp-config",
            "--dangerously-skip-permissions",
            "--tools", "Read",
            "--no-session-persistence",
            "--replay-user-messages",
        ]

        proc = subprocess.run(argv, cwd=str(cwd), env=env, input=stream_in,
                               capture_output=True, text=True, timeout=timeout)
        calls = list(FakeAnthropic.requests)
        events = compact_status_events(proc.stdout)
        results = [e.get("compact_result") for e in events if "compact_result" in e]

        print(f"\n=== {label}: window={window} pct={pct} ===")
        print(f"  exit_code={proc.returncode}  /v1/messages calls={len(calls)}")
        for i, c in enumerate(calls):
            has_tools = "tools" in c["body"] if isinstance(c["body"], dict) else None
            nmsg = len(c["body"].get("messages", [])) if isinstance(c["body"], dict) else -1
            print(f"    call {i+1}: messages={nmsg} tools_offered={has_tools}")
        for e in events:
            print(f"    status_event: {json.dumps(e)}")
        if proc.stderr.strip():
            for line in proc.stderr.strip().splitlines()[-10:]:
                print(f"    stderr: {line}")

        return {"calls": len(calls), "events": events, "results": results,
                "exit": proc.returncode}
    finally:
        if srv is not None:
            srv.shutdown()
            srv.server_close()
        shutil.rmtree(tmp, ignore_errors=True)


def selfcheck():
    raw = make_sse(123, 4, tool_call={"id": "t1", "name": "Read", "input": {"file_path": "/f"}})
    events = [json.loads(l[5:]) for l in raw.decode().splitlines() if l.startswith("data:")]
    assert events[0]["message"]["usage"]["input_tokens"] == 123
    assert events[-2]["usage"] == {"input_tokens": 123, "output_tokens": 4}
    assert events[-1]["type"] == "message_stop"
    assert any(e.get("content_block", {}).get("type") == "tool_use" for e in events)
    print("selfcheck OK")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--window", type=int, default=100000)
    ap.add_argument("--pct", type=int, default=75)
    ap.add_argument("--timeout", type=int, default=100)
    ap.add_argument("--claude-bin")
    ap.add_argument("--selfcheck", action="store_true")
    args = ap.parse_args()

    if args.selfcheck:
        selfcheck()
        return 0

    claude_bin = resolve_claude_bin(args.claude_bin)
    if not claude_bin or not claude_bin.exists():
        print("LIMIT: no claude binary found (--claude-bin / $CLAUDE_BIN / PATH) — cannot run.")
        return 1

    print(f"canary against {claude_bin}, ANTHROPIC_BASE_URL pinned to 127.0.0.1, no real spend")

    over = run_case(claude_bin, "above-threshold", window=args.window, pct=args.pct,
                     force_turns=6, timeout=args.timeout)
    FakeAnthropic.over_threshold_input = 40000  # well under 75% of 100000
    under = run_case(claude_bin, "below-threshold-control", window=args.window, pct=args.pct,
                      force_turns=6, timeout=args.timeout)

    print(f"\nabove: compact_events={len(over['events'])} results={over['results']}")
    print(f"below: compact_events={len(under['events'])} results={under['results']}")

    # Executable assertion: crossing the threshold must produce a status
    # event; staying under it must not. Fails loudly if either is false —
    # never prints a hardcoded conclusion.
    ok = bool(over["events"]) and not under["events"]
    print(f"ASSERTION {'PASS' if ok else 'FAIL'}: "
          f"above-threshold compacted={bool(over['events'])}, "
          f"below-threshold compacted={bool(under['events'])}")
    if "success" in over["results"]:
        print("Real summarization SUCCESS observed (not just triggered-and-failed).")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
