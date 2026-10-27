# ccmorph

Before changing launch, installation, packaging, migration, or recovery behavior,
read [Scope and ownership](docs/ARCHITECTURE.md#scope-and-ownership). That section is
the project-wide authority for integration boundaries.

Verification (the four suites and the per-file commands), isolated test homes,
commit style and how a review is read: [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md).

## Hive

Intent: [INTENT.md](docs/INTENT.md) is the Alex-approved v0 starting point; changes require a PR he approves.

Repo-specific part of `~/.agents/AGENTS.md` Task Queue.

**Security: nightly.** ccmorph holds every profile's credentials and routes
every Claude Code session on this Mac.

**Merge rule (Alex, 2026-10-17): the Hive SOP's risk classes.** Every PR gets
a fresh reviewer and the four Verification suites (see
[.github/CONTRIBUTING.md](.github/CONTRIBUTING.md)) in isolated test
homes on its exact head; only failures the PR introduced count (a failing test
is checked on `origin/main` and rerun alone). Behaviour changes merge after a
clean review like any other. High risk waits for Alex's ✅ on the exact head:
auth or credentials, which backend or provider a profile reaches, permissions,
the `_ccm_launch -a` entry point, and changes to this rule.

**ccmorph focus:** maintenance and patching, not new features. Every patch
ships with regression tests in `tests/` that fail before the fix, and merges
only when the four Verification suites show no failure it introduced;
an untested change does not merge. A bug fix that restores intended behaviour
is maintenance. A new behaviour at launch, in the model picker, or in which
backend a profile reaches is a product decision (`proposed`). The live install is `~/.config/ccmorph`: never run, edit or
fast-forward it, and never run the installer against a real home; merged
work reaches it when a session there next integrates `origin/main`.

**External contract:** `~/.agents` agent-dispatch, which launches every Hive
and spawned job, runs `source lib/ccmorph.sh && _ccm_launch -a <profile> <args>`.
Renaming or changing that entry point is a one-way door; the `_ccm_launch -a`
cases in `tests/ccmorph_test.zsh` guard it and must keep passing unchanged.

**In the Hive microVM** (Ubuntu): `sudo apt-get install -y zsh` once, and run
the Python suite as `CCMORPH_LIB_DIR="$PWD/lib" uv run --with pytest --with pytest-xdist --with pytest-timeout python -m
pytest tests/ -q`. The VM proves Linux; a change to a macOS-only path (BSD
tools, Keychain, launchd) says so under Merge danger.
- Vault: research, decisions and reports go to the Obsidian vault `20 Projects/ccmorph/` as `<Subject> — <Kind> — YYYY-MM-DD.md` (rules: llm-wiki skill); progress stays in Discord.
