# ccmorph-context.zsh — Context window and settings: auto-compact trigger maths, window hazards and advisories, settings layering and the projection into CLAUDE_CONFIG_DIR.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

# The two engine knobs that move the compact trigger, read once from the launch env
# and parsed the way the engine parses them. Both the hazard set and the unsafe-model
# warning need this pair, and it was open-coded in both places -- the duplicated
# implementation that produced five defects in this change alone.
#
# Takes three variable NAMES and assigns them: pct raw (its consumer mirrors
# parseFloat, which takes a numeric prefix), maxout already through _ccm_engine_int
# (a positive integer or nothing), and the 1M kill switch normalised to 1 or empty.
_ccm_trigger_env() {
  local _pct="" _out="" _1m="" _e=""
  for _e in "${penv[@]}"; do
    case "$_e" in
      CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=*) _pct="${_e#*=}" ;;
      CLAUDE_CODE_MAX_OUTPUT_TOKENS=*)   _out=$(_ccm_engine_int "${_e#*=}") ;;
      CLAUDE_CODE_DISABLE_1M_CONTEXT=*)  _1m="${_e#*=}" ;;
    esac
  done
  # xe() is `if (Wn()) return false; return /\[1m\]/i.test(e)`, so with the kill
  # switch set a [1m] id resolves at 200,000, not 1,000,000. The switch is read with
  # the engine's own env boolean (ECMAScript trim, then 1/true/yes/on), which lives in
  # the core: `off`, `no`, `false` and `2` are all false.
  [[ -n "$_1m" ]] && _ccm_util engine-bool "$_1m" && _1m=1 || _1m=""
  # Assigned BY NAME into the caller's variables, not printed as delimited fields: an
  # env value may legitimately contain any byte, and a conf `env.` line carrying a tab
  # survives parsing intact -- which split a tab-separated return into the wrong
  # fields, handing the percentage parser "8" and the reserve reader "0<TAB>9000".
  # With no delimiter there is nothing to corrupt.
  : ${(P)1::=$_pct}
  : ${(P)2::=$_out}
  : ${(P)3::=$_1m}
}




_ccm_launch_diagnostics() {
  # Answer every read-only question a launch asks of the merged settings layers
  # and the profile's own files, in ONE interpreter. Five interpreter starts in
  # series answered them before (#332), and those starts are most of a launch's
  # wall time on a loaded host (#321).
  #
  # Records are NUL-framed on stdout, each `<kind>\t<text>`, so a field may hold
  # any byte but NUL -- and the framing is NUL only: a payload's own shape is its
  # kind's business, and `unsafe` is deliberately newline-joined:
  #   pct-set | policy\t<value> | chosen\t<key> | userset\t<value> | disguised\t<id> | unsafe\t<block> | hazard\t<message>
  # `_ccm_invariant_report` prints the two warning kinds; `_ccm_launch` reads the
  # rest into variables. A kind neither consumer names is ignored, so a new one
  # needs its own fixture. No caller tests the status and stderr is dropped, so a
  # diagnostic can never fail a launch.
  #
  # `hazard` is the exception to "read-only answer": it is asked for by name
  # (`_CCM_DIAG_HAZARD=1`, from the launch) and swallows every failure.
  local profile="$1" pct_in_env="$2" pct="$3" maxout="$4" no1m="$5" budget="$6" policy_pct="$7" hdr_key="$8"
  shift 8
  # The invariant half runs only on the two files it reads: an absent or
  # symlinked settings.json, or a missing context conf, is nothing to check.
  local settings="$_CCMORPH_PROFILE_DIRS/$profile/settings.json"
  [[ -f "$settings" && ! -L "$settings" && -f "$_CCMORPH_DIR/$profile.conf" ]] || settings=""
  local -a sources=("${(@f)$(_ccm_settings_sources "$profile")}")
  _CCM_SETTINGS="$settings" \
  _CCM_PROFILE_CONF="$_CCMORPH_DIR/$profile.conf" \
  _CCM_PCT_IN_ENV="$pct_in_env" _CCM_ENV_PCT="$pct" _CCM_MAXOUT="$maxout" \
  _CCM_NO1M="$no1m" _CCM_BUDGET="$budget" _CCM_POLICY="$_CCMORPH_CONTEXT_POLICY" \
  _CCM_POLICY_PCT="$policy_pct" _CCM_PROFILE="$profile" _CCM_HDR_KEY="$hdr_key" \
  _CCM_HAZARD="${_CCM_DIAG_HAZARD:-}" _CCM_CEILING="${_CCM_DIAG_CEILING:-}" \
  _CCM_WITHHELD="${CCMORPH_WITHHOLD_FATAL:-0}" \
    _ccm_util launch-diagnostics "$@" -- "${sources[@]}" 2>/dev/null
}

_ccm_invariant_report() {
  # Print the warnings one `_ccm_launch_diagnostics` run produced, in the order it
  # produced them. Every other record kind is an answer `_ccm_launch` reads into a
  # variable, and a record this loop does not name is ignored.
  local profile="$1" record
  shift
  for record in "$@"; do
    case "$record" in
      disguised$'\t'*)
        _ccm_err \
          "warning: '$profile' is on a disguised model id, which caps the window at 200,000 — select ${record#*$'\t'} for the full window"
        ;;
      unsafe$'\t'*)
        local line
        while IFS= read -r line; do
          [[ -n "$line" ]] && _ccm_err \
            "warning: '$profile' offers a model that will refuse before it compacts — $line"
        done <<< "${record#*$'\t'}"
        ;;
    esac
  done
  return 0
}

_ccm_invariant_warn() {
  # Name any model the profile offers that will refuse before it compacts.
  #
  # Claude Code compacts at roughly 0.85 of (window - 20k) and cannot see a routed
  # model's real ceiling, so a model holding less than that trigger dies mid-session
  # on "input exceeds the context window" with no warning and nothing to be done. The
  # rule has been checked by hand arithmetic in docs/ARCHITECTURE.md section 5 until now,
  # which is exactly the kind of check that stops happening: widening one allowlist
  # silently reintroduces the hazard.
  #
  # A warning, never a refusal. The ceiling data is best-effort and a stale registry
  # must not be able to stop a launch.
  #
  # No launch calls this any more: it runs the same two halves itself, at a point
  # where it can also read the layer answers out of the one interpreter (#332).
  # Kept as the seam the invariant fixtures call, which is why the arguments are
  # the launch env's own readings -- pct, max-output, the 1M kill switch and the
  # session budget -- rather than a penv. The trailing in-env reading (default 1)
  # says whether the launch env holds the percentage key at all: a fixture that
  # passes a value models an env carrying the key, and 0 models the layer-only
  # launch, where the env injects nothing by design and the check must read the
  # percentage out of the settings layers the way the engine does (#428).
  _ccm_invariant_report "$1" "${(@0)$(_ccm_launch_diagnostics "$1" "${6:-1}" "${2:-}" "${3:-}" "${4:-}" "${5:-}" "" "")}"
  return 0
}


_ccm_settings_sources() {
  # The user's own settings layers -- canon first, profile on top, last writer
  # wins -- in the order settings-compose merges them. NOT the generated file:
  # that also carries ccmorph's saved defaults and derived budget, which readers
  # asking "what did the user choose" must not see. One definition, because
  # four hand-rolled copies of this rule is what produced three separate defects --
  # each place that still read the profile alone reported a different wrong answer
  # (an unresolved tier alias and therefore no budget at all; a route ceiling that
  # clamped the largest model; a hazard set that disagreed with the withhold).
  _ccm_canonical_settings
  [[ -e "$_CCMORPH_PROFILE_DIRS/$1/settings.policy.json" ]] &&
    print -r -- "$_CCMORPH_PROFILE_DIRS/$1/settings.policy.json"
  _ccm_profile_layer "$1"
}

_ccm_canonical_settings() {
  print -r -- "$HOME/.claude/settings.json"
}

# The profile's own settings layer. settings.json is what Claude Code reads, and
# settings-compose generates it from canonical settings plus this layer. Until
# the first projection runs, the old settings.json still IS the layer, so readers
# and writers use it and the projection adopts it with their changes intact.
_ccm_profile_layer() {
  local dir="$_CCMORPH_PROFILE_DIRS/$1"
  if [[ -e "$dir/settings.profile.json" || -L "$dir/settings.profile.json" ||
        -e "$dir/cache/settings.base.json" ]]; then
    print -r -- "$dir/settings.profile.json"
  else
    print -r -- "$dir/settings.json"
  fi
}

_ccm_effective_acw() {
  # The auto-compact budget the ENGINE will use (`util effective-acw`): env
  # CLAUDE_CODE_AUTO_COMPACT_WINDOW clamped, else a saved autoCompactWindow
  # (schema-checked, $3 when the launch already read it), else the derived $2.
  local profile="$1" derived="${2:-}" env="" saved="" _e
  for _e in "${penv[@]}"; do
    [[ "$_e" == CLAUDE_CODE_AUTO_COMPACT_WINDOW=* ]] && env="${_e#*=}"
  done
  if (( $# >= 3 )); then
    saved="$3"
  else
    local -a _lw=("${(@ps:\t:)$(_ccm_launch_window "$profile" "")}")
    saved="${_lw[6]:-}"
  fi
  _ccm_util effective-acw "$env" "$saved" "$derived"
}

_ccm_engine_int() {
  # The integer the ENGINE will read out of an env var, or empty when it rejects the
  # value. Mirrors Jd/yN in 2.1.247:
  #
  #   P(v)  -- for a string of at most 32 chars: an integer-valued exponent form
  #            (1e5, 2E4) yields that integer; grouped digits (100_000, "100 000",
  #            100,000) yield the digits with separators stripped.
  #   else  -- parseInt semantics: the leading integer prefix, so 100000junk is
  #            100000 and junk100000 is nothing.
  #   then  -- a value <= 0 is INVALID, and the caller falls through as the engine
  #            does; an upper bound is applied by the caller, not here.
  #
  # One implementation because two hand-rolled ones diverged: a pure-digit zsh glob
  # rejected engine-valid pins such as 1e5 and 100_000, then computed the trigger from
  # a different budget entirely. A separate parseFloat-prefix regex in the reserve
  # reader was a second copy of the same rule, free to drift the other way.
  _ccm_util engine-int "${1:-}" 2>/dev/null
}

_ccm_clamp_acw() {  # clamp the named variable in place to the engine's [1e5, 1e6]
  local _v
  _v=$(_ccm_util clamp-acw "${(P)1}") && : ${(P)1::=$_v}
}



_ccm_hazard_set() {
  # Positional array indexing and "${(f)...}" splitting below assume zsh's
  # native 1-based arrays; KSH_ARRAYS in a caller would silently shift every
  # index. localoptions keeps the change inside this function.
  setopt localoptions noksharrays
  # The models this session could not compact, as `<wall>\t<id>` lines, largest
  # wall first. ONE implementation, because two copies of this arithmetic drifted:
  # the reserve deduction below was missing from both, so every trigger was
  # overstated by a constant 16,000 and a model that Claude Code would in fact
  # have compacted got reported (or withheld) as fatal.
  #
  # The trigger is Claude Code's own, not an approximation of it. From 2.1.247:
  #
  #   window  = min(ceiling, budget)          whichever levers are set
  #   usable  = window - min(window, 20000)   RL(): the output reserve
  #   trigger = min(usable - round(usable*0.2),
  #                 min(usable*pct/100, usable - 13000))    KRt()/xWe()
  #
  # Printing the SET rather than a message keeps the policy (warn vs withhold) in
  # the callers, where it belongs, and keeps this function testable on its own.
  # `settings` may be a single file (the generated settings.json, already merged)
  # or empty, in which case the profile's own view is reconstructed the way the
  # projection builds it: canon settings first, profile on top. Reading only the
  # profile file made the advisory and the withhold act on DIFFERENT model sets --
  # a route whose availableModels live in canon produced no advisory while the
  # withhold still removed models, breaking this code's own "named, never silently
  # dropped" invariant.
  local profile="$1" budget="${2:-}" ceiling="${3:-}"
  local -a sources=()
  if [[ -n "${4:-}" ]]; then
    sources=("$4")                       # generated settings: already merged
  else
    sources=("${(f)$(_ccm_settings_sources "$profile")}")
  fi
  local _s _any=0
  for _s in "${sources[@]}"; do
    [[ -f "$_s" ]] && _any=1
  done
  (( _any )) || return 0
  local context_conf="$_CCMORPH_DIR/$profile.conf"
  [[ -f "$context_conf" ]] || return 0

  local pct="" _maxout="" _no1m=""
  _ccm_trigger_env pct _maxout _no1m
  # Whether the launch env carries the percentage at all -- the same presence test the
  # launch used to decide the injection (#428). Empty means "not carried", the shape
  # _ccm_trigger_env's own outputs have: a literal 0 would reach the core below as a
  # truthy STRING and silence the fallback it is there to allow. The core reads the
  # layers when this is empty, because that is then the engine's own value.
  local pct_in_env=""
  (( ${penv[(I)CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=*]} )) && pct_in_env=1

  _CCM_SOURCES="${(pj:\n:)sources}" _CCM_PROFILE_CONF="$context_conf" _CCM_BUDGET="$budget" \
  _CCM_CEILING="$ceiling" _CCM_PCT="$pct" _CCM_PROFILE="$profile" \
  _CCM_MAXOUT="$_maxout" _CCM_NO1M="$_no1m" _CCM_PCT_IN_ENV="$pct_in_env" \
    _ccm_util hazard-set
}

_ccm_switch_hazard_warn() {
  # Report the models a mid-session /model switch cannot compact safely.
  #
  # The compaction trigger is fixed at launch. A switch DOWN to a model whose real
  # wall sits under it runs past that wall and meets the gateway's over-limit error
  # instead of compacting -- and by then the transcript is too large for /compact
  # to summarize either, so the session is dead, not merely degraded. Nothing
  # inside the session can fix it: no hook fires on /model (all 31 hook events
  # checked; ConfigChange fires on settings writes, not model picks), and the
  # engine re-reads no settings mid-session -- only the user's own /autocompact
  # moves the budget in process.
  #
  # Naming is the default response, not hiding. Claude ids are absent from the set
  # by construction (the engine sizes them from the id, so min() narrows them on
  # every switch), so this only ever concerns routed models. The exact token count
  # IS the protection: below the trigger every model is safe, and the person
  # holding the session is the one who knows whether it will grow past that.
  # CCMORPH_WITHHOLD_FATAL=1 removes them instead.
  local profile="$1" budget="${2:-}" ceiling="${3:-}" settings="${4:-}"
  local hazards
  hazards=$(_ccm_hazard_set "$profile" "$budget" "$ceiling" "$settings") || return 0
  [[ -n "$hazards" ]] || return 0

  # The wording lives with the row arithmetic, in `_ccm_hazard_msg_py`: a launch
  # that reached the advisory through its own diagnostics interpreter renders the
  # same text there, and a second copy here is how the two would drift. `hazards`
  # is already the shape `hazard_message` takes -- trigger first, one row per line
  # -- and it is never only a trigger, so no split-and-rejoin is owed here.
  local msg
  msg=$(_CCM_ROWS="$hazards" _CCM_WITHHELD="${CCMORPH_WITHHOLD_FATAL:-0}" \
        _ccm_util hazard-msg 2>/dev/null) || return 0

  [[ -n "$msg" ]] || return 0
  _ccm_advise "$msg"
  return 0
}

_ccm_withhold_fatal_switches() {
  # Positional array indexing and "${(f)...}" splitting below assume zsh's
  # native 1-based arrays; KSH_ARRAYS in a caller would silently shift every
  # index. localoptions keeps the change inside this function.
  setopt localoptions noksharrays
  # Remove from the picker the models this session could not compact.
  #
  # The opt-in companion to `_ccm_switch_hazard_warn` (CCMORPH_WITHHOLD_FATAL=1):
  # that names them, this makes the choice unavailable. Opt-in because hiding a
  # model the user selected on purpose costs them a reviewer they wanted, and the
  # alternative -- one budget small enough for the smallest model -- taxes every
  # large model in the same session.
  #
  # Writes the generated settings.json, never the profile layer: the next
  # projection renders availableModels from the layer again. The hazard set is
  # computed FROM the generated file, so a saved or env-pinned autoCompactWindow
  # that outranks the derived budget is accounted for rather than guessed at.
  local profile="$1" budget="${2:-}" ceiling="${3:-}" settings="$4"
  [[ -f "$settings" ]] || return 0

  local hazards
  hazards=$(_ccm_hazard_set "$profile" "$budget" "$ceiling" "$settings") || return 0
  [[ -n "$hazards" ]] || return 0

  local -a rows=("${(f)hazards}")
  shift rows                       # drop the trigger line; only the names matter
  (( ${#rows} )) || return 0

  _CCM_SETTINGS="$settings" _CCM_ROWS="${(pj:\n:)rows}" _CCM_LAUNCHED="${_pmodel:-}" \
    _ccm_util withhold-fatal 2>/dev/null || return 0
  return 0
}

_ccm_advise() {
  # Queue one launch advisory for the user to actually READ.
  #
  # Anything printed before exec is painted over: Claude Code takes several
  # seconds to start and then draws its own alternate screen, so a stderr line
  # is visible for a fraction of a second and warns nobody -- the same mistake
  # as putting a real number on a status line the engine ignores. A hook's
  # `systemMessage` renders INSIDE the TUI and stays on screen, which is the
  # only durable channel a launcher has. Claude Code's own unknown-model warning
  # uses the same one.
  #
  # Queued rather than emitted, because advisories are computed at different
  # points of the launch and one hook carries them all.
  _ccm_advisories+=("$1")
}

_ccm_context_compaction_percent() {
  [[ -e "$_CCMORPH_CONTEXT_POLICY" ]] || {
    print 75
    return 0
  }
  _ccm_util policy-pct "$_CCMORPH_CONTEXT_POLICY" 2>/dev/null || {
    _ccm_err "invalid fleet context policy: $_CCMORPH_CONTEXT_POLICY"
    return 1
  }
}

_ccm_effective_compaction_percent() {
  local profile="$1" conf_value="${2:-}" saved=""
  saved=$(_ccm_util effective-pct "$conf_value" "${(f)$(_ccm_settings_sources "$profile")}" 2>/dev/null) || saved=""
  if [[ -n "$saved" ]]; then
    print -r -- "$saved"
  else
    _ccm_context_compaction_percent
  fi
}

# Fill `_ccm_sc_opts` and `_ccm_sc_strip` from a launch's option list:
# settings-compose options up to `--`, then the launch environment, whose
# `KEY=value` entries each ask for a --strip so that key stays out of the
# generated env block. Named for this launcher's use of them, not `reply`: a
# helper that writes a bare `reply` overwrites its caller's local under zsh's
# dynamic scope, and that name is this repo's own two-value return.
_ccm_projection_args() {
  _ccm_sc_opts=() _ccm_sc_strip=()
  while (( $# )) && [[ "$1" != -- ]]; do _ccm_sc_opts+=("$1"); shift; done
  (( $# )) && shift
  local item
  for item in "$@"; do
    [[ "$item" == *=* ]] && _ccm_sc_strip+=(--strip "${item%%=*}")
  done
}

# Fill `_ccm_sc_argv` with one settings-compose invocation's argv -- verb and
# profile first, then the shared options. The executable path is left off: the
# store hands this array to settings-compose's own main() as its argv, with
# sys.argv[0] set apart, so a path in here would arrive as a second verb.
_ccm_settings_argv() {
  local verb="$1" profile="$2"
  shift 2
  _ccm_sc_argv=("$verb" "$_CCMORPH_PROFILE_DIRS/$profile" \
         --canonical "$(_ccm_canonical_settings)" \
         "${(@)${_CCMORPH_MODEL_MAPPING_ENV[@]}/#/--mapping=}" "$@")
}

# Generate the profile's settings.json -- canonical settings, the profile's own
# layer, ccmorph's saved model defaults -- so every Claude Code entry point reads
# the same configuration from CLAUDE_CONFIG_DIR. Every key this launch passes in
# the environment is left out of the file's env block, because Claude Code lets
# that block override the process environment. See settings-compose.
_ccm_settings_projection() {
  local verb="$1" profile="$2"
  shift 2
  _ccm_settings_argv "$verb" "$profile" "$@"
  "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" settings-compose "${_ccm_sc_argv[@]}"
}

# Carry edits Claude Code made to the generated settings.json into the layer. The
# projection saves settings.json's exact bytes as cache/settings.base.json (its
# BASE), so an unedited file is recognised by content -- no process, and no
# timestamp a restore or copy can fake. Anything else, including a missing saved
# copy, runs the fold.
_ccm_fold_settings() {
  local dir="$_CCMORPH_PROFILE_DIRS/$1"
  [[ -f "$dir/settings.json" ]] || return 0
  [[ -f "$dir/cache/settings.base.json" &&
     "$(<"$dir/settings.json")" == "$(<"$dir/cache/settings.base.json")" ]] && return 0
  _ccm_settings_projection fold "$1" 2>/dev/null
}

# $1 profile, $2 saved model defaults (JSON), then materialize options, `--`, and
# the launch environment, whose keys stay out of the generated env block.
_ccm_project_settings() {
  local profile="$1" saved="$2"
  shift 2
  _ccm_projection_args "$@"
  local rc
  _ccm_settings_projection materialize "$profile" --saved "$saved" \
    "${_ccm_sc_opts[@]}" "${_ccm_sc_strip[@]}" || {
    rc=$?
    _ccm_err "could not generate settings for '$profile'"
    return $rc
  }
}

# $1 profile, then materialize options, `--`, and the launch environment -- the same
# arguments _ccm_project_settings takes, minus the saved defaults it would have to be
# handed. Reading the conf and generating the settings are two halves of one job on
# one profile, so a launch pays for one process instead of two: the store reads
# the conf and then runs settings-compose in the same process.
_ccm_prepare_and_project() {
  local profile="$1"
  shift
  # The two helpers below shift their own positional parameters, never this
  # function's, so `$@` still holds this launch's options, `--`, and its
  # environment for the fallback run.
  _ccm_projection_args "$@"
  _ccm_settings_argv materialize "$profile" "${_ccm_sc_opts[@]}" "${_ccm_sc_strip[@]}"
  local -a sc_argv=("${_ccm_sc_argv[@]}")
  # The argv travels in the environment one element per line, so an element that
  # carries a newline cannot be expressed -- a profile path or an option could hold
  # one. Then the two runs, which take their arguments as arguments.
  local item
  for item in "${sc_argv[@]}"; do
    if [[ "$item" == *$'\n'* ]]; then
      local saved
      # Emptied, not just unset: these two are what put the store in materialize
      # mode and would otherwise follow this fallback into its own store run.
      saved=$(_CCM_MATERIALIZE= _ccm_config_store prepare-launch "$profile") || return 10
      _ccm_project_settings "$profile" "$saved" "$@"
      return $?
    fi
  done
  local rc
  _CCM_MATERIALIZE="${(pj:\n:)sc_argv}" _ccm_config_store prepare-launch "$profile" >/dev/null
  rc=$?
  # settings-compose's own statuses, shifted past the store's: the launch handles
  # each of them as it does when they come from its own process.
  case $rc in
    0) return 0 ;;
    11 | 12 | 13) _ccm_err "could not generate settings for '$profile'"; return $(( rc - 10 )) ;;
    *) return 10 ;;
  esac
}

