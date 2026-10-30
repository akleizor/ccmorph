# ccmorph-providers.zsh — The dialect adapter and the provider proxy (cliproxyapi): start, stop, login and listing.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

# ── Launch helper -------------------------------------------------------------

# ── Dialect adapter ──────────────────────────────────────────────────────────
#
# A profile with adapter=true has an endpoint speaking dialects Claude Code does
# not. ccmorph-adapter fronts it on a loopback ephemeral port for the life of the
# session. One instance per profile, never shared: a shared translator has an
# instance-wide inbound key list, so any profile pointing at it could spend another
# profile's credential.
#
# The adapter is handed only the profile name -- it reads the endpoint and
# credential from the profile itself, so no key passes through argv.

_ccm_adapter_stop() {
  if [[ -n "${_ccm_adapter_pid:-}" ]]; then
    local pid="$_ccm_adapter_pid"
    kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null

    # A disowned process is no longer waitable by zsh: `wait` returns 127 while
    # the adapter is still handling TERM. Do not return with its translator and
    # rendered credential alive. The adapter allows its child 10 seconds to stop,
    # so give the supervisor 12 before forcing termination.
    local waited=0
    while kill -0 "$pid" 2>/dev/null && (( waited < 240 )); do
      command sleep 0.05
      (( ++waited ))
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null
      waited=0
      while kill -0 "$pid" 2>/dev/null && (( waited < 40 )); do
        command sleep 0.05
        (( ++waited ))
      done
    fi
  fi
  unset _ccm_adapter_pid _ccm_adapter_profile _ccm_adapter_port
}

# ── Provider proxy (cliproxyapi) ─────────────────────────────────────────────

_ccm_provider_bin() {
  local bin
  bin=$(whence -p cliproxyapi 2>/dev/null) || bin=""
  [[ -n "$bin" && -x "$bin" ]] || {
    _ccm_err "cliproxyapi not found — install it: brew install cliproxyapi"
    return 1
  }
  REPLY="$bin"
}

_ccm_provider_list() {
  local bin
  _ccm_provider_bin || return 1
  bin="$REPLY"
  REPLY=()
  local line
  while IFS= read -r line; do
    [[ "$line" == *-login* ]] || continue
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line#-}"
    line="${line%%-login*}"
    [[ "$line" == *device* ]] && continue
    [[ -n "$line" ]] && REPLY+=("$line")
  done < <("$bin" --help 2>&1)
}

# Resolve a profile's provider auth dir into REPLY, refusing a symlinked auth/
# root or per-profile dir. Following a symlink here would read, write, or rm
# OAuth credentials outside the 0700 state root — the same hazard every other
# state path guards against — so every caller (login, start, remove) funnels
# through this one check rather than each guarding its own copy.
_ccm_provider_auth_dir() {
  local root="$_CCMORPH_DIR/auth"
  [[ ! -L "$root" ]] || {
    _ccm_err "auth directory root is a symlink, refusing: $root"
    return 1
  }
  local dir="$root/$1"
  [[ ! -L "$dir" ]] || {
    _ccm_err "auth directory for '$1' is a symlink, refusing: $dir"
    return 1
  }
  REPLY="$dir"
}

# Ensure the auth/ root exists and is 0700 (the per-profile dir is made 0700 by
# its own caller). Kept separate from resolution so read-only callers pay nothing.
_ccm_provider_auth_root_ok() {
  local root="$_CCMORPH_DIR/auth"
  [[ ! -L "$root" ]] || {
    _ccm_err "auth directory root is a symlink, refusing: $root"
    return 1
  }
  [[ -d "$root" ]] || command mkdir -p "$root" || return 1
  command chmod 0700 "$root" 2>/dev/null || :
}

_ccm_provider_login() {
  local profile="$1" provider="$2"
  local bin auth_dir tmpdir conf yaml
  _ccm_provider_bin || return 1
  bin="$REPLY"
  _ccm_provider_auth_dir "$profile" || return 1
  auth_dir="$REPLY"
  _ccm_provider_auth_root_ok || return 1
  command mkdir -p "$auth_dir" && command chmod 0700 "$auth_dir" || {
    _ccm_err "could not create auth directory"
    return 1
  }
  print "${_ccm_dim}Opening your browser to sign in to ${_ccm_bold}${provider}${_ccm_reset}${_ccm_dim} — Ctrl-C to cancel.${_ccm_reset}"
  tmpdir=$(command mktemp -d "${TMPDIR:-/tmp}/ccmorph-login-$UID-XXXXXX") || return 1
  local _cred_file=""
  {
    conf="$tmpdir/config.yaml"
    yaml="host: \"127.0.0.1\"
port: 0
auth-dir: \"$auth_dir\"
api-keys:
  - \"unused\""
    _ccm_write "$conf" "$yaml" || return 1
    "$bin" -"$provider"-login -config "$conf" || {
      _ccm_err "$provider login failed"
      return 1
    }
    _cred_file=$(command find -H "$auth_dir" -maxdepth 2 -name '*.json' -type f 2>/dev/null | command head -1)
    [[ -n "$_cred_file" ]] || {
      _ccm_err "no credential appeared after login — check your browser"
      return 1
    }
  } always {
    command rm -rf "$tmpdir"
    # A cancelled/failed login must not leave an empty auth/<name> behind: it is
    # invisible to _ccm_exists (which tests the conf) and a later `add` of the
    # same name would rm -rf or silently adopt it. rmdir removes it ONLY if
    # empty, so a real credential is never touched.
    [[ -n "$_cred_file" ]] || command rmdir "$auth_dir" 2>/dev/null || :
  }
  command chmod 0600 "$auth_dir"/**/*.json 2>/dev/null || :
}

_ccm_provider_start() {
  # cliproxyapi on a loopback port with a fresh inbound key, handed back only once
  # /v1/models answers 200 (a bound port is not yet a ready router).
  local profile="$1" bin auth_dir out line
  _ccm_provider_bin || return 1
  bin="$REPLY"
  _ccm_provider_auth_dir "$profile" || return 1
  auth_dir="$REPLY"
  [[ -d "$auth_dir" ]] || {
    _ccm_err "no auth directory for '$profile' — run: ccmorph login $profile"
    return 1
  }
  _ccm_log_init
  out=$(_ccm_util bridge-start "$profile" "$bin" "$auth_dir" "${_CCMORPH_LOG:-/dev/null}" </dev/null) || return 1
  for line in "${(@f)out}"; do
    case "$line" in
      PID=*) _ccm_provider_pid="${line#PID=}" ;;
      PORT=*) _ccm_provider_port="${line#PORT=}" ;;
      KEY=*) _ccm_provider_key="${line#KEY=}" ;;
      TMPDIR=*) _ccm_provider_tmpdir="${line#TMPDIR=}" ;;
    esac
  done
  _ccm_provider_auth="$auth_dir"
  _ccm_provider_active="$profile"
  REPLY="$_ccm_provider_port"
}

_ccm_provider_stop() {
  # Also re-tightens the OAuth files cliproxyapi rewrote mid-session to 0600.
  [[ -n "${_ccm_provider_pid:-}${_ccm_provider_tmpdir:-}${_ccm_provider_auth:-}" ]] &&
    _ccm_util bridge-stop "${_ccm_provider_pid:-0}" "${_ccm_provider_tmpdir:-}" "${_ccm_provider_auth:-}"
  unset _ccm_provider_pid _ccm_provider_active _ccm_provider_port _ccm_provider_key _ccm_provider_tmpdir _ccm_provider_auth
  return 0
}

# Start the adapter for a profile and set REPLY to the port it serves. Waits for
# the adapter's own readiness line so Claude Code is never pointed at a dead port.
_ccm_adapter_start() {
  local profile="$1" selected="${2:-}"
  local -a scope=(--auto)
  [[ -n "$selected" ]] && scope+=(--model "${selected%%\[*}")
  local bin="$_CCMORPH_LIBEXEC_DIR/ccmorph-core"

  [[ -x "$bin" ]] || {
    _ccm_err "profile '$profile' needs an adapter but $bin is not executable"
    return 1
  }

  local ready adapter_log="$_CCMORPH_LOG"
  _ccm_log_init
  ready=$(command mktemp "${TMPDIR:-/tmp}/ccmorph-adapter-ready.XXXXXX") || return 1

  # Redirections are prepared by this shell before the adapter starts. If an
  # optional log is unwritable, redirecting to it would abort the child before
  # its executable runs, so fall back to /dev/null at that boundary.
  if ! { : >> "$adapter_log" } 2>/dev/null; then
    adapter_log=/dev/null
  fi

  # Two independent notices to suppress for this internal adapter job:
  #   • the START line "[7] 68382" — printed synchronously by `&` when `monitor`
  #     is on, so `monitor` must be OFF at that instant. localoptions keeps it
  #     off across the `&` and restores after.
  #   • the DONE line "[7] + done …" — printed at the NEXT prompt, long after
  #     this function (and localoptions) returns, so an option can't reach it;
  #     `disown` removes the job from the table entirely so there's nothing to
  #     report.
  # $! stays valid for kill; readiness is tracked via the $ready file, not
  # `wait`, so disowning costs nothing.
  setopt localoptions no_monitor
  # The adapter always runs in the launcher's tree, so it inherits the launcher's
  # environment — including any ambient HTTPS_PROXY that makes a private endpoint
  # reachable. Nothing here overrides it.
  # </dev/null is load-bearing, not cosmetic: fd0 must never be the tty (the
  # 610137d wedge — the adapter holding fd0 froze the TUI). no_monitor buys the
  # redirect today as an emergent property of job control; make it explicit so
  # a future setopt change cannot silently reintroduce the wedge.
  "$bin" adapter "$profile" "${scope[@]}" --parent-pid $$ </dev/null >"$ready" 2>>"$adapter_log" &
  _ccm_adapter_pid=$!
  # `disown` takes a JOB spec, not a PID: a bare number is read as a job
  # number and fails (rc=127), leaving the job in the table so zsh prints
  # the "[N] + done" notice at the next prompt. `%+` (the most-recent job,
  # i.e. the `&` just backgrounded) is the correct spec; keep the PID form
  # only as a fallback for the rare shell where `%+` is unavailable.
  disown %+ 2>/dev/null || disown "$_ccm_adapter_pid" 2>/dev/null

  # A selected launch has a bounded compatibility check, never a fleet sweep.
  # Keep the legacy diagnostic budget only when no model was supplied.
  local waited=0 wait_limit=3000
  [[ -n "$selected" ]] && wait_limit=225
  local event="" line=""
  # Which of the three ways this can fail, so the message can say. The loop already
  # distinguished them to decide when to stop waiting and then threw the distinction
  # away, leaving one sentence ("did not become ready") for an adapter that refused
  # to start, one that crashed, and one that is still grinding.
  local outcome="timeout"
  while (( waited < wait_limit )); do
    if [[ -r "$ready" ]]; then
      IFS= read -r event < "$ready" || event=""
    else
      event=""
    fi
    if [[ "$event" == NATIVE=1 ]]; then
      command rm -f "$ready"
      _ccm_adapter_stop
      _ccm_adapter_profile="$profile"
      _ccm_adapter_port=""
      REPLY=""
      return 0
    elif [[ "$event" == PORT=* ]]; then
      line="$event"
      break
    elif [[ "$event" == EXIT=* ]]; then
      outcome="declined"
      break
    elif [[ -n "${_ccm_adapter_pid:-}" ]] &&
         ! kill -0 "$_ccm_adapter_pid" 2>/dev/null; then
      outcome="died"
      break
    fi
    if (( waited == 5 )); then
      print -u2 -- "ccmorph: Starting adapter for '$profile'${selected:+ ($selected)}…"
    elif (( waited > 5 && waited % 25 == 0 )); then
      print -u2 -- "ccmorph: Waiting for '$profile' adapter ($(( waited / 5 ))s); Ctrl-C to cancel."
    fi
    command sleep 0.2
    (( ++waited ))
  done
  command rm -f "$ready"

  [[ -n "$line" ]] || {
    _ccm_adapter_stop
    # Say what the adapter IS before saying it failed. "did not become ready" is a
    # sentence about an internal handshake — a reader who has never opened this file
    # cannot tell whether something crashed, timed out, or was never installed, and
    # nothing in it suggests an action. Name the job, the outcome, and the next step.
    case "$outcome" in
      declined)
        _ccm_err "${event#EXIT=}" ;;
      died)
        _ccm_err "'$profile': adapter exited before readiness; session not started." ;;
      *)
        _ccm_err "'$profile': adapter timed out after $(( wait_limit / 5 ))s; session not started." ;;
    esac
    print -u2 -- "  Check configuration: ccmorph inspect $profile"
    if [[ "$adapter_log" != /dev/null && -r "$adapter_log" ]]; then
      print -u2 -- "  Log: $adapter_log"
    fi
    return 1
  }

  _ccm_adapter_profile="$profile"
  _ccm_adapter_port="${line#PORT=}"
  REPLY="$_ccm_adapter_port"
}
