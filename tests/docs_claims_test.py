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
Issue #609: f6210cf moved pytest into pyproject.toml's default uv test group,
and INTENT.md's Checks, the guide and ARCHITECTURE.md still taught
`python3 -m pytest`, which no longer finds pytest.
Issue #1037: the add section promised nothing is written outside the state
root and the profile directory, while the first add seeds the shared harness
layer and session history under `~/.claude` for every profile to link into —
the add paragraph must name that shared home it writes.
Issue #1050: README and the guide sold `CCMORPH_REF=main` / `update <ref>` as
installing unreleased code, while install.sh's remote pre-check refuses every
ref download (a GitHub ref archive carries no core binary) — every mention of
the ref form must carry that refusal, as #1035's help already does.
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHANGELOG = ROOT / "CHANGELOG.md"
GUIDE = ROOT / "docs/guide.md"
ARCHITECTURE = ROOT / "docs/ARCHITECTURE.md"
SECURITY = ROOT / ".github/SECURITY.md"
INTENT = ROOT / "INTENT.md"
CONTRIBUTING = ROOT / ".github/CONTRIBUTING.md"

# A `scripts/<name>` the architecture document names, backticked or bare, at a
# path boundary: `scripts/usage-export` matches in a code span and in prose.
# `lib/scripts/x` or `foo-scripts/x` is another directory's path and a name
# boundary keeps it out; a trailing sentence dot is not part of a name.
NAMED_SCRIPT = re.compile(r"(?<![\w/.-])scripts/([\w-]+(?:\.[\w-]+)*)")

# #609: a pytest command the docs teach. The optional `uv run ` prefix is part
# of the match, so `uv run python -m pytest` is one whole match rather than a
# bare `python -m pytest` inside it; only matches without the prefix are stale.
PYTEST_COMMAND = re.compile(r"(?:uv run )?python3? -m pytest")


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


def test_the_full_set_is_one_parallel_command():
    """The one-minute ceiling is a user order (pytest.ini states it). The suites
    work in disjoint scratch homes and mostly wait on process spawns, so `make
    test` runs them at once; chained one after another they take about three
    minutes. A `test` target that lists them, or a doc that chains them with
    `&&`, sends every later change back down the sequential path."""
    rule = re.search(r"^test:(.*)$", (ROOT / "Makefile").read_text(encoding="utf-8"), re.M)
    assert rule and rule.group(1).split() == ["gate"], (
        "Makefile's `test` target must be the parallel gate, not a chain of suites")
    chained = re.compile(r"zsh tests/[\w./]+ &&")
    for doc in (GUIDE, ARCHITECTURE, ROOT / ".github/CONTRIBUTING.md", ROOT / "README.md"):
        assert not chained.search(doc.read_text(encoding="utf-8")), (
            f"{doc} chains the suites one after another; the full set is `make test`")


def test_docs_teach_the_uv_pytest_command_issue_609():
    """#609: f6210cf moved pytest into pyproject.toml's default uv test group,
    so a bare `python3 -m pytest` ends in "No module named pytest" — but
    INTENT.md's Checks and the guide still taught it, and the Checks are what a
    reader runs to verify a promise. Every pytest command a reader is meant to
    type carries `uv run`, and the documents that carry one also carry the lib
    dir beside it, because a Check run without `CCMORPH_LIB_DIR` tests a tree
    the reader's binaries are not in. ARCHITECTURE is scanned but names no
    pytest command: it sends the reader to `make test`. docs/test-runtime.md is
    left out on purpose: it records the commands its measurements ran, not a
    command to type today."""
    for doc in (INTENT, ROOT / "README.md", GUIDE, ARCHITECTURE, CONTRIBUTING):
        flat = _flatten(doc.read_text(encoding="utf-8"))
        bare = [m for m in PYTEST_COMMAND.findall(flat)
                if not m.startswith("uv run python ")]
        assert not bare, (
            f"{doc.relative_to(ROOT)} teaches a pytest command outside uv run: {bare}")
    for doc in (INTENT, GUIDE):
        flat = _flatten(doc.read_text(encoding="utf-8"))
        assert 'CCMORPH_LIB_DIR="$PWD/lib" uv run python -m pytest' in flat, (
            f"{doc.relative_to(ROOT)} no longer names the lib dir beside the pytest command")
    # Every Check line that runs pytest runs it with the lib dir in front, so a
    # reader who copies one command gets the whole command.
    unprefixed = [line for line in INTENT.read_text(encoding="utf-8").splitlines()
                  if "uv run python -m pytest" in line
                  and not all(f'CCMORPH_LIB_DIR="$PWD/lib" {m}'
                              in line for m in PYTEST_COMMAND.findall(line))]
    assert not unprefixed, (
        f"INTENT.md Check lines run pytest without CCMORPH_LIB_DIR: {unprefixed}")


# The receipts folder under both roots, as quota prints and reads it:
# paths.StateDir() is $XDG_STATE_HOME/ccmorph (else ~/.local/state/ccmorph) and
# quota joins "usage" to it (quota/spend.go), so the folder is ccmorph/usage and
# never the XDG root's own usage/. quota/quota_test.go
# TestQuotaTextNamesTheReceiptsFolder pins the printed default footer to
# "receipts: " + RECEIPTS_DEFAULT verbatim (the folder spelled ~/… under the
# home) and the XDG footer to <XDG_STATE_HOME>/ccmorph/usage; these two literals
# are what the docs must carry for the documented and printed paths to be the
# same folder.
RECEIPTS_DEFAULT = "~/.local/state/ccmorph/usage"
RECEIPTS_XDG = "$XDG_STATE_HOME/ccmorph/usage"


def test_docs_name_both_state_roots_issue_731():
    """#731: README called the receipts folder ~/.local/state/ccmorph/usage and
    the state root $CCMORPH_STATE_DIR. Each half was true; read together they
    were one root's promise, so a CCMORPH_STATE_DIR user took receipts to a
    path the tool never writes — the receipts and the quota spend cache resolve
    against the XDG state root ($XDG_STATE_HOME, default ~/.local/state), which
    CCMORPH_STATE_DIR does not move. The documents and the printed paths must
    agree, so each doc names the folder under both roots as the reader builds
    it — `ccmorph/usage` under the XDG root, the same segment the printed
    footer carries — not the XDG root alone, and names the boundary."""
    readme = _flatten((ROOT / "README.md").read_text(encoding="utf-8"))
    guide = _flatten(GUIDE.read_text(encoding="utf-8"))
    for name, flat in (("README.md", readme), ("docs/guide.md", guide)):
        assert RECEIPTS_DEFAULT in flat, (
            f"{name} no longer names the default receipts folder {RECEIPTS_DEFAULT}")
        assert RECEIPTS_XDG in flat, (
            f"{name} does not name the receipts folder under the XDG root as "
            f"{RECEIPTS_XDG} — the folder quota prints and reads")
        assert "CCMORPH_STATE_DIR" in flat, (
            f"{name} no longer names the profile root's variable")
        assert re.search(r"CCMORPH_STATE_DIR[^.]*\bdoes not move\b", flat), (
            f"{name} does not say CCMORPH_STATE_DIR leaves the receipts where they are")


# #1037: what the add paragraph is allowed to promise, and what it must name.
ADD_PROMPT_MARKER = "asks which kind of profile"
# The exclusive shape of the old promise ("Nothing is written outside the
# state root and the profile directory"): any sentence that bounds ALL of
# add's writes to a named set of roots, whatever set it names.
EXCLUSIVE_WRITE_PROMISE = re.compile(r"nothing (?:else )?is written outside", re.I)
# `~/.claude` as its own path token. The lookahead rejects every near-match
# path that is a different file or directory — `~/.claude.json` (the settings
# file), `~/.claude-extra`, the legacy `~/.claude-profiles` — by excluding
# word characters, dots and hyphens after the name; only a real path boundary
# still counts: a deeper path under the home (`~/.claude/projects`) and the
# home itself followed by prose or nothing.
SHARED_HOME = re.compile(r"~/\.claude(?![\w.\-])")


def _add_paragraph(text):
    """The paragraph that walks a person through `ccmorph add`: the blank-line
    block carrying the add prompt flow. Paragraph-scoped, so a `~/.claude`
    mention elsewhere in the guide — the uninstall passage, the sharing
    section — cannot stand in for what the add promise itself says."""
    for para in re.split(r"\n[ \t]*\n", text):
        if ADD_PROMPT_MARKER in para:
            return _flatten(para)
    raise AssertionError(f"docs/guide.md has no paragraph containing {ADD_PROMPT_MARKER!r}")


def test_guide_add_names_the_shared_home_issue_1037():
    """#1037: the add section promised "Nothing is written outside
    ~/.config/ccmorph and the profile directory" while the first add creates
    the shared harness layer and session history under ~/.claude that every
    profile links into (util/setup.go). The paragraph must both drop the
    exclusive promise and name the shared home — each half on its own: a
    paragraph that names ~/.claude beside the old promise fails as surely as
    one that drops the promise while naming only ~/.claude.json, another
    path that native claude shares but that is not the home.
    The uninstall clause must agree with the guide's uninstall passage and
    with install.sh: uninstall removes the command, RETAINS the shared home
    and names the session history it keeps — and it does not leave the home
    untouched, because it repairs plugin index paths under ~/.claude/plugins
    (install.sh, util/misc.go), so an untouched phrasing passes only beside
    that repair named."""
    para = _add_paragraph(GUIDE.read_text(encoding="utf-8"))
    assert not EXCLUSIVE_WRITE_PROMISE.search(para), (
        "docs/guide.md's add paragraph still bounds all of add's writes to a "
        "named set of roots, while the first add seeds the shared Claude Code "
        "home ~/.claude")
    assert SHARED_HOME.search(para), (
        "docs/guide.md's add paragraph names no shared Claude Code home; the "
        "first add creates the shared layer and session history under ~/.claude")
    _, verb, after = para.partition("uninstall")
    assert verb, "docs/guide.md's add paragraph says nothing about uninstall"
    assert re.search(r"\b(?:names?|lists?|reports?)\b.{0,80}"
                     r"\b(?:session history|history\.jsonl|transcripts)\b", after, re.I), (
        "the add paragraph must say uninstall reports the shared session history")
    untouched = re.search(r"\bleaves?\b.{0,65}\b(?:alone|untouched|unmodified|unchanged)\b"
                          r"|\b(?:does not|doesn.t|never) (?:write|touch|change|modify)\b",
                          after, re.I)
    repair = re.search(r"\b(?:repairs?|normalizes?|rewrites?|updates?)\b.{0,35}\bplugin index\b",
                       after, re.I)
    assert not untouched or repair, (
        "uninstall does change the shared home when it repairs the plugin "
        "index; the add paragraph may claim it untouched only beside that repair")
    assert re.search(r"\b(?:keeps?|preserves?|retains?|not remov(?:e|ed)|in place|stays?)\b",
                     after, re.I), (
        "the add paragraph must say uninstall retains the shared home/history")


# GH-1050: the ref form of install and update. install.sh's remote pre-check
# refuses every CCMORPH_REF download — "No ccmorph-core binary … nothing was
# changed" — because a GitHub archive of a branch or tag carries only
# `libexec/.gitkeep`; 43f30fa0 (#1035) put the same fact in the update help.
# The syntax must stay named (it selects the ref, and `ccmorph update <ref>`
# still parses it); what may not return is the promise that such an install
# completes. Both halves of the printed fact must ride every paragraph that
# names the ref form, so a reworded promise cannot slip past on wording.
REF_FORM = re.compile(r"CCMORPH_REF|branch[- ]or[- ]tag|update main", re.I)
# The false spellings landed on fbe4dcd2, pinned as regexes so the test fails
# on that tree whatever else it finds; a reworded promise that drops them must
# still carry the fact halves asserted below.
FALSE_REF_PROMISE = re.compile(r"installs unreleased code|install that instead", re.I)
REF_HAS_NO_CORE = re.compile(r"no ccmorph-core binary", re.I)
REF_NOTHING_CHANGED = re.compile(r"nothing was changed|before anything changes", re.I)


def test_docs_never_promise_a_ref_install_completes_issue_1050():
    """#1050: README and the guide sold `CCMORPH_REF=main` and
    `ccmorph update <branch-or-tag>` as installs of unreleased code, but a ref
    archive carries no ccmorph-core binary, so install.sh refuses every such
    download before changing anything — each of those one-liners was a
    guaranteed refusal. Help learned the truth in #1035; nothing pinned the
    docs, so one rule lived as two spellings. Every paragraph naming the ref
    form must drop the completion promise, carry both halves of what
    install.sh prints, and name the path that works for unreleased code: a
    clone built with `make shim`."""
    for doc in (ROOT / "README.md", GUIDE):
        paragraphs = [para
                      for para in re.split(r"\n[ \t]*\n", doc.read_text(encoding="utf-8"))
                      if REF_FORM.search(para)]
        assert paragraphs, (
            f"{doc.relative_to(ROOT)} no longer names the ref syntax at all; the "
            "honest fix keeps the syntax and states the refusal, it does not "
            "delete the syntax")
        for para in paragraphs:
            flat = _flatten(para)
            assert not FALSE_REF_PROMISE.search(flat), (
                f"{doc.relative_to(ROOT)} still promises a ref install completes: {flat}")
            assert REF_HAS_NO_CORE.search(flat), (
                f"{doc.relative_to(ROOT)} names the ref form without saying a ref "
                f"archive contains no ccmorph-core binary: {flat}")
            assert REF_NOTHING_CHANGED.search(flat), (
                f"{doc.relative_to(ROOT)} names the ref form without saying the "
                f"refused install changes nothing: {flat}")
            assert "make shim" in flat, (
                f"{doc.relative_to(ROOT)} names the ref form without pointing "
                f"unreleased code at a clone built with `make shim`: {flat}")
