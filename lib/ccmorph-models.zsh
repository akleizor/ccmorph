# ccmorph-models.zsh — Models: the session's model and state on resume, gateway /v1/models listings, published windows, route ceilings and model lookup.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

_ccm_resume_model() {
  local session_id="$1"
  local transcript model

  REPLY=""
  [[ -n "$session_id" ]] && _ccm_clean_value "$session_id" || return 1

  _ccm_session_file "$session_id" || return 1
  transcript="$REPLY" REPLY=""

  model=$(_ccm_util resume-model "$transcript"
  ) || return 1

  [[ -n "$model" ]] && _ccm_clean_value "$model" || return 1
  case "$model" in
    -*|@*|\?|*[[:space:]]*) return 1 ;;
  esac
  REPLY="$model"
}

_ccm_session_state() {
  local session_id="$1"
  local transcript state model effort

  reply=()
  [[ -n "$session_id" ]] && _ccm_clean_value "$session_id" || return 1

  _ccm_session_file "$session_id" || return 1
  transcript="$REPLY"

  state=$(_ccm_util session-state "$transcript"
  ) || return 1

  [[ "$state" == *$'\t'* ]] || return 1
  model="${state%%$'\t'*}"
  effort="${state#*$'\t'}"
  [[ -n "$model" && -n "$effort" ]] || return 1
  reply=("$model" "$effort")
}

_ccm_models_listing() {  # $1 = base url, $2 = key, $3 = max-time seconds
  # One `/v1/models` listing per launch, shared.
  #
  # The window probe and the picker projection both need this listing, and both
  # fetched it themselves — the same URL with the same headers, twice per launch,
  # differing only in timeout. The listing is free, so this was never a token cost,
  # but it is a wasted round trip on every launch and two chances for a slow
  # gateway to stall a shell instead of one.
  #
  # Memoised per (base, key) for the life of the shell, which is the right scope
  # for its two callers: they run within a single launch, and the answer cannot
  # change between them. Deliberately NOT a durable cache — a gateway's catalogue
  # does change, and `_ccm_project_picker` is documented as a probe rather
  # than a memory, so a later launch must ask again.
  # Answers in $REPLY, never on stdout. A caller writing `body=$(_ccm_models_listing
  # ...)` would run this whole function in a subshell, and the memo write would then
  # land in a copy of the array that dies with it — every call a cache miss, the
  # duplicate fetch intact, and nothing visibly wrong. Returning through REPLY keeps
  # the assignment in the caller's own shell, which is the only place it survives.
  local base="$1" key="$2" maxtime="${3:-8}"
  REPLY=""
  [[ -n "$base" && -n "$key" ]] || return 1

  # The credential is part of the identity: two profiles may share a base URL with
  # different keys and be shown different catalogues.
  local memo_key="${base}"$'\x1f'"${key}"
  if [[ -n "${_ccm_models_memo[$memo_key]:-}" ]]; then
    REPLY="${_ccm_models_memo[$memo_key]}"
    return 0
  fi

  REPLY=$(print -r -- "$key" | _ccm_util fetch-listing "$base" launch "$maxtime" 2>/dev/null) || return 1
  _ccm_models_memo[$memo_key]="$REPLY"
}

# "<known window or empty>\t<source>\t<unknown budget or empty>\t<send_id>",
# one process.
_ccm_model_window_fields() {
  _ccm_migrate_state || return 1
  local resolver="$_CCMORPH_LIBEXEC_DIR/ccmorph-core"
  [[ -x "$resolver" ]] || return 1
  command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
    CCMORPH_STATE_DIR="$_CCMORPH_DIR" MODEL_WINDOW_PROFILE_CONF="$_CCMORPH_DIR/$1.conf" \
    "$resolver" model-window --fields "$1" "$2" 2>/dev/null
}

# One local resolver reads manual and discovered limits from the profile itself.
_ccm_model_window() {
  _ccm_migrate_state || return 1
  local resolver="$_CCMORPH_LIBEXEC_DIR/ccmorph-core"
  [[ -x "$resolver" ]] || return 1
  command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
    CCMORPH_STATE_DIR="$_CCMORPH_DIR" MODEL_WINDOW_PROFILE_CONF="$_CCMORPH_DIR/$1.conf" \
    "$resolver" model-window "$1" "$2" 2>/dev/null
}

# Run a command detached: no job-table entry, no SIGHUP when this shell exits.
#
# A FUNCTION, not an inline block, and that is the whole point. `setopt
# localoptions` restores at function return — never at the end of an enclosing
# `{ ... }` — so the same three lines written inline in the launch path left
# `monitor` off for the rest of the launch, including the foreground `claude`.
# Job control then applied to nothing: claude shares the launcher's process group
# instead of getting its own, so Ctrl-Z did not suspend it and terminal signals
# went to the launcher. Proven under a pty (own pgid with monitor on, shared pgid
# with it off). Here the option cannot outlive the spawn.
#
# no_monitor is what makes the child immune to the shell's exit rather than the
# `disown`, which is belt-and-braces: with monitor off the job table is empty, so
# `disown %+` finds nothing and the `|| disown $!` form is the one that applies.
#
# Output goes to the ccmorph log, never /dev/null. A background job whose 401,
# traceback or timeout is discarded cannot be diagnosed after the fact: the user is
# told the work "continues in the background" and nothing ever contradicts it,
# however it ended. Every one of these jobs is silent on its happy path, so the log
# stays quiet until something goes wrong.
_ccm_spawn_bg_logged() {
  setopt localoptions no_monitor
  # The redirection is prepared by THIS shell before the child starts, so an
  # unwritable log aborts the spawn before the executable ever runs — the job
  # silently never happens while the caller reports it started. Same boundary,
  # same fallback as the adapter's own log handling.
  local _log="${_CCMORPH_LOG:-/dev/null}"
  _ccm_log_init
  if ! { : >> "$_log" } 2>/dev/null; then
    _log=/dev/null
  fi
  ( "$@" ) >>"$_log" 2>&1 &
  disown %+ 2>/dev/null || disown $! 2>/dev/null
}

# True when the listing names a model the profile has no size, pin or recent miss
# for, so the sizer is worth a start. A false positive only costs one sizer run.
_ccm_listing_has_unsized() {  # $1 = profile, $2 = /v1/models JSON
  print -r -- "$2" | CCMORPH_STATE_DIR="$_CCMORPH_DIR" _ccm_util listing-has-unsized "$1"
}

# Free metadata only, off add/launch's critical path. Inference requires the
# operator's explicit `ccmorph probe --allow-inference --profile ... --model ...`.
_ccm_probe_nudge() {  # $1 = profile name
  [[ -x "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" ]] || return 0
  _ccm_spawn_bg_logged "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" context-probe \
    --declared-only --profile "$1" --deadline 30
}

# One interpreter start for every window fact a launch asks, as one tab-separated
# line: the launched model's own record ("<window>\t<source>\t<budget>\t<send_id>",
# the --fields contract), the route ceiling's sibling maximum, and the saved
# auto-compact budget the engine's schema keeps. Three helpers each paid a full
# start for one of these (#321, #333). Each part keeps its own try, because each
# used to fail on its own: a broken resolver must not blank the saved budget the
# clamp still needs, and a settings-compose that cannot be read must not take the
# resolver's record with it. The launched model's own window is NOT seeded into
# field 5 -- the caller applies its seed when it reads the ceiling, so the field
# stays the route's own answer. The run happens even with no model: a routed
# launch still needs its ceiling and its saved budget.
_ccm_launch_window() {
  local profile="$1" model="${2:-}"
  if [[ -n "$model" ]]; then
    _ccm_migrate_state || model=""
  fi
  [[ -x "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" ]] || model=""
  command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
    CCMORPH_STATE_DIR="$_CCMORPH_DIR" MODEL_WINDOW_PROFILE_CONF="$_CCMORPH_DIR/$profile.conf" \
    "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" util launch-window "$profile" "$model" \
    "${(f)$(_ccm_settings_sources "$profile")}" 2>/dev/null
}

# Ask a profile's own endpoint what models it serves.
#
# ccmorph deliberately carries no model knowledge: the endpoint is the only
# authority for what exists, the answer is never cached (so a gateway change is
# visible immediately), and the only filter applied is the endpoint's own
# advertised chat_completion capability. No per-model table, alias, or
# capability judgement belongs here.
# scheme://host[:port]/path only: drops userinfo, query and fragment so a URL
# can appear in a message or log without leaking a credential.
_ccm_redact_url() {
  _ccm_util redact-url "$1" 2>/dev/null || print -r -- '[unparseable endpoint]'
}

_ccm_gw_models_json() {
  local name="$1"
  local base key
  REPLY=""

  _ccm_read "$name" || return 1
  _ccm_validate_loaded "$name" || {
    unset _ccm
    return 1
  }

  # Configure may be previewing an unsaved endpoint/key. Never query the old
  # endpoint with the replacement credential, or save just to populate a picker.
  key="${3-${_ccm[api_key]:-}}"
  base="${2-${_ccm[base_url]:-https://api.anthropic.com}}"
  base="${base:-https://api.anthropic.com}"
  unset _ccm

  [[ -n "$key" ]] || {
    _ccm_err "'$name' has no api_key — model discovery needs one"
    return 1
  }

  # No anthropic-version header on purpose. Anthropic-compatible proxies rewrite
  # non-claude ids into "claude-<junk>-<reversed-id>" when they see it, which makes
  # the listing unreadable and non-claude models look absent. The same endpoints
  # return clean ids without it, and the native Anthropic endpoint authenticates
  # before validating the version, so nothing is lost.
  # The core honours the ambient HTTPS_PROXY: a bridge uses it to make a
  # private endpoint reachable.
  REPLY=$(print -r -- "$key" | _ccm_util fetch-listing "$base" models) || {
    _ccm_err "could not reach '$(_ccm_redact_url "$base")' — if it is an internal endpoint, connect your private network and try again"
    return 1
  }
}

_ccm_gw_models() {
  local name="$1"
  _ccm_gw_models_json "$name" "${@:3}" || return 1

  print -r -- "$REPLY" | _ccm_util gw-models "$_CCMORPH_DIR/$name.conf" "${2:-catalogue}" || {
    _ccm_err "'$name' endpoint returned no usable model list"
    return 1
  }
}

# Does this profile's own resolver serve this model?  0 = yes, 1 = no, 2 = cannot tell.
#
# A model recorded in a session is a fact about the profile that RAN it, not about
# the one taking it over. Both carriers -- `hop`, and a direct `--resume` -- hand
# that id to the next launch unconditionally, so hopping a gateway model into a
# Claude profile sends Anthropic a name it has never heard of, and the reverse sends
# a gateway an id it does not serve. Ask first, and let a definite no fall back to
# the target's own default, which is what omitting the model already means.
#
# Whom to ask is the whole design, and there is no third authority to invent:
#
#   * No base_url -- Claude Code resolves the id itself, out of its built-in
#     catalogue. Membership there is the claude- prefix: the same test the launcher
#     already applies when deciding whether an id needs
#     CLAUDE_CODE_MAX_CONTEXT_TOKENS, for the same reason. Free, offline, certain.
#   * A base_url -- the endpoint's own /v1/models, through `_ccm_gw_models`, so the
#     answer routes exactly as `ccmorph models` does and no per-model table is born
#     here either.
#
# A tier alias is not an id and never fails: each profile maps it through its own
# ANTHROPIC_DEFAULT_<TIER>_MODEL, so it is servable wherever it lands.
#
# Everything else -- no key, endpoint down, empty listing -- is ignorance, not
# absence, and answers 2 so the carried model survives. Dropping a model because a
# gateway blinked is the worse of the two failures: the model is the user's choice,
# and the fallback is only ever a guess at it.
_ccm_settings_model_alias() {
  local tier="${(U)2}" value
  [[ "$tier" == (OPUS|SONNET|HAIKU|FABLE) ]] || return 1
  value=$(_ccm_config_store get "$1" "env.ANTHROPIC_DEFAULT_${tier}_MODEL") || return 1
  [[ -n "$value" ]] || return 1
  print -r -- "$value"
}

_ccm_model_known() {  # 0 served, 1 not served, 2 cannot tell
  local name="$1" want="$2" typ base map key
  [[ -n "$name" && -n "$want" ]] || return 2
  _ccm_read "$name" >/dev/null 2>&1 || return 2
  _ccm_validate_loaded "$name" >/dev/null 2>&1 || { unset _ccm; return 2; }
  typ="${_ccm[type]:-}" base="${_ccm[base_url]:-}" map="${_ccm[model_map]:-}" key="${_ccm[api_key]:-}"
  unset _ccm
  print -r -- "$key" | _ccm_util model-served "$want" "$typ" "$base" "$map" 2>/dev/null
}
