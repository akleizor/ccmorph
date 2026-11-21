#!/usr/bin/env python3
"""Gemini thought-signature round trip through a gateway route (#646) — not auto-run by pytest.

Runs the real Claude Code binary against `ccmorph-core adapter`, which starts the
real cliproxyapi and the stock shim in front of a fake OpenAI chat-completions
gateway on loopback (no network, no spend). The fake gateway answers like Gemini
behind Vertex: the first reply is one Bash tool call whose thought signature
rides in `delta.reasoning_details`, and any later request whose assistant
`tool_calls` message does not carry that signature back in `reasoning_details`
gets Vertex's 400 ("Function call is missing a thought_signature").

PASS means every hop keeps the signature: the translator puts it on the Claude
reply, Claude Code replays it for a model it does not recognize, the shim
neither drops it (malformedThinking) nor resends without it
(resendWithoutThinking), and the translator puts it back on the next request.

Translator resolution: --translator, else `cliproxyapi` on PATH. A translator
without the fix (upstream cliproxyapi, akleizor fork before
v8.0.76-akleizor.4) fails with the thought_signature 400.

Usage:
    make shim
    python3 tests/native_gemini_signature_smoke.py [--claude-bin PATH] [--translator PATH] [--timeout 180]
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

from native_compaction_smoke import resolve_claude_bin, serve
from support import CORE

MODEL = "gemini-4-carbon"
SIGNATURE = "SIG-BASH-646"
MISSING = ("Google Vertex bad request: Function call is missing a thought_signature in "
           "functionCall parts. function call `default_api:Bash`, position 3")
RESEND_LINE = "resending once without thinking blocks"


def chunk(delta, finish=None, usage=None):
    body = {"id": "c1", "object": "chat.completion.chunk", "model": MODEL,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
    if usage:
        body["usage"] = usage
    return "data: " + json.dumps(body) + "\n\n"


def tool_call_reply():
    """One Bash call; its thought signature arrives as Gemini-through-gateway sends it."""
    return (chunk({"role": "assistant"})
            + chunk({"tool_calls": [{"index": 0, "id": "call_bash", "type": "function",
                                     "function": {"name": "Bash", "arguments": json.dumps(
                                         {"command": "echo hi", "description": "say hi"})}}],
                     "reasoning_details": [{"type": "tool", "text": "Bash", "signature": SIGNATURE}]})
            + chunk({}, "tool_calls", {"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10})
            + "data: [DONE]\n\n")


def text_reply():
    return (chunk({"role": "assistant", "content": "done"})
            + chunk({}, "stop", {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6})
            + "data: [DONE]\n\n")


class Gateway(BaseHTTPRequestHandler):
    """A chat-completions gateway that checks thought signatures as Vertex does."""

    protocol_version = "HTTP/1.1"
    requests = []

    def log_message(self, *a):
        pass

    def answer(self, status, text, content_type="application/json"):
        data = text.encode()
        self.send_response(status)
        self.send_header("content-type", content_type)
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        # The listing declares the dialect, so the adapter classifies without inference.
        self.answer(200, json.dumps({"object": "list", "data": [
            {"id": MODEL, "object": "model", "supported_endpoints": ["/v1/chat/completions"]}]}))

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            body = json.loads(raw or b"{}")
        except ValueError:
            body = {}
        Gateway.requests.append(body)
        messages = body.get("messages") or []
        for m in messages:
            if m.get("role") == "assistant" and m.get("tool_calls") \
                    and SIGNATURE not in json.dumps(m.get("reasoning_details") or []):
                return self.answer(400, json.dumps({"error": {
                    "code": 400, "message": MISSING, "status": "INVALID_ARGUMENT"}}))
        if not body.get("stream"):
            return self.answer(500, json.dumps({"error": {"message": "only streaming is faked"}}))
        tool_turn = body.get("tools") and not any(m.get("role") == "tool" for m in messages)
        self.answer(200, tool_call_reply() if tool_turn else text_reply(), "text/event-stream")


def replayed_details(body):
    """reasoning_details on the assistant tool_calls message of one upstream request."""
    for m in body.get("messages") or []:
        if m.get("role") == "assistant" and m.get("tool_calls"):
            return m.get("reasoning_details")
    return None


def run(claude_bin, translator, core, timeout):
    tmp = Path(tempfile.mkdtemp(prefix="ccmorph-646-"))
    gateway = adapter = None
    Gateway.requests = []
    try:
        gateway = serve(Gateway)
        base = f"http://127.0.0.1:{gateway.server_address[1]}"
        env0 = {k: v for k, v in os.environ.items()
                if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC")
                        or k.upper().endswith("_PROXY") or k.startswith("CCMORPH_"))}
        env0.update({"no_proxy": "*", "NO_PROXY": "*"})

        # An isolated home: a stub ccmorph whose _env names the fake gateway, and
        # the chosen translator first on PATH, as the adapter tests do.
        home = tmp / "adapter-home"
        (home / "lib").mkdir(parents=True)
        (home / "bin").mkdir()
        (home / "lib/ccmorph.sh").write_text(
            "ccmorph() {\n"
            '  [[ "$1" == _env ]] || return 1\n'
            f"  print 'ANTHROPIC_BASE_URL={base}'\n"
            "  print 'ANTHROPIC_API_KEY=smoke-key'\n"
            "}\n")
        (home / "bin/cliproxyapi").symlink_to(translator)
        adapter_env = dict(env0, HOME=str(home), CCMORPH_LIB_DIR=str(home / "lib"),
                           CCMORPH_STATE_DIR=str(home),
                           PATH=f"{home / 'bin'}:{env0.get('PATH', '/usr/bin:/bin')}")
        adapter_log = tmp / "adapter.log"
        adapter = subprocess.Popen(
            [str(core), "adapter", "p", "--auto", "--model", MODEL, f"--parent-pid={os.getpid()}"],
            stdout=subprocess.PIPE, stderr=adapter_log.open("w"), text=True, env=adapter_env)
        line = ""
        while not line.startswith("PORT="):
            line = adapter.stdout.readline()
            if not line:
                return {"error": "adapter printed no PORT", "log": adapter_log.read_text()}
        port = int(line.strip().split("=", 1)[1])

        client_home, config_dir, cwd = (tmp / n for n in ("home", "claude-config", "cwd"))
        for d in (client_home, config_dir, cwd):
            d.mkdir()
        env = dict(env0, HOME=str(client_home), CLAUDE_CONFIG_DIR=str(config_dir),
                   ANTHROPIC_BASE_URL=f"http://127.0.0.1:{port}", ANTHROPIC_API_KEY="smoke-key",
                   CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1")
        prompt = json.dumps({"type": "user", "message": {"role": "user",
                                                         "content": "run echo hi with Bash"}}) + "\n"
        proc = subprocess.run(
            [str(claude_bin), "-p", "--input-format", "stream-json", "--output-format", "stream-json",
             "--verbose", "--model", MODEL, "--setting-sources", "", "--strict-mcp-config",
             "--dangerously-skip-permissions", "--tools", "Bash", "--no-session-persistence"],
            cwd=str(cwd), env=env, input=prompt, capture_output=True, text=True, timeout=timeout)
        result = {}
        for out in proc.stdout.splitlines():
            try:
                event = json.loads(out)
            except ValueError:
                continue
            if isinstance(event, dict) and event.get("type") == "result":
                result = event
        return {"exit": proc.returncode, "result": result, "requests": list(Gateway.requests),
                "log": adapter_log.read_text(), "stderr": proc.stderr}
    finally:
        if adapter is not None:
            adapter.terminate()
            try:
                adapter.wait(timeout=10)
            except subprocess.TimeoutExpired:
                adapter.kill()
                adapter.wait(timeout=10)
            adapter.stdout.close()
        if gateway is not None:
            gateway.shutdown()
            gateway.server_close()
        shutil.rmtree(tmp, ignore_errors=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--claude-bin")
    ap.add_argument("--translator")
    ap.add_argument("--core", help="ccmorph-core binary (default: libexec/ccmorph-core from make shim)")
    ap.add_argument("--timeout", type=int, default=180)
    args = ap.parse_args()

    claude_bin = resolve_claude_bin(args.claude_bin)
    if not claude_bin or not claude_bin.exists():
        print("LIMIT: no claude binary found (--claude-bin / $CLAUDE_BIN / PATH) — cannot run.")
        return 1
    translator = args.translator or shutil.which("cliproxyapi")
    if not translator or not Path(translator).exists():
        print("LIMIT: no cliproxyapi found (--translator / PATH) — cannot run.")
        return 1
    core = Path(args.core) if args.core else CORE
    if not core.exists():
        print(f"LIMIT: {core} is missing; run make shim first.")
        return 1
    banner = subprocess.run([translator, "-h"], capture_output=True, text=True).stdout
    version = next((l for l in banner.splitlines() if "Version" in l), "unknown version")
    claude_version = subprocess.run([str(claude_bin), "--version"], capture_output=True,
                                    text=True).stdout.strip()
    print(f"claude {claude_version}; translator {Path(translator).resolve()} ({version.strip()}); "
          "fake loopback gateway, no network, no spend")

    r = run(claude_bin, Path(translator).resolve(), core, args.timeout)
    if "error" in r:
        print(f"FAIL setup: {r['error']}")
        print(r["log"][-1500:])
        print("ASSERTION FAIL")
        return 1
    tool_requests = [b for b in r["requests"] if b.get("tools")]
    second = replayed_details(tool_requests[1]) if len(tool_requests) >= 2 else None
    checks = [
        ("turn completed", r["exit"] == 0 and not r["result"].get("is_error")
         and r["result"].get("result") == "done"),
        ("second request re-sends the signature", SIGNATURE in json.dumps(second or [])),
        ("shim never resent without thinking", RESEND_LINE not in r["log"]),
    ]
    for name, passed in checks:
        print(f"{'PASS' if passed else 'FAIL'} {name}")
    print(f"result: {json.dumps(r['result'].get('result'))[:300]}")
    print(f"second request reasoning_details: {json.dumps(second)[:300]}")
    ok = all(passed for _, passed in checks)
    if not ok:
        for out in (r["log"].strip().splitlines() + r["stderr"].strip().splitlines())[-8:]:
            print(f"    log: {out}")
    print(f"ASSERTION {'PASS' if ok else 'FAIL'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
