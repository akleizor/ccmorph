# The tail of every fake ccmorph-adapter: read the --parent-pid it was started
# with, then serve until that pid is gone, the way check_parent in
# ccmorph-adapter does. One copy, shared by the Python and the zsh launch tests
# (#373, GH-330). A serve started without a parent keeps serving, by contract.
#
# Orphaning is how every launch hands its adapter over, not a death (GH-850):
# `util adapter-start` exits once it has read the ready line (util/hop.go:
# "util adapter-start exits once it is ready, so the adapter is reparented").
# A double the head (fake_adapter_claim.sh) claimed -- its named parent was one
# of its own ancestors when it started, as the launcher is -- serves while that
# same process lives and leaves on the launcher's TERM. Its GH-434 guard is the
# parent's start time: once the pid answers with another start time it has
# been recycled, and the double leaves.
#
# Any other double -- the head never ran (this file run on its own), or the
# pid it names is a stranger -- keeps GH-434's orphan exit: it leaves once the
# process that started it is gone (every pass, a builtin) and on each tenth
# pass once ps reports it reparented to PID 1, so a stranger's live pid never
# keeps it serving, whatever its output is.
parent=""
shared=""
prev=""
for arg in "$@"; do
  [ "$prev" = --shared ] && shared="$arg"
  [ "$prev" = --parent-pid ] && parent="$arg"
  case "$arg" in --parent-pid=*) parent="${arg#--parent-pid=}" ;; esac
  prev="$arg"
done
# A shared adapter (one per profile, model and conf; every session of that key
# holds LOCK_SH on <base>.users) lives while some session holds that file: the
# real adapter polls the lock itself. The double asks lsof about once a second
# and leaves once nothing holds the file or its mutex file is gone.
claimed=""
[ "${_fake_claimed:-}" = 1 ] && [ "${_fake_parent:-}" = "$parent" ] && claimed=1
pass=0
while :; do
  if [ -n "$shared" ]; then
    pass=$(( pass + 1 ))
    if [ $(( pass % 5 )) -eq 0 ]; then
      [ -e "$shared.lock" ] || exit 0
      [ -n "$(lsof -t "$shared.users" 2>/dev/null)" ] || exit 0
    fi
  fi
  if [ -n "$parent" ]; then
    kill -0 "$parent" 2>/dev/null || exit 0
    [ -n "$claimed" ] || kill -0 "$PPID" 2>/dev/null || exit 0
    pass=$(( pass + 1 ))
    if [ $(( pass % 10 )) -eq 0 ]; then
      if [ -n "$claimed" ]; then
        [ "$(ps -o lstart= -p "$parent" 2>/dev/null)" = "$_fake_parent_start" ] || exit 0
      else
        set -- $(ps -o ppid= -p $$ 2>/dev/null)
        [ "${1:-}" = 1 ] && exit 0
      fi
    fi
  fi
  sleep 0.2
done
