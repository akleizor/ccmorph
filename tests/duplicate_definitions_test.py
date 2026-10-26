"""A definition name binds once in its namespace; a second one silently kills the first.

Issue #454: `refresh_probe_test.py` defined `ProbeOutputStaysOnPoint` twice, 130
lines apart. Python runs both `class` statements and the second rebinds the name,
so every method of the first was collected as nothing at all -- the suite stayed
green while that half of the class could be broken or deleted with no effect. A
method defined twice inside one `TestCase` disappears the same way, so the check
reads class bodies too.
"""
import ast
from pathlib import Path

TESTS = Path(__file__).resolve().parent
BINDING = (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)


def rebindings(module):
    """`(name, [lineno, ...])` for each name bound more than once in one namespace.

    The two namespaces where a second binding hides a test from collection: the
    module's and a class body's. Inside a function body the second definition is
    a local that only that function overwrites, and no test vanishes with it.
    """
    scopes = [module.body] + [node.body for node in module.body
                              if isinstance(node, ast.ClassDef)]
    found = []
    for scope in scopes:
        lines = {}
        for node in scope:
            if isinstance(node, BINDING):
                lines.setdefault(node.name, []).append(node.lineno)
        found += [(name, seen) for name, seen in lines.items() if len(seen) > 1]
    return found


def test_a_name_bound_twice_in_a_class_body_is_reported():
    # No test file carries one today, so only this pins the class-body scope:
    # a second method of a TestCase hides the first from collection too.
    module = ast.parse("class T:\n    def m(self): pass\n    def m(self): pass\n")
    assert rebindings(module) == [("m", [2, 3])]


def test_no_test_file_binds_a_name_twice():
    offenders = [
        f"{path.name}: {name} on lines {', '.join(map(str, lines))}"
        for path in sorted(TESTS.glob("*_test.py"))
        for name, lines in rebindings(ast.parse(path.read_text()))
    ]
    assert not offenders, (
        "a second definition rebinds the name, so the tests under the first one "
        "never run: " + "; ".join(offenders))