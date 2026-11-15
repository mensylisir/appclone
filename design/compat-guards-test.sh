#!/usr/bin/env bash
# Synthetic supervisor/control regression. Never starts installed applications.
set -euo pipefail
umask 077
script_dir=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
source "$script_dir/compat-common.sh"
source "$script_dir/compat-home.sh"
driver="$script_dir/app-compat.sh"
root=$(bash "$driver" init /usr/bin/true /usr/bin/true fixture /usr/bin/true)
compat_init "$root"
printf 'COMPAT guards fixture=%s\n' "$root"
service_pid=; service_start=
cleanup() {
  if [ -n "$service_pid" ] && compat_same "$service_pid" "$service_start"; then
    if ! compat_active_records; then
      bash "$driver" "$root" stop-service >/dev/null 2>&1 || true
      compat_stop_child "$service_pid" "$service_start" || true
    fi
  fi
  echo "COMPAT guards retained fixture=$root" >&2
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
expect_failure() {
  if "$@"; then fail "unexpected success: $*"; fi
}
await_file() {
  local attempt
  for ((attempt=0; attempt<80; attempt++)); do
    [ ! -f "$1" ] || return 0
    sleep .1
  done
  fail "file not ready: $1"
}

environment="$root/control/environment-test"
printf 'HOME=%s\000PATH=/bin\000' "$root/home" > "$environment"
compat_environment "$environment"
for state in empty zeros newline missing wrong duplicate trailing; do
  case "$state" in
    empty) : > "$environment"; expected=1;;
    zeros) printf '\000\000\000' > "$environment"; expected=1;;
    newline) printf '\n\000\n' > "$environment"; expected=2;;
    missing) printf 'PATH=/bin\000' > "$environment"; expected=2;;
    wrong) printf 'HOME=/wrong\000' > "$environment"; expected=2;;
    duplicate) printf 'HOME=%s\000HOME=%s\000' "$root/home" "$root/home" > "$environment"; expected=2;;
    trailing) printf 'HOME=%s\000broken' "$root/home" > "$environment"; expected=2;;
  esac
  rc=0; compat_environment "$environment" || rc=$?
  [ "$rc" = "$expected" ] || fail "environment state $state=$rc"
done
compat_home_bind probe --bind / / --setenv NOTE -- --bind "$root/work/appclone-probe-$(id -u)-123/merged" "$root/home" -- /bin/true
[ "$compat_home_source" = "$root/work/appclone-probe-$(id -u)-123/merged" ] || fail 'wrong merged parsed'
compat_home_bind run --bind / / --tmpfs "$root/base" -- /bin/true
expect_failure compat_home_bind probe --bind / / --bind /unrelated/merged "$root/home" -- /bin/true
expect_failure compat_home_bind probe --bind / / -- /bin/true --bind "$root/work/appclone-probe-$(id -u)-123/merged" "$root/home"
expect_failure compat_home_bind probe --bind / / --bind "$root/work/appclone-probe-$(id -u)-123/merged" "$root/home" --bind "$root/work/appclone-probe-$(id -u)-456/merged" "$root/home" -- /bin/true
expect_failure compat_home_bind run --bind / / --bind /other /tmp -- /bin/true
compat_same_view 'mnt:[1]' 'mnt:[1]' '2:3' '2:3'
expect_failure compat_same_view 'mnt:[1]' 'mnt:[2]' '2:3' '2:3'
expect_failure compat_same_view 'mnt:[1]' 'mnt:[1]' '2:3' '2:4'
(
  compat_environment() { return 1; }
  compat_bwrap_home() { return 1; }
  expect_failure compat_home_proof "$$" run "$$:1:2" '2:3:4'
)
echo 'PASS HOME environment states, exact binding, ancestor and namespace/view rejection'

# Unknown identity and PID reuse must never reach signal or wait.
(
  compat_inspect() { return 2; }
  kill() { : > "$root/control/bad-signal"; }
  wait() { : > "$root/control/bad-wait"; }
  expect_failure compat_stop_child 12345 1
)
(
  compat_inspect() { compat_pid_start=2; compat_pid_state=S; return 0; }
  kill() { : > "$root/control/bad-signal"; }
  wait() { : > "$root/control/bad-wait"; }
  expect_failure compat_stop_child 12345 1
)
[ ! -e "$root/control/bad-signal" ] && [ ! -e "$root/control/bad-wait" ] || fail 'unknown identity acted on'
sleep 30 & child=$!
started=$(compat_start "$child")
compat_stop_child "$child" "$started"
[ "$compat_child_rc" = 143 ] || fail 'TERM exit code lost'
echo 'PASS unknown/changed identity, bounded TERM and exit status'

printf 'keep\n' > "$root/control/outside"
ln -s "$root/control/missing-outside" "$root/control/services"
expect_failure timeout 8 bash "$driver" "$root" serve
[ ! -e "$root/control/missing-outside" ] || fail 'dangling service link followed'
rm -- "$root/control/services"
mkfifo "$root/control/stop-service"
expect_failure compat_new_file "$root/control/stop-service"
expect_failure timeout 8 bash "$driver" "$root" serve
[ -p "$root/control/stop-service" ] || fail 'FIFO replaced'
rm -- "$root/control/stop-service"
echo 'PASS dangling services and FIFO stop request refused'

# This CLI imitates only lifecycle status; it cannot mount or access an account.
fake_cli="$root/control/fake-cli"
cat > "$fake_cli" <<'CLI'
#!/usr/bin/env bash
set -euo pipefail
[ -z "${WORKBUDDY_USER_DATA_DIR:-}${WORKBUDDY_CONFIG_DIR:-}${CODEBUDDY_CONFIG_DIR:-}" ] || exit 9
control="$(dirname "$AC_BASE")/control"
case "$1" in
  add) exit 0;;
  run)
    case "$3" in
      fast0) exit 0;; fast7) exit 7;;
      hold*)
        trap 'exit 0' TERM
        while [ ! -f "$control/release.$3" ]; do sleep .1; done
        exit 0;;
    esac;;
esac
exit 2
CLI
chmod 700 "$fake_cli"
printf '%s\n' "$fake_cli" > "$root/control/cli"
bash "$driver" "$root" serve > "$root/control/service-test.log" 2>&1 &
service_pid=$!; service_start=$(compat_start "$service_pid")
await_file "$root/control/services"
compat_service
record_backup=$(cat "$root/control/services")
sleep .2 & dead_child=$!
dead_start=$(compat_start "$dead_child")
wait "$dead_child"
compat_lock
for variant in missing duplicate-role duplicate-pid bad-start exited trailing; do
  printf '%s\n' "$record_backup" > "$root/control/services"
  case "$variant" in
    missing) printf '%s\n' "$record_backup" | awk '$1 != "keyring"' > "$root/control/services";;
    duplicate-role) printf '%s\n' "$record_backup" | awk '$1 == "bus"' >> "$root/control/services";;
    duplicate-pid)
      printf '%s\n' "$record_backup" | awk '$1 == "bus" {p=$2;s=$3} $1 == "keyring" {$2=p;$3=s} {print}' > "$root/control/services";;
    bad-start) printf '%s\n' "$record_backup" | awk '$1 == "serve" {$3=$3 "9"} {print}' > "$root/control/services";;
    exited)
      printf '%s\n' "$record_backup" | awk -v p="$dead_child" -v s="$dead_start" '$1 == "serve" {$2=p;$3=s} {print}' > "$root/control/services";;
    trailing) printf 'invalid trailing record' >> "$root/control/services";;
  esac
  expect_failure compat_service
  printf '%s\n' "$record_backup" > "$root/control/services"
done
for exited_state in Z X x; do
  (
    compat_inspect() {
      compat_pid_start=$(awk -v p="$1" '$2 == p {print $3}' "$root/control/services")
      compat_pid_state=$exited_state
      return 0
    }
    expect_failure compat_service
  )
done
compat_service
compat_unlock
echo 'PASS complete unique live service identities and malformed EOF'
compat_new_file "$root/control/preparing.fixture"
expect_failure bash "$driver" "$root" stop-service
rm -- "$root/control/preparing.fixture"
echo 'PASS preparing record prevents service shutdown'

WORKBUDDY_USER_DATA_DIR=/must-not-inherit bash "$driver" "$root" add a
bash "$driver" "$root" run fast0
compat_active_records && fail 'successful fast action retained record'
rc=0; bash "$driver" "$root" run fast7 || rc=$?
[ "$rc" = 7 ] || fail 'nonzero fast exit lost'
compat_active_records || fail 'failed action erased its record'
for record in "$root"/control/supervisor.* "$root"/control/preparing.*; do
  [ -f "$record" ] || continue
  if [[ "$record" = *'/supervisor.'* ]]; then
    read -r child started _ < "$record"
    rc=0; compat_inspect "$child" || rc=$?
    [ "$rc" = 1 ] || fail 'failed action child still present'
  fi
  rm -- "$record"
done
echo 'PASS fast success/failure and environment whitelist'

bash "$driver" "$root" run hold1 > "$root/control/hold1.log" 2>&1 & first=$!
first_start=$(compat_start "$first")
bash "$driver" "$root" run hold2 > "$root/control/hold2.log" 2>&1 & second=$!
for ((attempt=0; attempt<80; attempt++)); do
  records=("$root"/control/supervisor.*)
  [ "${#records[@]}" != 2 ] || break
  sleep .1
done
[ "${#records[@]}" = 2 ] || fail 'concurrent actions lack separate records'
expect_failure bash "$driver" "$root" stop-service
compat_same "$first" "$first_start" || fail 'first driver changed'
kill -TERM "$first"
rc=0; wait "$first" || rc=$?
[ "$rc" = 143 ] || fail 'interrupted driver status lost'
records=("$root"/control/supervisor.*)
[ "${#records[@]}" = 2 ] || fail 'interrupted action erased record'
: > "$root/control/release.hold2"
wait "$second"
records=("$root"/control/supervisor.*)
[ "${#records[@]}" = 1 ] || fail 'successful concurrent action did not clean record'
read -r child started _ < "${records[0]}"
rc=0; compat_inspect "$child" || rc=$?
[ "$rc" = 1 ] || fail 'interrupted fake child remains'
rm -- "${records[0]}"
echo 'PASS concurrent records and exceptional retention'

bash "$driver" "$root" stop-service
expect_failure bash "$driver" "$root" add after-stop
for ((attempt=0; attempt<80; attempt++)); do
  rc=0; compat_inspect "$service_pid" || rc=$?
  [ "$rc" = 1 ] || [ "${compat_pid_state:-}" = Z ] || { sleep .1; continue; }
  break
done
[ "$rc" = 1 ] || [ "${compat_pid_state:-}" = Z ] || fail 'service shutdown timed out'
wait "$service_pid"
service_pid=
[ ! -e "$root/control/services" ] || fail 'service record remains'
echo 'PASS stop marker rejects new action; service cleanup complete'
