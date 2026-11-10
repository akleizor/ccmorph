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
Issue #69: ARCHITECTURE.md described `scripts/usage-summary` — its flags, its
two output files, its run report and day schema — but that script was never
committed to the repo, so every `scripts/` path the document names must exist.
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHANGELOG = ROOT / "CHANGELOG.md"
GUIDE = ROOT / "docs/guide.md"
ARCHITECTURE = ROOT / "docs/ARCHITECTURE.md"
SECURITY = ROOT / ".github/SECURITY.md"

# A `scripts/<name>` the architecture document names, backticked or bare, at a
# path boundary: `scripts/usage-export` matches in a code span and in prose.
# `lib/scripts/x` or `foo-scripts/x` is another directory's path and a name
# boundary keeps it out; a trailing sentence dot is not part of a name.
NAMED_SCRIPT = re.compile(r"(?<![\w/.-])scripts/([\w-]+(?:\.[\w-]+)*)")


def _flatten(text):
    return re.sub(r"\s+", " ", text)


def _unreleased():
    text = CHANGELOG.read_text(encoding="utf-8")
    # The newest section: Unreleased while work is in flight, the release it became after.
    match = re.search(r"^## (?:Unreleased|v\d[^\n]*)\n(.*?)(?=^## )", text, re.M | re.S)
    assert match, "CHANGELOG.md has no `## Unreleased` or `## v...` section"
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


def test_unreleased_has_at_most_one_changed_heading():
    headings = [heading for heading, _ in _sections(_unreleased()) if heading]
    # A release empties Unreleased, so zero is valid; two is the defect.
    assert headings.count("Changed") <= 1, (
        f"`## Unreleased` must hold at most one `### Changed`: {headings}")


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
    that shipped are joined by an underscore (`CCMORPH_ROLE`, `role=`,
    `x-ccmorph-role`) where `\\b` finds no boundary. Spliced so this file is not
    a hit for the name it guards; an entry that later needs the word for a message
    role should say so in a narrower guard rather than widen this one."""
    stale = "ro" "le"
    offenders = [line.strip() for line in _unreleased().splitlines()
                 if re.search(rf"(?<![A-Za-z]){stale}s?(?![A-Za-z])", line, re.I)]
    assert not offenders, (
        f"`## Unreleased` still mentions the never-shipped role feature: {offenders}")


def test_guide_states_the_model_fallback_and_where_a_token_comes_from():
    """#188: an unserved id takes the same version or newer, else goes as asked.
    #290: the guide called a token profile 'a long-lived OAuth token' and never
    said where one comes from, so a reader with no token could not get one. One
    passage must carry the whole flow: the command that makes a token, what a
    valid one starts with, and the `ccmorph add` step that pastes it."""
    flat = _flatten(GUIDE.read_text(encoding="utf-8"))
    assert "newest served model" not in flat, (
        "docs/guide.md still describes the pre-#188 fallback ('newest served model')")
    assert "never an older one" in flat, (
        "docs/guide.md no longer states that an older model is never substituted")
    assert "through unchanged when none exists" in flat, (
        "docs/guide.md no longer states that an id with no such model goes through unchanged")
    assert re.search(r"claude setup-token.{0,400}sk-ant-oat01-", flat), (
        "docs/guide.md names no command that makes a token beside what a valid one starts with")
    assert re.search(r"sk-ant-oat01-.{0,300}Claude token", flat), (
        "docs/guide.md never ties a made token to the menu choice that adds it")


def test_architecture_names_only_scripts_that_exist_issue_69():
    """#69: the architecture document described `scripts/usage-summary` in detail,
    but that script was never committed, so a reader following the document found
    nothing to run. Every `scripts/` path the document names must be a file the
    repo ships, whatever the sentence around it claims about the command."""
    named = set(NAMED_SCRIPT.findall(ARCHITECTURE.read_text(encoding="utf-8")))
    missing = sorted(name for name in named if not (ROOT / "scripts" / name).is_file())
    assert not missing, (
        f"ARCHITECTURE.md names scripts/ paths the repo does not ship: {missing}")



def test_security_tells_one_credential_storage_story_issue_262():
    """#262: SECURITY.md's table said the macOS key and token move to the login
    keychain while a later section denied any keychain integration — a reader
    could not tell where a key would live. The one story, both halves: the
    login keychain on macOS when there is one, the conf file otherwise, and
    what each does and does not defend against."""
    flat = _flatten(SECURITY.read_text(encoding="utf-8"))
    assert "There is no keychain integration" not in flat, (
        "SECURITY.md still denies the keychain integration the product has")
    assert "Credentials are stored unencrypted" not in flat, (
        "SECURITY.md still states the pre-keychain blanket claim")
    assert "move to the login keychain on first use" in flat, (
        "SECURITY.md no longer states the macOS keychain half of the story")
    assert "no login keychain" in flat, (
        "SECURITY.md no longer states the fallback half: with no login keychain "
        "the credential stays in the conf")
    assert "not a boundary inside your own session" in flat, (
        "SECURITY.md no longer says what the login keychain does not defend against")


def test_docs_name_the_provider_type_and_its_dependency_issue_283():
    """#283: `provider` was in the `.conf` field table but missing from Profile
    types, and neither README nor guide said that the browser logins need the
    optional cliproxyapi. A person without it never learned they exist.

    #366: the two Kimi logins are named by region, with the same names the
    sign-in menu and the API-key provider list use, so each doc names both
    services instead of one plain Kimi."""
    logins = ("Antigravity", "Codex", "Devin",
              "Kimi For Coding (global)", "Kimi For Coding (China)", "Meta", "xAI")
    for doc in (ROOT / "README.md", GUIDE):
        flat = _flatten(doc.read_text(encoding="utf-8"))
        assert "`provider`" in flat, f"{doc.name} does not name the provider profile type"
        assert "cliproxyapi" in flat, f"{doc.name} does not name the program provider profiles need"
        for login in logins:
            assert login in flat, f"{doc.name} does not name the {login} browser login"
    guide = GUIDE.read_text(encoding="utf-8")
    profile_types = re.search(r"^## Profile types\n(.*?)(?=^## )", guide, re.M | re.S)
    assert profile_types, "docs/guide.md has no Profile types section"
    assert "| `provider` |" in _flatten(profile_types.group(1)), (
        "the Profile types table in docs/guide.md leaves out `provider`")
    assert "cliproxyapi" in _flatten(profile_types.group(1)), (
        "the Profile types table does not say a provider profile needs cliproxyapi")


def test_docs_name_the_setting_effort_alone_issue_367():
    """#367: the config menu, its help and the README called the effort setting
    "reasoning" while set, get, inspect and the errors say effort, so a person
    who learned the word from the menu typed it into set and met a usage line.
    One word names the setting everywhere a person can read it. The guide keeps
    "reasoning" as the plain noun (the reasoning behind a behaviour), so only
    the phrases that name the setting are pinned."""
    readme = _flatten((ROOT / "README.md").read_text(encoding="utf-8")).lower()
    assert "change model, effort, or connection" in readme, (
        "README.md no longer names effort on the config line")
    assert "model, reasoning, or connection" not in readme, (
        "README.md still calls the effort setting reasoning")
    guide = _flatten(GUIDE.read_text(encoding="utf-8")).lower()
    assert "model and effort" in guide, (
        "docs/guide.md no longer names model and effort as what config changes")
    for phrase in ("model and reasoning", "reasoning levels"):
        assert phrase not in guide, (
            f"docs/guide.md still calls the effort setting reasoning ('{phrase}')")


def test_readme_says_what_a_status_line_of_your_own_shows_issue_393():
    """#393: the README said the session's status line names the profile, in
    the highlights and under Usage, but a status line of your own is kept as
    written and never shows the name, and the startup message that names it
    scrolls away. Every README block that promises the name in the status line
    says what a status line of your own shows and names the one step that
    prints the profile at any point."""
    text = (ROOT / "README.md").read_text(encoding="utf-8")
    # A paragraph or one list item: the highlights are a list with no blank lines.
    blocks = [_flatten(b).lower() for b in re.split(r"\n\s*\n|\n(?=- )", text)]
    promises = [b for b in blocks if "status line names the profile" in b]
    assert promises, "README.md no longer says the status line names the profile"
    for block in promises:
        assert "status line of your own" in block, (
            f"README.md promises the name in the status line without saying "
            f"what a status line of your own shows: {block}")
        assert "/ccmorph:profile" in block, (
            f"README.md names no step that prints the profile beside: {block}")
