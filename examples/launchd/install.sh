#!/bin/sh
# Install a macOS LaunchAgent that starts Oscar (with its supervisor) when you log in
# and starts it again if the supervisor itself stops with an error.
#
#   examples/launchd/install.sh [path-to-oscar-binary]
#
# Uninstall:  launchctl bootout gui/$(id -u)/dev.oscar && rm ~/Library/LaunchAgents/dev.oscar.plist
# Stop/start: launchctl kickstart -k gui/$(id -u)/dev.oscar   (restart)
#
# launchd starts programs with a very short PATH, so the PATH of the shell that runs this
# script is saved into the agent. Run it from a shell where claude, git and go work.
set -eu
label=${OSCAR_LABEL:-dev.oscar}
bin=${1:-"$(cd "$(dirname "$0")/../.." && pwd)/oscar"}
log=${OSCAR_LOG:-"$HOME/.oscar/server.log"}
plist="$HOME/Library/LaunchAgents/$label.plist"

[ -x "$bin" ] || { echo "no oscar binary at $bin (build it: go build -o oscar ./cmd/oscar)" >&2; exit 1; }
command -v claude >/dev/null || { echo "claude is not on PATH" >&2; exit 1; }
mkdir -p "$(dirname "$log")" "$HOME/Library/LaunchAgents"

# Keep only absolute PATH entries.
path=$(printf '%s' "$PATH" | tr ':' '\n' | grep '^/' | awk '!seen[$0]++' | paste -sd: -)

cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$bin</string></array>
  <key>WorkingDirectory</key><string>$(dirname "$bin")</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$path</string>
    <key>HOME</key><string>$HOME</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>$log</string>
  <key>StandardErrorPath</key><string>$log</string>
</dict>
</plist>
PLIST

uid=$(id -u)
launchctl bootout "gui/$uid/$label" 2>/dev/null || true
launchctl bootstrap "gui/$uid" "$plist"
launchctl enable "gui/$uid/$label"
echo "installed $plist; Oscar starts now and at every login. Log: $log"
