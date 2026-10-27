"""ARCHITECTURE.md's pointers must land where the sentence says they do: an
in-page anchor on a heading, a code citation on the one line it names.

Issue #396: the send_id paragraph cited two `ccmorph.sh` line numbers, and both
had drifted into unrelated code -- a reader following either landed in a TERM/HUP
trap or the model picker and concluded the doc meant something else. #105 was the
same failure on the launch-sweep anchor. A line number moves with every unrelated
edit above it -- the gate below sat on 19 different lines across the 25 commits
before this one -- so the doc names the code instead, and a citation matching more
than one line is the one way that name stops being usable. A citation names the
FILE too, and #344/#352 split that file into modules: the test resolves whichever
one the prose points at, so a citation follows the code across the move instead
of silently outliving it.
"""
import re
import subprocess
from pathlib import Path

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
DOC = ROOT / "docs/ARCHITECTURE.md"

# A citation and the code it names, as (`GATE` in `ccmorph.sh`), allowing the one
# word the prose puts between them ("the gate `GATE` in `ccmorph.sh`", "the
# `READ` in `ccmorph.sh`"): a citation the pattern cannot see is one this test
# would pass while it lies. The file is whichever module holds the code -- since
# #352 the cited function may live in any of them -- and only ccmorph library
# files match: the doc also writes "in `model-window`" and "in `anthropic-beta`",
# which name a helper executable and an HTTP header, not a library.
CITATION = re.compile(r"`([^`]+)`(?:\s+\w+)?\s+in\s+`(ccmorph(?:-[\w.-]+)?\.zsh|ccmorph\.sh)`")


def _slug(h):
    s = re.sub(r"[^\w\- ]", "", h.strip().lower())
    return s.replace(" ", "-")


def test_architecture_anchors_resolve():
    text = DOC.read_text()
    slugs = {_slug(m.group(1)) for m in re.finditer(r"^#{1,6}\s+(.+)$", text, re.M)}
    links = set(re.findall(r"\]\(#([^)]+)\)", text))
    missing = sorted(links - slugs)
    assert not missing, f"dangling anchors: {missing}"


def test_architecture_cites_code_the_reader_can_land_on_issue_396():
    citations = CITATION.findall(DOC.read_text())
    assert citations, "ARCHITECTURE.md cites no ccmorph module code"
    for token, name in citations:
        source = ROOT / "lib" / name
        assert source.is_file(), (
            f"ARCHITECTURE.md cites {token!r} in {name}, which is not a file")
        # Split on `\n`, never `splitlines()`: the cited module may carry literal
        # U+2028 and U+2029 inside its JS-whitespace tables (ccmorph-context.zsh
        # does), and `splitlines()` counts those as line breaks, so it sees more
        # lines than the file has and every count below those tables comes out
        # wrong.
        lines = source.read_text().split("\n")
        hits = [n for n, line in enumerate(lines, 1) if token in line]
        assert len(hits) == 1, (
            f"ARCHITECTURE.md cites {token!r} in {name}, which holds it on "
            f"{len(hits)} lines {hits} -- cite code that names the one place meant")


def _tracked_markdown():
    """Every markdown file the repo ships, from the index: a machine-local
    tree (.claude/worktrees, dist) may sit beside the checkout holding other
    lanes' half-done docs, and their links are not this repo's to guard."""
    out = subprocess.run(["git", "ls-files", "--", "*.md"], cwd=ROOT,
                         capture_output=True, text=True, check=True,
                         timeout=HANG_TIMEOUT)
    docs = [ROOT / line for line in out.stdout.splitlines()]
    assert docs, "no tracked markdown found to check"
    return docs


def test_repo_markdown_links_resolve_issue_21():
    """#21 moved half the docs off the root; a link left pointing at the old
    path is one a reader hits on GitHub. Every relative link in tracked
    markdown must land on a file that exists and, when it names an anchor, on
    a heading that exists there. CHANGELOG.md is history: its released entries
    keep the paths that were true when they shipped, so it is not scanned."""
    link = re.compile(r"\]\(([^)#\s]+)(?:#([^)\s]+))?\)")
    for doc in _tracked_markdown():
        for target, anchor in link.findall(doc.read_text()):
            if "://" in target or target.startswith("mailto:"):
                continue
            path = doc.parent / target
            assert path.is_file(), (
                f"{doc.relative_to(ROOT)} links to {target!r}, which does not exist")
            if anchor:
                slugs = {_slug(m.group(1)) for m in re.finditer(
                    r"^#{1,6}\s+(.+)$", path.read_text(), re.M)}
                assert anchor in slugs, (
                    f"{doc.relative_to(ROOT)} links to #{anchor} in {target}, "
                    f"which has no such heading")