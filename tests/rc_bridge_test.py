"""Remote Control bridge add-on: routing decision and credential isolation."""
import contextlib
import json
import os
import re
import socket
import subprocess
import sys
import threading
import time
import types
from pathlib import Path

import pytest

from support import HANG_TIMEOUT, load_script

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "addons" / "remote-control"))
from rc_route import inference_target, is_inference, reroute  # noqa: E402

OAUTH = "sk-ant-oat01-ACCOUNT-SECRET"


@pytest.mark.parametrize("host,path", [
    ("api.anthropic.com", "/v1/messages"),
    ("api.anthropic.com", "/v1/messages?beta=true"),
    ("API.anthropic.com.", "/v1/messages/count_tokens"),
])
def test_inference_is_rerouted(host, path):
    assert is_inference(host, path)


@pytest.mark.parametrize("host,path", [
    ("api.anthropic.com", "/v1/code/sessions/abc/worker/heartbeat"),
    ("api.anthropic.com", "/v1/environments/bridge"),
    ("api.anthropic.com", "/api/oauth/validate"),
    ("api.anthropic.com", "/v1/messagesX"),
    ("evil.example", "/v1/messages"),
    ("api.anthropic.com.evil.example", "/v1/messages"),
])
def test_everything_else_passes_through(host, path):
    assert not is_inference(host, path)


def test_account_credential_never_reaches_the_route():
    headers = {"Authorization": f"Bearer {OAUTH}", "X-Api-Key": OAUTH, "Cookie": f"s={OAUTH}",
               "anthropic-beta": "oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14",
               "anthropic-version": "2023-06-01", "Host": "api.anthropic.com"}
    out, target = reroute(headers, "https://gw.example:18443", "route-key")
    assert OAUTH not in repr(out)
    assert out["x-api-key"] == "route-key" and out["authorization"] == "Bearer route-key"
    assert out["anthropic-beta"] == "fine-grained-tool-streaming-2025-05-14"
    assert out["anthropic-version"] == "2023-06-01"
    assert out["host"] == "gw.example:18443"
    assert target == ("https", "gw.example", 18443)


def test_only_oauth_beta_means_no_beta_header():
    out, _ = reroute({"anthropic-beta": "oauth-2025-04-20"}, "http://127.0.0.1:5000", "k")
    assert "anthropic-beta" not in out


def test_loopback_adapter_target():
    _, target = reroute({}, "http://127.0.0.1:54505", "k")
    assert target == ("http", "127.0.0.1", 54505)


def test_non_http_upstream_is_refused():
    with pytest.raises(ValueError):
        reroute({}, "file:///etc/passwd", "k")


def test_first_party_only_thinking_display_is_made_portable():
    from rc_route import normalize_body
    body = {"thinking": {"type": "adaptive", "display": "updates"}}
    assert normalize_body(body)
    assert body["thinking"] == {"type": "adaptive", "display": "omitted"}


def test_first_party_cache_scope_is_dropped_but_the_breakpoint_kept():
    from rc_route import normalize_body
    cc = lambda: {"type": "ephemeral", "ttl": "1h", "scope": "global"}
    body = {"system": [{"type": "text", "text": "a"}, {"type": "text", "text": "b", "cache_control": cc()}],
            "messages": [{"role": "user", "content": [{"type": "text", "text": "q", "cache_control": cc()}]}],
            "tools": [{"name": "t", "cache_control": cc()}]}
    assert normalize_body(body)
    assert "scope" not in repr(body)
    assert body["system"][1]["cache_control"] == {"type": "ephemeral", "ttl": "1h"}


def _turn(header_tail, messages):
    """One first-party turn: the billing header, a stable system prompt, the turn's messages."""
    return {"model": "claude-opus-5-5", "tools": [{"name": "t"}],
            "system": [{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.302.a1b;"
                                                " cc_entrypoint=sdk-cli;" + header_tail},
                       {"type": "text", "text": "You are Claude Code.",
                        "cache_control": {"type": "ephemeral", "ttl": "1h", "scope": "global"}}],
            "messages": messages}


def test_consecutive_first_party_turns_share_a_cacheable_prefix():
    """The route caches on the request prefix, so a prompt cache only survives if
    turn 2's body literally starts with turn 1's. A first-party login puts six
    per-request fields in the billing header, which rewrote that prefix every turn."""
    from rc_route import normalize_body
    asked = [{"role": "user", "content": [{"type": "text", "text": "q"}]}]
    t1 = _turn(" cch=1f2e3; cc_prompt_id=11111111-1111-4111-8111-111111111111; cc_turn_origin=user;"
               " cc_prompt_index=0; cc_turn_index=1;", asked)
    t2 = _turn(" cch=9a8b7; cc_prev_req=req_011CAbc; cc_prompt_id=22222222-2222-4222-8222-222222222222;"
               " cc_turn_origin=user; cc_prompt_index=1; cc_turn_index=2;",
               asked + [{"role": "assistant", "content": [{"type": "text", "text": "a"}]},
                        {"role": "user", "content": [{"type": "text", "text": "q2"}]}])
    assert normalize_body(t1) and normalize_body(t2)
    # What the route caches is the body's text: every byte of turn 1, up to the closers
    # of its message list and body, opens turn 2 — which then goes on with a comma.
    t1_json, t2_json = json.dumps(t1), json.dumps(t2)
    assert t2_json.startswith(t1_json[:-2])
    assert t2_json[len(t1_json) - 2] == ","
    assert t2["system"][0]["text"] == "x-anthropic-billing-header: cc_version=2.1.302.a1b; cc_entrypoint=sdk-cli;"


@pytest.mark.parametrize("tail,left", [
    (" cch=abc cc_workload=cron;", " cc_workload=cron;"),   # no ";" yet: the next field stays
    ("\tcch=abc;\ncc_workload=cron;", "\ncc_workload=cron;"),  # its own separator goes, the next one stays
])
def test_a_turn_field_never_takes_its_neighbour_with_it(tail, left):
    from rc_route import normalize_body
    head = "x-anthropic-billing-header: cc_version=2.1.302.a1b; cc_entrypoint=sdk-cli;"
    body = {"system": [{"type": "text", "text": head + tail}]}
    assert normalize_body(body)
    assert body["system"][0]["text"] == head + left


@pytest.mark.parametrize("body", [
    {"thinking": {"type": "adaptive", "display": "omitted"}},
    {"thinking": {"type": "adaptive", "display": "summarized"}},
    {"thinking": {"type": "disabled"}},
    {"messages": []},
    {"system": [{"type": "text", "text": "a", "cache_control": {"type": "ephemeral"}}]},
    {"system": [{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.302.a1b;"
                                         " cc_entrypoint=cli; cc_workload=cron; cc_is_subagent=true;"
                                         " cc_gateway=env;"}]},
    [],
])
def test_portable_bodies_are_left_alone(body):
    from rc_route import normalize_body
    before = repr(body)
    assert not normalize_body(body)
    assert repr(body) == before


@pytest.mark.parametrize("model,sent", [
    ("claude-fable-5-1", "gpt-6-astra"),
    ("claude-fable-5", "gpt-6-astra"),
    ("fable", "gpt-6-astra"),
    ("claude-opus-5-5", "claude-opus-5-5"),
    ("gpt-6-astra", "gpt-6-astra"),
])
def test_pinned_tier_covers_full_ids(model, sent):
    from rc_route import normalize_body
    body = {"model": model}
    assert normalize_body(body, {"fable": "gpt-6-astra"}) == (model != sent)
    assert body["model"] == sent


def _launcher():
    return load_script(Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc",
                       "ccmorph_rc")


def test_a_folder_is_trusted_before_the_host_starts(tmp_path):
    """An unattended start in an untrusted folder waits on a prompt nobody can
    answer; the folder never registers and reads as a dead host."""
    rc = _launcher()
    account, folder = tmp_path / "account", tmp_path / "repo"
    account.mkdir(); folder.mkdir()
    rc.seed_trust(str(account), str(folder))
    data = json.loads((account / ".claude.json").read_text())
    assert data["projects"][str(folder)]["hasTrustDialogAccepted"] is True
    rc.seed_trust(str(account), str(folder))          # idempotent
    assert len((account / ".claude.json").read_text()) > 0
    assert not list(account.glob("*.tmp"))
    (account / ".claude.json").write_text("{ not json")
    rc.seed_trust(str(account), str(folder))          # malformed: left alone, no crash
    assert (account / ".claude.json").read_text() == "{ not json"


def test_inherited_credentials_never_reach_the_session(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("CCMORPH_STATE_DIR", raising=False)
    rc = _launcher()
    for var in ("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"):
        assert var in rc.strip_env()


def test_session_models_are_real_claude_one_to_one():
    rc = _launcher()
    route = {"ANTHROPIC_DEFAULT_FABLE_MODEL": "gpt-6-astra", "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5-5",
             "ANTHROPIC_DEFAULT_SONNET_MODEL": "deepseek-v4.1-flash"}
    inherited = {"ANTHROPIC_DEFAULT_HAIKU_MODEL": "gpt-5.6-luna", "PATH": "/bin"}
    env = rc.session_env(inherited, route, route_models=False)
    # The route's map must not leak (that is the point of the test); a tier the
    # caller pinned itself is forwarded, since only the caller knows why.
    assert "ANTHROPIC_DEFAULT_FABLE_MODEL" not in env and env["PATH"] == "/bin"
    assert env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] == "gpt-5.6-luna"
    assert rc.session_env(inherited, route, route_models=True)["ANTHROPIC_DEFAULT_SONNET_MODEL"] == "deepseek-v4.1-flash"


def test_route_profile_is_resolved_through_route_env(monkeypatch):
    # _route_env, not _env: a provider profile has no endpoint until its bridge
    # runs, and the handle that retires it comes back on the same channel.
    rc = _launcher()
    seen = []

    def fake_run(cmd, **kw):
        seen.append(cmd[2])
        return rc.subprocess.CompletedProcess(cmd, 0, "ANTHROPIC_BASE_URL=https://gw\n"
                                                      "CCMORPH_ROUTE_HANDLE=provider|1|/t|/a\n", "")
    monkeypatch.setattr(rc.subprocess, "run", fake_run)
    env = rc.resolve_route("gw")
    assert seen and "_route_env" in seen[0]
    assert env["CCMORPH_ROUTE_HANDLE"] == "provider|1|/t|/a"


def test_adapter_keeps_the_ambient_proxy_but_no_inherited_credential(monkeypatch):
    # The session loses every proxy (its one proxy is the mitmdump), but the
    # adapter dials the route's real endpoint and must reach it the same way
    # everything else does — through whatever HTTPS_PROXY the caller exported.
    rc = _launcher()
    monkeypatch.setenv("HTTPS_PROXY", "http://bridge:1")
    monkeypatch.setenv("ANTHROPIC_API_KEY", "inherited-key")
    env = rc.adapter_env()
    assert env["HTTPS_PROXY"] == "http://bridge:1"
    assert "ANTHROPIC_API_KEY" not in env
    monkeypatch.delenv("HTTPS_PROXY")
    assert "HTTPS_PROXY" not in rc.adapter_env()


def test_adapter_is_told_the_route_model(tmp_path, monkeypatch):
    """Without --model the adapter sweeps the whole gateway inventory on every launch."""
    rc = _launcher()
    (tmp_path / "gw.conf").write_text("type=apikey\nmodel=claude-opus-5-5\n")
    monkeypatch.setattr(rc, "STATE_ROOT", tmp_path)
    monkeypatch.setattr(rc, "STATE", tmp_path / "bridge")
    seen = {}
    class Stop(Exception):
        pass
    def popen(cmd, **kw):
        seen["cmd"] = cmd
        seen["stderr"] = kw.get("stderr")
        raise Stop
    monkeypatch.setattr(rc.subprocess, "Popen", popen)
    try:
        rc.start_adapter("gw", 1, {})
    except Stop:
        pass
    assert seen["cmd"][-2:] == ["--model", "claude-opus-5-5"]
    # the long-lived adapter must not inherit the caller's stderr (a --switch pipe hung on it)
    assert seen["stderr"].name.endswith("adapter-gw.log")


def test_route_behaviour_flags_reach_the_session_either_way():
    # A gateway that rejects the advisor tool declares it on the route profile; the
    # session must honour it whether or not model aliases come from the route.
    rc = _launcher()
    route = {"ANTHROPIC_BASE_URL": "https://gw", "CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "1"}
    for route_models in (False, True):
        env = rc.session_env({"PATH": "/bin"}, route, route_models=route_models)
        assert env["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] == "1"
        assert "ANTHROPIC_BASE_URL" not in env


def test_a_shared_edge_whose_adapter_died_reports_itself_down(monkeypatch):
    """2026-10-19: the translator exited, the proxy kept answering, and every routed
    call got 502 while the edge looked healthy and was never restarted."""
    rc = _launcher()
    listening = {("127.0.0.1", 47670), ("127.0.0.1", 57706)}

    class Conn:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def connect(addr, timeout):
        if addr not in listening:
            raise ConnectionRefusedError(addr)
        return Conn()
    monkeypatch.setattr(rc.socket, "create_connection", connect)
    assert rc.edge_down(47670, "http://127.0.0.1:57706") is None
    listening.discard(("127.0.0.1", 57706))                      # adapter gone
    assert "upstream on 127.0.0.1:57706" in rc.edge_down(47670, "http://127.0.0.1:57706")
    listening.discard(("127.0.0.1", 47670))                      # proxy gone too
    assert "proxy on 127.0.0.1:47670" in rc.edge_down(47670, "http://127.0.0.1:57706")


def test_a_network_outage_does_not_take_the_shared_edge_down(monkeypatch):
    """GH-461: the Mac's DNS answered nothing for 5h12m on 2026-10-22 while both
    probe targets stayed on loopback. This check decides whether the edge exits to
    be restarted, and it runs every 15s: a probe of a remote host in it would kill
    and relaunch the shared edge for the whole outage, dropping every host's live
    session -- and reconnect nothing, since Claude Code's own backoff already
    recovers the poll."""
    rc = _launcher()
    probed = []

    def connect(addr, timeout):
        probed.append(addr[0])
        if addr[0] != "127.0.0.1":           # a remote probe: what a DNS outage kills
            raise socket.gaierror(8, "nodename nor servname provided, or not known")
        return contextlib.nullcontext()
    monkeypatch.setattr(rc.socket, "create_connection", connect)
    assert rc.edge_down(47670, "http://127.0.0.1:57706") is None
    assert set(probed) == {"127.0.0.1"}


def test_shared_edge_carries_route_flags_for_later_hosts():
    # A host that reuses a live edge takes model ids AND route flags from the edge's
    # record, not from a fresh resolve — so an edge whose record omits a flag serves
    # every later host without it. The record carries every key MODEL_ENV and ROUTE_ENV
    # name, and no unrelated environment entry rides along.
    rc = _launcher()
    pinned = {
        "ANTHROPIC_DEFAULT_FABLE_MODEL": "fable-x",
        "ANTHROPIC_DEFAULT_OPUS_MODEL": "opus-x",
        "ANTHROPIC_DEFAULT_SONNET_MODEL": "sonnet-x",
        "ANTHROPIC_DEFAULT_HAIKU_MODEL": "haiku-x",
        "ANTHROPIC_SMALL_FAST_MODEL": "small-x",
        "CLAUDE_CODE_SUBAGENT_MODEL": "subagent-x",
        "CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "0",
        "CLAUDE_CODE_MODEL_CAPABILITIES": "-mid_conv_effort",
    }
    unrelated = {"PATH": "/bin", "ANTHROPIC_BASE_URL": "http://u", "HOME": "/home/u"}
    record = rc.edge_record(1, 47100, "/ca.pem", "http://u", {**pinned, **unrelated}, {})
    assert record["route_env"] == pinned


def test_the_edge_record_names_the_tiers_its_proxy_pins():
    """The record is the only thing a later supervisor can compare its config to:
    without the tiers in it, an edge adopted after a restart keeps the pins it was
    started with for as long as it lives (issue #356)."""
    rc = _launcher()
    record = rc.edge_record(1, 47100, "/ca.pem", "http://u", {},
                            {"ANTHROPIC_DEFAULT_FABLE_MODEL": "new-model[1m]", "PATH": "/bin"})
    assert record["tiers"] == {"fable": "new-model"}
    assert (record["pid"], record["port"], record["ca"]) == (1, 47100, "/ca.pem")


def test_an_edge_is_identified_by_its_start_not_just_its_pid(monkeypatch):
    """issue #595: a handoff naming only a pid reads as live when the OS gives that
    pid to the edge's replacement. The start cannot change for a living process, so
    the supervisor can tell the newcomer from the edge the hand was made beside. It
    is the kernel's own start -- Linux ticks with the boot id, macOS microseconds --
    never `ps`, whose whole second a pid reused within it shares; a pid with no
    process has no start."""
    rc = _launcher()
    monkeypatch.setattr(subprocess, "run", lambda *a, **k: pytest.fail("the start was read from ps"))
    ident = rc.edge_started(os.getpid())
    assert ident and rc.edge_started(os.getpid()) == ident
    if sys.platform == "darwin":
        assert re.fullmatch(r"darwin:\d+\.\d{6}", ident)
    other = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
    try:
        assert rc.edge_started(other.pid) not in (None, ident)
    finally:
        other.kill()
        other.wait()
    assert rc.edge_started(other.pid) is None


def test_a_tier_pinned_by_the_caller_survives_into_the_session():
    # rc-hosts pins tier.fable=gpt-6-astra because the route serves no claude-fable
    # model. session_env strips model aliases, and used to strip that pin with them.
    rc = _launcher()
    route = {"ANTHROPIC_BASE_URL": "https://gw", "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5-5[1m]"}
    inherited = {"ANTHROPIC_DEFAULT_FABLE_MODEL": "gpt-6-astra", "PATH": "/bin"}
    env = rc.session_env(inherited, route, route_models=False)
    assert env["ANTHROPIC_DEFAULT_FABLE_MODEL"] == "gpt-6-astra"   # the pin is kept
    assert "ANTHROPIC_DEFAULT_OPUS_MODEL" not in env               # the route's map is not
    assert rc.session_env(inherited, route, route_models=True)["ANTHROPIC_DEFAULT_OPUS_MODEL"] == "claude-opus-5-5[1m]"


def test_a_folder_keeps_one_host_across_restarts(tmp_path):
    """A fresh pointer is kept in the folder; a dropped one is put back from it."""
    import os, re
    rc = _launcher()
    account, folder = tmp_path / "account", tmp_path / "repo"
    folder.mkdir()
    pointer = account / "projects" / re.sub(r"[^A-Za-z0-9]", "-", str(folder.resolve())) / "bridge-pointer.json"
    rc.keep_host(str(account), str(folder))                      # first start: nothing
    assert not pointer.exists()
    pointer.parent.mkdir(parents=True)
    pointer.write_text(json.dumps({"sessionId": "session_01A", "environmentId": "env_01K",
                                   "source": "standalone", "pid": 99}))
    rc.keep_host(str(account), str(folder))
    pointer.unlink()                                             # Claude dropped it
    rc.keep_host(str(account), str(folder))
    assert json.loads(pointer.read_text()) == {"sessionId": "session_01A",
                                               "environmentId": "env_01K", "source": "standalone"}
    os.utime(pointer, (0, 0))                                    # stale (> 4h)
    rc.keep_host(str(account), str(folder))
    assert time.time() - pointer.stat().st_mtime < 60


def test_sessions_send_the_whole_conversation_never_a_server_thread():
    """A route holds no server-side thread, so `thread: continue` would send it
    only the newest turn: the session must run with message threads off."""
    rc = _launcher()
    assert rc.session_env({"CLAUDE_CODE_TETHER_LIVE": "1"}, {}, False)["CLAUDE_CODE_TETHER_LIVE"] == "0"
    assert rc.session_env({}, {}, True)["CLAUDE_CODE_TETHER_LIVE"] == "0"


def test_event_uploads_keep_only_what_the_phone_shows():
    from rc_route import TEXT_MAX, is_event_upload, slim_events
    assert is_event_upload("api.anthropic.com", "/v1/code/sessions/cse_01Ab/worker/events")
    assert not is_event_upload("api.anthropic.com", "/v1/code/sessions/cse_01Ab/worker/events/stream")
    long = "x" * (TEXT_MAX * 3)
    shot = {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "A" * 200_000}}
    icon = {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "B" * 500}}
    body = {"worker_epoch": 1, "events": [
        {"payload": {"type": "control_response", "response": {"response": {
            "models": [{"value": "opus"}],
            "commands": [{"name": "a", "argumentHint": "<x>", "description": "long"}]}}}},
        {"payload": {"type": "assistant", "wire_tool_inputs": {"t": {"command": "ls"}}, "message": {"content": [
            {"type": "thinking", "thinking": "why", "signature": "S" * 900},
            {"type": "tool_use", "id": "t", "input": {"command": "ls"}}]}}},
        {"payload": {"type": "user", "message": {"content": [
            {"type": "tool_result", "tool_use_id": "t", "content": [shot, icon, {"type": "text", "text": long}]},
            {"type": "tool_result", "tool_use_id": "u", "content": long}]},
         "tool_use_result": {"originalFile": long, "structuredPatch": [{"lines": ["-a", "+b"]}], "stdout": long}}},
    ]}
    assert slim_events(body, lambda data, kind: ("C" * 9, "image/jpeg"))
    listing, said, result = (e["payload"] for e in body["events"])
    assert listing["response"]["response"]["commands"] == [{"name": "a", "argumentHint": "<x>"}]
    assert listing["response"]["response"]["models"] == [{"value": "opus"}]
    assert "wire_tool_inputs" not in said
    assert said["message"]["content"][0] == {"type": "thinking", "thinking": "why", "signature": ""}
    assert said["message"]["content"][1]["input"] == {"command": "ls"}
    first, second = result["message"]["content"]
    assert first["content"][0]["source"] == {"type": "base64", "media_type": "image/jpeg", "data": "C" * 9}
    assert first["content"][1] == icon
    assert first["content"][2]["text"].startswith("x" * TEXT_MAX) and len(first["content"][2]["text"]) < TEXT_MAX + 100
    assert len(second["content"]) < TEXT_MAX + 100
    assert result["tool_use_result"] == {"structuredPatch": [{"lines": ["-a", "+b"]}]}   # the diff stays
    assert not slim_events(body, lambda data, kind: None)   # already slim: nothing to do
    # A tool_result without `content` has always gained `content: null`; that key
    # insertion is a change the flag has to carry, like any rewrite of its value.
    bare = {"events": [{"payload": {"type": "user", "message": {"content": [
        {"type": "tool_result", "tool_use_id": "v"}]}}}]}
    assert slim_events(bare, lambda data, kind: None)
    assert bare["events"][0]["payload"]["message"]["content"][0]["content"] is None
    assert not slim_events(bare, lambda data, kind: None)


def test_one_event_upload_is_serialized_at_most_once(monkeypatch):
    """slim_events knows what it changed while it walks the body; re-encoding a
    multi-MB upload to diff it against itself doubled every session open."""
    import rc_route
    serialized = []
    real_encode = rc_route.encode_json

    def counting(obj, ensure_ascii=False):
        serialized.append(1)
        return real_encode(obj, ensure_ascii=ensure_ascii)

    monkeypatch.setattr(rc_route, "encode_json", counting)
    body = {"events": [
        {"payload": {"type": "user", "message": {"content": [
            {"type": "tool_result", "tool_use_id": "t", "content": [
                {"type": "image", "source": {"type": "base64", "media_type": "image/png",
                                             "data": "A" * 200_000}},
                {"type": "text", "text": "x" * 10_000}]}]},
         "tool_use_result": {"stdout": "y" * 10_000}}}]}
    assert rc_route.slim_events(body, lambda data, kind: ("C" * 9, "image/jpeg"))
    assert len(serialized) <= 1, f"one upload serialized the body {len(serialized)} times"
    serialized.clear()
    assert not rc_route.slim_events(body, lambda data, kind: ("C" * 9, "image/jpeg"))
    assert len(serialized) <= 1, f"an already-slim upload serialized the body {len(serialized)} times"


def test_the_proxy_inherits_a_raised_open_file_limit(monkeypatch):
    import resource
    m = _launcher()
    calls = []
    monkeypatch.setattr(resource, "getrlimit", lambda _: (256, resource.RLIM_INFINITY))
    monkeypatch.setattr(resource, "setrlimit", lambda _, v: calls.append(v))
    m.raise_nofile()
    assert calls == [(m.NOFILE, resource.RLIM_INFINITY)]


def test_session_keeps_claude_prompt_prefix_stable_behind_the_adapter():
    # The route's adapter hoists mid-conversation system turns, which breaks prompt
    # caching; the phone session must get the same default an adapter launch gets.
    rc = _launcher()
    env = rc.session_env({"PATH": "/bin"}, {}, route_models=False)
    assert env["CLAUDE_CODE_MODEL_CAPABILITIES"] == rc.ADAPTER_MODEL_CAPABILITIES
    root = rc.Path(rc.__file__).resolve().parents[2]
    assert rc.ADAPTER_MODEL_CAPABILITIES in "".join(
        p.read_text() for p in sorted((root / "cli").glob("*.go")))
    # A value the caller set is never overruled.
    own = rc.session_env({"CLAUDE_CODE_MODEL_CAPABILITIES": "x=-y"}, {}, route_models=False)
    assert own["CLAUDE_CODE_MODEL_CAPABILITIES"] == "x=-y"
    # The advisor is withheld the same way, and a route that serves it can say so.
    assert env["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] == "1"
    keep = rc.session_env({"PATH": "/bin"}, {"CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "0"}, route_models=False)
    assert keep["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] == "0"


@pytest.mark.parametrize("confs, must, must_not", [
    # Issue #276: with no Claude account profile there is nothing to pass --as, so
    # the refusal must name the command that creates one instead.
    pytest.param({}, ["Claude account profiles found", "ccmorph add"], ["--as"], id="none-names-add-issue-276"),
    pytest.param({"a.conf": "type=sub\n", "b.conf": "type=sub\n"}, ["--as", "a", "b"], [],
                 id="two-still-ask-for-as"),
])
def test_the_account_refusal_names_a_step_that_can_be_taken(tmp_path, monkeypatch, capsys,
                                                            confs, must, must_not):
    """`default_account()` refuses to guess in two shapes, and each refusal must
    name a step the reader can actually take: with no Claude account
    profile, the command that creates one; with several, the --as choice between
    the profiles it lists."""
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(tmp_path))
    for name, text in confs.items():
        (tmp_path / name).write_text(text)
    rc = _launcher()
    with pytest.raises(SystemExit) as raised:
        rc.default_account()
    assert raised.value.code == 1
    err = capsys.readouterr().err
    for needle in must:
        assert needle in err, err
    for needle in must_not:
        assert needle not in err, err


def test_a_route_declared_model_mask_reaches_the_session():
    """A route that declares its own capability mask must reach the phone session.

    The adapter default is a gap-filler for a route that declares nothing; a route
    that names its own mask describes the endpoint being dialed, exactly as the
    advisor flag does, so it must not hinge on --route-models either.
    """
    rc = _launcher()
    route = {"ANTHROPIC_BASE_URL": "https://gw",
             "CLAUDE_CODE_MODEL_CAPABILITIES": "claude-*=-mid_conv_tool_change"}
    for route_models in (False, True):
        env = rc.session_env({"PATH": "/bin"}, route, route_models=route_models)
        assert env["CLAUDE_CODE_MODEL_CAPABILITIES"] == "claude-*=-mid_conv_tool_change"
        assert "-mid_conv_system" not in env["CLAUDE_CODE_MODEL_CAPABILITIES"]
    # A route that declares nothing still gets the adapter default, and a value the
    # caller itself set is never overruled.
    assert rc.session_env({"PATH": "/bin"}, {}, route_models=False)["CLAUDE_CODE_MODEL_CAPABILITIES"] == rc.ADAPTER_MODEL_CAPABILITIES
    assert rc.session_env({"CLAUDE_CODE_MODEL_CAPABILITIES": "x=-y"}, {}, route_models=False)["CLAUDE_CODE_MODEL_CAPABILITIES"] == "x=-y"
    # A route-declared mask travels to later hosts through the shared edge record.
    assert "CLAUDE_CODE_MODEL_CAPABILITIES" in rc.ROUTE_ENV


def test_default_account_names_claude_account_profiles(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(tmp_path))
    rc = _launcher()
    with pytest.raises(SystemExit):
        rc.default_account()
    err = capsys.readouterr().err
    assert "Claude account profiles found" in err
    assert "subscription" not in err


def test_inference_switch_is_read_live_and_defaults_to_the_route(tmp_path):
    switch = tmp_path / "inference"
    assert inference_target(None) == "route"
    assert inference_target(str(switch)) == "route"          # no file: the route, as before
    switch.write_text("account\n")
    assert inference_target(str(switch)) == "account"        # the next turn sees it, no restart
    switch.write_text("route\n")
    assert inference_target(str(switch)) == "route"
    switch.write_text("oli\n")
    assert inference_target(str(switch)) == "oli"            # a profile --switch brought up
    switch.write_text("../etc/x\n")
    assert inference_target(str(switch)) == "route"          # not a profile name: the route


ADDON = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "rc_addon.py"


def _addon(monkeypatch, tmp_path, word):
    """The addon loaded with a stub mitmproxy: its decision, not the proxy, is
    what these tests read. The end-to-end path is rc_bridge_failclosed_test.py,
    which needs a real mitmdump; this table runs wherever the suite does."""
    import types
    http = types.ModuleType("mitmproxy.http")
    http.HTTPFlow = type("HTTPFlow", (), {})
    package = types.ModuleType("mitmproxy")
    package.http = http
    monkeypatch.setitem(sys.modules, "mitmproxy", package)
    monkeypatch.setitem(sys.modules, "mitmproxy.http", http)
    key = tmp_path / "route-key"
    key.write_text("route-key")
    switch = tmp_path / "inference"
    switch.write_text(word + "\n")
    monkeypatch.setenv("RC_UPSTREAM", "http://127.0.0.1:5000")
    monkeypatch.setenv("RC_KEY_FILE", str(key))
    monkeypatch.setenv("RC_INFERENCE", str(switch))
    for name in ("RC_TIERS", "RC_LOG"):
        monkeypatch.delenv(name, raising=False)
    return load_script(ADDON, "rc_addon_decision")


class _Flow:
    """What requestheaders touches: the request and the flow's metadata."""

    class Request:
        pretty_host = "api.anthropic.com"
        path = "/v1/messages"
        method = "POST"
        scheme, host, port = "https", "api.anthropic.com", 443

        def __init__(self):
            self.headers = {"authorization": "Bearer ACCOUNT"}

    def __init__(self):
        self.request = self.Request()
        self.metadata = {}


@pytest.mark.parametrize("word, first_party", [("account", True), ("route", False)])
def test_the_account_word_marks_what_it_leaves_first_party(word, first_party, tmp_path, monkeypatch):
    """GH-594: the account word hands the call to Anthropic untouched, on the
    destination the fail-closed guard closes. It must mark that flow, and the
    route word must not: only the addon's mark tells a legitimate account call
    from one no addon ever saw."""
    addon = _addon(monkeypatch, tmp_path, word)
    flow = _Flow()
    addon.requestheaders(flow)
    assert (flow.metadata.get(addon.ACCOUNT_METADATA) is True) == first_party
    if first_party:
        assert (flow.request.scheme, flow.request.host) == ("https", "api.anthropic.com")
        assert flow.request.headers == {"authorization": "Bearer ACCOUNT"}
    else:
        assert flow.metadata["rc_route"] is True
        assert flow.request.headers["x-api-key"] == "route-key"


def test_the_guard_exempts_the_mark_the_account_branch_sets(tmp_path, monkeypatch):
    """The launcher's block_list and the addon name one metadata key: the guard
    closes the account destination for every flow without it, and the account
    branch is the only code that sets it."""
    addon = _addon(monkeypatch, tmp_path, "account")
    assert f"!~meta {addon.ACCOUNT_METADATA}" in _launcher().FAIL_CLOSED


def test_a_pinned_tier_reaches_the_backend_without_the_context_marker():
    """oli pins opus to claude-opus-5-5[1m]; its backend 404s on the marker (2026-11-16)."""
    from rc_route import normalize_body
    body = {"model": "claude-opus-5-5"}
    normalize_body(body, {"opus": "claude-opus-5-5[1m]"})
    assert body["model"] == "claude-opus-5-5"
    body = {"model": "claude-opus-5-5[1m]"}
    assert normalize_body(body, {"opus": "claude-opus-5-5[1m]"})
    assert body["model"] == "claude-opus-5-5"


def test_an_rc_session_loads_the_router_plugin_while_the_router_is_on(tmp_path, monkeypatch):
    """`claude remote-control` is exec'd directly, not through `ccmorph run`, so
    the session took no plugin path and never saw the router's delegate tool."""
    rc = _launcher()
    if not rc.ADAPTER.exists():
        pytest.skip(f"{rc.ADAPTER} is not built: run `make shim` first")
    env = {"PATH": "/usr/bin:/bin", "HOME": str(tmp_path), "CCMORPH_STATE_DIR": str(tmp_path),
           "CCMORPH_LIB_DIR": str(rc.LIB_DIR)}
    share = rc.LIB_DIR.parent / "share"
    assert rc.plugin_dirs(env) == str(share / "claude-plugin")
    (tmp_path / "router.json").write_text('{"enabled": true}')
    assert rc.plugin_dirs(env) == f"{share / 'claude-plugin'}:{share / 'claude-plugin-router'}"

def _switch_rig(rc, monkeypatch, tmp_path, alive=None, serving=None):
    """One live edge, a key profile, and seams where real adapter processes would be:
    sidecar state (None: none yet; a dict: this record on disk), whether the recorded
    pid is really our adapter for the profile, whether its port answers."""
    state = tmp_path / "bridge"
    (state / "edges").mkdir(parents=True)
    (state / "edges" / "gateway.json").write_text('{"pid": 500, "port": 8080}')
    monkeypatch.setattr(rc, "STATE", state)
    monkeypatch.setattr(rc, "EDGES", state / "edges")
    monkeypatch.setattr(rc, "INFERENCE", state / "inference")
    monkeypatch.setattr(rc, "profile_type", lambda p: "apikey")
    monkeypatch.setattr(rc, "live_edge", lambda r: {"pid": 500} if r == "gateway" else None)
    monkeypatch.setattr(rc, "edge_started", lambda pid: f"start-{pid}")
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-oli"})
    monkeypatch.setattr(rc, "retire_route_with", lambda *a: None)
    monkeypatch.setattr(rc, "adapter_env", lambda: {})
    monkeypatch.setattr(rc, "our_adapter", lambda pid, profile, edge: bool(alive))
    monkeypatch.setattr(rc, "_listening", lambda upstream: bool(serving))
    if alive is not None:
        (state / "sidecar-oli.json").write_text(json.dumps(
            {"pid": 7321, "upstream": "http://127.0.0.1:45110", "edge": 500, "edge_started": "start-500"}))
    started, signals = [], []
    ports, pids = iter([45111, 45112, 45113]), iter([4321, 4322, 4323])
    def start(route, parent, env):
        url = f"http://127.0.0.1:{next(ports)}"
        started.append((route, parent, url, next(pids)))
        return started[-1][2], started[-1][3]
    monkeypatch.setattr(rc, "start_adapter", start)
    living = {7321}
    def kill(pid, sig):
        if sig:
            signals.append(sig.name)
            living.discard(pid)                      # it takes the signal and dies
        if pid not in living:
            raise ProcessLookupError
    monkeypatch.setattr(rc.os, "kill", kill)
    return state, started, signals


@pytest.mark.parametrize("alive, serving, starts, sigs", [
    pytest.param(None, None, 1, [], id="first-switch-starts-one"),
    pytest.param(True, True, 0, [], id="repeat-switch-reuses-the-serving-adapter-issue-595"),
    pytest.param(True, False, 1, ["SIGTERM"], id="dead-sidecar-retired-then-replaced-issue-595"),
    pytest.param(False, True, 1, [], id="foreign-pid-in-the-record-gets-no-signal-issue-595"),
])
def test_one_sidecar_adapter_per_edge_and_profile(tmp_path, monkeypatch, alive, serving, starts, sigs):
    """issue #595: every repeated `--switch` ran start_adapter again, so each retry
    doubled the sidecar and orphaned the old adapter beside the same edge. A repeat
    reuses the one that answers, or retires the old one -- confirmed gone -- before
    starting its replacement, and the handoff always names the one upstream."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path, alive, serving)
    rc.switch("oli", "alex")
    assert len(started) == starts
    assert signals == sigs
    used = started[0][2] if started else "http://127.0.0.1:45110"
    assert json.loads((state / "switch-oli.json").read_text())["upstream"] == used
    assert json.loads((state / "switch-oli.json").read_text())["edge"] == 500
    assert json.loads((state / "switch-oli.json").read_text())["edge_started"] == "start-500"
    assert (state / "inference").read_text() == "oli\n"
    if started:
        assert (started[0][0], started[0][1]) == ("oli", 500)   # owned by the one live edge
        assert json.loads((state / "sidecar-oli.json").read_text())["pid"] == started[0][3]


@pytest.mark.parametrize("recorded, now", [
    pytest.param("start-500", "later-500", id="recycled-edge-pid-retires-the-old-sidecar-issue-595"),
    pytest.param(None, "start-500", id="ledger-without-an-edge-start-is-not-reused-issue-595"),
])
def test_a_sidecar_is_reused_only_beside_the_edge_process_it_was_made_for(tmp_path, monkeypatch,
                                                                          recorded, now):
    """issue #595 review: the ledger named only the edge's pid, so an adapter made
    beside an edge that exited was reused beside the next edge the OS gave that
    number. The ledger records the edge's start; another start (or none) retires the
    old adapter and starts one for the edge that is live now."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path, alive=True, serving=True)
    rec = {"pid": 7321, "upstream": "http://127.0.0.1:45110", "edge": 500}
    if recorded is not None:
        rec["edge_started"] = recorded
    (state / "sidecar-oli.json").write_text(json.dumps(rec))
    monkeypatch.setattr(rc, "edge_started", lambda pid: now)
    rc.switch("oli", "alex")
    assert signals == ["SIGTERM"]
    assert [(s[0], s[1]) for s in started] == [("oli", 500)]
    ledger = json.loads((state / "sidecar-oli.json").read_text())
    assert (ledger["pid"], ledger["edge"], ledger["edge_started"]) == (4321, 500, now)
    hand = json.loads((state / "switch-oli.json").read_text())
    assert (hand["upstream"], hand["edge_started"]) == ("http://127.0.0.1:45111", now)


def test_a_switch_that_cannot_read_its_edges_start_changes_nothing(tmp_path, monkeypatch):
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    monkeypatch.setattr(rc, "edge_started", lambda pid: None)
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert started == [] and signals == []
    assert not (state / "switch-oli.json").exists() and not (state / "inference").exists()


@pytest.mark.parametrize("edges, ca, lands", [
    pytest.param(1, True, True, id="one-live-edge-names-the-switch-that-lands"),
    pytest.param(1, False, False, id="ca-gone-names-no-switch-that-refuses-issue-595"),
    pytest.param(2, True, False, id="two-live-edges-name-no-switch-that-refuses-issue-595"),
])
def test_the_note_names_switch_only_where_switch_lands(tmp_path, monkeypatch, edges, ca, lands):
    import rc_route
    rc = _launcher()
    bridge = tmp_path / "bridge"
    (bridge / "edges").mkdir(parents=True)
    pem = tmp_path / "ca.pem"
    if ca:
        pem.write_text("ca")
    for i in range(edges):
        (bridge / "edges" / f"e{i}.json").write_text(json.dumps(
            {"pid": os.getpid(), "port": 47101 + i, "ca": str(pem)}))
    (bridge / "landed-oli.json").write_text("{}")    # the marker the old note trusted: it must not matter
    monkeypatch.setattr(rc_route.socket, "create_connection", lambda *a, **k: contextlib.nullcontext())
    monkeypatch.setattr(rc, "STATE", bridge)
    monkeypatch.setattr(rc, "EDGES", bridge / "edges")
    monkeypatch.setattr(rc, "INFERENCE", bridge / "inference")
    monkeypatch.setattr(rc, "profile_type", lambda p: "apikey")
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-oli"})
    monkeypatch.setattr(rc, "retire_route_with", lambda *a: None)
    monkeypatch.setattr(rc, "sidecar", lambda p, owner, started: ("http://127.0.0.1:45111", 4321))
    note = rc_route.lost_endpoint_note("oli", bridge)
    assert ("ccmorph-rc oli --switch" in note) is lands
    if lands:
        rc.switch("oli", "alex")
        assert (bridge / "inference").read_text() == "oli\n"
    else:
        assert "--switch" not in note
        with pytest.raises(SystemExit):
            rc.switch("oli", "alex")


def test_a_sidecar_that_refuses_to_die_aborts_the_switch_instead_of_doubling(tmp_path, monkeypatch):
    """issue #595's second half: an adapter replaced while it still answers its port
    must be gone, not left serving an old endpoint. If it ignores both signals the
    switch refuses to start a second one."""
    import signal as sig_mod
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path, alive=True, serving=False)
    monkeypatch.setattr(rc.os, "kill", lambda pid, s: signals.append(s) or 0)   # never dies
    monkeypatch.setattr(rc.time, "sleep", lambda _: None)                       # the wait, scripted
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert started == []                                  # never doubled: no replacement
    assert sig_mod.SIGTERM in signals and sig_mod.SIGKILL in signals


@pytest.mark.parametrize("argv, ours", [
    pytest.param("/opt/ccmorph/libexec/ccmorph-core adapter oli --auto --parent-pid 500 --model m", True,
                 id="the-sidecar-start_adapter-launched"),
    pytest.param("/opt/ccmorph/libexec/ccmorph-core adapter oli --auto --parent-pid 777", False,
                 id="another-edges-adapter-issue-595"),
    pytest.param("/opt/ccmorph/libexec/ccmorph-core adapter different-profile --model oli --parent-pid 500", False,
                 id="profile-only-as-a-flag-value-issue-595"),
    pytest.param("/usr/bin/python3 /x/tool adapter oli --parent-pid 500 /y/ccmorph-core", False,
                 id="not-ccmorph-core"),
    pytest.param("/opt/ccmorph/libexec/ccmorph-core adapter oli --auto --parent-pid", False,
                 id="no-parent-value"),
    pytest.param("", False, id="gone"),
])
def test_a_sidecar_is_ours_only_by_its_own_operands(monkeypatch, argv, ours):
    """issue #595: ownership was any word anywhere in argv, so a stale ledger could
    name another edge's adapter, or another profile's, and the switch signalled it."""
    rc = _launcher()
    monkeypatch.setattr(rc.subprocess, "run", lambda *a, **k: types.SimpleNamespace(stdout=argv + "\n"))
    assert rc.our_adapter(900, "oli", 500) is ours


def test_a_stale_ledger_never_signals_another_edges_adapter(tmp_path, monkeypatch):
    rc = _launcher()
    ours = rc.our_adapter
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    monkeypatch.setattr(rc, "our_adapter", ours)        # the real check, over a faked process table
    (state / "sidecar-oli.json").write_text(json.dumps(
        {"pid": 900, "upstream": "http://127.0.0.1:45110", "edge": 500}))
    monkeypatch.setattr(rc.subprocess, "run", lambda *a, **k: types.SimpleNamespace(
        stdout="/opt/ccmorph/libexec/ccmorph-core adapter oli --auto --parent-pid 777\n"))
    rc.switch("oli", "alex")
    assert signals == []
    assert len(started) == 1


def test_overlapping_switches_start_one_sidecar(tmp_path, monkeypatch):
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    monkeypatch.setattr(rc, "our_adapter", lambda pid, profile, edge: any(s[3] == pid for s in started))
    monkeypatch.setattr(rc, "_listening", lambda upstream: True)
    rigged = rc.start_adapter
    def slow(route, parent, env):
        time.sleep(0.3)
        return rigged(route, parent, env)
    monkeypatch.setattr(rc, "start_adapter", slow)
    got = []
    threads = [threading.Thread(target=lambda: got.append(rc.sidecar("oli", 500, "start-500"))) for _ in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(HANG_TIMEOUT)
    assert len(started) == 1
    assert got[0] == got[1] == (started[0][2], started[0][3])
    assert json.loads((state / "sidecar-oli.json").read_text())["pid"] == started[0][3]


def test_a_sidecar_whose_record_cannot_be_written_is_stopped(tmp_path, monkeypatch):
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    (state / "sidecar-oli.tmp").mkdir()
    with pytest.raises(SystemExit):
        rc.sidecar("oli", 500, "start-500")
    assert len(started) == 1
    assert signals == ["SIGTERM"]
    assert not (state / "sidecar-oli.json").exists()


ADDON = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "rc_addon.py"


def _proxy_addon(monkeypatch, tmp_path, parent):
    fake = types.ModuleType("mitmproxy")
    fake.http = types.ModuleType("mitmproxy.http")
    monkeypatch.setitem(sys.modules, "mitmproxy", fake)
    monkeypatch.setitem(sys.modules, "mitmproxy.http", fake.http)
    bridge = tmp_path / "bridge"
    bridge.mkdir(exist_ok=True)
    key = tmp_path / f"route-key.{len(list(tmp_path.glob('route-key.*')))}"
    key.write_text("sk-route")
    monkeypatch.setenv("RC_UPSTREAM", "http://127.0.0.1:9")
    monkeypatch.setenv("RC_KEY_FILE", str(key))
    monkeypatch.setenv("RC_INFERENCE", str(bridge / "inference"))
    monkeypatch.setenv("RC_PARENT_PID", str(parent))
    monkeypatch.delenv("RC_LOG", raising=False)
    return load_script(ADDON, "rc_addon_under_test"), bridge


def test_a_restarted_proxy_finds_the_switched_endpoint_again(tmp_path, monkeypatch):
    first, bridge = _proxy_addon(monkeypatch, tmp_path, os.getpid())
    (bridge / "switch-oli.json").write_text(json.dumps(
        {"upstream": "http://127.0.0.1:45111", "key": "sk-oli", "tiers": {}, "edge": os.getpid()}))
    assert first._switched("oli") == ("http://127.0.0.1:45111", "sk-oli", {})
    second, _ = _proxy_addon(monkeypatch, tmp_path, os.getpid())
    assert second._switched("oli") == ("http://127.0.0.1:45111", "sk-oli", {})
    assert (bridge / "switch-oli.json").exists()


def test_a_handoff_goes_with_the_edge_it_serves(tmp_path, monkeypatch):
    gone = subprocess.Popen(["true"])
    gone.wait()
    mod, bridge = _proxy_addon(monkeypatch, tmp_path, gone.pid)
    for name, edge in (("oli", gone.pid), ("kimi", os.getpid())):
        (bridge / f"switch-{name}.json").write_text(json.dumps({"upstream": "u", "key": "k", "edge": edge}))
    mod.done()
    assert not (bridge / "switch-oli.json").exists()
    assert (bridge / "switch-kimi.json").exists()
    live, _ = _proxy_addon(monkeypatch, tmp_path, os.getpid())
    (bridge / "switch-oli.json").write_text(json.dumps(
        {"upstream": "u", "key": "k", "edge": os.getpid(), "edge_started": live.proc_started(os.getpid())}))
    live.done()
    assert (bridge / "switch-oli.json").exists()


@pytest.mark.parametrize("recorded, now, kept", [
    pytest.param("start-now", "start-now", True, id="proxy-restart-behind-the-same-edge-keeps-it"),
    pytest.param("start-old", "start-now", False, id="recycled-edge-pid-drops-it-issue-595"),
    pytest.param(None, "start-now", False, id="no-recorded-edge-start-drops-it-issue-595"),
    pytest.param("start-old", None, True, id="live-edge-whose-start-cannot-be-read-keeps-it"),
])
def test_a_proxy_forgets_a_handoff_once_its_edge_process_is_gone(tmp_path, monkeypatch, recorded, now, kept):
    """issue #595 review: the proxy forgot its edge's handoffs only once the edge's
    pid stopped answering, so a pid the OS handed to another process kept the old
    endpoint and key on disk. The start the handoff records decides."""
    mod, bridge = _proxy_addon(monkeypatch, tmp_path, os.getpid())
    monkeypatch.setattr(mod, "proc_started", lambda pid: now)
    hand = {"upstream": "u", "key": "k", "edge": os.getpid()}
    if recorded is not None:
        hand["edge_started"] = recorded
    (bridge / "switch-oli.json").write_text(json.dumps(hand))
    mod.done()
    assert (bridge / "switch-oli.json").exists() is kept
