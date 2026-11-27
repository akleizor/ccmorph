# Contributing to ccmorph

## Verification

`ccmorph-core` is Go: build it once with `make shim` (Go from `go.mod`) before
any suite (on macOS, set `CCMORPH_CODESIGN_IDENTITY` to a codesigning identity
so privacy grants survive rebuilds), and run `go vet ./... && go test ./...` when you touch `shim/` or
`cmd/`. While iterating, run only the tests for what you touched
(`CCMORPH_TEST_FILTER=<name> zsh tests/ccmorph_test.zsh`,
`CCMORPH_LIB_DIR="$PWD/lib" uv run python -m pytest tests/<file>.py -q`). Run the full set
once, before you commit or open the PR, never again for the same diff:
`make test`. It runs every suite at once — they work in disjoint scratch homes
and mostly wait on process spawns, so one after another is about three minutes
while at once it is about a minute on a quiet host, and one minute is the
ceiling. Never chain the suites sequentially. One suite at a time:
`zsh tests/ccmorph_test.zsh` (shards across the cores that help; about 40 s),
`zsh tests/install_test.zsh` (shards 4 ways; `CCMORPH_INSTALL_JOBS=1` for serial),
`zsh tests/cli_configure_test.zsh`, or
`CCMORPH_LIB_DIR="$PWD/lib" uv run python -m pytest tests/ -q`
(parallel by default: the pytest packages ride in pyproject.toml's default test
group, and conftest.py turns xdist on once it is installed; about 35 s). These
are light and run bare.
Use isolated test homes; do not run the installer against the operator's home as
a smoke test.

### Harness-only seams

A launch test's gateway is a stub `curl` the test installs, so its answer is a
child process. A gate at its load can hold that child past the launch's
five-second reach probe; the probe then honestly names the gateway silent and
the launch ends before the behaviour the test asserts — the class behind #875,
#935, #947, #997 and #1026. `CCMORPH_TEST_REACH_WAIT=<seconds>` is the suite's
backstop for it: it moves only the probe's own stop, far past any healthy start,
and a launch that does not set it is untouched. The promise to people does not
move — `CCMORPH_REACH_BOUND` still cannot advertise or wait longer than five
seconds, and a stub that truly never answers is still named silent. Only
`tests/ccmorph_test.zsh` sets `CCMORPH_TEST_REACH_WAIT`; never a profile's
`env.` lines, a user's shell, or another harness that can answer the probe
without a child.

## Rules

- Every patch ships with a regression test in `tests/` that fails before the
  fix; an untested change does not merge.
- Never run `install.sh` against a real home; tests use isolated homes.
- `source lib/ccmorph.sh && _ccm_launch -a <profile> <args>` is a stable entry
  point that other tools call. The `_ccm_launch -a` cases in
  `tests/ccmorph_test.zsh` guard it and must keep passing unchanged.

## Review

Reviewers read [REVIEW.md](../REVIEW.md) first (what a real bug looks like here).

## Commits

One pull request lands as one squashed commit on `main`, so a rollback is one
revert. Conventional style: `feat(scope): …`,
`fix(scope): …`, `docs: …`, `test(scope): …`.
