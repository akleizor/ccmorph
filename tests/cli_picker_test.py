"""Picker UX under a real TTY: endpoint-driven choices, cancellation, pending
endpoint discovery, numbered fallback, and the keep/custom escapes.

Each child gets its own HOME, state dir, TMPDIR and a fake `curl`/`fzf` on PATH,
so no real profile, credential, or network is reached; every child runs under an
explicit deadline. Rows are selected by value or by the menu header, never by a
numbered count, so these tests do not pin a list length the picker may change.
"""
from __future__ import annotations

import fcntl, json, os, pty, re, select, shutil, signal, struct, subprocess, sys, tempfile, termios, time, unittest
from pathlib import Path
from unittest.mock import patch

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
CCMORPH = ROOT / 'lib/ccmorph.sh'
MODELS = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                              ('picker-alpha', 'picker-beta')]})

# `-w` marks the reachability probe; it must answer or discovery never runs.
FAKE_CURL = '''#!/bin/sh
printf '%s\\n' "$*" >> "$CURL_LOG"
[ "${1-}" = -K ] && cat >> "$CURL_LOG"
for arg in "$@"; do
  if [ "$arg" = -w ]; then printf 200; exit 0; fi
done
[ "$CURL_MODE" = ok ] || exit 22
printf '%s\\n' "$MODELS"
'''

# Selects by prompt argv: FZF_SPEC holds `prompt-substring=row-value` lines.
FAKE_FZF = '''#!/bin/sh
rows=$(cat)
printf 'ARGS %s\\nROWS %s\\n' "$*" "$rows" >> "$FZF_LOG"
prompt=""; previous=""
for arg in "$@"; do
  [ "$previous" = --prompt ] && prompt="$arg"
  previous="$arg"
done
pick=""
while IFS= read -r spec; do
  case "$spec" in
    *=*) pattern="${spec%%=*}"; value="${spec#*=}" ;;
    *) continue ;;
  esac
  case "$prompt" in *"$pattern"*) pick="$value"; break ;; esac
done < "$FZF_SPEC"
if [ "$pick" = CANCEL ]; then exit 130; fi  # Esc, as the real fzf does
if [ -n "$pick" ]; then
  printf '%s\\n' "$rows" |
    awk -F'\\t' -v want="$pick" '$1 == want { print; found = 1; exit } END { exit !found }'
else
  printf '%s\\n' "$rows" | head -1
fi
'''


def _pump(master, out, pos, pattern, deadline, process):
    """Read until `pattern` appears after `pos`; returns the new offset, or None."""
    while True:
        found = out.find(pattern, pos)
        if found >= 0:
            return found + len(pattern)
        if deadline - time.monotonic() <= 0:
            return None
        if not select.select([master], [], [], .25)[0]:
            # Recheck readiness after exit: output can arrive between select
            # timing out and poll observing the child's completion.
            if process.poll() is not None and not select.select([master], [], [], 0)[0]:
                return None
            continue
        try:
            chunk = os.read(master, 65536)
        except OSError:  # EIO: the child closed the terminal
            return None
        if not chunk:
            return None
        out.extend(chunk)
        if b'\x1b[6n' in chunk:  # fzf asks the terminal for its cursor position
            os.write(master, b'\x1b[1;1R')


def _drain(master, out, deadline, process):
    while deadline - time.monotonic() > 0:
        if not select.select([master], [], [], .25)[0]:
            # Recheck readiness after exit: output can arrive between select
            # timing out and poll observing the child's completion.
            if process.poll() is not None and not select.select([master], [], [], 0)[0]:
                return None
            continue
        try:
            chunk = os.read(master, 65536)
        except OSError:
            return
        if not chunk:
            return
        out.extend(chunk)
        if b'\x1b[6n' in chunk:  # fzf asks the terminal for its cursor position
            os.write(master, b'\x1b[1;1R')


def _wait_that_times_out_once():
    """A `subprocess.Popen.wait` stand-in whose first call reports a timeout.

    Only that first wait's outcome is faked: the cleanup's kill and the reap
    after it stay real. The real method is read here, before patching installs
    this one, or the later call would recurse into the fake.
    """
    wait, timed_out = subprocess.Popen.wait, []

    def wait_times_out(process, timeout=None):
        if not timed_out:
            timed_out.append(True)
            raise subprocess.TimeoutExpired(process.args, timeout)
        return wait(process, timeout)

    return wait_times_out


def cells(text):
    """Terminal cells, counting East Asian wide characters as two."""
    import unicodedata
    return sum(2 if unicodedata.east_asian_width(c) in 'WF' else 1 for c in text)


def _controlling_terminal():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)


def run_pty(command, env, exchanges=(), size=(24, 100), library=CCMORPH):
    """Run `command` on a pty, answering each (pattern, reply) in turn."""
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', *size, 0, 0))
    input_flags = termios.tcgetattr(slave)[3] & (termios.ECHO | termios.ICANON | termios.ISIG)
    process = subprocess.Popen(
        ['zsh', '-fc', 'source "$1"\n' + command, 'ccmorph-picker-test', str(library)],
        stdin=slave, stdout=slave, stderr=slave, env=env, cwd=str(ROOT), close_fds=True,
        preexec_fn=_controlling_terminal)
    # Keep a slave reference until capture ends: macOS can discard unread output
    # when the child closes the last slave before this observer gets scheduled.
    out, pos = bytearray(), 0
    deadline, missed = time.monotonic() + HANG_TIMEOUT, None
    try:
        for pattern, reply in exchanges:
            pos = _pump(master, out, pos, pattern, deadline, process)
            if pos is None:
                missed = pattern
                break
            if reply:
                os.write(master, reply)
        if missed is None:
            _drain(master, out, deadline, process)
    finally:
        os.close(slave)
        try:
            # The pump and drain above already spent part of the bound, and a
            # child still alive after them is the same hang: spend what is left,
            # so HANG_TIMEOUT bounds the call rather than each of its waits.
            process.wait(timeout=max(0.1, deadline - time.monotonic()) if missed is None else 0.5)
        except subprocess.TimeoutExpired:
            missed = missed or b'process did not exit'
            try:
                # The controlling-terminal setup owns a session, including shell children.
                os.killpg(process.pid, signal.SIGKILL)
            except OSError:
                # The group is already gone, which macOS spells two ways: EPERM
                # while its only member is the exited, unreaped leader that the
                # wait above timed out on, ESRCH once nothing is left. The wait
                # below still reaps the leader, and the missed bound above is the
                # real problem the caller hears about.
                pass
            process.wait(timeout=HANG_TIMEOUT)
        restored = termios.tcgetattr(master)[3] & (termios.ECHO | termios.ICANON | termios.ISIG)
        if restored != input_flags:
            missed = missed or b'terminal input mode not restored'
        os.close(master)
    return process.returncode, out.decode('utf-8', 'replace'), missed


class PickerTTYTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix='ccmorph-picker-'))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.state = self.tmp / 'state'
        for path in (self.tmp / 'home/.claude', self.state / 'profiles', self.tmp / 'bin',
                     self.tmp / 'tmpdir'):
            path.mkdir(parents=True, exist_ok=True)

    def env(self, *, fzf=False, curl='ok'):
        for name, body in (('curl', FAKE_CURL), ('fzf', FAKE_FZF)):
            if name == 'fzf' and not fzf:
                continue
            path = self.tmp / 'bin' / name
            path.write_text(body)
            path.chmod(0o755)
        # Real PATH minus any entry carrying a real fzf: `whence -p fzf` must fail
        # for the numbered fallback, while python3 and zsh still resolve.
        entries = [str(self.tmp / 'bin')]
        for entry in os.environ['PATH'].split(os.pathsep):
            if fzf or not (Path(entry) / 'fzf').exists():
                entries.append(entry)
        (self.tmp / 'curl.log').touch()
        return {'HOME': str(self.tmp / 'home'), 'CCMORPH_STATE_DIR': str(self.state),
                'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'PATH': os.pathsep.join(entries),
                'TMPDIR': str(self.tmp / 'tmpdir'), 'TERM': 'xterm', 'LANG': 'C.UTF-8',
                'CURL_LOG': str(self.tmp / 'curl.log'), 'CURL_MODE': curl, 'MODELS': MODELS,
                'FZF_LOG': str(self.tmp / 'fzf.log'), 'FZF_SPEC': str(self.tmp / 'fzf.spec')}

    def spec(self, *lines):
        (self.tmp / 'fzf.spec').write_text('\n'.join(lines) + '\n')

    def profile(self, name='demo', endpoint='https://old.example', key='old-secret'):
        (self.state / f'{name}.conf').write_text(
            f'type=apikey\napi_key={key}\nbase_url={endpoint}\n')
        home = self.state / 'profiles' / name
        home.mkdir(parents=True, exist_ok=True)
        settings = {'model': 'native-model', 'effortLevel': 'low', 'other': {'keep': True}}
        (home / 'settings.json').write_text(json.dumps(settings))

    def field(self, name, key):
        lines = (self.state / f'{name}.conf').read_text().splitlines()
        values = [line.split('=', 1)[1] for line in lines if line.startswith(key + '=')]
        return values[-1] if values else None

    def native(self, name='demo'):
        return (self.state / 'profiles' / name / 'settings.json').read_bytes()

    def test_cold_launch_selects_a_model_before_starting_the_adapter(self):
        env = self.env(fzf=True)
        self.profile()
        (self.state / 'profiles/demo/settings.json').write_text('{}')
        # Stop at the real launcher's adapter boundary: no client or inference.
        command = '_ccm_adapter_start() { print -r -- "SELECTED=$2"; return 1; }\nccmorph run demo'
        self.spec('=picker-beta')
        rc, out, missed = run_pty(command, env)
        self.assertIsNone(missed, out)
        self.assertIn('SELECTED=picker-beta', out)
        self.assertIsNone(self.field('demo', 'model'))  # invocation-only choice
        self.assertNotIn('old-secret', out)

        self.spec('=CANCEL')
        rc, out, missed = run_pty(command, env)
        self.assertNotEqual(rc, 0, out)
        self.assertNotIn('SELECTED=', out)  # cancellation must not start anything

    def test_automatic_picker_preserves_explicit_saved_and_headless_choices(self):
        env = self.env(fzf=True)
        self.profile()
        (self.state / 'profiles/demo/settings.json').write_text('{}')
        command = '_ccm_adapter_start() { print -r -- "SELECTED=<$2>"; return 1; }\nccmorph run demo'
        self.spec('=CANCEL')  # any unexpected picker would abort before SELECTED
        for args, expected in (('--model picker-alpha', 'picker-alpha'),
                               ('--print hello', ''), ('--continue', ''),
                               ('--resume absent-session', '')):
            with self.subTest(args=args):
                rc, out, _ = run_pty(command + ' ' + args, env)
                self.assertIn('SELECTED=<' + expected + '>', out)
        result = subprocess.run(['zsh', '-fc', 'source "$1"\n' + command, 'test', str(CCMORPH)],
                                env=env, stdin=subprocess.DEVNULL, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        self.assertIn('SELECTED=<>', result.stdout)
        with (self.state / 'demo.conf').open('a') as conf:
            conf.write('model=picker-alpha\n')
        rc, out, _ = run_pty(command, env)
        self.assertIn('SELECTED=<picker-alpha>', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.spec('=picker-beta')
        rc, out, _ = run_pty(command + ' -i', env)
        self.assertIn('SELECTED=<picker-beta>', out)
        self.assertEqual(self.field('demo', 'model'), 'picker-alpha')

    def test_configure_picker_offers_endpoint_models_and_saves_the_pick(self):
        env = self.env(fzf=True)
        self.profile()
        before = self.native()
        self.spec('Default model=picker-beta', 'Default effort=xhigh')
        rc, out, missed = run_pty('ccmorph config -i', env, [(b'Settings', b'\r'), (b'Default effort', b'\x1b[B' * 3 + b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        log = (self.tmp / 'fzf.log').read_text()
        self.assertIn('--prompt Default model — demo>', log)
        self.assertIn('picker-beta', log)  # the endpoint's own listing reached the picker
        self.assertEqual(self.field('demo', 'model'), 'picker-beta')
        self.assertEqual(self.field('demo', 'effort'), 'xhigh')
        self.assertEqual(self.native(), before)
        self.assertNotIn('Save changes?', out)
        self.assertNotIn('Endpoint [', out)
        self.assertNotIn('Replace stored credential?', out)

    def test_cancelling_configure_saves_nothing(self):
        env = self.env()
        self.profile()
        conf = self.state / 'demo.conf'
        before = (conf.read_bytes(), self.native())
        rc, out, missed = run_pty('ccmorph config demo', env, [
            (b'Settings', b'\r'),
            (b'Default model', b'q\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertNotEqual(rc, 0, out)
        self.assertEqual((conf.read_bytes(), self.native()), before)

    def test_connection_edit_skips_models_and_next_picker_uses_new_connection(self):
        env = self.env(fzf=True)
        self.profile()
        self.spec('=CANCEL')
        rc, out, missed = run_pty('ccmorph config demo', env, [
            (b'Settings', b'\x1b[B\r'),
            (b'Endpoint [https://old.example]', b'https://new.example\n'),
            (b'Credential (hidden; Enter keeps current):', b'new-secret\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotIn('Default model:', out)
        self.assertNotIn('new-secret', out)
        self.assertEqual((self.tmp / 'curl.log').read_text(), '')
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.assertEqual(self.field('demo', 'model'), 'native-model')
        self.assertEqual(self.field('demo', 'effort'), 'low')
        self.assertEqual(self.field('demo', 'base_url'), 'https://new.example')
        self.assertEqual(self.field('demo', 'api_key'), 'new-secret')

        self.spec('Default model=picker-beta', 'Default effort=high')
        rc, out, missed = run_pty('ccmorph config demo', env, [(b'Settings', b'\r'), (b'Default effort', b'\x1b[B' * 2 + b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        log = (self.tmp / 'curl.log').read_text()
        self.assertIn('https://new.example/v1/models?limit=1000', log)
        self.assertIn('x-api-key: new-secret', log)
        self.assertNotIn('old.example', log)
        self.assertNotIn('old-secret', log)
        self.assertEqual(self.field('demo', 'model'), 'picker-beta')
        self.assertEqual(self.field('demo', 'effort'), 'high')

    def test_confirmation_uses_native_menu_with_safe_default(self):
        env = self.env()
        rc, out, missed = run_pty('source "$1"\nccm_ui_confirm "Proceed?"', env, [
            (b'Esc Cancel', b'\x1b[B\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotIn('Select [1-', out)

    def test_numbered_fallback_reprompts_until_the_choice_is_valid(self):
        env = self.env()
        env['ROWS'] = 'alpha\tfirst choice\nbeta\tsecond choice'
        rc, out, missed = run_pty('source "$1"\nccm_ui_select search "pick one" "$ROWS" all', env, [
            (b'Select [1-2]', b'abc\n'),                                # not a number
            (b'Please choose one of the numbered options.', b'9\n'),  # out of range
            (b'Please choose one of the numbered options.', b'2\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('  1) alpha    \x1b[38;2;153;153;153mfirst choice', out)
        self.assertTrue(out.rstrip().endswith('beta'), out)

    def test_set_without_slot_picks_slot_then_value(self):
        env = self.env()
        self.profile()
        rc, out, missed = run_pty('ccmorph set demo', env, [
            (b'Setting', b'\x1b[B\r'),  # effort
            (b'Effort', b'\x1b[B' * 2 + b'\r'),  # high
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('Medium', out)
        self.assertIn('Extra high', out)
        self.assertEqual(self.field('demo', 'effort'), 'high')
        self.assertEqual(self.field('demo', 'model'), 'native-model')  # seeded, not guessed

    def test_set_dash_i_chains_profile_then_value_without_losing_the_slot(self):
        env = self.env(fzf=True)
        self.profile()
        self.spec('Profile=demo', 'effort=medium')
        rc, out, missed = run_pty('ccmorph set -i effort', env, [(b'Effort', b'\x1b[B\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        log = (self.tmp / 'fzf.log').read_text()
        self.assertIn('--prompt Profile>', log)
        self.assertIn('Effort', out)  # `effort` survived the -i shift
        self.assertNotIn('--prompt Effort — demo>', log)  # and was not re-asked as a slot menu
        self.assertEqual(self.field('demo', 'effort'), 'medium')

    def test_unavailable_catalogue_still_allows_keep_or_custom(self):
        env = self.env(fzf=True, curl='fail')
        self.profile('demo')
        self.profile('keepme')
        self.spec('Default model=@custom')
        rc, out, missed = run_pty('ccmorph config demo', env, [
            (b'Settings', b'\r'),
            (b'Custom model ID (Enter keeps current):', b'ghost-model\n'),
            (b'Default effort', b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('could not reach', out)
        self.assertIn('--prompt Default model — demo>', (self.tmp / 'fzf.log').read_text())
        self.assertEqual(self.field('demo', 'model'), 'ghost-model')
        self.assertEqual(self.field('demo', 'effort'), 'low')

        self.spec('Default model=@keep')
        rc, out, missed = run_pty('ccmorph config keepme', env, [
            (b'Settings', b'\r'),
            (b'Default effort', b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.field('keepme', 'model'), 'native-model')

    def test_add_uses_native_fixed_menu_even_when_fzf_is_installed(self):
        env = self.env(fzf=True)
        self.spec('=CANCEL')  # fzf must not be reached at all
        rc, out, missed = run_pty(
            '_ccm_provider_list() { REPLY=(claude mockprov); return 0 }\nccmorph add demo',
            env, [(b'Esc Cancel', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertIn('\x1b[38;2;215;119;87m\x1b[1mSign-in method — demo\x1b[0m', out)
        self.assertIn('\x1b[38;2;177;185;249mClaude account\x1b[0m    \x1b[38;2;153;153;153mSign in with your browser\x1b[0m', out)
        self.assertNotIn('type to filter', out)
        self.assertNotIn('Select [', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.assertFalse((self.state / 'demo.conf').exists())

    def test_add_command_entrypoint_styles_descriptions(self):
        env = self.env(fzf=True)
        self.spec('Sign-in method=CANCEL')
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        rc, out, missed = run_pty('exec "$CLI_TARGET" add demo', env, [(b'Esc Cancel', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)  # intentional cancellation, before login
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.assertIn('\x1b[38;2;177;185;249mClaude account\x1b[0m    \x1b[38;2;153;153;153mSign in with your browser\x1b[0m', out)
        self.assertIn('API key           \x1b[38;2;153;153;153mAnthropic or a gateway\x1b[0m', out)
        self.assertFalse((self.state / 'demo.conf').exists())

    def test_labeled_menu_and_help_remain_plain_when_color_is_disabled(self):
        env = {**self.env(), 'NO_COLOR': '1', 'ROWS': 'internal\tAction\tDescription'}
        rc, out, missed = run_pty('_ccm_ui_numbered Choose "$ROWS" labels; ccmorph --help', env, [
            (b'(q to cancel):', b'1\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('Action    Description', out)
        self.assertIn('Launch the default profile', out)
        self.assertNotIn('\x1b[', out)

    def test_add_finishes_without_optional_setup_questions(self):
        env = self.env(fzf=True)
        self.spec('Sign-in method=token')
        rc, out, missed = run_pty(
            '_ccm_provider_list() { REPLY=(claude); return 0 }\nccmorph add demo', env, [
            (b'Sign-in method', b'\x1b[B\r'),
            (b'Paste token for', b'test-token\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotIn('Configure model', out)
        self.assertNotIn('Save changes?', out)
        self.assertNotIn('test-token', out)
        self.assertIn('Next: ccmorph run demo', out)
        self.assertIsNone(self.field('demo', 'model'))
        self.assertIsNone(self.field('demo', 'effort'))

    def test_default_picker_hides_internal_action_values(self):
        env = self.env(fzf=True)
        self.profile()
        self.spec('Default model=picker-alpha')
        rc, out, missed = run_pty('ccmorph config demo', env, [(b'Settings', b'\r'), (b'Default effort', b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        log = (self.tmp / 'fzf.log').read_text()
        self.assertIn('--with-nth=2..', log)

    def test_cancelling_unfinished_edits_preserves_defaults_and_connection(self):
        env = self.env(fzf=True)
        self.profile()
        conf = self.state / 'demo.conf'
        before = conf.read_bytes(), self.native()
        self.spec('Default model=picker-beta', 'Default effort=CANCEL')
        rc, out, missed = run_pty('ccmorph config demo', env, [(b'Settings', b'\r'), (b'Default effort', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertEqual((conf.read_bytes(), self.native()), before)

        self.spec('Settings=connection')
        rc, out, missed = run_pty('ccmorph config demo', env, [
            (b'Settings', b'\x1b[B\r'),
            (b'Endpoint [', b'https://replacement.example\n'),
            (b'Credential (hidden;', b'\x04'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertEqual((conf.read_bytes(), self.native()), before)

    def test_picker_profiles_match_the_commands_capabilities(self):
        env = self.env(fzf=True)
        self.profile('gateway')
        (self.state / 'subscription.conf').write_text('type=sub\nemail=demo@example.com\n')
        (self.state / 'token.conf').write_text('type=token\ntoken=fixture-secret\n')
        (self.state / 'provider.conf').write_text('type=provider\nprovider=mockprov\n')
        self.spec('Profile=CANCEL')
        for verb, offered, excluded in [
            ('models', ('gateway',), ('subscription', 'token', 'provider')),
            ('cred', ('gateway', 'token'), ('subscription', 'provider')),
            ('login', ('provider',), ('gateway', 'subscription', 'token')),
        ]:
            with self.subTest(verb=verb):
                (self.tmp / 'fzf.log').write_text('')
                rc, out, missed = run_pty('ccmorph ' + verb, env)
                self.assertIsNone(missed, out)
                self.assertNotEqual(rc, 0, out)
                rows = (self.tmp / 'fzf.log').read_text().split('ROWS ', 1)[1]
                values = [line.split('\t')[0] for line in rows.splitlines()]
                self.assertEqual(set(values), set(offered))
                for name in excluded:
                    self.assertNotIn(name, values)
                self.assertNotIn('fixture-secret', out)

    def test_current_value_is_not_offered_twice(self):
        env = self.env(fzf=True)
        self.profile()
        self.spec('Default effort=@keep')
        rc, out, missed = run_pty('_ccm_prompt_default effort demo low', env, [(b'Default effort', b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        log = re.sub(r'\x1b\[[0-9;]*m', '', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.assertIn('Keep low', log)
        self.assertNotIn('\x1b[1mlow\x1b[0m', out)

    def test_plain_terminal_output_and_explicit_cancel(self):
        for settings in ({'NO_COLOR': '1'}, {'TERM': 'dumb'}):
            env = {**self.env(), **settings, 'ROWS': 'a\tFirst\nb\tSecond'}
            rc, out, missed = run_pty('_ccm_ui_numbered choose "$ROWS"', env, [
                (b'(q to cancel):', b'q\n'),
            ])
            self.assertIsNone(missed, out)
            self.assertNotEqual(rc, 0, out)
            self.assertNotIn('\x1b[', out)

    def test_command_entrypoint_configures_only_the_selected_task(self):
        env = self.env(fzf=True)
        self.profile()
        before = self.native()
        self.spec('Default model=picker-beta', 'Default effort=high')
        # Delivery can name the installed command; otherwise exercise the source
        # executable. Let it resolve its own library, never a test override.
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            # The source installer locates code under XDG_CONFIG_HOME. Keep that
            # real layout while explicit STATE_DIR and HOME isolate all user data.
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        rc, out, missed = run_pty('exec "$CLI_TARGET" config demo', env, [(b'Settings', b'\r'), (b'Default effort', b'\x1b[B' * 2 + b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.field('demo', 'model'), 'picker-beta')
        self.assertEqual(self.field('demo', 'effort'), 'high')
        self.assertEqual(self.field('demo', 'api_key'), 'old-secret')
        self.assertEqual(self.native(), before)
        self.assertNotIn('Endpoint [', out)
        self.assertNotIn('old-secret', out)

    def test_real_keyboard_selects_one_item_and_escape_cancels(self):
        real_fzf = shutil.which('fzf')
        if not real_fzf:
            self.skipTest('fzf is not installed; numbered fallback covered separately')
        env = self.env(fzf=True)
        (self.tmp / 'bin/fzf').unlink()
        env['PATH'] = str(Path(real_fzf).parent) + os.pathsep + env['PATH']
        env['ROWS'] = 'alpha\tFirst option\tFirst explanation\nbeta\tSecond option\tSecond explanation'
        command = 'picked=$(ccm_ui_select search "Keyboard check" "$ROWS") || exit $?; print -r -- "RESULT=$picked"'
        rc, out, missed = run_pty(command, env, [(b'Second option', b'\x1b[B\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('RESULT=beta', out)
        self.assertNotIn('RESULT=alpha', out)
        rc, out, missed = run_pty(command, env, [(b'Second option', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertNotIn('RESULT=', out)

    def test_command_entrypoint_accepts_real_arrow_and_enter_without_extra_questions(self):
        env = self.env(fzf=True)
        self.spec('=CANCEL')
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        rc, out, missed = run_pty('exec "$CLI_TARGET" add demo', env, [
            (b'Claude token', b'\x1b[B\r'),
            (b'Paste token for', b'fixture-token\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.field('demo', 'type'), 'token')
        self.assertEqual(self.field('demo', 'token'), 'fixture-token')
        self.assertNotIn('fixture-token', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.assertNotIn('Select [', out)
        self.assertNotIn('Save changes?', out)
        self.assertNotIn('Configure model', out)
        self.assertIn('Next: ccmorph run demo', out)

    def test_native_fixed_keys_restore_terminal_and_never_spawn_fzf(self):
        env = self.env(fzf=True)
        env['ROWS'] = 'alpha\tFirst\tDescription\nbeta\tSecond\tDescription'
        command = 'picked=$(ccm_ui_select native "Native choices" "$ROWS"); rc=$?; print -r -- "RESULT=$picked"; exit $rc'
        cases = [(b'\r', 0, 'alpha'), (b'\x1b[A\r', 0, 'alpha'),
                 (b'\x1b[B\r', 0, 'beta'), (b'\x1bOB\r', 0, 'beta'),
                 (b'\x1b[B\x1b[A\r', 0, 'alpha'), (b'\x1b', 1, ''),
                 (b'\x03', 130, ''), (b'\x04', 1, '')]
        for keys, expected_rc, value in cases:
            with self.subTest(keys=keys):
                rc, out, missed = run_pty(command, env, [(b'Esc Cancel', keys)])
                self.assertIsNone(missed, out)  # includes echo/canonical/signal restoration
                self.assertEqual(rc, expected_rc, out)
                self.assertIn('RESULT=' + value, out)
                self.assertIn('\x1b[?25h', out)
                self.assertIn('\x1b[?7h', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_native_fixed_menu_scrolls_in_a_small_terminal(self):
        env = self.env(fzf=True)
        env['ROWS'] = '\n'.join(f'{i}\tOption {i}\t' + 'long description ' * 8 for i in range(10))
        rc, out, missed = run_pty(
            'picked=$(ccm_ui_select native "Native choices" "$ROWS") || exit $?; print -r -- "RESULT=$picked"',
            env, [(b'Esc Cancel', b'\x1b[B' * 9 + b'\r')], size=(8, 35))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('RESULT=9', out)
        self.assertTrue(all(int(n) <= 7 for n in re.findall(r'\x1b\[(\d+)A', out)), out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_native_fixed_menu_preserves_no_color_and_aligns_descriptions(self):
        env = {**self.env(fzf=True), 'NO_COLOR': '1',
               'ROWS': 'a\tShort\tFirst description\nb\tLonger label\tSecond description'}
        rc, out, missed = run_pty('ccm_ui_select native Choose "$ROWS"', env, [(b'Esc Cancel', b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotRegex(out, r'\x1b\[[0-9;]*m')
        first = next(line.index('First description') for line in out.splitlines() if 'First description' in line)
        second = next(line.index('Second description') for line in out.splitlines() if 'Second description' in line)
        self.assertEqual(first, second, out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_provider_labels_are_cased_without_changing_selected_ids(self):
        env = self.env(fzf=True)
        command = (
            '_ccm_provider_list() { REPLY=(claude antigravity codex devin kimi meta xai); return 0 }\n'
            '_ccm_provider_login() { print -r -- "PROVIDER_ID=$2"; return 1 }\n'
            'ccmorph add my_profile'
        )
        rc, out, missed = run_pty(command, env, [
            (b'Esc Cancel', b'\x1b[B' * 8 + b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)  # stop before authentication or saving
        self.assertIn('Sign-in method — my_profile', out)
        for label in ('Antigravity', 'Codex', 'Devin', 'Kimi', 'Meta', 'xAI'):
            self.assertIn(label, out)
        self.assertIn('\x1b[38;2;153;153;153mSign in with your browser', out)
        self.assertIn('\x1b[38;2;153;153;153mPaste an existing OAuth token', out)
        self.assertIn('PROVIDER_ID=xai', out)
        self.assertNotIn('PROVIDER_ID=xAI', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_standalone_ui_contract_and_unicode_alignment(self):
        env = {**self.env(fzf=True), 'NO_COLOR': '1',
               'ROWS': 'first\t中文\tFirst description\nsecond\tA\tSecond description'}
        self.spec('Standalone=second')
        # Only the module is sourced: it must not require ccmorph state/helpers.
        rc, out, missed = run_pty('ccm_ui_select native Standalone "$ROWS"', env,
                                  [(b'Esc Cancel', b'\x1b[B\r')],
                                  library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', out)
        # One left edge: heading and labels start after the margin, only ❯ hangs.
        self.assertIn('  Standalone\r\n\r\n❯ 中文', plain)
        self.assertIn('\r\n  A ', plain)
        # Descriptions align in terminal cells (中文 is 4 cells) a fixed gap after
        # the widest label.
        rows = [line.split('\x1b')[0] for line in plain.split('\r\n') if ' description' in line]
        starts = {cells(re.split(r'(First|Second) description', line)[0]) for line in rows}
        self.assertEqual(starts, {2 + cells('中文') + 4}, rows)
        self.assertIn('  Standalone › A\r\nsecond', plain)
        self.assertTrue(out.rstrip().endswith('second'), out)
        self.assertNotRegex(out, r'\x1b\[[0-9;]*m')
        self.assertFalse((self.tmp / 'fzf.log').exists())
        rc, out, missed = run_pty('ccm_ui_select search Standalone "$ROWS"', env,
                                  library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertTrue(out.rstrip().endswith('second'), out)
        log = (self.tmp / 'fzf.log').read_text()
        self.assertIn('--pointer=❯', log)
        self.assertIn('--no-color', log)
        self.assertNotRegex(log, r'\x1b\[[0-9;]*m')

    def test_native_menu_fits_every_line_to_a_narrow_terminal(self):
        env = {**self.env(fzf=True), 'NO_COLOR': '1',
               'ROWS': 'a\tA fairly long option label\t' + 'wordy description ' * 6}
        rc, out, missed = run_pty('ccm_ui_select native "A heading far wider than this pane" "$ROWS"',
                                  env, [(b'Esc', b'\r')], size=(12, 30))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', out)
        lines = [line for line in re.split(r'[\r\n]+', plain) if line.strip() and 'Move' not in line]
        self.assertTrue(lines and all(cells(line) <= 30 for line in lines), lines)

    def test_standalone_helpers_survive_caller_state(self):
        script = r"""
LC_ALL=C
_ccm_ui_fit "Sign-in method — demo" 18; print -r -- "FIT=$REPLY"
interrupted=143
print hello | ccm_ui_input "Name: " 2>/dev/null; print "INPUT=$? $REPLY"
setopt sh_glob
print yes | ccm_ui_confirm 'Go?' 2>/dev/null; print "CONFIRM=$?"
ccm_ui_list Pick $'a\tAlpha' 2>&1 >/dev/null | od -c | grep -q 033 && print LISTANSI=1 || print LISTANSI=0
"""
        rc, out, _ = run_pty(script, self.env(), library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertEqual(rc, 0, out)
        self.assertIn('FIT=Sign-in method — …', out)   # whole characters, valid UTF-8
        self.assertIn('INPUT=0 hello', out)             # caller's name is not the signal flag
        self.assertIn('CONFIRM=0', out)                 # caller's glob options do not flip Yes
        self.assertIn('LISTANSI=0', out)                # redirected stderr stays plain

    def test_installed_resume_status_uses_brand_not_green(self):
        env = self.env()
        for name in ('demo', 'target'):
            (self.state / f'{name}.conf').write_text(
                'type=token\ntoken=synthetic-token\nmodel=sonnet\neffort=low\nmodel_defaults=1\n')
        # Exercise the real launch/resume driver with a non-networked Claude stub.
        claude = self.tmp / 'bin/claude'
        claude.write_text("""#!/bin/sh
if [ "$1" = --help ]; then printf 'Commands:\n'; exit; fi
if [ ! -f "$HOME/has-resumed" ]; then
  touch "$HOME/has-resumed"
  printf 'profile=target\nsession=fixture-session\n' > "$CCMORPH_STATE_DIR/.reswitch.$CCMORPH_RELAY_ID"
fi
""")
        claude.chmod(0o755)
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        rc, out, missed = run_pty('exec "$CLI_TARGET" run demo', env)
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('\x1b[38;2;215;119;87mccmorph:\x1b[0m resuming as', out)
        self.assertNotIn('\x1b[32m', out)
        self.assertNotIn('\x1b[38;2;78;186;101m', out)
        self.assertNotIn('synthetic-token', out)

    def test_theme_reload_recolors_older_running_driver(self):
        # A running shell can still be executing the old driver's function body.
        # Sourcing the updated theme must replace, not retain, its green value.
        rc, out, missed = run_pty(
            '_ccm_green=$\'\\e[32m\'; source "$CCMORPH_LIB_DIR/ccmorph-ui.zsh"; '
            'print -r -- "${_ccm_green}ccmorph:${_ccm_reset}"', self.env())
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('\x1b[38;2;215;119;87mccmorph:', out)
        self.assertNotIn('\x1b[32m', out)

    def test_theme_source_under_ksh_arrays_matches_default_issue_408(self):
        # Sourcing initialises the theme with the caller's own options. Under
        # KSH_ARRAYS the palette walk builds the roles from values, not keys:
        # six _ccm_* colours never exist and a stray _ccm_999999 does (#408).
        # Both branches walk the same palette, so both sets must match.
        script = r"""
theme_names() {
  # Name collection itself needs native arrays: "${(@k)parameters}" is the
  # exact expansion a caller's KSH_ARRAYS breaks.
  setopt localoptions noksharrays
  REPLY=${(j.,.)${(o)${(M)${(k)parameters}:#_ccm_*}}}
}
lib=$1
theme_names; styled=$REPLY
[[ "$styled" == *_ccm_accent* ]] || { print -r -- "MISSINGACCENT<$styled>"; exit 3; }
unset ${(M)${(k)parameters}:#_ccm_*}
(
  setopt ksharrays
  source "$lib"
  setopt noksharrays
  theme_names
  [[ "$REPLY" == "$styled" ]] || { print -r -- "KSHSET<$REPLY>DEFAULT<$styled>"; exit 3; }
) || exit 3
NO_COLOR=1
source "$lib"
theme_names; plain=$REPLY
[[ "$plain" == *_ccm_accent* ]] || { print -r -- "MISSINGACCENT<$plain>"; exit 3; }
unset ${(M)${(k)parameters}:#_ccm_*}
(
  setopt ksharrays
  source "$lib"
  setopt noksharrays
  theme_names
  [[ "$REPLY" == "$plain" ]] || { print -r -- "PLAINKSHSET<$REPLY>DEFAULT<$plain>"; exit 3; }
) || exit 3
print -r -- THEMESETS=OK
"""
        rc, out, missed = run_pty(script, self.env(), library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('THEMESETS=OK', out)

    def test_claude_palette_is_shared_by_native_and_search(self):
        env = {**self.env(fzf=True), 'ROWS': 'first\tFirst\tDescription\nsecond\tSecond\tDetails'}
        self.spec('Theme=first')
        rc, out, missed = run_pty('ccm_ui_select native Theme "$ROWS"', env,
                                  [(b'Esc Cancel', b'\x1b')],
                                  library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertIn('\x1b[38;2;215;119;87m\x1b[1mTheme', out)
        self.assertIn('\x1b[38;2;177;185;249m❯', out)
        self.assertIn('\x1b[38;2;153;153;153mDescription', out)
        for retired in ('◆', '◇', '●', '○', '│', '└', '\x1b[36m'):
            self.assertNotIn(retired, out)
        rc, out, missed = run_pty('ccm_ui_select search Theme "$ROWS"', env,
                                  library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertEqual(rc, 0, out)
        log = (self.tmp / 'fzf.log').read_text()
        self.assertIn('prompt:#d77757', log)
        self.assertIn('pointer:#b1b9f9', log)
        self.assertIn('header:#999999', log)

    def test_shared_input_escape_cancels_text_and_secret(self):
        for mode in ('text', 'secret'):
            for settings in ({}, {'NO_COLOR': '1'}, {'TERM': 'dumb'}):
                with self.subTest(mode=mode, settings=settings):
                    env = {**self.env(), **settings}
                    rc, out, missed = run_pty(
                        'ccm_ui_input "Value: " ' + mode + '; result=$?; '
                        'print -r -- "VALUE=<$REPLY>"; exit $result', env,
                        [(b'Value:', b'partial-value\x1b'), (b'VALUE=', b'')],
                        library=ROOT / 'lib/ccmorph-ui.zsh')
                    self.assertIsNone(missed, out)
                    self.assertNotEqual(rc, 0, out)
                    self.assertIn('Esc Cancel', out)
                    self.assertIn('Cancelled', out)
                    self.assertIn('VALUE=<>', out)
                    if mode == 'secret':
                        self.assertNotIn('partial-value', out)

    def test_shared_input_preserves_entry_and_editing(self):
        for keys, expected in ((b'abc\x7fd\r', 'abd'),
                               (b'discard\x15keep\r', 'keep'),
                               (b'one two   \x17three\r', 'one three'),
                               ('中文\x7f字\r'.encode(), '中字'),
                               (b'abc\x1b[Dd\r', 'abcd'),
                               (b'\r', '')):
            with self.subTest(keys=keys):
                rc, out, missed = run_pty(
                    'ccm_ui_input "Value: " || exit $?; print -r -- "VALUE=<$REPLY>"',
                    self.env(), [(b'Value:', keys)],
                    library=ROOT / 'lib/ccmorph-ui.zsh')
                self.assertIsNone(missed, out)
                self.assertEqual(rc, 0, out)
                self.assertIn('VALUE=<' + expected + '>', out)

    def test_shared_input_fits_its_heading_under_a_callers_ksh_arrays(self):
        # ccm_ui_input reads the terminal through _ccm_ui_size, which indexes the
        # `stty size` pair from 1 like the rest of the library. A caller that had
        # `setopt ksharrays` (index 0 is the first element) made that read one
        # field over and the heading was fitted to the 80-column fallback on a
        # 100-column terminal. It is the only path into _ccm_ui_size without an
        # `emulate -L zsh` above it -- ccm_ui_list and _ccm_ui_native set it, and
        # _ccm_ui_search, whose _ccm_ui_receipt calls it too, is reached only from
        # ccm_ui_select, which sets it -- so the guard belongs on this function.
        prompt = 'P' * 90 + ': '
        rc, out, missed = run_pty(
            'setopt ksharrays; ccm_ui_input "' + prompt + '" || exit $?; '
            'print -r -- "VALUE=<$REPLY>"; [[ -o ksharrays ]] && print KSH_KEPT',
            self.env(), [(b'Esc Cancel', b'abc\r')], size=(24, 100),
            library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('P' * 90, out)
        self.assertNotIn('…', out)
        self.assertIn('VALUE=<abc>', out)
        self.assertIn('KSH_KEPT', out)

    def test_installed_connection_input_cancel_preserves_profile(self):
        env = self.env(fzf=True)
        self.profile()
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        before = (self.state / 'demo.conf').read_bytes(), self.native()
        # Cold installed-CLI startup shares the helper budget, not the UI's Esc latency.
        for keys in (b'\x1b', b'https://not-saved.example\x1b', b'\x03'):
            with self.subTest(keys=keys):
                rc, out, missed = run_pty('exec "$CLI_TARGET" config demo', env, [
                    (b'Esc Cancel', b'\x1b[B\r'),
                    (b'Endpoint [', keys),
                ])
                self.assertIsNone(missed, out)
                self.assertNotEqual(rc, 0, out)
                self.assertIn('Cancelled', out)
                self.assertNotIn('Traceback', out)
                self.assertEqual(((self.state / 'demo.conf').read_bytes(), self.native()), before)
        # Cancelling the credential after editing the URL must discard both.
        rc, out, missed = run_pty('exec "$CLI_TARGET" config demo', env, [
            (b'Esc Cancel', b'\x1b[B\r'),
            (b'Endpoint [', b'https://not-saved.example\n'),
            (b'Credential (hidden;', b'not-saved-secret\x1b'),
        ])
        self.assertIsNone(missed, out)
        self.assertNotEqual(rc, 0, out)
        self.assertNotIn('not-saved-secret', out)
        self.assertEqual(((self.state / 'demo.conf').read_bytes(), self.native()), before)

    def test_standalone_confirmation_defaults_to_no_and_cancels(self):
        for keys, status in ((b'\r', 1), (b'\x1b', 2), (b'\x03', 2)):
            with self.subTest(keys=keys):
                rc, out, missed = run_pty('ccm_ui_confirm "Remove?"', self.env(fzf=True),
                                          [(b'Esc Cancel', keys)],
                                          library=ROOT / 'lib/ccmorph-ui.zsh')
                self.assertIsNone(missed, out)
                self.assertEqual(rc, status, out)
                self.assertIn('❯', out)
                self.assertIn('No', out)
                self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_ui_rejects_control_characters_in_ids(self):
        env = {**self.env(), 'ROWS': 'bad\x1b[2J\tLabel'}
        rc, out, missed = run_pty('ccm_ui_select native Choose "$ROWS"', env,
                                  library=ROOT / 'lib/ccmorph-ui.zsh')
        self.assertEqual(rc, 2, out)
        self.assertNotIn('\x1b[2J', out)
        self.assertIn('invalid choice ID', out)

    def test_pty_preserves_output_when_reader_starts_after_child(self):
        env = {**self.env(), 'ROWS': 'bad\x1b[2J\tLabel'}
        drain = _drain

        def read_after_child(master, out, deadline, process):
            # On macOS the last slave close discards unread terminal output; a
            # retained slave holds the child's exit until that output is read.
            # Waiting for the child to finish is the case under test: a run_pty
            # that dropped its slave lets the child exit at once (~0.7 s) and
            # the drain below then reads nothing, while a correct one keeps the
            # child waiting here. The wait takes a fraction of the bound so the
            # drain keeps a share of it; a child still running when it expires
            # is the correct case, not a hang.
            try:
                process.wait(timeout=max(0.1, (deadline - time.monotonic()) / 10))
            except subprocess.TimeoutExpired:
                pass
            return drain(master, out, deadline, process)

        for command, status, expected in (
                ('ccm_ui_select native Choose "$ROWS"', 2, 'invalid choice ID'),
                ('print -r -- stdout-marker; print -ru2 -- stderr-marker', 0,
                 'stderr-marker')):
            with self.subTest(command=command), \
                    patch(__name__ + '._drain', side_effect=read_after_child):
                rc, out, missed = run_pty(command, env, library=ROOT / 'lib/ccmorph-ui.zsh')
                self.assertIsNone(missed, out)
                self.assertEqual(rc, status, out)
                self.assertIn(expected, out)
                self.assertNotIn('\x1b[2J', out)
                if status == 0:
                    self.assertIn('stdout-marker', out)

    def test_pty_cleanup_reports_a_leader_that_exited_as_the_wait_ran_out_issue_430(self):
        if sys.platform != 'darwin':
            self.skipTest('only macOS refuses to signal a process group whose only member has exited')

        def drain_until_the_leader_is_a_zombie(master, out, deadline, process):
            # The cleanup's wait runs out because the leader exited between the
            # pump and it, and nothing has reaped that exit yet. `_drain` would
            # reap it, hiding the race, so wait for the exit instead: a zero
            # signal asks whether the group is still signallable, and macOS
            # answers EPERM for a group whose only member has exited -- the
            # answer the cleanup's own killpg then lands on. Nothing runs between
            # this answer and that call, so the group it meets is this one. The
            # exit takes milliseconds, so the probe stops at half of pytest.ini's
            # per-test cap: a platform that never refuses meets the assertion
            # below, not a capped-out test with a thread dump, while a caller who
            # set a shorter bound keeps the bound it asked for.
            stop = time.monotonic() + min(deadline - time.monotonic(), 30)
            while time.monotonic() < stop:
                try:
                    os.killpg(process.pid, 0)
                except PermissionError:
                    return
                time.sleep(0.02)
            raise AssertionError('the platform never refused the exited leader its group')

        with patch(__name__ + '._drain', side_effect=drain_until_the_leader_is_a_zombie), \
                patch.object(subprocess.Popen, 'wait', _wait_that_times_out_once()):
            rc, out, missed = run_pty('exit 7', self.env())
        self.assertEqual(missed, b'process did not exit', out)
        self.assertEqual(rc, 7, out)

    def test_pty_cleanup_survives_a_refused_kill_without_macos_issue_430(self):
        # The case above is macOS's own refusal, so it cannot run in the Linux
        # Hive VM; this one covers the same handler on the platforms that never
        # refuse a group, by faking every spelling a gone group comes in -- the
        # handler's widening is only as good as the second one. The child exits
        # on its own and the reap that follows the refused kill is real, which
        # is what the assertions read.
        for refusal in (PermissionError, ProcessLookupError):
            with self.subTest(refusal=refusal.__name__), \
                    patch.object(os, 'killpg', side_effect=refusal), \
                    patch.object(subprocess.Popen, 'wait', _wait_that_times_out_once()):
                rc, out, missed = run_pty('exit 7', self.env())
                self.assertEqual(missed, b'process did not exit', out)
                self.assertEqual(rc, 7, out)

    def test_pty_cleanup_still_kills_a_child_that_did_not_exit_issue_430(self):
        def drain_while_the_child_holds_its_terminal(master, out, deadline, process):
            # The child is still running when the cleanup's wait runs out, so the
            # killpg meets a live session: the case the handler for the exited
            # leader must not swallow. It announces itself first, so the kill
            # cannot land on a session that does not exist yet.
            if _pump(master, out, 0, b'holding-the-terminal', deadline, process) is None:
                raise AssertionError('the child never reached its read')

        with patch(__name__ + '._drain', side_effect=drain_while_the_child_holds_its_terminal), \
                patch.object(subprocess.Popen, 'wait', _wait_that_times_out_once()):
            rc, out, missed = run_pty('print -r -- holding-the-terminal; read -r line', self.env())
        self.assertEqual(missed, b'process did not exit', out)
        self.assertEqual(rc, -signal.SIGKILL, out)

    def test_installed_native_and_real_fzf_share_the_module(self):
        real_fzf = shutil.which('fzf')
        if not real_fzf:
            self.skipTest('real fzf not installed')
        env = self.env(fzf=True, curl='fail')
        (self.tmp / 'bin/fzf').unlink()
        env['PATH'] = str(Path(real_fzf).parent) + os.pathsep + env['PATH']
        # Global preferences cannot inject extra output lines or switch to multi.
        env['FZF_DEFAULT_OPTS'] = '--multi --print-query --expect=enter'
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        self.profile()
        before = (self.state / 'demo.conf').read_bytes(), self.native()
        rc, out, missed = run_pty('exec "$CLI_TARGET" config demo', env, [
            (b'Esc Cancel', b'\r'),          # shared native settings menu
            (b'Type to filter', b'\x1b'),  # real fzf even without a model list
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertIn('\x1b[38;2;215;119;87m', out)
        self.assertIn('❯', out)
        self.assertNotIn('◆', out)
        self.assertIn('Default model', out)
        self.assertEqual(((self.state / 'demo.conf').read_bytes(), self.native()), before)

    def test_subscription_model_picker_uses_fzf(self):
        env = self.env(fzf=True)
        (self.state / 'subscription.conf').write_text('type=sub\nemail=demo@example.com\n')
        self.spec('Model — subscription=sonnet')
        rc, out, missed = run_pty('_ccm_pick_value model subscription', env)
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertTrue(out.rstrip().endswith('sonnet'), out)
        self.assertIn('--prompt Model — subscription>', (self.tmp / 'fzf.log').read_text())

    def test_value_rows_follow_the_profile_type(self):
        self.profile('subdemo')
        (self.state / 'subdemo.conf').write_text('type=sub\nemail=someone@example.com\n')
        self.profile('provdemo')
        (self.state / 'provdemo.conf').write_text('type=provider\nprovider=mockprov\n')
        (self.state / 'profiles/provdemo/.claude.json').write_text(
            json.dumps({'additionalModelOptionsCache': [{'value': 'saved-provider-model'}]}))
        env = self.env()
        rc, out, _ = run_pty('_ccm_value_rows model subdemo', env)
        self.assertEqual(rc, 0, out)
        for alias in ('fable', 'opus', 'sonnet', 'haiku'):
            self.assertIn(alias, out)
        rc, out, _ = run_pty('_ccm_value_rows model provdemo', env)
        self.assertEqual(rc, 0, out)
        self.assertIn('saved-provider-model', out)


if __name__ == '__main__':
    unittest.main()