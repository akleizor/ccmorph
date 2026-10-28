# Topic 5: Capability, price and effort ladder per model

Snapshot date: 2026-10-28. All numbers come from vendor pages, Artificial Analysis (AA), Arena (ex-LMArena) and vendor launch posts. I ran no benchmarks. "AA index" means the Intelligence Index v4.3.2, which AA computes from 10 evals: AA-Briefcase, GDPval-AA, AutomationBench-AA, Terminal-Bench 4.0, SciCode, HLE, GDP.pdf, CritPt, AA-Omniscience and AA-LCR ([AA index page](https://artificialanalysis.ai/evaluations/artificial-analysis-intelligence-index)).

## 1. Key findings

- **Every model on the list exists publicly except as noted:** `claude-haiku-5-5` is released ([docs](https://platform.claude.com/docs/en/models/haiku-5-5/overview)), `gpt-6-sol` is still a live ID but superseded by `gpt-6.1-sol` ([model page](https://developers.openai.com/api/docs/models/gpt-6-sol.md)), and `kimi-k3` has been superseded by `kimi-k3.1-pro`, which is 11 AA points better and 33% cheaper ([AA article](https://artificialanalysis.ai/articles/kimi-k3-1-pro-launch-benchmarks)). The "current Grok" is `grok-4.7`, from SpaceXAI ([xAI docs](https://docs.x.ai/developers/grok-4-7)).
- **Effort is a bigger quality lever than model choice.** On AA, Opus 5.5 goes from 42 (low) to 58 (max) at about 11x the cost per task, and Sonnet 5.5 goes from 36 to 56 ([AA leaderboard](https://artificialanalysis.ai/leaderboards/models)). The router's unit should therefore be (model, effort), not model.
- **GPT-6.1 Sol is the API price/performance leader:** AA 50 at high effort for $0.32 per index task, against Opus 5.5 medium at 51 for $1.34 ([AA leaderboard](https://artificialanalysis.ai/leaderboards/models)). Opus 5.5 holds the top absolute score (58) and the top Arena WebDev score ([Arena WebDev](https://arena.ai/leaderboard/code/webdev?rankBy=labs)).
- **Several rows are dominated and can be dropped or demoted:** Haiku 4.5 (Haiku 5.5 is 10x cheaper per input token and far stronger), Fable 5.1 (AA 53 vs Opus 5.5 at 58, at 2.5x the price), DeepSeek V4 Pro (AA 36 vs Flash-1017 at 45) and Kimi K3.
- **Effort vocabularies differ and the defaults differ,** so the router must always pass effort explicitly. Claude and OpenAI take 5 levels, Kimi, GLM and DeepSeek take `low/high/max`, Gemini takes `low/medium/high`, and Grok takes `low` to `xhigh`. Kimi and GLM default to `max`, which is the most expensive setting ([Kimi](https://platform.kimi.ai/docs/guide/use-reasoning-effort), [GLM](https://docs.z.ai/guides/llm/glm-5.3)).
- **Three capabilities are single-vendor:** live X/Twitter search (`x_search`, Grok only, [docs](https://docs.x.ai/developers/tools/x-search)), native video understanding (Gemini; GPT-6.1 Sol and Terra list video as unsupported, [Sol page](https://developers.openai.com/api/docs/models/gpt-6.1-sol)) and 256k to 1M-token long-context retrieval (Gemini 4 Argon leads, [DeepMind card](https://deepmind.google/models/model-cards/gemini-4-argon/)).
- **Vendor benchmark numbers conflict with each other and with AA.** Terminal-Bench 4.0 for Fable 5.1 is 55.8% in [Anthropic's table](https://www.anthropic.com/claude-opus-5-5) and 57.9% in [Google's](https://deepmind.google/models/model-cards/gemini-4-argon/). Sonnet 5.5 is 70.6% in [Anthropic's table](https://www.anthropic.com/claude-sonnet-5-5) and 64% on AA. Use AA as the common yardstick and treat vendor numbers as priors.
- **Two prices have expiry dates:** Gemini 4 Argon doubles on 2026-12-01 ($2/$10 to $4/$20, [Google](https://ai.google.dev/gemini-api/docs/latest-model)), and DeepSeek charges double during UTC peak hours ([DeepSeek pricing](https://api-docs.deepseek.com/quick_start/pricing)).

## 2. Details

### 2.1 How to read the scores

AA's leaderboard lists one row per (model, effort). I use AA's "cost per index task" (input, cache, reasoning and answer tokens at list price) because it already folds in how many tokens a model burns. For subscription quota the relevant cost is tokens, not dollars. AA also publishes output tokens per task (Kimi K3.1 Pro uses 55K, [AA](https://artificialanalysis.ai/articles/kimi-k3-1-pro-launch-benchmarks)).

Speed figures (tokens/s) are the leaderboard's second numeric column. The captured page had no column headers, so the meaning is inferred; it matches vendor labels (Haiku 5.5 "fastest" at 237, Fable "slower" at 68, [Claude overview](https://platform.claude.com/docs/en/about-claude/models/overview)) but is unconfirmed. "With fallback" on Claude rows means a classifier refusal falls back to another Claude model.

### 2.2 Anthropic

Source for specs, prices and effort: [overview](https://platform.claude.com/docs/en/about-claude/models/overview), [pricing](https://platform.claude.com/docs/en/about-claude/pricing), [effort](https://platform.claude.com/docs/en/build-with-claude/effort). All 5.x models: 1M context, 128K max output, text and image in, text out, knowledge cutoff Jun 2026.

- **Opus 5.5** (`claude-opus-5-5`): "long-running agentic coding and knowledge work". Effort `low/medium/high/xhigh/max`, default `medium`; adaptive thinking is always on and `thinking: disabled` returns 400. Vendor numbers at max effort ([announcement](https://www.anthropic.com/claude-opus-5-5)): Terminal-Bench 4.0 66.4% (xhigh), FrontierCode v1.1 54.4%, CursorBench 4.0 57.8%, GDPval-AA 1846 Elo, HLE with tools 67.7%, OSWorld 2.1 81.8% (partial). Arena WebDev #1 at 1807 ([Arena](https://arena.ai/leaderboard/code/webdev?rankBy=labs)); Arena Text #2 at 1507 ([Arena Text](https://arena.ai/leaderboard/text)). AA 58 (max, $5.98 per task, ~98 t/s). Anthropic says its low effort catches 72% of known bugs in a code-review test, so low is usable for subagents.
- **Sonnet 5.5** (`claude-sonnet-5-5`): "best speed/intelligence mix", 30%+ faster than Sonnet 5, good at bug fixes and documents/slides ([announcement](https://www.anthropic.com/claude-sonnet-5-5)). Effort has the same five levels, API default `high` (Claude Code and the apps default to medium). Thinking: adaptive, or `between_tools` (no up-front thinking) at high or below; `disabled` is rejected ([effort docs](https://platform.claude.com/docs/en/build-with-claude/effort)). Vendor: Terminal-Bench 4.0 70.6%, CursorBench 55.5%, GDPval-AA 1844, AA-Briefcase 1811, OSWorld 2.1 80.1%. AA 56 (max), 47 (high, $0.88), speed 113 to 141 t/s.
- **Haiku 5.5** (`claude-haiku-5-5`): built for "classification, routing, extraction, and subagent tasks". Effort five levels, default `medium`; `thinking: disabled` is allowed at high or below. Price steps up above 100K prompt tokens (see table). Vendor: Terminal-Bench 4.0 39.2%, GDPval-AA 1620, OSWorld 2.1 72.4% (offline subset), HLE 57.4% with tools, FrontierCode 46.4%. AA 43 (max, $0.21), 41 (xhigh, $0.12), 34 (medium, $0.05); speed 174 to 237 t/s.
- **Haiku 4.5** (`claude-haiku-4-5`): legacy, 200K context, 64K output, $1/$5, manual extended thinking with `budget_tokens`, no effort parameter ([page](https://platform.claude.com/docs/en/models/haiku-4-5/overview)). Haiku 5.5's own comparison shows 4.5 at GDPval-AA 735, OSWorld 15.7% and Terminal-Bench 0.0% ([announcement](https://www.anthropic.com/claude-haiku-5-5)). Drop it.
- **Fable 5.1** (`claude-fable-5-1`): "demanding reasoning and long-horizon agentic work", effort five levels, default `high`, thinking always on ([page](https://platform.claude.com/docs/en/models/fable-5-1/overview)). Anthropic itself says the gap to Opus 5.5 "is narrower than these scores suggest" ([announcement](https://www.anthropic.com/claude-opus-5-5)). AA 53 (max, $7.63), and the curve is flat: 47 (low) to 53 (max). Arena Search #5 at 1246 ([Arena Search](https://arena.ai/leaderboard/chat/search)). Use it only as an escalation rung. Claude Mythos 5.1 has the same specs but is gated to verified organisations.

### 2.3 OpenAI

Source: [models](https://developers.openai.com/api/docs/models), [pricing](https://developers.openai.com/api/docs/pricing), per-model `.md` pages, [GPT-6 guide](https://developers.openai.com/api/docs/guides/latest-model), [reasoning guide](https://developers.openai.com/api/docs/guides/reasoning). All: 1.05M context window (922K max input), 128K max output, text and image in, text out. Tools: functions, web search, file search, computer use. Cache writes cost 1.25x input (30-minute) or 2.5x (12-hour). A "Fast" mode costs 2x and "Flex/Batch" costs 0.5x.

- **GPT-6 Astra** (`gpt-6-astra`, $10/$50): flagship, effort `low/medium/high/xhigh/max`, no `none` (HTTP 400). Default not stated on its page. Strongest on science and hard engineering: Terminal-Bench Science 0.1 68.1% (best), FrontierSWE v2 65.5% (best of four), OSWorld-2.0 offline 72.6%; Terminal-Bench 4.0 58.2%, DeepSWE 74.1% ([DeepMind card](https://deepmind.google/models/model-cards/gemini-4-argon/)). AA 53 (max, $3.26, 61 t/s).
- **GPT-6.1 Sol** (`gpt-6.1-sol`, $2/$10, cache read $0.10): "near-Astra for one-fifth the price". Effort `low/medium/high/xhigh/max`, default `medium`, no `none`/`minimal`. OpenAI claims it matches Astra on DeepSWE v1.1 at ~1/5 the cost, beats Opus 5.5 medium on AutomationBench by 2.2 points at ~1/3 the cost, and beats Opus 5.5 on GDP.pdf (PDF question answering) at under half the cost per task ([announcement](https://openai.com/index/introducing-gpt-6-1-sol/)). AA 52 (max, $0.72, 74 t/s), 50 (high, $0.32), 48 (medium, $0.21). AA-Briefcase is weaker at 1,557 Elo vs Opus 5.5's 1,822 ([BenchmarkList](https://benchmarklist.com/models/openai-gpt-6.1-sol/)). Video and audio input are unsupported.
- **GPT-6 Sol** (`gpt-6-sol`): the previous Sol, same $2/$10, cache read $0.20, effort `none/low/medium/high/xhigh/max`, default `medium` ([page](https://developers.openai.com/api/docs/models/gpt-6-sol.md)). Superseded; use 6.1.
- **GPT-6.1 Terra** (`gpt-6.1-terra`, $1/$5): "visual reasoning, precise computer use, clear natural writing"; effort `none` to `max`, default `medium`. AA 48 (max), 41 (medium, $0.10 per task), 91 t/s.
- **GPT-6 Luna** ($0.10/$0.50, cache read $0.01): "most efficient, focused high-volume". Effort `none` to `max`, default `medium`. AA 38 (max, $0.07), 22 (low), 18 (none); 120 t/s. Claude's launch post puts Haiku 5.5 ahead on Terminal-Bench 4.0 (39.2% vs 16.4%) and OSWorld (72.4% vs 48.9%) at the same list price ([announcement](https://www.anthropic.com/claude-haiku-5-5)).
- **Search and browsing:** Arena Search top is GPT-5.6 Sol (xhigh) at 1261; GPT-6 Astra (max) is 1234 and GPT-6.1 Sol (max) 1233 ([Arena Search](https://arena.ai/leaderboard/chat/search)). A secondary tracker gives GPT-6.1 Sol a BrowseComp of 95.4% ([BenchmarkList](https://benchmarklist.com/models/openai-gpt-6.1-sol/)), while another lists Astra at 91.5% as #2 and does not list Sol ([LLMLearner excerpt](https://llmlearner.com/rankings/browsecomp)). They conflict; BrowseComp is saturated near 90 to 95 either way.

### 2.4 Google

- **Gemini 4 Argon** (`gemini-4-argon`): thinking levels `low/medium/high`, default `medium`. One 1,048,576-token window shared by input, thinking and output; default `max_output_tokens` 65,536, long decode to ~1M. Price $2/$10 through 2026-11-30, then $4/$20; cached input 95% off ([docs](https://ai.google.dev/gemini-api/docs/latest-model)). AA 53 (high, $1.62, 79 t/s). Arena Text #1 at 1525 (preliminary, 2026-10-08, [Arena Text](https://arena.ai/leaderboard/text)); WebDev #5 at 1674 ([Arena WebDev](https://arena.ai/leaderboard/code/webdev?rankBy=labs)). DeepMind's card ([card](https://deepmind.google/models/model-cards/gemini-4-argon/)): DeepSWE v1.1 77.9% (best), Vibe Code Bench 91.9%, AutomationBench 51.3%, Vals Index 68.9%, GraphWalks 256k to 1M 84.2% (Astra 71.8%, Opus 5.5 66.8%), LVBench (video) 91.7% (Astra 87.5%, Opus 5.5 83.7%), Chartography 71.6%. Weaker on FrontierSWE v2 (55.0%) and Terminal-Bench 4.0 (57.4% vs Opus 5.5's 66.4%). Agentic video understanding is GA. Google Search grounding costs $14 per 1,000 requests after a free allowance ([pricing](https://ai.google.dev/gemini-api/docs/pricing)).
- **Gemini 3.8 Flash** (`gemini-3.8-flash`): $0.75/$3.75 through 2026-12-31, then $1.50/$7.50; cache read $0.075 ([pricing](https://ai.google.dev/gemini-api/docs/pricing)). AA 41 (high), 40 (medium), 33 (low), 134 t/s, but $3.49 per task at high, so it burns many tokens and is not cheap in practice. Effort values are not documented on pages I could open (AA lists high, medium and low variants).
- Gemini 3.1 Pro Preview scores 30 on AA and is superseded by Argon as the Pro tier.

### 2.5 xAI

- **Grok 4.7** (`grok-4.7`, released 2026-09-21): $2/$6, cached $0.50; at 200K tokens and above the whole request is billed at $4/$12 (cached $1). 500K context, text and image in. `reasoning_effort` `low/medium/high/xhigh`, default `high`, cannot be disabled ([reasoning docs](https://docs.x.ai/developers/model-capabilities/text/reasoning), [pricing](https://docs.x.ai/developers/pricing)). Grok 4.7 Fast exists only inside Cursor and Grok Build, not on the public API.
- **Live X search:** `x_search` tool at $5 per 1,000 posts and $10 per 1,000 profiles, plus tokens. It has handle allow/exclude lists (max 20), date range, and optional image and video understanding of posts ([docs](https://docs.x.ai/developers/tools/x-search)). `web_search` is $5 per 1,000 calls. I found no independent benchmark of X-search quality, only the tool's existence.
- **Quality:** xAI-reported (xhigh): CursorBench 4.0 46.3%, DeepSWE 71.0% (high), AA-Briefcase 1,657, Terminal-Bench 4.0 37.6%, Harvey Legal 19.6% ([announcement](https://x.ai/news/grok-4-7)). AA 46 (xhigh and high), 93 t/s, but $2.36 to $3.24 per task, which is high for that score. It is absent from the top 10 of Arena Search, and I could not retrieve its exact rank.
- Other API models: `grok-build-0.1` ($1/$2, 256K), `grok-4.3` ($1.25/$2.50, 1M) ([pricing](https://docs.x.ai/developers/pricing)).

### 2.6 DeepSeek

([pricing](https://api-docs.deepseek.com/quick_start/pricing), [thinking mode](https://api-docs.deepseek.com/guides/thinking_mode/)). Both models: 1M context, 384K max output, thinking on by default, switchable with `thinking: {type: enabled|disabled}`. Effort `low/high/max`, default `high`; `minimal` maps to low, `medium` and `xhigh` map to high, `ultra` maps to max. The Anthropic-format endpoint accepts `output_config.effort` with the same values. Peak hours are 01:00 to 04:00 and 06:00 to 10:00 UTC, Monday to Friday; off-peak is half price.

- **`deepseek-flash`** = V4.1-Flash-1017 (released 2026-10-17, MIT weights): input miss $0.15 off-peak ($0.30 peak), cache hit $0.003 ($0.006), output $0.60 ($1.20). Supports vision. AA 45 (max, **$0.26 per task**, 170 t/s). DeepSeek-reported: Terminal-Bench 4.0 42.3%, DeepSWE 77.0%, HLE (no tools) 42.6%, Chartography 83.0% with tools; the TECHi write-up notes none are independently reproduced ([TECHi](https://www.techi.com/deepseek-v4-1-flash-1017-update-benchmarks-price/)).
- **`deepseek-v4-pro`** = V4-Pro-0813: miss $0.66 off-peak ($1.32 peak), cache hit $0.022 ($0.044), output $1.98 ($3.96). No vision. AA 36 (max), 20 (non-reasoning). Older and worse on AA than Flash-1017 while costing 3x per token.

### 2.7 Moonshot (Kimi)

([pricing](https://platform.kimi.ai/docs/pricing/chat), [effort](https://platform.kimi.ai/docs/guide/use-reasoning-effort), [K3 docs](https://platform.kimi.ai/docs/guide/kimi-k3-quickstart)). `kimi-k3` is labelled "previous-generation" and costs $3/$15. The current flagship is **`kimi-k3.1-pro`**: $2/$10, cache hit $0.20, 1,048,576-token context, `max_completion_tokens` default 131,072. Thinking is always on; `reasoning_effort` `low/high/max`, default `max`. A `-highspeed` variant is $4/$20. Text and image input (base64 or uploaded file, no public URLs). 2.8T total / 104B active parameters; weights not yet published.

AA 55 (4th overall, $2.11 per task, 41 t/s, slowest of the group). Terminal-Bench 4.0 67% (AA's run, highest of any model), GDPval-AA 1846 Elo (3rd), AutomationBench-AA 73%, HLE 52%, hallucination rate 44% ([AA article](https://artificialanalysis.ai/articles/kimi-k3-1-pro-launch-benchmarks)). Arena WebDev #4 at 1692 (preliminary). BrowseComp 92.1% (#1 on a secondary tracker, [LLMLearner excerpt](https://llmlearner.com/rankings/browsecomp)). Kimi's docs say its web-search tool "is being updated and is not recommended" for now.

### 2.8 Zhipu (GLM)

([model docs](https://docs.z.ai/guides/llm/glm-5.3), [pricing](https://docs.z.ai/guides/overview/pricing)). **GLM-5.3** (`glm-5.3`): $1.40 in, $0.26 cached, $4.40 out; 1M context, 128K output, **text-only input**. Reasoning is always on; `reasoning_effort` `low/high/max`, default `max`. Sending `thinking.type: "disabled"` fails on this model. AA 45 (max, $2.01, 76 t/s), 34 (low, $0.85). **GLM-5.3-Flash**: $0.15/$0.03/$0.50, AA 42 ($0.28 per task); FlashX is $0.37/$0.075/$1.25. I did not capture GLM-5.3's launch benchmarks (the blog excerpt was truncated).

### 2.9 Perplexity Sonar (search specialist)

Sonar Chat Completions is being folded into Perplexity's **Agent API**, which can call OpenAI, Anthropic, Google, xAI, Z.AI, Moonshot and NVIDIA models plus Sonar, with built-in web search ([migration page](https://docs.perplexity.ai/docs/sonar/models)). Tool pricing ([pricing](https://docs.perplexity.ai/docs/getting-started/pricing)): Search API $5 per 1,000 requests ($1 per 1,000 "fast"), agent `web_search` $2.50 per 1,000, `fetch_url` $0.50 per 1,000. Sonar Pro input is $3 per 1M tokens ([page excerpt](https://perplexity.mintlify.app/docs/sonar/models/sonar-pro)); I did not capture its output price, context or any benchmark. On Arena Search no Sonar model appeared in the top 10 ([Arena Search](https://arena.ai/leaderboard/chat/search)). The same pricing page lists `pplx-decider-v1.2` ($0.02 per 1M input, free output) and `pplx-decider-flash-v1.2` ($0.01); I did not open their docs, so I do not know what they decide.

### 2.10 Work-type view (what the evidence supports)

| Work type | Best evidence | Leaders |
|---|---|---|
| Agentic coding / terminal | Terminal-Bench 4.0, DeepSWE v1.1, FrontierSWE, CursorBench | Opus 5.5, Sonnet 5.5, Kimi K3.1 Pro; DeepSWE: Argon 77.9; FrontierSWE: Astra 65.5 |
| Web/UI build | Arena WebDev | Opus 5.5 1807, Astra 1774, Kimi K3.1 Pro 1692 |
| Knowledge work / docs | GDPval-AA, AA-Briefcase, GDP.pdf | Opus 5.5 1846, Sonnet 5.5 1844, Kimi K3.1 Pro 1846; PDFs: GPT-6.1 Sol, Astra |
| Computer use | OSWorld 2.x | Opus 5.5 81.8, Sonnet 5.5 80.1, Haiku 5.5 72.4 (cheap) |
| Web search | Arena Search; BrowseComp (saturated) | GPT-5.6/6.x, Claude; Grok not in top 10 |
| X/Twitter | tool availability only | Grok 4.7 (only option) |
| Video / long context | LVBench, GraphWalks | Gemini 4 Argon |
| Science / hard reasoning | Terminal-Bench Science, HLE | Astra 68.1, Opus 5.5, Fable 5.1 |
| Writing | Arena Text only (no category page retrieved) | Argon 1525, Opus 5.5 1507 |
| Tool-use reliability | none comparable | No 2026 tau-bench figures found |

### 2.11 Per-vendor tables

Prices are $ per 1M tokens, list. "?" cells are footnoted at the end of the section.

**Anthropic**

| Model | AA index (effort) | Input / Output | Cache read / write (5m) | Context / max out | Effort (default) | Thinking off? |
|---|---|---|---|---|---|---|
| claude-opus-5-5 | 58 (max); 51 (medium) | $4 / $20 | $0.20 / $5 | 1M / 128K | low, medium*, high, xhigh, max | No |
| claude-sonnet-5-5 | 56 (max); 47 (high) | $2 / $10 | $0.10 / $2.50 | 1M / 128K | low, medium, high*, xhigh, max | No (`between_tools` at <= high) |
| claude-haiku-5-5 | 43 (max); 34 (medium) | $0.10 / $0.50 (<=100K prompt); $0.50 / $2.50 (>100K) | $0.01 / $0.125 (<=100K); $0.05 / $0.625 (>100K) | 1M / 128K | low, medium*, high, xhigh, max | Yes at <= high |
| claude-haiku-4-5 | ?1 | $1 / $5 | $0.10 / $1.25 | 200K / 64K | none (manual `budget_tokens`) | Yes |
| claude-fable-5-1 | 53 (max); 47 (low) | $10 / $50 | $0.25 / $12.50 | 1M / 128K | low, medium, high*, xhigh, max | No |

**OpenAI**

| Model | AA index (effort) | Input / Output | Cache read / write (30m) | Context / max out | Effort (default) | `none`? |
|---|---|---|---|---|---|---|
| gpt-6-astra | 53 (max); 46 (low) | $10 / $50 | $1.00 / $12.50 | 1.05M / 128K | low, medium, high, xhigh, max (default ?2) | No (400) |
| gpt-6.1-sol | 52 (max); 48 (medium) | $2 / $10 | $0.10 / $2.50 | 1.05M / 128K | low, medium*, high, xhigh, max | No |
| gpt-6-sol | ?3 | $2 / $10 | $0.20 / ?3 | 1.05M / 128K | none, low, medium*, high, xhigh, max | Yes |
| gpt-6.1-terra | 48 (max); 33 (none) | $1 / $5 | $0.05 / $1.25 | 1.05M / 128K | none, low, medium*, high, xhigh, max | Yes |
| gpt-6-luna | 38 (max); 18 (none) | $0.10 / $0.50 | $0.01 / $0.125 | 1.05M / 128K | none, low, medium*, high, xhigh, max | Yes |

**Google**

| Model | AA index (effort) | Input / Output | Cache read | Context / max out | Effort (default) |
|---|---|---|---|---|---|
| gemini-4-argon | 53 (high) | $2 / $10 until 2026-11-30, then $4 / $20 | $0.10 (then $0.20) | 1.05M shared / 64K default, to ~1M | low, medium*, high |
| gemini-3.8-flash | 41 (high); 33 (low) | $0.75 / $3.75 until 2026-12-31, then $1.50 / $7.50 | $0.075 | 1M / ?4 | ?4 |

**xAI**

| Model | AA index (effort) | Input / Output | Cache read | Context / max out | Effort (default) |
|---|---|---|---|---|---|
| grok-4.7 | 46 (high, xhigh); 42 (low) | $2 / $6 ($4 / $12 at >=200K) | $0.50 ($1 at >=200K) | 500K / ?5 | low, medium, high*, xhigh; cannot be disabled |

**DeepSeek** (peak / off-peak, off-peak is half)

| Model | AA index (effort) | Input miss / Output | Cache hit | Context / max out | Effort (default) |
|---|---|---|---|---|---|
| deepseek-flash (V4.1-Flash-1017) | 45 (max) | $0.30 / $1.20 peak; $0.15 / $0.60 off-peak | $0.006 / $0.003 | 1M / 384K | low, high*, max; thinking toggle |
| deepseek-v4-pro (0813) | 36 (max); 20 (off) | $1.32 / $3.96 peak; $0.66 / $1.98 off-peak | $0.044 / $0.022 | 1M / 384K | low, high*, max; thinking toggle |

**Moonshot, Zhipu, Perplexity**

| Model | AA index (effort) | Input / Output | Cache read | Context / max out | Effort (default) |
|---|---|---|---|---|---|
| kimi-k3.1-pro | 55 | $2 / $10 | $0.20 | 1.05M / 1.05M (default 131K) | low, high, max*; always on |
| kimi-k3 | 44 (max) | $3 / $15 | $0.30 | 1.05M / ?6 | low, high, max*; always on |
| glm-5.3 | 45 (max); 34 (low) | $1.40 / $4.40 | $0.26 | 1M / 128K | low, high, max*; always on |
| glm-5.3-flash | 42 | $0.15 / $0.50 | $0.03 | 1M / ?6 | ?6 |
| sonar-pro (legacy path) | ?7 | $3 / ?7 | ?7 | ?7 | n/a |

**Footnotes for "?"**

1. Haiku 4.5 is not on the AA leaderboard snapshot I fetched (the board shows the top ~117 of 132 models, and only the first screen was captured).
2. Astra's model page lists supported values but no default; the guide states defaults only for Sol and Luna.
3. GPT-6 Sol has no AA row in the snapshot, and its cache-write price is not on the pricing table I fetched. A third-party page claims "GPT-5.6 Sol" scores 58.9 ([benchgap](https://benchgap.net/b/artificialanalysis/current)), which conflicts with AA's own board where Opus 5.5 (max) is first at 58. I did not resolve this.
4. Gemini 3.8 Flash's model page did not expose thinking values or max output in the text I could read.
5. Grok 4.7's max output is not stated on the pages I read.
6. Kimi K3 max output, and GLM-5.3-Flash max output and effort values, were not on pages I opened.
7. Sonar Pro's output price, context and index score were not retrievable.

### 2.12 Cross-vendor table

AA score is the best configuration shown, with the effort in brackets. "Best at" tags are my reading of sections 2.2 to 2.10.

| Model | Overall (AA) | Best at | In $/M | Out $/M | Context | Effort values |
|---|---|---|---|---|---|---|
| claude-opus-5-5 | 58 (max) | agentic-coding, knowledge-work, webdev | 4 | 20 | 1M | low, medium*, high, xhigh, max |
| claude-sonnet-5-5 | 56 (max) | agentic-coding, docs/slides, fast | 2 | 10 | 1M | low, medium, high*, xhigh, max |
| claude-haiku-5-5 | 43 (max) | bulk-subagent, computer-use-cheap, speed | 0.10 | 0.50 | 1M | low, medium*, high, xhigh, max |
| claude-haiku-4-5 | ?1 | legacy (dominated) | 1 | 5 | 200K | none |
| claude-fable-5-1 | 53 (max) | hard-reasoning, long-horizon, research | 10 | 50 | 1M | low, medium, high*, xhigh, max |
| gpt-6-astra | 53 (max) | science/terminal, hard-engineering, PDFs | 10 | 50 | 1.05M | low, medium, high, xhigh, max |
| gpt-6.1-sol | 52 (max) | price-performance, coding, PDFs | 2 | 10 | 1.05M | low, medium*, high, xhigh, max |
| gpt-6-sol | ?3 | superseded | 2 | 10 | 1.05M | none, low, medium*, high, xhigh, max |
| gpt-6.1-terra | 48 (max) | vision, computer-use, writing | 1 | 5 | 1.05M | none, low, medium*, high, xhigh, max |
| gpt-6-luna | 38 (max) | cheapest-bulk, classification, extraction | 0.10 | 0.50 | 1.05M | none, low, medium*, high, xhigh, max |
| gemini-4-argon | 53 (high) | long-context, video/multimodal, SWE (DeepSWE) | 2 (then 4) | 10 (then 20) | 1.05M | low, medium*, high |
| gemini-3.8-flash | 41 (high) | fast multimodal, volume, long-context | 0.75 | 3.75 | 1M | ?4 |
| grok-4.7 | 46 (xhigh) | X-search, web-search tool, office docs | 2 | 6 | 500K | low, medium, high*, xhigh |
| deepseek-flash | 45 (max) | cheapest-capable, coding-cheap, vision | 0.15 (off-peak) | 0.60 | 1M | low, high*, max (+off) |
| deepseek-v4-pro | 36 (max) | none (dominated by Flash) | 0.66 (off-peak) | 1.98 | 1M | low, high*, max (+off) |
| kimi-k3.1-pro | 55 | terminal/agentic, browse, webdev | 2 | 10 | 1.05M | low, high, max* |
| kimi-k3 | 44 (max) | superseded | 3 | 15 | 1.05M | low, high, max* |
| glm-5.3 | 45 (max) | coding (text-only), agentic | 1.40 | 4.40 | 1M | low, high, max* |
| sonar-pro | ?7 | cited web search | 3 | ?7 | ?7 | n/a |

`*` = documented default.

## 3. What this means for our design

1. **Make (model, effort) the routing unit and store the AA curve per model.** The five-point curves (e.g. Opus 5.5: 42/51/54/56/58 at $0.55/$1.34/$1.82/$3.46/$5.98) let a simple algorithm pick the cheapest rung that clears a quality threshold, with no LLM call ([AA leaderboard](https://artificialanalysis.ai/leaderboards/models)). Fable's curve is nearly flat (47 to 53), so it is never worth its price except as a last escalation.
2. **Apply hard filters before scoring.** The evidence gives clean gates: X/Twitter needs `grok-4.7`; video needs Gemini; images exclude GLM-5.3 and DeepSeek V4 Pro; prompts above 500K exclude Grok; at 200K and above Grok's per-token cost doubles and above 100K multiplies Haiku 5.5's price by 5. Then rank the survivors.
3. **Always send an explicit effort.** Defaults span medium (Opus 5.5, Haiku 5.5, GPT-6.1 family, Gemini), high (Sonnet API, Grok, DeepSeek) and max (Kimi, GLM). Letting Kimi or GLM default to `max` spends the most tokens for a small gain. Keep a per-model map from a 3-tier router scale (low/mid/high) to the native strings, e.g. mid = Claude `medium`, OpenAI `medium`, Gemini `medium`, DeepSeek `high`, Kimi `high`, GLM `high`, Grok `medium`.
4. **Prune dominated rows from the default pool:** Haiku 4.5, GPT-6 Sol, Kimi K3, DeepSeek V4 Pro, Gemini 3.1 Pro; keep Fable 5.1 as escalation only. Fewer rows means fewer wrong picks and a smaller table to maintain.
5. **Tier by purpose.** Mechanical bulk: GPT-6 Luna (low), Haiku 5.5 (low/medium), DeepSeek Flash at off-peak. Everyday coding: GPT-6.1 Sol (medium/high) on API; Sonnet 5.5 (medium/high) on subscription. Hard agentic: Opus 5.5 (high/xhigh), Kimi K3.1 Pro or Astra on API. Long context and video: Argon. Search: GPT-6.x or Claude with their server-side search; Grok only for X.
6. **Use tokens, not dollars, as the cost on subscription profiles.** Subscription quota is perishable, so prefer the highest rung the pacing allows. Use AA's output tokens per task as the quota-burn estimate and AA cost per task for paid API profiles.
7. **Give every price an expiry date.** Argon's price doubles on 2026-12-01, Gemini 3.8 Flash's doubles on 2027-01-01, DeepSeek has UTC peak windows (batch jobs can wait for off-peak), and OpenAI's GPT-5.6 Sol promo runs to 2026-11-21 ([OpenAI pricing](https://developers.openai.com/api/docs/pricing)).
8. **Treat published scores as priors, not truth.** Vendors disagree by 2 to 6 points on the same benchmark (Terminal-Bench 4.0: Fable 55.8 vs 57.9; Sonnet 5.5 70.6 vs AA's 64). The table should carry a source and date per number. The router should update a per-(model, task tag) success estimate from its own outcomes, because no cross-vendor tool-use reliability benchmark exists (I found no 2026 tau-bench figures).
9. **Keep the decider's job small.** It only needs to output difficulty (1 to 5), modality need (none/image/video/X), context bucket and task tag (code-edit, agentic-code, docs, search, bulk); model and effort then come from table lookup. Perplexity lists hosted "decider" models at $0.01 to $0.02 per 1M input tokens ([pricing](https://docs.perplexity.ai/docs/getting-started/pricing)); I did not verify what they do.
10. **Open gaps:** Gemini 3.8 Flash effort values, Astra's default effort, Sonar Pro output price, and Arena Vision and Creative-Writing ranks (those category pages did not load).

Skipped: no benchmark of my own, no code. Risk: the AA tokens/s column is inferred, and all vendor-reported scores are self-reported.

## 4. Sources

- https://artificialanalysis.ai/leaderboards/models : AA leaderboard, per-effort index, cost per task, speed (column headers not captured)
- https://artificialanalysis.ai/evaluations/artificial-analysis-intelligence-index : AA Index v4.3.2 composition (search excerpt)
- https://artificialanalysis.ai/models/claude-opus-5-5 : AA model page (index composition, no speed row captured)
- https://artificialanalysis.ai/models/comparisons/gpt-6-1-sol-vs-gpt-6-astra : AA comparison page (partly rendered)
- https://artificialanalysis.ai/articles/kimi-k3-1-pro-launch-benchmarks : AA review of Kimi K3.1 Pro
- https://benchgap.net/b/artificialanalysis/current : third-party index tracker (search excerpt; conflicting claim)
- https://platform.claude.com/docs/en/about-claude/models/overview : Claude model comparison
- https://platform.claude.com/docs/en/models/opus-5-5/overview : Opus 5.5 spec page
- https://platform.claude.com/docs/en/models/sonnet-5-5/overview : Sonnet 5.5 spec page
- https://platform.claude.com/docs/en/models/haiku-5-5/overview : Haiku 5.5 spec and tiered pricing
- https://platform.claude.com/docs/en/models/fable-5-1/overview : Fable 5.1 spec page
- https://platform.claude.com/docs/en/models/haiku-4-5/overview : Haiku 4.5 legacy spec
- https://platform.claude.com/docs/en/build-with-claude/effort : Claude effort values and defaults
- https://platform.claude.com/docs/en/about-claude/pricing : Claude price list incl. cache
- https://www.anthropic.com/claude-opus-5-5 : Opus 5.5 launch post and benchmarks
- https://www.anthropic.com/claude-sonnet-5-5 : Sonnet 5.5 launch post and benchmarks
- https://www.anthropic.com/claude-haiku-5-5 : Haiku 5.5 launch post and benchmarks
- https://developers.openai.com/api/docs/models : OpenAI model list, prices, efforts
- https://developers.openai.com/api/docs/pricing : OpenAI price table with cache writes
- https://developers.openai.com/api/docs/guides/latest-model : GPT-6 migration and effort guidance
- https://developers.openai.com/api/docs/guides/reasoning : OpenAI reasoning effort docs
- https://developers.openai.com/api/docs/models/gpt-6-astra.md : Astra model page
- https://developers.openai.com/api/docs/models/gpt-6.1-sol : Sol 6.1 model page (search excerpt)
- https://developers.openai.com/api/docs/models/gpt-6-sol.md : GPT-6 Sol model page
- https://developers.openai.com/api/docs/models/gpt-6.1-terra.md : Terra model page
- https://developers.openai.com/api/docs/models/gpt-6-luna.md : Luna model page
- https://openai.com/index/introducing-gpt-6-1-sol/ : GPT-6.1 Sol launch post
- https://benchmarklist.com/models/openai-gpt-6.1-sol/ : third-party benchmark aggregator (BrowseComp, Briefcase)
- https://ai.google.dev/gemini-api/docs/latest-model : Gemini 4 Argon docs (search excerpt)
- https://ai.google.dev/gemini-api/docs/pricing : Gemini API pricing
- https://deepmind.google/models/model-cards/gemini-4-argon/ : Argon model card with benchmark table (search excerpt)
- https://blog.google/innovation-and-ai/models-and-research/gemini-models/gemini-4-argon/ : Argon launch blog
- https://arena.ai/leaderboard/text : Arena Text (search excerpt)
- https://arena.ai/leaderboard/code/webdev?rankBy=labs : Arena WebDev (search excerpt)
- https://arena.ai/leaderboard/chat/search : Arena Search (search excerpt)
- https://docs.x.ai/developers/pricing : xAI price table (search excerpt)
- https://docs.x.ai/developers/grok-4-7 : Grok 4.7 spec page
- https://docs.x.ai/developers/model-capabilities/text/reasoning : Grok reasoning_effort values
- https://docs.x.ai/developers/tools/x-search : X Search tool
- https://x.ai/news/grok-4-7 : Grok 4.7 launch post
- https://api-docs.deepseek.com/quick_start/pricing : DeepSeek models, peak/off-peak prices
- https://api-docs.deepseek.com/guides/thinking_mode/ : DeepSeek thinking toggle and effort map (search excerpt)
- https://www.techi.com/deepseek-v4-1-flash-1017-update-benchmarks-price/ : Flash-1017 vendor benchmarks (search excerpt)
- https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro-0813 : V4 Pro card (search excerpt)
- https://platform.kimi.ai/docs/pricing/chat : Kimi price table
- https://platform.kimi.ai/docs/guide/kimi-k3-quickstart : Kimi K3 docs
- https://platform.kimi.ai/docs/guide/use-reasoning-effort : Kimi K3.1 Pro effort values (search excerpt)
- https://docs.z.ai/guides/llm/glm-5.3 : GLM-5.3 docs and effort (search excerpt)
- https://docs.z.ai/guides/overview/pricing : Z.AI price table
- https://z.ai/blog/glm-5.3 : GLM-5.3 launch blog (truncated excerpt only)
- https://docs.perplexity.ai/docs/getting-started/pricing : Perplexity pricing
- https://docs.perplexity.ai/docs/sonar/models : Sonar to Agent API migration
- https://perplexity.mintlify.app/docs/sonar/models/sonar-pro : Sonar Pro (search excerpt)
- https://llmlearner.com/rankings/browsecomp : BrowseComp ranking (search excerpt)
