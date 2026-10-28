"""The routing decision: pure functions over plain data, no I/O.

A task is a mix of kinds of work plus a difficulty. An option is one
(profile, model, effort) triple. Each option gets a predicted success
probability (item response theory: ability minus difficulty through a logistic,
ability read off the Artificial Analysis index of that model at that effort) and
an effective cost: the dollar cost for paid API, and for subscriptions and
budgets the same dollars times the quota's shadow price, which is the chance the
quota runs out before it resets (perishable capacity is free while it would
otherwise expire unused). The router keeps the options that are good enough or
nearly the best, and takes the cheapest in the class the user ranks first.
"""
from __future__ import annotations

from dataclasses import dataclass, field
import math

CLASSES = ("subscription", "budget", "api")
PERISHABLE = ("subscription", "budget")
FLOOR = 0.02        # a subscription is never quite free: it still spends quota
PRIOR_RATE = 0.5    # without history, assume others use half a window per window
PRIOR_WEIGHT = 0.25  # how much of a window the prior counts for
SIGMA = 0.8         # spread of future demand (log scale)
FIT_MARGIN = 2.0    # a job must fit twice over before a window's reserve: sizes are heavy-tailed


@dataclass
class Task:
    kinds: dict            # kind -> probability, sums to 1
    difficulty: float      # expected level, 0 (trivial) .. 4 (very hard)
    needs: set = field(default_factory=set)   # "vision", "web", "x" (posts from X)
    context_tokens: int = 0
    source: str = "given"
    self_contained: float | None = None


@dataclass
class Window:
    name: str              # "5h", "7d", "7d-opus", "month"
    used: float            # 0..1
    resets_at: float       # epoch seconds
    length: float          # seconds
    capacity_usd: float | None = None   # API-equivalent dollars one full window holds
    models: tuple = ()     # empty = applies to every model on the profile


@dataclass
class Profile:
    name: str
    klass: str             # subscription | budget | api
    models: list
    windows: list = field(default_factory=list)
    reserve: dict = field(default_factory=dict)   # window name -> fraction kept for the human
    pending_usd: float = 0.0                      # routed work not yet visible in the reading
    quota_known: bool = True
    price_factor: float = 1.0                     # this route's price against list price
    ids: dict = field(default_factory=dict)       # catalogue model -> the id this profile serves it under


@dataclass
class Option:
    profile: str
    model: str
    effort: str
    klass: str
    quality: float
    cost_usd: float
    runout: float
    effective_usd: float
    tier: int = 0


@dataclass
class Policy:
    priority: tuple = CLASSES
    quality_target: float = 0.95  # good enough on its own: P(success) at or above this
    band: float = 0.04            # or this close to the best option for the task
    protect_above: float = 0.5   # a subscription likelier than this to run out is kept for the human
    unknown_runout: float = 0.3  # assumed for a perishable profile whose quota cannot be read
    max_job_usd: float = 15.0    # never route a job estimated above this (paid, or quota-priced, dollars)


# Copilot, Cursor and Azure expose the same three-way dial: how close to the best a cheaper pick must be.
# Values chosen from a sweep of the synthetic week (`model-router simulate`).
MODES = {"quality": {"quality_target": 0.95, "band": 0.02},
         "balanced": {"quality_target": 0.95, "band": 0.04},
         "cost": {"quality_target": 0.9, "band": 0.06}}


def sigmoid(x: float) -> float:
    return 1 / (1 + math.exp(-x)) if x > -60 else 0.0


def phi(x: float) -> float:
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def interp(table: list, x: float) -> float:
    x = min(max(x, 0), len(table) - 1)
    i = min(int(x), len(table) - 2)
    return table[i] + (table[i + 1] - table[i]) * (x - i)


def efforts(cat: dict, model: str) -> list:
    return list(cat["models"][model]["aa"])


def quality(cat: dict, model: str, effort: str, task: Task) -> float:
    """P(success), mixed over the task's kinds.

    Ability is the AA index at this effort on a logit scale. Kinds that gain
    little from thinking (bulk edits, social search) get only part of the step
    away from the model's middle rung: `medium`, or the middle of a ladder
    without one (`high` on low/high/max).
    """
    entry = cat["models"][model]
    aa = entry["aa"]
    a0, a1 = cat["ability"]["index_at_zero"], cat["ability"]["index_per_logit"]
    mid = "medium" if "medium" in aa else list(aa)[len(aa) // 2]
    at_mid, at_e = (aa[mid][0] - a0) / a1, (aa[effort][0] - a0) / a1
    b = interp(cat["difficulty"]["logit"], task.difficulty)
    total = 0.0
    for kind, p in task.kinds.items():
        shift = entry.get("adjust", {}).get(kind, cat.get("default_adjust", {}).get(kind, 0.0))
        theta = at_mid + cat["kinds"][kind]["reasoning"] * (at_e - at_mid) + shift
        total += p * sigmoid(cat["discrimination"] * (theta - b))
    return total


def job_cost(cat: dict, model: str, effort: str, task: Task, price_factor: float = 1.0) -> float:
    """List-price dollars: job size in AA-task units times this model's AA cost per task at this effort."""
    size = interp(cat["difficulty"]["size"], task.difficulty)
    size *= sum(p * cat["kinds"][k]["size"] for k, p in task.kinds.items())
    return size * cat["models"][model]["aa"][effort][1] * price_factor


def runout(window: Window, now: float, reserve: float, take: float) -> float:
    """Chance this window runs out before it resets if `take` (a fraction of it) is spent now.

    The bid price of perishable capacity (Littlewood; Talluri and van Ryzin): one
    more unit is worth keeping only if demand before the reset would have used it.
    Demand is forecast from the pace so far this window, smoothed toward a prior,
    and taken as lognormal around that mean.
    """
    left = 1 - reserve - window.used - take
    if left <= 0:
        return 1.0
    remaining = min(max((window.resets_at - now) / window.length, 1e-3), 1.0)
    elapsed = 1 - remaining
    rate = (window.used + PRIOR_RATE * PRIOR_WEIGHT) / (elapsed + PRIOR_WEIGHT)
    demand = rate * remaining
    if demand <= 0:
        return 0.0
    return 1 - phi((math.log(left / demand) + SIGMA ** 2 / 2) / SIGMA)


def quota_price(profile: Profile, model: str, cost_usd: float, now: float) -> float | None:
    """Price of a job's quota as a fraction of its dollar cost; None when the job does not fit.

    One job draws on every window at once, so its price is the sum of the window
    prices (an LP column's reduced cost sums over its rows), capped at 1: quota is
    never worth more than the paid alternative.
    """
    total = 0.0
    for w in profile.windows:
        if w.models and not any(m in model for m in w.models):
            continue
        reserve = profile.reserve.get(w.name, 0.0)
        take = (cost_usd + profile.pending_usd) / w.capacity_usd if w.capacity_usd else 0.0
        if 1 - reserve - w.used - FIT_MARGIN * take <= 0:   # chance constraint on a heavy-tailed job size
            return None
        total += runout(w, now, reserve, take)
    return min(total, 1.0)


def options(task: Task, profiles: list, cat: dict, inputs: dict, contexts: dict,
            now: float, policy: Policy, pin: dict | None = None) -> tuple[list, list]:
    """Every feasible option, scored, plus the reasons others were dropped."""
    pin = pin or {}
    out, dropped = [], []
    for prof in profiles:
        if pin.get("profile") and prof.name != pin["profile"]:
            continue
        for model in prof.models:
            if pin.get("model") and model != pin["model"]:
                continue
            if model not in cat["models"]:
                dropped.append(f"{prof.name}/{model}: no capability rating")
                continue
            if "vision" in task.needs and "image" not in inputs.get(model, ("text",)):
                dropped.append(f"{prof.name}/{model}: cannot read images")
                continue
            if task.context_tokens and contexts.get(model) and task.context_tokens > contexts[model]:
                dropped.append(f"{prof.name}/{model}: context too small")
                continue
            ladder = efforts(cat, model)
            if pin.get("effort"):
                if pin["effort"] not in ladder:
                    dropped.append(f"{prof.name}/{model}: effort {pin['effort']} not accepted")
                    continue
                ladder = [pin["effort"]]
            for effort in ladder:
                cost = job_cost(cat, model, effort, task, prof.price_factor)
                q = quality(cat, model, effort, task)
                if prof.klass in PERISHABLE:
                    if prof.quota_known:
                        lam = quota_price(prof, model, cost, now)
                        if lam is None:
                            dropped.append(f"{prof.name}/{model}/{effort}: would cross the quota reserve")
                            continue
                    else:
                        lam = policy.unknown_runout
                    eff = cost * (lam + FLOOR)
                else:
                    lam, eff = 0.0, cost
                if eff > policy.max_job_usd:
                    dropped.append(f"{prof.name}/{model}/{effort}: estimated ${eff:.2f} is over the per-job limit")
                    continue
                rank = policy.priority.index(prof.klass) if prof.klass in policy.priority else len(policy.priority)
                if prof.klass in PERISHABLE and lam > policy.protect_above:
                    rank = len(policy.priority)   # last resort: likely to run out on the human
                out.append(Option(prof.name, model, effort, prof.klass, q, cost, lam, eff, rank))
    x = [o for o in out if "x_search" in cat["models"][o.model].get("tools", ())]
    if "x" in task.needs and x:   # only once the X-capable options are known to be usable
        dropped += sorted({f"{o.profile}/{o.model}: no native X search" for o in out if o not in x})
        out = x
    return out, dropped


def choose(opts: list, policy: Policy) -> tuple[Option | None, list]:
    """The pick and the ranked shortlist it came from.

    Keep options that are good enough or nearly as good as the best, then take the
    cheapest in the class ranked first. Subscriptions kept for the human only count
    when nothing else is acceptable.
    """
    if not opts:
        return None, []
    pool = [o for o in opts if o.tier < len(policy.priority)] or opts
    best = max(o.quality for o in pool)
    good = [o for o in pool if o.quality >= best - policy.band or o.quality >= policy.quality_target]
    good.sort(key=lambda o: (o.tier, o.effective_usd, -o.quality, o.runout, o.profile, o.model))
    return good[0], good
