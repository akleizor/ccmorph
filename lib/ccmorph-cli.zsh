# ccmorph-cli.zsh — The ccmorph command: the config store, doctor, help, usage and the verb dispatcher.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

# ── Main ─────────────────────────────────────────────────────────────────────

# `-i` / `--interactive` is the ONE spelling of "ask me", at every site that takes a
# choice: a launch, `set <acct> model`, `set <acct> effort`. It is a flag, never a value,
# so it appears once per line and position says what it applies to.
#
# It replaced two earlier spellings, both of which were mistakes worth naming. `@menu` was
# a VALUE, so it had to be repeated per slot -- the documented launch line read
# `ccmorph @menu --model @menu`, one token twice for two different questions, with nothing
# saying which was which. `?` was an alias for it and worse than ambiguous: ccmorph runs as
# an ordinary executable, so the CALLER's shell expands a bare `?` before this process
# starts. Probed both failure modes -- in a clean directory zsh aborts with "no matches
# found: ?" and ccmorph never runs; with any one-character name in the cwd the glob MATCHES
# and `ccmorph set <acct> model ?` silently records that FILENAME as the default, reporting
# success. A `noglob` alias used to paper over that, which meant the fix existed only in
# shells that had sourced the library -- absent on exactly the fresh machines reading the
# docs that advertised it.
_ccm_config_store() {
  _ccm_util config-store "$1" "$_CCMORPH_DIR/$2.conf" "$(_ccm_profile_layer "$2")" "${3:-}" "$HOME/.claude/settings.json" "${_CCMORPH_MODEL_MAPPING_ENV[@]}"
}

ccmorph() {
  emulate -L zsh
  # Shell snapshots can retain this public function without its private library.
  # The executable already owns loading and path resolution; do not duplicate it.
  if (( ! $+functions[_ccm_core] )) || [[ -z "${_CCMORPH_DIR:-}" ]]; then
    command ccmorph "$@"
    return $?
  fi
  # Every verb runs in the core. A launch runs through the fork-and-forward, so
  # `kill <this shell>` still reaches the session.
  if [[ "${1:-}" == run ]]; then
    _ccm_fork_forward _ccm_main_exec "$@"
    return $?
  fi
  local before="" rc
  [[ "${1:-}" == rm && -f "$_CCMORPH_DIR/active" && ! -L "$_CCMORPH_DIR/active" ]] && before=$(<"$_CCMORPH_DIR/active")
  _ccm_core cli main "$@"
  rc=$?
  # A new sign-in, or a removed default, must not keep this shell's old token.
  (( rc == 0 )) && [[ "${1:-}" == add || ( "${1:-}" == rm && -n "$before" && "$before" == "${2:-}" ) ]] &&
    unset CLAUDE_CODE_OAUTH_TOKEN
  return $rc
}
