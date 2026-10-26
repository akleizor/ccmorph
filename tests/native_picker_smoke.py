"""Optional real-fzf terminal smoke: run from the repository root; no network."""
import fcntl
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time

root = Path.cwd()
with tempfile.TemporaryDirectory(prefix='picker-live-', dir=root) as temp:
    home = Path(temp)
    state = home / 'state'
    state.mkdir()
    (state / 'demo.conf').write_text('type=apikey\napi_key=synthetic-key\nmodel_defaults=1\n')
    env = dict(os.environ, HOME=temp, ZDOTDIR=temp, XDG_CONFIG_HOME=str(home / '.config'),
               CCPROTEUS_STATE_DIR=str(state), CCPROTEUS_LIB_DIR=str(root), TERM='xterm-256color',
               FZF_DEFAULT_OPTS='', FZF_DEFAULT_OPTS_FILE='')
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 30, 110, 0, 0))
    def terminal():
        os.setsid()
        fcntl.ioctl(0, termios.TIOCSCTTY, 0)
    proc = subprocess.Popen([str(root / 'ccproteus'), 'set', 'demo', 'effort'],
                            env=env, cwd=temp, stdin=slave, stdout=slave, stderr=slave,
                            preexec_fn=terminal)
    os.close(slave)
    output = b''
    sent = False
    deadline = time.monotonic() + 20
    try:
        while proc.poll() is None and time.monotonic() < deadline:
            if select.select([master], [], [], .2)[0]:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                output += data
                if b'\x1b[6n' in data:
                    os.write(master, b'\x1b[1;1R')
                if not sent and b'fastest' in output and b'demo effort>' in output:
                    os.write(master, b'\x1b[B\x1b[B\r')
                    sent = True
        assert proc.wait(timeout=2) == 0, output[-3000:]
        assert sent and 'effort=high' in (state / 'demo.conf').read_text(), output[-3000:]
        print('PASS installed fzf TTY: selected high with arrow keys and Enter; isolated profile saved high')
    finally:
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.wait()
        os.close(master)
