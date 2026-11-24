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
# while the launch it watches is alive. A launched stray is reaped from
# outside -- fake_adapter.reap by the pid the fake records beside itself,
# which is what GH-434's hang really called for (#347), and the zsh suite's
# per-test sweep of the home.
#
# The GH-434 tenth-pass orphan exit stands for the shape it was written for,
# which is how GH-850 narrows it rather than deletes it. The launcher takes
# the double's output: it starts the adapter with stdout on a readiness file
# and stderr on a log (util/adapterstart.go: cmd.Stdin, cmd.Stdout, cmd.Stderr
# = devnull, ready, logf). A double whose stdout is still the starter's pipe
# was started outside any launcher -- no launcher holds its stop, so an orphan
# of that shape is forever, and it is the one that held a readiness reader
# stranded (GH-434). Tenth pass of that shape, ppid 1 leaves and the inherited
# output closes. A double that cannot probe stdout keeps the safeguard: an
# unclaimed double must never outlive its starter, a launched one may serve
# until its parent or its launcher's stop ends it.
parent=""
prev=""
for arg in "$@"; do
  [ "$prev" = --parent-pid ] && parent="$arg"
  case "$arg" in --parent-pid=*) parent="${arg#--parent-pid=}" ;; esac
  prev="$arg"
done
launched=""
[ -f /dev/fd/1 ] && launched=1
pass=0
while :; do
  if [ -n "$parent" ]; then
    kill -0 "$parent" 2>/dev/null || exit 0
    if [ -z "$launched" ]; then
      pass=$(( pass + 1 ))
      if [ $(( pass % 10 )) -eq 0 ]; then
        set -- $(ps -o ppid= -p $$ 2>/dev/null)
        [ "${1:-}" = 1 ] && exit 0
      fi
    fi
  fi
  sleep 0.2
done
