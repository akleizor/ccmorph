"""Profile-home migration moves inodes, preserves legacy access, refuses collisions."""
import json
import os
from pathlib import Path
import subprocess

from support import HANG_TIMEOUT

ROOT = Path(__file__).resolve().parents[1]


def run(home, state, script='_ccm_secure_dirs', extra=None):
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(('CCMORPH_', '_CCMORPH_', 'CLAUDE_', 'ANTHROPIC_'))}
    env.update(HOME=str(home), ZDOTDIR=str(home), CCMORPH_STATE_DIR=str(state),
               CCMORPH_LIB_DIR=str(ROOT / "lib"))
    env.update(extra or {})
    return subprocess.run(['zsh', '-fc', 'source "$1"; ' + script, '_',
                           str(ROOT / 'lib/ccmorph.sh')], env=env, text=True,
                          capture_output=True, timeout=HANG_TIMEOUT)


def test_migration_preserves_inodes_symlinks_and_old_session_access(tmp_path):
    home = tmp_path
    state = home / '.config/ccmorph'
    old = home / '.claude-profiles'
    profile = old / 'demo'
    profile.mkdir(parents=True, mode=0o700)
    fixture = profile / 'config.json'
    fixture.write_text('{"fixture":"synthetic-auth-state"}')
    fixture.chmod(0o600)
    inode = fixture.stat().st_ino
    shared = home / '.claude/projects'
    shared.mkdir(parents=True)
    (profile / 'projects').symlink_to(shared, target_is_directory=True)
    result = run(home, state)
    assert result.returncode == 0, result.stderr
    target = state / 'profiles'
    assert old.is_symlink() and old.resolve() == target
    assert (target / 'demo/config.json').stat().st_ino == inode
    assert (target / 'demo/config.json').stat().st_mode & 0o777 == 0o600
    assert Path(os.readlink(target / 'demo/projects')) == shared
    # Simulate an already-running session retaining the old absolute pathname.
    (old / 'demo/session-state').write_text('live')
    assert (target / 'demo/session-state').read_text() == 'live'
    before = target.stat().st_mtime_ns
    assert run(home, state).returncode == 0
    assert target.stat().st_mtime_ns == before


def test_provisioning_leaves_native_claude_dir_mode_alone(tmp_path):
    # ~/.claude is Claude Code's. Provisioning links the shared layer into the
    # profile and creates the shared dirs, but the directory's own mode is not
    # ccmorph's to change (it used to be forced to 700).
    home = tmp_path
    state = home / '.config/ccmorph'
    native = home / '.claude'
    native.mkdir()
    os.chmod(native, 0o755)
    result = run(home, state,
                 script='typeset -gA _ccm=(type apikey); _ccm_secure_dirs && _ccm_setup_dir_unlocked demo')
    assert result.returncode == 0, result.stderr
    assert native.stat().st_mode & 0o777 == 0o755
    assert (state / 'profiles/demo/skills').is_symlink()
    assert (state / 'profiles/demo').stat().st_mode & 0o777 == 0o700


def plugin_indexes(home, state):
    plugins = home / '.claude/plugins'
    market = plugins / 'marketplaces/example'
    cache = plugins / 'cache/example/tool/1.2.3'
    market.mkdir(parents=True)
    cache.mkdir(parents=True)
    known = {
        'example': {
            'source': {'source': 'git', 'url': 'git@example.invalid:owner/repo.git'},
            'installLocation': str(home / '.claude-profiles/demo/plugins/marketplaces/example'),
            'unknown': {'keep': True},
        },
        'external': {
            'source': {'source': 'directory', 'path': '/opt/external'},
            'installLocation': '/opt/external',
        },
        'missing': {
            'source': {'source': 'github', 'repo': 'owner/missing'},
            'installLocation': str(state / 'profiles/demo/plugins/marketplaces/missing'),
        },
    }
    installed = {
        'version': 2,
        'plugins': {
            'tool@example': [{
                'scope': 'project',
                'projectPath': '/work/project',
                'installPath': str(state / 'profiles/demo/plugins/cache/example/tool/1.2.3'),
                'version': '1.2.3',
                'unknown': 'keep',
            }],
            'missing@example': [{
                'scope': 'user',
                'installPath': str(home / '.claude-profiles/demo/plugins/cache/example/missing/9'),
                'version': '9',
            }],
        },
    }
    (plugins / 'known_marketplaces.json').write_text(json.dumps(known, indent=2))
    (plugins / 'installed_plugins.json').write_text(json.dumps(installed, indent=2))
    return plugins, known, installed


def test_plugin_index_paths_follow_shared_plugin_topology(tmp_path):
    state = tmp_path / '.config/ccmorph'
    plugins, known_before, installed_before = plugin_indexes(tmp_path, state)

    result = run(tmp_path, state)
    assert result.returncode == 0, result.stderr

    known = json.loads((plugins / 'known_marketplaces.json').read_text())
    installed = json.loads((plugins / 'installed_plugins.json').read_text())
    assert known['example']['installLocation'] == str(plugins / 'marketplaces/example')
    assert installed['plugins']['tool@example'][0]['installPath'] == str(
        plugins / 'cache/example/tool/1.2.3')
    assert known['example']['source'] == known_before['example']['source']
    assert known['example']['unknown'] == {'keep': True}
    assert known['external'] == known_before['external']
    assert known['missing'] == known_before['missing']
    assert installed['plugins']['tool@example'][0]['scope'] == 'project'
    assert installed['plugins']['tool@example'][0]['projectPath'] == '/work/project'
    assert installed['plugins']['tool@example'][0]['unknown'] == 'keep'
    assert installed['plugins']['missing@example'] == installed_before['plugins']['missing@example']

    paths = [plugins / 'known_marketplaces.json', plugins / 'installed_plugins.json']
    snapshot = [(p.read_bytes(), p.stat().st_mtime_ns) for p in paths]
    assert run(tmp_path, state).returncode == 0
    assert [(p.read_bytes(), p.stat().st_mtime_ns) for p in paths] == snapshot


def test_malformed_plugin_index_does_not_block_profile_setup(tmp_path):
    state = tmp_path / '.config/ccmorph'
    plugins = tmp_path / '.claude/plugins'
    plugins.mkdir(parents=True)
    malformed = plugins / 'known_marketplaces.json'
    malformed.write_text('{not json')

    result = run(tmp_path, state)
    assert result.returncode == 0, result.stderr
    assert malformed.read_text() == '{not json'
    assert (state / 'profiles').is_dir()


def test_doctor_does_not_normalize_plugin_indexes(tmp_path):
    state = tmp_path / '.config/ccmorph'
    state.mkdir(parents=True)
    (state / 'demo.conf').write_text('type=token\ntoken=synthetic\n')
    plugins, _, _ = plugin_indexes(tmp_path, state)
    before = (plugins / 'known_marketplaces.json').read_bytes()

    run(tmp_path, state, 'ccmorph doctor demo')
    assert (plugins / 'known_marketplaces.json').read_bytes() == before


def test_fresh_install_uses_state_profiles_without_legacy_alias(tmp_path):
    state = tmp_path / 'custom-state'
    result = run(tmp_path, state, '_ccm_secure_dirs; print -r -- "$_CCMORPH_PROFILE_DIRS"')
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == str(state / 'profiles')
    assert (state / 'profiles').is_dir()
    assert not (tmp_path / '.claude-profiles').exists()


def test_doctor_observes_legacy_home_without_migrating(tmp_path):
    state = tmp_path / '.config/ccmorph'
    state.mkdir(parents=True)
    (state / 'demo.conf').write_text('type=token\ntoken=synthetic\n')
    old = tmp_path / '.claude-profiles'
    profile = old / 'demo'
    profile.mkdir(parents=True)
    (profile / 'settings.json').write_text('{}')
    result = run(tmp_path, state, 'ccmorph doctor demo')
    assert str(profile) in result.stdout
    assert old.is_dir() and not old.is_symlink()
    assert not (state / 'profiles').exists()
    assert not (state / '.profile-home-migration.lock').exists()


def test_collision_refuses_without_merging_or_overwriting(tmp_path):
    state = tmp_path / '.config/ccmorph'
    old = tmp_path / '.claude-profiles'
    target = state / 'profiles'
    old.mkdir()
    target.mkdir(parents=True)
    (old / 'old').write_text('old')
    (target / 'new').write_text('new')
    result = run(tmp_path, state)
    assert result.returncode != 0
    assert 'nothing moved' in result.stderr
    assert not old.is_symlink()
    assert (old / 'old').read_text() == 'old'
    assert (target / 'new').read_text() == 'new'


def test_concurrent_migration_converges_on_one_directory(tmp_path):
    from concurrent.futures import ThreadPoolExecutor
    state = tmp_path / '.config/ccmorph'
    old = tmp_path / '.claude-profiles'
    old.mkdir()
    (old / 'fixture').write_text('one')
    with ThreadPoolExecutor(max_workers=3) as pool:
        results = list(pool.map(lambda _: run(tmp_path, state), range(3)))
    assert all(r.returncode == 0 for r in results), [r.stderr for r in results]
    assert old.is_symlink() and (state / 'profiles/fixture').read_text() == 'one'


def test_unrelated_legacy_symlink_refused(tmp_path):
    state = tmp_path / '.config/ccmorph'
    other = tmp_path / 'unrelated'
    other.mkdir()
    old = tmp_path / '.claude-profiles'
    old.symlink_to(other, target_is_directory=True)
    result = run(tmp_path, state)
    assert result.returncode != 0
    assert Path(os.readlink(old)) == other
    assert not (state / 'profiles').exists()


# The two failures that need an injected syscall (a cross-device rename, a refused
# alias) are util/util_test.go's TestMigrate*.
