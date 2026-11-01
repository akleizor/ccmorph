# The launch and its relaunch loop live in ccmorph-core (package launch). This
# shell hands it the paths and env-key lists it owns, and runs it as a child that
# TERM and HUP reach: `kill <launcher pid>` names this shell, not the core.
# Run <command...> (or, after --exec, the external command, replacing the child
# so its parent is this shell) as a child this shell waits on, and forward TERM and HUP to it.
# A foreground child defers the caller's own traps until it exits, so a signal sent
# to the launcher (agent-dispatch, a user's kill) never reached claude. Returns the
# child's exit status.
# The forwarding traps go in before the fork: a signal that lands before the pid is
# known is held in _ccm_sig, and the child exits on it unless the pid gets it below.
# Without job control zsh gives an async job /dev/null for stdin and ignored
# INT/QUIT, so the terminal comes in on a saved fd and the child restores default
# dispositions first. no_monitor keeps claude in this process group (job control
# would give it its own and stop it with SIGTTIN when the library is sourced in an
# interactive shell); it is scoped to this function. The caller's options leak in
# (noclobber breaks `{fd}` redirections), so emulate zsh resets them.
_ccm_fork_forward() {
  emulate -L zsh
  setopt no_monitor no_bg_nice   # bg_nice would run claude at nice +5
  _ccm_forwarded_sig=0
  local _ccm_pid=0 _ccm_sig=0 _ccm_in _ccm_err _ccm_errf
  trap '_ccm_forwarded_sig=15; _ccm_sig=15; (( _ccm_pid )) && kill -TERM $_ccm_pid 2>/dev/null && _ccm_sig=0' TERM
  trap '_ccm_forwarded_sig=1; _ccm_sig=1; (( _ccm_pid )) && kill -HUP $_ccm_pid 2>/dev/null && _ccm_sig=0' HUP
  { exec {_ccm_in}<&0 } 2>/dev/null || _ccm_in=   # stdin closed: nothing to hand on
  # No place for the wait loop's scratch file (unwritable $TMPDIR): run claude in
  # the foreground as before, without forwarding, rather than not at all.
  if ! _ccm_errf=$(command mktemp "${TMPDIR:-/tmp}/ccmorph-wait.XXXXXX" 2>/dev/null) ||
     ! { exec {_ccm_err}<>"$_ccm_errf" } 2>/dev/null; then   # a bare `exec ... 2>/dev/null` would silence this shell's stderr for good
    [[ -n $_ccm_errf ]] && command rm -f -- "$_ccm_errf"
    [[ -n $_ccm_in ]] && exec {_ccm_in}<&-
    if [[ $1 == --exec ]]; then
      shift
      ( exec "$@" )
    else
      ( "$@" )
    fi
    return $?
  fi
  command rm -f -- "$_ccm_errf"
  {
    trap - EXIT TERM HUP INT QUIT
    (( _ccm_sig )) && exit $(( 128 + _ccm_sig ))
    [[ -n $_ccm_in ]] && exec <&$_ccm_in {_ccm_in}<&-
    exec {_ccm_err}<&-
    if [[ $1 == --exec ]]; then
      shift
      exec "$@"
    fi
    "$@"
    exit $?
  } &
  _ccm_pid=$!
  [[ -n $_ccm_in ]] && exec {_ccm_in}<&-
  (( _ccm_sig )) && kill -$_ccm_sig $_ccm_pid 2>/dev/null
  # wait returns early when a trap runs, and zsh files each stop and continue
  # of the child as a status of its own ahead of the exit status. So read statuses
  # until wait reports the pid is no longer this shell's child (its only stderr
  # output): the last one is the child's exit status. stderr goes to a descriptor
  # opened (and its file unlinked) before the fork, so nothing on disk can break
  # the loop, and once the child is gone the loop is bounded whatever wait says.
  local _ccm_st=0 _ccm_r _ccm_n=0
  local -a _ccm_sz
  zmodload -F zsh/stat b:zstat 2>/dev/null
  while :; do
    wait $_ccm_pid 2>&$_ccm_err; _ccm_r=$?
    zstat -A _ccm_sz +size -f $_ccm_err 2>/dev/null && (( _ccm_sz[1] )) && break
    _ccm_st=$_ccm_r
    kill -0 $_ccm_pid 2>/dev/null || (( ++_ccm_n < 1000 )) || break
  done
  exec {_ccm_err}<&-
  return $_ccm_st
}

# The core's own view of this shell: paths, env-key lists and the run's state,
# plus the host's prelaunch opt-in, which local.sh may set without `export`.
_ccm_core_env() {
  reply=(
    _CCMORPH_DIR="$_CCMORPH_DIR"
    _CCMORPH_LIB_DIR="$_CCMORPH_LIB_DIR"
    _CCMORPH_LIBEXEC_DIR="$_CCMORPH_LIBEXEC_DIR"
    _CCMORPH_PROFILE_DIRS="$_CCMORPH_PROFILE_DIRS"
    _CCMORPH_CONTEXT_POLICY="$_CCMORPH_CONTEXT_POLICY"
    _CCMORPH_LOG="${_CCMORPH_LOG:-}"
    _CCMORPH_TUNING_ENV="${_CCMORPH_TUNING_ENV[*]}"
    _CCMORPH_PROFILE_ENV="${_CCMORPH_PROFILE_ENV[*]}"
    _CCMORPH_LAUNCH_ENV="${_CCMORPH_LAUNCH_ENV[*]}"
    _CCMORPH_BACKEND_ENV="${_CCMORPH_BACKEND_ENV[*]}"
    _CCMORPH_MODEL_MAPPING_ENV="${_CCMORPH_MODEL_MAPPING_ENV[*]}"
    _CCMORPH_LAUNCHER_PID=$$
    _CCMORPH_VERSION="$_CCMORPH_VERSION"
    _CCMORPH_ZSH_VERSION="$ZSH_VERSION"
    _CCMORPH_NOTICE="${_ccm_pending_cache_notice:-}"
    _CCMORPH_LAUNCH_PROFILE="${_CCMORPH_LAUNCH_PROFILE:-}"
    _CCMORPH_INTERACTIVE="${_CCMORPH_INTERACTIVE:-0}"
    _CCMORPH_NATIVE_ARGS="${_CCMORPH_NATIVE_ARGS:-}"
    CCMORPH_SUBCOMMAND_LAUNCH="${CCMORPH_SUBCOMMAND_LAUNCH:-0}"
    ${CCMORPH_PRELAUNCH:+"CCMORPH_PRELAUNCH=$CCMORPH_PRELAUNCH"}
  )
}

# Run ccmorph-core with that view.
_ccm_core() {
  local -a reply
  _ccm_core_env
  command env "${reply[@]}" "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" "$@"
}

_ccm_launch_core() {
  local -a reply
  _ccm_core_env
  exec env "${reply[@]}" "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" launch "$@"
}

_ccm_launch() { _ccm_fork_forward _ccm_launch_core "$@"; }

_ccm_main_exec() {
  local -a reply
  _ccm_core_env
  exec env "${reply[@]}" "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" cli main "$@"
}

_ccm_launch_driver() { _ccm_launch "$@"; }

# One stage of the launch each, for callers that need only that stage: Claude Code
# through the session setup with TERM/HUP forwarded, and the gateway picker projection.
_ccm_run_claude() { _ccm_fork_forward _ccm_launch_core --run-claude "$@"; }
_ccm_project_picker() { ( _ccm_launch_core --project-picker "$@" ); }
