# Contributing to ccmorph

## Verification

`ccmorph-core` is Go: build it once with `make shim` (Go from `go.mod`) before
any suite, and run `go vet ./... && go test ./...` when you touch `shim/` or
`cmd/`. While iterating, run only the tests for what you touched
(`CCMORPH_TEST_FILTER=<name> zsh tests/ccmorph_test.zsh`,
`CCMORPH_LIB_DIR="$PWD/lib" python3 -m pytest tests/<file>.py -q`). Run the full set
once, before you commit or open the PR, never again for the same diff:
`make test` — the suites work in disjoint scratch homes and mostly wait on
process spawns, so it runs them all at once (minutes one after another, about a
minute at once; never chain the suites sequentially). One suite at a time:
`zsh tests/ccmorph_test.zsh` (about 40 s), `zsh tests/install_test.zsh`
(`CCMORPH_INSTALL_JOBS=1` for serial), `zsh tests/cli_configure_test.zsh`, or
`CCMORPH_LIB_DIR="$PWD/lib" uv run --with pytest --with pytest-xdist --with pytest-timeout python -m pytest tests/ -q`
(about 50 s). These are light and run bare.
Use isolated test homes; do not run the installer against the operator's home as
a smoke test.

## Rules

- Every patch ships with a regression test in `tests/` that fails before the
  fix; an untested change does not merge.
- Never run `install.sh` against a real home; tests use isolated homes.
- `source lib/ccmorph.sh && _ccm_launch -a <profile> <args>` is a stable entry
  point that other tools call. The `_ccm_launch -a` cases in
  `tests/ccmorph_test.zsh` guard it and must keep passing unchanged.

## Review

Reviewers read [REVIEW.md](REVIEW.md) first (what a real bug looks like here).

## Commits

One pull request lands as one squashed commit on `main`, so a rollback is one
revert. Conventional style: `feat(scope): …`,
`fix(scope): …`, `docs: …`, `test(scope): …`.
