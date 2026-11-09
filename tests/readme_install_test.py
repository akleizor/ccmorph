"""The README's commands against a fresh install (#261).

The Usage and cost section told every reader to run `scripts/usage-export`, a
file only a source checkout has: a Homebrew or one-line install dies on the
first token of the very command the README hands it. Every command the README
names — in an ```sh block or in a backticked span with arguments — must be
found for real by a fresh install, or be marked as clone-only:

- `ccmorph <verb>`: the installed CLI must know the verb;
- a path: an executable the install placed (`~/.config/ccmorph/...`, or the
  Homebrew prefix's `libexec/`, which holds the same tree). `./...` is the
  clone-only mark and is checked against the checkout instead;
- a bare tool: `command -v` must find a real executable or shell builtin on a
  reader's PATH: the fresh install's `~/.local/bin`, then the system's
  `/usr/bin` and `/bin`; `brew` is also looked for in Homebrew's own
  prefixes, where its installer puts it. The check writes nothing onto that
  PATH, so a tool no install route and no system provides — `ccusage` — is
  reported whatever the README says about it.
"""
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
README = ROOT / "README.md"
# A leading `NAME=value` assignment prefixes the command that really runs.
ENV_ASSIGNMENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*=")
INLINE_SPAN = re.compile(r"`([^`\n]+)`")
# The installer fixture's stand-ins for claude and fzf; never on a reader's PATH.
STUB = "#!/bin/sh\nexit 0\n"
# Homebrew keeps the runtime in the formula prefix's libexec/: the tree the
# installer copies into ~/.config/ccmorph.
BREW_LIBEXEC = "$(brew --prefix ccmorph)/libexec/"
# A reader's PATH right after a fresh install: the install's ~/.local/bin,
# then the system's own utilities. Homebrew is not a system utility: its
# installer puts `brew` in its own prefix (Apple silicon, Intel macOS, Linux;
# https://docs.brew.sh/Installation), so brew alone is also looked for there:
# those directories hold whatever a host installed, which must not pass.
SYSTEM_PATH = ("/usr/bin", "/bin")
HOMEBREW_BIN = ("/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin")


def _split(piece):
    try:
        return shlex.split(piece)
    except ValueError:
        return piece.split()


def _commands_in(line):
    """(command, arguments) for each command one line of shell runs."""
    for piece in re.split(r"&&|\|\||\||;", line):
        words = _split(piece)
        if words[:1] == ["!"]:  # the Claude Code prompt's shell escape
            words = words[1:]
        while words and ENV_ASSIGNMENT.match(words[0]):
            words = words[1:]
        if words:
            yield words[0], words[1:]


def _readme_commands(text):
    """(line, command, arguments) for every command the README names.

    An ```sh block runs every line it holds (continuations joined); the README's
    plain fences — the hop prompt, the diagram — are not shell and are skipped.
    Prose names commands in single backticks: a span counts as one when it has
    arguments, unless its first word ends with `:`, so `costSource: estimate`
    and `~/.config/ccmorph` are not commands.
    """
    commands = []
    fence = None
    pending = ""
    for number, line in enumerate(text.splitlines(), 1):
        stripped = line.strip()
        if stripped.startswith("```"):
            fence = None if fence is not None else stripped[3:].strip()
            pending = ""
            continue
        if fence == "sh":
            if not stripped or stripped.startswith("#"):
                continue
            if stripped.endswith("\\"):
                pending += stripped[:-1] + " "
                continue
            for command, args in _commands_in(pending + stripped):
                commands.append((number, command, args))
            pending = ""
        elif fence is None:
            for span in INLINE_SPAN.findall(line):
                for command, args in _commands_in(span):
                    if args and not command.endswith(":"):  # `costSource: estimate` is a field
                        commands.append((number, command, args))
    return commands


class ReadmeInstallTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        td = tempfile.TemporaryDirectory()
        cls.addClassCleanup(td.cleanup)
        cls.home = home = Path(td.name)
        stubs = home / "stub-bin"
        stubs.mkdir()
        for tool in ("claude", "fzf"):
            stub = stubs / tool
            stub.write_text(STUB)
            stub.chmod(0o755)
        # The installer must find `claude` and may offer `fzf`; stdin closed, so
        # an offer declines instead of blocking. XDG_CONFIG_HOME matches the
        # README's `~/.config/ccmorph` spelling of the install root.
        cls.env = {"HOME": str(home), "ZDOTDIR": str(home),
                   "XDG_CONFIG_HOME": str(home / ".config"),
                   "PATH": f"{stubs}:/usr/bin:/bin"}
        installed = subprocess.run(["zsh", str(ROOT / "install.sh")], env=cls.env,
                                   input="", text=True, capture_output=True,
                                   timeout=HANG_TIMEOUT)
        assert installed.returncode == 0, installed.stdout + installed.stderr
        cls.config = home / ".config/ccmorph"
        cls.cli = str(home / ".local/bin/ccmorph")

    @classmethod
    def _run_cli(cls, argv):
        return subprocess.run(argv, env=cls.env, input="", text=True,
                              capture_output=True, timeout=HANG_TIMEOUT)

    def _problems(self, text):
        problems = []
        for number, command, args in _readme_commands(text):
            reason = self._why_not_found(command, args)
            if reason:
                problems.append(f"line {number}: {reason}")
        return problems

    def _why_not_found(self, command, args):
        if command == "ccmorph":
            # The installed CLI is the oracle for its own verbs: `ccmorph
            # help <verb>` exits 0 only for a command it knows.
            if not args or args[0][0] in "<[-":
                return None
            result = self._run_cli([self.cli, "help", args[0]])
            if result.returncode == 0:
                return None
            return (f"'ccmorph {args[0]}' — the installed CLI has no such "
                    f"command (help exits {result.returncode})")
        if "/" in command or command.startswith("$"):
            if command.startswith("./"):
                found = ROOT / command[2:]  # spelled from the checkout: clone-only
            elif command.startswith("~/"):
                found = self.home / command[2:]
            elif command.startswith(BREW_LIBEXEC):
                found = self.config / command[len(BREW_LIBEXEC):]
            elif command.startswith(("/", "$")):
                return f"'{command}' — a path this check cannot place"
            else:
                found = self.config / command  # a bare relative path addresses the install
            if found.is_file() and os.access(found, os.X_OK):
                return None
            return f"'{command}' — a fresh install has no executable at {found}"
        dirs = (str(self.home / ".local/bin"),) + SYSTEM_PATH
        if command == "brew":
            dirs += HOMEBREW_BIN
        lookup = subprocess.run(["/bin/sh", "-c", 'command -v "$1"', "sh", command],
                                env={"HOME": str(self.home), "PATH": os.pathsep.join(dirs)},
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        if lookup.returncode == 0:
            return None
        return (f"'{command}' — not found on a reader's PATH: neither the fresh "
                f"install nor the system provides it")

    def test_every_command_the_readme_names_is_found_by_a_fresh_install(self):
        problems = self._problems(README.read_text(encoding="utf-8"))
        self.assertEqual(problems, [], "README names commands a fresh install "
                         "cannot run:\n  " + "\n  ".join(problems))

    def test_the_check_rejects_what_a_fresh_install_lacks(self):
        """The negative control: each way a README command can be missing is
        reported, in a block and inline, so a green check means something.
        Naming a tool in Requirements, or linking it, makes nothing found."""
        fence = "`" * 3
        sample = "\n".join([
            "## Requirements", "",
            "- Needs `nosuchtool-261` and [linked-261](https://example.com); naming a tool installs nothing.", "",
            "## Usage", "",
            f"{fence}sh", "ccmorph nosuchverb", "nosuchtool-261 --flag",
            "./missing-in-checkout", "~/.config/ccmorph/share/gone --output x", "sh -c true", fence,
            "Inline: `ccmorph nosuchinline now`, and",
            f'`"{BREW_LIBEXEC}share/gone" --output x`.',
            "A [linked-261](https://example.com) tool is not installed by a link: `linked-261 --flag`.",
            "An inline `nosuchtool-261 --flag` is checked too.",
            "A field is not a command: `costSource: estimate`.",
        ])
        problems = self._problems(sample)
        for needle in ("ccmorph nosuchverb", "'nosuchtool-261'", "'./missing-in-checkout'",
                       "share/gone", "ccmorph nosuchinline", "'linked-261'"):
            self.assertTrue(any(needle in p for p in problems), (needle, problems))
        self.assertEqual(sum("'nosuchtool-261'" in p for p in problems), 2, problems)
        self.assertEqual(len(problems), 8, problems)
        self.assertFalse(any("'sh'" in p for p in problems), problems)  # a real tool is found

    def test_a_ccusage_command_is_reported_though_the_requirements_name_it(self):
        """No install route and no system directory provides ccusage: naming it
        in Requirements and linking it, as the README does, leaves its command
        unfound on every host."""
        fence = "`" * 3
        sample = "\n".join([
            "## Requirements", "",
            "- Reading the usage export needs `ccusage`: get it from [ccusage](https://github.com/ccusage/ccusage).", "",
            "## Usage and cost", "",
            f"{fence}sh",
            "CLAUDE_CONFIG_DIR=~/.cache/ccmorph-usage ccusage claude daily --instances \\",
            "  --config ~/.cache/ccmorph-usage/ccusage.json",
            fence,
        ])
        problems = self._problems(sample)
        self.assertEqual(len(problems), 1, problems)
        self.assertIn("'ccusage'", problems[0])


if __name__ == "__main__":
    unittest.main()
