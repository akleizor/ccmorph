# Topic 1: LLM routing algorithms in the research literature

Researched 2026-10-28. Every claim below comes from a page I opened (see Sources). Where I could not confirm something (usually code availability) I say so.

## 1. Key findings

- **The decision rule is almost always the same one-liner**: pick `argmin_m [ expected_loss(x,m) + λ·cost(m) ]`; UniRoute states it as its rule (7) ([UniRoute](https://arxiv.org/pdf/2502.08773)) and CARROT proves a plug-in version of it minimax-optimal ([CARROT](https://arxiv.org/pdf/2502.03261)). Methods differ only in how they estimate `expected_loss`.
- **Under unified evaluation, fancy routers barely beat simple baselines**: LLMRouterBench (400K instances, 33 models, 10 routers) finds many routers, including the commercial OpenRouter, fail to reliably beat the best single model, and a large gap to the oracle remains, driven by "model-recall failures" ([LLMRouterBench](https://aclanthology.org/2026.findings-acl.1881.pdf)).
- **Training-free cold start works from model metadata**: structured profiles built from model family, description and published benchmark scores route new models with no query-response data (TextGNN-4hop 0.580 vs best-single 0.547 vs oracle 0.679) ([RouteProfile](https://arxiv.org/html/2605.00180v2)); flat text concatenation does worse than best-single.
- **Decoupling "what the task needs" from "what each model offers" removes retraining**: GitHub's HyDRA predicts a 4-dimensional requirement vector per query and matches it against a YAML capability profile per model, so a catalogue change needs no retraining ([HyDRA](https://arxiv.org/html/2605.17106)).
- **Budget-aware online routers exist and one is open source**: ParetoBandit combines LinUCB, a primal-dual budget pacer, geometric forgetting and a hot-swap registry; it onboards a new model in about 142 steps ([ParetoBandit](https://arxiv.org/html/2604.00136v2)).
- **Effort routing is its own small literature, and agent-step-level selection is the closest to our case**: Ares picks the lowest sufficient reasoning level per agent step and cuts reasoning tokens by up to 52.7% with minimal success loss ([Ares](https://arxiv.org/html/2603.07915)); DART is training-free ([DART](https://arxiv.org/abs/2606.23181)).
- **For coding agents, the prompt alone is a weak signal**: SWE-Router argues a task description has a Bayes-error floor and routes on a partial trajectory instead ([SWE-Router](https://arxiv.org/abs/2607.00053)); Agent-as-a-Router finds that adding per-task-dimension performance statistics gives a 15.3% relative gain ([Agent-as-a-Router](https://arxiv.org/html/2606.22902v2)).

## 2. Details

### 2.1 Quality-prediction routers

Common setup: a query embedding `e(x)` goes into a predictor of success `p̂(x,m)`; the router applies the rule above.

**RouteLLM** ([paper](https://arxiv.org/abs/2406.18665), [repo](https://github.com/lm-sys/RouteLLM)).
- Rule: send to the strong model if `P(strong wins | q) ≥ α`, else weak. α is calibrated on sample traffic to hit a target share of strong calls (the repo ships `calibrate_threshold`).
- Predictors: similarity-weighted Bradley-Terry ranking, matrix factorisation, BERT classifier, Llama-3-8B causal classifier.
- Inputs: the query (OpenAI `text-embedding-3-small` embeddings for MF and SW). Only a strong/weak pair.
- Data: 65K Chatbot Arena pairwise votes over 64 models; plus either 1,500 MMLU-validation golds or an LLM-judge set (augmentation matters a lot out of distribution).
- Result: on MT-Bench, up to 75% fewer strong calls than random at 95% of GPT-4 quality; cost ratio 3.66× at 95% quality (MT-Bench), 1.41× (MMLU, 92%), 1.49× (GSM8K, 87%). Routers trained on GPT-4/Mixtral transferred to other model pairs without retraining.
- Code: Apache-2.0, 5.6K stars, last push 2024-08-10 (stale).

**FrugalGPT** ([paper](https://arxiv.org/abs/2305.05176)).
- Sequential cascade: call the cheapest API, score `g(q, answer)` with a small learned scorer, return if above a per-model threshold, else escalate. List order and thresholds are optimised under an average-cost budget.
- Data: labelled queries per target task. Result: matches GPT-4 with up to 98% lower cost or beats it by 4% at equal cost. Cost: every escalation pays for the failed cheaper attempt. I found no code link in the paper.

**Hybrid LLM** ([paper](https://arxiv.org/abs/2404.14618)).
- Router score is `Pr[q(S(x)) ≥ q(L(x))]`, with a quality-gap tolerance chosen at test time; trained on quality scores of both models.
- Result: up to 40% fewer large-model calls with no quality drop. Code: `m365-core/hybrid_llm_routing` (linked in the paper, not opened).

**RouterBench** ([paper](https://arxiv.org/abs/2403.12031), [repo](https://github.com/withmartian/routerbench)). 405K inference outcomes; defines the cost-quality deferral-curve evaluation most later work reuses. Its KNN/MLP baselines are fixed to the model set. MIT, last push 2024-06.

**RouterDC** ([paper](https://arxiv.org/abs/2409.19886), [repo](https://github.com/shuhao02/RouterDC)).
- Score is `softmax(cos(E(x), k_t))` over learnable LLM embeddings `k_t`; two contrastive losses (sample-LLM and sample-sample) so several good models are all treated as positives.
- Data: scored responses of every model on training queries. Gain: +2.76% in-distribution, +1.90% out-of-distribution over the best single model. No explicit cost term. No licence file; 77 stars.

**GraphRouter** ([paper](https://arxiv.org/abs/2410.03834), [repo](https://github.com/ulab-uiuc/GraphRouter)).
- Heterogeneous graph of task, query and LLM nodes; a GNN predicts the edge attributes (quality, cost). LLM nodes are embedded from a GPT-4o-written description (strengths, price, context length), so a new LLM needs no retraining.
- Result: at least 12.3% better than baselines across three effect-cost weightings and at least 9.5% better with new LLMs. MIT, last push 2025-12-30.

**EmbedLLM** ([paper](https://arxiv.org/abs/2410.02223), [repo](https://github.com/richardzhuang0412/EmbedLLM)). Encoder-decoder over a 112-model × 36K-question correctness matrix; the decoder predicts `σ(p1 − p0)` of model m solving question q from Hadamard(model emb, question emb). Router = argmax predicted correctness. No cost term; a new model needs a probe set of its own answers.

**IRT-Router** ([paper](https://arxiv.org/abs/2506.01048), [repo](https://github.com/Mercidaiha/IRT-Router)).
- `P̂(q,m) = σ(aᵀ(θ_m − b))`: each model has a multi-dimensional ability `θ_m`; each query has discrimination `a` and difficulty `b`, both predicted from the query embedding. Router: `argmax_m α·P̂ − β·C(m)`, α+β=1.
- A "warm-up" blends a new query's vector with its 5 nearest training neighbours (cold-start parameter λ, 0.3 to 0.4 best out-of-distribution).
- Data: 20 LLMs × 12 datasets of graded responses. On a held-out new model (Claude 3.5 Haiku) its quality prediction was far better than RouterBench's near-random baseline. Interpretable: abilities and difficulties can be inspected. No licence; 36 stars.

**CARROT** ([paper](https://arxiv.org/pdf/2502.03261)).
- Estimate `Φ(x)=E[Y|x]` (accuracy and cost, per model) with any regressor (RoBERTa or kNN), then `g_μ(x)=argmin_m Σ_k μ_k Φ(x)_{m,k}`. Changing the cost/quality mix `μ` needs no refit; it is minimax rate-optimal under smoothness assumptions.
- Introduces the SPROUT dataset (15 models, 6 benchmarks). On RouterBench it could not beat GPT-4 accuracy but cut cost a lot. No code link found in the paper text.

**UniRoute** ([paper](https://arxiv.org/pdf/2502.08773)).
- Represent each LLM as a vector of per-cluster errors on a small labelled validation set: k-means the training prompts into K clusters, then route to `argmin_m [ error(cluster(x), m) + λ·c(m) ]`. A learned soft cluster map is the second variant. kNN routing is a special case.
- Handles 30+ unseen LLMs with no retraining; the cost of adding a model is one pass of the validation prompts through it. Closest published match to "add a model with no training". No public repo link found.

**MixLLM** ([paper](https://arxiv.org/abs/2502.18482)). Tag-enhanced query embeddings (InsTag clustered into domains, BERT fine-tuned), per-LLM quality and cost predictors, a latency-penalised meta decision maker, continual training from feedback. 97.25% of GPT-4 quality at 24.18% of cost under a latency limit; online-vs-offline split table shows continual training recovers quality when the query mix drifts. No code link found.

**Arch-Router** ([paper](https://arxiv.org/abs/2506.16655), [model](https://huggingface.co/katanemo/Arch-Router-1.5B), [Plano/archgw](https://github.com/katanemo/plano)).
- A 1.5B generative model reads the query plus a list of user-written route policies `(name, description)` such as "code review" or "image editing", and emits the policy name. A separate table maps policy to model, so adding a model means editing config, not retraining.
- Trained on 43K synthetic conversations; overall routing accuracy 93.17% vs 92.79% for Claude Sonnet 3.7 on its own benchmark. It matches human-defined preference, not measured quality; there is no cost term. Plano is Apache-2.0, 7.1K stars, active (2026-10-24).

**Benchmarks.** [RouterEval](https://arxiv.org/abs/2503.10657) (200M performance records, 8,500+ LLMs, finds routing gains grow with pool size); [RouterArena](https://github.com/RouteWorks/RouterArena) (8,400 queries, 9 domains, accuracy-cost "arena score"; evaluation-only; the live leaderboard top ten sit within roughly 76.3 to 74.1 arena score, i.e. a narrow band); [LLMRouterBench](https://aclanthology.org/2026.findings-acl.1881.pdf) as above. Newer pretrained-foundation attempts exist: RouteFM learns to characterise anonymous models from behavioural context ([abstract](https://arxiv.org/abs/2609.37362)); I did not read beyond the abstract.

### 2.2 Cost- and budget-aware routing

| Method | Formulation | Result |
|---|---|---|
| [PILOT](https://arxiv.org/html/2508.21141v2) | Contextual bandit. Query projection `ψ(q)` and LLM embeddings `θ_a` pretrained on preference data; reward `cos(ψ(q), θ_a)`; pick `argmax_a cos + α√(ψᵀA_a⁻¹ψ)` (LinUCB with prior `A⁰=λI, b⁰=λθ_pref`). Budget: online multiple-choice knapsack (ZCL threshold `cos / ((UB·e/LB)^z · LB/e)`, z=budget used) applied per bin of queries with spill-over. | 93% of GPT-4 quality at 25% of its cost (multi-task, multi-LLM). No code link found. |
| [OmniRouter](https://arxiv.org/abs/2502.20576) | Batch-level constrained optimisation: minimise total cost s.t. performance threshold and per-model capacity, solved by Lagrangian dual decomposition with adaptive multipliers; hybrid retrieval-augmented predictor. | Up to 6.30% more accurate and at least 10.15% cheaper than baselines on its own test set. Needs the batch; no streaming horizon. |
| [ParetoBandit](https://arxiv.org/html/2604.00136v2) | LinUCB with cost penalty, primal-dual pacer with a hard price ceiling when the dual is positive, geometric forgetting for drift, hot-swap registry with forced exploration for newcomers. | Budget compliance within 0.4%; new model integrated in about 142 steps. Apache-2.0, `pip install paretobandit`, 18 stars, last push 2026-05-08. |
| [BaRP](https://arxiv.org/html/2510.07429) | Trains under partial feedback (only the chosen model's outcome); takes a preference vector at inference so the trade-off is a dial. | At least 12.46% over offline routers. |
| [Chance-constrained GLM-UCB](https://arxiv.org/html/2610.22695) | Primal-dual GLM-UCB with a high-probability satisfaction guarantee. | Theory-led. |
| [OrcaRouter](https://arxiv.org/html/2605.30736) | LinUCB over lexical+embedding features; offline full-information reward matrix fits one ridge regressor per arm, then optional online updates. | 75.54% accuracy at $1.00/1K queries; second on RouterArena at the time of its May 2026 submission. |

Notes for us:
- Per-query cost caps (RouteLLM, IRT, CARROT) are a Lagrange multiplier `λ` fixed by hand. OmniRouter and PILOT show how to make `λ` track a budget instead. Our budget is a rate-limited quota, not dollars; the pacer idea (raise `λ` as the window drains) maps onto it, but no paper models perishable quota. That mapping is my inference, not evidence.
- [T2MO](https://arxiv.org/html/2608.08528) gives a coding-specific refinement: price escalations. Expected cost is `C_m + (1−P_m)(C_M + W)`; a cheaper model is only worth it if `P_m > 1 − (C_M − C_m)/(C_M + W)`, where W is the cost of a failed attempt (retries, developer wait). Using token price alone over-routes to cheap models.

### 2.3 Routing the reasoning effort, not just the model

- **Semantic router for vLLM** ([paper](https://arxiv.org/abs/2510.08731), [repo](https://github.com/vllm-project/semantic-router)): a query classifier decides whether to enable thinking. On MMLU-Pro: +10.2 points accuracy, −47.1% latency, −48.5% tokens vs always-on direct inference as the paper words it. The repo (Apache-2.0, 6.3K stars, pushed 2026-10-27) now ships a dozen selection algorithms (router_dc by model description, multi_factor, latency_aware, automix cascade, elo, kNN/SVM/MLP, static rules) and an OpenCode "auto mode" guide where routing costs 1 to 18 ms on CPU ([guide](https://vllm-sr.ai/blog/opencode-auto-mode/)). In that guide's own logs the description-similarity scores are nearly flat (0.9939 to 1.0000), which suggests description matching alone discriminates weakly.
- **GPT-5 router**: a real-time router picks the fast or the reasoning model from conversation type, complexity, tool needs and explicit intent ("think hard"), trained on user switches, preference rates and measured correctness ([system card](https://openai.com/index/gpt-5-system-card/)). Not available as code.
- **Ares** ([paper](https://arxiv.org/html/2603.07915)): lightweight router predicts the lowest sufficient level (high/medium/low) per agent step from the interaction history; labels come from replaying each step at each level to find the minimum that succeeds. Up to 52.7% fewer reasoning tokens on TAU-Bench, BrowseComp-Plus, WebArena. Fixed low effort degrades; random does not save much. No code link seen.
- **DART** ([paper](https://arxiv.org/abs/2606.23181)): training-free. Sample two cheap no-think drafts; if they agree, accept; if not, set the thinking budget from draft entropy. Cuts thinking tokens 32 to 73%; code at `js-lee-AI/DART` (linked in the abstract).
- **Route-to-Reason** ([paper](https://arxiv.org/abs/2505.19435)): learns compressed embeddings of both models and reasoning strategies and picks the pair per query under a budget; beats the best single model while using over 60% fewer tokens (seven models, four strategies).
- **R2-Router** ([paper](https://arxiv.org/abs/2602.02823)): treats output-length budget as a control variable chosen jointly with the model, so a strong model with a short answer can beat a weak model at the same cost; reports 4 to 5× lower cost than prior routers. Code: `UCF-ML-Research/R2-Router`.
- **BEST-Route** ([paper](https://arxiv.org/pdf/2506.22716v1.pdf)): picks the model and the number of samples (best-of-n) by query difficulty; up to 60% cost cut with under 1% quality drop. Code: `microsoft/best-route-llm`.
- **Model-internal switches** (not routers across models): [ThinkSwitcher](https://arxiv.org/abs/2505.14183) (20 to 30% compute saved), [AdaCoT](https://arxiv.org/abs/2505.11896) (RL-trained CoT trigger, 69% fewer tokens on production traffic), [SelfBudgeter](https://arxiv.org/abs/2505.11274) (61% shorter on math). All need fine-tuning of the reasoning model, so none apply to API models we do not own.
- **Vendor side.** Claude's thinking is already adaptive per request, and effort (`low` … `max`) is its steering lever; level availability differs per model and the effort page is the authority, and on Haiku 5.5 a "answer directly" phrase does not reliably reduce thinking so a per-message effort change is needed ([adaptive thinking](https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking), [effort](https://platform.claude.com/docs/en/build-with-claude/effort)). Consequence: effort is a typed parameter per (model, level), not a free choice.

### 2.4 Cold start and adding a new model

Ranked by how little data they need:

1. **Zero data, metadata only**: RouteProfile ([paper](https://arxiv.org/html/2605.00180v2)), GraphRouter's LLM descriptions, vLLM-SR's model cards, Arch-Router's policy table, HyDRA's YAML capability vector built from published benchmark scores normalised across the pool ([HyDRA](https://arxiv.org/html/2605.17106)).
2. **A few hundred probe prompts**: UniRoute's per-cluster error vector; IRT-Router's nearest-neighbour warm-up. UniRoute needs one labelled validation set run once per new model.
3. **Online, tens to hundreds of calls**: ParetoBandit (about 142 steps); PILOT's preference prior plus bandit feedback; MixLLM's continual training.
4. **Task-type synthesis**: TRouter ([paper](https://aclanthology.org/2026.acl-long.1007/)) synthesises question-answer pairs from a task taxonomy to approximate an unseen query distribution; reported as alleviating cold start, details not read beyond the abstract.

How few labelled examples suffice: no paper gives a clean number. LLMRouterBench reports that the embedding backbone matters little and that bigger pools show diminishing returns versus careful curation ([LLMRouterBench](https://aclanthology.org/2026.findings-acl.1881.pdf)), so a small, curated pool of models is the safer bet than a wide one.

### 2.5 What predicts which model wins

- **Task type / domain.** Cluster or tag labels carry most of the signal: UniRoute clusters, MixLLM tags, IRT-Router relevance vectors, TRouter latent task types, T2MO's category × difficulty cells.
- **Difficulty.** Hybrid LLM's quality gap; IRT difficulty `b`; BEST-Route sampling count; HyDRA's requirement score. Scalar difficulty alone loses information: HyDRA splits it into reasoning, code generation, debugging, tool use.
- **Model capability vectors.** IRT `θ_m`, EmbedLLM and RouterDC embeddings, UniRoute error vectors, HyDRA profiles. All are fitted from graded outputs, except HyDRA/RouteProfile which start from published scores.
- **Execution evidence.** SWE-Router reads a partial trajectory (Bayes-optimality theorem: conditioning never hurts) and Agent-as-a-Router accumulates execution-grounded memory; both note the prompt alone cannot tell a typo from a multi-module refactor.
- **Code-health signals.** Triage uses pre-computed code-health metrics to assign light/standard/heavy tiers and states two falsifiable conditions for it to pay off; it is evaluated on SWE-bench Lite ([Triage](https://arxiv.org/html/2604.07494v1)).
- **HyDRA's coding numbers.** On SWE-Bench Verified it holds quality within 0.3 points of always-strong at 54.1% cost savings; labels came from 50,159 dual-model LLM-judge comparisons over Copilot telemetry, one ModernBERT-base forward pass per query; no public code seen ([HyDRA](https://arxiv.org/html/2605.17106)).

### 2.6 Comparison table

| Method | Inputs | Needs training data? | Handles budget? | Picks effort? | Code |
|---|---|---|---|---|---|
| RouteLLM | query embedding | Yes (arena prefs + golds) | Threshold α | No | Yes, Apache-2.0 |
| FrugalGPT | query + answers | Yes (per task) | Yes (cascade budget) | No | Not found |
| Hybrid LLM | query | Yes (both models' scores) | Quality tolerance | No | Yes (linked) |
| RouterDC | query + LLM embeddings | Yes | No | No | Yes, no licence |
| GraphRouter | task, query, LLM description | Yes; new LLMs without retraining | Cost weight | No | Yes, MIT |
| EmbedLLM | query, model embedding | Yes (correctness matrix) | No | No | Yes |
| IRT-Router | query embedding | Yes; warm-up for cold queries | α·perf − β·cost | No | Yes, no licence |
| CARROT | query | Yes (accuracy+cost labels) | μ dial, no refit | No | Not found |
| UniRoute | query, validation errors per model | Small val. set per new model | λ·cost | No | Not found |
| MixLLM | query, tags | Yes + continual | Yes, plus latency | No | Not found |
| Arch-Router | query + policy text | Pretrained; none per new model | No | No | Model + Plano |
| PILOT | query embedding | Prefs prior + online | Yes (knapsack) | No | Not found |
| OmniRouter | batch of queries | Yes | Yes (Lagrangian) | No | Not found |
| ParetoBandit | query features | Optional prior; online | Yes (pacer, hard ceiling) | No | Yes, Apache-2.0 |
| HyDRA | query → 4 requirement scores | Yes for predictor; none per model | τ + cheapest | No | Not found |
| Ares | interaction history | Yes (replay labels) | Indirect | **Yes (per step)** | Not found |
| DART | two no-think drafts | **No** | Via budget | **Yes (thinking budget)** | Yes |
| R2-Router | query | Yes (R2-Bench) | Yes | Output length | Yes |
| vLLM-SR | prompt, signals, model cards | Optional | multi_factor | Reasoning on/off | Yes, Apache-2.0 |

## 3. What this means for our design

1. **Use the HyDRA shape for the first version: requirement scores × capability vectors, matched by shortfall.** Evidence: a tiny query model emits K scores, models are YAML rows, `shortfall(m)=Σ w_k·max(0, r_k − c_{m,k})`, eligible set `shortfall ≤ τ`, pick the cheapest, fall back to least shortfall ([HyDRA](https://arxiv.org/html/2605.17106)). That is exactly what a "typed choice/score" 4B decider can answer in one forward pass (ask for 0 to 1 scores for reasoning, code, debugging, tool use). No training is needed to start if the decider is prompted rather than fine-tuned; whether a prompted 4B model produces calibrated scores is untested here and must be measured.
2. **Seed capability vectors from public benchmarks, then overwrite them with our own outcomes.** RouteProfile shows metadata profiles can beat best-single without any interaction data, while flat text does not ([RouteProfile](https://arxiv.org/html/2605.00180v2)); HyDRA does pool-relative normalisation so profiles stay comparable ([HyDRA](https://arxiv.org/html/2605.17106)). A new model enters as a YAML row.
3. **Treat subscription quota as a price that depends on time to reset, in the same `argmin loss + λ·cost` rule.** Evidence for the rule: UniRoute and CARROT. Evidence for the pacer: ParetoBandit's dual variable and PILOT's budget-utilisation threshold. The mapping from "perishable quota" to a shadow price is my design inference; no paper does it.
4. **Include effort as a second axis of the arm, with only the levels each model supports.** Arms are `(profile, model, effort)`; the Claude docs say level support varies per model ([effort](https://platform.claude.com/docs/en/build-with-claude/effort)). Ares shows a fixed low level loses accuracy and a random level saves little, so the level must come from the task/step, and R2-Router shows the cheap-but-strong arm (strong model, bounded output or effort) is often the best one ([Ares](https://arxiv.org/html/2603.07915), [R2-Router](https://arxiv.org/abs/2602.02823)).
5. **Price failure, not just tokens.** Use T2MO's boundary: `P_m > 1 − (C_M − C_m)/(C_M + W)`; for a subagent W includes the main agent's wasted turn ([T2MO](https://arxiv.org/html/2608.08528)). This stops the router drifting to the cheapest model when a miss costs a retry.
6. **Learn online from outcome signals with a bandit layer on top of the static prior.** PILOT's prior-informed LinUCB and ParetoBandit's forgetting handle both "few labels" and "model quality shifts silently". Outcome labels for subagents can come from tests passing, retries or escalation; learning only the chosen arm's reward is enough (partial feedback, [BaRP](https://arxiv.org/html/2510.07429)). Forced exploration for a new model is bounded by the pacer.
7. **Spread load with the same mechanism, not a separate one.** MixLLM's latency penalty and ParetoBandit's pacer are both per-arm penalties added to the score; a per-profile "window consumed vs window elapsed" penalty is the same pattern. Again my inference from the formulas.
8. **Do not over-invest in the predictor.** LLMRouterBench says routers cluster together and often fail to beat best-single ([LLMRouterBench](https://aclanthology.org/2026.findings-acl.1881.pdf)); gains came from recall failures. Keep the pool small and curated (a handful of models), log the chosen-vs-oracle gap from our own runs, and judge any predictor against "always use the best subscription model" first.
9. **Let the main agent override cheaply.** GPT-5's router honours explicit intent in the prompt ("think hard") ([system card](https://openai.com/index/gpt-5-system-card/)); Claude exposes a per-message effort change ([adaptive thinking](https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking)). An optional `effort`/`tier` hint on the delegating call costs the main agent one token and removes the hardest misroutes.
10. **Consider an escalate-after-peek step for expensive tasks.** SWE-Router and DART both decide after a cheap look (partial trajectory, two no-think drafts) rather than from the prompt alone ([SWE-Router](https://arxiv.org/abs/2607.00053), [DART](https://arxiv.org/abs/2606.23181)). Defer until the base router is measured; it adds latency.

What to build first: items 1 to 3 plus a log, nothing trained; add item 6 once there are a few hundred logged subagent outcomes.

## 4. Sources

Papers and pages I opened (all fetched 2026-10-28):

- https://arxiv.org/abs/2406.18665 — RouteLLM abstract
- https://arxiv.org/pdf/2406.18665 — RouteLLM full text (routers, tables 1 to 7)
- https://github.com/lm-sys/RouteLLM — RouteLLM README and repo metadata
- https://arxiv.org/abs/2305.05176 — FrugalGPT abstract
- https://arxiv.org/pdf/2305.05176 — FrugalGPT full text (cascade)
- https://arxiv.org/abs/2404.14618 — Hybrid LLM abstract
- https://arxiv.org/pdf/2404.14618 — Hybrid LLM full text (router score, code link)
- https://arxiv.org/abs/2403.12031 — RouterBench abstract
- https://github.com/withmartian/routerbench — RouterBench repo metadata
- https://arxiv.org/abs/2409.19886 — RouterDC abstract
- https://arxiv.org/pdf/2409.19886 — RouterDC full text (loss, encoder)
- https://github.com/shuhao02/RouterDC — repo metadata
- https://arxiv.org/abs/2410.03834 — GraphRouter abstract
- https://arxiv.org/pdf/2410.03834 — GraphRouter full text (LLM descriptions)
- https://github.com/ulab-uiuc/GraphRouter — repo metadata
- https://arxiv.org/abs/2410.02223 — EmbedLLM abstract
- https://arxiv.org/pdf/2410.02223 — EmbedLLM full text (encoder-decoder)
- https://arxiv.org/abs/2506.01048 — IRT-Router abstract
- https://arxiv.org/pdf/2506.01048 — IRT-Router full text (formulas, warm-up)
- https://github.com/Mercidaiha/IRT-Router — repo metadata
- https://arxiv.org/abs/2502.18482 — MixLLM abstract
- https://arxiv.org/pdf/2502.18482 — MixLLM full text
- https://arxiv.org/abs/2502.03261 — CARROT abstract
- https://arxiv.org/pdf/2502.03261 — CARROT full text (plug-in rule)
- https://arxiv.org/abs/2502.08773 — UniRoute abstract
- https://arxiv.org/pdf/2502.08773 — UniRoute full text (cluster routing rule)
- https://arxiv.org/abs/2506.16655 — Arch-Router abstract
- https://arxiv.org/pdf/2506.16655 — Arch-Router full text
- https://huggingface.co/katanemo/Arch-Router-1.5B — model link (from paper; not opened)
- https://github.com/katanemo/plano — Plano README and metadata
- https://arxiv.org/abs/2508.21141 — PILOT abstract
- https://arxiv.org/html/2508.21141v2 — PILOT full text (LinUCB, knapsack policy)
- https://arxiv.org/abs/2502.20576 — OmniRouter abstract
- https://arxiv.org/pdf/2502.20576 — OmniRouter full text
- https://arxiv.org/html/2604.00136v2 — ParetoBandit
- https://github.com/ParetoBandit/ParetoBandit — repo metadata
- https://arxiv.org/html/2510.07429 — BaRP (search highlight)
- https://arxiv.org/html/2610.22695 — chance-constrained GLM-UCB (search highlight)
- https://arxiv.org/html/2605.30736 — OrcaRouter (search highlight)
- https://arxiv.org/html/2603.30035 — NeuralUCB routing (search highlight; not used above)
- https://arxiv.org/abs/2503.10657 — RouterEval abstract
- https://github.com/RouteWorks/RouterArena — RouterArena README and leaderboard
- https://aclanthology.org/2026.findings-acl.1881.pdf — LLMRouterBench
- https://arxiv.org/html/2605.00180v2 — RouteProfile
- https://aclanthology.org/2026.acl-long.1007/ — TRouter / task-aware cold start
- https://arxiv.org/abs/2609.37362 — RouteFM abstract
- https://arxiv.org/html/2605.17106 — HyDRA
- https://arxiv.org/html/2606.22902v2 — Agent-as-a-Router (search highlight)
- https://arxiv.org/abs/2607.00053 — SWE-Router
- https://arxiv.org/html/2604.07494v1 — Triage (search highlight)
- https://arxiv.org/html/2608.08528 — T2MO
- https://arxiv.org/html/2609.34712v1 — RSI-Router (search highlight; not used above)
- https://arxiv.org/abs/2510.08731 — semantic router for vLLM
- https://github.com/vllm-project/semantic-router — repo README and metadata
- https://vllm-sr.ai/blog/opencode-auto-mode/ — OpenCode auto mode and algorithm list
- https://arxiv.org/abs/2505.19435 — Route-to-Reason
- https://arxiv.org/html/2603.07915 — Ares (search highlight)
- https://arxiv.org/abs/2606.23181 — DART
- https://arxiv.org/abs/2602.02823 — R2-Router
- https://arxiv.org/pdf/2506.22716v1.pdf — BEST-Route
- https://arxiv.org/abs/2505.14183 — ThinkSwitcher
- https://arxiv.org/abs/2505.11896 — AdaCoT
- https://arxiv.org/abs/2505.11274 — SelfBudgeter
- https://openai.com/index/gpt-5-system-card/ — GPT-5 router description
- https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking — Claude adaptive thinking
- https://platform.claude.com/docs/en/build-with-claude/effort — Claude effort levels per model

Not verified: code availability for FrugalGPT, CARROT, UniRoute, MixLLM, PILOT, OmniRouter, HyDRA, Ares (no link found in the text I read; they may exist). Two 2026 surveys ([ACL Findings](https://aclanthology.org/2026.findings-emnlp.2072/), [arXiv 2603.04445](https://arxiv.org/html/2603.04445v2)) were fetched but only skimmed, so nothing above relies on them. Exa search returned HTTP 429 for several queries, so there may be 2026 papers I did not see; the arXiv listing API timed out.
