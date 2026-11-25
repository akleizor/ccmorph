"""Remote Control bridge add-on: routing decision and credential isolation."""
import contextlib
import json
import re
import signal
import socket
import sys
import time
from pathlib import Path

import pytest

from support import HANG_TIMEOUT, load_script

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
    route_env = {"ANTHROPIC_BASE_URL": "https://gw", "ANTHROPIC_API_KEY": "route-key",
                 "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5-5[1m]",
                 "CCMORPH_ROUTE_HANDLE": "provider|1|/t|/a"}
    monkeypatch.setattr(rc, "resolve_route", lambda profile: route_env)
    monkeypatch.setattr(rc, "retire_route_with", lambda pid, handle: None)
    monkeypatch.setattr(rc, "adapter_env", lambda: {})
    monkeypatch.setattr(rc, "start_adapter", lambda profile, owner, env: "http://127.0.0.1:54505")
    monkeypatch.setattr(rc, "get_tiers", lambda profile: {"sonnet": "claude-sonnet-5-5"})
    monkeypatch.setattr(rc, "handoff_taken", lambda hand, payload: True)   # the edge's proxy took it
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
    pytest.param(None, 60111, "was never brought up", id="never-brought-up-keeps-the-route"),
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
    if k.get("prefix") == "route-key.":
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
    for _ in range(50):
        if not hand.exists():
            break
        time.sleep(0.1)
    assert not hand.exists()
    assert addon.SWITCHED["gw"] == ("http://127.0.0.1:54505", "k2", {})


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
