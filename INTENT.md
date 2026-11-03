Status: adopted (2026-11-04); changes only via a reviewed PR.
# INTENT.md — ccmorph

These are enduring promises to the user, not a feature list, roadmap, or implementation plan. Only the maintainers may change
this file, through a reviewed PR; anyone may report a failed Check, but a promise or its Check is not weakened to make a change pass.

Run every Check from the repository root after `make shim`. The suites use isolated test homes. Never run a Check against your real `~/.config/ccmorph` or your real home.

## Promises

1. Any model from any provider can run inside any Claude Code profile.

   **Check:** Run `go test ./adapter/... ./shim/...`, `python3 -m pytest tests/adapter_test.py tests/gateway_shim_test.py -q` (with `CCMORPH_LIB_DIR="$PWD/lib"`), and `zsh tests/ccmorph_test.zsh`. Require zero failures. A fake non-Anthropic backend must receive the translated request for the selected profile and Claude Code must receive a valid reply. **Measurement gap:** no test runs a real provider, so a provider that changes its API after release is not caught here.

2. The model and effort you chose are never changed without telling you first.

   **Check:** Run `zsh tests/ccmorph_test.zsh` and `go test ./util/...`. Require zero failures, including `test_direct_resume_reports_a_rejected_carried_model`, `test_hop_relays_model_and_effort`, `test_hop_falls_back_when_the_target_cannot_serve_the_model` and `test_mapped_model_resume_and_hop_keep_servable_identity`. In each case the model seen by the fake upstream must equal the requested one, or the output must say that the model is not served and which default replaces it. **Measurement gap:** no test asserts the "not served" notice for a hop, only the resume path.

3. Two terminals can run two different profiles at the same time, and neither changes the other's login, settings or history.

   **Check:** Run `python3 -m pytest tests/profile_directory_test.py tests/launch_env_rules_test.py -q` and `zsh tests/ccmorph_test.zsh`. Require zero failures, including `test_launch_does_not_inherit_other_backends_credentials` and `test_resume_by_id_reaches_another_profiles_session`. **Measurement gap:** no test runs two live launches on two profiles at once and compares their state afterwards.

4. One command moves a running conversation to another profile and it continues there, with its history, model and effort.

   **Check:** Run `go test ./util/ -run Hop` and `CCMORPH_TEST_FILTER=hop zsh tests/ccmorph_test.zsh`. Require zero failures: the relay file for the target profile must name the same session, effort and (when the target serves it) model. **Measurement gap:** no test types `/ccmorph:hop` into a real Claude Code prompt; the native smoke files in `tests/native_*_smoke.py` cover only other behaviour.

5. Your API keys, tokens and passwords stay private: stored with owner-only permissions, never on a command line, never sent to another profile's backend, and masked from the provider when you turn masking on.

   **Check:** Run `python3 -m pytest tests/launch_env_rules_test.py tests/credential_argv_test.py tests/credential_log_mode_test.py tests/credential_url_redact_test.py -q`, `go test ./shim/ ./profile/`, and `python3 -m pytest tests/doctor_test.py -q`. Require zero failures: a canary key must not appear in `ps` output, in diagnostics or in another backend's environment, and credential files must be mode `0600`, as `.github/SECURITY.md` states. Masking is opt in (`CCMORPH_MASK`) and covers only routes the shim fronts. Permissions do not protect against other processes running as the same user.

6. Claude Code stays Claude Code: `claude` is never replaced, wrapped or renamed, native arguments pass through untouched, and installing, updating or removing ccmorph never overwrites your profiles, credentials or shell startup files.

   **Check:** Run `zsh tests/install_test.zsh`, `zsh tests/cli_configure_test.zsh`, and `python3 -m pytest tests/doctor_test.py tests/cli_installed_test.py tests/profile_directory_test.py -q`. Require zero failures, fixture profiles and credentials unchanged after install, update and uninstall, no `claude` shim on `PATH`, and no edit to shell startup files. The boundary is written down in `docs/ARCHITECTURE.md#scope-and-ownership`.

7. A session starts without a pile of helper processes: the core is one compiled binary and needs no Python except for the optional Remote Control add-on.

   **Check:** Run `python3 -m pytest tests/no_python_test.py tests/launch_latency_test.py -q` and `go test ./cli/...`. Require zero failures. **Measurement gap:** no test counts the processes one session spawns or sets a start-up time limit, so a slow launch passes these tests.

8. When a session cannot start or a route is broken, you get a plain, truthful reason and the commands to fix it, never a hang or a session that pretends to be healthy.

   **Check:** Run `python3 -m pytest tests/doctor_test.py tests/hang_bound_test.py -q`, `zsh tests/cli_configure_test.zsh` and `go test ./shim/`. Require zero failures: a missing credential or invalid endpoint must be named and must send no inference request, `ccmorph doctor` must exit with a clear status on a fresh install, and a rejected request is retried or reported (see `shim/resend_test.go`). **Measurement gap:** no telemetry shows how often real sessions fail to start.

## Sources and limits

Written from this repository: `README.md`, `AGENTS.md`, `docs/ARCHITECTURE.md`, `docs/guide.md`, `.github/SECURITY.md`, `.github/CONTRIBUTING.md`, the test suites named in each Check, and the earlier maintainer decisions on scope and direction (any model in any Claude Code profile, no silent model change, one binary, secrets kept private).

It leaves out features that serve one provider only, the planned model router, a graphical interface, other runtimes than Claude Code, and the look of the README and demo. Checks were verified to exist but were not all run when this file was written.
