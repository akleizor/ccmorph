"""The adapter-route defaults vs a settings layer that sets the key to "".

`_ccp_launch_diagnostics` decides whether the settings layers already make a
choice for one of the three adapter-route defaults. Its own rule is that only a
NON-EMPTY string is a real choice: Claude Code reads an empty value as unset, so
counting `""` as a choice both skips the default and leaves the engine with
nothing -- the mid-conversation-system turn, the advisor tool and the per-turn
effort all come back, which is the thrash the default exists to prevent. Its
answer reaches `_ccp_launch` as a `chosen` record from the one interpreter run
per launch, #332.

A real launch is the only place this is observable: the predicate is one step
inside `_ccp_launch`, and the generated settings.json carries the layer. Each
case copies the shipped library into an isolated state dir, stubs `claude`,
`curl` and the adapter, and launches a routed profile offline.
"""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time

import fake_adapter
import pytest

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]
LIB_FILES = ('libexec/model-window', 'libexec/settings-compose', 'lib/ccproteus.sh',
             *sorted(f'lib/{p.name}' for p in (ROOT / 'lib').glob('ccproteus-*.zsh')),
             'share/window-sources.json', 'share/model-catalogue.json')

# The exact default the launch path installs for every adapter route.
ROUTE_DEFAULT = '-mid_conv_system,-mid_conv_tool_change,-per_turn_effort'

# How many python3 interpreters one routed launch may start, counted through a
# PATH wrapper: 15 before the read-only diagnostics were folded into one (#332),
# 11 after; 10 and 9 once the send id moved onto the fields line; 8 and 7 once
# the window facts join one run and the seeded default stops costing a read
# (#333); 7 and 6 once the hazard warning is asked for as one more record from
# that same run instead of a run of its own (#433, and a hazard-free launch pays
# nothing for it); 6 and 5 once the store that reads the conf generates the
# settings in the same start, instead of the launch paying a second one for the
# projection (#433). The interpreter start is what a launch spends nearly all of
# its wall time on (#321), so the number is pinned rather than timed: a clock
# reading moves with host load, a start count does not. A new interpreter in the
# launch path updates this number; it is the count of every start, not of a
# subset.
#
# Four shapes, four numbers. A launch that names a model also resolves a send_id,
# the one interpreter start that shape adds; one that names none still reads the
# window facts -- a routed launch needs its ceiling -- but resolves no send_id. A
# gateway profile pays one more than a token one whatever it names: reaching the
# gateway's model list is its own interpreter, and this issue leaves that lookup
# alone (it is the start the no-model shape pays for its saved default too). A
# launch whose route trips the hazard warning pays one more again: rendering the
# advisory into CCPROTEUS_ADVISE is an interpreter of its own, and #433 leaves
# that encoder alone. Pinning each keeps a fix to one shape from paying for
# another with a new interpreter.
PYTHON_STARTS_PER_LAUNCH = 6
PYTHON_STARTS_NO_MODEL = 5
PYTHON_STARTS_TOKEN = 4
PYTHON_STARTS_HAZARD = 7

# GH-433 cuts interpreter starts out of the launch path, which is only safe if the
# launch still writes what it wrote. One golden holds the whole observable output
# of five profile shapes, generated once on the pre-fix tree with GH433_REGEN=1
# and compared unchanged afterwards (test_a_launch_writes_what_it_wrote_before_gh433).
GH433_REGEN = 'GH433_REGEN'
GH433_SNAPSHOT = ROOT / 'tests/fixtures/gh433_launch_snapshot.json'
# The values a launch legitimately inherits from whoever ran it: the shell's own
# bookkeeping (`PWD`, `SHLVL`, `_` and friends), the account name zsh reads out of
# the caller's passwd entry, and the directory this host keeps its temporaries in.
# Nothing else may move between two runs of the same tree, or the golden stops
# pinning the launch and starts pinning the machine.
GH433_VOLATILE = ('PWD', 'OLDPWD', 'SHLVL', '_', 'TMPDIR', 'LOGNAME')
# And the one value the launcher mints fresh on every launch. It cannot be
# compared -- it is a new id each time -- so the golden pins that the launch sets
# the key, and nothing about what it set it to.
GH433_MINTED = ('CCPROTEUS_RELAY_ID',)

CLAUDE = '''#!/bin/sh
case "$1" in --help) exit 0 ;; esac
# The environment the launch handed this stub, straight from the shell: the
# interpreter below is Xcode's python3 on a Mac with the developer tools, and it
# exports SDKROOT, CPATH, LIBRARY_PATH and MANPATH into its own process, so an env
# read inside it pins the recorder's toolchain instead of the launch (GH-433).
env > "$HOME/launch_env.txt"
python3 - "$@" <<'PY' >> "$HOME/launches.jsonl"
import json, os, sys
doc = {}
path = os.path.join(os.environ.get('CLAUDE_CONFIG_DIR', ''), 'settings.json')
if os.path.exists(path):
    doc = json.load(open(path))
print(json.dumps({'argv': sys.argv[1:],
                  'caps': os.environ.get('CLAUDE_CODE_MODEL_CAPABILITIES'),
                  'advisor': os.environ.get('CLAUDE_CODE_DISABLE_ADVISOR_TOOL'),
                  'effort': os.environ.get('CLAUDE_CODE_ALWAYS_ENABLE_EFFORT'),
                  'pct': os.environ.get('CLAUDE_AUTOCOMPACT_PCT_OVERRIDE'),
                  'settings_env': doc.get('env', {})}))
PY
'''
CURL = '''#!/bin/sh
for a do case "$a" in *http_code*) echo 200; exit 0 ;; esac; done
exit 1
'''
# The launcher relies on the adapter's own --parent-pid poll to stop it
# (ccproteus.sh _ccp_adapter_start): a stub that ignores the flag outlives a
# hard-killed launch forever (issues #385/#386, GH-330).
ADAPTER = fake_adapter.script('echo PORT=45999')

# The adapter route every one of these launches must reach.
GW_CONF = 'type=apikey\nbase_url=https://gw.invalid\napi_key=k\nadapter=true\nmodel=claude-sonnet-4-5\n'
# The same route naming no model at all: the profile's saved default decides.
GW_CONF_NO_MODEL = GW_CONF.replace('model=claude-sonnet-4-5\n', '')
# A profile on the subscription: no adapter, no gateway, one model named.
TOKEN_CONF = 'type=token\ntoken=t\nmodel=claude-sonnet-4-5\n'

# The gateway route again, now with the walls its siblings answer with: the launch
# compacts at 135k (200k window on the profile's own model, 75%), and one sibling
# is below that -- so the hazard advisory fires. The walls ride the conf, so the
# numbers the advisory prints do not depend on a probe (GH-433).
HAZARD_CONF = (GW_CONF
               + 'context_window.claude-sonnet-4-5=200000\n'
               + 'context_window.gpt-6-astra=400000\n'
               + 'context_window.kimi-k3=40000\n')
# The three the route offers; the ceiling comes from the largest wall among the
# non-claude ids, the advisory's rows from every offered model but the aliases.
HAZARD_MODELS = ['claude-sonnet-4-5', 'gpt-6-astra', 'kimi-k3']


class Launch:
    """One isolated HOME: the shipped library, stubbed tools, a routed profile."""

    def __init__(self, home: Path):
        self.home = home
        self.state = home / '.config/ccproteus'
        self.state.mkdir(parents=True)
        (home / 'bin').mkdir()
        (home / '.claude').mkdir()
        for name, body in (('claude', CLAUDE), ('curl', CURL)):
            (home / 'bin' / name).write_text(body)
            (home / 'bin' / name).chmod(0o755)
        for name in LIB_FILES:
            (self.state / name).parent.mkdir(exist_ok=True)
            shutil.copy(ROOT / name, self.state / name)
        (self.state / 'libexec/ccproteus-adapter').write_text(ADAPTER)
        (self.state / 'libexec/ccproteus-adapter').chmod(0o755)
        (self.state / 'gw.conf').write_text(GW_CONF)
        # PATH carries no directory of this host's: the environment a launch is
        # handed is compared byte for byte against a golden (GH-433), and
        # appending wherever zsh happens to live turned one stored PATH into a
        # macOS PATH and a Linux one that could never match. zsh is in /bin or
        # /usr/bin on every host this repo runs its suites on, and both are here.
        self.env = {'HOME': str(home), 'PATH': f'{home}/bin:/usr/bin:/bin',
                    'LANG': 'C.UTF-8', 'XDG_CONFIG_HOME': str(home / '.config'),
                    'CCPROTEUS_STATE_DIR': str(self.state), 'CCPROTEUS_LIB_DIR': str(self.state / 'lib')}
        assert shutil.which('zsh', path=self.env['PATH']), f'no zsh on {self.env["PATH"]}'
        self.last = None

    def canonical_settings(self, env_block: dict) -> None:
        """The user's own ~/.claude/settings.json -- the canon layer."""
        (self.home / '.claude/settings.json').write_text(json.dumps({'env': env_block}))

    def profile_settings(self, env_block: dict) -> None:
        """The profile's layer, the file settings-compose merges over canon."""
        layer = self.state / 'profiles/gw/settings.profile.json'
        layer.parent.mkdir(parents=True, exist_ok=True)
        layer.write_text(json.dumps({'env': env_block}))

    def run(self) -> dict:
        result = subprocess.run([str(ROOT / "bin/ccproteus"), 'run', 'gw'], env=self.env,
                                stdin=subprocess.DEVNULL, capture_output=True, text=True,
                                timeout=HANG_TIMEOUT)
        self.last = result
        assert result.returncode == 0, result.stdout + result.stderr
        log = self.home / 'launches.jsonl'
        assert log.exists(), result.stdout + result.stderr
        rows = [json.loads(line) for line in log.read_text().splitlines()]
        assert len(rows) == 1, rows
        return rows[0]


@pytest.fixture
def launch(tmp_path):
    made = Launch(tmp_path)
    yield made
    # The launch leaves its adapter stub detached, and `--parent-pid` only
    # reaches it while the launcher's number stays dead (#347). The stub records
    # what it started as; this kills that exact pid.
    fake_adapter.reap(made.state / 'libexec/ccproteus-adapter')


def test_empty_caps_in_the_canon_layer_still_gets_the_route_default(launch):
    launch.canonical_settings({'CLAUDE_CODE_MODEL_CAPABILITIES': ''})
    row = launch.run()
    assert row['caps'] == ROUTE_DEFAULT, \
        'an empty string is not a choice: the adapter route must still be withheld the turn'


def test_empty_caps_in_the_profile_layer_still_gets_the_route_default(launch):
    launch.profile_settings({'CLAUDE_CODE_MODEL_CAPABILITIES': ''})
    row = launch.run()
    assert row['caps'] == ROUTE_DEFAULT, \
        'an empty string is not a choice: the adapter route must still be withheld the turn'


def test_a_declared_caps_value_still_wins_over_the_default(launch):
    declared = 'claude-*=-per_turn_effort'
    launch.profile_settings({'CLAUDE_CODE_MODEL_CAPABILITIES': declared})
    row = launch.run()
    assert row['caps'] is None, 'a declared mask must not be replaced by the ccproteus default'
    assert row['settings_env']['CLAUDE_CODE_MODEL_CAPABILITIES'] == declared, \
        'the layer value must reach the engine untouched'


def test_one_blanked_key_does_not_withhold_the_other_defaults(launch):
    """Each key is decided on its own value, not on the batch.

    A layer that blanks the ability mask and declares the advisor off leaves the
    two keys in opposite states, and the one interpreter that reads them together
    must still answer differently for each.
    """
    launch.profile_settings({'CLAUDE_CODE_MODEL_CAPABILITIES': '',
                             'CLAUDE_CODE_DISABLE_ADVISOR_TOOL': '0'})
    row = launch.run()
    assert row['caps'] == ROUTE_DEFAULT
    assert row['effort'] == '1', 'a default keyed by a different name must still be filled'
    assert row['advisor'] is None, 'a declared advisor value must not be replaced'
    assert row['settings_env']['CLAUDE_CODE_DISABLE_ADVISOR_TOOL'] == '0'


def test_a_profile_tombstone_removes_a_canon_key_so_the_default_applies(launch):
    """A key the profile deleted (settings-compose's $ccproteusUnset) is not chosen."""
    launch.canonical_settings({'CLAUDE_CODE_DISABLE_ADVISOR_TOOL': '0'})
    layer = launch.state / 'profiles/gw/settings.profile.json'
    layer.parent.mkdir(parents=True, exist_ok=True)
    layer.write_text(json.dumps({'$ccproteusUnset': [['env', 'CLAUDE_CODE_DISABLE_ADVISOR_TOOL']]}))
    row = launch.run()
    assert row['advisor'] == '1', 'a tombstoned canon value must not count as a choice'


def test_a_whole_env_tombstone_removes_every_canon_key(launch):
    """["env"] deletes the whole block in settings-compose build, canon keys included."""
    launch.canonical_settings({'CLAUDE_CODE_DISABLE_ADVISOR_TOOL': '0'})
    layer = launch.state / 'profiles/gw/settings.profile.json'
    layer.parent.mkdir(parents=True, exist_ok=True)
    layer.write_text(json.dumps({'$ccproteusUnset': [['env']]}))
    row = launch.run()
    assert row['advisor'] == '1', 'a whole-env tombstone must not leave canon keys chosen'


def test_a_tombstoned_canon_compaction_percent_gets_the_default(launch):
    """The percentage reader honours $ccproteusUnset like every other reader."""
    launch.canonical_settings({'CLAUDE_AUTOCOMPACT_PCT_OVERRIDE': '70'})
    layer = launch.state / 'profiles/gw/settings.profile.json'
    layer.parent.mkdir(parents=True, exist_ok=True)
    layer.write_text(json.dumps({'$ccproteusUnset': [['env', 'CLAUDE_AUTOCOMPACT_PCT_OVERRIDE']]}))
    row = launch.run()
    assert row['pct'] == '75'


def test_a_malformed_tombstone_is_ignored_so_the_canon_percent_is_kept(launch):
    """settings-compose tombstones() drops a path with a non-string key."""
    launch.canonical_settings({'CLAUDE_AUTOCOMPACT_PCT_OVERRIDE': '70'})
    layer = launch.state / 'profiles/gw/settings.profile.json'
    layer.parent.mkdir(parents=True, exist_ok=True)
    layer.write_text(json.dumps({'$ccproteusUnset': [['env', ['CLAUDE_AUTOCOMPACT_PCT_OVERRIDE']]]}))
    row = launch.run()
    assert row['pct'] is None and row['settings_env']['CLAUDE_AUTOCOMPACT_PCT_OVERRIDE'] == '70'


def test_a_malformed_tombstone_does_not_crash_the_advisor_reader(launch):
    """An unhashable tombstone key crashed the env reader, so the default overrode canon's 0."""
    launch.canonical_settings({'CLAUDE_CODE_DISABLE_ADVISOR_TOOL': '0'})
    layer = launch.state / 'profiles/gw/settings.profile.json'
    layer.parent.mkdir(parents=True, exist_ok=True)
    layer.write_text(json.dumps({'$ccproteusUnset': [['env', ['X']]]}))
    row = launch.run()
    assert row['advisor'] is None and row['settings_env']['CLAUDE_CODE_DISABLE_ADVISOR_TOOL'] == '0'


def test_a_nonempty_caps_in_canon_survives_an_empty_profile_value(launch):
    """Last writer wins, and the empty one is not a choice either way.

    Canon names a mask and the profile layer blanks it: the merge makes the empty
    value effective, so the default -- not canon's -- is what the session reads,
    and the blanked key is kept out of the generated env block that would
    otherwise override the launch environment.
    """
    launch.canonical_settings({'CLAUDE_CODE_MODEL_CAPABILITIES': 'claude-*=-per_turn_effort'})
    launch.profile_settings({'CLAUDE_CODE_MODEL_CAPABILITIES': ''})
    row = launch.run()
    assert row['caps'] == ROUTE_DEFAULT
    assert 'CLAUDE_CODE_MODEL_CAPABILITIES' not in row['settings_env'], \
        'a key the launch sets itself must not remain in the generated env block'


@pytest.mark.parametrize('case, expected', [('gateway', PYTHON_STARTS_PER_LAUNCH),
                                            ('gateway-no-model', PYTHON_STARTS_NO_MODEL),
                                            ('token', PYTHON_STARTS_TOKEN),
                                            ('hazard', PYTHON_STARTS_HAZARD)])
def test_a_routed_launch_starts_python_a_fixed_number_of_times(launch, case, expected):
    """Count python3 starts, not seconds: the launch's cost is interpreter starts.

    The wrapper goes in front of PATH, which `command python3` obeys, so every
    interpreter the launch starts is one line. The first launch provisions the
    profile and is not counted; the stub's own interpreter is pointed at the real
    python3 so the harness does not count itself.
    """
    _gh433_arrange(launch, case)
    real = shutil.which('python3', path=launch.env['PATH'])
    wrapper = launch.home / 'bin' / 'python3'
    wrapper.write_text(f'#!/bin/sh\necho start >> "$HOME/python3.starts"\nexec {real} "$@"\n')
    wrapper.chmod(0o755)
    claude = launch.home / 'bin' / 'claude'
    claude.write_text(CLAUDE.replace('python3 -', f'{real} -', 1))
    claude.chmod(0o755)

    launch.run()                                   # first launch provisions
    started = launch.home / 'python3.starts'
    (launch.home / 'launches.jsonl').unlink()
    started.unlink()
    launch.run()

    count = len(started.read_text().splitlines())
    assert count == expected, f'{count} python3 starts'


def _gh433_arrange(launch, case):
    """The five shapes a launch's writes are pinned across."""
    if case == 'token':
        (launch.state / 'gw.conf').write_text(TOKEN_CONF)
    elif case == 'gateway-no-model':
        (launch.state / 'gw.conf').write_text(GW_CONF_NO_MODEL)
    elif case in ('hazard', 'hazard-withheld'):
        (launch.state / 'gw.conf').write_text(HAZARD_CONF)
        # The route's offer, in the canon layer: without it the hazard has no
        # siblings to weigh, and with it the launch's own ceiling climbs to the
        # largest wall among them (400k), which is what the advisory measures
        # the trigger against.
        (launch.home / '.claude/settings.json').write_text(json.dumps({'availableModels': HAZARD_MODELS}))
        if case == 'hazard-withheld':
            launch.env['CCPROTEUS_WITHHOLD_FATAL'] = '1'
    else:
        assert case == 'gateway'


def _gh433_record(launch, row):
    """What one launch left behind: the files it wrote, and how it called claude.

    The environment and the stderr carry the launcher's own absolute paths, which
    differ per run, so the tmp HOME is folded to `$HOME` -- the golden then pins
    what the launch decided, not where this copy of the tree lives. The
    environment is the file the stub's own shell wrote, not `os.environ` from a
    recording interpreter: see the stub's comment.
    """
    home = str(launch.home)

    def scrub(text):
        return text.replace(home, '$HOME')

    env = {}
    for line in (launch.home / 'launch_env.txt').read_text().splitlines():
        name, _, value = line.partition('=')
        env[name] = value
    profile_dir = launch.state / 'profiles/gw'
    files = {}
    for name, path in (('settings.json', profile_dir / 'settings.json'),
                       ('cache/settings.base.json', profile_dir / 'cache/settings.base.json'),
                       ('gw.conf', launch.state / 'gw.conf')):
        files[name] = scrub(path.read_text()) if path.exists() else None
    return {'argv': row['argv'],
            'env': {k: ('<minted per launch>' if k in GH433_MINTED else scrub(v))
                    for k, v in sorted(env.items()) if k not in GH433_VOLATILE},
            'files': files,
            'stderr': scrub(launch.last.stderr)}


@pytest.mark.parametrize('case', ['token', 'gateway', 'gateway-no-model', 'hazard', 'hazard-withheld'])
def test_a_launch_writes_what_it_wrote_before_gh433(launch, case):
    """Everything one launch writes, pinned byte for byte while its starts are cut.

    GH-433 takes interpreters out of the launch path. The only safe way to do that
    is to show the launch still does what it did: the profile's settings.json and
    its composed base, the conf, the argv and the environment claude is handed,
    and the launcher's own stderr. Five shapes, because the launch walks different
    paths through the store, the projection and the hazard advisory: a token
    profile, a gateway naming a model, the same gateway naming none, and a gateway
    whose siblings compact below the launch's trigger -- once keeping them and
    once withholding them from /model.

    The golden is written once, on a known-good tree, with GH433_REGEN=1; without
    it the test only compares, so the change under test cannot rewrite what it is
    measured against. The recorded launch is the SECOND one, after the first has
    provisioned the profile: a launch reads back what the last one saved, and a
    first launch's own bookkeeping is not what a steady state looks like.
    """
    _gh433_arrange(launch, case)
    launch.run()                                   # the first launch provisions
    (launch.home / 'launches.jsonl').unlink()
    got = _gh433_record(launch, launch.run())

    # A golden that recorded "no advisory" as "no advisory" would pass for the
    # wrong reason, so the two cases that must carry one say what it has to say.
    advise = got['env'].get('CCPROTEUS_ADVISE', '')
    if case == 'hazard':
        assert 'compacts at 135k' in advise and 'lower model limits: kimi-k3 limit 40k' in advise, advise
    elif case == 'hazard-withheld':
        assert 'hidden from /model: kimi-k3 limit 40k' in advise, advise
        assert 'kimi-k3' not in got['files']['settings.json'], 'the withhold edits the generated settings'
    else:
        assert 'CCPROTEUS_ADVISE' not in got['env'], advise

    if os.environ.get(GH433_REGEN):
        golden = json.loads(GH433_SNAPSHOT.read_text()) if GH433_SNAPSHOT.exists() else {}
        golden[case] = got
        GH433_SNAPSHOT.write_text(json.dumps(golden, indent=2, sort_keys=True) + '\n')
    assert GH433_SNAPSHOT.exists(), f'generate the golden first: {GH433_REGEN}=1 python -m pytest -k gh433'
    assert got == json.loads(GH433_SNAPSHOT.read_text())[case]


def test_a_killed_launch_leaves_no_adapter_stub_behind(launch):
    """A hard-killed launcher must not leave an immortal adapter stub.

    The launcher's EXIT trap is the only thing that stops the stub, and a
    SIGKILL skips it (issues #385/#386); like the real adapter, the stub must
    die with the --parent-pid it was handed.
    """
    # Hold the launch open so the stub is up when the launcher is killed.
    (launch.home / 'bin' / 'claude').write_text('#!/bin/sh\nsleep 5\n')
    (launch.home / 'bin' / 'claude').chmod(0o755)
    proc = subprocess.Popen([str(ROOT / "bin/ccproteus"), 'run', 'gw'], env=launch.env,
                            stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    stub_pid = 0
    try:
        stub = launch.state / 'libexec/ccproteus-adapter'
        deadline = time.monotonic() + HANG_TIMEOUT
        pids = []
        while time.monotonic() < deadline:
            pids = subprocess.run(['pgrep', '-f', f'{stub} gw '],
                                  capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout.split()
            if pids:
                break
            time.sleep(0.1)
        assert pids, 'the launch never started the adapter stub'
        stub_pid = int(pids[0])
        parent = int(subprocess.run(['ps', '-o', 'ppid=', '-p', str(stub_pid)],
                                    capture_output=True, text=True, timeout=HANG_TIMEOUT).stdout)
        # `ccproteus` sources its library and launches in its own process, so
        # the stub's parent is the launch itself.
        assert parent == proc.pid, 'the stub must watch the launch process'
        proc.kill()  # SIGKILL skips the EXIT trap that normally stops the stub
        proc.wait(timeout=HANG_TIMEOUT)
        deadline = time.monotonic() + HANG_TIMEOUT
        while time.monotonic() < deadline:
            try:
                os.kill(stub_pid, 0)
            except ProcessLookupError:
                return
            time.sleep(0.2)
        raise AssertionError('the adapter stub outlived its killed launcher')
    finally:
        # A red run must not leak the very stub this test guards against.
        proc.kill()
        proc.wait(timeout=HANG_TIMEOUT)
        if stub_pid:
            try:
                os.kill(stub_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
