#!/usr/bin/env bash
# Read-only ownership proof for one loopback diagnostic port in our fixture.
set -euo pipefail
source "$(dirname "$(realpath "${BASH_SOURCE[0]}")")/compat-common.sh"
source "$(dirname "$(realpath "${BASH_SOURCE[0]}")")/compat-home.sh"
root=${1:?ROOT}; port=${2:?PORT}
[[ "$port" =~ ^[0-9]+$ ]] || exit 2
compat_init "$root"
compat_service
exe=$(realpath "$(cat "$root/control/exe")")
proc_fields() {
  local line
  [ -O "/proc/$1" ] || return 1
  line=$(cat "/proc/$1/stat") || return 1
  read -r -a fields <<< "${line##*) }"
  [ "${#fields[@]}" -ge 20 ] && [[ "${fields[19]}" =~ ^[0-9]+$ && "${fields[1]}" =~ ^[0-9]+$ ]] || return 1
}
parent_chain() {
  local ancestor=$1 supervisor=$2 depth
  chain_members=()
  for ((depth=0; depth<64; depth++)); do
    [ "$ancestor" -gt 1 ] || return 1
    proc_fields "$ancestor" || return 1
    chain_members+=("$ancestor:${fields[19]}:${fields[1]}")
    [ "$ancestor" != "$supervisor" ] || return 0
    ancestor=${fields[1]}
  done
  return 1
}
home_signature() {
  printf '%s|%s|%s|%s|%s|%s' "$compat_home_kind" "$compat_home_bwrap" "$compat_home_start" \
    "$compat_home_namespace" "$compat_home_view" "$compat_home_source"
}
hex=$(printf '%04X' "$port")
listener_inode() {
  local found v6
  found=$(awk -v p=":$hex" '$2 ~ (p "$") && $4 == "0A" { if ($2 != "0100007F" p) exit 2; print $10 }' /proc/net/tcp) || return 1
  v6=$(awk -v p=":$hex" '$2 ~ (p "$") && $4 == "0A" { print $10 }' /proc/net/tcp6) || return 1
  [[ "$found" =~ ^[0-9]+$ && -z "$v6" ]] || return 1
  printf '%s' "$found"
}
inode=$(listener_inode) || exit 3
for proc in /proc/[0-9]*; do
  [ -O "$proc" ] || continue
  [ "$(readlink "$proc/exe" 2>/dev/null || true)" = "$exe" ] || continue
  pid=${proc##*/}
  proc_fields "$pid" || continue
  started=${fields[19]}
  match=false
  for fd in "$proc"/fd/*; do
    if [ "$(readlink "$fd" 2>/dev/null || true)" = "socket:[$inode]" ]; then match=true; listen_fd=$fd; break; fi
  done
  "$match" || continue
  for record in "$root"/control/supervisor.*; do
    compat_file "$record" || continue
    read -r supervisor supervisor_start phase instance < "$record"
    [[ "$supervisor" =~ ^[0-9]+$ && "$supervisor_start" =~ ^[0-9]+$ && "$phase" =~ ^[a-z]+$ && "$instance" =~ ^[a-z0-9._-]+$ ]] || continue
    compat_same "$supervisor" "$supervisor_start" || continue
    parent_chain "$pid" "$supervisor" || continue
    chain=$(IFS=,; printf '%s' "${chain_members[*]}")
    compat_home_proof "$pid" "$phase" "${chain_members[@]}" || continue
    first_home=$(home_signature)
    parent_chain "$pid" "$supervisor" || exit 3
    [ "$(IFS=,; printf '%s' "${chain_members[*]}")" = "$chain" ] || exit 3
    compat_home_proof "$pid" "$phase" "${chain_members[@]}" || exit 3
    [ "$(home_signature)" = "$first_home" ] || exit 3
    compat_same "$pid" "$started" && compat_same "$supervisor" "$supervisor_start" || exit 3
    [ -O "$proc" ] && [ "$(readlink "$proc/exe")" = "$exe" ] || exit 3
    [ "$(readlink "$listen_fd")" = "socket:[$inode]" ] || exit 3
    [ "$(listener_inode)" = "$inode" ] || exit 3
    nspid=$(awk '/^NSpid:/ {print $NF}' "$proc/status")
    [[ "$nspid" =~ ^[0-9]+$ ]] || exit 3
    printf '{"pid":%s,"start":"%s","supervisor":%s,"supervisorStart":"%s","namespacePID":%s,"phase":"%s","instance":"%s","homeProof":"%s","homeBwrap":%s,"homeBwrapStart":"%s","mountNamespace":"%s","homeView":"%s","homeSource":"%s","chain":"%s"}\n' \
      "$pid" "$started" "$supervisor" "$supervisor_start" "$nspid" "$phase" "$instance" "$compat_home_kind" \
      "$compat_home_bwrap" "$compat_home_start" "$compat_home_namespace" "$compat_home_view" "$compat_home_source" "$chain"
    exit 0
  done
done
exit 3
