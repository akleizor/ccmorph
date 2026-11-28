# Architecture entropy: progress and task pool

This file records the refactor rounds: what changed, how it is enforced, what
is still open, and what needs a maintainer's decision. Decisions with lasting
effect are recorded as ADRs in `docs/adr/`.

## Map (2026-11-29 baseline)

- **Entry:** `bin/ccmorph`, then `lib/ccmorph.sh`, then `ccmorph-core`.
  `cmd/ccmorph-core/main.go` holds the one subcommand switch.
- **Package graph:** acyclic. The layers are in
  [ADR 0001](adr/0001-leaf-packages-own-shared-rules.md).
- **Main source of entropy:** `util/` has 50 non-test files. Most of their
  exports have only `cli` as a caller: doctor, launch argv and flags,
  pickers, store, fetch, reach, relay, compinit. `util` imports
  `settingscompose`, `ctxwindow` and `profile`, so packages below it copy
  its helpers.
- **Existing guards:** `tests/go_helper_dedup_test.py` (banned copies),
  `tests/duplicate_definitions_test.py`, `tests/repo_layout_test.py`, and
  the env-list rule in `profile/envkeys.go`.

## Rounds

| # | Change | Guard | Status |
|---|---|---|---|
| 1 | #544: `util/modelid` leaf package. 30 hand-written marker strips and first-`[` cuts now call `modelid.TrimWindowMarker`. Tier env keys are built from `TierEnvKey`. | `test_the_window_marker_is_spelled_once_issue_544`, tier guard without exemptions | done (#1059) |
| 2 | The profile live-lock protocol had two copies, in `util/setup.go` and `settingscompose/compose.go`: path, bounded wait, inode recheck, open loop and errors. It is now one `util/paths/livelock.go`, and `util` and `settingscompose` call it. Review fix to round 1: the launch window probe cuts the marker where the engine does (`CutWindowMarker`); the adapter and doctor keep `TrimWindowMarker`, as the adapter does. | `paths` `TakeLive` tests; the dedup guard bans `flockWithin`, `lockWithin`, `sameLockInode` and `lockNamesFile`; zsh `test_the_window_probe_cuts_the_marker_where_the_engine_does` | done |
| 3 | Claude Code's own directory was joined on a raw `$HOME` at ten sites (relative to the cwd when HOME was unset, #138) and on `os.UserHomeDir()` with its error dropped at two more. `paths.ClaudeHome()` builds it on `paths.Home`; every site calls it, and the router's rc fallback and the cache migration use `paths.Home`. | dedup guard `test_claude_home_is_built_once` (mutation-checked) | done |

## Task pool (ranked by value and risk)

1. **Unbounded `ps`:** `util/gc.go` runs `ps -axo command` with no context
   and no `WaitDelay`. That is REVIEW.md kind 16. Route it through `psCmd`.
2. **Forwarding layers:** `util.GlobQuote` and `cli.globQuote` only forward
   to `paths.GlobQuote`. The hop wrappers (`ProcessTable`, `HopTarget`,
   `SessionAdapter`, `ProcStartToken`) only forward too. Several exports
   are used only inside `util` (`ModelsListing`, `ModelsAnswer`,
   `BridgeStop`, `ReadOwnedRegular`).
3. **cli verb lists:** the same verbs are listed in `cli/main.go:27`,
   `:41`, `:65`, the `Main` switch, the `cli/verbs.go` `Verb` switch and the
   help text. Replace them with one table.
4. **`cmd/ccmorph-core` usage string:** it omits `edge-proxy`, `tether`,
   `launch`, `cli`, `util` and `usage-compact`.
5. **Settings file names:** `settings.profile.json` is a constant in
   `settingscompose` but a literal in `cli/launch.go` and `util/store.go`.
   `cli/launch.go:562-571` decides on its own which settings layer wins.
6. **cli-only util files:** move into `cli` the files whose only caller is
   `cli`: `compinit.go`, `claudehelp.go`, `launchargv.go`,
   `launchflags.go`, `doctor.go`.

## Pending a maintainer's decision

- **Two `.conf` parsers:** `ctxwindow.ReadConf` is lenient. It trims, skips
  lines without `=`, and the last duplicate wins. `profile.readConf` is
  strict and rejects all three. One parser would change which confs load.
  Choose the strict or the lenient rule.
- **Process liveness on EPERM:** eight `kill(pid, 0)` checks treat EPERM as
  dead, and three treat it as alive. One helper needs one answer. The two
  groups protect different things: reaping files versus keeping an edge.
