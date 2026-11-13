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
own `isLink` in `rm.go` was the same predicate and stayed for its own change
(#539): its ten call sites ask `paths.IsSymlink`, and the spelling is banned
with the others. `inList` joined
util's `hasString`; `mergedView` and `acwValid` became direct
`settingscompose` calls (`settingscompose`'s own `acwValid` judges its
internal `value`, not `any`, so it is a different function with the same
name). The rest leave no name to ban: the twice-compiled regexp, the
status-marker split, the home `.claude.json` fallback pair, and the ten verb
preambles are inline duplicates, which `requireProfile`/`judgeProfile` now
own.

Issue #520: inspect's model row hand-scanned the conf for a `model=` line the
readRaw above it had already parsed; the scan was a copy of the store's conf
parse that could not see the merged default the store seeds, so the row reads
the parsed copy first and asks the store only when the conf is silent.
invariantRecords parsed the settings twice — once for its model record, once
for the walk — and now holds one load. contextEnv spelled authKey's API-key
precedence into a pkey it discarded; the walk reads no key. The Stat and
Lstat existence predicates were spelled four ways more (`fileExists` in
cli/launch.go, `present` in cli/rm.go, `lexists` in util/setup.go, and
launch's profileLayer inlining its own Lstat) and the symlink resolver two
(`resolveLink` in util/misc.go, `resolvePath` in cli/hop.go); paths.Lexists
and paths.Resolve own them now. The tier-word set — opus, sonnet, haiku,
fable — was spelled six ways (cli's isTier, engine's aliases map, picker's
families slice and its inline check, and the served and refusal switches
in fetch and misc); util.IsTier owns it. EndRouteHold was endHold's pure
rename — one body, two names, the exported one for cli and the tests —
and is the one name now.

Issue #521: the write-temp-then-rename dance had two exported spellings
(util's and shim's AtomicWrite) and five hand-rolled copies in the packages
neither reaches — quota's two caches, settingscompose's projection, and
ctxwindow's conf rewrite and discovered tables. Its one definition is
`paths.AtomicWrite` in util/paths, the leaf every writer imports; util's and
shim's exported signatures stayed as thin delegations. `replaceFields`'
identity re-check rides along as the write's guard, run after the temp is
written and before the rename. The writes that are not the dance (a key
linked into place, a usage row's claim, configure's staged set, a streaming
copy, a readiness file, the session env file, the history repair's verified
copy) are named
with their reasons in `NOT_THE_DANCE`.

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
from functools import lru_cache
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
    "resolveLink": "paths.Resolve",
    "resolvePath": "paths.Resolve",
    "isTier": "util.IsTier",
    "endHold": "util.EndRouteHold",
}
# #422's predicates, and #520's strays beside them: the unexported spelling a
# new copy would take, mapped to the exported definition that replaced it.
# Only that exported spelling, and only inside util/paths, may be defined.
PREDICATES = {
    "exists": "Exists",
    "isdir": "IsDir",
    "isfile": "IsFile",
    "issymlink": "IsSymlink",
    "islink": "IsSymlink",
    "lexists": "Lexists",
    "present": "Lexists",
    "fileexists": "Exists",
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


# The three walks are memoized per root (#539): every guard runs them, no
# guard changes the tree it walks, and a test that pins ROOT builds its
# fixture whole before its first guard call — so the second guard in a run
# reuses the first's walk instead of re-running git and re-globbing the tree.
# The key is always the root, never a global: a cache keyed on nothing would
# serve one test's fixture to every other.
@lru_cache(maxsize=None)
def tree_tracked_files(root):
    """The paths this tree's repository tracks, as a set of absolute paths.

    The index is the tree's record of what it ships, so a tracked file is always
    this tree's copy, whatever lies beside it."""
    listed = git_output(root, "ls-files", "-z") or b""
    return {root / os.fsdecode(rel) for rel in listed.split(b"\0") if rel}


@lru_cache(maxsize=None)
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
    """Every .go file of this tree, by the walk `go_definitions` documents.

    ROOT is read at call time, so a test that pins it to its own fixture walks
    that fixture; the walk itself is `go_sources_of`'s, memoized per root."""
    return go_sources_of(ROOT)


@lru_cache(maxsize=None)
def go_sources_of(root):
    """The walk `go_sources` resolves ROOT into, as one list per root."""
    tracked = tree_tracked_files(root)
    siblings = sibling_worktrees(root)
    return [path for path in sorted(root.glob("**/*.go"))
            if ".git" not in path.parts
            and not in_a_sibling_worktree(path, siblings, tracked)]


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


# #520: inspect's model row fell back to a hand scan of the conf for a `model=`
# line — a second read of a file the readRaw at the top of inspect had already
# parsed, and a copy of the store's own conf parse that cannot see the merged
# default the store seeds from the shared settings. The row reads the parsed
# copy first and asks the store only when the conf is silent; this guard fails
# on the scan's return.
MODEL_SCAN = 'HasPrefix(line, "model=")'


def test_the_conf_is_not_hand_scanned_for_the_model_line():
    offenders = [str(path.relative_to(ROOT)) for path in go_sources()
                 if MODEL_SCAN in path.read_text()]
    assert not offenders, (
        "a conf is hand-scanned for its model= line beside the reads that "
        f"already own it (#520): {'; '.join(offenders)}")


# #520: invariantRecords parsed the settings twice — once for the model
# record, once for the walk — two reads of one file that must agree. The
# function holds one load now; this guard fails on a second.
SETTINGS_LOAD = "loadJSON(settings)"


def test_the_invariants_load_the_settings_once():
    diag = ROOT / "util" / "diag.go"
    loads = diag.read_text().count(SETTINGS_LOAD)
    assert loads == 1, (
        f"util/diag.go loads the settings {loads} times; the model record and "
        "the walk read one load (#520)")


# #520: contextEnv collected ANTHROPIC_API_KEY into pkey and let
# ANTHROPIC_AUTH_TOKEN fill it when absent — authKey's precedence, spelled a
# second time — and then discarded the value (`_ = pkey`): the walk wants the
# base URL, the model and the tier aliases, never a key. This guard fails on
# the precedence's return.
AUTHKEY_ENV = ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")


def test_contextenv_does_not_spell_authkeys_precedence():
    text = (ROOT / "cli" / "context.go").read_text()
    offenders = [name for name in AUTHKEY_ENV if name in text]
    assert not offenders, (
        "cli/context.go spells the API-key precedence that cli/run.go's "
        f"authKey owns (#520): {', '.join(offenders)}")


# #520: the tier-word set — opus, sonnet, haiku, fable — was spelled six ways:
# cli's isTier, engine's aliases map, picker's families slice and its inline
# check, and the served and refusal switches in fetch and misc. One predicate,
# util.IsTier, owns the set; the env walk's per-word case arms left with #517's
# resolveTier, so no other file spells a word. This guard fails on the set's
# return anywhere else (test fixtures may spell model names, so tests are not
# walked).
#
# #539: the two spellings the count could not see. The picker's alias rows
# spelled the words in a tab-separated literal, and the add wizard and the env
# allowlists spelled the uppercase keys; both are built from the word list now
# — the picker's rows and the wizard's env keys from util.TierWords and
# util.TierEnvKey — so the guard counts those spellings too: a word
# hand-spelled in either form fails it. profile and ctxwindow cannot import
# util (#423), so their env-key allowlists stay hand-spelled and are the key
# form's allowed homes beside the builder's own file.
TIER_DEFINITION = ROOT / "util" / "tier.go"
TIER_KEY_HOMES = {ROOT / "profile" / "envkeys.go", ROOT / "ctxwindow" / "conf.go"}
# The picker-row spelling: a tier word and the tab that follows it.
TIER_ROWS = ("fable\\t", "opus\\t", "sonnet\\t", "haiku\\t")
# The env-key spelling: the prefix of every ANTHROPIC_DEFAULT_<word>_MODEL key,
# spelled whole or built by hand-built concatenation.
TIER_KEY = "ANTHROPIC_DEFAULT_"


def test_the_tier_words_are_spelled_once():
    words, rows, keys = [], [], []
    for path in go_sources():
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text()
        if text.count('"fable"') > 0 and path != TIER_DEFINITION:
            words.append(f"{path.relative_to(ROOT)} spells the word")
        if any(row in text for row in TIER_ROWS):
            rows.append(f"{path.relative_to(ROOT)} spells a picker row")
        if TIER_KEY in text and path != TIER_DEFINITION and path not in TIER_KEY_HOMES:
            keys.append(f"{path.relative_to(ROOT)} spells the env key")
    assert not words, (
        "the tier-word set is hand-spelled outside util/tier.go (#520): "
        + "; ".join(words))
    assert not rows, (
        "a picker alias row is hand-spelled instead of built from "
        "util.TierWords (#539): " + "; ".join(rows))
    assert not keys, (
        "the tier env key is hand-spelled outside util.TierEnvKey and the "
        "allowlists that cannot import it (#539): " + "; ".join(keys))
    text = TIER_DEFINITION.read_text()
    assert text.count('"fable"') == 1, (
        "the tier-word set's one definition is no longer spelled once in "
        f"{TIER_DEFINITION.relative_to(ROOT)} (#520)")
    assert TIER_KEY in text, (
        f"the tier env key's builder is no longer in {TIER_DEFINITION.relative_to(ROOT)} (#539)")


# #539: the response-identity Key — id, stamp and turn joined on NULs — was
# spelled twice, writer and reader (shim's rowIdentity, quota's readRow),
# both citing #298 and flagged in #514's handoff as belonging to that fix.
# One builder, util.ResponseKey, owns the join; this guard fails on the
# turn's append spelled anywhere else (tests may join other keys on NULs,
# so they are not walked).
IDENTITY_TURN = '"\\x00" + turn'
IDENTITY_HOME = ROOT / "util" / "identity.go"


def test_the_response_identity_key_is_spelled_once():
    spelled = [path for path in go_sources()
               if not path.name.endswith("_test.go") and IDENTITY_TURN in path.read_text()]
    offenders = [str(path.relative_to(ROOT)) for path in spelled if path != IDENTITY_HOME]
    assert not offenders, (
        "the response-identity key is hand-spelled outside util.ResponseKey "
        f"(#539): {'; '.join(offenders)}")
    assert IDENTITY_HOME in spelled, (
        f"the response-identity key's one definition (#539) is no longer in "
        f"{IDENTITY_HOME.relative_to(ROOT)}")


# #521: the write-temp-then-rename dance — a temp in the target's own
# directory, written, closed, chmod'ed and renamed over the target, the temp
# removed on every failure — was hand-rolled beside the two exported
# AtomicWrites util and shim each kept: quota's reading and spend caches,
# settingscompose's projection, ctxwindow's conf rewrite, discovered tables
# and Restamp, and the launcher's log rotation. One definition,
# paths.AtomicWrite, owns it; util/paths is the leaf every writer reaches,
# since settingscompose and ctxwindow cannot import util (#423) and nothing
# below cli imports shim. These guards fail on a new hand-rolled copy in
# either spelling: a temp file belongs to that definition, or to the few
# writes that are not the dance.
TEMP_FILE = re.compile(r"os\.CreateTemp\(")
# The temp files that are not the dance, each with its reason.
NOT_THE_DANCE = {
    "shim/mask.go": "a masking key linked into place: the name only ever holds a whole key",
    "shim/sidecar.go": "a usage row's claim: hardlink or exclusive create, never a blind rename",
    "util/store.go": "configure's staged set: temps staged, then committed as one transaction",
    "util/migrate.go": "a streaming copy that verifies its hash before it renames",
    "util/adapterstart.go": "the adapter's readiness file, written by the child process",
    "cli/run.go": "the session env file: the temp is the file, nothing is replaced",
    "adapter/adapter_test.go": "a test's own stdio capture",
    # #539: the repair names itself here though the scanners cannot see its
    # write — it spells no CreateTemp and no WriteFile, only copyVerified and
    # a fixed-name temp — so the record, not the guard, carries its reason.
    "util/setup.go": "the history repair's verified copy (#205): streamed, hashed and read "
                     "back at a temp beside the store, then renamed over the store's link, so "
                     "the shared path always names a file — semantics AtomicWrite cannot "
                     "express, its guard never seeing the temp nor streaming the bytes",
}


def test_the_temp_then_rename_dance_is_spelled_once():
    home = ROOT / "util" / "paths" / "atomic.go"
    spelled = [path for path in go_sources() if TEMP_FILE.search(path.read_text())]
    offenders = [str(path.relative_to(ROOT)) for path in spelled
                 if path != home and path.relative_to(ROOT).as_posix() not in NOT_THE_DANCE]
    assert not offenders, (
        "a temp file is created outside paths.AtomicWrite (#521) and the writes "
        "that are not the dance (each named with its reason in NOT_THE_DANCE): "
        + "; ".join(offenders))
    assert home in spelled, (
        f"the temp-then-rename dance (#521) is no longer in {home.relative_to(ROOT)}")


# The dance's other spelling, without CreateTemp: a fixed-name temp written
# whole and renamed over its target — Restamp's, the last to move to
# paths.AtomicWrite. A production file that both writes with os.WriteFile and
# renames is either that dance or names itself here with its reason; a test
# file is fixture setup, not a writer.
WRITE_FILE = re.compile(r"os\.WriteFile\(")
RENAME = re.compile(r"os\.Rename\(")
# The writes and renames that are not the dance, each with its reason.
WRITES_AND_RENAMES = {
    "settingscompose/compose.go": "backup writes a unique stamped name directly and settle "
                                  "moves the pending file: neither replaces a file through a temp",
}


def test_a_fixed_temp_write_then_rename_is_spelled_nowhere():
    offenders = []
    for path in go_sources():
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text()
        if WRITE_FILE.search(text) and RENAME.search(text) \
                and path.relative_to(ROOT).as_posix() not in WRITES_AND_RENAMES:
            offenders.append(str(path.relative_to(ROOT)))
    assert not offenders, (
        "a production file both writes with os.WriteFile and renames (#521): "
        "replacing a file through a fixed-name temp is the dance "
        "paths.AtomicWrite owns — the files that are not it are named with "
        "their reason in WRITES_AND_RENAMES: " + "; ".join(offenders))


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
