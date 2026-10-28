"""model-router: pick the profile, model and effort for a task, then hand it to a dispatcher."""
from __future__ import annotations

import argparse
import json
import math
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import time

from . import classify, core, sources

CATALOGUE = Path(__file__).with_name("catalogue.json")
KINDS = sorted(json.loads(CATALOGUE.read_text())["kinds"])


def load_catalogue() -> dict:
    cat = json.loads(CATALOGUE.read_text())
    extra = sources.read_json(sources.config_dir() / "skills.json", {}) or {}
    for model, shifts in extra.items():   # learned offsets from `feedback`
        if model in cat["models"]:
            adjust = cat["models"][model].setdefault("adjust", {})
            for kind, delta in shifts.items():
                if kind in cat["kinds"]:
                    adjust[kind] = adjust.get(kind, cat.get("default_adjust", {}).get(kind, 0.0)) + delta
    return cat


def policy(cfg: dict, mode: str | None = None) -> core.Policy:
    preset = core.MODES[mode or cfg.get("mode", "balanced")]
    return core.Policy(tuple(cfg["priority"]), float(cfg.get("quality_target", preset["quality_target"])),
                       float(cfg.get("band", preset["band"])), float(cfg["protect_above"]),
                       max_job_usd=float(cfg.get("max_job_usd", 15.0)))


def read_prompt(args) -> str:
    if args.prompt_file:
        return Path(args.prompt_file).read_text()
    if args.prompt:
        return " ".join(args.prompt)
    if not sys.stdin.isatty():
        return sys.stdin.read()
    raise SystemExit("model-router: give the task as text, --prompt-file, or on stdin")


def task_for(args, prompt: str, cat: dict, cfg: dict) -> tuple[core.Task, list]:
    notes = []
    if args.kind and args.difficulty is not None:
        task = core.Task({args.kind: 1.0}, min(max(args.difficulty, 0.0), 4.0), source="given")
    else:
        url = None if args.no_decider else (cfg.get("decider") or {}).get("url")
        task, note = classify.classify(prompt, cat, url)
        if note:
            notes.append(note)
        if args.kind:
            task.kinds = {args.kind: 1.0}
        if args.difficulty is not None:
            task.difficulty = min(max(args.difficulty, 0.0), 4.0)
    if args.needs:
        task.needs |= set(args.needs.split(","))
    if args.context_tokens:
        task.context_tokens = args.context_tokens
    return task, notes


def decide(args, prompt: str) -> dict:
    cfg = sources.load_config()
    if not cfg.get("profiles"):
        raise SystemExit("model-router: no profiles configured; run `model-router init` first")
    cat = load_catalogue()
    inputs, contexts = sources.model_facts(cat, sources.models_dev())
    task, notes = task_for(args, prompt, cat, cfg)
    profiles, quota_notes = sources.build_profiles(cfg, cat, refresh=args.refresh)
    notes += [f"{p}: {n}" for p, n in quota_notes.items()]
    pin = {k: getattr(args, k) for k in ("profile", "model", "effort") if getattr(args, k)}
    if pin.get("model"):
        pin["model"] = sources.resolve(cat, pin["model"]) or pin["model"]
    pol = policy(cfg, args.mode)
    opts, dropped = core.options(task, profiles, cat, inputs, contexts, time.time(), pol, pin)
    pick, shortlist = core.choose(opts, pol)
    if pick and "x" in task.needs and "x_search" not in cat["models"][pick.model].get("tools", ()):
        notes.append("no routed model can search X natively (Grok's x_search); the pick has only web search")
    level = cat["difficulty"]["levels"][min(int(round(task.difficulty)), 4)]
    ids = next((p.ids for p in profiles if pick and p.name == pick.profile), {})
    return {
        "pick": {**vars(pick), "model_id": ids.get(pick.model, pick.model)} if pick else None,
        "task": {"kinds": {k: round(p, 3) for k, p in sorted(task.kinds.items(), key=lambda x: -x[1])},
                 "difficulty": round(task.difficulty, 2), "level": level, "needs": sorted(task.needs),
                 "source": task.source,
                 "self_contained": None if task.self_contained is None else round(task.self_contained, 2)},
        "shortlist": [vars(o) for o in shortlist[:8]],
        "considered": len(opts), "dropped": dropped, "notes": notes,
        "policy": {"priority": list(pol.priority), "quality_target": pol.quality_target, "band": pol.band},
    }


def fmt_option(o: dict) -> str:
    effort = o["effort"] or "-"
    return (f"{o['profile']:<10} {o['model']:<20} {effort:<7} {o['klass']:<12} "
            f"{o['quality']:.2f}  ${o['cost_usd']:<7.2f} {o['runout']:.2f}  ${o['effective_usd']:.3f}")


def show(decision: dict, explain: bool) -> None:
    t, p = decision["task"], decision["pick"]
    kinds = ", ".join(f"{k} {v:.0%}" for k, v in t["kinds"].items())
    if not p:
        print("no option fits this task; see the reasons below (or relax --profile/--model/--effort)")
    else:
        print(f"→ {p['profile']} · {p['model']} · effort {p['effort'] or 'default'}")
        print(f"  quality {p['quality']:.2f} (target {decision['policy']['quality_target']:.2f}) · "
              f"est ${p['cost_usd']:.2f} API-equivalent · quota price {p['runout']:.2f} · class {p['klass']}")
    print(f"  task: {kinds} · {t['level']} ({t['difficulty']}) · via {t['source']}"
          + (f" · needs {', '.join(t['needs'])}" if t["needs"] else ""))
    for note in decision["notes"]:
        print(f"  note: {note}")
    if explain or not p:
        print("\n  profile    model                effort  class        qual  est $    quota eff $")
        for o in decision["shortlist"]:
            print("  " + fmt_option(o))
        if decision["dropped"]:
            print(f"\n  dropped {len(decision['dropped'])}:")
            for d in decision["dropped"][:12]:
                print(f"    {d}")


def level(text: str) -> float:
    value = float(text)
    if not math.isfinite(value):
        raise argparse.ArgumentTypeError("difficulty must be a number from 0 to 4")
    return value


def add_task_args(p: argparse.ArgumentParser) -> None:
    p.add_argument("prompt", nargs="*")
    p.add_argument("--prompt-file")
    p.add_argument("--profile", help="pin the profile")
    p.add_argument("--model", help="pin the model")
    p.add_argument("--effort", help="pin the effort")
    p.add_argument("--kind", choices=KINDS, help="skip classification of the kind of work")
    p.add_argument("--difficulty", type=level, help="0 trivial .. 4 very hard")
    p.add_argument("--needs", help="comma list: vision,web,x")
    p.add_argument("--context-tokens", type=int, help="context the task needs")
    p.add_argument("--mode", choices=sorted(core.MODES), help="quality, balanced (default) or cost")
    p.add_argument("--no-decider", action="store_true", help="keyword rules only")
    p.add_argument("--refresh", action="store_true", help="read quotas now, not from the 5-minute cache")
    p.add_argument("--json", action="store_true")


def cmd_route(args) -> int:
    decision = decide(args, read_prompt(args))
    if args.json:
        print(json.dumps(decision, indent=2))
    else:
        show(decision, args.explain)
    return 0 if decision["pick"] else 3


def executor_command(cfg: dict, pick: dict, args, prompt: str) -> list:
    extra = list(args.passthrough or [])
    if args.agent:
        extra = ["--agent", args.agent, *extra]
    if cfg.get("executor", "agent-dispatch") == "agent-dispatch":
        tool = cfg.get("agent_dispatch") or shutil.which("agent-dispatch")
        if not tool:
            raise SystemExit("model-router: agent-dispatch not found; set \"agent_dispatch\" in the config or \"executor\": \"ccmorph\"")
        cmd = [tool, "run", "--profile", pick["profile"], "--model", pick["model_id"]]
        cmd += ["--effort", pick["effort"]] if pick["effort"] else []
        cmd += ["--timeout", str(args.timeout)] + (["--cwd", args.cwd] if args.cwd else [])
        cmd += ["--prompt-file", str(Path(args.prompt_file).resolve())] if args.prompt_file else [prompt]   # the job runs in --cwd
        return cmd + (["--", *extra] if extra else [])
    cmd = ["ccmorph", "run", pick["profile"], "--model", pick["model_id"]]
    cmd += ["--effort", pick["effort"]] if pick["effort"] else []
    return cmd + extra + ["-p", prompt]


def cmd_dispatch(args) -> int:
    prompt = read_prompt(args)
    args.cwd = str(Path(args.cwd).resolve()) if args.cwd else None   # used twice: by the executor and as its cwd
    with sources.ledger_lock():   # read pending work and reserve this pick as one step
        decision = decide(args, prompt)
        pick = decision["pick"]
        if not pick:
            show(decision, True)
            return 3
        cmd = executor_command(sources.load_config(), pick, args, prompt)
        job = time.strftime("mr-%Y%m%d-%H%M%S-") + secrets.token_hex(3)
        print(f"model-router {job}: {pick['profile']} · {pick['model_id']} · effort {pick['effort'] or 'default'} "
              f"(quality {pick['quality']:.2f}, est ${pick['cost_usd']:.2f}, {decision['task']['level']} "
              f"{next(iter(decision['task']['kinds']))})", file=sys.stderr, flush=True)
        if args.dry_run:
            print(" ".join(c if c != prompt else "<prompt>" for c in cmd))
            return 0
        try:
            proc = subprocess.Popen(cmd, env={**os.environ, "CCMORPH_TASK": job}, cwd=args.cwd)   # ccmorph tags receipts with it
        except OSError as e:
            raise SystemExit(f"model-router: could not start {cmd[0]}: {e.strerror}") from None
        sources.ledger_append({"id": job, "ts": time.time(), "profile": pick["profile"], "model": pick["model"],
                               "model_id": pick["model_id"], "effort": pick["effort"], "class": pick["klass"],
                               "cost_usd": round(pick["cost_usd"], 4), "quality": round(pick["quality"], 3),
                               "task": decision["task"]})
    return proc.wait()


def cmd_init(args) -> int:
    cfg = sources.read_json(sources.config_path(), {}) or {}
    found = sources.discover()
    if not found:
        raise SystemExit(f"model-router: no ccmorph profiles in {sources.ccmorph_dir()}")
    profiles = {} if args.force else cfg.get("profiles", {})
    for name, entry in found.items():
        old = profiles.get(name, {})   # discovery owns only models and source; every other setting is the user's
        profiles[name] = {**entry, **{k: v for k, v in old.items() if k not in ("models", "source")},
                          "enabled": old.get("enabled", True)}
    cfg = {**{k: v for k, v in sources.DEFAULTS.items() if k != "profiles"}, **cfg, "profiles": profiles}
    sources.write_json(sources.config_path(), cfg)
    sources.models_dev(refresh=True)
    print(f"wrote {sources.config_path()} with {len(profiles)} profiles; `model-router profiles` to review")
    return 0


def cmd_profiles(args) -> int:
    cfg = sources.load_config()
    cat = load_catalogue()
    print("profile    on   class         quota      models")
    for name, e in cfg.get("profiles", {}).items():
        rated = [sources.resolve(cat, m) for m in e.get("models", [])]
        rated = sorted({m for m in rated if m and m not in e.get("disabled_models", [])})
        on = "yes" if e.get("enabled", True) else "no"
        shown = ", ".join(rated[:6]) + (f" +{len(rated) - 6}" if len(rated) > 6 else "")
        print(f"{name:<10} {on:<4} {e.get('class', 'api'):<13} {e.get('quota', 'none'):<10} {shown or '(none rated)'}")
    return 0


def cmd_toggle(args) -> int:
    cfg = sources.read_json(sources.config_path(), {}) or {}
    for name in args.names:
        if name not in cfg.get("profiles", {}):
            raise SystemExit(f"model-router: no profile {name!r}; run `model-router init` to pick up new ones")
        cfg["profiles"][name]["enabled"] = args.cmd == "enable"
    sources.write_json(sources.config_path(), cfg)
    return cmd_profiles(args)


def cmd_quota(args) -> int:
    cfg = sources.load_config()
    cat = load_catalogue()
    profiles, notes = sources.build_profiles(cfg, cat, refresh=args.refresh)
    now = time.time()
    rows = []
    for p in profiles:
        if p.klass == "api":
            continue
        for w in p.windows:
            price = core.runout(w, now, p.reserve.get(w.name, 0.0), 0.0)
            rows.append({"profile": p.name, "window": w.name, "used": round(w.used, 3),
                         "resets_in_h": round((w.resets_at - now) / 3600, 1), "quota_price": round(price, 3),
                         "pending_usd": round(p.pending_usd, 2)})
        if not p.windows:
            rows.append({"profile": p.name, "window": None, "note": notes.get(p.name, "quota unknown")})
    if args.json:
        print(json.dumps(rows, indent=2))
        return 0
    print("profile    window   used   resets in  quota price")
    for r in rows:
        if r.get("window"):
            print(f"{r['profile']:<10} {r['window']:<8} {r['used']:>5.0%}  {r['resets_in_h']:>6.1f} h   {r['quota_price']:.2f}")
        else:
            print(f"{r['profile']:<10} -        {r['note']}")
    return 0


def cmd_record(args) -> int:
    """Statusline side-car: keep the rate_limits Claude Code passes on stdin, print nothing, never fail."""
    try:
        name = args.profile or sources.session_profile()
        if sources.NAME.fullmatch(name):
            sources.record_statusline(json.loads(sys.stdin.read() or "{}"), name)
    except Exception:   # noqa: BLE001 -- a recorder error must not break the user's status line
        pass
    return 0


ALIASES = (("opus", "claude-opus-5-5"), ("sonnet", "claude-sonnet-5-5"), ("haiku", "claude-haiku-5-5"), ("fable", None))


def native_route(tool_input: dict) -> tuple[dict, str] | None:
    """(Agent tool input with model and effort filled in, one line for the main agent), or None to leave the call alone.

    A native subagent stays on this session's account, so only the session's own
    model aliases are candidates, priced on this profile's quota.
    """
    cfg = sources.load_config()
    cat = load_catalogue()
    name = sources.session_profile()
    entry = cfg.get("profiles", {}).get(name) or {"class": "subscription", "quota": "statusline"}
    if not entry.get("enabled", True):
        return None   # the user took this profile out of routing
    served = {sources.resolve(cat, m) for m in entry.get("models", [])}   # empty list: the profile did not say
    aliases = {}
    for alias, default in ALIASES:
        model = sources.resolve(cat, os.environ.get(f"ANTHROPIC_DEFAULT_{alias.upper()}_MODEL") or default or "")
        if model and (not entry.get("models") or model in served) and model not in entry.get("disabled_models", []):
            aliases[alias] = model
    named = tool_input.get("model")
    if not aliases or (named and named not in aliases):
        return None   # a model the caller named that the router cannot rate stays as named
    entry = {**entry, "enabled": True, "models": list(dict.fromkeys(aliases.values()))}
    pin = {"model": aliases[named]} if named else {}
    if tool_input.get("effort"):
        pin["effort"] = tool_input["effort"]
    task, _ = classify.classify(tool_input.get("prompt", ""), cat, (cfg.get("decider") or {}).get("url"), timeout=5)
    # the cached copy only: a subagent should not wait on a download
    inputs, contexts = sources.model_facts(cat, sources.read_json(sources.cache_dir() / "models.dev.json", {}) or {})
    pol = policy(cfg)
    with sources.ledger_lock():   # read pending work and reserve this pick as one step
        profiles, _ = sources.build_profiles({**cfg, "profiles": {name: entry}}, cat)
        pick, _ = core.choose(core.options(task, profiles, cat, inputs, contexts, time.time(), pol, pin)[0], pol)
        if not pick:
            return None
        sources.ledger_append({"id": "native-" + secrets.token_hex(3), "ts": time.time(), "profile": name,
                               "model": pick.model, "effort": pick.effort, "class": pick.klass,
                               "cost_usd": round(pick.cost_usd, 4), "quality": round(pick.quality, 3),
                               "task": {"kinds": task.kinds, "difficulty": task.difficulty}, "native": True})
    alias = named if named and aliases[named] == pick.model else next(a for a, m in aliases.items() if m == pick.model)
    level = cat["difficulty"]["levels"][min(int(round(task.difficulty)), 4)]
    kind = max(task.kinds, key=task.kinds.get)
    note = f"model-router ran this subagent on {alias} ({pick.model}) at effort {pick.effort}: {level} {kind}."
    return {**tool_input, "model": alias, "effort": pick.effort}, note


def cmd_hook(args) -> int:
    """Claude Code PreToolUse hook for the Agent tool. Never blocks: on any error the call goes ahead unchanged."""
    try:
        event = json.loads(sys.stdin.read() or "{}")
        tool_input = event.get("tool_input") or {}
        if event.get("tool_name") not in ("Agent", "Task") or tool_input.get("subagent_type") == "fork" \
                or (tool_input.get("model") and tool_input.get("effort")):
            return 0   # a fork keeps the parent's model; a call that names both was pinned on purpose
        routed = native_route(tool_input)
    except Exception:   # noqa: BLE001 -- a routing failure must never stop a subagent
        return 0
    if routed:
        print(json.dumps({"hookSpecificOutput": {"hookEventName": "PreToolUse", "updatedInput": routed[0],
                                                 "additionalContext": routed[1]}}))
    return 0


def cmd_feedback(args) -> int:
    """Move a model's ability for the job's kinds toward the outcome (one logistic step)."""
    row = next((r for r in sources.ledger_rows() if r.get("id") == args.job), None)
    if not row:
        raise SystemExit(f"model-router: no routed job {args.job!r} in the ledger")
    path = sources.config_dir() / "skills.json"
    outcome = 1.0 if args.outcome == "pass" else 0.0
    step = 0.3 * (outcome - row.get("quality", 0.5))
    with sources.ledger_lock():   # read-modify-write of skills.json
        learned = sources.read_json(path, {}) or {}
        for kind, p in row["task"]["kinds"].items():
            learned.setdefault(row["model"], {}).setdefault(kind, 0.0)
            learned[row["model"]][kind] = round(learned[row["model"]][kind] + step * p, 4)
        sources.write_json(path, learned)
    print(f"{row['model']}: ability {'+' if step >= 0 else ''}{step:.3f} on {', '.join(row['task']['kinds'])}")
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="model-router", description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("init", help="find ccmorph profiles and write the config")
    p.add_argument("--force", action="store_true", help="forget earlier choices")
    p.set_defaults(func=cmd_init)
    sub.add_parser("profiles", help="which profiles take part").set_defaults(func=cmd_profiles)
    for name in ("enable", "disable"):
        p = sub.add_parser(name, help=f"{name} profiles for routing")
        p.add_argument("names", nargs="+")
        p.set_defaults(func=cmd_toggle)
    p = sub.add_parser("quota", help="live 5h/7d/monthly windows and their quota price")
    p.add_argument("--refresh", action="store_true")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_quota)
    p = sub.add_parser("route", help="choose profile, model and effort for a task")
    add_task_args(p)
    p.add_argument("--explain", action="store_true", help="show the shortlist and what was dropped")
    p.set_defaults(func=cmd_route)
    p = sub.add_parser("dispatch", help="route, then run the task through the dispatcher")
    add_task_args(p)
    p.add_argument("--agent", help="run as this Claude Code agent definition")
    p.add_argument("--timeout", type=int, default=3600)
    p.add_argument("--cwd")
    p.add_argument("--dry-run", action="store_true", help="print the command instead of running it")
    p.set_defaults(func=cmd_dispatch)
    p = sub.add_parser("record", help="store the rate_limits a Claude Code statusline receives on stdin")
    p.add_argument("--profile", help="default: the name of CLAUDE_CONFIG_DIR")
    p.set_defaults(func=cmd_record)
    sub.add_parser("hook", help="Claude Code PreToolUse hook: route native Agent calls (reads the event on stdin)"
                   ).set_defaults(func=cmd_hook)
    p = sub.add_parser("feedback", help="record whether a routed job passed")
    p.add_argument("job")
    p.add_argument("outcome", choices=("pass", "fail"))
    p.set_defaults(func=cmd_feedback)
    p = sub.add_parser("simulate", help="replay a synthetic week against baseline policies")
    p.add_argument("--days", type=int, default=7)
    p.add_argument("--seed", type=int, default=7)
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=lambda a: __import__("model_router.simulate", fromlist=["run"]).run(a, load_catalogue()))
    argv = list(sys.argv[1:] if argv is None else argv)
    # dispatch: after --, arguments for the agent CLI, never the prompt; elsewhere -- keeps its usual meaning
    cut = argv.index("--") if argv[:1] == ["dispatch"] and "--" in argv else len(argv)
    args = parser.parse_args(argv[:cut])
    args.passthrough = argv[cut + 1:]
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
