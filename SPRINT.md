# Sprint: `make gate` wall — 2026-11-16 night into 11-17

Branch `sprint/suite-ccmorph`, worktree `suite-sprint/wt/ccmorph`, Mac mini
(18 cores). Baseline = `origin/main` b533b0d in a parallel worktree.

## Measurement conditions — read this before the numbers

The step-1 method is `rig run cpu -- /usr/bin/time -p make gate` with
`PYTEST_XDIST_AUTO_NUM_WORKERS` unset. The rig cpu lane admits a job only under
load 18. It never admitted one tonight: from 01:25 to past 05:40 HKT the 1-minute
load stayed between 80 and 383 — Symphony merge-gate waves plus four other sprint
sessions and their rig lanes, none of which stopped.

Two environment facts that shape every number below:

1. `make gate` on this machine goes through a PATH shim (`symphony/shims/make`)
   that re-routes a bare `make gate` into `rig run cpu --mac --wait 3600`. A
   measurement must therefore pin the binary: `/usr/bin/make gate`. (Inside a rig
   lane the shim is the real make, and the lane's offload environment races the
   install suite's relrepo clone — the first baseline run died on exactly that.
   Not a repo bug: a rig-lane artifact.)
2. At load 120+ the gate cannot be compared at all: GNU make exits when the first
   suite fails, and the python lane's four `doctor … issue-467` cases — pre-existing
   on main — lose their 1 s process-table bound under that much contention and
   fail. Measured walls used `/usr/bin/make -k gate` so all five lanes ran to the
   end on both sides.

Paired direct runs, alternating, one wave window (load 122–273), `-k`:

| run | side | wall (s) | load at start |
|---|---|---|---|
| 2 | branch | 43.21 | 273 |
| 3 | baseline | 38.55 | 237 |
| 4 | branch | 41.68 | 180 |
| 5 | baseline | 37.67 | 155 |
| 6 | branch | 43.08 | 135 |
| 7 | baseline | 43.66 | 123 |
| 8 | branch | 38.17 | 152 |
| 9 | baseline | 38.98 | 204 |

Branch best 38.17 s, baseline best 37.67 s. CPU totals are equally tied
(branch 220–254 s, baseline 224–230 s). Per-lane alternations say the same —
python 23.25/41.58 s branch vs 21.79/24.83 s baseline, launcher 21.63/21.39 s vs
20.70/24.72 s, with load swinging 130→357 between adjacent runs.

**Stated plainly: on this machine tonight, the full-gate wall of branch and
baseline is the same number within noise, and neither is under 30 s.** Both are
wait-bound on the co-tenant run queue, not on this repo's work. Every gate wall
above is a number about the wave, not about the suite. What the branch changed is
measurable only at the per-test and per-unit scale where the same-window method
holds — that table follows, every row from its own alternating pair.
<!-- TODO: final median — only if a load<30 window arrives before 07:40 -->

## Counts — nothing lost, from tonight's full `-k` logs on both sides

| lane | baseline b533b0d | branch |
|---|---|---|
| launcher | 328 unique `PASS:` test names, 0 FAIL | 328 unique `PASS:` test names, 0 FAIL |
| install | `install tests: 350 passed, 0 failed` | `install tests: 350 passed, 0 failed` |
| python | 1346 passed, 0 skipped, 110 subtests, 2 failed | 1346 passed, 0 skipped, 110 subtests, 2 failed |
| go | all packages ok | all packages ok |
| configure | `PASS cli_configure_test` | `PASS cli_configure_test` |

Static audit behind "same count, not just same run":

- launcher: the 328 `run_test` names diff byte-identical between the two checkouts.
- python: `def test_…` names identical in every python file the branch touches.
- install: unit list grew 19 → 21 because 62ec5bf splits one unit into three; the
  suite's own parent fails unless every unit ran exactly once, and 350 is the same
  check count before and after the split.
- The two python failures are pre-existing on main:
  `adapter_test.py::…test_print_config_redacts_upstream_proxy_credentials` (refused
  connect to the test's own proxy port — passes alone on both sides) and
  `rc_install_test.py::test_every_example_writes_the_profile_where_a_profile_goes`
  (the README's `--switch` lines predate the test; neither file is touched by the
  branch). The four `doctor_test.py::…issue-467` cases that also failed in some
  branch runs fail identically on baseline at the same load — they lose a 1 s
  process-table bound to co-tenant contention, on both sides, unchanged.
- No timeout literal was lowered except the two reasoned ones in the table below
  (adapter delay 2.2→1.4, ready window 3→2); a baseline-vs-branch diff of every
  sleep/timeout literal shows only the fixed-sleep-to-event replacements.

## Changes and what each is worth

| commit | change | saving |
|---|---|---|
| 3b5a075 | one pristine core, hard-linked into every launch home | 13 MB copy + 134–153 ms scan per copied-core test |
| 601ba9a | credential scan releases the gateway when it has looked | 5.82 s → 1.9 s on the test; ~50 `ps` spawns dropped |
| 6be6277 | poll the proxy for the fail-closed reload | fixed 3 s sleep → posts until 502; file 5.15 s for 3 tests |
| 48c02ca | one shared stub per text, linked into every launch home | ~1,000 first-exec scans (55–62 ms each) → one per distinct text per shard |
| 78d5cf1 | rebuild the curl stub through the store, never with mv | correctness: BSD mv prompts on 0555; byte-identical stubs |
| 5d561ab | build each stub link without a fork | 40% cheaper stub writes cold; shell forks were the fixture's top cost |
| 62ec5bf | split the update section into three units | suite tail 9.81 s → 5.12 s; 4-shard wall 10.04→9.81 / 11.58→11.34 (load ~90) |
| 9fae2ae | signal tests end when the test says so | ~1.4 s of suite wall; also closes a load-dependent flake |
| 31b3a3c | one tether-table sweep after adapter ready | CPU 2.92 s → 1.84 s per run |
| 19f8359 | adapter delay 2.2 s → 1.4 s, just past the core's 1 s mark | test wall 2.55 s → 1.72 s median |
| 32dea6a | take only four builtins from zsh/files | fix: `chmod +x` fixtures work again; suite-wide 328 green both sides |
| 6ea4f01 | one stub store for the machine, persistent, content-addressed | 18 assessments per stub text per run → 0 on warm runs (280–400 ms each under load) |
| ff00ea8 | same for the python store + content-digest core tag | kills the per-run cold store; `make shim`'s byte-identical rewrite no longer churns it |
| e0272cb | a cache name holding other bytes fails the stub call | correctness follow-up |
| a1eaa2b | ready-timeout window 3 s → 2 s | 1 s off the test whenever it lands on the tail |

## Still slow, and why

Tonight's durations profile (top 30 of 1348, warm store) plus the paired lanes
name exactly where the wall lives now:

- Python (~20–25 s at these loads, the gate's usual tail): ~40 tests wait on real
  time by design — the biggest five are ready-window/PTY/live-lock tests
  (8.3→~7, 6.0, 5.2, 5.2, 5.1 s). The 6.0 s PTY case is a deliberate detection
  margin (its comment says every second past the child-exit time is suite wall;
  the margin is what makes the buggy shape fail under load). The four `doctor
  issue-467` cases (2.7–5.2 s) pay the product's own 1 s-per-pid process-table
  deadline — that is doctor's shipped behaviour under test, not test slack. The
  live-lock pair waits for real cross-process lock release. None of these shrinks
  without deleting coverage or weakening what is asserted.
- Launcher (~21 s): wait-bound on the core's real 1 s adapter-ready tick across
  ~18 shards; the stub store removed the fork and scan cost (commit 5d561ab: 40%
  cheaper cold) but never the product's own second.
- Install (~10 s): the `remote` unit's `make dist` (four cross-platform Go
  builds) is the 12 s table entry; with `_jobs_default` 6 the greedy deal would
  floor at that same unit, so raising jobs buys nothing — measured 4-shard walls
  (9.81–11.34 s, 62ec5bf) already sit at the `remote` floor.
- The gate wall is `max(lanes) + shim build`; on a quiet machine the branch's
  per-test wins (−3.9 s, −3 s, −1.4 s, −1 s, −0.8 s, unit tail −4.7 s) should land
  it in the mid-20s; the 10 s best is not reachable inside this suite's design —
  it spawns hundreds of real processes and many waits are the shipped product's
  own timers. Cutting those is deleting coverage, which this sprint's rules forbid.

## What was NOT done, and why

- No clean quiet-machine median: the rig cpu lane (load <18) never admitted a job
  tonight and no sub-load-80 window appeared on the mini from 01:25 to 05:40.
  The three-run median under `rig run cpu` should be taken in the morning on a
  quiet machine; everything needed to run it is in the table above's command.
- <!-- TODO: M1 Pro supplement, filled when those pairs finish -->
