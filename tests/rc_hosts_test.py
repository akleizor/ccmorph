"""ccmorph-rc-hosts: config parsing and the relaunch policy."""
import importlib.machinery
import importlib.util
import os
from pathlib import Path

import pytest

from support import HANG_TIMEOUT

SCRIPT = Path(__file__).resolve().parent.parent / "addons" / "remote-control" / "ccmorph-rc-hosts"


def load():
    loader = importlib.machinery.SourceFileLoader("rc_hosts", str(SCRIPT))
    spec = importlib.util.spec_from_loader("rc_hosts", loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


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
             edge_ready=lambda route: True, clock=None, args=None):
    """Drive m.main() over cfg until the scripted sleep raises SystemExit: logs
    under tmp_path, Host.start faked by `start`, the clock read from `clock`
    when given. `args` replaces the `--config` argument list main() gets.
    Without a `sleep`, main() stops on its fifth sleep."""
    monkeypatch.setattr(m, "LOGS", tmp_path / "logs")
    (tmp_path / "logs").mkdir(exist_ok=True)
    monkeypatch.setattr(m, "edge_ready", edge_ready)
    monkeypatch.setattr(m.Host, "start", start)
    if clock is not None:
        monkeypatch.setattr(m.time, "time", lambda: clock[0])
    if sleep is None:
        ticks = [0]
        def sleep(_):
            ticks[0] += 1
            if ticks[0] > 4:
                raise SystemExit
    monkeypatch.setattr(m.time, "sleep", sleep)
    with pytest.raises(SystemExit):
        m.main(args if args is not None else ["--config", str(cfg)])


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
    monkeypatch.setattr(m.time, "sleep",
                        lambda _: pytest.fail("a usage error exits before the loop"))
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
    assert m.LOGS.is_relative_to(tmp_path) and m.EDGES.is_relative_to(tmp_path)
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
        def default_account(): return "acct"
        @staticmethod
        def profile_env(p): return {"CLAUDE_CONFIG_DIR": "/x/acct"}

    monkeypatch.setattr(m.importlib.machinery.SourceFileLoader, "load_module", lambda self: FakeRc)
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


def test_a_live_edge_is_the_record_its_own_file_holds(tmp_path, monkeypatch):
    """The record is what the adoption decision compares, so the reader returns it,
    and only for an edge that is both alive and answering."""
    import json, os, socket
    m = load()
    monkeypatch.setattr(m, "EDGES", tmp_path)
    with socket.socket() as srv:
        srv.bind(("127.0.0.1", 0))
        srv.listen()
        record = {"pid": os.getpid(), "port": srv.getsockname()[1], "ca": "x",
                  "upstream": "u", "tiers": {"fable": "m"}}
        (tmp_path / "gw.json").write_text(json.dumps(record))
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
    monkeypatch.setattr(m.time, "time", lambda: clock[0])
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
    for f in (h.debugfile, child):
        f.write_bytes(b"x" * (m.LOG_CAP + 1))
    ino = h.debugfile.stat().st_ino
    h.bound_logs()
    assert h.debugfile.stat().st_size == 0 and h.debugfile.stat().st_ino == ino
    assert child.stat().st_size == 0
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
    monkeypatch.setattr(m.time, "time", lambda: clock[0])
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
    monkeypatch.setattr(m.time, "time", lambda: clock[0])
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
    monkeypatch.setattr(m.time, "time", lambda: clock[0])
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
    monkeypatch.setattr(m.time, "time", lambda: clock[0])
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

    def serve():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return
            c.recv(65536)
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


def test_purge_days_is_a_setting_not_a_folder():
    m = load()
    text = "route=gw\nidle_days=1\npurge_days=7\n~/proj\n"
    assert all("purge_days" not in folder for folder in m.parse(text))
    assert m.purge_days(text) == 7.0


def is_python_helper(text):
    """A python shebang, or a uv inline script (its shebang need not name python)."""
    shebang = text.splitlines()[0] if text.startswith("#!") else ""
    return "python" in shebang or ("uv run --script" in shebang and "# /// script" in text)


def test_every_shipped_python_helper_uses_nothing_newer_than_the_python_38_floor():
    """The add-on declares 3.8 (CI python-floor); str.removeprefix/removesuffix, functools.cache
    and dict | dict are 3.9+ and broke imports there (2026-10). Static, so it fails on any
    interpreter. One rule set over every tracked file with a python shebang."""
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
        if not is_python_helper(text):
            continue
        seen.add(name)
        for node in ast.walk(ast.parse(text)):
            if isinstance(node, ast.Attribute) and node.attr in ("removesuffix", "removeprefix"):
                bad.append(f"{name} line {node.lineno}: str.{node.attr}")
            if isinstance(node, ast.Attribute) and node.attr == "cache" \
                    and getattr(node.value, "id", "") == "functools":
                bad.append(f"{name} line {node.lineno}: functools.cache")
            if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr) and (
                    getattr(node.left, "attr", "") == "environ"
                    or isinstance(node.right, (ast.Dict, ast.DictComp))):
                bad.append(f"{name} line {node.lineno}: dict | dict")
    assert len(seen) >= 8, seen
    assert not bad, bad


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
