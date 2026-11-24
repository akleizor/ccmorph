"""The documented model/effort shorthand with the profile omitted (GH-788).

The usage lines mark the profile optional (`set [profile] [model|effort] [value]`),
so a leading `model` or `effort` names the setting, not a profile: the command
chooses the profile (one saved profile is the choice) and then reads or saves the
setting. A profile whose name collides with a setting word stays reachable by
naming both words: the leading word is the profile when the word after it is
also a setting (`get model model`, `set model effort high`).
"""
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

from support import HANG_TIMEOUT, path_without

ROOT = Path(__file__).resolve().parents[1]


class GetSetShorthandTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.state = self.home / 'state'
        self.state.mkdir()
        self.conf = self.state / 'work.conf'
        self.conf.write_text('type=apikey\napi_key={{SECRET_zt3k2m5u3xkq}}')
        self.conf.write_text(self.conf.read_text() + '\nmodel=work-model\n')
        self.env = {**os.environ, 'HOME': str(self.home), 'ZDOTDIR': str(self.home),
                    'CCMORPH_STATE_DIR': str(self.state), 'CCMORPH_LIB_DIR': str(ROOT / 'lib')}
        self.env.pop('CCMORPH_CONTEXT_LOCKED', None)

    def run_cli(self, *args, env=None):
        return subprocess.run(['zsh', str(ROOT / 'bin/ccmorph'), *args], input='',
                              text=True, capture_output=True, env=env or self.env,
                              timeout=HANG_TIMEOUT)

    def profile(self, name, text):
        (self.state / (name + '.conf')).write_text(text)
        return self.state / (name + '.conf')

    def fields(self, conf=None):
        return dict(line.split('=', 1) for line in
                    (conf or self.conf).read_text().splitlines() if '=' in line)

    def test_get_model_shorthand_reads_the_single_profile(self):
        # One saved profile is the choice the picker would make; the read is the
        # same value `get work model` gives.
        result = self.run_cli('get', 'model')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, 'work-model\n')

    def test_get_effort_shorthand_reads_the_single_profile(self):
        # Effort is the other setting the shorthand takes; unset is a real
        # state, so the read is empty stdout with the unsaved words on stderr.
        result = self.run_cli('get', 'effort')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, '')
        self.assertIn('work: no effort saved', result.stderr)

    def test_set_effort_shorthand_saves_the_single_profile(self):
        result = self.run_cli('set', 'effort', 'high')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, 'high\n')
        self.assertEqual(self.fields()['effort'], 'high')

    def test_set_model_shorthand_saves_the_single_profile(self):
        result = self.run_cli('set', 'model', 'chosen-model')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.fields()['model'], 'chosen-model')

    def test_two_profiles_ask_the_chooser_not_a_no_profile_error(self):
        # With more than one profile the omitted profile is the chooser's decision;
        # a word-for-a-profile refusal is the bug this issue names.
        self.profile('backup', 'type=apikey\napi_key=backup-key\n')
        env = {**self.env, 'PATH': path_without('fzf', path=self.env['PATH'])}
        for command in (('get', 'model'), ('set', 'effort', 'high'), ('set', 'model', '-i')):
            with self.subTest(command=command):
                result = self.run_cli(*command, env=env)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn('pass the value directly', result.stderr)
                self.assertIn('work', result.stderr)
                self.assertNotIn("no profile 'model'", result.stderr)
                self.assertNotIn("no profile 'effort'", result.stderr)

    def test_set_shorthand_with_interactive_opens_the_single_profiles_picker(self):
        # ux-walk 2026-11-23: `set effort -i` is the shorthand too; with one
        # profile the value picker opens for it, never a no-profile refusal.
        env = {**self.env, 'PATH': path_without('fzf', path=self.env['PATH'])}
        for flag in ('-i', '--interactive'):
            with self.subTest(flag=flag):
                result = self.run_cli('set', 'effort', flag, env=env)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn('pass the value directly', result.stderr)
                self.assertIn('Use: ccmorph set work effort', result.stderr)
                self.assertNotIn("no profile 'effort'", result.stderr)
                self.assertNotIn('effort=', self.conf.read_text())

    def test_a_profile_named_after_a_setting_stays_reachable(self):
        # Naming both words is the unambiguous form: with a setting word in the
        # second slot, the first is the profile.
        named = self.profile('model', 'type=apikey\napi_key=model-key\nmodel=served-model\n')
        self.profile('work2', 'type=apikey\napi_key=other-key\n')
        result = self.run_cli('get', 'model', 'model')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, 'served-model\n')
        result = self.run_cli('set', 'model', 'effort', 'low')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.fields(named)['effort'], 'low')

    def test_an_explicit_profile_named_model_reads_its_other_fields(self):
        # A key in the second slot keeps the explicit form: `get model base_url`
        # reads that profile's endpoint, not the shorthand on the profile's
        # model (gate finding on the first revision of #788).
        named = self.profile('model', 'type=apikey\napi_key={{SECRET_k2m4v7x9q1wz}}')
        named.write_text(named.read_text() + '\nbase_url=https://example.test\n')
        result = self.run_cli('get', 'model', 'base_url')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, 'https://example.test\n')
        result = self.run_cli('get', 'model', '--json')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        fields = json.loads(result.stdout)
        self.assertEqual(fields['profile'], 'model')
        self.assertEqual(fields['base_url'], 'https://example.test')
        # api_key keeps cred's pipe rule: the key alone, no trailing newline.
        result = self.run_cli('get', 'model', 'api_key')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, self.fields(named)['api_key'])

    def test_gate_repro_a_lone_profile_named_model_reads_base_url(self):
        # The merge gate's repro, verbatim in shape (finding on #788): a state
        # dir holding only model.conf, `get model base_url` printing its
        # endpoint. It must exit 0 and print exactly the saved URL.
        for conf in self.state.glob('*.conf'):
            conf.unlink()
        self.profile('model', 'type=apikey\napi_key=key\n'
                              'base_url=https://example.test\nmodel=served-model\n')
        result = self.run_cli('get', 'model', 'base_url')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, 'https://example.test\n')

    def test_explicit_forms_keep_working(self):
        result = self.run_cli('get', 'work', 'model')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, 'work-model\n')
        result = self.run_cli('set', 'work', 'effort', 'medium')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.fields()['effort'], 'medium')

    def test_shorthand_with_no_profiles_still_says_how_to_add_one(self):
        for conf in self.state.glob('*.conf'):
            conf.unlink()
        result = self.run_cli('get', 'model')
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertNotIn("no profile 'model'", result.stderr)
        self.assertIn('ccmorph add', result.stderr)

    def test_get_json_with_the_profile_omitted_reads_the_single_profile(self):
        # --json is a flag wherever it stands (#788 follow-up): with the profile
        # left out the single saved profile is the choice, as for the shorthand.
        result = self.run_cli('get', '--json')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout)['profile'], 'work')
        self.assertEqual(json.loads(result.stdout)['model'], 'work-model')

    def test_get_json_before_the_profile_reads_that_profile(self):
        # The flag changes position, not meaning: `get --json work` is the same
        # object `get work --json` prints.
        self.profile('backup', 'type=apikey\napi_key=k2\n')
        result = self.run_cli('get', '--json', 'work')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout, self.run_cli('get', 'work', '--json').stdout)
        self.assertEqual(json.loads(result.stdout)['profile'], 'work')

    def test_get_json_with_two_profiles_asks_the_chooser(self):
        # An omitted profile with more than one saved follows the picker rule:
        # --json prints a key, so it never guesses when the choice is real.
        self.profile('backup', 'type=apikey\napi_key=k2\n')
        env = {**self.env, 'PATH': path_without('fzf', path=self.env['PATH'])}
        result = self.run_cli('get', '--json', env=env)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn('pass the value directly', result.stderr)
        self.assertNotIn("invalid profile name '--json'", result.stderr)

    def test_get_json_with_no_profiles_says_how_to_add_one(self):
        for conf in self.state.glob('*.conf'):
            conf.unlink()
        result = self.run_cli('get', '--json')
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn('ccmorph add', result.stderr)
        self.assertNotIn("'--json'", result.stderr)

    def test_get_json_first_names_a_profile_called_model(self):
        # --json first never turns the next word into the setting shorthand:
        # the profile named model is read, exactly as `get model --json` does.
        named = self.profile('model', 'type=apikey\napi_key=k3\n')
        named.write_text(named.read_text() + '\nbase_url=https://example.test\n')
        self.profile('work2', 'type=apikey\napi_key=k4\n')
        result = self.run_cli('get', '--json', 'model')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        fields = json.loads(result.stdout)
        self.assertEqual(fields['profile'], 'model')
        self.assertEqual(fields['base_url'], 'https://example.test')
        self.assertEqual(self.run_cli('get', 'model', '--json').stdout, result.stdout)

    def test_get_json_then_two_words_is_an_extra_argument(self):
        result = self.run_cli('get', '--json', 'work', 'extra')
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("unexpected argument 'extra'", result.stderr)

    def test_set_takes_no_json(self):
        result = self.run_cli('set', 'work', '--json')
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn('usage: ccmorph set', result.stderr)

    def test_no_words_still_opens_the_profile_picker_with_one_profile(self):
        # The single-profile selection belongs to the omitted-profile forms, not
        # to a bare verb: with no words at all the picker is still the answer.
        env = {**self.env, 'PATH': path_without('fzf', path=self.env['PATH'])}
        for command in (('get',), ('set',)):
            with self.subTest(command=command):
                result = self.run_cli(*command, env=env)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn('pass the value directly', result.stderr)

    def test_help_and_guide_document_the_collision_forms(self):
        # The unambiguous syntax is only reachable if it is written down.
        result = self.run_cli('help', 'get')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('get model model', result.stdout)
        self.assertIn('get model base_url', result.stdout)
        self.assertIn('before or after the profile', result.stdout)
        guide = (ROOT / 'docs/guide.md').read_text()
        self.assertIn('ccmorph get model effort', guide)
        self.assertIn('ccmorph get model base_url', guide)
        self.assertIn('ccmorph get --json', guide)


if __name__ == '__main__':
    unittest.main()
