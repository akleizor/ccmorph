# Test vocabulary: the old zsh helper names, each a thin call into the Go verb that
# replaced it, so the zsh suite keeps driving the real core. Not shipped.

_ccm_util() { "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" util "$@"; }
_ccm_err() { print -u2 -- "ccmorph: $1"; return 1; }
_ccm_log() { ( _ccm_launch_core --log "$@" ); }

_ccm_profile_layer() {
  local dir="$_CCMORPH_PROFILE_DIRS/$1"
  if [[ -e "$dir/settings.profile.json" || -L "$dir/settings.profile.json" ||
        -e "$dir/cache/settings.base.json" ]]; then
    print -r -- "$dir/settings.profile.json"
  else
    print -r -- "$dir/settings.json"
  fi
}

_ccm_config_store() {
  _ccm_util config-store "$1" "$_CCMORPH_DIR/$2.conf" "$(_ccm_profile_layer "$2")" "${3:-}" \
    "$HOME/.claude/settings.json" "${_CCMORPH_MODEL_MAPPING_ENV[@]}"
}

_ccm_cleanup_stale() {
  _ccm_util gc-stale "$_CCMORPH_DIR" "$_CCMORPH_PROFILE_DIRS" "${TMPDIR:-/tmp}" 2>/dev/null || :
}

# Provisioning runs through the public path, which reads and validates the conf.
_ccm_setup_dir_unlocked() { ccmorph _env "$1" >/dev/null; }
_ccm_setup_dir() { ccmorph _env "$1" >/dev/null; }
_ccm_dir_ok() { _ccm_util profile-ok "$_CCMORPH_PROFILE_DIRS" "$1"; }

_ccm_clamp_acw() { local _v; _v=$(_ccm_util clamp-acw "${(P)1}") && : ${(P)1::=$_v}; }
_ccm_engine_int() { _ccm_util engine-int "${1:-}" 2>/dev/null; }

_ccm_listing_has_unsized() {  # $1 = profile, $2 = /v1/models JSON
  print -r -- "$2" | CCMORPH_STATE_DIR="$_CCMORPH_DIR" _ccm_util listing-has-unsized "$1"
}

# The launch's own seams, one stage of `ccmorph-core launch` each.
_ccm_invariant_warn() { ( _ccm_launch_core --invariant "$@" ); }
_ccm_launch_window() { ( _ccm_launch_core --launch-window "$@" ); }
_ccm_fold_settings() { ( _ccm_launch_core --fold "$@" ); }
# The launch env the old helper read from its caller's scope is passed explicitly.
_ccm_hazard_set() { ( _ccm_launch_core --hazard-set "$1" "$2" "$3" "${4:-}" -- "${penv[@]}" ); }

_ccm_read() {  # fills the global assoc _ccm from the core's profile reader
  typeset -gA _ccm=()
  local name="$1" readonly="${2:-0}" out
  out=$(_ccm_util profile-read "$_CCMORPH_DIR" "$name" $(( ! readonly )) \
    "${(j:,:)_CCMORPH_PROFILE_ENV}" "${(j:,:)_CCMORPH_MODEL_MAPPING_ENV}") || {
    _ccm_err "${out:-could not read profile '$name'}"
    unset _ccm
    return 1
  }
  out="${out%.}"
  [[ -z "$out" ]] || _ccm=("${(@0)${out%$'\0'}}")
}

_ccm_validate_loaded() {
  [[ "${_ccm[type]:-}" == apikey ]] || return 0
  local out k
  out=$(for k in type api_key base_url adapter; do print -rn -- "$k"$'\0'"${_ccm[$k]:-}"$'\0'; done |
    _ccm_util profile-validate "$1") || { _ccm_err "$out"; return 1; }
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

_ccm_gw_models_json() {  # REPLY = the profile's /v1/models listing
  REPLY=$( _ccm_launch_core --models-json "$@" )
}

_ccm_gw_models() {
  local name="$1"
  _ccm_gw_models_json "$name" "${@:3}" || return 1
  print -r -- "$REPLY" | _ccm_util gw-models "$_CCMORPH_DIR/$name.conf" "${2:-catalogue}" || {
    _ccm_err "'$name' endpoint returned no usable model list"
    return 1
  }
}

_ccm_migrate_projects() { _ccm_util migrate-projects "$1" "$2"; }
_ccm_model_window() { ( _ccm_launch_core --model-window "$@" ); }
_ccm_migrate_state() { ( _ccm_launch_core --migrate-state ); }
_ccm_prelaunch() { ( _ccm_launch_core --prelaunch "$@" ); }
_ccm_remove_profile_dir() { ( _ccm_launch_core --remove-profile "$@" ); }
_ccm_carry_session() { ( _ccm_launch_core --carry-session "$@" ); }
_ccm_claude_verbs() { command claude --help 2>/dev/null | _ccm_util claude-verbs; }
_ccm_is_claude_subcommand() { ( _ccm_launch_core --claude-subcommand "$1" ) 2>/dev/null; }
_ccm_resume_model() { REPLY=$( _ccm_launch_core --resume-model "$1" ) && [[ -n "$REPLY" ]]; }
_ccm_trigger_env() {  # $1 $2 $3 = names for pct, output reserve, 1M switch, read from penv
  local -a _t
  _t=("${(@f)$( ( _ccm_launch_core --trigger-env "${penv[@]}" ); print x )}")
  : ${(P)1::=${_t[1]}} ${(P)2::=${_t[2]}} ${(P)3::=${_t[3]}}
}
_ccm_probe_nudge() { ( _ccm_launch_core --probe-nudge "$@" ); }
_ccm_pick_value() { ccmorph _ui pick "$@"; }
_ccm_unwire_statusline() {  # $1 = a profile dir; only a ccmorph-installed statusline goes
  local file="$1/settings.json"
  [[ -f "$file" && ! -L "$file" ]] || return 0
  _CCM_SL_OWNED="$_CCMORPH_DIR/statusline"$'\n'"$_CCMORPH_LIB_DIR/statusline" \
    _ccm_util unwire-statusline "$file" 2>/dev/null || :
}
