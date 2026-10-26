"""ccmorph owns model/effort defaults in the conf; native settings are read-only to it.

The contract under test:

  * defaults live in the conf as `model=`, `effort=`, `effort.<model>=` and the
    mapping `env.ANTHROPIC_*` keys, with `model_defaults=1` marking the profile
    as seeded;
  * a setter NEVER writes settings.json -- Claude Code owns that file, and an
    inline /model or /effort during a session must not silently become the
    profile's default;
  * the first seed imports whatever native settings currently resolve to, so an
    existing install keeps its model, but an explicit conf value always wins;
  * after the marker is present, native is no longer consulted at all -- an
    absent default stays absent rather than reviving a native value.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def conf_fields(path):
    """The conf's parsed key=value pairs. A save may regroup lines canonically;
    what it must never do is change, add or drop a field."""
    fields = {}
    for line in Path(path).read_text().split("\n"):
        key, sep, value = line.strip(" \t\r").partition("=")
        if sep and not key.startswith("#"):
            fields[key] = value
    return fields


def tree(root):
    """Every file under a home, as {relative path: bytes}.

    A settings backup is named for the instant it was taken, so its name is folded
    away; the number of files is compared separately, which is what stops the fold
    from hiding a backup."""
    files = {}
    for path in sorted(Path(root).rglob('*')):
        if not path.is_file():
            continue
        rel = path.relative_to(root)
        if rel.parent.name == 'backups':
            rel = rel.parent / 'settings.<stamp>.json'
        files[str(rel)] = path.read_bytes()
    return files


def file_count(root):
    return sum(1 for path in Path(root).rglob('*') if path.is_file())


# Nested past what the decoder's recursion limit can finish. Written as text:
# building it as a dict would recurse in the test itself (#420).
TOO_DEEP_TO_DECODE = '{"sandbox":' + "[" * 2_000_000 + "0" + "]" * 2_000_000 + "}"


class ConfigurationSaveTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.state = self.home / 'state'
        self.state.mkdir()
        self.conf = self.state / 'demo.conf'
        self.conf.write_text('type=apikey\napi_key=test-key\nbase_url=https://old.invalid\ncontext_window.m=10000\n')
        self.settings = self.state / 'profiles/demo/settings.json'
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text(json.dumps({'env': {'ANTHROPIC_MODEL': 'old'}, 'other': True}))
        self.env = {**os.environ, 'HOME': str(self.home), 'ZDOTDIR': str(self.home),
                    'CCMORPH_STATE_DIR': str(self.state), 'CCMORPH_LIB_DIR': str(ROOT / "lib")}
        self.env.pop('CCMORPH_CONTEXT_LOCKED', None)

    def run_cli(self, *args, input='', env=None):
        return subprocess.run(['zsh', str(ROOT / "bin/ccmorph"), *args], input=input,
                              text=True, capture_output=True, env=env or self.env,
                              timeout=HANG_TIMEOUT)

    def store(self, *args, env=None):
        """Call _ccm_config_store directly, for the operations no verb exposes."""
        script = 'source "$1"; shift; _ccm_config_store "$@"'
        return subprocess.run(['zsh', '-fc', script, '_', str(ROOT / 'lib/ccmorph.sh'), *args],
                              env=env or self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)

    def conf_fields(self):
        return dict(line.split('=', 1) for line in
                    self.conf.read_text().splitlines() if '=' in line)

    def test_model_map_is_profile_scoped_and_survives_a_config_save(self):
        mapping = '{"claude-sonnet-5-5":"gpt-6.1-sol"}'
        with self.conf.open('a') as f:
            f.write('model_map=' + mapping + '\n')
        result = self.run_cli('_env', 'demo')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('CCMORPH_MODEL_MAP=' + mapping, result.stdout)
        self.assertEqual(self.run_cli('set', 'demo', 'effort', 'high').returncode, 0)
        self.assertEqual(self.conf_fields()['model_map'], mapping)
        self.conf.write_text('type=apikey\napi_key=test-key\nbase_url=https://old.invalid\n')
        self.assertNotIn('CCMORPH_MODEL_MAP=', self.run_cli('_env', 'demo').stdout)

    def test_model_map_rejects_malformed_values_duplicate_ids_and_wrong_profile_types(self):
        # The last two are not malformed but unreachable: the [1m] window marker is
        # stripped before a map is looked up, in either spelling ccmorph reads it,
        # so such a key could never name the model the gateway sees.
        for value in ('[]', 'null', '{"m":1}', '{"m":""}', '{"m":"a b"}',
                      '{"m":"one","m":"two"}', '{"m":{"nested":"id"}}', 'not-json',
                      '{"m[1m]":"target"}', '{"m[1M]":"target"}'):
            with self.subTest(value=value):
                self.conf.write_text('type=apikey\napi_key=test-key\nmodel_map=' + value + '\n')
                result = self.run_cli('_env', 'demo')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('model_map', result.stderr)
        self.conf.write_text('type=sub\nemail=test@example.invalid\nmodel_map={"m":"target"}\n')
        self.assertNotEqual(self.run_cli('_env', 'demo').returncode, 0)

    # ---- the setter never touches native settings -------------------------

    def test_set_model_writes_conf_field_and_leaves_settings_untouched(self):
        native = self.settings.read_bytes()
        result = self.run_cli('set', 'demo', 'model', 'new')
        self.assertEqual(result.returncode, 0, result.stderr)
        fields = self.conf_fields()
        self.assertEqual(fields['model'], 'new')
        self.assertEqual(fields['model_defaults'], '1')
        self.assertEqual(self.settings.read_bytes(), native)
        self.assertEqual(self.run_cli('get', 'demo', 'model').stdout.strip(), 'new')

    def test_set_effort_writes_conf_field_and_leaves_settings_untouched(self):
        native = self.settings.read_bytes()
        self.assertEqual(self.run_cli('set', 'demo', 'effort', 'high').returncode, 0)
        self.assertEqual(self.conf_fields()['effort'], 'high')
        self.assertEqual(self.settings.read_bytes(), native)
        self.assertEqual(self.run_cli('get', 'demo', 'effort').stdout.strip(), 'high')

    def test_configure_saves_defaults_without_writing_settings(self):
        native = self.settings.read_bytes()
        result = self.run_cli('config', 'demo',
                              input='https://new.invalid\nn\nchosen\nmedium\ny\n')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        fields = self.conf_fields()
        self.assertEqual(fields['model'], 'chosen')
        self.assertEqual(fields['effort'], 'medium')
        self.assertEqual(fields['base_url'], 'https://new.invalid')
        self.assertEqual(fields['model_defaults'], '1')
        self.assertEqual(self.settings.read_bytes(), native)

    # ---- reads do not mutate ---------------------------------------------

    def test_first_get_imports_native_value_without_writing_anything(self):
        before = (conf_fields(self.conf), self.settings.read_bytes())
        self.assertEqual(self.run_cli('get', 'demo', 'model').stdout.strip(), 'old')
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)

    def test_model_settings_projection_does_not_write(self):
        before = (conf_fields(self.conf), self.settings.read_bytes())
        result = self.store('model-settings', 'demo')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)['model'], 'old')
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)

    # ---- the first seed -------------------------------------------------

    def test_first_seed_imports_effective_native_values_and_leaves_settings_alone(self):
        self.settings.write_text(json.dumps({
            'model': 'native-model', 'effortLevel': 'high',
            'modelSettings': {'custom-model': {'effortLevel': 'low'}},
            'env': {'ANTHROPIC_DEFAULT_OPUS_MODEL': 'native-opus', 'UNRELATED': 'keep'},
            'other': True}))
        native = self.settings.read_bytes()
        result = self.store('seed-model-defaults', 'demo', 'model')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'native-model\n')
        fields = self.conf_fields()
        self.assertEqual(fields['model'], 'native-model')
        self.assertEqual(fields['effort'], 'high')
        self.assertEqual(fields['effort.custom-model'], 'low')
        self.assertEqual(fields['env.ANTHROPIC_DEFAULT_OPUS_MODEL'], 'native-opus')
        self.assertEqual(fields['model_defaults'], '1')
        # Unrelated conf keys survive, and native is untouched -- including the
        # non-mapping env key, which is not ccmorph's to move.
        self.assertEqual(fields['context_window.m'], '10000')
        self.assertEqual(self.settings.read_bytes(), native)

    def test_seed_is_idempotent(self):
        result = self.store('seed-model-defaults', 'demo')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '')
        before = (self.conf.read_bytes(), self.settings.read_bytes())
        result = self.store('seed-model-defaults', 'demo')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '')
        self.assertEqual((self.conf.read_bytes(), self.settings.read_bytes()), before)

    def test_explicit_conf_values_win_over_native_on_first_seed(self):
        self.conf.write_text('type=apikey\napi_key=k\nmodel=conf-model\neffort=low\n'
                             'env.ANTHROPIC_DEFAULT_OPUS_MODEL=conf-opus\n')
        self.settings.write_text(json.dumps({
            'model': 'native-model', 'effortLevel': 'high',
            'env': {'ANTHROPIC_DEFAULT_OPUS_MODEL': 'native-opus'}}))
        self.assertEqual(self.run_cli('get', 'demo', 'model').stdout.strip(), 'conf-model')
        self.assertEqual(self.run_cli('get', 'demo', 'effort').stdout.strip(), 'low')
        native = self.settings.read_bytes()
        result = self.store('seed-model-defaults', 'demo', 'model')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'conf-model\n')
        self.assertEqual(self.settings.read_bytes(), native)
        fields = self.conf_fields()
        self.assertEqual(fields['model'], 'conf-model')
        self.assertEqual(fields['effort'], 'low')
        self.assertEqual(fields['env.ANTHROPIC_DEFAULT_OPUS_MODEL'], 'conf-opus')

    def test_migrate_native_is_an_alias_for_seed_model_defaults(self):
        self.settings.write_text(json.dumps({'model': 'nm', 'effortLevel': 'high'}))
        native = self.settings.read_bytes()
        self.assertEqual(self.store('migrate-native', 'demo').returncode, 0)
        migrated = conf_fields(self.conf)
        self.assertEqual(self.settings.read_bytes(), native)
        # The same profile seeded the other way lands on the same conf.
        self.conf.write_text('type=apikey\napi_key=test-key\n'
                             'base_url=https://old.invalid\ncontext_window.m=10000\n')
        self.assertEqual(self.store('seed-model-defaults', 'demo').returncode, 0)
        self.assertEqual(conf_fields(self.conf), migrated)

    def test_unsupported_operation_is_refused_without_writing(self):
        before = (conf_fields(self.conf), self.settings.read_bytes())
        result = self.store('promote-native', 'demo')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unsupported configuration operation', result.stderr)
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)

    # ---- after the marker, native is no longer an input -------------------

    def test_inline_native_model_and_effort_are_ignored_after_seed(self):
        self.settings.write_text(json.dumps({
            'model': 'native-model', 'effortLevel': 'high',
            'modelSettings': {'custom-model': {'effortLevel': 'low'}}}))
        self.assertEqual(self.store('seed-model-defaults', 'demo').returncode, 0)
        seeded = conf_fields(self.conf)
        # What an in-session /model and /effort leave behind.
        self.settings.write_text(json.dumps({
            'model': 'inline-picked', 'effortLevel': 'xhigh',
            'modelSettings': {'custom-model': {'effortLevel': 'low'},
                              'inline-model': {'effortLevel': 'max'}}}))
        self.assertEqual(self.store('seed-model-defaults', 'demo').returncode, 0)
        self.assertEqual(conf_fields(self.conf), seeded)
        self.assertEqual(self.run_cli('get', 'demo', 'model').stdout.strip(), 'native-model')
        self.assertEqual(self.run_cli('get', 'demo', 'effort').stdout.strip(), 'high')
        projected = json.loads(self.store('model-settings', 'demo').stdout)
        self.assertEqual(projected['model'], 'native-model')
        self.assertEqual(projected['effortLevel'], 'high')
        self.assertEqual(projected['modelSettings'], {'custom-model': {'effortLevel': 'low'}})

    def test_absent_defaults_do_not_revive_native_values(self):
        self.conf.write_text('type=apikey\napi_key=k\nmodel_defaults=1\n')
        self.settings.write_text(json.dumps({
            'model': 'native-model', 'effortLevel': 'high',
            'modelSettings': {'m': {'effortLevel': 'low'}},
            'env': {'ANTHROPIC_MODEL': 'pin', 'ANTHROPIC_DEFAULT_OPUS_MODEL': 'native-opus'}}))
        before = (conf_fields(self.conf), self.settings.read_bytes())
        self.assertEqual(self.run_cli('get', 'demo', 'model').stdout.strip(), '')
        self.assertEqual(self.run_cli('get', 'demo', 'effort').stdout.strip(), '')
        result = self.store('seed-model-defaults', 'demo', 'model')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '\n')
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)
        self.assertEqual(json.loads(self.store('model-settings', 'demo').stdout),
                         {'env': {}, 'modelSettings': {}})
        # And the alias reader reports "nothing mapped" rather than the native value.
        alias = subprocess.run(
            ['zsh', '-fc', 'source "$1"; _ccm_settings_model_alias demo opus',
             '_', str(ROOT / 'lib/ccmorph.sh')],
            env=self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(alias.returncode, 0)
        self.assertEqual(alias.stdout.strip(), '')

    def test_role_mapping_is_read_from_the_conf(self):
        self.conf.write_text(self.conf.read_text() +
                             'env.ANTHROPIC_DEFAULT_OPUS_MODEL=profile-opus\nmodel_defaults=1\n')
        self.settings.write_text(json.dumps({
            'env': {'ANTHROPIC_DEFAULT_OPUS_MODEL': 'stale-opus'}}))
        result = subprocess.run(
            ['zsh', '-fc', 'source "$1"; _ccm_settings_model_alias demo opus',
             '_', str(ROOT / 'lib/ccmorph.sh')],
            env=self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'profile-opus')

    # ---- retired verbs ---------------------------------------------------

    def test_specialized_commands_refuse_without_changing_anything(self):
        before = (conf_fields(self.conf), self.settings.read_bytes())
        for role in ('fable', 'opus', 'sonnet', 'haiku', 'small-fast', 'subagent',
                     'effort:custom-model'):
            self.assertEqual(self.run_cli('set', 'demo', role, 'value').returncode, 2)
            self.assertEqual(self.run_cli('get', 'demo', role).returncode, 2)
            self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)

    def test_generated_settings_ignore_inline_defaults_but_preserve_other_settings(self):
        self.conf.write_text(self.conf.read_text() + 'model_defaults=1\nmodel=configured\neffort=medium\n')
        self.settings.write_text(json.dumps({
            'model': 'inline', 'effortLevel': 'high',
            'modelSettings': {'inline': {'effortLevel': 'high', 'thinkingBudget': 7}},
            'env': {'ANTHROPIC_MODEL': 'pinned', 'CLAUDE_CODE_EFFORT_LEVEL': 'high',
                    'ANTHROPIC_DEFAULT_OPUS_MODEL': 'stale'},
            'enabledPlugins': {'test@example': True},
            'permissions': {'deny': ['Bash(rm *)']}}))
        result = subprocess.run(['zsh', '-fc', 'source "$1"; _ccm_project_settings demo "$(_ccm_config_store model-settings demo)" --acw none',
                                 '_', str(ROOT / 'lib/ccmorph.sh')],
                                env=self.env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 0, result.stderr)
        saved = json.loads(self.settings.read_text())
        self.assertEqual(saved['model'], 'configured')
        self.assertEqual(saved['effortLevel'], 'medium')
        self.assertEqual(saved['modelSettings'], {'inline': {'thinkingBudget': 7}})
        self.assertNotIn('env', saved)
        self.assertEqual(saved['enabledPlugins'], {'test@example': True})
        self.assertEqual(saved['permissions'], {'deny': ['Bash(rm *)']})

    def test_prepare_launch_removes_only_owned_native_preferences(self):
        self.conf.write_text(self.conf.read_text() +
                             'model_defaults=1\nmodel=configured\neffort=medium\n')
        self.settings.write_text(json.dumps({
            'model': 'inline', 'effortLevel': 'high',
            'modelSettings': {'inline': {'effortLevel': 'high', 'thinkingBudget': 7}},
            'env': {'ANTHROPIC_MODEL': 'pinned', 'CLAUDE_CODE_EFFORT_LEVEL': 'high',
                    'ANTHROPIC_DEFAULT_OPUS_MODEL': 'stale', 'CUSTOM': 'keep'},
            'enabledPlugins': {'test@example': True},
            'permissions': {'deny': ['Bash(rm *)']}}))
        result = self.store('prepare-launch', 'demo')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.settings.read_text()), {
            'modelSettings': {'inline': {'thinkingBudget': 7}},
            'env': {'CUSTOM': 'keep'}, 'enabledPlugins': {'test@example': True},
            'permissions': {'deny': ['Bash(rm *)']}})
        self.assertEqual(self.conf_fields()['model'], 'configured')
        before = self.settings.read_bytes()
        self.assertEqual(self.store('prepare-launch', 'demo').returncode, 0)
        self.assertEqual(self.settings.read_bytes(), before)

    def test_prepare_launch_keeps_intentionally_absent_defaults_absent(self):
        self.conf.write_text('type=apikey\napi_key=k\nmodel_defaults=1\n')
        self.settings.write_text(json.dumps({
            'model': 'inline', 'effortLevel': 'high',
            'modelSettings': {'m': {'effortLevel': 'high'}},
            'env': {'ANTHROPIC_MODEL': 'pin'}}))
        self.assertEqual(self.store('prepare-launch', 'demo').returncode, 0)
        self.assertEqual(json.loads(self.store('model-settings', 'demo').stdout),
                         {'env': {}, 'modelSettings': {}})
        native = json.loads(self.settings.read_text())
        self.assertNotIn('model', native)
        self.assertNotIn('effortLevel', native)
        self.assertEqual(native['modelSettings'], {'m': {}})
        self.assertEqual(native['env'], {})

    def test_prepare_launch_seeds_before_clearing_and_refuses_invalid_settings(self):
        self.settings.write_text(json.dumps({'model': 'native', 'effortLevel': 'low'}))
        self.assertEqual(self.store('prepare-launch', 'demo').returncode, 0)
        self.assertEqual(self.conf_fields()['effort'], 'low')
        self.assertEqual(json.loads(self.settings.read_text()), {})
        self.settings.write_text('{"modelSettings": []}')
        before = (conf_fields(self.conf), self.settings.read_bytes())
        self.assertNotEqual(self.store('prepare-launch', 'demo').returncode, 0)
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)

    # ---- layering and validation ----------------------------------------

    def test_shared_native_settings_seed_under_the_profile(self):
        shared = self.home / '.claude/settings.json'
        shared.parent.mkdir()
        shared.write_text(json.dumps({'effortLevel': 'high'}))
        self.assertEqual(self.run_cli('get', 'demo', 'effort').stdout.strip(), 'high')
        self.assertEqual(self.run_cli('set', 'demo', 'effort', 'low').returncode, 0)
        self.assertEqual(self.run_cli('get', 'demo', 'effort').stdout.strip(), 'low')
        # One setter seeds every default, so the sibling model resolved from the
        # profile layer is captured too and cannot drift afterwards.
        self.assertEqual(self.conf_fields()['model'], 'old')
        self.assertEqual(json.loads(shared.read_text()), {'effortLevel': 'high'})

    def test_invalid_effort_never_changes_the_conf(self):
        before = conf_fields(self.conf)
        for value in ('invalid', 'MAX', ''):
            self.assertNotEqual(self.run_cli('set', 'demo', 'effort', value).returncode, 0)
            self.assertEqual(conf_fields(self.conf), before)

    def test_invalid_model_never_changes_endpoint_or_defaults(self):
        before = (conf_fields(self.conf), self.settings.read_bytes())
        result = self.run_cli('config', 'demo', input='https://new.invalid\nn\n-i\nhigh\ny\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)

    def test_malformed_settings_block_the_save_and_are_not_overwritten(self):
        self.settings.write_text('{"env": []}')
        before = conf_fields(self.conf)
        result = self.run_cli('set', 'demo', 'model', 'new')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.settings.read_text(), '{"env": []}')
        self.assertEqual(conf_fields(self.conf), before)

    def test_malformed_native_layers_block_the_save(self):
        # Native settings are external input to the conf writer, so a shape the
        # seed cannot read must stop the save rather than seed a partial default.
        shared = self.home / '.claude/settings.json'
        shared.parent.mkdir()
        before = conf_fields(self.conf)
        for path, text, message in (
                (shared, '{"env": []}', 'shared settings and its env must be objects'),
                (self.settings, '{"modelSettings": []}', 'modelSettings must be an object')):
            shared.write_text('{}')
            self.settings.write_text('{}')
            path.write_text(text)
            result = self.run_cli('set', 'demo', 'model', 'new')
            self.assertNotEqual(result.returncode, 0, message)
            self.assertIn(message, result.stderr)
            self.assertEqual(conf_fields(self.conf), before)

    def test_a_native_value_cannot_forge_conf_fields(self):
        # The conf is line-oriented `name=value`, so a native model carrying a
        # newline would otherwise write an extra field of the attacker's choosing.
        before = conf_fields(self.conf)
        self.settings.write_text(json.dumps({'model': 'a\nmodel_defaults=0'}))
        for args in (('get', 'demo', 'model'), ('set', 'demo', 'effort', 'high')):
            result = self.run_cli(*args)
            self.assertNotEqual(result.returncode, 0, args)
            self.assertIn('single-line strings', result.stderr)
            self.assertEqual(conf_fields(self.conf), before)

    def test_symlinked_settings_refuse_the_save(self):
        real = self.home / 'real-settings.json'
        real.write_text(json.dumps({'model': 'native'}))
        self.settings.unlink()
        self.settings.symlink_to(real)
        before = (conf_fields(self.conf), real.read_bytes())
        result = self.run_cli('set', 'demo', 'model', 'new')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('profile-local files', result.stderr)
        self.assertEqual((conf_fields(self.conf), real.read_bytes()), before)
        result = self.store('seed-model-defaults', 'demo', 'model')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, '')
        self.assertIn('profile-local files', result.stderr)
        self.assertEqual((conf_fields(self.conf), real.read_bytes()), before)
        # GH-327: a seed failure remains fatal even when launch captures stdout.
        result = subprocess.run(
            ['zsh', '-fc', 'source "$1"; _ccm_launch -a demo --model',
             '_', str(ROOT / 'lib/ccmorph.sh')],
            env=self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, '')
        self.assertIn('profile-local files', result.stderr)
        self.assertEqual((conf_fields(self.conf), real.read_bytes()), before)

    def test_native_launch_reuses_seeded_model_without_a_second_reader_gh327(self):
        self.conf.write_text('type=token\ntoken=test-token\nmodel=native-default\n')
        native = self.settings.read_bytes()
        bin_dir = self.home / 'bin'
        bin_dir.mkdir()
        calls = self.home / 'python-calls'
        wrapper = bin_dir / 'python3'
        wrapper.write_text('#!/bin/zsh\n'
                           'printf "%s\\n" "$2" >> "$TEST_PYTHON_CALLS"\n'
                           'exec "$TEST_REAL_PYTHON" "$@"\n')
        wrapper.chmod(0o755)
        env = {**self.env, 'PATH': str(bin_dir) + os.pathsep + os.environ['PATH'],
               'TEST_PYTHON_CALLS': str(calls),
               'TEST_REAL_PYTHON': shutil.which('python3')}
        # Stop at argument validation, immediately after the native default seam.
        # The shell's dynamic scope exposes the actual default consumed by launch.
        result = subprocess.run(
            ['zsh', '-fc', 'source "$1"; _ccm_err() { print -r -- "$default_model"; }; '
             '_ccm_launch -a demo --model', '_', str(ROOT / 'lib/ccmorph.sh')],
            env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertEqual(result.stdout, 'native-default\n')
        self.assertEqual([op for op in calls.read_text().splitlines()
                          if op in ('seed-model-defaults', 'get')], ['seed-model-defaults'])
        self.assertEqual(self.conf_fields()['model'], 'native-default')
        self.assertEqual(self.settings.read_bytes(), native)

    # ---- atomicity -------------------------------------------------------

    def fail_replace_env(self, target):
        hooks = self.home / 'python-hooks'
        hooks.mkdir()
        (hooks / 'sitecustomize.py').write_text(
            'import os\n'
            '_replace = os.replace\n'
            'def fail_replace(src, dst):\n'
            '    if str(dst) == os.environ.get("TEST_TARGET"):\n'
            '        raise OSError("simulated replace failure")\n'
            '    return _replace(src, dst)\n'
            'os.replace = fail_replace\n')
        return {**self.env, 'PYTHONPATH': str(hooks), 'TEST_TARGET': str(target)}

    def test_failed_conf_replacement_preserves_both_files(self):
        # Configure writes only the conf: a failed replacement must leave both
        # files untouched and no staging file in either directory.
        before = (conf_fields(self.conf), self.settings.read_bytes())
        env = self.fail_replace_env(self.conf)
        result = self.run_cli('config', 'demo',
                              input='https://new.invalid\nn\nnew\nhigh\ny\n', env=env)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('simulated replace failure', result.stderr)
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)
        self.assertEqual(list(self.state.glob('.configure.*')), [])
        self.assertEqual(list(self.settings.parent.glob('.configure.*')), [])

    def test_failed_launch_preparation_restores_seed_and_keeps_native_settings(self):
        before = (conf_fields(self.conf), self.settings.read_bytes())
        env = self.fail_replace_env(self.settings)
        result = subprocess.run(
            ['zsh', '-fc', 'source "$1"; _ccm_config_store prepare-launch demo',
             '_', str(ROOT / 'lib/ccmorph.sh')], env=env, text=True,
            capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('simulated replace failure', result.stderr)
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)
        self.assertEqual(list(self.state.glob('.configure.*')), [])
        self.assertEqual(list(self.settings.parent.glob('.configure.*')), [])

    def test_external_change_after_snapshot_is_preserved(self):
        command = ('source "$1"; export _CCM_CONFIG_SNAPSHOT=$(_ccm_config_store snapshot demo); '
                   'printf "%s\\n" "# external change" >> "$CCMORPH_STATE_DIR/demo.conf"; '
                   'export _CCM_CONFIG_MODEL=new; _ccm_config_store set demo')
        before_settings = self.settings.read_bytes()
        result = subprocess.run(['zsh', '-fc', command, '_', str(ROOT / 'lib/ccmorph.sh')],
                                env=self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('changed while configuring', result.stderr)
        self.assertTrue(self.conf.read_text().endswith('# external change\n'))
        self.assertNotIn('model=new', self.conf.read_text())
        self.assertEqual(self.settings.read_bytes(), before_settings)

    def test_blank_endpoint_can_keep_defaults(self):
        self.conf.write_text('type=apikey\napi_key=test-key\n')
        result = self.run_cli('config', 'demo', input='\nn\n\n\ny\n')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        fields = self.conf_fields()
        self.assertNotIn('base_url', fields)
        self.assertEqual(fields['api_key'], 'test-key')


    # ---- the launch's settings projection, folded into one run (GH-433) --
    #
    # A launch spent two interpreters on the same inputs back to back:
    # `_ccm_config_store prepare-launch`, then `settings-compose materialize`. GH-433
    # folds them into one run, so what has to hold is that the launch writes exactly
    # what the two runs wrote -- same files, same bytes, same status, nothing on
    # stdout -- and that every failure still lands on the status the launch handles.

    def seeded_launch_fixture(self):
        """The conf defaults, the shared native settings and the profile's own layer
        that a launch projects from."""
        self.conf.write_text(self.conf.read_text() +
                             'model_defaults=1\nmodel=configured\neffort=medium\n'
                             'env.ANTHROPIC_DEFAULT_OPUS_MODEL=profile-opus\n')
        shared = self.home / '.claude/settings.json'
        shared.parent.mkdir(exist_ok=True)
        shared.write_text(json.dumps({'model': 'canonical',
                                      'env': {'CLAUDE_CODE_EFFORT_LEVEL': 'low'}}))
        self.settings.write_text(json.dumps({
            'model': 'inline', 'effortLevel': 'high',
            'modelSettings': {'inline': {'effortLevel': 'high', 'thinkingBudget': 7}},
            'env': {'ANTHROPIC_MODEL': 'pinned', 'ANTHROPIC_DEFAULT_OPUS_MODEL': 'stale',
                    'CUSTOM': 'keep'},
            'enabledPlugins': {'test@example': True},
            'permissions': {'deny': ['Bash(rm *)']}}))

    def projection_env(self, tag, name='home', count_python=False):
        """A copy of this test's home to run one projection in, and optionally a
        `python3` that records every start.

        A projection rewrites the profile's settings, so two runs are comparable only
        from copies of the same state; and "one fewer interpreter" is a claim about
        starts, which only a counting wrapper settles."""
        root = Path(tempfile.mkdtemp(prefix=f'ccmorph-{tag}-'))
        self.addCleanup(shutil.rmtree, root, ignore_errors=True)
        home = root / name
        shutil.copytree(self.home, home)
        env = {**self.env, 'HOME': str(home), 'ZDOTDIR': str(home),
               'CCMORPH_STATE_DIR': str(home / 'state'),
               # A bytecode cache written beside the library is not part of either
               # run's result, and a tree comparison would see it.
               'PYTHONDONTWRITEBYTECODE': '1'}
        if not count_python:
            return env, None
        calls = root / 'python-calls'
        bin_dir = root / 'bin'
        bin_dir.mkdir()
        wrapper = bin_dir / 'python3'
        wrapper.write_text('#!/bin/zsh\n'
                           'printf "%s\\n" "$2" >> "$TEST_PYTHON_CALLS"\n'
                           'exec "$TEST_REAL_PYTHON" "$@"\n')
        wrapper.chmod(0o755)
        env.update({'PATH': str(bin_dir) + os.pathsep + os.environ['PATH'],
                    'TEST_PYTHON_CALLS': str(calls),
                    'TEST_REAL_PYTHON': shutil.which('python3')})
        return env, calls

    def project(self, style, opts=(), penv=(), env=None):
        """Run a launch's projection the way it ran before GH-433 -- prepare-launch,
        then materialize (`pair`) -- or through the helper that folds them (`one`),
        with the same options and launch environment."""
        script = ('source "$1"; shift; style="$1"; shift; o=(); '
                  'while (( $# )) && [[ "$1" != -- ]]; do o+=("$1"); shift; done; '
                  '(( $# )) && shift; '
                  'if [[ "$style" == pair ]]; then '
                  'saved=$(_ccm_config_store prepare-launch demo) || exit 10; '
                  '_ccm_project_settings demo "$saved" "${o[@]}" -- "$@"; '
                  'else _ccm_prepare_and_project demo "${o[@]}" -- "$@"; fi')
        return subprocess.run(
            ['zsh', '-fc', script, '_', str(ROOT / 'lib/ccmorph.sh'), style,
             *opts, '--', *penv],
            env=env or self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)

    def test_prepare_and_project_writes_what_two_runs_wrote(self):
        self.seeded_launch_fixture()
        # The launch resolves the tier roles itself, so its own values are kept out of
        # the generated env block rather than baked into the file.
        penv = ('ANTHROPIC_DEFAULT_OPUS_MODEL=launch-opus', 'ANTHROPIC_API_KEY=sk-live')
        untouched, _ = self.projection_env('gh433-untouched')
        two, _ = self.projection_env('gh433-pair')
        one, _ = self.projection_env('gh433-folded')
        pair = self.project('pair', opts=('--acw', '9000'), penv=penv, env=two)
        folded = self.project('one', opts=('--acw', '9000'), penv=penv, env=one)
        self.assertEqual((pair.returncode, pair.stdout, pair.stderr), (0, '', ''))
        self.assertEqual((folded.returncode, folded.stdout, folded.stderr), (0, '', ''))
        self.assertEqual(tree(two['HOME']), tree(one['HOME']))
        self.assertEqual(file_count(two['HOME']), file_count(one['HOME']))
        # ... and the comparison is not between two untouched homes.
        self.assertNotEqual(tree(two['HOME']), tree(untouched['HOME']))
        written = json.loads((Path(two['HOME']) / 'state/profiles/demo/settings.json')
                             .read_text())
        self.assertEqual(written['model'], 'configured')
        self.assertEqual(written['effortLevel'], 'medium')
        self.assertEqual(written['autoCompactWindow'], 9000)
        self.assertEqual(written['env'], {'CUSTOM': 'keep'})
        self.assertEqual(written['modelSettings'], {'inline': {'thinkingBudget': 7}})
        self.assertEqual(written['permissions'], {'deny': ['Bash(rm *)']})

    def test_prepare_and_project_store_failure_aborts_with_store_message(self):
        # The store's own status is what tells a launch to abort rather than start on
        # settings from last time; folding in a second run must not swallow it.
        self.settings.write_text('{"modelSettings": []}')
        before = (conf_fields(self.conf), self.settings.read_bytes())
        two, _ = self.projection_env('gh433-pair')
        one, _ = self.projection_env('gh433-folded')
        pair = self.project('pair', env=two)
        folded = self.project('one', env=one)
        self.assertEqual(pair.returncode, 10)
        self.assertEqual((folded.returncode, folded.stdout), (10, ''))
        self.assertEqual(folded.stderr, pair.stderr)
        self.assertIn('could not save/read configuration', pair.stderr)
        self.assertNotIn('settings-compose', pair.stderr)
        self.assertEqual((conf_fields(self.conf), self.settings.read_bytes()), before)
        self.assertEqual(tree(one['HOME']), tree(two['HOME']))

    def test_prepare_and_project_maps_a_failed_projection(self):
        # `settings-compose` refuses a launch three ways the launcher already handles:
        # 1 warns and falls back to the settings generated last time, 2 is a usage
        # error, 3 is the policy refusal it must not degrade past. Running it inside
        # the store's interpreter must put each of them back on its own status.
        self.seeded_launch_fixture()
        cases = (('usage-error', ('--nope',), None, 2),
                 ('materialize-error', ('--acw', 'bogus'), None, 1),
                 ('policy-refusal', ('--acw', 'bogus'), {'sandbox': {'enabled': True}}, 3))
        for name, opts, policy, expected in cases:
            with self.subTest(name):
                policy_path = self.state / 'profiles/demo/settings.policy.json'
                if policy:
                    policy_path.write_text(json.dumps(policy))
                else:
                    policy_path.unlink(missing_ok=True)
                two, _ = self.projection_env('gh433-pair')
                one, _ = self.projection_env('gh433-folded')
                pair = self.project('pair', opts=opts, env=two)
                folded = self.project('one', opts=opts, env=one)
                self.assertEqual(pair.returncode, expected, pair.stderr)
                self.assertEqual((folded.returncode, folded.stdout), (expected, ''))
                # The refusal names the profile's directory, and the two runs sit in
                # different copies of the home: compare the message with the copy it
                # names taken out.
                self.assertEqual(folded.stderr.replace(one['HOME'], '<home>'),
                                 pair.stderr.replace(two['HOME'], '<home>'))
                self.assertIn("could not generate settings for 'demo'", pair.stderr)
                self.assertEqual(tree(one['HOME']), tree(two['HOME']))

    def test_prepare_and_project_newline_argument_uses_two_runs(self):
        # The folded argv travels in the environment, one element per line, so an
        # element carrying a newline cannot be expressed: the helper must take the two
        # runs rather than pass a corrupted argument. A launch value may hold one --
        # the guard is on the argv, not on the environment.
        headers = ('ANTHROPIC_CUSTOM_HEADERS=accept: application/json\n'
                   'authorization: Bearer x')
        newline, calls = self.projection_env('gh433-newline', name='ho\nme',
                                             count_python=True)
        # `_CCM_MATERIALIZE` in the launch's own environment is how a parent shell
        # leaks it: the store turns it into materialize mode, which prints nothing to
        # read the saved defaults from, and the fallback's own store run must not
        # inherit it. Before the guard this aborted the launch with the store's
        # status.
        newline['_CCM_MATERIALIZE'] = '1'
        result = self.project('one', penv=(headers,), env=newline)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls.read_text().splitlines()), 2)
        # Falling back is a return to the two runs, not a second implementation of
        # them: the same inputs must write the same bytes as the paired path.
        pair, _ = self.projection_env('gh433-newline-pair', name='ho\nme')
        result = self.project('pair', penv=(headers,), env=pair)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(tree(newline['HOME']), tree(pair['HOME']))
        env, calls = self.projection_env('gh433-plain', count_python=True)
        result = self.project('one', penv=(headers,), env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls.read_text().splitlines()), 1)

    def test_a_projection_crash_with_a_linked_policy_refuses_through_the_folded_path(self):
        # 19780fc made a settings-compose crash -- a canonical nested past the
        # decoder's limit, a layer of tombstones it cannot parse -- refuse rather
        # than take the launch down (#420). GH-433 moved that crash inside the
        # store's interpreter, so the refusal has to survive the fold: the store
        # must still put it on status 13, the helper on 3, and a real launch must
        # refuse rather than abort or start.
        self.seeded_launch_fixture()
        shared = self.home / 'shared-policy.json'
        shared.write_text(json.dumps({'sandbox': {'enabled': True}}))
        (self.state / 'profiles/demo/settings.policy.json').symlink_to(shared)
        canon = self.home / '.claude/settings.json'
        good_canon = canon.read_text()
        crashes = (('deep-canonical', 'RecursionError', canon, TOO_DEEP_TO_DECODE),
                   ('malformed-tombstones', 'TypeError',
                    self.state / 'profiles/demo/settings.profile.json',
                    json.dumps({'$ccmorphUnset': None})))
        for name, crash_class, target, text in crashes:
            with self.subTest(name):
                # Each case starts from the pristine home.
                canon.write_text(good_canon)
                (self.state / 'profiles/demo/settings.profile.json').unlink(missing_ok=True)
                target.write_text(text)
                # The store's own status: the folded tail has to keep the refusal
                # on 13, the 10+rc shift the launch's helper maps back to 3.
                store = subprocess.run(
                    ['zsh', '-fc',
                     'source "$1"; '
                     '_ccm_projection_args --; '
                     '_ccm_settings_argv materialize demo '
                     '"${_ccm_sc_opts[@]}" "${_ccm_sc_strip[@]}"; '
                     'sc_argv=("${_ccm_sc_argv[@]}"); '
                     '_CCM_MATERIALIZE="${(pj:\n:)sc_argv}" _CCM_LIB="$_CCMORPH_LIBEXEC_DIR" '
                     '_CCM_SC_PY="$(_ccm_settings_compose_py)" '
                     '_ccm_config_store prepare-launch demo >/dev/null; '
                     'print -- $?',
                     '_', str(ROOT / 'lib/ccmorph.sh')],
                    env=self.env, text=True, capture_output=True, timeout=HANG_TIMEOUT)
                self.assertEqual(store.returncode, 0, store.stderr)
                self.assertEqual(store.stdout.strip(), '13')
                self.assertIn('settings.policy.json', store.stderr)
                self.assertIn(crash_class, store.stderr)
                # Both paths land on the helper's status 3 with the same words.
                two, _ = self.projection_env('gh433-crash-pair')
                one, _ = self.projection_env('gh433-crash-folded')
                pair = self.project('pair', env=two)
                folded = self.project('one', env=one)
                self.assertEqual(pair.returncode, 3, pair.stderr)
                self.assertEqual((folded.returncode, folded.stdout), (3, ''))
                self.assertEqual(folded.stderr.replace(one['HOME'], '<home>'),
                                 pair.stderr.replace(two['HOME'], '<home>'))
                self.assertIn("could not generate settings for 'demo'", folded.stderr)
                self.assertIn(crash_class, folded.stderr)
                # A real launch refuses: rc 1, the refusal message, claude never
                # started, and the old settings kept -- the store's stripped
                # layer, exactly what the folded projection copy holds.
                launch, _ = self.projection_env('gh433-crash-launch')
                home = Path(launch['HOME'])
                # Drop the conf's base_url in this copy alone: a non-Anthropic
                # base_url fronts the profile with an adapter, whose failure
                # aborts the launch before the projection. Without one the
                # profile takes the native route the refusal lives on, and the
                # projection itself reads nothing that changes.
                conf = home / 'state/demo.conf'
                conf.write_text(''.join(
                    line + '\n' for line in conf.read_text().splitlines()
                    if not line.startswith('base_url=')))
                (home / 'bin').mkdir()
                claude = home / 'bin/claude'
                claude.write_text('#!/bin/sh\n'
                                  'case "$1" in --help) exit 0 ;; esac\n'
                                  'touch "$HOME/claude-started"\n')
                claude.chmod(0o755)
                curl = home / 'bin/curl'
                curl.write_text('#!/bin/sh\nfor a do case "$a" in *http_code*) '
                                'echo 200; exit 0;; esac; done\nexit 1\n')
                curl.chmod(0o755)
                run = subprocess.run(
                    [str(ROOT / "bin/ccmorph"), 'run', 'demo'],
                    env={'HOME': str(home), 'PATH': f'{home}/bin:/usr/bin:/bin',
                         'LANG': 'C.UTF-8', 'XDG_CONFIG_HOME': str(home / '.config'),
                         'CCMORPH_STATE_DIR': launch['CCMORPH_STATE_DIR'],
                         'CCMORPH_LIB_DIR': str(ROOT / "lib"),
                         'PYTHONDONTWRITEBYTECODE': '1'},
                    stdin=subprocess.DEVNULL, text=True, capture_output=True,
                    timeout=HANG_TIMEOUT)
                self.assertEqual(run.returncode, 1, run.stdout + run.stderr)
                self.assertIn('will not start without its settings policy', run.stderr)
                self.assertFalse((home / 'claude-started').exists(),
                                 'a refused launch must not reach claude')
                self.assertEqual(
                    (home / 'state/profiles/demo/settings.json').read_bytes(),
                    (Path(one['HOME']) / 'state/profiles/demo/settings.json').read_bytes())


if __name__ == '__main__':
    unittest.main()
