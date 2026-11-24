# The tail of every fake ccmorph-adapter: read the --parent-pid it was started
# with, then serve until that pid is gone, the way check_parent in
# ccmorph-adapter does. One copy, shared by the Python and the zsh launch tests
# (#373, GH-330). A serve started without a parent keeps serving, by contract.
#
# Orphaning is not a death (GH-850). The launched adapter is reparented the
# moment it is ready -- the launcher's `util adapter-start` exits once it has
# read the port line (util/hop.go: "util adapter-start exits once it is ready,
# so the adapter is reparented") -- so every healthy launch runs its adapter
# with ppid 1 for its whole life, real ones included. The GH-434 backstop this
# file once carried read a tenth-pass ppid of 1 as a stray and ended doubles
# about two seconds into every launch: before the launcher's stop TERM
# arrived, and without running its trap, so the stop record never appeared and
# the launch's stopped-when-the-session-ends test failed under the gate's
# parallel load while passing alone. What a double must never do is leave
# while the launch it watches is alive. A stray is reaped from outside --
# fake_adapter.reap by the pid the fake records beside itself, which is what
# GH-434's hang really called for (#347), and the zsh suite's per-test sweep
# of the home.
parent=""
prev=""
for arg in "$@"; do
  [ "$prev" = --parent-pid ] && parent="$arg"
  case "$arg" in --parent-pid=*) parent="${arg#--parent-pid=}" ;; esac
  prev="$arg"
done
while :; do
  if [ -n "$parent" ]; then
    kill -0 "$parent" 2>/dev/null || exit 0
  fi
  sleep 0.2
done
