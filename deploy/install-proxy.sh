#!/bin/sh
# Install or update the proxy. Only one installer may switch slots at a time.
set -eu
umask 077

deployment_directory=$(CDPATH='' cd -P "$(dirname "$0")" && pwd)
. "$deployment_directory/installation.sh"
# Existing installations may have a custom data directory and listen address.
for existing_plist in "$plist" "$HOME/Library/LaunchAgents/$label.a.plist" "$HOME/Library/LaunchAgents/$label.b.plist"; do
  if [ -f "$existing_plist" ]; then
    data=$(/usr/bin/plutil -extract EnvironmentVariables.CODEX_SWITCH_DATA_DIR raw "$existing_plist")
    break
  fi
done
mode=${1:?usage: install-proxy.sh <proxy-binary> | --back}
manual=false
if [ "$mode" = --back ]; then
  manual=true
  candidate="$data/previous-binary"
else
  candidate=$mode
fi
[ -f "$candidate" ] || { echo "proxy binary not found: $candidate" >&2; exit 1; }
/bin/mkdir -p -m 700 "$data" "$(dirname "$binary")" "$(dirname "$plist")"
workspace=$(/usr/bin/mktemp -d "$data/proxy-update.XXXXXX")
/bin/cp "$candidate" "$workspace/codex-switch"
printf '%s\n' "$$" > "$workspace/owner"
lock="$data/proxy-install.lock"
owns_lock=false
cleanup() {
  if [ "$owns_lock" = true ]; then /bin/rmdir "$lock/$$" "$lock" 2>/dev/null || true; fi
  /usr/bin/trash "$workspace"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
while ! /bin/mkdir "$lock" 2>/dev/null; do
  owner=""
  for entry in "$lock"/*; do [ -d "$entry" ] && owner=${entry##*/}; done
  if [ -n "$owner" ] && ! kill -0 "$owner" 2>/dev/null; then
    /bin/rmdir "$lock/$owner" "$lock" 2>/dev/null || true
  elif [ -z "$owner" ]; then
    sleep 1
    /bin/rmdir "$lock" 2>/dev/null || true
  else
    sleep 1
  fi
done
/bin/mkdir "$lock/$$"
owns_lock=true

domain="gui/$(id -u)"
state="$data/serving-slot"
status_field() { printf '%s' "$1" | /usr/bin/plutil -extract "$2" raw -o - - 2>/dev/null; }
serving_status() {
  [ -f "$data/client-capability" ] || return 1
  /usr/bin/curl -sf --max-time 2 "http://$listen/client/$(cat "$data/client-capability")/codex-switch/deploy-status"
}
job_pid() { /bin/launchctl print "$domain/$1" 2>/dev/null | /usr/bin/awk '$1 == "pid" && $2 == "=" {print $3; exit}'; }
job_loaded() { /bin/launchctl print "$domain/$1" >/dev/null 2>&1; }
slot_plist() { printf '%s/Library/LaunchAgents/%s.%s.plist' "$HOME" "$label" "$1"; }
slot_binary() { printf '%s.%s' "$binary" "$1"; }
stop_job() {
  stop_label=$1
  stop_pid=$(job_pid "$stop_label")
  /bin/launchctl bootout "$domain/$stop_label" 2>/dev/null &
  stop_command=$!
  while [ -n "$stop_pid" ] && kill -0 "$stop_pid" 2>/dev/null; do
    if [ $(($(/bin/date +%s) - switch_started)) -ge 1800 ]; then
      kill -KILL "$stop_pid" 2>/dev/null || true
      break
    fi
    sleep 1
  done
  wait "$stop_command" || true
}
# Old installations are kept running in their original job during adoption.
if [ -f "$state" ]; then
  active=$(cat "$state")
  active_plist=$(slot_plist "$active")
  active_label="$label.$active"
elif [ -f "$plist" ]; then
  active=legacy
  active_plist=$plist
  active_label=$label
else
  active=none
fi
if [ "$active" != none ]; then
  listen=$(/usr/bin/plutil -extract ProgramArguments.3 raw "$active_plist")
  data_in_plist=$(/usr/bin/plutil -extract EnvironmentVariables.CODEX_SWITCH_DATA_DIR raw "$active_plist")
  [ "$data_in_plist" = "$data" ] || { echo "proxy data directory differs from installation settings" >&2; exit 1; }
  active_binary=$(/usr/bin/plutil -extract ProgramArguments.0 raw "$active_plist")
else
  listen=$new_install_listen
fi
# A candidate left by an interrupted installer was never promoted. Stop it;
# the promoted slot remains the only persistent job.
switch_started=$(cat "$data/switch-started" 2>/dev/null || /bin/date +%s)
for slot in a b; do
  [ "$slot" = "$active" ] && continue
  stop_job "$label.$slot"
  unused_plist=$(slot_plist "$slot")
  [ ! -e "$unused_plist" ] || /usr/bin/trash "$unused_plist"
done
if [ "$active" != legacy ] && [ "$active" != none ]; then
  stop_job "$label"
  [ ! -e "$plist" ] || /usr/bin/trash "$plist"
fi
if [ "$active" != none ] && ! job_loaded "$active_label"; then
  /bin/launchctl bootstrap "$domain" "$active_plist"
fi
if [ "$active" != none ]; then
  /bin/mkdir -p "$(dirname "$command_link")"
  /bin/ln -sf "$active_binary" "$command_link"
fi
wait_for_job() {
  job=$1
  count=0
  while [ "$count" -lt 120 ]; do
    pid=$(job_pid "$job")
    status=$(serving_status || true)
    if [ -n "$pid" ] && [ "$(status_field "$status" pid || true)" = "$pid" ]; then return 0; fi
    count=$((count + 1)); sleep 1
  done
  echo "the proxy job $job did not answer on $listen; see $data/launchd.log" >&2
  return 1
}
if [ "$active" != none ]; then wait_for_job "$active_label"; fi

# Compare all waiting installers after acquiring the lock. Their staged copies
# survive replacement of the app bundle while a previous switch is in progress.
choose_newest() {
  chosen="$workspace/codex-switch"
  chosen_build=$("$chosen" print-build 2>/dev/null || echo 0)
  case "$chosen_build" in ''|*[!0-9]*) chosen_build=0 ;; esac
  [ "$manual" = false ] || return
  for pending in "$data"/proxy-update.*; do
    [ -f "$pending/owner" ] && [ -f "$pending/codex-switch" ] || continue
    kill -0 "$(cat "$pending/owner")" 2>/dev/null || continue
    pending_build=$("$pending/codex-switch" print-build 2>/dev/null || echo 0)
    case "$pending_build" in ''|*[!0-9]*) continue ;; esac
    if [ "$pending_build" -gt "$chosen_build" ]; then chosen="$pending/codex-switch"; chosen_build=$pending_build; fi
  done
}
choose_newest
[ "$manual" = true ] || [ "$chosen_build" -gt 0 ] || { echo 'candidate has no build number' >&2; exit 1; }
if [ "$active" != none ]; then
  active_build=$(status_field "$status" build || echo 0)
  case "$active_build" in ''|*[!0-9]*) active_build=0 ;; esac
  if [ "$manual" = false ] && [ "$chosen_build" -le "$active_build" ]; then echo 'the newest proxy is already serving'; exit 0; fi
  if [ "$manual" = false ] && [ -f "$data/skipped-builds" ] && /usr/bin/grep -qxF "$chosen_build" "$data/skipped-builds"; then echo "build $chosen_build was skipped"; exit 0; fi
  if [ "$manual" = false ]; then
    echo 'waiting for model work before switching'
    while :; do
      status=$(serving_status || true)
      last=$(status_field "$status" last_completed || true)
      last=$(printf '%s' "$last" | /usr/bin/sed -E 's/\.[0-9]+Z$/Z/')
      last_epoch=$(/bin/date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$last" '+%s' 2>/dev/null || echo 0)
      # Older versions record completions in the shared database but have no
      # last_completed status field.
      if [ "$active" = legacy ] && [ "$last_epoch" -eq 0 ] && [ -f "$data/codex-switch.db" ]; then
        last_epoch=$(/usr/bin/sqlite3 "$data/codex-switch.db" 'SELECT coalesce(max(completed_at_unix_nano)/1000000000,0) FROM completed_responses;' 2>/dev/null || echo 0)
      fi
      now=$(/bin/date +%s)
      if [ "$last_epoch" -gt 0 ] && [ $((now - last_epoch)) -le 120 ]; then break; fi
      sleep 5
    done
    choose_newest
    if [ "$chosen_build" -le "$active_build" ]; then exit 0; fi
    if [ -f "$data/skipped-builds" ] && /usr/bin/grep -qxF "$chosen_build" "$data/skipped-builds"; then echo "build $chosen_build was skipped"; exit 0; fi
  fi
fi
if [ "$active" = a ]; then next=b; else next=a; fi
next_label="$label.$next"
next_binary=$(slot_binary "$next")
next_plist=$(slot_plist "$next")
/bin/cp "$chosen" "$next_binary.next"
/bin/mv -f "$next_binary.next" "$next_binary"
# The temporary plist is loaded directly, and becomes a login job only on promotion.
temporary_plist="$workspace/$next_label.plist"
if [ "$active" != none ]; then
  /bin/cp "$active_plist" "$temporary_plist"
  /usr/bin/plutil -replace Label -string "$next_label" "$temporary_plist"
  /usr/bin/plutil -replace ProgramArguments.0 -string "$next_binary" "$temporary_plist"
  argument_index=4
  has_provider_check=false
  while argument=$(/usr/bin/plutil -extract "ProgramArguments.$argument_index" raw "$temporary_plist" 2>/dev/null); do
    [ "$argument" != --check-providers ] || has_provider_check=true
    argument_index=$((argument_index + 1))
  done
  if [ "$has_provider_check" = false ]; then
    /usr/bin/plutil -insert ProgramArguments -string --check-providers -append "$temporary_plist"
  fi
else
  /usr/bin/plutil -create xml1 "$temporary_plist"
  /usr/bin/plutil -insert Label -string "$next_label" "$temporary_plist"
  /usr/bin/plutil -insert ProgramArguments -array "$temporary_plist"
  for argument in "$next_binary" serve --listen "$listen"; do /usr/bin/plutil -insert ProgramArguments -string "$argument" -append "$temporary_plist"; done
  /usr/bin/plutil -insert EnvironmentVariables -dictionary "$temporary_plist"
  /usr/bin/plutil -insert EnvironmentVariables.HOME -string "$HOME" "$temporary_plist"
  /usr/bin/plutil -insert EnvironmentVariables.CODEX_SWITCH_DATA_DIR -string "$data" "$temporary_plist"
  /usr/bin/plutil -insert EnvironmentVariables.PATH -string "$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH" "$temporary_plist"
  /usr/bin/plutil -insert RunAtLoad -bool true "$temporary_plist"
  /usr/bin/plutil -insert KeepAlive -bool true "$temporary_plist"
  /usr/bin/plutil -insert ExitTimeOut -integer 0 "$temporary_plist"
  /usr/bin/plutil -insert StandardOutPath -string "$data/launchd.log" "$temporary_plist"
  /usr/bin/plutil -insert StandardErrorPath -string "$data/launchd.log" "$temporary_plist"
fi
switch_started=$(/bin/date +%s)
printf '%s\n' "$switch_started" > "$data/switch-started"
/bin/launchctl bootstrap "$domain" "$temporary_plist"
if ! wait_for_job "$next_label"; then
  stop_job "$next_label"
  /usr/bin/trash "$data/switch-started"
  if [ "$active" != none ] && [ "$manual" = false ]; then printf '%s\n' "$chosen_build" >> "$data/skipped-builds"; echo "build $chosen_build was skipped"; exit 0; fi
  exit 1
fi
candidate_pid=$pid
if [ "$active" != none ]; then
  echo "proving build $chosen_build on real work"
  while [ $(($(/bin/date +%s) - switch_started)) -lt 300 ]; do
    status=$(serving_status || true)
    current_pid=$(job_pid "$next_label")
    failures=$(status_field "$status" failures || echo 0)
    case "$failures" in ''|*[!0-9]*) failures=0 ;; esac
    if ! kill -0 "$candidate_pid" 2>/dev/null || { [ -n "$current_pid" ] && [ "$current_pid" != "$candidate_pid" ]; } || [ "$failures" -ge 3 ]; then
      [ "$manual" = true ] || printf '%s\n' "$chosen_build" >> "$data/skipped-builds"
      stop_job "$next_label"
      /usr/bin/trash "$data/switch-started"
      echo "build $chosen_build was skipped; the previous proxy remains serving"
      exit 0
    fi
    if [ "$(status_field "$status" pid || true)" != "$candidate_pid" ]; then
      stop_job "$next_label"
      /usr/bin/trash "$data/switch-started"
      echo "could not check the candidate; try again" >&2
      exit 1
    fi
    sleep 1
  done
  /bin/cp "$active_binary" "$data/previous-binary.next"
  /bin/mv -f "$data/previous-binary.next" "$data/previous-binary"
fi
/bin/cp "$temporary_plist" "$next_plist.next"
/bin/mv -f "$next_plist.next" "$next_plist"
printf '%s\n' "$next" > "$state.next"
/bin/mv -f "$state.next" "$state"
/bin/mkdir -p "$(dirname "$command_link")"
/bin/ln -sf "$next_binary" "$command_link"
if [ "$active" != none ]; then
  # bootout signals the old service; it drains rather than accepting new work.
  stop_job "$active_label"
  [ ! -e "$active_plist" ] || /usr/bin/trash "$active_plist"
fi
/usr/bin/trash "$data/switch-started"
echo "Codex Switch build $chosen_build is running on $listen"
