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
