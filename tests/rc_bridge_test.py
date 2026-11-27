"""Remote Control launcher: credential isolation, edge handoffs and the proxy's start. The
request routing is the Go edge proxy's (agentproxy/edge*_test.go)."""
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

OAUTH = "sk-ant-oat01-ACCOUNT-SECRET"


















def _turn(header_tail, messages):
    """One first-party turn: the billing header, a stable system prompt, the turn's messages."""
    return {"model": "claude-opus-5-5", "tools": [{"name": "t"}],
            "system": [{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.302.a1b;"
                                                " cc_entrypoint=sdk-cli;" + header_tail},
                       {"type": "text", "text": "You are Claude Code.",
                        "cache_control": {"type": "ephemeral", "ttl": "1h", "scope": "global"}}],
            "messages": messages}










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
    # The session loses every proxy (its one proxy is the edge proxy), but the
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










ADDON = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "rc_addon.py"




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
    """The edge proxy the launcher watches at startup: dead at once, or alive."""

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
    pytest.param("no-binary", "not found", id="edge-proxy-binary-missing"),
    pytest.param("proxy-exits", "proxy exited during startup", id="stub-proxy-dies-at-once"),
    pytest.param("no-ca", "proxy CA not generated", id="ca-never-generated"),
    pytest.param("key-unconsumed", "did not consume the route key", id="proxy-never-reads-the-key"),
])
def test_no_failed_start_ever_leaves_the_route_key_behind(tmp_path, monkeypatch, capsys, shape, message):
    """issue #777: the key file's only unlink lived in the proxy, so every start
    that died after `mkstemp` — missing binary, dead proxy, missing CA, and the
    very path that prints "refusing to continue" — left the profile's real API key
    in STATE. This is the sweep: after any failing start, no route-key.* survives."""
    import argparse
    rc = _edge_spawn_state(tmp_path, monkeypatch)
    if shape == "no-binary":
        monkeypatch.setattr(rc, "ADAPTER", tmp_path / "no-bin" / "ccmorph-core")
    else:
        stub = tmp_path / "bin" / "ccmorph-core"                     # a core that never really runs
        stub.parent.mkdir()
        stub.write_text("#!/bin/sh\nexit 0\n")
        stub.chmod(0o755)
        monkeypatch.setattr(rc, "ADAPTER", stub)
        if shape == "proxy-exits":                                   # wait_listening stays real
            _stub_proxy_spawn(rc, monkeypatch, 1)
        else:                                                        # listens; the key is the question
            _stub_proxy_spawn(rc, monkeypatch, None)
            monkeypatch.setattr(rc, "wait_listening", lambda *a, **k: None)
            if shape == "key-unconsumed":
                ca = rc.STATE / "edge-ca" / "ca.pem"                # a proxy that did come up
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
    dead_addon = rc.STATE / f"addon.{gone.pid}.abc"       # a copy an older version made
    dead_addon.mkdir()
    (dead_addon / "old.py").write_text("")
    live = rc.STATE / f"{rc.owned_prefix('route-key')}abc"
    live.write_text("in use")
    rc.sweep_orphans(rc.STATE)
    assert not dead_key.exists() and not dead_addon.exists()
    assert live.exists()


# GH-932: each row is one HANG_TIMEOUT (120 s) Python child whose import, proxy
# spawn and SIGTERM teardown all run under the gate's scheduler; the night gate
# struck the [sigterm-handle-with-no-stop-api] row, where that child was the
# whole test. Per-test headroom for every row; the ceiling stays 60 s.
@pytest.mark.timeout(300)
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
rc.ADAPTER = __import__("pathlib").Path(path)           # any existing file: the proxy spawn is stubbed
class Proxy:
    returncode = None
    def poll(self): return self.returncode
    def terminate(self): self.returncode = -15
    def kill(self): self.returncode = -9
    def wait(self, timeout=None): return self.returncode
proxy = Proxy()
def spawn(cmd, env=None, **kw):
    os.unlink(env["RC_KEY_FILE"])                     # the proxy read the key and removed it
    (rc.STATE / "edge-ca" / "ca.pem").write_text("ca")
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
    pytest.param("key-unconsumed", "did not consume the route key", id="proxy-never-reads-the-key"),
    pytest.param("healthy", None, id="healthy-start-keeps-its-proxy"),
])
def test_a_failed_start_stops_the_proxy_it_spawned(tmp_path, monkeypatch, capsys, shape, message):
    """issue #777: a failed start must not leave the proxy it spawned running: it
    would outlive us holding its port (on --edge, the fixed one every restart
    needs). The start that gives up stops the proxy it spawned; a healthy one keeps it."""
    import argparse
    import os
    import subprocess

    rc = _edge_spawn_state(tmp_path, monkeypatch)
    stub = tmp_path / "bin" / "ccmorph-core"
    stub.parent.mkdir()
    stub.write_text("#!/bin/sh\nexit 0\n")
    stub.chmod(0o755)
    monkeypatch.setattr(rc, "ADAPTER", stub)
    real = rc.subprocess.Popen
    spawned = []

    def spawn(cmd, env=None, **kw):
        if cmd[0] == "zsh":
            return real(cmd, env=env, **kw)
        if shape == "healthy":
            os.unlink(env["RC_KEY_FILE"])                            # the proxy consumed it
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
        ca = rc.STATE / "edge-ca" / "ca.pem"
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




def _routed_port(addon):
    flow = _Flow()
    addon.requestheaders(flow)
    return flow.request.port
























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










# Cross-profile subagents: the router's PreToolUse leaves a pick under the Agent
# prompt's key; the proxy binds the subagent whose first request carries that
# prompt, and sends every request of that agent to the pick.



def _first_request(prompt):
    return {"model": "claude-sonnet-5-5", "messages": [{"role": "user", "content": [
        {"type": "text", "text": "<system-reminder>context</system-reminder>"},
        {"type": "text", "text": prompt}]}]}














def _agent_flow(agent, body, session=None):
    flow = _Flow()
    if agent:
        flow.request.headers["x-claude-code-agent-id"] = agent
    if session:
        flow.request.headers["x-claude-code-session-id"] = session
    flow.request.content = json.dumps(body).encode()
    return flow




def test_a_rehand_holds_a_profile_without_a_word(tmp_path, monkeypatch):
    """The router asks the edge to hold a picked profile with --switch --rehand:
    the proxy gets the endpoint, and no host's word changes."""
    rc, _ = _switch_state(tmp_path, monkeypatch)
    rc.switch("gw", "me", rehand=True)
    assert (rc.STATE / "switch-gw.json").exists()
    assert not (rc.STATE / "inference").exists()
