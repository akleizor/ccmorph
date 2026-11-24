#!/usr/bin/env bash
# The merge gate's repro for #835, kept as a test: a listing 404 or 405 saves
# the profile unchecked and its notice names the refused listing (the first
# acceptance criterion). The same assertions live in the 403/404/405 loop of
# tests/ccmorph_test.zsh's #254 add test; this script is the gate's own
# command, exit 0 meaning fixed. Needs Go and `make shim`.
set -eu
cd "$(dirname "$0")/../.."
d=$(mktemp -d "${TMPDIR:-/tmp}/ccmorph-listing.XXXXXX")
GOCACHE="${GOCACHE:-$TMPDIR/ccmorph-evaluator-gocache}" make shim
mkdir "$d/bin"
printf '%s\n' '#!/bin/sh' 'for a do case "$a" in */v1/models*) printf "__CCMORPH_STATUS__%s\n" "$STUB_STATUS"; exit 0;; esac; done' 'exit 1' > "$d/bin/curl"
chmod +x "$d/bin/curl"
rc=0
for code in 404 405; do
  mkdir "$d/$code"
  printf 'apikey\ncustom\nhttp://gateway.invalid\nsk-not-a-real-key\n' | env -i HOME="$d/$code" TMPDIR="$d/$code" PATH="$d/bin:$PATH" STUB_STATUS="$code" CCMORPH_LIB_DIR="$PWD/lib" bin/ccmorph add gateway > "$d/$code/out" 2> "$d/$code/err"
  test -f "$d/$code/.config/ccmorph/gateway.conf"
  if ! awk -v code="$code" 'tolower($0) ~ /listing/ && index($0, code) && tolower($0) ~ /not checked/ {found=1} END {exit !found}' "$d/$code/out"; then printf 'HTTP %s did not name the refused listing\n' "$code"; rc=1; fi
done
[ "$rc" -eq 0 ] && printf 'add_listing_notices: PASS\n'
exit "$rc"
