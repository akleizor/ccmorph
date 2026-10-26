"""Regression tests for user-facing behaviour no other suite pinned down.

Covers the profile lifecycle commands (use, list, mv, rm, login), the launch
guards of `ccproteus run`, and the credential environment a launch hands Claude
Code per profile type. Everything runs the real `ccproteus` executable against an
isolated HOME with a stub `claude`; nothing touches the network or a real home.
"""
import os
import stat
import subprocess
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


class Box:
    def __init__(self, home):
        self.home = home
        self.state = home / '.config/ccproteus'
        (self.state / 'profiles').mkdir(parents=True)
        (home / 'bin').mkdir()
        (home / '.claude/projects').mkdir(parents=True)
        claude = home / 'bin/claude'
        claude.write_text(
            '#!/bin/sh\n'
            '# `claude --help` is how ccproteus discovers subcommands; not a launch.\n'
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
                    and not k.startswith(('CCPROTEUS', '_CCPROTEUS', 'CLAUDE_', 'ANTHROPIC_'))}
        self.env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / '.config'),
                        CCPROTEUS_STATE_DIR=str(self.state), CCPROTEUS_LIB_DIR=str(ROOT / "lib"),
                        PATH=f'{home}/bin:{os.environ["PATH"]}', LANG='C.UTF-8', LC_ALL='C.UTF-8',
                        NO_COLOR='1')

    def conf(self, name, text):
        (self.state / f'{name}.conf').write_text(text)

    def run(self, *args, answer=None):
        return subprocess.run([str(ROOT / "bin/ccproteus"), *args], env=self.env, input=answer or '',
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
    out = box.run('list').stdout
    rows = {line.split()[1] if line.lstrip().startswith('*') else line.split()[0]: line
            for line in out.splitlines() if line.strip()}
    assert rows['tok'].lstrip().startswith('*') and '(token)' in rows['tok']
    assert not rows['work'].lstrip().startswith('*')
    assert 'sub: me@example.invalid' in rows['work']
    assert 'apikey: https://gw.example.invalid' in rows['gw']
    assert 'canary' not in out


def test_list_with_no_profiles_points_at_add(box):
    out = box.run('list')
    assert out.returncode == 0 and 'ccproteus add' in out.stdout


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

def test_login_is_refused_for_a_profile_that_is_not_provider_backed(box):
    seed(box)
    out = box.run('login', 'gw')
    assert out.returncode != 0 and 'not a provider profile' in out.stderr
    gone = box.run('login', 'ghost')
    assert gone.returncode != 0 and "no profile 'ghost'" in gone.stderr


# --- run guards ------------------------------------------------------------------

def test_run_with_no_default_and_no_profile_launches_nothing(box):
    seed(box)
    out = box.run('run')
    assert out.returncode != 0 and 'no default profile set' in out.stderr
    assert box.log() == ''


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
    for name, why in (('empty', 'has no token'), ('anon', 'has no email'),
                      ('odd', 'invalid or missing type')):
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
    # GH-321: `-a` after the profile is claude's value, not ccproteus's flag.
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
        assert f"unknown command '{old}'. Run: ccproteus {new} tok" in out.stderr, (old, out.stderr)
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
