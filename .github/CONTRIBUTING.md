# Contributing to ccmorph

## Verification

While iterating, run only the tests for what you touched
(`CCMORPH_TEST_FILTER=<name> zsh tests/ccmorph_test.zsh`,
`CCMORPH_LIB_DIR="$PWD/lib" python3 -m pytest tests/<file>.py -q`). Run the full set
once, before you commit or open the PR, never again for the same diff:
`zsh tests/ccmorph_test.zsh` (shards across the cores that help; about 40 s),
`zsh tests/install_test.zsh`, `zsh tests/cli_configure_test.zsh`, and
`CCMORPH_LIB_DIR="$PWD/lib" uv run --with pytest --with pytest-xdist --with pytest-timeout python -m pytest tests/ -q`
(parallel whenever xdist is present; about 50 s). These are light and run bare, not under `rig run build`.
Use isolated test homes; do not run the installer against the operator's home as
a smoke test.

## Review

Reviewers read [REVIEW.md](REVIEW.md) first (what a real bug looks like here).

## Commits

One logical change per commit, conventional style: `feat(scope): …`,
`fix(scope): …`, `docs: …`, `test(scope): …`.
