# Sourced by the .tape files. Builds a throwaway sandbox: fresh state dir, fake
# profiles, and a scripted stand-in for `claude`. Touches nothing real.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
export HOME=$(mktemp -d) PS1='\[\033[38;5;245m\]~/app\[\033[0m\] $ '
export CCMORPH_STATE_DIR=$HOME/state
export PATH=$root/assets/demo/bin:$root/bin:$PATH
mkdir -p "$CCMORPH_STATE_DIR"
# A private runtime copy whose ccmorph-core answers "adapter" with NATIVE=1, so a
# gateway profile launches with no network; every other subcommand is the real one.
mkdir -p "$HOME/rt/lib" "$HOME/rt/libexec" "$HOME/rt/share"
cp "$root"/lib/*.sh "$root"/lib/*.zsh "$HOME/rt/lib/"
cp -R "$root"/share/. "$HOME/rt/share/"
printf '#!/bin/sh
[ "$1" = adapter ] && { echo NATIVE=1; exit 0; }
exec "%s/libexec/ccmorph-core" "$@"
' "$root" > "$HOME/rt/libexec/ccmorph-core"
chmod +x "$HOME/rt/libexec/ccmorph-core"
export CCMORPH_LIB_DIR=$HOME/rt/lib
printf 'type=sub\nemail=you@example.com\n' > "$CCMORPH_STATE_DIR/work.conf"
printf 'type=apikey\nbase_url=https://api.deepseek.com/anthropic\napi_key=sk-demo-not-real\n\n' > "$CCMORPH_STATE_DIR/deepseek.conf"
printf 'type=apikey\nbase_url=https://api.moonshot.ai/anthropic\napi_key=sk-demo-not-real\nmodel=gpt-6.1-sol\n\n' > "$CCMORPH_STATE_DIR/kimi.conf"
ccmorph use work >/dev/null
# Hide the launcher's stderr notices so only the story shows.
ccmorph() { command ccmorph "$@" 2>/dev/null; }
unset root
clear
