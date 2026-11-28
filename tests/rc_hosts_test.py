"""ccmorph-rc-hosts: config parsing and the relaunch policy."""
import json
import os
import threading
import time
import types
from pathlib import Path
from types import SimpleNamespace

import pytest

from support import (HANG_TIMEOUT, _ScriptedTime, load_script, needs_loopback,
                     port_outside_the_ephemeral_pool, proxy_took)

SCRIPT = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc-hosts"


def load():
    return load_script(SCRIPT, "rc_hosts")


def rc_start_mark(pid):
    """rc_route.proc_started, loaded by path: the stamp's own reader, so this
    test can name the mark it expects on the tree before #926, where rc-hosts
    does not re-export it -- the regression must fail on the published record,
    never on an AttributeError in its own fixture."""
    return load_script(SCRIPT.parent / "rc_route.py", "rc_route").proc_started(pid)


@pytest.fixture(autouse=True)
def state_dir(tmp_path, monkeypatch):
    """A loaded module computes LOGS/EDGES/BUSY from CCMORPH_STATE_DIR at import;
    without this, every test reaching Host.start() wrote into ~/.config/ccmorph
    (issue #179: 505 pytest logs in the live .rc-bridge directory)."""
    monkeypatch.setenv("CCMORPH_STATE_DIR", str(tmp_path / "state"))


def test_one_route_line_serves_every_folder_and_a_folder_may_override(tmp_path):
    m = load()
    a, b = tmp_path / "a", tmp_path / "b"
    got = m.parse(f"# comment\nroute=profile\n{a}\n{b} route=ds   # own provider\n\n")
    assert got == {str(a): ("profile", ()), str(b): ("ds", ())}
    got = m.parse(f"route=profile\n{a} -- --session-id session_01X\n{b} route=ds -- -c\n")
    assert got == {str(a): ("profile", ("--session-id", "session_01X")), str(b): ("ds", ("-c",))}
    assert m.parse(f"{a}\n") == {}                      # no route anywhere: nothing to run


@pytest.mark.parametrize("line, kept", [
    pytest.param("{a}", True, id="bare-folder"),
    pytest.param("{a} -- --session-id session_01X", True, id="valued-pin"),
    pytest.param("{a} -- --continue", True, id="known-passthrough"),
    pytest.param("{a} -- --no-create-session-in-dir", True, id="unknown-passthrough"),
    pytest.param("{a} -- --session-id", False, id="valueless-pin-issue-342"),
    pytest.param("{a} -- --session-id  ", False, id="valueless-pin-trailing-space-issue-342"),
    pytest.param("{a} -- --session-id # note", False, id="valueless-pin-with-comment-issue-342"),
])
def test_only_a_pin_that_names_a_conversation_is_hosted(tmp_path, line, kept):
    """A valueless `--session-id` crashed Host construction with IndexError, out of
    main() at every config reload (issue #342). It may not be hosted unpinned
    either: the bridge treats any `--session-id` as a pin (`ccmorph-rc` skips
    keep_host for one), so an unpinned host would take worktree mode and git-init
    the folder. Args the line does carry still pass through unchanged."""
    m = load()
    a = tmp_path / "a"
    got = m.parse(f"route=gateway\n{line.format(a=a)}\n")
    assert bool(got) is kept
    if kept:                       # whatever was typed after `--` reaches the child as typed
        assert got[str(a)] == ("gateway", tuple(line.format(a=a).partition(" -- ")[2].split()))


@pytest.mark.parametrize("line, want", [
    pytest.param("{a} -- --session-id s1", [("{a}", "s1")], id="valued-pin"),
    pytest.param("{a} --session-id s1", [], id="pin-without-separator"),
    pytest.param("{a} -- --session-id", [], id="valueless-pin"),
    pytest.param("{a} -- --continue", [], id="not-a-pin"),
    pytest.param("{a}/.claude/worktrees/bridge-s1 -- --session-id s1", [], id="worktree-pin"),
])
def test_session_entries_lists_only_pins_that_name_a_conversation(tmp_path, line, want):
    """A pin line with no ` -- ` separator raised ValueError out of main()'s idle
    cleanup, and launchd restarted the supervisor into the same crash with every
    host (issue #388, same class as #342). The line names a conversation nowhere,
    so idle cleanup simply skips it, as parse() already did. A worktree pin
    parse() refuses (issue #416) is skipped the same way: no host ever ran it,
    so idle cleanup may not retire it."""
    m = load()
    a = tmp_path / "a"
    assert list(m.session_entries(line.format(a=a) + "\n")) == [(m.resolve(f.format(a=a)), s) for f, s in want]


def test_a_valueless_pin_does_not_take_the_supervisor_down(tmp_path, monkeypatch):
    """One malformed line must not stop the others (issue #342). Host construction
    raised IndexError out of main() at every config reload, so launchd restarted
    the supervisor into the same crash and every host -- the unrelated folder's
    included -- was stopped and restarted with it."""
    m = load()
    repo, bad = tmp_path / "repo", tmp_path / "bad"
    for d in (repo, bad):
        d.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n{bad} -- --session-id\n")
    started = []
    def start(self):
        if not self.session_id and not self.edge:   # as the real start() does
            self.ensure_repo()
        self.proc, self.started = FakeProc(), m.time.time()
        started.append(self.folder)
    run_main(m, monkeypatch, tmp_path, cfg, start)
    assert [f for f in started if f != str(m.EDGES.parent)] == [str(repo)]
    assert not (bad / ".git").exists()   # refused, never git-init'd a folder it was not asked to host


def test_a_pin_without_a_separator_does_not_take_the_supervisor_down(tmp_path, monkeypatch):
    """A pin line that carries no ` -- ` separator (`~/x --session-id abc`) raised
    ValueError out of main()'s idle cleanup, and launchd restarted the supervisor
    into the same crash with every host (issue #388, the #342 class). The idle
    cycle now runs on and the other folder still starts; `cleaned` proves the
    cleanup seam was reached, not skipped."""
    m = load()
    repo, bad = tmp_path / "repo", tmp_path / "bad"
    for d in (repo, bad):
        d.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n{bad} --session-id abc\n")
    clock = [1_000_000.0]
    started, cleaned = [], []

    def start(self):
        self.proc, self.started = FakeProc(), clock[0]
        started.append(self.folder)
    monkeypatch.setattr(m, "clean_archived",
                        lambda folders, now, lim, p, pinned: cleaned.append(folders))

    def sleep(_):
        clock[0] += m.IDLE_CHECK            # every sleep puts the idle check due again
        if len(cleaned) >= 2:
            raise SystemExit                # the supervisor outlived the idle seam
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep, clock=clock)
    assert len(cleaned) >= 2
    assert cleaned[-1] == [str(repo), str(bad)]     # the bad line is an unpinned folder, no more
    assert [f for f in started if f != str(m.EDGES.parent)] == [str(repo), str(bad)]


READ_FAILURES = [   # the #340 window: a read that fails after a successful stat
    pytest.param(lambda: PermissionError(13, "injected read error after successful stat"),
                 id="permissions-pulled"),
    pytest.param(lambda: UnicodeDecodeError("utf-8", b"\xff", 0, 1, "invalid start byte"),
                 id="undecodable-bytes"),
]


def _unreadable_between(cfg, ticks, lo, hi, boom):
    """A read_text standing in for the window: cfg alone raises a fresh `boom()`
    while ticks[0] is in [lo, hi) -- the stat still succeeds, only the read
    fails. A factory, not a shared instance: an exception re-raised across ticks
    keeps every earlier frame in its __traceback__."""
    real = type(cfg).read_text

    def read_text(self, *a, **k):
        if lo <= ticks[0] < hi and self == cfg:
            raise boom()
        return real(self, *a, **k)

    return read_text


def _unwritable_between(cfg, ticks, lo, hi, boom):
    """The other half of the same window (issue #355): cfg alone refuses the
    rewrite with a fresh `boom()` while ticks[0] is in [lo, hi) -- a config that
    turned read-only at the moment of the retire, which reads back fine. The
    rewrite lands in one os.replace of a temp written beside cfg (#412), so
    that is where the refusal lands: the temp is taken out, cfg untouched."""
    real, target = os.replace, cfg.resolve()

    def replace(src, dst):
        if lo <= ticks[0] < hi and Path(dst) == target:
            raise boom()
        return real(src, dst)

    return replace


# The retire edit's two halves can each fail: the read it starts with, or the
# rewrite it ends with. Same factories as READ_FAILURES, so a fix that holds for
# one selection of the window's halves holds for the other. The rewrite lands
# in one os.replace (#412), so that half is injected at the replace.
EDIT_FAILURES = [pytest.param("read_text", p.values[0], id=p.id) for p in READ_FAILURES] + [
    pytest.param("replace", lambda: PermissionError(13, "injected write error"), id="write-refused"),
]


@pytest.mark.parametrize("boom", READ_FAILURES)
def test_an_unreadable_config_does_not_take_the_supervisor_down(tmp_path, monkeypatch, capsys, boom):
    """config.read_text() ran unguarded after a successful stat (issue #340): a
    config that had just changed but turned unreadable -- permissions pulled, or
    a write cut mid-byte -- raised out of main(), and launchd restarted the
    supervisor into the same crash, stopping and restarting every host. A failed
    read keeps the loaded folders running, and the mtime is committed only once
    the load succeeded, so the file restored readable is picked up even though
    its mtime never moved again (chmod restores no mtime)."""
    m = load()
    repo, added = tmp_path / "repo", tmp_path / "added"
    for d in (repo, added):
        d.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n")
    os.utime(cfg, (1_000_000, 1_000_000))   # fixed mtimes: no filesystem granularity decides this test
    started = []

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        started.append(self.folder)

    ticks, clock = [0], [1000.0]
    # ticks 2-4: the changed config is unreadable (stat still succeeds, only the
    # read fails); tick 5 on: readable again, the mtime where the change left it
    monkeypatch.setattr(type(cfg), "read_text", _unreadable_between(cfg, ticks, 2, 5, boom))

    def sleep(_):
        ticks[0] += 1
        clock[0] = 1000.0 + ticks[0] * m.TICK
        if ticks[0] == 2:      # the config changed, and turned unreadable at once
            cfg.write_text(f"route=gateway\n{repo}\n{added}\n")
            os.utime(cfg, (2_000_000, 2_000_000))
        if ticks[0] > 8:
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, clock=clock)
    assert [f for f in started if f != str(m.EDGES.parent)] == [str(repo), str(added)]
    assert "config not read" in capsys.readouterr().out


@pytest.mark.parametrize("blackout", [False, True],
                         ids=["readable", "unreadable-after-our-write"])
def test_our_own_retire_edit_is_not_read_as_a_config_change(tmp_path, monkeypatch, capsys, blackout):
    """The retire paths rewrite the config themselves; that rewrite must not act
    as an operator's change (found by review of #340). A folder with a bare line
    shadowed by a pinned line used to stay unhosted once the pin's line was
    retired; reloading our own edit launched an unpinned host for the shadowed
    line -- and stopped the edge for a tick -- with no external change at all.
    A successful edit commits the observed mtime, so the next tick does not
    reload it -- and the acknowledgement must not depend on reading the file
    back: a config that turns unreadable right after our own write (review
    round on the merge-gate head) left the edit unacknowledged, and recovery
    read it as an operator's change."""
    m = load()
    f = tmp_path / "repo"
    f.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{f}\n{f} -- --session-id session_01X\n")  # the pin shadows the bare line
    os.utime(cfg, (1_000_000, 1_000_000))   # a fixed mtime: no filesystem granularity decides this test
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))  # project_dir stays in tmp
    monkeypatch.setattr(m, "account_token", lambda: "tok")   # the idle check's purge pass
    monkeypatch.setattr(m, "sessions", lambda token, status: [])
    monkeypatch.setattr(m.os, "killpg", lambda pid, sig: None)  # a retire may stop hosts; no real signal
    started = []
    pinned = {}               # the pinned Host, once start() has run for it

    def start(self):
        self.proc, self.started = DyingProc(), m.time.time()
        started.append(self.folder)
        if self.session_id:
            pinned["h"] = self

    clock, ticks = [1000.0], [0]
    if blackout:
        wrote = []         # the ticks our own retire edits landed on
        real_read, real_replace = type(cfg).read_text, os.replace
        target = cfg.resolve()

        def replace(src, dst):
            r = real_replace(src, dst)
            if Path(dst) == target:
                wrote.append(ticks[0])
            return r

        def read_text(self, *a, **k):
            # the #340 window turned on our own write: the rest of that tick and
            # the next cannot read the config back (the stat still succeeds)
            if self == cfg and wrote and ticks[0] <= wrote[-1] + 1:
                raise PermissionError(13, "injected read error after successful stat")
            return real_read(self, *a, **k)

        monkeypatch.setattr(m.os, "replace", replace)
        monkeypatch.setattr(type(cfg), "read_text", read_text)

    def sleep(_):
        ticks[0] += 1
        clock[0] += m.TICK
        if ticks[0] == 2:      # the pinned bridge exits; its session log names the archive
            pinned["h"].proc.rc = 0
            d = pinned["h"].debugfile
            d.with_name(d.stem + "-cse_01X.log").write_text(
                "session_not_active (409): the conversation was archived")
        if ticks[0] == 4:      # past IDLE_CHECK and idle_days: the line is retired
            clock[0] += 100_000
        if ticks[0] > 12:
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, clock=clock)
    assert started == [str(m.EDGES.parent), str(f)]   # the pin once; never an unpinned host
    out = capsys.readouterr().out
    assert out.count("config:") == 1                  # only the first load: our edits reload nothing
    assert "# idle-retired" in cfg.read_text()        # the retire did happen (not vacuously green)


@pytest.mark.parametrize("method, boom", EDIT_FAILURES)
def test_a_failed_retire_edit_neither_stops_the_supervisor_nor_unarchives(
        tmp_path, monkeypatch, capsys, method, boom):
    """The retire paths edit the config from inside main()'s loop, and that edit
    read and rewrote the config unguarded too: the #340 window at the edit_config
    call also exited main() (found by the merge gate). The edit is logged and
    skipped, and the supervisor keeps running the other hosts through their
    restarts -- but the skipped edit leaves the pinned line active-looking in the
    file, so the next reload hosts that folder again and restarts the
    conversation that was archived, un-archiving it: an operator touch inside the
    window is enough (#355). The retire now stands until its marker is in the
    file: the folder is not re-hosted on a reload the window let through, and the
    marker write is retried on later ticks until it lands."""
    m = load()
    repo, a, b = tmp_path / "repo", tmp_path / "a", tmp_path / "b"
    for d in (repo, a, b):
        d.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\n{a} -- --session-id session_01AAA\n"
                   f"{b} -- --session-id session_01BBB\n")
    os.utime(cfg, (1_000_000, 1_000_000))
    monkeypatch.setattr(m.os, "killpg", lambda pid, sig: None)  # a retire may stop hosts
    started = []
    procs = {}

    def start(self):
        self.proc, self.started = DyingProc(), m.time.time()
        procs[self.folder] = self
        started.append(self.folder)

    ticks, clock = [0], [1000.0]
    # ticks 4-8: every host is up, the bridges exit, the config fails exactly
    # when the retire path edits it, and an operator touch lands mid-window
    if method == "read_text":   # the read half of the window
        monkeypatch.setattr(type(cfg), method, _unreadable_between(cfg, ticks, 4, 9, boom))
    else:                       # the rewrite half: one os.replace now (#412)
        monkeypatch.setattr(m.os, method, _unwritable_between(cfg, ticks, 4, 9, boom))

    def sleep(_):
        ticks[0] += 1
        clock[0] = 1000.0 + ticks[0] * m.TICK
        if ticks[0] == 4:      # every bridge exits rc=0; a's session log names the archive
            for f in (repo, a, b):
                procs[str(f)].proc.rc = 0
            d = procs[str(a)].debugfile
            d.with_name(d.stem + "-cse_01AAA.log").write_text(
                "session_not_active (409): the conversation was archived")
        if ticks[0] == 7:      # the operator touches the file: the load still fails, or
            os.utime(cfg, (3_000_000, 3_000_000))   # a's marker never landed, so a reload
        if ticks[0] > 14:                          # lists a again and restarts it
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, clock=clock)
    assert started.count(str(a)) == 1            # retired: never restarted, marker or no
    assert started.count(str(repo)) == 2         # the others ride out the window
    assert started.count(str(b)) == 2
    out = capsys.readouterr().out
    assert "archived or expired; bridge retired" in out   # the retire did happen
    assert "config not edited" in out                     # the failed edit, logged not fatal
    assert f"archived={a}" in cfg.read_text()             # and retried until it landed


def test_a_config_rewrite_that_fails_mid_replace_leaves_the_file_it_had(
        tmp_path, monkeypatch, capsys):
    """GH-412: edit_config() rewrote the config with Path.write_text(), which
    truncates the file on open and then writes -- a write that failed after the
    truncate (disk full, an I/O error) left the config empty or cut short, and
    the next reload stopped every host it listed. The rewrite now lands in one
    os.replace of a temp file written beside the config: the moment the replace
    fails, the config still holds exactly what it held, and the half-done temp
    is taken out with the failure."""
    m = load()
    folder = tmp_path / "repo"
    folder.mkdir()
    f = m.resolve(str(folder))
    cfg = tmp_path / "rc-hosts"
    before = f"route=profile\n{f} -- --session-id session_01X\n"
    cfg.write_text(before)
    cfg_here, real_replace, faults = cfg.resolve(), m.os.replace, [1]

    def replace(src, dst):   # the rewrite's one landing step, refused once
        if Path(dst) == cfg_here and faults:
            faults.pop()
            raise OSError(28, "injected replace error: no space left on device")
        return real_replace(src, dst)

    monkeypatch.setattr(m.os, "replace", replace)

    assert m.edit_config(cfg, f, "session_01X", loaded=before) is None   # did not land
    assert cfg.read_text() == before      # the previous content, byte for byte
    assert sorted(p.name for p in cfg.parent.iterdir()) == ["rc-hosts", "repo"]  # no temp left
    assert "config not edited" in capsys.readouterr().out

    assert m.edit_config(cfg, f, "session_01X", loaded=before) is not None  # not vacuous:
    assert "# idle-retired" in cfg.read_text()   # the same edit lands once the replace can


def test_the_rewrite_keeps_a_symlinked_config_its_link_and_its_mode(tmp_path):
    """GH-412: the one-step rewrite os.replace()s a temp file over the config
    path -- which, for a config the operator symlinked into place, would replace
    the link itself and leave the real file stale, silently unmoved by every
    later edit. The temp is written and replaced beside the *resolved* config,
    with the mode the config had; the link keeps pointing at the file the
    supervisor edits."""
    m = load()
    folder = tmp_path / "repo"
    folder.mkdir()
    f = m.resolve(str(folder))
    real = tmp_path / "real-hosts"
    real.write_text(f"route=profile\n{f} -- --session-id session_01X\n")
    cfg = tmp_path / "rc-hosts"
    cfg.symlink_to(real)
    os.chmod(real, 0o640)   # the mode the operator set survives the rewrite

    assert m.edit_config(cfg, f, "session_01X", loaded=real.read_text()) is not None
    assert cfg.is_symlink()                        # the link itself was not replaced
    assert "# idle-retired" in real.read_text()    # the edit landed in the linked file
    assert real.stat().st_mode & 0o777 == 0o640


def _unreadable_once(cfg, window, boom):
    """A read_text standing in for the shortest #340 window: cfg alone raises a
    fresh `boom()` for the single read while `window[0]` is armed, then the
    operator restores readability -- a later read in the same tick succeeds."""
    real = type(cfg).read_text

    def read_text(self, *a, **k):
        if self == cfg and window[0]:
            window[0] = False
            raise boom()
        return real(self, *a, **k)

    return read_text


def test_a_retire_edit_does_not_acknowledge_a_config_it_never_loaded(tmp_path, monkeypatch, capsys):
    """The retire acknowledgement commits the edit's (text, mtime) as already
    loaded (review round 3 of #340's rebase): valid only while what edit_config
    read is the configuration the hosts are running. An operator change that
    landed during an unreadable window left the reload pending; the retire edit
    of that same tick then read -- and rewrote -- the changed file. Carrying that
    edit's text (it was never acked) handed the second retire in the same tick a
    file equal to what the loop carried, so *its* edit acknowledged: the
    operator's change was committed as loaded, the pending reload was swallowed,
    and the added folder never started (review of the #355 landing). Two pinned
    bridges is the smallest shape of it: the first edit only has to leave the
    caller holding the changed file."""
    m = load()
    repo, a, c, b = (tmp_path / n for n in ("repo", "a", "c", "b"))
    for d in (repo, a, c, b):
        d.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n{a} -- --session-id session_01AAA\n"
                   f"{c} -- --session-id session_01CCC\n")
    os.utime(cfg, (1_000_000, 1_000_000))   # a fixed mtime: no filesystem granularity decides this test
    monkeypatch.setattr(m.os, "killpg", lambda pid, sig: None)  # a retire may stop hosts
    started = []
    pinned = {}               # the pinned Hosts, keyed by folder, once start() has run

    def start(self):
        self.proc, self.started = DyingProc(), m.time.time()
        started.append(self.folder)
        if self.session_id:
            pinned[self.folder] = self

    ticks, clock, window = [0], [1000.0], [False]

    def sleep(_):
        ticks[0] += 1
        clock[0] = 1000.0 + ticks[0] * m.TICK
        if ticks[0] == 4:      # the operator adds a folder; both pinned bridges exit archived
            cfg.write_text(f"route=gateway\n{repo}\n{a} -- --session-id session_01AAA\n"
                           f"{c} -- --session-id session_01CCC\n{b}\n")
            os.utime(cfg, (2_000_000, 2_000_000))
            window[0] = True   # and pulls readability until the next read has failed once
            for folder, sid in ((a, "01AAA"), (c, "01CCC")):
                pinned[str(folder)].proc.rc = 0
                d = pinned[str(folder)].debugfile
                d.with_name(f"{d.stem}-cse_{sid}.log").write_text(
                    "session_not_active (409): the conversation was archived")
        if ticks[0] > 8:
            raise SystemExit

    # tick 5: the reload's read fails (the window), then both retire edits read --
    # and rewrite -- the changed file; neither may count as already loaded
    monkeypatch.setattr(type(cfg), "read_text",
                        _unreadable_once(cfg, window, lambda: PermissionError(
                            13, "injected read error after successful stat")))

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, clock=clock)
    assert [f for f in started if f != str(m.EDGES.parent)] == \
        [str(repo), str(a), str(c), str(b)]     # b last: the reload happened after the retires
    out = capsys.readouterr().out
    assert "config not read" in out             # the reload really failed first
    assert out.count("config:") == 2            # ...and was retried, not swallowed
    text = cfg.read_text()
    assert f"archived={a}" in text and f"archived={c}" in text  # both retires did happen


def test_the_idle_purge_does_not_run_under_a_config_it_never_loaded(tmp_path, monkeypatch, capsys):
    """The destructive idle pass must wait for the config it will apply (review
    round 2 on the rebase): a reload that failed, or a retire edit that landed
    on changed content, leaves `text` at the last loaded config while the file
    holds the operator's new one -- purging then deleted a ten-day-old session
    under the stale seven-day default although `purge_days=30` sat on disk, and
    a delete cannot be undone. The pass is deferred until a reload has landed;
    then it runs with the operator's settings, and the session survives."""
    import json
    m = load()
    repo, a = tmp_path / "repo", tmp_path / "a"
    for d in (repo, a):
        d.mkdir()
    (repo / ".claude").mkdir()
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n{a} -- --session-id session_01AAA\n")
    os.utime(cfg, (1_000_000, 1_000_000))   # a fixed mtime: no filesystem granularity decides this test
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))  # project_dir stays in tmp
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m.os, "killpg", lambda pid, sig: None)  # a retire may stop hosts
    base = m.stamp("2026-10-20T12:00:00Z")   # a fake now: the session below is ten days archived
    archived = [{"id": "cse_01OLD", "environment_id": "env_01R", "updated_at": "2026-10-10T12:00:00Z",
                 "environment_kind": "bridge"}]
    monkeypatch.setattr(m, "sessions", lambda token, status: archived if status == "archived" else [])
    deleted = []
    monkeypatch.setattr(m, "api", lambda method, path, token, body=None:
                        deleted.append(path) or {})   # the purge's DELETE, recorded not sent
    started = []
    pinned = {}               # the pinned Host, once start() has run for it

    def start(self):
        self.proc, self.started = DyingProc(), m.time.time()
        started.append(self.folder)
        if self.session_id:
            pinned["h"] = self

    ticks, clock, window = [0], [base], [False]

    def sleep(_):
        ticks[0] += 1
        clock[0] += m.TICK
        if ticks[0] == 4:      # the operator demands 30-day retention, pulls readability for
            clock[0] += 100_000   # one read, and the pinned bridge exits archived
            cfg.write_text(f"route=gateway\n{repo}\n{a} -- --session-id session_01AAA\npurge_days=30\n")
            os.utime(cfg, (2_000_000, 2_000_000))
            window[0] = True
            pinned["h"].proc.rc = 0
            d = pinned["h"].debugfile
            d.with_name(d.stem + "-cse_01AAA.log").write_text(
                "session_not_active (409): the conversation was archived")
        if ticks[0] == 6:      # past IDLE_CHECK again: the deferred pass must run, on the loaded config
            clock[0] += 1_200
        if ticks[0] > 9:
            raise SystemExit

    # tick 5: the reload's read fails (the window); the retire edit reads the
    # changed, restored file -- unacknowledged, so `text` stays stale all tick
    monkeypatch.setattr(type(cfg), "read_text",
                        _unreadable_once(cfg, window, lambda: PermissionError(
                            13, "injected read error after successful stat")))

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, clock=clock)
    out = capsys.readouterr().out
    assert deleted == []                       # ten days old: kept, the loaded 30 days say
    assert "# idle-retired" in cfg.read_text() # the deferred pass did run after the reload
    assert "config not read" in out            # the premise held: the reload failed first
    assert "config edited but changed" in out  # ...and the retire edit was left unacknowledged


@pytest.mark.parametrize("where", ["archived-bridge", "idle-retired"])
def test_an_operator_edit_saved_with_a_self_edit_is_still_loaded(tmp_path, monkeypatch, capsys, where):
    """GH-357: no read fails here -- the operator saved the config in the tick
    that the supervisor also edited it, so the window test above is a different
    path. The edit reads their changed text and rewrites it: an edit on content
    the hosts never loaded. Acknowledging it commits their mtime as applied and
    swallows the reload that was already due, so the folder they added never
    starts. Neither self-edit site -- a bridge that exited archived, the idle
    pass -- may acknowledge it."""
    m = load()
    repo, a, b = tmp_path / "repo", tmp_path / "a", tmp_path / "b"
    for d in (repo, a, b):
        d.mkdir()
    marker = f"archived={a}\n" if where == "idle-retired" else ""
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n{a} -- --session-id session_01AAA\n{marker}")
    os.utime(cfg, (1_000_000, 1_000_000))   # a fixed mtime: no filesystem granularity decides this test
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))  # project_dir stays in tmp
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "sessions", lambda token, status: [])
    monkeypatch.setattr(m.os, "killpg", lambda pid, sig: None)  # a retire may stop hosts
    monkeypatch.setattr(m, "IDLE_CHECK", 0)                     # the idle pass is due every tick
    started = []
    pinned = {}               # the pinned Host, once start() has run for it

    def start(self):
        self.proc, self.started = DyingProc(), m.time.time()
        started.append(self.folder)
        if self.session_id:
            pinned["h"] = self

    ticks, clock, saved = [0], [200_000.0], [False]
    real_edit = m.edit_config

    def edit(config, folder, sid=None, **kw):
        if config == cfg and not saved[0]:   # the operator saves, with the supervisor's edit
            saved[0] = True
            config.write_text(config.read_text() + f"{b}\n")
        return real_edit(config, folder, sid, **kw)

    monkeypatch.setattr(m, "edit_config", edit)

    def sleep(_):
        ticks[0] += 1
        clock[0] += m.TICK
        if ticks[0] == 4 and where == "archived-bridge":   # the pinned bridge exits archived
            pinned["h"].proc.rc = 0
            d = pinned["h"].debugfile
            d.with_name(d.stem + "-cse_01AAA.log").write_text(
                "session_not_active (409): the conversation was archived")
        if ticks[0] > 8:
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, clock=clock)
    assert saved[0]        # the premise held: a self-edit ran, so the operator's save landed with it
    # the idle pass retires `a`'s line in the tick it first runs, before that
    # host's turn to start: `b` -- the folder the operator added -- is what must host
    expected = [str(repo), str(b)] if where == "idle-retired" else [str(repo), str(a), str(b)]
    assert [f for f in started if f != str(m.EDGES.parent)] == expected
    out = capsys.readouterr().out
    assert out.count("config:") == 2            # the operator's change was reloaded, not swallowed
    assert str(b) in cfg.read_text()            # and their line survived the edit (not vacuous)


def test_a_worktree_session_is_never_pinned(tmp_path):
    """Pinning a worktree session hijacked its repo's host row in the app (2026-10-10)."""
    m = load()
    wt = tmp_path / "repo" / ".claude" / "worktrees" / "bridge-cse_01X"
    got = m.parse(f"route=profile\n{tmp_path / 'repo'}\n{wt} -- --session-id session_01X\n")
    assert got == {str(tmp_path / "repo"): ("profile", ())}


@pytest.mark.parametrize("line", [
    "a\x00b -- --session-id session_01X",
    "archived=a\x00b",
], ids=["nul-pin-issue-414", "nul-archived-issue-414"])
def test_a_line_whose_path_cannot_be_resolved_is_skipped_not_fatal(tmp_path, line):
    """GH-414: resolve() raises for a path it cannot canonicalise (an embedded
    NUL byte, the one form every python rejects), and parse() called it bare on
    every folder and archived= line -- one such line crashed the supervisor at
    every config reload. The line is skipped; the other folders keep their
    hosts."""
    m = load()
    a = tmp_path / "a"
    got = m.parse(f"route=gateway\n{a}\n{line}\n")
    assert got == {str(a): ("gateway", ())}


@pytest.mark.parametrize("boom", [
    RuntimeError("Symlink loop from /private/tmp/self"),   # pythons <= 3.12, the system 3.9 included
    OSError("Too many levels of symbolic links"),
], ids=["symlink-loop-issue-414", "oserror-issue-414"])
def test_resolve_answers_none_for_what_path_resolve_raises(monkeypatch, boom):
    """GH-414: on the system python 3.9 that runs the supervisor, and up to
    3.12, resolving a symlink loop raises RuntimeError -- the form the issue
    verified; newer pythons return the loop path instead, so the exception is
    injected by replacing the module's own Path. Whatever Path.resolve raises
    for a path it cannot canonicalise, resolve() answers None and the config
    readers skip the line, instead of taking the supervisor down."""
    m = load()

    class UnresolvablePath:
        def __init__(self, path):
            self.path = path

        def resolve(self):
            raise boom
    monkeypatch.setattr(m, "Path", UnresolvablePath)
    assert m.resolve("/tmp/self/x") is None


def test_an_archived_worktree_pin_is_not_retired_by_idle_cleanup(tmp_path, monkeypatch):
    """parse() refuses a worktree pin (it would take over its repo's host), but
    session_entries() still yielded the pair, so main()'s idle cleanup deleted a
    host row and commented the line out for a session no host ever ran (issue
    #416). The pin rule now lives in one place and both readers apply it, so a
    refused pin is never listed and never retired, while a hosted archived pin
    in the same cycle still is: that is what the list is for."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    wt = repo / ".claude" / "worktrees" / "bridge-cse_01X"
    bridge = tmp_path / "bridge"   # a hosted pin, archived and idle: retired as before
    bridge.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\n{wt} -- --session-id cse_01X\n"
                   f"archived={wt}\n{bridge} -- --session-id ses_01X\narchived={bridge}\n")
    clock = [1_000_000.0]
    cleaned, deleted = [], []

    def start(self):
        self.proc, self.started = FakeProc(), clock[0]

    monkeypatch.setattr(m, "clean_archived",
                        lambda folders, now, lim, p, pinned: cleaned.append(folders))
    monkeypatch.setattr(m, "last_activity", lambda folder: 0.0)      # idle past any limit

    def delete(h):                   # settled, as the server's record answering would be
        deleted.append(h.folder)
        return True
    monkeypatch.setattr(m, "delete_host", delete)

    def sleep(_):
        clock[0] += m.IDLE_CHECK            # every sleep puts the idle check due again
        if len(cleaned) >= 2:
            raise SystemExit                # the idle seam ran twice and stayed clean
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep, clock=clock)
    assert len(cleaned) >= 2                # the cleanup cycle ran, was not skipped
    assert deleted == [str(bridge)]         # only the hosted pin's row is deleted
    text = cfg.read_text()
    active = [l.split("#", 1)[0].strip() for l in text.splitlines()]
    assert f"{wt} -- --session-id cse_01X" in active   # never hosted: line left alone
    assert f"archived={wt}" in active       # its marker is left for the app-side retire path
    assert f"{bridge} -- --session-id ses_01X" not in active and f"archived={bridge}" not in active
    assert f": {bridge} -- --session-id ses_01X" in text and text.count("idle-retired") == 1


class FakeProc:
    def __init__(self, rc=None):
        self.rc, self.pid = rc, 1

    def poll(self):
        return self.rc


def run_main(m, monkeypatch, tmp_path, cfg, start, sleep=None,
             edge_ready=lambda route: True, clock=None, args=None, logs=True,
             profile_tiers=lambda route: {}, exits=True):
    """Drive m.main() over cfg until the scripted sleep raises SystemExit: logs
    under tmp_path, Host.start faked by `start`, the clock read from `clock`
    when given. `args` replaces the `--config` argument list main() gets.
    Without a `sleep`, main() stops on its fifth sleep. `logs=False` leaves
    tmp_path/logs absent: a fresh state dir whose first reload reaches the
    switch bring-up before any Host.start created LOGS (issue #596).
    `profile_tiers` answers for `ccmorph get <route> --json`'s tier map.
    `exits=False` lets main() return its own exit code instead: a tick that
    asks for a clean stop (the SIGTERM handler) ends the loop and returns."""
    monkeypatch.setattr(m, "LOGS", tmp_path / "logs")
    monkeypatch.setattr(m, "read_profile_tiers", profile_tiers, raising=False)
    if logs:
        (tmp_path / "logs").mkdir(exist_ok=True)
    monkeypatch.setattr(m, "edge_ready", edge_ready)
    monkeypatch.setattr(m.Host, "start", start)
    if sleep is None:
        ticks = [0]
        def sleep(_):
            ticks[0] += 1
            if ticks[0] > 4:
                raise SystemExit
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, sleep))
    argv = args if args is not None else ["--config", str(cfg)]
    if not exits:
        return m.main(argv)
    with pytest.raises(SystemExit):
        m.main(argv)


def test_a_foreign_sleeper_cannot_advance_a_clock_patched_main(tmp_path, monkeypatch):
    """GH-1002: another file's daemon thread can wake from the process's own
    `time.sleep` while this test drives main(). Under a shared-module patch that
    wake ran the test's sleep script -- one tick per foreign wake, a spin -- and
    the fake clock answered every other thread. _ScriptedTime is patched onto
    the loaded module's own `time`, so the foreign wake sleeps for real and
    reads the real clock, and only main()'s own sleeps advance the script."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\n")
    real_sleep, seen, ticks = time.sleep, [], []

    def start(self):
        if not seen:                 # awake inside main()'s first tick, as a leak would be
            def foreign():
                seen.append((time.sleep, time.time()))
                time.sleep(0)        # a real sleep, never the script's tick
            thread = threading.Thread(target=foreign)
            thread.start()
            thread.join()
            assert len(ticks) == 0   # the foreign wake did not advance main()
        self.proc, self.started = FakeProc(), 0.0

    def tick(seconds):
        ticks.append(seconds)
        if len(ticks) > 2:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, tick, clock=[1000.0])
    assert seen[0][0] is real_sleep          # the shared module kept the real sleep
    assert seen[0][1] > 1_000_000_000        # ... and the real epoch, not the script's 1000.0
    assert len(ticks) == 3                   # only main()'s own sleeps ran the script
    assert m.time.time() == 1000.0           # the driven module still read the fixture's clock


def test_the_attached_config_spelling_names_the_file_watched(tmp_path, monkeypatch):
    """`--config=PATH` named nothing: main() read only the exact string
    `--config`, so the equals spelling used across ccmorph's other CLIs
    (`--profile=`, `--model=`, `--effort=`) silently watched the default file
    instead (issue #417). The separate spelling is the argv every other
    run_main test drives."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\n")
    started = []
    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        started.append(self.folder)
    run_main(m, monkeypatch, tmp_path, cfg, start, args=[f"--config={cfg}"])
    assert [f for f in started if f != str(m.EDGES.parent)] == [str(repo)]


@pytest.mark.parametrize("argv, says", [
    pytest.param(["--config"], "expected one argument", id="valueless-issue-417"),
    pytest.param(["--config", "--verbose"], "expected one argument", id="flag-valued-issue-417"),
    pytest.param(["--config="], "requires a path", id="empty-attached-issue-417"),
    pytest.param(["--config=--verbose"], "is a flag, not a path", id="flag-valued-attached-issue-417"),
])
def test_a_config_argument_without_a_path_is_a_usage_error(argv, says, monkeypatch, capsys):
    """Each spelling failed its own way before the fix: `--config` as the last
    argument raised IndexError, `--config --verbose` watched the file
    "--verbose" (zero hosts, no complaint), and the attached spellings watched
    the default file -- the silent half of issue #417. A missing or flag-like
    value is now a usage error on stderr, exit 2, before any watching starts."""
    m = load()
    monkeypatch.setattr(m, "time", _ScriptedTime(
        m.time, None, lambda _: pytest.fail("a usage error exits before the loop")))
    with pytest.raises(SystemExit) as done:
        m.main(argv)
    assert done.value.code == 2
    assert says in capsys.readouterr().err


def test_the_usage_line_survives_an_interpreter_without_docstrings(monkeypatch, capsys):
    """argparse's description read the module docstring, so under -OO
    (PYTHONOPTIMIZE=2, which strips __doc__) main() raised AttributeError at
    startup -- before it watched anything, and the LaunchAgent's KeepAlive
    would loop on that. --help still names the program and exits 0."""
    m = load()
    monkeypatch.setattr(m, "__doc__", None)
    with pytest.raises(SystemExit) as done:
        m.main(["--help"])
    assert done.value.code == 0
    assert "usage: ccmorph-rc-hosts" in capsys.readouterr().out


class DyingProc(FakeProc):
    """A proc whose bridge already exited: stop() may wait() on it."""

    def wait(self, *_a):
        return 0


def idle_sleep(m, clock, done, checks=2):
    """A sleep for run_main that puts the idle check due again every tick and
    ends the run once `done` (a list the idle path appends to) holds `checks`
    entries: a green run means main() outlived that many idle checks."""
    def sleep(_):
        clock[0] += m.IDLE_CHECK            # every sleep puts the idle check due again
        if len(done) >= checks:
            raise SystemExit                # the supervisor outlived the idle seam
    return sleep


def test_a_dead_host_is_relaunched_with_growing_backoff_reset_when_healthy(tmp_path, monkeypatch):
    m = load()
    h = m.Host(str(tmp_path), "profile")
    starts = []
    monkeypatch.setattr(h, "start", lambda: (starts.append(1), setattr(h, "proc", FakeProc()),
                                             setattr(h, "started", t[0])))
    t = [1000.0]
    h.tend(t[0]); assert len(starts) == 1
    h.proc.rc = 1; t[0] += 5; h.tend(t[0])             # died fast: back off
    assert h.proc is None and h.backoff == 2 * m.BACKOFF_MIN
    h.tend(t[0] + 1); assert len(starts) == 1           # still waiting
    t[0] += h.backoff; h.tend(t[0]); assert len(starts) == 2
    h.proc.rc = 1; t[0] += m.HEALTHY_AFTER; h.tend(t[0])  # ran long: back off resets
    assert h.backoff == m.BACKOFF_MIN


def test_a_bridge_that_quits_cleanly_after_an_outage_is_relaunched_at_once(tmp_path, monkeypatch):
    """GH-461: three hosts came out of the 5h12m DNS outage of 2026-10-22 with
    `Fatal bridge error: Poll: Access denied (403)` and exited rc=0 after 48154s
    up. A clean exit is still an exit, and the host must be relaunched at the
    ordinary minimum backoff: only the log file says why it went, and a bridge the
    supervisor is waiting on must never leave the folder unserved until someone
    notices."""
    m = load()
    h = m.Host(str(tmp_path), "profile")
    starts, t = [], [1000.0]
    monkeypatch.setattr(h, "start", lambda: (starts.append(1), setattr(h, "proc", FakeProc()),
                                             setattr(h, "started", t[0])))
    h.tend(t[0]); assert len(starts) == 1
    h.proc.rc = 0                       # quit by itself after a long, healthy run
    t[0] += 48154
    h.tend(t[0])
    assert h.proc is None and h.backoff == m.BACKOFF_MIN
    h.tend(t[0] + m.BACKOFF_MIN - 1); assert len(starts) == 1    # not before the backoff
    h.tend(t[0] + m.BACKOFF_MIN); assert len(starts) == 2        # back, at the outer bound


def test_host_stdin_stays_open(tmp_path, monkeypatch):
    """EOF on script(1)'s stdin becomes a ^D keystroke that quits remote-control."""
    m = load()
    seen = {}
    class P:
        pid = 1
        def poll(self): return None
    def popen(cmd, **kw):
        seen.update(kw); return P()
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", popen)
    monkeypatch.setattr(m, "LOGS", tmp_path)
    m.Host(str(tmp_path), "profile").start()
    assert seen["stdin"] is m.subprocess.PIPE


def test_a_started_host_logs_nowhere_under_the_users_config(tmp_path, monkeypatch):
    """Every host log used to land in ~/.config/ccmorph (issue #179): 505 pytest
    logs sat in the live .rc-bridge directory. The user's config dir is simulated
    with HOME/XDG_CONFIG_HOME; CCMORPH_STATE_DIR stays whatever the test run
    provides, so the isolation itself is what this asserts."""
    home = tmp_path / "home"
    home.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("XDG_CONFIG_HOME", str(home / ".config"))
    m = load()
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    (tmp_path / "repo").mkdir()
    m.Host(str(tmp_path / "repo"), "gw").start()
    # parents, not Path.is_relative_to: that is 3.9+, and CI runs this suite on 3.8.
    assert tmp_path in m.LOGS.parents and tmp_path in m.EDGES.parents
    assert not (home / ".config").exists()               # nothing reached the user's config


def test_session_args_reach_ccmorph_rc_after_a_separator(tmp_path, monkeypatch):
    m = load()
    seen = {}
    class P:
        pid = 1
        def poll(self): return None
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: seen.setdefault("cmd", cmd) and P())
    monkeypatch.setattr(m, "LOGS", tmp_path)
    m.Host(str(tmp_path), "profile", ("--session-id", "session_01X")).start()
    assert seen["cmd"][-5:-1] == ["profile", "--", "--session-id", "session_01X"]
    assert seen["cmd"][-1].startswith("--debug-file=")


def test_hosts_start_one_at_a_time(tmp_path, monkeypatch):
    m = load()
    hs = [m.Host(str(tmp_path / str(i)), "profile") for i in range(3)]
    for h in hs:
        monkeypatch.setattr(h, "start", (lambda h=h: setattr(h, "proc", FakeProc())))
    launched = False
    for h in hs:
        launched |= h.tend(1000.0, may_start=not launched)
    assert [h.proc is not None for h in hs] == [True, False, False]


def test_tend_on_a_running_host_returns_false_not_none(tmp_path):
    m = load()
    h = m.Host(str(tmp_path), "profile")
    h.proc, h.started = FakeProc(), 1000.0
    assert h.tend(1001.0) is False


def _git_repo(path):
    import subprocess
    path.mkdir(parents=True)
    run = lambda *a: subprocess.run(["git", "-C", str(path), *a], check=True, capture_output=True, timeout=HANG_TIMEOUT)
    run("init", "-q"); run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
    return path


def test_a_session_that_is_gone_server_side_retires_instead_of_retrying(tmp_path, monkeypatch):
    """An archived conversation 409s; a deleted or expired one 404s. Both are
    terminal: the bridge must stop, not retry every backoff interval forever."""
    m = load()
    expired = "Error: Session session_01GONE not found. It may have been archived or expired."
    assert m.ARCHIVED_SIGNAL.search(expired)
    assert m.ARCHIVED_SIGNAL.search("Session not active (409): the session was archived or deleted.")
    assert not m.ARCHIVED_SIGNAL.search("Could not reach the server to look up session session_01X")
    monkeypatch.setattr(m, "LOGS", tmp_path)
    h = m.Host("/x/bridge", "testprofile", ("--session-id", "session_01GONE"))
    h.logfile.write_text(expired)
    assert h.archived()                       # the plain host log carries the verdict


def test_a_dead_repo_host_reattaches_nothing_and_an_archived_conversation_stays_down(tmp_path, monkeypatch):
    """Archiving in the app ends the bridge with rc=0 like any failure; restarting it
    would un-archive the conversation. Only the debug log tells them apart."""
    m = load()
    repo, a, b = tmp_path / "project", tmp_path / "a", tmp_path / "b"
    for d in (repo, a, b):
        d.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\n{a} -- --session-id session_01AAA\n"
                   f"{b} -- --session-id session_01BBB\n")
    clock = [1_000_000.0]
    procs, starts = {}, []
    def start(self):
        self.proc, self.started = FakeProc(), clock[0]
        procs[self.folder] = self
        starts.append(self.folder)
    script = []
    def sleep(_):
        clock[0] += 5
        if not script:
            raise SystemExit
        script.pop(0)()
    idle = lambda: None
    def exits():
        for f in (repo, a, b):
            procs[str(f)].proc.rc = 0
        archived = procs[str(a)].debugfile
        archived.with_name(archived.stem + "-cse_01AAA.log").write_text(
            "CCRClient: Epoch mismatch (409, reason=session_not_active), shutting down")
    script += [idle] * 4 + [exits] + [idle] * 30
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep, clock=clock)
    assert {h.args for h in procs.values()} == {(), ("--session-id", "session_01AAA"),
                                                 ("--session-id", "session_01BBB")}
    assert starts.count(str(a)) == 1                 # archived: never restarted
    assert starts.count(str(b)) == 2                 # plain failure: restarted
    assert starts.count(str(repo)) == 2
    assert "archived=" in cfg.read_text() and f"{b} -- --session-id session_01BBB\n" in cfg.read_text()


def test_a_transcript_that_vanishes_mid_idle_check_keeps_the_supervisor_up(
        tmp_path, monkeypatch, capsys):
    """GH-380: last_activity() stat'ed every transcript of the folder with no
    guard, and Claude Code rotates and deletes transcripts while the supervisor
    runs. A transcript going between the glob and the stat raised
    FileNotFoundError out of main()'s idle check and stopped supervision -- the
    #340 class on the transcripts path. Unreadable activity is unknown, not
    idle, so the line waits for the next check instead of being retired."""
    m = load()
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))
    folder = tmp_path / "bridge"
    folder.mkdir()
    f = m.resolve(str(folder))
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\nidle_days=1\n{f} -- --session-id session_01X\n")
    m.edit_config(cfg, f, loaded=cfg.read_text())   # archived in the app: dormant, no host starts
    proj = m.project_dirs(f)[0]
    proj.mkdir(parents=True)
    transcript = proj / "session_01X.jsonl"
    transcript.write_text("{}\n")
    m.os.utime(transcript, (1_000_000.0, 1_000_000.0))   # 3 days before `now`: past the limit
    clock = [1_000_000.0 + 3 * 86400]
    monkeypatch.setattr(m, "cycle", lambda hosts, repos, now, c: True)   # idle check due every tick
    monkeypatch.setattr(m, "clean_archived", lambda *a: None)
    retired = []
    def delete(h):
        retired.append(h.session_id)
        return True
    monkeypatch.setattr(m, "delete_host", delete)
    faults, real_stat = [1], m.Path.stat
    def stat(self, *a, **k):
        if self == transcript and faults:       # the file went between glob and stat
            faults.pop()
            raise FileNotFoundError(2, "transcript vanished between glob and stat")
        return real_stat(self, *a, **k)
    monkeypatch.setattr(m.Path, "stat", stat)
    snap = []
    def sleep(_):
        snap.append((list(retired), cfg.read_text()))
        if len(snap) > 2:                      # the supervisor outlived the idle seam
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, lambda self: pytest.fail("the line is archived"),
             sleep, clock=clock)
    assert not faults                            # the idle check hit the fault, not something else
    assert "transcript vanished" in capsys.readouterr().out
    assert snap[0][0] == []                      # unknown activity: not retired on this check
    assert f"{f} -- --session-id session_01X" in snap[0][1]   # left for the next check
    assert snap[1][0] == ["session_01X"]         # readable again: retired on the next check
    assert retired == ["session_01X"] and "# idle-retired" in cfg.read_text()


def test_a_transient_listing_failure_defers_the_retire_instead_of_losing_the_row(
        tmp_path, monkeypatch, capsys):
    """GH-450: delete_host finds the row to delete by listing the account's
    archived sessions, and that listing can fail transiently (a 503, a cut-off
    reply). Retiring the pin anyway would leave its host row behind with no pin
    left to name it -- undeletable for good. The retire waits for the listing,
    like every other unknown here: this check leaves the pin, the next one,
    with the server answering, deletes the row and retires the line."""
    m = load()
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))
    folder = tmp_path / "bridge"
    folder.mkdir()
    f = m.resolve(str(folder))
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\nidle_days=1\n{f} -- --session-id session_01X\n")
    m.edit_config(cfg, f, loaded=cfg.read_text())   # archived in the app: dormant, no host starts
    proj = m.project_dirs(f)[0]
    proj.mkdir(parents=True)
    transcript = proj / "session_01X.jsonl"
    transcript.write_text("{}\n")
    m.os.utime(transcript, (1_000_000.0, 1_000_000.0))   # 3 days before `now`: past the limit
    clock = [1_000_000.0 + 3 * 86400]
    monkeypatch.setattr(m, "cycle", lambda hosts, repos, now, c: True)   # idle check due every tick
    monkeypatch.setattr(m, "clean_archived", lambda *a: None)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    faults, calls = [1], []

    def api(method, path, token, body=None):
        if faults and path.startswith("/v1/code/sessions"):
            faults.pop()
            raise m.urllib.error.HTTPError(path, 503, "Service Unavailable", None, None)
        if method == "DELETE":
            calls.append((method, path))
            return {}
        return {"data": [{"id": "cse_01X", "environment_id": "env_01OLD"}]}

    monkeypatch.setattr(m, "api", api)
    snap = []

    def sleep(_):
        snap.append((list(calls), cfg.read_text()))
        if len(snap) > 2:                      # the supervisor outlived the idle seam
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, lambda self: pytest.fail("the line is archived"),
             sleep, clock=clock)
    assert not faults                            # the fault was hit, not dodged
    assert "host not deleted" in capsys.readouterr().out
    assert snap[0][0] == []                      # nothing deleted under the failed listing
    assert "# idle-retired" not in snap[0][1]    # the pin waited for the listing, not retired
    assert snap[1][0] == [("DELETE", "/v1/environments/bridge/env_01OLD")]
    assert "# idle-retired" in cfg.read_text()


def test_the_idle_pass_retires_one_pin_of_a_folder_and_keeps_the_sibling_hosted(
        tmp_path, monkeypatch, capsys):
    """GH-449: the idle pass snapshotted the archived set once and walked every
    pin of a folder in it, but a retire drops the folder's archived marker, so
    from the folder's second pin on the gate read a set the file no longer held:
    both pins of a two-pin folder were retired in one tick and parse() hosted
    nothing for it -- the #415 harm, reached through the supervisor. The gate
    reads what the retire left, so the sibling line survives and parse() hosts
    it."""
    m = load()
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))
    folder = tmp_path / "bridge"
    folder.mkdir()
    f = m.resolve(str(folder))
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\nidle_days=1\n{f} -- --session-id session_01X\n"
                   f"{f} -- --session-id session_01XB\n")
    m.edit_config(cfg, f, loaded=cfg.read_text())   # archived in the app: dormant, no host starts
    proj = m.project_dirs(f)[0]
    proj.mkdir(parents=True)
    transcript = proj / "session_01X.jsonl"
    transcript.write_text("{}\n")
    m.os.utime(transcript, (1_000_000.0, 1_000_000.0))   # 3 days before `now`: past the limit
    clock = [1_000_000.0 + 3 * 86400]
    monkeypatch.setattr(m, "cycle", lambda hosts, repos, now, c: True)   # idle check due every tick
    monkeypatch.setattr(m, "clean_archived", lambda *a: None)
    deleted = []
    def delete(h):
        deleted.append(h.session_id)
        return True
    monkeypatch.setattr(m, "delete_host", delete)
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] > 2:                      # the supervisor outlived the idle seam
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, lambda self: pytest.fail("the line is archived"),
             sleep, clock=clock)
    text = cfg.read_text()
    assert deleted == ["session_01X"]         # one host row deleted, not the sibling's too
    assert text.count("# idle-retired") == 1  # one line retired, not the sibling's too
    assert m.parse(text) == {f: ("profile", ("--session-id", "session_01XB"))}
    assert capsys.readouterr().out.count("archived and idle") == 1


def test_archived_line_is_dormant_and_cleaned_only_after_idle_limit(tmp_path):
    """An archived pin makes its whole folder dormant, and retiring it cleans only
    its own line. One folder's pin ids share prefixes, and a substring match over
    the raw line retired a sibling pin too, so parse() hosted nothing for the
    folder until the line was restored by hand (issue #415)."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    folder = tmp_path / "bridge"
    folder.mkdir()
    cfg.write_text(f"route=profile\n{folder} -- --session-id session_01X\n"
                   f"{folder} -- --session-id session_01XB\n")
    m.edit_config(cfg, str(folder), loaded=cfg.read_text())
    again = m.edit_config(cfg, str(folder), loaded=cfg.read_text())   # idempotent
    # #355: a marker already in the file is a landed edit -- the text comes back,
    # with no mtime, so a caller deferring its retire can tell "recorded" from
    # "never reached the file" (which is a bare None).
    assert again is not None and again[1] is None and f"archived={folder}" in again[0]
    assert cfg.read_text().count("archived=") == 1
    assert str(folder) not in m.parse(cfg.read_text())  # dormant: never started
    m.edit_config(cfg, str(folder), "session_01X", loaded=cfg.read_text())
    text = cfg.read_text()
    assert "# idle-retired" in text and "archived=" not in text
    assert text.count("# idle-retired") == 1            # its own line, not the sibling's
    assert m.parse(text) == {str(folder): ("profile", ("--session-id", "session_01XB"))}


@pytest.mark.parametrize("marker", ["archived={folder}", "archived= {folder}  # note"],
                         ids=["plain", "space-and-comment"])
def test_the_retired_pin_takes_its_archived_marker_with_it(tmp_path, marker):
    """GH-847 review: archived_set() reads `archived= <folder> # note` as the
    folder under the one line rule, but edit_config() matched the marker with
    `line[9:]` -- the space included -- resolving " <folder>", so the marker
    stayed after the pin it retired was dropped. The folder and its sibling pin
    stayed dormant under a marker that should have gone. Reader and writer now
    extract the value the same way."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    folder = tmp_path / "bridge"
    folder.mkdir()
    cfg.write_text(f"route=profile\n{marker.format(folder=folder)}\n"
                   f"{folder} -- --session-id session_01X\n"
                   f"{folder} -- --session-id session_01XB\n")
    assert m.parse(cfg.read_text()) == {}           # the marker makes the folder dormant
    m.edit_config(cfg, str(folder), "session_01X", loaded=cfg.read_text())
    text = cfg.read_text()
    assert "archived=" not in text                  # its marker went with its pin
    assert text.count("# idle-retired") == 1        # its own line, not the sibling's
    assert m.parse(text) == {str(folder): ("profile", ("--session-id", "session_01XB"))}


def test_an_edit_that_cannot_be_statd_back_comes_back_without_a_stamp(tmp_path, monkeypatch, capsys):
    """#355: the marker reached the file but nothing could be committed for it --
    the post-write stat failed. The caller reads that as landed-and-unacked (the
    text, no stamp), not as a failed edit, which is a bare None: called failed,
    the pinned line would be held active-looking and the retire retried against
    a file that already carries the marker."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    folder = tmp_path / "bridge"
    cfg.write_text(f"route=profile\n{folder} -- --session-id session_01X\n")
    monkeypatch.setattr(m, "mtime_of", lambda path: None)   # written, not observable
    after = m.edit_config(cfg, str(folder), loaded=cfg.read_text())
    assert after is not None and after[1] is None
    assert f"archived={folder}" in after[0] and f"archived={folder}" in cfg.read_text()
    assert "edited but not stat'd afterwards" in capsys.readouterr().out


def test_edit_config_leaves_an_unresolvable_marker_alone_and_retires_the_rest(tmp_path):
    """GH-414: edit_config re-resolves config lines to find the one to retire,
    from main()'s idle cleanup; an unresolvable path raised there too. The bad
    marker is left alone (it matches no folder) and the good line is still
    retired."""
    m = load()
    folder = tmp_path / "bridge"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"archived=a\x00b\n{folder} -- --session-id session_01X\n")
    m.edit_config(cfg, str(folder), "session_01X", loaded=cfg.read_text())
    text = cfg.read_text()
    assert "# idle-retired" in text and "archived=a\x00b" in text


def test_the_shared_edge_starts_first_and_bridges_wait_for_it(tmp_path, monkeypatch):
    m = load()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{tmp_path}/a\n{tmp_path}/b -- --session-id session_01X\n")
    (tmp_path / "a").mkdir(); (tmp_path / "b").mkdir()
    order, ready = [], [False]
    def start(self):
        self.proc, self.started = FakeProc(), 0.0
        order.append("edge" if self.edge else self.folder)
        if self.edge:
            ready[0] = True
    run_main(m, monkeypatch, tmp_path, cfg, start, edge_ready=lambda route: ready[0])
    assert order[0] == "edge" and order.count("edge") == 1   # one edge for the one route
    assert len(order) == 3                                   # then both bridges


def test_a_down_edge_is_probed_once_per_tick_not_once_per_folder(tmp_path, monkeypatch):
    """#381: each probe can block a second; N folders on a down edge stalled every tick N s."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text("route=profile\n" + "".join(f"{tmp_path}/{d}\n" for d in "abc"))
    for d in "abc":
        (tmp_path / d).mkdir()
    probes = []
    def start(self):
        self.proc, self.started = FakeProc(), 0.0
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] > 4:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep,
             edge_ready=lambda route: probes.append(route) or False)
    assert probes and len(probes) <= ticks[0]


@pytest.mark.parametrize("before, after, edge, folders", [
    ("permission=bypassPermissions", "permission=default", False, True),
    ("permission=bypassPermissions", "", False, True),
    ("", "permission=bypassPermissions", False, True),
    ("permission=bypassPermissions", "permission=bypassPermissions", False, False),
    ("tier.fable=old-model", "tier.fable=new-model", True, True),
    ("tier.fable=old-model", "", True, True),
    ("", "tier.fable=new-model", True, True),
    ("tier.fable=old-model", "tier.fable=old-model", False, False),
    ("tier.fabl=old-model", "tier.fabl=new-model", False, False),
], ids=["permission-changed-issue-339", "permission-removed-issue-339", "permission-added-issue-339",
        "permission-kept-issue-339", "tier-changed-issue-339", "tier-removed-issue-339",
        "tier-added-issue-339", "tier-kept-issue-339", "alias-no-launch-reads-issue-339"])
def test_permission_and_tier_changes_restart_the_hosts_that_carry_them(
        tmp_path, monkeypatch, before, after, edge, folders):
    """Both settings are fixed when the process starts, so a reload that only
    rewrites them in memory leaves a running host on the old ones -- a
    bypassPermissions downgrade would not apply until some later crash.
    A host carrying an unchanged setting stays up (issue #339)."""
    m = load()
    a, b = tmp_path / "a", tmp_path / "b"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{before}\n{a}\n{b}\n")
    monkeypatch.setattr(m, "cycle", lambda *args: False)
    starts, stops = [], []
    def start(self):
        starts.append((self.edge, self.folder, self.permission, dict(self.tiers)))
        self.proc, self.started = FakeProc(), m.time.time()
    monkeypatch.setattr(m.Host, "stop", lambda self: stops.append((self.edge, self.folder)))
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 4:                          # past the first three launches
            cfg.write_text(f"# reloaded\nroute=gateway\n{after}\n{a}\n{b}\n")
            m.os.utime(cfg, (cfg.stat().st_mtime + 2,) * 2)
        if ticks[0] == 9:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep)

    shared, one, two = (True, str(m.EDGES.parent)), (False, str(a)), (False, str(b))
    assert [s[1] for s in starts[:3]] == [shared[1], one[1], two[1]]   # the edge, then the folders
    assert stops == ([shared] if edge else []) + ([one, two] if folders else [])
    reloaded = f"# reloaded\nroute=gateway\n{after}\n{a}\n{b}\n"
    restarted = starts[3:]
    assert [s[1] for s in restarted] == [h[1] for h in stops]
    for is_edge, _, permission, tiers in restarted:
        assert permission == (None if is_edge else m.permission_mode(reloaded))
        assert tiers == m.tier_map(reloaded)


def test_a_folder_with_a_live_host_is_not_duplicated(tmp_path, monkeypatch):
    """A second host for one folder registers a second environment; the first is
    orphaned and its row can never be removed from the Claude app."""
    m = load()
    h = m.Host(str(tmp_path), "gateway")
    # live_pointer_pid reads under Path.home(); point it at tmp_path so the fixture
    # does not touch the real ~/.claude/projects. (Subclassing Path breaks on 3.8.)
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))
    proj = tmp_path / ".claude" / "projects" / m.re.sub(r"[^A-Za-z0-9]", "-", str(tmp_path))
    proj.mkdir(parents=True)
    me = m.os.getppid()                          # alive, and not our own pid
    proj.joinpath("bridge-pointer.json").write_text(m.json.dumps({"pid": me}))
    assert h.live_pointer_pid() == me
    started = []
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda *a, **k: started.append(1) or FakeProc())
    h.start()
    assert not started and h.proc is None        # refused to duplicate
    proj.joinpath("bridge-pointer.json").write_text(m.json.dumps({"pid": 999999}))
    assert h.live_pointer_pid() is None          # dead pid: folder is free
    h.start()
    assert started                              # now it may start


def test_a_host_running_as_a_profile_is_found_in_that_profiles_history(tmp_path, monkeypatch):
    """History is per profile: a host started as an account profile writes its
    bridge pointer and transcripts under that profile's projects/, not native."""
    m = load()
    monkeypatch.setattr(m.Path, "home", staticmethod(lambda: tmp_path))
    monkeypatch.setattr(m, "STATE_ROOT", tmp_path / "state")
    h = m.Host(str(tmp_path), "gateway")
    proj = tmp_path / "state" / "profiles" / "acct" / "projects" / m.re.sub(r"[^A-Za-z0-9]", "-", str(tmp_path))
    proj.mkdir(parents=True)
    me = m.os.getppid()
    proj.joinpath("bridge-pointer.json").write_text(m.json.dumps({"pid": me}))
    assert h.live_pointer_pid() == me
    t = proj / "s.jsonl"
    t.write_text("{}\n")
    m.os.utime(t, (5.0, 5.0))
    assert m.last_activity(str(tmp_path)) == 5.0


def test_a_host_that_released_its_environment_is_reported(tmp_path, monkeypatch):
    m = load()
    h = m.Host(str(tmp_path), "gateway")
    h.logfile = tmp_path / "x.log"
    h.logfile.write_text("starting...\n")
    assert not h.deregistered(0)
    h.logfile.write_text("[bridge:shutdown] Environment deregistered, bridge offline\n")
    assert h.deregistered(0)
    h.logfile.write_text("[bridge:shutdown] Environment marked offline\n")
    assert h.deregistered(0)                      # resume handle, also a release


def test_tier_alias_points_one_alias_at_the_route_model(tmp_path, monkeypatch):
    """gateway serves no claude-fable model, so `fable` needs pointing at what it
    does serve -- from the config, and only that alias."""
    m = load()
    text = "route=gateway\ntier.fable=gpt-6-astra\n{a} -- --session-id session_01X\n"
    assert m.tier_map(text) == {"fable": "gpt-6-astra"}
    assert m.parse(text.format(a=tmp_path)) == {str(tmp_path): ("gateway", ("--session-id", "session_01X"))}
    assert m.tier_map("route=gateway\n") == {}          # untouched config: identity


# The live llmgate route profile and the rc-hosts tier lines on 2026-11-22.
LLMGATE_TIERS = {"fable": "gpt-6-astra", "opus": "claude-opus-5-5",
                 "sonnet": "claude-sonnet-5-5", "haiku": "deepseek-v4.1-flash"}
LIVE_TIER_LINES = ("route=llmgate\ntier.fable=gpt-6-astra\ntier.haiku=deepseek-v4.1-flash\n"
                   "permission=bypassPermissions\nidle_days=1\npurge_days=7\n")


def test_tiers_come_from_the_route_profile_and_lines_override():
    """The route profile is the one tier map: only an alias it maps outside its
    own Claude family is pinned (opus/sonnet stay native 1:1), and a `tier.` line
    overrides one alias. Today's config computes the map the hosts already run,
    with or without its now-duplicate lines, so landing this restarts no host."""
    m = load()
    want = {"fable": "gpt-6-astra", "haiku": "deepseek-v4.1-flash"}
    assert m.tier_map(LIVE_TIER_LINES, LLMGATE_TIERS) == want
    assert m.tier_map(LIVE_TIER_LINES) == want                    # the lines the hosts started on
    assert m.tier_map("route=llmgate\n", LLMGATE_TIERS) == want   # duplicate lines removed
    assert m.tier_map("tier.haiku=other\n", LLMGATE_TIERS)["haiku"] == "other"   # a line wins
    assert m.tier_map("", {"opus": "claude-opus-5-5[1m]"}) == {}  # same family, any spelling


def test_a_profile_tier_reaches_a_host_without_a_tier_line(tmp_path, monkeypatch):
    """2026-11-22: rc-hosts pinned only fable, so every phone session sent haiku as
    the native claude-haiku-5-5, which the gateway does not serve -- HTTP 400 on
    every Explore agent. The route profile's haiku must reach the host's env."""
    m = load()
    monkeypatch.delenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", raising=False)
    monkeypatch.delenv("ANTHROPIC_DEFAULT_OPUS_MODEL", raising=False)
    a = tmp_path / "a"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\ntier.fable=gpt-6-astra\n{a}\n")
    monkeypatch.setattr(m, "cycle", lambda *args: False)
    envs = {}
    def start(self):
        envs[self.folder] = self.child_env()
        self.proc, self.started = FakeProc(), m.time.time()
    asked = []
    run_main(m, monkeypatch, tmp_path, cfg, start,
             profile_tiers=lambda route: asked.append(route) or dict(LLMGATE_TIERS))
    assert asked and set(asked) == {"gateway"}
    env = envs[str(a)]
    assert env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] == "deepseek-v4.1-flash"
    assert env["ANTHROPIC_DEFAULT_FABLE_MODEL"] == "gpt-6-astra"
    assert "ANTHROPIC_DEFAULT_OPUS_MODEL" not in env              # same family: native 1:1
    assert envs[str(m.EDGES.parent)]["ANTHROPIC_DEFAULT_HAIKU_MODEL"] == "deepseek-v4.1-flash"


def test_a_tier_set_on_the_route_profile_restarts_the_hosts_with_no_config_edit(tmp_path, monkeypatch):
    """2026-11-24: Stark's phone control sets a tier in the route profile's file, but
    the supervisor reloaded only when rc-hosts changed, so the phone's pick reached no
    host until an unrelated edit. The profile's file is watched too."""
    m = load()
    monkeypatch.setattr(m, "STATE_ROOT", tmp_path)
    monkeypatch.delenv("ANTHROPIC_DEFAULT_OPUS_MODEL", raising=False)
    a = tmp_path / "a"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{a}\n")
    conf = tmp_path / "gateway.conf"
    conf.write_text("")
    monkeypatch.setattr(m, "cycle", lambda *args: False)
    monkeypatch.setattr(m.Host, "stop", lambda self: None)
    tiers = dict(LLMGATE_TIERS)
    opus = []
    def start(self):
        if self.folder == str(a):
            opus.append(self.child_env().get("ANTHROPIC_DEFAULT_OPUS_MODEL"))
        self.proc, self.started = FakeProc(), m.time.time()
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 2:
            tiers["opus"] = "gemini-3.5-pro"
            os.utime(conf, (1, 1))
        if ticks[0] > 4:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, profile_tiers=lambda route: dict(tiers))
    assert opus == [None, "gemini-3.5-pro"]


def test_a_failed_profile_read_keeps_the_last_map_and_logs_once(monkeypatch):
    """A read that fails must not change a host's tiers (that would restart it):
    the last map read stays, the `tier.` lines alone before any read worked, and
    the failure is one log line, not one per reload."""
    m = load()
    logged, answers = [], [None, dict(LLMGATE_TIERS), None, None]
    monkeypatch.setattr(m, "log", logged.append)
    monkeypatch.setattr(m, "read_profile_tiers", lambda route: answers.pop(0))
    lines = "tier.fable=gpt-6-astra\n"
    assert m.route_tiers("gw", lines) == {"fable": "gpt-6-astra"}  # never read: the lines alone
    full = {"fable": "gpt-6-astra", "haiku": "deepseek-v4.1-flash"}
    assert m.route_tiers("gw", lines) == full
    assert m.route_tiers("gw", lines) == full                      # failed: the last map read
    assert m.route_tiers("gw", lines) == full
    assert len(logged) == 2                                        # one per failure episode


def test_the_profile_tiers_are_read_from_ccmorph_get(monkeypatch):
    """The owner answers, never the .conf: `ccmorph-core cli get <route> --json`."""
    m = load()
    seen = []
    def run(cmd, **kw):
        seen.append(cmd)
        return SimpleNamespace(returncode=0, stdout='{"profile": "gw", "tiers": {"haiku": "x"}}')
    monkeypatch.setattr(m.subprocess, "run", run)
    assert m.read_profile_tiers("gw") == {"haiku": "x"}
    assert seen[0][1:] == ["cli", "get", "gw", "--json"]
    monkeypatch.setattr(m.subprocess, "run", lambda cmd, **kw: SimpleNamespace(returncode=1, stdout=""))
    assert m.read_profile_tiers("gw") is None


def test_hosted_record_is_the_routes_the_tier_overrides_and_its_writer_gh_881_926():
    """GH-881: what the supervisor alone knows -- the routes its parse() hosts
    and the `tier.` pins -- in one record `ccmorph doctor` reads, so no other
    reader re-parses rc-hosts. Routes are sorted; only the aliases a host
    exports survive; the one line rule (cut at `#`, strip) reads `tier. haiku = y`.

    GH-926: the record also names its writer -- the supervisor's pid and the
    kernel's mark of when it started -- because it outlives a supervisor killed
    past its clean stop, and doctor may believe a hosted claim only while that
    exact process runs. Both halves are asserted: a pid alone is the number the
    OS recycles (#206), and no later change may silently drop either."""
    m = load()
    stamp = {"pid": os.getpid(), "started": rc_start_mark(os.getpid())}
    assert stamp["started"], "the test host must read its own start mark"
    assert m.hosted_record({"other", "demo"},
                           "tier.opus=gpt-6-astra\ntier.bogus=x\ntier. haiku = y # note\n") == {
        "routes": ["demo", "other"], "overrides": {"opus": "gpt-6-astra", "haiku": "y"},
        "supervisor": stamp}
    assert m.hosted_record(set(), "") == {
        "routes": [], "overrides": {}, "supervisor": stamp}


def test_main_publishes_the_routes_it_hosts_at_every_config_load_gh_881(tmp_path, monkeypatch):
    """The publisher's real test: a load with a case-twin route (refused, so it
    starts no edge and hosts no folder) and a NBSP-ended own route publishes
    exactly what the supervisor hosts; a config edit republishes on the next
    load; a stop that was not clean leaves the record for the next doctor read."""
    m = load()
    a, b, c, d = (tmp_path / n for n in "abcd")
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw\n{a}\n{d}\n{c} route=GW\n{b} route=demo  -- -c\n"
                   "tier.opus=gpt-6-astra\n")
    monkeypatch.setattr(m, "cycle", lambda *args: False)
    monkeypatch.setattr(m.Host, "stop", lambda self: None)

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()

    seen = []
    ticks = [0]

    def sleep(_):
        ticks[0] += 1
        seen.append(json.loads(m.HOSTED.read_text()))
        if ticks[0] == 2:
            cfg.write_text(f"route=other\n{a} -- route=demo\n")
            os.utime(cfg, (1, 1))
        if ticks[0] > 2:
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep)
    stamp = {"pid": os.getpid(), "started": rc_start_mark(os.getpid())}
    assert seen[0] == {"routes": ["demo", "gw"], "overrides": {"opus": "gpt-6-astra"},
                       "supervisor": stamp}
    assert seen[-1] == {"routes": ["other"], "overrides": {},
                        "supervisor": stamp}   # the forwarded route= hosts nothing
    assert m.HOSTED.exists()                                    # a stop that was not clean leaves it


def test_a_publish_that_cannot_land_is_logged_once_and_retried_each_tick_gh_881(
        tmp_path, monkeypatch, capsys):
    """A blocked record directory must not end the tick or repeat the complaint
    every second: one log line per failure episode, the tick goes on, the record
    lands on the first tick after the block clears, and no temp file is left."""
    m = load()
    blocker = tmp_path / "blocker"
    blocker.write_text("")              # a regular file where the record's directory belongs
    monkeypatch.setattr(m, "HOSTED", blocker / "hosted.json")
    a = tmp_path / "a"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw\n{a}\n")
    monkeypatch.setattr(m, "cycle", lambda *args: False)
    monkeypatch.setattr(m.Host, "stop", lambda self: None)

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()

    ticks = [0]

    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 3:
            blocker.unlink()            # the block clears; the next tick's retry lands
        if ticks[0] > 4:
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep)
    assert capsys.readouterr().out.count("cannot publish") == 1
    assert json.loads(m.HOSTED.read_text()) == {
        "routes": ["gw"], "overrides": {},
        "supervisor": {"pid": os.getpid(), "started": rc_start_mark(os.getpid())}}
    assert sorted(p.name for p in m.HOSTED.parent.iterdir()) == ["hosted.json"]


def test_a_clean_stop_takes_the_published_record_with_it_gh_881(tmp_path, monkeypatch):
    """SIGTERM ends the loop at the top, the hosts stop, and the record goes: a
    supervisor that is gone must not speak for a profile in doctor's report."""
    m = load()
    a = tmp_path / "a"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw\n{a}\n")
    monkeypatch.setattr(m, "cycle", lambda *args: False)
    monkeypatch.setattr(m.Host, "stop", lambda self: None)

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()

    published = []

    def sleep(_):
        published.append(m.HOSTED.exists())
        m.signal.getsignal(m.signal.SIGTERM)(m.signal.SIGTERM, None)

    assert run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, exits=False) == 0
    assert published == [True]
    assert not m.HOSTED.exists()


@pytest.mark.parametrize("ambient_opus", [None, "parent-pinned-opus"],
                         ids=["absent", "parent-opus-issue-256"])
def test_a_host_carries_its_tier_into_the_child_environment(tmp_path, monkeypatch, ambient_opus):
    """A configured tier overrides the parent's pin; an unnamed tier is inherited
    from the parent unchanged -- and when the parent has none, the child gets none
    (issue #256: the parent session's own pins must not leak into the assertion)."""
    m = load()
    seen = {}
    class P:
        pid = 1
        def poll(self): return None
    monkeypatch.setenv("ANTHROPIC_DEFAULT_FABLE_MODEL", "parent-pinned-fable")
    monkeypatch.delenv("ANTHROPIC_DEFAULT_OPUS_MODEL", raising=False)
    if ambient_opus is not None:
        monkeypatch.setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", ambient_opus)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: seen.update(kw) or P())
    m.Host(str(tmp_path), "gateway", tiers={"fable": "gpt-6-astra"}).start()
    assert seen["env"]["ANTHROPIC_DEFAULT_FABLE_MODEL"] == "gpt-6-astra"   # configured beats the parent
    if ambient_opus is None:
        assert "ANTHROPIC_DEFAULT_OPUS_MODEL" not in seen["env"]   # only named tiers
    else:
        assert seen["env"]["ANTHROPIC_DEFAULT_OPUS_MODEL"] == ambient_opus   # unnamed: inherited unchanged


def test_permission_mode_reaches_spawned_sessions(tmp_path, monkeypatch):
    """Remote Control defaults to prompting, which on a phone means approving every
    step from the device; the mode is set from the config for all hosts."""
    m = load()
    text = "route=gateway\npermission=bypassPermissions\n{a}\n"
    assert m.permission_mode(text) == "bypassPermissions"
    assert m.parse(text.format(a=tmp_path)) == {str(tmp_path): ("gateway", ())}
    assert m.permission_mode("route=gateway\n") is None        # unset: Remote Control's own default
    assert m.permission_mode("permission=nonsense\n") is None  # only modes the CLI accepts
    seen = {}
    class P:
        pid = 1
        def poll(self): return None
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: seen.update(cmd=cmd) or P())
    m.Host(str(tmp_path), "gateway", permission="bypassPermissions").start()
    assert "--permission-mode=bypassPermissions" in seen["cmd"]


def test_settings_lines_are_not_read_as_folders(tmp_path):
    """Every `key=value` setting must be skipped by parse(); a stray one becomes
    "<home>/<setting>" -- a phantom folder the supervisor then waits on forever."""
    m = load()
    text = (f"route=gateway\nidle_days=3\nretire=archived\narchived=/a\n"
            f"permission=bypassPermissions\ntier.fable=gpt-6-astra\n{tmp_path}\n")
    got = m.parse(text)
    assert len(got) == 1, f"settings leaked into folders: {sorted(got)}"
    assert list(got.values()) == [("gateway", ())]
    assert list(got)[0].endswith(str(tmp_path).split("/")[-1])


def test_an_inference_line_is_a_setting_not_a_ghost_folder(tmp_path):
    """GH-655: `inference=` is documented and read by main(), but parse() decided
    "settings, not folders" from its own tuple, which omitted it. With a global
    route, a config following the README made `<cwd>/inference=oli` a folder the
    supervisor hosted on every reload -- a host that could never launch."""
    m = load()
    assert m.parse(f"route=gw\ninference=oli\n{tmp_path}\n") == {str(tmp_path): ("gw", ())}


def test_a_retired_folder_is_not_hosted(tmp_path):
    """`retire=<folder>` names a folder the operator retires from hosting: its
    line stays in the config, but no host starts for it."""
    m = load()
    other = tmp_path / "other"
    assert m.parse(f"route=gw\nretire={tmp_path}\n{tmp_path}\n") == {}
    assert m.parse(f"route=gw\nretire={tmp_path}\n{other}\n") == {str(other): ("gw", ())}


def test_every_settings_reader_applies_parse_s_line_rule(tmp_path):
    """GH-847: a settings line must mean to its reader what parse() sees, not the
    raw bytes. Read off the raw line, `retire=<folder>#done` retired a folder
    spelled with the note while the one the operator named stayed hosted,
    `inference=oli#x` handed inference() a profile named `oli#x`, and
    `permission= plan` -- a line parse() skips as a setting -- read as no line at
    all, in silence. The `#` cut and the whitespace after `=` are the line rule's,
    and every settings reader applies them alike."""
    m = load()
    folder = str(tmp_path / "retired")
    for line in (f"retire={folder}#done", f"retire={folder}  # done"):
        assert m.retire_set(f"{line}\n") == {folder}, line
        assert m.parse(f"route=gw\n{line}\n{folder}\n") == {}, line
    assert m.inference_name("inference=oli#x\n") == "oli"
    assert m.inference_name("inference=oli # x\n") == "oli"
    assert m.inference_name("inference= oli # x\n") == "oli"   # whitespace after `=` is not the value
    assert m.permission_mode("permission= plan\n") == "plan"
    assert m.permission_mode("permission=plan  # the phone approves nothing\n") == "plan"
    assert m.idle_days("idle_days= 3 # note\n") == 3.0
    assert m.tier_map("route=gw\ntier.opus = m  # note\n") == {"opus": "m"}
    # What parse() hides, no reader sees: a commented-out settings line.
    assert m.retire_set(f"# retire={folder}\n") == set()
    assert m.inference_name("# inference=oli\n") is None
    assert m.permission_mode("# permission=plan\n") is None


def test_a_permission_value_no_mode_names_is_refused_out_loud(capsys):
    """GH-847: `permission=` is skipped by parse() as a setting, so a reader that
    hands back None for a value it read leaves the operator believing a mode is
    in force. A value the CLI does not accept is named on one line."""
    m = load()
    assert m.permission_mode("permission=plan extra\n") is None
    assert "permission=plan extra" in capsys.readouterr().out


def settings_samples(m, tmp_path, *, commented=False):
    """The one line each registry prefix reads. Plain, it is the baseline the
    registry-removal meta test needs: a reader that quietly falls back to the raw
    line still consumes `archived=/path`. Commented, it is how an operator may
    write the same line -- a space after `=` and an inline `#` note -- which the
    line rule must give every reader alike (#847)."""
    note, pad = ("  # note", " ") if commented else ("", "")
    return {"idle_days=": f"{pad}3{note}", "purge_days=": f"{pad}3{note}",
            "park_hours=": f"{pad}3{note}",
            "permission=": f"{pad}plan{note}", "inference=": f"{pad}oli{note}",
            "archived=": f"{pad}{tmp_path / 'archived'}{note}",
            "retire=": f"{pad}{tmp_path / 'retired'}{note}",
            m.TIER_PREFIX: f"opus{pad}={pad}m{note}"}


@pytest.mark.parametrize("commented", [False, True], ids=["plain", "space-and-comment"])
def test_every_settings_prefix_has_a_reader_and_is_skipped(tmp_path, commented):
    """GH-655: the skip tuple and the per-key readers are one registry now, so
    neither can drift: a reader cannot exist without its line being skipped
    (`inference=`, which became a ghost host) and a skipped prefix cannot be
    inert (`retire=`, which had no parser before this). Both written forms count:
    the plain line, and the same line with a space after `=` and a `#` note."""
    m = load()
    samples = settings_samples(m, tmp_path, commented=commented)
    assert set(samples) == set(m.SETTINGS_READERS)
    assert tuple(m.SETTINGS_READERS) == m.SETTINGS_PREFIXES
    for prefix, reader in m.SETTINGS_READERS.items():
        assert callable(reader), prefix
        text = f"route=gw\n{prefix}{samples[prefix]}\n{tmp_path}\n"
        assert list(m.parse(text)) == [str(tmp_path)], f"{prefix} leaked into folders"
        assert reader(text) != reader(f"route=gw\n{tmp_path}\n"), f"{prefix} has no reader"


@pytest.mark.parametrize("commented", [False, True], ids=["plain", "space-and-comment"])
def test_no_reader_reads_without_its_registry_entry(tmp_path, monkeypatch, commented):
    """GH-655 review: SETTINGS_READERS is the source of truth for the readers
    too, not only for parse()'s skip. Each reader finds its own prefix there, so
    deleting an entry silences that reader in the same edit that stops the skip;
    a reader left alive would consume a line parse() now hosts as a folder --
    the `inference=` ghost host. The plain sample is the one a hard-coded raw
    matcher would still consume, so the deletion is checked on both forms."""
    m = load()
    samples = settings_samples(m, tmp_path, commented=commented)
    baseline = f"route=gw\n{tmp_path}\n"
    for prefix, reader in list(m.SETTINGS_READERS.items()):
        with monkeypatch.context() as patch:
            patch.delitem(m.SETTINGS_READERS, prefix)
            assert reader(baseline + f"{prefix}{samples[prefix]}\n") == reader(baseline), prefix


def test_a_folder_listed_for_hosting_is_made_a_repo_before_worktree_mode(tmp_path):
    """Worktree spawn mode is the only mode (capacity 1 reads as a dead host from
    the phone), and it needs a git repo with a HEAD to branch worktrees from."""
    import subprocess
    m = load()
    scratch = tmp_path / "scratch"; scratch.mkdir()
    (scratch / "notes.txt").write_text("keep me")
    m.Host(str(scratch), "testprofile").ensure_repo()
    head = subprocess.run(["git", "-C", str(scratch), "rev-parse", "HEAD"], capture_output=True, timeout=HANG_TIMEOUT)
    assert head.returncode == 0
    assert (scratch / "notes.txt").read_text() == "keep me"   # untracked, untouched
    unborn = tmp_path / "unborn"; unborn.mkdir()
    subprocess.run(["git", "-C", str(unborn), "init", "-q"], capture_output=True, timeout=HANG_TIMEOUT)
    m.Host(str(unborn), "testprofile").ensure_repo()
    assert subprocess.run(["git", "-C", str(unborn), "rev-parse", "HEAD"], capture_output=True, timeout=HANG_TIMEOUT).returncode == 0


def test_the_permission_flag_goes_after_the_separator(tmp_path, monkeypatch):
    """Bare args reach ccmorph-rc, which exits 2 on a flag it does not know; only
    what follows `--` reaches claude. A line with no args must still get the `--`
    so the flag has somewhere to go."""
    m = load()
    seen = {}
    class P:
        pid = 1
        def poll(self): return None
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: seen.update(cmd=cmd) or P())

    m.Host(str(tmp_path), "gateway", permission="bypassPermissions").start()
    cmd = seen["cmd"]
    assert "--" in cmd, cmd
    assert cmd.index("--") < cmd.index("--permission-mode=bypassPermissions"), cmd

    seen.clear()
    m.Host(str(tmp_path), "gateway", ("--session-id", "session_01X"),
           permission="bypassPermissions").start()
    cmd = seen["cmd"]
    assert cmd.index("--") < cmd.index("--session-id"), cmd
    assert cmd.index("--") < cmd.index("--permission-mode=bypassPermissions"), cmd

    seen.clear()
    m.Host(str(tmp_path), "gateway").start()          # a repo host names its spawn mode:
    assert "--spawn=worktree" in seen["cmd"]          # no first-run prompt to block on


def test_a_worktree_host_keeps_a_debug_log(tmp_path, monkeypatch):
    """A fresh phone session is the one that goes wrong with nothing on disk.

    Only the conversation host used to get --debug-file, so a session started
    from the phone left no record to read afterwards.
    """
    m = load()
    seen = {}
    class P:
        pid = 1
        def poll(self): return None
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: seen.update(cmd=cmd) or P())

    m.Host(str(tmp_path), "gateway").start()
    cmd = seen["cmd"]
    assert cmd.index("--") < cmd.index("--spawn=worktree"), cmd
    debug = [c for c in cmd if c.startswith("--debug-file=")]
    assert len(debug) == 1 and cmd.index(debug[0]) > cmd.index("--"), cmd


def test_a_retired_conversation_has_its_host_deleted(tmp_path, monkeypatch):
    """Archived + idle past the limit: its host row is deleted, by the id its
    own conversation has on the server, with the account's own login. The id
    comes from the server's record of the pinned session, not the folder's
    debug log -- that is every pin's, each pinned start clears it, and here the
    sibling ran last, so the folder holds only the sibling's environment: the
    retired conversation's own row is deleted anyway, and only it (GH-450)."""
    m = load()
    monkeypatch.setattr(m, "LOGS", tmp_path)
    h = m.Host(str(tmp_path / "bridge"), "", ("--session-id", "session_01X"))
    h.debugfile.write_text("[bridge:init] reuseEnvironmentId=env_BBBBBB\n"
                           "[bridge:init] Registered, server environmentId=env_BBBBBB\n")
    monkeypatch.setattr(m, "sessions", lambda token, status:
                        [{"id": "cse_01X", "environment_id": "env_01OLD"},
                         {"id": "cse_01XB", "environment_id": "env_BBBBBB"}]
                        if status == "archived" else [])
    calls = []

    class FakeRc:
        @staticmethod
        def account_candidates(): return ["acct"]
        @staticmethod
        def profile_env(p): return {"CLAUDE_CONFIG_DIR": "/x/acct"}

    monkeypatch.setattr(m, "rc_module", lambda: FakeRc)
    monkeypatch.setattr(m.subprocess, "run", lambda *a, **k: type(
        "R", (), {"stdout": '{"claudeAiOauth": {"accessToken": "tok"}}'})())

    class Resp:
        def __enter__(self): return self
        def __exit__(self, *a): pass
        def read(self): return b""
    monkeypatch.setattr(m.DIRECT, "open", lambda req, timeout: calls.append(req) or Resp())
    m.delete_host(h)
    assert calls[0].get_method() == "DELETE"
    assert calls[0].full_url.endswith("/v1/environments/bridge/env_01OLD")
    assert calls[0].headers["Authorization"] == "Bearer tok"
    assert len(calls) == 1        # the archived sibling's row stays while its pin does


def test_retiring_a_pin_that_never_ran_deletes_no_host_row(tmp_path, monkeypatch):
    """GH-450: delete_host read the folder's logs, so on a folder with two pins
    where only the sibling had run, the last environment id on disk was the
    sibling's: retiring the never-started pin deleted the sibling's host row in
    the app while its pin stayed, and the conversation vanished from the phone.
    The server lists each conversation under its own id, and a pin that never
    ran has no archived session of its own -- not even beside its sibling's --
    so its retire issues no delete at all."""
    m = load()
    monkeypatch.setattr(m, "LOGS", tmp_path)
    deleted = []
    monkeypatch.setattr(m, "api", lambda method, path, token, body=None: deleted.append(path))
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    folder = str(tmp_path / "bridge")
    sibling = m.Host(folder, "", ("--session-id", "session_01XB"))
    sibling.debugfile.write_text("[bridge:init] reuseEnvironmentId=env_BBBBBB\n"
                                 "[bridge:init] Registered, server environmentId=env_BBBBBB\n")
    monkeypatch.setattr(m, "sessions", lambda token, status:
                        [{"id": "cse_01XB", "environment_id": "env_BBBBBB"}]
                        if status == "archived" else [])
    m.delete_host(m.Host(folder, "", ("--session-id", "session_01XA")))
    assert deleted == []        # the sibling's row is not this pin's to delete


def test_purging_keeps_a_session_whose_conversation_is_still_pinned(tmp_path, monkeypatch):
    """Retirement finds the host row to delete in the session's own record
    (GH-450), so a purge must not eat that record while its pin still exists --
    however long the folder's other pins keep its retirement waiting. Once the
    pin line is gone, the session ages out like any other."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    deleted = []
    def delete(method, path, token, body=None):
        deleted.append(path)
        found[:] = [s for s in found if f"/v1/code/sessions/{s['id']}" != path]
    monkeypatch.setattr(m, "api", delete)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    old = {"environment_kind": "bridge", "environment_id": "env_01X",
           "updated_at": "2026-10-01T00:00:00Z"}
    found = [{"id": "cse_01PIN", **old}, {"id": "cse_01OLD", **old}]
    monkeypatch.setattr(m, "sessions", lambda token, status: found)
    monkeypatch.setattr(m, "host_envs", lambda folders: {})
    now = m.stamp("2026-10-10T12:00:00Z")
    m.purge_archived([], now, 86400, ("session_01PIN",))
    assert deleted == ["/v1/code/sessions/cse_01OLD"]   # only the pinned one waits
    m.purge_archived([], now, 86400)
    assert deleted == ["/v1/code/sessions/cse_01OLD", "/v1/code/sessions/cse_01PIN"]


@pytest.mark.parametrize("name, default", [("idle_days", 1.0), ("purge_days", 7.0)])
def test_days_settings_fall_back_to_the_documented_default_when_absent_or_malformed(name, default, capsys):
    """Both retire limits parse one way: absent gives the documented default
    (idle 1 day, purge 7), a number is used as given, and a malformed value is
    logged and falls back to the default. GH-341: `.` and `1..0` matched the
    `[0-9.]+` pattern but not float(), and the ValueError ended supervision."""
    m = load()
    days = getattr(m, name)
    assert days("route=x\n") == default                    # absent: the default
    assert days(f"{name}=3.5\n") == 3.5                    # a real number
    for bad in (".", "1..0", "0.5.5"):
        assert days(f"{name}={bad}\n") == default          # malformed: never a raise
        assert f"{name}={bad} is not a number" in capsys.readouterr().out


@needs_loopback  # this live edge really listens on loopback: #611
def test_a_live_edge_is_the_record_its_own_file_holds(tmp_path, monkeypatch):
    """The record is what the adoption decision compares, so the reader returns it,
    and only for an edge that is alive, answering, and still holds its TLS
    material. Liveness is ccmorph-rc's live_edge (#638): the supervisor must not
    adopt an edge whose CA file is gone, because ccmorph-rc refuses to serve from
    one and every bridge under it would fail closed."""
    import json, os, socket
    m = load()
    rc = m.rc_module()
    monkeypatch.setattr(m, "EDGES", tmp_path)
    monkeypatch.setattr(rc, "EDGES", tmp_path)    # live_edge reads its own module's
    # Below the ephemeral floors: the record's port must read dead the moment
    # srv closes it, and a number drawn from the ephemeral pool is free for
    # any parallel process to take in that gap.
    port = port_outside_the_ephemeral_pool()
    ca = tmp_path / "edge-ca.pem"
    ca.write_text("")
    with socket.socket() as srv:
        srv.bind(("127.0.0.1", port))
        srv.listen()
        record = {"pid": os.getpid(), "port": port, "ca": str(ca),
                  "upstream": "u", "tiers": {"fable": "m"}}
        (tmp_path / "gw.json").write_text(json.dumps(record))
        ca.unlink()
        assert m.edge_ready("gw") is None         # the CA file is gone: refused
        ca.write_text("")
        assert m.edge_ready("gw") == record
    assert m.edge_ready("gw") is None             # its proxy stopped answering
    (tmp_path / "gw.json").write_text(json.dumps({**record, "pid": 5_000_000}))
    assert m.edge_ready("gw") is None             # no process with that pid
    (tmp_path / "gw.json").unlink()
    assert m.edge_ready("gw") is None             # no record at all


@pytest.mark.parametrize("recorded, configured, killed", [
    pytest.param({"fable": "old-model"}, {"fable": "old-model"}, False, id="same-tiers"),
    pytest.param({"fable": "old-model"}, {"fable": "new-model"}, True, id="changed-tier-issue-356"),
    pytest.param(None, {}, True, id="record-without-tiers-issue-356"),
])
def test_a_live_edge_from_a_previous_supervisor_is_reused(tmp_path, monkeypatch, recorded, configured, killed):
    """An edge outlives the supervisor that started it, so a restart adopts one it
    must compare to the config it now serves: the tiers its proxy pins were fixed
    at its own start, and every folder host under it inherits them. An edge whose
    record predates the tiers stays no evidence, so it is replaced (issue #356)."""
    m = load()
    monkeypatch.setattr(m, "LOGS", tmp_path)
    for var in m.ALIAS_ENV.values():
        monkeypatch.delenv(var, raising=False)      # the ambient map is not the config's
    e = m.Host(str(tmp_path), "gw", tiers=configured)
    e.edge = True
    live = {"pid": 4242, "port": 1, "ca": "x", "upstream": "u",
            **({} if recorded is None else {"tiers": recorded})}
    monkeypatch.setattr(m, "edge_ready", lambda route: live)
    monkeypatch.setattr(m, "is_our_edge", lambda pid, route: True)
    monkeypatch.setattr(m.subprocess, "Popen", lambda *a, **k: (_ for _ in ()).throw(AssertionError("started")))
    kills = []
    monkeypatch.setattr(m.os, "kill", lambda pid, sig: kills.append((pid, sig)))
    e.start()
    assert e.proc is None
    assert kills == ([(4242, m.signal.SIGTERM)] if killed else [])


def test_a_record_naming_a_pid_that_is_not_an_edge_is_left_alone(tmp_path, monkeypatch):
    """A record outlives the edge that wrote it, so its pid can belong to something
    else by the time a supervisor reads it. A stale pid is never signalled on the
    strength of the file alone."""
    m = load()
    monkeypatch.setattr(m, "LOGS", tmp_path)
    monkeypatch.setattr(m, "edge_ready", lambda route: {"pid": 4242, "port": 1, "ca": "x",
                                                        "upstream": "u", "tiers": {"fable": "old-model"}})
    monkeypatch.setattr(m, "is_our_edge", lambda pid, route: False)
    monkeypatch.setattr(m.subprocess, "Popen", lambda *a, **k: (_ for _ in ()).throw(AssertionError("started")))
    kills = []
    monkeypatch.setattr(m.os, "kill", lambda pid, sig: kills.append((pid, sig)))
    e = m.Host(str(tmp_path), "gw", tiers={"fable": "new-model"})
    e.edge = True
    e.start()
    assert kills == []


def test_an_edge_on_a_fresh_state_dir_makes_the_bridge_dir_and_starts(tmp_path, monkeypatch):
    """The edge's folder is the bridge dir itself, and a fresh state dir has none
    of it: start() checked for the folder before anything could create it (the
    LOGS mkdir sits past that early return), so the edge backed off forever and
    every folder host, each gated on edge readiness, waited with it (issue #419)."""
    m = load()
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)   # argv test: git is separate
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    e = m.Host(str(m.EDGES.parent), "gw")        # the folder main() gives an edge
    e.edge = True
    e.start()
    assert e.proc is not None                    # launched, not backed off forever
    assert Path(e.folder).is_dir()               # and the bridge dir exists


@pytest.mark.parametrize("command, ours", [
    # The shape `ps -o command=` prints for the pid the record holds: the edge's own
    # argv, not the /usr/bin/script wrapper the supervisor starts it through.
    pytest.param("python3 /x/addons/remote-control/ccmorph-rc gw --edge", True, id="this-routes-edge"),
    pytest.param("python3 /x/addons/remote-control/ccmorph-rc ds --edge", False, id="another-routes-edge"),
    pytest.param("python3 /x/addons/remote-control/ccmorph-rc gw -- --session-id session_01X", False,
                 id="a-bridge-of-the-same-route"),
    pytest.param("grep -r ccmorph-rc --edge gw", False, id="something-that-merely-mentions-it"),
])
def test_only_this_routes_own_edge_process_is_signalled(monkeypatch, command, ours):
    """A record's pid can be recycled by anything, so the signal waits for the
    process to say, in its own argv, that it is this route's `--edge` run."""
    m = load()
    monkeypatch.setattr(m.subprocess, "run", lambda *a, **k: type("R", (), {"stdout": command})())
    assert m.is_our_edge(4242, "gw") is ours
    monkeypatch.setattr(m.subprocess, "run", lambda *a, **k: (_ for _ in ()).throw(OSError("no ps")))
    assert m.is_our_edge(4242, "gw") is False   # no verdict without the lookup


def test_a_created_edge_leaves_no_git_repo_in_the_state_dir(tmp_path, monkeypatch):
    """GH-418: start() ran its worktree-spawn setup for the edge host too. The edge
    never execs claude remote-control -- it writes its record and loops, so it
    spawns no worktree, and its claude_args drop `extra` whole -- while
    ensure_repo() had already run against its folder, the shared bridge state dir.
    A supervisor start that had to (re)create an edge wrote a git repo and an
    empty base commit into it, and the state dir's logs and edge records became
    untracked files of a repo nothing uses."""
    m = load()
    m.EDGES.parent.mkdir(parents=True)                 # the state dir, with no repo in it
    e = m.Host(str(m.EDGES.parent), "gw")              # the edge host, as main() builds it
    e.edge, e.logfile = True, m.LOGS / "edge-gw.log"
    monkeypatch.setattr(m, "edge_ready", lambda route: False)   # no live edge to adopt
    real_popen = m.subprocess.Popen
    def popen(cmd, **kw):          # ensure_repo's git runs for real through run(); only
        return real_popen(cmd, **kw) if cmd[0] == "git" else FakeProc()   # the spawn is faked
    monkeypatch.setattr(m.subprocess, "Popen", popen)
    e.start()
    assert e.proc is not None                          # the edge started ...
    assert not (m.EDGES.parent / ".git").exists()      # ... without a repo in the state dir


def test_a_slashed_route_gets_a_cleaned_edge_log_and_survives_the_tick(tmp_path, monkeypatch, capsys):
    """A folder host's log name is cleaned of path characters before it is joined onto
    LOGS; the edge host's was built from the raw `route=` (issue #846): `route=gw/eu`
    opened `edge-gw/eu.log` inside a directory nothing ever creates, and the
    FileNotFoundError escaped the tick with no guard, so launchd restarted the
    supervisor into the same crash and Remote Control went down on one config line.
    The edge now starts -- naming the line in the log -- and its log sits directly
    under LOGS under the cleaned name."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw/eu\n{tmp_path}/a\n")
    (tmp_path / "a").mkdir()
    # the real Host.start, with only the spawn faked: the open() that raised is
    # the one this tick runs, and main() must outlive it.
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    run_main(m, monkeypatch, tmp_path, cfg, m.Host.start, edge_ready=lambda route: False)
    assert "started via gw/eu" in capsys.readouterr().out
    logs = sorted(p.name for p in (tmp_path / "logs").glob("edge-*.log"))
    assert len(logs) == 1 and logs[0].startswith("edge-gw-eu-")   # cleaned, one file
    assert not (tmp_path / "logs" / "gw").exists()                # never a subdirectory


def test_two_routes_that_clean_alike_open_their_own_edge_logs(tmp_path, monkeypatch):
    """#846 review: the first fix named a cleaned route with a CRC32 suffix, and
    two distinct profile names can be steered onto one checksum -- both edges
    then opened one log and wrote one state record, so one route adopted the
    other's edge. These two long underscore-heavy routes -- the reviewer's pair,
    equal stems and equal CRC32 -- now keep their whole names, because a token a
    single file name can hold is never renamed."""
    m = load()
    a = "a____b_c___d__e___f__g___h__i_j____k__l_m____n____o___p_q"
    b = "a_b____c____d__e___f___g___h____i__j___k_l_m____n___o_p___q"
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route={a}\n{tmp_path}/a\n{tmp_path}/b route={b}\n")
    for d in "ab":
        (tmp_path / d).mkdir()
    logs = []
    def spawn(cmd, **kw):                    # the real start(), with only the spawn faked
        if "--edge" in cmd:
            logs.append(Path(kw["stdout"].name))
        return FakeProc()
    monkeypatch.setattr(m.subprocess, "Popen", spawn)
    run_main(m, monkeypatch, tmp_path, cfg, m.Host.start, edge_ready=lambda route: False)
    assert len(logs) == 2 and len(set(logs)) == 2       # one log per edge, never one shared log
    assert all(p.parent == tmp_path / "logs" for p in logs)   # both directly under LOGS


def test_a_second_route_pushed_onto_an_owned_edge_name_is_refused_in_the_log(tmp_path, monkeypatch, capsys):
    """Any digest has an equal-name case somewhere, so the supervisor keeps the
    promise itself rather than trusting it: a route whose edge name the first
    route already owns is refused with a line naming both, as a refused pin line
    is (#342) -- never silently sharing the log or the state record."""
    m = load()
    monkeypatch.setattr(m, "edge_name", lambda route: "one-name")   # force the collision
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=first\n{tmp_path}/a\n{tmp_path}/b route=second\n")
    for d in "ab":
        (tmp_path / d).mkdir()
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    run_main(m, monkeypatch, tmp_path, cfg, m.Host.start, edge_ready=lambda route: False)
    out = capsys.readouterr().out
    assert "refused" in out and "one-name" in out and "second" in out
    assert "not hosted, its route second was refused" in out   # nor is its bridge
    assert len(list((tmp_path / "logs").glob("edge-*.log"))) == 1   # the second opened nothing


def test_a_folder_log_name_keeps_the_cleaning_it_always_had(tmp_path, monkeypatch):
    """The folder site's cleaning is the one the edge now shares (#846): the very
    expression it ran before the shared helper existed is the oracle here, so a
    change to the rule can never move a folder's log out from under its rows."""
    import re
    m = load()
    monkeypatch.setattr(m, "LOGS", tmp_path)
    folder = "/srv/a project/c++"
    h = m.Host(folder, "gw")
    name = re.sub(r"[^A-Za-z0-9]+", "-", folder).strip("-")
    assert h.logfile == tmp_path / f"{name}.log"
    assert h.debugfile == tmp_path / f"{name}.debug.log"


def _recording_start(m, started):
    """The real Host.start's identity recorded, with the spawn faked: what the
    supervisor decided to run, and under which name, without a process."""
    def start(host):
        started.append((host.route, host.edge, host.folder))
        host.proc, host.started = FakeProc(), m.time.time()
    return start


def test_routes_that_differ_only_by_letter_case_run_one_edge(tmp_path, monkeypatch, capsys):
    """#846 review round 4: on macOS's case-insensitive volume `edge-gw.log` and
    `edge-GW.log` are one file, and the supervisor compared names case-sensitively,
    so it started both edges onto one log and one state record. A name is one file
    when it differs only by case: `GW` is refused in the log, and none of its
    folders are hosted -- a bridge behind a refused edge would wait forever on it
    and read the owner's record (INTENT 8, 3). `gw` serves two folders, so the
    one-folder `GW` loses even though it sorts first."""
    m = load()
    started = []
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw\n{tmp_path}/a\n{tmp_path}/b\n{tmp_path}/c route=GW\n")
    for d in "abc":
        (tmp_path / d).mkdir()
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    run_main(m, monkeypatch, tmp_path, cfg, _recording_start(m, started))
    assert [r for r, edge, _ in started if edge] == ["gw"]       # one edge, the owner
    assert not [f for f, edge, _ in started if not edge and f.endswith("/c")]
    out = capsys.readouterr().out
    assert "edge: GW refused, gw already owns the name gw" in out
    assert "not hosted, its route GW was refused" in out


def test_a_running_edge_keeps_its_name_when_a_case_twin_arrives_with_more_folders(tmp_path,
                                                                                monkeypatch, capsys):
    """A name already serving a live edge is not taken away by a later reload: a
    config edit adding two folders on `route=GW` -- more than `gw` has -- must not
    let the twin win the name off the running edge, or the edge that is up would
    keep writing the log and record the twin now owns (review round 4, as round 3
    for a digest alias). The twin is refused and none of its folders are hosted."""
    m = load()
    started = []
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw\n{tmp_path}/a\n")
    for d in "abc":
        (tmp_path / d).mkdir()
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 1:                    # mid-run: the twin arrives with two folders
            cfg.write_text(f"route=gw\n{tmp_path}/a\n{tmp_path}/b route=GW\n{tmp_path}/c route=GW\n")
        if ticks[0] > 4:                     # the rewrite's own mtime drives the reload
            raise SystemExit                 # (ns granularity, as every reload test here assumes)
    run_main(m, monkeypatch, tmp_path, cfg, _recording_start(m, started), sleep=sleep,
             edge_ready=lambda route: False)
    assert [r for r, edge, _ in started if edge] == ["gw"]       # the running edge kept its name
    assert not [r for r, _, _ in started if r == "GW"]           # the twin hosts nothing
    out = capsys.readouterr().out
    assert "edge: GW refused, gw already owns the name gw" in out
    assert out.count("not hosted, its route GW was refused") == 2


def test_a_route_spelled_as_another_routes_cleaned_name_gets_its_own_edge_log(tmp_path, monkeypatch,
                                                                            capsys):
    """A route may BE the text another route's name is built from -- `gw-eu-<digest>`
    is exactly what `edge_name("gw/eu")` emits, and a person may type it as a
    route. It is not that profile name (a hyphen is not in [A-Za-z0-9_]), so it
    is cleaned and digests to a name of its own: both edges start, on two
    different logs, and neither is refused."""
    m = load()
    alias = m.edge_name("gw/eu")
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw/eu\n{tmp_path}/a\n{tmp_path}/b route={alias}\n")
    for d in "ab":
        (tmp_path / d).mkdir()
    logs = []
    def spawn(cmd, **kw):                    # the real start(), with only the spawn faked
        if "--edge" in cmd:
            logs.append(Path(kw["stdout"].name))
        return FakeProc()
    monkeypatch.setattr(m.subprocess, "Popen", spawn)
    run_main(m, monkeypatch, tmp_path, cfg, m.Host.start, edge_ready=lambda route: False)
    assert len(logs) == 2 and len(set(logs)) == 2       # a name of its own, never the first's
    assert all(p.parent == tmp_path / "logs" for p in logs)
    assert "already owns the name" not in capsys.readouterr().out


def test_every_processless_session_is_handed_back_and_retried_slowly(tmp_path, monkeypatch):
    """Archive removes a clean worktree and unarchive never restarts it (2026-10-10);
    a session idle without a process waited hours on a message (2026-10-12)."""
    import json
    m = load()
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    active = [{"id": "cse_01UNARCH", "environment_id": "env_01R", "connection_status": "disconnected",
               "external_metadata": {"unarchived_epoch": 3}},
              {"id": "cse_01LIVE", "environment_id": "env_01R", "connection_status": "connected",
               "external_metadata": {"unarchived_epoch": 1}},
              {"id": "cse_01PARKED", "environment_id": "env_01R", "connection_status": "disconnected",
               "external_metadata": {}},
              {"id": "cse_01OTHER", "environment_id": "env_01ELSE", "connection_status": "disconnected",
               "external_metadata": {"unarchived_epoch": 2}}]
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            assert path == "/v1/environments/env_01R/bridge/reconnect"
            return {}
        assert "statuses=active" in path
        return {"data": active}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    done = {}
    m.recover_processless([str(repo)], done, set())
    assert posts == []                                  # no host is up: nothing to hand to
    m.recover_processless([str(repo)], done, {str(repo)})
    assert posts == ["cse_01UNARCH", "cse_01PARKED"]   # not the connected one, not another host's
    m.recover_processless([str(repo)], done, {str(repo)})
    assert len(posts) == 2                              # within REWAKE_AFTER: never re-queued
    active[0]["external_metadata"]["unarchived_epoch"] = 4
    m.recover_processless([str(repo)], done, {str(repo)})
    assert posts[2:] == ["cse_01UNARCH"]                # a new unarchive: handed back at once
    clock[0] += m.REWAKE_AFTER
    m.recover_processless([str(repo)], done, {str(repo)})
    assert sorted(posts[3:]) == ["cse_01PARKED", "cse_01UNARCH"]   # still dead: retried slowly


def test_a_session_the_server_never_starts_is_parked_not_rewoken_forever(tmp_path, monkeypatch,
                                                                         capsys):
    """#974: one session answered 200 to 232 hand-backs and never spawned, every
    REWAKE_AFTER for 40 hours. The hand-backs must stop at REWAKE_TRIES, park the
    session until its next input, and the log must say once why they stopped."""
    import json
    m = load()
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    active = [{"id": "cse_01DEAD", "environment_id": "env_01R", "connection_status": "disconnected",
               "external_metadata": {}, "user_message_count": 4,
               "last_event_at": "2026-11-25T04:50:00Z"},
              {"id": "cse_01WOKEN", "environment_id": "env_01R", "connection_status": "disconnected",
               "external_metadata": {}}]
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            assert path == "/v1/environments/env_01R/bridge/reconnect"
            return {}
        return {"data": active}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    done, failed, parked, tries = {}, set(), {}, {}

    def check():
        return m.recover_processless([str(repo)], done, {str(repo)}, failed, parked, tries)

    check()                                      # a host restart's death: its one hand-back goes out
    assert sorted(posts) == ["cse_01DEAD", "cse_01WOKEN"]
    active[1]["connection_status"] = "connected"    # WOKEN's wake worked: it started
    for _ in range(m.REWAKE_TRIES - 1):             # DEAD stays processless between checks
        clock[0] += m.REWAKE_AFTER
        check()
    assert posts.count("cse_01DEAD") == m.REWAKE_TRIES   # a fixed number of tries, no more
    capsys.readouterr()
    clock[0] += m.REWAKE_AFTER
    check()                                        # still no process after the last hand-back
    assert posts.count("cse_01DEAD") == m.REWAKE_TRIES   # parked: no further POST
    assert "cse_01DEAD" in parked
    out = capsys.readouterr().out
    assert out.count("cse_01DEAD") == 1 and "parked" in out   # named once, with the reason
    for _ in range(3):                             # and silent from then on
        clock[0] += m.REWAKE_AFTER
        check()
    assert posts.count("cse_01DEAD") == m.REWAKE_TRIES
    assert "cse_01DEAD" not in capsys.readouterr().out
    active[0]["user_message_count"] = 5            # its owner messages it on the phone
    check()
    assert posts.count("cse_01DEAD") == m.REWAKE_TRIES + 1 and "cse_01DEAD" not in parked
    active[0]["connection_status"] = "connected"   # it started: the tries are cleared
    check()
    active[0]["connection_status"] = "disconnected"    # a later death gets them again
    clock[0] += m.REWAKE_AFTER
    check()
    assert posts.count("cse_01DEAD") == m.REWAKE_TRIES + 2


def test_a_session_that_only_ever_gets_500_parks_too(tmp_path, monkeypatch):
    """The same forever loop with an error answer: a persistent 500 every
    REWAKE_AFTER is a hand-back the server answered and nothing started, so it
    caps and parks like a 200; only a transient (no answer) is not counted."""
    import urllib.error
    m, repo, posts, clock = _failing_hand_back(
        tmp_path, monkeypatch, lambda p: urllib.error.HTTPError(p, 500, "no", {}, None))
    done, failed, parked, tries = {}, set(), {}, {}
    per = []
    for _ in range(m.REWAKE_TRIES + 2):
        before = len(posts)
        m.recover_processless([str(repo)], done, {str(repo)}, failed, parked, tries)
        per.append(len(posts) - before)
        clock[0] += m.REWAKE_AFTER
    assert per == [1] * m.REWAKE_TRIES + [0, 0]
    assert "cse_01B" in parked


def test_a_parked_session_remembered_lost_is_held_back_by_both_paths(tmp_path, monkeypatch):
    """The park only holds if recover_interrupted honors it: remembered `lost`, a
    parked session was POSTed at by the other recovery path every RECOVER_EVERY
    under a persistent 500 -- a fourth hand-back in the very cycle the log said it
    parked, a fifth 20 s later, forever. As the supervisor runs it, through cycle()."""
    import json
    import urllib.error
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    m.BUSY.write_text(json.dumps({"cse_01B": "lost"}))
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    row = {"id": "cse_01B", "environment_id": "env_01R", "connection_status": "disconnected"}
    posts, lines = [], []

    def api(method, path, token, body=None):
        posts.append(body["session_id"])
        raise urllib.error.HTTPError(path, 500, "no", {}, None)
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "host_sessions", lambda *_: ("tok", [(str(repo), row)]))
    monkeypatch.setattr(m, "log", lines.append)
    monkeypatch.setattr(m, "keep_login_fresh", lambda *a: None)
    monkeypatch.setattr(m, "restart_logged_out", lambda *a: None)
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    monkeypatch.setattr(m, "IDLE_CHECK", float("inf"))
    state = m.Clock()
    state.last_recover = clock[0] - m.RECOVER_EVERY
    hosts = {str(repo): SimpleNamespace(folder=str(repo), proc=True, started=0)}

    def tick(step=None):
        m.cycle(hosts, [str(repo)], clock[0], state)
        clock[0] += step if step is not None else m.REWAKE_AFTER
    for _ in range(m.REWAKE_TRIES):                  # three answered, failed hand-backs
        tick()
    assert len(posts) == m.REWAKE_TRIES
    lines.clear()
    tick()                                           # the cap cycle: parked, named once
    tick(m.RECOVER_EVERY)                            # and silent from there on
    assert len(posts) == m.REWAKE_TRIES              # the other path POSTed at a parked session
    assert sum("parked" in line for line in lines) == 1
    assert all("recovery failed" not in line for line in lines)


def test_a_park_does_not_carry_across_a_new_unarchive(tmp_path, monkeypatch):
    """`done` and `tries` are keyed by (id, unarchived_epoch); a park keyed by the
    id alone suppresses the new unarchive's first hand-back whenever the server's
    message count and event stamp come back unchanged -- main sends one there, so
    the park must belong to the epoch it was made in."""
    import json
    m = load()
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    row = {"id": "cse_01DEAD", "environment_id": "env_01R", "connection_status": "disconnected",
           "user_message_count": 4, "last_event_at": "2026-11-25T04:50:00Z",
           "external_metadata": {"unarchived_epoch": 1}}
    active = [row]
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            return {}
        return {"data": active}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    done, failed, parked, tries = {}, set(), {}, {}

    def check():
        m.recover_processless([str(repo)], done, {str(repo)}, failed, parked, tries)
        clock[0] += m.REWAKE_AFTER
    for _ in range(m.REWAKE_TRIES + 1):              # capped and parked under epoch 1
        check()
    assert len(posts) == m.REWAKE_TRIES and "cse_01DEAD" in parked
    active.clear()                                   # archived: gone from the active list
    check()
    row["external_metadata"]["unarchived_epoch"] = 2  # unarchived again: fresh worktree, same stamps
    active.append(row)
    check()
    assert len(posts) == m.REWAKE_TRIES + 1          # the new epoch gets its hand-back
    assert "cse_01DEAD" not in parked


def test_the_issue_974_loop_ends_through_cycle_at_the_real_cadence(tmp_path, monkeypatch):
    """#974 as the supervisor ran it: every RECOVER_EVERY tick finds the session
    processless and the server answers each hand-back with 200 and never starts
    it. Four hours of ticks must yield exactly REWAKE_TRIES hand-backs (no closer
    together than REWAKE_AFTER), one line naming the session and why it stopped,
    nothing else about it -- and its owner's message wakes it at once, then the
    ordinary spacing returns."""
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")   # no turn was cut off
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    sid = "cse_01UYCe7TqcH2EuG69UtpJ5fm"
    row = {"id": sid, "environment_id": "env_01R", "connection_status": "disconnected",
           "external_metadata": {}, "user_message_count": 4,
           "last_event_at": "2026-11-25T04:50:00Z"}
    posts, lines = [], []
    clock = [1000.0]

    def api(method, path, token, body=None):
        assert method == "POST" and path == "/v1/environments/env_01R/bridge/reconnect"
        posts.append(clock[0])
        return {}                                    # HTTP 200, and the process never starts
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "host_sessions", lambda *_: ("tok", [(str(repo), row)]))
    monkeypatch.setattr(m, "log", lines.append)
    monkeypatch.setattr(m, "keep_login_fresh", lambda *a: None)
    monkeypatch.setattr(m, "restart_logged_out", lambda *a: None)
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    monkeypatch.setattr(m, "IDLE_CHECK", float("inf"))
    state = m.Clock()
    state.last_recover = clock[0] - m.RECOVER_EVERY
    hosts = {str(repo): SimpleNamespace(folder=str(repo), proc=True, started=0)}

    def tick():
        m.cycle(hosts, [str(repo)], clock[0], state)
        clock[0] += m.RECOVER_EVERY

    for _ in range(int(4 * 3600 / m.RECOVER_EVERY)):   # four hours of supervisor ticks
        tick()
    assert len(posts) == m.REWAKE_TRIES               # a fixed number, not one every 10 min...
    assert all(b - a >= m.REWAKE_AFTER for a, b in zip(posts, posts[1:]))
    named = [line for line in lines if sid in line]
    assert len(named) == m.REWAKE_TRIES + 1           # ...each hand-back, then one park line
    assert all("handed back" in line for line in named[:-1])
    assert "parked" in named[-1]                      # why it stopped, said once
    row["user_message_count"] = 5                     # its owner messages it on the phone
    woke_at = clock[0]
    tick()
    assert len(posts) == m.REWAKE_TRIES + 1 and posts[-1] == woke_at   # input goes out at once
    while clock[0] < woke_at + m.REWAKE_AFTER:
        tick()
    assert len(posts) == m.REWAKE_TRIES + 1           # then the ordinary 10-minute spacing
    tick()
    assert len(posts) == m.REWAKE_TRIES + 2


def test_an_idle_session_parks_and_wakes_only_on_input(tmp_path, monkeypatch):
    """21 idle phone sessions held 3.2 GB, some 2.5 days old (2026-11-19): an idle
    session's process ends after park_hours and comes back on its next message."""
    import json
    m = load()
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    now = 100_000.0
    old, fresh = "1970-01-01T00:00:00Z", "1970-01-02T03:46:30Z"   # 0 s and 99,990 s
    active = [{"id": "cse_01OLD", "environment_id": "env_01R", "connection_status": "connected",
               "worker_status": "idle", "last_event_at": old, "user_message_count": 4},
              {"id": "cse_01BUSY", "environment_id": "env_01R", "connection_status": "connected",
               "worker_status": "running", "last_event_at": old},
              {"id": "cse_01NEW", "environment_id": "env_01R", "connection_status": "connected",
               "worker_status": "idle", "last_event_at": fresh}]
    posts, killed = [], []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            return {}
        return {"data": active}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, [now], m.time.sleep))
    monkeypatch.setattr(m, "session_pid", lambda sid: {"cse_01OLD": 41, "cse_01BUSY": 42, "cse_01NEW": 43}[sid])
    monkeypatch.setattr(m.os, "kill", lambda pid, sig: killed.append(pid))
    monkeypatch.setattr(m, "gone_within", lambda pid, s: True)
    parked = {}
    assert m.park_idle([str(repo)], now, parked, m.park_hours("") * 3600) == ["cse_01OLD"]
    assert killed == [41]                       # not the running one, not the recent one
    active[0]["connection_status"] = "disconnected"
    done = {}
    m.recover_processless([str(repo)], done, {str(repo)}, None, parked)
    assert posts == []                          # parked, no input: left processless
    active[0]["user_message_count"] = 5         # Alex sent a message
    m.recover_processless([str(repo)], done, {str(repo)}, None, parked)
    assert posts == ["cse_01OLD"] and parked == {}   # handed back: it wakes


def test_park_hours_zero_keeps_every_process(monkeypatch):
    m = load()
    assert m.park_hours("park_hours=0\n") == 0
    assert m.park_idle([], 0.0, {}, 0) == []
    assert m.parse("park_hours=6\n") == {}     # a setting, not a folder


def test_no_hand_back_before_the_host_that_serves_it_is_up(tmp_path, monkeypatch):
    """A supervisor restart handed every session back to a host still starting
    (2026-10-14).

    main() ran the pass with `last_recover = 0.0`, so it fired on tick 1, before
    the folder's host had started; the POST reached a host that was not polling
    yet and was dropped, and `done[key] = now` then suppressed the hand-back for
    REWAKE_AFTER -- exactly the window in which it was needed. The session stayed
    disconnected until someone unarchived it or the check came round again.
    """
    import json
    m = load()
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    # A host that re-registered left this behind: the session is known even though
    # no host of this supervisor run is up yet.
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\n")
    active = [{"id": "cse_01IDLE", "environment_id": "env_01R",
               "connection_status": "disconnected", "external_metadata": {}}]
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append((ticks[0], body["session_id"]))
            return {}
        return {"data": active}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    clock, ticks, host_up = [1_000_000.0], [0], [False]
    HOST_UP_AT = 4                    # well inside the window before the first pass

    def start(self):
        if self.edge or host_up[0]:
            self.proc, self.started = FakeProc(), clock[0]
        else:
            self.next_try = clock[0] + m.BACKOFF_MIN     # a host that is not up yet

    def sleep(_):
        ticks[0] += 1
        clock[0] += m.TICK
        if ticks[0] == HOST_UP_AT:
            host_up[0] = True             # the folder's host starts here, started=now
        if ticks[0] > 70:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep, clock=clock)
    # Nothing may be handed to a host that is not polling yet -- the POST is
    # dropped and the session is then suppressed for REWAKE_AFTER.
    assert [t for t, _ in posts if t < HOST_UP_AT] == [], posts
    # Once the host is up and has been for RECOVER_EVERY, the session is handed back.
    assert posts and posts[0][1] == "cse_01IDLE", posts
    assert posts[0][0] >= HOST_UP_AT + m.RECOVER_EVERY, posts
    assert len(posts) == 1, posts          # and then not again within REWAKE_AFTER


@pytest.mark.parametrize("line, limit, purge", [
    ("idle_days=.", 1, 7),
    ("idle_days=1..0", 1, 7),
    ("purge_days=.", 1, 7),
    ("purge_days=0.5.5", 1, 7),
    ("idle_days=2\npurge_days=30", 2, 30),
], ids=["idle-dot-issue-341", "idle-dots-issue-341", "purge-dot-issue-341",
        "purge-dots-issue-341", "valid-passes-through"])
def test_a_malformed_days_setting_falls_back_to_the_documented_default(
        tmp_path, monkeypatch, line, limit, purge):
    """GH-341: a malformed retire limit raised ValueError out of main()'s idle
    cleanup (float() on the `[0-9.]+` match), and launchd restarted the
    supervisor into the same crash at every idle check. The idle cycle now runs
    on with the documented limits; the valid case guards the harness -- green
    means the seam was reached and passed real values through, not skipped."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{line}\n{repo}\n")
    clock = [1_000_000.0]

    def start(self):
        self.proc, self.started = FakeProc(), clock[0]
    cleaned = []
    monkeypatch.setattr(m, "clean_archived",
                        lambda folders, now, lim, p, pinned: cleaned.append((lim, p)))
    day = 86400
    run_main(m, monkeypatch, tmp_path, cfg, start, idle_sleep(m, clock, cleaned), clock=clock)
    assert len(cleaned) >= 2
    assert cleaned == [(limit * day, purge * day)] * len(cleaned)


def test_an_unresolvable_folder_does_not_take_the_supervisor_down_past_the_idle_check(
        tmp_path, monkeypatch):
    """GH-414: a pinned line whose folder resolve() rejects raised out of
    main() -- from parse() at the config reload and from session_entries() at
    every idle check -- so launchd restarted the supervisor into the same
    crash, and every host restarted with it. The line here carries a NUL byte,
    the one form every python rejects (the symlink loop of the issue stops
    raising on newer pythons; its exception is pinned at resolve()'s own seam
    in the unit tests). The line is skipped at both seams and the good
    folder's host runs on through the idle checks."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=profile\n{repo}\na\x00b -- --session-id session_01X\n")
    clock = [1_000_000.0]
    started = []

    def start(self):
        started.append(self.folder)
        self.proc, self.started = FakeProc(), clock[0]
    cleaned = []
    monkeypatch.setattr(m, "clean_archived",
                        lambda folders, now, lim, p, pinned: cleaned.append(folders))
    run_main(m, monkeypatch, tmp_path, cfg, start, idle_sleep(m, clock, cleaned), clock=clock)
    assert len(cleaned) == 2                # two idle checks ran, session_entries() and all
    assert [f for f in started if f != str(m.EDGES.parent)] == [str(repo)]


def test_a_config_flag_without_a_path_is_a_usage_error_not_a_crash(capsys):
    """GH-414: `main(["--config"])` raised IndexError building the config path
    (latent: the LaunchAgent passes no arguments). A supervisor asked for a
    specific config says how to name one and exits, rather than crash -- or
    silently watch the default config instead. #417 moved the parsing onto
    argparse, which exits with that code itself."""
    m = load()
    with pytest.raises(SystemExit) as done:
        m.main(["--config"])
    assert done.value.code == 2
    assert "usage" in capsys.readouterr().err


# #892: this case builds a real repository — init, commit, three locked worktrees,
# then the supervisor's own `git rev-list`/`branch` children — and on a loaded
# gate a single child start can occupy most of the one-minute ceiling (the
# 2026-11-25 night gate struck this test inside `Popen._execute_child`). The
# marker is its per-test headroom; the ceiling stays 60 s everywhere else.
@pytest.mark.timeout(300)
def test_an_archived_sessions_worktree_is_removed_after_the_limit_unless_it_holds_work(tmp_path, monkeypatch):
    import json
    import subprocess
    m = load()
    repo = tmp_path / "repo"
    git = ["git", "-C", str(repo)]
    subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(git + ["-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q",
                          "--allow-empty", "-m", "init"], check=True, timeout=HANG_TIMEOUT)
    (repo / ".claude").mkdir()
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    for sid in ("cse_01OLD", "cse_01DIRTY", "cse_01FRESH"):
        subprocess.run(git + ["worktree", "add", "-q", "--lock", "-b", f"worktree-bridge-{sid}",
                              str(repo / ".claude" / "worktrees" / f"bridge-{sid}")], check=True, timeout=HANG_TIMEOUT)
    (repo / ".claude" / "worktrees" / "bridge-cse_01DIRTY" / "work.txt").write_text("unsaved")
    day = 86400
    now = m.stamp("2026-10-10T12:00:00Z")
    archived = [{"id": "cse_01OLD", "environment_id": "env_01R", "updated_at": "2026-10-09T11:00:00Z",
                 "environment_kind": "bridge"},
                {"id": "cse_01DIRTY", "environment_id": "env_01R", "updated_at": "2026-10-09T11:00:00Z",
                 "environment_kind": "bridge"},
                {"id": "cse_01FRESH", "environment_id": "env_01R", "updated_at": "2026-10-10T01:00:00Z",
                 "environment_kind": "bridge"},
                # an old host's environment: nothing lists it, it must still age out
                {"id": "cse_01GONEHOST", "environment_id": "env_01OLD", "updated_at": "2026-10-09T11:00:00Z",
                 "environment_kind": "bridge"},
                # a cloud session is not this supervisor's to delete
                {"id": "cse_01CLOUD", "environment_id": "env_01C", "updated_at": "2026-10-01T11:00:00Z",
                 "environment_kind": "cloud"}]
    deleted = []

    def api(method, path, token, body=None):
        if method == "DELETE":
            deleted.append(path)
            return {}
        return {"data": archived} if "statuses=archived" in path else {}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    m.clean_archived([str(repo)], now, day, 7 * day)
    wts = repo / ".claude" / "worktrees"
    assert not (wts / "bridge-cse_01OLD").exists()                     # archived > 1 day: gone
    assert "worktree-bridge-cse_01OLD" not in subprocess.run(git + ["branch"], capture_output=True,
                                                             text=True, timeout=HANG_TIMEOUT).stdout
    assert (wts / "bridge-cse_01DIRTY" / "work.txt").exists()          # uncommitted work: kept
    assert (wts / "bridge-cse_01FRESH").exists()                       # archived 11 h ago: kept
    assert deleted == []                                               # none archived 7 days yet

    # Seven days on: the clean ones leave the server too; the one holding work stays
    # so it can still be resumed.
    m.clean_archived([str(repo)], now + 7 * day, day, 7 * day)
    assert deleted == ["/v1/code/sessions/cse_01OLD", "/v1/code/sessions/cse_01FRESH",
                       "/v1/code/sessions/cse_01GONEHOST"]
    assert (wts / "bridge-cse_01DIRTY" / "work.txt").exists()


def _archived_worktree_setup(tmp_path, monkeypatch, sids):
    """A repo whose HEAD is one commit behind origin/main, one bridge worktree per sid
    based on origin/main (as the bridge makes them), and a server listing them archived."""
    import json
    import subprocess
    m = load()
    repo = tmp_path / "repo"
    git = ["git", "-C", str(repo)]
    c = ["-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m"]
    subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(git + c + ["init"], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(git + c + ["upstream"], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(git + ["update-ref", "refs/remotes/origin/main", "HEAD"], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(git + ["reset", "-q", "--hard", "HEAD~1"], check=True, timeout=HANG_TIMEOUT)   # host checkout lags origin
    (repo / ".claude").mkdir()
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    for sid in sids:
        subprocess.run(git + ["worktree", "add", "-q", "--lock", "-b", f"worktree-bridge-{sid}",
                              str(repo / ".claude" / "worktrees" / f"bridge-{sid}"), "origin/main"], check=True, timeout=HANG_TIMEOUT)
    day = 86400
    now = m.stamp("2026-10-10T12:00:00Z")
    archived = [{"id": sid, "environment_id": "env_01R", "updated_at": "2026-10-09T11:00:00Z",
                 "environment_kind": "bridge"} for sid in sids]
    deleted = []

    def api(method, path, token, body=None):
        if method == "DELETE":
            deleted.append(path)
            return {}
        return {"data": archived} if "statuses=archived" in path else {}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    return m, repo, git, now, day, deleted


def test_a_lagging_host_head_does_not_keep_an_empty_archived_worktree(tmp_path, monkeypatch):
    import subprocess
    m, repo, git, now, day, deleted = _archived_worktree_setup(tmp_path, monkeypatch, ["cse_01EMPTY"])
    m.clean_archived([str(repo)], now, day, 7 * day)
    assert not (repo / ".claude" / "worktrees" / "bridge-cse_01EMPTY").exists()
    out = subprocess.run(["git", "-C", str(repo), "branch", "--list", "worktree-bridge-*"],
                         capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout
    assert "cse_01EMPTY" not in out          # `branch -d` refuses it: not merged into the lagging HEAD
    m.clean_archived([str(repo)], now + 7 * day, day, 7 * day)
    assert deleted == ["/v1/code/sessions/cse_01EMPTY"]


def test_a_file_named_head_does_not_make_the_work_check_ambiguous(tmp_path, monkeypatch, capsys):
    m, repo, git, now, day, deleted = _archived_worktree_setup(tmp_path, monkeypatch, ["cse_01HEADFILE"])
    (repo / ".claude" / "worktrees" / "bridge-cse_01HEADFILE" / "HEAD").write_text("x")
    m.clean_archived([str(repo)], now, day, 7 * day)
    assert "git could not tell" not in capsys.readouterr().out   # rev-list HEAD <branch> without `--`: ambiguous


def test_a_detached_head_commit_or_a_missing_branch_keeps_the_worktree(tmp_path, monkeypatch):
    import subprocess
    m, repo, git, now, day, deleted = _archived_worktree_setup(
        tmp_path, monkeypatch, ["cse_01DETACHED", "cse_01NOBRANCH"])
    wts = repo / ".claude" / "worktrees"
    det = wts / "bridge-cse_01DETACHED"
    subprocess.run(["git", "-C", str(det), "checkout", "-q", "--detach"], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(["git", "-C", str(det), "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q",
                    "--allow-empty", "-m", "only copy"], check=True, timeout=HANG_TIMEOUT)
    nob = wts / "bridge-cse_01NOBRANCH"
    subprocess.run(["git", "-C", str(nob), "checkout", "-q", "--detach"], check=True, timeout=HANG_TIMEOUT)
    subprocess.run(git + ["branch", "-D", "worktree-bridge-cse_01NOBRANCH"], check=True, timeout=HANG_TIMEOUT)
    m.clean_archived([str(repo)], now, day, 7 * day)
    m.clean_archived([str(repo)], now + 30 * day, day, 7 * day)
    assert det.exists() and nob.exists()
    assert deleted == []


def test_work_a_worktree_holds_but_head_does_not_survives_clean_and_purge(tmp_path, monkeypatch):
    """A clean tree can still be the only copy of the work: a commit of its own.

    `git worktree remove` only refuses a dirty or locked worktree, and `git branch
    -d` says "not merged" long after the worktree holding it is gone -- so the
    commit was reported and lost. A session whose host re-registered is on an
    environment nothing lists any more, and its worktree is the only evidence left.
    """
    import json
    import subprocess
    m = load()
    repo = tmp_path / "repo"
    git = ["git", "-C", str(repo)]

    def commit(cwd, msg):
        subprocess.run(["git", "-C", str(cwd), "-c", "user.email=a@b", "-c", "user.name=a",
                        "commit", "-q", "--allow-empty", "-m", msg], check=True, timeout=HANG_TIMEOUT)

    subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True, timeout=HANG_TIMEOUT)
    commit(repo, "init")
    (repo / ".claude").mkdir()
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    wts = repo / ".claude" / "worktrees"
    for sid in ("cse_01UNMERGED", "cse_01CLEANER", "cse_01STALE"):
        subprocess.run(git + ["worktree", "add", "-q", "--lock", "-b", f"worktree-bridge-{sid}",
                              str(wts / f"bridge-{sid}")], check=True, timeout=HANG_TIMEOUT)
    # A clean tree whose branch carries a commit HEAD does not have.
    wt = wts / "bridge-cse_01UNMERGED"
    (wt / "answer.txt").write_text("42")
    subprocess.run(["git", "-C", str(wt), "add", "-A"], check=True, timeout=HANG_TIMEOUT)
    commit(wt, "the only copy of the work")
    assert subprocess.run(git + ["rev-list", "--count", "HEAD..worktree-bridge-cse_01UNMERGED"],
                          capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout.strip() == "1"
    day = 86400
    now = m.stamp("2026-10-10T12:00:00Z")
    old = {"updated_at": "2026-10-09T11:00:00Z", "environment_kind": "bridge"}
    archived = [{"id": "cse_01UNMERGED", "environment_id": "env_01R", **old},
                {"id": "cse_01CLEANER", "environment_id": "env_01R", **old},
                # a host that re-registered: its environment is configured nowhere
                {"id": "cse_01STALE", "environment_id": "env_01STA", **old}]
    deleted = []

    def api(method, path, token, body=None):
        if method == "DELETE":
            deleted.append(path)
            return {}
        return {"data": archived} if "statuses=archived" in path else {}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    branches = lambda: subprocess.run(git + ["branch"], capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout

    m.clean_archived([str(repo)], now, day, 7 * day)
    assert (wt / "answer.txt").exists()                          # its branch is unmerged: kept
    assert "worktree-bridge-cse_01UNMERGED" in branches()
    assert not (wts / "bridge-cse_01CLEANER").exists()            # a clean, fully merged worktree still goes
    assert "worktree-bridge-cse_01CLEANER" not in branches()
    assert (wts / "bridge-cse_01STALE").exists()                  # no host lists it; only the disk knows
    assert deleted == []                                          # none archived 7 days yet

    m.clean_archived([str(repo)], now + 30 * day, day, 7 * day)
    assert (wt / "answer.txt").exists()                           # still there a month on
    assert (wts / "bridge-cse_01STALE").exists()
    assert deleted == ["/v1/code/sessions/cse_01CLEANER"]         # only the empty one left the server


def test_a_record_with_no_usable_timestamp_is_skipped_not_fatal(tmp_path, monkeypatch):
    """One bad record must not take the supervisor down (2026-10-14).

    `stamp()` is `datetime.fromisoformat`, so a missing, empty or null `updated_at`
    raises out of clean_archived/purge_archived, both called bare from main():
    launchd restarts the supervisor into the same crash every idle check, stopping
    and restarting every host and interrupting phone sessions. A bad record is
    skipped with a log line; the pass keeps going for the others.
    """
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    day = 86400
    now = m.stamp("2026-10-10T12:00:00Z")
    old = "2026-10-09T11:00:00Z"
    archived = [{"id": "cse_01MISSING", "environment_id": "env_01R", "environment_kind": "bridge"},
                {"id": "cse_01EMPTY", "environment_id": "env_01R", "environment_kind": "bridge",
                 "updated_at": ""},
                {"id": "cse_01NULL", "environment_id": "env_01R", "environment_kind": "bridge",
                 "updated_at": None},
                {"id": "cse_01JUNK", "environment_id": "env_01R", "environment_kind": "bridge",
                 "updated_at": "not-a-timestamp"},
                {"id": "cse_01GOOD", "environment_id": "env_01R", "environment_kind": "bridge",
                 "updated_at": old}]
    deleted = []

    def api(method, path, token, body=None):
        if method == "DELETE":
            deleted.append(path)
            return {}
        return {"data": archived} if "statuses=archived" in path else {}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    m.clean_archived([str(repo)], now, day, 7 * day)             # must not raise
    assert deleted == []                                         # none archived 7 days yet
    m.clean_archived([str(repo)], now + 30 * day, day, 7 * day)  # the bad four stay out of the way
    assert deleted == ["/v1/code/sessions/cse_01GOOD"]


def test_the_busy_state_write_survives_a_missing_state_dir(tmp_path, monkeypatch):
    """`BUSY.write_text` raised FileNotFoundError when .rc-bridge was absent."""
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "profiles" / ".rc-bridge" / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    s = {"id": "cse_01BUSY", "environment_id": "env_01R", "connection_status": "connected",
         "worker_status": "running"}
    seen = []

    def api(method, path, token, body=None):
        if method == "POST":
            seen.append(path)
            return {}
        page = {"data": [s]} if "statuses=active" in path else {}
        return page
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    m.recover_interrupted([str(repo)], {str(repo)})             # must not raise
    assert json.loads(m.BUSY.read_text()) == {"cse_01BUSY": "busy"}


def test_a_session_cut_off_mid_turn_is_handed_back_once(tmp_path, monkeypatch):
    """A host restart kills its sessions; the restarted host adopts none (2026-10-10)."""
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    s = {"id": "cse_01BUSY", "environment_id": "env_01R", "connection_status": "connected",
         "worker_status": "running"}
    idle = {"id": "cse_01IDLE", "environment_id": "env_01R", "connection_status": "connected",
            "worker_status": "idle"}
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append((path, body))
            return {}
        return {"data": [s, idle]}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    up = {str(repo)}
    m.recover_interrupted([str(repo)], up)
    assert posts == []
    m = load()                              # a supervisor restart forgets nothing
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    s["connection_status"] = idle["connection_status"] = "disconnected"
    m.recover_interrupted([str(repo)], set())
    m.recover_interrupted([str(repo)], set())
    assert posts == []                      # its host is not back yet: nothing to hand it to
    m.recover_interrupted([str(repo)], up)
    assert posts == [("/v1/environments/env_01R/bridge/reconnect", {"session_id": "cse_01BUSY"})]
    m.recover_interrupted([str(repo)], up)
    s["connection_status"] = "connected"
    m.recover_interrupted([str(repo)], up)
    s["connection_status"] = "disconnected"
    m.recover_interrupted([str(repo)], up)
    assert len(posts) == 1                  # crashed again after its re-queue: left to the phone
    s["connection_status"], s["worker_status"] = "connected", "idle"
    m.recover_interrupted([str(repo)], up)
    s["worker_status"] = "running"
    m.recover_interrupted([str(repo)], up)
    s["connection_status"] = "disconnected"
    m.recover_interrupted([str(repo)], up)
    assert len(posts) == 1                  # one disconnected check is a network blip
    s["connection_status"] = "connected"
    m.recover_interrupted([str(repo)], up)
    s["connection_status"] = "disconnected"
    m.recover_interrupted([str(repo)], up)
    assert len(posts) == 1                  # ...and riding it out starts the count over
    m.recover_interrupted([str(repo)], up)
    assert len(posts) == 2                  # a new turn, cut off again: handed back again


def test_a_repo_host_restart_keeps_its_session_debug_logs(tmp_path, monkeypatch):
    """A repo host has one .debug-<id>.log per phone session; a relaunch deleted the
    logs of the sessions that had just failed (2026-10-14). Pinned hosts still sweep."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    crash = logs / f"{h.debugfile.stem}-cse_01CRASH.log"
    crash.write_text("x")
    h.start()
    assert crash.exists()

    pinned = m.Host(str(repo), "gw", ("--session-id", "session_01X"))
    stale = logs / f"{pinned.debugfile.stem}-cse_01OLD.log"
    stale.write_text("x")
    pinned.start()
    assert not stale.exists()


def test_a_repo_host_rotates_an_oversized_debug_log_at_start(tmp_path, monkeypatch):
    """`claude --debug-file` appends, so a repo host's <name>.debug.log grew without
    bound; the main log was already rotated at 5 MB (#64)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    h.debugfile.write_bytes(b"x" * 5_000_001)
    h.start()
    assert not h.debugfile.exists()
    assert h.debugfile.with_suffix(".log.1").exists()


def test_a_running_repo_host_log_is_truncated_in_place_past_the_cap(tmp_path, monkeypatch):
    """The debug logs grew unbounded between starts (#128): past the cap the
    running host's logs are copied to .log.1 and truncated, keeping the inode."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    h = m.Host(str(tmp_path / "repo"), "gw")
    child = logs / f"{h.debugfile.stem}-cse_01A.log"
    for f in (h.logfile, h.debugfile, child):
        f.write_bytes(b"x" * (m.LOG_CAP + 1))
    ino = h.debugfile.stat().st_ino
    h.bound_logs()
    assert h.debugfile.stat().st_size == 0 and h.debugfile.stat().st_ino == ino
    assert child.stat().st_size == 0
    assert h.logfile.stat().st_size == 0     # the host's own log reached 300 MB uncapped
    assert h.debugfile.with_suffix(".log.1").stat().st_size == m.LOG_CAP + 1


def test_a_pinned_host_does_not_rotate_its_debug_log_into_a_leftover(tmp_path, monkeypatch):
    """A pinned host unlinks its debug log at start; rotating it first left a
    .log.1 behind that nothing ever removed."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw", ("--session-id", "session_01X"))
    h.debugfile.write_bytes(b"x" * 5_000_001)
    h.start()
    assert not h.debugfile.exists()
    assert not h.debugfile.with_suffix(".log.1").exists()


def test_purging_a_session_removes_its_debug_log(tmp_path, monkeypatch):
    """A repo host keeps one .debug-<sid>.log per phone session and nothing removed
    them when the session was purged (#64)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    gone = logs / "repo.debug-cse_01OLD.log"
    kept = logs / "repo.debug-cse_01NEW.log"
    gone.write_text("x")
    kept.write_text("x")
    old = {"environment_kind": "bridge", "environment_id": "env_01X",
           "updated_at": "2026-10-01T00:00:00Z"}
    found = [{"id": "cse_01OLD", **old}]
    monkeypatch.setattr(m, "api", lambda method, path, token, body=None: {})
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "sessions", lambda token, status: found)
    monkeypatch.setattr(m, "host_envs", lambda folders: {})
    m.purge_archived([], m.stamp("2026-10-10T12:00:00Z"), 86400)
    assert not gone.exists()
    assert kept.exists()


def test_purging_a_session_removes_its_rotated_debug_log(tmp_path, monkeypatch):
    """bound_logs() leaves <name>.debug-<sid>.log.1 behind; purge removed only the
    live log (#141)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    h = m.Host(str(tmp_path / "repo"), "gw")
    live = logs / "repo.debug-cse_01OLD.log"
    live.write_bytes(b"x" * (m.LOG_CAP + 1))
    monkeypatch.setattr(h, "debug_logs", lambda: [live])
    h.bound_logs()
    assert live.with_suffix(".log.1").exists()
    found = [{"id": "cse_01OLD", "environment_kind": "bridge", "environment_id": "env_01X",
              "updated_at": "2026-10-01T00:00:00Z"}]
    monkeypatch.setattr(m, "api", lambda method, path, token, body=None: {})
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "sessions", lambda token, status: found)
    monkeypatch.setattr(m, "host_envs", lambda folders: {})
    m.purge_archived([], m.stamp("2026-10-10T12:00:00Z"), 86400)
    assert not list(logs.glob("*cse_01OLD*"))


def test_purging_a_session_removes_its_bridge_transcript(tmp_path, monkeypatch):
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    gone = logs / "bridge-transcript-cse_01OLD.jsonl"
    kept = logs / "bridge-transcript-cse_01NEW.jsonl"
    gone.write_text("x")
    kept.write_text("x")
    found = [{"id": "cse_01OLD", "environment_kind": "bridge", "environment_id": "env_01X",
              "updated_at": "2026-10-01T00:00:00Z"}]
    monkeypatch.setattr(m, "api", lambda method, path, token, body=None: {})
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "sessions", lambda token, status: found)
    monkeypatch.setattr(m, "host_envs", lambda folders: {})
    m.purge_archived([], m.stamp("2026-10-10T12:00:00Z"), 86400)
    assert not gone.exists()
    assert kept.exists()


def test_a_start_removes_session_records_past_retention(tmp_path, monkeypatch):
    """Unbounded session logs filled the internal disk (239 files, 2.8 GB on 2026-11-21)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    old = logs / "bridge-transcript-cse_01OLD.jsonl"
    kept = logs / "bridge-transcript-cse_01NEW.jsonl"
    old.write_text("x")
    kept.write_text("x")
    stale = time.time() - (m.LOG_KEEP_DAYS + 1) * 86400
    os.utime(old, (stale, stale))
    m.sweep_logs()
    assert not old.exists()
    assert kept.exists()


def _ps_table(monkeypatch, m, table, code=0, argv=None):
    """ps answers `table` with exit `code`, for every subprocess.run in m. A
    `pid=,command=` read gives each line a pid, as ps does: its own leading one,
    else 1000 plus the line's index; a `-o command=` read gets the lines as
    written. proc_argv() answers from `argv` ({pid: [arguments]}) and None for
    any other pid, so no test reads a real process's argv by a made-up pid."""
    import subprocess
    numbered = "".join(
        (line if not line.strip() or line.split()[0].isdigit() else f"{1000 + i} {line}") + "\n"
        for i, line in enumerate(table.splitlines()))

    def run(cmd, **kw):
        out = numbered if any("pid=" in str(c) for c in cmd) else table
        return subprocess.CompletedProcess(cmd, code, stdout=out, stderr="")

    monkeypatch.setattr(m.subprocess, "run", run)
    monkeypatch.setattr(m, "proc_argv", lambda pid: list((argv or {}).get(pid, [])) or None,
                        raising=False)


def _procs(monkeypatch, m, *argvs):
    """A process table of real argv lists: each listed by ps as its arguments
    joined with one space (as ps prints them), under _ps_table()'s pids 1000
    and up, and proc_argv() answering each pid with its own arguments, whole.
    Returns the pids, in order."""
    table = {1000 + i: list(a) for i, a in enumerate(argvs)}
    _ps_table(monkeypatch, m, "".join(f"{' '.join(a)}\n" for a in argvs), argv=table)
    return list(table)


def test_live_sessions_reads_session_children_and_pins(monkeypatch):
    m = load()
    claude = "/Users/a b/.local/bin/claude"          # the claude the hosts run, under a home with a space
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: claude if name == "claude" else None)
    rc = m.HERE / "ccmorph-rc"
    table = ["/x/claude --print --session-id cse_01KID --sdk-url wss://x",
             "/Users/me/.local/bin/claude remote-control --session-id session_01PIN --debug-file=/x/p.debug.log",
             f"{claude} remote-control --session-id session_01SPACED --debug-file=/x/s.debug.log",
             f"/usr/bin/script -q /dev/null {rc} gw -- --session-id session_01WRAP --debug-file=/x/w.debug.log",
             f"/usr/bin/script -q /dev/null /bin/sh {rc} gw -- --session-id cse_01SPOOF",
             "/usr/bin/script -q /dev/null /bin/sh /x/ccmorph-rc --session-id cse_01SPOOF2",
             f"/bin/echo {claude} remote-control --session-id cse_01ECHO1",
             "/bin/printf %s --session-id cse_01OLD",
             "/bin/echo remote-control --session-id cse_01ECHO",
             "/usr/bin/vim notes.txt"]
    # a session child (--sdk-url, as session_pid reads it), a pinned host (as
    # host_at reads one, or by the exact claude the hosts run) and the pty
    # start() runs a host in (by the exact command start() builds) prove a
    # session; a bare --session-id in any other command does not, nor does a
    # script row running /bin/sh that merely carries the wrapper's path
    assert m.live_sessions(table) == {"01KID", "01PIN", "01SPACED", "01WRAP"}


def test_process_table_keeps_rows_whole_and_refuses_a_failed_ps(monkeypatch):
    import subprocess
    m = load()
    _ps_table(monkeypatch, m, "  /usr/bin/script -q /dev/null /a b/ccmorph-rc gw\n\n 77 /usr/bin/vim x\n")
    rows = m.process_table()
    assert rows == ["/usr/bin/script -q /dev/null /a b/ccmorph-rc gw", "/usr/bin/vim x"]
    assert [r.pid for r in rows] == [1000, 77]      # each row keeps the pid ps listed it under
    _ps_table(monkeypatch, m, "/x/claude --session-id cse_01KID --sdk-url wss://x\n", code=1)
    assert m.process_table() is None          # a nonzero exit is unread, partial rows or not
    _ps_table(monkeypatch, m, "")
    assert m.process_table() is None
    monkeypatch.setattr(m.subprocess, "run", lambda cmd, **kw: subprocess.CompletedProcess(
        cmd, 0, stdout="/usr/bin/vim x\n", stderr=""))
    assert m.process_table() is None          # a line with no pid in front is not the table asked for
    monkeypatch.setattr(m.subprocess, "run", lambda cmd, **kw: (_ for _ in ()).throw(OSError("no ps")))
    assert m.process_table() is None


def test_record_session_names_the_session_a_log_file_records():
    m = load()
    assert m.record_session("bridge-transcript-cse_01A.jsonl") == "01A"
    assert m.record_session("bridge-transcript-session_01A.jsonl") == "01A"
    assert m.record_session("Users-me-repo.debug-cse_01A.log") == "01A"
    assert m.record_session("Users-me-repo.debug-cse_01A.log.1") == "01A"   # bound_logs()'s copy
    for name in ("Users-me-repo.log", "Users-me-repo.debug.log", "Users-me-repo.log.1",
                 "Users-me-repo.debug.log.1", "edge-gw.log", "adapter-gw.log", "switch.log"):
        assert m.record_session(name) is None


def _idle_check_due(m):
    """A Clock whose idle check is due now and whose recovery step is not."""
    state = m.Clock()
    now = state.last_idle_check + m.IDLE_CHECK + 1
    state.last_recover = now
    return state, now


def _age_past_retention(m, *files):
    stale = time.time() - (m.LOG_KEEP_DAYS + 1) * 86400
    for f in files:
        os.utime(f, (stale, stale))


def test_a_running_supervisor_removes_session_records_past_retention(tmp_path, monkeypatch):
    """#758: a sweep only at supervisor start let the log dir refill between
    restarts -- launchd KeepAlive never restarts a healthy supervisor. The age
    cap must ride the same idle-check path as the size cap (bound_logs), so the
    test drives cycle(), not sweep_logs()."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    old = logs / "bridge-transcript-cse_01OLD.jsonl"
    kept = logs / "bridge-transcript-cse_01NEW.jsonl"
    old.write_text("x")
    kept.write_text("x")
    _age_past_retention(m, old)
    _ps_table(monkeypatch, m, "/usr/bin/vim notes.txt\n")   # ps answers; no session runs
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    assert not old.exists()
    assert kept.exists()


def test_on_a_running_host_the_periodic_sweep_removes_only_dead_sessions_records(tmp_path, monkeypatch):
    """#758: a running host's own stdout log and --debug-file, and the records of
    a session some process still serves, survive the sweep however quiet: the
    writer holds them open and bound_logs() must still cap them by path. A dead
    session's records go, its .log.1 copy too, even on a host that is still up
    -- a host outlives its sessions, and holding them by host let a healthy host
    refill the dir."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    _ps_table(monkeypatch, m, "/x/claude --print --session-id cse_01LIVE --sdk-url wss://x\n")
    hosts = _running_host(m, tmp_path / "repo", [time.time()])
    h = next(iter(hosts.values()))
    pin = m.Host(str(tmp_path / "pin"), "gw", ("--session-id", "session_01PIN"))
    pin.proc = FakeProc()
    hosts[pin.folder] = pin
    stem = h.debugfile.stem
    held = [h.logfile, h.debugfile, h.logfile.with_suffix(".log.1"),
            logs / f"{stem}-cse_01LIVE.log", logs / "bridge-transcript-cse_01LIVE.jsonl",
            pin.logfile, logs / "bridge-transcript-session_01PIN.jsonl",
            logs / f"{pin.debugfile.stem}-cse_01PIN.log"]
    dead = [logs / f"{stem}-cse_01DEAD.log", logs / f"{stem}-cse_01DEAD.log.1",
            logs / "bridge-transcript-cse_01DEAD.jsonl"]
    fresh = logs / f"{stem}-cse_01DONE.log"
    for f in (*held, *dead, fresh):
        f.write_text("x")
    _age_past_retention(m, *held, *dead)
    state, now = _idle_check_due(m)
    m.cycle(hosts, [], now, state)
    assert [f.name for f in held if not f.exists()] == []    # still written, or a host's: kept
    assert [f.name for f in dead if f.exists()] == []        # same host, session over: gone
    assert fresh.exists()                                    # inside the week: kept


@pytest.mark.parametrize("managed", [True, False], ids=["beside-a-managed-host", "alone"])
@pytest.mark.parametrize("row", [
    "",
    "/Users/me/.local/bin/claude remote-control --session-id session_01PIN --debug-file={debug}",
    "/usr/bin/script -q /dev/null {rc} gw -- --session-id session_01PIN --debug-file={debug}",
], ids=["no-row", "claude-row", "pty-row"])
def test_the_periodic_sweep_keeps_a_pin_an_earlier_supervisor_started(tmp_path, monkeypatch, row, managed):
    """#758: a pinned host outlives the supervisor that started it, and the one
    after holds no Popen for it -- start() only waits on its live pointer. Its
    records are a live session's all the same: the config names its session, so
    the sweep keeps them whatever ps shows of the pin (no-row: nothing), while a
    dead session's records still go."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    pin = m.Host(str(tmp_path / "pin"), "gw", ("--session-id", "session_01PIN"))   # proc None
    hosts = {pin.folder: pin}
    if managed:
        hosts.update(_running_host(m, tmp_path / "repo", [time.time()]))
    _ps_table(monkeypatch, m, row.format(debug=pin.debugfile, rc=m.HERE / "ccmorph-rc")
              + "\n/bin/printf %s --session-id cse_01DEAD\n")
    held = [logs / "bridge-transcript-session_01PIN.jsonl",
            logs / f"{pin.debugfile.stem}-cse_01PIN.log", pin.logfile, pin.debugfile]
    dead = logs / "bridge-transcript-cse_01DEAD.jsonl"
    for f in (*held, dead):
        f.write_text("x")
    _age_past_retention(m, *held, dead)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert [f.name for f in held if not f.exists()] == []
    assert not dead.exists()


def test_the_periodic_sweep_leaves_a_log_while_its_writer_names_it(tmp_path, monkeypatch):
    """#758: a host's own logs, an edge's and an adapter's are one per folder or
    route rather than one per session, and unlinking one a live process writes
    orphans its descriptor. They are held while a writer may run: a host the
    config still lists (running or adopted -- no Popen here), an edge Host, the
    adapter of a configured route or of a ps row that starts with the adapter
    ccmorph-rc launches (a --switch profile's). What no
    writer names any more -- an adapter no row runs and no route names, or one
    an unrelated command merely mentions -- ages out."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    adapter = m.rc_module().ADAPTER
    _ps_table(monkeypatch, m,
              f"{adapter} adapter ds --auto --parent-pid 1\n"
              f"/bin/echo {adapter} adapter old --auto --parent-pid 1\n")
    h = m.Host(str(tmp_path / "repo"), "gw")      # served by an earlier supervisor's host: proc None
    edge = m.Host(str(tmp_path / "edge"), "gw")
    edge.edge, edge.logfile = True, logs / "edge-gw.log"
    hosts = {h.folder: h, "edge:gw": edge}
    stem = h.debugfile.stem
    held = [h.logfile, h.debugfile, h.logfile.with_suffix(".log.1"), h.debugfile.with_suffix(".log.1"),
            logs / "edge-gw.log", logs / "adapter-gw.log", logs / "adapter-ds.log"]
    dead = [logs / f"{stem}-cse_01DEAD.log", logs / f"{stem}-cse_01DEAD.log.1",
            logs / "bridge-transcript-cse_01DEAD.jsonl", logs / "adapter-old.log",
            logs / "switch.log"]      # no --switch child runs: not held
    for f in (*held, *dead):
        f.write_text("x")
    _age_past_retention(m, *held, *dead)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert [f.name for f in held if not f.exists()] == []
    assert [f.name for f in dead if f.exists()] == []


@pytest.mark.parametrize("row", [
    "python3 {rc} ds --switch",
    "/usr/bin/python3 {rc} ds --switch",
    "{rc} ds --switch",
    "/Library/Frameworks/Python.framework/Versions/3.9/Resources/Python.app/Contents/MacOS/Python {rc} ds --switch",
    "/Applications/Xcode App/Python.app/Contents/MacOS/Python {rc} ds --switch",
    "/bin/sh /usr/bin/python3 {rc} ds --switch",
    "/bin/echo {rc} ds --switch",
    "",
], ids=["env-python", "abs-python", "direct", "apple-python", "spaced-apple-python",
        "shell-carrying-python", "echo-row", "no-child"])
def test_the_periodic_sweep_reads_no_switch_writer_from_ps(tmp_path, monkeypatch, row):
    """#758: ps flattens argv, so a `--switch` child's interpreter cannot be told
    from the same words in another command's arguments (`/bin/sh /usr/bin/python3
    <add-on>/ccmorph-rc ds --switch` kept an aged switch.log -- the gate's
    refusal). No row holds switch.log: an aged one goes whatever ps shows, and
    switch_log()'s stamp is what protects a live child's (the next test)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    _ps_table(monkeypatch, m, row.format(rc=m.rc_module().HERE / "ccmorph-rc") + "\n/usr/bin/vim x\n")
    switch = logs / "switch.log"
    switch.write_text("x")
    _age_past_retention(m, switch)
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    assert not switch.exists()


@pytest.mark.parametrize("launched", [True, False], ids=["launched", "launch-refused"])
def test_a_switch_log_handed_to_a_child_outlives_the_sweep_and_then_ages_out(tmp_path, monkeypatch, launched):
    """#758: start_switch() stamps switch.log once the `--switch` child runs: an
    old file handed to a new child is a week young, so the sweep leaves it while
    the short-lived child writes through its descriptor, and it ages out like any
    other once nothing hands it on. A launch that raises (the launcher missing,
    retried every tick) starts no writer and stamps nothing, so repeated
    refusals never keep an aged log (the gate's refusal)."""
    import subprocess
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    _ps_table(monkeypatch, m, "/usr/bin/vim x\n")
    switch = logs / "switch.log"
    switch.write_text("old\n")
    _age_past_retention(m, switch)
    children = []

    def popen(cmd, **kw):
        if not launched:
            raise FileNotFoundError(2, "No such file or directory", cmd[0])
        children.append(os.fdopen(os.dup(kw["stderr"].fileno()), "a"))   # the child's stderr
        return FakeProc()

    monkeypatch.setattr(m.subprocess, "Popen", popen)
    for _ in range(2):
        if launched:
            m.start_switch("ds")
        else:
            with pytest.raises(OSError):
                m.start_switch("ds")
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    if not launched:
        assert not switch.exists()                  # no writer was ever started
        return
    assert switch.exists()
    children[-1].write("new\n")
    for child in children:
        child.close()
    assert switch.read_text() == "old\nnew\n"      # appended, through the path the sweep left
    _age_past_retention(m, switch)
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    assert not switch.exists()


def test_main_stamps_the_switch_log_it_hands_a_bring_up(tmp_path, monkeypatch):
    """#758: the stamp must happen where main() hands the log over -- an old
    switch.log opened for append keeps its old mtime, and the sweep then unlinks
    it under the child's open descriptor."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\ninference=oli\n")
    (tmp_path / "logs").mkdir()
    switch = tmp_path / "logs" / "switch.log"
    switch.write_text("old\n")
    before = os.stat(switch).st_mtime - 2 * 86400     # inside the week: the boot sweep keeps it
    os.utime(switch, (before, before))
    popens = []
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: popens.append((cmd, kw)) or FakeProc())

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        if self.edge:
            _live_edge(m, self.route, 500)     # a bring-up waits for a live shared edge (#595)

    run_main(m, monkeypatch, tmp_path, cfg, start)
    assert [c for c, _ in popens] == [[str(m.HERE / "ccmorph-rc"), "oli", "--switch"]]
    assert Path(popens[0][1]["stderr"].name) == switch
    assert os.stat(switch).st_mtime > before + 86400
    assert switch.read_text() == "old\n"


def _claude_store(tmp_path, monkeypatch, m):
    """A native claude install under a home with a space: bin/claude links into
    a store of versions, as ~/.local/bin/claude does."""
    store = tmp_path / "home dir" / ".local" / "share" / "claude" / "versions"
    store.mkdir(parents=True)
    for v in ("2.1.329", "2.1.330"):
        (store / v).write_text("")
    link = tmp_path / "home dir" / ".local" / "bin" / "claude"
    link.parent.mkdir(parents=True)
    link.symlink_to(store / "2.1.330")
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: str(link) if name == "claude" else None)
    return store, link


def test_a_session_child_is_read_only_from_the_claude_that_runs_it(tmp_path, monkeypatch):
    """#758: `--session-id` beside `--sdk-url` is a session child only when the
    command is claude: argv[0] named claude, the claude the hosts run by its PATH
    name or resolved file (whole-row prefix, so a home with a space still
    matches), or a file straight in that link's store of versions (a child an
    update left on the previous version). The same flags in another command's
    arguments prove nothing -- for the sweep, and for session_pid(), whose pid
    gets a SIGTERM."""
    m = load()
    store, link = _claude_store(tmp_path, monkeypatch, m)
    rows = [f"{store}/2.1.330 --print --session-id cse_01NEW --sdk-url wss://x",
            f"{store}/2.1.329 --print --session-id cse_01PREV --sdk-url wss://x",
            f"{link} --print --session-id cse_01LINK --sdk-url wss://x",
            "/x/claude --print --session-id cse_01NAMED --sdk-url wss://x",
            "/bin/sh -c sleep --session-id cse_01SH --sdk-url wss://x",
            f"/bin/sh {store}/2.1.330 --session-id cse_01ARG --sdk-url wss://x",
            f"/bin/echo /x/claude --session-id cse_01ECHO --sdk-url wss://x",
            f"{store}/sub/tool --session-id cse_01SUB --sdk-url wss://x"]
    assert m.live_sessions(rows) == {"01NEW", "01PREV", "01LINK", "01NAMED"}
    _ps_table(monkeypatch, m, "".join(f"{40 + i} {r}\n" for i, r in enumerate(rows)))
    assert m.session_pid("cse_01NEW") == 40
    assert m.session_pid("cse_01PREV") == 41
    for sid in ("cse_01SH", "cse_01ARG", "cse_01ECHO", "cse_01SUB"):
        assert m.session_pid(sid) is None


def _npm_claude(tmp_path, monkeypatch, m):
    """An npm install under a home with a space: bin/claude links to the
    package's cli.js (`#!/usr/bin/env node`), and node sits on PATH under a
    path with a space too."""
    home = tmp_path / "home dir"
    cli = home / "lib" / "node_modules" / "@anthropic-ai" / "claude-code" / "cli.js"
    cli.parent.mkdir(parents=True)
    cli.write_text("")
    link = home / "bin" / "claude"
    link.parent.mkdir(parents=True)
    link.symlink_to(cli)
    node = tmp_path / "node js" / "bin" / "node"
    paths = {"claude": str(link), "node": str(node)}
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: paths.get(name))
    return cli, link, node


def test_an_npm_installed_claude_is_read_through_node(tmp_path, monkeypatch):
    """#758 (merge-gate finding): an npm install runs claude as node's script --
    `node <cli.js> --print --session-id ... --sdk-url ...` -- and reading claude
    only as argv[0] lost the live child: the sweep unlinked its records and
    session_pid() could not find it to park. node is the row's own first word
    named node, or the node on PATH (a path with a space); its script is the
    kernel's argv[1] for the row's pid, the same file as the claude the hosts
    run, by PATH name (the shebang's form), resolved cli.js or another path to
    it. The same words after another command prove nothing, for the sweep, for
    session_pid() and for host_at(), whose "host" lets stop_orphan() signal; and
    ps's text alone, with no argv to read, makes no node row claude's."""
    m = load()
    cli, link, node = _npm_claude(tmp_path, monkeypatch, m)
    alias = tmp_path / "alias dir"                 # another path to the same cli.js, as
    alias.symlink_to(cli.parents[3])               # macOS's /var -> /private/var gives
    aliased = alias / cli.relative_to(cli.parents[3])
    argvs = [["/usr/bin/node", str(cli), "--print", "--session-id", "cse_01CLI", "--sdk-url", "wss://x"],
             ["/usr/bin/node", str(aliased), "--print", "--session-id", "cse_01ALIAS", "--sdk-url", "wss://x"],
             ["node", str(link), "--print", "--session-id", "cse_01SHEBANG", "--sdk-url", "wss://x"],
             [str(node), str(cli), "--print", "--session-id", "cse_01SPACED", "--sdk-url", "wss://x"],
             ["/usr/bin/node", str(cli), "remote-control", "--session-id", "session_01PIN"],
             ["/bin/sh", "/usr/bin/node", str(cli), "--session-id", "cse_01SH", "--sdk-url", "wss://x"],
             ["/bin/echo", "node", str(cli), "--session-id", "cse_01ECHO", "--sdk-url", "wss://x"],
             ["/usr/bin/node", "/x/other.js", str(cli), "--session-id", "cse_01ARG", "--sdk-url", "wss://x"],
             ["/usr/bin/python3", str(cli), "--session-id", "cse_01PY", "--sdk-url", "wss://x"]]
    pids = _procs(monkeypatch, m, *argvs)
    assert m.live_sessions(m.process_table()) == {"01CLI", "01ALIAS", "01SHEBANG", "01SPACED", "01PIN"}
    assert [m.session_pid(s) for s in ("cse_01CLI", "cse_01ALIAS", "cse_01SHEBANG", "cse_01SPACED")] \
        == pids[:4]
    for sid in ("cse_01SH", "cse_01ECHO", "cse_01ARG", "cse_01PY"):
        assert m.session_pid(sid) is None
    assert m.live_sessions([" ".join(a) for a in argvs]) == set()    # rows with no pid to read
    _ps_table(monkeypatch, m, "".join(f"{' '.join(a)}\n" for a in argvs))
    assert m.live_sessions(m.process_table()) == set()               # pids whose argv reads None
    assert m.session_pid("cse_01CLI") is None
    for argv, held in ((["/usr/bin/node", str(cli), "remote-control", "--session-id", "session_01PIN"], "host"),
                       ([str(node), str(link), "remote-control"], "host"),
                       (["/bin/sh", "/usr/bin/node", str(cli), "remote-control"], "gone"),
                       (["/usr/bin/node", str(cli), "--print"], "gone"),
                       (["/usr/bin/node", "/x/other.js", str(cli), "remote-control"], "gone")):
        _ps_table(monkeypatch, m, " ".join(argv) + "\n", argv={4242: argv})
        assert m.host_at(4242) == held, argv
    _ps_table(monkeypatch, m, f"/usr/bin/node {cli} remote-control\n")
    assert m.host_at(4242) == "unknown"       # node runs there; with no argv, which script is unknown
    # host_at() decides on one argv read (merge-gate finding): a first read that
    # failed and a second that answered must never add up to "gone", which
    # stop_orphan() takes as a stop and restart_logged_out() records
    for later, held in ((["/usr/bin/node", str(cli), "remote-control"], "unknown"),
                        (["/usr/bin/node", "/x/other.js"], "unknown"),
                        (None, "unknown")):
        reads = iter([None] + [later] * 3)
        monkeypatch.setattr(m, "proc_argv", lambda pid: next(reads))
        assert m.host_at(4242) == held, later
    reads = iter([["/usr/bin/node", str(cli), "remote-control"], None, None])
    monkeypatch.setattr(m, "proc_argv", lambda pid: next(reads))
    assert m.host_at(4242) == "host"          # a read that answered decides, whatever a later one says


@pytest.mark.parametrize("dirs", [
    ["a b"],
    [" " * 32],
    [" " * 33],
    [" " * 40] * 5,
], ids=["one-space", "32-spaces", "33-spaces", "200-spaces"])
def test_an_npm_claude_under_a_path_with_any_number_of_spaces_is_read(tmp_path, monkeypatch, dirs):
    """#758 (merge-gate finding): node's script was found in ps's text, first
    only up to its 32nd space, so a cli.js under a path with more spaces was not
    claude: the sweep unlinked its live child's aged records and session_pid()
    could not find it. host_at() split ps's row into words and joined them
    again, so a run of spaces became one and a live pin read "gone". The script
    is the kernel's argv[1], whole however many spaces it holds, and host_at()
    reads the row whole; the same words after another command still prove
    nothing."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    cli = tmp_path.joinpath(*dirs) / "cli.js"
    cli.parent.mkdir(parents=True)
    cli.write_text("")
    link = tmp_path / "claude"
    link.symlink_to(cli)
    paths = {"claude": str(link), "node": "/usr/bin/node"}
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: paths.get(name))
    child = ["/usr/bin/node", str(cli), "--print", "--session-id", "cse_01LIVE", "--sdk-url", "wss://x"]
    shell = ["/bin/sh", "/usr/bin/node", str(cli), "--print", "--session-id", "cse_01DEAD", "--sdk-url", "wss://x"]
    pids = _procs(monkeypatch, m, child, shell)
    assert m.session_pid("cse_01LIVE") == pids[0]
    assert m.session_pid("cse_01DEAD") is None
    claude, node = m.claude_paths(), m.node_paths()
    assert m.session_child_id(" ".join(child), claude, node, pids[0]) == "cse_01LIVE"
    assert m.session_child_id(" ".join(shell), claude, node, pids[1]) is None
    for argv, held in ((["/usr/bin/node", str(cli), "remote-control", "--session-id", "session_01PIN"], "host"),
                       (["/bin/sh", "/usr/bin/node", str(cli), "remote-control"], "gone")):
        _ps_table(monkeypatch, m, " ".join(argv) + "\n", argv={4242: argv})
        assert m.host_at(4242) == held, argv
    hosts = _running_host(m, tmp_path / "repo", [time.time()])
    h = next(iter(hosts.values()))
    _procs(monkeypatch, m, child, shell)
    live = [logs / f"{h.debugfile.stem}-cse_01LIVE.log", logs / "bridge-transcript-cse_01LIVE.jsonl"]
    dead = [logs / f"{h.debugfile.stem}-cse_01DEAD.log", logs / "bridge-transcript-cse_01DEAD.jsonl"]
    for f in (*live, *dead):
        f.write_text("x")
    _age_past_retention(m, *live, *dead)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert [f.name for f in live if not f.exists()] == []
    assert [f.name for f in dead if f.exists()] == []
    native = cli.parent / "claude"                 # a native claude under the same path
    native.write_text("")
    paths["claude"] = str(native)
    _ps_table(monkeypatch, m, f"{native} remote-control --session-id session_01PIN\n")
    assert m.host_at(4242) == "host"


def _counted_argv(monkeypatch, m, answer):
    """proc_argv() answers `answer` for any pid; returns the list of pids read."""
    reads = []
    monkeypatch.setattr(m, "proc_argv", lambda pid: reads.append(pid) or (list(answer) if answer else None))
    return reads


@pytest.mark.parametrize("folder", ["node" + " " * 33 + "tools", "node tools", "nodejs  tools", "claude code"],
                         ids=["node-33-spaces", "node-space", "nodejs-two-spaces", "claude-space"])
def test_host_at_reads_the_exact_native_claude_under_a_folder_named_like_node(tmp_path, monkeypatch, folder):
    """#758 (merge-gate finding): a native claude under `~/node<spaces>tools/`
    made the row's first word `.../node`, so host_at() took it for a node row
    and, with argv unread, answered "unknown" before it looked at the exact
    claude the hosts run: stop_orphan() refused to stop a live pin and login
    recovery could never restart it. Under `~/claude code/` the first word
    `~/claude` was read as claude and the pin read "gone". The exact native
    path needs no argv and is read before any first word; an npm row under the
    same folder with argv unread stays "unknown"; argv, when read, decides
    both, and is read once."""
    import signal
    m = load()
    install = tmp_path / folder
    install.mkdir()
    native, cli = install / "claude", install / "cli.js"
    native.write_text("")
    cli.write_text("")
    paths = {"claude": str(native), "node": "/usr/bin/node"}
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: paths.get(name))
    pin = f"{native} remote-control --session-id session_01PIN"
    for argv, held in ((None, "host"), ([str(native), "remote-control", "--session-id", "session_01PIN"], "host")):
        _ps_table(monkeypatch, m, pin + "\n")
        reads = _counted_argv(monkeypatch, m, argv)
        assert (m.host_at(4242), len(reads)) == (held, 1), argv
    paths["claude"] = str(cli)                     # an npm install in the same folder
    npm = f"/usr/bin/node {cli} remote-control"
    for argv, held in ((None, "unknown"), (["/usr/bin/node", str(cli), "remote-control"], "host")):
        _ps_table(monkeypatch, m, npm + "\n")
        reads = _counted_argv(monkeypatch, m, argv)
        assert (m.host_at(4242), len(reads)) == (held, 1), argv
    _ps_table(monkeypatch, m, "/usr/bin/python3 -m http.server\n")
    _counted_argv(monkeypatch, m, None)
    assert m.host_at(4242) == "gone"               # no remote-control word: no host, argv or not
    # stop_orphan() stops the native pin with argv unread: SIGINT, then gone
    paths["claude"] = str(native)
    state, sent = ["host"], []
    monkeypatch.setattr(m, "ps_row", lambda pid: pin if state[0] == "host" else "")

    def kill(pid, sig):
        if state[0] == "gone":
            raise ProcessLookupError(pid)
        if sig:
            sent.append(signal.Signals(sig).name)
            state[0] = "gone"

    monkeypatch.setattr(m.os, "kill", kill)
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, None, lambda seconds: None))
    _counted_argv(monkeypatch, m, None)
    assert m.stop_orphan(4242) is True
    assert sent == ["SIGINT"]


def test_argv_decides_a_command_whose_path_reads_like_claude(tmp_path, monkeypatch):
    """#758: ps's text cannot say where a path with spaces ends, so a first word
    named claude, or a row starting with the claude the hosts run, proves
    nothing once argv can be read. A node under `~/claude tools/` running
    claude's cli.js is an npm pin and its child, not claude with the arguments
    `tools/node ...`; a shell under the same folder carrying a dead session's
    flags is no child; and `/a b/claude ...` run as the executable /a with the
    argument b/claude is not the claude at `/a b/claude`. host_at() reads the
    pin as "host" and the others as "gone"; session_pid() hands park_idle()'s
    SIGTERM only the real child; the sweep keeps only the live child's records.
    With argv unread, a remote-control row it cannot prove stays "unknown"."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    tools = tmp_path / "claude tools"
    tools.mkdir()
    node, sh = str(tools / "node"), str(tools / "sh")
    cli = tmp_path / "pkg" / "cli.js"
    cli.parent.mkdir()
    cli.write_text("")
    link = tmp_path / "bin" / "claude"
    link.parent.mkdir()
    link.symlink_to(cli)
    paths = {"claude": str(link)}                  # no node on PATH: the row's own node
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: paths.get(name))
    pin = [node, str(cli), "remote-control", "--session-id", "session_01PIN"]
    _ps_table(monkeypatch, m, " ".join(pin) + "\n", argv={4242: pin})
    assert m.host_at(4242) == "host"
    _ps_table(monkeypatch, m, " ".join(pin) + "\n")
    assert m.host_at(4242) == "unknown"            # argv unread: never "gone" on a guess
    live = [node, str(cli), "--print", "--session-id", "cse_01LIVE", "--sdk-url", "wss://x"]
    dead = [sh, "-c", "sleep", "--session-id", "cse_01DEAD", "--sdk-url", "wss://x"]
    pids = _procs(monkeypatch, m, live, dead)
    assert m.session_pid("cse_01LIVE") == pids[0]
    assert m.session_pid("cse_01DEAD") is None     # never handed to park_idle()'s SIGTERM
    records = {sid: [logs / f"bridge-transcript-cse_{sid}.jsonl"] for sid in ("01LIVE", "01DEAD")}
    for f in (*records["01LIVE"], *records["01DEAD"]):
        f.write_text("x")
    _age_past_retention(m, *records["01LIVE"], *records["01DEAD"])
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    assert [f.name for f in records["01LIVE"] if not f.exists()] == []
    assert [f.name for f in records["01DEAD"] if f.exists()] == []
    native = tmp_path / "a b" / "claude"           # the claude the hosts run, under a spaced path
    native.parent.mkdir()
    native.write_text("")
    (tmp_path / "a").write_text("")
    paths["claude"] = str(native)
    split = [str(tmp_path / "a"), "b/claude", "remote-control"]
    _ps_table(monkeypatch, m, " ".join(split) + "\n", argv={4242: split})
    assert m.host_at(4242) == "gone"               # /a with the argument b/claude: not claude
    whole = [str(native), "remote-control"]
    _ps_table(monkeypatch, m, " ".join(whole) + "\n", argv={4242: whole})
    assert m.host_at(4242) == "host"
    child = [str(tmp_path / "a"), "b/claude", "--print", "--session-id", "cse_01SPLIT", "--sdk-url", "wss://x"]
    _procs(monkeypatch, m, child)
    assert m.session_pid("cse_01SPLIT") is None


@pytest.mark.parametrize("case", ["relative-dotdot", "absolute-dotdot", "plain-path",
                                  "directory-script", "split-spaced-path"])
def test_claude_named_in_another_node_scripts_arguments_is_not_its_child(tmp_path, monkeypatch, case):
    """#758 (merge-gate findings): node's script was guessed from ps's text, so
    another node script's arguments read as claude -- realpath() collapsed `x/..`
    without asking whether x exists (`node other.js <32 args> sub/../cli.js`),
    and the first prefix that exists passed over a directory script (`node
    other dir/../cli.js`, node running other/index.js) -- keeping a dead
    session's aged transcript and handing session_pid() an unrelated pid that
    park_idle() sends SIGTERM. No rule on the text can be right: `node /a
    b/cli.js` is node running claude under a spaced path, or node running /a
    with the argument b/cli.js. The kernel's argv tells them apart, and its
    argv[1] alone is node's script; the real child beside each, its row the same
    words, stays live."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.chdir(tmp_path)
    cli = tmp_path / "a b" / "cli.js"             # claude under a path with a space
    cli.parent.mkdir()
    cli.write_text("")
    link = tmp_path / "claude"
    link.symlink_to(cli)
    paths = {"claude": str(link), "node": "/usr/bin/node"}
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: paths.get(name))
    other = tmp_path / "other.js"
    other.write_text("")
    if case == "directory-script":                # node runs other/index.js
        (tmp_path / "other").mkdir()
        (tmp_path / "other" / "index.js").write_text("")
        (tmp_path / "other dir").mkdir()
    if case == "split-spaced-path":               # node runs /a, with the argument b/cli.js
        (tmp_path / "a").write_text("")
    script = {"relative-dotdot": [str(other), *["x"] * 32, "sub/../a b/cli.js"],
              "absolute-dotdot": [str(other), *["x"] * 32, f"{tmp_path}/sub/../a b/cli.js"],
              "plain-path": [str(other), *["x"] * 32, str(cli)],
              "directory-script": [str(tmp_path / "other"), "dir/../a b/cli.js"],
              "split-spaced-path": [str(tmp_path / "a"), "b/cli.js"]}[case]
    dead = ["/usr/bin/node", *script, "--print", "--session-id", "cse_01DEAD", "--sdk-url", "fixture"]
    live = ["/usr/bin/node", str(cli), "--print", "--session-id", "cse_01LIVE", "--sdk-url", "fixture"]
    pids = _procs(monkeypatch, m, dead, live)
    assert m.session_pid("cse_01DEAD") is None    # never handed to park_idle()'s SIGTERM
    assert m.session_pid("cse_01LIVE") == pids[1]
    old = [logs / "bridge-transcript-cse_01DEAD.jsonl", logs / "bridge-transcript-cse_01LIVE.jsonl"]
    for f in old:
        f.write_text("x")
    _age_past_retention(m, *old)
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    assert [f.name for f in old if f.exists()] == ["bridge-transcript-cse_01LIVE.jsonl"]
    claude, node = m.claude_paths(), m.node_paths()
    assert m.session_child_id(" ".join(dead), claude, node, pids[0]) is None
    assert m.session_child_id(" ".join(live), claude, node, pids[1]) == "cse_01LIVE"


def test_two_real_processes_with_one_ps_row_are_told_apart_by_their_argv(tmp_path, monkeypatch):
    """#758, the merge gate's attack with real processes and the real ps: two
    processes whose rows read the same words but for the session id -- `node
    <tmp>/a b/cli.js --print --session-id ...` -- one running the script
    `<tmp>/a b/cli.js` (claude's, under a spaced path), the other running
    `<tmp>/a` with the argument `b/cli.js`. Only the first is claude's child:
    session_pid() finds it and never the other, and the sweep keeps only its
    aged record. /bin/sh stands in for node, its argv[0] set to node's path."""
    import subprocess
    import sys
    m = load()
    if m.process_table() is None:
        pytest.skip("no readable ps here")
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    cli = tmp_path / "a b" / "cli.js"
    cli.parent.mkdir()
    for script in (cli, tmp_path / "a"):
        script.write_text("echo ready; read x\n")
    link = tmp_path / "claude"
    link.symlink_to(cli)
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: str(link) if name == "claude" else None)
    procs = [subprocess.Popen(["/usr/bin/node", *script, "--print", "--session-id", sid, "--sdk-url", "wss://x"],
                              executable="/bin/sh", stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
             for script, sid in (([str(cli)], "cse_01LIVE"), ([str(tmp_path / "a"), "b/cli.js"], "cse_01DEAD"))]
    try:
        for p in procs:
            assert p.stdout.readline() == "ready\n"
        if sys.platform == "darwin" and m.proc_argv(procs[0].pid) is None:
            pytest.skip("this macOS sandbox shows no KERN_PROCARGS2, even a child's")
        assert m.session_pid("cse_01LIVE") == procs[0].pid
        assert m.session_pid("cse_01DEAD") is None    # never handed to park_idle()'s SIGTERM
        old = [logs / "bridge-transcript-cse_01LIVE.jsonl", logs / "bridge-transcript-cse_01DEAD.jsonl"]
        for f in old:
            f.write_text("x")
        _age_past_retention(m, *old)
        state, now = _idle_check_due(m)
        assert m.cycle({}, [], now, state) is True
        assert [f.name for f in old if f.exists()] == ["bridge-transcript-cse_01LIVE.jsonl"]
    finally:
        for p in procs:
            p.stdin.close()
            p.wait(timeout=HANG_TIMEOUT)
            p.stdout.close()


def test_proc_argv_reads_each_argument_whole_from_the_kernel():
    """#758: ps joins argv with spaces; proc_argv() reads the kernel's own copy,
    each argument whole -- spaces inside one, a run of them, an empty one -- so
    node's script is argv[1] whatever path it sits under. A real process, read
    as the supervisor reads it (/proc on Linux, KERN_PROCARGS2 on macOS); once
    reaped, its pid reads None."""
    import subprocess
    import sys
    m = load()
    if sys.platform != "darwin" and not os.path.exists("/proc/self/cmdline"):
        pytest.skip("neither /proc nor macOS: proc_argv() reads None here by design")
    args = ["/opt/node js/bin/node", "/a b/cli.js", "  two  spaces  ", "", "--session-id", "cse_01X"]
    p = subprocess.Popen(["/bin/sh", "-c", "echo ready; read x", *args],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    try:
        assert p.stdout.readline() == "ready\n"   # sh runs its script: its argv is its own
        got = m.proc_argv(p.pid)
        if got is None and sys.platform == "darwin":
            pytest.skip("this macOS sandbox shows no KERN_PROCARGS2, even for a child")
        assert got is not None and got[1:] == ["-c", "echo ready; read x", *args]
    finally:
        p.stdin.close()
        p.wait(timeout=HANG_TIMEOUT)
        p.stdout.close()
    assert m.proc_argv(p.pid) is None


def test_procargs2_argv_reads_a_macos_kern_procargs2_buffer(monkeypatch):
    """#758: on macOS proc_argv() reads sysctl's KERN_PROCARGS2 buffer -- argc,
    the executable's path, NUL padding, the arguments, then the environment --
    and each argument comes back whole; a buffer short of argc whole arguments,
    an empty one and argc 0 are unread."""
    import sys
    m = load()
    argc = (3).to_bytes(4, sys.byteorder)
    body = b"/usr/local/bin/node\0\0\0\0" + b"node\0/a b/cli.js\0  two  spaces \0" + b"HOME=/Users/me\0"
    assert m.procargs2_argv(argc + body) == ["node", "/a b/cli.js", "  two  spaces "]
    assert m.procargs2_argv(argc + body[:-len(b"HOME=/Users/me\0")]) == ["node", "/a b/cli.js", "  two  spaces "]
    assert m.procargs2_argv(argc + b"/usr/local/bin/node\0\0node\0/a b/cli.js") is None
    assert m.procargs2_argv((5).to_bytes(4, sys.byteorder) + body[:-len(b"HOME=/Users/me\0")]) is None
    assert m.procargs2_argv((0).to_bytes(4, sys.byteorder) + body) is None
    assert m.procargs2_argv(b"") is None
    monkeypatch.setattr(m.sys, "platform", "darwin")
    monkeypatch.setattr(m, "darwin_procargs", lambda pid: argc + body if pid == 42 else None)
    assert m.proc_argv(42) == ["node", "/a b/cli.js", "  two  spaces "]
    assert m.proc_argv(43) is None


def test_darwin_procargs_asks_sysctl_for_argmax_then_the_pids_arguments(monkeypatch):
    """#758: the macOS read is sysctl(3) twice, as ps itself reads a process:
    {CTL_KERN, KERN_ARGMAX} for the buffer's size, then {CTL_KERN,
    KERN_PROCARGS2, pid} into it. A libc stand-in with sysctl's own C signature
    checks the call -- the mib, the lengths, NULL new values -- and a refused
    one reads None."""
    import ctypes
    import sys
    m = load()
    payload = (2).to_bytes(4, sys.byteorder) + b"/usr/bin/node\0\0\0node\0/a b/cli.js\0HOME=/x\0"
    proto = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ctypes.c_int), ctypes.c_uint, ctypes.c_void_p,
                             ctypes.POINTER(ctypes.c_size_t), ctypes.c_void_p, ctypes.c_size_t)
    calls = []

    def sysctl(name, namelen, oldp, oldlenp, newp, newlen):
        mib = [name[i] for i in range(namelen)]
        calls.append((mib, oldlenp[0], newp, newlen))
        if mib == [1, 8]:
            ctypes.c_int.from_address(oldp).value = 4096
            oldlenp[0] = ctypes.sizeof(ctypes.c_int)
            return 0
        if mib == [1, 49, 4242]:
            ctypes.memmove(oldp, payload, len(payload))
            oldlenp[0] = len(payload)
            return 0
        return -1

    class Libc:
        def __init__(self, *args, **kwargs):
            self.sysctl = proto(sysctl)

    monkeypatch.setattr(ctypes, "CDLL", Libc)
    assert m.darwin_procargs(4242) == payload
    assert calls == [([1, 8], ctypes.sizeof(ctypes.c_int), None, 0), ([1, 49, 4242], 4096, None, 0)]
    assert m.procargs2_argv(m.darwin_procargs(4242)) == ["node", "/a b/cli.js"]
    assert m.darwin_procargs(7) is None


@pytest.mark.parametrize("install", ["native", "npm", "npm-aliased"])
def test_the_periodic_sweep_keeps_no_record_for_session_flags_in_another_command(tmp_path, monkeypatch, install):
    """#758: a shell carrying a dead session's `--session-id ... --sdk-url`
    as arguments kept its aged records; a real child under a home with a space
    still keeps its own, whether claude is a native binary or npm's cli.js run
    by node (the second gate finding: its live records were unlinked)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    if install == "native":
        store, _ = _claude_store(tmp_path, monkeypatch, m)
        child = [f"{store}/2.1.330"]
    else:
        cli, _, _ = _npm_claude(tmp_path, monkeypatch, m)
        if install == "npm-aliased":               # ps names cli.js through a linked dir
            alias = tmp_path / "alias dir"
            alias.symlink_to(cli.parents[3])
            cli = alias / cli.relative_to(cli.parents[3])
        child = ["/usr/bin/node", str(cli)]
    hosts = _running_host(m, tmp_path / "repo", [time.time()])
    h = next(iter(hosts.values()))
    _procs(monkeypatch, m, ["/bin/sh", "-c", "sleep", "--session-id", "cse_01DEAD", "--sdk-url", "wss://x"],
           [*child, "--print", "--session-id", "cse_01LIVE", "--sdk-url", "wss://x"])
    live = [logs / f"{h.debugfile.stem}-cse_01LIVE.log", logs / "bridge-transcript-cse_01LIVE.jsonl"]
    dead = [logs / f"{h.debugfile.stem}-cse_01DEAD.log", logs / "bridge-transcript-cse_01DEAD.jsonl"]
    for f in (*live, *dead):
        f.write_text("x")
    _age_past_retention(m, *live, *dead)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert [f.name for f in live if not f.exists()] == []
    assert [f.name for f in dead if f.exists()] == []


def test_a_bare_session_id_in_an_unrelated_command_does_not_keep_a_dead_record(tmp_path, monkeypatch):
    """#758: a `--session-id` token alone proves nothing -- `printf %s
    --session-id cse_01OLD` is not a session child. Only the argv session_pid()
    reads as one (--session-id beside --sdk-url) keeps a record."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    table = "/bin/printf %s --session-id cse_01OLD\n"
    _ps_table(monkeypatch, m, table)
    hosts = _running_host(m, tmp_path / "repo", [time.time()])
    h = next(iter(hosts.values()))
    dead = [logs / f"{h.debugfile.stem}-cse_01OLD.log",
            logs / "bridge-transcript-cse_01OLD.jsonl"]
    for f in dead:
        f.write_text("x")
    _age_past_retention(m, *dead)
    state, now = _idle_check_due(m)
    m.cycle(hosts, [], now, state)
    assert [f.name for f in dead if f.exists()] == []


def test_a_failed_ps_leaves_the_records_for_the_next_check(tmp_path, monkeypatch):
    """#758: a nonzero ps exit is an unread table even when it printed rows, so
    that idle check unlinks nothing; the next check sweeps."""
    import subprocess
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    table = "/x/claude --session-id cse_01LIVE --sdk-url wss://x\n"
    monkeypatch.setattr(m.subprocess, "run", lambda cmd, **kw: subprocess.CompletedProcess(
        cmd, 1, stdout=table, stderr="ps: partial"))
    hosts = _running_host(m, tmp_path / "repo", [time.time()])
    old = logs / "bridge-transcript-cse_01OLD.jsonl"
    old.write_text("x")
    _age_past_retention(m, old)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert old.exists()


def test_the_periodic_sweep_waits_when_the_process_table_is_unread(tmp_path, monkeypatch):
    """#758: with no process table nothing proves a session dead, so that idle
    check unlinks nothing; the next one sweeps."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    _ps_table(monkeypatch, m, "/x/claude --print --session-id cse_01LIVE --sdk-url wss://x\n", code=1)
    hosts = _running_host(m, tmp_path / "repo", [time.time()])
    old = logs / "bridge-transcript-cse_01OLD.jsonl"
    old.write_text("x")
    _age_past_retention(m, old)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert old.exists()


def test_the_periodic_sweep_drops_a_retired_folders_logs(tmp_path, monkeypatch):
    """#758: a folder removed from rc-hosts keeps no claim on its host-level
    logs. They are one per folder, not one per session, and with no process
    naming them nothing writes them any more, so they must age out like any
    other record -- otherwise a supervisor that never restarts accumulates
    them as folders are retired (the evaluator's refusal)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    _ps_table(monkeypatch, m, "/usr/bin/vim notes.txt\n")
    h = m.Host(str(tmp_path / "retired"), "gw")     # the folder is gone: hosts no longer holds it
    old = [h.logfile, h.debugfile, h.logfile.with_suffix(".log.1"), h.debugfile.with_suffix(".log.1"),
           logs / "bridge-transcript-session_01GONE.jsonl"]   # a retired pin's record: no config, no row
    kept = logs / "kept.log"
    for f in (*old, kept):
        f.write_text("x")
    _age_past_retention(m, *old)
    state, now = _idle_check_due(m)
    assert m.cycle({}, [], now, state) is True
    assert [f.name for f in old if f.exists()] == []
    assert kept.exists()                                    # inside the week: kept


def test_a_script_row_running_a_shell_keeps_no_dead_record(tmp_path, monkeypatch):
    """#758: a `script` row whose launched command is /bin/sh does not serve a
    session just because a later word is the wrapper's path and a --session-id
    (the evaluator's refusal). The launched command decides, so the dead
    session's records go while a configured host's own logs stay."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    _ps_table(monkeypatch, m,
              "/usr/bin/script -q /dev/null /bin/sh /x/ccmorph-rc --session-id cse_01DEAD\n")
    h = m.Host(str(tmp_path / "repo"), "gw")       # configured, not running
    dead = [logs / "repo.debug-cse_01DEAD.log", logs / "bridge-transcript-cse_01DEAD.jsonl"]
    own = [h.logfile, h.debugfile]
    for f in (*dead, *own):
        f.write_text("x")
    _age_past_retention(m, *dead, *own)
    state, now = _idle_check_due(m)
    assert m.cycle({h.folder: h}, [], now, state) is True
    assert [f.name for f in dead if f.exists()] == []
    assert all(f.exists() for f in own)


def _spaced_install(tmp_path):
    """The add-on loaded from a copy under a path with spaces, beside ccmorph-rc
    and the modules it imports, so HERE and rc_module() both read that path."""
    import shutil
    here = tmp_path / "Application Support" / "cc morph" / "remote-control"
    here.mkdir(parents=True)
    for name in ("ccmorph-rc-hosts", "ccmorph-rc", "rc_route.py"):
        shutil.copy(SCRIPT.parent / name, here / name)
    return load_script(here / "ccmorph-rc-hosts", "rc_hosts")


@pytest.mark.parametrize("managed", [True, False], ids=["beside-a-managed-host", "alone"])
@pytest.mark.parametrize("row", ["pty-row", "claude-row"])
def test_the_periodic_sweep_reads_a_live_pin_and_adapter_under_a_path_with_spaces(
        tmp_path, monkeypatch, row, managed):
    """#758: ps joins a command's argv with spaces, so an add-on, a lib dir or a
    claude under a path with spaces split into several words, and a reader that
    looked for the path at a word position missed the live pin and the adapter:
    their aged records and log were unlinked while the processes still wrote
    them. The rows the supervisor's own commands make are matched by their whole
    start. The pin here is in no config, so only its ps row can hold it."""
    monkeypatch.setenv("CCMORPH_LIB_DIR", str(tmp_path / "lib dir" / "lib"))
    m = _spaced_install(tmp_path)
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    claude = str(tmp_path / "home dir" / ".local" / "bin" / "claude")
    monkeypatch.setattr(m.shutil, "which", lambda name, *a, **kw: claude if name == "claude" else None)
    adapter = m.rc_module().ADAPTER
    assert " " in str(m.HERE) and " " in str(adapter)
    pin = m.Host(str(tmp_path / "pin"), "gw", ("--session-id", "session_01PIN"))   # not in hosts
    hosts = _running_host(m, tmp_path / "repo", [time.time()]) if managed else {}
    launched = (f"/usr/bin/script -q /dev/null {m.HERE / 'ccmorph-rc'} gw -- " if row == "pty-row"
                else f"{claude} remote-control ")
    _ps_table(monkeypatch, m,
              f"{launched}--session-id session_01PIN --debug-file={pin.debugfile}\n"
              f"{adapter} adapter ds --auto --parent-pid 1\n")
    held = [logs / "bridge-transcript-session_01PIN.jsonl",
            logs / f"{pin.debugfile.stem}-cse_01PIN.log", logs / "adapter-ds.log"]
    dead = [logs / "bridge-transcript-cse_01DEAD.jsonl", logs / "adapter-old.log"]
    for f in (*held, *dead):
        f.write_text("x")
    _age_past_retention(m, *held, *dead)
    state, now = _idle_check_due(m)
    assert m.cycle(hosts, [], now, state) is True
    assert [f.name for f in held if not f.exists()] == []
    assert [f.name for f in dead if f.exists()] == []


def test_a_pinned_host_start_removes_its_stale_bridge_transcript(tmp_path, monkeypatch):
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    mine = logs / "bridge-transcript-session_01X.jsonl"
    other = logs / "bridge-transcript-session_01Y.jsonl"
    mine.write_text("x")
    other.write_text("x")
    m.Host(str(repo), "gw", ("--session-id", "session_01X")).start()
    assert not mine.exists()
    assert other.exists()


def test_a_failing_hand_back_is_retried_at_most_once_per_rewake_after(tmp_path, monkeypatch):
    """A 409 or 503 left `done` unset, so the POST repeated every RECOVER_EVERY.
    A transient error (no answer from the server) is still retried at the next check."""
    import json
    import urllib.error
    m = load()
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    active = [{"id": "cse_01A", "environment_id": "env_01R", "connection_status": "disconnected"}]
    posts = []
    fail = [None]

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            raise fail[0] or urllib.error.HTTPError(path, 409, "conflict", {}, None)
        return {"data": active}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    done = {}
    for _ in range(int(m.REWAKE_AFTER // m.RECOVER_EVERY)):
        m.recover_processless([str(repo)], done, {str(repo)})
        clock[0] += m.RECOVER_EVERY
    assert posts == ["cse_01A"]                       # answered 409: once per REWAKE_AFTER

    posts.clear()
    done.clear()
    fail[0] = TimeoutError("timed out")
    for _ in range(3):
        m.recover_processless([str(repo)], done, {str(repo)})
        clock[0] += m.RECOVER_EVERY
    assert len(posts) == 3                            # no answer: the next check retries


@pytest.mark.parametrize("seed", ["busy", "lost"])
def test_one_session_gets_one_hand_back_per_cycle(tmp_path, monkeypatch, seed):
    """A disconnected session remembered in busy.json was handed back by both
    recovery functions in one cycle: two POSTs, two restarts of one turn. A
    `busy` one became `lost` and was handed back again the next cycle."""
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    m.BUSY.write_text(json.dumps({"cse_01B": seed}))
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            return {}
        return {"data": [{"id": "cse_01B", "environment_id": "env_01R",
                          "connection_status": "disconnected"}]}
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    up = {str(repo)}
    done, failed = {}, set()
    for _ in range(2):                    # two disconnected cycles
        handed = m.recover_processless([str(repo)], done, up, failed)
        m.recover_interrupted([str(repo)], up, handed)
    assert posts == ["cse_01B"]
    assert json.loads(m.BUSY.read_text()) == {"cse_01B": "requeued"}


def _failing_hand_back(tmp_path, monkeypatch, error):
    """One disconnected session remembered `lost` whose hand-back POST raises
    error(path); returns (module, repo, posts, clock)."""
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    m.BUSY.write_text(json.dumps({"cse_01B": "lost"}))
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            raise error(path)
        return {"data": [{"id": "cse_01B", "environment_id": "env_01R",
                          "connection_status": "disconnected"}]}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    return m, repo, posts, clock


def _posts_per_cycle(m, repo, posts, clock, n, done, failed):
    """POSTs per cycle over n cycles RECOVER_EVERY apart, both calls as main() runs them."""
    up, per = {str(repo)}, []
    for _ in range(n):
        before = len(posts)
        handed = m.recover_processless([str(repo)], done, up, failed)
        m.recover_interrupted([str(repo)], up, handed)
        per.append(len(posts) - before)
        clock[0] += m.RECOVER_EVERY
    return per


@pytest.mark.parametrize("code", [409, 503])
def test_a_failed_hand_back_is_not_retried_by_the_other_function(tmp_path, monkeypatch, code):
    """#75 let a 409/503 fall through to recover_interrupted, which POSTed again
    and recorded `lost`, so the next cycle POSTed again: [2,1,1,1], one hand-back
    every RECOVER_EVERY instead of one per REWAKE_AFTER."""
    import json
    import urllib.error
    m, repo, posts, clock = _failing_hand_back(
        tmp_path, monkeypatch, lambda p: urllib.error.HTTPError(p, code, "no", {}, None))
    done, failed = {}, set()
    assert _posts_per_cycle(m, repo, posts, clock, 4, done, failed) == [1, 0, 0, 0]
    assert json.loads(m.BUSY.read_text()) != {"cse_01B": "requeued"}
    clock[0] = 1000.0 + m.REWAKE_AFTER                 # the window has passed: once more
    assert _posts_per_cycle(m, repo, posts, clock, 2, done, failed) == [1, 0]


def test_a_new_turn_cut_off_soon_after_a_hand_back_is_requeued_once(tmp_path, monkeypatch):
    """A successful hand-back holds the session in `done` for REWAKE_AFTER; that
    hold must not stop recover_interrupted re-queueing a new turn cut off inside
    it (once, on the second disconnected check), as before #75."""
    import json
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    session = {"id": "cse_01B", "environment_id": "env_01R", "connection_status": "disconnected"}
    posts = []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append(body["session_id"])
            return {}
        return {"data": [dict(session)]}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    done, failed = {}, set()
    assert _posts_per_cycle(m, repo, posts, clock, 1, done, failed) == [1]   # handed back
    session.update(connection_status="connected", worker_status="running")
    assert _posts_per_cycle(m, repo, posts, clock, 1, done, failed) == [0]   # a new turn
    assert json.loads(m.BUSY.read_text()) == {"cse_01B": "busy"}
    session.update(connection_status="disconnected", worker_status="idle")  # cut off
    assert _posts_per_cycle(m, repo, posts, clock, 4, done, failed) == [0, 1, 0, 0]
    assert clock[0] - 1000.0 < m.REWAKE_AFTER        # all inside the first hold
    assert json.loads(m.BUSY.read_text()) == {"cse_01B": "requeued"}


def test_a_transient_hand_back_failure_gets_one_post_per_check(tmp_path, monkeypatch):
    """No answer from the server: recover_processless retries at the next check
    (its contract); recover_interrupted must not add a second POST to a cycle."""
    import json
    m, repo, posts, clock = _failing_hand_back(
        tmp_path, monkeypatch, lambda p: TimeoutError("timed out"))
    assert _posts_per_cycle(m, repo, posts, clock, 4, {}, set()) == [1, 1, 1, 1]
    assert json.loads(m.BUSY.read_text()) != {"cse_01B": "requeued"}


def test_a_failed_hand_back_is_not_recorded_as_requeued(tmp_path, monkeypatch):
    """recover_processless marked the session sent before its POST, so a failed
    POST still suppressed recover_interrupted and busy.json said `requeued`: a
    state that is never handed back again."""
    import json
    import urllib.error
    m, repo, posts, clock = _failing_hand_back(
        tmp_path, monkeypatch, lambda p: urllib.error.HTTPError(p, 500, "boom", {}, None))
    m.BUSY.write_text(json.dumps({"cse_01B": "busy"}))
    up = {str(repo)}
    handed = m.recover_processless([str(repo)], {}, up)
    m.recover_interrupted([str(repo)], up, handed)
    assert posts == ["cse_01B"]
    assert json.loads(m.BUSY.read_text()) == {"cse_01B": "lost"}


def test_a_dns_outage_leaves_hosts_running_and_hands_back_once_it_ends(tmp_path, monkeypatch):
    """GH-461: the Mac's DNS was down for 5h12m on 2026-10-22 and every session
    check on both recovery paths failed with `nodename nor servname provided`. A
    failed check is not a reason to touch a host: the running bridge is the thing
    that reconnects the poll, and stopping it would cut the very session that was
    only waiting for DNS. The hand-back happens once the lookups answer again."""
    import json
    import socket
    import urllib.error
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    offline, posts = [True], []

    def api(method, path, token, body=None):
        if offline[0]:
            raise urllib.error.URLError(
                socket.gaierror(8, "nodename nor servname provided, or not known"))
        if method == "POST":
            posts.append(body["session_id"])
            return {}
        return {"data": [{"id": "cse_01B", "environment_id": "env_01R",
                          "connection_status": "disconnected"}]}
    clock = [1000.0]
    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    h = m.Host(str(repo), "profile")
    proc, starts, stopped = FakeProc(), [], []
    h.proc, h.started = proc, 0.0                      # running, up long enough to be asked
    monkeypatch.setattr(h, "start", lambda: starts.append(1))
    monkeypatch.setattr(h, "stop", lambda: stopped.append(1))
    hosts = {str(repo): h}
    state = m.Clock()
    state.last_recover = -m.RECOVER_EVERY             # every cycle's recovery is due
    state.last_idle_check = clock[0]
    for _ in range(3):                                 # three checks with no DNS answer
        m.cycle(hosts, [str(repo)], clock[0], state)
        clock[0] += m.RECOVER_EVERY
    assert (posts, starts, stopped) == ([], [], [])    # the host was never touched
    assert h.proc is proc
    offline[0] = False                                 # DNS answers again
    m.cycle(hosts, [str(repo)], clock[0], state)
    assert posts == ["cse_01B"]                        # handed back once, to the live host


@pytest.mark.parametrize("code", [400, 404])
def test_a_session_the_server_will_not_resume_is_not_asked_again(tmp_path, monkeypatch, code):
    """400/404: not resumable; neither function asks again, however long it waits."""
    import urllib.error
    m, repo, posts, clock = _failing_hand_back(
        tmp_path, monkeypatch, lambda p: urllib.error.HTTPError(p, code, "no", {}, None))
    done, failed = {}, set()
    assert _posts_per_cycle(m, repo, posts, clock, 3, done, failed) == [1, 0, 0]
    clock[0] += 10 * m.REWAKE_AFTER
    assert _posts_per_cycle(m, repo, posts, clock, 2, done, failed) == [0, 0]


@needs_loopback  # the half-written reply comes off a real listener: #611
def test_a_truncated_api_reply_does_not_stop_the_supervisor(tmp_path, monkeypatch):
    """A reply cut off mid-body raised http.client.IncompleteRead (not an OSError)
    out of api() and exited main(); launchd then restarted every host (2026-10-14)."""
    import json
    import socket
    import threading
    m = load()
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    srv = socket.socket()
    srv.bind(("127.0.0.1", 0))
    srv.listen(8)

    seen = []        # every request the listener was asked for, as `posts` records hand-backs

    def serve():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return
            seen.append(c.recv(65536))       # before the reply: a call that returned saw this
            c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 500\r\n\r\n" + b'{"data": []}')
            c.close()
    threading.Thread(target=serve, daemon=True).start()
    monkeypatch.setattr(m, "API", f"http://127.0.0.1:{srv.getsockname()[1]}")
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    try:
        m.recover_processless([str(repo)], {}, {str(repo)})
        m.recover_interrupted([str(repo)], {str(repo)})
        m.purge_archived([str(repo)], 10**10, 86400)
    finally:
        srv.close()
    assert len(seen) >= 3, (
        f"only {len(seen)} request(s) reached the listener: each of the three calls must "
        "read the truncated reply through api() for the 2026-10-14 exit to be guarded at all")


def test_purge_days_is_a_setting_not_a_folder():
    m = load()
    text = "route=gw\nidle_days=1\npurge_days=7\n~/proj\n"
    assert all("purge_days" not in folder for folder in m.parse(text))
    assert m.purge_days(text) == 7.0


PEP585_GENERICS = ("list", "dict", "set", "frozenset", "tuple", "type")


def is_python_helper(name, text):
    """A python shebang, a uv inline script (its shebang need not name python),
    or any module under addons/, which ships on the add-on's 3.8 floor without a
    shebang of its own."""
    if name.startswith("addons/") and name.endswith(".py"):
        return True
    shebang = text.splitlines()[0] if text.startswith("#!") else ""
    return "python" in shebang or ("uv run --script" in shebang and "# /// script" in text)


def annotation_nodes(tree):
    """Every annotation expression in the tree: parameters, returns, declared names."""
    import ast
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            a = node.args
            for arg in [a.vararg, a.kwarg, *a.posonlyargs, *a.args, *a.kwonlyargs]:
                if arg is not None and arg.annotation is not None:
                    yield arg.annotation
            if node.returns is not None:
                yield node.returns
        elif isinstance(node, ast.AnnAssign) and node.annotation is not None:
            yield node.annotation


def test_every_shipped_python_helper_uses_nothing_newer_than_the_python_38_floor():
    """The add-on declares 3.8 (CI python-floor); str.removeprefix/removesuffix, functools.cache
    and dict | dict are 3.9+ and broke imports there (2026-10). Static, so it fails on any
    interpreter. One rule set over every shipped Python file: a python shebang,
    a uv inline script, or any .py under addons/. And on that floor the add-on's
    PEP 585 builtins and PEP 604 | in an annotation need "from __future__ import
    annotations" or the import itself evaluates them (a tuple | None
    raised TypeError on <= 3.9, 2026-11-16)."""
    import ast
    import subprocess
    root = SCRIPT.parent.parent.parent
    names = subprocess.run(["git", "ls-files"], cwd=root, capture_output=True, text=True,
                           check=True, timeout=HANG_TIMEOUT).stdout.split()
    bad = []
    seen = set()
    for name in names:
        path = root / name
        if not path.is_file() or path.is_symlink():
            continue
        text = path.read_text(errors="replace")
        if not is_python_helper(name, text):
            continue
        seen.add(name)
        tree = ast.parse(text)
        for node in ast.walk(tree):
            if isinstance(node, ast.Attribute) and node.attr in ("removesuffix", "removeprefix"):
                bad.append(f"{name} line {node.lineno}: str.{node.attr}")
            if isinstance(node, ast.Attribute) and node.attr == "cache" \
                    and getattr(node.value, "id", "") == "functools":
                bad.append(f"{name} line {node.lineno}: functools.cache")
            if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr) and (
                    getattr(node.left, "attr", "") == "environ"
                    or isinstance(node.right, (ast.Dict, ast.DictComp))):
                bad.append(f"{name} line {node.lineno}: dict | dict")
        if name.startswith("addons/"):
            defers = any(isinstance(n, ast.ImportFrom) and n.module == "__future__"
                         and any(a.name == "annotations" for a in n.names)
                         for n in ast.walk(tree))
            if not defers:
                for ann in annotation_nodes(tree):
                    for node in ast.walk(ann):
                        if isinstance(node, ast.Subscript) and isinstance(node.value, ast.Name) \
                                and node.value.id in PEP585_GENERICS:
                            bad.append(f"{name} line {node.lineno}: PEP 585 {node.value.id}[...] "
                                       f"without the annotations future import")
                        if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr):
                            bad.append(f"{name} line {node.lineno}: PEP 604 | annotation "
                                       f"without the annotations future import")
    assert len(seen) >= 8, seen
    assert not bad, bad


def test_no_shipped_python_helper_loads_a_module_with_load_module():
    """Loader.load_module() is deprecated in 3.12 and gone in 3.15: a shipped script
    that loads through it starts raising AttributeError the day python3 bumps, and
    the rc-hosts supervisor would crash-loop every start with no edge maintained
    (GH-812). The floor test above catches APIs newer than 3.8; this is its ceiling
    twin over the same selection walk: newer APIs that are already removed.
    This file itself is not in the walk (no python shebang), so the message below
    cannot match its own rule."""
    import subprocess
    root = SCRIPT.parent.parent.parent
    names = subprocess.run(["git", "ls-files"], cwd=root, capture_output=True, text=True,
                           check=True, timeout=HANG_TIMEOUT).stdout.split()
    bad = []
    for name in names:
        path = root / name
        if not path.is_file() or path.is_symlink():
            continue
        text = path.read_text(errors="replace")
        if not is_python_helper(name, text):
            continue
        for lineno, line in enumerate(text.splitlines(), 1):
            if ".load_module(" in line:
                bad.append(f"{name} line {lineno}: {line.strip()}")
    assert not bad, (
        "load_module() was removed in Python 3.15 (deprecated in 3.12), so this "
        "launcher load breaks the day python3 bumps; use importlib.util."
        "spec_from_loader + module_from_spec + exec_module instead "
        "(see tests/support.py load_script): " + "; ".join(bad))


def _running_host(m, repo, clock):
    """A repo host with a live process, up long enough to be hand-back eligible."""
    h = m.Host(str(repo), "gw")
    h.proc, h.started = FakeProc(), clock[0] - 10 * m.RECOVER_EVERY
    return {str(repo): h}


def test_main_loop_cycle_hands_back_once_per_rewake_after_on_a_503(tmp_path, monkeypatch):
    """main() must pass its hand_back_failed set to recover_processless (#75):
    through the real per-cycle step, a 503 is one POST per REWAKE_AFTER, not one
    from recover_interrupted on the next cycle as well."""
    import urllib.error
    m, repo, posts, clock = _failing_hand_back(
        tmp_path, monkeypatch, lambda p: urllib.error.HTTPError(p, 503, "no", {}, None))
    hosts = _running_host(m, repo, clock)
    state = m.Clock()
    state.last_recover = clock[0] - m.RECOVER_EVERY
    per = []
    for _ in range(4):
        before = len(posts)
        m.cycle(hosts, [str(repo)], clock[0], state)
        per.append(len(posts) - before)
        clock[0] += m.RECOVER_EVERY
    assert per == [1, 0, 0, 0]
    clock[0] += m.REWAKE_AFTER
    m.cycle(hosts, [str(repo)], clock[0], state)
    assert len(posts) == 2


def test_main_loop_cycle_caps_an_oversized_running_log(tmp_path, monkeypatch):
    """main() must run bound_logs() on running repo hosts at each idle check (#128)."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    clock = [1000.0]
    hosts = _running_host(m, tmp_path / "repo", clock)
    h = next(iter(hosts.values()))
    h.debugfile.write_bytes(b"x" * (m.LOG_CAP + 1))
    state = m.Clock()
    state.last_idle_check = state.last_recover = clock[0]
    assert m.cycle(hosts, [h.folder], clock[0] + 1, state) is False       # not yet due
    assert h.debugfile.stat().st_size == m.LOG_CAP + 1
    assert m.cycle(hosts, [h.folder], clock[0] + m.IDLE_CHECK, state) is True
    assert h.debugfile.stat().st_size == 0


@pytest.mark.parametrize("where, line, signal", [
    pytest.param("log", "[bridge] No access token for session fetch",
                 "No access token for session fetch", id="host-log-no-access-token"),
    pytest.param("debug", "API error (attempt 1/11): Could not resolve authentication method",
                 "Could not resolve authentication method", id="host-debug-resolve-auth"),
    pytest.param("session", "[Bootstrap] Skipped: no usable OAuth, WIF, or API key",
                 "[Bootstrap] Skipped: no usable OAuth", id="session-bootstrap"),
])
def test_a_lost_login_counts_only_when_this_run_of_the_host_wrote_it(tmp_path, monkeypatch,
                                                                     where, line, signal):
    """GH-239: a host that can no longer log its sessions in is recognised from
    the lines the issue recorded. Only what this run printed counts: the same
    log files survive a restart, and every check re-reading an earlier run's
    failure would restart the host again and again."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    target = {"log": h.logfile, "debug": h.debugfile,
              "session": logs / f"{h.debugfile.stem}-cse_01A.log"}[where]
    target.write_text(line + "\n")
    h.start()
    assert h.lost_login() is None              # an earlier run's line is not this run's
    with target.open("a") as fh:
        fh.write(line + "\n")
    assert signal in h.lost_login()


@pytest.mark.parametrize("where", [
    pytest.param("log", id="host-log"),
    pytest.param("debug", id="host-debug"),
    pytest.param("session", id="session-log"),
])
def test_a_failure_line_is_detected_however_much_output_follows_it(tmp_path, monkeypatch, where):
    """GH-296: lost_login() read only the last TAIL bytes past the mark, so a
    verbose host buried its own failure line under tool output and was never
    restarted. The line counts wherever in this run's output it sits."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    target = {"log": h.logfile, "debug": h.debugfile,
              "session": logs / f"{h.debugfile.stem}-cse_01A.log"}[where]
    target.write_text("an earlier run's output\n")
    h.start()
    with target.open("a") as fh:
        fh.write("No access token for session fetch\n")
        fh.write("x" * (m.TAIL + 1) + "\n")     # the run talked on after it failed
    assert h.lost_login() is not None


def test_a_failure_line_the_writer_had_not_finished_is_still_detected(tmp_path, monkeypatch):
    """GH-296: the forward reader must not step past a line the writer had not
    finished. A check that read only `No access token for ` and the next one,
    after `session fetch` arrived, must together see the whole line -- the tail
    window re-read the region and did, so the scan may advance only past
    complete lines."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    h.start()
    with h.logfile.open("a") as fh:
        fh.write("No access token for ")
    assert h.lost_login() is None              # the line is not whole yet
    with h.logfile.open("a") as fh:
        fh.write("session fetch\n")
        fh.write("x" * 1000 + "\n")            # and the run talked on after it
    assert h.lost_login() is not None


@pytest.mark.parametrize("line", [
    pytest.param("StopFailure:authentication_failed", id="stop-failure"),
    pytest.param('API Error: 401 {"error":{"type":"authentication_error"}}', id="route-401"),
])
def test_a_rejected_route_key_is_not_a_lost_account_login(tmp_path, monkeypatch, line):
    """A 401 from the route and Claude Code's `authentication_failed` say the
    route refused, not that the account login is gone; restarting the host would
    not fix either. Only the login lines the issue recorded count."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    h.start()
    with h.logfile.open("a") as fh:
        fh.write(line + "\n")
    assert h.lost_login() is None


def test_a_debug_log_truncated_after_start_is_read_from_its_start(tmp_path, monkeypatch):
    """bound_logs() truncates a running host's debug log in place, so the byte
    offset remembered at start can sit past the new end. The whole file is then
    this run's, and a login failure written after the truncation counts."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    h.debugfile.write_text("x" * 10_000)
    h.start()
    os.truncate(h.debugfile, 0)
    h.debugfile.write_text("Could not resolve authentication method\n")
    assert h.lost_login() is not None


def test_a_capped_log_regrown_past_its_old_position_is_read_from_its_head_issue_417(
        tmp_path, monkeypatch):
    """GH-417: bound_logs() truncates a running host's debug log in place, then
    fresh output can regrow it past the position the login reader had already
    taken. Size alone proved nothing then -- mark and pos were both below the
    new size -- so the reader seeked past the new head and a login failure
    written at that head was never seen, leaving a logged-out host down. The
    truncation now tells the reader the whole file is this run's."""
    m = load()
    logs = tmp_path / "logs"
    logs.mkdir()
    monkeypatch.setattr(m, "LOGS", logs)
    monkeypatch.setattr(m, "LOG_CAP", 4_000)
    monkeypatch.setattr(m.Host, "ensure_repo", lambda self: None)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())
    repo = tmp_path / "repo"
    repo.mkdir()
    h = m.Host(str(repo), "gw")
    h.debugfile.write_text("x" * 100)
    h.start()                                  # the reader's mark sits at 100 bytes
    with h.debugfile.open("a") as fh:
        fh.write("y" * 3_900 + "\n")           # past the cap; the reader takes it
    assert h.lost_login() is None
    h.bound_logs()                             # the cap truncates the live log
    with h.debugfile.open("a") as fh:
        fh.write("No access token for session fetch\n")
        fh.write("z" * 5_000 + "\n")           # regrown past the old position
    why = h.lost_login()
    assert why is not None and "No access token for session fetch" in why


def _lost_login_host(tmp_path, monkeypatch):
    """A registered repo folder with its host up, and the account API faked.

    The host's stop()/start() are replaced on the instance: the real stop()
    signals its process group, and FakeProc reports pid 1, which is this
    machine's launchd. stop() drops the process as the real one does; start()
    is what the loop's next tick does after a restart.
    """
    import json
    m = load()
    monkeypatch.setattr(m, "LOGS", tmp_path / "logs")
    (tmp_path / "logs").mkdir(exist_ok=True)
    monkeypatch.setattr(m, "BUSY", tmp_path / "busy.json")
    repo = tmp_path / "repo"
    (repo / ".claude").mkdir(parents=True)
    (repo / ".claude" / "rc-host.json").write_text(json.dumps({"environmentId": "env_01R"}))
    clock = [1000.0]
    posts, stops, starts = [], [], []

    def api(method, path, token, body=None):
        if method == "POST":
            posts.append((path, body["session_id"]))
            return {}
        return {"data": [{"id": "cse_01B", "environment_id": "env_01R",
                          "connection_status": "disconnected"}]}

    def start():                               # the relaunch a stopped host gets next tick
        starts.append(clock[0])
        h.proc, h.started = FakeProc(), clock[0]
        h.mark_logs()

    def stop():
        stops.append(clock[0])
        h.proc = None

    monkeypatch.setattr(m, "api", api)
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, clock, m.time.sleep))
    h = m.Host(str(repo), "profile")
    h.proc, h.started = FakeProc(), clock[0] - 10 * m.RECOVER_EVERY
    monkeypatch.setattr(h, "start", start)
    monkeypatch.setattr(h, "stop", stop)
    return m, h, str(repo), clock, posts, stops


def _recovery_due(m, clock):
    state = m.Clock()
    state.last_recover = clock[0] - m.RECOVER_EVERY
    state.last_idle_check = clock[0]
    return state


def _orphan_host(tmp_path, monkeypatch, dies_on, reused_by=""):
    """A host an earlier supervisor left running, with its pid faked.

    `dies_on` names the signals the fake host does not survive; once one lands
    the pid is empty, or -- when `reused_by` names a command -- it is held by
    that other process, as a recycled pid is. os.kill is the process table
    (signal 0 probes liveness, a real signal is recorded and changes the
    state), time.sleep is a no-op, and subprocess.run answers `ps`. Nothing
    here is a real process or a real wait.
    """
    import signal
    import subprocess
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)
    h.proc = None                              # not this supervisor's child
    state, sent = ["host"], []                 # host, gone, or reused

    def kill(pid, sig):
        if state[0] == "gone":
            raise ProcessLookupError(pid)
        if sig == 0:
            return
        if state[0] == "reused":
            pytest.fail("signalled a pid the host no longer holds")
        name = signal.Signals(sig).name
        sent.append(name)
        if name in dies_on:
            state[0] = "reused" if reused_by else "gone"

    def run(*cmd, **opts):
        out = {"host": "/Users/me/.local/bin/claude remote-control --spawn=worktree\n",
               "reused": f"{reused_by}\n", "gone": ""}[state[0]]
        return subprocess.CompletedProcess(cmd, 0, stdout=out, stderr="")

    monkeypatch.setattr(m.os, "kill", kill)
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, None, lambda seconds: None))
    monkeypatch.setattr(m.subprocess, "run", run)
    monkeypatch.setattr(h, "live_pointer_pid", lambda: 4242 if state[0] != "gone" else None)
    monkeypatch.setattr(m, "proc_argv", lambda pid: None)   # the text decides: no real pid 4242's argv
    h.debugfile.write_text("[bridge:token] No OAuth token available for refresh (failure 1/3)\n")
    return m, h, repo, clock, sent


def test_a_host_that_lost_its_login_is_restarted_and_its_sessions_keep_their_ids(
        tmp_path, monkeypatch, capsys):
    """GH-239: three folders' hosts kept a stale account token for ten hours and
    every session they spawned answered with a login error while the account
    profile was logged in. Restarting a host fixed it, and its row and session
    ids survived. The supervisor now does that at its next recovery check: one
    line naming the folder, then the same host in the same folder, whose
    processless session is handed back under its own id."""
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)
    (tmp_path / "logs" / f"{h.debugfile.stem}-cse_01B.log").write_text(
        "API error (attempt 1/11): Could not resolve authentication method\n")
    kept = (tmp_path / "repo" / ".claude" / "rc-host.json").read_text()
    state = _recovery_due(m, clock)
    m.cycle({repo: h}, [repo], clock[0], state)
    assert stops == [clock[0]]
    assert f"{repo}: sessions lost their login" in capsys.readouterr().out
    assert h.proc is None                      # nothing else starts the host this tick
    assert h.tend(clock[0])                    # next tick: the same folder, reattached
    clock[0] += 2 * m.RECOVER_EVERY
    m.cycle({repo: h}, [repo], clock[0], state)
    assert posts == [("/v1/environments/env_01R/bridge/reconnect", "cse_01B")]
    assert (tmp_path / "repo" / ".claude" / "rc-host.json").read_text() == kept


def test_a_lost_login_does_not_restart_a_host_while_the_account_login_is_unreadable(
        tmp_path, monkeypatch, capsys):
    """A restart cannot fix an account that is itself logged out; it would only
    replace the host in a loop. The event is logged all the same, and the host
    is left to serve whatever it still can."""
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)

    def logged_out():
        raise KeyError("claudeAiOauth")

    monkeypatch.setattr(m, "account_token", logged_out)
    (tmp_path / "logs" / f"{h.debugfile.stem}-cse_01B.log").write_text(
        "[Bootstrap] Skipped: no usable OAuth, WIF, or API key\n")
    m.cycle({repo: h}, [repo], clock[0], _recovery_due(m, clock))
    assert stops == [] and h.proc is not None
    out = capsys.readouterr().out
    assert f"{repo}: sessions lost their login" in out
    assert "account login is unreadable" in out


@pytest.mark.parametrize("line_after_the_window", [True, False],
                         ids=["a-new-line-arrives", "only-the-line-inside-the-window"])
def test_a_host_that_loses_its_login_again_is_restarted_at_most_once_per_window(
        tmp_path, monkeypatch, capsys, line_after_the_window):
    """A restart that did not fix the host must not become a restart loop: the
    same folder gets at most one login restart per LOGIN_RESTART_AFTER, and the
    suppressed one is logged with its own wording. The line the window defers on
    is the one the check after the window must still see (GH-296: it was marked
    read at the deferral, so only a brand-new failure could restart the host,
    and none arriving left it serving errors for good)."""
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)
    log = tmp_path / "logs" / f"{h.debugfile.stem}-cse_01B.log"
    log.write_text("Could not resolve authentication method\n")
    state = _recovery_due(m, clock)
    m.cycle({repo: h}, [repo], clock[0], state)
    assert len(stops) == 1
    h.tend(clock[0])
    with log.open("a") as fh:
        fh.write("No access token for session fetch\n")
    clock[0] += m.RECOVER_EVERY
    m.cycle({repo: h}, [repo], clock[0], state)
    assert len(stops) == 1                     # inside the window: waits
    assert "again" in capsys.readouterr().out
    clock[0] += m.LOGIN_RESTART_AFTER
    if line_after_the_window:
        with log.open("a") as fh:
            fh.write("No access token for session fetch\n")
    m.cycle({repo: h}, [repo], clock[0], state)
    assert len(stops) == 2                     # a new window: one more try


def test_a_host_that_loses_its_login_after_a_cap_truncation_is_restarted_issue_417(
        tmp_path, monkeypatch, capsys):
    """GH-417: a session log the cap truncated and fresh output regrew past the
    reader's old position still names the lost login at its head, and the next
    recovery check restarts the host instead of leaving it down."""
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)
    monkeypatch.setattr(m, "LOG_CAP", 4_000)
    log = tmp_path / "logs" / f"{h.debugfile.stem}-cse_01B.log"
    log.write_text("x" * 100)
    h.mark_logs()                              # this supervisor's run starts here
    with log.open("a") as fh:
        fh.write("y" * 3_900 + "\n")           # past the cap; the reader takes it
    assert h.lost_login() is None
    h.bound_logs()                             # the cap truncates the live log
    with log.open("a") as fh:
        fh.write("Could not resolve authentication method\n")
        fh.write("z" * 5_000 + "\n")           # regrown past the old position
    m.cycle({repo: h}, [repo], clock[0], _recovery_due(m, clock))
    assert stops == [clock[0]]
    assert f"{repo}: sessions lost their login" in capsys.readouterr().out


def _login_refresh(monkeypatch):
    """The supervisor with a fake keychain and a fake claude, for the account
    login refresh: `left` is the seconds of token left, `reads` counts keychain
    reads and `runs` holds one Proc per refresh turn started."""
    m = load()
    left = [3600.0]
    reads = []
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, [1000.0], m.time.sleep))

    def login():
        reads.append(1)
        return {"expiresAt": (1000.0 + left[0]) * 1000, "refreshToken": "refresh"}
    monkeypatch.setattr(m, "account_login", login)

    class RC:
        PROXY_ENV = ("HTTPS_PROXY",)

        @staticmethod
        def account_candidates():
            return ["acct"]

        @staticmethod
        def profile_env(name):
            return {"CLAUDE_CONFIG_DIR": f"/p/{name}"}

        @staticmethod
        def strip_env():
            return ("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK",
                    "AWS_BEARER_TOKEN_BEDROCK", "HTTPS_PROXY")
    monkeypatch.setattr(m, "rc_module", lambda: RC)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "route-key")
    monkeypatch.setenv("CLAUDE_CODE_USE_BEDROCK", "1")
    monkeypatch.setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-key")
    monkeypatch.setenv("HTTPS_PROXY", "http://proxy:3128")
    runs = []

    class Proc:
        returncode = 0
        killed = False

        def __init__(self, argv, env, **_):
            self.argv, self.env = argv, env
            runs.append(self)

        def poll(self):
            return None

        def kill(self):
            self.killed = True
            self.returncode = -9

        def wait(self, timeout=None):
            return self.returncode
    monkeypatch.setattr(m.subprocess, "Popen", Proc)
    return m, left, reads, runs


def test_the_account_login_is_refreshed_before_it_expires(monkeypatch):
    """Route sessions never refresh the account login, so the supervisor asks
    Claude Code to, once it nears expiry, one run at a time, never with the
    route's credential in the environment."""
    m, left, reads, runs = _login_refresh(monkeypatch)
    clock = m.Clock()
    m.keep_login_fresh(clock, 0.0)
    assert runs == []                                   # an hour left: nothing to do
    assert len(reads) == 1
    for t in range(m.RECOVER_EVERY, 3000, m.RECOVER_EVERY):
        m.keep_login_fresh(clock, float(t))
    # a token with an hour left is not re-read from the keychain every 20 s cycle
    assert len(reads) == 1 and runs == []
    left[0] = 300.0
    m.keep_login_fresh(clock, 3000.0)
    m.keep_login_fresh(clock, 3001.0)                   # still running: no second run
    assert len(runs) == 1
    argv, env = runs[0].argv, runs[0].env
    assert argv[:2] == ["claude", "-p"]
    assert env["CLAUDE_CONFIG_DIR"] == "/p/acct" and "ANTHROPIC_API_KEY" not in env
    # a Bedrock-routed profile's own env must not send the refresh turn to Bedrock,
    # but the proxy stays: a host behind one needs it to reach the account endpoint
    assert "CLAUDE_CODE_USE_BEDROCK" not in env and "AWS_BEARER_TOKEN_BEDROCK" not in env
    assert env["HTTPS_PROXY"] == "http://proxy:3128"
    clock.refresher.poll = lambda: 0
    m.keep_login_fresh(clock, 3002.0)                   # done, but within the retry wait
    assert len(runs) == 1
    m.keep_login_fresh(clock, 3002.0 + m.LOGIN_REFRESH_RETRY)
    assert len(runs) == 2


def test_signed_out_login_does_not_refresh_and_reports_once_per_episode(monkeypatch, capsys):
    m, left, reads, runs = _login_refresh(monkeypatch)
    login = {"expiresAt": 0, "accessToken": "expired"}
    monkeypatch.setattr(m, "account_login", lambda: login)
    monkeypatch.setattr(m, "host_envs", lambda folders: {"env": "repo"})

    def unauthorized(token, status):
        raise m.urllib.error.HTTPError("url", 401, "Unauthorized", None, None)
    monkeypatch.setattr(m, "sessions", unauthorized)
    clock = m.Clock()
    for now in (0.0, 20.0):
        m.keep_login_fresh(clock, now)
        m.host_sessions(["repo"], "active")
    output = capsys.readouterr().out
    assert runs == []
    assert output.count("account acct is signed out; fix: ccmorph login acct") == 1
    assert output.count("active session check failed:") == 1
    assert "active session check failed: HTTP Error 401: Unauthorized; fix: ccmorph login acct" in output
    login.update(refreshToken="refresh", expiresAt=4_600_000)
    m.keep_login_fresh(clock, 40.0)
    del login["refreshToken"]
    m.keep_login_fresh(clock, clock.next_login_check)
    m.host_sessions(["repo"], "active")
    output = capsys.readouterr().out
    assert output.count("account acct is signed out; fix: ccmorph login acct") == 1
    assert output.count("active session check failed:") == 1
    assert runs == []


def test_a_refresh_turn_that_never_exits_is_killed_and_replaced(monkeypatch):
    """A refresh turn that hangs past the deadline is killed and a new one starts
    in the same cycle, so one stuck claude never blocks later refreshes."""
    m, left, reads, runs = _login_refresh(monkeypatch)
    left[0] = 300.0
    clock = m.Clock()
    m.keep_login_fresh(clock, 0.0)
    assert len(runs) == 1 and not runs[0].killed
    m.keep_login_fresh(clock, m.LOGIN_REFRESH_DEADLINE - 1.0)
    assert len(runs) == 1 and not runs[0].killed        # inside the deadline: waits
    m.keep_login_fresh(clock, float(m.LOGIN_REFRESH_DEADLINE))
    assert runs[0].killed                               # past it: killed, not waited on
    assert len(runs) == 2 and clock.refresher is runs[1]   # and replaced at once


def test_a_host_an_earlier_supervisor_left_running_is_restarted_when_it_lost_its_login(
        tmp_path, monkeypatch, capsys):
    """2026-11-08: the mindmur host outlived two supervisor restarts. The new
    supervisor only waited on it, so when it logged "No OAuth token available
    for refresh" nothing checked it and its sessions stayed dead. Such a host is
    now checked too and interrupted by its exact pid."""
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)
    h.proc = None                              # not this supervisor's child
    monkeypatch.setattr(h, "live_pointer_pid", lambda: 4242)
    stopped = []
    monkeypatch.setattr(m, "stop_orphan", lambda pid: stopped.append(pid) or True)
    h.debugfile.write_text("[bridge:token] No OAuth token available for refresh (failure 1/3)\n")
    m.cycle({repo: h}, [repo], clock[0], _recovery_due(m, clock))
    assert stopped == [4242]
    assert h.login_restarted == clock[0]       # recorded only with the stop confirmed
    assert f"{repo}: sessions lost their login" in capsys.readouterr().out


@pytest.mark.parametrize("dies_on, expected, restarted", [
    ({"SIGTERM"}, ["SIGINT", "SIGTERM"], True),
    (set(), ["SIGINT", "SIGTERM", "SIGKILL"] * 2, False),
], ids=["ignores-sigint-issue-297", "outlives-sigkill-issue-297"])
def test_an_orphan_that_ignores_sigint_is_escalated_and_only_a_confirmed_stop_is_a_restart(
        tmp_path, monkeypatch, dies_on, expected, restarted):
    """2026-11-09, #297: an orphan that ignored SIGINT stayed up while the
    supervisor recorded its restart, set the cooldown and never tried again.
    A stop now waits, escalates to SIGTERM and SIGKILL, and only a pid that is
    gone counts as a restart; one that survives is signalled again next check."""
    m, h, repo, clock, sent = _orphan_host(tmp_path, monkeypatch, dies_on)
    first = m.restart_logged_out({repo: h}, clock[0])
    second = m.restart_logged_out({repo: h}, clock[0] + m.RECOVER_EVERY)
    assert (first, second) == (([repo] if restarted else []), [])
    assert sent == expected
    assert (h.login_restarted == clock[0]) is restarted


@pytest.mark.parametrize("returncode", [1, 0], ids=["ps-failed-issue-297", "ps-blind-issue-297"])
def test_a_ps_that_cannot_show_the_live_orphan_does_not_record_a_stop(
        tmp_path, monkeypatch, returncode):
    """#297's refusal: ps's exit status was discarded, so a ps that failed or
    named no process read as the orphan gone. While something lives at the pid
    the stop is unconfirmed: it is not signalled blind, nothing records a
    restart, and the next check tries again."""
    import subprocess
    m, h, repo, clock, sent = _orphan_host(tmp_path, monkeypatch, dies_on=set())

    def silent(*cmd, **opts):
        return subprocess.CompletedProcess(cmd, returncode, stdout="", stderr="ps: unreadable")

    monkeypatch.setattr(m.subprocess, "run", silent)
    first = m.restart_logged_out({repo: h}, clock[0])
    second = m.restart_logged_out({repo: h}, clock[0] + m.RECOVER_EVERY)
    assert (first, second) == ([], [])
    assert sent == []                          # no signal on an unread identity
    assert h.login_restarted == float("-inf")  # no cooldown suppressing the retry


def test_a_pid_reused_after_the_interrupt_is_never_signalled_issue_297(tmp_path, monkeypatch):
    """#297's second refusal: the host exits on SIGINT and another process takes
    the pid. The host is confirmed gone, so the restart is recorded, but the
    replacement is alive and must not receive SIGTERM or SIGKILL."""
    m, h, repo, clock, sent = _orphan_host(
        tmp_path, monkeypatch, dies_on={"SIGINT"}, reused_by="/usr/bin/python3 -m http.server")
    first = m.restart_logged_out({repo: h}, clock[0])
    second = m.restart_logged_out({repo: h}, clock[0] + m.RECOVER_EVERY)
    assert (first, second) == ([repo], [])
    assert sent == ["SIGINT"]                  # nothing reached the reused pid
    assert h.login_restarted == clock[0]


def test_a_folder_with_no_host_at_all_is_not_checked_for_its_login(tmp_path, monkeypatch):
    m, h, repo, clock, posts, stops = _lost_login_host(tmp_path, monkeypatch)
    h.proc = None
    monkeypatch.setattr(h, "live_pointer_pid", lambda: None)
    monkeypatch.setattr(m, "stop_orphan", lambda pid: pytest.fail("no host to stop"))
    h.debugfile.write_text("Remote Control is only available with claude.ai subscriptions\n")
    m.cycle({repo: h}, [repo], clock[0], _recovery_due(m, clock))
    assert stops == []


def test_a_kept_worktree_is_reported_once_per_run(capsys):
    m = load()
    for _ in range(3):
        m.kept_once("/r: cse_1 archived; worktree kept: its branch is missing")
    assert capsys.readouterr().out.count("worktree kept") == 1


def test_inference_reads_the_literal_value_and_no_line_is_empty():
    m = load()
    assert m.inference("route=llmgate\n~/x\n") == ""
    assert m.inference("inference=alex  # subscription first\n") == "alex"
    assert m.inference("inference=llmgate\n") == "llmgate"
    assert m.inference("# inference=alex\n") == ""


def _switcher(m, monkeypatch, tmp_path):
    """The supervisor's Switcher over fake edges and a fake `--switch`: `edge(route,
    pid)` starts an edge process (a new start each time, even for a reused pid) or
    stops it (pid None); a Popen'd child takes its ticket as the real --switch does
    (a --rehand child takes none), and `finish(child)` lands its word by that ticket
    (a re-hand lands none) and has the edge's proxy hold the handoff (held.<pid>.json
    and up-<word>.json, as --switch and rc_addon leave them). `proxy_restart(pid)` is
    that proxy starting again: it holds nothing."""
    rc = m.rc_module()
    live, alive, children = {}, set(), []
    started, gen = {}, [0]            # pid -> the start identity the fake edge process has
    m.EDGES.mkdir(parents=True, exist_ok=True)
    m.INFERENCE.parent.mkdir(parents=True, exist_ok=True)
    monkeypatch.setattr(m, "LOGS", tmp_path / "logs")
    monkeypatch.setattr(m, "account_name", lambda: "alex")
    monkeypatch.setattr(m, "edge_ready", lambda r: {"pid": live[r]} if r in live else None)
    monkeypatch.setattr(rc, "live_edge", lambda r: {"pid": live[r]} if r in live else None)
    monkeypatch.setattr(rc.rc_route, "proc_started", lambda pid: started.get(pid))
    monkeypatch.setattr(rc, "_listening", lambda upstream: True)
    monkeypatch.setattr(rc, "get_tiers", lambda p: {})
    took = proxy_took(rc)
    monkeypatch.setattr(rc, "handoff_taken", took)
    def edge(route, pid):
        if pid is None:
            live.pop(route, None)
            return
        live[route] = pid
        gen[0] += 1
        started[pid] = f"start-{gen[0]}"   # a new process, even when the OS reuses the number
        (m.EDGES / f"{route}.json").write_text(json.dumps({"pid": pid, "port": 8080}))
    def popen(cmd, **kw):
        child = FakeProc()
        child.rehand = "--rehand" in cmd
        child.ticket = None if child.rehand else rc.take_ticket(m.INFERENCE)   # as the real --switch does
        child.word, child.edge = cmd[1], next(iter(live.values()), None)
        child.started = started.get(child.edge)   # the edge --switch would hand to at this moment
        children.append(child)
        return child
    monkeypatch.setattr(m.subprocess, "Popen", popen)
    def finish(child, rc_=0):
        if rc_ == 0:
            word = "route" if child.word in live else child.word
            if not child.rehand and not rc.land_word(word, child.ticket, m.INFERENCE):
                child.rc = 1                         # a later switch landed first: it refuses
                return
            if word != "route" and child.edge is not None:
                hid = f"hand-{len(children)}-{child.word}"
                took(m.INFERENCE.parent / f"switch-{word}.json", json.dumps(
                    {"edge": child.edge, "edge_started": child.started, "id": hid}).encode())
                (m.INFERENCE.parent / f"up-{word}.json").write_text(json.dumps(
                    {"edge": child.edge, "edge_started": child.started, "upstream": "http://127.0.0.1:45111",
                     "tiers": {}, "hand": hid}))
        child.rc = rc_
    def proxy_restart(pid):
        (m.INFERENCE.parent / f"held.{pid}.json").write_text(json.dumps(
            {"edge": pid, "edge_started": started[pid], "profiles": {}}))
    return types.SimpleNamespace(live=live, alive=alive, edge=edge, children=children, finish=finish,
                                 started=started, proxy_restart=proxy_restart)


@pytest.mark.parametrize("removed", [
    pytest.param("route=llmgate\n~/x\n", id="line-removed-restores-the-route"),
    pytest.param("# inference=alex\n", id="line-commented-out-restores-the-route"),
])
def test_removing_an_applied_inference_line_restores_the_route(tmp_path, monkeypatch, removed):
    """The literal is "" with no line, and a config that drops an applied
    `inference=` still means the route: the account that line chose is not left
    serving every proxy."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    sw = m.Switcher()
    sw.reload(m.inference("inference=alex  # subscription first\n"))
    sw.tend(0)
    assert m.INFERENCE.read_text() == "account\n"
    sw.reload(m.inference(removed))
    sw.tend(1)
    assert m.INFERENCE.read_text() == "route\n"
    assert w.children == []


@pytest.mark.parametrize("edge_live, word", [
    pytest.param(True, "route", id="a-profile-with-its-own-live-edge-spells-route-issue-595"),
    pytest.param(False, "gateway", id="a-profile-without-one-stays-a-profile-issue-595"),
])
def test_one_rule_decides_the_route_word_at_both_entry_points(tmp_path, monkeypatch, edge_live, word):
    """issue #595: `--switch` wrote "route" for a profile with a live edge, while the
    supervisor wrote it for any route the config named, edge or no edge. Both entry
    points now ask ccmorph-rc's inference_word, and both spellings are driven
    through both."""
    m = load()
    rc = m.rc_module()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway" if edge_live else "other", 500)
    sw = m.Switcher()
    sw.reload("gateway")
    sw.tend(0)
    if edge_live:
        assert w.children == [] and m.INFERENCE.read_text() == "route\n"
    else:
        assert [c.word for c in w.children] == ["gateway"]
    monkeypatch.setattr(rc, "profile_type", lambda p: "apikey")
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-g"})
    monkeypatch.setattr(rc, "retire_route_with", lambda *a: None)
    monkeypatch.setattr(rc, "sidecar", lambda p, owner, started: ("http://127.0.0.1:45111", 4321))
    rc.switch("gateway", "alex")
    assert m.INFERENCE.read_text() == word + "\n"


def test_a_comment_reload_keeps_a_manual_switch_whatever_the_edge_says(tmp_path, monkeypatch):
    """issue #595 review: the supervisor compared a word DERIVED from the config
    (gateway before its edge answered, route after), so a comment-only reload read
    as a moved ask and wrote route over a manual oli. It compares the literal."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    sw = m.Switcher()
    sw.reload("gateway")
    sw.tend(0)
    assert w.children == []
    w.edge("gateway", 500)
    sw.tend(1)
    assert m.INFERENCE.read_text() == "route\n" and m.WANTED.read_text() == "gateway\n"
    m.INFERENCE.write_text("oli\n")                      # the person's manual --switch oli
    sw.reload("gateway")
    sw.tend(2)
    assert m.INFERENCE.read_text() == "oli\n"
    assert [c.word for c in w.children] in ([], ["oli"])  # at most a re-hand of oli itself


def test_boot_with_the_ask_already_applied_keeps_a_manual_choice(tmp_path, monkeypatch):
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    m.WANTED.write_text("oli\n")
    m.INFERENCE.write_text("account\n")
    w.edge("gateway", 500)
    sw = m.Switcher()
    sw.reload("oli")
    for t in range(3):
        sw.tend(t)
    assert w.children == [] and m.INFERENCE.read_text() == "account\n"


def test_a_reload_during_a_bring_up_waits_for_it_and_then_wins(tmp_path, monkeypatch, capsys):
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    sw = m.Switcher()
    sw.reload("oli")
    sw.tend(0)
    assert [c.word for c in w.children] == ["oli"]
    sw.reload("alex")
    sw.tend(1)
    assert not m.INFERENCE.exists()
    w.finish(w.children[0])
    sw.tend(2)
    out = capsys.readouterr().out
    assert "inference: oli live" in out
    assert m.INFERENCE.read_text() == "account\n" and m.WANTED.read_text() == "alex\n"
    assert len(w.children) == 1


def test_an_older_bring_up_never_overwrites_a_manual_switch_that_landed_meanwhile(tmp_path, monkeypatch, capsys):
    """issue #595 review: the supervisor's `--switch oli` child was still running when
    the person ran `--switch kimi`; the child wrote oli after kimi had landed and the
    supervisor took its success as the ask applied. The child is the older switch:
    its word is refused, and neither that tick nor a retry puts oli back."""
    m = load()
    rc = m.rc_module()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    sw = m.Switcher()
    sw.reload("oli")
    sw.tend(0)
    child = w.children[0]
    monkeypatch.setattr(rc, "profile_type", lambda p: "apikey")
    monkeypatch.setattr(rc, "resolve_route", lambda p: {"ANTHROPIC_API_KEY": "sk-test"})
    monkeypatch.setattr(rc, "retire_route_with", lambda *a: None)
    monkeypatch.setattr(rc, "sidecar", lambda p, owner, started: ("http://127.0.0.1:45111", 4321))
    rc.switch("kimi", "alex")
    w.finish(child)
    for t in range(1, 5):
        sw.tend(t * m.BACKOFF_MAX)
    assert m.INFERENCE.read_text() == "kimi\n"
    assert [c.word for c in w.children] == ["oli"]          # no retry put oli back over kimi
    assert "set to kimi by hand" in capsys.readouterr().out


def test_a_switch_log_stamp_that_fails_never_loses_the_running_child(tmp_path, monkeypatch):
    """#758's stamp beside #595's one child: the stamp follows a launch that
    succeeded, so a stamp that raises must not lose the child just started -- the
    next tick would start a second `--switch` for the same ask."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)

    def refuse(*a, **k):
        raise PermissionError(13, "stamp refused")
    monkeypatch.setattr(m.os, "utime", refuse)
    sw = m.Switcher()
    sw.reload("oli")
    for t in range(3):
        sw.tend(t)
    assert [c.word for c in w.children] == ["oli"]
    assert sw.proc is w.children[0]


def test_switching_away_and_back_brings_the_profile_up_again(tmp_path, monkeypatch):
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    sw = m.Switcher()
    sw.reload("oli")
    sw.tend(0)
    w.finish(w.children[0])
    sw.tend(1)
    assert m.INFERENCE.read_text() == "oli\n"
    sw.reload("alex")
    sw.tend(2)
    assert m.INFERENCE.read_text() == "account\n"
    sw.reload("oli")
    sw.tend(3)
    assert [c.word for c in w.children] == ["oli", "oli"]
    w.finish(w.children[1])
    sw.tend(4)
    assert m.INFERENCE.read_text() == "oli\n"


def test_a_manual_switch_while_the_ask_waits_is_not_overruled(tmp_path, monkeypatch, capsys):
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    sw = m.Switcher()
    sw.reload("oli")
    sw.tend(0)
    m.INFERENCE.write_text("account\n")
    w.edge("gateway", 500)
    sw.tend(1)
    assert w.children == [] and m.INFERENCE.read_text() == "account\n"
    assert m.WANTED.read_text() == "oli\n"
    assert "set to account by hand" in capsys.readouterr().out


def test_boot_waits_for_exactly_one_live_edge_and_says_so_once(tmp_path, monkeypatch, capsys):
    """issue #595: the reload branch Popen-fired `--switch` before any edge was up and
    the child died on "found 0". The Switcher dispatches nothing until exactly one
    shared edge serves, says why once, and brings the profile up on the first tick
    one does."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    sw = m.Switcher()
    sw.reload("oli")
    for t in range(3):
        sw.tend(t)
    assert w.children == []
    w.edge("gateway", 500)
    w.edge("other", 600)
    sw.tend(3)
    assert w.children == []
    w.edge("other", None)
    sw.tend(4)
    assert [c.word for c in w.children] == ["oli"]
    out = capsys.readouterr().out
    assert out.count("waits for exactly one live shared edge (0 live)") == 1
    assert out.count("waits for exactly one live shared edge (2 live)") == 1


def _live_edge(m, route, pid):
    """The trace of a live edge the switcher can see: its record file, naming its pid."""
    m.EDGES.mkdir(parents=True, exist_ok=True)
    (m.EDGES / f"{route}.json").write_text(f'{{"pid": {pid}, "port": 8080}}')


def _account(m, monkeypatch, name="alex"):
    """account_name() reads the state dir's profiles; a test names its account directly."""
    monkeypatch.setattr(m, "account_name", lambda: name)


def test_a_profile_is_brought_up_once_its_edge_is_live_and_the_outcome_is_logged(tmp_path, monkeypatch, capsys):
    """issue #595: the reload branch Popen-fired `--switch` before any edge was up.
    The child died on "needs exactly one live shared edge, found 0", nothing retried,
    and the intent was already recorded as done: the phone stayed on the route with
    one dead child's note as the only trace. Bring-up now waits for a live edge and
    the supervisor log names the one outcome of each attempt."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    (tmp_path / "a").mkdir()
    cfg.write_text(f"route=gateway\ninference=oli\n{tmp_path}/a\n")
    _account(m, monkeypatch)
    monkeypatch.setattr(m.rc_module().rc_route, "proc_started", lambda pid: f"start-{pid}")
    order, fires = [], []
    def popen(cmd, **kw):
        fires.append(cmd[-2])
        return FakeProc(rc=0)
    monkeypatch.setattr(m.subprocess, "Popen", popen)
    def start(self):
        self.proc, self.started = FakeProc(), 0.0
        order.append("edge" if self.edge else self.folder)
        if self.edge:
            _live_edge(m, self.route, 500)
    run_main(m, monkeypatch, tmp_path, cfg, start,
             edge_ready=lambda route: (m.EDGES / f"{route}.json").exists())
    out = capsys.readouterr().out
    assert order[0] == "edge"
    assert fires[:1] == ["oli"]                              # only after the edge had its record
    assert out.count("inference: bringing up oli") == 1
    assert out.count("inference: oli live") == 1               # the one outcome, named once


def test_a_failed_bring_up_retries_until_it_lands(tmp_path, monkeypatch, capsys):
    """issue #595: a bring-up that died (edge flapping, profile unreadable) left the
    word pointing at a profile nothing serves, forever. Each failed attempt logs its
    own outcome and the target comes back within the module's backoff."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text("route=gateway\ninference=oli\n")
    _account(m, monkeypatch)
    _live_edge(m, "gateway", 500)
    monkeypatch.setattr(m.rc_module().rc_route, "proc_started", lambda pid: f"start-{pid}")
    children = iter([FakeProc(rc=1), FakeProc(rc=0)])          # first attempt dies, the retry lands
    fires = []
    def popen(cmd, **kw):
        fires.append(cmd[-2])
        return next(children, FakeProc())
    monkeypatch.setattr(m.subprocess, "Popen", popen)
    def start(self):
        self.proc, self.started = FakeProc(), 0.0
    clock, ticks = [0.0], [0]
    def sleep(_):
        ticks[0] += 1
        clock[0] += m.BACKOFF_MIN
        if ticks[0] > 4:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep, clock=clock)
    out = capsys.readouterr().out
    assert fires[:2] == ["oli", "oli"]
    assert out.count("inference: oli bring-up failed (rc=1); retrying") == 1
    assert out.count("inference: oli live") == 1


@pytest.mark.parametrize("manual, change, writes, word_after", [
    pytest.param("oli", "# reloaded\nroute=gateway\n", ["route"], "oli",
                 id="manual-switch-survives-an-unchanged-reload-issue-595"),
    pytest.param("route", "# reloaded\nroute=gateway\n", ["route"], "route",
                 id="unchanged-reload-writes-nothing-issue-595"),
    pytest.param("oli", "route=gateway\ninference=alex\n", ["route", "account"], "account",
                 id="manual-switch-yields-to-a-moved-target-issue-595"),
    pytest.param("route", "route=gateway\ninference=alex\n", ["route", "account"], "account",
                 id="moved-target-rewrites-the-word-issue-595"),
])
def test_a_reload_overrules_a_manual_switch_only_when_its_own_target_moved(
        tmp_path, monkeypatch, manual, change, writes, word_after):
    """issue #595: every reload wrote "route" over a live `--switch`, so switching
    to a provider held only until someone edited the config. The word is written
    when the config target moved from what this supervisor last recorded AND the
    word a proxy answers is not already it."""
    m = load()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text("route=gateway\n")
    _account(m, monkeypatch)
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: FakeProc())   # a re-hand of oli waits
    words = []
    real_write = m.rc_module().write_inference
    def spy(word, path):
        words.append(word)
        real_write(word, path)
    monkeypatch.setattr(m.rc_module(), "write_inference", spy)
    def start(self):
        self.proc, self.started = FakeProc(), 0.0
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 3:                    # the person switches by hand, then edits the config
            m.INFERENCE.write_text(manual + "\n")
            cfg.write_text(change)
            m.os.utime(cfg, (cfg.stat().st_mtime + 2,) * 2)
        if ticks[0] > 4:
            raise SystemExit
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep)
    assert words == writes                                   # boot imposed the config once, then the gate held
    assert m.INFERENCE.read_text().strip() == word_after


@pytest.mark.parametrize("case", ["edge-restarted", "edge-pid-recycled", "proxy-restarted-behind-the-same-edge",
                                  "adapter-stopped-answering", "down-tick-with-completion", "manual-word"])
def test_a_restart_rehands_the_live_word_without_manual_repair(tmp_path, monkeypatch, case):
    """issue #595: a proxy keeps a handoff only in memory, so an edge or proxy restart
    lost the switched profile and the phone went back to the route, with a note
    naming a repair. The supervisor asks the live edge's proxy what it holds every
    tick and re-runs `--switch` for the live word -- the config's or a manual one --
    when it does not hold it; never while it does."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    rc = m.rc_module()
    if case == "manual-word":
        m.WANTED.write_text("\n")
        m.INFERENCE.write_text("kimi\n")                     # a manual --switch kimi, its edge since replaced
        w.edge("gateway", 600)
        sw = m.Switcher()
        sw.reload("")
        sw.tend(0)
        assert [c.word for c in w.children] == ["kimi"]
        assert m.INFERENCE.read_text() == "kimi\n"
        return
    sw = m.Switcher()
    w.edge("gateway", 500)
    sw.reload("oli")
    sw.tend(0)
    if case == "down-tick-with-completion":
        w.edge("gateway", None)
        w.finish(w.children[0])
        sw.tend(1)
        assert len(w.children) == 1
        w.edge("gateway", 500)                               # back, as a new process
        sw.tend(2)
        assert [c.word for c in w.children] == ["oli", "oli"]
        return
    w.finish(w.children[0])
    for t in range(1, 4):
        sw.tend(t)
    assert len(w.children) == 1                              # held: nothing re-handed
    if case == "edge-restarted":
        w.edge("gateway", 600)
    elif case == "edge-pid-recycled":
        w.edge("gateway", 500)                               # a replacement process, same number
    elif case == "proxy-restarted-behind-the-same-edge":
        w.proxy_restart(500)
    else:
        monkeypatch.setattr(rc, "_listening", lambda upstream: False)
    sw.tend(4)
    assert [c.word for c in w.children] == ["oli", "oli"]
    assert w.children[1].edge == w.live["gateway"]
    assert w.children[1].rehand and not w.children[0].rehand   # upkeep writes no word
    if case != "adapter-stopped-answering":
        w.finish(w.children[1])
        for t in range(5, 8):
            sw.tend(t)
        assert len(w.children) == 2 and m.INFERENCE.read_text() == "oli\n"


def test_a_restart_rehands_a_session_word_too(tmp_path, monkeypatch):
    """GH-977: `--switch --session` hands one phone session its own profile and
    leaves its word in <state>/sessions/<id>; the endpoint lives only in the edge's
    proxy, and the re-hand loop asked after only the SHARED word. An edge or proxy
    restart therefore kept the shared word served and dropped every session's pick:
    the session fell to the edge's route while Stark still showed the profile it
    chose — the #920 miss reborn through the door #595 left open. A session's word
    is now kept held the same way the live word is, writes no word, and never
    moves the shared switch."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text("route\n")                 # no inference= line: every host follows the route
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    (m.SESSION_WORDS / "cse_one").write_text("kimi\n")
    sw = m.Switcher()
    sw.reload("")                                     # the config asks for nothing the switch must move
    sw.tend(0)                                        # records the (empty) ask
    assert w.children == []
    sw.tend(1)                                        # the pick has no endpoint yet: bring it up
    assert [c.word for c in w.children] == ["kimi"]
    w.finish(w.children[0])
    for t in range(2, 5):
        sw.tend(t)
    assert len(w.children) == 1                       # held: nothing re-handed
    w.proxy_restart(500)                              # the proxy forgets every endpoint it held
    sw.tend(5)
    assert [c.word for c in w.children] == ["kimi", "kimi"]
    assert w.children[1].rehand                       # upkeep writes no word
    assert m.INFERENCE.read_text() == "route\n"       # the shared word never moved
    w.finish(w.children[1])
    for t in range(6, 9):
        sw.tend(t)
    assert len(w.children) == 2 and m.INFERENCE.read_text() == "route\n"


@pytest.mark.parametrize("shared, files, expected", [
    pytest.param("route", {"cse_a": "account", "cse_b": "route", "cse_c": "not a word!"}, [],
                 id="words-that-need-no-endpoint-ask-for-nothing"),
    pytest.param("oli", {"cse_a": "kimi"}, ["oli", "kimi"],
                 id="the-shared-word-goes-first-and-the-session-follows"),
    pytest.param("kimi", {"cse_a": "kimi"}, ["kimi"],
                 id="a-session-on-the-live-word-asks-for-one-child-not-two"),
    pytest.param("route", {"cse_a": b"\xff\xfe", "cse_b": "kimi"}, ["kimi"],
                 id="a-word-file-that-is-not-text-is-skipped-like-an-unreadable-one"),
])
def test_rehand_orders_the_words_and_writes_none_of_them(tmp_path, monkeypatch, shared, files, expected):
    """GH-977 edges: words that name no endpoint (account, route, garbage) never
    ask for a child; the shared word is kept before any session's; one word wants
    one endpoint however many files name it. No re-hand ever moves the switch."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text(shared + "\n")
    m.WANTED.write_text(("" if shared == "route" else shared) + "\n")   # the ask already applied
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    for sid, word in files.items():
        (m.SESSION_WORDS / sid).write_bytes(word if isinstance(word, bytes) else (word + "\n").encode())
    sw = m.Switcher()
    sw.reload("" if shared == "route" else shared)
    for i, word in enumerate(expected):
        sw.tend(i)
        assert w.children[i].word == word and w.children[i].rehand
        w.finish(w.children[i])
    sw.tend(len(expected))
    assert len(w.children) == len(expected)                             # held: nothing further asked
    assert m.INFERENCE.read_text() == shared + "\n"                     # upkeep wrote no word


@pytest.mark.parametrize("shared, files, failing, expected_first", [
    pytest.param("route", {"cse_a": "gone", "cse_b": "kimi"}, {"gone"}, ["gone", "kimi"],
                 id="a-failing-first-session-word"),
    pytest.param("oli", {"cse_a": "kimi"}, {"oli"}, ["oli", "kimi"],
                 id="a-failing-shared-word"),
    pytest.param("route", {"cse_a": "gone", "cse_b": "kimi"}, {"gone", "kimi"}, ["gone", "kimi"],
                 id="every-word-failing-takes-turns"),
])
def test_a_word_whose_rehand_fails_never_starves_the_others(tmp_path, monkeypatch, shared, files,
                                                           failing, expected_first):
    """GH-977: rehand dispatched the first unheld word every tick and returned, so a
    word whose --switch kept failing (a deleted profile, a dead upstream) was retried
    for good and every word behind it was never handed -- a session's pick set in
    Stark, never served, by one bad neighbour. A word whose re-hand failed now goes
    behind the words that have not, and is tried again afterwards, never dropped."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text(shared + "\n")
    m.WANTED.write_text(("" if shared == "route" else shared) + "\n")   # the ask already applied
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    for sid, word in files.items():
        (m.SESSION_WORDS / sid).write_text(word + "\n")
    sw = m.Switcher()
    sw.reload("" if shared == "route" else shared)
    for t in range(400):
        sw.tend(t)
        if w.children and w.children[-1].rc is None:
            w.finish(w.children[-1], rc_=1 if w.children[-1].word in failing else 0)
    words = [c.word for c in w.children]
    assert words[:2] == expected_first                   # the failed word does not hold the queue
    assert all(c.rehand for c in w.children)             # upkeep writes no word
    wanted = {word for word in [*files.values(), shared] if word not in ("route", "account")}
    assert wanted <= set(words), f"{wanted - set(words)} never handed"
    for word in failing:
        assert words.count(word) >= 2, f"{word} was dropped after its failure"
    for word in set(words) - failing:
        assert words.count(word) == 1, f"{word} was re-handed though the proxy holds it"
    if failing >= set(words):
        assert all(a != b for a, b in zip(words, words[1:]))   # takes turns, no word twice in a row
    assert m.INFERENCE.read_text() == shared + "\n"             # upkeep wrote no word


def test_a_word_whose_rehand_fails_then_succeeds_is_held_and_forgets_its_mark(tmp_path, monkeypatch):
    """GH-977 edge: a word that failed and then goes through is held like any
    other and stops being re-handed; its failure mark is forgotten, so its next
    failure is ordered among equals, not behind a history it no longer has."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text("route\n")
    m.WANTED.write_text("\n")
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    (m.SESSION_WORDS / "cse_a").write_text("gone\n")
    (m.SESSION_WORDS / "cse_b").write_text("kimi\n")
    sw = m.Switcher()
    sw.reload("")
    sw.tend(0)
    assert [c.word for c in w.children] == ["gone"]
    w.finish(w.children[0], rc_=1)               # its first re-hand fails
    sw.tend(1)                                   # the failure is recorded, the next try waits
    assert sw.failed == {"gone": 1}
    sw.tend(3)                                   # the word that never failed goes first
    assert [c.word for c in w.children] == ["gone", "kimi"]
    w.finish(w.children[1])
    sw.tend(4)                                   # the failed word is tried again
    assert [c.word for c in w.children] == ["gone", "kimi", "gone"]
    w.finish(w.children[2])                      # this time it succeeds
    sw.tend(5)
    assert sw.failed == {}                       # its mark is forgotten
    for t in range(6, 10):
        sw.tend(t)
    assert len(w.children) == 3                  # held: nothing re-handed
    assert m.INFERENCE.read_text() == "route\n"


def test_a_rehand_of_a_word_with_its_own_edge_sets_no_backoff(tmp_path, monkeypatch):
    """A re-hand that exits REHAND_NOT_HELD (the word has its own live edge) is not
    a failed bring-up: it sets no backoff, so the next word is tried on the next tick."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text("route\n")
    m.WANTED.write_text("\n")
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    (m.SESSION_WORDS / "cse_a").write_text("gone\n")
    (m.SESSION_WORDS / "cse_b").write_text("kimi\n")
    sw = m.Switcher()
    sw.reload("")
    sw.tend(0)
    assert [c.word for c in w.children] == ["gone"]
    w.finish(w.children[0], rc_=m.REHAND_NOT_HELD)
    sw.tend(1)
    assert sw.failed == {"gone": 1} and sw.backoff == m.BACKOFF_MIN
    assert [c.word for c in w.children] == ["gone", "kimi"]   # no wait behind a backoff
    assert m.REHAND_NOT_HELD == m.rc_module().REHAND_NOT_HELD   # one exit code on both sides


def test_a_word_whose_rehand_fails_and_is_named_no_more_forgets_its_mark(tmp_path, monkeypatch):
    """GH-977 edge: when a session's file is gone, no word names that profile any
    more; its failure mark must not outlive it and order a later word behind a
    ghost."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text("route\n")
    m.WANTED.write_text("\n")
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    (m.SESSION_WORDS / "cse_a").write_text("gone\n")
    (m.SESSION_WORDS / "cse_b").write_text("kimi\n")
    sw = m.Switcher()
    sw.reload("")
    sw.tend(0)
    w.finish(w.children[0], rc_=1)
    sw.tend(1)                                   # the failure is recorded, the next try waits
    assert sw.failed == {"gone": 1}
    (m.SESSION_WORDS / "cse_a").unlink()         # its session is gone: no file names the word
    sw.tend(3)                                   # the next unheld word is handed, the mark dropped
    assert [c.word for c in w.children] == ["gone", "kimi"]
    assert sw.failed == {}
    w.finish(w.children[1])
    for t in range(4, 7):
        sw.tend(t)
    assert [c.word for c in w.children] == ["gone", "kimi"]   # the absent word is never retried
    assert m.INFERENCE.read_text() == "route\n"


def test_the_last_word_going_away_forgets_its_failure_mark(tmp_path, monkeypatch):
    """GH-977 edge: when the shared word is the route and the last session pick
    disappears, no word is named at all; the failure mark must go with it, so a
    later pick of that profile is ordered among equals, not behind a ghost."""
    m = load()
    w = _switcher(m, monkeypatch, tmp_path)
    w.edge("gateway", 500)
    m.INFERENCE.write_text("route\n")
    m.WANTED.write_text("\n")
    m.SESSION_WORDS.mkdir(parents=True, exist_ok=True)
    (m.SESSION_WORDS / "cse_a").write_text("gone\n")
    sw = m.Switcher()
    sw.reload("")
    sw.tend(0)
    w.finish(w.children[0], rc_=1)
    sw.tend(1)                                   # the failure is recorded
    assert sw.failed == {"gone": 1}
    (m.SESSION_WORDS / "cse_a").unlink()         # the last pick is gone: no word is named
    sw.tend(3)
    assert sw.failed == {}


def _two_account_profiles(state_dir: Path) -> None:
    """Two Claude account profiles on the machine: the state under which the
    one-shot `default_account()` refuses, and a refusal there is SystemExit."""
    state_dir.mkdir(parents=True, exist_ok=True)
    for name in ("acct1", "acct2"):
        (state_dir / f"{name}.conf").write_text("type=sub\n")


@pytest.mark.parametrize("mapping, named, hosted", [
    pytest.param("inference=acct1", True, True, id="ambiguous-account-issue-596"),
    pytest.param("# inference=acct1", False, False, id="comment-mentions-inference-issue-596"),
])
def test_a_config_reload_never_dies_on_account_resolution(tmp_path, monkeypatch, capsys,
                                                          mapping, named, hosted):
    """Two Claude account profiles made every config reload kill the supervisor:
    the reload branch called `default_account()`, a one-shot CLI helper whose
    refusal prints for a human holding a terminal and exits the process, and
    launchd's next start only deferred the same crash to the next edit (GH-596,
    a5b5339's branch). The old trigger was the substring `inference=` anywhere
    in the text, so a comment merely mentioning it resolved accounts too. Now
    resolution happens only for a real mapping and never by dying: an ambiguous
    account is no account match, so the word is brought up as a profile -- where
    `ccmorph-rc --switch` refuses into switch.log, not into the supervisor --
    and the ambiguity itself is named once in the supervisor's log."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    _two_account_profiles(tmp_path / "state")
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n{mapping}\n")
    popens = []
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: popens.append(cmd) or FakeProc())

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        if self.edge:
            _live_edge(m, self.route, 500)     # a bring-up waits for a live shared edge (#595)

    run_main(m, monkeypatch, tmp_path, cfg, start)
    out = capsys.readouterr().out
    assert ("no unique Claude account profile (acct1, acct2)" in out) is named
    assert (len(popens) == 1) is hosted
    if hosted:
        assert popens[0] == [str(m.HERE / "ccmorph-rc"), "acct1", "--switch"]
    else:     # a comment is no mapping: nothing resolved, the route word stands
        assert (tmp_path / "state" / "profiles" / ".rc-bridge" / "inference").read_text() == "route\n"


def test_a_signed_out_session_report_never_dies_on_account_resolution(tmp_path, monkeypatch, capsys):
    """Two Claude account profiles turned the signed-out 401 report into a crash
    loop: host_sessions() named the account in its fix line with default_account(),
    a one-shot CLI helper whose refusal exits the process, and the refusal is a
    SystemExit raised inside the very `except LOGIN_ERRORS` handler that caught the
    401, so it escaped the supervisor at every active check -- and since the flag
    landed after the log line, launchd's next start met the same crash (GH-805,
    the shape #596 recorded for the reload branch). The report must stand on its
    own: a fix line that names no account when none can be named, and the
    once-per-episode flag landing with it."""
    m = load()
    _two_account_profiles(tmp_path / "state")   # the real default_account() now refuses: SystemExit
    login = {"expiresAt": 0, "accessToken": "expired"}   # signed out: no refreshToken
    monkeypatch.setattr(m, "account_login", lambda: login)
    monkeypatch.setattr(m, "host_envs", lambda folders: {"env": "repo"})

    def unauthorized(token, status):
        raise m.urllib.error.HTTPError("url", 401, "Unauthorized", None, None)
    monkeypatch.setattr(m, "sessions", unauthorized)

    assert m.host_sessions(["repo"], "active") == ("", [])
    out = capsys.readouterr().out
    assert "active session check failed: HTTP Error 401: Unauthorized; fix: ccmorph login" in out
    assert m.host_sessions(["repo"], "active") == ("", [])   # once per episode
    assert "session check failed" not in capsys.readouterr().out


def test_account_name_resolves_one_profile_and_degrades_on_zero_or_several(monkeypatch, capsys):
    """GH-807: the supervisor's one account-name resolution, in inference_account's
    shape. One candidate names the account; zero is a silent no-op; several are no
    account at all, and the operator gets ONE line naming them per episode -- the
    hot paths that resolve this run every recovery cycle, so a line per call would
    bury the log it exists to reach."""
    m = load()
    candidates = [["acct"]]
    monkeypatch.setattr(m, "rc_module",
                        lambda: SimpleNamespace(account_candidates=lambda: candidates[0]))
    assert m.account_name() == "acct"
    assert capsys.readouterr().out == ""
    candidates[0] = []                     # nothing to pass --as yet: no line either
    assert m.account_name() == ""
    assert capsys.readouterr().out == ""
    candidates[0] = ["acct1", "acct2"]
    assert m.account_name() == ""
    out = capsys.readouterr().out
    assert out.count("no unique Claude account profile (acct1, acct2)") == 1
    assert m.account_name() == ""          # the same episode: one line, no more
    assert capsys.readouterr().out == ""
    candidates[0] = ["acct1", "acct2", "acct3"]   # a new set is a new episode
    assert m.account_name() == ""
    assert "no unique Claude account profile (acct1, acct2, acct3)" in capsys.readouterr().out


def test_inference_and_the_login_resolution_share_one_ambiguity_notice(tmp_path, capsys):
    """GH-807 merge-gate finding: inference_account() enumerated the candidates
    and logged its own ambiguity line, while account_name() did it again with a
    separate episode state -- so one episode that resolved an `inference=`
    mapping and then the login account produced two ambiguity notices. The
    reload's resolution now goes through account_name(), sharing both the
    resolution and the once-per-episode line, in either order."""
    _two_account_profiles(tmp_path / "state")
    m = load()
    assert m.inference_account("inference=acct1\n") == ""
    assert m.account_name() == ""
    notices = [line for line in capsys.readouterr().out.splitlines() if "acct1, acct2" in line]
    assert len(notices) == 1, notices
    m = load()                              # the other order shares the same episode too
    assert m.account_name() == ""
    assert m.inference_account("inference=acct1\n") == ""
    notices = [line for line in capsys.readouterr().out.splitlines() if "acct1, acct2" in line]
    assert len(notices) == 1, notices


def test_two_account_profiles_still_report_a_signed_out_login(tmp_path, monkeypatch, capsys):
    """GH-807, first #805 sibling: with two Claude account profiles the signed-out
    report called the one-shot account helper under `except LOGIN_ERRORS`, which
    swallowed its SystemExit -- no line, no flag, every retry silent forever. The
    fix must log the signed-out line (naming no account, since none can be named),
    set signed_out_reported, and still name the candidates once through the shared
    resolution."""
    m = load()
    _two_account_profiles(tmp_path / "state")
    login = {"expiresAt": 0, "accessToken": "expired"}   # signed out: no refreshToken
    monkeypatch.setattr(m, "account_login", lambda: login)
    clock = m.Clock()
    m.keep_login_fresh(clock, 0.0)
    out = capsys.readouterr().out
    assert out.count("no unique Claude account profile (acct1, acct2)") == 1
    assert "is signed out; fix: ccmorph login" in out
    assert m.signed_out_reported is True
    m.keep_login_fresh(clock, m.RECOVER_EVERY)     # the episode is reported once
    assert "signed out" not in capsys.readouterr().out


def test_two_account_profiles_still_retry_the_login_refresh(tmp_path, monkeypatch, capsys):
    """GH-807, second #805 sibling: with two Claude account profiles the refresh
    target's resolve raised the one-shot helper's SystemExit before Popen, and the
    guard swallowed it -- no line, no refresh, and nothing said why. Inside the
    refresh window the refusal must land once naming the candidates, no turn may
    start, and the next recovery cycle must read the login again rather than
    starve forever."""
    m = load()
    _two_account_profiles(tmp_path / "state")
    reads = []
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, [1000.0], m.time.sleep))

    def login():
        reads.append(1)
        return {"expiresAt": 1_300_000.0, "refreshToken": "refresh"}   # 300 s left
    monkeypatch.setattr(m, "account_login", login)
    runs = []
    monkeypatch.setattr(m.subprocess, "Popen", lambda *a, **kw: runs.append(a) or pytest.fail(
        "no refresh turn can run without a nameable account"))
    clock = m.Clock()
    m.keep_login_fresh(clock, 0.0)
    out = capsys.readouterr().out
    assert out.count("no unique Claude account profile (acct1, acct2)") == 1
    assert runs == []
    assert clock.last_refresh == float("-inf")     # no refresh was marked as attempted
    m.keep_login_fresh(clock, m.RECOVER_EVERY)
    assert len(reads) == 2                         # retried on the next cycle
    assert capsys.readouterr().out == ""           # one refusal line per episode


def test_no_unique_account_profile_is_a_readable_cause_at_every_keychain_reader(
        tmp_path, monkeypatch, capsys):
    """GH-807: keychain_service() resolved through the one-shot helper, so with two
    account profiles its refusal surfaced at four sites as the bare `1` of
    SystemExit's str. The cause must name the candidates, and each site must print
    it instead of `failed: 1`."""
    m = load()
    _two_account_profiles(tmp_path / "state")
    with pytest.raises(ValueError) as raised:
        m.keychain_service()
    assert "no unique Claude account profile (acct1, acct2)" in str(raised.value)
    monkeypatch.setattr(m, "host_envs", lambda folders: {"env": "repo"})

    class Host:
        edge = False
        proc = object()                # running here, so restart_logged_out checks it
        folder = "repo"

        def lost_login(self):
            return "No access token for session fetch"

        def mark_logs(self):
            pass

    m.host_sessions(["repo"], "active")
    m.restart_logged_out({"repo": Host()}, 0.0)
    m.purge_archived(["repo"], 0.0, 1.0)
    assert m.delete_host(Host()) is False
    out = capsys.readouterr().out
    cause = "no unique Claude account profile (acct1, acct2)"
    for prefix in ("active session check failed: ", "archived session purge failed: ",
                   "host not deleted: ", "account login is unreadable too ("):
        assert any(prefix in line and cause in line for line in out.splitlines()), prefix
    assert "failed: 1" not in out and "unreadable too (1)" not in out


def test_the_supervisor_never_calls_the_one_shot_account_helper():
    """GH-807 kind-level guard (#596's shape): ccmorph-rc-hosts is a long-lived
    supervisor, and the one-shot account helper refuses by exiting the process.
    A guard around such a call prevents the crash only -- it swallows the refusal's
    message and whatever work the caller was about to do. Every site resolves
    through account_name(), fed by account_candidates()."""
    source = SCRIPT.read_text()
    assert "default_account(" not in source, (
        "ccmorph-rc-hosts must not call default_account(): its refusal is a SystemExit "
        "that a surrounding guard swallows (GH-807). Resolve through account_name(), "
        "fed by rc_module().account_candidates().")


def test_a_first_config_with_inference_creates_its_switch_log(tmp_path, monkeypatch):
    """A fresh state dir whose first config carries `inference=<profile>` died
    FileNotFoundError on its first reload: the bring-up Popen's stderr opened
    LOGS/switch.log, but LOGS is created only inside Host.start, which runs
    later in the same tick -- the #419 comment already warns the mkdir sits
    past the early returns, and the reload branch walked into the same hole
    (GH-596). The supervisor must live, and the log it wants must be created
    on demand, mkdir first."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\ninference=oli\n")
    popens = []
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: popens.append(cmd) or FakeProc())

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        if self.edge:
            _live_edge(m, self.route, 500)     # a bring-up waits for a live shared edge (#595)

    run_main(m, monkeypatch, tmp_path, cfg, start, logs=False)   # no host ever started: no LOGS
    assert popens == [[str(m.HERE / "ccmorph-rc"), "oli", "--switch"]]
    assert (tmp_path / "logs" / "switch.log").exists()


def test_two_concurrent_write_inference_callers_each_land_a_whole_word(tmp_path):
    """write_inference used one shared `<target>.tmp`, and both the supervisor
    and a manual `--switch` write STATE/inference: one writer's replace() could
    rename the other's half-written temp away (FileNotFoundError, the same
    uncaught path as the reload-death), or the two O_TRUNC writes could
    interleave into a torn word that inference_target silently reads as "route"
    (GH-596). Two writers through the real seam must each land a whole word and
    leave no temp behind. The loop count manufactures contention, not a timing
    bound: with unique temps every interleave satisfies these assertions."""
    from concurrent.futures import ThreadPoolExecutor
    mod = load()
    rc = mod.rc_module()
    path = tmp_path / "inference"
    errors = []

    def writer(word):
        for _ in range(200):
            try:
                rc.write_inference(word, path)
            except OSError as e:
                errors.append(e)

    with ThreadPoolExecutor(max_workers=2) as pool:
        list(pool.map(writer, ("firstword", "secondword")))
    assert errors == []
    assert path.read_text() in ("firstword\n", "secondword\n")
    assert [p.name for p in tmp_path.iterdir() if p != path] == []


def test_a_failed_write_inference_raises_its_own_error_and_leaves_no_temp(tmp_path, monkeypatch):
    """write_inference cleans a failed write up under `suppress(OSError)`, and
    ccmorph-rc never imported suppress: when os.replace failed, the cleanup
    itself raised NameError, which masked the real write failure and left
    `inference.<unique>.tmp` beside the switch file (GH-596, merge-gate
    finding). A failed write must re-raise its own OSError, remove its temp,
    and leave the previous word in place, so every proxy keeps reading the
    last whole word."""
    rc = load().rc_module()
    path = tmp_path / "bridge" / "inference"
    path.parent.mkdir()
    path.write_text("route\n")
    injected = OSError("injected: the disk is full")

    def refuse(src, dst):
        raise injected

    with monkeypatch.context() as mp:
        mp.setattr(os, "replace", refuse)
        with pytest.raises(OSError) as raised:
            rc.write_inference("account", path)
    assert raised.value is injected
    assert path.read_text() == "route\n"
    assert sorted(p.name for p in path.parent.iterdir()) == ["inference"]


@pytest.mark.skipif(os.geteuid() == 0, reason="root writes into a mode-0500 directory")
def test_a_refused_inference_word_does_not_take_the_supervisor_down(tmp_path, monkeypatch, capsys):
    """GH-845: the reload branch called rc_module().write_inference() outside
    every guard, and write_inference re-raises the OSError its cleanup cannot
    swallow (pinned at #596). A full disk or a read-only `.rc-bridge/` therefore
    ended main(), and launchd restarted the supervisor into the same crash. The
    failure is one log line naming the cause, the tick continues, and the word
    is retried on a later tick until the bridge takes it."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n")     # no inference= line: the word is route
    bridge = tmp_path / "bridge"
    bridge.mkdir()
    bridge.chmod(0o500)                            # the bridge dir refuses every write
    monkeypatch.setattr(m, "INFERENCE", bridge / "inference")

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()

    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 2:
            bridge.chmod(0o700)                    # the operator repairs the bridge dir
        if ticks[0] > 4:
            raise SystemExit

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep)
    out = capsys.readouterr().out
    assert out.count("inference route: not applied") == 1   # retried without a line per tick
    assert "Permission denied" in out
    assert (bridge / "inference").read_text() == "route\n"  # the retry landed the word


def test_a_missing_switch_launcher_does_not_take_the_supervisor_down(tmp_path, monkeypatch, capsys):
    """GH-845, fresh-start shape (merge-gate finding): the reload resolves the
    inference word through account_candidates(), which imports HERE/ccmorph-rc.
    A launchd restart of a partial install therefore raised FileNotFoundError
    before the bring-up guard could run, and the first tick never finished. The
    resolution is retried every tick; once the launcher returns, the same tick
    resolves the word and brings the profile up."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\ninference=oli\n")
    real_here, real_edge_ready = m.HERE, m.edge_ready
    addons = tmp_path / "addons"                   # no ccmorph-rc beside the script
    monkeypatch.setattr(m, "HERE", addons)
    popens = []
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 3:                          # the launcher returns after two more ticks
            monkeypatch.setattr(m, "HERE", real_here)
            monkeypatch.setattr(m.subprocess, "Popen",
                                lambda cmd, **kw: popens.append(cmd) or FakeProc())
            monkeypatch.setattr(m, "edge_ready", lambda route: True)   # the fake edge, now readable
        if ticks[0] > 5:
            raise SystemExit

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        if self.edge:
            _live_edge(m, self.route, 500)     # a bring-up waits for a live shared edge (#595)

    # The real edge_ready() runs too: it loads the same launcher module from HERE.
    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep, edge_ready=real_edge_ready)
    out = capsys.readouterr().out
    assert out.count("inference: cannot resolve the word") == 1   # retried without a line per tick
    assert out.count("gateway: cannot read its edge") == 1        # the same missing module, once
    assert str(addons / "ccmorph-rc") in out and "No such file or directory" in out
    assert popens == [[str(real_here / "ccmorph-rc"), "oli", "--switch"]]   # the retry brought it up


def test_a_missing_switch_launcher_at_bring_up_is_logged_and_retried(tmp_path, monkeypatch, capsys):
    """GH-845, the running supervisor's shape (merge-gate finding): the launcher
    module is already loaded, so the word resolves and the --switch Popen is the
    site that meets the missing binary. It is guarded, logged once with the cause
    and retried; the test fails if the handler re-raises its FileNotFoundError,
    which the fresh-start test above cannot see because its Popen is only reached
    after HERE is restored."""
    m = load()
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\ninference=oli\n")
    (tmp_path / "logs").mkdir()
    switch = tmp_path / "logs" / "switch.log"
    switch.write_text("old\n")
    before = os.stat(switch).st_mtime - 2 * 86400  # inside the week: the boot sweep keeps it
    os.utime(switch, (before, before))
    real_here = m.HERE
    m.rc_module()                                  # loaded once per run; only the binary goes missing
    addons = tmp_path / "addons"                   # no ccmorph-rc beside the script
    monkeypatch.setattr(m, "HERE", addons)
    popens = []
    ticks = [0]
    refused_mtime = []
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 3:                          # the launcher returns after two more tries
            refused_mtime.append(os.stat(switch).st_mtime)
            monkeypatch.setattr(m, "HERE", real_here)
            monkeypatch.setattr(m.subprocess, "Popen",
                                lambda cmd, **kw: popens.append(cmd) or FakeProc())
        if ticks[0] > 5:
            raise SystemExit

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()
        if self.edge:
            _live_edge(m, self.route, 500)     # a bring-up waits for a live shared edge (#595)

    run_main(m, monkeypatch, tmp_path, cfg, start, sleep=sleep)
    out = capsys.readouterr().out
    assert out.count("inference oli: not applied") == 1   # retried without a line per tick
    assert str(addons / "ccmorph-rc") in out and "No such file or directory" in out
    assert popens == [[str(real_here / "ccmorph-rc"), "oli", "--switch"]]   # the retry brought it up
    # #758: the refused launches started no writer, so they left switch.log's age
    # alone for the sweep; only the launch that ran stamped it
    assert refused_mtime == [before]
    assert os.stat(switch).st_mtime > before + 86400


@pytest.mark.skipif(os.geteuid() == 0, reason="root writes into a mode-0500 directory")
def test_a_read_only_bridge_does_not_end_the_tick_at_host_start(tmp_path, monkeypatch, capsys):
    """GH-845, merge-gate finding: guarding the inference write was not enough
    on a read-only `.rc-bridge/`. The same tick's Host.start() created
    `.rc-bridge/logs` and let PermissionError escape tend(), so no later tick
    retried and launchd restarted the supervisor into the same crash. The start
    is guarded where tend() calls it: one log line names the cause, the tick
    continues, and the retries land both the word and the edge start once the
    bridge is writable. The real Host.start() runs here: the refusal happens
    before any launcher is exec'd."""
    m = load()
    bridge = tmp_path / "state" / "profiles" / ".rc-bridge"   # the real LOGS/EDGES home
    bridge.mkdir(parents=True)
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\n")
    monkeypatch.setattr(m, "read_profile_tiers", lambda route: {}, raising=False)
    monkeypatch.setattr(m, "BACKOFF_MIN", 0)       # every tick may retry, no clock to wait on
    popens = []
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: popens.append(cmd) or FakeProc())
    ticks = [0]
    def sleep(_):
        ticks[0] += 1
        if ticks[0] == 2:
            bridge.chmod(0o700)                    # the operator repairs the bridge
        if ticks[0] > 4:
            raise SystemExit
    monkeypatch.setattr(m, "time", _ScriptedTime(m.time, None, sleep))
    bridge.chmod(0o500)
    try:
        with pytest.raises(SystemExit):
            m.main(["--config", str(cfg)])
        out = capsys.readouterr().out
        assert out.count("inference route: not applied") == 1     # the reload's write refused
        assert out.count(f"{bridge}: cannot start") == 1          # the edge's LOGS mkdir refused
        assert "Permission denied" in out
        assert (bridge / "inference").read_text() == "route\n"    # both retried after the repair
        assert "started via gateway" in out                      # the edge's start got through
        assert popens and popens[-1][-1] == "--edge"
    finally:
        bridge.chmod(0o700)


@pytest.mark.parametrize("damage, cause", [
    pytest.param("mode-000", "Permission denied", id="mode-000-conf"),
    pytest.param("invalid-utf8", "codec can't decode", id="undecodable-conf"),
])
def test_an_unreadable_profile_conf_does_not_take_the_supervisor_down(tmp_path, monkeypatch,
                                                                     capsys, damage, cause):
    """GH-845, the #596 class one level down: profile_field read every
    `*.conf` in the state root with no guard, so one unreadable (mode 000) or
    invalid-UTF-8 conf made account_candidates() raise at every config reload
    and launchd restarted the supervisor into the same crash. An unreadable
    conf is no field: the readable account still resolves, the reload writes
    its word, and the cause is named on stderr (merged into rc-hosts.log)."""
    if damage == "mode-000" and os.geteuid() == 0:
        pytest.skip("root reads a mode-0 file")
    m = load()
    state = tmp_path / "state"                     # the autouse CCMORPH_STATE_DIR
    state.mkdir()
    (state / "acct.conf").write_text("type=sub\n")
    bad = state / "bad.conf"
    if damage == "mode-000":
        bad.write_text("type=sub\n")
        bad.chmod(0)
    else:
        bad.write_bytes(b"type=sub\n\xff\n")
    repo = tmp_path / "repo"
    repo.mkdir()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gateway\n{repo}\ninference=acct\n")
    popens = []
    monkeypatch.setattr(m.subprocess, "Popen", lambda cmd, **kw: popens.append(cmd) or FakeProc())

    def start(self):
        self.proc, self.started = FakeProc(), m.time.time()

    run_main(m, monkeypatch, tmp_path, cfg, start)
    err = capsys.readouterr().err
    assert "bad.conf" in err and "unreadable" in err and cause in err
    assert popens == []                            # acct resolved: the word is account, no bring-up
    assert (state / "profiles" / ".rc-bridge" / "inference").read_text() == "account\n"


def test_profile_field_reads_an_unreadable_conf_as_no_field(tmp_path, capsys):
    """GH-845: profile_field is the one reader of a profile's .conf, so the
    guard belongs there rather than at each caller. A conf that cannot be read
    (mode 000) or decoded is no field; a missing conf is still no field with no
    complaint, and a readable one still answers."""
    rc = load().rc_module()                        # STATE_ROOT is the autouse state dir
    state = tmp_path / "state"
    state.mkdir()
    (state / "ok.conf").write_text("type=sub\nmodel=m-1\n")
    bad = state / "badutf.conf"
    bad.write_bytes(b"type=sub\n\xff\n")
    mode0 = state / "mode0.conf"
    mode0.write_text("type=sub\n")
    if os.geteuid() != 0:                          # root reads a mode-0 file
        mode0.chmod(0)
    assert rc.profile_field("ok", "type") == "sub"
    assert rc.profile_field("ok", "model") == "m-1"
    assert rc.profile_field("missing", "type") == ""
    assert rc.profile_field("badutf", "type") == ""
    if os.geteuid() != 0:
        assert rc.profile_field("mode0", "type") == ""
    err = capsys.readouterr().err
    assert "badutf.conf" in err and "unreadable" in err and "codec can't decode" in err
    if os.geteuid() != 0:
        assert "mode0.conf" in err and "Permission denied" in err
    assert "missing.conf" not in err               # absent is not a failure


def test_the_session_list_names_each_sessions_own_word_and_drops_stale_ones(monkeypatch):
    """A phone control picks a profile per session from `--list-sessions`. A word whose
    session is archived and not running is removed; a failed fetch removes nothing."""
    m = load()
    m.SESSION_WORDS.mkdir(parents=True)
    (m.SESSION_WORDS / "cse_live").write_text("gpro\n")
    (m.SESSION_WORDS / "cse_gone").write_text("ds\n")
    (m.SESSION_WORDS / "cse_running").write_text("ds\n")
    monkeypatch.setattr(m, "account_name", lambda: "me")
    monkeypatch.setattr(m, "session_pid", lambda sid: 7 if sid == "cse_running" else None)
    monkeypatch.setattr(m, "host_sessions", lambda folders, status: ("", []))
    assert m.list_sessions(["repo"]) is None
    assert sorted(p.name for p in m.SESSION_WORDS.iterdir()) == ["cse_gone", "cse_live", "cse_running"]
    found = [("repo", {"id": "cse_live", "title": "Fix it", "worker_status": "running",
                       "connection_status": "connected"}), ("repo", {"id": "cse_new"})]
    monkeypatch.setattr(m, "host_sessions", lambda folders, status: ("tok", found))
    assert m.list_sessions(["repo"]) == {"account": "me", "sessions": [
        {"id": "cse_live", "title": "Fix it", "folder": "repo", "word": "gpro", "working": True, "connected": True},
        {"id": "cse_new", "title": "", "folder": "repo", "word": None, "working": False, "connected": False}]}
    assert sorted(p.name for p in m.SESSION_WORDS.iterdir()) == ["cse_live", "cse_running"]


def test_list_sessions_prints_only_json_and_asks_only_about_repo_hosts(tmp_path, monkeypatch, capsys):
    """Stark parses stdout: log() writes there too, so a line the fetch logs goes to
    stderr. A pinned conversation line is not a repo host and is not asked about."""
    import json
    m = load()
    cfg = tmp_path / "rc-hosts"
    cfg.write_text(f"route=gw\n{tmp_path / 'a'}\n{tmp_path / 'b'} -- --session-id cse_pin\n")
    asked = []

    def fetch(folders, status):
        asked.append(folders)
        m.log("active session check failed: once")
        return "tok", []
    monkeypatch.setattr(m, "host_sessions", fetch)
    monkeypatch.setattr(m, "account_name", lambda: "me")
    assert m.main(["--config", str(cfg), "--list-sessions"]) == 0
    out = capsys.readouterr()
    assert json.loads(out.out) == {"account": "me", "sessions": []}
    assert "check failed" in out.err
    assert asked == [[str(tmp_path / "a")]]


def test_session_titles_name_every_session_active_and_archived(monkeypatch, capsys):
    """A cost ledger keyed by transcript names each session by its title: `--session-titles`
    prints both lists as JSON on stdout alone; an active listing wins over an archived one."""
    import json
    m = load()
    monkeypatch.setattr(m, "account_token", lambda: "tok")
    lists = {"active": [{"id": "cse_a", "title": "Stark UI"}],
             "archived": [{"id": "cse_b", "title": "Old"}, {"id": "cse_a", "title": "stale"}, {"title": "no id"}]}

    def fetch(token, status):
        m.log("fetched " + status)
        return lists[status]
    monkeypatch.setattr(m, "sessions", fetch)
    assert m.main(["--session-titles"]) == 0
    out = capsys.readouterr()
    assert json.loads(out.out) == {"sessions": {"cse_a": {"title": "Stark UI", "archived": False},
                                                "cse_b": {"title": "Old", "archived": True}}}
    assert "fetched" in out.err

    def fail(token, status):
        raise OSError("offline")
    monkeypatch.setattr(m, "sessions", fail)
    assert m.main(["--session-titles"]) == 1
    assert capsys.readouterr().out == ""


@pytest.mark.parametrize("dies", ["reply", "login"])
def test_session_titles_degrades_when_the_login_or_reply_dies(monkeypatch, capsys, dies):
    """The two failures this module already owns at LOGIN_ERRORS — a cut-off reply
    (http.client.IncompleteRead) and a hung `security` lookup (subprocess.TimeoutExpired)
    — are neither an OSError, so a narrower except in session_titles() let them escape
    as a traceback where the contract says: exit 1, one line on stderr (#1001).
    One case per run: a stub left from the other would answer before the patched
    failure is reached (a live account_token stub answers before account_login)."""
    import http.client
    import subprocess
    m = load()
    if dies == "reply":
        monkeypatch.setattr(m, "account_token", lambda: "tok")

        def cut(token, status):
            raise http.client.IncompleteRead(b"")
        monkeypatch.setattr(m, "sessions", cut)
    else:
        def hung():
            raise subprocess.TimeoutExpired("security", 10)
        # only account_login is patched: the real account_token calls it, so the
        # TimeoutExpired travels the same chain a real hung `security` would.
        monkeypatch.setattr(m, "account_login", hung)
    assert m.main(["--session-titles"]) == 1
    out = capsys.readouterr()
    assert out.out == ""
    assert "ccmorph-rc-hosts: the sessions could not be read" in out.err
