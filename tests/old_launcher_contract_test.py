"""A launcher from an earlier release keeps a live session working across an update.

`ccmorph update` replaces libexec/ccmorph-core under running sessions, and every
hop or resume re-execs the on-disk core by path. A v0.4.3 or v0.4.4 launcher still
sends the argv those releases used, so the core must keep reading it: the first
switch after an update must not end the session (#137).
"""
import os
import subprocess

import pytest

from support import CORE, HANG_TIMEOUT, needs_shim

pytestmark = needs_shim

PROFILE_KEY = 'sk-profile-canary-137'
BRIDGE_KEY = 'sk-bridge-canary-137'
# The v0.4.3 launcher's env-key lists. The lists live in Go now (#130), so any CSV
# proves the two operands are accepted and ignored.
LISTS = ('ANTHROPIC_MODEL', 'ANTHROPIC_DEFAULT_OPUS_MODEL')


@pytest.fixture
def box(tmp_path):
    state = tmp_path / 'state'
    (state / 'profiles').mkdir(parents=True)
    conf = state / 'k.conf'
    conf.write_text('type=apikey\nbase_url=https://gw.example.invalid\napi_key=' + PROFILE_KEY + '\n')
    conf.chmod(0o600)
    bin_dir = tmp_path / 'bin'
    bin_dir.mkdir()
    seen = tmp_path / 'seen'
    seen.mkdir()
    claude = bin_dir / 'claude'
    claude.write_text(
        '#!/bin/sh\n'
        'printf "%s\\n" "$@" > "$SEEN/argv"\n'
        'printf "%s|%s\\n" "${ANTHROPIC_BASE_URL-}" "${ANTHROPIC_API_KEY-}" > "$SEEN/route"\n')
    claude.chmod(0o755)
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(('CCMORPH_', '_CCM', 'CLAUDE_', 'ANTHROPIC_'))}
    env.update(HOME=str(tmp_path), TMPDIR=str(tmp_path), SEEN=str(seen),
               PATH=str(bin_dir) + ':/usr/bin:/bin')
    return state, env, seen


def core(env, *args, stdin=''):
    return subprocess.run([str(CORE), 'util'] + list(args), env=env, input=stdin,
                          text=True, capture_output=True, timeout=HANG_TIMEOUT)


def test_a_launcher_from_an_earlier_release_still_starts_claude_on_its_route(box):
    state, env, seen = box
    # profile-read: a v0.4.3 launcher passes two env-key lists after <secure>.
    read = core(env, 'profile-read', str(state), 'k', '1', *LISTS)
    assert read.returncode == 0, read.stderr
    assert 'api_key\0' + PROFILE_KEY + '\0' in read.stdout

    # profile-env: the same two lists.
    penv = core(env, 'profile-env', str(state), str(state / 'profiles'), 'k', *LISTS)
    assert penv.returncode == 0, penv.stderr
    lines = penv.stdout.splitlines()
    assert lines[0] == 'type=apikey'
    assert 'ANTHROPIC_API_KEY=' + PROFILE_KEY in lines[1:]

    # launch-env: the bridge key is the second operand in v0.4.3 and v0.4.4.
    routed = core(env, 'launch-env', '9', BRIDGE_KEY, '0', '', '', '',
                  stdin='\n'.join(lines[1:]) + '\n')
    assert routed.returncode == 0, routed.stderr
    assert routed.stdout.endswith('.')
    entries = routed.stdout[:-1].split('\0')[:-1]
    assert 'ANTHROPIC_BASE_URL=http://127.0.0.1:9' in entries
    assert 'ANTHROPIC_API_KEY=' + BRIDGE_KEY in entries

    # claude-session: the launch env inline, then -- and claude's own args. This is
    # the argv a v0.4.4 launcher sends too: its util verbs run in-process, but
    # claude-session execs the on-disk core.
    started = core(env, 'claude-session', '', '', '', '', *entries, '--', '--resume', 'abc')
    assert started.returncode == 0, started.stderr
    assert (seen / 'argv').read_text() == '--resume\nabc\n'
    assert (seen / 'route').read_text() == 'http://127.0.0.1:9|' + BRIDGE_KEY + '\n'