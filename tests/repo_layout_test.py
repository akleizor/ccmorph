"""The repository root is the surface a public launch shows (issue #21).

GitHub and common tools look for their files at the root — README, LICENSE,
CHANGELOG, AGENTS.md, the installer, Makefile, pytest.ini — and everything
else lives under docs/ or .github/. The guard sits on the repo's own
product/machine-local line (the .gitignore header): a name at the root that is
not gitignored is product and must be in the list; one that is gitignored
(`dist/` from `make release`, `rc-hosts`) is this machine's, not the surface.
"""
import subprocess
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]

LAUNCH_SURFACE = {
    "AGENTS.md", "CHANGELOG.md", "LICENSE", "Makefile", "README.md",
    "install.sh", "pytest.ini", "go.mod",
    "adapter", "addons", "pycompat", "ctxwindow", "settingscompose", "assets", "bin", "cmd", "compat", "docs", "lib", "libexec", "scripts",
    "quota", "shim", "share", "tests", "util",
}


def test_root_holds_exactly_the_launch_surface_issue_21():
    visible = {p.name for p in ROOT.iterdir() if not p.name.startswith(".")}
    extra = sorted(visible - LAUNCH_SURFACE)
    # Gitignored names are machine-local build or tooling output, not shipped
    # surface; `git check-ignore --no-index` answers by pattern, not by what
    # happens to be tracked right now.
    if extra:
        proc = subprocess.run(
            ["git", "check-ignore", "--no-index", "--", *extra],
            cwd=ROOT, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        stray = [n for n in extra if n not in proc.stdout.split()]
        assert not stray, f"root carries files outside the launch surface: {stray}"
    missing = sorted(LAUNCH_SURFACE - visible)
    assert not missing, f"launch-surface entries missing from the root: {missing}"
    # The two dot-directories are part of the surface even though a plain `ls`
    # hides them: GitHub reads .github/SECURITY.md and .github/CONTRIBUTING.md,
    # Claude Code reads .claude/CLAUDE.md.
    assert (ROOT / ".github").is_dir(), ".github/ is part of the launch surface"
    assert (ROOT / ".claude").is_dir(), ".claude/ holds the Claude Code project memory"


def test_claude_project_memory_include_resolves_issue_21():
    """.claude/CLAUDE.md routes to AGENTS.md through a relative @-include; the
    include is the only line, and a path that stops resolving silently drops
    the project instructions from every Claude Code session in the repo."""
    memory = (ROOT / ".claude" / "CLAUDE.md").read_text().strip()
    assert memory.startswith("@"), "the project memory must be one @-include"
    target = (ROOT / ".claude" / memory[1:]).resolve()
    assert target.is_file(), f"@-include {memory[1:]!r} does not resolve"


def test_libexec_tracks_no_build_output():
    # libexec/ is filled by `make shim` and `make dist`; a committed binary
    # once shipped inside the v0.4.0 tag and every brew install.
    tracked = subprocess.run(
        ["git", "ls-files", "--", "libexec"], cwd=ROOT,
        capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout.split()
    assert tracked == ["libexec/.gitkeep"], f"libexec/ tracks build output: {tracked}"
