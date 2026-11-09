"""Regression tests for user-facing behaviour no other suite pinned down.

Covers the profile lifecycle commands (use, list, mv, rm, login), the launch
guards of `ccmorph run`, and the credential environment a launch hands Claude
Code per profile type. Everything runs the real `ccmorph` executable against an
isolated HOME with a stub `claude`; nothing touches the network or a real home.
"""
import json
import os
import stat
import subprocess
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


class Box:
    def __init__(self, home, state='.config/ccmorph'):
        self.home = home
        self.state = home / state
        (self.state / 'profiles').mkdir(parents=True)
        (home / 'bin').mkdir()
        (home / '.claude/projects').mkdir(parents=True)
        claude = home / 'bin/claude'
        claude.write_text(
            '#!/bin/sh\n'
            '# `claude --help` is how ccmorph discovers subcommands; not a launch.\n'
            '[ "$1" = --help ] && exit 0\n'
            '{ printf "ARGS"; for a do printf " [%s]" "$a"; done; echo\n'
            '  echo "TOKEN=${CLAUDE_CODE_OAUTH_TOKEN-<unset>}"\n'
            '  echo "KEY=${ANTHROPIC_API_KEY-<unset>}"\n'
            '  echo "BASE=${ANTHROPIC_BASE_URL-<unset>}"\n'
            '  echo "CFG=${CLAUDE_CONFIG_DIR-<unset>}"\n'
            '} >> "$HOME/claude.log"\n')
        claude.chmod(claude.stat().st_mode | stat.S_IXUSR)
        self.env = {k: v for k, v in os.environ.items()
                    if k != 'CLAUDECODE'
                    and not k.startswith(('CCMORPH', '_CCMORPH', 'CLAUDE_', 'ANTHROPIC_'))}
        self.env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / '.config'),
                        CCMORPH_STATE_DIR=str(self.state), CCMORPH_LIB_DIR=str(ROOT / "lib"),
                        PATH=f'{home}/bin:{os.environ["PATH"]}', LANG='C.UTF-8', LC_ALL='C.UTF-8',
                        NO_COLOR='1')

    def conf(self, name, text):
        (self.state / f'{name}.conf').write_text(text)

    def stub_oauth_claude(self, email):
        # What `add` of a subscription profile needs from `claude`: auth
        # login/logout succeed, and auth status reports an oauth session for
        # `email` — the readback that names the new profile.
        claude = self.home / 'bin/claude'
        claude.write_text(
            '#!/bin/sh\n'
            '[ "$1" = --help ] && exit 0\n'
            'if [ "$1 $2" = "auth status" ]; then\n'
            f'  printf \'{{"loggedIn": true, "authMethod": "claude.ai", "email": "{email}"}}\\n\'\n'
            'fi\n'
            'exit 0\n')
        claude.chmod(claude.stat().st_mode | stat.S_IXUSR)

    def run(self, *args, answer=None):
        return subprocess.run([str(ROOT / "bin/ccmorph"), *args], env=self.env, input=answer or '',
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)

    @property
    def active(self):
        p = self.state / 'active'
        return p.read_text().strip() if p.exists() else None

    def log(self):
        p = self.home / 'claude.log'
        return p.read_text() if p.exists() else ''


@pytest.fixture
def box(tmp_path):
    return Box(tmp_path)


def seed(box):
    box.conf('work', 'type=sub\nemail=me@example.invalid\n')
    box.conf('tok', 'type=token\ntoken=sk-ant-oat-canary\n')
    box.conf('gw', 'type=apikey\napi_key=canary-key\nbase_url=https://gw.example.invalid\n')


# --- use / list -------------------------------------------------------------

def test_use_sets_the_default_and_bare_use_reports_it(box):
    seed(box)
    assert 'No default set' in box.run('use').stdout
    done = box.run('use', 'tok')
    assert done.returncode == 0, done.stderr
    assert 'Default set' in done.stdout
    assert box.active == 'tok'
    assert box.run('use').stdout.strip() == 'tok'


def test_use_refuses_an_unknown_or_invalid_profile_and_keeps_the_default(box):
    seed(box)
    box.run('use', 'work')
    missing = box.run('use', 'nope')
    assert missing.returncode != 0 and "no profile 'nope'" in missing.stderr
    bad = box.run('use', 'bad/name')
    assert bad.returncode != 0 and 'invalid name' in bad.stderr
    assert box.active == 'work'


def test_list_marks_the_default_and_labels_every_profile_type(box):
    seed(box)
    box.run('use', 'tok')
    box.conf('priv', 'type=token\ntoken=t\nprivate=1\n')
    out = box.run('list').stdout
    rows = {line.split()[1] if line.lstrip().startswith('*') else line.split()[0]: line
            for line in out.splitlines() if line.strip()}
    assert rows['tok'].lstrip().startswith('*') and '(token)' in rows['tok']
    assert not rows['work'].lstrip().startswith('*')
    assert 'sub: me@example.invalid' in rows['work']
    assert 'apikey: https://gw.example.invalid' in rows['gw']
    assert 'canary' not in out
    assert 'private' in rows['priv'] and 'private' not in rows['tok']


def test_list_with_no_profiles_points_at_add(box):
    out = box.run('list')
    assert out.returncode == 0 and 'ccmorph add' in out.stdout


@pytest.mark.parametrize('meta', [
    pytest.param('[6]', id='brackets-gh-113'),
    pytest.param('*', id='star-gh-113'),
    pytest.param('?', id='question-gh-113'),
])
def test_list_tolerates_glob_metacharacters_in_the_state_dir_gh_113(tmp_path, meta):
    # GH-113: discovery joined the state dir into the glob pattern, so a
    # bracket in the path matched nothing and a * or ? reached into sibling
    # directories. The zsh quoted the directory and globbed only the conf
    # pattern, so the path stayed literal; list must show the same profiles
    # either way.
    plain = Box(tmp_path)
    seed(plain)
    odd = Box(tmp_path / 'odd', state=f'ccm{meta}')
    seed(odd)
    # A sibling the unquoted pattern also matches: ccm[6], ccm* and ccm? all
    # match the name ccm6, so a pattern that is not literal lists its conf too.
    decoy = odd.home / 'ccm6'
    decoy.mkdir()
    (decoy / 'tok.conf').write_text('type=token\ntoken=decoy\n')
    out = odd.run('list')
    baseline = plain.run('list')
    assert out.returncode == 0, out.stderr
    assert out.stdout == baseline.stdout


# --- mv / rm ------------------------------------------------------------------

def test_mv_carries_conf_profile_dir_auth_and_default_to_the_new_name(box):
    seed(box)
    box.run('use', 'work')
    (box.state / 'profiles/work').mkdir(exist_ok=True)
    (box.state / 'profiles/work/settings.json').write_text('{"keep":true}')
    done = box.run('mv', 'work', 'job')
    assert done.returncode == 0, done.stderr
    assert not (box.state / 'work.conf').exists()
    assert 'email=me@example.invalid' in (box.state / 'job.conf').read_text()
    assert (box.state / 'profiles/job/settings.json').read_text() == '{"keep":true}'
    assert not (box.state / 'profiles/work').exists()
    assert box.active == 'job'


def test_mv_refuses_an_existing_target_and_changes_nothing(box):
    seed(box)
    before = (box.state / 'work.conf').read_text()
    clash = box.run('mv', 'work', 'tok')
    assert clash.returncode != 0 and "'tok' already exists" in clash.stderr
    assert (box.state / 'work.conf').read_text() == before
    assert 'token=sk-ant-oat-canary' in (box.state / 'tok.conf').read_text()
    gone = box.run('mv', 'ghost', 'new')
    assert gone.returncode != 0 and "no profile 'ghost'" in gone.stderr


def test_mv_onto_a_leftover_directory_is_refused_not_nested(box):
    seed(box)
    (box.state / 'profiles/work').mkdir()
    (box.state / 'profiles/job').mkdir()
    refused = box.run('mv', 'work', 'job')
    assert refused.returncode != 0 and 'already exists' in refused.stderr
    assert (box.state / 'work.conf').exists() and not (box.state / 'job.conf').exists()
    assert (box.state / 'profiles/work').is_dir()
    assert not (box.state / 'profiles/job/work').exists()


def test_rm_deletes_the_profile_and_clears_it_as_the_default(box):
    seed(box)
    box.run('use', 'gw')
    (box.state / 'profiles/gw').mkdir(exist_ok=True)
    done = box.run('rm', 'gw')
    assert done.returncode == 0, done.stderr
    assert not (box.state / 'gw.conf').exists()
    assert not (box.state / 'profiles/gw').exists()
    assert box.active is None
    assert (box.state / 'tok.conf').exists()
    assert 'gw' not in box.run('list').stdout


def test_rm_of_a_non_default_profile_keeps_the_default(box):
    seed(box)
    box.run('use', 'work')
    assert box.run('rm', 'tok').returncode == 0
    assert box.active == 'work'


def test_rm_unknown_profile_is_an_error(box):
    out = box.run('rm', 'ghost')
    assert out.returncode != 0 and "no profile 'ghost'" in out.stderr


# --- login ----------------------------------------------------------------------

def test_login_for_an_api_key_profile_records_the_approval_again(box):
    # The fix a refused launch names: the key stays as it is, and Claude Code is
    # told to use it, so the next launch reaches a session instead of the login menu.
    seed(box)
    key = next(line.split('=', 1)[1] for line in (box.state / 'gw.conf').read_text().splitlines()
               if line.startswith('api_key='))
    out = box.run('login', 'gw')
    assert out.returncode == 0, out.stderr
    doc = json.loads((box.state / 'profiles' / 'gw' / '.claude.json').read_text())
    assert doc['customApiKeyResponses']['approved'] == [key[-20:]], doc
    # login still refuses what it cannot serve: only provider and API-key
    # profiles are its work, and a subscription or token profile must not fall
    # through to the provider browser login.
    for other in ('work', 'tok'):
        bad = box.run('login', other)
        assert bad.returncode != 0 and 'not a provider or API-key profile' in bad.stderr
    gone = box.run('login', 'ghost')
    assert gone.returncode != 0 and "no profile 'ghost'" in gone.stderr


# --- run guards ------------------------------------------------------------------

def test_run_with_no_default_and_no_profile_launches_nothing(box):
    seed(box)
    out = box.run('run')
    assert out.returncode != 0 and 'no default profile set' in out.stderr
    assert box.log() == ''


def test_run_with_no_profiles_points_at_add(box):
    out = box.run('run')
    assert out.returncode != 0
    assert 'ccmorph add' in out.stderr and 'ccmorph use' not in out.stderr
    assert box.log() == ''


def test_the_first_added_profile_becomes_the_default(box):
    first = box.run('add', 't1', answer='token\nsk-ant-oat-canary\n')
    assert first.returncode == 0, first.stderr
    assert box.active == 't1'
    second = box.run('add', 't2', answer='token\nsk-ant-oat-canary2\n')
    assert second.returncode == 0, second.stderr
    assert box.active == 't1'


def test_add_token_prompt_names_where_a_token_comes_from_issue_290(box):
    # GH-290: the token prompt asked for a paste and never said where a token
    # comes from or what a valid one starts with, so a person without one hit a
    # dead end that no other screen in ccmorph opened a way out of.
    done = box.run('add', 'tk1', answer='token\nsk-ant-oat01-pasted-example\n')
    assert done.returncode == 0, done.stderr
    assert 'claude setup-token' in done.stdout
    assert 'starts with sk-ant-oat01-' in done.stdout
    assert 'For scripts/CI; quota cannot read plan usage' in done.stderr


@pytest.mark.parametrize('with_a_default', [
    pytest.param(True, id='an-existing-default-is-kept-gh-17'),
    pytest.param(False, id='a-first-subscription-profile-becomes-the-default'),
])
def test_adding_a_subscription_profile_and_the_default(box, with_a_default):
    # GH-17: the sub branch of `add` wrote `active` itself, ahead of the shared
    # first-profile guard, so a subscription add replaced whichever profile
    # held the default.
    box.stub_oauth_claude('sub@example.invalid')
    if with_a_default:
        seed(box)
        box.run('use', 'work')
    done = box.run('add', 'personal', answer='1\n')
    assert done.returncode == 0, done.stderr
    assert box.active == ('work' if with_a_default else 'personal')


def test_run_names_an_unknown_profile_instead_of_launching_the_default(box):
    seed(box)
    box.run('use', 'work')
    out = box.run('run', 'ghost')
    assert out.returncode != 0 and "no profile 'ghost'" in out.stderr
    assert box.log() == ''


def test_run_rejects_the_retired_dash_a_profile_flag(box):
    seed(box)
    out = box.run('run', '-a', 'work')
    assert out.returncode == 2 and 'launch profile is positional' in out.stderr
    assert box.log() == ''


def test_a_profile_with_a_missing_credential_is_refused_at_launch(box):
    box.conf('empty', 'type=token\n')
    box.conf('anon', 'type=sub\n')
    box.conf('odd', 'type=banana\n')
    box.conf('keyless', 'type=apikey\n')
    for name, why in (('empty', 'has no token'), ('anon', 'has no email'),
                      ('odd', 'invalid or missing type'),
                      ('keyless', 'has no api_key')):
        out = box.run('run', name)
        assert out.returncode != 0 and why in out.stderr, (name, out.stderr)
    assert box.log() == ''


def test_a_key_foreign_to_the_profile_type_is_rejected(box):
    box.conf('tok', 'type=token\ntoken=t\napi_key=oops\n')
    out = box.run('run', 'tok')
    assert out.returncode != 0 and "key 'api_key' is invalid for token profile" in out.stderr
    assert box.log() == ''


# --- the credential environment a launch hands Claude Code -------------------------

def test_a_token_profile_launches_with_its_token_and_no_api_key(box):
    seed(box)
    box.env['ANTHROPIC_API_KEY'] = 'ambient-must-not-leak'
    out = box.run('run', 'tok')
    assert out.returncode == 0, out.stderr
    log = box.log()
    assert 'TOKEN=sk-ant-oat-canary' in log
    assert 'KEY=<unset>' in log and 'BASE=<unset>' in log
    assert f'CFG={box.state}/profiles/tok' in log


def test_a_subscription_profile_launches_with_no_injected_credential(box):
    seed(box)
    box.env.update(CLAUDE_CODE_OAUTH_TOKEN='ambient-token', ANTHROPIC_API_KEY='ambient-key')
    out = box.run('run', 'work')
    assert out.returncode == 0, out.stderr
    log = box.log()
    assert 'TOKEN=<unset>' in log and 'KEY=<unset>' in log
    assert f'CFG={box.state}/profiles/work' in log


# --- launch arguments, retired verbs, secret handling ----------------------------------

@pytest.mark.parametrize('argv, forwarded', [
    (('--', '--continue', '-p', 'hi'), 'ARGS [--continue] [-p] [hi]'),
    # GH-321: `-a` after the profile is claude's value, not ccmorph's flag.
    (('--system-prompt', '-a'), 'ARGS [--system-prompt] [-a]'),
])
def test_run_forwards_claude_arguments_and_swallows_its_own_separator(box, argv, forwarded):
    seed(box)
    out = box.run('run', 'tok', *argv)
    assert out.returncode == 0, out.stderr
    assert forwarded in box.log()


def test_run_uses_the_default_profile_when_none_is_named(box):
    seed(box)
    box.run('use', 'tok')
    assert box.run('run').returncode == 0
    assert 'TOKEN=sk-ant-oat-canary' in box.log()


def test_retired_verbs_point_at_their_replacement_and_launch_nothing(box):
    seed(box)
    for old, new in (('start', 'run'), ('default', 'use'), ('show', 'inspect'),
                     ('creds', 'cred'), ('secret', 'cred')):
        out = box.run(old, 'tok')
        assert out.returncode == 1, old
        assert f"unknown command '{old}'. Run: ccmorph {new} tok" in out.stderr, (old, out.stderr)
    assert box.log() == '' and box.active is None


def test_cred_prints_the_stored_secret_bare_and_refuses_when_there_is_none(box):
    seed(box)
    assert box.run('cred', 'tok').stdout == 'sk-ant-oat-canary'
    assert box.run('cred', 'gw').stdout == 'canary-key'
    out = box.run('cred', 'work')
    assert out.returncode == 1 and out.stdout == '' and 'stores no credential' in out.stderr
    out = box.run('cred', 'ghost')
    assert out.returncode == 1 and "no profile 'ghost'" in out.stderr


def test_inspect_never_prints_the_stored_credential(box):
    seed(box)
    for name in ('tok', 'gw'):
        out = box.run('inspect', name)
        assert out.returncode == 0, out.stderr
        assert 'canary' not in out.stdout + out.stderr
        assert 'deleted when the session ends' not in out.stdout


def test_inspect_shows_a_profile_whose_conf_fails_validation_gh_113(box):
    # GH-113: inspect read the conf through the validating reader, so it
    # refused exactly the profiles that need diagnosing — the conf `list`
    # still shows. The zsh read the conf and validated nothing; validation
    # belongs to launch and hop, not to the read-only view.
    box.conf('broken', 'type=apikey\n')
    out = box.run('inspect', 'broken')
    assert out.returncode == 0, out.stderr
    assert 'broken' in out.stdout and 'apikey' in out.stdout
    assert 'has no api_key' not in out.stderr


def test_inspect_names_the_history_a_private_profile_deletes_gh_292(box):
    # GH-292: a private profile deletes its conversation when the session ends,
    # and no view said so. inspect is where a profile's promises are read, each
    # line naming its source, so the deleted history is named there too.
    box.conf('priv', 'type=token\ntoken=t\nprivate=1\n')
    out = box.run('inspect', 'priv')
    assert out.returncode == 0, out.stderr
    assert 'deleted when the session ends' in out.stdout
    assert 'private=1' in out.stdout


# --- an unknown profile name reads the same in every command ---------------

UNKNOWN_PROFILE_ARGV = [
    ['run', 'zzz'], ['use', 'zzz'], ['quota', 'zzz'], ['models', 'zzz'], ['hop', 'zzz'],
    ['context', 'zzz', 'm'], ['config', 'zzz'], ['get', 'zzz', 'model'],
    ['set', 'zzz', 'model', 'x'], ['mv', 'zzz', 'yyy'], ['rm', 'zzz'], ['inspect', 'zzz'],
    ['login', 'zzz'], ['cred', 'zzz'], ['doctor', 'zzz'], ['probe', 'zzz'],
]


@pytest.mark.parametrize('argv', UNKNOWN_PROFILE_ARGV, ids=lambda a: ' '.join(a))
def test_an_unknown_profile_reads_the_same_in_every_command(box, argv):
    seed(box)
    result = box.run(*argv)
    out = result.stdout + result.stderr
    assert result.returncode != 0, out
    assert "no profile 'zzz'. Profiles: gw tok work" in out, out
    assert 'Run: ccmorph list' not in out
    assert 'missing or symlinked' not in out
    assert 'Nothing discovered' not in out


@pytest.mark.parametrize('argv', [['use', 'zzz'], ['inspect', 'zzz'], ['doctor', 'zzz']],
                         ids=lambda a: ' '.join(a))
def test_an_unknown_profile_with_none_saved_points_to_add(box, argv):
    result = box.run(*argv)
    out = result.stdout + result.stderr
    assert result.returncode != 0, out
    assert "no profile 'zzz'. No profiles yet — run: ccmorph add <name>" in out, out


def test_add_and_rm_without_a_name_print_usage(box):
    added = box.run('add')
    assert added.returncode != 0
    assert 'usage: ccmorph add <profile>' in added.stderr
    assert 'invalid name' not in added.stderr
    removed = box.run('rm')
    assert removed.returncode != 0
    assert 'usage: ccmorph rm <profile>' in removed.stderr


# --- a help flag anywhere is help; a word too many is refused ---------------

def state_files(root):
    return {str(p.relative_to(root)): p.read_bytes()
            for p in sorted(root.rglob('*')) if p.is_file()}


HELP_ANYWHERE = [
    ['rm', 'gw', '--help'], ['rm', 'gw', '-h'], ['use', 'tok', '-h'],
    ['login', 'gw', '--help'], ['cred', 'gw', '--help'], ['inspect', 'gw', '-h'],
    ['context', 'gw', 'm', '--help'], ['models', 'gw', '--help'],
    ['doctor', 'gw', '--help'], ['quota', 'gw', '--help'], ['config', 'gw', '--help'],
    ['get', 'gw', '--help'], ['set', 'gw', 'model', '--help'], ['mv', 'gw', 'new', '--help'],
    ['hop', 'gw', '--help'], ['add', 'new', '--help'], ['list', '--help'],
    ['update', 'v1', '--help'], ['uninstall', '--help'], ['version', '--help'],
    ['help', 'rm', '--help'], ['run', '--help'],
]


@pytest.mark.parametrize('argv', HELP_ANYWHERE, ids=lambda a: ' '.join(a))
def test_a_help_flag_anywhere_prints_help_and_changes_nothing(box, argv):
    # The issue: `ccmorph rm gw --help` deleted gw, `ccmorph cred gw --help`
    # printed the key, and use/login changed state. Help must win wherever the
    # flag sits, before secureDirs or any verb runs.
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    before = state_files(box.state)
    result = box.run(*argv)
    assert result.returncode == 0, result.stderr
    assert result.stdout.startswith('Usage: ccmorph ' + argv[0]), result.stdout
    assert state_files(box.state) == before
    assert box.active == 'tok'
    assert 'canary-key\nbase_url=https://gw.example.invalid\n' not in result.stdout + result.stderr
    assert box.log() == ''


VERB_ARITY = [('list', 0), ('use', 1), ('cred', 1), ('inspect', 1), ('context', 3),
              ('models', 2), ('rm', 1), ('mv', 2), ('get', 2), ('set', 3), ('config', 1),
              ('add', 1), ('login', 1), ('hop', 1), ('quota', 2), ('doctor', 2),
              ('update', 1), ('uninstall', 0)]


@pytest.mark.parametrize('verb,n', VERB_ARITY, ids=lambda v: str(v))
def test_a_word_after_the_arguments_is_refused_with_the_help_pages_usage(box, verb, n):
    # The issue's second half: `ccmorph rm gw tok` was read as `rm gw`. One word
    # beyond what a command takes is refused before anything runs, named in the
    # message, with the same usage line its help page prints.
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    page = box.run('help', verb)
    assert page.returncode == 0, page.stderr
    usage = page.stdout.splitlines()[0]
    assert usage.startswith('Usage: '), usage
    argv = [verb, *['gw', 'tok', 'x'][:n], 'extra9']
    before = state_files(box.state)
    result = box.run(*argv)
    assert result.returncode == 2, result.stderr
    assert "unexpected argument 'extra9'" in result.stderr
    assert 'usage: ' + usage[len('Usage: '):] in result.stderr
    assert state_files(box.state) == before
    assert box.active == 'tok'


WRONG_ARGUMENT = [
    ['models', 'gw', '--bogus'], ['doctor', 'gw', 'tok'], ['quota', 'gw', 'tok'],
    ['get', 'gw', 'bogus'], ['set', 'gw', 'bogus'], ['config', 'bad/name'],
    ['login', 'bad/name'], ['inspect', 'bad/name'], ['hop', 'bad/name'],
    ['cred', 'bad/name'], ['mv', 'gw'], ['context', 'bad/name'], ['rm'],
]


@pytest.mark.parametrize('argv', WRONG_ARGUMENT, ids=lambda a: ' '.join(a))
def test_a_wrong_argument_names_the_help_pages_usage(box, argv):
    # Every rejected shape prints the same usage line its help page does, so
    # the correction cannot drift from the documentation.
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    page = box.run('help', argv[0])
    assert page.returncode == 0, page.stderr
    usage = page.stdout.splitlines()[0]
    assert usage.startswith('Usage: '), usage
    before = state_files(box.state)
    result = box.run(*argv)
    assert result.returncode != 0, (result.stdout, result.stderr)
    assert 'usage: ' + usage[len('Usage: '):] in result.stderr, result.stderr
    assert state_files(box.state) == before
