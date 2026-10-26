"""Exercise installed commands without real credentials or billable requests.

The workflow is split across cases that share one installed home per worker
(setUpClass), because stacking install, configure and every launch in a single
case put that case against pytest.ini's per-test cap under load (GH-424).  The
design rule that keeps every case inside the cap is one expensive launch per
case plus a shared setup whose cost is far below the cap, so the first case a
worker pulls still fits; the per-call subprocess bound is the suite's shared
hang detector (support.HANG_TIMEOUT), never tuned to how long a healthy call
takes.  Each case owns a claim that must hold
whatever order xdist hands it to a worker: nothing here depends on another
case having run first, and the uninstall case builds its own home because
removing the install would take the shared one away from cases that have not
run yet.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def _fake_native(home):
    """Build the stand-in for Claude Code: it records its config dir and args."""
    native = home / 'native'
    native.mkdir()
    target = native / 'claude-native'
    target.write_text('#!/bin/sh\nif [ "$1" = --help ]; then printf "Commands:\\n  mcp  Manage servers\\n"; exit; fi\nprintf "%s\\n" "$CLAUDE_CONFIG_DIR" > "$HOME/launched-profile"\nprintf "%s\\n" "$@" > "$HOME/launched-args"\n')
    target.chmod(0o755)
    (native / 'claude').symlink_to(target)
    return native / 'claude'


def _isolated_env(home):
    native = home / 'native'
    env = {k: v for k, v in os.environ.items() if not k.startswith(('CCPROTEUS_', '_CCPROTEUS_', 'CLAUDE_', 'ANTHROPIC_'))}
    env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / '.config'),
               PATH=f'{home}/.local/bin:{native}:/opt/homebrew/bin:/usr/bin:/bin')
    return env


def _run(argv, env, input=''):
    return subprocess.run(argv, input=input, env=env, text=True,
                          capture_output=True, timeout=HANG_TIMEOUT)


class InstalledCliTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        td = tempfile.TemporaryDirectory()
        cls.addClassCleanup(td.cleanup)
        cls.home = home = Path(td.name)
        cls.native = _fake_native(home)
        cls.native_before = cls.native.readlink()
        cls.native_bytes = cls.native_before.read_bytes()
        cls.env = _isolated_env(home)
        cls.cli = str(home / '.local/bin/ccproteus')

        installed = cls._run_cli(['zsh', str(ROOT / 'install.sh')])
        assert installed.returncode == 0, installed.stdout + installed.stderr
        # --help is answered by the fresh install, before any profile exists.
        help_result = cls._run_cli([cls.cli, '--help'])
        assert help_result.returncode == 0, help_result.stderr
        assert help_result.stderr == ''
        assert 'ccproteus help <command>' in help_result.stdout
        cls.help_stdout = help_result.stdout
        added = cls._run_cli([cls.cli, 'add', 'demo'], 'token\nexample-token\nn\n')
        assert added.returncode == 0, added.stdout + added.stderr
        assert cls._run_cli([cls.cli, 'use', 'demo']).returncode == 0
        configured = cls._run_cli([cls.cli, 'config', 'demo'], 'n\nsonnet\nhigh\ny\n')
        assert configured.returncode == 0, configured.stdout + configured.stderr
        # A second profile the launch cases can name; its existence is what
        # makes the refused two-profile launch a refusal about the rule, not
        # about a missing profile. Copy only synthetic fixture state, never
        # an operator's credentials.
        (home / '.config/ccproteus/other.conf').write_text('type=token\ntoken=other-example\n')

    @classmethod
    def _run_cli(cls, argv, input=''):
        return _run(argv, cls.env, input)

    def _markers(self):
        return (self.home / 'launched-profile', self.home / 'launched-args')

    def _clear_markers(self):
        for marker in self._markers():
            marker.unlink(missing_ok=True)

    def _launch(self, argv):
        # A marker left by an earlier launch would satisfy the assertions
        # below even when this command never reached native Claude.
        self._clear_markers()
        result = self._run_cli(argv)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        markers = self._markers()
        self.assertTrue(all(marker.exists() for marker in markers),
                        f'{argv[1:]} exited 0 without launching native Claude')
        # The workflow must leave the operator's own Claude Code alone; every
        # launch re-checks what the original asserted once at the end: the
        # symlink it resolves and the file it points to, unchanged.
        self.assertEqual(self.native.readlink(), self.native_before)
        self.assertEqual(self.native_before.read_bytes(), self.native_bytes)
        return markers[0].read_text().strip(), markers[1].read_text().splitlines()

    def test_install_and_help_preserve_native_claude(self):
        self.assertTrue(self.native.exists())
        self.assertEqual(self.native.readlink(), self.native_before)
        self.assertFalse((self.home / '.local/bin/claude').exists())
        self.assertIn('ccproteus config [profile]', self.help_stdout)

    def test_configured_profile_reports_model_without_token(self):
        self.assertEqual(self._run_cli([self.cli, 'get', 'demo', 'model']).stdout.strip(), 'sonnet')
        inspected = self._run_cli([self.cli, 'inspect', 'demo'])
        self.assertEqual(inspected.returncode, 0, inspected.stderr)
        self.assertIn('sonnet', inspected.stdout)
        self.assertNotIn('example-token', inspected.stdout)
        self.assertEqual(self._run_cli([self.cli, 'cred', 'demo']).stdout, 'example-token')

    def test_run_launches_named_profile(self):
        profile, _ = self._launch([self.cli, 'run', 'demo', '--version'])
        self.assertEqual(profile, str(self.home / '.config/ccproteus/profiles/demo'))

    def test_run_launches_default_profile(self):
        profile, _ = self._launch([self.cli, 'run', '--version'])
        self.assertEqual(profile, str(self.home / '.config/ccproteus/profiles/demo'))

    def test_run_launches_other_profile(self):
        profile, _ = self._launch([self.cli, 'run', 'other', '--version'])
        self.assertEqual(profile, str(self.home / '.config/ccproteus/profiles/other'))

    def test_run_forwards_arguments(self):
        self.assertEqual(self._launch([self.cli, 'run', 'demo', 'mcp', 'list'])[1], ['mcp', 'list'])

    def test_run_refuses_two_profiles_and_passes_flag_arguments(self):
        self._clear_markers()
        self.assertNotEqual(self._run_cli([self.cli, 'run', '-a', 'demo', '-a', 'other']).returncode, 0)
        self.assertFalse(any(marker.exists() for marker in self._markers()),
                         'a refused command launched native Claude')
        self.assertEqual(self._launch([self.cli, 'run', 'demo', '--system-prompt', '-a'])[1][:2],
                         ['--system-prompt', '-a'])

    def test_uninstall_preserves_native_claude(self):
        with tempfile.TemporaryDirectory() as td:
            home = Path(td)
            native = _fake_native(home)
            native_before = native.readlink()
            native_bytes = native_before.read_bytes()
            env = _isolated_env(home)
            installed = _run(['zsh', str(ROOT / 'install.sh')], env)
            self.assertEqual(installed.returncode, 0, installed.stdout + installed.stderr)
            self.assertEqual(native.readlink(), native_before)
            removed = _run(['zsh', str(ROOT / 'install.sh'), '--uninstall'], env)
            self.assertEqual(removed.returncode, 0, removed.stderr)
            self.assertEqual(native.readlink(), native_before)
            self.assertEqual(native_before.read_bytes(), native_bytes)
            self.assertFalse((home / '.local/bin/claude').exists())


if __name__ == '__main__':
    unittest.main()
