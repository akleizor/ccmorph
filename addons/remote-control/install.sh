#!/bin/zsh
# Opt-in install of Remote Control. The core installer never runs this; ccmorph
# works identically with or without it.
#   addons/remote-control/install.sh              link the commands, start the supervisor
#   addons/remote-control/install.sh --uninstall  stop the supervisor, remove what this added
#
# The supervisor (ccmorph-rc-hosts) runs as a LaunchAgent: it starts at login,
# relaunches itself if it dies, and keeps every folder listed in rc-hosts served.
set -eu
HERE="${0:A:h}"
BIN="$HOME/.local/bin"
LABEL="com.ccmorph.rc-hosts"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/ccmorph/rc-hosts.log"
STATE="${CCMORPH_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph}"
DOMAIN="gui/$(id -u)"

if [[ "${1:-}" == --uninstall ]]; then
  launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
  rm -f "$PLIST"
  for cmd in ccmorph-rc ccmorph-rc-hosts; do
    [[ -L "$BIN/$cmd" && "$(readlink "$BIN/$cmd")" == "$HERE/$cmd" ]] && rm "$BIN/$cmd"
  done
  print "removed $LABEL and its commands (rc-hosts and hosts' state are kept)"
  exit 0
fi

command -v mitmdump >/dev/null || print -u2 "note: mitmdump not found — brew install mitmproxy"
mkdir -p "$BIN"
for cmd in ccmorph-rc ccmorph-rc-hosts; do
  if [[ -e "$BIN/$cmd" && ! -L "$BIN/$cmd" ]] || [[ -L "$BIN/$cmd" && "$(readlink "$BIN/$cmd")" != "$HERE/$cmd" ]]; then
    print -u2 "refusing: $BIN/$cmd exists and is not this add-on's link"
    exit 1
  fi
  ln -sfn "$HERE/$cmd" "$BIN/$cmd"
done

[[ -e "$STATE/rc-hosts" ]] || cat > "$STATE/rc-hosts" <<'CONF'
# One Remote Control host per folder, kept up by the com.ccmorph.rc-hosts
# LaunchAgent. Edits apply within seconds.
#route=<endpoint profile>
#permission=bypassPermissions
#~/project
CONF

mkdir -p "${PLIST:h}" "${LOG:h}"
# ProcessType Interactive: launchd gives the supervisor no CPU/IO throttling
# (man launchd.plist), so the poll loop keeps its bridges online under heavy load.
cat > "$PLIST" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>$LABEL</string>
	<key>ProcessType</key><string>Interactive</string>
	<key>ProgramArguments</key><array><string>$HERE/ccmorph-rc-hosts</string></array>
	<key>EnvironmentVariables</key><dict>
		<key>PATH</key><string>$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>2</integer>
	<key>WorkingDirectory</key><string>$HOME</string>
	<key>StandardOutPath</key><string>$LOG</string>
	<key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
PL
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootstrap "$DOMAIN" "$PLIST"
print "running $LABEL (log: $LOG); folders: $STATE/rc-hosts"
