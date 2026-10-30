#!/bin/sh
# Installs Codex Switch from this checkout, or updates the installation.
#
# The first run builds the proxy, runs it under a launchd job that keeps it
# running, and installs the menu app. Later runs switch the proxy with
# install-proxy.sh, so new work goes to the new build while running work
# finishes on the old one, then rebuild and reopen the menu app.
set -eu

repository=$(CDPATH='' cd -P "$(dirname "$0")/.." && pwd)
. "$repository/deploy/installation.sh"

build=$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/codex-switch-build.XXXXXX")
trap '/usr/bin/trash "$build"' EXIT
"$repository/macos/CodexSwitchMenu/package_app.sh" --live --destination "$build/CodexSwitchMenu.app"

"$repository/deploy/install-proxy.sh" "$build/CodexSwitchMenu.app/Contents/Helpers/codex-switch"
binary=$(/bin/realpath "$command_link")
case "$binary" in
  *.a) active_slot=a ;;
  *.b) active_slot=b ;;
  *) echo 'could not identify the serving proxy slot' >&2; exit 1 ;;
esac
active_plist="$HOME/Library/LaunchAgents/$label.$active_slot.plist"
data=$(/usr/bin/plutil -extract EnvironmentVariables.CODEX_SWITCH_DATA_DIR raw "$active_plist")
listen=$(/usr/bin/plutil -extract ProgramArguments.3 raw "$active_plist")
config_line=$("$binary" print-codex-config --listen "$listen")

quit_menu_app
if [ -e "$app" ]; then
  /usr/bin/trash "$app"
fi
/bin/mkdir -p "$(dirname "$app")"
/bin/mv "$build/CodexSwitchMenu.app" "$app"
/usr/bin/open -n "$app" --args --data-dir "$data"

echo
echo "Codex Switch is running on $listen. Point Codex at it with this line at the top of ~/.codex/config.toml:"
echo
echo "$config_line"
