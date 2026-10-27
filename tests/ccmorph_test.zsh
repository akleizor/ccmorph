#!/bin/zsh

setopt nounset pipefail
zmodload zsh/datetime

# The suite often runs from inside a ccmorph-launched Claude session, whose
# exports (ANTHROPIC_*, CLAUDE*, CCMORPH_*) reach every fixture through the
# launcher's pre-launch `claude --help` probe and env: they flip assertions
# and dump live credentials into failure output (#200). Scrub them once, here,
# keeping only this suite's own knobs.
ccmorph_test_scrub_runner_env() {
  local v
  for v in ${(k)parameters}; do
    case $v in
      CCMORPH_SOURCE|CCMORPH_TEST*) continue ;;
      ANTHROPIC_*|CLAUDE*|CCMORPH_*) ;;
      *) continue ;;
    esac
    [[ "${parameters[$v]}" == *export* ]] || continue
    unset "$v"
  done
}
ccmorph_test_scrub_runner_env

readonly SCRIPT_DIR="${0:A:h}"
readonly REPO_ROOT="${SCRIPT_DIR:h}"
readonly CCMORPH_SOURCE="$REPO_ROOT/lib/ccmorph.sh"
# The library text: the loader plus every module split out of it. Not the UI
# module, which was never part of it. Text checks read all of it.
typeset -a _ccmorph_library=("$CCMORPH_SOURCE" "$REPO_ROOT"/lib/ccmorph-*.zsh(N))
readonly -a CCMORPH_LIBRARY=(${_ccmorph_library:#*/lib/ccmorph-ui.zsh})
readonly ORIGINAL_PATH="$PATH"
# Captured before any test redirects $HOME to a scratch dir. Only for locating
# things that genuinely live on this machine (see the differential parser test).
readonly REAL_HOME="$HOME"

typeset -gi tests_run=0
typeset -gi tests_failed=0
typeset -gi tests_seen=0

# A wait ends on an event, never on a deadline: a bound that competes with the
# other lanes' CPU fails a healthy run whatever number it picks (#398 saw 8.9 s
# of a 10 s budget, then 27-103 s launches). One wait cannot meet an event at all
# - the one a broken signal forwarding leaves hanging - so this is its backstop,
# a hang detector sized far above any healthy duration, never a deadline. Seconds;
# zselect counts hundredths.
readonly CCMORPH_TEST_HANG_TIMEOUT="${CCMORPH_TEST_HANG_TIMEOUT:-120}"

# Every test builds its own scratch home, so the suite runs as N shards at once;
# CCMORPH_TEST_JOBS=1 runs it serially. Default on Apple silicon: performance
# cores plus a third of the efficiency cores (10 on the mini beat 18: 37 s vs 45 s).
_ccm_test_jobs() {
  local fast slow
  fast=$(sysctl -n hw.perflevel0.physicalcpu 2>/dev/null) && slow=$(sysctl -n hw.perflevel1.physicalcpu 2>/dev/null) &&
    { print -- $(( fast + slow / 3 )); return; }
  sysctl -n hw.ncpu 2>/dev/null || nproc
}
if [[ -z "${CCMORPH_TEST_SHARD:-}" ]] && (( ${CCMORPH_TEST_JOBS:-$(_ccm_test_jobs)} > 1 )); then
  typeset -i jobs=${CCMORPH_TEST_JOBS:-$(_ccm_test_jobs)} shard rc=0
  typeset -a pids
  for (( shard = 0; shard < jobs; shard++ )); do
    CCMORPH_TEST_SHARD="$shard/$jobs" zsh "$0" "$@" & pids+=($!)
  done
  for pid in $pids; do wait $pid || rc=1; done
  exit $rc
fi

# GH-434: a test's children must not outlive it. The landing batch found copies
# of this suite stranded at zero CPU long after the batch that started them was
# killed: a test had left a child behind (a fake adapter still holding its
# home's fifo open), and nothing failed the test for it. Every home setup_home
# builds is remembered here, and after each test the homes it built are swept:
# a process still carrying one in its argv after the test function returned has
# outlived the test, so the test fails and the child is reaped -- TERM first,
# the fakes exit on it, KILL for whatever ignores it. The shards are separate
# processes, so each keeps its own registry. The launcher in the launch tests
# carries no home in its argv, so the sweep also takes this shell's direct
# children, and when it reaps one it takes that child's whole pid tree: the
# launcher's own children can hide their argv from pgrep entirely.
typeset -ga _ccm_test_homes=()
typeset -gi _ccm_test_homes_mark=0
typeset -gi _ccm_test_homes_exiting=0
typeset -ga _ccm_test_leaked=()
typeset -ga _ccm_test_kids=()

_ccm_test_descendants_of() {
  local p c depth
  local -a frontier=("$@") found next
  for (( depth = 0; depth < 8 && $#frontier; depth++ )); do
    next=()
    for p in $frontier; do
      for c in ${(f)"$(pgrep -P $p 2>/dev/null)"}; do
        [[ -n $c ]] || continue
        found+=($c)
        next+=($c)
      done
    done
    frontier=($next)
  done
  (( $#found )) && print -r -- $found
}

# Set _ccm_test_leaked (processes carrying one of the homes from $1 on in
# their argv) and _ccm_test_kids (this shell's live direct children).
_ccm_test_collect_leaks() {
  local from=$1 i home pid
  local -a raw
  _ccm_test_leaked=()
  _ccm_test_kids=()
  for (( i = from; i <= $#_ccm_test_homes; i++ )); do
    home=${_ccm_test_homes[i]}
    _ccm_test_leaked+=(${(f)"$(pgrep -f -- "${home//./\\.}" 2>/dev/null)"})
  done
  raw=(${(f)"$(pgrep -P $$ 2>/dev/null)"})
  for pid in $raw; do
    # pgrep -P $$ lists the $(...) subshell it ran in too, but that one is
    # dead and reaped by the time this loop sees it; a zombie a test forgot
    # to wait for holds nothing open either. Neither is a leak.
    kill -0 $pid 2>/dev/null || continue
    [[ $(ps -o stat= -p $pid 2>/dev/null) != Z* ]] || continue
    _ccm_test_kids+=($pid)
  done
}

_ccm_test_sweep_homes() {
  local from=$1 pid pass rc=0
  local -a tree volley_tree
  _ccm_test_collect_leaks $from
  # A child can be a few tenths from its own exit when the test returns --
  # the fake adapter polls its parent every 0.2 s -- so give the set that
  # long to leave on its own before calling anything a leak. command sleep:
  # tests shadow sleep with a clock-counting function, and a failed one
  # never unfunctions it. Spelled while: the retry cap sits in the condition
  # next to the event it caps, the shape the hang-bound guard reads as a
  # wait (#457).
  pass=0
  while (( ++pass <= 3 && ( $#_ccm_test_leaked || $#_ccm_test_kids ) )); do
    command sleep 0.2
    _ccm_test_collect_leaks $from
  done
  (( $#_ccm_test_leaked || $#_ccm_test_kids )) || return 0
  rc=1
  for pid in $_ccm_test_leaked $_ccm_test_kids; do
    print -u2 -- "pid $pid outlived its test: $(ps -o command= -p $pid 2>/dev/null)"
    kill -TERM $pid 2>/dev/null
  done
  # A reaped child's own children can outlive it -- the launcher's are the
  # point -- and their argv can be unreadable, so walk the pid trees.
  volley_tree=(${(f)"$(_ccm_test_descendants_of $_ccm_test_leaked $_ccm_test_kids)"})
  for pid in $volley_tree; do
    kill -TERM $pid 2>/dev/null
  done
  pass=0
  while (( ++pass <= 20 && ( $#_ccm_test_leaked || $#_ccm_test_kids ) )); do
    command sleep 0.2
    _ccm_test_collect_leaks $from
  done
  for pid in $_ccm_test_leaked $_ccm_test_kids; do
    print -u2 -- "pid $pid ignored TERM; killing it"
    kill -KILL $pid 2>/dev/null
  done
  tree=(${(f)"$(_ccm_test_descendants_of $_ccm_test_leaked $_ccm_test_kids)"})
  for pid in $volley_tree $tree; do
    kill -KILL $pid 2>/dev/null
  done
  return $rc
}

# GH-434: a trapped signal runs its handler the moment it lands, even
# mid-builtin, but the exit the handler asks for is held until the interrupted
# builtin returns -- and inside a function, where every test's wait runs, one
# zselect holds it for its whole budget. The kill the landing batch sent could
# not wake these reads either (the suite's own RDWR handle is a fifo writer, so
# the launcher's death brings no EOF), so each backstop waits in one-second
# slices: a killed suite leaves its zselect within a second of the signal
# instead of after the whole backstop.
_ccm_test_wait_readable() {
  local fd=$1
  local -i left=$2
  while (( left > 0 )); do
    zselect -r $fd -t $(( left < 100 ? left : 100 )) && return 0
    (( left -= 100 ))
  done
  return 1
}

# The exit sweep covers only what the per-test sweep never reached: a suite
# killed mid-test leaves that test's children behind, exactly how the stranded
# copies were left. It never changes the exit status, and it never runs twice:
# the signal path may have swept already.
_ccm_test_sweep_on_exit() {
  local rc=$?
  (( _ccm_test_homes_exiting )) && return $rc
  _ccm_test_sweep_homes $(( _ccm_test_homes_mark + 1 ))
  return $rc
}

# A signal ends the suite here and now, but not before the sweep: the landing
# batch's kill left the stranded copies behind, and this sweep is what keeps a
# killed run from leaking the same way. A second signal during the sweep must
# not cut it short, so only the first one owns the exit.
_ccm_test_sweep_on_signal() {
  (( _ccm_test_homes_exiting )) && return 0
  typeset -gi _ccm_test_homes_exiting=1
  _ccm_test_sweep_homes $(( _ccm_test_homes_mark + 1 ))
  exit $(( 128 + $1 ))
}
trap _ccm_test_sweep_on_exit EXIT
trap '_ccm_test_sweep_on_signal 1' HUP
trap '_ccm_test_sweep_on_signal 2' INT
trap '_ccm_test_sweep_on_signal 15' TERM

assert_contains() {
  local actual="$1"
  local expected="$2"
  local message="$3"

  if [[ "$actual" != *"$expected"* ]]; then
    print -u2 -- \
      "$message\n  expected to contain: ${(qqq)expected}\n  actual: ${(qqq)actual}"
    return 1
  fi
}

assert_not_contains() {
  local actual="$1"
  local unexpected="$2"
  local message="$3"

  if [[ "$actual" == *"$unexpected"* ]]; then
    print -u2 -- \
      "$message\n  expected not to contain: ${(qqq)unexpected}\n  actual: ${(qqq)actual}"
    return 1
  fi
}

# Installs a fake ccmorph-adapter: the given lines, then the shared serve loop,
# which exits once the launch that passed --parent-pid is gone (#373).
write_fake_adapter() {
  local adapter="$HOME/.config/ccmorph/libexec/ccmorph-adapter"
  printf '%s\n' '#!/bin/sh' "$@" > "$adapter"
  print -r -- "$(<"$SCRIPT_DIR/fake_adapter_serve.sh")" >> "$adapter"
  chmod +x "$adapter"
}

setup_home() {
  # GH-434: remembered so the sweep after each test can fail a test whose
  # child outlives it.
  _ccm_test_homes+=("$1")
  export HOME="$1"
  # The runner may export tuning keys (or be a Claude Code session itself);
  # the launcher passes them through, so scrub them for a hermetic fixture.
  unset CLAUDE_CODE_SIMPLE MAX_THINKING_TOKENS CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC CCMORPH_SET_ENV CLAUDECODE CLAUDE_CODE_CHILD_SESSION
  export CCMORPH_TEST_LOG="$HOME/claude.log"
  export PATH="$HOME/bin:$ORIGINAL_PATH"

  # Tests source ccmorph.sh from the REPO, so its derived library path is the repo
  # — but they stub helpers (ccmorph-adapter, model-window) inside the scratch
  # $HOME. Point the library seam at the scratch dir so a stub is found where the
  # tests put it. This is the seam a package manager uses for the same reason: the
  # library does not have to live beside the state.
  export CCMORPH_LIB_DIR="$HOME/.config/ccmorph/lib"

  mkdir -p \
    "$HOME/bin" \
    "$HOME/.config/ccmorph" \
    "$HOME/.config/ccmorph/profiles" \
    "$HOME/.config/ccmorph/lib" "$HOME/.config/ccmorph/libexec" "$HOME/.config/ccmorph/share" \
    "$HOME/.claude/projects" \
    "$HOME/.agents/runtime/policies"
  print -r -- '{"schemaVersion":1,"autoCompactAtPercent":80}' \
    > "$HOME/.agents/runtime/policies/context.json"
  # Context lookup now uses the shipped canonical resolver, never a shell copy
  # of its registry parser. The fixture must install that runtime dependency.
  install_model_window

  printf '%s\n' \
    '#!/bin/sh' \
    '{' \
    '  printf "ARG"' \
    '  for arg do printf "\\t%s" "$arg"; done' \
    '  printf "\\n"' \
    '  printf "SUBAGENT=%s\\n" "${CLAUDE_CODE_SUBAGENT_MODEL-<unset>}"' \
    '  printf "ALWAYS_EFFORT=%s\\n" "${CLAUDE_CODE_ALWAYS_ENABLE_EFFORT-<unset>}"' \
    '  printf "DISABLE_ADVISOR=%s\\n" "${CLAUDE_CODE_DISABLE_ADVISOR_TOOL-<unset>}"' \
    '  printf "SIMPLE=%s\\n" "${CLAUDE_CODE_SIMPLE-<unset>}"' \
    '  printf "MAX_CONTEXT=%s\\n" "${CLAUDE_CODE_MAX_CONTEXT_TOKENS-<unset>}"' \
    '  printf "AUTO_COMPACT=%s\\n" "${CLAUDE_CODE_AUTO_COMPACT_WINDOW-<unset>}"' \
    '  printf "MAX_OUTPUT=%s\\n" "${CLAUDE_CODE_MAX_OUTPUT_TOKENS-<unset>}"' \
    '  printf "AUTOCOMPACT_PCT=%s\\n" "${CLAUDE_AUTOCOMPACT_PCT_OVERRIDE-<unset>}"' \
    '  printf "MODEL_CAPS=%s\\n" "${CLAUDE_CODE_MODEL_CAPABILITIES-<unset>}"' \
    '  printf "PICKER_ROW=%s\\n" "${ANTHROPIC_CUSTOM_MODEL_OPTION-<unset>}"' \
    '  printf "BASE_URL=%s\\n" "${ANTHROPIC_BASE_URL-<unset>}"' \
    '  printf "API_KEY=%s\\n" "${ANTHROPIC_API_KEY-<unset>}"' \
    '  printf "LAUNCH_MODEL=%s\\n" "${CCMORPH_LAUNCH_MODEL-<unset>}"' \
    '  printf "LAUNCH_EFFORT=%s\\n" "${CCMORPH_LAUNCH_EFFORT-<unset>}"' \
    '  printf "MODEL_ENV=%s\\n" "${ANTHROPIC_MODEL-<unset>}"' \
    '  printf "ADVISE=%s\\n" "${CCMORPH_ADVISE-<unset>}"' \
    '  if [ -n "$CLAUDE_CONFIG_DIR" ] && [ -f "$CLAUDE_CONFIG_DIR/settings.json" ]; then' \
    '    printf "SETTINGS=%s\\n" "$(python3 -c "import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(chr(44), chr(58))))" "$CLAUDE_CONFIG_DIR/settings.json")"' \
    '  fi' \
    '} >> "$CCMORPH_TEST_LOG"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"

  # Default launcher probes are hermetically reachable. Tests for discovery,
  # published metadata, and transport failure replace this stub explicitly.
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do [ "$arg" = "-w" ] && { printf "200"; exit 0; }; done' \
    'for arg do case "$arg" in *messages*) printf "200"; exit 0 ;; esac; done' \
    'exit 1' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"

  # Automatic adapters are part of the launch path for custom endpoints. Keep the
  # hermetic fixture native-compatible; tests that exercise adapter behavior replace it.
  write_fake_adapter \
    'echo "NATIVE=1"' \
    'trap "exit 0" TERM INT'

  printf '%s\n' \
    'type=proxy' \
    'base_url=http://proxy.invalid' \
    'api_key=test-key' \
    'model=bootstrap-model' \
    'env.CLAUDE_CODE_ALWAYS_ENABLE_EFFORT=1' \
    > "$HOME/.config/ccmorph/proxy.conf"
}

# Fake the gateway. ccmorph must ask the endpoint what it serves rather than
# carry any per-model knowledge, so the fixture is the only source of truth
# about which models exist.
# The route probe asks curl for -w %{http_code}; any fake that only cats a body
# must answer 200 there or the launch routes via a proxy that does not exist.
make_fake_curl_probe_aware() {
  local f="$HOME/bin/curl"
  { print -r -- '#!/bin/sh'
    print -r -- 'for a do case "$a" in *http_code*) echo 200; exit 0 ;; esac; done'
    tail -n +2 "$f"; } > "$f.new" && mv "$f.new" "$f"
  chmod +x "$f"
}

write_curl_mock() {
  printf '%s\n' '#!/bin/zsh' "$1" > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
}

fake_gateway() {
  printf '%s\n' \
    '#!/bin/sh' \
    '# record the invocation so tests can assert on url and auth' \
    'for a do printf "%s\n" "$a" >> "$HOME/curl.args"; done' \
    '[ "${1-}" = -K ] && cat >> "$HOME/curl.args"' \
    'cat <<JSON' \
    '{"object":"list","data":[' \
    '{"id":"zeta-chat","owned_by":"vendor-b","capabilities":{"chat_completion":true}},' \
    '{"id":"alpha-chat","owned_by":"vendor-a","capabilities":{"chat_completion":true}},' \
    '{"id":"embed-only","owned_by":"vendor-c","capabilities":{"chat_completion":false,"embeddings":true}}' \
    ']}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
}

# Fake the picker: take the first offered line, like a user hitting enter.
fake_fzf() {
  printf '%s\n' \
    '#!/bin/sh' \
    'input=$(cat)' \
    'printf "%s\n" "$input" > "$HOME/fzf.input"' \
    'printf "%s\n" "$input" | head -n 1' \
    > "$HOME/bin/fzf"
  chmod +x "$HOME/bin/fzf"
}

# Some Anthropic-compatible proxies rewrite non-claude model ids into
# "claude-<junk>-<reversed-id>" when a request carries anthropic-version, so the
# listing becomes unreadable and non-claude models look absent. The same endpoint
# returns clean ids without that header, and dropping it is safe for the native
# Anthropic endpoint too (it authenticates before validating the version).
test_models_does_not_send_anthropic_version() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  ccmorph _models gw >/dev/null || {
    rm -rf "$test_home"
    return 1
  }

  local rc=0
  assert_not_contains \
    "$(<"$HOME/curl.args")" \
    'anthropic-version' \
    'model discovery must not send anthropic-version: it makes proxies mangle ids' || rc=1
  assert_contains \
    "$(<"$HOME/curl.args")" \
    'x-api-key: sk_prod_test' \
    'the key must still be sent' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_versioned_base_url_is_not_given_a_second_v1() {
  source "$CCMORPH_SOURCE"
  local rc=0
  _ccm_api_url 'https://provider.invalid/api/v4' '/v1/models'
  [[ "$REPLY" == 'https://provider.invalid/api/v4/models' ]] || {
    print -u2 -- 'a versioned provider base URL must not receive a second API version'
    rc=1
  }
  _ccm_api_url 'https://provider.invalid/api/V4' '/v1/models'
  [[ "$REPLY" == 'https://provider.invalid/api/V4/models' ]] || {
    print -u2 -- 'API version matching must be case-insensitive'
    rc=1
  }
  _ccm_api_url 'https://provider.invalid' '/v1/models'
  [[ "$REPLY" == 'https://provider.invalid/v1/models' ]] || {
    print -u2 -- 'an unversioned provider base URL must receive the standard API version'
    rc=1
  }
  return $rc
}

test_models_lists_only_chat_capable_from_the_gateway() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _models gw) || {
    print -u2 -- 'ccmorph models must succeed against a reachable gateway'
    rm -rf "$test_home"
    return 1
  }

  assert_contains "$actual" 'alpha-chat' 'chat-capable models must be listed' || rc=1
  assert_contains "$actual" 'zeta-chat' 'chat-capable models must be listed' || rc=1
  assert_not_contains \
    "$actual" \
    'embed-only' \
    'a model the gateway does not mark chat_completion must be omitted' || rc=1
  assert_contains "$actual" 'vendor-a' 'the owning provider must be shown' || rc=1
  # Sorted, so selection order is stable rather than gateway-order dependent.
  [[ "${actual%%$'\n'*}" == alpha-chat* ]] || {
    print -u2 -- "models output must be sorted; got: ${(qqq)actual}"
    rc=1
  }

  local args="$(<"$HOME/curl.args")"
  assert_contains \
    "$args" \
    'https://gateway.invalid/v1/models' \
    "models must query the profile's own base_url" || rc=1
  assert_contains \
    "$args" \
    'x-api-key: sk_prod_test' \
    "models must authenticate with the profile's key" || rc=1
  rm -rf "$test_home"
  return $rc
}

# A gateway records the HTTPS_PROXY it was invoked with, so a test can prove
# discovery went through the connect proxy.
fake_gateway_records_proxy() {
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do' \
    '  if [ "$arg" = "-w" ]; then printf "000"; exit 28; fi' \
    'done' \
    'printf "HTTPS_PROXY=%s\n" "${HTTPS_PROXY-<unset>}" >> "$HOME/curl.env"' \
    'cat <<JSON' \
    '{"object":"list","data":[{"id":"a-chat","owned_by":"v","capabilities":{"chat_completion":true}}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
}

# Regression: every launch path used to hand its network consumers
# `HTTPS_PROXY="${_ccm_route_https_proxy:-}"`, which BLANKED an ambient proxy
# whenever no transport ran — the operator's own bridge was deleted by the tool
# that should have used it. Discovery must see the caller's value unchanged.
test_models_discovery_uses_the_ambient_proxy() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_records_proxy
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://alternate.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0
  HTTPS_PROXY='http://127.0.0.1:47999' https_proxy='http://127.0.0.1:47999' \
    ccmorph _models gw >/dev/null 2>&1 || {
    print -u2 -- 'discovery must succeed through the ambient proxy'
    rc=1
  }
  assert_contains "$(<"$HOME/curl.env")" 'HTTPS_PROXY=http://127.0.0.1:47999' \
    'discovery curl must see the ambient proxy, never a blanked one' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Endpoints that predate or omit a capabilities field (the native Anthropic
# model list, and Anthropic-shaped local translators) must not be filtered to
# nothing. Absence of a declaration is not a denial.
test_models_keeps_entries_that_declare_no_capabilities() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"data":[' \
    '{"id":"plain-a","object":"model","owned_by":"vendor-a"},' \
    '{"id":"plain-b","object":"model","owned_by":"vendor-b"}' \
    ']}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  write_apikey_conf native 'api_key=sk-test-native'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _models native) || {
    print -u2 -- 'an endpoint without a capabilities field must still list models'
    rm -rf "$test_home"
    return 1
  }

  assert_contains "$actual" 'plain-a' 'undeclared capability must not exclude a model' || rc=1
  assert_contains "$actual" 'plain-b' 'undeclared capability must not exclude a model' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_models_defaults_to_anthropic_when_profile_has_no_base_url() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf native 'api_key=sk-test-native'
  source "$CCMORPH_SOURCE"

  ccmorph _models native >/dev/null || {
    rm -rf "$test_home"
    return 1
  }

  local rc=0
  assert_contains \
    "$(<"$HOME/curl.args")" \
    'https://api.anthropic.com/v1/models' \
    'a profile with no base_url must query the native endpoint' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_models_rejects_a_profile_with_no_key() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  printf '%s\n' 'type=sub' 'email=a@b.c' > "$HOME/.config/ccmorph/subacct.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  if ccmorph _models subacct >/dev/null 2>&1; then
    print -u2 -- 'a subscription profile has no key to query a gateway with'
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

# Defaults use Claude Code's native model and effortLevel settings. /model may
# save a new default without competing with an environment pin. Readers still
# honor explicit legacy environment overrides until the user sets a model again.
test_the_listing_shows_the_model_that_will_actually_launch() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  # The conf carries a bootstrap value; the profile's real default is elsewhere.
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid' \
    'model=conf-bootstrap'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"model":"chosen-model"}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 shown
  shown=$(ccmorph list) || {
    print -u2 -- 'the listing must succeed'
    rm -rf "$test_home"; return 1
  }
  assert_contains "$shown" 'conf-bootstrap' \
    'the listing must show the configured conf default' || rc=1
  assert_not_contains "$shown" 'chosen-model' \
    'native inline state must not override a configured default' || rc=1

  rm -rf "$test_home"
  return $rc
}

# An profile that sets no default is a real state, and the listing must say so
# rather than printing a stale conf value or a bare `?`.
test_the_listing_names_an_account_with_no_default_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 shown
  shown=$(ccmorph list) || { rm -rf "$test_home"; return 1; }
  assert_not_contains "$shown" '→ ?' \
    'an unset default must not be shown as an unexplained question mark' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_model_command_sets_the_profiles_default_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  fake_gateway
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"effortLevel":"high"}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  ccmorph set gw model alpha-chat >/dev/null || {
    print -u2 -- 'ccmorph model <name> <id> must succeed'
    rm -rf "$test_home"; return 1
  }
  local got
  got=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
env = d.get("env") or {}
print(d.get("model"), d.get("effortLevel"), "ANTHROPIC_MODEL" in env)
' "$HOME/.config/ccmorph/profiles/gw/settings.json")
  [[ "$got" == "None high False" && "$(ccmorph get gw model)" == alpha-chat ]] || {
    print -u2 -- "the default model must save in conf without changing native settings (got: $got)"
    rc=1
  }

  # No argument reports, never guesses.
  local shown
  shown=$(ccmorph get gw model) || rc=1
  assert_contains "$shown" 'alpha-chat' \
    'ccmorph get <profile> model must report the current default' || rc=1
  rm -rf "$test_home"
  return $rc
}

# `-i` reuses the picker already here rather than growing a second one: it lists what
# the endpoint serves and emits a bare id. It is a FLAG, not a value, which is what
# both earlier spellings got wrong -- `@menu` had to be repeated per slot, and `?` was
# eaten by the caller's shell before ccmorph ever started.
#
# It now SAVES rather than erroring. `-i` meaning "ask" at launch and "invalid
# argument" at `set` made one flag behave two ways depending on the verb, and a
# gateway's model list cannot be enumerated in advance -- so refusing to ask was
# refusing the only reliable way to spell a model id. A missing value means the same
# request as an explicit `-i`.
test_set_model_interactively_saves_the_chosen_id() {
  local test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  fake_gateway
  fake_fzf
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"
  local rc=0 out saved

  # fake_fzf takes the first offered row. The listing is sorted, so that is
  # alpha-chat -- an id this test never types, which is the point.
  out=$(ccmorph set gw model -i 2>/dev/null) || {
    print -u2 -- '-i must save a chosen model, not fail'
    rm -rf "$test_home"; return 1
  }
  [[ "$out" == alpha-chat ]] || {
    print -u2 -- "-i must print the bare chosen id (got: $out)"; rc=1
  }
  saved=$(ccmorph get gw model 2>/dev/null)
  [[ "$saved" == alpha-chat ]] || {
    print -u2 -- "the chosen model must be saved (got: $saved)"; rc=1
  }
  # The non-chat model must never be offered.
  assert_not_contains "$(<"$HOME/fzf.input")" 'embed-only' \
    'the picker must not offer a model that cannot serve a session' || rc=1

  # A missing value is the same request as an explicit -i.
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  out=$(ccmorph set gw model 2>/dev/null) || {
    print -u2 -- 'an omitted value must open the picker, not error'; rc=1
  }
  [[ "$out" == alpha-chat ]] || {
    print -u2 -- "an omitted value must save the chosen id (got: $out)"; rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# The four levels are Claude Code's own fixed enum, so this picker needs no
# endpoint call -- and must not make one.
test_set_effort_interactively_offers_the_engine_levels() {
  local test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  fake_fzf
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"
  # Unit-test dispatch/persistence here; the PTY suite drives the real keys.
  ccm_ui_select() {
    [[ "$1" == native ]] || return 1
    print -r -- "$3" > "$HOME/fixed.input"
    print -r -- "${${3%%$'\n'*}%%$'\t'*}"
  }
  local rc=0 out offered

  out=$(ccmorph set gw effort -i 2>/dev/null) || {
    print -u2 -- '-i must save a chosen effort level'
    rm -rf "$test_home"; return 1
  }
  # Cheapest first, so the first row is `low`.
  [[ "$out" == low ]] || { print -u2 -- "expected 'low' (got: $out)"; rc=1 }
  [[ "$(ccmorph get gw effort 2>/dev/null)" == low ]] || {
    print -u2 -- 'the chosen effort must be saved'; rc=1
  }
  offered="$(<"$HOME/fixed.input")"
  [[ ! -f "$HOME/fzf.input" ]] || { print -u2 -- "fixed effort choices must not invoke fzf"; rc=1; }
  local level
  for level in low medium high xhigh; do
    assert_contains "$offered" "$level" "the picker must offer $level" || rc=1
  done
  # No endpoint call belongs in a closed-enum picker.
  [[ ! -f "$HOME/curl.args" ]] || {
    print -u2 -- 'the effort picker must not query the endpoint'; rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# Without fzf the flag cannot open a list, but it must still leave the reader able
# to finish the job: the exact command for THIS slot, and the values to choose from.
test_the_picker_without_fzf_names_the_direct_command_and_the_choices() {
  local test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"
  local rc=0 err
  # `whence -p fzf` searches PATH, so hiding fzf means a PATH without it -- but the
  # library needs mkdir and friends, so the real PATH minus fzf's directory is what
  # this needs, not $HOME/bin alone.
  local nofzf real_fzf
  real_fzf=$(whence -p fzf 2>/dev/null)
  if [[ -n "$real_fzf" ]]; then
    nofzf="${PATH//${real_fzf:h}:/}"
  else
    nofzf="$PATH"   # already absent; the assertions below still apply
  fi
  err=$(PATH="$nofzf" ccmorph set gw effort -i 2>&1) && {
    print -u2 -- 'without fzf the picker must fail rather than guess'
    rc=1
  }
  assert_contains "$err" 'ccmorph set gw effort' \
    'the refusal must name the direct command with the profile filled in' || rc=1
  assert_contains "$err" 'xhigh' \
    'the refusal must still show what there was to choose from' || rc=1
  rm -rf "$test_home"
  return $rc
}

# `ccmorph model` writes settings.json's `model`, but a profile naming a backend
# model in its conf ALSO injects `--model <conf value>` at launch -- and a CLI
# argument outranks every settings layer in Claude Code. So on exactly the profiles
# this command exists for (proxy/apikey with `model=`), the value it saved was
# overridden on the very next launch and the command looked inert. The saved default
# is a later, more specific choice than the conf's bootstrap value, so it wins.
test_saved_default_model_beats_the_confs_bootstrap_model_at_launch() {
  local test_home
  test_home=$(mktemp -d)
  local TMPDIR="$test_home/tmp"
  mkdir -p "$TMPDIR"
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/proxy"
  # `ccmorph set proxy model chosen-model` writes exactly this.
  print -r -- '{"model":"chosen-model"}' \
    > "$HOME/.config/ccmorph/profiles/proxy/settings.json"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual rc=0
  actual=$(grep -E '^(ARG|MODEL_ENV)' "$CCMORPH_TEST_LOG")
  assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
    'the configured default model must be what launch hands Claude Code' || rc=1
  assert_not_contains "$actual" 'chosen-model' \
    'native saved state must not override the configured default' || rc=1

  rm -rf "$test_home"
  return $rc
}

# An explicit `--model` on the command line still beats the saved default: it is
# the most specific choice of the three, and the fix above must not invert that.
test_explicit_model_flag_beats_the_saved_default() {
  local test_home
  test_home=$(mktemp -d)
  local TMPDIR="$test_home/tmp"
  mkdir -p "$TMPDIR"
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/proxy"
  print -r -- '{"model":"chosen-model"}' \
    > "$HOME/.config/ccmorph/profiles/proxy/settings.json"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy --model flag-model prompt || {
    rm -rf "$test_home"
    return 1
  }

  # Asserted on the argv line only: the saved `model` key still sits in the
  # settings overlay, which is correct -- Claude Code just ranks the flag above it.
  local actual rc=0
  actual=$(grep '^ARG' "$CCMORPH_TEST_LOG")
  assert_contains "$actual" $'--model\tflag-model' \
    'an explicit --model must reach Claude Code' || rc=1
  assert_not_contains "$actual" 'chosen-model' \
    'an explicit --model must outrank the saved default' || rc=1

  rm -rf "$test_home"
  return $rc
}

# settings.json is Claude Code's file. `ccmorph effort` edits ONE key in it and
# every other key survives untouched -- including `modelSettings`, which is Claude
# Code's own per-model state written by an in-session `/effort`.
#
# v1.15.1 deleted those entries so this command's value would always win. That
# inverted the tool's job: ccmorph is a convenient editor for a default, not an
# arbiter of Claude Code's precedence. Claude Code consults per-model effort before
# the top-level default; that hierarchy is its design to own.
test_effort_command_leaves_native_per_model_state_untouched() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- \
    '{"modelSettings":{"kimi-k3":{"effortLevel":"low"},"other":{"effortLevel":"low","thinkingBudget":7}}}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  ccmorph set gw effort xhigh >/dev/null || {
    print -u2 -- 'ccmorph effort <profile> <level> must succeed'
    rm -rf "$test_home"; return 1
  }
  local got
  got=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
ms = d.get("modelSettings") or {}
print(d.get("effortLevel"),
      (ms.get("kimi-k3") or {}).get("effortLevel"),
      (ms.get("other") or {}).get("effortLevel"),
      (ms.get("other") or {}).get("thinkingBudget"))
' "$HOME/.config/ccmorph/profiles/gw/settings.json")
  # The default is written; every native per-model entry is exactly as it was.
  [[ "$got" == "None low low 7" && "$(ccmorph get gw effort)" == xhigh ]] || {
    print -u2 -- "native modelSettings must survive a new default (got: $got)"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_effort_command_sets_the_profiles_default_effort() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"model":"kept-model"}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  ccmorph set gw effort xhigh >/dev/null || {
    print -u2 -- 'ccmorph effort <name> <level> must succeed'
    rm -rf "$test_home"; return 1
  }
  local got
  got=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print(d.get("effortLevel"), d.get("model"))
' "$HOME/.config/ccmorph/profiles/gw/settings.json")
  [[ "$got" == "None kept-model" && "$(ccmorph get gw effort)" == xhigh ]] || {
    print -u2 -- "the default effort must land in conf, native settings kept (got: $got)"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# Claude Code's own enum is low|medium|high|xhigh. Accepting anything else would
# write a value the binary drops, so the profile would silently keep the old one.
test_effort_command_refuses_a_level_claude_code_would_drop() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  source "$CCMORPH_SOURCE"

  local rc=0
  if ccmorph set gw effort turbo >/dev/null 2>&1; then
    print -u2 -- 'an effort level outside low|medium|high|xhigh must be refused'
    rc=1
  fi
  [[ ! -f "$HOME/.config/ccmorph/profiles/gw/settings.json" ]] ||
    assert_not_contains "$(<"$HOME/.config/ccmorph/profiles/gw/settings.json")" 'turbo' \
      'a refused level must not be written' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Effort has a closed enum to validate against; a model id has none, so the only
# guard was "no CR/LF" -- and a flag-shaped value sailed straight through. A user
# typing `ccmorph set gw model --menu` (the flag form the design REJECTED in favour
# of `?`) had the literal string `--menu` written as their default model, and the
# next launch tried to run it. A model id never begins with `-` or `@`: both are
# how a value would be misread as a flag or a sentinel downstream, so they are
# refused with the command that was actually meant.
test_set_model_refuses_a_flag_shaped_value() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"model":"kept-model"}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 bad err
  # `-i` is deliberately NOT in this list: it is the picker flag, covered by
  # test_set_model_interactively_saves_the_chosen_id. These are the values that must
  # be refused: flag-shaped, and a multi-token string that can never resolve to a
  # model. The retired sentinels are here now -- they must read as garbage, not
  # quietly open a picker, which is what `-i` is for.
  for bad in --menu --pick -m @bogus @menu '?' "kimi k3"; do
    err=$(ccmorph set gw model "$bad" 2>&1) && {
      print -u2 -- "a flag-shaped model id '$bad' must be refused"
      rc=1
      continue
    }
    assert_contains "$err" 'model' \
      "the refusal must name what was being set (got: $err)" || rc=1
    # The existing value must survive every refusal.
    local still
    still=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("model"))' \
      "$HOME/.config/ccmorph/profiles/gw/settings.json")
    [[ "$still" == kept-model ]] || {
      print -u2 -- "a refused model id must not overwrite the default (now: $still)"
      rc=1
    }
  done
  rm -rf "$test_home"
  return $rc
}

# `-i` for effort needs no endpoint call: the four levels are Claude Code's own
# fixed enum, so the picker lists them directly.
test_set_adapter_is_rejected_as_automatic() {
  # Adapter selection is automatic and is not a user setting. The old write path
  # must reject it rather than creating a second compatibility control plane.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0 out conf="$HOME/.config/ccmorph/gw.conf"
  out=$(ccmorph set gw adapter true 2>&1) && {
    print -u2 -- 'set adapter must be rejected'
    rc=1
  }
  assert_contains "$out" 'model|effort' 'retired adapter setting must name the supported settings' || rc=1
  rm -rf "$test_home"
  return $rc

}

# A nonexistent profile is still an error, `-i` or not: the flag offers a choice
# among things that exist, and must never be read as permission to invent one.
#
# The name it checks matters. This previously used `proxy`, which `setup_home`
# creates -- so the old "rejects -i" assertion passed for the wrong reason (the flag
# was refused as a literal value, never reaching the profile check) and would have
# gone on passing whatever the profile logic did. `nosuch` is genuinely absent.
test_effort_interactive_still_requires_a_real_profile() {
  local test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_fzf
  source "$CCMORPH_SOURCE"
  local rc=0 err
  err=$(ccmorph set nosuch effort -i 2>&1) && {
    print -u2 -- "-i must not conjure a profile (got: $err)"
    rc=1
  }
  assert_contains "$err" 'nosuch' 'the refusal must name the profile asked for' || rc=1
  [[ ! -f "$HOME/.config/ccmorph/profiles/nosuch/settings.json" ]] || {
    print -u2 -- 'nothing may be written for a profile that does not exist'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# `pick` was the name before 1.10. It stays as an undocumented alias, because a
# rename that breaks someone's script to tidy a vocabulary is not a tidy-up.
test_the_former_pick_name_still_works() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0 picked
  picked=$(ccmorph _menu gw) || {
    print -u2 -- 'the former `pick` name must keep working as an alias'
    rm -rf "$test_home"
    return 1
  }
  [[ "$picked" == alpha-chat ]] || {
    print -u2 -- "the alias must behave identically; got: ${(qqq)picked}"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# The migration is only complete if the OLD token is gone from the code and the
# docs teach exactly one word. A half-rename leaves two spellings and a user
# guessing which one this version wants.
# `ccmorph -i` chains — profile chooser, then that profile's models. `ccmorph _menu`
# with no profile errored instead of doing the same, which is the same concept
# behaving two ways depending on where it is typed.
test_menu_with_no_account_chains_from_the_account_chooser() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0 picked
  picked=$(ccmorph _menu) || {
    print -u2 -- 'the model picker with no profile must offer the profile chooser first'
    rm -rf "$test_home"
    return 1
  }
  # The stub picks the first row of each chooser: the only profile, then its
  # first model. So a chained run emits a MODEL id, not an profile name.
  [[ "$picked" == alpha-chat ]] || {
    print -u2 -- "chaining must end at a model id; got: ${(qqq)picked}"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# Every other tool spells it -V. Accepting only -v/--version means a habit from
# any other CLI meets an "unknown command" error for no reason.
test_version_accepts_the_conventional_flags() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0 flag out
  for flag in -v -V --version version; do
    out=$(ccmorph "$flag" 2>&1) || {
      print -u2 -- "ccmorph $flag must succeed"
      rc=1
      continue
    }
    assert_contains "$out" 'ccmorph ' "ccmorph $flag must print the version" || rc=1
  done
  rm -rf "$test_home"
  return $rc
}

# The help placeholder said <name> for both an ACCOUNT and, in the models
# section, appeared to promise a model name — the exact ambiguity a reader hit.
# An profile argument is spelled <profile> everywhere it means one.
test_help_names_an_account_argument_unambiguously() {
  local rc=0

  # No help line may spell an profile argument as the bare, overloaded <name>.
  local offenders
  offenders=$(grep -nE '^\s+print "  ccmorph [a-z]+ <name>' "${CCMORPH_LIBRARY[@]}" || true)
  if [[ -n "$offenders" ]]; then
    print -u2 -- "help must spell an profile argument <profile>, not <name>:"
    print -u2 -- "$offenders"
    rc=1
  fi
  # …and it must actually teach the word, so the rename is not just a deletion.
  if ! grep -q '<profile>' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'help must use <profile> for profile arguments'
    rc=1
  fi
  return $rc
}

# One spelling per concept, in everything a user reads. The taught vocabulary is the
# verbs in `--help` plus `-i` as the single interactive flag; a retired verb surviving
# in help or docs is how two vocabularies start coexisting, which is what forced the
# retirement of both `@menu` and `?`.
# Backward compatibility was waived deliberately, so a retired verb must be REJECTED,
# not merely absent from help. v1.16.0 left `model`/`effort` reachable as the
# fallthrough target `set` jumps into, so `ccmorph model gateway` still answered --
# two vocabularies coexisting, which is the thing the rewrite existed to end. An
# undocumented command that works is worse than one that errors: it teaches a spelling
# the next release will remove.
# The subcommand lookup reads `claude --help`, so a test about subcommands must give
# the stub something to read. Wraps the EXISTING stub rather than replacing it: the
# argv/env logging every launch assertion depends on has to survive.
fake_claude_help() {
  local real="$HOME/bin/claude"
  local logged="$HOME/bin/claude-logging"
  mv "$real" "$logged"
  # One `echo` per line, no escapes. A printf carrying "\\n" into the GENERATED script
  # emits a literal backslash-n rather than a newline, so the Commands: block arrived
  # as a single unparsable line and every verb went unrecognised -- a fixture that
  # looked right and tested nothing.
  {
    print -r -- '#!/bin/sh'
    print -r -- 'if [ "$1" = "--help" ]; then'
    print -r -- '  echo "Usage: claude [options] [command]"'
    print -r -- '  echo ""'
    # The Options section is what the arity reader parses: one value-taking
    # option, one optional-value pair with a short alias, one flag. Those are
    # the three arities the launch scans distinguish.
    print -r -- '  echo "Options:"'
    print -r -- '  echo "  --system-prompt <prompt>   stub"'
    print -r -- '  echo "  --model <model>            stub"'
    print -r -- '  echo "  --effort <level>           stub"'
    print -r -- '  echo "  -r, --resume [value]       stub"'
    print -r -- '  echo "  --verbose                  stub"'
    print -r -- '  echo ""'
    print -r -- '  echo "Commands:"'
    local _v
    for _v in agents attach auth auto-mode doctor gateway import install logs \
              mcp plugin project respawn rm setup-token stop ultrareview update; do
      print -r -- "  echo \"  $_v   stub\""
    done
    print -r -- '  exit 0'
    print -r -- 'fi'
    print -r -- "exec \"$logged\" \"\$@\""
  } > "$real"
  chmod +x "$real"
}

test_claude_subcommands_are_asked_for_never_hardcoded() {
  # ccmorph must not carry a copy of another tool's command surface. A baked-in verb
  # list is wrong the moment Claude Code ships a new subcommand, and ccmorph would
  # then read it as an profile name -- so the answer is taken from the installed
  # binary's own `--help`.
  #
  # Proven by giving the stub a verb that does NOT exist in any real Claude Code, and
  # withholding one that does: a hardcoded list cannot pass both halves. The same
  # proof covers option arity: a value option that does not exist in any real
  # release must be discovered as one, from the same Options section.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<EOF' \
    'Usage: claude [options] [command]' \
    '' \
    'Options:' \
    '  --invented-value <thing>      an option no real release has' \
    '  --invented-flag               likewise, taking no value' \
    '  -z, --invented-optional [x]   likewise, with an optional value' \
    '' \
    'Commands:' \
    '  invented-verb   a subcommand no real release has' \
    '  mcp             configure MCP servers' \
    '  stop|kill <id>  an aliased subcommand' \
    'EOF' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"

  local rc=0 discovered
  # Interactive shells may replace grep with an rg wrapper whose -E means
  # encoding, not extended regex. Parsing must use the executable's contract.
  discovered=$(
    grep() { print -u2 -- 'unexpected shell grep override'; return 2; }
    _ccm_claude_verbs
  ) 2> "$test_home/discovery.err"
  [[ "$discovered" == $'invented-verb\nmcp\nstop\nkill' && ! -s "$test_home/discovery.err" ]] || rc=1
  _ccm_is_claude_subcommand kill || {
    print -u2 -- 'FAIL: an alias the installed binary lists must be recognised'
    rc=1
  }

  _ccm_is_claude_subcommand invented-verb || {
    print -u2 -- 'FAIL: a verb the installed binary lists must be recognised'
    rc=1
  }
  # `doctor` is real in Claude Code but absent from THIS stub's help, so a hardcoded
  # list would still claim it.
  _CCMORPH_CLAUDE_VERBS=""
  if _ccm_is_claude_subcommand doctor; then
    print -u2 -- 'FAIL: a verb the installed binary does not list must not be assumed'
    rc=1
  fi

  # Option arity is discovered from the same surface, and a value the option
  # owns is its value whatever it looks like: a flag-looking token behind a
  # value option belongs to that option, not to the launch. (#334)
  # Cases: option, the token after it, the tokens the option must own.
  local step opt next want_step
  for step in \
    '--invented-value|--continue|2' \
    '--invented-flag|x|1' \
    '-z|x|2' \
    '--invented-optional|-p|1' \
    '--no-such-option|x|0'; do
    IFS='|' read -r opt next want_step <<< "$step"
    _ccm_claude_option_step "$opt" "$next"
    [[ "$REPLY" == "$want_step" ]] || {
      print -u2 -- "FAIL: '$opt' with '$next' after it must own $want_step token(s), got $REPLY"
      rc=1
    }
  done

  # ccmorph's own verbs win over anything the binary also names, so a future Claude
  # Code `list` or `use` cannot steal a ccmorph command.
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<EOF' \
    'Commands:' \
    '  list            claude also has a list now' \
    '  rm              and an rm' \
    'EOF' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  _CCMORPH_CLAUDE_VERBS=""
  local verb
  for verb in list rm; do
    if _ccm_is_claude_subcommand "$verb"; then
      print -u2 -- "FAIL: ccmorph's own '$verb' must win over Claude Code's"
      rc=1
    fi
  done

  # No claude reachable at all: answer NO rather than aborting. An unknown word then
  # falls through to the profile reading, which is what a bare `ccmorph <word>` has
  # always meant. Run in a SUBSHELL with an empty PATH -- deleting the stub would not
  # prove it (the real binary is still on PATH) and would break every later test that
  # relies on setup_home's stub.
  # An empty PATH would leave no zsh to run the subshell at all, so keep the system
  # dirs and point HOME's bin at nothing: what must be absent is `claude`, not the
  # shell. Verified by checking the probe itself ran.
  local degraded probe_dir
  probe_dir=$(mktemp -d)
  degraded=$(PATH="$probe_dir:/usr/bin:/bin" zsh -fc "
    CCMORPH_SOURCE=${(q)CCMORPH_SOURCE}
    source \"\$CCMORPH_SOURCE\" >/dev/null 2>&1
    command -v claude >/dev/null 2>&1 && print 'claude-present' && exit
    _ccm_is_claude_subcommand mcp && print yes || print no
  " 2>/dev/null)
  rm -rf "$probe_dir"
  [[ "$degraded" == no ]] || {
    print -u2 -- "FAIL: with no claude reachable the lookup must answer no, got: $degraded"
    rc=1
  }

  # And the source must not carry a verb list to fall back on.
  if grep -qE '^\s*(agents\|attach|attach\|auth)' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- "FAIL: a hardcoded Claude Code verb list is back in the source"
    rc=1
  fi

  # The verb cache is a global: leaving this test's stub answers in it would give the
  # next test a Claude Code surface that does not exist. The arity table and the
  # shared help capture with it.
  _CCMORPH_CLAUDE_VERBS=""
  _CCMORPH_CLAUDE_OPT_ARITY=()
  _CCMORPH_CLAUDE_OPT_ARITY_PROBED=0
  _CCMORPH_CLAUDE_HELP=""
  _CCMORPH_CLAUDE_HELP_PROBED=0
  rm -rf "$test_home"
  return $rc
}

test_arguments_reach_claude_exactly_as_given() {
  # ccmorph adds nothing to the command line. Launch state -- the profile's model,
  # its settings, its credentials -- reaches Claude Code through CLAUDE_CONFIG_DIR
  # and the environment, which every entry point reads: a session, any subcommand,
  # the sessions Remote Control spawns. So no subcommand, hidden or new, can meet a
  # flag ccmorph invented, and ccmorph never needs Claude Code's grammar.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  # A ROUTED profile with a default model: the case that used to inject --model.
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid' 'model=gw-model'
  fake_claude_help
  source "$CCMORPH_SOURCE"

  local rc=0 argv
  # The last case is the #334 value-looks-like-a-flag spelling: `--effort`
  # there is the system prompt's VALUE, so no effort flag was given and the
  # arguments must pass through untouched either way.
  for argv in 'mcp list' 'remote-control --name x' 'prompt' '-p hello --output-format json' \
              '--system-prompt --effort -p x'; do
    : > "$CCMORPH_TEST_LOG"
    ccmorph run gw ${=argv} || {
      print -u2 -- "FAIL: ccmorph run gw $argv must succeed"
      rc=1; continue
    }
    local actual="$(<"$CCMORPH_TEST_LOG")"
    local tab=$'\t'
    assert_contains "$actual" "ARG${tab}${argv// /$tab}"$'\nSUBAGENT' \
      "'$argv' must reach Claude Code unchanged" || rc=1
    assert_contains "$actual" 'MODEL_ENV=gw-model' \
      "'$argv' must still carry the profile's model" || rc=1
    assert_contains "$actual" 'BASE_URL=https://gateway.invalid' \
      "'$argv' must still carry the profile's endpoint" || rc=1
  done

  rm -rf "$test_home"
  return $rc
}

test_a_missing_argument_prints_usage_under_nounset() {
  # ccmorph is sourced into whatever options the caller runs, and `local.sh` or a
  # strict wrapper script can `setopt nounset`. A bare `$2` in a usage guard then
  # aborts with "2: parameter not set" -- from the very branch whose job is to
  # explain the missing argument. `ccmorph rm` did exactly that.
  #
  # Run in a hermetic `zsh -fc` with nounset set BEFORE the source, because the whole
  # point is what happens when the option is already live.
  local rc=0 verb out
  for verb in rm mv default show login secret set probe list; do
    out=$(zsh -fc "
      setopt nounset
      CCMORPH_SOURCE=${(q)CCMORPH_SOURCE}
      source \"\$CCMORPH_SOURCE\" >/dev/null 2>&1
      ccmorph $verb 2>&1
    " 2>&1)
    if [[ "$out" == *"parameter not set"* ]]; then
      print -u2 -- "FAIL: 'ccmorph $verb' with no argument aborts under nounset: $out"
      rc=1
    fi
  done

  # And the usage text must name the verb the user typed. `ccmorph mv` reported
  # "usage: ccmorph rename", a verb retired in v1.16.0 -- so the one line whose job
  # is to correct the user pointed at a command that no longer exists.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  out=$(ccmorph mv 2>&1)
  assert_contains "$out" 'ccmorph mv' \
    'the usage line must name the verb that was typed, not a retired one' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_claude_subcommands_reach_claude_through_the_launch_pipeline() {
  # `ccmorph mcp list` was read as an ACCOUNT name and answered "unknown command or
  # profile 'mcp'", which made every Claude Code subcommand unreachable through
  # ccmorph. That is the wrong answer twice over: MCP servers, auth and plugins are
  # per-profile state, so they are exactly what a switcher has to be able to address,
  # and routing them through the launch pipeline is what points them at the right
  # profile's config directory instead of whatever ~/.claude happens to be.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  fake_claude_help
  source "$CCMORPH_SOURCE"
  ccmorph use gw >/dev/null || {
    print -u2 -- 'could not set a default profile'
    rm -rf "$test_home"; return 1
  }

  # The dispatcher marks a subcommand launch for the launcher, which is what suppresses
  # --model/--settings downstream, so record both argv and the mark.
  local calls="" flagged="" rc=0
  _ccm_launch_driver() {
    calls+="[${_CCMORPH_LAUNCH_PROFILE:-}:${(j: :)${(q)@}}]"
    flagged+="${CCMORPH_SUBCOMMAND_LAUNCH:-0}"
  }

  # A subcommand rides behind the explicit launch verb and every argument after it
  # survives untouched.
  local verb
  for verb in mcp auth doctor plugin agents update; do
    calls=""
    ccmorph run "$verb" list
    assert_contains "$calls" "[:$verb list]" \
      "ccmorph run $verb must reach Claude Code" || rc=1
  done
  [[ "$flagged" == 111111 ]] || {
    print -u2 -- "FAIL: every subcommand launch must be marked for the launcher, got: $flagged"
    rc=1
  }

  # An explicit profile still wins, and the subcommand rides behind it.
  calls=""; ccmorph run gw mcp list
  assert_contains "$calls" '[gw:mcp list]' \
    'an explicit profile must still be honoured before a subcommand' || rc=1

  # `rm` is NOT passed through: ccmorph's own rm matches earlier, and deleting an
  # profile is what it has always meant. Guard the precedence, because a
  # pass-through list that swallowed `rm` would silently stop removing profiles.
  local err
  # A VALID but absent name: _ccm_valid rejects hyphens, so a hyphenated name would
  # fail the shape check before reaching the existence check this asserts on.
  err=$(ccmorph rm no_such_account 2>&1)
  assert_contains "$err" "no profile 'no_such_account'" \
    "ccmorph rm must stay ccmorph's own verb" || rc=1
  assert_not_contains "$calls" 'rm' \
    'ccmorph rm must never reach Claude Code as a subcommand' || rc=1

  # A typo must still be told it is a typo, in ccmorph's vocabulary — the
  # pass-through is only behind `run`, never a catch-all that hands anything unknown
  # to Claude Code to reject.
  err=$(ccmorph mpc list 2>&1)
  assert_contains "$err" "unknown command 'mpc'" \
    'an unrecognised word must still be an error, not a pass-through' || rc=1

  # The binary accepts its own options before the command (`claude --verbose mcp
  # …`, `claude --model foo mcp …`), so the mark must survive them. Each option
  # steps over the value it owns: an option VALUE that looks like a verb is not
  # the command, and behind `-p` the positional is the prompt, never a command.
  # (#334)
  local scan argv want
  for scan in '--verbose mcp list:1' '--model pick mcp list:1' \
              '--system-prompt mcp list:0' '-p mcp list:0'; do
    argv="${scan%:*}"
    want="${scan##*:}"
    calls=""; flagged=""
    ccmorph run gw ${=argv}
    [[ "$flagged" == "$want" ]] || {
      print -u2 -- "FAIL: 'ccmorph run gw ${scan%:*}' must mark subcommand=$want, got: $flagged"
      rc=1
    }
  done

  rm -rf "$test_home"
  return $rc
}

test_a_retired_verb_is_rejected_not_merely_undocumented() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"model":"kept"}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 verb err
  for verb in model effort menu pick key token default show secret creds start rename; do
    err=$(ccmorph "$verb" gw 2>&1) && {
      print -u2 -- "the retired verb '$verb' must not still work"
      rc=1
      continue
    }
    assert_contains "$err" 'unknown command' \
      "a retired verb must report itself unknown (got: $err)" || rc=1
  done
  rm -rf "$test_home"
  return $rc
}

test_the_window_cache_holds_every_model_it_is_told() {
  # The per-model window cache was written with a single `>|`, so it held ONE row: a
  # LAST-MODEL cache wearing a per-model name. Switch model, switch back, and the
  # window had to be probed again -- on a mixed route, nearly every launch.
  #
  # It is a table now, and this pins the two ways a table silently stops being one:
  #  1. a second model must not evict the first;
  #  2. the row for a model must be matched LITERALLY. A model id is data, not a
  #     pattern -- `Qwen3.6-27B-FP8` used as a regex also matches `Qwen3X6-27B-FP8`,
  #     so caching one dropped the other and re-probed it. 27 ids in a real catalogue
  #     here contain a regex metacharacter, so this is the common case, not the edge.
  local c rc=0
  c=$(mktemp -d)/context-window

  _cache_put() {  # mirrors the launch path's write
    local m="$1" w="$2" t="${c}.$$"
    { mkdir -p "${c:h}" &&
      { print -r -- "$m	$w"
        if [[ -f "$c" && ! -L "$c" ]]; then
          command grep -vF -- "${m}	" "$c" 2>/dev/null | command head -n 63
        fi
        :
      } >| "$t" && command mv -f "$t" "$c"
    } 2>/dev/null || { command rm -f "$t" 2>/dev/null; : }
  }

  _cache_put 'Qwen3.6-27B-FP8' 131072
  _cache_put 'Qwen3X6-27B-FP8' 999999
  _cache_put 'gpt-6-astra'     200000
  _cache_put 'Qwen3.6-27B-FP8' 262144   # update in place, not a duplicate row

  local body="$(<"$c")"
  assert_contains "$body" 'Qwen3X6-27B-FP8	999999' \
    'a look-alike id was evicted: the row must be matched literally, not as a regex' || rc=1
  assert_contains "$body" 'gpt-6-astra	200000' \
    'a second model evicted the first: the cache is not a table' || rc=1
  assert_contains "$body" 'Qwen3.6-27B-FP8	262144' \
    'the updated row is missing' || rc=1
  if (( $(command grep -cF -- 'Qwen3.6-27B-FP8	' "$c") != 1 )); then
    print -u2 -- 'the updated model has a duplicate row; the table will grow forever'
    rc=1
  fi
  unfunction _cache_put
  rm -rf "${c:h}"
  return $rc
}

test_the_retired_window_verb_still_dispatches() {
  # The rename kept the pre-2.4.0 verb as a dispatch alias, for scripts and muscle
  # memory that already say it. A bulk rename then rewrote that alias into a second
  # copy of the NEW verb, so the case arm listed one word twice, the comment promising
  # back-compat was false, and the retired verb fell through to Claude Code. Nothing
  # failed, because no test named it.
  #
  # This asserts BEHAVIOUR, not spelling. A first version of this test grepped the
  # source for `probe|<retired>)` and would itself have broken the moment that arm was
  # rewritten to assemble the word at runtime -- a test of how the code is written is
  # not a test of what it does. The needle is still assembled from fragments so a
  # future rename cannot rewrite it here either.
  local test_home retired
  retired="mea""sure"
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0
  # The verb must NOT be forwarded to Claude Code as a subcommand, and must not be
  # read as an profile name. Both spellings must reach the same internal handler,
  # which with no profile configured reports no routed profiles rather than "unknown
  # command" (the fall-through) or "no profile" (read as a profile).
  # `--help` reaches the same dispatch arm and returns without touching the network.
  # An earlier draft used `--declared-only`, which resolves routes and hung the suite
  # against a real gateway: a DISPATCH test must not depend on reachability.
  local out_new out_old
  out_new=$(ccmorph probe --help 2>&1)
  out_old=$(ccmorph "$retired" --help 2>&1)

  if [[ "$out_old" == *"unknown command"* || "$out_old" == *"no profile"* ]]; then
    print -u2 -- "'${retired}' no longer dispatches to the window verb: $out_old"
    rc=1
  fi
  # Same handler, so the same message. Compared rather than pattern-matched, so this
  # cannot pass by both spellings failing in two different ways.
  if [[ "$out_new" != "$out_old" ]]; then
    print -u2 -- "'probe' and '${retired}' took different paths"
    print -u2 -- "  probe:     $out_new"
    print -u2 -- "  ${retired}: $out_old"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_only_the_current_vocabulary_is_taught() {
  local rc=0

  # No retired verb may appear in the help text a user reads. `list` is NOT retired:
  # it is the discoverable spelling of the bare form, kept deliberately because a word
  # that appears in help and tab completion is findable where a bare invocation is not.
  if grep -qE '^\s+print "  ccmorph (menu|pick|models|model|effort|key|token|rename|use|creds|start) ' \
       "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'help must not teach a retired verb'
    rc=1
  fi
  # Nor in the docs -- and that includes a retired ARGUMENT FORM, not just a retired
  # verb. `ccmorph probe --profile <profile>` survived the rewrite in the README
  # because the verb was still current and only its spelling of the profile had
  # changed, which a verb-name check cannot see. `claude -a <profile>` is retired for
  # the same reason the `claude` shadow is: ccmorph no longer defines that command.
  local doc
  for doc in README.md docs/guide.md docs/ARCHITECTURE.md; do
    if grep -qE 'ccmorph (menu|pick|model|effort|key|token|rename|init) |@pick' \
         "$REPO_ROOT/$doc"; then
      print -u2 -- "$doc must not teach a retired verb or sentinel"
      rc=1
    fi
    # Matched as a COMMAND the reader could type (leading backtick or shell
    # prompt), not as a bare word: the closed-issues table has to be able to say
    # that `ccrelay` was removed, and a substring match would forbid recording
    # the very removal this guard exists to enforce.
    if grep -qE '(`|\$ )(ccrelay|claude --profile)' "$REPO_ROOT/$doc"; then
      print -u2 -- "$doc must not teach a retired argument form"
      rc=1
    fi
    if grep -qE 'ccmorph probe --profile' "$REPO_ROOT/$doc"; then
      print -u2 -- "$doc must not teach a retired argument form"
      rc=1
    fi
    # The verb is `probe`. The old spelling survives ONLY as a dispatch alias for
    # scripts that already say it, and a doc must never teach it: an alias a reader
    # is taught is not an alias, it is a second name, and the next person writes one
    # while reading the other. The needle is assembled from fragments so a future
    # global rename cannot silently rewrite the very word this guard forbids --
    # exactly what happened once, leaving the guard demanding and forbidding the
    # same spelling.
    local _retired_verb="mea""sure"
    if grep -qE "(\`|\$ )ccmorph ${_retired_verb}" "$REPO_ROOT/$doc"; then
      print -u2 -- "$doc teaches the retired verb 'ccmorph ${_retired_verb}'; docs teach 'ccmorph probe'"
      rc=1
    fi
  done
  # `-i` is the ONE spelling of "ask me", and no reader may be shown either retired
  # sentinel. `@menu` was a value, so it had to be repeated per slot and produced the
  # ambiguous `ccmorph @menu --model @menu`; `?` is a zsh glob the caller's shell eats
  # before ccmorph runs. Both are gone from code and docs, not merely undocumented,
  # because a half-rename leaves two vocabularies and a user guessing which this
  # version wants.
  for doc in README.md docs/guide.md docs/ARCHITECTURE.md; do
    if grep -qE '(ccmorph [a-z@<-]* |--model |model |effort )\?( |$)' "$REPO_ROOT/$doc"; then
      print -u2 -- "$doc must teach -i, not a bare ? the caller's shell would glob"
      rc=1
    fi
    if grep -q '@menu' "$REPO_ROOT/$doc"; then
      print -u2 -- "$doc still teaches the retired @menu sentinel; -i is the one spelling"
      rc=1
    fi
  done
  # And the flag must actually be recognised, not merely documented.
  if ! grep -q -- '-i|--interactive)' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- '-i/--interactive must be accepted wherever a choice is taken'
    rc=1
  fi
  # And help must teach the set verb. The help is rendered through `_ccm_help` rows,
  # not literal `print "  ccmorph …"` lines, so the check matches the call shape.
  if ! grep -qE '_ccm_help "ccmorph set <profile> <model\|effort>' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'help must teach the set verb'
    rc=1
  fi
  # Launching must be taught as a VERB. A bare `ccmorph` prints usage, so if help did
  # not name `run`, the tool would have no discoverable way to launch at all.
  if ! grep -qE '_ccm_help "ccmorph run"' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'help must teach ccmorph run as the way to launch'
    rc=1
  fi
  # `go` was considered and rejected as too vague (and it shadows the Go toolchain in
  # muscle memory). Guard it so it cannot drift back in beside `start`.
  if grep -qE '^\s*go\)' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'the rejected `go` verb must not reappear alongside start'
    rc=1
  fi
  return $rc
}

test_menu_returns_a_bare_model_id() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local picked rc=0
  picked=$(ccmorph _menu gw) || {
    print -u2 -- 'the model picker must succeed when fzf returns a choice'
    rm -rf "$test_home"
    return 1
  }

  [[ "$picked" == alpha-chat ]] || {
    print -u2 -- "pick must emit only the model id; got: ${(qqq)picked}"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_launch_interactive_resolves_the_account() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf alpha 'api_key=sk-test-alpha' 'base_url=https://gateway.invalid'
  write_apikey_conf zeta 'api_key=sk-test-zeta' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  # No --model, so -i asks for the profile AND then the model. The fake picker
  # answers both from the same stub.
  _ccm_launch -i prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")" rc=0
  assert_contains "$actual" 'API_KEY=sk-test-alpha' \
    '-i must launch the selected profile' || rc=1
  assert_not_contains "$actual" $'\t-i' \
    'the interactive flag must never reach Claude Code' || rc=1
  assert_not_contains "$(<"$HOME/fzf.input")" 'sk-test-' \
    'profile picker rows must not expose credentials' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_interactive_with_an_account_asks_only_the_model() {
  # Position is the whole contract: with the profile already named, `-i` must not
  # ask for it again. Every spelling of "the profile is given" behaves the same.
  local form test_home actual rc=0
  for form in '-a alpha' '--profile alpha' '--profile=alpha' 'run alpha' 'run -i alpha'; do
    test_home=$(mktemp -d)
    setup_home "$test_home"
    fake_gateway
    fake_fzf
    write_apikey_conf alpha 'api_key=sk-test-alpha' 'base_url=https://gateway.invalid'
    source "$CCMORPH_SOURCE"

    case "$form" in
      '-a alpha')        _ccm_launch -a alpha -i prompt || rc=1 ;;
      '--profile alpha') _ccm_launch --profile alpha -i prompt || rc=1 ;;
      'run alpha')       ccmorph run alpha -i prompt || rc=1 ;;
      'run -i alpha')    ccmorph run -i alpha prompt || rc=1 ;;
      *)                 _ccm_launch --profile=alpha -i prompt || rc=1 ;;
    esac
    actual="$(<"$CCMORPH_TEST_LOG")"
    assert_contains "$actual" 'API_KEY=sk-test-alpha' \
      "$form -i must launch that profile without asking again" || rc=1
    assert_contains "$actual" 'MODEL_ENV=alpha-chat' \
      "$form -i must still ask for the model" || rc=1
    # The profile chooser must never have run: only the MODEL list was offered, and
    # the model rows carry no credential.
    if [[ -f "$HOME/fzf.input" ]] && grep -q 'sk-test-' "$HOME/fzf.input"; then
      print -u2 -- "$form -i ran the profile chooser it should have skipped"
      rc=1
    fi
    rm -rf "$test_home"
  done
  return $rc
}

test_public_model_picker_flag_asks_only_for_model() {
  local form test_home actual rc=0
  for form in default explicit; do
    test_home=$(mktemp -d)
    setup_home "$test_home"
    fake_gateway
    fake_fzf
    write_apikey_conf alpha 'api_key=sk-test-alpha' 'base_url=https://gateway.invalid'
    write_apikey_conf zeta 'api_key=sk-test-zeta' 'base_url=https://gateway.invalid'
    print -r -- alpha > "$HOME/.config/ccmorph/active"
    source "$CCMORPH_SOURCE"

    if [[ "$form" == default ]]; then
      ccmorph run --model -i prompt || rc=1
    else
      ccmorph run alpha --model -i prompt || rc=1
    fi
    actual="$(<"$CCMORPH_TEST_LOG")"
    assert_contains "$actual" 'MODEL_ENV=alpha-chat' \
      "public $form --model -i must forward the picked model" || rc=1
    assert_not_contains "$actual" $'ARG\t-i' \
      "public $form --model -i must not pass -i as a model or native argument" || rc=1
    if [[ -f "$HOME/fzf.input" ]] && grep -q 'sk-test-' "$HOME/fzf.input"; then
      print -u2 -- "public $form --model -i ran the profile chooser"
      rc=1
    fi
    rm -rf "$test_home"
  done
  return $rc
}

test_interactive_asks_no_model_where_ccmorph_owns_no_list() {
  # `-i` must ask only questions ccmorph can answer. A routed profile has an
  # endpoint to enumerate; a native Anthropic profile does not, and Claude Code owns
  # the model choice there. Asking anyway sent a discovery request to
  # api.anthropic.com and failed the launch with "could not reach" -- the tool
  # inventing a question it has no way to answer, on the most common profile type.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_fzf
  printf '%s\n' 'type=sub' 'email=someone@example.com' \
    > "$HOME/.config/ccmorph/native.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/native"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -i prompt || {
    print -u2 -- '-i on a native profile must launch, not fail reaching an endpoint'
    rm -rf "$test_home"; return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_not_contains "$actual" '--model' \
    'ccmorph must not choose a model for a native profile' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_cancelled_profile_picker_launches_nothing() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'exit 130' > "$HOME/bin/fzf"
  chmod +x "$HOME/bin/fzf"
  write_apikey_conf alpha 'api_key=sk-test-alpha'
  source "$CCMORPH_SOURCE"

  local rc=0
  if _ccm_launch -i prompt 2>/dev/null; then
    print -u2 -- 'a cancelled profile picker must abort the launch'
    rc=1
  fi
  [[ ! -s "$CCMORPH_TEST_LOG" ]] || {
    print -u2 -- 'a cancelled profile picker must not launch Claude Code'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_profile_picker_does_not_change_default() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf alpha 'api_key=sk-test-alpha' 'base_url=https://gateway.invalid'
  write_apikey_conf zeta 'api_key=sk-test-zeta' 'base_url=https://gateway.invalid'
  printf '%s' zeta > "$HOME/.config/ccmorph/active"
  source "$CCMORPH_SOURCE"

  _ccm_launch -i prompt >/dev/null || {
    rm -rf "$test_home"
    return 1
  }

  local rc=0
  [[ "$(<"$HOME/.config/ccmorph/active")" == zeta ]] || {
    print -u2 -- '-i must not change the saved default'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_profile_pick_precedes_model_pick() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf alpha \
    'api_key=sk-test-alpha' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  _ccm_launch -i prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")" rc=0
  assert_contains "$actual" 'MODEL_ENV=alpha-chat' \
    'the model list must come from the profile just chosen' || rc=1
  assert_contains "$actual" 'API_KEY=sk-test-alpha' \
    '-i must launch the selected profile' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_interactive_resolves_a_real_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_fzf
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw -i prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains \
    "$actual" \
    'MODEL_ENV=alpha-chat' \
    '-i must reach Claude Code as the chosen model' || rc=1
  assert_not_contains \
    "$actual" \
    $'--model\t-i' \
    'the flag must never be passed through as a model value' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_aborts_when_pick_is_cancelled() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  printf '%s\n' '#!/bin/sh' 'exit 130' > "$HOME/bin/fzf"
  chmod +x "$HOME/bin/fzf"
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0
  if _ccm_launch -a gw -i prompt 2>/dev/null; then
    print -u2 -- 'a cancelled picker must abort the launch, not start a session'
    rc=1
  fi
  [[ ! -s "$CCMORPH_TEST_LOG" ]] || {
    print -u2 -- 'a cancelled picker must not launch Claude Code at all'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_proxy_model_is_bootstrap_fallback() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" $'ARG\tprompt\nSUBAGENT' \
    'a proxy profile model must remain the bootstrap fallback' || rc=1
  assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
    'a proxy profile model must remain the bootstrap fallback' || rc=1
  assert_contains \
    "$actual" \
    'ALWAYS_EFFORT=1' \
    'proxy profiles must retain CLAUDE_CODE_ALWAYS_ENABLE_EFFORT=1' || rc=1
  assert_contains \
    "$actual" \
    'LAUNCH_MODEL=<unset>' \
    'ccmorph must not expose wrapper-owned model relay state' || rc=1
  assert_contains \
    "$actual" \
    'LAUNCH_EFFORT=<unset>' \
    'ccmorph must not expose wrapper-owned effort relay state' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_proxy_context_limit_reaches_claude() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  print -- 'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=400000' \
    >> "$HOME/.config/ccmorph/proxy.conf"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains \
    "$actual" \
    'MAX_CONTEXT=400000' \
    'proxy context limit must reach the Claude Code process' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_add_folds_proxy_into_apikey() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  # Stub curl so the /v1/messages probe returns 404 — the gateway does not
  # speak Anthropic natively, so the adapter must be auto-enabled.
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do' \
    '  case "$arg" in *messages*) printf "404"; exit 0 ;; esac' \
    'done' \
    'for arg do' \
    '  if [ "$arg" = "-w" ]; then printf "200"; exit 0; fi' \
    'done' \
    'exit 1' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"

  local out
  out=$(ccmorph add gateway <<< $'3\ngateway-key\nhttp://gateway.invalid\n') || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$HOME/.config/ccmorph/gateway.conf")" rc=0
  assert_contains "$actual" 'type=apikey' \
    'a key-authenticated gateway must use the single apikey profile type' || rc=1
  assert_not_contains "$out" 'proxy         ' \
    'new-profile onboarding must not offer a duplicate proxy type' || rc=1
  assert_contains "$actual" 'adapter=true' \
    'adapter must be auto-enabled when the endpoint does not speak /v1/messages' || rc=1
  assert_not_contains "$actual" 'env.CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY=' \
    'gateway compatibility must be explicit, not a proxy-type default' || rc=1
  assert_not_contains "$actual" 'env.ENABLE_TOOL_SEARCH=' \
    'the folded type must not disable native tool search automatically' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_add_defaults_the_adapter_on_for_any_non_anthropic_endpoint() {
  # The costs are asymmetric, and that decides the default. `adapter=true` against an
  # endpoint that already speaks Anthropic costs one wasted local hop and still works;
  # `adapter=false` against one that does not is a hard launch failure with a dialect
  # error. So an inconclusive probe must fail toward ON.
  #
  # It used to fail toward OFF, and that was wrong on a live profile: an ollama
  # endpoint serves /v1/models (200) and 404s /v1/messages -- exactly the case needing
  # translation -- and was written with no adapter key at all, because the probe
  # happened to be inconclusive when it was added.
  #
  # Native is claimed only on PROOF, in one of two shapes: a 2xx, or a 404 naming the
  # MODEL (route present, model wrong). Everything else gets the adapter.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0 case_name expected got
  # case -> body/status the fake curl returns, and the verdict required.
  local -A cases=(
    native_model_404 native
    native_200       native
    route_404        adapter
    waf_405          adapter
    auth_401         adapter
    unreachable      adapter
  )

  for case_name in ${(k)cases}; do
    expected="${cases[$case_name]}"
    printf '%s\n' \
      '#!/bin/sh' \
      'case "$CASE" in' \
      '  native_model_404) printf %s "{\"error\":{\"type\":\"not_found_error\",\"message\":\"model: probe\"}}"; printf "\n404" ;;' \
      '  native_200)       printf %s "{\"id\":\"msg_1\"}"; printf "\n200" ;;' \
      '  route_404)        printf %s "<html>404 not found</html>"; printf "\n404" ;;' \
      '  waf_405)          printf %s "<html>blocked</html>"; printf "\n405" ;;' \
      '  auth_401)         printf %s "{\"error\":\"unauthorized\"}"; printf "\n401" ;;' \
      '  unreachable)      printf "\n000" ;;' \
      'esac' \
      'exit 0' \
      > "$HOME/bin/curl"
    chmod +x "$HOME/bin/curl"

    rm -f "$HOME/.config/ccmorph/probe_$case_name.conf"
    print -l 2 'k' 'https://gw.invalid' |
      CASE="$case_name" ccmorph add "probe_$case_name" >/dev/null 2>&1 || true

    [[ -f "$HOME/.config/ccmorph/probe_$case_name.conf" ]] || {
      print -u2 -- "FAIL: add wrote no conf for the $case_name endpoint"
      rc=1
      continue
    }
    got=native
    grep -q '^adapter=true' "$HOME/.config/ccmorph/probe_$case_name.conf" && got=adapter
    [[ "$got" == "$expected" ]] || {
      print -u2 -- "FAIL: a $case_name endpoint must be classified $expected, got $got"
      rc=1
    }
  done

  rm -rf "$test_home"
  return $rc
}

test_add_never_asks_how_to_reach_the_endpoint() {
  # Reaching a private endpoint is a MACHINE property -- this laptop needs a bridge
  # for these hosts -- not a profile one, and it applies to almost nobody. `add`
  # therefore asks nothing about it: the caller exports HTTPS_PROXY if they need one,
  # and the probe then classifies the endpoint through it like any other.
  local rc=0

  local add_block
  add_block=$(sed -n '/^    add)/,/^    login)/p' "${CCMORPH_LIBRARY[@]}")
  if print -r -- "$add_block" | grep -qiE 'read .*(tunnel|transport|proxy)|print.*"(transport|tunnel|proxy)\?'; then
    print -u2 -- 'FAIL: add must not prompt for how to reach the endpoint'
    rc=1
  fi
  print -r -- "$add_block" | grep -q '_ccm_probe_nudge ' || rc=1
  if print -r -- "$add_block" | grep -q -- '-X POST'; then
    print -u2 -- 'add must not send inference probes'
    rc=1
  fi
  return $rc
}

test_add_apikey_native_endpoint_skips_adapter() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  # Default curl stub returns 200 for -w probes — the endpoint speaks
  # /v1/messages natively, so no adapter should be set.
  local out
  out=$(ccmorph add nativegw <<< $'3\nnative-key\nhttp://native.invalid\n') || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$HOME/.config/ccmorph/nativegw.conf")" rc=0
  assert_contains "$actual" 'type=apikey' \
    'apikey type must be written' || rc=1
  assert_not_contains "$actual" 'adapter=true' \
    'adapter must NOT be set when the endpoint speaks /v1/messages' || rc=1
  rm -rf "$test_home"
  return $rc
}


# The auto-detect probe decides on the RESPONSE BODY, not on the status alone,
# and these four cases are why. Each one shipped uncovered and each was wrong:
# the suite only ever stubbed a bodiless 404 and a 200.
#
# Helper: stub curl so the /v1/messages probe answers with a chosen status and
# body, in the `-o - -w '\n%{http_code}'` shape the probe actually requests.
_stub_probe_response() {  # $1 = status, $2 = body
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do' \
    "  case \"\$arg\" in *messages*) printf '%s\\n%s' '$2' '$1'; exit 0 ;; esac" \
    'done' \
    'for arg do' \
    '  if [ "$arg" = "-w" ]; then printf "200"; exit 0; fi' \
    'done' \
    'exit 1' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
}

test_add_apikey_model_level_404_is_native() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  # THE regression this guards: a native Anthropic endpoint returns 404 for an
  # unknown model id, and the probe deliberately sends a nonexistent one. Reading
  # that as "no /v1/messages route" put a translator in front of exactly the
  # native endpoints auto-detection exists to spare.
  _stub_probe_response 404 '{"type":"error","error":{"type":"not_found_error","message":"model: probe"}}'

  local out rc=0
  out=$(ccmorph add nativemodel404 <<< $'3\nk\nhttp://native404.invalid\n') || {
    rm -rf "$test_home"
    return 1
  }
  local actual="$(<"$HOME/.config/ccmorph/nativemodel404.conf")"
  assert_not_contains "$actual" 'adapter=true' \
    'a 404 naming the MODEL proves the route exists — must not enable the adapter' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_add_apikey_route_level_404_enables_adapter() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  # The other half of the same branch: a 404 that says nothing about a model is a
  # genuinely absent route, and that one does need translation.
  _stub_probe_response 404 '<html><head><title>404 Not Found</title></head></html>'

  local out rc=0
  out=$(ccmorph add route404 <<< $'3\nk\nhttp://route404.invalid\n') || {
    rm -rf "$test_home"
    return 1
  }
  local actual="$(<"$HOME/.config/ccmorph/route404.conf")"
  assert_contains "$actual" 'adapter=true' \
    'a 404 that names no model is an absent route — the adapter must be enabled' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_add_apikey_inconclusive_probe_enables_the_adapter() {
  # These two used to assert the OPPOSITE -- that an inconclusive probe wrote no
  # adapter key -- and the rule was wrong because it weighed the two errors equally.
  # They are not equal: adapter=true against an endpoint that already speaks Anthropic
  # costs one wasted local hop and still works, while adapter=false against one that
  # does not is a hard launch failure with a dialect error. Failing toward ON is the
  # cheaper mistake, so an inconclusive probe now takes it.
  #
  # Confirmed on a live profile before changing this: an ollama endpoint here serves
  # /v1/models (200) and 404s /v1/messages -- needing translation -- yet had no adapter
  # key, because its probe was inconclusive on the day it was added.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0 out actual

  # 000: any network failure (VPN down, DNS). Nothing to retry through, so the
  # verdict stands on the asymmetry alone.
  _stub_probe_response 000 ''
  out=$(ccmorph add vpndown <<< $'3\nk\nhttp://unreachable.invalid\n') || {
    rm -rf "$test_home"
    return 1
  }
  actual="$(<"$HOME/.config/ccmorph/vpndown.conf")"
  assert_contains "$actual" 'adapter=true' \
    'an unreachable endpoint must get the adapter, which is what works for a gateway' || rc=1
  assert_contains "$out" 'adapter false' \
    'and the user must be told the one-line correction if it turns out native' || rc=1

  # 401/403: refused before routing, so it proves nothing about the dialect either.
  _stub_probe_response 401 '{"error":{"message":"invalid x-api-key"}}'
  out=$(ccmorph add authrefused <<< $'3\nk\nhttp://authrefused.invalid\n') || {
    rm -rf "$test_home"
    return 1
  }
  actual="$(<"$HOME/.config/ccmorph/authrefused.conf")"
  assert_contains "$actual" 'adapter=true' \
    'an auth-refused probe must still default the adapter on' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_help_teaches_credential_type_not_gateway_shape() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local help="$(ccmorph help add)" rc=0
  assert_contains "$help" 'apikey' \
    'help must teach the one API-key credential type' || rc=1
  assert_contains "$help" 'credential presented to Anthropic or a gateway' \
    'apikey must be defined by credential mechanism, not provider identity' || rc=1
  assert_not_contains "$help" $'\n  proxy' \
    'help must not teach legacy proxy as a second profile type' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_direct_resume_recovers_last_main_thread_model_command() {
  local test_home
  local session_id='session-123'
  local transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-message>model</command-message>\\n<command-name>/model</command-name>\\n<command-args>first-model</command-args>"}}' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-message>model</command-message>\\n<command-name>/model</command-name>\\n<command-args>current-main-model[1m]</command-args>"}}' \
    '{"type":"user","isSidechain":true,"message":{"role":"user","content":"<command-message>model</command-message>\\n<command-name>/model</command-name>\\n<command-args>sidechain-model</command-args>"}}' \
    > "$transcript"

  _ccm_launch -a proxy --resume "$session_id" || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" $'ARG\t--resume\tsession-123\nSUBAGENT' \
    'direct resume must restore the last main-thread user /model argument' || rc=1
  assert_contains "$actual" 'MODEL_ENV=current-main-model[1m]' \
    'direct resume must restore the last main-thread user /model argument' || rc=1
  assert_not_contains \
    "$actual" \
    'sidechain-model' \
    'direct resume must ignore sidechain /model commands' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_direct_resume_derives_the_carried_claude_send_id() {
  local test_home session_id='resume-1m' transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for a do case "$a" in *http_code*) echo 200; exit 0 ;; esac; done' \
    'printf '\''{"data":[{"id":"claude-opus-5","max_input_tokens":1000000}]}\n'\''' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    'model=fallback-model' > "$HOME/.config/ccmorph/resume1m.conf"
  write_profile_context resume1m claude-opus-5=1000000
  install_model_window
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/model</command-name>\\n<command-args>claude-opus-5</command-args>"}}' \
    > "$transcript"

  _ccm_launch -a resume1m --resume "$session_id" || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")" rc=0
  assert_contains "$actual" 'MODEL_ENV=claude-opus-5[1m]' \
    'a resumed probed Claude model must pass through the canonical send-id resolver' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_direct_resume_carries_a_model_without_a_profile_default() {
  local test_home session_id='resume-no-default' transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    > "$HOME/.config/ccmorph/nodefault.conf"
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/model</command-name>\\n<command-args>alpha-chat</command-args>"}}' \
    > "$transcript"

  _ccm_launch -a nodefault --resume "$session_id" || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")" rc=0
  assert_contains "$actual" 'MODEL_ENV=alpha-chat' \
    'a routed resume must carry its valid model even when the profile has no default' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_direct_resume_recovers_the_last_assistant_model_without_a_model_command() {
  local test_home session_id='resume-assistant-model' transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    > "$HOME/.config/ccmorph/nodefault.conf"
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"high","message":{"role":"assistant","model":"alpha-chat","content":[]}}' \
    '{"type":"assistant","isSidechain":true,"effort":"high","message":{"role":"assistant","model":"sidechain-model","content":[]}}' \
    > "$transcript"

  _ccm_launch -a nodefault --resume "$session_id" || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")" rc=0
  assert_contains "$actual" 'MODEL_ENV=alpha-chat' \
    'a routed resume with no /model command must recover the main assistant model' || rc=1
  assert_not_contains "$actual" 'sidechain-model' \
    'assistant sidechains must not decide the resumed model' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_an_option_value_that_looks_like_a_flag_is_not_that_flag() {
  # The model loop scanned argv one token at a time, so the VALUE of a native
  # option was read as a flag whenever it looked like one. `--system-prompt
  # --continue -p x` hands Claude Code the literal text `--continue` as the
  # prompt -- a fresh session -- but the scan saw a resume and suppressed the
  # profile's model; `--system-prompt --resume=<id> -p x` went further and
  # carried a model out of a session this launch never resumes. Which options
  # take a value now comes from the installed binary's own Options section,
  # the same source the subcommand verbs already use. (#334)
  local test_home transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_claude_help
  # Routed, with a default: a phantom resume must not carry a transcript model
  # over the profile's own.
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid' 'model=gw-model'
  transcript="$HOME/.claude/projects/project/phantom-sid.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-message>model</command-message>\\n<command-name>/model</command-name>\\n<command-args>phantom-carried</command-args>"}}' \
    > "$transcript"
  # Not routed, with a default: a phantom --continue must not suppress it.
  printf '%s\n' 'type=sub' 'email=test@example.invalid' 'model=sub-model' \
    > "$HOME/.config/ccmorph/subwith.conf"
  source "$CCMORPH_SOURCE"
  _ccm_setup_dir() { return 0; }
  _ccm_repair_projects_link() { return 0; }

  local rc=0 actual
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a gw --system-prompt --resume=phantom-sid -p x || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MODEL_ENV=gw-model' \
    'a --resume= inside an option value must not carry a transcript model' || rc=1
  assert_not_contains "$actual" 'phantom-carried' \
    'a --resume= inside an option value must not carry a transcript model' || rc=1

  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a subwith --system-prompt --continue -p x || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MODEL_ENV=sub-model' \
    'a --continue inside an option value must not suppress the profile model' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_a_direct_launch_forwards_a_native_value_that_looks_like_a_launcher_flag() {
  # The launcher's own argv scan -- the one a direct `_ccm_launch -a <profile>
  # <args>` call runs, the agent-dispatch entry point -- matched one token at a
  # time, so the VALUE of a native option was claimed as a launcher flag
  # whenever it looked like one. `--system-prompt --profile other` hands Claude
  # Code the literal text `--profile` as the prompt, but the scan saw a second
  # profile and refused the launch; `--system-prompt -i` lost its token to the
  # interactive flag and the picker aborted the launch, prompt and all. The scan
  # now steps over a native option and the value it owns with the same walker
  # the model loop uses (#334), so a launcher flag is claimed only where it is
  # not an owned value. (#405)
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_claude_help
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid' 'model=gw-model'
  source "$CCMORPH_SOURCE"
  _ccm_setup_dir() { return 0; }
  _ccm_repair_projects_link() { return 0; }

  # The three spellings the issue reports: a --profile and an -a value are read
  # as this launcher's own profile flag, an -i value as its interactive flag.
  local rc=0 argv actual
  local tab=$'\t'
  for argv in '--profile other' '-a other' '-i'; do
    : > "$CCMORPH_TEST_LOG"
    _ccm_launch -a gw --system-prompt ${=argv} -p x || rc=1
    actual="$(<"$CCMORPH_TEST_LOG")"
    assert_contains "$actual" "ARG${tab}--system-prompt${tab}${argv// /$tab}${tab}-p${tab}x"$'\nSUBAGENT' \
      "'$argv' is the system prompt's value and must reach claude as prompt text, not as a launcher flag" || rc=1
  done

  # The scan's `--` separator is not an option value: it must still end the
  # scan and forward the tokens after it verbatim, not be stepped over into
  # the very flags it separates (#405).
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a gw --system-prompt -- --profile other -p x || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" "ARG${tab}--system-prompt${tab}--${tab}--profile${tab}other${tab}-p${tab}x"$'\nSUBAGENT' \
    'a separator and the tokens after it must forward verbatim' || rc=1

  # The same launch under a caller's KSH_ARRAYS: _ccm_launch turns that option
  # off for its own scan -- the walker's token pair is a positional slice,
  # which KSH_ARRAYS would misread, and a misread pair claims the value again
  # -- so a flag-shaped value stays a value there too. The arity table is
  # cached from the launches above, so the subshell steps the same way.
  : > "$CCMORPH_TEST_LOG"
  ( setopt ksharrays
    _ccm_launch -a gw --system-prompt --profile other -p x >/dev/null 2>&1 ) || {
    print -u2 -- "the launch must start under a caller's KSH_ARRAYS"
    rc=1
  }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" "ARG${tab}--system-prompt${tab}--profile${tab}other${tab}-p${tab}x"$'\nSUBAGENT' \
    'a flag-shaped value must remain the prompt text under KSH_ARRAYS' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_direct_resume_reports_a_rejected_carried_model() {
  local test_home session_id='resume-rejected' transcript out
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/model</command-name>\\n<command-args>absent-chat</command-args>"}}' \
    > "$transcript"

  out=$(_ccm_launch -a proxy --resume "$session_id" 2>&1) || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")" rc=0
  assert_contains "$out" 'absent-chat is not served by proxy' \
    'a direct resume must report when it changes the carried model' || rc=1
  assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
    'a rejected carried model must fall back to the profile default' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_mapped_model_resume_and_hop_keep_servable_identity() {
  local test_home session_id='mapped-resume' relay_id='1234.mapped' transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=apikey' 'base_url=http://mapped.invalid' 'api_key=test-key' \
    'model=other-default' \
    'model_map={"claude-sonnet-5-5":"gpt-6.1-sol","plain-alias":"gpt-6.1-sol","bad-alias":"unavailable"}' \
    > "$HOME/.config/ccmorph/mapped.conf"
  write_curl_mock 'for a do case "$a" in *http_code*) print 200; exit 0 ;; esac; done
print -r -- '\''{"has_more":false,"data":[{"id":"gpt-6.1-sol"},{"id":"other-default"}]}'\'''
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  export CLAUDE_CODE_SESSION_ID="$session_id" CCMORPH_RELAY_ID="$relay_id"
  local rc=0 requested out actual expected
  for requested in claude-sonnet-5-5 'plain-alias[1m]' bad-alias unmapped-alias; do
    printf '%s\n' \
      "{\"type\":\"assistant\",\"isSidechain\":false,\"effort\":\"high\",\"message\":{\"role\":\"assistant\",\"model\":\"$requested\",\"content\":[]}}" \
      > "$transcript"
    : > "$CCMORPH_TEST_LOG"
    out=$(_ccm_launch -a mapped --resume "$session_id" 2>&1) || rc=1
    actual="$(<"$CCMORPH_TEST_LOG")"
    case "$requested" in
      claude-sonnet-5-5|'plain-alias[1m]')
        assert_contains "$actual" "MODEL_ENV=$requested" \
          "resume must retain the mapped identity $requested, not the profile default" || rc=1
        assert_not_contains "$out" 'is not served by mapped' \
          "a served mapped target must not produce a false resume rejection for $requested" || rc=1
        expected=$'profile=mapped\nsession=mapped-resume\n'"model=$requested"$'\neffort=high'
        ;;
      *)
        assert_contains "$actual" 'MODEL_ENV=other-default' \
          "resume must retain the fallback for an unavailable target or unmapped id: $requested" || rc=1
        assert_contains "$out" "$requested is not served by mapped" \
          "resume must still diagnose an unavailable target or unmapped id: $requested" || rc=1
        expected=$'profile=mapped\nsession=mapped-resume\neffort=high'
        ;;
    esac
    out=$(ccmorph hop mapped 2>&1) || rc=1
    actual="$(<"$HOME/.config/ccmorph/.reswitch.$relay_id")"
    if [[ "$actual" != "$expected" ]]; then
      print -u2 -- "hop must validate the target but preserve requested identity $requested; actual: ${(qqq)actual}"
      rc=1
    fi
    case "$requested" in
      claude-sonnet-5-5|'plain-alias[1m]')
        assert_not_contains "$out" 'is not served by mapped' 'a healthy mapped hop must not be rejected' || rc=1 ;;
      *) assert_contains "$out" "$requested is not served by mapped" 'an unavailable mapped hop must retain its diagnosis' || rc=1 ;;
    esac
  done

  : > "$CCMORPH_TEST_LOG"
  out=$(_ccm_launch -a mapped --resume "$session_id" --model claude-sonnet-5-5 --effort low 2>&1) || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" $'ARG\t--resume\tmapped-resume\t--model\tclaude-sonnet-5-5\t--effort\tlow' \
    'explicit model and effort flags must remain untouched during mapped resume' || rc=1
  assert_not_contains "$out" 'is not served by mapped' 'explicit model must bypass carried-model rejection' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_mapped_model_known_matches_shim_scope_and_suffix() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_curl_mock 'print -r -- '\''{"data":[{"id":"served-target"},{"id":"literal-id"},{"id":"embed-only","capabilities":{"chat_completion":false}},{"id":"listed-alias"}]}'\'''
  source "$CCMORPH_SOURCE"
  local rc=0 base requested known expected
  for base in https://api.anthropic.com https://api.anthropic.com/ http://mapped.invalid; do
    printf '%s\n' 'type=apikey' "base_url=$base" 'api_key=test-key' \
      'model_map={"mapped-alias":"served-target","literal-id":"absent-target","window-target":"served-target[1m]","nonchat-alias":"embed-only","listed-alias":"absent-target","chain-alias":"mapped-alias"}' \
      > "$HOME/.config/ccmorph/mapped.conf"
    for requested in mapped-alias 'mapped-alias[1M]' 'mapped-alias[1m]' window-target literal-id 'literal-id[1M]' nonchat-alias listed-alias chain-alias unmapped-alias; do
      if [[ "$base" == http://mapped.invalid ]]; then
        case "$requested" in mapped-alias|'mapped-alias[1M]'|'mapped-alias[1m]'|window-target) expected=0 ;; *) expected=1 ;; esac
      else
        case "$requested" in literal-id|'literal-id[1M]'|listed-alias) expected=0 ;; *) expected=1 ;; esac
      fi
      _ccm_model_known mapped "$requested"
      known=$?
      (( known == expected )) || {
        print -u2 -- "predicate must match active shim mapping for $base $requested: expected $expected, got $known"
        rc=1
      }
    done
  done
  rm -rf "$test_home"
  return $rc
}

test_mapped_model_known_preserves_catalogue_uncertainty() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=apikey' 'base_url=http://mapped.invalid' 'api_key=test-key' \
    'model_map={"mapped-alias":"absent-target"}' > "$HOME/.config/ccmorph/mapped.conf"
  write_curl_mock 'print -r -- "$(<"$HOME/catalogue.json")"'
  source "$CCMORPH_SOURCE"
  local rc=0 payload requested known
  for payload in \
    '{"has_more":true,"data":[{"id":"other-model"}]}' \
    '{"data":[{"id":[]},{"id":"other-model"}]}'; do
    print -r -- "$payload" > "$HOME/catalogue.json"
    for requested in mapped-alias unmapped-alias; do
      _ccm_model_known mapped "$requested"
      known=$?
      (( known == 2 )) || {
        print -u2 -- "an uncertain catalogue must preserve $requested, got status $known for $payload"
        rc=1
      }
    done
  done
  write_curl_mock 'exit 22'
  _ccm_model_known mapped mapped-alias
  known=$?
  (( known == 2 )) || { print -u2 -- "a failed endpoint must preserve a mapped model, got $known"; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_model_known_strips_only_the_terminal_window_suffix() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for a do case "$a" in *http_code*) echo 200; exit 0 ;; esac; done' \
    'printf '\''{"data":[{"id":"vendor[beta]"}]}\n'\''' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    > "$HOME/.config/ccmorph/brackets.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_model_known brackets 'vendor[beta]' || {
    print -u2 -- 'a bracketed endpoint model id must be compared intact'
    rc=1
  }
  _ccm_model_known brackets 'vendor[beta][1m]' || {
    print -u2 -- 'only a terminal [1m] window suffix may be removed before comparison'
    rc=1
  }
  _ccm_model_known brackets 'vendor[beta][1M]' || {
    print -u2 -- 'the terminal window suffix must be matched case-insensitively'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_hop_relays_model_and_effort() {
  local test_home
  local relay_id='1234.test'
  local relay_file
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  export CLAUDE_CODE_SESSION_ID='relay-session'
  export CCMORPH_RELAY_ID="$relay_id"
  mkdir -p "$HOME/.claude/projects/project"
  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"high","message":{"role":"assistant","model":"source-model-xhigh","content":[]}}' \
    > "$HOME/.claude/projects/project/relay-session.jsonl"

  local hop_output
  local NO_COLOR=1
  source "$REPO_ROOT/lib/ccmorph-ui.zsh"
  hop_output=$(ccmorph hop proxy) || {
    rm -rf "$test_home"
    return 1
  }

  relay_file="$HOME/.config/ccmorph/.reswitch.$relay_id"
  local actual="$(<"$relay_file")"
  local expected=$'profile=proxy\nsession=relay-session\nmodel=source-model-xhigh\neffort=high'
  local rc=0
  [[ "$hop_output" == $'Switch queued → proxy\nExit now (Ctrl-C or /exit) to complete the switch.' ]] || {
    print -u2 -- "hop must keep its queue and exit instructions unchanged: ${(qqq)hop_output}"
    rc=1
  }
  if [[ "$actual" != "$expected" ]]; then
    print -u2 -- \
      "relay must carry the current model and effort\n  expected: ${(qqq)expected}\n  actual: ${(qqq)actual}"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_hop_falls_back_when_the_target_cannot_serve_the_model() {
  # A gateway model hopped into a Claude profile is a name Anthropic has never
  # heard of. The field is dropped so the target launches on its own default; the
  # claude- id in the second half proves the guard is a test, not a blanket.
  local test_home
  local relay_id='1234.unserved'
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  printf '%s\n' 'type=sub' 'email=a@b.c' > "$HOME/.config/ccmorph/subacct.conf"

  export CLAUDE_CODE_SESSION_ID='unserved-session'
  export CCMORPH_RELAY_ID="$relay_id"
  mkdir -p "$HOME/.claude/projects/project"

  local rc=0 actual
  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"high","message":{"role":"assistant","model":"gpt-5.6-sol","content":[]}}' \
    > "$HOME/.claude/projects/project/unserved-session.jsonl"
  ccmorph hop subacct >/dev/null || rc=1
  actual="$(<"$HOME/.config/ccmorph/.reswitch.$relay_id")"
  if [[ "$actual" != $'profile=subacct\nsession=unserved-session\neffort=high' ]]; then
    print -u2 -- \
      "a model the target cannot serve must be dropped\n  actual: ${(qqq)actual}"
    rc=1
  fi

  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"high","message":{"role":"assistant","model":"claude-opus-5","content":[]}}' \
    > "$HOME/.claude/projects/project/unserved-session.jsonl"
  ccmorph hop subacct >/dev/null || rc=1
  actual="$(<"$HOME/.config/ccmorph/.reswitch.$relay_id")"
  if [[ "$actual" != $'profile=subacct\nsession=unserved-session\nmodel=claude-opus-5\neffort=high' ]]; then
    print -u2 -- \
      "a claude- model must still be carried to a Claude profile\n  actual: ${(qqq)actual}"
    rc=1
  fi

  rm -rf "$test_home"
  return $rc
}

test_hop_asks_the_gateway_which_models_it_serves() {
  # The endpoint is the only authority for a routed profile: a model in its listing
  # is carried, one absent from it is not, and nothing here knows a model by name.
  local test_home
  local relay_id='1234.gateway'
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  fake_gateway

  export CLAUDE_CODE_SESSION_ID='gateway-session'
  export CCMORPH_RELAY_ID="$relay_id"
  mkdir -p "$HOME/.claude/projects/project"

  local rc=0 actual
  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"high","message":{"role":"assistant","model":"alpha-chat","content":[]}}' \
    > "$HOME/.claude/projects/project/gateway-session.jsonl"
  ccmorph hop proxy >/dev/null || rc=1
  actual="$(<"$HOME/.config/ccmorph/.reswitch.$relay_id")"
  if [[ "$actual" != $'profile=proxy\nsession=gateway-session\nmodel=alpha-chat\neffort=high' ]]; then
    print -u2 -- \
      "a model the gateway lists must be carried\n  actual: ${(qqq)actual}"
    rc=1
  fi

  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"high","message":{"role":"assistant","model":"absent-chat","content":[]}}' \
    > "$HOME/.claude/projects/project/gateway-session.jsonl"
  ccmorph hop proxy >/dev/null || rc=1
  actual="$(<"$HOME/.config/ccmorph/.reswitch.$relay_id")"
  if [[ "$actual" != $'profile=proxy\nsession=gateway-session\neffort=high' ]]; then
    print -u2 -- \
      "a model absent from the listing must be dropped\n  actual: ${(qqq)actual}"
    rc=1
  fi

  rm -rf "$test_home"
  return $rc
}

test_gateway_catalogue_uses_route_limit_and_pagination_uncertainty() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'printf "PROXY=%s\\n" "$HTTPS_PROXY" >> "$HOME/catalogue.request"' \
    'for a do printf "ARG=%s\\n" "$a" >> "$HOME/catalogue.request"; done' \
    '[ "${1-}" = -K ] && sed "s/^header = \"/ARG=/; s/\"\$//" >> "$HOME/catalogue.request"' \
    'printf '\''{"has_more":true,"data":[{"id":"alpha-chat"}]}\n'\''' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  source "$CCMORPH_SOURCE"

  local listing rc=0 known
  listing=$(HTTPS_PROXY='http://127.0.0.1:47999' _ccm_gw_models proxy) || {
    rm -rf "$test_home"
    return 1
  }
  assert_contains "$listing" 'alpha-chat' \
    'the first catalogue page must still be usable' || rc=1
  local request="$(<"$HOME/catalogue.request")"
  assert_contains "$request" 'PROXY=http://127.0.0.1:47999' \
    'catalogue requests must inherit the ambient proxy, never a blanked one' || rc=1
  assert_contains "$request" 'ARG=http://proxy.invalid/v1/models?limit=1000' \
    'catalogue requests must ask for the largest supported page' || rc=1
  assert_contains "$request" 'ARG=Authorization: Bearer test-key' \
    'catalogue requests must authenticate like adapter and launch probes' || rc=1

  _ccm_model_known proxy absent-chat
  known=$?
  (( known == 2 )) || {
    print -u2 -- "an absent model on a partial catalogue is unknown, got status $known"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_gateway_http_error_is_uncertainty_not_model_absence() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for a do' \
    '  case "$a" in' \
    '    *http_code*) printf 200; exit 0 ;;' \
    '    -f|--fail|--fail-with-body) fail=1 ;;' \
    '  esac' \
    'done' \
    'printf '\''{"error":{"message":"rate limited"}}\n'\''' \
    '[ "${fail:-}" = 1 ] && exit 22' \
    'exit 0' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  source "$CCMORPH_SOURCE"

  _ccm_model_known proxy alpha-chat
  local known=$? rc=0
  (( known == 2 )) || {
    print -u2 -- "an HTTP catalogue error must be unknown, got status $known"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_gateway_explicit_non_chat_model_is_not_carryable() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for a do case "$a" in *http_code*) printf 200; exit 0 ;; esac; done' \
    'printf '\''{"data":[{"id":[]},{"id":"image-only","capabilities":{"chat_completion":false}},{"id":"undeclared-chat","capabilities":{"embeddings":true}}]}\n'\''' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  source "$CCMORPH_SOURCE"

  local rc=0 known
  _ccm_model_known proxy image-only
  known=$?
  (( known == 1 )) || {
    print -u2 -- "an explicit chat denial must be definite absence, got status $known"
    rc=1
  }
  _ccm_model_known proxy undeclared-chat
  known=$?
  (( known == 0 )) || {
    print -u2 -- "undeclared chat capability must remain carryable, got status $known"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_hop_without_profile_uses_current_profile() {
  local test_home
  local relay_id='1234.same-profile'
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # The current running model is a stronger fact than a fresh catalogue answer.
  # Make it absent so an argument-free same-profile hop proves it skips rejection.
  fake_gateway
  source "$CCMORPH_SOURCE"

  export CLAUDE_CODE_SESSION_ID='same-profile-session'
  export CCMORPH_RELAY_ID="$relay_id"
  mkdir -p "$HOME/.claude/projects/project"
  printf '%s\n' \
    '{"type":"assistant","isSidechain":false,"effort":"max","message":{"role":"assistant","model":"current-model","content":[]}}' \
    > "$HOME/.claude/projects/project/same-profile-session.jsonl"

  CLAUDE_CONFIG_DIR="$HOME/.config/ccmorph/profiles/proxy" ccmorph hop >/dev/null || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$HOME/.config/ccmorph/.reswitch.$relay_id")"
  local expected=$'profile=proxy\nsession=same-profile-session\nmodel=current-model\neffort=max'
  local rc=0
  if [[ "$actual" != "$expected" ]]; then
    print -u2 -- \
      "an argument-free hop must resume the current profile\n  expected: ${(qqq)expected}\n  actual: ${(qqq)actual}"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_relay_resume_preserves_dangerous_permission_mode() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://target.invalid' \
    'api_key=target-key' \
    'model=target-model' \
    > "$HOME/.config/ccmorph/target.conf"
  printf '%s\n' \
    '#!/bin/sh' \
    'formatted=0; code=200' \
    'for arg do' \
    '  [ "$arg" = "-w" ] && formatted=1' \
    '  case "$arg" in *target.invalid/v1/models*) code=000 ;; esac' \
    'done' \
    '[ "$formatted" = 1 ] || exit 1' \
    'printf "%s" "$code"' \
    '[ "$code" != 000 ]' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' \
    '#!/bin/sh' \
    'printf "ARG" >> "$CCMORPH_TEST_LOG"' \
    'for arg do printf "\\t%s" "$arg" >> "$CCMORPH_TEST_LOG"; done' \
    'printf "\\n" >> "$CCMORPH_TEST_LOG"' \
    'if [ ! -f "$HOME/relay-triggered" ]; then' \
    '  : > "$HOME/relay-triggered"' \
    '  printf "profile=target\\nsession=relay-session\\nmodel=source-model\\neffort=high\\n" > "$HOME/.config/ccmorph/.reswitch.$CCMORPH_RELAY_ID"' \
    'fi' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"
  cp "$CCMORPH_SOURCE" "$HOME/.config/ccmorph/lib/ccmorph.sh"
  cp "$REPO_ROOT"/lib/ccmorph-*.zsh "$HOME/.config/ccmorph/lib/"
  printf '%s\n' ': > "$HOME/ccmorph-reloaded"' > "$HOME/.config/ccmorph/local.sh"
  printf '%s\n' ': > "$HOME/startup-sourced"' 'exit 99' > "$HOME/.zshrc"
  cp "$HOME/.zshrc" "$HOME/startup-before"

  local relay_output
  relay_output=$(ZDOTDIR="$HOME" ccmorph run proxy --dangerously-skip-permissions) || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  [[ -f "$HOME/startup-sourced" ]] && {
    print -u2 -- 'relay resume must not source the user startup file'
    rc=1
  }
  cmp -s "$HOME/.zshrc" "$HOME/startup-before" || {
    print -u2 -- 'relay resume must leave the user startup file untouched'
    rc=1
  }
  [[ -f "$HOME/ccmorph-reloaded" ]] || {
    print -u2 -- 'relay resume must reload ccmorph local opt-in state'
    rc=1
  }
  local expected_output=$'\nccmorph: resuming as target...\n\nSwitching accounts or backends may rebuild the conversation cache on your first message and charge for the history again.'
  [[ "$relay_output" == "$expected_output" ]] || {
    print -u2 -- \
      "relay resume must print only its structured status\n  expected: ${(qqq)expected_output}\n  actual: ${(qqq)relay_output}"
    rc=1
  }
  assert_contains \
    "$actual" \
    $'ARG\t--dangerously-skip-permissions\t--model\tsource-model\t--effort\thigh\t--resume\trelay-session' \
    'relay resume must preserve permission mode, model, and effort' || rc=1
  rm -rf "$test_home"
  return $rc
}

check_relay_cache_notice_absent() {
  local NO_COLOR=1
  local target="$1" expected_output="$2" expected_rc="$3"
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Seeding atomically replaces the conf: finish it before creating the alias.
  source "$CCMORPH_SOURCE"
  _ccm_config_store seed-model-defaults proxy || { rm -rf "$test_home"; return 1; }
  # Use a regular same-file alias when the filesystem distinguishes case.
  if [[ "$target" == PROXY && ! -e "$HOME/.config/ccmorph/PROXY.conf" ]]; then
    ln "$HOME/.config/ccmorph/proxy.conf" "$HOME/.config/ccmorph/PROXY.conf"
  fi
  local preflight="${4:-}"
  case "$preflight" in
    settings)
      cp "$HOME/.config/ccmorph/proxy.conf" "$HOME/.config/ccmorph/$target.conf"
      mkdir -p "$HOME/.config/ccmorph/profiles/$target"
      print -r -- '{"env":[]}' > "$HOME/.config/ccmorph/profiles/$target/settings.profile.json"
      ;;
    private)
      cp "$HOME/.config/ccmorph/proxy.conf" "$HOME/.config/ccmorph/$target.conf"
      print -r -- 'private=1' >> "$HOME/.config/ccmorph/$target.conf"
      local real_mktemp="$(whence -p mktemp)"
      printf '%s\n' '#!/bin/sh' \
        'case "$*" in *ccmorph-private.*) printf "private view creation refused\n" >&2; exit 1 ;; esac' \
        "exec ${(qqq)real_mktemp} \"\$@\"" > "$HOME/bin/mktemp"
      chmod +x "$HOME/bin/mktemp"
      ;;
    provider)
      printf '%s\n' 'type=provider' 'provider=codex' > "$HOME/.config/ccmorph/$target.conf"
      printf '%s\n' '#!/bin/sh' 'exit 1' > "$HOME/bin/cliproxyapi"
      chmod +x "$HOME/bin/cliproxyapi"
      ;;
  esac
  export CCMORPH_TEST_RELAY_TARGET="$target"
  printf '%s\n' \
    '#!/bin/sh' \
    'printf "STARTED\n" >> "$CCMORPH_TEST_LOG"' \
    'if [ ! -f "$HOME/relay-triggered" ]; then' \
    '  : > "$HOME/relay-triggered"' \
    '  printf "profile=%s\nsession=relay-session\n" "$CCMORPH_TEST_RELAY_TARGET" > "$HOME/.config/ccmorph/.reswitch.$CCMORPH_RELAY_ID"' \
    'fi' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"

  local output actual_rc=0 rc=0
  output=$(ccmorph run proxy 2>&1) || actual_rc=$?
  if [[ -n "$preflight" ]]; then
    assert_contains "$output" "$expected_output" 'the target must fail its preflight' || rc=1
    [[ "$actual_rc" == "$expected_rc" &&
       "$output" != *'Switching accounts or backends may rebuild the conversation cache'* &&
       "$(<"$CCMORPH_TEST_LOG")" == STARTED ]] || {
      print -u2 -- "failed $preflight preflight must not start the target or print the cache notice (issue 226)\n  actual: ${(qqq)output}, status $actual_rc"
      rc=1
    }
  else
    [[ "$output" == "$expected_output" && "$actual_rc" == "$expected_rc" ]] || {
      print -u2 -- "relay $target must omit the cache notice (issue 226)\n  expected: ${(qqq)expected_output}, status $expected_rc\n  actual: ${(qqq)output}, status $actual_rc"
      rc=1
    }
  fi
  rm -rf "$test_home"
  return $rc
}

test_relay_same_profile_omits_cache_notice_issue_226() {
  check_relay_cache_notice_absent proxy $'\nccmorph: resuming as proxy...' 0
}

test_relay_same_file_profile_omits_cache_notice_issue_226() {
  check_relay_cache_notice_absent PROXY $'\nccmorph: resuming as PROXY...' 0
}

test_relay_failed_switch_omits_cache_notice_issue_226() {
  check_relay_cache_notice_absent missing 'ccmorph: invalid relay signal' 1
}

test_relay_settings_preflight_omits_cache_notice_issue_226() {
  check_relay_cache_notice_absent target 'settings.json and its env must be objects' 1 settings
}

test_relay_provider_preflight_omits_cache_notice_issue_226() {
  check_relay_cache_notice_absent target "no auth directory for 'target'" 1 provider
}

test_relay_private_preflight_omits_cache_notice_issue_226() {
  check_relay_cache_notice_absent target 'private view creation refused' 1 private
}

test_prelaunch_runs_configured_step_silently() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  mkdir -p "$HOME/bin" "$HOME/work"
  print -r -- '#!/bin/sh
printf "%s %s %s\n" "$1" "$2" "$CLAUDE_CONFIG_DIR" > "$HOME/ran"; echo noise' > "$HOME/bin/step"
  chmod +x "$HOME/bin/step"
  export CCMORPH_PRELAUNCH="$HOME/bin/step"

  local out
  out=$(cd "$HOME/work" && _ccm_prelaunch demo)
  [[ -z "$out" ]] || {
    print -u2 "prelaunch must stay stdout-silent inside _env, got: $out"
    unset CCMORPH_PRELAUNCH; rm -rf "$test_home"; return 1
  }
  assert_contains "$(<"$HOME/ran")" "prelaunch $HOME/work ${_CCMORPH_PROFILE_DIRS}/demo" \
    'prelaunch must receive the launch cwd and the profile config dir' || {
    unset CCMORPH_PRELAUNCH; rm -rf "$test_home"; return 1
  }
  export CCMORPH_PRELAUNCH="$HOME/bin/missing"
  _ccm_prelaunch demo
  local rc=$?
  (( rc == 0 )) || print -u2 'a missing prelaunch executable must never block a launch'
  unset CCMORPH_PRELAUNCH
  rm -rf "$test_home"
  return $rc
}

test_local_sh_loads_and_its_exports_take_effect() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"

  # The load-time seam: a host's gitignored local.sh sets an opt-in variable
  # before the rest of ccmorph runs. Written BEFORE sourcing so the source
  # itself is what loads it — not the test reaching in afterward.
  printf '%s\n' 'export CCMORPH_PRELAUNCH="/host/rendered/step"' \
    > "$HOME/.config/ccmorph/local.sh"

  unset CCMORPH_PRELAUNCH
  source "$CCMORPH_SOURCE"

  assert_contains "[${CCMORPH_PRELAUNCH:-<unset>}]" "[/host/rendered/step]" \
    'sourcing ccmorph must load local.sh so its opt-in exports take effect'
  local rc=$?
  unset CCMORPH_PRELAUNCH
  rm -rf "$test_home"
  return $rc
}

test_absent_local_sh_is_a_clean_noop() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"

  # Vanilla install: no local.sh. Sourcing must succeed, run to completion, and
  # leave the opt-in unset. Drop the driver first so its presence afterward proves
  # THIS source reached the end (it is defined on the last lines of ccmorph.sh),
  # not a stale definition left by an earlier test in this shell.
  rm -f "$HOME/.config/ccmorph/local.sh"
  unset CCMORPH_PRELAUNCH
  unfunction _ccm_launch_driver 2>/dev/null
  source "$CCMORPH_SOURCE" || {
    print -u2 -- 'sourcing must succeed with no local.sh present'
    rm -rf "$test_home"; return 1
  }

  assert_contains "[${CCMORPH_PRELAUNCH:-<unset>}]" "[<unset>]" \
    'a vanilla install leaves the prelaunch opt-in unset' || {
    rm -rf "$test_home"; return 1
  }
  # typeset -f, not whence -w: the noglob alias shadows the name in whence's precedence
  # order while the function is still defined and still what runs.
  assert_contains \
    "$(typeset -f _ccm_launch_driver >/dev/null && print function || print missing)" \
    "function" \
    'a vanilla source must run to completion, defining the final launcher'
  local rc=$?
  rm -rf "$test_home"
  return $rc
}

test_broken_local_sh_does_not_abort_the_source() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"

  # local.sh is trusted host config, but a parse error in it must not brick the
  # rest of ccmorph: the failed nested `source` (line ~16) must not abort the
  # outer source, so the driver on the LAST lines of ccmorph.sh must still be
  # defined. Drop it first, and run in a subshell with default zsh
  # options (no inherited ERR_EXIT) — the documented guarantee holds under
  # ordinary interactive options, which is what a fresh user runs.
  printf '%s\n' 'this is (not valid zsh' \
    > "$HOME/.config/ccmorph/local.sh"

  unset CCMORPH_PRELAUNCH
  local result
  # -f: no startup files, so no inherited definition or option (e.g. ERR_EXIT) can
  # taint the result — fully hermetic, the source under test is the only thing that
  # can define the driver.
  result=$(zsh -fc "
    CCMORPH_SOURCE=${(q)CCMORPH_SOURCE}
    unfunction _ccm_launch_driver 2>/dev/null
    source \"\$CCMORPH_SOURCE\" 2>/dev/null
    print -r -- \"src_rc=\$?\"
    print -r -- \"launcher=\$(typeset -f _ccm_launch_driver >/dev/null && print function || print missing)\"
  ")

  assert_contains "$result" "src_rc=0" \
    'a parse error in local.sh must not fail the outer source of ccmorph' || {
    rm -rf "$test_home"; return 1
  }
  assert_contains "$result" "launcher=function" \
    'a broken local.sh must not prevent ccmorph from reaching its final launcher'
  local rc=$?
  rm -rf "$test_home"
  return $rc
}

test_shipped_surface_never_names_the_transport_product() {
  # The transport is not merely decoupled — it is GONE, and UNNAMED: no tracked
  # file may carry its product name, its executables, or the seam variables that
  # used to call out to it. The product needle is assembled at runtime so this
  # test file honors its own rule.
  # Same guard as the other repo-wide git scans in this file: a deployed copy
  # without .git has nothing to validate, and a swallowed git failure would
  # read as a silent PASS in the one test whose job is catching reintroduction.
  git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || { print -- "  (skipped: not a git checkout)"; return 0 }
  # Every needle is spliced from fragments, so this file is never its own hit.
  # CHANGELOG.md is exempt: a changelog's job is to record what no longer exists,
  # and a reader who has the removed names in their own local.sh needs to read
  # them there. It is history, not shipped surface.
  local rc=0 needle hits
  local -a needles=(
    'net''bridge'
    'CCMORPH_''TRANSPORT_RUNNER'
    'CCMORPH_''TRANSPORT_PROXY'
    'ccmorph-''via-runner'
    'ccmorph-''connect-proxy'
  )
  for needle in "${needles[@]}"; do
    hits=$(git -C "$REPO_ROOT" grep -il -- "$needle" -- . ':!CHANGELOG.md' 2>/dev/null)
    [[ -z "$hits" ]] && continue
    print -u2 -- "FAIL: tracked files still name '$needle':\n$hits"
    rc=1
  done
  # And no tracked FILE may be one of the transport's own.
  hits=$(git -C "$REPO_ROOT" ls-files -- "${needles[1]}*" "${needles[4]}" "${needles[5]}" 2>/dev/null)
  [[ -z "$hits" ]] || {
    print -u2 -- "FAIL: the transport's files are still tracked:\n$hits"
    rc=1
  }
  return $rc
}


# A published tool is read by someone who cannot ask the author anything, so an
# instruction they cannot act on is a dead end. But the fix is NOT to paste this
# repository's URL in: `test_source_carries_no_host_specific_defaults` forbids an
# org or host identifier in any shipped file, and a fork's URL would be wrong for
# every other fork anyway.
#
# So the rule is narrow: prose a READER follows must not contain a slot only the
# author could fill.
# No model's context window may be written in the source. Every window is a
# probed fact that lives in the registry (`ccmorph probe` writes it), keyed
# per model and per route; a number typed here would be a second, silently
# diverging source of truth for exactly the fact this tool exists to get right --
# and a wrong one is invisible until a session limit a wall nobody predicted.
#
# The two schema bounds are named constants, declared once with their provenance
# (Claude Code's own .int().min(1e5).max(1e6) on autoCompactWindow) and matched by
# name, not value. Comments are exempt: prose explaining a probed number is how
# the reasoning stays auditable.
# An apostrophe inside an embedded `python3 -c '\''...'\''` program closes the
# single-quoted zsh string early; the remainder is parsed as shell, and the
# enclosing function silently stops being defined. That has broken this file three
# separate times, and the symptom is always far from the cause: an advisory
# disappears, or a function is "not found", while `zsh -n` may still call the file
# valid. The only safe form inside those programs is the escaped `'\''`.
test_no_embedded_program_carries_a_bare_apostrophe() {
  local rc=0 hits
  # Per file, never the concatenation: a program's closing quote must be sought
  # in the file that opened it. Joined, a one-line `python3 -c '…'` — which has
  # no `\n'` after it — would take the next FILE's closer as its own and bury
  # every line up to it inside the program, reporting them all as bare
  # apostrophes.
  hits=$(command python3 - "${CCMORPH_LIBRARY[@]}" <<'PY'
import sys

needle = "python3 -c " + chr(39)
out = []
for path in sys.argv[1:]:
    src = open(path, encoding="utf-8").read()
    name = path.rsplit("/", 1)[-1]
    i = 0
    while True:
        k = src.find(needle, i)
        if k == -1:
            break
        end = src.find(chr(10) + chr(39), k)
        if end == -1:
            break
        base = src[:k].count(chr(10)) + 1
        body = src[k + len(needle):end]
        for n, line in enumerate(body.split(chr(10))):
            # The escaped form is fine; a bare apostrophe is the bug.
            if chr(39) in line.replace(chr(39) + chr(92) + chr(39) + chr(39), ""):
                out.append(f"{name}:{base + n}: {line.strip()[:100]}")
        i = end
print(chr(10).join(out))
PY
) || hits=""
  if [[ -n "$hits" ]]; then
    print -u2 -- 'FAIL: an embedded python program contains a bare apostrophe, which closes'
    print -u2 -- 'its single-quoted string early and drops the enclosing function:'
    print -u2 -- "$hits"
    rc=1
  fi
  return $rc
}

test_no_context_window_is_written_into_the_source() {
  local rc=0 f hits
  # Any 6+ digit run in executable code, minus the two named schema bounds and
  # minus the log-rotation size. Comments and blank lines are stripped first.
  # One file at a time, never the concatenation: a hit is reported against the
  # file that holds it and carries that file's own line number. Joined, a hit in
  # a module would be reported at an offset that exists in no file — the same
  # reason the apostrophe scan above reads per file.
  for f in "${CCMORPH_LIBRARY[@]}"; do
    hits=$(sed -e 's/#.*$//' "$f" \
           | grep -nE '\b[0-9]{6,}\b' \
           | grep -vE '_CCMORPH_(ACW_(MIN|MAX)|LOG_MAX)=' \
           || true)
    [[ -z "$hits" ]] && continue
    print -u2 -- "FAIL: a context window is written into ${f:t}; probe it into the registry instead:"
    print -u2 -- "$hits"
    rc=1
  done
  return $rc
}

test_no_reader_instruction_carries_an_authors_placeholder() {
  local rc=0 f hits
  for f in README.md install.sh; do
    [[ -f "$REPO_ROOT/$f" ]] || continue
    # The shapes a reader cannot act on. Needles are assembled so this test file
    # does not trip its own rule.
    hits=$(grep -nE "(OWN""ER|<[a-z-]*repo-url>|YOUR""-|REPLA""CE_WITH)" \
           "$REPO_ROOT/$f" || true)
    [[ -z "$hits" ]] && continue
    print -u2 -- "FAIL: $f asks the reader to fill in something only the author knows:"
    print -u2 -- "$hits"
    rc=1
  done
  return $rc
}

test_onboarding_documents_no_wiring_step_at_all() {
  # The first command a packaged install documents must be one that EXISTS at that
  # moment. This used to fail in the worst possible way: the tool shipped as zsh
  # FUNCTIONS, so the docs told a fresh user to run `ccmorph init` -- circular,
  # because it needs the thing it sets up. A director hit exactly that (brew
  # install, then "command not found: ccmorph") and read a working package as
  # broken. The fix was not better wording for the wiring step; it was removing the
  # wiring step, so this guard forbids the whole class.
  local rc=0 f hits
  for f in README.md; do
    [[ -f "$REPO_ROOT/$f" ]] || continue
    # Needles assembled from fragments so this test does not match its own source.
    hits=$(grep -nE "ccmorph"" init|ccmorph""-init|exec ""zsh|source .*ccmorph\\.sh" \
             "$REPO_ROOT/$f" || true)
    [[ -z "$hits" ]] && continue
    print -u2 -- "FAIL: $f documents a shell-wiring step; ccmorph is one executable and needs none:"
    print -u2 -- "$hits"
    rc=1
  done
  return $rc
}





test_adapter_spawn_never_inherits_a_tty() {
  # The 610137d wedge: the adapter holding the terminal on fd0 froze the TUI.
  # The spawn redirects fd0 from /dev/null explicitly; no_monitor alone was the
  # previous, emergent guard — one setopt edit away from silently recurring.
  # Counted, not merely found: there was briefly a proxied and a direct spawn
  # line, and only one of them carried the redirect.
  local source_text="$(cat -- "${CCMORPH_LIBRARY[@]}")"
  local rc=0 spawns redirected
  spawns=$(print -r -- "$source_text" | grep -c -- '--parent-pid \$\$')
  redirected=$(print -r -- "$source_text" | grep -c -- '--parent-pid \$\$ </dev/null')
  [[ "$spawns" == "$redirected" && "$spawns" -ge 1 ]] || {
    print -u2 -- "FAIL: every adapter spawn must redirect fd0 from /dev/null ($redirected of $spawns)"
    rc=1
  }
  return $rc
}

test_a_failed_migration_is_loud() {
  local test_home rc=0 err
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  print 'legacy=100000' > "$HOME/.config/ccmorph/model-registry"
  printf '%s\n' '#!/bin/sh' 'exit 1' > "$HOME/.config/ccmorph/libexec/model-window"
  err=$(_ccm_migrate_state 2>&1) && rc=1
  assert_contains "$err" 'migration incomplete' 'failed migration must preserve legacy context' || rc=1
  [[ -f "$HOME/.config/ccmorph/model-registry" ]] || rc=1
  rm -rf "$test_home"
  return $rc
}


test_installers_are_portable_and_leak_nothing() {
  # 1. `#!/bin/zsh` assumes a path that is not universal: zsh lives at
  # /usr/bin/zsh on several Linux distributions, where the installer would fail
  # with "command not found" for an interpreter the machine has installed.
  # The `ccmorph` launcher is installed executable and run DIRECTLY (install.sh copies it to $BIN_DIR and chmod +x's it), so its shebang is
  # load-bearing the same way the installer's is.
  local rc=0 f
  for f in "$REPO_ROOT/install.sh" "$REPO_ROOT/bin/ccmorph"; do
    [[ -r "$f" ]] || continue
    local shebang
    shebang=$(head -1 "$f")
    [[ "$shebang" == '#!/usr/bin/env zsh' ]] || {
      print -u2 -- "FAIL: ${f:t} must find zsh via env, got: $shebang"
      rc=1
    }
  done

  # 1b. Every shipped executable must at least PARSE, and it must be executable —
  # install.sh puts the launcher on PATH, and a non-executable file there is an
  # install that finished with no working command, which is the exact failure this
  # whole design exists to prevent.
  for f in "$REPO_ROOT/bin/ccmorph"; do
    [[ -r "$f" ]] || continue
    zsh -n "$f" 2>/dev/null || {
      print -u2 -- "FAIL: ${f:t} is not valid zsh"
      rc=1
    }
    [[ -x "$f" ]] || {
      print -u2 -- "FAIL: ${f:t} must be executable — install.sh puts it on PATH"
      rc=1
    }
  done

  # 2. The rc rewrite must clean up its temp files on EVERY exit path. cp -p and
  # mv are unguarded under `set -e`, so a read-only rc directory aborted the
  # script between the mktemps and the final rm, littering temps beside the rc.
  local d
  d=$(mktemp -d)
  printf '%s\n' 'export KEEP_BEFORE=1' '# >>> ccmorph >>>' 'OLD' \
    '# <<< ccmorph <<<' 'export KEEP_AFTER=2' > "$d/.zshrc"
  chmod 500 "$d"
  env -i HOME="$d" ZDOTDIR="$d" PATH=/usr/bin:/bin \
    zsh "$REPO_ROOT/install.sh" >/dev/null 2>&1
  chmod 700 "$d"
  local leaked
  leaked=$(ls -a "$d" | grep -c 'ccmorph-\(repl\|tmp\)')
  (( leaked == 0 )) || {
    print -u2 -- "FAIL: an aborted rc rewrite leaked $leaked temp file(s)"
    rc=1
  }
  grep -q KEEP_AFTER "$d/.zshrc" || {
    print -u2 -- 'FAIL: an aborted rewrite must leave the rc intact'
    rc=1
  }
  rm -rf "$d"
  return $rc
}

test_a_renamed_state_file_migrates_itself() {
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  print 'proxy:bootstrap-model=321000' > "$HOME/.config/ccmorph/model-registry.conf"
  source "$CCMORPH_SOURCE"
  ccmorph list >/dev/null 2>&1 || rc=1
  [[ ! -e "$HOME/.config/ccmorph/model-registry.conf" ]] || rc=1
  [[ -f "$HOME/.config/ccmorph/model-registry.conf.migrated" ]] || rc=1
  local out=$(_ccm_model_window proxy bootstrap-model)
  assert_contains "$out" '321000' 'legacy scoped context must move into its profile' || rc=1
  ccmorph list >/dev/null 2>&1 || rc=1
  rm -rf "$test_home"
  return $rc
}


test_chpwd_hooks_cannot_corrupt_migrated_links() {
  # ccmorph.sh is sourced into interactive shells, where chpwd hooks (ls-on-cd,
  # direnv, prompt integrations) are ordinary. _ccm_physical_dir runs `builtin
  # cd` inside $(...), so a printing hook's output joined the captured path:
  # ln -s then wrote a dangling link, rm -f deleted the original, and the agent
  # definition disappeared from the live profile (#164). cd -q skips the chpwd
  # hook (both the chpwd function and chpwd_functions), keeping the capture
  # clean; a relative link must still migrate to one resolvable line.
  local test_home rc=0 from to target
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  chpwd() { print NOISE }
  from="$HOME/.config/ccmorph/profiles/p1/agents"
  to="$HOME/.claude/agents"
  command mkdir -p "$from/defs" "$from/links"
  print 'agent' > "$from/defs/agent.md"
  command ln -s ../defs/agent.md "$from/links/agent.md"
  _ccm_migrate_projects "$from" "$to" || rc=1
  target=$(command readlink "$to/links/agent.md")
  [[ "$target" != *$'\n'* ]] || {
    print -u2 -- "FAIL: chpwd output joined the migrated link target: ${(qqq)target}"
    rc=1
  }
  [[ -f "$to/links/agent.md" ]] || {
    print -u2 -- 'FAIL: the migrated agent link must resolve to its definition'
    rc=1
  }
  unset -f chpwd
  rm -rf "$test_home"
  return $rc
}


test_a_link_target_ending_in_a_newline_survives_the_migration() {
  # _ccm_migrate_projects captured the link's target with
  # `tgt="$(command readlink "$f")"`, and command substitution strips every
  # trailing newline: a relative target whose own name ends in one was rewritten
  # to the shortened name, ln -s wrote that target and `rm -f` deleted the
  # original, so the definition left the profile dangling. Same class, one line
  # below the capture #267 fixed (#399). The capture now ends in a sentinel, so
  # there is no trailing newline for $(...) to strip, and the consumer drops it.
  # The definition's own name ends in a newline too; since #400 it migrates as well, so the link must resolve.
  local test_home rc=0 from to target
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  from="$HOME/.config/ccmorph/profiles/p1/agents"
  to="$HOME/.claude/agents"
  command mkdir -p "$from"
  print 'agent' > "$from/"$'notes.md\n'
  command ln -s $'notes.md\n' "$from/alias.md"
  _ccm_migrate_projects "$from" "$to" || rc=1
  # Read through the same sentinel: a bare $(command readlink ...) on this side
  # would eat the very newline the link has to keep.
  target=$(command readlink -n "$to/alias.md" && print -rn -- /)
  target="${target%/}"
  [[ "$target" == "$to/"$'notes.md\n' ]] || {
    print -u2 -- "FAIL: a relative target ending in a newline must migrate literally (#399): ${(qqq)target}"
    rc=1
  }
  [[ -f "$to/alias.md" ]] || { print -u2 -- "FAIL: the migrated link must resolve to the migrated definition (#399)"; rc=1; }
  rm -rf "$test_home"
  return $rc
}


test_missing_parent_links_still_migrate() {
  # Since 09eb145 (#151) a relative link whose target's parent does not exist
  # failed both _ccm_physical_dir attempts and the chain ended in `|| continue`:
  # the link survived only in .premigration, then agents/ was replaced by the
  # shared link and the agent vanished from the profile, with no message (#167).
  # The fallback recreates it dangling, as it did before 09eb145. A parent that
  # does resolve must still resolve physically, and a missing parent inside the
  # migrated tree must still remap into the shared store. The fallback must not
  # run `cd`, so a printing chpwd hook stays out of its target too.
  # `:a` collapses `x/..` textually, which is wrong when x is a dangling
  # component the kernel cannot resolve through: the recreation then points at
  # whatever the collapsed path names in the shared store — an unrelated
  # `real.md`, or the migrated link itself — and the profile silently loads the
  # wrong definition (#376).
  local test_home rc=0 from to target up="" nl=$'\n'
  local -a ext_target
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  chpwd() { print NOISE }
  from="$HOME/.config/ccmorph/profiles/p1/agents"
  to="$HOME/.claude/agents"
  command mkdir -p "$from/defs" "$from/deep"
  print 'agent' > "$from/defs/agent.md"
  command ln -s ../nodir/f.md "$from/f.md"
  command ln -s ../defs/agent.md "$from/deep/agent.md"
  command ln -s ../nodir2/x.md "$from/deep/x.md"
  # Two `..` more than the backup path is deep, so the chain overshoots the root
  # whatever depth the scratch home has. `:A` because the physical path can be
  # deeper than the spelled one (/var -> /private/var).
  repeat $(( ${#${(s:/:)${from:A}}} + 3 )); do up+='../'; done
  command ln -s "${up}nodir3/.." "$from/root.md"
  # `out` dangles, so `out/..` resolves nowhere: the kernel refuses the whole
  # path. `real.md` is what `:a` collapses `out/../real.md` onto — a file that
  # already exists in the shared store — and `a.md` collapses onto itself.
  command mkdir -p "$to"
  print 'shared real' > "$to/real.md"
  command ln -s "$test_home/gone" "$from/out"
  command ln -s out/../real.md "$from/x.md"
  command ln -s out/../a.md "$from/a.md"
  # The new fallback is captured by $(...) exactly as _ccm_physical_dir's own
  # output is, so it needs the same trailing `/` sentinel. Without it the capture
  # eats a newline that ends a component's name, and the link lands on whatever
  # the shortened name is — here `nodir`, which the shared store already has
  # (#267).
  command mkdir -p "$to/nodir"
  print 'decoy' > "$to/nodir/nl.md"
  command ln -s $'nodir\n/nl.md' "$from/nl.md"
  # A target that itself ends in a newline names a dir beside the newline-free
  # decoy already in the shared store: the $(readlink) capture ate that
  # newline, so the recreated link loaded the decoy's definition instead of
  # the profile's own (#404). And a file whose own name contains a newline
  # must migrate like any other: the loop's newline-delimited read split it
  # into two paths that do not exist, and it silently stayed in the backup
  # (#404).
  command mkdir -p "$from/real$nl" "$to/real"
  print 'INTENDED' > "$from/real$nl/agent.md"
  print 'DECOY' > "$to/real/agent.md"
  command ln -s "real$nl" "$from/ext"
  print 'we-ird' > "$from/we${nl}ird.md"
  _ccm_migrate_projects "$from" "$to" || rc=1

  [[ -L "$to/f.md" ]] || {
    print -u2 -- 'FAIL: a link whose target parent is missing must still migrate (#167)'
    rc=1
  }
  target=$(command readlink "$to/f.md")
  [[ "$target" == *'/profiles/p1/nodir/f.md' ]] || {
    print -u2 -- "FAIL: the recreated dangling link must keep its textual target: ${(qqq)target}"
    rc=1
  }
  [[ "$target" != *$'\n'* && "$target" != *NOISE* ]] || {
    print -u2 -- "FAIL: chpwd output joined the fallback link target: ${(qqq)target}"
    rc=1
  }
  [[ ! -e "$to/f.md" ]] || {
    print -u2 -- 'FAIL: a link to a missing parent must stay dangling, not point at something'
    rc=1
  }
  [[ "$(command readlink "$to/deep/agent.md")" == "$to/defs/agent.md" ]] || {
    print -u2 -- 'FAIL: a link whose parent resolves must keep its physical resolution'
    rc=1
  }
  [[ -f "$to/deep/agent.md" ]] || {
    print -u2 -- 'FAIL: the resolved migrated link must still resolve'
    rc=1
  }
  [[ "$(command readlink "$to/deep/x.md")" == "$to/nodir2/x.md" ]] || {
    print -u2 -- 'FAIL: a missing parent inside the migrated tree must remap into the shared store'
    rc=1
  }
  # A dangling component mid-path stays in the target, so the link dangles where
  # it always did instead of collapsing onto the shared store (#376). `real.md`
  # is the trap: it exists over there, and pointing at it hands the profile an
  # unrelated agent definition.
  target=$(command readlink "$to/x.md")
  [[ "$target" == "$to/out/../real.md" ]] || {
    print -u2 -- "FAIL: a dangling mid-path component must stay in the target, not collapse onto a shared file (#376): ${(qqq)target}"
    rc=1
  }
  [[ ! -e "$to/x.md" ]] || {
    print -u2 -- "FAIL: the link through the dangling component must dangle, not load the shared real.md (#376): ${(qqq)target}"
    rc=1
  }
  [[ "$(<"$to/real.md")" == 'shared real' ]] || {
    print -u2 -- 'FAIL: the migration must not overwrite an existing file in the shared store'
    rc=1
  }
  target=$(command readlink "$to/a.md")
  [[ "$target" == "$to/out/../a.md" ]] || {
    print -u2 -- "FAIL: the self-pointing shape must keep the dangling component too (#376): ${(qqq)target}"
    rc=1
  }
  [[ ! -e "$to/a.md" ]] || {
    print -u2 -- 'FAIL: a link that collapsed onto itself must dangle instead (#376)'
    rc=1
  }
  target=$(command readlink "$to/nl.md")
  [[ "$target" == "$to/"$'nodir\n'"/nl.md" ]] || {
    print -u2 -- "FAIL: a component whose name ends in a newline must survive the capture (#267): ${(qqq)target}"
    rc=1
  }
  [[ ! -e "$to/nl.md" ]] || {
    print -u2 -- 'FAIL: the newline-ending component must dangle, not land on the shared decoy (#267)'
    rc=1
  }
  [[ "$(<"$to/nodir/nl.md")" == 'decoy' ]] || {
    print -u2 -- 'FAIL: the migration must not overwrite an existing file in the shared store'
    rc=1
  }
  # A `..` chain that overshoots the root clamps at the root, and the prefix
  # after the last component the kernel can resolve keeps going: `nodir3/..`
  # stays in the target, exactly as it did before the migration — the link
  # dangles instead of resolving to `/`. What #267 guards is unchanged:
  # _ccm_physical_dir ends its own output with a `/` sentinel its caller strips,
  # and a fallback that lost it would write an empty target (a link macOS
  # creates and nothing can resolve, a failed `ln` on Linux that leaves the
  # original in the backup), or no link at all.
  target=$(command readlink "$to/root.md")
  [[ -L "$to/root.md" && "$target" == '/nodir3/..' ]] || {
    print -u2 -- "FAIL: an overshooting fallback must keep one root and the unresolved tail: ${(qqq)target}"
    rc=1
  }
  [[ ! -e "$to/root.md" ]] || {
    print -u2 -- "FAIL: an overshooting target must dangle, not resolve to /: ${(qqq)target}"
    rc=1
  }
  # The newline ending a target survives the migration: the link resolves to
  # the profile's own definition, never the newline-free decoy beside it, and
  # a name containing a newline arrives with its file (#404).
  [[ "$(command cat "$to/ext/agent.md")" == 'INTENDED' ]] || {
    print -u2 -- 'FAIL: a link whose target ends in a newline must load its own definition, not the decoy (#404)'
    rc=1
  }
  if zstat -A ext_target +link -- "$to/ext" 2>/dev/null; then
    [[ "${ext_target[1]}" == "$to/real$nl" ]] || {
      print -u2 -- "FAIL: the newline ending a link target must survive the capture (#404): ${(qqq)ext_target[1]}"
      rc=1
    }
  else
    print -u2 -- 'FAIL: the link whose target ends in a newline must migrate (#404)'
    rc=1
  fi
  [[ -f "$to/we${nl}ird.md" && "$(<"$to/we${nl}ird.md")" == 'we-ird' ]] || {
    print -u2 -- 'FAIL: a name containing a newline must migrate with its file (#404)'
    rc=1
  }
  unset -f chpwd
  rm -rf "$test_home"
  return $rc
}


test_newline_names_migrate_whole_and_stay_inside_the_tree() {
  # GH-400: the walk was newline-separated, so a name containing a newline
  # arrived as two entries. The real entry never left .premigration (the agent
  # definition stayed behind while the profile pointed at the shared store),
  # and the remainder reached mv as a relative path — resolved against the
  # caller's cwd, moving an unrelated same-named file into the shared store and
  # deleting it from where it was. The walk is NUL-separated now: a name is one
  # entry however many newlines it contains, and nothing outside the migrated
  # tree is touched. The migration runs from a cwd holding the same name the
  # split would produce, because a relative mv lands there.
  local test_home rc=0 from to cwd leftover
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  from="$HOME/.config/ccmorph/profiles/p1/agents"
  to="$HOME/.claude/agents"
  cwd="$test_home/cwd"
  command mkdir -p "$from/sub" "$cwd"
  print 'newline agent' > "$from/sub/a"$'\n'"b.md"
  print 'ordinary agent' > "$from/plain.md"
  command ln -s ../plain.md "$from/sub/l"$'\n'"k.md"
  print 'unrelated' > "$cwd/b.md"
  ( builtin cd "$cwd" && _ccm_migrate_projects "$from" "$to" ) || rc=1
  [[ "$(command cat "$to/sub/a"$'\n'"b.md")" == 'newline agent' ]] || {
    print -u2 -- 'FAIL: a name containing a newline must migrate as one entry (#400)'
    rc=1
  }
  [[ "$(command readlink "$to/sub/l"$'\n'"k.md")" == "$to/plain.md" ]] || {
    print -u2 -- 'FAIL: a link whose name contains a newline must migrate as one entry (#400)'
    rc=1
  }
  [[ "$(command cat "$to/plain.md")" == 'ordinary agent' ]] || {
    print -u2 -- 'FAIL: ordinary names must still migrate (#400)'
    rc=1
  }
  [[ "$(command cat "$cwd/b.md" 2>/dev/null)" == 'unrelated' ]] || {
    print -u2 -- "FAIL: a same-named file in the caller's cwd must not be moved (#400)"
    rc=1
  }
  [[ ! -e "$to/b.md" ]] || {
    print -u2 -- 'FAIL: nothing from outside the tree may enter the shared store (#400)'
    rc=1
  }
  leftover=$(command find "$from".premigration.* \( -type f -o -type l \) 2>/dev/null)
  [[ -z "$leftover" ]] || {
    print -u2 -- "FAIL: every entry must leave the backup: ${(qqq)leftover}"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}


test_every_state_file_is_gitignored() {
  # The 2026-08-24 data loss: v1.4.0 renamed model-registry.conf to
  # model-registry. The `*.conf` rule had been what kept it out of git, so the new
  # name was ignored by NOTHING — and canon-reconcile, which sweeps untracked
  # files out of the canonical worktree, correctly read it as a stray edit and
  # moved 52 probed windows to a quarantine branch. The .gitignore fix landed
  # nine minutes too late.
  #
  # The lesson is not "remember to update .gitignore": it is that a state file's
  # name and its ignore rule are one fact, and nothing checked they agreed. This
  # asserts the agreement directly — every path ccmorph WRITES into its own
  # directory must be ignored, so a future rename cannot orphan one silently.
  git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 0

  local rc=0 f
  # Machine-local state ccmorph creates in $_CCMORPH_DIR. A new one added here
  # without an ignore rule fails this test rather than the user's data.
  local -a state=(
    model-registry            # probed windows (refresh-model-registry)
    active                    # the default-profile pointer
    local.sh                  # host opt-ins
    ccmorph.log              # runtime log
    example.conf              # any profile
    .reswitch.abc123          # relay handoff
    example.system.md         # per-profile system prompt
    profiles/demo/config.json # a per-profile Claude home, now inside the state dir
    .profile-home-migration.lock  # guards the one-time move of the profile root
    backups/anything.conf     # pre-edit snapshots
    orphaned-profiles/x/y     # profile dir left by a deleted conf
    ccmorph.sh.bak-20260713  # hand-made copy of the runtime, mode 0600
    ccmorph.sh.orig          # a merge/patch leftover of the same
    dist/ccmorph-v1.0.0.tar.gz   # release artifact
    .pi/tasks/session/result.output  # local background-check evidence
    .DS_Store                 # OS dropping, in a dir one `git add -A` from a key
  )
  for f in "${state[@]}"; do
    git -C "$REPO_ROOT" check-ignore -q --no-index "$f" || {
      print -u2 -- "FAIL: '$f' is machine-local state but is not gitignored — canon-reconcile will sweep it"
      rc=1
    }
  done

  # And the inverse: a tracked component must NOT be ignored, or an installer
  # change could silently stop shipping it. The modules are derived from the same
  # glob install.sh enumerates — a name listed here by hand is a name a later
  # module can be forgotten from, which is the drift this whole split is about.
  local -a shipped=(ccmorph claude ccmorph.sh install.sh model-window refresh-model-registry "$REPO_ROOT"/lib/ccmorph-*.zsh(N:t))
  for f in "${shipped[@]}"; do
    if git -C "$REPO_ROOT" check-ignore -q --no-index "$f"; then
      print -u2 -- "FAIL: shipped component '$f' is gitignored"
      rc=1
    fi
  done
  return $rc
}

test_every_module_is_sourced_once_and_defines_its_own_functions() {
  # Now that the module set is derived everywhere else — install.sh enumerates
  # the glob, publish ships it, this harness sources it — nothing is left that
  # would notice a module which stops loading. Three such failures are invisible
  # in the source tree and reach an operator's profile home instead: a module no
  # loader line names (its functions are simply undefined), a loader line naming
  # a file that is gone (sourcing dies part-way through the library), and one
  # function copied into a second module (the later source wins, so the code you
  # read is not the code that runs). Pin all three here, where the source tree is
  # in front of us.
  local rc=0 f m n
  local loader="$CCMORPH_SOURCE" arch="$REPO_ROOT/docs/ARCHITECTURE.md"
  # The library as the harness already defines it: the loader plus every module
  # split out of it.
  local -a files=("${_ccmorph_library[@]}")
  local -a modules=("$REPO_ROOT"/lib/ccmorph-*.zsh(N:t))
  if (( ! ${#modules} )); then
    print -u2 -- 'FAIL: no ccmorph-*.zsh module beside ccmorph.sh — every check here would be vacuous'
    return 1
  fi

  # The live loader lines, read once: leading whitespace is allowed, a comment
  # marker is not, because a commented-out source line leaves the module
  # unloaded — the very failure this check exists to catch.
  local -a sourced=(${(@f)$(grep -hoE '^[[:space:]]*source "\$_CCMORPH_LIB_DIR/[^"]+"' "$loader" |
    sed -E 's|^[[:space:]]*source "\$_CCMORPH_LIB_DIR/||; s|"$||')})
  sourced=(${sourced:#})
  (( ${#sourced} )) || {
    print -u2 -- "FAIL: no live loader source line found in ${loader:t} — every check here would be vacuous"
    rc=1
  }

  # Sourced exactly once: twice redefines every function the module owns, and
  # never means it ships without ever running.
  for m in "${modules[@]}"; do
    n=${#${(M)sourced:#$m}}
    (( n == 1 )) || {
      print -u2 -- "FAIL: ccmorph.sh sources $m $n time(s) — expected exactly once"
      rc=1
    }
  done

  # Everything the loader sources is really beside it. A stale name here is a
  # launch that dies mid-library, in the user's own home.
  for f in "${sourced[@]}"; do
    [[ -e "$REPO_ROOT/lib/$f" ]] || {
      print -u2 -- "FAIL: ccmorph.sh sources _CCMORPH_LIB_DIR/$f, which does not exist beside it"
      rc=1
    }
  done

  # And the module map still names every one of them, or the one document that
  # says what each file is for has silently fallen behind the tree.
  for m in "${modules[@]}"; do
    grep -qF "\`$m\`" "$arch" || {
      print -u2 -- "FAIL: ARCHITECTURE.md does not name $m — the module map is hand-copied again"
      rc=1
    }
  done

  # No top-level function defined in two places: the second source wins, so a
  # copy-paste between modules is a change nobody can see by reading either file.
  # Non-emptiness over the WHOLE library is what keeps the extraction honest — a
  # pattern that stopped matching would let the duplicate check pass on an empty
  # set. It is deliberately not asked of each file: the split has progressed to
  # where ccmorph.sh is a loader and defines nothing itself, and every
  # definition lives in a module, so a per-file form fails on a healthy tree.
  # Both spellings zsh accepts, `name() {` and `function name {` (with or
  # without the parentheses): a definition the pattern cannot see is a
  # duplicate this check would pass over.
  local defre='^(function +[A-Za-z_][A-Za-z0-9_]* *(\(\))? *\{|[A-Za-z_][A-Za-z0-9_]* *\(\) *\{)'
  local -a all defs
  all=("${(@f)$(grep -hEo "$defre" "${files[@]}" |
    sed -E 's/^function +//; s/[[:space:]]*(\(\))?[[:space:]]*\{$//; s/[[:space:]]*\{$//')}")
  all=(${all:#})
  (( ${#all} )) || {
    print -u2 -- 'FAIL: no top-level function definition found across the library — the duplicate check would be blind to all of it'
    rc=1
  }
  defs=("${(@f)$(printf '%s\n' "${all[@]}" | sort | uniq -d)}")
  [[ -z "${defs[1]:-}" ]] || {
    print -u2 -- "FAIL: defined in more than one place: ${(j:, :)defs}"
    rc=1
  }
  return $rc
}

test_runtime_data_never_looks_like_a_profile() {
  # In the ccmorph directory, `*.conf` means exactly one thing: a profile,
  # holding a credential. It is what the profile glob enumerates and what
  # .gitignore excludes. The probed-window registry shipped with that suffix,
  # so every command tried to parse it as an profile and printed
  # "skipping invalid profile 'model-registry'". Guard the CLASS: no runtime data
  # file may claim that suffix, and a data file dropped beside the profiles must
  # not be mistaken for one.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0
  # The behaviour itself: a registry beside the profiles is not one of them.
  printf '%s\n' 'type=sub' 'email=someone@example.com' \
    > "$HOME/.config/ccmorph/real.conf"
  printf '%s\n' 'claude-opus-5=1000000' \
    > "$HOME/.config/ccmorph/model-registry"
  local out
  out=$(ccmorph list 2>&1)
  assert_contains "$out" 'real' 'a genuine profile must still be listed' || rc=1
  assert_not_contains "$out" 'model-registry' \
    'the registry must never appear in, or warn from, the profile list' || rc=1
  # The exact warning this bug produced, not the bare word "invalid" — a seeded
  # test profile legitimately uses a .invalid hostname.
  assert_not_contains "$out" 'invalid profile' \
    'listing profiles must produce no invalid-name warning' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_one_models_listing_per_launch_is_shared() {
  # The window probe and the picker projection each need the endpoint's model
  # listing, and each used to fetch it — same URL, same headers, twice per launch.
  # The listing is free, so this was never a token cost, only a wasted round trip
  # and a second chance for a slow gateway to stall a shell.
  #
  # Two things must hold, and the first attempt at this memo satisfied neither:
  # the second ask must not reach the network, and a different credential against
  # the same base URL must, because two profiles can be shown different catalogues.
  # (A `body=$(_ccm_models_listing ...)` caller runs the whole function in a
  # subshell and throws the memo away with it, which is why the answer is in REPLY.)
  local test_home tally rc=0
  test_home=$(mktemp -d)
  tally="$test_home/curl-calls"
  : > "$tally"

  local HOME="$test_home"
  mkdir -p "$HOME/bin"
  printf '%s\n' '#!/bin/sh' "printf x >> '$tally'" \
    'echo "{\"data\":[{\"id\":\"m1\",\"context_length\":123456}]}"' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  local PATH="$HOME/bin:$PATH"

  source "$CCMORPH_SOURCE"

  _ccm_models_listing http://gw.invalid sk_a 5
  local first="$REPLY"
  _ccm_models_listing http://gw.invalid sk_a 8
  local second="$REPLY"

  [[ -n "$first" && "$first" == "$second" ]] || {
    print -u2 -- "FAIL: a memoised listing must return the same body"
    rc=1
  }
  local n
  n=$(wc -c < "$tally" | tr -d ' ')
  [[ "$n" == 1 ]] || {
    print -u2 -- "FAIL: two asks for one endpoint must fetch once, made $n request(s)"
    rc=1
  }

  _ccm_models_listing http://gw.invalid sk_different 8
  n=$(wc -c < "$tally" | tr -d ' ')
  [[ "$n" == 2 ]] || {
    print -u2 -- "FAIL: a different credential must re-fetch, total requests: $n"
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}

test_code_and_state_are_separate_directories() {
  # One directory used to hold both the runtime (replaced wholesale by an upgrade)
  # and every profile (which an upgrade must never touch). That only worked because
  # a git clone was also the install. A packaged install separates them — the
  # library lands in a read-only, per-version prefix — so anything writing state
  # beside the script writes somewhere unwritable or somewhere the next upgrade
  # discards. This pins the split from three sides.
  local test_home rc=0
  test_home=${$(mktemp -d):A}   # :A — the runtime resolves symlinks too

  # 1. The library path is DERIVED from where ccmorph.sh actually is, so a copy
  #    of the runtime uses its own helpers rather than reaching back to a path
  #    that may hold a different version.
  mkdir -p "$test_home/prefix/lib" "$test_home/prefix/libexec" "$test_home/state"
  cp "$CCMORPH_SOURCE" "$test_home/prefix/lib/ccmorph.sh"
  cp "$REPO_ROOT"/lib/ccmorph-*.zsh "$test_home/prefix/lib/"
  print -r -- 'print "helper-from-prefix"' > "$test_home/prefix/libexec/model-window"
  chmod +x "$test_home/prefix/libexec/model-window"

  local got
  got=$(HOME="$test_home" CCMORPH_STATE_DIR="$test_home/state" CCMORPH_LIB_DIR= \
        zsh -c "unset CCMORPH_LIB_DIR
                source '$test_home/prefix/lib/ccmorph.sh' >/dev/null 2>&1
                print -r -- \"\$_CCMORPH_LIB_DIR\"")
  [[ "$got" == "$test_home/prefix/lib" ]] || {
    print -u2 -- "FAIL: lib dir must derive from the sourced file, got '$got'"
    rc=1
  }

  # 2. State does NOT follow the library — it stays where the state seam says.
  got=$(HOME="$test_home" CCMORPH_STATE_DIR="$test_home/state" \
        zsh -c "source '$test_home/prefix/lib/ccmorph.sh' >/dev/null 2>&1
                print -r -- \"\$_CCMORPH_DIR\"")
  [[ "$got" == "$test_home/state" ]] || {
    print -u2 -- "FAIL: state dir must be independent of the library, got '$got'"
    rc=1
  }

  # 3. XDG_CONFIG_HOME is honoured. The old hardcoded path ignored it, so a
  #    machine that sets it had profiles written somewhere its owner never chose.
  got=$(HOME="$test_home" XDG_CONFIG_HOME="$test_home/xdg" \
        zsh -c "source '$test_home/prefix/lib/ccmorph.sh' >/dev/null 2>&1
                print -r -- \"\$_CCMORPH_DIR\"")
  [[ "$got" == "$test_home/xdg/ccmorph" ]] || {
    print -u2 -- "FAIL: XDG_CONFIG_HOME must place state, got '$got'"
    rc=1
  }

  # 4. The Python helpers resolve the registry from state, not from beside
  #    themselves — the bug that only worked while both were one directory.
  printf 'type=apikey\ncontext_window.pinned=424242\n' > "$test_home/state/someprofile.conf"
  # Scrubbed exactly as _ccm_model_window scrubs it: rung 1 of the resolver's
  # precedence is an author's env/conf pin, and every ccmorph-launched shell
  # exports one (the route ceiling), so an unscrubbed call here answers from the
  # ambient session instead of the registry under test.
  got=$(command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS \
        CCMORPH_STATE_DIR="$test_home/state" \
        python3 "$REPO_ROOT/libexec/model-window" someprofile pinned 2>/dev/null)
  [[ "$got" == *424242* ]] || {
    print -u2 -- "FAIL: model-window must read the registry from state, got '$got'"
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}

test_source_carries_no_host_specific_defaults() {
  # B47 + the machine-neutrality law: vanilla-shippable source must not bake in
  # this host's layout. Host specifics arrive via local.sh (gitignored) as env
  # overrides; absent, the feature silently turns off. Guard the class, not the
  # instance: no host layer path, no private socket name, no private volume layout.
  local source_text="$(cat -- "${CCMORPH_LIBRARY[@]}")"
  local rc=0
  # Needles assembled from fragments, so this test file does not itself contain
  # the private strings it forbids — the same reason the scan below does it.
  assert_not_contains "$source_text" ".config/${:-hel}${:-m}" \
    'a host layer path is a host-specific default (B47)' || rc=1
  assert_not_contains "$source_text" "${:-mei}${:-li}/" \
    'a private volume path is a host-specific default' || rc=1

  # The guard above read ONE file for TWO strings, which is how a service name, a
  # local port, and a dated incident note all sat in comments for months — each
  # individually harmless, collectively a description of one machine's private
  # setup, and none of it visible to a test that only looked for two paths in one
  # file. Scan every tracked file for the class of thing that must never ship: an
  # organisation identifier, a private hostname, a specific host's port.
  # Assembled from fragments so this test is not itself the leak it prevents.
  # Concatenated, not printed as separate words: `print -r -- 'he' 'lix'` yields
  # "he lix" and would match nothing, which made an earlier version of this guard
  # pass against a deliberately planted leak.
  local -a needles
  needles=(
    "ok""g"          # organisation short name
    "ok""x-"         # organisation product prefix
    "DA""CS"         # private environment name
    "he""lix"        # private service codename
    "83""17"         # a specific host's local port
  )
  local f n hits
  for f in ${(f)"$(cd "$REPO_ROOT" && git ls-files)"}; do
    [[ "$f" == tests/ccmorph_test.zsh ]] && continue   # this file holds the needles
    for n in "${needles[@]}"; do
      hits=$(grep -Fil -- "$n" "$REPO_ROOT/$f" 2>/dev/null) || continue
      print -u2 -- "FAIL: tracked file '$f' names '$n' — shipped source and docs must carry no org, host, or private-port identifier"
      rc=1
    done
  done
  return $rc
}

test_native_effort_does_not_rewrite_explicit_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy --model 'legacy-model-xhigh' --effort low || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains \
    "$actual" \
    $'ARG\t--model\tlegacy-model-xhigh\t--effort\tlow' \
    'native --effort must pass through without rewriting an explicit model'
  local rc=$?
  rm -rf "$test_home"
  return $rc
}

test_native_effort_validation_belongs_to_claude() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy --effort || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local _argv_rc=0
  assert_contains "$actual" $'ARG\t--effort\nSUBAGENT' \
    'ccmorph must pass native effort arguments through for Claude Code to validate' || _argv_rc=1
  assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
    'ccmorph must pass native effort arguments through for Claude Code to validate' && (( ! _argv_rc ))
  local rc=$?
  rm -rf "$test_home"
  return $rc
}

test_subscription_profile_does_not_inject_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/subscription.conf"
  source "$CCMORPH_SOURCE"
  _ccm_setup_dir() { return 0; }
  _ccm_repair_projects_link() { return 0; }

  _ccm_launch -a subscription prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains \
    "$actual" \
    $'ARG\tprompt' \
    'subscription profile arguments must reach Claude Code' || {
    rm -rf "$test_home"
    return 1
  }
  assert_not_contains \
    "$actual" \
    $'\t--model\t' \
    'subscription profiles must leave model ownership to Claude Code'
  local rc=$?
  rm -rf "$test_home"
  return $rc
}

test_invalid_resume_model_falls_back_to_profile_model() {
  local test_home
  local session_id='session-invalid-model'
  local transcript
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  printf '%s\n' \
    '{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/model</command-name>\n<command-args>unsafe\nmodel</command-args>"}}' \
    > "$transcript"

  _ccm_launch -a proxy --resume "$session_id" || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local _argv_rc=0
  assert_contains "$actual" $'ARG\t--resume\tsession-invalid-model\nSUBAGENT' \
    'invalid transcript model values must fall back to the profile bootstrap model' || _argv_rc=1
  assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
    'invalid transcript model values must fall back to the profile bootstrap model' && (( ! _argv_rc ))
  local rc=$?
  rm -rf "$test_home"
  return $rc
}

test_resume_rejects_flag_picker_and_multitoken_model_values() {
  local test_home session_id='resume-bad-shape' transcript bad actual rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  transcript="$HOME/.claude/projects/project/$session_id.jsonl"
  mkdir -p "${transcript:h}"
  for bad in '--help' '@menu' '?' 'two words'; do
    printf '%s\n' \
      "{\"type\":\"user\",\"isSidechain\":false,\"message\":{\"role\":\"user\",\"content\":\"<command-name>/model</command-name>\\n<command-args>$bad</command-args>\"}}" \
      > "$transcript"
    : > "$CCMORPH_TEST_LOG"
    _ccm_launch -a proxy --resume "$session_id" || {
      rm -rf "$test_home"
      return 1
    }
    actual="$(<"$CCMORPH_TEST_LOG")"
    assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
      "a transcript model shaped as ${(qqq)bad} must fall back to the profile default" || rc=1
  done

  rm -rf "$test_home"
  return $rc
}

write_apikey_conf() {
  local name="$1"
  shift
  printf '%s\n' 'type=apikey' "$@" > "$HOME/.config/ccmorph/$name.conf"
}

# Appends manual context_window.<model>=<value> lines to an EXISTING profile
# conf (the profile's own .conf is now the sole store -- no more global
# registry). Usage: write_profile_context <name> model=value [model=value ...]
write_profile_context() {
  local name="$1"
  shift
  python3 - "$HOME/.config/ccmorph/$name.conf" "$@" <<'PYCTX'
from pathlib import Path
import sys
p = Path(sys.argv[1])
updates = dict(pair.split("=", 1) for pair in sys.argv[2:])
lines = p.read_text().split("\n") if p.exists() else []
lines = [line for line in lines if line.partition("=")[0].removeprefix("context_window.") not in updates]
lines.extend(f"context_window.{key}={value}" for key, value in updates.items())
p.write_text("\n".join(lines) + "\n")
PYCTX
}

# Writes a context_discovered=<json> blob whose route fingerprint matches the
# profile's CURRENT conf contents, so resolve()'s discovery lookup (which
# includes glob-pattern matching, unlike manual context_window.* keys) accepts
# it. Delegates fingerprinting to the real model-window module so the route
# hash always matches production's algorithm.
write_profile_discovered() {
  local name="$1"
  shift
  local models_json="{"
  local pair model value sep=""
  for pair in "$@"; do
    model="${pair%%=*}"
    value="${pair#*=}"
    models_json+="${sep}\"${model}\":${value}"
    sep=","
  done
  models_json+="}"
  CCMORPH_STATE_DIR="$HOME/.config/ccmorph" python3 - "$name" "$models_json" <<'PY'
import importlib.machinery, importlib.util, json, os, sys
spec = importlib.util.spec_from_loader(
    "context_store",
    importlib.machinery.SourceFileLoader(
        "context_store", os.path.join(os.environ["HOME"], ".config/ccmorph/libexec/model-window")))
store = importlib.util.module_from_spec(spec)
spec.loader.exec_module(store)
name, models_json = sys.argv[1], sys.argv[2]
models = json.loads(models_json)
identity = store.profile_identity(name)
if not store.save_discovered(name, models, identity):
    sys.exit("write_profile_discovered: save failed for " + name)
PY
}

test_provider_profile_accepts_picker_exclusions() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=provider' 'provider=codex' 'excluded_models=gpt-5.5,gpt-image-2' > "$HOME/.config/ccmorph/cx.conf"
  source "$CCMORPH_SOURCE"
  local rc=0
  _ccm_read cx || { print -u2 -- "a provider profile must accept excluded_models"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
test_listings_are_undisguised_on_entry() {
  source "$CCMORPH_SOURCE"
  _ccm_undisguise_ids '{"data":[{"id":"claude-fable-5-dd-los-6-tpg","display_name":"x"},{"id":"claude-opus-5"}]}'
  assert_contains "$REPLY" '"id":"gpt-6-sol"' "a disguised id must arrive as its true id" || return 1
  assert_contains "$REPLY" '"id":"claude-opus-5"' "a genuine claude id must be left alone" || return 1
  assert_not_contains "$REPLY" "-dd-" "no disguise may survive the listing" || return 1
}
test_every_profile_keeps_its_own_secure_storage() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf apikey 'api_key=sk-test-apikey'
  printf '%s\n' 'type=sub' 'email=a@b.c' > "$HOME/.config/ccmorph/sub.conf"
  printf '%s\n' 'type=token' 'token=token-test' > "$HOME/.config/ccmorph/token.conf"
  source "$CCMORPH_SOURCE"

  # A shared secure-storage namespace hands one profile's claude.ai sign-in to
  # every other profile. Claude Code keys the store by CLAUDE_CONFIG_DIR instead.
  local profile actual rc=0
  for profile in apikey sub token proxy; do
    actual=$(ccmorph _env "$profile") || {
      print -u2 -- "profile '$profile' must produce an environment"
      rc=1
      continue
    }
    assert_not_contains "$actual" "CLAUDE_SECURESTORAGE_CONFIG_DIR=" \
      "profile '$profile' must not share a secure-storage namespace" || rc=1
    assert_contains "$actual" "CLAUDE_CONFIG_DIR=$HOME/.config/ccmorph/profiles/$profile" \
      "profile '$profile' must own its config dir" || rc=1
  done
  rm -rf "$test_home"
  return $rc
}
test_apikey_profile_exports_key_without_base_url() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf native 'api_key=sk-test-native'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env native) || {
    print -u2 -- 'a native apikey profile must produce an environment'
    rm -rf "$test_home"
    return 1
  }

  assert_contains \
    "$actual" \
    'ANTHROPIC_API_KEY=sk-test-native' \
    'an apikey profile must export ANTHROPIC_API_KEY' || rc=1
  assert_not_contains \
    "$actual" \
    'ANTHROPIC_BASE_URL' \
    'an apikey profile without base_url must reach api.anthropic.com natively' || rc=1
  assert_contains \
    "$actual" \
    "CLAUDE_CONFIG_DIR=$HOME/.config/ccmorph/profiles/native" \
    'an apikey profile must still get its own config dir' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_profile_exports_gateway_base_url() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env gw) || {
    print -u2 -- 'a gateway apikey profile must produce an environment'
    rm -rf "$test_home"
    return 1
  }

  assert_contains \
    "$actual" \
    'ANTHROPIC_BASE_URL=https://gateway.invalid' \
    'an apikey profile with base_url must point Claude Code at the gateway' || rc=1
  assert_contains \
    "$actual" \
    'ANTHROPIC_API_KEY=sk_prod_test' \
    'a gateway apikey profile must export its key' || rc=1
  assert_contains \
    "$actual" \
    'CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000' \
    'apikey profiles must honour the env.* allowlist' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_profile_advisor_disable_reaches_launch_without_leaking() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=test-key' 'base_url=https://gateway.invalid' \
    'env.CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1'
  # A native endpoint, so the adapter-route default does not mask a leak.
  write_apikey_conf other 'api_key=test-key' 'base_url=https://api.anthropic.com'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env gw) || rc=1
  assert_contains "$actual" 'CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1' \
    'a profile must be able to disable the native advisor' || rc=1
  ccmorph run gw >/dev/null 2>&1 || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'DISABLE_ADVISOR=1' \
    'the native child must receive the declared advisor control' || rc=1
  : > "$CCMORPH_TEST_LOG"
  CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1 ccmorph run other >/dev/null 2>&1 || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'DISABLE_ADVISOR=<unset>' \
    'an ambient advisor disable must not leak into another profile' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_profile_simple_mode_reaches_launch_without_leaking() {
  # A local-model profile opts into Claude Code's minimal mode through the env
  # channel, never argv (the launch contract passes arguments through untouched).
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf local 'api_key=test-key' 'base_url=http://127.0.0.1:9' \
    'env.CLAUDE_CODE_SIMPLE=1' 'env.MAX_THINKING_TOKENS=0' \
    'env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1'
  write_apikey_conf other 'api_key=test-key' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env local) || rc=1
  assert_contains "$actual" 'CLAUDE_CODE_SIMPLE=1' \
    'a profile must be able to select minimal mode' || rc=1
  assert_contains "$actual" 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1' \
    'a profile must be able to stop non-essential model calls' || rc=1
  ccmorph run local >/dev/null 2>&1 || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'SIMPLE=1' \
    'the child must receive the declared minimal mode' || rc=1
  : > "$CCMORPH_TEST_LOG"
  ccmorph run other >/dev/null 2>&1 || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'SIMPLE=<unset>' \
    'minimal mode must not leak into another profile' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_profile_passes_unknown_model_window_flag() {
  # A profile may declare CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT so
  # non-Claude models run to their real limit instead of Claude Code's 200k guess.
  # It must survive the env.* allowlist (regression for the day it was dropped).
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'env.CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env gw) || {
    print -u2 -- 'a gateway apikey profile must produce an environment'
    rm -rf "$test_home"
    return 1
  }
  assert_contains \
    "$actual" \
    'CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1' \
    'the unknown-model-window flag must survive the env.* allowlist' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_profile_may_pin_one_model_picker_row() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'env.ANTHROPIC_CUSTOM_MODEL_OPTION=claude-opus-4-8[1m]' \
    'env.ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=Opus 4.8 (1M)' \
    'env.ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=this endpoint, 1M window' \
    'env.ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-8[1m]' \
    'env.ANTHROPIC_DEFAULT_OPUS_MODEL_NAME=Opus 4.8 (1M context)' \
    'env.ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION=1M window through the gateway'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env gw) || {
    print -u2 -- 'a profile pinning a picker row must still produce an environment'
    rm -rf "$test_home"
    return 1
  }

  assert_contains \
    "$actual" \
    'ANTHROPIC_CUSTOM_MODEL_OPTION=claude-opus-4-8[1m]' \
    'a profile must be able to pin the model id of one picker row' || rc=1
  assert_contains \
    "$actual" \
    'ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=Opus 4.8 (1M)' \
    'a pinned row must be able to carry its own label' || rc=1
  assert_contains \
    "$actual" \
    'ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=this endpoint, 1M window' \
    'a pinned row must be able to carry its own description' || rc=1
  assert_contains \
    "$actual" \
    'ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-8[1m]' \
    'a profile must be able to pin the native Opus family alias' || rc=1
  assert_contains \
    "$actual" \
    'ANTHROPIC_DEFAULT_OPUS_MODEL_NAME=Opus 4.8 (1M context)' \
    'the pinned Opus alias must carry its own label' || rc=1
  assert_contains \
    "$actual" \
    'ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION=1M window through the gateway' \
    'the pinned Opus alias must carry its own description' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_profile_without_model_defers_to_claude_code() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf native 'api_key=sk-test-native'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a native prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_not_contains \
    "$actual" \
    '--model' \
    'an apikey profile with no model must let Claude Code own model selection' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_profile_with_model_injects_it() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'model=gateway-claude'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" $'ARG\tprompt\nSUBAGENT' \
    'a configured apikey model must be injected for the gateway backend' || rc=1
  assert_contains "$actual" 'MODEL_ENV=gateway-claude' \
    'a configured apikey model must be injected for the gateway backend' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_explicit_model_overrides_profile_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'model=profile-default'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw --model other-backend-model prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(grep '^ARG' "$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains \
    "$actual" \
    '--model' \
    'an explicit --model must reach Claude Code' || rc=1
  assert_not_contains \
    "$actual" \
    'profile-default' \
    'an explicit --model must suppress the profile model, not stack with it' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_without_model_passes_explicit_model_through() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf native 'api_key=sk-test-native'
  write_profile_discovered native claude-opus-5=1000000
  source "$CCMORPH_SOURCE"
  _ccm_probe_nudge() { print -r -- "$1" > "$HOME/probe-nudge"; }

  _ccm_launch -a native --model some-model prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains \
    "$actual" \
    $'--model\tsome-model' \
    'a modelless apikey profile must still forward an explicit --model' || rc=1

  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a native --model claude-opus-5 prompt || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" $'--model\tclaude-opus-5' \
    'a native Claude model must reach Claude Code' || rc=1
  assert_not_contains "$actual" 'claude-opus-5[1m]' \
    'ccmorph must not rewrite a native Claude model from probe state' || rc=1
  assert_not_contains "$actual" '"autoCompactWindow":1000000' \
    'probe state must not override native Claude context sizing' || rc=1
  assert_not_contains "$actual" 'ccmorph probe' \
    'native Claude models must leave context discovery to Claude Code' || rc=1
  [[ ! -e "$HOME/probe-nudge" ]] || {
    print -u2 -- 'a native Claude model must not schedule ccmorph discovery'
    rc=1
  }

  print -r -- 'context_window.claude-opus-5=300000' >> "$HOME/.config/ccmorph/native.conf"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a native --model claude-opus-5 prompt || rc=1
  # An explicit --model is per-session: parallel sessions share one settings.json,
  # so its budget rides the environment rather than the file.
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'AUTO_COMPACT=300000' \
    'an explicit native context override must still be honored' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_apikey_profile_requires_a_key() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf broken 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0
  if ccmorph _env broken >/dev/null 2>&1; then
    print -u2 -- 'an apikey profile with no api_key must be rejected'
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_apikey_profile_rejects_non_http_base_url() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf broken \
    'api_key=sk_prod_test' \
    'base_url=gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0
  if ccmorph _env broken >/dev/null 2>&1; then
    print -u2 -- 'an apikey base_url without a scheme must be rejected'
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

# --- adapter contract -------------------------------------------------------
# A profile may declare that its endpoint needs a dialect adapter in front of it.
# The profile keeps its REAL base_url; only the environment handed to Claude Code
# is redirected. That keeps `ccmorph _env` truthful for every other consumer.

test_adapter_accepted_on_apikey_with_base_url() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'adapter=true'
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_read gw || {
    print -u2 -- 'adapter=true must be a valid key on an apikey profile'
    rc=1
  }
  [[ "${_ccm[adapter]:-}" == true ]] || {
    print -u2 -- "adapter value not parsed; got ${(qqq)_ccm[adapter]:-}"
    rc=1
  }
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_adapter_requires_a_base_url_to_front() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf native 'api_key=sk-test' 'adapter=true'
  source "$CCMORPH_SOURCE"

  local rc=0
  # A native Anthropic key has no foreign dialect to translate, so an adapter
  # would have nothing to front.
  if _ccm_read native && _ccm_validate_loaded native; then
    print -u2 -- 'adapter=true without base_url must be rejected'
    rc=1
  fi
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_adapter_rejects_non_true_values() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=sk-test' 'base_url=https://g.invalid' 'adapter=yes'
  source "$CCMORPH_SOURCE"

  local rc=0
  if _ccm_read gw && _ccm_validate_loaded gw; then
    print -u2 -- 'only adapter=true may be accepted, not an arbitrary value'
    rc=1
  fi
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_the_profile_template_is_a_valid_conf_naming_every_key() {
  # profile.conf.example is the documented contract for a hand-edited profile. It
  # must load as-is, and its key list must stay the parser's list: a key the
  # reader accepts but the template never names is undocumented, and one the
  # template names but the reader rejects is a lie.
  local test_home rc=0 k line arm rest documented
  test_home=$(mktemp -d)
  setup_home "$test_home"
  command cp "$REPO_ROOT/share/profile.conf.example" "$HOME/.config/ccmorph/tpl.conf"
  source "$CCMORPH_SOURCE"
  _ccm_read tpl 2>/dev/null && _ccm_validate_loaded tpl 2>/dev/null || {
    print -u2 -- 'the profile template must load and validate unchanged'
    rc=1
  }
  unset _ccm

  # Read EVERY case arm of the reader, not the first: the accepted keys are split
  # across arms, so `head -1` sees only the type/identity list and every key below
  # it — including any added later — goes unchecked. An arm is a line of exactly
  # six spaces followed by `key|key)`. Arms that report a removed key are not
  # accepted keys and are skipped by the marker their `case` body prints, so this
  # reads `_ccm_read` structurally rather than retyping its six-arm layout.
  integer in_case=0 removed=0
  local -a accepted=()
  while IFS= read -r line; do
    [[ "$line" == '_ccm_read() {' ]] && { in_case=1; continue }
    (( in_case )) || continue
    [[ "$line" == '}' ]] && break
    if [[ "$line" == '      '*')' ]]; then
      rest="${line#'      '}"
      if [[ "$rest" == *[[:space:]]* ]]; then
        continue
      elif [[ -n "$arm" ]]; then
        (( removed )) || accepted+=("${(s:|:)${arm%\)}}")
      fi
      arm="$rest"
      removed=0
    elif [[ "$line" == *'was removed'* ]]; then
      removed=1
    fi
  done < <(cat -- "${CCMORPH_LIBRARY[@]}")
  if [[ -n "$arm" ]] && (( ! removed )); then
    accepted+=("${(s:|:)${arm%\)}}")
  fi
  (( ${#accepted} )) || { print -u2 -- 'could not find the reader key list'; rc=1; }
  # The last arm is the catch-all pattern. A read that stopped at the first arm
  # never sees it, so finding it proves the whole `case` was read.
  [[ " ${(j: :)accepted} " == *' * '* ]] || {
    print -u2 -- 'the reader key list must come from every case arm, not the first'
    rc=1
  }

  local doc
  doc=$(<"$REPO_ROOT/share/profile.conf.example")
  for k in $accepted; do
    # A key is documented by its literal line, by a comment naming it, or by the
    # `# pattern.<arg>` comment a pattern key uses (`effort.<model>`). The
    # trailing class stops `# model` from being satisfied by `# model_defaults`.
    documented=0
    if [[ "$k" == '*' ]]; then
      [[ "$doc" == *'#'[[:space:]]'env.'* ]] && documented=1
    else
      local bare="${k%\*}"
      [[ "$bare" == *.* ]] && bare="${bare%%.*}"
      [[ "$doc" == *"$bare="* || "$doc" == *"# $bare"[^a-zA-Z0-9_]* ||
         "$doc" == *"# $bare."[^a-zA-Z0-9_]* ]] && documented=1
    fi
    (( documented )) || {
      print -u2 -- "profile.conf.example does not document the accepted key '$k'"
      rc=1
    }
  done
  rm -rf "$test_home"
  return $rc
}

test_monthly_budget_usd_loads_and_a_non_number_is_refused() {
  # Stark owns monthly_budget_usd in the shared conf; ccmorph must load it, not refuse the profile.
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=sk-test' 'base_url=https://g.invalid' 'monthly_budget_usd=500'
  write_apikey_conf bad 'api_key=sk-test' 'base_url=https://g.invalid' 'monthly_budget_usd=lots'
  source "$CCMORPH_SOURCE"
  _ccm_read gw >/dev/null 2>&1 || { print -u2 -- 'monthly_budget_usd=500 must load'; rc=1; }
  _ccm_read bad >/dev/null 2>&1 && { print -u2 -- 'a non-number budget must be refused'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_a_legacy_transport_key_is_reported_and_ignored() {
  # `transport=`, `forward_ip=`, `subagent_model=`, `lean=` and `role=` were all
  # removed, not reserved. An unknown key is fatal here, so a conf written before
  # any of these removals would stop launching — the one outcome a removal must
  # not cause. Each loads, says once what was removed, and carries on with the key
  # absent. `role` lost its arm when roles were removed wholesale, so it fell to
  # the unknown-key error instead of warning like its siblings (GH-425).
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=sk-test' 'base_url=https://g.invalid' \
    'adapter=true' 'transport=runner-token@east' 'forward_ip=192.0.2.46' \
    'subagent_model=legacy-subagent-model' 'lean=true' 'role=worker'
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(_ccm_read gw 2>&1 >/dev/null) || {
    print -u2 -- 'a conf still declaring legacy keys must still load'
    rc=1
  }
  assert_contains "$err" 'transport= was removed' \
    'the removal must name itself on stderr' || rc=1
  assert_contains "$err" 'HTTPS_PROXY' \
    'the line must say what to do instead' || rc=1
  assert_contains "$err" 'forward_ip= was removed' \
    'the forward_ip removal must name itself on stderr' || rc=1
  assert_contains "$err" 'subagent_model= was removed' \
    'the subagent_model removal must name itself on stderr' || rc=1
  assert_contains "$err" 'lean= was removed' \
    'the lean removal must name itself on stderr' || rc=1
  assert_contains "$err" 'role= was removed' \
    'the role removal must name itself on stderr (GH-425)' || rc=1
  _ccm_read gw 2>/dev/null
  local _legacy_k
  for _legacy_k in transport forward_ip subagent_model lean role; do
    [[ -z "${_ccm[$_legacy_k]:-}" ]] || {
      print -u2 -- "the removed key '$_legacy_k' must not survive into the profile; got ${(qqq)_ccm[$_legacy_k]}"
      rc=1
    }
  done
  _ccm_validate_loaded gw || {
    print -u2 -- 'a profile carrying legacy keys must still validate'
    rc=1
  }
  unset _ccm

  # The earlier rename carries NO back-compat alias either: a profile still
  # writing `via=` is rejected as invalid rather than silently ignored.
  write_apikey_conf old 'api_key=sk-test' 'base_url=https://g.invalid' \
    'via=runner-token@east'
  if _ccm_read old 2>/dev/null && _ccm_validate_loaded old; then
    print -u2 -- 'the old `via=` key must no longer be recognised'
    rc=1
  fi
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_key_prints_the_api_key_for_an_apikey_account() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=sk-test-value' 'base_url=https://g.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0 out
  out=$(ccmorph cred gw) || rc=1
  if [[ "$out" != 'sk-test-value' ]]; then
    print -u2 -- "key must print the api key bare; got ${(qqq)out}"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_key_does_not_provision_claude_profile_state() {
  # `_env` reaches a key only by calling _ccm_setup_dir, which creates the
  # profile dir, 11 shared symlinks and .claude.json. A non-Claude consumer
  # (Pi reads gateway keys through this) must not trigger any of that.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=sk-test-value' 'base_url=https://g.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0
  ccmorph cred gw >/dev/null || rc=1
  if [[ -e "$test_home/.config/ccmorph/profiles/gw" ]]; then
    print -u2 -- 'reading a key must not provision Claude profile state'
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

# `secret` replaced `key` and `token`, which were one operation split by profile type.
# The old test asserted that split -- `key` had to REJECT a token profile -- so it
# encoded exactly the friction the merge removes. What must hold now: one verb answers
# for whichever credential the profile actually holds, and the user never has to know
# which kind it is before choosing a command.
test_secret_prints_whichever_secret_the_account_holds() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  print -r -- $'type=token\ntoken=tok-secret' > "$test_home/.config/ccmorph/tok.conf"
  write_apikey_conf ak 'api_key=key-secret' 'base_url=https://g.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0 got
  got=$(ccmorph cred tok) || {
    print -u2 -- 'secret must succeed on a token profile'
    rm -rf "$test_home"; return 1
  }
  [[ "$got" == tok-secret ]] || {
    print -u2 -- "secret must print a token profile's token (got: $got)"
    rc=1
  }
  got=$(ccmorph cred ak) || {
    print -u2 -- 'secret must succeed on an apikey profile'
    rm -rf "$test_home"; return 1
  }
  [[ "$got" == key-secret ]] || {
    print -u2 -- "secret must print an apikey profile's key (got: $got)"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# A sub profile authenticates by browser login and stores no credential in the conf,
# so there is nothing to print. That is a refusal with an explanation, not a success.
test_secret_refuses_an_account_that_stores_no_secret() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  print -r -- $'type=sub\nemail=a@b.invalid' > "$test_home/.config/ccmorph/s.conf"
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(ccmorph cred s 2>&1) && {
    print -u2 -- 'secret must fail on an profile that stores no credential'
    rc=1
  }
  assert_contains "$err" 'login' \
    'the refusal must say how a sub profile does authenticate' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_legacy_route_key_is_rejected_as_unknown() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  local legacy_key="endpoint"_"side"
  write_apikey_conf gw 'api_key=sk-test' 'base_url=https://g.invalid' \
    "${legacy_key}=outside"
  source "$CCMORPH_SOURCE"

  local rc=0
  if _ccm_read gw && _ccm_validate_loaded gw; then
    print -u2 -- 'the legacy route key must be rejected as unknown'
    rc=1
  fi
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_adapter_is_invalid_on_other_profile_types() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=token' 'token=sk-ant-x' 'adapter=true' \
    > "$HOME/.config/ccmorph/tok.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  if _ccm_read tok; then
    print -u2 -- 'adapter is meaningless on a token profile and must be rejected'
    rc=1
  fi
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_an_unreachable_endpoint_names_the_network_not_an_exception() {
  # ccmorph no longer probes reachability — there is nothing left to decide with
  # it — so this message now rides on the request that genuinely failed. It must
  # still lead with what the USER can act on: the endpoint, and the network that
  # is probably off, never a variable or a curl exit code.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  write_curl_mock 'return 7'

  printf '%s\n' 'type=proxy' 'base_url=http://unreachable.invalid' 'api_key=k' \
    'model=m' > "$HOME/.config/ccmorph/dead.conf"

  local err fail=0
  err=$(_ccm_gw_models_json dead 2>&1 >/dev/null) && {
    print -u2 -- 'an unreachable endpoint must fail model discovery'
    fail=1
  }
  assert_contains "$err" 'unreachable.invalid' \
    'the failure must name the endpoint that did not answer' || fail=1
  assert_contains "$err" 'private network' \
    'the failure must tell the user how to fix it' || fail=1
  rm -rf "$test_home"
  return $fail
}

test_proxy_profile_may_declare_adapter() {
  # The launcher already reads adapter for proxy|apikey alike (one case arm), so a
  # proxy conf declaring it was rejected by validation for a capability the rest
  # of the code supports. A local llm behind a translator is exactly this shape.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=proxy' 'base_url=http://127.0.0.1:11434' 'api_key=none' \
    'model=local-model' 'adapter=true' \
    > "$HOME/.config/ccmorph/loc.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  if ! _ccm_read loc; then
    print -u2 -- 'a proxy profile must be allowed to declare adapter=true'
    rc=1
  elif [[ "${_ccm[adapter]:-}" != true ]]; then
    print -u2 -- 'adapter=true must survive parsing on a proxy profile'
    rc=1
  fi
  unset _ccm
  rm -rf "$test_home"
  return $rc
}

test_env_reports_the_real_endpoint_not_the_adapter() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf gw \
    'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' \
    'adapter=true'
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph _env gw) || {
    rm -rf "$test_home"
    return 1
  }
  # _env is consumed by other tooling, including the adapter itself to learn its
  # own upstream. It must never report the local redirect.
  assert_contains "$actual" 'ANTHROPIC_BASE_URL=https://gateway.invalid' \
    '_env must report the profile'"'"'s real endpoint' || rc=1
  assert_not_contains "$actual" '127.0.0.1' \
    '_env must not report a local adapter address' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A stand-in adapter at the path ccmorph resolves, so no PATH tricks are needed.
# Records its argv, announces a port, idles, and marks the fact it was stopped.
fake_adapter() {
  write_fake_adapter \
    'printf "%s\n" "$@" >> "$HOME/adapter.argv"' \
    'trap "echo stopped >> $HOME/adapter.stopped; exit 0" TERM INT' \
    'echo "PORT=45999"'
}

# An adapter that fails before announcing a port: the launch must abort.
fake_adapter_broken() {
  printf '%s\n' '#!/bin/sh' 'echo "boom" >&2' 'exit 1' \
    > "$HOME/.config/ccmorph/libexec/ccmorph-adapter"
  chmod +x "$HOME/.config/ccmorph/libexec/ccmorph-adapter"
}

# The port the picker test's launch reads, named like its siblings so
# test_adapter_doubles_die_with_a_killed_launcher installs it too.
fake_adapter_picker_port() {
  write_fake_adapter 'echo PORT=49123' 'trap "exit 0" TERM INT'
}

# A recording stub proves adapter launch does not invoke tmux for placement.
fake_recording_tmux() {
  printf '%s\n' \
    '#!/bin/sh' \
    'printf "%s\n" "$*" >> "$HOME/tmux.argv"' \
    'case "$1" in' \
    '  list-sessions) exit 0 ;;' \
    '  new-session)' \
    '    shift' \
    '    while [ "$1" != "--" ]; do shift; done' \
    '    shift' \
    '    ( eval "$1" ) &' \
    '    printf "%s\n" "$!" > "$HOME/tmux.pid"' \
    '    ;;' \
    '  kill-session)' \
    '    kill "$(cat "$HOME/tmux.pid")" 2>/dev/null || :' \
    '    ;;' \
    'esac' \
    > "$HOME/bin/tmux"
  chmod +x "$HOME/bin/tmux"
}

test_adapter_redirects_claude_to_the_local_port() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  # The Gate runs this suite from a terminal, and a terminal is the launcher's
  # condition for asking a model-less routed route to choose a model
  # (ccmorph.sh: `needs_adapter && ask_missing_model && -t 0 && -t 2`). Without
  # the fake picker this fixture fell through to the operator's real fzf and the
  # suite waited on it until someone killed it (#222). The choice is not what this
  # test is about; the fake answers from the fixture's own rows.
  fake_fzf
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" 'BASE_URL=http://127.0.0.1:45999' \
    'Claude Code must be pointed at the adapter, not the real endpoint' || rc=1
  assert_contains "$actual" 'API_KEY=sk_prod_test' \
    "the profile's own key must still be used" || rc=1
  # The adapter is told which profile to serve, never handed the credential.
  assert_contains "$(<"$HOME/adapter.argv")" 'gw' \
    'the adapter must receive the profile name' || rc=1
  assert_not_contains "$(<"$HOME/adapter.argv")" 'sk_prod_test' \
    'the credential must never appear in adapter argv' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A picker sentinel: it stands where a real fzf would be found and fails at once,
# so a fixture that falls through to a picker fails the test instead of blocking
# it on terminal input. It records the attempt to a path of the caller's choosing,
# because the suite it guards installs a scratch HOME of its own.
fzf_sentinel() {
  local dir="$1" ran="$2"
  mkdir -p "$dir"
  printf '%s\n' \
    '#!/bin/sh' \
    "printf 'reached\n' >> '$ran'" \
    'exit 1' \
    > "$dir/fzf"
  chmod +x "$dir/fzf"
}

# Run a command with a pty as stdin, stdout and stderr, as a terminal does, and
# leave what the terminal saw in $1. A test cannot fake `-t`, so a terminal is the
# only way to reproduce the launcher's own condition for asking a model-less
# routed route to pick a model. The pty is drained (the launch can out-talk the
# terminal buffer) and the child is killed at the deadline -- 45 s of idle
# machine, longer while the load is high, so a busy fleet is not a suite failure
# (#365) -- so a fixture that waits on input cannot hang the suite again. The
# runner itself is tests/under_a_terminal.py, so its deadline is tested there.
run_under_a_terminal() {
  local out="$1"
  shift
  python3 "$SCRIPT_DIR/under_a_terminal.py" "$@" > "$out" 2>&1
}

# The adapter-redirect test again, but launched the way the remote Gate launches
# the suite: from a terminal. Both stdin and stderr are a pty there, which is the
# launcher's condition for opening the model picker on a model-less routed route
# (ccmorph.sh: `needs_adapter && ask_missing_model && -t 0 && -t 2`). Without a
# picker of its own the fixture reached the operator's real fzf, and the suite sat
# on that child until someone killed it (#222: 7m58s, exit 70).
#
# It runs the test itself, in its own suite process, so the assertions under the
# terminal are the endpoint, credential and adapter-argument ones already there --
# a copy could drift from them. The sentinel goes on PATH behind the fixture's own
# bin/: setup_home keeps $HOME/bin first, so a repaired launch answers from the
# fixture's fake picker, and anything else it reaches is the failure this test
# exists for.
test_adapter_redirect_under_a_terminal_never_reaches_the_operators_picker() {
  local test_home rc=0
  test_home=$(mktemp -d)
  local sentinel_dir="$test_home/sentinel" sentinel_ran="$test_home/sentinel.ran"
  fzf_sentinel "$sentinel_dir" "$sentinel_ran"
  local target=test_adapter_redirects_claude_to_the_local_port
  local suite="$SCRIPT_DIR/ccmorph_test.zsh" out="$test_home/terminal.log"
  # The wrapper can run on any parent shard. Its child selects one test, so
  # give it a single shard rather than inheriting one that skips the fixture.

  # The probe allocates a pty and gives both ends back; a runner that refuses one
  # says why, and that reason is printed rather than silenced -- the fallback is a
  # weaker trigger, not evidence of the terminal case.
  local probe
  if probe=$(python3 -c 'import os, pty; m, s = pty.openpty(); os.close(m); os.close(s)' 2>&1); then
    run_under_a_terminal "$out" env PATH="$sentinel_dir:$PATH" \
      CCMORPH_TEST_FILTER="$target" CCMORPH_TEST_SHARD=0/1 CCMORPH_TEST_JOBS=1 \
      zsh "$suite" || rc=1
  else
    # A sandboxed runner can refuse pty allocation. The launcher's own
    # _CCMORPH_INTERACTIVE seam enters the same picker branch, so the launch
    # under test is still the one that asks; only how the terminal is simulated
    # changes.
    print -u2 -- "$probe"
    print -u2 -- 'note: no pty here; driving the model picker through' \
      '_CCMORPH_INTERACTIVE (this run is not PTY evidence)'
    env PATH="$sentinel_dir:$PATH" _CCMORPH_INTERACTIVE=1 \
      CCMORPH_TEST_FILTER="$target" CCMORPH_TEST_SHARD=0/1 CCMORPH_TEST_JOBS=1 \
      zsh "$suite" > "$out" 2>&1 || rc=1
  fi
  [[ ! -e "$sentinel_ran" ]] || {
    print -u2 -- 'the launch reached a picker outside the fixture; it must use only fakes'
    rc=1
  }
  # The filter has to have matched that test: a rename would otherwise leave this
  # regression passing on an empty run.
  assert_contains "$(<"$out")" 'All 1 tests passed' \
    'the adapter-redirect test must complete under a terminal' || rc=1
  assert_contains "$(<"$out")" 'Gateway ready' \
    'a launch behind the adapter announces its gateway on a terminal' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Task tags (#173): the launcher turns CCMORPH_TASK/ATTEMPT into request headers
# for the gateway shim, only through the adapter, only in shapes the shim accepts, and
# never lets the variables reach the session's own children.
task_stub_claude() {
  printf '%s\n' '#!/bin/sh' \
    '{ printf "HDRS=<%s>\n" "${ANTHROPIC_CUSTOM_HEADERS-unset}"' \
    '  printf "TASK=<%s> ATTEMPT=<%s>\n" "${CCMORPH_TASK-unset}" "${CCMORPH_ATTEMPT-unset}"' \
    '  printf "PCT=<%s>\n" "${CLAUDE_AUTOCOMPACT_PCT_OVERRIDE-unset}"' \
    '} >> "$CCMORPH_TEST_LOG"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
}

test_task_env_becomes_request_headers_on_an_adapter_launch() {
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  task_stub_claude
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"
  CCMORPH_TASK=issue-42 CCMORPH_ATTEMPT=2 _ccm_launch -a gw prompt || rc=1
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'x-ccmorph-task: issue-42' 'the task header' || rc=1
  assert_contains "$actual" 'x-ccmorph-attempt: 2>' 'the attempt header' || rc=1
  assert_contains "$actual" 'TASK=<unset> ATTEMPT=<unset>' 'the session must not inherit the variables' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The launch sets ANTHROPIC_CUSTOM_HEADERS for the task tags and the projection
# strips that key from the generated env block, so without a merge the user's own
# header lines (a tenant id, say, in the profile's settings layer) are dropped by
# exactly the launches that carry a tag (#208). The user's lines come first, then
# the tags.
test_task_tags_merge_with_the_users_own_custom_headers_issue_208() {
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  task_stub_claude
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"env":{"ANTHROPIC_CUSTOM_HEADERS":"x-tenant: acme"}}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.profile.json"
  source "$CCMORPH_SOURCE"
  CCMORPH_TASK=issue-208 _ccm_launch -a gw prompt || rc=1
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" $'x-tenant: acme\nx-ccmorph-task: issue-208' \
    'the user header must be kept in front of the task tag' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The record stream is NUL-framed, so a header value carrying an embedded NUL
# (JSON \u0000) would end its userset record early and the rest of the value
# would be parsed as records of their own -- a settings-layer header must not
# be able to forge a policy record and move the compaction trigger (#208
# review). The framer drops a NUL-bearing record whole; the tags stand alone.
test_a_nul_in_the_users_header_cannot_inject_a_record_issue_208() {
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  task_stub_claude
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"env":{"ANTHROPIC_CUSTOM_HEADERS":"x-tenant: acme\u0000policy\t20"}}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.profile.json"
  source "$CCMORPH_SOURCE"
  CCMORPH_TASK=issue-208 _ccm_launch -a gw prompt || rc=1
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'PCT=<80>' \
    'the fixture policy must set the trigger' || rc=1
  assert_not_contains "$actual" 'PCT=<20>' \
    'text after an embedded NUL must not be parsed as a record' || rc=1
  assert_not_contains "$actual" 'x-tenant' \
    'a NUL-bearing header value is dropped whole, not emitted truncated' || rc=1
  assert_contains "$actual" 'x-ccmorph-task: issue-208' 'the task header' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_no_task_means_no_headers_and_bad_shapes_are_dropped() {
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  task_stub_claude
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"
  _ccm_launch -a gw prompt || rc=1
  CCMORPH_TASK=/etc/passwd CCMORPH_ATTEMPT=x _ccm_launch -a gw prompt || rc=1
  [[ "$(grep -c '^HDRS=<unset>$' "$CCMORPH_TEST_LOG")" == 2 ]] || {
    print -u2 -- 'no task, or only malformed values, must leave the headers unset'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_a_launch_without_the_adapter_never_carries_task_headers() {
  local test_home rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  task_stub_claude
  write_apikey_conf direct 'api_key=sk_prod_test' 'base_url=https://api.anthropic.com'
  source "$CCMORPH_SOURCE"
  CCMORPH_TASK=issue-42 _ccm_launch -a direct prompt || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'HDRS=<unset>' 'a direct launch has no shim to read them' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_adapter_is_stopped_when_the_session_ends() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw prompt >/dev/null 2>&1
  local rc=0
  [[ -s "$HOME/adapter.stopped" ]] || {
    print -u2 -- 'adapter must be stopped when the launch returns'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# Launch whatever double is installed as the adapter and say whether it keeps
# the real adapter's lifetime contract against a launcher that dies without
# running any trap: it must announce "$2" as its first stdout line (the line
# the launcher's handshake reads, _ccm_adapter_start), still be alive once it
# has, and be gone within seconds of the launcher it was handed with
# --parent-pid. "$1" picks the flag's spelling: "separate" is what
# _ccm_adapter_start passes, "equals" is what the real adapter's argparse
# accepts just as well.
_double_dies_with_a_killed_launcher() {
  local spelling="$1" announce="$2" out="$HOME/double.out"
  local -a parent
  local launcher double first rc=1
  # The retry count IS the shared bound (0.1 s a pass), named in each loop header
  # so the hang-bound guard reads it as that bound and not a private budget (#457).
  local -i i
  rm -f "$out"
  sleep 30 & launcher=$!
  if [ "$spelling" = equals ]; then
    parent=("--parent-pid=$launcher")
  else
    parent=(--parent-pid "$launcher")
  fi
  "$HOME/.config/ccmorph/libexec/ccmorph-adapter" gw "${parent[@]}" >"$out" 2>&1 &
  double=$!
  for (( i = 0; i < CCMORPH_TEST_HANG_TIMEOUT * 10; i++ )); do [[ -s "$out" ]] && break; sleep 0.1; done
  first=$(head -n 1 "$out" 2>/dev/null)
  if [ "$first" != "$announce" ]; then
    print -ru2 -- "  expected the announce '$announce', got '${first:-nothing}'"
  elif ! kill -0 "$double" 2>/dev/null; then
    print -ru2 -- "  died before its launcher did"
  else
    kill -KILL "$launcher" 2>/dev/null
    wait "$launcher" 2>/dev/null  # reaped, not a zombie: the double's poll must tell the truth
    for (( i = 0; i < CCMORPH_TEST_HANG_TIMEOUT * 10; i++ )); do kill -0 "$double" 2>/dev/null || break; sleep 0.1; done
    if kill -0 "$double" 2>/dev/null; then
      print -ru2 -- "  outlived its killed launcher"
    else
      rc=0
    fi
  fi
  kill -KILL "$launcher" "$double" 2>/dev/null
  wait "$launcher" "$double" 2>/dev/null
  return $rc
}

test_adapter_doubles_die_with_a_killed_launcher() {
  # The real adapter polls the pid the launcher hands it with --parent-pid and
  # exits once that pid is gone (docs/ARCHITECTURE.md): the launcher's EXIT trap is
  # the only other stop path, and a launcher killed outright runs no trap --
  # the stranding #386 counted. Every double this suite installs must carry the
  # same watchdog, one case per construction point (#373, #395): the hermetic
  # fixture every test inherits, both fake_adapter writers, and the picker
  # test's own port announce. Each case also checks the handshake half -- the
  # announce is the first stdout line and the double is alive to have printed
  # it. The Python suite pins the same contract on fake_adapter.script()
  # (fake_adapter_contract_test.py); this is the zsh twin, and the only case
  # that runs the doubles write_fake_adapter and tests/fake_adapter_serve.sh
  # actually build.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  local rc=0 writer announce
  for writer in hermetic fake_adapter fake_adapter_records_proxy fake_adapter_picker_port; do
    case "$writer" in
      hermetic) announce='NATIVE=1' ;;  # setup_home already wrote it
      fake_adapter) fake_adapter; announce='PORT=45999' ;;
      fake_adapter_records_proxy) fake_adapter_records_proxy; announce='PORT=45999' ;;
      fake_adapter_picker_port) fake_adapter_picker_port; announce='PORT=49123' ;;
    esac
    _double_dies_with_a_killed_launcher separate "$announce" || {
      print -u2 -- "$writer: the adapter double broke the lifetime contract"
      rc=1
    }
  done
  # The equals spelling once, on a double written for it: every double shares
  # the same emitted parse, and the launcher only ever passes the separate
  # form, so a typo there would surface nowhere else.
  fake_adapter_picker_port
  _double_dies_with_a_killed_launcher equals 'PORT=49123' || {
    print -u2 -- 'the adapter doubles ignored the equals-spelled parent pid'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_adapter_launch_survives_an_unwritable_optional_log() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  : > "$HOME/blocked.log"
  chmod 0444 "$HOME/blocked.log"
  _CCMORPH_LOG="$HOME/blocked.log"
  local rc=0
  _ccm_launch -a gw prompt >/dev/null 2>&1 || {
    print -u2 -- 'adapter launch must survive an unwritable optional log'
    rc=1
  }
  local -a _st
  zstat -A _st +mode "$HOME/blocked.log"
  (( (_st[1] & 8#777) == 8#400 )) || {
    print -u2 -- 'log init must drop group/other access without adding owner write'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_adapter_launch_uses_current_process_tree() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_adapter
  fake_recording_tmux
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  local rc=0
  mkdir -p "$HOME/not-shared-tmp"
  TMPDIR="$HOME/not-shared-tmp" \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || {
    print -u2 -- 'the adapter must launch locally in every process tree'
    rc=1
  }
  [[ ! -e "$HOME/tmux.argv" ]] || {
    print -u2 -- 'adapter launch must not cross the process-tree boundary'
    rc=1
  }
  assert_contains "$(<"$HOME/adapter.argv")" 'gw' \
    'the local adapter must receive the profile name' || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'BASE_URL=http://127.0.0.1:45999' \
    'the launched Claude process must use the local adapter port' || rc=1
  rm -rf "$test_home"
  return $rc
}

# An adapter stub that records the HTTPS_PROXY it was launched with, so a test can
# prove the launcher pointed it through the connect proxy.
fake_adapter_records_proxy() {
  write_fake_adapter \
    'printf "%s\n" "$@" >> "$HOME/adapter.argv"' \
    'printf "HTTPS_PROXY=%s\n" "${HTTPS_PROXY-<unset>}" >> "$HOME/adapter.env"' \
    'trap "exit 0" TERM INT' \
    'echo "PORT=45999"'
}

test_no_ambient_proxy_means_the_adapter_gets_none() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter_records_proxy
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  ( unset HTTPS_PROXY https_proxy; _ccm_launch -a gw prompt ) >/dev/null 2>&1
  local rc=0
  # ccmorph invents no proxy: with none in the environment the adapter gets none.
  assert_contains "$(<"$HOME/adapter.env")" 'HTTPS_PROXY=<unset>' \
    'ccmorph must not synthesize a proxy for the adapter' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_an_ambient_proxy_reaches_the_child_and_the_adapter() {
  # THE regression. Every network consumer used to be handed
  # `HTTPS_PROXY="${_ccm_route_https_proxy:-}"`, which with no transport running
  # expanded to the empty string — so a user who had already bridged correctly had
  # their proxy BLANKED by the tool that was supposed to use it. ccmorph now owns
  # no transport and must pass the caller's value through untouched.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_records_proxy
  fake_adapter_records_proxy
  printf '%s\n' \
    '#!/bin/sh' \
    'printf "HTTPS_PROXY=%s\n" "${HTTPS_PROXY-<unset>}" >> "$HOME/claude.env"' \
    'printf "https_proxy=%s\n" "${https_proxy-<unset>}" >> "$HOME/claude.env"' \
    'printf "NO_PROXY=%s\n" "${NO_PROXY-<unset>}" >> "$HOME/claude.env"' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  local rc=0
  HTTPS_PROXY='http://127.0.0.1:48080' https_proxy='http://127.0.0.1:48080' \
    NO_PROXY='127.0.0.1,localhost' \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || {
    print -u2 -- 'a launch under an ambient proxy must still start Claude'
    rc=1
  }

  local child="$(<"$HOME/claude.env")"
  assert_contains "$child" 'HTTPS_PROXY=http://127.0.0.1:48080' \
    'the Claude child must see the ambient HTTPS_PROXY unchanged' || rc=1
  assert_contains "$child" 'https_proxy=http://127.0.0.1:48080' \
    'the lowercase spelling must survive too' || rc=1
  assert_contains "$child" 'NO_PROXY=127.0.0.1,localhost' \
    'NO_PROXY must survive the launch sweep' || rc=1
  assert_contains "$(<"$HOME/adapter.env")" 'HTTPS_PROXY=http://127.0.0.1:48080' \
    'the adapter must dial the endpoint through the same ambient proxy' || rc=1
  assert_contains "$(<"$HOME/curl.env")" 'HTTPS_PROXY=http://127.0.0.1:48080' \
    'the model listing must go through the ambient proxy too' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The adapter's cliproxyapi config hoists every mid-conversation system turn into
# the top-level system field (rebuild-mid-system-message, set for vLLM upstreams
# that reject the role). cliproxyapi pools same-type entries round-robin, so that
# setting cannot be scoped per model, and hoisting rewrites the prompt prefix each
# turn -- which defeats prompt caching for every Claude model on the route. Told
# no model may carry the turn, Claude Code folds its reminders into user turns
# instead and the prefix stays append-only. One rule for every model id.
test_adapter_launch_withholds_mid_conversation_system_for_claude_models() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw prompt >/dev/null 2>&1
  local rc=0
  assert_contains "$(<"$CCMORPH_TEST_LOG")" \
    'MODEL_CAPS=-mid_conv_system,-mid_conv_tool_change,-per_turn_effort' \
    'an adapter launch must stop Claude Code sending mid-conversation system turns' || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'DISABLE_ADVISOR=1' \
    'an adapter route must not be offered the server-side advisor tool' || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'ALWAYS_EFFORT=1' \
    'an adapter route must expose /effort without a per-profile line' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A value the user or the conf already carries wins: ccmorph fills a gap in the
# launch env, it never overrules a choice made there.
test_declared_model_capabilities_is_never_overruled() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  write_apikey_conf gw 'api_key=sk_prod_test' \
    'base_url=https://gateway.invalid' 'adapter=true' \
    'env.CLAUDE_CODE_MODEL_CAPABILITIES=claude-*=-mid_conv_tool_change' \
    'env.CLAUDE_CODE_DISABLE_ADVISOR_TOOL=0'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a gw prompt >/dev/null 2>&1
  local rc=0
  local log="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$log" \
    'MODEL_CAPS=claude-*=-mid_conv_tool_change' \
    'a conf-declared capability mask must reach the engine unchanged' || rc=1
  assert_not_contains "$log" '-mid_conv_system' \
    'the conf value must not be replaced by the ccmorph default' || rc=1
  assert_contains "$log" 'DISABLE_ADVISOR=0' \
    'a conf that keeps the advisor must not be overruled' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Only an adapter route hoists, so only it gets the default.
test_native_endpoint_keeps_mid_conversation_system() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf plain 'api_key=sk_prod_test' \
    'base_url=https://api.anthropic.com'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a plain prompt >/dev/null 2>&1
  local rc=0
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MODEL_CAPS=<unset>' \
    'an endpoint that speaks Anthropic natively must keep the native turn' || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'DISABLE_ADVISOR=<unset>' \
    'a native endpoint keeps the advisor tool' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_plain_profile_never_starts_an_adapter() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter
  write_apikey_conf plain 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  _ccm_launch -a plain prompt >/dev/null 2>&1
  local rc=0
  [[ ! -e "$HOME/adapter.argv" ]] || {
    print -u2 -- 'a profile without adapter=true must not invoke the adapter'
    rc=1
  }
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'BASE_URL=https://gateway.invalid' \
    'a plain profile must reach its real endpoint directly' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_aborts_when_the_adapter_cannot_serve() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  fake_adapter_broken
  write_apikey_conf gw \
    'api_key=sk_prod_test' 'base_url=https://gateway.invalid' 'adapter=true'
  source "$CCMORPH_SOURCE"

  local rc=0
  if _ccm_launch -a gw prompt >/dev/null 2>&1; then
    print -u2 -- 'a failed adapter must abort the launch'
    rc=1
  fi
  # Claude Code must never run against a dead port.
  [[ ! -s "$CCMORPH_TEST_LOG" ]] || {
    print -u2 -- 'Claude Code was started despite the adapter failing'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# A profile declares the model-behaviour env it wants. Anything the surrounding
# shell happens to export must not reach a profile that did not ask for it, or one
# terminal's experiment silently changes another profile's compaction behaviour.
test_shell_env_does_not_leak_into_a_profile() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway
  write_apikey_conf plain 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local rc=0
  CLAUDE_CODE_MAX_CONTEXT_TOKENS=999999 \
  CLAUDE_CODE_AUTO_COMPACT_WINDOW=888888 \
  CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=77 \
  ANTHROPIC_CUSTOM_MODEL_OPTION=claude-leaked-from-the-shell \
    _ccm_launch -a plain prompt >/dev/null 2>&1 || rc=1

  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=<unset>' \
    'a shell-exported context limit must not reach a profile that did not set it' || rc=1
  assert_contains "$actual" 'PICKER_ROW=<unset>' \
    'a shell-exported picker row must not reach a profile that pins none' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A gateway that publishes context windows, which is how the compaction window is
# derived instead of typed. Records the invocation so a test can assert the header:
# max_input_tokens is only returned on the Anthropic-shaped route, and only when
# anthropic-version is sent -- without it the same endpoint answers four bare fields.
fake_gateway_with_windows() {
  printf '%s\n' \
    '#!/bin/sh' \
    'for a do printf "%s\n" "$a" >> "$HOME/curl.args"; done' \
    '[ "${1-}" = -K ] && cat >> "$HOME/curl.args"' \
    'cat <<JSON' \
    '{"object":"list","data":[' \
    '{"id":"claude-fable-5-dd-los-6.5-tpg","display_name":"GPT 5.6 Sol","max_input_tokens":372000},' \
    '{"id":"plain-model","display_name":"Plain","max_input_tokens":131072},' \
    '{"id":"claude-fable-5-dd-2-egami-tpg","display_name":"GPT Image 2","max_input_tokens":200000}' \
    ']}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
}

test_a_route_scoped_probe_beats_the_endpoint_declaration() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/derived.conf"
  write_profile_context derived gpt-5.6-sol=999999
  # A profile with no context data of its own: launch uses live metadata,
  # never another profile's stored window (no cross-profile borrowing).
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/unscoped.conf"
  write_profile_context unscoped gpt-5.6-sol=888888
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a derived prompt || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  # The endpoint disguises non-Claude ids as claude-fable-5-dd-<reversed>, so the
  # record for gpt-5.6-sol arrives under a name the profile never mentions. Matching
  # has to undo that or every routed profile silently finds nothing.
  # Canonical order (5dbef40, and the model-window resolver's rungs): a probed
  # ceiling stored on the profile beats the endpoint's declaration, which may be an
  # invented default (the codex route declared 272000 for a model probed at 543000).
  assert_contains "$actual" 'MAX_CONTEXT=999999' \
    'a stored ceiling must beat the endpoint declaration' || rc=1
  assert_not_contains "$actual" 'MAX_CONTEXT=372000' \
    'a declaration must not overrule the stored window' || rc=1

  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a unscoped prompt || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=888888' \
    'each profile must use its own stored window' || rc=1
  assert_not_contains "$actual" 'MAX_CONTEXT=372000' \
    'live metadata must not override known registry state during launch' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_conf_window_beats_the_published_one() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=gpt-5.6-sol' \
    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=120000' \
    > "$HOME/.config/ccmorph/pinned.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a pinned prompt || { rm -rf "$test_home"; return 1; }

  # An author who pins a ceiling has a reason the endpoint cannot know -- a smaller
  # budget on purpose, or an endpoint that over-declares. Derivation fills a gap; it
  # does not overrule.
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=120000' \
    'an explicit conf ceiling must win over the published one' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_endpoint_publishing_no_window_changes_nothing() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway   # publishes ids and capabilities, no max_input_tokens
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=alpha-chat' \
    > "$HOME/.config/ccmorph/silent.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a silent prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }

  # A gateway may publish no window for any model it serves, so this is the
  # common case, not an edge one. It must leave the launch exactly as it found it.
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=<unset>' \
    'an endpoint that publishes no window must not invent one' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_registry_supplies_window_when_endpoint_probe_has_none() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do' \
    '  if [ "$arg" = "-w" ]; then printf "200"; exit 0; fi' \
    'done' \
    'exit 22' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=context-model' \
    > "$HOME/.config/ccmorph/routed.conf"
  write_profile_context routed context-model=1048576
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a routed prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=1048576' \
    'a stored context window must supply the missing endpoint window' || rc=1
  # 1048576 exceeds the setting's schema cap (1M); an over-cap value is dropped
  # by Claude Code entirely, so it must be clamped, not passed through.
  assert_contains "$actual" '"autoCompactWindow":1000000' \
    'the stored window must set the auto-compact budget, clamped to the schema cap' || rc=1
  assert_contains "$actual" 'AUTO_COMPACT=<unset>' \
    'no env auto-compact pin: it would shadow the settings layer and /autocompact' || rc=1

  # A profile with no context data of its own never borrows another profile's
  # stored window (no cross-profile borrowing) -- it falls through to the
  # endpoint probe / default like any other unprobed profile.
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=context-model' \
    > "$HOME/.config/ccmorph/other.conf"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a other prompt >/dev/null 2>&1 || rc=1
  assert_not_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=1048576' \
    'a profile with no stored data must never inherit another profile''s window' || rc=1
  rm -rf "$test_home"
  return $rc
}

# KSH_ARRAYS indexes arrays from 0, so `a[1]` is the SECOND element while the
# library's every subscript assumes zsh's own 1-based arrays. _ccm_launch's body
# walks args, penv, the window record and the route defaults that way, so a
# caller's KSH_ARRAYS used to skip the first argument and lose the published
# window. The option is a property of the sourcing shell, invisible to a default
# run, so the same launch runs twice here and both must agree.
test_launch_reads_its_arrays_under_a_callers_ksh_arrays() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do' \
    '  if [ "$arg" = "-w" ]; then printf "200"; exit 0; fi' \
    'done' \
    'exit 22' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=context-model' \
    > "$HOME/.config/ccmorph/routed.conf"
  write_profile_context routed context-model=1048576
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a routed --model context-model prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }
  local want_arg="$(grep '^ARG' "$CCMORPH_TEST_LOG")"
  assert_contains "$want_arg" 'context-model' \
    'sanity: the default shell keeps the launch arguments' || rc=1

  : > "$CCMORPH_TEST_LOG"
  local ksh_rc=0
  ( setopt ksharrays
    _ccm_launch -a routed --model context-model prompt >/dev/null 2>&1 || exit 1
    [[ -o ksharrays ]] || exit 13 ) || ksh_rc=$?
  (( ksh_rc == 0 )) || {
    print -u2 -- "the launch must not fail under a caller's KSH_ARRAYS (exit $ksh_rc)"
    rc=1
  }
  local ksh_arg="$(grep '^ARG' "$CCMORPH_TEST_LOG")"
  [[ "$ksh_arg" == "$want_arg" ]] || {
    print -u2 -- "KSH_ARRAYS must not shift the launch's own arguments\n  default: ${(qqq)want_arg}\n  ksharrays: ${(qqq)ksh_arg}"
    rc=1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=1048576' \
    'the stored window must still supply the launch under KSH_ARRAYS' || rc=1
  assert_contains "$actual" 'AUTO_COMPACT=1000000' \
    'the auto-compact budget from the stored window must survive KSH_ARRAYS' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A mid-session /model switch cannot change CLAUDE_CODE_MAX_CONTEXT_TOKENS (env is
# fixed at process start), so the env must already cover the LARGEST model the route
# serves -- or a switch to it runs clamped at the launched model's window. The
# launched model's own budget is then narrowed per model by autoCompactWindow.
test_the_window_env_covers_every_model_the_route_serves() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=small-model' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small-model=200000 big-model=900000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["big-model","small-model"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=900000' \
    'the env ceiling must cover the largest model the route serves, not just the launched one' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The per-model budget travels as the NATIVE autoCompactWindow setting, not as
# CLAUDE_CODE_AUTO_COMPACT_WINDOW env: the binary checks the env var before the
# settings layer on every resolution, so an env pin silently disables the user's
# own /autocompact command for the whole session.
test_the_launched_model_budgets_through_the_native_setting() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=small-model' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw big-model=900000 small-model=300000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["big-model","small-model"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"autoCompactWindow":300000' \
    'the launched model must budget through the native autoCompactWindow setting' || rc=1
  assert_contains "$actual" 'AUTO_COMPACT=<unset>' \
    'the env auto-compact pin must go: it shadows the settings layer and /autocompact' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A profile that maps a 1M id onto a smaller model must budget against the model
# the shim serves. The gateway rewrite is final: `_resolve` replaces the body's
# model with the map's target before the request leaves, so a session sized
# against the requested id runs to the source's 1,000,000 trigger behind the
# target's 128,000 wall -- past the wall, with nothing left to compact into. The
# client-facing identity stays the requested id (#285); only the window moves.
test_a_mapped_alias_budgets_against_its_target() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=claude-opus-5' \
    'model_map={"claude-opus-5":"small-target"}' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_discovered gw claude-opus-5=1000000 small-target=128000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["claude-opus-5","small-target"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"autoCompactWindow":128000' \
    'a mapped session must budget against the model the shim serves, not the id it asked for' || rc=1
  assert_not_contains "$actual" '"autoCompactWindow":1000000' \
    'the source 1M budget must never sit behind the target wall' || rc=1
  assert_contains "$actual" 'MODEL_ENV=claude-opus-5' \
    'the client-facing identity stays the requested id' || rc=1
  assert_not_contains "$actual" 'MODEL_ENV=claude-opus-5[1m]' \
    'the [1m] marker belongs to a window this session does not have' || rc=1
  assert_not_contains "$actual" 'claude-opus-5[1m]' \
    'no marker for the source window may survive anywhere the launch writes' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The models a launch actually offers, read from the overlay the stub recorded.
#
# The stub prints the overlay's CONTENT as `SETTINGS={...}` because the temp file
# is unlinked when the launcher returns -- so the log is the only surviving copy
# of what this session would have read.
_ccm_offered_models() {
  print -r -- "$1" | command python3 -c '
import json, sys

raw = sys.stdin.read()
marker = "SETTINGS="
i = raw.find(marker)
if i == -1:
    raise SystemExit(0)
decoder = json.JSONDecoder()
try:
    doc, _ = decoder.raw_decode(raw[i + len(marker):].lstrip())
except ValueError:
    raise SystemExit(0)
models = doc.get("availableModels") or []
print(",".join(m for m in models if isinstance(m, str)))
' 2>/dev/null
}

# THE failure this mechanism exists to prevent, stated end to end.
#
# The compaction trigger is fixed when the session starts: the engine re-reads no
# settings mid-session, and only the user's own /autocompact moves it in process.
# So a /model switch DOWN to a model whose wall sits under this session's trigger
# can never compact -- it runs into the gateway's over-limit error, and by then the
# transcript is too large for /compact to summarize either. The session is dead,
# not degraded.
#
# The fix keeps each model's own budget (a big model is NOT dragged down to the
# route's smallest wall). Removing the unusable models from the picker is the
# OPT-IN response (CCMORPH_WITHHOLD_FATAL=1): through a gateway there is exactly
# one auto-compact window per session -- the engine's per-model map is gated on a
# first-party connection and reads null here -- so making a small model safe costs
# every large model in the session. Hiding a model the user chose deliberately
# takes away a reviewer they wanted, so by default they are told the exact token
# count instead and decide for themselves. Withholding, when asked for, edits the
# launch OVERLAY only.
test_a_session_never_offers_a_model_it_cannot_compact() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # A route spanning 5x: launching on the largest model puts the trigger at
  # 800,000, far above tiny/small's walls.
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=huge' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw tiny=200000 small=272000 huge=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["tiny","small","huge"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  CCMORPH_WITHHOLD_FATAL=1 \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  # The launched model keeps its OWN budget -- the whole point of per-model
  # windows. 1,000,000 * 80% = 800,000.
  # The overlay is pretty-printed when it is rewritten (withholding rewrites it),
  # so match the value, not one spelling of the separator.
  assert_contains "${actual//\"autoCompactWindow\": /\"autoCompactWindow\":}" \
    '"autoCompactWindow":1000000' \
    'the launched model must keep its own budget, not the route floor' || rc=1

  # And the models that budget cannot protect are not offered. The stub records
  # the overlay's CONTENT as SETTINGS=..., which is what this session will read;
  # the temp file itself is gone by the time the launcher returns.
  local offered
  offered=$(_ccm_offered_models "$actual")

  assert_contains "$offered" 'huge' \
    'the running model must stay in the picker' || rc=1
  assert_not_contains "$offered" 'tiny' \
    'a model whose wall is under the trigger must be withheld' || rc=1
  assert_not_contains "$offered" 'small' \
    'every model under the trigger must be withheld, not just the smallest' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The mirror case: launched on the SMALLEST model, the trigger is low enough that
# every sibling can compact -- so nothing is withheld. A guard that always fires
# would just be a smaller picker, not a safer session.
test_a_small_launch_still_offers_the_whole_route() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=tiny' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw tiny=200000 small=272000 huge=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["tiny","small","huge"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"autoCompactWindow":200000' \
    'the small launch budgets from its own wall' || rc=1

  local offered
  offered=$(_ccm_offered_models "$actual")
  local _m
  for _m in tiny small huge; do
    assert_contains "$offered" "$_m" \
      "a 160k trigger clears every wall, so $_m must stay offered" || rc=1
  done
  rm -rf "$test_home"
  return $rc
}

# The profile is the profile's, not one session's: withholding lives in the
# overlay so tomorrow's launch on a smaller model still sees every model.
test_withholding_never_edits_the_profile() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=huge' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw tiny=200000 huge=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["tiny","huge"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  CCMORPH_WITHHOLD_FATAL=1 \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local profile_models
  profile_models=$(command python3 -c '
import json, sys
print(",".join(json.load(open(sys.argv[1])).get("availableModels") or []))' \
    "$HOME/.config/ccmorph/profiles/gw/settings.profile.json" 2>/dev/null)
  assert_contains "$profile_models" 'tiny' \
    'the profile must keep every model the profile can reach' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The DEFAULT, and the reason it is the default: hiding a model the user picked on
# purpose costs them a reviewer they wanted, and through a gateway the only
# alternative -- one budget small enough for the smallest model -- taxes every
# large model in the session. So by default every model stays selectable and the
# advisory names the exact token count past which each one cannot compact. The
# number is the protection: below the trigger every model is safe, and the user is
# the one who knows whether this session will grow past it.
test_by_default_every_model_stays_selectable_and_is_named() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=huge' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw tiny=200000 small=272000 huge=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["tiny","small","huge"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  local offered
  offered=$(_ccm_offered_models "$actual")
  local _m
  for _m in tiny small huge; do
    assert_contains "$offered" "$_m" \
      "no model may be hidden unless the user asked: $_m must stay offered" || rc=1
  done

  # And the advisory has to carry the number, or the user is protected by nothing.
  assert_contains "$actual" 'limit 200k' \
    'the advisory must name the token count where a risky model stops compacting' || rc=1
  assert_contains "$actual" 'limit 272k' \
    'every risky model gets its own number, not one lumped warning' || rc=1
  rm -rf "$test_home"
  return $rc
}

# /autocompact persists the user's choice into the profile settings.json.
# Derivation fills a gap, it never overrules a person.
test_a_users_saved_autocompact_choice_survives_derivation() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=small-model' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small-model=300000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"autoCompactWindow":123456}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"autoCompactWindow":123456' \
    'a /autocompact choice saved in the profile settings must beat the derived value' || rc=1
  assert_not_contains "$actual" '"autoCompactWindow":300000' \
    'the derived budget must not overrule the saved one' || rc=1
  rm -rf "$test_home"
  return $rc
}

# GH-307: a shared policy's tombstones belong to profile layers and are dropped
# when the settings are composed, so a reader that applied them would see a
# different document from the one Claude runs with -- here the user's canonical
# budget vanished from the launcher's view while the generated settings kept it,
# and the launch then overruled the user's choice with the derived budget.
test_a_policy_tombstone_does_not_hide_a_canonical_budget() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"autoCompactWindow":400000}' > "$HOME/.claude/settings.json"
  printf '%s\n' '{"$ccmorphUnset":[["autoCompactWindow"]]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.policy.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  # --model roomy is the shape that carries a per-session budget: it is injected
  # into the environment, which the engine reads BEFORE the settings layer.
  _ccm_launch -a gw --model roomy prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "${actual//\"autoCompactWindow\": /\"autoCompactWindow\":}" \
    '"autoCompactWindow":400000' \
    'the generated settings must keep the canonical budget the policy only tombstones' || rc=1
  assert_contains "$actual" 'AUTO_COMPACT=<unset>' \
    'the saved budget must reach the launcher too, so no derived budget overrides it' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A profile whose default is a claude id used to skip derivation entirely, so the
# very first /model switch to a routed model ran at Claude Code's 200,000. The
# route ceiling must be sized from what the route SERVES, not from what happened
# to be launched.
test_a_claude_default_still_sizes_the_route_for_switches() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # The claude id carries its own probed entry: its budget is the model's own
  # 1M, while the route ceiling stays at the largest routed model (500,000).
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=claude-opus-4-6' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw routed-a=500000 claude-opus-4-6=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["routed-a","claude-opus-4-6[1m]"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=500000' \
    'a claude default must still size the route for mid-session switches' || rc=1
  # A claude-launched session must ALSO carry its own budget. Leaving it off is
  # what wedged a real session: with no autoCompactWindow the window resolves
  # source:"auto" against the ROUTE ceiling, so the trigger lands above the
  # launched model's own wall and auto-compact never fires -- the session runs to
  # the gateway's hard over-limit error, by which point /compact cannot fit
  # either. claude-opus-4-6 is probed at 1M, and the value is the model's, not
  # the route's 500,000.
  assert_contains "$actual" '"autoCompactWindow":1000000' \
    'a claude-launched session must budget from its own probed window' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A tier alias is not a model and has no window of its own: Claude Code resolves
# it through the profile's ANTHROPIC_DEFAULT_<TIER>_MODEL. Sizing the budget from
# the bare alias finds nothing in the registry, which is the same silent
# no-budget launch as the claude case above.
test_a_tier_alias_budgets_from_the_model_behind_it() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=fable' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw routed-big=700000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' \
    '{"availableModels":["routed-big"],"env":{"ANTHROPIC_DEFAULT_FABLE_MODEL":"routed-big"}}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"autoCompactWindow":700000' \
    'an alias launch must budget from the model the alias resolves to' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The wedge, stated as an invariant: the budget must never exceed the launched
# model's own wall. When it does, the trigger sits above the wall, auto-compact
# never fires, and the session dies on the gateway's over-limit error with a
# transcript too large for /compact to summarize. A route whose ceiling is larger
# than the launched model is exactly the shape that produced it.
test_the_budget_never_exceeds_the_launched_models_wall() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=small-model' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small-model=200000 huge-model=1000000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small-model","huge-model"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  # The ceiling covers the route (so a switch up is not clamped)...
  assert_contains "$actual" 'MAX_CONTEXT=1000000' \
    'the env ceiling must still cover the largest model the route serves' || rc=1
  # ...while the budget stays on the launched model's own wall.
  assert_contains "$actual" '"autoCompactWindow":200000' \
    'the budget must track the launched model, never the route ceiling' || rc=1
  assert_not_contains "$actual" '"autoCompactWindow":1000000' \
    'a budget at the route ceiling is the never-compacts wedge' || rc=1
  rm -rf "$test_home"
  return $rc
}

# When the conf names no model, Claude Code runs the /model choice saved in the
# profile settings -- so the window must be derived from that same model, or the
# session launches sized for a model it is not running.
test_the_window_follows_the_saved_default_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=sk_prod_test' 'base_url=https://gateway.invalid'
  write_profile_context gw saved-model=500000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"model":"saved-model"}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=500000' \
    'the saved /model default must drive the window when the conf names no model' || rc=1
  assert_contains "$actual" '"autoCompactWindow":500000' \
    'the saved default model must also set its own auto-compact budget' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_model_flag_drives_the_window_not_the_conf_default() {
  # THE ten-times bug: `claude -a <profile> --model routed-xl` on a profile whose
  # conf default is a claude id (or any other model) sized the window from the
  # DEFAULT, not the model actually launched. When the default was a claude id it
  # was blanked and derivation was skipped entirely, so the launched non-claude
  # model got no window and stuck at Claude Code's 200000. The --model flag must
  # drive the window.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'for arg do if [ "$arg" = "-w" ]; then printf "200"; exit 0; fi; done' \
    'exit 22' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  # Conf default is a claude id, exactly like the real gateway profiles.
  printf '%s\n' \
    'type=apikey' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=claude-opus-4-8[1m]' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw routed-xl=1048576
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw --model routed-xl prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=1048576' \
    'the --model flag, not the conf default, must size the launch window' || rc=1
  assert_not_contains "$actual" 'MAX_CONTEXT=200000' \
    'a launched non-claude model must never be left to Claude Code'"'"'s 200000 default' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_a_flat_endpoint_declaration_yields_to_probe_at_launch() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # An adapter=true profile fronts the gateway with a local translator, and some
  # translators declare one flat value for every model they proxy -- an adapter
  # that reports 200000 for a large-window model and every sibling alike. Taking the
  # lower of that and a probe (the launch derivation's min()) hides the real
  # ceiling behind the invented default: a session that truly holds 1048576 was
  # capped and compacted at 200000. When a value repeats across every served model
  # it is a default, not a probe, so the endpoint probe must return nothing
  # and let the registry answer per model. Mirror of the picker's flat-default
  # handling, which lived only in _ccm_project_picker until this path grew it too.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[' \
    '{"id":"routed-xl","max_input_tokens":200000},' \
    '{"id":"qwen3.8-max","max_input_tokens":200000},' \
    '{"id":"glm-5.2","max_input_tokens":200000}' \
    ']}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=routed-xl' \
    > "$HOME/.config/ccmorph/flatgw.conf"
  write_profile_context flatgw routed-xl=1048576
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a flatgw prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=1048576' \
    'a flat endpoint declaration must yield to the probed registry ceiling at launch' || rc=1
  assert_not_contains "$actual" 'MAX_CONTEXT=200000' \
    'the invented flat default must never become the injected window' || rc=1
  rm -rf "$test_home"
  return $rc
}


test_a_genuine_shared_window_is_not_mistaken_for_flat() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # The inverse guard: two models legitimately sharing a window is not a flat
  # default, and a single-model endpoint has nothing to compare against. Flatness
  # requires more than one distinct id AND every declared value identical. Here the
  # target's value differs from its sibling, so the endpoint's own number stands.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[' \
    '{"id":"routed-xl","max_input_tokens":1048576},' \
    '{"id":"qwen3.8-max","max_input_tokens":983616}' \
    ']}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=routed-xl' \
    > "$HOME/.config/ccmorph/realgw.conf"
  write_profile_context realgw routed-xl=1048576
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a realgw prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=1048576' \
    'a non-flat endpoint declaration must still supply the window' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_a_claude_id_keeps_claude_codes_own_window() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # The gateway adapter really does declare 200000 for claude-opus-4-8, a model
  # probed at 935,511 through that same gateway. Claude Code reaches 1M for a
  # claude- id via the [1m] suffix, so honouring the declaration would have cut the
  # window by five times -- derivation must not overrule a path Claude Code owns.
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"data":[{"id":"claude-opus-4-8","display_name":"claude-opus-4-8","max_input_tokens":200000}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  printf '%s\n' \
    'type=apikey' \
    'base_url=https://gateway.invalid' \
    'api_key=sk_prod_test' \
    'model=claude-opus-4-8[1m]' \
    > "$HOME/.config/ccmorph/gwclaude.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a gwclaude prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }

  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=<unset>' \
    'a claude- id must keep Claude Code own window, not an endpoint declaration' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_a_derived_window_survives_an_endpoint_blip() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/blip.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  # A previous metadata refresh recorded the discovered window in the profile's
  # own conf. The launch must remember it locally, so even an unreachable
  # endpoint later cannot shrink it.
  write_profile_discovered blip gpt-5.6-sol=372000
  _ccm_launch -a blip prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=372000' \
    'the first launch must use the recorded discovered window' || rc=1

  # Now the endpoint is unreachable. Without the remembered value the window falls back
  # to Claude Code's 200,000, which moves the compaction trigger from 299,200 to
  # 153,000 -- a session that silently loses two fifths of its context because one
  # request timed out, with nothing said. A ceiling does not change between launches;
  # reachability does.
  printf '%s\n' '#!/bin/sh' 'exit 7' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a blip prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }

  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=372000' \
    'an unreachable endpoint must reuse the last known window, not shrink it' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_warns_when_a_model_refuses_before_compacting() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # An endpoint with nothing projectable, so the hand-written allowlist below survives
  # to be checked. A profile the projection does own cannot reach this state: it filters
  # a sub-trigger model out before the check runs.
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  mkdir -p "$HOME/.config/ccmorph/profiles/mixed"
  # A cloaked id gets Claude Code's 200,000 window, so its trigger is 153,000. The
  # first model here holds less than that and will refuse mid-session instead of
  # compacting; the second holds more and is fine. Nothing in Claude Code knows the
  # difference, because it cannot see a routed model's real ceiling.
  print -r -- '{"availableModels":["claude-fable-5-dd-llams","claude-fable-5-dd-gib"]}' \
    > "$HOME/.config/ccmorph/profiles/mixed/settings.json"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/mixed.conf"
  # The launched model is sized at 200,000 so its trigger (153,000) is the one under
  # test; an unsized launch would budget conservatively and endanger nothing.
  write_profile_context mixed small=131072 big=262144 gpt-5.6-sol=200000
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(        _ccm_launch -a mixed prompt 2>&1 >/dev/null) || {
    rm -rf "$test_home"
    return 1
  }

  assert_contains "$err" 'small' \
    'a model whose ceiling is under the compaction trigger must be named' || rc=1
  assert_contains "$err" '131072' \
    'the warning must state the ceiling that makes it unsafe' || rc=1
  assert_not_contains "$err" 'big' \
    'a model with headroom must not be reported' || rc=1
  rm -rf "$test_home"
  return $rc
}

# This session's compaction trigger is fixed at launch, so a mid-session switch
# DOWN to a model whose wall sits under that trigger runs past its limit instead
# of compacting. No hook fires on /model, so launch is the only place to say it.
test_launch_names_switch_targets_the_trigger_would_overrun() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  # Launch on the 1M model: trigger = 80% of 1000000 = 800000. `narrow` (700k)
  # and `tiny` (262k) sit under it; `wide` does not.
  printf '%s\n' 'roomy=1000000' 'narrow=700000' 'tiny=262144' 'wide=1048576' \
    > "$HOME/registry.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/spread"
  print -r -- '{"availableModels":["roomy","narrow","tiny","wide"]}' \
    > "$HOME/.config/ccmorph/profiles/spread/settings.json"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=roomy' \
    > "$HOME/.config/ccmorph/spread.conf"
  write_profile_context spread roomy=1000000 narrow=700000 tiny=262144 wide=1048576
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a spread prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }

  # The warning travels in the launch overlay as a SessionStart hook, NOT as
  # pre-exec stderr: Claude Code takes 5+ seconds to start and then paints its
  # alternate screen over anything written before it, so a stderr line flashes
  # for a fraction of a second and warns nobody. A hook's `systemMessage`
  # renders INSIDE the TUI and stays on screen.
  local actual="$(<"$CCMORPH_TEST_LOG")"
  # 784k, not 800k: the trigger is Claude Code's own, which deducts a 20,000-token
  # output reserve BEFORE applying the percentage (and two further caps). A plain
  # budget*pct/100 overstates it by a constant 16,000 at every window size, which
  # made this warning name a number the engine never uses -- and falsely flag a
  # model the engine would in fact have compacted.
  assert_contains "$actual" 'compacts at 784k' \
    'the warning must lead with the trigger, the number that explains every failure' || rc=1
  assert_contains "$actual" 'narrow' \
    'a switch target under the trigger must be named' || rc=1
  assert_contains "$actual" 'tiny' \
    'every unsafe switch target must be named' || rc=1
  assert_not_contains "$actual" 'wide (' \
    'a switch target with headroom must not be named' || rc=1
  assert_contains "$actual" 'systemMessage' \
    'the warning must ride a hook systemMessage, which persists in the TUI' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The trigger ccmorph reports must be the one Claude Code actually uses, or the
# advisory lies in both directions: it names a number the engine never applies, and
# it calls a model fatal that the engine would have compacted safely.
#
# Claude Code's own arithmetic (2.1.247) deducts an output reserve BEFORE the
# percentage, then applies two further caps:
#
#   usable  = window - min(window, 20000)
#   trigger = min(usable - round(usable * 0.2),
#                 min(usable * pct / 100, usable - 13000))
#
# A plain `window * pct / 100` overstates that by a constant 16,000 at every window
# size. This test pins the boundary case the drift produced: a 200,000 launch that
# offers a 150,000 model. The engine compacts at 144,000, so the 150,000 model is
# SAFE and must not be named; the old formula computed 160,000 and named it.
# The advisory and the withhold must agree, because they are two halves of one
# promise: a model is either named or removed, never removed unnamed. They agree
# only if both compute from the SAME budget -- and the effective budget is not the
# derived one whenever the user has saved an autoCompactWindow or a conf pins the
# env var, since the overlay applies the derivation with setdefault semantics.
#
# The disagreement this pins: a saved 300,000 budget with a 1,000,000 launch. The
# real trigger is ~224,000, which a 262,144 model clears -- so nothing should be
# named and nothing removed. Passing the derived 1,000,000 to the withhold alone
# made it compute ~784,000 and silently drop that safe model.
# A profile that pins the ceiling in its own conf skips ccmorph's derivation
# branch entirely -- but the engine does not stop resolving a window, and does not
# stop refusing to compact a model whose wall is under the trigger. So the hazard
# path must run whether or not ccmorph did the deriving, and must use the budget
# the engine would use: the env pin if present, else a saved autoCompactWindow.
#
# The case: conf pins the ceiling at 1,000,000, canon settings save
# autoCompactWindow 1,000,000, and the route offers a 400,000 model. The engine
# triggers at 784,000 -- above that model's wall, so it is fatal and must be named.
# Resolving no budget outside the branch left this session silent.
# The trigger must track CLAUDE_AUTOCOMPACT_PCT_OVERRIDE, because the engine's
# compact threshold IS that percentage of the usable window (capped 13,000 below
# it) -- and nothing else. ccmorph briefly added the precompute-arming buffer term
# on top, which is a different threshold behind a different gate; the two agree
# exactly at pct=80 and nowhere else, so only a non-default percentage can catch it.
#
# At pct=90 on a 1,000,000 window: usable 980,000, engine trigger 882,000. A model
# walled at 800,000 is therefore FATAL and must be named. The buffered formula gave
# 784,000, judged 800,000 safe, and printed nothing.
#
# The value is `90junk`, not `90`, deliberately: the engine reads this with parseFloat
# and takes the numeric prefix, so it means 90 there. A plain `90` would leave this
# test green if the prefix handling were reverted to whole-string float(), which
# rejects the value and silently drops a live override.
# The engine computes the percentage as Math.floor(e*(r/100)) -- it DIVIDES before
# multiplying. Floating-point multiplication is not associative, so `usable*pct/100`
# lands one token high on real windows, and a model walled exactly AT the engine's
# trigger then reads as fatal when the engine would still compact it.
#
# Probed, not assumed. Window 120,000, usable 100,000, pct=29:
#
#   engine  floor(100000*(29/100)) = 28999  -> wall 28999 < 28999 is False -> SAFE
#   old     int(100000*29/100)     = 29000  -> wall 28999 < 29000 is True  -> FATAL
#
# Both round to "29k", so the trigger TEXT cannot show this. The observable
# difference is whether the sibling is named at all -- which is exactly the
# user-visible cost: named for no reason by default, and with
# CCMORPH_WITHHOLD_FATAL=1 dropped from the picker entirely.
test_the_percentage_divides_before_it_multiplies() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    'env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=29' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=120000 exactly=28999
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["roomy","exactly"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  assert_not_contains "$actual" 'exactly limit' \
    'a model walled exactly at the engine trigger is safe, so it must not be named' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_the_trigger_follows_a_non_default_percentage() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    'env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=90junk' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000 eighthundred=800000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["roomy","eighthundred"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  assert_contains "$actual" 'compacts at 882k' \
    'the trigger must be the percentage of the usable window, not a buffered form' || rc=1
  assert_contains "$actual" 'eighthundred limit 800k' \
    'a model between the two formulas must be named fatal' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A saved autoCompactWindow the SCHEMA rejects must not suppress the derivation.
# The setting is `.int().min(1e5).max(1e6).optional().catch(void 0)`, so the engine
# discards an out-of-range value entirely -- and gating the overlay on key presence
# meant a stale 50,000 both blocked ccmorph from deriving AND gave the engine
# nothing, leaving the session sized by the route ceiling with a trigger above the
# launched model wall. That is the never-compacts failure this feature exists to
# prevent, reached through the feature's own overlay.
# The engine's usable window is `window - min(cV(model), 20000)`, and cV() reads
# CLAUDE_CODE_MAX_OUTPUT_TOKENS -- so a value below 20,000 makes it reserve LESS and
# trigger HIGHER. ccmorph must see the same number, which means two things have to
# hold: the key must be in the profile-env allowlist (or it never reaches the launched
# process, and the engine never sees it either), and the hazard path must read it from
# that same launch env. A unit test on the hazard helper alone would pass even with
# the key absent from the allowlist, which is exactly how that gap shipped.
#
# 8192 reserve on a 200,000 window: usable 191,808, engine trigger 153,446. The
# 150,000-wall sibling is therefore FATAL. With the key stripped, the reserve stays
# 20,000, the trigger is 144,000, and the sibling is judged safe and never named.
# Env values must be read the way the engine reads them. It parses these through
# `P(trim(v)) ?? parseInt(trim(v),10)`, so `1e5`, `100_000` and `100000junk` are all
# valid budgets there -- and a pure-digit test here rejected them, fell through to a
# different budget, and reported a trigger the session does not use. With withholding
# enabled that removed a model the engine would have compacted without trouble.
#
# `1e5` pin on a 400,000 launch: the engine reads 100,000, so the trigger is 67,000
# and the 300,000-wall sibling is SAFE. Under a pure-digit gate the pin is dropped,
# the derived 400,000 governs, the trigger is 307,000, and the sibling is named fatal.
# Both env parsers must reject non-ASCII digits, because JavaScript does. Python's
# character class and int()/float() accept Arabic-Indic digits; the engine's \d,
# parseInt and parseFloat do not, so it calls such a value invalid and keeps its own
# default. Accepting one here computes a trigger the session never uses.
#
# BOTH parsers in one test, one launch each: the repeated failure in this feature has
# been to fix one path and leave its neighbour, which a single-path test cannot see.
# The percentage parser was missed exactly that way after the integer one was fixed.
# An env value the engine treats as invalid must come back EMPTY and SUCCESSFUL, never
# as a failure. `1e999` matches the exponent form, float() gives inf, and int(inf)
# raises -- which made the helper exit non-zero. Every reader spells the call
# `$(_ccm_engine_int ...) || fallback`, so a non-zero exit was swallowed and each fell
# through to a different budget; on a profile pinning the ceiling in its conf that meant
# no hazard analysis at all.
#
# Asserted at the helper, because the defect WAS the exit status: that covers every
# reader at once. Two earlier attempts in this series to test an equivalent fault
# through one downstream symptom were blind.
# Whitespace must be ECMAScript whitespace, not Python whitespace. str.strip() and the
# Python \s class also remove U+0085 (NEL); the engine's trim does not -- its WhiteSpace
# production is TAB, VT, FF, SP, NBSP, ZWNBSP plus Unicode Zs, and NEL is category Cc.
# A leading NEL was therefore stripped here and kept there, so parseInt failed, the
# engine called the value invalid, and ccmorph computed a trigger from a budget the
# session never had.
#
# Both directions and both parsers: rejecting NEL is the fix, and still accepting
# SP/TAB/NBSP is what stops the fix overshooting into rejecting whitespace the engine
# genuinely skips. A test that only checked the rejection would miss that.
# The percentage parser keeps its own copy of the whitespace class and cannot be called
# in isolation, so it needs a launch-level test -- and the two cases must expect OPPOSITE
# outcomes, or the test cannot tell "rejected" from "accepted".
#
# Probed on a 200,000-token window (usable 180,000, xWe cap usable-13,000 = 167,000):
#
#   no override -> trigger 167,000 -> a 150,000-wall sibling is FATAL
#   50%         -> trigger  90,000 -> that sibling is SAFE
#
#   NEL + 50   the engine keeps NEL, so the override is REJECTED: trigger 167,000, the
#              sibling is named. Wrongly accepting it as 50% gives 90,000 and silence.
#   NBSP + 50  the engine skips NBSP, so the override GOVERNS: trigger 90,000 and
#              silence. An over-tight class rejects it, giving 167,000 and a false name.
#
# Written because this fix first shipped with the percentage half unguarded: a draft
# stanza that asserted nothing was removed and never replaced, so reverting only this
# parser left the helper-level test green.
# U+FEFF is the ONE ECMAScript whitespace char Python float() rejects. Every other
# char in the class -- NBSP, U+1680, U+2000-200A, 2028/2029, 202F, 205F, 3000 -- is
# strippable by float() itself, which is exactly why the NBSP test above stays green
# whether or not the whitespace is stripped before parsing. So FEFF is the only value
# that pins the seam.
#
# Probed on a 200,000-token window (usable 180,000, default cap 167,000):
#
#   engine  parseFloat("\ufeff50") = 50   -> trigger  90,000 -> 150k sibling SAFE
#   folded  float("\ufeff50") raises      -> pct dropped     -> trigger 167,000
#                                          -> 150k sibling falsely named FATAL
test_the_percentage_parser_skips_a_zero_width_no_break_space() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small","onefifty"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 feff
  feff=$(printf '\ufeff')
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=small' \
    "env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=${feff}50" \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 onefifty=150000
  : > "$CCMORPH_TEST_LOG"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_not_contains "$actual" 'onefifty limit' \
    'a FEFF-prefixed percentage is engine whitespace and must govern' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Multibyte whitespace is what a UTF-8 terminal exercises; the VM defaults to POSIX.
# Prints an installed UTF-8 locale name; callers scope it with `local -x LC_ALL=...`
# because run_test runs every test in one shell.
_utf8_locale_name() {
  local l
  for l in C.UTF-8 C.utf8 en_US.UTF-8 en_US.utf8; do
    if locale -a 2>/dev/null | grep -qx "$l"; then
      print -r -- "$l"; return 0
    fi
  done
  return 0
}

test_the_percentage_parser_uses_engine_whitespace() {
  local -x LC_ALL="$(_utf8_locale_name)"
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small","onefifty"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 nel nbsp actual
  nel=$'\xc2\x85'
  nbsp=$'\xc2\xa0'

  # NEL is not engine whitespace: the override is rejected and the default cap governs.
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=small' \
    "env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=${nel}50" \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 onefifty=150000
  : > "$CCMORPH_TEST_LOG"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'onefifty limit 150k' \
    'a NEL-prefixed percentage must be rejected, leaving the 167k cap in force' || rc=1

  # NBSP IS engine whitespace: the override governs and lowers the trigger.
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=small' \
    "env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=${nbsp}50" \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 onefifty=150000
  : > "$CCMORPH_TEST_LOG"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_not_contains "$actual" 'onefifty limit' \
    'an NBSP-prefixed percentage is engine whitespace and must govern' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_whitespace_handling_matches_the_engine() {
  local -x LC_ALL="$(_utf8_locale_name)"
  local rc=0 out=""
  source "$CCMORPH_SOURCE"

  local nel tab nbsp
  nel=$'\xc2\x85'
  tab=$(printf '\t')
  nbsp=$'\xc2\xa0'

  # Rejected: the engine keeps NEL, so parseInt sees a non-digit first.
  out=$(_ccm_engine_int "${nel}100000")
  [[ -z "$out" ]] || {
    print -u2 -- "FAIL: a leading NEL must make the value invalid, got '$out'"
    rc=1
  }

  # Accepted: all three are ECMAScript whitespace.
  # Parallel arrays, not a delimited string: a tab inside the string would split on
  # itself and the fixture would test garbage.
  local -a ws_chars=(" " "$tab" "$nbsp")
  local -a ws_names=(space tab nbsp)
  local i
  for i in {1..3}; do
    out=$(_ccm_engine_int "${ws_chars[$i]}100000")
    [[ "$out" == 100000 ]] || {
      print -u2 -- "FAIL: leading ${ws_names[$i]} is engine whitespace and must be skipped, got '$out'"
      rc=1
    }
  done

  return $rc
}

# The clamp must bound by digit LENGTH before doing arithmetic. zsh integers are 64-bit,
# so a twenty-digit value truncates -- and the truncated form compares LESS than the
# floor, so the value lands on 100,000 where the engine caps at 1,000,000, with a raw
# "number truncated" warning leaking alongside it.
#
# Probed, not assumed: with the bound a 20-digit value yields 1,000,000; without it,
# 100,000 plus the warning. Asserted at the helper because the arithmetic is the helper's
# and every caller inherits it.
test_the_clamp_bounds_by_length_before_arithmetic() {
  local rc=0 v out
  source "$CCMORPH_SOURCE"

  # Twenty digits: beyond what zsh can represent, so only a length bound can handle it.
  # NOT via $( ): the helper assigns by name, and a command substitution runs it in a
  # subshell where that assignment is discarded. Capture stderr to a file instead.
  local errfile
  errfile=$(mktemp)
  v=99999999999999999999
  _ccm_clamp_acw v 2>"$errfile"
  if [[ -s "$errfile" ]]; then
    print -u2 -- "FAIL: clamping an oversized value leaked: $(<"$errfile")"
    rc=1
  fi
  rm -f "$errfile"
  [[ "$v" == "$_CCMORPH_ACW_MAX" ]] || {
    print -u2 -- "FAIL: an oversized value must clamp to the ceiling $_CCMORPH_ACW_MAX, got $v"
    rc=1
  }

  # And the ordinary cases still behave.
  v=50000;   _ccm_clamp_acw v
  [[ "$v" == "$_CCMORPH_ACW_MIN" ]] || {
    print -u2 -- "FAIL: a sub-floor value must clamp up, got $v"; rc=1
  }
  v=2000000; _ccm_clamp_acw v
  [[ "$v" == "$_CCMORPH_ACW_MAX" ]] || {
    print -u2 -- "FAIL: an above-ceiling value must clamp down, got $v"; rc=1
  }
  v=700000;  _ccm_clamp_acw v
  [[ "$v" == 700000 ]] || {
    print -u2 -- "FAIL: an in-range value must be left alone, got $v"; rc=1
  }
  return $rc
}

test_an_overflowing_env_value_is_invalid_not_an_error() {
  local rc=0 out=""
  source "$CCMORPH_SOURCE"

  local v
  for v in 1e999 -1e999 1e309; do
    out=$(_ccm_engine_int "$v")
    if (( $? != 0 )); then
      print -u2 -- "FAIL: _ccm_engine_int exited non-zero for $v; readers swallow that and fall through"
      rc=1
    fi
    if [[ -n "$out" ]]; then
      print -u2 -- "FAIL: $v must be invalid (empty), got $out"
      rc=1
    fi
  done

  # A large but finite exponent form must still parse. This does NOT exercise the
  # 10^18 cap -- 1e21 yields the same value with or without it, because zsh only
  # truncates past 19 digits and the cap lands at exactly 19. It guards the weaker
  # property that the inf/nan check above did not start rejecting finite values.
  out=$(_ccm_engine_int 1e21)
  [[ "$out" == <-> ]] && (( ${#out} <= 19 )) || {
    print -u2 -- "FAIL: 1e21 must yield a representable integer, got '$out'"
    rc=1
  }
  out=$(_ccm_engine_int 700000)
  [[ "$out" == 700000 ]] || {
    print -u2 -- "FAIL: a plain integer must survive, got '$out'"
    rc=1
  }
  return $rc
}

test_neither_env_parser_accepts_non_ascii_digits() {
  local test_home nonascii
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Arabic-Indic "50", built from code points so this file stays ASCII.
  nonascii=$(printf '\u0665\u0660')

  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small","onetwenty"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 key="" actual=""
  # A rejected override leaves the engine default in charge: on a 200,000 window that
  # trigger clears 120,000, so the sibling IS fatal and must be named. Accepting the
  # value would compute a lower trigger and say nothing.
  for key in CLAUDE_AUTOCOMPACT_PCT_OVERRIDE CLAUDE_CODE_AUTO_COMPACT_WINDOW; do
    printf '%s\n' \
      'type=proxy' \
      'base_url=http://gw.invalid' \
      'api_key=test-key' \
      'model=small' \
      "env.$key=$nonascii" \
      > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 onetwenty=120000
    : > "$CCMORPH_TEST_LOG"
      _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
    actual="$(<"$CCMORPH_TEST_LOG")"
    assert_contains "$actual" 'onetwenty limit 120k' \
      "a non-ASCII $key must be rejected, leaving the sibling fatal" || rc=1
  done
  rm -rf "$test_home"
  return $rc
}

test_an_env_budget_is_parsed_the_way_the_engine_parses_it() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=big' \
    'env.CLAUDE_CODE_AUTO_COMPACT_WINDOW=1e5' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw big=400000 mid=300000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["big","mid"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  assert_not_contains "$actual" 'mid limit' \
    'an engine-valid exponent pin must govern, leaving the sibling safe' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_the_output_reserve_override_reaches_the_hazard_path() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=small' \
    'env.CLAUDE_CODE_MAX_OUTPUT_TOKENS=8192' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 onefifty=150000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small","onefifty"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  # It has to reach the launched process at all...
  assert_contains "$actual" 'MAX_OUTPUT=8192' \
    'the reserve override must be allowed through to the engine' || rc=1
  # ...and it has to move the trigger ccmorph reports.
  assert_contains "$actual" 'onefifty limit 150k' \
    'a smaller reserve raises the trigger, making the sibling fatal' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_an_out_of_schema_saved_budget_does_not_block_derivation() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000 fourhundred=400000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["roomy","fourhundred"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # Canon holds a VALID budget; the profile overrides it with one below the schema
  # floor. Ordering matters: the overlay merges canon-then-profile, so the engine sees
  # only the merged (invalid) key, discards it, and falls through to the derived
  # budget -- canon's 300,000 never reaches it. A resolver that kept the earlier valid
  # value would compute a 224,000 trigger and miss the fatal sibling entirely, which is
  # exactly the defect this ordering exercises.
  printf '%s\n' '{"autoCompactWindow":300000}' > "$HOME/.claude/settings.json"
  printf '%s\n' '{"availableModels":["roomy","fourhundred"],"autoCompactWindow":50000}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  assert_contains "${actual//\"autoCompactWindow\": /\"autoCompactWindow\":}" \
    '"autoCompactWindow":1000000' \
    'a schema-invalid saved budget must not block the derived one' || rc=1
  assert_not_contains "$actual" '"autoCompactWindow":50000' \
    'a value the engine discards must never be shipped in the overlay' || rc=1
  # And the ADVISORY must use the same budget. Asserting only the overlay left this
  # test green when the resolver still honoured the discarded 50,000: the trigger
  # then computed to ~29,600 and the fatal sibling went unnamed.
  assert_contains "$actual" 'fourhundred limit 400k' \
    'the advisory must resolve the same budget the overlay ships' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A schema-VALID saved budget still wins: the derivation fills a gap, it never
# overrules a person. Paired with the test above so the fix cannot overshoot.
# The overlay gate and the resolver both decide whether a saved budget survives, in
# two separate embedded programs that cannot share a function. They must agree on every
# boundary, or the session ships one budget and the advisory reports another: an
# integer-valued float was accepted by one and rejected by the other, so the overlay
# kept the user's 300,000 while the advisory computed from the derived value.
#
# One launch per value, asserting both surfaces together. A saved budget that governs
# must appear in the overlay AND leave the sibling unnamed (its trigger clears the
# sibling wall); one that does not govern must let the derived budget through and name
# the sibling.
# 1e999 is valid JSON, and json.load returns it as inf. The engine tolerates such a
# file completely -- JSON.parse yields Infinity, then .int().catch(void 0) discards it
# -- and launches normally. int(inf) raises OverflowError, so a gate that converts
# before checking finiteness crashed the whole overlay program and aborted the launch
# with "could not render temporary launch settings": a settings file the engine
# ignores made ccmorph refuse to start.
#
# The resolver copy guarded in the right order and the overlay copy did not, which is
# the one-rule-two-copies drift behind five defects in this review. The five-value
# parity test never probed a non-finite number, so nothing caught it.
test_a_non_finite_saved_budget_does_not_abort_the_launch() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=small' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["small"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # Valid JSON; json.load gives inf.
  printf '%s\n' '{"autoCompactWindow": 1e999}' > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || {
      print -u2 -- "FAIL: a non-finite saved budget must not abort the launch"
      rm -rf "$test_home"
      return 1
    }
  # And the overlay actually reached Claude. Pre-fix the program died on int(inf) and
  # wrote nothing, so "the launch returned 0" is not enough on its own: the stub logs
  # the --settings file it was handed, so its presence is the real evidence.
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'SETTINGS=' \
    'the overlay must still be rendered and passed through --settings' || rc=1
  assert_contains "$actual" 'availableModels' \
    'the rendered overlay must carry the merged settings, not an empty file' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_the_overlay_and_the_advisory_agree_on_the_schema() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000 fourhundred=400000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["roomy","fourhundred"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 saved="" want="" actual=""
  # value|governs — a governing 200,000 budget triggers at 144,000, clearing the
  # 400,000 sibling; a rejected one falls through to 1,000,000 and triggers at
  # 784,000, making the sibling fatal.
  for spec in '200000|yes' '200000.0|yes' '50000|no' '2000000|no' 'true|no'; do
    saved="${spec%%|*}"
    want="${spec##*|}"
    printf '%s\n' "{\"autoCompactWindow\":$saved}" > "$HOME/.claude/settings.json"
    : > "$CCMORPH_TEST_LOG"
      _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
    actual="$(<"$CCMORPH_TEST_LOG")"
    if [[ "$want" == yes ]]; then
      assert_contains "${actual//\"autoCompactWindow\": /\"autoCompactWindow\":}" \
        '"autoCompactWindow":200000' \
        "saved $saved must govern the overlay" || rc=1
      assert_not_contains "$actual" 'fourhundred limit' \
        "saved $saved must govern the advisory too" || rc=1
    else
      assert_contains "${actual//\"autoCompactWindow\": /\"autoCompactWindow\":}" \
        '"autoCompactWindow":1000000' \
        "saved $saved must be discarded by the overlay" || rc=1
      assert_contains "$actual" 'fourhundred limit' \
        "saved $saved must be discarded by the advisory too" || rc=1
    fi
  done
  rm -rf "$test_home"
  return $rc
}

test_a_valid_saved_budget_still_beats_the_derivation() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=fourhundred' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw fourhundred=400000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["fourhundred"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # Written with a trailing .0 deliberately: the engine's `.int()` accepts an
  # integer-valued float, and an isinstance(v, int) test rejects it -- so a plain
  # integer here would leave this test green if that support were reverted.
  printf '%s\n' '{"autoCompactWindow":300000.0}' > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "${actual//\"autoCompactWindow\": /\"autoCompactWindow\":}" \
    '"autoCompactWindow":300000' \
    'a schema-valid saved budget must survive the derivation' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_a_conf_pinned_ceiling_still_gets_hazard_analysis() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=big' \
    'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=1000000' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw big=1000000 small=400000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["big","small"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' '{"autoCompactWindow":1000000}' > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  assert_contains "$actual" 'small limit 400k' \
    'a fatal switch must be named even when the conf pins the ceiling' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Every sibling must be probed against the ceiling the ENGINE resolves for ITS OWN
# id, because zE() recomputes the ceiling per model on every call. Skipping claude-
# ids outright assumed min() had already narrowed them -- true for a real Anthropic
# model, false for a gateway model wearing a claude- name, and ccmorph has the
# machinery to translate exactly those.
#
# Probed. Launch on a 1,000,000 route, pct 80, so the session trigger is 784,000.
# A sibling id `claude-mini` takes the engine hardcoded 200,000 ceiling regardless of
# CLAUDE_CODE_MAX_CONTEXT_TOKENS: usable 180,000, trigger 144,000. Its probed wall
# is 131,072, BELOW that -- a fatal switch. The old loop skipped it for its prefix and
# printed nothing, which is the unrecoverable wedge this feature exists to prevent.
# `claude-foo` and `claude-foo[1m]` are one bare id and TWO engine ceilings, 200,000
# and 1,000,000, so their triggers differ by up to 823,000 tokens. Deduping on the
# bare id alone was harmless while every sibling shared one trigger, but once each
# derives its own it silently drops whichever variant is listed second -- and because
# the [1m] trigger is always the higher one, bare-listed-first is precisely the
# ordering that hides a fatality.
#
# Probed on a 1,000,000 route at pct 80, registry claude-foo=300000:
#
#   bare first   bare checked against its own 144,000 (safe), [1m] then skipped
#                as a duplicate -> SILENT, though its trigger is 784,000 > 300,000
#   [1m] first   named at 784,000 -> correct
#
# The pair is reachable: the allowlist keeps every claude- entry a user has had, so a
# downward re-probe leaves a stale [1m] beside its bare form.
# CL() is Math.min(ceiling, autoCompactWindow) for both the env and the settings source,
# so the session budget caps EVERY model on every call -- not only the launched one. A
# sibling trigger derived from the family ceiling alone overstates it by up to ~560,000
# tokens, which names a safe model fatal and, under CCMORPH_WITHHOLD_FATAL=1, removes a
# model the user can actually use.
#
# The shipped advisory was self-contradictory before this: on a 300,000 budget it printed
# "compacts at 224k" and, in the same message, "claude-foo limit 500k" -- a wall the
# session can never reach.
#
# Probed. Sibling claude-foo[1m], registry wall 500,000, route ceiling 922,000, pct 80:
#
#   budget 300,000  window min(1e6, 300000) = 300,000 -> usable 280,000
#                   trigger 224,000 < 500,000 -> SAFE, must be silent
#   no budget       window 1,000,000 -> usable 980,000
#                   trigger 784,000 > 500,000 -> FATAL, must be named
#
# Every other hazard test passes budget=1000000, which is the one value where the min is
# invisible -- so without this test the whole rule was unguarded.
# cV() falls back to the MODEL default max-output when the env pin is absent, and three
# legacy ids declare one below the 20,000 cap -- claude-3-opus and claude-3-haiku at 4096,
# claude-3-sonnet at 8192, read out of 2.1.247:
#
#   if(o)t=o.default,n=o.upper;
#   else if(r==="claude-3-opus"||r==="claude-3-haiku")t=4096,n=4096;
#   else if(r==="claude-3-sonnet")t=8192
#
# A smaller reserve means a LARGER usable window and a HIGHER trigger, which is the
# direction that leaves a wedge unnamed.
#
# Probed on a 200,000 window at pct 80, wall 150,000:
#
#   claude-3-opus   usable 195,904 -> trigger 156,723 >= 150,000 -> FATAL
#   any other id    usable 180,000 -> trigger 144,000 <  150,000 -> safe
test_a_legacy_id_uses_its_own_output_default() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  source "$CCMORPH_SOURCE"

  local rc=0 err
  write_profile_context gw claude-3-opus=150000
  print -r -- '{"availableModels":["claude-3-opus"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  err=$(_ccm_invariant_warn gw 80 "" "" "" 2>&1)
  assert_contains "$err" 'compaction starts at 156723' \
    'a 4096-default id reserves less, so its trigger is 156,723 and the wall is fatal' || rc=1

  # An ordinary id at the SAME wall keeps the 20,000 reserve and stays safe, so this
  # cannot pass by naming every model.
  write_profile_context gw claude-plain=150000
  print -r -- '{"availableModels":["claude-plain"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  err=$(_ccm_invariant_warn gw 80 "" "" "" 2>&1)
  assert_not_contains "$err" 'claude-plain holds' \
    'an ordinary id keeps the 20,000 reserve and is safe at the same wall' || rc=1

  # An env pin does NOT simply overrule the model default: yN caps it at that model's
  # upperLimit first (`if(o>r){effective:r,status:"capped"}`), and claude-3-opus declares
  # 4096/4096. So a pin of 20,000 is reduced to 4096 by the engine and the model is STILL
  # fatal. Asserting the opposite -- that the pin makes it safe -- locked the bug in.
  write_profile_context gw claude-3-opus=150000
  print -r -- '{"availableModels":["claude-3-opus"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  err=$(_ccm_invariant_warn gw 80 20000 "" "" 2>&1)
  assert_contains "$err" 'compaction starts at 156723' \
    'a pin above the model upperLimit is capped down to it, so the wall stays fatal' || rc=1

  # A pin BELOW the limit is honoured, which is the half that is genuinely a pin.
  err=$(_ccm_invariant_warn gw 80 2000 "" "" 2>&1)
  assert_contains "$err" 'compaction starts at 158400' \
    'a pin under the model upperLimit is used as given' || rc=1

  # Two catalogue ids also declare a sub-cap default -- 8192, so trigger 153,446 -- and
  # they are reachable only through the catalogue branch, which the ids above do not
  # exercise. Dated and mixed-case forms fold to the same base id.
  write_profile_context gw claude-3-5-haiku=150000
  print -r -- '{"availableModels":["claude-3-5-haiku"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  err=$(_ccm_invariant_warn gw 80 "" "" "" 2>&1)
  assert_contains "$err" 'compaction starts at 153446' \
    'a catalogue id with an 8192 default reserves 8192, not 20,000' || rc=1

  write_profile_context gw claude-3-5-haiku-20241022=150000
  print -r -- '{"availableModels":["claude-3-5-haiku-20241022"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  err=$(_ccm_invariant_warn gw 80 "" "" "" 2>&1)
  assert_contains "$err" 'compaction starts at 153446' \
    'a dated id folds to its base, as the engine normalizer does' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_hazard_set_honours_a_profile_tombstone_of_availablemodels() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_profile_context p m-small=150000
  mkdir -p "$HOME/.claude" "$HOME/.config/ccmorph/profiles/p"
  print -r -- '{"availableModels":["m-small","m-big"]}' > "$HOME/.claude/settings.json"
  print -r -- '{"$ccmorphUnset":[["availableModels"]]}' \
    > "$HOME/.config/ccmorph/profiles/p/settings.profile.json"
  source "$CCMORPH_SOURCE"

  local rc=0 out
  local -a penv
  out=$(_ccm_hazard_set p 700000 900000 '' 2>/dev/null)
  assert_not_contains "$out" 'm-small' \
    'a profile that deleted availableModels offers no models, so none is a hazard' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_session_budget_caps_every_sibling_ceiling() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_profile_context gw claude-foo=500000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["claude-foo[1m]"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 out err
  local -a penv
  penv=(CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80)

  # With the budget in force the sibling is safe and must not be named.
  out=$(        _ccm_hazard_set gw 300000 922000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  assert_not_contains "$out" 'claude-foo' \
    'the budget caps the sibling ceiling, so a 500,000 wall clears a 224,000 trigger' || rc=1

  # Without a budget the family ceiling stands and the same model IS fatal.
  out=$(        _ccm_hazard_set gw "" 1000000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  assert_contains "$out" 'claude-foo' \
    'with no budget the 1,000,000 ceiling gives a 784,000 trigger, which is fatal' || rc=1

  # The same rule in the other mirror, which computes its own window.
  err=$(_ccm_invariant_warn gw 80 "" "" 300000 2>&1)
  assert_not_contains "$err" 'claude-foo holds' \
    'the unsafe warning must cap by the budget too' || rc=1
  err=$(_ccm_invariant_warn gw 80 "" "" "" 2>&1)
  assert_contains "$err" 'claude-foo holds 500000' \
    'and must still name it when no budget narrows the ceiling' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_a_bare_and_1m_pair_are_two_different_siblings() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_profile_context gw roomy=1000000 claude-foo=300000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  source "$CCMORPH_SOURCE"

  local rc=0 out
  local -a penv
  penv=(CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80)

  # Bare listed FIRST: the ordering that used to hide the [1m] fatality.
  printf '%s\n' '{"availableModels":["roomy","claude-foo","claude-foo[1m]"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  out=$(        _ccm_hazard_set gw 1000000 1000000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  assert_contains "$out" 'claude-foo' \
    'a [1m] variant listed after its bare form must still be named' || rc=1

  # And listed the other way round it is named exactly once, not twice.
  printf '%s\n' '{"availableModels":["roomy","claude-foo[1m]","claude-foo"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  out=$(        _ccm_hazard_set gw 1000000 1000000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  local hits
  hits=$(print -r -- "$out" | grep -c 'claude-foo')
  [[ "$hits" == 1 ]] || {
    print -u2 -- "FAIL: the pair must yield one row, got $hits"
    rc=1
  }

  # And when BOTH variants clear the fatal test, the model is still named once. Keying
  # the comparison on (id, ceiling) is right, but the ROW is keyed on the name, because
  # the name is what gets printed and two identical names burn two of three slots.
  #
  # Note the budget is passed empty here: with a budget in force CL() mins it into BOTH
  # variants, which collapses their ceilings to the same number and would make this a
  # test of the min rather than of the dedup. An earlier version of this comment claimed
  # the two triggers were 304,000 and 784,000 on a 400,000 route -- wrong, because the
  # budget caps both to 304,000. The dedup is what is under test, so no budget.
  write_profile_context gw roomy=400000 plain=250000
  printf '%s\n' '{"availableModels":["roomy","plain","plain[1m]"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  out=$(        _ccm_hazard_set gw "" 400000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  hits=$(print -r -- "$out" | grep -c 'plain')
  [[ "$hits" == 1 ]] || {
    print -u2 -- "FAIL: a model fatal at both ceilings must be named once, got $hits"
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}

test_a_claude_named_routed_model_is_sized_by_its_own_id() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_profile_context gw roomy=1000000 claude-mini=131072
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["roomy","claude-mini"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 out
  local -a penv
  penv=(CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80)
  out=$(        _ccm_hazard_set gw 1000000 1000000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  assert_contains "$out" 'claude-mini' \
    'a claude-named model walled under its own 200,000-family trigger is fatal' || rc=1

  # A claude- sibling with a wall ABOVE its own trigger is safe and must stay unnamed,
  # so this cannot pass by naming every claude- id.
  write_profile_context gw roomy=1000000 claude-ample=200000
  printf '%s\n' '{"availableModels":["roomy","claude-ample"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  out=$(        _ccm_hazard_set gw 1000000 1000000 \
          "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  assert_not_contains "$out" 'claude-ample' \
    'a claude-named model above its own trigger is safe and must not be named' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_advisory_and_the_withhold_share_one_budget() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000 narrow=262144
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["roomy","narrow"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # The user's own choice, which outranks the derivation.
  printf '%s\n' '{"autoCompactWindow":300000}' > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  CCMORPH_WITHHOLD_FATAL=1 \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  # Nothing is fatal at a ~224k trigger, so the model must survive in the picker
  # AND no advisory may name it. Either one alone would be the bug.
  local offered
  offered=$(_ccm_offered_models "$actual")
  assert_contains "$offered" 'narrow' \
    'a model above the effective trigger must not be withheld' || rc=1
  assert_not_contains "$actual" 'narrow limit' \
    'a model above the effective trigger must not be named either' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The percentage may live in a settings layer and nowhere else -- the profile conf
# carries none, so the launch injects none (a penv value would reach the engine over
# the user's own choice; #428) and the layer value rides the overlay into the engine
# instead. The hazard set read the launch env alone, so it computed the
# no-percentage trigger `usable - 13000` -- a larger number than the engine's own --
# and named every model between the two. The set feeds the advisory AND
# _ccm_withhold_fatal_switches, so under CCMORPH_WITHHOLD_FATAL=1 a model the
# session can in fact switch to was silently dropped from the picker.
test_a_layer_percentage_sizes_the_hazard_set() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000 big-model=800000 small-model=500000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["roomy","big-model","small-model"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # The user's trigger, set in the file /autocompact writes to. The profile conf
  # stays silent about it, which is what leaves penv empty.
  printf '%s\n' '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"70"}}' \
    > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  # usable = 1000000 - 20000 = 980000, and the engine's trigger is 70% of that:
  # 686000. Reading the launch env alone yields 967000 instead -- above the
  # 800000 wall of a model the engine in fact compacts long before reaching.
  local -a penv
  local hazards
  hazards=$(_ccm_hazard_set gw "" 1000000 '' 2>/dev/null)
  assert_contains "$hazards" '686000' \
    'the trigger must come from the percentage the layers set, not from the launch env alone' || rc=1
  assert_not_contains "$hazards" 'big-model' \
    'a model wall above the layer trigger is safe and must not be named' || rc=1
  assert_contains "$hazards" 'small-model' \
    'a model wall below the layer trigger is still fatal' || rc=1

  # The same set feeds the picker, so the launch half must agree: the model above
  # the layer trigger survives the overlay's availableModels, and the one below it
  # still goes.
  CCMORPH_WITHHOLD_FATAL=1 \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  # The premise, at the launch: the layers set the percentage, so nothing is injected
  # into the engine's environment and the layer value is all the engine has.
  assert_contains "$actual" 'AUTOCOMPACT_PCT=<unset>' \
    'the launch must not inject a percentage over the layer the user set' || rc=1
  assert_contains "$actual" 'compacts at 686k' \
    'the launch must warn with the layer trigger too' || rc=1
  local offered
  offered=$(_ccm_offered_models "$actual")
  assert_contains "$offered" 'big-model' \
    'a model above the layer trigger must not be withheld from the picker' || rc=1
  assert_not_contains "$offered" 'small-model' \
    'a model below the layer trigger is still withheld' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_the_reported_trigger_is_the_engines_own_arithmetic() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=small' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 midsize=150000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small","midsize"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"

  assert_not_contains "$actual" 'midsize limit' \
    'a model above the engine trigger must not be called fatal' || rc=1
  assert_not_contains "$actual" 'compacts at 160k' \
    'the trigger must not be computed as a bare percentage of the budget' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The defect this channel exists to fix: anything written before exec is painted
# over by Claude Code's alternate screen. No launch advisory may rely on stderr
# for a human to read — it flashes for 0.2s and is gone.
test_no_launch_advisory_relies_on_pre_exec_stderr() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  mkdir -p "$HOME/.config/ccmorph/profiles/spread"
  print -r -- '{"availableModels":["roomy","tiny"]}' \
    > "$HOME/.config/ccmorph/profiles/spread/settings.json"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=roomy' \
    > "$HOME/.config/ccmorph/spread.conf"
  write_profile_context spread roomy=1000000 tiny=262144
  # A profile whose model nothing can size, so the other launch-time advisory is
  # held to the same rule.
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=unprobed' \
    > "$HOME/.config/ccmorph/blank.conf"
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(        _ccm_launch -a spread prompt 2>&1 >/dev/null)
  assert_not_contains "$err" 'compacts at' \
    'the switch warning must not go where the TUI paints over it' || rc=1

  err=$(        _ccm_launch -a blank prompt 2>&1 >/dev/null)
  assert_not_contains "$err" 'Context limit unknown' \
    'the unprobed-model line must not go where the TUI paints over it' || rc=1
  rm -rf "$test_home"
  return $rc
}

# An unprobed routed model is the other launch-time advisory and had the same
# flaw. Same channel, so that it is seen at all.
test_the_unprobed_model_advisory_persists_in_the_tui() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  : > "$HOME/registry.conf"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=unprobed' \
    > "$HOME/.config/ccmorph/blank.conf"
  source "$CCMORPH_SOURCE"

  local _CCMORPH_LOG="$HOME/ccmorph.log"
  local rc=0
    _ccm_launch -a blank prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"; return 1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_not_contains "$actual" 'Context limit unknown' \
    'an unsized model is handled silently; there is nothing for the person to do' || rc=1
  assert_not_contains "$actual" 'ccmorph probe' \
    'no command is handed to the person' || rc=1
  assert_contains "$(<"$_CCMORPH_LOG")" 'window unknown profile=blank' \
    'the fallback is still recorded where it can be diagnosed' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A wide route (OpenRouter serves 8k to 2M) would otherwise print a wall of
# names. Three plus a count conveys the same fact in one line.
test_the_switch_warning_caps_the_names_it_lists() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  printf '%s\n' 'roomy=1000000' 'n1=300000' 'n2=310000' 'n3=320000' 'n4=330000' \
    'n5=340000' > "$HOME/registry.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/wide"
  print -r -- '{"availableModels":["roomy","n1","n2","n3","n4","n5"]}' \
    > "$HOME/.config/ccmorph/profiles/wide/settings.json"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=roomy' \
    > "$HOME/.config/ccmorph/wide.conf"
  write_profile_context wide roomy=1000000 n1=300000 n2=310000 n3=320000 n4=330000 n5=340000
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a wide prompt >/dev/null 2>&1 || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  # "smaller", not "more": the list is ordered largest wall first, so the tail is
  # always the models with less room than the ones named.
  assert_contains "$actual" '+2 smaller' \
    'a wide route must summarise the tail rather than list every model' || rc=1
  # One advisory, not one per model: the systemMessage is a single line in the
  # TUI, so a route with fifty narrow models must still cost one line.
  local occurrences
  occurrences=$(print -r -- "$actual" | grep -o 'compacts at' | grep -c . || true)
  if (( occurrences != 1 )); then
    print -u2 -- "the switch warning must appear exactly once (got $occurrences)"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

# One advisory, two paths. The launch reads the hazard warning out of the
# diagnostics interpreter as a `hazard` record beside the answers it already
# brings back (#433); `_ccm_switch_hazard_warn` is still there for a launch that
# gets no record, computing the same rows the way it always did. Neither path may
# change what the person sees, so this launches the same profile three times: the
# record it computed, the same launch with the record dropped out of the answers
# (so it falls back), and the withholding wording both must agree on.
test_the_launch_hazard_advisory_matches_switch_hazard_warn() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"availableModels":["roomy","kimi-k3"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  # The walls ride the conf, so the advisory's numbers do not depend on a probe.
  write_profile_context gw roomy=1000000 kimi-k3=40000
  source "$CCMORPH_SOURCE"

  local rc=0
  local advise_from_record advise_withheld advise_from_fallback

  rm -f "$CCMORPH_TEST_LOG"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1 }
  advise_from_record=$(grep -m1 '^ADVISE=' "$CCMORPH_TEST_LOG" || true)
  advise_from_record=${advise_from_record#ADVISE=}
  assert_contains "$advise_from_record" 'lower model limits: kimi-k3 limit 40k' \
    'a route with a model below the trigger must say so' || rc=1

  rm -f "$CCMORPH_TEST_LOG"
  CCMORPH_WITHHOLD_FATAL=1 \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1 }
  advise_withheld=$(grep -m1 '^ADVISE=' "$CCMORPH_TEST_LOG" || true)
  advise_withheld=${advise_withheld#ADVISE=}
  assert_contains "$advise_withheld" 'hidden from /model: kimi-k3 limit 40k' \
    'withheld choices must be reported in their own wording' || rc=1

  # The fallback, forced: the record the diagnostics just computed is dropped
  # before the launch reads it, so `_ccm_launch` takes its `else` branch and
  # `_ccm_switch_hazard_warn` renders the warning instead. Every other answer the
  # diagnostics gives passes through untouched, so which path speaks is the only
  # difference between this launch and the one above.
  functions[_ccm_diag_without_hazard]="${functions[_ccm_launch_diagnostics]}"
  _ccm_launch_diagnostics() {
    local -a records=("${(@0)$(_ccm_diag_without_hazard "$@")}")
    local record
    for record in "${records[@]}"; do
      [[ -n "$record" ]] || continue
      if [[ "$record" == hazard$'\t'* ]]; then
        : > "$HOME/hazard-record-dropped"
        continue
      fi
      printf '%s\0' "$record"
    done
  }
  rm -f "$CCMORPH_TEST_LOG" "$HOME/hazard-record-dropped"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1 }
  advise_from_fallback=$(grep -m1 '^ADVISE=' "$CCMORPH_TEST_LOG" || true)
  advise_from_fallback=${advise_from_fallback#ADVISE=}
  # Without this the third launch proves nothing: a stub that never saw a hazard
  # record would leave the launch on the record path, comparing it with itself.
  [[ -e "$HOME/hazard-record-dropped" ]] || {
    print -u2 -- 'the launch never asked for a hazard record to drop'
    rc=1
  }
  if [[ "$advise_from_fallback" != "$advise_from_record" ]]; then
    print -u2 -- "the fallback's advisory differs\n  from the record: ${(qqq)advise_from_record}\n  from the fallback: ${(qqq)advise_from_fallback}"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

# The order the fold rests on. The folded `hazard` record is computed BEFORE the
# launch injects CLAUDE_AUTOCOMPACT_PCT_OVERRIDE, and `_ccm_switch_hazard_warn`
# runs AFTER it. The parity test above pins their wording for one percentage; this
# pins the order for every source a launch can take one from: the fleet policy, the
# built-in default when no policy exists, a conf line, and the user's own layer --
# valid, and malformed so the launch injects the fleet policy over it. In each case
# the environment must carry the percentage the launch settled on, the folded record
# must name the trigger THAT percentage produces, and the fallback -- which sizes
# after the injection -- must say exactly the same thing.
test_the_hazard_record_sizes_from_the_percentage_the_launch_injects() {
  local case rc=0 test_home actual advise_from_record advise_from_fallback value
  local -a conf
  # usable = 1,000,000 - 20,000 = 980,000, so the trigger is the smaller of
  # 980,000*pct/100 and 980,000 - 13,000. A layer value the engine cannot parse
  # leaves the injected fleet policy of 80 in charge.
  local -A want_pct want_trigger
  want_pct=(policy73 73 absent 75 conf60 60 layer70 '<unset>' malformed 80)
  want_trigger=(policy73 715k absent 735k conf60 588k layer70 686k malformed 784k)

  for case in policy73 absent conf60 layer70 malformed; do
    test_home=$(mktemp -d)
    setup_home "$test_home"
    printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
    chmod +x "$HOME/bin/curl"
    make_fake_curl_probe_aware
    mkdir -p "$HOME/.config/ccmorph/profiles/gw"
    print -r -- '{"availableModels":["roomy","kimi-k3"]}' \
      > "$HOME/.config/ccmorph/profiles/gw/settings.json"
    conf=('type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=roomy')
    case "$case" in
      policy73)
        print -r -- '{"schemaVersion":1,"autoCompactAtPercent":73}' \
          > "$HOME/.agents/runtime/policies/context.json"
        ;;
      absent)
        rm -f "$HOME/.agents/runtime/policies/context.json"
        ;;
      conf60)
        conf+=('env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=60')
        ;;
      layer70|malformed)
        value=70
        [[ "$case" == malformed ]] && value=broken
        mkdir -p "$HOME/.claude"
        printf '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"%s"}}\n' "$value" \
          > "$HOME/.claude/settings.json"
        ;;
    esac
    printf '%s\n' "${conf[@]}" > "$HOME/.config/ccmorph/gw.conf"
    write_profile_context gw roomy=1000000 kimi-k3=40000
    source "$CCMORPH_SOURCE"

    rm -f "$CCMORPH_TEST_LOG"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
    actual=$(<"$CCMORPH_TEST_LOG")
    assert_contains "$actual" "AUTOCOMPACT_PCT=${want_pct[$case]}" \
      "the $case percentage must be the one the engine gets" || rc=1
    advise_from_record=$(grep -m1 '^ADVISE=' "$CCMORPH_TEST_LOG" || true)
    advise_from_record=${advise_from_record#ADVISE=}
    assert_contains "$advise_from_record" "compacts at ${want_trigger[$case]}" \
      "the folded record must size the $case trigger the launch injects" || rc=1
    assert_contains "$advise_from_record" 'kimi-k3 limit 40k' \
      "the $case advisory must name the model under the trigger" || rc=1

    # The fallback, forced the same way the parity test forces it: the record the
    # diagnostics computed is dropped, so `_ccm_launch` takes its `else` branch and
    # `_ccm_switch_hazard_warn` computes from the launch env after the injection.
    functions[_ccm_diag_without_hazard]="${functions[_ccm_launch_diagnostics]}"
    _ccm_launch_diagnostics() {
      local -a records=("${(@0)$(_ccm_diag_without_hazard "$@")}")
      local record
      for record in "${records[@]}"; do
        [[ -n "$record" ]] || continue
        if [[ "$record" == hazard$'\t'* ]]; then
          : > "$HOME/hazard-record-dropped"
          continue
        fi
        printf '%s\0' "$record"
      done
    }
    rm -f "$CCMORPH_TEST_LOG" "$HOME/hazard-record-dropped"
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
    advise_from_fallback=$(grep -m1 '^ADVISE=' "$CCMORPH_TEST_LOG" || true)
    advise_from_fallback=${advise_from_fallback#ADVISE=}
    [[ -e "$HOME/hazard-record-dropped" ]] || {
      print -u2 -- "the $case launch never asked for a hazard record to drop"
      rc=1
    }
    if [[ "$advise_from_fallback" != "$advise_from_record" ]]; then
      print -u2 -- "the $case fallback's advisory differs\n  from the record: ${(qqq)advise_from_record}\n  from the fallback: ${(qqq)advise_from_fallback}"
      rc=1
    fi
    rm -rf "$test_home"
  done
  return $rc
}

# The malformed mirror of the layer test above: the engine drops a percentage it
# cannot parse, so the launch injects the fleet percentage over the gap. The folded
# hazard record is computed BEFORE that injection, and hazard_rows re-reads the
# layers when the percentage comes from them -- a presence-gated read would swap the
# malformed string back in and size the set with the no-percentage trigger
# `usable - 13000`, naming models the engine in fact compacts long before as fatal
# and withholding them under CCMORPH_WITHHOLD_FATAL=1. The record must size from
# the percentage the ENGINE will use: the one in the launch env, or the one this
# launch is about to inject.
test_a_malformed_layer_percentage_leaves_the_injected_one_in_charge() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=roomy' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw roomy=1000000 big-model=800000 small-model=500000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw" "$HOME/.claude"
  printf '%s\n' '{"availableModels":["roomy","big-model","small-model"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # The user's trigger, malformed where /autocompact and a hand edit both land. The
  # profile conf stays silent about it, which is what leaves penv empty.
  printf '%s\n' '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"broken"}}' \
    > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  # usable = 1000000 - 20000 = 980000, and the fleet policy is 80, so the engine's
  # trigger is 784000. Reading the layer alone yields 967000 instead -- above the
  # 800000 wall of a model the engine in fact compacts long before reaching.
  CCMORPH_WITHHOLD_FATAL=1 \
    _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  # The premise, at the launch: the layer value does not parse, so the engine gets
  # the injected fleet percentage and nothing else.
  assert_contains "$actual" 'AUTOCOMPACT_PCT=80' \
    'the launch must inject the fleet percentage over a layer value the engine cannot parse' || rc=1
  assert_contains "$actual" 'compacts at 784k' \
    'the launch must warn with the injected trigger, not the malformed layer it replaced' || rc=1
  local offered
  offered=$(_ccm_offered_models "$actual")
  assert_contains "$offered" 'big-model' \
    'a model wall above the injected trigger must not be withheld from the picker' || rc=1
  assert_not_contains "$offered" 'small-model' \
    'a model wall below the injected trigger is still withheld' || rc=1
  assert_not_contains "$actual" 'big-model limit' \
    'a model wall above the injected trigger is safe and must not be named' || rc=1
  rm -rf "$test_home"
  return $rc
}

# Nothing to warn about when every switch target clears the trigger. A line that
# appears when it is not true is a line people learn to ignore.
test_no_switch_warning_when_the_whole_route_clears_the_trigger() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'cat <<JSON' '{"data":[]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  mkdir -p "$HOME/.config/ccmorph/profiles/safe"
  print -r -- '{"availableModels":["roomy","wide"]}' \
    > "$HOME/.config/ccmorph/profiles/safe/settings.json"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=roomy' \
    > "$HOME/.config/ccmorph/safe.conf"
  write_profile_context safe roomy=1000000 wide=1048576
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(        _ccm_launch -a safe prompt 2>&1 >/dev/null) || {
    rm -rf "$test_home"
    return 1
  }

  assert_not_contains "$err" 'compacts at' \
    'a route whose every model clears the trigger must warn about nothing' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_is_silent_when_every_model_has_headroom() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  mkdir -p "$HOME/.config/ccmorph/profiles/roomy"
  print -r -- '{"availableModels":["claude-fable-5-dd-gib"]}' \
    > "$HOME/.config/ccmorph/profiles/roomy/settings.json"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/roomy.conf"
  write_profile_context roomy big=262144
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(        _ccm_launch -a roomy prompt 2>&1 >/dev/null) || { rm -rf "$test_home"; return 1; }

  # A check that cries wolf gets ignored, which is worse than no check at all.
  assert_not_contains "$err" 'refuse' \
    'a profile whose models all have headroom must say nothing' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_projects_picker_rows_with_our_own_tag() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # The endpoint answers with disguised ids, which is what discovery does to anything
  # not called claude-*. The projection has to undo that to find each model's ceiling.
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"data":[' \
    '{"id":"claude-fable-5-dd-2.5-mlg","display_name":"glm-5.2"},' \
    '{"id":"claude-fable-5-dd-o4-tpg","display_name":"gpt-4o"},' \
    '{"id":"claude-fable-5-dd-wen","display_name":"unprobed"},' \
    '{"id":"claude-opus-4-8","display_name":"claude-opus-4-8"}' \
    ']}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"additionalModelOptionsCache":[{"value":"stale","label":"Stale","description":"x"}]}' \
    > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":["opus","fable","claude-opus-4-8","claude-sonnet-4-6","glm-5.2","glm-5.2"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=apikey' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=claude-opus-4-8' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw glm-5.2=700000 gpt-4o=128000
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_project_picker gw http://gw.invalid sk_t

  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"
  # Claude Code reads value/label/description from this cache verbatim, which is the
  # only row source whose description is not hardcoded to "From gateway".
  assert_contains "$state" '"description": "Gateway"' \
    'a projected row must carry our concise tag, not the built-in one' || rc=1
  assert_contains "$state" '"label": "glm-5.2"' \
    'the label is the bare model name -- the statusline shows the window' || rc=1
  assert_not_contains "$state" '700k' \
    'no ceiling is shown in a row; it would only duplicate the statusline' || rc=1
  assert_not_contains "$state" 'Stale' \
    'projection is a full replacement, so a withdrawn model does not linger' || rc=1
  assert_contains "$state" 'gpt-4o' \
    'projection must leave session-safety decisions to the live advisory and opt-in withhold' || rc=1
  assert_contains "$state" '"label": "new"' \
    'a model with no probed ceiling must still be offered, labelled by its real id' || rc=1

  local settings="$(<"$HOME/.config/ccmorph/profiles/gw/settings.json")"
  assert_contains "$settings" '"opus"' \
    'a pinned native family alias must survive projection' || rc=1
  assert_contains "$settings" '"fable"' \
    'the fable tier alias must survive projection like every other native family' || rc=1
  assert_contains "$settings" 'claude-opus-4-8' \
    'a native family alias must not hide a gateway model' || rc=1
  assert_contains "$settings" 'claude-sonnet-4-6' \
    'an unaliased Claude family entry must survive projection' || rc=1
  assert_contains "$settings" '"glm-5.2"' \
    'the allowlist must admit what the projection offers, by its real name' || rc=1
  local projected_count=$(print -r -- "$settings" | jq \
    '[.availableModels[] | select(. == "glm-5.2")] | length')
  [[ "$projected_count" == 1 ]] || {
    print -u2 -- "a projected model must appear once, got $projected_count copies"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_projected_row_carries_the_profiles_own_tag() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Two profiles, two routes, two tags. The description on a projected row must name
  # where the model came from, not a single string stamped on every route's rows.
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"data":[{"id":"claude-fable-5-dd-2.5-mlg","display_name":"glm-5.2"}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  write_profile_context gw glm-5.2=700000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":[]}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t "" "Alt Gateway"

  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"
  assert_contains "$state" '"description": "Alt Gateway"' \
    'a projected row must carry the tag the caller supplied' || rc=1
  assert_not_contains "$state" '"description": "Gateway"' \
    'the supplied tag must replace the built-in default, not sit beside it' || rc=1

  # An absent tag falls back to the concise built-in, so old callers keep working.
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
    _ccm_project_picker gw http://gw.invalid sk_t
  assert_contains "$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")" '"description": "Gateway"' \
    'an omitted tag must fall back to the built-in default' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_projection_leaves_everything_alone_when_the_endpoint_is_down() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'exit 7' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"additionalModelOptionsCache":[{"value":"keep","label":"Keep","description":"Gateway"}]}' \
    > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":["keep"]}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=apikey' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=claude-opus-4-8' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw glm-5.2=700000
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_project_picker gw http://gw.invalid sk_t

  # An unreachable endpoint must not empty the picker. Yesterday's rows are still
  # true -- a model list changes far less often than a network does.
  assert_contains "$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")" 'Keep' \
    'a failed projection must leave the previous rows in place' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_projection_undisguises_ids_and_labels_every_row_with_the_id() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Every id here arrives DISGUISED (claude-fable-5-dd-<reversed>), which is what
  # discovery does to anything not called claude-*. Two things are proven: the disguise is
  # undone so a row carries the real id, and that real id is also the LABEL -- the
  # endpoint's display_name is ignored, because a family-alias row is labelled by Claude
  # Code with the literal id and a prettified neighbour reads as a different model. The
  # fixture keeps its mix of declared/undeclared/alias-only models because that mix is
  # what a real listing looks like -- it no longer decides whether a row exists, since
  # the picker offers every served model.
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"data":[' \
    '{"id":"claude-fable-5-dd-wol-los-6.5-tpg","display_name":"GPT 5.6 Sol (low)","max_input_tokens":372000},' \
    '{"id":"claude-fable-5-dd-5.5-tpg","display_name":"GPT 5.5","max_input_tokens":272000},' \
    '{"id":"claude-fable-5-dd-o4-tpg","display_name":"gpt-4o","max_input_tokens":200000},' \
    '{"id":"claude-fable-5-dd-2.5-mlg","display_name":"glm-5.2"}' \
    ']}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=x' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw gpt-5.5=922000 glm-5.2=700000 gpt-4o=128000
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t
  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"

  # The label is the bare name -- the statusline shows the live window -- so no ceiling
  # appears in a row at all. The launch advisory and opt-in withhold own live safety.
  assert_contains "$state" '"label": "gpt-5.6-sol-low"' \
    'an alias-only variant the endpoint publishes must survive, labelled by its id' || rc=1
  assert_not_contains "$state" 'GPT 5.6 Sol (low)' \
    'a display_name must not replace the id in a label' || rc=1
  assert_contains "$state" '"label": "gpt-5.5"' \
    'the reconstructed id is the label; the endpoint display_name is ignored' || rc=1
  assert_contains "$state" '"label": "glm-5.2"' \
    'a model the endpoint declares no size for is offered like any other' || rc=1
  assert_not_contains "$state" '272k' \
    'no ceiling is shown alongside the name; it would duplicate the statusline' || rc=1
  assert_not_contains "$state" 'claude-fable-5-dd' \
    'the disguise must be undone: a claude- value would cap the session at 200k' || rc=1
  # A translator may overstate a model that has a smaller probe. The row still
  # belongs in the picker; the live advisory uses the smaller ceiling and names the risk.
  assert_contains "$state" 'gpt-4o' \
    'a probed model must remain selectable even when its smaller ceiling needs a warning' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_projection_never_drops_the_profiles_own_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # codex's model is gpt-5.6-sol, an id the endpoint only ever lists in disguise. Writing
  # the allowlist from the projection alone removes it, and a model outside the allowlist
  # is not refused but remapped -- onto Claude Code's own catalogue, which this endpoint
  # has never heard of. The session then dies on "unknown provider".
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"data":[{"id":"claude-fable-5-dd-los-6.5-tpg","display_name":"GPT 5.6 Sol","max_input_tokens":372000}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":["claude-fable-5-dd","gpt-5.6-sol","claude-opus-4-8"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw x=1
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t gpt-5.6-sol
  local settings="$(<"$HOME/.config/ccmorph/profiles/gw/settings.json")"

  assert_contains "$settings" '"gpt-5.6-sol"' \
    "the profile's own model must stay allowed however the rows are written" || rc=1
  assert_contains "$settings" '"claude-opus-4-8"' \
    'a genuine Claude entry must survive' || rc=1
  assert_not_contains "$settings" '"claude-fable-5-dd"' \
    'the old disguise prefix must go: the projection lists each id exactly' || rc=1
  rm -rf "$test_home"
  return $rc
}

# A cross-profile hop does not relaunch on the target's default: the relay hands
# the next launch `--model <id>` (ccmorph.sh `_ccm_relay_next_args`). When that id
# is a `model_map` alias the endpoint never lists it -- the shim is what turns the
# alias into a model -- so no projected row carries it. An allowlist written from
# the projected rows alone therefore drops the model the relaunch is about to ask
# for, and a launch model outside availableModels is not refused but remapped onto
# Claude Code's own catalogue: the session silently runs something else, which is
# the promise a hop must not break.
test_relay_relaunch_keeps_a_mapped_alias_allowed() {
  local -a gw_conf=(
    'type=proxy'
    'base_url=http://gw.invalid'
    'api_key=sk_t'
    'model=plain-model'
    'model_map={"sol-alias":"gpt-5.6-sol","claude-sonnet-5-5":"plain-model"}'
  )
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' "${gw_conf[@]}" > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":["sonnet"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  # `sonnet` resolves to the route's own newest sonnet, the mapped alias to the model
  # the map names, so the family row must not subsume it: the session would then run
  # a model the hop never chose. The rule lives in the projection, so this pins it
  # there, on the layer the projection reads.
  local rc=0
  _ccm_project_picker gw http://gw.invalid sk_t '' '' claude-sonnet-5-5 || rc=1
  local seeded="$(<"$HOME/.config/ccmorph/profiles/gw/settings.json")"
  assert_contains "$seeded" '"sonnet"' \
    'a pinned family alias keeps its row beside the mapped alias' || rc=1
  assert_contains "$seeded" '"claude-sonnet-5-5"' \
    'a pinned family alias must not subsume the mapped alias this relaunch asks for' || rc=1

  # A launch replaces the profile layer with its generated settings, so the relaunch
  # cases below run against a home that no projection has written yet.
  rm -rf "$test_home"
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' "${gw_conf[@]}" > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  source "$CCMORPH_SOURCE"

  # The window marker is stripped for the map lookup exactly as the shim strips
  # it, which is case-sensitive, so a spelling the shim would not map is no
  # mapped alias here either. An id that is neither a map key nor a served row
  # stays out, as it does today.
  local spec model expect launched allowed
  for spec in 'sol-alias:allowed' 'claude-sonnet-5-5:allowed' \
              'claude-sonnet-5-5[1m]:allowed' 'claude-sonnet-5-5[1M]:absent' \
              'ghost-model:absent'; do
    model="${spec%%:*}"
    expect="${spec##*:}"
    print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
    : > "$CCMORPH_TEST_LOG"

    # Exactly the arguments `_ccm_relay_next_args` builds for a hop, and the shape
    # a `--model <id> --resume <session>` launch carries.
    _ccm_launch -a gw --model "$model" --resume relay-session >/dev/null 2>&1 || {
      print -u2 -- "the relaunch on '$model' must start Claude Code"
      rc=1
      continue
    }

    # The id on Claude Code's argv is the one the session validates against
    # availableModels, and the list it reads is the one this launch generated.
    local actual="$(<"$CCMORPH_TEST_LOG")"
    local argv_line="$(grep -m1 '^ARG' "$CCMORPH_TEST_LOG")"
    local -a argv=("${(@ps:\t:)argv_line}")
    local -i _mi=${argv[(I)--model]}
    (( _mi )) || {
      print -u2 -- "the relaunch on '$model' must hand Claude Code that model"
      rc=1
      continue
    }
    launched="${argv[_mi + 1]}"
    allowed="$(_ccm_offered_models "$actual")"
    [[ -n "$launched" && -n "$allowed" ]] || {
      print -u2 -- "the relaunch on '$model' must reach Claude Code with a generated model list"
      rc=1
      continue
    }

    if [[ "$expect" == allowed ]]; then
      assert_contains ",$allowed," ",$launched," \
        "the relaunch on '$model' carries it as --model, so the projected list must keep it" || rc=1
    else
      assert_not_contains ",$allowed," ",$launched," \
        "'$model' is no mapped alias and no served id, so it stays out of the list as it does today" || rc=1
    fi
  done

  # The same relaunch while the endpoint's catalogue cannot be fetched. An
  # exclusion is local config, true without the endpoint, so the offline branch
  # still rewrites the allowlist from what it has -- and the alias must survive
  # that rewrite too: the relaunch is about to ask for it, and a missing id is
  # not refused but remapped. With no exclusion the offline contract leaves both
  # files alone (test_projection_leaves_everything_alone_when_the_endpoint_is_down),
  # which is why this leg sets one.
  rm -rf "$test_home"
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' "${gw_conf[@]}" 'excluded_models=embed-only' \
    > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"additionalModelOptionsCache":[{"value":"plain-model"}]}' \
    > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":["sonnet","plain-model","embed-only"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"
  : > "$CCMORPH_TEST_LOG"

  _ccm_launch -a gw --model sol-alias --resume relay-session >/dev/null 2>&1 || {
    print -u2 -- 'the offline relaunch on a mapped alias must still start Claude Code'
    rc=1
  }
  local offline_allowed
  offline_allowed="$(_ccm_offered_models "$(<"$CCMORPH_TEST_LOG")")"
  [[ "$offline_allowed" == "sonnet,plain-model,sol-alias" ]] || {
    print -u2 -- "the offline rewrite must keep yesterday's list in order, still apply the exclusion, and add the relaunch's alias: got '$offline_allowed'"
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}

test_projected_rows_use_the_real_model_id() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Cloaking exists only so gateway DISCOVERY accepts an id -- it filters on
  # ^(claude|anthropic) before listing. Projected rows never go through discovery, and a
  # cloaked value is actively harmful: it starts with claude-, which makes Claude Code
  # skip CLAUDE_CODE_MAX_CONTEXT_TOKENS and hand the session 200,000 however the profile
  # is configured. Selecting a row labelled 372k then gave 200k. Both endpoints route the
  # real name, so that is what the row carries.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[{"id":"claude-fable-5-dd-hgihx-los-6.5-tpg","display_name":"GPT 5.6 Sol (xhigh)","max_input_tokens":372000}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  # A live session already picked something. Replacing the allowlist without keeping it
  # does not refuse that model, it remaps it onto Claude Code's catalogue -- which is how
  # a routed session ended up asking its local translator for a model the gateway
  # has never heard of, and getting a 502.
  print -r -- '{"model":"claude-fable-5-dd-hgihx-los-6.5-tpg"}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw x=1
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t gpt-5.6-sol
  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"
  local settings="$(<"$HOME/.config/ccmorph/profiles/gw/settings.json")"

  assert_contains "$state" '"value": "gpt-5.6-sol-xhigh"' \
    'a projected row must carry the real id, so the full window applies' || rc=1
  assert_not_contains "$state" '"value": "claude-fable-5-dd' \
    'a cloaked value would cap the session at 200,000' || rc=1
  # Superseded by rewriting the stored id outright: the session keeps the same model,
  # addressed by the name that gets the full window, and that name is allowed.
  assert_contains "$settings" '"model": "gpt-5.6-sol-xhigh"' \
    "a live session's chosen model must survive, remapping kills the session" || rc=1
  rm -rf "$test_home"
  return $rc
}

test_projection_undisguises_a_stored_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # A model chosen before the rows carried real ids stays in settings.json as the
  # disguised name, and a disguised name is what caps the session at 200,000. Leaving it
  # means the profile keeps the reduced window until the user happens to re-pick. The two
  # ids address the same model, so rewriting it changes nothing except the window.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[{"id":"claude-fable-5-dd-hgihx-los-6.5-tpg","display_name":"GPT 5.6 Sol (xhigh)","max_input_tokens":372000}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"model":"claude-fable-5-dd-hgihx-los-6.5-tpg"}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw x=1
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t gpt-5.6-sol
  local settings="$(<"$HOME/.config/ccmorph/profiles/gw/settings.json")"

  assert_contains "$settings" '"model": "gpt-5.6-sol-xhigh"' \
    'a stored disguised model must be rewritten to the id that gets the full window' || rc=1
  rm -rf "$test_home"
  return $rc
}


test_the_picker_offers_a_model_whatever_its_siblings_declare() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # This used to assert flat-detection semantics IN THE PICKER: two models declaring
  # the same window plus one declaring nothing is not "flat", so the two survived and
  # the third was dropped for want of a probe. The picker no longer decides a
  # row's existence from a window at all, so the flat question moved entirely to
  # _ccm_endpoint_window -- where it still matters (a flat default must not become a
  # session's real budget) and where it stays covered by
  # test_a_flat_endpoint_declaration_yields_to_probe_at_launch and
  # test_a_genuine_shared_window_is_not_mistaken_for_flat.
  #
  # What the picker owes now is simpler and is what this asserts: every chat-capable
  # model the endpoint serves is offered, whatever it or its siblings declare.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[' \
    '{"id":"a","context_length":200000},' \
    '{"id":"b","context_length":200000},' \
    '{"id":"c"}' \
    ']}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  write_profile_context gw unrelated=1
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 state
    _ccm_project_picker gw http://gw.invalid sk_t
  state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"
  assert_contains "$state" '"value": "a"' \
    'a declared model must be offered' || rc=1
  assert_contains "$state" '"value": "b"' \
    'a model declaring the same window as a sibling must be offered' || rc=1
  assert_contains "$state" '"value": "c"' \
    'a model declaring no window at all must be offered too' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_picker_follows_gateway_chat_capabilities() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Gateway capability declarations decide visibility, not model-name guesses.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[' \
    '{"id":"gpt-5.6-sol","capabilities":{"inference":true,"completion":false,"embeddings":true,"chat_completion":true,"image_generation":false}},' \
    '{"id":"text-embedding-3-large","capabilities":{"inference":true,"completion":false,"embeddings":true,"chat_completion":true,"image_generation":false}},' \
    '{"id":"text-embedding-3-small","capabilities":{"inference":true,"completion":false,"embeddings":true,"chat_completion":true,"image_generation":false}},' \
    '{"id":"bge-m3","capabilities":{"inference":true,"completion":false,"embeddings":true,"chat_completion":false,"image_generation":false}},' \
    '{"id":"gpt-image-2"}' \
    ']}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  # No registry file on purpose: this test's models are all unprobed, and the picker
  # no longer requires a registry to exist. An absent registry costs rows nothing.
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 state
    _ccm_project_picker gw http://gw.invalid sk_t
  state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"
  assert_contains "$state" '"value": "gpt-5.6-sol"' \
    'a real chat model with this exact capability block must still be offered' || rc=1
  assert_contains "$state" 'text-embedding-3-large' \
    'explicit chat capability must be honored even for an embedding-shaped name' || rc=1
  assert_contains "$state" 'text-embedding-3-small' \
    'catalogue visibility is independent of protocol classification' || rc=1
  assert_not_contains "$state" 'bge-m3' \
    'explicit chat_completion:false is excluded' || rc=1
  assert_contains "$state" 'gpt-image-2' \
    'missing capability metadata must not become a name-based exclusion' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_picker_skips_a_malformed_non_string_model_id() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[' \
    '{"id":{"bad":true},"context_length":300000},' \
    '{"id":"valid","context_length":400000},' \
    '{"id":[],"context_length":500000}' \
    ']}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  write_profile_context gw unrelated=1
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t
  assert_contains "$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")" '"value": "valid"' \
    'one malformed id must not abort every valid picker row' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_picker_uses_a_route_scoped_registry_probe() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[{"id":"route-only"}]}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' 'gw:route-*=700000' > "$HOME/registry.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t
  assert_contains "$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")" '"value": "route-only"' \
    'the picker must use the canonical route-scoped registry probe' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The unsafe-model warning computes a trigger too, and it must be the ENGINE's
# trigger. A hand-fitted `usable - round(usable*0.15)` sat here for eight versions
# and matches no engine function at all -- neither xWe nor KRt.
#
# Probed on a 200,000 window (what a plain claude- id gets), usable 180,000:
#
#   stale   min(180000 - round(180000*0.15), 167000) = 153,000
#   engine  min(floor(180000*(80/100)),      167000) = 144,000
#
# So a model holding 150,000 was NAMED as refusing before it compacts, when the
# engine compacts at 144,000 and it is perfectly safe -- a 9,000-token band of false
# alarms here, and 49,000 on a 1,000,000 window. Nothing covered this: the other test
# on this function exercises only the disguised-model warning.
# The unsafe warning must use the engine reserve, min(maxOutputTokens, 20000), not a
# flat 20,000. A pin below the cap makes the engine reserve LESS and trigger HIGHER,
# which is the direction that hides a fatality -- and _ccm_hazard_set cannot cover the
# gap, because it exits when no window is known, which is exactly a first launch with
# the endpoint down.
#
# Probed on a 200,000 window with no percentage override:
#
#   no pin (reserve 20,000)  usable 180,000  trigger 167,000  -> 170,000 wall safe
#   pin 8,000                usable 192,000  trigger 179,000  -> 170,000 wall FATAL
#
# A flat reserve computes 167,000 in both cases and leaves the second silent.
# The unsafe warning must look a ceiling up the way the sibling function does: strip
# the [1m] window marker, and try the profile-scoped key first. Doing neither made its
# own [1m] branch dead code -- the registry writer emits only bare ids -- and hid every
# profile-scoped row, of which the shipped registry holds eight.
#
# Probed on a 200,000-family id, no percentage override:
#
#   offered claude-foo[1m], registry claude-foo=300000
#     engine ceiling 1,000,000 -> trigger 967,000 >= 300,000 -> FATAL
#     unstripped lookup finds nothing -> silent
#   offered claude-x, registry gw:claude-x=160000
#     engine trigger 167,000 >= 160,000 -> FATAL
#     unscoped lookup finds nothing -> silent
# The two knobs that move the trigger are read once and handed to both consumers by
# NAME, not as delimited fields. An env value may contain any byte, and a conf `env.`
# line carrying a literal tab survives parsing intact -- so a tab-separated return
# split into the wrong fields, giving the percentage parser "8" and the reserve reader
# "0<TAB>9000": a trigger computed from a reserve the session does not have.
#
# The tab must stay inside the percentage (parseFloat takes the numeric prefix, so the
# engine reads 8 either way) and must never leak into the reserve.
test_the_trigger_env_survives_a_delimiter_in_a_value() {
  source "$CCMORPH_SOURCE"
  local rc=0 tab
  tab=$(printf '\t')

  local -a penv
  local p="" o="" n=""

  penv=("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=8${tab}0" CLAUDE_CODE_MAX_OUTPUT_TOKENS=9000)
  _ccm_trigger_env p o n
  [[ "$o" == 9000 ]] || {
    print -u2 -- "FAIL: the output reserve must not absorb a neighbouring field, got '$o'"
    rc=1
  }
  [[ "$p" == "8${tab}0" ]] || {
    print -u2 -- "FAIL: the percentage must arrive whole, got '$p'"
    rc=1
  }

  # All three, and the ordinary case still works.
  penv=(CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80 CLAUDE_CODE_MAX_OUTPUT_TOKENS=8000
        CLAUDE_CODE_DISABLE_1M_CONTEXT=1)
  p="" o="" n=""
  _ccm_trigger_env p o n
  [[ "$p" == 80 && "$o" == 8000 && "$n" == 1 ]] || {
    print -u2 -- "FAIL: all three knobs must arrive, got '$p' '$o' '$n'"
    rc=1
  }

  # A falsy kill switch reads as unset, so the [1m] tag keeps its meaning.
  penv=(CLAUDE_CODE_DISABLE_1M_CONTEXT=0)
  p="" o="" n=""
  _ccm_trigger_env p o n
  [[ -z "$n" ]] || {
    print -u2 -- "FAIL: CLAUDE_CODE_DISABLE_1M_CONTEXT=0 must read as unset, got '$n'"
    rc=1
  }

  return $rc
}

test_the_unsafe_warning_looks_up_a_ceiling_like_its_sibling() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  source "$CCMORPH_SOURCE"

  local rc=0 err

  # A [1m]-tagged entry against a bare registry key.
  print -r -- '{"availableModels":["claude-foo[1m]"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  write_profile_context gw claude-foo=300000
  err=$(_ccm_invariant_warn gw "" "" "" 2>&1)
  assert_contains "$err" 'claude-foo holds 300000' \
    'a [1m] entry must find its bare registry key' || rc=1

  # A profile-scoped key, the form the registry actually uses.
  print -r -- '{"availableModels":["claude-x"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  write_profile_context gw claude-x=160000
  err=$(_ccm_invariant_warn gw "" "" "" 2>&1)
  assert_contains "$err" 'claude-x holds 160000' \
    'a profile-scoped ceiling must be honoured here as it is elsewhere' || rc=1

  # A model above its trigger is still silent, so this cannot pass by naming all.
  write_profile_context gw claude-x=200000
  err=$(_ccm_invariant_warn gw "" "" "" 2>&1)
  assert_not_contains "$err" 'claude-x holds' \
    'a model above the trigger must not be named' || rc=1

  rm -rf "$test_home"
  return $rc
}

# The allowlist above is also the sweep: `_ccm_run_claude` unsets every key on it except the tuning keys (_CCMORPH_TUNING_ENV: the shell value passes through unless the profile declares one), so a
# profile that omits one gets Claude Code's default rather than whatever this shell
# happens to export. That is the invariant an earlier permitted-but-never-cleared split
# broke, so pin it BEHAVIOURALLY -- an earlier version asserted on the source text and
# was defeated twice: `grep -A6` stopped inside the comment block once it grew, and
# naming the array said nothing about the loop body, so replacing `unset` with `:` kept
# it green.
test_the_launch_sweep_clears_every_allowlisted_env_key() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 k

  # Every key the engine reads for a window or a compaction trigger must be on the
  # list, or an ambient export reaches the engine while ccmorph computes from a
  # different value.
  local -a must_sweep=(
    CLAUDE_CODE_MAX_CONTEXT_TOKENS
    CLAUDE_CODE_MAX_OUTPUT_TOKENS
    CLAUDE_CODE_AUTO_COMPACT_WINDOW
    CLAUDE_AUTOCOMPACT_PCT_OVERRIDE
    CLAUDE_CODE_DISABLE_1M_CONTEXT
    CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT
    CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY
  )
  for k in "${must_sweep[@]}"; do
    (( ${_CCMORPH_PROFILE_ENV[(Ie)$k]} )) || {
      print -u2 -- "FAIL: $k reaches the engine unswept; add it to _CCMORPH_PROFILE_ENV"
      rc=1
    }
  done

  # Export the whole list, enter the launch path with a stub `claude` that reports
  # what survived, and require the report to be empty.
  printf '%s\n' '#!/bin/sh' 'env | grep -E "^(CLAUDE_|ANTHROPIC_|CCMORPH_|MAX_THINKING)" | cut -d= -f1 | sort' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"

  local survived
  survived=$(
    unset CLAUDECODE CLAUDE_CODE_CHILD_SESSION  # a top-level shell; the runner may itself be Claude Code
    for k in "${_CCMORPH_PROFILE_ENV[@]}"; do
      export "$k=ambient-leak"
    done
    _ccm_run_claude --
  )

  for k in "${_CCMORPH_PROFILE_ENV[@]}"; do
    # The repair flag is re-exported deliberately by the launch path itself when
    # a profile asks for it; here no profile does, so nothing survives.
    # Client tuning keys are the one deliberate exception (#53): the shell's value
    # passes through when the profile does not declare the key.
    if (( ${_CCMORPH_TUNING_ENV[(Ie)$k]} )); then
      print -r -- "$survived" | grep -qx -- "$k" || {
        print -u2 -- "FAIL: tuning key $k did not pass through from the shell"
        rc=1
      }
    elif print -r -- "$survived" | grep -qx -- "$k"; then
      print -u2 -- "FAIL: $k survived the launch sweep; an ambient export reaches the engine"
      rc=1
    fi
  done

  rm -rf "$test_home"
  return $rc
}

# xe() is `if (Wn()) return false; return /\[1m\]/i.test(e)`, and Wn() reads
# CLAUDE_CODE_DISABLE_1M_CONTEXT -- so with that set a [1m] id resolves at its family
# default, not 1,000,000. Computing the trigger from 1e6 anyway names a safe model
# fatal, and CCMORPH_WITHHOLD_FATAL=1 then drops a model that works.
#
# Probed: claude-foo[1m] with registry claude-foo=300000, pct 80.
#   kill switch unset -> ceiling 1,000,000, trigger 784,000 >= 300,000 -> FATAL
#   kill switch set   -> ceiling   200,000, trigger 144,000 <  300,000 -> safe
# The kill switch has to be REACHABLE, and it has to mean what the engine means.
#
# Reachable: `env.*` conf keys are rejected unless the key is in _CCMORPH_PROFILE_ENV,
# and the launcher clears only keys on that list -- so without membership a conf could
# not set it AND an ambient export reached the engine invisibly. Both halves are the
# same missing entry.
#
# Meaning: the engine env boolean, read out of 2.1.247, is
#
#   function p(e){ if(!e) return false; if (typeof e === "boolean") return e;
#                  let n = String(e).toLowerCase().trim();
#                  return ["1","true","yes","on"].includes(n) }
#
# so `off`, `no`, `false` and `2` are all FALSE, and a padded ` on ` is TRUE. Treating
# anything non-empty and non-zero as true inverted every one of those -- and that is the
# under-warn direction: ccmorph sizes a [1m] id at 200,000 where the engine gives it
# 1,000,000, and the wedge goes unnamed.
test_the_kill_switch_is_settable_and_reads_like_the_engine() {
  source "$CCMORPH_SOURCE"
  local rc=0

  # It must be declarable in a conf at all.
  (( ${_CCMORPH_PROFILE_ENV[(Ie)CLAUDE_CODE_DISABLE_1M_CONTEXT]} )) || {
    print -u2 -- "FAIL: the kill switch must be an allowlisted profile env key"
    rc=1
  }

  local -a penv
  local p o n v
  # Every value the engine calls TRUE, including padded and mixed case.
  for v in 1 true yes on TRUE YES 'On' ' on ' 'yes '; do
    p="" o="" n=""
    penv=("CLAUDE_CODE_DISABLE_1M_CONTEXT=$v")
    _ccm_trigger_env p o n
    [[ "$n" == 1 ]] || {
      print -u2 -- "FAIL: '$v' must read as set, got '$n'"
      rc=1
    }
  done
  # U+FEFF is in the ECMAScript WhiteSpace table, so the engine TRIMS it: `<FEFF>on`
  # is true there. zsh [[:space:]] does not match U+FEFF, and neither does Python
  # str.strip -- so checking against Python said the two agreed while they did not.
  # Same character, same wrong oracle, as the percentage-parser whitespace gap.
  local feff
  feff=$(printf '\ufeff')
  for v in "${feff}on" "${feff}yes" "${feff}1"; do
    p="" o="" n=""
    penv=("CLAUDE_CODE_DISABLE_1M_CONTEXT=$v")
    _ccm_trigger_env p o n
    [[ "$n" == 1 ]] || {
      print -u2 -- "FAIL: a FEFF-padded true value must read as set, got '$n'"
      rc=1
    }
  done

  # Under a C charmap a bracket class matches BYTES, so a multibyte character whose UTF-8
  # bytes all appear in the class was eaten byte by byte: U+3080 (e3 82 80) trimmed away
  # and `<U+3080>on` read as TRUE where the engine reads false. Whole-character comparison
  # is exact in every locale. NEL is the other direction -- the engine does NOT trim it,
  # so it must stay false.
  local u3080 nel
  u3080=$(printf '\xe3\x82\x80')
  nel=$(printf '\xc2\x85')

  # And every value it calls FALSE.
  for v in off no false FALSE 0 2 onx '' "${feff}off" "${u3080}on" "${nel}on"; do
    p="" o="" n=""
    penv=("CLAUDE_CODE_DISABLE_1M_CONTEXT=$v")
    # LC_ALL=C deliberately: a bracket class matches BYTES under a C charmap, so the
    # U+3080 case only distinguishes whole-character trimming from a class HERE. Run in
    # the ambient locale it passes either way, which is how this nearly shipped unfixed.
    LC_ALL=C _ccm_trigger_env p o n
    [[ -z "$n" ]] || {
      print -u2 -- "FAIL: '$v' must read as unset, got '$n'"
      rc=1
    }
  done

  return $rc
}

# xe() is the UNANCHORED regex /\[1m\]/i, so the tag counts anywhere in the id -- not
# only at the end. Gateways version-suffix ids freely, so `claude-x[1m]-eu` is a real
# shape, and the engine gives it 1,000,000.
#
# Probed: registry claude-x=300000, no percentage override.
#   engine    ceiling 1,000,000 -> trigger 967,000 >= 300,000 -> FATAL
#   endswith  tag missed -> claude- prefix -> 200,000 -> trigger 167,000 -> silent
test_the_one_million_tag_counts_anywhere_in_the_id() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"availableModels":["claude-x[1m]-eu"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  write_profile_context gw claude-x=300000
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(_ccm_invariant_warn gw "" "" "" 2>&1)
  assert_contains "$err" 'claude-x holds 300000' \
    'a [1m] tag anywhere in the id means 1,000,000, as the engine regex is unanchored' || rc=1

  # An id with no tag at all is still sized at 200,000 and stays safe. The registry MUST
  # carry this exact id: with only `claude-x` present, the untagged `claude-x-eu` found
  # no wall at all, so the assertion passed on a lookup MISS rather than on the ceiling
  # -- and an "every id is 1M" mutant passed both halves. Verified: with the wall
  # present the correct code still stays silent (trigger 167,000 < 300,000) and that
  # mutant is caught.
  write_profile_context gw claude-x=300000 claude-x-eu=300000
  print -r -- '{"availableModels":["claude-x-eu"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  err=$(_ccm_invariant_warn gw "" "" "" 2>&1)
  assert_not_contains "$err" 'holds 300000' \
    'an untagged id keeps its 200,000 family ceiling and stays safe' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_one_million_kill_switch_lowers_the_ceiling() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"availableModels":["claude-foo[1m]"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  write_profile_context gw claude-foo=300000
  source "$CCMORPH_SOURCE"

  local rc=0 err
  # Unset: the tag means 1,000,000 and the model is fatal.
  err=$(_ccm_invariant_warn gw 80 "" "" 2>&1)
  assert_contains "$err" 'claude-foo holds 300000' \
    'without the kill switch a [1m] id is sized at 1,000,000' || rc=1

  # Set: the tag means nothing, so the model is safe and must not be named.
  err=$(_ccm_invariant_warn gw 80 "" 1 2>&1)
  assert_not_contains "$err" 'claude-foo holds' \
    'with the kill switch set a [1m] id falls back to its family default' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_unsafe_warning_uses_the_engine_output_reserve() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"availableModels":["claude-x"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  write_profile_context gw claude-x=170000
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(_ccm_invariant_warn gw "" 8000 2>&1)
  assert_contains "$err" 'claude-x holds 170000' \
    'a small output pin raises the trigger, and the model above it is fatal' || rc=1

  # Without the pin the same model is safe, so this cannot pass by naming everything.
  err=$(_ccm_invariant_warn gw "" "" 2>&1)
  assert_not_contains "$err" 'claude-x holds' \
    'at the default reserve the same model is safe and must not be named' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_unsafe_warning_uses_the_engine_trigger() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{"availableModels":["claude-small"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # 150,000 sits between the engine trigger (144,000) and the stale one (153,000).
  write_profile_context gw claude-small=150000
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(_ccm_invariant_warn gw 80 2>&1)
  assert_not_contains "$err" 'claude-small holds' \
    'a model above the engine trigger is safe and must not be named' || rc=1

  # And a model genuinely BELOW the engine trigger is still named, so this test
  # cannot pass by the warning simply having gone silent.
  write_profile_context gw claude-small=100000
  err=$(_ccm_invariant_warn gw 80 2>&1)
  assert_contains "$err" 'claude-small holds 100000' \
    'a model below the engine trigger must still be named' || rc=1

  rm -rf "$test_home"
  return $rc
}

# The unsafe-model warning computed its trigger from the percentage in the launch
# env alone. A percentage the SETTINGS LAYERS hold reaches the engine through the
# settings file, so the launch injects nothing over it by design -- and the check
# then saw an empty percentage and computed the no-percentage trigger
# `usable - 13000` instead, naming every model between the two as fatal. (#428)
#
# Probed: registry claude-small=150000, canon settings
# env CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=70, no launch-env percentage.
#
#   layer 70  usable 180,000 -> trigger 126,000 <  150,000 -> SAFE, must be silent
#   no pct    usable 180,000 -> trigger 167,000 >= 150,000 -> FATAL (the bug)
test_the_unsafe_warning_uses_a_layer_only_percentage() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  # The user's own trigger, set where /autocompact and a hand edit both land.
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"70"}}' \
    > "$HOME/.claude/settings.json"
  print -r -- '{"availableModels":["claude-small"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  # 150,000 sits between the layer trigger (126,000) and the no-percentage one
  # (167,000), so only the layer value decides whether it is named.
  write_profile_context gw claude-small=150000
  source "$CCMORPH_SOURCE"

  local rc=0 err
  # The launch's shape when the layers hold the value: the env injects nothing, so
  # the percentage reading is empty and the key is absent from the launch env.
  err=$(_ccm_invariant_warn gw "" "" "" "" 0 2>&1)
  assert_not_contains "$err" 'claude-small holds' \
    'a model above the layer trigger is safe and must not be named' || rc=1

  # A model genuinely BELOW the layer trigger is still named, so this test cannot
  # pass by the warning having gone silent.
  write_profile_context gw claude-small=100000
  err=$(_ccm_invariant_warn gw "" "" "" "" 0 2>&1)
  assert_contains "$err" 'claude-small holds 100000' \
    'a model below the layer trigger must still be named' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_launch_warns_about_a_hand_set_disguised_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  # /model accepts an id outside the catalogue, so a disguised name can still be typed in
  # by hand -- and it silently caps the session at 200,000 with nothing said.
  print -r -- '{"model":"claude-fable-5-dd-los-6.5-tpg","availableModels":["gpt-5.6-sol"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw x=1
  source "$CCMORPH_SOURCE"

  local rc=0 err
  err=$(_ccm_invariant_warn gw 2>&1)
  assert_contains "$err" '200,000' \
    'a disguised session model must be reported with what it costs' || rc=1
  assert_contains "$err" 'gpt-5.6-sol' \
    'the warning must name the id that gets the full window' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_projection_omits_models_that_cannot_serve_a_session() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Claude Code only ever calls /v1/messages. An image model answers that with
  # "only supported on /v1/images/generations", so selecting its row 503s on the first
  # turn. Nothing in its /v1/models record says so -- the shape is identical to a chat
  # model's -- which is why this is a name list and why it is worth a test.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[' \
    '{"id":"claude-fable-5-dd-2-egami-tpg","display_name":"GPT Image 2","max_input_tokens":200000},' \
    '{"id":"claude-fable-5-dd-los-6.5-tpg","display_name":"GPT 5.6 Sol","max_input_tokens":372000}' \
    ']}' 'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=sk_t' 'model=gpt-5.6-sol' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw x=1
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t gpt-5.6-sol
  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"

  assert_not_contains "$state" 'Image' \
    'a model that cannot answer /v1/messages must not be offered' || rc=1
  assert_contains "$state" 'gpt-5.6-sol' \
    'a chat model beside it must still be offered' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_setup_dir_shares_output_styles_with_canonical() {
  # Regression guard: every read-only content dir in `_ccm_setup_dir`'s shared[]
  # set must become a symlink into ~/.claude so behaviour does not fork per
  # profile. output-styles was omitted from that set, so the output style never
  # went global across profiles — this asserts it (and its siblings) are shared.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"

  # canonical content the shared[] set links to
  mkdir -p "$HOME/.claude/output-styles" "$HOME/.claude/commands" \
           "$HOME/.claude/skills" "$HOME/.claude/hooks" "$HOME/.claude/plugins"
  : > "$HOME/.claude/output-styles/i-have-adhd.md"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"

  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type sub)
  _ccm_setup_dir_unlocked gw || {
    unset _ccm
    print -u2 -- 'setup_dir must provision the profile'
    rm -rf "$test_home"; return 1
  }
  unset _ccm

  local rc=0 item link src
  for item in output-styles commands skills hooks plugins agents; do
    link="$HOME/.config/ccmorph/profiles/gw/$item"
    src="$HOME/.claude/$item"
    if [[ ! -L "$link" || "${link:A}" != "${src:A}" ]]; then
      print -u2 -- "'$item' must be a symlink into ~/.claude (was: ${link}=$(readlink $link 2>/dev/null || echo not-a-symlink))"
      rc=1
    fi
  done
  rm -rf "$test_home"
  return $rc
}

test_setup_dir_removes_a_stale_ccmorph_statusline() {
  # v1.7.x auto-wired a ccmorph statusline into every profile's settings.json.
  # The engine-level window mechanism replaced it, and the script no longer
  # ships -- a leftover entry would spawn a missing command on every render.
  # Provisioning is the chokepoint every profile passes through, so the same
  # place that wired it cleans it up.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' \
    "{\"statusLine\":{\"type\":\"command\",\"command\":\"$HOME/.config/ccmorph/statusline\"},\"model\":\"kept\"}" \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"

  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type sub)
  _ccm_setup_dir_unlocked gw || {
    unset _ccm
    print -u2 -- 'setup_dir must provision the profile'
    rm -rf "$test_home"; return 1
  }
  unset _ccm

  local rc=0 state
  state=$(python3 -c '
import json, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    doc = json.load(fh)
print((doc.get("statusLine") or {}).get("command") or "none", doc.get("model"))
' "$HOME/.config/ccmorph/profiles/gw/settings.json")

  if [[ "$state" != "none kept" ]]; then
    print -u2 -- "a stale ccmorph statusline must be unwired, other keys kept (got: $state)"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

# The cleanup matched `command.endswith("/ccmorph/statusline")` -- a path SUFFIX,
# not the path ccmorph ever wrote. So a user's own bar living in any directory
# named `ccmorph/` was deleted, while the function's own comment promised "a
# user's own bar is never touched".
#
# Git settles the ownership question: the v1.7 wire function (faa1ea9) wrote exactly
# `$_CCMORPH_LIB_DIR/statusline` and REFUSED to overwrite an existing command
# ("Already ours -> nothing to do. Someone else's -> theirs to keep."). So no user
# bar was ever behind a ccmorph-wired entry, and matching a broader class than was
# ever installed can only ever destroy something else.
#
# The paired test above proves a non-matching command survives; only a COLLIDING
# one exercises the precision of the guard, which is why this case existed unseen.
test_cleanup_keeps_a_user_bar_whose_path_merely_collides() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  # A personal bar someone wrote FOR their ccmorph setup: the likeliest collision.
  printf '%s\n' \
    '{"statusLine":{"type":"command","command":"/Users/me/bin/ccmorph/statusline"},"model":"kept"}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_unwire_statusline "$HOME/.config/ccmorph/profiles/gw"

  local got
  got=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print((d.get("statusLine") or {}).get("command") or "<removed>", d.get("model"))
' "$HOME/.config/ccmorph/profiles/gw/settings.json")
  [[ "$got" == "/Users/me/bin/ccmorph/statusline kept" ]] || {
    print -u2 -- "a colliding user bar must survive cleanup (got: $got)"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

# What ccmorph DID write must still be cleaned up, from either directory it ever
# used: the repo splits state-dir (_CCMORPH_DIR) from lib-dir (_CCMORPH_LIB_DIR),
# so a pre-1.8 profile can hold either path. Matching only one would leave a real
# leftover spawning a missing command on every render.
test_cleanup_removes_the_statusline_ccmorph_itself_wired() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  source "$CCMORPH_SOURCE"

  local rc=0 dir
  for dir in "$_CCMORPH_DIR" "$_CCMORPH_LIB_DIR"; do
    mkdir -p "$HOME/.config/ccmorph/profiles/gw"
    print -r -- "{\"statusLine\":{\"type\":\"command\",\"command\":\"$dir/statusline\"},\"model\":\"kept\"}" \
      > "$HOME/.config/ccmorph/profiles/gw/settings.json"
    _ccm_unwire_statusline "$HOME/.config/ccmorph/profiles/gw"
    local got
    got=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print("statusLine" in d, d.get("model"))
' "$HOME/.config/ccmorph/profiles/gw/settings.json")
    [[ "$got" == "False kept" ]] || {
      print -u2 -- "ccmorph's own statusline at $dir must be cleaned up (got: $got)"
      rc=1
    }
  done
  rm -rf "$test_home"
  return $rc
}

test_setup_dir_never_touches_a_users_own_statusline() {
  # Someone with their own bar (a themed one, a different tool's) must keep it.
  # The cleanup removes only what ccmorph itself wired -- never a deliberate
  # user choice.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"statusLine":{"type":"command","command":"/my/own/bar.sh"}}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"

  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type sub)
  _ccm_setup_dir_unlocked gw || {
    unset _ccm
    print -u2 -- 'setup_dir must provision the profile'
    rm -rf "$test_home"; return 1
  }
  unset _ccm

  local rc=0 wired
  wired=$(python3 -c '
import json, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    print((json.load(fh).get("statusLine") or {}).get("command") or "")
' "$HOME/.config/ccmorph/profiles/gw/settings.json")

  if [[ "$wired" != /my/own/bar.sh ]]; then
    print -u2 -- "a user's own statusLine must survive provisioning (got: $wired)"
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_no_statusline_machinery_ships() {
  # The per-model context window is delivered engine-side (the route-ceiling env
  # plus the native autoCompactWindow setting), so ccmorph ships no display
  # machinery: no script, no subcommand, no wiring, no README mention. A display
  # can only repaint a number; it can never move the compaction trigger.
  local rc=0

  if [[ -e "$REPO_ROOT/statusline" ]]; then
    print -u2 -- 'ccmorph must not ship a statusline script'
    rc=1
  fi
  if grep -qE '^\s+statusline\)' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'ccmorph must expose no `statusline` subcommand'
    rc=1
  fi
  if grep -q '_ccm_wire_statusline' "${CCMORPH_LIBRARY[@]}"; then
    print -u2 -- 'provisioning must not wire a statusline'
    rc=1
  fi
  if grep -qiE '(ccmorph|^\s*\$) statusline' "$REPO_ROOT/README.md"; then
    print -u2 -- 'the README must not teach a statusline command'
    rc=1
  fi
  return $rc
}

test_setup_dir_is_idempotent_on_settings() {
  # Provisioning runs on EVERY launch, so any pass that rewrote settings.json
  # each time would churn the file (and its mtime) forever.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"

  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type sub)
  _ccm_setup_dir_unlocked gw >/dev/null 2>&1
  local first
  first=$(cat "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  _ccm_setup_dir_unlocked gw >/dev/null 2>&1
  local second
  second=$(cat "$HOME/.config/ccmorph/profiles/gw/settings.json" 2>/dev/null)
  unset _ccm

  local rc=0
  if [[ "$first" != "$second" ]]; then
    print -u2 -- 'repeated provisioning must not rewrite settings.json'
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_setup_dir_links_the_harness_even_before_content_exists() {
  # A colleague adds profiles FIRST and installs skills/plugins later. The
  # shared dirs must be created in ~/.claude and linked at provisioning even
  # when empty, so later content reaches every profile with no re-add.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"
  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type sub)
  _ccm_setup_dir_unlocked gw || {
    unset _ccm; print -u2 -- 'setup_dir must provision the profile'
    rm -rf "$test_home"; return 1
  }
  unset _ccm

  local rc=0 item link src
  for item in skills hooks plugins commands output-styles session-env file-history; do
    link="$HOME/.config/ccmorph/profiles/gw/$item"
    src="$HOME/.claude/$item"
    [[ -d "$src" ]] || { print -u2 -- "canonical '$item' must be created"; rc=1 }
    if [[ ! -L "$link" || "${link:A}" != "${src:A}" ]]; then
      print -u2 -- "'$item' must be linked even with no content yet"
      rc=1
    fi
  done
  rm -rf "$test_home"
  return $rc
}

test_late_harness_files_heal_on_next_setup() {
  # A CLAUDE.md (or keybindings.json) written to ~/.claude AFTER the profile
  # existed must reach the profile on the next launch's setup pass — the
  # fast path must notice the missing link, not short-circuit past it.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  # apikey, not sub: an apikey profile writes config.json at provisioning, so
  # the fast path genuinely engages — the case that used to skip the heal.
  printf '%s\n' 'type=apikey' 'api_key=sk-test' 'base_url=https://gw.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"
  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type apikey)
  _ccm_setup_dir_unlocked gw || { unset _ccm; rm -rf "$test_home"; return 1 }
  unset _ccm

  local rc=0
  [[ -e "$HOME/.config/ccmorph/profiles/gw/CLAUDE.md" ]] && {
    print -u2 -- 'sanity: no CLAUDE.md link expected before the file exists'
    rc=1
  }
  print 'shared memory' > "$HOME/.claude/CLAUDE.md"
  typeset -gA _ccm=(type apikey)
  _ccm_setup_dir gw || { unset _ccm; rm -rf "$test_home"; return 1 }
  unset _ccm
  [[ -L "$HOME/.config/ccmorph/profiles/gw/CLAUDE.md" && \
     "$(<"$HOME/.config/ccmorph/profiles/gw/CLAUDE.md")" == 'shared memory' ]] \
    || { print -u2 -- 'late CLAUDE.md must be linked by the next setup pass'; rc=1 }
  rm -rf "$test_home"
  return $rc
}

test_profile_local_harness_content_survives_an_upgrade() {
  # A v1.0 profile may hold a REAL skills dir with content. Skills belong to
  # their owning tool, so provisioning refuses and moves nothing rather than
  # merging (see tests/shared_recovery_test.py) — and never loses a file. A real
  # profile-local FILE (CLAUDE.md) is a deliberate override: respected.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw/skills/my-skill"
  print 'local skill' > "$HOME/.config/ccmorph/profiles/gw/skills/my-skill/SKILL.md"
  print 'local override' > "$HOME/.config/ccmorph/profiles/gw/CLAUDE.md"
  print 'canonical memory' > "$HOME/.claude/CLAUDE.md"
  source "$CCMORPH_SOURCE"
  typeset -gA _ccm=(type sub)
  local rc=0 err
  err=$(_ccm_setup_dir_unlocked gw 2>&1) \
    && { print -u2 -- 'a skills collision must refuse, not merge'; rc=1 }
  unset _ccm
  [[ $err == *'nothing moved'* ]] \
    || { print -u2 -- "refusal must say nothing moved: $err"; rc=1 }
  [[ "$(<"$HOME/.config/ccmorph/profiles/gw/skills/my-skill/SKILL.md")" == 'local skill' ]] \
    || { print -u2 -- 'profile-local skill content must stay in place'; rc=1 }
  [[ ! -e "$HOME/.claude/skills/my-skill" ]] \
    || { print -u2 -- 'nothing may move into the shared store'; rc=1 }
  [[ ! -L "$HOME/.config/ccmorph/profiles/gw/CLAUDE.md" && \
     "$(<"$HOME/.config/ccmorph/profiles/gw/CLAUDE.md")" == 'local override' ]] \
    || { print -u2 -- 'a real profile-local file is an override, never replaced'; rc=1 }
  rm -rf "$test_home"
  return $rc
}

# Record scheduling without starting work; any direct invocation is a regression.
fake_context_refresh() {
  printf '%s\n' '#!/bin/sh' \
    'printf "%s\\n" "$*" >> "$HOME/refresh.direct"' \
    'exit 1' > "$CCMORPH_LIB_DIR/../libexec/refresh-model-registry"
  chmod +x "$CCMORPH_LIB_DIR/../libexec/refresh-model-registry"
  _ccm_spawn_bg_logged() { print -r -- "$*" >> "$HOME/background.args"; }
  _ccm_endpoint_window() { print called >> "$HOME/context.endpoint"; return 1; }
}

test_add_context_refresh_is_background_and_declared_only() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  fake_context_refresh
  local out rc=0
  out=$(print -l apikey sk-test-not-real 'http://gw.invalid' '' | ccmorph add gw 2>&1)
  assert_contains "$out" 'Added' 'adding a gateway must still succeed' || rc=1
  local scheduled=""
  [[ -f "$HOME/background.args" ]] && scheduled="$(<"$HOME/background.args")"
  assert_contains "$scheduled" 'refresh-model-registry --declared-only --profile gw --deadline 30' \
    'add must schedule only a bounded, free metadata refresh' || rc=1
  [[ ! -f "$HOME/refresh.direct" ]] || { print -u2 'add synchronously ran the refresh'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_launch_resolves_scoped_registry_globs() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  install_model_window
  write_apikey_conf gw 'api_key=test-key' 'base_url=https://gateway.invalid' 'model=gpt-example'
  print -r -- 'gw:gpt-*=333333' > "$HOME/.config/ccmorph/model-registry"
  source "$CCMORPH_SOURCE"
  fake_context_refresh
  local rc=0
  _ccm_launch -a gw prompt >/dev/null 2>&1 || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=333333' \
    'launch must use canonical scoped wildcard registry matching' || rc=1
  assert_not_contains "$(<"$HOME/background.args")" 'refresh-model-registry' \
    'a wildcard-known window needs no refresh' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_context_refresh_does_not_wait_for_metadata() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  # Exercise the actual background helper with a bounded slow executable, not
  # network traffic. Its completion belongs to the log, never to the caller.
  printf '%s\n' '#!/usr/bin/env python3' 'import time' \
    'time.sleep(2)' 'print("metadata complete", flush=True)' \
    > "$CCMORPH_LIB_DIR/../libexec/refresh-model-registry"
  chmod +x "$CCMORPH_LIB_DIR/../libexec/refresh-model-registry"
  local _CCMORPH_LOG="$HOME/refresh.log"
  local rc=0
  _ccm_probe_nudge gw
  # The helper sleeps 2 s before it says anything, so a completion line in the
  # log at this instant would mean the caller waited for it. Read the event, not
  # the clock: how long the return took is the scheduler's business, and a
  # loaded host can spend a second on a spawn the test has no claim about.
  assert_not_contains "$(<"$HOME/refresh.log")" 'metadata complete' \
    'the nudge must return before the background helper completes' || rc=1
  PYTHONPATH="${REPO_ROOT}/tests" python3 - "$HOME/refresh.log" <<'PY' || rc=1
from pathlib import Path
import sys, time
from support import HANG_TIMEOUT
path = Path(sys.argv[1])
deadline = time.monotonic() + HANG_TIMEOUT
while time.monotonic() < deadline:
    if path.exists() and "metadata complete" in path.read_text():
        break
    time.sleep(0.05)
else:
    raise SystemExit("background metadata never completed in the log")
PY
  rm -rf "$test_home"
  return $rc
}

test_launch_context_lookup_is_local_before_background_refresh() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  install_model_window
  write_apikey_conf gw 'api_key=test-key' 'base_url=https://gateway.invalid' 'model=opaque-model'
  source "$CCMORPH_SOURCE"
  fake_context_refresh
  local rc=0 actual scheduled
  _ccm_launch -a gw prompt >/dev/null 2>&1 || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  scheduled="$(<"$HOME/background.args")"
  assert_not_contains "$actual" 'Context limit unknown for opaque-model' \
    'unknown context is a silent safe default, not a warning' || rc=1
  assert_contains "$scheduled" 'refresh-model-registry --declared-only --profile gw --deadline 30' \
    'an unknown launch must schedule free metadata work off its critical path' || rc=1
  [[ ! -f "$HOME/context.endpoint" ]] || { print -u2 'launch fetched context synchronously'; rc=1; }
  [[ ! -f "$HOME/refresh.direct" ]] || { print -u2 'launch ran the refresh synchronously'; rc=1; }

  # A remembered window is useful even if the registry has no entry. Launching it
  # again must neither fetch context nor schedule another context refresh.
  mkdir -p "$HOME/.config/ccmorph/profiles/gw/cache"
  print -r -- $'other-model\t123456\nopaque-model\t372000' \
    > "$HOME/.config/ccmorph/profiles/gw/cache/context-window"
  : > "$HOME/background.args"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a gw prompt >/dev/null 2>&1 || rc=1
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=372000' 'a cached model keeps its known ceiling' || rc=1
  assert_contains "$actual" '"autoCompactWindow":372000' 'the cache supplies the native budget' || rc=1
  assert_not_contains "$(<"$HOME/background.args")" 'refresh-model-registry' \
    'a known cached window must not schedule redundant work' || rc=1

  # A route-specific registry update outranks the older per-model cache.
  write_profile_context gw opaque-model=512000
  : > "$HOME/background.args"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a gw prompt >/dev/null 2>&1 || rc=1
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'MAX_CONTEXT=512000' \
    'registry state must precede the remembered launch cache' || rc=1
  assert_not_contains "$(<"$HOME/background.args")" 'refresh-model-registry' \
    'a known registry window must not schedule redundant work' || rc=1
  [[ ! -f "$HOME/context.endpoint" && ! -f "$HOME/refresh.direct" ]] || rc=1
  rm -rf "$test_home"
  return $rc
}

test_probe_dispatch_preserves_explicit_inference_arguments() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  printf '%s\n' '#!/bin/sh' 'printf "%s\\n" "$@" > "$HOME/probe.args"' \
    > "$CCMORPH_LIB_DIR/../libexec/refresh-model-registry"
  chmod +x "$CCMORPH_LIB_DIR/../libexec/refresh-model-registry"
  local out rc=0
  out=$(ccmorph probe --profile gw --model opaque-model --allow-inference --deadline 30)
  [[ "$(<"$HOME/probe.args")" == $'--profile\ngw\n--model\nopaque-model\n--allow-inference\n--deadline\n30' ]] || {
    print -u2 'probe must preserve option values rather than turn them into profile flags'; rc=1
  }
  out=$(ccmorph probe gw --declared-only 2>&1)
  [[ "$(<"$HOME/probe.args")" == $'--profile\ngw\n--declared-only' ]] || rc=1
  # Metadata-only is the default and needs no banner; only paid mode warns.
  assert_not_contains "$out" 'may incur charges' 'the default run must not print the paid-mode warning' || rc=1
  assert_not_contains "$out" 'one probe per model' 'the default must not promise an inference sweep' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_add_gateway_profile_offers_probe() {
  # A colleague connecting a gateway does not know `ccmorph probe` exists.
  # Adding a routed profile harvests the free listing-stated sizes on the spot
  # and — when the route states none — says what to run, right there.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  source "$CCMORPH_SOURCE"
  local out rc=0
  out=$(print -l apikey sk-test-not-real 'http://gw.invalid' '' | ccmorph add gw 2>&1)
  # The nudge runs against an unreachable fake gateway, so it may produce no
  # window-related output — that is correct: a down endpoint is not an error.
  # Verify the profile was created and the nudge did not abort the add.
  [[ "$out" == *"Added"* ]] \
    || { print -u2 -- 'adding a routed profile must succeed even when the endpoint is unreachable'; rc=1 }
  rm -rf "$test_home"
  return $rc
}

test_add_native_profile_stays_quiet_about_probe() {
  # Native Anthropic needs no probing — the nudge there would be noise.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  export _CCMORPH_PROFILE_DIRS="$HOME/.config/ccmorph/profiles"
  source "$CCMORPH_SOURCE"
  local out rc=0
  out=$(print -l apikey sk-test-not-real '' '' | ccmorph add nat 2>&1)
  [[ "$out" == *"ccmorph probe"* ]] \
    && { print -u2 -- 'a native profile must not be nudged to probe'; rc=1 }
  rm -rf "$test_home"
  return $rc
}

test_an_unprobed_launch_is_not_given_a_siblings_ceiling() {
  # The route ceiling is the LARGEST probed window among the models a route serves, so
  # on a mixed route an unprobed model would inherit a sibling's number -- and the
  # branch that narrows it back via autoCompactWindow only runs for a PROBED model.
  # Probed 2026-09-08 on a real route: an unprobed model launched at
  # CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576 inherited from kimi-k3, a 5x over-promise,
  # with nothing to compact it before the endpoint refused. The advisory says the session
  # runs at Claude Code's default; this asserts it actually does.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"object":"list","data":[{"id":"unprobed-model"},{"id":"big-sibling"}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  # The clamp asks model-window for the unprobed default rather than hardcoding one,
  # so the resolver has to be present for it to fire at all.
  install_model_window
  # The sibling is probed far above the default; the launched model is not probed.
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    'model=unprobed-model' > "$HOME/.config/ccmorph/mix.conf"
  write_profile_context mix big-sibling=1048576
  mkdir -p "$HOME/.config/ccmorph/profiles/mix"
  print -r -- '{"availableModels":["unprobed-model","big-sibling"]}' \
    > "$HOME/.config/ccmorph/profiles/mix/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  # The suite's own shell may export CLAUDE_CODE_MAX_CONTEXT_TOKENS, which is rung 1 of
  # model-window's precedence and would answer "probed" for every model. _ccm_launch
  # already keeps the ambient value out of the CHILD's env, but the clamp consults the
  # resolver in THIS shell, so it is unset here too.
    command env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS zsh -c \
      "source '$CCMORPH_SOURCE' >/dev/null 2>&1; _ccm_launch -a mix prompt" \
      >/dev/null 2>&1 || rc=1
  # The fake claude records the env it was handed as MAX_CONTEXT=<value>.
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MAX_CONTEXT=1048576' \
    'later model switches must retain the route-wide ceiling' || rc=1
  assert_contains "$actual" '"autoCompactWindow":128000' \
    'the unknown launch model must receive conservative compaction, not a process-wide clamp' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_launch_window_reports_every_window_fact() {
  # One interpreter start answers every window question a launch asks: the
  # launched model's own record (the --fields contract: window, source, unprobed
  # fallback budget, send id), the route ceiling's sibling maximum, and the saved
  # auto-compact budget the engine's schema keeps. These were three interpreter
  # runs, each failing on its own; the one run keeps that -- a broken resolver
  # must not blank the saved budget the clamp still needs. The expected values
  # are the ones the three helpers this replaced produced in this same fixture.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    'model=manual-model' > "$HOME/.config/ccmorph/mix.conf"
  write_profile_context mix manual-model=400000 big-sibling=1048576
  mkdir -p "$HOME/.config/ccmorph/profiles/mix" "$HOME/.claude"
  print -r -- '{"availableModels":["manual-model","big-sibling","unprobed-model"]}' \
    > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 v want
  local -a f
  # A probed model: its own window and source, no fallback budget, the sibling
  # maximum as the ceiling, nothing saved.
  f=("${(@ps:\t:)$(_ccm_launch_window mix manual-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '400000|conf-manual||manual-model|1048576|' ]] ||
    { print -u2 "probed model: ${(j:|:)f}"; rc=1 }
  # The caller's own ceiling export must not come back as the model's window:
  # resolve() pins to CLAUDE_CODE_MAX_CONTEXT_TOKENS, so the run strips it.
  f=("${(@ps:\t:)$(CLAUDE_CODE_MAX_CONTEXT_TOKENS=999999 _ccm_launch_window mix manual-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '400000|conf-manual||manual-model|1048576|' ]] ||
    { print -u2 "env-pinned: ${(j:|:)f}"; rc=1 }
  # An unprobed model: no window of its own, the resolver's conservative budget.
  f=("${(@ps:\t:)$(_ccm_launch_window mix unprobed-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '|default|128000|unprobed-model|1048576|' ]] ||
    { print -u2 "unprobed model: ${(j:|:)f}"; rc=1 }
  # No model at all -- a routed launch still needs its ceiling, so the run
  # happens anyway and only the record is empty.
  f=("${(@ps:\t:)$(_ccm_launch_window mix "")}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '||||1048576|' ]] ||
    { print -u2 "no model: ${(j:|:)f}"; rc=1 }
  # The profile layer narrows the route: its own availableModels is the last
  # writer (a single list element cannot be tombstoned), so the ceiling drops
  # from the removed sibling to the next model the route serves.
  print -r -- '{"availableModels":["manual-model","unprobed-model"]}' \
    > "$HOME/.config/ccmorph/profiles/mix/settings.json"
  f=("${(@ps:\t:)$(_ccm_launch_window mix manual-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '400000|conf-manual||manual-model|400000|' ]] ||
    { print -u2 "narrowed route: ${(j:|:)f}"; rc=1 }
  # The saved budget: only a value the engine's schema keeps counts -- an
  # integer-valued float is fine, out-of-range and wrong-typed are dropped.
  for v in 300000 300000.0 50000 true; do
    case $v in
      300000|300000.0) want=300000 ;;
      *) want='' ;;
    esac
    print -r -- "{\"autoCompactWindow\":$v}" \
      > "$HOME/.config/ccmorph/profiles/mix/settings.json"
    f=("${(@ps:\t:)$(_ccm_launch_window mix manual-model)}")
    [[ ${#f} == 6 && "${f[6]}" == "$want" ]] ||
      { print -u2 "saved $v: budget '${f[6]:-}', expected '${want:-}'"; rc=1 }
  done
  # Nothing saved: the sixth field is empty and every other fact survives.
  rm -f "$HOME/.config/ccmorph/profiles/mix/settings.json"
  f=("${(@ps:\t:)$(_ccm_launch_window mix manual-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '400000|conf-manual||manual-model|1048576|' ]] ||
    { print -u2 "no saved budget: ${(j:|:)f}"; rc=1 }

  rm -rf "$test_home"
  return $rc
}

test_a_broken_settings_compose_does_not_take_the_window_facts_with_it() {
  # The settings loader and the model-window resolver answer independent
  # questions, and on the separate interpreter runs this fold replaced they
  # failed independently: a settings-compose that could not be read cost only
  # the route ceiling and the saved budget, never the model's own record or
  # send id. The one run must keep that line -- the loader fails in its own
  # try, the diagnostics run's rule (#332).
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    'model=manual-model' > "$HOME/.config/ccmorph/mix.conf"
  write_profile_context mix manual-model=400000 big-sibling=1048576
  mkdir -p "$HOME/.config/ccmorph/profiles/mix" "$HOME/.claude"
  print -r -- '{"availableModels":["manual-model","big-sibling","unprobed-model"]}' \
    > "$HOME/.claude/settings.json"
  printf '%s\n' '#!/bin/sh' 'echo "settings-compose: gone" >&2' 'exit 1' \
    > "$HOME/.config/ccmorph/libexec/settings-compose"
  chmod +x "$HOME/.config/ccmorph/libexec/settings-compose"
  source "$CCMORPH_SOURCE"

  local rc=0
  local -a f
  # The resolver's record survives -- window, source, send id -- while the two
  # facts that read settings pay for the broken loader with empty strings.
  f=("${(@ps:\t:)$(_ccm_launch_window mix manual-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '400000|conf-manual||manual-model||' ]] ||
    { print -u2 "probed model: ${(j:|:)f}"; rc=1 }
  # The unprobed fallback budget rides the same resolver record and must
  # survive the same failure.
  f=("${(@ps:\t:)$(_ccm_launch_window mix unprobed-model)}")
  [[ ${#f} == 6 && "${(j:|:)f}" == '|default|128000|unprobed-model||' ]] ||
    { print -u2 "unprobed model: ${(j:|:)f}"; rc=1 }

  rm -rf "$test_home"
  return $rc
}

# The seed already carries the /model choice from the profile's settings.json
# into default_model, so the launch needs no second store read to size the
# budget from it (#333): a conf with no model= and a settings.json model must
# still derive the window from that model.
test_a_settings_model_still_sizes_the_budget() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    > "$HOME/.config/ccmorph/seeded.conf"
  write_profile_context seeded plain-model=131072
  mkdir -p "$HOME/.config/ccmorph/profiles/seeded"
  print -r -- '{"model":"plain-model"}' \
    > "$HOME/.config/ccmorph/profiles/seeded/settings.profile.json"
  source "$CCMORPH_SOURCE"

  local _CCMORPH_LOG="$HOME/ccmorph.log"
  local rc=0
  _ccm_launch -a seeded prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1 }
  assert_contains "$(<"$_CCMORPH_LOG")" 'window derived profile=seeded model=plain-model' \
    'the settings.json model must still size the budget' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_hints_when_a_routed_window_is_unprobed() {
  # A routed model nothing could size launches silently on the safe fallback;
  # the person is never asked to probe, before or after it is sized.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Endpoint lists the model but states no sizes (and none in any registry).
  printf '%s\n' \
    '#!/bin/sh' \
    'cat <<JSON' \
    '{"object":"list","data":[{"id":"opaque-model"},{"id":"other-model"}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  make_fake_curl_probe_aware
  printf '%s\n' \
    'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' \
    'model=opaque-model' > "$HOME/.config/ccmorph/opq.conf"
  source "$CCMORPH_SOURCE"

  # The hint rides the launch overlay's SessionStart systemMessage, not stderr:
  # Claude Code paints its alternate screen over anything written before exec,
  # so a hint printed there is visible for a fraction of a second.
  local rc=0
  _ccm_launch -a opq prompt >/dev/null 2>&1 || rc=1
  [[ "$(<"$CCMORPH_TEST_LOG")" == *"ccmorph probe"* ]] \
    && { print -u2 -- 'an unsized routed launch must not hand the person a command'; rc=1 }

  # Probed: the light goes off.
  write_profile_context opq opaque-model=500000
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a opq prompt >/dev/null 2>&1 || rc=1
  [[ "$(<"$CCMORPH_TEST_LOG")" == *"ccmorph probe"* ]] \
    && { print -u2 -- 'a probed launch must not nag'; rc=1 }
  rm -rf "$test_home"
  return $rc
}

test_an_unwritable_window_cache_is_silent_and_nonfatal() {
  # The per-model window cache is an optimisation. In a sandboxed shell its path
  # can be read-only while the registry and the launched Claude remain usable.
  # A trailing `|| :` kept the launch alive, but zsh reports a redirection failure
  # BEFORE the command's own `2>/dev/null` is installed, so every launch still
  # printed `_ccm_launch:… operation not permitted: …/cache/context-window`.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' 'type=apikey' 'base_url=http://gw.invalid' 'api_key=test-key' \
    'model=opaque-model' > "$HOME/.config/ccmorph/readonly.conf"
  write_profile_context readonly opaque-model=500000
  mkdir -p "$HOME/.config/ccmorph/profiles/readonly/cache"
  : > "$HOME/.config/ccmorph/profiles/readonly/cache/context-window"
  chmod 400 "$HOME/.config/ccmorph/profiles/readonly/cache/context-window"
  source "$CCMORPH_SOURCE"

  local err rc=0
  err=$(    _ccm_launch -a readonly prompt 2>&1 >/dev/null) || rc=1
  assert_not_contains "$err" 'operation not permitted' \
    'a refused best-effort cache write must not leak a shell error into launch UX' || rc=1
  assert_not_contains "$err" 'permission denied' \
    'a refused best-effort cache write must stay silent' || rc=1
  chmod 600 "$HOME/.config/ccmorph/profiles/readonly/cache/context-window"
  rm -rf "$test_home"
  return $rc
}

test_flag_errors_name_the_spelling_the_user_typed() {
  # -a, --profile and --profile=<name> are one flag; the diagnostics used to
  # say "-a" whatever the user typed, sending them to look for an argument
  # they never wrote.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 err

  err=$(_ccm_launch --profile 2>&1); assert_contains "$err" '--profile requires' \
    'a bare --profile must name --profile' || rc=1
  err=$(_ccm_launch -a 2>&1); assert_contains "$err" '-a requires' \
    'a bare -a must name -a' || rc=1
  assert_not_contains "$err" '--profile requires' \
    '-a must not be reported as --profile' || rc=1
  err=$(_ccm_launch --profile= x 2>&1); assert_contains "$err" '--profile= requires' \
    'an empty --profile= must name that form' || rc=1
  err=$(_ccm_launch --profile a --profile b 2>&1)
  assert_contains "$err" '--profile may be specified only once' \
    'a repeated --profile must name --profile' || rc=1
  err=$(_ccm_launch -a a -a b 2>&1)
  assert_contains "$err" '-a may be specified only once' \
    'a repeated -a must name -a' || rc=1
  err=$(_ccm_launch -a a --profile=b 2>&1)
  assert_contains "$err" '--profile= may be specified only once' \
    'the duplicate report must name the second, offending form' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_source_names_no_machine_local_files() {
  # The author's machine keeps private files (RTK.md) beside the canonical
  # store; shipped code must never reach for them by name.
  local source_text="$(cat -- "${CCMORPH_LIBRARY[@]}")"
  assert_not_contains "$source_text" 'RTK' \
    'a machine-local file name must not appear in shipped source'
}

test_subscription_profile_settings_override_global_launch_settings() {
  local test_home
  test_home=$(mktemp -d)
  local TMPDIR="$test_home/tmp"
  mkdir -p "$TMPDIR"
  setup_home "$test_home"
  printf '%s\n' 'type=sub' 'email=test@example.invalid' \
    > "$HOME/.config/ccmorph/sub.conf"
  printf '%s\n' \
    '{"env":{"GLOBAL_SETTING":"kept"},"effortLevel":"high"}' \
    > "$HOME/.claude/settings.json"
  mkdir -p "$HOME/.config/ccmorph/profiles/sub"
  printf '%s\n' \
    '{"env":{"PROFILE_SETTING":"kept"},"effortLevel":"medium"}' \
    > "$HOME/.config/ccmorph/profiles/sub/settings.json"
  printf '%s\n' '{}' > "$HOME/.config/ccmorph/profiles/sub/config.json"
  printf '%s\n' '{}' > "$HOME/.config/ccmorph/profiles/sub/.claude.json"
  ln -s "$HOME/.claude/projects" "$HOME/.config/ccmorph/profiles/sub/projects"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a sub prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" '"effortLevel":"medium"' \
    'subscription profile effort must override global effort' || rc=1
  assert_not_contains "$actual" '"effortLevel":"high"' \
    'subscription launch must not pass the unmerged global settings' || rc=1
  assert_contains "$actual" '"GLOBAL_SETTING":"kept"' \
    'subscription profile must inherit absent global env values' || rc=1
  assert_contains "$actual" '"PROFILE_SETTING":"kept"' \
    'subscription profile must retain profile env values' || rc=1
  assert_not_contains "$actual" 'CLAUDE_CODE_OAUTH_TOKEN' \
    'subscription credentials must remain process-only' || rc=1

  rm -rf "$test_home"
  return $rc
}


test_profile_settings_override_global_launch_settings() {
  local test_home
  test_home=$(mktemp -d)
  local TMPDIR="$test_home/tmp"
  mkdir -p "$TMPDIR"
  setup_home "$test_home"
  printf '%s\n' \
    '{"env":{"GLOBAL_SETTING":"kept","SHARED_SETTING":"global"},"effortLevel":"high","permissions":{"defaultMode":"ask"}}' \
    > "$HOME/.claude/settings.json"
  mkdir -p "$HOME/.config/ccmorph/profiles/proxy"
  printf '%s\n' \
    '{"env":{"PROFILE_SETTING":"kept","SHARED_SETTING":"profile"},"effortLevel":"medium"}' \
    > "$HOME/.config/ccmorph/profiles/proxy/settings.json"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" '"effortLevel":"medium"' \
    'profile effort must override the global effort in launch settings' || rc=1
  assert_not_contains "$actual" '"effortLevel":"high"' \
    'global effort must not override profile effort' || rc=1
  assert_contains "$actual" '"GLOBAL_SETTING":"kept"' \
    'profile launch settings must inherit absent global env values' || rc=1
  assert_contains "$actual" '"PROFILE_SETTING":"kept"' \
    'profile launch settings must retain profile env values' || rc=1
  assert_contains "$actual" '"SHARED_SETTING":"profile"' \
    'profile env values must override matching global env values' || rc=1
  assert_contains "$actual" '"permissions":{"defaultMode":"ask"}' \
    'profile launch settings must inherit absent global objects' || rc=1

  rm -rf "$test_home"
  return $rc
}


test_proxy_gateway_credentials_reach_claude_only_through_the_environment() {
  local test_home
  test_home=$(mktemp -d)
  local TMPDIR="$test_home/tmp"
  mkdir -p "$TMPDIR"
  setup_home "$test_home"
  printf '%s\n' '{"env":{"CANONICAL_SETTING":"kept"},"permissions":{"allow":["Read"]}}' \
    > "$HOME/.claude/settings.json"
  source "$CCMORPH_SOURCE"

  _ccm_launch -a proxy prompt || {
    rm -rf "$test_home"
    return 1
  }

  local actual="$(<"$CCMORPH_TEST_LOG")"
  local rc=0
  assert_contains "$actual" '"CANONICAL_SETTING":"kept"' \
    'the generated settings must preserve canonical settings env' || rc=1
  assert_contains "$actual" 'BASE_URL=http://proxy.invalid' \
    'gateway base URL must reach Claude through the environment' || rc=1
  assert_contains "$actual" 'API_KEY=test-key' \
    'gateway key must reach Claude through the environment' || rc=1
  assert_not_contains "$(<"$HOME/.config/ccmorph/profiles/proxy/settings.json")" 'test-key' \
    'a credential must never be written into the generated settings' || rc=1
  assert_contains "$actual" '"permissions":{"allow":["Read"]}' \
    'the generated settings must preserve non-env canonical settings' || rc=1
  if [[ -n "$(print -r -- $HOME/.config/ccmorph/profiles/proxy/cache/launch-settings.*(N))" ]]; then
    print -u2 -- 'a launch must leave no temporary settings file'
    rc=1
  fi

  rm -rf "$test_home"
  return $rc
}




test_the_installer_wires_no_shell_and_installs_managed_commands() {
  # ccmorph installs only its own command. Anthropic's native `claude` command
  # remains entirely user-owned and is never copied, renamed, or shadowed.
  local installer="$(<"$REPO_ROOT/install.sh")"
  local rc=0

  assert_contains "$installer" 'cp "$SRC/bin/ccmorph" "$BIN_DIR/ccmorph"' \
    'installer must place ccmorph on PATH' || rc=1
  assert_not_contains "$installer" 'cp "$SRC/claude" "$BIN_DIR/claude"' \
    'installer must not install a claude wrapper' || rc=1
  assert_not_contains "$installer" 'source \"$CONFIG_DIR/lib/ccmorph.sh\"' \
    'installer must not wire a shell startup file' || rc=1
  assert_not_contains "$installer" 'claude() { _ccm_launch' \
    'installer must not define a shell function for claude' || rc=1
  assert_not_contains "$installer" 'claudex' \
    'installer must not resurrect the retired per-operator launcher' || rc=1
  assert_not_contains "$installer" 'source "$RC"' \
    'installer must not source or edit startup files' || rc=1
  return $rc
}


test_the_command_resolves_its_library_and_forwards_every_argument() {
  # The executable's whole job: find ccmorph.sh, source it, hand over argv. It is
  # what makes the package installable at all -- the tool used to ship only as zsh
  # functions, which an installer cannot put on PATH, so a fresh install answered its own
  # first documented step with "command not found".
  local test_home
  test_home=$(mktemp -d)
  mkdir -p "$test_home/lib"
  printf '%s\n' \
    'ccmorph() {' \
    '  printf "%s\\n" "$*" > "$HOME/forwarded.log"' \
    '}' \
    > "$test_home/lib/ccmorph.sh"

  HOME="$test_home" CCMORPH_LIB_DIR="$test_home/lib" \
    zsh "$REPO_ROOT/bin/ccmorph" hop latest || {
      print -u2 -- 'the command must load the library and run it'
      rm -rf "$test_home"
      return 1
    }

  local rc=0
  local actual="$(<"$test_home/forwarded.log")"
  [[ "$actual" == 'hop latest' ]] || {
    print -u2 -- \
      "argv must reach ccmorph untouched\n  expected: hop latest\n  actual: ${(qqq)actual}"
    rc=1
  }

  # The library must receive its own directory, not derive it, so a child
  # session's `hop` reaches the same library.
  printf '%s\n' \
    'ccmorph() { printf "%s\\n" "$CCMORPH_LIB_DIR" > "$HOME/libdir.log" }' \
    > "$test_home/lib/ccmorph.sh"
  HOME="$test_home" CCMORPH_LIB_DIR="$test_home/lib" \
    zsh "$REPO_ROOT/bin/ccmorph" list >/dev/null
  [[ "$(<"$test_home/libdir.log")" == "$test_home/lib" ]] || {
    print -u2 -- 'the launcher must export the library directory it resolved'
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}



test_the_command_reports_a_library_it_cannot_load() {
  local test_home
  test_home=$(mktemp -d)
  mkdir -p "$test_home/lib"
  local rc=0 output exit_status

  # A library that exists but fails to load: one actionable line, not zsh's noise.
  printf '%s\n' 'return 1' > "$test_home/lib/ccmorph.sh"
  output=$(HOME="$test_home" CCMORPH_LIB_DIR="$test_home/lib" \
    zsh "$REPO_ROOT/bin/ccmorph" list 2>&1)
  exit_status=$?
  [[ $exit_status -ne 0 ]] || {
    print -u2 -- 'the command must fail when its library cannot be loaded'
    rc=1
  }
  assert_contains "$output" "failed to load $test_home/lib/ccmorph.sh" \
    'a load failure must name the file it could not load' || rc=1

  # An explicit CCMORPH_LIB_DIR is AUTHORITATIVE: pointing it at a directory with
  # no library must fail loudly rather than quietly running a different library than
  # the caller named, which is how a debugging session ends up reasoning about the
  # wrong file.
  output=$(HOME="$test_home" CCMORPH_LIB_DIR="$test_home/nowhere" \
    zsh "$REPO_ROOT/bin/ccmorph" list 2>&1)
  exit_status=$?
  [[ $exit_status -ne 0 ]] || {
    print -u2 -- 'an explicit but wrong CCMORPH_LIB_DIR must not fall back silently'
    rc=1
  }
  assert_contains "$output" 'CCMORPH_LIB_DIR is set but has no readable ccmorph.sh' \
    'a wrong CCMORPH_LIB_DIR must say so by name' || rc=1

  rm -rf "$test_home"
  return $rc
}


test_a_bare_invocation_prints_usage_and_launches_nothing() {
  # A tool with a verb vocabulary prints usage when invoked bare -- git, docker, npm,
  # gh, kubectl and brew all do, and only single-purpose tools (fzf, vim, top) act.
  # ccmorph has twelve verbs, so it belongs to the first group.
  #
  # v2.0.0 briefly made a bare invocation LAUNCH the default profile, which was wrong
  # in the worst available direction: bare `git` cannot cost anything, while a bare
  # launch starts a billable session against whichever profile happens to be default.
  # It also displaced the harmless thing (the listing) with the consequential one.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"
  ccmorph use gw >/dev/null || {
    print -u2 -- 'could not set a default profile'
    rm -rf "$test_home"; return 1
  }

  local calls="" rc=0
  _ccm_launch_driver() { calls+="[${_CCMORPH_LAUNCH_PROFILE:-}:${(j: :)${(q)@}}]" }

  # Bare, WITH a default profile configured: the dangerous case, because this is
  # exactly when a bare launch would have had something to launch.
  local bare_out
  bare_out=$(ccmorph 2>&1)
  assert_contains "$bare_out" 'Launch:' \
    'a bare ccmorph must print usage' || rc=1
  [[ -z "$calls" ]] || {
    print -u2 -- "FAIL: a bare ccmorph launched something: $calls"
    rc=1
  }

  # `run` is the explicit launch spelling; without a profile, _ccm_launch resolves
  # the saved default itself.
  calls=""; ccmorph run
  assert_contains "$calls" '[:]' \
    'ccmorph run must launch, letting the launcher resolve the default' || rc=1

  # Flags after `run` are Claude Code's.
  calls=""; ccmorph run -p hello
  assert_contains "$calls" '[:-p hello]' \
    'arguments after run must pass through untouched' || rc=1

  # `--` is run's separator, consumed rather than forwarded: passed on, it made
  # Claude Code read `-p` and the prompt as prompt text.
  calls=""; ccmorph run gw -- -p hello
  assert_contains "$calls" '[gw:-p hello]' \
    'run must consume its own -- separator after a profile' || rc=1
  calls=""; ccmorph run -- -p hello
  assert_contains "$calls" '[:-p hello]' \
    'run must consume its own -- separator without a profile' || rc=1
  calls=""; ccmorph run gw -- -- -p
  assert_contains "$calls" '[gw:-- -p]' \
    'only the first -- is run'"'"'s; a second one is Claude Code'"'"'s' || rc=1

  # A named profile belongs after `run`.
  calls=""; ccmorph run gw --resume plain-prompt
  assert_contains "$calls" '[gw:--resume plain-prompt]' \
    'ccmorph run <profile> must select that profile and pass the rest through' || rc=1

  # Old top-level launch shortcuts are rejected with replacements.
  calls=""
  local old_account old_flag
  old_account=$(ccmorph gw --resume plain-prompt 2>&1)
  [[ -z "$calls" ]] || { print -u2 -- "FAIL: positional profile launched: $calls"; rc=1; }
  assert_contains "$old_account" 'ccmorph run gw' \
    'a positional profile must name the explicit replacement' || rc=1
  old_flag=$(ccmorph -p hello 2>&1)
  [[ -z "$calls" ]] || { print -u2 -- "FAIL: leading flag launched: $calls"; rc=1; }
  assert_contains "$old_flag" 'ccmorph run -p' \
    'a leading flag must name the explicit replacement' || rc=1

  local old_start
  old_start=$(ccmorph start gw 2>&1)
  assert_contains "$old_start" 'ccmorph run gw' \
    'the old start verb must name the run replacement' || rc=1

  # `run` with no default configured must say so rather than launching blind.
  rm -f "$HOME/.config/ccmorph/active"
  calls=""
  local no_default
  no_default=$(ccmorph run 2>&1)
  [[ -z "$calls" ]] || {
    print -u2 -- 'FAIL: run with no default must not launch'
    rc=1
  }
  assert_contains "$no_default" 'no default profile set' \
    'run with no default must name the fix' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_the_launcher_adds_nothing_unrequested() {
  # A launch must pass argv through untouched. There is no silent permission
  # bypass: `--dangerously-skip-permissions` reaches the launcher only when the
  # caller types it themselves, never from an ambient env-var opt-in.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=k' 'base_url=https://gateway.invalid'
  source "$CCMORPH_SOURCE"

  local calls=""
  _ccm_launch() { calls+="${_CCMORPH_LAUNCH_PROFILE:-}:${(j: :)${(q)@}}\n" }

  local rc=0
  calls=""; ccmorph run gw plain-prompt
  assert_not_contains "$calls" 'skip-permissions' \
    'a vanilla install must not bypass permissions on the caller behalf' || rc=1

  calls=""; ccmorph run gw --dangerously-skip-permissions explicit
  assert_contains "$calls" 'gw:--dangerously-skip-permissions explicit' \
    'an explicit flag must always reach the launcher' || rc=1

  # Argument fidelity: an arg containing spaces, an empty arg, and a glob-looking
  # arg must each arrive as ONE argument, unsplit and unexpanded. Counted rather
  # than string-matched -- the count is what proves nothing was split or dropped.
  local seen=0
  _ccm_launch() { seen=$# }
  ccmorph run gw -p 'two words' '' '*.md'
  (( seen == 4 )) || {
    print -u2 -- "FAIL: caller args must arrive as 4, got $seen"
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}

test_run_keeps_native_command_delimiters() {
  # `--` is run's separator in the OUTER position only (usage: run [profile]
  # [-i] [-- <claude arguments>]). Removing the first exact `--`
  # anywhere in the array removed ones Claude Code owns as well: the MCP command
  # delimiter and a literal separator prompt, so `mcp add demo -- npx -y server`
  # reached Claude Code one argument short (#229). Claude Code's command grammar
  # is its own (docs/ARCHITECTURE.md, Scope and ownership), so the outer separator is
  # recognisable only as the first argument that follows nothing but ccmorph's
  # own leading options; the first native argument ends that prefix.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_claude_help
  source "$CCMORPH_SOURCE"

  local rc=0 actual tab=$'\t'

  # A native command's argument array must arrive whole and in order, its own
  # delimiter included.
  : > "$CCMORPH_TEST_LOG"
  ccmorph run proxy mcp add demo -- npx -y server || rc=1
  actual=$(<"$CCMORPH_TEST_LOG")
  assert_contains "$actual" "ARG${tab}mcp${tab}add${tab}demo${tab}--${tab}npx${tab}-y${tab}server" \
    'a native MCP command must keep its own -- delimiter' || rc=1

  # A `--` behind a native flag is that flag's VALUE, never run's separator.
  : > "$CCMORPH_TEST_LOG"
  ccmorph run proxy -p -- || rc=1
  actual=$(<"$CCMORPH_TEST_LOG")
  assert_contains "$actual" "ARG${tab}-p${tab}--" \
    'a literal separator prompt must survive as the flag value' || rc=1

  # And run's own separator is still consumed -- including after the leading
  # model/effort options the dispatcher recognises as its own prefix.
  : > "$CCMORPH_TEST_LOG"
  ccmorph run proxy --model mine -- -p 'reply ok' || rc=1
  actual=$(<"$CCMORPH_TEST_LOG")
  assert_contains "$actual" "ARG${tab}--model${tab}mine" \
    'an explicit model still reaches Claude Code untouched' || rc=1
  assert_contains "$actual" "${tab}-p${tab}reply ok" \
    'the prompt after the outer separator still arrives' || rc=1
  assert_not_contains "$actual" "${tab}--${tab}" \
    'the outer separator must not be forwarded' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_source_names_no_operator_profile() {
  # Shipped source must not name any particular profile: `claudex` hardcoded
  # --profile codex, so a fresh install carried a launcher that referenced a
  # profile the installing machine did not have. Guard the class — a profile
  # name in shipped source is host contamination, the same as a host path.
  local source_text="$(cat -- "${CCMORPH_LIBRARY[@]}")"
  local rc=0
  assert_not_contains "$source_text" '--profile codex' \
    'a hardcoded profile name in a launcher is host contamination' || rc=1
  assert_not_contains "$source_text" 'claudex' \
    'a per-operator launcher must not ship' || rc=1
  return $rc
}

# The REAL shipped resolver, placed where the product looks for it
# ($_CCMORPH_DIR/libexec/model-window under the scratch $HOME) — these tests pin the
# true integration, not a stand-in's reading of the contract.
install_model_window() {
  cp "$REPO_ROOT/libexec/model-window" "$HOME/.config/ccmorph/libexec/model-window"
  cp "$REPO_ROOT/libexec/settings-compose" "$HOME/.config/ccmorph/libexec/settings-compose"
  cp "$REPO_ROOT/lib/ccmorph.sh" "$HOME/.config/ccmorph/lib/ccmorph.sh"
  cp "$REPO_ROOT"/lib/ccmorph-*.zsh "$HOME/.config/ccmorph/lib/"
  chmod +x "$HOME/.config/ccmorph/libexec/model-window" "$HOME/.config/ccmorph/libexec/settings-compose"
}

test_picker_without_a_registry_offers_rows_and_spawns_nothing() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # A machine that has never run `ccmorph probe` has no registry at all. Two things
  # must hold together, and they used to be in tension:
  #   * every non-claude model is still OFFERED (the launcher's `[[ -f $registry ]] ||
  #     return 0` guard used to return before writing a single row, so such a machine
  #     got an empty picker on a working gateway);
  #   * and no `model-window` process is spawned for the claude- ids, because with no
  #     registry it can only answer `unprobed`, which derived_send_id discards --
  #     ~50ms of interpreter startup each, for nothing.
  # The resolver is replaced by a counting stub, so the spawn count is observable.
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[{"id":"glm-5.3"},{"id":"claude-opus-5"},{"id":"claude-sonnet-5"}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  install_model_window
  # Count every invocation, then behave as the real resolver would with no registry.
  printf '%s\n' '#!/bin/sh' 'echo x >> "$HOME/mw-calls"' \
    'echo "{\"window\":200000,\"send_id\":\"$2\",\"source\":\"default\",\"unprobed\":true}"' \
    > "$HOME/.config/ccmorph/libexec/model-window"
  chmod +x "$HOME/.config/ccmorph/libexec/model-window"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":[]}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=apikey' 'base_url=http://gw.invalid' 'api_key=sk_t' \
    > "$HOME/.config/ccmorph/gw.conf"
  source "$CCMORPH_SOURCE"

  local rc=0
  # No registry file exists under this isolated state root.
  _ccm_project_picker gw http://gw.invalid sk_t
  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"

  assert_contains "$state" '"value": "glm-5.3"' \
    'with no registry a served model must still be offered -- an absent registry costs
     the [1m] suffix on claude rows, never the rows themselves' || rc=1
  local calls=0
  [[ -f "$HOME/mw-calls" ]] && calls=$(command wc -l < "$HOME/mw-calls" | tr -d ' ')
  [[ "$calls" == 0 ]] || {
    print -u2 -- "no registry must mean no model-window spawns, got $calls"
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}

test_picker_projects_a_derived_1m_row_for_a_probed_claude() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # Genuine claude ids served by the gateway. Claude Code's own row for a bare
  # claude- id budgets 200,000 behind a foreign base URL, so a probed 1M model
  # must be offered as its [1m] id -- the suffix is what lifts the window, and
  # only a projected row can carry it (discovery never produces one).
  printf '%s\n' \
    '#!/bin/sh' 'cat <<JSON' \
    '{"data":[{"id":"claude-opus-5"},{"id":"claude-haiku-4-5"},{"id":"claude-new-9"}]}' \
    'JSON' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  install_model_window
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  print -r -- '{"availableModels":[]}' > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  printf '%s\n' 'type=apikey' 'base_url=http://gw.invalid' 'api_key=sk_t' \
    > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw claude-opus-5=1000000 claude-haiku-4-5=200000
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_project_picker gw http://gw.invalid sk_t
  local state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"

  assert_contains "$state" '"value": "claude-opus-5[1m]"' \
    'a claude model probed at 1M must be offered with its [1m] id' || rc=1
  assert_not_contains "$state" '"value": "claude-opus-5"' \
    'the bare id would budget 200k against a 1M route' || rc=1
  assert_not_contains "$state" 'claude-haiku-4-5[1m]' \
    'haiku-4-5 is probed at 200k: [1m] would report 1M against a 200k wall' || rc=1
  assert_contains "$state" 'claude-new-9' \
    'an unprobed Claude model stays visible under its exact ID' || rc=1

  # Without the resolver, catalogue rows retain their exact IDs; only the
  # derived context suffix is unavailable.
  print -r -- '{}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  rm -f "$HOME/.config/ccmorph/libexec/model-window"
    _ccm_project_picker gw http://gw.invalid sk_t
  state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")"
  assert_contains "$state" 'claude-opus-5' \
    'no resolver must preserve catalogue visibility without adding a derived suffix' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_substitutes_the_derived_send_id_for_a_claude_model() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_gateway_with_windows
  printf '%s\n' \
    'type=apikey' \
    'base_url=http://gw.invalid' \
    'api_key=test-key' \
    'model=claude-opus-5' \
    > "$HOME/.config/ccmorph/sendid.conf"
  write_profile_context sendid claude-opus-5=1000000
  install_model_window
  source "$CCMORPH_SOURCE"

  local rc=0
    _ccm_launch -a sendid prompt || { rm -rf "$test_home"; return 1; }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'MODEL_ENV=claude-opus-5[1m]' \
    'a conf default probed at 1M must launch with the [1m] id' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_inspect_uses_the_fleet_or_default_compaction_percent() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  print -r -- '{"schemaVersion":1,"autoCompactAtPercent":73}' \
    > "$HOME/.agents/runtime/policies/context.json"
  write_profile_context proxy bootstrap-model=200000
  install_model_window
  source "$CCMORPH_SOURCE"

  local actual rc=0
  actual=$(ccmorph inspect proxy) || { rm -rf "$test_home"; return 1; }
  assert_contains "$actual" 'compaction target 73%' \
    'show must describe the fleet percentage instead of a hardcoded 80 percent' || rc=1

  mkdir -p "$HOME/.config/ccmorph/profiles/proxy"
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"90"}}' \
    > "$HOME/.config/ccmorph/profiles/proxy/settings.json"
  actual=$(ccmorph inspect proxy) || { rm -rf "$test_home"; return 1; }
  assert_contains "$actual" 'compaction target 90%' \
    'show must use the profile settings override instead of the fleet value' || rc=1

  print -r -- '{"schemaVersion":1,"autoCompactAtPercent":"broken"}' \
    > "$HOME/.agents/runtime/policies/context.json"
  actual=$(ccmorph inspect proxy) || { rm -rf "$test_home"; return 1; }
  assert_contains "$actual" 'compaction target 90%' \
    'a valid settings override must make a malformed unused fleet policy irrelevant' || rc=1

  print -r -- 'env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=75' \
    >> "$HOME/.config/ccmorph/proxy.conf"
  actual=$(ccmorph inspect proxy) || { rm -rf "$test_home"; return 1; }
  assert_contains "$actual" 'compaction target 75%' \
    'show must use the conf override before settings and fleet' || rc=1

  command sed -i.bak '/^env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=/d' \
    "$HOME/.config/ccmorph/proxy.conf"
  rm -f "$HOME/.config/ccmorph/proxy.conf.bak" \
    "$HOME/.config/ccmorph/profiles/proxy/settings.json" \
    "$HOME/.agents/runtime/policies/context.json"
  actual=$(ccmorph inspect proxy) || { rm -rf "$test_home"; return 1; }
  assert_contains "$actual" 'compaction target 75%' \
    'show must use the built-in 75 percent fallback when the policy is absent' || rc=1
  rm -rf "$test_home"
  return $rc
}

test_launch_compacts_at_fleet_percent_unless_the_conf_overrules() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  print -r -- '{"schemaVersion":1,"autoCompactAtPercent":73}' \
    > "$HOME/.agents/runtime/policies/context.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'AUTOCOMPACT_PCT=73' \
    'every profile reads the fleet compaction policy instead of a local constant' || rc=1

  printf '%s\n' \
    'type=proxy' \
    'base_url=http://proxy.invalid' \
    'api_key=test-key' \
    'model=bootstrap-model' \
    'env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=90' \
    > "$HOME/.config/ccmorph/pct90.conf"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a pct90 prompt || { rm -rf "$test_home"; return 1; }
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'AUTOCOMPACT_PCT=90' \
    'a conf declaring its own trigger must overrule the default' || rc=1

  rm -f "$HOME/.config/ccmorph/pct90.conf" "$HOME/.agents/runtime/policies/context.json"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'AUTOCOMPACT_PCT=75' \
    'an absent fleet policy must fall back to the built-in 75 percent default' || rc=1

  print -r -- '{"schemaVersion":1,"autoCompactAtPercent":"broken"}' \
    > "$HOME/.agents/runtime/policies/context.json"
  local err
  err=$(_ccm_launch -a proxy prompt 2>&1) && {
    print -u2 -- 'a present malformed fleet policy must stop the launch'
    rc=1
  }
  assert_contains "$err" 'invalid fleet context policy' \
    'a malformed present policy must fail instead of falling back' || rc=1
  rm -rf "$test_home"
  return $rc
}

# The 80% default was injected after checking only the CONF (`penv`), so a value the
# user had set in their own settings.json was silently replaced: the overlay merges
# canon+profile first, then writes every penv entry over the result unconditionally.
# settings.json is Claude Code's file and a value in it is the user's -- a ccmorph
# default may fill a gap, never overrule a choice.
#
# Both layers are checked, because both are the user's: canon
# (~/.claude/settings.json) and the profile's own.
test_a_settings_file_compaction_trigger_is_not_overruled() {
  local test_home
  test_home=$(mktemp -d)
  local TMPDIR="$test_home/tmp"
  mkdir -p "$TMPDIR"
  setup_home "$test_home"
  # The user's own trigger, set where /autocompact and a hand edit both land.
  mkdir -p "$HOME/.config/ccmorph/profiles/proxy"
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"70"}}' \
    > "$HOME/.config/ccmorph/profiles/proxy/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  # The launch overlay is what Claude Code actually reads for a settings-file value
  # (it reaches the process through --settings, not through the environment), so the
  # overlay's env block is the signal. The stub records it as SETTINGS=.
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"70"' \
    "a trigger set in the profile's settings.json must survive the launch" || rc=1
  assert_not_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"80"' \
    'the ccmorph default must not overrule a value the user already set' || rc=1

  # Same rule for the canonical settings file.
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"65"}}' \
    > "$HOME/.claude/settings.json"
  rm -f "$HOME/.config/ccmorph/profiles/proxy/settings.profile.json"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"65"' \
    'a trigger set in canonical settings.json must survive the launch' || rc=1
  assert_not_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"80"' \
    'the ccmorph default must not overrule a canonical value either' || rc=1

  # Presence is not validity: Claude Code drops an engine-invalid value, so it must
  # not suppress the fleet/default injection. A parseFloat prefix remains valid.
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"broken"}}' \
    > "$HOME/.claude/settings.json"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'AUTOCOMPACT_PCT=80' \
    'an engine-invalid saved trigger must leave a gap for fleet/default injection' || rc=1

  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"90junk"}}' \
    > "$HOME/.claude/settings.json"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"90junk"' \
    'settings validation must use the engine parseFloat prefix semantics' || rc=1
  assert_not_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"80"' \
    'an engine-valid prefixed trigger must continue to suppress the default' || rc=1

  # Last writer wins even when the last value is invalid: the profile key replaces
  # canonical settings before schema validation, so the canonical 65 is no longer
  # effective and the fleet/default must fill the resulting gap.
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"65"}}' \
    > "$HOME/.claude/settings.json"
  mkdir -p "$HOME/.config/ccmorph/profiles/proxy"
  print -r -- '{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"broken"}}' \
    > "$HOME/.config/ccmorph/profiles/proxy/settings.json"
  : > "$CCMORPH_TEST_LOG"
  _ccm_launch -a proxy prompt || { rm -rf "$test_home"; return 1; }
  actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'AUTOCOMPACT_PCT=80' \
    'an invalid profile override must leave a gap for fleet/default injection' || rc=1
  assert_not_contains "$actual" '"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"65"' \
    'an overridden canonical trigger must not reappear through validation order' || rc=1

  rm -rf "$test_home"
  return $rc
}

test_launch_skips_the_sizer_when_nothing_is_new() {
  # The sizer is an interpreter start (~2s under endpoint scanning); a launch
  # where every listed model is sized, excluded, disguised-but-known or non-chat
  # must not pay it. A genuinely new id must still reach it.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  write_apikey_conf gw 'api_key=test-key' 'base_url=https://gateway.invalid' \
    'excluded_models=hidden-model'
  write_profile_context gw known-model=300000 gpt-5.6-sol=372000
  source "$CCMORPH_SOURCE"

  local rc=0 listing
  listing='{"data":[{"id": "known-model"},{"id": "claude-fable-5-dd-los-6.5-tpg"},{"id": "hidden-model"},{"id": "bge-m3", "capabilities": {"chat_completion": false}}]}'
  _ccm_listing_has_unsized gw "$listing" &&
    { print -u2 -- 'an all-known listing must skip the sizer'; rc=1; }
  _ccm_listing_has_unsized gw '{"data":[{"id": "known-model"},{"id": "brand-new"}]}' ||
    { print -u2 -- 'a new model must reach the sizer'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_add_sub_completes_the_browser_login() {
  # `add` wrote a placeholder `type=sub` conf and then re-read it through the
  # validator, which rejects a sub profile without an email -- so every sub add
  # died before the browser opened, telling the user to re-create the profile.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'case "$1 $2" in' \
    '  "auth login") : > "$HOME/login.ran"; exit 0 ;;' \
    '  "auth logout") exit 0 ;;' \
    '  "auth status") printf '"'"'{"loggedIn": true, "authMethod": "claude.ai", "email": "a@example.com"}\n'"'"'; exit 0 ;;' \
    'esac' \
    'exit 0' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"

  local rc=0 out
  out=$(print 1 | ccmorph add demo 2>&1) || { print -u2 -- "$out"; rc=1; }
  assert_not_contains "$out" 'has no email' \
    'the placeholder must not be validated as a finished profile' || rc=1
  [[ -f "$HOME/login.ran" ]] || { print -u2 -- 'the browser login never ran'; rc=1; }
  command grep -qx 'email=a@example.com' "$HOME/.config/ccmorph/demo.conf" ||
    { print -u2 -- 'the profile must be saved with the signed-in email'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_logged_email_reads_the_given_config_dir() {
  # _ccm_logged_email <dir> must run `claude auth status` with CLAUDE_CONFIG_DIR
  # pointed at <dir>, so `add sub` reads the email from the isolated profile it
  # just logged into — not the global file. Stub claude to echo the dir it saw.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  printf '%s\n' \
    '#!/bin/sh' \
    'if [ "$1" = "auth" ] && [ "$2" = "status" ]; then' \
    '  printf '"'"'{"loggedIn": true, "email": "%s"}\n'"'"' "${CLAUDE_CONFIG_DIR:-GLOBAL}"' \
    '  exit 0' \
    'fi' \
    'exit 1' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"

  local rc=0
  _ccm_logged_email "$HOME/.config/ccmorph/profiles/second" || rc=1
  assert_contains "$_ccm_email" "ccmorph/profiles/second" \
    'a config-dir argument must reach auth status as CLAUDE_CONFIG_DIR' || rc=1

  # The dir is required: an unscoped read would report the base install's
  # profile, never the profile's. Run in a subshell so the :? abort cannot
  # take the test runner down with it.
  ( _ccm_logged_email ) 2>/dev/null &&
    { print -u2 -- 'a missing config dir must abort, not read the global config'; rc=1 }

  rm -rf "$test_home"
  return $rc
}

test_add_sub_auth_calls_scrub_securestorage_dir() {
  # A private session exports CLAUDE_SECURESTORAGE_CONFIG_DIR; status, logout and
  # login must not inherit it (they would read/write the wrong credential store).
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' \
    '#!/bin/sh' \
    'echo "$1 $2 [${CLAUDE_SECURESTORAGE_CONFIG_DIR-unset}]" >> "$HOME/auth_calls"' \
    'case "$1 $2" in' \
    '  "auth status") printf '"'"'{"loggedIn": true, "authMethod": "claude.ai", "email": "a@example.com"}\n'"'"' ;;' \
    'esac' \
    'exit 0' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"

  local rc=0 out
  # The runner calls tests in-process: the export must not outlive the subshell.
  out=$( export CLAUDE_SECURESTORAGE_CONFIG_DIR=/leak
    print 1 | ccmorph add demo 2>&1 ) || { print -u2 -- "$out"; rc=1; }
  local n
  for n in status logout login; do
    command grep -qx "auth $n \[unset\]" "$HOME/auth_calls" ||
      { print -u2 -- "auth $n did not run scrubbed"; rc=1; }
  done
  # The runner calls tests in-process: the export must not outlive the test.
  [[ ${CLAUDE_SECURESTORAGE_CONFIG_DIR-unset} == unset ]] ||
    { print -u2 -- 'FAIL: CLAUDE_SECURESTORAGE_CONFIG_DIR leaked into the runner shell'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_logged_email_scrubs_every_backend_env_key() {
  # auth status must not see ANY key in _CCMORPH_BACKEND_ENV (a Bedrock/Vertex
  # flag or an unix socket would redirect the check just like an API key).
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  printf '%s\n' \
    '#!/bin/sh' \
    'env > "$HOME/seen_env"' \
    'printf '"'"'{"loggedIn": true, "email": "a@b.c"}\n'"'"'' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"

  local k rc=0
  ( for k in "${_CCMORPH_BACKEND_ENV[@]}"; do export "$k=leak"; done
    _ccm_logged_email "$HOME/.config/ccmorph/profiles/second" ) || rc=1
  for k in "${_CCMORPH_BACKEND_ENV[@]}"; do
    if grep -q "^$k=" "$HOME/seen_env"; then
      print -u2 -- "auth status inherited $k"; rc=1
    fi
  done

  # The runner calls tests in-process: the exports must not outlive the subshell.
  for k in "${_CCMORPH_BACKEND_ENV[@]}"; do
    [[ ${(P)k-} == leak ]] && { print -u2 -- "FAIL: $k leaked into the runner shell"; rc=1 }
  done

  rm -rf "$test_home"
  return $rc
}

# One spelling of "ask me", everywhere, with the retired ones deleted rather than
# left accepted-but-undocumented -- two vocabularies for one idea is how a user ends
# up guessing which one this version wants.
#
# `-i` / `--interactive` is a FLAG, so it appears once per line and position says what
# it applies to. Both predecessors failed on shape, not taste. `@menu` was a VALUE, so
# it had to be repeated per slot and the documented launch line read
# `ccmorph @menu --model @menu` -- one token twice for two different questions. `?`
# was an alias for it and is a zsh glob: the caller's shell expands it before ccmorph
# starts. Probed both failure modes -- in a clean directory zsh aborts with "no
# matches found: ?" and ccmorph never runs; with any one-character name in the cwd
# the glob MATCHES, so `ccmorph set <acct> model ?` silently recorded that FILENAME
# as the default and reported success. A `noglob` alias papered over that, and only
# ever worked in a shell that had sourced the library -- absent on exactly the fresh
# machines reading the docs that advertised it. As an executable, no alias is possible
# at all, so the sentinel had to go rather than be defended.
test_one_interactive_flag_is_the_only_way_to_ask() {
  local rc=0

  # The flag must be accepted at the launch parser.
  grep -q -- '-i|--interactive)' "${CCMORPH_LIBRARY[@]}" || {
    print -u2 -- 'FAIL: -i/--interactive is not accepted at launch'
    rc=1
  }

  # Neither retired sentinel may still be MATCHED anywhere: not as a launch value,
  # not in `set`. A leftover branch is a second vocabulary that outlives its docs.
  grep -qE '\(@menu\|\\\?\)|@menu\|\\\?' "${CCMORPH_LIBRARY[@]}" && {
    print -u2 -- 'FAIL: a retired @menu/? sentinel branch is still live in the source'
    rc=1
  }

  # No alias may come back: it cannot work for an executable, and its presence
  # would signal a protection that does not exist.
  grep -qE "^alias (claude|ccmorph)=" "${CCMORPH_LIBRARY[@]}" && {
    print -u2 -- 'FAIL: a noglob alias is back; it cannot protect an executable command'
    rc=1
  }

  # Help must teach the flag, at the launch line and at both `set` verbs, and must
  # never advertise a bare `?` as an argument.
  # Taught on the LAUNCH line, whatever that line's verb is: `-i` has to be
  # discoverable where a reader is looking for how to launch.
  grep -qE '_ccm_help "ccmorph run -i"' "${CCMORPH_LIBRARY[@]}" || {
    print -u2 -- 'FAIL: help must teach -i on the launch line'
    rc=1
  }
  grep -qF '_ccm_help "ccmorph config [profile]"' "${CCMORPH_LIBRARY[@]}" || {
    print -u2 -- 'FAIL: help must teach configure for interactive editing'
    rc=1
  }
  grep -qE '_ccm_help "[^"]*\|\?\]' "${CCMORPH_LIBRARY[@]}" && {
    print -u2 -- 'FAIL: help advertises a bare ? the caller shell would glob; teach -i'
    rc=1
  }

  return $rc
}

test_every_credential_type_states_how_it_authenticates() {
  # A sub profile deliberately injects no credential (it authenticates from its
  # own config dir). An unknown type must fail loudly rather than fall through
  # to a credential-less launch, which surfaces as Claude Code's login picker
  # with nothing explaining why.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0

  printf '%s\n' 'type=sub' 'email=someone@example.com' \
    > "$HOME/.config/ccmorph/subacct.conf"
  local sub_env
  sub_env=$(ccmorph _env subacct 2>&1) || {
    print -u2 -- 'a sub profile must produce an environment'
    rm -rf "$test_home"; return 1
  }
  assert_contains "$sub_env" "CLAUDE_CONFIG_DIR=" \
    'a sub profile must still be pointed at its own config dir' || rc=1
  if print -r -- "$sub_env" | grep -qE 'ANTHROPIC_API_KEY=|CLAUDE_CODE_OAUTH_TOKEN='; then
    print -u2 -- 'a sub profile must inject no credential of its own'
    rc=1
  fi

  # An unsupported type is rejected. Written directly: the conf parser only
  # accepts known keys, but `type` itself is a free value.
  printf '%s\n' 'type=quantum' > "$HOME/.config/ccmorph/weird.conf"
  if ccmorph _env weird >/dev/null 2>&1; then
    print -u2 -- 'an unsupported credential type must not produce an environment'
    rc=1
  fi

  rm -rf "$test_home"
  return $rc
}

run_test() {
  local name="$1" rc=0
  [[ -z "${CCMORPH_TEST_FILTER:-}" || "$name" == *"$CCMORPH_TEST_FILTER"* ]] || return 0
  [[ -z "${CCMORPH_TEST_SHARD:-}" ]] ||
    (( tests_seen++ % ${CCMORPH_TEST_SHARD#*/} == ${CCMORPH_TEST_SHARD%/*} )) || return 0
  (( ++tests_run ))
  "$name" || rc=1
  # GH-434: fail a test whose child outlives it -- the leak that stranded the
  # landing batch -- and reap the child so nothing escapes the suite.
  _ccm_test_sweep_homes $(( _ccm_test_homes_mark + 1 )) || rc=1
  _ccm_test_homes_mark=$#_ccm_test_homes
  if (( rc == 0 )); then
    print -- "PASS: $name"
  else
    print -u2 -- "FAIL: $name"
    (( ++tests_failed ))
  fi
}

run_test test_subscription_profile_settings_override_global_launch_settings
run_test test_profile_settings_override_global_launch_settings
run_test test_proxy_gateway_credentials_reach_claude_only_through_the_environment

test_a_launch_survives_a_settings_projection_it_cannot_write() {
  # Some restricted process trees deny creating new files under the home tree.
  # Generating the profile's settings must then cost a warning, never the launch:
  # every per-launch value still reaches Claude Code through the environment.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'echo "settings-compose: Operation not permitted" >&2' 'exit 1' \
    > "$HOME/.config/ccmorph/libexec/settings-compose"
  chmod +x "$HOME/.config/ccmorph/libexec/settings-compose"
  source "$CCMORPH_SOURCE"

  local err rc=0
  err=$(_ccm_launch -a proxy prompt 2>&1 >/dev/null) || {
    print -u2 -- 'launch must succeed when the settings cannot be generated'
    rm -rf "$test_home"; return 1
  }
  local actual="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$actual" 'BASE_URL=http://proxy.invalid' \
    'gateway base URL must still reach Claude' || rc=1
  assert_contains "$actual" 'MODEL_ENV=bootstrap-model' \
    'the launch model must still reach Claude' || rc=1
  assert_contains "$err" 'previously generated settings' \
    'the degraded launch must say so' || rc=1

  rm -rf "$test_home"
  return $rc
}
run_test test_a_launch_survives_a_settings_projection_it_cannot_write

test_a_launch_refuses_when_its_settings_policy_cannot_be_read() {
  local kind="$1" test_home err rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local dir="$_CCMORPH_PROFILE_DIRS/proxy"
  mkdir -p "$dir"
  ln -s "$HOME/policy.json" "$dir/settings.policy.json"
  [[ "$kind" == invalid ]] && print -r -- '{' > "$HOME/policy.json"
  [[ "$kind" == deep ]] && python3 -c 'print("{\"sandbox\":" + "[" * 2000 + "0" + "]" * 2000 + "}")' \
    > "$HOME/policy.json"

  err=$(_ccm_launch -a proxy -p verify 2>&1 >/dev/null) && {
    print -u2 -- "launch must refuse an unreadable settings policy ($kind, issue 308)"
    rc=1
  }
  assert_contains "$err" 'settings.policy.json' \
    'the refusal must name the policy file' || rc=1
  [[ ! -s "$CCMORPH_TEST_LOG" ]] || {
    print -u2 -- 'Claude must not start without its policy'
    rc=1
  }
  [[ ! -e "$dir/settings.json" ]] || {
    print -u2 -- 'a failed policy projection must not write settings'
    rc=1
  }

  rm -rf "$test_home"
  return $rc
}
test_a_launch_refuses_a_dangling_policy_issue_308() {
  test_a_launch_refuses_when_its_settings_policy_cannot_be_read dangling
}
test_a_launch_refuses_an_invalid_policy_issue_308() {
  test_a_launch_refuses_when_its_settings_policy_cannot_be_read invalid
}
test_a_launch_refuses_a_too_deep_policy_issue_318() {
  test_a_launch_refuses_when_its_settings_policy_cannot_be_read deep
}
run_test test_a_launch_refuses_a_dangling_policy_issue_308
run_test test_a_launch_refuses_an_invalid_policy_issue_308
run_test test_a_launch_refuses_a_too_deep_policy_issue_318

test_a_launch_with_stale_settings_and_a_new_policy() {
  # A projection can fail for a reason that has nothing to do with the policy --
  # an unopenable lock, an invalid canonical file -- and the launch then falls back
  # to the settings generated last time. When a policy was linked since, those
  # settings predate it, so starting with them would run without its sandbox.
  local kind="$1" test_home err launch_rc=0 rc=0
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local dir="$_CCMORPH_PROFILE_DIRS/proxy"
  mkdir -p "$dir"
  # An invalid canonical stops a profile ccmorph has not configured even earlier,
  # in the config store that derives its defaults from it. Seeding them first is
  # the profile in daily use, and the one whose launch reaches the projection.
  [[ "$kind" == canonical ]] && _ccm_config_store seed-model-defaults proxy >/dev/null
  print -r -- '{}' > "$dir/settings.json"
  print -r -- '{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/Documents"]}}}' \
    > "$HOME/policy.json"
  ln -s "$HOME/policy.json" "$dir/settings.policy.json"
  if [[ "$kind" == canonical ]]; then
    print -r -- '{' > "$HOME/.claude/settings.json"
  else
    # `carried` generates its settings with the policy in place first: the one
    # fallback that is still safe.
    [[ "$kind" == carried ]] && _ccm_settings_projection materialize proxy 2>/dev/null
    rm -f "$dir/cache/.settings.lock"
    mkdir -p "$dir/cache/.settings.lock"
  fi

  err=$(_ccm_launch -a proxy -p verify 2>&1 >/dev/null) || launch_rc=$?
  if [[ "$kind" == carried ]]; then
    (( launch_rc == 0 )) || {
      print -u2 -- 'a launch may keep stale settings that carry the policy (issue 319)'
      rm -rf "$test_home"; return 1
    }
    assert_not_contains "$err" 'settings.policy.json' \
      'carried settings must not read as a refusal' || rc=1
    assert_contains "$err" 'previously generated settings' \
      'the degraded launch must say which settings it starts with' || rc=1
    [[ -s "$CCMORPH_TEST_LOG" ]] || {
      print -u2 -- 'Claude must start when the stale settings carry the policy'
      rc=1
    }
  else
    (( launch_rc != 0 )) || {
      print -u2 -- "launch must refuse stale settings without the policy ($kind, issue 319)"
      rc=1
    }
    assert_contains "$err" 'settings.policy.json' \
      'the refusal must name the policy file' || rc=1
    [[ ! -s "$CCMORPH_TEST_LOG" ]] || {
      print -u2 -- 'Claude must not start without its policy'
      rc=1
    }
    [[ "$(<"$dir/settings.json")" == '{}' ]] || {
      print -u2 -- 'a refused launch must leave the stale settings alone'
      rc=1
    }
  fi

  rm -rf "$test_home"
  return $rc
}
test_a_launch_refuses_stale_settings_when_the_lock_cannot_open_issue_319() {
  test_a_launch_with_stale_settings_and_a_new_policy lock
}
test_a_launch_refuses_stale_settings_when_canonical_is_invalid_issue_319() {
  test_a_launch_with_stale_settings_and_a_new_policy canonical
}
test_a_launch_keeps_stale_settings_that_carry_the_policy_issue_319() {
  test_a_launch_with_stale_settings_and_a_new_policy carried
}
run_test test_a_launch_refuses_stale_settings_when_the_lock_cannot_open_issue_319
run_test test_a_launch_refuses_stale_settings_when_canonical_is_invalid_issue_319
run_test test_a_launch_keeps_stale_settings_that_carry_the_policy_issue_319

test_the_fold_runs_exactly_when_settings_json_changed() {
  # The launch skips the fold by comparing settings.json with the bytes the last
  # projection saved. Content, not timestamps: a restored or copied file keeps an
  # old mtime and must still be folded, and a missing saved copy must fail open.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local dir="$_CCMORPH_PROFILE_DIRS/gw" rc=0
  mkdir -p "$dir/cache"
  printf '%s\n' '#!/bin/sh' 'echo "$1" >> "$HOME/fold.log"' > "$CCMORPH_LIB_DIR/../libexec/settings-compose"
  chmod +x "$CCMORPH_LIB_DIR/../libexec/settings-compose"

  print -r -- '{"a": 1}' > "$dir/settings.json"
  cp "$dir/settings.json" "$dir/cache/settings.base.json"
  _ccm_fold_settings gw
  [[ ! -e "$HOME/fold.log" ]] || { print -u2 -- 'FAIL: an unedited settings.json must not spawn a fold'; rc=1; }

  print -r -- '{"a": 2}' > "$dir/settings.json"
  touch -t 200001010000 "$dir/settings.json"
  _ccm_fold_settings gw
  assert_contains "$(<"$HOME/fold.log" 2>/dev/null)" fold \
    'an edit with an old timestamp must still be folded' || rc=1

  : > "$HOME/fold.log"
  rm -f "$dir/cache/settings.base.json"
  _ccm_fold_settings gw
  assert_contains "$(<"$HOME/fold.log")" fold \
    'a missing saved copy must run the fold, never skip it' || rc=1

  rm -rf "$test_home"
  return $rc
}
run_test test_the_fold_runs_exactly_when_settings_json_changed

test_a_nested_launch_never_inherits_an_outer_advisory() {
  # CCMORPH_ADVISE belongs to the session it was computed for. A launch started
  # from inside that session must show its own advisory or none.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0
  CCMORPH_ADVISE='{"systemMessage": "ccmorph: stale outer advisory"}' _ccm_launch -a proxy prompt ||
    { rm -rf "$test_home"; return 1; }
  assert_contains "$(<"$CCMORPH_TEST_LOG")" 'ADVISE=<unset>' \
    'an inherited advisory must not reach the nested session' || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_a_nested_launch_never_inherits_an_outer_advisory

test_the_advisory_is_valid_json() {
  # The hook prints CCMORPH_ADVISE verbatim as a SessionStart systemMessage, so it
  # must be JSON a real encoder produced: a hand-escaped string broke on the first
  # quote or control character a gateway model id carried.
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=small' \
    'env.CLAUDE_CODE_MAX_OUTPUT_TOKENS=8192' > "$HOME/.config/ccmorph/gw.conf"
  write_profile_context gw small=200000 'one"fifty'=150000
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"availableModels":["small","one\"fifty"]}' \
    > "$HOME/.config/ccmorph/profiles/gw/settings.json"
  source "$CCMORPH_SOURCE"

  local rc=0 advise
  _ccm_launch -a gw prompt >/dev/null 2>&1 || { rm -rf "$test_home"; return 1; }
  advise=$(grep '^ADVISE=' "$CCMORPH_TEST_LOG")
  advise=${advise#ADVISE=}
  [[ "$advise" != '<unset>' ]] || { print -u2 -- 'FAIL: the fixture must produce an advisory'; rc=1; }
  print -r -- "$advise" | python3 -c 'import json, sys; json.load(sys.stdin)' ||
    { print -u2 -- "FAIL: the advisory must be valid JSON: $advise"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_the_advisory_is_valid_json
run_test test_the_installer_wires_no_shell_and_installs_managed_commands
run_test test_the_command_resolves_its_library_and_forwards_every_argument
run_test test_the_command_reports_a_library_it_cannot_load
run_test test_a_bare_invocation_prints_usage_and_launches_nothing
run_test test_run_keeps_native_command_delimiters
run_test test_the_launcher_adds_nothing_unrequested
run_test test_source_names_no_operator_profile
run_test test_setup_dir_shares_output_styles_with_canonical
run_test test_setup_dir_removes_a_stale_ccmorph_statusline
run_test test_cleanup_keeps_a_user_bar_whose_path_merely_collides
run_test test_cleanup_removes_the_statusline_ccmorph_itself_wired
run_test test_setup_dir_never_touches_a_users_own_statusline
run_test test_setup_dir_is_idempotent_on_settings
run_test test_no_statusline_machinery_ships
run_test test_setup_dir_links_the_harness_even_before_content_exists
run_test test_add_context_refresh_is_background_and_declared_only
run_test test_launch_resolves_scoped_registry_globs
run_test test_context_refresh_does_not_wait_for_metadata
run_test test_launch_context_lookup_is_local_before_background_refresh
run_test test_probe_dispatch_preserves_explicit_inference_arguments
run_test test_add_gateway_profile_offers_probe
run_test test_add_native_profile_stays_quiet_about_probe
run_test test_an_unprobed_launch_is_not_given_a_siblings_ceiling
run_test test_the_launch_window_reports_every_window_fact
run_test test_a_broken_settings_compose_does_not_take_the_window_facts_with_it
run_test test_a_settings_model_still_sizes_the_budget
run_test test_launch_hints_when_a_routed_window_is_unprobed
run_test test_late_harness_files_heal_on_next_setup
run_test test_profile_local_harness_content_survives_an_upgrade
run_test test_an_unwritable_window_cache_is_silent_and_nonfatal
run_test test_flag_errors_name_the_spelling_the_user_typed
run_test test_source_names_no_machine_local_files
run_test test_provider_profile_accepts_picker_exclusions
run_test test_listings_are_undisguised_on_entry
run_test test_every_profile_keeps_its_own_secure_storage
run_test test_native_effort_does_not_rewrite_explicit_model
run_test test_native_effort_validation_belongs_to_claude
run_test test_proxy_model_is_bootstrap_fallback
run_test test_proxy_context_limit_reaches_claude
# Adapter choice is automatic at launch; add only persists profile credentials.
run_test test_add_never_asks_how_to_reach_the_endpoint
run_test test_add_apikey_native_endpoint_skips_adapter
run_test test_add_apikey_model_level_404_is_native
# add does not persist adapter decisions; launch performs automatic compatibility handling.
run_test test_help_teaches_credential_type_not_gateway_shape
run_test test_apikey_profile_exports_key_without_base_url
run_test test_apikey_profile_exports_gateway_base_url
run_test test_profile_advisor_disable_reaches_launch_without_leaking
run_test test_profile_simple_mode_reaches_launch_without_leaking
run_test test_apikey_profile_passes_unknown_model_window_flag
run_test test_apikey_profile_without_model_defers_to_claude_code
run_test test_apikey_profile_with_model_injects_it
run_test test_shell_env_does_not_leak_into_a_profile
run_test test_adapter_redirects_claude_to_the_local_port
run_test test_adapter_redirect_under_a_terminal_never_reaches_the_operators_picker
run_test test_task_env_becomes_request_headers_on_an_adapter_launch
run_test test_task_tags_merge_with_the_users_own_custom_headers_issue_208
run_test test_a_nul_in_the_users_header_cannot_inject_a_record_issue_208
run_test test_no_task_means_no_headers_and_bad_shapes_are_dropped
run_test test_a_launch_without_the_adapter_never_carries_task_headers
run_test test_adapter_is_stopped_when_the_session_ends
run_test test_adapter_doubles_die_with_a_killed_launcher
run_test test_adapter_launch_survives_an_unwritable_optional_log
# GH-321: a launch with nothing old to reap must not scan the host's processes.
_check_stale_overlay_cleanup() {
  local state="$1" expected_present="$2" expected_scans="$3" ps_status="${4:-0}"
  local test_home rc=0 f
  test_home=$(mktemp -d)
  setup_home "$test_home"
  # A $TMPDIR whose own path looks like a bridge must not make its neighbours one.
  local TMPDIR="$HOME/ccmorph-bridge-$UID-tmp"
  export TMPDIR
  mkdir -p "$TMPDIR" "$HOME/.config/ccmorph/profiles/demo/cache"
  printf '%s\n' '#!/bin/sh' 'printf "946731600\n"' > "$HOME/bin/date"
  printf '%s\n' '#!/bin/sh' 'printf "scan\n" >> "$HOME/ps-scans"' \
    'cat "$HOME/live-processes"' "exit $ps_status" > "$HOME/bin/ps"
  chmod +x "$HOME/bin/date" "$HOME/bin/ps"
  : > "$HOME/live-processes"
  : > "$HOME/ps-scans"
  # Old, but not an overlay or a bridge: the single $TMPDIR read must spare it.
  local unrelated="$TMPDIR/ccmorph-rc-ready.test"
  mkdir "$unrelated"
  TZ=UTC touch -t 200001010000 "$unrelated"
  source "$CCMORPH_SOURCE"

  local -a candidates=( "$HOME/.config/ccmorph/profiles/demo/cache/launch-settings.test"
                        "$TMPDIR/ccmorph-launch-settings.test"
                        "$TMPDIR/ccmorph-bridge-$UID-test" )
  if [[ "$state" != empty ]]; then
    print -r -- '{}' > "${candidates[1]}"
    print -r -- '{}' > "${candidates[2]}"
    mkdir "${candidates[3]}"
    print -r -- '{}' > "${candidates[3]}/config.yaml"
    for f in "${candidates[@]}"; do
      if [[ "$state" == young ]]; then
        TZ=UTC touch -t 200001011200 "$f"
      else
        TZ=UTC touch -t 200001010000 "$f"
      fi
    done
    if [[ "$state" == live ]]; then
      printf '%s\n' "claude --settings ${candidates[1]}" \
        "claude --settings ${candidates[2]}" \
        "cliproxyapi --config ${candidates[3]}/config.yaml" > "$HOME/live-processes"
    fi
  fi

  _ccm_cleanup_stale_overlays

  local scans=( ${(f)"$(<"$HOME/ps-scans")"} )
  (( ${#scans} == expected_scans )) || {
    print -u2 -- "cleanup $state: expected $expected_scans process scans, got ${#scans}"
    rc=1
  }
  [[ -e "$unrelated" ]] || { print -u2 -- "cleanup $state: removed $unrelated"; rc=1; }
  for f in "${candidates[@]}"; do
    if [[ -e "$f" ]]; then
      [[ "$expected_present" == yes ]] || rc=1
    else
      [[ "$expected_present" == no ]] || rc=1
    fi
  done
  rm -rf "$test_home"
  return $rc
}

test_stale_overlay_cleanup_empty_issue_321() { _check_stale_overlay_cleanup empty no 0; }
test_stale_overlay_cleanup_young_issue_321() { _check_stale_overlay_cleanup young yes 0; }
test_stale_overlay_cleanup_abandoned_issue_321() { _check_stale_overlay_cleanup old no 1; }
test_stale_overlay_cleanup_live_issue_321() { _check_stale_overlay_cleanup live yes 1; }
test_stale_overlay_cleanup_failed_ps_issue_321() { _check_stale_overlay_cleanup live no 1 1; }
run_test test_stale_overlay_cleanup_empty_issue_321
run_test test_stale_overlay_cleanup_young_issue_321
run_test test_stale_overlay_cleanup_abandoned_issue_321
run_test test_stale_overlay_cleanup_live_issue_321
run_test test_stale_overlay_cleanup_failed_ps_issue_321

test_file_metadata_reads_portably() {
  # `stat -f %z/%m` is BSD-only. Both call sites swallowed the error GNU stat
  # returns (|| echo 0, || continue), so on Linux the log grew without bound and
  # stale launch-settings were never collected — silently, on the platform least
  # likely to be noticed from here. Guard the class: no `stat -f` in shipped
  # source, and the helpers behave correctly for present and absent files.
  local source_text="$(cat -- "${CCMORPH_LIBRARY[@]}")"
  local rc=0
  local uses
  uses=$(print -r -- "$source_text" | grep -c '^[^#]*stat -f')
  (( uses == 0 )) || {
    print -u2 -- "FAIL: $uses BSD-only 'stat -f' call(s) remain in shipped source"
    rc=1
  }

  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  print -n 'hello' > "$HOME/five"
  [[ "$(_ccm_size "$HOME/five")" == 5 ]] || {
    print -u2 -- 'FAIL: _ccm_size must report a real byte count'; rc=1
  }
  # 0 for an absent file: both callers treat 0 as "nothing to do", which is the
  # safe reading — never "rotate/collect this".
  [[ "$(_ccm_size "$HOME/nope")" == 0 ]] || {
    print -u2 -- 'FAIL: _ccm_size must report 0 for an absent file'; rc=1
  }
  _ccm_mtime "$HOME/five" >/dev/null || {
    print -u2 -- 'FAIL: _ccm_mtime must succeed for a present file'; rc=1
  }
  # Nonzero exit, NOT 0: a caller doing `|| continue` must skip. Returning 0
  # would read as epoch 0 — infinitely stale — and delete files it never saw.
  if _ccm_mtime "$HOME/nope" >/dev/null 2>&1; then
    print -u2 -- 'FAIL: _ccm_mtime must fail for an absent file'; rc=1
  fi

  # The rotation path must actually fire on an oversized log.
  _CCMORPH_LOG="$HOME/big.log"
  _CCMORPH_LOG_MAX=2048
  local i
  for i in {1..4000}; do print -r -- "filler line $i"; done > "$_CCMORPH_LOG"
  local before after
  before=$(_ccm_size "$_CCMORPH_LOG")
  _ccm_log "trigger"
  after=$(_ccm_size "$_CCMORPH_LOG")
  (( after < before )) || {
    print -u2 -- "FAIL: log rotation did not fire (before=$before after=$after)"
    rc=1
  }

  # Every one of these helpers indexes a 1-based zstat array. A caller with
  # KSH_ARRAYS set — an option a user's interactive shell may carry — reads
  # `$s[1]` as the whole value plus a literal `[1]` (`1790781633[1]`), and an
  # arithmetic `s[1]` as the second element, so every caller got garbage: the
  # stale-overlay sweep reaped nothing (it carries credentials, so leaving it is
  # the dangerous direction) and a world-readable log stayed that way. Exercise
  # the real call sites under that option; the subshell keeps it out of every
  # later test.
  local tmpd="$HOME/tmp"
  mkdir -p "$tmpd" "$_CCMORPH_PROFILE_DIRS/p/cache"
  print -r -- '{}' > "$_CCMORPH_PROFILE_DIRS/p/cache/launch-settings.stale"
  print -r -- '{}' > "$_CCMORPH_PROFILE_DIRS/p/cache/launch-settings.fresh"
  touch -t 202001010000 "$_CCMORPH_PROFILE_DIRS/p/cache/launch-settings.stale"
  local bridge="$tmpd/ccmorph-bridge-$UID-stale"
  mkdir -p "$bridge"
  print -r -- 'key: x' > "$bridge/config.yaml"
  touch -t 202001010000 "$bridge"
  print -r -- 'x' > "$HOME/loose.log"
  command chmod 644 "$HOME/loose.log"

  local -i ksh_rc=0
  ( setopt ksharrays
    TMPDIR="$tmpd"
    [[ "$(_ccm_size "$HOME/five")" == 5 ]] || exit 11
    [[ "$(_ccm_mtime "$HOME/five")" == <-> ]] || exit 12
    _ccm_cleanup_stale_overlays
    _CCMORPH_LOG="$HOME/loose.log"
    _ccm_log_init
    [[ -o ksharrays ]] || exit 13   # the helpers fix it locally, never for their caller
  ) || ksh_rc=$?
  (( ksh_rc == 0 )) || {
    print -u2 -- "FAIL: zstat helpers must ignore the caller's KSH_ARRAYS (exit $ksh_rc)"
    rc=1
  }
  [[ ! -e "$_CCMORPH_PROFILE_DIRS/p/cache/launch-settings.stale" ]] || {
    print -u2 -- 'FAIL: a stale launch-settings overlay must be reaped under KSH_ARRAYS'; rc=1
  }
  [[ -e "$_CCMORPH_PROFILE_DIRS/p/cache/launch-settings.fresh" ]] || {
    print -u2 -- 'FAIL: a fresh launch-settings overlay must survive cleanup under KSH_ARRAYS'; rc=1
  }
  [[ ! -e "$bridge" ]] || {
    print -u2 -- 'FAIL: a stale bridge dir must be reaped under KSH_ARRAYS'; rc=1
  }
  local -a loose_mode
  # A failed zstat must not read as "no group or other bits left".
  zstat -A loose_mode +mode "$HOME/loose.log" && (( (loose_mode[1] & 8#077) == 0 )) || {
    print -u2 -- 'FAIL: a loose log must be locked down under KSH_ARRAYS'; rc=1
  }

  rm -rf "$test_home"
  return $rc
}

run_test test_file_metadata_reads_portably

run_test test_adapter_launch_uses_current_process_tree
run_test test_no_ambient_proxy_means_the_adapter_gets_none
run_test test_an_ambient_proxy_reaches_the_child_and_the_adapter
# Custom endpoints use the automatic adapter path, even without a legacy adapter key.
run_test test_adapter_launch_withholds_mid_conversation_system_for_claude_models
run_test test_declared_model_capabilities_is_never_overruled
run_test test_native_endpoint_keeps_mid_conversation_system
run_test test_launch_aborts_when_the_adapter_cannot_serve
run_test test_adapter_accepted_on_apikey_with_base_url
run_test test_adapter_requires_a_base_url_to_front
run_test test_adapter_rejects_non_true_values
run_test test_monthly_budget_usd_loads_and_a_non_number_is_refused
run_test test_a_legacy_transport_key_is_reported_and_ignored
run_test test_the_profile_template_is_a_valid_conf_naming_every_key
run_test test_key_prints_the_api_key_for_an_apikey_account
run_test test_key_does_not_provision_claude_profile_state
run_test test_secret_prints_whichever_secret_the_account_holds
run_test test_secret_refuses_an_account_that_stores_no_secret
run_test test_legacy_route_key_is_rejected_as_unknown
run_test test_adapter_is_invalid_on_other_profile_types
run_test test_env_reports_the_real_endpoint_not_the_adapter
run_test test_models_does_not_send_anthropic_version
run_test test_versioned_base_url_is_not_given_a_second_v1
run_test test_models_lists_only_chat_capable_from_the_gateway
run_test test_models_discovery_uses_the_ambient_proxy
run_test test_models_keeps_entries_that_declare_no_capabilities
run_test test_models_defaults_to_anthropic_when_profile_has_no_base_url
run_test test_models_rejects_a_profile_with_no_key
run_test test_menu_returns_a_bare_model_id
run_test test_the_former_pick_name_still_works
run_test test_claude_subcommands_are_asked_for_never_hardcoded
run_test test_arguments_reach_claude_exactly_as_given
run_test test_a_missing_argument_prints_usage_under_nounset
run_test test_claude_subcommands_reach_claude_through_the_launch_pipeline
run_test test_a_retired_verb_is_rejected_not_merely_undocumented
run_test test_only_the_current_vocabulary_is_taught
run_test test_the_retired_window_verb_still_dispatches
run_test test_the_window_cache_holds_every_model_it_is_told
run_test test_menu_with_no_account_chains_from_the_account_chooser
run_test test_version_accepts_the_conventional_flags
run_test test_help_names_an_account_argument_unambiguously
run_test test_the_listing_shows_the_model_that_will_actually_launch
run_test test_the_listing_names_an_account_with_no_default_model
run_test test_model_command_sets_the_profiles_default_model
run_test test_set_model_interactively_saves_the_chosen_id
run_test test_set_effort_interactively_offers_the_engine_levels
run_test test_the_picker_without_fzf_names_the_direct_command_and_the_choices
run_test test_saved_default_model_beats_the_confs_bootstrap_model_at_launch
run_test test_explicit_model_flag_beats_the_saved_default
run_test test_effort_command_leaves_native_per_model_state_untouched
run_test test_effort_command_sets_the_profiles_default_effort
run_test test_effort_command_refuses_a_level_claude_code_would_drop
run_test test_set_model_refuses_a_flag_shaped_value
run_test test_set_adapter_is_rejected_as_automatic
run_test test_effort_interactive_still_requires_a_real_profile
run_test test_launch_interactive_resolves_the_account
run_test test_launch_interactive_with_an_account_asks_only_the_model
run_test test_public_model_picker_flag_asks_only_for_model
run_test test_interactive_asks_no_model_where_ccmorph_owns_no_list
run_test test_cancelled_profile_picker_launches_nothing
run_test test_profile_picker_does_not_change_default
run_test test_profile_pick_precedes_model_pick
run_test test_launch_interactive_resolves_a_real_model
run_test test_launch_aborts_when_pick_is_cancelled
run_test test_apikey_explicit_model_overrides_profile_model
run_test test_apikey_without_model_passes_explicit_model_through
run_test test_apikey_profile_requires_a_key
run_test test_apikey_profile_rejects_non_http_base_url
run_test test_subscription_profile_does_not_inject_model
run_test test_direct_resume_recovers_last_main_thread_model_command
run_test test_direct_resume_derives_the_carried_claude_send_id
run_test test_direct_resume_carries_a_model_without_a_profile_default
run_test test_direct_resume_recovers_the_last_assistant_model_without_a_model_command
run_test test_an_option_value_that_looks_like_a_flag_is_not_that_flag
run_test test_a_direct_launch_forwards_a_native_value_that_looks_like_a_launcher_flag
run_test test_direct_resume_reports_a_rejected_carried_model
run_test test_mapped_model_resume_and_hop_keep_servable_identity
run_test test_mapped_model_known_matches_shim_scope_and_suffix
run_test test_mapped_model_known_preserves_catalogue_uncertainty
run_test test_model_known_strips_only_the_terminal_window_suffix
run_test test_invalid_resume_model_falls_back_to_profile_model
run_test test_resume_rejects_flag_picker_and_multitoken_model_values
run_test test_hop_relays_model_and_effort
run_test test_hop_without_profile_uses_current_profile
run_test test_hop_falls_back_when_the_target_cannot_serve_the_model
run_test test_hop_asks_the_gateway_which_models_it_serves
run_test test_gateway_catalogue_uses_route_limit_and_pagination_uncertainty
run_test test_gateway_http_error_is_uncertainty_not_model_absence
run_test test_gateway_explicit_non_chat_model_is_not_carryable
run_test test_relay_resume_preserves_dangerous_permission_mode
run_test test_relay_same_profile_omits_cache_notice_issue_226
run_test test_relay_same_file_profile_omits_cache_notice_issue_226
run_test test_relay_failed_switch_omits_cache_notice_issue_226
run_test test_relay_settings_preflight_omits_cache_notice_issue_226
run_test test_relay_provider_preflight_omits_cache_notice_issue_226
run_test test_relay_private_preflight_omits_cache_notice_issue_226
run_test test_prelaunch_runs_configured_step_silently
run_test test_local_sh_loads_and_its_exports_take_effect
run_test test_absent_local_sh_is_a_clean_noop
run_test test_broken_local_sh_does_not_abort_the_source
run_test test_shipped_surface_never_names_the_transport_product
run_test test_no_embedded_program_carries_a_bare_apostrophe
run_test test_no_context_window_is_written_into_the_source
run_test test_no_reader_instruction_carries_an_authors_placeholder
run_test test_onboarding_documents_no_wiring_step_at_all
run_test test_adapter_spawn_never_inherits_a_tty
run_test test_a_failed_migration_is_loud
run_test test_installers_are_portable_and_leak_nothing
run_test test_a_renamed_state_file_migrates_itself
run_test test_chpwd_hooks_cannot_corrupt_migrated_links
run_test test_a_link_target_ending_in_a_newline_survives_the_migration
run_test test_missing_parent_links_still_migrate
run_test test_newline_names_migrate_whole_and_stay_inside_the_tree
run_test test_every_state_file_is_gitignored
run_test test_every_module_is_sourced_once_and_defines_its_own_functions
run_test test_runtime_data_never_looks_like_a_profile
run_test test_one_models_listing_per_launch_is_shared
run_test test_code_and_state_are_separate_directories
run_test test_source_carries_no_host_specific_defaults
run_test test_an_unreachable_endpoint_names_the_network_not_an_exception
run_test test_proxy_profile_may_declare_adapter
run_test test_profile_may_pin_one_model_picker_row
run_test test_a_route_scoped_probe_beats_the_endpoint_declaration
run_test test_conf_window_beats_the_published_one
run_test test_endpoint_publishing_no_window_changes_nothing
run_test test_registry_supplies_window_when_endpoint_probe_has_none
run_test test_launch_reads_its_arrays_under_a_callers_ksh_arrays
run_test test_the_window_env_covers_every_model_the_route_serves
run_test test_the_launched_model_budgets_through_the_native_setting
run_test test_a_mapped_alias_budgets_against_its_target
run_test test_a_session_never_offers_a_model_it_cannot_compact
run_test test_a_small_launch_still_offers_the_whole_route
run_test test_withholding_never_edits_the_profile
run_test test_by_default_every_model_stays_selectable_and_is_named
run_test test_a_users_saved_autocompact_choice_survives_derivation
run_test test_a_policy_tombstone_does_not_hide_a_canonical_budget
run_test test_a_claude_default_still_sizes_the_route_for_switches
run_test test_a_tier_alias_budgets_from_the_model_behind_it
run_test test_the_budget_never_exceeds_the_launched_models_wall
run_test test_the_window_follows_the_saved_default_model
run_test test_launch_model_flag_drives_the_window_not_the_conf_default
run_test test_a_flat_endpoint_declaration_yields_to_probe_at_launch
run_test test_a_genuine_shared_window_is_not_mistaken_for_flat
run_test test_a_claude_id_keeps_claude_codes_own_window
run_test test_a_derived_window_survives_an_endpoint_blip
run_test test_launch_warns_when_a_model_refuses_before_compacting
run_test test_launch_names_switch_targets_the_trigger_would_overrun
run_test test_the_reported_trigger_is_the_engines_own_arithmetic
run_test test_a_legacy_id_uses_its_own_output_default
run_test test_hazard_set_honours_a_profile_tombstone_of_availablemodels
run_test test_the_session_budget_caps_every_sibling_ceiling
run_test test_a_bare_and_1m_pair_are_two_different_siblings
run_test test_a_claude_named_routed_model_is_sized_by_its_own_id
run_test test_the_advisory_and_the_withhold_share_one_budget
run_test test_a_layer_percentage_sizes_the_hazard_set
run_test test_the_percentage_parser_skips_a_zero_width_no_break_space
run_test test_the_percentage_parser_uses_engine_whitespace
run_test test_whitespace_handling_matches_the_engine
run_test test_the_clamp_bounds_by_length_before_arithmetic
run_test test_an_overflowing_env_value_is_invalid_not_an_error
run_test test_neither_env_parser_accepts_non_ascii_digits
run_test test_an_env_budget_is_parsed_the_way_the_engine_parses_it
run_test test_the_output_reserve_override_reaches_the_hazard_path
run_test test_an_out_of_schema_saved_budget_does_not_block_derivation
run_test test_a_non_finite_saved_budget_does_not_abort_the_launch
run_test test_the_overlay_and_the_advisory_agree_on_the_schema
run_test test_a_valid_saved_budget_still_beats_the_derivation
run_test test_a_conf_pinned_ceiling_still_gets_hazard_analysis
run_test test_the_percentage_divides_before_it_multiplies
run_test test_the_trigger_follows_a_non_default_percentage
run_test test_no_launch_advisory_relies_on_pre_exec_stderr
run_test test_the_unprobed_model_advisory_persists_in_the_tui
run_test test_the_switch_warning_caps_the_names_it_lists
run_test test_the_launch_hazard_advisory_matches_switch_hazard_warn
run_test test_the_hazard_record_sizes_from_the_percentage_the_launch_injects
run_test test_a_malformed_layer_percentage_leaves_the_injected_one_in_charge
run_test test_no_switch_warning_when_the_whole_route_clears_the_trigger
run_test test_launch_is_silent_when_every_model_has_headroom
run_test test_launch_projects_picker_rows_with_our_own_tag
run_test test_projected_row_carries_the_profiles_own_tag
run_test test_projection_leaves_everything_alone_when_the_endpoint_is_down
run_test test_projection_undisguises_ids_and_labels_every_row_with_the_id
run_test test_projection_never_drops_the_profiles_own_model
run_test test_relay_relaunch_keeps_a_mapped_alias_allowed
run_test test_projected_rows_use_the_real_model_id
run_test test_projection_undisguises_a_stored_model
run_test test_the_picker_offers_a_model_whatever_its_siblings_declare
run_test test_picker_follows_gateway_chat_capabilities
run_test test_picker_skips_a_malformed_non_string_model_id
run_test test_picker_uses_a_route_scoped_registry_probe
run_test test_the_trigger_env_survives_a_delimiter_in_a_value
run_test test_the_unsafe_warning_looks_up_a_ceiling_like_its_sibling
run_test test_the_launch_sweep_clears_every_allowlisted_env_key
run_test test_the_kill_switch_is_settable_and_reads_like_the_engine
run_test test_the_one_million_tag_counts_anywhere_in_the_id
run_test test_the_one_million_kill_switch_lowers_the_ceiling
run_test test_the_unsafe_warning_uses_the_engine_output_reserve
run_test test_the_unsafe_warning_uses_the_engine_trigger
run_test test_the_unsafe_warning_uses_a_layer_only_percentage
run_test test_launch_warns_about_a_hand_set_disguised_model
run_test test_projection_omits_models_that_cannot_serve_a_session
run_test test_picker_without_a_registry_offers_rows_and_spawns_nothing
run_test test_picker_projects_a_derived_1m_row_for_a_probed_claude
run_test test_launch_substitutes_the_derived_send_id_for_a_claude_model
run_test test_inspect_uses_the_fleet_or_default_compaction_percent
run_test test_launch_compacts_at_fleet_percent_unless_the_conf_overrules
run_test test_a_settings_file_compaction_trigger_is_not_overruled
run_test test_add_sub_completes_the_browser_login
run_test test_launch_skips_the_sizer_when_nothing_is_new
run_test test_logged_email_reads_the_given_config_dir
run_test test_add_sub_auth_calls_scrub_securestorage_dir
run_test test_logged_email_scrubs_every_backend_env_key
run_test test_one_interactive_flag_is_the_only_way_to_ask
run_test test_every_credential_type_states_how_it_authenticates

# Bootstrap must observe the projected cache, not race a background refresh.
test_picker_projection_is_complete_before_claude_entry() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  printf '%s\n' 'type=proxy' 'base_url=http://gw.invalid' 'api_key=test-key' 'model=opaque-model' > "$HOME/.config/ccmorph/gw.conf"
  mkdir -p "$HOME/.config/ccmorph/profiles/gw"
  printf '%s\n' '{"additionalModelOptionsCache":[{"value":"opaque-model","label":"Cosmetic","description":"stale"}]}' > "$HOME/.config/ccmorph/profiles/gw/.claude.json"
  printf '%s\n' \
    '#!/bin/sh' \
    'for a do case "$a" in *http_code*) printf 200; exit 0 ;; esac; done' \
    'sleep 1' \
    'cat <<JSON' \
    '{"object":"list","data":[{"id":"opaque-model","display_name":"Cosmetic","capabilities":{"chat_completion":true}}]}' \
    'JSON' \
    > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  printf '%s\n' '#!/bin/sh' 'python3 -c '\''import json,os,sys; d=json.load(open(os.environ["CLAUDE_CONFIG_DIR"]+"/.claude.json")); assert all(r["label"] == r["value"] for r in d["additionalModelOptionsCache"])'\'' ' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"
  _ccm_launch -a gw prompt
  local rc=$?
  rm -rf "$test_home"
  return $rc
}
run_test test_picker_projection_is_complete_before_claude_entry

test_claude_inherits_callers_tmux_environment() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "${TMUX-<unset>}"' > "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"
  local rc=0 actual
  actual=$(TMUX='/tmp/test-tmux/default,123,0' _ccm_run_claude --)
  [[ "$actual" == '/tmp/test-tmux/default,123,0' ]] || rc=1
  actual=$(TMUX= _ccm_run_claude --)
  [[ "$actual" == '' ]] || rc=1
  actual=$(unset TMUX; _ccm_run_claude --)
  [[ "$actual" == '<unset>' ]] || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_claude_inherits_callers_tmux_environment

test_context_verb_reads_sets_and_clears_one_models_window() {
  # `probe` tells a reader to run this when a model is too new for the gateway and
  # every catalogue to publish a size. It has to exist, and it has to be the whole
  # loop -- read, set, clear -- or the advice sends them back to hand-editing a
  # .conf the CLI otherwise owns.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 actual

  printf '%s\n' 'type=apikey' 'api_key=canary' \
    'base_url=https://gw.example.com' > "$HOME/.config/ccmorph/gw.conf"

  actual=$(ccmorph context gw brand-new-model 2>&1)
  assert_contains "$actual" 'auto' 'an unset window reads as automatic' || rc=1

  actual=$(ccmorph context gw brand-new-model 128000 2>&1)
  assert_contains "$actual" '128000' 'setting a window confirms the number' || rc=1
  actual=$(ccmorph context gw brand-new-model 2>&1)
  [[ "$actual" == '128000' ]] || {
    print -u2 -- "a set window must read back: ${(qqq)actual}"; rc=1
  }
  assert_contains "$(<"$HOME/.config/ccmorph/gw.conf")" \
    'context_window.brand-new-model=128000' \
    'the window belongs in the profile it was set on' || rc=1

  ccmorph context gw brand-new-model auto >/dev/null 2>&1 || rc=1
  actual=$(ccmorph context gw brand-new-model 2>&1)
  [[ "$actual" == 'auto' ]] || {
    print -u2 -- "auto must return the model to discovery: ${(qqq)actual}"; rc=1
  }

  # An unknown profile and a missing model are usage errors, not silent writes.
  ccmorph context nosuch some-model 2>/dev/null && rc=1
  ccmorph context gw 2>/dev/null && rc=1

  rm -rf "$test_home"
  return $rc
}
run_test test_context_verb_reads_sets_and_clears_one_models_window

test_picker_uses_gateway_before_adapter_and_settings_overlay() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  write_apikey_conf gw 'api_key=canary' 'base_url=http://gateway.invalid' 'model=first' 'adapter=true'
  printf '%s\n' '#!/bin/sh' \
    'case "$*" in *127.0.0.1*) printf '\''{"data":[{"id":"first"}]}'\'' ;; *http_code*) printf 200 ;; *) printf '\''{"data":[{"id":"first"},{"id":"new-chat","capabilities":{"chat_completion":true}},{"id":"claude-new-9","capabilities":{"chat_completion":true}},{"id":"embedding","capabilities":{"chat_completion":false}}]}'\'' ;; esac' > "$HOME/bin/curl"
  fake_adapter_picker_port
  source "$CCMORPH_SOURCE"
  _ccm_launch -a gw prompt >/dev/null 2>&1
  local rc=$? state="$(<"$HOME/.config/ccmorph/profiles/gw/.claude.json")" logged="$(<"$CCMORPH_TEST_LOG")"
  assert_contains "$state" 'new-chat' 'picker must use upstream not one-model adapter' || rc=1
  assert_contains "$state" 'claude-new-9' 'unknown Claude models stay visible' || rc=1
  assert_not_contains "$state" '"value": "embedding"' 'explicit non-chat models excluded' || rc=1
  assert_contains "$logged" 'new-chat' 'launch settings must include newly discovered rows on first launch' || rc=1
  assert_contains "$logged" 'BASE_URL=http://127.0.0.1:49123' 'inference must still use the adapter' || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_picker_uses_gateway_before_adapter_and_settings_overlay

test_model_mappings_work_for_every_credential_type() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 kind name output
  for kind in sub token apikey proxy provider; do
    name="mapping_$kind"
    case "$kind" in
      sub) print -r -- $'type=sub\nemail=test@example.invalid' > "$_CCMORPH_DIR/$name.conf" ;;
      token) print -r -- $'type=token\ntoken=test-token' > "$_CCMORPH_DIR/$name.conf" ;;
      apikey|proxy) print -r -- "type=$kind"$'\napi_key=test-key\nbase_url=https://gateway.invalid' > "$_CCMORPH_DIR/$name.conf" ;;
      provider) print -r -- $'type=provider\nprovider=codex' > "$_CCMORPH_DIR/$name.conf" ;;
    esac
    print -r -- $'env.ANTHROPIC_DEFAULT_OPUS_MODEL=custom-opus\nenv.ANTHROPIC_SMALL_FAST_MODEL=custom-fast' >> "$_CCMORPH_DIR/$name.conf"
    output=$(ccmorph _env "$name") || rc=1
    assert_contains "$output" 'ANTHROPIC_DEFAULT_OPUS_MODEL=custom-opus' "$kind exports its tier mapping" || rc=1
    assert_contains "$output" 'ANTHROPIC_SMALL_FAST_MODEL=custom-fast' "$kind exports its helper mapping" || rc=1
    if [[ "$kind" == sub || "$kind" == token ]]; then
      print -r -- 'env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=999999' >> "$_CCMORPH_DIR/$name.conf"
      _ccm_read "$name" 1 >/dev/null 2>&1 && rc=1
    fi
  done
  rm -rf "$test_home"
  return $rc
}
run_test test_model_mappings_work_for_every_credential_type

test_launch_refuses_a_model_map_key_no_request_can_carry() {
  # A map is keyed by the model id the gateway sees, and the [1m] window marker is
  # stripped before any lookup, so a key carrying it can never name a model: the
  # map would sit unreachable and decide nothing. Refuse it before the session
  # starts, naming the rule, rather than launching an inert map -- in either
  # spelling ccmorph reads the marker (the picker takes it case-insensitively).
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"

  local rc=0 i=0 key
  for key in 'claude-sonnet-5-5[1m]' 'claude-sonnet-5-5[1M]'; do
    (( ++i ))
    write_apikey_conf "mapsuffixed$i" 'api_key=sk_prod_test' 'base_url=https://gateway.invalid' \
      "model_map={\"$key\":\"gpt-6.1-sol\"}"
    _ccm_launch -a "mapsuffixed$i" prompt >/dev/null 2>"$test_home/launch.err" && {
      print -u2 -- "a map key carrying the window marker must be refused at launch, not run as an inert map: $key"
      rc=1
    }
    assert_contains "$(<"$test_home/launch.err")" 'model_map' \
      "the refusal must name the map rule: $key" || rc=1
    [[ ! -s "$CCMORPH_TEST_LOG" ]] || {
      print -u2 -- "a refused map must not start Claude Code at all: $key"
      rc=1
    }
  done

  # The same profile with the wire spelling of the key still launches: the rule
  # rejects an unreachable key, not the map itself.
  write_apikey_conf mapbare 'api_key=sk_prod_test' 'base_url=https://gateway.invalid' \
    'model_map={"claude-sonnet-5-5":"gpt-6.1-sol"}'
  _ccm_launch -a mapbare prompt >/dev/null 2>&1 || {
    print -u2 -- 'a map keyed by the wire model id must still launch'
    rc=1
  }
  rm -rf "$test_home"
  return $rc
}
run_test test_launch_refuses_a_model_map_key_no_request_can_carry

test_adapter_controls_do_not_reach_claude_or_overlay() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  printf '%s\n' '#!/bin/sh' 'printf "%s|%s|%s|%s|%s|%s" "${CCMORPH_REPAIR_TOOL_REFERENCES-unset}" "${CCMORPH_STALL_TIMEOUT-unset}" "${CCMORPH_UPSTREAM_PROXY-unset}" "${CCMORPH_RANDOM_DEVICE_ID-unset}" "${CCMORPH_AFFINITY_HEADER-unset}" "${CCMORPH_SHIM_HOOKS-unset}"' > "$HOME/bin/claude"
  local actual rc=0
  actual=$(CCMORPH_REPAIR_TOOL_REFERENCES=1 CCMORPH_STALL_TIMEOUT=1 CCMORPH_UPSTREAM_PROXY=http://127.0.0.1:1 CCMORPH_RANDOM_DEVICE_ID=1 CCMORPH_AFFINITY_HEADER=X-A CCMORPH_SHIM_HOOKS=/tmp/hooks.py _ccm_run_claude CCMORPH_REPAIR_TOOL_REFERENCES=1 CCMORPH_STALL_TIMEOUT=1 CCMORPH_UPSTREAM_PROXY=http://127.0.0.1:1 CCMORPH_RANDOM_DEVICE_ID=1 CCMORPH_AFFINITY_HEADER=X-A CCMORPH_SHIM_HOOKS=/tmp/hooks.py --)
  [[ "$actual" == 'unset|unset|unset|unset|unset|unset' ]] || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_adapter_controls_do_not_reach_claude_or_overlay

test_private_session_leaves_no_transcript_behind() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local prof="$test_home/prof" rc=0 seen
  mkdir -p "$prof/projects" && print '{}' > "$prof/.claude.json"
  # The fake claude writes a transcript and reports where its config dir was.
  printf '%s\n' '#!/bin/sh' 'mkdir -p "$CLAUDE_CONFIG_DIR/projects/p" && echo t > "$CLAUDE_CONFIG_DIR/projects/p/s.jsonl"; test -L "$CLAUDE_CONFIG_DIR/.claude.json" && echo "$CLAUDE_CONFIG_DIR"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  seen=$(_ccm_run_claude "CLAUDE_CONFIG_DIR=$prof" CCMORPH_PRIVATE=1 --)
  [[ -n "$seen" && "$seen" != "$prof" && "${seen:t}" == "${prof:t}" ]] || rc=1
  [[ ! -e "${seen:h}" ]] || rc=1
  [[ ! -e "$prof/projects/p" ]] || rc=1
  seen=$(_ccm_run_claude "CLAUDE_CONFIG_DIR=$prof" --)
  [[ -e "$prof/projects/p/s.jsonl" ]] || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_leaves_no_transcript_behind

test_private_session_key_stays_the_profiles_own() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local prof="$test_home/prof" rc=0 seen
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  printf '%s\n' '#!/bin/sh' 'echo "${CLAUDE_SECURESTORAGE_CONFIG_DIR-unset}|$CLAUDE_CONFIG_DIR"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  seen=$(_ccm_run_claude "CLAUDE_CONFIG_DIR=$prof" CCMORPH_PRIVATE=1 --)
  [[ "${seen%%|*}" == "$prof" && "${seen#*|}" != "$prof" ]] || { print -u2 -- "private: $seen"; rc=1; }
  seen=$(_ccm_run_claude "CLAUDE_CONFIG_DIR=$prof" --)
  [[ "$seen" == "unset|$prof" ]] || { print -u2 -- "normal: $seen"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_key_stays_the_profiles_own

test_private_session_is_removed_when_terminated() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 sig pid st info cpid fifo fd crutch
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  # The fake claude records the launcher's pid and config dir, writes a transcript, waits.
  printf '%s\n' '#!/bin/sh' 'mkdir -p "$CLAUDE_CONFIG_DIR/projects/p" && echo t > "$CLAUDE_CONFIG_DIR/projects/p/s.jsonl"' 'printf "%s\n" "$PPID $$ $CLAUDE_CONFIG_DIR" >&3 || exit 9' 'exec sleep 30' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  for sig in TERM HUP; do
    fifo="$test_home/started.$sig"
    mkfifo "$fifo"
    # Opened RDWR before the fork (GH-434): neither open can block on a launcher
    # that never starts, and holding the handle across the readiness read keeps
    # a writer on the fifo, so a ready fd below can only mean claude's report,
    # never a premature EOF. Dropped before the termination wait, whose EOF means
    # every holder of the write end is gone: the zsh -c, its claude subshell, the
    # stub that inherited fd 3.
    exec {crutch}<>"$fifo"
    exec {fd}<"$fifo"
    zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" 3>"$fifo" &
    pid=$!
    # No budget, a backstop: claude's report, or a launcher that never produced
    # one - dead or wedged, killed and reported either way.
    if ! _ccm_test_wait_readable $fd $hang; then
      if kill -0 $pid 2>/dev/null; then
        print -u2 -- "$sig: no report and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
        kill -KILL $pid 2>/dev/null
        wait $pid 2>/dev/null
      else
        wait $pid; st=$?
        print -u2 -- "$sig: claude did not start (the launcher exited $st first)"
      fi
      rc=1
      exec {fd}<&-; exec {crutch}>&-
      continue
    fi
    read -r info <&$fd
    cpid=${${info#* }%% *}
    kill -$sig ${info%% *}
    exec {crutch}>&-
    # Only the launcher was signalled: it must stop claude itself. EOF arrives when
    # every holder of the write end is gone (zsh -c, its claude subshell, the stub);
    # the shared bound ends a run whose forwarding is broken.
    if ! _ccm_test_wait_readable $fd $hang; then
      print -u2 -- "$sig: neither the launcher nor claude exited in ${CCMORPH_TEST_HANG_TIMEOUT}s"; rc=1
      kill -KILL $pid ${info%% *} $cpid 2>/dev/null   # exact pids: zsh -c, its subshell, the stub
    fi
    exec {fd}<&-
    wait $pid; st=$?
    kill -0 $cpid 2>/dev/null && { print -u2 -- "$sig: claude still running"; rc=1; kill -KILL $cpid 2>/dev/null; }
    [[ ! -e "${${info#* * }:h}" ]] || { print -u2 -- "$sig: private dir left behind"; rc=1; }
    (( st == 128 + ${signals[(i)$sig]} - 1 )) || { print -u2 -- "$sig: status $st"; rc=1; }
  done
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_is_removed_when_terminated

test_private_session_is_removed_when_hop_notice_hits_closed_stdout() {
  # The hop cache notice is written after the private view exists; a stdout with
  # no reader (SIGPIPE) must neither leave the view behind nor stop the launch (issue 226).
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 st=0 r w
  local TMPDIR="$test_home/tmp"
  mkdir -p "$prof" "$TMPDIR" && print '{}' > "$prof/.claude.json"
  printf '%s\n' '#!/bin/sh' ': > "$HOME/claude-started"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  # A pipe whose only reader is already closed: the first write raises SIGPIPE.
  mkfifo "$test_home/out"
  exec {r}<>"$test_home/out" {w}>"$test_home/out"
  exec {r}<&-
  TMPDIR="$TMPDIR" zsh -c 'source "$1"; _ccm_pending_cache_notice=notice; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' \
    _ "$CCMORPH_SOURCE" "$prof" >&$w || st=$?
  exec {w}>&-
  local -a left=("$TMPDIR"/ccmorph-private.*(DN))
  (( ${#left} == 0 )) || { print -u2 -- "private dir left behind: $left"; rc=1; }
  (( st == 0 )) || { print -u2 -- "status $st, expected claude's 0"; rc=1; }
  [[ -e "$HOME/claude-started" ]] || { print -u2 -- 'claude did not start after the notice write failed'; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_is_removed_when_hop_notice_hits_closed_stdout

test_private_session_is_removed_on_group_quit() {
  # Ctrl-\ sends SIGQUIT to the whole foreground group: the launcher must outlive
  # claude (which dies of it) and still remove the private dir.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 pid st info fifo fd crutch
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$PPID $$ $CLAUDE_CONFIG_DIR" >&3 || exit 9' 'exec sleep 30' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  fifo="$test_home/started.QUIT"
  mkfifo "$fifo"
  # Opened RDWR before the fork (GH-434): neither open can block on a launcher
  # that never starts, and holding the handle across the readiness read keeps
  # a writer on the fifo, so a ready fd below can only mean claude's report,
  # never a premature EOF. Dropped before the termination wait, whose EOF means
  # every holder of the write end is gone.
  exec {crutch}<>"$fifo"
  exec {fd}<"$fifo"
  perl -e 'setpgrp(0,0); exec @ARGV' zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" 3>"$fifo" &
  pid=$!
  # No budget, a backstop: claude's report, or a launcher that never produced
  # one - dead or wedged, killed and reported either way.
  if ! _ccm_test_wait_readable $fd $hang; then
    if kill -0 $pid 2>/dev/null; then
      print -u2 -- "QUIT: no report and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
      kill -KILL -- -$pid 2>/dev/null
      wait $pid 2>/dev/null
    else
      wait $pid; st=$?
      print -u2 -- "QUIT: claude did not start (the launcher exited $st first)"
    fi
    rc=1
    exec {fd}<&-; exec {crutch}>&-
    rm -rf "$test_home"; return $rc
  fi
  read -r info <&$fd
  kill -QUIT -- -$pid 2>/dev/null
  exec {crutch}>&-
  # EOF means every holder of the write end is gone; the launcher's EXIT trap
  # (which removes the private dir) runs before it can exit. The shared bound
  # ends a run whose teardown hangs.
  if ! _ccm_test_wait_readable $fd $hang; then
    print -u2 -- "QUIT: the launcher still running ${CCMORPH_TEST_HANG_TIMEOUT}s after the group quit"; rc=1
    kill -KILL -- -$pid 2>/dev/null
  fi
  exec {fd}<&-
  wait $pid
  [[ ! -e "${${info#* * }:h}" ]] || { print -u2 -- "QUIT: private dir left behind"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_is_removed_on_group_quit

test_private_session_forwards_term_and_keeps_claudes_status() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 sig pid st info cpid fifo fd crutch
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  # The fake claude shuts down cleanly on TERM/HUP, noting whether its config was
  # still there at that moment (the launcher must not remove it under claude).
  printf '%s\n' '#!/bin/sh' 'trap '\''test -e "$CLAUDE_CONFIG_DIR/.claude.json" && echo kept > "$SEEN"; exit 0'\'' TERM HUP' 'printf "%s\n" "$PPID $$ $CLAUDE_CONFIG_DIR" >&3 || exit 9' 'while :; do sleep 0.1; done' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  for sig in TERM HUP; do
    export SEEN="$test_home/seen.$sig"
    fifo="$test_home/started.$sig"
    mkfifo "$fifo"
    # Opened RDWR before the fork (GH-434): neither open can block on a launcher
    # that never starts, and holding the handle across the readiness read keeps
    # a writer on the fifo, so a ready fd below can only mean claude's report,
    # never a premature EOF. Dropped before the termination wait, whose EOF means
    # every holder of the write end is gone: the zsh -c, its claude subshell,
    # the stub that inherited fd 3.
    exec {crutch}<>"$fifo"
    exec {fd}<"$fifo"
    zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" 3>"$fifo" &
    pid=$!
    # No budget, a backstop: claude's report, or a launcher that never produced
    # one - dead or wedged, killed and reported either way.
    if ! _ccm_test_wait_readable $fd $hang; then
      if kill -0 $pid 2>/dev/null; then
        print -u2 -- "$sig: no report and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
        kill -KILL $pid 2>/dev/null
        wait $pid 2>/dev/null
      else
        wait $pid; st=$?
        print -u2 -- "$sig: claude did not start (the launcher exited $st first)"
      fi
      rc=1
      exec {fd}<&-; exec {crutch}>&-
      continue
    fi
    read -r info <&$fd
    cpid=${${info#* }%% *}
    kill -$sig ${info%% *}
    exec {crutch}>&-
    # EOF arrives when every holder of the write end is gone (zsh -c, its claude
    # subshell, the stub); the shared bound ends a run whose forwarding is broken.
    if ! _ccm_test_wait_readable $fd $hang; then
      print -u2 -- "$sig: neither the launcher nor claude exited in ${CCMORPH_TEST_HANG_TIMEOUT}s"; rc=1
      kill -KILL $pid ${info%% *} $cpid 2>/dev/null   # exact pids: zsh -c, its subshell, the stub
    fi
    exec {fd}<&-
    wait $pid; st=$?
    kill -0 $cpid 2>/dev/null && { print -u2 -- "$sig: claude still running"; rc=1; kill -KILL $cpid 2>/dev/null; }
    (( st == 0 )) || { print -u2 -- "$sig: status $st, want claude's 0"; rc=1; }
    [[ -s "$SEEN" && "$(<"$SEEN")" == kept ]] || { print -u2 -- "$sig: config gone before claude exited"; rc=1; }
    [[ ! -e "${${info#* * }:h}" ]] || { print -u2 -- "$sig: private dir left behind"; rc=1; }
  done
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_forwards_term_and_keeps_claudes_status

test_private_session_keeps_exit_status_after_stop_and_continue() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 pid st info cpid fifo fd crutch
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$PPID $$ $CLAUDE_CONFIG_DIR" >&3 || exit 9' 'sleep 1' 'exit 7' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  fifo="$test_home/started.STOP"
  mkfifo "$fifo"
  # Opened RDWR before the fork (GH-434): neither open can block on a launcher
  # that never starts, and holding the handle across the readiness read keeps
  # a writer on the fifo, so a ready fd below can only mean claude's report,
  # never a premature EOF.
  exec {crutch}<>"$fifo"
  exec {fd}<"$fifo"
  zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" 3>"$fifo" &
  pid=$!
  # No budget, a backstop: claude's report, or a launcher that never produced
  # one - dead or wedged, killed and reported either way.
  if ! _ccm_test_wait_readable $fd $hang; then
    if kill -0 $pid 2>/dev/null; then
      print -u2 -- "STOP/CONT: no report and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
      kill -KILL $pid 2>/dev/null
      wait $pid 2>/dev/null
    else
      wait $pid; st=$?
      print -u2 -- "STOP/CONT: claude did not start (the launcher exited $st first)"
    fi
    rc=1
    exec {fd}<&-; exec {crutch}>&-
    rm -rf "$test_home"; return $rc
  fi
  read -r info <&$fd
  exec {fd}<&-
  exec {crutch}>&-
  cpid=${${info#* }%% *}
  kill -STOP $cpid; sleep 0.3; kill -CONT $cpid
  wait $pid; st=$?
  (( st == 7 )) || { print -u2 -- "STOP/CONT: status $st, want claude's 7"; rc=1; }
  [[ ! -e "${${info#* * }:h}" ]] || { print -u2 -- "STOP/CONT: private dir left behind"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_keeps_exit_status_after_stop_and_continue

test_private_session_launcher_ends_with_claude() {
  # Neither the caller's noclobber nor a private dir deleted mid-session may keep
  # the launcher from starting claude or from exiting promptly with its status.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 mode pid st info cpid fifo fd crutch
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  # The fake claude waits for $GO, then exits 6.
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$PPID $$ $CLAUDE_CONFIG_DIR" >&3 || exit 9' 'while [ ! -e "$GO" ]; do sleep 0.1; done' 'exit 6' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  for mode in noclobber deleted; do
    export GO="$test_home/go.$mode"
    fifo="$test_home/started.$mode"
    mkfifo "$fifo"
    # Opened RDWR before the fork (GH-434): neither open can block on a launcher
    # that never starts, and holding the handle across the readiness read keeps
    # a writer on the fifo, so a ready fd below can only mean claude's report,
    # never a premature EOF. Dropped before the termination wait, whose EOF means
    # every holder of the write end is gone: the zsh -c, its claude subshell,
    # the stub that inherited fd 3.
    exec {crutch}<>"$fifo"
    exec {fd}<"$fifo"
    zsh -c '[[ $3 == noclobber ]] && setopt noclobber; source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" "$mode" 3>"$fifo" &
    pid=$!
    # No budget, a backstop: claude's report, or a launcher that never produced
    # one - dead or wedged, killed and reported either way.
    if ! _ccm_test_wait_readable $fd $hang; then
      if kill -0 $pid 2>/dev/null; then
        print -u2 -- "$mode: no report and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
        kill -KILL $pid 2>/dev/null
        wait $pid 2>/dev/null
      else
        wait $pid; st=$?
        print -u2 -- "$mode: claude did not start (the launcher exited $st first)"
      fi
      rc=1
      exec {fd}<&-; exec {crutch}>&-
      continue
    fi
    read -r info <&$fd
    cpid=${${info#* }%% *}
    [[ $mode == deleted ]] && rm -rf -- "${${info#* * }:h}"
    : > "$GO"
    exec {crutch}>&-
    # EOF arrives when every holder of the write end is gone - the launcher must
    # end with claude; the shared bound ends a run whose teardown hangs.
    if ! _ccm_test_wait_readable $fd $hang; then
      print -u2 -- "$mode: the launcher did not end with claude within ${CCMORPH_TEST_HANG_TIMEOUT}s"; rc=1
      kill -KILL $pid ${info%% *} $cpid 2>/dev/null   # exact pids: zsh -c, its subshell, the stub
    fi
    exec {fd}<&-
    wait $pid; st=$?
    (( st == 6 )) || { print -u2 -- "$mode: status $st, want claude's 6"; rc=1; }
    [[ ! -e "${${info#* * }:h}" ]] || { print -u2 -- "$mode: private dir left behind"; rc=1; }
  done
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_launcher_ends_with_claude

test_private_session_keeps_the_terminal_as_stdin() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 mode
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  printf '%s\n' '#!/bin/sh' 'if [ -t 0 ]; then echo tty; else echo notty; fi > "$OUT"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  # Run the launcher with a pty as stdin/stdout/stderr, as a terminal does,
  # through run_under_a_terminal, so its load-aware deadline, drain and group
  # kill apply (#421).
  for mode in private normal; do
    export OUT="$test_home/out.$mode"
    run_under_a_terminal "$test_home/pty.$mode" zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" ${3:+CCMORPH_PRIVATE=1} --' _ "$CCMORPH_SOURCE" "$prof" "${mode:#normal}"
    [[ -s "$OUT" && "$(<"$OUT")" == tty ]] || { print -u2 -- "$mode: claude's stdin is not the terminal"; rc=1; }
  done
  # A launch with stdin closed still starts claude.
  export OUT="$test_home/out.closed"
  zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" <&- 2>/dev/null
  [[ -s "$OUT" ]] || { print -u2 -- "closed stdin: claude did not start"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_keeps_the_terminal_as_stdin

test_private_session_claude_does_not_inherit_ignored_int_quit() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local prof="$test_home/prof" rc=0 mask
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  # Linux-only probe: SigIgn bit 1 = INT, bit 2 = QUIT.
  printf '%s\n' '#!/bin/sh' 'sed -n "s/^SigIgn:[[:space:]]*//p" /proc/self/status' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  if [[ -r /proc/self/status ]]; then
    mask=$(_ccm_run_claude "CLAUDE_CONFIG_DIR=$prof" CCMORPH_PRIVATE=1 --)
    (( (16#${mask:-0} & 6) == 0 )) || { print -u2 -- "SigIgn=$mask: INT/QUIT ignored"; rc=1; }
  fi
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_claude_does_not_inherit_ignored_int_quit

test_private_session_forwarding_traps_precede_the_fork() {
  # A TERM between the fork and the trap swap would run the dir-removing trap
  # while claude runs, so the forwarding traps must be installed before the fork.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local -a body; body=("${(@f)$(functions _ccm_fork_forward)}")
  rm -rf "$test_home"
  local p_fwd='*trap*kill -TERM $_ccm_pid*' p_fork='*} *&'
  local fwd=${body[(i)$~p_fwd]} fork=${body[(i)$~p_fork]}
  (( fork <= $#body )) || { print -u2 -- "no background fork of claude"; return 1; }
  (( fwd < fork )) || { print -u2 -- "TERM forwarding trap at line $fwd, after the fork at $fork"; return 1; }
}
run_test test_private_session_forwarding_traps_precede_the_fork

test_private_session_keeps_exit_status_after_interrupt() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0 pid st info fifo fd crutch
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  # The fake claude survives SIGINT (as Claude Code does), then exits 0 on its own.
  printf '%s\n' '#!/bin/sh' 'trap "" INT' 'printf "%s\n" "$PPID $$ $CLAUDE_CONFIG_DIR" >&3 || exit 9' 'sleep 1' 'exit 0' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  fifo="$test_home/started.INT"
  mkfifo "$fifo"
  # Opened RDWR before the fork (GH-434): neither open can block on a launcher
  # that never starts, and holding the handle across the readiness read keeps
  # a writer on the fifo, so a ready fd below can only mean claude's report,
  # never a premature EOF.
  exec {crutch}<>"$fifo"
  exec {fd}<"$fifo"
  zsh -c 'source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof" 3>"$fifo" &
  pid=$!
  # No budget, a backstop: claude's report, or a launcher that never produced
  # one - dead or wedged, killed and reported either way.
  if ! _ccm_test_wait_readable $fd $hang; then
    if kill -0 $pid 2>/dev/null; then
      print -u2 -- "INT: no report and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
      kill -KILL $pid 2>/dev/null
      wait $pid 2>/dev/null
    else
      wait $pid; st=$?
      print -u2 -- "INT: claude did not start (the launcher exited $st first)"
    fi
    rc=1
    exec {fd}<&-; exec {crutch}>&-
    rm -rf "$test_home"; return $rc
  fi
  read -r info <&$fd
  exec {fd}<&-
  exec {crutch}>&-
  kill -INT ${info%% *}
  wait $pid; st=$?
  (( st == 0 )) || { print -u2 -- "INT: status $st, want 0"; rc=1; }
  [[ ! -e "${${info#* * }:h}" ]] || { print -u2 -- "INT: private dir left behind"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_keeps_exit_status_after_interrupt

test_launcher_forwards_term_and_hup_to_claude() {
  # A foreground child defers the launcher's traps until it exits, so a TERM/HUP
  # sent to the top-level launcher pid (agent-dispatch, a user's kill) never
  # reached claude. The launcher must forward it and exit with claude's status.
  #
  # GH-398: neither wait is a deadline. A fresh zsh sourcing ccmorph.sh and
  # reaching claude is unbounded work on a loaded machine - 8.9 s of the old 10 s
  # budget at load 460, and 27-103 s launches were measured on this box - so no
  # number put here can be right as a budget. GH-434 still puts a bound on both
  # waits as a backstop: the fake adapter the launcher auto-starts inherits the
  # fifo's write end, and an adapter outliving its launcher held the pipe open
  # forever - the readiness read met no event at all and the landing batch hung
  # at zero CPU. The readiness read is bounded by CCMORPH_TEST_HANG_TIMEOUT
  # with an RDWR handle held across it, so a ready fd can only mean claude's pid
  # line, never a premature EOF; the bound fires only when the pid never came
  # (the launcher dead or wedged, killed either way). The termination wait keeps
  # waiting out the launcher's own teardown (0.5 s measured, against the whole
  # launch the readiness read above waits out).
  # GH-431 re-reported the same failure from 7ef40c5, a head that predates the
  # fix: reproduced at load 153-283, that shape lost both signals to "claude
  # did not start"; this shape passed TERM and HUP twice at load 339-353.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local rc=0 sig pid st cpid fifo fd crutch clock_waits=0
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  # Counted, never slept: a wait that polls the clock must fail this test on an
  # idle machine too, where before it could only fail under load - how #398 hid.
  sleep() { (( ++clock_waits )); }
  printf '%s\n' '#!/bin/sh' 'trap '\''echo got > "$SEEN"; exit 7'\'' TERM HUP' 'printf "%s\n" "$$" >&3' 'while :; do sleep 0.1; done' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  for sig in TERM HUP; do
    export SEEN="$test_home/seen.$sig"
    fifo="$test_home/started.$sig"
    mkfifo "$fifo"
    # Opened RDWR before the fork (GH-434): neither open can block on a peer
    # that never comes, and holding the handle across the readiness read keeps
    # a writer on the fifo, so a ready fd below can only mean claude's pid
    # line, never a premature EOF. Dropped before the termination wait, whose
    # EOF means every holder of the write end is gone: launcher, claude, and
    # the auto-started adapter that inherited fd 3.
    exec {crutch}<>"$fifo"
    exec {fd}<"$fifo"
    zsh -c 'source "$1"; _ccm_launch -a proxy prompt' _ "$CCMORPH_SOURCE" 3>"$fifo" &
    pid=$!
    # No budget, a backstop: claude's pid line, or a launcher that never
    # produced one - dead or wedged, killed and reported either way.
    if ! _ccm_test_wait_readable $fd $hang; then
      if kill -0 $pid 2>/dev/null; then
        print -u2 -- "$sig: no pid line and no exit within ${CCMORPH_TEST_HANG_TIMEOUT}s"
        kill -KILL $pid 2>/dev/null
        wait $pid 2>/dev/null
      else
        wait $pid; st=$?
        print -u2 -- "$sig: claude did not start (the launcher exited $st first)"
      fi
      rc=1
      exec {fd}<&-; exec {crutch}>&-
      continue
    fi
    read -r cpid <&$fd
    kill -$sig $pid
    exec {crutch}>&-
    if ! _ccm_test_wait_readable $fd $hang; then
      print -u2 -- "$sig: neither the launcher nor claude exited in ${CCMORPH_TEST_HANG_TIMEOUT}s"; rc=1
      kill -KILL $pid 2>/dev/null
      kill -KILL $cpid 2>/dev/null
    fi
    exec {fd}<&-
    wait $pid; st=$?
    kill -0 $cpid 2>/dev/null && { print -u2 -- "$sig: claude still running after the launcher exited"; rc=1; kill -KILL $cpid 2>/dev/null; }
    [[ -s "$SEEN" ]] || { print -u2 -- "$sig: claude never got the signal"; rc=1; }
    (( st == 7 )) || { print -u2 -- "$sig: status $st, want claude's 7"; rc=1; }
  done
  (( clock_waits == 0 )) || { print -u2 -- "GH-398: the waits used real-clock polling"; rc=1; }
  unfunction sleep
  rm -rf "$test_home"
  return $rc
}
run_test test_launcher_forwards_term_and_hup_to_claude

test_launcher_keeps_stdin_and_exit_status() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local rc=0 st
  printf '%s\n' '#!/bin/sh' 'read -r line; echo "$line" > "$OUT"; exit 5' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  export OUT="$test_home/out"
  print hello | zsh -c 'source "$1"; _ccm_launch -a proxy prompt' _ "$CCMORPH_SOURCE"; st=$?
  [[ -s "$OUT" && "$(<"$OUT")" == hello ]] || { print -u2 -- "claude did not get the caller's stdin"; rc=1; }
  (( st == 5 )) || { print -u2 -- "status $st, want claude's 5"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_launcher_keeps_stdin_and_exit_status

test_launch_runs_claude_when_tmpdir_is_unwritable() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local rc=0 st
  mkdir -p "$test_home/ro" && chmod 555 "$test_home/ro"
  printf '%s\n' '#!/bin/sh' 'echo ran > "$OUT"' 'exit 5' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  export OUT="$test_home/out"
  TMPDIR="$test_home/ro" zsh -c 'source "$1"; _ccm_run_claude --' _ "$CCMORPH_SOURCE" 2>/dev/null; st=$?
  [[ -s "$OUT" ]] || { print -u2 -- "claude did not start with an unwritable TMPDIR"; rc=1; }
  (( st == 5 )) || { print -u2 -- "status $st, want claude's 5"; rc=1; }
  chmod 755 "$test_home/ro"; rm -rf "$test_home"
  return $rc
}
run_test test_launch_runs_claude_when_tmpdir_is_unwritable

test_launch_keeps_claudes_niceness() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local rc=0 mine
  printf '%s\n' '#!/bin/sh' 'ps -o ni= -p $$ | tr -d " " > "$OUT"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  export OUT="$test_home/out"
  zsh -c 'source "$1"; ps -o ni= -p $$ | tr -d " " > "$OUT.me"; _ccm_launch -a proxy prompt' _ "$CCMORPH_SOURCE"
  mine=$(<"$OUT.me")
  [[ -s "$OUT" && "$(<"$OUT")" == "$mine" ]] || { print -u2 -- "claude nice $(<"$OUT"), launcher $mine"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_launch_keeps_claudes_niceness

test_signal_with_a_queued_hop_ends_the_launch() {
  # A TERM/HUP is the user ending the session: a hop queued before it must not
  # relaunch claude, and its relay file must not be left behind.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local rc=0 st pid cpid relay started crutch clock_waits=0
  local -i hang=$(( CCMORPH_TEST_HANG_TIMEOUT * 100 ))
  zmodload zsh/zselect || { rm -rf "$test_home"; return 1; }
  # GH-303: scheduling must not decide whether the signal contract passes.
  sleep() { (( ++clock_waits )); command sleep "$@"; }
  export READY="$test_home/ready"
  mkfifo "$test_home/started" "$READY.hold"
  printf '%s\n' '#!/bin/sh' 'trap "exit 9" TERM' \
    'exec 4<> "$READY.hold"' \
    'printf "profile=proxy\nsession=sess-1\n" > "$HOME/.config/ccmorph/.reswitch.$CCMORPH_RELAY_ID"' \
    'if [ -s "$READY" ]; then echo "$CCMORPH_RELAY_ID" >> "$READY"; exit 6; fi' \
    'echo "$CCMORPH_RELAY_ID" >> "$READY"' \
    'printf "%s %s\n" "$CCMORPH_RELAY_ID" "$$" >&3 || exit 6' \
    'read -r line <&4' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  # Claude publishes readiness only after its trap and queued relay exist.
  # Opened RDWR before the fork (GH-434): neither open can block on a peer
  # that never comes, and holding the handle across the readiness read keeps
  # a writer on the fifo, so a ready fd below can only mean claude's pid
  # line, never a premature EOF. Dropped before the termination wait, whose
  # EOF still means everyone holding the write end is gone.
  exec {crutch}<>"$test_home/started"
  exec {started}< "$test_home/started"
  zsh -c 'source "$1"; _ccm_launch -a proxy prompt' _ "$CCMORPH_SOURCE" 3>"$test_home/started" 2>/dev/null &
  pid=$!
  # The hang bound as a backstop (GH-434), not a readiness deadline: the pid
  # line, or a launcher that never produced one, killed either way.
  if ! _ccm_test_wait_readable $started $hang; then
    print -u2 -- "claude's pid line never arrived within ${CCMORPH_TEST_HANG_TIMEOUT}s"
    kill -KILL $pid 2>/dev/null
    wait $pid 2>/dev/null
    exec {started}<&-; exec {crutch}>&-
    unfunction sleep
    rm -rf "$test_home"
    return 1
  fi
  read -r relay cpid <&$started || { print -u2 -- "claude did not start"; rc=1; }
  kill -TERM $pid 2>/dev/null
  exec {crutch}>&-
  # Keep the original five-second termination backstop, not a readiness deadline.
  # EOF wakes immediately when launcher and child close their inherited writers.
  if ! _ccm_test_wait_readable $started 500; then
    kill -0 $pid 2>/dev/null && { print -u2 -- "launcher still running"; rc=1; kill -KILL $pid 2>/dev/null; }
    [[ -n $cpid ]] && kill -KILL $cpid 2>/dev/null
  fi
  exec {started}<&-
  wait $pid; st=$?
  (( st == 9 )) || { print -u2 -- "status $st, want claude's 9"; rc=1; }
  [[ -s "$READY" ]] && (( $(wc -l < "$READY") == 1 )) || { print -u2 -- "claude was relaunched or did not start"; rc=1; }
  [[ -n $relay && ! -e "$HOME/.config/ccmorph/.reswitch.$relay" ]] || { print -u2 -- "relay file left behind"; rc=1; }
  (( clock_waits == 0 )) || { print -u2 -- "GH-303: signal synchronization used real-clock polling"; rc=1; }
  unfunction sleep
  rm -rf "$test_home"
  return $rc
}
run_test test_signal_with_a_queued_hop_ends_the_launch

test_private_session_runs_the_binary_not_a_shell_function() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local prof="$test_home/prof" rc=0
  mkdir -p "$prof" && print '{}' > "$prof/.claude.json"
  printf '%s\n' '#!/bin/sh' 'echo BINARY-RAN > "$OUT"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  export OUT="$test_home/out"
  zsh -c 'claude() { print FUNCTION-RAN > "$OUT"; }; source "$1"; _ccm_run_claude "CLAUDE_CONFIG_DIR=$2" CCMORPH_PRIVATE=1 --' _ "$CCMORPH_SOURCE" "$prof"
  [[ -s "$OUT" && "$(<"$OUT")" == BINARY-RAN ]] || { print -u2 -- "ran: $(<"$OUT")"; rc=1; }
  rm -rf "$test_home"
  return $rc
}
run_test test_private_session_runs_the_binary_not_a_shell_function

test_model_prompt_survives_run_separators() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  write_profile_context proxy claude-opus-5=1000000
  source "$CCMORPH_SOURCE"
  local rc=0 actual tab=$'\t' separator
  for separator in yes no; do
    : > "$CCMORPH_TEST_LOG"
    local -a flags=(--model mine)
    [[ "$separator" == yes ]] && flags+=(--)
    ccmorph run proxy "${flags[@]}" -p 'reply ok' || rc=1
    actual=$(<"$CCMORPH_TEST_LOG")
    assert_contains "$actual" "${tab}-p${tab}reply ok" "$separator preserves the prompt" || rc=1
    assert_not_contains "$actual" "${tab}--${tab}" "$separator consumes run's separator" || rc=1
    assert_contains "$actual" "${tab}--model${tab}mine" 'explicit model stays untouched' || rc=1
  done
  : > "$CCMORPH_TEST_LOG"
  ccmorph run proxy --model mine -- -- -p 'literal prompt' || rc=1
  actual=$(<"$CCMORPH_TEST_LOG")
  assert_contains "$actual" "${tab}--${tab}-p${tab}literal prompt" 'a second separator belongs to Claude' || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_model_prompt_survives_run_separators



# Sourcing the library and launching must leave this shell's stderr alone: it once
# went to /dev/null for good, hiding every FAIL line and the summary (#191).
test_fork_forward_leaves_the_callers_stderr_intact() {
  local scratch out
  scratch=$(mktemp -d)
  # The child gets the real library and a clean HOME: inheriting this shell's
  # CCMORPH_LIB_DIR (a deleted scratch home of an earlier test) once made the
  # source fail, so _ccm_fork_forward never ran and the checks below passed
  # vacuously (#207). The whence line proves the function really loaded.
  out=$(zsh -c '
    export HOME="$2" CCMORPH_LIB_DIR="${1:A:h}" CCMORPH_STATE_DIR="$2/.config/ccmorph"
    source "$1" || exit $?
    whence -w _ccm_fork_forward
    _ccm_fork_forward true
    print -u2 -- STDERR_SURVIVES
  ' _ "$CCMORPH_SOURCE" "$scratch" 2>&1)
  rm -rf "$scratch"
  assert_contains "$out" '_ccm_fork_forward: function' \
    'the library must load, or the stderr check below is vacuous' || return 1
  assert_contains "$out" 'STDERR_SURVIVES' 'stderr must survive a launch through _ccm_fork_forward'
}
run_test test_fork_forward_leaves_the_callers_stderr_intact

# #200: a runner that is itself a ccmorph-launched Claude session exports its
# credentials and tuning knobs, and they used to reach the fake claude -- the
# `claude --help` probe alone logged the live API key into failure dumps, and
# the runner's CLAUDE_CONFIG_DIR aimed that same probe at the session's real
# settings file. The canary greps its log rather than asserting on the text, so
# a failure can never echo a real credential back into the suite output.
test_env_scrub_canary() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  local rc=0
  source "$CCMORPH_SOURCE"
  ccmorph run proxy prompt || rc=1
  if command grep -qE 'leak-sentinel|leak-model|leak-advise|leak-settings' "$CCMORPH_TEST_LOG"; then
    print -u2 -- 'the runner session env reached the fake claude'
    rc=1
  fi
  rm -rf "$test_home"
  return $rc
}

test_the_runners_session_env_never_reaches_a_fixture() {
  # Re-run this suite in a child with a sentinel export for each scrubbed
  # family (ANTHROPIC_*, CLAUDE*, CCMORPH_*), filtered down to the canary
  # above: the suite-start scrub is the only thing between the runner's env
  # and that launch, so this fails if the scrub is lost or narrowed.
  #
  # CLAUDE_CONFIG_DIR carries a sentinel too, in a synthetic settings file: it
  # is the one export whose damage is not its own value but the settings.json
  # the fake claude's `--help` probe then reads and logs, so a runner file
  # whose model text collides with an assertion elsewhere in the suite dumps
  # its content into the failure output (see #200). Synthetic content only,
  # never the runner's own config.
  local out st rc=0
  local cfg; cfg=$(mktemp -d)
  print -r -- '{"model":"leak-settings-model"}' > "$cfg/settings.json"
  # The wrapper can run on any parent shard. Its child selects one test, so
  # give it a single shard rather than inheriting a shard that skips the canary.
  out=$(ANTHROPIC_API_KEY=leak-sentinel CLAUDE_CODE_SUBAGENT_MODEL=leak-model \
    CCMORPH_ADVISE=leak-advise CLAUDE_CONFIG_DIR="$cfg" \
    CCMORPH_TEST_FILTER=test_env_scrub_canary \
    CCMORPH_TEST_SHARD=0/1 CCMORPH_TEST_JOBS=1 \
    zsh "$REPO_ROOT/tests/ccmorph_test.zsh" 2>&1)
  st=$?
  rm -rf "$cfg"
  (( st == 0 )) || { print -u2 -- "the scrubbed child suite failed with status $st"; rc=1; }
  assert_contains "$out" 'PASS: test_env_scrub_canary' \
    'the canary must run and pass with sentinels in the runner env' || rc=1
  assert_not_contains "$out" 'leak-' 'no sentinel may reach the suite output' || rc=1
  return $rc
}
run_test test_env_scrub_canary
run_test test_the_runners_session_env_never_reaches_a_fixture

# GH-434: a launcher killed outright runs no trap, so the fake's only reap is
# the --parent-pid poll -- and a pid is recycled, so the parent it names can be
# a live process long after the launch is gone. Reparented to PID 1 under a
# parent that never dies, the fake served forever, holding the launcher's fifo
# write end open while a test's readiness read waited for an EOF that never
# came, at zero CPU. The starter here exits at once, so the tail is orphaned,
# and the bystander is the recycled pid the tail keeps trusting.
test_the_serve_tail_leaves_when_its_own_launcher_is_gone() {
  local test_home; test_home=$(mktemp -d)
  local rc=0 serve_pid bystander
  /bin/sleep 300 & bystander=$!
  /bin/sh -c 'sh "$1" gw --parent-pid "$2" &
echo $! > "$3"' _ "$SCRIPT_DIR/fake_adapter_serve.sh" "$bystander" "$test_home/serve.pid" 2>/dev/null
  serve_pid=$(<"$test_home/serve.pid")
  local -i i=0
  while kill -0 "$serve_pid" 2>/dev/null; do
    (( ++i < 100 )) || break
    sleep 0.1
  done
  if kill -0 "$serve_pid" 2>/dev/null; then
    print -u2 -- "the serve tail outlived its orphaned launcher for 10s while --parent-pid named a live pid"
    kill -KILL "$serve_pid" 2>/dev/null
    rc=1
  fi
  kill -KILL "$bystander" 2>/dev/null
  wait "$bystander" 2>/dev/null
  rm -rf "$test_home"
  return $rc
}
run_test test_the_serve_tail_leaves_when_its_own_launcher_is_gone

# GH-434, the other half: the readiness read itself. The launcher's fd 3 is
# the fifo claude publishes its pid on, and the adapter the launcher
# auto-starts inherits that write end, so a read with no bound can meet no
# event at all -- an adapter outliving its launcher held the pipe open and
# the shard sat at zero CPU in the landing batch. Every test that opens the
# launcher's fifo must open it through an RDWR handle (so the open cannot
# block on a launcher that never starts) and put a bounded wait -- a zselect
# or the _ccm_test_wait_readable slices -- before the read (so waiting for
# the pid line is hang-bounded, not eventless).
test_the_launcher_fifo_reads_are_bounded() {
  local self="$REPO_ROOT/tests/ccmorph_test.zsh" rc=0
  # Counted, not merely found: a third launcher fifo must extend this list,
  # not slip past it.
  local -a sites=(test_private_session_is_removed_when_terminated \
                  test_private_session_is_removed_on_group_quit \
                  test_private_session_forwards_term_and_keeps_claudes_status \
                  test_private_session_keeps_exit_status_after_stop_and_continue \
                  test_private_session_launcher_ends_with_claude \
                  test_private_session_keeps_exit_status_after_interrupt \
                  test_launcher_forwards_term_and_hup_to_claude \
                  test_signal_with_a_queued_hop_ends_the_launch)
  local launchers=$(( $(grep -c -- '3>''"' "$self") ))
  (( launchers == $#sites )) || {
    print -u2 -- "FAIL: $launchers launcher fifo opens for $#sites bounded sites"
    rc=1
  }
  local site body first_read first_guard
  for site in "${sites[@]}"; do
    body=$(sed -n "/^${site}()/,/^run_test ${site}/p" "$self")
    [[ -n "$body" ]] || { print -u2 -- "FAIL: $site not found in the suite"; rc=1; continue }
    grep -q -- 'exec {crutch}<>"' <<<"$body" || {
      print -u2 -- "FAIL: $site opens the launcher fifo without an RDWR handle"
      rc=1
    }
    grep -q -- 'exec {crutch}>&-' <<<"$body" || {
      print -u2 -- "FAIL: $site never drops its RDWR handle, so no EOF could reach the termination wait"
      rc=1
    }
    first_read=$(grep -nE -- 'read -r (cpid|relay cpid|info) <&' <<<"$body" | head -1 | cut -d: -f1)
    first_guard=$(grep -nE -- '(zselect -r|_ccm_test_wait_readable)' <<<"$body" | head -1 | cut -d: -f1)
    [[ -n "$first_read" && -n "$first_guard" && "$first_guard" -lt "$first_read" ]] || {
      print -u2 -- "FAIL: $site reads the launcher fifo before any zselect bound"
      rc=1
    }
  done
  return $rc
}
run_test test_the_launcher_fifo_reads_are_bounded

# GH-434: nothing failed a test that leaked a child -- the leak only showed up
# as copies of this suite stuck at zero CPU long after the landing batch that
# started them was killed. The canary is a test that deliberately leaks one;
# the wrapper asserts the suite failed that test and that no child outlived it.
test_leak_canary() {
  # CCMORPH_TEST_LEAK_CANARY names the pidfile a leaked child writes; without
  # it this is an ordinary passing test, so the suite's own run costs nothing.
  [[ -n "${CCMORPH_TEST_LEAK_CANARY:-}" ]] || return 0
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  # The leaker ignores every catchable signal: a stray TERM landing on it once
  # emptied the sweep's nets mid-canary and this test passed vacuously, with
  # nothing in the output to say why. With the traps set only the sweep's KILL
  # escalation can end it, so the child the sweep is judged on is one nothing
  # else can take away.
  local armed="$test_home/armed"
  printf '%s\n' '#!/bin/sh' \
    "trap '' TERM HUP INT" \
    ": > '$armed'" \
    'while :; do /bin/sleep 0.2; done' > "$HOME/bin/leaker"
  chmod +x "$HOME/bin/leaker"
  # Disowned with its output thrown away, the way the adapter is started: a
  # child the suite will not clean up on exit, which is the leak class that
  # stranded the landing batch.  The redirect matters twice: a leaked child
  # must not hold this suite's stdout pipe, or the run that wraps us in a
  # command substitution would hang on our open descriptor.
  "$HOME/bin/leaker" >/dev/null 2>&1 &!
  local pid=$!
  print -r -- $pid > "${CCMORPH_TEST_LEAK_CANARY:?}"
  # Wait until the traps are provably armed rather than sleeping a fixed
  # interval and hoping: under fleet load a fresh sh can take arbitrarily
  # long to reach its second line, and the self-test TERM below would then
  # kill an unarmed leaker and fail the suite for nothing. The marker also
  # proves the script itself is open, so the home can go away while the child
  # runs, the way a real leaked adapter outlives the test home that started it.
  local -i i=0
  while [[ ! -e $armed ]]; do
    (( ++i < 200 )) || {
      print -u2 -- "the leaked child $pid never armed its traps; the canary is broken, not the sweep"
      kill -KILL "$pid" 2>/dev/null
      rm -rf "$test_home"
      return 1
    }
    command sleep 0.05
  done
  rm -rf "$test_home"
  # Prove the leaker is beyond any stray signal's reach before judging the
  # sweep on catching it: TERM it ourselves and demand it survive. With the
  # traps armed only the sweep's KILL escalation can end it -- a stray TERM
  # once killed an unarmed leaker mid-canary, both sweep nets saw nothing,
  # and this test passed vacuously, failing a landing batch for nothing. A
  # child that dies here breaks the canary's premise, not the sweep, so say
  # which one gave out. The sweep skips dead children and zombies by design;
  # a zombie is a dead child that has not been waited for yet.
  kill -TERM "$pid" 2>/dev/null
  command sleep 0.2
  if ! kill -0 "$pid" 2>/dev/null ||
     [[ "$(ps -o stat= -p "$pid" 2>/dev/null)" == Z* ]]; then
    print -u2 -- "the leaked child $pid did not survive a TERM the canary sent it; the canary is broken, not the sweep"
    return 1
  fi
  return 0
}

test_a_test_that_leaks_a_child_is_failed_and_reaped() {
  local out st rc=0 pid
  local marker; marker=$(mktemp -d)
  out=$(CCMORPH_TEST_LEAK_CANARY="$marker/leaker.pid" \
    CCMORPH_TEST_FILTER=test_leak_canary \
    CCMORPH_TEST_SHARD=0/1 CCMORPH_TEST_JOBS=1 \
    zsh "$REPO_ROOT/tests/ccmorph_test.zsh" 2>&1)
  st=$?
  (( st != 0 )) || { print -u2 -- 'the canary passed: a leaked child did not fail its test'; rc=1 }
  assert_contains "$out" 'FAIL: test_leak_canary' \
    'the canary must run and fail' || rc=1
  assert_contains "$out" 'outlived its test' \
    'the sweep itself must catch the leaked child' || rc=1
  pid=$(<"$marker/leaker.pid" 2>/dev/null)
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    print -u2 -- "the leaked child $pid outlived the suite"
    kill -KILL "$pid" 2>/dev/null
    rc=1
  fi
  rm -rf "$marker"
  return $rc
}
run_test test_leak_canary
run_test test_a_test_that_leaks_a_child_is_failed_and_reaped

test_profile_history_is_its_own_and_a_shared_link_heals() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 dir="$HOME/.config/ccmorph/profiles/proxy"
  mkdir -p "$HOME/.claude/projects/p" "$dir"
  print -r -- old > "$HOME/.claude/projects/p/native.jsonl"
  ln -sn "$HOME/.claude/projects" "$dir/projects"
  _ccm_read proxy && _ccm_setup_dir proxy || rc=1
  unset _ccm
  # The shared-history link goes; the native history it pointed at stays whole.
  [[ -d "$dir/projects" && ! -L "$dir/projects" ]] || { print -u2 'profile history must be a real dir'; rc=1; }
  [[ "$(<"$HOME/.claude/projects/p/native.jsonl")" == old ]] || { print -u2 'native history must survive'; rc=1; }
  _ccm_dir_ok proxy || { print -u2 'a healed profile must pass the fast path'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_removing_a_profile_keeps_its_history_resumable() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 dir="$HOME/.config/ccmorph/profiles/gone"
  mkdir -p "$dir/projects/p/s-1/subagents"
  print -r -- kept > "$dir/projects/p/s-1.jsonl"
  print -r -- sub > "$dir/projects/p/s-1/subagents/a.jsonl"
  _ccm_remove_profile_dir gone || rc=1
  [[ ! -e "$dir" ]] || { print -u2 'the profile dir must be removed'; rc=1; }
  [[ "$(<"$HOME/.claude/projects/p/s-1.jsonl")" == kept && -f "$HOME/.claude/projects/p/s-1/subagents/a.jsonl" ]] ||
    { print -u2 'its history must move to native history, not be deleted'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_removing_profiles_never_loses_a_carried_transcript() {
  # Review of #464: A owns S, B carried it; rm B then rm A must leave S whole.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 P="$HOME/.config/ccmorph/profiles"
  mkdir -p "$P/a/projects/p" "$P/b/projects"
  print -r -- story > "$P/a/projects/p/S.jsonl"
  _ccm_carry_session S "$P/b/projects" || rc=1
  _ccm_remove_profile_dir b || rc=1
  _ccm_remove_profile_dir a || rc=1
  [[ -f "$HOME/.claude/projects/p/S.jsonl" && ! -L "$HOME/.claude/projects/p/S.jsonl" &&
     "$(<"$HOME/.claude/projects/p/S.jsonl")" == story ]] ||
    { print -u2 'the transcript must end up as a real file in native history'; rc=1; }
  # GH-466: a no-profile resume carries a session into NATIVE history, so the
  # sidecar dir arrives as a link on the parent of its files' destinations. rm
  # must not mistake that for a collision and keep the profile forever.
  mkdir -p "$P/d/projects/p/T/subagents"
  print -r -- tail > "$P/d/projects/p/T.jsonl"
  print -r -- sub > "$P/d/projects/p/T/subagents/x.jsonl"
  _ccm_carry_session T "$HOME/.claude/projects" || rc=1
  _ccm_remove_profile_dir d || rc=1
  [[ -f "$HOME/.claude/projects/p/T.jsonl" && ! -L "$HOME/.claude/projects/p/T.jsonl" &&
     "$(<"$HOME/.claude/projects/p/T.jsonl")" == tail &&
     -f "$HOME/.claude/projects/p/T/subagents/x.jsonl" && ! -L "$HOME/.claude/projects/p/T" &&
     "$(<"$HOME/.claude/projects/p/T/subagents/x.jsonl")" == sub ]] ||
    { print -u2 'a session carried into native history must not block rm or lose its sidecar'; rc=1; }
  # A real collision keeps the profile instead of deleting its copy.
  mkdir -p "$P/c/projects/p" && print -r -- other > "$P/c/projects/p/S.jsonl"
  _ccm_remove_profile_dir c 2>/dev/null && { print -u2 'a colliding history must keep the profile'; rc=1; }
  [[ "$(<"$P/c/projects/p/S.jsonl")" == other ]] || rc=1
  rm -rf "$test_home"
  return $rc
}

test_native_resume_carries_into_native_history_even_inside_a_profile() {
  # Review of #464: a no-profile launch runs claude on ~/.claude whatever
  # CLAUDE_CONFIG_DIR the calling shell has.
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 P="$HOME/.config/ccmorph/profiles"
  mkdir -p "$P/c/projects/p"
  print -r -- t > "$P/c/projects/p/T.jsonl"
  printf '%s\n' '#!/bin/sh' 'test -f "$HOME/.claude/projects/p/T.jsonl" && echo FOUND >> "$CCMORPH_TEST_LOG"' > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  CLAUDE_CONFIG_DIR="$P/c" _ccm_launch --resume T >/dev/null 2>&1
  [[ -f "$CCMORPH_TEST_LOG" && "$(<"$CCMORPH_TEST_LOG")" == *FOUND* ]] ||
    { print -u2 'a native resume must find the session in native history'; rc=1; }
  rm -rf "$test_home"
  return $rc
}

test_hop_carries_the_one_session_into_the_target_history() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  printf '%s\n' 'type=proxy' 'base_url=http://target.invalid' 'api_key=target-key' 'model=target-model' \
    > "$HOME/.config/ccmorph/target.conf"
  printf '%s\n' '#!/bin/sh' 'printf 200' > "$HOME/bin/curl"
  chmod +x "$HOME/bin/curl"
  # First run (proxy) writes the session and asks for a hop; the second (target)
  # appends to whatever its own history holds under that id.
  printf '%s\n' \
    '#!/bin/sh' \
    'h="$CLAUDE_CONFIG_DIR/projects/p"' \
    'if [ ! -f "$HOME/relay-triggered" ]; then' \
    '  : > "$HOME/relay-triggered"' \
    '  mkdir -p "$h/hop-session/subagents" && echo first > "$h/hop-session.jsonl"' \
    '  printf "profile=target\nsession=hop-session\n" > "$HOME/.config/ccmorph/.reswitch.$CCMORPH_RELAY_ID"' \
    'elif [ -f "$h/hop-session.jsonl" ]; then echo second >> "$h/hop-session.jsonl"' \
    'else echo MISSING >> "$CCMORPH_TEST_LOG"; fi' \
    > "$HOME/bin/claude"
  chmod +x "$HOME/bin/claude"
  source "$CCMORPH_SOURCE"
  local rc=0 src="$HOME/.config/ccmorph/profiles/proxy/projects/p" dst="$HOME/.config/ccmorph/profiles/target/projects/p"
  ccmorph run proxy >/dev/null 2>&1 || { print -u2 'the hop must succeed'; rc=1; }
  [[ "$(<"$src/hop-session.jsonl")" == $'first\nsecond' ]] || { print -u2 'the target must append to the one transcript'; rc=1; }
  [[ -L "$dst/hop-session.jsonl" && -L "$dst/hop-session" ]] || { print -u2 'the transcript and its sidecar dir must be linked, not copied'; rc=1; }
  [[ -z "$(command ls -A "$HOME/.claude/projects")" ]] || { print -u2 'profile sessions must not land in native history'; rc=1; }
  [[ ! -f "$CCMORPH_TEST_LOG" || "$(<"$CCMORPH_TEST_LOG")" != *MISSING* ]] || rc=1
  rm -rf "$test_home"
  return $rc
}

test_resume_by_id_reaches_another_profiles_session() {
  local test_home; test_home=$(mktemp -d); setup_home "$test_home"
  source "$CCMORPH_SOURCE"
  local rc=0 other="$HOME/.config/ccmorph/profiles/other/projects/p"
  mkdir -p "$other"
  printf '%s\n' '{"type":"assistant","isSidechain":false,"message":{"role":"assistant","model":"m1","content":[]}}' > "$other/sid-1.jsonl"
  _ccm_resume_model sid-1 && [[ "$REPLY" == m1 ]] || { print -u2 'a session in any profile history must be found'; rc=1; }
  _ccm_carry_session sid-1 "$HOME/.claude/projects" || rc=1
  [[ "$HOME/.claude/projects/p/sid-1.jsonl" -ef "$other/sid-1.jsonl" ]] || { print -u2 'resume must link the session in'; rc=1; }
  _ccm_carry_session '../x' "$HOME/.claude/projects" && { print -u2 'a non-id must be refused'; rc=1; }
  _ccm_carry_session nowhere "$HOME/.claude/projects" && rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_profile_history_is_its_own_and_a_shared_link_heals
run_test test_removing_a_profile_keeps_its_history_resumable
run_test test_removing_profiles_never_loses_a_carried_transcript
run_test test_native_resume_carries_into_native_history_even_inside_a_profile
run_test test_hop_carries_the_one_session_into_the_target_history
run_test test_resume_by_id_reaches_another_profiles_session

# Launched only from the terminal test below: a native subscription profile.
test_a_sub_profile_launch_for_the_terminal_check() {
  local test_home
  test_home=$(mktemp -d)
  setup_home "$test_home"
  fake_claude_help
  printf '%s\n' 'type=sub' 'email=test@example.invalid' 'model=sub-model' \
    > "$HOME/.config/ccmorph/subwith.conf"
  source "$CCMORPH_SOURCE"
  _ccm_setup_dir() { return 0; }
  _ccm_repair_projects_link() { return 0; }
  local rc=0
  _ccm_launch -a subwith -p x || rc=1
  rm -rf "$test_home"
  return $rc
}

# A subscription profile has no gateway, so a terminal launch must not claim one
# is ready (it did for every profile).
test_a_sub_profile_never_announces_a_gateway_on_a_terminal() {
  local test_home rc=0
  test_home=$(mktemp -d)
  local out="$test_home/terminal.log"
  python3 -c 'import os, pty; m, s = pty.openpty(); os.close(m); os.close(s)' 2>/dev/null || {
    print -u2 -- 'note: no pty here; skipping the terminal-only check'
    rm -rf "$test_home"; return 0
  }
  run_under_a_terminal "$out" env \
    CCMORPH_TEST_FILTER=test_a_sub_profile_launch_for_the_terminal_check \
    CCMORPH_TEST_SHARD=0/1 CCMORPH_TEST_JOBS=1 zsh "$SCRIPT_DIR/ccmorph_test.zsh" || rc=1
  assert_contains "$(<"$out")" 'All 1 tests passed' \
    'the sub-profile launch must complete under a terminal' || rc=1
  assert_not_contains "$(<"$out")" 'Gateway ready' \
    'a subscription profile must not announce a gateway' || rc=1
  rm -rf "$test_home"
  return $rc
}
run_test test_a_sub_profile_launch_for_the_terminal_check
run_test test_a_sub_profile_never_announces_a_gateway_on_a_terminal

if (( tests_failed > 0 )); then
  print -u2 -- "$tests_failed of $tests_run tests failed"
  exit 1
fi

(( tests_run == 0 )) || print -- "All $tests_run tests passed"

