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

from support import HANG_TIMEOUT, path_without

ROOT = Path(__file__).resolve().parents[1]
CCMORPH = ROOT / 'lib/ccmorph.sh'
# The menu primitives under their old zsh names, now answered by the Go UI.
UI_SHIM = ('ccm_ui_select() { ccmorph _ui select "$@"; }\n'
           'ccm_ui_confirm() { ccmorph _ui confirm "$@"; }\n'
           'ccm_ui_input() { local r; r=$(ccmorph _ui input "$@") || return $?; REPLY=$r; }\n'
           'ccm_ui_list() { ccmorph _ui list "$@"; }\n'
           '_ccm_ui_numbered() { ccmorph _ui numbered "$@"; }\n'
           '_ccm_value_rows() { ccmorph _ui rows "$@"; }\n'
           '_ccm_pick_value() { ccmorph _ui pick "$@"; }\n'
           '_ccm_prompt_default() { ccmorph _ui default "$@"; }\n')
MODELS = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                              ('picker-alpha', 'picker-beta')]})

# `-w` marks the reachability probe; it must answer or discovery never runs.
# A numeric CURL_MODE is the HTTP status the endpoint refused with, in curl -f's
# own words, so the listing path meets a refused key without a network.
FAKE_CURL = '''#!/bin/sh
printf '%s\\n' "$*" >> "$CURL_LOG"
[ "${1-}" = -K ] && cat >> "$CURL_LOG"
for arg in "$@"; do
  if [ "$arg" = -w ]; then printf 200; exit 0; fi
done
case "$CURL_MODE" in
  [0-9][0-9][0-9]) printf 'curl: (56) The requested URL returned error: %s\n' "$CURL_MODE" >&2; exit 56 ;;
esac
[ "$CURL_MODE" = ok ] || exit 22
printf '%s\\n' "$MODELS"
'''

# Selects by prompt argv: FZF_SPEC holds `prompt-substring=row-value` lines.
# --version answers before anything is logged: the core probes it to decide
# which flags the installed fzf understands, and that probe is not a selection.
FAKE_FZF = '''#!/bin/sh
if [ "$1" = --version ]; then printf '0.75.0 (fake)\\n'; exit 0; fi
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


def screen(raw, columns=100):
    """The rows a terminal holds after replaying `raw`, the way run_pty emits it.

    Carriage returns, newlines, cursor moves, line/screen erases and autowrap at
    `columns` move and clear the way the child wrote them; SGR and private-mode
    sequences draw nothing, so the rows are what a person reads, color-free.
    Writing past the last row grows the screen; trailing blank rows drop. Wide
    characters count as one cell — the screens these tests read stay ASCII.
    """
    rows, r, c = [''], 0, 0

    def grow(row):
        while len(rows) <= row:
            rows.append('')

    i, n = 0, len(raw)
    while i < n:
        ch = raw[i]
        if ch == '\x1b' and raw.startswith('[', i + 1):
            j = i + 2
            while j < n and not raw[j].isalpha():
                j += 1
            seq, cmd = raw[i + 2:j], raw[j] if j < n else ''
            if cmd == 'A':
                r = max(0, r - (int(seq) if seq.isdigit() else 1))
            elif cmd == 'B':
                r += int(seq) if seq.isdigit() else 1
                grow(r)
            elif cmd == 'J':
                if seq in ('', '0'):  # cursor to end of screen
                    grow(r)
                    rows[r] = rows[r][:c]
                    for k in range(r + 1, len(rows)):
                        rows[k] = ''
            elif cmd == 'K':
                grow(r)
                rows[r] = rows[r][:c]
            elif cmd == 'H':
                parts = (seq or '1;1').split(';')
                r, c = int(parts[0]) - 1, int(parts[1]) - 1 if len(parts) > 1 else 0
                grow(r)
            i = j + 1
            continue
        if ch == '\r':
            c = 0
        elif ch == '\n':
            r, c = r + 1, 0
            grow(r)
        elif ch == '\b':
            c = max(0, c - 1)
        else:
            if c >= columns:  # autowrap, pending at the last column
                r, c = r + 1, 0
                grow(r)
            line = rows[r]
            if c < len(line):
                rows[r] = line[:c] + ch + line[c + 1:]
            else:
                rows[r] = line + ' ' * (c - len(line)) + ch
            c += 1
        i += 1
    while rows and rows[-1] == '':
        rows.pop()
    return rows


def _controlling_terminal():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)


def run_pty(command, env, exchanges=(), size=(24, 100), library=CCMORPH):
    """Run `command` on a pty, answering each (pattern, reply) in turn."""
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', *size, 0, 0))
    input_flags = termios.tcgetattr(slave)[3] & (termios.ECHO | termios.ICANON | termios.ISIG)
    process = subprocess.Popen(
        ['zsh', '-fc', 'source "$1"\n' + UI_SHIM + command, 'ccmorph-picker-test', str(library)],
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
            if callable(reply):
                # Some answers are more than bytes — a signal timed against
                # the sequence it interrupts — and drive the pty themselves.
                reply(master, out)
            elif reply:
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


# Stop a launch at the adapter boundary: a core whose `util adapter-start` reports
# the model it was asked for (on stderr) and fails, all else the real core.
ADAPTER_BOUNDARY = r'''mkdir -p "$HOME/core"
printf '%s\n' '#!/bin/sh' "real='$_CCMORPH_LIBEXEC_DIR/ccmorph-core'" \
  'if [ "$1 $2" = "util adapter-start" ]; then printf "SELECTED=<%s>\n" "$5" >&2; exit 1; fi' \
  'exec "$real" "$@"' > "$HOME/core/ccmorph-core"
chmod +x "$HOME/core/ccmorph-core"
_CCMORPH_LIBEXEC_DIR="$HOME/core"
'''

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
        # Real PATH minus any entry carrying a real fzf or cliproxyapi: `whence
        # -p fzf` must fail for the numbered fallback, and the add menu's
        # provider rows must be the test's own, not the host's (#414), while
        # python3 and zsh still resolve.
        hidden = ('cliproxyapi',) if fzf else ('fzf', 'cliproxyapi')
        entries = [str(self.tmp / 'bin')]
        rest = path_without(*hidden)
        if rest:
            entries.extend(rest.split(os.pathsep))
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

    def bridge(self, *providers):
        """A fake cliproxyapi whose --help lists exactly these logins."""
        logins = ' '.join(f"'-{p}-login'" for p in providers)
        path = self.tmp / 'bin' / 'cliproxyapi'
        path.write_text(f"#!/bin/sh\nprintf '%s\\n' {logins}\n")
        path.chmod(0o755)

    def field(self, name, key):
        lines = (self.state / f'{name}.conf').read_text().splitlines()
        values = [line.split('=', 1)[1] for line in lines if line.startswith(key + '=')]
        return values[-1] if values else None

    def native(self, name='demo'):
        return (self.state / 'profiles' / name / 'settings.json').read_bytes()

    def first_menu(self):
        """The first fzf call's argv and offered rows, from the fzf log."""
        lines = (self.tmp / 'fzf.log').read_text().splitlines()
        start = next(i for i, line in enumerate(lines) if line.startswith('ARGS '))
        args, rows, in_rows = lines[start], [], False
        for line in lines[start + 1:]:
            if line.startswith('ARGS '):
                break
            if line.startswith('ROWS '):
                rows.append(line[5:])
                in_rows = True
            elif in_rows:
                rows.append(line)
        return args, rows

    def test_a_test_env_hides_the_hosts_fzf_and_bridge(self):
        """#414: the add menu reads its provider rows from the cliproxyapi on
        PATH, and the real Kimi names widen its label column, so the padding
        the two add tests pin held only on a machine without the bridge. Each
        env now hides both tools the suite stubs, and a test that wants either
        writes its own."""
        for fzf in (False, True):
            env = self.env(fzf=fzf)
            self.assertIsNone(shutil.which('cliproxyapi', path=env['PATH']), env['PATH'])
            if fzf:
                self.assertEqual(Path(shutil.which('fzf', path=env['PATH'])).parent,
                                 self.tmp / 'bin')
            else:
                self.assertIsNone(shutil.which('fzf', path=env['PATH']), env['PATH'])

    def test_cold_launch_selects_a_model_before_starting_the_adapter(self):
        env = self.env(fzf=True)
        self.profile()
        (self.state / 'profiles/demo/settings.json').write_text('{}')
        # Stop at the real launcher's adapter boundary: no client or inference.
        command = ADAPTER_BOUNDARY + 'ccmorph run demo'
        self.spec('=picker-beta')
        rc, out, missed = run_pty(command, env)
        self.assertIsNone(missed, out)
        self.assertIn('SELECTED=<picker-beta>', out)
        self.assertIsNone(self.field('demo', 'model'))  # invocation-only choice
        self.assertIn('ccmorph set demo model picker-beta', out)  # told how to keep it (#360)
        self.assertNotIn('old-secret', out)

        self.spec('=CANCEL')
        rc, out, missed = run_pty(command, env)
        self.assertNotEqual(rc, 0, out)
        self.assertNotIn('SELECTED=', out)  # cancellation must not start anything

    def test_automatic_picker_preserves_explicit_saved_and_headless_choices(self):
        env = self.env(fzf=True)
        self.profile()
        (self.state / 'profiles/demo/settings.json').write_text('{}')
        command = ADAPTER_BOUNDARY + 'ccmorph run demo'
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
        self.assertIn('SELECTED=<>', result.stderr)
        with (self.state / 'demo.conf').open('a') as conf:
            conf.write('model=picker-alpha\n')
        rc, out, _ = run_pty(command, env)
        self.assertIn('SELECTED=<picker-alpha>', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.spec('=picker-beta')
        rc, out, _ = run_pty(command + ' -i', env)
        self.assertIn('SELECTED=<picker-beta>', out)
        self.assertEqual(self.field('demo', 'model'), 'picker-alpha')

    def test_launch_picker_opens_on_the_saved_model_issue_377(self):
        """`run -i` must meet the person where `set model -i` does (#377).

        It opened a bare catalogue on the alphabetically first model, ignoring
        the profile's saved one, so Enter alone started Haiku on a profile
        whose default was Sonnet. The saved model comes first, as the Keep row
        `set model -i` opens on (#279)."""
        env = self.env(fzf=True)
        self.profile()
        (self.state / 'profiles/demo/settings.json').write_text('{}')
        with (self.state / 'demo.conf').open('a') as conf:
            conf.write('model=picker-beta\n')
        # No spec line matches this prompt: the fake fzf answers with the first
        # offered row, exactly as Enter alone answers the real one.
        self.spec('no-such-prompt=novalue')
        rc, out, missed = run_pty(ADAPTER_BOUNDARY + 'ccmorph run demo -i', env)
        self.assertIsNone(missed, out)
        self.assertIn('SELECTED=<picker-beta>', out)  # Enter alone kept the saved model
        args, rows = self.first_menu()
        self.assertIn('--prompt Model — demo>', args)
        self.assertEqual(rows[0], '@keep\tKeep picker-beta')  # the saved model marked, first
        self.assertEqual([row.split('\t')[0] for row in rows], ['@keep', 'picker-alpha'])
        # Keeping the default is not a pick to keep: the session-only note (#360)
        # would send a person to save what is already saved.
        self.assertNotIn('this session only', out)

        # The same marking in `set model -i`: the Keep row is the row both share.
        (self.tmp / 'fzf.log').unlink()
        self.spec('Default model=@keep')
        rc, out, missed = run_pty('ccmorph set demo model -i', env)
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        _, set_rows = self.first_menu()
        self.assertEqual(set_rows[0], rows[0])
        self.assertEqual(self.field('demo', 'model'), 'picker-beta')  # Keep kept it

    def test_launch_picker_without_a_saved_model_says_so_issue_377(self):
        """A profile with nothing saved opens without a marker, titled so (#377)."""
        env = self.env(fzf=True)
        self.profile()
        (self.state / 'profiles/demo/settings.json').write_text('{}')
        self.spec('=picker-alpha')
        rc, out, missed = run_pty(ADAPTER_BOUNDARY + 'ccmorph run demo -i', env)
        self.assertIsNone(missed, out)
        self.assertIn('SELECTED=<picker-alpha>', out)
        args, rows = self.first_menu()
        self.assertIn('--prompt Model — demo (none saved)>', args)
        self.assertFalse(any(row.startswith('@keep') for row in rows), rows)
        self.assertEqual([row.split('\t')[0] for row in rows], ['picker-alpha', 'picker-beta'])
        self.assertIn('this session only', out)  # a real pick is still session-only (#360)

    def test_unexported_prelaunch_in_local_sh_runs_the_hook(self):
        env = self.env(fzf=True)
        self.profile()
        mark = self.tmp / 'home' / 'mark'
        mark.write_text('#!/bin/sh\nprintf "%s\\n" "$1" > "$HOME/prelaunch-ran"\n')
        mark.chmod(0o755)
        # local.sh may set the opt-in without `export` (docs/ARCHITECTURE.md).
        (self.state / 'local.sh').write_text('CCMORPH_PRELAUNCH="$HOME/mark"\n')
        command = ADAPTER_BOUNDARY + 'ccmorph run demo --model picker-alpha'
        result = subprocess.run(['zsh', '-fc', 'source "$1"\n' + command, 'test', str(CCMORPH)],
                                env=env, stdin=subprocess.DEVNULL, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        # The boundary proves the launch got past profile setup, where the hook runs.
        self.assertIn('SELECTED=<picker-alpha>', result.stderr)
        ran = self.tmp / 'home' / 'prelaunch-ran'
        self.assertTrue(ran.exists(), result.stdout + result.stderr)
        self.assertEqual(ran.read_text().strip(), 'prelaunch')

    def test_local_sh_output_and_side_effects_stay_out_of_pickers(self):
        env = self.env(fzf=True)
        self.profile()
        self.spec('=picker-beta')
        (self.state / 'local.sh').write_text(
            'print local-sh-noise\nprint ran >> "$HOME/local-sh-runs"\n')
        rc, out, missed = run_pty(ADAPTER_BOUNDARY + 'ccmorph run demo -i', env)
        self.assertIsNone(missed, out)
        # The picker's captured answer is the picked name, not local.sh's echo.
        self.assertIn('SELECTED=<picker-beta>', out)
        # The launching shell sourced local.sh once; the picker shell must not.
        runs = self.tmp / 'home' / 'local-sh-runs'
        self.assertEqual(len(runs.read_text().splitlines()), 1, out)

    def test_headless_picker_answer_stays_clean_when_local_sh_prints(self):
        env = self.env(fzf=True)
        self.profile()
        self.spec('=picker-beta')
        (self.state / 'local.sh').write_text(
            'print local-sh-noise\nprint ran >> "$HOME/local-sh-runs"\n')
        result = subprocess.run(
            ['zsh', '-fc', 'source "$1"\nccmorph context demo -i', 'test', str(CCMORPH)],
            env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn('model id is invalid', result.stderr)
        runs = self.tmp / 'home' / 'local-sh-runs'
        self.assertEqual(len(runs.read_text().splitlines()), 1, result.stdout + result.stderr)

    def test_models_names_a_rejected_key_and_keeps_the_network_for_silence(self):
        self.profile()
        for mode, want in (('401', "rejected the API key for 'demo' (HTTP 401)"),
                           ('403', "rejected the API key for 'demo' (HTTP 403)")):
            with self.subTest(mode=mode):
                result = subprocess.run(
                    ['zsh', '-fc', 'source "$1"\nccmorph models demo', 'test', str(CCMORPH)],
                    env=self.env(curl=mode), stdin=subprocess.DEVNULL,
                    capture_output=True, text=True, timeout=HANG_TIMEOUT)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn(want, result.stderr)
                self.assertIn('replace it with: ccmorph config demo', result.stderr)
                # The endpoint answered: the network is not the problem.
                self.assertNotIn('network', result.stderr)
                # curl's own line never reaches the terminal.
                self.assertNotIn('curl:', result.stderr)
        with self.subTest(mode='fail'):
            result = subprocess.run(
                ['zsh', '-fc', 'source "$1"\nccmorph models demo', 'test', str(CCMORPH)],
                env=self.env(curl='fail'), stdin=subprocess.DEVNULL,
                capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn('could not reach', result.stderr)
            self.assertNotIn('curl:', result.stderr)

    def test_configure_picker_offers_endpoint_models_and_saves_the_pick(self):
        env = self.env(fzf=True)
        self.profile()
        before = self.native()
        self.spec('Default model=picker-beta', 'Default effort=xhigh')
        rc, out, missed = run_pty('ccmorph config -i', env, [(b'Settings', b'\r'), (b'Default effort', b'\x1b[B' * 3 + b'\r')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('Model and effort', out)  # one word names the setting (#367)
        log = (self.tmp / 'fzf.log').read_text()
        self.assertIn('--prompt Default model — demo>', log)
        self.assertIn('picker-beta', log)  # the endpoint's own listing reached the picker
        self.assertEqual(self.field('demo', 'model'), 'picker-beta')
        self.assertEqual(self.field('demo', 'effort'), 'xhigh')
        self.assertEqual(self.native(), before)
        self.assertNotIn('Save changes?', out)
        self.assertNotIn('Endpoint [', out)
        self.assertNotIn('Replace stored credential?', out)
        # The answered pickers agree on one summary shape (#361): the settings
        # menu and the effort menu each fold to "Title › choice".
        flat = '\n'.join(screen(out))
        self.assertIn('  Settings — demo › Model and effort', flat, out)
        self.assertIn('  Default effort — demo › Extra high', flat, out)

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
            (b'Select [1-2]', b'abc\n'),                                # matches no row
            # Each answer waits for the prompt it answers, not the reprompt
            # message before it: typed ahead of the raw-mode prompt, the line
            # echoes into the screen the fold erases.
            (b'Please choose a number or part of a name.', b''),
            (b'Select [1-2]', b'9\n'),                                  # out of range, matches nothing
            (b'Please choose a number or part of a name.', b''),
            (b'Select [1-2]', b'2\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('  1) alpha    \x1b[38;2;153;153;153mfirst choice', out)
        self.assertTrue(out.rstrip().endswith('beta'), out)
        # The answered list folds (#361): the rows, the Select prompt, its hint
        # and the two reprompts leave the one summary line, and stdout after it.
        self.assertEqual(screen(out), ['  pick one › beta', 'beta'], out)

    def test_numbered_fallback_takes_a_typed_id_or_part_issue_387(self):
        """#387: without fzf the numbered menu answered only row numbers, so a
        260-model gateway could not be answered with the model's own name — the
        person had to count rows. The same loop now takes the id or part of it,
        and a part that matches several rows narrows the list to those. The
        piped stdin drives the same matching loop a terminal does."""
        env = self.env(fzf=False)
        env['ROWS'] = '\n'.join(['@keep\tKeep native-model']
                                + [f'openai/model-{n}-instruct\topenai/model-{n}-instruct\tfake'
                                   for n in range(1, 261)]
                                + ['@custom\tEnter a custom model ID…'])

        def number(*answers):
            return subprocess.run(
                ['zsh', '-fc', 'source "$1"\n' + UI_SHIM
                 + '_ccm_ui_numbered "Default model — many" "$ROWS" labels',
                 'numbered-387', str(CCMORPH)],
                env=env, input=''.join(a + '\n' for a in answers),
                capture_output=True, text=True, timeout=HANG_TIMEOUT)

        # The whole id — the answer the issue's person typed and was refused.
        r = number('openai/model-137-instruct')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(r.stdout, 'openai/model-137-instruct\n')
        self.assertNotIn('Please choose', r.stderr)
        # Part of one, unique among the rows: no counting, no narrowing.
        r = number('model-137')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(r.stdout, 'openai/model-137-instruct\n')
        # A part that matches several rows narrows the list to those, renumbered.
        r = number('model-13', '1')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(r.stdout, 'openai/model-13-instruct\n')
        self.assertIn('Select [1-11]', r.stderr)  # 13 and 130 through 139
        # A number still answers the row it names.
        r = number('2')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(r.stdout, 'openai/model-1-instruct\n')
        # What matches nothing keeps the list and asks again, naming both ways.
        r = number('no-such-model', 'q')
        self.assertNotEqual(r.returncode, 0)
        self.assertIn('Please choose a number or part of a name.', r.stderr)
        self.assertIn('Cancelled', r.stderr)

    def test_numbered_fallback_fits_the_screen_and_names_the_fzf_step_issue_387(self):
        """#387: a 260-model gateway on a 24-line terminal pushed the title and
        the first rows out of sight before the person could answer, and nothing
        on screen said fzf would add search. The list now fits the screen with a
        "N more" line and the one fzf hint, and a typed id reaches a model no
        visible row names."""
        env = self.env(fzf=False)
        env['MODELS'] = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                                             [f'openai/model-{n}-instruct' for n in range(1, 261)]]})
        self.profile('many', 'https://gw.example', 'gw-secret')
        rc, out, missed = run_pty('ccmorph set many model -i', env, [
            (b'Select [1-262]', b'openai/model-137-instruct\n'),
        ], size=(24, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        # The screen the person answers on: the title and the first rows are
        # still in view, the cut is named, and the hint says what fzf adds.
        pos = out.find('Select [1-262]') + len('Select [1-262]')
        rows = screen(out[:pos], columns=80)
        self.assertLessEqual(len(rows), 24, rows)
        self.assertIn('  Default model — many', rows)
        self.assertTrue(any(row.startswith('   2) openai/model-1-instruct ') for row in rows), rows)  # the catalogue starts in view
        self.assertTrue(any('↓ 245 more' in row for row in rows), rows)
        self.assertTrue(any('fzf adds search:' in row for row in rows), rows)
        self.assertFalse(any('openai/model-260-instruct' in row for row in rows), rows)
        # The typed id reaches a model no visible row names, and the answer
        # folds (#361): the receipt, the save, the value.
        self.assertEqual(self.field('many', 'model'), 'openai/model-137-instruct')
        flat = '\n'.join(screen(out, columns=80))
        self.assertIn('  Default model — many › openai/model-137-instruct', flat)
        self.assertIn('Saved many model = openai/model-137-instruct (new sessions)', flat)
        self.assertNotIn('Select [1-', flat)
        self.assertNotIn('fzf adds search', flat)
        self.assertNotIn('↓ 245 more', flat)

    def test_numbered_fallback_narrows_a_typed_part_issue_387(self):
        """#387: a part that matches several rows narrows the list to those,
        renumbered, so the second answer is short — and the fold still ends one
        receipt line."""
        env = self.env(fzf=False)
        env['MODELS'] = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                                             [f'openai/model-{n}-instruct' for n in range(1, 261)]]})
        self.profile('many', 'https://gw.example', 'gw-secret')
        rc, out, missed = run_pty('ccmorph set many model -i', env, [
            (b'Select [1-262]', b'model-1\n'),      # 1, 10-19, 100-199: 111 rows
            (b'Select [1-111]', b'model-137\n'),
        ], size=(24, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        pos = out.find('Select [1-111]') + len('Select [1-111]')
        rows = screen(out[:pos], columns=80)
        self.assertLessEqual(len(rows), 24, rows)
        self.assertIn('  Default model — many', rows)  # the narrowed list reopens on the title
        self.assertTrue(any('↓ 94 more' in row for row in rows), rows)
        self.assertFalse(any('openai/model-2-instruct' in row for row in rows), rows)
        self.assertEqual(self.field('many', 'model'), 'openai/model-137-instruct')
        flat = '\n'.join(screen(out, columns=80))
        self.assertIn('  Default model — many › openai/model-137-instruct', flat)
        self.assertIn('Saved many model = openai/model-137-instruct (new sessions)', flat)
        self.assertNotIn('Select [1-', flat)

    def test_numbered_fallback_keeps_the_screen_across_a_retry_issue_387(self):
        """#387: a failed answer used to leave its message and the next question
        below the cut list, so the block grew past a 24-line screen — the title
        and the first rows were gone before a valid choice. The round now folds
        with the list and the message takes the hint's line: the second question
        finds the same 24 rows the first did."""
        env = self.env(fzf=False)
        env['MODELS'] = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                                             [f'openai/model-{n}-instruct' for n in range(1, 261)]]})
        self.profile('many', 'https://gw.example', 'gw-secret')
        rc, out, missed = run_pty('ccmorph set many model -i', env, [
            (b'Select [1-262]', b'999\n'),  # the issue's out-of-range number
            # The answer waits for the prompt it answers, not the message before
            # it: typed ahead of the raw-mode prompt, the line echoes onto the
            # screen the fold erases.
            (b'Please choose a number or part of a name.', b''),
            (b'Select [1-262]', b'2\n'),
        ], size=(24, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        # The question after the refusal: anchored past the message the retry
        # painted, so the receipt the failed round left is not mistaken for it.
        note = out.find('Please choose a number or part of a name.')
        second = out.find('Select [1-262]', note)
        self.assertGreater(note, 0, out)
        self.assertGreater(second, 0, out)
        rows = screen(out[:second + len('Select [1-262]')], columns=80)
        self.assertLessEqual(len(rows), 24, rows)
        self.assertIn('  Default model — many', rows)  # the title survived the retry
        self.assertTrue(any(row.startswith('   2) openai/model-1-instruct ') for row in rows), rows)
        self.assertTrue(any('↓ 245 more' in row for row in rows), rows)
        self.assertTrue(any('Please choose a number or part of a name.' in row for row in rows), rows)
        self.assertFalse(any('fzf adds search' in row for row in rows), rows)  # the message took its line
        self.assertFalse(any('openai/model-260-instruct' in row for row in rows), rows)
        self.assertEqual(self.field('many', 'model'), 'openai/model-1-instruct')
        flat = '\n'.join(screen(out, columns=80))
        self.assertIn('  Default model — many › openai/model-1-instruct', flat)
        self.assertIn('Saved many model = openai/model-1-instruct (new sessions)', flat)
        self.assertNotIn('Select [1-', flat)
        self.assertNotIn('Please choose a number', flat)

    def test_numbered_fallback_cuts_the_list_on_a_dumb_terminal_issue_387(self):
        """#387: a dumb terminal is a screen too. The cut list, its "N more"
        line and the fzf hint painted only where cursor codes were allowed, so
        TERM=dumb still got all 262 rows and no word about fzf. They now paint
        there as well, as plain text."""
        env = self.env(fzf=False)
        env['TERM'] = 'dumb'
        env['MODELS'] = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                                             [f'openai/model-{n}-instruct' for n in range(1, 261)]]})
        self.profile('many', 'https://gw.example', 'gw-secret')
        rc, out, missed = run_pty('ccmorph set many model -i', env, [
            (b'Select [1-262]', b'openai/model-137-instruct\n'),
        ], size=(24, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotIn('\x1b[', out)  # a dumb screen sees no cursor codes at all
        rows = screen(out[:out.find('Select [1-262]') + len('Select [1-262]')], columns=80)
        self.assertLessEqual(len(rows), 24, rows)
        self.assertIn('  Default model — many', rows)
        self.assertTrue(any(row.startswith('   2) openai/model-1-instruct ') for row in rows), rows)
        self.assertTrue(any('↓ 245 more' in row for row in rows), rows)
        self.assertTrue(any('fzf adds search:' in row for row in rows), rows)
        self.assertFalse(any('openai/model-260-instruct' in row for row in rows), rows)
        self.assertEqual(self.field('many', 'model'), 'openai/model-137-instruct')
        self.assertIn('Saved many model = openai/model-137-instruct (new sessions)', out)

    def test_numbered_fallback_keeps_the_screen_across_retries_on_a_dumb_terminal_issue_387(self):
        """#387: a dumb terminal cannot erase, so each failed answer left its
        message and the next question under the cut list: two misses on a
        24-line screen pushed the title out of sight before a valid choice.
        A miss now repaints the cut list, the message in the hint's line, so
        the screen the third question waits on opens on the title again."""
        env = self.env(fzf=False)
        env['TERM'] = 'dumb'
        env['MODELS'] = json.dumps({'data': [{'id': m, 'owned_by': 'fake'} for m in
                                             [f'openai/model-{n}-instruct' for n in range(1, 261)]]})
        self.profile('many', 'https://gw.example', 'gw-secret')
        rc, out, missed = run_pty('ccmorph set many model -i', env, [
            (b'Select [1-262]', b'999\n'),  # the issue's out-of-range number
            (b'Please choose a number or part of a name.', b''),
            (b'Select [1-262]', b'999\n'),  # and again
            (b'Please choose a number or part of a name.', b''),
            (b'Select [1-262]', b'2\n'),
        ], size=(24, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotIn('\x1b[', out)  # a dumb screen sees no cursor codes at all
        # The third question: a dumb screen only scrolls, so the 24 rows a
        # 24-line terminal shows are the last 24.
        at = -1
        for _ in range(3):
            at = out.find('Select [1-262]', at + 1)
            self.assertGreaterEqual(at, 0, out)
        view = screen(out[:at + len('Select [1-262]')], columns=80)[-24:]
        self.assertIn('  Default model — many', view)  # the title survived both misses
        self.assertTrue(any(row.startswith('   2) openai/model-1-instruct ') for row in view), view)
        self.assertTrue(any('↓ 245 more' in row for row in view), view)
        self.assertTrue(any('Please choose a number or part of a name.' in row for row in view), view)
        self.assertFalse(any('openai/model-260-instruct' in row for row in view), view)
        self.assertEqual(self.field('many', 'model'), 'openai/model-1-instruct')
        self.assertIn('Saved many model = openai/model-1-instruct (new sessions)', out)

    def test_set_without_slot_picks_slot_then_value(self):
        env = self.env()
        self.profile()
        # The value menu is pumped on its full header: the setting menu's own
        # rows name "Effort" too (#367), so the bare word would match them.
        rc, out, missed = run_pty('ccmorph set demo', env, [
            (b'Setting', b'\x1b[B\r'),  # effort
            (b'Effort \xe2\x80\x94 demo', b'\x1b[B' * 2 + b'\r'),  # high
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('Default effort level', out)  # one word names the setting (#367)
        self.assertIn('Medium', out)
        self.assertIn('Extra high (xhigh)', out)  # the label names the value set takes (#367)
        self.assertIn('Max', out)  # gh264: the menu names every level the conf accepts
        self.assertEqual(self.field('demo', 'effort'), 'high')
        self.assertEqual(self.field('demo', 'model'), 'native-model')  # seeded, not guessed

        # Leaving the value picker answers like every picker (issue #280): one
        # Cancelled line and status 1, not a fall-through to a usage error.
        rc, out, missed = run_pty('ccmorph set demo', env, [
            (b'Setting', b'\r'),  # model
            (b'Select [1-', b'\x1b'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertIn('Cancelled', out)
        self.assertNotIn('Use: ccmorph set', out)
        self.assertNotIn('usage:', out)
        self.assertEqual(self.field('demo', 'model'), 'native-model')
        # The leave folds the numbered list away too (#361): one Cancelled line
        # under the slot's receipt, and no list row or Select prompt above it.
        rows = screen(out)
        self.assertEqual(rows[-1], '  Cancelled', out)
        self.assertFalse(any('Select [1-' in row for row in rows), rows)

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

    def test_set_confirms_once_in_a_terminal_and_pipes_the_value_alone_issue_358(self):
        """#358: a terminal saw the Saved sentence and then the value again on a
        line of its own: the sentence went to stderr, the value to stdout, and a
        terminal shows both streams. stdout still carries the value alone for a
        script; on the terminal that shows the sentence, that line folds into it
        (#361), so a person reads one confirming line."""
        env = self.env()
        self.profile()
        # A script capturing set reads the saved value alone; stderr stays empty.
        result = subprocess.run(['zsh', '-fc', 'source "$1"\nccmorph set demo effort high',
                                 'test', str(CCMORPH)], env=env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'high\n', result.stderr)
        self.assertEqual(result.stderr, '', result.stderr)
        # A typed value: the screen holds the one Saved line, no echo below it.
        for key, value in (('effort', 'high'), ('model', 'picker-alpha')):
            with self.subTest(key=key):
                self.profile()
                rc, out, missed = run_pty('ccmorph set demo %s %s' % (key, value), env)
                self.assertIsNone(missed, out)
                self.assertEqual(rc, 0, out)
                self.assertEqual(self.field('demo', key), value, out)
                self.assertEqual(screen(out),
                                 ['Saved demo %s = %s (new sessions)' % (key, value)], out)
        # A value wider than the terminal wraps; the fold takes every row it used.
        with self.subTest(columns=10):
            self.profile()
            rc, out, missed = run_pty('ccmorph set demo model picker-alpha', env, size=(24, 10))
            self.assertIsNone(missed, out)
            self.assertEqual(rc, 0, out)
            self.assertEqual(''.join(screen(out, columns=10)),
                             'Saved demo model = picker-alpha (new sessions)', out)
        # A capture inside a terminal session: the person reads Saved once,
        # and the script still gets the value.
        with self.subTest(stdout='captured'):
            self.profile()
            rc, out, missed = run_pty('v=$(ccmorph set demo effort high); print -r -- "got=<$v>"', env)
            self.assertIsNone(missed, out)
            self.assertEqual(rc, 0, out)
            self.assertEqual(screen(out),
                             ['Saved demo effort = high (new sessions)', 'got=<high>'], out)
        # stderr hidden: no sentence is shown, so the value is the one line.
        with self.subTest(stderr='hidden'):
            self.profile()
            rc, out, missed = run_pty('ccmorph set demo effort high 2>/dev/null', env)
            self.assertIsNone(missed, out)
            self.assertEqual(rc, 0, out)
            self.assertEqual(screen(out), ['high'], out)
        # A dumb terminal cannot fold and sees no cursor codes (#387): the
        # sentence alone, the bare value left out.
        with self.subTest(term='dumb'):
            self.profile()
            rc, out, missed = run_pty('ccmorph set demo effort high', {**env, 'TERM': 'dumb'})
            self.assertIsNone(missed, out)
            self.assertEqual(rc, 0, out)
            self.assertNotIn('\x1b[', out)
            self.assertEqual(screen(out), ['Saved demo effort = high (new sessions)'], out)
            self.assertNotIn('high', [line.strip() for line in out.splitlines()], out)
        # A picked value folds the same way, under the picker's own receipt.
        env = self.env(fzf=True)
        with self.subTest(key='model', picked=True):
            self.profile()
            self.spec('Default model=picker-beta')
            rc, out, missed = run_pty('ccmorph set demo model -i', env)
            self.assertIsNone(missed, out)
            self.assertEqual(rc, 0, out)
            self.assertEqual(self.field('demo', 'model'), 'picker-beta', out)
            rows = screen(out)
            self.assertEqual(rows[-1], 'Saved demo model = picker-beta (new sessions)', out)
            self.assertNotIn('picker-beta', rows, out)
        with self.subTest(key='effort', picked=True):
            self.profile()
            rc, out, missed = run_pty('ccmorph set demo effort -i', env, [(b'Effort', b'\x1b[B\r')])
            self.assertIsNone(missed, out)
            self.assertEqual(rc, 0, out)
            self.assertEqual(self.field('demo', 'effort'), 'medium', out)
            rows = screen(out)
            self.assertEqual(rows[-1], 'Saved demo effort = medium (new sessions)', out)
            self.assertNotIn('medium', rows, out)

    def test_set_model_picker_opens_the_config_model_step_issue_279(self):
        """`set <profile> model -i` and config's model step are one picker.

        They used to differ: the set picker offered a bare catalogue that
        ignored the saved model, while config marked it Keep (#279). Both must
        open the same rows the same way, with the saved model marked."""
        env = self.env(fzf=True)
        env['MODELS'] = json.dumps({'data': [
            {'id': 'picker-alpha', 'owned_by': 'fake'},
            {'id': 'picker-beta', 'display_name': 'Picker Beta'}]})
        self.profile()

        # Effort is a native menu, not fzf: a key answers it, never the spec.
        # Enter takes its first row, the Keep row.
        self.spec('Default model=@keep')
        rc, out, missed = run_pty('ccmorph config demo', env, [
            (b'Settings', b'\r'),
            (b'Default effort', b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        config_args, config_rows = self.first_menu()

        (self.tmp / 'fzf.log').unlink()
        self.spec('Default model=@keep')
        rc, out, missed = run_pty('ccmorph set demo model -i', env)
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        set_args, set_rows = self.first_menu()

        self.assertEqual(set_args, config_args)   # the same way of opening
        self.assertEqual(set_rows, config_rows)   # the same list
        self.assertIn('--prompt Default model — demo>', set_args)
        self.assertTrue(set_rows[0].startswith('@keep\t'), set_rows)
        self.assertIn('Keep native-model', set_rows[0])  # the saved model marked
        self.assertTrue(any('Picker Beta' in row for row in set_rows), set_rows)
        self.assertEqual(self.field('demo', 'model'), 'native-model')  # Keep kept it

        # A catalogue row picked from the new menu still saves.
        self.spec('Default model=picker-beta')
        rc, out, missed = run_pty('ccmorph set demo model -i', env)
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.field('demo', 'model'), 'picker-beta')
        # The model picker leaves the same one summary line as every picker
        # (#361): the row's own label, not fzf's echoed line or its description.
        self.assertIn('  Default model — demo › picker-beta', '\n'.join(screen(out)), out)
        # stdout carries the value alone on its own line; stderr's "Saved …" line
        # and whatever the shell prints after it share the pty.
        self.assertIn('picker-beta', [line.strip() for line in out.splitlines()], out)

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

    def test_config_names_a_rejected_key_before_the_fallback_picker(self):
        env = self.env(fzf=True, curl='401')
        self.profile()
        self.spec('Default model=@keep')
        rc, out, missed = run_pty('ccmorph config demo', env, [
            (b'Settings', b'\r'),
            (b'Default effort', b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        rejected = "rejected the API key for 'demo' (HTTP 401)"
        self.assertIn(rejected, out)
        self.assertIn('replace it with: ccmorph config demo', out)
        # The reason comes before the fallback picker it explains.
        self.assertLess(out.index(rejected), out.index('Model list unavailable'))
        # The endpoint answered: the network is not the problem, and curl's own
        # line never reaches the terminal.
        self.assertNotIn('network', out)
        self.assertNotIn('curl:', out)
        # Keep still means the profile's current model, unchanged.
        self.assertEqual(self.field('demo', 'model'), 'native-model')

    def test_add_uses_native_fixed_menu_even_when_fzf_is_installed(self):
        env = self.env(fzf=True)
        # The provider rows come from the cliproxyapi the core finds on PATH
        # (#414); the env keeps the host's out, so the test seeds its own list.
        self.bridge('claude', 'mockprov')
        self.spec('=CANCEL')  # fzf must not be reached at all
        rc, out, missed = run_pty(
            'ccmorph add demo', env, [(b'Esc Cancel', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertIn('Cancelled', out)  # the leave is answered, not silent
        self.assertIn('\x1b[38;2;215;119;87m\x1b[1mSign-in method — demo\x1b[0m', out)
        self.assertIn('\x1b[38;2;177;185;249mClaude account\x1b[0m    \x1b[38;2;153;153;153mSign in with your browser\x1b[0m', out)
        self.assertIn('Sign in to Mockprov with your browser', out)  # the stub bridge's row
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
        # GH-290: the row must say when a token beats a browser sign-in and what it cannot do.
        self.assertIn('Claude token      \x1b[38;2;153;153;153mFor scripts/CI; quota cannot read plan usage\x1b[0m', out)
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
        # The answered list folds (#361): NO_COLOR governs color, not cursor
        # movement. The only escapes are the two folds: the Select question's
        # (heading, hint, typed line: 3) and the list's (heading, blank, one
        # row, the Select receipt: 4). After the list's fold — its receipt
        # and the whole --help output — no escape at all, as on main.
        folds = list(re.finditer(r'\x1b\[(\d+)A\r\x1b\[J', out))
        self.assertEqual([m.group(1) for m in folds], ['3', '4'], out)
        self.assertNotIn('\x1b[', re.sub(r'\x1b\[\d+A\r\x1b\[J', '', out))
        self.assertNotIn('\x1b[', out[folds[-1].end():])

    def test_add_finishes_without_optional_setup_questions(self):
        env = self.env(fzf=True)
        self.spec('Sign-in method=token')
        rc, out, missed = run_pty('ccmorph add demo', env, [
            (b'Sign-in method', b'\x1b[B\r'),
            (b'Paste token for', b'sk-ant-oat01-picker-echo-fixture-not-real-00000000000\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertNotIn('Configure model', out)
        self.assertNotIn('Save changes?', out)
        self.assertNotIn('sk-ant-oat01-picker-echo-fixture-not-real-00000000000', out)
        self.assertIn('Next: ccmorph run demo', out)
        self.assertIsNone(self.field('demo', 'model'))
        self.assertIsNone(self.field('demo', 'effort'))

    def test_add_apikey_lists_anthropic_first_and_asks_only_for_the_key(self):
        env = self.env(fzf=True)
        self.spec('Provider=anthropic')
        rc, out, missed = run_pty('ccmorph add demo', env, [
            (b'Sign-in method', b'\x1b[B' * 2 + b'\r'),  # API key — Anthropic or a gateway
            (b'API key for demo', b'sk-ant-fixture\n'),  # … — Anthropic (hidden):
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        # The provider list names Anthropic itself, first, ahead of every
        # gateway: the sign-in menu promises "Anthropic or a gateway" (#278).
        rows = (self.tmp / 'fzf.log').read_text().split('ROWS ', 1)[1]
        first = rows.splitlines()[0]
        self.assertEqual(first.split('\t')[0], 'anthropic')
        self.assertIn('Anthropic', first)
        self.assertIn('api.anthropic.com', first)
        # A list that may outgrow its window is reachable end to end, says what
        # is hidden, and grows to fit when the terminal allows it.
        log = (self.tmp / 'fzf.log').read_text()
        for flag in ('--prompt Provider — demo>', '--cycle', '--scrollbar',
                     'home:first,end:last', '--height ~-1'):
            self.assertIn(flag, log)
        # The row asks for the key only, named for its service: no address, no
        # tier mapping. The key is checked against Anthropic's own address only.
        self.assertIn('API key for demo — Anthropic', out)
        self.assertNotIn('Base URL', out)
        self.assertEqual(self.field('demo', 'type'), 'apikey')
        self.assertEqual(self.field('demo', 'api_key'), 'sk-ant-fixture')
        self.assertIsNone(self.field('demo', 'base_url'))
        self.assertNotIn('sk-ant-fixture', out)
        log = (self.tmp / 'curl.log').read_text()
        self.assertIn('https://api.anthropic.com', log)
        self.assertEqual(log.count('https://'), log.count('https://api.anthropic.com'))
        self.assertIn('API key → api.anthropic.com', out)
        self.assertIn('Next: ccmorph run demo', out)
        # The answered steps agree on one summary shape (#361): the sign-in
        # menu, the provider search and the typed key question each leave one
        # line — and no list, footer, hint, typed answer or cut description
        # stays above them.
        flat = '\n'.join(screen(out))
        for row in ('  Sign-in method — demo › API key',
                    '  Provider — demo › Anthropic',
                    '  API key for demo — Anthropic (hidden):'):
            self.assertIn(row, flat, out)
        for residue in ('Enter Submit', '❯', 'Select [', '(q to cancel)', '↑/↓ Move', '…'):
            self.assertNotIn(residue, flat)

    def test_numbered_add_folds_each_answered_step_to_one_line_issue_361(self):
        # The owner's follow-up on #361: with no fzf on PATH the provider
        # question falls back to the numbered list, and an answered list folds
        # like every picker. Each step leaves its one summary line; the rows,
        # the numbered prompt's dead hint and the menu footers do not stay.
        env = self.env()
        rc, out, missed = run_pty('ccmorph add eps', env, [
            (b'Sign-in method', b'\x1b[B' * 2 + b'\r'),  # API key
            (b'Select [1-', b'2\n'),                     # DeepSeek, the first gateway
            (b'API key for eps', b'sk-ds-fixture\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        flat = '\n'.join(screen(out))
        for row in ('  Sign-in method — eps › API key',
                    '  Provider — eps › DeepSeek',
                    '  API key for eps — DeepSeek (hidden):'):
            self.assertIn(row, flat, out)
        for residue in ('Enter Submit', '❯', 'Select [', '(q to cancel)', '↑/↓ Move', '…'):
            self.assertNotIn(residue, flat)
        self.assertNotIn('sk-ds-fixture', out)
        self.assertEqual(self.field('eps', 'type'), 'apikey')
        self.assertEqual(self.field('eps', 'api_key'), 'sk-ds-fixture')
        self.assertEqual(self.field('eps', 'base_url'), 'https://api.deepseek.com/anthropic')
        self.assertIn('Next: ccmorph run eps', out)

    def test_typing_anthropic_ranks_the_anthropic_row_first(self):
        real_fzf = shutil.which('fzf')
        if not real_fzf:
            self.skipTest('fzf is not installed; numbered fallback covered separately')
        env = self.env(fzf=True)
        (self.tmp / 'bin/fzf').unlink()
        env['PATH'] = str(Path(real_fzf).parent) + os.pathsep + env['PATH']
        rc, out, missed = run_pty('ccmorph add demo', env, [
            (b'Sign-in method', b'\x1b[B' * 2 + b'\r'),
            (b'Provider', b'anthropic\r'),  # filter the list, accept the top match
            (b'API key for demo', b'sk-ant-real\n'),  # … — Anthropic (hidden):
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        # The top match of "anthropic" is Anthropic itself, not a gateway whose
        # URL merely contains the word (#278).
        self.assertEqual(self.field('demo', 'api_key'), 'sk-ant-real')
        self.assertIsNone(self.field('demo', 'base_url'))
        self.assertNotIn('sk-ant-real', out)
        self.assertIn('API key → api.anthropic.com', out)
        # The pump matches the prompt's ASCII prefix; this pins its full shape:
        # the provider named once, the answer hidden (#361).
        self.assertIn('API key for demo — Anthropic (hidden):', out)

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
            ('login', ('gateway', 'subscription', 'provider'), ('token',)),
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

    def test_cancelled_pickers_answer_with_one_line_and_status_one_issue_280(self):
        # Leaving a picker answers the same way everywhere: one short Cancelled
        # line and exit status 1 — never silence, never a usage line. The search
        # picker runs fzf without needing a terminal, so this is checked headless.
        env = self.env(fzf=True)
        self.profile()
        cases = [
            ('profile picker of models', 'ccmorph models', 'Profile=CANCEL'),
            ('profile picker of get', 'ccmorph get', 'Profile=CANCEL'),
            ('value picker of set', 'ccmorph set demo model -i', 'Default model=CANCEL'),
        ]
        for label, command, spec in cases:
            with self.subTest(picker=label):
                self.spec(spec)
                result = subprocess.run(
                    ['zsh', '-fc', 'source "$1"\n' + command, 'test', str(CCMORPH)],
                    env=env, stdin=subprocess.DEVNULL, capture_output=True,
                    text=True, timeout=HANG_TIMEOUT)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertNotIn('Use: ccmorph set', result.stderr)
                self.assertNotIn('usage:', result.stderr)
                self.assertEqual(result.stderr.count('Cancelled'), 1, result.stderr)
                self.assertTrue(result.stderr.rstrip('\n').endswith('Cancelled'), result.stderr)

    def test_rm_leftover_confirmation_answers_a_leave_with_one_line_issue_280(self):
        # A profile with private state but no .conf asks before removing it. Esc
        # at that prompt is a leave like any other: the picker's one Cancelled
        # line, status 1, and rm adds no second line of its own.
        env = self.env()
        leftover = self.state / 'profiles' / 'demo'
        leftover.mkdir(parents=True)
        rc, out, missed = run_pty('ccmorph rm demo', env, [(b'Remove these leftovers?', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertEqual(out.count('Cancelled'), 1, out)
        self.assertNotIn('nothing removed', out)
        self.assertTrue(leftover.is_dir())

    def test_rm_leftover_confirmation_without_a_terminal_still_says_nothing_was_removed(self):
        # With no terminal nothing showed the prompt, so rm's own line is the only
        # answer a script gets that the leftovers were kept (the headless refusal
        # of issue #280 keeps its words; only a leave at a terminal drops them).
        env = self.env()
        leftover = self.state / 'profiles' / 'demo'
        leftover.mkdir(parents=True)
        result = subprocess.run(
            ['zsh', '-fc', 'source "$1"\nccmorph rm demo', 'test', str(CCMORPH)],
            env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True,
            timeout=HANG_TIMEOUT)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertEqual(result.stderr.count('cancelled; nothing removed'), 1, result.stderr)
        self.assertTrue(leftover.is_dir())

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
            self.assertIn('Cancelled', out)  # q answers like Esc: one short line
            if 'NO_COLOR' in settings:
                # NO_COLOR keeps folding (#361): color is off, cursor movement
                # is not. The only escapes are the two folds: the Select
                # question's (heading, hint, typed line: 3) and the list's
                # (heading, blank, two rows, the Select receipt: 5).
                self.assertEqual(re.findall(r'\x1b\[(\d+)A\r\x1b\[J', out), ['3', '5'], out)
                self.assertNotIn('\x1b[', re.sub(r'\x1b\[\d+A\r\x1b\[J', '', out))
            else:
                # TERM=dumb keeps the plain path: no escape at all, as on main.
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
        self.assertIn('Cancelled', out)  # a real Esc is answered like any picker

    def test_command_entrypoint_accepts_real_arrow_and_enter_without_extra_questions(self):
        env = self.env(fzf=True)
        self.spec('=CANCEL')
        env.pop('CCMORPH_LIB_DIR', None)
        env['CLI_TARGET'] = os.environ.get('CCMORPH_TEST_CLI', str(ROOT / "bin/ccmorph"))
        if 'CCMORPH_TEST_CLI' in os.environ:
            env['XDG_CONFIG_HOME'] = str(ROOT.parent)
        rc, out, missed = run_pty('exec "$CLI_TARGET" add demo', env, [
            (b'Claude token', b'\x1b[B\r'),
            (b'Paste token for', b'sk-ant-oat01-entrypoint-fixture-not-real-00000000000\n'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.field('demo', 'type'), 'token')
        self.assertEqual(self.field('demo', 'token'), 'sk-ant-oat01-entrypoint-fixture-not-real-00000000000')
        self.assertNotIn('sk-ant-oat01-entrypoint-fixture-not-real-00000000000', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())
        self.assertNotIn('Select [', out)
        self.assertNotIn('Save changes?', out)
        self.assertNotIn('Configure model', out)
        self.assertIn('Next: ccmorph run demo', out)

    def test_native_fixed_keys_restore_terminal_and_never_spawn_fzf(self):
        env = self.env(fzf=True)
        env['ROWS'] = 'alpha\tFirst\tDescription\nbeta\tSecond\tDescription'
        command = 'picked=$(ccm_ui_select native "Native choices" "$ROWS"); rc=$?; print -r -- "RESULT=$picked"; exit $rc'
        # ↑ at the first row and ↓ at the last stay put, as they always did;
        # Home/End put either end one key away (#278) — in every terminal's
        # spelling of them (#388): tmux and screen end the key with ~, rxvt
        # numbers it 7 or 8.
        cases = [(b'\r', 0, 'alpha'), (b'\x1b[A\r', 0, 'alpha'),
                 (b'\x1b[B\r', 0, 'beta'), (b'\x1bOB\r', 0, 'beta'),
                 (b'\x1b[B\x1b[A\r', 0, 'alpha'), (b'\x1b', 1, ''),
                 (b'\x03', 130, ''), (b'\x04', 1, ''),
                 (b'\x1b[B\x1b[B\r', 0, 'beta'),
                 (b'\x1b[F\r', 0, 'beta'), (b'\x1bOF\r', 0, 'beta'),
                 (b'\x1b[F\x1b[H\r', 0, 'alpha'),
                 (b'\x1b[4~\r', 0, 'beta'), (b'\x1b[B\x1b[1~\r', 0, 'alpha'),
                 (b'\x1b[8~\r', 0, 'beta'), (b'\x1b[B\x1b[7~\r', 0, 'alpha')]
        for keys, expected_rc, value in cases:
            with self.subTest(keys=keys):
                rc, out, missed = run_pty(command, env, [(b'Esc Cancel', keys)])
                self.assertIsNone(missed, out)  # includes echo/canonical/signal restoration
                self.assertEqual(rc, expected_rc, out)
                self.assertIn('RESULT=' + value, out)
                self.assertIn('\x1b[?25h', out)
                self.assertIn('\x1b[?7h', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_native_sigterm_mid_sequence_ends_the_menu(self):
        # A leave that arrives while the reader waits for the final ~ of
        # ESC [ 1 must end the menu with its own exit status: the tail's
        # reader once consumed the signal and threw it away, so the menu went
        # on waiting for the rest of a key its sender had already interrupted
        # (#388).
        env = self.env(fzf=True)
        env['ROWS'] = 'alpha\tFirst\tDescription\nbeta\tSecond\tDescription'
        # The picker runs as a background job so the shell can name its pid:
        # _ccm_main_exec execs the core (the public ccmorph path waits on it
        # through command env), so $! is the core itself. An async list's
        # stdin is /dev/null by default, so the terminal fd is saved and duped
        # back onto the job.
        command = ('exec {tty}<&0\n'
                   '_ccm_main_exec _ui select native "Native choices" "$ROWS" <&$tty & picker=$!\n'
                   'exec {tty}<&-\n'
                   'print -r -- "PICKER=$picker"\n'
                   'wait $picker; rc=$?\n'
                   'print -r -- "RESULT="\n'
                   'exit $rc')

        def stab(master, out):
            # The shell prints PICKER= before the core can draw; still, read
            # on until the whole line is in rather than trust that order.
            deadline = time.monotonic() + HANG_TIMEOUT
            found = re.search(rb'PICKER=(\d+)\r?\n', bytes(out))
            while found is None and time.monotonic() < deadline:
                if select.select([master], [], [], .25)[0]:
                    out.extend(os.read(master, 65536))
                found = re.search(rb'PICKER=(\d+)\r?\n', bytes(out))
            pid = int(found.group(1))
            # tmux's Home cut before its final ~, in one write: the menu has
            # read ESC [ 1 and waits for the ~ when the leave arrives. Every
            # byte is in before the signal, so no pause decides the outcome:
            # the leave lands inside the tail's 80ms window, which must hand
            # it back, or after it, where the menu's own wait takes it, and
            # both end with 143. A pause between ESC [ and its digit could
            # outlast the window on a slow runner, and an empty tail is Esc's
            # own leave (rc 1).
            os.write(master, b'\x1b[1')
            time.sleep(0.01)  # lets the reader reach the tail; 143 never depends on it
            os.kill(pid, signal.SIGTERM)

        rc, out, missed = run_pty(command, env, [(b'Esc Cancel', stab), (b'RESULT=', b'')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 143, out)
        self.assertIn('RESULT=', out)
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
        # A window that cuts rows says so at each edge, in rows, not just by
        # stopping (#278): the list opens announcing what follows below it...
        self.assertIn('↓ 9 more', out)
        # ...and once it reaches the bottom, what sits above.
        self.assertIn('↑ 9 more', out)
        self.assertTrue(all(int(n) <= 7 for n in re.findall(r'\x1b\[(\d+)A', out)), out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_native_fixed_menu_in_a_tiny_terminal_still_says_the_list_is_cut(self):
        env = self.env(fzf=True)
        env['ROWS'] = '\n'.join(f'{i}\tOption {i}' for i in range(10))
        rc, out, missed = run_pty(
            'picked=$(ccm_ui_select native "Native choices" "$ROWS") || exit $?; print -r -- "RESULT=$picked"',
            env, [(b'Esc Cancel', b'\x1b[F\r')], size=(6, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('RESULT=9', out)
        # Too short for the "N more" edge lines, the footer carries the position
        # in the whole list instead (#278).
        self.assertIn('1/10', out)
        self.assertIn('10/10', out)

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
        # A fake cliproxyapi: its --help lists the logins; a login names its id.
        fake = self.tmp / 'bin' / 'cliproxyapi'
        fake.write_text('#!/bin/sh\n'
                        'if [ "$1" = --help ]; then for p in claude antigravity codex devin kimi-ai kimi meta xai; do'
                        ' echo "  -$p-login"; done; exit 0; fi\n'
                        'p=${1#-}; echo "PROVIDER_ID=${p%-login}"; exit 1\n')
        fake.chmod(0o755)
        command = 'ccmorph add my_profile'
        rc, out, missed = run_pty(command, env, [
            (b'Esc Cancel', b'\x1b[B' * 9 + b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)  # stop before authentication or saving
        self.assertIn('Sign-in method — my_profile', out)
        # GH-366: seven logins, seven names — the two Kimi rows carry the
        # region names the provider list uses, not a bare Kimi and a domain.
        for label in ('Antigravity', 'Codex', 'Devin', 'Kimi For Coding (global)',
                      'Kimi For Coding (China)', 'Meta', 'xAI'):
            self.assertIn(label, out)
        self.assertNotIn('Kimi-ai', out)
        self.assertIn('\x1b[38;2;153;153;153mSign in with your browser', out)
        self.assertIn('\x1b[38;2;153;153;153mFor scripts/CI; quota cannot read plan usage', out)
        # Each row says which product or plan it signs in to (issue #283) while
        # the rendered row keeps its browser-and-bridge explanation: the two
        # Kimi logins are kimi.ai and kimi.com, not the same text twice.
        plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', out)
        self.assertIn('kimi.ai account', plain)
        self.assertIn('kimi.com account', plain)
        self.assertIn('ChatGPT (Codex)', plain)
        self.assertIn('Browser sign-in to xAI (Grok), through cliproxyapi', out)
        self.assertIn('\x1b[38;2;153;153;153mBrowser sign-in to a kimi.ai account', out)
        self.assertIn('PROVIDER_ID=xai', out)
        self.assertNotIn('PROVIDER_ID=xAI', out)
        self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_add_without_cliproxyapi_names_the_browser_logins_and_the_install_step(self):
        # Issue #283: without the optional cliproxyapi the menu used to stop at
        # three rows, so nobody learned the browser logins exist or what turns
        # them on. The menu now carries a row saying both; nothing is saved.
        # The step the answer names is the machine's own (#369), so the brew
        # this test wants is a stub on the test PATH, not the host's.
        env = self.env(fzf=True)
        entries = [e for e in env['PATH'].split(os.pathsep)
                   if not (Path(e) / 'cliproxyapi').exists()]
        env['PATH'] = os.pathsep.join(entries)
        self.assertFalse(shutil.which('cliproxyapi', path=env['PATH']),
                         'the test PATH must not hold a real cliproxyapi')
        (self.tmp / 'bin' / 'brew').write_text('#!/bin/sh\nexit 0\n')
        (self.tmp / 'bin' / 'brew').chmod(0o755)
        rc, out, missed = run_pty('ccmorph add demo', env, [
            (b'Esc Cancel', b'\x1b[B' * 3 + b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)  # told the step, saved nothing
        plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', out)
        self.assertIn('More sign-ins', plain)
        self.assertIn('Antigravity, Codex, Devin, Kimi For Coding (global), '
                      'Kimi For Coding (China), Meta and xAI', plain)
        self.assertIn('brew install cliproxyapi', plain)
        self.assertNotIn('router-for-me', plain)
        self.assertIn('ccmorph add demo', plain)
        self.assertFalse((self.state / 'demo.conf').exists())

    def test_add_without_brew_answers_with_the_project_page_gh_369(self):
        # Issue #369: `brew install cliproxyapi` cannot be run on a machine
        # with no Homebrew, yet that is what More sign-ins used to say there.
        # The installer's own answer on such a machine is the project page,
        # and add gives the same one.
        env = self.env(fzf=True)
        entries = [e for e in env['PATH'].split(os.pathsep)
                   if not (Path(e) / 'cliproxyapi').exists()
                   and not (Path(e) / 'brew').exists()]
        env['PATH'] = os.pathsep.join(entries)
        self.assertIsNone(shutil.which('cliproxyapi', path=env['PATH']),
                          'the test PATH must not hold a real cliproxyapi')
        self.assertIsNone(shutil.which('brew', path=env['PATH']),
                          'the test PATH must not hold a real brew')
        rc, out, missed = run_pty('ccmorph add demo', env, [
            (b'Esc Cancel', b'\x1b[B' * 3 + b'\r'),
        ])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', out)
        self.assertIn('More sign-ins', plain)
        self.assertIn('https://github.com/router-for-me/CLIProxyAPI', plain)
        self.assertNotIn('brew install', plain)
        self.assertIn('ccmorph add demo', plain)
        self.assertFalse((self.state / 'demo.conf').exists())

    def test_add_more_signins_row_ends_on_a_whole_word_at_80_columns_gh_369(self):
        # Issue #369: at 80 columns the More sign-ins row used to end inside a
        # word ("install clip…"). After #366's fuller login names the row no
        # longer fits whole, so what matters is where the cut lands: after a
        # whole word — never a non-space right before the ellipsis.
        env = self.env(fzf=True)
        entries = [e for e in env['PATH'].split(os.pathsep)
                   if not (Path(e) / 'cliproxyapi').exists()]
        env['PATH'] = os.pathsep.join(entries)
        rc, out, missed = run_pty('ccmorph add demo', env, [
            (b'Esc Cancel', b'\x1b'),
        ], size=(24, 80))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        plain = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', out)
        rows = [line for line in plain.splitlines() if 'More sign-ins' in line]
        self.assertTrue(rows, out)
        self.assertIn('Antigravity', rows[0])
        self.assertNotRegex(rows[0], r'\S…')

    def test_standalone_ui_contract_and_unicode_alignment(self):
        env = {**self.env(fzf=True), 'NO_COLOR': '1',
               'ROWS': 'first\t中文\tFirst description\nsecond\tA\tSecond description'}
        self.spec('Standalone=second')
        # Only the module is sourced: it must not require ccmorph state/helpers.
        rc, out, missed = run_pty('ccm_ui_select native Standalone "$ROWS"', env,
                                  [(b'Esc Cancel', b'\x1b[B\r')])
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
        rc, out, missed = run_pty('ccm_ui_select search Standalone "$ROWS"', env)
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
interrupted=143
print hello | ccm_ui_input "Name: " 2>/dev/null; print "INPUT=$? $REPLY"
setopt sh_glob
print yes | ccm_ui_confirm 'Go?' 2>/dev/null; print "CONFIRM=$?"
ccm_ui_list Pick $'a\tAlpha' 2>&1 >/dev/null | od -c | grep -q 033 && print LISTANSI=1 || print LISTANSI=0
"""
        rc, out, _ = run_pty(script, self.env())
        self.assertEqual(rc, 0, out)
        self.assertIn('INPUT=0 hello', out)             # caller's name is not the signal flag
        self.assertIn('CONFIRM=0', out)                 # caller's glob options do not flip Yes
        self.assertIn('LISTANSI=0', out)                # redirected stderr stays plain

    def test_installed_resume_status_uses_brand_not_green(self):
        env = self.env()
        for name in ('demo', 'target'):
            (self.state / f'{name}.conf').write_text(
                'type=token\ntoken=synthetic-token\nmodel=sonnet\neffort=low\nmodel_defaults=1\n')
        # The relay resumes fixture-session: seed the record its first message
        # writes, so the resume has a conversation to carry (#288).
        projects = self.tmp / 'home/.claude/projects/p'
        projects.mkdir(parents=True)
        (projects / 'fixture-session.jsonl').write_text(
            '{"type":"user","sessionId":"fixture-session","uuid":"seed-1",'
            '"timestamp":"2026-01-01T00:00:00.000Z","isSidechain":false,'
            '"message":{"role":"user","content":"spoken before the switch"}}\n')
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

    def test_claude_palette_is_shared_by_native_and_search(self):
        env = {**self.env(fzf=True), 'ROWS': 'first\tFirst\tDescription\nsecond\tSecond\tDetails'}
        self.spec('Theme=first')
        rc, out, missed = run_pty('ccm_ui_select native Theme "$ROWS"', env,
                                  [(b'Esc Cancel', b'\x1b')])
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 1, out)
        self.assertIn('\x1b[38;2;215;119;87m\x1b[1mTheme', out)
        self.assertIn('\x1b[38;2;177;185;249m❯', out)
        self.assertIn('\x1b[38;2;153;153;153mDescription', out)
        for retired in ('◆', '◇', '●', '○', '│', '└', '\x1b[36m'):
            self.assertNotIn(retired, out)
        rc, out, missed = run_pty('ccm_ui_select search Theme "$ROWS"', env)
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
                        [(b'Value:', b'partial-value\x1b'), (b'VALUE=', b'')])
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
                    self.env(), [(b'Value:', keys)])
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
            self.env(), [(b'Esc Cancel', b'abc\r')], size=(24, 100))
        self.assertIsNone(missed, out)
        self.assertEqual(rc, 0, out)
        self.assertIn('P' * 90, out)
        self.assertNotIn('…', out)
        self.assertIn('VALUE=<abc>', out)
        self.assertIn('KSH_KEPT', out)

    def test_answered_input_folds_to_one_summary_line_issue_361(self):
        # An answered question folds like an answered picker (#361): the
        # heading, the "Enter Submit · Esc Cancel" hint and the typed line give
        # way to one line naming the question and its answer. A secret answer,
        # which echoed nothing, leaves the question alone — no answer text. An
        # answer longer than the terminal wraps beneath its question — the
        # question is never squeezed out — and the fold still erases every
        # line the answer touched, so the summary is all that stays.
        cases = (
            ('text', 'an-answer', ['  Value: › an-answer']),
            ('secret', 's3cret', ['  Value:']),
            # 120 cells: the echoed "❯ "-prefixed line wraps to 2, so the fold
            # must erase 4 lines; the answer cannot share the line with its
            # question, so the question stays whole and the answer wraps under
            # it, never cut.
            ('text', 'a' * 120, ['  Value: › ' + 'a' * 89, 'a' * 31]),
        )
        for mode, typed, summary in cases:
            with self.subTest(mode=mode, answer=typed[:12]):
                rc, out, missed = run_pty(
                    'ccm_ui_input "Value: " ' + mode + ' || exit $?; '
                    'print -r -- "LEN=${#REPLY}"', self.env(),
                    [(b'Value:', typed.encode() + b'\r')])
                self.assertIsNone(missed, out)
                self.assertEqual(rc, 0, out)
                rows = screen(out)
                self.assertEqual([row.rstrip() for row in rows],
                                 [row.rstrip() for row in summary] + ['LEN=%d' % len(typed)],
                                 out)
                flat = '\n'.join(rows)
                self.assertNotIn('Enter Submit', flat)  # the hint folds away
                self.assertNotIn('❯', flat)
                if mode == 'secret':
                    self.assertNotIn(typed, out)  # a hidden answer shows no text

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
                                          [(b'Esc Cancel', keys)])
                self.assertIsNone(missed, out)
                self.assertEqual(rc, status, out)
                self.assertIn('❯', out)
                self.assertIn('No', out)
                self.assertFalse((self.tmp / 'fzf.log').exists())

    def test_ui_rejects_control_characters_in_ids(self):
        env = {**self.env(), 'ROWS': 'bad\x1b[2J\tLabel'}
        rc, out, missed = run_pty('ccm_ui_select native Choose "$ROWS"', env)
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
                rc, out, missed = run_pty(command, env)
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