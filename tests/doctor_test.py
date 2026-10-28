import hashlib
import os
import signal
import subprocess
import sys
import errno
import pty
import select
import shutil
import time
from pathlib import Path
from unittest.mock import patch

import pytest

from support import FAKE_SERVER_NO_DNS, HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def run_doctor(tmp_path, conf, curl=None):
    state = tmp_path / "state"
    home = tmp_path / "home"
    bindir = tmp_path / "bin"
    (state / "profiles" / "demo").mkdir(parents=True, exist_ok=True)
    state.mkdir(exist_ok=True)
    bindir.mkdir(exist_ok=True)
    if not (state / "demo.conf").exists():
        (state / "demo.conf").write_text(conf)
    if not (state / "local.sh").exists():
        (state / "local.sh").write_text("printf marker > \"$DOCTOR_MARKER\"\n")
    (state / "local.sh").chmod(0o700)
    claude = bindir / "claude"
    claude.write_text("#!/bin/sh\nexit 0\n")
    claude.chmod(0o755)
    if curl is not None:
        c = bindir / "curl"
        c.write_text(curl)
        c.chmod(0o755)
    env = {**os.environ, "HOME": str(home), "PATH": f"{bindir}:{os.environ['PATH']}",
           "CCMORPH_STATE_DIR": str(state), "CCMORPH_LIB_DIR": str(ROOT / "lib"),
           "CURL_ARGS": str(bindir / "curl.args"), "CURL_CALLED": str(bindir / "called"),
           "DOCTOR_MARKER": str(bindir / "doctor.marker")}
    result = subprocess.run([str(ROOT / "bin/ccmorph"), "doctor", "demo"], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    return result, state / "demo.conf", bindir


def test_doctor_auth_redaction_and_no_mutation(tmp_path):
    curl = """#!/bin/sh
printf '%s\\n' "$@" > "$CURL_ARGS"
[ "$1" = -K ] && cat >> "$CURL_ARGS"
printf '%s\\n' '{"data":[]}'
printf '%s\\n' '__CCMORPH_STATUS__200'
"""
    conf = "type=apikey\nbase_url=https://user:secret@example.test/api\napi_key=secret-key\n"
    result, path, bindir = run_doctor(tmp_path, conf, curl)
    assert result.returncode == 0, result.stderr
    assert "HTTP 200" in result.stdout
    assert "secret" not in result.stdout
    assert "secret-key" not in result.stderr
    args = (bindir / "curl.args").read_text().splitlines()
    assert any("secret-key" in arg for arg in args)
    assert "--max-time" in args and "5" in args
    assert any("x-api-key:" in arg for arg in args)
    assert any("Authorization: Bearer" in arg for arg in args)
    assert hashlib.sha256(path.read_bytes()).hexdigest() == hashlib.sha256(conf.encode()).hexdigest()
    assert not (bindir / "doctor.marker").exists()
    assert (path.parent / "local.sh").stat().st_mode & 0o777 == 0o700
    # Doctor's metadata request must match the launch route, which means going
    # through the caller's own HTTPS_PROXY rather than refusing every proxy.
    assert "--noproxy" not in args
    assert any("/v1/models?limit=1000" in arg for arg in args)


def test_doctor_rejects_malformed_listing(tmp_path):
    curl = """#!/bin/sh
printf '%s\n' '{"data":"not-a-list"}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""
    result, _, _ = run_doctor(tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", curl)
    assert result.returncode != 0
    assert "malformed model listing" in result.stdout


OK_CURL = """#!/bin/sh
printf '%s\n' '{"data":[]}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""


def test_doctor_reports_the_profile_default_model_whatever_its_origin(tmp_path):
    # The model is one profile-owned default now, so doctor names it as such
    # instead of pointing at whichever native layer happened to hold it. Every
    # origin below is still exercised, because doctor must resolve each one --
    # only the label is uniform.
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    (tmp_path / "home" / ".claude").mkdir(parents=True)
    shared = tmp_path / "home" / ".claude" / "settings.json"
    shared.write_text('{"model":"global"}')
    profile = tmp_path / "state" / "profiles" / "demo"
    profile.mkdir(parents=True)
    profile_settings = profile / "settings.json"

    def effective(conf_text, profile_json):
        profile_settings.write_text(profile_json)
        (tmp_path / "state" / "demo.conf").parent.mkdir(exist_ok=True)
        (tmp_path / "state" / "demo.conf").write_text(conf_text)
        result, path, _ = run_doctor(tmp_path, conf_text, OK_CURL)
        assert result.returncode == 0, result.stderr
        line = [l for l in result.stdout.splitlines() if "effective model:" in l]
        assert len(line) == 1, result.stdout
        # Doctor is a read-only check: resolving the default must not seed the conf.
        assert path.read_text() == conf_text
        return line[0].split("effective model:", 1)[1].strip()

    assert effective(conf, '{"env":{"ANTHROPIC_MODEL":"saved"}}') == "saved (profile default)"
    assert effective(conf, '{}') == "global (profile default)"
    shared.write_text('{"env":{"ANTHROPIC_MODEL":"shared-pin"}}')
    assert effective(conf, '{"model":"profile-model"}') == "shared-pin (profile default)"
    assert effective(conf, '{"model":"profile-model","env":{"ANTHROPIC_MODEL":""}}') \
        == "profile-model (profile default)"
    shared.write_text('{}')
    # A conf default wins over the native layer and reports the same way.
    assert effective(conf + "model=conf-model\n", '{"model":"native-model"}') \
        == "conf-model (profile default)"
    # A seeded profile with no default names none, and native cannot revive it.
    assert effective(conf + "model_defaults=1\n", '{"model":"native-model"}') \
        == "Claude Code default (no saved model)"
    assert effective(conf, '{}') == "Claude Code default (no saved model)"


def test_doctor_rejects_invalid_profile_settings_json(tmp_path):
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    profile = tmp_path / "state" / "profiles" / "demo"
    profile.mkdir(parents=True)
    (profile / "settings.json").write_text('{ invalid')
    result, _, _ = run_doctor(tmp_path, conf)
    assert result.returncode != 0
    assert "invalid JSON" in result.stdout


# The process shapes of a live session on today's architecture: the shim is
# served inside the adapter process (GH-49, PR #40), so the supervisor has no
# shim child. A translator route adds a cliproxyapi child, and only a profile's
# own CCMORPH_SHIM_HOOKS build still gets a spawned gateway-shim. The /bin/echo
# row is the decoy a substring match would mistake for the adapter.
ADAPTER_PS_ROWS = {
    "translator-route": (
        "'4000 1 /bin/echo ccmorph-core adapter demo' "
        "'4321 1 /usr/local/libexec/ccmorph-core adapter demo --auto --parent-pid 400' "
        "'4322 4321 /usr/local/bin/cliproxyapi -config /tmp/config.yaml'"),
    "native-route": (
        "'4000 1 /bin/echo ccmorph-core adapter demo' "
        "'4321 1 /usr/local/libexec/ccmorph-core adapter demo --auto --parent-pid 400'"),
    "hooks-build": (
        "'4000 1 /bin/echo ccmorph-core adapter demo' "
        "'4321 1 /usr/local/libexec/ccmorph-core adapter demo --auto --parent-pid 400' "
        "'4322 4321 /usr/local/bin/cliproxyapi -config /tmp/config.yaml' "
        "'4323 4321 /usr/local/bin/gateway-shim 4568 http://127.0.0.1:4567'"),
}

# What doctor owes for each shape: every listener holder running-adapter names —
# a spawned child, or the adapter process itself when the shim is in-process —
# gets its port checked, exactly as a child's was.
ADAPTER_LISTENERS = {
    "translator-route": (("cliproxyapi child", "4322"), ("in-process shim", "4321")),
    "native-route": (("in-process shim", "4321"),),
    "hooks-build": (("cliproxyapi child", "4322"), ("gateway-shim child", "4323")),
}

# lsof's answer per pid: the shim (in-process or spawned) serves the session on
# 4568, the translator on 4567.
ADAPTER_LSOF_ROWS = (
    ("4321", "ccmorph-core 4321 u 3u IPv4 0x 0t0 TCP 127.0.0.1:4568 (LISTEN)"),
    ("4322", "cliproxyapi 4322 u 3u IPv4 0x 0t0 TCP 127.0.0.1:4567 (LISTEN)"),
    ("4323", "gateway-shim 4323 u 3u IPv4 0x 0t0 TCP 127.0.0.1:4568 (LISTEN)"),
)


def _adapter_stubs(bindir, slow, rows):
    """Install the ps/lsof doubles doctor's read-only inspection consults.

    `slow` names the inspection whose double outlives doctor's one-second bound
    (GH-467): the deadline kills it, and doctor's designed answer is SKIP."""
    bindir.mkdir(parents=True, exist_ok=True)
    ps = "#!/bin/sh\n" + ("sleep 2\n" if slow == "ps" else "") + f"printf '%s\\n' {rows}\n"
    (bindir / "ps").write_text(ps)
    (bindir / "ps").chmod(0o755)
    # lsof answers per pid, like the real one: a matching row and exit 0, or
    # nothing and exit 1.
    lsof = ("#!/bin/sh\n" + ("sleep 2\n" if slow == "lsof" else "")
            + "printf '%s\\n' 'COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME'\n"
            + "case \"$4\" in\n")
    for pid, row in ADAPTER_LSOF_ROWS:
        lsof += f"  {pid}) printf '%s\\n' '{row}'; exit 0;;\n"
    (bindir / "lsof").write_text(lsof + "esac\nexit 1\n")
    (bindir / "lsof").chmod(0o755)


def assert_adapter_report(out, shape):
    """Accept every answer doctor promises for the running-adapter report, and
    only those: each read-only inspection (ps, then lsof per listener holder) is
    bounded at one second, and a bound blown under load is answered SKIP (GH-467).
    A dropped report, a WARN, or the decoy pid stays red in every outcome."""
    assert "pid 4000" not in out  # the /bin/echo decoy row is never an adapter
    if "process inspection unavailable" in out:
        # ps blew its bound: one SKIP line is the whole answer — nothing
        # half-reported, nothing started.
        assert "listener" not in out, out
        return
    for subject, pid in ADAPTER_LISTENERS[shape]:
        reported = (f"adapter: {subject} listener" in out
                    and f"(pid {pid}; read-only check)" in out)
        unknown = f"adapter: {subject} listener status unknown (pid {pid})" in out
        assert reported or unknown, out


@pytest.mark.parametrize("slow", ["", "ps", "lsof"],
                         ids=["prompt", "ps-over-deadline-issue-467",
                              "lsof-over-deadline-issue-467"])
@pytest.mark.parametrize("shape", ["translator-route", "native-route", "hooks-build"])
def test_doctor_reports_existing_adapter_without_starting_it(tmp_path, shape, slow):
    curl = """#!/bin/sh
printf '%s\n' '{"data":[]}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nadapter=true\n"
    # Stubs first: run_doctor only fills in what is missing, so doctor's single
    # run already consults them (no separate unstubbed setup run).
    _adapter_stubs(tmp_path / "bin", slow, ADAPTER_PS_ROWS[shape])
    result, _, bindir = run_doctor(tmp_path, conf, curl)
    assert result.returncode == 0, result.stderr
    assert_adapter_report(result.stdout, shape)
    # Read-only: the profile's own hook never ran, so nothing was started.
    assert not (bindir / "doctor.marker").exists()


def test_doctor_tolerates_a_conf_that_still_declares_removed_keys(tmp_path):
    """Removed keys (transport, forward_ip, subagent_model, lean) were removed,
    not reserved: an old conf carrying any or all of them must still be checkable."""
    curl = "#!/bin/sh\nprintf '%s\\n' '{\"data\":[]}'\nprintf '%s\\n' '__CCMORPH_STATUS__200'\n"
    result, _, _ = run_doctor(
        tmp_path / "legacy",
        "type=apikey\nbase_url=https://example.test\napi_key=k\ntransport=tunnel\n"
        "forward_ip=192.0.2.46\nsubagent_model=legacy-subagent-model\nlean=true\n", curl)
    assert result.returncode == 0, result.stderr
    assert "transport= was removed" in result.stderr
    assert "forward_ip= was removed" in result.stderr
    assert "subagent_model= was removed" in result.stderr
    assert "lean= was removed" in result.stderr
    assert "gateway: reachable" in result.stdout


def test_doctor_invalid_url_skips_request(tmp_path):
    curl = """#!/bin/sh
printf '%s\n' called >> "$CURL_CALLED"
printf '%s\n' '{"data":[]}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""
    result, _, bindir = run_doctor(tmp_path, "type=apikey\nbase_url=https://example.test/path?token=secret\napi_key=k\n", curl)
    assert result.returncode != 0
    assert "no request sent" in result.stdout
    assert not (bindir / "called").exists()


def test_doctor_missing_credential_fails_without_request(tmp_path):
    result, _, _ = run_doctor(tmp_path, "type=apikey\nbase_url=https://example.test\n")
    assert result.returncode != 0
    assert "has no credential" in result.stdout
    assert "no API credential" in result.stdout


def test_doctor_native_profile_without_base_url_is_clear(tmp_path):
    result, _, _ = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n")
    assert result.returncode == 0
    assert "native endpoint" in result.stdout
    assert "native profile path" in result.stdout


def test_run_doctor_remains_native_passthrough(tmp_path):
    state = tmp_path / "state"
    home = tmp_path / "home"
    bindir = tmp_path / "bin"
    state.mkdir()
    home.mkdir()
    bindir.mkdir()
    (state / "active").write_text("demo\n")
    (state / "demo.conf").write_text("type=provider\nprovider=test\n")
    (state / "profiles" / "demo").mkdir(parents=True, exist_ok=True)
    (state / "profiles" / "demo" / "config.json").write_text('{}')
    (state / "profiles" / "demo" / ".claude.json").write_text('{}')
    (state / "auth").mkdir()
    (state / "auth" / "demo").mkdir()
    (bindir / "claude").write_text("#!/bin/sh\nif [ \"$1\" = --help ]; then printf 'Commands:\\n  doctor\\n'; else printf '%s\\n' \"$@\" > \"$NATIVE_OUT\"; fi\n")
    (bindir / "claude").chmod(0o755)
    # A provider profile's launch needs a cliproxyapi that binds a port and answers
    # /v1/models with 200; stub it so the test does not depend on the host's install.
    (bindir / "cliproxyapi").write_text(
        "#!/usr/bin/env python3\n"
        "from http.server import BaseHTTPRequestHandler, HTTPServer\n" + FAKE_SERVER_NO_DNS +
        "class H(BaseHTTPRequestHandler):\n"
        "    def do_GET(self):\n"
        "        self.send_response(200); self.send_header('Content-Length', '2')\n"
        "        self.end_headers(); self.wfile.write(b'{}')\n"
        "    def log_message(self, *a): pass\n"
        "HTTPServer(('127.0.0.1', 0), H).serve_forever()\n")
    (bindir / "cliproxyapi").chmod(0o755)
    env = {**os.environ, "HOME": str(home), "PATH": f"{bindir}:{os.environ['PATH']}",
           "CCMORPH_STATE_DIR": str(state), "CCMORPH_LIB_DIR": str(ROOT / "lib"),
           "NATIVE_OUT": str(tmp_path / "native")}
    # Provider readiness alone has a 30-second budget; this is not a launch-speed test.
    process = subprocess.Popen([str(ROOT / "bin/ccmorph"), "run", "doctor"], env=env,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               text=True, start_new_session=True)
    try:
        _, stderr = process.communicate(timeout=HANG_TIMEOUT)
        assert process.returncode == 0, stderr
        assert (tmp_path / "native").read_text().splitlines()[-1] == "doctor"
    finally:
        # A timed-out launcher must not orphan its disowned provider fixture.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            # The group is gone, which macOS spells two ways: EPERM while its
            # only member is the exited, unreaped launcher — the window between
            # the bound above and this kill (GH-5) — and ESRCH once nothing is
            # left. The launcher itself is still signalable, and this kill reaps
            # it, so the run's own verdict, not a cleanup error, is what the
            # caller hears.
            process.kill()
        process.wait(timeout=HANG_TIMEOUT)


@pytest.mark.skipif(sys.platform != "darwin",
                    reason="only macOS refuses to signal a group whose only member has exited")
def test_run_doctor_cleanup_reports_the_bound_not_a_refused_killpg_issue_5(tmp_path):
    """GH-5: a launcher that exits in the window between the bound and the
    cleanup killpg leaves its group led by an unreaped zombie, which macOS
    refuses to signal; the cleanup must tolerate that, so the blown bound —
    the report a genuinely hung run owes — is what the caller hears, not a
    cleanup PermissionError."""
    communicate, spent = subprocess.Popen.communicate, []

    def communicate_spending_the_bound_on_the_exiting_launcher(process, timeout=HANG_TIMEOUT):
        # Same forced window as the launch test: spend the bound waiting for
        # the exit nothing reaps — the group answers EPERM exactly then, and
        # nothing runs between that answer and the cleanup's killpg — then
        # report it blown. A healthy run reaches that answer in seconds; the
        # stop holds a platform that never refuses at half of pytest.ini's
        # per-test cap.
        if not spent:
            spent.append(True)
            stop = time.monotonic() + min(timeout, 30)
            while time.monotonic() < stop:
                try:
                    os.killpg(process.pid, 0)
                    time.sleep(0.02)
                except PermissionError:
                    break
            raise subprocess.TimeoutExpired(process.args, timeout)
        return communicate(process, timeout=timeout)

    with patch.object(subprocess.Popen, "communicate",
                      communicate_spending_the_bound_on_the_exiting_launcher), \
            pytest.raises(subprocess.TimeoutExpired):
        # The passthrough test is the act itself: its finally holds the
        # cleanup under guard, met exactly as a genuinely hung run meets it.
        test_run_doctor_remains_native_passthrough(tmp_path)
    # The launcher really finished before the bound was reported blown — the
    # refusal the cleanup met was the exited leader, not a live run.
    assert (tmp_path / "native").read_text().splitlines()[-1] == "doctor"


def test_doctor_terminal_statuses(tmp_path):
    # Exercise the executable, not a copy of its formatting function.
    _, conf, bindir = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n")
    env = {**os.environ, "HOME": str(tmp_path / "home"),
           "PATH": f"{bindir}:{os.environ['PATH']}",
           "CCMORPH_STATE_DIR": str(conf.parent), "CCMORPH_LIB_DIR": str(ROOT / "lib"),
           "TERM": "xterm-256color"}
    env.pop("NO_COLOR", None)
    for overrides, profile, expected, code in [
        ({}, "demo", "\x1b[38;2;215;119;87m✓\x1b[0m", 0),
        ({}, "missing", "\x1b[38;2;255;107;128m✗\x1b[0m", 1),
        ({"NO_COLOR": "1"}, "demo", "✓ profile:", 0),
        ({"TERM": "dumb"}, "demo", "✓ profile:", 0),
    ]:
        master, slave = pty.openpty()
        result = None
        try:
            result = subprocess.Popen([str(ROOT / "bin/ccmorph"), "doctor", profile],
                                      env={**env, **overrides}, stdin=subprocess.DEVNULL,
                                      stdout=slave, stderr=slave)
            os.close(slave)
            slave = None
            output = bytearray()
            deadline = time.monotonic() + HANG_TIMEOUT
            # Drain while the child runs: RGB escapes can fill the PTY buffer.
            while time.monotonic() < deadline:
                if not select.select([master], [], [], .1)[0]:
                    continue
                try:
                    chunk = os.read(master, 65536)
                except OSError as exc:
                    if exc.errno == errno.EIO:
                        break
                    raise
                if not chunk:
                    break
                output.extend(chunk)
            text = output.decode()
            assert result.wait(timeout=max(.1, deadline - time.monotonic())) == code, text
            assert expected in text, repr(text)
            assert "\\e[" not in text
            assert "\x1b[32m" not in text
            if overrides:
                assert "\x1b[" not in text
        finally:
            if result is not None and result.poll() is None:
                result.kill()
                result.wait(timeout=HANG_TIMEOUT)
            if slave is not None:
                os.close(slave)
            os.close(master)


def run_bare_doctor(tmp_path, confs=(), claude=True, python=None, rc_hosts=False):
    """`ccmorph doctor` with no profile argument, on a PATH holding only what it names."""
    state = tmp_path / "state"
    bindir = tmp_path / "bin"
    state.mkdir()
    bindir.mkdir()
    (tmp_path / "tmp").mkdir()
    # No python3 by default: the core runs on none, so a bare PATH must pass.
    for tool in ("zsh", "env"):
        found = shutil.which(tool)
        assert found, tool
        (bindir / tool).symlink_to(found)
    if python is not None:
        (bindir / "python3").write_text(python)
        (bindir / "python3").chmod(0o755)
    if rc_hosts:
        (state / "rc-hosts").write_text("1:/tmp\n")
    if claude:
        stub = bindir / "claude"
        stub.write_text("#!/bin/sh\nexit 0\n")
        stub.chmod(0o755)
    for name in confs:
        (state / f"{name}.conf").write_text("type=token\ntoken=sk-ant-oat-canary\n")
    env = {"HOME": str(tmp_path / "home"), "PATH": str(bindir), "TMPDIR": str(tmp_path / "tmp"),
           "CCMORPH_STATE_DIR": str(state), "CCMORPH_LIB_DIR": str(ROOT / "lib"),
           "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "NO_COLOR": "1"}
    result = subprocess.run([str(ROOT / "bin/ccmorph"), "doctor"], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    return result, state


def test_doctor_on_a_fresh_install_reports_the_environment(tmp_path):
    result, state = run_bare_doctor(tmp_path)
    assert result.returncode == 0, result.stdout + result.stderr
    for row in ("no profiles yet", "ccmorph add <name>", "claude executable:", "zsh:"):
        assert row in result.stdout, result.stdout
    assert "FAIL" not in result.stdout
    assert "python" not in result.stdout, result.stdout
    # Read-only: a report must not create the state it reports on.
    assert not (state / "profiles").exists()
    assert sorted(p.name for p in state.iterdir()) == []


def test_doctor_on_a_fresh_install_fails_without_claude(tmp_path):
    result, _ = run_bare_doctor(tmp_path, claude=False)
    assert result.returncode == 1, result.stdout + result.stderr
    assert "FAIL claude executable: not found" in result.stdout
    assert "no profiles yet" in result.stdout


def test_doctor_with_profiles_but_no_default_names_them(tmp_path):
    result, _ = run_bare_doctor(tmp_path, confs=("alpha", "beta"))
    assert result.returncode == 1
    out = result.stdout + result.stderr
    assert "no profile selected" in out
    assert "alpha" in out and "beta" in out
    assert "no profiles yet" not in out


def test_doctor_checks_python_only_where_remote_control_is_in_use(tmp_path):
    # Remote Control is the one add-on that still runs on an interpreter.
    for name in ("old", "absent", "ok"):
        (tmp_path / name).mkdir()
    old, state = run_bare_doctor(tmp_path / "old", python="#!/bin/sh\necho 3.7.9\nexit 1\n", rc_hosts=True)
    assert old.returncode == 1, old.stdout + old.stderr
    assert "FAIL remote control interpreter: 3.7.9 (need 3.8 or newer)" in old.stdout
    absent, _ = run_bare_doctor(tmp_path / "absent", rc_hosts=True)
    assert absent.returncode == 1, absent.stdout + absent.stderr
    assert "FAIL remote control interpreter: not found (need 3.8 or newer)" in absent.stdout
    ok, _ = run_bare_doctor(tmp_path / "ok", python="#!/bin/sh\necho 3.12.1\nexit 0\n", rc_hosts=True)
    assert "OK remote control interpreter: 3.12.1" in ok.stdout, ok.stdout
