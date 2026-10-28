# Topic 4: When to delegate to a subagent, and how agents pick the subagent's model and effort

Researched 2026-10-28. Vendor docs below were read live today (they describe Claude Code v2.1.3xx, Opus/Sonnet/Haiku 5.5, GPT-6.x). Anything I could not open or confirm is marked **unverified**.

## 1. Key findings

- **Delegation is a token multiplier, justified only by parallel, read-heavy, independent work.** Anthropic measured agents at about 4× chat tokens and multi-agent systems at about 15×, and said most coding tasks have fewer parallelizable parts than research ([Anthropic, 2025-06](https://www.anthropic.com/engineering/multi-agent-research-system)).
- **Even the original "don't" camp now accepts a narrow form.** Cognition's 2026 follow-up says multi-agent works when writes stay single-threaded and extra agents contribute intelligence (review, "smart friend", map-reduce), and calls unstructured swarms "mostly a distraction" ([Cognition, 2026-04-22](https://cognition.com/blog/multi-agents-working)).
- **For strong models on coding, 2026 studies find coordination does not help.** A 260-configuration Nature Machine Intelligence study found a capability-saturation threshold of about 45% single-agent score; on SWE-bench Verified every multi-agent architecture was slightly worse than one agent ([Nature MI, 2026-07](https://www.nature.com/articles/s42256-026-01268-y)).
- **Newer Claude models over-delegate, so vendors tell you to cap it.** Anthropic's Opus 5 guide says it "delegates to subagents more readily", that this "multiplies cost and time" on small tasks, and points to concurrency and depth caps ([Prompting Claude Opus 5](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompting-claude-opus-5)).
- **Claude Code already exposes every knob a router needs, except provider.** Model resolves per invocation, then frontmatter, then `CLAUDE_CODE_SUBAGENT_MODEL`, then the parent; `effort` exists in frontmatter and per invocation ([sub-agents docs](https://code.claude.com/docs/en/sub-agents)). A subagent cannot use a different base URL or account; that request is open and unanswered ([#38698](https://github.com/anthropics/claude-code/issues/38698)).
- **Effort is the main cost lever and the docs name subagents as the `low` use case.** Anthropic's effort table lists `low` for "simpler tasks that need the best speed and lowest costs, such as subagents" ([Effort docs](https://platform.claude.com/docs/en/build-with-claude/effort)).
- **Thinking in subagents is mostly wasted; thinking in the planner is not.** One controlled study found orchestrator-only thinking gave the largest gains and sub-agent thinking gave limited or negative benefit ([Żywot 2026, search excerpt](https://arxiv.org/html/2601.11327v2)).
- **A cheap-model-by-complexity precedent exists.** Factory Droid's Task tool takes `complexity: light|medium|heavy` and maps it to a model plus effort from user settings ([Factory docs](https://docs.factory.ai/cli/configuration/custom-droids)). That is the closest existing shape to our router.

## 2. Details

### 2.1 Evidence for and against

**For**
- Anthropic's research system: Opus 4 lead plus Sonnet 4 subagents beat single-agent Opus 4 by 90.2% on its internal eval. Token usage alone explained 80% of BrowseComp variance; "multi-agent systems work mainly because they help spend enough tokens". The authors state the downside plainly: 15× tokens, so the task must be valuable ([Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)). Its lead agent was told effort scaling rules because "agents struggle to judge appropriate effort": 1 agent with 3–10 tool calls for a fact lookup, 2–4 subagents with 10–15 calls each for comparisons, 10+ for complex research.
- Anthropic's context-engineering post: a subagent may use tens of thousands of tokens but returns a 1,000–2,000 token summary ([Effective context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)).
- Google's 180-configuration study: on a parallelizable finance task, centralized coordination was +80.9% over one agent ([Google Research](https://research.google/blog/towards-a-science-of-scaling-agent-systems-when-and-why-agent-systems-work/)).
- Cognition 2026: a reviewer agent with a clean context finds on average 2 bugs per PR, about 58% severe, and works best when it shares no context with the coder ([Cognition](https://cognition.com/blog/multi-agents-working)). Claude's own guide gives the same reason for fresh-context review ([Anthropic subagent guide](https://claude.com/resources/articles/subagents-in-claude-code)).
- OpenAI's Codex docs: use parallel agents for "read-heavy tasks such as exploration, tests, triage, and summarization", to avoid context pollution and context rot ([Codex subagents](https://developers.openai.com/codex/subagents)).

**Against**
- Cognition 2025: subtasks carry implicit decisions; sub-agents that cannot see each other's work produce inconsistent results. Its two principles are "share full agent traces" and "actions carry implicit decisions" ([Don't Build Multi-Agents](https://cognition.ai/blog/dont-build-multi-agents)). LangChain's summary: read tasks parallelize, write tasks do not ([LangChain, 2025-06](https://www.langchain.com/blog/how-and-when-to-build-multi-agent-systems)).
- MAST: 1,600+ annotated traces from 7 frameworks, 14 failure modes in three groups (system design, inter-agent misalignment, task verification) ([MAST, arXiv 2503.13657](https://arxiv.org/abs/2503.13657)). A search excerpt of the full text gives per-mode rates such as 6.8% "proceeding with wrong assumptions" and 7.4% "task derailment" (not opened in full: unverified).
- Google: on sequential planning every multi-agent variant lost 39–70%; independent agents amplified errors 17.2× versus 4.4× with an orchestrator; the coordination "tax" rises past about 16 tools ([Google Research](https://research.google/blog/towards-a-science-of-scaling-agent-systems-when-and-why-agent-systems-work/)).
- Nature MI 2026: on SWE-bench Verified the mean single-agent score was 0.488; hybrid −1.3%, centralized −2.6%, decentralized −6.4%, independent −12.8%. On Terminal-Bench (single-agent mean 0.312, below the threshold) independent gained +6.0% while centralized lost 20% ([Nature MI](https://www.nature.com/articles/s42256-026-01268-y)). The authors call the threshold "a practical selection rule rather than a universal scaling principle".
- Stanford 2026: at equal thinking-token budgets, single agents match or beat multi-agent on multi-hop reasoning, and many reported multi-agent gains come from unaccounted compute ([Tran 2026, search excerpt](https://arxiv.org/html/2604.02460)). This is reasoning QA, not coding.
- Field report: asked to fix 199 failing tests, Opus 4.6 kept spawning subagents for 3–5 files each; the reporter estimates direct edits are 5–10× cheaper (an estimate, not a measurement; issue closed) ([claude-code #27645](https://github.com/anthropics/claude-code/issues/27645)).
- Vendor guidance now says to cap: Opus 5 prompting guide and the `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS` / `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH` variables ([Opus 5 guide](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompting-claude-opus-5)).

**Reading across all of it.** Delegation gains come from three things: more total tokens on a parallel problem, a clean context for a verifier, and a cheaper model on bulk reading. Against that sit coordination loss and error amplification, which grow with sequential dependency and with how strong the single agent already is. A router that makes subagents cheap helps only the third.

### 2.2 Checklist: features that make delegation pay or lose

| Feature | Pays / loses | Source |
|---|---|---|
| Independent parts, no shared state ("three or more independent pieces") | Pays | [Anthropic guide](https://claude.com/resources/articles/subagents-in-claude-code), [Google](https://research.google/blog/towards-a-science-of-scaling-agent-systems-when-and-why-agent-systems-work/) |
| Read-heavy: search, log analysis, test runs, exploring "ten or more files" | Pays | [Codex](https://developers.openai.com/codex/subagents), [Anthropic guide](https://claude.com/resources/articles/subagents-in-claude-code) |
| Output far smaller than input (1–2k summary from tens of thousands read) | Pays | [Context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents) |
| Verbose output the parent does not need afterward (test logs, stack traces) | Pays | [Codex](https://developers.openai.com/codex/subagents), [Amp](https://ampcode.com/docs/models-and-subagents) |
| Fresh, unbiased context wanted (review, second opinion) | Pays | [Cognition 2026](https://cognition.com/blog/multi-agents-working) |
| Result is checkable (tests, diff, cited facts) so the parent can verify cheaply | Pays | [Anthropic research](https://www.anthropic.com/engineering/multi-agent-research-system) (end-state eval) |
| Single agent is weak on the task (below about 45% on the benchmark) | Pays | [Nature MI](https://www.nature.com/articles/s42256-026-01268-y) |
| Tightly coupled edits, same file, "conflicting decisions" | Loses | [Cognition 2025](https://cognition.ai/blog/dont-build-multi-agents), [Anthropic guide](https://claude.com/resources/articles/subagents-in-claude-code) |
| Sequential chain where step 2 needs all of step 1 | Loses | [Google](https://research.google/blog/towards-a-science-of-scaling-agent-systems-when-and-why-agent-systems-work/) |
| Needs the conversation's context or the user's earlier decisions | Loses | [Cognition 2025](https://cognition.ai/blog/dont-build-multi-agents), [Amp](https://ampcode.com/docs/models-and-subagents) (subagents get only what the parent writes) |
| Short task: a quick fix or focused question | Loses | [Anthropic guide](https://claude.com/resources/articles/subagents-in-claude-code) |
| Strong single agent already succeeds (above threshold) | Loses | [Nature MI](https://www.nature.com/articles/s42256-026-01268-y) |
| Subagents must talk to each other | Loses (use agent teams, about 7× tokens) | [Claude costs](https://code.claude.com/docs/en/costs) |

### 2.3 Mechanics per tool

**Claude Code** ([docs](https://code.claude.com/docs/en/sub-agents), [model config](https://code.claude.com/docs/en/model-config))
- Frontmatter fields: `description`, `tools`, `disallowedTools`, `model`, `permissionMode`, `mcpServers`, `hooks`, `maxTurns`, `skills`, `memory`, `effort`, `background`, `isolation`, and others. Only `name` and `description` are required. Unknown fields are ignored silently.
- `model` accepts an alias (`sonnet`, `opus`, `haiku`, `fable`), a full ID, or `inherit`. Resolution order: per-invocation `model` parameter, then frontmatter, then `CLAUDE_CODE_SUBAGENT_MODEL`, then the parent's model. Before v2.1.251 the environment variable came first and overrode everything.
- `CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1` (v2.1.257+) makes the model field and the per-invocation parameter ignored. **A router must not set it.**
- `effort` in frontmatter (`low` to `max`, model-dependent) overrides the session level. A per-invocation `effort` parameter exists from v2.1.292. `CLAUDE_CODE_EFFORT_LEVEL` beats both, so it must stay unset. An unsupported level falls back to the highest supported level at or below it.
- Subagents inherit the parent's thinking setting (v2.1.198+); there is no per-subagent thinking switch.
- Subagents spawned from the main conversation start in the background by default; `background: true` forces it. Background subagents lose some built-in tools. `/tasks` shows each subagent's model and effort.
- Built-in Explore runs on the `opus` alias when the parent is Fable (on subscription, Console or gateway), otherwise inherits; a user-defined `Explore` with `model: haiku` overrides it.
- A **fork** inherits the parent's system prompt and history and reads the parent's prompt cache; a normal subagent starts cold ([prompt caching](https://code.claude.com/docs/en/prompt-caching)).
- **Insertion points for a router:** a mod's `agent.spawn` hook can return `next({...e, model})` or deny; a `PreToolUse` hook's `updatedInput` replaces the Agent tool's whole input, which has an optional `model` field ([mods reference](https://code.claude.com/docs/en/plugins/mods/reference), [hooks](https://code.claude.com/docs/en/hooks)). The hooks page lists only `prompt`, `description`, `subagent_type` and `model` as Agent input fields, while the sub-agents page documents a per-invocation `effort` parameter (v2.1.292+); the two pages disagree, so whether `updatedInput` can set `effort` is **unverified** (test it on the installed version).
- **Different provider or account:** `ANTHROPIC_BASE_URL` applies to the whole session, subagents included. [#74073](https://github.com/anthropics/claude-code/issues/74073) (per-subagent provider) was closed as stale on 2026-09-08 with no maintainer reply; [#38698](https://github.com/anthropics/claude-code/issues/38698) is open with 11 comments. One commenter notes a gateway credential replaces the subscription login for that session, so subscription quota and a gateway cannot be mixed natively. The workaround is a gateway that routes by model name. [claude-code-router](https://github.com/musistudio/claude-code-router) (37,879 stars, MIT, last push 2026-10-10 per `gh`) injects the model catalogue into the Agent tool description and reads a `<CCR-SUBAGENT-MODEL>provider/model</CCR-SUBAGENT-MODEL>` tag from the subagent's prompt ([DeepWiki](https://deepwiki.com/musistudio/claude-code-router/6.2-subagent-and-workflow-routing)). [#43869](https://github.com/anthropics/claude-code/issues/43869) (April 2026, still open) reports subagent model settings ignored on a Max plan; the resolution order has since changed, and I did not re-test.

**OpenAI Codex CLI** ([subagents](https://developers.openai.com/codex/subagents), [config reference](https://developers.openai.com/codex/config-reference))
- Unconfigured subagents inherit the parent's model and effort. If a model is set without an effort, the subagent gets that model's default effort, which can be higher than the parent's.
- Controls: custom agent TOML (`model`, `model_reasoning_effort`, `developer_instructions`), `agents.default_subagent_model`, `agents.default_subagent_reasoning_effort`, `agents.max_concurrent_threads_per_session`, or a model and effort named in the spawn request.
- `model_provider`, `sandbox_mode`, `approval_policy` and `mcp_servers` placed in a custom agent file do **not** override the parent. So a Codex subagent cannot run on another provider; only on another model of the same provider.

**Cursor** ([docs](https://cursor.com/docs/subagents)): `model: inherit` (default) or a model ID, with bracket options such as `claude-opus-5[effort=high]`. Explore uses a faster model so that 10 parallel searches take the time of one.

**Gemini CLI** ([docs](https://geminicli.com/docs/core/subagents/)): frontmatter `model` (default inherit), `temperature`, `max_turns`; per-agent overrides in `settings.json` under `agents.overrides`. I saw no effort field in the table.

**OpenCode** ([docs](https://opencode.ai/docs/agents/)): each agent has `model` as `provider/model-id`, a `steps` cap, and extra options passed straight through to the provider. This is the most open design: a subagent can be on another provider.

**Factory Droid** ([docs](https://docs.factory.ai/cli/configuration/custom-droids)): `model` and `reasoningEffort` (`low|medium|high`, ignored when `inherit`). The Task tool takes `complexity: light|medium|heavy` and `run_in_background`. Model order: droid-pinned model, then complexity routing from `/settings` (a model plus optional effort, or "Auto" model router), then the parent. A blocked model falls back to the parent.

**Amp** ([models and subagents](https://ampcode.com/docs/models-and-subagents), [the Dial](https://ampcode.com/docs/the-dial)): Task and Read Thread subagents run the **same model and effort as the parent thread**. Cheaper or stronger models are reached differently: modes (`low`, `medium`, `high`, `ultra`) and the **oracle**, a separate stronger-model consultation tool. GPT-5.4 as oracle raised internal response quality from 60.8% to 68.2%, with mean latency 6.7 to 4.9 minutes ([Amp news](https://ampcode.com/news/gpt-5.4-the-new-oracle)). Amp's `rush` mode (GPT-5.5 with no reasoning) scored 44% of tasks at $0.58 and 1m32s per task, subagents included ([Rush 2.0](https://ampcode.com/news/rush-2.0)). Cognition's "smart friend" is the same idea: a small primary calls a frontier model, and they found the hard part is the small model knowing when it is stuck ([Cognition](https://cognition.com/blog/multi-agents-working)).

**GitHub Copilot**: the custom-agent reference page I opened documents `tools` but I found no `model` field on it (unverified for the coding agent). GitHub's cost guide says to pick the model per task, that reasoning level is configurable in VS Code and Copilot CLI, and that overusing reasoning models in execution-heavy work "can reduce quality" ([Optimize AI usage](https://docs.github.com/en/copilot/tutorials/optimize-ai-usage)).

### 2.4 Estimating the cost of a delegation

Published numbers:
- Multi-agent about 15× chat tokens; agents about 4× ([Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)). Agent teams about 7× a standard session ([Claude costs](https://code.claude.com/docs/en/costs)). LangChain counts one extra model call for the result to flow back through the parent ([LangChain](https://www.langchain.com/blog/choosing-the-right-multi-agent-architecture)).
- A normal subagent's first request does not read the parent's cache; it warms its own, with a 5-minute TTL by default ([prompt caching](https://code.claude.com/docs/en/prompt-caching)). Opus 5.5 lists input at $4/M and cache reads at $0.20/M, so a cold token costs 20× a cached one ([Opus 5.5](https://www.anthropic.com/claude-opus-5-5)).
- Subagent requests draw on the same plan usage as the parent ([Claude costs](https://code.claude.com/docs/en/costs)). Plans that meter per model family (the reporter in [#43869](https://github.com/anthropics/claude-code/issues/43869) watched a "Sonnet only" meter) would charge a Sonnet subagent there; my inference, not confirmed in the docs.

My own break-even model (arithmetic, not a published formula). Let `w` be the cold input price, `r` the cache-read price, `T` the parent turns left after the work, `R` the tokens the work reads, `B` the brief plus the subagent's system prompt and tools, `S` the returned summary, `n` the subagent's own later turns.
- Inline: `R·w + R·r·T` (read once, then re-read from cache every later turn).
- Delegated: `B·w + R·w_s + R·r_s·n + S·(w + r·T)`.
- Example at Opus 5.5 prices with `T=20`, `n=5`, `B=6k`, `S=1.5k`, same model: `R=5k` costs $0.04 inline versus $0.061 delegated (loses); `R=10k` is $0.080 versus $0.086 (break-even); `R=40k` is $0.32 versus $0.236 (wins, about 26% cheaper). A cheaper subagent model moves break-even lower, and perishable quota makes `w_s` effectively zero.
- Break-even near 10k tokens read agrees with Anthropic's "ten or more files" rule of thumb. The model ignores quality loss from lost context, which is why the checklist, not the arithmetic, decides coupled work.

### 2.5 Effort selection

**Levels per vendor**

| Vendor | Levels | Default | Source |
|---|---|---|---|
| Anthropic | `low`, `medium`, `high`, `xhigh`, `max` (Opus/Sonnet 4.6: no `xhigh`) | `high`; Opus 5.5 and Haiku 5.5 `medium`; Sonnet 5.5 `medium` in Claude Code but `high` on the API per the two docs | [Model config](https://code.claude.com/docs/en/model-config), [Effort](https://platform.claude.com/docs/en/build-with-claude/effort) |
| OpenAI API | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, model-dependent; GPT-6.1 Sol has no `none`/`minimal` | `gpt-5.5` `medium`; GPT-6.1 Sol `medium` | [Reasoning guide](https://developers.openai.com/api/docs/guides/reasoning) |
| Codex | `low` to `max`, plus `ultra` where the model supports it | inherit parent | [Codex subagents](https://developers.openai.com/codex/subagents) |
| Factory | `low`, `medium`, `high` | parent | [Droid](https://docs.factory.ai/cli/configuration/custom-droids) |
| Amp | modes `low`, `medium`, `high`, `ultra` (model plus effort bundled) | `medium` | [The Dial](https://ampcode.com/docs/the-dial) |

I did not verify Gemini's thinking-level names.

**What a level costs and buys.** Anthropic publishes no token-per-level table; the scale is "calibrated per model", so the same name is not the same amount of thinking across models ([Model config](https://code.claude.com/docs/en/model-config)). Published points:
- Effort covers all output tokens, including tool calls: lower effort means "fewer and terser tool calls" ([Effort](https://platform.claude.com/docs/en/build-with-claude/effort)). Claude Code's own wording: effort controls how many files are read, how much is verified, and how far it goes before checking in ([Anthropic blog, 2026-07](https://claude.com/blog/claude-model-and-effort-level-in-claude-code)).
- Opus 5.5 at default (`medium`) beats Opus 5 at `max` on Terminal-Bench 4.0 for about a fifth of the cost; a customer reports Opus 5.5 at `low` finding 72% of known bugs versus Opus 5 at `high` 56% ([Opus 5.5](https://www.anthropic.com/claude-opus-5-5)). These are launch-post claims.
- `max` "may show diminishing returns and is prone to overthinking" ([Model config](https://code.claude.com/docs/en/model-config)). Opus 5 guidance: `xhigh` for demanding coding, `max` only when it justifies unconstrained spend; sweep levels on your own evals ([Effort](https://platform.claude.com/docs/en/build-with-claude/effort)).

**Per-task guidance**
- Anthropic: `low` for subagent-style simple work; `medium` for scoped feature work; `high` for bug fixing in an existing codebase or where verification matters; `xhigh` and `max` for long or hard autonomous work ([Model config](https://code.claude.com/docs/en/model-config)). For Sonnet 5.5 agentic coding, `medium` for well-specified tasks and `high` for harder ones ([Effort](https://platform.claude.com/docs/en/build-with-claude/effort)).
- Escalation signal: if the model had the context and still got it wrong, pick a more capable **model**; if it skipped a file, skipped tests or quit a refactor, pick a higher **effort** ([Anthropic blog](https://claude.com/blog/claude-model-and-effort-level-in-claude-code)).
- OpenAI Codex: `high` for reviewer or security agents tracing logic; `medium` balances; `low` when the task is straightforward and speed matters; a fast model for narrow, high-volume agents ([Codex subagents](https://developers.openai.com/codex/subagents)).
- Do not add "double-check" or "use a subagent to verify" to briefs for Opus 5; it already verifies and the instruction adds cost ([Opus 5 guide](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompting-claude-opus-5)).

### 2.6 Decision procedure for the main agent

1. Stay inline if the task is under about 10k tokens of reading, is a quick fix, or edits files you are also editing.
2. Stay inline if the subtask needs this conversation's decisions that you cannot write into a short brief.
3. Delegate if it reads 10+ files or 3+ independent pieces, and the answer fits in about 1–2k tokens.
4. Delegate a review or second opinion when a clean context helps; never delegate two writers to the same files.
5. Write the brief: objective, output format, files and sources, boundaries, how to return (path or summary).
6. Ask the router for `(profile, model, effort)`; pass `model` and `effort` on the Agent call and do not override them.
7. If the router reports free perishable quota, lower the delegation threshold in step 1 to about 5k tokens.
8. Run independent subagents in parallel and in the background; cap at 3–5 unless the work is clearly larger.
9. Verify the result by a cheap check (tests, diff, cited lines), not by re-doing the work.
10. If it failed with full context, re-run once on the next model tier, same effort. If it skipped steps, re-run once at higher effort, same model. After that, take it inline.

**Numbers the router should return with its pick**
1. `quota_free`: whether the chosen profile's quota is perishable and still idle, with hours to reset. It changes the threshold in step 7 because marginal cash cost is zero.
2. `cost_ratio`: estimated subagent cost over estimated inline cost at the parent's price (from the section 2.4 formula, with the router's own price table). Below 1.0 favours delegation.
3. `fit`: the decider's probability that the chosen cheap model succeeds at this effort, plus the named escalation target. Below about 0.7 (my threshold, to be calibrated) the router should pick the next tier up instead.

## 3. What this means for our design

- **Do not decide whether to delegate; advertise a cost.** The decision belongs to the main agent, but Opus-class models over-delegate by default ([Opus 5 guide](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompting-claude-opus-5)). Returning `cost_ratio` and `quota_free` gives it the missing price signal. Put the 10-step procedure, shortened, in the main agent's instructions; Anthropic's own research prompts embedded effort-scaling rules because agents cannot judge effort alone ([Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)).
- **Route by task type, not by the whole conversation.** The checklist features are cheap to extract: file count, read versus write, expected output size, independence. These fit the 4B decider as typed questions (is it read-only? does it touch files the parent edits? is the answer checkable?). Evidence: [Google](https://research.google/blog/towards-a-science-of-scaling-agent-systems-when-and-why-agent-systems-work/), [Codex](https://developers.openai.com/codex/subagents).
- **Default to the cheapest tier with `low` or `medium` effort for read-only work, and spend effort on the orchestrator.** Sub-agent thinking showed limited or negative benefit ([Żywot](https://arxiv.org/html/2601.11327v2)), Anthropic names `low` for subagents, and effort also cuts tool calls ([Effort](https://platform.claude.com/docs/en/build-with-claude/effort)). Reserve `high` for reviewers and bug fixes in existing code. Caveat: the Żywot result uses 1.7B–32B models, so test it on our models first.
- **Escalate on the failure type.** Use the missing-context versus skipped-steps rule from [Anthropic's blog](https://claude.com/blog/claude-model-and-effort-level-in-claude-code) to choose between a higher tier and a higher effort. One retry only, then inline.
- **Insert the router where Claude Code allows it.** Preferred: a `PreToolUse` hook that rewrites the Agent tool input (`updatedInput`) so the main agent spends zero effort; fallback: an MCP tool the agent calls before delegating, plus the `agent.spawn` hook for the model. Do not set `CLAUDE_CODE_SUBAGENT_MODEL_FORCE` or `CLAUDE_CODE_EFFORT_LEVEL`; both disable per-task choice ([sub-agents docs](https://code.claude.com/docs/en/sub-agents)). Verify in a test that `updatedInput` can carry `effort`: the hooks page lists no such field, so fall back to a frontmatter `effort` per router tier (one subagent definition per tier) if it cannot.
- **Cross-account and cross-provider routing must live in the gateway.** Claude Code has no per-subagent base URL ([#38698](https://github.com/anthropics/claude-code/issues/38698)); Codex ignores `model_provider` in agent files ([Codex](https://developers.openai.com/codex/subagents)); OpenCode and Droid (BYOK `custom:` models) can. So the router outputs a model alias and the gateway maps it to a profile, as claude-code-router does with its tag. Subscription OAuth profiles are the hard case: a gateway credential replaces the subscription login, so the gateway itself would have to hold or forward the subscription tokens (unverified for this setup).
- **Honour the 5-minute cache and the fork distinction.** A cold subagent pays about 20× per input token against the parent's cached reads (Opus 5.5 prices). Prefer one larger subagent over many small ones, resume rather than respawn, and use a fork only when the subtask needs the whole conversation ([prompt caching](https://code.claude.com/docs/en/prompt-caching)).
- **Cap concurrency in the router.** Set `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS` and report back-pressure; Anthropic's early agents spawned 50 subagents for simple queries ([Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)).
- **Copy Factory's tiering, not its manual settings.** `light/medium/heavy` maps to `(model, effort)` in [Droid](https://docs.factory.ai/cli/configuration/custom-droids); our router can fill that mapping per request from quota state, which no tool I found does.

**Not verified:** Gemini thinking-level names; Copilot coding agent model field; claude-code-router behaviour on a live Claude Code version; whether per-subagent quota splits (Sonnet-only versus all-models) apply to routed subagents on every plan; published token multipliers per effort level (none found).

## 4. Sources

Opened and read today:
- https://www.anthropic.com/engineering/multi-agent-research-system — Anthropic multi-agent research system (90.2%, 4×/15×)
- https://cognition.ai/blog/dont-build-multi-agents — Cognition 2025, context principles
- https://cognition.com/blog/multi-agents-working — Cognition 2026 follow-up, smart friend, reviewer loop
- https://www.langchain.com/blog/how-and-when-to-build-multi-agent-systems — LangChain on read versus write
- https://www.langchain.com/blog/choosing-the-right-multi-agent-architecture — LangChain 2026, subagent extra call
- https://arxiv.org/abs/2503.13657 — MAST failure taxonomy (abstract page)
- https://www.nature.com/articles/s42256-026-01268-y — Nature MI capability-saturation threshold
- https://research.google/blog/towards-a-science-of-scaling-agent-systems-when-and-why-agent-systems-work/ — Google scaling study
- https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents — Anthropic context engineering, subagent summaries
- https://claude.com/resources/articles/subagents-in-claude-code — Anthropic guide: when to use subagents and when not
- https://code.claude.com/docs/en/sub-agents — Claude Code subagent frontmatter, model order, effort
- https://code.claude.com/docs/en/model-config — model aliases, effort levels and defaults
- https://code.claude.com/docs/en/costs — cost guidance, agent teams about 7×
- https://code.claude.com/docs/en/prompt-caching — subagents and the cache, forks
- https://code.claude.com/docs/en/agent-teams — agent teams versus subagents
- https://code.claude.com/docs/en/workflows — workflow cost note
- https://code.claude.com/docs/en/plugins/mods/reference — `agent.spawn` hook
- https://code.claude.com/docs/en/hooks — Agent tool input, `updatedInput`
- https://platform.claude.com/docs/en/build-with-claude/effort — effort levels and per-model advice
- https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompting-claude-opus-5 — over-delegation guidance
- https://claude.com/blog/claude-model-and-effort-level-in-claude-code — model versus effort blog
- https://www.anthropic.com/claude-opus-5-5 — Opus 5.5 pricing and effort claims
- https://github.com/anthropics/claude-code/issues/38698 — per-agent provider request (open)
- https://github.com/anthropics/claude-code/issues/74073 — multi-provider routing (closed stale)
- https://github.com/anthropics/claude-code/issues/43869 — subagent model routing bug report
- https://github.com/anthropics/claude-code/issues/27645 — subagent over-spawning report
- https://github.com/musistudio/claude-code-router — router with 37,879 stars (via `gh`)
- https://deepwiki.com/musistudio/claude-code-router/6.2-subagent-and-workflow-routing — CCR subagent tag mechanism
- https://developers.openai.com/codex/subagents — Codex subagents and custom agents
- https://developers.openai.com/codex/config-reference — Codex `agents.*` keys
- https://developers.openai.com/api/docs/guides/reasoning — OpenAI reasoning effort values
- https://cursor.com/docs/subagents — Cursor subagent model and effort syntax
- https://geminicli.com/docs/core/subagents/ — Gemini CLI subagents
- https://opencode.ai/docs/agents/ — OpenCode agents
- https://docs.factory.ai/cli/configuration/custom-droids — Factory Droid subagents, complexity routing
- https://ampcode.com/docs/models-and-subagents — Amp subagents inherit model
- https://ampcode.com/docs/the-dial — Amp modes
- https://ampcode.com/news/gpt-5.4-the-new-oracle — Amp oracle numbers
- https://ampcode.com/news/rush-2.0 — Amp rush cost and speed
- https://docs.github.com/en/copilot/reference/custom-agents-configuration — Copilot custom agent config
- https://docs.github.com/en/copilot/tutorials/optimize-ai-usage — Copilot model and reasoning guidance

Seen only as search excerpts (not opened in full):
- https://arxiv.org/html/2601.11327v2 — small agents versus large model, sub-agent thinking
- https://arxiv.org/html/2604.02460 — single versus multi-agent at equal thinking budgets
