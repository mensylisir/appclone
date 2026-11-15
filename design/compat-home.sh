#!/usr/bin/env bash
# HOME provenance for owned diagnostic processes whose title overwrites environ.
# Source after compat-common.sh. No user data or environment values are printed.
compat_environment() {
  local path=$1 nonzero entry homes=0
  # Count bytes: command substitution would strip newline-only nonempty data.
  nonzero=$(LC_ALL=C tr -d '\000' < "$path" | wc -c) || return 2
  [ "$nonzero" -gt 0 ] || return 1
  while IFS= read -r -d '' entry; do
    case "$entry" in
      HOME=*)
        [ "$entry" = "HOME=$root/home" ] || return 2
        homes=$((homes+1));;
    esac
  done < "$path"
  [ -z "$entry" ] && [ "$homes" = 1 ] || return 2
}

compat_home_bind() {
  local phase=$1 op src dst count=0 roots=0 boundary=0
  shift
  compat_home_source="$root/home"
  case "$phase" in probe|reprobe|run) ;; *) return 1;; esac
  while [ "$#" -gt 0 ]; do
    op=$1; shift
    case "$op" in
      --) boundary=1; break;;
      --unshare-user|--unshare-pid|--unshare-ipc|--die-with-parent) ;;
      --setenv)
        [ "$#" -ge 2 ] || return 1
        if [ "$1" = HOME ]; then [ "$2" = "$root/home" ] || return 1; fi
        shift 2;;
      --chdir) [ "$#" -ge 1 ] || return 1; shift;;
      --proc|--tmpfs)
        [ "$#" -ge 1 ] || return 1
        dst=$1; shift
        if [ "$dst" = / ] || [ "$dst" = "$root/home" ] || [[ "$root/home" = "$dst/"* ]]; then return 1; fi;;
      --bind|--bind-try|--ro-bind-try|--dev-bind)
        [ "$#" -ge 2 ] || return 1
        src=$1; dst=$2; shift 2
        if [ "$op" = --bind ] && [ "$src" = / ] && [ "$dst" = / ]; then roots=$((roots+1)); continue; fi
        if [ "$dst" = "$root/home" ]; then
          [ "$phase" != run ] && [ "$op" = --bind ] || return 1
          local relative=${src#"$root/work/appclone-probe-$(id -u)-"}
          [[ "$relative" =~ ^[0-9]+/merged$ ]] && [ "$relative" != "$src" ] || return 1
          compat_home_source=$src; count=$((count+1))
        elif [ "$dst" = / ] || [[ "$root/home" = "$dst/"* ]]; then
          return 1
        fi;;
      *) return 1;;
    esac
  done
  [ "$boundary" = 1 ] && [ "$roots" = 1 ] || return 1
  if [ "$phase" = run ]; then [ "$count" = 0 ]; else [ "$count" = 1 ]; fi
}

compat_same_view() {
  [[ "$1" =~ ^mnt:\[[0-9]+\]$ && "$3" =~ ^[0-9]+:[0-9]+$ ]] || return 1
  [ "$1" = "$2" ] && [ "$3" = "$4" ]
}

compat_bwrap_home() {
  local pid=$1 ancestor=$2 original=$3 phase=$4 bwrap_exe namespace view target_view
  local -a argv
  bwrap_exe=$(realpath /usr/bin/bwrap) || return 1
  [ -O "/proc/$ancestor" ] && [ "$(readlink "/proc/$ancestor/exe")" = "$bwrap_exe" ] || return 1
  compat_inspect "$ancestor" || return 1
  [ "$compat_pid_start" = "$original" ] || return 1
  case "$compat_pid_state" in Z|X|x) return 1;; esac
  compat_environment "/proc/$ancestor/environ" || return 1
  mapfile -d '' -t argv < "/proc/$ancestor/cmdline" || return 1
  [ "${#argv[@]}" -gt 1 ] || return 1
  compat_home_bind "$phase" "${argv[@]:1}" || return 1
  namespace=$(readlink "/proc/$ancestor/ns/mnt") || return 1
  if [ "$phase" != run ]; then
    compat_dir "$(dirname "$compat_home_source")" || return 1
    [ ! -L "$compat_home_source" ] || return 1
    awk -v p="$compat_home_source" '$5 == p && index($0," - fuse.fuse-overlayfs ") {ok++} END {exit ok != 1}' /proc/self/mountinfo || return 1
  else
    compat_dir "$compat_home_source" || return 1
  fi
  view=$(timeout -k 1 2 stat -Lc '%d:%i' "$compat_home_source") || return 1
  target_view=$(timeout -k 1 2 stat -Lc '%d:%i' "/proc/$pid/root$root/home") || return 1
  compat_same_view "$namespace" "$(readlink "/proc/$pid/ns/mnt")" "$view" "$target_view" || return 1
  compat_same "$ancestor" "$original" || return 1
  compat_home_bwrap=$ancestor; compat_home_start=$original
  compat_home_namespace=$namespace; compat_home_view=$view
}

compat_home_proof() {
  local pid=$1 phase=$2 member ancestor original parent rc=0
  shift 2
  compat_home_kind=process-env; compat_home_bwrap=0; compat_home_start=0
  compat_home_source=; compat_home_view=
  compat_home_namespace=$(readlink "/proc/$pid/ns/mnt") || return 1
  compat_environment "/proc/$pid/environ" || rc=$?
  [ "$rc" != 2 ] || return 1
  [ "$rc" != 0 ] || return 0
  for member in "$@"; do
    IFS=: read -r ancestor original parent <<< "$member"
    [ "$ancestor" != "$pid" ] || continue
    if compat_bwrap_home "$pid" "$ancestor" "$original" "$phase"; then
      compat_home_kind=bwrap-env+mount-view
      return 0
    fi
  done
  return 1
}
