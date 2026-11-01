# ccmorph — Claude Code profile switcher
# Loaded by the `ccmorph` executable itself; not meant to be sourced
# directly into an interactive shell.

_CCMORPH_VERSION="0.4.2"
# The pre-2.4.0 name for `probe`, still accepted so a script that already says it keeps
# working. Assembled rather than written so the retired word appears nowhere in this
# source; a test pins that the alias still dispatches.
_ccm_retired_verb="mea""sure"

# TWO directories, deliberately. They were one, which worked only because a
# git clone was also the install: the same path held ccmorph.sh and the helper
# executables (code, replaced wholesale by an upgrade) AND every profile, the
# discovered context values, and the active marker (state, which an upgrade must never
# touch). An install breaks that coincidence — install.sh replaces the code
# wholesale on every update — so code and state must name themselves separately.
#
# STATE. Honours XDG_CONFIG_HOME, which the hardcoded path silently ignored.
_CCMORPH_DIR="${CCMORPH_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph}"
#
# CODE: wherever this file is, its helpers sit beside it — in a clone or
# ~/.config. Derived rather than declared, so a copy of the runtime
# runs from where it was put instead of reaching back to a path that may hold a
# different version. %x is the file being sourced; :A resolves symlinks, which
# matters when a package manager links the library into place.
_CCMORPH_LIB_DIR="${CCMORPH_LIB_DIR:-${${(%):-%x}:A:h}}"
# Internal executables and data files sit beside lib/ (bin/, lib/, libexec/, share/).
_CCMORPH_LIBEXEC_DIR="${_CCMORPH_LIB_DIR:h}/libexec"
_CCMORPH_PROFILE_DIRS="$_CCMORPH_DIR/profiles"
_CCMORPH_CONTEXT_POLICY="${CCMORPH_CONTEXT_POLICY:-$HOME/.agents/runtime/policies/context.json}"
_CCMORPH_LOG="${TMPDIR:-/tmp}/ccmorph-${UID}.log"
_CCMORPH_LOG_MAX=102400  # rotate at 100KB

# The schema bounds Claude Code enforces on its own `autoCompactWindow` setting
# (.int().min(1e5).max(1e6) in the settings validator). These are NOT any model's
# window -- no window is ever written in this file; every one is probed and read
# from the registry. They exist only to keep a probed value inside the range the
# setting will accept, because a value outside it is DROPPED silently and the
# session then falls back to Claude Code's own default budget.
_CCMORPH_ACW_MIN=100000
_CCMORPH_ACW_MAX=1000000

# EPOCHREALTIME backs relay-id uniqueness; the module is NOT loaded by default
# and an unset expansion silently degrades the id to <pid>..<RANDOM>. Load it
# here (-i: never an error if unavailable) rather than depending on .zshrc.
zmodload -i zsh/datetime 2>/dev/null


# Machine-local overrides — a gitignored file that never ships. This is the
# single seam where a host's own tooling opts into optional behavior without the
# product carrying any knowledge of that tooling. CCMORPH_PRELAUNCH may name an
# executable to run before launch.
# `doctor` is intentionally safe even when a host local.sh is executable zsh.
# It must not run host opt-ins or let them mutate global state.
# A picker zsh started by the core sets _CCMORPH_LOCAL_SOURCED: the launching
# shell already ran local.sh, and its output would land in the picker's answer.
[[ "${CCMORPH_DOCTOR_READONLY:-0}" != 1 && "${_CCMORPH_LOCAL_SOURCED:-0}" != 1 && -r "$_CCMORPH_DIR/local.sh" ]] && source "$_CCMORPH_DIR/local.sh"

# ── Color (auto-off when piped) ──────────────────────────────────────────────

# Model-behaviour environment a profile may declare as `env.<NAME>=...`.
#
# This list is the single source for two things that must not drift apart: which
# keys a conf may set, and which keys are cleared from the launch environment so a
# profile only ever gets what it asked for. They were duplicated by hand and had
# already diverged -- MAX_CONTEXT_TOKENS, AUTO_COMPACT_WINDOW and
# AUTOCOMPACT_PCT_OVERRIDE were permitted but never cleared, so exporting one in a
# shell silently changed compaction for every profile that omitted it.
#
# CLAUDE_CODE_ALWAYS_ENABLE_EFFORT works on a routed profile: the level does reach
# the provider. Two traps, both of which produced a wrong answer here once --
# BOTH thinking and output_config.effort must be present (the translator reads the
# effort only for adaptive thinking), and effort cannot be verified by comparing
# output_tokens (variance swamps it). docs/ARCHITECTURE.md section 6 has the evidence
# and the method that does work.
# Gateway role mappings belong to the profile, independent of credential type.
_CCMORPH_MODEL_MAPPING_ENV=(
  ANTHROPIC_DEFAULT_FABLE_MODEL
  ANTHROPIC_DEFAULT_OPUS_MODEL
  ANTHROPIC_DEFAULT_OPUS_MODEL_NAME
  ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION
  ANTHROPIC_DEFAULT_SONNET_MODEL
  ANTHROPIC_DEFAULT_HAIKU_MODEL
  ANTHROPIC_SMALL_FAST_MODEL
  CLAUDE_CODE_SUBAGENT_MODEL
)
# Client tuning keys. They shape how Claude Code behaves, not which backend,
# account or credential a launch reaches, so a profile that does not declare one
# lets the shell's own value through. A profile that does declare one wins. The
# launcher records the ones it set in CCMORPH_SET_ENV (names only) so a NESTED
# launch clears them rather than inherit an outer session's value (and a launch
# inside a Claude Code child session also clears CLAUDE_CODE_SIMPLE, which `claude
# --bare` sets itself and no launch records). Every key here
# must also be in _CCMORPH_PROFILE_ENV (a profile may declare it).
_CCMORPH_TUNING_ENV=(
  CLAUDE_CODE_SIMPLE
  MAX_THINKING_TOKENS
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
)
# Keys that pick the backend or account a launch reaches, or carry its credential.
# ALWAYS cleared before a launch: a profile declares what it needs and nothing
# comes from the calling shell. A profile may not declare these via env. lines
# (they are not in _CCMORPH_PROFILE_ENV); the launcher sets its own credential and
# endpoint itself. `ccmorph _backend_env` prints this list for the Remote Control
# add-on, which builds its own strip list from it.
_CCMORPH_BACKEND_ENV=(
  CLAUDE_CODE_OAUTH_TOKEN
  CLAUDE_CODE_OAUTH_REFRESH_TOKEN
  CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR
  ANTHROPIC_API_KEY
  ANTHROPIC_AUTH_TOKEN
  ANTHROPIC_BASE_URL
  ANTHROPIC_UNIX_SOCKET
  ANTHROPIC_CUSTOM_HEADERS
  CLAUDE_CODE_USE_BEDROCK
  CLAUDE_CODE_USE_VERTEX
  CLAUDE_CODE_USE_FOUNDRY
  CLAUDE_CODE_USE_GATEWAY
  CLAUDE_CODE_USE_MANTLE
  CLAUDE_CODE_USE_ANTHROPIC_AWS
  CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD
  AWS_BEARER_TOKEN_BEDROCK
  ANTHROPIC_AWS_API_KEY
  ANTHROPIC_FOUNDRY_API_KEY
  ANTHROPIC_FOUNDRY_AUTH_TOKEN
  ANTHROPIC_FOUNDRY_BASE_URL
  ANTHROPIC_GOOGLE_CLOUD_BASE_URL
  ANTHROPIC_AWS_BASE_URL
  ANTHROPIC_BEDROCK_BASE_URL
  ANTHROPIC_BEDROCK_MANTLE_BASE_URL
  ANTHROPIC_VERTEX_BASE_URL
  CLAUDE_CODE_API_BASE_URL
  ANTHROPIC_ORGANIZATION_ID
  ANTHROPIC_WORKSPACE_ID
  ANTHROPIC_PROFILE
  ANTHROPIC_FEDERATION_RULE_ID
  ANTHROPIC_IDENTITY_TOKEN
  ANTHROPIC_IDENTITY_TOKEN_FILE
  CLAUDE_CODE_ACCOUNT_UUID
  CLAUDE_CODE_ORGANIZATION_UUID
  CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR
  CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR
  CLAUDE_CODE_SESSION_ACCESS_TOKEN
)
# The keys a launch sets for itself, only sometimes. Each is cleared before a
# launch so a nested one never inherits an outer session's value.
_CCMORPH_LAUNCH_ENV=(
  ANTHROPIC_MODEL
  CLAUDE_CODE_AUTO_COMPACT_WINDOW
  CCMORPH_ADVISE
  CCMORPH_TASK
  CCMORPH_ATTEMPT
)
_CCMORPH_PROFILE_ENV=(
  "${_CCMORPH_MODEL_MAPPING_ENV[@]}"
  CLAUDE_CODE_ALWAYS_ENABLE_EFFORT
  CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY
  ENABLE_TOOL_SEARCH
  # Native opt-out for routes that reject Anthropic's server-side advisor tool.
  CLAUDE_CODE_DISABLE_ADVISOR_TOOL
  # Claude Code's own minimal mode (what --bare sets): no hooks, plugins, LSP,
  # CLAUDE.md discovery or side requests. For small local models on slow prefill.
  CLAUDE_CODE_SIMPLE
  # Thinking budget; 0 turns extended thinking off for local models.
  MAX_THINKING_TOKENS
  # Stops the per-turn session-state classifier call, which costs a slow local
  # model a cold ~4.5k-token prefill (~15 s) after every turn, even in minimal mode.
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
  CCMORPH_RANDOM_DEVICE_ID
  CCMORPH_AFFINITY_HEADER
  CCMORPH_USER_AGENT
  CCMORPH_QUIRKS
  CCMORPH_STALL_TIMEOUT
  CCMORPH_UPSTREAM_PROXY
  # Secret masking in the gateway shim, and the profile's own password commands.
  CCMORPH_MASK
  CCMORPH_MASK_COMMANDS
  # Path to the profile's request-hooks module, loaded by the gateway shim.
  CCMORPH_SHIM_HOOKS
  CLAUDE_CODE_MAX_CONTEXT_TOKENS
  CLAUDE_CODE_AUTO_COMPACT_WINDOW
  CLAUDE_AUTOCOMPACT_PCT_OVERRIDE
  # Allowed because the compaction trigger depends on it: the engine's usable
  # window is `window - min(cV(model), 20000)` and cV() reads this var, so a value
  # below 20,000 makes the engine reserve less and trigger HIGHER. Without the key
  # here it never reached the launched process, and the hazard path's reserve
  # calculation -- which reads the same launch env the engine gets -- could never
  # see it either.
  CLAUDE_CODE_MAX_OUTPUT_TOKENS
  # For a model id Claude Code doesn't know (kimi-k3, glm, deepseek behind a
  # gateway) it assumes a 200k window and compacts there. MAX_CONTEXT_TOKENS can
  # correct that but is one launch-fixed value that a mid-session /model switch
  # can't retarget. This flag instead makes CC stop guessing for unknown ids: they
  # run to the model's real limit and compact only on the gateway's actual
  # too-long error. Only touches unrecognized ids; claude-* keep their own window.
  # Caveat: the reactive recovery needs the gateway to forward the over-limit error
  # verbatim — a gateway that rewraps it leaves CC unable to catch the overflow.
  CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT
  # The engine kill switch that makes a [1m] tag stop meaning 1,000,000. Allowlisted so
  # a conf can declare it AND so the launcher clears any ambient copy -- it only clears
  # keys on this list, so without membership an exported value reached the engine while
  # ccmorph could not see it, and both trigger mirrors computed from the wrong ceiling.
  CLAUDE_CODE_DISABLE_1M_CONTEXT
  # Makes /model list what the endpoint serves, labelled "From gateway", instead
  # of only Claude Code's built-in catalogue. Claude Code GETs
  # /v1/models?limit=1000 at bootstrap and skips it entirely when this is unset.
  # Worth one request per launch on any profile whose backend is not Anthropic.
  CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY
  # Adds exactly one row to /model, with a label and description of the profile's
  # choosing. Two jobs no other key can do. A profile whose endpoint advertises no
  # id starting with claude- or anthropic- gets no discovered rows at all, because
  # Claude Code filters the catalogue on that prefix -- so without this, /model on
  # such a profile offers nothing but Default. And a row pinned here is the only
  # way to offer a model id ending in [1m]: that suffix is what lifts the context
  # window to 1M on a non-Anthropic endpoint, and discovery never produces it.
  ANTHROPIC_CUSTOM_MODEL_OPTION
  ANTHROPIC_CUSTOM_MODEL_OPTION_NAME
  ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION
  # Which model ids may carry a mid-conversation {"role":"system"} turn. Allowed
  # because the launcher sets it for every adapter route (see the launch path):
  # a conf that declares its own mask must be able to reach the engine instead
  # of being silently overwritten.
  CLAUDE_CODE_MODEL_CAPABILITIES

)

source "$_CCMORPH_LIB_DIR/ccmorph-launch.zsh" || return 1
