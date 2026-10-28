# How model-router decides

model-router answers one question per task: which **profile** (account or key), which **model** and which **effort** should run it. It reads the task, predicts how likely each option is to succeed and what it costs, prices subscription quota by the chance it runs out before it resets, keeps the options that are good enough, and takes the cheapest one in the class the user ranks first. No language model is called to make the choice; a free local classifier reads the task, and the rest is arithmetic.

The code is `model_router/core.py` (pure functions, no I/O). Everything it reads comes from `model_router/sources.py`.

## 1. The problem

A user has several profiles of three kinds:

| Class | Examples | What it costs |
|---|---|---|
| `subscription` | Claude Pro/Max, ChatGPT Plus/Pro (Codex) | Nothing extra, but 5-hour and 7-day windows run out, and the human needs them too |
| `budget` | A gateway with a monthly spending limit | Prepaid dollars that reset monthly |
| `api` | Pay-per-token keys and gateways | Real money per token |

Each task can run on any (profile, model, effort) the profile serves. The goals pull against each other:

1. Finish the task: pick a model and effort strong enough for it.
2. Spend subscription quota before money (the default order; the user can change it).
3. Never strand the human: do not drain the window they are working in.
4. Use quota that would otherwise expire, and spread work so one account is not drained while another sits idle.
5. Spend as little real money as possible.

### The exact problem is a linear programme

Write `x_to = 1` when task `t` runs on option `o`. Each option has a success probability `q_to`, a dollar cost `c_to` (zero on a subscription) and uses `a_tor` of each resource `r` (a 5-hour window, a 7-day window, a monthly budget), of which `R_r` is left:

```
maximise    Σ_t Σ_o x_to · (q_to − κ·c_to)
subject to  Σ_o x_to = 1                 for every task t
            Σ_t Σ_o a_tor · x_to ≤ R_r   for every window or budget r
            x_to ≥ 0
```

Relaxing the window rows with multipliers `μ_r` splits it into one small problem per task: pick `argmax_o [q_to − κ·c_to − Σ_r μ_r·a_tor]`. The multipliers are **shadow prices**: what one more unit of a window is worth. Complementary slackness gives the use-it-or-lose-it rule for free: a window that will not run out before its reset has price zero ([Balseiro, Lu and Mirrokni 2020](https://proceedings.mlr.press/v119/balseiro20a/balseiro20a.pdf); [Talluri and van Ryzin 1998](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)).

Tasks arrive one at a time and the future is unknown, so the router cannot solve the programme once. It does what airline revenue management does with seats that expire at departure: it estimates each window's price from the chance it runs out (a **bid price**) and makes the per-task choice with that price. Bid-price control is asymptotically optimal when each sale is small against capacity ([Talluri and van Ryzin](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf)); a subagent task is 0.1% to a few percent of a window, which is that regime.

## 2. The decision, step by step

```
task text ──► 1 read the task ──► 2 quality per option ──► 3 cost per option
                                                                  │
          pick ◄── 6 choose ◄── 5 filter ◄── 4 price the quota ◄──┘
```

### Step 1: read the task

A task is a mix of eight kinds of work with probabilities (`code_edit`, `code_review`, `web_research`, `social_search`, `vision`, `writing`, `analysis`, `mechanical`), a difficulty from 0 (trivial) to 4 (very hard), and needs (`vision`, `web`).

The reader is a **typed decider**: a small model that answers typed questions (choice, score, yes/no) in one forward pass, without generating text. The default is Perplexity's `pplx-decider-flash-v1.2` running locally (`http://127.0.0.1:8099/v1/systemone`), so reading a task costs nothing and takes under a second. It is asked six questions: the kind (with a probability per kind), the difficulty for a strong senior engineer, whether it needs images, the internet, or posts from X, and whether a fresh assistant could do it from the text alone.

The decider ranks difficulty correctly but squeezes it toward the middle: tasks labelled 0 to 4 come back as 0.9 to 2.9 on average. A linear map fitted on 30 labelled tasks, `level = −1.27 + 1.72 × score`, stretches it back (section 4.2). It lives in the catalogue as `decider_scale`.

Without a decider, keyword rules give a rougher reading. The caller can pin any part (`--kind`, `--difficulty`, `--needs`), and pinning both kind and difficulty skips the reader.

### Step 2: predict the chance of success

Each option gets a success probability from **item response theory**, the model used to score exams and, for routing, by [IRT-Router](https://arxiv.org/abs/2506.01048): a model has an ability `θ`, a task has a difficulty `b`, and

```
P(success) = Σ_k  π_k · σ( 1.4 · (θ_mk − b) )
```

where `π_k` is the task's share of kind `k` and `σ` is the logistic function.

- **Ability** comes from the [Artificial Analysis Intelligence Index](https://artificialanalysis.ai/leaderboards/models), which scores each model at each effort: `θ = (index − 19) / 11.2`. Read it as anchors: a model at index 47 has even odds on a very hard task and about 85% on a hard one, and every 8 index points multiply the odds by e. These are hand-set priors that the feedback step corrects.
- **Effort** moves ability along the model's own curve, but kinds of work that gain little from thinking get only part of the step. Each kind has a reasoning factor `r_k` (analysis 1.2, code 1.0, research 0.6, writing 0.5, social search 0.4, bulk edits 0.3), and `θ_mk = θ_mid + r_k·(θ_effort − θ_mid) + δ_mk`. The research behind it: effort is the biggest quality lever on hard reasoning (Opus 5.5 runs from index 42 at `low` to 58 at `max`), while thinking in subagents doing simple work is mostly wasted ([Żywot 2026](https://arxiv.org/html/2601.11327v2); Anthropic's effort docs name `low` for subagents).
- **Strengths** are the `δ_mk` offsets: Grok +1.2 on social search (it is the only model with native X search), Gemini 4 Argon +0.4 on vision, GPT-6 Luna −0.4 on code edits (16% on Terminal-Bench 4.0 against Haiku 5.5's 39% at the same price), and so on. Each comes from vendor or leaderboard evidence, recorded in `model_router/catalogue.json`. Every model without native X search gets −0.2 on social search.
- **Difficulty** maps levels 0 to 4 onto `b = −1.5, −0.5, 0.5, 1.5, 2.5`.

### Step 3: estimate the cost

```
cost = size(difficulty) × Σ_k π_k · s_k × AA cost per task(model, effort) × price factor
```

Artificial Analysis publishes, per model and effort, what one index task costs at list price, which already folds in how many thinking and tool tokens that model burns at that effort. A job's size is counted in those tasks: 0.1 (trivial), 0.3, 1, 2.5, 6 (very hard), times a factor per kind (`s_k`: research 1.2, writing 0.6, review 0.7). The profile's price factor covers routes that sell below or above list price.

On a subscription the same number means **API-equivalent dollars**: how much of the window the job will use, since a window is sized in roughly that currency (a Max 5x 5-hour window holds about $60 of API-priced work; the plan figures are in `sources.py` and a profile's `capacity_usd` overrides them).

### Step 4: price the quota

For a subscription or budget, each window `w` the option draws on gets a price `λ_w`: the chance the window runs out before it resets if this job is taken now.

```
take   = (cost + pending) / capacity              # this job's share of the window
left   = 1 − reserve − used − take
pace   = (used + 0.5·0.25) / (elapsed + 0.25)      # use so far per window, pulled toward half a window
demand ~ lognormal, mean = pace × time left, σ = 0.8
λ_w    = P(demand > left)
```

- This is the probabilistic bid price `λ = r·P(D > x)` of [Talluri and van Ryzin](https://business.columbia.edu/sites/default/files-efs/pubfiles/3933/network_revenue_management.pdf) (eq. 19), with demand forecast from the window's own pace. It falls to zero as the reset nears with quota left over, which is exactly "use it or lose it", and it rises toward one when the human is burning the window fast.
- **One job draws on every window at once**, so its price is the sum over windows (an LP column's reduced cost sums over its rows), capped at 1: quota is never worth more than paying for the same work. Model-specific weekly caps (Claude's Opus or Sonnet weekly limit) are extra windows that apply only to that model.
- **Reserve** keeps part of each window for the human: 15% of the 5-hour window, 10% of the weekly one, 5% of a monthly budget by default.
- **Pending** is routed work not yet visible in the reading (the ledger's last two hours of jobs on that profile since the reading), so parallel subagents cannot all pass the same check and overshoot.
- **Chance constraint**: a job must fit twice over (`1 − reserve − used − 2·take > 0`), because job sizes are heavy-tailed and running out mid-job wastes the work.
- The option's **effective cost** is `cost × (λ + 0.02)`: a subscription is never quite free, since it still spends quota. On a pay-per-token profile the effective cost is the cost.
- A profile whose quota cannot be read is priced at `λ = 0.3`, neither free nor dear.

### Step 5: filter

An option is dropped, with the reason recorded, when the catalogue has no rating for the model; the task needs posts from X and the model has no native X search while another reachable model has (only Grok's `x_search` does; with none reachable the pick goes ahead with a note that it has only web search); the task needs images and the model reads only text (input types come from [models.dev](https://models.dev)); the task's context does not fit; the effort is not on that model's own ladder (Kimi, GLM and DeepSeek take `low/high/max`, Gemini `low/medium/high`, Grok `low` to `xhigh`, Claude and OpenAI five levels); the job would cross a window's reserve; or its effective cost is over the per-job limit ($15 by default).

### Step 6: choose

1. **Tier by class**, in the user's order: subscription, then budget, then API by default. A subscription likelier than 50% to run out (`λ > 0.5`) drops to a last-resort tier: it is kept for the human and used only when nothing else is acceptable.
2. **Keep the good options**: those within `band` of the best success probability in the usable tiers, or at or above the quality target.
3. **Take the cheapest**: lowest tier first, then lowest effective cost; ties go to higher quality, then lower quota price.

This is the shape every shipped router uses: Azure's model router keeps models within 1–2% of the best for Balanced and 5–6% for Cost, then takes the cheapest; Copilot and Cursor expose the same three-way dial ([Azure](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/model-router), [Copilot](https://docs.github.com/en/copilot/concepts/auto-model-selection), [Cursor](https://cursor.com/docs/cursor-router)). The three modes are:

| Mode | Quality target | Band |
|---|---|---|
| `quality` | 0.95 | 0.02 |
| `balanced` (default) | 0.95 | 0.04 |
| `cost` | 0.90 | 0.06 |

The values came from a sweep of the synthetic week in section 4.1: the band moved cost the most.

### Worked example

Two Claude Max 5x subscriptions and a gateway; the task is a moderate code edit (difficulty 2).

- `work`: 62% of the 5-hour window used one hour in, and 40% of the week with 3.5 days left. For the $0.88 job below its 5-hour price is 0.97 and its weekly price 0.20; the sum caps at 1, which is over 0.5, so `work` is kept for the human.
- `home`: 10% used, resets in an hour, weekly window 30% used with 20 hours left. Both prices are 0.00: this quota will expire unused.
- The best usable option is Opus 5.5 at `max` (0.988), so the good set is everything at 0.948 or above.
- In the subscription tier the cheapest good option is `home · claude-sonnet-5-5 · high`: success 0.953, $0.88 API-equivalent, effective $0.018. Opus at `medium` (0.971) costs $1.34 of the same quota, and the gateway's GPT-6.1 Sol at `medium` (0.949, $0.21 real money) is in a later tier.

`tests/test_router.py::test_the_worked_example_in_the_algorithm_doc` checks these numbers. `model-router route --explain "<task>"` prints the same table for a real task: the shortlist with each option's success, cost, quota price and effective cost, and every dropped option with its reason.

### Where the decision is applied

- **Native subagents.** A PreToolUse hook on Claude Code's Agent tool runs the same steps over the session's own model aliases (a native subagent cannot leave its session's account) and rewrites the call's `model` and `effort`. It respects a call that names both and a call naming a model it cannot rate, skips forks (they keep the parent's model) and profiles the user disabled, considers only aliases the profile serves, waits at most 5 seconds for the decider and then uses the keyword rules, reads models.dev only from cache, and on any error lets the call go ahead unchanged. The read of pending work, the pick and its ledger row happen under one file lock, so parallel subagent launches see each other's reservations.
- **Dispatched jobs.** `model-router dispatch` routes over every configured profile and launches the vendor's own CLI on the pick through agent-dispatch or ccmorph. This is the only way to reach another account, because Claude Code has no per-subagent base URL or login ([#38698](https://github.com/anthropics/claude-code/issues/38698)).

Both paths write the job to a ledger, so the next decision counts work that is running but not yet visible in a quota reading.

## 3. Learning from outcomes

`model-router feedback <job> pass|fail` moves the model's ability on the job's kinds one logistic-regression step toward the outcome: `δ_mk += 0.3 × (outcome − predicted) × π_k`. The offsets live in `~/.config/model-router/skills.json` and add to the catalogue's. A model that keeps failing at a kind of work is routed less for it; one that keeps passing gets more of it.

This is the cheapest version of the online layer the literature recommends once outcomes accumulate (prior-informed bandits such as [PILOT](https://arxiv.org/html/2508.21141v2) and [ParetoBandit](https://arxiv.org/html/2604.00136v2)). Only the chosen option's outcome is observed, which is enough ([BaRP](https://arxiv.org/html/2510.07429)).

## 4. Evidence

### 4.1 A synthetic week

`model-router simulate --seed 1` to `--seed 5` replay the same week against several policies: three subscriptions with a human working on them (Max 5x, Pro, ChatGPT Plus), a $500 monthly budget, and a pay-per-token gateway. About 720 tasks arrive per week with a mix of kinds and difficulties, and each job's real size is its estimate times a heavy-tailed draw. Five seeds:

| Policy | Paid $ per week | Mean success |
|---|---|---|
| One model (Opus 5.5 medium on the gateway) | 1,281–1,561 | 0.919–0.932 |
| Role table (cheap, middle, strong model by difficulty) | 1,517–1,894 | 0.905–0.912 |
| Account pool (round robin over subscriptions) | 536–746 | 0.912–0.925 |
| model-router, cost | 442–728 | 0.889–0.898 |
| **model-router, balanced** | **765–1,069** | **0.917–0.926** |
| model-router, quality | 1,316–1,708 | 0.926–0.935 |
| model-router, balanced, gateway only (no prepaid quota) | 1,069–1,377 | 0.915–0.925 |

What the paid column does not show:

| Policy | Human blocked (10-minute slots per week) | Jobs cut off by a limit |
|---|---|---|
| One model | 0 | 0 |
| Account pool | 1,163 | 5.8 |
| model-router, balanced | 2 | 0.2 |

- Balanced spends 32–40% less than one strong model on every seed, for 0.2–0.6 points less mean success.
- Where the saving comes from: the same router limited to the gateway, with no subscription or budget to spend, is 10–17% cheaper than one strong model. That part is model choice alone; the rest comes from spending prepaid quota that would otherwise expire, without taking the human's.
- The account pool is cheaper only because it takes the human's quota: they are locked out 1,163 times a week. The router's quota price keeps the window the human is burning, and uses the idle ones.
- With one model on the gateway, $226 of weekly subscription quota expires unused each week; under the router, $39.
- Tested and dropped: spending quota that is about to expire on the strongest good option, rather than the cheapest, moved mean success by 0.0002 and paid dollars by +0.7% over the five seeds. The simpler rule stays.

Limits of this evidence: the week is synthetic, and success is scored with the same ability table the router uses. It measures **allocation** (money, quota, lock-outs) given the table, not whether the table is right. Section 4.2 and the feedback step are about the table.

### 4.2 How well the decider reads tasks

`bench/decider.py` compares the decider and the keyword rules against 30 labelled tasks: one frontier model's reading of each task, which is what the main agent would decide if asked. Each reading is routed over all 14 catalogue models and the pick is scored on the labelled task.

| Reader | Kind right | Level error |
|---|---|---|
| Decider, raw | 90% | 0.56 |
| Decider, calibrated | 90% | 0.35 (0.37 out of sample) |
| Keyword rules | 67% | 0.88 |

| Reader | Success lost vs the labelled route | Mean cost vs the labelled route |
|---|---|---|
| Decider, raw | 0.006 | 1.82× |
| Decider, calibrated | 0.008 | 1.37× |
| Keyword rules | 0.011 | 2.90× |

- The decider found every task's needs (images, internet, X posts) and took 0.6 to 0.9 seconds a call for its six questions. The keyword rules missed 7% of needs. The decider's three wrong kinds were bulk edits read as code edits.
- A misreading rarely costs success, because several near-equal options sit inside the band; it mostly costs money, by over-reading an easy task. Calibration cut the overspend from 1.82× to 1.37×.
- The remaining large misses are hard tasks read as moderate (a websocket race read at 2.2 instead of 3). Reading the prompt alone has a floor: [SWE-Router](https://arxiv.org/abs/2607.00053) shows a task description cannot always tell a typo from a multi-module refactor.

So the answer to "decision model or main agent?" is: the free decider gets 90% of kinds and lands within half a level of the main agent's reading, at no cost and without spending any of the main agent's context. The main agent can still pin difficulty when it knows better.

### 4.3 Live checks

- `model-router dispatch` routed a test task through agent-dispatch to `gateway · gpt-6-luna · xhigh`; the answer was correct and cost about a cent.
- Quota readers returned live windows for a Claude subscription (OAuth usage endpoint), a Codex subscription and a monthly budget.
- In a headless Claude Code session on a gateway, the main agent called the Agent tool without a model; the hook set `haiku` at `low` effort, and the subagent ran on DeepSeek V4.1 Flash (the gateway's `haiku`), answered correctly and took 1.5 seconds. A local relay that logged each request's model and effort confirmed both reach the API: the subagent's request carried `model: deepseek-v4.1-flash` and `output_config.effort: low`, and a call that named only `sonnet` went out as `claude-sonnet-5-5` with the hook's `low` while the main session stayed at `medium`.

## 5. Parameters and where they come from

| Parameter | Value | Source |
|---|---|---|
| Ability anchors | index 19 at zero, 11.2 per logit, slope 1.4 | Hand-set priors; corrected by feedback |
| Difficulty logits | −1.5, −0.5, 0.5, 1.5, 2.5 | Hand-set, one logit per level |
| Job sizes | 0.1, 0.3, 1, 2.5, 6 AA tasks | Hand-set; heavy-tailed on purpose |
| Demand spread σ | 0.8 (lognormal) | Assumption; agent token use is heavy-tailed |
| Pace prior | half a window per window, weight a quarter window | Keeps the forecast sane early in a window |
| Fit margin | 2× | Between the normal 5% bound (1.65) and Cantelli's (4.36) |
| Reserve | 15% (5 h), 10% (7 d), 5% (month) | Room for the human |
| Protect above | λ = 0.5 | A window likelier than not to run out belongs to the human |
| Floor | 0.02 | A subscription still spends quota |
| Unknown quota | λ = 0.3 | Neither free nor dear |
| Bands | 0.02 / 0.04 / 0.06 | Azure's published bands, then the sweep |
| Decider scale | −1.27 + 1.72 × score | Fitted on 30 labelled tasks |
| Feedback step | 0.3 | One logistic-regression step |

## 6. Known limits and what comes next

1. **Window capacity is the weakest number.** How many API-equivalent dollars a 5-hour window holds comes from public plan figures; a wrong figure skews every quota price on that profile. Next: learn it per profile from the change in percent used around routed jobs, discarding readings where the human was also working ([research note 2, §2.8](research/2-budget-optimisation.md)).
2. **The ability table is a prior.** Benchmarks are self-reported and disagree by 2–6 points between vendors. Feedback corrects it per model and kind; a bandit layer with forgetting is worth adding after a few hundred logged outcomes.
3. **Failure is priced only through the quality target.** A cheaper model is worth it only if `P_cheap > 1 − (C_strong − C_cheap)/(C_strong + W)`, where `W` is the cost of a failed attempt, including the main agent's wasted turn ([T2MO](https://arxiv.org/html/2608.08528)). Adding `W` would replace the band with a principled number.
4. **The prompt has a floor.** For expensive coding tasks, deciding after a cheap first look (a partial trajectory, or two quick drafts that agree or not) beats deciding from the prompt ([SWE-Router](https://arxiv.org/abs/2607.00053), [DART](https://arxiv.org/abs/2606.23181)).
5. **A statusline reading is only as fresh as the last session on that profile.** A window whose reset has passed is treated as empty; between readings, the router counts only its own routed work.

<a id="research"></a>The five research notes behind this design, each with its sources, are in [docs/research](research/): [routing papers](research/1-routing-papers.md), [budget optimisation](research/2-budget-optimisation.md), [products and terms of service](research/3-products-landscape.md), [when to delegate](research/4-subagent-delegation.md), [model capabilities and prices](research/5-model-capabilities.md). They were written on 2026-10-28 by five research agents; every claim cites a page they opened.
