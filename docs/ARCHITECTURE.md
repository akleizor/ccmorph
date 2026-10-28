# ccmorph — architecture

**The one question this document answers:** how does a Claude Code session get
pointed at a chosen account or backend, and what does each profile control?

Evidence tags used below: **[wire]** observed in a captured request or response,
**[src]** read from vendor source, **[test]** pinned by this repo's suite.
Anything unmarked is a design statement, not a probe.

Most `[src]` citations are against **cliproxyapi 7.2.110** (upgraded from 7.2.70 on
2026-08-02). The installed version is now **8.0.45**; the translator-config workarounds
were re-verified on it (table in section 4), the rest were not.
7.2.113 differs inside the same effort pipeline — `internal/thinking/validate.go`
adds clamping of the generated fallback level and `apply.go` adds cross-format
`xhigh`/`max` remapping — so an upgrade can change effort behaviour even though
the `reasoningEffort` block itself is byte-identical. Re-verify after upgrading;
do not carry these citations forward unchecked.

The repo suite: `tests/ccmorph_test.zsh` (112 zsh tests),
`tests/install_test.zsh` (installer against a throwaway `$HOME`),
`tests/adapter_test.py`, `tests/gateway_shim_test.py` (black-box, against the
built binaries) and the Go unit tests in each package. Run them all with
`make shim && go test ./... && zsh tests/ccmorph_test.zsh && zsh tests/install_test.zsh && python3 -m pytest tests/`.

## Scope and ownership

ccmorph owns profile selection, its profile configuration, automatic backend
compatibility, and the child processes it starts. It integrates with other tools
through their supported commands and configuration interfaces; integration does
not transfer ownership of those tools.

- Anthropic owns the `claude` executable and its command vocabulary. ccmorph
  never ships that name, renames it, installs over it, or registers a shell alias
  or function in its place. Users may create personal aliases themselves.
- Shell startup files, PATH ordering, global package wiring, unrelated services,
  and another product's configuration remain with their owners. Installation,
  upgrade, uninstall, and migration preserve them; an obsolete integration is
  reported rather than silently repaired outside ccmorph's scope.
- ccmorph owns each profile's generated `settings.json` (canonical settings plus
  the profile's own `settings.profile.json` layer, see
  [Launch contract](#launch-contract-arguments-pass-through)). Edits Claude Code or the
  user make to that file are carried into the layer, never discarded; the layer and
  `~/.claude/settings.json` are edited only for the requested supported fields, and
  conversation history is preserved.
  One narrow exception repairs Claude plugin index paths invalidated by ccmorph's
  own shared-plugin topology; package intent, installation, wiring, cache contents,
  and marketplace source identity remain with apm and Claude.
  Permission bypass remains an explicit host opt-in.
- A convenience feature must fit this boundary in every delivery path: source
  installer, launcher, recovery, tests, and documentation.
  Shorter typing never justifies taking over a neighboring tool.

This project-wide decision supersedes the temporary managed `claude` wrapper.
Installer preservation tests and the public command manifest enforce the boundary.

## Code layout

`lib/ccmorph.sh` is the runtime's only entry point: the `ccmorph` command, the helpers
and agent dispatch (`source lib/ccmorph.sh && _ccm_launch -a …`) load the library through
it and nothing else. (Two callers fall outside the runtime and are the only exception:
`install.sh` sources `lib/ccmorph-ui.zsh` directly for its colour helpers, and the picker
tests source that module as a library in its own right.) It resolves the state and code
paths, sets the globals `local.sh` may read, sources `local.sh` and `ccmorph-ui.zsh`,
then sources each module below, in this order. Every file named here must sit beside
`ccmorph.sh` in `lib/`; a module that fails to load fails the whole source.
Each module has one purpose; a function lives in exactly one.

The repository and the installed copy under `~/.config/ccmorph` share one layout:
`bin/ccmorph` (the command), `lib/` (`ccmorph.sh` and the modules), `libexec/`
(internal executables: `ccmorph-core` and `gateway-shim`, both Go and built by
`make shim`; `ccmorph-core <adapter|settings-compose|model-window|refresh-model-registry>`
is the one binary every launch, install and zsh caller runs, from `cmd/ccmorph-core/`;
`gateway-shim` is the standalone stock shim, `cmd/gateway-shim/`, for a profile that runs it directly.
No Python runs in the core: `python3` is needed only by the Remote Control add-on in `addons/remote-control/`) and `share/` (data:
`model-catalogue.json`, `window-sources.json`). `lib/` finds `libexec/` and `share/`
beside itself. `CCMORPH_LIB_DIR` names the `lib/` directory.

| Module | Purpose |
|---|---|
| `ccmorph-base.zsh` | Shared primitives: portable file metadata, logging, executable resolution, the environment-name lists, Claude Code's own CLI surface and value checks. |
| `ccmorph-models.zsh` | Models: the session's model and state on resume, gateway /v1/models listings, published windows, route ceilings and model lookup. |
| `ccmorph-context.zsh` | Context window and settings: auto-compact trigger maths, window hazards and advisories, settings layering and the projection into CLAUDE_CONFIG_DIR. |
| `ccmorph-store.zsh` | Profile state files: stale-file cleanup, state and profile-home migration, directory security, atomic writes, profile reads and validation. |
| `ccmorph-profiles.zsh` | Profile homes and choices: the shared harness layer linked into each profile home, profile and value pickers, sign-in state, and committing a new profile. |
| `ccmorph-cli.zsh` | The `ccmorph` command: the config store, doctor, help, usage and the verb dispatcher. |
| `ccmorph-providers.zsh` | The dialect adapter and the provider proxy (cliproxyapi): start, stop, login and listing. |
| `ccmorph-launch.zsh` | Launching Claude Code: the project picker, the forked claude child, _ccm_launch and the relay driver. |

## 1. Profile model

A profile's editable configuration is `<name>.conf`, holding `key=value` lines.
It includes manual `context_window.<model>` overrides and, in the same file, a
`context_discovered` JSON blob (`{route: <fingerprint>, models: {<id>: <tokens>}}`)
holding limits `ccmorph probe` discovered for the profile's current route.
There is no separate shared registry file: `rm` and `mv` move or delete the whole
`.conf`, discovered data included, with no registry-prefix bookkeeping. `add`
refuses orphaned private state left by a manually deleted configuration. Shared
conversation history survives profile removal. All `*.conf` are gitignored: they
carry credentials and never enter history.

**The key set is a contract, and the layout is one too.** Which keys a profile may
carry is decided in `_ccm_read` (ccmorph-store.zsh) by type — one shared list, one list
per type, the `env.` allowlist, and the removed keys that report themselves and are
ignored. Unknown keys and wrong-type keys refuse the profile. `profile.conf.example`
is the same contract in human form: what it names must load, and every accepted key
must appear in it, both pinned by `test_the_profile_template_is_a_valid_conf_naming_every_key`.
Ordering is separate and has one owner: `CONF_SECTIONS` with `canonical_conf()` in
model-window. Every writer goes through it — `replace_fields` for `context_discovered`
and `context_window.<model>`, and `_ccm_config_store` for model, effort, mappings and
a first configure — so a file rewritten by any of them comes out in the same five
sections (connection, model choice, behaviour, context, machine-written state),
regardless of how it was hand-edited. Comments travel with the key line below them,
and a line the layout does not know keeps its place after the known ones.

**Migration off the legacy shared registry runs automatically.** `model-window
--migrate` looks for a legacy `model-registry` file and per-profile
`cache/context-window` files; `install.sh` invokes it on every install/upgrade,
and `ccmorph-store.zsh`'s `_ccm_migrate_state` invokes it again from the CLI path the
first time it sees leftover legacy state (so a host that skips reinstalling still
gets migrated on next use). A row scoped to a profile (`<profile>:<model>`)
migrates straight into that profile's `context_discovered`. An unscoped legacy
row has no route provenance of its own — it is attributed to a profile only when
that profile's own `cache/context-window` corroborates it; otherwise it is left
in the archived legacy file rather than guessed into a profile it might not
belong to. Migrated source files are never deleted, only renamed to `.migrated`
(numbered on collision), so a mis-migration is always recoverable by hand.

### One state root, and the overrides that must reach all of it

The state root is `$CCMORPH_STATE_DIR`, else `$XDG_CONFIG_HOME/ccmorph`, else
`~/.config/ccmorph`. Profile runtime state is `profiles/<name>` **under that
root**, not a sibling of `~/.claude`. Both overrides therefore mean what they
say: every reader — launcher, adapter, and context migration — derives the
profile directory from the same root. Overrides select a location; changing one
after migration does not move an existing state tree. A hardcoded
`~/.claude-profiles` silently ignored both, which is
the same class of bug the state/code split in `ccmorph.sh`'s header exists to
prevent: a host that set `XDG_CONFIG_HOME` got its confs in one place and its
profile homes in another, and the two disagreed about which machine they were on.

**The rename runs itself, once, and is conservative in every direction.**
`_ccm_migrate_profile_home` runs inside `_ccm_secure_dirs`, so it is on both the
install path and the ordinary CLI path — a host that never reinstalls migrates on
next use. It holds an exclusive `flock` on a lock file in the state root, so two
concurrent launches cannot both move the same directory. Its rules:

- **Rename, never copy.** `os.rename` moves the inode, so credentials are not
  duplicated on disk, modes survive without a chmod pass, and inner symlinks
  (each profile's links into the shared harness) are preserved rather
  than dereferenced. A cross-device rename fails loudly instead of degrading into
  a copy.
- **The legacy path becomes a compatibility symlink** to the new location. A
  Claude Code session already running holds `CLAUDE_CONFIG_DIR` as an absolute
  path; without the link, a live session's writes would land in a directory that
  no longer exists. External tools reading the old path keep working too. If the
  symlink cannot be created, the rename is rolled back — a moved directory with
  no way back to its old name is worse than not moving.
- **Collisions refuse.** Two real directories, a non-directory at either end, or
  a legacy symlink pointing somewhere other than the target: nothing is moved,
  merged, or overwritten. Merging two profile homes would silently mix identities.
- **Already-migrated and fresh installs are no-ops**, checked before spawning an
  interpreter or taking the lock: a legacy symlink already resolving to the target
  returns immediately, and a host with no legacy path never gains one.
- **`ccmorph doctor` stays read-only**, so it does not migrate. It falls back to
  reading the legacy path when the new one does not yet exist, so diagnostics on a
  pre-migration host still describe the profiles that are actually there.

Unaffected: native `~/.claude` and the shared harness layer symlinked into it.
Each profile's secure storage follows its own `CLAUDE_CONFIG_DIR` (§2).
Uninstall retains profile state, as before.

Context migration (`model-window`) reads its per-profile `cache/context-window`
files from the new root, and falls back to the legacy root only when the new one
does not exist and no explicit `CCMORPH_PROFILE_DIRS` was given — legacy cache
recovery must still work on a host whose profile move has not happened yet.

| Type | What it sets | Backend |
|---|---|---|
| `sub` | OAuth subscription state | Anthropic |
| `token` | long-lived token | Anthropic |
| `apikey` | credential presented to the selected endpoint, optional `base_url` | Anthropic or any API-key gateway |

`proxy` remains a parser-compatible legacy alias, not a distinct architecture.
New profiles use `apikey`. `base_url` selects the endpoint; custom endpoint
translation is automatic at launch (`--auto`) and is not a profile knob.
Model and effort settings select behavior; none of those facts changes the
credential type.

Recognised keys: `type`, `email`, `token`, `base_url`, `api_key`, `model`,
`effort`, `effort.<model>`, `model_defaults`, `model_map` (apikey only), `adapter`,
`context_window.<model>`, and `env.<NAME>` for the model-behaviour variables listed in
`_CCMORPH_PROFILE_ENV`.

The live subagent-model knob is `CLAUDE_CODE_SUBAGENT_MODEL`, owned by the
profile `.conf` as `env.` and passed in the launch environment (forces every
Task subagent to that model; `inherit` disables). Both gateway profiles set it
so bulk subagent fan-out does not run on the session's own model.

### The env lists and the sweep

Three lists in `ccmorph-base.zsh` decide what a launch clears from its environment:

- `_CCMORPH_PROFILE_ENV` is the set of keys a conf may declare as `env.<NAME>`.
- `_CCMORPH_TUNING_ENV` (`CLAUDE_CODE_SIMPLE`, `MAX_THINKING_TOKENS`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`) is a subset of it: client tuning that
  does not pick a backend or account. The shell's value **passes through** unless the
  profile declares the key. The launcher records the tuning keys it set in
  `CCMORPH_SET_ENV` (names only); a nested launch clears those, since they are the
  outer session's values, not the shell's. `CLAUDE_CODE_SIMPLE` is also cleared inside
  a Claude Code child session (`CLAUDE_CODE_CHILD_SESSION=1`; `CLAUDECODE=1` alone is set in every shell Claude Code spawns and does not count), because `claude --bare` sets it itself and
  no launch records it.
- `_CCMORPH_BACKEND_ENV` (provider switches, base URL, API keys, OAuth tokens) is
  **always cleared** and is not declarable by a profile; the launcher sets its own
  credential and endpoint. `ccmorph _backend_env` prints it, answered before any state
  work, so the Remote Control add-on builds its strip list from this one list.

Every other key on `_CCMORPH_PROFILE_ENV`, plus `_CCMORPH_LAUNCH_ENV` (keys a launch
sets for itself), is cleared unconditionally. The permit set and the clear set once
diverged: `MAX_CONTEXT_TOKENS`, `AUTO_COMPACT_WINDOW` and `AUTOCOMPACT_PCT_OVERRIDE`
were permitted but never cleared, so exporting one in a shell silently changed
compaction for every profile that omitted it.

Consequence: never remove a key from `_CCMORPH_PROFILE_ENV` to "clean up" an unused
setting. Removing it stops the value being cleared, which reintroduces the leak. A key
joins the tuning list only if a shell value is harmless to every backend.

`test_the_launch_sweep_clears_every_allowlisted_env_key` pins that behaviourally:
it exports every allowlisted key, enters the launch path against a stub `claude` that
reports what survived, and requires that no key survives except the tuning keys, which
must pass through. Asserting on the source
text instead proved nothing — an earlier version did, was defeated by a comment block
growing under its `grep -A6`, and would equally have passed with `unset` replaced by
`:`.

## 2. What is per-profile, and what is deliberately global

Profile files and launch environments select model credentials and backends.
MCP credentials (through capsules), settings, history, and tooling are **global on purpose**.

| Concern | Scope | Mechanism |
|---|---|---|
| Profile configuration and model credential injection | per profile | `~/.config/ccmorph/<name>.conf` + `~/.config/ccmorph/profiles/<name>/` |
| claude.ai sign-in and native secure storage | per profile | no launch sets `CLAUDE_SECURESTORAGE_CONFIG_DIR` except a private launch, which sets it to the real profile dir so the key is unchanged; otherwise Claude Code keys the keychain entry by the profile's `CLAUDE_CONFIG_DIR`; an interactive login stays in its profile and no other profile can see it. |
| MCP credentials | global | owned by MCP capsules (`~/.agents/mcp/<name>/state/`); every profile reaches the same local service, so one grant serves every profile and every agent. A remote OAuth MCP joins as a capsule, never through Claude Code's per-profile store. |
| Session history | per profile | each profile keeps its own `projects/`, as a plain `CLAUDE_CONFIG_DIR` does. A hop, or `--resume <id>` naming a session from another profile or from native `~/.claude/projects/`, links that one transcript and its sidecar dir into the target (`_ccm_carry_session`); Claude Code appends through the link, so the history stays one file. A profile from the shared-history layout has its `projects` link removed (never the target) at the next launch. |
| Adapter probe cache | **per endpoint**, shared across profiles | `.adapter-cache/<sha256(base_url)[:16]>.json`, beside the profile dirs — keyed by endpoint so a second profile on an already-classified gateway starts instantly; the stored `endpoint` is re-checked on read, so a moved URL or hash collision re-probes instead of mis-serving |
| Adapter runtime config, auth, and log | per launch | mode-0700 directory under `${TMPDIR:-/tmp}`; removed on exit |
| `settings.json` | **generated** per profile | canonical `~/.claude/settings.json` + the profile's `settings.profile.json` layer + saved model defaults, written by `settings-compose` at launch; edits made to it are folded back into the layer — see [Launch contract](#launch-contract-arguments-pass-through) |
| Shared harness layer — `skills`, `plugins`, `commands`, `hooks`, `output-styles`, `agents`, `session-env`, `file-history`, `CLAUDE.md`, `keybindings.json`, `settings.local.json` | global | one manifest (`_CCMORPH_SHARED_DIRS`/`_FILES`), symlinked into `~/.claude`. Dirs are created-and-linked even while empty, so content installed later reaches every profile; a file appearing in `~/.claude` after a profile existed is linked by the next launch's setup pass. A profile-local REAL dir is merged into the shared store and linked only for the mergeable stores — `agents`, `session-env`, `file-history` — never overwriting an existing entry. Links move as links, not followed (a symlinked dir is one link): an absolute link as is, a relative one rewritten to an absolute path — into the shared store when its target is inside the migrated tree, else to the target's physical path — and its backup entry is removed only once the new link exists; any other one is refused and moved nowhere, to be reconciled through its owning tool instead. A profile-local real file is a deliberate override, respected. Claude may persist the profile spelling used during installation in its plugin indexes; `_ccm_normalize_plugin_index_paths` maps only those stale `installLocation`/`installPath` prefixes back to an existing canonical `~/.claude/plugins` target. It never changes source URLs, package metadata, or missing targets. |
| Translator instance | per credential domain | one adapter per credential domain when translation is required; native endpoints use none |
| MCP servers | per profile | each profile keeps the `mcpServers` it was seeded with at provision (or added via `claude mcp add`); nothing projects an external set onto it at launch. |

### Launch contract: arguments pass through

`ccmorph run <profile> [arguments]` hands Claude Code the caller's arguments
exactly as given. Nothing is inserted, removed or rewritten, so no subcommand —
listed, hidden or released tomorrow — meets a flag ccmorph invented, and ccmorph
never needs Claude Code's command grammar. (The additions are a hop's resume,
which is ccmorph's own relaunch.) Launch state reaches Claude Code through the
two channels it reads at
every entry point — a session, any subcommand, the sessions Remote Control spawns:

| State | Channel | Why |
|---|---|---|
| Profile | `CLAUDE_CONFIG_DIR=profiles/<name>` | Claude Code's only user settings root |
| Endpoint, credential, launch model, per-session compaction budget, advisory text | process environment (`ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL`, `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, `CCMORPH_ADVISE`) | per launch; nothing on disk, parallel sessions never collide |
| Canonical settings, profile layer, saved model and effort defaults, the default model's `autoCompactWindow`, a static advisory `SessionStart` hook | generated `profiles/<name>/settings.json` | per profile; read natively everywhere |

Verified **[wire]** against Claude Code 2.1.283 with a recording gateway:

- `ANTHROPIC_MODEL` beats the `model` setting, a caller's `--model` beats the
  variable, and the variable re-forces a routed model on `--resume` (without it the
  resumed session fell back to Claude Code's own default).
- `CLAUDE_CODE_EFFORT_LEVEL` beats `effortLevel`; the binary states that
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW` takes precedence over `autoCompactWindow`.
- A settings file's `env` block **beats the process environment**, so the generated
  file never carries a variable the launch sets itself.
- With `CLAUDE_CONFIG_DIR` set, `~/.claude/settings.json` is not read at all — hence
  the generated file.
- `--settings` before `remote-control` is refused outright ("not carried over to the
  sessions Remote Control starts") — the reason argv injection was abandoned.

**Generation and edit tracking** (`settings-compose`). Under a lock, each launch
first folds pending edits — the difference between `settings.json` and the copy last
written (`cache/settings.base.json`) — into `settings.profile.json`, before any reader
consults the layer; removing a key that came from canonical settings is recorded as
a removal. It then writes `settings.json` = canonical + layer + saved defaults, only
when that changes, keeping the previous version in `backups/`. Keys ccmorph renders
from the conf (`model`, `effortLevel`, per-model effort, tier mappings,
`availableModels`, the advisory hook) are never folded, so an in-session `/model`
choice stays session-local as before. A missing `settings.json` is regenerated, never
read as removals. On the first launch after upgrade the old `settings.json` — which
was the profile layer — becomes `settings.profile.json`. A projection that cannot
write (a sandbox denying new files) warns and the session starts on the previous
generated file; per-launch state still arrives through the environment. It refuses
(exit 3, and the launch refuses with it) when that file does not already carry the
profile's linked policy — one that appeared since it was generated, or one that
cannot be read — because such a fallback would run the session without the sandbox
the policy configures.

**Compaction budget.** The profile default model's derived budget is written as the
native `autoCompactWindow`, so the user's `/autocompact` keeps working. Any other
launched model (explicit, picked, resumed) is per-session: parallel sessions share
one `settings.json`, so its budget rides `CLAUDE_CODE_AUTO_COMPACT_WINDOW` unless the
user saved a budget, which always wins.

**A caller's `--model` is never rewritten.** ccmorph used to swap an explicit
`--model claude-…` for its `[1m]` id on a 1M route; it now leaves the flag as typed
and names the `[1m]` id on stderr. Models ccmorph chooses itself (the default, the
picker, a resume) still launch with the derived id.

### Interactive selection is one flag, resolved per verb

`ccmorph run -i` resolves one saved profile through a credential-free `fzf`
list, then enters the same launch pipeline as an explicit profile name. The
sentinel is never stored or passed to Claude Code, and selection never changes
the saved default. With no `--model` given, `-i` resolves the profile first
because the selected profile owns the endpoint and credential used for model
discovery. Model discovery uses the same direct-first route resolution as launch,
so profile and model selection do not diverge across process boundaries.

The same flag applies to `set`, `get`, `context`, `inspect`, `use`, `login`,
`cred`, `config`, `models`, `doctor`, `probe`, and `hop`. Existing default/report
semantics stay unchanged when the flag is absent; an omitted model/effort value
opens its picker. `set` and `get` also offer their setting names when omitted.
`rm` and `mv` remain explicit-target operations, never inferred from a menu.

All ccmorph-owned prompts use the standalone `ccmorph-ui.zsh` module.
`ccm_ui_select native|search` accepts tab-separated ID/label/description rows;
`ccm_ui_input` owns text and hidden credential entry; `ccm_ui_confirm` uses the
native selector with No selected by default. `ccm_ui_list` renders instructions
for existing piped-input flows. The module has no profile or persistence logic.

Native selection uses one Claude Code-style layout for sign-in, settings,
reasoning, and confirmation: orange headings, lavender chevrons, gray descriptions,
and blank-line spacing. Layout has one left edge: headings, labels, hints and
numbered indexes start after a two-cell margin, and only the `❯` pointer hangs in
it. Descriptions start a fixed four-cell gap after the widest label. Headings,
rows, hints and the one-line `Title › choice` receipt are fitted to the terminal
width in cells, measured in UTF-8 even under a byte locale; the choice keeps
priority over the title. Output that is not a terminal is never truncated.
Help and `inspect` share one two-column grid. The palette comes from the installed Claude Code 2.1.278
dark-theme roles; native ANSI and fzf colors derive from the same values. Profile and all model selectors use the
single `fzf` adapter, including Claude aliases and unavailable-list keep/custom
choices. Both adapters share label rendering, column widths, and theme tokens.
Only the module handles key reads, terminal modes, cursor movement, and fzf argv.
The source installer ships it beside `lib/ccmorph.sh`.

`TERM=dumb` and search without fzf use the module's numbered renderer. `NO_COLOR`
removes styling. Esc/EOF cancels selection; confirmations never default to Yes.
Menus go to stderr, selected IDs alone to stdout; text and secret input return in
`REPLY`. Hidden input disables echo before displaying its prompt. Explicit
arguments and the existing piped configuration sequence retain their behavior.

`add` offers profile types and discovered providers, then only the corresponding
sign-in fields. `config` separates model/reasoning from connection editing.
Model rows come from `_ccm_value_rows`: API-key endpoints advertise their own
models; subscription/token profiles use Claude aliases; provider profiles reuse
saved rows. Model discovery failure still permits keeping the current default or
entering a custom ID. No versioned model catalogue is embedded in the UI.
Cancellation writes no pending configuration. Claude Code's native `/model`
menu remains vendor-owned; ccmorph supplies supported configuration only.

### settings.json is layered, not global

The launch line passes `~/.claude/settings.json`, but every profile also has a real
`settings.json` at its root, and **its keys win**. Probed: all nine shadow `hooks`;
`codex` shadows fourteen keys including `permissions` and `theme` **[wire]**.

This is not drift to be cleaned up. Claude Code writes those files itself — theme,
onboarding state, the model last chosen — so a populated per-profile file is expected,
not the failure; an empty `hooks` block is what a host's own verifier (if any) should
flag. `ccmorph` removes a profile-local `settings.json` only when it is a *symlink*,
which is the mechanism that keeps a real file authoritative.

Two consequences worth holding:

- A global-only change does not reach a profile that shadows that key. Edit the
  global file for a new key, and expect to touch each profile for a shadowed one.
- A profile directory left behind by a deleted conf keeps its `settings.json`, and
  doctor then reports it as a broken settings home. One stale throwaway accounted
  for 25 of 26 failures until it was removed, so profile directories need
  reconciling against confs — nothing does that automatically.

### Host opt-ins live in `local.sh`, never in shipped source

`ccmorph.sh` sources `~/.config/ccmorph/local.sh` at load if it exists. That file
is gitignored and never ships — it is the single seam where a host's own tooling
opts into optional behavior without the product carrying any knowledge of that
tooling. Host opt-ins via `local.sh`: `CCMORPH_PRELAUNCH`, naming an executable
to run before launch. Legacy context migration reads `model-registry` or
`model-registry.conf` under the state root. Profile caches live under that same
root's `profiles/` directory, with recovery from the old `~/.claude-profiles`
location retained when the new directory does not exist.
A fresh install has no `local.sh`, every opt-in defaults off, and no tracked
source names this host's layout; `test_source_carries_no_host_specific_defaults`
pins that. A hardcoded institution path in shipped code is contamination; an
env var read from `local.sh` is the fix.

**Trust boundary.** `local.sh` is *trusted executable zsh*, not sandboxed
configuration — it is sourced into the ccmorph process alongside the library
itself, and anyone who can write it can already write `ccmorph.sh` beside it, so
it grants no new privilege. It follows that a bug in your own `local.sh` (a stray
`set -e`, a bad `export`) is a config error to fix in that file. Under ordinary
interactive zsh options, a
*parse* error in it does not brick ccmorph — zsh contains the failed nested
`source`, so the outer source succeeds and the launchers defined after the source
line still load (proven by `test_broken_local_sh_does_not_abort_the_source`, which
asserts both the source's exit status and the final launcher). This guarantee is
scoped to default options; a `local.sh` that turns on `ERR_EXIT` and then fails is
the "config error to fix in that file" case above, not a supported input.

### Runtime logging is ephemeral and optional

Launch and adapter stderr go to `${TMPDIR:-/tmp}/ccmorph-$UID.log`, not durable
state under the home directory. A sandbox may deny writes across directory trees;
a shell redirection to an unwritable log fails before the adapter process starts.
The adapter therefore probes the log path first and falls back to `/dev/null`, so
optional diagnostics can never become a launch dependency.

### The isolation boundary is the credential, not the profile

What isolation protects is **authority to spend**. So the unit that must not be
shared is a credential domain, and profiles inside one domain may share its
endpoint freely.

| Domain | Credential | Endpoint | Shared by |
|---|---|---|---|
| Local models | **none** | ollama `:11434`, direct | any number of profiles — safe |
| Codex | OAuth token in `~/.cli-proxy-api` (one file) | dedicated cliproxyapi `:<translator-port>` | `codex` alone |
| Private gateway | API key held in the profile conf | per-session adapter, ephemeral port | `gateway` alone |

Two consequences that look contradictory until the rule is stated:

- `codex` and `qwen` sharing `:<translator-port>` **was** a violation — cliproxyapi's inbound
  `api-keys` is instance-wide, so the credential-free profile held the key that
  spends the codex account.
- Several local profiles sharing ollama is **not** a violation. There is no
  credential, therefore no authority to leak. Giving each local model its own
  process would be machinery with no invariant behind it.

The visible symptom that started this — a foreign model in the picker — is a
consequence of multi-tenancy, not the reason to fix it. Local profiles listing each
other's models is expected and useful, because they are one domain.

### Adding a local model

Local models are model-addressed, not credential-addressed, so a new one is a
profile and nothing else. No change to `codex` or `gateway`, no new service.

1. `<name>.conf`: `type=apikey`, `base_url=http://localhost:11434`,
   `model=<ollama tag>`, and a placeholder `api_key` (ollama ignores it).
2. No adapter setting is needed. ollama answers `/v1/messages` itself **[wire]**;
   automatic launch classification keeps the native path. A future local server
   speaking OpenAI only is handled by the same automatic adapter path.
3. Nothing. `_ccm_launch_window` reads `num_ctx` from the endpoint's own
   `/api/show` at launch, so an authored copy could only drift from it.
4. Nothing else required — an unprobed tag is clamped to Claude Code's 200,000
   default. Optionally, the internal model-window resolver records
   a manual `context_window.<tag>` override in that profile's `.conf` ahead of
   any glob a discovered entry might match.

Both use `num_ctx`, not the model's advertised context length. The two
`qwen3-coder` tags serve 65,536 and 16,384 while both advertising 262,144, and
ollama truncates silently — so the advertised figure over-promises without erroring.

## 2b. Reaching a private endpoint

**ccmorph ships no transport.** Bridging to an endpoint this process tree cannot
dial is a separate product: it starts a local CONNECT proxy, and the caller runs
`HTTPS_PROXY=http://127.0.0.1:<port> ccmorph run <profile>`.

ccmorph's whole duty is to pass that ambient value through **unchanged** —
`HTTPS_PROXY`, `https_proxy` and `NO_PROXY` reach the Claude child, the adapter,
`ccmorph-core adapter`'s cliproxyapi, model listing, and `ccmorph-core refresh-model-registry`
exactly as exported. `HTTPS_PROXY` is deliberately absent from
`_CCMORPH_PROFILE_ENV`, so [the launch sweep](#the-env-lists-and-the-sweep)
never clears it, and no launch path sets or blanks it.

The one exception is a probe of ccmorph's own loopback services — adapter and
provider-bridge readiness on `127.0.0.1`. Those carry `--noproxy '*'`, because an
ambient proxy answers `000` for a service that is serving perfectly, and readiness
would never arrive.

**Why the seam is gone.** Until 2026-10 a profile could declare `transport=<token>`
and ccmorph called out to a host runner to open the crossing. Every consumer then
had to be handed the resulting proxy explicitly — and each of those sites spelled
the hand-off as `HTTPS_PROXY="${_ccm_route_https_proxy:-}"`, which **blanks** an
ambient proxy when no runner ran. A user who had already bridged correctly got
their proxy deleted by the tool that was supposed to use it. Removing the seam
removes the override: there is one value, the caller's, and it is never rewritten.
`test_shipped_surface_never_names_the_transport_product` pins the boundary from
this side: ccmorph ships no transport file, path, or even name.

A conf still carrying `transport=` keeps launching: `_ccm_read` prints one line
naming the replacement and ignores the key.

## 3. The adapter

Claude Code speaks exactly one dialect: Anthropic `/v1/messages`. A gateway that
also serves OpenAI-shaped models needs translation. Automatic launch classification
selects native pass-through or `ccmorph-core adapter`; nothing about it is specific to
a vendor or model list.

Lifecycle, per session:

1. Read endpoint and credential from the profile via `ccmorph _env`.
2. Ask the endpoint what it serves. Discovery must **not** send
   `anthropic-version`, or proxies rewrite non-Claude ids into
   `claude-fable-5-dd-<reversed-id>` **[wire]**.
3. A fresh terminal launch without a saved model opens the existing model picker.
   Use a declared or cached per-model dialect when present; otherwise check only
   the selected model, using the serving client's URL convention. Headless launches
   without usable metadata/cache ask for an explicit model instead of probing the
   whole catalogue. Cache proven results.
4. Render a `0600` cliproxyapi config under `<profile>/adapter/`.
5. Supervise cliproxyapi on an ephemeral loopback port; print `PORT=<n>`.
6. Rewrite `ANTHROPIC_BASE_URL` to that port, then launch Claude Code.

### The long-lived endpoint form

A launch is per session. A non-Claude client that outlives sessions — Hermes
— needs a fixed address and a fixed credential instead, on the same adapter:

```
ccmorph-core adapter <profile> --auto --model <id> --port <n> --inbound-token-file <path>
```

Loopback only, like every route the adapter serves. The port is served exactly
as given, and refused up front when out of range (1024–65535) or already
taken; from validation until the shim binds it stays reserved, so no other
process can take it mid-startup and collect the readiness probe's credential,
and the adapter can never health-check a stranger's server and report it as
its own **[test]**. The token file must be a regular `0600` file owned by
the user holding at least 32 characters; anything else — a symlink, a broader
mode, a shorter file, control characters — is refused before anything
starts **[test]**. The
token is never printed: a service's stdout may be redirected to a file, so
unlike a minted per-launch token there is no `TOKEN=` line **[test]**. At the
shim the client's token is swapped for the profile key, and the shim's
outbound rules below apply unchanged — the same task-tag, hooks and
identifier handling as every other profile's traffic **[test]**.

Without `--parent-pid` the adapter runs until signalled; running it as a
service is the caller's job, not ccmorph's.

### Why a credentialled endpoint gets its own instance

cliproxyapi's inbound `api-keys` is instance-wide, so an instance shared across
credential domains lets one profile spend another's credential. A profile holding a
credential therefore gets its own instance, with its own key as the inbound key —
nothing new is minted and no second credential exists on disk beyond the rendered
config. This is the mechanism behind the boundary rule above; it is not an argument
for one process per profile.

### Supervision details that were each a bug once

- Cleanup runs on **EXIT only**. The pre-existing `trap ':' INT` must stay: an
  INT handler unwinds the relay loop and loses a queued profile hop.
- `SIGHUP` is trapped. Without it, `tmux kill-session` left a `0600` config
  holding the credential on disk.
- `--parent-pid` watchdog: the adapter exits when its launcher dies **[test]**.
- Adapter reconciliation happens **inside** the relay loop, because a hop changes
  the profile and therefore the required instance.
- **The adapter always runs locally**, in the launcher's own process tree, so it
  inherits the launcher's environment — including any ambient proxy. Nothing on the
  launch path sets or clears `HTTPS_PROXY` for it **[test]**.
- **A subprocess that must REACH the endpoint asks for the route, not just the
  environment.** `ccmorph _env` answers "what does this account launch as", which
  for every type but `provider` is also enough to dial it. A `provider` has no
  endpoint until its bridge process runs, so `ccmorph _route_env` starts one and
  prints `CCMORPH_ROUTE_HANDLE`; the caller owns that bridge and retires it with
  `ccmorph _route_stop <handle>` **[test]**.

### When no translator is needed at all

Automatic classification keeps native endpoints direct. Local ollama serves
`/v1/messages` and answers 200 **[wire]**, so a profile pointed at it runs
`type=apikey` straight to `http://localhost:11434` with a live turn confirmed and
zero adapter processes.
The adapter's own classification is the tell — it labelled both ollama models
`anthropic`, i.e. it would have proxied Anthropic to Anthropic and translated
nothing.

`codex` remains on the shared brew `:<translator-port>` because its upstream authenticates by
OAuth held inside that instance's config, which a profile-rendered config cannot
reproduce. That is a real constraint, not a preference.

Isolating `qwen` off `:<translator-port>` was worth doing for a reason that is not tidiness.
cliproxyapi's inbound `api-keys` is instance-wide, and `codex` and `qwen` were
probed to present the **identical key string** (same sha256), each able to drive
the other's backend. `qwen` needs no credential at all, so it was holding the one
key that can spend the codex account. Moving it to ollama removes that reach;
`:<translator-port>` now serves one profile, so the shared key is no longer shared.

`adapter` is valid on `apikey` and tolerated on legacy `proxy` profiles. It
describes the endpoint's dialect, which does not depend on how the endpoint
authenticates — and the launcher already read it for either type, so only
validation had disagreed **[test]**.

### What the shim changes in an outbound request

Nothing, by default. The body, `metadata` and every header — including
`X-Claude-Code-Session-Id`, `X-Stainless-*` and the `User-Agent` — go upstream
as the client sent them **[test]**. The one per-profile opt-in that remains core
is identifier randomisation **[test]**:

| Profile key | Shim flag | Effect |
|---|---|---|
| `env.CCMORPH_RANDOM_DEVICE_ID=1` | `--random-device-id` | On `POST /v1/messages` and `/count_tokens`, `device_id` inside `metadata.user_id` (a JSON string) becomes a random 64-hex value minted once per session and stable within it (keyed by the session id, held in memory only). `session_id`, `account_uuid`, every other key and every header are untouched, so prompt caching and gateway affinity do not move. A body it cannot parse, or whose `user_id` is not that JSON shape, goes upstream byte-identical. |

Generic gateway compatibility lives in `compat/`, which imports `shim` (never the
reverse); the stock `cmd/gateway-shim` passes `compat.FromEnv()` to `shim.Main`. Each
rule is decided by the request's structure or the HTTP status, never by a gateway's
error text. Always on: a `tool_reference` inside a `tool_result` whose `tool_name` the
request does not declare in `tools` becomes a text block (declared references and every
other byte stay), and a 400 on `POST /v1/messages` with nothing yet sent downstream is
resent once with `thinking` and `redacted_thinking` blocks removed from assistant turns
(`Hooks.ResendBody`; not a transport retry, not counted by the breaker, the second answer
is final) **[test]**. Profile keys, ferried as `CCMORPH_*` env: `env.CCMORPH_AFFINITY_HEADER=<name>`
sends the session id in that header, `env.CCMORPH_USER_AGENT=<template>` sets the outbound
`User-Agent` from the client's `claude-cli` version, and the adapter's passthrough entries
take theirs from the shim's `translator-user-agent` subcommand **[test]**. `env.CCMORPH_COMPAT=off` gives the shim no hooks at all (no repair, no resend, no affinity header, no User-Agent rewrite) **[test]**. Anything beyond
that is a custom build: `env.CCMORPH_SHIM_HOOKS=<binary>` names a Go `main` that wraps
`compat.FromEnv()` and calls `shim.Main`; the adapter runs it in place of the stock shim,
and a missing or non-executable file fails the launch.

### Transport retry and the provider breaker (direct route only)

The adapter passes `--retry-transport` to the shim it starts in `front_directly` and never
to the translator path's shim: with no translator underneath, nobody else retries, while
behind cliproxyapi a shim retry would multiply its own `request-retry` (which on 8.0.45
also retries 403 and 429). On a 429, 5xx or
refused connection — and only before the client has any byte — the shim resends the same
body to the same target twice, waiting a random interval each time (full jitter, up to
0.5 s then 1 s; `CCMORPH_RETRY_BASE` sets the base and the tests set it to 0). A stall is
not retried: it already waited the stall timeout and Claude Code retries it itself
**[test]**. Each retry is the same bytes to the same target, so no retry can move a call
to another model or provider, and the tier cannot change under load **[test]**.

Three requests whose reply was a retryable status, or whose transport failed before any
reply, and that **still failed after their retries**, within 5 minutes, open a per-shim —
that is, per-provider — breaker for 10 minutes. While it is open the shim answers a
readable 503 without sending the request upstream (a route it has not seen may still cost
one model-catalogue read before that answer); the next real request past those 10 minutes
is the probe, and success closes the breaker while failure reopens it. No synthetic test
call is ever made, and the state is in-process only (a relaunched shim starts closed)
**[test]**. The breaker reads the reply's status, not the stream, so a 200 that breaks
mid-stream neither counts as a failure nor keeps a failure history alive — the streaming
path is deliberately unchanged here. Counting failed requests rather than attempts is what
keeps a blip from taking a single-provider route offline.

### The usage sidecar: one metadata row per response id

The adapter hands the selected route to `gateway-shim` in
`CCMORPH_MODEL_ROUTE` (it needs the spec anyway to prepare a newly picked
model). A shim holding that spec writes one metadata-only row for every
`/v1/messages` response it relays, at

```
${XDG_STATE_HOME:-~/.local/state}/ccmorph/usage/<sha256(response_id)>.json
```

so a response id quoted in a Claude Code transcript joins to what that turn
cost by computing that filename directly. If a gateway recycles an id, the
earlier row is never overwritten: when that file already holds a row with a
different `turn_id` (or one that cannot be parsed), the later turn is written
to `<sha256(response_id.turn_id)>.json` instead, and a consumer that needs
every turn for an id globs the directory and matches the `response_id` field.
If that name too holds another turn's row, the later turn takes the first free
`<sha256(response_id.turn_id.N)>.json`, N = 1–3; a write only takes a name that
is free or already holds its own turn's row — the claim is a hardlink where the
filesystem has them and an exclusive create where it does not, atomic either
way — so a turn whose every name is taken loses its own row rather than destroy
one **[test]**.
Rewrites of the same record (the usage updates and `message_stop`) share its
`turn_id` and keep their file. `turn_id` is a random per-turn token, not the
`timestamp`, because separate turns can carry the same microsecond timestamp
and a timestamp was therefore not a usable ownership key. Rows written before
`turn_id` existed carry no token and stay readable:

```
{schema: 1, response_id, turn_id, route_id, profile, requested_model, task, attempt,
 served_model, retries, response_model, timestamp, usage, complete}
```

`task` and `attempt` are the task tags below (`null`, `1` when a launch set none).
`retries` is how many times the shim re-sent the request before the response this row
describes: up to two transport retries under `--retry-transport`, otherwise the single
unexpected-EOF replay, so `0` when it re-sent nothing **[test]**.

`usage` carries whitelisted numeric buckets only — `input_tokens`,
`output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, the nested
`cache_creation` split, and the gateway's own billed `cost` when it reports one as a
number `>= 0` below the shim's `COST_LIMIT` (the largest amount a float still holds to
the cent, `2 ** 53 / 100`). `cost` is that billed amount and nothing else: no conversion
and no list price ever enters it. When the upstream bills outside the response (a provider reached directly, such as
DeepSeek's own API) the shim writes its computed amount as `estimated_cost` instead, in the
same US dollars: the four token
classes times the per-million price models.dev lists for the served model under the provider
whose `api` host is the route's upstream host (a class with no listed price is charged at the
input price; context tiers are not applied, so `estimated_cost` is a list price and an
under-estimate on a prompt past a tier). An upstream models.dev does not know gets no
`estimated_cost`. The shim never writes `estimated_cost` from an upstream's response and
never writes `cost` itself, so a reader that sums billed spend never adds an estimate.
The host's slice of `api.json` is cached in `prices-<host>.json` beside the receipts and
refreshed after a day. A billed cost is never replaced, and rows written before this existed
are not recomputed; rows written by builds that carried #34 before `estimated_cost` existed
hold that computed amount in `cost`, and nothing marks them as estimates. A string (`service_tier`), an unknown key or
a bool never enters the file, and neither does a prompt, a header, a session
identifier or a credential **[test]**. `route_id` is a domain-separated digest
of the profile and the adapter's route identity; that identity is itself a
digest of `upstream + key`, so it, the key, its hash, the rendered config path
and the token are never written anywhere in the row.

**Task tags.** A launch through the adapter may carry `CCMORPH_TASK` (opaque
id, `[A-Za-z0-9._-]{1,64}`) and `CCMORPH_ATTEMPT` (digits). The launcher turns the
well-formed ones into Claude Code's `ANTHROPIC_CUSTOM_HEADERS` (`x-ccmorph-task`,
`-attempt`) and clears the variables so the session's children never inherit
them; a direct (no-adapter) launch carries none. Because the generated settings
strip every key the launch sets itself (the process environment must win), the
one diagnostics interpreter that answers the launch's other settings-layer
questions also brings back the user's own `env.ANTHROPIC_CUSTOM_HEADERS`, and
the launcher sends their lines in front of the tags, so a tagged launch no
longer drops a header such as a tenant id. After verifying the key the shim tags
the row and drops both headers before the upstream. **Task tags never
change the requested model**: the caller picks the model once per task with `--model`,
because a gateway rewrite would also override subagents.
`requested_model` is the exact client ID, `served_model` is the ID sent upstream after
resolution, and `response_model` is the ID reported to the client.

**Explicit profile model map.** An apikey profile may declare a JSON object such as
`model_map={"claude-sonnet-5-5":"gpt-6.1-sol"}` in its `.conf`. The adapter carries only
that profile's map into its native or translator shim. `/v1/messages` and
`/v1/messages/count_tokens` apply one exact lookup before preparing or sending the
model, and its key is the **wire** model ID: the id with the `[1m]` window marker
stripped, which is what the gateway is asked for — the shell strips that marker before
it hands the adapter a model to prepare, and a native client's `claude-sonnet-5-5[1m]`
picker row reaches the wire as `claude-sonnet-5-5` because Claude Code normalizes it,
so the launch, preparation and the request all resolve one key. The marker is
ccmorph's own tag, never part of a model id, so a key carrying it names nothing:
reading the profile refuses one with a plain error — at launch, and in the
`ccmorph _env` the adapter reads the map through — instead of running a map that
decides nothing **[test]**. Mapped targets are final: no family substitution or
recursive mapping. Responses keep the requested picker identity, while the usage row
records the gateway target separately. Missing maps and unknown keys retain the
existing resolution behaviour, including the no-downgrade rule for messages.
Unmapped token-count requests keep the pre-map pass-through: no
catalogue lookup, no compatibility preparation, and the next hop authenticates
them, while the per-launch inbound token check remains unconditional. The
opt-in device-id swap and tool-reference repair apply to them as to any count
post; only the model work is skipped.
The map applies to any matching request on that profile, including subagents; it
does not install a default map. Existing RC `tier.*` pins
run earlier in the proxy, so a map sees the pinned ID when a host has one.
Outcomes are appended with `libexec/gateway-shim outcome <task>
accepted|rejected|escalated|parked` to `<state>/ccmorph/outcomes.jsonl`
(`{task, outcome, timestamp}`), joined to rows by task.

**Ownership.** The shim owns the write, ccmorph owns the format and the
retention, and the consumer — Helm, or anything joining response ids — reads
these files and nothing else. Nothing reads them back into a launch, they never
travel to a remote host, and they sit under the XDG *state* root rather than the
profile root because they are derived state: deleting the directory loses no
configuration, no profile and no credential, and the state root keeps containing
only what a launch needs.

**Lifecycle.** An SSE row is written at `message_start`, before the turn can
finish, rewritten on each usage update (`message_delta`) and once more at
`message_stop` — the only event that sets `complete: true`. Writes are atomic
(tmp + rename) at `0600`, so a crash, a truncated stream or a client disconnect
leaves the last known partial row rather than a half-written one, and a sidecar
that cannot write leaves the relayed response byte-identical. A non-stream JSON
message is one complete row.

**Limits.** The response id is the only join key — nothing here identifies a
session, by design, since the transcript already carries the id. A response
whose id never reaches the client cannot be attributed: the shim's own
unexpected-EOF retry consumes an upstream 500 whose body carries no id, so that
call produces no row at all. Capture is off unless the adapter passed the route
spec, so a hand-launched shim writes nothing. Every session that runs through
`ccmorph-core adapter` carries the shim — it is the stable request shim, served inside the adapter's own process (one helper per gateway session; only a translator route adds the `cliproxyapi` child and its tether, and a profile's `CCMORPH_SHIM_HOOKS` build is still spawned as its own executable), interposed
with `--preserve-tool-ids` where the upstream does not recycle ids — but a
profile that never starts the adapter has no shim and therefore no rows:
native, direct endpoints and any route still served by the shared brew
translator (§3). Coverage is per launched session, not per profile.

### The daily usage summary: what the gateway billed, per Hong Kong day

A monitor reading transcripts cannot price gateway-routed traffic itself: the gateway serves
a model the client did not ask for and bills its own price, so the model a transcript names
is not the model that answered and a list price is not what was paid. `scripts/usage-summary`
reads the sidecar receipts and writes only what the gateway itself recorded, at

```
${XDG_STATE_HOME:-~/.local/state}/ccmorph/usage-summary.json
```

beside the index that makes a repeat run cheap,
`${XDG_STATE_HOME:-~/.local/state}/ccmorph/usage-summary.sqlite`. The command takes
`--source` (the receipts) and `--state` (both output files), each defaulting to the state
root as above **[test]**, and prints its run report — `read`, `statted`, `removed`,
`written`, `days` — as JSON. It is private (`scripts/`), stdlib only, and starts no service:
whatever wants a fresh summary runs it.

```
{"schema":1,"timezone":"Asia/Hong_Kong","files":N,"invalid":N,
 "days":{"YYYY-MM-DD":{"total":{...},"groups":[{...}]}}}
```

A **day** is a Hong Kong day — `Asia/Hong_Kong`, UTC+8, no DST: a row's `timestamp` is
converted to it and the key is `%Y-%m-%d`, so a turn at 15:59:59 UTC belongs to that day and
the turn one second later, at 16:00:00 UTC, belongs to the next **[test]**. Each day holds a
`total` and one **group** per `(profile, served model)`, sorted by profile and then model.
Both hold the same fields: the four token buckets, `rows` (complete rows), `priced`,
`unpriced`, `incomplete` and `cost`; a group adds `profile` and `model`.

`cost` sums the gateway's own billed amounts and nothing else: US dollars, the unit the
shim stores, never a conversion or a list price. A receipt's `usage.estimated_cost` is the
shim's own models.dev list price and never enters `cost`: a row carrying only an estimate
counts `unpriced` here, and reading the estimate is a separate question for a separate
reader. A complete row whose
`usage.cost` is missing, null, non-finite, negative or at or above the shim's `COST_LIMIT`
is counted `unpriced` and adds no cost at all — never a zero and never a list price
**[test]** — and a group or day with no priced row has `"cost": null`, so a monitor shows
"unpriced" rather than claiming the traffic was free. The sum is a `math.fsum` of the priced
rows, so it is exact and independent of the order the receipts were read in **[test]**. A
row whose `complete` is not exactly `true` — the reading the shim's own consumer of these rows
applies — is counted `incomplete` and nothing else: its tokens and its
partial cost stay out of the totals, because a crashed turn's usage is not what was billed
**[test]**. The model is `served_model`, else `response_model`, with one trailing `[1m]`
window marker stripped, the rule the shim's own model resolution applies; `requested_model`
is never used as the served identity, and a row carrying neither lands in the `model: null`
group **[test]**. A profile or a model name the index cannot store — a lone surrogate, which
JSON carries through as an escape and the shim can pass straight through from upstream — is
filed as no name, never dropping the turn with it: the gateway billed for that row whatever
its name prints. `files` is how many receipts are indexed and `invalid` how many of them
could not be read: bad JSON or bytes that are not text, another `schema`, a `response_id` or
`usage` object the writer always stores and this reader will not take — a response id has to be
a string, which is the whole of what the shim tests it with, so an empty one is a receipt it did
write while a missing or non-string one is damage — a naive or missing `timestamp`, a
stamp with no Hong Kong day left in it, or a bucket present as anything but a finite number —
which is the whole of what the writer's own filter drops. A number that filter keeps but no
gateway billed — a negative count, or one at or past the index's own 64-bit integer bound —
counts as no count and is no damage at all: the row keeps its day, its `rows` and the cost the
gateway charged for it, because the shim bounds a cost and never a count **[test]**. An absent
bucket is not damage either: that filter omits the ones it never billed, so an absent bucket is
the zero it never had to store. Damage is a status, never an exception, and the
shim's own `tmp*` names are not receipts at all and are never read **[test]**, nor is a name
that is not a regular file — the writer lands a receipt by renaming one into place — so a
directory or a FIFO under a receipt's name is skipped rather than read, and a run never waits
on one for bytes that have no end **[test]**.

**Incremental.** A run lists the names, reads the ones the index has not seen, and after
that re-stats only the names it holds as incomplete or invalid — re-reading one whose stamp,
size and inode changed — drops the names no longer on disk, and recomputes only the days
those rows touched, carrying the rest over from the text the index itself committed, never
from the summary file beside it, which is this tool's output and no input of it **[test]**.
The writer is what makes that sound: a receipt is rewritten only while it is incomplete
(atomic tmp + rename) and a complete row is never written twice. On the real directory,
about half a million receipts, that is the difference between about two seconds of listing
and twenty-three of statting every one of them, which is what makes a poll affordable. One
boundary comes with it: a receipt deleted outside the tool whose name a later turn lands on
again — the writer names a receipt by its response id's hash — is a row on disk no run here
reads, so that day stands as the index last had it until the index is deleted, which rebuilds
every day from the receipts that are there **[test]**. A run is one `BEGIN IMMEDIATE`
transaction, so two overlapping runs serialise instead of each undoing the other's work
**[test]** — a run that cannot take the lock inside the index's 30 s busy timeout exits 1
having written nothing, which is the honest failure for a poll that arrived mid-run — and the
summary's own text is committed in that transaction, the output file published only once it
has committed, so a run that fails or dies before the `COMMIT` rolls that text back with the
rows and the next run counts from what the index does hold, while a run that dies after it
leaves at worst a stale file, which the next run republishes **[test]**. The publish takes the
lock again and writes the text the index holds rather than the text that run counted, so two
runs meeting at the file — one's days committed, the other's a receipt further on — cannot end
with the older run's days landing last, which would be a summary going backwards until a later
receipt lands on that day again **[test]**. The output file is
replaced only when its bytes differ and carries no generated-at stamp, so a poll can tell a
change by its mtime **[test]**. A
`--source` that is not a directory fails the same plain way — one line naming it on stderr,
exit 1, no summary published — because a monitor reads a summary rather than the exit code,
and must never find one saying a day was empty that the run could not count **[test]**. The
tool's own index and summary are never treated as receipts even when `--state` puts them in
`--source`, which is a directory layout that is otherwise perfectly ordinary **[test]**.

**Derived state.** Both files are derived: delete the index and the next run reads every
receipt again and counts every day from the receipts, so the summary is rebuilt with it. That
is the repair both for a receipt edited by hand outside the shim (a complete row already
indexed is not re-read) and for a day whose receipts are gone, which no longer stands on a
total nothing on disk supports **[test]**. It is also why the summary file is never read back:
a day edited into it by hand is not a day any receipt supports, and the next run replaces the
file with what the index committed **[test]**. Nothing prunes receipts today, so the one
`listdir` per run grows with retention; the index itself is only a cache of what they said.
The tool never writes to `usage/`, and neither file holds prompt text — only profile and
served-model names and counts — though both files are `0600`, like the receipts, and any
directory the tool creates is `0700`: one already there keeps the mode it has **[test]**.

## 4. Dialects

Probed in preference order, cheapest translation first. The tool shape differs
between the two OpenAI dialects and getting it wrong yields a confident
misclassification:

| Dialect | Endpoint | Tool shape |
|---|---|---|
| `anthropic` | `/v1/messages` | Anthropic |
| `responses` | `/v1/responses` | flat `{"type":"function","name":...}` |
| `chat` | `/v1/chat/completions` | nested `{"type":"function","function":{...}}` |
| `chat-noreason` | `/v1/chat/completions` | as `chat`, minus `reasoning_effort` |

The Anthropic probe must not force `thinking: adaptive`. Claude Code adapts
thinking per model for ids in its own catalogue; forcing it made
`claude-opus-4-5`, `claude-sonnet-4-5` and `claude-haiku-4-5` report "adaptive
thinking is not supported" and look unusable when all three work **[wire]**.

Rejecting a parameter is not grounds for dropping a model. Two config mechanisms
absorb the difference instead:

- `payload.filter` strips `reasoning_effort` for `chat-noreason` models.
- `payload.override` clamps `max_tokens` to each model's own stated cap, read
  from that model's error text rather than assumed.

`disable-image-generation: "chat"` is required. Without it the Responses provider
injects an image_generation tool and the upstream rejects the call with
`imagegen deployment must be provided through header
x-ms-oai-image-generation-deployment` **[wire]**.

### Translator config workarounds, verified on cliproxyapi 8.0.45

Each workaround `adapter/render.go` writes was re-checked against 8.0.45 source and
a black-box run (real binary, loopback fake upstream) **[src][wire]**:

| Setting | 8.0.45 source | Verdict |
|---|---|---|
| `request-retry: 3` | `sdk/cliproxy/auth/conductor_selection.go` `isCredentialRetryRoundStatus`: 403, 408, 429, 500, 502, 503, 504 plus transport EOF | keep. **429 and 403 are now retried too**, immediately and without backoff; one 502, 429 or EOF then 200 reached the upstream twice, `request-retry: 0` did not retry |
| `disable-image-generation: "chat"` | `internal/config/disable_image_generation_mode.go` | keep. Without it a Responses call carries `tools:[{"type":"image_generation"}]` upstream |
| `disable-cooling: true` | `internal/watcher/synthesizer/config.go` (`disable_cooling` metadata) | keep. Without it the call after one 500 returns 503 `auth_unavailable` with no upstream hit |
| `rebuild-mid-system-message: true` | `internal/runtime/executor/claude_signing.go` `rebuildMidSystemMessageEnabled` | keep. Off: a `role:"system"` message reaches the upstream; on: it is folded into `system` |
| `cloak: mode never` | `internal/runtime/executor/claude_executor_cloaking.go` `resolveClaudeWirePolicy` (`Uncloak`); 7.2.110 `helps/cloak_utils.go:41` `ShouldCloak` | **version-gated**. On 7.2.110 no block means `auto`, which cloaks any client whose User-Agent is not `claude-cli` (frozen cc_version billing header), so `never` is written for translators < 8.0.34. Since 8.0.34 `never` strips Claude Code's billing header, identity system block, `metadata.user_id` and `diagnostics` and rewrites the User-Agent, while no block forwards the caller verbatim, so it is omitted. The version comes from `cliproxyapi -h` (banner, exit 0); an unparseable version counts as new |
| `thinking.block_binding` filter | `claude_executor_request.go` `applyClaudePreservedThinkingBinding` runs before `ApplyPayloadConfigWithTrackedPaths` (`claude_executor_execute.go:246-249`) | keep. Sonnet 5.5 with a signed thinking replay: field and `thinking-binding-controls-2026-08-01` beta header present without the filter, absent with it |
| never put `system` in `payload.override` | payload override | keep. `override: system` replaces the whole field |
| `display-name` on `oauth-model-alias` | `internal/config/config_types.go` `OAuthModelAlias.DisplayName` | keep. Not written by the adapter; only the root-owned `:<translator-port>` config uses it |

## 5. Context window

Two separate things, often confused:

| Control | Applies to | Effect |
|---|---|---|
| `[1m]` model suffix | any id | window becomes 1M, on any endpoint — the first test in the chain |
| `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | ids whose name does **not** start with `claude-` | functional — drives compaction |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW` | any id | a ceiling on the compaction trigger, capped by the window above |
| `context_window.<model>` | one profile/model pair | manual context limit stored in the profile configuration; wins over discovered values. Set with `ccmorph context <profile> <model> <tokens>`, cleared with `auto` |
| `context_discovered` | one profile, route-bound | JSON blob in the same `.conf`; limits `ccmorph probe` discovered for the profile's current route, invalidated when the route changes |

So on one profile a `claude-*` model and a routed model get different windows from
the same config, and both are correct.

For a routed id, the launcher reads only local data. `_ccm_launch_window`
uses the canonical `model-window` resolver: an explicit `CLAUDE_CODE_MAX_CONTEXT_TOKENS`
pin wins outright; otherwise the profile's exact manual override
(`context_window.<model>`) wins over its `context_discovered` metadata, which is
tried by exact model id first and then by glob pattern. If no value is known
there, `model-window` falls to `MODEL_WINDOW_ENDPOINT_JSON` (the current launch's
own endpoint listing), then to Claude Code's 200,000 default. There is no
separate on-disk cache file and no cross-profile registry read — everything but
the endpoint listing lives in the one profile's `.conf`.
Gateway-declared sizes pass through `window-sources.json` before they are stored:
a provider's own catalogue applies only to profiles that name it
(`window_sources=`), scoped further by listing `owned_by`. It raises a declared size
only when that size is exactly the provider's published default (the gateway is
echoing it). Any other declared figure stands, nothing is lowered, and a size the
gateway omits is left to the tagged catalogue estimate. While a source cannot be
read, the models it speaks for stay undecided and are retried on the next run.
The shipped file is the whole contract; supporting a provider adds an entry, not code.
Positive integer metadata is usable even when fields are equal. A background
refresh preserves an existing discovered value for the same model on conflict and
discloses the listing's different claim rather than overwriting silently.
Discovered data is keyed to a route fingerprint (endpoint, credential,
and the `ANTHROPIC_*` env overrides other than tier mappings and model choice, which
select a model rather than a route); the moment any of those change, the stored
`context_discovered` for that profile is treated as absent and a fresh probe
starts clean. A manual override is never route-bound and always wins
**[test]**.

The window is resolved by a fixed chain, first match wins **[src]**:

1. id ends in `[1m]` → 1,000,000
2. the request carries `context-1m-2025-08-07` and the model supports it → 1,000,000
3. the model is natively 1M **and the base URL is `api.anthropic.com`** → 1,000,000
4. `CLAUDE_CODE_MAX_CONTEXT_TOKENS`, but only if the id does not start with `claude-`
5. otherwise 200,000

Every routed profile fails step 3 by construction, and a `claude-*` id fails step 4
on its prefix. So a Claude model reached through a gateway lands on **200,000** no
matter what the conf declares — the trap that made gateway read `27k/200k` after a
`/model` pick. Step 1 is the only escape, which is why `gateway` sets
`model=claude-opus-4-8[1m]` and pins the native `opus` family alias to the same suffix.

Verified on the live route **[wire]**: a request on `claude-opus-4-8[1m]` succeeds
and reports usage under that exact id, so the suffix survives the translator. The
gateway itself is not the limit — `claude-opus-4-8` accepted **935,511** input
tokens there.

### The compaction trigger runs on a different number than the display

Codex compacted at `34% (120k/350k)`. Both numbers were right and neither was the
one that fired it. `/context` on that session showed the split **[wire]**:

| | Tokens |
|---|---|
| header, statusline, and the provider's own `usage` | 120,491 |
| `/context` → Messages | 300,100 |
| `/context` → autocompact buffer | 33,000 |

Claude Code decides on its **local estimate**, which read 317k of a 350k window —
correctly at the limit — while every visible counter showed the provider's 120k.
The estimate ran **2.6×** the truth.

The route is not at fault, which took probing to establish. Sent prompts of
rising size against `:<translator-port>`, each with a unique prefix so nothing was served from
cache **[test]**:

| Sent | Reported | |
|---|---|---|
| ~130k | 182,314 | ratio 1.40 |
| ~265k | 371,314 | ratio 1.40 |
| ~280k | — | `400 Your input exceeds the context window of this model` |

Reported tokens track the input linearly to the last accepted size and then the
endpoint **refuses**. Nothing is silently trimmed, and `cache_read_input_tokens` is
reported (120,320 of that 120,491), so the provider's figure is the whole prompt.

Two consequences:

- **`gpt-5.6-sol` through `:<translator-port>` holds ~371k, refusing by ~392k.** The declared
  350,000 was already honest — the window was never the bug.
- **The window is the only lever on the trigger.** `aY` takes
  `min(context window, CLAUDE_CODE_AUTO_COMPACT_WINDOW)`, so raising one without the
  other changes nothing. `_ccm_launch` sets both together or neither.

### The invariant: compaction must fire before the endpoint refuses

```
usable  = window − min(max output tokens, 20,000)
trigger = min(usable − usable×0.15, usable − 13,000)
```

The estimate Claude Code probes is never *below* the true prompt, so:

> **`trigger < the real ceiling of every model the profile can reach`.**

Satisfy that and compaction always arrives first. Violate it and the session dies on
`Your input exceeds the context window of this model` with no warning, because
nothing in Claude Code knows the real ceiling. `window ≤ ceiling` is sufficient.

Launch uses a manual profile/model override when configured, then the profile's
own `context_discovered` metadata. Before the picker projection, launch hands the
`/v1/models` listing it already fetched to `ccmorph-core refresh-model-registry --listing-stdin`,
which sizes only listed models the profile has no limit for, in this order:

1. the gateway's declared size in the listing;
2. a models.dev estimate (3s per catalogue fetch);
3. the total length the server states when refusing a `max_tokens` of 100M. The
   refusal happens at validation, before generation, and vLLM-backed internal models
   name `max_model_len` there (`Qwen3.8-27B-FP8` → 1,000,000, 2026-09-24). Only a
   total-length phrase is read: `[1,131072]`-style ranges are output caps;
4. otherwise a dated entry in `context_discovered.misses`, retried after 7 days.

With nothing new it is a local comparison. Everything goes to the log; nothing asks
the person launching to act. A model still unknown after all four is budgeted at
128,000 (`UnknownBudget` in `modelwindow/`) rather than Claude Code's 200,000, because
measured walls on one route sat at 131,072 and 128,000: early compaction on a big
model is recoverable, a session past its wall is not. Two traps, both found by probe:

- The field appears **only** on the Anthropic-shaped route and **only** when
  `anthropic-version` is sent. Without the header the same endpoint answers four bare
  fields, which is why `ccmorph models` — which drops the header deliberately to keep
  ids readable — cannot supply it.
- Sending the header is what makes translators disguise non-Claude ids as
  `claude-<cover>-dd-<reversed id>`, so `gpt-5.6-sol` comes back as
  `claude-fable-5-dd-los-6.5-tpg`. Matching undoes the reversal, keying on the `-dd-`
  shape rather than the cover name so a different cover still resolves.

The answer is remembered per model in the profile's own `context_discovered`
field, and a launch that cannot reach the endpoint reuses it. Without that, one
timed-out request drops the
window to Claude Code's 200,000 and moves the trigger from 299,200 to **153,000** — a
session quietly losing two fifths of its context because of a network blip, with
nothing said. A ceiling does not change between launches; reachability does **[test]**.

Derivation applies **only to ids that do not start with `claude-`** — the same prefix
test Claude Code applies to `CLAUDE_CODE_MAX_CONTEXT_TOKENS`, for the same reason.
Claude Code resolves a `claude-` id from its own catalogue and is right where the
endpoint is not: the gateway adapter declares **200,000** for `claude-opus-4-8`, a
model probed at **935,511** through that same gateway. Honouring that declaration
would have cut the window fivefold, so a `[1m]` profile keeps what Claude Code knows
**[test]**. A conf that pins a ceiling also still wins — an author may want a smaller
budget on purpose, so derivation fills a gap rather than overruling one **[test]**.

`_ccm_invariant_warn` is that check (a launch reaches the same two halves through the
one interpreter it runs, `_ccm_launch_diagnostics`) and it names any offender, rather than
leaving it to arithmetic in this document that stops being done. It reads each
profile's `availableModels`, undoes the translator disguise, and compares the ceiling
in that profile's `context_discovered` data against the trigger the id will actually
get. A warning, never
a refusal — the ceiling data is best-effort and stale discovered data must not be able to
block a launch **[test]**.

Checked for every reachable model, 2026-08-02 — **0 exposed** (was 2):

| Profile | Model as Claude Code sees it | Window | Trigger | Ceiling |
|---|---|---|---|---|
| codex | `gpt-5.6-sol` | 372,000 *(derived)* | 299,200 | 372,000 |
| codex | any cloaked gateway id | 200,000 | 153,000 | ≥ 272,000 |
| qwen | `qwen3-coder:64k` | 65,536 | 32,536 | 65,536 |
| gateway | `claude-opus-4-8[1m]` | 1,000,000 | 833,000 | 935,511 |
| gateway | the other six Claude families | 200,000 | 153,000 | ≥ 200,000 |
| gateway | `claude-sonnet-4-6[1m]` | 1,000,000 | 833,000 | 1,000,000 |
| gateway | `glm-5.2` and 19 others | 200,000 | 153,000 | ≥ 258,048 |

The two that were exposed, and why each resisted the obvious fix:

- **codex at 550,000** put the trigger at 450,500 against a 372,000 ceiling. Raising
  the window to buy back context from the 2.6× over-count had walked straight past
  the wall. Typing 372,000 instead only moved the hardcode, so the value is now read
  from the endpoint on every launch and the conf carries no ceiling at all.
- **gateway's cloaked third-party ids** included `Qwen3.6-27B-FP8` (131,072),
  `gpt-4o` and `gpt-4o-mini` (128,000), all under that profile's 153,000 trigger. No
  env var reaches them: a cloaked id starts with `claude-`, which skips
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS` entirely, and lowering the profile's window would
  have cost `claude-opus-4-8[1m]` its 1M. Only the allowlist could — so those three
  are excluded by id and the other **20 were restored**, having been removed as
  collateral in the first pass.

### Two ceilings that were guesses

| Model | Ceiling | How it was established |
|---|---|---|
| `claude-sonnet-4-6` | **1,000,000** | the gateway states it: `prompt is too long: 1201388 tokens > 1000000 maximum` |
| `glm-5.2` | **700,000 – 1,400,000** | 700,000 arrived intact, markers at both ends echoed; 1.4M, 2.1M and 3M all refused |

`claude-sonnet-4-6[1m]` matters because Claude Code offers that row unconditionally and
no allowlist can remove it: `[1m]` forces a 1M window and an 833,000 trigger, which the
probed 1,000,000 clears.

**glm-5.2 did not truncate, and did not error either.** In the 2026-08-02 check, an
over-limit request answered **HTTP 200** with empty content,
`stop_reason: "model_context_window_exceeded"`, and `usage` all zeroes. That is an
observed result for that route, not a billing guarantee for future probes. Claude Code
recognises that stop reason and reports "The model has reached its context window limit"
**[src]**, so the failure is visible rather than silent. The registry's standing note
that it "truncates silently" was inferred from acceptance alone; acceptance is neither
truncation nor success.

Hence the rule that makes the invariant maintainable rather than a coincidence:

> **The route ceiling is the max; the budget is per model; a model the budget
> cannot protect is named, not hidden.**
> `CLAUDE_CODE_MAX_CONTEXT_TOKENS` is fixed at process start, so ccmorph sets it
> to the LARGEST probed window the route serves (`_ccm_launch_window`) — a
> mid-session `/model` switch can then never run clamped below its real window.
> The launched model's own limit rides the native `autoCompactWindow` setting
> instead, written into the generated settings, so the compaction trigger is
> `min(ceiling, launched-model wall)`.
>
> That trigger is fixed for the session's life, which makes a switch DOWN fatal
> rather than merely inexact: a model whose wall sits under the trigger can never
> compact, so it runs into the gateway's over-limit error — and by then the
> transcript no longer fits in a `/compact` summarization request either, so the
> session cannot be recovered at all.
>
> **The scalar is a cap, not the window.** `autoCompactWindow` is one integer per
> session, but the engine never uses it directly: `CL()` returns
> `min(ceiling(model), scalar)`, and `ceiling(model)` is recomputed **from the
> model in force on every call** — `[1m]` → 1e6, any other `claude-` id → 200,000,
> a non-claude id → `CLAUDE_CODE_MAX_CONTEXT_TOKENS` **[src `zE`]**. So one scalar
> already yields per-model windows. In a session capped at 1,000,000, a switch to
> `claude-haiku-4-5` self-corrects to a 200,000 window and a 144,000 trigger, and
> `claude-opus-5[1m]` sits at 1,000,000/784,000 — each at ~78% of its OWN ceiling,
> natively, with nothing from ccmorph.
>
> The gap is narrower than "one window per session" suggests, and it is entirely
> in the **non-claude** family: every routed model shares the single env ceiling,
> so `min()` cannot tell a 700,000-token model from a 1,048,576-token one. There is
> no general per-model hook to fix that with — `DM()` is hardcoded to
> `claude-sonnet-4-6`, `modelOverrides` maps only the four tier aliases, and the
> engine's own per-model map is gated on a first-party connection
> (`if (k() !== "firstParty") return null`, which is why `~/.claude.json` carries
> `"autoCompactWindowsCache": null`). Subagents are unaffected either way: each
> query carries its own window.
>
> **Therefore the default names, and does not hide.** The exposure is one family
> and one direction: a switch to a *smaller non-claude* model. Making it safe would
> mean lowering the scalar to that model's wall, which taxes every large model in
> the same session — on a 1M-to-200k route that is 640k of headroom surrendered to
> protect a model the user may never select. Hiding it instead costs them a model
> they chose on purpose, often a reviewer. Both are worse than telling the truth:
> the launch advisory names each risky model with the exact token count past which
> it stops compacting, largest first, because the useful warning is the 700k model
> that dies at 700k and not the 258k one nobody would take into a long session.
> Below the trigger every model is safe; the person holding the session is the one
> who knows whether it will grow past that. `CCMORPH_WITHHOLD_FATAL=1` switches to
> removal (`_ccm_withhold_fatal_switches`, generated settings only — the layer always
> keeps every model, and a session launched on a smaller one offers the whole route
> again).
>
> The remaining direction is safe: switching to a LARGER model compacts early, and
> becomes exact when explicitly selected for a future launch.
> `CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1` stays set because the
> engine's own enforcement would clamp a routed model to its guessed default
> rather than the probed one.

`CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` is removed from codex. It enters as
`min(usable − usable×reserve, usable×pct/100, usable − 13000)` **[src]** — a ceiling,
never a floor, so the 90 it carried could only ever compact *sooner*.

ccmorph ships **no display machinery** for any of this. A v1.7.x statusline
(display-only — it repainted a number the engine never used) was removed; a
leftover `statusLine` entry it wired into a profile is cleaned up by
`_ccm_unwire_statusline` at provisioning, and a statusLine the user set
themselves is never touched. What the engine budgets, its own surfaces
(`/context`, `/autocompact`, the built-in indicator, any statusLine the user
runs) display natively.

Why the settings key and not the env var: the binary consults
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` BEFORE the settings layer on every
resolution, so an env pin permanently shadows the user's own `/autocompact`
command. The derived budget therefore enters as the native `autoCompactWindow`
setting in the generated settings — filled only where no valid value exists, so a
value the user saved (canonical settings, or `/autocompact`, which lands in the
generated file and is folded into the profile layer) always wins. A launch on a
model other than the profile default carries its budget in the environment instead
(see [Launch contract](#launch-contract-arguments-pass-through)). A conf that pins `env.CLAUDE_CODE_AUTO_COMPACT_WINDOW` keeps env
semantics: an author override outranks everything, including `/autocompact`.

Mid-session, the running engine re-reads nothing from disk — verified live
(2.1.246): an external `settings.json` edit is invisible to the session, across
`/model` switches and turn starts; only `/autocompact` itself (which patches the
flag layer in-process) moves the trigger inside a session. So per-model
exactness is delivered at launch, the ceiling covers every switch target, a
model the session's budget cannot compact is named by the live advisory (and
withheld only under the opt-in `CCMORPH_WITHHOLD_FATAL`) rather than hidden by
default, and a switched-into model becomes exact again when explicitly
selected for a future launch.

Context limits are configuration or discovered data, never constants in the
source. the internal model-window resolver sets or removes an
exact manual override in the profile `.conf`. That same `.conf` also holds
`context_discovered`, a JSON blob keyed by the profile's current route
fingerprint that `ccmorph probe` writes. A tier alias (`opus`, `fable`) resolves
through the profile's own `ANTHROPIC_DEFAULT_<TIER>_MODEL` to the model that
actually answers. The only numbers in `ccmorph.sh` are `_CCMORPH_ACW_MIN` /
`_CCMORPH_ACW_MAX`, which are Claude Code's own schema bounds on the setting
(`.int().min(1e5).max(1e6)`) — a value outside them is dropped silently, which
would put the session back on the default budget.
`test_no_context_window_is_written_into_the_source` fails the build if a window
literal reappears.

`ccmorph probe <account>` is metadata-only by default. It spends 0 inference
tokens, runs under a 30s whole-run deadline with `min(1s, 10%)` reserved for
cleanup, and records positive integer gateway fields even when their values are
equal. It preserves an existing discovered value for a model already known on
the current route; if the listing now states a
different value for that same profile/model, `context_discovered` records a note and
waits for explicit retest rather than guessing which value is right. A route
change (endpoint or credential) invalidates the whole
`context_discovered` blob for that profile, so stale data from a retired route
can never masquerade as proof for the new one.

Inference probing is a paid last resort: `ccmorph probe <account> --model <id>
--allow-inference` requires a single profile/model pair, skips already known
windows unless `--probe-all` is also set, and uses a 10s per-wait
`--probe-timeout` by default (each socket wait, not the whole exchange; the run's
`--deadline` bounds the total). `--bisect` additionally enables hidden-ceiling
search and requires `--allow-inference`; only an actual context overflow counts
as the ceiling, not generic errors or timeouts. Rejections and timeouts are not
guaranteed free, and the first accepted prompt itself can be billed; the request
uses character estimates up to 6,000,000 characters, not a known token count. No
live paid probe evidence exists for this change.

## 5b. Choosing a model

Claude Code can list what the endpoint serves. It is off by default:

```
env.CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
```

With it set, Claude Code GETs `/v1/models?limit=1000` at bootstrap and adds the
results to the `/model` picker, labelled `From gateway`, with the active one shown
as `Custom model`. Unset, it logs `[Bootstrap] Skipped gateway /v1/models
(CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY not set)` and the picker shows only the
built-in catalogue — which is why a routed profile appeared to offer Claude
subscription models and none of its own.

Verified on both routes **[wire]**: gateway lists its gateway models
(`claude-opus-4-8 ✔ From gateway`, `… +30 models`), and codex lists `:<translator-port>`'s
(`GPT 5.6 Sol`, `GPT 5.6 Terra`, `qwen3-coder`). Enabled in the conf for
`gateway`, `codex` and `qwen`. Cost is one request per launch.

`modelOverrides` does not help and should not be proposed again: it maps an Anthropic
id to a provider-specific id, which would surface gateway models under Claude names —
an alias namespace that drifts against the endpoint's real list **[src]**.

### The picker rows are projected, not discovered

Claude Code builds a row three ways and only one lets a description through **[src]**:

| Source | Label | Description |
|---|---|---|
| built-in catalogue | its own | its own |
| gateway discovery (`u5r`) | **ours**, from `display_name` | hardcoded `"From gateway"` |
| `additionalModelOptionsCache` (`F1e`) | **ours** | **ours** |

The third reads `{value, label, description}` back verbatim from the profile's own
`.claude.json`, and was already in use there — the stray `Fable` row came through it.
So `_ccm_project_picker` writes the rows at every launch and discovery stays **off** for
such a profile: discovery is added first, and its hardcoded string would win the dedupe.
Keeping discovery off also keeps it out of the launch/add critical path; the background
helper may refresh the cached model inventory for the next launch.

A row's description names the route it came from. A profile declares `tag=<name>` in its
conf (for example, `Primary` or `Alternate`); the launcher passes it to `_ccm_project_picker`,
which stamps it on every projected row. A profile that declares none falls back to the
concise built-in `Gateway`, so an untagged route still reads honestly **[test]**. This is
a bootstrap display value, not a capability claim — it says where the model comes from and
infers nothing about what it can do.

Everything else in a row is derived. Ids and names come from the endpoint as it answers
now, and **every chat-capable model it serves is offered, probed or not**. A row used to
require a ceiling — from the endpoint's declaration or from discovered data — and was
dropped otherwise, so as never to promise headroom a model lacks. That reasoning was
inverted: with no window, `model-window` falls to its last rung and the session gets Claude
Code's 200,000, the *smallest* budget available, so such a row under-promises and cannot
overflow. What the gate cost was every model added since the last successful probe,
and every model whose provider refuses the probe at all — a permanent condition, not a
temporary gap. Probed on one gateway 2026-09-08: **43 served, 30 offered**; four models
were invisible for having no discovered entry, and one for having an entry bound to a
different
route. Existence is the endpoint's fact, capacity is `model-window`'s, and fusing the two
questions into one `if` answered neither. A model whose window is genuinely too small to
compact remains selectable and is handled downstream, where the real budget is known: the
live launch advisory names it, and `CCMORPH_WITHHOLD_FATAL=1` removes it from that session
only. Note the split precisely — both of those read a **probed** wall (`_ccm_hazard_set`
skips any model without one), so they speak about known-and-small models; an *unprobed*
model is covered instead by the unprobed advisory and conservative compaction fallback below. Projection is a full replacement, like the MCP projection,
so a withdrawn model stops being offered instead of lingering — and an unreachable
endpoint leaves both files untouched, because yesterday's rows are still true and a
model list changes far less often than a network does **[test]**.

A row's value is the model's **real id**, not the disguised one. The disguise exists so
gateway discovery will list a non-Claude model at all — it filters ids on
`^(claude|anthropic)` before adding them — and a projected row skips discovery entirely.
Carrying the disguise there is actively harmful: a value starting with `claude-` makes
Claude Code skip `CLAUDE_CODE_MAX_CONTEXT_TOKENS` and cap the session at **200,000**
however the profile is configured, so a row that promised a 372k window delivered 200k,
and the compaction trigger fell from 299,200 to 153,000 with it. Both endpoints route the
real name **[wire]**.

That mattered more than a wrong label. At a 200,000 window the gap between the trigger
and the blocked threshold is 44,000 tokens, so any single injection larger than that —
a big file read, or the 209k `claude-api` skill — clears both in one message and the
session hits `Context limit reached` without ever compacting. Restoring the real window
widens that gap to 69,800; it does not close it, and no window value can.

The picker reads the real gateway catalogue before inference is redirected to the
adapter. Projection finishes before the session budget and generated settings are read.
An incomplete protocol cache therefore cannot truncate model visibility, including on
first launch. Every valid model ID without an explicit `chat_completion: false` is
shown; model-name guesses and missing context metadata do not exclude rows. Genuine
Claude IDs remain visible too, with a derived `[1m]` suffix only when supported by the
context resolver.

`availableModels` remains the generated Claude-facing projection, not a protocol
allowlist. A listed model may still fail inference if the gateway's capability claim
is wrong or its protocol has not been classified. Classification remains separate;
listing does not probe every model or promise every model accepts `/v1/messages`.

A ceiling comes from local data at **launch**: `_ccm_launch_window` reads
the profile's manual override first, then its `context_discovered` entry for
that exact model, then a glob match within the same blob.
This rule once also governed which rows the picker offered; it no longer does,
since a row needs no ceiling. It still decides every window. When a metadata refresh conflicts with
an existing discovered value for that model, ccmorph keeps the existing value, discloses the
listing's different claim, and requires an explicit retest before replacing it.

An unprobed launch gets the resolver's **conservative compaction fallback** through
`autoCompactWindow`, while the process retains the route-wide context ceiling.
Clamping that environment ceiling to the fallback also clamped every later `/model`
choice, even when the switched-to model had a known larger limit. Both known and
unknown launches now narrow compaction through the same native setting **[test]**.
The ceiling is not a claim about the unknown model's upstream limit. Compaction
remains session-wide after a switch: launch directly with the desired model for its
exact budget, or explicitly adjust native `/autocompact` when appropriate.

A model stored from before the rows carried real ids is **rewritten** to its real name,
not merely tolerated. Otherwise the profile keeps the 200,000 window until the user
happens to re-pick, which is not a thing anyone would think to do. Both ids address the
same model, so nothing changes but the window.

The conf's model **and whatever a live session has already chosen** are kept in the
allowlist. Dropping either does not refuse it: Claude Code *remaps* an unlisted model
onto its own catalogue, which a routed endpoint has never heard of — which is how a codex
session came to ask `localhost:<translator-port>` for `claude-opus-5` and take a 502. Replacing the
list under a running session is exactly when this bites.

The profile's own model is kept in the allowlist whatever shape it has. codex's
`model=gpt-5.6-sol` is an id the endpoint only ever lists in disguise, so writing the
allowlist from the projection alone removed it — and a model outside the allowlist is
not refused but *remapped*, onto Claude Code's own catalogue, which a routed endpoint
has never heard of **[test]**.

Both routed profiles are projected **[wire]**: codex renders 13 rows including all four
effort variants (`GPT 5.6 Sol (xhigh)`), gateway its own set (`glm-5.2`), each tagged
with the profile's route. The label is the bare model name — a `· 700k` suffix went
stale the day a route was reprobed, and the session budgets the real window
engine-side anyway. `availableModels` is written from the endpoint inventory, not the
discovered-window data, so newly served chat-capable models are selectable before any paid
ceiling probe. Known sub-trigger models are still named by that invariant check, and
`CCMORPH_WITHHOLD_FATAL=1` can remove them from that launch's generated settings.

### Hiding the models the profile cannot serve

Discovery only ever **adds** rows, so a routed profile's picker still offered the
whole Claude subscription catalogue. `availableModels` in the profile's
`settings.json` removes them: Claude Code drops every picker row outside the list.
The schema calls it admin-owned ("typically set in managed settings"), but the user
tier is read — verified by a request that the allowlist refused **[test]**.

| Profile | `availableModels` | Why |
|---|---|---|
| `codex` | three tier aliases plus five explicit `gpt-*` ids | eight entries; no longer uses the cloak prefix below |
| `qwen` | `qwen2.5:32b` | exact tag; hand-curated, not projected |
| `gateway` | tier aliases plus the projected served ids | mirrors the endpoint; 21 entries as projected |
| `profile` | the projected served ids, plus `fable` | same shape, no bare `opus`/`sonnet` |

A permitted bare family alias shadows the explicit `[1m]` row for that family, so a
profile wanting a 1M Claude launch must not list both (§ what a tier mapping does).

Entries match on a `-` boundary, so **one prefix can cover a whole cloaked namespace**:
`claude-fable-5-dd` admits every disguised id the translator serves without naming any of
them, and stops matching the moment its catalogue changes shape rather than silently
going stale **[test]**. The cloaking itself is still live and pinned by the suite; the
`codex` profile simply no longer relies on the prefix, listing explicit ids instead.

Three sharp edges, each of which cost a wrong answer here:

- **The list must contain the profile's own model or its pinned family alias.** A model
  outside both is not refused, it is *remapped* — and the remap walks Claude Code's
  built-in catalogue, not the endpoint's, so the session dies on `502 unknown provider
  for model claude-opus-5` **[test]**. Both gateway profiles therefore list the
  explicit `[1m]` ids their endpoint serves; `gateway` also still permits the bare
  `opus`/`sonnet` aliases, which now resolve to those same served ids unmapped.
- **The suffix is invisible to matching.** `[1m]` is stripped before comparison. A
  permitted pinned family alias therefore owns the picker row and suppresses explicit
  versions from the same family; otherwise the 200k plain row reappears beside it.
- **Default always survives, and it is not the configured default.** The one row an
  allowlist can never remove. It resolves through Claude Code's own unpinned default,
  which need not be the id `model=` launches — `ANTHROPIC_MODEL` establishes launch
  precedence, but the row stays selectable, so an unserved resolved id is a routing
  failure waiting to be clicked, not merely misleading text. A tier mapping is the only
  lever over it, so one is warranted when that id is unserved.

### Pinning a row the endpoint cannot advertise

```
env.ANTHROPIC_CUSTOM_MODEL_OPTION=<id>
env.ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=<label>
env.ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=<subtitle>
```

Adds exactly one row, deduped against the built-in catalogue. Two jobs nothing else
does. `qwen` gets a named row at all: ollama advertises `qwen3-coder:64k`, discovery
filters ids on `^(claude|anthropic)`, so without this its picker held nothing but
Default.

### What a tier mapping does, and the window it does not buy

`env.ANTHROPIC_DEFAULT_<TIER>_MODEL` names the model a tier alias resolves to on this
route. Probed against client 2.1.274 through a local recording stub, the **unmapped**
aliases send `opus` → `claude-opus-5`, `sonnet` → `claude-sonnet-5`,
`fable` → `claude-fable-5-1`, `haiku` → `claude-haiku-4-5-20251001` **[wire]**.

Two distinct reasons to set one:

- **Servability.** The client's own target for that alias is not served by the route, so
  the tier is dead without a mapping. Neither gateway serves `claude-fable-5-1`, which is
  why `fable` carries one on both.
- **Task scope.** The operator wants the tier to *mean* a model chosen on merit from the
  whole catalogue. This is a deliberate choice, not a defect, and it is what both gateway
  profiles do — a picker row's family name is then not the answering model's family. The
  cost is exactly that mismatch: everything resolving the alias, including helper calls
  and subagents, answers from the mapped model whatever the row is called.

**A mapping does not buy the `[1m]` window, and it is important not to believe it does.**
For a `claude-*` id the window is chosen by the id: `[1m]` reads 1M, a bare `claude-`
prefix takes a hardcoded 200,000 and ignores the env ceiling. ccmorph derives the
suffixed `send_id` only when the **launch target itself** starts with `claude-` — the gate
`"$_send_target" == claude-*` in `ccmorph-launch.zsh`; a bare alias never does, and the mapped
target is read separately, for budget sizing alone, where `_pmodel="${_palias[$_pbare]}"` in
`ccmorph-launch.zsh` swaps the alias for the model behind it. So mapping `opus` → `claude-opus-5`
leaves a `--model opus` launch on the 200,000 family ceiling — the mapping changes which
model answers, not which window it gets. Two further limits: the tag is inert under
`CLAUDE_CODE_DISABLE_1M_CONTEXT` **[src]**, and an explicit `claude-opus-5[1m]` row is
suppressed in the picker whenever `availableModels` also permits the bare family alias,
since the suffix is invisible to matching (§ sharp edges above). A 1M Claude launch
therefore needs the explicit suffixed id *and* an allowlist that does not shadow it.

Dropping a mapping costs one thing beyond the tier's meaning: a bare-alias launch
(`--model opus`) is refused outright with nothing to resolve. That refusal covers
non-Anthropic `apikey` routes, which take the adapter path regardless of how the
endpoint is reached — but **not** `type=provider` profiles, which set `_is_provider`
without `needs_adapter` and proceed to the bridge. Where a launch does proceed, an
unresolvable alias leaves no exact model to size `autoCompactWindow` from; a saved budget
or a computable route ceiling still applies, but a profile offering neither falls back to
the unprobed 200,000 default. Explicit-id launches are unaffected throughout.

Complements, still useful:

| Need | Do this |
|---|---|
| Everything an account is set to | `ccmorph inspect <account>` |
| Choose at launch | `ccmorph <account> -i` |
| Save a default you cannot spell from memory | `ccmorph set <account> model -i` |
| An id the picker cannot show, e.g. an effort suffix | `/model <exact-id>` |

`/model <id>` accepts an id outside the catalogue. Verified: a session on
`gpt-5.6-sol` switched via `/model gpt-5.6-terra`, and the next request carried the
new id **[wire]**. The switched-into model runs under the route ceiling for the
rest of the session. An explicit launch with that model gets its exact budget;
inline selection does not change the next launch's configured default.

The confirmation dialog names the real cost: the conversation is prompt-cached per
model, so switching re-reads the full history on the next message. Cheap once,
expensive if repeated.

### Model defaults belong to ccmorph; inline choices belong to the session

The profile `.conf` owns configured `model`, `effort`, `effort.<model>` and model
tier/helper mappings. `ccmorph set <profile> model|effort` changes future defaults;
advanced values are edited directly in the same file. Unused fields stay absent.
Other native settings remain in profile `settings.json` over shared settings.

Before the first interactive launch, the transactional configuration writer imports
existing effective native model settings once, retaining explicit conf values.
`model_defaults=1` marks the ownership transition, including intentionally absent
defaults. Subsequent native writes are not re-imported. Explicit setters use the
same transaction, and read-only inspection does not mutate a profile.

Claude Code's `/model` and `/effort` still write native preferences and may display
“saved as default.” Those writes are not the ccmorph defaults. Launch preparation
first seeds the conf if needed, then transactionally removes only native model,
effort, per-model effort and model-mapping preferences from the profile's
settings layer. Unrelated fields and shared settings stay untouched; concurrent
writes detected during preparation abort rather than overwrite changed data.
The generated `settings.json` supplies the configured model values.

Native user discovery stays enabled: disabling the user settings source also
hides shared skills, commands and output styles. Project/local sources and managed policy remain in force. The persistent
config directory, authentication and shared transcripts are unchanged. Inline
choices remain native session state; no exit-time restoration or continuous
synchronization is required, and they never overwrite the conf defaults.

The same default readers serve launch, listing, alias resolution and inspection.
Defaults apply across credential types. Explicit native launch flags override them
for one invocation. Configured per-model effort retains native precedence over a
general effort default, without adopting per-model effort written by an earlier
interactive session. Adapter-only controls stay outside Claude's settings.

The guarantee applies to ccmorph-launched profiles, not an independently launched
native `claude` executable. Installation does not take ownership of that command.
The optional `tests/native_session_defaults_smoke.py` exercises actual native TUI
changes, shared-skill discovery and effective settings across sequential and
concurrent launches, using an isolated home and synthetic authentication without
model inference.

Two defects of the undeclared kind, found by a three-way adversarial audit at v1.15.2
and fixed in v1.15.3:

- **A default may fill a gap, never overrule a choice.** The compaction trigger comes
  from the fleet manifest `~/.agents/runtime/policies/context.json`; ccmorph carries
  that default into every Claude profile only when neither the profile conf nor either
  settings layer declares an engine-valid `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`. Both layers
  are consulted before injecting **[test]**. An absent manifest uses the built-in 75%; a
  present invalid manifest stops an unoverridden launch. The general trap: proving "the user
  did not set this" has to happen *before* the write, because merge order will not do it
  for you.
- **Ownership is proven by exact path, never by a suffix.** `_ccm_unwire_statusline`
  matched `command.endswith("/ccmorph/statusline")` — a strictly broader class than
  ccmorph ever wrote, so a personal bar at `~/bin/ccmorph/statusline` was deleted
  while the function's own comment promised the opposite. The v1.7 wire function it
  cleans up after wrote an exact path *and* refused to replace an existing command, so
  an exact match cannot miss anything ccmorph installed; both the state-dir and
  lib-dir spellings are checked, since a pre-1.8 profile may hold either **[test]**.

One finding was deliberately **not** fixed, recorded so it is not rediscovered as new:
`set_allowed` replaces `availableModels` wholesale, and for routed profiles that is
declared — the table above gives gateway's list as tier aliases plus the projected
served ids, withdrawal semantics are stated at `_ccm_project_picker`, and
`test_projection_never_drops_the_profiles_own_model` pins what survives — and the
launch's own `--model` survives with it when the shim serves it, because a hop
relaunches on a `model_map` alias the endpoint never lists and dropping it silently
remapped the session onto Claude Code's own catalogue: a map key is kept, and stays kept
past the family-alias rule above, which would otherwise shadow the explicit row — an
unmapped id is not, pinned by `test_relay_relaunch_keeps_a_mapped_alias_allowed`
**[test]**. The real gap
is that the *hand-curated* regime this document names is not enforced in code: `qwen`
declares `base_url`+`api_key`, so it sits inside the destructive path, protected today
only by coincidence (its endpoint serves no above-trigger probed model, and its one
curated entry equals the stored model). Latent, not live — no profile loses data as of
v1.15.3. The fix, when a second curated entry appears in a routed profile, is a conf
declaration in the shape of `adapter=true`: projection-owned by default, hand-curated
makes ccmorph read-and-warn.

## 5c. Why the picker can show five identical rows

Investigated because codex's `/model` listed "GPT 5.6 Sol" five times. The cause is
none of the places first suspected, and the chain is worth recording whole.

**Claude Code does no formatting.** A discovered row is
`{value: id, label: display_name || id, description: "From gateway"}` — taken
verbatim from the models list, with no registry lookup, regex, or family rule
**[src]**. There is no setting, env var, or plugin hook that changes a label.

**`/v1/models` is a split route.** An `anthropic-version` header or a `claude-cli`
user agent returns the full model records **including `display_name`**; any other
caller gets a response stripped to `id`/`object`/`owned_by`/`created` **[src]**. A
plain `curl` therefore shows no display name and invites the wrong conclusion —
Claude Code is on the rich branch and does receive one.

**Non-Claude ids arrive disguised.** Discovery keeps only ids matching
`^(claude|anthropic)`, so the translator rewrites others as
`claude-fable-5-dd-<real-id-reversed>` — `claude-fable-5-dd-hgihx-los-6.5-tpg` is
`gpt-5.6-sol-xhigh` **[wire]**. Two consequences: the id in the picker is not the id
you type, and any row whose id contains `claude-fable-5` is relabelled
"Fable (disabled)" while a credits gate is active **[src]**.

**The collapse is one instance's defect.** `:<translator-port>` declares five
`oauth-model-alias` entries with `fork: true`; the fork path clones the model record
and rewrites only `ID`, never `DisplayName`, so all five inherit the registry's
single `"GPT 5.6 Sol"` **[src]**. Fixing it at source needs both a root-owned config
edit and cliproxyapi ≥ 7.2.113, since 7.2.70's alias struct has no `display-name`
field.

The ccmorph adapter is not affected: it emits `name == alias == real id` and no
`display-name`, so labels fall back to real ids and are already distinct. Adding
effort variants there would mean minting ids the endpoint does not serve, against a
live-probed inventory — rejected.

**The usable lever is `availableModels`**, in a profile's own
`<state root>/profiles/<name>/settings.profile.json` layer, rendered into the generated
`settings.json`. Verified **[wire]**: it is honoured (profile layer wins per key), it
accepts non-Claude ids, it filters at exact-id granularity — five
identical rows reduced to one — and it does not leak to sibling profiles.

Two traps probed while confirming it:

- The list must contain the profile's own configured model string. With only the
  disguised picker id present, the launch model is dropped and the session silently
  falls back to Default.
- `Default (recommended)` is never filtered, even by an allowlist naming only
  nonexistent ids.

Related: a profile's layer (`settings.profile.json`) persists and shadows canonical
keys for as long as it holds them; `settings.json` itself is generated.

### How it was fixed, 2026-08-02

Two defects, one change.

**The dash aliases were already working — via a different mechanism than expected.**
`:<translator-port>` declares four `oauth-model-alias` entries — `gpt-5.6-sol-low/-medium/-xhigh/
-max`, each `name: "gpt-5.6-sol"` with `fork: true`. `ParseSuffix` reads only a
**parenthesised** suffix, so the dash names carry no level *by that route*; the
effort comes from the same config's `payload.override` rules, which match
`*-low`/`*-medium`/`*-xhigh`/`*-max` by glob and set `reasoning.effort` directly.
`payload.default` sets the base `gpt-5.6-sol` to **high**.

That distinction cost a wrong change: the aliases were briefly rewritten to the
parenthesised form, which silently orphaned the override globs. Reverted. Read the
whole `payload:` section before touching an alias name — the alias and the rule that
gives it meaning are far apart in the file.

**The labels collided** because the fork path cloned the model record and rewrote
only `ID`, leaving every clone with the registry's single `"GPT 5.6 Sol"`.

The fix required a version bump: `display-name` on `OAuthModelAlias` does not exist
in 7.2.70 and lands in 7.2.100 **[src]**. Upgraded 7.2.70 → **7.2.110** via brew;
the model list stayed identical at 14 and all request forms still answered 200.

The whole change is therefore four added lines — one `display-name` per alias, with
the dash names left exactly as they were so the override globs keep matching:

```yaml
    - name: "gpt-5.6-sol"          # matches the existing model id
      alias: "gpt-5.6-sol-max"     # outward id — matched by the *-max override
      display-name: "GPT 5.6 Sol (max)"
      fork: true
```

Verified **[wire]**: the Anthropic branch of `/v1/models` returns six distinct sol
entries with distinct `display_name`s; the obfuscated ids Claude Code actually sends
(`claude-fable-5-dd-)xam(los-6.5-tpg` — reversed, parens included) round-trip 200 and
resolve to `gpt-5.6-sol`; and the picker renders `GPT 5.6 Sol (xhigh)`.

Scope limit: the suffix-to-`reasoning.effort` conversion was captured on the gateway
upstream, not this one — `:<translator-port>`'s egress cannot be intercepted. Same binary and
code path, but it is inference, not a second observation.

Operational notes:

- The edited file is `/opt/homebrew/etc/cliproxyapi.conf` — outside any repo, so it
  is not version-controlled. Backup of the pre-change state:
  `~/.config/ccmorph/backup-cliproxyapi.conf.20260802` (mode 600).
- Rollback is the cached 7.2.70 bottle; the Cellar keeps only the current version.
- `~/.config/ccmorph/profiles/<name>/cache/gateway-models.json` must be deleted after any
  change to the advertised set, or the picker serves the stale list. It is keyed on
  base URL only.

## 6. Effort

Claude Code sends the level as **`output_config.effort`** alongside
`thinking: {"type":"adaptive"}`, with `effort-2025-11-24` in `anthropic-beta`
**[wire]**. Every adapter launch sets `CLAUDE_CODE_ALWAYS_ENABLE_EFFORT=1` to
expose the `/effort` selector; a profile's own `env.` value overrides it.

**Both fields are required.** The translator reads `output_config.effort` only
inside `case "adaptive","auto"` of `thinking.type` **[src]**. Sent alone it is
never looked at and effort defaults to `medium`; adaptive thinking with no effort
is hardcoded to `xhigh`. Vary one field while omitting the other and the
probe is of a constant.

| Dialect | Effort | How |
|---|---|---|
| `responses` | yes | → `reasoning.effort`, seen leaving for the gateway as `{"effort":"low"}` / `{"effort":"max"}` **[wire]** |
| `anthropic` | yes | passes through untouched |
| `chat` | sent | → `reasoning_effort` **[src]**; this gateway's two chat models are qwen3.5 and ignore it **[wire]** |
| `chat-noreason` | **no** | `payload.filter` strips the parameter by design |

One exception on every dialect: when `tool_choice.type` is `any` or `tool`, the
executor deletes `thinking` and `output_config.effort`, because the Anthropic API
forbids thinking with a forced tool call **[src]**. Correct behaviour, but your
effort selection is dropped on those turns.

### Effort by model id — the form that survives a model switch

cliproxyapi reads a **parenthesised** suffix on the model id and turns it into
`reasoning.effort`. Verified end to end with no `thinking` and no `output_config`
in the request, so the id alone produced the effort **[wire]**:

| requested id | upstream model | `reasoning.effort` | upstream |
|---|---|---|---|
| `gpt-5.6-sol` | `gpt-5.6-sol` | `medium` | 200 |
| `gpt-5.6-sol(low)` | `gpt-5.6-sol` | `low` | 200 |
| `gpt-5.6-sol(max)` | `gpt-5.6-sol` | `max` | 200 |
| `gpt-5.6-sol-low` | forwarded unchanged | `medium` | **404 on the gateway** |
| `gpt-5.6-sol-max` | forwarded unchanged | `medium` | **404 on the gateway** |

Three things follow.

**The default is `medium`.** A routed session with no effort set anywhere runs at
medium, not high. `reasoningEffort := "medium"` is the initialiser in
`codex_claude_request.go` **[src]**.

**Dashes are not a cliproxyapi suffix, but they are not inert either.** The gateway
has no such ids, hence the 404 above. On the codex route they return **200** and
resolve upstream to `gpt-5.6-sol`, because `:<translator-port>`'s config declares them as
`oauth-model-alias` entries with `fork: true` **[src]**. So on that route the dash
forms are working effort aliases; on gateway they do not exist. Do not generalise
either way from one route.

**Parentheses survive Claude Code.** `model=gpt-5.6-sol(max)` in a conf, or
`/model gpt-5.6-sol(max)` mid-session, is accepted and sent verbatim **[wire]**.
This is the only effort mechanism that survives a model switch, because the level
*is* the id — where `/effort` is session state that a `/model` change can outlive.

Confirmed on the real routes, not just a hand-built config: both the brew `:<translator-port>`
and a live ccmorph adapter return 200 for `gpt-5.6-sol(low)` and
`gpt-5.6-sol(max)` **[wire]**. This mattered because each route builds its model
allowlist from the endpoint's `/v1/models`, which advertises no parenthesised
forms — the suffix is stripped before that match, so the allowlist is not a
barrier. The suffix-to-`reasoning.effort` conversion itself was captured on the
gateway upstream; the codex upstream cannot be intercepted, but it is the same
binary and the same code path.

Caveat: a parenthesised id matches neither Claude Code's catalogue nor a
`context_discovered` glob, so it budgets at `CLAUDE_CODE_MAX_CONTEXT_TOKENS` — the
route ceiling on `gateway` and `codex`, which both derive one, and Claude
Code's conservative 200,000 on a profile that does not.

### Do not verify effort with token counts

Per-request variance swamps the effect. `low` versus `max` gave medians of 166
and 178 output tokens over five trials each — a null result regardless of the
truth. Capture the outbound request instead: put a recorder between cliproxyapi
and the upstream and read the `reasoning` object.

## 7. Known gaps

### Optional server-side tools are route capabilities

An Anthropic-shaped endpoint accepting ordinary tools does not prove that it
implements Anthropic's server-side tools. On 2026-09-18, bounded direct requests
with `claude-opus-5` succeeded for text and a custom-tool/result round trip on
both tested gateways. Adding `advisor_20260301`, even with
`anthropic-beta: advisor-tool-2026-03-01`, failed before inference: one route
reported a Bedrock unsupported-tool error; the other rejected the tool-type
union. These requests bypassed the local adapter entirely **[wire]**.

Every adapter launch therefore sets `CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1`; a route
that does serve the advisor declares `env.CLAUDE_CODE_DISABLE_ADVISOR_TOOL=0`. This native Claude Code
control prevents the optional advisor from being offered; ordinary tools remain
available. ccmorph accepts and isolates the control, but does not infer advisor
support from model names, strip arbitrary tool definitions, or emulate the
server-side consultation. Restoring advisor requires upstream support for its
schema, beta, result blocks and accounting, followed by a new route test.

Claude Code can add `thinking.<adaptive|enabled>.block_binding.prefix_mismatch_behavior` to a
request, and cliproxyapi (8.0.35+) sets it on Claude 5.5 requests whose history replays signed
thinking. The gateway rejects the field with a 400 (`Extra inputs are not permitted`) and masks the
path (`thinking.` becomes `***.`), so Claude Code's fallback never matches. The adapter's translator
config therefore filters `thinking.block_binding` for the Claude passthrough, last in the chain.

**Nested launches.** Every launch clears the route flags, so a `ccmorph run` started
inside a Remote Control bridge session would reach the session's route (its loopback
`HTTPS_PROXY` is inherited) without the advisor opt-out and be rejected with a 400.
The add-on's session sets `CCMORPH_EDGE_ROUTE_ENV` (`KEY=VALUE` per line, the
effective `CLAUDE_CODE_DISABLE_ADVISOR_TOOL` and `CLAUDE_CODE_MODEL_CAPABILITIES`,
defaults included). A launch re-applies exactly those lines after clearing, only when
`HTTPS_PROXY` is `http://127.0.0.1:<port>`, the profile has no `ANTHROPIC_BASE_URL`
of its own and is not a provider **[test]**. The variable is the whole interface;
core reads no add-on file.

The flag and its boolean parsing were checked in Claude Code 2.1.276. The launch
regression checks delivery to the child and absence of cross-profile leakage.
`python3 tests/native_advisor_smoke.py --claude-bin /path/to/claude` checks actual
request omission on main and Opus subagent calls against an isolated local stub,
with advisor-enabled positive controls. Re-run it after client upgrades; neither
a schema label nor a successful text-only probe establishes complete tool
compatibility.

| Gap | Detail |
|---|---|
| ~~Entry points live outside the repo~~ | **Closed, then removed entirely.** There is now ONE tracked entry point: the `ccmorph` executable at the repo root, which sources the library in its own process. No shell function, no startup-file block, no shadow of the native CLI, and no second command for the mid-session hop — that is `ccmorph hop`. The permission bypass that used to ride along outside review as a silent env-var opt-in is gone; `--dangerously-skip-permissions` only ever reaches a launch as an explicit flag the caller typed **[test]**. |
| No launch preflight for a plain proxy | An adapter profile fails loudly (`adapter for 'gateway' did not become ready`). A plain `proxy` profile never checks its endpoint, so a down backend first appears as a mid-session API error. Low severity: the brew `:<translator-port>` plist carries `KeepAlive: true`, so launchd restarts it — the gap is the error message, not availability. |
| Mid-session `408` | `stream disconnected before completion` appears intermittently on the gpt route. A fault-injection proxy exists; Claude Code's recovery behaviour is uncharacterised. |
| Forced summarisation compact | Never observed. Context would not grow past 47k against the windows in use. |
| The invariant check is only as current as the last probe | `_ccm_invariant_warn` — reached from a launch through `_ccm_launch_diagnostics` — compares each offered model against the profile's own `context_discovered` data at launch, so a widened allowlist is caught. What it cannot catch is a ceiling that changed since it was last discovered, or a model that was never discovered at all — an unknown id is passed over rather than flagged, on the grounds that a check which cries wolf gets ignored. Refresh metadata with `ccmorph probe <account>`; use `--allow-inference` only as an explicit paid last resort. |

## 8. Re-verifying any claim here

| Claim | Method |
|---|---|
| A model's real ceiling | over-limit request, output capped, unique prefix; read the cap from the error |
| Which window Claude Code budgets | launch it and run `/context` or `/autocompact` |
| Whether effort reaches the provider | recorder between cliproxyapi and upstream; inspect `reasoning` |
| What Claude Code puts on the wire | point a profile at a stub that records and returns a valid SSE response |
| A model's dialect | `.adapter-cache/<sha256(base_url)[:16]>.json` (endpoint-keyed, shared) |
