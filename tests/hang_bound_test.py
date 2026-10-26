"""One hang bound for the suite, and a check that no test file invents its own.

A literal `timeout=` on a spawned process — or on a socket read from one that a
test started — competes with an unbounded queue rather than detecting a hang, so
a healthy run on a loaded host goes red. So does a budget spelled as a count of
sleeps: a loop that runs `range(N)` times in python, or counts `{1..N}` in a
zsh suite, and sleeps each pass holds a fixed wall-clock budget a loaded host
can exceed. Every such wait reads `support.HANG_TIMEOUT` instead, and so does
every test file that spawns a process; the zsh suites' embedded python obeys
the same rule through PYTHONPATH, and these checks hold that line.
"""
import ast
import os
import re
import subprocess
import sys
import textwrap
from pathlib import Path

from support import HANG_TIMEOUT

TESTS = Path(__file__).resolve().parent

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
    """Read and parse each test file once; both checks below share the result."""
    literals, silent, unbounded = [], [], []
    for path in sorted(TESTS.glob("*_test.py")):
        text = path.read_text()
        tree = ast.parse(text)
        literals += [f"{path.name}:{line}" for line in literal_process_bounds(tree)]
        if "HANG_TIMEOUT" not in text and spawns_a_process(tree):
            silent.append(path.name)
        unbounded += [f"{path.name}:{line}" for line in unbounded_spawns(tree)]
    return literals, silent, unbounded


def scan():
    global _SCAN
    if _SCAN is None:
        _SCAN = _scan()
    return _SCAN


def test_no_literal_process_bound_in_the_suite():
    offenders, _, _ = scan()
    assert not offenders, (
        "a test bounds a real process or its socket by a literal or a counted sleep "
        "loop, not the shared bound:\n"
        + "\n".join(offenders))


def test_every_spawned_process_is_bounded_by_the_shared_bound():
    _, silent, unbounded = scan()
    assert not silent, (
        "a test spawns a real process and never reads HANG_TIMEOUT from support:\n"
        + "\n".join(silent))
    assert not unbounded, (
        "a test spawns a process with no timeout at all; pass timeout=HANG_TIMEOUT:\n"
        + "\n".join(unbounded))


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
# the right (`+ CCPROTEUS_TEST_HANG_TIMEOUT`) is the shared bound, not a budget
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
# not a budget the loop picked: `i < CCPROTEUS_TEST_HANG_TIMEOUT * 10` iterations
# of at least a 0.1 s sleep never finish before the bound does, and the
# environment override scales it. Same rule the deadline regexes apply to a name
# on the right of `+`.
ZSH_SHARED_COUNT = re.compile(r"\s*\(\([^)]*\bCCPROTEUS_TEST_HANG_TIMEOUT\b[^)]*\)\)")
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
    C-style count taken from `CCPROTEUS_TEST_HANG_TIMEOUT` in the header itself —
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
        while (( ++pass <= 3 && ( $#_ccs_test_leaked || $#_ccs_test_kids ) )); do
          command sleep 0.2
        done
        for (( i = 0; i < CCPROTEUS_TEST_HANG_TIMEOUT * 10; i++ )); do
          [[ -s out ]] && break
          sleep 0.1
        done
        """) == []
    # the shared bound must be the count, in the header: naming it only in the
    # body leaves a literal budget standing
    assert loops("""
        for (( i = 0; i < 100; i++ )); do x=$CCPROTEUS_TEST_HANG_TIMEOUT; sleep 0.1; done
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
        "(( deadline = EPOCHREALTIME + CCPROTEUS_TEST_HANG_TIMEOUT ))")


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
    def read_bound(override):
        env = {k: v for k, v in os.environ.items() if k != "CCPROTEUS_TEST_HANG_TIMEOUT"}
        if override is not None:
            env["CCPROTEUS_TEST_HANG_TIMEOUT"] = override
        result = subprocess.run([sys.executable, "-c", "import support; print(support.HANG_TIMEOUT)"],
                                cwd=TESTS, env=env, capture_output=True, text=True,
                                timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        return result.stdout.strip()

    assert read_bound("7") == "7.0"
    assert read_bound(None) == "120.0"