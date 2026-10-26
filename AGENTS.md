# ccproteus

Before changing launch, installation, packaging, migration, or recovery behavior,
read [Scope and ownership](ARCHITECTURE.md#scope-and-ownership). That section is
the project-wide authority for integration boundaries.

Verification: while iterating, run only the tests for what you touched
(`CCPROTEUS_TEST_FILTER=<name> zsh tests/ccproteus_test.zsh`,
`CCPROTEUS_LIB_DIR="$PWD" python3 -m pytest tests/<file>.py -q`). Run the full set
once, before you commit or open the PR, never again for the same diff:
`zsh tests/ccproteus_test.zsh` (shards across the cores that help; about 40 s),
`zsh tests/install_test.zsh`, `zsh tests/cli_configure_test.zsh`, and
`CCPROTEUS_LIB_DIR="$PWD" uv run --with pytest --with pytest-xdist --with pytest-timeout python -m pytest tests/ -q`
(parallel whenever xdist is present; about 50 s). These are light and run bare, not under `rig run build`.
Use isolated test homes; do not run the installer against the operator's home as
a smoke test.

Review: reviewers read [REVIEW.md](REVIEW.md) first (what a real bug looks like here).

## Hive

Intent: [INTENT.md](INTENT.md) is the Alex-approved v0 starting point; changes require a PR he approves.

Repo-specific part of `~/.agents/AGENTS.md` Task Queue.

**Security: nightly.** ccproteus holds every profile's credentials and routes
every Claude Code session on this Mac.

**Merge rule (Alex, 2026-10-17): the Hive SOP's risk classes.** Every PR gets
a fresh reviewer and the four Verification suites above in isolated test
homes on its exact head; only failures the PR introduced count (a failing test
is checked on `origin/main` and rerun alone). Behaviour changes merge after a
clean review like any other. High risk waits for Alex's ✅ on the exact head:
auth or credentials, which backend or provider a profile reaches, permissions,
the `_ccs_launch -a` entry point, and changes to this rule.

**ccproteus focus:** maintenance and patching, not new features. Every patch
ships with regression tests in `tests/` that fail before the fix, and merges
only when the four Verification suites above show no failure it introduced;
an untested change does not merge. A bug fix that restores intended behaviour
is maintenance. A new behaviour at launch, in the model picker, or in which
backend a profile reaches is a product decision (`proposed`). The live install is `~/.config/ccproteus`: never run, edit or
fast-forward it, and never run the installer against a real home; merged
work reaches it when a session there next integrates `origin/main`.

**External contract:** `~/.agents` agent-dispatch, which launches every Hive
and spawned job, runs `source ccproteus.sh && _ccs_launch -a <profile> <args>`.
Renaming or changing that entry point is a one-way door; the `_ccs_launch -a`
cases in `tests/ccproteus_test.zsh` guard it and must keep passing unchanged.

**In the Hive microVM** (Ubuntu): `sudo apt-get install -y zsh` once, and run
the Python suite as `CCPROTEUS_LIB_DIR="$PWD" uv run --with pytest --with pytest-xdist --with pytest-timeout python -m
pytest tests/ -q`. The VM proves Linux; a change to a macOS-only path (BSD
tools, Keychain, launchd) says so under Merge danger.
- Vault: research, decisions and reports go to the Obsidian vault `20 Projects/ccproteus/` as `<Subject> — <Kind> — YYYY-MM-DD.md` (rules: llm-wiki skill); progress stays in Discord.
