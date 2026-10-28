---
name: model-router
description: Hand a task to a subagent on the best profile, model and effort without choosing them yourself. Use when you are about to delegate work to a background worker or subagent, or when the user asks which model or account should run something. The router reads live 5-hour, weekly and budget quota, spends subscription quota before paid API, and picks the cheapest model and effort that can do the task.
---

# model-router

You decide whether to delegate. The router decides where it runs.

## Decide whether to delegate

Delegate when the work:
- reads a lot (10+ files, logs, search results) and the answer fits in 1–2k tokens;
- splits into 3+ independent pieces that can run in parallel;
- needs a fresh, unbiased context (a review, a second opinion);
- has a cheap check for the result (tests, a diff, cited lines).

Stay inline when the work is short, edits files you are also editing, or depends on decisions in this conversation that you cannot write into a short brief. Never send two writers to the same files.

## Delegate

- **On this account (most work):** use the Agent tool as usual and leave `model` and `effort` unset unless the user named one. The plugin's hook reads the prompt and fills in both; the tool result says what it chose.
- **On any account (long background work, or when this session's quota is tight):**
  1. Write the brief to a file: goal, files and sources, boundaries, what to return and where.
  2. Run it in the background:

     ```sh
     model-router dispatch --prompt-file brief.md --cwd <repo> --timeout 1800 -- --permission-mode acceptEdits
     ```

     The first stderr line names the pick, for example `gateway · gpt-6.1-sol · effort high`. Do not override it.
  3. To run one of the user's agent definitions there: `--agent <name>`.

Only pin what the user pinned: `--profile`, `--model`, `--effort`, `--mode quality|balanced|cost`. `model-router route --explain "<task>"` previews a choice without running anything.

## After the result

- Verify it cheaply (tests, diff, cited lines), not by redoing the work.
- For a dispatched job, record the outcome so the router learns: `model-router feedback <job-id> pass|fail` (the job id is in the first stderr line).
- If it failed although it had the context, rerun once with `--mode quality`. If it skipped steps, rerun once pinned one effort higher. After that, do it inline.
