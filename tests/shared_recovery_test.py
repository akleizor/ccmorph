"""Shared recovery merges history, never package or hook content."""
import os
from pathlib import Path
import shutil
import subprocess

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def migrate(tmp_path, name):
    original = tmp_path / "profile" / name
    shared = tmp_path / ".claude" / name
    original.mkdir(parents=True)
    shared.mkdir(parents=True)
    (original / "private").write_text("profile content")
    (shared / "private").write_text("shared content")
    (original / "new").write_text("new content")
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH_", "_CCMORPH_", "CLAUDE_", "ANTHROPIC_"))}
    env.update(HOME=str(tmp_path), ZDOTDIR=str(tmp_path),
               CCMORPH_STATE_DIR=str(tmp_path / "state"), CCMORPH_LIB_DIR=str(ROOT / "lib"))
    result = subprocess.run(
        ["zsh", "-fc", 'source "$1"; _ccm_migrate_projects "$2" "$3"', "_",
         str(ROOT / "lib/ccmorph.sh"), str(original), str(shared)],
        env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
    return result, original, shared


@pytest.mark.parametrize("name", ["agents", "projects", "session-env", "file-history"])
def test_sharable_content_collision_is_merged_into_the_shared_store(tmp_path, name):
    # agents holds content (subagent definitions), like skills, so a profile that
    # has a real agents/ before the shared store does is merged and linked rather
    # than refused — refusing bricked every launch for such a profile.
    result, original, shared = migrate(tmp_path, name)
    assert result.returncode == 0, result.stderr
    assert not original.exists()
    assert (shared / "new").read_text() == "new content"
    # The shared copy of a colliding name wins; the profile copy is kept aside.
    assert (shared / "private").read_text() == "shared content"
    backup, = original.parent.glob("*.premigration.*")
    assert (backup / "private").read_text() == "profile content"


@pytest.mark.parametrize("name", ["skills", "plugins", "hooks", "commands", "output-styles"])
def test_managed_content_collision_is_preserved(tmp_path, name):
    result, original, shared = migrate(tmp_path, name)
    assert result.returncode == 1
    assert "nothing moved" in result.stderr
    assert (original / "private").read_text() == "profile content"
    assert (original / "new").read_text() == "new content"
    assert (shared / "private").read_text() == "shared content"
    assert not (shared / "new").exists()
    assert not list(original.parent.glob("*.premigration.*"))


@pytest.mark.parametrize("name", ["projects", "session-env", "file-history"])
def test_history_migration_keeps_collisions_in_backup(tmp_path, name):
    result, original, shared = migrate(tmp_path, name)
    assert result.returncode == 0, result.stderr
    assert not original.exists()
    assert (shared / "new").read_text() == "new content"
    assert (shared / "private").read_text() == "shared content"
    backup, = original.parent.glob("*.premigration.*")
    assert (backup / "private").read_text() == "profile content"


def test_a_profile_with_its_own_agents_directory_launches(tmp_path):
    # End to end at the provisioning chokepoint: a profile that already holds a
    # real, non-empty agents/ must provision (and therefore launch) rather than
    # fail on the collision, ending with the file in the shared store and a
    # symlink in its place. Before the fix this returned 1 and printed "shared
    # 'agents' collision", so every launch of that profile was blocked.
    home = tmp_path / "home"
    profile = home / ".config/ccmorph/profiles/gw"
    (profile / "agents").mkdir(parents=True)
    (profile / "agents" / "reviewer.md").write_text("my reviewer")
    (home / ".claude" / "agents").mkdir(parents=True)
    (home / ".claude" / "agents" / "other.md").write_text("shared other")
    (home / ".config/ccmorph/gw.conf").write_text("type=sub\nemail=me@example.invalid\n")

    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH_", "_CCMORPH_", "CLAUDE_", "ANTHROPIC_"))}
    env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / ".config"),
               CCMORPH_STATE_DIR=str(home / ".config/ccmorph"), CCMORPH_LIB_DIR=str(ROOT / "lib"))
    result = subprocess.run(
        ["zsh", "-fc",
         'source "$1"; typeset -gA _ccm=(type sub); _ccm_setup_dir_unlocked gw', "_",
         str(ROOT / "lib/ccmorph.sh")],
        env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)

    assert result.returncode == 0, result.stderr
    assert "collision" not in result.stderr
    link = profile / "agents"
    assert link.is_symlink() and link.resolve() == (home / ".claude/agents").resolve()
    assert (home / ".claude/agents/reviewer.md").read_text() == "my reviewer"
    assert (home / ".claude/agents/other.md").read_text() == "shared other"


def provision_agents(tmp_path, build, *, state_link=False, script=None):
    """Provision profile gw whose real agents/ is filled by build(agents_dir, home).

    With state_link, ~/.config/ccmorph is a symlink to another directory, so the
    profile's physical path differs from its spelled one.
    """
    home = tmp_path / "home"
    state = home / ".config/ccmorph"
    if state_link:
        real = tmp_path / "elsewhere" / "ccs"
        real.mkdir(parents=True)
        state.parent.mkdir(parents=True)
        state.symlink_to(real)
    profile = state / "profiles/gw"
    (profile / "agents").mkdir(parents=True)
    (home / ".claude/agents").mkdir(parents=True)
    (state / "gw.conf").write_text("type=sub\nemail=me@example.invalid\n")
    build(profile / "agents", home)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CCMORPH_", "_CCMORPH_", "CLAUDE_", "ANTHROPIC_"))}
    env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / ".config"),
               CCMORPH_STATE_DIR=str(state), CCMORPH_LIB_DIR=str(ROOT / "lib"))
    script = script or 'source "$1"; typeset -gA _ccm=(type sub); _ccm_setup_dir_unlocked gw'
    result = subprocess.run(["zsh", "-fc", script, "_", str(ROOT / "lib/ccmorph.sh")],
                            env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
    return result, home, profile


def test_env_output_stays_clean_while_relative_agent_links_migrate(tmp_path):
    # Constraint 1 (#70): a `local` re-declared inside the merge loop makes zsh
    # print `tgt=...`, which `ccmorph _env` emits and a launch exports.
    def build(agents, home):
        (home / "dot").mkdir()
        for n in ("a", "b", "c"):
            (home / "dot" / f"{n}.md").write_text(n)
            (agents / f"{n}.md").symlink_to(f"../../../../../dot/{n}.md")
    result, home, profile = provision_agents(
        tmp_path, build, script='source "$1"; ccmorph _env gw')
    assert result.returncode == 0, result.stderr
    lines = result.stdout.splitlines()
    assert lines, result.stderr
    for line in lines:
        key = line.split("=", 1)[0]
        assert key.isupper() and key.replace("_", "").isalnum(), line
    assert "tgt=" not in result.stdout + result.stderr
    assert "abs=" not in result.stdout + result.stderr
    for n in ("a", "b", "c"):
        assert (home / ".claude/agents" / f"{n}.md").read_text() == n


def test_relative_agent_link_outside_the_tree_becomes_absolute_physical(tmp_path):
    # Constraint 2: the kernel resolves a relative link from its physical
    # directory; under a symlinked state dir a lexical rewrite points elsewhere.
    # Physically: agents -> gw -> profiles -> ccs -> elsewhere -> tmp_path.
    def build(agents, home):
        (tmp_path / "dot").mkdir()
        (tmp_path / "dot" / "out.md").write_text("outside")
        (agents / "out.md").symlink_to("../../../../../dot/out.md")
        assert (agents / "out.md").read_text() == "outside"
    result, home, profile = provision_agents(tmp_path, build, state_link=True)
    assert result.returncode == 0, result.stderr
    moved = home / ".claude/agents/out.md"
    assert moved.is_symlink()
    assert os.readlink(moved) == str((tmp_path / "dot" / "out.md").resolve())
    assert moved.read_text() == "outside"


def test_relative_agent_link_inside_the_tree_points_into_the_shared_store(tmp_path):
    # Constraint 3: b.md -> a.md must follow a.md into the shared store, not
    # the profile dir, so it survives `ccmorph rm/mv` of the profile.
    def build(agents, home):
        (agents / "a.md").write_text("a")
        (agents / "sub").mkdir()
        (agents / "sub" / "c.md").write_text("c")
        (agents / "b.md").symlink_to("a.md")
        (agents / "sub" / "d.md").symlink_to("../a.md")
        (agents / "e").symlink_to("sub")
    result, home, profile = provision_agents(tmp_path, build)
    assert result.returncode == 0, result.stderr
    shared = home / ".claude/agents"
    assert os.readlink(shared / "b.md") == str(shared / "a.md")
    assert os.readlink(shared / "sub/d.md") == str(shared / "a.md")
    assert os.readlink(shared / "e") == str(shared / "sub")
    shutil.rmtree(profile)
    assert (shared / "b.md").read_text() == "a"
    assert (shared / "sub/d.md").read_text() == "a"
    assert (shared / "e/c.md").read_text() == "c"


def test_relative_link_through_a_symlinked_dir_follows_the_kernel_not_dotdot_text(tmp_path):
    # `out/../real.md` with `out` a link: the kernel resolves `..` from out's
    # physical target, not by dropping the `out` component textually.
    def build(agents, home):
        deep = tmp_path / "other" / "deep"
        deep.mkdir(parents=True)
        (tmp_path / "other" / "real.md").write_text("kernel")
        (agents / "real.md").write_text("textual")
        (agents / "out").symlink_to(deep)
        (agents / "viasym.md").symlink_to("out/../real.md")
        assert (agents / "viasym.md").read_text() == "kernel"
    result, home, profile = provision_agents(tmp_path, build)
    assert result.returncode == 0, result.stderr
    assert (home / ".claude/agents/viasym.md").read_text() == "kernel"


@pytest.mark.parametrize("dirname", ["defs", "defs\n"], ids=["plain", "trailing-newline"])
def test_relative_link_through_a_dir_link_keeps_its_literal_dir_name(tmp_path, dirname):
    # $(...) strips trailing newlines, so a physical dir resolved through
    # command substitution named a different directory (#267).
    real = tmp_path / dirname
    def build(agents, home):
        real.mkdir()
        (real / "reviewer.md").write_text("real")
        if dirname != "defs":
            (tmp_path / "defs").mkdir()
            (tmp_path / "defs" / "reviewer.md").write_text("decoy")
        (agents / "ext").symlink_to(real)
        (agents / "reviewer.md").symlink_to("ext/reviewer.md")
        assert (agents / "reviewer.md").read_text() == "real"
    result, home, profile = provision_agents(tmp_path, build)
    assert result.returncode == 0, result.stderr
    moved = home / ".claude/agents/reviewer.md"
    assert os.readlink(moved) == str(real.resolve() / "reviewer.md")
    assert moved.read_text() == "real"


def test_absolute_agent_links_and_dir_links_move_as_links_without_overwriting(tmp_path):
    # Constraint 4: absolute links and symlinked dirs move as links (a dir link
    # is not descended); a name already in the shared store wins and the
    # profile's link stays in the backup.
    src = tmp_path / "src"
    def build(agents, home):
        (src / "teamdir").mkdir(parents=True)
        (src / "teamdir" / "t.md").write_text("t")
        (src / "linked.md").write_text("linked")
        (src / "taken.md").write_text("profile taken")
        (agents / "linked.md").symlink_to(src / "linked.md")
        (agents / "team").symlink_to(src / "teamdir")
        (agents / "taken.md").symlink_to(src / "taken.md")
        (agents / "rel-taken.md").symlink_to("linked.md")
        (home / ".claude/agents/taken.md").write_text("shared taken")
        (home / ".claude/agents/rel-taken.md").write_text("shared rel")
    result, home, profile = provision_agents(tmp_path, build)
    assert result.returncode == 0, result.stderr
    shared = home / ".claude/agents"
    assert os.readlink(shared / "linked.md") == str(src / "linked.md")
    assert os.readlink(shared / "team") == str(src / "teamdir")
    assert (shared / "team/t.md").read_text() == "t"
    assert not (shared / "t.md").exists()
    assert (shared / "taken.md").read_text() == "shared taken"
    assert (shared / "rel-taken.md").read_text() == "shared rel"
    backup, = profile.glob("agents.premigration.*")
    assert os.readlink(backup / "taken.md") == str(src / "taken.md")
    assert os.readlink(backup / "rel-taken.md") == "linked.md"
    assert not (backup / "linked.md").is_symlink()
    assert not (backup / "team").is_symlink()


def test_failed_link_creation_keeps_the_backup_entry(tmp_path):
    # Constraint 4: the backup entry is removed only after the new link exists.
    # A read-only shared subdir makes `ln -s` fail there.
    if os.geteuid() == 0:
        pytest.skip("root ignores directory permissions")
    def build(agents, home):
        (agents / "sub").mkdir()
        (agents / "a.md").write_text("a")
        (agents / "sub" / "r.md").symlink_to("../a.md")
        ro = home / ".claude/agents/sub"
        ro.mkdir()
        ro.chmod(0o555)
    try:
        result, home, profile = provision_agents(tmp_path, build)
    finally:
        (tmp_path / "home/.claude/agents/sub").chmod(0o755)
    assert result.returncode == 0, result.stderr
    backup, = profile.glob("agents.premigration.*")
    assert os.readlink(backup / "sub/r.md") == "../a.md"


def test_architecture_lists_agents_as_merged_and_links_moving_as_links():
    # Constraint 5: the documented contract matches the merge loop.
    row, = [l for l in (ROOT / "docs/ARCHITECTURE.md").read_text().splitlines()
            if "mergeable stores" in l]
    assert "`agents`, `session-env`, `file-history`" in row
    assert "Links move as links" in row
