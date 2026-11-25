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
  prefixes, where its installer puts it, and where a host carries none the
  check writes its own stub brew — brew is a documented prerequisite of one
  install route, not a command a fresh install runs, and the same way the
  fixture already stands in `claude`. Apart from that stub the check writes
  nothing onto that PATH, so a tool no install route and no system provides —
  `ccusage` — is reported whatever the README says about it.
"""
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

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
# those directories hold whatever a host installed, which must not pass. A
# brew found here or in the system dirs only proves the `brew` command
# itself is runnable: the check never executes a brew it did not plant or
# write — neither a system brew nor Homebrew's. Where Homebrew installed
# ccmorph, its real formula prefix is read from the disk instead, never asked:
# `brew --prefix ccmorph` is `<Homebrew's prefix>/opt/ccmorph` (#717).
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

    @classmethod
    def _command_on(cls, command, dirs):
        """What `command -v` says about COMMAND with PATH = dirs."""
        return subprocess.run(["/bin/sh", "-c", 'command -v "$1"', "sh", command],
                              env={"HOME": str(cls.home), "PATH": os.pathsep.join(dirs)},
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)

    def _brew_stub(self):
        """Stand in for brew on a host that carries none, the way the fixture
        already stands in `claude` and `fzf`: brew is a documented
        prerequisite of the Homebrew route, not a command a fresh install
        runs, so the host's lack of brew must not decide the README's
        verdict. The stub answers `--prefix ccmorph` with a prefix whose
        `libexec/` is the config root — the same tree the BREW_LIBEXEC
        branch maps — and exits 0 otherwise."""
        prefix = self.home / "brew-prefix"
        if not (prefix / "libexec").exists():
            prefix.mkdir(exist_ok=True)
            (prefix / "libexec").symlink_to(self.config)
        stubs = self.home / "brew-stub"
        stubs.mkdir(exist_ok=True)
        brew = stubs / "brew"
        if not brew.exists():
            brew.write_text("#!/bin/sh\n"
                            f'if [ "$1" = "--prefix" ]; then echo "{prefix}"; fi\n'
                            "exit 0\n")
            brew.chmod(0o755)
        return str(stubs)

    def _brew_prefix(self):
        """The prefix a brew the fixture planted or wrote reports — one a
        test planted in the scratch home's `~/.local/bin`, or the stub
        written after a host-wide miss — or None when no such brew answers.
        The host's brews are never asked: neither the system's PATH nor
        Homebrew's own prefixes are searched here, because that brew belongs
        to the machine, its ccmorph may never have been installed through
        it, and running it could fail a host the README serves fine. Only
        executables the fixture placed are ever executed for this check."""
        dirs = (str(self.home / ".local/bin"), str(self.home / "brew-stub"))
        lookup = self._command_on("brew", dirs)
        if lookup.returncode != 0:
            return None
        reported = subprocess.run([lookup.stdout.strip(), "--prefix", "ccmorph"],
                                  env={"HOME": str(self.home),
                                       "PATH": os.pathsep.join(dirs)},
                                  input="", text=True, capture_output=True,
                                  timeout=HANG_TIMEOUT)
        out = reported.stdout.strip()
        if reported.returncode != 0 or not out.startswith("/"):
            return None
        return Path(out)

    def _homebrew_formula_prefix(self):
        """The real prefix `brew --prefix ccmorph` prints on a host whose
        Homebrew installed ccmorph, or None. A Homebrew's prefix is the parent
        of a HOMEBREW_BIN dir holding an executable brew, and it keeps the
        formula's stable prefix at `<prefix>/opt/ccmorph` — the link
        cli/run.go's stableCore already names. The first Homebrew that holds
        the formula answers: a host with two (an Intel Homebrew left beside
        the Apple Silicon one) must not stop at one without ccmorph (#636).
        Read from the disk, so that brew is never executed; a Homebrew with no
        ccmorph formula installed has no real prefix to verify (#717)."""
        for bin_dir in HOMEBREW_BIN:
            brew = Path(bin_dir) / "brew"
            if brew.is_file() and os.access(brew, os.X_OK):
                formula = Path(bin_dir).parent / "opt" / "ccmorph"
                if formula.is_dir():
                    return formula
        return None

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
                # The tree the installer copies into the fresh install's
                # config dir is always what this check verifies; a brew the
                # fixture planted or wrote (_brew_prefix) additionally
                # reports a prefix whose libexec/ ships that same tree, so
                # when one answers the command must be executable there too
                # — the prefix adds, it never substitutes (#717).
                rest = command[len(BREW_LIBEXEC):]
                found = self.config / rest
                prefix = self._brew_prefix()
                if prefix is not None:
                    brew_found = prefix / "libexec" / rest
                    if not (brew_found.is_file() and os.access(brew_found, os.X_OK)):
                        return (f"'{command}' — the prefix brew reports "
                                f"({prefix}) has no executable at libexec/{rest}")
                # Where Homebrew installed ccmorph, the real formula prefix
                # is a third place the command must be — also additive.
                formula = self._homebrew_formula_prefix()
                if formula is not None:
                    real = formula / "libexec" / rest
                    if not (real.is_file() and os.access(real, os.X_OK)):
                        return (f"'{command}' — Homebrew's ccmorph prefix "
                                f"({formula}) has no executable at "
                                f"libexec/{rest}")
            elif command.startswith(("/", "$")):
                return f"'{command}' — a path this check cannot place"
            else:
                found = self.config / command  # a bare relative path addresses the install
            if found.is_file() and os.access(found, os.X_OK):
                return None
            return f"'{command}' — a fresh install has no executable at {found}"
        if self._resolve_tool(command) is not None:
            return None
        return (f"'{command}' — not found on a reader's PATH: neither the fresh "
                f"install nor the system provides it")

    def _resolve_tool(self, command):
        """The path a reader's `command -v COMMAND` finds right after a fresh
        install, or None: the install's ~/.local/bin, then SYSTEM_PATH, and
        for brew alone HOMEBREW_BIN too. Only existence is checked here —
        nothing found is executed."""
        dirs = (str(self.home / ".local/bin"),) + SYSTEM_PATH
        if command == "brew":
            dirs += HOMEBREW_BIN
        lookup = self._command_on(command, dirs)
        if lookup.returncode == 0:
            return lookup.stdout.strip()
        if command == "brew":
            # A host with no brew at any prefix (every Linux box without
            # Linuxbrew) reads the clone/curl route of the README; its
            # absence says nothing about whether the Homebrew lines name a
            # runnable command, so ask the check's own stub (#717).
            lookup = self._command_on(command, (self._brew_stub(),) + dirs)
            if lookup.returncode == 0:
                return lookup.stdout.strip()
        return None

    def test_every_command_the_readme_names_is_found_by_a_fresh_install(self):
        problems = self._problems(README.read_text(encoding="utf-8"))
        self.assertEqual(problems, [], "README names commands a fresh install "
                         "cannot run:\n  " + "\n  ".join(problems))

    def test_the_readme_and_help_config_name_the_profile_template_issue_392(self):
        """#392: the README called `profile.conf.example` the template that lists
        every allowed key but gave no path, and a Homebrew install keeps it in
        the package's prefix, far from the profiles. The README names the path
        for each install route and the help page that prints this install's;
        each path is a file there, and `ccmorph help config` on a fresh install
        names the template the install placed."""
        flat = re.sub(r"\s+", " ", README.read_text(encoding="utf-8"))
        for named in ("~/.config/ccmorph/profile.conf.example",
                      f"{BREW_LIBEXEC}share/profile.conf.example",
                      "ccmorph help config"):
            self.assertIn(named, flat, f"the README never names {named}")
        installed = self.config / "profile.conf.example"
        self.assertTrue(installed.is_file(), f"a fresh install has no {installed}")
        # A Homebrew package's libexec holds the repository's own tree.
        self.assertTrue((ROOT / "share/profile.conf.example").is_file())
        result = self._run_cli([self.cli, "help", "config"])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        named = [line.strip() for line in result.stdout.splitlines()
                 if line.strip().endswith("profile.conf.example")]
        self.assertEqual(len(named), 1, result.stdout)
        path = Path(re.sub(r"^~/", f"{self.home}/", named[0]))
        self.assertEqual(path.resolve(), installed.resolve(), result.stdout)

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

    def test_a_planted_brew_without_a_libexec_tree_is_still_reported(self):
        """#717 negative pin: the stub this check writes for a host without
        brew stands in for the documented prerequisite only — it must not
        make the check blind. A brew planted on the reader's PATH answering
        `--prefix ccmorph` with a prefix that lacks part of the tree (and
        holds an executable the fresh install does not) plus a README line
        naming a command no install provides are all still reported: the
        prefix adds a second place the command must be, never a
        substitute for the install's own tree."""
        prefix = self.home / "planted-prefix"
        planted_share = prefix / "libexec" / "share"
        planted_share.mkdir(parents=True)
        # executable under the planted prefix, absent from the fresh install:
        only = planted_share / "only-in-planted-prefix"
        only.write_text(STUB)
        only.chmod(0o755)
        planted = self.home / ".local/bin" / "brew"
        planted.write_text("#!/bin/sh\n"
                           f'if [ "$1" = "--prefix" ]; then echo "{prefix}"; fi\n'
                           "exit 0\n")
        planted.chmod(0o755)
        self.addCleanup(planted.unlink)
        fence = "`" * 3
        sample = "\n".join([
            "## Install", "",
            f"{fence}sh", "brew install akleizor/tap/ccmorph", "nosuchtool-717 --flag", fence,
            "The exporter runs from "
            f'`"{BREW_LIBEXEC}share/usage-export" --output x`.',
            "And what only the planted prefix carries: "
            f'`"{BREW_LIBEXEC}share/only-in-planted-prefix" --output x`.',
        ])
        problems = self._problems(sample)
        # The planted brew itself resolves, so the problems are exactly the
        # unfound command, the tree the planted prefix lacks, and the
        # executable the fresh install lacks.
        self.assertEqual(len(problems), 3, problems)
        for needle in ("nosuchtool-717", "share/usage-export", "share/only-in-planted-prefix"):
            self.assertTrue(any(needle in p for p in problems), (needle, problems))

    def test_homebrews_formula_prefix_is_verified_and_its_brew_never_run(self):
        """#717, a Mac with Homebrew: its installer puts brew at
        /opt/homebrew/bin, HOMEBREW_BIN's first entry, and `brew --prefix
        ccmorph` there is /opt/homebrew/opt/ccmorph. The check finds that
        brew — never its own stub — and never runs it; where the formula is
        installed it verifies the README's `$(brew --prefix ccmorph)/libexec/`
        commands in that real prefix as well as in the fresh install's tree.
        A scratch Homebrew stands in for /opt/homebrew, its brew recording any
        run, and the system dirs are swapped for an empty one wherever the
        lookup's answer is asserted, so this runs alike on every host."""
        self.assertEqual(HOMEBREW_BIN[0], "/opt/homebrew/bin")
        homebrew = self.home / "homebrew-717"
        homebrew_bin = homebrew / "bin"
        homebrew_bin.mkdir(parents=True, exist_ok=True)
        ran = self.home / "homebrew-brew-ran"
        unrelated = self.home / "unrelated-homebrew-prefix"
        brew = homebrew_bin / "brew"
        brew.write_text("#!/bin/sh\n"
                        f'echo "$*" >> "{ran}"\n'
                        f'echo "{unrelated}"\n'
                        "exit 0\n")
        brew.chmod(0o755)
        no_system = self.home / "no-system-dirs"
        no_system.mkdir(exist_ok=True)
        formula = homebrew / "opt" / "ccmorph"
        readme = README.read_text(encoding="utf-8")
        fence = "`" * 3
        only = "only-in-formula-717"
        sample = "\n".join([
            f"{fence}sh", "brew install akleizor/tap/ccmorph", fence,
            f'Run `"{BREW_LIBEXEC}share/usage-export" --output x`, and',
            f'`"{BREW_LIBEXEC}share/{only}" --output x`.',
        ])
        # A Mac never writes the stub; an earlier test on a brew-less host
        # may have, and _brew_stub writes it again whenever a later test
        # needs it.
        shutil.rmtree(self.home / "brew-stub", ignore_errors=True)
        consulted = AssertionError("the check consulted its stub on a host with brew")
        with mock.patch.dict(globals(), {"HOMEBREW_BIN": (str(homebrew_bin),)}), \
                mock.patch.object(self, "_brew_stub", side_effect=consulted):
            # The lookup finds Homebrew's brew, whatever the host's own
            # /usr/bin or /bin carry: those are swapped out for this answer.
            with mock.patch.dict(globals(), {"SYSTEM_PATH": (str(no_system),)}):
                self.assertEqual(self._resolve_tool("brew"), str(brew))
            # Homebrew without the ccmorph formula: no real prefix to verify,
            # so the fresh install's tree judges, as on main.
            self.assertIsNone(self._homebrew_formula_prefix())
            self.assertEqual(self._problems(readme), [])
            # The formula installed with the repository's tree: verified there.
            formula.mkdir(parents=True)
            (formula / "libexec").symlink_to(self.config)
            self.assertEqual(self._homebrew_formula_prefix(), formula)
            self.assertEqual(self._problems(readme), [])
            # The real prefix lacks the exporter: reported, naming that prefix.
            (formula / "libexec").unlink()
            (formula / "libexec" / "share").mkdir(parents=True)
            extra = formula / "libexec" / "share" / only
            extra.write_text(STUB)
            extra.chmod(0o755)
            problems = self._problems(sample)
        # The prefix adds, it never substitutes: what only the formula holds
        # is still missing from the fresh install.
        self.assertEqual(len(problems), 2, problems)
        self.assertIn("share/usage-export", problems[0])
        self.assertIn(str(formula), problems[0])
        self.assertIn(only, problems[1])
        self.assertIn("a fresh install has no executable", problems[1])
        self.assertFalse(ran.exists(), "the check ran the host's brew")
        self.assertFalse((self.home / "brew-stub").exists())

    def test_the_formula_prefix_is_found_past_a_homebrew_without_it(self):
        """#636: the lookup returned None at the first brew whose prefix lacked
        the formula, so a second Homebrew that holds ccmorph went unverified."""
        bins = []
        for name in ("first-homebrew", "second-homebrew"):
            b = self.home / name / "bin"
            b.mkdir(parents=True, exist_ok=True)
            (b / "brew").write_text("#!/bin/sh\nexit 1\n")
            (b / "brew").chmod(0o755)
            bins.append(str(b))
        formula = self.home / "second-homebrew" / "opt" / "ccmorph"
        formula.mkdir(parents=True, exist_ok=True)
        with mock.patch.dict(globals(), {"HOMEBREW_BIN": tuple(bins)}):
            self.assertEqual(self._homebrew_formula_prefix(), formula)
            shutil.rmtree(formula)
            self.assertIsNone(self._homebrew_formula_prefix())

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
