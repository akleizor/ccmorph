"""Model-map agreement across the real shell launch, adapter, and gateway shim.

Only Claude Code and its upstream are fake. Each case copies the shipped library
into a private HOME and runs the public ``ccmorph run`` entry point. A map is
keyed by the wire model id: the launcher strips the ``[1m]`` window marker before
the adapter prepares a model, and Claude Code normalizes the same marker to the
bare id on the wire (observed for 2.1.301 during #232), so the stub reproduces
that normalization instead of echoing its own argv. The upstream serves only the
expected target, so a launch that prepared or requested anything else is
refused here rather than silently reaching a different model; every
launch-path fetch (catalogue, sizing, inference) lands on this loopback
gateway, and the one tool that would reach beyond it
(refresh-model-registry) is not copied. Preparation names the model it
resolved in the adapter's own log, and the answer must come back in the
client's requested identity, never the served target's.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import threading
import time
from unittest.mock import patch

import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
FULL = "claude-sonnet-5-5[1m]"
BARE = "claude-sonnet-5-5"
LIB_FILES = (
    "bin/ccmorph", "lib/ccmorph.sh", *sorted(f"lib/{p.name}" for p in (ROOT / "lib").glob("ccmorph-*.zsh")),
    "libexec/ccmorph-core", "libexec/gateway-shim", "libexec/model-window", "libexec/settings-compose",
    "share/window-sources.json", "share/model-catalogue.json",
)

CLAUDE = r'''import json, os, pathlib, sys, urllib.error, urllib.request
if "--help" in sys.argv[1:]:
    raise SystemExit(0)
args = sys.argv[1:]
model = args[args.index("--model") + 1] if "--model" in args else None
if model is None:
    raise SystemExit("launch did not pass --model")
# Claude Code 2.1.301, launched with --model <id>[1m], names the bare id in the
# Messages body and lifts the window into the context-1m beta header (#232's
# native-wire evidence). Send what the real client sends, not our own argv.
wire = model[:-4] if model.endswith("[1m]") else model
headers = {"content-type": "application/json", "anthropic-version": "2023-06-01",
           "x-api-key": os.environ["ANTHROPIC_API_KEY"],
           "x-launch-model-map-test": "inference"}
if wire != model:
    headers["anthropic-beta"] = "context-1m-2025-08-07"
row = {"argv": args, "requested": model, "wire": wire,
       "base_url": os.environ["ANTHROPIC_BASE_URL"]}
log = pathlib.Path(os.environ["HOME"]) / "launch.json"
try:
    request = urllib.request.Request(
        row["base_url"].rstrip("/") + "/v1/messages",
        data=json.dumps({"model": wire, "max_tokens": 16,
                         "messages": [{"role": "user", "content": "offline launch test"}]}).encode(),
        headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    # The stub's read waits on the shim, whose legitimate budgets (catalogue
    # 20 s, prepare timeout+5) outlast any small literal; read the suite's
    # shared bound through support.py's override.
    with opener.open(request, timeout=float(os.environ.get(
        "CCMORPH_TEST_HANG_TIMEOUT", "120"))) as response:
        row["status"] = response.status
        row["response"] = json.load(response)
except urllib.error.HTTPError as error:
    row["status"] = error.code
    row["error"] = error.read().decode()
finally:
    log.write_text(json.dumps(row))
raise SystemExit(0 if row["status"] == 200 else 1)
'''


@pytest.fixture
def launch(tmp_path):
    """Own one loopback gateway and the complete launch process group."""
    requests = []
    targets = set()

    class Gateway(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def reply(self, status, body):
            payload = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def do_GET(self):
            if self.path != "/v1/models":
                self.reply(404, {"error": "unknown route"})
                return
            # The listing names exactly what inference serves. Hermeticity does
            # not rest on its context_window: a suffixed launch runs at Claude
            # Code's own window for the id (declaring the id here changes
            # nothing — the adapter's window probe still falls back unprobed),
            # so the real guard is that every launch-path fetch goes to this
            # loopback base_url and the one tool that would fetch a live
            # catalogue (refresh-model-registry) is absent from LIB_FILES.
            self.reply(200, {"data": [
                {"id": target, "object": "model", "context_window": 200000,
                 "supported_endpoints": ["/v1/messages"]}
                for target in sorted(targets)
            ]})

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["content-length"])))
            model = body.get("model")
            requests.append({"path": self.path, "target": model,
                             "phase": "inference" if self.headers.get(
                                 "x-launch-model-map-test") == "inference" else "classification"})
            if self.path != "/v1/messages" or model not in targets:
                self.reply(400, {"error": {"message": "model or endpoint is not served"}})
                return
            self.reply(200, {"id": "msg_offline", "type": "message", "role": "assistant",
                             "model": model, "content": [{"type": "text", "text": "ok"}],
                             "stop_reason": "end_turn", "stop_sequence": None,
                             "usage": {"input_tokens": 1, "output_tokens": 1}})

    server = ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
    base = f"http://127.0.0.1:{server.server_address[1]}"
    state = tmp_path / ".config/ccmorph"
    state.mkdir(parents=True)
    (tmp_path / ".claude").mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    temp_dir = tmp_path / "tmp"
    temp_dir.mkdir()
    for name in LIB_FILES:
        (state / name).parent.mkdir(exist_ok=True)
        shutil.copy2(ROOT / name, state / name)
    # Use the pytest interpreter for shipped Python helpers as well as the stub;
    # never inherit the operator's PATH, credentials, proxy, or harness settings.
    (bin_dir / "python3").symlink_to(sys.executable)
    zsh = shutil.which("zsh")
    assert zsh, "zsh is required for the public shell entry point"
    (bin_dir / "zsh").symlink_to(zsh)
    stub = bin_dir / "claude.py"
    stub.write_text(CLAUDE)
    claude = bin_dir / "claude"
    claude.write_text('#!/bin/sh\nexec python3 -I "$HOME/bin/claude.py" "$@"\n')
    claude.chmod(0o755)
    env = {
        "HOME": str(tmp_path), "PATH": f"{bin_dir}:/usr/bin:/bin",
        "LANG": "C.UTF-8", "XDG_CONFIG_HOME": str(tmp_path / ".config"),
        "TMPDIR": str(temp_dir), "CCMORPH_STATE_DIR": str(state),
        "CCMORPH_LIB_DIR": str(state / "lib"),
        # One knob for every bound in the suite, the stub's own read included
        # (support.py's override).
        "CCMORPH_TEST_HANG_TIMEOUT": str(HANG_TIMEOUT),
    }

    def run(model_map, requested, expected):
        # The upstream serves only the expected target: a launch that resolved a
        # different model than this scenario's map is refused, not quietly rerouted.
        targets.add(expected)
        (state / "gw.conf").write_text(
            f"type=apikey\nbase_url={base}\napi_key=offline-key\nadapter=true\n"
            f"model_map={json.dumps(model_map)}\n")
        process = subprocess.Popen(
            [str(state / "bin/ccmorph"), "run", "gw", "--model", requested],
            cwd=tmp_path, env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            start_new_session=True,
        )
        try:
            # The suite's shared hang bound (tests/support.py), deliberately
            # above the 60 s per-test cap: a healthy launch stretches with
            # runner load, and a fixture-side bound near the loaded healthy
            # runtime killed healthy launches (round 1's finding). The cap
            # reports a hung launch with a thread dump; this bound backs the
            # reaps below, which the cap cannot see.
            stdout, stderr = process.communicate(timeout=HANG_TIMEOUT)
        except subprocess.TimeoutExpired as expired:
            stdout, stderr = expired.stdout or "", expired.stderr or ""
        finally:
            # The launcher normally reaps its adapter and shim; on an assertion,
            # timeout, or unexpected crash, reap only this fixture's process group.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                # The group is gone, which macOS spells two ways: EPERM while
                # its only member is the exited, unreaped launcher — the window
                # between the bound above and this kill (GH-5) — and ESRCH once
                # nothing is left. The launcher itself is still signalable, and
                # this kill reaps it, so the launch's own verdict, not a cleanup
                # error, is what the assertions below report.
                process.kill()
            try:
                process.communicate(timeout=HANG_TIMEOUT)
            except subprocess.TimeoutExpired:
                pass
        log = tmp_path / "launch.json"
        row = json.loads(log.read_text()) if log.exists() else None
        adapter_log = temp_dir / f"ccmorph-{os.getuid()}.log"
        adapter_text = adapter_log.read_text() if adapter_log.exists() else ""
        detail = (f"exit={process.returncode}\nstdout={stdout}\nstderr={stderr}\n"
                  f"launch={row}\nupstream={requests}\nadapter={adapter_text}")
        assert process.returncode == 0, detail
        assert row is not None, detail
        assert row["base_url"] != base, "launch bypassed the real gateway shim\n" + detail
        inference = [r for r in requests if r["phase"] == "inference"]
        assert [(r["path"], r["target"]) for r in inference] == [
            ("/v1/messages", expected)
        ], detail
        # Preparation's own log names the model it resolved. Pinning that
        # wording is deliberate: the selected-model handoff has no wire trace
        # (classification reads the /v1/models listing client-side, and this
        # fixture's single-target listing makes the inventory branch look
        # identical), so a regression that loses the handoff still launches
        # and infers cleanly — a round-2 mutation proved 4/4 green with it
        # dropped, and only this line refuses. A reworded diagnostic is a
        # one-line test update; that hole would be silent.
        assert f"classifying only the requested model '{expected}'" in adapter_text, \
            "launch preparation and inference must resolve the same mapped target\n" + detail
        # The shim answers in the client's requested identity, not the target's.
        wire = requested[:-4] if requested.endswith("[1m]") else requested
        assert row["response"]["model"] == wire, detail

    thread = threading.Thread(target=server.serve_forever,
                              kwargs={"poll_interval": 0.05}, daemon=True)
    try:
        thread.start()
        yield run
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)


@pytest.mark.parametrize(("model_map", "requested", "expected"), [
    pytest.param({BARE: "gpt-6-astra"}, FULL, "gpt-6-astra",
                 id="suffixed-launch-mapped-wire-key-issue-232"),
    pytest.param({BARE: "mapped-bare"}, BARE, "mapped-bare",
                 id="bare-launch-mapped-wire-key"),
    pytest.param({BARE: "gpt-6-astra[1m]"}, FULL, "gpt-6-astra",
                 id="mapped-target-window-marker-issue-232"),
    pytest.param({}, FULL, BARE,
                 id="suffixed-launch-unmapped-control"),
])
def test_launch_prepares_the_same_model_map_target_as_inference(launch, model_map,
                                                                requested, expected):
    launch(model_map, requested, expected)


@pytest.mark.skipif(sys.platform != "darwin",
                    reason="only macOS refuses to signal a group whose only member has exited")
def test_launch_cleanup_tolerates_a_leader_that_exited_between_bound_and_killpg_issue_5(launch):
    """GH-5: a launcher that exits in the window between the bound and the
    cleanup killpg leaves its group led by an unreaped zombie, which macOS
    refuses to signal; the cleanup must tolerate that, so the launch's own
    assertions below stay the verdict, not a cleanup PermissionError."""
    communicate, spent = subprocess.Popen.communicate, []

    def communicate_spending_the_bound_on_the_exiting_launcher(process, timeout=HANG_TIMEOUT):
        # The bound fires while the launcher is exiting: spend it waiting for
        # the exit nothing reaps — the group answers EPERM exactly then, and
        # nothing runs between that answer and the cleanup's killpg, so the
        # group that kill meets is this one — then report the bound blown. A
        # healthy launch reaches that answer in seconds; the stop holds a
        # platform that never refuses at half of pytest.ini's per-test cap, so
        # the failure would be the launch's own report, not a capped-out test.
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
                      communicate_spending_the_bound_on_the_exiting_launcher):
        # The unmapped control's own scenario, judged end to end: the window
        # forced above must change nothing the test can observe.
        launch({}, FULL, BARE)
