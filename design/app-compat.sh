#!/usr/bin/env bash
# Manually driven installed-app acceptance, with retained artificial state.
# init CLI CORE_TEST APP EXEC [ARGS...]
# ROOT serve|stop-service|probe|reprobe|add ID|run ID|stop ID|mode shared|mode private|trash ID
set -euo pipefail
umask 077
script_dir=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
source "$script_dir/compat-common.sh"
if [ "${1:-}" = init ]; then
  shift
  cli=$(realpath "${1:?CLI}"); probe=$(realpath "${2:?CORE_TEST}")
  app=${3:?APP}; exe=${4:?EXEC}; shift 4
  [[ "$app" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || exit 2
  [ -x "$cli" ] && [ -x "$probe" ] && [ -x "$exe" ] || exit 2
  for arg in "$exe" "$@"; do
    [[ "$arg" != *$'\n'* && "$arg" != *'%'* && "$arg" != *'"'* && "$arg" != *'\'* && "$arg" != *'$'* ]] || exit 2
  done
  root=$(mktemp -d "/tmp/appclone-w4-$app.XXXXXX")
  mkdir -p "$root/home/.config" "$root/home/.local/share/applications" \
    "$root/home/.local/state" "$root/home/.cache" "$root/home/Downloads" \
    "$root/run" "$root/base" "$root/work" "$root/control" "$root/service/"{home,config,data,state,cache,run}
  printf 'appclone compatibility fixture v1\n' > "$root/.fixture"
  printf '%s\n' "$cli" > "$root/control/cli"
  printf '%s\n' "$probe" > "$root/control/probe-test"
  printf '%s\n' "$app" > "$root/control/app"
  printf '%s\n' "$exe" > "$root/control/exe"
  : > "$root/control/fixture.lock"
  printf 'XDG_DOWNLOAD_DIR="$HOME/Downloads"\n' > "$root/home/.config/user-dirs.dirs"
  printf 'appclone synthetic public file\n' > "$root/home/Downloads/compat-public.txt"
  {
    printf '[Desktop Entry]\nType=Application\nName=Compatibility %s\nExec=' "$app"
    printf '"%s" ' "$exe" "$@"
    printf '\nCategories=Utility;\n'
  } > "$root/home/.local/share/applications/$app.desktop"
  printf '%s\n' "$root"
  exit 0
fi
root=${1:?ROOT}; action=${2:?ACTION}; shift 2
compat_init "$root"
cd "$root/home"
compat_lock

if [ "$action" = serve ]; then
  for file in "$root/control/services" "$root/run/test-bus" "$root/control/stop-service"; do
    [ ! -e "$file" ] && [ ! -L "$file" ] || exit 3
  done
  compat_active_records && exit 3
  for file in test-bus.conf test-bus.log test-keyring.log; do
    if [ -e "$root/control/$file" ] || [ -L "$root/control/$file" ]; then compat_file "$root/control/$file" || exit 3; fi
  done
  cat > "$root/control/test-bus.conf" <<BUS
<busconfig><type>session</type><listen>$bus_address</listen><auth>EXTERNAL</auth>
<policy context="default"><allow own="*"/><allow send_destination="*"/><allow receive_sender="*"/></policy></busconfig>
BUS
  bus_pid=; keyring_pid=; bus_start=; keyring_start=
  clean_services() {
    local failed=0
    compat_lock || return 1
    if compat_active_records; then echo "active or unresolved application records; retain $root" >&2; return 1; fi
    if [ -n "$keyring_pid" ]; then compat_stop_child "$keyring_pid" "$keyring_start" || failed=1; fi
    if [ "$failed" = 0 ] && [ -n "$bus_pid" ]; then compat_stop_child "$bus_pid" "$bus_start" || failed=1; fi
    if [ "$failed" = 0 ]; then
      for file in services stop-service; do
        if [ -e "$root/control/$file" ] || [ -L "$root/control/$file" ]; then
          compat_file "$root/control/$file" || return 1
          rm -- "$root/control/$file"
        fi
      done
    fi
    return "$failed"
  }
  trap 'rc=$?; trap - EXIT; clean_services || rc=1; exit "$rc"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  compat_child_exec "${service_env[@]}" dbus-daemon --nofork --config-file="$root/control/test-bus.conf" \
    > "$root/control/test-bus.log" 2>&1 &
  bus_pid=$!; bus_start=$(compat_start "$bus_pid")
  for ((attempt=0; attempt<40; attempt++)); do
    [ ! -S "$root/run/test-bus" ] || break
    compat_same "$bus_pid" "$bus_start" || exit 3
    sleep .1
  done
  [ -S "$root/run/test-bus" ] || exit 3
  printf 'appclone artificial keyring password\n' | compat_child_exec "${service_env[@]}" \
    gnome-keyring-daemon --foreground --components=secrets --unlock \
    > "$root/control/test-keyring.log" 2>&1 &
  keyring_pid=$!; keyring_start=$(compat_start "$keyring_pid")
  ready=false
  for ((attempt=0; attempt<40; attempt++)); do
    if env -i "${service_env[@]}" timeout 3 gdbus call --session --dest org.freedesktop.DBus \
      --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner org.freedesktop.secrets | grep -q true; then
      ready=true; break
    fi
    compat_same "$keyring_pid" "$keyring_start" || break
    sleep .1
  done
  "$ready" || exit 3
  service_record=$(mktemp "$root/control/.services.XXXXXXXX")
  compat_file "$service_record" || exit 3
  {
    printf 'serve %s %s\n' "$$" "$(compat_start "$$")"
    printf 'bus %s %s\nkeyring %s %s\n' "$bus_pid" "$bus_start" "$keyring_pid" "$keyring_start"
  } > "$service_record"
  [ ! -e "$root/control/services" ] && [ ! -L "$root/control/services" ] || exit 3
  mv -T -- "$service_record" "$root/control/services"
  compat_service
  compat_unlock
  printf 'COMPAT persistent test service ready root=%s bus=%s keyring=%s\n' "$root" "$bus_pid" "$keyring_pid"
  while [ ! -e "$root/control/stop-service" ] && [ ! -L "$root/control/stop-service" ]; do sleep .2; done
  compat_file "$root/control/stop-service" && [ ! -s "$root/control/stop-service" ] || exit 3
  exit 0
fi

compat_service || { echo 'verified fixture service required' >&2; exit 3; }
if [ "$action" != stop ]; then
  [ ! -e "$root/control/stop-service" ] && [ ! -L "$root/control/stop-service" ] || exit 3
fi
case "$action" in
  add|run|stop|trash) [[ "${1:-}" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || exit 2;;
esac
case "$action" in
  stop-service)
    compat_active_records && { echo 'stop all fixture applications first' >&2; exit 3; }
    compat_new_file "$root/control/stop-service"
    exit 0;;
  stop) compat_unlock; exec env -i "${app_env[@]}" "$cli" stop "$app" "$1";;
  trash)
    compat_active_records && exit 3
    env -i "${app_env[@]}" "$cli" rm "$app" "$1"
    exit $?;;
  mode)
    compat_active_records && exit 3
    case "${1:-}" in shared) value=false;; private) value=true;; *) exit 2;; esac
    compat_dir "$root/base/profiles.d" && compat_file "$root/base/profiles.d/$app.toml" || exit 2
    profile="$root/base/profiles.d/$app.toml"
    sed -i -E '/^isolate_(dbus|runtime|keyring) = /d' "$profile"
    sed -i "1i isolate_keyring = $value\nisolate_dbus = $value\nisolate_runtime = $value" "$profile"
    exit 0;;
esac
case "$action" in probe|reprobe|add|run) ;; *) exit 2;; esac
supervisor=; original=; record=
preparing="$root/control/preparing.$$.$(compat_start "$$")"
compat_new_file "$preparing"
printf '%s %s %s\n' "$$" "$action" "${1:-probe}" > "$preparing"
cleanup_action() {
  if [ -n "$supervisor" ]; then
    compat_stop_child "$supervisor" "$original" || return 1
  fi
  # The supervisor exiting after our signal does not prove core cleanup passed.
  # Keep both kinds of record on every exceptional path for explicit postcheck.
}
trap 'rc=$?; trap - EXIT; cleanup_action || rc=1; exit "$rc"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
case "$action" in
  probe|reprobe)
    if [ "$action" = reprobe ]; then app_env+=(APPCLONE_COMPAT_REPROBE=1); fi
    compat_child_exec "${app_env[@]}" "$probe" -test.run '^TestAppCompatProbeE2E$' -test.v -test.timeout 12m &;;
  add) compat_child_exec "${app_env[@]}" "$cli" add "$app" "$1" &;;
  run) compat_child_exec "${app_env[@]}" "$cli" run "$app" "$1" &;;
esac
supervisor=$!
inspect_rc=0; original=$(compat_start "$supervisor") || inspect_rc=$?
if [ "$inspect_rc" = 1 ]; then
  rc=0; wait "$supervisor" || rc=$?
  supervisor=
  if [ "$rc" = 0 ]; then rm -- "$preparing"; fi
  printf 'COMPAT action=%s fast_exit=%s\n' "$action" "$rc"
  exit "$rc"
fi
[ "$inspect_rc" = 0 ] || exit 3
record="$root/control/supervisor.$supervisor.$original"
compat_new_file "$record"
printf '%s %s %s %s\n' "$supervisor" "$original" "$action" "${1:-probe}" > "$record"
rm -- "$preparing"
compat_unlock
printf 'COMPAT action=%s supervisor=%s start=%s record=%s\n' "$action" "$supervisor" "$original" "$record"
rc=0; wait "$supervisor" || rc=$?
supervisor=
if [ "$rc" = 0 ]; then
  compat_lock
  compat_file "$record" || exit 3
  rm -- "$record"
fi
printf 'COMPAT action=%s exited=%s retained_record=%s\n' "$action" "$rc" "$record"
exit "$rc"
