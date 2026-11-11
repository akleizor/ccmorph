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
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
# Names whose hand-written copies #303 and #423 removed; the value says where
# the one definition now lives, for the failure message.
CONSOLIDATED = {
    "contains": "the standard library's slices.Contains",
    "stateDir": "paths.StateDir",
    "ccmorphScript": "paths.Script",
    "isDigits": "ordjson.IsDigits",
    "isNumber": "ordjson.IsNumber",
    "stripBOM": "ordjson.StripBOM",
    "truthy": "ordjson.Truthy",
    "truncRunes": "ordjson.TruncRunes",
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
# map[string]any; it is a different function with the same name, not a copy
# (#423). It is the only allowed unexported definition of a consolidated name.
NOT_COPIES = {(ROOT / "adapter" / "probe.go", "truthy")}
DEFINITION = re.compile(r"^func (\w+)\(", re.MULTILINE)


def go_definitions():
    """[(file, name)] for every function or method a non-vendored .go file defines."""
    found = []
    for path in sorted(ROOT.glob("**/*.go")):
        if ".git" in path.parts:
            continue
        for name in DEFINITION.findall(path.read_text()):
            found.append((path, name))
    return found


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
