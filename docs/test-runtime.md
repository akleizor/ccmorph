# Python verification runtime (measured 2026-10-20)

What this records: the measured profile of the Python verification suite — where its
wall time goes, which part of that is the suite's own work and which part is the host
underneath it, and what this change costs. Every figure below points at a log in the
issue workspace's `.hive/` directory. The run logs written on this head and on `main`
(`.hive/main-base/.hive/round6-main.log`, `.hive/round6-head.log`, the `.hive/land*-*.log`
four-suite runs, `.hive/hangcheck-*.log`) open and close with a BEGIN/END stamp naming the commit,
the dirty flag, the one-minute load and the free disk. The round-4 and round-5 four-suite
records are themselves run logs (`.hive/round4-summary.log`, `.hive/round5-summary.log`):
they stamp the load and free disk at both ends, but not the commit. Derived files stamp
nothing and say what they are where they are printed — the two round-6 summaries
(`.hive/round6-*-summary.log`) by `.hive/round6-summarize.py`, the launch-environment A/B
table by `.hive/proccost-summarize.py`, each from records taken inside the runs named
beside it, and the latency samples open with a `BEGIN latency samples` line naming the
interpreter they sampled. Nothing here rests on a run that cannot be reopened.

## How it was measured

Host: Mac17,16, 48 GiB RAM, 18 physical cores (6 performance, 12 efficiency).
`tests/conftest.py` selects pytest-xdist `auto` with `worksteal` — 10 workers here —
and `pytest.ini` caps any single test at 60 s. No worker count, timeout, test
selection or production setting was changed by this change or by these runs.

Two measurement-only instruments produced the records quoted here, and each figure below
is from the one named with it. Round 6 was measured by a pytest plugin,
`.hive/instrument.py` (loaded as `-p instrument`, writing to `GH294_INSTRUMENT_LOG`),
which counts the suite's process spawns from the CPython audit hook and takes CPU from
`RUSAGE_CHILDREN`; `.hive/round6-summarize.py` folds its one-record-per-process output
into the summaries quoted here. The launch-environment A/B was measured by
`.hive/proccost/sitecustomize.py`, on `PYTHONPATH`, whose processes each append one JSON
line at exit — interpreter, argv, self CPU, wall — folded by `.hive/proccost-summarize.py`.
Two of the numbers they report are contention-invariant and carry this profile: **how
many processes were spawned** and **how many CPU-seconds they burned**. Wall clock is
recorded, but is never used here as evidence of cost, because on this host it moves
with what else is running — shown next.

## Contention, measured apart from the work

- **Identical work twice** (`.hive/proccost-summary.log`): `tests/launch_env_rules_test.py`,
  51 tests and 432 Python processes each time, took 26.77 s and then 50.50 s of wall;
  self CPU was 21.4 s and then 22.5 s, and its 67 `-c` children summed 254.2 s and
  then 488.1 s of wall (5.2 s and 5.4 s of CPU). The median the table prints is each
  process's own CPU, and that moved by hundredths — the 311 plain starts 0.041 s then
  0.044 s, the `-c` children 0.041 s then 0.043 s. The median *wall* on the same records
  moved fourfold: 0.038 s then 0.159 s for the plain starts, 0.045 s then 0.178 s for the
  `-c` children (medians recomputed over `.hive/proccost-envrules.jsonl` and
  `.hive/proccost-envrules2.jsonl` with the upper-median rule the table uses). The work
  is the same to the process; the clock it was handed multiplied.
- **A fixed start probe** (`git`, `zsh`, `python`, one start each — `.hive/round6-main-latency.log`,
  `.hive/round6-head-latency.log`; the `python` sample is the system Xcode `python3` named
  in each file's `BEGIN` line, not the uv 3.14.8 the suite runs under): on `main` 0.015 /
  0.007 / 0.019 s at load 38.22 versus 0.242 / 0.327 / 0.261 s at load 128.24 — 16×, 47×
  and 14× — and on the head tree 0.024 / 0.018 / 0.052 s at load 169.03 versus 0.108 /
  0.073 / 0.229 s at load 257.65 — 4.5×, 4.1× and 4.4×. What this host charges for one
  process start moves by several times to an order of magnitude, and the suite starts
  thousands.
- **The same command on this host over a day**: 608.57 s (`.hive/round4-python.log`,
  load 158.29 → 199.00 across the Python suite itself), 663.28 s
  (`.hive/round5-python.log`, 197.64 → 372.66), 135.78 s (round 6 below, 169.03 →
  171.80): a 4.9× spread at one command on one machine. Those three runs did not carry
  the same suite — rounds 4 and 5 collected 890 and 899 cases on this head
  (`.hive/round4-cases.txt`, `.hive/round5-cases.txt`) against round 6's 916 — so on its
  own the spread bounds host variance only loosely; the identical work compared at
  unchanged CPU is the A/B above, and round 6 is the like-for-like pair. (The lower 68.22
  and 77.74 figures in the four-suite table below are those runs' starting loads, taken
  before the Python suite ran, not this suite's own.) Counting the six further runs of that
  command in the table below, the same suite on the same host spans 66.24 s to 663.28 s —
  a factor of ten.

## Round 6: `main` and this branch, same command, back to back

`CCPROTEUS_LIB_DIR=$PWD uv run --with pytest --with pytest-xdist --with pytest-timeout python -m pytest tests/ -q`,
first in a worktree of `origin/main` and then in this worktree, nothing else of mine
running between them. The branch side was profiled on `a105f8b`, the pre-rebase commit
carrying the code every later head of this branch still has:

| Tree | Commit | Cases | Result | Wall | Suite CPU | Spawns (git) | Load start → end |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `origin/main` | `77a66e2` | 903 | 903 passed, 166 subtests, rc=0 | 130.14 s | 182.7 s | 920 (102) | 38.22 → 169.03 |
| this branch | `a105f8b` | 916 | 916 passed, 166 subtests, rc=0 | 135.78 s | 190.0 s | 1010 (195) | 169.03 → 171.80 |

Logs: `.hive/main-base/.hive/round6-main.log`, `.hive/round6-head.log`; the derived
columns come from the raw per-process records
`.hive/main-base/.hive/round6-main-instrument.jsonl` and
`.hive/round6-head-instrument.jsonl` via `.hive/round6-summarize.py`, printed as
`.hive/round6-main-summary.log` and `.hive/round6-head-summary.log`. Both runs are
uv's Python 3.14.8, the interpreter the documented command selects.

Both trees are green, and the branch side is 5.6 s slower at the same command while carrying
13 more cases and starting 90 more processes. This is the pair the issue asks for: the
same work measured on the same base on the same host, which no earlier round had.

## Where the time goes

From `.hive/round6-head-summary.log`: 11 processes (1 controller, 10 workers). The
workers burned 187.3 s of CPU, 168.4 s of it inside the child processes their own
tests started, and the log puts the unstarved floor at that child work shared over the
workers — `ideal wall at 10 workers, unstarved = 16.8s of child work each` — against a
controller wall of 135.68 s. The workers each actually spent 112.4–126.7 s. The
suite is not short of cores, and it is not short of CPU: each worker spends most of
its wall waiting on the child processes its own tests start. The census is the workers'
Python-visible spawns — the summary prints the column as `spawned programs (Python-visible
spawns only)`, and the controller's own ten worker spawns are outside it — and of those
1010, `spawns outside any test = 0`: all of it belongs to a case.

The slowest 40 entries on both trees are the same shape — 39 `call` entries and one
`setup` (the installed-CLI fixture, 5.64 s here, 5.17 s on `main`) — and every one of
them is a real integration case: a PTY picker, model selection through the real CLI,
doctor, the routed-launch cases, adapter endpoints. Nothing in the tail is collection,
and nothing in it is a synthetic stand-in for a launch.

What the run starts (`.hive/round6-head-summary.log`): 266 `python`, 209 `ccproteus`,
195 `git`, 182 `zsh` sourcing `ccproteus.sh`, 83 `zsh`, 23 `model-window`, 22 `python3`,
11 `ps`, 7 `pgrep`, 3 `ccproteus-adapter`, 3 `claude`, 3 `publish`. Cost by file: the
five most expensive files hold 93.8 s of the 168.3 s attributed to a test,
`tests/launch_env_rules_test.py` alone 39.2 s over 51 tests and 52 spawns.

**The dominant controllable cost this profile names is the number of real launches
and the interpreter starts inside each of them** — about two hundred `ccproteus`
launches and 266 `python` starts per run, each launch a fresh interpreter reading a
232 kB `ccproteus.sh` and a settings compose. The repository already knows this and pins
it itself; the `PYTHON_STARTS_PER_LAUNCH` block is quoted from
`tests/adapter_route_defaults_test.py:38–45`:

```
# How many python3 interpreters one routed launch may start, counted through a
# PATH wrapper: 15 before the read-only diagnostics were folded into one (#332),
# 11 after. The interpreter start is what a launch spends nearly all of its wall
# time on (#321), so the number is pinned rather than timed: a clock reading moves
# with host load, a start count does not. A new interpreter in the launch path
# updates this number; it is the count of every start, not of a subset.
# #333 tightens this when it folds the window helpers.
PYTHON_STARTS_PER_LAUNCH = 11
```

That is the same argument this document makes, and #333 will tighten the count.
Cutting it changes how a launch behaves, so it is a product decision (#321, #333), not
maintenance — and this change does not touch it. The other large cost, subprocess
waiting inside real integration tests, is the product's end-to-end behaviour being
verified for real; making it cheaper would mean verifying less.

## What this change owns

| | `main` `77a66e2` | this branch, profiled at `a105f8b` |
| --- | --- | --- |
| Cases | 903 | 916 (+13, 0 removed) |
| Spawns | 920 | 1010 (+90) |
| `git` spawns | 102 | 195 (+93) |
| `tests/publish_test.py`, per-test CPU / spawns | 0.62 s / 5 | 2.83 s / 98 |
| Suite CPU | 182.7 s | 190.0 s (+7.3 s, +4%) |

Case lists: `.hive/land-cases.txt` (`.hive/collect-cases.zsh`, both sides collected
with the documented command at `77a66e2` and at `a105f8b`; the "removed" section is
empty). The per-file pair comes from the two instrument records above.

**This change removes no suite cost relative to `main`.** It is a correctness patch in
the dependency oracle, and against `main` it adds work rather than saving any: `main`
already carries the one-snapshot batching (`0808403`, #394 — `head_blob_types()` is one
cached `git ls-tree -r -z HEAD`, and `is_head_path` reads that snapshot). What this
branch adds to the lookup is one cached native `git cat-file -t HEAD:<path>` per
distinct non-ASCII candidate and a `False` answer, with no Git process at all, for a
candidate carrying a NUL byte. On a tree holding no non-ASCII name — this repository's
tree, which is what the suite runs against — the added path starts no process.

The 13 added cases are this issue's regression contract: 12 inherited-environment
cases and one native-parity case. Each builds a real Git repository, because the
inherited environment has to be present while the fixture's Git runs. Measured over
the round-6 records, that is 2.25 s of CPU and 93 of the file's 98 spawns; the file's
17 cases cost 2.83 s and 98 spawns in total, against `main`'s 4 cases at 0.62 s and 5.

## Coverage and verification

All existing tests and assertions remain. No case was removed, skipped, weakened,
trimmed, re-tiered, or replaced with mocked product behaviour (`.hive/land-cases.txt`:
nothing in `main` is missing here; the 13 additions are the only difference).

The public-export regression runs real Git against isolated repositories and asserts
ordinary files, symlinks, embedded tabs and newlines, absent and untracked files,
directories, absolute paths and parent traversal, over 12 inherited-environment cases:
a clean environment, a repository redirect, an index redirect, and signing, hooks and
excludes policies at global, system and command scope. The pre-existing
`test_head_path_lookups_are_one_git_call_not_one_per_literal` still runs on this head
and still pins the process budget.

`test_head_dependency_lookup_matches_native_git_issue_294` commits NFC, NFD and U+212B
names and asserts, for every candidate including the U+00C5 twin that cannot exist
beside U+212B, that the lookup's answer equals native Git's own
(`git cat-file -t HEAD:<candidate>`). It exists because Python's NFC applies singleton
canonical mappings and Git's rule does not: an earlier revision of this branch turned a
committed `tests/Å.py` (U+212B) into `tests/Å.py` (U+00C5) and reported the name absent —
the mismatch that case now guards. So does a wrapping spy, which keeps real execution
and asserts the process budget: one shared snapshot for ASCII names, one question per
distinct non-ASCII name, cached on repeat, and none at all when the tree holds no
non-ASCII name. An intermediate revision of this branch parametrized the fixture on
`core.precomposeunicode`; that axis is gone because the final lookup reads no such
setting and asks Git instead. `main` never had it.

Hermeticity is asserted, not assumed: the fixture pins `HOME` and `XDG_CONFIG_HOME`
into `tmp_path` (the default `$XDG_CONFIG_HOME/git/ignore` is not a Git config, and a
`*.py` rule there emptied the fixture's commit silently), pins `core.excludesfile` at
command scope, and asserts the committed path set, so an empty or pruned tree fails
loudly instead of passing vacuously. Native Git environment and configuration
selectors isolate all writes, and each case asserts, before any lookup runs, that the
fixture's own Git setup wrote nothing into a synthetic caller repository — the
comparison is byte-for-byte and stands at `tests/publish_test.py:249–251` and `:296–298`,
one line ahead of `isolate_subject`. All 17 cases in the touched file pass in the round-6 run
(`.hive/round6-head.log`).

## What this host does to a verification run

This suite is verified on a shared 18-core Mac mini that also runs other Hive sessions,
their suites, Xcode builds and iOS simulators. The full four suites in isolated homes,
run one after another on 2026-10-20:

| Round | Launcher | Installer | Configuration | Python | Load start → end |
| --- | --- | --- | --- | --- | --- |
| 4 (`.hive/round4-summary.log`) | rc=0, 971 s | rc=0, 142 s | rc=0, 90 s | 10 failed, 881 passed, 1 error, 608.57 s | 68.22 → 199.00 |
| 5 (`.hive/round5-summary.log`) | rc=0, 1139 s | rc=0, 121 s | rc=0, 96 s | 11 failed, 889 passed, 3 errors, 663.28 s | 77.74 → 372.66 |
| 7 (`.hive/land-four-suite.log`) | rc=0, 241 s | rc=0, 14 s | rc=0, 11 s | 916 passed, 166 subtests, rc=0, 208.98 s | 53.44 → 74.56 |
| 8 (`.hive/land7-four-suite.log`) | rc=0, 369 s | rc=0, 34 s | rc=0, 18 s | 916 passed, 166 subtests, rc=0, 150.30 s | 67.33 → 80.60 |
| 9 (`.hive/land8-four-suite.log`) | rc=0, 401 s | rc=0, 23 s | rc=0, 22 s | 916 passed, 166 subtests, rc=0, 242.77 s | 53.73 → 373.05 |
| 10 (`.hive/land9-four-suite.log`) | rc=0, 680 s | rc=0, 21 s | rc=0, 18 s | 916 passed, 166 subtests, rc=0, 170.12 s | 242.10 → 219.66 |
| 11 (`.hive/land10-four-suite.log`) | rc=0, 145 s | rc=0, 5 s | rc=0, 3 s | 916 passed, 166 subtests, rc=0, 66.24 s | 181.76 → 97.48 |
| 12 (`.hive/land11-four-suite.log`) | rc=0, 608 s | rc=0, 47 s | rc=0, 16 s | 916 passed, 166 subtests, rc=0, 193.13 s | 124.27 → 112.47 |

Rounds 7 to 12 are four-suite runs of this branch's heads — `4019e8a`, `c13df7a`,
`6ddd057`, `4e0d9d1`, `3fc947b` and `9f59f50` — with all four suites green: eight minutes
for the four, then ten, eleven, fifteen, four and fourteen, on trees whose code is
byte-identical to the `a105f8b` profiled above (`a105f8b` is the pre-rebase commit that
code was profiled on and is no longer in this branch's history; `git diff --stat a105f8b
<each head above>` is this document alone, and every commit after `defb3ea` on this branch
touches this document alone). Hosts move in both directions: the one-minute load average at
the start was 53 in round 7, 67 in round 8, 54 in round 9, 242 in round 10, 182 in round
11, 124 in round 12 and 169 in round 6, yet Python ran 208.98 s, 150.30 s, 242.77 s,
170.12 s, 66.24 s, 193.13 s and 135.78 s. Round 9 opened at a fifth of round 10's load and
ran 1.4× slower; the fastest of all, round 11, opened at over three times round 9's load.
The load average is a decaying window over everything on the machine, not a measurement of
what this suite's own processes wait on — which is why the profile above rests on
CPU-seconds, spawn counts and per-process medians, the quantities that did not move
between rounds on the same code. The launcher suite shows the same shape on a third of the
cases (308 against the Python suite's 916): 241 s in round 7, 680 s in round 10 and 608 s
in round 12 against 145 s in round 11, all four green on identical code, and it is the
suite whose cost is nearly all process launches.

The Python failures in rounds 4 and 5 are not all one class, and the sentence that stood
here before — that none of them was a failed assertion — was wrong. Most are the suite's
own timeout guards firing on a saturated host: `pytest-timeout`'s 60 s cap, and a 102.8 s
`subprocess.TimeoutExpired` inside a picker launch (`.hive/round5-python.log:722`). Those
rounds also recorded a value assertion inside the deadline case at
`tests/refresh_probe_test.py:169` (`AssertionError: {} != {'gw:a': 123000}`,
`.hive/round4-python.log:155`) — the elapsed-time check three lines above it
(`assertLess(..., 0.5)`, `:166`) passed, since execution reached `:169`, so that failure
is a returned-value mismatch rather than a clock reading — plus a `shutil` teardown
`OSError` on a tmp tree (`.hive/round4-python.log:60`) and a picker teardown
`PermissionError` on `os.killpg` (`.hive/round5-python.log:785`). None of the failing
cases is in a file this change touches. What the reruns establish, and all they
establish: round 5's set rerun alone on this head passed 10 cases of the 11 that had
failed, in 217.34 s at a starting load of 356.71 (`.hive/round5-attribution.log` — the log
names the count, not the cases); the same set on unmodified `origin/main` gave 1 failed,
9 passed in 233.59 s (`.hive/round5-main-baseline.log`), so unmodified `main` is not green
on them either; and round 4's rerun of 10 cases — a list that repeats
`test_set_model_persists_and_run_uses_it_without_a_flag` once and omits the deadline case
(`.hive/round4-attribution.log:1`) — ended 5 failed, 5 passed in 566.98 s, with its
101.95 load reading printed after the run rather than at its start. That is a host effect
on most of the failures and a baseline timeout on `main`; it is not a per-case
attribution of every one of them.
The launcher failures recorded in round 3 were #365,
fixed on `main` (`df5efdb`, "spend the pty deadline in idle-machine seconds"), and rounds
4 and 5 ran that suite green.

The merge rule also asks whether a capped case is this PR's. The one case that hit the
60 s cap in a round-6 pre-rebase run,
`tests/adapter_test.py::AdapterTest::test_a_listener_on_another_address_does_not_block_the_endpoint`,
was rerun alone on both trees at the same host reading: `1 passed in 10.03s` on this
head, `1 passed in 12.51s` on `origin/main` (`.hive/hangcheck-head.log`,
`.hive/hangcheck-main.log`, load ~90 both) — the host's, not the diff's.

**Done-when 2, stated plainly: this head is not consistently under one minute on this
host.** Round 6 measured 135.78 s while the machine carried a one-minute load of 169–172,
and the same command run two minutes earlier on the same machine, on `main` at load 38 →
169, measured 130.14 s. The full four suites ran green on `4019e8a`, again on `c13df7a`,
`6ddd057`, `4e0d9d1`, `3fc947b` and `9f59f50`, with Python at 208.98 s, 150.30 s,
242.77 s, 170.12 s, 66.24 s and 193.13 s.
The support for calling that contention rather than suite cost is what this document
measures: the identical-work A/B at unchanged CPU and multiplied wall, the fixed start
probe moving 4.1–4.5× on the head's own samples and 14–47× on `main`'s, and the same
command spanning 663.28 s to 66.24 s on this host — rounds 4 and 5 at the wide end,
carrying 890 and 899 cases rather than round 6's 916, and round 11 at the narrow end on
round 10's own code, so the pair that isolates host from suite is round 6 itself. The
suite's own work is not nothing — 168.4 s of child work over 10 workers is a 16.8 s floor
if the workers never waited on anything but their own children, and the whole process tree
burned 190.0 s of CPU — but no quiescent-host run of this head exists, no run has come in
under a minute (the closest, round 11's 66.24 s, is still above it), and no sub-minute
claim is made here. Criterion 2 is therefore met in the form the issue allows, a
hardware/host contention explanation supported by measurements, and not as a sub-minute
number; the earlier sub-minute figure in this file's history was measured on a different,
smaller suite and is not evidence for this head.

## Scope

No live installation, credential, route or launch entry point was changed or exercised.
All runs used the suites' own isolated synthetic homes and endpoints. Linux and a
quiescent-host whole-suite timing remain unverified. The measurement scripts, the raw
per-process records and every log named above are kept in the issue workspace's
`.hive/` directory.