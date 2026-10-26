# ccproteus-base.zsh — Shared primitives: portable file metadata, logging, executable resolution, the environment-name lists, Claude Code's own CLI surface and value checks.
# Sourced by ccproteus.sh after ccproteus-ui.zsh (see its Modules block); never run directly.
# Runs code at load (zsh/stat, the env-name lists, the verb cache), so it loads first.

# ── File metadata, portably ──────────────────────────────────────────────────
# `stat` has no portable spelling: BSD/macOS wants -f %z/%m, GNU/Linux wants
# -c %s/%Y, and each errors on the other's flags. Both call sites here swallowed
# that error (|| echo 0, || continue), so on Linux log rotation and stale-file
# collection silently never ran. zsh's own stat module is the same on every
# platform and forks no process at all.
zmodload -F zsh/stat b:zstat 2>/dev/null

# zstat arrays are indexed 1-based below; a caller's KSH_ARRAYS would shift them.
# Size in bytes; 0 when unknowable, which both callers already treat as "skip".
_ccs_size() {
  setopt localoptions noksharrays
  local -a s
  if zstat -A s +size "$1" 2>/dev/null; then
    print -r -- "$s[1]"
  else
    print -r -- 0
  fi
}

# Mtime as a unix timestamp; nonzero exit when unknowable, so a caller can
# `|| continue` rather than treat a missing file as epoch 0 (infinitely stale).
_ccs_mtime() {
  setopt localoptions noksharrays
  local -a s
  zstat -A s +mtime "$1" 2>/dev/null || return 1
  print -r -- "$s[1]"
}

# External utility calls use `command` so sourced user functions and aliases cannot
# alter ccproteus behavior; this is isolation from shell overrides, not from PATH.

# ── Logging ──────────────────────────────────────────────────────────────────

_ccs_log_init() {
  setopt localoptions noksharrays
  # Create the log 0600 before any writer appends at the caller's umask.
  if [[ ! -e "$_CCPROTEUS_LOG" ]]; then
    ( umask 077; : >> "$_CCPROTEUS_LOG" ) 2>/dev/null || :
  elif [[ -f "$_CCPROTEUS_LOG" && ! -L "$_CCPROTEUS_LOG" && -O "$_CCPROTEUS_LOG" ]]; then
    # An older/looser log keeps its mode otherwise. Own regular file only: never
    # chmod through a planted symlink. zstat first, so no fork when already private.
    local -a _ccs_st
    zstat -A _ccs_st +mode "$_CCPROTEUS_LOG" 2>/dev/null &&
      (( _ccs_st[1] & 8#077 )) && command chmod go-rwx "$_CCPROTEUS_LOG" 2>/dev/null || :
  fi
}

_ccs_log() {
  # Append a timestamped line to the log. Best-effort — never fatal.
  local ts
  _ccs_log_init
  ts=$(command date '+%Y-%m-%d %H:%M:%S' 2>/dev/null) || ts="?"
  print -r -- "$ts [$$] $1" >> "$_CCPROTEUS_LOG" 2>/dev/null || :

  # Auto-rotate: if the log exceeds the cap, keep the last half.
  # _ccs_size, not `stat -f %z`: that spelling is BSD-only, so on Linux it failed,
  # the `|| echo 0` reported an empty log, and rotation NEVER ran — a silent
  # unbounded log on exactly the platform nobody here would notice it on.
  if [[ -f "$_CCPROTEUS_LOG" ]] &&
     (( $(_ccs_size "$_CCPROTEUS_LOG") > _CCPROTEUS_LOG_MAX )); then
    # mktemp, not a fixed .tmp name: under the /tmp fallback the name is
    # guessable, and a planted symlink there would be installed AS the log.
    local _ccs_rot
    _ccs_rot=$(command mktemp "$_CCPROTEUS_LOG.XXXXXX" 2>/dev/null) &&
      command tail -n 500 "$_CCPROTEUS_LOG" > "$_ccs_rot" 2>/dev/null &&
      command mv -f "$_ccs_rot" "$_CCPROTEUS_LOG" 2>/dev/null || :
  fi
}

# ── Executable resolution ────────────────────────────────────────────────────
# ONE resolver for every configured executable seam (CCPROTEUS_PRELAUNCH): a bare
# name resolves through PATH the way invoking it would, and the result is
# canonicalized ONCE (:A). `[[ -x ]]` on a bare name tests $PWD, not PATH, and a
# relative path would resolve against wherever the launch ran.
_ccs_resolve_exe() {  # $1 = env var name (for messages), $2 = configured value
  local var="$1" val="$2"
  if [[ "$val" != */* ]]; then
    val="${commands[$val]:-}"
    [[ -n "$val" ]] || { _ccs_err "$var is not on PATH: $2"; return 1 }
  fi
  val="${val:A}"
  [[ -x "$val" ]] || { _ccs_err "$var is not executable: $val"; return 1 }
  print -r -- "$val"
}

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
# output_tokens (variance swamps it). ARCHITECTURE.md section 6 has the evidence
# and the method that does work.
# Gateway role mappings belong to the profile, independent of credential type.
_CCPROTEUS_MODEL_MAPPING_ENV=(
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
# launcher records the ones it set in CCPROTEUS_SET_ENV (names only) so a NESTED
# launch clears them rather than inherit an outer session's value (and a launch
# inside a Claude Code child session also clears CLAUDE_CODE_SIMPLE, which `claude
# --bare` sets itself and no launch records). Every key here
# must also be in _CCPROTEUS_PROFILE_ENV (a profile may declare it).
_CCPROTEUS_TUNING_ENV=(
  CLAUDE_CODE_SIMPLE
  MAX_THINKING_TOKENS
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
)
# Keys that pick the backend or account a launch reaches, or carry its credential.
# ALWAYS cleared before a launch: a profile declares what it needs and nothing
# comes from the calling shell. A profile may not declare these via env. lines
# (they are not in _CCPROTEUS_PROFILE_ENV); the launcher sets its own credential and
# endpoint itself. `ccproteus _backend_env` prints this list for the Remote Control
# add-on, which builds its own strip list from it.
_CCPROTEUS_BACKEND_ENV=(
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
_CCPROTEUS_LAUNCH_ENV=(
  ANTHROPIC_MODEL
  CLAUDE_CODE_AUTO_COMPACT_WINDOW
  CCPROTEUS_ADVISE
  CCPROTEUS_TASK
  CCPROTEUS_ATTEMPT
)
_CCPROTEUS_PROFILE_ENV=(
  "${_CCPROTEUS_MODEL_MAPPING_ENV[@]}"
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
  CCPROTEUS_REPAIR_TOOL_REFERENCES
  CCPROTEUS_RANDOM_DEVICE_ID
  CCPROTEUS_AFFINITY_HEADER
  CCPROTEUS_STALL_TIMEOUT
  CCPROTEUS_UPSTREAM_PROXY
  # Path to the profile's request-hooks module, loaded by the gateway shim.
  CCPROTEUS_SHIM_HOOKS
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
  # ccproteus could not see it, and both trigger mirrors computed from the wrong ceiling.
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

# ── Helpers ──────────────────────────────────────────────────────────────────

# Is this word one of Claude Code's own subcommands? ASKED, never hardcoded.
#
# ccproteus has no business knowing another tool's surface. A baked-in verb list is
# wrong the moment Claude Code ships a new subcommand -- and it ships often -- so the
# answer comes from the installed binary's own `--help`. That way a subcommand added
# tomorrow works today, and one removed stops being special without a ccproteus
# release. `claude --help` costs ~0.2s and is asked at most once per launch, cached in
# a global for the second reader.
#
# The dispatcher needs this answer to pass these through to Claude Code rather than
# read them as a profile name. The launch adds nothing to the arguments either way;
# it only keeps session-only work (the model picker) off a subcommand. A verb
# Claude Code hides from `--help` therefore costs a picker prompt.
#
# On failure it answers NO, deliberately. An unknown word then falls through to the
# profile reading, which is what a bare `ccproteus <word>` has always meant, and
# Claude Code treats a word it does not recognise as a prompt rather than an error --
# so the worst case of a wrong answer is the behaviour that predates this function.
#
# The cache lives for THIS process only, which is the whole life of one ccproteus
# invocation. Nothing is remembered across runs on purpose: `npm i -g
# @anthropic-ai/claude-code` between two launches must take effect on the second,
# and a stale cached surface is exactly the drift this function exists to avoid.
typeset -g _CCPROTEUS_CLAUDE_VERBS=""

# One capture of the installed binary's own `claude --help`: the single
# subprocess both native-surface readers parse, Commands for the verbs and
# Options for option arity. Asked at most once per process however it
# answers — an unreachable binary must not be re-asked for every token a
# launch scan steps over — and reset by re-sourcing, like the caches that
# parse it.
typeset -g _CCPROTEUS_CLAUDE_HELP=""
typeset -g _CCPROTEUS_CLAUDE_HELP_PROBED=0

_ccs_claude_help() {
  (( _CCPROTEUS_CLAUDE_HELP_PROBED )) && return 0
  _CCPROTEUS_CLAUDE_HELP_PROBED=1
  # Whatever the binary prints is the answer, its exit status is not: a
  # failing run that still prints its surface is read, an empty one is not.
  _CCPROTEUS_CLAUDE_HELP="$(command claude --help 2>/dev/null)"
  return 0
}

_ccs_claude_verbs() {
  [[ -n "$_CCPROTEUS_CLAUDE_VERBS" ]] && { print -r -- "$_CCPROTEUS_CLAUDE_VERBS"; return 0 }

  # `Commands:` section only, two-space-indented leading word per row. Anything the
  # binary does not list is not a subcommand, by definition.
  local out
  _ccs_claude_help
  out="$(print -r -- "$_CCPROTEUS_CLAUDE_HELP" |
        command sed -n '/^Commands:/,$p' |
        command grep -oE '^  [a-z][a-z0-9|-]*' |
        command tr -d ' ' |
        command tr '|' '\n')"

  _CCPROTEUS_CLAUDE_VERBS="$out"
  print -r -- "$out"
}

_ccs_is_claude_subcommand() {
  local word="${1:-}"
  [[ -n "$word" ]] || return 1

  # ccproteus's OWN verbs win, always, regardless of what Claude Code also calls them.
  # `rm` is the collision that matters: both tools have one, and `ccproteus rm` has
  # always meant "delete an profile". This check is first so the answer cannot change
  # when Claude Code adds a verb ccproteus already uses.
  case "$word" in
    list|inspect|show|add|config|login|default|use|rm|mv|get|set|context|cred|secret|creds|probe|hop|help|version|run|start) return 1 ;;
    # The retired window verb, spelled without writing it: it must still be reserved
    # (never read as an profile name, never forwarded to Claude Code) while the word
    # itself is gone from this source. `$_ccs_retired_verb` is set beside the version.
    ("$_ccs_retired_verb") return 1 ;;
  esac

  local verb
  for verb in ${(f)"$(_ccs_claude_verbs)"}; do
    [[ "$word" == "$verb" ]] && return 0
  done
  return 1
}

# Which native options take a value, and how the value is given: the Options
# section of the installed binary's own `claude --help`, the same source of
# truth _ccs_claude_verbs reads for Commands. A value is marked <required> or
# [optional] after the (comma-separated) spellings:
#   --system-prompt <prompt>   -r, --resume [value]   --verbose
# The launch scans need this because an option VALUE can look exactly like a
# flag — `--system-prompt --continue` hands Claude Code the literal text
# `--continue` as the prompt — and a scan that does not know the arity reads
# that value as the flag.
#
# Same caching contract as _CCPROTEUS_CLAUDE_VERBS: this process only, so a
# binary installed between two launches is re-read on the second. Re-sourcing
# resets it for the same reason the verb cache resets. The table is parsed at
# most once however it comes out: an empty one would otherwise be re-parsed
# for every option token the model loop steps over.
typeset -gA _CCPROTEUS_CLAUDE_OPT_ARITY=()
typeset -g _CCPROTEUS_CLAUDE_OPT_ARITY_PROBED=0

_ccs_claude_opt_arities() {
  (( _CCPROTEUS_CLAUDE_OPT_ARITY_PROBED )) && return 0
  _CCPROTEUS_CLAUDE_OPT_ARITY_PROBED=1

  # Option rows only: two-space indent, dash-leading. A wrapped description
  # continues at the description column, so it never matches. The spec is the
  # row up to the column alignment: the spellings, then the placeholder when
  # there is one.
  local out spec arity spelling
  _ccs_claude_help
  out="$(print -r -- "$_CCPROTEUS_CLAUDE_HELP" |
        command sed -n '/^Options:/,/^Commands:/p' |
        command grep -E '^  -' |
        command sed -e 's/^  //' -e 's/  .*$//')"

  for spec in ${(f)out}; do
    # The placeholder, when present, is the spec's last word.
    case "${spec##* }" in
      \<*) arity=value;    spec="${spec% *}" ;;
      \[*) arity=optional; spec="${spec% *}" ;;
      *)   arity=flag ;;
    esac
    for spelling in ${(s:, :)spec}; do
      _CCPROTEUS_CLAUDE_OPT_ARITY[$spelling]="$arity"
    done
  done

  return 0
}

# Steps one native argv token the way the installed binary's own parser does.
# $1 is an option spelling; $2 is the token that follows it, passed only when
# one exists. Answers in REPLY how many argv tokens the option owns:
#   2  the spelling and its value: the token after it IS the value, whatever
#      it looks like. An [optional] value is taken only when it could be one.
#   1  the spelling alone.
#   0  the installed binary does not list the spelling: it will reject the
#      launch, so ccproteus reads nothing on its behalf.
_ccs_claude_option_step() {
  REPLY=0
  local opt="$1"

  # --spelling=value carries its value inside the token: one token, always.
  [[ "$opt" == --*\=* ]] && opt="${opt%%=*}"

  _ccs_claude_opt_arities
  local arity="${_CCPROTEUS_CLAUDE_OPT_ARITY[$opt]:-}"
  [[ -n "$arity" ]] || return 0

  # A known option owns at least itself; an embedded value owns nothing more.
  REPLY=1
  [[ "$1" == --*\=* ]] && return 0

  if (( $# >= 2 )); then
    case "$arity" in
      value) REPLY=2 ;;
      optional) [[ -n "$2" && "$2" != -* ]] && REPLY=2 ;;
    esac
  fi
  return 0
}

_ccs_launch_has_claude_subcommand() {
  local arg
  while (( $# > 0 )); do
    arg="$1"
    case "$arg" in
      --) return 1 ;;
      # This launcher's own flags are not Claude Code's concern.
      -i|--interactive|--dangerously-skip-permissions) shift; continue ;;
      # In print mode the binary reads the positional as the prompt, so no
      # later token can be the command.
      -p|--print|--print=*) return 1 ;;
      # Every other option steps over the value it owns, the way the binary
      # parses its own argv: `--verbose mcp get` and `--model foo mcp get`
      # are subcommand launches, `--system-prompt mcp get` is not (the prompt
      # value is not a command). An option the installed binary does not list
      # ends the walk — ccproteus knows nothing about it and claims nothing.
      # The slice hands the walker the option and the token after it when one
      # exists, so an empty value token is still passed as a value.
      -?*)
        _ccs_claude_option_step "${@:1:2}"
        (( REPLY > 0 )) || return 1
        shift "$REPLY"
        continue
        ;;
    esac
    _ccs_is_claude_subcommand "$arg"
    return $?
  done
  return 1
}

# Whether the launch's argv holds, after $1, a token _ccs_launch's first scan
# would claim for itself: one of that scan's own flag spellings. That scan
# stops at its `--` separator -- nothing after it is claimed -- so this
# look-ahead stops there too, which also keeps the separator itself from ever
# being stepped as an option's value. The spellings mirror the cases of that
# scan.
_ccs_launch_has_flag_ahead() {
  shift  # $1 is the option being stepped; the claimable tokens come after it.
  local arg
  for arg in "$@"; do
    case "$arg" in
      --) return 1 ;;
      -a|--profile|--profile=*|-i|--interactive) return 0 ;;
    esac
  done
  return 1
}

# Which argument, if any, is run's OWN `--` separator? Leaves the 1-based index
# in REPLY, or 0 when the caller gave none. `--` is run's separator in the outer
# position only (usage: run [profile] [-i] [-- args]), so it
# counts only while nothing but this launcher's own leading options precede it.
# The first argument Claude Code owns ends that prefix, and every `--` from
# there on is Claude Code's: its command grammar is its own (ARCHITECTURE.md,
# Scope and ownership), so the MCP delimiter in `mcp add demo -- npx -y server`
# and the literal prompt value in `-p --` must both arrive intact. Deleting the
# first exact `--` wherever it appeared removed those too, one argument short.
_ccs_run_separator_index() {
  local i=0
  REPLY=0
  while (( $# > 0 )); do
    case "$1" in
      --)
        REPLY=$(( i + 1 ))
        return 0
        ;;
      -i|--interactive)
        shift
        (( ++i ))
        ;;
      # A value token belongs to its option even when the token itself is `--`:
      # the separator is recognised in prefix position, never inside a value.
      --model|--effort)
        if (( $# >= 2 )); then
          shift 2
          (( i += 2 ))
        else
          shift
          (( ++i ))
        fi
        ;;
      --model=*|--effort=*)
        shift
        (( ++i ))
        ;;
      *) return 0 ;;
    esac
  done
  return 0
}

_ccs_valid() {
  # Case-blind on the reserved names: 'Active' must not name a profile either.
  [[ -n "$1" &&
     "$1" != *[^A-Za-z0-9_]* &&
     "${(L)1}" != active &&
     "${(L)1}" != ccproteus ]]
}

_ccs_exists() {
  [[ -f "$_CCPROTEUS_DIR/$1.conf" &&
     ! -L "$_CCPROTEUS_DIR/$1.conf" ]]
}

_ccs_err() {
  print -u2 "${_ccs_red}ccproteus:${_ccs_reset} $1"
  _ccs_log "ERROR $1"
  return 1
}

_ccs_clean_value() {
  [[ "$1" != *$'\n'* && "$1" != *$'\r'* ]]
}

_ccs_relay_valid() {
  [[ -n "$1" && "$1" != *[^A-Za-z0-9_.-]* ]]
}
