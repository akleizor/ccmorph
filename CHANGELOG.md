# Changelog

> The window-recording verb became `probe` in v2.4.0. Entries below use that
> spelling throughout, including ones describing releases that shipped its
> predecessor — so a reader following an old entry types the command that exists
> today. The former spelling still dispatches, undocumented.

## Unreleased

### Changed

- **A computed cost is no longer billed spend** (#51). The shim writes the
  models.dev list price it computes for a provider that bills outside the
  response as `usage.estimated_cost`, never as `usage.cost`, and
  `scripts/usage-export` marks it `costSource: estimate` with its own
  `estimated_usd`/`estimated_rows` totals; `ccmorph-core shim report` prints it on
  its own "estimated spend" line. Gateway-billed totals stay billed only.
- **`ccmorph-core refresh-model-registry` is now `ccmorph-core context-probe`.**
  It finds each model's context window; the shared model registry it was named
  after was folded into each profile's `.conf` long ago. Users type
  `ccmorph probe`, which is unchanged.
- **Breaking: `libexec/gateway-shim` is removed; the shim is `ccmorph-core shim`.**
  One binary now ships per platform. A script that ran `libexec/gateway-shim ...`
  (including `report`, `outcome` and `translator-user-agent`) must run
  `libexec/ccmorph-core shim ...` with the same arguments. An upgrade deletes the
  old file. `CCMORPH_SHIM_HOOKS` is unchanged: it still names your own build.
- `model-window` and `context-probe` now share one Go package, `ctxwindow/`. Both
  subcommands behave exactly as before.

## v0.3.0 — 2026-10-28

The core is Go. `ccmorph-core` replaces the four Python helpers, and `python3` is
no longer needed unless you use the Remote Control add-on.

### Breaking

- **`python3` is no longer a requirement.** Launch, install, `doctor` and the zsh
  library run on zsh and the two Go binaries only. Remote Control
  (`addons/remote-control`) is still Python and still needs `python3` 3.8+.
- **The Python helpers are gone.** `libexec/ccmorph-adapter`, `model-window`,
  `settings-compose` and `refresh-model-registry` are now
  `ccmorph-core adapter|model-window|settings-compose|refresh-model-registry`.
  A script that ran the old paths must call `ccmorph-core <verb>`. `install.sh`
  removes the old copies of the last three on upgrade and keeps
  `ccmorph-adapter` for sessions that still run it (see Fixed).

### Removed

- **The model-router add-on** (#35) moved to its own repository, with the root
  plugin marketplace. Uninstall the plugin from Claude Code if you added it.

### Added

- **`CCMORPH_COMPAT=off`** (#43) turns off every built-in gateway compatibility
  repair in the shim.

### Changed

- **One helper process per native gateway session.** The shim runs inside the
  adapter (#40) instead of beside it, so a native launch holds one process where
  it held two (idle adapter 33 MB to 13.5 MB).
- **Go ports of all four helpers** (#37 adapter, #38 settings-compose,
  model-window, refresh-model-registry) with the same flags, output and exit
  codes. Known differences: OS error wording, unknown classification-cache keys
  are dropped, an empty `CCMORPH_MODEL_MAP=` is `{}` (the Python crashed).
- **The test suite runs against the Go binaries only.** The
  `CCMORPH_TEST_ADAPTER` and `CCMORPH_TEST_HELPERS` switches are removed; Python
  white-box tests that imported the helpers are replaced by Go unit tests.

### Fixed

- **An upgrade no longer deletes `libexec/ccmorph-adapter` under sessions that
  still run it** (#52). A session launched before the Go port names that path in
  its route spec for a mid-session `/model`; deleting it broke the switch with a
  message about gateway availability that named neither the update nor the
  restart. The old copy now stays beside the binary (nothing calls it; it costs
  only disk), and when a session's adapter is missing from disk the refusal
  names the update and the restart.
- **cliproxyapi 8.0.34 and newer is no longer given `cloak: never`** (#42). From
  8.0.34 it strips Claude Code's billing header, identity prompt and
  `metadata.user_id` and rewrites the User-Agent; the adapter now writes it only
  when `cliproxyapi -h` reports an older version. The other translator
  workarounds were re-verified on 8.0.45.
- **The first turn of a session waits for the price table** (#39) instead of
  racing it.

## v0.2.0 — 2026-10-28

### Added

- **Homebrew.** `brew install akleizor/tap/ccmorph` installs a prebuilt bottle on
  Apple silicon (newest macOS) and Linux x86_64, and builds from source with Go
  elsewhere. `ccmorph update` and `ccmorph uninstall` on a brew install
  name the `brew upgrade` / `brew uninstall` command instead of running the
  installer. Profiles stay in `~/.config/ccmorph` either way.
- **model-router add-on** (#32): `addons/router`, installable as a Claude Code
  plugin from this repository's marketplace. Removed again in v0.3.0.

### Changed

- **`gateway-shim` is a Go binary.** Same flags, wire bytes, usage rows and
  `report`/`outcome` subcommands. Under a long session's multi-MB requests it
  holds about 32 MB instead of about 134 MB. Releases ship it prebuilt for macOS
  and Linux (arm64, amd64); a clone builds it with `make shim` (needs Go).
- **Gateway compatibility is built into the shim (`compat/`).** A dangling
  `tool_reference` becomes text automatically (the
  `CCMORPH_REPAIR_TOOL_REFERENCES` switch is gone), a `400` on a message request
  with `thinking` blocks is resent once without them, and two new profile keys
  exist: `CCMORPH_AFFINITY_HEADER` (session id in a header) and
  `CCMORPH_USER_AGENT` (a template with `{version}`).
- **Breaking: request hooks are a Go build, not a Python module.**
  `CCMORPH_SHIM_HOOKS` now names an executable built from a Go `main` around
  `compat.FromEnv()` and `shim.Main`, and the adapter runs it in place of the
  stock shim; it is only for behaviour the built-in compatibility does not cover.
  A Python hooks file no longer loads; the launch stops and names it.

## v0.1.0 — 2026-10-27

First public release as ccmorph. It is beta: commands and settings may still change before 1.0.

### Added

- **Usage rows carry task and attempt.** A launch through the adapter with
  `CCMORPH_TASK` (plus optional `CCMORPH_ATTEMPT`) tags every usage row;
  the shim never changes the requested model. `gateway-shim outcome <task> <result>`
  records how a task ended. See "The usage sidecar" in docs/ARCHITECTURE.md.
- **One-line install.** `sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"`
  downloads the latest release and runs its installer; no clone is needed.
  `CCMORPH_REF` opts into the GitHub archive of a branch or tag (`CCMORPH_REF=main`
  for unreleased code) and `CCMORPH_INSTALL_URL` replaces the whole tarball URL.
  `curl ... | sh` works too but has no terminal, so it declines the optional
  installs. The clone form still works. `make release` writes the stable-name
  `dist/ccmorph.tar.gz` the default URL downloads, to attach to the release.
- **`ccmorph doctor` reports the environment before any profile exists.** With no
  profile it shows the version, `zsh`, `python3`, `claude`, `fzf`, `cliproxyapi`
  and whether `ccmorph` is on `PATH`, read-only, and fails only for a missing or
  too-old required tool.
- **`ccmorph update` and `ccmorph uninstall`.** The installer now ships beside the
  library, so an install updates itself with `ccmorph update [ref]` (the latest
  release, or a branch or tag) and removes its command with `ccmorph uninstall`;
  profiles, credentials and `local.sh` are kept. A git checkout is refused by
  `update` and updated with git.

### Changed

- **The repository root is the launch surface.** `ARCHITECTURE.md` and
  `INTENT.md` moved to `docs/`, `SECURITY.md` and `REVIEW.md` to `.github/`
  (GitHub serves both from there), `CLAUDE.md` to `.claude/` and the `.example`
  templates to `share/`, where the installer reads them; installed paths are
  unchanged, and every link follows the move. `AGENTS.md` keeps the Hive
  contract and points to the new `.github/CONTRIBUTING.md` for verification
  and commit style, and `.git-worktree-policy` (lane tooling, read by nothing
  here) is gone.
- **The installer names what is missing for any package manager.** `zsh` and
  `python3` get the exact command for the first of `brew`, `apt-get`, `dnf`, `apk`,
  `pacman` or `zypper` found, and `claude` gets its setup page; none of the three
  is installed for you. `fzf` is offered through the same manager and still asks
  first. Windows shells (Git Bash, MSYS, Cygwin) are refused with a pointer to WSL.
- **The PATH hint and the closing report fit your shell and the run.** When
  `~/.local/bin` is off `PATH` the installer names the file and line for your
  login shell (`~/.zshrc`, `~/.bashrc` or `~/.bash_profile`, or fish's
  `config.fish` with `fish_add_path`) and still edits nothing. Every run ends with
  `Check:  ccmorph doctor`, and a fresh install adds `Next:   ccmorph add <name>`.
- **The first profile you add becomes the default.** Before, only a subscription
  profile did, so `ccmorph run` failed after adding any other kind until you
  learned `ccmorph use`. An existing default is never changed, and `ccmorph run`
  with no profiles now says to run `ccmorph add <name>`.
- **BREAKING: the project is named ccmorph.** The command, the modules, the
  environment variables (`CCMORPH_*`), the install path (`~/.config/ccmorph`),
  the LaunchAgent labels and the repository moved with it. No old name is read
  and no alias is kept: install the new name, and a profile directory under the
  old path is left where it is.
- **BREAKING: repo and install layout.** `bin/ccmorph` (command), `lib/` (`ccmorph.sh` and
  the modules), `libexec/` (internal executables) and `share/` (data), in the repo and under
  `~/.config/ccmorph` alike. `CCMORPH_LIB_DIR` now names `lib/`. The internal shell prefix
  is now `_ccm_`, so dispatch runs `source lib/ccmorph.sh && _ccm_launch -a`. Re-run
  `./install.sh`; no old path is read.
- **Spend per profile through ccusage.** `scripts/usage-export` writes one
  ccusage project per profile, keeps the gateway's billed cost on every turn
  that has one, and leaves the rest for ccusage to price at list price, so a
  gateway that bills nothing (or a translated route that drops the cost) no
  longer reads as unpriced. The private rate table is gone, days follow the
  local time zone, and the exporter ships in the public tree.

- **Session history is per profile.** Each profile keeps its own `projects/`,
  as a plain Claude Code config dir does, so `/resume` lists only that
  profile's sessions. `ccmorph hop` and `--resume <id>` link in the one
  session they continue, from any profile or native `~/.claude/projects`, and
  Claude Code appends through the link. The shared-history link is removed at
  the next launch (its target, the native history, is untouched), and
  `ccmorph rm` moves a profile's history into native history instead of
  deleting it.

- **A launch pays for fewer python3 starts: the hazard warning and the settings
  generation each ride a run the launch already makes (#433).** The warning's rows
  and its wording each took a start of their own; both now come back as one
  `hazard` record from the interpreter that already answers the launch's read-only
  questions. And the store composes the settings inside the interpreter that reads
  the profile's conf, rather than starting settings-compose from one of its own.
  A launch with a model starts 6 python3 interpreters instead of 8, one without a
  model 5 instead of 7, a token launch 4 instead of 6, and a launch whose route
  trips the warning 7 instead of 10. What the warning says is unchanged, byte for
  byte, in both the plain and the withholding wording, and a route with no hazard
  pays nothing for a record it does not get. `_ccm_switch_hazard_warn` still
  speaks for a launch that gets no record.
- **The launch's window facts come from one interpreter run.** `_ccm_launch_window`
  returns the launched model's published window (with its unprobed fallback), the
  send-id, the route ceiling and the saved `autoCompactWindow` budget in a single
  python3 start, replacing the helper runs that each computed one of them. The
  launcher also no longer re-reads the seeded model from the config store: the
  seed already froze that choice into the launch's model default. A launch with a
  model starts 8 python3 interpreters instead of 11, one without a model 7
  instead of 9.
- **ccmorph.sh loads its code from modules beside it** (`ccmorph-*.zsh`, map in docs/ARCHITECTURE.md "Code layout"). The installer ships them; behaviour is unchanged.
- **Identifier handling is a per-profile opt-in; the shim is schema-neutral by
  default.** It no longer removes `metadata` or the session and `X-Stainless-*` headers
  from every request. `env.CCMORPH_RANDOM_DEVICE_ID=1` swaps the `device_id` in
  `metadata.user_id` for a random per-session value (session id untouched).
  Add the key to a profile's `.conf` where wanted (see "Identifier handling" in
  docs/guide.md).
- **Gateway-specific rewrites left the core shim for a request-hooks module
  (issue #438).** The core shim now forwards the `User-Agent` exactly as the
  client sent it; the rewrites that exist only because one gateway misbehaves —
  the `User-Agent` restatement, dangling tool-reference repair and the
  session-affinity header — are served by a module a profile opts into with
  `env.CCMORPH_SHIM_HOOKS=<file>`, and the adapter ferries the profile's other
  `CCMORPH_*` keys to it. A profile pointing at a missing module fails the
  launch loudly. ccmorph's own quirks module ships outside the public
  snapshot, so a release names no gateway; the `--repair-tool-references` and
  `--affinity-header` shim flags are gone with it.
- **What the shim changes in an outbound request is now written down.** By
  default only transport plumbing: internal tag (`x-ccmorph-*`) and hop-by-hop
  headers are dropped, identity encoding is forced, and inbound-token
  launches replace authentication headers. `metadata`, client identifier
  headers and the `User-Agent` are preserved; identifier changes are the
  per-profile opt-in above, and any body or header rewrite is the profile's
  hooks module's doing. Model resolution and explicit model maps can change
  the requested model. This is identifier preservation, not whole-request
  pass-through.
- **A launch takes its tuning from your shell, and its backend only from the profile.**
  `MAX_THINKING_TOKENS`, `CLAUDE_CODE_SIMPLE` and
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` now reach Claude Code from the shell
  when the profile does not set them (a profile's own value still wins, and a
  nested launch never inherits an outer session's, including the
  `CLAUDE_CODE_SIMPLE` that `claude --bare` sets, detected by
  `CLAUDE_CODE_CHILD_SESSION=1`, not `CLAUDECODE`, which every Claude Code shell has). Backend and credential
  variables (`CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CODE_OAUTH_REFRESH_TOKEN`,
  `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
  `ANTHROPIC_UNIX_SOCKET`, `ANTHROPIC_CUSTOM_HEADERS`,
  `CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY/GATEWAY/MANTLE/ANTHROPIC_AWS/ANTHROPIC_GOOGLE_CLOUD`,
  `AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_AWS_API_KEY`, `ANTHROPIC_FOUNDRY_API_KEY`,
  `ANTHROPIC_FOUNDRY_AUTH_TOKEN`, `ANTHROPIC_AWS_BASE_URL`, `ANTHROPIC_BEDROCK_BASE_URL`,
  `ANTHROPIC_BEDROCK_MANTLE_BASE_URL`, `ANTHROPIC_VERTEX_BASE_URL`, `CLAUDE_CODE_API_BASE_URL`,
  `ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_WORKSPACE_ID`, `ANTHROPIC_PROFILE`,
  `ANTHROPIC_FEDERATION_RULE_ID`, `CLAUDE_CODE_ACCOUNT_UUID`, `CLAUDE_CODE_ORGANIZATION_UUID`,
  `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`, `CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR`,
  `CLAUDE_CODE_SESSION_ACCESS_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`,
  `ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_GOOGLE_CLOUD_BASE_URL`, `ANTHROPIC_IDENTITY_TOKEN`,
  `ANTHROPIC_IDENTITY_TOKEN_FILE`) from the shell no longer reach a launch; the Remote Control add-on reads the same
  list from ccmorph.
- **A gateway profile needs only its base URL and key.** Every adapter launch now
  also hides the server-side advisor tool and exposes `/effort`, as it already
  kept Claude's prompt prefix stable for caching. These were per-profile `env.`
  lines that a second machine never got; a profile's own value still wins.
- **The prompt-cache mask is one rule for every model.** It no longer names
  `claude-*`: Claude Code only grants the mid-conversation system turn to ids in
  its own catalogue, so other models already ran this way and are unchanged.
- **A profile `.conf` has a template and a fixed layout.** `profile.conf.example`
  ships with the library as the documented key list for the profile's type,
  installed beside `local.sh.example`. Every writer now emits the
  same five sections in the same order — connection, model choice, behaviour,
  context, machine-written state — owned by `CONF_SECTIONS` in `model-window`, so a
  file edited by hand comes out tidy the next time ccmorph saves it. A comment
  block stays with the key line below it, and a line the layout does not know keeps
  its place after the known ones.
- **Arguments pass through untouched.** `ccmorph run <profile> [arguments]` no
  longer inserts `--settings` or `--model`, so every Claude Code subcommand —
  including `remote-control`, which refuses `--settings` — works through ccmorph,
  and no list of subcommands is needed. The launch model travels as
  `ANTHROPIC_MODEL`; endpoint and credential stay in the process environment.
- **Profiles get a generated `settings.json`.** It is written from
  `~/.claude/settings.json`, the profile's new `settings.profile.json` layer and the
  saved model defaults, so a plain `claude` run or a Remote Control session with the
  profile's `CLAUDE_CONFIG_DIR` sees the same configuration as a ccmorph launch.
  Edits Claude Code makes to `settings.json` are carried into the layer before each
  rewrite; the previous file is kept in `backups/`. The first launch adopts the old
  `settings.json` as the layer. The temporary `launch-settings.*` file is gone.
- **An explicit `--model` is no longer rewritten** to its `[1m]` id on a 1M route;
  ccmorph names the id instead. Models ccmorph chooses still launch with it.

### Fixed

- **The gateway shim starts without a reverse DNS lookup.** Python's HTTP server
  resolves its own bind address with `getfqdn`, and on macOS that lookup of
  127.0.0.1 can stall for tens of seconds before the shim reports ready, which
  delays every adapter launch behind it. The shim binds loopback only and now
  skips the lookup.
- **`--config=PATH` names a config, and a missing or flag-like value is refused.**
  The supervisor matched only the exact string `--config`, so the equals spelling
  used across ccmorph's other CLIs named nothing and the default
  `$CCMORPH_STATE_DIR/rc-hosts` was watched instead, while `--config --verbose`
  watched a file called `--verbose`. Both spellings now read the path, and a
  missing or flag-like value in either is a usage error on stderr naming the
  problem, exit 2, before any watching starts (issue #417). The LaunchAgent
  passes no arguments, so an installed supervisor is unchanged.
- **A fresh Remote Control install starts its hosts.** The supervisor's shared
  edge sits in the bridge dir under the state root, and `Host.start()` checked
  for that folder before anything created it — the log dir's own `mkdir` sits
  past that early return — so on a first install the edge backed off forever
  and, every folder host gated on edge readiness, nothing ever started
  (issue #419). The edge now creates the bridge dir before the check.
- **A turn no longer overwrites another turn's usage row.** When a turn's primary
  and spill names both hold other turns' rows, it moves to a further spill name
  instead of replacing one, and drops its own row only when all of them are
  taken. See "The usage sidecar" in docs/ARCHITECTURE.md.
- **The no-hardlink claim no longer drops a racing turn's usage row.** Where a
  filesystem has no hardlinks, claiming a row's name fell back to a
  check-then-replace, so two writers racing on one response id could both see
  the name free and the second replace clobbered the first turn's row — a row
  the displaced turn only recovered at its next usage update, which a cancelled
  stream or a killed process never writes. The claim is now an exclusive create,
  atomic like the hardlink, so the loser lands in its spill on that first write
  (issue #444). See "The usage sidecar" in docs/ARCHITECTURE.md.
- **Task tags no longer drop the user's own request headers.** A tagged launch
  sets `ANTHROPIC_CUSTOM_HEADERS` for its `x-ccmorph-task`/`-attempt` lines, and
  the generated settings strip that key (the process environment must win), which
  silently removed any `env.ANTHROPIC_CUSTOM_HEADERS` the user's own settings
  layers carry — a tenant id, say — for exactly the launches carrying a tag. The
  one diagnostics interpreter that already answers the launch's settings-layer
  questions now also brings that composed value back, and the launcher sends the
  user's lines in front of the tags; an untagged launch is unchanged, and the
  launch path starts no interpreter it did not start before.

- **A launch on a mapped alias keeps its model.** The session's `availableModels` is
  rebuilt from the endpoint's catalogue, and a `model_map` alias is never in it — the
  shim is what turns the alias into a model. So a hop, or any `--model <alias>`, was
  relaunched on a model the generated list did not carry, and a launch model outside
  that list is not refused but silently remapped onto Claude Code's own catalogue. The
  projection now keeps a launch model that is a `model_map` key whose target the
  profile does not exclude, and an id that is neither a served row nor a map key
  stays out exactly as before. A pinned family alias no longer subsumes it either,
  since `sonnet` resolves to the route's own newest sonnet while the map names
  another model. An unreachable endpoint still rebuilds nothing beyond the ids the
  profile excludes.

- **A Remote Control host config line whose folder path cannot be resolved is skipped,
  not fatal.** `resolve()` raised for a path it cannot canonicalise (a symlink loop, an
  embedded NUL byte) — out of `parse()` at every config reload and out of
  `session_entries()`/`archived_set()` at every idle check, so launchd restarted the
  supervisor into the same crash and every host restarted with it. Such a line is now
  refused with a log line, like a valueless pin. `ccmorph-rc-hosts --config` with no
  path prints usage instead of raising `IndexError`.

- **A model map is keyed by the model ID the gateway sees.** The `[1m]` window marker
  is ccmorph's own tag, stripped from the map's lookup key and from the model handed
  to adapter preparation, so a map entry keyed with it named nothing: a native client's
  `claude-sonnet-5-5[1m]` picker row reaches the wire as `claude-sonnet-5-5` (Claude Code
  normalizes it), while the shim's second lookup still honoured the suffixed spelling.
  Both ends now resolve
  one key, and a key carrying the marker — in either spelling ccmorph reads it — is
  refused with a plain error by every profile read (the launch and the adapter's
  `ccmorph _env` alike), instead of launching a session whose map silently does
  nothing.

- **The shim never falls back to an older model.** An unserved Claude id is replaced
  only by a served model of the same family whose version is the same or newer (a dated
  snapshot, a later release). With none, the request goes through unchanged, so the
  gateway's own error shows, and the shim logs one line to stderr.

- **The shim no longer doubles CJK request bodies.** Stripping `metadata` and
  repairing tool references now share one parse and one dump, written as UTF-8
  instead of `\uXXXX` escapes; unchanged or unparseable bodies pass byte for byte.
- **The usage sidecar keeps the gateway's billed cost and sits where consumers look.**
  A gateway's `usage.cost` (finite, `>= 0`) is now written into the row's `usage`,
  and the row is named `sha256(response_id).json` as documented; a recycled
  response id spills to `sha256(response_id.turn_id).json`, or on to
  `sha256(response_id.turn_id.N).json` (N = 1–3) when that name is taken too,
  keyed on a random per-turn token rather than the timestamp, so an earlier turn
  is not overwritten even when two turns carry the same microsecond timestamp.
- **Test stubs die with their launcher.** The adapter stubs in
  `adapter_route_defaults_test` and `cli_model_selection_test` honour the
  `--parent-pid` the launcher passes, so a hard-killed run stops leaking
  immortal `/bin/sh` stubs (issue #386: 124 found on the mini, ~600 process
  spawns/s of permanent load). `gateway-shim` gains `--parent-pid=<pid>` —
  the adapter's own contract — and the shim tests pass it and escalate
  teardown to SIGKILL, so a run killed outright no longer leaves shims
  listening for hours.
- **`publish_test` batches its git lookups.** One `ls-tree -r` replaces two
  `cat-file` subprocesses per string literal; the public-dependency snapshot
  check was the slowest test in the suite (52 s under load) and now runs in
  seconds.

### Removed

- **The Homebrew formula is gone.** It pointed at a private package host and the
  documented `brew install ccmorph` had no public tap behind it. Install and update
  with `./install.sh` from a clone. The launcher no longer looks for a brew keg.
- **The transport seam is gone, and an ambient `HTTPS_PROXY` now survives.**
  Bridging to a private endpoint is a separate product: start its local proxy and
  run `HTTPS_PROXY=http://127.0.0.1:<port> ccmorph run <profile>`. ccmorph passes
  `HTTPS_PROXY`, `https_proxy` and `NO_PROXY` through unchanged to the Claude child,
  the adapter, model listing, metadata discovery and forward_ip probing; only its own
  loopback health checks still bypass a proxy. This fixes a real bug — every consumer
  used to be handed `HTTPS_PROXY="${_ccm_route_https_proxy:-}"`, which BLANKED the
  proxy a user had correctly exported whenever no transport ran.
  Removed with it: the `transport=` profile key, `CCMORPH_TRANSPORT_RUNNER` /
  `CCMORPH_TRANSPORT_PROXY`, the direct-reachability probe and its cache, the
  `route` row in `ccmorph inspect`, the transport rows in `ccmorph doctor`, and
  the bundled reference transport with its `ccmorph-via-runner` and
  `ccmorph-connect-proxy` entry points. `ccmorph _route_env` / `_route_stop`
  remain, now only to start and retire a `provider` profile's bridge.
  A conf still carrying `transport=` keeps launching: the key is reported once on
  stderr and ignored. `ccmorph doctor` now probes the gateway through the caller's
  proxy rather than refusing one, so it answers for the route the user actually has.

- **`forward_ip`, `subagent_model`, and `lean` are gone.** `forward_ip`'s
  source-IP allowlist header (`X-Forwarded-For`) is removed from
  `ccmorph-adapter`, `gateway-shim`, `model-window` and
  `refresh-model-registry`, including its contribution to the adapter's route
  identity digest (now `upstream + key`). `subagent_model` was accepted by the
  profile parser but read by nothing, so it is dropped outright. `lean` and the
  `is_lean` launch logic (the `--bare --strict-mcp-config` tool-stripping path
  for a slow local backend) are gone too. All three keys still launch an old
  conf: each is reported once on stderr and ignored, the same as `transport=`.
- **Two silent env-var opt-ins are gone.** `CCMORPH_SKIP_PERMISSIONS` used to
  add `--dangerously-skip-permissions` to every launch from `local.sh`; an
  explicit `--dangerously-skip-permissions` flag on the command line still
  passes through untouched, but there is no more ambient opt-in.
  `CCMORPH_MANAGED_MCP` and `_ccm_project_mcp` used to project one externally
  rendered `mcpServers` map onto every profile at launch; a profile now always
  keeps whatever MCP set it was seeded with or added itself.
- **`.gitlab-ci.yml` and `packaging/web/` are gone.** GitHub Actions remains the
  only CI; the internal install page is retired along with the
  `.impeccable/config.json` entry that existed only for it.

## Before ccmorph

The entries below are the private predecessor's releases, kept for their history. Their version numbers are not comparable with 0.x.

## v2.5.1 — 2026-09-18

- **Fix noninteractive help output.** Print the `-i` usage line as literal text
  instead of letting zsh interpret it as an option. The installed-command test
  now requires empty stderr and the complete help line.

## v2.5.0 — 2026-09-18

- **Remove seven redundant environment overrides.** The adapter locates its
  translator through `PATH`, uses its packaged sibling shim, and keeps readiness
  and selected-model deadlines as built-in constants. Legacy registry and profile
  cache paths derive from `CCMORPH_STATE_DIR`; old default locations and migration
  backups remain supported. For unattended installation, redirect stdin from
  `/dev/null` instead of setting `CCMORPH_ASSUME_NO`. The removed names are
  `CCMORPH_ADAPTER_TRANSLATOR`, `CCMORPH_ADAPTER_GATEWAY_SHIM`,
  `CCMORPH_ADAPTER_READY_TIMEOUT`, `CCMORPH_ADAPTER_SELECT_DEADLINE`,
  `CCMORPH_PROFILE_DIRS`, `CCMORPH_MODEL_REGISTRY`, and `CCMORPH_ASSUME_NO`.
  Move nonstandard legacy data into the state root before requesting migration.

- **Consolidate three more ccmorph options.** Adapter classification uses eight
  workers and reachability caching uses 60 seconds; `CCMORPH_ADAPTER_JOBS` and
  `CCMORPH_REACHABLE_TTL` are no longer read. Adapter profile directories now
  follow `CCMORPH_STATE_DIR` (or the usual XDG default), without a separate
  `CCMORPH_PROFILE_DIRS_OVERRIDE`. Tests use the same state-root interface.

- **Remove five ccmorph environment overrides.** Forwarded-IP caching uses its
  existing one-day lifetime; adapter classification-lock waiting uses 120 seconds;
  catalogue discovery uses the two built-in models.dev URLs; provider executable
  selection uses `PATH`. `CCMORPH_FORWARD_IP_TTL`, `CCMORPH_ADAPTER_LOCK_WAIT`,
  `CCMORPH_CATALOGUE_URL`, `CCMORPH_CANONICAL_CATALOGUE_URL`, and
  `CCMORPH_PROVIDER_BIN` are no longer read. Normal defaults are unchanged.
  Restore the native Claude environment allowlist altered by the unrelated
  env-list refactor; model mappings and ambient-variable cleanup are preserved.

- **Profile state moved into ccmorph's own state root.** Per-profile runtime
  state was `~/.claude-profiles`, a hardcoded home-directory path that ignored
  both `CCMORPH_STATE_DIR` and `XDG_CONFIG_HOME` — so a host that redirected the
  root got its `.conf` files in one place and its profile homes in another. It is
  now `<state root>/profiles/<name>`, default `~/.config/ccmorph/profiles`, and
  the launcher, the adapter, and context migration all derive it from that one
  root. The move runs itself once, on the next install or command: a rename rather
  than a copy, so no credential is duplicated and modes and inner symlinks survive
  as they are. The old path is left as a compatibility symlink, so a session
  already running and any external tool holding the absolute path keep working. It
  refuses rather than merging when both locations hold real directories, refuses a
  cross-device move instead of copying, and rolls back if it cannot leave the
  symlink behind. Native `~/.claude`, the shared harness layer, and MCP secure
  storage are untouched; uninstall still retains profile state.

- **`-i` now means "ask" everywhere, not only at launch.** `ccmorph set gw model
  -i` used to be an error: the same flag meant "choose from a list" on `run` and
  "invalid argument" on `set`, so a reader who learned it once had to learn, per
  verb, whether it applied there. A gateway's model listing cannot be enumerated in
  advance — it changes without ccmorph knowing — so refusing to ask was refusing
  the only reliable way to spell a model id. `-i` now works on `set`, `get`,
  `context`, `inspect`, `use`, `login`, and `cred`, and picks the profile when none
  is named. `rm` and `mv` stay explicit: destroying or renaming should not be a
  menu selection.

- **An omitted value asks rather than printing usage.** `ccmorph set gw model` is
  the same request as `-i`, so it opens the picker. A usage line telling the reader
  to supply a value is useless precisely when they do not know the value.

- **`set` confirms what it saved.** The bare value still goes to stdout, so
  `$(ccmorph get ...)` and existing scripts are unaffected; the confirmation goes
  to stderr, where a *picked* value needs it most — the user never typed the id
  they chose, so without it the terminal showed a list closing and nothing else.

- **A missing `fzf` is not a dead end.** The picker names the direct command with
  the profile already filled in and prints the choices it would have offered.

- **`probe` now says what to do next.** A model the gateway publishes no size for
  and no catalogue knows — a model added to the gateway today — produced a run that
  named the problem (`no declared size`) and stopped. The three remedies existed but
  printed only when some *other* model in the same run resolved, so
  `ccmorph probe <profile> --model <new-id>`, the exact command a reader reaches
  for, was the one shape with no next step on screen. The advice now prints on both
  exits, ordered cheapest first, with the profile and model already substituted in.

- **`ccmorph context <profile> <model> [tokens|auto]`.** The free fix `probe` now
  recommends is a command. Setting a window by hand previously meant editing
  `context_window.<model>` into a `.conf` the CLI otherwise owns — a step no output
  ever mentioned. Reads with two arguments, sets with a token count, returns the
  model to discovery with `auto`.

- **A successful probe reports the outcome, not the write.** `wrote 1 context
  entries to gateway.conf` named a file and a count, which cannot tell a reader
  whether the model they asked about got a window or whether anything changed at
  all. Each changed limit is now printed with its model and number, catalogue
  estimates are labelled as estimates, and a run that changes nothing says so.

## v2.4.0 — 2026-09-09

- **Profiles can declare ordered transport fallback.** `transport=primary,secondary`
  keeps direct reachability first, then tests each opaque runner token against the
  real endpoint. Service answers (`2xx`, `401`, `403`, `404`) select that route for
  the session; interception/unreachable answers (`405`, `5xx`, `000`) stop it and
  advance. Single-token profiles retain the established readiness-only behavior.
  Core learns no locations, networks, products, or token meanings.

- **The window-recording verb is `ccmorph probe`.** One verb, one spelling: the
  previous name is gone from the tool, its docs, its tests and its packaging — 826
  occurrences, including the `unprobed` JSON key the resolver and the launcher
  exchange, renamed in one pass so no half of that contract could drift. The former
  spelling still dispatches, so a script that already uses it keeps working, but
  nothing teaches it and a doc guard fails if anything tries.

- **An unprobed launch now says what budget it is actually running under.** The old
  advisory reported only that the window was unknown, and was followed by a hazard
  list naming
  OTHER models' walls (131k, 128k…), leaving a reader with a 200k bar and no way to
  tell that 200k is Claude Code's fallback for an unknown model rather than their
  model's real size. The line now names the budget in force and labels it a fallback.
  The number is read from the resolver, never written into the source.

- **`--bisect` finds a ceiling a gateway refuses to state.** Some gateways strip the
  number out of their own refusal (observed: `[prompt is too long]` with the count
  removed), and the tool recorded nothing for those. A refusal is still an answer —
  it says the ceiling is below what was sent — so halving brackets the boundary with
  no cooperation from the error text.

  The cost asymmetry is the design: a refused request is free (rejected before
  inference), an accepted one is billed for its whole payload. So the descent halves
  while refused, which is free and reaches the bracket from above — making the one
  unavoidable billed request the cheapest available — and narrowing probes high in
  the remaining range so the likely outcome is another free refusal. Accepts are
  capped (default 1), because capping steps would buy a dozen accepted megabyte
  payloads while "in budget". One accept lands 6–28% low, which is safe: an
  understated window compacts early, an overstated one fails late.

  Because the worst case is large — a 46-model catalogue is ~46× one model — the flag
  prints its worst case in tokens before sending anything and, on a terminal, asks.
  Tokens rather than dollars: the tool cannot know what the operator pays, and
  inventing a figure for someone else's contract is worse than none. Non-interactive
  runs are not blocked.

- **A launch no longer re-probes what it already knows.** Three caches on the launch
  path were each defeating themselves, together turning a warm launch into a ~20s wait:

  - the per-model window cache was written with a single `>|`, so it held ONE row and
    was really a *last model* cache — launch A, launch B, then A again and A had been
    overwritten, so it probed from scratch. On a route whose models are switched
    between, that is a probe on nearly every launch. It is now a keyed table (one row
    per model, 64 max, updated in place, single atomic write).
  - the `forward_ip` cache expired every 300s, and each expiry cost a cold walk of
    every candidate — timed at 20.3s for `ccmorph _env` alone. An allowlisted
    source address changes when the pod restarts, not on a clock, so the TTL is now a
    day (`CCMORPH_FORWARD_IP_TTL`); a stale entry costs one failed request that falls
    through to a re-probe.
  - the direct-reachability verdict was not cached at all, so every launch and every
    subprocess paid a flat 2s timeout to re-learn that a private endpoint still is not
    directly reachable. Now cached with a deliberately SHORT 60s TTL
    (`CCMORPH_REACHABLE_TTL`) — reachability is exactly what flips when a laptop
    joins or leaves the private network, and being wrong there routes a launch down
    the wrong path.

  End to end: **~20s → 0.4s** warm, with `CCMORPH_REACHABLE_TTL=0` still forcing a
  real probe so a network change is detected rather than assumed away.

- **Probe search no longer rebuilds its payload every step.** Each step regenerated a
  6MB string from ~54k pieces while the caller already held a superset of it; the
  search now slices that blob (231ms → 0.6ms per model, and it no longer serialises
  `--jobs` workers behind the GIL). The oversized payload is also built lazily, so
  `--declared-only` — the mode `ccmorph add` runs synchronously while a human waits —
  builds nothing at all.

- **Fixed: a timeout could be recorded as a ceiling.** The searcher briefly reported a
  timed-out request as a refusal so it could get past a slow opening probe, which
  silently manufactured wrong answers — under `--probe-timeout 3`, a model holding
  hundreds of thousands of tokens was recorded at 23,051, a number read off the clock
  rather than the model. A timeout now ends the search and records nothing, because a
  wrong entry in a registry that is trusted permanently is worse than an absent one.

## v2.3.0 — 2026-09-08

- **The `via` key is now `transport`, and its variables are `CCMORPH_TRANSPORT_*`.**
  `via=<token>` never said what it selected — it read like a preposition, not a
  seam. A profile now writes `transport=<token>`, and the machine-local seams are
  `CCMORPH_TRANSPORT_RUNNER` / `CCMORPH_TRANSPORT_PROXY`
  (`CCMORPH_TRANSPORT_DEFAULT` for the token `add` records, `CCMORPH_TRANSPORT`
  in `_route_env` output). There is **no back-compat alias**: `via=` is no longer a
  recognised key and a profile still carrying it is rejected as invalid, so rename
  the line in any `*.conf` and the exports in `local.sh`.

- **`forward_ip` accepts a range, and ccmorph picks a working address.** A fixed
  `forward_ip` already survives a rotating pod IP (the gateway allowlists a whole
  range, and a fixed value stays inside it), but if individual addresses come and
  go you can now give a range instead of one address:

  ```
  forward_ip=192.0.2.10-20                          # one octet as lo-hi
  forward_ip=192.0.2.10,192.0.2.15,192.0.2.20    # or a comma list
  ```

  At launch ccmorph probes the candidates against `<base_url>/v1/models` (carrying
  the key and each candidate as `X-Forwarded-For`, last-good first) and uses the
  first the gateway accepts, cached briefly. The range is profile data — ccmorph
  bakes in no address, subnet, or gateway — and probing rides whatever transport the
  profile's `transport` already configures. Nothing accepted, or an unreachable endpoint,
  leaves `forward_ip` empty and the launch proceeds without the header. A single
  address is still emitted verbatim, never probed.

  This supersedes the `forward_ip=auto`/`CCMORPH_FORWARD_IP_RESOLVER` seam: the
  range and the probe are now built in and generic, so no host resolver script is
  needed.

- **`ccmorph probe` works on an account that needs a tunnel.** It read a
  profile's endpoint and credential from `ccmorph _env` and then dialled with plain
  urllib, while a LAUNCH of the same profile first asked `_ccm_route` for a
  transport. So on any account carrying `transport=`, probing from outside the private
  network could only ever time out — and the timeout was reported as
  `probed nothing; refusing to overwrite the registry`, naming a file the reader
  had not asked about instead of the network. Probing and launching now resolve
  their route through one seam (`ccmorph _route_env`), which returns the endpoint,
  the credential, the resolved `forward_ip` and the transport's proxy together; the
  requests go through it, and the route is retired when the run ends.

- **Failure messages say what happened and what to do.** Three that described
  internal mechanism to whoever wrote the code:
  - `probed nothing; refusing to overwrite the registry` → the reason the run had
    already collected (endpoint timed out, credential refused, DNS, no declared
    size) is now printed, grouped by cause, followed by how many existing values
    were kept.
  - `adapter for 'x' did not become ready` → now names what the adapter is, which of
    the three ways it failed (exited during startup / crashed / still not serving),
    the likely cause, and the last line of its log rather than only a path to it.
  - `model list unavailable (URLError(TimeoutError('timed out')))` → a python
    exception class is what the code caught, never what went wrong; the message now
    names the host, the cause, whether the account declares a transport, and the
    action.

- **Fixed: a tunnelled profile could never resolve a `forward_ip`.** Three causes,
  each independently fatal, all on the path that exists FOR a gateway behind a
  tunnel — so the feature was reachable only on a direct route:
  - the proxy flag was built as `${proxy:+--proxy "$proxy"}`, which zsh keeps as ONE
    word: curl received the literal argument `--proxy http://127.0.0.1:NNNNN`,
    rejected it as an unknown option, and scored every candidate `000`;
  - `--connect-timeout` covers the TLS handshake, not just the TCP connect. Over the
    tunnel, TCP completes in 0.0004s while appconnect takes 3.4–3.8s, so at 2s the
    probe could never succeed — 000 at exactly 2.00s, every candidate, every run.
    Raised to 10s, with 20s total (a whole successful probe probed ~5.5s);
  - one `000` abandoned the entire candidate list. A transient timeout therefore
    discarded four working addresses.

  Each ended the same way: no header, 403 on every model, reported to the user as an
  expired key.

- **A probe's own failures are explained too.** The previous entry fixed the
  listing path and left `probe()` returning `repr(exc)` and a 90-char slice, so a
  real run printed `TimeoutError('The read operation timed out')` — the class the
  code caught, not what happened. A timeout now says the probe is deliberately
  over-limit (~1,500,000 tokens), that the route answers but not within the budget,
  and names `--probe-timeout`. The opaque-body note widens 90 → 300 chars, because
  90 cut off exactly where a wrapped body's number would be: with the full text, a
  gateway that reports `[prompt is too long]` and strips the number is
  distinguishable from one whose number the patterns simply missed.

- **Fixed: a ceiling served as hex was invisible.** One gateway forwards
  Bedrock's error body as `SdkBytes(bytes=0x7b22…)`, and inside it is the ordinary
  `prompt is too long: N tokens > M maximum` the parser already understands. Every
  probe on that route reported `opaque` and probed nothing while the answer was in
  the response the whole time; the blob is now decoded before the patterns run.

## v2.1.0 — 2026-09-08

- **`forward_ip` for gateways that allowlist by source IP.** Some gateways accept a
  request only from an allowlisted address and trust a client-supplied
  `X-Forwarded-For` to carry it — so a laptop outside that network is refused (403)
  with a perfectly valid key. A profile can now present a fixed one:

  ```
  forward_ip=192.0.2.46
  ```

  The adapter sends `X-Forwarded-For` on every upstream call: its own model-discovery
  probes (which hit the gateway directly) and the served inference traffic (through
  cliproxyapi's per-entry `headers:` map — the one config field that reaches the
  upstream call; the others were verified to leave it at 403). The gateway-shim is
  force-interposed when `forward_ip` is set, since it is the last hop before the
  translator and Claude Code sends no such header itself.

  This is what makes a shared-pool gateway usable from a machine outside its network.
  Two caveats, both documented: the value is only as stable as that IP (a workspace
  pod's changes on restart), and it relies on the gateway trusting a client
  `X-Forwarded-For` — confirm that path is intended before relying on it. Absent the
  key, nothing is added and traffic is byte-for-byte unchanged.

## v2.0.0 — 2026-09-08

**ccmorph is now one ordinary command.** It does not edit your shell, define
shell functions, or shadow `claude`. This is a breaking change with no
compatibility shims, taken during internal testing while that is still cheap.

- **`brew install` is the whole install.** There is no wiring step, no
  `exec zsh`, and nothing to add to a startup file: brew puts one executable on
  `PATH` and it works immediately, in any shell. The previous design shipped the
  tool as zsh *functions*, which a package manager cannot place on `PATH`, so a
  fresh install answered its own first documented step with
  `command not found: ccmorph`. Every fix inside that design was a better way of
  explaining a broken first impression; the fix was to remove the step.

- **The command surface collapsed to one word.**

  | Was | Now |
  |---|---|
  | `claude` | `ccmorph start` |
  | `claude -a <account>` | `ccmorph <account>` |
  | `claude -a ? --model ?` | `ccmorph start -i` |
  | `ccrelay <account>` | `ccmorph hop <account>` |
  | `ccmorph` (list) | `ccmorph list` |
  | `ccmorph-init`, `ccmorph init` | removed — nothing to wire |

  `ccmorph start` launches the default account and a bare `ccmorph` prints usage,
  the way every tool with a verb vocabulary does — `git`, `docker`, `npm`, `gh`,
  `kubectl`, `brew`. Only single-purpose tools (`fzf`, `vim`, `top`) act on a bare
  invocation, and ccmorph has twelve verbs. Everything after the account name
  passes through to Claude Code untouched, so `ccmorph work --resume` and
  `ccmorph work -p '…'` work as written; a leading flag with no account launches
  the default one.

- **`claude` is yours again.** Shadowing it was the same mistake one level up: a
  switcher that overrides the tool it switches makes every session's behaviour
  depend on whether one file was sourced. Anyone who wants `claude` to mean
  "launch through ccmorph" can write that alias themselves — a personal
  convention, not something the product imposes.

- **`-i` / `--interactive` is the one way to say "ask me",** at every site that
  takes a choice: a launch, `set <acct> model`, `set <acct> effort`. It is a flag,
  never a value, so it appears once per line and its position says what it applies
  to — `ccmorph -i` asks for account then model, `ccmorph work -i` asks only for
  the model.

  This retires both earlier spellings outright, with no synonyms left behind.
  `@menu` was a *value*, so it had to be repeated once per slot: the documented
  launch line read `ccmorph @menu --model @menu` — one token twice for two
  different questions, with nothing on the line saying which was which. `?` was an
  alias for it and worse than ambiguous: ccmorph is an executable, so the caller's
  shell expands a bare `?` before it starts, and in a directory holding any
  one-character file `set <acct> model ?` silently recorded that *filename* as the
  default and reported success. A `noglob` alias used to hide that, which meant the
  protection existed only in shells that had sourced the library — absent on
  exactly the fresh machines reading the docs that advertised it.

- **The installer removes an older version's rc block** rather than leaving a
  managed block behind that sources a library which no longer defines any command.
  It reports the removal instead of editing your rc silently, and it checks
  `~/.local/bin` is on `PATH` rather than arranging that behind your back.

- **`CCMORPH_LIB_DIR` is passed into each session**, so `! ccmorph hop <account>`
  typed inside Claude Code resolves the same library that launched it. The launcher
  prefers `<prefix>/opt/ccmorph` over the versioned Cellar path, so nothing it
  hands to a child dies on the next `brew upgrade`.

- **Claude Code's own subcommands reach it through ccmorph.** `ccmorph mcp list`
  was read as an *account* name and answered `unknown command or account 'mcp'`,
  which made all 19 of them unreachable — wrong twice over, because MCP servers,
  logins and plugins are per-profile state, so they are exactly what a switcher has
  to be able to address, and routing them through the launch pipeline is what points
  them at the right profile's config directory. `ccmorph mcp list` uses the default
  account, `ccmorph work mcp list` a named one. The list is explicit, not a
  catch-all: `ccmorph mpc list` is still an error naming both readings rather than a
  typo handed to Claude Code to reject.

- The permission-bypass opt-in (`CCMORPH_SKIP_PERMISSIONS`) moved from the
  retired `claude` wrapper into the launch driver, so it now applies to every
  launch path instead of only to shells that had sourced the library.

## v1.20.5 — 2026-09-07

- **Sandboxed launches no longer print a shell permission error before Claude
  starts.** After v1.20.4 made the adapter lock optional, the full launch reached
  Claude Code but exposed one more best-effort write to the same boundary: saving
  the resolved context window under the profile cache. The trailing `|| :` kept
  the launch alive, but zsh reports a redirection failure before the command's
  own stderr redirect exists, so every launch still printed a shell-level
  `operation not permitted` error. The write and its directory creation are now
  one stderr-suppressed group. An unwritable cache is not a launch failure; the
  registry remains authoritative and the next launch simply resolves it again.

## v1.20.4 — 2026-09-07

- **A launch no longer dies because the classification lock could not be opened.**
  The shared adapter cache is guarded by a sidecar `.lock` file, and opening it is
  a write. In a sandboxed launch context that write can be refused on a path an
  ordinary shell in the same session writes fine — reported as `[Errno 1]
  Operation not permitted` — and the exception escaped `classify()`, killing the
  adapter about a second into every launch with `adapter for '<name>' did not
  become ready` and the real cause only in the log. The lock is an optimisation,
  never a precondition: if it cannot be opened, the sweep now runs unlocked and
  the launch proceeds from cache. Losing serialisation costs a redundant probe
  sweep; losing the launch costs the session. `refresh-model-registry`'s commit
  lock had the same shape and now degrades the same way, reporting rather than
  raising.

## v1.20.3 — 2026-09-03

- **The wiring now survives `brew upgrade`.** v1.20.2's `ccmorph-init` resolved
  its own symlink chain to find the library, which on a real install ends in the
  versioned keg — so the block it wrote named a directory the next upgrade
  deletes. The guard on that line then failed silently and a new shell was back
  to `command not found`: the bug v1.20.2 fixed, returning on a delay with
  nothing to explain it. It now records the version-stable `opt` path, derived
  from where the script actually sits rather than from an assumed prefix. A test
  builds a keg layout, upgrades it, and asserts a new shell still works.
- **`CCMORPH_LIB_DIR` works as an override.** It was consulted last, so it could
  never beat a library sitting beside the script, and its emptiness guard was
  dead. It is now authoritative and checked first, and a wrong value fails loudly
  instead of quietly loading a different library than the one named.

## v1.20.2 — 2026-09-02

- **`brew install` then `ccmorph` no longer says `command not found`.** The
  packaged install documented `ccmorph init` as its first step, but `ccmorph`
  is a zsh *function* that only exists once the shell has sourced the library —
  and sourcing it is exactly what that step was meant to arrange. The instruction
  required the thing it created, and the formula installed no executable that
  could do it, so every fresh user was told to run a command that could not exist
  yet. There is now a real `ccmorph-init` on `PATH`: it finds the library beside
  its own installed location, adds one managed block to the shell config, and is
  safe to re-run. Homebrew's caveats, the web page, and the README all lead with
  it, and the formula's own test asserts it runs and stays idempotent.
- **An unreachable internal endpoint now names the network, not an internal
  variable.** With the VPN off, a launch reported `'gateway' endpoint
  unreachable directly and no CCMORPH_VIA_RUNNER configured for via=default` —
  leading with a private seam a new user has never heard of and cannot usefully
  set, while never mentioning the VPN that was actually off. It now reads
  `'gateway' did not answer at <host> — connect your private network and try
  again`, with the transport seam demoted to a dim second line for the operators
  who run one.

## v1.20.1 — 2026-09-02

- **Gateway `add` now auto-detects whether a translating adapter is needed.**
  The add flow used to ask "Does this endpoint need a translating adapter?" — a
  question the user cannot answer without knowing the endpoint's API dialect. It
  now probes automatically: a minimal POST to `/v1/messages`; a 404/405/timeout
  means the endpoint does not speak Anthropic natively and `adapter=true` is set
  for you. Background context-window probing and adapter warm-up are also
  suppressed under test so the suite never hangs on them.

## v1.20.0 — 2026-09-02

- **Fixed: launch no longer looks hung on gateway/provider profiles.** The
  `/model` picker projection ran synchronously on every launch (a `/v1/models`
  fetch plus a projection, probed 2–5.6s) and blocked Claude Code from
  starting; it now runs in the background, so the launch returns immediately and
  the picker rows populate a second or two into the session.
- **Fixed: adding a second profile on an already-used gateway is no longer a
  ~60s wait.** The adapter classifies every model a gateway serves on first
  contact (probed 60s for 41 models). That cache was keyed per-profile, so a
  new profile on a gateway another profile had already classified re-probed the
  whole catalogue. The classification cache is now keyed by endpoint and shared
  across profiles (`~/.claude-profiles/.adapter-cache/`), and serialized with a
  lock so a background warmup and a launch never double-probe. Second profile on
  a known gateway: ~60s → ~1s.
- **Fixed: first-launch context-window probe scoped to the launched model.** It
  probed the whole gateway catalogue to learn one model's window; `--model`
  now scopes it, so a first launch is a single probe, not an N-model sweep.
- **Fixed: two write races the latency work exposed** — the model registry write
  is now atomic and lock-serialized (concurrent first-launches can't lose each
  other's probe), and the backgrounded picker re-reads `.claude.json`
  before its atomic replace so it can't clobber a value Claude Code wrote
  concurrently.
- **Progress is now visible during a full `ccmorph probe`.** Per-model
  results stream as each probe lands instead of appearing all at once at the end.

## v1.19.0 — 2026-09-01

- **Added: provider profiles for independent OAuth identities.** `type=provider`
  supports Codex, Gemini, Grok, Kimi, and any future cliproxyapi provider. Each
  profile owns an isolated auth directory and launches a per-session provider proxy
  on an ephemeral loopback port with a temporary inbound key — no user-visible URL,
  port, or credential entry. `ccmorph add` discovers available providers from
  cliproxyapi at runtime. `ccmorph login <name>` re-authenticates a provider
  profile.

- **Added: `ccmorph init` for one-time shell integration.** Idempotently wires
  the source line into `~/.zshrc`, matching the fzf/nvm industry pattern. Homebrew
  caveats updated to recommend `ccmorph init` instead of manual editing.

- **Changed: adapter auto-on for gateways.** New `apikey` profiles with a
  `base_url` automatically set `adapter=true`; the manual prompt is removed.

## v1.18.1 — 2026-08-31

- **Changed: `proxy` is folded into `apikey` for new profiles.** Both types used
  the same API-key and base-URL launch path, while the duplicate names made a
  gateway look like a different credential mechanism. `ccmorph add` now offers
  one API-key type for native Anthropic and gateways, asks directly whether a
  routed endpoint needs the adapter, and writes no speculative tool restrictions.
  Existing `type=proxy` files remain valid as a legacy alias.

## v1.18.0 — 2026-08-30

- **Fixed: a carried model falls back to the profile default when the target cannot
  serve it.** `ccmorph hop` and a cross-profile `--resume` both hand the session's
  model to the next launch, which sent a gateway id to a Claude account (a name
  Anthropic has never heard of) and a Claude id to a gateway that does not serve it.
  The model is now carried only where the target's own resolver knows it — Claude
  Code's built-in catalogue for an account with no `base_url`, the endpoint's
  `/v1/models` for one with a gateway — and a definite no is reported and dropped so
  the profile's own default takes over. An unreachable endpoint or a missing key
  stays a carry: ignorance is not absence.

## v1.17.2 — 2026-08-30

- **Changed: Claude profile compaction now consumes the fleet policy.** The 80%
  default no longer lives as a second constant in ccmorph. Unoverridden launches read
  `~/.agents/runtime/policies/context.json`; explicit profile and engine-valid settings
  overrides still win. An absent policy falls back to the built-in 80%, while a present
  malformed policy fails loudly.

## v1.17.1 — 2026-08-28

- **Fixed: a flag-shaped or multi-token value is refused where a model belongs.**
  `ccmorph set <account> model --menu` wrote the literal string `--menu` as the
  default model, and the next launch tried to run a model by that name. Effort has a
  closed enum to validate against; a model id has none, so the only guard was "no
  CR/LF". A model id is a single token that never begins with `-` or `@` — both are
  how a value would be misread downstream as a flag or a picker token — so those
  shapes are now refused, with the command that was actually meant. The guard runs
  before the picker, so `@menu` and `?` still reach it.

- **Fixed: a stale usage string in `set`.** The shared implementation still said
  `ccmorph model <name>` — the retired spelling — when the account argument was
  invalid. It now names `set`, and an unknown account lists the ones that exist.

## v1.17.0 — 2026-08-28

### Round three

Round three. Two more defects from `gpt-5.6-sol`, both in code round two had just
touched, and a guard for a mistake that broke this file three times in one sitting.

- **Fixed: a schema-invalid saved `autoCompactWindow` was treated as effective.**
  Round two clamped the env path but left the saved path accepting any positive
  integer. The correction differs between them, which is the point: the engine
  CLAMPS the env var (`Math.max(1e5, ...)`) but DISCARDS an out-of-range settings
  value, since the schema is `.int().min(1e5).max(1e6)`. So a saved 50,000 gave
  ccmorph a 17,000 trigger while the engine used 784,000, and a 400,000-token model
  died uncompacted with nothing naming it.

- **Fixed: `float()` is not `parseFloat()`.** The engine reads
  `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` with `parseFloat`, which consumes a leading
  numeric prefix — `parseFloat("90junk")` is 90. Python rejected the whole value, so
  ccmorph dropped a live override and computed 967,000 where the engine uses
  882,000, naming a safe 900,000 model fatal. Now matches, including whitespace, a
  sign, and exponent forms, with the engine's own `(0,100]` guard.

- **Changed: one budget resolver.** `_ccm_effective_acw` resolves env → saved →
  derived once; two open-coded copies are gone. That duplication is exactly why one
  copy received the schema clamp and the other did not. Flagged independently by two
  reviewers, and the fourth defect this review traced to a drifted copy.

- **Added: `test_no_embedded_program_carries_a_bare_apostrophe`.** An apostrophe in
  an English comment inside `python3 -c '...'` closes the single-quoted zsh string,
  the remainder parses as shell, and the enclosing function silently stops being
  defined — while `zsh -n` may still call the file valid, so the symptom surfaces far
  from the cause. That happened three times in one session, including once while
  fixing the items above. The test is verified to fail against a reintroduced
  apostrophe, and to allow the escaped form the file already uses in two places.

- Tests: 176 → 177.

### Round two

Round two closes. `opus-5` found the defect the other four missed, and it overturned
a verification that had already closed the file.

- **Fixed: the trigger used the wrong engine threshold.** `YRt` is the function that
  returns `"compact"`, and it thresholds on `xWe()` alone. ccmorph was adding
  `KRt`'s `0.2` precompute-buffer term — `KRt` is the separate precompute-ARMING
  threshold, behind its own gate. The two are identical at pct=80 and diverge
  everywhere else: 98,000 low at pct=90, 183,000 low at pct=100 or unset. At pct=90
  a model walled at 800,000 is fatal (engine compacts at 882,000) but ccmorph
  computed 784,000, judged it safe, and printed nothing.

  This is why 175 passing tests could not see it, and why another reviewer's
  numeric sweep over 20,000 randomized pairs confirmed the wrong formula as correct
  — it compared against the same wrong function. The new pct=90 test is verified to
  FAIL against the buffered form.

- **Fixed: a sub-100k `CLAUDE_CODE_AUTO_COMPACT_WINDOW` pin was used raw.** The
  engine floors it (`Math.max(1e5, effective)`), so a 50,000 pin still yields a
  100,000 window there — while ccmorph computed a 17,000 trigger and called a
  40,000-wall model safe. `_ccm_clamp_acw` now applies the same bounds to both env
  scans.

- **Changed: one definition of the settings the engine will see.** "Canon first,
  profile on top, last writer wins" was open-coded four times, and every copy that
  had not been updated produced a different wrong answer — an unresolved tier alias
  (no budget at all), a route ceiling that clamped the largest model, a hazard set
  that disagreed with the withhold. `_ccm_settings_sources` now defines it once and
  five readers consume it. The dead 4th parameter of `_ccm_switch_hazard_warn` is
  gone with it.

- Tests: 175 → 176.

### Round one

Round two of the five-model review. Every finding was reproduced before it was
fixed; three of these were defects introduced by the fixes in v1.15.5 (below).

- **Fixed: a conf-pinned ceiling skipped hazard analysis entirely.** The whole
  window-derivation block is guarded on ccmorph not finding
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS` already in the launch env, so a profile that
  pins it in its conf never reached the advisory or the withhold — while the engine
  still resolved a window from that pin and still refused to compact a model whose
  wall sat under the trigger. The hazard path now runs outside that branch and
  resolves the ceiling and the budget from `penv` and the saved settings when
  ccmorph did not derive them.

- **Fixed: the advisory and the withhold disagreed.** Round one corrected the
  advisory to the effective budget but left the withhold on the derived one, so a
  saved `autoCompactWindow` made one name a model the other never removed — or
  worse, made the withhold drop a model the advisory had cleared. Reported
  independently by three reviewers. Both call sites now take the effective value.

- **Fixed: tier aliases and the route ceiling read profile-only settings.** Both
  now merge canon then profile, the order the launch overlay itself uses. A tier
  mapping or an `availableModels` list declared in `~/.claude/settings.json` reaches
  the engine through that merge, so reading only the profile left an alias
  unresolved (no budget at all) and clamped the route's largest model to a smaller
  one's window.

- **Fixed: `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` parsing did not match the engine.**
  The engine takes it through `parseFloat` and ignores anything outside `(0, 100]`,
  keeping its 20% precompute reserve — it does not stop compacting. Treating an
  invalid value as "no trigger" made ccmorph fall silent on a session that would
  still have compacted. The percentage is now a cap applied only when valid.

- **Fixed: the hazard functions assumed zsh-native array indexing.** Under
  `KSH_ARRAYS` in a caller, `rows[1]` would read the first hazard as the trigger
  line. All three now carry `setopt localoptions noksharrays`.

- Note on v1.15.5's defect 4: hoisting the `local _pceil` declaration was a
  no-op, since zsh `local` is already function-scoped. That defect was in fact
  resolved by moving the hazard call out of the derivation branch — a different
  reviewer's finding. Recorded because the stated fix was not the real one.

- Tests: 173 → 175. The two new ones pin a conf-pinned ceiling with a saved budget,
  and the advisory/withhold agreement.

## v1.16.1 — 2026-08-28

Retired verbs are now rejected rather than silently still working (see the note at the
end of v1.16.0).

## v1.16.0 — 2026-08-28

**The command surface is rewritten. Backward compatibility was deliberately waived**,
so every retired spelling is gone rather than aliased — one spelling per concept was
the point. Designed against two independent proposals (Codex and Kimi K3) answering
the same brief; where they disagreed, the reasoning is recorded below.

```
ccmorph                        List accounts; the default is marked
ccmorph list                   The same list, spelled out
ccmorph use <account>          Make it the default for new sessions
ccmorph show [<account>]       What it launches as, and where each value comes from
ccmorph add | rm | mv          Manage accounts
ccmorph set <account> model  [<model>|?]
ccmorph set <account> effort [<level>|?]
ccmorph creds <account>        Print the credential
ccmorph probe [<account>]    Record context windows
```

- **`set <account> <key> [<value>|?]` replaces the `model` and `effort` verbs.** Two
  verbs for one mechanism — get-or-set a `settings.json` key — did not scale to a third
  preference, and `model` read as a noun where an action belonged.

- **`?` replaces `@menu`.** The old sentinel was an *instruction* occupying a *value's*
  slot: `set gw model kimi-k3` and `set gw model @menu` looked identical but were
  different kinds of thing. `?` is punctuation, already the terminal convention for
  "offer me the options", and it cannot collide — account names are `[A-Za-z0-9_]+`.
  A flag (`--menu`, `--select`) was rejected because it breaks at launch:
  `claude -a gateway --model --select` cannot be parsed without look-ahead, where a
  sentinel keeps arity fixed.

- **`show` is new, and answers the question nothing did:** what will this account
  actually launch as? Every line names the layer its value came from, and the last line
  is the command the launcher will run. It reports an unprobed window as unprobed
  rather than printing the resolver's 200,000 default as though it were a probe,
  and claims no window at all for native accounts, which Claude Code sizes itself.

- **`creds` replaces `key` and `token`** — one operation split by account type, which
  made the user learn the internal schema before choosing a command. The conf knows
  which secret it holds.

- **`use` replaces `default`, `mv` replaces `rename`.** `default` was a noun and a
  setter depending on argument count; reading now belongs to `show`. `mv` is what the
  operation is — a conf and a profile directory move.

- **`probe <account>` is positional**, no longer `--profile <account>` alone among
  every other command.

- **Argument mistakes are named, not just rejected.** `ccmorph set high` reported "no
  account 'high'"; it now says `high` is an effort level, shows the command that was
  meant, and lists the accounts that exist. Effort is a closed four-word enum, so the
  transposition can be identified rather than guessed at.

Retired from the taught surface: `model`, `effort`, `menu`, `pick`, `models`, `key`,
`token`, `default`, `rename`, and `@menu`. `list` is kept beside the bare form on
purpose: a word in `--help` and tab completion is findable where a bare invocation is
not.

Every one of those is REJECTED, not merely undocumented: an undocumented command that
still works teaches a spelling the next release removes. `model` and `effort` initially
survived as the fallthrough label `set` jumps into; that target is now spelled `_set_kv`,
which no account name can reach.

Two things are deliberately still accepted while going undocumented — `@menu`, so a
script written against it does not break silently, and the `-v`/`-V`/`version`
spellings, since a habit from another CLI meeting "unknown command" is friction with
nothing behind it. The internal `_menu`, `_models` and `_set_kv` labels are plumbing,
not surface.

## v1.15.5 — 2026-08-28

_Renumbered on merge: this section shipped as v1.16.0 on the
window/auto-compact line, which collided with an unrelated v1.16.0 on the
command-surface line. The content is unchanged._

Five independent cross-model reviewers (opus-5, gpt-5.6-sol, glm-5.2, kimi-k3,
qwen3.8-max) audited v1.15.0. Six defects, all reproduced before being fixed.

- **Fixed: the reported trigger was not the engine's.** The hazard math computed
  `window * pct / 100`. Claude Code deducts a 20,000-token output reserve BEFORE
  the percentage, then applies two further caps — a constant 16,000 overstatement
  at every window size. A 200,000 launch offering a 150,000 model called it fatal
  when the engine would in fact have compacted at 144,000.

- **Fixed: a fatal switch could go unreported.** The advisory used the DERIVED
  budget, but the overlay applies that with setdefault semantics, so a saved
  `autoCompactWindow` (or a conf-pinned `env.CLAUDE_CODE_AUTO_COMPACT_WINDOW`,
  which the engine reads before the settings layer) outranks it. With a saved 1M
  budget and a launch on a 272,000 model the real trigger is 800,000 — above that
  model's wall, the exact wedge this feature exists to prevent — yet the advisory
  computed 217,600, found no hazard, and printed nothing. The effective budget is
  now resolved in the engine's own precedence order: env, then saved, then derived.

- **Fixed: the advisory and the withhold acted on different model sets.** The
  advisory read `availableModels` from the profile's `settings.json` while the
  withhold read the merged overlay. A route whose models are declared in canon
  settings therefore had models removed from its picker with nothing naming them,
  breaking this code's own "named, never silently dropped" invariant. Both now read
  the same merged view.

- **Fixed: `_pceil` was scoped to the branch that computed it.** Declared inside
  the derivation block but read after it closed, so on a profile pinning
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS` in its conf — where that block is skipped — the
  `${_pceil:-}` at the call site masked the unset and `CCMORPH_WITHHOLD_FATAL=1`
  silently withheld nothing.

- **Fixed: `fable` was missing from the route-ceiling alias exclusion.** Three of
  the four tier aliases were excluded; a bare `fable` was treated as a model id.

- **Changed: one hazard computation instead of two.** All four reporting reviewers
  independently flagged the near-duplicate embedded Python in
  `_ccm_switch_hazard_warn` and `_ccm_withhold_fatal_switches`; the two copies had
  already drifted, which is what produced the arithmetic and `fable` defects. Both
  now consume `_ccm_hazard_set`, which computes the set once using the engine's own
  formula and returns it largest-wall-first.

- Tests: 172 → 173.

## v1.15.4 — 2026-08-28

- **Fixed: the listing showed a different model than the one that launches.** v1.15.1
  taught the launcher to read the account's default from `settings.json` instead of the
  conf's bootstrap `model=`, but the listing kept reading the conf — so `ccmorph list`
  said `gpt-5.6-sol` while `ccmorph model <account>` said `kimi-k3`, and the listing
  disagreed with the session it described. One reader, `_ccm_account_model`, now answers
  for the listing, the picker label and the launcher, so they cannot diverge again.

  The conf's `model=` stays reachable as the fallback: a routed account whose model
  lives only there must still launch with an explicit `--model`, because Claude Code
  cannot infer a non-Anthropic backend and would silently pick an Anthropic model the
  gateway does not serve. Order is what matters — a chosen default wins, and the conf
  answers only when nothing has been chosen. An account with neither now reads "no
  default model" rather than a bare `?`.

## v1.15.3 — 2026-08-28

Two places where ccmorph overwrote state no declaration made it the owner of, found
by a three-way adversarial audit (Codex, Fable 5, and a general reviewer) against one
rule: ccmorph may build on Claude Code's settings, never destructively hijack them.

- **Fixed: the 80% compaction default overruled a trigger the user had set.** The
  canonical trigger was injected after checking only the profile's conf. But the launch
  overlay merges canon + profile `settings.json` and *then* writes injected env over the
  result, so a `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` set in either settings file was
  silently replaced by `80`. Both layers are now consulted first, and a value found in
  either is left alone — a ccmorph default may fill a gap, never overrule a choice.

- **Fixed: statusline cleanup deleted bars ccmorph never installed.** The guard matched
  `command.endswith("/ccmorph/statusline")` — a path *suffix*, so a personal bar at
  `~/bin/ccmorph/statusline` was deleted while the function's own comment promised "a
  user's own bar is never touched". Ownership is now proven by exact path (both the
  state-dir and lib-dir spellings, since a pre-1.8 profile may hold either). The v1.7
  wire function this cleans up after wrote an exact path and refused to replace an
  existing command, so an exact match cannot miss anything ccmorph installed.

**Deliberately not changed**, recorded so it is not refiled as new: `set_allowed`
replaces `availableModels` wholesale, which is *declared* behaviour for routed profiles
(documented per-profile, with withdrawal semantics stated and pinned by test). The
audit's remaining concern is that the hand-curated regime ARCHITECTURE.md names is not
enforced in code — `qwen` sits inside the destructive path, protected today only by
coincidence. Latent, not live: no profile loses data. See ARCHITECTURE.md § 5b for the
trigger that would warrant the fix and its shape.

Also: `_ccm_project_mcp`'s wholesale `mcpServers` replacement was audited and **cleared**
— `CCMORPH_MANAGED_MCP` declares the managed file canonical, so erasing local drift is
centralised management working, not a violation.

## v1.15.2 — 2026-08-28

- **Fixed: `ccmorph effort` no longer deletes Claude Code's per-model effort.**
  v1.15.1 removed `modelSettings.<id>.effortLevel` entries so the level it wrote
  would always win. Those entries are native state, written by `/effort` inside a
  session — so the command destroyed a choice made natively in order to win a
  precedence argument, which is the opposite of what this tool is for.

  `settings.json` belongs to Claude Code. ccmorph is one entry point for editing
  it, alongside `/model` and `/effort`, and a convenience for setting a default
  before a session exists. It writes exactly the one key asked for and reflects
  everything else. The per-model-then-default hierarchy is Claude Code's to
  arbitrate, not ours to defeat.

  Profiles whose `modelSettings` v1.15.1 already cleared cannot have it restored —
  the entries are gone. Set them again with `/effort` in a session on that model, or
  leave them absent and the profile default applies.

## v1.15.1 — 2026-08-28

`ccmorph model` and `ccmorph effort` (v1.13.0) wrote the right settings key and
were overridden anyway. Both wrote to the profile's `settings.json` — the same place
`/model` and `/effort` persist a choice — and on the profiles the commands exist for,
something outranked that file on the next launch. Setting a model and effort, then
starting a fresh session, changed nothing.

The effort half of this release was wrong and is reverted in v1.15.2; the entry below
records what shipped.

- **Fixed: the conf's `model=` overrode the saved default model.** A
  `proxy`/`apikey` profile naming `model=` gets it injected as `--model` at launch,
  and a CLI argument beats every settings layer in Claude Code — so the launcher
  handed back the conf value every time and the saved one was never read. Injection
  itself has to stay: Claude Code cannot infer a routed backend and silently falls
  back to an Anthropic model without it. The conf value is a bootstrap — the one id
  needed to reach a gateway before anything is chosen — so the launcher now reads the
  saved default from `settings.json` and injects that instead. An explicit `--model`
  on the command line still wins over both.

- **Fixed: a stale per-model effort silently pinned the old level.** Claude Code
  resolves effort per model: it scans `modelSettings.<id>.effortLevel` for the
  launched model and consults the top-level `effortLevel` only when nothing matches.
  An in-session `/effort` writes a per-model entry, so one left behind outranked
  every later `ccmorph effort`. The command owns exactly one fact — the profile's
  default effort — so it now clears the per-model efforts that contradict it, rather
  than leaving a second source of truth. Only that key is removed: an entry carrying
  other per-model settings keeps them, and an entry left empty goes.

  Existing profiles carry these stale entries. Re-running `ccmorph effort <name>
  <level>` once clears them.

The shared lesson, recorded in ARCHITECTURE.md § 5b: writing the key Claude Code
reads is not the same as writing the layer Claude Code wins from. Before adding a
third such setting, check what outranks it — argv, and any per-model map.

## v1.15.0 — 2026-08-28

- **Changed: a model the session cannot compact is now named, not hidden.**
  v1.14.0 removed those models from the session's `/model` picker. That is the
  wrong default: it takes away a model the user chose on purpose — often the one
  they wanted as a reviewer — to prevent a failure only they can foresee.

  How narrow the exposure actually is: `autoCompactWindow` is one integer per
  session, but the engine uses `min(ceiling(model), scalar)` and recomputes
  `ceiling(model)` from the model in force on every call — so one scalar already
  produces per-model windows. A session capped at 1,000,000 gives
  `claude-opus-5[1m]` a 784,000 trigger and `claude-haiku-4-5` a 144,000 one,
  natively, with nothing from ccmorph. Claude models were never at risk.

  The gap is confined to the non-claude family, which shares the single
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS` ceiling, so `min()` cannot tell a 700,000-token
  model from a 1,048,576-token one. Nothing in the engine narrows that per model:
  `DM()` is hardcoded to `claude-sonnet-4-6`, `modelOverrides` maps only the four
  tier aliases, and the per-model window map is gated on a first-party connection.
  Lowering the scalar to the smallest model's wall would surrender 640k of headroom
  on a 1M-to-200k route to protect a model that may never be selected. Subagents
  are unaffected; each query carries its own window.

  So the launch advisory now names each risky model with the exact token count past
  which it stops compacting, **largest wall first** — the useful warning is the
  700k model that dies at 700k, not the 258k one nobody takes into a long session.
  Below the trigger every model is safe. `CCMORPH_WITHHOLD_FATAL=1` restores
  v1.14.0's removal behaviour (launch overlay only; the profile always keeps every
  model).

## v1.14.0 — 2026-08-28

The per-model window mechanism shipped in v1.11.0 did not actually reach most
sessions, and the one case it left open killed a real session.

- **Fixed: most launches carried no auto-compact budget at all.** The window
  lookup was fed a model name that had already been blanked for the `claude-*`
  family — correct for the env ceiling (Claude Code sizes those ids itself, and
  ignores `CLAUDE_CODE_MAX_CONTEXT_TOKENS` for them) but wrong for the budget,
  which every launch needs. So any session started on a `claude-*` id or a tier
  alias got **no `autoCompactWindow`**, fell back to `source:"auto"` against the
  route-wide ceiling, and put its trigger above the launched model's real wall.

  Probed on 16 live sessions before the fix: every compaction was `manual`,
  none `auto`, and the running processes' overlays showed `autoCompactWindow`
  absent. One session reached 921,054 tokens on a 922,000-token model — 946
  tokens of headroom — where `/compact` could no longer fit its own
  summarization request. It could neither proceed nor compact.

  The env-ceiling exclusion now narrows a copy, the `[1m]` tag is stripped before
  the registry lookup, and a tier alias resolves through the profile's own
  `ANTHROPIC_DEFAULT_<TIER>_MODEL` to the model that answers. All 35 models and 4
  aliases on a real route now resolve a probed window and their own trigger.

- **Fixed: a mid-session switch to a smaller model could kill the session.** The
  trigger is fixed at launch and nothing but the user's own `/autocompact` moves
  it in process, so a switch DOWN to a model whose wall sits under that trigger
  can never compact: it runs into the gateway's over-limit error, and by then the
  transcript is too large to summarize, so the session is unrecoverable rather
  than merely inexact. v1.13.0 only warned about this.

  Those models are now **withheld** from the session's `/model` picker
  (`_ccm_withhold_fatal_switches`), written into the launch overlay only — the
  profile keeps every model, and a session launched on a smaller one offers the
  whole route again. One advisory names what was held back and why. The launched
  model keeps its own budget; it is never dragged down to the route's floor.

- **Changed: no context window is written in the source.** Two `claude-*` window
  literals are gone; every window is a probed registry fact. The only remaining
  numbers are `_CCMORPH_ACW_MIN`/`_CCMORPH_ACW_MAX`, Claude Code's own schema
  bounds on the setting — a value outside them is dropped silently, which would
  put the session back on the default budget. A test fails the build if a window
  literal reappears.

- Tests: 168 → 171.


## v1.13.0 — 2026-08-27

- **Fixed: the launch advisories warned nobody.** Both of them — the
  switch-hazard line and the unprobed-model hint — were printed to stderr
  before `exec`. Claude Code takes several seconds to start and then draws its
  own alternate screen over anything written before it, so each line was visible
  for a fraction of a second and then gone. A correct warning on a surface no
  one can read is the same defect as a correct number on a status line the
  engine ignores.

  They now travel in the launch overlay as a `SessionStart` hook emitting a
  `systemMessage`, which renders inside the TUI and stays on screen — Claude
  Code's own unknown-model warning uses the same channel. The hook is a
  self-contained `printf` of a pre-built JSON literal: nothing to install on
  disk, nothing to read at fire time, and it is appended, so every hook the
  profile or canon settings already declare still runs. Advisories are queued
  during the launch and one hook carries them all.

  A test now fails if any launch advisory goes to stderr for a human to read.


## v1.12.0 — 2026-08-27

Publish readiness, from the outside in — what someone who cannot ask the author
anything needs in order to install and trust this.

- **Added: a Linux CI pipeline** alongside the GitHub workflow, so every suite
  runs before release rather than relying on one machine. Linux covers the Python
  floor; GitHub keeps the macOS runner for BSD-tool differences. Both prove the
  release tarball carries no live state.
- **Fixed: the README opened with an instruction nobody could follow** — a
  `<ccmorph-repo-url>` placeholder in the clone line. A test now fails if
  reader-facing prose contains a slot only the author could fill. Deliberately
  narrow: a packaging file's `REPLACE_WITH_…` is a release-time slot addressed
  to whoever cuts the release, and pasting a real URL into shipped files is
  itself forbidden (see below).
- **Added: verify-the-install step.** `ccmorph --version` first, with the fix
  when it fails, before any command that assumes the shell reloaded. Plus what
  `ccmorph add` actually asks for, which the README never said.
- **Fixed: the docs said "profile", the CLI says "account".** The README now
  states once that they are the same thing, and uses `<account>` wherever the
  commands do. `--profile` and `~/.claude-profiles` keep their names: they are
  identifiers, not prose.
- **Added: the formula's version is pinned by a test.** It sat at v1.7.0 through
  four releases; a formula pinned to an old tag installs an old tool while
  reporting the new one.

Note for anyone editing shipped files: `test_source_carries_no_host_specific_defaults`
forbids an org or host identifier in every tracked file, so the repository's own
URL cannot appear in them. That rule caught this change adding one, which is why
the clone line names no host.


## v1.11.0 — 2026-08-27

CLI conventions, from a reader hitting a genuine ambiguity in the help text.

- **Fixed: `<name>` meant two different things.** Every account argument is
  now spelled `<account>`, and a model argument `<model>`. Under a heading
  about models, `ccmorph menu <name>` read as though it should print a model
  name from nowhere — the argument is the account. A test fails if an account
  argument is ever spelled with the overloaded `<name>` again.
- **Fixed: `ccmorph menu` with no account errored** instead of chaining from
  the account chooser, so one concept behaved two ways depending on where it
  was typed (`claude -a @menu --model @menu` has always chained). It now asks
  which account, then which of that account's models.
- **Added: `-V` and `version`** alongside `-v` and `--version`. A habit from
  another CLI meeting "unknown command" is friction with nothing behind it.
- **Added: `--version` and `--help` are listed in the help text.** A tool that
  does not document its own flags makes the reader guess.

Checked and already correct, recorded so nobody re-audits: help goes to stdout,
errors to stderr, an unknown command exits 1, and `-h`/`--help`/`help` are all
accepted.

## v1.10.0 — 2026-08-27

- **Changed: the interactive chooser is `@menu`, and the subcommand is
  `ccmorph menu`.** `@pick` and `ccmorph pick` were two words for one
  concept, and `@pick` was a token this tool invented rather than any
  convention — so it was renamed rather than defended. `@menu` reads as an
  instruction in a slot that otherwise holds arbitrary model ids, and the `@`
  keeps it unmistakably not-an-id.

  `ccmorph pick` still works as an undocumented alias: a rename that breaks
  someone's script to tidy a vocabulary is not a tidy-up. A test pins both the
  alias and the migration, and fails if `@pick` reappears in the code or if the
  docs teach two spellings.

## v1.9.0 — 2026-08-27

- **Added: `ccmorph model <name> [<id>|@pick]` and `ccmorph effort <name>
  [<level>|@pick]`** — set (or show) an account's default model and effort from
  the command line, instead of launching a session to change them. `@pick`
  reuses the existing fzf choosers: models come from the endpoint's live
  listing (`ccmorph pick`), effort from Claude Code's fixed
  `low|medium|high|xhigh` enum, which needs no call. An effort outside that
  enum is refused rather than written, because the binary would drop it and
  leave the account silently on its previous level.

  Both write Claude Code's OWN settings keys (`model`, `effortLevel`) in the
  account's settings.json — the same place an in-session `/model` or `/effort`
  persists a choice — so the two paths are one setting and neither silently
  overrides the other. The conf's `model=` stays what it was: the backend model
  a routed profile cannot be inferred without, not a preference.

## v1.8.0 — 2026-08-27

Per-model context windows, delivered engine-side. Claude Code fixes its window
and auto-compact numbers at process start and re-reads nothing from disk
mid-session (verified live against 2.1.246), so ccmorph now sets both at the
only moment they can be set, from the model registry:

- **Added: the session's context ceiling covers the whole route.**
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS` is now the LARGEST probed window among the
  models the route serves (`_ccm_route_ceiling`), not the launched model's — a
  mid-session `/model` switch can no longer run a 1M model clamped at the
  launched model's window, and a profile whose default is a `claude-*` id no
  longer leaves every routed model at Claude Code's 200k
- **Changed: the launched model's budget rides the native `autoCompactWindow`
  setting** (injected into the launch overlay, clamped to the setting's
  100k–1M schema range) instead of the `CLAUDE_CODE_AUTO_COMPACT_WINDOW` env
  var. The binary consults that env var before the settings layer on every
  resolution, so the old pin silently disabled the user's own `/autocompact`
  command for the whole session. A value the user saved — `/autocompact`
  persists into the profile settings — always beats the derived one
- **Added: the saved `/model` default drives derivation.** When neither
  `--model` nor the conf names a model, Claude Code runs the choice saved in
  the profile's settings.json; the window is now sized from that same model
  instead of not at all
- **Removed: the v1.7.x statusline.** It was display-only — it repainted a
  number the engine never used, while compaction kept firing at the stale one.
  The script no longer ships; provisioning now removes a statusLine entry it
  previously wired (a user's own statusLine is never touched). What the engine
  budgets, Claude Code's own surfaces display natively

## v1.7.0 — 2026-08-25

Findings from a ten-component review against one question: what breaks when
someone who is not the author installs this on a machine that is not this one.

- **Fixed: a subscription profile could be created without a subscription.**
  `ccmorph add` verified the login by grepping `claude auth status` for
  `"loggedIn": true` and never asked which credential answered. An exported
  `ANTHROPIC_API_KEY` satisfies that check for a config dir that never completed a
  browser login — and ccmorph exports that variable per profile, so any shell it
  launched poisoned the next `add`. The email came back empty, blank passed the
  only validation there was, and a `type=sub` profile with no account was written
  and marked active, silently. The credential environment is now scrubbed before
  every auth call, `authMethod` must not be key-shaped, and a blank email rolls the
  profile back. `_ccm_read` gained the matching rule, so an already-written blank
  profile is caught on read with the command that fixes it
- **Fixed: `ccrelay` could not run on many Linux distributions.** It shipped
  `#!/bin/zsh`, a path that does not exist where zsh lives in `/usr/bin`. The
  installer was fixed for exactly this; `ccrelay` was missed, and the test that
  pins it only looked at the installer
- **Fixed: `bash install.sh` died with `A: unbound variable`** — bash reading a zsh
  parameter expansion, before the dependency checks that would have named the real
  requirement. It now re-execs under zsh, or says plainly that zsh is required
- **Added: the context size a gateway publishes is finally read.** Only
  `max_input_tokens` was consulted at launch, but OpenRouter and most
  OpenAI-compatible gateways publish `context_length`. Such a gateway was treated
  as publishing nothing: the window fell back to the stock 200k and the picker
  dropped every row for want of a ceiling, on a gateway stating the number for
  everything it serves. The invented-default guard still rejects a flat value
  whichever field carried it
- **Added: probing says what it costs before it spends.** Both the first-contact
  sweep and the probe pass now disclose the worst case up front, and
  `--check` no longer implies it is free — it probes like any run and only skips the
  write, which the help text now says. The over-limit probe keeps its 32000, because
  a smaller one is accepted by everything and records no cap; its acceptance is
  bounded with a stop sequence instead
- **Added: `SECURITY.md`** — where credentials live, what protects them, and the
  five things that do not
- Fixed: `ln -sh` is BSD-only, so the profile-symlink repair failed on Linux. `-n`
  is correct on both
- Fixed: `python3` was checked for existence but not version. The floor is 3.8, and
  below it an install succeeded and then failed days later inside a child process
- Changed: one `/v1/models` listing per launch instead of two identical fetches
- Changed: a guard test now scans every tracked file for host-specific and
  organisation-specific strings, rather than one file for two of them, so a
  machine-specific default cannot reach a release unnoticed
- Added: `make release` builds from `git archive` at a tag, never the working tree,
  and `make check-version` requires source, changelog and tag to agree. v1.5.1 was
  released without a tag and is now tagged

## v1.6.0 — 2026-08-25

Onboarding: the gaps a colleague's first hour actually hits.

- Added: the installer offers the optional dependencies it finds missing.
  `cliproxyapi` (the dialect translator `adapter=true` needs) and `fzf` were
  undocumented and unchecked, so a fresh install reached only the Claude-family
  half of a multi-vendor gateway and nobody was told why. Each is now named with
  what it unlocks and installed on confirm — never silently, and never at all
  without a tty
- Added: a re-run REPORTS that it is an update. It always preserved profiles, the
  probed registry and `local.sh`, but it closed by printing "fresh install",
  which reads as data loss to anyone who just updated over eight live profiles.
  It now names the version it moved you from and what it kept, and a test
  enforces the preservation rather than leaving it a property of the manifest
- Added: an unprobed routed profile is diagnosed at install time. A profile
  copied from a colleague launches fine but shows an EMPTY model picker, because
  rows are gated on a probed context window — indistinguishable from "ccmorph
  cannot see the gateway". The installer now names the cause and offers to run
  the probe
- Fixed: a declined optional dependency aborted the install. `set -e` turned the
  decline into a failure, breaking exactly the first-run case the offer exists to
  improve (caught by the new test, never shipped)
- Changed: the missing-translator error names `brew install cliproxyapi` instead
  of a path that does not exist on the machine reading it

## v1.5.2 — 2026-08-25

Findings from an adversarial review pass, and the gap behind a real data loss.

- Fixed: a failed state migration was silent. On a read-only state directory the
  rename fails, every reader then looks for a name that is not there, and the
  profile drops to a default context budget while compaction plans against that
  wrong number. It now warns once per shell, naming the consequence rather than
  just the failure
- Fixed: the shell-rc rewrite leaked temp files. `cp -p` and `mv` are unguarded
  under `set -e`, so a read-only rc directory aborted the installer between
  creating its temps and removing them, littering files beside your `.zshrc`
- Fixed: `#!/bin/zsh` is not a portable interpreter path — zsh lives at
  `/usr/bin/zsh` on several Linux distributions, where the installer failed with
  "command not found" for an interpreter the machine had installed
- Added a test that every machine-local state file is gitignored, and that no
  shipped component is. v1.4.0 renamed the probed-window registry off the
  `*.conf` suffix, which had been the rule keeping it out of git — so for nine
  minutes the file was ignored by nothing, and a worktree reconciler correctly
  read 52 probed windows as a stray edit and swept them aside. A state file's
  name and its ignore rule are one fact; nothing had checked they agreed

## v1.5.1 — 2026-08-25

- Added: `ccmorph key <name>` prints an apikey/proxy account's API key. Until
  now the only way to reach a key was `_env`, which also provisions the
  profile's Claude Code state (directory, shared symlinks, `.claude.json`) as a
  side effect — so a non-Claude consumer reading a key silently created a
  Claude profile. `key` is a pure read, the same shape as `token`

## v1.4.2 — 2026-08-24

- Fixed: upgrading no longer asks you to run anything. v1.4.0 renamed the
  probed-window registry and the release notes told users to `mv` it
  themselves — an upgrade step most people never perform, and missing it
  silently dropped every probed window back to a default. ccmorph now
  carries the file across itself, at load and in the installer: once, only when
  the old name exists and the new one does not, never following a symlink, and
  never overwriting a registry that is already there

## v1.4.0 — 2026-08-24

Only what a user of ccmorph needs now ships, and a data file stops
impersonating an account.

- Removed from the published repo: a private probe record that was never
  part of the tool, `probe-context-window.py` (orphaned — `ccmorph probe`
  calls `refresh-model-registry`, which probes on its own), and
  `.git-worktree-policy` (the author's lane-tooling config, read by nothing in
  this repository; now gitignored so local tooling keeps working)
- The `routed-model-onboarding` skill keeps its content and loses its
  indirection: it lived under `.agents/skills` with a tracked symlink from
  `.claude/skills`, and is now simply `.claude/skills/<name>/SKILL.md` where
  Claude Code natively finds a project skill. The 63-line test that policed
  that symlink's target and mode went with it — its own comment conceded it
  was "repository-wiring governance, not a deployed runtime invariant"
- Renamed `model-registry.conf` to `model-registry`. In this directory `*.conf`
  means a profile holding a credential — it is what the profile glob
  enumerates and what `.gitignore` excludes — so the probed-window table was
  parsed as an account and every command printed
  `skipping invalid profile 'model-registry'`. Harmless, but it told a new user
  their setup was broken when it was not. `CCMORPH_MODEL_REGISTRY` still
  repoints it, so a host that set that is unaffected; every other host is
  migrated automatically, at load and by the installer — nothing to run

## v1.3.2 — 2026-08-24

- Flag diagnostics now name the spelling you typed: `--profile requires an
  account name` instead of reporting `-a` for every form

## v1.3.1 — 2026-08-24

- Documented that `-a <name>` also spells as `--profile <name>` /
  `--profile=<name>` (README + `ccmorph help`)
- Documented that `ccrelay` runs from **inside** a Claude Code session — at
  the prompt, `! ccrelay <name>` — and what each refusal means outside one

## v1.3.0 — 2026-08-24

Nobody has to know `ccmorph probe` exists to benefit from it.

- Adding a routed profile (apikey with a base_url, or proxy) harvests the
  gateway's listing-stated context sizes on the spot — free — and reports;
  a route that states none gets one line naming the command and its cost
- A launch whose routed model still has no known window prints one dim
  line: what happened (running at Claude Code's default budget) and the
  fix — present exactly while true, gone once probed
- The resolver's own unprobed warning names `ccmorph probe` instead
  of the internal tool name
- Fixed: the `ccmorph models` ceiling column missed the v1.2.0 native
  registry default and stayed off unless the env override was set

## v1.2.0 — 2026-08-24

Probed context windows are now built in — `ccmorph probe`.

- `model-window`, the canonical window/send_id resolver, ships beside
  ccmorph.sh: launches and the picker use it natively (a `claude-*` model
  probed at 1M is sent as its `[1m]` variant), no host wiring required
- `ccmorph probe` records routed models' windows into the registry
  (`model-registry` beside ccmorph.sh, machine-local, never shipped; it
  shipped as `model-registry.conf` until the name collided with the profile
  glob — see the entry below).
  Sizes a gateway states in its /v1/models listing are recorded free and
  never probed; only opaque gateways are probed, only when you run it —
  rejection costs nothing, and the one call that can spend tokens (the
  confirming request after an accepted overshoot) is documented in --help
- `CCMORPH_MODEL_REGISTRY` now only repoints the registry file;
  `CCMORPH_MODEL_WINDOW` is gone — the resolver has exactly one
  implementation, and removing the shipped file is the only off-switch

## v1.1.0 — 2026-08-24

The shared harness layer is now guaranteed on fresh machines.

- Shared dirs (`skills`, `plugins`, `commands`, `hooks`, `output-styles`,
  `session-env`, `file-history`, `projects`) are created in `~/.claude` and
  linked at provisioning even while empty — content installed later reaches
  every profile
- Shared files (`CLAUDE.md`, `keybindings.json`, `settings.local.json`)
  appearing after a profile existed are linked by the next launch's setup pass
- A profile-local real dir is migrated into the shared store (never
  overwriting a file) and linked; a profile-local real file is respected as a
  deliberate override
- One manifest drives provisioning and the launch health check, so the two
  cannot drift

## v1.0.0 — 2026-08-23

Initial release.

- Profiles: `sub`, `token`, `apikey`, `proxy` — one credential and config
  directory per backend, switchable per session (`claude -a <name>`) or
  mid-session (`ccrelay`)
- Model choosers (`@pick`), per-endpoint model lists, and probed
  context-window budgets so compaction fires before an endpoint refuses
- One-command install and uninstall: a single managed `.zshrc` block,
  re-runnable, never double-edited
- Optional seams (all off by default, declared in `local.sh`): managed MCP
  projection, model registry/window resolvers, and a via-transport runner for
  endpoints only another process tree can dial
