import hashlib
import json
import os
import re
import shlex
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

from support import CLAUDE_MISSING, FAKE_SERVER_NO_DNS, HANG_TIMEOUT, load_script, needs_loopback, path_without, wait_until

ROOT = Path(__file__).resolve().parents[1]
RC_HOSTS_SCRIPT = ROOT / "addons" / "remote-control" / "ccmorph-rc-hosts"


def run_doctor(tmp_path, conf, curl=None, args=(), claude=None, home=True, env_extra=None, profile="demo",
               setup=None):
    state = tmp_path / "state"
    home_dir = tmp_path / "home"
    bindir = tmp_path / "bin"
    if home:
        (state / "profiles" / profile).mkdir(parents=True, exist_ok=True)
    state.mkdir(parents=True, exist_ok=True)
    bindir.mkdir(exist_ok=True)
    if not (state / f"{profile}.conf").exists():
        (state / f"{profile}.conf").write_text(conf)
    if not (state / "local.sh").exists():
        (state / "local.sh").write_text("printf marker > \"$DOCTOR_MARKER\"\n")
    (state / "local.sh").chmod(0o700)
    path = f"{bindir}:{os.environ['PATH']}"
    if claude is False:
        # No claude anywhere: a PATH the host's own claude cannot reach, and a
        # curl that answers nothing so no request leaves the machine.
        path = f"{bindir}:/usr/bin:/bin"
        if curl is None:
            curl = "#!/bin/sh\nprintf '%s\\n' '__CCMORPH_STATUS__000'\n"
    elif claude is None:
        # A healthy account profile is signed in, and doctor now asks claude
        # for that state the way add does — so the default stub must answer.
        claude = ('#!/bin/sh\n'
                  '[ "$1 $2" = "auth status" ] && '
                  'printf \'{"loggedIn": true, "authMethod": "claude.ai", '
                  '"email": "test@example.test"}\\n\'\n'
                  'exit 0\n')
    if claude is not False:
        (bindir / "claude").write_text(claude)
        (bindir / "claude").chmod(0o755)
    if curl is not None:
        c = bindir / "curl"
        c.write_text(curl)
        c.chmod(0o755)
    # The child is bin/ccmorph, a zsh script: zsh reads its startup files —
    # $ZDOTDIR/.zshenv, else $HOME/.zshenv — before the CLI runs, and an
    # operator's file can put a tool back on a PATH a test meant to hide it
    # from (GH-787: a .zshenv restoring brew changed the install step the
    # report named). HOME is this test's own empty fixture, so the startup
    # files are dropped too; a test that wants them names them in env_extra.
    env = {k: v for k, v in os.environ.items() if k not in ("ZDOTDIR", "FPATH")}
    env.update({"HOME": str(home_dir), "PATH": path,
                "CCMORPH_STATE_DIR": str(state), "CCMORPH_LIB_DIR": str(ROOT / "lib"),
                "CURL_ARGS": str(bindir / "curl.args"), "CURL_CALLED": str(bindir / "called"),
                "DOCTOR_MARKER": str(bindir / "doctor.marker")})
    if env_extra:
        env.update(env_extra)
    if setup:
        setup(state)   # the person's own files the report reads (GH-881: rc-hosts, an edge record)
    result = subprocess.run([str(ROOT / "bin/ccmorph"), "doctor", *args, profile], env=env,
                            capture_output=True, text=True, timeout=HANG_TIMEOUT)
    return result, state / f"{profile}.conf", bindir


def test_doctor_auth_redaction_and_no_mutation(tmp_path):
    curl = """#!/bin/sh
printf '%s\\n' "$@" > "$CURL_ARGS"
[ "$1" = -K ] && cat >> "$CURL_ARGS"
printf '%s\\n' '{"data":[{"id":"m-1"}]}'
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


@pytest.mark.parametrize("case", ["name-not-found-issue-260", "refused-issue-260",
                                  "secure-issue-260", "secure-name-mismatch-issue-260",
                                  "no-answer-issue-260"])
def test_doctor_names_the_one_cause_of_an_unreachable_gateway(tmp_path, case):
    """GH-260: doctor answered four transport failures with one timeout
    sentence that sent everyone to a private network. Each cause is named with
    its host and port, the advice rides only on the no-answer case, and curl's
    own words are read, never printed."""
    shapes = {
        "name-not-found-issue-260": ("https://gw.example.internal", 6,
            "curl: (6) Could not resolve host: gw.example.internal",
            "WARN gateway: name not found: gw.example.internal port 443", False),
        "refused-issue-260": ("http://127.0.0.1:9443", 7,
            "curl: (7) Failed to connect to 127.0.0.1 port 9443: Connection refused",
            "WARN gateway: connection refused: 127.0.0.1 port 9443", False),
        "secure-issue-260": ("https://127.0.0.1:18080", 35,
            "curl: (35) OpenSSL/3.0.13: error:0A00010B:SSL routines::wrong version number",
            "WARN gateway: secure connection failed: 127.0.0.1 port 18080", False),
        "secure-name-mismatch-issue-260": ("https://gw.example", 51,
            "curl: (51) SSL: no alternative certificate subject name matches target hostname 'gw.example'",
            "WARN gateway: secure connection failed: gw.example port 443", False),
        "no-answer-issue-260": ("http://10.255.255.1:8080", 28,
            "curl: (28) Failed to connect to 10.255.255.1 port 8080 after 2003 ms: Timeout was reached",
            "WARN gateway: no answer from 10.255.255.1 port 8080 after 2 seconds "
            "(if it is an internal endpoint, connect your private network and try again)", True),
    }
    base, code, complaint, want, advice = shapes[case]
    curl = f"#!/bin/sh\nprintf '%s\\n' {shlex.quote(complaint)} >&2\nexit {code}\n"
    result, _, _ = run_doctor(tmp_path, f"type=apikey\nbase_url={base}\napi_key=k\n", curl)
    out = result.stdout + result.stderr
    assert want in result.stdout, out
    assert ("private network" in result.stdout) == advice, out
    assert "did not answer within 5 seconds" not in out
    assert "curl:" not in out
    # GH-841: the verdict names the edit, not the launch that sent the person
    # here.
    assert result.stdout.splitlines()[-1] == (
        "WARN may not start: check gateway (see above); "
        "if the endpoint is wrong, change it: ccmorph config demo"), out
    assert "then: ccmorph run" not in out


@pytest.mark.parametrize("case", ["ready", "may-not-start"])
def test_doctor_exit_status_is_its_verdict_gh_970(tmp_path, case):
    """GH-970: doctor only ever answered whether it itself ran, so
    `ccmorph doctor gway && launch` read a profile whose own last line said
    `may not start` as healthy. The exit status is the verdict's: 0 only when
    the last line says ready, non-zero when it says may not start. The human
    report is word for word what the same fixture printed before."""
    if case == "ready":
        conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
        result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
        assert result.stdout.splitlines()[-1] == (
            "OK ready: start it with ccmorph run demo"), result.stdout
    else:
        curl = ("#!/bin/sh\n"
                "printf 'curl: (7) Failed to connect to 127.0.0.1 port 18787: "
                "Connection refused\\n' >&2\nexit 7\n")
        conf = "type=apikey\nbase_url=http://127.0.0.1:18787\napi_key=k\n"
        result, _, _ = run_doctor(tmp_path, conf, curl)
        assert "WARN gateway: connection refused: 127.0.0.1 port 18787" in result.stdout, result.stdout
        assert result.stdout.splitlines()[-1].startswith("WARN may not start"), result.stdout
    assert (result.returncode != 0) == (case == "may-not-start"), result.stdout


def test_doctor_reports_a_refused_api_key(tmp_path):
    # Claude Code stores a refusal in the profile's .claude.json; the credential
    # is still there, so only the stored answer tells doctor why a launch would
    # open the login menu. Its fix is one command, and doctor changes nothing.
    key = "sk-issue-236-doctor-key"
    home = tmp_path / "state" / "profiles" / "demo"
    home.mkdir(parents=True)
    doc = home / ".claude.json"
    body = json.dumps({"customApiKeyResponses": {"approved": [], "rejected": [key[-20:]]}})
    doc.write_text(body)
    # The key probe on Anthropic's endpoint is stubbed: no test reaches the network.
    result, _, _ = run_doctor(tmp_path, f"type=apikey\napi_key={key}\n", OK_CURL)
    assert result.returncode == 1, result.stdout + result.stderr
    assert "FAIL" in result.stdout
    assert "ccmorph login demo" in result.stdout
    assert doc.read_text() == body


def test_doctor_rejects_malformed_listing(tmp_path):
    curl = """#!/bin/sh
printf '%s\n' '{"data":"not-a-list"}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""
    result, _, _ = run_doctor(tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", curl)
    assert result.returncode != 0
    assert "malformed model listing" in result.stdout


@pytest.mark.parametrize("base", [" https://user:secret@gw.example/v1",
                                  "user:secret@gw.example/v1",
                                  "https:user:secret@gw.example/v1"])
def test_doctor_never_echoes_userinfo_of_an_invalid_endpoint(tmp_path, base):
    result, _, _ = run_doctor(tmp_path, f"type=apikey\nbase_url={base}\napi_key=k\n")
    assert "endpoint must be an http(s) URL" in result.stdout
    assert "endpoint:" in result.stdout
    assert "secret" not in result.stdout + result.stderr


OK_CURL = """#!/bin/sh
printf '%s\n' '{"data":[{"id":"m-1"}]}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""


@pytest.mark.parametrize("conf_line, marked", [("private=1\n", True), ("", False)],
                         ids=["private", "keeps-history"])
def test_doctor_names_the_history_a_private_profile_deletes_gh_292(tmp_path, conf_line, marked):
    # GH-292: private=1 deletes the session's conversation when it ends, and
    # doctor read every other promise the conf makes without saying so.
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n" + conf_line
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 0, result.stderr
    assert ("history: deleted when the session ends (private=1)" in result.stdout) == marked
@pytest.mark.parametrize(
    "conf,security_stub,expected",
    [("type=apikey\napi_key=plain-demo-key\n", None,
      "OK credentials: present in the conf file, unencrypted (value hidden)"),
     ("type=apikey\napi_key=keychain:demo.api_key\n", "sk-issue-262-keychain-key",
      "OK credentials: present in the login keychain (value hidden)")],
    ids=["plain-in-the-conf", "keychain-reference"])
def test_doctor_names_where_the_credential_is_stored_issue_262(tmp_path, conf, security_stub, expected):
    """#262: a person deciding whether to trust ccmorph with a key must be able
    to learn from doctor itself whether a profile's credential is in the login
    keychain or in the conf file, without the value ever being shown. The
    keychain reference is resolved through a `security` double, so the test says
    nothing about the host and touches no real keychain."""
    if security_stub is not None:
        bindir = tmp_path / "bin"
        bindir.mkdir(parents=True, exist_ok=True)
        security = bindir / "security"
        security.write_text(
            "#!/bin/sh\n"
            "[ \"$1\" = find-generic-password ] && [ \"$2\" = -s ] && [ \"$3\" = ccmorph ]"
            " && [ \"$4\" = -a ] && [ \"$5\" = demo.api_key ] && [ \"$6\" = -w ]"
            f" && printf '%s\\n' '{security_stub}' && exit 0\n"
            "exit 1\n")
        security.chmod(0o755)
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 0, result.stdout + result.stderr
    assert expected in result.stdout, result.stdout
    if security_stub is not None:
        assert security_stub not in result.stdout + result.stderr


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

    # The listing serves every id this test saves, so the label is all that varies.
    served = ",".join('{"id":"%s"}' % m for m in (
        "saved", "global", "shared-pin", "profile-model", "conf-model", "native-model"))
    listing = OK_CURL.replace('{"data":[{"id":"m-1"}]}', '{"data":[%s]}' % served)

    def effective(conf_text, profile_json, row="model"):
        profile_settings.write_text(profile_json)
        (tmp_path / "state" / "demo.conf").parent.mkdir(exist_ok=True)
        (tmp_path / "state" / "demo.conf").write_text(conf_text)
        result, path, _ = run_doctor(tmp_path, conf_text, listing)
        assert result.returncode == 0, result.stderr
        line = [l for l in result.stdout.splitlines() if f"effective {row}:" in l]
        assert len(line) == 1, result.stdout
        # Doctor is a read-only check: resolving the default must not seed the conf.
        assert path.read_text() == conf_text
        return line[0].split(f"effective {row}:", 1)[1].strip()

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
    # The words are the shared unsaved-state words (#381): this conf is a
    # gateway, so a launch asks which model at each launch.
    assert effective(conf + "model_defaults=1\n", '{"model":"native-model"}') \
        == "ccmorph asks which model at each launch (no saved model)"
    assert effective(conf, '{}') == "ccmorph asks which model at each launch (no saved model)"
    # The effort row names the level a plain launch passes (#381), resolved the
    # way the launch resolves it (#356): the override saved for the profile's
    # model first, then the profile's own default.
    assert effective(conf, '{"model":"profile-model","modelSettings":{"profile-model":{"effortLevel":"high"}}}',
                     "effort") == "high (profile override for profile-model)"
    assert effective(conf + "model=conf-model\neffort=low\n", '{}', "effort") == "low (profile default)"
    assert effective(conf + "model=conf-model\neffort=low\neffort.conf-model=max\n", '{}', "effort") \
        == "max (profile override for conf-model)"
    assert effective(conf + "model_defaults=1\n", '{"effortLevel":"high"}', "effort") \
        == "Claude Code's own default (no saved effort)"


MALFORMED_CURL = """#!/bin/sh
printf '%s\n' '{"data":"not-a-list"}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""


def status_curl(code):
    return f"#!/bin/sh\nprintf '%s\\n' '{{}}'\nprintf '%s\\n' '__CCMORPH_STATUS__{code}'\n"


GW_LISTING = """#!/bin/sh
printf '%s\\n' '{"data":[{"id":"glm-4.6"},{"id":"glm-4.5-air"}]}'
printf '%s\\n' '__CCMORPH_STATUS__200'
"""


def test_doctor_fails_while_the_saved_model_is_not_served_issue_267(tmp_path):
    # GH-267: a reachable listing that names models adjudicates the saved default.
    # Printing ready anyway promised a launch that fails at its first message.
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nmodel=glm-4.6-air\n"
    result, path, _ = run_doctor(tmp_path, conf, GW_LISTING)
    assert result.returncode != 0, result.stdout
    assert "model: 'glm-4.6-air' is not served by this gateway" in result.stdout
    assert "closest: glm-4.5-air, glm-4.6" in result.stdout
    assert "served: glm-4.5-air, glm-4.6" in result.stdout
    assert "FAIL not ready: choose a served model: ccmorph set demo model <model-id>" in result.stdout
    assert path.read_text() == conf


@pytest.mark.parametrize("rows", [
    "",
    '{"id":"embed-only","capabilities":{"chat_completion":false}}',
], ids=["empty-catalogue", "only-chat-denied"])
def test_doctor_fails_when_the_gateway_serves_no_chat_model_issue_267(tmp_path, rows):
    # A complete listing that serves nothing proves the saved model absent.
    listing = """#!/bin/sh
printf '%s\\n' '{"data":[ROWS]}'
printf '%s\\n' '__CCMORPH_STATUS__200'
""".replace("ROWS", rows)
    conf = "type=apikey\nbase_url=https://example.test\n" + "api_key=k\n" + "model=glm-4.6-air\n"
    result, _, _ = run_doctor(tmp_path, conf, listing)
    assert result.returncode != 0, result.stdout
    assert "model: 'glm-4.6-air' is not served by this gateway; served: (none)" in result.stdout
    # Nothing is served, so there is no model to choose: the fix is the gateway or its endpoint.
    assert result.stdout.splitlines()[-1] == (
        "FAIL not ready: make a chat model available on this gateway, "
        "or change the endpoint: ccmorph config demo"), result.stdout
    assert "choose a served model" not in result.stdout


def test_doctor_stays_ready_when_the_saved_model_is_served(tmp_path):
    # The refusal must not fire on a served default: an empty or unreadable
    # listing declines to adjudicate (covered elsewhere); a listing that names
    # the model is a clean bill of health.
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nmodel=glm-4.6\n"
    result, _, _ = run_doctor(tmp_path, conf, GW_LISTING)
    assert result.returncode == 0, result.stdout
    assert "ready: start it with ccmorph run demo" in result.stdout
    assert "is not served" not in result.stdout


def test_doctor_says_no_message_was_sent_issue_375(tmp_path):
    # #375: the gateway row vouches reachability from a model list alone — it
    # must say so, or "OK ready" reads as a passed message test on a gateway
    # that would refuse the first one.
    result, _, _ = run_doctor(tmp_path, GW, GW_LISTING)
    assert result.returncode == 0, result.stdout
    assert "gateway: reachable (HTTP 200); models: 2; no message was sent (the model list only)" in result.stdout
    assert "ready: start it with ccmorph run demo" in result.stdout


def test_doctor_does_not_call_a_listless_gateway_unreachable_issue_357(tmp_path):
    # #357: a gateway that answers HTTP is not unreachable — it offers no model
    # list. With no model saved no launch can pick one, so the verdict names
    # the one step instead of sending the reader to fix a working endpoint.
    result, _, _ = run_doctor(tmp_path, GW, status_curl(404))
    assert result.returncode == 1, result.stdout + result.stderr
    assert "unreachable" not in result.stdout, result.stdout
    assert "gateway: reachable (HTTP 404), but offers no model list" in result.stdout
    assert "FAIL not ready: set a model: ccmorph set demo model <model-id>" in result.stdout


def test_doctor_stays_ready_when_a_listless_gateway_has_a_saved_model_issue_357(tmp_path):
    # A saved model needs no listing: classification probes it directly, so the
    # profile runs — the report says the list is not offered and stays ready.
    result, _, _ = run_doctor(tmp_path, GW + "model=glm-4.6\n", status_curl(404))
    assert result.returncode == 0, result.stdout + result.stderr
    assert "gateway: reachable (HTTP 404), but offers no model list" in result.stdout
    assert "ready: start it with ccmorph run demo" in result.stdout
    assert "not ready" not in result.stdout


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
# own CCMORPH_SHIM_HOOKS build still gets a spawned hooks shim. The /bin/echo
# process is the decoy a substring match would mistake for the adapter.
_ADAPTER_SHAPE_CHILDREN = {
    "translator-route": ("cliproxyapi",),
    "native-route": (),
    "hooks-build": ("cliproxyapi", "hooks-shim"),
}


def _adapter_tree(tmp_path, profile, shape):
    """A live session on profile, as real processes with the argv a launch
    itself runs: `ccmorph-core adapter <profile> --auto --parent-pid 400` —
    the parent a session keeps naming once adapter-start has exited — the
    children behind the python host a wrapped install uses, and a decoy whose
    joined command line names an adapter it is not. A process table's rows are
    joined text and cannot say where an argument ends; the kernel's argv can,
    so the session is the real thing, read through the real table. Returns the
    decoy's pid, the listener holders doctor must check ((subject, pid), ...),
    lsof's answer for each holder (pid -> (name, address)), and a stop() that
    ends the session."""
    fake = tmp_path / "fakebin"
    ready = tmp_path / "ready"
    fake.mkdir(parents=True, exist_ok=True)
    ready.mkdir(parents=True, exist_ok=True)
    # Each process is an executable script of ours under the real interpreter,
    # so the kernel's argv is [<python>, <script>, args...]: the python-host
    # shape a wrapped install runs. The interpreter is never run under a
    # spoofed argv[0], which CPython uses to find its own library. A sleeper
    # reports itself ready by naming its pid; the adapter's own host spawns the
    # children and records their pids, so no caller races the kernel's argv.
    sleeper = ("import os, time\n"
               "open(os.path.join(os.environ['FAKE_READY_DIR'], str(os.getpid())), 'w').close()\n"
               "time.sleep(600)\n")
    host = ("import json, os, subprocess, time\n"
            "kids = {}\n"
            "for name, argv in json.loads(os.environ['FAKE_CHILDREN']):\n"
            "    kids[name] = subprocess.Popen(\n"
            "        argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).pid\n"
            "tmp = os.environ['FAKE_PIDS'] + '.tmp'\n"
            "with open(tmp, 'w') as f:\n"
            "    json.dump(kids, f)\n"
            "os.replace(tmp, os.environ['FAKE_PIDS'])\n"
            "time.sleep(600)\n")
    for name, body in (("ccmorph-core", host), ("cliproxyapi", sleeper),
                       ("hooks-shim", sleeper), ("decoy", sleeper)):
        (fake / name).write_text(f"#!{sys.executable}\n" + body)
        (fake / name).chmod(0o755)
    children = {
        "cliproxyapi": [str(fake / "cliproxyapi"), "-config", "/tmp/config.yaml"],
        "hooks-shim": [str(fake / "hooks-shim"), "4568", "http://127.0.0.1:4567"],
    }
    wanted = list(_ADAPTER_SHAPE_CHILDREN[shape])
    pids_path = tmp_path / "adapter-pids.json"
    env = dict(os.environ, FAKE_CHILDREN=json.dumps([[n, children[n]] for n in wanted]),
               FAKE_READY_DIR=str(ready), FAKE_PIDS=str(pids_path))
    started = {}

    def stop():
        # The adapter's session group holds it and its children; the decoy,
        # parented here, is ended on its own.
        adapter = started.get("adapter")
        if adapter is not None:
            try:
                os.killpg(adapter.pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass
            adapter.wait()
        decoy = started.get("decoy")
        if decoy is not None:
            decoy.kill()
            decoy.wait()

    try:
        started["adapter"] = subprocess.Popen(
            [str(fake / "ccmorph-core"), "adapter", profile, "--auto", "--parent-pid", "400"],
            start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
        started["decoy"] = subprocess.Popen(
            [str(fake / "decoy"), "ccmorph-core", "adapter", profile],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
        adapter, decoy = started["adapter"], started["decoy"]
        kids = {}

        def came_up():
            nonlocal kids
            kids = json.loads(pids_path.read_text()) if pids_path.exists() else {}
            if (set(kids) == set(wanted)
                    and all((ready / str(pid)).exists()
                            for pid in [decoy.pid, *kids.values()])):
                return True
            if adapter.poll() is not None or decoy.poll() is not None:
                raise AssertionError("the fake session exited before it came up")
            return False
        if not wait_until(came_up, HANG_TIMEOUT):
            raise AssertionError("the fake session never came up")
    except BaseException:
        stop()
        raise
    if shape == "native-route":
        listeners = (("in-process shim", adapter.pid),)
        ports = {str(adapter.pid): ("ccmorph-core", "127.0.0.1:4568")}
    elif shape == "hooks-build":
        listeners = (("cliproxyapi child", kids["cliproxyapi"]),
                     ("hooks-shim child", kids["hooks-shim"]))
        ports = {str(kids["cliproxyapi"]): ("cliproxyapi", "127.0.0.1:4567"),
                 str(kids["hooks-shim"]): ("hooks-shim", "127.0.0.1:4568")}
    else:
        listeners = (("cliproxyapi child", kids["cliproxyapi"]),
                     ("in-process shim", adapter.pid))
        ports = {str(kids["cliproxyapi"]): ("cliproxyapi", "127.0.0.1:4567"),
                 str(adapter.pid): ("ccmorph-core", "127.0.0.1:4568")}
    return decoy.pid, listeners, ports, stop


def _adapter_stubs(bindir, slow, ports):
    """Install the PATH doubles doctor's read-only inspections consult: an lsof
    that answers for the holders the session started, and — only where the
    table's read is a ps, off darwin — a ps that outlives doctor's one-second
    bound (GH-467): the deadline kills it, and doctor's designed answer is
    SKIP. darwin's table is the kernel's own sysctl, which no PATH double
    reaches, so there the session is always the real one."""
    bindir.mkdir(parents=True, exist_ok=True)
    if slow == "ps":
        (bindir / "ps").write_text("#!/bin/sh\nsleep 2\n")
        (bindir / "ps").chmod(0o755)
    # lsof answers per pid, like the real one: a matching row and exit 0, or
    # nothing and exit 1.
    rows = ["#!/bin/sh"]
    if slow == "lsof":
        rows.append("sleep 2")
    rows.append("printf '%s\\n' 'COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME'")
    rows.append("case \"$4\" in")
    for pid, (name, addr) in sorted(ports.items()):
        rows.append(f"  {pid}) printf '%s\\n' "
                    f"'{name} {pid} u 3u IPv4 0x0 0t0 TCP {addr} (LISTEN)'; exit 0;;")
    rows.append("esac")
    rows.append("exit 1")
    (bindir / "lsof").write_text("\n".join(rows) + "\n")
    (bindir / "lsof").chmod(0o755)


def assert_adapter_report(out, decoy, listeners):
    """Accept every answer doctor promises for the running-adapter report, and
    only those: each read-only inspection (the process table, then lsof per
    listener holder) is bounded at one second, and a bound blown under load is
    answered SKIP (GH-467). A dropped report, a WARN, or the decoy pid stays
    red in every outcome."""
    assert f"pid {decoy}" not in out  # the /bin/echo decoy is never an adapter
    if "could not tell whether a translator is running" in out:
        # the table read blew its bound: one SKIP line is the whole answer —
        # nothing half-reported, nothing started.
        assert "listener" not in out, out
        return
    for subject, pid in listeners:
        reported = (f"adapter: {subject} listener" in out
                    and f"(pid {pid}; read-only check)" in out)
        unknown = f"adapter: {subject} listener status unknown (pid {pid})" in out
        assert reported or unknown, out


# The adapter line's answers (GH-272). Whether cliproxyapi is installed depends
# on the machine, so the line that says so is matched, not compared.
TRANSLATOR_NOW = ("OK adapter: this profile needs a translator now: "
                  "an open session on it is running cliproxyapi")
NO_TRANSLATOR_NOW = ("OK adapter: this profile needs no translator now: "
                     "its open session runs without one")
def _until_a_model(profile):
    return re.compile(
        r"INFO adapter: this profile needs no translator until a model is chosen "
        r"\(none is set, and the endpoint's model list names no protocol\); "
        r"cliproxyapi is (not )?installed; choose one: ccmorph set "
        + profile + " model <model-id>")


TABLE_UNREAD = "SKIP adapter: could not tell whether a translator is running"


def _doctor_until_table_read(tmp_path, conf, curl, profile, tries=3):
    """(verbose run, its summary adapter lines, whether its table read was
    lost, bindir). One --verbose run answers both: its summary line and its
    SKIP marker come from the same process-table read, so a lost read is never
    judged by another run's answer. Asked again only while the read lost the
    one-second bound (a loaded machine, #902); a try that read the table
    answers at once, right or wrong, so a retry never hides a wrong answer."""
    for _ in range(tries):
        verbose, _, bindir = run_doctor(tmp_path, conf, curl, args=("--verbose",),
                                        profile=profile)
        lost = TABLE_UNREAD in verbose.stdout
        if not lost:
            break
    # Summary lines say "adapter: this ..."; the verbose detail lines never do.
    summary = [l for l in verbose.stdout.splitlines() if "adapter: this " in l]
    return verbose, summary, lost, bindir


@pytest.mark.parametrize("slow", ["", "ps", "lsof"],
                         ids=["prompt", "ps-over-deadline-issue-467",
                              "lsof-over-deadline-issue-467"])
@pytest.mark.parametrize("shape", ["translator-route", "native-route", "hooks-build"])
def test_doctor_reports_existing_adapter_without_starting_it(tmp_path, shape, slow):
    curl = """#!/bin/sh
printf '%s\n' '{"data":[{"id":"m-1"}]}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nadapter=true\n"
    # A profile of this test's own, so the machine's real sessions never match.
    profile = "demo" + str(os.getpid())
    # The session first, then the PATH stubs: run_doctor only fills in what is
    # missing, so doctor's single run already consults them (no separate
    # unstubbed setup run).
    decoy, listeners, ports, stop = _adapter_tree(tmp_path, profile, shape)
    _adapter_stubs(tmp_path / "bin", slow, ports)
    try:
        verbose, adapters, lost, bindir = _doctor_until_table_read(
            tmp_path, conf, curl, profile)
        assert verbose.returncode == 0, verbose.stderr
        # One adapter line by default, in every case (also when the table read
        # blew its bound): whether this profile needs a translator now, never
        # a pid or a process-inspection detail a first-time reader cannot act
        # on. Nothing is classified and no model is set, so the answer is what
        # the open session shows; with no session readable, that none starts
        # until a model is chosen. A running adapter alone is not a
        # translator: only a cliproxyapi child is.
        assert len(adapters) == 1, verbose.stdout
        result, _, _ = run_doctor(tmp_path, conf, curl, profile=profile)
        assert result.returncode == 0, result.stderr
        assert len([l for l in result.stdout.splitlines() if "adapter:" in l]) == 1, \
            result.stdout
        if slow == "ps" and sys.platform != "darwin":
            # Off darwin the table's read is a ps, so this run can lose it to
            # the bound.
            assert _until_a_model(profile).fullmatch(adapters[0]), verbose.stdout
        elif lost and _until_a_model(profile).fullmatch(adapters[0]):
            # Every try lost the table's one-second bound under load (GH-467,
            # #902): the designed answer then is the until-a-model line.
            pass
        elif shape == "native-route":
            assert adapters == [NO_TRANSLATOR_NOW], verbose.stdout
        else:
            assert adapters == [TRANSLATOR_NOW], verbose.stdout
        # Read-only: the profile's own hook never ran, so nothing was started.
        assert not (bindir / "doctor.marker").exists()
        assert_adapter_report(verbose.stdout, decoy, listeners)
        assert not (bindir / "doctor.marker").exists()
    finally:
        stop()


@pytest.mark.parametrize("shape,suffix", [
    ("translator-route", "; an open session on this profile is using one now"),
    ("native-route", ""),
], ids=["translator-running", "shim-only"])
def test_doctor_adapter_line_names_a_running_translator_issue_272(tmp_path, shape, suffix):
    """GH-272: with a classification cached, the line states the need, and an
    open session is added only when a cliproxyapi child is really running."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    # A profile of this test's own, so the machine's real sessions never match.
    profile = "demo" + str(os.getpid())
    _, _, ports, stop = _adapter_tree(tmp_path, profile, shape)
    _adapter_stubs(tmp_path / "bin", "", ports)
    try:
        _seed_adapter_cache(tmp_path, "https://example.test", [("m-openai", "responses")])
        stub = tmp_path / "bin" / "cliproxyapi"
        stub.write_text("#!/bin/sh\nexit 0\n")
        stub.chmod(0o755)
        verbose, adapters, lost, _ = _doctor_until_table_read(
            tmp_path, conf, OK_CURL, profile)
        assert verbose.returncode == 0, verbose.stdout + verbose.stderr
        need = "OK adapter: this endpoint needs a translator; cliproxyapi is installed"
        # Only a run whose table read was lost may leave the session out.
        assert adapters == [need + suffix] or lost and adapters == [need], verbose.stdout
    finally:
        stop()


def _status_lines(stdout):
    """(kind, subject) for every report line. The subject — the text before the
    first colon — is what the verdict line groups by, so it is also where a
    contradiction (an OK line and a FAIL line about the same thing) would live."""
    out = []
    for line in stdout.splitlines():
        kind, _, rest = line.partition(" ")
        if kind in {"OK", "FAIL", "WARN", "INFO", "SKIP"}:
            subject, _, _ = rest.partition(":")
            out.append((kind, subject))
    return out


def _seed_adapter_cache(tmp_path, base, rows):
    """The endpoint-keyed classification cache a launch writes (the same file
    adapter/classify.go writes and re-reads): doctor's adapter line reports
    what launch learned there, so the fixture mirrors its keying exactly."""
    key = hashlib.sha256(base.encode()).hexdigest()[:16]
    cache = tmp_path / "state" / "profiles" / ".adapter-cache" / f"{key}.json"
    cache.parent.mkdir(parents=True, exist_ok=True)
    cache.write_text(json.dumps({
        "endpoint": base, "anthropic_path": "/v1/messages", "thinking_probe": 2,
        "rows": [{"model": model, "dialect": dialect, "note": "", "status": 200,
                  "cap": None, "nothink": False, "id_recycle": False}
                 for model, dialect in rows]}))


@pytest.mark.parametrize("route", ["native", "gateway", "token"])
def test_doctor_never_calls_a_rejected_key_fine_issue_272(tmp_path, route):
    """GH-272: the approved line reported Claude Code's locally stored answer,
    which says nothing about the endpoint, so a rejected key was called fine
    two lines above its FAIL. When the endpoint rejects the key, no line may
    call it fine, and no subject may carry both OK and FAIL."""
    key = "sk-issue-272-rejected-key"
    home = tmp_path / "state" / "profiles" / "demo"
    home.mkdir(parents=True)
    (home / ".claude.json").write_text(json.dumps(
        {"customApiKeyResponses": {"approved": [key[-20:]], "rejected": []}}))
    curl = '#!/bin/sh\nprintf \'%s\\n\' \'{"data":[]}\'\nprintf \'%s\\n\' \'__CCMORPH_STATUS__401\'\n'
    conf = f"type=apikey\napi_key={key}\n"
    if route == "gateway":
        conf += "base_url=https://example.test\n"
    if route == "token":
        conf = f"type=token\ntoken={key}\n"
    result, _, _ = run_doctor(tmp_path, conf, curl)
    assert result.returncode == 1, result.stdout + result.stderr
    assert "approved" not in result.stdout, result.stdout
    assert "replace it: ccmorph config demo" in result.stdout, result.stdout
    kinds_by_subject = {}
    for kind, subject in _status_lines(result.stdout):
        kinds_by_subject.setdefault(subject, set()).add(kind)
    for subject, kinds in kinds_by_subject.items():
        assert not ({"OK", "FAIL"} <= kinds), (subject, result.stdout)
    # A renamed subject must not hide the contradiction: the credential is
    # never called present, accepted or fine above its own rejection.
    for kind, subject in _status_lines(result.stdout):
        assert not (kind == "OK" and subject in {"credentials", "api key", "token"}), result.stdout
    assert "credentials: present" not in result.stdout, result.stdout
    expected = {
        "native": "FAIL credentials: Anthropic rejected the API key (HTTP 401); replace it: ccmorph config demo",
        "gateway": "FAIL gateway: rejected the API key (HTTP 401); replace it: ccmorph config demo",
        "token": "FAIL credentials: Anthropic rejected the token (HTTP 401); replace it: ccmorph config demo",
    }[route]
    assert expected in result.stdout.splitlines(), result.stdout


def test_doctor_lines_stay_plain_words_issue_272(tmp_path):
    """GH-272: doctor spoke in internals — layered JSON, canonical settings,
    process inspection — none of which a first-time reader can act on. Every
    line, --verbose included, names what was checked in plain words, and a
    non-OK line names the way out."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    session = "demo" + str(os.getpid())
    settings = tmp_path / "state" / "profiles" / session
    settings.mkdir(parents=True)
    (settings / "settings.json").write_text('{"model":"glm-4.6"}')
    # The running-adapter answer in plain words: off darwin the table's read is
    # a ps this run loses to its one-second bound, and the whole answer is one
    # SKIP line (GH-467's nothing-half-reported, reworded); darwin's sysctl
    # answers first, so the wording is the session's own listener checks.
    decoy, listeners, ports, stop = _adapter_tree(tmp_path, session, "translator-route")
    _adapter_stubs(tmp_path / "bin", "ps", ports)
    try:
        result, _, _ = run_doctor(tmp_path, conf, GW_LISTING, args=("--verbose",),
                                  profile=session)
        assert result.returncode == 0, result.stdout + result.stderr
        for banned in ("layered JSON", "canonical settings", "process inspection"):
            assert banned not in result.stdout, (banned, result.stdout)
        assert "profile settings: valid JSON (" in result.stdout, result.stdout
        if sys.platform != "darwin":
            assert "SKIP adapter: could not tell whether a translator is running" in result.stdout
        else:
            assert_adapter_report(result.stdout, decoy, listeners)
        # A broken settings file is told how to be dealt with, not just that it is broken.
        (settings / "settings.json").write_text("{ invalid")
        broken, _, _ = run_doctor(tmp_path, conf, GW_LISTING, profile=session)
        assert "invalid JSON" in broken.stdout
        assert f"({settings / 'settings.json'}); fix that file" in broken.stdout, broken.stdout
    finally:
        stop()


def test_doctor_names_the_settings_file_that_is_actually_broken_issue_272(tmp_path):
    """GH-272: a malformed shared ~/.claude/settings.json was reported against
    the profile's own (valid) file, with advice to fix or remove that one —
    which would lose the profile's settings and leave the real fault in place."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    profile = tmp_path / "state" / "profiles" / "demo"
    profile.mkdir(parents=True)
    (profile / "settings.json").write_text('{"model":"glm-4.6"}')
    shared = tmp_path / "home" / ".claude" / "settings.json"
    shared.parent.mkdir(parents=True)
    shared.write_text("{ invalid")
    result, _, _ = run_doctor(tmp_path, conf, GW_LISTING)
    assert result.returncode == 1, result.stdout + result.stderr
    assert f"({shared}); fix that file" in result.stdout, result.stdout
    assert str(profile / "settings.json") not in result.stdout, result.stdout


def test_doctor_names_the_command_not_the_script_path_issue_272(tmp_path):
    """GH-272: the ccmorph line printed the internal wrapper script's path;
    it names the command the person types instead."""
    result, _, _ = run_doctor(
        tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", OK_CURL)
    assert result.returncode == 0, result.stdout + result.stderr
    lines = result.stdout.splitlines()
    assert any(l.startswith("OK ccmorph: ") and l.endswith(" (command: ccmorph)")
               for l in lines), result.stdout
    assert not any("ccmorph.sh" in l for l in lines), result.stdout


@pytest.mark.parametrize("cache_rows,stub,expected", [
    (None, False, None),
    ([("m-anthropic", "anthropic")], False,
     "OK adapter: this endpoint needs no translator"),
    ([("m-openai", "responses")], True,
     "OK adapter: this endpoint needs a translator; cliproxyapi is installed"),
], ids=["no-cache-no-model", "native-endpoint", "translator-endpoint"])
def test_doctor_adapter_line_states_the_need_issue_272(tmp_path, cache_rows, stub, expected):
    """GH-272: the adapter line said only that launch 'starts a compatibility
    adapter if this endpoint needs one'. It now answers the question — whether
    this endpoint needs a translator, and whether cliproxyapi is installed —
    from the classification cache a launch itself consults; with nothing cached
    and no model set it says no translator starts until a model is chosen.
    (The not-installed wording is pinned in util/doctor_test.go: a machine with
    cliproxyapi installed cannot produce that verdict end to end.)"""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    if cache_rows is not None:
        _seed_adapter_cache(tmp_path, "https://example.test", cache_rows)
    if stub:
        s = tmp_path / "bin" / "cliproxyapi"
        s.parent.mkdir(parents=True, exist_ok=True)
        s.write_text("#!/bin/sh\nexit 0\n")
        s.chmod(0o755)
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 0, result.stdout + result.stderr
    adapters = [l for l in result.stdout.splitlines() if "adapter:" in l]
    assert len(adapters) == 1, result.stdout
    if cache_rows is None:
        assert _until_a_model("demo").fullmatch(adapters[0]), result.stdout
    else:
        assert adapters == [expected], result.stdout


@pytest.mark.parametrize("path,stub,expected", [
    ("/v1/messages", False, "OK adapter: this endpoint needs no translator"),
    ("/v1/chat/completions", True,
     "OK adapter: this endpoint needs a translator; cliproxyapi is installed"),
], ids=["declared-messages", "declared-chat"])
def test_doctor_adapter_line_reads_declared_protocols_issue_272(tmp_path, path, stub, expected):
    """GH-272: with nothing cached, a model the gateway's own listing declares
    speaks a protocol a launch reads without any request, so doctor says
    whether the translator is needed from that declaration."""
    curl = ("#!/bin/sh\nprintf '%s\\n' '"
            + json.dumps({"data": [{"id": "m-1", "supported_endpoints": [path]}]})
            + "'\nprintf '%s\\n' '__CCMORPH_STATUS__200'\n")
    if stub:
        s = tmp_path / "bin" / "cliproxyapi"
        s.parent.mkdir(parents=True, exist_ok=True)
        s.write_text("#!/bin/sh\nexit 0\n")
        s.chmod(0o755)
    result, _, _ = run_doctor(
        tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", curl)
    assert result.returncode == 0, result.stdout + result.stderr
    adapters = [l for l in result.stdout.splitlines() if "adapter:" in l]
    assert adapters == [expected], result.stdout


@pytest.mark.parametrize("with_session,expected", [
    (False, None),
    (True, TRANSLATOR_NOW),
], ids=["no-session-line-omitted", "session-with-translator"])
def test_doctor_omits_the_adapter_line_for_an_unclassified_saved_model_issue_272(tmp_path, with_session, expected):
    """GH-272: a saved model nothing has classified can only be answered by a
    billed check, which doctor never sends: the line is left out rather than
    guessed. An open session that shows a translator still says so."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nmodel=glm-4.6\n"
    # A profile of this test's own, so the machine's real sessions never match.
    session = "demo" + str(os.getpid())
    stop = None
    if with_session:
        _, _, ports, stop = _adapter_tree(tmp_path, session, "translator-route")
        _adapter_stubs(tmp_path / "bin", "", ports)
    try:
        result, _, _ = run_doctor(tmp_path, conf, GW_LISTING, profile=session)
        assert result.returncode == 0, result.stdout + result.stderr
        adapters = [l for l in result.stdout.splitlines() if "adapter:" in l]
        if expected is None:
            assert adapters == [], result.stdout
            assert f"ready: start it with ccmorph run {session}" in result.stdout, result.stdout
        else:
            assert adapters == [expected], result.stdout
    finally:
        if stop is not None:
            stop()


def test_doctor_tolerates_a_conf_that_still_declares_removed_keys(tmp_path):
    """Removed keys (transport, forward_ip, subagent_model, lean) were removed,
    not reserved: an old conf carrying any or all of them must still be checkable."""
    curl = "#!/bin/sh\nprintf '%s\\n' '{\"data\":[{\"id\":\"m-1\"}]}'\nprintf '%s\\n' '__CCMORPH_STATUS__200'\n"
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


@pytest.mark.parametrize("typ", ["apikey", "token"])
def test_doctor_missing_credential_fails_without_request(tmp_path, typ):
    # Either way the verdict must stop short of ready with no request sent;
    # the apikey conf is the one shape the schema still accepts without a
    # credential, so doctor names it itself, while a token profile with no
    # token is refused the way launch would refuse it.
    result, _, _ = run_doctor(tmp_path, f"type={typ}\nbase_url=https://example.test\n")
    assert result.returncode != 0
    assert not result.stdout.splitlines()[-1].startswith("OK ready"), result.stdout
    if typ == "apikey":
        assert "has no credential" in result.stdout
        assert "no API credential" in result.stdout
    else:
        assert "invalid profile schema" in result.stdout
        assert "has no token" in result.stderr


def test_doctor_native_profile_without_base_url_is_clear(tmp_path):
    # A sub profile whose sign-in Claude Code keeps in the profile home needs
    # no base_url: its endpoint is Anthropic's own, and no adapter is started.
    home = tmp_path / "state" / "profiles" / "demo"
    home.mkdir(parents=True)
    (home / ".credentials.json").write_text(
        json.dumps({"claudeAiOauth": {"accessToken": SUB_SIGNIN}}))
    result, _, _ = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n", OK_CURL)
    assert result.returncode == 0, result.stdout
    assert "OK credentials: Anthropic accepted the stored sign-in (HTTP 200)" in result.stdout
    assert "adapter:" not in result.stdout


def test_doctor_names_the_fix_for_a_signed_out_account_profile(tmp_path):
    """GH-256: doctor called a signed-out Claude account profile ready. It must
    say the profile is not signed in and name the command that signs it in
    again — the same command cred and quota name. Signed-out is what a real
    `claude auth status` reports: loggedIn false, exit 1."""
    claude = ('#!/bin/sh\n'
              '[ "$1 $2" = "auth status" ] && '
              'printf \'{"loggedIn": false, "authMethod": "none"}\\n\' && exit 1\n'
              'exit 0\n')
    result, _, _ = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n", claude=claude)
    assert result.returncode == 1, result.stdout
    assert "credentials: not signed in — sign in again: ccmorph login demo" in result.stdout
    assert "FAIL not ready: sign in again: ccmorph login demo" in result.stdout
    assert "kept in the profile home" not in result.stdout


def test_doctor_admits_when_it_cannot_read_the_sign_in_state(tmp_path):
    """When claude answers nothing doctor must not guess: it says plainly it
    cannot tell, warns in the verdict, and does not fail a profile it could
    not read."""
    claude = "#!/bin/sh\nexit 0\n"
    result, _, _ = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n", claude=claude)
    # Unread is not healthy, and since GH-970 the exit code says so too: a
    # verdict of may-not-start is exactly what a script must not launch on.
    assert result.returncode != 0, result.stdout
    assert "credentials: cannot tell" in result.stdout
    assert "not signed in" not in result.stdout
    assert "may not start: check credentials" in result.stdout
    assert "ready: start it" not in result.stdout


@pytest.mark.parametrize("base", ["", "https://api.anthropic.com"],
                         ids=["no-base-url", "anthropic-base-url"])
@pytest.mark.parametrize("code", ["401", "200"])
def test_doctor_reports_a_rejected_native_key(tmp_path, base, code):
    """An apikey profile on Anthropic's own endpoint gets the same key check a
    gateway profile gets: a rejected key is a FAIL naming the command that
    replaces it, so the most common reason a profile fails is caught here."""
    curl = f"""#!/bin/sh
printf '%s\\n' "$@" > "$CURL_ARGS"
[ "$1" = -K ] && cat >> "$CURL_ARGS"
printf '%s\\n' '{{"data":[]}}'
printf '%s\\n' '__CCMORPH_STATUS__{code}'
"""
    conf = "type=apikey\napi_key=k\n"
    if base:
        conf += f"base_url={base}\n"
    result, _, bindir = run_doctor(tmp_path, conf, curl)
    assert "adapter:" not in result.stdout
    args = (bindir / "curl.args").read_text()
    assert "https://api.anthropic.com/v1/models?limit=1000" in args
    assert "anthropic-version: 2023-06-01" in args
    if code == "401":
        assert result.returncode == 1, result.stdout
        assert ("FAIL credentials: Anthropic rejected the API key (HTTP 401); "
                "replace it: ccmorph config demo") in result.stdout
        assert "credentials: present" not in result.stdout, result.stdout
    else:
        assert result.returncode == 0, result.stdout
        assert "OK endpoint: Anthropic accepted the API key (HTTP 200)" in result.stdout
        assert "OK credentials: present in the conf file, unencrypted (value hidden)" in result.stdout, result.stdout


@pytest.mark.parametrize("code", ["401", "200", "none"])
def test_doctor_on_a_token_profile_probes_the_token(tmp_path, code):
    """A token profile used to print the neutral sign-in line and a ready
    verdict whatever the token was: the issue's 'abc' was ready until launch
    answered 401. Doctor now asks Anthropic the one question that can vouch
    for the token, Bearer-only like the launch that will spend it, and says
    so when it could not check."""
    token = "sk-ant-oat01-example-token-0123456789abcdef"
    if code == "none":
        curl = "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CURL_ARGS\"\n[ \"$1\" = -K ] && cat >> \"$CURL_ARGS\"\nexit 1\n"
    else:
        curl = f"""#!/bin/sh
printf '%s\\n' "$@" > "$CURL_ARGS"
[ "$1" = -K ] && cat >> "$CURL_ARGS"
printf '%s\\n' '{{"data":[]}}'
printf '%s\\n' '__CCMORPH_STATUS__{code}'
"""
    result, _, bindir = run_doctor(tmp_path, f"type=token\ntoken={token}\n", curl)
    if code == "401":
        assert result.returncode == 1, result.stdout
        assert ("FAIL credentials: Anthropic rejected the token (HTTP 401); "
                "replace it: ccmorph config demo") in result.stdout
        assert "credentials: present" not in result.stdout, result.stdout
        assert not result.stdout.splitlines()[-1].startswith("OK ready"), result.stdout
    elif code == "200":
        assert result.returncode == 0, result.stdout
        assert "OK credentials: Anthropic accepted the token (HTTP 200)" in result.stdout
        assert "OK credentials: present in the conf file, unencrypted (value hidden)" in result.stdout, result.stdout
    else:
        assert "WARN endpoint: Anthropic did not answer, so the token was not checked" in result.stdout
        assert "rejected the token" not in result.stdout
        # An unanswered probe is not a rejection: the credential is still present.
        assert "OK credentials: present in the conf file, unencrypted (value hidden)" in result.stdout, result.stdout
    # However it answered, the request itself is the one a launch sends: the
    # token as a Bearer against Anthropic's listing, never an x-api-key, never
    # on a command line.
    args = (bindir / "curl.args").read_text()
    assert "https://api.anthropic.com/v1/models?limit=1000" in args
    assert "anthropic-version: 2023-06-01" in args
    bearer = f'header = "Authorization: Bearer {token}"'
    lines = args.splitlines()
    assert bearer in lines
    assert not any(token in line for line in lines if line != bearer)
    assert not any("x-api-key" in line for line in lines)


# The sign-in a sub profile's home can hold: Claude Code's own file layout for
# a claude.ai account, written where it cannot use the keychain.
SUB_SIGNIN = "{{API_KEY_jn4wq8v2ytse}}"


@pytest.mark.parametrize("code", ["401", "200", "none"])
def test_doctor_on_a_sub_profile_probes_the_stored_sign_in(tmp_path, code):
    """A sub profile used to print only the neutral sign-in line and a ready
    verdict whatever the stored sign-in was: where Claude Code keeps the
    sign-in in the profile home (.credentials.json), doctor now reads it and
    asks Anthropic the one question that can vouch for it, Bearer-only like the
    launch that will spend it. A sign-in is refreshable by design, so a
    rejection is a warning naming what the next session does with it, not a
    failed profile."""
    home = tmp_path / "state" / "profiles" / "demo"
    home.mkdir(parents=True)
    (home / ".credentials.json").write_text(
        json.dumps({"claudeAiOauth": {"accessToken": SUB_SIGNIN,
                                      "refreshToken": "r", "expiresAt": "e"}}))
    if code == "none":
        curl = "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CURL_ARGS\"\n[ \"$1\" = -K ] && cat >> \"$CURL_ARGS\"\nexit 1\n"
    else:
        curl = f"""#!/bin/sh
printf '%s\\n' "$@" > "$CURL_ARGS"
[ "$1" = -K ] && cat >> "$CURL_ARGS"
printf '%s\\n' '{{"data":[]}}'
printf '%s\\n' '__CCMORPH_STATUS__{code}'
"""
    result, _, bindir = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n", curl)
    if code == "401":
        assert ("WARN credentials: Anthropic rejected the stored sign-in (HTTP 401); "
                "the next session refreshes it if it can, else sign in again: ccmorph login demo") in result.stdout
        assert not result.stdout.splitlines()[-1].startswith("OK ready"), result.stdout
    elif code == "200":
        assert result.returncode == 0, result.stdout
        assert "OK credentials: Anthropic accepted the stored sign-in (HTTP 200)" in result.stdout
    else:
        assert "WARN endpoint: Anthropic did not answer, so the sign-in was not checked" in result.stdout
        assert "rejected the stored sign-in" not in result.stdout
    # However it answered, the request itself is the one a launch sends: the
    # sign-in as a Bearer against Anthropic's listing, never an x-api-key,
    # never on a command line.
    args = (bindir / "curl.args").read_text().splitlines()
    assert "https://api.anthropic.com/v1/models?limit=1000" in args
    assert "anthropic-version: 2023-06-01" in args
    bearer = f'header = "Authorization: Bearer {SUB_SIGNIN}"'
    assert bearer in args
    assert not any(SUB_SIGNIN in line for line in args if line != bearer)
    assert not any("x-api-key" in line for line in args)


def test_doctor_on_a_sub_profile_without_a_readable_sign_in_does_not_vouch(tmp_path):
    """Where Claude Code keeps the sign-in outside the profile home (macOS
    keeps it in the login keychain), doctor cannot ask Anthropic about it: the
    report says the sign-in was not checked and the verdict stops short of
    ready, instead of inheriting the old neutral line's confidence. The report
    keeps its shape with no sign-in to read: no adapter is started on
    Anthropic's own endpoint, no SKIP line appears, and every line carries its
    status word."""
    curl = "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CURL_ARGS\"\n[ \"$1\" = -K ] && cat >> \"$CURL_ARGS\"\nexit 1\n"
    result, _, bindir = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n", curl)
    assert ("WARN credentials: the sign-in was not checked (no stored sign-in could be read; "
            "the next session refreshes it if it can)") in result.stdout
    assert result.stdout.splitlines()[-1].startswith("WARN may not start"), result.stdout
    # Since GH-970 the exit code carries the verdict a reader without a
    # terminal has: may not start is not a green light.
    assert result.returncode != 0, result.stdout
    # The report keeps its shape with no sign-in to read: the endpoint is
    # Anthropic's own, so no adapter line; no SKIP line a first-time user
    # cannot act on; every line carries its status word.
    assert "adapter:" not in result.stdout
    assert "generated state" not in result.stdout
    for line in result.stdout.splitlines():
        assert line.split(" ", 1)[0] in {"OK", "FAIL", "WARN", "INFO", "SKIP"}, repr(line)
        assert not line.startswith("SKIP"), result.stdout
    # With no sign-in to read, nothing was asked: no request leaves the machine.
    assert not (bindir / "curl.args").exists()


@pytest.mark.parametrize("conf", [
    "type=apikey\napi_key=k\nenv.CCMORPH_MASK=1\n",
    "type=apikey\napi_key=k\nbase_url=https://api.anthropic.com\nenv.CCMORPH_MASK=1\n",
    "type=provider\nprovider=test\nenv.CCMORPH_MASK=1\n",
], ids=["no-base-url", "anthropic-base-url", "provider-bridge"])
def test_doctor_warns_when_masking_is_on_for_a_route_that_cannot_mask(tmp_path, conf):
    """Masking is a promise about what reaches the backend. On a route the shim
    does not front, the setting silently masks nothing (GH-293), so doctor must
    say so -- as a warning about the route, not a readiness failure: the profile
    still starts, and the verdict must not claim otherwise."""
    if "type=provider" in conf:
        # A provider profile is ready only when its bridge can start (#787):
        # a cliproxyapi on PATH and the profile's auth directory.
        bindir = tmp_path / "bin"
        bindir.mkdir(exist_ok=True)
        (bindir / "cliproxyapi").write_text("#!/bin/sh\nexit 0\n")
        (bindir / "cliproxyapi").chmod(0o755)
        (tmp_path / "state" / "auth" / "demo").mkdir(parents=True)
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 0, result.stdout
    warns = [l for l in result.stdout.splitlines() if l.startswith("WARN")]
    assert any("masking" in l for l in warns), result.stdout
    assert any("nothing is masked" in l for l in warns), result.stdout
    assert result.stdout.splitlines()[-1] == "OK ready: start it with ccmorph run demo"


def test_doctor_stays_quiet_when_masking_covers_the_route(tmp_path):
    """A gateway route is fronted by the shim, so masking on there is the
    promise kept (GH-293): no warning line, and the verdict stays ready."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nenv.CCMORPH_MASK=1\n"
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 0, result.stdout
    assert "masking" not in result.stdout
    assert result.stdout.splitlines()[-1] == "OK ready: start it with ccmorph run demo"


def test_doctor_fails_a_mask_value_the_shim_refuses_issue_411(tmp_path):
    """A mask value the shim refuses at start is not masking off: the launch
    stops in the shim (GH-411), so doctor must name the refused value and keep
    the verdict from ready instead of printing no masking row at all."""
    conf = ("type=apikey\nbase_url=https://example.test\n"
            "api_key=k\n"
            "env.CCMORPH_MASK=yes\n")
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 1, result.stdout
    assert "FAIL masking: env.CCMORPH_MASK=yes is refused" in result.stdout, result.stdout
    assert result.stdout.splitlines()[-1].startswith("FAIL not ready"), result.stdout


# GH-881: the Remote Control hosts kept a second, hand-kept copy of the route
# profile's tier map; when it drifted, phone sessions started on the old map —
# the gateway 400 Alex met on his phone — and nothing upstream of the phone
# said so. The doctor row is that machine signal: what the hosted edge
# published it pins (served) against what this profile itself maps (requested).
RC_GATEWAY_CONF = ("type=apikey\nbase_url=https://example.test\napi_key=k\n"
                   "env.ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4.1-flash\n"
                   "env.ANTHROPIC_DEFAULT_FABLE_MODEL=gpt-6-astra\n")


RC_HOSTS = "route=demo\n~/proj\npermission=bypassPermissions\n"


def rc_state(hosts=RC_HOSTS, tiers=None, route="demo", supervisor=None):
    """A setup callback for run_doctor: an rc-hosts config, the hosted record the
    supervisor publishes for it, and the edge record its launcher wrote pinning
    `tiers` (None: none yet).

    `hosted.json` is built by the add-on's own parse() and hosted_record(),
    loaded from addons/remote-control/ccmorph-rc-hosts -- never written by
    hand -- so a fixture cannot assert a hosting rule the supervisor does not
    have. The state dir is this fixture's own, exported for the load only.

    `supervisor` replaces the stamp hosted_record writes (#926). None keeps the
    publisher's own -- this test process, which is alive -- and a callable
    builds one, so a case can name a process demonstrably gone."""
    def setup(state):
        if hosts is not None:
            (state / "rc-hosts").write_text(hosts)
            with patch.dict(os.environ, {"CCMORPH_STATE_DIR": str(state)}):
                m = load_script(RC_HOSTS_SCRIPT, "rc_hosts")
            bridge = state / "profiles" / ".rc-bridge"
            bridge.mkdir(parents=True, exist_ok=True)
            record = m.hosted_record({r for r, _ in m.parse(hosts).values()}, hosts)
            if callable(supervisor):
                record["supervisor"] = supervisor()
            elif supervisor is not None:
                record["supervisor"] = supervisor
            (bridge / "hosted.json").write_text(json.dumps(record))
        if tiers is not None:
            edges = state / "profiles" / ".rc-bridge" / "edges"
            edges.mkdir(parents=True, exist_ok=True)
            (edges / "demo.json").write_text(
                json.dumps({"pid": 4242, "port": 8443, "route": route, "tiers": tiers}))
    return setup


def rc_start_mark(pid):
    """rc_route.proc_started, loaded by path: the stamp's own reader, so a fixture
    builds the same record on the tree before #926, where rc-hosts does not
    re-export it -- the regression must fail on what doctor prints, never on a
    fixture that could not build its case."""
    return load_script(RC_HOSTS_SCRIPT.parent / "rc_route.py", "rc_route").proc_started(pid)


def a_gone_supervisor():
    """A supervisor identity that is demonstrably gone (#926): the start mark of a
    real child read while it lived, then the child stopped. If the OS hands the
    number to a later process, that holder's mark is not this one; if it hands it
    to nobody, no process holds the pid at all -- either way the process the
    record names does not run (the #206 lesson: identity is pid + start)."""
    child = subprocess.Popen(["/bin/sleep", "10"])
    started = rc_start_mark(child.pid)
    child.terminate()
    child.wait()
    assert started, "the child's start mark must be readable while it lives"
    return {"pid": child.pid, "started": started}


def without_the_stamp(make):
    """A record from a supervisor older than #926: no supervisor key at all, so
    doctor keeps the behavior the record had before the stamp existed. pop, not
    delete: on the tree before the stamp the key is already absent."""
    def setup(state):
        make(state)
        path = state / "profiles" / ".rc-bridge" / "hosted.json"
        record = json.loads(path.read_text())
        record.pop("supervisor", None)
        path.write_text(json.dumps(record))
    return setup


STALE = {"fable": "gpt-6-astra", "haiku": "claude-haiku-5-5"}
STALE_HAIKU = ("WARN remote control: the hosted edge pins haiku=claude-haiku-5-5; "
               "this profile maps haiku=deepseek-v4.1-flash")


@pytest.mark.parametrize("hosts, tiers, named", [
    (RC_HOSTS, {"fable": "gpt-6-astra", "haiku": "claude-haiku-5-5"},
     "WARN remote control: the hosted edge pins haiku=claude-haiku-5-5; "
     "this profile maps haiku=deepseek-v4.1-flash"),
    (RC_HOSTS, {"fable": "gpt-6-astra"},
     "WARN remote control: the hosted edge pins no haiku model; "
     "this profile maps haiku=deepseek-v4.1-flash"),
    (RC_HOSTS, {"fable": "gpt-6-astra", "haiku": "deepseek-v4.1-flash", "sonnet": "gemini-3.5-pro"},
     "WARN remote control: the hosted edge pins sonnet=gemini-3.5-pro; "
     "this profile maps no sonnet model"),
    # The route assignment is the supervisor's: a `route= demo # …` line names
    # demo, though no token spells it (its config_lines cuts the comment first).
    ("route= demo # local route\n~/proj\npermission=bypassPermissions\n", STALE, STALE_HAIKU),
    # A folder's own `route=demo` before the ` -- ` separator hosts demo, exactly
    # where the same token after it is only a forwarded argument (see the other
    # test's forwarded-arg case).
    ("route=other\n~/proj route=demo -- --session-id session_01X\n", STALE, STALE_HAIKU),
    # The gate's round-3 repro: a no-break space ends the supervisor's `\S+`
    # word, so this folder is hosted on demo, whatever a token scan says.
    ("route=other\n~/proj route=demo  -- --session-id session_01X\n", STALE, STALE_HAIKU),
])
def test_doctor_names_the_tiers_a_hosted_edge_pins_wrong_gh_881(tmp_path, hosts, tiers, named):
    """A hosted edge whose pins drift from the profile is named alias by alias,
    with the one repair that repins; the profile itself still starts, so the
    row warns without weighing on the verdict (the masking rule of GH-293)."""
    result, _, _ = run_doctor(tmp_path, RC_GATEWAY_CONF, OK_CURL, setup=rc_state(hosts=hosts, tiers=tiers))
    assert result.returncode == 0, result.stdout
    assert named in result.stdout, result.stdout
    assert "launchctl kickstart -k gui/$(id -u)/com.ccmorph.rc-hosts" in result.stdout, result.stdout
    assert result.stdout.splitlines()[-1] == "OK ready: start it with ccmorph run demo"


@pytest.mark.parametrize("case", [
    "not-hosted",           # nothing hosts this profile: the row stays out of the report
    "no-config",            # the add-on is not configured at all
    "twin-record",          # #846: a record naming another route is not this edge
    "twin-record-no-stamp",  # #926: the same on a record older than the stamp -- main's case
    "matching-pins",        # overrides apply, [1m] spells one model: nothing to say
    "forwarded-arg",        # `route=demo` after a folder's ` -- ` is Claude's argument
    "inference-value",      # an unrelated settings line's value names no route either
    "fifo-config",          # a FIFO under rc-hosts: doctor reads no row and never waits
    "route-line-alone",     # `route=demo` with no folder line hosts nothing
    "last-route-wins",      # route=demo, a folder, route=other: the folder hosts on other
    "nothing-published",    # a stale edge record with no hosted.json speaks for nobody
    "fifo-published",       # a FIFO under hosted.json: refused, never waited on
    "damaged-published",    # hosted.json that is not JSON: no row
    "dead-supervisor",      # #926: the stamped writer is gone, so nothing claims hosted
    "dead-supervisor-no-edge",   # #926: nor does it claim "no edge record to compare"
    "dead-supervisor-other",     # #926: a dead record naming another route is not this profile's
    "recycled-supervisor",  # #926: the pid lives but its start mark is another process's
    "no-stamp",             # #926: a record from an older supervisor keeps today's row
])
def test_doctor_reports_a_hosted_edge_only_when_its_pins_are_the_question_gh_881(tmp_path, case):
    conf = RC_GATEWAY_CONF + "env.ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-5-5\n"
    hosts = "route=demo\n~/proj\n"
    tiers = {"fable": "gpt-6-astra", "haiku": "deepseek-v4.1-flash[1m]"}
    setup = rc_state(hosts=hosts, tiers=tiers)

    def with_files(record=None, fifo_record=False, rc_text="route=demo\n~/proj\n"):
        """hosts.json as the case asks, plus the files a live state dir holds."""
        def make(state):
            if rc_text is not None:
                (state / "rc-hosts").write_text(rc_text)
            bridge = state / "profiles" / ".rc-bridge"
            (bridge / "edges").mkdir(parents=True)
            (bridge / "edges" / "demo.json").write_text(
                json.dumps({"pid": 4242, "route": "demo", "tiers": tiers}))
            if fifo_record:
                os.mkfifo(bridge / "hosted.json")
            elif record is not None:
                (bridge / "hosted.json").write_text(record)
        return make

    if case == "not-hosted":
        setup = rc_state(hosts="route=other\n~/proj\n", tiers=tiers)
    elif case == "no-config":
        setup = rc_state(hosts=None, tiers=None)
    elif case == "twin-record":
        setup = rc_state(hosts=hosts, tiers=tiers, route="other")
    elif case == "twin-record-no-stamp":
        # Main's #881 case exactly: a record that names another route's edge and
        # carries no stamp. #926 keeps the behavior of a record written before
        # the stamp, so this legacy row must still stand -- a change that
        # suppressed it only for unstamped records would otherwise pass unseen.
        setup = without_the_stamp(rc_state(hosts=hosts, tiers=tiers, route="other"))
    elif case == "matching-pins":
        tiers = {"fable": "gpt-6-astra", "haiku": "deepseek-v4.1-flash", "opus": "gpt-6-astra"}
        setup = rc_state(hosts="route=demo\n~/proj\ntier.opus=gpt-6-astra\n", tiers=tiers)
    elif case == "forwarded-arg":
        # The supervisor hosts this folder on `other` and forwards route=demo to
        # Claude as written: demo is named nowhere the config uses as a route.
        setup = rc_state(hosts="route=other\n~/proj -- route=demo --session-id session_01X\n",
                         tiers=tiers)
    elif case == "inference-value":
        setup = rc_state(hosts="route=other\n~/proj\ninference=route=demo\n", tiers=tiers)
    elif case == "fifo-config":
        def fifo(state):
            os.mkfifo(state / "rc-hosts")
            (state / "profiles" / ".rc-bridge" / "edges").mkdir(parents=True)
            (state / "profiles" / ".rc-bridge" / "edges" / "demo.json").write_text(
                json.dumps({"pid": 4242, "route": "demo", "tiers": tiers}))
        setup = fifo
    elif case == "route-line-alone":
        setup = rc_state(hosts="route=demo\npermission=bypassPermissions\n", tiers=tiers)
    elif case == "last-route-wins":
        setup = rc_state(hosts="route=demo\n~/proj\nroute=other\n", tiers=tiers)
    elif case == "nothing-published":
        setup = with_files()                 # rc-hosts and the edge record, no hosted.json
    elif case == "fifo-published":
        setup = with_files(fifo_record=True)
    elif case == "damaged-published":
        setup = with_files(record="{not json")
    elif case == "dead-supervisor":
        setup = rc_state(hosts=hosts, tiers=tiers, supervisor=a_gone_supervisor)
    elif case == "dead-supervisor-no-edge":
        setup = rc_state(hosts=hosts, tiers=None, supervisor=a_gone_supervisor)
    elif case == "dead-supervisor-other":
        setup = rc_state(hosts="route=other\n~/proj\n", tiers=tiers,
                         supervisor=a_gone_supervisor)
    elif case == "recycled-supervisor":
        # The number answers, but the process holding it is not the supervisor
        # the record names: its start mark is another process's (#206).
        setup = rc_state(hosts=hosts, tiers=tiers,
                         supervisor={"pid": os.getpid(), "started": "stale:0"})
    elif case == "no-stamp":
        setup = without_the_stamp(rc_state(hosts=hosts, tiers=tiers))
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL, setup=setup)
    assert result.returncode == 0, result.stdout
    rows = [line for line in result.stdout.splitlines() if "remote control:" in line]
    if case == "matching-pins":
        # The publisher's own stamp is a live process (#926): the row stays.
        assert rows == ["OK remote control: hosted; the edge pins the tiers this profile maps"], rows
    elif case == "no-stamp":
        # A record from before the stamp existed keeps exactly today's row.
        assert rows == ["OK remote control: hosted; the edge pins the tiers this profile maps"], rows
    elif case == "dead-supervisor-other":
        assert rows == [], result.stdout   # not this profile's record: no row at all
    elif case in ("dead-supervisor", "dead-supervisor-no-edge", "recycled-supervisor"):
        # A record whose supervisor is demonstrably gone must not claim hosted:
        # one INFO names the stale writer and the repair, and no row vouches for
        # the route (the tiers are never compared against a dead supervisor's map).
        assert len(rows) == 1, rows
        assert rows[0].startswith("INFO remote control: not hosted; the record was left by supervisor "), rows
        assert "launchctl kickstart -k gui/$(id -u)/com.ccmorph.rc-hosts" in rows[0], rows
        assert "remote control: hosted" not in result.stdout, result.stdout
    elif case in ("twin-record", "twin-record-no-stamp"):
        assert rows == ["INFO remote control: hosted; no edge record to compare "
                        "(a running edge publishes one)"], rows
    else:
        assert rows == [], result.stdout


@pytest.mark.parametrize("case", [
    "miss",       # the live edge recorded misses: a WARN names them and keeps its last line
    "clean",      # turns served as picked: an INFO says so
    "stale",      # the record is an earlier proxy's at this pid: it answers nothing
    "no-held",    # no held record to say which proxy the served record is this run's
    "absent",     # no record yet: a fresh edge has no history to report
    "fifo",       # a FIFO under served.4242.json: refused, never waited on (#654 rule)
])
def test_doctor_answers_whether_the_edge_served_what_sessions_picked_gh_977(tmp_path, case):
    """GH-977: the per-turn asked-vs-served verdict (#920) reached only the --debug
    log, so a pick set in Stark and served elsewhere was audited and forgotten and
    Alex found the next miss before any check did. The proxy now keeps the outcome
    in the state dir and doctor answers the outcome on the hosted row: misses warn
    with the last miss line, a clean run says so, and nothing is claimed for a
    record an earlier proxy wrote (identity is pid AND start, the #595 rule --
    served and held must name the same edge run)."""
    base = rc_state(hosts=RC_HOSTS, tiers={"fable": "gpt-6-astra", "haiku": "deepseek-v4.1-flash"})

    def outcome(state, turns, misses, last=None, started="edge-s1"):
        bridge = state / "profiles" / ".rc-bridge"
        (bridge / "held.4242.json").write_text(json.dumps(
            {"edge": 4242, "edge_started": started, "profiles": {}}))
        (bridge / "served.4242.json").write_text(json.dumps(
            {"edge": 4242, "edge_started": started, "turns": turns, "misses": misses,
             "last_miss": {"at": 1, "line": last} if last else None}))

    def make(state):
        base(state)
        if case == "miss":
            outcome(state, 9, 2, last="asked=kimi via=route served-by=127.0.0.1:60111")
        elif case == "clean":
            outcome(state, 5, 0)
        elif case == "stale":
            outcome(state, 9, 2, last="asked=kimi served-by=x", started="edge-s0")
            (state / "profiles" / ".rc-bridge" / "held.4242.json").write_text(json.dumps(
                {"edge": 4242, "edge_started": "edge-s1", "profiles": {}}))
        elif case == "no-held":
            outcome(state, 9, 2, last="asked=kimi served-by=x")
            (state / "profiles" / ".rc-bridge" / "held.4242.json").unlink()
        elif case == "fifo":
            outcome(state, 5, 0)
            p = state / "profiles" / ".rc-bridge" / "served.4242.json"
            p.unlink()
            os.mkfifo(p)

    result, _, _ = run_doctor(tmp_path, RC_GATEWAY_CONF, OK_CURL, setup=make)
    assert result.returncode == 0, result.stdout
    rows = [line for line in result.stdout.splitlines() if "session turn" in line]
    if case == "miss":
        assert rows == ["WARN remote control: 2 of 9 session turns since this edge started "
                        "were served by another endpoint than the pick chose; last: asked=kimi "
                        "via=route served-by=127.0.0.1:60111"], rows
        # the profile itself still runs: the outcome warns without weighing on the verdict
        assert result.stdout.splitlines()[-1] == "OK ready: start it with ccmorph run demo", result.stdout
    elif case == "clean":
        assert rows == ["INFO remote control: all 5 session turns since this edge started "
                        "were served by the endpoint their pick chose"], rows
    else:
        assert rows == [], result.stdout


@pytest.mark.parametrize("case", ["gateway-ok", "missing-credential", "malformed-listing", "nothing-to-pick-gh-862"])
def test_doctor_ends_with_a_verdict(tmp_path, case):
    """The last line answers the only question a person has: is this profile
    ready to run, or what must be fixed first."""
    if case == "gateway-ok":
        result, _, _ = run_doctor(
            tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", OK_CURL)
    elif case == "missing-credential":
        result, _, _ = run_doctor(tmp_path, "type=apikey\nbase_url=https://example.test\n")
    elif case == "nothing-to-pick-gh-862":
        # GH-862: a gateway that lists no chat model leaves a profile with no
        # saved model nothing to start; the verdict names run's repair.
        result, _, _ = run_doctor(
            tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n",
            OK_CURL.replace('{"data":[{"id":"m-1"}]}', '{"data":[]}'))
    else:
        malformed = """#!/bin/sh
printf '%s\n' '{"data":"not-a-list"}'
printf '%s\n' '__CCMORPH_STATUS__200'
"""
        result, _, _ = run_doctor(
            tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", malformed)
    last = result.stdout.splitlines()[-1]
    if case == "gateway-ok":
        assert result.returncode == 0, result.stdout
        assert last == "OK ready: start it with ccmorph run demo", result.stdout
    elif case == "missing-credential":
        assert result.returncode == 1, result.stdout
        assert last == "FAIL not ready: set the API key: ccmorph config demo", result.stdout
    elif case == "nothing-to-pick-gh-862":
        assert result.returncode == 1, result.stdout
        assert ("FAIL model: the 'demo' gateway's model list names no chat model to pick; "
                "save a model id this gateway serves: ccmorph set demo model <model-id>") in result.stdout.splitlines(), result.stdout
        assert last == "FAIL not ready: save a model id this gateway serves: ccmorph set demo model <model-id>", result.stdout
    else:
        assert result.returncode == 1, result.stdout
        assert last == ("FAIL not ready: check the endpoint (it must list models at /v1/models): "
                        "ccmorph config demo"), result.stdout


GW = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
NATIVE = "type=apikey\napi_key=k\n"
REFUSED_KEY = "sk-issue-282-doctor-key"
SIGNED_OUT = ('#!/bin/sh\n[ "$1 $2" = "auth status" ] && '
              'printf \'{"loggedIn": false, "authMethod": "none"}\\n\' && exit 1\nexit 0\n')


@pytest.mark.parametrize("case,conf,curl,expected", [
    ("no-credential", "type=apikey\nbase_url=https://example.test\n", None,
     "set the API key: ccmorph config demo"),
    ("bad-endpoint", "type=apikey\nbase_url=https://example.test/v1?x=1\napi_key=k\n", OK_CURL,
     "set an http(s) endpoint: ccmorph config demo"),
    ("gateway-rejects-key", GW, status_curl(401), "replace the API key: ccmorph config demo"),
    ("gateway-down", GW, status_curl(500), "check the endpoint, or change it: ccmorph config demo"),
    ("malformed-listing", GW, MALFORMED_CURL,
     "check the endpoint (it must list models at /v1/models): ccmorph config demo"),
    ("model-not-served", GW + "model=glm-4.6-air\n", GW_LISTING,
     "choose a served model: ccmorph set demo model <model-id>"),
    ("native-key-rejected", NATIVE, status_curl(401), "replace the API key: ccmorph config demo"),
    ("token-rejected", "type=token\ntoken=sk-ant-oat01-issue-282\n", status_curl(401),
     "replace the token: ccmorph config demo"),
    ("signed-out", "type=sub\nemail=demo@example.test\n", OK_CURL, "sign in again: ccmorph login demo"),
    # #649: a sign-in interrupted before its email line is half-made, and the
    # repair named here must be the one `add`, `run` and `login` already name.
    ("half-made-sub", "type=sub\n", None, "sign in again: ccmorph login demo"),
    ("key-refused-by-claude", f"type=apikey\napi_key={REFUSED_KEY}\n", OK_CURL,
     "let Claude Code use the key again: ccmorph login demo"),
    ("settings-invalid", GW, OK_CURL, "fix the JSON in {settings}"),
    ("schema-invalid", "type=token\nbase_url=https://example.test\n", None,
     "edit {conf} to fix it, or re-create the profile: ccmorph rm demo && ccmorph add demo"),
    ("missing-type", "api_key=k\n", None,
     "edit {conf} to fix it, or re-create the profile: ccmorph rm demo && ccmorph add demo"),
    ("claude-missing", NATIVE, OK_CURL, CLAUDE_MISSING),
])
def test_doctor_verdict_names_the_fix_for_every_failed_check_issue_282(tmp_path, case, conf, curl, expected):
    """#282: the last line names the command or step that repairs the first
    failed check, whatever the check, never a pointer to the rows above."""
    home = tmp_path / "state" / "profiles" / "demo"
    if case == "key-refused-by-claude":
        home.mkdir(parents=True)
        (home / ".claude.json").write_text(json.dumps(
            {"customApiKeyResponses": {"approved": [], "rejected": [REFUSED_KEY[-20:]]}}))
    if case == "settings-invalid":
        home.mkdir(parents=True)
        (home / "settings.json").write_text("{ invalid")
    claude = {"signed-out": SIGNED_OUT, "claude-missing": False}.get(case)
    result, _, _ = run_doctor(tmp_path, conf, curl, claude=claude)
    last = result.stdout.splitlines()[-1]
    assert result.returncode == 1, result.stdout
    assert last == "FAIL not ready: " + expected.format(
        settings=home / "settings.json", conf=tmp_path / "state" / "demo.conf"), result.stdout
    assert "(see above)" not in last, result.stdout


@pytest.mark.parametrize("case,rc,row,last", [
    ("absent", 0,
     "INFO profile home: not created yet ({home}); the first launch makes it: ccmorph run demo",
     "OK ready: start it with ccmorph run demo"),
    ("home-occupied", 1,
     "FAIL profile home: cannot be created — {home} is not a directory",
     "FAIL not ready: remove {home} so the first launch can create the profile home: ccmorph run demo"),
    ("profiles-occupied", 1,
     "FAIL profile home: cannot be created — {profiles} is not a directory",
     "FAIL not ready: remove {profiles} so the first launch can create the profile home: ccmorph run demo"),
    ("unreadable", 1,
     "FAIL profile home: cannot be created — lstat {home}",
     "FAIL not ready: check the permissions on {home}"),
])
def test_doctor_tells_absence_from_a_blocked_home_issue_291(tmp_path, case, rc, row, last):
    """#291: a home that is simply not created yet is a note — the first launch
    makes it, so a profile copied in and never launched is ready — while a home
    that launch cannot make is a failed check naming the blocker. Lstat under a
    file answers ENOTDIR, which reads as absence to a bare exists-check, so the
    blocked shapes include a file where the state keeps its profiles."""
    state = tmp_path / "state"
    state.mkdir()
    profiles = state / "profiles"
    home = profiles / "demo"
    if case == "home-occupied":
        profiles.mkdir()
        home.write_text("not a directory")
    elif case == "profiles-occupied":
        profiles.write_text("not a directory")
    elif case == "unreadable":
        profiles.mkdir()
        profiles.chmod(0o000)
    result, _, _ = run_doctor(tmp_path, NATIVE, OK_CURL, home=False)
    if case == "unreadable":
        profiles.chmod(0o700)  # let pytest's tmp cleanup back in
    assert result.returncode == rc, result.stdout
    assert row.format(home=home, profiles=profiles) in result.stdout, result.stdout
    assert result.stdout.splitlines()[-1] == last.format(home=home, profiles=profiles), result.stdout
    assert ("FAIL" in result.stdout) == (rc == 1), result.stdout


@pytest.mark.parametrize("case", ["gateway", "native", "sub"])
def test_doctor_healthy_profile_has_no_skip_lines(tmp_path, case):
    """A working profile must not print a SKIP line a first-time user cannot act
    on, and every line must carry its status word."""
    if case == "gateway":
        conf, curl = "type=apikey\nbase_url=https://example.test\napi_key=k\n", OK_CURL
    elif case == "native":
        conf, curl = "type=apikey\napi_key=k\n", OK_CURL
    else:
        conf, curl = "type=sub\nemail=test@example.test\n", OK_CURL
        home = tmp_path / "state" / "profiles" / "demo"
        home.mkdir(parents=True)
        (home / ".credentials.json").write_text(
            json.dumps({"claudeAiOauth": {"accessToken": SUB_SIGNIN}}))
    result, _, _ = run_doctor(tmp_path, conf, curl)
    assert result.returncode == 0, result.stdout
    assert "generated state" not in result.stdout
    assert len([l for l in result.stdout.splitlines() if "adapter:" in l]) <= 1
    for line in result.stdout.splitlines():
        assert line.split(" ", 1)[0] in {"OK", "FAIL", "WARN", "INFO", "SKIP"}, repr(line)
        assert not line.startswith("SKIP"), result.stdout


@needs_loopback  # the provider stub binds a port: #611
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
    # /v1/models with 200 naming a model; stub it so the test does not depend on the
    # host's install.
    (bindir / "cliproxyapi").write_text(
        "#!/usr/bin/env python3\n"
        "from http.server import BaseHTTPRequestHandler, HTTPServer\n" + FAKE_SERVER_NO_DNS +
        "class H(BaseHTTPRequestHandler):\n"
        "    def do_GET(self):\n"
        "        body = b'{\"data\":[{\"id\":\"m\"}]}'\n"
        "        self.send_response(200); self.send_header('Content-Length', str(len(body)))\n"
        "        self.end_headers(); self.wfile.write(body)\n"
        "    def log_message(self, *a): pass\n"
        "import re, sys\n"
        "port = int(re.search(r'port: (\\d+)', open(sys.argv[2]).read()).group(1))\n"
        "HTTPServer(('127.0.0.1', port), H).serve_forever()\n")
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


@needs_loopback  # the passthrough act it guards needs a bound port: #611
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

            def refusing():
                try:
                    os.killpg(process.pid, 0)
                except PermissionError:
                    return True
                return False
            wait_until(refusing, min(timeout, 30), interval=0.02)
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
    # The demo fixture keeps no stored sign-in, so its verdict says may not
    # start, and since GH-970 that verdict is the exit status: 1 guards the
    # exit, `last` guards the warning verdict itself (a terminal-only hard
    # `FAIL not ready` exits the same 1 and must not pass), and every other
    # assertion guards the formatting.
    for overrides, profile, expected, code, last in [
        ({}, "demo", "\x1b[38;2;215;119;87m✓\x1b[0m", 1, "may not start"),
        ({}, "missing", "\x1b[38;2;255;107;128m✗\x1b[0m", 1, ""),
        ({"NO_COLOR": "1"}, "demo", "✓ profile:", 1, "may not start"),
        ({"TERM": "dumb"}, "demo", "✓ profile:", 1, "may not start"),
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
            if last:
                # The verdict is the report's last line: it must be the
                # warning the exit code now stands for, not a hard failure.
                assert last in text.rstrip().splitlines()[-1], repr(text)
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


def run_bare_doctor(tmp_path, confs=(), claude=True, python=None, rc_hosts=False, rc=None):
    """`ccmorph doctor` with no profile argument, on a PATH holding only what it names."""
    state = tmp_path / "state"
    bindir = tmp_path / "bin"
    state.mkdir()
    bindir.mkdir()
    (tmp_path / "tmp").mkdir()
    if rc is not None:
        (tmp_path / "home").mkdir()
        (tmp_path / "home" / ".zshrc").write_text(rc)
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

def test_bare_doctor_names_both_completion_lines_gh363(tmp_path):
    # Issue #363: a fresh home runs neither the fpath line nor compinit, so the
    # no-profile doctor must print both halves of the completion hint.
    result, _ = run_bare_doctor(tmp_path)
    assert "INFO   fpath=(" in result.stdout, result.stdout
    assert "INFO   autoload -Uz compinit && compinit # add both lines to ~/.zshrc" in result.stdout, result.stdout


@pytest.mark.parametrize("rc", [
    # #582: a # at a word start (after ;) begins a zsh comment, so the compinit
    # behind it starts nothing. Doctor must say the line is missing, not read
    # the comment as the rc already running it.
    "fpath=($HOME/.config/ccmorph/share/zsh/site-functions $fpath)\n"
    "setopt correct;# autoload -Uz compinit && compinit\n",
    # #582: \r is an ordinary word character to zsh, so a CRLF rc's compinit\r
    # is a command zsh fails to find; the rc has not started completions, and
    # doctor must still name the compinit line the user has to add.
    "fpath=($HOME/.config/ccmorph/share/zsh/site-functions $fpath)\r\n"
    "autoload -Uz compinit && compinit\r\n",
    # #582 review: zsh reads the whole file, so a string opened on one line is
    # still open on the next; once it closes, ;# begins a comment there too.
    "fpath=($HOME/.config/ccmorph/share/zsh/site-functions $fpath)\n"
    ": 'a\nb';# autoload -Uz compinit && compinit\n",
])
def test_bare_doctor_completion_row_follows_zsh_on_word_starts_and_crs_gh582(tmp_path, rc):
    result, _ = run_bare_doctor(tmp_path, rc=rc)
    assert "INFO completion: nothing in ~/.zshrc runs compinit — add:" in result.stdout, result.stdout
    assert "INFO   autoload -Uz compinit && compinit" in result.stdout, result.stdout
    assert "put the fpath line above compinit" not in result.stdout, result.stdout


def test_doctor_on_a_fresh_install_fails_without_claude(tmp_path):
    result, _ = run_bare_doctor(tmp_path, claude=False)
    assert result.returncode == 1, result.stdout + result.stderr
    assert f"FAIL claude executable: {CLAUDE_MISSING}" in result.stdout
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


def zsh_fpath_fails_compaudit(shell):
    # GitHub's Ubuntu image runs `chmod -R 777 /usr/share`, so compaudit rejects
    # zsh's own fpath there and a plain compinit aborts without a terminal.
    env = {k: v for k, v in os.environ.items() if k not in ("FPATH", "ZDOTDIR")}
    probe = subprocess.run([shell, "-fc", "autoload -Uz compaudit && compaudit"],
                           env=env, stdin=subprocess.DEVNULL, capture_output=True,
                           text=True, timeout=HANG_TIMEOUT)
    return probe.returncode != 0


def test_doctor_completion_row_lasts_until_a_new_zsh_completes(tmp_path):
    # Issue #363: the row used to vanish once the .zshrc carried the fpath
    # line, though zsh runs no completion system by itself — Tab completed
    # nothing. The row must stay until a new zsh completes the ccmorph
    # command, and say only the half the startup files still miss.
    conf = "type=apikey\napi_key=k\n"
    home = tmp_path / "home"
    home.mkdir()
    # A copy no group can write: compinit refuses a group-writable fpath
    # directory (a checkout made under umask 002, as on CI, is one) and would
    # leave a new zsh with no completion, which is not the case under test.
    install = tmp_path / "install"
    install.mkdir(mode=0o755)
    (install / "lib").symlink_to(ROOT / "lib")
    (install / "libexec").symlink_to(ROOT / "libexec")
    site = install / "share/zsh/site-functions"
    shutil.copytree(ROOT / "share/zsh/site-functions", site)
    for d in (install / "share", install / "share/zsh", site):
        d.chmod(0o755)
    for f in site.iterdir():
        f.chmod(0o644)
    shell = shutil.which("zsh") or "/bin/zsh"
    (home / ".zshrc").write_text(f"fpath=({site} $fpath)\n")
    result, _, _ = run_doctor(tmp_path, conf, claude=False,
                              env_extra={"SHELL": shell, "ZDOTDIR": str(home),
                                         "CCMORPH_LIB_DIR": str(install / "lib")})
    assert "INFO completion:" in result.stdout
    assert "autoload -Uz compinit && compinit" in result.stdout
    assert f"fpath=({site} $fpath)" not in result.stdout
    # The fpath line followed by the missing half: a new zsh now completes
    # ccmorph, so doctor stops reporting a completion problem.
    compinit = "autoload -Uz compinit && compinit"
    if zsh_fpath_fails_compaudit(shell):
        compinit += " -u"  # the host's own fpath, not the hint, fails there
    (home / ".zshrc").write_text(f"fpath=({site} $fpath)\n{compinit}\n")
    result, _, _ = run_doctor(tmp_path, conf, claude=False,
                              env_extra={"SHELL": shell, "ZDOTDIR": str(home),
                                         "CCMORPH_LIB_DIR": str(install / "lib")})
    assert "INFO completion:" not in result.stdout


@pytest.mark.parametrize("case", ["bridge-missing", "auth-missing", "ready"])
def test_doctor_asks_whether_a_provider_bridge_can_start_issue_787(tmp_path, case):
    """GH-787: a provider profile whose launch refuses to start its bridge --
    no cliproxyapi on PATH, or no auth directory -- ended `OK ready`, and the
    run then exited 1 with the prerequisite error. Doctor asks the launch's own
    refusal, prints it as a failing row and names it in the verdict."""
    bindir = tmp_path / "bin"
    bindir.mkdir()
    if case != "bridge-missing":
        (bindir / "cliproxyapi").write_text("#!/bin/sh\nexit 0\n")
        (bindir / "cliproxyapi").chmod(0o755)
    if case != "auth-missing":
        (tmp_path / "state" / "auth" / "demo").mkdir(parents=True)
    # The stubs' directory first, and no host cliproxyapi or brew behind it:
    # neither the bridge verdict nor the install step is the machine's own.
    path = os.pathsep.join([str(bindir), path_without("cliproxyapi", "brew")])
    result, _, _ = run_doctor(tmp_path, "type=provider\nprovider=antigravity\n",
                              env_extra={"PATH": path})
    last = result.stdout.splitlines()[-1]
    if case == "ready":
        assert result.returncode == 0, result.stdout
        assert ("OK provider bridge: cliproxyapi installed, auth directory present"
                in result.stdout), result.stdout
        assert last == "OK ready: start it with ccmorph run demo", result.stdout
        return
    why = ("cliproxyapi not found — install it: https://github.com/router-for-me/CLIProxyAPI"
           if case == "bridge-missing" else
           "no auth directory for 'demo' — run: ccmorph login demo")
    assert result.returncode == 1, result.stdout
    assert "FAIL provider bridge: " + why in result.stdout.splitlines(), result.stdout
    assert last == "FAIL not ready: " + why, result.stdout
    assert "OK ready" not in result.stdout, result.stdout


def test_doctor_ignores_the_operators_zsh_startup_files(tmp_path, monkeypatch):
    """GH-787's gate finding: bin/ccmorph is a zsh script, so zsh runs
    $ZDOTDIR/.zshenv before the CLI does. An operator's startup file that puts
    brew back on PATH changed the install step the report named, so a test that
    hid the tool from PATH passed on the gate and failed on that machine. The
    child must see no startup file but the test's own."""
    hostile = tmp_path / "zdot"
    (hostile / "bin").mkdir(parents=True)
    brew = hostile / "bin" / "brew"
    brew.write_text("#!/bin/sh\nexit 0\n")
    brew.chmod(0o755)
    (hostile / ".zshenv").write_text(f'export PATH="{hostile}/bin:$PATH"\n')
    monkeypatch.setenv("ZDOTDIR", str(hostile))
    # the claude stub's directory first: a host with no claude of its own fails before the bridge row
    path = os.pathsep.join([str(tmp_path / "bin"), path_without("cliproxyapi", "brew")])
    result, _, _ = run_doctor(tmp_path, "type=provider\nprovider=antigravity\n",
                              env_extra={"PATH": path})
    assert result.returncode == 1, result.stdout
    assert ("FAIL not ready: cliproxyapi not found — install it: "
            "https://github.com/router-for-me/CLIProxyAPI") in result.stdout, result.stdout
    assert "brew install cliproxyapi" not in result.stdout, result.stdout
