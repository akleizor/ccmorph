# ccmorph-store.zsh — Profile state files: stale-file cleanup, state and profile-home migration, directory security, atomic writes, profile reads and validation.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

_ccm_cleanup_stale_relays() {
  local f relay_id owner modified
  local now

  # Best-effort GC of abandoned relay signals: never fatal to a launch.
  now=$(command date +%s 2>/dev/null) || return 0

  for f in "$_CCMORPH_DIR"/.reswitch.*(N); do
    [[ -f "$f" && ! -L "$f" ]] || continue

    relay_id="${${f:t}#.reswitch.}"
    owner="${relay_id%%.*}"

    [[ "$owner" == <-> ]] || continue
    kill -0 "$owner" 2>/dev/null && continue

    modified=$(_ccm_mtime "$f") || continue
    (( now - modified >= 60 )) || continue

    command rm -f "$f" 2>/dev/null || continue
  done
}

_ccm_cleanup_stale_overlays() {
  # Launches before settings-compose wrote a credential-bearing
  # launch-settings overlay and handed it over with --settings. Reap any a
  # killed session left behind: only when old (12h) AND no live process still
  # names it. Best-effort: never fatal to a launch.
  local now f modified live
  local -i scanned=0
  now=$(command date +%s 2>/dev/null) || return 0
  # One read of $TMPDIR, which can hold tens of thousands of entries, serves
  # both the overlay files and the bridge dirs below.
  local -a candidates bridges
  candidates=( "$_CCMORPH_PROFILE_DIRS"/*/cache/launch-settings.*(N) )
  for f in "${TMPDIR:-/tmp}"/ccmorph-*(N); do
    case "${f:t}" in
      ccmorph-launch-settings.*) candidates+=( "$f" ) ;;
      ccmorph-bridge-$UID-*) bridges+=( "$f" ) ;;
    esac
  done
  for f in "${candidates[@]}"; do
    [[ -f "$f" && ! -L "$f" ]] || continue
    modified=$(_ccm_mtime "$f") || continue
    (( now - modified >= 43200 )) || continue
    if (( ! scanned )); then
      live=$(command ps -axo command 2>/dev/null) || live=""
      scanned=1
    fi
    [[ -n "$live" && "$live" == *"$f"* ]] && continue
    command rm -f "$f" 2>/dev/null || :
  done

  # Provider bridge config dirs (ccmorph-bridge-$UID-*/config.yaml) carry the
  # proxy's inbound key. _ccm_provider_stop reaps them on a clean teardown, but a
  # SIGKILL/OOM of the launcher leaves both the dir and (worse) an orphaned
  # cliproxyapi still serving OAuth creds. Same rule as above: reap only when old
  # (12h) AND no live process names the config, so a running bridge is spared.
  local d
  for d in "${bridges[@]}"; do
    [[ -d "$d" && ! -L "$d" ]] || continue
    modified=$(_ccm_mtime "$d") || continue
    (( now - modified >= 43200 )) || continue
    if (( ! scanned )); then
      live=$(command ps -axo command 2>/dev/null) || live=""
      scanned=1
    fi
    [[ -n "$live" && "$live" == *"$d/config.yaml"* ]] && continue
    command rm -rf "$d" 2>/dev/null || :
  done
}

# Move state a release renamed, so upgrading is never a chore the user performs.
# Shipping a rename and asking everyone to run `mv` is the software failing to do
# its own job: an upgrade that needs a manual step is an upgrade most people never
# complete, and the ones who miss it get silently degraded behaviour.
#
# Idempotent and conservative: it acts only when the old name exists and the new
# one does not, so a host that already migrated, or that authored a fresh file, is
# untouched. A failed move is not fatal — the caller falls back to the default
# window, exactly as it would for a machine that never probed.
_ccm_migrate_state() {
  [[ -n "${CCMORPH_CONTEXT_LOCKED:-}" ]] && return 0
  local -a legacy=("$_CCMORPH_DIR/model-registry" "$_CCMORPH_DIR/model-registry.conf"
    "$_CCMORPH_PROFILE_DIRS"/*/cache/context-window(N) "$_CCMORPH_DIR"/*.conf.lock(N))
  # A conf still carrying its discovered limits inline moves them to models.json.
  command grep -qs '^context_discovered=' "$_CCMORPH_DIR"/*.conf(N) /dev/null &&
    legacy+=("$_CCMORPH_DIR")
  # Plain credentials move into the login keychain (macOS); elsewhere they stay 0600.
  [[ "$OSTYPE" == darwin* ]] &&
    command grep -hsE '^(api_key|token)=.' "$_CCMORPH_DIR"/*.conf(N) /dev/null | command grep -qv '=keychain:' &&
    command security default-keychain >/dev/null 2>&1 &&
    "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" util keychain-migrate "$_CCMORPH_DIR"
  local legacy_file
  for legacy_file in "${legacy[@]}"; do
    if [[ -e "$legacy_file" || -L "$legacy_file" ]]; then
      command env CCMORPH_STATE_DIR="$_CCMORPH_DIR" \
        "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" model-window --migrate || {
        _ccm_err "context migration incomplete; original data kept for retry"
        return 1
      }
      return 0
    fi
  done
}

_ccm_migrate_profile_home() {
  # Fresh installs and completed migrations need no interpreter or lock.
  if [[ ! -e "$HOME/.claude-profiles" && ! -L "$HOME/.claude-profiles" ]]; then
    return 0
  fi
  if [[ -L "$HOME/.claude-profiles" && -d "$_CCMORPH_PROFILE_DIRS" &&
        ! -L "$_CCMORPH_PROFILE_DIRS" &&
        "${${:-$HOME/.claude-profiles}:A}" == "${_CCMORPH_PROFILE_DIRS:A}" ]]; then
    return 0
  fi
  _ccm_util migrate-profile-home "$HOME/.claude-profiles" "$_CCMORPH_PROFILE_DIRS"
}

_ccm_normalize_plugin_index_paths() {
  # Claude records the absolute profile spelling used during installation. Profiles
  # now share ~/.claude/plugins, so repair only paths made stale by that topology.
  # Keep the settled launch path fork-free; malformed indexes remain best-effort.
  local index content needs_repair=0
  for index in "$HOME/.claude/plugins/known_marketplaces.json" \
               "$HOME/.claude/plugins/installed_plugins.json"; do
    [[ -f "$index" ]] || continue
    content=$(<"$index") || continue
    if [[ "$content" == *"$HOME/.claude-profiles/"* ||
          "$content" == *"$_CCMORPH_PROFILE_DIRS/"* ]]; then
      needs_repair=1
      break
    fi
  done
  (( needs_repair )) || return 0

  _ccm_util normalize-plugin-index "$HOME" "$_CCMORPH_PROFILE_DIRS" 2>/dev/null || return 0
}

_ccm_secure_dirs() {
  local f root

  for root in "$_CCMORPH_DIR" "$_CCMORPH_PROFILE_DIRS"; do
    if [[ -L "$root" ]]; then
      _ccm_err "refusing symlinked state root '$root'"
      return 1
    fi
  done

  _ccm_migrate_profile_home || return 1
  command mkdir -p "$_CCMORPH_DIR" "$_CCMORPH_PROFILE_DIRS" || return 1
  _ccm_normalize_plugin_index_paths
  # Permission hardening is best-effort: never fail on a filesystem that
  # refuses chmod. The symlink guards above are the real safety invariant.
  command chmod 700 "$_CCMORPH_DIR" "$_CCMORPH_PROFILE_DIRS" 2>/dev/null || :

  if [[ -e "$_CCMORPH_DIR/active" || -L "$_CCMORPH_DIR/active" ]]; then
    [[ ! -L "$_CCMORPH_DIR/active" ]] || return 1
    command chmod 600 "$_CCMORPH_DIR/active" 2>/dev/null || :
  fi

  for f in "$_CCMORPH_DIR"/*.conf(N); do
    [[ ! -L "$f" ]] || return 1
    command chmod 600 "$f" 2>/dev/null || :
  done

  # Last: the state root exists and is sane, so renames within it are safe.
  _ccm_migrate_state
}

_ccm_atomic_write_unlocked() {
  local f="$1"
  local value="$2"
  local parent="${1:h}"
  local tmp

  [[ -d "$parent" ]] || command mkdir -p "$parent" || return 1
  command chmod 700 "$parent" || return 1

  tmp=$(command mktemp "$parent/.ccmorph.tmp.XXXXXX") || return 1
  command chmod 600 "$tmp" || {
    command rm -f "$tmp"
    return 1
  }

  if ! print -r -- "$value" >| "$tmp"; then
    command rm -f "$tmp"
    return 1
  fi

  command chmod 600 "$tmp" || {
    command rm -f "$tmp"
    return 1
  }

  command mv -f "$tmp" "$f" || {
    command rm -f "$tmp"
    return 1
  }
}

# Config writes are crash-atomic (mktemp + rename); no lock needed.
_ccm_write() {
  _ccm_atomic_write_unlocked "$1" "$2"
}

_ccm_remove_file() {
  [[ -e "$1" || -L "$1" ]] || return 0
  command rm -f "$1"
}

_ccm_read_active() {
  local f="$_CCMORPH_DIR/active"

  REPLY=""
  [[ -e "$f" || -L "$f" ]] || return 1
  [[ -f "$f" && ! -L "$f" ]] || {
    _ccm_err "invalid active-profile file"
    return 1
  }

  command chmod 600 "$f" || return 1
  REPLY=$(<"$f") || return 1

  _ccm_valid "$REPLY" || {
    _ccm_err "invalid active profile '$REPLY'"
    REPLY=""
    return 1
  }
}

_ccm_read() {
  typeset -gA _ccm=()

  local name="$1" readonly="${2:-0}" out
  # Parsing and validation live in ccmorph-core (package profile); it prints the
  # fields NUL-separated, or the refusal with exit 1.
  out=$(_ccm_util profile-read "$_CCMORPH_DIR" "$name" $(( ! readonly )) \
    "${(j:,:)_CCMORPH_PROFILE_ENV}" "${(j:,:)_CCMORPH_MODEL_MAPPING_ENV}") || {
    _ccm_err "${out:-could not read profile '$name'}"
    unset _ccm
    return 1
  }
  out="${out%.}"
  [[ -z "$out" ]] || _ccm=("${(@0)${out%$'\0'}}")
}

# Validation of the loaded profile (_ccm) lives in ccmorph-core (package profile).
# The fields go over stdin, since `config` edits them in memory before saving.
_ccm_validate_loaded() {
  [[ "${_ccm[type]:-}" == apikey ]] || return 0
  local out k
  out=$(for k in type api_key base_url adapter; do print -rn -- "$k"$'\0'"${_ccm[$k]:-}"$'\0'; done |
    _ccm_util profile-validate "$1") || { _ccm_err "$out"; return 1; }
}

_ccm_validate_base_url() {
  local out
  out=$(_ccm_util profile-validate-url "$1" "${_ccm[base_url]}") || { _ccm_err "$out"; return 1; }
}
