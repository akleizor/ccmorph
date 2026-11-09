"""One turn on a sub, token or provider profile writes one usage row.

A launch changes no network path: sub and token sessions keep talking to
api.anthropic.com with their own credential, and a provider session reaches its
bridge as before. What the launch adds is a Stop hook, and this runs the real
`ccmorph run` against a stub `claude` that does what Claude Code does: reads the
hook out of the generated settings, appends a turn to the transcript, and runs
the hook with the hook JSON on stdin. The composition the Go tests cannot show
is the launcher writing the hook into the session's settings and the row it
writes carrying the profile, the model, the tokens and a cost estimate.

The Remote Control host with no route is the same composition one layer out: a
`ccmorph run <sub> -- remote-control` starts the host on the subscription with
nothing fronting it, so the session the phone drives has only that Stop hook to
write its row — no adapter holds one for it.
"""
import json
import os
import socket
import stat
import subprocess
import time
from pathlib import Path

import pytest

from support import FAKE_SERVER_NO_DNS, HANG_TIMEOUT, needs_shim

pytestmark = needs_shim

ROOT = Path(__file__).resolve().parents[1]


def can_bind() -> bool:
    """A provider launch starts a bridge on a loopback port; a sandbox may refuse."""
    try:
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
        return True
    except OSError:
        return False


provider_only = pytest.mark.skipif(
    not can_bind(), reason="a provider launch needs a loopback port and this sandbox refuses binds"
)

# The stub stands in for Claude Code: it records the environment and the Stop
# hooks the launch generated, then plays one turn — one API response, written
# the way the client writes it (one transcript line per content block, all with
# the same message id and requestId) — and the Stop the client itself runs at
# the end of a turn. Started as `claude remote-control` it stands in for the
# Remote Control host instead: the session the phone drives runs in the folder
# the host serves, keeps its transcript in the account config's projects/
# directory like every session of that config dir, and cannot start at all in a
# folder the account has not trusted — the stop a headless check hits in a new
# folder, which is why such a check needs one already trusted.
CLAUDE = r'''#!/usr/bin/env python3
import json, os, re, subprocess, sys
home = os.environ["HOME"]
log = os.path.join(home, "claude.log")
def note(line):
    with open(log, "a") as f:
        f.write(line + "\n")
if any(a == "--help" for a in sys.argv[1:]):
    sys.exit(0)
note("ARGS=" + " ".join(sys.argv[1:]))
for key in ("ANTHROPIC_BASE_URL", "ANTHROPIC_UNIX_SOCKET", "HTTPS_PROXY", "https_proxy",
            "NODE_EXTRA_CA_CERTS", "CLAUDE_CONFIG_DIR"):
    note(key + "=" + os.environ.get(key, "<unset>"))
cfg = os.environ["CLAUDE_CONFIG_DIR"]
with open(os.path.join(cfg, "settings.json")) as f:
    settings = json.load(f)
commands = [h["command"]
            for entry in settings.get("hooks", {}).get("Stop", [])
            for h in entry.get("hooks", [])]
note("STOP=" + json.dumps(commands))
usage = {"input_tokens": 10, "output_tokens": 25, "cache_read_input_tokens": 0}
blocks = [("u1", "thinking"), ("u2", "text"), ("u3", "tool_use")]
if "remote-control" in sys.argv[1:]:
    state = {}
    try:
        with open(os.path.join(cfg, ".claude.json")) as f:
            state = json.load(f)
    except (OSError, ValueError):
        pass
    folder = os.getcwd()
    if not state.get("projects", {}).get(folder, {}).get("hasTrustDialogAccepted"):
        note("STOPPED=Workspace not trusted: " + folder)
        sys.exit(1)
    transcript = os.path.join(cfg, "projects",
                              re.sub(r"[^A-Za-z0-9]", "-", folder), "s1.jsonl")
else:
    transcript = os.path.join(home, "transcript.jsonl")
os.makedirs(os.path.dirname(transcript), exist_ok=True)
with open(transcript, "w") as f:
    f.write(json.dumps({"type": "user", "uuid": "u0",
                        "message": {"role": "user", "content": "hi"}}) + "\n")
    for uuid, block in blocks:
        f.write(json.dumps({
            "type": "assistant", "uuid": uuid, "requestId": "req_e2e",
            "timestamp": "2026-11-05T10:00:00.000Z",
            "message": {"id": "msg_e2e", "model": "claude-opus-5-5", "usage": usage},
            "content": [{"type": block}]}) + "\n")
hook_input = json.dumps({"session_id": "s1", "transcript_path": transcript,
                         "hook_event_name": "Stop"})
for command in commands:
    done = subprocess.run(["sh", "-c", command], input=hook_input, text=True,
                          capture_output=True, timeout=60)
    note("HOOK=%d %s %s" % (done.returncode, command, done.stderr.strip()))
sys.exit(0)
'''

# The bridge stub runs as its own process, so it needs the no-reverse-DNS line
# itself: HTTPServer.server_bind resolves 127.0.0.1 with getfqdn, a lookup that
# stalls past the bridge's 30s readiness budget on a macOS runner with no DNS.
CLIPROXYAPI = r'''#!/usr/bin/env python3
import http.server, json, re, socketserver, sys
''' + FAKE_SERVER_NO_DNS + r'''
socketserver.TCPServer.request_queue_size = 128
port = int(re.search(r"port: (\d+)", open(sys.argv[2]).read()).group(1))
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        data = b'{"data":[{"id":"claude-opus-5-5"}]}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
'''


class Box:
    """An isolated HOME and state dir holding one ccmorph install's files."""

    def __init__(self, home):
        self.home = home
        self.state = home / ".config/ccmorph"
        (self.state / "profiles").mkdir(parents=True)
        (self.state / "auth/codex").mkdir(parents=True)
        (home / "bin").mkdir()
        (home / ".claude/projects").mkdir(parents=True)
        self.script(home / "bin/claude", CLAUDE)
        self.script(home / "bin/cliproxyapi", CLIPROXYAPI)
        state_home = home / ".local/state/ccmorph"
        (state_home / "usage").mkdir(parents=True)
        self.usage = state_home / "usage"
        now = int(time.time())
        for label in ("anthropic", "any"):
            (state_home / ("prices-owned-%s.json" % label)).write_text(json.dumps({
                "host": "owned:" + label, "fetched": now,
                "models": {"claude-opus-5-5": {"input": 4, "output": 20,
                                               "cache_read": 0.2, "cache_write": 5}}}))

    @staticmethod
    def script(path, text):
        path.write_text(text)
        path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)

    def conf(self, name, text):
        (self.state / ("%s.conf" % name)).write_text(text)

    def env(self):
        env = {k: v for k, v in os.environ.items()
               if k != "CLAUDECODE"
               and not k.startswith(("CCMORPH", "_CCMORPH", "CLAUDE_", "ANTHROPIC_", "XDG_"))}
        for k in ("HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "NODE_EXTRA_CA_CERTS"):
            env.pop(k, None)
        env.update(HOME=str(self.home), ZDOTDIR=str(self.home),
                   XDG_CONFIG_HOME=str(self.home / ".config"),
                   CCMORPH_STATE_DIR=str(self.state), CCMORPH_LIB_DIR=str(ROOT / "lib"),
                   CCMORPH_CONTEXT_LOCKED="1",
                   PATH=f"{self.home}/bin:{os.environ['PATH']}",
                   LANG="C.UTF-8", LC_ALL="C.UTF-8", NO_COLOR="1")
        return env

    def run(self, profile, env_extra=None, args=("-p", "hi"), cwd=None):
        env = self.env()
        env.update(env_extra or {})
        return subprocess.run([str(ROOT / "bin/ccmorph"), "run", profile, "--", *args],
                              env=env, cwd=cwd, capture_output=True, text=True,
                              timeout=HANG_TIMEOUT)

    def trust(self, profile, folder):
        """Record folder as already trusted in the account's config — the record
        ccmorph-rc's seed_trust writes for a routed host. Without it a session in
        the folder stops at `Workspace not trusted` before any turn."""
        cfg = self.state / "profiles" / profile / ".claude.json"
        data = json.loads(cfg.read_text()) if cfg.exists() else {}
        data.setdefault("projects", {})[str(Path(folder).resolve())] = {
            "hasTrustDialogAccepted": True}
        cfg.write_text(json.dumps(data))

    def log(self):
        p = self.home / "claude.log"
        return p.read_text() if p.exists() else ""

    def hooks(self):
        line = [l for l in self.log().splitlines() if l.startswith("STOP=")]
        return json.loads(line[-1][len("STOP="):]) if line else []

    def rows(self):
        if not self.usage.exists():
            return []
        # The store is sharded by UTC day folder beside any flat rows (#227).
        return sorted(p for pattern in ("*.json", "*/*.json") for p in self.usage.glob(pattern))

    def wait_rows(self, count, timeout=HANG_TIMEOUT):
        end = time.time() + timeout
        while time.time() < end:
            if len(self.rows()) >= count:
                return self.rows()
            time.sleep(0.05)
        return self.rows()


def one_turn(tmp_path, profile, conf):
    box = Box(tmp_path / "home")
    box.conf(profile, conf)
    out = box.run(profile)
    assert out.returncode == 0, out.stderr
    return box


@pytest.mark.parametrize("profile,conf,pricing", [
    ("sub", "type=sub\nemail=me@example.invalid\n", "anthropic"),
    ("token", "type=token\ntoken=oauth-token-xyz\n", "anthropic"),
    pytest.param("codex", "type=provider\nprovider=codex\n", "any", marks=provider_only),
])
def test_one_turn_writes_one_row_with_profile_model_tokens_and_cost(tmp_path, profile, conf, pricing):
    box = one_turn(tmp_path, profile, conf)
    files = box.wait_rows(1)
    assert len(files) == 1, [f.read_text() for f in files]
    row = json.loads(files[0].read_text())
    assert row["profile"] == profile, row
    assert row["served_model"] == "claude-opus-5-5"
    assert row["usage"]["input_tokens"] == 10
    assert row["usage"]["output_tokens"] == 25
    assert row["usage"]["estimated_cost"] > 0
    assert "cost" not in row["usage"], "nothing billed this turn, so cost must stay absent"
    assert row["complete"] is True
    text = files[0].read_text()
    assert "oauth-token-xyz" not in text and "Bearer" not in text
    hooks = box.hooks()
    assert len(hooks) == 1 and hooks[0].startswith(str(ROOT / "libexec/ccmorph-core") + " usage-row "), hooks
    assert "--profile " + profile in hooks[0] and "--pricing " + pricing in hooks[0]


def test_a_split_response_is_counted_once(tmp_path):
    # Claude Code writes one transcript line per content block (thinking, text,
    # tool_use) with one message id: a reader summing rows must see that turn's
    # tokens once, not once per block.
    box = one_turn(tmp_path, "sub", "type=sub\nemail=me@example.invalid\n")
    files = box.wait_rows(1)
    assert len(files) == 1, [f.name for f in files]
    rows = [json.loads(f.read_text()) for f in files]
    assert sum(r["usage"]["input_tokens"] for r in rows) == 10
    assert sum(r["usage"]["output_tokens"] for r in rows) == 25
    assert sum(r["usage"].get("estimated_cost", 0) for r in rows) == rows[0]["usage"]["estimated_cost"]


def test_a_remote_control_host_on_a_sub_profile_with_no_route_writes_one_row(tmp_path):
    """The case no launch check covered: a Remote Control host started as
    `ccmorph run <sub> -- remote-control` runs on the subscription with no route,
    so no adapter relays the session it serves and no gateway row appears — the
    profile's own Stop hook must write the turn's row. The turn is checked
    headlessly, without the phone; the session cannot start in a folder the
    account has not trusted, so the check runs in one already trusted."""
    box = Box(tmp_path / "home")
    box.conf("sub", "type=sub\nemail=me@example.invalid\n")
    folder = tmp_path / "served"
    folder.mkdir()
    box.run("sub", args=("remote-control",), cwd=folder)
    assert "Workspace not trusted" in box.log(), box.log()
    assert box.rows() == [], box.rows()
    box.trust("sub", folder)
    out = box.run("sub", args=("remote-control",), cwd=folder)
    assert out.returncode == 0, out.stderr
    files = box.wait_rows(1)
    assert len(files) == 1, [f.read_text() for f in files]
    row = json.loads(files[0].read_text())
    assert row["profile"] == "sub", row
    assert row["served_model"] == "claude-opus-5-5"
    assert row["usage"]["input_tokens"] == 10
    assert row["usage"]["output_tokens"] == 25
    assert row["usage"]["estimated_cost"] > 0
    assert "cost" not in row["usage"], "the subscription bills nothing per token"
    assert row["complete"] is True
    proxies = [l for l in box.log().splitlines() if l.startswith("HTTPS_PROXY=")]
    assert proxies[-1] == "HTTPS_PROXY=<unset>", proxies   # no route: nothing fronts the session


def test_the_session_keeps_its_own_network_path(tmp_path):
    for profile, conf in (
        ("sub", "type=sub\nemail=me@example.invalid\n"),
        ("token", "type=token\ntoken=oauth-token-xyz\n"),
    ):
        box = one_turn(tmp_path / profile, profile, conf)
        log = box.log()
        assert "ANTHROPIC_BASE_URL=<unset>" in log, log
        assert "ANTHROPIC_UNIX_SOCKET=<unset>" in log, log
        assert "HTTPS_PROXY=<unset>" in log, log
        assert "https_proxy=<unset>" in log, log
        assert "NODE_EXTRA_CA_CERTS=<unset>" in log, log
        assert box.hooks(), log
        box.wait_rows(1)


@provider_only
def test_a_provider_session_still_reaches_its_bridge(tmp_path):
    box = one_turn(tmp_path, "codex", "type=provider\nprovider=codex\n")
    assert "ANTHROPIC_BASE_URL=http://127.0.0.1:" in box.log(), box.log()
    assert "HTTPS_PROXY=<unset>" in box.log(), box.log()
    assert len(box.wait_rows(1)) == 1


def test_an_ambient_proxy_reaches_the_session_unchanged(tmp_path):
    # ccmorph ships no transport and never sets a proxy of its own, so a caller's
    # value, and a nested launch inside the session that inherits it, are untouched.
    box = Box(tmp_path / "home")
    box.conf("sub", "type=sub\nemail=me@example.invalid\n")
    out = box.run("sub", {"HTTPS_PROXY": "http://127.0.0.1:9", "NO_PROXY": "localhost"})
    assert out.returncode == 0, out.stderr
    log = box.log()
    assert "HTTPS_PROXY=http://127.0.0.1:9" in log, log
    assert "https_proxy=<unset>" in log, log
    assert "NODE_EXTRA_CA_CERTS=<unset>" in log, log
    assert box.hooks(), log


def test_no_hook_and_no_row_for_an_apikey_or_direct_launch(tmp_path):
    for name, conf in (
        ("api", "type=apikey\napi_key=sk-test\n"),
        ("direct", "type=sub\nemail=me@example.invalid\nusage_route=direct\n"),
    ):
        box = one_turn(tmp_path / name, name, conf)
        assert box.hooks() == [], box.log()
        assert box.rows() == [], box.rows()


def test_a_later_direct_launch_removes_the_hook_it_found(tmp_path):
    box = one_turn(tmp_path, "sub", "type=sub\nemail=me@example.invalid\n")
    assert len(box.wait_rows(1)) == 1
    box.conf("sub", "type=sub\nemail=me@example.invalid\nusage_route=direct\n")
    out = box.run("sub")
    assert out.returncode == 0, out.stderr
    assert box.hooks() == [], box.log()
    assert len(box.rows()) == 1, "the earlier turn's row is kept; this launch adds none"


def test_a_launch_repoints_a_stale_cellar_hook_onto_the_stable_path(tmp_path):
    """#287: a Homebrew install runs from the versioned Cellar dir, which an
    upgrade moves and a cleanup deletes — the usage hook a launch wrote named
    it, so every session that lived across the upgrade lost its hook. A launch
    now writes brew's stable opt path, and one that finds a hook naming an
    older version's dead Cellar path re-points it, so the session that still
    runs keeps writing its rows."""
    box = Box(tmp_path / "home")
    box.conf("sub", "type=sub\nemail=me@example.invalid\n")
    brew = tmp_path / "brew"
    tree = brew / "Cellar/ccmorph/0.5.7/libexec"
    tree.mkdir(parents=True)
    (tree / "lib").symlink_to(ROOT / "lib")
    (tree / "libexec").symlink_to(ROOT / "libexec")
    opt = brew / "opt/ccmorph"
    opt.parent.mkdir(parents=True)
    opt.symlink_to(brew / "Cellar/ccmorph/0.5.7")
    # The settings an older ccmorph wrote: its hook names that version's Cellar
    # dir, deleted by the cleanup after the upgrade.
    stale = brew / "Cellar/ccmorph/0.5.5/libexec/libexec/ccmorph-core"
    profile = box.state / "profiles/sub"
    profile.mkdir(parents=True)
    (profile / "settings.json").write_text(json.dumps({"hooks": {"Stop": [
        {"hooks": [{"type": "command", "command": str(stale) +
                    " usage-row --profile sub --pricing anthropic"}]}]}}))
    out = box.run("sub", {"CCMORPH_LIB_DIR": str(tree / "lib")})
    assert out.returncode == 0, out.stderr
    stable = str(opt / "libexec/libexec/ccmorph-core") + \
        " usage-row --profile sub --pricing anthropic"
    assert box.hooks() == [stable], (box.hooks(), box.log())
    assert len(box.wait_rows(1)) == 1, "the re-pointed hook must run and write the turn's row"
