# Topic 2: Optimisation theory for spending perishable quotas and budgets

Research date: 2026-10-28. Output of researcher 2 of 5.

## 1. Key findings

- The whole problem is one linear programme (assignment of tasks to options, packing constraints per window and per budget). Its dual gives one shadow price per window or budget, and the online rule is "argmax of quality minus price-weighted consumption" ([Balseiro, Lu, Mirrokni, ICML 2020](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf)).
- For perishable capacity the right price is a probability, not a constant: price = (value of a unit) × P(demand before reset exceeds what is left). The classic airline form is `λ = r·P(D > x)` ([Talluri & van Ryzin 1998](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf), eq. 19). That price falls to zero as the reset nears with capacity left over, which is exactly the use-it-or-lose-it behaviour we want.
- For several windows consumed by one task, the price of an option is the **sum** of the per-window prices (a bid-price policy sells only if the fare exceeds the sum of the leg prices, [same paper](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)). Because non-binding windows have price ≈ 0, the sum behaves like a max in practice.
- Known guarantees: Balance/MSVV is `1 − 1/e` competitive against any arrival order, and that is optimal ([Mehta et al.](https://people.eecs.berkeley.edu/~vazirani/pubs/adwords.pdf)). With i.i.d. arrivals, dual mirror descent has `O(√T)` regret and `Ω(√T)` is unavoidable ([Balseiro et al.](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf)). Bandits with Knapsacks has regret `Õ(√(m·OPT) + OPT·√(m/B))`, tight up to logs ([Badanidiyuru et al.](https://arxiv.org/pdf/1305.2545)).
- Dual mirror descent has a built-in guarantee that matters here: its price update makes it target a constant consumption rate, and the authors prove it never depletes a resource too early ([same paper](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf)). That is budget pacing with a proof.
- Ad-tech pacing is feedback control on the gap between actual and scheduled spend: multiplicative throttling (`p ← p·(1±λ)`) or PID ([Practical Guide to Budget Pacing, 2025](https://arxiv.org/html/2503.06942v2)). The schedule should follow expected demand, not wall-clock time.
- LLM gateways do not do any of this. LiteLLM weights by configured rpm/tpm and labels usage-based routing "bad for perf" ([docs](https://docs.litellm.ai/docs/proxy/load_balancing)); OpenRouter picks among stable providers weighted by inverse square of price ([docs](https://openrouter.ai/docs/features/provider-routing)). Neither models perishability or windows. The gap is ours to fill.
- Hard limits mid-task are handled by a chance constraint: require `estimate + z·σ ≤ remaining − reserve`, where `z` is 1.65 (normal, 5%) or 4.36 (distribution-free Cantelli bound, 5%), plus subtracting tokens already reserved by in-flight tasks.

## 2. Details

### 2.1 The exact optimisation problem

Notation. A task `t` has candidate options `o` (a profile, model, effort triple). Resources `r` are: each subscription window (5-hour and 7-day, per profile; Claude Max also has a separate weekly limit for one model family per [Anthropic's help page](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan), which is just one more `r`), plus the proxy's monthly dollar budget. Let `q_to` be the quality score, `c_to` the dollar cost (zero for subscription options, tokens × price for API options), and `a_tor` the amount of resource `r` consumed (percent of the window for a subscription option, dollars for the monthly budget). Let `R_r` be what is left.

```
maximise    Σ_t Σ_o x_to · ( q_to − κ·c_to )
subject to  Σ_o x_to = 1                  for every task t
            Σ_t Σ_o a_tor · x_to ≤ R_r    for every resource r
            x_to ≥ 0
```

- It is a linear programme, so convex. Integrality (`x ∈ {0,1}`) makes it a generalised assignment problem, NP-hard offline, but the LP relaxation is nearly tight: a basic optimal solution splits at most `m` tasks, where `m` is the number of resource constraints (a standard LP fact, my derivation, not cited). With about 6 constraints, rounding costs almost nothing.
- Lagrangian relaxation of the resource rows with multipliers `μ_r ≥ 0` decouples the tasks. Each task solves
  `o*(t) = argmax_o [ q_to − κ·c_to − Σ_r μ_r · a_tor ]`
  and the dual problem is `min_{μ≥0} Σ_r μ_r R_r + Σ_t max_o(...)`. The multipliers are the shadow prices: `μ_r` is how much total quality would improve per extra unit of `R_r`.
- Complementary slackness gives the use-it-or-lose-it rule: if window `r` will not run out (`Σ a ≤ R_r` at the unconstrained optimum), then `μ_r = 0`. This is the same reason the deterministic LP bid price is zero on any leg whose mean demand is below capacity ([Talluri & van Ryzin, §5.1](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)).
- Where the dollar sign belongs: `κ·c` prices real money, `μ·a` prices scarce quota. Both are in the same quality units, so one `argmax` compares a free-but-scarce subscription slot with a paid API call.

### 2.2 Online versions and their guarantees

| Setting | Algorithm | Guarantee |
|---|---|---|
| Arrival order adversarial, small tasks vs budget | Balance / MSVV: pick `argmax bid·ψ(f)`, `ψ(f) = 1 − e^(f−1)`, `f` = fraction of budget spent ([Mehta et al.](https://people.eecs.berkeley.edu/~vazirani/pubs/adwords.pdf)) | `1 − 1/e ≈ 0.63` of offline optimum; no deterministic algorithm does better (Kalyanasundaram–Pruhs lower bound, quoted in the same paper) |
| i.i.d. arrivals from unknown distribution | Dual mirror descent: `x_t = argmax f_t(x) − μ_tᵀ b_t x`; `μ_{t+1} = μ_t·exp(−η(ρ − b_t x_t))` (multiplicative) or `max(0, μ_t − η(ρ − b_t x_t))` (subgradient), `ρ = B/T` ([Balseiro et al.](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf)) | `O(√T)` regret with `η ~ 1/√T`; lower bound `Ω(√T)`; resources never deplete early (their Proposition 2) |
| Quality and consumption unknown, must be learned | Bandits with Knapsacks, PrimalDualBwK: multiplicative-weights resource prices, UCB on reward, LCB on consumption, play the most cost-effective arm ([Badanidiyuru et al.](https://arxiv.org/pdf/1305.2545)) | `Õ(√(m·OPT) + OPT·√(m/B))`, matching lower bound up to logs; `B` is the smallest budget |
| Per-model token budgets, LLM routing | PORT: learn one dual weight per model from about 250 initial queries, then route by it ([Wu & Silwal, 2025](https://arxiv.org/html/2509.02718)) | Claims `1 − o(1)` competitive ratio under stated assumptions (I read the abstract and excerpts only) |

Reading the table for our case:

- MSVV's `ψ` is a multiplicative discount that goes to zero as a budget empties and is `1 − 1/e` at `f = 0`. It was built for budgets that never reset and for arrivals an adversary controls. With a reset it is wrong on its own: at `f = 0.6` just before the reset it still charges a positive price (`ψ(0.6) = 0.33`), although the leftover is about to vanish. Use it only as a tie-breaker between equivalent subscriptions, where its "prefer the emptier budget" behaviour is exactly the load-spreading we want.
- Dual mirror descent assumes one horizon `T` and a budget `B` that is not refilled. We have a horizon per window. Treat each reset as a new episode: restart with `ρ_r = R_r / (expected tasks until reset)` and warm-start `μ_r` from the previous episode. The `O(√T)` guarantee is weak when `T` is small (a 5-hour window may see tens of tasks), so the update should be a gentle correction, not the core of the price.
- BwK is the right frame only if quality scores are themselves unknown. If we are given a quality table, we are in the simpler known-distribution case and need no exploration. A cheap hybrid: log realised quality and token use, and let the table drift.

### 2.3 Perishable capacity: bid prices

Revenue management treats seats, rooms and similar stock that is worthless after a deadline. Gallego & van Ryzin's finite-horizon pricing paper ([abstract](https://business.columbia.edu/sites/default/files-efs/pubfiles/3943/vanryzin_optimal_dynamic_pricing.pdf)) is the model: stock sold by a deadline, value zero afterwards.

- **Deterministic bid price.** Solve the LP with mean demand, take the duals. If expected demand before the deadline is below remaining capacity, the dual is zero ([Talluri & van Ryzin, §5.1](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)). That paper also notes the DLP "considers only the mean demand" and its weakness, yet with frequent re-solving it performed well in simulation.
- **Probabilistic version.** With normal demand over the remaining horizon, the price of one unit is `λ = r·P(D > x) = r·(1 − Φ((x − m)/s))`, where `x` is remaining capacity, `m, s` the mean and standard deviation of remaining demand, and `r` the fare ([same paper, eq. 19](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)). This keeps a small positive price even when mean demand is below capacity, and falls smoothly to zero as the deadline approaches with capacity unspent, because `m` shrinks with the time left.
- **Our `r`.** The best value a unit of subscription quota can have is the API dollars it saves. So price a task's quota use at `api_equiv(task) × P(window runs out)`, where `api_equiv` is what the same tokens would cost on the API at list price. That is bounded above by the API price (never pay in quota more than the paid alternative) and below by zero (free when surplus). This is my adaptation, not a published algorithm.
- **Caveat on optimality.** Bid-price control is not optimal in general ([Talluri & van Ryzin, §3](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)), but it is asymptotically optimal as volume and capacity grow together. Our units (a task is 0.1% to a few percent of a window) are small relative to capacity, which is the favourable regime.

### 2.4 Nested windows: 5-hour inside 7-day

- A task burns the 5-hour and 7-day windows of the same profile at once, so each window is its own constraint row and its own price. The option's total quota cost is the sum `Σ_w μ_w · a_w`, because a column's reduced cost in an LP is the sum over rows. Using max would undercount whenever both are tight.
- Cap the sum at `api_equiv`: if both windows are nearly exhausted the quota is worth no more than the paid alternative.
- The windows differ in what leftover means. Leftover 5-hour quota is lost at the next 5-hour reset even if the week has plenty; leftover weekly quota is lost only at the weekly reset. If the week is the binding constraint, the 5-hour price is near zero but the weekly price is high, and the sum is correct without special cases. Neither window should be forecast with the same horizon: use a short smoothing window for the 5-hour burn rate and a longer one (about a day) for the weekly rate, because work is bursty by hour of day.
- Sub-limits that do not nest (a model-specific weekly cap) are extra rows applied only to options using that model.

### 2.5 Fixed vs rolling windows

- **Fixed window with a known reset time**: `τ = reset − now`, remaining `R = 100 − used`. Anthropic's help page says the session limit "resets every five hours" and weekly limits "reset once every week" ([Claude Help Center](https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work)); Codex shows its 5-hour window on a per-plan table ([OpenAI](https://developers.openai.com/codex/pricing/)). I did not verify whether the 5-hour window starts at the first message of a session; the router should read the reported reset time and, when no window is active, assume a full window ahead with nothing used.
- **Rolling window** (usage decays continuously) is a token bucket: level refills at rate `r = B/W` up to depth `B` ([Wikipedia](https://en.wikipedia.org/wiki/Token_bucket)). Waste happens only when the bucket is full and overflows, not at a reset. Use the same formula with `τ` = time until the bucket would be full at the current burn rate, capped at `W`. I did not find which vendors use which; treat it as a config flag per window.
- **Pacing a reset.** Ad platforms reset daily budgets and pace toward them by setting a target cumulative spend per interval proportional to expected supply, not linear in time, and then controlling the gap ([pacing guide](https://arxiv.org/html/2503.06942v2)). For us the "supply" is the historical share of the week's work that falls in each hour. A flat schedule is the default.

### 2.6 Budget pacing methods from advertising

- **Throttling.** Keep a participation probability `p`. If spend is ahead of schedule, `p ← p·(1 − λ)`; if behind, `p ← min(1, p·(1 + λ))` ([pacing guide, Algorithm 1](https://arxiv.org/html/2503.06942v2)). Cheap, but it drops traffic. We cannot drop tasks, so the analogue is "send fewer of them to this profile".
- **Bid-based multiplicative pacing.** Scale a base bid by a multiplier derived from the dual variable; that is exactly the price `μ` above. Dual mirror descent is the principled version.
- **PID.** Setpoint is target cumulative spend; the error between target and actual drives the control signal ([pacing guide, ch. 3](https://arxiv.org/html/2503.06942v2); [Zhang et al., feedback control for RTB](https://ar5iv.labs.arxiv.org/html/1603.01055)). Useful when the response of consumption to the price is unknown. A pure P-controller on pace error is enough for us: `μ_r ← μ_r·exp(β·(used_frac − sched_frac))`, with `β ≈ 3`. This is an inference from the sources, not something they prescribe.
- Smooth delivery is the stated goal of this literature ([Lee et al. smooth budget delivery](https://ar5iv.labs.arxiv.org/html/1305.3011)): avoid burning the budget early and avoid bursts. Our "do not drain one subscription while another idles" is the cross-account version of the same goal.

### 2.7 Load balancing across rate-limited keys

- **Weighted round robin / weighted random.** LiteLLM's default `simple-shuffle` picks a deployment weighted by configured rpm/tpm or an explicit weight, and the docs call usage-based routing "bad for perf" ([LiteLLM routing](https://docs.litellm.ai/docs/proxy/load_balancing)). Rpm/tpm are only routing hints unless `enforce_model_rate_limits` turns them into hard limits. Cooldowns and fallbacks handle failures ([LiteLLM routing](https://docs.litellm.ai/docs/routing)).
- **Price-weighted.** OpenRouter drops providers with outages in the last 30 seconds, then picks among the cheapest weighted by inverse square of price ([docs](https://openrouter.ai/docs/features/provider-routing)). A soft argmin avoids herding on one cheapest endpoint.
- **Power of two choices.** Sampling two servers and picking the less loaded gives an exponential improvement in maximum load over one random choice ([Mitzenmacher 2001](https://eecs.harvard.edu/~michaelm/postscripts/tpds2001.pdf)). With five or six profiles we can evaluate all of them, so the trick is unnecessary; what matters from this literature is that load information is stale, so count in-flight work.
- **Token buckets** model rate limits as refill rate plus burst depth ([Wikipedia](https://en.wikipedia.org/wiki/Token_bucket)). They fit tokens-per-minute limits, which are not our binding constraint; the percent windows are.
- Takeaway: the gateways balance for throughput and uptime, with no notion of "this capacity expires" or "this one is cheaper to use now". Our objective is different, so the selection rule must be ours; reuse their failure handling (cooldown, retry on the next option).

### 2.8 Risk of running out mid-task

- Each option carries a token estimate `n` and spread `σ` (from the decider's task class, or the history of similar tasks). Convert to percent of window: `a = k·n/10⁶`, where `k` is percent of window per million tokens for that (profile, model, effort). Anthropic says consumption depends on model, conversation length and effort level ([help page](https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work)), so `k` must be learned per option, from percent-used deltas around finished tasks (an exponential moving average; discard samples taken while other work shared the window).
- **Chance constraint.** Allow the option only if `P(a_actual > R_eff) ≤ ε`. With a normal assumption that is `a + z·σ_a ≤ R_eff`, `z = 1.65` for 5%. Without any distributional assumption, Cantelli's inequality gives `z = √((1−ε)/ε)`, which is 4.36 for 5% and 3 for 10% (standard result, my arithmetic). Agent token use is plausibly heavy-tailed, so a middle default of `z = 2` with a log-normal fit is sensible; I did not find a published measurement of agent token variance, so this is an assumption to be replaced by logged data.
- `R_eff = R − inflight − reserve`. `inflight` is the sum of `a_hat` reserved for tasks already dispatched but not yet reflected in the reported percent. `reserve` covers rounding and lag in the reported percent; default 3 points.
- Do not lean on Anthropic's "wrap-up allowance": it applies only to a response already in progress, is capped, and is rolling out gradually ([help page](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan)).
- If a limit error occurs anyway, fall to the next-best option, as gateways do with cooldown and fallback.

### 2.9 Recommended algorithm (per decision)

Keep per window: `used`, `reset`, burn-rate mean `m` and std `s` in percent per hour (smoothed over all sources, since the main agent shares the window with the router), and `inflight`. Keep per option: `k` (percent per million tokens), `n`, `σ`, `q`, and for API options `$/M tokens`.

```
def choose(task, options, now):
    ref = max(api_equiv(o) for o in options) or 1e-9          # scale: cost of the dearest option
    best = None
    for o in options:
        if o.q < task.q_min: continue                          # quality floor from the decider
        if o.kind == "api":
            d = o.n * o.price / 1e6
            if d + Z * o.sd * o.price / 1e6 > month.left - month.inflight: continue
            cost = d * (1 + p_run(month, now))                 # budget scarcity: at most 2x
        else:
            a = o.k * o.n / 1e6                                # percent of each window this task burns
            hi = a + Z * o.k * o.sd / 1e6                      # chance-constrained upper estimate
            if any(hi > 100 - w.used - w.inflight - RESERVE for w in o.windows): continue
            p = sum(p_run(w, now) for w in o.windows)          # nested windows: prices add
            cost = api_equiv(o) * min(1.0, p)                  # opportunity cost, capped at API price
        score = o.q - KAPPA * cost / ref + EPS * slack(o)      # slack = expected waste, tie-break
        if best is None or score > best[1]: best = (o, score)
    if best: reserve_inflight(best[0])                         # None -> escalate or wait for reset
    return best and best[0]

def p_run(w, now):                     # P(demand before reset exceeds what is left)
    tau = max(w.reset - now, 0) / 3600
    R = 100 - w.used - w.inflight
    return 1 - Phi((R - w.m * tau) / (w.s * sqrt(tau) + 1e-6))
```

Notes on the pseudocode: `Phi` is the standard normal CDF; `api_equiv(o)` is `o.n × list price of o's model / 10⁶`; `slack(o)` is the largest expected unused fraction among o's windows at reset (`max(0, R − m·τ)/100`). After each finished task: release `inflight`, update `k` from the percent delta, update `m, s` from percent-used deltas over time.

Parameters and defaults:

| Parameter | Meaning | Default | Why |
|---|---|---|---|
| `KAPPA` | quality you give up to save 100% of the dearest option's cost | 0.3 | No data behind it; tune against logged outcomes. Larger means cheaper routing |
| `Z` | safety multiplier on the token spread | 2 | Between normal 5% (1.65) and Cantelli 5% (4.36) |
| `RESERVE` | percent points kept untouched per window | 3 | Covers rounding and reporting lag |
| burn-rate smoothing | EWMA half-life for `m, s` | about 1/4 of the window (75 min for 5 h, about 1.5 days for 7 d) | Short enough to follow today's pace, long enough to ignore one big task |
| `EPS` | tie-break weight on expected waste | 0.01 | Prefers the profile with more leftover when prices are both about 0; plays the role MSVV's `ψ` would |
| price cap | `min(1, Σ p_w)` | 1 | Quota is never worth more than the API alternative |
| `p_run(month)` | pacing price for the dollar budget | same formula, dollars, horizon = days to month end | Gives the proxy budget a smooth ceiling |

Optional correction (from dual mirror descent): once per hour multiply each window's price by `exp(−η(ρ − realised_rate))` with `ρ = R/τ`. It brings in the proof that resources are not depleted early, but it is not needed to start.

## 3. What this means for our design

1. **Make the decision a single `argmax` of `q − κ·(dollars + Σ_w price_w·usage_w)`.** This is the Lagrangian form of the LP in §2.1, and it is what dual mirror descent, BwK and the revenue-management bid prices all reduce to. No call to the main model is needed; the 4B decider supplies `q_min`, `n` and `σ` and nothing else. Evidence: [Balseiro et al.](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf), [Badanidiyuru et al.](https://arxiv.org/pdf/1305.2545).
2. **Price a quota unit as `api_equiv × P(window runs out before reset)`.** It is a few lines, needs only percent used, reset time and a burn rate, gives "subscription first" automatically when surplus (price ≈ 0), and falls to zero near reset. Evidence: [Talluri & van Ryzin eq. 19](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf).
3. **Sum the per-window prices, cap at the API-equivalent.** Evidence: bid-price control sums leg prices ([same paper](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)); keep separate forecast horizons for 5-hour and 7-day.
4. **Do not use MSVV `ψ(f)` as the main rule.** It ignores the reset and would charge for leftover quota that is about to expire. Use its idea (prefer the emptier budget) only as a tie-break, which the `EPS·slack` term does. Evidence: [Mehta et al.](https://people.eecs.berkeley.edu/~vazirani/pubs/adwords.pdf) assume budgets that never refill.
5. **Start stateless; add feedback only if the forecast drifts.** The probability price has no learning rate to tune. Dual mirror descent's `O(√T)` regret is weak for the few tasks in a 5-hour window ([same bound](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf)), so use its update only as an hourly correction, and use the P-controller on pace error as the fallback if the burn-rate forecast is poor ([pacing guide](https://arxiv.org/html/2503.06942v2)).
6. **Reserve in-flight tokens and use a chance constraint.** Subagents run in parallel and the percent is reported late, so without a reservation two tasks can both pass the check and overshoot. Evidence for the load-information-is-stale problem: [Mitzenmacher](https://eecs.harvard.edu/~michaelm/postscripts/tpds2001.pdf); the mid-response cut-off is only partly softened by [Anthropic's wrap-up allowance](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan).
7. **Learn percent-per-token `k` per (profile, model, effort) from observed deltas.** The subscriptions expose percent only, and consumption depends on model, length and effort ([Anthropic](https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work)). This estimate is the weakest link in the design and decides how good all the prices are.
8. **Reuse gateway failure handling, not their selection.** Cooldown, retry and fallback exist in LiteLLM ([docs](https://docs.litellm.ai/docs/routing)); their rpm/tpm weighting and OpenRouter's inverse-price weighting do not know about expiring quota.
9. **Treat the model-choice part as separate from the budget part.** If quality scores are given as a table, no bandit is needed; if they must be learned, BwK's UCB-on-reward and LCB-on-consumption is the template ([Badanidiyuru et al.](https://arxiv.org/pdf/1305.2545)).

Not verified: which vendor windows are fixed vs rolling; how the reported percent is rounded; any empirical spread of agent token use; whether `KAPPA = 0.3` is a good trade-off. These need a few days of logged data from the real profiles.

## 4. Sources

- https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf — Dual Mirror Descent paper (read in full, algorithm and Theorem 1).
- https://arxiv.org/pdf/1305.2545 — Bandits with Knapsacks (read abstract, results, regret bounds).
- https://arxiv.org/abs/1305.2545 — BwK abstract page.
- https://people.eecs.berkeley.edu/~vazirani/pubs/adwords.pdf — AdWords and generalized online matching (search excerpt: ψ, 1−1/e, Balance).
- https://ics.uci.edu/~vazirani/Adwords.pdf — same paper, second copy (search excerpt).
- https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf — Talluri & van Ryzin 1998, bid-price controls (read §1–5 selectively).
- https://business.columbia.edu/sites/default/files-efs/pubfiles/3943/vanryzin_optimal_dynamic_pricing.pdf — Gallego & van Ryzin (abstract only).
- https://arxiv.org/html/2503.06942v2 — A Practical Guide to Budget Pacing Algorithms (read throttling and PID chapters).
- https://ar5iv.labs.arxiv.org/html/1305.3011 — Smooth budget delivery in RTB (search excerpt).
- https://ar5iv.labs.arxiv.org/html/1603.01055 — Feedback control of real-time display advertising (search excerpt).
- https://arxiv.org/html/2509.02718 — PORT, training-free online LLM routing under token budgets (search excerpt).
- https://eecs.harvard.edu/~michaelm/postscripts/tpds2001.pdf — Mitzenmacher, power of two choices (search excerpt).
- https://docs.litellm.ai/docs/routing — LiteLLM router strategies, cooldowns.
- https://docs.litellm.ai/docs/proxy/load_balancing — LiteLLM load balancing table and rate-limit enforcement.
- https://openrouter.ai/docs/features/provider-routing — OpenRouter inverse-square-price load balancing.
- https://en.wikipedia.org/wiki/Token_bucket — token bucket and hierarchical token bucket.
- https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan — Claude Code limits, wrap-up allowance.
- https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work — Claude usage limits and what affects them.
- https://developers.openai.com/codex/pricing/ — Codex 5-hour message limits per plan (search excerpt).
- https://r.jina.ai/https://portkey.ai/docs/product/ai-gateway/load-balancing — Portkey load balancing page; fetch returned nothing usable, so no Portkey claims are made.
- https://arxiv.org/abs/2007.04096 and https://arxiv.org/abs/1904.01609 — opened by mistake (wrong IDs, unrelated papers); ignore.
