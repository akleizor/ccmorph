# ccproteus-store.zsh — Profile state files: stale-file cleanup, state and profile-home migration, directory security, atomic writes, profile reads and validation.
# Sourced by ccproteus.sh after ccproteus-ui.zsh (see its Modules block); never run directly.

_ccs_cleanup_stale_relays() {
  local f relay_id owner modified
  local now

  # Best-effort GC of abandoned relay signals: never fatal to a launch.
  now=$(command date +%s 2>/dev/null) || return 0

  for f in "$_CCPROTEUS_DIR"/.reswitch.*(N); do
    [[ -f "$f" && ! -L "$f" ]] || continue

    relay_id="${${f:t}#.reswitch.}"
    owner="${relay_id%%.*}"

    [[ "$owner" == <-> ]] || continue
    kill -0 "$owner" 2>/dev/null && continue

    modified=$(_ccs_mtime "$f") || continue
    (( now - modified >= 60 )) || continue

    command rm -f "$f" 2>/dev/null || continue
  done
}

_ccs_cleanup_stale_overlays() {
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
  candidates=( "$_CCPROTEUS_PROFILE_DIRS"/*/cache/launch-settings.*(N) )
  for f in "${TMPDIR:-/tmp}"/ccproteus-*(N); do
    case "${f:t}" in
      ccproteus-launch-settings.*) candidates+=( "$f" ) ;;
      ccproteus-bridge-$UID-*) bridges+=( "$f" ) ;;
    esac
  done
  for f in "${candidates[@]}"; do
    [[ -f "$f" && ! -L "$f" ]] || continue
    modified=$(_ccs_mtime "$f") || continue
    (( now - modified >= 43200 )) || continue
    if (( ! scanned )); then
      live=$(command ps -axo command 2>/dev/null) || live=""
      scanned=1
    fi
    [[ -n "$live" && "$live" == *"$f"* ]] && continue
    command rm -f "$f" 2>/dev/null || :
  done

  # Provider bridge config dirs (ccproteus-bridge-$UID-*/config.yaml) carry the
  # proxy's inbound key. _ccs_provider_stop reaps them on a clean teardown, but a
  # SIGKILL/OOM of the launcher leaves both the dir and (worse) an orphaned
  # cliproxyapi still serving OAuth creds. Same rule as above: reap only when old
  # (12h) AND no live process names the config, so a running bridge is spared.
  local d
  for d in "${bridges[@]}"; do
    [[ -d "$d" && ! -L "$d" ]] || continue
    modified=$(_ccs_mtime "$d") || continue
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
_ccs_migrate_state() {
  [[ -n "${CCPROTEUS_CONTEXT_LOCKED:-}" ]] && return 0
  local -a legacy=("$_CCPROTEUS_DIR/model-registry" "$_CCPROTEUS_DIR/model-registry.conf"
    "$_CCPROTEUS_PROFILE_DIRS"/*/cache/context-window(N))
  local legacy_file
  for legacy_file in "${legacy[@]}"; do
    if [[ -e "$legacy_file" || -L "$legacy_file" ]]; then
      command env CCPROTEUS_STATE_DIR="$_CCPROTEUS_DIR" \
        "$_CCPROTEUS_LIB_DIR/model-window" --migrate || {
        _ccs_err "context migration incomplete; original data kept for retry"
        return 1
      }
      return 0
    fi
  done
}

_ccs_migrate_profile_home() {
  # Fresh installs and completed migrations need no interpreter or lock.
  if [[ ! -e "$HOME/.claude-profiles" && ! -L "$HOME/.claude-profiles" ]]; then
    return 0
  fi
  if [[ -L "$HOME/.claude-profiles" && -d "$_CCPROTEUS_PROFILE_DIRS" &&
        ! -L "$_CCPROTEUS_PROFILE_DIRS" &&
        "${${:-$HOME/.claude-profiles}:A}" == "${_CCPROTEUS_PROFILE_DIRS:A}" ]]; then
    return 0
  fi
  command python3 - "$HOME/.claude-profiles" "$_CCPROTEUS_PROFILE_DIRS" <<'PY'
import fcntl, os, sys, time
from pathlib import Path

legacy, target = map(Path, sys.argv[1:])
try:
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.parent.is_symlink():
        raise ValueError('refusing symlinked state root')
    fd = os.open(target.parent / '.profile-home-migration.lock',
                 os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as lock:
        deadline = time.monotonic() + 10
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise ValueError('another profile migration is still running; retry shortly')
                time.sleep(0.05)
        if legacy.is_symlink():
            if legacy.resolve() != target.resolve() or not target.is_dir() or target.is_symlink():
                raise ValueError('legacy profile link points elsewhere; nothing moved')
        elif legacy.exists():
            if not legacy.is_dir() or target.exists() or target.is_symlink():
                raise ValueError('both profile locations exist or are not directories; nothing moved')
            # Rename on the same filesystem: never copy credentials or merge homes.
            os.rename(legacy, target)
            try:
                os.symlink(str(target), legacy, target_is_directory=True)
            except OSError:
                if not legacy.exists() and not legacy.is_symlink():
                    os.rename(target, legacy)
                raise
            print(f'ccproteus: profile storage moved to {target}; legacy path retained as a compatibility link', file=sys.stderr)
except (OSError, ValueError) as exc:
    print(f'ccproteus: profile directory migration incomplete: {exc}', file=sys.stderr)
    sys.exit(1)
PY
}

_ccs_normalize_plugin_index_paths() {
  # Claude records the absolute profile spelling used during installation. Profiles
  # now share ~/.claude/plugins, so repair only paths made stale by that topology.
  # Keep the settled launch path fork-free; malformed indexes remain best-effort.
  local index content needs_repair=0
  for index in "$HOME/.claude/plugins/known_marketplaces.json" \
               "$HOME/.claude/plugins/installed_plugins.json"; do
    [[ -f "$index" ]] || continue
    content=$(<"$index") || continue
    if [[ "$content" == *"$HOME/.claude-profiles/"* ||
          "$content" == *"$_CCPROTEUS_PROFILE_DIRS/"* ]]; then
      needs_repair=1
      break
    fi
  done
  (( needs_repair )) || return 0

  command python3 - "$HOME" "$_CCPROTEUS_PROFILE_DIRS" <<'PY' 2>/dev/null || return 0
import json, os, sys, tempfile
from pathlib import Path

home, profile_root = map(Path, sys.argv[1:])
plugins = home / ".claude" / "plugins"
legacy_root = home / ".claude-profiles"
indexes = (
    (plugins / "known_marketplaces.json", "marketplaces"),
    (plugins / "installed_plugins.json", "cache"),
)


def canonical_path(value, kind):
    if not isinstance(value, str) or not os.path.isabs(value):
        return None
    path = Path(value)
    for root in (legacy_root, profile_root):
        try:
            relative = path.relative_to(root)
        except ValueError:
            continue
        parts = relative.parts
        if len(parts) < 3 or parts[1] != "plugins" or parts[2] != kind:
            return None
        target = plugins.joinpath(*parts[2:])
        return target if target.exists() else None
    return None


def load(path):
    try:
        with path.open() as stream:
            return json.load(stream)
    except Exception:
        return None


def write(path, data):
    mode = path.stat().st_mode & 0o777
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(data, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


known_path, _ = indexes[0]
known = load(known_path)
if isinstance(known, dict):
    changed = False
    for record in known.values():
        if not isinstance(record, dict):
            continue
        target = canonical_path(record.get("installLocation"), "marketplaces")
        if target is not None and record["installLocation"] != str(target):
            record["installLocation"] = str(target)
            changed = True
    if changed:
        write(known_path, known)

installed_path, _ = indexes[1]
installed = load(installed_path)
records = installed.get("plugins") if isinstance(installed, dict) else None
if isinstance(records, dict):
    changed = False
    for versions in records.values():
        if not isinstance(versions, list):
            continue
        for record in versions:
            if not isinstance(record, dict):
                continue
            target = canonical_path(record.get("installPath"), "cache")
            if target is not None and record["installPath"] != str(target):
                record["installPath"] = str(target)
                changed = True
    if changed:
        write(installed_path, installed)
PY
}

_ccs_secure_dirs() {
  local f root

  for root in "$_CCPROTEUS_DIR" "$_CCPROTEUS_PROFILE_DIRS"; do
    if [[ -L "$root" ]]; then
      _ccs_err "refusing symlinked state root '$root'"
      return 1
    fi
  done

  _ccs_migrate_profile_home || return 1
  command mkdir -p "$_CCPROTEUS_DIR" "$_CCPROTEUS_PROFILE_DIRS" || return 1
  _ccs_normalize_plugin_index_paths
  # Permission hardening is best-effort: never fail on a filesystem that
  # refuses chmod. The symlink guards above are the real safety invariant.
  command chmod 700 "$_CCPROTEUS_DIR" "$_CCPROTEUS_PROFILE_DIRS" 2>/dev/null || :

  if [[ -e "$_CCPROTEUS_DIR/active" || -L "$_CCPROTEUS_DIR/active" ]]; then
    [[ ! -L "$_CCPROTEUS_DIR/active" ]] || return 1
    command chmod 600 "$_CCPROTEUS_DIR/active" 2>/dev/null || :
  fi

  for f in "$_CCPROTEUS_DIR"/*.conf(N); do
    [[ ! -L "$f" ]] || return 1
    command chmod 600 "$f" 2>/dev/null || :
  done

  # Last: the state root exists and is sane, so renames within it are safe.
  _ccs_migrate_state
}

_ccs_atomic_write_unlocked() {
  local f="$1"
  local value="$2"
  local parent="${1:h}"
  local tmp

  [[ -d "$parent" ]] || command mkdir -p "$parent" || return 1
  command chmod 700 "$parent" || return 1

  tmp=$(command mktemp "$parent/.ccproteus.tmp.XXXXXX") || return 1
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
_ccs_write() {
  _ccs_atomic_write_unlocked "$1" "$2"
}

_ccs_remove_file() {
  [[ -e "$1" || -L "$1" ]] || return 0
  command rm -f "$1"
}

_ccs_read_active() {
  local f="$_CCPROTEUS_DIR/active"

  REPLY=""
  [[ -e "$f" || -L "$f" ]] || return 1
  [[ -f "$f" && ! -L "$f" ]] || {
    _ccs_err "invalid active-profile file"
    return 1
  }

  command chmod 600 "$f" || return 1
  REPLY=$(<"$f") || return 1

  _ccs_valid "$REPLY" || {
    _ccs_err "invalid active profile '$REPLY'"
    REPLY=""
    return 1
  }
}

_ccs_read() {
  typeset -gA _ccp=()

  local name="$1" readonly="${2:-0}"
  local file="$_CCPROTEUS_DIR/$1.conf"
  local line k v
  local -A seen=()

  _ccs_valid "$name" || {
    _ccs_err "invalid profile name"
    return 1
  }

  [[ -f "$file" && ! -L "$file" ]] || {
    _ccs_err "invalid profile file for '$name'"
    return 1
  }

  if (( ! readonly )); then
    command chmod 600 "$file" || {
      _ccs_err "cannot secure profile '$name'"
      return 1
    }
  fi

  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue

    if [[ "$line" == *$'\r'* || "$line" != *=* ]]; then
      _ccs_err "malformed config for '$name'"
      unset _ccp
      return 1
    fi

    k="${line%%=*}"
    v="${line#*=}"

    if [[ -z "$k" || "$k" == *[[:space:]]* ]]; then
      _ccs_err "invalid key in '$name'"
      unset _ccp
      return 1
    fi

    if [[ -n "${seen[$k]:-}" ]]; then
      _ccs_err "duplicate key '$k' in '$name'"
      unset _ccp
      return 1
    fi

    case "$k" in
      type|email|token|base_url|api_key|model|adapter|tag|provider|context_discovered|window_sources)
        ;;
      # Removed key. A conf written before the transport seam was dropped must
      # still launch, so say what replaced it once and carry on without it.
      transport)
        print -u2 "ccproteus: transport= was removed; export HTTPS_PROXY from your bridge instead"
        continue
        ;;
      forward_ip)
        print -u2 "ccproteus: forward_ip= was removed; the adapter no longer sends X-Forwarded-For"
        continue
        ;;
      subagent_model)
        print -u2 "ccproteus: subagent_model= was removed; it was never read"
        continue
        ;;
      lean)
        print -u2 "ccproteus: lean= was removed; launch behavior no longer varies by it"
        continue
        ;;
      role)
        print -u2 "ccproteus: role= was removed; pick the model with --model instead"
        continue
        ;;
      excluded_models)
        [[ -n "$v" && "$v" != *[[:space:]]* && "$v" != ,* && "$v" != *, && "$v" != *,,* ]] || {
          _ccs_err "excluded_models must be comma-separated exact model IDs"; unset _ccp; return 1
        }
        ;;
      model_map)
        command python3 - "$v" <<'PY' 2>/dev/null || {
import json, re, sys
try:
    pairs = json.loads(sys.argv[1], object_pairs_hook=list)
    valid = isinstance(pairs, list) and sys.argv[1].lstrip().startswith('{')
    ids = re.compile(r'[A-Za-z0-9._:\[\]()@/-]+')
    valid = valid and all(isinstance(k, str) and isinstance(v, str)
                          and ids.fullmatch(k) and ids.fullmatch(v) for k, v in pairs)
    valid = valid and len({k for k, v in pairs}) == len(pairs)
    # A key is the model id as the gateway sees it. The [1m] window marker is
    # ccproteus's own tag, stripped from the lookup key before any model is named,
    # and the picker reads that marker case-insensitively -- so a key carrying it,
    # in either spelling, names no model.
    valid = valid and not any(k.lower().endswith("[1m]") for k, _ in pairs)
except (ValueError, TypeError):
    valid = False
sys.exit(0 if valid else 1)
PY
          _ccs_err "model_map must be a JSON object of unique model IDs to model IDs, and no key may carry the [1m] suffix"
          unset _ccp; return 1
        }
        ;;
      # Stark's spend cap lives in the shared conf; ccproteus only keeps it so a rewrite does not drop it.
      monthly_budget_usd)
        [[ "$v" =~ '^([0-9]+\.?[0-9]*|\.[0-9]+)$' && "$v" == *[1-9]* ]] || {
          _ccs_err "monthly_budget_usd must be a positive number"; unset _ccp; return 1
        }
        ;;
      model_defaults)
        [[ "$v" == 1 ]] || { _ccs_err "invalid model defaults marker"; unset _ccp; return 1; }
        ;;
      private)
        [[ "$v" == 1 ]] || { _ccs_err "private must be 1"; unset _ccp; return 1; }
        ;;
      effort|effort.*)
        [[ "$v" == (low|medium|high|xhigh|max) && "$k" != effort. ]] || {
          _ccs_err "invalid effort setting '$k'"; unset _ccp; return 1
        }
        ;;
      env.*)
        if (( ! ${_CCPROTEUS_PROFILE_ENV[(Ie)${k#env.}]} )); then
          _ccs_err "disallowed environment key '$k' in '$name'"
          unset _ccp
          return 1
        fi
        ;;
      context_window.*)
        local _ctx_model="${k#context_window.}"
        [[ -n "$_ctx_model" && "$_ctx_model" != *[=$'\r\n']* && "$v" == <-> && "$v" -gt 0 ]] || {
          _ccs_err "invalid context pin '$k' in '$name'"
          unset _ccp
          return 1
        }
        ;;
      *)
        _ccs_err "unknown key '$k' in '$name'"
        unset _ccp
        return 1
        ;;
    esac

    seen[$k]=1
    _ccp[$k]="$v"
  done < "$file"

  # `proxy` was the old spelling for a key-authenticated routed endpoint. Fold
  # it at the boundary so every runtime path has one profile type to maintain.
  if [[ "${_ccp[type]:-}" == proxy ]]; then
    [[ -n "${_ccp[base_url]:-}" ]] || {
      _ccs_err "proxy '$name' has no base_url"
      unset _ccp
      return 1
    }
    _ccp[type]=apikey
  fi

  local _ccs_type_keys=""
  case "${_ccp[type]:-}" in
    sub)
      # The email IS the profile identity for this type, so a blank one is not a
      # cosmetic gap — `list` shows a nameless row and nothing ever re-flags it.
      # The token case has always required its own credential; this is the same
      # rule, applied to the type that was missing it.
      [[ -n "${_ccp[email]:-}" ]] || {
        _ccs_err "sub profile '$name' has no email — re-create it: ccproteus rm $name && ccproteus add $name"
        unset _ccp
        return 1
      }

      _ccs_type_keys="email"
      ;;

    token)
      [[ -n "${_ccp[token]:-}" ]] || {
        _ccs_err "token profile '$name' has no token"
        unset _ccp
        return 1
      }
      _ccs_type_keys="token"
      ;;

    apikey)
      _ccs_type_keys="api_key|base_url|excluded_models|adapter|tag|env.*|context_window.*|window_sources|model_map"
      ;;

    provider)
      [[ -n "${_ccp[provider]:-}" ]] || {
        _ccs_err "provider profile '$name' has no provider"
        unset _ccp
        return 1
      }
      _ccs_type_keys="provider|excluded_models|tag|env.*|context_window.*|window_sources"
      ;;

    *)
      _ccs_err "invalid or missing type in '$name'"
      unset _ccp
      return 1
      ;;
  esac

  # Keys every type accepts, plus the type's own. A sub or token profile carries
  # only model-mapping env keys; routed types accept any allowlisted env key.
  local _ccs_allowed="type|model|model_defaults|private|monthly_budget_usd|effort|effort.*|context_discovered|$_ccs_type_keys"
  for k in "${(@k)_ccp}"; do
    [[ "$k" == (${~_ccs_allowed}) ]] && continue
    [[ "$k" == env.* ]] && (( ${_CCPROTEUS_MODEL_MAPPING_ENV[(Ie)${k#env.}]} )) && continue
    _ccs_err "key '$k' is invalid for ${_ccp[type]} profile '$name'"
    unset _ccp
    return 1
  done
}

_ccs_validate_base_url() {
  local label="$1"

  [[ "${_ccp[base_url]}" == http://* ||
     "${_ccp[base_url]}" == https://* ]] || {
    _ccs_err "$label base_url must use http:// or https://"
    return 1
  }

  [[ "${_ccp[base_url]}" != *[[:space:]]* ]] || {
    _ccs_err "$label base_url contains whitespace"
    return 1
  }
}

# An apikey profile authenticates with ANTHROPIC_API_KEY instead of an OAuth
# subscription. base_url and model are optional on purpose: omit both for a
# native Anthropic API key (Claude Code owns the model list), set both for a
# gateway that speaks the Anthropic API under its own model names.
_ccs_validate_apikey() {
  local name="$1"

  [[ -n "${_ccp[api_key]:-}" ]] || {
    _ccs_err "apikey '$name' has no api_key"
    return 1
  }

  [[ "${_ccp[api_key]}" != *[[:space:]]* ]] || {
    _ccs_err "apikey '$name' api_key contains whitespace"
    return 1
  }

  if [[ -n "${_ccp[base_url]:-}" ]]; then
    _ccs_validate_base_url "apikey '$name'" || return 1
  fi

  # adapter=true means "this endpoint speaks dialects Claude Code does not, put a
  # translator in front of it at launch". It is a declaration of intent; the
  # mechanism belongs to the framework, so no adapter is ever named here. The
  # profile keeps its real base_url and only the launched environment is
  # redirected, which keeps `ccproteus _env` truthful for every other consumer.
  if [[ -n "${_ccp[adapter]:-}" ]]; then
    [[ "${_ccp[adapter]}" == (true|false) ]] || {
      _ccs_err "apikey '$name' adapter must be 'true', 'false', or absent"
      return 1
    }

    if [[ "${_ccp[adapter]}" == true ]]; then
      # A native Anthropic key has no foreign dialect to translate.
      [[ -n "${_ccp[base_url]:-}" ]] || {
        _ccs_err "apikey '$name' has adapter=true but no base_url to front"
        return 1
      }
    fi
  fi
}

# Dispatch validation for the currently loaded profile (_ccp). sub and token
# profiles carry nothing ccproteus has to check beyond _ccs_read.
_ccs_validate_loaded() {
  [[ "${_ccp[type]:-}" != apikey ]] || _ccs_validate_apikey "$1"
}
