#!/usr/bin/env bash
# Shared guards for the manually driven compatibility fixtures.
compat_inspect() {
  local line failure
  local -a fields
  compat_pid_start=; compat_pid_state=
  [[ "$1" =~ ^[1-9][0-9]*$ ]] || return 2
  if [ -O "/proc/$1" ] && line=$(cat "/proc/$1/stat" 2>/dev/null); then
    [[ "$line" = "$1 ("* && "$line" = *') '* ]] || return 2
    read -r -a fields <<< "${line##*) }"
    [ "${#fields[@]}" -ge 20 ] || return 2
    [[ "${fields[19]}" =~ ^[0-9]+$ && "${fields[0]}" =~ ^[RSDZTtXxKWPI]$ ]] || return 2
    compat_pid_start=${fields[19]}; compat_pid_state=${fields[0]}
    return 0
  fi
  # Permission/read/parse failures are not evidence that a process has gone.
  [ ! -e "/proc/$1" ] || return 2
  failure=$(LC_ALL=C kill -0 "$1" 2>&1) && return 2
  [[ "$failure" = *'No such process'* ]] || return 2
  return 1
}
compat_start() {
  local rc=0
  compat_inspect "$1" || rc=$?
  [ "$rc" = 0 ] || return "$rc"
  printf '%s\n' "$compat_pid_start"
}
compat_same() {
  local started
  [ -n "$2" ] && started=$(compat_start "$1") || return 1
  [ "$started" = "$2" ]
}
compat_no_mount() {
  awk -v p="$1" -v r="$root" '$5 == r || (index($5,r"/")==1 && ($5 == p || index(p,$5"/")==1)) {bad=1} END {exit bad}' /proc/self/mountinfo
}
compat_file() {
  compat_no_mount "$1" || return 1
  [ -f "$1" ] && [ ! -L "$1" ] && [ "$(realpath "$1")" = "$1" ]
}
compat_new_file() {
  compat_dir "$(dirname "$1")" && compat_no_mount "$1" || return 1
  [ ! -e "$1" ] && [ ! -L "$1" ] || return 1
  (set -o noclobber; : > "$1")
}
compat_lock() {
  [ "${compat_locked:-0}" = 0 ] || return 0
  local path="$root/control/fixture.lock" identity
  compat_file "$path" || return 1
  identity=$(stat -Lc '%d:%i' "$path") || return 1
  exec {compat_lock_fd}<> "$path"
  if ! flock -w 5 "$compat_lock_fd" || ! compat_file "$path" ||
    [ "$(stat -Lc '%d:%i' "$path")" != "$identity" ] ||
    [ "$(stat -Lc '%d:%i' "/proc/$$/fd/$compat_lock_fd")" != "$identity" ]; then
    exec {compat_lock_fd}>&-
    return 1
  fi
  compat_locked=1
}
compat_unlock() {
  [ "${compat_locked:-0}" = 1 ] || return 0
  flock -u "$compat_lock_fd"
  exec {compat_lock_fd}>&-
  compat_locked=0
}
compat_child_exec() {
  # Called in a background subshell. The parent retains the registration lock.
  exec {compat_lock_fd}>&-
  exec env -i "$@"
}
compat_active_records() {
  compgen -G "$root/control/supervisor.*" >/dev/null || compgen -G "$root/control/preparing.*" >/dev/null
}
compat_dir() {
  local path=$1 piece walk=$root
  [[ "$path" = "$root" || "$path" = "$root/"* ]] || return 1
  # Only fixed paths and their ancestors; the owned work/.../merged is allowed.
  compat_no_mount "$path" || return 1
  [ -d "$walk" ] && [ ! -L "$walk" ] || return 1
  if [ "$path" != "$root" ]; then
    local relative=${path#"$root/"}
    IFS=/ read -r -a parts <<< "$relative"
    for piece in "${parts[@]}"; do
      walk="$walk/$piece"
      [ -d "$walk" ] && [ ! -L "$walk" ] || return 1
    done
  fi
  [ "$(realpath "$path")" = "$path" ]
}
compat_init() {
  root=$1
  [[ "$root" = /tmp/appclone-w4-* && "$(dirname "$root")" = /tmp ]] || return 2
  [ -O "$root" ] && [ "$(stat -c %a "$root")" = 700 ] || return 2
  local path
  for path in "$root" "$root/home" "$root/base" "$root/control" "$root/work" "$root/run" \
    "$root/home/.config" "$root/home/.local/share/applications" "$root/home/.local/state" \
    "$root/home/.cache" "$root/home/Downloads" "$root/service/home" "$root/service/config" \
    "$root/service/data" "$root/service/cache" "$root/service/state" "$root/service/run"; do
    compat_dir "$path" || { echo "unsafe fixture directory: $path" >&2; return 2; }
  done
  for path in "$root/.fixture" "$root/control/fixture.lock" "$root/control/cli" "$root/control/probe-test" "$root/control/app" "$root/control/exe"; do
    compat_file "$path" || return 2
  done
  [ "$(cat "$root/.fixture")" = 'appclone compatibility fixture v1' ] || return 2
  cli=$(cat "$root/control/cli"); probe=$(cat "$root/control/probe-test"); app=$(cat "$root/control/app")
  bus_address="unix:path=$root/run/test-bus"
  app_env=(PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 LC_ALL=C.UTF-8
    HOME="$root/home" AC_HOME="$root/home" AC_BASE="$root/base"
    XDG_CONFIG_HOME="$root/home/.config" XDG_DATA_HOME="$root/home/.local/share"
    XDG_STATE_HOME="$root/home/.local/state" XDG_CACHE_HOME="$root/home/.cache" XDG_RUNTIME_DIR="$root/run"
    GSETTINGS_BACKEND=memory XDG_CURRENT_DESKTOP=GNOME
    DBUS_SESSION_BUS_ADDRESS="$bus_address"
    APPCLONE_APP_COMPAT=1 APPCLONE_COMPAT_ROOT="$root" APPCLONE_COMPAT_APP="$app")
  for path in DISPLAY WAYLAND_DISPLAY XAUTHORITY; do
    if [ -n "${!path:-}" ]; then app_env+=("$path=${!path}"); fi
  done
  service_env=(PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 LC_ALL=C.UTF-8
    HOME="$root/service/home" XDG_CONFIG_HOME="$root/service/config" XDG_DATA_HOME="$root/service/data"
    XDG_CACHE_HOME="$root/service/cache" XDG_STATE_HOME="$root/service/state" XDG_RUNTIME_DIR="$root/service/run"
    GSETTINGS_BACKEND=memory DBUS_SESSION_BUS_ADDRESS="$bus_address")
}
compat_stop_child() {
  local pid=$1 original=$2 attempt rc
  compat_child_rc=
  for ((attempt=0; attempt<=50; attempt++)); do
    rc=0; compat_inspect "$pid" || rc=$?
    [ "$rc" != 2 ] || { echo "unknown test process: pid=$pid; retain $root" >&2; return 1; }
    if [ "$rc" = 0 ]; then
      [ "$compat_pid_start" = "$original" ] || return 1
    fi
    if [ "$rc" = 1 ] || [ "$compat_pid_state" = Z ]; then
      compat_child_rc=0; wait "$pid" 2>/dev/null || compat_child_rc=$?
      printf 'COMPAT reclaimed pid=%s wait_exit=%s\n' "$pid" "$compat_child_rc" >&2
      return 0
    fi
    if [ "$attempt" = 0 ]; then kill -TERM "$pid" 2>/dev/null || true; fi
    if [ "$attempt" = 30 ]; then kill -KILL "$pid" 2>/dev/null || true; fi
    [ "$attempt" -lt 50 ] || break
    sleep .1
  done
  echo "test process unreclaimed: pid=$pid; retain $root" >&2
  return 1
}
compat_service() {
  local role pid started extra observed
  local -A roles=() pids=()
  compat_file "$root/control/services" || return 3
  [ -S "$root/run/test-bus" ] && [ ! -L "$root/run/test-bus" ] || return 3
  bus_pid=
  while read -r role pid started extra || [ -n "$role$pid$started$extra" ]; do
    [[ "$pid" =~ ^[1-9][0-9]*$ && "$started" =~ ^[1-9][0-9]*$ && -z "$extra" ]] || return 3
    case "$role" in bus|keyring|serve) ;; *) return 3;; esac
    [ -z "${roles[$role]:-}" ] && [ -z "${pids[$pid]:-}" ] || return 3
    roles[$role]=1; pids[$pid]=1
    compat_inspect "$pid" || return 3
    [ "$compat_pid_start" = "$started" ] || return 3
    case "$compat_pid_state" in Z|X|x) return 3;; esac
    [ "$role" != bus ] || bus_pid=$pid
  done < "$root/control/services"
  [ "${roles[serve]:-}" = 1 ] && [ "${roles[bus]:-}" = 1 ] && [ "${roles[keyring]:-}" = 1 ] || return 3
  observed=$(env -i "${service_env[@]}" timeout 3 gdbus call --session --dest org.freedesktop.DBus \
    --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.GetConnectionUnixProcessID org.freedesktop.DBus)
  [ "$observed" = "(uint32 $bus_pid,)" ]
}
