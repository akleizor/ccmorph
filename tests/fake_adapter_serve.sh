# The tail of every fake ccproteus-adapter: read the --parent-pid it was started
# with, then serve until that pid is gone, the way check_parent in
# ccproteus-adapter does. One copy, shared by the Python and the zsh launch tests
# (#373, GH-330). GH-434: a recycled pid can keep the named parent "alive"
# forever, so a serve that gave --parent-pid also leaves when its own launcher
# is gone (ppid 1) -- a serve started without one keeps serving, by contract.
parent=""
prev=""
for arg in "$@"; do
  [ "$prev" = --parent-pid ] && parent="$arg"
  case "$arg" in --parent-pid=*) parent="${arg#--parent-pid=}" ;; esac
  prev="$arg"
done
pass=0
while :; do
  pass=$(( pass + 1 ))
  if [ -n "$parent" ]; then
    kill -0 "$parent" 2>/dev/null || exit 0
    if [ $(( pass % 10 )) -eq 0 ]; then
      set -- $(ps -o ppid= -p $$ 2>/dev/null)
      [ "${1:-}" = 1 ] && exit 0
    fi
  fi
  sleep 0.2
done