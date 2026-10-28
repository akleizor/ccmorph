# model-router

Picks the profile, model and effort for every subagent task: idle subscription quota first, then the cheapest model that can do the job.

```
$ model-router route --explain "Fix the off-by-one bug in the pagination helper in src/api/list.ts and add a unit test"
→ work · claude-haiku-5-5 · effort high
  quality 0.96 (target 0.95) · est $0.03 API-equivalent · quota price 0.05 · class subscription
  task: code_edit 100% · easy (1.03) · via decider

  profile    model                effort  class        qual  est $    quota eff $
  work       claude-haiku-5-5     high    subscription 0.96  $0.03    0.05  $0.002
  work       claude-haiku-5-5     xhigh   subscription 0.97  $0.04    0.05  $0.003
  work       claude-haiku-5-5     max     subscription 0.98  $0.07    0.05  $0.005
  codex      gpt-6.1-sol          low     subscription 0.98  $0.05    0.11  $0.006
  work       claude-sonnet-5-5    low     subscription 0.95  $0.09    0.05  $0.006
```

A real run over four profiles (two subscriptions, a monthly budget, a gateway with 30 models): an easy edit goes to Haiku 5.5 on the emptier subscription.

- **Quota-aware.** Reads live 5-hour, weekly and monthly windows. Quota that would expire unused is free; a window the human is burning is kept for them.
- **Task-aware.** A free local classifier reads the kind of work and its difficulty; each (model, effort) gets a success probability from the Artificial Analysis index and per-model strengths (Grok for X search, Gemini for video, and so on).
- **Effort per task.** Uses each model's own effort ladder (`low/high/max` for Kimi, GLM and DeepSeek; five levels for Claude and OpenAI) and picks the rung the task needs.
- **The main agent only decides whether to delegate.** Where it runs is arithmetic, not a prompt.

How it decides, with formulas and evidence: [docs/ALGORITHM.md](docs/ALGORITHM.md).

## Install

A ccmorph add-on and a Claude Code plugin: nothing changes until you install it. Needs Python 3.10+ and no packages.

In Claude Code:

```
/plugin install model-router --marketplace akleizor/ccmorph
```

That adds the hook and a skill of about 100 tokens a session, which tells the main agent when delegation pays and to call `model-router dispatch` instead of choosing a model itself. The `model-router` command itself:

```sh
uv tool install "git+https://github.com/akleizor/ccmorph#subdirectory=addons/router"
model-router init        # finds your ccmorph profiles and writes ~/.config/model-router/config.json
model-router profiles    # which profiles take part
model-router quota       # live windows and their quota price
```

Without ccmorph, list your profiles by hand under `profiles` in that file (see below).

## Two ways in

| Path | Runs on | Main agent's part |
|---|---|---|
| The Agent tool, through the plugin's hook | This session's account, any of its model aliases | None: the hook fills in `model` and `effort` |
| `model-router dispatch` | Any configured profile | One command instead of the Agent tool |

The hook (`hooks/hooks.json`) runs before every native Agent call, reads the prompt, routes it over this session's aliases (`opus`, `sonnet`, `haiku`, `fable`, mapped through `ANTHROPIC_DEFAULT_*_MODEL` when a profile remaps them) and this profile's quota, and rewrites the call's `model` and `effort`. A call that names both, a fork, a call naming a model the router cannot rate, and any call on a profile you disabled go through untouched; any error lets the call go ahead unchanged. A decider that does not answer within 5 seconds gives way to the keyword rules. Verified live: a general-purpose subagent asked to multiply two numbers ran on `haiku` (DeepSeek V4.1 Flash on that gateway) at `low` effort.

## Use

```sh
model-router route --explain "<task>"                  # the pick, the shortlist and every dropped option
model-router dispatch --prompt-file brief.md --cwd repo -- --permission-mode acceptEdits
model-router dispatch --agent reviewer "Review the diff on this branch"   # your agent definition, routed
model-router feedback mr-20261028-020159-ab12cd pass   # teach it from the outcome
model-router simulate                                  # replay a synthetic week against other policies
```

`dispatch` prints the pick on stderr first (`model-router mr-…: home · claude-sonnet-5-5 · effort high …`), records the job in a ledger so parallel jobs see each other's spend, and runs it through the executor. Arguments after `--` go to the agent CLI untouched.

Pin only what you must: `--profile`, `--model`, `--effort`, `--kind`, `--difficulty 0-4`, `--needs vision,web,x`, `--mode quality|balanced|cost`.

A task that needs posts from X goes only to a model with native X search (Grok's `x_search`) when one is reachable; without one, the pick says it has only web search.

## Choose who takes part

```sh
model-router disable work      # the human's main account stays out of routing
model-router enable gateway
```

Settings live in `~/.config/model-router/config.json`:

| Key | Default | Meaning |
|---|---|---|
| `priority` | subscription, budget, api | Which class is spent first |
| `mode` | balanced | quality, balanced or cost |
| `reserve` | 5h 15%, 7d 10%, month 5% | Share of each window kept for the human |
| `protect_above` | 0.5 | A subscription likelier than this to run out is a last resort |
| `max_job_usd` | 15 | No single job estimated above this |
| `decider.url` | local pplx-decider | Typed decision endpoint; `null` for keyword rules |
| `executor` | agent-dispatch | Or `ccmorph` (`ccmorph run <profile> --model … --effort … -p`) |

Per profile: `enabled`, `class` (`subscription`, `budget`, `api`), `quota`, `models`, `disabled_models`, `plan` (`pro`, `max_5x`, `max_20x`, `chatgpt_plus`, `chatgpt_pro`), `capacity_usd` (API-equivalent dollars one window holds, for example `{"5h": 60, "7d": 500}`; overrides `plan`), `monthly_budget_usd`, `reserve`, `price_factor`. A `statusline` profile with neither `plan` nor `capacity_usd` is sized as Pro, the cautious guess.

## Quota sources

| `quota` | Reads | Status |
|---|---|---|
| `statusline` (default for Claude subscriptions) | The `rate_limits` block Claude Code passes to its status line | Documented |
| `anthropic` | The OAuth usage endpoint, with the profile's own sign-in | Undocumented; opt in |
| `codex` | ChatGPT's usage endpoint, with the profile's own Codex sign-in | Undocumented |
| budget profiles | `monthly_budget_usd` minus the ledger's spend this month | Local |

To feed `statusline`, keep your status line and copy its input to the recorder:

```sh
#!/bin/sh
# ~/.claude/statusline.sh — set as statusLine.command in Claude Code settings
input=$(cat)
printf '%s' "$input" | model-router record     # profile = the CLAUDE_CONFIG_DIR name
printf '%s' "$input" | your-old-statusline     # if you had one
```

An OAuth or Codex reading is cached for five minutes; a statusline reading stands until the next one. A window whose reset time has passed counts as empty.

## Compliance

model-router never proxies or stores credentials. Each job runs the vendor's own CLI under the profile it was routed to (through agent-dispatch or ccmorph), which is ordinary use of that account. The two usage endpoints read only the same account's own usage, with the sign-in that CLI already keeps, and only when you set `quota` to them.

## Limits

- How many dollars of work a subscription window holds comes from public plan figures; set `capacity_usd` when you know better.
- Success probabilities start from public benchmarks and improve only with `feedback`.
- The task is read from its prompt alone, which cannot always tell a typo from a refactor; pin `--difficulty` when you know.
- Grok counts as able to search X only if its route passes the `x_search` tool through; a gateway may not.

## Development

```sh
uv run --with pytest pytest -q       # 39 tests, about 2 seconds
uv run python bench/decider.py       # decider against 30 labelled tasks; refits its scale
```

MIT licence.
