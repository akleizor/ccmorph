"""The repository root is the surface a public launch shows (issue #21).

GitHub and common tools look for their files at the root — README, LICENSE,
CHANGELOG, AGENTS.md, the installer, Makefile, pytest.ini — and everything
else lives under docs/ or .github/. The guard sits on the repo's own
product/machine-local line (the .gitignore header): a name at the root that is
not gitignored is product and must be in the list; one that is gitignored
(`dist/` from `make release`, `rc-hosts`) is this machine's, not the surface.
"""
import re
import subprocess
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]

LAUNCH_SURFACE = {
    "AGENTS.md", "CHANGELOG.md", "INTENT.md", "LICENSE", "Makefile", "README.md",
    "install.sh", "pytest.ini", "pyproject.toml", "go.mod", "go.sum",
    "adapter", "addons", "ordjson", "profile", "cli", "ctxwindow", "settingscompose", "assets", "bin", "cmd", "docs", "lib", "libexec", "scripts",
    "netroute", "quirks", "quota", "router", "shim", "share", "tests", "util",
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


def test_the_pytest_dependencies_travel_with_the_repo():
    """conftest.py turns xdist on only once it is installed, and the install
    must not depend on memorized `--with` flags: a session that types
    `uv run python -m pytest tests/` without them otherwise runs 1300+
    spawn-bound cases serially — about four minutes against the 60 s ceiling
    pytest.ini states. The default `test` group in pyproject.toml is what makes
    any uv run carry them; CI's pip installs stay serial by design."""
    text = (ROOT / "pyproject.toml").read_text()
    assert 'default-groups = ["test"]' in text, \
        "the test group must be a uv default, or a bare uv run stays serial"
    for name in ("pytest", "pytest-timeout", "pytest-xdist"):
        assert f'"{name}"' in text, f'{name} must stay in the default test group'


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


def test_router_completion_offers_only_router_verbs():
    import re
    text = (ROOT / "share/zsh/site-functions/_ccmorph").read_text()
    m = re.search(r"router\)\n\s*\(\( CURRENT == 3 \)\) && \{ compadd -- ([^;]+);", text)
    assert m, "the router completion arm moved"
    assert set(m.group(1).split()) == {"on", "off", "status", "dispatch", "report"}


def test_the_guide_counts_five_effort_levels():
    # #636: the guide said four while the picker and README list five.
    guide = (ROOT / 'docs' / 'guide.md').read_text()
    assert re.search(r"five\s+levels", guide)
    assert not re.search(r"four\s+levels", guide)


def test_install_reads_no_colour_variable_nothing_sets():
    # #636: c_ok/c_y/c_r/c_dim read _ccm_* colour variables no path ever set.
    text = (ROOT / "install.sh").read_text()
    for name in set(re.findall(r"\$\{(_ccm_\w+):-", text)):
        assert re.search(rf"\b{name}=", text), f"install.sh reads {name}, which nothing sets"


def test_the_gate_refuses_gofmt_drift():
    # #636: CI refused gofmt drift while make gate ran no formatter.
    make = (ROOT / "Makefile").read_text()
    gate = re.search(r"^gate: .*\n\t(.+)$", make, re.M).group(1)
    assert "gate-fmt" in gate, gate
    assert re.search(r"^gate-fmt:\n\t.*gofmt -l", make, re.M)


def test_ci_installs_zsh_in_one_place_and_runs_no_duplicate_floor():
    # #636: the zsh apt block was pasted into five jobs, the add-on compile list
    # twice, and the floor matrix ran 3.13 again beside the test job's 3.13.
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    action = (ROOT / ".github/actions/install-zsh/action.yml").read_text()
    assert (ci + action).count("apt-get install -y zsh\n") == 1
    floor = re.search(r"python: \[([^\]]*)\]", ci).group(1)
    assert '"3.13"' not in floor and '"3.8"' in floor
    assert ci.count("addons/remote-control/*.py") == 0
    assert "py-compile:" in (ROOT / "Makefile").read_text()


def test_every_ci_pytest_step_reads_the_checkouts_lib():
    # #636: the floor job ran pytest without CCMORPH_LIB_DIR, the omission the
    # test job's line was fixed for (gate-py, CONTRIBUTING and INTENT set it).
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    lines = [l for l in ci.splitlines() if "-m pytest" in l]
    assert lines and all('CCMORPH_LIB_DIR="$PWD/lib"' in l for l in lines), lines


def test_ruff_is_pinned_and_runs_in_the_gate_and_ci():
    # #636: the static layer had no bug-class lint; ruff is pinned, its rules
    # live in pyproject.toml, and both the gate and CI run it.
    py = (ROOT / "pyproject.toml").read_text()
    assert '"ruff==0.17.5"' in py
    assert 'select = ["F", "E9", "PLE", "B"]' in py and 'ignore = ["B904", "B905"]' in py
    gate = re.search(r"^gate-py:\n((?:\t.*\n)+)", (ROOT / "Makefile").read_text(), re.M).group(1)
    assert "ruff check" in gate
    assert "ruff==0.17.5 check" in (ROOT / ".github/workflows/ci.yml").read_text()


def test_the_shim_build_cleans_its_temp_on_a_signal():
    # #636: the recipe removed .shim-build.$$ only after its && chain, so a build
    # killed by a signal left it in libexec/.
    recipe = re.search(r"^shim:\n((?:\t.*\n)+)", (ROOT / "Makefile").read_text(), re.M).group(1)
    assert re.search(r"trap 'rm -rf \"\$\$made\"' EXIT INT TERM HUP", recipe), recipe


def test_netroute_pins_no_go_patch_release():
    # #636: the comment named Go 1.27.4 while the code reads golang.org/x/net's
    # httpproxy and go.mod carries no toolchain line.
    text = (ROOT / "netroute/netroute.go").read_text()
    assert not re.search(r"Go 1\.\d+\.\d+", text)
    assert "golang.org/x/net's httpproxy" in text


def test_the_addon_readme_documents_retire():
    # #636: retire=<folder> lived only in the supervisor's module header.
    assert "`retire=<folder>`" in (ROOT / "addons/remote-control/README.md").read_text()


def test_review_seen_lines_count_their_own_days():
    # #636: the dates are the count. The supervisor kind listed 2026-11-16 twice
    # (#596, then #805 and #807 on 2026-11-23); the real-tool kind missed GH-809.
    review = (ROOT / ".github/REVIEW.md").read_text()
    assert "Seen 3× (2026-11-16, 2026-11-23, 2026-11-23)." in review
    assert "Seen 4× (2026-11-10, 2026-11-12, 2026-11-21, 2026-11-23)." in review
    assert "identity is a tool too" in review


def test_the_guide_defines_every_quota_json_count():
    # #636: quota --json's unpriced_turns was defined nowhere a reader looks.
    assert "`unpriced_turns`" in (ROOT / "docs/guide.md").read_text()


def test_usage_export_docstring_names_every_option():
    # #636: the Usage line in the docstring --help prints left out --source.
    text = (ROOT / "share/usage-export").read_text()
    usage = re.search(r"^Usage: usage-export (.*)$", text, re.M).group(1)
    parsed = set(re.findall(r'add_argument\("(--[a-z-]+)"', text))
    assert parsed == set(re.findall(r"--[a-z-]+", usage)), (parsed, usage)


def test_the_readme_spells_config_with_an_optional_profile():
    # #636: help config says [profile]; the README's usage block said <profile>.
    readme = (ROOT / "README.md").read_text()
    assert "ccmorph config [profile]" in readme and "ccmorph config <profile>" not in readme


def test_the_readme_says_what_happens_with_your_own_status_line():
    # #393 (moved to #636): a person with their own status line sees the profile
    # only in the startup message; the README promised the status line.
    assert "With\nyour own status line, ccmorph keeps yours" in (ROOT / "README.md").read_text()


def test_the_conf_template_marks_email_required_for_an_account():
    # #636: the template did not say a Claude account profile needs its email.
    text = (ROOT / "share/profile.conf.example").read_text()
    assert re.search(r"^# email\s+sub:\s+account email \(required", text, re.M)


def test_the_core_usage_line_names_every_verb_it_dispatches():
    # #636: the no-arg usage listed seven verbs while the switch took twelve.
    main = (ROOT / "cmd/ccmorph-core/main.go").read_text()
    usage = re.search(r'"usage: ccmorph-core ([^ ]+) \.\.\."', main).group(1)
    cases = set(re.findall(r'^\tcase "([a-z-]+)":', main, re.M))
    assert set(usage.split("|")) == cases, (usage, cases)


def test_the_guide_explains_the_router_and_the_readme_names_its_undo():
    # #636: the guide had no router section; the README's usage block listed
    # only router on.
    guide = (ROOT / "docs/guide.md").read_text()
    section = guide.split("## Model router", 1)[1].split("\n## ", 1)[0]
    for word in ("router off", "**class**", "**slack**", "**no reading**", "dispatch --dry-run", "router report"):
        assert word in section, word
    assert "ccmorph router off" in (ROOT / "README.md").read_text()


def test_install_checks_dependencies_before_its_update_banner():
    # #636: `ccmorph update` with claude missing printed Updating x -> y first.
    text = (ROOT / "install.sh").read_text()
    assert text.index('missing+=("claude') < text.index('Updating ccmorph')
    readme = (ROOT / "README.md").read_text()
    assert "for a missing `claude` the Claude Code setup page" in readme
