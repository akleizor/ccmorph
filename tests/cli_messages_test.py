"""Human output stays concise without changing machine-readable command output."""
import json
import os
import re
import shutil
from pathlib import Path
import subprocess
import tempfile

import pytest

from support import CLAUDE_MISSING, HANG_TIMEOUT, anthropic_accepts_the_key, path_without
from fake_adapter import CORE_DISPATCH

ROOT = Path(__file__).resolve().parents[1]
# Commands that never open a picker: their help pages say nothing about one (#263).
PICKERLESS = ('list', 'quota', 'price', 'check', 'mv', 'rm', 'hop', 'profile', 'update', 'uninstall')
PICKER_WORDS = r'picker|arrow|\besc\b|fzf|search|↑|↓'


@pytest.mark.parametrize('argv', [
    ['probe', '--help'], ['probe', 'gw', '--help'], ['probe', '-i', '-h'],
    ['mea' + 'sure', '--help'],
], ids=lambda a: ' '.join(a))
def test_probe_help_has_no_progress_banner_and_remains_actionable(argv):
    with tempfile.TemporaryDirectory() as tmp:
        env = {**os.environ, 'HOME': tmp, 'XDG_CONFIG_HOME': str(Path(tmp) / '.config'),
               'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(Path(tmp) / 'state')}
        result = subprocess.run([str(ROOT / "bin/ccmorph"), *argv], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert result.stdout.startswith('Usage: ccmorph probe [profile] [options]'), result.stdout
        assert 'Refreshing' not in result.stdout
        assert 'inference probe' not in result.stdout
        assert '--allow-inference' in result.stdout
        assert 'context window' in result.stdout
        helped = subprocess.run([str(ROOT / "bin/ccmorph"), 'help', 'probe'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert helped.returncode == 0, helped.stderr
        assert result.stdout == helped.stdout, (result.stdout, helped.stdout)


def test_probe_refuses_an_unknown_option_with_its_usage():
    with tempfile.TemporaryDirectory() as tmp:
        env = {**os.environ, 'HOME': tmp, 'XDG_CONFIG_HOME': str(Path(tmp) / '.config'),
               'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(Path(tmp) / 'state')}
        result = subprocess.run([str(ROOT / "bin/ccmorph"), 'probe', '--bogus'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 2, result.stderr
        assert result.stderr.startswith('usage: ccmorph probe [profile] [options]'), result.stderr
        assert 'unrecognized arguments: --bogus' in result.stderr
        assert 'ccmorph help probe' in result.stderr


def test_adapter_decline_surfaces_actionable_reason():
    with tempfile.TemporaryDirectory() as tmp:
        lib = Path(tmp) / 'lib'
        lib.mkdir()
        (lib.parent / 'libexec').mkdir()
        for f in (ROOT / 'lib').glob('ccmorph-*.zsh'):
            (lib / f.name).write_bytes(f.read_bytes())
        fake = lib.parent / 'libexec/ccmorph-core'
        fake.write_text('#!/bin/sh\n' + CORE_DISPATCH + 'printf "EXIT=set a model and retry\\n"\n')
        fake.chmod(0o755)
        env = {**os.environ, 'HOME': tmp, 'TMPDIR': tmp,
               'CCMORPH_LIB_DIR': str(lib)}
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_core_env; '
                                 'exec env "${reply[@]}" "$2" launch --adapter-start demo',
                                 'test', str(ROOT / 'lib/ccmorph.sh'), str(ROOT / 'libexec/ccmorph-core')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'set a model and retry' in result.stderr


def test_adapter_failure_reports_observation_not_guessed_cause():
    with tempfile.TemporaryDirectory() as tmp:
        lib = Path(tmp) / 'lib'
        lib.mkdir()
        (lib.parent / 'libexec').mkdir()
        for f in (ROOT / 'lib').glob('ccmorph-*.zsh'):
            (lib / f.name).write_bytes(f.read_bytes())
        fake = lib.parent / 'libexec/ccmorph-core'
        fake.write_text('#!/bin/sh\n' + CORE_DISPATCH + 'exit 1\n')
        fake.chmod(0o755)
        env = {**os.environ, 'HOME': tmp, 'CCMORPH_LIB_DIR': str(lib)}
        result = subprocess.run(['zsh', '-c', 'source "$1"; _ccm_core_env; '
                                 'exec env "${reply[@]}" "$2" launch --adapter-start demo known',
                                 'test', str(ROOT / 'lib/ccmorph.sh'), str(ROOT / 'libexec/ccmorph-core')], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert 'adapter exited before readiness' in result.stderr
        assert 'bug or a broken install' not in result.stderr
        assert 'Usually the gateway refused' not in result.stderr
        assert 'ccmorph doctor demo' in result.stderr


def test_help_is_concise_truthful_and_read_only(tmp_path):
    state = tmp_path / 'state'
    state.mkdir()
    marker = tmp_path / 'host-hook-ran'
    (state / 'local.sh').write_text(f'print ran > "{marker}"\n')
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(state)}
    cli = str(ROOT / "bin/ccmorph")
    for args in ([], ['--help'], ['help', 'config'], ['add', '--help'],
                 ['doctor', '--help'], ['update', '--help'], ['uninstall', '--help'],
                 ['help', 'update'], ['--version'],
                 ['rm', 'gw', '--help'], ['cred', 'gw', '-h'], ['use', 'gw', '--help'],
                 ['run', '--help'], ['run', '-h'], ['probe', 'gw', '--help']):
        result = subprocess.run([cli, *args], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert not marker.exists(), 'help must not source executable host overrides'
        assert not (state / 'profiles').exists(), 'help must not provision state'
        assert result.stderr == ''
    for flag in ('--help', '-h'):
        page = subprocess.run([cli, 'run', flag], env=env, capture_output=True,
                              text=True, timeout=HANG_TIMEOUT)
        assert page.stdout.startswith('Usage: ccmorph run'), page.stdout
        assert 'ccmorph run <profile> --help' in page.stdout
    result = subprocess.run([cli, '--help'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert len(result.stdout.splitlines()) <= 28
    assert 'ccmorph run -i' in result.stdout
    assert 'ccmorph help <command>' in result.stdout
    for advanced in ('--filtered-json', '--allow-inference', 'oauth_token', 'ccmorph cred', 'ccmorph context',
                     'ccmorph update', 'ccmorph uninstall'):
        assert advanced not in result.stdout
    result = subprocess.run([cli, 'help', 'all'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    for verb in ('run', 'add', 'list', 'use', 'config', 'inspect', 'doctor', 'models',
                 'get', 'set', 'login', 'mv', 'rm', 'hop', 'cred', 'probe', 'context',
                 'update', 'uninstall', 'quota', 'price', 'check', 'profile'):
        assert 'ccmorph ' + verb in result.stdout
        detail = subprocess.run([cli, 'help', verb], env=env, capture_output=True,
                                text=True, timeout=HANG_TIMEOUT)
        assert detail.returncode == 0, detail.stderr
        assert detail.stdout.startswith('Usage: ccmorph ' + verb)
        assert '[-a <profile>]' not in detail.stdout
        if verb in PICKERLESS:
            assert not re.search(PICKER_WORDS, detail.stdout, re.I), verb
        else:
            for phrase in ('arrow keys', 'type to search', 'Esc to cancel'):
                assert phrase in detail.stdout, verb
    # The page someone asked every command of is one list: no command row twice,
    # no line sending them back to `help all`, and the details/version footer
    # ends the page once (#365).
    assert 'ccmorph help all' not in result.stdout
    rows = [line for line in result.stdout.splitlines() if line.startswith('  ccmorph ')]
    assert len(rows) == len(set(rows)), 'a command row appears twice'
    assert result.stdout.count('Command details:') == 1
    assert result.stdout.count('Version: ccmorph --version') == 1
    assert result.stdout.rstrip().endswith(
        'Command details: ccmorph help <command>\nVersion: ccmorph --version')
    assert 'Start here: ccmorph add work' in result.stdout  # no profile exists yet
    # A reader with a profile has already started: neither page may point them
    # at a first profile they have (#365).
    (state / 'work.conf').write_text('type=apikey\nbase_url=https://api.anthropic.com\n')
    for args in (['--help'], ['help', 'all']):
        page = subprocess.run([cli, *args], env=env, capture_output=True,
                              text=True, timeout=HANG_TIMEOUT)
        assert page.returncode == 0, page.stderr
        assert 'Start here' not in page.stdout, args
    for verb in ('help', 'version'):
        page = subprocess.run([cli, 'help', verb], env=env, capture_output=True,
                              text=True, timeout=HANG_TIMEOUT)
        assert page.returncode == 0, page.stderr
        assert not re.search(PICKER_WORDS, page.stdout, re.I), verb
    result = subprocess.run([cli, 'help', 'quota'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    # One line names which profile kinds show plan usage (#270), so a token
    # profile's row is expected to explain rather than show windows.
    flat = ' '.join(result.stdout.split())
    assert 'Plan usage is read for Claude account and Codex profiles' in flat
    assert 'a pasted Claude token cannot read it' in flat
    result = subprocess.run([cli, 'help', 'typo'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 2
    assert 'ccmorph help all' in result.stderr
    pages = {verb: subprocess.run([cli, 'help', verb], env=env, capture_output=True,
                                  text=True, timeout=HANG_TIMEOUT).stdout for verb in ('get', 'set')}
    assert 'read' in pages['get'].lower() and 'save' not in pages['get'].lower(), pages['get']
    assert 'save' in pages['set'].lower(), pages['set']
    # One word names the model/effort setting on every page that names it (#367):
    # the config menu and its help once said "reasoning" where set, get, inspect
    # and the errors say effort, and nothing tied the two words together.
    pages['short'] = subprocess.run([cli, '--help'], env=env, capture_output=True,
                                    text=True, timeout=HANG_TIMEOUT).stdout
    pages['all'] = subprocess.run([cli, 'help', 'all'], env=env, capture_output=True,
                                  text=True, timeout=HANG_TIMEOUT).stdout
    pages['config'] = subprocess.run([cli, 'help', 'config'], env=env, capture_output=True,
                                     text=True, timeout=HANG_TIMEOUT).stdout
    for name, page in pages.items():
        assert 'reasoning' not in page.lower(), name
    assert 'Change model, effort, or connection' in pages['short']
    assert 'model and effort' in pages['config']
    assert not marker.exists()


def test_price_reads_cached_list_prices(tmp_path):
    state = tmp_path / 'xdg' / 'ccmorph'
    state.mkdir(parents=True)
    (state / 'prices-owned-any.json').write_text(json.dumps({
        'host': 'owned:any', 'fetched': 0,
        'models': {'example': {'input': 0.15, 'output': 0.6},
                   'free': {'input': 0, 'output': 0}},
    }))
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'XDG_STATE_HOME': str(tmp_path / 'xdg'),
           'CCMORPH_LIB_DIR': str(ROOT / 'lib'), 'CCMORPH_STATE_DIR': str(tmp_path / 'config')}
    cli = str(ROOT / 'bin/ccmorph')
    for args, expected in [
        (['price', '--json', 'example-FP8', 'missing', 'free'],
         {'example-FP8': {'input': 0.15, 'output': 0.6}, 'free': {'input': 0, 'output': 0}}),
        (['price', 'missing', '--json'], {}),
    ]:
        result = subprocess.run([cli, *args], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert json.loads(result.stdout) == expected
    result = subprocess.run([cli, 'price', 'example-FP8', 'missing'], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, result.stderr
    assert result.stdout == 'example-FP8  $0.15/$0.6 per 1M tokens\nmissing  unpriced\n'
    for args in (['price'], ['price', '--json']):
        result = subprocess.run([cli, *args], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 2
        assert 'usage: ccmorph price <model>... [--json]' in result.stderr


def test_price_empty_cache_message_and_json(tmp_path):
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'XDG_STATE_HOME': str(tmp_path / 'xdg'),
           'CCMORPH_LIB_DIR': str(ROOT / 'lib'), 'CCMORPH_STATE_DIR': str(tmp_path / 'config')}
    cli = str(ROOT / 'bin/ccmorph')
    hint = 'Run a `provider` profile with `ccmorph run PROFILE`; its launch refreshes this cache in the background.'
    (tmp_path / 'xdg' / 'ccmorph').mkdir(parents=True)
    for cache in (None, {'host': 'owned:any', 'fetched': 0, 'models': {}}):
        cache_path = tmp_path / 'xdg' / 'ccmorph' / 'prices-owned-any.json'
        if cache is None:
            cache_path.unlink(missing_ok=True)
        else:
            cache_path.write_text(json.dumps(cache))
        result = subprocess.run([cli, 'price', 'claude-sonnet-4-5', 'nosuchmodel'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert result.stdout == (
            f'No cached model prices. {hint}\n'
            'claude-sonnet-4-5  no cached price\nnosuchmodel  no cached price\n'
        )
        result = subprocess.run([cli, 'price', '--json', 'claude-sonnet-4-5'], env=env,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 0, result.stderr
        assert json.loads(result.stdout) == {'cache': 'empty', 'hint': hint, 'prices': {}}


def test_hop_help_needs_no_exit_and_names_both_forms_gh_289(tmp_path):
    # GH-289: the page said "Then exit Claude to complete the switch" although
    # hop switches the session at once, and taught only the ! form.
    state = tmp_path / 'state'
    state.mkdir()
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(state)}
    cli = str(ROOT / "bin/ccmorph")
    page = subprocess.run([cli, 'help', 'hop'], env=env, capture_output=True,
                          text=True, timeout=HANG_TIMEOUT)
    assert page.returncode == 0, page.stderr
    assert '/ccmorph:hop <profile>' in page.stdout
    assert '! ccmorph hop <profile>' in page.stdout
    assert 'exit Claude' not in page.stdout
    assert 'at once' in page.stdout
    assert 'No exit is needed' in page.stdout
    first = subprocess.run([cli, '--help'], env=env, capture_output=True,
                           text=True, timeout=HANG_TIMEOUT)
    assert first.returncode == 0, first.stderr
    hop_line = next((l for l in first.stdout.splitlines() if 'ccmorph hop' in l), '')
    assert hop_line, 'the first help screen must list hop'
    assert 'inside Claude' in hop_line


def test_hop_usage_line_agrees_across_help_and_the_slash_command(tmp_path):
    # GH-257: the places a user meets hop each drew their own usage line — a
    # bare `ccmorph hop` said [profile] while `ccmorph help hop` said <profile>
    # — and the slash command's description named neither.
    state = tmp_path / 'state'
    state.mkdir()
    # Scrub the runner's own session: this suite may run inside a Claude Code
    # session, where a bare hop is a question, not a usage error.
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(('CCMORPH', 'CLAUDE_', 'ANTHROPIC_'))}
    env.update(HOME=str(tmp_path / 'home'), CCMORPH_STATE_DIR=str(state),
               CCMORPH_LIB_DIR=str(ROOT / 'lib'), NO_COLOR='1')
    cli = str(ROOT / 'bin/ccmorph')
    usage = 'ccmorph hop [profile]'
    bare = subprocess.run([cli, 'hop'], env=env, capture_output=True,
                          text=True, timeout=HANG_TIMEOUT)
    assert bare.returncode == 1, bare.stdout
    assert 'usage: ' + usage in bare.stderr, bare.stderr
    detail = subprocess.run([cli, 'help', 'hop'], env=env, capture_output=True,
                            text=True, timeout=HANG_TIMEOUT)
    assert detail.returncode == 0, detail.stderr
    assert detail.stdout.splitlines()[0] == 'Usage: ' + usage
    assert '--slash' not in detail.stdout
    extra = subprocess.run([cli, 'hop', '--slash', 'a', 'b'], env=env, capture_output=True,
                           text=True, timeout=HANG_TIMEOUT)
    assert extra.returncode == 2, extra.stdout
    assert "unexpected argument 'b'" in extra.stderr
    assert 'usage: ' + usage in extra.stderr
    listing = subprocess.run([cli, 'help', 'all'], env=env, capture_output=True,
                             text=True, timeout=HANG_TIMEOUT)
    assert usage in listing.stdout
    command = (ROOT / 'share/claude-plugin/commands/hop.md').read_text()
    assert usage in command


def test_run_help_names_the_plain_run_question_and_its_keep_command_gh_360(tmp_path):
    # GH-360: the page tied the model question to -i alone, though a plain
    # `ccmorph run` on a gateway with no model saved asks too, and it never
    # named the command that keeps a choice.
    state = tmp_path / 'state'
    state.mkdir()
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(state)}
    cli = str(ROOT / "bin/ccmorph")
    page = subprocess.run([cli, 'help', 'run'], env=env, capture_output=True,
                          text=True, timeout=HANG_TIMEOUT)
    assert page.returncode == 0, page.stderr
    flat = ' '.join(page.stdout.split())
    assert 'asked on a plain run too' in flat
    assert 'this session only' in flat
    assert 'ccmorph set <profile> model <id>' in flat


def test_update_and_uninstall_under_homebrew_name_the_brew_command(tmp_path):
    # A brew install keeps the library under the Cellar with no installer beside it.
    root = tmp_path / 'Cellar/ccmorph/0.2.0/libexec'
    (root / 'lib').mkdir(parents=True)
    for f in (ROOT / 'lib').iterdir():
        (root / 'lib' / f.name).write_bytes(f.read_bytes())
    # A brew install ships the core beside the library, as every install does.
    (root / 'libexec').mkdir()
    shutil.copy2(ROOT / 'libexec/ccmorph-core', root / 'libexec/ccmorph-core')
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'CCMORPH_LIB_DIR': str(root / 'lib'), 'CCMORPH_STATE_DIR': str(tmp_path / 'state')}
    for verb, brew in (('update', 'brew upgrade ccmorph'), ('uninstall', 'brew uninstall ccmorph')):
        result = subprocess.run([str(ROOT / 'bin/ccmorph'), verb], env=env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode != 0
        assert brew in result.stderr, result.stderr


def _keg(tmp_path, rel):
    """An install laid out like a Homebrew keg under tmp_path/rel: the library in
    lib/, the core beside it. Returns the install root, the dir lib/'s parent."""
    root = tmp_path / rel
    (root / 'lib').mkdir(parents=True)
    for f in (ROOT / 'lib').iterdir():
        (root / 'lib' / f.name).write_bytes(f.read_bytes())
    (root / 'libexec').mkdir()
    shutil.copy2(ROOT / 'libexec/ccmorph-core', root / 'libexec/ccmorph-core')
    return root


def _env_home(tmp_path, lib_dir):
    # ZDOTDIR with HOME: the zsh launcher reads $ZDOTDIR/.zshenv before any CLI
    # code, so a real machine's startup file would print into stderr and break
    # these stderr assertions (REVIEW.md kind 3).
    return {**os.environ, 'HOME': str(tmp_path / 'home'), 'ZDOTDIR': str(tmp_path / 'home'),
            'CCMORPH_LIB_DIR': str(lib_dir), 'CCMORPH_STATE_DIR': str(tmp_path / 'state')}


def test_update_and_uninstall_help_name_the_homebrew_case_and_print_its_brew_command(tmp_path):
    # GH-1035: the README promises "ccmorph update and ccmorph uninstall refuse a
    # Homebrew install and print those commands", and a brew user who met that
    # refusal opens help first — where Homebrew was never named, and update's
    # example showed v0.1.0 on a 0.5.42 product.
    keg = _keg(tmp_path, Path('Cellar/ccmorph/0.2.0/libexec'))
    clone = _keg(tmp_path, 'clone')
    (clone / '.git').mkdir()  # a checkout is the layout the refusal was seen on
    opt = tmp_path / 'opt'
    opt.mkdir()
    (opt / 'ccmorph').symlink_to(keg)  # how a real brew reaches its keg
    envs = {'checkout': _env_home(tmp_path, clone / 'lib'),
            'cellar': _env_home(tmp_path, keg / 'lib'),
            'opt': _env_home(tmp_path, opt / 'ccmorph' / 'lib')}
    pages = {}
    for verb, brew in (('update', 'brew upgrade ccmorph'), ('uninstall', 'brew uninstall ccmorph')):
        forms = [['help', verb], [verb, '--help'], [verb, '-h'],
                 ['--help', verb], ['-h', verb]]
        if verb == 'update':
            forms.insert(3, ['update', 'v1', '--help'])
        pages[verb] = []
        for where, env in envs.items():
            for argv in forms:
                result = subprocess.run([str(ROOT / 'bin/ccmorph'), *argv], env=env,
                                        capture_output=True, text=True, timeout=HANG_TIMEOUT)
                assert result.returncode == 0, (where, argv, result.stderr)
                assert result.stderr == '', (where, argv, result.stderr)
                assert result.stdout.startswith('Usage: ccmorph ' + verb), (where, argv)
                flat = ' '.join(result.stdout.split())
                assert brew in flat, (where, argv, result.stdout)
                assert 'A Homebrew install' in flat, (where, argv, result.stdout)
                assert 'v0.1.0' not in result.stdout, (where, argv, result.stdout)
                pages[verb].append(result.stdout)
        assert all(p == pages[verb][0] for p in pages[verb][1:]), \
            f'help {verb}: a form or a layout printed a different page'
    update_page = ' '.join(pages['update'][0].split())
    assert 'A git checkout is refused: update it with git.' in update_page
    assert 'ccmorph update main' in update_page
    # The branch example must not read as a working install command: a plain
    # GitHub ref archive has no core binary, and the real installer refuses it.
    assert 'archive contains no ccmorph-core binary' in update_page
    assert 'update with no name installs the latest' in update_page
    uninstall_page = ' '.join(pages['uninstall'][0].split())
    assert 'Remove the ccmorph command from ~/.local/bin' in uninstall_page
    assert 'profiles and credentials stay' in uninstall_page
    assert 'not removed by this command' in uninstall_page
    assert 'not in ~/.local/bin' in uninstall_page
    assert not (tmp_path / 'state' / 'profiles').exists(), 'help must not provision state'


def test_update_help_example_is_durable_and_honest_about_what_it_installs(tmp_path):
    # GH-1035 (evaluator refusal): the branch example must read as a valid ref
    # name, not as a working install command — a plain GitHub archive of a
    # branch or tag carries no ccmorph-core binary, and the real installer
    # refuses such a tree before changing anything. The page says so, and names
    # the bare command as the path whose archive ships the binary.
    env = _env_home(tmp_path, _keg(tmp_path, 'clone') / 'lib')
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'help', 'update'], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, result.stderr
    assert result.stderr == '', result.stderr
    flat = ' '.join(result.stdout.split())
    assert 'ccmorph update main' in flat  # the durable ref-name example stays
    assert 'archive contains no ccmorph-core binary' in flat
    assert 'update with no name installs the latest' in flat
    assert 'v0.1.0' not in flat


def test_update_refuses_bad_refs_with_a_durable_example_and_never_reaches_the_installer(tmp_path):
    # GH-1035: update's example ages no more — in the page and in the advice a
    # mistyped ref lands on. The stub installer shows a rejected ref never
    # reaches it, a bare update still selects the latest release, and a
    # version-shaped ref is still accepted and installed.
    root = _keg(tmp_path, 'install')
    log = tmp_path / 'installer.log'
    installer = root / 'install.sh'
    installer.write_text('#!/bin/sh\n'
                         '{ echo "args: $*"; echo "ref: [${CCMORPH_REF-}]"; } > "$INSTALL_LOG"\n')
    installer.chmod(0o755)
    env = _env_home(tmp_path, root / 'lib')
    env['INSTALL_LOG'] = str(log)
    cli = str(ROOT / 'bin/ccmorph')
    for bad in ('', 'v0.1.0..bad', 'a b\nc'):
        result = subprocess.run([cli, 'update', bad], env=env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 2, (bad, result.returncode, result.stderr)
        assert not log.exists(), f'ref {bad!r} reached the installer'
        assert 'invalid ref' in result.stderr, (bad, result.stderr)
        advice = ' '.join(result.stderr.split()).split("' — ")[1]  # past the echoed operand
        assert 'such as main' in advice, (bad, advice)
        assert 'v0.1.0' not in advice, (bad, advice)
    assert subprocess.run([cli, 'update'], env=env, stdin=subprocess.DEVNULL,
                          capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode == 0
    assert log.read_text().splitlines() == ['args: --remote', 'ref: []'], log.read_text()
    log.unlink()
    assert subprocess.run([cli, 'update', 'v0.1.0'], env=env, stdin=subprocess.DEVNULL,
                          capture_output=True, text=True, timeout=HANG_TIMEOUT).returncode == 0
    assert log.read_text().splitlines() == ['args: --remote', 'ref: [v0.1.0]'], log.read_text()


def test_homebrew_reached_through_the_opt_symlink_still_refuses_byte_identically(tmp_path):
    # The refusal resolves the library dir, and a real brew reaches its keg through
    # opt/ccmorph: a test that only supplies a literal Cellar path would miss a
    # regression for brew installs as every brew machine spells them.
    keg = _keg(tmp_path, Path('Cellar/ccmorph/0.2.0/libexec'))
    opt = tmp_path / 'opt'
    opt.mkdir()
    (opt / 'ccmorph').symlink_to(keg)
    env = _env_home(tmp_path, opt / 'ccmorph' / 'lib')
    for verb, brew in (('update', 'brew upgrade ccmorph'), ('uninstall', 'brew uninstall ccmorph')):
        result = subprocess.run([str(ROOT / 'bin/ccmorph'), verb], env=env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=HANG_TIMEOUT)
        assert result.returncode == 1, (verb, result.returncode, result.stderr)
        assert result.stderr == ('ccmorph: ccmorph was installed with Homebrew; '
                                 'run: ' + brew + '\n'), result.stderr


def _help_add(tmp_path, bridge, providers='claude antigravity codex'):
    """help add with a PATH that has only a fake cliproxyapi, when bridge is
    given — and holds none otherwise, wherever the runner carries one (#414)."""
    bin_dir = tmp_path / 'bin'
    bin_dir.mkdir(exist_ok=True)
    if bridge:
        fake = bin_dir / 'cliproxyapi'
        fake.write_text('#!/bin/sh\n'
                        f'if [ "$1" = --help ]; then for p in {providers}; do'
                        ' echo "  -$p-login"; done; exit 0; fi\n'
                        'exit 1\n')
        fake.chmod(0o755)
    env = {**os.environ, 'HOME': str(tmp_path / 'home'),
           'PATH': f'{bin_dir}:{path_without("cliproxyapi", path="/usr/bin:/bin")}',
           'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'CCMORPH_STATE_DIR': str(tmp_path / 'state')}
    result = subprocess.run([str(ROOT / "bin/ccmorph"), 'help', 'add'], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, result.stderr
    return result.stdout + result.stderr


def test_add_help_names_provider_logins_only_when_the_bridge_is_installed(tmp_path):
    without = _help_add(tmp_path, bridge=False)
    assert 'cliproxyapi' not in without
    assert 'Provider' not in without
    with_bridge = _help_add(tmp_path, bridge=True)
    assert 'Antigravity, Codex: sign in to that service with your browser (through cliproxyapi).' in with_bridge
    assert 'Claude: sign in to that service' not in with_bridge


def test_add_help_names_both_kimi_sign_ins_by_region_gh_366(tmp_path):
    # GH-366: the login list called the two Kimi sites Kimi-ai and Kimi — names
    # only a domain tells apart — while the API-key provider list calls the same
    # sites Kimi For Coding (global) and Kimi For Coding (China), and the plain
    # name stood for the less obvious one. help add carries the provider list's
    # name for each service, beside the two-login case above.
    with_bridge = _help_add(tmp_path, bridge=True, providers='claude kimi-ai kimi')
    assert ('Kimi For Coding (global), Kimi For Coding (China): '
            'sign in to that service with your browser (through cliproxyapi).') in with_bridge
    assert 'Kimi-ai' not in with_bridge


def test_add_help_indents_every_sign_in_method_the_same_way_gh_364(tmp_path):
    # GH-364: the first method sat flush with the left edge while the others
    # were indented, so the list read as one method and a stray paragraph.
    lines = _help_add(tmp_path, bridge=True).splitlines()
    indents = {}
    for method in ('Claude account:', 'Claude token:', 'API key:', 'Antigravity,'):
        line = next((l for l in lines if l.lstrip().startswith(method)), None)
        assert line is not None, method
        indents[method] = len(line) - len(line.lstrip())
    assert set(indents.values()) == {2}, indents


def test_add_more_signins_names_the_machines_own_install_step_gh_369(tmp_path):
    """#369: the step More sign-ins names is the machine's own — the brew
    command where Homebrew is on PATH, the project page the installer gives
    where it is not — never a command the machine cannot run. The numbered
    menu (piped stdin) drives the same answer the picker does."""
    state, home, bindir, tmp = (tmp_path / d for d in ('state', 'home', 'bin', 'tmp'))
    for d in (state, home, bindir, tmp):
        d.mkdir()
    # The system directories keep every tool but the ones this test seeds its
    # own answers for: a machine that carries the real brew would otherwise
    # decide which step is the machine's own before the test asks (#414).
    env = {**os.environ, 'HOME': str(home),
           'PATH': f'{bindir}:{path_without("cliproxyapi", "brew", path="/usr/bin:/bin")}',
           'TMPDIR': str(tmp), 'CCMORPH_STATE_DIR': str(state),
           'CCMORPH_LIB_DIR': str(ROOT / 'lib')}
    assert shutil.which('cliproxyapi', path=env['PATH']) is None, 'the test PATH must not hold a cliproxyapi'
    assert shutil.which('brew', path=env['PATH']) is None, 'the test PATH must not hold a brew'
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'add', 'demo'], env=env,
                            input='more\n', capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 1, result.stdout + result.stderr
    assert 'https://github.com/router-for-me/CLIProxyAPI' in result.stdout, result.stdout
    assert 'brew install' not in result.stdout, result.stdout
    assert 'ccmorph add demo' in result.stdout, result.stdout
    assert not (state / 'demo.conf').exists()
    # The same machine with Homebrew on PATH: the command, not the page.
    (bindir / 'brew').write_text('#!/bin/sh\nexit 0\n')
    (bindir / 'brew').chmod(0o755)
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'add', 'demo'], env=env,
                            input='more\n', capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 1, result.stdout + result.stderr
    assert 'brew install cliproxyapi' in result.stdout, result.stdout
    assert 'router-for-me' not in result.stdout, result.stdout


def test_add_claude_account_without_claude_names_the_missing_cli_gh_789(tmp_path):
    """#789: Enter on the highlighted "Claude account" row on a machine whose
    PATH holds no claude exited 1 with only "logout failed" — it named neither
    the missing program nor the fix, and left no next step. The account
    sign-in now asks the question `ccmorph run` asks first (#282): the one
    sentence with the install link answers, for `add` and for `login`, and
    saves nothing: the install command and the way to another method answer
    with it. Methods that exec no claude keep working with it absent, and a
    PATH that holds one keeps the account flow reaching its own login."""
    state, home, bindir, tmp = (tmp_path / d for d in ('state', 'home', 'bin', 'tmp'))
    for d in (state, home, bindir, tmp):
        d.mkdir()
    # The system directories keep every tool but claude: the gate's machine
    # carries the real one, and a PATH that found it would answer before the
    # refusal the test is about ever ran (#414).
    env = {**os.environ, 'HOME': str(home),
           'PATH': f'{bindir}:{path_without("claude", path="/usr/bin:/bin")}',
           'TMPDIR': str(tmp), 'CCMORPH_STATE_DIR': str(state),
           'CCMORPH_LIB_DIR': str(ROOT / 'lib')}
    assert shutil.which('claude', path=env['PATH']) is None, 'the test PATH must not hold a claude'
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'add', 'demo'], env=env,
                            input='sub\n', capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 1, result.stdout + result.stderr
    assert CLAUDE_MISSING in result.stderr, result.stderr
    assert 'curl -fsSL https://claude.ai/install.sh' in result.stderr, result.stderr
    assert 'pick another sign-in method: 2) Claude token, 3) API key' in result.stderr, result.stderr
    assert 'logout failed' not in result.stderr, result.stderr
    assert not (state / 'demo.conf').exists()
    # login signs the same flow: a signed-out Claude account profile on the
    # same PATH gets the same sentence, never the bare "login failed".
    (state / 'demo.conf').write_text('type=sub\nemail=a@b.c\n')
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'login', 'demo'], env=env,
                            input='', capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 1, result.stdout + result.stderr
    assert CLAUDE_MISSING in result.stderr, result.stderr
    assert 'then run: ccmorph login demo' in result.stderr, result.stderr
    assert 'login failed' not in result.stderr, result.stderr
    assert (state / 'demo.conf').read_text() == 'type=sub\nemail=a@b.c\n'
    # A method that execs no claude still adds with it absent.
    (state / 'demo.conf').unlink()
    anthropic_accepts_the_key(bindir)
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'add', 'demo'], env=env,
                            input='token\nsk-ant-oat01-abcdefghij0123456789ABCD\n',
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 0, result.stdout + result.stderr
    assert 'Added' in result.stdout, result.stdout
    # The same sign-in on a PATH that does hold claude reaches its login:
    # the answer above refuses only a machine with no claude to run. (The
    # fake signs in no one, so the add still ends 1 — past the refusal.)
    (bindir / 'claude').write_text('#!/bin/sh\nexit 0\n')
    (bindir / 'claude').chmod(0o755)
    (state / 'demo.conf').unlink()
    shutil.rmtree(state / 'profiles' / 'demo', ignore_errors=True)
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'add', 'demo'], env=env,
                            input='sub\n', capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode == 1, result.stdout + result.stderr
    assert CLAUDE_MISSING not in result.stderr, result.stderr
    assert 'no Claude account profile found after login' in result.stderr, result.stderr


def test_run_without_claude_names_the_problem_and_the_path_it_searched(tmp_path):
    """#282: a launch that cannot find claude says what is missing, links its
    setup page and shows the PATH it searched (the sentence doctor and the
    installer print) instead of the shell's own command-not-found line."""
    state, home, bindir, tmp = (tmp_path / d for d in ('state', 'home', 'bin', 'tmp'))
    for d in (state, home, bindir, tmp):
        d.mkdir()
    (state / 'demo.conf').write_text('type=apikey\napi_key=k\n')
    # The launch asks Anthropic about the key before it looks for claude (#255).
    anthropic_accepts_the_key(bindir)
    # A PATH with no claude on it: the host's own PATH may carry one, so the
    # system directories are taken with it filtered out (#414).
    env = {**os.environ, 'HOME': str(home),
           'PATH': f'{bindir}:{path_without("claude", path="/usr/bin:/bin")}',
           'TMPDIR': str(tmp), 'CCMORPH_STATE_DIR': str(state),
           'CCMORPH_LIB_DIR': str(ROOT / 'lib')}
    result = subprocess.run([str(ROOT / 'bin/ccmorph'), 'run', 'demo'], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert result.returncode != 0, result.stdout
    assert CLAUDE_MISSING in result.stderr, result.stderr
    assert f'PATH searched: {bindir}' in result.stderr, result.stderr
    assert 'command not found' not in result.stderr, result.stderr
