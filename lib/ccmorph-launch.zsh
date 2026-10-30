# ccmorph-launch.zsh — Launching Claude Code: the project picker, the forked claude child, _ccm_launch and the relay driver.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

_ccm_project_picker() {
  # Write the profile's own /model rows, so the list is a probe and not a memory.
  #
  # Claude Code builds a picker row three ways and only one lets a description
  # through. The built-in catalogue is its own; gateway discovery takes our label but
  # hardcodes "From gateway" on every row it adds; and `additionalModelOptionsCache`
  # in the profile's state file is read back verbatim -- value, label and description.
  # So the rows go there, and discovery is left off for such a profile, since it is
  # added first and its hardcoded string would win the dedupe.
  #
  # Everything here is derived. Ids and names come from the endpoint as it answers now.
  # Every chat-capable model the endpoint serves is offered, probed or not: an
  # unprobed model resolves to Claude Code's 200,000 default, which under-promises,
  # so hiding it only costs a model the user could have chosen. Session safety belongs
  # to the live advisory and opt-in withhold, which know the actual launch budget and
  # compaction percentage.
  #
  # A full replacement, like the MCP projection: a model the endpoint has withdrawn
  # stops being offered instead of lingering. Best-effort throughout -- an unreachable
  # endpoint leaves both files untouched, because yesterday's rows are still true and
  # a model list changes far less often than a network does.
  local name="$1" base="$2" key="$3" own="${4:-}" tag="${5:-Gateway}" launch="${6:-}"
  local home="${_CCMORPH_PROFILE_DIRS}/$name"
  [[ -f "$home/.claude.json" && ! -L "$home/.claude.json" ]] || return 0
  [[ -n "$base" && -n "$key" ]] || return 0

  # Picker suffixes use the same profile context resolver as launch.
  local context_conf="$_CCMORPH_DIR/$name.conf"

  local body=""
  if _ccm_models_listing "$base" "$key" 8; then
    body="$REPLY"
  fi

  _CCM_MODELS_JSON="$body" _ccm_util project-picker "$home" "$context_conf" "$own" "$tag" "$name" \
    "$(_ccm_profile_layer "$name")" "$launch" || return $?
  return 0
}

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

# Runs claude with the profile's environment in a child process (see
# _ccm_fork_forward), so the unsets and exports below never reach this shell.
_ccm_run_claude() {
  _ccm_fork_forward _ccm_claude_session "$@"
}

_ccm_claude_session() {
  unset CLAUDE_CODE_EFFORT_LEVEL
  unset CLAUDE_CONFIG_DIR
  unset CLAUDE_SECURESTORAGE_CONFIG_DIR
  unset CLAUDE_CODE_SUBAGENT_MODEL
  unset CCMORPH_RANDOM_DEVICE_ID CCMORPH_AFFINITY_HEADER CCMORPH_USER_AGENT CCMORPH_QUIRKS CCMORPH_STALL_TIMEOUT CCMORPH_UPSTREAM_PROXY CCMORPH_SHIM_HOOKS CCMORPH_MASK CCMORPH_MASK_COMMANDS
  local _private=0

  # Clear every key a profile could declare or a launch sets, so one that is
  # absent this time gets Claude Code's default rather than whatever this shell
  # -- possibly an outer ccmorph session -- happens to export.
  # Tuning keys are the exception: the shell's value passes through unless the
  # profile declares the key (below) -- except a name an outer ccmorph session
  # recorded in CCMORPH_SET_ENV, which is that session's value, not the shell's.
  local _e
  # Only names on the tuning list are trusted from the inherited variable.
  for _e in "${(@)${(@s: :)${CCMORPH_SET_ENV:-}}:*_CCMORPH_TUNING_ENV}" \
            "${(@)_CCMORPH_PROFILE_ENV:|_CCMORPH_TUNING_ENV}" \
            "${_CCMORPH_LAUNCH_ENV[@]}" "${_CCMORPH_BACKEND_ENV[@]}"; do
    [[ -n "$_e" ]] && unset "$_e"
  done
  unset CCMORPH_SET_ENV
  # `claude --bare` sets CLAUDE_CODE_SIMPLE=1 in its own env, which no launch
  # records in CCMORPH_SET_ENV; inside a Claude Code child session (CLAUDE_CODE_CHILD_SESSION=1) it
  # is that session's, not the shell's, so a nested launch must not inherit it.
  [[ "${CLAUDE_CODE_CHILD_SESSION:-}" == 1 ]] && unset CLAUDE_CODE_SIMPLE
  local -a _set_env=()

  while [[ $# -gt 0 && "$1" != -- ]]; do
    case "$1" in
      CCMORPH_RANDOM_DEVICE_ID=*|CCMORPH_AFFINITY_HEADER=*|CCMORPH_USER_AGENT=*|CCMORPH_QUIRKS=*|CCMORPH_STALL_TIMEOUT=*|CCMORPH_UPSTREAM_PROXY=*|CCMORPH_SHIM_HOOKS=*|CCMORPH_MASK=*|CCMORPH_MASK_COMMANDS=*) ;;
      CCMORPH_PRIVATE=1) _private=1 ;;
      # Tuning keys the profile's settings layers set (the caller computes them):
      # Claude Code exports those to its children, so record them like env. lines.
      CCMORPH_SET_ENV=*) _set_env+=(${(s: :)${1#*=}}) ;;
      *) export "$1" || exit 1
         (( ${_CCMORPH_TUNING_ENV[(Ie)${1%%=*}]} )) && _set_env+=("${1%%=*}") ;;
    esac
    shift
  done
  (( ${#_set_env} )) && export CCMORPH_SET_ENV="${_set_env[*]}"

  [[ "$1" == -- ]] || exit 1
  shift

  # private=1: the session runs from a throwaway view of the profile. Config and
  # settings are linked in; transcripts, prompt history and session state land in
  # the throwaway dir and are deleted when Claude Code exits.
  # EXIT does not fire when zsh dies of a signal, so while the view is built TERM
  # and HUP get their own trap: remove the dir, exit 128+signal. Once claude is
  # started they are forwarded to it instead (_ccm_fork_forward) and the launcher exits with
  # claude's status. INT and QUIT are no-op traps: Claude Code handles
  # Ctrl-C itself and keeps running, and Ctrl-\ (group SIGQUIT) kills only claude,
  # so the launcher must neither die of them (that skips EXIT and leaks the dir)
  # nor exit 130/131 over claude's real status; EXIT
  # cleans up when claude ends. Only SIGKILL still leaves the dir in $TMPDIR
  # until reboot; sweep stale ccmorph-private.* dirs if that ever matters.
  if (( _private )) && [[ -d "${CLAUDE_CONFIG_DIR:-}" ]]; then
    # Global (this runs in its own process): exit unwinds to the caller before the
    # EXIT trap runs, and a local would be gone by then.
    local _f; typeset -g _ccm_priv
    _ccm_priv=$(command mktemp -d "${TMPDIR:-/tmp}/ccmorph-private.XXXXXX") || exit 1
    trap 'command rm -rf -- "$_ccm_priv"' EXIT
    trap 'command rm -rf -- "$_ccm_priv"; exit 143' TERM
    trap 'command rm -rf -- "$_ccm_priv"; exit 129' HUP
    trap : INT QUIT
    # Readers name the profile by the config dir's basename (statusline, window resolver).
    local _ccm_view="$_ccm_priv/${CLAUDE_CONFIG_DIR:t}"
    command mkdir "$_ccm_view" || exit 1
    for _f in "$CLAUDE_CONFIG_DIR"/*(DN); do
      case "${_f:t}" in
        projects|sessions|session-env|file-history|history.jsonl|shell-snapshots|paste-cache|debug|todos|tasks|plans|backups) ;;
        *) command ln -s "$_f" "$_ccm_view/${_f:t}" || exit 1 ;;
      esac
    done
    # Claude Code keys the keychain item by this var, else by CLAUDE_CONFIG_DIR; pin it
    # to the real profile dir so the private view reads the profile's own sign-in.
    export CLAUDE_SECURESTORAGE_CONFIG_DIR="$CLAUDE_CONFIG_DIR"
    export CLAUDE_CONFIG_DIR="$_ccm_view"

    # A foreground child defers the TERM/HUP traps until it exits, so neither
    # reached claude: start it through the same fork-and-forward helper as the
    # launcher itself. The cleanup traps above cover the window before the fork;
    # the helper's forwarding traps replace them while claude runs.
    # A hop notice waits for private-view setup as well as profile preflight. It is
    # written from a subshell so a stdout without a reader (SIGPIPE) kills only that
    # write, not this shell before its cleanup.
    [[ -n "${_ccm_pending_cache_notice:-}" ]] && ( print -r -- "$_ccm_pending_cache_notice" ) 2>/dev/null
    _ccm_fork_forward --exec command claude "$@"
    exit $?
  fi

  [[ -n "${_ccm_pending_cache_notice:-}" ]] && ( print -r -- "$_ccm_pending_cache_notice" ) 2>/dev/null
  exec command claude "$@"
}

# A launch started from inside a Remote Control bridge session still sends its
# api.anthropic.com inference through that session's edge: the bridge exports the
# edge's loopback proxy as HTTPS_PROXY, and the proxy is inherited by design. The
# route's behavioural flags describe the endpoint at the other end of that proxy,
# not the profile being launched, so clearing them (as every launch does) sends a
# request the route rejects. The session publishes the flags it was given in one
# variable, CCMORPH_EDGE_ROUTE_ENV (KEY=VALUE per line; the seam, documented in
# docs/ARCHITECTURE.md). Print those lines for the keys given, only while the caller's
# proxy is a loopback one, which is what ties them to this session's edge.
_ccm_nested_edge_env() {
  local proxy="${HTTPS_PROXY:-${https_proxy:-}}" line
  [[ "$proxy" == http://127.0.0.1:<1-65535>(|/) ]] || return 0
  for line in "${(@f)CCMORPH_EDGE_ROUTE_ENV:-}"; do
    [[ "$line" == *=* ]] && (( ${@[(Ie)${line%%=*}]} )) && print -r -- "$line"
  done
  return 0
}

_ccm_launch() {
  local profile="${_CCMORPH_LAUNCH_PROFILE:-}"
  local args=()
  local arg
  local profile_seen=0
  [[ -n "$profile" ]] && profile_seen=1
  local interactive="${_CCMORPH_INTERACTIVE:-0}"

  # Ctrl-C (incl. the double Ctrl-C used to quit) must terminate the launched
  # claude — a child, which still receives the terminal's SIGINT with its
  # default disposition — but must NOT abort this wrapper loop. Without this,
  # SIGINT unwinds the loop before it can act on a profile switch queued via
  # `ccmorph hop`, so the switch is silently lost. localtraps +
  # localoptions restore the shell's INT handler when the function returns.
  # noksharrays for the same local reason: this body and everything it calls
  # index 1-based arrays, and a caller's KSH_ARRAYS would shift every subscript
  # (an argument loop that starts at the second element, a window record read one
  # field over) for as long as the launch runs.
  setopt localoptions localtraps noksharrays
  trap ':' INT

  # EXIT only, and deliberately not INT: the INT handler above must stay ':' or a
  # Ctrl-C unwinds the relay loop and loses a queued profile switch. localtraps
  # means this fires when the function returns, so an adapter started for this
  # session cannot outlive it on any return path.
  trap '_ccm_provider_stop; _ccm_adapter_stop' EXIT

  # TERM/HUP must run the same teardown: EXIT alone leaves the adapter and
  # bridge running on the ordinary kill paths (tmux kill-session, adapter
  # escalation). Each trap re-arms the default disposition and re-raises so the
  # exit status still reports the signal.
  trap '_ccm_provider_stop; _ccm_adapter_stop; trap - TERM; kill -s TERM $$' TERM
  trap '_ccm_provider_stop; _ccm_adapter_stop; trap - HUP; kill -s HUP $$' HUP

  if [[ "${_CCMORPH_NATIVE_ARGS:-}" == 1 ]]; then
    args=("$@")
    set --
  fi

  while [[ $# -gt 0 ]]; do
    arg="$1"

    case "$arg" in
      --)
        args+=(--)
        shift
        args+=("$@")
        break
        ;;

      # -a, --profile and --profile=<name> are ONE flag (POSIX short option +
      # its long alias; GNU getopt_long treats `--opt value` and `--opt=value`
      # as equivalent). Diagnostics name the spelling the user actually typed —
      # reporting "-a" to someone who wrote --profile sends them looking for an
      # argument they never passed.
      -a|--profile)
        (( $# >= 2 )) &&
          [[ -n "$2" && "$2" != -- ]] || {
          _ccm_err "$arg requires an profile name"
          return 2
        }

        (( profile_seen == 0 )) || {
          _ccm_err "$arg may be specified only once"
          return 2
        }

        profile="$2"
        profile_seen=1
        shift 2
        continue
        ;;

      --profile=*)
        (( profile_seen == 0 )) || {
          _ccm_err "--profile= may be specified only once"
          return 2
        }

        profile="${arg#--profile=}"

        [[ -n "$profile" ]] || {
          _ccm_err "--profile= requires an profile name"
          return 2
        }

        profile_seen=1
        shift
        continue
        ;;

      # One flag means "ask me", and it means the same thing everywhere: fill in
      # whatever this launch has not already been told. Position carries the rest
      # -- `ccmorph run -i` has no profile yet so it asks for both;
      # `ccmorph run <profile> -i` already has a profile, so asks only for its model.
      #
      # This replaces a `@menu` VALUE (and a `?` alias for it). A value-shaped
      # sentinel had to be repeated once per slot, so the documented line read
      # `ccmorph @menu --model @menu` -- the same token twice, meaning two
      # different questions, with nothing on the line saying which was which. `?`
      # was worse than ambiguous: it is a zsh glob, so the caller's shell ate it
      # before ccmorph started, and in a directory holding any one-character file
      # `set <acct> model ?` silently recorded that FILENAME as the default and
      # reported success.
      -i|--interactive)
        interactive=1
        shift
        continue
        ;;

      # Every other native option steps over the value it owns, the way the
      # binary parses its own argv. An option VALUE can look exactly like one of
      # the flags above -- `--system-prompt --profile other` hands Claude Code
      # the literal text `--profile` as the prompt -- and the per-token match
      # claimed that value as this launcher's flag, refusing the launch or
      # eating the token (#405). The walker consults `claude --help` for its
      # arity table, and this scan runs before the profile's settings policy
      # is verified: a launch that will refuse must not start the binary
      # (#308, #319). Stepping only changes what the flag cases above claim,
      # so the walker is asked only while such a token is still ahead of the
      # option; with none ahead, the option and any value it owns forward
      # verbatim either way and the probe is skipped.
      -?*)
        if _ccm_launch_has_flag_ahead "$@"; then
          _ccm_claude_option_step "${@:1:2}"
        else
          REPLY=1
        fi
        if (( REPLY == 2 )); then
          args+=("$1" "$2")
          shift 2
        else
          args+=("$arg")
          shift
        fi
        continue
        ;;

      *)
        args+=("$arg")
        shift
        ;;
    esac
  done

  # Ask for what was not given, in dependency order: the profile owns the endpoint
  # and credential the model list comes from, so it must be resolved first.
  if (( interactive )) && [[ -z "$profile" ]]; then
    profile=$(_ccm_menu_profile) || return 1
    [[ -n "$profile" ]] || {
      _ccm_err "no profile chosen"
      return 1
    }
  fi

  if [[ -n "$profile" ]]; then
    _ccm_valid "$profile" || {
      _ccm_err "invalid profile name '$profile'"
      return 2
    }
  elif _ccm_read_active; then
    profile="$REPLY"
  fi

  # No hardcoded default: all credential types use their configured profile model.
  # Without one, the native engine chooses; inline choices are not re-imported.
  local default_model=""
  local profile_tag=""
  local _is_routed=0
  local needs_adapter=0

  if [[ -n "$profile" ]]; then
    _ccm_exists "$profile" || {
      _ccm_err "no profile '$profile'"
      return 1
    }

    _ccm_read "$profile" || return 1

    _ccm_validate_loaded "$profile" || {
      unset _ccm
      return 1
    }

    local _is_provider=0
    case "${_ccm[type]}" in
      apikey)
        _is_routed=1
        profile_tag="${_ccm[tag]:-}"
        case "${_ccm[base_url]:-}" in
          ""|https://api.anthropic.com|https://api.anthropic.com/) ;;
          *) needs_adapter=1 ;;
        esac
        ;;
      provider)
        _is_routed=1
        _is_provider=1
        profile_tag="${_ccm[tag]:-${_ccm[provider]:-}}"
        ;;
    esac

    unset _ccm

    # Freeze legacy native defaults before the first session can change them.
    # Listing and launch then read the same ccmorph-owned values.
    default_model=$(_ccm_config_store seed-model-defaults "$profile" model) || return 1
  fi

  local explicit_model=""
  local resume_session=""
  local relay_launch_args=()
  local skip_model=0 ask_missing_model=1 user_model=0 resume_carried=0
  # Set by the dispatcher; it only keeps session-only work (the picker) off a
  # subcommand.
  local _ccm_is_subcommand_launch="${CCMORPH_SUBCOMMAND_LAUNCH:-0}"
  # The model this launch hands Claude Code, through ANTHROPIC_MODEL. The caller's
  # own --model is never rewritten: Claude Code lets the flag beat the variable.
  local launch_model=""
  local i=1

  while (( i <= ${#args} )); do
    arg="${args[$i]}"

    case "$arg" in
      --)
        ask_missing_model=0
        break
        ;;

      --model)
        (( i < ${#args} )) &&
          [[ -n "${args[$(( i + 1 ))]}" &&
             "${args[$(( i + 1 ))]}" != -- ]] || {
          _ccm_err "--model requires a value"
          return 2
        }
        if [[ "${args[$(( i + 1 ))]}" == (-i|--interactive) ]]; then
          interactive=1
          args[$i,$(( i + 1 ))]=()
          continue
        fi

        explicit_model="${args[$(( i + 1 ))]}"
        user_model=1
        skip_model=1
        (( i += 2 ))
        continue
        ;;

      --model=*)
        explicit_model="${arg#--model=}"

        [[ -n "$explicit_model" ]] || {
          _ccm_err "--model requires a value"
          return 2
        }

        user_model=1
        skip_model=1
        ;;

      --dangerously-skip-permissions)
        # Unlike model and effort, permission bypass is process launch state and
        # Claude Code does not restore it from the resumed session transcript.
        relay_launch_args+=("$arg")
        ;;

      -r|--resume)
        ask_missing_model=0
        if (( i < ${#args} )) &&
           [[ -n "${args[$(( i + 1 ))]}" &&
              "${args[$(( i + 1 ))]}" != -* ]]; then
          resume_session="${args[$(( i + 1 ))]}"
        fi

        # A resumed session normally keeps its own recorded model, so ccmorph
        # stays out of the way. But Claude Code cannot restore a proxy/custom
        # model: it validates the recorded name against its built-in list and,
        # failing that, silently falls back to a default Anthropic model. So for
        # proxy profiles ccmorph must keep injecting --model to re-force the
        # backend on resume.
        (( _is_routed )) || skip_model=1
        ;;

      --resume=*)
        ask_missing_model=0
        resume_session="${arg#--resume=}"
        (( _is_routed )) || skip_model=1
        ;;

      -c|--continue|--from-pr)
        ask_missing_model=0
        (( _is_routed )) || skip_model=1
        ;;

      -p|--print|--print=*|-h|--help|-v|--version|--bg|--background|--from-pr=*)
        ask_missing_model=0
        ;;

      # Every other native option steps over the value it owns, if any, the
      # way the binary itself parses. An option VALUE can look exactly like
      # one of the flags above — `--system-prompt --continue` is a prompt, not
      # a resume — and reading the value as the flag made a fresh launch
      # behave as a resumed one (#334). The slice hands the walker this token
      # and the one after it when one exists.
      -?*)
        _ccm_claude_option_step "${args[@]:$(( i - 1 )):2}"
        (( REPLY == 2 )) && (( ++i ))
        ;;
    esac

    (( ++i ))
  done

  # `-i` with no `--model` asks for the model too — but ONLY where ccmorph owns a
  # model list. A routed profile has an endpoint to enumerate; a native Anthropic
  # profile does not, and Claude Code owns its own model choice there. Asking anyway
  # sent a discovery request to api.anthropic.com and failed the launch with
  # "could not reach", which is the tool inventing a question it cannot answer.
  #
  # This is launch wiring: ccmorph resolves the choice to a plain model name and
  # hands that to Claude Code, learning nothing about the model itself. A cancelled
  # picker aborts the launch rather than falling back to some default.
  #
  # An explicit `--model <id>` wins and asks nothing: the caller already answered
  # the question the flag exists to ask.
  # A fresh custom-endpoint session needs a concrete model before compatibility
  # detection. Reuse the picker on a terminal, but never interrupt headless runs,
  # subcommands, or resume; those keep their explicit/default/native choices.
  if (( needs_adapter && ask_missing_model && ! _ccm_is_subcommand_launch )) &&
     [[ -z "$explicit_model" && -z "$default_model" && -t 0 && -t 2 ]]; then
    interactive=1
  fi
  if (( interactive )) && [[ -z "$explicit_model" ]] && (( _is_routed )); then
    [[ -n "$profile" ]] || {
      _ccm_err "-i needs an profile: name one, or set a default with ccmorph use <profile>"
      return 2
    }

    local picked
    picked=$(ccmorph _menu "$profile") || return 1

    [[ -n "$picked" ]] || {
      _ccm_err "no model chosen"
      return 1
    }

    explicit_model="$picked"
    launch_model="$picked"
    skip_model=1
  fi

  if (( _is_routed )) &&
     [[ -z "$explicit_model" && -n "$resume_session" ]] &&
     _ccm_resume_model "$resume_session"; then
    # Resolve the carried model before deriving a Claude send id. A routed profile
    # with no saved default still needs the transcript model re-forced: Claude Code
    # cannot restore a custom backend from its native catalogue alone.
    local _rs_model="$REPLY"
    _ccm_model_known "$profile" "$_rs_model"
    case $? in
      1)
        print "${_ccm_yellow}ccmorph:${_ccm_reset} ${_ccm_bold}${_rs_model}${_ccm_reset} is not served by ${_ccm_bold}${profile}${_ccm_reset} — resuming on its default model."
        ;;
      *)
        default_model="$_rs_model"
        resume_carried=1
        ;;
    esac
  fi

  # Derived send_id. For claude-* ids Claude Code ignores
  # CLAUDE_CODE_MAX_CONTEXT_TOKENS: the window is set by CHOOSING THE ID, and a
  # bare id budgets 200,000 behind a foreign base URL even when the route holds
  # 1M (claude-opus-5 shipped exactly this). The canonical resolver answers with
  # the id to launch: <id>[1m] when the route's probed ceiling reaches 1M, the
  # bare id below it (claude-haiku-4-5[1m] would report 1M against a real 200k
  # wall). No resolver on this host -> the id passes through unchanged.
  local _send_target=""
  if [[ -n "$explicit_model" ]]; then
    _send_target="$explicit_model"
  else
    _send_target="$default_model"
  fi
  if [[ "$_send_target" == claude-* ]] && (( needs_adapter || _is_provider )); then
    # The id rides the fields line the window reader already asks for, so this
    # costs ONE interpreter rather than a lookup plus a parse of its JSON.
    local _mw_send=""
    local -a _mw_rec=("${(@ps:\t:)$(_ccm_model_window_fields "$profile" "$_send_target")}")
    _mw_send="${_mw_rec[4]:-}"
    if [[ -n "$_mw_send" && "$_mw_send" != "$_send_target" ]]; then
      _ccm_log "send_id derived profile=$profile from=$_send_target to=$_mw_send"
      if (( user_model )); then
        print -u2 -- "${_ccm_dim}ccmorph: this route serves ${_mw_send}; --model ${explicit_model} runs at Claude Code's own window for that id${_ccm_reset}"
      elif [[ -n "$explicit_model" ]]; then
        explicit_model="$_mw_send"
        launch_model="$_mw_send"
      else
        default_model="$_mw_send"
      fi
    fi
  fi

  # The configured default reaches Claude Code as ANTHROPIC_MODEL, never as an
  # inserted --model. The environment is read by every entry point -- a session,
  # any subcommand, the sessions Remote Control spawns -- and changes nothing on
  # the command line, so ccmorph never has to know Claude Code's grammar. The
  # variable also re-forces a routed model on --resume, which Claude Code would
  # otherwise replace with its own default; a caller's --model still wins.

  if (( ! skip_model )) && [[ -n "$default_model" ]]; then
    launch_model="$default_model"
  fi

  local relay_pid
  relay_pid=$(command sh -c 'printf "%s" "$PPID"') || return 1

  local relay_id="${relay_pid}.${EPOCHREALTIME//./_}.${RANDOM}"
  local relay_file="$_CCMORPH_DIR/.reswitch.${relay_id}"
  local penv=()
  local env_output
  local rs_profile rs_session rs_model rs_effort
  local rs_k rs_v
  local _ccm_rc

  _ccm_relay_valid "$relay_id" || {
    _ccm_err "could not create relay identifier"
    return 1
  }

  # Best-effort: stale-relay and legacy-overlay GC must never block a launch.
  _ccm_cleanup_stale

  command rm -f "$relay_file" || return 1

  _ccm_log "launch profile=${profile:-<none>} relay_id=$relay_id"

  while true; do
    if [[ -n "$profile" ]]; then
      env_output=$(ccmorph _env "$profile") || {
        command rm -f "$relay_file"
        return 1
      }

      penv=("${(@f)env_output}")

      # Carry edits Claude Code made to the generated settings.json into the
      # profile layer first, so every reader below sees them. Best-effort, like
      # the projection itself.
      _ccm_fold_settings "$profile" || :

      # Provider profiles use an isolated cliproxyapi process as both endpoint
      # and credential source. The launcher injects the ephemeral port and
      # temporary key into penv; no adapter is needed.
      if (( _is_provider )); then
        if [[ "${_ccm_provider_active:-}" != "$profile" ]]; then
          _ccm_provider_stop
          _ccm_provider_start "$profile" || {
            command rm -f "$relay_file"
            return 1
          }
        fi
        # Inject the bridge endpoint and temporary key into the launch env
        local _bi=1 _found_base=0 _found_key=0
        while (( _bi <= ${#penv} )); do
          case "${penv[$_bi]}" in
            ANTHROPIC_BASE_URL=*) penv[$_bi]="ANTHROPIC_BASE_URL=http://127.0.0.1:${_ccm_provider_port}"; _found_base=1 ;;
            ANTHROPIC_API_KEY=*)  penv[$_bi]="ANTHROPIC_API_KEY=${_ccm_provider_key}"; _found_key=1 ;;
          esac
          (( ++_bi ))
        done
        (( _found_base )) || penv+=("ANTHROPIC_BASE_URL=http://127.0.0.1:${_ccm_provider_port}")
        (( _found_key ))  || penv+=("ANTHROPIC_API_KEY=${_ccm_provider_key}")
      fi

      # Discovery belongs to the real gateway, not the adapter's partial protocol
      # cache. Capture its endpoint before redirecting inference to localhost.
      local _pv="" _pjb="" _pjk=""
      for _pv in "${penv[@]}"; do
        case "$_pv" in
          ANTHROPIC_BASE_URL=*) _pjb="${_pv#*=}" ;;
          ANTHROPIC_API_KEY=*)  _pjk="${_pv#*=}" ;;
          ANTHROPIC_AUTH_TOKEN=*) [[ -z "$_pjk" ]] && _pjk="${_pv#*=}" ;;
        esac
      done
      # Size models the gateway added since the last launch, so the picker rows and
      # this launch's budget below already know them. Reuses the listing the picker
      # fetches (memoised); with nothing new it is a local comparison. Best-effort.
      # Provider profiles size through their running bridge: its port and key are
      # ephemeral, but the route fingerprint reads only the conf, so values persist.
      if _ccm_models_listing "$_pjb" "$_pjk" 8 &&
         _ccm_listing_has_unsized "$profile" "$REPLY"; then
        _ccm_log_init
        print -r -- "$REPLY" | CCMORPH_STATE_DIR="$_CCMORPH_DIR" \
          ANTHROPIC_BASE_URL="$_pjb" ANTHROPIC_API_KEY="$_pjk" \
          "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" context-probe --listing-stdin --profile "$profile" \
          2>>"${_CCMORPH_LOG:-/dev/null}" || :
      fi
      # Complete projection before budgets and the generated settings read its output.
      # The launch model goes with it: a hop relaunches on a model_map alias, and the
      # allowlist it writes must cover what this session is about to ask for.
      _ccm_project_picker "$profile" "$_pjb" "$_pjk" "$default_model" "$profile_tag" \
        "$explicit_model" || return 1

      # An adapter left running for a profile we are no longer on would retain a
      # rendered credential for the rest of the session, so reconcile it too.
      if (( needs_adapter )); then
        if [[ "${_ccm_adapter_profile:-}" != "$profile" ]]; then
          _ccm_adapter_stop

          local _adapter_model="${explicit_model:-$default_model}"
          local _adapter_bare="${_adapter_model%%\[*}" _adapter_alias
          case "$_adapter_bare" in
            opus|sonnet|haiku|fable)
              for _adapter_alias in "${penv[@]}"; do
                [[ "$_adapter_alias" == "ANTHROPIC_DEFAULT_${(U)_adapter_bare}_MODEL="* ]] && _adapter_model="${_adapter_alias#*=}"
              done
              if [[ "${_adapter_model%%\[*}" == "$_adapter_bare" ]]; then
                _adapter_model=$(_ccm_settings_model_alias "$profile" "$_adapter_bare") || {
                  _ccm_err "no gateway model is mapped to tier '$_adapter_bare'; choose an explicit --model id"
                  return 1
                }
              fi
              ;;
          esac
          _ccm_adapter_start "$profile" "$_adapter_model" || {
            command rm -f "$relay_file"
            return 1
          }
        fi

        # Redirect only what Claude Code sees. The profile keeps its real base_url
        # so `ccmorph _env` stays truthful for every other consumer.
        local _pi=1
        while (( _pi <= ${#penv} )); do
          if [[ -n "$_ccm_adapter_port" && "${penv[$_pi]}" == ANTHROPIC_BASE_URL=* ]]; then
            penv[$_pi]="ANTHROPIC_BASE_URL=http://127.0.0.1:${_ccm_adapter_port}"
          fi
          (( ++_pi ))
        done
        # Task tags: label every request of this session for the gateway shim, which
        # records them on the usage row and never changes the model. Only valid shapes
        # pass. The user's own header lines, when the settings layers carry any, are
        # merged in front of these after the diagnostics read below.
        local _ccm_hdrs=""
        [[ "${CCMORPH_TASK:-}" =~ '^[A-Za-z0-9._-]{1,64}$' ]] && _ccm_hdrs+="x-ccmorph-task: $CCMORPH_TASK"$'\n'
        [[ "${CCMORPH_ATTEMPT:-}" =~ '^[0-9]{1,3}$' ]] && _ccm_hdrs+="x-ccmorph-attempt: $CCMORPH_ATTEMPT"$'\n'
        [[ -n "$_ccm_hdrs" ]] && penv+=("ANTHROPIC_CUSTOM_HEADERS=${_ccm_hdrs%$'\n'}")
      else
        _ccm_adapter_stop
      fi
      (( ${#penv} > 0 )) || {
        _ccm_err "profile '$profile' produced no environment"
        command rm -f "$relay_file"
        return 1
      }

      # Inside a bridge session the route's own flags travel with the proxy (see
      # _ccm_nested_edge_env). Only a launch that dials api.anthropic.com is
      # rerouted by it, so a profile with its own endpoint is left alone. The
      # edge's flag describes the endpoint actually being dialed, so it replaces
      # the cleared value rather than filling a gap.
      # A base_url of api.anthropic.com itself is native (needs_adapter stays 0), so
      # it is rerouted too.
      if (( ! _is_provider && ! needs_adapter )); then
        local _ccm_edge_line
        for _ccm_edge_line in "${(@f)$(_ccm_nested_edge_env \
            CLAUDE_CODE_DISABLE_ADVISOR_TOOL CLAUDE_CODE_MODEL_CAPABILITIES)}"; do
          [[ -n "$_ccm_edge_line" ]] || continue
          penv=("${(@)penv:#${_ccm_edge_line%%=*}=*}" "$_ccm_edge_line")
        done
      fi

      # Derive the context budget from the endpoint unless the conf pinned one.
      # An author who pins a ceiling knows something the endpoint cannot -- a smaller
      # budget on purpose, or an endpoint that over-declares -- so derivation fills a
      # gap rather than overruling. Best-effort throughout: an endpoint that publishes
      # no context window for its models (some gateways publish none) or one that is
      # down must cost nothing, so silence here is success.
      local _ccm_derived_acw=""
      # Both declared out here, not inside the derivation branch below: the
      # withhold call site reads them AFTER that branch closes, and a `local`
      # inside the branch left them unset on any profile that pins
      # CLAUDE_CODE_MAX_CONTEXT_TOKENS in its conf (the branch is skipped). The
      # `${_pceil:-}` at the call site then masked the unset, so withholding
      # silently protected nothing.
      #
      # _pacw is the EFFECTIVE auto-compact budget (env pin, else a saved
      # autoCompactWindow, else the derived value) and BOTH the advisory and the
      # withhold must use it. Passing the derived value to only one of them made
      # them disagree: with a saved 300,000 budget and a 1M launch the advisory
      # correctly cleared a 262,144 model while the withhold removed it anyway --
      # a safe model silently hidden, which is the invariant this feature exists
      # to keep.
      local _pceil="" _pacw=""
      local -a _ccm_advisories=()
      local _ccm_refresh_context=0
      # The one-run window facts (_ccm_launch_window), declared out here for the
      # same reason as _pceil above: the budget resolution after this branch
      # reads the saved field, but only when the run happened -- a profile that
      # pins the ceiling in its conf skips the branch and pays its own read.
      local -a _plw=()
      local _plw_ran=0
      if (( ! ${penv[(I)CLAUDE_CODE_MAX_CONTEXT_TOKENS=*]} )); then
        local _pbase="" _pkey="" _pmodel="" _pwin=""
        # An alias (`opus`, `sonnet`, `haiku`, `fable`) is not a model and has no
        # window of its own -- Claude Code resolves it through the profile's own
        # ANTHROPIC_DEFAULT_<TIER>_MODEL. Collect those so the budget is sized
        # from the model that actually answers, not from the alias.
        local -A _palias=()
        for _pv in "${penv[@]}"; do
          case "$_pv" in
            ANTHROPIC_BASE_URL=*)  _pbase="${_pv#*=}" ;;
            ANTHROPIC_API_KEY=*)   _pkey="${_pv#*=}" ;;
            ANTHROPIC_AUTH_TOKEN=*) [[ -z "$_pkey" ]] && _pkey="${_pv#*=}" ;;
            ANTHROPIC_MODEL=*)     _pmodel="${_pv#*=}" ;;
            ANTHROPIC_DEFAULT_OPUS_MODEL=*)   _palias[opus]="${_pv#*=}" ;;
            ANTHROPIC_DEFAULT_SONNET_MODEL=*) _palias[sonnet]="${_pv#*=}" ;;
            ANTHROPIC_DEFAULT_HAIKU_MODEL=*)  _palias[haiku]="${_pv#*=}" ;;
            ANTHROPIC_DEFAULT_FABLE_MODEL=*)  _palias[fable]="${_pv#*=}" ;;
          esac
        done
        # The model actually being launched drives the budget. Precedence mirrors
        # Claude Code's own: explicit --model, then a profile-injected
        # ANTHROPIC_MODEL, then the conf default (which, when present, was already
        # injected as --model above), and finally the /model choice saved in the
        # profile's settings.json, already in hand as default_model
        # (seed-model-defaults froze it into the conf before this branch) -- that
        # is what Claude Code runs when nothing else names a model, so the budget
        # must be sized from the same place.
        [[ -n "$explicit_model" ]] && _pmodel="$explicit_model"
        [[ -z "$_pmodel" ]] && _pmodel="$default_model"

        # Resolve a tier alias to the model behind it, the way Claude Code does.
        # A bare alias carries no window, so sizing the budget from it is what
        # leaves an alias launch with no autoCompactWindow at all.
        #
        # The mapping can live in either place Claude Code reads it from: the env
        # this launch injects, or the profile's own settings `env` block. Reading
        # only the former misses every profile that pins its tiers in settings,
        # which is the common shape.
        local _pbare="${_pmodel%%\[*}"
        if [[ -z "${_palias[$_pbare]:-}" && -n "$_pbare" ]]; then
          # Canon settings then profile, the projection's own order: a tier mapping
          # declared in ~/.claude/settings.json reaches the engine through the
          # generated settings just as a profile one does, so reading only the profile
          # left the alias unresolved and the session with no budget at all.
          local _amapped
          _amapped=$(_ccm_settings_model_alias "$profile" "$_pbare") || _amapped=""
          [[ -n "$_amapped" ]] && _palias[$_pbare]="$_amapped"
        fi
        if [[ -n "${_palias[$_pbare]:-}" ]]; then
          _ccm_log "alias resolved profile=$profile alias=$_pmodel model=${_palias[$_pbare]}"
          _pmodel="${_palias[$_pbare]}"
        fi

        # Two different questions, and conflating them is what left every
        # claude-* launch with no auto-compact budget at all:
        #
        #   * Which models need CLAUDE_CODE_MAX_CONTEXT_TOKENS?  Only ids Claude
        #     Code has no window of its own for.  It resolves a claude- id from
        #     its own catalogue -- 1M for a [1m] suffix, 200,000 behind a foreign
        #     base URL -- and that beats the declaration: a gateway may declare
        #     200,000 for a claude- id that actually serves far more.  Same prefix
        #     test Claude Code applies to the env var, for the same reason.
        #
        #   * Which models need autoCompactWindow?  EVERY launch.  It is the only
        #     lever that sizes the compaction trigger to the model in force, and a
        #     session without it runs at source:"auto" against the route ceiling --
        #     which, on a route whose largest model is bigger than the launched
        #     one, puts the trigger ABOVE the launched model's wall.  That is the
        #     never-compacts wedge: the session runs to the gateway's hard error
        #     with no compaction, and by then /compact cannot fit either.
        #
        # So the env-ceiling exclusion narrows a copy, never the model itself.
        local _pmodel_env="$_pmodel"
        [[ "$_pmodel_env" == claude-* ]] && _pmodel_env=""

        # Every window fact in one interpreter start. The run happens even with
        # no model: a routed launch still needs its ceiling and its saved
        # budget, and only the model's own record comes back empty.
        _plw=("${(@ps:\t:)$(_ccm_launch_window "$profile" "${_pmodel%%\[*}")}")
        _plw_ran=1

        if [[ -n "$_pmodel" ]]; then
          # _ccm_published_window's answer from the one run: the window alone,
          # empty unless it is a real number; the source field is checked directly.
          _pwin=""
          if [[ "${_plw[1]:-}" == <-> ]] && (( _plw[1] > 0 )); then
            if [[ "${_plw[2]:-}" == catalogue-bundled ]]; then
              (( ! _is_provider )) && _ccm_refresh_context=1
            fi
            if (( ! needs_adapter && ! _is_provider )) && [[ "${_plw[2]:-}" != conf-manual ]]; then
              _pwin=""
            else
              _pwin="${_plw[1]}"
            fi
          fi
        fi

        # The env ceiling covers the whole route, not just the launched model:
        # env is fixed at process start, so a mid-session /model switch to the
        # route's largest model would otherwise run clamped at the launched
        # one's window (and a claude-default launch, where _pwin is empty, would
        # leave every routed model at Claude Code's 200,000).
        # Seeded with the launched model's window only when the env var governs
        # it. A claude- id's window is derived from the id and Claude Code ignores
        # the env var for that family, so feeding it in here would inflate the
        # ceiling every routed model then gets clamped to.
        local _pseed=""
        [[ -n "$_pmodel_env" ]] && _pseed="${_pwin:-}"
        # _ccm_route_ceiling's answer from the one run: the sibling maximum
        # alone (the run never seeds it), then the caller's own seed -- the max
        # the old helper computed, applied here.
        _pceil="${_plw[5]:-}"
        [[ "$_pseed" == <-> ]] && (( _pseed > ${_pceil:-0} )) && _pceil="$_pseed"
        if [[ "$_pceil" == <-> ]] && (( _pceil > 0 )); then
          penv+=("CLAUDE_CODE_MAX_CONTEXT_TOKENS=$_pceil")
          _ccm_log "window ceiling profile=$profile tokens=$_pceil"
        fi

        if [[ "$_pwin" == <-> ]] && (( _pwin > 0 )); then
          # The launched model's own budget rides the NATIVE autoCompactWindow
          # setting (written into the generated settings), never the
          # CLAUDE_CODE_AUTO_COMPACT_WINDOW env var: the binary consults the env
          # var before the settings layer on every resolution, so an env pin
          # silently disables the user's own /autocompact command. The trigger is
          # min(window, auto-compact window), so the route-wide ceiling above is
          # narrowed back to this model's real wall. Clamped to the setting's
          # schema range [100k, 1M] -- an out-of-range value is dropped by Claude
          # Code entirely, which is worse than a slightly early compaction.
          if (( ! ${penv[(I)CLAUDE_CODE_AUTO_COMPACT_WINDOW=*]} )); then
            _ccm_derived_acw=$_pwin

            _ccm_clamp_acw _ccm_derived_acw
          fi
          _ccm_log "window derived profile=$profile model=$_pmodel tokens=$_pwin"
        elif (( needs_adapter || _is_provider )) && [[ -n "$_pmodel" ]]; then
          (( ! _is_provider )) && [[ -n "$_pbase" ]] && _ccm_refresh_context=1
          # The check-engine light: nothing could size this routed model, so
          # the session runs at Claude Code's default budget. Dim, one line,
          # present exactly while true — gone the moment it is probed.
          # Say what this model IS getting, not only that its real size is unknown.
          #
          # The old wording named no number for the launched model and was then
          # followed by the hazard list, which names OTHER models' walls (131k, 128k).
          # A reader saw "no probed window", then three numbers that are not their
          # model's, then a 200k bar — and had no way to tell that 200k is Claude
          # Code's fallback rather than gpt-6-astra's real window. Naming the budget
          # and its provenance in the same breath is what makes the sentence readable.
          # ...and make that sentence TRUE. The route ceiling above is the largest
          # probed window among the models this route serves, so on a mixed route an
          # UNPROBED model inherits a sibling's number: probed 2026-09-08, a model
          # with no entry launched on gateway at CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576,
          # inherited from kimi-k3, with no autoCompactWindow to narrow it back (the
          # branch above is what sets that, and it was skipped). A 5x over-promise, and
          # the failure local per-model context resolution must prevent: the
          # endpoint refuses before compaction ever fires.
          #
          # Narrow compaction, not the process ceiling: lowering the latter also
          # clamps every later /model choice. Ask the resolver for the fallback;
          # retain the route ceiling exactly as for a known launch model.
          local _pdef=""
          [[ -z "${_plw[1]:-}" && "${_plw[3]:-}" == <-> ]] && _pdef="${_plw[3]}"
          # The resolver's `budget` for an unknown model, not Claude Code's
          # 200,000: see UNKNOWN_BUDGET in model-window.
          if [[ "$_pdef" == <-> ]] && (( ! ${penv[(I)CLAUDE_CODE_AUTO_COMPACT_WINDOW=*]} )); then
            _ccm_derived_acw=$_pdef
            _ccm_clamp_acw _ccm_derived_acw
            _ccm_log "window fallback profile=$profile model=$_pmodel tokens=$_pdef (unprobed)"
          fi

          # Nothing for the person to do: sizing retries on its own, so this is
          # the log's business, not an advisory.
          _ccm_log "window unknown profile=$profile model=${_pmodel%%\[*} fallback=${_pdef:-none}"
        fi


      fi

      # OUTSIDE the derivation branch, because the hazard is a property of the
      # session's window and not of who computed it. A profile that pins
      # env.CLAUDE_CODE_MAX_CONTEXT_TOKENS in its conf skips the whole block above
      # -- yet the engine still resolves a window from that pin and still refuses
      # to compact a model whose wall sits under the trigger. Warning only when
      # ccmorph happened to do the deriving left exactly those profiles unguarded.
      if [[ -z "$_pceil" ]]; then
        local _pe2=""
        for _pe2 in "${penv[@]}"; do
          # Engine parse here too: zE() reads this through the same pair, so a
          # `400000junk` pin really does govern at 400000 and must not be dropped.
          [[ "$_pe2" == CLAUDE_CODE_MAX_CONTEXT_TOKENS=* ]] &&
            _pceil=$(_ccm_engine_int "${_pe2#*=}")
        done
      fi
      # ONE resolution of the budget, after the branch, for every launch shape.
      # `_ccm_derived_acw` is empty exactly when the branch was skipped (a conf that
      # pins the ceiling), so this single call covers both cases -- and the engine
      # still resolves a budget in that case, so the hazard path still needs one.
      local _ccm_user_acw
      if (( _plw_ran )); then
        _ccm_user_acw=$(_ccm_effective_acw "$profile" "" "${_plw[6]:-}")
      else
        _ccm_user_acw=$(_ccm_effective_acw "$profile" "")
      fi
      _pacw=${_ccm_user_acw:-$_ccm_derived_acw}

      # The adapter's cliproxyapi config sets rebuild-mid-system-message on the
      # single Anthropic-passthrough entry, which hoists each turn's
      # {"role":"system"} message into the top-level system field. That exists for
      # vLLM-backed upstreams that reject the role, but cliproxyapi resolves an
      # entry by (api-key, base-url) and pools same-type entries round-robin, so
      # there is no way to scope it per model: every model on the route gets it.
      # Hoisting rewrites the prompt prefix every turn, and prompt caching is a
      # prefix match, so the whole conversation is re-written to cache each turn
      # instead of read back (measured 2026-10-11: cache write 4.5x cache read,
      # at the cache-write rate on every re-write).
      #
      # Claude Code can be told no model may carry the turn. It then folds its
      # reminders into user turns, the prefix stays append-only, and the adapter
      # has nothing to hoist. The mask names no model, so it is one rule for every
      # id: Claude Code only grants the turn to ids in its own catalogue (Claude),
      # so other models already behaved this way and nothing changes for them.
      # per_turn_effort is withheld too: with --effort set, Claude Code otherwise
      # sends an empty {"role":"system"} turn carrying the effort, and the
      # gateway's backends 400 on it.
      #
      # ccmorph may fill a gap but never overrule: a value the conf, the launch
      # env, or either settings layer already carries wins. Rebuild only applies
      # behind the adapter, so an anthropic endpoint is left alone.
      #
      # Two more facts hold for every route behind the adapter, so they are
      # defaults here instead of lines each profile has to rediscover:
      # the advisor is an Anthropic server-side tool no gateway tested serves
      # (the request fails before inference, docs/ARCHITECTURE.md section 7), and
      # /effort is hidden for ids Claude Code does not know although the
      # level does reach the provider (section 6).
      #
      # The route defaults and the percentage default are decided HERE and now,
      # after the window derivation, because the unsafe-model warning needs the
      # budget that derivation just resolved -- and ALL of it is answered by one
      # interpreter: the five read-only questions the launch asks of the settings
      # layers cost five interpreter starts in series when asked separately, and
      # those starts are most of a launch's wall time (#332, #321).
      local -a _ccm_route_defaults=() _ccm_route_keys=() _ccm_chosen=() _ccm_invariant=()
      (( needs_adapter )) && _ccm_route_defaults=(
        'CLAUDE_CODE_MODEL_CAPABILITIES=-mid_conv_system,-mid_conv_tool_change,-per_turn_effort'
        'CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1'
        'CLAUDE_CODE_ALWAYS_ENABLE_EFFORT=1'
      )
      # The keys the launch env does not already carry are the only ones the
      # settings layers can still decide, so they are asked together with every
      # other question, reading the layer list once.
      local _ccm_route_default
      for _ccm_route_default in "${_ccm_route_defaults[@]}"; do
        (( ${penv[(I)${_ccm_route_default%%=*}=*]} )) ||
          _ccm_route_keys+=("${_ccm_route_default%%=*}")
      done

      # The fleet policy supplies every profile's default compaction trigger; a
      # conf declaring its own trigger overrules it. A value the USER set in either
      # settings.json also overrules, and checking
      # only the conf was a real override: penv reaches the engine over whatever
      # canon+profile settings hold, so injecting 80
      # here replaced a 70 the user had set. A ccmorph default may fill a gap in
      # Claude Code's file; it must never overrule a choice already in it. Both
      # layers count, because both are the user's.
      local _iw_pct="" _iw_out="" _iw_no1m="" _ccm_pct_in_env=0 _ccm_policy_pct=""
      local _ccm_pct_set=0 _ccm_rec
      # The hazard record, when this launch asked for one: `_ccm_hazard_done`
      # distinguishes "no model is unsafe" (an empty message) from "the
      # diagnostics could not compute it", which is the fallback's business.
      local _ccm_hazard_done=0 _ccm_hazard_msg=""
      # The header key this launch set tag lines for: the diagnostics read below
      # also brings back the user's own lines for it, when the layers hold any.
      local _ccm_hdr_key="" _ccm_user_hdrs=""
      (( ${penv[(I)ANTHROPIC_CUSTOM_HEADERS=*]} )) && _ccm_hdr_key=ANTHROPIC_CUSTOM_HEADERS
      _ccm_trigger_env _iw_pct _iw_out _iw_no1m
      (( ${penv[(I)CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=*]} )) && _ccm_pct_in_env=1
      # An absent policy is the built-in 75, which can be had without a read, so
      # pre-resolving it here is what keeps the interpreter from starting over a
      # file that is not there. A policy that IS there is read by the interpreter
      # itself, and comes back as a `policy` record.
      [[ -e "$_CCMORPH_CONTEXT_POLICY" ]] ||
        _ccm_policy_pct=$(_ccm_context_compaction_percent)
      for _ccm_rec in "${(@0)$(_CCM_DIAG_HAZARD=1 _CCM_DIAG_CEILING="${_pceil:-}" \
          _ccm_launch_diagnostics "$profile" "$_ccm_pct_in_env" \
          "$_iw_pct" "$_iw_out" "$_iw_no1m" "${_pacw:-}" "$_ccm_policy_pct" \
          "$_ccm_hdr_key" \
          "${_ccm_route_keys[@]}" "${_CCMORPH_TUNING_ENV[@]}")}"; do
        case "$_ccm_rec" in
          pct-set)                      _ccm_pct_set=1 ;;
          policy$'\t'*)                 _ccm_policy_pct="${_ccm_rec#*$'\t'}" ;;
          chosen$'\t'*)                 _ccm_chosen+=("${_ccm_rec#*$'\t'}") ;;
          userset$'\t'*)                _ccm_user_hdrs="${_ccm_rec#*$'\t'}" ;;
          hazard$'\t'*)                 _ccm_hazard_done=1; _ccm_hazard_msg="${_ccm_rec#*$'\t'}" ;;
          disguised$'\t'*|unsafe$'\t'*) _ccm_invariant+=("$_ccm_rec") ;;
        esac
      done
      if (( ! _ccm_pct_in_env && ! _ccm_pct_set )); then
        # A policy that does not parse stopped the launch right here before, with
        # today's message and status; the interpreter answered no percentage for it,
        # so this is the same abort one step later.
        [[ -n "$_ccm_policy_pct" ]] ||
          _ccm_policy_pct=$(_ccm_context_compaction_percent) || return 1
        penv+=("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=$_ccm_policy_pct")
      fi
      for _ccm_route_default in "${_ccm_route_defaults[@]}"; do
        (( ${penv[(I)${_ccm_route_default%%=*}=*]} )) && continue
        (( ${_ccm_chosen[(Ie)${_ccm_route_default%%=*}]} )) && continue
        penv+=("$_ccm_route_default")
      done
      # The tag lines replaced the user's own header lines in the process env --
      # the projection strips that key from the generated env block, so the
      # process value is the only carrier. Their lines go in front of the tags;
      # no record means the layers hold nothing, or could not be read, and the
      # tags stand alone.
      if [[ -n "$_ccm_user_hdrs" ]]; then
        local _ccm_hi=1
        while (( _ccm_hi <= ${#penv} )); do
          if [[ "${penv[$_ccm_hi]}" == ANTHROPIC_CUSTOM_HEADERS=* ]]; then
            penv[$_ccm_hi]="ANTHROPIC_CUSTOM_HEADERS=$_ccm_user_hdrs"$'\n'"${penv[$_ccm_hi]#ANTHROPIC_CUSTOM_HEADERS=}"
          fi
          (( ++_ccm_hi ))
        done
      fi
      # The switch hazard. Normally it arrives as the `hazard` record from the
      # diagnostics above: the same row arithmetic and the same wording, from the
      # interpreter that already read the layers and resolved the windows. The
      # two-interpreter path stays for the shapes that record cannot cover (no
      # source file, no context conf, a settings-compose that did not load), so a
      # launch can only ever save starts, never lose a warning.
      #
      # The advisory goes in HERE, not after the report below: `_ccm_advise` is
      # what the SessionStart hook carries, and the launch's advisory ORDER is
      # part of what it writes. Same point in the sequence, same order.
      if (( _ccm_hazard_done )); then
        [[ -z "$_ccm_hazard_msg" ]] || _ccm_advise "$_ccm_hazard_msg"
      else
        _ccm_switch_hazard_warn "$profile" "${_pacw:-}" "${_pceil:-}" || :
      fi

      # The unsafe-model warning belongs HERE, not before the derivation: it computes a
      # trigger too, and CL() mins the budget into every model, so a budget that exists
      # only as a derivation left it using the family ceiling -- trigger 784,000 where the
      # engine will use 224,000. That is the same defect a refuter found in the sibling
      # loop, reached by the other path, and in the over-warn direction: a safe model named
      # fatal, and hidden outright under CCMORPH_WITHHOLD_FATAL=1.
      #
      # Sharing `_pacw` means both mirrors read one resolver rather than two, which is the
      # duplicated-implementation class that produced seven defects in this change.
      # The warnings themselves came back with the layer answers above, from the one
      # interpreter, so this only prints what it already holds.
      _ccm_invariant_report "$profile" "${_ccm_invariant[@]}"

      # Per-launch state rides the process environment, which Claude Code reads
      # for every entry point and which never touches the caller's arguments.
      local -A _ccm_launch_env=()
      [[ -n "$launch_model" ]] && _ccm_launch_env[ANTHROPIC_MODEL]="$launch_model"

      # The launched model's compaction budget. For the profile's default model it
      # is written into the generated settings as the native autoCompactWindow, so
      # the user's own /autocompact keeps working. Any other model (an explicit
      # --model, a picked one, a resumed transcript's) is per-session: parallel
      # sessions share one settings.json, so its budget rides the environment --
      # unless the user already chose a budget, which always wins.
      local -a _ccm_acw_arg=(--acw "${_ccm_derived_acw:-none}")
      if [[ -n "$explicit_model" ]] || (( resume_carried )); then
        _ccm_acw_arg=()
        if [[ -n "$_ccm_derived_acw" && -z "$_ccm_user_acw" ]]; then
          _ccm_launch_env[CLAUDE_CODE_AUTO_COMPACT_WINDOW]="$_ccm_derived_acw"
        fi
      fi

      # The advisory reaches the session through a static SessionStart hook in the
      # generated settings, which prints this variable and nothing otherwise.
      if (( ${#_ccm_advisories} )); then
        _ccm_launch_env[CCMORPH_ADVISE]=$(_CCM_ADVISE="ccmorph: ${(pj:; :)_ccm_advisories}" \
          _ccm_util advise-json)
      fi

      local _pk=""
      for _pk in "${(@k)_ccm_launch_env}"; do
        (( ${_CCMORPH_LAUNCH_ENV[(Ie)$_pk]} )) || { _ccm_err "undeclared launch key $_pk"; return 1; }
        penv=("${(@)penv:#${_pk}=*}")
        penv+=("${_pk}=${_ccm_launch_env[$_pk]}")
      done

      # Seed the profile's saved model defaults and generate the settings from
      # them. A launch does both in one interpreter; a subcommand launch reads
      # the saved defaults with the store's read-only operation instead.
      local _ccm_saved _ccm_project_rc
      if (( ! _ccm_is_subcommand_launch )); then
        _ccm_prepare_and_project "$profile" "${_ccm_acw_arg[@]}" -- "${penv[@]}"
        _ccm_project_rc=$?
        (( _ccm_project_rc == 10 )) && return 1
      else
        _ccm_saved=$(_ccm_config_store model-settings "$profile") || return 1
        _ccm_project_settings "$profile" "$_ccm_saved" "${_ccm_acw_arg[@]}" -- "${penv[@]}"
        _ccm_project_rc=$?
      fi
      # Unwritable homes can use last time's settings: per-launch values still
      # arrive through the environment. An unreadable policy must instead refuse,
      # because those old settings may lack its sandbox.
      if (( _ccm_project_rc )); then
        if (( _ccm_project_rc == 3 )); then
          _ccm_err "'$profile' will not start without its settings policy ($_CCMORPH_PROFILE_DIRS/$profile/settings.policy.json)"
          return 1
        fi
        _ccm_err "warning: '$profile' starts with its previously generated settings"
      fi

      # A model this session's budget cannot compact is a session-killing choice.
      # Opt-in, because the engine leaves no better option and the person is the
      # better judge: through a gateway there is exactly ONE auto-compact window
      # per session (the per-model map the engine owns is gated on a first-party
      # connection and reads null here), so the only way to make a small model
      # safe is a budget small enough for it -- which taxes every large model in
      # the same session. Hiding a model the user picked deliberately costs them a
      # reviewer they wanted; the advisory tells them the exact token count where
      # each risky model would die, and they decide. Set
      # CCMORPH_WITHHOLD_FATAL=1 to have the launcher remove them instead.
      #
      # Runs after the projection, because the generated settings.json holds the
      # availableModels this session will read. It edits that file directly: availableModels is rendered
      # from the profile layer on every projection, so the edit lasts one launch.
      [[ "${CCMORPH_WITHHOLD_FATAL:-0}" == 1 ]] &&
      _ccm_withhold_fatal_switches "$profile" "${_pacw:-}" "${_pceil:-}" \
        "$_CCMORPH_PROFILE_DIRS/$profile/settings.json" || :

      # Only an unknown launch needs this metadata harvest, after all local
      # readers have finished.
      # Provider credentials exist only in this session, not in the refresher.
      (( _ccm_refresh_context )) && _ccm_probe_nudge "$profile"

      # CCMORPH_LIB_DIR is passed to the child so `! ccmorph hop <profile>` typed
      # INSIDE this session resolves the same library that launched it.
      # Only a route behind an adapter has a gateway that was waited on.
      (( needs_adapter || _is_provider )) && [[ -t 2 ]] &&
        print -u2 -- "ccmorph: Gateway ready; starting Claude Code…"
      # The same keys in the same order, taken from the answers the one
      # interpreter already produced: a tuning key the layers really set.
      local -a _ccm_set_layers=("${(@)_CCMORPH_TUNING_ENV:*_ccm_chosen}")
      # History is per profile; a session resumed by id from another one is linked in.
      [[ -n "$resume_session" ]] &&
        _ccm_carry_session "$resume_session" "$_CCMORPH_PROFILE_DIRS/$profile/projects" || :
      _ccm_run_claude \
        "${penv[@]}" \
        "CCMORPH_SET_ENV=${_ccm_set_layers[*]}" \
        "CCMORPH_RELAY_ID=$relay_id" \
        "CCMORPH_LIB_DIR=$_CCMORPH_LIB_DIR" \
        "CLAUDE_CODE_PLUGIN_DIRS=${CLAUDE_CODE_PLUGIN_DIRS:+$CLAUDE_CODE_PLUGIN_DIRS:}${_CCMORPH_LIB_DIR:h}/share/claude-plugin" \
        -- \
        "${args[@]}"
      _ccm_rc=$?
      (( _ccm_forwarded_sig )) && { command rm -f "$relay_file"; _ccm_log "signal ${_ccm_forwarded_sig} forwarded to claude; no relay"; return $_ccm_rc; }

      _ccm_log "exit rc=$_ccm_rc profile=$profile relay_file_exists=$([[ -f $relay_file ]] && echo yes || echo no)"
    else
      # No profile to clear anything: the session's route flags still travel
      # with the proxy, so hand them on as the profile path does.
      local -a _ccm_edge_env=("${(@f)$(_ccm_nested_edge_env \
        CLAUDE_CODE_DISABLE_ADVISOR_TOOL CLAUDE_CODE_MODEL_CAPABILITIES)}")
      _ccm_edge_env=("${(@)_ccm_edge_env:#}")
      [[ -n "$resume_session" ]] &&
        _ccm_carry_session "$resume_session" "$HOME/.claude/projects" || :
      _ccm_run_claude \
        "${_ccm_edge_env[@]}" \
        "CCMORPH_RELAY_ID=$relay_id" \
        "CCMORPH_LIB_DIR=$_CCMORPH_LIB_DIR" \
        "CLAUDE_CODE_PLUGIN_DIRS=${CLAUDE_CODE_PLUGIN_DIRS:+$CLAUDE_CODE_PLUGIN_DIRS:}${_CCMORPH_LIB_DIR:h}/share/claude-plugin" \
        -- \
        "${args[@]}"
      _ccm_rc=$?
      (( _ccm_forwarded_sig )) && { command rm -f "$relay_file"; _ccm_log "signal ${_ccm_forwarded_sig} forwarded to claude; no relay"; return $_ccm_rc; }

      _ccm_log "exit rc=$_ccm_rc profile=<none> relay_file_exists=$([[ -f $relay_file ]] && echo yes || echo no)"
    fi

    if [[ ! -f "$relay_file" || -L "$relay_file" ]]; then
      _ccm_log "normal exit (no relay)"
      command rm -f "$relay_file"
      return $_ccm_rc
    fi

    rs_profile=""
    rs_session=""
    rs_model=""
    rs_effort=""

    local -A relay_seen=()

    while IFS='=' read -r rs_k rs_v || [[ -n "$rs_k" ]]; do
      _ccm_clean_value "$rs_v" || {
        command rm -f "$relay_file"
        _ccm_err "invalid relay signal"
        return 1
      }

      [[ -z "${relay_seen[$rs_k]:-}" ]] || {
        command rm -f "$relay_file"
        _ccm_err "duplicate relay field '$rs_k'"
        return 1
      }
      relay_seen[$rs_k]=1

      case "$rs_k" in
        profile)
          rs_profile="$rs_v"
          ;;
        session)
          rs_session="$rs_v"
          ;;
        model)
          rs_model="$rs_v"
          ;;
        effort)
          rs_effort="$rs_v"
          ;;
        *)
          command rm -f "$relay_file"
          _ccm_err "invalid relay signal"
          return 1
          ;;
      esac
    done < "$relay_file"

    command rm -f "$relay_file" || return 1

    _ccm_valid "$rs_profile" &&
      _ccm_exists "$rs_profile" &&
      [[ -n "$rs_session" ]] || {
        _ccm_err "invalid relay signal"
        return 1
      }

    _ccm_read "$rs_profile" || return 1
    _ccm_validate_loaded "$rs_profile" || {
      unset _ccm
      return 1
    }

    _ccm_setup_dir "$rs_profile" || {
      unset _ccm
      return 1
    }
    unset _ccm

    _ccm_relay_next_args=("${relay_launch_args[@]}")
    [[ -n "$rs_model" ]] && _ccm_relay_next_args+=(--model "$rs_model")
    [[ -n "$rs_effort" ]] && _ccm_relay_next_args+=(--effort "$rs_effort")
    _ccm_relay_next_args+=(--resume "$rs_session")
    _ccm_relay_next_profile="$rs_profile"
    _ccm_relay_next_session="$rs_session"
    if [[ -z "$profile" || ! "$_CCMORPH_DIR/$profile.conf" -ef "$_CCMORPH_DIR/$rs_profile.conf" ]]; then
      _ccm_relay_next_crossed=1
    fi
    return 0
  done
}

# Keep one parent frame across hops: each launch returns through its EXIT cleanup,
# then the driver refreshes the shell before resolving the latest _ccm_launch.
#
# This is also where the opt-in permission bypass is applied, so every launch path
# gets it: it used to live in a shell wrapper function, which meant the setting
# worked only in a shell that had sourced the library.
_ccm_launch_driver() {
  local _CCMORPH_LAUNCH_PROFILE="${_CCMORPH_LAUNCH_PROFILE:-}"
  local _CCMORPH_INTERACTIVE="${_CCMORPH_INTERACTIVE:-0}"
  local launch_args=("$@")
  local rc=0 target="" session=""
  local _ccm_pending_cache_notice=""

  while true; do
    _ccm_relay_next_args=()
    _ccm_relay_next_profile=""
    _ccm_relay_next_session=""
    _ccm_relay_next_crossed=0

    _ccm_launch "${launch_args[@]}"
    rc=$?
    _ccm_pending_cache_notice=""
    (( ${#_ccm_relay_next_args} )) || return $rc

    launch_args=("${_ccm_relay_next_args[@]}")
    target="$_ccm_relay_next_profile"
    session="$_ccm_relay_next_session"
    # Reload only ccmorph and its user-owned opt-in local.sh between hops.
    # Sourcing the whole startup file would execute unrelated shell/tool config.
    source "$_CCMORPH_LIB_DIR/ccmorph.sh" || {
      _ccm_err 'could not reload ccmorph'
      return 1
    }

    _CCMORPH_LAUNCH_PROFILE="$target"
    _CCMORPH_INTERACTIVE=0
    _ccm_log "relay resume profile=$target session=$session"
    print "\n${_ccm_accent}ccmorph:${_ccm_reset} resuming as ${_ccm_bold}${target}${_ccm_reset}...\n"
    if (( _ccm_relay_next_crossed )); then
      _ccm_pending_cache_notice='Switching accounts or backends may rebuild the conversation cache on your first message and charge for the history again.'
    fi
  done
}
