import fcntl
import http.server
import json
import math
import os
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

if sys.version_info < (3, 10):   # the add-on needs 3.10; ccmorph itself still runs on 3.8
    pytest.skip("model-router needs Python 3.10+", allow_module_level=True)

ROOT = Path(__file__).resolve().parents[1] / "addons" / "router"
sys.path.insert(0, str(ROOT))

from model_router import classify, cli, core, simulate, sources  # noqa: E402

CAT = json.loads((ROOT / "model_router" / "catalogue.json").read_text())   # without the user's learned offsets
NOW = 1_800_000_000.0
IMG = {m: ("text", "image") for m in CAT["models"]}


@pytest.fixture
def home(tmp_path, monkeypatch):
    """An isolated config, cache and state for in-process CLI runs, with models.dev offline."""
    d = tmp_path.resolve()
    for name, value in {"MODEL_ROUTER_CONFIG": d / "config.json", "XDG_CONFIG_HOME": d, "XDG_CACHE_HOME": d,
                        "XDG_STATE_HOME": d, "CLAUDE_CONFIG_DIR": d / "work"}.items():
        monkeypatch.setenv(name, str(value))
    for name in [k for k in os.environ if k.startswith("ANTHROPIC_DEFAULT_")]:
        monkeypatch.delenv(name)
    monkeypatch.setattr(sources, "models_dev", lambda refresh=False: {})
    monkeypatch.chdir(d)
    return d


def task(kind="code_edit", level=2.0, needs=()):
    return core.Task({kind: 1.0}, level, set(needs))


def window(used, hours_left, name="5h", length=5 * 3600, cap=60.0):
    return core.Window(name, used, NOW + hours_left * 3600, length, cap)


def sub(name, used5=0.1, used7=0.1, h5=2.5, h7=84, models=("claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5")):
    return core.Profile(name, "subscription", list(models),
                        [window(used5, h5), window(used7, h7, "7d", 7 * 86400, 500.0)], {"5h": 0.15, "7d": 0.1})


def api(models=("claude-opus-5-5", "gpt-6.1-sol", "deepseek-v4-flash", "glm-5.3", "kimi-k3.1-pro", "gpt-6-luna")):
    return core.Profile("gateway", "api", list(models))


def pick(t, profiles, **policy):
    opts, dropped = core.options(t, profiles, CAT, IMG, {}, NOW, core.Policy(**policy))
    return core.choose(opts, core.Policy(**policy))[0], dropped


def test_quota_is_free_when_it_would_expire_and_dear_when_it_will_run_out():
    assert core.runout(window(0.2, 0.3), NOW, 0.15, 0.0) < 0.01          # 20 minutes left, most of it unused
    assert core.runout(window(0.6, 4.5), NOW, 0.15, 0.0) > 0.9           # 60% gone half an hour in
    assert core.runout(window(0.8, 2), NOW, 0.15, 0.1) == 1.0            # past the human's reserve


def test_window_prices_add_up_and_cap_at_the_api_price():
    p = sub("a", used5=0.55, used7=0.75, h5=4, h7=150)
    price = core.quota_price(p, "claude-opus-5-5", 1.0, NOW)
    assert 0.9 < price <= 1.0
    assert core.quota_price(sub("b", used5=0.7), "claude-opus-5-5", 20.0, NOW) is None   # does not fit


def test_subscription_first_and_the_emptier_one_wins():
    busy, idle = sub("busy", used5=0.5, h5=4.0), sub("idle", used5=0.05, h5=4.0)
    chosen, _ = pick(task(level=2), [busy, idle, api()])
    assert chosen.profile == "idle" and chosen.klass == "subscription"


def test_a_subscription_about_to_run_out_is_kept_for_the_human():
    nearly_out = sub("nearly", used5=0.6, h5=4.5)
    chosen, _ = pick(task(level=2), [nearly_out, api()])
    assert chosen.profile == "gateway"


def test_the_worked_example_in_the_algorithm_doc():
    work = sub("work", used5=0.62, h5=4.0, used7=0.4, h7=84)
    home = sub("home", used5=0.10, h5=1.0, used7=0.30, h7=20)
    opts, _ = core.options(task("code_edit", 2.0), [work, home, api()], CAT, IMG, {}, NOW, core.Policy())
    chosen = core.choose(opts, core.Policy())[0]
    assert core.quota_price(work, "claude-sonnet-5-5", 0.88, NOW) == 1.0
    assert {o.tier for o in opts if o.profile == "work"} == {len(core.Policy().priority)}   # kept for the human
    assert round(max(o.quality for o in opts), 3) == 0.988
    assert (chosen.profile, chosen.model, chosen.effort, chosen.cost_usd) == ("home", "claude-sonnet-5-5", "high", 0.88)
    assert (round(chosen.quality, 3), round(chosen.effective_usd, 3)) == (0.953, 0.018)


def test_api_first_when_the_user_ranks_it_first():
    chosen, _ = pick(task(level=2), [sub("idle", used5=0.05), api()], priority=("api", "subscription", "budget"))
    assert chosen.klass == "api"


def test_cheap_model_for_trivial_work_strong_one_for_very_hard_work():
    cheap, _ = pick(task("mechanical", 0.0), [api()])
    hard, _ = pick(task("analysis", 4.0), [api()])
    assert cheap.cost_usd < 0.05
    assert hard.model in ("claude-opus-5-5", "kimi-k3.1-pro", "gpt-6.1-sol") and hard.effort in ("high", "xhigh", "max")
    feasible, _ = core.options(task("analysis", 4.0), [api()], CAT, IMG, {}, NOW, core.Policy())
    assert hard.quality >= max(o.quality for o in feasible) - core.Policy().band


def test_effort_only_from_the_models_own_ladder():
    opts, dropped = core.options(task(), [api(("kimi-k3.1-pro",))], CAT, IMG, {}, NOW, core.Policy(), {"effort": "medium"})
    assert not opts and "effort medium not accepted" in dropped[0]
    opts, _ = core.options(task(), [api(("kimi-k3.1-pro",))], CAT, IMG, {}, NOW, core.Policy())
    assert {o.effort for o in opts} == {"low", "high", "max"}


def test_image_work_skips_text_only_models_and_dear_jobs_are_dropped():
    inputs = {**IMG, "glm-5.3": ("text",)}
    opts, dropped = core.options(task("vision", 1, ["vision"]), [api(("glm-5.3",))], CAT, inputs, {}, NOW, core.Policy())
    assert not opts and "cannot read images" in dropped[0]
    offline, _ = sources.model_facts(CAT, {})   # no models.dev copy yet: the catalogue's own input types
    opts, _ = core.options(task("vision", 1, ["vision"]), [api(("glm-5.3", "claude-sonnet-5-5"))], CAT, offline, {}, NOW,
                           core.Policy())
    assert {o.model for o in opts} == {"claude-sonnet-5-5"}
    opts, dropped = core.options(task("analysis", 4), [api(("claude-opus-5-5",))], CAT, IMG, {}, NOW, core.Policy(max_job_usd=5))
    assert all(o.cost_usd <= 5 for o in opts) and any("per-job limit" in d for d in dropped)


def test_keyword_rules_without_a_decider():
    t = classify.heuristic("Search Reddit and X for complaints about the new release")
    assert max(t.kinds, key=t.kinds.get) == "social_search" and "web" in t.needs
    assert classify.heuristic("rename usr to user").difficulty < classify.heuristic(
        "design the architecture of a distributed migration with security review").difficulty


def test_the_week_replay_saves_money_without_blocking_the_human():
    rows = {r["policy"]: r for r in (simulate.replay(n, 7, 7, CAT) for n in simulate.POLICIES)}
    router, opus = rows["model-router, balanced"], rows["one model (Opus, gateway)"]
    assert router["api_usd"] < 0.75 * opus["api_usd"]
    assert router["mean_quality"] > opus["mean_quality"] - 0.02
    assert router["human_blocked_10min"] == 0 and router["limit_hits"] == 0
    assert rows["account pool (round robin)"]["human_blocked_10min"] > 100
    assert rows["model-router, gateway only"]["api_usd"] < 0.95 * opus["api_usd"]   # model choice alone saves too


def test_discovery_reads_no_secrets(tmp_path, monkeypatch):
    (tmp_path / "work.conf").write_text("type=sub\napi_key=sk-secret\ntoken=tok-secret\n")
    (tmp_path / "gate.conf").write_text("base_url=https://gw.example\napi_key=sk-secret\nmodel=glm-5.3\n"
                                        "excluded_models=gpt-6-sol\ncontext_discovered={\"models\":{\"gpt-6-sol\":1,\"kimi-k3.1-pro\":2}}\n")
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(tmp_path))
    found = sources.discover()
    assert "secret" not in json.dumps(found)
    assert found["work"]["class"] == "subscription" and found["work"]["quota"] == "statusline"
    assert found["gate"]["class"] == "api" and found["gate"]["models"] == ["glm-5.3", "kimi-k3.1-pro"]


@pytest.mark.parametrize("prompt_args, shown", [
    (["list every TODO in src"], lambda d: ["<prompt>"]),
    (["--prompt-file", "task.md"], lambda d: ["--prompt-file", str(d / "task.md")]),   # the job runs in --cwd
], ids=["inline-prompt", "relative-prompt-file"])
def test_dispatch_dry_run_names_profile_model_and_effort(home, capsys, prompt_args, shown):
    cfg = {"profiles": {"gateway": {"class": "api", "models": ["deepseek-v4-flash-0731", "deepseek-v4.1-flash"]}},
           "decider": {"url": None}, "executor": "agent-dispatch", "agent_dispatch": "/bin/echo"}
    (home / "config.json").write_text(json.dumps(cfg))
    (home / "task.md").write_text("list every TODO in src")
    argv = ["dispatch", "--dry-run", "--agent", "worker", "--cwd", "repo", *prompt_args, "--", "--permission-mode", "acceptEdits"]
    assert cli.main(argv) == 0
    cmd = capsys.readouterr().out.split()
    end = cmd.index("--")
    assert cmd[:6] == ["/bin/echo", "run", "--profile", "gateway", "--model", "deepseek-v4.1-flash"]   # the gateway's own id
    assert cmd[cmd.index("--cwd") + 1] == str(home / "repo")   # absolute: it is also the executor's own cwd
    assert cmd[end - len(shown(home)):end] == shown(home)
    assert cmd[end + 1:] == ["--agent", "worker", "--permission-mode", "acceptEdits"]


def lock_held() -> bool:
    with (sources.state_dir() / "ledger.lock").open("a") as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return True
        fcntl.flock(f, fcntl.LOCK_UN)
        return False


class Started:
    def __init__(self, cmd, **_):
        self.cmd = cmd

    def wait(self):
        return 0


@pytest.mark.parametrize("act", [
    lambda: cli.native_route({"prompt": "Rename usr to user in src/", "subagent_type": "general-purpose"}) is not None,
    lambda: cli.main(["dispatch", "Rename usr to user in src/"]) == 0,
], ids=["hook", "dispatch"])
def test_the_pick_and_its_reservation_happen_under_one_lock(home, monkeypatch, act):
    cfg = {"profiles": {"work": {"class": "api", "models": ["claude-sonnet-5-5", "deepseek-v4.1-flash"]}},
           "decider": {"url": None}, "executor": "agent-dispatch", "agent_dispatch": "/bin/echo"}
    (home / "config.json").write_text(json.dumps(cfg))
    seen, build, append = [], sources.build_profiles, sources.ledger_append
    monkeypatch.setattr(sources, "build_profiles", lambda *a, **k: (seen.append(lock_held()), build(*a, **k))[1])
    monkeypatch.setattr(sources, "ledger_append", lambda row: (seen.append(lock_held()), append(row))[1])
    monkeypatch.setattr(cli.subprocess, "Popen", Started)
    assert act()
    assert seen == [True, True]


def test_a_job_that_cannot_start_reserves_nothing(home):
    cfg = {"profiles": {"gateway": {"class": "api", "models": ["deepseek-v4.1-flash"]}},
           "decider": {"url": None}, "executor": "agent-dispatch", "agent_dispatch": str(home / "missing")}
    (home / "config.json").write_text(json.dumps(cfg))
    with pytest.raises(SystemExit, match="could not start"):
        cli.main(["dispatch", "Rename usr to user in src/"])
    assert sources.ledger_rows() == []


def test_statusline_reading_is_kept_and_a_passed_reset_starts_the_window_empty(home):
    now = time.time()
    payload = {"rate_limits": {"five_hour": {"used_percentage": 42, "resets_at": now - 60},
                               "seven_day": {"used_percentage": 30, "resets_at": "2099-01-01T00:00:00Z"}}}
    assert sources.record_statusline({"rate_limits": {}}, "work") is None
    sources.record_statusline(payload, "work")
    entry = {"class": "subscription", "quota": "statusline", "plan": "max_5x", "models": ["claude-sonnet-5-5"]}
    windows = {w.name: w for w in sources.build_profiles({"profiles": {"work": entry}}, CAT)[0][0].windows}
    assert windows["5h"].used == 0.0 and now < windows["5h"].resets_at <= now + 5 * 3600
    assert windows["7d"].used == 0.3
    assert (windows["5h"].capacity_usd, windows["7d"].capacity_usd) == (60, 500)   # sized by the plan, not as Pro
    assert sources.quota("other", {"class": "subscription", "quota": "statusline"})["windows"] == []


def test_a_redraw_with_the_same_use_keeps_work_routed_since_pending(home):
    payload = {"rate_limits": {"five_hour": {"used_percentage": 42}}}
    first = sources.record_statusline(payload, "work")
    sources.ledger_append({"id": "job", "ts": time.time(), "profile": "work", "cost_usd": 7.0})
    assert sources.record_statusline(payload, "work")["as_of"] == first["as_of"]
    entry = {"class": "subscription", "quota": "statusline", "models": ["claude-sonnet-5-5"]}
    assert sources.build_profiles({"profiles": {"work": entry}}, CAT)[0][0].pending_usd == 7.0


def test_a_failed_refresh_still_ages_the_last_reading(home, monkeypatch):
    old = {"as_of": 1.0, "checked": 1.0, "windows": [{"name": "5h", "used": 0.9, "resets_at": time.time() - 60,
                                                      "length": 5 * 3600, "capacity_usd": 10, "models": []}]}
    sources.write_json(sources.quota_path("cx"), old)
    monkeypatch.setitem(sources.READERS, "codex", lambda name: {"note": "endpoint down"})
    q = sources.quota("cx", {"class": "subscription", "quota": "codex"}, refresh=True)
    assert q["windows"][0]["used"] == 0.0 and q["note"] == "endpoint down"


@pytest.mark.parametrize("read, body", [
    (lambda b: sources.read_anthropic("p"), []),
    (lambda b: sources.read_anthropic("p"), {"five_hour": {"utilization": 50, "resets_at": "bad-date"}}),
    (lambda b: sources.read_anthropic("p"), {"five_hour": {"utilization": float("nan")}}),
    (lambda b: sources.read_codex("p"), []),
    (lambda b: sources.read_codex("p"), {"rate_limit": {"primary_window": {"limit_window_seconds": 18000,
                                                                            "used_percent": 5, "reset_at": "soon"}}}),
    (lambda b: sources.record_statusline(b, "p"), []),
    (lambda b: sources.record_statusline(b, "p"), {"rate_limits": {"five_hour": {"used_percentage": 5, "resets_at": "x"}}}),
], ids=["anthropic-list", "anthropic-bad-date", "anthropic-nan", "codex-list", "codex-bad-date",
        "statusline-list", "statusline-bad-date"])
def test_malformed_quota_answers_never_raise(home, monkeypatch, read, body):
    monkeypatch.setattr(sources, "anthropic_login", lambda name: {"accessToken": "token"})
    monkeypatch.setattr(sources, "codex_login", lambda name: {"token": "token", "account": "account"})
    monkeypatch.setattr(sources, "_get", lambda url, headers, timeout=8: body)
    got = read(body) or {}
    assert all(math.isfinite(w["used"]) and math.isfinite(w["resets_at"]) for w in got.get("windows", []))


def test_a_bearer_token_never_follows_a_redirect(monkeypatch):
    seen = []

    class Hop(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            seen.append((self.path, self.headers.get("authorization")))
            self.send_response(302)
            self.send_header("location", "/elsewhere")
            self.end_headers()

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Hop)
    threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True).start()
    monkeypatch.setattr(sources, "tls", ssl.create_default_context)
    try:
        with pytest.raises(urllib.error.HTTPError) as answer:
            sources._get(f"http://127.0.0.1:{server.server_port}/usage", {"authorization": "Bearer token"})
    finally:
        server.shutdown()
        server.server_close()
    assert answer.value.code == 302 and seen == [("/usage", "Bearer token")]


def test_x_posts_go_to_the_model_with_x_search_and_are_flagged_without_one():
    t = classify.heuristic("Search X for what people said about the launch and summarise the complaints")
    assert "x" in t.needs and "web" in t.needs
    assert "x" not in classify.heuristic("Fix the box layout on the settings page").needs
    with_grok, dropped = pick(t, [sub("idle", used5=0.05), api(("grok-4.7", "gpt-6.1-sol"))])
    assert with_grok.model == "grok-4.7" and any("no native X search" in d for d in dropped)
    without, _ = pick(t, [sub("idle", used5=0.05), api(("gpt-6.1-sol",))])
    assert without is not None and without.model != "grok-4.7"
    unusable_grok, _ = core.options(t, [api(("grok-4.7", "claude-haiku-5-5"))], CAT, IMG, {}, NOW, core.Policy(),
                                    {"effort": "max"})   # Grok has no max rung
    assert {o.model for o in unusable_grok} == {"claude-haiku-5-5"}


def run_hook(tmp_path, event, extra_env=None, profile=None):
    cfg = {"profiles": {"work": profile or {"class": "api", "models": []}}, "decider": {"url": None}}
    (tmp_path / "config.json").write_text(json.dumps(cfg))
    env = {k: v for k, v in os.environ.items() if not k.startswith("ANTHROPIC_DEFAULT_")}
    env.update({"MODEL_ROUTER_CONFIG": str(tmp_path / "config.json"), "XDG_CONFIG_HOME": str(tmp_path),
                "XDG_CACHE_HOME": str(tmp_path), "XDG_STATE_HOME": str(tmp_path),
                "CLAUDE_CONFIG_DIR": str(tmp_path / "work"), **(extra_env or {})})
    out = subprocess.run([sys.executable, "-m", "model_router", "hook"], input=json.dumps(event) if isinstance(event, dict) else event,
                         cwd=ROOT, env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert out.returncode == 0, out.stderr
    return json.loads(out.stdout)["hookSpecificOutput"] if out.stdout.strip() else None


def test_hook_fills_model_and_effort_on_native_agent_calls_and_never_blocks(tmp_path):
    call = {"tool_name": "Agent", "tool_input": {"description": "rename", "prompt": "Rename usr to user in every file under src/",
                                                 "subagent_type": "general-purpose"}}
    got = run_hook(tmp_path, call)
    assert got["updatedInput"]["model"] in ("haiku", "sonnet", "opus") and got["updatedInput"]["effort"]
    assert got["updatedInput"]["prompt"] == call["tool_input"]["prompt"] and "model-router ran" in got["additionalContext"]
    hard = {"tool_name": "Agent", "tool_input": {"prompt": "Design the architecture of a distributed database migration "
                                                 "with security review", "subagent_type": "Plan"}}
    assert run_hook(tmp_path, hard)["updatedInput"]["model"] in ("sonnet", "opus")
    gateway = run_hook(tmp_path, call, {"ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-v4-flash"})
    assert gateway["updatedInput"]["model"] in ("haiku", "sonnet", "opus")
    pinned = {"tool_name": "Agent", "tool_input": {**call["tool_input"], "model": "opus"}}
    assert run_hook(tmp_path, pinned)["updatedInput"]["model"] == "opus"
    unroutable = {"tool_name": "Agent", "tool_input": {**call["tool_input"], "model": "fable"}}   # no fable model here
    assert run_hook(tmp_path, unroutable) is None
    assert run_hook(tmp_path, {"tool_name": "Agent", "tool_input": {**call["tool_input"], "subagent_type": "fork"}}) is None
    assert run_hook(tmp_path, {"tool_name": "Bash", "tool_input": {"command": "ls"}}) is None
    assert run_hook(tmp_path, "not json") is None


HARD = "Design the architecture of a distributed database migration with security review"
EASY = "Rename usr to user in every file under src/"


@pytest.mark.parametrize("profile, env, prompt, named, allowed", [
    ({"class": "api", "models": [], "enabled": False}, {}, EASY, None, {None}),
    ({"class": "api", "models": ["deepseek-v4.1-flash"]}, {"ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-v4-flash"}, HARD, None, {"haiku"}),
    ({"class": "api", "models": [], "disabled_models": ["claude-sonnet-5-5"]}, {}, EASY, None, {"haiku", "opus"}),
    ({"class": "api", "models": []}, {"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-opus-5-5"}, EASY, "sonnet", {"sonnet"}),
], ids=["disabled-profile", "only-models-the-profile-serves", "disabled-model", "named-alias-sharing-a-model"])
def test_hook_routes_only_within_what_the_profile_allows(tmp_path, profile, env, prompt, named, allowed):
    call = {"tool_name": "Agent", "tool_input": {"prompt": prompt, "subagent_type": "general-purpose",
                                                 **({"model": named} if named else {})}}
    got = run_hook(tmp_path, call, env, profile)
    assert (got["updatedInput"]["model"] if got else None) in allowed


def test_recorder_and_hook_agree_on_the_session_profile(monkeypatch):
    for config_dir, name in ((None, "default"), ("/home/a/.claude", "default"), ("/home/a/profiles/work", "work")):
        if config_dir:
            monkeypatch.setenv("CLAUDE_CONFIG_DIR", config_dir)
        else:
            monkeypatch.delenv("CLAUDE_CONFIG_DIR", raising=False)
        assert sources.session_profile() == name


def test_init_keeps_the_users_settings(home, monkeypatch):
    (home / "ccmorph").mkdir()
    (home / "ccmorph" / "gate.conf").write_text("model=glm-5.3\n")
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(home / "ccmorph"))
    old = {"class": "api", "models": ["gone"], "price_factor": 0.1, "enabled": False}
    (home / "config.json").write_text(json.dumps({"profiles": {"gate": old}}))
    assert cli.main(["init"]) == 0
    gate = json.loads((home / "config.json").read_text())["profiles"]["gate"]
    assert (gate["price_factor"], gate["enabled"], gate["models"]) == (0.1, False, ["glm-5.3"])


@pytest.mark.parametrize("argv", [["route", "--kind", "typo", "x"], ["route", "--difficulty", "nan", "x"]],
                         ids=["unknown-kind", "nan-difficulty"])
def test_bad_task_arguments_are_refused_up_front(home, argv):
    with pytest.raises(SystemExit) as refused:
        cli.main(argv)
    assert refused.value.code == 2


def test_route_reads_a_prompt_given_after_double_dash(home, capsys):
    (home / "config.json").write_text(json.dumps({"profiles": {"gateway": {"class": "api", "models": ["glm-5.3"]}},
                                                  "decider": {"url": None}}))
    assert cli.main(["route", "--json", "--", "Fix the security race"]) == 0
    got = json.loads(capsys.readouterr().out)["task"]["difficulty"]
    assert got == round(classify.heuristic("Fix the security race").difficulty, 2)
