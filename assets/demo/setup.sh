# Sourced by the .tape files. Builds a throwaway sandbox: fresh state dir, fake
# profiles, and a scripted stand-in for `claude`. Touches nothing real.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
export HOME=$(mktemp -d) PS1='$ '
# The sandbox is this shell's to put away, however it ends: a recording killed
# mid-tape must not leave its HOME in the temp dir for good (GH-196). The
# signal traps exit through the EXIT trap, so one removal covers every ending.
_ccm_demo_home=$HOME
trap 'rm -rf -- "$_ccm_demo_home"' EXIT
trap 'exit 129' HUP
trap 'exit 143' TERM
export CCMORPH_STATE_DIR=$HOME/state
export PATH=$root/assets/demo/bin:$root/bin:$PATH
mkdir -p "$CCMORPH_STATE_DIR"
printf 'type=sub\nemail=you@example.com\n' > "$CCMORPH_STATE_DIR/work.conf"
printf 'type=apikey\nbase_url=https://api.anthropic.com/\napi_key=sk-demo-not-real\n' > "$CCMORPH_STATE_DIR/personal.conf"
printf 'type=apikey\nbase_url=https://gateway.example/\napi_key=sk-demo-not-real\nmodel=gpt-6.1-sol\n' > "$CCMORPH_STATE_DIR/gateway.conf"
ccmorph use work >/dev/null
# Hide the launcher's stderr notices so only the story shows.
ccmorph() { command ccmorph "$@" 2>/dev/null; }
unset root
clear
