"""A launcher from an earlier release keeps a live session working across an update.

`ccmorph update` replaces libexec/ccmorph-core under running sessions, and every
hop or resume re-execs the on-disk core by path. A v0.4.3 or v0.4.4 launcher still
sends the argv those releases used, so the core must keep reading it: the first
switch after an update must not end the session (#137).

A v0.4.6 launcher execs the on-disk core beyond those four verbs: every util call
that carries an env, and every verb in its ownsProcess set, leaves the process
(#162). Those argv shapes are read here too.
"""
import json
import os
import signal
import stat
import subprocess
import threading

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


def test_a_v046_launcher_still_saves_its_settings_and_stops_its_bridge(box):
    state, env, seen = box
    layer = state / 'profiles' / 'k'
    layer.mkdir()
    canonical = state / 'settings.json'
    canonical.write_text('{}')

    # config-store prepare-launch: the store's five operands, the settings-compose
    # argv newline-joined in _CCM_MATERIALIZE (v0.4.6's settingsArgv("materialize")).
    materialize = '\n'.join(['materialize', str(layer), '--canonical', str(canonical), '--acw', 'none'])
    saved = core({**env, '_CCM_MATERIALIZE': materialize}, 'config-store', 'prepare-launch',
                 str(state / 'k.conf'), str(layer / 'settings.profile.json'), '', str(canonical))
    assert saved.returncode == 0, saved.stderr
    generated = json.loads((layer / 'settings.json').read_text())
    assert isinstance(generated, dict)

    # bridge-stop: pid, the rendered config dir, the auth dir.
    bridge = subprocess.Popen(['/bin/sleep', '30'])
    threading.Thread(target=bridge.wait, daemon=True).start()
    tmpdir = state / 'bridge'
    (tmpdir / 'sub').mkdir(parents=True)
    (tmpdir / 'sub' / 'config.json').write_text('{}')
    auth = state / 'auth'
    auth.mkdir()
    (auth / 'a.json').write_text('{}')
    (auth / 'a.json').chmod(0o644)
    stopped = core(env, 'bridge-stop', str(bridge.pid), str(tmpdir), str(auth))
    assert stopped.returncode == 0, stopped.stderr
    assert bridge.wait(timeout=HANG_TIMEOUT) == -signal.SIGTERM
    assert not tmpdir.exists()
    assert stat.S_IMODE(os.stat(auth / 'a.json').st_mode) == 0o600


def test_a_v046_launcher_still_lists_models_and_projects_the_picker(box):
    state, env, seen = box
    listing = '{"data":[{"id":"glm-5.2","display_name":"GLM 5.2"}]}'
    curl = (state.parent / 'bin' / 'curl')
    curl.write_text('#!/bin/sh\n'
                    'cat > "$SEEN/curl-config"\n'
                    'printf "%s\\n" "$@" > "$SEEN/curl-argv"\n'
                    'printf %s "$LISTING"\n')
    curl.chmod(0o755)
    env = {**env, 'LISTING': listing}

    # fetch-listing <base> <mode> [max-time], the key on stdin.
    launch = core(env, 'fetch-listing', 'https://gw.example.invalid', 'launch', '8', stdin=PROFILE_KEY + '\n')
    assert launch.returncode == 0, launch.stderr
    assert launch.stdout == listing
    assert '--max-time\n8\n' in (seen / 'curl-argv').read_text()
    assert PROFILE_KEY in (seen / 'curl-config').read_text()
    assert PROFILE_KEY not in (seen / 'curl-argv').read_text()

    models = core(env, 'fetch-listing', 'https://gw.example.invalid', 'models', stdin=PROFILE_KEY + '\n')
    assert models.returncode == 0, models.stderr
    assert models.stdout == listing

    # project-picker <home> <conf> <own> <tag> <profile> <layer> <launch>, the
    # listing in _CCM_MODELS_JSON.
    home = state / 'profiles' / 'k'
    home.mkdir()
    (home / '.claude.json').write_text('{}')
    (home / 'settings.json').write_text('{"availableModels":[]}')
    picked = core({**env, '_CCM_MODELS_JSON': listing}, 'project-picker', str(home), str(state / 'k.conf'),
                  '', 'Gateway', 'k', str(home / 'settings.profile.json'), '')
    assert picked.returncode == 0, picked.stderr
    assert '"label": "glm-5.2"' in (home / '.claude.json').read_text()


def test_a_v046_launcher_still_gets_its_launch_diagnostics_and_advisories(box):
    state, env, seen = box

    # advise-json: _CCM_ADVISE as the SessionStart hook's systemMessage.
    advised = core({**env, '_CCM_ADVISE': 'ccmorph: x'}, 'advise-json')
    assert advised.returncode == 0, advised.stderr
    assert advised.stdout == '{"systemMessage":"ccmorph: x"}\n'

    # hazard-set: rows for the switch advisory, its operands all in the env.
    hazard = core({**env, '_CCM_PROFILE': 'k', '_CCM_PROFILE_CONF': str(state / 'k.conf'), '_CCM_SOURCES': ''},
                  'hazard-set')
    assert hazard.returncode == 0, hazard.stderr

    # hazard-msg: those rows join into the one stdout line.
    message = core({**env, '_CCM_ROWS': ''}, 'hazard-msg')
    assert message.returncode == 0, message.stderr
    assert message.stdout == ''

    # launch-diagnostics <keys...> -- <sources...>: NUL-framed records.
    layer = state / 'layer.json'
    layer.write_text('{"env":{"ANTHROPIC_MODEL":"glm-5.2"}}')
    diag = core({**env, '_CCM_PCT_IN_ENV': '1'}, 'launch-diagnostics', 'ANTHROPIC_MODEL', '--', str(layer))
    assert diag.returncode == 0, diag.stderr
    assert 'chosen\tANTHROPIC_MODEL\x00' in diag.stdout

    # withhold-fatal: the fatal row leaves availableModels, the launched one stays.
    generated = state / 'generated.json'
    generated.write_text('{"availableModels":["a","b"]}')
    withheld = core({**env, '_CCM_SETTINGS': str(generated), '_CCM_ROWS': 'x\tb', '_CCM_LAUNCHED': 'a'},
                    'withhold-fatal')
    assert withheld.returncode == 0, withheld.stderr
    assert json.loads(generated.read_text())['availableModels'] == ['a']

    # listing-has-unsized <profile>: the listing on stdin.
    unsized = core({**env, 'CCMORPH_STATE_DIR': str(state)}, 'listing-has-unsized', 'k',
                   stdin='{"data":[{"id":"glm-5.2"}]}')
    assert unsized.returncode == 0, unsized.stderr

    # unwire-statusline <file>: the command _CCM_SL_OWNED names.
    wired = state / 'wired.json'
    wired.write_text('{"statusLine":{"command":"/x/statusline"}}')
    unwired = core({**env, '_CCM_SL_OWNED': '/x/statusline'}, 'unwire-statusline', str(wired))
    assert unwired.returncode == 0, unwired.stderr
    assert 'statusLine' not in json.loads(wired.read_text())

    # launch-window <profile> <model> [sources...]: six tab-separated fields.
    window = core({**env, 'CCMORPH_STATE_DIR': str(state), 'MODEL_WINDOW_PROFILE_CONF': str(state / 'k.conf')},
                  'launch-window', 'k', '')
    assert window.returncode == 0, window.stderr
    assert window.stdout.count('\t') == 5
    for run in (advised, hazard, message, diag, withheld, unsized, unwired, window):
        assert 'usage:' not in run.stderr


@pytest.mark.parametrize('stage', ['--model-known', '--carry-session', '--config-store', '--gw-models',
                                   '--listing-has-unsized', '--fold', '--prelaunch', '--remove-profile',
                                   '--models-json', '--profile-ok', '--claude-subcommand', '--resume-model',
                                   '--probe-nudge'])
def test_a_hidden_stage_without_its_operands_refuses_with_usage(box, stage):
    _, env, _ = box
    ran = subprocess.run([str(CORE), 'launch', stage], env=env, text=True,
                         capture_output=True, timeout=HANG_TIMEOUT)
    assert ran.returncode == 2
    assert ran.stderr.startswith('usage: ccmorph-core launch ' + stage)
    assert 'panic' not in ran.stderr
