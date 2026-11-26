"""Remote Control bridge add-on: routing decision and credential isolation."""
import contextlib
import json
import os
import re
import signal
import socket
import subprocess
import sys
import threading
import time
import types
from pathlib import Path

import pytest

from support import HANG_TIMEOUT, load_script, proxy_took, wait_until

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "addons" / "remote-control"))
from rc_route import ACCOUNT_METADATA, inference_target, is_inference, reroute  # noqa: E402

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


def _edge_record_at(rc, name, tmp_path):
    ca = tmp_path / "ca.pem"
    ca.write_text("pem")
    (tmp_path / f"{name}.json").write_text(json.dumps({"pid": 4242, "port": 1, "ca": str(ca)}))


def test_an_edge_state_name_keeps_a_profile_name_and_cleans_every_other_route(tmp_path, monkeypatch):
    """A folder host's log name is cleaned of path characters before it is joined onto
    LOGS; the edge's state names were built from the raw `route=` (issue #846):
    `route=gw/eu` sent the record and its temp write into a subdirectory that is
    never created, and live_edge read `EDGES/gw/eu.json`, so a bridge of such a
    route could never find the edge that was up. The rule: a valid profile name
    -- the only route `ccmorph _route_env` accepts (cli/main.go routeEnv ->
    profile.ValidName, [A-Za-z0-9_]{1,200}) -- keeps its name WHOLE, because a
    rule that renames `gw_eu` moves the name of a record already on disk,
    blinding a restarted supervisor to its live edge (REVIEW.md; the gate
    refused the first fix for exactly this). EVERY other route is cleaned and
    carries a sha256 digest: cleaning alone merges `gw/eu`, `gw-eu` and `gw.eu`
    onto one name, and CRC32 was refused too because two names can be steered
    onto one checksum. Every emitted name is short ASCII, so two names are one
    file on a case- or normalization-insensitive volume exactly when they are
    equal after .lower(): letter case, Unicode normalisation and NAME_MAX are
    closed by construction (review round 4)."""
    rc = _launcher()
    monkeypatch.setattr(rc, "EDGES", tmp_path)
    whole = ["gw", "gw_eu", "GW", "a" * 200]                 # rows on disk keep their names
    assert [rc.edge_name(r) for r in whole] == whole
    cleaned = ["gw/eu", "GW/EU", "gw-eu", "gw.eu", "gw eu", "a" * 201,
               "é", "é", ".", "..", "", "\x00", "///", "\udc80"]
    names = [rc.edge_name(r) for r in cleaned]
    for route, n in zip(cleaned, names):
        assert re.fullmatch(r"[A-Za-z0-9_-]+", n), (route, n)
        assert "-" in n and len(n) <= 200, (route, n)          # cleaned, and NAME_MAX holds
        assert len(f"edge-{n}.log".encode()) <= 255 and len(f".{n}.json.tmp".encode()) <= 255
    lowered = [n.lower() for n in names] + [r.lower() for r in ("gw", "gw_eu", "a" * 200)]
    assert len(lowered) == len(set(lowered)), sorted(lowered)  # one file only for one route
    assert rc.edge_name("gw").lower() == rc.edge_name("GW").lower()   # a case twin is refused
    alias = rc.edge_name("gw/eu")                          # a route spelled as a cleaned name
    assert re.fullmatch(r"gw-eu-[0-9a-f]{32}", alias)      # cleaned stem + a lowercase sha256 prefix
    assert rc.edge_name(alias) != alias                    # gets its own name, never the same one
    monkeypatch.setattr(rc.os, "kill", lambda pid, sig: None)     # the pid is fake: liveness is not under test
    monkeypatch.setattr(rc.socket, "create_connection", lambda addr, timeout=None: contextlib.nullcontext())
    _edge_record_at(rc, alias, tmp_path)
    assert rc.live_edge("gw/eu") is not None                 # live_edge reads the cleaned name
    assert not (tmp_path / "gw").exists()


def test_an_edge_record_written_under_the_raw_route_is_still_found(tmp_path, monkeypatch):
    """A naming rule may not change the names of rows already on disk (REVIEW.md).
    Before #846 an edge of route `gw_eu` wrote `gw_eu.json` just fine -- only a
    token holding a `/` ever crashed -- and a supervisor restarted while that
    edge stayed live must still find its record: a reader on a renamed path
    would start a SECOND edge against the same fixed port."""
    rc = _launcher()
    monkeypatch.setattr(rc, "EDGES", tmp_path)
    _edge_record_at(rc, "gw_eu", tmp_path)                   # the pre-change name, untouched
    monkeypatch.setattr(rc.os, "kill", lambda pid, sig: None)
    monkeypatch.setattr(rc.socket, "create_connection", lambda addr, timeout=None: contextlib.nullcontext())
    assert rc.live_edge("gw_eu") is not None


def test_a_published_edge_record_sits_under_its_edge_name_and_names_its_route(tmp_path, monkeypatch):
    """The record's path and its own `route` are one writer's decision (#846): a
    supervisor reads EDGES/<edge_name(route)>.json and must be able to tell
    whose edge it found there. publish_edge is that writer -- EDGES may not
    exist yet (a fresh state dir's first edge), and no temp file or
    subdirectory may be left behind."""
    rc = _launcher()
    monkeypatch.setattr(rc, "EDGES", tmp_path / "edges")     # never created before the first edge
    monkeypatch.setattr(rc.os, "kill", lambda pid, sig: None)
    monkeypatch.setattr(rc.socket, "create_connection", lambda addr, timeout=None: contextlib.nullcontext())
    ca = tmp_path / "ca.pem"
    ca.write_text("pem")
    routes = ["gw/eu", "gw_eu", "GW"]
    for route in routes:
        path = rc.publish_edge(route, {"pid": 4242, "port": 1, "ca": str(ca)})
        assert path == rc.EDGES / f"{rc.edge_name(route)}.json"
        assert json.loads(path.read_text())["route"] == route
        assert rc.live_edge(route)["route"] == route
    assert sorted(p.name for p in rc.EDGES.iterdir()) == sorted(
        f"{rc.edge_name(r)}.json" for r in routes)            # three records: no temp, no subdirectory


def test_an_edge_record_naming_another_route_is_not_adopted(tmp_path, monkeypatch):
    """On a case-insensitive volume `GW.json` and `gw.json` are one file, so a
    reader that never asked whose record it found would adopt the twin's edge
    at the twin's port (#846 review round 4). The record now names the route
    that wrote it; one naming another is not this route's edge. A record from
    before this change names none and reads as before; a JSON body that is no
    record at all is no edge, not a crash."""
    rc = _launcher()
    monkeypatch.setattr(rc, "EDGES", tmp_path)
    monkeypatch.setattr(rc.os, "kill", lambda pid, sig: None)
    monkeypatch.setattr(rc.socket, "create_connection", lambda addr, timeout=None: contextlib.nullcontext())
    ca = tmp_path / "ca.pem"
    ca.write_text("pem")
    body = {"pid": 4242, "port": 1, "ca": str(ca)}
    (tmp_path / "GW.json").write_text(json.dumps({**body, "route": "gw"}))
    assert rc.live_edge("GW") is None
    (tmp_path / "GW.json").write_text(json.dumps({**body, "route": "GW"}))
    assert rc.live_edge("GW") is not None
    for not_a_record in ([], "x"):
        (tmp_path / "GW.json").write_text(json.dumps(not_a_record))
        assert rc.live_edge("GW") is None


def test_a_route_less_edge_record_is_read_only_under_its_own_spelling(tmp_path, monkeypatch):
    """A record written before #846 names no route, and only its file's stored
    spelling says whose it is (#846 review round 6, the gate's refusal of
    d7a6ed0): on macOS's case-insensitive volume opening GW.json succeeds on
    gw's old gw.json, so a GW host adopted a gw edge at a gw port. A route-less
    record is this route's only when EDGES lists an entry spelled exactly
    `<edge_name(route)>.json` -- the rule cli/doctor.go:346 already applies to
    profiles (GH-259: "only a directory entry spelled as typed is this
    profile"). The record here is fixed as data on disk, never the writer's
    current spelling, so a rename cannot prove only that writer and test moved
    together (REVIEW.md). A record that names its route stays decided by that
    field alone: on a case-folding volume a rename may keep a twin's old
    spelling, and a spelling check there could hide the route's own edge and
    start a second one against its port."""
    rc = _launcher()
    monkeypatch.setattr(rc, "EDGES", tmp_path)
    monkeypatch.setattr(rc.os, "kill", lambda pid, sig: None)
    monkeypatch.setattr(rc.socket, "create_connection", lambda addr, timeout=None: contextlib.nullcontext())
    ca = tmp_path / "ca.pem"
    ca.write_text("pem")
    body = {"pid": 4242, "port": 1, "ca": str(ca)}
    real = rc.os.listdir

    def stored(name):        # what a case-folding volume lists: the spelling the writer used
        monkeypatch.setattr(rc.os, "listdir",
                            lambda p=".": [name, "ca.pem"] if Path(p) == tmp_path else real(p))

    (tmp_path / "GW.json").write_text(json.dumps(body))      # opening GW.json succeeds, as on macOS
    stored("gw.json")
    assert rc.live_edge("GW") is None                         # gw's pre-#846 record: not GW's
    stored("GW.json")
    assert rc.live_edge("GW") == body                         # its own pre-#846 record: found
    (tmp_path / "GW.json").write_text(json.dumps({**body, "route": "GW"}))
    stored("gw.json")
    assert rc.live_edge("GW")["route"] == "GW"                # a record naming its route decides
    (tmp_path / "GW.json").write_text(json.dumps({**body, "route": "gw"}))
    stored("GW.json")
    assert rc.live_edge("GW") is None                         # another route's record, unchanged
    monkeypatch.setattr(rc, "EDGES", tmp_path / "missing")
    assert rc.live_edge("GW") is None                         # no edges dir: no edge, no raise


def test_a_case_twin_never_adopts_a_route_less_record_on_a_case_insensitive_volume(tmp_path, monkeypatch):
    """The gate's round-6 repro on the real volume it was written for: gw's
    pre-#846 record, read as GW. On a case-sensitive volume GW.json simply does
    not exist and the test skips; the behaviour is pinned everywhere by
    test_a_route_less_edge_record_is_read_only_under_its_own_spelling."""
    rc = _launcher()
    monkeypatch.setattr(rc, "EDGES", tmp_path)
    monkeypatch.setattr(rc.os, "kill", lambda pid, sig: None)
    monkeypatch.setattr(rc.socket, "create_connection", lambda addr, timeout=None: contextlib.nullcontext())
    ca = tmp_path / "ca.pem"
    ca.write_text("pem")
    (tmp_path / "gw.json").write_text(json.dumps({"pid": 4242, "port": 47100, "ca": str(ca)}))
    if not (tmp_path / "GW.json").exists():
        pytest.skip("needs a case-insensitive volume (macOS's default APFS)")
    assert rc.live_edge("GW") is None          # gw's record, not GW's
    assert rc.live_edge("gw") is not None      # the route that wrote it still finds it


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


def test_a_route_pinned_tier_is_served_as_the_targets_own_model():
    """A session's env pins the route's fable (gpt-6-astra) on the bare alias. Under
    another target that id is read back to fable and served as the target's fable;
    the account (family=False) changes nothing else the phone picked."""
    from rc_route import normalize_body, set_model
    route = {"fable": "gpt-6-astra", "opus": "claude-opus-5-5"}
    gpro = {"fable": "claude-opus-4-6-thinking", "opus": "claude-opus-4-6-thinking"}
    body = {"model": "gpt-6-astra"}
    assert normalize_body(body, gpro, route) and body["model"] == "claude-opus-4-6-thinking"
    body = {"model": "gpt-6-astra"}
    assert set_model(body, {"fable": "claude-fable-5-2"}, route, family=False)
    assert body["model"] == "claude-fable-5-2"
    body = {"model": "claude-sonnet-5-5"}
    assert not set_model(body, {"sonnet": "x"}, route, family=False)
    body = {"model": "gpt-6-astra"}
    assert not normalize_body(body, route, route)           # on the route itself nothing moves


def test_a_session_word_is_read_only_from_a_plain_id(tmp_path):
    from rc_route import session_target
    (tmp_path / "cse_1").write_text("gw\n")
    (tmp_path / "bad").write_text("not a word!\n")
    assert session_target(str(tmp_path), "cse_1") == "gw"
    assert session_target(str(tmp_path), "bad") is None
    assert session_target(str(tmp_path), "missing") is None
    assert session_target(str(tmp_path), "../" + tmp_path.name + "/cse_1") is None
    assert session_target(None, "cse_1") is None


def test_a_session_word_is_found_by_the_header_claude_code_really_sends(tmp_path):
    """Claude Code sends a UUIDv5 of the session's --sdk-url, never the cse_ id (2026-11-25)."""
    from rc_route import header_id, session_target
    # a real pair: the Start UI host's --sdk-url id and the transcript id its requests carried
    assert header_id("cse_01UYCe7WrYRm399JWcJrn7Rd") == "c3ca46e6-52e5-57cc-a1c3-6a9e88650085"
    (tmp_path / "cse_01UYCe7WrYRm399JWcJrn7Rd").write_text("account\n")
    (tmp_path / "cse_other").write_text("gw\n")
    assert session_target(str(tmp_path), "c3ca46e6-52e5-57cc-a1c3-6a9e88650085") == "account"
    assert session_target(str(tmp_path), header_id("cse_other")) == "gw"
    assert session_target(str(tmp_path), header_id("cse_absent")) is None


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
    for name in ("RC_TIERS", "RC_LOG", "RC_SOURCE"):
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
    assert (flow.metadata.get(ACCOUNT_METADATA) is True) == first_party
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
    assert f"!~meta {ACCOUNT_METADATA}" in _launcher().FAIL_CLOSED


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


def _switch_state(tmp_path, monkeypatch):
    """The launcher set up for `switch()` to bring the route profile "gw" up beside the
    one live shared edge: edge liveness, the route resolve and the adapter spawn are the
    outside world here, so each answers exactly what a healthy host would. A conf that
    pins sonnet through the window marker is the #584 shape: the marker is ccmorph's own
    and belongs to no model id the proxy may pin."""
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(tmp_path))
    rc = _launcher()
    (tmp_path / "gw.conf").write_text("type=apikey\n")
    (rc.STATE / "edges").mkdir(parents=True)
    (rc.STATE / "edges" / "main.json").write_text("{}")     # the one shared edge switch() demands
    monkeypatch.setattr(rc, "live_edge", lambda r: {"pid": 4242} if r == "main" else None)
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: f"start-{pid}")   # the edge's start (#595)
    route_env = {"ANTHROPIC_BASE_URL": "https://gw", "ANTHROPIC_API_KEY": "route-key",
                 "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5-5[1m]",
                 "CCMORPH_ROUTE_HANDLE": "provider|1|/t|/a"}
    monkeypatch.setattr(rc, "resolve_route", lambda profile: route_env)
    monkeypatch.setattr(rc, "retire_route_with", lambda pid, handle: None)
    monkeypatch.setattr(rc, "adapter_env", lambda: {})
    monkeypatch.setattr(rc, "start_adapter", lambda profile, owner, env: "http://127.0.0.1:54505")
    monkeypatch.setattr(rc, "get_tiers", lambda profile: {"sonnet": "claude-sonnet-5-5"})
    monkeypatch.setattr(rc, "handoff_taken", proxy_took(rc))   # the edge's proxy took it
    return rc, route_env


# The edge's own route map, as ccmorph-rc hands it to the proxy at start (RC_TIERS). It differs
# from every handoff below, so a test can tell which of the two maps a turn used.
_BOOT_TIERS = {"sonnet": "boot-route-sonnet"}


def _switch_addon(monkeypatch, state):
    """rc_addon loaded the way ccmorph-rc loads it: a private copy configured through
    the environment alone. mitmproxy only ever appears in the hook signatures, and the
    suite runs without it, so where it is not installed the name resolves to a stub;
    neither the handoff read nor the routing decision reaches past it."""
    from types import ModuleType, SimpleNamespace
    key = state / "route-key"
    key.parent.mkdir(parents=True, exist_ok=True)
    key.write_text("route-key\n")
    monkeypatch.setenv("RC_UPSTREAM", "http://127.0.0.1:60111")
    monkeypatch.setenv("RC_KEY_FILE", str(key))
    monkeypatch.setenv("RC_INFERENCE", str(state / "inference"))
    monkeypatch.setenv("RC_LOG", str(state / "addon.log"))
    # Every RC_* the add-on reads at import is pinned here, never inherited from the
    # suite's environment (REVIEW.md: a test that cannot fail).
    monkeypatch.setenv("RC_TIERS", json.dumps(_BOOT_TIERS))
    monkeypatch.delenv("RC_PARENT_PID", raising=False)
    monkeypatch.delenv("RC_PARENT_STARTED", raising=False)
    monkeypatch.delenv("RC_SOURCE", raising=False)
    try:
        import mitmproxy  # noqa: F401
    except ImportError:
        mitm = ModuleType("mitmproxy")
        mitm.http = SimpleNamespace(HTTPFlow=object)
        monkeypatch.setitem(sys.modules, "mitmproxy", mitm)
    path = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "rc_addon.py"
    return load_script(path, "rc_addon_switch")


def test_a_switch_handoff_pins_its_tiers_through_pinned_tiers(tmp_path, monkeypatch):
    """issue #584: switch() spelled the tier map inline — pinned_tiers() minus that
    function's [1m] strip — so a profile whose conf pinned claude-…[1m] carried the
    marker verbatim into switch-<profile>.json, and the switched phone's bare `sonnet`
    came back rewritten to a model id wearing ccmorph's own window marker."""
    rc, route_env = _switch_state(tmp_path, monkeypatch)
    rc.switch("gw", "me")
    hand = rc.STATE / "switch-gw.json"
    assert json.loads(hand.read_text())["tiers"] == rc.pinned_tiers(route_env) == {"sonnet": "claude-sonnet-5-5"}
    assert hand.stat().st_mode & 0o777 == 0o600
    assert "[1m]" not in hand.read_text()
    assert (rc.STATE / "inference").read_text() == "gw\n"   # the word every host follows next turn


def test_a_session_switch_moves_that_session_only(tmp_path, monkeypatch):
    """--switch --session writes the word for one phone session; every other host
    keeps the switch they share, and --unswitch hands the session back."""
    rc, _ = _switch_state(tmp_path, monkeypatch)
    rc.switch("gw", "me", "cse_01ABC")
    assert (rc.STATE / "sessions" / "cse_01ABC").read_text() == "gw\n"
    assert not (rc.STATE / "inference").exists()            # the shared switch is untouched
    rc.unswitch("cse_01ABC")
    assert not (rc.STATE / "sessions" / "cse_01ABC").exists()


@pytest.mark.parametrize("sid", ["../inference", "a/b", "", "-x"])
def test_a_session_switch_refuses_an_id_that_is_a_path(tmp_path, monkeypatch, sid):
    rc, _ = _switch_state(tmp_path, monkeypatch)
    with pytest.raises(SystemExit):
        rc.switch("gw", "me", sid)
    with pytest.raises(SystemExit):
        rc.unswitch(sid)
    assert not (rc.STATE / "inference").exists()


def test_a_profile_already_up_is_shared_not_brought_up_again(tmp_path, monkeypatch):
    """Each bring-up starts an adapter that lives as long as the edge: a second
    session on the same profile reuses the first while it still answers, and only
    a dead upstream or changed tiers bring it up again."""
    rc, _ = _switch_state(tmp_path, monkeypatch)
    started = []
    with socket.socket() as srv:
        srv.bind(("127.0.0.1", 0))
        srv.listen()
        url = f"http://127.0.0.1:{srv.getsockname()[1]}"
        monkeypatch.setattr(rc, "start_adapter", lambda p, o, e: started.append(p) or url)
        rc.switch("gw", "me", "cse_one")
        rc.switch("gw", "me", "cse_two")
        assert started == ["gw"]
        assert (rc.STATE / "sessions" / "cse_two").read_text() == "gw\n"
        monkeypatch.setattr(rc, "get_tiers", lambda profile: {"sonnet": "other"})
        rc.switch("gw", "me", "cse_two")                     # tiers changed in Stark
        assert started == ["gw", "gw"]
    rc.switch("gw", "me", "cse_three")                       # its adapter is gone
    assert started == ["gw", "gw", "gw"]


def test_an_account_switch_records_the_accounts_own_tiers(tmp_path, monkeypatch):
    rc, _ = _switch_state(tmp_path, monkeypatch)
    monkeypatch.setattr(rc, "get_tiers", lambda profile: {"fable": "claude-fable-5-2"} if profile == "me" else None)
    rc.switch("me", "me", "cse_01ABC")
    assert (rc.STATE / "sessions" / "cse_01ABC").read_text() == "account\n"
    assert json.loads((rc.STATE / "account-tiers.json").read_text()) == {"fable": "claude-fable-5-2"}


def test_a_session_word_overrides_the_shared_switch_for_its_own_requests(tmp_path, monkeypatch):
    """The proxy reads X-Claude-Code-Session-Id on every turn: that session's word
    wins, a session with none (or another) follows the shared switch."""
    from types import SimpleNamespace
    state = tmp_path / "state"
    (state / "sessions").mkdir(parents=True)
    (state / "inference").write_text("route\n")
    (state / "sessions" / "cse_mine").write_text("account\n")
    addon = _switch_addon(monkeypatch, state)

    def flow(sid):
        f = SimpleNamespace(metadata={}, request=SimpleNamespace(
            pretty_host="api.anthropic.com", path="/v1/messages", method="POST",
            headers={"authorization": "Bearer ACCOUNT-SECRET", "x-claude-code-session-id": sid},
            scheme="https", host="api.anthropic.com", port=443))
        addon.requestheaders(f)
        return f
    assert flow("cse_mine").metadata.get(ACCOUNT_METADATA) is True
    assert flow("cse_other").metadata["rc_route"] is True
    assert flow("../inference").metadata["rc_route"] is True


def test_the_addon_reads_a_handoff_once_and_keeps_it_thereafter(tmp_path, monkeypatch):
    """The handoff is the whole of a bring-up: read once, deleted with the read, then
    remembered for every later turn — and its tiers are exactly what pinned_tiers says."""
    rc, route_env = _switch_state(tmp_path, monkeypatch)
    rc.switch("gw", "me")
    addon = _switch_addon(monkeypatch, rc.STATE)
    got = addon._switched("gw")
    assert got == ("http://127.0.0.1:54505", "route-key", rc.pinned_tiers(route_env))
    assert not (rc.STATE / "switch-gw.json").exists()      # the key lived one hop, never on disk
    assert addon._switched("gw") == got                    # brought up once; every turn follows


def test_a_switch_whose_word_never_lands_leaves_no_handoff_holding_the_key(tmp_path, monkeypatch):
    """A handoff is the route key in a 0600 file until the next request reads it. A
    switch whose word never lands leaves that file for good — no request would ever
    read it — so the failed switch takes its own handoff back. Same guarantee as the
    route-key file, on the --switch path (issue #777)."""
    rc, _ = _switch_state(tmp_path, monkeypatch)

    def refuse(word, path=rc.INFERENCE):
        raise OSError("inference is read-only")
    monkeypatch.setattr(rc, "write_inference", refuse)
    with pytest.raises(OSError):
        rc.switch("gw", "me")
    assert list(rc.STATE.glob("switch-*")) == []


def test_a_switch_whose_handoff_rename_fails_leaves_no_temp_holding_the_key(tmp_path, monkeypatch):
    """The handoff temp is the route key on disk from its first byte. A rename that
    fails leaves that temp as the only copy, and no request ever reads a .tmp, so the
    failed switch removes it and writes no word (README: no lingering file, #777)."""
    rc, _ = _switch_state(tmp_path, monkeypatch)
    real_replace = rc.os.replace
    staged = []

    def refuse(src, dst, *args, **kw):
        if Path(dst).name == "switch-gw.json":           # the handoff rename, not the word's
            staged.append(Path(src))
            raise OSError("rename refused")
        return real_replace(src, dst, *args, **kw)
    monkeypatch.setattr(rc.os, "replace", refuse)
    with pytest.raises(OSError, match="rename refused"):
        rc.switch("gw", "me")
    # The temp was this switch's own (a unique name, never a shared switch-gw.tmp
    # another switch could have staged), and it is gone with the failure.
    assert len(staged) == 1 and staged[0].name.startswith("switch-gw.json.")
    assert staged[0].name != "switch-gw.tmp"
    assert list(rc.STATE.glob("switch-*")) == []
    assert not (rc.STATE / "inference").exists()


@pytest.mark.parametrize("handoff,port,log", [
    pytest.param(None, 60111, "has no endpoint in this proxy", id="never-brought-up-keeps-the-route"),
    pytest.param({"upstream": "http://127.0.0.1:54505", "key": "route-key",
                  "tiers": {"sonnet": "claude-sonnet-5-5"}}, 54505, "via gw",
                 id="brought-up-moves-the-turn"),
])
def test_a_switch_word_moves_the_turn_only_where_a_profile_was_brought_up(tmp_path, monkeypatch,
                                                                          handoff, port, log):
    """A word file can name a profile `--switch` never raised; the turn then stays on
    the edge's route with a named reason — never guessed at, never failed."""
    from types import SimpleNamespace
    state = tmp_path / "state"
    state.mkdir()
    (state / "inference").write_text("gw\n")
    if handoff:
        (state / "switch-gw.json").write_text(json.dumps(handoff))
    addon = _switch_addon(monkeypatch, state)
    flow = SimpleNamespace(metadata={}, request=SimpleNamespace(
        pretty_host="api.anthropic.com", path="/v1/messages", method="POST",
        headers={"authorization": "Bearer ACCOUNT-SECRET"},
        scheme="https", host="api.anthropic.com", port=443))
    addon.requestheaders(flow)
    assert flow.request.port == port                        # the handoff's adapter, or the route
    assert log in (state / "addon.log").read_text()
    assert flow.metadata["rc_route"] is True
    assert flow.metadata["rc_tiers"] == (handoff["tiers"] if handoff else _BOOT_TIERS)


def _edge_spawn_state(tmp_path, monkeypatch):
    """The launcher set up so `own_edge` runs its start path with everything except
    the proxy spawn stubbed to what a healthy host gives it; the caller decides what
    the spawn does. The route key file is written on this path, so these tests read
    the README's "no lingering file" guarantee from `own_edge` itself."""
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(tmp_path))
    rc = _launcher()
    monkeypatch.setattr(rc, "resolve_route", lambda route: {
        "ANTHROPIC_API_KEY": "route-key", "ANTHROPIC_BASE_URL": "https://gw",
        "CCMORPH_ROUTE_HANDLE": ""})
    monkeypatch.setattr(rc, "retire_route_with", lambda pid, handle: None)
    monkeypatch.setattr(rc, "adapter_env", lambda: {})
    monkeypatch.setattr(rc, "start_adapter", lambda route, owner, env: "http://127.0.0.1:54505")
    monkeypatch.setattr(rc, "strip_env", lambda: ())     # ccmorph _backend_env wants the built shim
    return rc


class _SpawnedProxy:
    """The mitmdump the launcher watches at startup: dead at once, or alive."""

    def __init__(self, exit_code=None):
        self.returncode = exit_code

    def poll(self):
        return self.returncode

    def terminate(self):
        if self.returncode is None:
            self.returncode = -15

    def kill(self):
        self.terminate()

    def wait(self, timeout=None):
        return self.returncode


def _stub_proxy_spawn(rc, monkeypatch, exit_code):
    """Only the proxy spawn is stubbed: strip_env() still runs `ccmorph
    _backend_env` through subprocess.run, which shares the Popen entry."""
    real = rc.subprocess.Popen

    def spawn(cmd, **kw):
        if cmd[0] == "zsh":           # not the proxy: whatever else the launcher shells out for
            return real(cmd, **kw)
        return _SpawnedProxy(exit_code)
    monkeypatch.setattr(rc.subprocess, "Popen", spawn)


@pytest.mark.parametrize("shape,message", [
    pytest.param("no-mitmdump", "mitmdump not found", id="mitmdump-not-on-path"),
    pytest.param("proxy-exits", "proxy exited during startup", id="stub-mitmdump-dies-at-once"),
    pytest.param("no-ca", "proxy CA not generated", id="ca-never-generated"),
    pytest.param("key-unconsumed", "did not consume the route key", id="addon-never-loads"),
])
def test_no_failed_start_ever_leaves_the_route_key_behind(tmp_path, monkeypatch, capsys, shape, message):
    """issue #777: the key file's only unlink lived in the addon, so every start
    that died after `mkstemp` — missing mitmdump, dead proxy, missing CA, and the
    very path that prints "refusing to continue" — left the profile's real API key
    in STATE. This is the sweep: after any failing start, no route-key.* survives."""
    import argparse
    import os
    rc = _edge_spawn_state(tmp_path, monkeypatch)
    if shape == "no-mitmdump":
        monkeypatch.setenv("PATH", str(tmp_path / "no-bin"))         # PATH without mitmdump
    else:
        stub = tmp_path / "bin" / "mitmdump"                         # a mitmdump that never really runs
        stub.parent.mkdir()
        stub.write_text("#!/bin/sh\nexit 0\n")
        stub.chmod(0o755)
        monkeypatch.setenv("PATH", str(stub.parent) + os.pathsep + os.environ["PATH"])
        if shape == "proxy-exits":                                   # wait_listening stays real
            _stub_proxy_spawn(rc, monkeypatch, 1)
        else:                                                        # listens; addon is the question
            _stub_proxy_spawn(rc, monkeypatch, None)
            monkeypatch.setattr(rc, "wait_listening", lambda *a, **k: None)
            if shape == "key-unconsumed":
                ca = rc.STATE / "mitm" / "mitmproxy-ca-cert.pem"     # a proxy that did come up
                ca.parent.mkdir(parents=True, exist_ok=True)
                ca.write_text("stub ca")
    with pytest.raises(SystemExit):
        rc.own_edge("gw", 4242, argparse.Namespace(debug=False), None)
    assert message in capsys.readouterr().err
    assert list(rc.STATE.glob("route-key.*")) == []


def test_a_start_removes_what_a_killed_launcher_left_and_keeps_live_ones(tmp_path, monkeypatch):
    """A SIGKILL runs no cleanup, so its route key stayed on disk for good. The next
    start removes the files of every launcher that is gone, and only those."""
    import os
    import subprocess
    rc = _edge_spawn_state(tmp_path, monkeypatch)
    rc.STATE.mkdir(parents=True, exist_ok=True)
    gone = subprocess.Popen([sys.executable, "-c", ""])
    gone.wait()
    dead_key = rc.STATE / f"route-key.{gone.pid}.abc"
    dead_key.write_text("secret")
    dead_addon = rc.STATE / f"addon.{gone.pid}.abc"
    dead_addon.mkdir()
    (dead_addon / "rc_addon.py").write_text("")
    live = rc.STATE / f"{rc.owned_prefix('route-key')}abc"
    live.write_text("in use")
    rc.sweep_orphans(rc.STATE)
    assert not dead_key.exists() and not dead_addon.exists()
    assert live.exists()


@pytest.mark.parametrize("spawn_mode", ["child", "object"], ids=["spawned-child", "handle-with-no-stop-api"])
@pytest.mark.parametrize("sig,expected", [
    (signal.SIGTERM, str(128 + signal.SIGTERM)),
    (signal.SIGHUP, str(128 + signal.SIGHUP)),
    (signal.SIGQUIT, str(128 + signal.SIGQUIT)),
    (signal.SIGINT, "KeyboardInterrupt"),
], ids=["sigterm", "sighup", "sigquit", "sigint"])
def test_termination_during_proxy_startup_removes_the_route_key(tmp_path, sig, expected, spawn_mode):
    """A normal termination signal bypasses Python's finally unless startup
    temporarily handles it. A separate process exercises the real disposition.
    A real spawned proxy is stopped; a Popen handle with no stop API — the shape
    the issue's reproduction stubs — must not replace the signal's exit."""
    import os
    import subprocess

    state = tmp_path / "state"
    child = r"""import argparse, importlib.machinery, importlib.util, os, signal, sys
path, sig, expected, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
for s in (signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT):
    signal.signal(s, signal.SIG_DFL)          # a plain launch, whatever the test runner ignores
signal.signal(signal.SIGINT, signal.default_int_handler)
loader = importlib.machinery.SourceFileLoader("ccmorph_rc_signal", path)
spec = importlib.util.spec_from_loader(loader.name, loader)
rc = importlib.util.module_from_spec(spec)
loader.exec_module(rc)
rc.resolve_route = lambda route: {"ANTHROPIC_API_KEY": "test-only-key", "ANTHROPIC_BASE_URL": "https://example.invalid"}
rc.retire_route_with = lambda *a: None
rc.adapter_env = lambda: {}
rc.start_adapter = lambda *a: "http://127.0.0.1:1"
rc.strip_env = lambda: ()
rc.shutil.which = lambda name: "/test/" + name
real_popen = rc.subprocess.Popen
spawned = []
if mode == "object":
    rc.subprocess.Popen = lambda *a, **k: object()    # a handle with no terminate/wait at all
else:
    def spawn(cmd, **kw):
        spawned.append(real_popen([sys.executable, "-c", "import time; time.sleep(600)"],
                                  stdin=rc.subprocess.DEVNULL, start_new_session=True))
        return spawned[-1]
    rc.subprocess.Popen = spawn
rc.wait_listening = lambda *a, **k: os.kill(os.getpid(), sig)
try:
    rc.own_edge("test", os.getpid(), argparse.Namespace(debug=False), None)
except BaseException as stopped:
    keys = list(rc.STATE.glob("route-key.*"))
    alive = [p.pid for p in spawned if p.poll() is None]
    for p in spawned:
        if p.poll() is None:
            p.kill()
    code = str(stopped.code) if isinstance(stopped, SystemExit) else type(stopped).__name__
    stopped_ok = mode == "object" or (spawned and not alive)
    if code == expected and not keys and stopped_ok:
        raise SystemExit(0)
    print(f"unexpected exit={code} key_files={keys} live_proxies={alive}", file=sys.stderr)
    raise SystemExit(2)
raise SystemExit(3)
"""
    env = {**os.environ, "CCMORPH_STATE_DIR": str(state)}
    result = subprocess.run([sys.executable, "-c", child,
                             str(Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc"),
                             str(int(sig)), expected, spawn_mode],
                            env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, f"child exit={result.returncode}; stderr={result.stderr}"
    assert list(state.rglob("route-key.*")) == []


def test_an_ignored_hangup_stays_ignored_during_startup(tmp_path):
    """nohup hands the launcher SIGHUP ignored. Startup takes over only signals left
    at their default, so a hang-up in the startup window neither aborts the start
    nor turns the ignored signal back on afterwards."""
    import os
    import subprocess

    state = tmp_path / "state"
    child = r"""import argparse, importlib.machinery, importlib.util, os, signal, sys
path = sys.argv[1]
signal.signal(signal.SIGHUP, signal.SIG_IGN)          # what nohup gives the launcher
loader = importlib.machinery.SourceFileLoader("ccmorph_rc_nohup", path)
spec = importlib.util.spec_from_loader(loader.name, loader)
rc = importlib.util.module_from_spec(spec)
loader.exec_module(rc)
rc.resolve_route = lambda route: {"ANTHROPIC_API_KEY": "test-only-key", "ANTHROPIC_BASE_URL": "https://example.invalid"}
rc.retire_route_with = lambda *a: None
rc.adapter_env = lambda: {}
rc.start_adapter = lambda *a: "http://127.0.0.1:1"
rc.strip_env = lambda: ()
rc.shutil.which = lambda name: "/test/" + name
class Proxy:
    returncode = None
    def poll(self): return self.returncode
    def terminate(self): self.returncode = -15
    def kill(self): self.returncode = -9
    def wait(self, timeout=None): return self.returncode
proxy = Proxy()
def spawn(cmd, env=None, **kw):
    os.unlink(env["RC_KEY_FILE"])                     # the addon loaded and consumed the key
    (rc.STATE / "mitm" / "mitmproxy-ca-cert.pem").write_text("ca")
    return proxy
rc.subprocess.Popen = spawn
rc.wait_listening = lambda *a, **k: os.kill(os.getpid(), signal.SIGHUP)
rc.own_edge("test", os.getpid(), argparse.Namespace(debug=False), None)
keys = list(rc.STATE.glob("route-key.*"))
ignored = signal.getsignal(signal.SIGHUP) == signal.SIG_IGN
if ignored and not keys and proxy.returncode is None:
    raise SystemExit(0)
print(f"ignored_after={ignored} key_files={keys} proxy_rc={proxy.returncode}", file=sys.stderr)
raise SystemExit(2)
"""
    env = {**os.environ, "CCMORPH_STATE_DIR": str(state)}
    result = subprocess.run([sys.executable, "-c", child,
                             str(Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc")],
                            env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, f"child exit={result.returncode}; stderr={result.stderr}"
    assert list(state.rglob("route-key.*")) == []


@pytest.mark.parametrize("shape,message", [
    pytest.param("never-listens", "did not start listening", id="proxy-never-listens"),
    pytest.param("no-ca", "proxy CA not generated", id="ca-never-generated"),
    pytest.param("key-unconsumed", "did not consume the route key", id="addon-never-loads"),
    pytest.param("healthy", None, id="healthy-start-keeps-its-proxy"),
])
def test_a_failed_start_stops_the_proxy_it_spawned(tmp_path, monkeypatch, capsys, shape, message):
    """issue #777: a failed start removes the route key, and the addon reads that key
    on load and is what follows our pid. A proxy left behind would never load it, so
    it would outlive us holding its port (on --edge, the fixed one every restart
    needs). The start that gives up stops the proxy it spawned; a healthy one keeps it."""
    import argparse
    import os
    import subprocess

    rc = _edge_spawn_state(tmp_path, monkeypatch)
    stub = tmp_path / "bin" / "mitmdump"
    stub.parent.mkdir()
    stub.write_text("#!/bin/sh\nexit 0\n")
    stub.chmod(0o755)
    monkeypatch.setenv("PATH", str(stub.parent) + os.pathsep + os.environ["PATH"])
    real = rc.subprocess.Popen
    spawned = []

    def spawn(cmd, env=None, **kw):
        if cmd[0] == "zsh":
            return real(cmd, env=env, **kw)
        if shape == "healthy":
            os.unlink(env["RC_KEY_FILE"])                            # the addon consumed it
        spawned.append(real([sys.executable, "-c", "import time; time.sleep(600)"],
                            stdin=subprocess.DEVNULL, start_new_session=True))
        return spawned[-1]
    monkeypatch.setattr(rc.subprocess, "Popen", spawn)
    if shape == "never-listens":
        monkeypatch.setattr(rc, "wait_listening",
                            lambda *a, **k: rc.die("proxy did not start listening within 20s"))
    else:
        monkeypatch.setattr(rc, "wait_listening", lambda *a, **k: None)
    if shape in ("key-unconsumed", "healthy"):
        ca = rc.STATE / "mitm" / "mitmproxy-ca-cert.pem"
        ca.parent.mkdir(parents=True, exist_ok=True)
        ca.write_text("stub ca")
    try:
        if shape == "healthy":
            rc.own_edge("gw", 4242, argparse.Namespace(debug=False), None)
            assert spawned and spawned[0].poll() is None, "a healthy start stopped its own proxy"
        else:
            with pytest.raises(SystemExit):
                rc.own_edge("gw", 4242, argparse.Namespace(debug=False), None)
            assert message in capsys.readouterr().err
            assert spawned and spawned[0].poll() is not None, "the failed start left its proxy running"
        assert list(rc.STATE.glob("route-key.*")) == []
    finally:
        for proc in spawned:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=HANG_TIMEOUT)


_SWITCH_SIGNAL_CHILD = r"""import importlib.machinery, importlib.util, os, resource, signal, sys
path, sig, point, lands = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4] == "1"
resource.setrlimit(resource.RLIMIT_CORE, (0, 0))      # SIGQUIT's default action dumps core
for s in (signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT):
    signal.signal(s, signal.SIG_DFL)                  # a plain launch, whatever the test runner ignores
signal.signal(signal.SIGINT, signal.default_int_handler)
loader = importlib.machinery.SourceFileLoader("ccmorph_rc_switch_signal", path)
spec = importlib.util.spec_from_loader(loader.name, loader)
rc = importlib.util.module_from_spec(spec)
loader.exec_module(rc)
(rc.EDGES).mkdir(parents=True)
(rc.EDGES / "main.json").write_text("{}")            # the one shared edge switch() demands
rc.live_edge = lambda r: {"pid": 4242} if r == "main" else None
rc.rc_route.proc_started = lambda pid: "start-4242"      # the edge's start (#595)
rc.handoff_taken = lambda *a: True                    # the edge's proxy took it
rc.profile_type = lambda p: "apikey"
rc.resolve_route = lambda p: {"ANTHROPIC_BASE_URL": "https://gw", "ANTHROPIC_API_KEY": "test-only-key",
                              "CCMORPH_ROUTE_HANDLE": ""}
rc.retire_route_with = lambda *a: None
rc.adapter_env = lambda: {}
rc.start_adapter = lambda *a: "http://127.0.0.1:1"
real_write = os.write
def write(fd, data):
    n = real_write(fd, data)
    if point == "handoff-temp" and b"test-only-key" in bytes(data):
        os.kill(os.getpid(), sig)                     # the temp now holds the key
    return n
os.write = write
real_word = rc.write_inference
def word(w, path=rc.INFERENCE):
    if point == "handoff-written":
        os.kill(os.getpid(), sig)                     # switch-<profile>.json now holds the key
    if not lands:
        raise OSError("inference is read-only")
    real_word(w, path)
rc.write_inference = word
try:
    rc.switch("gw", "me")
except BaseException as stopped:
    print(type(stopped).__name__, file=sys.stderr)
    raise SystemExit(4)
raise SystemExit(0)
"""


@pytest.mark.parametrize("point,lands", [
    ("handoff-temp", False),
    ("handoff-written", False),
    ("handoff-written", True),
], ids=["key-in-temp-word-fails", "key-in-handoff-word-fails", "key-in-handoff-word-lands"])
@pytest.mark.parametrize("sig", [signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT, signal.SIGINT],
                         ids=["sigterm", "sighup", "sigquit", "sigint"])
def test_termination_during_a_switch_leaves_no_handoff_holding_the_key(tmp_path, sig, point, lands):
    """issue #777, the --switch path: the handoff is the route key in a file from its
    temp's first byte until the word that makes the next request read it. Its cleanup
    ran only when Python unwound, so a TERM/HUP/QUIT at its default disposition in that
    window killed the launcher and left the key on disk for good. The signal still ends
    the switch, after the window: the handoff is then either live (the word landed) or
    gone, and no other file ever held the key."""
    import os
    import subprocess

    root = tmp_path / "state"
    result = subprocess.run([sys.executable, "-c", _SWITCH_SIGNAL_CHILD,
                             str(Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc"),
                             str(int(sig)), point, "1" if lands else "0"],
                            env={**os.environ, "CCMORPH_STATE_DIR": str(root)}, cwd=tmp_path,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    if sig == signal.SIGINT:
        assert (result.returncode, result.stderr.strip()) == (4, "KeyboardInterrupt")
    else:
        assert result.returncode == -sig, f"exit={result.returncode}; stderr={result.stderr}"
    bridge = root / "profiles" / ".rc-bridge"
    holding = sorted(p.name for p in root.rglob("*") if p.is_file() and b"test-only-key" in p.read_bytes())
    if lands:
        assert holding == ["switch-gw.json"]               # the live handoff the next turn reads
        assert (bridge / "inference").read_text() == "gw\n"
    else:
        assert holding == []
        assert list(bridge.glob("switch-*")) == []


@pytest.mark.parametrize("sig,expected", [
    (signal.SIGTERM, str(128 + signal.SIGTERM)),
    (signal.SIGINT, "KeyboardInterrupt"),
], ids=["sigterm", "sigint"])
def test_a_signal_as_the_route_key_file_is_created_leaves_no_file(tmp_path, sig, expected):
    """The key file exists from inside mkstemp, before own_edge holds its name. A
    signal landing there must wait until cleanup can see the file: Ctrl-C as much as
    TERM, or an empty route-key.* outlives the start (README: no lingering file)."""
    import os
    import subprocess

    child = r"""import argparse, importlib.machinery, importlib.util, os, signal, sys
path, sig, expected = sys.argv[1], int(sys.argv[2]), sys.argv[3]
for s in (signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT):
    signal.signal(s, signal.SIG_DFL)
signal.signal(signal.SIGINT, signal.default_int_handler)
loader = importlib.machinery.SourceFileLoader("ccmorph_rc_mkstemp", path)
spec = importlib.util.spec_from_loader(loader.name, loader)
rc = importlib.util.module_from_spec(spec)
loader.exec_module(rc)
rc.resolve_route = lambda route: {"ANTHROPIC_API_KEY": "test-only-key", "ANTHROPIC_BASE_URL": "https://example.invalid"}
rc.retire_route_with = lambda *a: None
rc.adapter_env = lambda: {}
rc.start_adapter = lambda *a: "http://127.0.0.1:1"
rc.strip_env = lambda: ()
rc.shutil.which = lambda name: "/test/" + name
real_mkstemp = rc.tempfile.mkstemp
def mkstemp(*a, **k):
    made = real_mkstemp(*a, **k)
    if str(k.get("prefix", "")).startswith("route-key."):
        os.kill(os.getpid(), sig)                 # the file exists; own_edge has not got its name yet
    return made
rc.tempfile.mkstemp = mkstemp
rc.subprocess.Popen = lambda *a, **k: object()
try:
    rc.own_edge("test", os.getpid(), argparse.Namespace(debug=False), None)
except BaseException as stopped:
    code = str(stopped.code) if isinstance(stopped, SystemExit) else type(stopped).__name__
    keys = list(rc.STATE.glob("route-key.*"))
    if code == expected and not keys:
        raise SystemExit(0)
    print(f"unexpected exit={code} key_files={keys}", file=sys.stderr)
    raise SystemExit(2)
raise SystemExit(3)
"""
    state = tmp_path / "state"
    result = subprocess.run([sys.executable, "-c", child,
                             str(Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc"),
                             str(int(sig)), expected],
                            env={**os.environ, "CCMORPH_STATE_DIR": str(state)},
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, f"child exit={result.returncode}; stderr={result.stderr}"
    assert list(state.rglob("route-key.*")) == []


def _hot_addon(monkeypatch, tmp_path):
    """The addon as a running proxy holds it: its own private copy, and RC_SOURCE the
    install's folder it reloads rc_flow and rc_route from."""
    import shutil
    src, private = tmp_path / "src", tmp_path / "addon.x"
    for d in (src, private):
        d.mkdir()
        for name in ("rc_addon.py", "rc_flow.py", "rc_route.py"):
            shutil.copy(ADDON.parent / name, d / name)
    state = tmp_path / "state"
    state.mkdir()
    (state / "inference").write_text("route\n")
    (state / "route-key").write_text("route-key")
    for k, v in {"RC_UPSTREAM": "http://127.0.0.1:60111", "RC_KEY_FILE": str(state / "route-key"),
                 "RC_INFERENCE": str(state / "inference"), "RC_LOG": str(state / "addon.log"),
                 "RC_SOURCE": str(src), "RC_TIERS": "{}"}.items():
        monkeypatch.setenv(k, v)
    monkeypatch.delenv("RC_PARENT_PID", raising=False)
    addon = load_script(private / "rc_addon.py", "rc_addon_hot")
    monkeypatch.setattr(addon, "RECHECK", 0)
    return addon, src, state


def _routed_port(addon):
    flow = _Flow()
    addon.requestheaders(flow)
    return flow.request.port


def test_a_routing_change_reaches_the_running_proxy_with_no_restart(tmp_path, monkeypatch):
    """A routing fix used to need the proxy restarted, which took every phone session's
    turn with it. rc_flow and rc_route now reload from the install when they change;
    the key, held by the loader, stays. A file that does not load (a checkout half
    written) keeps the running code, and its next change is loaded."""
    import os
    addon, src, state = _hot_addon(monkeypatch, tmp_path)
    assert _routed_port(addon) == 60111
    flow_py = src / "rc_flow.py"
    good = flow_py.read_text()
    flow_py.write_text(good.replace("upstream, key, tiers = s.upstream, s.key, s.tiers",
                                    "upstream, key, tiers = 'http://127.0.0.1:60222', s.key, s.tiers"))
    os.utime(flow_py, ns=(1, 2_000_000_000_000_000_000))
    assert _routed_port(addon) == 60222                        # no restart: the next request
    flow_py.write_text("def requestheaders(:\n")
    os.utime(flow_py, ns=(1, 3_000_000_000_000_000_000))
    assert _routed_port(addon) == 60222                        # broken: the running code stays
    assert "failed" in (state / "addon.log").read_text()
    flow_py.write_text(good)
    os.utime(flow_py, ns=(1, 4_000_000_000_000_000_000))
    assert _routed_port(addon) == 60111
    assert "rc_flow" not in sys.modules                         # nothing else sees the reload


def test_the_proxy_takes_a_handoff_without_waiting_for_a_request(tmp_path, monkeypatch):
    """A session's profile can sit idle for days, and the handoff is its key on disk
    until the proxy reads it. The proxy now looks every half second, not on the next
    request for that profile."""
    import json
    addon, _, state = _hot_addon(monkeypatch, tmp_path)
    hand = state / "switch-gw.json"
    hand.write_text(json.dumps({"upstream": "http://127.0.0.1:54505", "key": "k2", "tiers": {}}))
    import threading
    threading.Thread(target=addon._take_handoffs, daemon=True).start()
    assert wait_until(lambda: not hand.exists(), HANG_TIMEOUT)
    assert addon.SWITCHED["gw"] == ("http://127.0.0.1:54505", "k2", {})


def test_a_taken_handoff_is_recorded_before_it_can_be_seen_gone(tmp_path, monkeypatch):
    """issue #900: the take removed the handoff before its record landed, so the one
    signal that says "taken" — the file is gone — could be read with nothing
    switched: the poller's own watcher caught SWITCHED empty a statement after its
    wait passed (CI run 45562448374). Hook the removal itself: every path that makes
    a valid handoff vanish, by unlink or by the take's rename, must find the record
    already in SWITCHED."""
    import json
    addon, _, state = _hot_addon(monkeypatch, tmp_path)
    hand = state / "switch-gw.json"
    hand.write_text(json.dumps({"upstream": "http://127.0.0.1:54505", "key": "k2", "tiers": {}}))

    expect = ["http://127.0.0.1:54505"]     # the upstream the record holds at each removal
    def recorded(what):                     # the hook fires while the name still stands
        assert addon.SWITCHED.get("gw", (None,))[0] == expect[0], \
            f"the {what} removed the handoff before recording it"

    class WatchedPath(type(addon.Path())):  # the handoff removed by name (issue #900's read)
        def unlink(self, missing_ok=False):
            if self.name == "switch-gw.json":
                recorded("unlink")
            return super().unlink(missing_ok=missing_ok)

    real = addon.os                         # and the rename is what a take makes gone
    class WatchedOs:
        def __getattr__(self, name):
            return getattr(real, name)

        def rename(self, src, dst):
            if Path(src).name == "switch-gw.json":
                recorded("rename")
            return real.rename(src, dst)

    monkeypatch.setattr(addon, "Path", WatchedPath)
    monkeypatch.setattr(addon, "os", WatchedOs())
    assert addon._switched("gw") == ("http://127.0.0.1:54505", "k2", {})
    assert not hand.exists()                # the take happened, hooks and all
    # A re-hand over an endpoint already held: at the removal the record is the new
    # one, not merely the old one still standing.
    expect[0] = "http://127.0.0.1:54506"
    hand.write_text(json.dumps({"upstream": "http://127.0.0.1:54506", "key": "k3", "tiers": {}}))
    assert addon._switched("gw") == ("http://127.0.0.1:54506", "k3", {})
    assert not hand.exists()


@pytest.mark.parametrize("body", [
    pytest.param('{"upstream": "http://127.0.0.1:54505"}', id="parses-but-holds-no-key"),
    pytest.param('{"key": "k2"}', id="parses-but-holds-no-upstream"),
    pytest.param("not json", id="does-not-parse"),
])
def test_a_handoff_holding_no_endpoint_is_removed_without_recording_anything(
        tmp_path, monkeypatch, body):
    """A switch-<profile>.json that can route nothing — it does not parse (the
    publish is atomic, so this is garbage, not a half write), or lacks the upstream
    or key an endpoint needs — is a credential held for nothing: one look removes it
    and records nothing, and it never lingers to be re-polled every half second
    (issue #900: pin the hygiene choice — today the fieldless one is already gone
    silently and the unparseable one stays forever)."""
    addon, _, state = _hot_addon(monkeypatch, tmp_path)
    hand = state / "switch-gw.json"
    hand.write_text(body)
    assert addon._switched("gw") is None
    assert not hand.exists()
    assert not list(state.glob("switch-*")) and not list(state.glob("taken.*"))
    assert addon.SWITCHED == {} and getattr(addon, "HELD", {}) == {}   # HELD: the #595 record


def test_a_failed_handoff_rename_leaves_the_endpoint_untouched(tmp_path, monkeypatch):
    """#900 review: the record runs ahead of the rename so a gone handoff always
    has its record — but a take that moved nothing must not leave a switched
    endpoint behind a handoff that still stands. A failed rename restores the
    endpoint and the held word: the switch's wait ends in its own cleanup as if
    nothing had happened, and every later look finds the state as it was."""
    import json
    addon, _, state = _hot_addon(monkeypatch, tmp_path)
    hand = state / "switch-gw.json"
    hand.write_text(json.dumps({"upstream": "http://127.0.0.1:54505", "key": "k1",
                                "tiers": {}, "id": "h1"}))
    assert addon._switched("gw") == ("http://127.0.0.1:54505", "k1", {})    # the baseline
    real = addon.os
    class RenamesRefused:
        def __getattr__(self, name):
            return getattr(real, name)

        def rename(self, src, dst, *args, **kw):
            raise OSError("rename refused")
    monkeypatch.setattr(addon, "os", RenamesRefused())
    hand.write_text(json.dumps({"upstream": "http://127.0.0.1:54599", "key": "k2",
                                "tiers": {}, "id": "h2"}))
    assert addon._switched("gw") == ("http://127.0.0.1:54505", "k1", {})    # not the new one
    assert addon.SWITCHED["gw"] == ("http://127.0.0.1:54505", "k1", {})     # endpoint unchanged
    assert addon.HELD["gw"] == "h1"                                         # and the same word held
    assert hand.exists()                                                    # stands there for a retry
    assert not list(state.glob("taken.*"))
    # A profile with no prior entry: the failed take restores "absent" — pops its
    # own record rather than leaving a None behind one that never switches.
    (state / "switch-no.json").write_text(json.dumps({"upstream": "http://127.0.0.1:54598",
                                                      "key": "k9", "tiers": {}, "id": "h9"}))
    assert addon._switched("no") is None
    assert "no" not in addon.SWITCHED and "no" not in addon.HELD


def test_a_take_that_puts_another_edge_handoff_back_switches_nothing(tmp_path, monkeypatch):
    """The same rule on the take's other failed path (#900 review): when the rename
    moved a replacement edge's publication, that one is put back — and the record
    written before the rename goes back too. What this proxy took was not its own,
    so the endpoint, the held word and held.<pid>.json all stay exactly as they were."""
    import json
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "start-500")
    hand = bridge / "switch-oli.json"
    def payload(start, upstream, id_):
        return json.dumps({"upstream": upstream, "key": "k", "tiers": {}, "id": id_,
                           "edge": 500, "edge_started": start})
    hand.write_text(payload("start-500", "u1", "h1"))
    assert addon._switched("oli") == ("u1", "k", {})            # the baseline
    real = addon.os
    class PublishedMeanwhile:
        def __getattr__(self, name):
            return getattr(real, name)

        def rename(self, src, dst, *args, **kw):
            Path(src).write_text(json.dumps({"upstream": "other", "key": "k", "tiers": {},
                                             "id": "h600", "edge": 600,
                                             "edge_started": "start-600"}))
            return real.rename(src, dst)                        # the take moves theirs
    monkeypatch.setattr(addon, "os", PublishedMeanwhile())
    hand.write_text(payload("start-500", "u2", "h2"))
    assert addon._switched("oli") == ("u1", "k", {})            # ours was replaced: nothing switched
    assert addon.SWITCHED["oli"] == ("u1", "k", {})
    assert addon.HELD["oli"] == "h1"
    assert json.loads(hand.read_text())["id"] == "h600"         # theirs stands, taken by its proxy
    assert not list(bridge.glob("taken.*"))
    assert json.loads((bridge / "held.500.json").read_text())["profiles"] == {"oli": "h1"}


@pytest.mark.parametrize("first", [
    pytest.param("valid", id="read-an-older-handoff-of-this-edge"),
    pytest.param("garbage", id="read-garbage"),
])
def test_a_same_edge_replacement_the_take_moves_is_recorded_before_it_is_acknowledged(
        tmp_path, monkeypatch, first):
    """#900 review: a publication for this very edge can land on the name between
    the take's read and its rename, so the rename moves one the take never read. No
    take by name can record it before that syscall (no proxy code runs between the
    writer's replace and the rename). What the take does: the moved file decides,
    its endpoint and id are recorded before held.<pid>.json names it and before
    _switched returns, and it is acknowledged once. ccmorph-rc's .switch.lock keeps
    its own publishes from replacing a standing handoff."""
    import json
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "start-500")
    hand = bridge / "switch-oli.json"
    def payload(upstream, id_):
        return json.dumps({"upstream": upstream, "key": "k", "tiers": {}, "id": id_,
                           "edge": 500, "edge_started": "start-500"})
    hand.write_text(payload("old", "h1") if first == "valid" else "not json")
    pending = bridge / "newer"
    pending.write_text(payload("new", "h2"))           # the next publication, about to land
    real = addon.os
    at_move = []                                       # SWITCHED right after the move
    class ReplacedBeforeTheRename:
        def __getattr__(self, name):
            return getattr(real, name)

        def rename(self, src, dst, *args, **kw):
            if os.fspath(src) == str(hand) and pending.exists():
                real.replace(pending, hand)             # same edge, published mid-take
            result = real.rename(src, dst, *args, **kw)
            at_move.append(addon.SWITCHED.get("oli"))
            return result
    monkeypatch.setattr(addon, "os", ReplacedBeforeTheRename())
    says = []                                          # each acknowledgement, and the record then
    real_publish = addon._publish_held
    def publish():
        says.append((dict(addon.HELD), addon.SWITCHED.get("oli")))
        real_publish()
    monkeypatch.setattr(addon, "_publish_held", publish)
    assert addon._switched("oli") == ("new", "k", {})   # the moved one is the one taken
    # At the move the record is the publication the rename itself moved: the take's
    # last look resolves inside the syscall, after any writer's replace.
    assert at_move == [("new", "k", {})]
    assert says == [({"oli": "h2"}, ("new", "k", {}))]  # recorded before the only word names it
    assert addon.HELD["oli"] == "h2"
    assert not hand.exists()
    assert list(bridge.glob("switch-*")) == [] and not list(bridge.glob("taken.*"))
    assert json.loads((bridge / "held.500.json").read_text())["profiles"] == {"oli": "h2"}


@pytest.mark.parametrize("meanwhile", [
    pytest.param("withdrawn", id="the-switch-withdrew-it"),
    pytest.param("garbage", id="replaced-by-garbage"),
])
def test_a_take_that_moved_nothing_of_its_own_leaves_the_endpoint_untouched(
        tmp_path, monkeypatch, meanwhile):
    """#900 review: the record runs ahead of the rename, so a take whose rename then
    finds the handoff withdrawn (FileNotFoundError), or moves garbage that replaced
    it, must put the endpoint and the held word back as they were."""
    import json
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "start-500")
    hand = bridge / "switch-oli.json"
    def payload(upstream, id_):
        return json.dumps({"upstream": upstream, "key": "k", "tiers": {}, "id": id_,
                           "edge": 500, "edge_started": "start-500"})
    hand.write_text(payload("old", "h1"))
    assert addon._switched("oli") == ("old", "k", {})            # the baseline
    hand.write_text(payload("new", "h2"))
    real = addon.os
    class ChangedBeforeTheRename:
        def __getattr__(self, name):
            return getattr(real, name)

        def rename(self, src, dst, *args, **kw):
            if os.fspath(src) == str(hand):
                if meanwhile == "withdrawn":
                    real.unlink(hand)
                else:
                    hand.write_text("not json")
            return real.rename(src, dst, *args, **kw)
    monkeypatch.setattr(addon, "os", ChangedBeforeTheRename())
    assert addon._switched("oli") == ("old", "k", {})
    assert addon.SWITCHED["oli"] == ("old", "k", {}) and addon.HELD["oli"] == "h1"
    assert list(bridge.glob("switch-*")) == [] and not list(bridge.glob("taken.*"))
    assert json.loads((bridge / "held.500.json").read_text())["profiles"] == {"oli": "h1"}


def test_another_edges_handoff_holding_no_endpoint_is_removed_after_one_look(tmp_path, monkeypatch):
    """#900 review: a handoff tagged for another edge that carries no endpoint is no
    publication any proxy can wait behind: one look removes it, nothing recorded.
    One that does carry an endpoint is still never touched."""
    import json
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "start-500")
    hand = bridge / "switch-oli.json"
    hand.write_text(json.dumps({"key": "k", "edge": 600, "edge_started": "start-600"}))
    assert addon._switched("oli") is None
    assert not hand.exists() and not list(bridge.glob("taken.*"))
    assert addon.SWITCHED == {} and addon.HELD == {}
    hand.write_text(json.dumps({"upstream": "u", "key": "k", "id": "h6",
                                "edge": 600, "edge_started": "start-600"}))
    assert addon._switched("oli") is None and hand.exists()      # theirs, untouched


def test_a_held_record_that_cannot_be_written_never_cuts_the_take_short(tmp_path, monkeypatch):
    """#900 review: an OSError writing held.<pid>.json must not escape _switched,
    where it would end the take poller's thread; the endpoint taken stays held."""
    import json
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "start-500")
    hand = bridge / "switch-oli.json"
    hand.write_text(json.dumps({"upstream": "u", "key": "k", "tiers": {}, "id": "h1",
                                "edge": 500, "edge_started": "start-500"}))
    def refused():
        raise PermissionError("cannot publish held record")
    monkeypatch.setattr(addon, "_publish_held", refused)
    assert addon._switched("oli") == ("u", "k", {})
    assert addon.HELD["oli"] == "h1"
    assert not hand.exists() and not list(bridge.glob("taken.*"))


def test_a_handoff_no_proxy_takes_is_removed_and_nothing_changes(tmp_path, monkeypatch):
    """A proxy started before the handoff poller never reads a handoff no request
    asks for: the switch removes its key file again and puts the word back."""
    rc, _ = _switch_state(tmp_path, monkeypatch)
    monkeypatch.setattr(rc, "handoff_taken", lambda hand, payload: False)
    (rc.STATE / "sessions").mkdir(parents=True)
    (rc.STATE / "sessions" / "cse_1").write_text("account\n")
    with pytest.raises(SystemExit):
        rc.switch("gw", "me", "cse_1")
    assert list(rc.STATE.glob("switch-*")) == []
    assert (rc.STATE / "sessions" / "cse_1").read_text() == "account\n"
    assert not (rc.STATE / "up-gw.json").exists()
    with pytest.raises(SystemExit):
        rc.switch("gw", "me", "cse_2")
    assert not (rc.STATE / "sessions" / "cse_2").exists()


def test_an_edge_is_identified_by_its_start_not_just_its_pid(monkeypatch):
    """issue #595: a record naming only a pid reads as live when the OS gives that
    pid to the edge's replacement. The start cannot change for a living process, so
    a newcomer is told from the edge a profile was handed to. It is the kernel's own
    start -- Linux ticks with the boot id, macOS microseconds -- never `ps`, whose
    whole second a pid reused within it shares; a pid with no process has no start."""
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


def _switch_rig(rc, monkeypatch, tmp_path, alive=None, serving=None):
    """One live edge (pid 500, start "start-500"), a key profile, and seams where real
    processes would be: sidecar state (None: no ledger yet; else a ledger naming pid
    7321), whether a recorded pid is really our adapter, whether a port answers. The
    edge's proxy is support.proxy_took: it says it holds each handoff, and the file stays so
    a test can read what was handed over. Returns (state dir, adapters started as
    (profile, edge, url, pid), signals sent)."""
    state = tmp_path / "bridge"
    (state / "edges").mkdir(parents=True)
    (state / "edges" / "gateway.json").write_text('{"pid": 500, "port": 8080}')
    monkeypatch.setattr(rc, "STATE", state)
    monkeypatch.setattr(rc, "EDGES", state / "edges")
    monkeypatch.setattr(rc, "INFERENCE", state / "inference")
    monkeypatch.setattr(rc, "profile_type", lambda p: "apikey")
    monkeypatch.setattr(rc, "live_edge", lambda r: {"pid": 500} if r == "gateway" else None)
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: f"start-{pid}")
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-oli"})
    monkeypatch.setattr(rc, "retire_route_with", lambda *a: None)
    monkeypatch.setattr(rc, "adapter_env", lambda: {})
    monkeypatch.setattr(rc, "get_tiers", lambda p: {})
    monkeypatch.setattr(rc, "handoff_taken", proxy_took(rc))
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
        return rc.AdapterURL(url, started[-1][3])
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
    hand = json.loads((state / "switch-oli.json").read_text())
    assert (hand["upstream"], hand["edge"], hand["edge_started"]) == (used, 500, "start-500")
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
    """issue #595 review: a ledger naming only the edge's pid let an adapter made
    beside an edge that exited be reused beside the next edge the OS gave that
    number. The ledger records the edge's start; another start (or none) retires the
    old adapter and starts one for the edge that is live now."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path, alive=True, serving=True)
    rec = {"pid": 7321, "upstream": "http://127.0.0.1:45110", "edge": 500}
    if recorded is not None:
        rec["edge_started"] = recorded
    (state / "sidecar-oli.json").write_text(json.dumps(rec))
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: now)
    rc.switch("oli", "alex")
    assert signals == ["SIGTERM"]
    assert [(s[0], s[1]) for s in started] == [("oli", 500)]
    ledger = json.loads((state / "sidecar-oli.json").read_text())
    assert (ledger["pid"], ledger["edge"], ledger["edge_started"]) == (4321, 500, now)
    hand = json.loads((state / "switch-oli.json").read_text())
    assert (hand["upstream"], hand["edge_started"]) == ("http://127.0.0.1:45111", now)


def test_a_stalled_switch_never_retires_the_sidecar_of_the_edge_that_replaced_its_own(tmp_path, monkeypatch):
    """issue #595 review: a switch read its edge's start, then stalled; the edge's
    pid went to a replacement and a second switch installed that edge's sidecar.
    The stalled switch must not read the newer start as stale and retire the
    healthy adapter: under the ledger lock it checks its edge is still the one
    live, and refuses without touching the ledger when it is not."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path, alive=True, serving=True)
    ledger = {"pid": 7321, "upstream": "http://127.0.0.1:45110", "edge": 500, "edge_started": "later-500"}
    (state / "sidecar-oli.json").write_text(json.dumps(ledger))
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: "later-500")   # the edge live now
    with pytest.raises(SystemExit):
        rc.sidecar("oli", 500, "start-500")                                     # what it read before
    assert signals == [] and started == []
    assert json.loads((state / "sidecar-oli.json").read_text()) == ledger


@pytest.mark.parametrize("kind", ["apikey", "provider"])
def test_a_stalled_switch_never_overwrites_the_handoff_of_the_edge_that_replaced_its_own(
        tmp_path, monkeypatch, kind):
    """issue #595 review: a switch read its edge's start, then stalled; the edge
    exited, the OS gave its pid to a replacement, and the replacement's own switch
    published first. The stalled switch re-checks its edge under the switch lock and
    refuses: the replacement's handoff stays and no switch word is written."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    edge = {"start": "start-500"}
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: edge["start"])
    monkeypatch.setattr(rc, "profile_type", lambda p: kind)
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-oli",
                                                        "ANTHROPIC_BASE_URL": "http://127.0.0.1:45200"})
    fresh = {"upstream": "http://127.0.0.1:45120", "key": "sk-new", "tiers": {},
             "edge": 500, "edge_started": "later-500", "id": "fresh"}
    def stalled(route_env):
        edge["start"] = "later-500"                                   # pid 500 is another edge now
        (state / "switch-oli.json").write_text(json.dumps(fresh))     # whose own switch published
        return {}
    monkeypatch.setattr(rc, "pinned_tiers", stalled)
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert json.loads((state / "switch-oli.json").read_text()) == fresh
    assert not (state / "inference").exists()


@pytest.mark.parametrize("kind", ["apikey", "provider"])
def test_a_switch_whose_edge_is_replaced_while_it_waits_for_the_word_lock_hands_nothing_over(
        tmp_path, monkeypatch, kind):
    """issue #595 review: the OS gave the edge's pid to a replacement while the
    switch waited for its locks, and the switch published the old edge's endpoint
    and reported success. The start is read once more after every wait, right
    before the handoff is written: nothing is handed over, no word is written and
    no temp is left."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    edge = {"start": "start-500"}
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: edge["start"])
    monkeypatch.setattr(rc, "profile_type", lambda p: kind)
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-oli",
                                                        "ANTHROPIC_BASE_URL": "http://127.0.0.1:45200"})
    real_lock, waits = rc._word_lock, []
    def word_lock(path):
        waits.append(path)
        if len(waits) == 2:                  # the look under the switch lock; take_ticket's was the first
            edge["start"] = "later-500"      # pid 500 is another edge now
        return real_lock(path)
    monkeypatch.setattr(rc, "_word_lock", word_lock)
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert len(waits) == 2
    assert not (state / "switch-oli.json").exists()
    assert not (state / "inference").exists()
    assert list(state.glob("switch-oli.json.*.tmp")) == []


def test_a_switch_that_started_earlier_never_overwrites_a_later_one_that_landed(tmp_path, monkeypatch):
    """issue #595 review: a stalled `--switch oli` (the supervisor's config bring-up)
    wrote its word after a manual `--switch kimi` started and landed meanwhile, so the
    person's later choice was silently replaced by the older one. Every writer of the
    word takes a ticket when it starts; a word lands only if no later ticket has, so
    the older switch refuses, hands nothing over, and kimi stays."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    real_tiers, nested = rc.pinned_tiers, []
    def stalled(route_env):
        if not nested:
            nested.append(1)
            rc.switch("kimi", "alex")                 # the person switches while oli stalls
            assert (state / "inference").read_text() == "kimi\n"
        return real_tiers(route_env)
    monkeypatch.setattr(rc, "pinned_tiers", stalled)
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert (state / "inference").read_text() == "kimi\n"
    assert (state / "switch-kimi.json").exists()
    assert not (state / "switch-oli.json").exists()   # the refused switch never wrote its key


def test_an_older_switch_overtaken_by_the_same_profile_keeps_the_later_handoff(tmp_path, monkeypatch, capsys):
    """issue #595 review: a stalled `--switch oli` was overtaken by a later
    `--switch oli` that landed first; the older one replaced the later one's
    handoff, refused, then removed it. An overtaken switch hands nothing over; when
    the word that landed is its own, it says so and succeeds."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    real_tiers, nested = rc.pinned_tiers, []
    def stalled(route_env):
        if not nested:
            nested.append(1)
            rc.switch("oli", "alex")                  # a later switch to the same profile lands first
            nested.append((state / "switch-oli.json").read_bytes())
        return real_tiers(route_env)
    monkeypatch.setattr(rc, "pinned_tiers", stalled)
    rc.switch("oli", "alex")                          # the older one: no SystemExit, its word is live
    assert (state / "switch-oli.json").read_bytes() == nested[1]
    assert (state / "inference").read_text() == "oli\n"
    assert "already goes to 'oli'" in capsys.readouterr().err


def test_an_overtaken_switch_never_touches_a_handoff_it_did_not_write(tmp_path, monkeypatch):
    """issue #595 review: an overtaken switch replaced the profile's handoff, then
    removed it on refusal. The overtaken switch writes nothing at all."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    rc.switch("oli", "alex")                          # an earlier oli, its handoff on disk
    before = (state / "switch-oli.json").read_bytes()
    real_tiers, nested = rc.pinned_tiers, []
    def stalled(route_env):
        if not nested:
            nested.append(1)
            rc.switch("kimi", "alex")                 # a later switch to another profile lands first
        return real_tiers(route_env)
    monkeypatch.setattr(rc, "pinned_tiers", stalled)
    monkeypatch.setattr(rc, "_listening", lambda upstream: False)   # oli is not up: it would hand over again
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert (state / "inference").read_text() == "kimi\n"
    assert (state / "switch-oli.json").read_bytes() == before
    assert (state / "switch-kimi.json").exists()


def test_a_rehand_writes_no_word_and_never_overtakes_a_switch_in_flight(tmp_path, monkeypatch, capsys):
    """issue #595: the supervisor re-hands the live profile after an edge or proxy
    restart. That upkeep must not take a place in line: a `--switch oli` the person
    started before it, still bringing its adapter up, lands afterwards all the same."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    (state / "inference").write_text("kimi\n")       # kimi is live; its proxy restarted
    real_tiers, nested = rc.pinned_tiers, []
    def stalled(route_env):
        if not nested:
            nested.append(1)
            rc.switch("kimi", "alex", rehand=True)   # rc-hosts' upkeep, while oli stalls
            assert (state / "inference").read_text() == "kimi\n"
            assert (state / "switch-kimi.json").exists()
        return real_tiers(route_env)
    monkeypatch.setattr(rc, "pinned_tiers", stalled)
    rc.switch("oli", "alex")
    assert (state / "inference").read_text() == "oli\n"
    assert "holds 'kimi' again" in capsys.readouterr().err


def test_switches_hand_over_one_at_a_time(tmp_path, monkeypatch):
    """issue #595: a switch's publish, its wait for the proxy and its word run under
    one lock, so no switch replaces or removes a handoff another one published."""
    import fcntl
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    hand, held, real = state / "switch-oli.json", [], rc.os.replace
    def replace(src, dst, *a, **k):
        if Path(dst) == hand:
            with open(state / ".switch.lock", "a") as lock:
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    held.append(True)
        return real(src, dst, *a, **k)
    monkeypatch.setattr(rc.os, "replace", replace)
    rc.switch("oli", "alex")
    assert held == [True]


def test_a_switch_that_cannot_read_its_edges_start_changes_nothing(tmp_path, monkeypatch):
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: None)
    with pytest.raises(SystemExit):
        rc.switch("oli", "alex")
    assert started == [] and signals == []
    assert not (state / "switch-oli.json").exists() and not (state / "inference").exists()


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
        {"pid": 900, "upstream": "http://127.0.0.1:45110", "edge": 500, "edge_started": "start-500"}))
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


def test_profiles_whose_names_clean_alike_keep_their_own_sidecar(tmp_path, monkeypatch):
    """issue #595 review: the ledger was named by safe_part, which merges `api_eu`
    and `api__eu` -- two valid profiles -- onto one file, so switching A -> B -> A
    retired nothing, started a second A and left the first unrecorded. Each
    profile keeps its own ledger, and the repeat reuses its own adapter."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    monkeypatch.setattr(rc, "our_adapter", lambda pid, profile, edge: any(
        s[3] == pid and s[0] == profile for s in started))
    monkeypatch.setattr(rc, "_listening", lambda upstream: True)
    first = rc.sidecar("api_eu", 500, "start-500")
    rc.sidecar("api__eu", 500, "start-500")
    assert rc.sidecar("api_eu", 500, "start-500") == first
    assert [s[0] for s in started] == ["api_eu", "api__eu"] and signals == []


def test_a_sidecar_whose_record_cannot_be_written_is_stopped(tmp_path, monkeypatch):
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    real = rc.write_json
    def refuse(path, obj):
        if path.name.startswith("sidecar-"):
            raise OSError("ledger refused")
        return real(path, obj)
    monkeypatch.setattr(rc, "write_json", refuse)
    with pytest.raises(SystemExit):
        rc.sidecar("oli", 500, "start-500")
    assert len(started) == 1
    assert signals == ["SIGTERM"]
    assert not (state / "sidecar-oli.json").exists()


def test_a_restarted_proxy_holds_nothing_so_a_repeat_switch_hands_it_over_again(tmp_path, monkeypatch):
    """issue #595: up-<profile>.json said "up" while the proxy that took the handoff
    was gone, so `--switch` after a proxy restart did nothing and the phone stayed on
    the route. A bring-up counts as up only while the edge's proxy says it holds that
    very handoff; the adapter is reused either way."""
    rc = _launcher()
    state, started, signals = _switch_rig(rc, monkeypatch, tmp_path)
    monkeypatch.setattr(rc, "our_adapter", lambda pid, profile, edge: any(s[3] == pid for s in started))
    monkeypatch.setattr(rc, "_listening", lambda upstream: True)
    handed = []
    took = proxy_took(rc)
    monkeypatch.setattr(rc, "handoff_taken", lambda hand, payload: handed.append(payload) or took(hand, payload))
    rc.switch("oli", "alex")
    rc.switch("oli", "alex")                              # its proxy holds it: nothing to hand over
    assert len(handed) == 1 and rc.brought_up("oli", 500, "start-500", {})
    (state / "held.500.json").write_text(json.dumps(     # what a restarted proxy says at start
        {"edge": 500, "edge_started": "start-500", "profiles": {}}))
    assert not rc.brought_up("oli", 500, "start-500", {})
    rc.switch("oli", "alex")
    assert len(handed) == 2 and len(started) == 1        # handed over again, on the same adapter
    assert json.loads(handed[1])["id"] != json.loads(handed[0])["id"]


@pytest.mark.parametrize("upstream, alive", [
    ("http://127.0.0.1:{port}", True),      # the record's host answers: shared, not brought up again
    ("http://gw.invalid:{port}", False),    # a LOCAL listener holds the port; the record's host does not answer
    ("http://127.0.0.1", False),            # no port to ask: never taken for alive (bring-up redone, safe)
])
def test_bring_up_reuse_is_proved_against_the_records_own_host(tmp_path, monkeypatch, upstream, alive):
    """issue #899: the reuse check took the recorded upstream's port and probed
    127.0.0.1, so a profile brought up on a remote endpoint read as "up" whenever a
    local process held that port -- the switch skipped the handoff and the session
    landed on the route, on the backend it had switched away from. The probe asks
    the host and port the record itself names: a port held only on loopback never
    proves a remote endpoint up, and the proxy's own held note cannot stand in."""
    rc = _launcher()
    monkeypatch.setattr(rc, "STATE", tmp_path)
    with socket.socket() as srv:                            # a real listener on this machine
        srv.bind(("127.0.0.1", 0))
        srv.listen()
        (tmp_path / "up-gw.json").write_text(json.dumps(
            {"edge": 500, "edge_started": "start-500", "hand": "h1", "tiers": {},
             "upstream": upstream.format(port=srv.getsockname()[1])}))
        (tmp_path / "held.500.json").write_text(json.dumps(  # the proxy says it holds this very handoff
            {"edge": 500, "edge_started": "start-500", "profiles": {"gw": "h1"}}))
        assert rc.brought_up("gw", 500, "start-500", {}) is alive   # only the probe can answer


def test_a_switch_lands_only_once_the_edges_proxy_says_it_holds_the_handoff(tmp_path, monkeypatch):
    """A handoff that is merely gone proves nothing: another proxy, or one that cannot
    say what it holds, took it. --switch waits for the edge's own proxy to name it."""
    rc = _launcher()
    monkeypatch.setattr(rc, "STATE", tmp_path)
    hand = tmp_path / "switch-oli.json"
    payload = json.dumps({"upstream": "u", "key": "k", "tiers": {}, "edge": 500,
                          "edge_started": "start-500", "id": "h1"}).encode()
    assert not rc.handoff_taken(hand, payload, 0.2)       # gone, but nobody says they hold it
    (tmp_path / "held.500.json").write_text(json.dumps(
        {"edge": 500, "edge_started": "other-500", "profiles": {"oli": "h1"}}))
    assert not rc.handoff_taken(hand, payload, 0.2)       # another process behind pid 500
    (tmp_path / "held.500.json").write_text(json.dumps(
        {"edge": 500, "edge_started": "start-500", "profiles": {"oli": "h1"}}))
    assert rc.handoff_taken(hand, payload, 0.2)
    hand.write_bytes(payload)
    assert not rc.handoff_taken(hand, payload, 0.2)       # still on disk: not taken


def _proxy_addon(monkeypatch, tmp_path, parent, started):
    """rc_addon as the proxy of edge `parent` (whose start is `started`) loads it."""
    from types import ModuleType, SimpleNamespace
    try:
        import mitmproxy  # noqa: F401
    except ImportError:
        mitm = ModuleType("mitmproxy")
        mitm.http = SimpleNamespace(HTTPFlow=object)
        monkeypatch.setitem(sys.modules, "mitmproxy", mitm)
    bridge = tmp_path / "bridge"
    bridge.mkdir(exist_ok=True)
    key = tmp_path / f"route-key.{len(list(tmp_path.glob('route-key.*')))}"
    key.write_text("sk-route")
    for k, v in {"RC_UPSTREAM": "http://127.0.0.1:9", "RC_KEY_FILE": str(key), "RC_TIERS": "{}",
                 "RC_INFERENCE": str(bridge / "inference"), "RC_PARENT_PID": str(parent),
                 "RC_PARENT_STARTED": started}.items():
        monkeypatch.setenv(k, v)
    for k in ("RC_LOG", "RC_SOURCE"):
        monkeypatch.delenv(k, raising=False)
    return load_script(ADDON, "rc_addon_proxy"), bridge


def test_a_proxy_takes_only_its_own_edges_handoff_and_says_what_it_holds(tmp_path, monkeypatch):
    """issue #595: every proxy took every handoff, so a bridge's own proxy could take
    the one --switch meant for the edge, and nothing said what any proxy held. A
    proxy takes a handoff naming its edge (pid and start) only, and names what it
    holds in held.<pid>.json -- before it deletes the handoff."""
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "start-500")
    addon._publish_held()                                 # what running() writes at start
    held = bridge / "held.500.json"
    assert json.loads(held.read_text()) == {"edge": 500, "edge_started": "start-500", "profiles": {}}
    hand = bridge / "switch-oli.json"
    for other in ({"edge": 600, "edge_started": "start-600"}, {"edge": 500, "edge_started": "other-500"}):
        hand.write_text(json.dumps({"upstream": "u", "key": "k", "tiers": {}, "id": "h0", **other}))
        assert addon._switched("oli") is None and hand.exists()
    hand.write_text(json.dumps({"upstream": "u", "key": "k", "tiers": {}, "id": "h1",
                                "edge": 500, "edge_started": "start-500"}))
    assert addon._switched("oli") == ("u", "k", {})
    assert not hand.exists()
    assert json.loads(held.read_text())["profiles"] == {"oli": "h1"}
    assert "k" not in held.read_text().replace('"oli"', "")   # no key in what it says
    addon.done()
    assert not held.exists()                              # what it held goes with it


def test_a_delayed_proxy_never_removes_a_handoff_published_after_its_take(tmp_path, monkeypatch):
    """issue #595 review: a proxy read the handoff, then unlinked the path later; a
    switch for the edge that replaced its own could publish there meanwhile, and the
    old proxy deleted that handoff, which the replacement's proxy never got. The take
    moves the one publication it read, so the newer handoff stays where it was."""
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "old")
    hand = bridge / "switch-oli.json"
    def payload(start):
        return json.dumps({"upstream": "u", "key": "k", "tiers": {}, "id": start,
                           "edge": 500, "edge_started": start})
    hand.write_text(payload("old"))
    held = addon._publish_held
    def replaced_meanwhile():                 # the replacement edge's switch publishes now
        hand.write_text(payload("new"))
        held()
    monkeypatch.setattr(addon, "_publish_held", replaced_meanwhile)
    assert addon._switched("oli") == ("u", "k", {})
    assert json.loads(hand.read_text())["id"] == "new"
    assert not list(bridge.glob("taken.*"))   # what it took holds no key on disk


def test_a_held_record_goes_with_the_edge_it_describes(tmp_path, monkeypatch):
    """A proxy killed with its edge cannot remove held.<pid>.json; the next start
    sweeps the record of an edge that is gone and keeps a live one's."""
    rc = _launcher()
    gone = subprocess.Popen([sys.executable, "-c", "pass"])
    gone.wait()
    (tmp_path / f"held.{gone.pid}.json").write_text("{}")
    (tmp_path / f"held.{os.getpid()}.json").write_text("{}")
    # issue #595 review: a switch killed between its publish and its cleanup left the
    # route key in switch-<p>.json for an edge no proxy will ever serve again, and a
    # proxy killed mid-take left taken.<pid>.*: both go; a live edge's handoff stays.
    me = rc.edge_started(os.getpid())
    for name, edge, started in (("dead", gone.pid, "s"), ("recycled", os.getpid(), "an-earlier-start"),
                                ("live", os.getpid(), me)):
        (tmp_path / f"switch-{name}.json").write_text(json.dumps(
            {"key": "k", "edge": edge, "edge_started": started}))
    (tmp_path / "switch-junk.json").write_text("not json")
    (tmp_path / f"taken.{gone.pid}.oli.json").write_text("{}")
    (tmp_path / f"taken.{os.getpid()}.oli.json").write_text("{}")
    rc.sweep_orphans(tmp_path)
    assert sorted(p.name for p in tmp_path.glob("held.*")) == [f"held.{os.getpid()}.json"]
    assert sorted(p.name for p in tmp_path.glob("switch-*")) == ["switch-live.json"]
    assert sorted(p.name for p in tmp_path.glob("taken.*")) == [f"taken.{os.getpid()}.oli.json"]
    # A live edge whose start cannot be read this once is not proof it is gone.
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: None)
    rc.sweep_orphans(tmp_path)
    assert sorted(p.name for p in tmp_path.glob("switch-*")) == ["switch-live.json"]


@pytest.mark.parametrize("now, kept", [
    pytest.param("another-process", False, id="pid-reused-by-another-process-drops-the-key-issue-595"),
    pytest.param(None, True, id="live-pid-whose-start-cannot-be-read-keeps-it"),
    pytest.param("taker", True, id="the-taker-itself-keeps-it"),
])
def test_a_proxy_killed_mid_take_leaves_no_key_once_its_pid_is_reused(tmp_path, monkeypatch, now, kept):
    """issue #595 review: a proxy that died between moving a handoff aside
    (taken.<pid>.*) and removing it left the key on disk, and once the OS gave its
    pid to another process the sweep kept it, because that pid answered. The taken
    name carries the taker's own start; another start at that pid proves the taker
    gone. A start that cannot be read proves nothing: the file stays."""
    rc = _launcher()
    bridge = tmp_path / "bridge"
    bridge.mkdir()
    edge = os.getpid()
    start = rc.edge_started(edge)
    (bridge / "switch-oli.json").write_text(json.dumps(
        {"upstream": "http://127.0.0.1:9", "key": "sk-synthetic-switch", "tiers": {}, "id": "h1",
         "edge": edge, "edge_started": start}))
    (tmp_path / "route-key").write_text("sk-synthetic-route")
    crash = ("import os, runpy, sys\n"
             "g = runpy.run_path(sys.argv[1])['_switched'].__globals__\n"
             "sys.path.insert(0, os.path.dirname(sys.argv[1]))\n"
             "import rc_route\n"
             "print(rc_route.proc_started(os.getpid()), flush=True)\n"
             "g['_publish_held'] = lambda: os._exit(73)\n"      # killed mid-take
             "g['_switched']('oli')\n")
    env = {**os.environ, "RC_UPSTREAM": "http://127.0.0.1:9", "RC_KEY_FILE": str(tmp_path / "route-key"),
           "RC_INFERENCE": str(bridge / "inference"), "RC_PARENT_PID": str(edge),
           "RC_PARENT_STARTED": start, "RC_LOG": ""}
    proxy = subprocess.Popen([sys.executable, "-c", crash, str(ADDON)], env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    out, err = proxy.communicate(timeout=HANG_TIMEOUT)
    assert proxy.returncode == 73, err
    taker, taker_start = proxy.pid, out.strip()
    left = list(bridge.glob(f"taken.{taker}.*.oli.json"))
    assert len(left) == 1 and "sk-synthetic-switch" in left[0].read_text()   # the crash left the key
    assert left[0].name.split(".")[2] == rc.rc_route.start_tag(taker_start)  # named by the taker's start
    real_kill, real_start = os.kill, rc.rc_route.proc_started
    monkeypatch.setattr(os, "kill", lambda pid, sig: None if (pid, sig) == (taker, 0) else real_kill(pid, sig))
    monkeypatch.setattr(rc.rc_route, "proc_started",
                        lambda pid: (taker_start if now == "taker" else now) if pid == taker else real_start(pid))
    rc.sweep_orphans(bridge)
    assert left[0].exists() is kept


def test_a_taken_file_whose_takers_start_is_unknown_is_judged_by_its_pid(tmp_path, monkeypatch):
    """A proxy that could not read its own start names its take "unknown": no start
    can prove that taker gone, so only a pid no process holds removes the file."""
    rc = _launcher()
    assert rc.rc_route.start_tag(None) == "unknown"
    tag = rc.rc_route.start_tag("darwin:1764100000.000002")
    assert len(tag) == 16 and "." not in tag and ":" not in tag
    gone = subprocess.Popen([sys.executable, "-c", "pass"])
    gone.wait()
    me = os.getpid()
    for pid in (me, gone.pid):
        (tmp_path / f"taken.{pid}.unknown.oli.json").write_text("{}")
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: "another-process")
    rc.sweep_orphans(tmp_path)
    assert [p.name for p in tmp_path.glob("taken.*")] == [f"taken.{me}.unknown.oli.json"]


def test_a_proxy_shutdown_never_removes_the_held_record_a_replacement_wrote(tmp_path, monkeypatch):
    """issue #595 review: done() read held.<pid>.json, found its own record, then
    unlinked the name; the proxy of a replacement edge given the same pid could write
    its own record in between, and the old proxy removed that one. The record is
    moved aside and checked where it lies: the replacement's goes back, and a record
    that was never ours is not touched."""
    addon, bridge = _proxy_addon(monkeypatch, tmp_path, 500, "old")
    held = bridge / "held.500.json"
    old = {"edge": 500, "edge_started": "old", "profiles": {"oli": "h-old"}}
    new = {"edge": 500, "edge_started": "new", "profiles": {"oli": "h-new"}}
    held.write_text(json.dumps(old))
    real_read = Path.read_text
    def read(path, *a, **kw):
        out = real_read(path, *a, **kw)
        if path == held:                            # the replacement's proxy writes now
            (bridge / "new-held").write_text(json.dumps(new))
            (bridge / "new-held").replace(held)
        return out
    with monkeypatch.context() as m:
        m.setattr(Path, "read_text", read)
        addon.done()
    assert json.loads(held.read_text()) == new
    assert sorted(p.name for p in bridge.glob("held.*")) == ["held.500.json"]   # nothing left aside
    addon.done()                                    # never ours from the start: not touched
    assert json.loads(held.read_text()) == new


def test_the_note_and_switch_both_skip_an_edge_record_naming_another_route(tmp_path, monkeypatch):
    """#846 kept beside #595: a record naming another route is a case twin's, not
    this route's edge. --switch's count (live_edge) and the fallback note's count
    (rc_route.route_edge) both skip it, so the note names no --switch that refuses."""
    import rc_route
    rc = _launcher()
    edges = tmp_path / "bridge" / "edges"
    edges.mkdir(parents=True)
    pem = tmp_path / "ca.pem"
    pem.write_text("ca")
    (edges / "GW.json").write_text(json.dumps(
        {"pid": os.getpid(), "port": 47101, "ca": str(pem), "route": "gw"}))
    monkeypatch.setattr(rc_route.socket, "create_connection", lambda *a, **k: contextlib.nullcontext())
    monkeypatch.setattr(rc, "EDGES", edges)
    assert rc.live_edge("GW") is None
    assert rc_route.switch_edge(edges) == (0, None)
    assert rc.switch_edge(edges, lambda f: rc.live_edge(f.stem)) == (0, None)
    note = rc_route.lost_endpoint_note("oli", edges.parent, os.getpid())
    assert "--switch" not in note and "(0 live)" in note


@pytest.mark.parametrize("edges, ca, start, mine, names", [
    pytest.param(1, True, True, True, True, id="this-proxys-edge-names-the-switch-that-lands"),
    pytest.param(1, True, True, False, False, id="another-edges-proxy-names-no-switch-issue-595"),
    pytest.param(1, False, True, True, False, id="ca-gone-names-no-switch-that-refuses-issue-595"),
    pytest.param(2, True, True, True, False, id="two-live-edges-name-no-switch-that-refuses-issue-595"),
    pytest.param(1, True, False, True, False, id="unreadable-edge-start-names-no-switch-that-refuses-issue-595"),
    pytest.param(1, True, True, "recycled", False,
                 id="proxy-of-an-earlier-process-at-the-edges-pid-names-no-switch-issue-595-review"),
    pytest.param(1, True, True, "unknown", False,
                 id="proxy-that-knows-no-edge-start-names-no-switch-issue-595-review"),
])
def test_the_note_names_switch_only_where_switch_lands(tmp_path, monkeypatch, edges, ca, start, mine, names):
    """issue #595: the turn's note named `ccmorph-rc <p> --switch` in cases that
    command refuses (no single live edge, a CA gone, a start it cannot read), or
    hands to another proxy than the one printing the note. It names the command
    only where it hands the endpoint to this very proxy, by the rule --switch uses:
    the edge's pid AND start, since a proxy whose edge's pid went to a replacement
    is never handed anything (issue #595 review)."""
    import rc_route
    rc = _launcher()
    if not start:
        monkeypatch.setattr(rc_route, "proc_started", lambda pid: None)
    bridge = tmp_path / "bridge"
    (bridge / "edges").mkdir(parents=True)
    pem = tmp_path / "ca.pem"
    if ca:
        pem.write_text("ca")
    for i in range(edges):
        (bridge / "edges" / f"e{i}.json").write_text(json.dumps(
            {"pid": os.getpid(), "port": 47101 + i, "ca": str(pem)}))
    monkeypatch.setattr(rc_route.socket, "create_connection", lambda *a, **k: contextlib.nullcontext())
    monkeypatch.setattr(rc, "STATE", bridge)
    monkeypatch.setattr(rc, "EDGES", bridge / "edges")
    monkeypatch.setattr(rc, "INFERENCE", bridge / "inference")
    monkeypatch.setattr(rc, "profile_type", lambda p: "apikey")
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-oli"})
    monkeypatch.setattr(rc, "retire_route_with", lambda *a: None)
    monkeypatch.setattr(rc, "get_tiers", lambda p: {})
    monkeypatch.setattr(rc, "sidecar", lambda p, owner, started: ("http://127.0.0.1:45111", 4321))
    monkeypatch.setattr(rc, "handoff_taken", proxy_took(rc))
    proxy_start = {"recycled": "an-earlier-start", "unknown": None}.get(mine, rc_route.proc_started(os.getpid()))
    note = rc_route.lost_endpoint_note("oli", bridge, os.getpid() if mine else 1, None, proxy_start)
    assert ("ccmorph-rc oli --switch" in note) is names
    if names:
        rc.switch("oli", "alex")
        assert (bridge / "inference").read_text() == "oli\n"
    else:
        assert "--switch" not in note
        if mine is True:                    # this edge's own proxy: the switch it was not told refuses
            with pytest.raises(SystemExit):
                rc.switch("oli", "alex")


def test_a_sessions_note_names_the_session_switch(tmp_path, monkeypatch):
    """A session's own word is repaired by `--switch --session <id>`; the shared
    switch would move every other host too."""
    import rc_route
    bridge = tmp_path / "bridge"
    (bridge / "edges").mkdir(parents=True)
    pem = tmp_path / "ca.pem"
    pem.write_text("ca")
    (bridge / "edges" / "gw.json").write_text(json.dumps({"pid": os.getpid(), "port": 47101, "ca": str(pem)}))
    monkeypatch.setattr(rc_route.socket, "create_connection", lambda *a, **k: contextlib.nullcontext())
    note = rc_route.lost_endpoint_note("oli", bridge, os.getpid(), "cse_01ABC",
                                       rc_route.proc_started(os.getpid()))
    assert "`ccmorph-rc oli --switch --session cse_01ABC` hands it over" in note
