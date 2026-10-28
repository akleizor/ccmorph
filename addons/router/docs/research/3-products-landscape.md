# Topic 3: Products and open-source projects that route between models, accounts and keys

Researched 2026-10-28. Star counts and last-push dates come from the GitHub API on that day. "Not verified" marks anything I could not open or confirm.

## 1. Key findings

- **The closest existing product is `clauth`** (MIT, 265 stars, pushed 2026-10-27): it tracks live 5h/7d usage per Claude and Codex account, auto-switches down a fallback chain, offers a burn-rate-projected mode, and exposes an MCP `delegate` tool that hands a headless prompt to another account. The main agent still has to choose the target, though. [clauth README](https://github.com/uwuclxdy/clauth), [Auto-Switch wiki](https://github.com/uwuclxdy/clauth/wiki/Auto-Switch)
- **Commercial routers never see subscription quota.** They route prompt to model on cost, quality, latency or health (Not Diamond, OpenRouter Auto, Azure, Bedrock, Copilot, Cursor, Kilo), and none documents a way to read a subscription window. [OpenRouter Auto Router](https://openrouter.ai/docs/guides/routing/routers/auto-router), [Azure model router](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/model-router)
- **Every shipped router uses a "quality band, then cheapest" shape.** Azure Balanced picks the cheapest model within 1–2% of the best quality for the prompt; Cost mode widens the band to 5–6%. Copilot and Cursor expose the same three-way dial (cost, balance, intelligence). This is the pattern to copy. [Azure](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/model-router), [Copilot Auto](https://docs.github.com/en/copilot/concepts/auto-model-selection), [Cursor Router](https://cursor.com/docs/cursor-router)
- **claude-code-router already routes subagents, but by asking the main model.** It injects model descriptions into the `Agent`/`Task` tool and the main model writes a `<CCR-SUBAGENT-MODEL>` tag. That is the effort we want to remove. [CCR routing docs](https://github.com/musistudio/claude-code-router/blob/main/docs/src/content/docs/en/configuration/routing.md)
- **Only one Anthropic quota read is documented**: the statusline JSON `rate_limits.five_hour` / `seven_day` (`used_percentage`, `resets_at`). The `/api/oauth/usage` endpoint and the `anthropic-ratelimit-unified-*` headers are reverse-engineered. [Statusline docs](https://code.claude.com/docs/en/statusline), [CodexBar Claude notes](https://github.com/steipete/CodexBar/blob/main/docs/claude.md)
- **Anthropic's terms forbid routing subscription OAuth credentials through third-party products**, and enforcement is real: a CLIProxyAPI user's OAuth-proxied traffic was billed as "extra usage" while the same account in Claude Desktop was not. Launching the unmodified official CLI per profile is the defensible design. [Claude Code legal page](https://code.claude.com/docs/en/legal-and-compliance), [CLIProxyAPI #2599](https://github.com/router-for-me/CLIProxyAPI/issues/2599)
- **OpenAI is more permissive on credential files**: its docs tell you to copy `~/.codex/auth.json` to a headless machine, but the Terms still ban sharing credentials with anyone else. [Codex auth](https://developers.openai.com/codex/auth), [OpenAI Terms](https://openai.com/policies/row-terms-of-use/)
- **Account pools balance by round-robin, priority or session. None balances by task.** [better-ccflare README](https://github.com/tombii/better-ccflare), [CLIProxyAPI README](https://github.com/router-for-me/CLIProxyAPI)

## 2. Details

### 2.1 Commercial and platform model routers

| Product | Routes on | Config and quota awareness | Verdict |
|---|---|---|---|
| **Not Diamond** (hosted) | Learned router: per-prompt predicted quality, with `cost_quality_tradeoff` 0 (quality) to 10 (cheapest); optional custom router trained on your eval data ([key concepts](https://docs.notdiamond.ai/docs/key-concepts)) | You list candidate models. No quota concept. The Python SDK repo is small (91 stars, last push 2025-12-11, no licence reported) | Best prompt-to-model quality dial, blind to accounts |
| **OpenRouter Auto** (`openrouter/auto`) | ~30-class task classifier, then models ranked by trailing-7-day community spend share per class, then a `cost_tier` band (low…max); session stickiness via `session_id`; `allowed_models` / `excluded_models` wildcards ([docs](https://openrouter.ai/docs/guides/routing/routers/auto-router)) | Credits only. The earlier Not Diamond tie-in is not mentioned on the current page | Elegant "market as ranking" idea; useful as a free prior over models per task class |
| **Martian** | Quality-predicting router, Accenture-backed (2024 news only) | Not verified: I saw only 2023–24 announcements, no current docs ([Accenture release](https://newsroom.accenture.com/news/2024/accenture-invests-in-martian-to-bring-dynamic-routing-of-large-language-queries-and-more-effective-ai-systems-to-clients)) | Enterprise play; no evidence of quota logic |
| **Requesty** | Gateway with fallback, load-balancing and latency-routing policies (page titles only; bodies not extracted, [docs](https://docs.requesty.ai/features/smart-routing)) | Not verified | Gateway, not a task router |
| **Arcee Conductor** | Small-model router, GA with free credits ([article](https://www.airealist.ai/p/arcee-conductor-is-now-generally)) | Not verified: vendor docs 404 on the page I tried | Not verified |
| **Unify**, **Vertex model optimizer** | Not verified: Unify not opened; the Vertex page returned only navigation ([page](https://cloud.google.com/vertex-ai/generative-ai/docs/model-optimizer/introduction)) | – | – |
| **Azure AI Foundry Model Router** | Trained LM reads complexity, reasoning need and task type; modes Quality / Balanced (default) / Cost; optional model subset; version `2025-11-18` routes OpenAI, DeepSeek, Meta, xAI and Anthropic models | Subset selection is the only user control; context window limited to the smallest member | The cleanest published "quality band" spec |
| **AWS Bedrock Intelligent Prompt Routing** | Predicts per-model response quality inside one model family; you pick a fallback (anchor) model and a "response quality difference" threshold ([docs](https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-routing.html)) | English prompts only; cannot use your performance data | Anchor-plus-threshold is a good config shape; narrow model support |
| **OpenAI GPT-5 router** | Real-time router on conversation type, complexity, tool needs and explicit intent ("think hard"); trained on switch events, preference rates and measured correctness ([GPT-5 post](https://openai.com/index/introducing-gpt-5/)) | Internal to ChatGPT | Proves explicit user intent can be a routing input |
| **GitHub Copilot Auto** | Two systems: real-time health/availability and task complexity; tiers Efficiency / Balance / Intelligence; routes only at natural cache boundaries; 10% discount on paid plans ([docs](https://docs.github.com/en/copilot/concepts/auto-model-selection)) | Health-aware, so it reduces rate limiting; no user quota inputs | Closest in spirit; closed |
| **Cursor Router** (`auto-smart`, `optimize_for` cost / balanced / intelligence) | Classifier on task type and complexity, picks the cheapest model of comparable quality; hides the chosen model by default so results are judged on merit ([docs](https://cursor.com/docs/cursor-router)) | Billing-aware: Cost mode draws on the weekly pool, other modes bill at list price | Shows tiering by plan pool vs paid credit is productizable |
| **Kilo Code Auto** (`kilo-auto/frontier`, `efficient`, `free`) | Efficient: session-aware difficulty classification, cheapest model "proven accurate enough" per continuously-run benchmarks, sticky across related turns ([docs](https://kilo.ai/docs/code-with-ai/agents/auto-model)) | None | Good model for cheap-first with sticky sessions |
| **Windsurf, Roo, Cline** | Not verified | – | – |

### 2.2 Gateways with load-balancing strategies

- **LiteLLM Router** (62,162 stars, pushed 2026-10-27, licence reported as NOASSERTION). Strategies: `simple-shuffle` (default, weight or rpm/tpm based), `latency-based-routing`, `usage-based-routing-v2`, `least-busy`, `cost-based-routing`. Extras: `order` tiers (try order 1, then 2…), weighted failover, cooldowns (`allowed_fails`, `cooldown_time`), per-group strategy overrides, and session affinity that pins a conversation to the first deployment unless it is in cooldown. State lives in Redis for tpm/rpm. It reads limits you configure; it does not read subscription windows. [Routing docs](https://docs.litellm.ai/docs/routing), [repo](https://github.com/BerriAI/litellm). Verdict: the reference vocabulary for API-key pools; reuse the `order` plus cooldown plus affinity ideas.
- **Portkey Gateway** (13,280 stars, MIT, last push 2026-05-25, so quiet): configs with fallback, load-balance weights and conditional routing on request metadata. Page body not extracted, so details are not verified. [Docs](https://portkey.ai/docs/product/ai-gateway/conditional-routing), [repo](https://github.com/Portkey-AI/gateway).
- **Helicone** (6,241 stars, Apache-2.0, pushed 2026-10-24): described as an observability platform; its gateway routing is not verified. [Repo](https://github.com/Helicone/helicone).
- **Kong AI Gateway** (44,309 stars, Apache-2.0, pushed 2026-10-19): `ai-proxy-advanced` offers a load balancer with retries, timeouts and cross-provider fallback (v3.10+), and a lowest-latency mode using EWMA, where slower targets still get 0.1–5% of traffic so their score keeps updating. Semantic routing needs Redis with vector search. [Docs](https://developer.konghq.com/plugins/ai-proxy-advanced/). The EWMA-with-exploration detail is worth copying for model latency.
- **Envoy AI Gateway** has been renamed **Agent Router** and moved to the Agentic AI Foundation; the repo `theagentrouter/agent-router` has 2,304 stars, Apache-2.0, last push 2026-10-26. Infra-level routing by model header; no task understanding verified. [Docs](https://aigateway.envoyproxy.io/docs/capabilities/traffic/model-routing).
- **vLLM Semantic Router** (6,277 stars, Apache-2.0, pushed 2026-10-27): "decision layer" in Go with small open "Decision" and "Vela" routing models, `vllm-sr/auto` model id, and a July 2026 post on adding Cursor-style Auto to OpenCode. It targets harnesses directly and selects or combines models per call. The nearest OSS analogue to a small-decider architecture. [README](https://github.com/vllm-project/semantic-router).
- **RouteLLM** (5,641 stars, Apache-2.0, last push 2024-08-10): research framework (strong vs weak model router). Dormant. [Repo](https://github.com/lm-sys/RouteLLM).

### 2.3 Coding-agent routers and multi-account tools

| Project | What it does | Quota source and routing |
|---|---|---|
| **claude-code-router** (musistudio) 37,879★ MIT, pushed 2026-10-10, v3.1.3 | Local gateway on :3456 for Claude Code, Codex, Kimi CLI, OpenCode and others. Now a desktop control plane: credential pools, ordered fallback models, header/body conditions, Node.js script rules (fail-open), subagent auto-routing by prompt tag | v2.0.0 had fixed scenarios `default`, `background`, `think`, `longContext` (threshold 60,000 tokens) and `webSearch` ([v2.0.0 README](https://github.com/musistudio/claude-code-router/blob/v2.0.0/README.md)); v3 routing is rule-based. No subscription quota reading found |
| **CCProxy** (starbased-co) 355★, NOASSERTION, pushed 2026-08-10 | Hook pipeline (inbound DAG, transform, outbound) to rewrite or reroute Claude Code requests; "intelligent model routing using your logic or ours" ([README](https://github.com/starbased-co/ccproxy)) | You write the hooks; no built-in quota awareness found |
| **ccflare** (snipeship) 1,052★ MIT, last push 2026-04-19 (dormant); **better-ccflare** (tombii) 274★ MIT, pushed 2026-10-23 | Proxy over several Claude accounts. Strategy `session` (default), sessions aligned to the 5h window, priority auto-fallback to a preferred account when its window resets, auto-restart of windows, `combos` (ordered account+model slots per model family), 503 when the pool is exhausted ([README](https://github.com/tombii/better-ccflare)) | Reacts to rate-limit responses and tracks reason and time per account. Excludes OAuth accounts from Codex traffic because "Anthropic bans these when used outside Claude CLI" |
| **claude-relay-service** (Wei-Shaw) 12,744★ MIT, pushed 2026-10-26; **sub2api** (Wei-Shaw) 44,947★ LGPL-3.0, pushed 2026-10-27 | Self-hosted relay turning subscriptions into API keys for "carpool" sharing; sub2api adds sticky sessions, per-user and per-account concurrency, and 5h/daily/7d spend windows per key | Both READMEs warn the project may violate Anthropic's terms ([CRS](https://github.com/Wei-Shaw/claude-relay-service), [sub2api](https://github.com/Wei-Shaw/sub2api)) |
| **CLIProxyAPI** (router-for-me) 56,621★ MIT, pushed 2026-10-27 | Wraps Claude Code, Codex, Gemini, Grok and others as OpenAI/Claude/Gemini-compatible APIs with round-robin across accounts | Ecosystem of quota inspectors for Codex 5h/7d per account ([README](https://github.com/router-for-me/CLIProxyAPI)); recurring ban and 429 issues (section 2.5) |
| **AIClient2API** (justlovemaki) 8,929★ GPL-3.0, pushed 2026-10-26 | Multi-protocol proxy with "provider pools, smart routing, and automatic failover" (GitHub description only; README not read) | Not verified |
| **cc-switch** (farion1231) 149,344★ MIT, pushed 2026-10-27 | Desktop app that switches provider config for Claude Code, Codex, OpenCode and others ([repo](https://github.com/farion1231/cc-switch)) | Config switcher; routing not verified (README not read in depth) |
| **CodexBar** (steipete) 22,847★ MIT, pushed 2026-10-26 | Menu-bar quota readout for 40+ providers (Claude, Codex, Copilot, Gemini, z.ai, Kimi, OpenRouter, sub2api…) with documented data-source order ([README](https://github.com/steipete/CodexBar)) | Best public catalogue of how to read each quota; read-only, no routing |
| **ccusage** (ccusage/ccusage) 19,198★, NOASSERTION, pushed 2026-10-27 | Token and cost reports from local Claude Code logs ([repo](https://github.com/ccusage/ccusage)) | Local-log based, not live quota |
| **clauth** (uwuclxdy) 265★ MIT, pushed 2026-10-27 | Multi-account manager and usage monitor, Rust, with `clauth daemon` and an MCP plugin (`profiles`, `switch_profile`, `delegate`, `monitor`). Isolated parallel sessions per `CLAUDE_CONFIG_DIR`. Also manages Codex profiles with a separate chain | Fallback chain: exhausted at 5h ≥ 95%, 7d ≥ 98% (configurable), per-model weekly windows gate too, plus `burn-aware` mode that projects utilisation forward. Prefers live over cached readings. `start --auto` picks the account from the models a session will run |
| **claude-code-multi-account** (valeriodiaco) 7★, pushed 2026-03-03 | Watchdog polls `GET /api/oauth/usage` every 60 s per account from the Keychain; `claude-pool select/launch/wait` picks the least-loaded "automation" account and reserves an "interactive" one ([README](https://github.com/valeriodiaco/claude-code-multi-account)) | Least-loaded by 5h %; weekly pacing mentioned in the README |

Nobody in this table decides **which subagent task goes to which account and model**. The closest are `clauth delegate` (main model chooses) and CCR subagent tags (main model chooses).

### 2.4 How each tool reads quota

| Source | Status | Notes |
|---|---|---|
| Claude Code statusline JSON `rate_limits.five_hour/seven_day.{used_percentage,resets_at}` | **Documented** | Pro/Max only; appears after the session's first API response; either window may be absent ([docs](https://code.claude.com/docs/en/statusline)). Only visible inside a running session |
| `GET https://api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20` | **Reverse-engineered** | Needs the `user:profile` scope; tokens from `claude setup-token` lack it ([CodexBar](https://github.com/steipete/CodexBar/blob/main/docs/claude.md)). I found no Anthropic page documenting it. clauth caches readings because the endpoint is itself rate-limited |
| Response headers `anthropic-ratelimit-unified-5h-utilization`, `-5h-reset`, `-7d-utilization`, `-7d-reset` | **Undocumented** | Anthropic's rate-limit page lists only the `anthropic-ratelimit-requests/tokens-*` families; the word "unified" does not appear ([platform docs](https://platform.claude.com/docs/en/api/rate-limits)). Parsed by [cpa-usage-keeper](https://github.com/Willxup/cpa-usage-keeper/blob/main/internal/quota/claude_header.go); a probe with `max_tokens: 1` suffices ([claude-usage-stick](https://github.com/oauramos/claude-usage-stick/blob/main/wiki/How-It-Works.md)). Pro/Max only; API-billed accounts do not emit them |
| Codex: `GET chatgpt.com/backend-api/wham/usage` (`primary_window`, `secondary_window`) | **Reverse-engineered** | [CodexBar Codex notes](https://github.com/steipete/CodexBar/blob/main/docs/codex.md) |
| Codex app-server: `account/rateLimits/read` and the `account/rateLimits/updated` notification | **In the open-source protocol schema**; no prose guide found | Snapshot has `primary`, `secondary`, `credits`, `planType`, `rateLimitReachedType` ([schema](https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitSnapshot.ts)). Best available Codex source |
| Codex `x-codex-primary-used-percent` headers | Legacy | Headroom notes the backend may no longer emit them ([source](https://github.com/headroomlabs-ai/headroom/blob/main/headroom/subscription/codex_rate_limits.py)) |
| Provider balance and window endpoints (OpenRouter credits, z.ai, Kimi, MiniMax, sub2api key quota…) | Mostly vendor APIs or cookies | CodexBar lists each ([README](https://github.com/steipete/CodexBar)) |

### 2.5 Terms of service, in the vendors' words

**Anthropic**, Claude Code legal page (fetched today):

> "OAuth authentication is intended exclusively for purchasers of Claude Free, Pro, Max, Team, and Enterprise subscription plans and is designed to support ordinary use of Claude Code and other native Anthropic applications."

> "Anthropic does not permit third-party developers to offer Claude.ai login into their own applications, or to route requests through Free, Pro, or Max plan credentials on behalf of their users. Moreover, developers may not collect, store, or intermediate Claude.ai credentials or session tokens."

Same page: "Advertised usage limits for Pro and Max plans assume ordinary, individual usage of Claude Code and the Agent SDK", and enforcement "may [happen] without prior notice". [Source](https://code.claude.com/docs/en/legal-and-compliance).

Consumer Terms: "You may not share your Account login information, Anthropic API key, or Account credentials with anyone else. You also may not make your Account available to anyone else." Also prohibited: "to access the Services through automated or non-human means, whether through a bot, script, or otherwise" except via an API key or where explicitly permitted. [Source](https://www.anthropic.com/legal/consumer-terms).

In February 2026 the legal page was stricter: "Using OAuth tokens obtained through Claude Free, Pro, or Max accounts in any other product, tool, or service — including the Agent SDK — is not permitted", and an Anthropic engineer said in January that third-party harnesses "are prohibited by our Terms of Service" ([The Register](https://www.theregister.com/software/2026/02/20/anthropic-clarifies-ban-on-third-party-tool-access-to-claude/5014546)). The current wording is softer, but the direction is unchanged.

Enforcement evidence: a Max user reported that requests through CLIProxyAPI's OAuth proxy were billed as extra usage ("You're out of extra usage") while Claude Desktop on the same account still showed 37% of the 5h window ([#2599](https://github.com/router-for-me/CLIProxyAPI/issues/2599)). The proxy's response was to make payloads look more like native Claude Code ([PR #2851](https://github.com/router-for-me/CLIProxyAPI/pull/2851)), which is an arms race a public product should avoid.

**OpenAI**, Terms of Use: "You may not share your account credentials or make your account available to anyone else." [Source](https://openai.com/policies/row-terms-of-use/). Codex docs: `auth.json` "contains access tokens"; "Treat `~/.codex/auth.json` like a password", while explicitly offering copying it to a headless machine as a login fallback. [Source](https://developers.openai.com/codex/auth).

**Not found**: any explicit statement either way on one person holding several subscriptions and rotating among them with the official client. Treat it as unclear, not allowed.

### 2.6 Table: ten most relevant projects

| # | Project | Why it matters |
|---|---|---|
| 1 | **clauth** · 265★ · MIT · 2026-10-27 | Closest product: quota-aware chain, burn-aware, per-model gates, MCP delegate. Main agent still chooses |
| 2 | **claude-code-router** · 37,879★ · MIT · 2026-10-10 | Subagent routing by description tags; rules, scripts, fallbacks. Needs the main model's judgement |
| 3 | **CodexBar** · 22,847★ · MIT · 2026-10-26 | Catalogue of quota-reading methods for 40+ providers |
| 4 | **better-ccflare** · 274★ · MIT · 2026-10-23 | Window-aligned sessions, combos (account+model slots). Proxies OAuth, which is a terms risk |
| 5 | **LiteLLM Router** · 62,162★ · NOASSERTION · 2026-10-27 | Reference strategies: order, weights, cooldowns, session affinity |
| 6 | **vLLM Semantic Router** · 6,277★ · Apache-2.0 · 2026-10-27 | Small open routing models plus a harness-facing endpoint |
| 7 | **CLIProxyAPI** · 56,621★ · MIT · 2026-10-27 | Largest multi-account pool; shows both demand and the enforcement risk |
| 8 | **claude-code-multi-account** · 7★ · 2026-03-03 | Tiny but exactly the pattern: poll usage, `select`/`wait`/`launch` per account |
| 9 | **OpenRouter Auto / Azure Model Router** · hosted | Task-class classifier and the quality-band-then-cheapest rule |
| 10 | **Cursor Router / Copilot Auto / Kilo Auto** · hosted | Cost/balance/intelligence tiers and cache-aware switching |

### 2.7 The gap

No product schedules work across subscription windows, API keys and gateway budgets using both what the task needs and which quota will otherwise expire unused. Account pools (clauth, ccflare, CLIProxyAPI, the pool script) know windows but pick by round-robin, priority or threshold. Model routers know tasks but see neither subscription quota nor reset times. The one subagent router (CCR) makes the main model pick from a description list. Nobody sets effort level per task or picks the account without the main agent's attention.

### 2.8 What users ask for most (evidence from issues; Reddit and X not searched successfully this session)

1. **Auto model and effort by task complexity**: [claude-code #43326](https://github.com/anthropics/claude-code/issues/43326) (open, proposes a classifier and a hook), [#15721](https://github.com/anthropics/claude-code/issues/15721) (automatic model switching for plan mode, 35 comments).
2. **Multiple accounts without re-login**: [#18435](https://github.com/anthropics/claude-code/issues/18435), [#36151](https://github.com/anthropics/claude-code/issues/36151) (both open).
3. **Limit visibility and relief**: [#38335](https://github.com/anthropics/claude-code/issues/38335) (Max limits exhausted abnormally fast, 879 comments), [#76272](https://github.com/anthropics/claude-code/issues/76272) (control weekly reset timing).
4. **Failover that works in pools**: CLIProxyAPI [#903](https://github.com/router-for-me/CLIProxyAPI/issues/903) (59 comments on "resource exhausted"), [#1015](https://github.com/router-for-me/CLIProxyAPI/issues/1015) (429 despite available quota), [#1637](https://github.com/router-for-me/CLIProxyAPI/issues/1637) (three Google accounts blocked at once).
5. **Per-account model mapping, more providers**: better-ccflare [#132](https://github.com/tombii/better-ccflare/issues/132) and [#47](https://github.com/tombii/better-ccflare/issues/47); CCR [#119](https://github.com/musistudio/claude-code-router/issues/119), [#407](https://github.com/musistudio/claude-code-router/issues/407).

## 3. What this means for our design

1. **Run above the agent, not in the token path.** Launch the unmodified `claude` / `codex` binary per profile, each with its own `CLAUDE_CONFIG_DIR` / `CODEX_HOME` (what clauth and the pool script do), and use `ANTHROPIC_BASE_URL` only for API-key and gateway profiles. Do not proxy subscription OAuth tokens: Anthropic's page forbids intermediating credentials, and #2599 shows the billing consequence. Sources: [legal page](https://code.claude.com/docs/en/legal-and-compliance), [#2599](https://github.com/router-for-me/CLIProxyAPI/issues/2599).
2. **Two stages: hard filter, then score.** Filter out profiles that are dead, capped (5h ≥ 95%, 7d ≥ 98%, or a per-model weekly window past its line, as clauth does), lack the model, or lack the requested effort level. Then rank what is left. clauth's per-model gating exists because a capped model strands a session. [Auto-Switch](https://github.com/uwuclxdy/clauth/wiki/Auto-Switch).
3. **Task choice: quality band, then cheapest.** Use the decider to produce a task class and a difficulty score (OpenRouter uses ~30 classes). Take the best model for that class from an offline (model, class) quality table, keep every model within a band of the best (Azure uses 1–2% for Balanced, 5–6% for Cost), and pick the cheapest in the band. Expose three user modes (cost, balanced, intelligence) like Copilot and Cursor. Sources: [Azure](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/model-router), [OpenRouter](https://openrouter.ai/docs/guides/routing/routers/auto-router).
4. **Perishability is our own term.** No cited product has it. Proposed score for a subscription profile: remaining weekly fraction divided by hours until reset, capped by a 5h headroom guard, with a burn-rate projection like clauth's `burn-aware`. Spread load by always choosing the highest pressure. Marked as a design proposal; no source reports this formula. [clauth burn-aware](https://github.com/uwuclxdy/clauth/wiki/Auto-Switch), [better-ccflare window alignment](https://github.com/tombii/better-ccflare).
5. **Quota reads: documented first, undocumented behind an adapter.** Prefer the statusline `rate_limits` JSON when a session is live, the Codex app-server `account/rateLimits/read` for Codex, and treat `/api/oauth/usage` and the unified headers as optional adapters that can break. Cache readings, poll with backoff (the usage endpoint is rate-limited), prefer fresh over cached, and fall back to cooldown-on-429 as LiteLLM does. Reuse CodexBar's per-provider notes rather than rediscovering endpoints. [CodexBar docs](https://github.com/steipete/CodexBar).
6. **Remove the main model from the loop.** CCR injects a model list into the `Agent` tool description and relies on the main model to write a tag; clauth's `delegate` requires the model to call `profiles` first. We should expose one call with the task text and nothing else (a hook or an MCP tool such as `route(task)`), returning the triple. This answers #43326, which asked for exactly that hook. [CCR routing](https://github.com/musistudio/claude-code-router/blob/main/docs/src/content/docs/en/configuration/routing.md), [#43326](https://github.com/anthropics/claude-code/issues/43326).
7. **Subagents need a different process for a different subscription.** An account is bound to the process's config dir, so routing a subagent to another subscription means launching a headless run under that profile (clauth `delegate`, `claude-pool launch`). Same-account model and effort changes can use the native subagent `model` field. This follows from the per-profile launch pattern in the pool and clauth READMEs.
8. **Switching cost is low for subagents.** Copilot routes only at cache boundaries and Kilo and OpenRouter keep a model sticky across turns because switching loses prompt cache. A fresh subagent context has no cache to lose, so per-task routing is cheap; keep stickiness only for resumed subagents (LiteLLM-style session affinity). Sources: [Copilot](https://docs.github.com/en/copilot/concepts/auto-model-selection), [LiteLLM](https://docs.litellm.ai/docs/routing).
9. **Log the decision.** CCR and OpenRouter both expose resolved route and task type in logs or metadata; do the same so the user can audit why a profile was chosen. [OpenRouter metadata](https://openrouter.ai/docs/guides/routing/routers/auto-router).
10. **Position and licence.** Publish as MIT, state the compliance stance in the README (official client, user's own accounts, no credential relay), and consider interoperating with clauth's profile store instead of building a competing one. Do not copy code without checking it: CLIProxyAPI and CCR are MIT; sub2api is LGPL-3.0, AIClient2API is GPL-3.0, so read-only study only.

## 4. Sources

Every URL below was opened this session. Items marked "(partial)" returned only navigation or a thin body.

- https://code.claude.com/docs/en/legal-and-compliance: Anthropic authentication and credential-use rules
- https://www.anthropic.com/legal/consumer-terms: Consumer Terms (credential sharing, automated access)
- https://www.theregister.com/software/2026/02/20/anthropic-clarifies-ban-on-third-party-tool-access-to-claude/5014546: Feb 2026 report on the earlier, stricter wording
- https://openai.com/policies/row-terms-of-use/: OpenAI Terms of Use
- https://developers.openai.com/codex/auth: Codex auth and `auth.json` handling
- https://code.claude.com/docs/en/statusline: documented `rate_limits` fields
- https://platform.claude.com/docs/en/api/rate-limits: documented API rate-limit headers (no "unified")
- https://github.com/steipete/CodexBar and its docs/claude.md, docs/codex.md: quota-reading catalogue
- https://github.com/Willxup/cpa-usage-keeper/blob/main/internal/quota/claude_header.go: unified header parser
- https://github.com/oauramos/claude-usage-stick/blob/main/wiki/How-It-Works.md: header probe design
- https://github.com/headroomlabs-ai/headroom/blob/main/headroom/subscription/codex_rate_limits.py: Codex wham/usage and header history
- https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitSnapshot.ts and AccountRateLimitsUpdatedNotification.ts: Codex rate-limit schema
- https://github.com/uwuclxdy/clauth and https://github.com/uwuclxdy/clauth/wiki/Auto-Switch: clauth README and decision rules
- https://github.com/valeriodiaco/claude-code-multi-account: watchdog and pool CLI
- https://github.com/musistudio/claude-code-router (README, docs/.../routing.md, v2.0.0 README): CCR
- https://github.com/starbased-co/ccproxy: CCProxy
- https://github.com/snipeship/ccflare and https://github.com/tombii/better-ccflare: ccflare family
- https://github.com/Wei-Shaw/claude-relay-service and https://github.com/Wei-Shaw/sub2api: relay services
- https://github.com/router-for-me/CLIProxyAPI, issues #903, #1015, #1637, #2599, PR #2851: CLIProxyAPI
- https://github.com/justlovemaki/AIClient2API, https://github.com/farion1231/cc-switch, https://github.com/ccusage/ccusage: metadata only (partial)
- https://github.com/anthropics/claude-code/issues/43326, /15721, /18435, /36151, /38335, /76272: Claude Code requests
- https://github.com/musistudio/claude-code-router/issues/119, /407; https://github.com/tombii/better-ccflare/issues/47, /132: feature requests
- https://docs.litellm.ai/docs/routing: LiteLLM routing strategies
- https://developer.konghq.com/plugins/ai-proxy-advanced/: Kong balancer and EWMA
- https://portkey.ai/docs/product/ai-gateway/conditional-routing (partial); https://github.com/Portkey-AI/gateway; https://github.com/Helicone/helicone
- https://aigateway.envoyproxy.io/docs/capabilities/traffic/model-routing and https://github.com/theagentrouter/agent-router: Envoy rename
- https://github.com/vllm-project/semantic-router: vLLM Semantic Router
- https://github.com/lm-sys/RouteLLM and https://github.com/Not-Diamond/notdiamond-python: repo metadata
- https://docs.notdiamond.ai/docs/what-is-not-diamond and /key-concepts: Not Diamond (key concepts page)
- https://openrouter.ai/docs/guides/routing/routers/auto-router: OpenRouter Auto Router
- https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/model-router: Azure model router
- https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-routing.html: Bedrock prompt routing
- https://cloud.google.com/vertex-ai/generative-ai/docs/model-optimizer/introduction (partial)
- https://docs.requesty.ai/features/smart-routing (partial)
- https://docs.github.com/en/copilot/concepts/auto-model-selection: Copilot Auto
- https://cursor.com/docs/cursor-router and https://cursor.com/docs/models-and-pricing: Cursor Router and pricing
- https://kilo.ai/docs/code-with-ai/agents/auto-model: Kilo Auto Model
- https://openai.com/index/introducing-gpt-5/: GPT-5 router description
- https://newsroom.accenture.com/news/2024/accenture-invests-in-martian-to-bring-dynamic-routing-of-large-language-queries-and-more-effective-ai-systems-to-clients: Martian (search snippet only, partial)
- https://www.airealist.ai/p/arcee-conductor-is-now-generally: Arcee Conductor GA (search snippet only, partial)
