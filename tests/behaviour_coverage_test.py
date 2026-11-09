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
            '  echo "PROFILE=${CCMORPH_PROFILE-<unset>}"\n'
            '} >> "$HOME/claude.log"\n')
        claude.chmod(claude.stat().st_mode | stat.S_IXUSR)
        self.env = {k: v for k, v in os.environ.items()
                    if k != 'CLAUDECODE'
                    and not k.startswith(('CCMORPH', '_CCMORPH', 'CLAUDE_', 'ANTHROPIC_'))}
        self.env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / '.config'),
                        CCMORPH_STATE_DIR=str(self.state), CCMORPH_LIB_DIR=str(ROOT / "lib"),
                        PATH=f'{home}/bin:{os.environ["PATH"]}', LANG='C.UTF-8', LC_ALL='C.UTF-8',
                        NO_COLOR='1')
        # `add` offers a pasted credential to its endpoint before saving it, and
        # a launch's sizing fetches the models.dev catalogue; point both at a
        # port nothing owns so the box stays offline and every answer is the
        # deterministic one (setup_home does the same for the zsh suite).
        self.env.update(HTTP_PROXY='http://127.0.0.1:1', HTTPS_PROXY='http://127.0.0.1:1',
                        http_proxy='http://127.0.0.1:1', https_proxy='http://127.0.0.1:1')
        self.env.pop('NO_PROXY', None)
        self.env.pop('no_proxy', None)

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


@pytest.mark.parametrize('verb', ['use', 'rm', 'run'])
def test_a_wrong_case_name_is_refused_with_the_real_spelling_gh_259(box, verb):
    # GH-259: on a case-insensitive volume every conf open succeeded under the
    # wrong case, so use rewrote the default to TOK, rm deleted gw2, and run
    # launched with .../profiles/TOK.
    seed(box)
    box.run('use', 'work')
    done = box.run(verb, 'TOK')
    assert done.returncode != 0, done.stdout
    assert "no profile 'TOK'" in done.stderr
    assert "'tok'" in done.stderr, done.stderr
    assert 'Removed' not in done.stdout and 'Default set' not in done.stdout
    assert 'type=token' in (box.state / 'tok.conf').read_text()
    assert box.active == 'work'
    assert box.log() == ''


def test_a_default_saved_with_the_wrong_case_still_marks_the_real_profile_gh_259(box):
    # A ccmorph before this fix wrote the typed spelling into `active`; reading
    # it as the profile that is really there restores the person's default.
    seed(box)
    box.run('use', 'tok')
    (box.state / 'active').write_text('TOK\n')
    assert box.run('use').stdout.strip() == 'tok'
    out = box.run('list').stdout
    tok = next(line for line in out.splitlines() if line.strip().endswith('(Claude token)'))
    assert tok.lstrip().startswith('*'), out
    # doctor with no name reads the same default, not the spelling TOK.
    doctor = box.run('doctor')
    assert doctor.returncode == 0, doctor.stdout
    assert 'profile: tok (Claude token)' in doctor.stdout, doctor.stdout


def test_doctor_refuses_a_wrong_case_name_with_the_real_spelling_gh_259(box):
    # GH-259: doctor opened tok.conf as TOK and could end with
    # `ready: start it with ccmorph run TOK`, a command that now refuses TOK.
    seed(box)
    box.run('use', 'tok')
    done = box.run('doctor', 'TOK')
    assert done.returncode != 0, done.stdout
    assert "no profile 'TOK'" in done.stdout
    assert "'tok'" in done.stdout, done.stdout
    assert 'profile: TOK' not in done.stdout
    assert 'ready: start it with ccmorph run TOK' not in done.stdout


def test_list_marks_the_default_and_labels_every_profile_type(box):
    seed(box)
    box.run('use', 'tok')
    box.conf('priv', 'type=token\ntoken=t\nprivate=1\n')
    out = box.run('list').stdout
    rows = {line.split()[1] if line.lstrip().startswith('*') else line.split()[0]: line
            for line in out.splitlines() if line.strip()}
    assert rows['tok'].lstrip().startswith('*') and '(Claude token)' in rows['tok']
    assert not rows['work'].lstrip().startswith('*')
    assert 'Claude account: me@example.invalid' in rows['work']
    assert 'API key: https://gw.example.invalid' in rows['gw']
    for internal in ('sub:', '(token)', 'apikey:'):
        assert internal not in out, 'list must not show the file spelling'
    assert 'canary' not in out
    assert 'private' in rows['priv'] and 'private' not in rows['tok']


def test_doctor_names_the_kind_and_the_endpoint(box):
    seed(box)
    (box.state / 'profiles/tok').mkdir()
    out = box.run('doctor', 'tok')
    assert out.returncode == 0, out.stderr
    assert 'profile: tok (Claude token)' in out.stdout
    box.conf('bad', 'type=apikey\napi_key=canary-key\nbase_url=gateway.invalid\n')
    out = box.run('doctor', 'bad')
    assert '(API key)' in out.stdout
    assert 'endpoint must be an http(s) URL' in out.stdout
    assert 'base_url' not in out.stdout


def test_add_done_messages_use_display_names(box):
    out = box.run('add', 't1', answer='2\nsk-ant-oat01-pasted-example-fixture-not-real-00000\n')
    assert out.returncode == 0, out.stderr
    assert 'Added t1 (Claude token)' in out.stdout
    out = box.run('add', 'k1', answer='3\ncustom\n\nsecret-key\n')
    assert out.returncode == 0, out.stderr
    assert 'Added k1 (API key → api.anthropic.com)' in out.stdout
    asked = out.stdout + out.stderr
    assert 'Endpoint (blank = api.anthropic.com): ' in asked
    assert 'Base URL' not in asked


def test_add_menu_names_each_kind_and_explains_a_provider_login(box):
    fake = box.home / 'bin/cliproxyapi'
    fake.write_text('#!/bin/sh\n'
                    'if [ "$1" = --help ]; then for p in claude codex; do echo "  -$p-login"; done; exit 0; fi\n'
                    'exit 1\n')
    fake.chmod(0o755)
    box.env['PATH'] = f'{box.home}/bin:/usr/bin:/bin'
    out = box.run('add', 'p1', answer='x\n')
    asked = out.stdout + out.stderr
    assert 'Claude token' in asked
    assert 'For scripts/CI; quota cannot read plan usage' in asked
    # Issue #283: the row names the product it signs in to, not just the bridge.
    assert 'Browser sign-in to ChatGPT (Codex), through cliproxyapi' in asked
    assert 'Claude: sign in to that service' not in asked


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

def test_add_refuses_a_name_that_differs_only_by_case_and_names_the_clash_gh_259(box):
    seed(box)
    names = sorted(p.name for p in box.state.glob('*.conf'))
    before = (box.state / 'work.conf').read_text()
    done = box.run('add', 'Work')
    assert done.returncode != 0, done.stdout
    assert "'work'" in done.stderr, done.stderr
    assert 'ccmorph rm' not in done.stderr, done.stderr
    assert sorted(p.name for p in box.state.glob('*.conf')) == names
    assert (box.state / 'work.conf').read_text() == before


def test_mv_carries_conf_profile_dir_auth_and_default_to_the_new_name(box):
    seed(box)
    box.run('use', 'work')
    (box.state / 'profiles/work').mkdir(exist_ok=True)
    (box.state / 'profiles/work/settings.json').write_text('{"keep":true}')
    auth = box.state / 'auth'
    auth.mkdir(exist_ok=True)
    (auth / '.work.bridge').write_text('handle')
    (auth / '.work.bridge.lock').write_text('')
    (auth / '.work.bridge.users').write_text('')
    done = box.run('mv', 'work', 'job')
    assert done.returncode == 0, done.stderr
    assert not (box.state / 'work.conf').exists()
    assert 'email=me@example.invalid' in (box.state / 'job.conf').read_text()
    assert (box.state / 'profiles/job/settings.json').read_text() == '{"keep":true}'
    assert not (box.state / 'profiles/work').exists()
    assert box.active == 'job'
    assert (auth / '.job.bridge').read_text() == 'handle'
    assert (auth / '.job.bridge.lock').exists() and (auth / '.job.bridge.users').exists()
    assert not list(auth.glob('.work.bridge*'))


def test_mv_refuses_a_case_only_rename_gh_259(box):
    # GH-259: mv tok Tok never returned — --locked flocked the one lock file
    # .locks/work2.lock twice. It is refused before any lock is taken.
    seed(box)
    names = sorted(p.name for p in box.state.glob('*.conf'))
    before = (box.state / 'tok.conf').read_text()
    done = box.run('mv', 'tok', 'Tok')
    assert done.returncode != 0, done.stdout
    assert 'only by letter case' in done.stderr, done.stderr
    assert sorted(p.name for p in box.state.glob('*.conf')) == names
    assert (box.state / 'tok.conf').read_text() == before


def test_mv_refuses_a_target_that_differs_only_by_case_from_another_profile_gh_259(box):
    seed(box)
    done = box.run('mv', 'work', 'TOK')
    assert done.returncode != 0, done.stdout
    assert "'tok'" in done.stderr, done.stderr
    assert (box.state / 'work.conf').exists()
    assert 'type=token' in (box.state / 'tok.conf').read_text()


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
    auth = box.state / 'auth'
    auth.mkdir(exist_ok=True)
    for f in ('.gw.bridge', '.gw.bridge.lock', '.gw.bridge.users', '.tok.bridge'):
        (auth / f).write_text('x')
    done = box.run('rm', 'gw')
    assert done.returncode == 0, done.stderr
    assert not (box.state / 'gw.conf').exists()
    assert not (box.state / 'profiles/gw').exists()
    assert box.active is None
    assert (box.state / 'tok.conf').exists()
    assert 'gw' not in box.run('list').stdout
    assert not list(auth.glob('.gw.bridge*'))
    assert (auth / '.tok.bridge').exists()


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
    # login still refuses what it cannot serve: a token profile has no sign-in
    # to renew, so the refusal names the command that replaces its credential.
    bad = box.run('login', 'tok')
    assert bad.returncode != 0 and 'ccmorph config tok' in bad.stderr
    gone = box.run('login', 'ghost')
    assert gone.returncode != 0 and "no profile 'ghost'" in gone.stderr


def test_login_signs_a_signed_out_account_profile_in_again(box):
    # GH-256: cred pointed at `ccmorph login` for an account profile and login
    # refused it — a circle with no working command. login runs the browser
    # sign-in on the profile's existing home and records the account it lands
    # on, the same readback `add` performs on a fresh profile.
    seed(box)
    box.stub_oauth_claude('sub@example.invalid')
    out = box.run('login', 'work')
    assert out.returncode == 0, out.stderr
    assert 'Signed in to Claude as sub@example.invalid' in out.stdout
    assert (box.state / 'work.conf').read_text() == 'type=sub\nemail=sub@example.invalid\n'
    # The circle's other half: cred's refusal names the command that works.
    refused = box.run('cred', 'work')
    assert refused.returncode == 1 and 'ccmorph login work' in refused.stderr


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
    first = box.run('add', 't1', answer='token\nsk-ant-oat01-first-added-profile-fixture-not-real-00000')
    assert first.returncode == 0, first.stderr
    assert box.active == 't1'
    second = box.run('add', 't2', answer='token\nsk-ant-oat01-second-added-profile-fixture-not-real-0000\n')
    assert second.returncode == 0, second.stderr
    assert box.active == 't1'


def test_add_token_prompt_names_where_a_token_comes_from_issue_290(box):
    # GH-290: the token prompt asked for a paste and never said where a token
    # comes from or what a valid one starts with, so a person without one hit a
    # dead end that no other screen in ccmorph opened a way out of.
    done = box.run('add', 'tk1', answer='token\nsk-ant-oat01-pasted-example-fixture-not-real-00000\n')
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


def test_a_running_session_names_its_profile_in_env_and_status_line(box):
    # #275: a person with a work subscription and a personal key must be able to
    # tell from inside the session which one serves it. The session's environment
    # carries the name, and the settings a launch projects render it in the status
    # line Claude Code draws on its own. An ambient value from an outer session
    # must never survive into the launch.
    seed(box)
    box.env['CCMORPH_PROFILE'] = 'ambient-must-not-leak'
    out = box.run('run', 'work')
    assert out.returncode == 0, out.stderr
    log = box.log()
    assert 'PROFILE=work\n' in log
    assert 'ambient-must-not-leak' not in log
    settings = json.loads((box.state / 'profiles/work/settings.json').read_text())
    assert settings['statusLine']['command'] == "printf 'profile: work'"


# --- the running session's one-word answer -------------------------------------------

def test_profile_answers_from_inside_a_session(box):
    # #275: one short command names the profile the session runs on.
    seed(box)
    box.env.update(CCMORPH_PROFILE='work', CLAUDE_CONFIG_DIR=str(box.state / 'profiles/work'))
    out = box.run('profile')
    assert (out.returncode, out.stdout) == (0, 'work\n'), out.stderr


def test_profile_names_the_config_dir_of_an_older_launch(box):
    # A session of a ccmorph older than CCMORPH_PROFILE still names its config
    # dir, which sits under the launcher's profile dirs.
    seed(box)
    box.env['CLAUDE_CONFIG_DIR'] = str(box.state / 'profiles/work')
    out = box.run('profile')
    assert (out.returncode, out.stdout) == (0, 'work\n'), out.stderr


def test_profile_outside_a_session_names_nothing(box):
    seed(box)
    out = box.run('profile')
    assert out.returncode == 1
    assert 'not inside a ccmorph session' in out.stderr


def test_profile_refuses_arguments(box):
    out = box.run('profile', 'extra')
    assert out.returncode == 2
    assert 'usage: ccmorph profile' in out.stderr


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


# A guessed or mistyped command names the real one, and the profile list stays
# for a word that could be a profile (#266).
@pytest.mark.parametrize('argv, hint', [
    (('ls',), 'Run: ccmorph list'),
    (('lst',), 'Run: ccmorph list'),
    (('remove', 'gw'), 'Run: ccmorph rm gw'),
    (('delete', 'gw'), 'Run: ccmorph rm gw'),
    (('rename', 'a', 'b'), 'Run: ccmorph mv a b'),
    (('switch', 'gw'), 'Run: ccmorph use gw'),
], ids=['ls-issue-266', 'lst-issue-266', 'remove-issue-266',
        'delete-issue-266', 'rename-issue-266', 'switch-issue-266'])
def test_a_guessed_command_names_the_real_one_and_prints_no_profile_list(box, argv, hint):
    seed(box)
    out = box.run(*argv)
    assert out.returncode == 1, out.stderr
    assert f"unknown command '{argv[0]}'" in out.stderr
    assert hint in out.stderr, out.stderr
    assert 'profiles:' not in out.stderr
    assert box.log() == '' and box.active is None


@pytest.mark.parametrize('flag, near', [
    ('--versoin', '--version'),
    ('--helpp', '--help'),
], ids=['versoin-issue-266', 'helpp-issue-266'])
def test_a_mistyped_flag_is_an_unknown_option_not_a_command(box, flag, near):
    seed(box)
    out = box.run(flag)
    assert out.returncode == 1, out.stderr
    assert f"unknown option '{flag}'" in out.stderr
    assert f'Run: ccmorph {near}' in out.stderr
    assert 'go after the verb' not in out.stderr
    assert 'profiles:' not in out.stderr


def test_a_misplaced_launch_flag_keeps_the_pointer_behind_run(box):
    seed(box)
    out = box.run('-p', 'hi')
    assert out.returncode == 1, out.stderr
    assert "unknown option '-p'" in out.stderr
    assert 'Launch flags go after the verb: ccmorph run -p' in out.stderr
    assert 'profiles:' not in out.stderr


def test_the_profile_list_appears_only_under_a_word_that_could_be_a_profile(box):
    seed(box)
    # A word that names a profile keeps the launch pointer and the list.
    out = box.run('gw')
    assert out.returncode == 1 and "unknown command 'gw'" in out.stderr
    assert 'To launch that profile: ccmorph run gw' in out.stderr
    assert 'profiles:' in out.stderr
    # A word that could be a profile but is not one keeps the list too: it may
    # be a mistyped profile name, and the list is what spots that.
    out = box.run('no_such_profile')
    assert out.returncode == 1 and "unknown command 'no_such_profile'" in out.stderr
    assert 'To launch that profile' not in out.stderr
    assert 'profiles:' in out.stderr


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
    assert 'broken' in out.stdout and 'API key' in out.stdout
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


# --- a fresh install hears one sentence from every command -------------------

# GH-269: with no profiles, seven commands answered seven different ways. The
# sentence is now one, never indented, and each command keeps the exit status
# its own nature asks for (a report succeeds, a refused act fails).
NO_PROFILES_YET = 'No profiles yet — run: ccmorph add <name>'

FRESH_INSTALL_ARGV = [
    (['list'], 0), (['quota'], 0), (['probe'], 0),
    # a valid option is still a sweep of nothing; an invalid one stays a usage error
    (['probe', '--check'], 0), (['probe', '--declared-only'], 0), (['probe', '--model', 'm'], 0),
    (['run'], 1), (['inspect'], 1), (['config'], 1), (['models'], 1),
    (['context'], 1), (['doctor', 'nosuch'], 1),
]


@pytest.mark.parametrize('argv, code', FRESH_INSTALL_ARGV,
                         ids=[' '.join(argv) for argv, _ in FRESH_INSTALL_ARGV])
def test_a_fresh_install_hears_one_sentence_from_every_command_gh_269(box, argv, code):
    result = box.run(*argv)
    out = result.stdout + result.stderr
    lines = [ln for ln in out.splitlines() if NO_PROFILES_YET in ln]
    assert lines, out
    assert all(not ln.startswith((' ', '\t')) for ln in lines), lines
    assert result.returncode == code, out
    # The private wordings the issue lists are gone.
    for gone in ('No profiles. Run:', 'no saved profiles.', 'no compatible profiles',
                 'profiles tried: )', 'Nothing discovered'):
        assert gone not in out, gone


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
