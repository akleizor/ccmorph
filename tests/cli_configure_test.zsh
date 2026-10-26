#!/bin/zsh
# `get`/`set`/`configure` against the conf-owned defaults contract: ccproteus
# reads native settings once to seed, then owns model/effort in its own conf and
# never writes settings.json again.
setopt nounset pipefail
readonly ROOT="${0:A:h:h}"
readonly TMP="${TMPDIR:-/tmp}/ccproteus-cli-configure.$$"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/state/profiles/demo" "$TMP/home/.agents/runtime/policies"
print -r -- '{"schemaVersion":1,"autoCompactAtPercent":80}' > "$TMP/home/.agents/runtime/policies/context.json"
cp "$ROOT/model-window" "$TMP/model-window"
cp "$ROOT/ccproteus.sh" "$TMP/ccproteus.sh"
cp "$ROOT"/ccproteus-*.zsh "$TMP/"
export HOME="$TMP/home" CCPROTEUS_STATE_DIR="$TMP/state" CCPROTEUS_LIB_DIR="$TMP"
readonly CONF="$TMP/state/demo.conf"
readonly SETTINGS="$TMP/state/profiles/demo/settings.json"
print -r -- 'type=apikey
api_key=old-secret
base_url=https://old.example
adapter=true
tag=keep-me' > "$CONF"
print -r -- '{"model":"old-model","effortLevel":"low","modelSettings":{"custom":{"effortLevel":"high"}},"other":{"keep":true}}' > "$SETTINGS"
source "$ROOT/ccproteus.sh"

fail() { print -u2 -- "FAIL: $1"; exit 1; }
assert_eq() { [[ "$1" == "$2" ]] || fail "$3 (got ${(qqq)1}, expected ${(qqq)2})"; }
conf_field() { command sed -n "s|^$1=||p" "$CONF" | command head -1; }

# The native values are what a pre-upgrade install already had; the first read
# reports them without writing anything.
native=$(<"$SETTINGS")
before_conf=$(<"$CONF")
assert_eq "$(ccproteus get demo model)" old-model 'get model seeds from native'
assert_eq "$(ccproteus get demo effort)" low 'get effort seeds from native'
assert_eq "$(<"$CONF")" "$before_conf" 'get did not write the conf'
assert_eq "$(<"$SETTINGS")" "$native" 'get did not write settings'

ccproteus set demo model new-model >/dev/null || fail 'set model'
ccproteus set demo effort high >/dev/null || fail 'set effort'
assert_eq "$(ccproteus get demo model)" new-model 'set model persisted'
assert_eq "$(ccproteus get demo effort)" high 'set effort persisted'
assert_eq "$(conf_field model)" new-model 'model saved to the conf'
assert_eq "$(conf_field effort)" high 'effort saved to the conf'
assert_eq "$(conf_field model_defaults)" 1 'defaults marker written'
assert_eq "$(conf_field 'effort\.custom')" high 'per-model effort captured on seed'
assert_eq "$(<"$SETTINGS")" "$native" 'set left native settings untouched'

ccproteus set demo model >/dev/null 2>&1 && fail 'set accepted missing value'
ccproteus set demo model -i >/dev/null 2>&1 && fail 'set accepted interactive flag'
ccproteus get demo model extra >/dev/null 2>&1 && fail 'get accepted extra argument'
ccproteus set demo effort invalid >/dev/null 2>&1 && fail 'set accepted invalid effort'
output=$(ccproteus configure demo </dev/null 2>&1) && fail 'removed configure alias was accepted'
[[ "$output" == *"unknown command 'configure'"* ]] || fail 'configure must be an unknown command'

# An inline /model or /effort during a session must not become the default.
print -r -- '{"model":"inline-picked","effortLevel":"xhigh","other":{"keep":true}}' > "$SETTINGS"
assert_eq "$(ccproteus get demo model)" new-model 'inline native model ignored'
assert_eq "$(ccproteus get demo effort)" high 'inline native effort ignored'
print -r -- "$native" > "$SETTINGS"

before_conf=$(<"$CONF")
before_settings=$(<"$SETTINGS")
printf '\n\n' | CCPROTEUS_CONTEXT_LOCKED=demo ccproteus config demo >/dev/null 2>&1 && fail 'cancel configure succeeded'
assert_eq "$(<"$CONF")" "$before_conf" 'cancel preserved conf'
assert_eq "$(<"$SETTINGS")" "$before_settings" 'cancel preserved settings'

output=$(printf 'https://new.example\ny\nnew-secret\nconfigured-model\nmedium\n' | ccproteus config demo 2>&1) || fail "configure failed: $output"
[[ "$output" != *new-secret* ]] || fail 'credential leaked in configure output'
grep -q '^base_url=https://new.example$' "$CONF" || fail 'endpoint not saved'
grep -q '^api_key=new-secret$' "$CONF" || fail 'credential not saved'
grep -q '^tag=keep-me$' "$CONF" || fail 'unrelated conf key lost'
grep -q '^adapter=true$' "$CONF" || fail 'adapter flag lost'
assert_eq "$(ccproteus get demo model)" configured-model 'configured model'
assert_eq "$(ccproteus get demo effort)" medium 'configured effort'
assert_eq "$(<"$SETTINGS")" "$before_settings" 'configure left native settings untouched'

before_conf=$(<"$CONF")
before_settings=$(<"$SETTINGS")
if printf 'not-an-endpoint\n' | CCPROTEUS_CONTEXT_LOCKED=demo ccproteus config demo >/dev/null 2>&1; then fail 'invalid endpoint accepted'; fi
assert_eq "$(<"$CONF")" "$before_conf" 'invalid input preserved conf'
assert_eq "$(<"$SETTINGS")" "$before_settings" 'invalid input preserved settings'

# A profile marked as seeded with no defaults keeps none: native is no longer read.
print -r -- 'type=apikey
api_key=k
model_defaults=1' > "$CONF"
print -r -- '{"model":"native-model","effortLevel":"high"}' > "$SETTINGS"
before_conf=$(<"$CONF")
assert_eq "$(ccproteus get demo model)" '' 'absent model stays absent'
assert_eq "$(ccproteus get demo effort)" '' 'absent effort stays absent'
assert_eq "$(<"$CONF")" "$before_conf" 'reading an unset default wrote nothing'

print 'PASS cli_configure_test'
