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

  local name="$1" readonly="${2:-0}"
  local file="$_CCMORPH_DIR/$1.conf"
  local line k v
  local -A seen=()

  _ccm_valid "$name" || {
    _ccm_err "invalid profile name"
    return 1
  }

  [[ -f "$file" && ! -L "$file" ]] || {
    _ccm_err "invalid profile file for '$name'"
    return 1
  }

  if (( ! readonly )); then
    command chmod 600 "$file" || {
      _ccm_err "cannot secure profile '$name'"
      return 1
    }
  fi

  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue

    if [[ "$line" == *$'\r'* || "$line" != *=* ]]; then
      _ccm_err "malformed config for '$name'"
      unset _ccm
      return 1
    fi

    k="${line%%=*}"
    v="${line#*=}"

    if [[ -z "$k" || "$k" == *[[:space:]]* ]]; then
      _ccm_err "invalid key in '$name'"
      unset _ccm
      return 1
    fi

    if [[ -n "${seen[$k]:-}" ]]; then
      _ccm_err "duplicate key '$k' in '$name'"
      unset _ccm
      return 1
    fi

    case "$k" in
      type|email|token|base_url|api_key|model|adapter|tag|provider|context_discovered|window_sources)
        ;;
      # Removed key. A conf written before the transport seam was dropped must
      # still launch, so say what replaced it once and carry on without it.
      transport)
        print -u2 "ccmorph: transport= was removed; export HTTPS_PROXY from your bridge instead"
        continue
        ;;
      forward_ip)
        print -u2 "ccmorph: forward_ip= was removed; the adapter no longer sends X-Forwarded-For"
        continue
        ;;
      subagent_model)
        print -u2 "ccmorph: subagent_model= was removed; it was never read"
        continue
        ;;
      lean)
        print -u2 "ccmorph: lean= was removed; launch behavior no longer varies by it"
        continue
        ;;
      role)
        print -u2 "ccmorph: role= was removed; pick the model with --model instead"
        continue
        ;;
      excluded_models)
        [[ -n "$v" && "$v" != *[[:space:]]* && "$v" != ,* && "$v" != *, && "$v" != *,,* ]] || {
          _ccm_err "excluded_models must be comma-separated exact model IDs"; unset _ccm; return 1
        }
        ;;
      model_map)
        _ccm_util valid-model-map "$v" 2>/dev/null || {
          _ccm_err "model_map must be a JSON object of unique model IDs to model IDs, and no key may carry the [1m] suffix"
          unset _ccm; return 1
        }
        ;;
      # Stark's spend cap lives in the shared conf; ccmorph only keeps it so a rewrite does not drop it.
      monthly_budget_usd)
        [[ "$v" =~ '^([0-9]+\.?[0-9]*|\.[0-9]+)$' && "$v" == *[1-9]* ]] || {
          _ccm_err "monthly_budget_usd must be a positive number"; unset _ccm; return 1
        }
        ;;
      model_defaults)
        [[ "$v" == 1 ]] || { _ccm_err "invalid model defaults marker"; unset _ccm; return 1; }
        ;;
      private)
        [[ "$v" == 1 ]] || { _ccm_err "private must be 1"; unset _ccm; return 1; }
        ;;
      effort|effort.*)
        [[ "$v" == (low|medium|high|xhigh|max) && "$k" != effort. ]] || {
          _ccm_err "invalid effort setting '$k'"; unset _ccm; return 1
        }
        ;;
      env.*)
        if (( ! ${_CCMORPH_PROFILE_ENV[(Ie)${k#env.}]} )); then
          _ccm_err "disallowed environment key '$k' in '$name'"
          unset _ccm
          return 1
        fi
        ;;
      context_window.*)
        local _ctx_model="${k#context_window.}"
        [[ -n "$_ctx_model" && "$_ctx_model" != *[=$'\r\n']* && "$v" == <-> && "$v" -gt 0 ]] || {
          _ccm_err "invalid context pin '$k' in '$name'"
          unset _ccm
          return 1
        }
        ;;
      *)
        _ccm_err "unknown key '$k' in '$name'"
        unset _ccm
        return 1
        ;;
    esac

    seen[$k]=1
    _ccm[$k]="$v"
  done < "$file"

  # A credential kept in the login keychain is read here, so every caller sees the value.
  for k in api_key token; do
    [[ "${_ccm[$k]:-}" == keychain:* ]] || continue
    _ccm[$k]=$("$_CCMORPH_LIBEXEC_DIR/ccmorph-core" util keychain-get "${_ccm[$k]}") || {
      _ccm_err "'$name': ${_ccm[$k]} is not in the login keychain; set it again with: ccmorph config $name"
      unset _ccm
      return 1
    }
  done

  # `proxy` was the old spelling for a key-authenticated routed endpoint. Fold
  # it at the boundary so every runtime path has one profile type to maintain.
  if [[ "${_ccm[type]:-}" == proxy ]]; then
    [[ -n "${_ccm[base_url]:-}" ]] || {
      _ccm_err "proxy '$name' has no base_url"
      unset _ccm
      return 1
    }
    _ccm[type]=apikey
  fi

  local _ccm_type_keys=""
  case "${_ccm[type]:-}" in
    sub)
      # The email IS the profile identity for this type, so a blank one is not a
      # cosmetic gap — `list` shows a nameless row and nothing ever re-flags it.
      # The token case has always required its own credential; this is the same
      # rule, applied to the type that was missing it.
      [[ -n "${_ccm[email]:-}" ]] || {
        _ccm_err "sub profile '$name' has no email — re-create it: ccmorph rm $name && ccmorph add $name"
        unset _ccm
        return 1
      }

      _ccm_type_keys="email"
      ;;

    token)
      [[ -n "${_ccm[token]:-}" ]] || {
        _ccm_err "token profile '$name' has no token"
        unset _ccm
        return 1
      }
      _ccm_type_keys="token"
      ;;

    apikey)
      _ccm_type_keys="api_key|base_url|excluded_models|adapter|tag|env.*|context_window.*|window_sources|model_map"
      ;;

    provider)
      [[ -n "${_ccm[provider]:-}" ]] || {
        _ccm_err "provider profile '$name' has no provider"
        unset _ccm
        return 1
      }
      _ccm_type_keys="provider|excluded_models|tag|env.*|context_window.*|window_sources"
      ;;

    *)
      _ccm_err "invalid or missing type in '$name'"
      unset _ccm
      return 1
      ;;
  esac

  # Keys every type accepts, plus the type's own. A sub or token profile carries
  # only model-mapping env keys; routed types accept any allowlisted env key.
  local _ccm_allowed="type|model|model_defaults|private|monthly_budget_usd|effort|effort.*|context_discovered|$_ccm_type_keys"
  for k in "${(@k)_ccm}"; do
    [[ "$k" == (${~_ccm_allowed}) ]] && continue
    [[ "$k" == env.* ]] && (( ${_CCMORPH_MODEL_MAPPING_ENV[(Ie)${k#env.}]} )) && continue
    _ccm_err "key '$k' is invalid for ${_ccm[type]} profile '$name'"
    unset _ccm
    return 1
  done
}

_ccm_validate_base_url() {
  local label="$1"

  [[ "${_ccm[base_url]}" == http://* ||
     "${_ccm[base_url]}" == https://* ]] || {
    _ccm_err "$label base_url must use http:// or https://"
    return 1
  }

  [[ "${_ccm[base_url]}" != *[[:space:]]* ]] || {
    _ccm_err "$label base_url contains whitespace"
    return 1
  }
}

# An apikey profile authenticates with ANTHROPIC_API_KEY instead of an OAuth
# subscription. base_url and model are optional on purpose: omit both for a
# native Anthropic API key (Claude Code owns the model list), set both for a
# gateway that speaks the Anthropic API under its own model names.
_ccm_validate_apikey() {
  local name="$1"

  [[ -n "${_ccm[api_key]:-}" ]] || {
    _ccm_err "apikey '$name' has no api_key"
    return 1
  }

  [[ "${_ccm[api_key]}" != *[[:space:]]* ]] || {
    _ccm_err "apikey '$name' api_key contains whitespace"
    return 1
  }

  if [[ -n "${_ccm[base_url]:-}" ]]; then
    _ccm_validate_base_url "apikey '$name'" || return 1
  fi

  # adapter=true means "this endpoint speaks dialects Claude Code does not, put a
  # translator in front of it at launch". It is a declaration of intent; the
  # mechanism belongs to the framework, so no adapter is ever named here. The
  # profile keeps its real base_url and only the launched environment is
  # redirected, which keeps `ccmorph _env` truthful for every other consumer.
  if [[ -n "${_ccm[adapter]:-}" ]]; then
    [[ "${_ccm[adapter]}" == (true|false) ]] || {
      _ccm_err "apikey '$name' adapter must be 'true', 'false', or absent"
      return 1
    }

    if [[ "${_ccm[adapter]}" == true ]]; then
      # A native Anthropic key has no foreign dialect to translate.
      [[ -n "${_ccm[base_url]:-}" ]] || {
        _ccm_err "apikey '$name' has adapter=true but no base_url to front"
        return 1
      }
    fi
  fi
}

# Dispatch validation for the currently loaded profile (_ccm). sub and token
# profiles carry nothing ccmorph has to check beyond _ccm_read.
_ccm_validate_loaded() {
  [[ "${_ccm[type]:-}" != apikey ]] || _ccm_validate_apikey "$1"
}
