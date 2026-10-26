Status: v0 starting point (2026-10-16), approved by Alex; changes only via a PR he approves.
# INTENT.md — ccproteus

**Draft for Alex's review. Not adopted. Only Alex may change the adopted INTENT.md; agents may report a failed Check but may not weaken a promise or its Check.**

These are enduring promises to the user, not a feature list, roadmap, or implementation plan. Checks use existing evidence unless explicitly marked **Measurement gap**; their existence was verified, but they were not run for this drafting task. All fixtures use isolated homes; the live installation at `~/.config/ccproteus` is not a test target.

## Promises

1. Every session launches on the backend and profile the user selected, or stops with a clear error instead of routing somewhere else.

   **Check:** Run `zsh tests/ccproteus_test.zsh`, `zsh tests/cli_configure_test.zsh`, and the Python launch/profile tests with `CCPROTEUS_LIB_DIR` pointed at the checkout; require zero failures for explicit-profile launch, model precedence, adapter endpoints, and the existing `_ccs_launch -a <profile> <args>` caller contract. A fake backend must see exactly the selected endpoint and identity. These isolated cases establish routing behavior, not a live provider availability SLA.

2. Credentials stay private and are never leaked in ordinary diagnostics, logs, command arguments, or another profile's session.

   **Check:** Run `tests/launch_env_rules_test.py` and `zsh tests/ccproteus_test.zsh`; require zero failures, zero other-backend credentials in captured launch environments, and zero fixture secret values in ordinary diagnostics/logs/process arguments. In the isolated test home, require credential/config files mode `0600` and private profile directories `0700`, as documented in `SECURITY.md`; do not open the real credential store. Explicit secret-export commands are intentional exports, not ordinary diagnostics. Permissions do not protect against processes running as the same user.

3. Each profile keeps its own identity, backend configuration, and declared privacy behavior without changing another profile's state.

   **Check:** Run `tests/profile_directory_test.py`, `tests/launch_env_rules_test.py`, and `tests/gateway_shim_test.py`; require zero failures for private profile state, nested credential isolation, and configured identifier behavior. The shim's opt-in random-device-id tests must show stability within a session and distinct IDs across sessions; flags-off tests must preserve the declared pass-through behavior. **Measurement gap:** no verified whole-launch, two-profile egress audit covers all routes and metadata, including direct routes. This is isolation of declared settings, not automatic anonymisation; skills are deliberately shared; session history is per profile, as in a plain Claude Code config dir, and a hop or a resume by id links in only the one session it continues (Alex, 2026-10-22).

4. The selected model and effort are never silently changed during launch, resume, or a profile switch.

   **Check:** In isolated launch/resume/hop cases, compare the requested model and effort with captured launch arguments and the fake upstream request; require exact agreement, or an explicit visible rejection/change notice before any replacement is used. Existing model-precedence and carried-model cases are in `tests/ccproteus_test.zsh`; `tests/cli_config_store_test.py` covers saved defaults. **Measurement gap:** no verified comprehensive notice-before-substitution check covers every launch/resume/hop path. **Current conflict:** `test_hop_falls_back_when_the_target_cannot_serve_the_model` explicitly drops an unsupported carried model, and `test_invalid_resume_model_falls_back_to_profile_model` expects fallback; their existence does not prove Alex's no-silent-change promise is met. Do not weaken this promise to match those tests.

5. Switching or resuming a session preserves its conversation and chosen operating settings instead of losing the user's work.

   **Check:** Run `zsh tests/ccproteus_test.zsh`, `tests/profile_context_lifecycle_test.py`, and `tests/profile_context_store_test.py`; require zero failures for direct/relay resume, session identity, history continuity, and preservation of effort and permission mode. Require no mutation of another session's profile state, and keep model-substitution exceptions visible under promise 4 rather than disguising them as successful continuity.

6. Installing, updating, or removing ccproteus never overwrites the user's profiles, credentials, customisations, or the native Claude command.

   **Check:** Run `zsh tests/install_test.zsh` in isolated homes and `tests/profile_directory_test.py`; require zero failures, preserved fixture profile/credential/local-override contents, usable old history after migration, and no replacement of native `claude` or edits to shell startup files. Uninstall must leave profiles intact. Never run the installer against Alex's real home for this Check.

7. When a route cannot work, the user receives a truthful diagnosis without hidden paid probing or pretending a session is healthy.

   **Check:** Run `zsh tests/cli_configure_test.zsh` and the error/diagnostic launch cases in `zsh tests/ccproteus_test.zsh`; require zero failures, named refusals for missing credentials or invalid configuration, and zero billable inference requests during ordinary inspection/model-metadata discovery in recorded fake-upstream fixtures. **Measurement gap:** no verified continuous session-routing success rate or provider-failure telemetry was found; fixture diagnostics do not prove every production outage is promptly identified.

## Sources and limits

Read from `/Users/alex/code/ccproteus`: `AGENTS.md` (Verification, Hive focus and external caller contract), `README.md`, `CLAUDE.md` (routes to AGENTS), `SECURITY.md`, and the cited measurement/test sources. Source revision: `9314b8d` on 2026-10-16. No `/Users/alex/.claude/memory/ccproteus/MEMORY.md` exists.

Identity, backend settings and session history are private per profile; shared tools are an existing user contract, not a defect to “fix” while drafting. No request bodies, credentials, live configuration, or real session logs are included in these drafts or their preview.
