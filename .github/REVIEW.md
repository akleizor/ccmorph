# Reviewing ccmorph

ccmorph holds every profile's credentials and routes every Claude Code session
on this Mac, so a bug here is a session reaching the wrong backend, leaking a
credential, or losing the user's choice.

## Output

- Open the review with one count line, exactly like `N Important, M notes`.
- Every finding cites `file:line` on the diff's head plus the input that breaks it; no `file:line` → not a finding.
- Only Important blocks: a real bug, data loss, credential or security exposure, or one of the kinds below.

## What failure looks like here

- **Backend, identity or credential env leaking into a launch or `claude auth` call** — a provider, identity or token name left set — `example: 4b80ca8 fix(launch): clear more provider, identity and credential env names`; also `e95b598 fix(auth): scrub CLAUDE_SECURESTORAGE_CONFIG_DIR around claude auth calls`.
- **A nested launch inheriting the parent session's state** — `CLAUDECODE`, `CLAUDE_CODE_SIMPLE` or another child-session var — `example: c3ebf44 fix(launch): a nested launch does not inherit claude --bare's CLAUDE_CODE_SIMPLE`.
- **One reader skipping the merged view** — a `$ccmorphUnset` tombstone that counts in one settings path and not another — `example: 189b398 fix(settings): every layer reader honours $ccmorphUnset`; also `9bce711 fix(settings): ignore a shared policy's tombstones in the readers too`.
- **The user's choice silently overruled** — a picked model or saved setting ignored, or an older model substituted — `example: 1e7a17c fix(launch): stop the role default overwriting a picked model`; also `9963261 fix(shim): never fall back to an older model`.
- **Signals or exit status lost between launcher and claude** — TERM/HUP not forwarded, the tty or the exit code dropped — `example: 63ba318 fix(launch): forward TERM and HUP from the launcher to claude`.
- **A shim rewrite corrupting the forwarded body** — UTF-8/CJK, surrogates, signed thinking, opaque tool input — `example: 5e9d09e fix(shim): keep CJK as UTF-8 when stripping metadata and repairing references`; also `52d918f fix(gateway): keep signed thinking and opaque tool input in sanitation`.
- **A loopback hop or catalogue read with no bound** — ambient proxy, cross-origin redirect, no deadline, unbounded body — `example: bcd9d26 fix(adapter,shim): loopback hops bypass ambient http_proxy`; also `b1dc252 fix(shim): end catalogue reads at their deadline`.
- **A runtime floor or upstream CLI contract assumed** — a Python 3.9+ API on the 3.8 floor, a flag Claude Code does not accept — `example: e14eecb fix: drop 3.9-only str.removeprefix/removesuffix from shipped helpers`; also `d50fe3c revert(rc): remote-control rejects --effort, so effort= took every bridge down`.
- **A redaction helper whose error path returns the secret it was meant to hide** — a parse failure that falls back to the raw value, so a proxy password holding a `^`, a bare `%` or a space is printed — `example: #47 fix(adapter): --print-config redacts a proxy URL Go cannot parse`.
- **A deadline only some documented paths arm** — a bound armed for `--model` but not `--auto`, though the launcher always passes `--auto` and adds `--model` only for a picked model — `example: #47 fix(adapter): --auto without --model is bounded by the select deadline`.
- **An observer still looking for the child process after the component moved in-process** — doctor kept scanning the snapshot for a `shim` child after the shim became a server inside the adapter process, so a healthy session read as a fault and the port its base_url pointed at was never checked; the ps fixture kept faking the old child shape, so the suite could not see it — `example: #49 fix(doctor): running-adapter reports the adapter's own in-process shim listener (in-process since #40)`.
- **A test that cannot fail** — inherits the suite's env, scratch home gone, asserts nothing ran — `example: 438e66f test(launch): make the fork_forward stderr test able to fail`.
- **A new writer reusing a field whose contract says otherwise** — the writer's own section is updated, but the field's other contract (the section its readers obey) is not, so the two disagree and readers mislabel the new values — `example: e84e42d feat(shim): price receipts for upstreams that bill outside the response (#34)`.
- **A ported normalizer whose search string equals its replacement** — a `\u003e` decoded to `>` on both sides of a `strings.NewReplacer`, so the escape it names is never replaced and the comparison that needs it silently fails — `example: e2b3c4d feat(core): Go ports of settings-compose, model-window, context-probe (#36 step 2)`.
- **A deadline only cooperative checks enforce** — the context is tested between steps while a blocking call (a `Flock`, a subprocess, a context-free read) waits past it — `example: e2b3c4d feat(core): Go ports of settings-compose, model-window, context-probe (#36 step 2)`.
- **Timer semantics swapped in a port** — urllib's timeout bounded each socket operation, and a single `context.WithTimeout` now bounds the whole exchange, so a slow but flowing transfer fails — `example: e2b3c4d feat(core): Go ports of settings-compose, model-window, context-probe (#36 step 2)`.
- **A path check that forgets a link can sit on a parent directory** — `-e`/`-ef` are true through a carried sidecar dir while the path itself is not a link, so a removal mistakes its own files for native collisions — `example: 93f3db8 fix(profiles): a carried sidecar dir no longer blocks rm`.
- **An upgrade deleting a file a running process still references** — a retired helper removed while a live session's route spec still names it, so the next `/model` reads as a gateway failure — `example: #52, b619556 removed libexec/ccmorph-adapter on upgrade under live sessions`.
- **A reader that turns unparseable input into a confident verdict** — a model listing whose `data` is not a list read as "not served" so the carried model is replaced, an unparsed endpoint echoed whole with its userinfo — `example: 7487a99 fix(util): model-known answers cannot-tell for a listing whose data is not a list`; also `a59f601 fix(util): redact-url never echoes an endpoint's userinfo`.
- **A diagnostics path that can die instead of degrade** — a raise outside `pycompat.Catch` takes every launch warning with it, since `util.Main` has no recover and the zsh caller drops stderr — `example: 824a001 fix(util): launch diagnostics keep the warnings when the merge raises`.
- **A damage guard not extended to the sibling field the same change added** — the guard counting a damaged cost unpriced sat on `usage.cost` alone, so the same damage in `usage.estimated_cost`, the field that change introduced, took the whole export down where `cost` was survived — `example: #68 fix(export): a damaged estimate is unpriced, never fatal`.

## Never blocks

Style, naming, wording, formatting, and "could be cleaner" go in the PR as notes,
at most 3 per review; drop the rest. A pre-existing bug the diff did not
introduce is a new issue, not a finding.

## Where the rules live

Scope, high-risk classes and the `_ccm_launch -a` contract are in
[AGENTS.md](../AGENTS.md); test homes and the Verification suites are in
[CONTRIBUTING.md](CONTRIBUTING.md); the integration boundaries are in
[ARCHITECTURE.md](../docs/ARCHITECTURE.md#scope-and-ownership). Check the diff
against them, do not restate them.