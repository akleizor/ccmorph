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

  local _dr_line _dr_out
  _dr_out=$(print -r -- "$key_value" | _ccm_util doctor-route "$name" "$base" "$adapter") || doctor_failed=1
  for _dr_line in "${(@f)_dr_out}"; do
    case "$_dr_line" in
      *$'\t'*) _ccm_doctor_status "${_dr_line%%$'\t'*}" "${_dr_line#*$'\t'}" ;;
      ?*) print -r -- "$_dr_line" ;;
    esac
  done
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
      _ccm_core cli list "${@:2}"
      return $?
      ;;

    add)
      _ccm_core cli add "${@:2}" || return $?
      # A new sign-in must not run under this shell's old token.
      unset CLAUDE_CODE_OAUTH_TOKEN
      return 0
      ;;

    login)
      _ccm_core cli login "${@:2}"
      return $?
      ;;

    rm)
      _ccm_core cli rm "${@:2}" || return $?
      # The removed profile's token must not outlive it in this shell.
      [[ "$active" == "${2:-}" ]] && unset CLAUDE_CODE_OAUTH_TOKEN
      return 0
      ;;

    cred)
      _ccm_core cli cred "${@:2}"
      return $?
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
      _ccm_core cli models "${@:2}"
      return $?
      ;;

    inspect)
      _ccm_core cli inspect "${@:2}"
      return $?
      ;;

    _menu)
      _ccm_core cli _menu "${@:2}"
      return $?
      ;;

    context)
      _ccm_core cli context "${@:2}"
      return $?
      ;;

    get|set)
      _ccm_core cli "$1" "${@:2}"
      return $?
      ;;

    config)
      _ccm_core cli config "${@:2}"
      return $?
      ;;

    mv)
      _ccm_core cli mv "${@:2}"
      return $?
      ;;

    hop)
      _ccm_core cli hop "${@:2}"
      return $?
      ;;

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

      # Read, validate and the type's credential env are one core call; the first
      # line names the type provisioning needs.
      local _env_out
      _env_out=$(_ccm_util profile-env "$_CCMORPH_DIR" "$_CCMORPH_PROFILE_DIRS" "$2" \
        "${(j:,:)_CCMORPH_PROFILE_ENV}" "${(j:,:)_CCMORPH_MODEL_MAPPING_ENV}") || {
        _ccm_err "${_env_out:-could not read profile '$2'}"
        return 1
      }
      typeset -gA _ccm=([type]="${${_env_out%%$'\n'*}#type=}")
      _ccm_setup_dir "$2" || {
        unset _ccm
        return 1
      }
      unset _ccm

      _ccm_prelaunch "$2" 2>/dev/null || :

      # Secure storage follows CLAUDE_CONFIG_DIR, so each profile's sign-in stays in
      # its own keychain entry.
      print -r -- "${_env_out#*$'\n'}"
      ;;

    quota)
      _ccm_core cli quota "${@:2}"
      return $?
      ;;

    use)
      _ccm_core cli use "${@:2}"
      return $?
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
