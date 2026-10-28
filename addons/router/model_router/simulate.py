"""A synthetic week: the router against the policies people use today.

Three subscriptions with a human using them, one monthly budget, one pay-per-token
gateway. Tasks arrive through the day with a mix of kinds and difficulties; each
job's real size is its estimate times a heavy-tailed draw. Quality is scored with
the same ability table the router uses, so the replay measures allocation
(cost, waste, limit hits), not the accuracy of the table itself.
"""
from __future__ import annotations

import json
import math
import random

from . import core

STEP = 600  # seconds
SUB_MODELS = {"anthropic": ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"],
              "openai": ["gpt-6.1-sol", "gpt-6-luna", "gpt-6-astra"]}
API_MODELS = ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "gpt-6.1-sol", "gpt-6-luna",
              "glm-5.3", "deepseek-v4-flash", "kimi-k3.1-pro", "gemini-4-argon"]
INPUTS = {m: ("text",) if m == "glm-5.3" else ("text", "image") for m in set(API_MODELS) | {m for v in SUB_MODELS.values() for m in v}}
# (name, class, vendor, 5h capacity $, 7d capacity $, human $ per active 10 minutes)
SUBS = [("max5", "subscription", "anthropic", 60, 500, 0.55),
        ("pro", "subscription", "anthropic", 12, 100, 0.08),
        ("plus", "subscription", "openai", 10, 80, 0.05)]
BUDGET = ("proxy", 500.0, 0.6)   # monthly $, fraction of the month already gone at the start
KIND_MIX = {"code_edit": 0.35, "code_review": 0.1, "web_research": 0.15, "social_search": 0.05,
            "vision": 0.05, "writing": 0.1, "analysis": 0.1, "mechanical": 0.1}
LEVEL_MIX = [0.1, 0.3, 0.3, 0.2, 0.1]


def active(t: float) -> bool:
    hour = (t / 3600) % 24
    return hour >= 9 or hour < 1


class World:
    def __init__(self, days: int):
        self.end = days * 86400.0
        self.used = {}   # (profile, window) -> $ used in the current window
        self.reset = {}
        self.wasted = 0.0
        self.human_blocked = 0
        self.limit_hits = 0
        self.api_usd = 0.0
        self.sub_usd = 0.0
        for name, _, _, c5, c7, _ in SUBS:
            self.used[(name, "5h")], self.reset[(name, "5h")] = 0.0, 5 * 3600.0
            self.used[(name, "7d")], self.reset[(name, "7d")] = 0.0, 7 * 86400.0
        self.used[("proxy", "month")] = BUDGET[1] * BUDGET[2] * 0.5   # half the pace so far
        self.reset[("proxy", "month")] = 30 * 86400 * (1 - BUDGET[2])

    def caps(self, name: str) -> dict:
        if name == "proxy":
            return {"month": BUDGET[1]}
        row = next(s for s in SUBS if s[0] == name)
        return {"5h": row[3], "7d": row[4]}

    def roll(self, t: float) -> None:
        for key, at in list(self.reset.items()):
            while t >= at:
                if key[1] == "7d":   # weekly quota left at the reset is gone
                    self.wasted += max(0.0, self.caps(key[0])["7d"] - self.used[key])
                self.used[key] = 0.0
                length = {"5h": 5 * 3600, "7d": 7 * 86400, "month": 30 * 86400}[key[1]]
                at += length
                self.reset[key] = at

    def room(self, name: str) -> float:
        return min(cap - self.used[(name, w)] for w, cap in self.caps(name).items())

    def spend(self, name: str, usd: float) -> bool:
        """Spend on every window of a profile; False when a window runs out mid-job."""
        fits = usd <= self.room(name)
        take = usd if fits else max(self.room(name), 0.0)
        for w in self.caps(name):
            self.used[(name, w)] += take
        return fits

    def profiles(self, t: float, reserve: dict) -> list:
        out = []
        for name, klass, vendor, *_ in SUBS:
            wins = [core.Window(w, self.used[(name, w)] / cap, self.reset[(name, w)],
                                {"5h": 5 * 3600, "7d": 7 * 86400}[w], cap) for w, cap in self.caps(name).items()]
            out.append(core.Profile(name, klass, SUB_MODELS[vendor], wins, reserve))
        cap = BUDGET[1]
        out.append(core.Profile("proxy", "budget", SUB_MODELS["openai"],
                                [core.Window("month", self.used[("proxy", "month")] / cap, self.reset[("proxy", "month")],
                                             30 * 86400, cap)], {"month": 0.05}))
        out.append(core.Profile("gateway", "api", API_MODELS))
        return out


def sample_task(rng: random.Random) -> core.Task:
    kind = rng.choices(list(KIND_MIX), weights=list(KIND_MIX.values()))[0]
    second = rng.choices(list(KIND_MIX), weights=list(KIND_MIX.values()))[0]
    kinds = {kind: 0.8, second: 0.2} if second != kind else {kind: 1.0}
    level = rng.choices(range(5), weights=LEVEL_MIX)[0] + rng.uniform(-0.4, 0.4)
    needs = {"vision"} if "vision" in kinds else set()
    return core.Task(kinds, min(max(level, 0), 4), needs)


def router(mode, only=None):
    pol = core.Policy(**core.MODES[mode])

    def pick(task, world, t, cat):
        profiles = [p for p in world.profiles(t, {"5h": 0.15, "7d": 0.1}) if not only or p.name in only]
        opts, _ = core.options(task, profiles, cat, INPUTS, {}, t, pol)
        return core.choose(opts, pol)[0]
    return pick


def pick_fixed(name, model, effort):
    def pick(task, world, t, cat):
        return core.Option(name, model, effort, "api", core.quality(cat, model, effort, task),
                           core.job_cost(cat, model, effort, task), 0, 0)
    return pick


def pick_roles(task, world, t, cat):
    """A static role table by difficulty, all on the gateway (junior, engineer, director)."""
    model, effort = (("deepseek-v4-flash", "high") if task.difficulty < 1.5 else
                     ("glm-5.3", "high") if task.difficulty < 2.5 else ("claude-opus-5-5", "high"))
    return pick_fixed("gateway", model, effort)(task, world, t, cat)


def pick_pool(task, world, t, cat):
    """Account-pool style: round robin over subscriptions with their strongest everyday model, gateway when all are near full."""
    order = [s for s in SUBS if world.room(s[0]) > 0.05 * min(world.caps(s[0]).values())]
    if not order:
        return pick_fixed("gateway", "claude-opus-5-5", "medium")(task, world, t, cat)
    name, _, vendor, *_ = order[int(t / STEP) % len(order)]
    model = "claude-opus-5-5" if vendor == "anthropic" else "gpt-6.1-sol"
    opt = pick_fixed(name, model, "medium")(task, world, t, cat)
    opt.klass = "subscription"
    return opt


POLICIES = {"one model (Opus, gateway)": pick_fixed("gateway", "claude-opus-5-5", "medium"),
            "role table (gateway)": pick_roles, "account pool (round robin)": pick_pool,
            "model-router, cost": router("cost"), "model-router, balanced": router("balanced"),
            "model-router, quality": router("quality"),
            "model-router, gateway only": router("balanced", {"gateway"})}   # model choice alone, no prepaid quota


def replay(policy_name: str, days: int, seed: int, cat: dict) -> dict:
    rng = random.Random(seed)
    world = World(days)
    pick = POLICIES[policy_name]
    done = quality = 0.0
    t = 0.0
    while t < world.end:
        world.roll(t)
        for name, *_, human in SUBS:
            if active(t):
                demand = human * rng.uniform(0.3, 1.7)
                if not world.spend(name, demand):
                    world.human_blocked += 1
        rate = 6.0 if active(t) else 1.0   # tasks per hour
        for _ in range(sum(1 for _ in range(12) if rng.random() < rate / 6 / 12)):
            task = sample_task(rng)
            opt = pick(task, world, t, cat)
            if opt is None:
                continue
            real = core.job_cost(cat, opt.model, opt.effort, task) * math.exp(rng.gauss(-0.18, 0.6))
            if opt.klass in core.PERISHABLE:
                if world.spend(opt.profile, real):
                    world.sub_usd += real
                else:   # ran out mid-job: redo it on the gateway
                    world.limit_hits += 1
                    world.api_usd += real
            else:
                world.api_usd += real
            quality += core.quality(cat, opt.model, opt.effort, task)
            done += 1
        t += STEP
    world.roll(world.end)
    return {"policy": policy_name, "tasks": int(done), "api_usd": round(world.api_usd, 2),
            "prepaid_used_usd": round(world.sub_usd, 2), "expired_quota_usd": round(world.wasted, 2),
            "limit_hits": world.limit_hits, "human_blocked_10min": world.human_blocked,
            "mean_quality": round(quality / max(done, 1), 3)}


def run(args, cat: dict) -> int:
    rows = [replay(name, args.days, args.seed, cat) for name in POLICIES]
    if args.json:
        print(json.dumps(rows, indent=2))
        return 0
    print(f"{args.days}-day replay, seed {args.seed}: same task stream and human usage for every policy\n")
    print(f"{'policy':<28} {'paid $':>8} {'prepaid used $':>15} {'weekly quota expired $':>23} {'limit hits':>11} {'human blocked':>14} {'quality':>8}")
    for r in rows:
        print(f"{r['policy']:<28} {r['api_usd']:>8.2f} {r['prepaid_used_usd']:>15.2f} {r['expired_quota_usd']:>23.2f} "
              f"{r['limit_hits']:>11} {r['human_blocked_10min']:>14} {r['mean_quality']:>8.3f}")
    return 0
