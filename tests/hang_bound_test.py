"""One hang bound for the suite, and a check that no test file invents its own.

A literal `timeout=` on a spawned process — or on a socket read from one that a
test started — competes with an unbounded queue rather than detecting a hang, so
a healthy run on a loaded host goes red. So does a budget spelled as a count of
sleeps: a loop that runs `range(N)` times in python, or counts `{1..N}` in a
zsh suite, and sleeps each pass holds a fixed wall-clock budget a loaded host
can exceed. Every such wait reads `support.HANG_TIMEOUT` instead, and so does
every test file that spawns a process; the zsh suites' embedded python obeys
the same rule through PYTHONPATH, and these checks hold that line.

GH-976 adds the other half: the 60 s ceiling is a verdict about the host, so a
test that really starts a child has its own timer extended to HANG_TIMEOUT +
SPAWN_SLACK at that first spawn (conftest.py), and an explicit marker below
that budget is a teardown verdict rather than a grant. The timer moves before
the call that can create the child, and a rejected call puts back what was
left. The probe below runs the
real conftest in an inner pytest and proves both directions, and the literal-
marker scan keeps a stale number from coming back.
"""
import ast
import datetime as dt
import fcntl
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest

from support import CORE, HANG_TIMEOUT, SPAWN_SLACK, SPAWN_TEST_BUDGET, store_month_first

TESTS = Path(__file__).resolve().parent

# GH-976: this file parses every test file on each check and starts nothing, so
# the grant below (which extends a test's timer at its first real spawn) cannot
# reach it — and the night gate starved exactly
# `test_no_literal_process_bound_in_the_suite`, the guard that enforces the
# bound rule, past the 60 s ceiling. The meta-guard carries the same budget it
# enforces on every other test as a file-level marker.
pytestmark = pytest.mark.timeout(SPAWN_TEST_BUDGET)

# Calls whose `timeout=` waits on a real process, or on a socket served by one.
# Thread waits (Barrier, join) and values handed to production code are
# deliberately absent: neither waits on a spawned process.
PROCESS_WAITS = {"run", "check_output", "check_call", "call", "wait", "communicate", "run_pty"}
SOCKET_WAITS = {"urlopen", "open", "HTTPConnection", "HTTPSConnection",
                "open_connection", "create_connection", "keepalive_post", "read_stream"}
WAITS = PROCESS_WAITS | SOCKET_WAITS

# The synchronous spawners: they take `timeout=` and finish on their own. `Popen`
# does not (its bound belongs on the wait that follows it) and `os.system` cannot.
RUN_CALLS = {"run", "check_output", "check_call", "call"}

# Every call that starts a real process: those, the `os.*` spawners and
# `pty.spawn`, plus the two bare names the suite uses for a process it starts
# itself. A local helper named `run` is not one of these.
SPAWN_CALLS = RUN_CALLS | {"Popen", "system", "spawnv", "spawnvp", "spawnl",
                           "spawnle", "posix_spawn", "run_pty"}
SPAWN_MODULES = ("subprocess", "os", "pty")
BARE_SPAWNS = {"Popen", "run_pty"}

# Every clock in the time module: a deadline written as `<clock>() + N` holds
# the same literal budget whichever one it reads, so they are named together.
# Bare names match module functions (`time.time`) and `from time import ...`
# alike — `_callee` reads the last part.
CLOCK_CALLS = {"monotonic", "monotonic_ns", "time", "time_ns",
               "perf_counter", "perf_counter_ns"}


def _names(func):
    """('subprocess', 'run') for `subprocess.run`; ('run_pty',) for a bare name."""
    parts = []
    while isinstance(func, ast.Attribute):
        parts.append(func.attr)
        func = func.value
    if isinstance(func, ast.Name):
        parts.append(func.id)
    return tuple(reversed(parts))


def _callee(func):
    names = _names(func)
    return names[-1] if names else None


def _is_spawn(func):
    names = _names(func)
    if not names or names[-1] not in SPAWN_CALLS:
        return False
    if len(names) == 1:
        return names[-1] in BARE_SPAWNS
    return names[0] in SPAWN_MODULES


def _is_number(node):
    return (isinstance(node, ast.Constant) and isinstance(node.value, (int, float))
            and not isinstance(node.value, bool))


def _sleeps(node):
    """True when a `sleep` call runs somewhere inside `node`."""
    return any(isinstance(n, ast.Call) and _callee(n.func) == "sleep"
               for n in ast.walk(node))


def literal_process_bounds(tree):
    """Every wait in `tree` bounded by a number the test file itself picked:
    a `timeout=` on a process or socket wait, a `<clock>() + N` deadline on any
    clock the time module offers, a helper's `timeout=` default, or a counted
    sleep loop — `for ... in range(...)` with a `sleep` in the body, a budget
    spelled as a count times a sleep."""
    offenders = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Call):
            if _callee(node.func) in WAITS:
                if any(k.arg in ("timeout", "connect_timeout") and _is_number(k.value)
                       for k in node.keywords):
                    offenders.append(node.lineno)
        elif (isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add)
              and isinstance(node.left, ast.Call) and _callee(node.left.func) in CLOCK_CALLS
              and _is_number(node.right)):
            offenders.append(node.lineno)
        elif (isinstance(node, (ast.For, ast.AsyncFor))
              and isinstance(node.iter, ast.Call) and _callee(node.iter.func) == "range"
              and any(_sleeps(stmt) for stmt in node.body)):
            offenders.append(node.lineno)
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            defaults = list(zip(node.args.args[len(node.args.args) - len(node.args.defaults):],
                                node.args.defaults))
            defaults += [(a, d) for a, d in zip(node.args.kwonlyargs, node.args.kw_defaults)
                         if d is not None]
            if any(a.arg in ("timeout", "connect_timeout") and _is_number(d) for a, d in defaults):
                offenders.append(node.lineno)
    return offenders


def spawns_a_process(tree):
    """True when this module starts a real process somewhere in the file."""
    return any(isinstance(node, ast.Call) and _is_spawn(node.func) for node in ast.walk(tree))


def unbounded_spawns(tree):
    """`subprocess.run/check_output/check_call/call` calls with no `timeout=`."""
    offenders = []
    for node in ast.walk(tree):
        if (isinstance(node, ast.Call) and _is_spawn(node.func) and _callee(node.func) in RUN_CALLS
                and not any(k.arg == "timeout" for k in node.keywords)):
            offenders.append(node.lineno)
    return offenders


_SCAN = None


def _scan():
    """Read and parse each tests/*.py once; the checks below share the result.

    The reach-second check is asked of every Python file under tests/, not only
    the *_test.py suite files: the rule names `tests/*.py`, and a helper or a
    fixture can carry the literal a test file would. It reads each line's text
    and, when the file parses, each string literal as Python's parser reads it,
    so a literal wrapped across lines is caught; a file that does not parse
    keeps the line reading. A suite file that does not parse still yields its
    reach diagnostic, and is recorded as broken so the process-bound checks
    fail on it as they did before the reach check shared this scan; they stay
    on the suite files they govern and keep the AST.
    """
    literals, silent, unbounded, reach, broken = [], [], [], [], []
    for path in sorted(TESTS.glob("*.py")):
        text = path.read_text()
        reach += [f"{path.name}:{line}" for line in reach_seconds(text)]
        if not path.name.endswith("_test.py"):
            continue
        try:
            tree = ast.parse(text)
        except (SyntaxError, ValueError) as exc:
            # Not skipped: pytest may never collect this file (conftest's
            # collect_ignore), so the process checks below name it instead.
            broken.append(f"{path.name}: {exc}")
            continue
        literals += [f"{path.name}:{line}" for line in literal_process_bounds(tree)]
        if "HANG_TIMEOUT" not in text and spawns_a_process(tree):
            silent.append(path.name)
        unbounded += [f"{path.name}:{line}" for line in unbounded_spawns(tree)]
    return literals, silent, unbounded, reach, broken


def scan():
    global _SCAN
    if _SCAN is None:
        _SCAN = _scan()
    return _SCAN


def _assert_every_suite_file_parses():
    broken = scan()[4]
    assert not broken, (
        "a test file does not parse, so its processes cannot be checked:\n"
        + "\n".join(broken))


def test_no_literal_process_bound_in_the_suite():
    _assert_every_suite_file_parses()
    offenders = scan()[0]
    assert not offenders, (
        "a test bounds a real process or its socket by a literal or a counted sleep "
        "loop, not the shared bound:\n"
        + "\n".join(offenders))


def test_every_spawned_process_is_bounded_by_the_shared_bound():
    _assert_every_suite_file_parses()
    _, silent, unbounded = scan()[:3]
    assert not silent, (
        "a test spawns a real process and never reads HANG_TIMEOUT from support:\n"
        + "\n".join(silent))
    assert not unbounded, (
        "a test spawns a process with no timeout at all; pass timeout=HANG_TIMEOUT:\n"
        + "\n".join(unbounded))


# The launch's reach probe states a measured second it is free to cap: the
# sentence is counted from the call and clamped to the bound the launch
# promised plus curl's one-second reap grace (util/reach.go, #891), so a
# saturated scheduler legitimately changes the number. A test that pins one
# literal second therefore asserts the scheduler's mood, not the product; #943
# found the #260 twin doing exactly that. Doctor's own rows are exempt: their
# count is curl's complaint ("after 2003 ms"), read by a probe with no bound of
# its own, which the scheduler cannot rewrite.
REACH_SECOND = re.compile(r"\bno answer\b[^\n]*\bafter [0-9]+ seconds?\b")


def text_reach_seconds(text):
    """Line numbers whose text pins a reach verdict's measured second.

    Doctor's own rows are exempt by line: their count is curl's complaint
    ("after 2003 ms"), which the probe measures neither nor caps.
    """
    return [n for n, line in enumerate(text.splitlines(), 1)
            if "WARN gateway:" not in line and REACH_SECOND.search(line)]


def _pins_a_reach_second(value):
    """True when a line of `value` states a reach verdict's literal second,
    doctor's own WARN gateway rows aside (the same exemption as by text)."""
    return any("WARN gateway:" not in line and REACH_SECOND.search(line)
               for line in value.splitlines())


def parsed_reach_seconds(tree):
    """Line numbers of string literals that pin a reach verdict's measured
    second, read as Python's parser reads them.

    The parser joins adjacent strings and a backslash-newline inside a string,
    so a literal wrapped across source lines is one value here; an f-string is
    read as its literal text with each replacement field as `{}`, since a field
    is an expression, not a literal second. An f-string is checked whole and is
    not walked again by its fragments: the WARN exemption belongs to the
    sentence, and a fragment after a field no longer carries it. Bytes are read
    as UTF-8.
    """
    found = set()

    def visit(node):
        if isinstance(node, ast.JoinedStr):
            value = "".join(part.value if isinstance(part, ast.Constant) else "{}"
                            for part in node.values)
            if _pins_a_reach_second(value):
                found.add(node.lineno)
            return  # whole; its fields are expressions, not literal text
        if isinstance(node, ast.Constant):
            if isinstance(node.value, str):
                value = node.value
            elif isinstance(node.value, bytes):
                value = node.value.decode("utf-8", "replace")
            else:
                return
            if _pins_a_reach_second(value):
                found.add(node.lineno)
            return
        for child in ast.iter_child_nodes(node):
            visit(child)

    visit(tree)
    return found


def reach_seconds(text):
    """Every line of a tests/*.py source that pins a reach verdict's measured
    second: each line's text, and when the file parses, each string literal as
    Python reads it. A file that does not parse keeps the text reading alone."""
    found = set(text_reach_seconds(text))
    try:
        found |= parsed_reach_seconds(ast.parse(text))
    except (SyntaxError, ValueError):
        pass
    return sorted(found)


def test_the_reach_second_guard_reports_a_literal():
    sentence = 'no answer from gw.example port 443 after ' + '1 second'
    assert text_reach_seconds(f"want = {sentence!r}\n") == [1]


def test_the_reach_second_guard_scans_every_python_file_in_tests(tmp_path, monkeypatch):
    # The rule names tests/*.py, not only tests/*_test.py: a helper or fixture
    # carrying the literal is the same mistake, so the scan is asked of every
    # Python file under tests/. Assemble the sentence here to keep this file
    # itself free of the literal the scan rejects.
    sentence = 'no answer from gw.example port 443 after ' + '1 second'
    (tmp_path / "new_smoke.py").write_text(f"want = {sentence!r}\n")
    monkeypatch.setattr(sys.modules[__name__], "TESTS", tmp_path)
    assert _scan()[3] == ["new_smoke.py:1"]


def test_the_reach_second_guard_reads_a_file_that_does_not_parse(tmp_path, monkeypatch):
    # A reader can write the literal with an escaped newline, leaving a line
    # continuation after the quote: the file no longer parses, and the scan
    # must still name the line rather than crash (the finding's repro does
    # exactly this). The sentence is assembled to keep this source clean.
    sentence = 'no answer from gw.example port 443 after ' + '1 second'
    (tmp_path / "new_smoke.py").write_text("want = '" + sentence + "'\\" + 'n')
    monkeypatch.setattr(sys.modules[__name__], "TESTS", tmp_path)
    assert _scan()[3] == ["new_smoke.py:1"]


def test_the_reach_second_guard_reads_a_literal_wrapped_across_lines(tmp_path, monkeypatch):
    # Python joins adjacent strings into one literal, so an assertion wrapped
    # across two source lines pins the second though no single line holds the
    # sentence (#943's gate repro). Its count is assembled here to keep this
    # source clean.
    count = '1 ' + 'second'
    source = ('def test_reach(output):\n'
              '    assert output == ("no answer from gw.example port 443 "\n'
              '                      "after ' + count + '")\n')
    (tmp_path / "reach_example_test.py").write_text(source)
    monkeypatch.setattr(sys.modules[__name__], "TESTS", tmp_path)
    assert _scan()[3] == ["reach_example_test.py:2"]


def test_the_reach_second_guard_reads_every_literal_as_python_does():
    # The parser, not the line, decides what one literal is: adjacent strings,
    # a backslash-newline inside a string, an f-string's literal text and bytes
    # all state the same sentence. A replacement field is an expression, not a
    # literal second, and doctor's own rows stay exempt however they wrap.
    head, count = 'no answer from gw.example port 443 ', '1 ' + 'second'
    cases = {
        'w = ("' + head + '"\n     "after ' + count + '")\n': [1],
        'w = "' + head + '\\\nafter ' + count + '"\n': [1],
        'w = (f"no answer from {host} port 443 "\n     f"after ' + count + '")\n': [1],
        'w = (b"' + head + '"\n     b"after ' + count + '")\n': [1],
        'w = f"' + head + 'after {n} second"\n': [],
        "w = ('WARN gateway: " + head + "'\n     'after " + count + "')\n": [],
    }
    for source, want in cases.items():
        assert reach_seconds(source) == want, source


def test_the_reach_second_guard_keeps_doctor_exempt_after_an_interpolation(tmp_path, monkeypatch):
    # The WARN exemption belongs to the sentence, not to the fragment after an
    # interpolation: an f-string checked piece by piece flags doctor's tail
    # (gate repro #hive-repro-3b0c16fe). Assemble the sentence here to keep
    # this source clean.
    sentence = 'no answer from 10.255.255.1 port 8080 after ' + '2 seconds'
    source = ('space = " "\n'
              'assert out == f"WARN gateway:{space}' + sentence + '"\n')
    (tmp_path / "doctor_test.py").write_text(source)
    monkeypatch.setattr(sys.modules[__name__], "TESTS", tmp_path)
    assert _scan()[3] == []


def test_the_reach_second_guard_reports_a_broken_suite_files_literal(tmp_path, monkeypatch):
    # A *_test.py that does not parse still has its reach literal named; the
    # process checks have nothing to read there, and pytest names the broken
    # file at collection (gate repro #hive-repro-be89d567). The sentence is
    # assembled and the newline escaped to keep this source clean and the file
    # unparseable.
    sentence = 'no answer from gw.example port 443 after ' + '1 second'
    (tmp_path / "reach_example_test.py").write_text("want = '" + sentence + "'\\" + 'n')
    monkeypatch.setattr(sys.modules[__name__], "TESTS", tmp_path)
    assert _scan()[3] == ["reach_example_test.py:1"]


def test_a_suite_file_that_does_not_parse_fails_the_process_checks(tmp_path, monkeypatch):
    # The reach scan reads a broken file by its text, but the process checks
    # cannot read it at all, so they fail on it rather than pass it unread
    # (gate repro #hive-repro-11d02242): pytest may never collect the file
    # itself (conftest's collect_ignore skips timeout_verdict_test.py).
    (tmp_path / "broken_test.py").write_text("def f(:\n    pass\n")
    (tmp_path / "helper.py").write_text("def f(:\n    pass\n")
    monkeypatch.setattr(sys.modules[__name__], "TESTS", tmp_path)
    monkeypatch.setattr(sys.modules[__name__], "_SCAN", None)
    assert [entry.split(":")[0] for entry in _scan()[4]] == ["broken_test.py"]
    for check in (test_no_literal_process_bound_in_the_suite,
                  test_every_spawned_process_is_bounded_by_the_shared_bound):
        try:
            check()
        except AssertionError as exc:
            assert "broken_test.py" in str(exc), exc
            assert "helper.py" not in str(exc), exc
        else:
            raise AssertionError(f"{check.__name__} passed a suite file that does not parse")


def test_the_reach_second_guard_leaves_doctors_own_complaint_alone():
    sentence = 'WARN gateway: no answer from 10.255.255.1 port 8080 after ' + '2 seconds'
    assert text_reach_seconds(sentence + '\n') == []


def test_no_python_file_in_tests_pins_a_reach_verdicts_measured_second():
    reach = scan()[3]
    assert not reach, (
        "a tests/*.py file pins a reach verdict's measured second as a literal; the probe may cap "
        "it at the caller's claim, so read the class and bound the number (util/reach.go, #891):\n"
        + "\n".join(reach))


def literal_timeout_markers(tree):
    """Line numbers of `pytest.mark.timeout(<number>)` calls in `tree`.

    A per-test budget spelled as a number is a stale copy of the bound chain: a
    test that really starts a child would be held to a literal the grant cannot
    move with HANG_TIMEOUT (#976). The pseudo-code inside a string (the probe
    sources here) is not a call and is not read."""
    markers = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        if _names(node.func)[-2:] != ("mark", "timeout"):
            continue
        values = list(node.args) + [k.value for k in node.keywords if k.arg == "timeout"]
        if any(_is_number(value) for value in values):
            markers.append(node.lineno)
    return markers


def test_no_literal_timeout_marker_in_the_suite():
    """The grant and every explicit marker read one derived budget; a numeric
    literal is how #932's stale 300 would come back, and how a test that really
    starts a child would keep an unreachable ceiling."""
    _assert_every_suite_file_parses()
    offenders = []
    for path in sorted(TESTS.glob("*.py")):
        try:
            tree = ast.parse(path.read_text())
        except SyntaxError as exc:
            offenders.append(f"{path.name}: does not parse: {exc}")
            continue
        offenders += [f"{path.name}:{line}" for line in literal_timeout_markers(tree)]
    assert not offenders, (
        "a pytest timeout marker is spelled as a number; use "
        "@pytest.mark.timeout(SPAWN_TEST_BUDGET) so the one HANG_TIMEOUT knob "
        "moves it:\n" + "\n".join(offenders))


def test_the_marker_scan_reads_the_ast():
    def flagged(source):
        return literal_timeout_markers(ast.parse(textwrap.dedent(source).lstrip("\n")))

    assert flagged("@pytest.mark.timeout(300)\ndef test_x():\n    pass\n") == [1]
    assert flagged("@pytest.mark.timeout(timeout=300)\ndef test_x():\n    pass\n") == [1]
    assert flagged("@pytest.mark.timeout(SPAWN_TEST_BUDGET)\ndef test_x():\n    pass\n") == []
    assert flagged("@pytest.mark.timeout(HANG_TIMEOUT + 60)\ndef test_x():\n    pass\n") == []
    # The probe's own pseudo-code is data, not a call.
    assert flagged('SOURCE = "pytest.mark.timeout(300)"\n') == []


# The zsh suites carry python inline, so the same rule is matched per line: a
# spawn form bounded by a literal, a wall-clock deadline the suite picked
# itself, and embedded python that starts a real process without reading the
# shared bound.
ZSH_LITERAL = re.compile(r"(\.wait\(|\.communicate\(|subprocess\.(run|call|check_call|check_output)\().*timeout=[0-9.]")
ZSH_DEADLINE = re.compile(r"\bmonotonic\(\s*\)\s*\+\s*\(?\s*[0-9.]")
ZSH_SPAWN = re.compile(r"(subprocess\.(Popen|run|call|check_call|check_output)\(|os\.system\(|pty\.spawn\()")


def test_no_zsh_embedded_python_bounds_a_wait_with_a_number():
    offenders = []
    for path in sorted(TESTS.glob("*.zsh")):
        text = path.read_text()
        offenders += [f"{path.name}:{n}" for n, line in enumerate(text.splitlines(), 1)
                      if ZSH_LITERAL.search(line) or ZSH_DEADLINE.search(line)]
        if ZSH_SPAWN.search(text) and "HANG_TIMEOUT" not in text:
            offenders.append(path.name)
    assert not offenders, (
        "embedded python in a zsh suite bounds a wait or a spawned process by itself; run it with "
        "PYTHONPATH pointing at tests/ and use support.HANG_TIMEOUT:\n" + "\n".join(offenders))


# The same deadline in zsh's hands: embedded python reading another clock from
# the time module, or zsh arithmetic on its own wall clocks. Bare `monotonic()`
# is deliberately absent — another change owns that spelling — and a name on
# the right (`+ CCMORPH_TEST_HANG_TIMEOUT`) is the shared bound, not a budget
# the suite picked.
ZSH_CLOCK_DEADLINE = re.compile(
    r"\b(monotonic_ns|time_ns|perf_counter_ns|perf_counter|time)\s*\(\s*\)\s*\+\s*\(?\s*[0-9.]")
ZSH_WALL_DEADLINE = re.compile(
    r"\b(EPOCHREALTIME|EPOCHSECONDS|SECONDS)\s*\+\s*\(?\s*[0-9.]")


def test_no_zsh_deadline_written_on_another_clock():
    offenders = []
    for path in sorted(TESTS.glob("*.zsh")):
        offenders += [f"{path.name}:{n}" for n, line in
                      enumerate(path.read_text().splitlines(), 1)
                      if ZSH_CLOCK_DEADLINE.search(line) or ZSH_WALL_DEADLINE.search(line)]
    assert not offenders, (
        "a zsh suite writes a deadline on a clock it picked itself; the shared "
        "hang bound is the only budget:\n" + "\n".join(offenders))


# The same counted sleep loop, spelled in zsh. Only some loop headers hold a
# count: a brace range or `seq` word list, a C-style `for ((init; cond; ...))`
# with both an init and a condition, or `repeat N`. `while`/`until` end on an
# event, a plain word list ends when the list does, and `for ((;;))` is a poll —
# none of them is a budget. Nor is a C-style count that names the shared bound in
# its own header: it can only expire after the bound it counts out (ZSH_SHARED_COUNT).
ZSH_LOOP_OPEN = re.compile(r"\b(for|while|until|repeat|select)\b")
ZSH_DONE = re.compile(r"\bdone\b")
ZSH_COUNTED_FOR = re.compile(
    r"\s*(?:\w+\s+in\s*(?:\{[^}]*\.\.[^}]*\}|\$\(\s*seq\b|`\s*seq\b)|\(\(\s*[^;)]+;\s*[^;)])")
# A C-style count that reads the shared bound in its own header is that bound,
# not a budget the loop picked: `i < CCMORPH_TEST_HANG_TIMEOUT * 10` iterations
# of at least a 0.1 s sleep never finish before the bound does, and the
# environment override scales it. Same rule the deadline regexes apply to a name
# on the right of `+`.
ZSH_SHARED_COUNT = re.compile(r"\s*\(\([^)]*\bCCMORPH_TEST_HANG_TIMEOUT\b[^)]*\)\)")
ZSH_COUNTED_REPEAT = re.compile(r"\s*([0-9]+|\$)")
# A `sleep` that is a wait — not the suite defining or removing one.
ZSH_SLEEP = re.compile(r"(?<!unfunction )\bsleep\b(?!\s*\()")
ZSH_HEREDOC = re.compile(r"<<-?")
ZSH_HEREDOC_TAG = re.compile(r"\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1")


def _zsh_mask(raw, in_single, in_double):
    """Blank every character zsh would not read as code on this line — quoted
    text with its quotes, backslash escapes, a trailing comment — keeping every
    position, and return the quote state the next line starts in. Whatever is
    left standing is a word or operator exactly where zsh sees it."""
    out = []
    i, n = 0, len(raw)
    while i < n:
        c = raw[i]
        if in_single:
            in_single = c != "'"
            out.append(" ")
            i += 1
        elif in_double:
            if c == "\\" and i + 1 < n:
                out.extend("  ")
                i += 2
            else:
                in_double = c != '"'
                out.append(" ")
                i += 1
        elif c == "\\" and i + 1 < n:
            out.extend("  ")
            i += 2
        elif c == "'":
            in_single = True
            out.append(" ")
            i += 1
        elif c == '"':
            in_double = True
            out.append(" ")
            i += 1
        elif c == "#" and (not out or out[-1] in " \t;|&"):
            break  # a comment: zsh reads nothing after it
        else:
            out.append(c)
            i += 1
    return "".join(out), in_single, in_double


def zsh_counted_sleep_loops(text):
    """Lines whose loop opens a counted sleep loop — `for x in {a..b}` or
    `$(seq ...)`, a C-style `for ((init; cond; ...))`, or `repeat N` — with a
    `sleep` somewhere inside. The count is the same fixed wall-clock budget the
    AST check reads in `range(...)` loops; an event loop (`while`, `until`,
    `for ((;;))`, a plain word list) is a poll and stays quiet, and so is a
    C-style count taken from `CCMORPH_TEST_HANG_TIMEOUT` in the header itself —
    it expires no earlier than the shared bound. Quoted text is blanked and
    heredoc bodies skipped, so data never parses as zsh; a sleep reports the
    innermost counted loop around it."""
    offenders = set()
    loops = []            # (header line, counted), innermost last
    heredoc = None        # (terminator, strip leading tabs?)
    in_single = in_double = False
    for lineno, raw in enumerate(text.splitlines(), 1):
        if heredoc is not None:
            tag, tabs = heredoc
            if raw == tag or (tabs and raw.lstrip("\t") == tag):
                heredoc = None
            continue
        masked, in_single, in_double = _zsh_mask(raw, in_single, in_double)
        code = masked
        for m in ZSH_HEREDOC.finditer(masked):
            tag_m = ZSH_HEREDOC_TAG.match(raw, m.end())
            if tag_m:
                heredoc = (tag_m.group(2), m.group(0) == "<<-")
                code = masked[:m.start()]
                break
        events = [(m.start(), m.end(), m.group(1)) for m in ZSH_LOOP_OPEN.finditer(code)]
        events += [(m.start(), m.end(), "done") for m in ZSH_DONE.finditer(code)]
        events += [(m.start(), m.end(), "sleep") for m in ZSH_SLEEP.finditer(code)]
        for start, end, kind in sorted(events):
            if kind == "done":
                if loops:
                    loops.pop()
            elif kind == "sleep":
                for header, counted in reversed(loops):
                    if counted:
                        offenders.add(header)
                        break
            elif kind == "select":
                # jq's `select(` is a function call, not zsh's menu loop; the
                # loop reads a name after the keyword.
                if re.match(r"\s*[A-Za-z_]", code[end:]):
                    loops.append((lineno, False))
            elif kind == "for":
                rest = code[end:]
                loops.append((lineno, bool(ZSH_COUNTED_FOR.match(rest))
                              and not ZSH_SHARED_COUNT.match(rest)))
            elif kind == "repeat":
                loops.append((lineno, bool(ZSH_COUNTED_REPEAT.match(code[end:]))))
            else:  # while, until
                loops.append((lineno, False))
    return sorted(offenders)


def test_no_zsh_counted_sleep_loop():
    offenders = []
    for path in sorted(TESTS.glob("*.zsh")):
        offenders += [f"{path.name}:{n}" for n in
                      zsh_counted_sleep_loops(path.read_text())]
    assert not offenders, (
        "a zsh suite sleeps a counted number of times, a wall-clock budget it "
        "picked itself; wait on the shared bound instead:\n" + "\n".join(offenders))


def test_the_zsh_scanner_reads_the_language_it_polices():
    def loops(source):
        return zsh_counted_sleep_loops(textwrap.dedent(source).lstrip("\n"))

    assert loops("""
        for i in {1..100}; do [[ -s ready ]] && break; sleep 0.1; done
        """) == [1]
    assert loops("""
        for i in $(seq 1 10)
        do
          sleep 1
        done
        """) == [1]
    assert loops("""
        repeat 5; do sleep 0.2; done
        """) == [1]
    assert loops("""
        for (( i = 0; i < 100; i++ )); do sleep 0.1; done
        """) == [1]
    assert loops("""
        for i in {1..100}; do
          for f in $files; do
            sleep 0.1
          done
        done
        """) == [1]
    assert loops("""
        while :; do
          for i in {1..3}; do sleep 0.1; done
          break
        done
        """) == [2]
    # event waits and word lists: the sanctioned shapes, including the leak
    # sweep's settle loop, an event wait whose retry cap sits in the condition
    assert loops("""
        while ! kill -0 $pid 2>/dev/null; do sleep 0.1; done
        until [[ -s ready ]]; do sleep 0.1; done
        for sig in TERM HUP; do sleep 0.1; done
        for ((;;)); do [[ -s ready ]] && break; sleep 0.1; done
        select opt in a b; do sleep 1; done
        while (( ++pass <= 3 && ( $#_ccm_test_leaked || $#_ccm_test_kids ) )); do
          command sleep 0.2
        done
        for (( i = 0; i < CCMORPH_TEST_HANG_TIMEOUT * 10; i++ )); do
          [[ -s out ]] && break
          sleep 0.1
        done
        """) == []
    # the shared bound must be the count, in the header: naming it only in the
    # body leaves a literal budget standing
    assert loops("""
        for (( i = 0; i < 100; i++ )); do x=$CCMORPH_TEST_HANG_TIMEOUT; sleep 0.1; done
        """) == [1]
    # quoted text and heredoc bodies are data, not zsh
    assert loops("""
        stub='while :; do sleep 0.1; done'
        print "for i in {1..100}; do sleep 0.1; done"
        """) == []
    assert loops("""
        run <<'PY'
        for i in range(10):
            time.sleep(0.1)
        PY
        """) == []
    # a sleep the suite defines or removes is not a wait
    assert loops("""
        sleep() { command sleep 0.001; }
        unfunction sleep
        """) == []

    # the deadline regexes see embedded python and zsh arithmetic alike, and
    # leave bare `monotonic` to the check the other branch adds
    assert ZSH_CLOCK_DEADLINE.search("deadline = time.time() + 5")
    assert ZSH_CLOCK_DEADLINE.search("until perf_counter() + 10 > x")
    assert ZSH_CLOCK_DEADLINE.search("if monotonic_ns() + 1 < y")
    assert not ZSH_CLOCK_DEADLINE.search("deadline = time.monotonic() + 5")
    assert not ZSH_CLOCK_DEADLINE.search("deadline = time.time() + HANG_TIMEOUT")
    assert ZSH_WALL_DEADLINE.search("local d=$(( EPOCHREALTIME + 10 ))")
    assert ZSH_WALL_DEADLINE.search("(( deadline = SECONDS + 30 ))")
    assert not ZSH_WALL_DEADLINE.search(
        "(( deadline = EPOCHREALTIME + CCMORPH_TEST_HANG_TIMEOUT ))")


def test_the_ast_deadline_check_names_every_clock():
    def bounds(source):
        return literal_process_bounds(ast.parse(textwrap.dedent(source).lstrip("\n")))

    # the module spelling and `from time import ...` alike
    assert bounds("deadline = time.time() + 5") == [1]
    assert bounds("deadline = perf_counter() + 10") == [1]
    assert bounds("end = monotonic_ns() + 1") == [1]
    # the shared bound on the right is a name, not a budget the test picked
    assert bounds("deadline = time.monotonic() + HANG_TIMEOUT") == []


def test_hang_bound_reads_its_environment_override():
    """One knob: the child's bound and the per-test budget move together (#976).
    Raising the override for a slower host raises both; nothing holds a stale
    literal the child can die under.

    The child's text is compared as printed, not through float(): an integer-only
    parser prints `7` where the suite's bound is `7.0`, and that mutation has to
    keep failing here (it did on main)."""
    def read_bound(override):
        env = {k: v for k, v in os.environ.items() if k != "CCMORPH_TEST_HANG_TIMEOUT"}
        if override is not None:
            env["CCMORPH_TEST_HANG_TIMEOUT"] = override
        result = subprocess.run(
            [sys.executable, "-c",
             "import support; print(support.HANG_TIMEOUT, support.SPAWN_TEST_BUDGET)"],
            cwd=TESTS, env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        return result.stdout.strip()

    assert read_bound("7") == f"7.0 {7.0 + SPAWN_SLACK}"
    assert read_bound(None) == f"240.0 {240.0 + SPAWN_SLACK}"


# The probe is the witness for the spawn grant (#976): it runs under a
# PROBE_CEILING ini ceiling, so a test that starts a process and is not granted
# fails its own `armed() > BUDGET - 30` assertion while a test that starts
# nothing keeps passing at the ceiling. `armed()` reads whichever timer pytest-timeout armed: the
# threading.Timer under the worker method, the ITIMER_REAL under signal.
# The inner session's ini ceiling: large enough that a starved no-spawn row is
# not killed by it, and small enough that the override-7 row still tells it from
# the budget (20 < 67 - 30).
PROBE_CEILING = 20

PROBE_TEST = '''\
import os, signal, subprocess, sys, threading, time, unittest
import pytest

BUDGET = float(os.environ["PROBE_BUDGET"])
CEILING = float(os.environ["PROBE_CEILING"])


def armed():
    timers = [t.interval for t in threading.enumerate()
              if isinstance(t, threading.Timer) and t.name.startswith("pytest_timeout")]
    return timers[0] if timers else signal.getitimer(signal.ITIMER_REAL)[0]


def test_control_no_spawn_keeps_the_ini_ceiling():
    assert armed() <= CEILING


def test_run_extends():
    subprocess.run([sys.executable, "-c", "pass"], check=True)
    assert armed() > BUDGET - 30


def test_os_system_extends():
    os.system("true")
    assert armed() > BUDGET - 30


def test_thread_spawn_extends():
    t = threading.Thread(target=lambda: subprocess.run(["true"]))
    t.start()
    t.join()
    assert armed() > BUDGET - 30


def test_delegating_wrapper_extends(monkeypatch):
    real = subprocess.Popen
    monkeypatch.setattr(subprocess, "Popen", lambda *a, **k: real(*a, **k))
    subprocess.run(["true"])
    assert armed() > BUDGET - 30


def test_stub_that_never_spawns_keeps_the_ceiling(monkeypatch):
    monkeypatch.setattr(subprocess, "Popen", lambda *a, **k: None)
    subprocess.Popen(["x"])
    assert armed() <= CEILING


def test_a_rejected_popen_call_is_not_a_spawn():
    with pytest.raises(TypeError, match="bufsize must be an integer"):
        subprocess.Popen(["/bin/true"], bufsize="invalid")
    assert armed() <= CEILING


@pytest.mark.timeout(5)
def test_marker_below_the_budget_on_a_rejected_call_is_left_alone():
    with pytest.raises(TypeError, match="bufsize must be an integer"):
        subprocess.Popen(["/bin/true"], bufsize="invalid")
    assert armed() <= 5.0


@pytest.mark.timeout(BUDGET)
def test_marker_at_the_budget_passes():
    subprocess.run(["true"])
    assert armed() >= BUDGET - 30


@pytest.mark.timeout(5)
def test_marker_below_the_budget_that_spawns_fails():
    subprocess.run(["true"])


@pytest.mark.timeout(5)
def test_marker_below_the_budget_without_a_spawn_is_left_alone():
    assert armed() <= 5.0


class Held(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        subprocess.run(["true"])

    def test_a(self):
        assert armed() > BUDGET - 30

    def test_b(self):
        assert armed() > BUDGET - 30


class Plain(unittest.TestCase):
    def test_c(self):
        assert armed() <= CEILING


@pytest.fixture(scope="module")
def held():
    p = subprocess.Popen(["sleep", "30"])
    yield p
    p.kill()
    p.wait()


def test_m1(held):
    assert armed() > BUDGET - 30


def test_m2(held):
    assert armed() > BUDGET - 30


def test_m_unrelated_keeps_the_ceiling():
    assert armed() <= CEILING


def _touch(path):
    os.close(os.open(path, os.O_CREAT | os.O_WRONLY))


def _preexec(path):
    def child():  # the child is already running while Popen waits for its exec
        _touch(path)
        time.sleep(0.4)
    return child


def _fork_form(path):
    pid = os.fork()
    if pid == 0:
        _touch(path)
        time.sleep(0.6)
        os._exit(0)
    os.waitpid(pid, 0)


def _sh(path):
    return f"touch {path}; sleep 0.6"


STARTS = {
    "popen_preexec": lambda path: subprocess.Popen(["true"], preexec_fn=_preexec(path)).wait(),
    "run": lambda path: subprocess.run(["sh", "-c", _sh(path)]),
    # these two block until the child ends
    "os_system": lambda path: os.system(_sh(path)),
    "spawnv_wait": lambda path: os.spawnv(os.P_WAIT, "/bin/sh", ["sh", "-c", _sh(path)]),
    "posix_spawn": lambda path: os.waitpid(
        os.posix_spawn("/bin/sh", ["sh", "-c", _sh(path)], os.environ), 0),
    "fork": _fork_form,
}


@pytest.mark.parametrize("form", sorted(STARTS))
def test_the_budget_is_armed_while_the_child_runs(form, tmp_path):
    """What is armed is read from a thread the moment the child reports that it
    is running, so a grant that waits for the start call to return reads the
    ceiling here."""
    path, seen = str(tmp_path / "running"), []

    def sample():
        end = time.monotonic() + 60
        while not os.path.exists(path) and time.monotonic() < end:
            time.sleep(0.005)
        if os.path.exists(path):
            seen.append(armed())

    sampler = threading.Thread(target=sample)
    sampler.start()
    try:
        STARTS[form](path)
    finally:
        sampler.join()
    assert seen and seen[0] > BUDGET - 30, seen


# name -> (the rejected call, does it get as far as the wrapper?). Popen's own
# argument check (bufsize) runs in __init__, before _execute_child, so that call
# never moves the timer; the others are moved and then put back.
REJECTED = {
    "popen_bufsize": (lambda: subprocess.Popen(["/bin/true"], bufsize="invalid"), False),
    "popen_nul_byte": (lambda: subprocess.Popen(["/bin/true", "a" + chr(0) + "b"]), True),
    "os_system_none": (lambda: os.system(None), True),
    "spawnv_not_a_list": (lambda: os.spawnv(os.P_WAIT, "/bin/true", "x"), True),
}


@pytest.mark.parametrize("form", sorted(REJECTED))
def test_a_rejected_call_puts_the_deadline_back_as_it_was(form):
    call, moved = REJECTED[form]
    time.sleep(0.3)
    with pytest.raises((TypeError, ValueError)):
        call()
    # Signal method: armed() is what is left, so refunding the whole ceiling shows.
    # Thread method: it is the Timer's duration, which an untouched timer keeps
    # (CEILING) and a put-back one has shortened by what was spent.
    threaded = any(isinstance(t, threading.Timer) and t.name.startswith("pytest_timeout")
                   for t in threading.enumerate())
    limit = CEILING if (threaded and not moved) else CEILING - 0.25
    assert armed() <= limit, (armed(), limit)


def test_a_real_start_after_a_rejected_call_still_extends():
    with pytest.raises(TypeError):
        os.system(None)
    subprocess.run(["true"])
    assert armed() > BUDGET - 30


@pytest.mark.timeout(5)
def test_marker_below_the_budget_on_a_call_rejected_after_the_wrapper_is_left_alone():
    with pytest.raises(TypeError):
        os.system(None)


@pytest.mark.timeout(5)
def test_a_marked_test_keeps_the_verdict_when_a_child_ran_and_popen_raised(tmp_path):
    path = str(tmp_path / "ran")

    def child():
        _touch(path)
        raise ValueError("child setup failed")

    with pytest.raises(subprocess.SubprocessError):
        subprocess.Popen(["true"], preexec_fn=child)
    assert os.path.exists(path)
'''


def run_probe(tmp_path, mode, override=None, grant=True):
    """Run `PROBE_TEST` in an inner pytest with this suite's conftest (or none),
    and answer ({nodeid: {outcome}}, output)."""
    if grant:
        for name in ("conftest.py", "support.py"):
            shutil.copy(TESTS / name, tmp_path / name)
        (tmp_path / "proc_pressure_test.py").write_text(PROBE_PROC_PRESSURE)
    else:
        (tmp_path / "conftest.py").write_text("# no spawn grant\n")
    (tmp_path / "pytest.ini").write_text(f"[pytest]\ntimeout = {PROBE_CEILING}\n")
    (tmp_path / "probe_test.py").write_text(PROBE_TEST)
    # The inner session reads the same knob the outer one did: CI installs no
    # xdist, so the serial row drops -n0 there instead of failing to start.
    inner_hang = float(override) if override is not None else HANG_TIMEOUT
    budget = inner_hang + SPAWN_SLACK
    env = {k: v for k, v in os.environ.items() if not k.startswith("PYTEST_")}
    env["PROBE_BUDGET"] = str(budget)
    env["PROBE_CEILING"] = str(PROBE_CEILING)
    env["CCMORPH_TEST_HANG_TIMEOUT"] = str(inner_hang)
    mode_args = ["-n1"] if mode == "-n1" else (
        ["-n0"] if mode == "-n0" and importlib.util.find_spec("xdist") is not None else [])
    proc = subprocess.run(
        [sys.executable, "-m", "pytest", *mode_args, "-p", "no:cacheprovider", "-rA",
         "probe_test.py"],
        cwd=tmp_path, env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    out = proc.stdout + proc.stderr
    outcomes = {}
    for status, nodeid in re.findall(r"^(PASSED|FAILED|ERROR) (probe_test\.py::\S+)", out, re.M):
        outcomes.setdefault(nodeid, set()).add(status)
    return outcomes, out


# The GH-919 watcher starts a process of its own at a worker's first test
# setup; the stub makes it real, and `watch.started` proves the setup ran it.
# The control test is first in PROBE_TEST, so it witnesses that the watcher's
# spawn was not counted as the test's own.
PROBE_PROC_PRESSURE = '''\
import subprocess
from pathlib import Path

RUN_SHARE = 0.5


def per_user_cap():
    return (1000, "stub")


class RunTreeWatch:
    def __init__(self, *args):
        pass

    def start(self):
        subprocess.run(["true"])
        Path(__file__).with_name("watch.started").write_text("started")
        return self

    def begin(self, test):
        pass

    def end(self):
        return None

    def stop(self):
        pass
'''


# The parametrized forms in PROBE_TEST (sorted, as pytest ids them).
PROBE_START_FORMS = ("fork", "os_system", "popen_preexec", "posix_spawn", "run", "spawnv_wait")
PROBE_REJECTED_FORMS = ("os_system_none", "popen_bufsize", "popen_nul_byte", "spawnv_not_a_list")

PROBE_ALL_PASS = {
    "probe_test.py::test_control_no_spawn_keeps_the_ini_ceiling": {"PASSED"},
    "probe_test.py::test_run_extends": {"PASSED"},
    "probe_test.py::test_os_system_extends": {"PASSED"},
    "probe_test.py::test_thread_spawn_extends": {"PASSED"},
    "probe_test.py::test_delegating_wrapper_extends": {"PASSED"},
    "probe_test.py::test_stub_that_never_spawns_keeps_the_ceiling": {"PASSED"},
    "probe_test.py::test_a_rejected_popen_call_is_not_a_spawn": {"PASSED"},
    "probe_test.py::test_marker_at_the_budget_passes": {"PASSED"},
    # The call phase passes; the teardown verdict is the ERROR. A bare PASSED
    # would mean the at-budget row above was silently verdicted too.
    "probe_test.py::test_marker_below_the_budget_that_spawns_fails": {"PASSED", "ERROR"},
    "probe_test.py::test_marker_below_the_budget_on_a_rejected_call_is_left_alone": {"PASSED"},
    "probe_test.py::test_marker_below_the_budget_without_a_spawn_is_left_alone": {"PASSED"},
    "probe_test.py::Held::test_a": {"PASSED"},
    "probe_test.py::Held::test_b": {"PASSED"},
    "probe_test.py::Plain::test_c": {"PASSED"},
    "probe_test.py::test_m1": {"PASSED"},
    "probe_test.py::test_m2": {"PASSED"},
    "probe_test.py::test_m_unrelated_keeps_the_ceiling": {"PASSED"},
    "probe_test.py::test_a_real_start_after_a_rejected_call_still_extends": {"PASSED"},
    "probe_test.py::test_marker_below_the_budget_on_a_call_rejected_after_the_wrapper_is_left_alone":
        {"PASSED"},
    # A child ran and Popen raised afterwards: it is a start, so the marker keeps
    # its verdict (the call phase passes, the teardown is the ERROR).
    "probe_test.py::test_a_marked_test_keeps_the_verdict_when_a_child_ran_and_popen_raised":
        {"PASSED", "ERROR"},
    **{f"probe_test.py::test_the_budget_is_armed_while_the_child_runs[{form}]": {"PASSED"}
       for form in PROBE_START_FORMS},
    **{f"probe_test.py::test_a_rejected_call_puts_the_deadline_back_as_it_was[{form}]": {"PASSED"}
       for form in PROBE_REJECTED_FORMS},
}


def _assert_probe(outcomes, out):
    """Every probe node and its exact outcome set: a teardown verdict leaves
    both PASSED (the call phase) and ERROR (its verdict), so an at-budget row
    that also collected an ERROR cannot pass silently."""
    assert outcomes == PROBE_ALL_PASS, out
    assert "explicit 5s timeout marker" in out, out
    assert ("test_a_marked_test_keeps_the_verdict_when_a_child_ran_and_popen_raised starts a "
            "process under an explicit 5s timeout marker") in out, out


@pytest.mark.parametrize("mode", ["-n0", "-n1", "plain"])
def test_a_test_that_really_starts_a_process_gets_the_spawn_budget(tmp_path, mode):
    """A real start extends the test's own timer to SPAWN_TEST_BUDGET, under the
    serial signal method and the xdist worker thread method alike; a stub, a
    thread's start() and a test that starts nothing keep the ini ceiling; an
    explicit marker below the budget is a verdict, not a grant."""
    if mode != "-n0":
        # "plain": no -n flag with xdist installed - a conftest that is the
        # rootdir's own stays serial (signal method), it does not fan out.
        pytest.importorskip("xdist")
    outcomes, out = run_probe(tmp_path, mode)
    _assert_probe(outcomes, out)
    # The watcher's own spawn ran at the first test's setup and did not grant it.
    assert (tmp_path / "watch.started").exists(), out


PLAIN_WITNESS = '''\
import signal, subprocess, sys, threading, time
from support import SPAWN_TEST_BUDGET


def test_the_itimer_holds_the_budget_while_a_preexec_child_runs(tmp_path):
    started, release = tmp_path / "started", tmp_path / "release"
    seen = []

    def before_exec():
        started.touch()
        end = time.monotonic() + 1
        while not release.exists() and time.monotonic() < end:
            time.sleep(.005)

    def observe():
        end = time.monotonic() + 1
        while not started.exists() and time.monotonic() < end:
            time.sleep(.005)
        if started.exists():
            seen.append(signal.getitimer(signal.ITIMER_REAL)[0])
        release.touch()

    thread = threading.Thread(target=observe)
    thread.start()
    with subprocess.Popen([sys.executable, "-c", "pass"], preexec_fn=before_exec) as child:
        assert child.wait() == 0
    thread.join()
    assert seen and seen[0] >= SPAWN_TEST_BUDGET - 1, seen
'''


def test_a_conftest_that_is_the_rootdirs_own_stays_serial_with_xdist_installed(tmp_path):
    """A one-test run beside a copy of this conftest, with xdist installed and no
    -n flag, runs in the main process: the signal method's itimer is what a
    witness reads. Fanned out to a worker (thread method) the itimer is never
    armed and the budget cannot be observed there (GH-976)."""
    pytest.importorskip("xdist")
    for name in ("conftest.py", "support.py"):
        shutil.copy(TESTS / name, tmp_path / name)
    (tmp_path / "pytest.ini").write_text(f"[pytest]\ntimeout = {PROBE_CEILING}\n")
    (tmp_path / "witness_test.py").write_text(PLAIN_WITNESS)
    env = {k: v for k, v in os.environ.items() if not k.startswith("PYTEST_")}
    proc = subprocess.run(
        [sys.executable, "-m", "pytest", "-p", "no:cacheprovider", "witness_test.py"],
        cwd=tmp_path, env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert proc.returncode == 0, proc.stdout + proc.stderr


def test_the_probe_is_red_without_the_grant(tmp_path):
    """The probe cannot pass vacuously: without this conftest's grant, every
    node that starts a child fails its own armed() assertion and the nodes that
    start nothing still pass at the ini ceiling."""
    outcomes, out = run_probe(tmp_path, "-n0", grant=False)
    for node in ("test_run_extends", "test_os_system_extends", "test_thread_spawn_extends",
                 "test_delegating_wrapper_extends", "Held::test_a", "Held::test_b",
                 "test_m1", "test_m2", "test_a_real_start_after_a_rejected_call_still_extends",
                 *(f"test_the_budget_is_armed_while_the_child_runs[{form}]"
                   for form in PROBE_START_FORMS)):
        assert outcomes.get(f"probe_test.py::{node}") == {"FAILED"}, (node, outcomes, out)
    for node in ("test_control_no_spawn_keeps_the_ini_ceiling",
                 "test_stub_that_never_spawns_keeps_the_ceiling",
                 "test_a_rejected_popen_call_is_not_a_spawn",
                 "test_marker_below_the_budget_on_a_rejected_call_is_left_alone",
                 "test_marker_below_the_budget_without_a_spawn_is_left_alone",
                 "test_marker_below_the_budget_on_a_call_rejected_after_the_wrapper_is_left_alone",
                 "test_a_marked_test_keeps_the_verdict_when_a_child_ran_and_popen_raised",
                 *(f"test_a_rejected_call_puts_the_deadline_back_as_it_was[{form}]"
                   for form in PROBE_REJECTED_FORMS),
                 "Plain::test_c", "test_m_unrelated_keeps_the_ceiling"):
        assert outcomes.get(f"probe_test.py::{node}") == {"PASSED"}, (node, outcomes, out)


def test_the_grant_follows_the_hang_timeout_override(tmp_path):
    """One knob: with CCMORPH_TEST_HANG_TIMEOUT=7 the inner session's budget is
    67, and the grant and the explicit-marker verdict read it, not 300."""
    outcomes, out = run_probe(tmp_path, "-n0", override="7")
    _assert_probe(outcomes, out)
    assert (tmp_path / "watch.started").exists(), out


EXPORTER = Path(__file__).resolve().parent.parent / "share/usage-export"


@pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
def test_a_held_store_lock_bounds_quota_and_export(tmp_path):
    """A compaction's exclusive hold over a real-sized day is a wait no hot
    path may outlive (INTENT 8, #654): the statusline's quota read and an
    export each answer or refuse under the store's lock bound. The holders here
    are the test's own locks, so a reader without that bound blocks until this
    test's own cap kills it."""
    store = tmp_path / "ccmorph" / "usage"
    day = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=3)).strftime("%Y-%m-%d")
    folder = store / day
    folder.mkdir(parents=True)
    for i in range(2000):
        (folder / f"row_{i}.json").write_text(json.dumps({
            "schema": 1, "response_id": f"held_{i}", "timestamp": f"{day}T09:00:00Z",
            "profile": "ledger", "complete": True,
            "usage": {"input_tokens": 10, "cost": 0.01}}))
    env = {**os.environ, "XDG_STATE_HOME": str(tmp_path)}
    with open(store / ".move.lock", "w") as held:
        fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
        quota_run = subprocess.run([str(CORE), "quota", "--json"],
                                   input="ledger\tgateway\t\t\n", env=env,
                                   capture_output=True, text=True, timeout=HANG_TIMEOUT)
        export_run = subprocess.run([sys.executable, str(EXPORTER), "--source", str(store),
                                     "--output", str(tmp_path / "out")], env=env,
                                    capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert quota_run.returncode == 1, (quota_run.returncode, quota_run.stderr)
    assert "usage store busy" in quota_run.stdout + quota_run.stderr, quota_run.stdout
    assert export_run.returncode == 1, (export_run.returncode, export_run.stdout)
    assert "usage store busy" in export_run.stderr, export_run.stderr


@pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
def test_quota_leaves_a_day_too_large_for_the_statusline_to_usage_compact(tmp_path):
    """The quota read that wins the store lock itself compacts at most one
    closed day, and only one its hot-path budget covers (INTENT 8, #654): a
    day folder over quota's autoCompactMaxFiles (2000) is left whole, so the
    statusline never pays for a real-sized day's rewrite, and
    `ccmorph-core usage-compact` still drains it."""
    store = tmp_path / "ccmorph" / "usage"
    day = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=3)).strftime("%Y-%m-%d")
    folder = store / day
    folder.mkdir(parents=True)
    for i in range(2100):
        (folder / f"row_{i}.json").write_text(json.dumps({
            "schema": 1, "response_id": f"big_{i}", "turn_id": f"t_{i}",
            "timestamp": f"{day}T09:00:00Z", "profile": "ledger", "complete": True,
            "usage": {"input_tokens": 10, "cost": 0.01}}))
    env = {**os.environ, "XDG_STATE_HOME": str(tmp_path)}
    quota_run = subprocess.run([str(CORE), "quota", "--json"],
                               input="ledger\tgateway\t\t\n", env=env,
                               capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert quota_run.returncode == 0, (quota_run.stdout, quota_run.stderr)
    assert not (store / f"{day}.jsonl").exists(), "the statusline's read compacted a day over its budget"
    assert len(list(folder.iterdir())) == 2100
    compact_run = subprocess.run([str(CORE), "usage-compact"], env=env,
                                 capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert compact_run.returncode == 0, compact_run.stderr
    assert "compacted 1 usage day(s)" in compact_run.stdout, compact_run.stdout
    assert (store / f"{day}.jsonl").exists() and not folder.exists()


@pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
def test_quota_leaves_a_day_of_large_receipts_to_usage_compact(tmp_path):
    """The statusline's budget bounds the bytes a day costs, not only its entry
    count (INTENT 8, #654): three receipts of a megabyte each are far under
    quota's autoCompactMaxFiles but over its 2 MiB autoCompactMaxBytes, so the
    quota read leaves the day whole, and `ccmorph-core usage-compact` still
    drains it."""
    store = tmp_path / "ccmorph" / "usage"
    day = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=3)).strftime("%Y-%m-%d")
    folder = store / day
    folder.mkdir(parents=True)
    for i in range(3):
        (folder / f"row_{i}.json").write_text(json.dumps({
            "schema": 1, "response_id": f"large_{i}", "turn_id": f"t_{i}",
            "timestamp": f"{day}T09:00:00Z", "profile": "ledger", "complete": True,
            "usage": {"input_tokens": 10, "cost": 0.01}, "note": "x" * (1 << 20)}))
    env = {**os.environ, "XDG_STATE_HOME": str(tmp_path)}
    quota_run = subprocess.run([str(CORE), "quota", "--json"],
                               input="ledger\tgateway\t\t\n", env=env,
                               capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert quota_run.returncode == 0, (quota_run.stdout, quota_run.stderr)
    assert not (store / f"{day}.jsonl").exists(), "the statusline's read compacted a day over its byte budget"
    assert len(list(folder.iterdir())) == 3
    compact_run = subprocess.run([str(CORE), "usage-compact"], env=env,
                                 capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert compact_run.returncode == 0, compact_run.stderr
    assert "compacted 1 usage day(s)" in compact_run.stdout, compact_run.stdout
    assert (store / f"{day}.jsonl").exists() and not folder.exists()


@pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
def test_quota_never_waits_on_a_fifo_in_a_closed_day(tmp_path):
    """A FIFO left under a receipt's name in a closed day never holds the quota
    read that wins the store lock and compacts that day itself (INTENT 8,
    #654): the open does not wait, the day is named and left, the read answers
    the rest of the month and names the FIFO, and no process is left holding
    the store lock."""
    store = tmp_path / "ccmorph" / "usage"
    now = dt.datetime.now(dt.timezone.utc)
    day_date = now - dt.timedelta(days=3)
    day = day_date.strftime("%Y-%m-%d")
    folder = store / day
    folder.mkdir(parents=True)
    (folder / "row.json").write_text(json.dumps({
        "schema": 1, "response_id": "r", "turn_id": "t",
        "timestamp": f"{day}T09:00:00Z", "profile": "ledger", "complete": True,
        "usage": {"input_tokens": 10, "cost": 0.01}}))
    os.mkfifo(folder / "unusable.json", 0o600)
    env = {**os.environ, "XDG_STATE_HOME": str(tmp_path)}
    quota_run = subprocess.run([str(CORE), "quota", "--json"],
                               input="ledger\tgateway\t\t\n", env=env,
                               capture_output=True, text=True, timeout=HANG_TIMEOUT)
    entry = json.loads(quota_run.stdout)["profiles"][0]
    if store_month_first(now) <= day_date.date() <= now.date():
        # The day is inside quota's own window — the month stepped from a local
        # midnight as an instant, which is not the UTC calendar's month east or
        # west of UTC: the FIFO is skipped and named, and the readable row is
        # still the month (#654, #804). Outside it, quota never opens the day.
        assert entry["spend"]["month_turns"] == 1 and entry["spend"]["unreadable_receipts"] == 1, entry
        assert "not readable" in entry["error"] and "unusable.json" in entry["error"], entry
    assert "not a regular file" in quota_run.stderr, quota_run.stderr
    assert not (store / f"{day}.jsonl").exists()
    with open(store / ".move.lock", "a+") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)  # nobody still holds it


@pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
def test_usage_compact_never_waits_on_a_fifo_archive_index(tmp_path):
    """A FIFO left under a closed day's archive-index name, with the day's folder
    beside it and no archive, is the one store name the #654 landing left
    unguarded (INTENT 8, #804): the orphan-index recovery wrapped around the
    guarded archive read opens the raw name, so `usage-compact` blocks there
    while it holds the store's exclusive lock — and every later closed-day write
    answers `usage store busy` forever. The compaction names the day as damage
    and leaves it whole, the quota read that follows answers, and no process is
    left holding the store lock."""
    store = tmp_path / "ccmorph" / "usage"
    day = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=3)).strftime("%Y-%m-%d")
    folder = store / day
    folder.mkdir(parents=True)
    (folder / "row.json").write_text(json.dumps({
        "schema": 1, "response_id": "r", "turn_id": "t",
        "timestamp": f"{day}T09:00:00Z", "profile": "ledger", "complete": True,
        "usage": {"input_tokens": 10, "cost": 0.01}}))
    os.mkfifo(store / f"{day}.jsonl.idx", 0o600)
    env = {**os.environ, "XDG_STATE_HOME": str(tmp_path)}
    compact_run = subprocess.run([str(CORE), "usage-compact"], env=env,
                                 capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert compact_run.returncode == 1, (compact_run.returncode, compact_run.stderr)
    assert f"day {day}" in compact_run.stderr, compact_run.stderr
    assert "not a regular file" in compact_run.stderr, compact_run.stderr
    assert "compacted 0 usage day(s)" in compact_run.stdout, compact_run.stdout
    assert (folder / "row.json").exists(), "the compaction took the damaged day's row"
    assert not (store / f"{day}.jsonl").exists(), "the compaction published over damaged bytes"
    quota_run = subprocess.run([str(CORE), "quota", "--json"],
                               input="ledger\tgateway\t\t\n", env=env,
                               capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert quota_run.returncode in (0, 1), quota_run.stderr  # an answer, never a wait
    assert "not a regular file" in quota_run.stderr, quota_run.stderr  # its own pass names it too
    assert not (store / f"{day}.jsonl").exists()
    with open(store / ".move.lock", "a+") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)  # nobody still holds it
