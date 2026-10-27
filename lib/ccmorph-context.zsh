# ccmorph-context.zsh — Context window and settings: auto-compact trigger maths, window hazards and advisories, settings layering and the projection into CLAUDE_CONFIG_DIR.
# Sourced by ccmorph.sh after ccmorph-ui.zsh (see its Modules block); never run directly.

# The percentage the ENGINE will honour, and the threshold it computes from it, as
# Python source that every embedded program includes verbatim. Two copies of this
# rule is exactly how it drifted: _ccm_invariant_warn carried a hand-fitted 0.15
# buffer matching no engine function, and named safe models fatal across a
# 49,000-token band.
#
# parseFloat semantics, not float(): a leading numeric PREFIX, so "90junk" is 90.
# ASCII digits only (the Python \d class is Unicode-aware, parseFloat is not), the
# ECMAScript whitespace set (Python \s is wider), and the engine guard
# !isNaN(r) && r > 0 && r <= 100.
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
  # xe() is `if (Wn()) return false; return /\[1m\]/i.test(e)`, so with the kill switch
  # set a [1m] id resolves at 200,000, not 1,000,000. Computing the trigger from 1e6
  # anyway names a safe model fatal -- and withholds it outright under
  # CCMORPH_WITHHOLD_FATAL=1, costing the user a model that works.
  # The engine's own env boolean, read out of 2.1.247:
  #
  #   function p(e){ if(!e) return false; if (typeof e === "boolean") return e;
  #                  let n = String(e).toLowerCase().trim();
  #                  return ["1","true","yes","on"].includes(n) }
  #
  # So `off`, `no`, `false` and `2` are all FALSE. Treating anything non-empty and
  # non-zero as true called them true, which sizes a [1m] id at 200,000 where the engine
  # gives it 1,000,000 -- the direction that leaves a wedge unnamed.
  # extendedglob for the ## quantifier: without it the trim patterns match nothing and
  # a padded value silently reads as false.
  setopt localoptions extendedglob
  # The ECMAScript WhiteSpace set, NOT [[:space:]]: the spec table includes U+FEFF
  # (ZWNBSP), which zsh does not count as space -- so the engine trims `<FEFF>on` to
  # `on` and calls it true where ccmorph read it as false, sizing a [1m] id at
  # 1,000,000 against the engine 200,000 and naming a safe model fatal. Python
  # str.strip() leaves U+FEFF too, which is what made checking against Python say the
  # two agreed: the same wrong oracle as the percentage-parser gap.
  # The ECMAScript WhiteSpace + LineTerminator production, one array element per
  # CHARACTER -- deliberately not a bracket class. Under a C charmap a bracket class
  # matches BYTES, so a multibyte character whose UTF-8 bytes all appear in the class is
  # eaten byte by byte: `<U+3080>on` (e3 82 80) trimmed to `on` and read as true where
  # the engine reads false. Comparing whole characters is exact in every locale, which a
  # class cannot be, and MULTIBYTE cannot override a C charmap to rescue it.
  #
  # The non-ASCII members are written as literal UTF-8, not $'\uXXXX': under a C charmap
  # zsh rejects those escapes outright ("character not in range"), so the array would fail
  # to build in exactly the locale this is here to handle.
  local -a _ws
  _ws=($'\t' $'\n' $'\v' $'\f' $'\r' ' ' ' ' '﻿' ' ' ' ' ' ' ' ' ' ' ' ' ' ' ' ' ' ' ' '
      ' ' ' ' ' ' ' ' ' ' ' ' '　')
  _1m="${(L)_1m}"
  local _w _cut=1
  while (( _cut )); do
    _cut=0
    for _w in "${_ws[@]}"; do
      [[ "$_1m" == "$_w"* ]] && { _1m="${_1m#$_w}"; _cut=1 }
      [[ "$_1m" == *"$_w" ]] && { _1m="${_1m%$_w}"; _cut=1 }
    done
  done
  case "$_1m" in
    1|true|yes|on) _1m=1 ;;
    *)             _1m=""  ;;
  esac
  # Assigned BY NAME into the caller's variables, not printed as delimited fields: an
  # env value may legitimately contain any byte, and a conf `env.` line carrying a tab
  # survives parsing intact -- which split a tab-separated return into the wrong
  # fields, handing the percentage parser "8" and the reserve reader "0<TAB>9000".
  # With no delimiter there is nothing to corrupt.
  : ${(P)1::=$_pct}
  : ${(P)2::=$_out}
  : ${(P)3::=$_1m}
}

_ccm_pct_parser() {
  command cat <<'PYPCT'
_JS_WS = "\t\v\f \u00a0\ufeff\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\n\r"


def engine_pct(raw):
    # The numeric portion in its OWN group, so the skipped whitespace never reaches
    # float(). Python float() accepts a leading NBSP, U+2000-200A, U+3000 and the
    # rest -- but rejects U+FEFF, which parseFloat skips like any other. FEFF is the
    # single diverging char in the class, so folding the prefix into the parse
    # dropped an engine-valid override and named a safe model fatal.
    m = re.match(r"[" + _JS_WS + r"]*([+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)",
                 raw or "")
    if not m:
        return None
    try:
        parsed = float(m.group(1))
    except ValueError:
        return None
    if parsed != parsed or not (0 < parsed <= 100):
        return None
    return parsed


# cV() resolves max-output per MODEL, and RL() then caps the reserve at 20,000. Only ids
# whose (default, upper) fall BELOW that cap can change the answer, so this table is the
# complete set of those -- five, enumerated from the 2.1.247 catalogue plus the three
# hardcoded ids, not a sample:
#
#   MM() hardcoded:   claude-3-opus 4096/4096   claude-3-haiku 4096/4096
#                     claude-3-sonnet 8192/-
#   catalogue rows:   claude-3-5-haiku 8192/8192   claude-3-5-sonnet 8192/8192
#
# Every other catalogue row is 32000 or 64000 and already floors to 20,000 through the
# fallback, so importing the catalogue would add rows that cannot matter.
#
# A smaller reserve means a LARGER usable window and a HIGHER trigger -- the direction that
# leaves a wedge unnamed. On a 200,000 window at pct 80: 8192 reserve triggers at 153,446
# and 4096 at 156,723, against 144,000 for a flat 20,000.
_MODEL_OUT = {
    "claude-3-opus": (4096, 4096),
    "claude-3-haiku": (4096, 4096),
    "claude-3-sonnet": (8192, 8192),
    "claude-3-5-haiku": (8192, 8192),
    "claude-3-5-sonnet": (8192, 8192),
}


def engine_reserve(model_id, pinned):
    """RL(): min(effective max-output-tokens, 20000), resolved per model as cV() does.

    Keyed by lowercased SUBSTRING, the way Et() normalizes -- `includes("claude-3-opus")`
    and a trailing eight-digit date strip -- so a dated id and a mixed-case one both
    fold to their base. An exact case-sensitive match missed every dated and
    mixed-case id.

    An env pin is not authoritative: yN caps it at the model upperLimit before RL sees it
    (`if(o>r){effective:r,status:"capped"}`), so a pin above a sub-cap model limit is
    silently reduced by the engine. Using it verbatim under-reserved and hid a wedge.
    """
    bare = model_id.split("[", 1)[0].lower()
    # The fallback pair is deliberately the 20,000 cap itself rather than a real catalogue
    # figure: every non-sub-cap model declares 32000 or more, and both a default and a pin
    # are then floored to 20,000 regardless -- so the true numbers cannot change the answer,
    # and writing one here would put a window-shaped literal in the source for no gain.
    default = upper = 20_000
    for key, (d, u) in _MODEL_OUT.items():
        if key in bare:
            default, upper = d, u
            break
    effective = min(pinned, upper) if pinned else default
    return min(effective, 20_000)


def engine_trigger(usable, pct):
    """xWe(): floor(usable*(pct/100)), capped 13,000 below usable.

    Divide BEFORE multiplying, as Math.floor(e*(r/100)) does. Floating-point
    multiplication is not associative, so multiplying first lands one token high,
    and a model walled exactly at the trigger then reads as fatal when it is safe.
    """
    n = usable - 13_000
    if pct is None:
        return n
    return min(int(usable * (pct / 100)), n)
PYPCT
}

_ccm_policy_pct_py() {
  # The fleet context policy's autoCompactAtPercent, or None when the file is
  # missing, malformed, or outside the schema this version understands. Shared
  # by the reader that prints it and by the one interpreter that answers every
  # read-only launch diagnostic (#332), so both judge a policy the same way.
  command cat <<'PYPOL'
def policy_pct(path):
    try:
        with open(path, encoding="utf-8") as fh:
            doc = json.load(fh)
        value = doc["autoCompactAtPercent"]
        if doc.get("schemaVersion") != 1 or isinstance(value, bool) or not isinstance(value, int) or not 0 < value < 100:
            raise ValueError
    except Exception:
        return None
    return value
PYPOL
}

_ccm_context_parser() {
  # Compatibility shape for the safety calculations, backed only by this profile.
  command cat <<'PYREG'
def parse_context(path):
    import importlib.machinery, importlib.util
    resolver = os.environ["_CCM_CONTEXT_RESOLVER"]
    spec = importlib.util.spec_from_loader("profile_context", importlib.machinery.SourceFileLoader("profile_context", resolver))
    store = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(store)
    os.environ["MODEL_WINDOW_PROFILE_CONF"] = path
    return store.profile_windows(os.environ["_CCM_PROFILE"])
PYREG
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
  # Composing the hazard snippets is a `cat` fork and ~9 KB of python source on
  # every call, and only the launch reads the record they serve.
  local hazard_snips=""
  [[ -n "${_CCM_DIAG_HAZARD:-}" ]] &&
    hazard_snips="$(_ccm_hazard_py)"$'\n'"$(_ccm_hazard_msg_py)"
  _CCM_LIB="$_CCMORPH_LIBEXEC_DIR" \
  _CCM_SC_PY="$(_ccm_settings_compose_py)" \
  _CCM_SETTINGS="$settings" \
  _CCM_PROFILE_CONF="$_CCMORPH_DIR/$profile.conf" \
  _CCM_CONTEXT_RESOLVER="$_CCMORPH_LIBEXEC_DIR/model-window" \
  _CCM_PCT_IN_ENV="$pct_in_env" _CCM_ENV_PCT="$pct" _CCM_MAXOUT="$maxout" \
  _CCM_NO1M="$no1m" _CCM_BUDGET="$budget" _CCM_POLICY="$_CCMORPH_CONTEXT_POLICY" \
  _CCM_POLICY_PCT="$policy_pct" _CCM_PROFILE="$profile" _CCM_HDR_KEY="$hdr_key" \
  _CCM_HAZARD="${_CCM_DIAG_HAZARD:-}" _CCM_CEILING="${_CCM_DIAG_CEILING:-}" \
  _CCM_WITHHELD="${CCMORPH_WITHHOLD_FATAL:-0}" \
    command python3 -c '
import json, os, re, sys
'"$(_ccm_pct_parser)"'
'"$(_ccm_context_parser)"'
'"$(_ccm_policy_pct_py)"'
'"$hazard_snips"'


def invariant_records():
    """The two warning blocks, each keeping its own failure rules.

    They were separate interpreters before this, so neither could silence the
    other: the disguised check still answers when the window lookup below dies,
    and the other way round.
    """
    records = []
    settings = os.environ.get("_CCM_SETTINGS", "")

    # The disguised check, moved verbatim: an id a -dd- suffix hides behind a
    # claude- prefix caps the window at 200,000 however the profile is
    # configured. Every failure printed nothing, as its SystemExit(1) did.
    dm = ""
    try:
        with open(settings, encoding="utf-8") as fh:
            m = json.load(fh).get("model")
    except Exception:
        m = None
    if isinstance(m, str) and re.match(r"^claude-.*-dd(-|$)", m):
        head, _, tail = m.partition("-dd-")
        dm = (tail[::-1] if tail else m).rstrip("\n")
    if dm:
        records.append(("disguised", dm))

    # The unsafe check below, moved verbatim but for where its lines go. Its exit
    # code was everything its caller saw: an unreadable settings file exited 0
    # having printed nothing, and any other failure exited non-zero, which threw
    # away the lines it had already printed.
    lines = []
    try:
        with open(settings, encoding="utf-8") as fh:
            allowed = json.load(fh).get("availableModels") or []
    except (OSError, json.JSONDecodeError, AttributeError):
        return records
    try:
        ceilings = parse_context(os.environ["_CCM_PROFILE_CONF"])


        # Already parsed by _ccm_engine_int at the call site, which prints a positive integer
        # or nothing -- so an empty value means the engine falls back to the MODEL default, which
        # engine_reserve() knows per id.
        _raw_out = os.environ.get("_CCM_MAXOUT", "").strip()
        _PINNED_OUT = int(_raw_out) if _raw_out.isdigit() else 0


        import runpy
        real_id = runpy.run_path(os.environ["_CCM_CONTEXT_RESOLVER"])["real_id"]


        _NO_1M = bool(os.environ.get("_CCM_NO1M", ""))
        # The session budget, which CL() mins into the window of EVERY model. Absent means no
        # in force, so each family ceiling stands on its own.
        _raw_budget = os.environ.get("_CCM_BUDGET", "").strip()
        _BUDGET = int(_raw_budget) if _raw_budget.isdigit() else 0
        _PROFILE = os.environ.get("_CCM_PROFILE", "")


        def window(model_id):
            """The window Claude Code will give this id, by its own resolution order."""
            # xe() tests the tag case-insensitively, and returns false outright when
            # CLAUDE_CODE_DISABLE_1M_CONTEXT is set.
            # xe() is the UNANCHORED regex /\[1m\]/i, so the tag counts anywhere in the id --
            # a gateway that version-suffixes after it (`claude-x[1m]-eu`) still gets
            # 1,000,000. An endswith test gave such an id 200,000 here and left its wedge
            # unnamed.
            if not _NO_1M and re.search(r"\[1m\]", model_id, re.IGNORECASE):
                return min(1_000_000, _BUDGET) if _BUDGET else 1_000_000
            if model_id.startswith("claude-"):
                # A claude- prefix skips CLAUDE_CODE_MAX_CONTEXT_TOKENS, so a disguised
                # third-party model lands on the default however the profile is configured --
                # and the session budget still caps it, since CL() mins every model.
                return min(200_000, _BUDGET) if _BUDGET else 200_000
            return None          # derived per launch; the derivation already bounds it


        for entry in allowed:
            if not isinstance(entry, str):
                continue
            w = window(entry)
            if w is None:
                continue
            # RL(): the reserve is min(effective max-output-tokens, 20000), not a flat 20,000.
            # A pin below the cap makes the engine reserve LESS and trigger HIGHER -- at 8,000
            # with no percentage override the engine triggers at 179,000 where a flat reserve
            # computes 167,000, so a model walled at 170,000 is fatal and went unnamed. The
            # hazard set was fixed for this in an earlier round; this copy was not, and
            # _ccm_hazard_set cannot cover the gap because it exits when no window is known
            # (a first launch with the endpoint down is exactly that case).
            usable = w - min(w, engine_reserve(entry, _PINNED_OUT))
            # xWe(), the same threshold the hazard set computes, from the percentage this
            # launch actually injects. A hand-fitted `usable - round(usable*0.15)` sat here
            # and matched no engine function at all: on a 1,000,000 window it read 833,000
            # where the engine compacts at 784,000, so every model holding between those two
            # was named as dying mid-session when the engine handles it fine -- a 49,000-token
            # band of false alarms, and 9,000 on a 200,000 window.
            trigger = engine_trigger(usable, engine_pct(os.environ.get("_CCM_PCT", "")))
            # The same lookup the sibling function does: strip the [1m] window marker (the
            # registry is keyed on ids, and the writer emits only bare ones), and try the
            # profile-scoped key first. Without either, this ceiling lookup missed every
            # [1m]-tagged entry -- making the branch above dead code -- and every one of the
            # profile-scoped rows the registry actually holds.
            _bare = real_id(entry).split("[", 1)[0]
            ceiling = ceilings.get(f"{_PROFILE}:{_bare}") or ceilings.get(_bare)
            if ceiling is not None and trigger >= ceiling:
                lines.append(f"{_bare} holds {ceiling} but compaction starts at {trigger}")
    except SystemExit as exc:
        # Today a non-zero exit threw away every line the script had printed.
        if exc.code not in (0, None):
            return records
    except Exception:
        return records
    if lines:
        records.append(("unsafe", "\n".join(lines).rstrip("\n")))
    return records


keys = sys.argv[1:sys.argv.index("--")]
paths = sys.argv[sys.argv.index("--") + 1:]


def emit(*fields):
    # The framing is NUL-delimited, so a field may hold any byte but NUL: a NUL
    # inside one would end its record early and the rest would be parsed as
    # records of their own, so such a record is dropped whole. Enforced here,
    # at the framer, because every field of every kind crosses it.
    if any(chr(0) in field for field in fields):
        return
    sys.stdout.write("\t".join(fields) + "\0")


# The settings loader is loaded in ITS own try. A settings-compose that cannot be
# read answers none of the env questions -- and today that killed only the two env
# readers, never the invariant warnings, which read settings.json themselves.
try:
    exec(os.environ["_CCM_SC_PY"], globals())
    env = _sc.merged_view(paths).get("env")
except Exception:
    env = None
env = env if isinstance(env, dict) else {}


# Presence is not an override: Claude Code drops a percentage it cannot parse, so
# treating an invalid value as set would leave the launch with neither the choice
# nor our default.
_PCT_KEY = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
pct_set = isinstance(env.get(_PCT_KEY), str) and engine_pct(env[_PCT_KEY]) is not None
if pct_set:
    emit("pct-set")


# The percentage the ENGINE will use, which the unsafe check needs: the launch env
# wins, then the layers, then the fleet policy. The middle step is the one the
# launch leaves to the layers on purpose -- it injects nothing over a value the
# user set, because penv would reach the engine over it -- so the layer value IS
# what the engine reads there, and skipping it computed the no-percentage trigger
# `usable - 13000` instead, naming every model between the two as fatal (#428).
# A policy that does not parse stops the launch before any warning, so it stops
# the warnings here too.
pct = os.environ.get("_CCM_ENV_PCT", "")
run_invariant = bool(os.environ.get("_CCM_SETTINGS", ""))
if os.environ.get("_CCM_PCT_IN_ENV") != "1":
    if pct_set:
        pct = env[_PCT_KEY]
    else:
        _policy = os.environ.get("_CCM_POLICY_PCT", "")
        if _policy:
            pct = _policy
        else:
            value = policy_pct(os.environ.get("_CCM_POLICY", ""))
            if value is None:
                run_invariant = False
            else:
                emit("policy", str(value))
                pct = str(value)


# An empty string is not a choice: the engine reads it as unset, so counting it
# would skip the default AND leave the engine with nothing.
for key in keys:
    if isinstance(env.get(key), str) and env[key]:
        emit("chosen", key)


# Lines the user set for the header key this launch replaces in the process
# env: the projection strips that key from the generated env block, so the
# launcher merges this value in front of its tag lines instead of replacing it.
# An empty _CCM_HDR_KEY asks nothing (the launch carried no tags).
hdr_key = os.environ.get("_CCM_HDR_KEY", "")
if hdr_key:
    value = env.get(hdr_key)
    if isinstance(value, str):
        value = value.rstrip("\n")
        if value:
            emit("userset", value)


if run_invariant:
    os.environ["_CCM_PCT"] = pct
    for kind, text in invariant_records():
        emit(kind, text)


# The switch hazard, from the interpreter that already holds the merged view, the
# context resolver and the percentage this launch injects. Every guard the
# two-interpreter path passes is repeated here in the same order -- a source file
# that exists, then the profile context conf, then a settings-compose that loaded
# -- so a shape the old path answered gets an answer here too, and anything else
# emits nothing and leaves the caller its fallback. The record is emitted even
# when the message is empty: "no model is unsafe" IS the answer, and paying two
# interpreters to hear `return 0` is the cost this exists to remove.
if os.environ.get("_CCM_HAZARD") == "1" and "_sc" in globals():
    try:
        _haz_conf = os.environ["_CCM_PROFILE_CONF"]
        _haz_srcs = [p for p in paths if p.strip()]
        if any(os.path.isfile(p) for p in _haz_srcs) and os.path.isfile(_haz_conf):
            # hazard_rows sizes from the percentage the ENGINE will use, so it may
            # read the layers only when that percentage comes from them: the process
            # env carried none and this launch is not about to inject one over a
            # value the engine drops (a layer percentage that does not parse --
            # presence is not validity). `pct` above then already holds the value
            # the engine gets instead, so this is the same answer the
            # two-interpreter fallback computes, which runs after the injection.
            _pct_from_env = os.environ.get("_CCM_PCT_IN_ENV") == "1" or (not pct_set and pct != "")
            _haz_rows = hazard_rows(_sc, os.environ.get("_CCM_PROFILE", ""), os.environ.get("_CCM_BUDGET", ""), os.environ.get("_CCM_CEILING", ""), _haz_srcs, pct, os.environ.get("_CCM_MAXOUT", "").strip(), bool(os.environ.get("_CCM_NO1M", "")), _haz_conf, _pct_from_env)
            emit("hazard", hazard_message(_haz_rows, os.environ.get("_CCM_WITHHELD") == "1"))
    except Exception:
        pass
' "$@" -- "${sources[@]}" 2>/dev/null
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

_ccm_settings_compose_py() {
  # Python prelude binding `_sc` to settings-compose, whose merged_view() is the one
  # reading of the layers (merge plus tombstones). Needs _CCM_LIB in the environment.
  print -r -- '
import importlib.machinery, importlib.util, os
_sp = importlib.util.spec_from_loader("settings_compose", importlib.machinery.SourceFileLoader("settings_compose", os.path.join(os.environ["_CCM_LIB"], "settings-compose")))
_sc = importlib.util.module_from_spec(_sp)
_sp.loader.exec_module(_sc)'
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
  # The auto-compact budget the ENGINE will use, in its own precedence order:
  #
  #   1. env CLAUDE_CODE_AUTO_COMPACT_WINDOW -- read before the settings layer, and
  #      CLAMPED there (`Math.max(1e5, effective)` over a bounded parse).
  #   2. a saved autoCompactWindow -- subject to the setting's schema
  #      (`.int().min(1e5).max(1e6)`), and an out-of-range value is DROPPED, not
  #      clamped, so the session falls through to the ceiling.
  #   3. the value ccmorph derived for the launched model.
  #
  # The env/saved asymmetry is the whole reason this is one function: two
  # open-coded copies gave the clamp to one and not the other, and a saved 50,000
  # then produced a 17,000 trigger where the engine used 784,000.
  local profile="$1" derived="${2:-}" _e=""
  for _e in "${penv[@]}"; do
    if [[ "$_e" == CLAUDE_CODE_AUTO_COMPACT_WINDOW=* ]]; then
      # Parsed the way the engine parses it, not as pure digits: `1e5`, `100_000`
      # and `100000junk` are all valid budgets there, and rejecting them here meant
      # falling through to a DIFFERENT budget and reporting the wrong trigger. A
      # value the engine calls invalid (<= 0, or no leading digits) yields empty and
      # falls through, exactly as its own bounded parse does.
      local _env=""
      _env=$(_ccm_engine_int "${_e#*=}") || _env=""
      if [[ "$_env" == <1-> ]]; then
        _ccm_clamp_acw _env
        print -r -- "$_env"
        return 0
      fi
    fi
  done

  # The saved autoCompactWindow, from the one-run window facts when the launch
  # already computed them (passed as $3 -- field 6, already schema-checked and
  # last-writer-wins-merged there), else from the same one run started here. A
  # profile that pins its ceiling in conf skips the launch's run, so this path
  # still pays its own start rather than losing the saved budget.
  local _saved=""
  if (( $# >= 3 )); then
    _saved="$3"
  else
    local -a _lw=()
    _lw=("${(@ps:\t:)$(_ccm_launch_window "$profile" "")}")
    _saved="${_lw[6]:-}"
  fi
  if [[ "$_saved" == <-> ]]; then
    print -r -- "$_saved"
    return 0
  fi

  [[ -n "$derived" ]] && print -r -- "$derived"
  return 0
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
  command python3 - "${1:-}" <<'PY' 2>/dev/null
import re
import sys

# ECMAScript whitespace, not Python whitespace: str.strip() also removes U+0085,
# which JavaScript trim does NOT, so a value the engine rejects was being accepted.
JS_WS = "\t\v\f  ﻿  -     　\n\r"
_raw = sys.argv[1] if len(sys.argv) > 1 else ""
raw = re.sub(r"^[" + JS_WS + r"]+|[" + JS_WS + r"]+$", "", _raw)
# ASCII digits ONLY. Python's \d is Unicode-aware and int() converts Arabic-Indic
# digits, but JavaScript's \d is ASCII and parseInt rejects them -- so a value the
# engine calls invalid was being accepted here, and the trigger computed from a budget
# the session does not have.
EXP = re.compile(r"^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)[eE][+-]?[0-9]+$")
GROUPED = re.compile(r"^[+-]?[0-9]{1,3}([_,\u00a0\u202f ])[0-9]{3}(?:\1[0-9]{3})*$")
SEPS = re.compile(r"[_,\u00a0\u202f ]")


def engine_int(v):
    if len(v) <= 32:
        if EXP.match(v):
            n = float(v)
            # inf and nan reach here from forms like 1e999, and int() raises
            # OverflowError on them -- which made this whole helper exit non-zero, so
            # every reader fell through to a different budget instead of matching the
            # engine, which reads the same value as NaN and simply calls it invalid.
            if n != n or n in (float("inf"), float("-inf")):
                return None
            return int(n) if n == int(n) else None      # Number.isInteger check
        if GROUPED.match(v):
            return int(SEPS.sub("", v))
    m = re.match(r"[+-]?[0-9]+", v)                     # parseInt prefix, ASCII
    return int(m.group(0)) if m else None


n = engine_int(raw)
if n is not None and n > 0:
    # Cap at a value zsh arithmetic can hold. The engine bounds every consumer of
    # this parse (yN caps at its own upper limit), and an exponent form like 1e21
    # otherwise reaches zsh as 22 digits -- which truncates to a NEGATIVE number and
    # made the ceiling read as "-8446744073709551k". Callers clamp to their real
    # limits; this only keeps the value representable on the way there.
    print(min(n, 10 ** 18))
PY
}

_ccm_clamp_acw() {
  # Clamp an auto-compact budget into the range the engine will actually honour,
  # in place, by name. Both the settings key and the env var are bounded there --
  # the key by its schema (`.int().min(1e5).max(1e6)`, out-of-range values dropped
  # entirely) and the env var by `Math.max(1e5, ...)` over a bounded parse. A value
  # outside the range therefore never reaches the trigger as written, so computing
  # from the raw number reports a window the session does not have.
  local _name="$1" _v="${(P)1}"
  [[ "$_v" == <-> ]] || return 0
  # Bound by LENGTH before any arithmetic: zsh integers are 64-bit, so a very long
  # decimal string overflows rather than comparing greater, and a wrapped value can
  # slip past the ceiling test. The engine caps at the same maximum, so anything with
  # more digits than the ceiling is simply the ceiling.
  if (( ${#_v} > ${#_CCMORPH_ACW_MAX} )); then
    _v=$_CCMORPH_ACW_MAX
  else
    (( _v < _CCMORPH_ACW_MIN )) && _v=$_CCMORPH_ACW_MIN
    (( _v > _CCMORPH_ACW_MAX )) && _v=$_CCMORPH_ACW_MAX
  fi
  : ${(P)_name::=$_v}
}

# _ccm_hazard_set's arithmetic as a named snippet, composed like the
# settings-compose, pct and context parsers beside it. Two runs hold it: the
# diagnostics interpreter, whose `hazard` record a launch asks for and which is
# given the percentage the launch is about to inject, and _ccm_hazard_set, the
# fallback that runs after the injection. hazard_rows() is the whole
# calculation: it takes what the env vars used to hold and returns the rows to
# print, or an empty list when there is nothing to say.
_ccm_hazard_py() {
  command cat <<'PYHAZ'
def num(v):
    return int(v) if v.isdigit() else 0


def hazard_rows(sc, profile, budget, ceiling, sources, pct_raw, raw_out, no_1m, conf,
                pct_in_env):
    budget, ceiling = num(budget), num(ceiling)

    # The merged view of the layers (canon first, profile on top, tombstones honoured),
    # the same reading every other settings reader uses. A single source is the
    # already-merged generated settings.json itself.
    doc = sc.merged_view(sources)

    # The percentage the engine will use, resolved in the order the engine sees it:
    # the launch env when it carries the key, the layers otherwise. The launch injects
    # its own percentage only when the layers set none (#428 -- a penv value would reach
    # the engine over the user's own choice), so a layer value IS the engine's when the
    # launch env is silent, and it arrives through this same overlay. Reading the launch
    # env alone computed the no-percentage trigger `usable - 13000`, which is HIGHER than
    # the engine's own, so every model between the two triggers was named by the advisory
    # and -- with CCMORPH_WITHHOLD_FATAL=1 -- silently dropped from the picker, although
    # the session compacts long before reaching that model's wall.
    #
    # Presence, not validity: the launch's own gate is presence -- it injects only when
    # penv does not carry the key at all -- so this reads whichever value the launch let
    # through, and a malformed one still resolves to no override in the parse below.
    if not pct_in_env:
        env = doc.get("env")
        layer_pct = env.get("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE") if isinstance(env, dict) else None
        if isinstance(layer_pct, str):
            pct_raw = layer_pct

    # The engine parses this override with parseFloat and IGNORES it unless it is a
    # finite number in (0, 100] -- an unset, malformed, zero or out-of-range value
    # leaves the default 20% precompute reserve in charge rather than disabling the
    # trigger. Treating a bad value as "no trigger" made ccmorph fall silent on a
    # session the engine would still have compacted (and still would have refused a
    # switch in), which is the one thing this advisory exists to prevent.
    # JavaScript parseFloat, not Python float(): the engine reads this with parseFloat,
    # which consumes a leading numeric PREFIX and ignores whatever follows, so
    # parseFloat("90junk") is 90. Rejecting the whole value instead dropped a live
    # override and computed the default trigger, naming a safe model fatal.
    raw = pct_raw
    # engine_pct() from the shared parser above: parseFloat semantics, the ECMAScript
    # whitespace class, ASCII digits, and the engine guard !isNaN(r) && r>0 && r<=100.
    # This was a second hand-written copy of that rule until the invariant warning needed
    # it too -- and a second copy is how the trigger formula drifted in the first place.
    pct = engine_pct(raw)
    window = min(x for x in (budget, ceiling) if x) if (budget or ceiling) else 0
    if not window:
        return []
    # RL(): the reserve is min(effective max-output-tokens, 20000), and cV() reads
    # CLAUDE_CODE_MAX_OUTPUT_TOKENS for that figure (routed default 32000, above the
    # cap, so the common case is a flat 20000). yN does not lower-clamp, so a small pin
    # makes the engine reserve LESS and trigger HIGHER -- the direction where ccmorph
    # under-reports and a fatal switch goes unnamed.
    reserve = 20_000
    # Already parsed the way the engine parses it; empty means the engine rejects
    # the value and keeps its own default.
    # _ccm_engine_int is the only writer, and it prints a positive integer or nothing --
    # so re-validating here would be guarding a condition the producer guarantees.
    if raw_out:
        reserve = min(int(raw_out), 20_000)
    usable = window - min(window, reserve)
    # xWe() ALONE, which is the threshold YRt returns "compact" at:
    #
    #   function YRt(e,t,n,r=t,o){ let s = o ?? xWe(t,n); ...
    #                              if (n.enabled && e >= s) return {level:"compact"} }
    #   function xWe(e,t){ let n = e - 13000; let r = t.testPctOverride;
    #                      if (valid r) return Math.min(Math.floor(e*(r/100)), n);
    #                      return n }
    #
    # NOT KRt(), which is `min(usable - round(usable*0.2), xWe(...))` -- that is the
    # separate precompute-ARMING threshold (C3n/fPt, behind its own gate). Including
    # its 0.2 buffer term understated the trigger: identical at pct=80, but 98,000 low
    # at pct=90 and 183,000 low at pct=100 or unset, so a fatal model went unnamed.
    trigger = usable - 13_000
    if pct is not None:
        # Divide BEFORE multiplying, exactly as the engine does: Math.floor(e*(r/100)).
        # Floating-point multiplication is not associative, so `usable * pct / 100`
        # lands one token ABOVE the engine on real registry windows -- a 700,000 route at
        # pct=70 gives 476,000 where the engine triggers at 475,999. One token high means
        # a model walled exactly AT that trigger reads as fatal when it is in fact safe:
        # named for no reason by default, and with CCMORPH_WITHHOLD_FATAL=1 removed from
        # the picker -- losing the user a model they can actually use.
        trigger = min(int(usable * (pct / 100)), trigger)
    if trigger <= 0:
        return []

    offered = doc.get("availableModels") if isinstance(doc.get("availableModels"), list) else []
    if not offered:
        return []

    walls = parse_context(conf)

    # A tier alias is not a model and carries no wall of its own; the launch path
    # resolves it through the profile default, so it is never a hazard by name.
    ALIASES = {"opus", "sonnet", "haiku", "fable"}
    seen = set()
    # Names already reported: the comparison keys on (id, ceiling) so both variants of a
    # bare/[1m] pair are checked, but a model is named at most once.
    named = set()
    rows = []
    for m in offered:
        if not isinstance(m, str):
            continue
        bare = m.split("[", 1)[0]
        # A tier alias is not a model and carries no wall of its own.
        if bare in ALIASES:
            continue
        wall = walls.get(f"{profile}:{bare}") or walls.get(bare)
        if not wall:
            continue
        # Each sibling gets the ceiling the ENGINE would resolve for ITS id, because zE()
        # recomputes it per model on every call: the [1m] regex wins first, a bare claude-
        # prefix takes the hardcoded 200,000 and IGNORES the env ceiling, and everything
        # else takes the route ceiling. Applying the launch trigger to every sibling and
        # skipping claude- ids outright assumed min() had already narrowed them -- true for
        # a real Anthropic model, false for a gateway model wearing a claude- name, whose
        # probed wall can sit UNDER the claude-family trigger. That is a fatal switch
        # that went unnamed, which is the unrecoverable wedge this whole feature prevents.
        # xe() returns false outright when CLAUDE_CODE_DISABLE_1M_CONTEXT is set, so the
        # tag stops meaning 1,000,000 and the id falls through to its family default.
        # Unanchored, as xe() is: the tag counts anywhere in the id.
        if not no_1m and re.search(r"\[1m\]", m, re.IGNORECASE):
            ceil_m = 1_000_000
        elif bare.startswith("claude-"):
            ceil_m = 200_000
        else:
            ceil_m = window
        # CL() is Math.min(ceiling, autoCompactWindow) for BOTH the env and settings sources,
        # so the session budget caps every model on every call -- not only the launched one.
        # Deriving a sibling trigger from the family ceiling alone overstated it by up to
        # ~560,000 tokens and produced a self-contradictory advisory: "compacts at 224k" in
        # the same breath as "claude-foo dies at 500k", a wall the session cannot reach.
        if budget:
            ceil_m = min(ceil_m, budget)
        # Per-model reserve: three legacy ids declare a max-output below the 20,000 cap, so
        # the engine reserves less for them and triggers higher.
        reserve_m = engine_reserve(m, raw_out and int(raw_out))
        # Dedup on (id, ceiling), not the id alone. `claude-foo` and `claude-foo[1m]` are
        # the SAME bare id and two different engine ceilings -- 200,000 and 1,000,000 --
        # so their triggers differ by up to 823,000 tokens. Keying on the bare id was
        # harmless while every sibling shared one trigger, but once each derives its own
        # (above), it silently dropped whichever variant was listed second: bare first,
        # and the [1m] fatality is never named. That pair is reachable through the
        # product, since the allowlist keeps every claude- entry a user has had, so a
        # downward re-probe leaves a stale [1m] beside its bare form.
        if (bare, ceil_m) in seen:
            continue
        seen.add((bare, ceil_m))
        usable_m = ceil_m - min(ceil_m, reserve_m)
        trigger_m = engine_trigger(usable_m, pct)
        if wall < trigger_m and bare not in named:
            # The ROW is keyed on the name, because the name is what gets printed. The
            # comparison above needs both variants of a bare/[1m] pair (their ceilings
            # differ), but once either is found fatal the model is named -- listing it
            # twice says nothing new and burns two of the three advisory slots.
            named.add(bare)
            rows.append((wall, bare))

    if not rows:
        return []
    return [str(trigger)] + [f"{wall}\t{name}" for wall, name in sorted(rows, reverse=True)]
PYHAZ
}

# The advisory text for those rows, in ONE place. The launch renders it from the
# same interpreter that computed the set, and `_ccm_switch_hazard_warn` renders
# it from `_ccm_hazard_set`'s output, so the wording, the three-name cap and the
# withhold branch cannot drift between the two paths.
_ccm_hazard_msg_py() {
  command cat <<'PYHMSG'
def hazard_message(rows, withheld):
    # `rows` is hazard_rows()' own output: the trigger, then `<wall>\t<name>`
    # lines, largest wall first. Fewer than two elements is nothing to say.
    if len(rows) < 2:
        return ""

    def k(n):
        return f"{round(n / 1000)}k"

    trigger = int(rows[0])
    named = []
    for line in rows[1:]:
        wall, _, name = line.partition("\t")
        named.append((int(wall), name))

    # Largest wall first (the set arrives sorted): the useful warning is the 700k
    # model that stops compacting at 700k, not the 258k one nobody takes into a long
    # session. Three names plus a count -- a route spanning 8k to 2M would otherwise
    # print a wall nobody reads.
    shown = ", ".join(f"{name} limit {k(wall)}" for wall, name in named[:3])
    more = f" +{len(named) - 3} smaller" if len(named) > 3 else ""
    if withheld:
        return (f"compacts at {k(trigger)}; hidden from /model: {shown}{more}. "
                "Start a new session to use them, or set CCMORPH_WITHHOLD_FATAL=0 to show them.")
    return (f"compacts at {k(trigger)}; lower model limits: {shown}{more}. "
            "Start a new session on a lower-limit model to size compaction for it. "
            "Set CCMORPH_WITHHOLD_FATAL=1 to hide these choices.")
PYHMSG
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
  # _ccm_trigger_env's own outputs have: a literal 0 would reach the python below as a
  # truthy STRING and silence the fallback it is there to allow. The python reads the
  # layers when this is empty, because that is then the engine's own value.
  local pct_in_env=""
  (( ${penv[(I)CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=*]} )) && pct_in_env=1

  _CCM_LIB="$_CCMORPH_LIBEXEC_DIR" _CCM_SOURCES="${(pj:\n:)sources}" _CCM_PROFILE_CONF="$context_conf" _CCM_CONTEXT_RESOLVER="$_CCMORPH_LIBEXEC_DIR/model-window" _CCM_BUDGET="$budget" \
  _CCM_CEILING="$ceiling" _CCM_PCT="$pct" _CCM_PROFILE="$profile" \
  _CCM_MAXOUT="$_maxout" _CCM_NO1M="$_no1m" _CCM_PCT_IN_ENV="$pct_in_env" \
    command python3 -c '
import json, os, re
'"$(_ccm_settings_compose_py)"'
'"$(_ccm_pct_parser)"'
'"$(_ccm_context_parser)"'

'"$(_ccm_hazard_py)"'
rows = hazard_rows(_sc, os.environ.get("_CCM_PROFILE", ""), os.environ.get("_CCM_BUDGET", ""), os.environ.get("_CCM_CEILING", ""), [p for p in os.environ.get("_CCM_SOURCES", "").split("\n") if p.strip()], os.environ.get("_CCM_PCT", ""), os.environ.get("_CCM_MAXOUT", "").strip(), bool(os.environ.get("_CCM_NO1M", "")), os.environ["_CCM_PROFILE_CONF"], bool(os.environ.get("_CCM_PCT_IN_ENV", "")))
if rows: print("\n".join(rows))
'
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
        command python3 -c '
import os
'"$(_ccm_hazard_msg_py)"'
rows = [line for line in os.environ.get("_CCM_ROWS", "").split("\n") if line.strip()]
msg = hazard_message(rows, os.environ.get("_CCM_WITHHELD") == "1")
if msg:
    print(msg)
' 2>/dev/null) || return 0

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
    command python3 -c '
import json, os, tempfile

path = os.environ["_CCM_SETTINGS"]
try:
    with open(path, encoding="utf-8") as fh:
        doc = json.load(fh)
except Exception:
    raise SystemExit(0)
if not isinstance(doc, dict):
    raise SystemExit(0)
offered = doc.get("availableModels")
if not isinstance(offered, list) or not offered:
    raise SystemExit(0)

fatal = set()
for line in os.environ.get("_CCM_ROWS", "").split("\n"):
    if not line.strip():
        continue
    _, _, name = line.partition("\t")
    fatal.add(name)
if not fatal:
    raise SystemExit(0)

launched = os.environ.get("_CCM_LAUNCHED", "").split("[", 1)[0]
kept = [m for m in offered
        if not isinstance(m, str)
        or m.split("[", 1)[0] == launched          # never hide what is running
        or m.split("[", 1)[0] not in fatal]
if len(kept) == len(offered):
    raise SystemExit(0)
doc["availableModels"] = kept

d = os.path.dirname(path) or "."
fd, tmp = tempfile.mkstemp(dir=d, prefix=".ovl.")
try:
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2)
        fh.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
except BaseException:
    os.unlink(tmp)
    raise
' 2>/dev/null || return 0
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
  command python3 -c '
import json, sys
'"$(_ccm_policy_pct_py)"'
value = policy_pct(sys.argv[1])
if value is None:
    raise SystemExit(1)
print(value)
' "$_CCMORPH_CONTEXT_POLICY" 2>/dev/null || {
    _ccm_err "invalid fleet context policy: $_CCMORPH_CONTEXT_POLICY"
    return 1
  }
}

_ccm_effective_compaction_percent() {
  local profile="$1" conf_value="${2:-}" saved=""
  saved=$(_CCM_LIB="$_CCMORPH_LIBEXEC_DIR" _CCM_CONF_PCT="$conf_value" command python3 -c '
import json, os, re, sys
'"$(_ccm_settings_compose_py)"'
'"$(_ccm_pct_parser)"'
conf = os.environ.get("_CCM_CONF_PCT", "")
if conf:
    value = engine_pct(conf)
    if value is not None:
        print(f"{value:g}")
        raise SystemExit(0)

key = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
env = _sc.merged_view(sys.argv[1:]).get("env")
raw = env.get(key) if isinstance(env, dict) else None  # last writer wins, even when invalid
value = engine_pct(raw) if isinstance(raw, str) else None
if value is None:
    raise SystemExit(1)
print(f"{value:g}")
' "${(f)$(_ccm_settings_sources "$profile")}" 2>/dev/null) || saved=""
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
  "$_CCMORPH_LIBEXEC_DIR/settings-compose" "${_ccm_sc_argv[@]}"
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
# one profile, so a launch pays for one Python start instead of two: the store reads
# the conf and then runs settings-compose inside its own interpreter.
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
      saved=$(_CCM_MATERIALIZE= _CCM_SC_PY= \
              _ccm_config_store prepare-launch "$profile") || return 10
      _ccm_project_settings "$profile" "$saved" "$@"
      return $?
    fi
  done
  local rc
  _CCM_MATERIALIZE="${(pj:\n:)sc_argv}" _CCM_LIB="$_CCMORPH_LIBEXEC_DIR" \
    _CCM_SC_PY="$(_ccm_settings_compose_py)" \
    _ccm_config_store prepare-launch "$profile" >/dev/null
  rc=$?
  # settings-compose's own statuses, shifted past the store's: the launch handles
  # each of them as it does when they come from its own process.
  case $rc in
    0) return 0 ;;
    11 | 12 | 13) _ccm_err "could not generate settings for '$profile'"; return $(( rc - 10 )) ;;
    *) return 10 ;;
  esac
}

