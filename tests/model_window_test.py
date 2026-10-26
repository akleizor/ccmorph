"""The profile-owned resolver's public subprocess contract."""
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import signal
import tempfile
import unittest

from support import HANG_TIMEOUT

SCRIPT = Path(__file__).resolve().parents[1] / "libexec/model-window"
spec = importlib.util.spec_from_loader("window_test_store", importlib.machinery.SourceFileLoader("window_test_store", str(SCRIPT)))
assert spec and spec.loader
store = importlib.util.module_from_spec(spec)
spec.loader.exec_module(store)


def run(profile, model, windows=None, manual=None, env=None, fields=False, model_map=None, base_url=None):
    """One resolver run. `fields` asks for the flat record the launch path reads,
    which parses it in one line instead of a second interpreter for the JSON.
    `model_map` is written before the fingerprint, as the conf writer does; it is
    not part of the route, so the order only keeps the doc shaped like a real one.
    `base_url` overrides the default routed gateway when a case needs a native one."""
    with tempfile.TemporaryDirectory() as tmp:
        fields_doc = {"type": "apikey", "api_key": "canary", "base_url": "https://gateway.invalid"}
        if base_url is not None:
            fields_doc["base_url"] = base_url
        fields_doc.update({f"context_window.{key}": str(value) for key, value in (manual or {}).items()})
        if model_map is not None:
            fields_doc["model_map"] = json.dumps(model_map)
        fields_doc["context_discovered"] = json.dumps({"route": store.route_fingerprint(fields_doc), "models": windows or {}})
        Path(tmp, f"{profile}.conf").write_text("".join(f"{key}={value}\n" for key, value in fields_doc.items()))
        e = {key: value for key, value in os.environ.items()
             if key not in ("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "MODEL_WINDOW_REGISTRY", "MODEL_WINDOW_PROFILE_CONF", "MODEL_WINDOW_ENDPOINT_JSON")}
        e.update(CCPROTEUS_STATE_DIR=tmp)
        e.update(env or {})
        argv = ["python3", str(SCRIPT)] + (["--fields"] if fields else []) + [profile, model]
        result = subprocess.run(argv, env=e, capture_output=True, text=True, timeout=HANG_TIMEOUT)
        if fields:
            return result.returncode, result.stdout.rstrip("\n").split("\t"), result.stderr
        return result.returncode, json.loads(result.stdout), result.stderr


class Resolver(unittest.TestCase):
    def test_locked_interrupt_cancels_without_traceback_and_releases_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = {**os.environ, 'CCPROTEUS_STATE_DIR': tmp}
            # Only signal the parent lock runner created by this test.
            result = subprocess.run([
                'python3', str(SCRIPT), '--locked', 'demo', 'python3', '-c',
                'import os, signal; os.kill(os.getppid(), signal.SIGINT)',
            ], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT,
                # A backgrounded runner ignores SIGINT; start the child with the default.
                preexec_fn=lambda: signal.signal(signal.SIGINT, signal.SIG_DFL))
            self.assertEqual(result.returncode, 130, result.stderr)
            self.assertIn('Cancelled', result.stderr)
            self.assertNotIn('Traceback', result.stderr)
            result = subprocess.run([
                'python3', str(SCRIPT), '--locked', 'demo', 'python3', '-c', 'pass',
            ], env=env, capture_output=True, text=True, timeout=HANG_TIMEOUT)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_claude_at_1m_gets_suffix_but_haiku_below_does_not(self):
        for model, value, expected in (("claude-opus-5", 1000000, "claude-opus-5[1m]"),
                                       ("claude-haiku-4-5", 200000, "claude-haiku-4-5"),
                                       ("gpt-5.6-sol", 922000, "gpt-5.6-sol")):
            with self.subTest(model=model):
                rc, result, _ = run("gw", model, {model: value})
                self.assertEqual(rc, 0)
                self.assertEqual(result["window"], value)
                self.assertEqual(result["send_id"], expected)
                # The flat record the launch path reads must carry the same id: it
                # is what replaces the launch's own JSON parse of this answer.
                self.assertEqual(run("gw", model, {model: value}, fields=True)[1][3], expected)

    def test_manual_pin_beats_discovery_and_uses_common_suffix_finalization(self):
        rc, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 200000}, {"claude-opus-5": 1000000})
        self.assertEqual(result["source"], "conf-manual")
        self.assertEqual(result["send_id"], "claude-opus-5[1m]")

    def test_legacy_launch_pin_is_still_authoritative(self):
        _, result, _ = run("gw", "m", {"m": 300000}, {"m": 400000}, {"CLAUDE_CODE_MAX_CONTEXT_TOKENS": "150000"})
        self.assertEqual(result, {"window": 150000, "send_id": "m", "source": "conf"})

    def test_manual_keys_are_exact_and_colon_model_ids_survive(self):
        _, result, _ = run("gw", "qwen3-coder:64k", {"qwen3-coder:64k": 128000}, {"qwen3-coder": 300000})
        self.assertEqual(result["window"], 128000)
        _, result, _ = run("gw", "qwen3-coder:64k", {}, {"qwen3-coder:64k": 65536})
        self.assertEqual(result["window"], 65536)

    def test_equal_gateway_declarations_are_valid_when_no_stored_value(self):
        rows = [{"id": "a", "context_length": 200000}, {"id": "b", "context_length": 200000}]
        _, result, _ = run("gw", "b", env={"MODEL_WINDOW_ENDPOINT_JSON": json.dumps(rows)})
        self.assertEqual(result["source"], "endpoint")
        self.assertEqual(result["window"], 200000)

    def test_declared_field_spellings_and_invalid_values(self):
        for field in store.DECLARED_FIELDS:
            with self.subTest(field=field):
                _, result, _ = run("gw", "m", env={"MODEL_WINDOW_ENDPOINT_JSON": json.dumps([{"id": "m", field: 321000}])})
                self.assertEqual(result["window"], 321000)
        for value in (0, -1, True, "300000"):
            with self.subTest(value=value):
                _, result, _ = run("gw", "m", env={"MODEL_WINDOW_ENDPOINT_JSON": json.dumps([{"id": "m", "context_length": value}])})
                self.assertTrue(result["unprobed"])

    def test_stored_value_beats_supplied_metadata(self):
        _, result, _ = run("gw", "m", {"m": 983616}, env={"MODEL_WINDOW_ENDPOINT_JSON": '[{"id":"m","context_length":200000}]'})
        self.assertEqual(result["window"], 983616)
        self.assertEqual(result["source"], "conf-discovered")

    def test_bundled_family_alias_resolves_when_all_variants_share_a_window(self):
        _, result, _ = run("gw", "deepseek-flash")
        self.assertEqual(result["window"], 1000000)
        self.assertEqual(result["source"], "catalogue-bundled")
        self.assertNotIn("unprobed", result)

    def test_unknown_is_explicit_and_never_silent(self):
        _, result, err = run("gw", "claude-sonnet-9")
        self.assertEqual(result["window"], 200000)
        self.assertEqual(result["source"], "default")
        self.assertTrue(result["unprobed"])
        self.assertIn("no known context", err)

    def test_existing_suffix_is_not_doubled(self):
        _, result, _ = run("gw", "claude-opus-5[1m]", {"claude-opus-5": 1000000})
        self.assertEqual(result["send_id"], "claude-opus-5[1m]")


class MappedTarget(unittest.TestCase):
    """A profile whose `model_map` rewrites the wire id is sized for the model the
    shim actually serves, not for the id the session asked for.

    The gateway rewrite is final (`gateway-shim` `_resolve`): it replaces the body's
    model with the map's target before the request leaves, so the window that matters
    is the target's. Sizing the source instead hands a 1M-labelled alias a 1,000,000
    compaction budget behind a 128,000 wall, and the session dies past the wall
    without ever compacting. The client-facing identity is a separate fact and stays
    the requested one.
    """
    MAP = {"claude-opus-5": "small-target"}

    def test_a_1m_source_mapped_to_a_128k_target_budgets_the_target(self):
        _, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 1000000, "small-target": 128000},
                           model_map=self.MAP)
        self.assertEqual(result["window"], 128000)
        self.assertEqual(result["source"], "conf-discovered")
        self.assertEqual(result["send_id"], "claude-opus-5")
        row = run("gw", "claude-opus-5", {"claude-opus-5": 1000000, "small-target": 128000},
                  model_map=self.MAP, fields=True)[1]
        self.assertEqual(row[0], "128000")
        self.assertEqual(row[3], "claude-opus-5")

    def test_a_1m_suffixed_request_maps_like_its_bare_id(self):
        # The shim strips the marker before its one map lookup, so `src[1m]` and
        # `src` are the same request and must not size differently.
        _, result, _ = run("gw", "claude-opus-5[1m]", {"claude-opus-5": 1000000, "small-target": 128000},
                           model_map=self.MAP)
        self.assertEqual(result["window"], 128000)
        self.assertEqual(result["send_id"], "claude-opus-5")

    def test_the_targets_own_1m_suffix_is_stripped_before_sizing(self):
        # A map value may carry the marker; the shim strips it before serving, so
        # the lookup key must be the bare target or the window is missed entirely.
        _, result, _ = run("gw", "claude-opus-5", {"small-target": 128000},
                           model_map={"claude-opus-5": "small-target[1m]"})
        self.assertEqual(result["window"], 128000)

    def test_a_1m_target_keeps_the_1m_send_id(self):
        _, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 200000, "big-target": 1000000},
                           model_map={"claude-opus-5": "big-target"})
        self.assertEqual(result["window"], 1000000)
        self.assertEqual(result["send_id"], "claude-opus-5[1m]")

    def test_an_unmapped_model_resolves_as_before(self):
        _, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 1000000},
                           model_map={"other": "small-target"})
        self.assertEqual(result["window"], 1000000)
        self.assertEqual(result["send_id"], "claude-opus-5[1m]")

    def test_the_env_pin_still_wins(self):
        _, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 1000000, "small-target": 128000},
                           model_map=self.MAP, env={"CLAUDE_CODE_MAX_CONTEXT_TOKENS": "150000"})
        self.assertEqual(result, {"window": 150000, "send_id": "claude-opus-5", "source": "conf"})

    def test_a_manual_source_pin_beats_the_target(self):
        _, result, _ = run("gw", "claude-opus-5", {"small-target": 128000}, {"claude-opus-5": 300000},
                           model_map=self.MAP)
        self.assertEqual(result["window"], 300000)
        self.assertEqual(result["source"], "conf-manual")

    def test_a_manual_target_pin_beats_target_discovery(self):
        _, result, _ = run("gw", "claude-opus-5", {"small-target": 128000}, {"small-target": 150000},
                           model_map=self.MAP)
        self.assertEqual(result["window"], 150000)
        self.assertEqual(result["source"], "conf-manual")

    def test_an_unknown_target_falls_back_to_the_unknown_budget(self):
        # Never the source's 1M: the shim serves the target, so a window nothing
        # knows for the target is the target's unknown, not the source's known.
        _, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 1000000}, model_map=self.MAP)
        self.assertTrue(result["unprobed"])
        self.assertEqual(result["budget"], store.UNKNOWN_BUDGET)
        self.assertEqual(result["source"], "default")
        self.assertNotEqual(result["window"], 1000000)

    def test_a_native_route_ignores_the_map(self):
        # The shell drops model_map on a native route (ccproteus-models `_ccp_models_…`
        # gate): no shim runs there, so the map decides nothing and must not be
        # consulted here either.
        _, result, _ = run("gw", "claude-opus-5", {"claude-opus-5": 1000000, "small-target": 128000},
                           model_map=self.MAP, base_url="https://api.anthropic.com")
        self.assertEqual(result["window"], 1000000)
        self.assertEqual(result["send_id"], "claude-opus-5[1m]")


if __name__ == "__main__":
    unittest.main()


def test_model_choice_does_not_change_the_route():
    base = {"type": "apikey", "base_url": "https://gw.invalid", "api_key": "k"}
    mapped = {**base, "env.ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5[1m]",
              "env.ANTHROPIC_SMALL_FAST_MODEL": "gpt-6-luna", "env.ANTHROPIC_MODEL": "x"}
    assert store.route_fingerprint(mapped) == store.route_fingerprint(base)
    headers = {**base, "env.ANTHROPIC_CUSTOM_HEADERS": "x-route: eu"}
    assert store.route_fingerprint(headers) != store.route_fingerprint(base)


def test_every_shell_mapping_key_is_excluded_from_the_route():
    # ccproteus.sh owns the mapping list; the fingerprint must ignore all of it.
    import re
    root = Path(__file__).resolve().parents[1]
    shell = "".join(p.read_text() for p in [root / "lib/ccproteus.sh", *sorted((root / "lib").glob("ccproteus-*.zsh"))])
    block = re.search(r"_CCPROTEUS_MODEL_MAPPING_ENV=\(\n(.*?)\n\)", shell, re.S).group(1)
    keys = [k.strip() for k in block.splitlines() if k.strip().startswith("ANTHROPIC_")]
    assert keys
    for key in keys:
        assert store.MODEL_CHOICE_ENV.fullmatch("env." + key), key


def test_real_id_and_exclusion_share_one_rule():
    assert store.real_id("claude-fable-5-dd-los-6-tpg") == "gpt-6-sol"
    assert store.real_id("claude-opus-5") == "claude-opus-5"
    fields = {"excluded_models": "gpt-6-sol,claude-opus-5", "env.ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5[1m]"}
    assert store.is_excluded(fields, "claude-fable-5-dd-los-6-tpg")   # disguised id
    assert store.is_excluded(fields, "opus[1m]")                      # through its tier alias
    assert not store.is_excluded(fields, "gpt-6-luna")
    assert not store.is_excluded(fields, None)


class ConfLayout(unittest.TestCase):
    def test_canonical_conf_groups_keys_keeps_comments_and_values(self):
        messy = ("context_discovered={}\n# why the shim flag\nenv.CCPROTEUS_REPAIR_TOOL_REFERENCES=1\n"
                 "model=m\napi_key=k\nmodel_defaults=1\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=o\n"
                 "type=apikey\ncontext_window.m=1000\neffort=high\n\nbase_url=https://g.invalid\n")
        tidy = store.canonical_conf(messy)
        self.assertEqual(tidy, "type=apikey\nbase_url=https://g.invalid\napi_key=k\n\n"
                               "model=m\neffort=high\nenv.ANTHROPIC_DEFAULT_OPUS_MODEL=o\n\n"
                               "# why the shim flag\nenv.CCPROTEUS_REPAIR_TOOL_REFERENCES=1\n\n"
                               "context_window.m=1000\n\nmodel_defaults=1\ncontext_discovered={}\n")
        self.assertEqual(store.canonical_conf(tidy), tidy)

    def test_a_removed_role_key_is_not_laid_out_with_the_model_keys(self):
        """#375: the role key is gone, so it ranks after every known section."""
        stale = "ro" "le"
        tidy = store.canonical_conf(f"model=m\n{stale}=r\neffort=high\n")
        self.assertEqual(tidy, f"model=m\neffort=high\n\n{stale}=r\n")

    def test_a_context_write_lands_the_file_in_canonical_layout(self):
        with tempfile.TemporaryDirectory() as tmp:
            conf = Path(tmp, "p.conf")
            conf.write_text("model=m\ntype=apikey\napi_key=k\n")
            self.assertTrue(store.replace_fields(conf, {"context_window.m": "5000"}))
            self.assertEqual(conf.read_text(), "type=apikey\napi_key=k\n\nmodel=m\n\ncontext_window.m=5000\n")
