# ADR 0001: A rule many packages compare lives in a leaf package

Status: accepted (2026-11-29)

## Context

The Go packages form one acyclic graph (Go refuses a cycle):

```
leaves:  netroute  ordjson  util/paths  util/modelid  util/modelsdev
         util/argparse -> ordjson
         profile       -> ordjson, util/modelid
         ctxwindow     -> profile, util/*
         settingscompose -> profile, util/*
         shim          -> ctxwindow, netroute
         quirks        -> shim
         quota         -> shim, ctxwindow
         agentproxy    -> netroute, util
         util          -> ctxwindow, profile, settingscompose
         router        -> agentproxy, quota
         adapter       -> shim, quirks, util
         cli           -> all of the above
         cmd/ccmorph-core -> cli and the verb owners
```

`util` sits high in that graph, so a helper defined there cannot be reached
from `profile`, `ctxwindow`, `settingscompose` or `shim`. Each time a rule
was needed below `util`, it was written out again. #423 moved five
predicates to `ordjson`; #422 moved the file predicates to `util/paths`;
#544 found the model-id window marker stripped by hand at 30 sites (15
case-sensitive, 8 cutting at the first `[`), next to the one helper in
`util/tier.go` that only the packages above `util` could call.

## Decision

A rule that more than one package compares has its one definition in a leaf
package, which imports only the standard library or other leaves. Its callers
name the leaf; `util` does not re-export it.

- `util/paths`: file system layout, path predicates, atomic writes, locks.
- `util/modelid`: model-id spellings: the tier words, the `[1m]` window
  marker (`WindowMarker`, `TrimWindowMarker`, `HasWindowMarker`), and the
  tier env key (`TierEnvKey`). These are two marker rules with different
  meanings, so they have two names. `TrimWindowMarker` strips a trailing
  marker; every routing and lookup surface uses it.
  `CutWindowMarker` cuts at the first marker found anywhere in the id, the
  way Claude Code's engine reads it; only the engine mirrors use it: the
  reserve, the window-registry walk and the route ceiling.
- `ordjson`: ordered JSON and the scalar predicates (#423).
- `netroute`: proxy and CA routing for outbound HTTP.

A new leaf needs a rule with callers in at least two packages that cannot
reach each other's copy. It must take those callers in the same change.

## Enforcement

`tests/go_helper_dedup_test.py` fails when a banned copy comes back:
`test_the_tier_words_are_spelled_once` and
`test_the_window_marker_is_spelled_once_issue_544`. These two tests cover
`modelid`. The older guards in the same file cover `paths` and `ordjson`.

## Consequences

- `profile/envkeys.go` and `ctxwindow/conf.go` build their tier env keys
  from `modelid.TierEnvKey`. The guard no longer exempts them.
- A bracket-bearing id (`vendor[beta]`) passes whole on every surface. A
  `[1M]` id sizes, maps and routes the same as its `[1m]` spelling.
- For `vendor[beta]` only, the shared adapter key now includes the brackets.
  The old adapter becomes idle and a new one starts. No stored data uses
  that key.
