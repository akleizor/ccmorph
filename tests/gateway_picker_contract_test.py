"""Documented gateway-picker behaviour (docs/guide.md "Exclude unwanted gateway models")
that model_exclusions_test.py does not reach: the real `set ... -i` and `run` entry points."""
import fcntl
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess

import pytest

from support import HANG_TIMEOUT, stdin_a_terminal, wait_until

ROOT = Path(__file__).resolve().parents[1]
FZF = '#!/usr/bin/env python3\nimport sys\nsys.stdout.write(sys.stdin.read())\n'


# The launch fetches /v1/models through curl; this one serves $LISTING.
FAKE_CURL = r'''mkdir -p "$HOME/fakebin"
printf '%s\n' '#!/bin/sh' 'printf "%s" "$LISTING"' > "$HOME/fakebin/curl"
chmod +x "$HOME/fakebin/curl"
export PATH="$HOME/fakebin:$PATH"
'''

def copied_lib(tmp_path):
    """A throwaway install root, so a test can stub one libexec program in place.

    The copy is the repository's own source — what git tracks, plus libexec's
    build output — never the checkout's incidental contents: tool data a
    machine's global gitignore hides (.codegraph's 21 MB database, a daemon's
    Unix socket, which cannot be copied at all) used to land in every test home
    and fail the copy itself (#220)."""
    inst = tmp_path / 'inst'
    tracked = subprocess.run(['git', '-C', str(ROOT), 'ls-files', '-z'],
                             stdout=subprocess.PIPE, check=True,
                             timeout=HANG_TIMEOUT).stdout
    for rel in tracked.split(b'\0'):
        if not rel or rel.startswith((b'tests/', b'.claude/')):
            continue
        src = ROOT / os.fsdecode(rel)
        if not src.is_file():  # a tracked path may be gone from the working tree
            continue
        dst = inst / os.fsdecode(rel)
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)
    # libexec holds only build output (`make shim`; gitignored) and the stub
    # below renames the built core, so the copy needs it too.
    for built in (ROOT / 'libexec').iterdir():
        # Dotfiles are the directory's own markers and the build lock, not
        # install content.
        if built.is_file() and not built.name.startswith('.'):
            dst = inst / 'libexec' / built.name
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(built, dst)
    return inst / 'lib'


# A cold `go build` is not a hang: this checkout's cold build takes about 100 s,
# and a loaded CI host can be slower. This bound only stops a wedged make;
# HANG_TIMEOUT alone (a per-spawn hang detector, 120 s) is too tight for a
# compile.
BUILD_TIMEOUT = HANG_TIMEOUT * 5


def ensure_compiled_core(root=ROOT, build_timeout=BUILD_TIMEOUT):
    """Build `root`'s libexec/ccmorph-core when a fresh checkout has none.

    libexec holds only gitignored build output, so a checkout starts without the
    core (only `make shim` makes it). Five tests here drive the real CLI or copy
    the install root; without the core they fail with a FileNotFoundError on the
    copied file (#714).

    Callers take the lock before looking at the core and hold it across the
    whole build, so one worker builds and the rest wait for it. `make shim`
    publishes the core only once it is built and signed; this helper holds the
    same line for a recipe that does not: a nonzero exit or a timeout removes
    whatever the recipe left, and a timeout first kills every process the build
    started, so none of them can write the core after the lock is released."""
    libexec = root / 'libexec'
    core = libexec / 'ccmorph-core'
    libexec.mkdir(parents=True, exist_ok=True)
    with open(libexec / '.ccmorph-core-build.lock', 'a') as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        if core.is_file():
            return core
        # Its own session, so the build's processes share one group: a timeout
        # in subprocess.run killed make alone, and the go build under it went on
        # to write the core after this lock was released (#714).
        build = subprocess.Popen(
            ['make', '-C', str(root), 'shim'], stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            out, _ = build.communicate(timeout=build_timeout)
        except BaseException:
            try:
                os.killpg(build.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            # The build's processes hold this pipe open, so reading it to its
            # end waits until the group is gone.
            build.communicate(timeout=HANG_TIMEOUT)
            core.unlink(missing_ok=True)
            raise
        if build.returncode != 0:
            core.unlink(missing_ok=True)
            raise RuntimeError(
                f'`make shim` failed (exit {build.returncode}), so {core} is '
                f'not usable:\n{out}')
        if not core.is_file():
            raise RuntimeError(
                f'`make shim` exited 0 but wrote no {core}:\n{out}')
    return core


# Build once, at collection. A session fixture would run inside the first
# test's setup, on pytest.ini's 60 s clock, and a cold `make shim` outlasts it:
# the first parallel run on a cold Go cache timed out every waiting worker. The
# collection phase is not on that clock, so the workers block there until the
# build finishes and every test runs against a completed core.
ensure_compiled_core()


def stub_sizer(lib, args_file):
    """Replace the networked sizer in a copied lib; it records its arguments instead.

    The sizer is `ccmorph-core context-probe`: the copied core becomes
    ccmorph-core.real and a wrapper answers that one subcommand."""
    core = lib.parent / 'libexec/ccmorph-core'
    real = core.with_name('ccmorph-core.real')
    core.rename(real)
    core.write_text(f'#!/bin/sh\nif [ "$1" = context-probe ]; then shift; echo "$@" > "{args_file}"; exit 0; fi\n'
                    f'exec "{real}" "$@"\n')
    core.chmod(0o755)


def setup(tmp_path, excluded):
    state = tmp_path / 'state'
    (state / 'profiles/demo').mkdir(parents=True)
    (state / 'demo.conf').write_text(
        'type=apikey\napi_key=synthetic\nbase_url=https://api.anthropic.com\n'
        f'excluded_models={excluded}\n')
    bindir = tmp_path / 'bin'
    bindir.mkdir()
    (bindir / 'fzf').write_text(FZF)
    (bindir / 'claude').write_text(f'#!/bin/sh\necho "$@" >> "{tmp_path}/claude.ran"\n'
                                  f'env > "{tmp_path}/claude.env"\n')
    for f in bindir.iterdir():
        f.chmod(0o755)
    return state, {**os.environ, 'HOME': str(tmp_path), 'CCMORPH_STATE_DIR': str(state),
                   'CCMORPH_LIB_DIR': str(ROOT / "lib"), 'PATH': f'{bindir}:{os.environ["PATH"]}',
                   'LISTING': json.dumps({'data': [{'id': m} for m in IDS]})}


IDS = ['blocked', 'allowed', 'future-model']


def claude_calls(tmp_path):
    f = tmp_path / 'claude.ran'
    return f.read_text() if f.exists() else ''


def zsh(script, env, stdin=subprocess.DEVNULL):
    return subprocess.run(['zsh', '-fc', 'source "$1"\n' + script, '_',
                           str(Path(env['CCMORPH_LIB_DIR']) / 'ccmorph.sh')],
                          env=env, text=True, capture_output=True, timeout=HANG_TIMEOUT, stdin=stdin)


def set_model_i(tmp_path, excluded):
    state, env = setup(tmp_path, excluded)
    # fzf stub echoes every row; the picker returns the first (sorted) one it was offered.
    # A pick needs a terminal (#725): stdin is a pty, stdout/stderr stay pipes.
    with stdin_a_terminal() as tty:
        r = zsh(FAKE_CURL + 'ccmorph set demo model -i', env, stdin=tty)
    assert r.returncode == 0, r.stderr
    return (state / 'demo.conf').read_text()


def test_set_model_interactive_honours_exclusions(tmp_path):
    assert 'model=blocked' in set_model_i(tmp_path, 'allowed')   # `allowed` sorts first but is excluded


def test_set_model_interactive_offers_everything_not_excluded(tmp_path):
    assert 'model=allowed' in set_model_i(tmp_path, 'blocked')


def test_launch_stops_when_exclusions_leave_no_rows(tmp_path):
    state, env = setup(tmp_path, 'blocked,allowed,future-model')
    r = zsh(FAKE_CURL + 'ccmorph run demo -p hi', env)
    assert r.returncode != 0
    assert 'no cached or served picker models remain' in r.stderr
    assert '-p hi' not in claude_calls(tmp_path)
    settings = state / 'profiles/demo/settings.json'
    assert not settings.exists() or 'availableModels' not in json.loads(settings.read_text()) \
        or json.loads(settings.read_text())['availableModels']


def test_run_on_a_silent_gateway_waits_then_exits_the_gateway_class(tmp_path):
    # The 2026-11-20 outage (#723): a gateway answering nothing emptied the
    # picker (cache empty, excluded_models set) and the run died as a generic
    # crash — exit 1 with the "revise the list" message. A caller must be able
    # to tell the outage from a build crash: the run waits on the listing, and
    # if the gateway never answers, exits the retry-later class naming the route.
    state, env = setup(tmp_path, 'blocked')
    (state / 'profiles/demo/.claude.json').write_text(
        json.dumps({'additionalModelOptionsCache': []}))
    dead = r'''mkdir -p "$HOME/fakebin"
printf '%s\n' '#!/bin/sh' 'exit 7' > "$HOME/fakebin/curl"
chmod +x "$HOME/fakebin/curl"
export PATH="$HOME/fakebin:$PATH"
'''
    r = zsh(dead + 'ccmorph run demo -p hi', env)
    assert 'no cached or served picker models remain' not in r.stderr
    assert "Waiting for 'demo' gateway" in r.stderr
    assert 'could not reach' in r.stderr
    assert 'ccmorph doctor demo' in r.stderr
    assert r.returncode == 75, r.stderr
    assert '-p hi' not in claude_calls(tmp_path)


def test_run_on_a_200_error_body_exits_the_gateway_class(tmp_path):
    # #747: a gateway that answers 200 with an error object has answered no
    # listing, exactly as silence is no listing. The picker once read the
    # missing data as an authoritative empty one, skipped the cache and died
    # with the "revise the list" config message. It must take the silence
    # route: the cache is judged, then the run exits 75 naming the route.
    state, env = setup(tmp_path, 'blocked')
    (state / 'profiles/demo/.claude.json').write_text(
        json.dumps({'additionalModelOptionsCache': []}))
    erroring = r'''mkdir -p "$HOME/fakebin"
printf '%s\n' '#!/bin/sh' 'printf "%s" "{\"error\":{\"message\":\"upstream is unwell\"}}"' > "$HOME/fakebin/curl"
chmod +x "$HOME/fakebin/curl"
export PATH="$HOME/fakebin:$PATH"
'''
    r = zsh(erroring + 'ccmorph run demo -p hi', env)
    assert 'no cached or served picker models remain' not in r.stderr
    assert 'could not reach' in r.stderr
    assert 'ccmorph doctor demo' in r.stderr
    assert r.returncode == 75, r.stderr
    assert '-p hi' not in claude_calls(tmp_path)


def test_launch_projects_filtered_rows_and_keeps_discovery_off(tmp_path):
    lib = copied_lib(tmp_path)
    stub_sizer(lib, tmp_path / 'sizer.ran')
    state, env = setup(tmp_path, 'blocked')
    env['CCMORPH_LIB_DIR'] = str(lib)
    r = zsh(FAKE_CURL + 'ccmorph run demo -p hi', env)
    assert r.returncode == 0, r.stderr
    # The copied lib's sizer stub writes this; the real one would size the synthetic
    # ids against models.dev and the live endpoint on every launch. A missing marker
    # means the real, networked sizer ran again. The deadline is the launch's bound
    # on the pass (#757): its save waits on the profile lock, which `ccmorph rm`
    # parked at its confirm holds for as long as the person thinks.
    assert (tmp_path / 'sizer.ran').read_text().split() == ['--listing-stdin', '--profile', 'demo', '--deadline', '30']
    assert '-p hi' in claude_calls(tmp_path)
    assert 'GATEWAY_MODEL_DISCOVERY' not in (tmp_path / 'claude.env').read_text()
    home = state / 'profiles/demo'
    rows = [x['value'] for x in json.loads((home / '.claude.json').read_text())['additionalModelOptionsCache']]
    assert rows == ['allowed', 'future-model']
    assert 'blocked' not in json.loads((home / 'settings.json').read_text())['availableModels']


def test_probe_model_interactive_honours_exclusions(tmp_path):
    # `--model -i` is the probe flag; it must offer the same filtered rows as `set ... -i`.
    lib = copied_lib(tmp_path)
    stub_sizer(lib, tmp_path / 'probe.args')
    state, env = setup(tmp_path, 'allowed')
    env['CCMORPH_LIB_DIR'] = str(lib)
    with stdin_a_terminal() as tty:
        r = zsh(FAKE_CURL + 'ccmorph probe demo --model -i', env, stdin=tty)
    assert r.returncode == 0, r.stderr
    assert '--model blocked' in (tmp_path / 'probe.args').read_text()


def test_a_fresh_checkout_builds_the_core_the_tests_drive(tmp_path):
    """A checkout has no libexec/ccmorph-core until `make shim` builds it, and
    five tests here used to fail with FileNotFoundError on the copied core when
    it was absent (#714). `ensure_compiled_core` builds it once; a checkout that
    already has one builds nothing."""
    checkout = tmp_path / 'checkout'
    (checkout / 'libexec').mkdir(parents=True)
    (checkout / 'Makefile').write_text(
        'shim:\n'
        '\t@printf "built\\n" >> calls\n'
        '\t@printf core > libexec/ccmorph-core\n')
    ensure_compiled_core(checkout)
    assert (checkout / 'libexec/ccmorph-core').read_text() == 'core'
    ensure_compiled_core(checkout)
    assert (checkout / 'calls').read_text() == 'built\n'


def test_a_failed_core_build_is_never_used_as_the_core(tmp_path):
    """A recipe that writes the core and then fails (this fake one, or the
    `make shim` before #714, which signed the binary in place) leaves a file on
    disk. That file is not a core: the failure is raised, the file is removed,
    and the next caller builds again instead of trusting it (#714)."""
    checkout = tmp_path / 'checkout'
    (checkout / 'libexec').mkdir(parents=True)
    (checkout / 'Makefile').write_text(
        'shim:\n'
        '\t@printf "build\\n" >> calls\n'
        '\t@printf partial > libexec/ccmorph-core\n'
        '\t@exit 1\n')
    for _ in range(2):
        with pytest.raises(RuntimeError, match='make shim'):
            ensure_compiled_core(checkout)
        assert not (checkout / 'libexec/ccmorph-core').exists()
    assert (checkout / 'calls').read_text() == 'build\nbuild\n'


def builder_is_gone(pid):
    """Whether `pid` no longer names a process. Signal 0 asks without sending:
    ProcessLookupError says the pid is free, PermissionError says another user
    owns it now (so the builder is gone either way). `ps` is not used because a
    sandboxed runner may refuse to spawn it."""
    try:
        os.kill(pid, 0)
    except (ProcessLookupError, PermissionError):
        return True
    return False


def test_a_timed_out_core_build_leaves_no_process_to_write_the_core(tmp_path):
    """A timeout used to kill make alone: the build under it kept running,
    wrote the core after the lock was released, and the next caller took that
    unsigned file (#714). The whole build is killed, and gone, before the
    helper returns."""
    checkout = tmp_path / 'checkout'
    (checkout / 'libexec').mkdir(parents=True)
    (checkout / 'Makefile').write_text(
        'shim:\n'
        '\t@echo $$$$ > builder.pid; while [ ! -e go-on ]; do sleep 0.05; done; '
        'printf late > libexec/ccmorph-core\n')
    try:
        with pytest.raises(subprocess.TimeoutExpired):
            ensure_compiled_core(checkout, build_timeout=HANG_TIMEOUT / 24)
        pid = int((checkout / 'builder.pid').read_text())
        assert wait_until(lambda: builder_is_gone(pid), HANG_TIMEOUT / 24), \
            f'builder {pid} still running'
    finally:
        (checkout / 'go-on').touch()
    assert not (checkout / 'libexec/ccmorph-core').exists()


def shim_checkout(tmp_path, go_body, codesign_body):
    """A scratch checkout holding this repository's Makefile, with stub `go`
    and `codesign` on PATH: `go` runs `go_body` with $out set to its -o path,
    `codesign` runs `codesign_body` with $last set to the file it signs."""
    checkout = tmp_path / 'checkout'
    (checkout / 'libexec').mkdir(parents=True)
    (checkout / 'lib').mkdir()
    (checkout / 'lib/ccmorph.sh').write_text('_CCMORPH_VERSION="0.0.0"\n')
    shutil.copy2(ROOT / 'Makefile', checkout / 'Makefile')
    stubs = tmp_path / 'stubs'
    stubs.mkdir()
    (stubs / 'go').write_text(
        '#!/bin/sh\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n' + go_body)
    (stubs / 'codesign').write_text('#!/bin/sh\nfor a; do last=$a; done\n' + codesign_body)
    for stub in stubs.iterdir():
        stub.chmod(0o755)
    env = {**os.environ, 'PATH': f'{stubs}:{os.environ["PATH"]}'}
    return checkout, env


def make_shim(checkout, env):
    return subprocess.run(['make', '-C', str(checkout), 'shim',
                           'CCMORPH_CODESIGN_IDENTITY=test-identity'],
                          env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)


def test_make_shim_publishes_the_core_only_after_signing_it(tmp_path):
    """`make shim` wrote libexec/ccmorph-core and signed it in place, so a
    reader could take the file between the two steps (#714). It signs a
    private copy and publishes it after, and leaves no build directory."""
    checkout, env = shim_checkout(
        tmp_path, 'printf core > "$out"\n',
        'if [ -e libexec/ccmorph-core ]; then w=published; else w=unpublished; fi\n'
        'printf \'%s %s\\n\' "$(basename "$last")" "$w" > signed\n')
    r = make_shim(checkout, env)
    assert r.returncode == 0, r.stdout + r.stderr
    assert (checkout / 'signed').read_text() == 'ccmorph-core unpublished\n'
    assert (checkout / 'libexec/ccmorph-core').read_text() == 'core'
    assert sorted(p.name for p in (checkout / 'libexec').iterdir()) == ['ccmorph-core']


def test_a_failed_make_shim_leaves_the_published_core_as_it_was(tmp_path):
    """A signing failure used to leave the new, unsigned binary as the core.
    The published core stays the one that was there (#714)."""
    checkout, env = shim_checkout(tmp_path, 'printf partial > "$out"\n', 'exit 1\n')
    (checkout / 'libexec/ccmorph-core').write_text('old')
    r = make_shim(checkout, env)
    assert r.returncode != 0
    assert (checkout / 'libexec/ccmorph-core').read_text() == 'old'
    assert sorted(p.name for p in (checkout / 'libexec').iterdir()) == ['ccmorph-core']


def test_copied_lib_copies_source_not_the_checkouts_incidental_contents_issue_220(tmp_path, monkeypatch):
    """`copied_lib` copies the repository's own source, never the checkout's
    incidental contents: local tooling leaves data git ignores in the root — on
    this machine codegraph's `.codegraph/` — and its Unix socket cannot be
    copied at all, which failed every test that used the scaffold (#220)."""
    planted = ROOT / '.copied-lib-regression-220'
    shutil.rmtree(planted, ignore_errors=True)
    planted.mkdir()
    (planted / 'tool.db').write_text('untracked tool data, not source')
    # Bind the relative name from inside the planted dir: the absolute one is
    # len(ROOT)+39 bytes and macOS refuses an AF_UNIX path of 104 or more, so a
    # checkout root longer than 64 bytes failed the gate on this bind itself,
    # not on the regression (#715). Eleven bytes answers the limit on every
    # checkout, and the socket still lands inside the checkout, where the
    # copy must not reach it.
    monkeypatch.chdir(planted)
    sock = socket.socket(socket.AF_UNIX)
    try:
        try:
            sock.bind('daemon.sock')
        except PermissionError:
            # A sandboxed runner refuses the bind — the same refusal that skips
            # temp_hygiene's witnesses there; the landing gate runs outside any
            # sandbox.
            pytest.skip("this runner refuses unix-socket binds; the landing gate "
                        "runs this test outside any sandbox")
        lib = copied_lib(tmp_path)
    finally:
        sock.close()
        shutil.rmtree(planted, ignore_errors=True)
    assert (lib / 'ccmorph.sh').is_file()            # the source itself is copied
    assert not (lib.parent / planted.name).exists()  # the planted tool data is not
    # The filing machine's case: a live .codegraph database in the root. Absent
    # elsewhere, where this holds vacuously.
    assert not (lib.parent / '.codegraph').exists()
