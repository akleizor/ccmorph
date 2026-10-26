#!/usr/bin/env python3
"""Optional real-Claude TUI check; isolated home, synthetic token, no inference.

Run: python3 tests/native_session_defaults_smoke.py /path/to/claude [ccproteus]
"""
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import struct
import subprocess
import sys
import tempfile
import termios
import time

ROOT = Path(__file__).resolve().parents[1]
ANSI = re.compile(r'\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*(?:\x07)|\x1b[78]')


def session(cli, home, env, commands, before_exit=None):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 140, 0, 0))
    proc = subprocess.Popen([str(cli), 'run', 'demo'], cwd=home, env=env,
                            stdin=slave, stdout=slave, stderr=slave)
    os.close(slave)
    output = b''
    text = ''
    deadline = time.monotonic() + 50
    sent = 0
    last_action = time.monotonic()
    trusted = False
    accepted = False
    try:
        while time.monotonic() < deadline and proc.poll() is None:
            if select.select([master], [], [], .2)[0]:
                try:
                    output += os.read(master, 65536)
                except OSError:
                    break
            text = ANSI.sub('', output.decode(errors='replace'))
            compact = ''.join(text.split())
            now = time.monotonic()
            if not trusted and 'Yes,Itrustthisfolder' in compact:
                # This test created the empty workspace itself.
                os.write(master, b'\x1b[B\r')
                trusted = True
                last_action = now
            elif not accepted and 'DoyouwanttousethisAPIkey?' in compact:
                os.write(master, b'\x1b[A\r')
                accepted = True
                last_action = now
            elif 'forshortcuts' in compact and now - last_action > 3:
                if sent < len(commands):
                    if commands[sent] == '/exit' and before_exit is not None:
                        before_exit()
                        before_exit = None
                        last_action = time.monotonic()
                        continue
                    os.write(master, commands[sent].encode() + b'\r')
                    sent += 1
                    last_action = now
        assert sent == len(commands), text[-6000:]
        assert proc.wait(timeout=5) == 0, text[-6000:]
        return text
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        os.close(master)


def discovery(cli, home, env, expected_model: str | None = 'sonnet',
              expected_effort: str | None = 'medium'):
    requests = [
        {'type': 'control_request', 'request_id': 'shared-discovery',
         'request': {'subtype': 'initialize'}},
        {'type': 'control_request', 'request_id': 'effective-settings',
         'request': {'subtype': 'get_settings'}},
    ]
    result = subprocess.run(
        [str(cli), 'run', 'demo', '-p', '--verbose', '--input-format', 'stream-json',
         '--output-format', 'stream-json', '--no-session-persistence'],
        input=''.join(json.dumps(request) + '\n' for request in requests), cwd=home, env=env,
        text=True, capture_output=True, timeout=40)
    assert result.returncode == 0, result.stderr
    discovered = False
    inspected = False
    for line in result.stdout.splitlines():
        try:
            message = json.loads(line)
        except ValueError:
            continue
        if message.get('type') == 'control_response':
            payload = message['response'].get('response', {})
            if message['response'].get('request_id') == 'shared-discovery':
                names = {entry['name'] for entry in payload.get('commands', [])}
                assert 'shared-discovery-probe' in names, sorted(names)
                discovered = True
            elif message['response'].get('request_id') == 'effective-settings':
                effective = payload['effective']
                assert effective.get('model') == expected_model
                assert effective.get('effortLevel') == expected_effort
                assert not effective.get('env', {}).get('ANTHROPIC_MODEL')
                assert not effective.get('env', {}).get('CLAUDE_CODE_EFFORT_LEVEL')
                assert all('effortLevel' not in row for row in
                           effective.get('modelSettings', {}).values())
                inspected = True
    assert discovered and inspected, 'Native initialization returned incomplete inspection results'


def main():
    native = Path(sys.argv[1]).resolve()
    cli = Path(sys.argv[2]).resolve() if len(sys.argv) > 2 else ROOT / "bin/ccproteus"
    with tempfile.TemporaryDirectory(prefix='ccproteus-session-defaults-') as td:
        home = Path(td).resolve()
        config = home / '.config/ccproteus'
        profile = config / 'profiles/demo'
        bindir = home / 'bin'
        for directory in (config, profile, bindir, home / '.claude'):
            directory.mkdir(parents=True)
        (bindir / 'claude').symlink_to(native)
        skill = home / '.agents/skills/shared-discovery-probe'
        skill.mkdir(parents=True)
        (skill / 'SKILL.md').write_text(
            '---\nname: shared-discovery-probe\ndescription: Verify shared skill discovery.\n---\n'
            'A synthetic skill; no inference is required.\n')
        shared_skills = home / '.claude/skills'
        shared_skills.mkdir()
        (shared_skills / skill.name).symlink_to(skill, target_is_directory=True)
        (config / 'demo.conf').write_text('type=token\ntoken=synthetic-not-a-credential\n')
        settings = profile / 'settings.json'
        settings.write_text(json.dumps({'model': 'sonnet', 'effortLevel': 'medium',
                                        'env': {'ANTHROPIC_DEFAULT_SONNET_MODEL': 'claude-sonnet-5'}}))
        (profile / '.claude.json').write_text(json.dumps({
            'hasCompletedOnboarding': True, 'theme': 'dark',
            'projects': {str(home): {'hasTrustDialogAccepted': True,
                                     'hasCompletedProjectOnboarding': True}}}))
        env = {k: v for k, v in os.environ.items()
               if not k.startswith(('CLAUDE', 'ANTHROPIC', 'CCPROTEUS', '_CCPROTEUS'))}
        env.update(HOME=str(home), ZDOTDIR=str(home), XDG_CONFIG_HOME=str(home / '.config'),
                   CCPROTEUS_STATE_DIR=str(config), CCPROTEUS_LIB_DIR=str(ROOT / "lib"),
                   CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC='1', TERM='xterm-256color',
                   PATH=f'{bindir}:' + env['PATH'])
        discovery(cli, home, env)
        initial_settings = settings.read_text()
        settings.write_text(json.dumps({
            'model': 'haiku', 'effortLevel': 'high',
            'modelSettings': {'claude-haiku-4-5': {'effortLevel': 'high'}},
            'env': {'ANTHROPIC_DEFAULT_SONNET_MODEL': 'claude-sonnet-5'},
        }))
        discovery(cli, home, env)
        settings.write_text(initial_settings)
        def concurrent_launch():
            runtime = json.loads(settings.read_text())
            assert runtime['model'] == 'haiku', 'native model choice was not saved'
            assert runtime['modelSettings']['claude-haiku-4-5']['effortLevel'] == 'high'
            discovery(cli, home, env)

        first = session(cli, home, env, ['/effort high', '/model haiku', '/effort high', '/exit'],
                        before_exit=concurrent_launch)
        compact = ''.join(first.split())
        assert 'SetmodeltoHaiku' in compact, first[-6000:]
        assert 'Seteffortleveltohigh' in compact, first[-6000:]
        for key, expected in (('model', 'sonnet'), ('effort', 'medium')):
            actual = subprocess.check_output([str(cli), 'get', 'demo', key], env=env, text=True).strip()
            assert actual == expected, (key, actual)
        discovery(cli, home, env)
        second = session(cli, home, env, ['/exit'])
        compact = ''.join(second.split())
        assert 'Sonnet5withmediumeffort' in compact, second[-6000:]
        (config / 'demo.conf').write_text(
            'type=token\ntoken=synthetic-not-a-credential\nmodel_defaults=1\n')
        settings.write_text(json.dumps({
            'model': 'haiku', 'effortLevel': 'high',
            'modelSettings': {'claude-haiku-4-5': {'effortLevel': 'high'}},
            'env': {'ANTHROPIC_MODEL': 'haiku', 'CLAUDE_CODE_EFFORT_LEVEL': 'high'},
        }))
        discovery(cli, home, env, expected_model=None, expected_effort=None)
        print('PASS real native: shared skills discovered; configured and absent defaults survive sequential and concurrent launches')


if __name__ == '__main__':
    main()
