#!/usr/bin/env bash
# The merge gate's repro for #835, kept as a test: with a credential-bearing
# endpoint URL and a listing HTTP 403, add saves the profile and no line of
# its stdout names the URL password (INTENT promise 5). The same assertions
# live in the pwurl leg of tests/ccmorph_test.zsh's #254 add test; this script
# is the gate's own command, exit 0 meaning fixed. Needs Go and `make shim`.
set -eu
cd "$(dirname "$0")/../.."
d=$(mktemp -d "${TMPDIR:-/tmp}/ccmorph-url.XXXXXX")
GOCACHE="${GOCACHE:-$TMPDIR/ccmorph-evaluator-gocache}" make shim
mkdir "$d/bin"
printf '%s\n' '#!/bin/sh' 'for a do case "$a" in */v1/models*) printf "__CCMORPH_STATUS__403\n"; exit 0;; esac; done' 'exit 1' > "$d/bin/curl"
chmod +x "$d/bin/curl"
p='ccmorph-url-canary-835'
u="http://user:${p}"'@gateway.invalid'
printf 'apikey\ncustom\n%s\nsk-not-a-real-key\n' "$u" | env -i HOME="$d" TMPDIR="$d" PATH="$d/bin:$PATH" CCMORPH_LIB_DIR="$PWD/lib" bin/ccmorph add gateway > "$d/out" 2> "$d/err"
test -f "$d/.config/ccmorph/gateway.conf"
if [[ $(<"$d/out") == *"$p"* ]]; then printf '%s\n' 'Synthetic URL password reached stdout'; exit 1; fi
printf 'add_stdout_password: PASS\n'
