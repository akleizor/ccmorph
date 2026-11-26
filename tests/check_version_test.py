"""check-version's version probes: the newest CHANGELOG entry (GH-610) and the
newest shipped tag (GH-928).

The live `## Unreleased` block sits above every release heading and grows with
every landing, so the gate must decide "newest" by content (the first heading
that is not exactly `## Unreleased`), never by a fixed line window. A release
can also ship without main recording it: the newest v* tag then names a tree
main never carried, so check-version must resolve that tag and refuse a skipped
sync. The seam is the real Makefile's check-version target run against scratch
git repos.
"""
import os
import shutil
import subprocess
import tempfile
import unittest

from support import HANG_TIMEOUT

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GIT_ENV = {
    "GIT_CONFIG_GLOBAL": "/dev/null",
    "GIT_CONFIG_SYSTEM": "/dev/null",
}


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update(GIT_ENV)
    return env


def _git(cwd, *args):
    subprocess.run(
        ["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example",
         "-c", "commit.gpgsign=false", *args],
        cwd=cwd, env=_env(), check=True,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        timeout=HANG_TIMEOUT)


def _run(changelog, version, main_tag=None, side_tag=None):
    """Run check-version in a scratch repo; return (rc, output).

    `main_tag` names the tag on the committed line (default `v<version>`).
    `side_tag`, when given, is tagged on a release branch that is never merged
    into it: the shipped-tag-off-main shape (#928).
    """
    d = tempfile.mkdtemp(prefix="ccmorph-check-version.")
    try:
        os.mkdir(os.path.join(d, "lib"))
        with open(os.path.join(d, "CHANGELOG.md"), "w", encoding="utf-8") as f:
            f.write(changelog)
        with open(os.path.join(d, "lib", "ccmorph.sh"), "w") as f:
            f.write('_CCMORPH_VERSION="%s"\n' % version)
        _git(d, "init", "-q")
        _git(d, "add", "-A")
        _git(d, "commit", "-qm", "fixture")
        _git(d, "tag", main_tag or ("v" + version))
        if side_tag:
            branch = subprocess.run(
                ["git", "symbolic-ref", "--short", "HEAD"], cwd=d, env=_env(),
                stdout=subprocess.PIPE, universal_newlines=True,
                timeout=HANG_TIMEOUT).stdout.strip()
            _git(d, "checkout", "-qb", "release")
            _git(d, "commit", "-qm", "release", "--allow-empty")
            _git(d, "tag", side_tag)
            _git(d, "checkout", "-q", branch)
        p = subprocess.run(
            ["make", "--no-print-directory", "-f",
             os.path.join(ROOT, "Makefile"), "-C", d, "check-version"],
            env=_env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            universal_newlines=True, timeout=HANG_TIMEOUT)
        return p.returncode, p.stdout
    finally:
        shutil.rmtree(d, ignore_errors=True)


def _log(unreleased_heading="## Unreleased", bullets=15, version="0.9.0"):
    lines = ["# Changelog", ""]
    if unreleased_heading:
        lines += [unreleased_heading, "", "### Fixed", ""]
        lines += ["- bullet %d" % i for i in range(bullets)] + [""]
    lines += ["## v%s — 2026-11-18" % version, "", "Current.", "",
              "## v0.8.0 — 2026-11-10", "", "Earlier.", ""]
    return "\n".join(lines)


class CheckVersionTest(unittest.TestCase):
    def test_newest_heading_below_line_20_passes_issue_610(self):
        text = _log()
        self.assertGreater(text.split("\n").index(
            "## v0.9.0 — 2026-11-18") + 1, 20)
        rc, out = _run(text, "0.9.0")
        self.assertEqual(rc, 0, out)
        self.assertIn("source, changelog and tag agree", out)

    def test_stale_tag_inside_old_window_fails_issue_610(self):
        text = _log(bullets=1)
        self.assertLessEqual(text.split("\n").index(
            "## v0.8.0 — 2026-11-10") + 1, 20)
        rc, out = _run(text, "0.8.0")
        self.assertNotEqual(rc, 0, out)
        self.assertIn("is not the newest CHANGELOG entry", out)

    def test_only_the_exact_live_unreleased_heading_is_skipped_issue_610(self):
        # "## Unreleased notes" is a real heading, not the live block: it is
        # the newest entry, so v0.9.0 below it must be refused.
        rc, out = _run(_log("## Unreleased notes", bullets=1), "0.9.0")
        self.assertNotEqual(rc, 0, out)
        self.assertIn("is not the newest CHANGELOG entry", out)

    def test_a_changelog_without_unreleased_passes_issue_610(self):
        rc, out = _run(_log(None), "0.9.0")
        self.assertEqual(rc, 0, out)

    def test_the_real_changelog_names_the_current_version_issue_610(self):
        with open(os.path.join(ROOT, "CHANGELOG.md"), encoding="utf-8") as f:
            text = f.read()
        with open(os.path.join(ROOT, "lib", "ccmorph.sh"),
                  encoding="utf-8") as f:
            version = next(l for l in f if l.startswith("_CCMORPH_VERSION="))
        version = version.split('"')[1]
        rc, out = _run(text, version)
        self.assertEqual(rc, 0, out)

    def test_a_shipped_tag_off_main_fails_issue_928(self):
        # v0.10.0 shipped from a release branch while the scratch main still
        # records 0.9.0: the guard must read origin's newest tag, not main's
        # own version, and name the record it needs.
        rc, out = _run(_log(), "0.9.0", side_tag="v0.10.0")
        self.assertNotEqual(rc, 0, out)
        self.assertIn("tag v0.10.0 is not on main — record the release", out)

    def test_a_release_recorded_on_main_passes_issue_928(self):
        # The sync record carries the tag's version and section; the release
        # commit itself stays on the release line (a squashed landing).
        rc, out = _run(_log(version="0.10.0"), "0.10.0",
                       main_tag="v0.9.0", side_tag="v0.10.0")
        self.assertEqual(rc, 0, out)

    def test_the_newest_tag_is_read_by_version_not_creation_issue_928(self):
        # A backport tag created after the release must not mask it: v0.9.5
        # is newer by creation date, v0.10.0 is the newest release.
        rc, out = _run(_log(version="0.10.0"), "0.10.0", side_tag="v0.9.5")
        self.assertEqual(rc, 0, out)


if __name__ == "__main__":
    unittest.main()
