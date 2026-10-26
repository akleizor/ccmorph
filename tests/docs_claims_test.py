"""Docs claims that must not outlive the code they describe.

Issue #211: the guide and the changelog still described shim behaviour that #188
and #190 had removed, and an Unreleased `Changed` entry still offered a
`profile.conf.example` that "ships with ... the formula" after the Homebrew
formula was removed. A released entry keeps whatever was true when it shipped;
the guide and the `## Unreleased` section are read as a description of today, so
those are what these guards pin.
Issue #375: Unreleased/Added still documented the role launch #348 removed.
Issue #389: Unreleased still mentioned that role launch outside `### Removed`,
under a feature that never appeared in any release.
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHANGELOG = ROOT / "CHANGELOG.md"
GUIDE = ROOT / "docs/guide.md"


def _flatten(text):
    return re.sub(r"\s+", " ", text)


def _unreleased():
    text = CHANGELOG.read_text(encoding="utf-8")
    match = re.search(r"^## Unreleased\n(.*?)(?=^## )", text, re.M | re.S)
    assert match, "CHANGELOG.md has no `## Unreleased` section"
    return match.group(1)


def _sections(section):
    """(heading, body) for each `### ` block; the lead-in gets an empty heading."""
    parts = re.split(r"^### ", section, flags=re.M)
    yield "", parts[0]
    for part in parts[1:]:
        heading, _, body = part.partition("\n")
        yield heading.strip(), body


def _claims_outside_removed(needles):
    """Unreleased lines naming any of `needles`, outside the `### Removed` block."""
    lowered = [needle.lower() for needle in needles]
    return [line.strip()
            for heading, body in _sections(_unreleased())
            if "removed" not in heading.lower()
            for line in body.splitlines() if any(needle in line.lower() for needle in lowered)]


def test_unreleased_has_one_changed_heading():
    headings = [heading for heading, _ in _sections(_unreleased()) if heading]
    assert headings.count("Changed") == 1, (
        f"`## Unreleased` must hold exactly one `### Changed`: {headings}")


def test_unreleased_offers_the_formula_only_where_it_is_removed():
    """The formula is gone, so no other Unreleased entry may still offer it."""
    offenders = _claims_outside_removed(["formula"])
    assert not offenders, (
        f"`## Unreleased` still offers the removed Homebrew formula: {offenders}")


def test_unreleased_never_names_the_retired_role_feature():
    """#375/#389: the role launch, its key and its tags never shipped — #348 added
    them on 2026-10-15 and removed them on 2026-10-18, so no release was cut while
    they existed — and the section read as a description of today must not name
    them at all. The boundary is "not a letter", not `\\b`, because the spellings
    that shipped are joined by an underscore (`CCPROTEUS_ROLE`, `role=`,
    `x-ccproteus-role`) where `\\b` finds no boundary. Spliced so this file is not
    a hit for the name it guards; an entry that later needs the word for a message
    role should say so in a narrower guard rather than widen this one."""
    stale = "ro" "le"
    offenders = [line.strip() for line in _unreleased().splitlines()
                 if re.search(rf"(?<![A-Za-z]){stale}s?(?![A-Za-z])", line, re.I)]
    assert not offenders, (
        f"`## Unreleased` still mentions the never-shipped role feature: {offenders}")


def test_guide_states_the_never_older_model_fallback():
    """#188: an unserved id takes the same version or newer, else goes as asked."""
    flat = _flatten(GUIDE.read_text(encoding="utf-8"))
    assert "newest served model" not in flat, (
        "docs/guide.md still describes the pre-#188 fallback ('newest served model')")
    assert "never an older one" in flat, (
        "docs/guide.md no longer states that an older model is never substituted")
    assert "through unchanged when none exists" in flat, (
        "docs/guide.md no longer states that an id with no such model goes through unchanged")