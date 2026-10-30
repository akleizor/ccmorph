# ccmorph-profiles.zsh — Profile homes and choices: the shared harness layer linked into each profile home, profile and value pickers, sign-in state, and committing a new profile.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

_ccm_accounts_hint() {
  # Name the profiles that exist, on stderr, beside an error about one that does not.
  # A user who mistyped or transposed an argument needs the valid set in front of
  # them, not a second command to run to find it.
  local -a names
  local f
  for f in "$_CCMORPH_DIR"/*.conf(N); do
    names+=("${f:t:r}")
  done
  (( ${#names} )) || return 0
  _ccm_err "profiles: ${names[*]}"
}

_ccm_account_model() {
  # One authority for launch, listing, picker and context resolution. Before the
  # first launch this reads the seed without writing it; afterwards native inline
  # changes cannot become profile defaults.
  local profile="$1" value
  value=$(_ccm_config_store get "$profile" model 2>/dev/null) || return 1
  _ccm_clean_value "$value" || return 1
  [[ -n "$value" ]] || return 1
  print -r -- "$value"
}

# The presets `ccmorph add` offers, as picker rows "id<TAB>label<TAB>base URL" in $REPLY.
_ccm_provider_presets() {
  local line
  REPLY=""
  while IFS= read -r line; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    local -a f=("${(@ps:\t:)line}")
    REPLY+="${REPLY:+$'\n'}${f[1]}"$'\t'"${f[2]}"$'\t'"${f[3]#https://}"
  done < "${_CCMORPH_LIB_DIR:h}/share/provider-presets.tsv"
}

# One preset by id: reply=(label, base URL, opus, sonnet, haiku).
_ccm_provider_preset() {
  local line
  reply=()
  while IFS= read -r line; do
    [[ "$line" == "$1"$'\t'* ]] || continue
    reply=("${(@)${(@ps:\t:)line}[2,6]}")
    return 0
  done < "${_CCMORPH_LIB_DIR:h}/share/provider-presets.tsv"
  return 1
}

# Provider IDs stay unchanged; only human-facing product labels are cased.
_ccm_provider_label() {
  case "$1" in
    xai) REPLY=xAI ;;
    *) REPLY="${(C)1}" ;;
  esac
}

_ccm_profile_rows() {
  local active="${1:-}"
  local -a types=("${@:2}")
  local f name type detail marker model

  for f in "$_CCMORPH_DIR"/*.conf(N); do
    name="${f:t:r}"
    if ! _ccm_read "$name" 1; then
      print -u2 "ccmorph: skipping invalid profile '$name'"
      continue
    fi

    type="${_ccm[type]}"
    (( ${#types} == 0 || ${types[(Ie)$type]} )) || continue
    detail=""
    marker=""
    case "$type" in
      sub)
        type="Claude account"
        detail="${_ccm[email]:-}"
        ;;
      token) type="Claude token" ;;
      provider)
        _ccm_provider_label "${_ccm[provider]}"
        type="$REPLY"
        model=$(_ccm_account_model "$name") || model=""
        detail="${model:-Browser login}"
        ;;
      apikey)
        type="API key"
        model=$(_ccm_account_model "$name") || model=""
        detail="${_ccm[base_url]:-api.anthropic.com}${model:+ → ${model}}"
        ;;
    esac
    [[ "$name" == "$active" ]] && marker="default"
    detail="${detail//$'\t'/ }"
    print -r -- "$name"$'\t'"$type"$'\t'"$detail"$'\t'"$marker"
  done
  unset _ccm
}

_ccm_menu_profile() {
  local active="" rows
  [[ -f "$_CCMORPH_DIR/active" && ! -L "$_CCMORPH_DIR/active" ]] && active=$(<"$_CCMORPH_DIR/active")
  rows=$(_ccm_profile_rows "$active" "$@") || return 1
  [[ -n "$rows" ]] || {
    if (( $# )); then
      _ccm_err "no compatible profiles for this command. Run: ccmorph add <profile>"
    else
      _ccm_err "no saved profiles. Run: ccmorph add <profile>"
    fi
    return 1
  }

  ccm_ui_select search Profile "$rows" all
}

# Every value a user cannot be expected to have memorised, chosen from a list.
#
# `-i` used to mean "ask" at launch and "invalid argument" at `set`, which is the
# defect this closes: a reader who learned the flag once had to learn, per verb,
# whether it worked there. The listing is not enumerable in advance either -- a
# gateway's model set changes without ccmorph knowing -- so refusing to ask was
# refusing the only reliable way to name a model correctly.
#
# $1 = what to choose (model|effort), $2 = profile. Rows feed every menu.
_ccm_value_rows() {
  local slot="$1" profile="$2" kind
  case "$slot" in
    model)
      _ccm_read "$profile" || return 1
      kind="${_ccm[type]}"
      unset _ccm
      case "$kind" in
        apikey) _ccm_gw_models "$profile" picker "${@:3}" ;;
        sub|token)
          # Native role aliases, not copied versioned model IDs or capabilities.
          print -r -- $'fable\tClaude Code Fable alias\nopus\tClaude Code Opus alias\nsonnet\tClaude Code Sonnet alias\nhaiku\tClaude Code Haiku alias'
          ;;
        provider)
          # Provider login is owned by the bridge. Its saved native picker rows
          # are useful offline; do not start a credentialed proxy just for a menu.
          _ccm_util saved-provider-models "$_CCMORPH_PROFILE_DIRS/$profile/.claude.json"
          ;;
      esac
      ;;
    effort)
      print -r -- $'low\tFastest, least thorough\nmedium\tBalanced\nhigh\tMore reasoning\nxhigh\tMost reasoning, slowest'
      ;;
    *) _ccm_err "no picker for '$slot'"; return 1 ;;
  esac
}

# Effort stays a compact fixed menu; model lists remain searchable.
_ccm_choose_value() {
  local slot="$1" profile="$2" prompt="$3" rows="$4" display="${5:-labels}"
  if [[ "$slot" == effort ]]; then
    local row value label detail labeled=""
    for row in "${(@f)rows}"; do
      value="${row%%$'\t'*}"
      if [[ "$value" != @* ]]; then
        detail="${row#*$'\t'}"
        [[ "$display" != labels ]] || detail="${detail#*$'\t'}"
        label="${(C)value}"
        [[ "$value" != xhigh ]] || label="Extra high"
        row="$value"$'\t'"$label"$'\t'"$detail"
      fi
      [[ -z "$labeled" ]] || labeled+=$'\n'
      labeled+="$row"
    done
    rows="$labeled"; display=labels
    ccm_ui_select native "$prompt" "$rows" "$display"
  elif [[ "$display" == all ]]; then
    ccm_ui_select search "$prompt" "$rows" all
  else
    ccm_ui_select search "$prompt" "$rows"
  fi
}

_ccm_pick_value() {
  local slot="$1" profile="$2" rows=""
  rows=$(_ccm_value_rows "$slot" "$profile") || return 1
  [[ -n "$rows" ]] || {
    _ccm_err "No saved model list. Use: ccmorph config $profile to enter a custom ID."
    return 1
  }
  _ccm_choose_value "$slot" "$profile" "${(C)slot} — $profile" "$rows" all || {
    print -ru2 -- "Use: ccmorph set $profile $slot <$slot>"
    return 1
  }
}

# Configure preserves a current value by default and allows custom IDs without
# pretending an unavailable catalogue means the profile cannot be configured.
_ccm_prompt_default() {
  local slot="$1" profile="$2" current="$3" rows="" choice="" answer=""
  if [[ ! -t 0 ]]; then
    ccm_ui_input "Default $slot [${current:-Claude default}]: " || return 1
    answer="$REPLY"
    print -r -- "${answer:-$current}"
    return 0
  fi
  [[ "$slot" == model && "${_CCM_CONFIG_TYPE:-}" == apikey ]] &&
    print -ru2 -- 'Loading models from the selected endpoint…'
  rows=$(_ccm_value_rows "$slot" "$profile" "${_CCM_CONFIG_ENDPOINT:-}" "${_CCM_CONFIG_CREDENTIAL:-}") || rows=""
  if [[ "$slot" == model && -z "$rows" ]]; then
    print -ru2 -- 'Model list unavailable. Keep the current default or enter a custom model ID.'
  fi
  if [[ -n "$rows" ]]; then
    local row value description rendered=""
    for row in "${(@f)rows}"; do
      value="${row%%$'\t'*}"
      description="${row#*$'\t'}"
      [[ "$value" == "$current" ]] && continue
      [[ -z "$rendered" ]] || rendered+=$'\n'
      rendered+="$value"$'\t'"$value"$'\t'"$description"
    done
    rows="$rendered"
  fi
  [[ -z "$rows" ]] || rows=$'\n'"$rows"
  rows=$'@keep\t'"Keep ${current:-Claude default}$rows"
  [[ "$slot" == model ]] && rows+=$'\n@custom\tEnter a custom model ID…'
  choice=$(_ccm_choose_value "$slot" "$profile" "Default $slot — $profile" "$rows") || return 1
  case "$choice" in
    @keep) print -r -- "$current" ;;
    @custom)
      ccm_ui_input 'Custom model ID (Enter keeps current): ' || return 1
      answer="$REPLY"
      print -r -- "${answer:-$current}"
      ;;
    *) print -r -- "$choice" ;;
  esac
}

# Run `claude auth <verb...>` against one config dir with the ambient backend
# and private-session credential env scrubbed (subshell: caller env untouched).
_ccm_claude_auth() {
  local dir="${1:?_ccm_claude_auth requires a config dir}"
  shift
  (
    unset "${_CCMORPH_BACKEND_ENV[@]}" CLAUDE_SECURESTORAGE_CONFIG_DIR
    CLAUDE_CONFIG_DIR="$dir" command claude auth "$@"
  )
}

_ccm_logged_email() {
  # $1 = the CLAUDE_CONFIG_DIR to read auth status from, required. A profile's
  # profile state lives in its own dir, so reading it is always dir-scoped —
  # an unscoped read would report whatever the base install happens to hold,
  # which is never the answer a caller wants about a specific profile.
  local dir="${1:?_ccm_logged_email requires a config dir}"
  local out

  # Scrub the ambient credential env before asking. `auth status` answers about
  # whatever credential it can find, and an exported ANTHROPIC_API_KEY wins:
  # it reports {"loggedIn": true, "authMethod": "api_key"} for a config dir that
  # never completed a browser login — even an empty one. ccmorph itself exports
  # that variable per profile, so any shell it launched poisons the answer, and
  # `add sub` would then commit a subscription profile on the strength of an API
  # key that has nothing to do with it. Scrubbed in a subshell so the caller's
  # environment is untouched.
  out=$(_ccm_claude_auth "$dir" status 2>&1) || {
    _ccm_err "could not read authentication status"
    return 1
  }

  # A subscription login is an OAuth session, so `loggedIn` alone is not the
  # question — authMethod is. Anything key-shaped (api_key, bedrock, vertex) is
  # not the thing `add sub` is verifying, and treating it as one is how a blank
  # profile got written. Absent authMethod means an older CLI shape: fall back to
  # loggedIn so this degrades to the previous behaviour rather than refusing.
  _ccm_loggedin=false
  if print -r -- "$out" | command grep -q '"loggedIn": true'; then
    if print -r -- "$out" | command grep -q '"authMethod"'; then
      print -r -- "$out" | command grep -qE '"authMethod": "(api_key|bedrock|vertex)"' ||
        _ccm_loggedin=true
    else
      _ccm_loggedin=true
    fi
  fi

  _ccm_email=$(
    print -r -- "$out" |
      command grep -o '"email": "[^"]*"' |
      command head -1 |
      command cut -d'"' -f4
  )
}

_ccm_physical_dir() {
  # Physical path of directory $1, following every link as the kernel does
  # (chasedots makes `link/..` leave the link target, not the link's parent).
  # -q: this runs inside $(...) in the user's interactive shell, where a printing
  # chpwd hook would join the capture and corrupt the link written from it.
  # The trailing `/` is a sentinel: it stops the caller's $(...) from stripping
  # newlines that end the directory's own name; callers remove it (#267).
  (setopt chasedots chaselinks; builtin cd -q -- "$1" 2>/dev/null && print -rn -- "$PWD/")
}

_ccm_physical_prefix() {
  # Physical path of $1's longest prefix that resolves, with the rest kept as
  # spelled. A missing dir or a dangling link mid-path — and everything after
  # it, `..` included, since the kernel cannot resolve through it — stays
  # textual, so a link recreated through it still dangles where it pointed
  # rather than `:a` collapsing `out/..` onto another file (#376).
  local head="$1" rest="" dir
  until dir="$(_ccm_physical_dir "$head")"; do
    # Nothing left to strip: `${head:h}` of `/` is `/`, and of an empty string
    # it stays empty, so without this the walk never ends.
    [[ "$head" == "${head:h}" ]] && return 1
    rest="/${head:t}$rest"
    head="${head:h}"
  done
  # `pwd -P` is `/` only at the root, where the sentinel doubles it, and a
  # walk that ends there is the root itself.
  [[ "$dir" == // ]] && dir=""
  # The trailing `/` is the sentinel _ccm_physical_dir writes, for the same
  # reason: this runs inside the caller's $(...), which would otherwise eat a
  # newline ending the last component's name and recreate the link on the
  # shorter name (#267).
  print -r -- "${dir%/}${rest:-/}/"
}

_ccm_migrate_projects() {
  # Only session/history stores and agent definitions may be merged: those are
  # content the profile itself owns, so moving them into the shared store loses
  # nothing and leaves every profile working. Package and hook collisions belong
  # to their owning tools; preserve profile content in place. `agents` belongs in
  # the first group — it joined the shared manifest without joining this case
  # list, so a profile that already had a real agents/ failed provisioning (and
  # therefore every launch) instead of being healed, while docs/ARCHITECTURE.md
  # documented the merge.
  # Every local is declared here, once: zsh prints `name=value` when `local`
  # re-declares a set variable, and inside the loop that lands in `_env` output.
  local from="$1" to="$2" bak bakp f rel dest tgt phys leaf pdir
  case "${to:t}" in
    agents|projects|session-env|file-history) ;;
    *)
      _ccm_err "shared '${to:t}' collision at '$from'; nothing moved — reconcile through its owning tool"
      return 1
      ;;
  esac
  bak="${from}.premigration.${$}.${RANDOM}"

  command mv "$from" "$bak" 2>/dev/null || return 1
  bakp="${bak:A}"

  # Files and links (find does not follow links, so a symlinked dir is one
  # entry). Links move as links; a name already in the shared store wins.
  # -print0 with read -d '': a name may itself contain a newline, and a
  # newline-separated walk split it — the real entry never left the backup and
  # the remainder reached mv as a path relative to the caller's cwd (#400).
  command find "$bak" \( -type f -o -type l \) -print0 2>/dev/null | while IFS= read -r -d '' f; do
    case "${f:t}" in
      .DS_Store|.localized) continue ;;
    esac
    rel="${f#$bak/}"
    dest="$to/$rel"
    [[ -e "$dest" || -L "$dest" ]] && continue
    command mkdir -p "${dest:h}" 2>/dev/null || continue
    tgt=""
    # -n: readlink prints no trailing newline of its own, so the `/` sentinel is
    # the only thing keeping $(...) from stripping one that ends the target's
    # own name — the class #267 fixed in _ccm_physical_dir. Without it a
    # relative target ending in a newline was rewritten to the shortened name
    # and the original link then deleted (#399). The consumer drops the
    # sentinel: one `/` is all it adds, so `%/` takes back the sentinel and
    # never a `/` that ends the target itself, and every test below then reads
    # the literal target.
    [[ -L "$f" ]] && tgt="$(command readlink -n "$f" && print -rn -- /)"
    tgt="${tgt%/}"
    if [[ -n "$tgt" && "$tgt" != /* ]]; then
      # A relative link resolves from its directory, which is about to change.
      # Resolve it from that directory's physical path (the rename kept the
      # parent, and find never descends a link, so it is $bakp/<rel dir>), the
      # way the kernel does. A target inside the migrated tree moves to the
      # shared store too, so point at its new place there; any other target
      # becomes an absolute link to its physical (:A) path.
      # zsh `:a`/`:A` drop `x/..` textually, wrong when x is a link, so the
      # parent is resolved by the shell's physical cd; the last component stays
      # unresolved, so a link to a sibling link follows that sibling. A target
      # ending in . or .. has no link leaf: resolve the whole path.
      phys="$bakp/${rel:h}/$tgt"
      case "$tgt" in
        .|..|*/.|*/..) leaf="" ;;
        *) leaf="/${phys:t}"; phys="${phys:h}" ;;
      esac
      # A link dir earlier in this loop already moved to the shared store
      # (same physical target), so retry the parent there before giving up.
      # When the parent does not resolve anywhere the link is still recreated,
      # dangling: dropping it took the agent definition out of the profile with
      # no message (#167). Only the longest existing prefix of the parent is
      # resolved physically, and the components the kernel cannot resolve stay
      # as spelled, so the link dangles at its true location instead of
      # collapsing onto another file (#376). Never `:a` the whole parent: it
      # drops `x/..` textually whether or not x resolves.
      pdir="$(_ccm_physical_dir "$phys")" ||
        pdir="$(_ccm_physical_dir "$to/${phys#$bakp/}")" ||
        pdir="$(_ccm_physical_prefix "$phys")" || continue
      phys="${pdir%/}$leaf"
      case "$phys" in
        "$bakp"/*) phys="$to/${phys#$bakp/}" ;;
      esac
      command ln -s "$phys" "$dest" 2>/dev/null && command rm -f "$f"
    else
      command mv "$f" "$dest" 2>/dev/null
    fi
  done

  return 0
}

_ccm_session_file() {
  # REPLY = the physical transcript of session $1, from native Claude Code's
  # history or any profile's. History is per profile (each CLAUDE_CONFIG_DIR keeps
  # its own projects/), so a session is looked up across all of them.
  local id="$1" store
  local -a hits
  REPLY=""
  [[ "$id" =~ '^[A-Za-z0-9_-]+$' ]] || return 1
  for store in "$HOME/.claude/projects" "$_CCMORPH_PROFILE_DIRS"/*/projects(N); do
    hits=("$store"/*/"$id".jsonl(N-.))
    (( ${#hits} )) || continue
    REPLY="${hits[1]:A}"
    return 0
  done
  return 1
}

_ccm_carry_session() {
  # Claude Code resumes only sessions under its own projects/. A hop or a
  # `--resume <id>` into another profile links that one session in: its
  # transcript (Claude Code appends through the link, so the history stays one
  # file) and its sidecar dir of subagent transcripts and tool results.
  # 0 when the session is already in $2 or now linked there, 1 when no store has it.
  local id="$1" dst="$2" f slug
  local -a here
  [[ "$id" =~ '^[A-Za-z0-9_-]+$' ]] || return 1
  # -. follows links: a link left dangling by a removed profile is replaced.
  here=("$dst"/*/"$id".jsonl(N-.))
  (( ${#here} )) && return 0
  _ccm_session_file "$id" || return 1
  f="$REPLY" slug="${REPLY:h:t}"
  command mkdir -p "$dst/$slug" &&
    command ln -sfn "$f" "$dst/$slug/$id.jsonl" || return 1
  # The sidecar dir is made at the source first, so subagent transcripts written
  # later land beside the transcript rather than in the carrying profile.
  command mkdir -p "${f:r}" && [[ ! -e "$dst/$slug/$id" ]] && command ln -sfn "${f:r}" "$dst/$slug/$id"
  return 0
}

_ccm_dir_ok() {
  # Lock-free predicate: is this profile already correctly provisioned?
  # Verifies only the load-bearing invariants — shared session history and
  # auth present — so the common launch path needs neither the lock nor any
  # mutation, and therefore cannot wedge on a lock/chmod/write failure.
  local name="$1"
  local dir="${_CCMORPH_PROFILE_DIRS}/$name"

  _ccm_valid "$name" || return 1
  [[ -d "$dir" && ! -L "$dir" ]] || return 1
  [[ -f "$dir/config.json" && -f "$dir/.claude.json" ]] || return 1
  # History is the profile's own; a link left by the shared-history layout heals.
  [[ -d "$dir/projects" && ! -L "$dir/projects" ]] || return 1
  # The shared harness layer must be fully wired: a manifest entry present in
  # ~/.claude but unlinked here (content installed after this profile was
  # created, or a pre-manifest profile) routes through full provisioning to
  # heal. A real profile-local FILE is a deliberate override — healthy as-is.
  local item link src canonical="$HOME/.claude"
  for item in "${_CCMORPH_SHARED_DIRS[@]}"; do
    link="$dir/$item" src="$canonical/$item"
    [[ -d "$src" ]] || return 1
    [[ -L "$link" && "${link:A}" == "${src:A}" ]] || return 1
  done
  for item in "${_CCMORPH_SHARED_FILES[@]}"; do
    link="$dir/$item" src="$canonical/$item"
    [[ -e "$src" ]] || continue
    [[ -e "$link" || -L "$link" ]] || return 1
    [[ -L "$link" && "${link:A}" != "${src:A}" ]] && return 1
  done
  return 0
}

_ccm_prelaunch() {
  # Run the host's CCMORPH_PRELAUNCH executable, when configured, as
  # `<exe> prelaunch <cwd>` with CLAUDE_CONFIG_DIR set to the launching profile.
  # It runs before Claude reads its config, for state that must be in place
  # before MCPs connect. Stdout-silent, best-effort, bounded, never blocks a
  # launch. Absent on a vanilla install.
  local exe="${CCMORPH_PRELAUNCH:-}"
  [[ -n "$exe" ]] || return 0
  exe=$(_ccm_resolve_exe CCMORPH_PRELAUNCH "$exe" 2>/dev/null) || return 0
  CLAUDE_CONFIG_DIR="${_CCMORPH_PROFILE_DIRS}/$1" \
    command perl -e 'alarm 10; exec @ARGV' "$exe" prelaunch "$PWD" </dev/null >/dev/null 2>&1 || :
}

_ccm_setup_dir() {
  # Fast path: an already-provisioned profile needs no work. Otherwise provision
  # directly — it is idempotent and its writes are atomic, so concurrent
  # first-time provisioning converges without a lock.
  _ccm_dir_ok "$1" && return 0
  _ccm_setup_dir_unlocked "$1"
}

_ccm_setup_dir_unlocked() {
  local name="$1"
  local dir="${_CCMORPH_PROFILE_DIRS}/$1"
  local canonical="$HOME/.claude"
  local item src link legacy_nested
  local -a legacy_entries

  _ccm_valid "$name" || return 1

  if [[ -L "$dir" || -L "$canonical" ]]; then
    _ccm_err "refusing symlinked profile state for '$name'"
    return 1
  fi

  command mkdir -p "$dir" "${_CCMORPH_SHARED_DIRS[@]/#/$canonical/}" || {
    _ccm_err "cannot create profile '$name'"
    return 1
  }

  # Best-effort hardening (see _ccm_secure_dirs): mkdir already created these
  # with the right mode, so a filesystem that refuses chmod must not brick
  # provisioning.
  command chmod 700 "$dir" "$canonical" 2>/dev/null || :

  _ccm_util provision-profile "$dir" "${_ccm[type]}"

  [[ $? -eq 0 ]] || {
    _ccm_err "profile setup failed for '$name'"
    return 1
  }

  # settings.json may be profile-local. At launch, ccmorph layers it over the
  # canonical ~/.claude/settings.json before injecting the launch environment, so
  # profile choices such as model and effort override global defaults while absent
  # settings still inherit globally. A legacy settings symlink is removed below;
  # the shared harness layer (see _CCMORPH_SHARED_DIRS/_FILES) stays symlinked.
  local shared=("${_CCMORPH_SHARED_DIRS[@]}" "${_CCMORPH_SHARED_FILES[@]}")

  for item in "${shared[@]}"; do
    src="$canonical/$item"
    link="$dir/$item"

    if [[ -e "$src" || -L "$src" ]]; then
      if [[ -L "$link" ]]; then
        if [[ "${link:A}" != "${src:A}" ]]; then
          _ccm_err "'$item' for '$name' points outside the shared store"
          return 1
        fi
      elif [[ -e "$link" ]]; then
        # Purge ignorable OS/editor metadata so a stray .DS_Store (Finder drops
        # one the instant the folder is opened) can't defeat the empty-dir heal.
        if [[ -d "$link" ]]; then
          command rm -f -- "$link"/.DS_Store(N) "$link"/.localized(N) 2>/dev/null
        fi

        legacy_nested="$link/$item"
        legacy_entries=("$link"/*(DN))

        if [[ -d "$link" &&
              -L "$legacy_nested" &&
              "${legacy_nested:A}" == "${src:A}" &&
              ${#legacy_entries} -eq 1 ]]; then
          command rm -f "$legacy_nested" || {
            _ccm_err "could not remove legacy nested '$item' link for '$name'"
            return 1
          }
        fi

        if [[ -d "$link" ]] && command rmdir "$link" 2>/dev/null; then
          command ln -sn "$src" "$link" || {
            _ccm_err "could not link '$item' for '$name'"
            return 1
          }
        elif [[ -d "$link" ]]; then
          # A non-empty real dir where a shared link belongs — a stale shell
          # replaced the projects link mid-session, or an older profile grew
          # local skills/plugins before the canonical dir existed. Migrate the
          # content into the shared store (rename keeps files held open by
          # live sessions valid; never overwrite an existing file) and link —
          # self-heal rather than brick the launch.
          _ccm_migrate_projects "$link" "$src" && command ln -sn "$src" "$link" || {
            _ccm_err "could not migrate profile-local '$item' for '$name'"
            return 1
          }
        else
          # A real profile-local FILE is a deliberate override (a per-profile
          # CLAUDE.md, say): respected, never replaced. The shared copy stays
          # available in ~/.claude and the launch proceeds.
          _ccm_log "OVERRIDE profile-local '$item' kept for profile=$name"
        fi
      elif ! ln -sn "$src" "$link"; then
        _ccm_err "could not link '$item' for '$name'"
        return 1
      fi
    fi
  done

  # Session history is the profile's own, as in a plain CLAUDE_CONFIG_DIR. The
  # shared-history layout linked it to ~/.claude/projects: drop only the link,
  # never its target, so that history stays native and any profile still reaches
  # a session in it by id (_ccm_carry_session).
  if [[ -L "$dir/projects" ]]; then
    command rm -f "$dir/projects" || {
      _ccm_err "could not unlink shared history for '$name'"
      return 1
    }
  fi
  command mkdir -p "$dir/projects" || {
    _ccm_err "could not create history for '$name'"
    return 1
  }
  command chmod 700 "$dir/projects" 2>/dev/null

  if [[ -L "$dir/settings.json" ]]; then
    command rm -f "$dir/settings.json" || {
      _ccm_err "could not remove legacy settings link for '$name'"
      return 1
    }
  fi

  _ccm_unwire_statusline "$dir"
}

_ccm_unwire_statusline() {
  # v1.7.x auto-wired a ccmorph statusline into every profile's settings.json.
  # The per-model window is now delivered engine-side (route-ceiling env +
  # native autoCompactWindow), the script no longer ships, and a leftover entry
  # would spawn a missing command on every render -- so the same chokepoint
  # that wired it cleans it up. Removes ONLY a statusLine that points at a
  # ccmorph-installed script; a user's own bar is never touched. Transitional:
  # deletable once no pre-1.8 profile remains in the wild.
  #
  # Ownership is proven by EXACT path, never by a suffix. The guard used to match
  # `command.endswith("/ccmorph/statusline")`, which is a strictly broader class
  # than ccmorph ever wrote: a personal bar at ~/bin/ccmorph/statusline -- the
  # likeliest collision, since someone writing a bar FOR their ccmorph setup would
  # name it that -- was silently deleted, breaking the promise made two lines above.
  #
  # The wire function this cleans up after (v1.7, faa1ea9) wrote exactly
  # `$_CCMORPH_LIB_DIR/statusline` and refused to replace an existing command
  # ("Already ours -> nothing to do. Someone else's -> theirs to keep."), so an
  # exact match cannot miss anything ccmorph installed. Both directories are
  # checked because this repo splits state-dir from lib-dir and a pre-1.8 profile
  # may hold either spelling.
  local file
  file=$(_ccm_profile_layer "${1:t}")
  [[ -f "$file" && ! -L "$file" ]] || return 0

  _CCM_SL_OWNED="$_CCMORPH_DIR/statusline"$'\n'"$_CCMORPH_LIB_DIR/statusline" \
    _ccm_util unwire-statusline "$file" 2>/dev/null

  # Never fatal: cleanup is best-effort and must not block a launch.
  return 0
}

_ccm_keep_history() {
  # Move a profile's own transcripts into native history. Its links are carries
  # whose real file lives elsewhere, so only regular files move; a native link to
  # the file being moved (or a dangling one) gives way to it. 1 while any file
  # could not move, so the caller keeps the profile rather than lose it.
  local from="$1" to="$HOME/.claude/projects" f dest
  # A carry can sit on a parent dir, not only on a file (#466): a sidecar dir
  # linked into native history leaves every file under it resolving through the
  # link to itself, so the move below would see itself as a collision and keep
  # the profile on every retry. The dir link gives way to a real dir first —
  # find visits a parent before its contents, so the shallowest falls first —
  # and the files then move in, leaving no link into the profile behind.
  command find "$from" -mindepth 1 -type d -print0 2>/dev/null | while IFS= read -r -d '' f; do
    dest="$to/${f#$from/}"
    [[ -L "$dest" ]] && { [[ "$dest" -ef "$f" || ! -e "$dest" ]] &&
      { command rm -f "$dest" && command mkdir -p "$dest"; }; }
  done
  command find "$from" -type f -print0 2>/dev/null | while IFS= read -r -d '' f; do
    dest="$to/${f#$from/}"
    [[ -L "$dest" ]] && { [[ "$dest" -ef "$f" || ! -e "$dest" ]] && command rm -f "$dest"; }
    [[ -e "$dest" || -L "$dest" ]] && continue
    command mkdir -p "${dest:h}" && command mv "$f" "$dest"
  done
  [[ -z "$(command find "$from" -type f -print -quit 2>/dev/null)" ]]
}

_ccm_remove_profile_dir() {
  local name="$1"
  local profile_path="$_CCMORPH_PROFILE_DIRS/$1"

  _ccm_valid "$name" || return 1

  # The profile's own session history outlives it: it moves into native
  # ~/.claude/projects, where any profile still resumes it by id.
  if [[ -d "$profile_path/projects" && ! -L "$profile_path/projects" && ! -L "$profile_path" ]]; then
    _ccm_keep_history "$profile_path/projects" || {
      _ccm_err "kept '$name': a session in its history collides with native history"
      return 1
    }
  fi

  if [[ -e "$profile_path" || -L "$profile_path" ]]; then
    command rm -rf "$profile_path" || return 1
  fi

  # Provider profiles store OAuth state in a separate auth directory. Resolve it
  # through the guarded accessor so a symlinked auth path is refused, never
  # followed by rm -rf (which would delete only the link and orphan the creds).
  local _auth_dir
  if _ccm_provider_auth_dir "$name"; then
    _auth_dir="$REPLY"
    [[ -d "$_auth_dir" ]] && command rm -rf "$_auth_dir"
  fi

  [[ ! -e "$profile_path" && ! -L "$profile_path" ]]
}

# Write a profile conf, then load+validate+setup it, rolling everything back on
# failure. The profile-type branches hand their completed config here so save,
# validation, setup, rollback, and secret scrubbing stay one operation. Args:
# <name> <conf-text> <secret-var-name>. The
# secret var (a `local` in the caller — dynamic scope lets us see and scrub it)
# is unset on every path so a plaintext key never lingers. Returns non-zero on
# any failure with the caller-facing error already printed.
_ccm_commit_profile() {
  local _cp_name="$1" _cp_conf="$2" _cp_secret="$3"
  local dir="$_CCMORPH_DIR"
  if ! _ccm_write "$dir/$_cp_name.conf" "$_cp_conf"; then
    _ccm_err "could not save profile '$_cp_name'"
    unset "$_cp_secret"
    return 1
  fi
  if ! _ccm_read "$_cp_name" ||
     ! _ccm_validate_loaded "$_cp_name" ||
     ! _ccm_setup_dir "$_cp_name"; then
    unset _ccm "$_cp_secret"
    _ccm_remove_profile_dir "$_cp_name"
    _ccm_remove_file "$dir/$_cp_name.conf"
    return 1
  fi
  unset _ccm "$_cp_secret"
}
