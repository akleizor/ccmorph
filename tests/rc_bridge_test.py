"""Remote Control bridge add-on: routing decision and credential isolation."""
import contextlib
import json
import socket
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "addons" / "remote-control"))
from rc_route import is_inference, reroute  # noqa: E402

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
    import importlib.machinery
    import importlib.util
    path = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc"
    loader = importlib.machinery.SourceFileLoader("ccmorph_rc", str(path))
    spec = importlib.util.spec_from_loader("ccmorph_rc", loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


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
    seen = {}
    class Stop(Exception):
        pass
    def popen(cmd, **kw):
        seen["cmd"] = cmd
        raise Stop
    monkeypatch.setattr(rc.subprocess, "Popen", popen)
    try:
        rc.start_adapter("gw", 1, {})
    except Stop:
        pass
    assert seen["cmd"][-2:] == ["--model", "claude-opus-5-5"]


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
    # every later host without it.
    rc = _launcher()
    src = Path(rc.__file__).read_text() if hasattr(rc, "__file__") else ""
    assert 'for k in (*MODEL_ENV, *ROUTE_ENV)' in src


def test_the_edge_record_names_the_tiers_its_proxy_pins():
    """The record is the only thing a later supervisor can compare its config to:
    without the tiers in it, an edge adopted after a restart keeps the pins it was
    started with for as long as it lives (issue #356)."""
    rc = _launcher()
    record = rc.edge_record(1, 47100, "/ca.pem", "http://u", {},
                            {"ANTHROPIC_DEFAULT_FABLE_MODEL": "new-model[1m]", "PATH": "/bin"})
    assert record["tiers"] == {"fable": "new-model"}
    assert (record["pid"], record["port"], record["ca"]) == (1, 47100, "/ca.pem")


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
        p.read_text() for p in [root / "lib/ccmorph.sh", *sorted((root / "lib").glob("ccmorph-*.zsh"))])
    # A value the caller set is never overruled.
    own = rc.session_env({"CLAUDE_CODE_MODEL_CAPABILITIES": "x=-y"}, {}, route_models=False)
    assert own["CLAUDE_CODE_MODEL_CAPABILITIES"] == "x=-y"
    # The advisor is withheld the same way, and a route that serves it can say so.
    assert env["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] == "1"
    keep = rc.session_env({"PATH": "/bin"}, {"CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "0"}, route_models=False)
    assert keep["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] == "0"


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
