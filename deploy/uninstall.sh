#!/bin/sh
# Removes the proxy, its launchd job and the menu app. Sign-ins and usage
# history stay in the data directory.
set -eu

repository=$(CDPATH='' cd -P "$(dirname "$0")/.." && pwd)
. "$repository/deploy/installation.sh"
for installed_plist in "$plist" "$HOME/Library/LaunchAgents/$label.a.plist" "$HOME/Library/LaunchAgents/$label.b.plist"; do
  if [ -f "$installed_plist" ]; then
    data=$(/usr/bin/plutil -extract EnvironmentVariables.CODEX_SWITCH_DATA_DIR raw "$installed_plist")
    break
  fi
done

# Codex and Pi would fail every request once the proxy is gone.
if [ -f "$data/client-capability" ] &&
  /usr/bin/grep -qsF "$(cat "$data/client-capability")" "$HOME/.codex/config.toml" "$HOME/.pi/agent/models.json"; then
  echo "Codex or Pi still uses Codex Switch. Remove its URL from ~/.codex/config.toml and ~/.pi/agent/models.json, restart them, then run this again." >&2
  exit 1
fi

for installed_label in "$label" "$label.a" "$label.b"; do
  /bin/launchctl bootout "gui/$(id -u)/$installed_label" 2>/dev/null || true
done
quit_menu_app
for installed in "$plist" "$HOME/Library/LaunchAgents/$label.a.plist" "$HOME/Library/LaunchAgents/$label.b.plist" "$(dirname "$binary")" "$command_link" "$app" "$data/serving-slot" "$data/switch-started" "$data/previous-binary" "$data/skipped-builds"; do
  if [ -e "$installed" ] || [ -L "$installed" ]; then
    /usr/bin/trash "$installed"
  fi
done

echo "Codex Switch is uninstalled. Your sign-ins and usage history are in $data; move that folder to the Trash to remove them."
