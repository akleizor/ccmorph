"""A launch nested in a bridge session keeps the session's route flags (issue #1).

ccproteus clears every profile-declared environment key before a launch, so one
session's value never leaks into a nested one. A route's behavioural flags describe
the ENDPOINT, though, and a Remote Control session exports its edge's loopback proxy
as `HTTPS_PROXY`, so a nested launch still reaches that route; with the flags cleared
it got `400 tools.12.advisor_20260301.model: Field required`.

The seam is one variable, CCPROTEUS_EDGE_ROUTE_ENV, that ccproteus-rc's `session_env`
sets on the session (the EFFECTIVE flags, adapter defaults included, so a route
whose conf declares no env.* lines still carries them) and that `ccproteus run`
re-applies under a loopback proxy. These tests build that environment with the
add-on's real `resolve_route` and `session_env`, then run the real `ccproteus`
against an isolated HOME with a stub `claude`. Nothing touches the network.
"""
import importlib.machinery
import importlib.util
import json
import os
import stat
import subprocess
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def _load_rc():
    loader = importlib.machinery.SourceFileLoader("ccproteus_rc_nested", str(ROOT / "addons/remote-control/ccproteus-rc"))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


class Box:
    def __init__(self, home, monkeypatch):
        self.home = home
        self.state = home / '.config/ccproteus'
        (self.state / 'profiles').mkdir(parents=True)
        (home / 'bin').mkdir()
        (home / '.claude/projects').mkdir(parents=True)
        claude = home / 'bin/claude'
        claude.write_text(
            '#!/bin/sh\n'
            '[ "$1" = --help ] && exit 0\n'
            '{ printf "ARGS"; for a do printf " [%s]" "$a"; done; echo\n'
            '  echo "DISABLE_ADVISOR=${CLAUDE_CODE_DISABLE_ADVISOR_TOOL-<unset>}"\n'
            '  echo "MODEL_CAPS=${CLAUDE_CODE_MODEL_CAPABILITIES-<unset>}"\n'
            '  echo "PROXY=${HTTPS_PROXY-<unset>}"\n'
            '  echo "CUSTOM_HEADERS=${ANTHROPIC_CUSTOM_HEADERS-<unset>}"\n'
            '} >> "$HOME/claude.log"\n')
        claude.chmod(claude.stat().st_mode | stat.S_IXUSR)
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith(('CCPROTEUS', '_CCPROTEUS', 'CLAUDE_', 'ANTHROPIC_'))
                    and k not in ('HTTPS_PROXY', 'https_proxy', 'NO_PROXY')}
        self.env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / '.config'),
                        CCPROTEUS_STATE_DIR=str(self.state), CCPROTEUS_LIB_DIR=str(ROOT),
                        PATH=f'{home}/bin:{os.environ["PATH"]}', LANG='C.UTF-8', LC_ALL='C.UTF-8',
                        NO_COLOR='1')
        (self.state / 'alex.conf').write_text('type=sub\nemail=me@example.invalid\n')
        # The route: a conforming gateway profile, no env.* lines at all.
        (self.state / 'gateway.conf').write_text(
            'type=apikey\nbase_url=https://gw.example.invalid/\napi_key=route-key\nadapter=true\n')
        for k, v in self.env.items():
            monkeypatch.setenv(k, v)
        self.rc = _load_rc()

    def session(self, route_env=None):
        """The environment ccproteus-rc gives a bridge session on `gateway`."""
        route_env = self.rc.resolve_route('gateway') if route_env is None else route_env
        base = dict(self.env)
        env = self.rc.session_env(base, route_env, route_models=False)
        env.update(HTTPS_PROXY='http://127.0.0.1:47670', NO_PROXY='localhost,127.0.0.1,::1')
        return env

    def run(self, env, *args):
        return subprocess.run([str(ROOT / 'ccproteus'), *args], env=env, input='',
                              capture_output=True, text=True, timeout=HANG_TIMEOUT)

    def log(self):
        p = self.home / 'claude.log'
        return p.read_text() if p.exists() else ''


@pytest.fixture
def box(tmp_path, monkeypatch):
    return Box(tmp_path, monkeypatch)


def test_the_route_conf_declares_no_flag_itself(box):
    # The premise: a conforming route's resolved env carries neither flag, which is
    # why an edge record (built from it) cannot be the source.
    route = box.rc.resolve_route('gateway')
    assert 'CLAUDE_CODE_DISABLE_ADVISOR_TOOL' not in route
    assert 'CLAUDE_CODE_MODEL_CAPABILITIES' not in route


def test_a_nested_launch_keeps_the_session_route_flags(box):
    """The reproduced failure: `ccproteus run alex -- -p …` inside a session on a
    route must not reach the route without the session's advisor opt-out."""
    out = box.run(box.session(), 'run', 'alex', '--', '-p', 'Reply ok')
    assert out.returncode == 0, out.stderr
    log = box.log()
    assert 'DISABLE_ADVISOR=1' in log, log
    assert f'MODEL_CAPS={box.rc.ADAPTER_MODEL_CAPABILITIES}' in log, log
    assert 'PROXY=http://127.0.0.1:47670' in log, log


def test_a_nested_launch_honours_a_route_that_serves_the_advisor(box):
    env = box.session({'CLAUDE_CODE_DISABLE_ADVISOR_TOOL': '0'})
    out = box.run(env, 'run', 'alex', '--', '-p', 'ok')
    assert out.returncode == 0, out.stderr
    assert 'DISABLE_ADVISOR=0' in box.log()


def test_a_plain_launch_still_clears_an_inherited_flag(box):
    # No loopback proxy: an ambient flag is some outer session's stale value.
    env = box.session()
    del env['HTTPS_PROXY']
    out = box.run(env, 'run', 'alex', '--', '-p', 'ok')
    assert out.returncode == 0, out.stderr
    log = box.log()
    assert 'DISABLE_ADVISOR=<unset>' in log and 'MODEL_CAPS=<unset>' in log


def test_a_remote_proxy_is_not_a_local_bridge(box):
    env = box.session()
    env['HTTPS_PROXY'] = 'http://proxy.example.invalid:3128'
    out = box.run(env, 'run', 'alex', '--', '-p', 'ok')
    assert out.returncode == 0, out.stderr
    assert 'DISABLE_ADVISOR=<unset>' in box.log()


def test_a_profile_with_its_own_endpoint_is_left_alone(box, tmp_path):
    # The proxy only reroutes api.anthropic.com; a profile dialing its own base_url
    # is not what the session's route flags describe. A stub adapter stands in for
    # the real one: the profile is launched through ANTHROPIC_BASE_URL=<loopback>.
    lib = tmp_path / 'lib'
    lib.mkdir()
    for entry in ROOT.iterdir():
        if entry.name not in ('ccproteus-adapter', '.git', '.claude'):
            (lib / entry.name).symlink_to(entry)
    stub = lib / 'ccproteus-adapter'
    stub.write_text('#!/bin/sh\necho PORT=1\nsleep 30\n')
    stub.chmod(0o755)
    (box.state / 'gw2.conf').write_text(
        'type=apikey\nbase_url=https://other.example.invalid\napi_key=k\n')
    # The session's route serves the advisor (0); this profile's own adapter default
    # is 1, and the edge's value must not replace it.
    env = box.session({'CLAUDE_CODE_DISABLE_ADVISOR_TOOL': '0'})
    env['CCPROTEUS_LIB_DIR'] = str(lib)
    out = box.run(env, 'run', 'gw2', '--', '-p', 'ok')
    assert out.returncode == 0, out.stderr
    assert 'DISABLE_ADVISOR=1' in box.log(), box.log()


@pytest.mark.parametrize('url', ['https://api.anthropic.com', 'https://api.anthropic.com/'])
def test_a_profile_with_the_native_anthropic_url_keeps_the_route_flags(box, url):
    # base_url=api.anthropic.com is native: it still dials Anthropic, so the edge
    # proxy reroutes it and the session's flags apply.
    (box.state / 'gw2.conf').write_text(f'type=apikey\nbase_url={url}\napi_key=k\n')
    out = box.run(box.session(), 'run', 'gw2', '--', '-p', 'ok')
    assert out.returncode == 0, out.stderr
    assert 'DISABLE_ADVISOR=1' in box.log(), box.log()


def test_a_profileless_nested_launch_keeps_the_route_flags(box):
    env = box.session()
    env.pop('CLAUDE_CODE_DISABLE_ADVISOR_TOOL', None)
    env['CCPROTEUS_EDGE_ROUTE_ENV'] = 'CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1'
    out = subprocess.run(
        ['zsh', '-c', 'source "$CCPROTEUS_LIB_DIR/ccproteus.sh" && _ccs_launch -p ok'],
        env=env, input='', capture_output=True, text=True, timeout=HANG_TIMEOUT)
    assert out.returncode == 0, out.stderr
    assert 'DISABLE_ADVISOR=1' in box.log(), (box.log(), out.stderr)


def _stub_adapter_lib(tmp_path):
    """A lib view whose `ccproteus-adapter` is a stub, so a nested ADAPTER launch
    starts no real translator; everything else is the real library."""
    lib = tmp_path / 'lib'
    lib.mkdir()
    for entry in ROOT.iterdir():
        if entry.name not in ('ccproteus-adapter', '.git', '.claude'):
            (lib / entry.name).symlink_to(entry)
    stub = lib / 'ccproteus-adapter'
    stub.write_text('#!/bin/sh\necho PORT=1\nsleep 30\n')
    stub.chmod(0o755)
    return lib
