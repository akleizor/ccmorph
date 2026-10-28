"""Model selection through the shipped `ccmorph` command, end to end.

One test per path that decides which model (and which window) Claude Code gets:
`--model <id>`, `--model=<id>`, `--model -i`, `set <profile> model`, the profile
default, flag-beats-default, the `[1m]` send id, and the window each id resolves
to. A stub `claude` records argv and the environment; a stub `curl` and adapter
keep it offline. Each child gets its own HOME and state dir.

Each test id also holds at most one launch, however cheap an extra one looks: a
launch is a stack of interpreter startups, so stacking several in one id puts it
over pytest.ini's 60 s per-test cap on a loaded host (#429) — the same
one-expensive-launch floor the installed-CLI suite was split to in #424.

Mid-session `/model` is Claude Code's own; ccmorph controls only what it writes
before launch (`ANTHROPIC_MODEL`, the window variables, settings.json), and that
is what the last test pins.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import fake_adapter
from support import HANG_TIMEOUT, HELPERS_IMPL, helper_cmd

ROOT = Path(__file__).resolve().parents[1]
LIB_FILES = ('libexec/model-window', 'libexec/settings-compose', 'lib/ccmorph.sh',
             *sorted(f'lib/{p.name}' for p in (ROOT / 'lib').glob('ccmorph-*.zsh')),
             'share/window-sources.json', 'share/model-catalogue.json')

CLAUDE = '''#!/bin/sh
case "$1" in --help) exit 0 ;; esac
python3 - "$@" <<'PY' >> "$HOME/launches.jsonl"
import json, os, sys
keys = ('ANTHROPIC_MODEL', 'CLAUDE_CODE_MAX_CONTEXT_TOKENS', 'CLAUDE_CODE_AUTO_COMPACT_WINDOW',
        'ANTHROPIC_CUSTOM_MODEL_OPTION')
doc = {}
path = os.path.join(os.environ.get('CLAUDE_CONFIG_DIR', ''), 'settings.json')
if os.path.exists(path):
    doc = json.load(open(path))
print(json.dumps({'argv': sys.argv[1:], 'env': {k: os.environ.get(k) for k in keys}, 'settings': doc}))
PY
'''
CURL = '''#!/bin/sh
for a do case "$a" in *http_code*) echo 200; exit 0 ;; esac; done
exit 1
'''
# Honours the launcher's --parent-pid like the real adapter (issue #386): a
# stub that spins forever outlives a hard-killed launch.
ADAPTER = fake_adapter.script('echo NATIVE=1', core=True)


class ModelSelection(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name)
        self.state = self.home / '.config/ccmorph'
        self.state.mkdir(parents=True)
        (self.home / 'bin').mkdir()
        for name, body in (('claude', CLAUDE), ('curl', CURL)):
            (self.home / 'bin' / name).write_text(body)
            (self.home / 'bin' / name).chmod(0o755)
        for name in LIB_FILES:
            (self.state / name).parent.mkdir(exist_ok=True)
            shutil.copy(ROOT / name, self.state / name)
        (self.state / 'libexec/ccmorph-core').write_text(ADAPTER)
        (self.state / 'libexec/ccmorph-core').chmod(0o755)
        # A launch leaves the stub detached, and `--parent-pid` only reaches it
        # while the launcher's number stays dead (#347). Registered after
        # tmp.cleanup, so this LIFO cleanup kills the stub's recorded pid first.
        self.addCleanup(fake_adapter.reap, self.state / 'libexec/ccmorph-core')
        path = f'{self.home}/bin:/usr/bin:/bin:{Path(shutil.which("zsh")).parent}'
        self.env = {'HOME': str(self.home), 'PATH': path, 'LANG': 'C.UTF-8',
                    'XDG_CONFIG_HOME': str(self.home / '.config'),
                    'CCMORPH_STATE_DIR': str(self.state), 'CCMORPH_LIB_DIR': str(self.state / 'lib')}
        # Native token profile (no routing) and a routed gateway with pinned windows.
        (self.state / 'tp.conf').write_text('type=token\ntoken=t\nmodel=sonnet\n')
        (self.state / 'gw.conf').write_text(
            'type=proxy\nbase_url=http://gw.invalid\napi_key=k\nmodel=claude-sonnet-4-5\n'
            'context_window.claude-opus-5=1000000\ncontext_window.claude-haiku-4-5=200000\n'
            'context_window.pinned-x=424242\n')

    def cli(self, *args, tool='bin/ccmorph', stdin=subprocess.DEVNULL):
        cmd = helper_cmd('model-window') if tool == 'libexec/model-window' else [str(ROOT / tool)]
        return subprocess.run([*cmd, *args], env=self.env, stdin=stdin,
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)

    def launch(self, *args):
        log = self.home / 'launches.jsonl'
        log.unlink(missing_ok=True)
        result = self.cli('run', *args)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        rows = [json.loads(line) for line in log.read_text().splitlines()]
        self.assertEqual(len(rows), 1, rows)
        rows[0]['stderr'] = result.stderr
        return rows[0]

    def window(self, profile, model):
        result = self.cli(profile, model, tool='libexec/model-window')
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_model_flag_space_form_reaches_claude_verbatim(self):
        row = self.launch('tp', '--model', 'opus')
        self.assertEqual(row['argv'], ['--model', 'opus'])
        # The flag is never rewritten and no competing default is injected.
        self.assertIsNone(row['env']['ANTHROPIC_MODEL'])

    def test_model_flag_equals_form_reaches_claude_verbatim(self):
        row = self.launch('tp', '--model=opus')
        self.assertEqual(row['argv'], ['--model=opus'])
        self.assertIsNone(row['env']['ANTHROPIC_MODEL'])

    def test_model_flag_needs_a_value(self):
        for argv in (['--model'], ['--model='], ['--model', '--']):
            result = self.cli('run', 'tp', *argv)
            self.assertNotEqual(result.returncode, 0, argv)
            self.assertIn('--model requires a value', result.stderr)

    def test_model_dash_i_without_a_terminal_falls_back_to_the_default(self):
        row = self.launch('tp', '--model', '-i')
        self.assertNotIn('--model', row['argv'])
        self.assertEqual(row['env']['ANTHROPIC_MODEL'], 'sonnet')

    def test_profile_default_reaches_claude_without_a_flag(self):
        self.assertEqual(self.launch('tp')['env']['ANTHROPIC_MODEL'], 'sonnet')

    def test_set_model_persists_and_run_uses_it_without_a_flag(self):
        self.assertEqual(self.cli('set', 'tp', 'model', 'opus').returncode, 0)
        self.assertEqual(self.cli('get', 'tp', 'model').stdout.strip(), 'opus')
        row = self.launch('tp')
        self.assertEqual(row['argv'], [])
        self.assertEqual(row['env']['ANTHROPIC_MODEL'], 'opus')
        self.assertNotEqual(self.cli('set', 'tp', 'model').returncode, 0)  # no tty: no picker answer

    def test_explicit_flag_beats_the_profile_default(self):
        self.assertEqual(self.cli('set', 'tp', 'model', 'opus').returncode, 0)
        row = self.launch('tp', '--model', 'haiku')
        self.assertEqual(row['argv'], ['--model', 'haiku'])
        self.assertIsNone(row['env']['ANTHROPIC_MODEL'])

    def test_one_m_suffix_goes_on_the_default(self):
        self.assertEqual(self.cli('set', 'gw', 'model', 'claude-opus-5').returncode, 0)
        self.assertEqual(self.launch('gw')['env']['ANTHROPIC_MODEL'], 'claude-opus-5[1m]')

    def test_one_m_suffix_never_on_an_explicit_flag(self):
        self.assertEqual(self.cli('set', 'gw', 'model', 'claude-opus-5').returncode, 0)
        row = self.launch('gw', '--model', 'claude-opus-5')
        self.assertEqual(row['argv'], ['--model', 'claude-opus-5'])
        self.assertIsNone(row['env']['ANTHROPIC_MODEL'])
        self.assertIn('claude-opus-5[1m]', row['stderr'])  # told, not rewritten

    def test_a_200k_id_is_never_suffixed(self):
        self.assertEqual(self.cli('set', 'gw', 'model', 'claude-haiku-4-5').returncode, 0)
        self.assertEqual(self.launch('gw')['env']['ANTHROPIC_MODEL'], 'claude-haiku-4-5')

    def test_a_non_claude_id_passes_through(self):
        self.assertEqual(self.cli('set', 'gw', 'model', 'pinned-x').returncode, 0)
        self.assertEqual(self.launch('gw')['env']['ANTHROPIC_MODEL'], 'pinned-x')

    def test_resolved_window_for_each_source(self):
        cache = self.state / 'profiles/gw/cache/context-window'
        cache.parent.mkdir(parents=True)
        cache.write_text('disc-model\t333000\n')
        # Migration turns the legacy cache into discovered windows stored in the conf.
        self.assertEqual(self.cli('--migrate', tool='libexec/model-window').returncode, 0)
        got = self.window('gw', 'claude-opus-5')
        self.assertEqual((got['window'], got['send_id']), (1000000, 'claude-opus-5[1m]'))
        got = self.window('gw', 'claude-opus-5[1m]')
        self.assertEqual(got['window'], 1000000)
        self.assertTrue(got['send_id'].endswith('[1m]'))
        got = self.window('gw', 'claude-haiku-4-5')
        self.assertEqual((got['window'], got['send_id']), (200000, 'claude-haiku-4-5'))
        got = self.window('gw', 'pinned-x')
        self.assertEqual((got['window'], got['source']), (424242, 'conf-manual'))
        got = self.window('gw', 'disc-model')
        self.assertEqual((got['window'], got['source']), (333000, 'conf-discovered'))
        # A manual pin outranks a discovered value for the same id.
        self.assertEqual(self.cli('--context', 'gw', 'disc-model', '111111', tool='libexec/model-window').returncode, 0)
        self.assertEqual(self.window('gw', 'disc-model')['window'], 111111)

    def test_launch_writes_the_window_and_default_for_the_session_to_switch_within(self):
        # Claude Code owns /model; ccmorph fixes the default and the window it starts with.
        self.assertEqual(self.cli('set', 'gw', 'model', 'pinned-x').returncode, 0)
        row = self.launch('gw')
        self.assertEqual(row['env']['ANTHROPIC_MODEL'], 'pinned-x')
        self.assertEqual(row['settings'].get('model'), 'pinned-x')
        self.assertEqual(row['settings'].get('autoCompactWindow'), 424242)


if __name__ == '__main__':
    unittest.main()
