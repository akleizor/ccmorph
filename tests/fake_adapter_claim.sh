# The head of every fake ccmorph-adapter, run before its ready line (GH-850).
# The launcher's `util adapter-start` starts the adapter with
# `--parent-pid <launcher>` and waits for the ready line, so while the head
# runs that launcher is one of this double's own ancestors. Once the ready line
# is out, adapter-start exits and the double is reparented (util/hop.go), and
# from then on only what the head noted can tell the launch it serves from a
# stranger whose pid happens to answer. So the head notes, for the serve tail
# (fake_adapter_serve.sh): whether the named parent is an ancestor
# (_fake_claimed=1) and when that process started (_fake_parent_start), so a
# recycled pid is seen for what it is (GH-434). A double whose named parent is
# not an ancestor -- the stranger of GH-434 -- stays unclaimed, and the tail
# ends it once its starter is gone. Prints nothing: the ready line must stay
# the double's first stdout line.
_fake_parent=""
_fake_prev=""
for _fake_arg in "$@"; do
  [ "$_fake_prev" = --parent-pid ] && _fake_parent="$_fake_arg"
  case "$_fake_arg" in --parent-pid=*) _fake_parent="${_fake_arg#--parent-pid=}" ;; esac
  _fake_prev="$_fake_arg"
done
_fake_claimed=""
_fake_parent_start=""
if [ -n "$_fake_parent" ]; then
  _fake_claimed=$(ps -A -o pid= -o ppid= 2>/dev/null | awk -v me="$$" -v want="$_fake_parent" '
    { up[$1] = $2 }
    END { p = me; for (i = 0; i < 64 && p > 1; i++) { p = up[p]; if (p == want) { print 1; exit } } }')
  [ "$_fake_claimed" = 1 ] && _fake_parent_start=$(ps -o lstart= -p "$_fake_parent" 2>/dev/null)
  [ -n "$_fake_parent_start" ] || _fake_claimed=""
fi
