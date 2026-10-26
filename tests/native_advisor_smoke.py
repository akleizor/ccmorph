#!/usr/bin/env python3
"""Native advisor-tool canary — not auto-run by pytest.

Runs the real Claude Code binary against a local recording stub (no network,
no credentials, no spend) and reads what the CLI actually put on the wire.
Ground truth is the recorded request, never a log guess.

Proves, at the wire level for 2.1.276, that CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1
removes the `advisor_20260301` server tool and the `advisor-tool-2026-03-01`
beta even when CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL=1 forces the
experimental enable on (disable beats enable) — on the main loop and on an
explicit Opus subagent request, with every run exiting 0.

Frontend canary: it proves what Claude Code sends, not what a gateway or
backend accepts. Binary resolution: --claude-bin, else $CLAUDE_BIN, else `which`.

Usage:
    python3 tests/native_advisor_smoke.py
    python3 tests/native_advisor_smoke.py --claude-bin /path/to/claude
    python3 tests/native_advisor_smoke.py --selfcheck
"""
import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

TOOL_TYPE = "advisor_20260301"
BETA = "advisor-tool-2026-03-01"
MARKER = "CCPROTEUS-ADVISOR-SMOKE-SUBAGENT-MARKER"
# Every path the child could read shared state from; all of them stay private.
SANDBOX_DIRS = ("home", "claude-config", "securestorage", "xdg", "zdotdir", "project")


def _sse(events):
    return "".join(f"event:{e}\ndata:{json.dumps(d)}\n\n" for e, d in events).encode()


def make_sse(tool=None, text="ok"):
    """One fake streamed Messages response: a text block, or one tool_use."""
    head = ("message_start", {"type": "message_start", "message": {
        "id": "msg_smoke", "type": "message", "role": "assistant", "model": "stub",
        "content": [], "stop_reason": None, "stop_sequence": None,
        "usage": {"input_tokens": 10, "output_tokens": 0}}})
    if tool is None:
        mid = [("content_block_start", {"type": "content_block_start", "index": 0,
                                        "content_block": {"type": "text", "text": ""}}),
               ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                        "delta": {"type": "text_delta", "text": text}})]
        stop = "end_turn"
    else:
        name, tool_input = tool
        mid = [("content_block_start", {"type": "content_block_start", "index": 0,
                                        "content_block": {"type": "tool_use", "id": "toolu_smoke",
                                                          "name": name, "input": {}}}),
               ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                        "delta": {"type": "input_json_delta",
                                                  "partial_json": json.dumps(tool_input)}})]
        stop = "tool_use"
    return _sse([head, *mid,
                 ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                 ("message_delta", {"type": "message_delta",
                                    "delta": {"stop_reason": stop, "stop_sequence": None},
                                    "usage": {"input_tokens": 10, "output_tokens": 3}}),
                 ("message_stop", {"type": "message_stop"})])


def advisor_tool(record):
    """The advisor server tool the CLI sent on this request, if any."""
    body = record["body"] if isinstance(record["body"], dict) else {}
    for tool in body.get("tools") or []:
        if tool.get("type") == TOOL_TYPE:
            return tool
    return None


def is_subagent(record):
    """A subagent request carries the marker in its own system prompt.

    Deliberately system-only: a parent request that merely echoes the marker
    in a tool result or an agent listing must not count as a subagent request.
    """
    body = record["body"] if isinstance(record["body"], dict) else {}
    return MARKER in json.dumps(body.get("system"))


def has_advisor_beta(record):
    return BETA in (record.get("beta") or "")


class Stub(BaseHTTPRequestHandler):
    """Records every request; answers locally. Turn 1 optionally spawns an agent.

    Turn 1 either offers the Agent tool or answers with text; every later turn
    answers with end_turn text, so the CLI's turn always ends. The subprocess
    timeout is the hard stop if a run ever stops making progress.
    """

    protocol_version = "HTTP/1.1"
    records = []
    spawn_subagent = False

    def log_message(self, format, *args):  # noqa: A002 - stdlib signature
        pass

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            body = json.loads(raw)
        except ValueError:
            body = {"_unparsed": raw.decode(errors="replace")}
        Stub.records.append({"path": self.path.split("?", 1)[0], "body": body,
                             "beta": self.headers.get("anthropic-beta")})
        turns = len([r for r in Stub.records if r["path"] == "/v1/messages"])
        if Stub.records[-1]["path"] == "/v1/messages/count_tokens":
            payload, ctype = json.dumps({"input_tokens": 10}).encode(), "application/json"
        elif Stub.records[-1]["path"] == "/v1/messages":
            if Stub.spawn_subagent and turns == 1:
                payload = make_sse(tool=("Agent", {"subagent_type": "advisorprobe",
                                                   "description": "smoke probe",
                                                   "prompt": "reply ok"}))
            else:
                payload = make_sse()
            ctype = "text/event-stream"
        else:
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def run_cli(argv, cwd, env, timeout):
    """Bounded subprocess: kills the whole process group, never leaks a child."""
    proc = subprocess.Popen(argv, cwd=str(cwd), env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True, start_new_session=True)
    try:
        out, err = proc.communicate(timeout=timeout)
        return proc.returncode, out, err
    except subprocess.TimeoutExpired:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            proc.kill()
        out, err = proc.communicate()
        return "TIMEOUT", out, err


def run_case(claude_bin, label, *, disable, subagent, timeout):
    """One CLI run against a fresh stub and a 0700 sandbox of private dirs."""
    tmp = tempfile.mkdtemp(prefix="ccproteus-advisor-smoke-")
    os.chmod(tmp, 0o700)
    path = {name: Path(tmp, name) for name in SANDBOX_DIRS}
    for directory in path.values():
        directory.mkdir(parents=True)
    Stub.records, Stub.spawn_subagent = [], subagent
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Stub)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        # Nothing inherited may reach the child: no Claude/Anthropic/ccproteus
        # vars, no proxy vars — it must not find shared hooks, MCP, plugins,
        # settings, credentials or caches.
        env = {k: v for k, v in os.environ.items()
               if not (k.startswith(("CLAUDE", "ANTHROPIC", "CCPROTEUS", "_CCPROTEUS"))
                       or k.endswith("_PROXY") or k in ("no_proxy", "NO_PROXY"))}
        env.update({"HOME": str(path["home"]),
                    "CLAUDE_CONFIG_DIR": str(path["claude-config"]),
                    "CLAUDE_SECURESTORAGE_CONFIG_DIR": str(path["securestorage"]),
                    "XDG_CONFIG_HOME": str(path["xdg"]), "ZDOTDIR": str(path["zdotdir"]),
                    "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{srv.server_address[1]}",
                    "ANTHROPIC_API_KEY": "sk-ant-synthetic-not-a-credential",
                    "PATH": os.environ.get("PATH", "/usr/bin:/bin"), "no_proxy": "*",
                    "NO_PROXY": "*", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                    "CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL": "1"})
        if disable:
            env["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] = "1"
        argv = [str(claude_bin), "-p", "Reply with the single word: ok", "--model", "opus",
                "--output-format", "stream-json", "--verbose", "--setting-sources", "",
                "--disable-slash-commands", "--strict-mcp-config",
                "--mcp-config", '{"mcpServers":{}}', "--no-session-persistence",
                # Neutral prompt: no personal context, and no permission bypass.
                "--system-prompt", "You are a smoke-test probe. Obey the user literally.",
                "--tools", "Agent,Read", "--allowedTools", "Agent",
                "--permission-mode", "dontAsk", "--settings",
                json.dumps({"advisorModel": "opus"})]
        if subagent:
            argv += ["--agents", json.dumps({"advisorprobe": {
                "description": "smoke probe", "prompt": f"{MARKER} you are a probe",
                "model": "opus"}})]
        rc, _, err = run_cli(argv, path["project"], env, timeout)
        calls = [r for r in Stub.records if r["path"] == "/v1/messages"]
        marked = [r for r in calls if is_subagent(r)]
        result = {"exit": rc, "calls": calls, "subagent": marked,
                  "advisor": [r for r in calls if advisor_tool(r)],
                  "beta": [r for r in calls if has_advisor_beta(r)],
                  "advisor_subagent": [r for r in marked if advisor_tool(r)],
                  "beta_subagent": [r for r in marked if has_advisor_beta(r)], "stderr": err}
        print(f"\n=== {label} (disable={disable} subagent={subagent}) ===")
        print(f"  exit={rc}  /v1/messages calls={len(calls)}  subagent calls={len(marked)}")
        for i, call in enumerate(calls):
            print(f"    call {i+1}: model={call['body'].get('model')}"
                  f" subagent={is_subagent(call)} advisor_tool={json.dumps(advisor_tool(call))}"
                  f" advisor_beta={has_advisor_beta(call)}")
        if rc != 0 or not calls:
            for line in (err or "").strip().splitlines()[-8:]:
                print(f"    stderr: {line[:200]}")
        return result
    finally:
        srv.shutdown()
        srv.server_close()
        shutil.rmtree(tmp, ignore_errors=True)


def selfcheck():
    """Parsing helpers must agree with a request the CLI really sends."""
    tool = {"type": TOOL_TYPE, "name": "advisor", "model": "claude-opus-5"}
    hit = {"path": "/v1/messages", "beta": f"claude-code-20250219,{BETA}",
           "body": {"tools": [{"name": "Read"}, tool], "system": "plain main prompt"}}
    assert advisor_tool(hit) == tool and has_advisor_beta(hit) and not is_subagent(hit)
    clean = {"path": "/v1/messages", "beta": "claude-code-20250219",
             "body": {"tools": [{"name": "Read"}]}}
    assert advisor_tool(clean) is None and not has_advisor_beta(clean)
    # Same marker outside the system prompt must not read as a subagent request.
    assert is_subagent({"body": {"system": f"{MARKER} you are a probe"}})
    assert not is_subagent({"body": {"messages": [{"content": MARKER}]}})
    assert '"stop_reason": "end_turn"' in make_sse(text="hi").decode()
    assert '"name": "Agent"' in make_sse(tool=("Agent", {"subagent_type": "x"})).decode()
    print("selfcheck OK")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--claude-bin")
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--selfcheck", action="store_true")
    args = ap.parse_args()
    if args.selfcheck:
        selfcheck()
        return 0

    found = args.claude_bin or os.environ.get("CLAUDE_BIN") or shutil.which("claude")
    if not found or not Path(found).exists():
        print("LIMIT: no claude binary found (--claude-bin / $CLAUDE_BIN / PATH) — cannot run.")
        return 1
    claude_bin = Path(found)
    print(f"advisor smoke against {claude_bin}; private 0700 HOME/CLAUDE_CONFIG_DIR/"
          f"securestorage/XDG/ZDOTDIR, local stub only, no spend")
    print("frontend canary: proves what the CLI sends, not what a backend accepts.")

    kw = {"timeout": args.timeout}
    on = run_case(claude_bin, "advisor forced on", disable=False, subagent=False, **kw)
    off = run_case(claude_bin, "advisor forced on + disabled", disable=True, subagent=False, **kw)
    sub_on = run_case(claude_bin, "opus subagent, advisor forced on", disable=False, subagent=True, **kw)
    sub_off = run_case(claude_bin, "opus subagent + disabled", disable=True, subagent=True, **kw)

    def ran(case):
        """A case only counts if the CLI exited 0 and really hit the API."""
        return case["exit"] == 0 and bool(case["calls"])

    checks = [
        ("control run: exit 0 and reached the API", ran(on), f"exit={on['exit']}, {len(on['calls'])} calls"),
        ("control sent the advisor tool", on["advisor"],
         json.dumps(advisor_tool(on["advisor"][0])) if on["advisor"] else "absent"),
        ("control sent the advisor beta", on["beta"], f"{len(on['beta'])}/{len(on['calls'])} calls"),
        ("disabled run: exit 0 and reached the API", ran(off), f"exit={off['exit']}, {len(off['calls'])} calls"),
        ("disable dropped the advisor tool", not off["advisor"], f"{len(off['advisor'])} calls carried it"),
        ("disable dropped the advisor beta", not off["beta"], f"{len(off['beta'])} calls carried it"),
        ("opus subagent: exit 0 and subagent request seen", ran(sub_on) and sub_on["subagent"],
         f"exit={sub_on['exit']}, {len(sub_on['subagent'])} subagent calls"),
        ("subagent carried the advisor tool", sub_on["advisor_subagent"],
         json.dumps(advisor_tool(sub_on["advisor_subagent"][0])) if sub_on["advisor_subagent"] else "absent"),
        ("subagent carried the advisor beta", sub_on["beta_subagent"],
         f"{len(sub_on['beta_subagent'])}/{len(sub_on['subagent'])} subagent calls"),
        ("disabled subagent: exit 0 and subagent request seen", ran(sub_off) and sub_off["subagent"],
         f"exit={sub_off['exit']}, {len(sub_off['subagent'])} subagent calls"),
        ("disable dropped the advisor tool from the subagent", not sub_off["advisor_subagent"],
         f"{len(sub_off['advisor_subagent'])} subagent calls carried it"),
        ("disable dropped the advisor beta from the subagent", not sub_off["beta_subagent"],
         f"{len(sub_off['beta_subagent'])} subagent calls carried it"),
    ]
    print("\n--- results ---")
    for name, passed, detail in checks:
        print(f"  {'PASS' if passed else 'FAIL'}: {name} ({detail})")
    ok = all(passed for _, passed, _ in checks)
    print(f"\nASSERTION {'PASS' if ok else 'FAIL'}: CLAUDE_CODE_DISABLE_ADVISOR_TOOL removes "
          f"{TOOL_TYPE} and {BETA} even with the experimental advisor enable forced on")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())