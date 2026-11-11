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
    assert bad.returncode != 0 and 'invalid profile name' in bad.stderr
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
    # Issue #360: adding a gateway ends by naming the model question each
    # launch will ask, and the command that saves a default and ends it.
    out = box.run('add', 'gw1', answer='3\ncustom\nhttps://gw.example\nsecret-key\n')
    assert out.returncode == 0, out.stderr
    assert 'Added gw1 (API key → https://gw.example)' in out.stdout
    asked = out.stdout + out.stderr
    assert 'Each launch asks which model until one is saved' in asked
    assert 'ccmorph set gw1 model <model-id>' in asked
    # Custom can also name Anthropic's own endpoint; that profile launches
    # native, so add must not promise a model question (#360).
    out = box.run('add', 'nat1', answer='3\ncustom\nhttps://api.anthropic.com\nsecret-key\n')
    assert out.returncode == 0, out.stderr
    assert 'Added nat1 (API key → https://api.anthropic.com)' in out.stdout
    assert 'Each launch asks which model' not in out.stdout + out.stderr


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


def test_kimi_sign_ins_and_provider_list_share_one_name_per_service_gh_366(box):
    # GH-366: the sign-in methods called the two Kimi sites Kimi-ai and Kimi,
    # while the API-key provider list called the same sites Kimi For Coding
    # (global) and Kimi For Coding (China), so the lists could not be matched
    # and the plain name stood for the less obvious site. Both lists now carry
    # the same region name for each service, and the domain stays in the
    # sign-in row's explanation, not in its name.
    fake = box.home / 'bin/cliproxyapi'
    fake.write_text('#!/bin/sh\n'
                    'if [ "$1" = --help ]; then for p in claude kimi-ai kimi; do echo "  -$p-login"; done; exit 0; fi\n'
                    'exit 1\n')
    fake.chmod(0o755)
    box.env['PATH'] = f'{box.home}/bin:/usr/bin:/bin'
    sign_in = box.run('add', 'p1', answer='x\n')
    sign_in = sign_in.stdout + sign_in.stderr
    assert 'kimi.ai account' in sign_in and 'kimi.com account' in sign_in
    provider_list = box.run('add', 'p2', answer='3\nx\n')
    provider_list = provider_list.stdout + provider_list.stderr
    assert 'api.kimi.ai/coding' in provider_list and 'api.kimi.com/coding' in provider_list
    for name in ('Kimi For Coding (global)', 'Kimi For Coding (China)'):
        assert name in sign_in, f'the sign-in methods do not name {name}'
        assert name in provider_list, f'the provider list does not name {name}'
    assert 'Kimi-ai' not in sign_in, 'the kimi.ai sign-in is still named by its domain alone'


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


@pytest.mark.parametrize('name, renew', [
    ('work', 'ccmorph login work'), ('cx', 'ccmorph login cx'), ('gw', None), ('tok', None)])
def test_add_on_an_existing_name_names_config_and_rm_only_as_starting_over_gh_391(box, name, renew):
    # GH-391: add on a taken name offered only rm, which deletes the profile and
    # its credential for good. It names config, the command that changes a
    # profile; a browser sign-in is renewed by login and never hears rm.
    seed(box)
    box.conf('cx', 'type=provider\nprovider=codex\n')
    before = {p.name: p.read_text() for p in box.state.glob('*.conf')}
    done = box.run('add', name)
    assert done.returncode != 0, done.stdout
    assert f"'{name}' already exists" in done.stderr, done.stderr
    assert f'ccmorph config {name}' in done.stderr, done.stderr
    if renew:
        assert renew in done.stderr, done.stderr
        assert 'ccmorph rm' not in done.stderr, done.stderr
    else:
        assert (f'To start over instead: ccmorph rm {name} deletes the profile '
                'and its saved credential for good') in done.stderr, done.stderr
    assert {p.name: p.read_text() for p in box.state.glob('*.conf')} == before


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


@pytest.mark.parametrize('name, kind', [('work', 'Claude account'), ('tok', 'Claude token')])
def test_models_and_probe_answer_an_account_or_token_profile_alike_gh_390(box, name, kind):
    # GH-390: models named the internal key api_key and probe said "profile has
    # no api key, skipped"; neither named the kind or a command that helps.
    seed(box)
    replies = [box.run(verb, name) for verb in ('models', 'probe')]
    for out in replies:
        assert out.returncode == 1, out.stderr
        assert out.stdout == ''
        assert f"'{name}' is a {kind} profile and has no model list from an endpoint" in out.stderr
        assert f'choose its model: ccmorph set {name} model -i' in out.stderr
        assert 'api_key' not in out.stderr and 'api key' not in out.stderr
    assert replies[0].stderr == replies[1].stderr


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


@pytest.mark.parametrize('with_settings', [True, False], ids=['settings', 'no-settings'])
@pytest.mark.parametrize('name,endpoint,words', [
    ('nat', '', 'Claude Code picks the model'),
    ('gw', 'https://gw.example', 'ccmorph asks which model at each launch'),
])
def test_the_unsaved_state_reads_the_same_words_on_every_screen_gh_381(box, name, endpoint, words,
                                                                        with_settings):
    # GH-381: a profile with no saved model was worded five ways across config,
    # inspect, get and doctor, and none of the five said what a launch does.
    # Every screen now carries the same words for the state, and the words are
    # what the launch really does for that profile type: a gateway (an API key
    # on another endpoint) is asked which model at each launch (#360), every
    # other profile leaves the model to Claude Code, and no saved effort passes
    # no level, so the session starts at Claude Code's own default.
    box.conf(name, 'type=apikey\napi_key={{SECRET_4zo8c1xfyqjs}}\n'
                   + (f'base_url={endpoint}\n' if endpoint else ''))
    # A profile never launched has no settings file and is ready (#291):
    # every screen, doctor too, names the state the same way without one.
    if with_settings:
        (box.state / f'profiles/{name}').mkdir()
        (box.state / f'profiles/{name}/settings.json').write_text('{}')
    effort_words = "Claude Code's own default"
    screens = {}
    # get: stdout stays empty for scripts (#268); the sentence on stderr names
    # the state and the shared words.
    out = box.run('get', name, 'model')
    assert out.returncode == 0, out.stderr
    assert out.stdout == ''
    assert f'{name}: no model saved — {words}' in out.stderr
    screens['get'] = out.stderr
    out = box.run('get', name, 'effort')
    assert out.returncode == 0, out.stderr
    assert out.stdout == ''
    assert f'{name}: no effort saved — {effort_words}' in out.stderr
    screens['get'] += out.stderr
    # inspect: the model, effort and launches-as rows carry the shared words;
    # the gateway's model row keeps its "none saved; " lead-in.
    out = box.run('inspect', name)
    assert out.returncode == 0, out.stderr
    model_row = f'— none saved; {words}' if endpoint else f'— {words}'
    assert model_row in out.stdout
    assert f'— {effort_words}' in out.stdout
    assert f'({words})' in out.stdout
    screens['inspect'] = out.stdout
    # doctor: the effective rows answer what the session runs with.
    out = box.run('doctor', name)
    assert out.returncode == 0, out.stderr
    assert f'effective model: {words} (no saved model)' in out.stdout
    assert f'effective effort: {effort_words} (no saved effort)' in out.stdout
    screens['doctor'] = out.stdout
    # config: the prompt shows what keeping nothing means, and the summary
    # prints the shared words instead of a name for the state.
    out = box.run('config', name, answer='\n\n\n\n')
    assert out.returncode == 0, out.stderr
    assert f'Default model [none — {words}]: ' in out.stderr
    assert f'Default effort [none — {effort_words}]: ' in out.stderr
    assert f'Model: {words} · Effort: {effort_words}' in out.stderr
    screens['config'] = out.stderr
    # None of the four screens may fall back to the old drift words.
    for screen, text in screens.items():
        for old in ('Claude Code chooses', 'Claude default', 'Claude Code default'):
            assert old not in text, (screen, old)


def test_context_answers_the_window_the_session_will_use_gh_372(box):
    # GH-372: `ccmorph context <profile> <model>` answered the pin alone --
    # `auto`, or a bare number once one was set -- while inspect showed the
    # window the session would actually run at. The read now answers that
    # count in tokens with where it came from, and the number is inspect's
    # window line.
    box.conf('gw', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=claude-opus-5\n')
    out = box.run('context', 'gw', 'claude-opus-5')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'gw: claude-opus-5 context 1,000,000 tokens · estimated from the model catalogue\n'
    assert '1,000,000  catalogue-estimate' in box.run('inspect', 'gw').stdout
    # a pin reads back as set by hand, still the number inspect shows
    assert box.run('context', 'gw', 'claude-opus-5', '500000').returncode == 0
    out = box.run('context', 'gw', 'claude-opus-5')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'gw: claude-opus-5 context 500,000 tokens · set by hand\n'
    assert '500,000  manual' in box.run('inspect', 'gw').stdout
    # a model nothing sized answers the default it will run at, named as that
    out = box.run('context', 'gw', 'claude-mystery-9')
    assert out.returncode == 0, out.stderr
    assert out.stdout == "gw: claude-mystery-9 context 200,000 tokens · left at Claude Code's default\n"
    # a ceiling set by hand in the profile's env is the cap every model but a
    # claude- id launches under -- launch keeps it instead of deriving the
    # window -- so the read answers it over the pin
    box.conf('cap', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=glm-5.2\n'
                    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=300000\n'
                    'context_window.glm-5.2=500000\n')
    out = box.run('context', 'cap', 'glm-5.2')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'cap: glm-5.2 context 300,000 tokens · set by hand\n'
    # inspect's window line shows the same cap, named by the key that set it
    window = [ln for ln in box.run('inspect', 'cap').stdout.splitlines() if ln.strip().startswith('window')]
    assert len(window) == 1 and '300,000  manual (env.CLAUDE_CODE_MAX_CONTEXT_TOKENS)' in window[0], window
    # the ceiling is read as launch and Claude Code read it: 1e5 is 100,000
    box.conf('scicap', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=glm-5.2\n'
                       'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=1e5\n'
                       'context_window.glm-5.2=500000\n')
    out = box.run('context', 'scicap', 'glm-5.2')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'scicap: glm-5.2 context 100,000 tokens · set by hand\n'
    window = [ln for ln in box.run('inspect', 'scicap').stdout.splitlines() if ln.strip().startswith('window')]
    assert len(window) == 1 and '100,000  manual (env.CLAUDE_CODE_MAX_CONTEXT_TOKENS)' in window[0], window
    # a claude- id is the one model the ceiling never binds (GH-447): Claude
    # Code ignores the var for its own ids, so both surfaces answer the
    # resolver's own window -- with a pin set, that answer is the pin, and the
    # manual cap never overrides it
    box.conf('ceil', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=claude-sonnet-5-5\n'
                     'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=300000\n'
                     'context_window.claude-sonnet-5-5=500000\n')
    out = box.run('context', 'ceil', 'claude-sonnet-5-5')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'ceil: claude-sonnet-5-5 context 500,000 tokens · set by hand\n'
    # inspect's window line shows the resolver's own answer, not the cap
    window = [ln for ln in box.run('inspect', 'ceil').stdout.splitlines() if ln.strip().startswith('window')]
    assert len(window) == 1 and '500,000  manual' in window[0], window
    # a scientific-notation ceiling is ignored for a claude- id too -- never
    # parsed into the window the answer names
    box.conf('sci', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=claude-sonnet-5-5\n'
                    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=1e5\n'
                    'context_window.claude-sonnet-5-5=500000\n')
    out = box.run('context', 'sci', 'claude-sonnet-5-5')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'sci: claude-sonnet-5-5 context 500,000 tokens · set by hand\n'
    window = [ln for ln in box.run('inspect', 'sci').stdout.splitlines() if ln.strip().startswith('window')]
    assert len(window) == 1 and '500,000  manual' in window[0], window
    # with no pin, the resolver's own window is the catalogue estimate
    box.conf('own', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=claude-opus-5\n'
                    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=300000\n')
    out = box.run('context', 'own', 'claude-opus-5')
    assert out.returncode == 0, out.stderr
    assert out.stdout == 'own: claude-opus-5 context 1,000,000 tokens · estimated from the model catalogue\n'
    window = [ln for ln in box.run('inspect', 'own').stdout.splitlines() if ln.strip().startswith('window')]
    assert len(window) == 1 and '1,000,000  catalogue-estimate' in window[0], window
    # and for a claude- id nothing sized, it is Claude Code's own default, on
    # both surfaces -- inspect's window line answers the count, not an unknown
    box.conf('def', 'type=apikey\napi_key={{SECRET_0eqa8i3kouoh}}\nmodel=claude-mystery-9\n'
                    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=300000\n')
    out = box.run('context', 'def', 'claude-mystery-9')
    assert out.returncode == 0, out.stderr
    assert out.stdout == "def: claude-mystery-9 context 200,000 tokens · left at Claude Code's default\n"
    window = [ln for ln in box.run('inspect', 'def').stdout.splitlines() if ln.strip().startswith('window')]
    assert len(window) == 1 and "200,000  Claude Code's default" in window[0], window


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
    assert 'invalid profile name' not in added.stderr
    removed = box.run('rm')
    assert removed.returncode != 0
    assert 'usage: ccmorph rm <profile>' in removed.stderr


# --- a word that can never be a profile name gets one reply (#373) ----------

NAME_RULE = 'a name may hold only A-Z, a-z, 0-9 and _ (no hyphen, dot, slash or space)'
BAD_NAME_REPLIES = [
    ('a/b', "invalid profile name 'a/b': " + NAME_RULE),
    ('bad name', "invalid profile name 'bad name': " + NAME_RULE),
    ('a.b', "invalid profile name 'a.b': " + NAME_RULE),
    ('my-work', "invalid profile name 'my-work': " + NAME_RULE),
    ('日本語', "invalid profile name '日本語': " + NAME_RULE),
    ('y' * 300, "invalid profile name '" + 'y' * 32 + "…': too long; at most 200 of A-Z, a-z, 0-9 and _"),
    ('active', "invalid profile name 'active': that word is reserved; choose another"),
]
BAD_NAME_ARGV = [
    ['add', '{n}'], ['use', '{n}'], ['login', '{n}'], ['inspect', '{n}'],
    ['rm', '{n}'], ['mv', '{n}', 'zzz'], ['mv', 'gw', '{n}'], ['cred', '{n}'],
    ['run', '{n}'], ['config', '{n}'], ['get', '{n}', 'model'],
    ['set', '{n}', 'model', 'x'], ['context', '{n}', 'm'], ['context', '{n}'],
    ['probe', '{n}'], ['probe', '--profile', '{n}'], ['probe', '--profile={n}'],
    ['quota', '{n}'], ['quota', '--json', '{n}'], ['models', '{n}'],
    ['doctor', '{n}'], ['hop', '{n}'], ['_menu', '{n}'],
]


@pytest.mark.parametrize('name,reply', BAD_NAME_REPLIES,
                         ids=['slash', 'space', 'dot', 'hyphen', 'cjk', 'too long', 'reserved'])
@pytest.mark.parametrize('argv', BAD_NAME_ARGV, ids=lambda a: ' '.join(a))
def test_a_bad_name_gets_the_one_reply_from_every_command_gh_373(box, argv, name, reply):
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    before = state_files(box.state)
    result = box.run(*[a.format(n=name) for a in argv])
    assert result.returncode == 1, (result.stdout, result.stderr)
    # The reply names the words that could be the typo, as the no-profile reply
    # does (#228), so a bad name still cannot hide the profiles that exist.
    assert result.stderr == 'ccmorph: ' + reply + '. Profiles: gw tok work\n', result.stderr
    assert result.stdout == '', result.stdout
    assert state_files(box.state) == before
    assert box.log() == '', box.log()


@pytest.mark.parametrize('argv', BAD_NAME_ARGV, ids=lambda a: ' '.join(a))
def test_a_leading_hyphen_word_gets_the_one_reply_gh_373(box, argv):
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    before = state_files(box.state)
    result = box.run(*[a.format(n='-x') for a in argv])
    assert result.returncode == 1, (result.stdout, result.stderr)
    assert result.stderr == ("ccmorph: invalid profile name '-x': " + NAME_RULE
                             + '. Profiles: gw tok work\n'), result.stderr
    assert result.stdout == '', result.stdout
    assert state_files(box.state) == before
    # -x in run's profile slot must not reach a launch (the stub logs every call).
    assert box.log() == '', box.log()


# A short word led by one of Claude Code's own short options is Claude's: the
# rest is that option's value (-nTitle, -dapi) or more of its flags (-pc), so it
# passes through untouched, a profile named or not (INTENT promise 6). A lone -
# was forwarded before #373 and still is.
@pytest.mark.parametrize('argv, forwarded', [
    (('-nTitle', '-p', 'hello'), 'ARGS [-nTitle] [-p] [hello]'),
    (('tok', '-nTitle', '-p', 'hello'), 'ARGS [-nTitle] [-p] [hello]'),
    (('-dapi', '-p', 'hello'), 'ARGS [-dapi] [-p] [hello]'),
    (('-pc',), 'ARGS [-pc]'),
    (('-',), 'ARGS [-]'),
], ids=['attached value', 'attached value after a profile', 'attached optional value',
        'combined flags', 'lone dash'])
def test_a_native_short_option_with_more_attached_still_launches_gh_373(box, argv, forwarded):
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    out = box.run('run', *argv)
    assert out.returncode == 0, out.stderr
    assert 'invalid profile name' not in out.stderr, out.stderr
    assert forwarded in box.log(), box.log()


# A short option Claude Code's own help lists is Claude's too, though it is not
# one of the documented ones the stubbed runs fall back to.
def test_a_short_option_from_claudes_help_still_launches_gh_373(box):
    seed(box)
    assert box.run('use', 'tok').returncode == 0
    claude = box.home / 'bin/claude'
    claude.write_text(claude.read_text().replace(
        '[ "$1" = --help ] && exit 0\n',
        "[ \"$1\" = --help ] && printf 'Options:\\n  -q, --quiet  Hush\\n' && exit 0\n"))
    out = box.run('run', '-qx', '-p', 'hello')
    assert out.returncode == 0, out.stderr
    assert 'ARGS [-qx] [-p] [hello]' in box.log(), box.log()
    refused = box.run('run', '-x')
    assert refused.returncode == 1, refused.stderr
    assert "invalid profile name '-x'" in refused.stderr, refused.stderr


@pytest.mark.parametrize('argv', [
    ['rm', ''], ['mv', ''], ['mv', '', 'x'], ['mv', 'x', ''], ['cred', ''], ['cred', '-i'],
    ['models', ''], ['probe', ''],
], ids=lambda a: repr(a))
def test_a_left_out_name_keeps_the_usage_line_gh_373(box, argv):
    seed(box)
    page = box.run('help', argv[0])
    assert page.returncode == 0, page.stderr
    usage = page.stdout.splitlines()[0]
    assert usage.startswith('Usage: '), usage
    result = box.run(*argv)
    assert result.returncode == 1, (result.stdout, result.stderr)
    assert 'usage: ' + usage[len('Usage: '):] in result.stderr, result.stderr
    assert 'invalid profile name' not in result.stderr


@pytest.mark.parametrize('flag', ['--filtered', '--filtered-json'])
def test_models_with_only_a_mode_flag_is_a_left_out_profile_gh_373(box, flag):
    seed(box)
    result = box.run('models', flag)
    out = result.stdout + result.stderr
    assert result.returncode != 0, out
    assert 'invalid profile name' not in out
    assert 'model listing requires' not in out
    assert 'usage:' not in out


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
    ['help', 'rm', '--help'], ['run', '--help'], ['run', '-h'],
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
    ['get', 'gw', 'bogus'], ['set', 'gw', 'bogus'], ['config', 'gw', 'extra9'],
    ['login', 'gw', 'extra9'], ['inspect', 'gw', 'extra9'], ['hop', 'gw', 'extra9'],
    ['cred', 'gw', 'extra9'], ['mv', 'gw'], ['context', 'gw', 'm', 'x', 'extra9'], ['rm'],
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
