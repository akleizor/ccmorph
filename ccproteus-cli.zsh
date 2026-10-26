# ccproteus-cli.zsh — The ccproteus command: the config store, doctor, help, usage and the verb dispatcher.
# Sourced by ccproteus.sh after ccproteus-ui.zsh (see its Modules block); never run directly.

# ── Main ─────────────────────────────────────────────────────────────────────

# `-i` / `--interactive` is the ONE spelling of "ask me", at every site that takes a
# choice: a launch, `set <acct> model`, `set <acct> effort`. It is a flag, never a value,
# so it appears once per line and position says what it applies to.
#
# It replaced two earlier spellings, both of which were mistakes worth naming. `@menu` was
# a VALUE, so it had to be repeated per slot -- the documented launch line read
# `ccproteus @menu --model @menu`, one token twice for two different questions, with nothing
# saying which was which. `?` was an alias for it and worse than ambiguous: ccproteus runs as
# an ordinary executable, so the CALLER's shell expands a bare `?` before this process
# starts. Probed both failure modes -- in a clean directory zsh aborts with "no matches
# found: ?" and ccproteus never runs; with any one-character name in the cwd the glob MATCHES
# and `ccproteus set <acct> model ?` silently records that FILENAME as the default, reporting
# success. A `noglob` alias used to paper over that, which meant the fix existed only in
# shells that had sourced the library -- absent on exactly the fresh machines reading the
# docs that advertised it.
_ccs_config_store() {
  command python3 - "$1" "$_CCPROTEUS_DIR/$2.conf" "$(_ccs_profile_layer "$2")" "${3:-}" "$HOME/.claude/settings.json" "$_CCPROTEUS_LIB_DIR/model-window" "${_CCPROTEUS_MODEL_MAPPING_ENV[@]}" <<'PY'
import hashlib, importlib.machinery, importlib.util, json, os, sys, tempfile, traceback
from pathlib import Path

operation, conf_name, settings_name, key, shared_name, layout_name, *mapping_keys = sys.argv[1:]

# A launch that needs the generated settings hands this interpreter the argv of the
# run that generates them (one element per line), so the profile's settings are
# written by the same start that read the conf instead of a second one. Absent --
# every other caller, and any launch whose values cannot survive the newline join --
# this store behaves exactly as it always has. Both halves or neither: the caller
# that sends an argv sends the settings-compose to run it in, so a lone export --
# one that leaked into a launch from somewhere else -- cannot turn this read into a
# projection that prints nothing. The caller takes that stdout for the model
# defaults, so a silent projection is a launch started on last time's settings.
_materialize = (os.environ.get('_CCS_MATERIALIZE')
                if os.environ.get('_CCS_SC_PY') else None)
conf, settings = Path(conf_name), Path(settings_name)

def content(path):
    try:
        return path.read_bytes()
    except FileNotFoundError:
        return None

def fingerprint(parts):
    return hashlib.sha256(repr(parts).encode()).hexdigest()

def staged(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.configure.')
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
        return name
    except BaseException:
        os.unlink(name)
        raise

try:
    originals = [content(conf), content(settings)]
    if originals[0] is None:
        raise ValueError('profile no longer exists')
    if operation == 'snapshot':
        print(fingerprint(originals))
        sys.exit(0)
    doc = json.loads(originals[1]) if originals[1] is not None else {}
    if not isinstance(doc, dict) or not isinstance(doc.get('env', {}), dict):
        raise ValueError('settings.json and its env must be objects')
    lines = originals[0].decode().split('\n')
    if lines and lines[-1] == '':
        lines.pop()
    fields = dict(line.split('=', 1) for line in lines if '=' in line and not line.startswith('#'))
    defaults = {name: value for name, value in fields.items()
                if name in ('model', 'effort') or name.startswith('effort.')
                or name in {'env.' + item for item in mapping_keys}}
    if fields.get('model_defaults') != '1':
        shared_bytes = content(Path(shared_name))
        shared = json.loads(shared_bytes) if shared_bytes else {}
        def merge(base, override):
            result = dict(base)
            for name, value in override.items():
                result[name] = merge(result[name], value) if isinstance(result.get(name), dict) and isinstance(value, dict) else value
            return result
        if not isinstance(shared, dict) or not isinstance(shared.get('env', {}), dict):
            raise ValueError('shared settings and its env must be objects')
        native = merge(shared, doc)
        if not isinstance(native.get('modelSettings', {}), dict):
            raise ValueError('modelSettings must be an object')
        env = native.get('env', {})
        chosen = env.get('ANTHROPIC_MODEL') or native.get('model')
        if chosen:
            defaults.setdefault('model', chosen)
        if env.get('CLAUDE_CODE_EFFORT_LEVEL') or native.get('effortLevel'):
            defaults.setdefault('effort', env.get('CLAUDE_CODE_EFFORT_LEVEL') or native['effortLevel'])
        for model, settings_row in native.get('modelSettings', {}).items():
            if isinstance(settings_row, dict) and settings_row.get('effortLevel'):
                defaults.setdefault('effort.' + model, settings_row['effortLevel'])
        for name in mapping_keys:
            if env.get(name):
                defaults.setdefault('env.' + name, env[name])
    model_settings = {}
    for name, value in defaults.items():
        if not isinstance(value, str) or '=' in name or any(c in name + value for c in '\r\n') or any(c.isspace() for c in name):
            raise ValueError('model defaults must be single-line strings with valid keys')
        if name == 'effort' or name.startswith('effort.'):
            if value not in ('low', 'medium', 'high', 'xhigh', 'max') or name == 'effort.':
                raise ValueError('invalid saved effort default')
        if name.startswith('effort.'):
            model_settings[name[7:]] = {'effortLevel': value}
    projected = {'env': {name[4:]: value for name, value in defaults.items() if name.startswith('env.')},
                 'modelSettings': model_settings}
    for field, native_key in (('model', 'model'), ('effort', 'effortLevel')):
        if defaults.get(field):
            projected[native_key] = defaults[field]
    if operation == 'get':
        print(defaults.get(key, ''))
        sys.exit(0)
    if operation == 'model-settings':
        print(json.dumps(projected))
        sys.exit(0)
    expected = os.environ.get('_CCS_CONFIG_SNAPSHOT')
    if expected and expected != fingerprint(originals):
        raise ValueError('profile changed while configuring; nothing saved')
    updates = {}
    for name in ('model', 'effort'):
        value = os.environ.get('_CCS_CONFIG_' + name.upper(), '')
        if not value:
            continue
        if name == 'model' and (value.startswith(('-', '@')) or value == '?' or any(c.isspace() for c in value)):
            raise ValueError('model must be a nonempty model id, not a flag or picker token')
        if name == 'effort' and value not in ('low', 'medium', 'high', 'xhigh'):
            raise ValueError('effort must be low, medium, high or xhigh')
        updates[name] = value
    defaults.update(updates)
    if operation not in ('set', 'configure', 'seed-model-defaults', 'prepare-launch', 'migrate-native'):
        raise ValueError('unsupported configuration operation')
    if conf.is_symlink() or settings.is_symlink():
        raise ValueError('model defaults require profile-local files')
    changes = dict(defaults, model_defaults='1')
    if operation == 'configure':
        kind = os.environ['_CCS_CONFIG_TYPE']
        if kind == 'apikey':
            changes['base_url'] = os.environ['_CCS_CONFIG_ENDPOINT']
            changes['api_key'] = os.environ['_CCS_CONFIG_CREDENTIAL']
        elif kind == 'token':
            changes['token'] = os.environ['_CCS_CONFIG_CREDENTIAL']
    if any('\r' in str(v) or '\n' in str(v) for v in changes.values()):
        raise ValueError('configuration values cannot contain CR/LF')
    kept = [line for line in lines if line.partition('=')[0] not in changes]
    kept.extend(f'{name}={value}' for name, value in changes.items() if value)
    # One layout for every writer: model-window owns it (CONF_SECTIONS).
    spec = importlib.util.spec_from_loader("conf_layout", importlib.machinery.SourceFileLoader("conf_layout", layout_name))
    layout = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(layout)
    proposed = layout.canonical_conf('\n'.join(kept)).encode()
    targets = [(conf, originals[0], proposed)] if proposed != originals[0] else []
    if operation == 'prepare-launch':
        # The conf owns defaults (including deliberate absence) after seeding.
        # Keep native discovery without reviving preferences from prior sessions.
        for name in ('model', 'effortLevel'):
            doc.pop(name, None)
        rows = doc.get('modelSettings', {})
        if not isinstance(rows, dict):
            raise ValueError('modelSettings must be an object')
        for row in rows.values():
            if isinstance(row, dict):
                row.pop('effortLevel', None)
        for name in ('ANTHROPIC_MODEL', 'CLAUDE_CODE_EFFORT_LEVEL', *mapping_keys):
            doc.get('env', {}).pop(name, None)
        if originals[1] is not None and doc != json.loads(originals[1]):
            targets.append((settings, originals[1], (json.dumps(doc, indent=2, ensure_ascii=False) + '\n').encode()))
    pending, applied = [], []
    try:
        for path, before, after in targets:
            pending.append((path, before, after, staged(path, after)))
        if [content(conf), content(settings)] != originals:
            raise ValueError('profile changed while configuring; nothing saved')
        for path, before, after, temp in pending:
            if content(path) != before:
                raise ValueError(f'{path.name} changed while saving')
            os.replace(temp, path)
            applied.append((path, before, after))
    except BaseException:
        for path, before, after in reversed(applied):
            if content(path) != after:
                print(f'ccproteus: {path.name} changed externally; left untouched during rollback', file=sys.stderr)
                continue
            if before is None:
                path.unlink()
            else:
                temp = staged(path, before)
                try:
                    os.replace(temp, path)
                finally:
                    if os.path.exists(temp): os.unlink(temp)
        raise
    finally:
        for _, _, _, temp in pending:
            if os.path.exists(temp): os.unlink(temp)
    if operation == 'prepare-launch':
        if not _materialize:
            print(json.dumps(projected))
    elif operation == 'seed-model-defaults' and key == 'model':
        print(defaults.get('model', ''))
except (OSError, ValueError) as exc:
    print(f'ccproteus: could not save/read configuration: {exc}', file=sys.stderr)
    sys.exit(1)

# Outside the handler above on purpose: settings-compose's own failures are the
# launch's to act on, and its statuses are shifted past the store's -- 10 + rc, so
# exit 1 still means "the store failed, do not start". Nothing here may end in a
# silent success: a status no launcher knows is a refused projection, not a launch.
if _materialize and operation == 'prepare-launch':
    rc = 1
    try:
        _globals = {}
        # Loading settings-compose is inside the handler because it is one of its
        # failures: a file that cannot be read or run at all -- a restricted tree
        # can leave one behind -- costs the launch its settings, never its start,
        # exactly as it did when this ran as a process of its own.
        exec(os.environ['_CCS_SC_PY'], _globals)
        argv = _materialize.split('\n') + ['--saved', json.dumps(projected)]
        sys.argv[0] = os.path.join(os.environ['_CCS_LIB'], 'settings-compose')
        rc = _globals['_sc'].main(argv)
    except SystemExit as exc:
        # The status the process would have exited with: an int is that status,
        # None -- a bare sys.exit() -- is success, anything else a failed run.
        rc = exc.code if isinstance(exc.code, int) else (0 if exc.code is None else 1)
    except BaseException:
        traceback.print_exc()
        rc = 1
    # The same rule for main()'s return value, so either way of leaving
    # settings-compose reaches this exit as a status the launcher knows.
    if not isinstance(rc, int):
        rc = 0 if rc is None else 1
    sys.exit(0 if rc == 0 else 10 + rc)
PY
}

_ccs_doctor_status() {
  emulate -L zsh
  local kind="$1"; shift
  local symbol color
  case "$kind" in
    OK) symbol='✓'; color="$_ccs_accent" ;; FAIL) symbol='✗'; color="$_ccs_red" ;;
    WARN) symbol='!'; color="$_ccs_yellow" ;; INFO) symbol='ℹ'; color="$_ccs_focus" ;; SKIP) symbol='–'; color="$_ccs_gray" ;;
    *) print -r -- "$kind $*"; return ;;
  esac
  if [[ -t 1 && -z "${NO_COLOR:-}" && "${TERM:-}" != dumb ]]; then
    printf '%s%s%s %s\n' "$color" "$symbol" "$_ccs_reset" "$*"
  elif [[ -t 1 ]]; then print -r -- "$symbol $*"; else print -r -- "$kind $*"; fi
}

_ccs_doctor() {
  emulate -L zsh
  local name="${1:-}" conf base key key_value type model adapter settings profile_dir
  local doctor_failed=0
  if [[ -z "$name" ]]; then
    [[ -f "$_CCPROTEUS_DIR/active" && ! -L "$_CCPROTEUS_DIR/active" ]] && name=$(<"$_CCPROTEUS_DIR/active")
  fi
  if [[ -z "$name" ]]; then
    _ccs_doctor_status 'FAIL' 'profile: no profile selected (use: ccproteus doctor <profile>)'
    return 1
  fi
  _ccs_valid "$name" || { _ccs_doctor_status 'FAIL' 'profile: invalid profile name'; return 1; }
  conf="$_CCPROTEUS_DIR/$name.conf"
  if [[ ! -f "$conf" || -L "$conf" ]]; then
    _ccs_doctor_status "FAIL" "profile: missing or symlinked config ($conf)"
    return 1
  fi

  # Use the canonical parser without its permission repair. This keeps doctor’s
  # schema and duplicate-key checks aligned with launch while preserving modes.
  _ccs_read "$name" 1 || {
    _ccs_doctor_status 'FAIL' 'config: invalid profile schema (launch would reject it)'
    unset _ccp
    return 1
  }
  type="${_ccp[type]:-}"; base="${_ccp[base_url]:-}"; model="${_ccp[model]:-}"
  adapter="${_ccp[adapter]:-}"
  if [[ -n "${_ccp[api_key]:-}" || -n "${_ccp[token]:-}" ]]; then
    key=present; key_value="${_ccp[api_key]:-${_ccp[token]:-}}"
  fi
  unset _ccp
  [[ -n "$type" ]] || { _ccs_doctor_status 'FAIL' 'config: missing type'; return 1; }
  case "$type" in
    apikey|sub|token|provider) ;;
    *) _ccs_doctor_status "FAIL" "config: unsupported profile type '$type'"; doctor_failed=1 ;;
  esac
  if [[ "$type" == apikey && -z "$key" ]]; then
    _ccs_doctor_status 'FAIL' 'config: apikey profile has no credential (value not displayed)'
    doctor_failed=1
  fi
  _ccs_doctor_status "OK" "profile: $name ($type)"
  _ccs_doctor_status "OK" "ccproteus executable: ${_CCPROTEUS_LIB_DIR}/ccproteus.sh"
  if whence -p claude >/dev/null 2>&1; then
    local claude_bin
    claude_bin=$(whence -p claude 2>/dev/null) || claude_bin=''
    if [[ -n "$claude_bin" ]]; then
      _ccs_doctor_status "OK" "claude executable: $claude_bin"
    else
      _ccs_doctor_status 'INFO' 'claude command: provided by shell function/alias (path not inspectable)'
    fi
  else
    _ccs_doctor_status 'FAIL' 'claude executable: not found on PATH'
    doctor_failed=1
  fi
  profile_dir="$_CCPROTEUS_PROFILE_DIRS/$name"
  if [[ -d "$profile_dir" ]]; then
    _ccs_doctor_status "OK" "profile home: $profile_dir"
  else
    _ccs_doctor_status "FAIL" "profile home: missing ($profile_dir)"
    doctor_failed=1
  fi
  settings=$(_ccs_profile_layer "$name")
  if [[ -f "$settings" ]]; then
    if command python3 - "$HOME/.claude/settings.json" "$settings" <<'PY' >/dev/null 2>&1
import json, sys
for path in sys.argv[1:]:
    try:
        with open(path, encoding='utf-8') as fh: doc=json.load(fh)
    except FileNotFoundError: continue
    if not isinstance(doc, dict): raise SystemExit(1)
PY
    then
      _ccs_doctor_status "OK" "profile settings: valid layered JSON ($settings overrides canonical settings)"
      local effective_model
      effective_model=$(_ccs_account_model "$name") || effective_model=''
      if [[ -n "$effective_model" ]]; then
        _ccs_doctor_status 'INFO' "effective model: $effective_model (profile default)"
      else
        _ccs_doctor_status 'INFO' 'effective model: Claude Code default (no saved model)'
      fi
    else
      _ccs_doctor_status 'FAIL' "profile settings: invalid JSON or non-object ($settings)"
      doctor_failed=1
    fi
  else
    _ccs_doctor_status 'SKIP' "profile settings: missing ($settings)"
  fi
  [[ -n "$key" ]] && _ccs_doctor_status 'OK' 'credentials: present (value hidden)' || _ccs_doctor_status 'SKIP' 'credentials: none stored'

  if [[ -n "$base" ]]; then
    local invalid_url=0
    if [[ "$base" != (http|https)://* || "$base" == *'?'* || "$base" == *'#'* ]]; then
      _ccs_doctor_status 'FAIL' 'config: base_url must be an http(s) URL without query or fragment'
      doctor_failed=1; invalid_url=1
    fi
    local safe_url http_code payload models_count
    safe_url=$(_ccs_redact_url "$base")
    print -r -- "endpoint: $safe_url"
    if (( invalid_url )); then
      _ccs_doctor_status 'SKIP' 'gateway: invalid endpoint (no request sent)'
    elif [[ -z "$key" ]]; then
      _ccs_doctor_status 'SKIP' 'gateway: no API credential for metadata request'
    else
      # Probe the same metadata route a launch uses, through the same ambient
      # HTTPS_PROXY a launch would inherit — doctor must answer for the route the
      # user actually has, not for a direct one they may not.
      local _doctor_url
      _ccs_api_url "$base" "/v1/models?limit=1000"
      _doctor_url="$REPLY"
      payload=$(_ccs_curl_keyed both "$key_value" -sS --connect-timeout 2 --max-time 5 \
        -w $'\\n__CCPROTEUS_STATUS__%{http_code}' \
        "$_doctor_url" 2>/dev/null) || payload="__CCPROTEUS_STATUS__000"
      http_code="${payload##*__CCPROTEUS_STATUS__}"; payload="${payload%%__CCPROTEUS_STATUS__*}"; payload="${payload%$'\\n'}"
      if [[ "$http_code" == 2?? ]]; then
        models_count=$(print -r -- "$payload" | command python3 -c "import json,sys; d=json.load(sys.stdin); assert isinstance(d,dict) and isinstance(d.get(\"data\"),list); print(len(d[\"data\"]))" 2>/dev/null) || models_count=''
        if [[ -n "$models_count" ]]; then
          _ccs_doctor_status "OK" "gateway: reachable (HTTP $http_code); models: $models_count"
        else
          _ccs_doctor_status "FAIL" "gateway: malformed model listing (HTTP $http_code)"
          doctor_failed=1
        fi
      else
        if [[ "$http_code" == 000 ]]; then
          _ccs_doctor_status 'SKIP' 'gateway: endpoint did not answer (no changes made)'
        else
          _ccs_doctor_status "FAIL" "gateway: unreachable or rejected (HTTP $http_code)"
          doctor_failed=1
        fi
      fi
    fi
  else
    _ccs_doctor_status 'SKIP' 'gateway: profile has no base_url (native endpoint)'
  fi
  if [[ "$adapter" == true ]]; then
    _ccs_doctor_status 'INFO' 'adapter: configured=true (not started by read-only doctor)'
  elif [[ -n "$base" ]]; then
    _ccs_doctor_status 'INFO' 'adapter: automatic compatibility may apply at launch (not started by doctor)'
  else
    _ccs_doctor_status 'SKIP' 'adapter: native profile path (not started by doctor)'
  fi
  if [[ -n "$base" && -x "$_CCPROTEUS_LIB_DIR/ccproteus-adapter" ]]; then
    local running_adapter
    running_adapter=$(command python3 - "$name" <<'PY' 2>/dev/null
import os, re, shlex, subprocess, sys
profile = sys.argv[1]
try:
    rows = subprocess.run(['ps', '-axo', 'pid=,ppid=,command='], text=True,
                          capture_output=True, timeout=1, check=True).stdout.splitlines()
except Exception:
    print('__UNKNOWN__'); raise SystemExit(0)
items = {}
for row in rows:
    fields = row.strip().split(None, 2)
    if len(fields) != 3: continue
    try: pid, ppid = int(fields[0]), int(fields[1]); args = shlex.split(fields[2])
    except (ValueError, OSError): continue
    items[pid] = (ppid, args)
def executable_index(args):
    if not args:
        return -1
    if re.fullmatch(r"python(?:\d+(?:\.\d+)*)?", os.path.basename(args[0]), re.I):
        return 1 if len(args) > 1 else -1
    return 0

for pid, (ppid, args) in items.items():
    adapter_index = executable_index(args)
    if adapter_index >= 0 and os.path.basename(args[adapter_index]) == 'ccproteus-adapter' and adapter_index + 1 < len(args) and args[adapter_index + 1] == profile:
        children = []
        for child, (child_ppid, child_args) in items.items():
            if child_ppid != pid or not child_args: continue
            child_index = executable_index(child_args)
            if child_index >= 0 and os.path.basename(child_args[child_index]) in ('cliproxyapi', 'gateway-shim'):
                base = os.path.basename(child_args[child_index])
                children.append(f'{child}:{base}')
        print(str(pid) + '\t' + ','.join(children)); raise SystemExit(0)
PY
) || running_adapter=''
    if [[ "$running_adapter" == __UNKNOWN__ ]]; then
      _ccs_doctor_status 'SKIP' 'adapter: status unknown (process inspection unavailable; not started by doctor)'
    elif [[ -n "$running_adapter" ]]; then
      local adapter_pid="${running_adapter%%$'\t'*}" adapter_children="${running_adapter#*$'\t'}"
      if [[ -z "$adapter_children" ]]; then
        _ccs_doctor_status 'SKIP' "adapter: supervisor found (pid $adapter_pid) but no known listener child in process snapshot"
      else
        local child_spec child_pid child_name listener
        for child_spec in ${(s:,:)adapter_children}; do
          child_pid="${child_spec%%:*}"; child_name="${child_spec#*:}"
          listener=$(command python3 - "$child_pid" <<'PY' 2>/dev/null
import subprocess, sys
try:
    out = subprocess.run(['lsof', '-nP', '-a', '-p', sys.argv[1], '-iTCP', '-sTCP:LISTEN'], text=True, capture_output=True, timeout=1, check=True).stdout.splitlines()
except Exception:
    print('__UNKNOWN__'); raise SystemExit(0)
for row in out[1:]:
    fields = row.split()
    if fields: print(fields[-2]); break
PY
) || listener='__UNKNOWN__'
          if [[ "$listener" == __UNKNOWN__ ]]; then
            _ccs_doctor_status "SKIP" "adapter: $child_name child listener status unknown (pid $child_pid)"
          elif [[ -n "$listener" ]]; then
            _ccs_doctor_status "OK" "adapter: $child_name listener $listener (pid $child_pid; read-only check)"
          else
            _ccs_doctor_status "WARN" "adapter: $child_name child running without TCP listener (pid $child_pid)"
          fi
        done
      fi
    else
      _ccs_doctor_status 'INFO' 'adapter: not running (not started by read-only doctor)'
    fi
  fi
  # Do not classify arbitrary cache names as stale: ownership is only established
  # by ccproteus's provisioning manifest, and doctor never provisions or repairs.
  _ccs_doctor_status 'SKIP' 'generated state: ownership/conflict not established (no changes made)'
  return $doctor_failed
}

# Short overview first; details are available without loading profiles or running
# setup. Keep Claude's own `run ... --help` pass-through unchanged.
# Help follows gh: only section headings carry weight; rows stay plain so the
# eye lands on structure first, then scans a calm two-column table.
# One two-column grid for help and inspect: _ccs_grid WIDTH KEY TEXT. A key too
# wide for its column gets its own line, the text starting under the column.
_ccs_grid() {
  if (( ${#2} > $1 )); then
    printf '  %s\n  %-*s %s\n' "$2" "$1" '' "$3"
  else
    printf '  %-*s %s\n' "$1" "$2" "$3"
  fi
}
_ccs_help() { _ccs_grid 32 "$@"; }

_ccs_usage() {
  local topic="${1:-}" usage="" detail=""
  case "$topic" in
    '')
      print 'ccproteus — Claude Code profile switcher'
      print '\nUsage: ccproteus <command> [arguments]'
      print "\n${_ccs_bold}Launch:${_ccs_reset}"
      _ccs_help "ccproteus run" "Launch the default profile"
      _ccs_help "ccproteus run <profile>" "Use a profile for this session"
      _ccs_help "ccproteus run -i" "Choose a profile interactively"
      print "\n${_ccs_bold}Profiles:${_ccs_reset}"
      _ccs_help "ccproteus add <profile>" "Add an account or API key"
      _ccs_help "ccproteus list" "See profiles and the default"
      _ccs_help "ccproteus use <profile>" "Set the default; do not launch"
      _ccs_help "ccproteus config [profile]" "Change model, reasoning, or connection"
      print "\n${_ccs_bold}Troubleshooting:${_ccs_reset}"
      _ccs_help "ccproteus inspect [profile]" "Show effective settings; no secrets"
      _ccs_help "ccproteus doctor [profile]" "Check why a profile is not working"
      print '\nStart here: ccproteus add work'
      print 'More commands: ccproteus help all'
      print 'Command details: ccproteus help <command>'
      print 'Version: ccproteus --version'
      return 0
      ;;
    all)
      _ccs_usage
      print "\n${_ccs_bold}More commands:${_ccs_reset}"
      _ccs_help "ccproteus models <profile>" "List models from an API-key endpoint"
      _ccs_help "ccproteus get <profile> <model|effort>" "Read a saved default"
      _ccs_help "ccproteus set <profile> <model|effort> [value]" "Save a default; omit value to choose"
      _ccs_help "ccproteus login <profile>" "Sign in again to a provider account"
      _ccs_help "ccproteus mv <old> <new>" "Rename a profile"
      _ccs_help "ccproteus rm <profile>" "Delete a profile and its private sign-in state"
      _ccs_help "ccproteus hop <profile>" "Switch a running session; use inside Claude"
      _ccs_help "ccproteus cred <profile>" "Print a stored secret (sensitive)"
      print "\n${_ccs_bold}Advanced troubleshooting (normally automatic):${_ccs_reset}"
      _ccs_help "ccproteus probe [profile]" "Refresh context-limit metadata; no inference"
      _ccs_help "ccproteus context <profile> <model> [tokens|auto]" "Override or reset one model context limit"
      return 0
      ;;
    run)
      usage='ccproteus run [profile] [-i] [-- <claude arguments...>]'
      detail='Omit the profile to use your default. -i asks for a profile if needed,
then a model for routed profiles. Explicit --model <id> skips the model picker.
Choices apply to this session only; saved defaults are unchanged.

Examples:
  ccproteus run work
  ccproteus run work --model sonnet
  ccproteus run work -- --continue

Native Claude help: ccproteus run <profile> --help' ;;
    add)
      usage='ccproteus add <profile>'
      detail='Choose how to sign in. Only the selected method asks for credentials:
  Subscription: sign in to Claude with your browser.
  API key: credential presented to Anthropic or a gateway (apikey).
  OAuth token: paste an existing Claude token.
  Provider: browser login, when a supported provider bridge is installed.

Credentials are hidden while typing. Ctrl-C cancels setup.' ;;
    config)
      usage='ccproteus config [profile]'
      detail='Choose what to change: model and reasoning, or connection/sign-in details.
Only settings relevant to that task and profile are shown.
Keep the current defaults or select new values. Changes save when you finish;
there is no extra confirmation. Esc cancels a picker without saving.
Changes affect new sessions.

For a single setting: ccproteus set <profile> model|effort [value]' ;;
    list) usage='ccproteus list'; detail='List saved profiles. * marks the default used by ccproteus run.' ;;
    use) usage='ccproteus use [profile|-i]'; detail='Set the default without launching. No argument shows the current default.' ;;
    inspect) usage='ccproteus inspect [profile|-i]'; detail='Show effective settings and their sources, never the stored credential.
Omit the profile to inspect the default; -i opens the profile picker.' ;;
    doctor) usage='ccproteus doctor [profile|-i]'; detail='Read-only checks of the executable, endpoint and generated state.
Omit the profile to check the default. Does not repair or launch a session.' ;;
    models) usage='ccproteus models [profile] [--filtered|--filtered-json]'; detail='List models from an API-key endpoint. Omit the profile to choose one.
--filtered applies profile exclusions; --filtered-json returns JSON.' ;;
    get|set)
      usage="ccproteus $topic [profile] [model|effort]"
      [[ "$topic" == set ]] && usage+=' [value]'
      detail='Read or save a default for new sessions. Missing arguments open pickers.
Use explicit arguments in scripts. Values are printed to stdout.
Effort: low, medium, high, xhigh. Available models come from the profile.' ;;
    login) usage='ccproteus login [profile|-i]'; detail='Sign in again to a provider-backed profile using its browser login.
To replace an API key or token instead: ccproteus config <profile>' ;;
    mv) usage='ccproteus mv <old> <new>'; detail='Rename a profile and its private state. Existing destinations are refused.' ;;
    rm) usage='ccproteus rm <profile>'; detail='Delete this profile, its saved credential and private sign-in state.
Shared conversation history is kept. This cannot be undone.
Leftover private state from a missing profile requires confirmation.' ;;
    hop) usage='ccproteus hop <profile>'; detail='Inside a running Claude session, type: ! ccproteus hop <profile>
Then exit Claude to complete the switch. Keeps model and effort when supported.
This command does not work from an unrelated terminal.' ;;
    cred) usage='ccproteus cred [profile]'; detail='Print the stored API key or token to stdout. This reveals a secret.
Only profiles with a stored key or token are offered. Name one in scripts.' ;;
    probe) usage='ccproteus probe [profile] [options]'; detail='Refresh published context metadata; no inference requests by default.
Paid inference requires --allow-inference, --profile and --model explicitly.
Full probe options: ccproteus probe --help' ;;
    context) usage='ccproteus context <profile> <model> [tokens|auto]'; detail='Read a context limit, set a known token count, or restore automatic discovery.
Normally unnecessary: compatibility and context limits are automatic.' ;;
    *) _ccs_err "unknown help topic '$topic'. Run: ccproteus help all"; return 2 ;;
  esac
  print -r -- "Usage: $usage"
  print
  print -r -- "$detail"
  print '\nFixed choices: ↑/↓ move, Enter selects one, Esc cancels. No search box.'
  print 'Profile/model catalogues: type to search with fzf; numbers without it.'
}

ccproteus() {
  emulate -L zsh
  # Shell snapshots can retain this public function without its private library.
  # The executable already owns loading and path resolution; do not duplicate it.
  if (( ! $+functions[_ccs_secure_dirs] )) || [[ -z "${_CCPROTEUS_DIR:-}" ]]; then
    command ccproteus "$@"
    return $?
  fi

  case "${1:-}" in
    ''|-h|--help|help)
      (( $# <= 2 )) || { _ccs_err 'usage: ccproteus help [command|all]'; return 2; }
      _ccs_usage "${2:-}"
      return $?
      ;;
    -v|-V|--version|version) print "ccproteus ${_CCPROTEUS_VERSION}"; return 0 ;;
    # The names a launch never inherits from the shell (see _CCPROTEUS_BACKEND_ENV),
    # one per line, for the Remote Control add-on. A pure constant: answered before
    # any state migration or chmod, so the query never touches the caller's config.
    _backend_env) print -l -- "${_CCPROTEUS_BACKEND_ENV[@]}"; return 0 ;;
  esac
  # Exact command-help requests only: run/probe retain their native help, and
  # `set profile model --help` remains an invalid value rather than hiding a typo.
  if (( $# == 2 )) && [[ "$2" == (-h|--help) && "$1" != (run|probe|"$_ccs_retired_verb") ]]; then
    _ccs_usage "$1"
    return $?
  fi

  local dir="$_CCPROTEUS_DIR"
  local active=""

  # Keep diagnostics genuinely read-only: bypass active-file chmod, directory
  # creation, migration, and all launch provisioning.
  if [[ "${1:-}" == doctor ]]; then
    shift
    (( $# <= 1 )) || { _ccs_err 'usage: ccproteus doctor [profile]'; return 2; }
    local _CCPROTEUS_PROFILE_DIRS="$_CCPROTEUS_PROFILE_DIRS"
    if [[ ! -e "$_CCPROTEUS_PROFILE_DIRS" && ! -L "$_CCPROTEUS_PROFILE_DIRS" &&
          -d "$HOME/.claude-profiles" && ! -L "$HOME/.claude-profiles" ]]; then
      _CCPROTEUS_PROFILE_DIRS="$HOME/.claude-profiles"
    fi
    if [[ "${1:-}" == (-i|--interactive) ]]; then
      local _doctor_profile
      _doctor_profile=$(_ccs_menu_profile) || return 1
      set -- "$_doctor_profile"
    fi
    _ccs_doctor "$@"
    return $?
  fi

  _ccs_secure_dirs || {
    _ccs_err "cannot secure ccproteus directories"
    return 1
  }

  if _ccs_read_active; then
    active="$REPLY"
  fi

  case "$1" in

    run)
      shift
      case "${1:-}" in
        -a|--profile|--profile=*)
          _ccs_err "launch profile is positional: ccproteus run <profile>"
          return 2
          ;;
      esac
      local _run_has_interactive=0 _CCPROTEUS_NATIVE_ARGS=1 _CCPROTEUS_INTERACTIVE=0
      if [[ "${1:-}" == (-i|--interactive) ]]; then
        _run_has_interactive=1
        _CCPROTEUS_INTERACTIVE=1
        shift
      fi

      local _run_account=""
      if (( $# )) && [[ "$1" != -* ]]; then
        if _ccs_exists "$1"; then
          _run_account="$1"
          shift
          if [[ "${1:-}" == (-i|--interactive) ]]; then
            _CCPROTEUS_INTERACTIVE=1
            shift
          fi
        elif ! _ccs_is_claude_subcommand "$1"; then
          _ccs_valid "$1" || { _ccs_err "invalid profile name '$1'"; return 2; }
          _ccs_err "no profile '$1'. Run: ccproteus list"
          return 1
        fi
      fi

      if [[ -z "$_run_account" && -z "$active" ]] && (( ! _run_has_interactive )); then
        _ccs_err "no default profile set — run: ccproteus use <profile> or ccproteus run -i"
        _ccs_accounts_hint
        return 1
      fi
      # `--` is run's own separator (usage: run [profile] [-i]
      # [-- args]). Forwarded, it makes Claude Code read every later flag as
      # prompt text -- but only the OUTER one is run's. Every `--` after the
      # first argument Claude Code owns is Claude Code's own (see
      # _ccs_run_separator_index), so a native command keeps its delimiter.
      local -a _run_args=("$@")
      local _run_sep=0
      _ccs_run_separator_index "${_run_args[@]}"
      _run_sep=$REPLY
      (( _run_sep > 0 )) && _run_args[_run_sep]=()
      local _run_sub=0
      _ccs_launch_has_claude_subcommand "${_run_args[@]}" && _run_sub=1
      _CCPROTEUS_LAUNCH_PROFILE="$_run_account" CCPROTEUS_SUBCOMMAND_LAUNCH=$_run_sub _ccs_launch_driver "${_run_args[@]}"
      ;;

    start)
      _ccs_err "unknown command 'start'. Run: ccproteus run${2:+ ${@:2}}"
      return 1
      ;;

    default)
      _ccs_err "unknown command 'default'. Run: ccproteus use${2:+ ${@:2}}"
      return 1
      ;;

    show)
      _ccs_err "unknown command 'show'. Run: ccproteus inspect${2:+ ${@:2}}"
      return 1
      ;;

    creds|secret)
      _ccs_err "unknown command '$1'. Run: ccproteus cred${2:+ ${@:2}}"
      return 1
      ;;

    list)
      local n f
      local found=0

      for f in "$dir"/*.conf(N); do
        n="${f:t:r}"

        if ! _ccs_read "$n"; then
          print -u2 "ccproteus: skipping invalid profile '$n'"
          continue
        fi

        local marker=" "
        local name_fmt="${n}"

        if [[ "$n" == "$active" ]]; then
          marker="${_ccs_accent}*${_ccs_reset}"
          name_fmt="${_ccs_bold}${n}${_ccs_reset}"
        fi

        local label="${_ccs_dim}${_ccp[type]}${_ccs_reset}"
        # Declared before the branches: `local x; x=$(...)` inside an if/elif chain
        # emits the assignment to stdout in this context, which corrupts the listing.
        local _lm=""

        if [[ "${_ccp[type]}" == sub &&
              -n "${_ccp[email]:-}" ]]; then
          label="${_ccs_cyan}sub${_ccs_reset}${_ccs_dim}: ${_ccp[email]}${_ccs_reset}"
        elif [[ "${_ccp[type]}" == token ]]; then
          label="${_ccs_yellow}token${_ccs_reset}"
        elif [[ "${_ccp[type]}" == apikey ]]; then
          _lm=$(_ccs_account_model "$n") || _lm=""
          label="${_ccs_cyan}apikey${_ccs_reset}${_ccs_dim}: ${_ccp[base_url]:-api.anthropic.com}${_lm:+ → ${_lm}}${_ccs_reset}"
        elif [[ "${_ccp[type]}" == provider ]]; then
          _lm=$(_ccs_account_model "$n") || _lm=""
          _ccs_provider_label "${_ccp[provider]:-provider}"
          label="${_ccs_accent}${REPLY}${_ccs_reset}${_lm:+${_ccs_dim} → ${_lm}${_ccs_reset}}"
        fi

        print " ${marker} ${name_fmt} (${label})"
        found=1
      done

      (( found )) ||
        print "${_ccs_dim}  No profiles. Run: ccproteus add <name>${_ccs_reset}"

      unset _ccp
      ;;

    add)
      if [[ -z "${2:-}" && -t 0 ]]; then
        local _new_name
        ccs_ui_input 'New profile name: ' || return 1
        _new_name="$REPLY"
        set -- add "$_new_name"
      fi
      _ccs_valid "${2:-}" || {
        _ccs_err "invalid name — use alphanumeric/underscore"
        return 1
      }

      _ccs_exists "$2" && {
        _ccs_err "'$2' already exists. Remove first: ccproteus rm $2"
        return 1
      }

      if [[ -e "$_CCPROTEUS_PROFILE_DIRS/$2" || -L "$_CCPROTEUS_PROFILE_DIRS/$2" ||
            -e "$dir/auth/$2" || -L "$dir/auth/$2" ]]; then
        _ccs_err "leftover private state for '$2'; run ccproteus rm $2 to review and remove it, or choose another name"
        return 1
      fi

      local -a _add_providers=()
      if _ccs_provider_list 2>/dev/null; then
        _add_providers=("${(@)REPLY:#claude}")
      fi

      local choice _prov _type_rows=$'sub\tClaude account\tSign in with your browser\ntoken\tClaude token\tPaste an existing OAuth token\napikey\tAPI key\tAnthropic or a gateway'
      for _prov in "${_add_providers[@]}"; do
        _ccs_provider_label "$_prov"
        _type_rows+=$'\n'"$_prov"$'\t'"$REPLY"$'\t'"Sign in with your browser"
      done
      if [[ -t 0 ]]; then
        choice=$(ccs_ui_select native "Sign-in method — $2" "$_type_rows") || return 1
      else
        ccs_ui_list "Sign-in method — $2" "$_type_rows"
        ccs_ui_input '> ' || return 1
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
          _ccs_write \
            "$dir/$2.conf" \
            "type=sub" || {
            _ccs_err "could not save profile '$2'"
            return 1
          }

          # Not _ccs_read: the email comes from the login below, so the validator
          # would reject this placeholder before the browser ever opens.
          # Provisioning only needs the type.
          typeset -gA _ccp=([type]=sub)
          if ! _ccs_setup_dir "$2"; then
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          fi

          local _profile_dir="${_CCPROTEUS_PROFILE_DIRS}/$2"

          # Same scrub as _ccs_logged_email, for the same reason: with an
          # ANTHROPIC_API_KEY exported, the CLI can satisfy itself from that key
          # instead of running the browser flow this profile type exists to run.
          # A subshell per call keeps the caller's environment intact.
          _ccs_claude_auth "$_profile_dir" logout &>/dev/null || {
            _ccs_err "logout failed"
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          }

          print "${_ccs_dim}Opening your browser to sign in — Ctrl-C to cancel.${_ccs_reset}"
          _ccs_claude_auth "$_profile_dir" login || {
            _ccs_err "login failed"
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          }

          _ccs_logged_email "$_profile_dir" || {
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          }

          # Blank is not clean. _ccs_clean_value only rejects CR/LF, so an empty
          # email passed it — and an empty email is exactly what a key-shaped
          # auth answer produces, which is how a `type=sub` profile with no
          # address at all came to be written and marked active.
          if [[ -z "$_ccs_email" ]] || ! _ccs_clean_value "$_ccs_email"; then
            _ccs_err "no subscription profile found after login — nothing was saved"
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          fi

          _ccs_write \
            "$dir/$2.conf" \
            "type=sub"$'\n'"email=$_ccs_email" || {
            _ccs_err "could not save profile '$2'"
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          }

          _ccs_write "$dir/active" "$2" || {
            unset _ccp
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            _ccs_err "could not set default profile"
            return 1
          }

          unset CLAUDE_CODE_OAUTH_TOKEN
          unset _ccp

          print "${_ccs_accent}Added${_ccs_reset} $2 (sub${_ccs_email:+: $_ccs_email})"
          ;;

        2|token|oauth_token|oauth-token)
          local token

          ccs_ui_input "Paste token for $2 (hidden): " secret || return 1
          token="$REPLY"; REPLY=''

          [[ -n "$token" ]] || {
            _ccs_err "empty token"
            return 1
          }

          _ccs_clean_value "$token" || {
            _ccs_err "token contains CR/LF"
            return 1
          }

          _ccs_write \
            "$dir/$2.conf" \
            "type=token"$'\n'"token=$token" || {
            _ccs_err "could not save profile '$2'"
            unset token
            return 1
          }

          if ! _ccs_read "$2" || ! _ccs_setup_dir "$2"; then
            unset _ccp token
            _ccs_remove_profile_dir "$2"
            _ccs_remove_file "$dir/$2.conf"
            return 1
          fi

          unset _ccp token
          print "${_ccs_accent}Added${_ccs_reset} $2 (token)"
          ;;

        3|apikey|api_key|api-key)
          local _ak_key _ak_url _ak_conf

          ccs_ui_input "API key for $2 (hidden): " secret || return 1
          _ak_key="$REPLY"; REPLY=''

          [[ -n "$_ak_key" ]] || {
            _ccs_err "empty api key"
            return 1
          }

          # Base URL optional (blank = native Anthropic endpoint). Model is
          # deliberately NOT prompted for apikey profiles: Claude Code keeps
          # owning model selection (choose in-session with /model). A gateway
          # that renames models made a pinned default a footgun.
          ccs_ui_input 'Base URL (blank = api.anthropic.com): ' || return 1
          _ak_url="$REPLY"

          _ccs_clean_value "$_ak_key" &&
            _ccs_clean_value "$_ak_url" || {
              _ccs_err "apikey values cannot contain CR/LF"
              unset _ak_key
              return 1
            }

          _ak_conf="type=apikey"
          _ak_conf+=$'\n'"api_key=$_ak_key"
          if [[ -n "$_ak_url" ]]; then
            _ak_conf+=$'\n'"base_url=$_ak_url"

          fi

          _ccs_commit_profile "$2" "$_ak_conf" _ak_key || return 1

          print "${_ccs_accent}Added${_ccs_reset} $2 (apikey → ${_ak_url:-api.anthropic.com})"
          if [[ -n "$_ak_url" ]]; then
            _ccs_probe_nudge "$2"

          fi
          print "${_ccs_dim}Claude Code may ask you to approve this key on first launch.${_ccs_reset}"
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
            _ccs_provider_login "$2" "$_matched_provider" || {
              command rm -rf "$_CCPROTEUS_DIR/auth/$2"
              return 1
            }

            _ccs_write \
              "$dir/$2.conf" \
              "type=provider"$'\n'"provider=$_matched_provider" || {
              _ccs_err "could not save profile '$2'"
              command rm -rf "$_CCPROTEUS_DIR/auth/$2"
              return 1
            }

            if ! _ccs_read "$2" || ! _ccs_setup_dir "$2"; then
              unset _ccp
              _ccs_remove_profile_dir "$2"
              _ccs_remove_file "$dir/$2.conf"
              return 1
            fi
            unset _ccp

            print "${_ccs_accent}Added${_ccs_reset} $2 (provider: $_matched_provider)"
            print "${_ccs_dim}Set a default model:  ccproteus set $2 model <model-id>${_ccs_reset}"
            print "${_ccs_dim}Configure defaults: ccproteus config $2${_ccs_reset}"
          else
            _ccs_err "cancelled"
            return 1
          fi
          ;;
      esac
      print -r -- "Next: ccproteus run $2"
      ;;

    login)
      # Re-authenticating is safe and repeatable, so a missing name asks.
      if [[ -z "${2:-}" || "${2:-}" == (-i|--interactive) ]]; then
        local _login_pick
        _login_pick=$(_ccs_menu_profile provider) || return 1
        [[ -n "$_login_pick" ]] || return 1
        set -- login "$_login_pick"
      fi
      _ccs_valid "${2:-}" || {
        _ccs_err "usage: ccproteus login <name>"
        return 1
      }
      _ccs_exists "$2" || {
        _ccs_err "no profile '$2'"
        _ccs_accounts_hint
        return 1
      }
      _ccs_read "$2" || return 1
      [[ "${_ccp[type]}" == provider ]] || {
        _ccs_err "'$2' is not a provider profile — login is only for provider profiles"
        unset _ccp
        return 1
      }
      local _login_provider="${_ccp[provider]}"
      unset _ccp
      _ccs_provider_login "$2" "$_login_provider"
      ;;

    rm)
      _ccs_valid "${2:-}" || {
        _ccs_err "usage: ccproteus rm <name>"
        return 1
      }
      if [[ "${CCPROTEUS_CONTEXT_LOCKED:-}" != "$2" ]]; then
        command env CCPROTEUS_STATE_DIR="$dir" CCPROTEUS_CONTEXT_LOCKED="$2" \
          "$_CCPROTEUS_LIB_DIR/model-window" --locked "$2" zsh -c \
          'source "$1"; shift; ccproteus "$@"' ccproteus "$_CCPROTEUS_LIB_DIR/ccproteus.sh" "$@"
        return $?
      fi
      if ! _ccs_exists "$2"; then
        if [[ ! -e "$_CCPROTEUS_PROFILE_DIRS/$2" && ! -L "$_CCPROTEUS_PROFILE_DIRS/$2" &&
              ! -e "$dir/auth/$2" && ! -L "$dir/auth/$2" ]]; then
          _ccs_err "no profile '$2'"
          return 1
        fi
        print -u2 -- "Profile '$2' has no .conf. Remove leftover private state?"
        print -u2 -- "  $_CCPROTEUS_PROFILE_DIRS/$2"
        print -u2 -- "  $dir/auth/$2"
        print -u2 -- "Shared conversation history will be kept."
        ccs_ui_confirm 'Remove these leftovers?' || { _ccs_err "cancelled; nothing removed"; return 1; }
      fi

      _ccs_remove_profile_dir "$2" || {
        _ccs_err "could not remove credential directory for '$2'"
        return 1
      }

      command rm -f "$dir/$2.conf" || {
        _ccs_err "could not remove profile '$2'"
        return 1
      }

      if [[ "$active" == "$2" ]]; then
        unset CLAUDE_CODE_OAUTH_TOKEN

        command rm -f "$dir/active" || {
          _ccs_err "could not clear default profile"
          return 1
        }
      fi

      print "${_ccs_accent}Removed${_ccs_reset} $2"
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
        _cred_pick=$(_ccs_menu_profile apikey token) || return 1
        [[ -n "$_cred_pick" ]] || return 1
        set -- cred "$_cred_pick"
      fi
      _ccs_valid "${2:-}" || {
        _ccs_err "usage: ccproteus cred <profile>"
        return 1
      }

      _ccs_exists "$2" || {
        _ccs_err "no profile '$2'"
        _ccs_accounts_hint
        return 1
      }

      _ccs_read "$2" || return 1

      # Deliberately NOT _env: that provisions the profile's Claude Code state
      # (mkdir + 11 shared symlinks + .claude.json) as a side effect, so a
      # non-Claude consumer reading a credential would silently create a Claude
      # profile. This is a pure read.
      local _cred="${_ccp[api_key]:-${_ccp[token]:-}}"
      [[ -n "$_cred" ]] || {
        _ccs_err "'$2' is a ${_ccp[type]} profile and stores no credential here"
        _ccs_err "subscription profiles use browser login: ccproteus login $2"
        unset _ccp
        return 1
      }

      # Newline for a human, none for a pipe: `x=$(ccproteus cred gw)` must not
      # capture a trailing newline into a header value.
      if [[ -t 1 ]]; then
        print -r -- "$_cred"
      else
        print -rn -- "$_cred"
      fi

      unset _ccp
      ;;

    # `probe` is kept as an alias, not removed. v2.3.0 is published (release
    # tarball), so a script or a colleague's muscle memory already
    # says `probe`; silently breaking it to win a naming argument costs a user more
    # than the inconsistency does. `probe` is what the help and the docs teach.
    probe|"$_ccs_retired_verb")
      # The profile is positional, like every other command. It was `--profile
      # <profile>` alone among them, which is the kind of inconsistency a user pays
      # for every time they guess wrong.
      shift
      local -a _m_args=()
      if [[ "${1:-}" == (-i|--interactive) ]]; then
        local _probe_profile
        _probe_profile=$(_ccs_menu_profile) || return 1
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
        (( _pi )) || { _ccs_err "usage: ccproteus probe <profile> --model -i"; return 2; }
        local _probe_model
        _probe_model=$(_ccs_pick_value model "${_m_args[_pi+1]}") || return 1
        if [[ -z "${_m_args[_mi+1]:-}" ]]; then
          _m_args+=("$_probe_model")
        else
          _m_args[_mi+1]="$_probe_model"
        fi
      fi
      if (( ${_m_args[(I)--allow-inference]} )); then
        print -u2 -- "ccproteus: Checking context limits; inference requests may incur charges."
      fi
      "$_CCPROTEUS_LIB_DIR/refresh-model-registry" "${_m_args[@]}"
      ;;


    models|_models)
      (( $# <= 3 )) || { _ccs_err 'usage: ccproteus models [profile] [--filtered|--filtered-json]'; return 2; }
      if (( $# == 1 )) || [[ "${2:-}" == (-i|--interactive) ]]; then
        local _models_profile
        _models_profile=$(_ccs_menu_profile apikey) || return 1
        set -- "$1" "$_models_profile" "${@:3}"
      fi
      (( $# >= 2 )) || { _ccs_err "usage: ccproteus models <profile> [--filtered|--filtered-json]"; return 2; }
      [[ "${3:-}" == "" || "${3:-}" == "--filtered" || "${3:-}" == "--filtered-json" ]] || {
        _ccs_err "usage: ccproteus models <profile> [--filtered|--filtered-json]"
        return 2
      }
      _ccs_valid "${2:-}" || {
        _ccs_err "model listing requires an profile"
        return 1
      }

      _ccs_exists "$2" || {
        _ccs_err "no profile '$2'. Run: ccproteus list"
        return 1
      }

      if [[ "${3:-}" == "--filtered" ]]; then
        _ccs_gw_models "$2" picker
      elif [[ "${3:-}" == "--filtered-json" ]]; then
        _ccs_gw_models_json "$2" || return 1
        local _models_json="$REPLY"
        local _models_rows
        _models_rows=$(print -r -- "$_models_json" | command python3 -c '
import json
import re
import sys

fields = {}
with open(sys.argv[1], encoding="utf-8") as source:
    fields = dict(line.rstrip("\n").split("=", 1) for line in source
                  if "=" in line and not line.startswith("#"))
excluded = set(fields.get("excluded_models", "").split(",")) - {""}

import runpy
_is_excluded = runpy.run_path(sys.argv[-1])["is_excluded"]


def is_excluded(model):
    return _is_excluded(fields, model)

payload = json.load(sys.stdin)
for m in payload.get("data", []):
    if not isinstance(m, dict):
        continue
    caps = m.get("capabilities")
    if isinstance(caps, dict) and caps.get("chat_completion") is False:
        continue
    model_id = m.get("id")
    if not isinstance(model_id, str) or not model_id or is_excluded(model_id):
        continue
    print(model_id)
' "$_CCPROTEUS_DIR/$2.conf" "$_CCPROTEUS_LIB_DIR/model-window") || return 1
        print -r -- "$_models_json" | command python3 -c '
import json, sys
allowed = {line for line in sys.argv[1].splitlines() if line}
payload = json.load(sys.stdin)
payload["data"] = [row for row in payload.get("data", [])
                   if isinstance(row, dict) and row.get("id") in allowed]
print(json.dumps(payload))
' "$_models_rows"
      else
        _ccs_gw_models "$2"
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
        _sh_name=$(_ccs_menu_profile) || return 1
        [[ -n "$_sh_name" ]] || return 1
      fi
      _ccs_valid "$_sh_name" || {
        _ccs_err "usage: ccproteus inspect [<profile>]"
        return 1
      }
      _ccs_exists "$_sh_name" || {
        _ccs_err "no profile '$_sh_name'"
        _ccs_accounts_hint
        return 1
      }

      _ccs_read "$_sh_name" || return 1

      local _sh_type="${_ccp[type]}"
      local _sh_base="${_ccp[base_url]:-}"
      local _sh_tag="${_ccp[tag]:-}"
      local _sh_pct_conf="${_ccp[env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE]:-}"
      unset _ccp

      # Heading: name, type, route label, and whether this is the default.
      local _sh_head="${_ccs_bold}${_sh_name}${_ccs_reset}  ${_ccs_dim}${_sh_type}"
      [[ -n "$_sh_tag" ]] && _sh_head+=" · ${_sh_tag}"
      [[ "$_sh_name" == "$active" ]] && _sh_head+=" · default profile"
      print -r -- "${_sh_head}${_ccs_reset}"
      print ""

      [[ -n "$_sh_base" ]] &&
        _ccs_grid 10 endpoint "${_sh_base#https://}"

      # Model, and which of the two places answered.
      local _sh_model _sh_from
      _sh_model=$(_ccs_config_store get "$_sh_name" model) || return 1
      if [[ -n "$_sh_model" ]]; then
        _sh_from="profile default"
      else
        _sh_model=$(command sed -n 's/^model=//p' "$dir/$_sh_name.conf" 2>/dev/null | command head -1) ||
          _sh_model=""
        [[ -n "$_sh_model" ]] && _sh_from="conf bootstrap"
      fi
      if [[ -n "$_sh_model" ]]; then
        _ccs_grid 10 model "$_sh_model  ${_ccs_dim}${_sh_from}${_ccs_reset}"
      else
        _ccs_grid 10 model "${_ccs_dim}— Claude Code chooses${_ccs_reset}"
      fi

      local _sh_effort _sh_ef_from="profile default"
      _sh_effort=$(_ccs_config_store get "$_sh_name" effort) || return 1
      if [[ -n "$_sh_effort" ]]; then
        _ccs_grid 10 effort "$_sh_effort  ${_ccs_dim}${_sh_ef_from}${_ccs_reset}"
      else
        _ccs_grid 10 effort "${_ccs_dim}— Claude Code's own default${_ccs_reset}"
      fi

      _ccs_config_store model-settings "$_sh_name" | command python3 -c '
import json, sys
for model, settings in sorted(json.load(sys.stdin).get("modelSettings", {}).items()):
    print("  effort:" + model + " " + settings["effortLevel"] + "  (profile override)")
'

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
        _sh_win=$(_ccs_model_window "$_sh_name" "$_sh_model" 2>/dev/null) || _sh_win=""
        if [[ -n "$_sh_win" ]]; then
          _sh_tok=$(command python3 -c '
import json, sys
d = json.loads(sys.stdin.read())
source = d.get("source", "")
label = "catalogue-estimate" if source in ("conf-discovered-catalogue", "catalogue-bundled") else "manual" if source == "conf-manual" else "probed"
print(d.get("window") or "", 1 if d.get("unprobed") else 0, label)
' <<< "$_sh_win" 2>/dev/null) || _sh_tok=""
          _sh_source="${_sh_tok##* }"
          _sh_tok="${_sh_tok% *}"
          _sh_unprobed="${_sh_tok##* }"
          _sh_tok="${_sh_tok%% *}"
        fi
        if [[ "$_sh_tok" == <-> ]] && (( _sh_tok > 0 )) && (( ! _sh_unprobed )); then
          local _sh_pct
          _sh_pct=$(_ccs_effective_compaction_percent "$_sh_name" "$_sh_pct_conf") || return 1
          _ccs_grid 10 window \
            "$(printf "%'d" "$_sh_tok")  ${_ccs_dim}${_sh_source} · compaction target ${_sh_pct}%${_ccs_reset}"
        else
          _ccs_grid 10 window \
            "${_ccs_dim}unknown · compacts early to stay safe${_ccs_reset}"
        fi
      fi

      case "$_sh_base" in
        ""|https://api.anthropic.com|https://api.anthropic.com/)
          _ccs_grid 10 protocol 'native Anthropic' ;;
        *)
          _ccs_grid 10 protocol 'automatic — cached compatibility or validation at launch' ;;
      esac

      # The whole point: what this becomes at launch.
      print ""
      if [[ -n "$_sh_model" && "$_sh_type" == apikey ]]; then
        print -r -- "  ${_ccs_dim}launches as${_ccs_reset}  claude --model ${_sh_model}"
      else
        print -r -- "  ${_ccs_dim}launches as${_ccs_reset}  claude ${_ccs_dim}(Claude Code picks the model)${_ccs_reset}"
      fi
      ;;

    _menu)
      # `pick` is the pre-1.10 name, kept as an undocumented alias so a script
      # or a habit from an earlier version keeps working. Both are internal: the
      # spelling users read is `-i`, which routes here.
      #
      # No profile named: chain from the profile chooser, exactly as `ccproteus -i`
      # does. Erroring here instead made one concept behave two ways depending on
      # where it was typed, and the argument it asked for is the one the chooser
      # exists to supply.
      local _menu_account="${2:-}"
      if [[ -z "$_menu_account" ]]; then
        _menu_account=$(_ccs_menu_profile) || return 1
        [[ -n "$_menu_account" ]] || return 1
      fi

      _ccs_valid "$_menu_account" || {
        _ccs_err "usage: the model picker needs an profile"
        return 1
      }

      _ccs_exists "$_menu_account" || {
        _ccs_err "no profile '$_menu_account'. Run: ccproteus list"
        return 1
      }

      _ccs_pick_value model "$_menu_account"
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
        _ctx_profile=$(_ccs_menu_profile) || return 1
        [[ -n "$_ctx_profile" ]] || return 1
      fi
      _ccs_valid "$_ctx_profile" || {
        _ccs_err "usage: ccproteus context <profile> <model> [tokens|auto]"
        return 2
      }
      _ccs_exists "$_ctx_profile" || {
        _ccs_err "no profile '$_ctx_profile'. Run: ccproteus list"
        return 1
      }
      # A model id is exactly as unguessable here as it is at `set`, and this verb
      # is reached from a `probe` failure — the moment a reader is least sure of the
      # spelling. So a missing or `-i` model opens the same picker.
      if [[ -z "$_ctx_model" || "$_ctx_model" == (-i|--interactive) ]]; then
        _ctx_model=$(_ccs_pick_value model "$_ctx_profile") || return 1
        [[ -n "$_ctx_model" ]] || return 1
        # A picked model means the tokens argument (if any) shifted along with it.
        set -- context "$_ctx_profile" "$_ctx_model" "${@:4}"
      fi
      command env CCPROTEUS_STATE_DIR="$dir" "$_CCPROTEUS_LIB_DIR/model-window" \
        --context "$_ctx_profile" "$_ctx_model" "${@:4}"
      ;;

    get|set|config)
      local _cfg_verb="$1" _cfg_profile="${2:-}" _cfg_key="${3:-}"
      case "$_cfg_verb" in
        get) (( $# <= 3 )) || { _ccs_err 'usage: ccproteus get [profile] [model|effort]'; return 2; } ;;
        set) (( $# <= 4 )) || { _ccs_err 'usage: ccproteus set [profile] [model|effort] [value]'; return 2; } ;;
        config) (( $# <= 2 )) || { _ccs_err 'usage: ccproteus config [profile]'; return 2; } ;;
      esac
      # No profile named: chain from the profile chooser rather than erroring, the
      # same way `run -i` does. The argument being demanded is the one the chooser
      # exists to supply, and a user who cannot recall a model id often cannot
      # recall a profile name either.
      if [[ -z "$_cfg_profile" || "$_cfg_profile" == (-i|--interactive) ]]; then
        _cfg_profile=$(_ccs_menu_profile) || return 1
        [[ -n "$_cfg_profile" ]] || return 1
        set -- "$1" "$_cfg_profile" "${@:3}"
        _cfg_key="${3:-}"
      fi
      _ccs_valid "$_cfg_profile" || { _ccs_err "usage: ccproteus $_cfg_verb <profile>"; return 2; }
      _ccs_exists "$_cfg_profile" || { _ccs_err "no profile '$_cfg_profile'"; return 1; }
      # `set <profile> <slot>` with no value, and `set <profile> <slot> -i`, both
      # mean "show me the choices": a missing value is the same request as an
      # explicit ask, so it opens the picker instead of printing a usage line the
      # reader cannot act on without knowing the value already.
      if [[ "$_cfg_verb" == set && "$_cfg_key" == (model|effort) ]] &&
         { (( $# == 3 )) || [[ "${4:-}" == (-i|--interactive) ]] }; then
        local _cfg_picked
        _cfg_picked=$(_ccs_pick_value "$_cfg_key" "$_cfg_profile") || return 1
        [[ -n "$_cfg_picked" ]] || return 1
        set -- set "$_cfg_profile" "$_cfg_key" "$_cfg_picked"
      fi
      if [[ "$_cfg_verb" == (get|set) && "$_cfg_key" == (''|-i|--interactive) ]] && (( $# <= 3 )); then
        _cfg_key=$(ccs_ui_select native "Setting — $_cfg_profile" \
          $'model\tModel\tDefault model for new sessions\neffort\tReasoning\tDefault reasoning level') || return 1
        if [[ "$_cfg_verb" == set ]]; then
          local _cfg_value
          _cfg_value=$(_ccs_pick_value "$_cfg_key" "$_cfg_profile") || return 1
          set -- set "$_cfg_profile" "$_cfg_key" "$_cfg_value"
        else
          set -- get "$_cfg_profile" "$_cfg_key"
        fi
      fi
      case "$_cfg_verb" in
        get) (( $# == 3 )) && [[ "$_cfg_key" == (model|effort) ]] || { _ccs_err "usage: ccproteus get <profile> <model|effort>"; return 2; } ;;
        set) (( $# == 4 )) && [[ "$_cfg_key" == (model|effort) && -n "$4" ]] || { _ccs_err "usage: ccproteus set <profile> <model|effort> [value]  (omit the value, or pass -i, to choose from a list)"; return 2; } ;;
        config) (( $# == 2 )) || { _ccs_err "usage: ccproteus config <profile>"; return 2; } ;;
      esac
      if [[ "$_cfg_verb" == get ]]; then
        _ccs_config_store get "$_cfg_profile" "$_cfg_key"
        return $?
      fi
      if [[ "${CCPROTEUS_CONTEXT_LOCKED:-}" != "$_cfg_profile" ]]; then
        command env CCPROTEUS_STATE_DIR="$dir" CCPROTEUS_CONTEXT_LOCKED="$_cfg_profile" \
          "$_CCPROTEUS_LIB_DIR/model-window" --locked "$_cfg_profile" zsh -c \
          'source "$1"; shift; ccproteus "$@"' ccproteus "$_CCPROTEUS_LIB_DIR/ccproteus.sh" "$@"
        return $?
      fi
      local _CCS_CONFIG_MODEL="" _CCS_CONFIG_EFFORT="" _CCS_CONFIG_SNAPSHOT=""
      local _CCS_CONFIG_TYPE="" _CCS_CONFIG_ENDPOINT="" _CCS_CONFIG_CREDENTIAL=""
      export _CCS_CONFIG_MODEL _CCS_CONFIG_EFFORT _CCS_CONFIG_SNAPSHOT
      export _CCS_CONFIG_TYPE _CCS_CONFIG_ENDPOINT _CCS_CONFIG_CREDENTIAL
      if [[ "$_cfg_verb" == set ]]; then
        if [[ "$_cfg_key" == model ]]; then _CCS_CONFIG_MODEL="$4"; else _CCS_CONFIG_EFFORT="$4"; fi
        _ccs_config_store set "$_cfg_profile" || return 1
        # A bare value on stdout, so `m=$(ccproteus get gw model)` and any script
        # already parsing this keep working. The sentence goes to stderr, where a
        # picked value needs it most: the user never typed the id they chose, so
        # without this the terminal shows a list closing and nothing else.
        [[ -t 2 ]] && print -u2 -- \
          "${_ccs_accent}Saved${_ccs_reset} $_cfg_profile $_cfg_key = ${_ccs_bold}$4${_ccs_reset} ${_ccs_dim}(new sessions)${_ccs_reset}"
        print -r -- "$4"
        return 0
      fi
      _CCS_CONFIG_SNAPSHOT=$(_ccs_config_store snapshot "$_cfg_profile") || return 1
      _ccs_read "$_cfg_profile" || return 1
      _CCS_CONFIG_TYPE="${_ccp[type]}"
      _CCS_CONFIG_ENDPOINT="${_ccp[base_url]:-}"
      _CCS_CONFIG_CREDENTIAL="${_ccp[api_key]:-${_ccp[token]:-}}"
      _CCS_CONFIG_MODEL=$(_ccs_config_store get "$_cfg_profile" model) || return 1
      _CCS_CONFIG_EFFORT=$(_ccs_config_store get "$_cfg_profile" effort) || return 1
      # Terminal users choose a task, not a tour of every setting. Preserve the
      # existing piped-input sequence for scripts.
      local answer="" _cfg_section=all _cfg_rows=""
      if [[ -t 0 ]]; then
        _cfg_section=defaults
        if [[ "$_CCS_CONFIG_TYPE" == (apikey|token) ]]; then
          _cfg_rows=$'defaults\tModel and reasoning\tDefaults for new sessions'
          if [[ "$_CCS_CONFIG_TYPE" == apikey ]]; then
            _cfg_rows+=$'\nconnection\tConnection\tEndpoint or API key'
          else
            _cfg_rows+=$'\nconnection\tSign-in token\tReplace the stored token'
          fi
          _cfg_section=$(ccs_ui_select native "Settings — $_cfg_profile" "$_cfg_rows") || { unset _ccp; return 1; }
        fi
      fi
      if [[ "$_cfg_section" != defaults && "$_CCS_CONFIG_TYPE" == apikey ]]; then
        ccs_ui_input "Endpoint [${_CCS_CONFIG_ENDPOINT:-https://api.anthropic.com}]: " || { unset _ccp; return 1; }
        answer="$REPLY"
        [[ -z "$answer" ]] || _CCS_CONFIG_ENDPOINT="$answer"
        _ccp[base_url]="$_CCS_CONFIG_ENDPOINT"
        if [[ -n "$_CCS_CONFIG_ENDPOINT" ]]; then
          _ccs_validate_base_url "profile '$_cfg_profile'" || { unset _ccp; return 1; }
        fi
      fi
      if [[ "$_cfg_section" != defaults && "$_CCS_CONFIG_TYPE" == (apikey|token) ]]; then
        if [[ "$_cfg_section" == connection ]] || ccs_ui_confirm 'Replace stored credential?'; then
          ccs_ui_input 'Credential (hidden; Enter keeps current): ' secret || { unset _ccp; return 1; }
          answer="$REPLY"; REPLY=''
          [[ -z "$answer" ]] || _CCS_CONFIG_CREDENTIAL="$answer"
        else
          (( $? == 2 )) && { unset _ccp; return 1; }
        fi
        _ccs_clean_value "$_CCS_CONFIG_CREDENTIAL" || { unset _ccp; _ccs_err "credential contains CR/LF"; return 1; }
        if [[ "$_CCS_CONFIG_TYPE" == apikey ]]; then _ccp[api_key]="$_CCS_CONFIG_CREDENTIAL"; else _ccp[token]="$_CCS_CONFIG_CREDENTIAL"; fi
      fi
      _ccs_validate_loaded "$_cfg_profile" || { unset _ccp; return 1; }
      unset _ccp
      if [[ "$_cfg_section" != connection ]]; then
        _CCS_CONFIG_MODEL=$(_ccs_prompt_default model "$_cfg_profile" "$_CCS_CONFIG_MODEL") || return 1
        _CCS_CONFIG_EFFORT=$(_ccs_prompt_default effort "$_cfg_profile" "$_CCS_CONFIG_EFFORT") || return 1
      fi
      print -ru2 -- "Profile: $_cfg_profile"
      if [[ "$_cfg_section" != defaults ]]; then
        [[ "$_CCS_CONFIG_TYPE" != apikey ]] || print -ru2 -- "Endpoint: ${_CCS_CONFIG_ENDPOINT:-https://api.anthropic.com}"
        print -ru2 -- 'Credential: hidden'
      fi
      print -ru2 -- "Model: ${_CCS_CONFIG_MODEL:-Claude default} · Effort: ${_CCS_CONFIG_EFFORT:-Claude default}"
      _ccs_config_store configure "$_cfg_profile" || return 1
      print -r -- "Saved $_cfg_profile (new sessions)"
      ;;

    mv)
      _ccs_valid "${2:-}" && _ccs_valid "${3:-}" || {
        _ccs_err "usage: ccproteus mv <old> <new>"
        return 1
      }
      if [[ "${CCPROTEUS_CONTEXT_LOCKED:-}" != "$2,$3" ]]; then
        command env CCPROTEUS_STATE_DIR="$dir" CCPROTEUS_CONTEXT_LOCKED="$2,$3" \
          "$_CCPROTEUS_LIB_DIR/model-window" --locked "$2,$3" zsh -c \
          'source "$1"; shift; ccproteus "$@"' ccproteus "$_CCPROTEUS_LIB_DIR/ccproteus.sh" "$@"
        return $?
      fi

      _ccs_exists "$2" || {
        _ccs_err "no profile '$2'"
        return 1
      }

      _ccs_exists "$3" && {
        _ccs_err "'$3' already exists"
        return 1
      }


      local old_profile="$_CCPROTEUS_PROFILE_DIRS/$2"
      local new_profile="$_CCPROTEUS_PROFILE_DIRS/$3"
      local moved_profile=0
      local rc=0

      # mv onto an EXISTING target nests the source inside it (new/old/…),
      # which the rollback below cannot undo — refuse instead. A leftover
      # directory from a deleted conf is exactly the case that hits this.
      if [[ -e "$new_profile" || -L "$new_profile" ]]; then
        _ccs_err "profile directory '$3' already exists — remove it first"
        return 1
      fi

      if [[ -e "$old_profile" || -L "$old_profile" ]]; then
        command mv "$old_profile" "$new_profile" || {
          _ccs_err "could not rename profile directory"
          return 1
        }
        moved_profile=1
      fi

      command mv "$dir/$2.conf" "$dir/$3.conf" || rc=$?

      # Rename the provider auth directory if it exists.
      local _old_auth="$_CCPROTEUS_DIR/auth/$2"
      local _new_auth="$_CCPROTEUS_DIR/auth/$3"
      local _moved_auth=0
      if (( rc == 0 )) && [[ -d "$_old_auth" ]]; then
        if [[ -e "$_new_auth" ]]; then
          _ccs_err "auth directory for '$3' already exists: $_new_auth — remove it first"
          rc=1
        else
          command mv "$_old_auth" "$_new_auth" || rc=$?
          (( rc == 0 )) && _moved_auth=1
        fi
      fi

      if (( rc == 0 )) && [[ "$active" == "$2" ]]; then
        _ccs_atomic_write_unlocked "$dir/active" "$3" || rc=$?
      fi

      if (( rc != 0 )) && [[ -e "$dir/$3.conf" ]]; then
        command mv "$dir/$3.conf" "$dir/$2.conf"
      fi

      if (( rc != 0 )); then
        (( _moved_auth )) && command mv "$_new_auth" "$_old_auth"
        (( moved_profile )) && command mv "$new_profile" "$old_profile"
        _ccs_err "could not rename profile"
        return 1
      fi

      print "${_ccs_accent}Renamed${_ccs_reset} ${_ccs_bold}$2${_ccs_reset} → ${_ccs_bold}$3${_ccs_reset}"
      ;;

    hop)
      local _hop_profile="${2:-}" _hop_current=0
      if [[ "$_hop_profile" == (-i|--interactive) ]]; then
        [[ -n "${CLAUDE_CODE_SESSION_ID:-}" ]] && _ccs_relay_valid "${CCPROTEUS_RELAY_ID:-}" || {
          _ccs_err 'profile switching requires a running ccproteus session'
          return 1
        }
        _hop_profile=$(_ccs_menu_profile) || return 1
      fi
      if [[ -z "$_hop_profile" && -n "${CCPROTEUS_RELAY_ID:-}" ]]; then
        # Extract parent PID from relay ID and find the adapter process serving this session
        local _parent_pid="${CCPROTEUS_RELAY_ID%%.*}"
        local _adapter_cmd
        _adapter_cmd=$(ps -o pid,ppid,command | grep "ccproteus-adapter" | grep -E "^[[:space:]]*[0-9]+[[:space:]]+${_parent_pid}[[:space:]]" | head -1)
        if [[ -n "$_adapter_cmd" ]]; then
          # Parse profile from adapter command line: "ccproteus-adapter <profile> ..."
          _hop_profile=$(echo "$_adapter_cmd" | sed -E 's/.*ccproteus-adapter[[:space:]]+([^[:space:]]+).*/\1/')
          _hop_current=1
        fi
      fi
      # No adapter serves sub and token sessions; their config dir is the only witness.
      if [[ -z "$_hop_profile" && -n "${CCPROTEUS_RELAY_ID:-}" && -n "${CLAUDE_CONFIG_DIR:-}" &&
            "${CLAUDE_CONFIG_DIR:A:h}" == "${_CCPROTEUS_PROFILE_DIRS:A}" ]]; then
        _hop_profile="${CLAUDE_CONFIG_DIR:t}"
        _hop_current=1
      fi

      _ccs_valid "$_hop_profile" || {
        _ccs_err "usage: ccproteus hop [profile]"
        return 1
      }

      _ccs_exists "$_hop_profile" || {
        _ccs_err "no profile '$_hop_profile'. Run: ccproteus list"
        return 1
      }

      _ccs_read "$_hop_profile" || return 1

      _ccs_validate_loaded "$_hop_profile" || {
        unset _ccp
        return 1
      }
      unset _ccp

      [[ -n "${CLAUDE_CODE_SESSION_ID:-}" ]] || {
        _ccs_err "not inside a Claude Code session"
        return 1
      }

      _ccs_relay_valid "${CCPROTEUS_RELAY_ID:-}" || {
        _ccs_err "this session was not launched by the ccproteus relay"
        return 1
      }

      local _signal
      local -a _session_state=()

      _signal="profile=$_hop_profile"
      _signal+=$'\n'"session=$CLAUDE_CODE_SESSION_ID"
      if _ccs_session_state "$CLAUDE_CODE_SESSION_ID"; then
        _session_state=("${reply[@]}")
        # Carry the model only where the target can actually serve it. Leaving the
        # field out is already how the relay asks for the profile's own default, so
        # the fallback needs no second mechanism -- and a definite no is said out
        # loud, because a silently changed model is the thing the user notices last.
        if (( _hop_current )); then
          _signal+=$'\n'"model=${_session_state[1]}"
        else
          _ccs_model_known "$_hop_profile" "${_session_state[1]}"
          case $? in
            1) print "${_ccs_yellow}ccproteus:${_ccs_reset} ${_ccs_bold}${_session_state[1]}${_ccs_reset} is not served by ${_ccs_bold}${_hop_profile}${_ccs_reset} — resuming on its default model." ;;
            *) _signal+=$'\n'"model=${_session_state[1]}" ;;
          esac
        fi
        _signal+=$'\n'"effort=${_session_state[2]}"
      fi

      _ccs_write \
        "$dir/.reswitch.${CCPROTEUS_RELAY_ID}" \
        "$_signal" || {
        _ccs_log "hop FAILED to write relay target=$_hop_profile relay_id=${CCPROTEUS_RELAY_ID}"
        _ccs_err "could not queue switch"
        return 1
      }

      _ccs_log "hop queued target=$_hop_profile session=$CLAUDE_CODE_SESSION_ID relay_id=${CCPROTEUS_RELAY_ID}"
      export _ccs_relay_expected="$_hop_profile"

      print "${_ccs_accent}Switch queued${_ccs_reset} → ${_ccs_bold}$_hop_profile${_ccs_reset}"
      print "Exit now (${_ccs_bold}Ctrl-C${_ccs_reset} or ${_ccs_bold}/exit${_ccs_reset}) to complete the switch."
      ;;

    # `ccproteus _env <name>`, plus a running endpoint for a provider profile.
    #
    # _env answers "what does this profile launch as" and stops there, which for
    # every other type is also everything a subprocess needs to dial it: the
    # endpoint, the credential, and whatever HTTPS_PROXY the caller's own
    # environment already carries. A provider is the exception — it has no
    # endpoint until its bridge runs — so this arm starts one and hands back a
    # handle the caller must retire (`ccproteus _route_stop <handle>`) when done.
    _route_env)
      _ccs_valid "${2:-}" || return 1

      _ccs_exists "$2" || {
        print -u2 "ccproteus: unknown profile '$2'"
        return 1
      }

      _ccs_read "$2" || return 1
      local _re_type="${_ccp[type]:-}"
      unset _ccp

      if [[ "$_re_type" == provider ]]; then
        _ccs_provider_start "$2" || return 1
        ccproteus _env "$2" || { _ccs_provider_stop; return 1; }
        print "ANTHROPIC_BASE_URL=http://127.0.0.1:${_ccs_provider_port}"
        print "ANTHROPIC_API_KEY=${_ccs_provider_key}"
        print "CCPROTEUS_ROUTE_HANDLE=provider|${_ccs_provider_pid}|${_ccs_provider_tmpdir}|${_ccs_provider_auth}"
        return 0
      fi

      ccproteus _env "$2"
      ;;

    # Retire a route started by _route_env. Only a provider ever opens one, so
    # any other handle names nothing this process owns.
    _route_stop)
      if [[ "${2:-}" == provider\|* ]]; then
        local -a _rs=("${(@s:|:)2}")
        [[ "${_rs[2]}" == <-> ]] || return 1
        _ccs_provider_pid="${_rs[2]}" _ccs_provider_tmpdir="${_rs[3]}" _ccs_provider_auth="${_rs[4]}"
        _ccs_provider_stop
      fi
      ;;

    _env)
      _ccs_valid "${2:-}" || return 1

      _ccs_exists "$2" || {
        print -u2 "ccproteus: unknown profile '$2'"
        return 1
      }

      _ccs_read "$2" || return 1

      _ccs_validate_loaded "$2" || {
        unset _ccp
        return 1
      }

      _ccs_setup_dir "$2" || {
        unset _ccp
        return 1
      }

      _ccs_prelaunch "$2" 2>/dev/null || :

      # Secure storage follows CLAUDE_CONFIG_DIR, so each profile's sign-in stays in
      # its own keychain entry. MCP credentials are shared by capsules, not here.
      print "CLAUDE_CONFIG_DIR=${_CCPROTEUS_PROFILE_DIRS}/$2"
      [[ "${_ccp[private]:-}" == 1 ]] && print "CCPROTEUS_PRIVATE=1"

      case "${_ccp[type]}" in
        apikey)
          # An apikey profile may have no base_url: that is the native
          # api.anthropic.com case, where Claude Code's own default endpoint is
          # correct and overriding it would break the profile.
          if [[ -n "${_ccp[base_url]:-}" ]]; then
            print "ANTHROPIC_BASE_URL=${_ccp[base_url]}"
          fi

          print "ANTHROPIC_API_KEY=${_ccp[api_key]}"
          ;;

        token)
          print "CLAUDE_CODE_OAUTH_TOKEN=${_ccp[token]}"
          ;;

        provider)
          # Provider profiles have no static credential; the launcher injects
          # ANTHROPIC_BASE_URL and ANTHROPIC_API_KEY from the per-session
          # provider proxy at launch time. Emit env.* overrides only.
          ;;

        sub)
          # Deliberately no credential env. A sub profile authenticates from
          # the profile state in its own CLAUDE_CONFIG_DIR (written by
          # `ccproteus add sub`), so injecting anything here would override the
          # login it is supposed to use. Listed explicitly so the silence is a
          # decision, not a fall-through.
          ;;

        *)
          # Every credential type must say how it authenticates. Falling
          # through silently ships a session with no credential, which
          # surfaces as Claude Code's interactive login picker with nothing
          # explaining why — the failure mode is indistinguishable from a
          # revoked profile.
          _ccs_err "profile '$2' has unsupported type '${_ccp[type]}'"
          unset _ccp
          return 1
          ;;
      esac

      [[ -n "${_ccp[model_map]:-}" ]] && print "CCPROTEUS_MODEL_MAP=${_ccp[model_map]}"
      local _k
      for _k in "${(@k)_ccp}"; do
        [[ "$_k" == env.* ]] && print "${_k#env.}=${_ccp[$_k]}"
      done
      unset _ccp
      ;;

    use)
      if [[ -z "${2:-}" ]]; then
        if [[ -n "$active" ]]; then
          print "${_ccs_bold}${active}${_ccs_reset}"
        else
          print "${_ccs_dim}No default set. Run: ccproteus use <profile>${_ccs_reset}"
        fi
        return 0
      fi

      # A bare `use` reports the current default, which is a useful answer and stays
      # as it is. `-i` is the explicit "let me choose a new one".
      if [[ "$2" == (-i|--interactive) ]]; then
        local _use_pick
        _use_pick=$(_ccs_menu_profile) || return 1
        [[ -n "$_use_pick" ]] || return 1
        set -- use "$_use_pick"
      fi

      _ccs_valid "${2:-}" || {
        _ccs_err "invalid name — use alphanumeric/underscore"
        return 1
      }

      _ccs_exists "$2" || {
        _ccs_err "no profile '$2'. Run: ccproteus list"
        return 1
      }

      _ccs_read "$2" || return 1

      _ccs_validate_loaded "$2" || {
        unset _ccp
        return 1
      }

      _ccs_setup_dir "$2" || {
        unset _ccp
        return 1
      }

      _ccs_write "$dir/active" "$2" || {
        unset _ccp
        _ccs_err "could not set default profile"
        return 1
      }

      unset _ccp
      print "${_ccs_accent}Default set${_ccs_reset} to ${_ccs_bold}$2${_ccs_reset}"
      ;;

    *)
      if [[ "$1" == -* ]]; then
        _ccs_err "unknown command '$1'. Launch flags go after the verb: ccproteus run $1"
      else
        _ccs_err "unknown command '$1'. Run: ccproteus --help"
        _ccs_exists "$1" && print -u2 -- "  To launch that profile: ccproteus run $1"
      fi
      _ccs_accounts_hint
      return 1
      ;;
  esac
}
