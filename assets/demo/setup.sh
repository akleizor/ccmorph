# Sourced by the .tape files. Builds a throwaway sandbox: fresh state dir, fake
# profiles, and a scripted stand-in for `claude`. Touches nothing real.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
export HOME=$(mktemp -d) PS1='$ '
export CCPROTEUS_STATE_DIR=$HOME/state
export PATH=$root/assets/demo/bin:$root:$PATH
mkdir -p "$CCPROTEUS_STATE_DIR"
printf 'type=sub\nemail=you@example.com\n' > "$CCPROTEUS_STATE_DIR/work.conf"
printf 'type=apikey\nbase_url=https://api.anthropic.com/\napi_key=sk-demo-not-real\n' > "$CCPROTEUS_STATE_DIR/personal.conf"
printf 'type=apikey\nbase_url=https://gateway.example/\napi_key=sk-demo-not-real\nmodel=gpt-6.1-sol\n' > "$CCPROTEUS_STATE_DIR/gateway.conf"
ccproteus use work >/dev/null
# Hide the launcher's stderr notices so only the story shows.
ccproteus() { command ccproteus "$@" 2>/dev/null; }
unset root
clear
