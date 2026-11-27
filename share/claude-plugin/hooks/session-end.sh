#!/bin/sh
# The SessionEnd hook ccmorph's plugin ships: an attended profile session
# records its id, and Claude Code's JSON input beside it, in a pid-keyed TMPDIR
# file the launcher's resume hint reads (#285, #359). Both records are written
# through link(2), so a file planted at a predictable name in a shared TMPDIR is
# never followed (#410). The command is one call of this file so the line
# Claude Code prints when it cancels or fails the hook names the script, never
# its shell source (#969).
if [ "$CLAUDE_CODE_SESSION_ATTENDED" = 1 ] && [ -n "$CLAUDE_CONFIG_DIR" ] && [ -n "$CLAUDE_CODE_SESSION_ID" ]; then
  d="${TMPDIR:-/tmp}"
  f="$d/ccmorph-session-end.$CLAUDE_PID"
  t=$(mktemp "$d/ccmorph-session-end.XXXXXX") || exit 0
  { cat > "$t" && link "$t" "$f.json"; } 2>/dev/null
  rm -f "$t"
  t=$(mktemp "$d/ccmorph-session-end.XXXXXX") || exit 0
  { printf %s "$CLAUDE_CODE_SESSION_ID" > "$t" && link "$t" "$f"; } 2>/dev/null
  rm -f "$t"
fi
exit 0