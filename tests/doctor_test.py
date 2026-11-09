import hashlib
import json
import os
import re
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


def run_doctor(tmp_path, conf, curl=None, args=(), claude=None):
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
    if claude is None:
        # A healthy account profile is signed in, and doctor now asks claude
        # for that state the way add does — so the default stub must answer.
        claude = ('#!/bin/sh\n'
                  '[ "$1 $2" = "auth status" ] && '
                  'printf \'{"loggedIn": true, "authMethod": "claude.ai", '
                  '"email": "test@example.test"}\\n\'\n'
                  'exit 0\n')
    (bindir / "claude").write_text(claude)
    (bindir / "claude").chmod(0o755)
    if curl is not None:
        c = bindir / "curl"
        c.write_text(curl)
        c.chmod(0o755)
    env = {**os.environ, "HOME": str(home), "PATH": f"{bindir}:{os.environ['PATH']}",
           "CCMORPH_STATE_DIR": str(state), "CCMORPH_LIB_DIR": str(ROOT / "lib"),
           "CURL_ARGS": str(bindir / "curl.args"), "CURL_CALLED": str(bindir / "called"),
           "DOCTOR_MARKER": str(bindir / "doctor.marker")}
    result = subprocess.run([str(ROOT / "bin/ccmorph"), "doctor", *args, "demo"], env=env,
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
printf '%s\n' '{"data":[]}'
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
    listing = OK_CURL.replace('{"data":[]}', '{"data":[%s]}' % served)

    def effective(conf_text, profile_json):
        profile_settings.write_text(profile_json)
        (tmp_path / "state" / "demo.conf").parent.mkdir(exist_ok=True)
        (tmp_path / "state" / "demo.conf").write_text(conf_text)
        result, path, _ = run_doctor(tmp_path, conf_text, listing)
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
    assert "not ready: fix model" in result.stdout
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
    assert "not ready: fix model" in result.stdout


def test_doctor_stays_ready_when_the_saved_model_is_served(tmp_path):
    # The refusal must not fire on a served default: an empty or unreadable
    # listing declines to adjudicate (covered elsewhere); a listing that names
    # the model is a clean bill of health.
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nmodel=glm-4.6\n"
    result, _, _ = run_doctor(tmp_path, conf, GW_LISTING)
    assert result.returncode == 0, result.stdout
    assert "ready: start it with ccmorph run demo" in result.stdout
    assert "is not served" not in result.stdout


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
# own CCMORPH_SHIM_HOOKS build still gets a spawned hooks build. The /bin/echo
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
        "'4323 4321 /usr/local/bin/hooks-shim 4568 http://127.0.0.1:4567'"),
}

# What doctor owes for each shape: every listener holder running-adapter names —
# a spawned child, or the adapter process itself when the shim is in-process —
# gets its port checked, exactly as a child's was.
ADAPTER_LISTENERS = {
    "translator-route": (("cliproxyapi child", "4322"), ("in-process shim", "4321")),
    "native-route": (("in-process shim", "4321"),),
    "hooks-build": (("cliproxyapi child", "4322"), ("hooks-shim child", "4323")),
}

# lsof's answer per pid: the shim (in-process or spawned) serves the session on
# 4568, the translator on 4567.
ADAPTER_LSOF_ROWS = (
    ("4321", "ccmorph-core 4321 u 3u IPv4 0x 0t0 TCP 127.0.0.1:4568 (LISTEN)"),
    ("4322", "cliproxyapi 4322 u 3u IPv4 0x 0t0 TCP 127.0.0.1:4567 (LISTEN)"),
    ("4323", "hooks-shim 4323 u 3u IPv4 0x 0t0 TCP 127.0.0.1:4568 (LISTEN)"),
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
    if "could not tell whether a translator is running" in out:
        # ps blew its bound: one SKIP line is the whole answer — nothing
        # half-reported, nothing started.
        assert "listener" not in out, out
        return
    for subject, pid in ADAPTER_LISTENERS[shape]:
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
UNTIL_A_MODEL = re.compile(
    r"INFO adapter: this profile needs no translator until a model is chosen "
    r"\(none is set, and the endpoint's model list names no protocol\); "
    r"cliproxyapi is (not )?installed; choose one: ccmorph set demo model <model-id>")


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
    # One adapter line by default, in every case (also when ps blew its bound):
    # whether this profile needs a translator now, never a pid or a
    # process-inspection detail a first-time reader cannot act on. Nothing is
    # classified and no model is set, so the answer is what the open session
    # shows; with no session readable, that none starts until a model is chosen.
    # A running adapter alone is not a translator: only a cliproxyapi child is.
    adapters = [l for l in result.stdout.splitlines() if "adapter:" in l]
    assert len(adapters) == 1, result.stdout
    if slow == "ps":
        assert UNTIL_A_MODEL.fullmatch(adapters[0]), result.stdout
    elif shape == "native-route":
        assert adapters == [NO_TRANSLATOR_NOW], result.stdout
    else:
        assert adapters == [TRANSLATOR_NOW], result.stdout
    # Read-only: the profile's own hook never ran, so nothing was started.
    assert not (bindir / "doctor.marker").exists()
    verbose, _, _ = run_doctor(tmp_path, conf, curl, args=("--verbose",))
    assert verbose.returncode == 0, verbose.stderr
    assert_adapter_report(verbose.stdout, shape)
    assert not (bindir / "doctor.marker").exists()


@pytest.mark.parametrize("shape,suffix", [
    ("translator-route", "; an open session on this profile is using one now"),
    ("native-route", ""),
], ids=["translator-running", "shim-only"])
def test_doctor_adapter_line_names_a_running_translator_issue_272(tmp_path, shape, suffix):
    """GH-272: with a classification cached, the line states the need, and an
    open session is added only when a cliproxyapi child is really running."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\n"
    _adapter_stubs(tmp_path / "bin", "", ADAPTER_PS_ROWS[shape])
    _seed_adapter_cache(tmp_path, "https://example.test", [("m-openai", "responses")])
    stub = tmp_path / "bin" / "cliproxyapi"
    stub.write_text("#!/bin/sh\nexit 0\n")
    stub.chmod(0o755)
    result, _, _ = run_doctor(tmp_path, conf, OK_CURL)
    assert result.returncode == 0, result.stdout + result.stderr
    adapters = [l for l in result.stdout.splitlines() if "adapter:" in l]
    assert adapters == ["OK adapter: this endpoint needs a translator; cliproxyapi is installed" + suffix], result.stdout


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
    profile = tmp_path / "state" / "profiles" / "demo"
    profile.mkdir(parents=True)
    (profile / "settings.json").write_text('{"model":"glm-4.6"}')
    # ps over its one-second bound: the whole running-adapter answer is one
    # SKIP line in plain words (GH-467's nothing-half-reported, reworded).
    _adapter_stubs(tmp_path / "bin", "ps", ADAPTER_PS_ROWS["translator-route"])
    result, _, _ = run_doctor(tmp_path, conf, GW_LISTING, args=("--verbose",))
    assert result.returncode == 0, result.stdout + result.stderr
    for banned in ("layered JSON", "canonical settings", "process inspection"):
        assert banned not in result.stdout, (banned, result.stdout)
    assert "profile settings: valid JSON (" in result.stdout, result.stdout
    assert "SKIP adapter: could not tell whether a translator is running" in result.stdout
    # A broken settings file is told how to be dealt with, not just that it is broken.
    (profile / "settings.json").write_text("{ invalid")
    broken, _, _ = run_doctor(tmp_path, conf, GW_LISTING)
    assert "invalid JSON" in broken.stdout
    assert f"({profile / 'settings.json'}); fix that file" in broken.stdout, broken.stdout


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
        assert UNTIL_A_MODEL.fullmatch(adapters[0]), result.stdout
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


@pytest.mark.parametrize("rows,expected", [
    ("'1 0 /sbin/launchd'", None),
    (ADAPTER_PS_ROWS["translator-route"], TRANSLATOR_NOW),
], ids=["no-session-line-omitted", "session-with-translator"])
def test_doctor_omits_the_adapter_line_for_an_unclassified_saved_model_issue_272(tmp_path, rows, expected):
    """GH-272: a saved model nothing has classified can only be answered by a
    billed check, which doctor never sends: the line is left out rather than
    guessed. An open session that shows a translator still says so."""
    conf = "type=apikey\nbase_url=https://example.test\napi_key=k\nmodel=glm-4.6\n"
    _adapter_stubs(tmp_path / "bin", "", rows)
    result, _, _ = run_doctor(tmp_path, conf, GW_LISTING)
    assert result.returncode == 0, result.stdout + result.stderr
    adapters = [l for l in result.stdout.splitlines() if "adapter:" in l]
    if expected is None:
        assert adapters == [], result.stdout
        assert "ready: start it with ccmorph run demo" in result.stdout, result.stdout
    else:
        assert adapters == [expected], result.stdout


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
    assert "not ready: fix credentials" in result.stdout
    assert "kept in the profile home" not in result.stdout


def test_doctor_admits_when_it_cannot_read_the_sign_in_state(tmp_path):
    """When claude answers nothing doctor must not guess: it says plainly it
    cannot tell, warns in the verdict, and does not fail a profile it could
    not read."""
    claude = "#!/bin/sh\nexit 0\n"
    result, _, _ = run_doctor(tmp_path, "type=sub\nemail=test@example.test\n", claude=claude)
    assert result.returncode == 0, result.stdout
    assert "credentials: cannot tell" in result.stdout
    assert "not signed in" not in result.stdout
    # Unread is not healthy: the verdict must not say ready right after it.
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
                "the next session refreshes it if it can, else it opens the login menu") in result.stdout
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
    assert ("WARN credentials: the sign-in was not checked (Claude Code keeps it "
            "outside the profile home; the next session refreshes it if it can)") in result.stdout
    assert result.stdout.splitlines()[-1].startswith("WARN may not start"), result.stdout
    assert result.returncode == 0, result.stdout
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


@pytest.mark.parametrize("case", ["gateway-ok", "missing-credential", "malformed-listing"])
def test_doctor_ends_with_a_verdict(tmp_path, case):
    """The last line answers the only question a person has: is this profile
    ready to run, or what must be fixed first."""
    if case == "gateway-ok":
        result, _, _ = run_doctor(
            tmp_path, "type=apikey\nbase_url=https://example.test\napi_key=k\n", OK_CURL)
    elif case == "missing-credential":
        result, _, _ = run_doctor(tmp_path, "type=apikey\nbase_url=https://example.test\n")
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
        assert last == "FAIL not ready: fix config (see above)", result.stdout
    else:
        assert result.returncode == 1, result.stdout
        assert last.startswith("FAIL not ready: fix") and "gateway" in last, result.stdout


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
