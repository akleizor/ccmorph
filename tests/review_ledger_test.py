"""The nightly ledger promoter reads this repo's mistake ledger by path and by
regex: `git show origin/main:REVIEW.md`, and a kind paragraph must match
`^(?:\\*\\*(\\d+)\\.\\s+|(\\d+)\\.\\s+\\*\\*)([^*]+?)\\*\\*` to be counted,
which its incident dates (`Seen <n>× (<dates>).`) then feed the 3-distinct-days
promotion to a gate-rule issue.

GH-916: #21 (4ae8a278) moved REVIEW.md to `.github/` and its kinds to
unnumbered `**Title** —` bullets; the promoter found no REVIEW.md at the root,
so for ccmorph the ledger was permanently empty and repeated mistakes never
became gate rules. This guard runs the promoter's own path and regex against
this checkout, so a future move or format drift fails here, in `make gate`,
instead of silently deadening the ledger.
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
LEDGER = ROOT / "REVIEW.md"
MOVED_AWAY = ROOT / ".github" / "REVIEW.md"

# The promoter's kind regex (lessons:78), copied verbatim: a kind paragraph the
# promoter cannot parse counts as nothing, which is the silent failure this
# guard exists to make loud.
KIND = re.compile(r"^(?:\*\*(\d+)\.\s+|(\d+)\.\s+\*\*)([^*]+?)\*\*")

# A paragraph starts like a kind when a bold span opens it, wherever the line
# starts and however the bullet is spelled: the unnumbered `- **Title** —`
# bullet is the exact shape #21 left behind, and any other list spacing
# (`-  **Title**`, `+ **Title**`, an indented bold paragraph) is the same dead
# ledger the promoter cannot parse. Any line this matches that fails KIND is
# named as a malformed kind paragraph.
STARTS_LIKE_A_KIND = re.compile(r"^\s*(?:[-*+]\s+)?\*\*")

# Any Seen-ish line — the promoter counts dates only from a Seen line attached
# to a kind paragraph it can parse, so a Seen line beside an unparseable kind,
# before any kind, or after the last kind's section, counts as nothing.
SEEN_MARKER = re.compile(r"Seen \d")

# The Seen line is the ledger's count of incident dates; the promoter counts
# distinct days from it, so a paragraph without exactly one, or with an
# undated or letter-x spelling, is a kind that cannot reach promotion.
SEEN_STRICT = re.compile(r"Seen (\d+)× \(([^()]*\d{4}-\d{2}[^()]*)")
SEEN_LOOSE = re.compile(r"Seen \d+[×x]")

# The kinds GH-916 found sitting at the promotion threshold, with the dates
# each was seen. These are recorded incidents, so a later Seen line may add a
# date but may never lose one of these.
DUE = {
    'A deadline only cooperative checks enforce':
        ['2026-11-13', '2026-11-20', '2026-11-24'],
    'A second implementation of a rule the codebase already owns':
        ['2026-11-16', '2026-11-21', '2026-11-24'],
    'A reader that snapshots the names it opens later':
        ['2026-11-14', '2026-11-16', '2026-11-17'],
}


def _lines():
    return LEDGER.read_text(encoding='utf-8').split('\n')


def paragraphs():
    """(section, kind line number, title, paragraph text) for every line that
    starts like a kind paragraph, parsed or not; a paragraph runs to the next
    kind line or section heading, so its Seen line — on its own line or at the
    end of the kind's own line — belongs to it."""
    lines = _lines()
    starts = [i for i, line in enumerate(lines)
              if KIND.match(line) or STARTS_LIKE_A_KIND.match(line)]
    out = []
    for j, i in enumerate(starts):
        end = starts[j + 1] if j + 1 < len(starts) else len(lines)
        block = lines[i:end]
        stop = next((x for x, b in enumerate(block[1:], 1) if b.startswith('## ')),
                    len(block))
        section = next((b[3:] for b in reversed(lines[:i]) if b.startswith('## ')), '')
        out.append((section, i + 1, KIND.match(lines[i]), '\n'.join(block[:stop])))
    return out


def test_the_ledger_sits_where_the_promoter_reads_it():
    # cause: the promoter runs `git show origin/main:REVIEW.md`; any other
    # location reads as no ledger, and the whole file counts as zero kinds.
    assert LEDGER.is_file(), (
        'cause: REVIEW.md is not at the repository root, where the nightly '
        'ledger promoter reads it (`git show origin/main:REVIEW.md`). '
        'fix: `git mv <wherever it is> REVIEW.md`. (GH-916: #21 moved it to '
        '.github/ and the ledger went silently empty.)')
    assert not MOVED_AWAY.exists(), (
        'cause: REVIEW.md exists at .github/REVIEW.md again beside the root '
        'file, so a reader (or a future mover) can pick the copy the promoter '
        'never reads. fix: keep one copy, at the root. (GH-916)')


def test_every_kind_paragraph_parses_with_the_promoters_regex():
    parsed, unparsed = [], []
    for _, number, m, _ in paragraphs():
        (parsed if m else unparsed).append((number, _lines()[number - 1][:80]))
    assert parsed, (
        'cause: no kind paragraph in REVIEW.md matches the promoter regex '
        '`^(?:\\*\\*(\\d+)\\.\\s+|(\\d+)\\.\\s+\\*\\*)([^*]+?)\\*\\*`, so the '
        'ledger counts nothing. fix: number each kind in the form '
        '`**N. Title**`. (GH-916)')
    assert not unparsed, (
        'cause: kind paragraphs the promoter regex cannot parse (an unnumbered '
        '`**Title**` paragraph, or a kind bullet in any spelling — '
        f'`- **Title**`, `-  **Title**`, `+ **Title**`): {unparsed}. '
        'fix: number each as `**N. Title**`; the kind takes the next number in '
        'its section, and kinds that stay keep theirs (kind numbers are '
        'identity). (GH-916)')


def test_kind_numbers_are_identity_within_each_section():
    # A promotion removes a kind paragraph and leaves its number's gap: the
    # ledger keeps gaps and loses promoted entries, so the guard demands only
    # that within a section every number names one kind and they rise with the
    # file — never renumbered, never reused, never out of order.
    by_section = {}
    for section, _, m, _ in paragraphs():
        if m:
            by_section.setdefault(section, []).append(int(m.group(1) or m.group(2)))
    assert by_section, 'no numbered kind paragraphs'
    for section, numbers in by_section.items():
        assert numbers[0] >= 1, (
            f'cause: a kind number in section {section!r} is not a positive '
            'number. fix: kind numbers start at 1. (GH-916)')
        assert all(a < b for a, b in zip(numbers, numbers[1:])), (
            f'cause: kind numbers in section {section!r} are not strictly '
            'increasing, so a number names more than one kind or arrives out '
            'of order. fix: a new kind takes the next number in its section, '
            'and when a promoted paragraph leaves the file the gap stays — '
            'no renumbering, numbers are identity. (GH-916)')


def test_every_kind_paragraph_carries_one_strict_seen_line():
    # the strict form is `Seen <n>× (<dates>).` with the multiplication sign;
    # the promoter counts distinct days from those dates. And every Seen-ish
    # line must attach to a kind paragraph the promoter can parse: a Seen line
    # beside an unparseable kind, before the first kind, or after the last
    # kind's section counts for nothing, so the guard says so.
    bad, orphaned = [], []
    lines = _lines()
    owners = []  # per line: True if inside a parsed kind paragraph, else False
    parsed = None
    for i, line in enumerate(lines):
        if STARTS_LIKE_A_KIND.match(line) or KIND.match(line):
            parsed = bool(KIND.match(line))
        elif line.startswith('## '):
            parsed = None
        owners.append(parsed)
    for _, number, m, text in paragraphs():
        if not m:
            if SEEN_LOOSE.search(text) or SEEN_MARKER.search(text):
                orphaned.append((number, text.splitlines()[0][:60]))
            continue
        strict = SEEN_STRICT.findall(text)
        loose = SEEN_LOOSE.findall(text)
        if len(strict) != 1 or len(loose) != len(strict):
            bad.append((number, m.group(3)[:60], text.count('Seen')))
    for i, line in enumerate(lines):
        if SEEN_MARKER.search(line) and owners[i] is not True:
            orphaned.append((i + 1, line.strip()[:60]))
    assert not bad, (
        f'cause: kind paragraphs without exactly one dated `Seen <n>× '
        f'(<dates>).` line (line, title, Seen-marker count): {bad}. fix: each '
        'kind carries one Seen line naming the dates of its incidents — '
        'undated, doubled, or letter-x spellings count as nothing to the '
        'promoter. (GH-916)')
    assert not orphaned, (
        'cause: Seen lines attached to no parseable kind paragraph (before any '
        f'kind, beside an unparseable one, or past the last kind): {orphaned}. '
        'fix: number the kind the Seen line counts for as `**N. Title**`, or '
        'remove the Seen line with its kind. (GH-916)')


# The record line as the lessons ledger must re-count it: the record alone
# closes the line, and its parentheses hold nothing but full dates.
SEEN_RECORD = re.compile(r"Seen (\d+)× \(([^()]*)\)\.$")
FULL_DATE = re.compile(r"\d{4}-\d{2}-\d{2}")


def test_every_seen_record_says_exactly_what_the_ledger_counts():
    # GH-1017's landing refusal: kind 1's record read
    # `Seen 2× (2026-10, 2026-10 — …predate the 2026-10-26 history squash…)`,
    # while `lessons ccmorph` printed `Seen 1x 2026-10-26` for it — the ledger
    # re-counts each kind from the full `YYYY-MM-DD` dates on the Seen record,
    # as distinct days, and ignores everything the record cannot be a date in.
    # So a record with prose or month-only tokens inside its parentheses, a
    # note sharing the record's line before or after it, a repeated date
    # entry, or a count above its distinct dates is REVIEW.md stating a fact
    # the ledger will not print — the same mistake the caught kind is about
    # (a reader turning an unparseable shape into a confident count), written
    # into the ledger's own input. A kind's dating notes belong in the
    # paragraph text; the record line states only the days.
    bad = []
    for i, line in enumerate(_lines(), 1):
        if not SEEN_MARKER.search(line):
            continue
        rec = SEEN_RECORD.search(line)
        if rec is None or line[:rec.start()].strip() or line[rec.end():].strip():
            bad.append((i, 'the Seen marker is not the whole record '
                           '`Seen <n>× (<dates>).` closing its line — prose '
                           'shares the line, and one reader counts it while '
                           'the ledger does not'))
            continue
        inner = rec.group(2)
        dates = FULL_DATE.findall(inner)
        if ', '.join(dates) != inner:
            bad.append((i, f'the record holds more than dates as `d1, d2, …`: '
                           f'{inner!r} — the ledger counts only full '
                           '`YYYY-MM-DD` days from it'))
        elif dates != sorted(dates) or len(set(dates)) != len(dates):
            bad.append((i, f'the record\'s dates repeat or are out of order: '
                           f'{inner!r} — the ledger keeps distinct days'))
        elif int(rec.group(1)) != len(dates):
            bad.append((i, f'the record counts {rec.group(1)} but lists '
                           f'{len(dates)} distinct days: {inner!r} — the '
                           'ledger prints the number of days it holds'))
    assert not bad, (
        f'cause: Seen records the lessons ledger will count differently than '
        f'they read (line, defect): {bad}. fix: a record line is only '
        '`Seen <n>× (<date>, <date>, …).` — full `YYYY-MM-DD` dates, one per '
        'distinct day, ascending, and n their number; move dating notes into '
        'the kind\'s paragraph text, which the ledger does not count. '
        '(GH-1017)')


def test_the_promotion_threshold_records_keep_their_dates():
    # GH-916's three Seen-3× kinds are recorded incidents. The live ledger may
    # promote one: its paragraph then leaves the kinds section (its number's
    # gap stays), and that is the ledger working, not a failure. So the guard
    # tolerates a fully gone kind but never a kind left with its dates
    # under-reported — a promotion must count what was actually seen.
    seen_by_kind = {m.group(3): SEEN_STRICT.findall(text)[0][1]
                    for _, _, m, text in paragraphs()
                    if m and SEEN_STRICT.findall(text)}
    for kind, dates in DUE.items():
        entry = next((v for k, v in seen_by_kind.items() if k.startswith(kind)), None)
        if entry is None:
            continue  # promoted or otherwise gone: the file lost the whole
            # paragraph, which the promoter itself performs on promotion.
        for date in dates:
            assert date in entry, (
                f'cause: kind {kind!r} lost recorded incident date {date} from '
                f'its Seen line ({entry!r}), under-reporting a promoted kind. '
                'fix: Seen lines record incidents; add dates, never drop them.')


def test_the_header_records_the_no_renumber_process():
    # The issue requires the file header to record: kind numbers are identity —
    # when a promoted paragraph leaves the file, the remaining kinds keep their
    # numbers (no renumber).
    header = []
    for line in _lines():
        if line.startswith('## '):
            break
        header.append(line)
    header = '\n'.join(header)
    assert ('identity' in header and 'promoted' in header
            and 'keep their numbers' in header and 'no renumber' in header), (
        'cause: the file header no longer records the process rule that kind '
        'numbers are identity — that when a promoted paragraph leaves the '
        'file, the remaining kinds keep their numbers (no renumber). fix: '
        'restore that note to the header block above the first section. '
        '(GH-916)')


def test_the_two_sections_name_the_promoter_that_writes_promotions():
    # Each of the two kind sections records that the nightly promoter is now
    # the writer of a promotion: the failure-kinds section says a Seen entry
    # whose dates reach 3 distinct days opens a gate-rule issue; the Gate rules
    # section says the promoter's date opens the rules it files. The ledger's
    # location is test_the_ledger_sits_where_the_promoter_reads_it's claim;
    # this guard speaks about the two sections, so with no ledger to hold a
    # section it has nothing to say — the location guard already failed that
    # tree.
    if not LEDGER.is_file():
        return
    sections, current = {}, None
    for line in _lines():
        if line.startswith('## '):
            current = line[3:]
            sections[current] = []
        elif current is not None:
            sections[current].append(line)
    kinds = '\n'.join(sections.get('What failure looks like here', []))
    gate = '\n'.join(sections.get('Gate rules', []))
    assert kinds and gate, 'the ledger lost one of its two kind sections'
    assert ('nightly' in kinds and 'promoter' in kinds
            and 'Seen' in kinds and '3 distinct days' in kinds
            and 'gate-rule issue' in kinds), (
        "cause: the failure-kinds section does not record that the nightly "
        "ledger promoter writes promotions — that a Seen entry whose dates "
        "reach 3 distinct days opens a gate-rule issue. fix: add that entry "
        "below the section heading. (GH-916)")
    assert ('nightly' in gate and 'promoter' in gate and 'date' in gate), (
        'cause: the Gate rules section does not record that the nightly '
        'promoter opens its rules and that each rule carries the promoter\'s '
        'date. fix: add that entry below the section heading. (GH-916)')


# Each kind's number and title as GH-916 leaves them, per section. Kind numbers
# are identity (REVIEW.md's header says so): a promotion removes a paragraph
# and leaves its number as a gap, a new kind takes a number above the section's
# highest, and a surviving kind keeps its number and title. This record makes
# the gate say so: moving a survivor to another number, shifting the survivors
# down a gap, reusing a retired number, or rewriting a title beyond
# capitalisation fails, naming the section, the number and both titles.
RECORDED = {
    'What failure looks like here': {
        1: 'Backend, identity or credential env leaking into a launch or `claude auth` call',
        2: "A nested launch inheriting the parent session's state",
        3: 'One reader skipping the merged view',
        4: "The user's choice silently overruled",
        5: 'Signals or exit status lost between launcher and claude',
        6: 'A shim rewrite corrupting the forwarded body',
        7: 'A loopback hop or catalogue read with no bound',
        8: 'A runtime floor or upstream CLI contract assumed',
        9: 'A redaction helper whose error path returns the secret it was meant to hide',
        10: 'A deadline only some documented paths arm',
        11: 'An observer still looking for the child process after the component moved in-process',
        12: 'A fix wired into the entry point the default path no longer takes',
        13: 'A test that cannot fail',
        14: 'A new writer reusing a field whose contract says otherwise',
        15: 'A ported normalizer whose search string equals its replacement',
        16: 'A deadline only cooperative checks enforce',
        17: 'Timer semantics swapped in a port',
        18: 'A per-wait timer that forgets one of the waits it names',
        19: 'A cleanup gated on a handle its own helper clears first',
        20: 'A path check that forgets a link can sit on a parent directory',
        21: 'An upgrade deleting a file a running process still references',
        22: 'An upgrade changing the argv contract a running launcher still uses',
        23: 'A reader that turns unparseable input into a confident verdict',
        24: 'A diagnostics path that can die instead of degrade',
        25: 'A damage guard not extended to the sibling field the same change added',
        26: 'A value filter that rejects the very values it exists to catch',
        27: 'A length floor counted in bytes where the contract says characters',
        28: 'A stream restorer that skips the final unterminated event',
        29: 'A held join flushed by an unrelated event',
        30: 'A masker that descends deeper than its restorer',
        31: 'A secret file created empty and filled by a second call',
        32: 'A keychain lookup by service alone where the service is shared across items',
        33: 'A byte stripped by one reader and saved back by another writer of the same file',
        34: 'A confident verdict kept for a shape the sibling fix already made uncertain',
        35: 'A fallback home that only some path builders use',
        36: "A fixture whose mode the runner's umask can mask away",
        37: 'A drop-in replacement that ships without the test pinning the old contract',
        38: 'A fixed-port test drawing its number from the ephemeral pool every parallel gate process draws from',
        39: 'A busy check sampled before the racer it guards against can register',
        40: 'A shared file the feature depends on that nothing ever creates',
        41: 'A repair that unlinks the shared file before its replacement exists',
        42: "A store's reader disagreeing with its own writer about the store",
        43: 'A unit test that walks into a background fetch',
        44: 'A sweep that deletes what its walk failed to see',
        45: 'A dangling-link check that reads every stat error as dangling',
        46: 'A command matched by a substring of what ccmorph itself builds',
        47: 'A refusal meant for the launch path, wired into verbs that start no session',
        48: 'A background refresher with no hang deadline that re-runs a cheap check every cycle',
        49: 'Evidence consumed before the decision that needs it, beside a tail window that can skip the line that matters',
        50: 'A verdict line reports ready without testing the thing it vouches for',
        51: 'A stop recorded without waiting for the process to be gone',
        52: 'A two-step migration that ignores the failure between its steps while its readers trust it',
        53: 'A refusal that names the cause but not the repair its channel promises',
        54: 'One table of a set gaining an entry the others miss',
        55: "A wrapper that names its neighbour from the link's directory instead of the script's",
        56: "A formatter's drift the gate never runs",
        57: 'A test whose lens reaches outside the tree it guards',
        58: 'A reader that snapshots the names it opens later, beside a writer that renames between the two',
        59: "A second implementation of a rule the codebase already owns, ported without its owner's edge cases",
        60: 'A fail-closed guard not told about a newly-legitimate path, when the guard matches by destination and the new path shares that exact destination',
        61: 'A change landed with the suite already red, and the gate landed more changes on top of red main',
        62: "A store's naming or identity rule changed under rows already on disk",
        63: "A long-lived supervisor on a one-shot helper's refusal path, or opening a path no run has created yet",
        64: "A test that pins an exact count at a stub shared across tests while an earlier test's killed client can still deliver",
        65: 'A trust boundary crossed on a predictable path in a shared temporary directory',
        66: 'A read-modify-write of shared state shipped without the lock its siblings use',
    },
    'Gate rules': {
        1: 'An override one reader honours and another forgets',
        2: 'A fallback home for private data that no sweeper owns',
        3: 'A suite green on the gate and red on a machine that carries the real tool',
        4: 'A shared resource torn down or doubled on transient evidence, with state files that outlive their owner',
    },
}


def test_a_surviving_kind_keeps_its_number_and_title():
    # A promotion deletes a paragraph — its number becomes a gap — and a new
    # kind is appended above the section's highest number; both pass. A
    # renumbering fails: a survivor moved to another number, survivors shifted
    # down a gap, a retired number reused, or a title rewritten (a case-only
    # wording fix like `claude` -> `Claude` still counts as the same kind).
    if not LEDGER.is_file():
        return  # the location guard fails that tree; a missing file is not a renumber
    live = {}
    for section, _, m, _ in paragraphs():
        if m:
            live.setdefault(section, {})[int(m.group(1) or m.group(2))] = m.group(3).strip()
    problems = []
    for section, recorded in RECORDED.items():
        present = live.get(section, {})
        highest = max(recorded)
        for number, title in recorded.items():
            found = present.get(number)
            if found is not None and found.casefold() == title.casefold():
                continue
            if found is not None:
                problems.append(
                    f"section {section!r} number {number}: recorded title "
                    f"{title!r}, found {found!r} — a recorded number now holds a "
                    'different kind')
                continue
            # Gone from its number: a promotion is the ledger working, but only
            # if the same kind is not sitting at some other number instead.
            moved = sorted(n for n, t in present.items()
                           if t.casefold() == title.casefold())
            if moved:
                problems.append(
                    f"section {section!r} number {moved[0]}: recorded kind "
                    f"{title!r} (number {number}) found at another number")
        for number, title in sorted(present.items()):
            if number not in recorded and number <= highest:
                problems.append(
                    f"section {section!r} number {number}: kind {title!r} fills a "
                    f"number at or below the highest recorded ({highest}) that "
                    'records no kind — a retired number reused')
    for section in live:
        if section not in RECORDED:
            problems.append(
                f"section {section!r}: kind paragraphs outside the two recorded "
                'sections')
    assert not problems, (
        'cause: kind numbers and titles are identity, and the ledger renumbered '
        'or reused them:\n  - ' + '\n  - '.join(problems) + '\nfix: a promoted '
        'kind leaves its number as a gap; a new kind takes the next number above '
        "the section's highest; a surviving kind keeps its number and title. "
        '(GH-916)')
