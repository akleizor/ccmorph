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

_ccm_doctor_status() {
  emulate -L zsh
  local kind="$1"; shift
  local symbol color
  case "$kind" in
    OK) symbol='✓'; color="$_ccm_accent" ;; FAIL) symbol='✗'; color="$_ccm_red" ;;
    WARN) symbol='!'; color="$_ccm_yellow" ;; INFO) symbol='ℹ'; color="$_ccm_focus" ;; SKIP) symbol='–'; color="$_ccm_gray" ;;
    *) print -r -- "$kind $*"; return ;;
  esac
  if [[ -t 1 && -z "${NO_COLOR:-}" && "${TERM:-}" != dumb ]]; then
    printf '%s%s%s %s\n' "$color" "$symbol" "$_ccm_reset" "$*"
  elif [[ -t 1 ]]; then print -r -- "$symbol $*"; else print -r -- "$kind $*"; fi
}

_ccm_doctor_claude() {
  # The claude row shared by the profile check and the fresh-install report;
  # returns 1 only when it printed a FAIL.
  emulate -L zsh
  local claude_bin
  if whence -p claude >/dev/null 2>&1; then
    claude_bin=$(whence -p claude 2>/dev/null) || claude_bin=''
    if [[ -n "$claude_bin" ]]; then
      _ccm_doctor_status "OK" "claude executable: $claude_bin"
    else
      _ccm_doctor_status 'INFO' 'claude command: provided by shell function/alias (path not inspectable)'
    fi
    return 0
  fi
  _ccm_doctor_status 'FAIL' 'claude executable: not found on PATH'
  return 1
}

_ccm_doctor_environment() {
  # What a fresh install can say before any profile exists. Read-only: it looks
  # at the machine and the state directory and creates nothing.
  emulate -L zsh
  local rc_version tool_path
  local failed=0
  _ccm_doctor_status 'OK' "ccmorph: $_CCMORPH_VERSION ($_CCMORPH_LIB_DIR)"
  _ccm_doctor_status 'OK' "zsh: $ZSH_VERSION"
  # Only the Remote Control add-on still runs on an interpreter, so only a machine
  # that uses it is held to one (3.8 or newer).
  if [[ -e "$_CCMORPH_DIR/rc-hosts" ]]; then
    if rc_version=$(_ccm_util rc-interpreter 2>/dev/null); then
      _ccm_doctor_status 'OK' "remote control interpreter: $rc_version"
    elif [[ -n "$rc_version" ]]; then
      _ccm_doctor_status 'FAIL' "remote control interpreter: $rc_version (need 3.8 or newer)"
      failed=1
    else
      _ccm_doctor_status 'FAIL' 'remote control interpreter: not found (need 3.8 or newer)'
      failed=1
    fi
  fi
  _ccm_doctor_claude || failed=1
  tool_path=$(whence -p fzf 2>/dev/null) || tool_path=''
  if [[ -n "$tool_path" ]]; then _ccm_doctor_status 'INFO' "fzf: $tool_path"
  else _ccm_doctor_status 'INFO' 'fzf: not found (numbered menus)'; fi
  tool_path=$(whence -p cliproxyapi 2>/dev/null) || tool_path=''
  if [[ -n "$tool_path" ]]; then _ccm_doctor_status 'INFO' "cliproxyapi: $tool_path"
  else _ccm_doctor_status 'INFO' 'cliproxyapi: not found (Claude-family models only on gateways)'; fi
  tool_path=$(whence -p ccmorph 2>/dev/null) || tool_path=''
  if [[ -n "$tool_path" ]]; then _ccm_doctor_status 'OK' "PATH: ccmorph is on PATH (${tool_path:h})"
  else _ccm_doctor_status 'INFO' 'PATH: ccmorph is not on PATH (add the directory it was installed to)'; fi
  _ccm_doctor_status 'INFO' "state: $_CCMORPH_DIR (no profiles yet) — next: ccmorph add <name>"
  return $failed
}

_ccm_doctor() {
  emulate -L zsh
  local name="${1:-}" conf base key key_value type model adapter settings profile_dir
  local doctor_failed=0
  if [[ -z "$name" ]]; then
    [[ -f "$_CCMORPH_DIR/active" && ! -L "$_CCMORPH_DIR/active" ]] && name=$(<"$_CCMORPH_DIR/active")
  fi
  if [[ -z "$name" ]]; then
    local -a _doctor_confs=("$_CCMORPH_DIR"/*.conf(N))
    (( ${#_doctor_confs} )) || { _ccm_doctor_environment; return $?; }
    _ccm_doctor_status 'FAIL' 'profile: no profile selected (use: ccmorph doctor <profile>)'
    _ccm_accounts_hint
    return 1
  fi
  _ccm_valid "$name" || { _ccm_doctor_status 'FAIL' 'profile: invalid profile name'; return 1; }
  conf="$_CCMORPH_DIR/$name.conf"
  if [[ ! -f "$conf" || -L "$conf" ]]; then
    _ccm_doctor_status "FAIL" "profile: missing or symlinked config ($conf)"
    return 1
  fi

  # Use the canonical parser without its permission repair. This keeps doctor’s
  # schema and duplicate-key checks aligned with launch while preserving modes.
  _ccm_read "$name" 1 || {
    _ccm_doctor_status 'FAIL' 'config: invalid profile schema (launch would reject it)'
    unset _ccm
    return 1
  }
  type="${_ccm[type]:-}"; base="${_ccm[base_url]:-}"; model="${_ccm[model]:-}"
  adapter="${_ccm[adapter]:-}"
  if [[ -n "${_ccm[api_key]:-}" || -n "${_ccm[token]:-}" ]]; then
    key=present; key_value="${_ccm[api_key]:-${_ccm[token]:-}}"
  fi
  unset _ccm
  [[ -n "$type" ]] || { _ccm_doctor_status 'FAIL' 'config: missing type'; return 1; }
  case "$type" in
    apikey|sub|token|provider) ;;
    *) _ccm_doctor_status "FAIL" "config: unsupported profile type '$type'"; doctor_failed=1 ;;
  esac
  if [[ "$type" == apikey && -z "$key" ]]; then
    _ccm_doctor_status 'FAIL' 'config: apikey profile has no credential (value not displayed)'
    doctor_failed=1
  fi
  _ccm_doctor_status "OK" "profile: $name ($type)"
  _ccm_doctor_status "OK" "ccmorph executable: ${_CCMORPH_LIB_DIR}/ccmorph.sh"
  _ccm_doctor_claude || doctor_failed=1
  profile_dir="$_CCMORPH_PROFILE_DIRS/$name"
  if [[ -d "$profile_dir" ]]; then
    _ccm_doctor_status "OK" "profile home: $profile_dir"
  else
    _ccm_doctor_status "FAIL" "profile home: missing ($profile_dir)"
    doctor_failed=1
  fi
  settings=$(_ccm_profile_layer "$name")
  if [[ -f "$settings" ]]; then
    if _ccm_util settings-valid "$HOME/.claude/settings.json" "$settings" >/dev/null 2>&1
    then
      _ccm_doctor_status "OK" "profile settings: valid layered JSON ($settings overrides canonical settings)"
      local effective_model
      effective_model=$(_ccm_account_model "$name") || effective_model=''
      if [[ -n "$effective_model" ]]; then
        _ccm_doctor_status 'INFO' "effective model: $effective_model (profile default)"
      else
        _ccm_doctor_status 'INFO' 'effective model: Claude Code default (no saved model)'
      fi
    else
      _ccm_doctor_status 'FAIL' "profile settings: invalid JSON or non-object ($settings)"
      doctor_failed=1
    fi
  else
    _ccm_doctor_status 'SKIP' "profile settings: missing ($settings)"
  fi
  [[ -n "$key" ]] && _ccm_doctor_status 'OK' 'credentials: present (value hidden)' || _ccm_doctor_status 'SKIP' 'credentials: none stored'

  if [[ -n "$base" ]]; then
    local invalid_url=0
    if [[ "$base" != (http|https)://* || "$base" == *'?'* || "$base" == *'#'* ]]; then
      _ccm_doctor_status 'FAIL' 'config: base_url must be an http(s) URL without query or fragment'
      doctor_failed=1; invalid_url=1
    fi
    local safe_url http_code payload models_count
    safe_url=$(_ccm_redact_url "$base")
    print -r -- "endpoint: $safe_url"
    if (( invalid_url )); then
      _ccm_doctor_status 'SKIP' 'gateway: invalid endpoint (no request sent)'
    elif [[ -z "$key" ]]; then
      _ccm_doctor_status 'SKIP' 'gateway: no API credential for metadata request'
    else
      # Probe the same metadata route a launch uses, through the same ambient
      # HTTPS_PROXY a launch would inherit — doctor must answer for the route the
      # user actually has, not for a direct one they may not.
      local _doctor_url
      _ccm_api_url "$base" "/v1/models?limit=1000"
      _doctor_url="$REPLY"
      payload=$(_ccm_curl_keyed both "$key_value" -sS --connect-timeout 2 --max-time 5 \
        -w $'\\n__CCMORPH_STATUS__%{http_code}' \
        "$_doctor_url" 2>/dev/null) || payload="__CCMORPH_STATUS__000"
      http_code="${payload##*__CCMORPH_STATUS__}"; payload="${payload%%__CCMORPH_STATUS__*}"; payload="${payload%$'\\n'}"
      if [[ "$http_code" == 2?? ]]; then
        models_count=$(print -r -- "$payload" | _ccm_util models-count 2>/dev/null) || models_count=''
        if [[ -n "$models_count" ]]; then
          _ccm_doctor_status "OK" "gateway: reachable (HTTP $http_code); models: $models_count"
        else
          _ccm_doctor_status "FAIL" "gateway: malformed model listing (HTTP $http_code)"
          doctor_failed=1
        fi
      else
        if [[ "$http_code" == 000 ]]; then
          _ccm_doctor_status 'SKIP' 'gateway: endpoint did not answer (no changes made)'
        else
          _ccm_doctor_status "FAIL" "gateway: unreachable or rejected (HTTP $http_code)"
          doctor_failed=1
        fi
      fi
    fi
  else
    _ccm_doctor_status 'SKIP' 'gateway: profile has no base_url (native endpoint)'
  fi
  if [[ "$adapter" == true ]]; then
    _ccm_doctor_status 'INFO' 'adapter: configured=true (not started by read-only doctor)'
  elif [[ -n "$base" ]]; then
    _ccm_doctor_status 'INFO' 'adapter: automatic compatibility may apply at launch (not started by doctor)'
  else
    _ccm_doctor_status 'SKIP' 'adapter: native profile path (not started by doctor)'
  fi
  if [[ -n "$base" && -x "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" ]]; then
    local running_adapter
    running_adapter=$(_ccm_util running-adapter "$name" 2>/dev/null) || running_adapter=''
    if [[ "$running_adapter" == __UNKNOWN__ ]]; then
      _ccm_doctor_status 'SKIP' 'adapter: status unknown (process inspection unavailable; not started by doctor)'
    elif [[ -n "$running_adapter" ]]; then
      local adapter_pid="${running_adapter%%$'\t'*}" adapter_children="${running_adapter#*$'\t'}"
      local child_spec child_pid child_name listener what
      # running-adapter names every listener holder: spawned children
      # (cliproxyapi, a CCMORPH_SHIM_HOOKS build), and the adapter
      # process itself when the shim is served in-process — the stock design,
      # and the port a live session's base_url points at.
      for child_spec in ${(s:,:)adapter_children}; do
        child_pid="${child_spec%%:*}"; child_name="${child_spec#*:}"
        listener=$(_ccm_util listener-of "$child_pid" 2>/dev/null) || listener='__UNKNOWN__'
        what="$child_name child"
        if [[ "$child_pid" == "$adapter_pid" ]]; then
          what="in-process shim"
        fi
        if [[ "$listener" == __UNKNOWN__ ]]; then
          _ccm_doctor_status "SKIP" "adapter: $what listener status unknown (pid $child_pid)"
        elif [[ -n "$listener" ]]; then
          _ccm_doctor_status "OK" "adapter: $what listener $listener (pid $child_pid; read-only check)"
        else
          _ccm_doctor_status "WARN" "adapter: $what running without TCP listener (pid $child_pid)"
        fi
      done
    else
      _ccm_doctor_status 'INFO' 'adapter: not running (not started by read-only doctor)'
    fi
  fi
  # Do not classify arbitrary cache names as stale: ownership is only established
  # by ccmorph's provisioning manifest, and doctor never provisions or repairs.
  _ccm_doctor_status 'SKIP' 'generated state: ownership/conflict not established (no changes made)'
  return $doctor_failed
}

# Short overview first; details are available without loading profiles or running
# setup. Keep Claude's own `run ... --help` pass-through unchanged.
# Help follows gh: only section headings carry weight; rows stay plain so the
# eye lands on structure first, then scans a calm two-column table.
# One two-column grid for help and inspect: _ccm_grid WIDTH KEY TEXT. A key too
# wide for its column gets its own line, the text starting under the column.
_ccm_grid() {
  if (( ${#2} > $1 )); then
    printf '  %s\n  %-*s %s\n' "$2" "$1" '' "$3"
  else
    printf '  %-*s %s\n' "$1" "$2" "$3"
  fi
}
_ccm_help() { _ccm_grid 32 "$@"; }

_ccm_usage() {
  local topic="${1:-}" usage="" detail=""
  case "$topic" in
    '')
      print 'ccmorph — Claude Code profile switcher'
      print '\nUsage: ccmorph <command> [arguments]'
      print "\n${_ccm_bold}Launch:${_ccm_reset}"
      _ccm_help "ccmorph run" "Launch the default profile"
      _ccm_help "ccmorph run <profile>" "Use a profile for this session"
      _ccm_help "ccmorph run -i" "Choose a profile interactively"
      print "\n${_ccm_bold}Profiles:${_ccm_reset}"
      _ccm_help "ccmorph add <profile>" "Add an account or API key"
      _ccm_help "ccmorph list" "See profiles and the default"
      _ccm_help "ccmorph quota [profile]" "Plan usage and spend of every profile"
      _ccm_help "ccmorph use <profile>" "Set the default; do not launch"
      _ccm_help "ccmorph config [profile]" "Change model, reasoning, or connection"
      print "\n${_ccm_bold}Troubleshooting:${_ccm_reset}"
      _ccm_help "ccmorph inspect [profile]" "Show effective settings; no secrets"
      _ccm_help "ccmorph doctor [profile]" "Check why a profile is not working"
      print '\nStart here: ccmorph add work'
      print 'More commands: ccmorph help all'
      print 'Command details: ccmorph help <command>'
      print 'Version: ccmorph --version'
      return 0
      ;;
    all)
      _ccm_usage
      print "\n${_ccm_bold}More commands:${_ccm_reset}"
      _ccm_help "ccmorph models <profile>" "List models from an API-key endpoint"
      _ccm_help "ccmorph get <profile> <model|effort>" "Read a saved default"
      _ccm_help "ccmorph set <profile> <model|effort> [value]" "Save a default; omit value to choose"
      _ccm_help "ccmorph login <profile>" "Sign in again to a provider account"
      _ccm_help "ccmorph mv <old> <new>" "Rename a profile"
      _ccm_help "ccmorph rm <profile>" "Delete a profile and its private sign-in state"
      _ccm_help "ccmorph hop <profile>" "Switch a running session; use inside Claude"
      _ccm_help "ccmorph cred <profile>" "Print a stored secret (sensitive)"
      _ccm_help "ccmorph update [ref]" "Update to the latest version, or to a branch or tag"
      _ccm_help "ccmorph uninstall" "Remove the command; profiles stay"
      print "\n${_ccm_bold}Advanced troubleshooting (normally automatic):${_ccm_reset}"
      _ccm_help "ccmorph probe [profile]" "Refresh context-limit metadata; no inference"
      _ccm_help "ccmorph context <profile> <model> [tokens|auto]" "Override or reset one model context limit"
      return 0
      ;;
    run)
      usage='ccmorph run [profile] [-i] [-- <claude arguments...>]'
      detail='Omit the profile to use your default. -i asks for a profile if needed,
then a model for routed profiles. Explicit --model <id> skips the model picker.
Choices apply to this session only; saved defaults are unchanged.

Examples:
  ccmorph run work
  ccmorph run work --model sonnet
  ccmorph run work -- --continue

Native Claude help: ccmorph run <profile> --help' ;;
    add)
      usage='ccmorph add <profile>'
      detail='Choose how to sign in. Only the selected method asks for credentials:
  Subscription: sign in to Claude with your browser.
  API key: credential presented to Anthropic or a gateway (apikey).
  OAuth token: paste an existing Claude token.
  Provider: browser login, when a supported provider bridge is installed.

Credentials are hidden while typing. Ctrl-C cancels setup.' ;;
    config)
      usage='ccmorph config [profile]'
      detail='Choose what to change: model and reasoning, or connection/sign-in details.
Only settings relevant to that task and profile are shown.
Keep the current defaults or select new values. Changes save when you finish;
there is no extra confirmation. Esc cancels a picker without saving.
Changes affect new sessions.

For a single setting: ccmorph set <profile> model|effort [value]' ;;
    list) usage='ccmorph list'; detail='List saved profiles. * marks the default used by ccmorph run.' ;;
    quota) usage='ccmorph quota [profile] [--json]'; detail='Every profile in one view, grouped by kind: Claude subscriptions, Codex
subscriptions, budget accounts, and API or gateway profiles. Each plan window is a
bar with the percent used and when it resets; a plan tier comes from the provider'"'"'s
own account data. A rate-limited or unreachable read shows the last good reading
and its age. Gateways and API keys show today'"'"'s and this month'"'"'s billed spend from
their receipts, and the account balance where the provider publishes one (DeepSeek,
Kimi, GLM, OpenRouter, SiliconFlow, StepFun). --json prints the same data for tools. No inference, nothing billed.
Omit the profile to list every profile.' ;;
    use) usage='ccmorph use [profile|-i]'; detail='Set the default without launching. No argument shows the current default.' ;;
    inspect) usage='ccmorph inspect [profile|-i]'; detail='Show effective settings and their sources, never the stored credential.
Omit the profile to inspect the default; -i opens the profile picker.' ;;
    doctor) usage='ccmorph doctor [profile|-i]'; detail='Read-only checks of the executable, endpoint and generated state.
Omit the profile to check the default. Does not repair or launch a session.' ;;
    models) usage='ccmorph models [profile] [--filtered|--filtered-json]'; detail='List models from an API-key endpoint. Omit the profile to choose one.
--filtered applies profile exclusions; --filtered-json returns JSON.' ;;
    get|set)
      usage="ccmorph $topic [profile] [model|effort]"
      [[ "$topic" == set ]] && usage+=' [value]'
      detail='Read or save a default for new sessions. Missing arguments open pickers.
Use explicit arguments in scripts. Values are printed to stdout.
Effort: low, medium, high, xhigh. Available models come from the profile.' ;;
    login) usage='ccmorph login [profile|-i]'; detail='Sign in again to a provider-backed profile using its browser login.
To replace an API key or token instead: ccmorph config <profile>' ;;
    mv) usage='ccmorph mv <old> <new>'; detail='Rename a profile and its private state. Existing destinations are refused.' ;;
    rm) usage='ccmorph rm <profile>'; detail='Delete this profile, its saved credential and private sign-in state.
Shared conversation history is kept. This cannot be undone.
Leftover private state from a missing profile requires confirmation.' ;;
    hop) usage='ccmorph hop <profile>'; detail='Inside a running Claude session, type: ! ccmorph hop <profile>
Then exit Claude to complete the switch. Keeps model and effort when supported.
This command does not work from an unrelated terminal.' ;;
    cred) usage='ccmorph cred [profile]'; detail='Print the stored API key or token to stdout. This reveals a secret.
Only profiles with a stored key or token are offered. Name one in scripts.' ;;
    update) usage='ccmorph update [ref]'; detail='Download the latest release and run its installer, which updates this install
in place. Give a branch or tag to install that instead: ccmorph update v0.1.0.
Profiles, credentials and local.sh are kept, and the installer names what it kept.
A git checkout is refused: update it with git.
Needs the install.sh that sits beside the library; without it, run the one-line
install from the README once.' ;;
    uninstall) usage='ccmorph uninstall'; detail='Remove the ccmorph command from ~/.local/bin. Your profiles and credentials stay
in ~/.config/ccmorph; delete that directory by hand if you want them gone.
Needs the install.sh that sits beside the library.' ;;
    probe) usage='ccmorph probe [profile] [options]'; detail='Refresh published context metadata; no inference requests by default.
Paid inference requires --allow-inference, --profile and --model explicitly.
Full probe options: ccmorph probe --help' ;;
    context) usage='ccmorph context <profile> <model> [tokens|auto]'; detail='Read a context limit, set a known token count, or restore automatic discovery.
Normally unnecessary: compatibility and context limits are automatic.' ;;
    *) _ccm_err "unknown help topic '$topic'. Run: ccmorph help all"; return 2 ;;
  esac
  print -r -- "Usage: $usage"
  print
  print -r -- "$detail"
  print '\nFixed choices: ↑/↓ move, Enter selects one, Esc cancels. No search box.'
  print 'Profile/model catalogues: type to search with fzf; numbers without it.'
}

# `ccmorph update [ref]` and `ccmorph uninstall` hand over to the install.sh that
# the installer placed beside lib/. Everything that matters (what is downloaded,
# what is kept, what is removed) is the installer's own, so the profiles,
# credentials and local.sh guarantees live in one place. $1 = update|uninstall,
# $2 = the ref, for update only (a required argument here, so an empty one is
# refused rather than read as the default).
_ccm_run_installer() {
  emulate -L zsh
  local verb="$1" ref="${2-}" has_ref=$(( $# >= 2 ))
  local root="${_CCMORPH_LIB_DIR:h}" installer="${_CCMORPH_LIB_DIR:h}/install.sh"
  if [[ "$verb" == update ]]; then
    # A ref becomes part of a download URL and an env value, so it is a plain name:
    # a branch or tag, slashes allowed, no `..`, no leading dash, nothing a shell
    # or a URL would read as syntax. No ref at all means the latest release.
    if (( has_ref )) && [[ ! "$ref" =~ '^[A-Za-z0-9][A-Za-z0-9._/-]*$' || "$ref" == *..* ]]; then
      _ccm_err "invalid ref '$ref' — use a branch or tag name such as main or v0.1.0"
      return 2
    fi
    # A checkout is the user's working tree. Copying a download over it would dirty
    # it and fight their branch, so it is updated with the tool that owns it.
    if [[ -e "$root/.git" ]]; then
      _ccm_err "$root is a git checkout; update it with git (git -C ${(q)root} pull), not with ccmorph update"
      return 1
    fi
  fi
  # A Homebrew install lives under the Cellar and has no installer beside it:
  # the package manager that placed it updates and removes it.
  if [[ "${root:A}" == */Cellar/ccmorph/* ]]; then
    _ccm_err "ccmorph was installed with Homebrew; run: brew ${${verb:/update/upgrade}} ccmorph"
    return 1
  fi
  if [[ ! -f "$installer" ]]; then
    _ccm_err "no installer at $installer, so ccmorph cannot $verb itself. Run the one-line install once:"
    print -u2 -r -- '  sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"'
    return 1
  fi
  if [[ "$verb" == update ]] && (( has_ref )); then
    CCMORPH_REF="$ref" command sh "$installer" --remote
  elif [[ "$verb" == update ]]; then
    # The installer's own default, the latest release. An exported CCMORPH_REF is
    # dropped so the bare command means the same thing in every shell.
    ( unset CCMORPH_REF; command sh "$installer" --remote )
  else
    command sh "$installer" --uninstall
  fi
}

ccmorph() {
  emulate -L zsh
  # Shell snapshots can retain this public function without its private library.
  # The executable already owns loading and path resolution; do not duplicate it.
  if (( ! $+functions[_ccm_secure_dirs] )) || [[ -z "${_CCMORPH_DIR:-}" ]]; then
    command ccmorph "$@"
    return $?
  fi

  case "${1:-}" in
    ''|-h|--help|help)
      (( $# <= 2 )) || { _ccm_err 'usage: ccmorph help [command|all]'; return 2; }
      _ccm_usage "${2:-}"
      return $?
      ;;
    -v|-V|--version|version) print "ccmorph ${_CCMORPH_VERSION}"; return 0 ;;
    # The names a launch never inherits from the shell (see _CCMORPH_BACKEND_ENV),
    # one per line, for the Remote Control add-on. A pure constant: answered before
    # any state migration or chmod, so the query never touches the caller's config.
    _backend_env) print -l -- "${_CCMORPH_BACKEND_ENV[@]}"; return 0 ;;
  esac
  # Exact command-help requests only: run/probe retain their native help, and
  # `set profile model --help` remains an invalid value rather than hiding a typo.
  if (( $# == 2 )) && [[ "$2" == (-h|--help) && "$1" != (run|probe|"$_ccm_retired_verb") ]]; then
    _ccm_usage "$1"
    return $?
  fi

  # Before the state is touched: these act on the installed code, not on profiles.
  case "$1" in
    update)
      (( $# <= 2 )) || { _ccm_err 'usage: ccmorph update [ref]'; return 2; }
      _ccm_run_installer update "${@:2}"
      return $?
      ;;
    uninstall)
      (( $# == 1 )) || { _ccm_err 'usage: ccmorph uninstall'; return 2; }
      _ccm_run_installer uninstall
      return $?
      ;;
  esac

  local dir="$_CCMORPH_DIR"
  local active=""

  # Keep diagnostics genuinely read-only: bypass active-file chmod, directory
  # creation, migration, and all launch provisioning.
  if [[ "${1:-}" == doctor ]]; then
    shift
    (( $# <= 1 )) || { _ccm_err 'usage: ccmorph doctor [profile]'; return 2; }
    local _CCMORPH_PROFILE_DIRS="$_CCMORPH_PROFILE_DIRS"
    if [[ ! -e "$_CCMORPH_PROFILE_DIRS" && ! -L "$_CCMORPH_PROFILE_DIRS" &&
          -d "$HOME/.claude-profiles" && ! -L "$HOME/.claude-profiles" ]]; then
      _CCMORPH_PROFILE_DIRS="$HOME/.claude-profiles"
    fi
    if [[ "${1:-}" == (-i|--interactive) ]]; then
      local _doctor_profile
      _doctor_profile=$(_ccm_menu_profile) || return 1
      set -- "$_doctor_profile"
    fi
    _ccm_doctor "$@"
    return $?
  fi

  _ccm_secure_dirs || {
    _ccm_err "cannot secure ccmorph directories"
    return 1
  }

  if _ccm_read_active; then
    active="$REPLY"
  fi

  case "$1" in

    run)
      shift
      case "${1:-}" in
        -a|--profile|--profile=*)
          _ccm_err "launch profile is positional: ccmorph run <profile>"
          return 2
          ;;
      esac
      local _run_has_interactive=0 _CCMORPH_NATIVE_ARGS=1 _CCMORPH_INTERACTIVE=0
      if [[ "${1:-}" == (-i|--interactive) ]]; then
        _run_has_interactive=1
        _CCMORPH_INTERACTIVE=1
        shift
      fi

      local _run_account=""
      if (( $# )) && [[ "$1" != -* ]]; then
        if _ccm_exists "$1"; then
          _run_account="$1"
          shift
          if [[ "${1:-}" == (-i|--interactive) ]]; then
            _CCMORPH_INTERACTIVE=1
            shift
          fi
        elif ! _ccm_is_claude_subcommand "$1"; then
          _ccm_valid "$1" || { _ccm_err "invalid profile name '$1'"; return 2; }
          _ccm_err "no profile '$1'. Run: ccmorph list"
          return 1
        fi
      fi

      if [[ -z "$_run_account" && -z "$active" ]] && (( ! _run_has_interactive )); then
        # A first run has nothing to pick a default from; point at add, not use.
        local -a _run_confs=("$dir"/*.conf(N))
        if (( ! ${#_run_confs} )); then
          _ccm_err "no profiles yet — run: ccmorph add <name>"
          return 1
        fi
        _ccm_err "no default profile set — run: ccmorph use <profile> or ccmorph run -i"
        _ccm_accounts_hint
        return 1
      fi
      # `--` is run's own separator (usage: run [profile] [-i]
      # [-- args]). Forwarded, it makes Claude Code read every later flag as
      # prompt text -- but only the OUTER one is run's. Every `--` after the
      # first argument Claude Code owns is Claude Code's own (see
      # _ccm_run_separator_index), so a native command keeps its delimiter.
      local -a _run_args=("$@")
      local _run_sep=0
      _ccm_run_separator_index "${_run_args[@]}"
      _run_sep=$REPLY
      (( _run_sep > 0 )) && _run_args[_run_sep]=()
      local _run_sub=0
      _ccm_launch_has_claude_subcommand "${_run_args[@]}" && _run_sub=1
      _CCMORPH_LAUNCH_PROFILE="$_run_account" CCMORPH_SUBCOMMAND_LAUNCH=$_run_sub _ccm_launch_driver "${_run_args[@]}"
      ;;

    start)
      _ccm_err "unknown command 'start'. Run: ccmorph run${2:+ ${@:2}}"
      return 1
      ;;

    default)
      _ccm_err "unknown command 'default'. Run: ccmorph use${2:+ ${@:2}}"
      return 1
      ;;

    show)
      _ccm_err "unknown command 'show'. Run: ccmorph inspect${2:+ ${@:2}}"
      return 1
      ;;

    creds|secret)
      _ccm_err "unknown command '$1'. Run: ccmorph cred${2:+ ${@:2}}"
      return 1
      ;;

    list)
      local n f
      local found=0

      for f in "$dir"/*.conf(N); do
        n="${f:t:r}"

        if ! _ccm_read "$n"; then
          print -u2 "ccmorph: skipping invalid profile '$n'"
          continue
        fi

        local marker=" "
        local name_fmt="${n}"

        if [[ "$n" == "$active" ]]; then
          marker="${_ccm_accent}*${_ccm_reset}"
          name_fmt="${_ccm_bold}${n}${_ccm_reset}"
        fi

        local label="${_ccm_dim}${_ccm[type]}${_ccm_reset}"
        # Declared before the branches: `local x; x=$(...)` inside an if/elif chain
        # emits the assignment to stdout in this context, which corrupts the listing.
        local _lm=""

        if [[ "${_ccm[type]}" == sub &&
              -n "${_ccm[email]:-}" ]]; then
          label="${_ccm_cyan}sub${_ccm_reset}${_ccm_dim}: ${_ccm[email]}${_ccm_reset}"
        elif [[ "${_ccm[type]}" == token ]]; then
          label="${_ccm_yellow}token${_ccm_reset}"
        elif [[ "${_ccm[type]}" == apikey ]]; then
          _lm=$(_ccm_account_model "$n") || _lm=""
          label="${_ccm_cyan}apikey${_ccm_reset}${_ccm_dim}: ${_ccm[base_url]:-api.anthropic.com}${_lm:+ → ${_lm}}${_ccm_reset}"
        elif [[ "${_ccm[type]}" == provider ]]; then
          _lm=$(_ccm_account_model "$n") || _lm=""
          _ccm_provider_label "${_ccm[provider]:-provider}"
          label="${_ccm_accent}${REPLY}${_ccm_reset}${_lm:+${_ccm_dim} → ${_lm}${_ccm_reset}}"
        fi

        print " ${marker} ${name_fmt} (${label})"
        found=1
      done

      (( found )) ||
        print "${_ccm_dim}  No profiles. Run: ccmorph add <name>${_ccm_reset}"

      unset _ccm
      ;;

    add)
      if [[ -z "${2:-}" && -t 0 ]]; then
        local _new_name
        ccm_ui_input 'New profile name: ' || return 1
        _new_name="$REPLY"
        set -- add "$_new_name"
      fi
      _ccm_valid "${2:-}" || {
        _ccm_err "invalid name — use alphanumeric/underscore"
        return 1
      }

      _ccm_exists "$2" && {
        _ccm_err "'$2' already exists. Remove first: ccmorph rm $2"
        return 1
      }

      if [[ -e "$_CCMORPH_PROFILE_DIRS/$2" || -L "$_CCMORPH_PROFILE_DIRS/$2" ||
            -e "$dir/auth/$2" || -L "$dir/auth/$2" ]]; then
        _ccm_err "leftover private state for '$2'; run ccmorph rm $2 to review and remove it, or choose another name"
        return 1
      fi

      local -a _add_providers=()
      if _ccm_provider_list 2>/dev/null; then
        _add_providers=("${(@)REPLY:#claude}")
      fi

      local choice _prov _type_rows=$'sub\tClaude account\tSign in with your browser\ntoken\tClaude token\tPaste an existing OAuth token\napikey\tAPI key\tAnthropic or a gateway'
      for _prov in "${_add_providers[@]}"; do
        _ccm_provider_label "$_prov"
        _type_rows+=$'\n'"$_prov"$'\t'"$REPLY"$'\t'"Sign in with your browser"
      done
      if [[ -t 0 ]]; then
        choice=$(ccm_ui_select native "Sign-in method — $2" "$_type_rows") || return 1
      else
        ccm_ui_list "Sign-in method — $2" "$_type_rows"
        ccm_ui_input '> ' || return 1
        choice="$REPLY"
      fi

      case "$choice" in
        1|sub*)
          # Isolate the login to the profile dir. Running `claude auth login`
          # bare writes this profile's profile state to the GLOBAL
          # ~/.claude.json, which is not where a profile's state belongs:
          # every other credential type lands in its own profile dir, and a
          # sub profile should be no different. Provision the profile dir
          # first, then point CLAUDE_CONFIG_DIR at it so both the login and
          # the email readback land in the profile, never the global file.
          _ccm_write \
            "$dir/$2.conf" \
            "type=sub" || {
            _ccm_err "could not save profile '$2'"
            return 1
          }

          # Not _ccm_read: the email comes from the login below, so the validator
          # would reject this placeholder before the browser ever opens.
          # Provisioning only needs the type.
          typeset -gA _ccm=([type]=sub)
          if ! _ccm_setup_dir "$2"; then
            unset _ccm
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          fi

          local _profile_dir="${_CCMORPH_PROFILE_DIRS}/$2"

          # Same scrub as _ccm_logged_email, for the same reason: with an
          # ANTHROPIC_API_KEY exported, the CLI can satisfy itself from that key
          # instead of running the browser flow this profile type exists to run.
          # A subshell per call keeps the caller's environment intact.
          _ccm_claude_auth "$_profile_dir" logout &>/dev/null || {
            _ccm_err "logout failed"
            unset _ccm
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          }

          print "${_ccm_dim}Opening your browser to sign in — Ctrl-C to cancel.${_ccm_reset}"
          _ccm_claude_auth "$_profile_dir" login || {
            _ccm_err "login failed"
            unset _ccm
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          }

          _ccm_logged_email "$_profile_dir" || {
            unset _ccm
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          }

          # Blank is not clean. _ccm_clean_value only rejects CR/LF, so an empty
          # email passed it — and an empty email is exactly what a key-shaped
          # auth answer produces, which is how a `type=sub` profile with no
          # address at all came to be written and marked active.
          if [[ -z "$_ccm_email" ]] || ! _ccm_clean_value "$_ccm_email"; then
            _ccm_err "no subscription profile found after login — nothing was saved"
            unset _ccm
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          fi

          _ccm_write \
            "$dir/$2.conf" \
            "type=sub"$'\n'"email=$_ccm_email" || {
            _ccm_err "could not save profile '$2'"
            unset _ccm
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          }

          unset CLAUDE_CODE_OAUTH_TOKEN
          unset _ccm

          print "${_ccm_accent}Added${_ccm_reset} $2 (sub${_ccm_email:+: $_ccm_email})"
          ;;

        2|token|oauth_token|oauth-token)
          local token

          ccm_ui_input "Paste token for $2 (hidden): " secret || return 1
          token="$REPLY"; REPLY=''

          [[ -n "$token" ]] || {
            _ccm_err "empty token"
            return 1
          }

          _ccm_clean_value "$token" || {
            _ccm_err "token contains CR/LF"
            return 1
          }

          _ccm_write \
            "$dir/$2.conf" \
            "type=token"$'\n'"token=$token" || {
            _ccm_err "could not save profile '$2'"
            unset token
            return 1
          }

          if ! _ccm_read "$2" || ! _ccm_setup_dir "$2"; then
            unset _ccm token
            _ccm_remove_profile_dir "$2"
            _ccm_remove_file "$dir/$2.conf"
            return 1
          fi

          unset _ccm token
          print "${_ccm_accent}Added${_ccm_reset} $2 (token)"
          ;;

        3|apikey|api_key|api-key)
          local _ak_key _ak_url _ak_conf _ak_preset="" _ak_label=""
          local -a _ak_tiers=()

          # A known provider fills its endpoint and the models its Claude Code
          # tiers map to; "custom" asks for the URL, blank = api.anthropic.com.
          _ccm_provider_presets
          local _ak_rows="${REPLY:+$REPLY$'\n'}"$'custom\tCustom endpoint\tAnthropic or any gateway URL'
          if [[ -t 0 ]]; then
            _ak_preset=$(ccm_ui_select search "Provider — $2" "$_ak_rows") || return 1
          else
            ccm_ui_list "Provider — $2" "$_ak_rows"
            ccm_ui_input '> ' || return 1
            _ak_preset="$REPLY"
            [[ "$_ak_preset" == <-> ]] && _ak_preset="${${(@f)_ak_rows}[$_ak_preset]%%$'\t'*}"
          fi
          if [[ "$_ak_preset" != custom ]]; then
            _ccm_provider_preset "$_ak_preset" || {
              _ccm_err "unknown provider '$_ak_preset'"
              return 1
            }
            _ak_label="${reply[1]}" _ak_url="${reply[2]}" _ak_tiers=("${reply[@]:2}")
          fi

          ccm_ui_input "API key for $2${_ak_label:+ ($_ak_label)} (hidden): " secret || return 1
          _ak_key="$REPLY"; REPLY=''

          [[ -n "$_ak_key" ]] || {
            _ccm_err "empty api key"
            return 1
          }

          # Base URL optional (blank = native Anthropic endpoint). Model is
          # deliberately NOT prompted for apikey profiles: Claude Code keeps
          # owning model selection (choose in-session with /model). A gateway
          # that renames models made a pinned default a footgun.
          if [[ -z "$_ak_label" ]]; then
            ccm_ui_input 'Base URL (blank = api.anthropic.com): ' || return 1
            _ak_url="$REPLY"
          fi

          _ccm_clean_value "$_ak_key" &&
            _ccm_clean_value "$_ak_url" || {
              _ccm_err "apikey values cannot contain CR/LF"
              unset _ak_key
              return 1
            }

          _ak_conf="type=apikey"
          _ak_conf+=$'\n'"api_key=$_ak_key"
          if [[ -n "$_ak_url" ]]; then
            _ak_conf+=$'\n'"base_url=$_ak_url"

          fi
          if (( ${#_ak_tiers} )); then
            _ak_conf+=$'\n'"env.ANTHROPIC_DEFAULT_OPUS_MODEL=${_ak_tiers[1]}"
            _ak_conf+=$'\n'"env.ANTHROPIC_DEFAULT_SONNET_MODEL=${_ak_tiers[2]}"
            _ak_conf+=$'\n'"env.ANTHROPIC_DEFAULT_HAIKU_MODEL=${_ak_tiers[3]}"
          fi

          _ccm_commit_profile "$2" "$_ak_conf" _ak_key || return 1

          print "${_ccm_accent}Added${_ccm_reset} $2 (${_ak_label:-apikey} → ${_ak_url:-api.anthropic.com})"
          if [[ -n "$_ak_url" ]]; then
            _ccm_probe_nudge "$2"

          fi
          print "${_ccm_dim}Claude Code may ask you to approve this key on first launch.${_ccm_reset}"
          ;;

        *)
          # Check if the choice matches a discovered provider (by number or name)
          local _matched_provider=""
          if (( ${#_add_providers} )); then
            if [[ "$choice" == <-> ]] && (( choice >= 4 && choice < 4 + ${#_add_providers} )); then
              _matched_provider="${_add_providers[$(( choice - 3 ))]}"
            else
              local _mp
              for _mp in "${_add_providers[@]}"; do
                [[ "${(L)choice}" == "${(L)_mp}" ]] && { _matched_provider="$_mp"; break }
              done
            fi
          fi

          if [[ -n "$_matched_provider" ]]; then
            _ccm_provider_login "$2" "$_matched_provider" || {
              command rm -rf "$_CCMORPH_DIR/auth/$2"
              return 1
            }

            _ccm_write \
              "$dir/$2.conf" \
              "type=provider"$'\n'"provider=$_matched_provider" || {
              _ccm_err "could not save profile '$2'"
              command rm -rf "$_CCMORPH_DIR/auth/$2"
              return 1
            }

            if ! _ccm_read "$2" || ! _ccm_setup_dir "$2"; then
              unset _ccm
              _ccm_remove_profile_dir "$2"
              _ccm_remove_file "$dir/$2.conf"
              return 1
            fi
            unset _ccm

            print "${_ccm_accent}Added${_ccm_reset} $2 (provider: $_matched_provider)"
            print "${_ccm_dim}Set a default model:  ccmorph set $2 model <model-id>${_ccm_reset}"
            print "${_ccm_dim}Configure defaults: ccmorph config $2${_ccm_reset}"
          else
            _ccm_err "cancelled"
            return 1
          fi
          ;;
      esac
      # The first profile is the default so `ccmorph run` works straight away; an
      # existing default is never changed, and failing to record one is not an
      # add failure (the profile is saved).
      if [[ ! -e "$dir/active" && ! -L "$dir/active" ]]; then
        _ccm_write "$dir/active" "$2" && print "${_ccm_dim}Default profile: $2 (first profile)${_ccm_reset}"
      fi
      print -r -- "Next: ccmorph run $2"
      ;;

    login)
      # Re-authenticating is safe and repeatable, so a missing name asks.
      if [[ -z "${2:-}" || "${2:-}" == (-i|--interactive) ]]; then
        local _login_pick
        _login_pick=$(_ccm_menu_profile provider) || return 1
        [[ -n "$_login_pick" ]] || return 1
        set -- login "$_login_pick"
      fi
      _ccm_valid "${2:-}" || {
        _ccm_err "usage: ccmorph login <name>"
        return 1
      }
      _ccm_exists "$2" || {
        _ccm_err "no profile '$2'"
        _ccm_accounts_hint
        return 1
      }
      _ccm_read "$2" || return 1
      [[ "${_ccm[type]}" == provider ]] || {
        _ccm_err "'$2' is not a provider profile — login is only for provider profiles"
        unset _ccm
        return 1
      }
      local _login_provider="${_ccm[provider]}"
      unset _ccm
      _ccm_provider_login "$2" "$_login_provider"
      ;;

    rm)
      _ccm_valid "${2:-}" || {
        _ccm_err "usage: ccmorph rm <name>"
        return 1
      }
      if [[ "${CCMORPH_CONTEXT_LOCKED:-}" != "$2" ]]; then
        command env CCMORPH_STATE_DIR="$dir" CCMORPH_CONTEXT_LOCKED="$2" \
          "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" model-window --locked "$2" zsh -c \
          'source "$1"; shift; ccmorph "$@"' ccmorph "$_CCMORPH_LIB_DIR/ccmorph.sh" "$@"
        return $?
      fi
      if ! _ccm_exists "$2"; then
        if [[ ! -e "$_CCMORPH_PROFILE_DIRS/$2" && ! -L "$_CCMORPH_PROFILE_DIRS/$2" &&
              ! -e "$dir/auth/$2" && ! -L "$dir/auth/$2" ]]; then
          _ccm_err "no profile '$2'"
          return 1
        fi
        print -u2 -- "Profile '$2' has no .conf. Remove leftover private state?"
        print -u2 -- "  $_CCMORPH_PROFILE_DIRS/$2"
        print -u2 -- "  $dir/auth/$2"
        print -u2 -- "Shared conversation history will be kept."
        ccm_ui_confirm 'Remove these leftovers?' || { _ccm_err "cancelled; nothing removed"; return 1; }
      fi

      _ccm_remove_profile_dir "$2" || {
        _ccm_err "could not remove credential directory for '$2'"
        return 1
      }

      command rm -f "$dir/$2.conf" || {
        _ccm_err "could not remove profile '$2'"
        return 1
      }
      "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" util keychain-delete "$2"
      command rm -f "$_CCMORPH_PROFILE_DIRS/../.locks/$2.lock"

      if [[ "$active" == "$2" ]]; then
        unset CLAUDE_CODE_OAUTH_TOKEN

        command rm -f "$dir/active" || {
          _ccm_err "could not clear default profile"
          return 1
        }
      fi

      print "${_ccm_accent}Removed${_ccm_reset} $2"
      ;;

    cred)
      # One verb for "print this profile's secret". Replaces `key`, `token`, and
      # `creds`,
      # which were the same operation split by profile type or abbreviated
      # into jargon. The conf already knows
      # which secret it holds, so the tool answers rather than asking.
      #
      # Named or chosen, never guessed: a bare `cred` must not silently print the
      # default profile's secret, but demanding a name it could have offered is the
      # same refusal-to-help `-i` exists to end. Only when stdin is a terminal —
      # piped into a script, a missing argument stays an error.
      if [[ -z "${2:-}" || "${2:-}" == (-i|--interactive) ]] && [[ -t 0 ]]; then
        local _cred_pick
        _cred_pick=$(_ccm_menu_profile apikey token) || return 1
        [[ -n "$_cred_pick" ]] || return 1
        set -- cred "$_cred_pick"
      fi
      _ccm_valid "${2:-}" || {
        _ccm_err "usage: ccmorph cred <profile>"
        return 1
      }

      _ccm_exists "$2" || {
        _ccm_err "no profile '$2'"
        _ccm_accounts_hint
        return 1
      }

      _ccm_read "$2" || return 1

      # Deliberately NOT _env: that provisions the profile's Claude Code state
      # (mkdir + 11 shared symlinks + .claude.json) as a side effect, so a
      # non-Claude consumer reading a credential would silently create a Claude
      # profile. This is a pure read.
      local _cred="${_ccm[api_key]:-${_ccm[token]:-}}"
      [[ -n "$_cred" ]] || {
        _ccm_err "'$2' is a ${_ccm[type]} profile and stores no credential here"
        _ccm_err "subscription profiles use browser login: ccmorph login $2"
        unset _ccm
        return 1
      }

      # Newline for a human, none for a pipe: `x=$(ccmorph cred gw)` must not
      # capture a trailing newline into a header value.
      if [[ -t 1 ]]; then
        print -r -- "$_cred"
      else
        print -rn -- "$_cred"
      fi

      unset _ccm
      ;;

    # `probe` is kept as an alias, not removed. v2.3.0 is published (release
    # tarball), so a script or a colleague's muscle memory already
    # says `probe`; silently breaking it to win a naming argument costs a user more
    # than the inconsistency does. `probe` is what the help and the docs teach.
    probe|"$_ccm_retired_verb")
      # The profile is positional, like every other command. It was `--profile
      # <profile>` alone among them, which is the kind of inconsistency a user pays
      # for every time they guess wrong.
      shift
      local -a _m_args=()
      if [[ "${1:-}" == (-i|--interactive) ]]; then
        local _probe_profile
        _probe_profile=$(_ccm_menu_profile) || return 1
        _m_args+=(--profile "$_probe_profile")
        shift
      fi
      if (( $# )) && [[ "$1" != -* ]]; then
        _m_args+=(--profile "$1")
        shift
      fi
      _m_args+=("$@")
      # `--model -i` or a trailing bare `--model`: pick from the route's listing
      # instead of leaking argparse's usage for an internal flag layout.
      local -i _mi=${_m_args[(I)--model]}
      if (( _mi )) && [[ -z "${_m_args[_mi+1]:-}" || "${_m_args[_mi+1]}" == (-i|--interactive) ]]; then
        local -i _pi=${_m_args[(I)--profile]}
        (( _pi )) || { _ccm_err "usage: ccmorph probe <profile> --model -i"; return 2; }
        local _probe_model
        _probe_model=$(_ccm_pick_value model "${_m_args[_pi+1]}") || return 1
        if [[ -z "${_m_args[_mi+1]:-}" ]]; then
          _m_args+=("$_probe_model")
        else
          _m_args[_mi+1]="$_probe_model"
        fi
      fi
      if (( ${_m_args[(I)--allow-inference]} )); then
        print -u2 -- "ccmorph: Checking context limits; inference requests may incur charges."
      fi
      "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" context-probe "${_m_args[@]}"
      ;;


    models|_models)
      (( $# <= 3 )) || { _ccm_err 'usage: ccmorph models [profile] [--filtered|--filtered-json]'; return 2; }
      if (( $# == 1 )) || [[ "${2:-}" == (-i|--interactive) ]]; then
        local _models_profile
        _models_profile=$(_ccm_menu_profile apikey) || return 1
        set -- "$1" "$_models_profile" "${@:3}"
      fi
      (( $# >= 2 )) || { _ccm_err "usage: ccmorph models <profile> [--filtered|--filtered-json]"; return 2; }
      [[ "${3:-}" == "" || "${3:-}" == "--filtered" || "${3:-}" == "--filtered-json" ]] || {
        _ccm_err "usage: ccmorph models <profile> [--filtered|--filtered-json]"
        return 2
      }
      _ccm_valid "${2:-}" || {
        _ccm_err "model listing requires an profile"
        return 1
      }

      _ccm_exists "$2" || {
        _ccm_err "no profile '$2'. Run: ccmorph list"
        return 1
      }

      if [[ "${3:-}" == "--filtered" ]]; then
        _ccm_gw_models "$2" picker
      elif [[ "${3:-}" == "--filtered-json" ]]; then
        _ccm_gw_models_json "$2" || return 1
        local _models_json="$REPLY"
        print -r -- "$_models_json" | _ccm_util models-filtered-json "$_CCMORPH_DIR/$2.conf" || return 1
      else
        _ccm_gw_models "$2"
      fi
      ;;

    inspect)
      # The command that did not exist: what does this profile actually launch as?
      #
      # Answering it used to mean running `list`, `model`, `effort` and `models` and
      # then knowing which layer wins -- and even then the listing could contradict
      # the launch. Every line here names the source of its value, so "where does
      # this come from" is a readable fact rather than a trace through the code.
      local _sh_name="${2:-$active}"
      # `-i` here, and a bare `inspect` with no default set: the profile list is
      # exactly what a reader in that position needs, not an error telling them to
      # supply what the tool could show them.
      if [[ "${2:-}" == (-i|--interactive) || -z "$_sh_name" ]]; then
        _sh_name=$(_ccm_menu_profile) || return 1
        [[ -n "$_sh_name" ]] || return 1
      fi
      _ccm_valid "$_sh_name" || {
        _ccm_err "usage: ccmorph inspect [<profile>]"
        return 1
      }
      _ccm_exists "$_sh_name" || {
        _ccm_err "no profile '$_sh_name'"
        _ccm_accounts_hint
        return 1
      }

      _ccm_read "$_sh_name" || return 1

      local _sh_type="${_ccm[type]}"
      local _sh_base="${_ccm[base_url]:-}"
      local _sh_tag="${_ccm[tag]:-}"
      local _sh_pct_conf="${_ccm[env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE]:-}"
      unset _ccm

      # Heading: name, type, route label, and whether this is the default.
      local _sh_head="${_ccm_bold}${_sh_name}${_ccm_reset}  ${_ccm_dim}${_sh_type}"
      [[ -n "$_sh_tag" ]] && _sh_head+=" · ${_sh_tag}"
      [[ "$_sh_name" == "$active" ]] && _sh_head+=" · default profile"
      print -r -- "${_sh_head}${_ccm_reset}"
      print ""

      [[ -n "$_sh_base" ]] &&
        _ccm_grid 10 endpoint "${_sh_base#https://}"

      # Model, and which of the two places answered.
      local _sh_model _sh_from
      _sh_model=$(_ccm_config_store get "$_sh_name" model) || return 1
      if [[ -n "$_sh_model" ]]; then
        _sh_from="profile default"
      else
        _sh_model=$(command sed -n 's/^model=//p' "$dir/$_sh_name.conf" 2>/dev/null | command head -1) ||
          _sh_model=""
        [[ -n "$_sh_model" ]] && _sh_from="conf bootstrap"
      fi
      if [[ -n "$_sh_model" ]]; then
        _ccm_grid 10 model "$_sh_model  ${_ccm_dim}${_sh_from}${_ccm_reset}"
      else
        _ccm_grid 10 model "${_ccm_dim}— Claude Code chooses${_ccm_reset}"
      fi

      local _sh_effort _sh_ef_from="profile default"
      _sh_effort=$(_ccm_config_store get "$_sh_name" effort) || return 1
      if [[ -n "$_sh_effort" ]]; then
        _ccm_grid 10 effort "$_sh_effort  ${_ccm_dim}${_sh_ef_from}${_ccm_reset}"
      else
        _ccm_grid 10 effort "${_ccm_dim}— Claude Code's own default${_ccm_reset}"
      fi

      _ccm_config_store model-settings "$_sh_name" | _ccm_util effort-rows

      # Window, and ONLY when it is a probed fact about a routed model.
      #
      # Two traps, both of which produced a confidently wrong number here first: the
      # resolver answers with `{"window": 200000, "unprobed": true}` when it has
      # nothing recorded, so printing `.window` unconditionally reports a default as
      # though it were probed; and for a native (sub/token) profile the window is
      # Claude Code's own business, sized from the model id, so claiming a number is
      # not ours to do. An unprobed routed model says so and names the fix.
      if [[ -n "$_sh_model" && "$_sh_type" == apikey ]]; then
        local _sh_win _sh_tok="" _sh_unprobed=1 _sh_source="probed"
        _sh_win=$(_ccm_model_window "$_sh_name" "$_sh_model" 2>/dev/null) || _sh_win=""
        if [[ -n "$_sh_win" ]]; then
          _sh_tok=$(_ccm_util window-label <<< "$_sh_win" 2>/dev/null) || _sh_tok=""
          _sh_source="${_sh_tok##* }"
          _sh_tok="${_sh_tok% *}"
          _sh_unprobed="${_sh_tok##* }"
          _sh_tok="${_sh_tok%% *}"
        fi
        if [[ "$_sh_tok" == <-> ]] && (( _sh_tok > 0 )) && (( ! _sh_unprobed )); then
          local _sh_pct
          _sh_pct=$(_ccm_effective_compaction_percent "$_sh_name" "$_sh_pct_conf") || return 1
          _ccm_grid 10 window \
            "$(printf "%'d" "$_sh_tok")  ${_ccm_dim}${_sh_source} · compaction target ${_sh_pct}%${_ccm_reset}"
        else
          _ccm_grid 10 window \
            "${_ccm_dim}unknown · compacts early to stay safe${_ccm_reset}"
        fi
      fi

      case "$_sh_base" in
        ""|https://api.anthropic.com|https://api.anthropic.com/)
          _ccm_grid 10 protocol 'native Anthropic' ;;
        *)
          _ccm_grid 10 protocol 'automatic — cached compatibility or validation at launch' ;;
      esac

      # The whole point: what this becomes at launch.
      print ""
      if [[ -n "$_sh_model" && "$_sh_type" == apikey ]]; then
        print -r -- "  ${_ccm_dim}launches as${_ccm_reset}  claude --model ${_sh_model}"
      else
        print -r -- "  ${_ccm_dim}launches as${_ccm_reset}  claude ${_ccm_dim}(Claude Code picks the model)${_ccm_reset}"
      fi
      ;;

    _menu)
      # `pick` is the pre-1.10 name, kept as an undocumented alias so a script
      # or a habit from an earlier version keeps working. Both are internal: the
      # spelling users read is `-i`, which routes here.
      #
      # No profile named: chain from the profile chooser, exactly as `ccmorph -i`
      # does. Erroring here instead made one concept behave two ways depending on
      # where it was typed, and the argument it asked for is the one the chooser
      # exists to supply.
      local _menu_account="${2:-}"
      if [[ -z "$_menu_account" ]]; then
        _menu_account=$(_ccm_menu_profile) || return 1
        [[ -n "$_menu_account" ]] || return 1
      fi

      _ccm_valid "$_menu_account" || {
        _ccm_err "usage: the model picker needs an profile"
        return 1
      }

      _ccm_exists "$_menu_account" || {
        _ccm_err "no profile '$_menu_account'. Run: ccmorph list"
        return 1
      }

      _ccm_pick_value model "$_menu_account"
      ;;

    # The escape hatch `probe` sends people to. Discovery answers for almost every
    # model, but a gateway that publishes no size for an id newer than every
    # catalogue leaves exactly one free fix: type the number in. That was reachable
    # only by hand-editing `context_window.<model>` into a .conf -- a file the CLI
    # otherwise owns, and a step no output ever named. `probe` now prints this as
    # option 1, so the verb has to exist.
    context)
      local _ctx_profile="${2:-}" _ctx_model="${3:-}"
      if [[ -z "$_ctx_profile" || "$_ctx_profile" == (-i|--interactive) ]]; then
        _ctx_profile=$(_ccm_menu_profile) || return 1
        [[ -n "$_ctx_profile" ]] || return 1
      fi
      _ccm_valid "$_ctx_profile" || {
        _ccm_err "usage: ccmorph context <profile> <model> [tokens|auto]"
        return 2
      }
      _ccm_exists "$_ctx_profile" || {
        _ccm_err "no profile '$_ctx_profile'. Run: ccmorph list"
        return 1
      }
      # A model id is exactly as unguessable here as it is at `set`, and this verb
      # is reached from a `probe` failure — the moment a reader is least sure of the
      # spelling. So a missing or `-i` model opens the same picker.
      if [[ -z "$_ctx_model" || "$_ctx_model" == (-i|--interactive) ]]; then
        _ctx_model=$(_ccm_pick_value model "$_ctx_profile") || return 1
        [[ -n "$_ctx_model" ]] || return 1
        # A picked model means the tokens argument (if any) shifted along with it.
        set -- context "$_ctx_profile" "$_ctx_model" "${@:4}"
      fi
      command env CCMORPH_STATE_DIR="$dir" "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" model-window \
        --context "$_ctx_profile" "$_ctx_model" "${@:4}"
      ;;

    get|set|config)
      local _cfg_verb="$1" _cfg_profile="${2:-}" _cfg_key="${3:-}"
      case "$_cfg_verb" in
        get) (( $# <= 3 )) || { _ccm_err 'usage: ccmorph get [profile] [model|effort]'; return 2; } ;;
        set) (( $# <= 4 )) || { _ccm_err 'usage: ccmorph set [profile] [model|effort] [value]'; return 2; } ;;
        config) (( $# <= 2 )) || { _ccm_err 'usage: ccmorph config [profile]'; return 2; } ;;
      esac
      # No profile named: chain from the profile chooser rather than erroring, the
      # same way `run -i` does. The argument being demanded is the one the chooser
      # exists to supply, and a user who cannot recall a model id often cannot
      # recall a profile name either.
      if [[ -z "$_cfg_profile" || "$_cfg_profile" == (-i|--interactive) ]]; then
        _cfg_profile=$(_ccm_menu_profile) || return 1
        [[ -n "$_cfg_profile" ]] || return 1
        set -- "$1" "$_cfg_profile" "${@:3}"
        _cfg_key="${3:-}"
      fi
      _ccm_valid "$_cfg_profile" || { _ccm_err "usage: ccmorph $_cfg_verb <profile>"; return 2; }
      _ccm_exists "$_cfg_profile" || { _ccm_err "no profile '$_cfg_profile'"; return 1; }
      # `set <profile> <slot>` with no value, and `set <profile> <slot> -i`, both
      # mean "show me the choices": a missing value is the same request as an
      # explicit ask, so it opens the picker instead of printing a usage line the
      # reader cannot act on without knowing the value already.
      if [[ "$_cfg_verb" == set && "$_cfg_key" == (model|effort) ]] &&
         { (( $# == 3 )) || [[ "${4:-}" == (-i|--interactive) ]] }; then
        local _cfg_picked
        _cfg_picked=$(_ccm_pick_value "$_cfg_key" "$_cfg_profile") || return 1
        [[ -n "$_cfg_picked" ]] || return 1
        set -- set "$_cfg_profile" "$_cfg_key" "$_cfg_picked"
      fi
      if [[ "$_cfg_verb" == (get|set) && "$_cfg_key" == (''|-i|--interactive) ]] && (( $# <= 3 )); then
        _cfg_key=$(ccm_ui_select native "Setting — $_cfg_profile" \
          $'model\tModel\tDefault model for new sessions\neffort\tReasoning\tDefault reasoning level') || return 1
        if [[ "$_cfg_verb" == set ]]; then
          local _cfg_value
          _cfg_value=$(_ccm_pick_value "$_cfg_key" "$_cfg_profile") || return 1
          set -- set "$_cfg_profile" "$_cfg_key" "$_cfg_value"
        else
          set -- get "$_cfg_profile" "$_cfg_key"
        fi
      fi
      case "$_cfg_verb" in
        get) (( $# == 3 )) && [[ "$_cfg_key" == (model|effort) ]] || { _ccm_err "usage: ccmorph get <profile> <model|effort>"; return 2; } ;;
        set) (( $# == 4 )) && [[ "$_cfg_key" == (model|effort) && -n "$4" ]] || { _ccm_err "usage: ccmorph set <profile> <model|effort> [value]  (omit the value, or pass -i, to choose from a list)"; return 2; } ;;
        config) (( $# == 2 )) || { _ccm_err "usage: ccmorph config <profile>"; return 2; } ;;
      esac
      if [[ "$_cfg_verb" == get ]]; then
        _ccm_config_store get "$_cfg_profile" "$_cfg_key"
        return $?
      fi
      if [[ "${CCMORPH_CONTEXT_LOCKED:-}" != "$_cfg_profile" ]]; then
        command env CCMORPH_STATE_DIR="$dir" CCMORPH_CONTEXT_LOCKED="$_cfg_profile" \
          "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" model-window --locked "$_cfg_profile" zsh -c \
          'source "$1"; shift; ccmorph "$@"' ccmorph "$_CCMORPH_LIB_DIR/ccmorph.sh" "$@"
        return $?
      fi
      local _CCM_CONFIG_MODEL="" _CCM_CONFIG_EFFORT="" _CCM_CONFIG_SNAPSHOT=""
      local _CCM_CONFIG_TYPE="" _CCM_CONFIG_ENDPOINT="" _CCM_CONFIG_CREDENTIAL=""
      export _CCM_CONFIG_MODEL _CCM_CONFIG_EFFORT _CCM_CONFIG_SNAPSHOT
      export _CCM_CONFIG_TYPE _CCM_CONFIG_ENDPOINT _CCM_CONFIG_CREDENTIAL
      if [[ "$_cfg_verb" == set ]]; then
        if [[ "$_cfg_key" == model ]]; then _CCM_CONFIG_MODEL="$4"; else _CCM_CONFIG_EFFORT="$4"; fi
        _ccm_config_store set "$_cfg_profile" || return 1
        # A bare value on stdout, so `m=$(ccmorph get gw model)` and any script
        # already parsing this keep working. The sentence goes to stderr, where a
        # picked value needs it most: the user never typed the id they chose, so
        # without this the terminal shows a list closing and nothing else.
        [[ -t 2 ]] && print -u2 -- \
          "${_ccm_accent}Saved${_ccm_reset} $_cfg_profile $_cfg_key = ${_ccm_bold}$4${_ccm_reset} ${_ccm_dim}(new sessions)${_ccm_reset}"
        print -r -- "$4"
        return 0
      fi
      _CCM_CONFIG_SNAPSHOT=$(_ccm_config_store snapshot "$_cfg_profile") || return 1
      _ccm_read "$_cfg_profile" || return 1
      _CCM_CONFIG_TYPE="${_ccm[type]}"
      _CCM_CONFIG_ENDPOINT="${_ccm[base_url]:-}"
      _CCM_CONFIG_CREDENTIAL="${_ccm[api_key]:-${_ccm[token]:-}}"
      _CCM_CONFIG_MODEL=$(_ccm_config_store get "$_cfg_profile" model) || return 1
      _CCM_CONFIG_EFFORT=$(_ccm_config_store get "$_cfg_profile" effort) || return 1
      # Terminal users choose a task, not a tour of every setting. Preserve the
      # existing piped-input sequence for scripts.
      local answer="" _cfg_section=all _cfg_rows=""
      if [[ -t 0 ]]; then
        _cfg_section=defaults
        if [[ "$_CCM_CONFIG_TYPE" == (apikey|token) ]]; then
          _cfg_rows=$'defaults\tModel and reasoning\tDefaults for new sessions'
          if [[ "$_CCM_CONFIG_TYPE" == apikey ]]; then
            _cfg_rows+=$'\nconnection\tConnection\tEndpoint or API key'
          else
            _cfg_rows+=$'\nconnection\tSign-in token\tReplace the stored token'
          fi
          _cfg_section=$(ccm_ui_select native "Settings — $_cfg_profile" "$_cfg_rows") || { unset _ccm; return 1; }
        fi
      fi
      if [[ "$_cfg_section" != defaults && "$_CCM_CONFIG_TYPE" == apikey ]]; then
        ccm_ui_input "Endpoint [${_CCM_CONFIG_ENDPOINT:-https://api.anthropic.com}]: " || { unset _ccm; return 1; }
        answer="$REPLY"
        [[ -z "$answer" ]] || _CCM_CONFIG_ENDPOINT="$answer"
        _ccm[base_url]="$_CCM_CONFIG_ENDPOINT"
        if [[ -n "$_CCM_CONFIG_ENDPOINT" ]]; then
          _ccm_validate_base_url "profile '$_cfg_profile'" || { unset _ccm; return 1; }
        fi
      fi
      if [[ "$_cfg_section" != defaults && "$_CCM_CONFIG_TYPE" == (apikey|token) ]]; then
        if [[ "$_cfg_section" == connection ]] || ccm_ui_confirm 'Replace stored credential?'; then
          ccm_ui_input 'Credential (hidden; Enter keeps current): ' secret || { unset _ccm; return 1; }
          answer="$REPLY"; REPLY=''
          [[ -z "$answer" ]] || _CCM_CONFIG_CREDENTIAL="$answer"
        else
          (( $? == 2 )) && { unset _ccm; return 1; }
        fi
        _ccm_clean_value "$_CCM_CONFIG_CREDENTIAL" || { unset _ccm; _ccm_err "credential contains CR/LF"; return 1; }
        if [[ "$_CCM_CONFIG_TYPE" == apikey ]]; then _ccm[api_key]="$_CCM_CONFIG_CREDENTIAL"; else _ccm[token]="$_CCM_CONFIG_CREDENTIAL"; fi
      fi
      _ccm_validate_loaded "$_cfg_profile" || { unset _ccm; return 1; }
      unset _ccm
      if [[ "$_cfg_section" != connection ]]; then
        _CCM_CONFIG_MODEL=$(_ccm_prompt_default model "$_cfg_profile" "$_CCM_CONFIG_MODEL") || return 1
        _CCM_CONFIG_EFFORT=$(_ccm_prompt_default effort "$_cfg_profile" "$_CCM_CONFIG_EFFORT") || return 1
      fi
      print -ru2 -- "Profile: $_cfg_profile"
      if [[ "$_cfg_section" != defaults ]]; then
        [[ "$_CCM_CONFIG_TYPE" != apikey ]] || print -ru2 -- "Endpoint: ${_CCM_CONFIG_ENDPOINT:-https://api.anthropic.com}"
        print -ru2 -- 'Credential: hidden'
      fi
      print -ru2 -- "Model: ${_CCM_CONFIG_MODEL:-Claude default} · Effort: ${_CCM_CONFIG_EFFORT:-Claude default}"
      _ccm_config_store configure "$_cfg_profile" || return 1
      print -r -- "Saved $_cfg_profile (new sessions)"
      ;;

    mv)
      _ccm_valid "${2:-}" && _ccm_valid "${3:-}" || {
        _ccm_err "usage: ccmorph mv <old> <new>"
        return 1
      }
      if [[ "${CCMORPH_CONTEXT_LOCKED:-}" != "$2,$3" ]]; then
        command env CCMORPH_STATE_DIR="$dir" CCMORPH_CONTEXT_LOCKED="$2,$3" \
          "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" model-window --locked "$2,$3" zsh -c \
          'source "$1"; shift; ccmorph "$@"' ccmorph "$_CCMORPH_LIB_DIR/ccmorph.sh" "$@"
        return $?
      fi

      _ccm_exists "$2" || {
        _ccm_err "no profile '$2'"
        return 1
      }

      _ccm_exists "$3" && {
        _ccm_err "'$3' already exists"
        return 1
      }


      local old_profile="$_CCMORPH_PROFILE_DIRS/$2"
      local new_profile="$_CCMORPH_PROFILE_DIRS/$3"
      local moved_profile=0
      local rc=0

      # mv onto an EXISTING target nests the source inside it (new/old/…),
      # which the rollback below cannot undo — refuse instead. A leftover
      # directory from a deleted conf is exactly the case that hits this.
      if [[ -e "$new_profile" || -L "$new_profile" ]]; then
        _ccm_err "profile directory '$3' already exists — remove it first"
        return 1
      fi

      if [[ -e "$old_profile" || -L "$old_profile" ]]; then
        command mv "$old_profile" "$new_profile" || {
          _ccm_err "could not rename profile directory"
          return 1
        }
        moved_profile=1
      fi

      command mv "$dir/$2.conf" "$dir/$3.conf" || rc=$?

      # Rename the provider auth directory if it exists.
      local _old_auth="$_CCMORPH_DIR/auth/$2"
      local _new_auth="$_CCMORPH_DIR/auth/$3"
      local _moved_auth=0
      if (( rc == 0 )) && [[ -d "$_old_auth" ]]; then
        if [[ -e "$_new_auth" ]]; then
          _ccm_err "auth directory for '$3' already exists: $_new_auth — remove it first"
          rc=1
        else
          command mv "$_old_auth" "$_new_auth" || rc=$?
          (( rc == 0 )) && _moved_auth=1
        fi
      fi

      if (( rc == 0 )) && [[ "$active" == "$2" ]]; then
        _ccm_atomic_write_unlocked "$dir/active" "$3" || rc=$?
      fi

      if (( rc != 0 )) && [[ -e "$dir/$3.conf" ]]; then
        command mv "$dir/$3.conf" "$dir/$2.conf"
      fi

      if (( rc != 0 )); then
        (( _moved_auth )) && command mv "$_new_auth" "$_old_auth"
        (( moved_profile )) && command mv "$new_profile" "$old_profile"
        _ccm_err "could not rename profile"
        return 1
      fi

      "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" util keychain-rename "$dir" "$2" "$3" ||
        _ccm_err "renamed, but '$3' still reads its credential from the keychain item of '$2'"
      print "${_ccm_accent}Renamed${_ccm_reset} ${_ccm_bold}$2${_ccm_reset} → ${_ccm_bold}$3${_ccm_reset}"
      ;;

    hop)
      local _hop_profile="${2:-}" _hop_current=0
      if [[ "$_hop_profile" == (-i|--interactive) ]]; then
        [[ -n "${CLAUDE_CODE_SESSION_ID:-}" ]] && _ccm_relay_valid "${CCMORPH_RELAY_ID:-}" || {
          _ccm_err 'profile switching requires a running ccmorph session'
          return 1
        }
        _hop_profile=$(_ccm_menu_profile) || return 1
      fi
      if [[ -z "$_hop_profile" && -n "${CCMORPH_RELAY_ID:-}" ]]; then
        # The adapter this session's launcher started names the profile in its argv.
        _hop_profile=$(_ccm_util session-adapter "${CCMORPH_RELAY_ID%%.*}") && _hop_current=1 || _hop_profile=""
      fi
      # No adapter serves sub and token sessions; their config dir is the only witness.
      if [[ -z "$_hop_profile" && -n "${CCMORPH_RELAY_ID:-}" && -n "${CLAUDE_CONFIG_DIR:-}" &&
            "${CLAUDE_CONFIG_DIR:A:h}" == "${_CCMORPH_PROFILE_DIRS:A}" ]]; then
        _hop_profile="${CLAUDE_CONFIG_DIR:t}"
        _hop_current=1
      fi

      _ccm_valid "$_hop_profile" || {
        _ccm_err "usage: ccmorph hop [profile]"
        return 1
      }

      _ccm_exists "$_hop_profile" || {
        _ccm_err "no profile '$_hop_profile'. Run: ccmorph list"
        return 1
      }

      _ccm_read "$_hop_profile" || return 1

      _ccm_validate_loaded "$_hop_profile" || {
        unset _ccm
        return 1
      }
      unset _ccm

      [[ -n "${CLAUDE_CODE_SESSION_ID:-}" ]] || {
        _ccm_err "not inside a Claude Code session"
        return 1
      }

      _ccm_relay_valid "${CCMORPH_RELAY_ID:-}" || {
        _ccm_err "this session was not launched by the ccmorph relay"
        return 1
      }

      # Hop ends the session itself: Claude Code is the first non-shell process
      # below the relay in this process's ancestry. The relay sees an exit it did
      # not signal, finds the queued switch and resumes.
      local _hop_claude
      _hop_claude=$(_ccm_util hop-target "${CCMORPH_RELAY_ID%%.*}" $$) || _hop_claude=""
      [[ -n "$_hop_claude" ]] || {
        _ccm_err "cannot find the Claude Code process to end. Run hop from the Claude Code prompt (/ccmorph:hop or ! ccmorph hop)"
        return 1
      }

      local _signal
      local -a _session_state=()

      _signal="profile=$_hop_profile"
      _signal+=$'\n'"session=$CLAUDE_CODE_SESSION_ID"
      if _ccm_session_state "$CLAUDE_CODE_SESSION_ID"; then
        _session_state=("${reply[@]}")
        # Carry the model only where the target can actually serve it. Leaving the
        # field out is already how the relay asks for the profile's own default, so
        # the fallback needs no second mechanism -- and a definite no is said out
        # loud, because a silently changed model is the thing the user notices last.
        if (( _hop_current )); then
          _signal+=$'\n'"model=${_session_state[1]}"
        else
          _ccm_model_known "$_hop_profile" "${_session_state[1]}"
          case $? in
            1) print "${_ccm_yellow}ccmorph:${_ccm_reset} ${_ccm_bold}${_session_state[1]}${_ccm_reset} is not served by ${_ccm_bold}${_hop_profile}${_ccm_reset} — resuming on its default model." ;;
            *) _signal+=$'\n'"model=${_session_state[1]}" ;;
          esac
        fi
        _signal+=$'\n'"effort=${_session_state[2]}"
      fi

      _ccm_write \
        "$dir/.reswitch.${CCMORPH_RELAY_ID}" \
        "$_signal" || {
        _ccm_log "hop FAILED to write relay target=$_hop_profile relay_id=${CCMORPH_RELAY_ID}"
        _ccm_err "could not queue switch"
        return 1
      }

      _ccm_log "hop queued target=$_hop_profile session=$CLAUDE_CODE_SESSION_ID relay_id=${CCMORPH_RELAY_ID}"
      print "${_ccm_accent}Switching${_ccm_reset} → ${_ccm_bold}$_hop_profile${_ccm_reset}"
      _ccm_log "hop ends claude pid=$_hop_claude"
      kill -TERM "$_hop_claude" 2>/dev/null || {
        command rm -f "$dir/.reswitch.${CCMORPH_RELAY_ID}"
        _ccm_err "could not end Claude Code (pid $_hop_claude)"
        return 1
      }
      ;;

    # `ccmorph _env <name>`, plus a running endpoint for a provider profile.
    #
    # _env answers "what does this profile launch as" and stops there, which for
    # every other type is also everything a subprocess needs to dial it: the
    # endpoint, the credential, and whatever HTTPS_PROXY the caller's own
    # environment already carries. A provider is the exception — it has no
    # endpoint until its bridge runs — so this arm starts one and hands back a
    # handle the caller must retire (`ccmorph _route_stop <handle>`) when done.
    _route_env)
      _ccm_valid "${2:-}" || return 1

      _ccm_exists "$2" || {
        print -u2 "ccmorph: unknown profile '$2'"
        return 1
      }

      _ccm_read "$2" || return 1
      local _re_type="${_ccm[type]:-}"
      unset _ccm

      if [[ "$_re_type" == provider ]]; then
        _ccm_provider_start "$2" || return 1
        ccmorph _env "$2" || { _ccm_provider_stop; return 1; }
        print "ANTHROPIC_BASE_URL=http://127.0.0.1:${_ccm_provider_port}"
        print "ANTHROPIC_API_KEY=${_ccm_provider_key}"
        print "CCMORPH_ROUTE_HANDLE=provider|${_ccm_provider_pid}|${_ccm_provider_tmpdir}|${_ccm_provider_auth}"
        return 0
      fi

      ccmorph _env "$2"
      ;;

    # Retire a route started by _route_env. Only a provider ever opens one, so
    # any other handle names nothing this process owns.
    _route_stop)
      if [[ "${2:-}" == provider\|* ]]; then
        local -a _rs=("${(@s:|:)2}")
        [[ "${_rs[2]}" == <-> ]] || return 1
        _ccm_provider_pid="${_rs[2]}" _ccm_provider_tmpdir="${_rs[3]}" _ccm_provider_auth="${_rs[4]}"
        _ccm_provider_stop
      fi
      ;;

    _env)
      _ccm_valid "${2:-}" || return 1

      _ccm_exists "$2" || {
        print -u2 "ccmorph: unknown profile '$2'"
        return 1
      }

      _ccm_read "$2" || return 1

      _ccm_validate_loaded "$2" || {
        unset _ccm
        return 1
      }

      _ccm_setup_dir "$2" || {
        unset _ccm
        return 1
      }

      _ccm_prelaunch "$2" 2>/dev/null || :

      # Secure storage follows CLAUDE_CONFIG_DIR, so each profile's sign-in stays in
      # its own keychain entry. MCP credentials are shared by capsules, not here.
      print "CLAUDE_CONFIG_DIR=${_CCMORPH_PROFILE_DIRS}/$2"
      [[ "${_ccm[private]:-}" == 1 ]] && print "CCMORPH_PRIVATE=1"

      case "${_ccm[type]}" in
        apikey)
          # An apikey profile may have no base_url: that is the native
          # api.anthropic.com case, where Claude Code's own default endpoint is
          # correct and overriding it would break the profile.
          if [[ -n "${_ccm[base_url]:-}" ]]; then
            print "ANTHROPIC_BASE_URL=${_ccm[base_url]}"
          fi

          print "ANTHROPIC_API_KEY=${_ccm[api_key]}"
          ;;

        token)
          print "CLAUDE_CODE_OAUTH_TOKEN=${_ccm[token]}"
          ;;

        provider)
          # Provider profiles have no static credential; the launcher injects
          # ANTHROPIC_BASE_URL and ANTHROPIC_API_KEY from the per-session
          # provider proxy at launch time. Emit env.* overrides only.
          ;;

        sub)
          # Deliberately no credential env. A sub profile authenticates from
          # the profile state in its own CLAUDE_CONFIG_DIR (written by
          # `ccmorph add sub`), so injecting anything here would override the
          # login it is supposed to use. Listed explicitly so the silence is a
          # decision, not a fall-through.
          ;;

        *)
          # Every credential type must say how it authenticates. Falling
          # through silently ships a session with no credential, which
          # surfaces as Claude Code's interactive login picker with nothing
          # explaining why — the failure mode is indistinguishable from a
          # revoked profile.
          _ccm_err "profile '$2' has unsupported type '${_ccm[type]}'"
          unset _ccm
          return 1
          ;;
      esac

      [[ -n "${_ccm[model_map]:-}" ]] && print "CCMORPH_MODEL_MAP=${_ccm[model_map]}"
      local _k
      for _k in "${(@k)_ccm}"; do
        [[ "$_k" == env.* ]] && print "${_k#env.}=${_ccm[$_k]}"
      done
      unset _ccm
      ;;

    quota)
      # Plan windows and spend of every profile, or of the one named. A token or
      # an API key goes to ccmorph-core on stdin, never in argv. An API key rides
      # with its base URL so a provider with a balance API (quota/balance.go) can
      # be asked what is left; ccmorph-core sends it to that provider's own host
      # only, and drops it for every other endpoint.
      local _q_json=0 _q_only="" _q_arg
      for _q_arg in "${@[2,-1]}"; do
        case "$_q_arg" in
          --json) _q_json=1 ;;
          -*) _ccm_err "usage: ccmorph quota [profile] [--json]"; return 2 ;;
          *)
            if [[ -n "$_q_only" ]]; then
              _ccm_err "usage: ccmorph quota [profile] [--json]"
              return 2
            fi
            _q_only="$_q_arg"
            ;;
        esac
      done
      local -a _q_names=()
      if [[ -n "$_q_only" ]]; then
        _ccm_exists "$_q_only" || {
          _ccm_err "no profile '$_q_only'. Run: ccmorph list"
          return 1
        }
        _q_names=("$_q_only")
      else
        local _q_f
        for _q_f in "$dir"/*.conf(N); do _q_names+=("${_q_f:t:r}"); done
      fi
      local _q_n _q_in="" _q_dir
      for _q_n in "${_q_names[@]}"; do
        _ccm_read "$_q_n" 2>/dev/null || continue
        case "${_ccm[type]}" in
          sub)   _q_in+="${_q_n}"$'\t'"sub"$'\t'"${_CCMORPH_PROFILE_DIRS}/${_q_n}"$'\t'$'\n' ;;
          token) _q_in+="${_q_n}"$'\t'"token"$'\t'"${_CCMORPH_PROFILE_DIRS}/${_q_n}"$'\t'"${_ccm[token]:-}"$'\n' ;;
          provider)
            if [[ "${_ccm[provider]:-}" == codex ]] && _ccm_provider_auth_dir "$_q_n" 2>/dev/null; then
              _q_dir="$REPLY"
              _q_in+="${_q_n}"$'\t'"codex"$'\t'"$_q_dir"$'\t'$'\n'
            else
              _q_in+="${_q_n}"$'\t'"gateway"$'\t'$'\t'"${_ccm[monthly_budget_usd]:-}"$'\n'
            fi
            ;;
          apikey)
            _q_in+="${_q_n}"$'\t'"gateway"$'\t'"${_ccm[base_url]:-}"$'\t'"${_ccm[monthly_budget_usd]:-}"$'\t'"${_ccm[api_key]:-}"$'\n'
            ;;
          *)
            _q_in+="${_q_n}"$'\t'"gateway"$'\t'$'\t'"${_ccm[monthly_budget_usd]:-}"$'\n'
            ;;
        esac
      done
      unset _ccm
      local -a _q_flags=()
      (( _q_json )) && _q_flags+=(--json)
      if (( ! _q_json )) && [[ -t 1 && -z "${NO_COLOR:-}" && "${TERM:-}" != dumb ]]; then
        _q_flags+=(--color)
      fi
      print -rn -- "$_q_in" | "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" quota "${_q_flags[@]}"
      ;;

    use)
      if [[ -z "${2:-}" ]]; then
        if [[ -n "$active" ]]; then
          print "${_ccm_bold}${active}${_ccm_reset}"
        else
          print "${_ccm_dim}No default set. Run: ccmorph use <profile>${_ccm_reset}"
        fi
        return 0
      fi

      # A bare `use` reports the current default, which is a useful answer and stays
      # as it is. `-i` is the explicit "let me choose a new one".
      if [[ "$2" == (-i|--interactive) ]]; then
        local _use_pick
        _use_pick=$(_ccm_menu_profile) || return 1
        [[ -n "$_use_pick" ]] || return 1
        set -- use "$_use_pick"
      fi

      _ccm_valid "${2:-}" || {
        _ccm_err "invalid name — use alphanumeric/underscore"
        return 1
      }

      _ccm_exists "$2" || {
        _ccm_err "no profile '$2'. Run: ccmorph list"
        return 1
      }

      _ccm_read "$2" || return 1

      _ccm_validate_loaded "$2" || {
        unset _ccm
        return 1
      }

      _ccm_setup_dir "$2" || {
        unset _ccm
        return 1
      }

      _ccm_write "$dir/active" "$2" || {
        unset _ccm
        _ccm_err "could not set default profile"
        return 1
      }

      unset _ccm
      print "${_ccm_accent}Default set${_ccm_reset} to ${_ccm_bold}$2${_ccm_reset}"
      ;;

    *)
      if [[ "$1" == -* ]]; then
        _ccm_err "unknown command '$1'. Launch flags go after the verb: ccmorph run $1"
      else
        _ccm_err "unknown command '$1'. Run: ccmorph --help"
        _ccm_exists "$1" && print -u2 -- "  To launch that profile: ccmorph run $1"
      fi
      _ccm_accounts_hint
      return 1
      ;;
  esac
}
