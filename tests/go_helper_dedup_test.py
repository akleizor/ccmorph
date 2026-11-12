"""A hand-copied helper drifts, and a durable contract splits in two.

Issue #303: `contains` was written out three times, `stateDir` twice (where
usage rows live) and `ccmorphScript` twice (where the driver script lives).
Identical copies pass every suite while they stay identical; the first edit to
one copy the others miss is a silent behaviour change. So each keeps one
definition: `contains` is the standard library's `slices.Contains`, and the two
path contracts are `paths.StateDir` and `paths.Script`. This guard fails on any
re-introduction of a hand-written copy.

Issue #422: the file predicates `exists`, `isDir`, `isFile` and `isSymlink`
were hand-written eleven times across six files. Their one definition each is
exported from `util/paths`; this guard fails on a new copy anywhere else.

Issue #423: `isDigits`, `isNumber`, `stripBOM`, `truthy` and `truncRunes` were
each written out twice because `util` imports `settingscompose`, so the latter
cannot reach `util`'s definitions. Their one definition each now lives in
`ordjson`, which both packages already import. `pyStr` stays split: its two
fallbacks marshal maps with different key orders, so merging it would change
behaviour.

Issue #415: the guard walked the checkout directory into `.claude/worktrees/`,
where sibling sessions keep their private worktrees, so a sibling's in-flight
copy of an old helper failed it on main (2026-11-10). The walk now skips a
directory only when git itself says it is another working tree of this
repository: `git worktree list` registers it, and git run inside it finds its
top level there. A `.git` entry alone marks nothing, and a tracked file is
always read; every other .go file under the checkout, tracked, untracked or
ignored, is still read, as before.

Issue #487: the strays the earlier sweeps missed. The shim spelled its UTC
stamp out three times (`isoNow`, the sidecar's start, `rowTime`); ctxwindow
kept `objOf` beside `asObj`, `posInt` beside `asPosInt`, `stemOf` beside
`stem`, and three hand-rolled conf-directory scans; profile and
settingscompose each had a `set`. One definition each: `shim.utcStamp`,
`ctxwindow.asObj`/`asPosInt`/`stem`/`confScan`, `profile.SetOf`. The inline
duplicates (the session capture in `swapDevice`, the argparse loops) leave no
name to ban, so the guard holds only the named ones. `cli/launch.go`'s
`posInt` parses a digit string into an int, not a JSON number into a window,
so it is a different function with the same name, not a copy — as #423's
`adapter`/`truthy` is.

Issue #486: the strays the util+cli batch carried. util's `isLink` was
`paths.IsSymlink` spelled out — the #422 predicate sweep had missed it; cli's
own `isLink` in `rm.go` is the same predicate and stays for its own change,
so the name is not banned while the two packages differ. `inList` joined
util's `hasString`; `mergedView` and `acwValid` became direct
`settingscompose` calls (`settingscompose`'s own `acwValid` judges its
internal `value`, not `any`, so it is a different function with the same
name). The rest leave no name to ban: the twice-compiled regexp, the
status-marker split, the home `.claude.json` fallback pair, and the ten verb
preambles are inline duplicates, which `requireProfile`/`judgeProfile` now
own.

Issue #523: the profile-root ladder — `$CCMORPH_STATE_DIR`, else
`$XDG_CONFIG_HOME/ccmorph`, else `~/.config/ccmorph` over the account home
#138 made every reader use — was spelled three times: complete in the
launcher's `defaultPaths`, degraded in `ctxwindow`'s `StateDir` (HOME unset
answered `/.config/ccmorph`) and in the adapter's `defaultProfileDirs` (HOME
unset answered `.config/ccmorph` relative to the cwd). Its one definition is
`paths.ProfileRoot` over `paths.Home`, which carries the account-home
fallback and the HOME re-export; the adapter's private `home()` is gone and
`findTranslator` reads `paths.Home`. `bin/ccmorph` keeps a
deliberately-parallel derivation of its own — it must find `lib/ccmorph.sh`
from zsh before any Go code can run — so the guard bans only the Go env
reads, not the zsh spelling.
"""
import os
import re
import subprocess
import sys
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parent.parent
# Names whose hand-written copies #303, #423 and #487 removed; the value says
# where the one definition now lives, for the failure message.
CONSOLIDATED = {
    "contains": "the standard library's slices.Contains",
    "stateDir": "paths.StateDir",
    "ccmorphScript": "paths.Script",
    "isDigits": "ordjson.IsDigits",
    "isNumber": "ordjson.IsNumber",
    "stripBOM": "ordjson.StripBOM",
    "truthy": "ordjson.Truthy",
    "truncRunes": "ordjson.TruncRunes",
    "isoNow": "shim.utcStamp",
    "posInt": "ctxwindow.asPosInt",
    "objOf": "ctxwindow.asObj",
    "stemOf": "ctxwindow.stem",
    "globConf": "ctxwindow.confScan",
    "set": "profile.SetOf",
    "inList": "util.hasString",
    "mergedView": "settingscompose.MergedView",
    "acwValid": "settingscompose.ACWValid",
}
# #422's predicates: the unexported spelling a new copy would take, mapped to
# the exported definition that replaced it. Only that exported spelling, and
# only inside util/paths, may be defined.
PREDICATES = {
    "exists": "Exists",
    "isdir": "IsDir",
    "isfile": "IsFile",
    "issymlink": "IsSymlink",
}
PATHS_PACKAGE = ROOT / "util" / "paths"
# `adapter` decodes with encoding/json, so its `truthy` takes float64 and
# map[string]any; `cli`'s `posInt` parses a digit string into an int, not a
# JSON number into a window; `settingscompose`'s `acwValid` judges its
# internal `value`, not `any` (#423, #487, #486). Each is a different function
# with the same name, not a copy. They are the only allowed unexported
# definitions of a consolidated name.
NOT_COPIES = {(ROOT / "adapter" / "probe.go", "truthy"),
              (ROOT / "cli" / "launch.go", "posInt"),
              (ROOT / "settingscompose" / "compose.go", "acwValid")}
DEFINITION = re.compile(r"^func (\w+)\(", re.MULTILINE)


def git_output(where, *args):
    """git's stdout for `args` run in `where`, or None when git refuses there.

    None stands for "git does not see a tree here": no repository, an empty or
    broken `.git`, or no git at all."""
    try:
        done = subprocess.run(["git", "-C", str(where), *args],
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                              timeout=HANG_TIMEOUT)
    except OSError:
        return None
    return done.stdout if done.returncode == 0 else None


def tree_tracked_files(root):
    """The paths this tree's repository tracks, as a set of absolute paths.

    The index is the tree's record of what it ships, so a tracked file is always
    this tree's copy, whatever lies beside it."""
    listed = git_output(root, "ls-files", "-z") or b""
    return {root / os.fsdecode(rel) for rel in listed.split(b"\0") if rel}


def sibling_worktrees(root):
    """The roots of the other working trees of `root`'s repository below `root`.

    A directory is another tree's root only when git says so twice: `root`'s
    repository registers it (`git worktree list`), and git, run inside it, finds
    its top level there. A stray, empty or broken `.git`, or a repository of its
    own, is neither, so every file beneath it stays this tree's (#415)."""
    listed = git_output(root, "worktree", "list", "--porcelain") or b""
    found = set()
    for line in os.fsdecode(listed).splitlines():
        if not line.startswith("worktree "):
            continue
        tree = Path(line[len("worktree "):]).resolve()
        if root not in tree.parents:
            continue
        top = git_output(tree, "rev-parse", "--show-toplevel")
        if top is not None and Path(os.fsdecode(top).rstrip("\n")).resolve() == tree:
            found.add(tree)
    return found


def in_a_sibling_worktree(path, siblings, tracked):
    """Whether `path` is a sibling worktree's file, not this tree's: it lies under
    one of `siblings` and this tree's repository does not track it."""
    return path not in tracked and any(parent in siblings for parent in path.parents)


def go_sources():
    """Every .go file of this tree, by the walk `go_definitions` documents."""
    tracked = tree_tracked_files(ROOT)
    siblings = sibling_worktrees(ROOT)
    for path in sorted(ROOT.glob("**/*.go")):
        if ".git" in path.parts or in_a_sibling_worktree(path, siblings, tracked):
            continue
        yield path


def go_definitions():
    """[(file, name)] for every function or method a .go file of this tree defines.

    The walk reads every .go file under ROOT, tracked, untracked or ignored, as
    8a151a5's did. It skips only an untracked file inside a sibling worktree."""
    return [(path, name) for path in go_sources()
            for name in DEFINITION.findall(path.read_text())]


def test_the_consolidated_helpers_are_not_hand_written_again():
    offenders = [
        f"{path.relative_to(ROOT)}: func {name}("
        for path, name in go_definitions()
        if name in CONSOLIDATED and (path, name) not in NOT_COPIES
    ]
    assert not offenders, (
        "a consolidated helper is hand-written again (it must be "
        + ", ".join(f"{n} -> {home}" for n, home in CONSOLIDATED.items())
        + "; copies drift): " + "; ".join(offenders))


def test_the_file_predicates_are_defined_only_in_paths():
    offenders = []
    for path, name in go_definitions():
        shared = PREDICATES.get(name.lower())
        if shared is None:
            continue
        if PATHS_PACKAGE in path.parents and name == shared:
            continue
        offenders.append(f"{path.relative_to(ROOT)}: func {name}(")
    assert not offenders, (
        "a file predicate #422 consolidated is hand-written outside util/paths "
        "(it must be " + ", ".join(f"{n} -> paths.{p}" for n, p in PREDICATES.items())
        + "; copies drift): " + "; ".join(offenders))


# #516: the verdict a probe's HTTP status earns — a 2xx accepts, 401/403
# rejects, "000" or empty says nothing answered — was hand-spelled in cli and
# again in each of doctor's four route checks. It has one definition,
# util.ProbeVerdict, which cli and util both read; this guard fails on a new
# hand-spelled copy.
VERDICT_SWITCH = 'HasPrefix(code, "2")'


def test_the_probe_verdict_switch_is_spelled_once():
    home = ROOT / "util" / "fetch.go"
    spelled = [path for path in go_sources() if VERDICT_SWITCH in path.read_text()]
    offenders = [str(path.relative_to(ROOT)) for path in spelled if path != home]
    assert not offenders, (
        "the probe verdict's status switch is hand-spelled outside "
        f"util.ProbeVerdict (#516): {'; '.join(offenders)}")
    assert home in spelled, (
        f"the probe verdict's status switch (#516) is no longer in {home.relative_to(ROOT)}")


# #523: the profile-root ladder ($CCMORPH_STATE_DIR, else $XDG_CONFIG_HOME,
# else ~/.config/ccmorph over the account home) was spelled three times — the
# launcher's copy complete, the probe's answering /.config/ccmorph with HOME
# unset and the adapter's a path relative to the cwd. It has one definition,
# paths.ProfileRoot over paths.Home; this guard fails on a new hand-spelled
# copy.
LADDER_RUNGS = ('os.Getenv("CCMORPH_STATE_DIR")', 'os.Getenv("XDG_CONFIG_HOME")')


def test_the_profile_root_ladder_is_spelled_once():
    home = ROOT / "util" / "paths" / "paths.go"
    offenders = []
    spelled = []
    for path in go_sources():
        rungs = [rung for rung in LADDER_RUNGS if rung in path.read_text()]
        if not rungs:
            continue
        spelled.append(path)
        if path != home:
            offenders.append(f"{path.relative_to(ROOT)}: {', '.join(rungs)}")
    assert not offenders, (
        "the profile-root ladder (#523) is hand-spelled outside util/paths "
        "(it is paths.ProfileRoot there, over paths.Home): " + "; ".join(offenders))
    assert home in spelled, (
        f"the profile-root ladder (#523) is no longer spelled in {home.relative_to(ROOT)}")


COPY = ("package shim\n\n"
        "func contains(xs []string, x string) bool { return false }\n"
        "func stateDir() string { return \"\" }\n"
        "func isFile(p string) bool { return false }\n")

def git(repo, *args):
    """Run git in the fixture checkout `repo`; a failure fails the test."""
    subprocess.run(["git", "-C", str(repo), *args], check=True, timeout=HANG_TIMEOUT)


def checkout(tmp_path):
    """A throwaway checkout with one commit, so a worktree can be added to it.

    `.claude/worktrees/` is ignored there as this repo ignores it."""
    repo = tmp_path / "repo"
    (repo / "shim").mkdir(parents=True)
    (repo / "shim" / "mask.go").write_text(
        "package shim\n\nfunc mask(s string) string { return s }\n")
    (repo / ".gitignore").write_text(".claude/worktrees/\n")
    git(repo, "init", "-q")
    # -f: the operator's global ignore rules must not decide this fixture's tree.
    git(repo, "add", "-f", "--", "shim/mask.go", ".gitignore")
    git(repo, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "-m", "init")
    return repo


def add_sibling(repo, *flags):
    """A sibling session's worktree, as `git worktree add` makes one under
    `.claude/worktrees/`."""
    sibling = repo / ".claude" / "worktrees" / "sibling"
    git(repo, "worktree", "add", "-q", "--detach", *flags, str(sibling))
    return sibling


# How git holds a copy: in the index, untracked, excluded by .git/info/exclude,
# or ignored by .gitignore. The walk reads it every way.
HOW = ["tracked", "untracked", "excluded", "gitignored"]


def place_copy(repo, copy, how):
    """Write the hand-written copy at `copy` and hold it as `how` says."""
    target = repo / copy
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(COPY)
    if how == "tracked":
        git(repo, "add", "-f", "--", copy)
    elif how == "excluded":
        with (repo / ".git" / "info" / "exclude").open("a") as f:
            f.write("/" + copy + "\n")
    elif how == "gitignored":
        with (repo / ".gitignore").open("a") as f:
            f.write(copy + "\n")


def assert_both_guards_fail(name):
    """Both guards raise, naming the copy at `name`."""
    file = re.escape(name)
    with pytest.raises(AssertionError, match=file + r": func contains\("):
        test_the_consolidated_helpers_are_not_hand_written_again()
    with pytest.raises(AssertionError, match=file + r": func isFile\("):
        test_the_file_predicates_are_defined_only_in_paths()


@pytest.mark.parametrize("flags", [[], ["--lock"]], ids=["unlocked", "locked"])
def test_a_sibling_worktree_copy_does_not_fail_the_guard(tmp_path, monkeypatch, flags):
    repo = checkout(tmp_path)
    add_sibling(repo, *flags)
    place_copy(repo, ".claude/worktrees/sibling/shim/sidecar.go", "untracked")
    monkeypatch.setattr(sys.modules[__name__], "ROOT", repo)
    # The #415 reproduction: a sibling session's in-flight copy; neither guard reads it.
    test_the_consolidated_helpers_are_not_hand_written_again()
    test_the_file_predicates_are_defined_only_in_paths()


@pytest.mark.parametrize("how", HOW)
def test_a_copy_this_tree_holds_still_fails_both_guards(tmp_path, monkeypatch, how):
    repo = checkout(tmp_path)
    # A sibling worktree beside the copy must not hide it.
    add_sibling(repo)
    place_copy(repo, "shim/reintroduced.go", how)
    monkeypatch.setattr(sys.modules[__name__], "ROOT", repo)
    assert_both_guards_fail("shim/reintroduced.go")


# A `.git` entry in the copy's own parent that git does not take for a worktree
# of this repository: an empty file, an empty directory, a gitfile pointing
# nowhere, or a repository of its own.
STRAY = ["file", "dir", "gitfile", "repo"]


@pytest.mark.parametrize("how", ["tracked", "untracked"])
@pytest.mark.parametrize("stray", STRAY)
def test_a_copy_beside_a_stray_git_entry_still_fails_both_guards(
        tmp_path, monkeypatch, stray, how):
    repo = checkout(tmp_path)
    place_copy(repo, "shim/reintroduced.go", how)
    marker = repo / "shim" / ".git"
    if stray == "file":
        marker.write_text("")
    elif stray == "dir":
        marker.mkdir()
    elif stray == "gitfile":
        marker.write_text("gitdir: " + str(tmp_path / "nowhere") + "\n")
    else:
        git(repo / "shim", "init", "-q")
    monkeypatch.setattr(sys.modules[__name__], "ROOT", repo)
    assert_both_guards_fail("shim/reintroduced.go")


@pytest.mark.parametrize("damage", ["emptied", "removed"])
def test_a_copy_in_a_worktree_git_cannot_open_still_fails_both_guards(
        tmp_path, monkeypatch, damage):
    repo = checkout(tmp_path)
    sibling = add_sibling(repo)
    place_copy(repo, ".claude/worktrees/sibling/shim/sidecar.go", "untracked")
    # Registered still, but git no longer finds a tree at its root.
    if damage == "emptied":
        (sibling / ".git").write_text("")
    else:
        (sibling / ".git").unlink()
    monkeypatch.setattr(sys.modules[__name__], "ROOT", repo)
    assert_both_guards_fail(".claude/worktrees/sibling/shim/sidecar.go")
