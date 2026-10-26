"""One profile configuration owns manual and discovered model context."""
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1] / "libexec/model-window"
spec = importlib.util.spec_from_loader("context_store", importlib.machinery.SourceFileLoader("context_store", str(SCRIPT)))
assert spec and spec.loader
store = importlib.util.module_from_spec(spec)
spec.loader.exec_module(store)


class ProfileContextStore(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.state = self.root / "state"
        self.state.mkdir()
        self.env = patch.dict(os.environ, {"HOME": str(self.root), "CCMORPH_STATE_DIR": str(self.state)}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.a = self.state / "a.conf"
        self.b = self.state / "b.conf"
        self.a.write_text("# keep\ntype=apikey\nbase_url=https://a.invalid\napi_key=canary-a\n")
        self.a.chmod(0o600)
        self.b.write_text("type=apikey\nbase_url=https://b.invalid\napi_key=canary-b\n")

    def test_discovery_and_manual_values_share_conf_and_stay_profile_scoped(self):
        identity = store.profile_identity("a")
        self.assertTrue(store.save_discovered("a", {"m": 123000}, identity))
        self.assertEqual(store.resolve("a", "m")["window"], 123000)
        self.assertTrue(store.resolve("b", "m")["unprobed"])
        store.set_context("a", "m", "654321")
        self.assertEqual(store.resolve("a", "m")["window"], 654321)
        store.set_context("a", "m", "auto")
        self.assertEqual(store.resolve("a", "m")["window"], 123000)
        text = self.a.read_text()
        self.assertIn("# keep\n", text)
        self.assertIn("api_key=canary-a\n", text)
        self.assertEqual(self.a.stat().st_mode & 0o777, 0o600)
        self.assertFalse((self.state / "model-registry").exists())

    def test_unicode_line_separators_in_unrelated_values_are_preserved(self):
        original = "type=apikey\napi_key=before\x85context_window.m=keep\u2028after\nenv.ANTHROPIC_AUTH_TOKEN=first\x85second\n"
        self.a.write_text(original)
        _, fields = store.read_conf(self.a)
        self.assertEqual(fields["api_key"], "before\x85context_window.m=keep\u2028after")
        self.assertEqual(fields["env.ANTHROPIC_AUTH_TOKEN"], "first\x85second")
        store.set_context("a", "m", "300000")
        store.save_discovered("a", {"other": 400000}, store.profile_identity("a"))
        # Same values, canonical layout: a context write may regroup lines but
        # must never alter a value, cut one at a line break, or drop it.
        _, after = store.read_conf(self.a)
        self.assertEqual(after["api_key"], "before\x85context_window.m=keep\u2028after")
        self.assertEqual(after["env.ANTHROPIC_AUTH_TOKEN"], "first\x85second")
        self.assertEqual(after["context_window.m"], "300000")

    def test_route_change_invalidates_discovery_but_not_manual_pin(self):
        store.save_discovered("a", {"m": 123000}, store.profile_identity("a"))
        store.set_context("a", "manual", "300000")
        self.a.write_text(self.a.read_text().replace("https://a.invalid", "https://other.invalid"))
        self.assertTrue(store.resolve("a", "m")["unprobed"])
        self.assertEqual(store.resolve("a", "manual")["window"], 300000)

    def test_deleted_recreated_renamed_or_edited_profile_rejects_stale_commit(self):
        for mutation in ("delete", "recreate", "rename", "manual"):
            with self.subTest(mutation=mutation):
                self.a.write_text("type=apikey\napi_key=canary\n")
                identity = store.profile_identity("a")
                if mutation == "manual":
                    store.set_context("a", "m", "400000")
                elif mutation == "rename":
                    self.a.rename(self.state / "renamed.conf")
                else:
                    self.a.unlink()
                    if mutation == "recreate":
                        self.a.write_text("type=apikey\napi_key=canary\n")
                self.assertFalse(store.save_discovered("a", {"m": 900000}, identity))
                if self.a.exists():
                    self.assertNotIn("900000", self.a.read_text())

    def test_exact_manual_keys_preserve_credentials_and_neighbours(self):
        self.a.write_text("type=apikey\napi_key=context_window.a.b=keep\ncontext_window.axb=500000\n")
        store.set_context("a", "a.b", "131072")
        self.assertIn("api_key=context_window.a.b=keep", self.a.read_text())
        self.assertEqual(store.resolve("a", "axb")["window"], 500000)
        self.assertEqual(store.context_value("a", "a.b"), "131072")
        for model, value in (("bad\nkey", "100000"), ("a=b", "100000"), ("m", "0"), ("m", "-1")):
            with self.assertRaises(ValueError):
                store.set_context("a", model, value)

    def test_migration_keeps_scoped_data_and_only_attributed_bare_data(self):
        legacy = self.state / "model-registry"
        legacy.write_text("a:scoped=300000\na:wild*=350000\nb:scoped=400000\nbare=500000\nunknown=600000\n")
        cache = self.root / ".claude-profiles/a/cache/context-window"
        cache.parent.mkdir(parents=True)
        cache.write_text("bare\t500000\nlocal:tag\t65536\n")
        store.set_context("a", "scoped", "250000")
        self.assertEqual(store.migrate(), 0)
        self.assertEqual(store.resolve("a", "scoped")["window"], 250000)
        self.assertEqual(store.resolve("b", "scoped")["window"], 400000)
        self.assertEqual(store.resolve("a", "wild-id")["window"], 350000)
        self.assertEqual(store.resolve("a", "bare")["window"], 500000)
        self.assertTrue(store.resolve("b", "bare")["unprobed"])
        self.assertTrue(store.resolve("a", "unknown")["unprobed"])
        self.assertEqual(store.resolve("a", "local:tag")["window"], 65536)
        self.assertFalse(legacy.exists())
        self.assertFalse(cache.exists())
        self.assertTrue((self.state / "model-registry.migrated").exists())
        before = self.a.read_text()
        self.assertEqual(store.migrate(), 0)
        self.assertEqual(self.a.read_text(), before)

    def test_migration_preview_reports_counts_without_writing(self):
        legacy = self.state / "model-registry"
        legacy.write_text("a:m=321000\nunknown=500000\n")
        before = {path.name: path.read_bytes() for path in self.state.iterdir()}
        preview = store.migration_preview()
        self.assertEqual(preview["profiles"]["a"]["scoped"], 1)
        self.assertEqual(preview["unattributed_entries"], 1)
        self.assertEqual({path.name: path.read_bytes() for path in self.state.iterdir()}, before)
        self.assertNotIn("canary", json.dumps(preview))

    def test_concurrent_migrations_archive_once_and_preserve_values(self):
        import concurrent.futures
        legacy = self.state / "model-registry"
        legacy.write_text("a:m=321000\n")
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            self.assertEqual(list(pool.map(lambda _: store.migrate(), range(2))), [0, 0])
        self.assertEqual(store.resolve("a", "m")["window"], 321000)
        self.assertTrue(legacy.with_name("model-registry.migrated").exists())
        self.assertFalse(legacy.with_name("model-registry.migrated.1").exists())

    def test_symlink_cache_refuses_migration_without_archiving_registry(self):
        legacy = self.state / "model-registry"
        legacy.write_text("a:m=321000\n")
        cache = self.root / ".claude-profiles/a/cache/context-window"
        cache.parent.mkdir(parents=True)
        cache.symlink_to(self.b)
        with self.assertRaises(OSError):
            store.migrate()
        self.assertTrue(legacy.exists())
        self.assertNotIn("context_discovered", self.a.read_text())

    def test_resolver_never_falls_back_to_legacy_registry_or_profile_cache(self):
        (self.state / "model-registry").write_text("a:m=900000\nm=900000\n")
        self.assertTrue(store.resolve("a", "m")["unprobed"])

    def test_migration_never_archives_cache_without_owning_profile(self):
        cache = self.root / '.claude-profiles/unrelated/cache/context-window'
        cache.parent.mkdir(parents=True)
        cache.write_text('m\t300000\n')
        store.migrate()
        self.assertTrue(cache.exists())

    def test_explicit_recovery_imports_profile_backups_without_overwriting_current(self):
        cache = self.root / '.claude-profiles/a/cache/context-window.migrated'
        cache.parent.mkdir(parents=True)
        cache.write_text('m\t300000\nknown\t400000\n')
        store.save_discovered('a', {'known': 500000}, store.profile_identity('a'))
        store.recover_cache_backups()
        self.assertEqual(store.resolve('a', 'm')['window'], 300000)
        self.assertEqual(store.resolve('a', 'known')['window'], 500000)
        self.assertTrue(cache.exists())

    def test_cache_entry_rules_match_preview_migration_and_recovery(self):
        rows = ('tag:variant[1m]\t300000\ntag:variant\t400000\n'
                'bad model\t100000\nzero\t0\nnegative\t-1\n'
                'extra\t200000\textra\nmissing-tab\n')
        cache = self.root / '.claude-profiles/a/cache/context-window'
        cache.parent.mkdir(parents=True)
        cache.write_text(rows)
        self.assertEqual(store.migration_preview()['profiles']['a']['cached'], 2)
        store.migrate()
        migrated = store.profile_windows('a')
        self.assertEqual(migrated, {'tag:variant': 400000})
        backup = self.root / '.claude-profiles/b/cache/context-window.migrated'
        backup.parent.mkdir(parents=True)
        backup.write_text(rows)
        store.recover_cache_backups()
        self.assertEqual(store.profile_windows('b'), migrated)
        self.assertEqual(backup.read_text(), rows)

    def test_discovered_json_does_not_contain_credentials(self):
        store.save_discovered("a", {"m": 123000}, store.profile_identity("a"))
        line = next(line for line in self.a.read_text().splitlines() if line.startswith("context_discovered="))
        data = json.loads(line.partition("=")[2])
        self.assertEqual(data["models"], {"m": 123000})
        self.assertNotIn("canary-a", line)
        self.assertNotIn("https://a.invalid", line)
