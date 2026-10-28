"""How close the free local decider and the keyword rules get to a frontier model's own reading of a task.

The labels in tasks.json are one strong model's judgment (kind of work, difficulty
0-4, needs), i.e. what the main agent would decide. For each task the script asks
the decider and the keyword rules, then routes each reading over every catalogue
model on one pay-per-token profile and scores the pick against the labelled task:
lost quality and extra cost are what a misreading actually costs. It also refits
`decider_scale` (labelled level against the decider's raw score) and reports the
leave-one-out error, so another decider can be calibrated the same way.

    uv run python bench/decider.py [--url http://127.0.0.1:8099/v1/systemone]
"""
import argparse
import json
from pathlib import Path
import statistics
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from model_router import classify, core  # noqa: E402
from model_router.cli import load_catalogue  # noqa: E402


def pick(task, cat):
    pool = [core.Profile("gateway", "api", list(cat["models"]))]
    inputs = {m: ("text", "image") for m in cat["models"]}
    opts, _ = core.options(task, pool, cat, inputs, {}, 0.0, core.Policy())
    return core.choose(opts, core.Policy())[0]


def fit(xs, ys):
    mx, my = statistics.mean(xs), statistics.mean(ys)
    b = sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / sum((x - mx) ** 2 for x in xs)
    return my - b * mx, b


def clamp(x):
    return min(max(x, 0.0), 4.0)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8099/v1/systemone")
    args = ap.parse_args()
    cat = load_catalogue()
    offset, scale = cat.get("decider_scale", (0.0, 1.0))
    raw_cat = {**cat, "decider_scale": (0.0, 1.0)}
    cases = json.loads(Path(__file__).with_name("tasks.json").read_text())
    rows = {"decider, raw": [], "decider": [], "keywords": []}
    raw_scores, seconds = [], []
    for c in cases:
        truth = core.Task({c["kind"]: 1.0}, c["level"], set(c.get("needs", [])))
        best = pick(truth, cat)
        t0 = time.time()
        try:
            raw = classify.ask_decider(args.url, c["task"], raw_cat)
        except (OSError, ValueError, KeyError) as e:
            print(f"decider failed: {e}", file=sys.stderr)
            return 1
        seconds.append(time.time() - t0)
        raw_scores.append(raw.difficulty)
        calibrated = core.Task(raw.kinds, clamp(offset + scale * raw.difficulty), raw.needs)
        for name, t in (("decider, raw", raw), ("decider", calibrated), ("keywords", classify.heuristic(c["task"]))):
            chosen = pick(t, cat)
            q = core.quality(cat, chosen.model, chosen.effort, truth)   # how the pick does on the real task
            cost = core.job_cost(cat, chosen.model, chosen.effort, truth)
            rows[name].append({"kind_ok": max(t.kinds, key=t.kinds.get) == c["kind"],
                               "level_err": abs(t.difficulty - c["level"]),
                               "needs_ok": t.needs >= set(c.get("needs", [])),
                               "lost_quality": best.quality - q, "cost_ratio": cost / best.cost_usd,
                               "pick": f"{chosen.model}/{chosen.effort}", "best": f"{best.model}/{best.effort}",
                               "read": f"{max(t.kinds, key=t.kinds.get)} {t.difficulty:.1f}", "task": c["task"][:60]})
    print(f"{len(cases)} labelled tasks; reference = the route of the labelled task over all {len(cat['models'])} models; "
          f"decider {statistics.mean(seconds):.2f} s a call\n")
    print(f"{'reader':<14} {'kind right':>10} {'level error':>12} {'needs found':>12} {'quality lost':>13} {'cost vs ref':>12} {'off by 1.5x+':>13}")
    for name, r in rows.items():
        n = len(r)
        print(f"{name:<14} {sum(x['kind_ok'] for x in r) / n:>10.0%} {statistics.mean(x['level_err'] for x in r):>12.2f} "
              f"{sum(x['needs_ok'] for x in r) / n:>12.0%} {statistics.mean(x['lost_quality'] for x in r):>13.3f} "
              f"{statistics.mean(x['cost_ratio'] for x in r):>11.2f}x "
              f"{sum(not 1 / 1.5 < x['cost_ratio'] < 1.5 for x in r):>13}")
    levels = [c["level"] for c in cases]
    a, b = fit(raw_scores, levels)
    loo = []
    for i in range(len(levels)):
        aa, bb = fit(raw_scores[:i] + raw_scores[i + 1:], levels[:i] + levels[i + 1:])
        loo.append(abs(clamp(aa + bb * raw_scores[i]) - levels[i]))
    print(f"\nrefit decider_scale: [{a:.2f}, {b:.2f}] (catalogue has [{offset}, {scale}]); "
          f"level error raw {statistics.mean(abs(x - y) for x, y in zip(raw_scores, levels)):.2f}, "
          f"calibrated out of sample (leave one out) {statistics.mean(loo):.2f}")
    print("\nworst calibrated misreadings (by quality lost):")
    for x in sorted(rows["decider"], key=lambda x: -x["lost_quality"])[:5]:
        print(f"  {x['lost_quality']:+.3f}  read {x['read']:<18} pick {x['pick']:<26} ref {x['best']:<26} {x['task']}")
    Path(__file__).with_name("decider-results.json").write_text(json.dumps(rows, indent=1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
