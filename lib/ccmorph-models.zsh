# ccmorph-models.zsh — Models: the session's model and state on resume, gateway /v1/models listings, published windows, route ceilings and model lookup.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

_ccm_resume_model() {
  local session_id="$1"
  local transcript model

  REPLY=""
  [[ -n "$session_id" ]] && _ccm_clean_value "$session_id" || return 1

  _ccm_session_file "$session_id" || return 1
  transcript="$REPLY" REPLY=""

  model=$(command python3 - "$transcript" <<'PY'
import json
import re
import sys

command_model = assistant_model = ""
saw_model_command = False
pattern = re.compile(r"<command-args>(.*?)</command-args>", re.DOTALL)

try:
    transcript = open(sys.argv[1], encoding="utf-8")
except OSError:
    raise SystemExit(1)

with transcript:
    for line in transcript:
        try:
            record = json.loads(line)
        except (json.JSONDecodeError, TypeError):
            continue

        message = record.get("message")
        if (
            record.get("type") == "assistant"
            and record.get("isSidechain") is not True
            and isinstance(message, dict)
            and message.get("role") == "assistant"
            and isinstance(message.get("model"), str)
            and message["model"] not in ("", "<synthetic>")
        ):
            assistant_model = message["model"]

        if (
            record.get("type") != "user"
            or record.get("isSidechain") is True
            or not isinstance(message, dict)
            or message.get("role") != "user"
        ):
            continue

        content = message.get("content")
        if (
            not isinstance(content, str)
            or "<command-name>/model</command-name>" not in content
        ):
            continue

        saw_model_command = True
        match = pattern.search(content)
        value = match.group(1) if match else ""
        command_model = value if value and "\n" not in value and "\r" not in value else ""

sys.stdout.write(command_model if saw_model_command else assistant_model)
PY
  ) || return 1

  [[ -n "$model" ]] && _ccm_clean_value "$model" || return 1
  case "$model" in
    -*|@*|\?|*[[:space:]]*) return 1 ;;
  esac
  REPLY="$model"
}

_ccm_session_state() {
  local session_id="$1"
  local transcript state model effort

  reply=()
  [[ -n "$session_id" ]] && _ccm_clean_value "$session_id" || return 1

  _ccm_session_file "$session_id" || return 1
  transcript="$REPLY"

  state=$(command python3 - "$transcript" <<'PY'
import json
import sys

model = effort = ""
try:
    transcript = open(sys.argv[1], encoding="utf-8")
except OSError:
    raise SystemExit(1)

with transcript:
    for line in transcript:
        try:
            record = json.loads(line)
        except (json.JSONDecodeError, TypeError):
            continue
        message = record.get("message")
        if (
            record.get("type") == "assistant"
            and record.get("isSidechain") is not True
            and isinstance(message, dict)
            and message.get("role") == "assistant"
            and message.get("model") not in (None, "", "<synthetic>")
            and record.get("effort") not in (None, "")
        ):
            model = str(message["model"])
            effort = str(record["effort"])

if not model or not effort or any(c in model + effort for c in "\t\r\n"):
    raise SystemExit(1)
sys.stdout.write(f"{model}\t{effort}")
PY
  ) || return 1

  [[ "$state" == *$'\t'* ]] || return 1
  model="${state%%$'\t'*}"
  effort="${state#*$'\t'}"
  [[ -n "$model" && -n "$effort" ]] || return 1
  reply=("$model" "$effort")
}

_ccm_api_url() {  # $1 = API root or versioned SDK base, $2 = /v1/... path
  local base="${1%/}" path="$2"
  [[ "${base:l}" == */v<-> ]] && path="${path#/v1}"
  REPLY="$base$path"
}

_ccm_undisguise_ids() {  # REPLY = $1 with every claude-<cover>-dd-<reversed> id restored
  # Translators disguise non-Claude ids so gateway discovery will list them. Every
  # consumer here wants the true id, so each listing is undisguised once, on entry.
  local body="$1" q='"' id
  for id in ${(u)${(M)${(ps:$q:)body}:#claude-*-dd-?*}}; do
    body=${body//$q$id$q/$q${(j::)${(Oas::)id#*-dd-}}$q}
  done
  REPLY="$body"
}

# curl with the credential headers fed through its stdin config (-K -), so the
# key never appears on a command line (ps). Usage: _ccm_curl_keyed <both|bearer>
# <key> <curl args...>. Callers must not send a body on stdin.
_ccm_curl_keyed() {
  local kind="$1" key="$2"; shift 2
  local esc="${key//\\/\\\\}"; esc="${esc//\"/\\\"}"
  {
    [[ "$kind" == both ]] && print -r -- "header = \"x-api-key: $esc\""
    print -r -- "header = \"Authorization: Bearer $esc\""
  } | command curl -K - "$@"
}

_ccm_models_listing() {  # $1 = base url, $2 = key, $3 = max-time seconds
  # One `/v1/models` listing per launch, shared.
  #
  # The window probe and the picker projection both need this listing, and both
  # fetched it themselves — the same URL with the same headers, twice per launch,
  # differing only in timeout. The listing is free, so this was never a token cost,
  # but it is a wasted round trip on every launch and two chances for a slow
  # gateway to stall a shell instead of one.
  #
  # Memoised per (base, key) for the life of the shell, which is the right scope
  # for its two callers: they run within a single launch, and the answer cannot
  # change between them. Deliberately NOT a durable cache — a gateway's catalogue
  # does change, and `_ccm_project_picker` is documented as a probe rather
  # than a memory, so a later launch must ask again.
  # Answers in $REPLY, never on stdout. A caller writing `body=$(_ccm_models_listing
  # ...)` would run this whole function in a subshell, and the memo write would then
  # land in a copy of the array that dies with it — every call a cache miss, the
  # duplicate fetch intact, and nothing visibly wrong. Returning through REPLY keeps
  # the assignment in the caller's own shell, which is the only place it survives.
  local base="$1" key="$2" maxtime="${3:-8}"
  REPLY=""
  [[ -n "$base" && -n "$key" ]] || return 1

  # The credential is part of the identity: two profiles may share a base URL with
  # different keys and be shown different catalogues.
  local memo_key="${base}"$'\x1f'"${key}"
  if [[ -n "${_ccm_models_memo[$memo_key]:-}" ]]; then
    REPLY="${_ccm_models_memo[$memo_key]}"
    return 0
  fi

  local body url
  _ccm_api_url "$base" "/v1/models?limit=1000"
  url="$REPLY"
  body=$(_ccm_curl_keyed both "$key" -sS -f --connect-timeout 2 --max-time "$maxtime" \
           "$url" \
           -H 'anthropic-version: 2023-06-01' 2>/dev/null) || return 1
  [[ -n "$body" ]] || return 1

  _ccm_undisguise_ids "$body"
  _ccm_models_memo[$memo_key]="$REPLY"
}

# "<known window or empty>\t<source>\t<unknown budget or empty>\t<send_id>",
# one process.
_ccm_model_window_fields() {
  _ccm_migrate_state || return 1
  local resolver="$_CCMORPH_LIBEXEC_DIR/ccmorph-core"
  [[ -x "$resolver" ]] || return 1
  command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
    CCMORPH_STATE_DIR="$_CCMORPH_DIR" MODEL_WINDOW_PROFILE_CONF="$_CCMORPH_DIR/$1.conf" \
    "$resolver" model-window --fields "$1" "$2" 2>/dev/null
}

# One local resolver reads manual and discovered limits from the profile itself.
_ccm_model_window() {
  _ccm_migrate_state || return 1
  local resolver="$_CCMORPH_LIBEXEC_DIR/ccmorph-core"
  [[ -x "$resolver" ]] || return 1
  command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
    CCMORPH_STATE_DIR="$_CCMORPH_DIR" MODEL_WINDOW_PROFILE_CONF="$_CCMORPH_DIR/$1.conf" \
    "$resolver" model-window "$1" "$2" 2>/dev/null
}

# Run a command detached: no job-table entry, no SIGHUP when this shell exits.
#
# A FUNCTION, not an inline block, and that is the whole point. `setopt
# localoptions` restores at function return — never at the end of an enclosing
# `{ ... }` — so the same three lines written inline in the launch path left
# `monitor` off for the rest of the launch, including the foreground `claude`.
# Job control then applied to nothing: claude shares the launcher's process group
# instead of getting its own, so Ctrl-Z did not suspend it and terminal signals
# went to the launcher. Proven under a pty (own pgid with monitor on, shared pgid
# with it off). Here the option cannot outlive the spawn.
#
# no_monitor is what makes the child immune to the shell's exit rather than the
# `disown`, which is belt-and-braces: with monitor off the job table is empty, so
# `disown %+` finds nothing and the `|| disown $!` form is the one that applies.
#
# Output goes to the ccmorph log, never /dev/null. A background job whose 401,
# traceback or timeout is discarded cannot be diagnosed after the fact: the user is
# told the work "continues in the background" and nothing ever contradicts it,
# however it ended. Every one of these jobs is silent on its happy path, so the log
# stays quiet until something goes wrong.
_ccm_spawn_bg_logged() {
  setopt localoptions no_monitor
  # The redirection is prepared by THIS shell before the child starts, so an
  # unwritable log aborts the spawn before the executable ever runs — the job
  # silently never happens while the caller reports it started. Same boundary,
  # same fallback as the adapter's own log handling.
  local _log="${_CCMORPH_LOG:-/dev/null}"
  _ccm_log_init
  if ! { : >> "$_log" } 2>/dev/null; then
    _log=/dev/null
  fi
  ( "$@" ) >>"$_log" 2>&1 &
  disown %+ 2>/dev/null || disown $! 2>/dev/null
}

# True when the listing names a model the profile has no size or recent miss for.
# A plain-text pre-check, so the usual launch (nothing new) starts no interpreter:
# every model id already appears in the conf's context JSON or a manual override.
# A false positive only costs one sizer run; the sizer itself decides.
_ccm_listing_has_unsized() {  # $1 = profile, $2 = /v1/models JSON
  local conf="$_CCMORPH_DIR/$1.conf" id
  [[ -r "$conf" ]] || return 0
  local known="$(<"$conf")" line
  local -a excluded=()
  for line in ${(f)known}; do
    [[ "$line" == excluded_models=* ]] && excluded=(${(s:,:)line#excluded_models=})
  done
  # One row per model; a row the endpoint marks non-chat is never sized, so it
  # must not count as new either (bge-m3 would rerun the sizer every launch).
  local rows="${2//\},\{/$'\n'}"
  for id in ${(f)"$(print -r -- "$rows" | command grep -v '"chat_completion": *false' |
                      command grep -o '"id": *"[^"]*"' |
                      command sed 's/.*"\([^"]*\)"$/\1/')"}; do
    (( ${excluded[(Ie)$id]} )) && continue
    [[ "$known" == *"\"$id\""* || "$known" == *"context_window.$id="* ]] || return 0
  done
  return 1
}

# Free metadata only, off add/launch's critical path. Inference requires the
# operator's explicit `ccmorph probe --allow-inference --profile ... --model ...`.
_ccm_probe_nudge() {  # $1 = profile name
  [[ -x "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" ]] || return 0
  _ccm_spawn_bg_logged "$_CCMORPH_LIBEXEC_DIR/ccmorph-core" refresh-model-registry \
    --declared-only --profile "$1" --deadline 30
}

# One interpreter start for every window fact a launch asks, as one tab-separated
# line: the launched model's own record ("<window>\t<source>\t<budget>\t<send_id>",
# the --fields contract), the route ceiling's sibling maximum, and the saved
# auto-compact budget the engine's schema keeps. Three helpers each paid a full
# start for one of these (#321, #333). Each part keeps its own try, because each
# used to fail on its own: a broken resolver must not blank the saved budget the
# clamp still needs, and a settings-compose that cannot be read must not take the
# resolver's record with it. The launched model's own window is NOT seeded into
# field 5 -- the caller applies its seed when it reads the ceiling, so the field
# stays the route's own answer. The run happens even with no model: a routed
# launch still needs its ceiling and its saved budget.
_ccm_launch_window() {
  local profile="$1" model="${2:-}" resolver="$_CCMORPH_LIBEXEC_DIR/model-window"
  if [[ -n "$model" ]]; then
    _ccm_migrate_state || model=""
  fi
  [[ -x "$resolver" ]] || model=""
  { command cat <<'PY'
import importlib.machinery, importlib.util, os, sys

resolver, profile, model = sys.argv[1:4]
sources = sys.argv[4:]
try:
    spec = importlib.util.spec_from_loader("profile_context", importlib.machinery.SourceFileLoader("profile_context", resolver))
    store = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(store)
except Exception:
    store = None
fields = ["", "", "", "", "", ""]

# The settings loader is loaded in ITS own try, the diagnostics run's rule (#332):
# a settings-compose that cannot be read costs only the facts that read settings --
# on the separate runs this fold replaced, it never touched the resolver's record.
try:
    exec(os.environ["_CCM_SC_PY"], globals())
except Exception:
    _sc = None

# The merged settings view, read once and shared by the route ceiling and the
# saved budget: last writer wins, tombstones included, exactly as the projection
# merge reads it. merged_view always returns a dict; None means the composition
# itself failed and each fact below falls through on its own.
doc = None
try:
    doc = _sc.merged_view(sources)
except Exception:
    pass

# The launched model's own record, the --fields contract.
try:
    if model:
        fields[0:4] = store.record_fields(store.resolve(profile, model))
except Exception:
    pass

# The largest probed window among the models this route serves. claude-* ids are
# skipped: Claude Code ignores the env var for that family and sizes them from
# the id itself. Per-model narrowing is autoCompactWindow's job, not the
# ceiling's.
try:
    values = store.profile_windows(profile)
    best = 0
    models = doc.get("availableModels") if doc is not None else None
    if not isinstance(models, list):
        models = []
    for m in models:
        if not isinstance(m, str):
            continue
        m = m.split("[", 1)[0]
        # All FOUR tier aliases, not three: a bare `fable` reaching the registry would
        # be treated as a model id, and a profile that happens to have an entry under
        # that name would inflate the ceiling every routed model is then clamped to.
        if m.startswith("claude-") or m in {"opus", "sonnet", "haiku", "fable"}:
            continue
        w = values.get(m)
        if w:
            best = max(best, w)
    if best > 0:
        fields[4] = str(best)
except Exception:
    pass

# The saved auto-compact budget. Only a value the engine's schema keeps counts:
# out of range, wrong type, or absent all mean the engine never sees it. The
# schema check is acw_valid -- the one the projection merge itself uses -- so a
# saved budget written with a trailing .0 (an integer-valued float) is legitimate.
try:
    found = ""
    if doc is not None and "autoCompactWindow" in doc:
        v = doc["autoCompactWindow"]
        if _sc.acw_valid(v):
            found = str(int(v))
    fields[5] = found
except Exception:
    pass

print(*fields, sep="\t")
PY
  } | command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
    CCMORPH_STATE_DIR="$_CCMORPH_DIR" MODEL_WINDOW_PROFILE_CONF="$_CCMORPH_DIR/$profile.conf" \
    _CCM_LIB="$_CCMORPH_LIBEXEC_DIR" _CCM_SC_PY="$(_ccm_settings_compose_py)" \
    python3 - "$resolver" "$profile" "$model" \
    "${(f)$(_ccm_settings_sources "$profile")}" 2>/dev/null
}

# Ask a profile's own endpoint what models it serves.
#
# ccmorph deliberately carries no model knowledge: the endpoint is the only
# authority for what exists, the answer is never cached (so a gateway change is
# visible immediately), and the only filter applied is the endpoint's own
# advertised chat_completion capability. No per-model table, alias, or
# capability judgement belongs here.
# scheme://host[:port]/path only: drops userinfo, query and fragment so a URL
# can appear in a message or log without leaking a credential.
_ccm_redact_url() {
  command python3 - "$1" <<'PY' 2>/dev/null || print -r -- '[unparseable endpoint]'
import sys, urllib.parse
u=urllib.parse.urlsplit(sys.argv[1]); host=u.hostname or ''
if u.port: host += ':' + str(u.port)
print(urllib.parse.urlunsplit((u.scheme, host, u.path.rstrip('/'), '', '')))
PY
}

_ccm_gw_models_json() {
  local name="$1"
  local base key body
  REPLY=""

  _ccm_read "$name" || return 1
  _ccm_validate_loaded "$name" || {
    unset _ccm
    return 1
  }

  # Configure may be previewing an unsaved endpoint/key. Never query the old
  # endpoint with the replacement credential, or save just to populate a picker.
  key="${3-${_ccm[api_key]:-}}"
  base="${2-${_ccm[base_url]:-https://api.anthropic.com}}"
  base="${base:-https://api.anthropic.com}"
  unset _ccm

  [[ -n "$key" ]] || {
    _ccm_err "'$name' has no api_key — model discovery needs one"
    return 1
  }

  # No anthropic-version header on purpose. Anthropic-compatible proxies rewrite
  # non-claude ids into "claude-<junk>-<reversed-id>" when they see it, which makes
  # the listing unreadable and non-claude models look absent. The same endpoints
  # return clean ids without it, and the native Anthropic endpoint authenticates
  # before validating the version, so nothing is lost.
  local url
  _ccm_api_url "$base" "/v1/models?limit=1000"
  url="$REPLY"
  # No proxy override: curl reads whatever HTTPS_PROXY the caller's environment
  # carries, which is the one channel a bridge uses to make a private endpoint
  # reachable. Overriding it here blanked an ambient proxy and the request died.
  body=$(_ccm_curl_keyed both "$key" -sS -f --max-time 20 "$url") || {
    _ccm_err "could not reach '$(_ccm_redact_url "$base")' — if it is an internal endpoint, connect your private network and try again"
    return 1
  }
  _ccm_undisguise_ids "$body"
}

_ccm_gw_models() {
  local name="$1"
  _ccm_gw_models_json "$name" "${@:3}" || return 1

  print -r -- "$REPLY" | command python3 -c '
import json
import re
import sys

fields = {}
if sys.argv[2] == "picker":
    with open(sys.argv[1], encoding="utf-8") as source:
        fields = dict(line.rstrip("\n").split("=", 1) for line in source
                      if "=" in line and not line.startswith("#"))
excluded = set(fields.get("excluded_models", "").split(",")) - {""}

import runpy
_is_excluded = runpy.run_path(sys.argv[-1])["is_excluded"]


def is_excluded(model):
    return _is_excluded(fields, model)

try:
    payload = json.load(sys.stdin)
except (json.JSONDecodeError, TypeError):
    raise SystemExit(1)

rows = []
for m in payload.get("data", []):
    if not isinstance(m, dict):
        continue
    # Exclude a model only when the endpoint explicitly denies chat use. Most
    # endpoints (the native Anthropic list, Anthropic-shaped local translators)
    # publish no capabilities at all, and an undeclared capability is not a
    # denial -- filtering on its absence would hide every model they serve.
    caps = m.get("capabilities")
    if isinstance(caps, dict) and caps.get("chat_completion") is False:
        continue
    model_id = m.get("id")
    if not isinstance(model_id, str) or not model_id or is_excluded(model_id):
        continue
    rows.append((model_id, m.get("owned_by") or "?"))

if not rows:
    raise SystemExit(2)

for model_id, owner in sorted(rows):
    print(f"{model_id}\t{owner}")
' "$_CCMORPH_DIR/$name.conf" "${2:-catalogue}" "$_CCMORPH_LIBEXEC_DIR/model-window" || {
    _ccm_err "'$name' endpoint returned no usable model list"
    return 1
  }
}

# Does this profile's own resolver serve this model?  0 = yes, 1 = no, 2 = cannot tell.
#
# A model recorded in a session is a fact about the profile that RAN it, not about
# the one taking it over. Both carriers -- `hop`, and a direct `--resume` -- hand
# that id to the next launch unconditionally, so hopping a gateway model into a
# Claude profile sends Anthropic a name it has never heard of, and the reverse sends
# a gateway an id it does not serve. Ask first, and let a definite no fall back to
# the target's own default, which is what omitting the model already means.
#
# Whom to ask is the whole design, and there is no third authority to invent:
#
#   * No base_url -- Claude Code resolves the id itself, out of its built-in
#     catalogue. Membership there is the claude- prefix: the same test the launcher
#     already applies when deciding whether an id needs
#     CLAUDE_CODE_MAX_CONTEXT_TOKENS, for the same reason. Free, offline, certain.
#   * A base_url -- the endpoint's own /v1/models, through `_ccm_gw_models`, so the
#     answer routes exactly as `ccmorph models` does and no per-model table is born
#     here either.
#
# A tier alias is not an id and never fails: each profile maps it through its own
# ANTHROPIC_DEFAULT_<TIER>_MODEL, so it is servable wherever it lands.
#
# Everything else -- no key, endpoint down, empty listing -- is ignorance, not
# absence, and answers 2 so the carried model survives. Dropping a model because a
# gateway blinked is the worse of the two failures: the model is the user's choice,
# and the fallback is only ever a guess at it.
_ccm_settings_model_alias() {
  local tier="${(U)2}" value
  [[ "$tier" == (OPUS|SONNET|HAIKU|FABLE) ]] || return 1
  value=$(_ccm_config_store get "$1" "env.ANTHROPIC_DEFAULT_${tier}_MODEL") || return 1
  [[ -n "$value" ]] || return 1
  print -r -- "$value"
}

_ccm_model_known() {
  local name="$1"
  local want="$2"
  # Claude Code recognizes the suffix case-insensitively; strip only that terminal
  # marker, never brackets that belong to the model id itself.
  [[ "${(L)want}" == *'[1m]' ]] && want="${want[1,-5]}"

  [[ -n "$name" && -n "$want" ]] || return 2
  [[ "$want" == (opus|sonnet|haiku|fable) ]] && return 0

  _ccm_read "$name" >/dev/null 2>&1 || return 2
  local base="${_ccm[base_url]:-}"
  local _mk_type="${_ccm[type]:-}"
  local model_map="${_ccm[model_map]:-}"
  unset _ccm
  # These native routes launch without an adapter/shim, so their maps are unused.
  case "$base" in
    ""|https://api.anthropic.com|https://api.anthropic.com/) model_map="" ;;
  esac

  # Provider profiles have no base_url but are not native Anthropic — the
  # provider proxy serves its own model catalogue at runtime, which is not
  # queryable outside a live session. Two sub-cases:
  #   * a claude-* id is a DEFINITE no (1): a provider proxy serves its own
  #     non-Anthropic catalogue and 400s on Anthropic ids, so carrying one in is
  #     a guaranteed failure — say no and let the caller fall back to the default.
  #   * anything else is unknowable offline (2), so the carried model survives.
  if [[ "$_mk_type" == provider ]]; then
    [[ "$want" == claude-* ]] && return 1
    return 2
  fi

  if [[ -z "$base" ]]; then
    [[ "$want" == claude-* ]]
    return $?
  fi

  _ccm_gw_models_json "$name" 2>/dev/null || return 2
  local body="$REPLY"
  [[ -n "$body" ]] || return 2

  _CCM_MODELS_JSON="$body" command python3 -c '
import json, os, sys
try:
    payload = json.loads(os.environ["_CCM_MODELS_JSON"])
except Exception:
    raise SystemExit(2)
# One lookup on the wire id, like the shim (#285); a mapped target is final.
model_map = json.loads(sys.argv[2] or "{}")
mapped = model_map.get(sys.argv[1])
want = sys.argv[1] if mapped is None else mapped
if mapped is not None and want.endswith("[1m]"):
    want = want[:-4]
uncertain = False
for m in payload.get("data", []):
    if not isinstance(m, dict):
        uncertain = True
        continue
    model_id = m.get("id")
    if not isinstance(model_id, str) or not model_id:
        uncertain = True
        continue
    if model_id != want:
        continue
    caps = m.get("capabilities")
    raise SystemExit(1 if isinstance(caps, dict) and caps.get("chat_completion") is False else 0)
raise SystemExit(2 if uncertain or payload.get("has_more") is True else 1)
' "$want" "$model_map"
}
