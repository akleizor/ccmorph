"""The suite's shared helpers: one hang-detection bound, one poll loop, one script
loader; imported by the tests that wait on a real process."""
import ast
import atexit
import contextlib
import datetime as dt
import importlib.machinery
import importlib.util
import os
import pty
import random
import shlex
import shutil
import socket
import tempfile
import time
from pathlib import Path


@contextlib.contextmanager
def stdin_a_terminal():
    """A pty to hand a child as stdin: the state an answered picker needs (#725).

    Since GH-725 a picker is only opened where a terminal can carry it — stdin's
    own or the controlling one — and without one the command refuses by name. A
    test that drives a pick must therefore give stdin a terminal; stdout and
    stderr may stay pipes, so what the command printed is still separable. The
    child does not become a session leader: it needs `isatty(0)`, not a
    controlling terminal, and tests that need the latter use run_pty."""
    master, slave = pty.openpty()
    try:
        yield slave
    finally:
        os.close(slave)
        os.close(master)


# A hang detector, not a performance bound: the longest any test waits on a
# spawned process, sized so (in principle) only a real hang reaches it. It sits
# above pytest.ini's 60 s per-test cap so that no site is stricter than the
# literal it replaced: the cap reports a single hung wait first, with a thread
# dump, while this bound holds the waits the cap cannot see — a test that polls a
# spawned process several times over, where the cap would fire in the middle of
# the polling.
#
# GH-976: 120 s was a quiet-host number. On the night gate's own load a single
# `bin/ccmorph run gw` child crossed it (#932's three marked adapter tests died
# of `TimeoutExpired` while the test still had budget), and the #932 fix was only
# as good as this bound: a marked test whose child is killed first can never
# spend its marker. The child's bound is therefore raised to 240 s — twice the
# observed death point — and every test that may wait on a child gets
# HANG_TIMEOUT + SPAWN_SLACK from conftest.py, so #932's 300 s budget stays the
# budget while the child can now spend four fifths of it.
#
# One knob governs the chain: override CCMORPH_TEST_HANG_TIMEOUT and both the
# child's bound and every derived per-test budget move with it.
HANG_TIMEOUT = float(os.environ.get("CCMORPH_TEST_HANG_TIMEOUT", "240"))

# The named slack between the child's own bound and the test's: room for the
# fixture lifecycle, further sequential children and report construction after a
# child hits its bound. 60 s is pytest.ini's old per-test ceiling, so the raised
# budget is HANG_TIMEOUT + the minute a whole test used to get.
SPAWN_SLACK = 60.0
# The pytest per-test budget every test that can wait on a real process carries.
# conftest.py applies it to the population found by spawn_bound_tests() below;
# hang_bound_test.py checks the effective value on every collected item.
SPAWN_TEST_BUDGET = HANG_TIMEOUT + SPAWN_SLACK


def wait_until(pred, timeout, interval=0.05):
    """Poll `pred` until it answers true, giving up after `timeout` seconds.

    Returns whether it was seen true, so the caller names its own failure. The
    give-up semantics live here once: a monotonic deadline, a short sleep between
    looks, and one last look past the bound. `timeout` is the suite's shared hang
    bound, never a literal a test picked (hang_bound_test.py holds that line)."""
    deadline = time.monotonic() + timeout
    while True:
        if pred():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(interval)


# --- The spawn-bound population ---------------------------------------------
#
# GH-976: #932 marked eighteen cases by hand, but the population a per-test
# budget exists for is "every test that can wait on a real process", and the
# night gate killed the cases the hand list missed in the same four files. The
# walk below derives the population from the same spawn forms hang_bound_test.py
# already polices; conftest.py gives every member SPAWN_TEST_BUDGET and the
# meta-check holds the line, so a new spawn-bound test is covered without an
# enumeration that can fall behind.

# Every call that starts a real process: the synchronous spawners that take
# `timeout=`, `Popen` (whose bound belongs on the wait that follows), the
# `os.*` spawners, `pty.spawn`, and the two bare names the suite uses for a
# process it starts itself. A local helper named `run` is not one of these.
RUN_CALLS = {"run", "check_output", "check_call", "call"}
SPAWN_CALLS = RUN_CALLS | {"Popen", "system", "spawnv", "spawnvp", "spawnl",
                           "spawnle", "posix_spawn", "run_pty"}
SPAWN_MODULES = ("subprocess", "os", "pty")
BARE_SPAWNS = {"Popen", "run_pty"}

# pytest's own default python_files: a test module is collected under either
# spelling, so the budget hook and the check must see both.
TEST_NAME_PATTERNS = ("test_*.py", "*_test.py")


def is_test_module(name):
    """Whether pytest collects `name` as a test module."""
    return name.endswith(".py") and (name.startswith("test_") or name.endswith("_test.py"))


def call_names(func):
    """('subprocess', 'run') for `subprocess.run`; ('run_pty',) for a bare name."""
    parts = []
    while isinstance(func, ast.Attribute):
        parts.append(func.attr)
        func = func.value
    if isinstance(func, ast.Name):
        parts.append(func.id)
    return tuple(reversed(parts))


def callee(func):
    names = call_names(func)
    return names[-1] if names else None


def is_spawn(func):
    names = call_names(func)
    if not names or names[-1] not in SPAWN_CALLS:
        return False
    if len(names) == 1:
        return names[-1] in BARE_SPAWNS
    return names[0] in SPAWN_MODULES


# The xunit lifecycle names a class or module can spawn from; a test in that
# scope waits on the child too, so it gets the budget.
SETUP_NAMES = {"setup_module", "teardown_module", "setup_class", "teardown_class",
               "setup_method", "teardown_method", "setup", "teardown",
               "setUp", "setUpClass", "tearDown", "tearDownClass"}


def _is_fixture(func):
    for deco in func.decorator_list:
        if callee(deco.func if isinstance(deco, ast.Call) else deco) == "fixture":
            return True
    return False


def _fixture_autouse(func):
    for deco in func.decorator_list:
        if isinstance(deco, ast.Call) and callee(deco.func) == "fixture":
            return any(k.arg == "autouse" and isinstance(k.value, ast.Constant)
                       and k.value.value for k in deco.keywords)
    return False


def _params(func):
    args = list(func.args.posonlyargs) + list(func.args.args) + list(func.args.kwonlyargs)
    return {a.arg for a in args} - {"self", "cls"}


def _uses(func):
    """Names a test asks for through `pytest.mark.usefixtures` (none today, but a
    fixture spawn must still reach a test that names it this way)."""
    names = set()
    for deco in func.decorator_list:
        if not isinstance(deco, ast.Call) or call_names(deco.func)[:2] != ("pytest", "mark"):
            continue
        if callee(deco.func) != "usefixtures":
            continue
        names.update(a.value for a in deco.args
                     if isinstance(a, ast.Constant) and isinstance(a.value, str))
    return names


class _Element:
    """The element a `for` target is bound to; `value` is the container."""

    def __init__(self, value):
        self.value = value


def _scope_nodes(func):
    """The nodes in `func`'s own scope; a nested function's locals are its own."""
    stack = [func]
    while stack:
        node = stack.pop()
        yield node
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda, ast.ClassDef)):
                continue
            stack.append(child)


def _bindings(func):
    """Local name -> the values bound to it in `func`'s own scope."""
    bound = {}
    for node in _scope_nodes(func):
        if isinstance(node, ast.Assign):
            for target in node.targets:
                if isinstance(target, ast.Name):
                    bound.setdefault(target.id, []).append(node.value)
        elif isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.value is not None:
            bound.setdefault(node.target.id, []).append(node.value)
        elif isinstance(node, (ast.For, ast.AsyncFor)) and isinstance(node.target, ast.Name):
            bound.setdefault(node.target.id, []).append(_Element(node.iter))
        elif isinstance(node, (ast.With, ast.AsyncWith)):
            for item in node.items:
                if isinstance(item.optional_vars, ast.Name):
                    bound.setdefault(item.optional_vars.id, []).append(item.context_expr)
        elif isinstance(node, ast.NamedExpr) and isinstance(node.target, ast.Name):
            bound.setdefault(node.target.id, []).append(node.value)
    return bound


def _suite_names(tree):
    """Names referring to something this checkout defines: module functions,
    classes, and imports from sibling test modules, support and the fake."""
    names = {node.name for node in tree.body
             if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))}
    for node in tree.body:
        if isinstance(node, ast.ImportFrom):
            module = (node.module or "").split(".")[0]
            if module.endswith("_test") or module in ("support", "fake_adapter"):
                names.update(alias.asname or alias.name for alias in node.names)
    return names


def _external_imports(tree):
    """Names bound by imports of what the suite does not define (threading,
    pathlib, subprocess): the receiver gate's evidence that an object was not
    built by a suite helper."""
    external = set()
    for node in tree.body:
        if isinstance(node, ast.Import):
            for alias in node.names:
                root = alias.name.split(".")[0]
                if not (root.endswith("_test") or root in ("support", "fake_adapter")):
                    external.add(alias.asname or root)
        elif isinstance(node, ast.ImportFrom):
            root = (node.module or "").split(".")[0]
            if not (root.endswith("_test") or root in ("support", "fake_adapter")):
                external.update(alias.asname or alias.name for alias in node.names)
    return external


def _value_external(value, bound, suite, external, seen=frozenset()):
    """Whether `value` is something the suite did not construct: a thread, a
    socket, a Path. A local bound to one is gated out of the name fallback, so
    `thread.start()` cannot resolve to a same-named suite helper."""
    if isinstance(value, _Element):
        return _value_external(value.value, bound, suite, external, seen)
    if isinstance(value, ast.Name):
        name = value.id
        if name in seen:
            return name not in suite
        if name in bound:
            return all(_value_external(v, bound, suite, external, seen | {name})
                       for v in bound[name])
        return name not in suite
    if isinstance(value, ast.Call):
        names = call_names(value.func)
        if not names:
            return True
        if len(names) > 1 and names[0] in external:
            return True
        return names[0] not in suite and names[-1] not in suite
    if isinstance(value, (ast.ListComp, ast.SetComp, ast.GeneratorExp)):
        return _value_external(value.elt, bound, suite, external, seen)
    if isinstance(value, (ast.List, ast.Tuple, ast.Set)):
        return all(_value_external(e, bound, suite, external, seen) for e in value.elts)
    if isinstance(value, ast.Subscript):
        return isinstance(value.value, ast.Name) and _value_external(
            value.value, bound, suite, external, seen)
    if isinstance(value, ast.Attribute):
        root, names = value, call_names(value)
        while isinstance(root, ast.Attribute):
            root = root.value
        if isinstance(root, ast.Name) and root.id in bound:
            return _value_external(root, bound, suite, external, seen)
        return bool(names) and (names[0] in external or names[0] not in suite)
    if isinstance(value, ast.BinOp):
        return (_value_external(value.left, bound, suite, external, seen)
                or _value_external(value.right, bound, suite, external, seen))
    return True


def _stubbed_names(func):
    """Attribute names this body monkeypatches: `monkeypatch.setattr(obj, "name",
    fake)` replaces the real helper for this test, so a name the body stubs is
    not a process it starts."""
    names = set()
    for node in ast.walk(func):
        if isinstance(node, ast.Call) and callee(node.func) == "setattr" and len(node.args) >= 2:
            arg = node.args[1]
            if isinstance(arg, ast.Constant) and isinstance(arg.value, str):
                names.add(arg.value)
    return names


def _named_defs(tree):
    """Every module function and class method in `tree`, by name. The walk cannot
    know a receiver's class, so two bodies of one name union: a name reaches a
    spawn when any body of that name does."""
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            yield node.name, node
        elif isinstance(node, ast.ClassDef):
            for item in node.body:
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    yield item.name, item


def _graph(tagged):
    """(direct_test, direct_script, children, stubs) for every helper name.

    `tagged` pairs each tree with whether it is a loaded production script.
    `direct_*` name the literal spawn forms a body contains — separately for a
    test-side module and for a script — so a test that stubs `Popen` on a loaded
    script's module suppresses that script's real start while the test's own
    `subprocess.run` still counts. `children` are the names a body calls, with a
    method call on a value the suite did not build (a thread, a Path) left out;
    `stubs` are the names it monkeypatches away.

    Following the four together is what makes a loaded script's `strip_env()` the
    same wait as a `subprocess.run` in a test, while `thread.start()` is not a
    spawn at all and a stubbed sidecar start is not a wait."""
    direct_test, direct_script, children, stubs = {}, {}, {}, {}
    for tree, script in tagged:
        if tree is None:
            continue
        suite = _suite_names(tree)
        external = _external_imports(tree)
        direct = direct_script if script else direct_test
        for name, func in _named_defs(tree):
            direct.setdefault(name, set()).update(
                callee(node.func) for node in ast.walk(func)
                if isinstance(node, ast.Call) and is_spawn(node.func))
            children.setdefault(name, set())
            stubs.setdefault(name, set()).update(_stubbed_names(func))
            bound = _bindings(func)
            for node in ast.walk(func):
                if not isinstance(node, ast.Call):
                    continue
                names = call_names(node.func)
                if not names:
                    continue
                if (len(names) > 1 and names[0] not in ("self", "cls")
                        and names[0] in bound
                        and _value_external(ast.Name(id=names[0], ctx=ast.Load()),
                                            bound, suite, external)):
                    continue
                children[name].add(names[-1])
    return direct_test, direct_script, children, stubs


def spawn_graph(*trees):
    """The call graph of these test-side modules; the walk's own tests read it."""
    return _graph((tree, False) for tree in trees)


def _proof(name, direct_test, direct_script, children, stubbed=frozenset(), seen=None):
    """Whether a call to `name` can reach a process start without passing through
    a name this test has stubbed out. A script's own spawn form is suppressed by
    a stub of its name; a test-side module's spawn form is not — that is code the
    test itself runs."""
    if name in stubbed:
        return False
    if seen is None:
        seen = set()
    if name in seen:
        return False
    if direct_test.get(name):
        return True
    if any(spawn not in stubbed for spawn in direct_script.get(name, ())):
        return True
    seen.add(name)
    return any(_proof(child, direct_test, direct_script, children, stubbed, seen)
               for child in children.get(name, ()))


def spawn_bound_tests(tree, graph=None):
    """The qualnames (`test_x`, `Class.test_x`) of every test in `tree` that can
    wait on a process the suite starts.

    A test is bound when its own body reaches a spawn form through the calls it
    makes, when it requests a spawn-bound fixture (directly or transitively), or
    when its class's setup/teardown reaches one. The walk is transitive through
    the suite's call graph, so `launch.run()`, a helper imported from a sibling
    test file, a fixture, and a production script loaded by `load_script` all
    count as the same wait as a `subprocess.run` in the test's own body. Calls a
    test monkeypatches away are not waits for that test, and a method call on an
    object the suite did not build (a `threading.Thread`) is not one either."""
    direct_test, direct_script, children, stubs = (
        graph if graph is not None else _graph([(tree, False)]))
    module_funcs, class_methods = {}, {}
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            module_funcs[node.name] = node
        elif isinstance(node, ast.ClassDef):
            class_methods[node.name] = {
                item.name: item for item in node.body
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef))}
    local_names = set(module_funcs) | {name for methods in class_methods.values()
                                       for name in methods}

    fixtures = {name: func for name, func in module_funcs.items() if _is_fixture(func)}
    fixture_defs = {("", name): func for name, func in fixtures.items()}
    for cls, methods in class_methods.items():
        for name, func in methods.items():
            if _is_fixture(func):
                fixture_defs[(cls, name)] = func
    spawn_fixtures = {key for key, func in fixture_defs.items()
                      if _proof(key[1], direct_test, direct_script, children,
                             stubs.get(key[1], frozenset()))}
    changed = True
    while changed:
        changed = False
        for key, func in fixture_defs.items():
            if key in spawn_fixtures:
                continue
            if any((key[0], name) in spawn_fixtures or ("", name) in spawn_fixtures
                   for name in _params(func)):
                spawn_fixtures.add(key)
                changed = True

    def fixture_for(cls, name):
        if cls and (cls, name) in fixture_defs:
            return (cls, name)
        if ("", name) in fixture_defs:
            return ("", name)
        return None

    def requested(cls, func):
        names = _params(func) | _uses(func)
        return any(fixture_for(cls, name) in spawn_fixtures for name in names)

    autouse_keys = {key for key, func in fixture_defs.items() if _fixture_autouse(func)}

    def autouse(cls):
        return any(key in spawn_fixtures and key[0] in ("", cls) for key in autouse_keys)

    def module_setup():
        return any(name in module_funcs and _proof(name, direct_test, direct_script, children)
                   for name in SETUP_NAMES)

    def class_setup(cls):
        return any(name in class_methods.get(cls, {}) and _proof(name, direct_test, direct_script, children)
                   for name in SETUP_NAMES)

    def test_stubs(name):
        """The stubs that apply to this test: its own body's and those of the
        same-file helpers it calls (a rig that stubs the real spawn)."""
        stubbed, seen, work = set(stubs.get(name, ())), {name}, [name]
        while work:
            for child in children.get(work.pop(), ()):
                if child in local_names and child not in seen:
                    seen.add(child)
                    stubbed |= stubs.get(child, set())
                    work.append(child)
        return stubbed

    found = set()
    for name, func in module_funcs.items():
        if name.startswith("test_") and (
                _proof(name, direct_test, direct_script, children, test_stubs(name)) or module_setup()
                or requested("", func) or autouse("")):
            found.add(name)
    for cls, methods in class_methods.items():
        for name, func in methods.items():
            if name.startswith("test_") and (
                    _proof(name, direct_test, direct_script, children, test_stubs(name)) or class_setup(cls)
                    or requested(cls, func) or autouse(cls)):
                found.add(f"{cls}.{name}")
    return found


_SPAWN_BOUND_CACHE = {}
_SUITE_GRAPH_CACHE = {}
_PARSE_CACHE = {}


def _parsed(path):
    """The file's AST, parsed once per process (None when it does not parse)."""
    key = str(Path(path).resolve())
    if key not in _PARSE_CACHE:
        try:
            _PARSE_CACHE[key] = ast.parse(Path(key).read_text())
        except (OSError, SyntaxError):
            _PARSE_CACHE[key] = None
    return _PARSE_CACHE[key]


def _test_module_paths(tests_dir):
    """Every file pytest would collect in `tests_dir`, under either spelling."""
    found = {}
    for pattern in TEST_NAME_PATTERNS:
        for path in Path(tests_dir).glob(pattern):
            found[str(path.resolve())] = path
    return [found[key] for key in sorted(found)]


def _path_parts(expr, constants, depth=0):
    """The literal path parts of `ROOT / "addons" / "x"`; [] when the root is a
    runtime `Path(__file__)` and None when the path is fully dynamic."""
    if depth > 8:
        return None
    if isinstance(expr, ast.Constant) and isinstance(expr.value, str):
        return [expr.value]
    if isinstance(expr, ast.Name):
        return (_path_parts(constants[expr.id], constants, depth + 1)
                if expr.id in constants else None)
    if isinstance(expr, ast.BinOp) and isinstance(expr.op, ast.Div):
        right = _path_parts(expr.right, constants, depth + 1)
        if right is None:
            return None
        return (_path_parts(expr.left, constants, depth + 1) or []) + right
    if isinstance(expr, ast.Attribute):
        if expr.attr in ("parent", "resolve", "absolute", "expanduser"):
            return _path_parts(expr.value, constants, depth + 1)
        return None
    if isinstance(expr, ast.Subscript):
        return _path_parts(expr.value, constants, depth + 1)
    if isinstance(expr, ast.Call):
        return []                     # Path(__file__) names no literal part
    return None


def _loaded_scripts(tree, tests_dir):
    """The checkout files a test module loads with `load_script`, resolved from
    its literal path (against the checkout root, then the tests directory)."""
    root = Path(tests_dir).resolve().parent
    constants = {}
    for node in tree.body:
        if isinstance(node, ast.Assign):
            for target in node.targets:
                if isinstance(target, ast.Name):
                    constants[target.id] = node.value
    for node in ast.walk(tree):
        if not (isinstance(node, ast.Call) and callee(node.func) == "load_script" and node.args):
            continue
        parts = _path_parts(node.args[0], constants)
        if not parts:
            continue
        for base in (root, Path(tests_dir)):
            candidate = base.joinpath(*parts)
            if candidate.is_file():
                yield candidate
                break


def _suite_graph(tests_dir):
    """The call graph of every test module and every script they load, once per
    process. A production helper reached through `load_script` (`rc.strip_env()`
    -> ccmorph-rc's `subprocess.run`) is part of the same suite-wide relation as
    a helper imported from a sibling test file."""
    key = str(Path(tests_dir).resolve())
    if key not in _SUITE_GRAPH_CACHE:
        tagged = {}
        for path in _test_module_paths(key):
            tree = _parsed(path)
            tagged[str(path.resolve())] = (tree, False)
            if tree is None:
                continue
            for script in _loaded_scripts(tree, key):
                tagged.setdefault(str(script.resolve()), (_parsed(script), True))
        _SUITE_GRAPH_CACHE[key] = _graph(tagged.values())
    return _SUITE_GRAPH_CACHE[key]


def spawn_bound_names(path):
    """`spawn_bound_tests` for the file at `path`, parsed once per process."""
    key = str(Path(path).resolve())
    if key not in _SPAWN_BOUND_CACHE:
        tree = _parsed(key)
        _SPAWN_BOUND_CACHE[key] = (set() if tree is None else
                                   spawn_bound_tests(tree, _suite_graph(Path(key).parent)))
    return _SPAWN_BOUND_CACHE[key]


def load_script(path, name):
    """Load an extensionless script as a module under `name`, without importing it
    through sys.path or keeping it in sys.modules.

    The add-ons and the usage exporter ship as scripts, not as importable .py
    modules, so a test that exercises one runs it by path. Each call returns a
    fresh module, so a test may change the environment before loading."""
    loader = importlib.machinery.SourceFileLoader(name, str(path))
    spec = importlib.util.spec_from_loader(name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


# The one sentence, with the one link, for a claude no PATH search can find
# (#282). The installer, `ccmorph run` and `ccmorph doctor` print these same
# bytes, so a missing Claude Code meets a person with one answer wherever it
# finds them; the install suite ties the installer's copy to the core binary.
CLAUDE_MISSING = ("Claude Code is not installed or not on the PATH — install it: "
                  "https://docs.anthropic.com/en/docs/claude-code/setup")

# Source for a fake HTTP server the tests write out and run as its own process,
# placed after its http.server import. HTTPServer.server_bind resolves 127.0.0.1
# with socket.getfqdn, a reverse DNS lookup that can stall for tens of seconds on
# macOS; conftest.py patches that away for in-process servers, and a subprocess
# needs the same line itself. No fake here uses the name it would find.
FAKE_SERVER_NO_DNS = "import socket\nsocket.getfqdn = lambda name='': name\n"

# Tools whose real copy on the host must not answer a test that seeded neither:
# the picker falls back to fzf when it finds one, and the add menu reads its
# provider rows from cliproxyapi on PATH, so a machine that carries either
# decides rows and widths the test never asked for (#414). A test that needs
# one writes its own stub, in a directory in front of this PATH.
HERMETIC_TOOLS = ("fzf", "cliproxyapi")

# One mirror directory per (real directory, hidden names), made once per process:
# every test env that hides the same tools from the same directory reuses it.
_MIRRORED_PATHS = {}


def _mirror_without(entry, hidden):
    """A directory linking every executable in `entry` except the `hidden` names."""
    try:
        real = Path(entry).resolve()
    except OSError:
        return None
    key = (str(real), frozenset(hidden))
    if key in _MIRRORED_PATHS:
        return _MIRRORED_PATHS[key]
    try:
        names = os.listdir(real)
    except OSError:
        return None
    target = Path(tempfile.mkdtemp(prefix="ccmorph-path-"))
    atexit.register(shutil.rmtree, str(target), True)
    for name in names:
        if name in hidden:
            continue
        source = real / name
        try:
            if source.is_file() and os.access(source, os.X_OK):
                (target / name).symlink_to(source)
        except OSError:
            continue
    _MIRRORED_PATHS[key] = str(target)
    return str(target)


def path_without(*tools, path=None):
    """PATH minus `tools` (default: HERMETIC_TOOLS), keeping every other tool.

    An entry carrying one of `tools` is replaced by a directory of links to its
    other executables: dropping the entry whole would also drop zsh and the
    coreutils a test's shell needs, and a merged-/usr Linux keeps both fzf and
    zsh in /usr/bin (#414). The caller puts its own stub directory first, in
    front of this result."""
    entries = (os.environ.get("PATH", "") if path is None else path).split(os.pathsep)
    hidden = tools or HERMETIC_TOOLS
    kept, seen = [], set()
    for entry in entries:
        if not entry:
            continue
        if any((Path(entry) / tool).exists() for tool in hidden):
            # A relative entry cannot be mirrored from a test's directory and
            # then mean the same thing to the child; hiding wins.
            if not os.path.isabs(entry):
                continue
            entry = _mirror_without(entry, hidden)
            if entry is None:
                continue
        if entry in seen:
            continue
        seen.add(entry)
        kept.append(entry)
    return os.pathsep.join(kept)


def anthropic_accepts_the_key(bindir):
    """Write bindir/curl for a test that launches a native apikey profile.

    Such a launch asks Anthropic whether its key works before the session opens
    (#255). A test's key is a placeholder Anthropic would reject, and a test must
    not reach the network to find that out: this curl answers a request to
    api.anthropic.com as Anthropic answers a working key (HTTP 200, an empty
    model list) and hands every other request to the real curl, so a test's own
    gateway or loopback server is reached exactly as before."""
    bindir = Path(bindir)
    elsewhere = os.pathsep.join(p for p in os.environ.get("PATH", "").split(os.pathsep)
                                if p and Path(p).resolve() != bindir.resolve())
    real = shutil.which("curl", path=elsewhere) or "/usr/bin/curl"
    curl = bindir / "curl"
    curl.write_text(
        "#!/bin/sh\n"
        "for a do case \"$a\" in https://api.anthropic.com/*)\n"
        "  printf '%s' '{\"data\":[]}'\n"
        "  for b do case \"$b\" in *http_code*) printf '\\n__CCMORPH_STATUS__200' ;; esac; done\n"
        "  exit 0 ;; esac; done\n"
        f"exec {shlex.quote(real)} \"$@\"\n")
    curl.chmod(0o755)


_POOL_LOW, _POOL_HIGH = 1024, 32768


def _pool_slice() -> tuple:
    """The slice of the pool this process may draw from, as (low, high).

    The probe releases a number before the test's child binds it, so on one
    shared draw space a parallel worker can draw the very number and be up
    with its listener on the route another test expects to be dead (#877 —
    `rc_bridge_failclosed_test` reroutes answered by a sibling's proxy).
    xdist tells every worker who it is and how many the run has, so each
    worker gets one even slice of the pool and no sibling can draw its
    number at all. Without a usable worker identity — serial (-n0, CI), or
    an env the pool cannot slice by: a number that is not one, an index the
    run does not have, or a count with no number left for every worker —
    the whole pool comes back, as before. The fallback is all-or-none:
    a partial slicing would leave one worker on the full pool while a
    sibling draws inside it, which is the sharing this pool exists to
    stop."""
    low, high = _POOL_LOW, _POOL_HIGH
    worker = os.environ.get("PYTEST_XDIST_WORKER", "")
    if worker.startswith("gw"):
        try:
            index = int(worker[2:])
            count = int(os.environ.get("PYTEST_XDIST_WORKER_COUNT", "1"))
        except ValueError:
            return low, high
        span = high - low
        # count <= span keeps every worker's slice nonempty; above it some
        # slices vanish and those workers fall back while others slice —
        # a full pool and a stub inside it, which can meet on one number.
        if 0 <= index < count <= span:
            low, high = (low + span * index // count,
                         low + span * (index + 1) // count)
    return low, high


def port_outside_the_ephemeral_pool() -> int:
    """A loopback port number for a test whose subject binds a port the test
    chose — the adapter's --port, a shim's listen port, a route that must stay
    dead — where no parallel process can be handed the number while the test
    waits for that bind. A number drawn by binding port 0 comes from the OS's
    ephemeral range (49152-65535 on macOS, 32768-60999 on Linux), the same
    system-wide pool every parallel gate process draws from, so between the
    draw and the bind another worker's listener, shim or outbound source port
    can take it and a full gate fails a test that is green alone (#176, #188).
    A number below every range's floor cannot be drawn by a bind-to-zero at
    all; the probe skips the fixed services that do live down there, and a
    bind that loses its number draws again. Parallel xdist workers draw
    worker-disjoint slices of this range, so a number a sibling test is about
    to bind is never on this worker's list either (#877)."""
    low, high = _pool_slice()
    for _ in range(64):
        p = random.randrange(low, high)
        s = socket.socket()
        try:
            s.bind(("127.0.0.1", p))
        except OSError:
            continue                       # a fixed service holds the number
        finally:
            s.close()
        return p
    raise RuntimeError("no port below the ephemeral ranges is free on 127.0.0.1")


# Issue #611: a hive sandbox refuses loopback binds at the syscall, so every
# listener-based test errors inside its own fixture and a Handoff spends its
# lines re-discovering the same environment fact. The probe below answers that
# fact once per process, and the `needs_loopback` marker turns the refusal
# into one loud SKIP line per test instead of a wall of identical failures.
# Only the sandbox's own permission refusal (EPERM/EACCES) is a capability
# fact: every other bind error — an exhausted fd table, a full backlog — is a
# transient resource failure and must stay the visible failure it was, never
# a skipped suite. Where binds are allowed — every CI job, and any session
# with sandbox.network.allowLocalBinding — the probe answers True and nothing
# skips.
LOOPBACK_SKIP = "SKIP: loopback bind denied in this sandbox (runs in CI)"

_LOOPBACK_BIND = None  # None until the probe's one answer; then True/False.


def can_bind_loopback():
    """Whether a test may bind a loopback port here. Draws port 0, releases
    it, and remembers the answer — one bind per process, because a permission
    refusal is stable for the life of the process (the sandbox's policy, or
    its absence) and a bind that once succeeded on the machine keeps working.
    A permission refusal (EPERM/EACCES, which Python both map to
    PermissionError) is the only thing cached as False: any other error
    propagates with its own face and caches nothing, so a transient resource
    failure retries and surfaces instead of quietly skipping a suite."""
    global _LOOPBACK_BIND
    if _LOOPBACK_BIND is None:
        try:
            s = socket.socket()
            try:
                s.bind(("127.0.0.1", 0))
            finally:
                s.close()
        except PermissionError:
            _LOOPBACK_BIND = False
        else:
            _LOOPBACK_BIND = True
    return _LOOPBACK_BIND


def store_month_first(instant):
    """The first UTC-named day quota's month reader opens at `instant`.
    quota/spend.go steps the month's start as a LOCAL midnight turned into an
    instant and names its window in UTC day names, so a fixture that places a
    day by the UTC calendar does not know the window a reader east or west of
    UTC actually opens: the first day is the UTC date of the instant's local
    month start, not of its UTC month start (#804).

    The local zone is the one a reader answers from: every reader here is a
    fresh process that reads TZ from the environment at its start, while this
    interpreter froze its C copy at startup and rereads it only on tzset().
    Rereading first keeps the fixture and the binary on the same zone even
    when a test sets TZ after startup; tzset is Unix-only, and so is this
    suite (flock, mkfifo)."""
    time.tzset()
    local = instant.astimezone()
    midnight = local.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    return midnight.astimezone(dt.timezone.utc).date()


# The compiled core (`make shim`): adapter, shim and helper verbs are its subcommands.
CORE = Path(__file__).resolve().parent.parent / "libexec/ccmorph-core"
# argv prefix of the stock shim.
SHIM = [str(CORE), "shim"]
# Module-level `pytestmark = needs_shim` skips a file that spawns the compiled shim.
# pytest is imported here, not at the top: the zsh suite imports this module
# for HANG_TIMEOUT under a python3 that has no pytest.
def adapter_cmd(binary=None):
    """argv prefix that runs the adapter under test, optionally from a copied `binary`."""
    return [str(binary or CORE), "adapter"]


def helper_cmd(name, binary=None):
    """argv prefix that runs the helper `name` under test, optionally from a copied `binary`."""
    return [str(binary or CORE), name]


def proxy_took(rc):
    """ccmorph-rc's handoff_taken as the shared edge's proxy answers it (issue #595):
    the proxy says it holds the handoff -- held.<edge>.json names its id, as the Go edge
    writes it -- and the file stays, so a test can read what was handed over."""
    import json

    def took(hand, payload, *a):
        rec = json.loads(payload)
        held = rc.STATE / f"held.{rec['edge']}.json"
        try:
            have = json.loads(held.read_text())["profiles"]
        except (OSError, ValueError, KeyError):
            have = {}
        have[hand.name[len("switch-"):-len(".json")]] = rec["id"]
        held.write_text(json.dumps({"edge": rec["edge"], "edge_started": rec["edge_started"], "profiles": have}))
        return True
    return took


def __getattr__(name):
    if name == "needs_shim":
        import pytest
        return pytest.mark.skipif(not CORE.exists(), reason=f"{CORE} is not built: run `make shim` first")
    if name == "needs_loopback":
        import pytest
        return pytest.mark.skipif(not can_bind_loopback(), reason=LOOPBACK_SKIP)
    raise AttributeError(name)
