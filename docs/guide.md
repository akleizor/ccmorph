# ccmorph user guide

The full reference behind the [README](../README.md): installation details, profile
settings, model and context-window handling, switching, and state layout. For the
reasoning behind each behavior, see [ARCHITECTURE.md](ARCHITECTURE.md).

## Install details

From a clone: `git clone https://github.com/akleizor/ccmorph && cd ccmorph && ./install.sh`.

Or the one-liner, which works once the repository is public and has a release —
it is private today, so the download returns 404:

```sh
sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"
```

Run outside a source tree, `install.sh` downloads the latest release's tarball
(`releases/latest/download/ccmorph.tar.gz`), unpacks it in a temporary directory
and runs the installer from there, which then takes the same path as a clone.
`CCMORPH_REF` (a branch or tag) downloads the GitHub archive of that ref instead
— `CCMORPH_REF=main` for unreleased code — and `CCMORPH_INSTALL_URL` replaces
the whole tarball URL, for a mirror or a local file. It needs `curl` or `wget`,
and `tar`. The `sh -c "$(curl ...)"` form keeps
the terminal attached, so the optional installs below can ask. Piping
(`curl -fsSL ... | sh`) works too, but there is no terminal to ask on: the
installer skips those offers and prints the commands to run yourself.

That is the whole install. ccmorph installs only its own command and runtime.
It never edits shell startup files, including obsolete integration blocks:
it reports those for the user to remove.

**Platforms:** macOS and Linux. Windows shells (Git Bash, MSYS, Cygwin) are
refused with a pointer to WSL; install inside a WSL distribution instead.

**Required:** `zsh`, `python3` 3.8 or newer, and the `claude` CLI — the installer
refuses without them. It never installs them: they need root or belong to another
vendor. It names the command for the package manager it finds (the first of
`brew`, `apt-get`, `dnf`, `apk`, `pacman`, `zypper`) and stops. Claude Code gets
a link to its setup page instead of a command.

**Optional** (the installer explains what each one unlocks and installs on
confirm — never silently, and never without a terminal):

| | Needed for | Without it | Installer |
|---|---|---|---|
| `fzf` | searchable profile and model pickers | numbered choices in a terminal; explicit arguments in scripts | offered through the detected package manager |
| `cliproxyapi` | non-native custom endpoints — automatic dialect translation | only the endpoint's Claude-family models are reachable | offered through `brew` only; elsewhere it points to the project page |

The installer copies the runtime (`lib/`, `libexec/`, `share/`) into `~/.config/ccmorph` and the `ccmorph`
command into `~/.local/bin`. It does not edit your shell config; if
`~/.local/bin` is not already on your `PATH` it says so and prints the one line
to add, for your login shell:

| Shell | File | Line |
|---|---|---|
| zsh | `~/.zshrc` | `export PATH="$HOME/.local/bin:$PATH"` |
| bash | `~/.bashrc` (Linux), `~/.bash_profile` (macOS) | `export PATH="$HOME/.local/bin:$PATH"` |
| fish | `~/.config/fish/config.fish` | `fish_add_path $HOME/.local/bin` |

Every install ends with `Check:  ccmorph doctor` (spelled with the full path
while `~/.local/bin` is off your `PATH`). A fresh install adds
`Next:   ccmorph add <name>`.

## Updating

Run the one-line install again, or `./install.sh` from a fresh clone. **It is an
update, not a reinstall** — it replaces the runtime scripts and never touches
your profiles' settings or credentials, or `local.sh`.
It reports the version it moved you from and names what it preserved, so you can
see that nothing was lost. One exception, and it only adds: if legacy
shared-registry or per-profile cache state is present, install folds it into the
owning profile's `.conf` as `context_discovered` and leaves the original behind
as a `.migrated` backup — your profile gains its own context data, nothing in it
is removed. There is no shell to re-source: the
new code is live on the next run. `./install.sh --uninstall` removes the command
and leaves your profiles alone.

Profile runtime state moved from `~/.claude-profiles` to
`~/.config/ccmorph/profiles`, so everything ccmorph stores now sits under one
root that honours `CCMORPH_STATE_DIR` and `XDG_CONFIG_HOME`. The move happens
once, by itself, on the next install or command — see *Where state lives* below.

Confirm it:

```sh
ccmorph doctor
```

With no profiles yet, `doctor` reports the machine instead of a profile:
the version, `zsh`, `python3`, `claude`, `fzf`, `cliproxyapi`, and whether
`ccmorph` is on your `PATH`. It changes nothing, and fails only when a required
tool is missing or too old. After you add a profile, `ccmorph doctor <profile>`
checks that profile.

Then set up your first profile:

```sh
ccmorph add work         # one-time login or credential paste
ccmorph run              # launch on it
```

The first profile you add becomes the default, so `ccmorph run` needs no
argument. Adding another never changes the default; `ccmorph list` marks it and
`ccmorph use <profile>` moves it. With no profiles, `ccmorph run` tells you to
run `ccmorph add`.

`ccmorph add` asks which kind of profile (see *Profile types* below) and prompts
only for what that kind needs: a subscription opens the normal browser login, an
API key or gateway asks for the key and base URL. Nothing is written outside
`~/.config/ccmorph` and the profile directory.

A fresh install has **no profiles** — like a newly installed app. Nothing is
carried over from the machine it was built on.

If you were handed a working profile by a colleague, copy the `.conf` in but
**use your own credential**. The model picker offers every chat-capable model the
endpoint serves, probed or not, so a copied profile works immediately. Running
`ccmorph probe <profile>` is still worth it — by default it records published
metadata only, without inference tokens — but a model with no known window is
budgeted at Claude Code's 200,000 default. A colleague's discovered context data
(`context_discovered` in that `.conf`) is bound to their route's fingerprint, so
it is ignored the moment your credential or endpoint differs, and reprobes clean
on your own route.

## Profile types

A profile is the unit you choose and isolate. Its runtime state lives under
`~/.config/ccmorph/profiles/<name>`.

| Type | What it holds | Backend |
|---|---|---|
| `sub` | OAuth subscription state | Anthropic |
| `token` | a long-lived OAuth token | Anthropic |
| `apikey` | key presented to the selected endpoint, optional `base_url` | Anthropic or any API-key gateway; custom endpoint translation is automatic |

`proxy` is a legacy alias retained only so existing profile files keep working.
New profiles use `apikey`; endpoint, dialect, network route, model, and effort are
separate settings rather than additional profile types.

A profile is one file, `~/.config/ccmorph/<name>.conf`, holding `key=value`
lines. All `*.conf` are gitignored: they carry credentials and never enter
history.

```sh
ccmorph list               # list profiles; the default is marked
ccmorph inspect [<profile>] # effective configuration and its sources
ccmorph use <profile>      # make it the default for new sessions
ccmorph add <profile>      # create one with guided setup
ccmorph config <profile>   # choose which settings to change
ccmorph login <profile>    # re-authenticate a provider-backed profile
ccmorph rm <profile>       # remove it and its config directory
ccmorph mv <old> <new>     # rename it
ccmorph cred <profile>      # print its credential to stdout
```

`ccmorph --help` shows everyday actions. Use `ccmorph help all` for the full
command list and `ccmorph help <command>` for details. Management commands also
accept `--help`; `ccmorph run <profile> --help` still opens Claude's native help.
Help and version requests do not load host overrides, migrate state, or launch.

In a terminal, `config` asks what you want to change: **model and reasoning** or
**connection/sign-in details**, where applicable. Changing a model does not ask
for a URL or credential; replacing a key does not fetch or ask for models.
Use ↑/↓ to move, Enter to select one option, and Esc to cancel. Sign-in methods,
settings sections, reasoning levels, and yes/no confirmations use one native
selector: warm orange headings, a lavender `❯` selection marker, blank-line
separation, and aligned gray descriptions, matching Claude Code’s dark palette. Profile and **all model lists** use `fzf` for search,
including Claude aliases and the keep/custom fallback when discovery fails.
Both renderers share the same theme. Routine status messages, resume banners,
installer output, and successful doctor checks use the same orange—not green.
Confirmation defaults to **No**.
Text and secret prompts show **Enter Submit · Esc Cancel**. Escape immediately
cancels without saving partial input; Ctrl+C also exits cleanly without a traceback.
Piped input retains its line-based behavior.
Prompts use sentence case; product names retain their casing, and profile names
and model IDs remain literal. Changes save when you finish; cancelling an
unfinished edit leaves settings intact.
The existing piped-input configuration sequence is unchanged for scripts.

### Reusable CLI UI

[`lib/ccmorph-ui.zsh`](../lib/ccmorph-ui.zsh) owns every interactive renderer and theme.
It can be sourced on its own: no profile store, launcher, or external dependency
is needed for native menus. Commands supply content and select `native` or
`search`; they do not implement key handling, cursor movement, or styling.

```zsh
source ./lib/ccmorph-ui.zsh
rows=$'defaults\tModel and reasoning\tDefaults for new sessions\nconnection\tConnection\tEndpoint or API key'
choice=$(ccm_ui_select native 'Settings — demo' "$rows") || return
# Use `search` for a model/profile catalogue; selected IDs alone go to stdout.
ccm_ui_input 'Profile name: ' || return  # result in REPLY
ccm_ui_input 'API key: ' secret || return  # echo disabled before the prompt
ccm_ui_confirm 'Continue?' || return  # native No/Yes; piped input uses y/N
```

`ccm_ui_list` renders the same rows for scripted instructions. `NO_COLOR` disables
styling; `TERM=dumb` and missing `fzf` use numbered choices. The installer ships
this module. Claude Code's own in-session menus are
owned by Claude Code, not rendered or replaced by ccmorph.

Profile pickers offer only usable profiles: `models` lists API-key profiles,
`cred` lists stored keys/tokens, and `login` lists provider-backed accounts.

Use `rm` and `mv` instead of deleting or renaming `.conf` files by hand. Both
manual context overrides and discovered context data live in the one `.conf`, so
these commands move or remove them along with everything else — no separate
registry file, no prefix bookkeeping. The profile's conversation
history is not deleted: it moves into `~/.claude/projects`, where any profile can
still resume a session by id. If a `.conf` was removed manually, `rm` recognizes any
leftover runtime or auth state for that name, lists the exact paths, and asks
**No / Yes** (or `[y/N]` with piped input) before touching them — no manual filesystem surgery needed, and nothing
is removed without that confirmation.

Each profile can carry its own default model and effort, used by every new
session on it. `get` reads one setting; `set` writes one:

```sh
ccmorph get <profile> model             # or effort
ccmorph set <profile> model kimi-k3
ccmorph set <profile> effort xhigh      # low | medium | high | xhigh
ccmorph set <profile> model -i          # choose from what the endpoint serves
ccmorph set <profile> model             # a missing value asks the same way
ccmorph set -i                          # choose the profile too
```

`-i` — or simply omitting the value — asks instead of failing. A model id cannot
be enumerated in advance, since a gateway's listing changes without ccmorph
knowing, so being asked is the only reliable way to spell one correctly; the
model list comes from the endpoint itself, and effort from Claude Code's four
levels. `-i` works the same way on `run`, `set`, `get`, `context`, `inspect`,
`use`, `login`, and `cred`, and picks the profile when none is named. With
`fzf`, dynamic catalogues support type-to-filter; without it they use numbered
choices (`q` cancels). Fixed menus always use native arrow keys, regardless of
whether `fzf` is installed; `TERM=dumb` uses the numbered fallback. Scripts should pass explicit arguments. `rm` and `mv`
stay explicit — destructive operations should not be a menu selection.

`set` writes the profile's saved model or effort default. Native `/model` and
`/effort` choices affect the current session, not the next ccmorph launch.
It prints the bare value to stdout
(so `m=$(ccmorph get gw model)` and existing scripts keep working) and confirms
the save on stderr. `inspect` reports the effective selection.

`ccmorph inspect` is the one to reach for when a session does not behave as
expected: every line names the layer its value came from, and the last line is
the command the launcher will actually run.

### Saved defaults versus session choices

`ccmorph set <profile> model|effort <value>` changes future launch defaults.
Claude's `/model` and `/effort` change the current session. Claude may still say
it saved a default and write its own settings; ccmorph does not adopt those
writes as configured defaults on the next launch.

The profile's `~/.config/ccmorph/<profile>.conf` owns model defaults and mappings,
alongside connection settings. The keys a profile may carry are fixed by its type,
and `profile.conf.example` (installed beside the library) is the template: copy it,
or read it in full. In brief:

| `.conf` field | Purpose |
|---|---|
| `type` | `sub`, `token`, `apikey` or `provider` (required) |
| `email`, `token` | Identity for `sub` and `token` profiles |
| `provider` | Provider name, on a `provider` profile |
| `base_url`, `api_key` | Endpoint and key, on an `apikey` profile |
| `adapter` | `true`/`false`: force or forbid the translator |
| `tag` | Label for this route in the model picker |
| `model`, `effort`, `effort.<model>` | Saved default model and effort; per-model override |
| `env.<NAME>` | Launch environment; see section 3 of the template for the allowlist |
| `private=1` | Run from a throwaway view: config and settings are linked in, transcripts, prompt history and session state are deleted when the session exits |
| `excluded_models=<id>,<id>` | Gateway picker exclusions (exact IDs, no spaces) |
| `window_sources=<name>` | Shipped catalogue to size this route by |
| `context_window.<model>` | Pinned token limit; beats discovery |
| `model_defaults=1` | This profile owns its model defaults (written once; keep it) |
| `context_discovered={...}` | Limits `probe` found for this route (machine-written) |

The model-mapping keys in the table are the `env.ANTHROPIC_*` names listed in the
template's second section. Add only overrides you need; leave unused fields absent rather than blank or
`null`. A tier field says which model that alias resolves to on this route — set
one to repair a tier whose native target the endpoint does not serve, or to make
the tier mean a model you chose on merit. Either way the alias, not just the
picker row, resolves to it: helper calls and subagents follow, so a cross-family
mapping answers from a different model than the row's name suggests.

A mapping does **not** grant the 1M window. For a `claude-*` id the window comes
from the id itself, and ccmorph adds the `[1m]` suffix only to an explicit launch
target — never to a tier alias — so a mapped `opus` still runs at 200,000. For 1M,
launch the suffixed id (`--model claude-opus-5[1m]`) and make sure
`availableModels` does not also permit the bare `opus`, which would shadow that
row. See ARCHITECTURE.md, "What a tier mapping does".

On first launch or explicit save, existing effective model settings are
imported once, preserving current defaults. `model_defaults=1` records that the
profile has adopted this separation; keep that marker when editing defaults.

Other Claude preferences live in the profile's layer,
`~/.config/ccmorph/profiles/<profile>/settings.profile.json`. At each launch ccmorph
writes the profile's `settings.json` from `~/.claude/settings.json`, that layer and the
configured model values, and carries any edit Claude Code made to `settings.json`
back into the layer first. Your arguments reach Claude Code exactly as typed:
`ccmorph run <profile> mcp list`, `remote-control`, `-p …` or any future
subcommand need nothing from ccmorph. No write-back hook, credential copy, or
transcript rewrite is needed. Explicit launch flags win for that invocation;
configured per-model effort overrides general effort. Managed policy remains
authoritative.

This applies to sessions launched through `ccmorph run` (including a personal
alias pointing to it). Installing ccmorph does not replace or change independently
launched native `claude`. Existing sessions need a relaunch to adopt the new
launch behavior.

### Exclude unwanted gateway models

For an API-key gateway profile, edit its `.conf` rather than maintaining a full
allowlist:

```ini
excluded_models=unwanted-model,text-embedding-3-large
```

ccmorph refreshes the gateway catalogue and generates Claude's native
`availableModels` minus these IDs. Command-line selection (`--model -i`,
`set <profile> model -i`) honors the same exclusions. Newly added models appear
automatically. Use `ccmorph models <profile>` to inspect the unfiltered
chat-capable gateway catalogue, including models excluded from selection.
Matching is exact and case-sensitive; derived `[1m]` variants and configured tier
aliases targeting an excluded ID are excluded too. Cached rows are filtered even
when discovery is offline. An exclusion that leaves no rows stops launch rather
than accidentally restoring Claude's unrestricted picker.

This is picker configuration, not gateway access control. It does not delete model
metadata or silently change the saved default. Remove the `.conf` field to stop
excluding models, then relaunch with the gateway reachable to refresh the list.

### Historical tool references

For a gateway session failing with `Tool reference '…' not found in available tools`,
the fix is a request-hooks module: `env.CCMORPH_SHIM_HOOKS=<file>` points the
shim at a module whose repair hook converts only unresolved historical
`tool_reference` blocks into text containing the tool name (the hook contract is
documented in the shim's header). References declared in the current request
remain unchanged; no permissions or tool definitions are added, and saved
transcripts are not changed. Relaunch/resume the same session to activate it.
Default: off.

### Identifier handling

By default the shim forwards `metadata` and every header exactly as Claude Code sent
them. A profile opts in with `env.CCMORPH_RANDOM_DEVICE_ID=1` to send a random
per-session `device_id` in `metadata.user_id` instead of the real one (the session
id passes through, so caching is unaffected). Relaunch to apply.

### Gateway edge

Every route the adapter serves goes through its request shim. A gateway may answer
under internal model names, retire the id Claude Code asks for, wrap its errors
unreadably, or stall. Claude Code and the Claude apps see only Anthropic ids: an
unserved `claude-*` id goes to a served model of the same family at the same
version or newer — never an older one, and through unchanged when none exists —
replies carry that Anthropic id, and hex-wrapped gateway errors are unwrapped so
Code's own recovery can run. A gateway silent for `env.CCMORPH_STALL_TIMEOUT`
seconds (default 300), before its reply or mid-stream, ends the turn with an error
Claude Code can retry instead of a reply that never comes. A gateway that already
speaks Anthropic is fronted by the shim alone, with no translator.
Set `env.CCMORPH_UPSTREAM_PROXY` (an `http://` CONNECT proxy) to send the
translator's upstream calls through a proxy, e.g. a loopback bridge holding the
macOS Local Network grant for a gateway on the LAN.

### Gateway startup

Custom endpoints use the automatic adapter path. Launch passes `--auto`; native
Anthropic-compatible endpoints return a pass-through result, while other dialects
get a per-launch translator. Protocol selection first uses the proven cache or an
explicit per-model `supported_endpoints` declaration from `/v1/models`. If metadata
is absent, selecting a model triggers bounded compatibility requests and caches the
working protocol. A fresh terminal launch without a saved model opens the existing
model picker; the choice applies only to that invocation. Headless launches require
an explicit or saved model when no usable protocol is cached or declared. Resume
and native subcommands never trigger this automatic picker. Endpoint failures still
fail explicitly.

OpenAI-compatible base URLs may include an API version, such as `/api/v4`.
Anthropic-compatible bases must be the prefix before `/v1/messages`, as required by
Claude Code and the translator. Compatibility and context probes use those same
client URL conventions.

Context-limit inference probes remain opt-in; ordinary context refresh reads free
endpoint or catalogue metadata automatically, and an unknown limit uses the
conservative 200,000-token fallback without blocking launch.

In translating sessions, selecting an uncached model with `/model` prepares its
route before the first inference request. The adapter confirms gateway membership,
classifies only that model, and waits for the translator to hot-load the updated
configuration on the same port. Preparation can incur the bounded compatibility
charges described above. Failure does not send or replay the user's request;
a later request can retry. Native pass-through sessions remain unchanged.

Startup identifies the connecting and native-Claude handoff stages. If adapter
startup lasts over one second it shows progress, then repeats the wait status
every five seconds. A selected-model startup that is not ready within 45 seconds
stops with the log location instead of waiting silently for ten minutes. This
bounds ccmorph's adapter wait, not Claude Code's subsequent MCP initialization
or the gateway's time to answer a real prompt.

## Switching

```sh
ccmorph run                        # the default profile
ccmorph run <profile>              # this session only
ccmorph run -i                     # ask which profile, then which model
ccmorph run --model -i              # default profile, ask only for model
ccmorph run <profile> --model -i    # named profile, ask only for model
ccmorph run <profile> --model <id> # profile plus explicit model
ccmorph hop <profile>              # hop a RUNNING session, keeping model + effort
```

After the optional profile and ccmorph's interactive selection flag, native
arguments pass through to Claude Code. `ccmorph run work -p 'summarise this'`
and `ccmorph run work --resume <session-id>` work as
you would expect. A leading flag belongs after the verb: `ccmorph run -p 'hi'`.

Claude Code's own subcommands work the same way, on the default profile or a named
one — and going through ccmorph is what points them at the right profile's config
directory, which matters because MCP servers, logins and plugins are per-profile:

```sh
ccmorph run mcp list         # the default profile's MCP servers
ccmorph run work mcp list    # the work profile's
ccmorph run doctor           # diagnose the default profile's install
```

An *unrecognised* top-level word is an error. If it matches a profile, ccmorph
prints the explicit replacement: `ccmorph run <profile>`.

A bare `ccmorph` prints usage, the way `git` and `docker` do — launching always
uses `run`, so nothing starts a session by accident.

`-i` is the one spelling for interactive selection, at launch and everywhere else.
At launch it chooses the profile first, then the model when ccmorph can list
models. With a named profile you can use Claude Code's native `--model <id>`
instead, or `ccmorph set <profile> model -i` to pick from the endpoint's listing.

`ccmorph hop` queues the switch and asks you to exit; the relay then resumes the
session on the new profile with its current model and effort intact. It only
works **from inside a running Claude Code session** — it reads that session's
id, model, and effort to hand them to the next one. In the Claude Code prompt,
run it with a leading `!`:

```
! ccmorph hop personal
```

In a plain terminal it exits with `not inside a Claude Code session`, and a
session started by plain `claude` (bypassing the launcher) reports `this session
was not launched by the ccmorph relay` — start it with `ccmorph` and hop from
there.

## What is isolated, and what is shared

Each profile gets its own `CLAUDE_CONFIG_DIR` under
`~/.config/ccmorph/profiles/<name>`, so profile state, settings layers, and
model choices never bleed between profiles.

Deliberately **shared** — your harness is per-machine, not per-profile: skills,
plugins, commands, hooks, output styles, `CLAUDE.md`, keybindings, and session
history are one set, symlinked into `~/.claude`, identical in every profile. MCP
servers are shared as capsules: each holds its own credentials and every profile
reaches the same service. A remote OAuth MCP registered directly in one profile,
not as a capsule, is authorized per profile, because Claude Code keeps its grants
beside that profile's sign-in. Install a plugin or write a skill once and every profile has
it — including things you add *after* a profile was created (the next launch
wires them in). Two profiles are two identities against a backend, not two
people. `ARCHITECTURE.md` §2 has the reasoning and the full table.

### Where state lives

Everything ccmorph stores is under one root: profile `.conf` files sit in it,
and profile runtime state sits in `profiles/<name>` beneath it. The root is
`$CCMORPH_STATE_DIR` if you set it, otherwise `$XDG_CONFIG_HOME/ccmorph`,
otherwise `~/.config/ccmorph`. The launcher, adapter, and context migration derive the profile directory from
that root. These variables select a location; changing them after migration does
not automatically move an existing state tree.

Profile state used to live in `~/.claude-profiles`. Upgrading moves it for you,
once:

- The old directory is **renamed**, not copied, so no credential is ever
  duplicated on disk and file modes and inner symlinks are carried as-is.
- `~/.claude-profiles` is left behind as a symlink to the new location, so a
  Claude Code session that is already running — or an external tool holding the
  old absolute path — keeps working without a relaunch.
- If both locations already hold a real directory, the move **refuses** and
  changes nothing rather than merging two profile homes. Same for a rename that
  would cross filesystems: it is reported and nothing is copied. Resolve it by
  hand, then run any ccmorph command again.
- An already-migrated host keeps the existing compatibility link unchanged.
  Fresh installs create only the nested directory, with no legacy link.

Untouched by all of this: native `~/.claude` and the shared harness layer inside
it. Each profile keeps its own secure storage, so a subscription login made in one
profile is never visible to another; MCP credentials are shared through MCP
capsules instead. `./install.sh --uninstall` still leaves your
profile state and credentials in place — removing the state root remains a
deliberate, manual step.

### Context windows behind a gateway

Context limits belong to a profile and model pair, not to a model name across
all gateways. Launch reads local configuration and cached limits; metadata
discovery runs in the background. If no limit is known, launch warns and uses
Claude Code's 200,000 fallback. That fallback is not a verified gateway limit.

The built-in compaction target is **75%**; a fleet policy or explicit profile
percentage can override it. Claude Code checks compaction between requests, so a
large response or tool result can temporarily cross the target. Disabled
compaction, repeated compaction failures, or an explicit opt-out of unknown-model
window enforcement can prevent proactive compaction; a percentage alone does not
enable it. Existing sessions must be resumed in a new process to load changed
launch settings.

Context limits are managed automatically from endpoint metadata and route-bound
profile state. `probe` is the explicit command for refreshing metadata, and
`ccmorph context <profile> <model> [tokens|auto]` reads, sets, or clears one
model's limit by hand — the free answer for a model too new for the gateway and
every catalogue to publish a size, where the only alternative is a paid probe.
`auto` returns that model to discovery.

`ccmorph probe <profile>` refreshes that profile's discovered context data,
stored as `context_discovered` in the same `.conf`. The default probe is
metadata-only: it lists the route, accepts positive integer context fields even
when they are equal, preserves existing readings for the current route, and
spends **0 inference tokens**. The whole run has a 30s deadline, with
`min(1s, 10%)` reserved for cleanup; `--deadline` can raise it. Conflicting
metadata is disclosed, but only an existing value for that same model on the
profile's *current* route wins
automatically. Changing a profile's route (endpoint or credential)
invalidates its discovered data — a fresh probe starts clean; a manual
`context_window.<model>` override is unaffected and always persists.

If gateway metadata is missing, discovery consults the maintained
[Models.dev catalogue](https://models.dev): an exact provider/model match first,
then an unambiguous canonical model ID. It uses the smaller of published input
and combined-context limits when both exist. These values are labelled
`catalogue-estimate` by `ccmorph inspect`, recorded with source/date in the profile,
and replaced by gateway-declared values when those become available. They are
not proof of a custom gateway's enforced limit. Ambiguous aliases remain unknown;
manual overrides and verified gateway values are never replaced by estimates.
A dated, MIT-licensed snapshot ships as `share/model-catalogue.json`: a recognised model
can use it on its first launch without any network lookup. Background discovery
still tries current metadata; if public access is blocked, it reports that it is
using the bundled snapshot. A model newer than both sources remains unresolved. An
exact catalogue family alias is accepted when every published window for that family
agrees; variants without a published window are ignored, while conflicting values
remain unknown rather than guessed.

Some providers publish their own model catalogue, and a gateway in front of them
may only echo the provider's default window. `share/window-sources.json` ships those
catalogues: which listing owners each one speaks for, where it lives, and which
fields name the model, its default and its usable window. A profile opts in with
`window_sources=<name>[,<name>]` in its `.conf`; no other profile is re-sized, and
a gateway's own figure is never lowered or overridden (rules: ARCHITECTURE.md §5).
Supporting a new provider is one entry in the shipped file; installs and upgrades
replace it, so add entries upstream, not locally.

Paid probing is explicit. Use
`ccmorph probe <profile> --model <id> --allow-inference` when metadata cannot
answer one profile/model pair. Known models are skipped unless `--probe-all` is
also set. `--bisect` additionally enables hidden-ceiling search and requires
`--allow-inference`. Bisection starts only after an explicit context-overflow
refusal; the refusal need not contain a number. Generic errors and timeouts stop
the search without recording a new limit. A retry is a separate, potentially paid
operator decision; the tool does not automatically retry with a larger budget.
`--probe-timeout` defaults to 10 seconds per request. Rejections and timeouts are not guaranteed to be free, and the first
accepted prompt — up to 6,000,000 characters — can be billed; character estimates
are not known token counts.

Probing never changes WHICH models the picker offers — that is the endpoint's
answer alone — only how large a window each one is given. Values discovered in a
background launch helper or a probe apply to subsequent launches; Claude Code
fixes the current process environment at start and does not reread the profile's
`.conf` mid-session. Your own `/autocompact` choice, if you make one, still wins
over the derived number.

### A note on the `claude` command

`claude` remains Anthropic's command. To use a ccmorph profile, run
`ccmorph run [profile]`. Personal aliases are the user's choice and are not a
ccmorph installation or configuration feature. An alias does not add profile
flags to Anthropic's CLI; profile selection belongs to `ccmorph run`.

### Reaching a private endpoint

ccmorph ships no transport. If a gateway is only reachable through a bridge,
run the bridge's local proxy and export `HTTPS_PROXY` for the command:

```sh
HTTPS_PROXY=http://127.0.0.1:<port> ccmorph run <profile>
```

ccmorph passes `HTTPS_PROXY`, `https_proxy` and `NO_PROXY` through unchanged to
every network consumer — the Claude child, the adapter, model listing and
metadata discovery. Its own loopback health checks never go through a proxy.
`ARCHITECTURE.md` has the detail.

## Evidence markers in ARCHITECTURE.md

`ARCHITECTURE.md` marks claims by evidence: **[wire]** observed in a captured
request, **[src]** read from vendor source, **[test]** pinned by the suite.
Anything unmarked is a design statement, not a probe. Read it before
changing behavior — most of its sections exist because something was once a bug.

## Tests

```sh
make test                        # everything below
zsh tests/ccmorph_test.zsh      # profiles, launch, routing, model choosers
zsh tests/install_test.zsh       # the installer against a throwaway $HOME
python3 -m pytest tests/         # adapter + gateway shim
```

TDD is the convention here: a behavior worth keeping gets a test that fails
without it.

CI runs the same suites on every push (`.github/workflows/ci.yml`), on Linux
and on a macOS runner for the BSD-tool differences. It also proves the release
tarball carries no live state, because this repository's working directory
doubles as its author's live install.
