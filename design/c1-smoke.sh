#!/usr/bin/env bash
# c1-smoke.sh —— C1（AC_BASE 遮蔽 + 私有总线）在真机上的隔离冒烟。
#
# 全程假 HOME / AC_BASE / XDG_RUNTIME_DIR，不碰真实主目录。四种布局：
#   1) 普通短路径：AC_BASE 被 tmpfs 遮蔽、公共目录共享、私有总线禁服务激活，
#      宿主 /tmp 总线被 /dev/null 遮蔽；
#   2) 超长 AC_BASE：socket 路径超过 dbus-daemon 上限时走短回退目录；
#   3) 宿主总线地址是软链别名：必须先解析成真实 socket 再遮蔽（bwrap 拒绝
#      在最终分量是软链的目标上挂载，否则整条启动失败）；
#   4) dbus-daemon 不可用 + isolate_runtime：必须降级为共享宿主总线
#      （宿主 bus 绑回私有 runtime），而不是让应用失去 DBus。
#
# 用法：design/c1-smoke.sh /path/to/appclone
set -euo pipefail

CLI=${1:?用法: c1-smoke.sh /path/to/appclone}
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/appclone-c1-XXXXXX")
HOSTBUS_PID=""
cleanup() {
  if [ -n "$HOSTBUS_PID" ]; then
    kill "$HOSTBUS_PID" 2>/dev/null || true
    wait "$HOSTBUS_PID" 2>/dev/null || true
  fi
  rm -rf "$ROOT"
}
trap cleanup EXIT
mkdir -p "$ROOT/bin"
cp "$CLI" "$ROOT/bin/appclone"
export PATH="$ROOT/bin:$PATH"
CLI="$ROOT/bin/appclone"

# start_host_bus <sock> <addrfile>：起一个宿主会话总线（模拟 dbus-launch），
# 把 --print-address 原文写进 addrfile。调用方负责 stop_host_bus。
start_host_bus() {
  local sock=$1 addrfile=$2
  dbus-daemon --session --address="unix:path=$sock" --nofork --print-address \
    >"$addrfile" 2>/dev/null &
  HOSTBUS_PID=$!
  for _ in $(seq 1 50); do [ -S "$sock" ] && return 0; sleep 0.1; done
  return 1
}

stop_host_bus() {
  if [ -n "$HOSTBUS_PID" ]; then
    kill "$HOSTBUS_PID" 2>/dev/null || true
    wait "$HOSTBUS_PID" 2>/dev/null || true
    HOSTBUS_PID=""
  fi
}

run_case() {
  local name=$1 base=$2 style=${3:-direct}
  local H="$ROOT/$name/home" X="$ROOT/$name/xdg"
  mkdir -p "$H/Downloads" "$H/.config" "$H/.local/share/applications" \
           "$H/.local/state" "$H/.cache" "$ROOT/$name/run" "$base"
  chmod 700 "$ROOT/$name/run"
  printf 'XDG_DOWNLOAD_DIR="$HOME/Downloads"\n' >"$H/.config/user-dirs.dirs"

  # 宿主总线放在 /tmp 形态的路径（不在 runtime dir 内）：私有总线在跑时
  # 必须被 /dev/null 遮住，应用不能绕过它连回宿主。
  start_host_bus "$ROOT/$name/hostbus" "$ROOT/$name/hostbus.addr" || {
    echo "[$name] 宿主总线没起来" >&2; exit 20; }
  local hostaddr; hostaddr=$(head -1 "$ROOT/$name/hostbus.addr")
  if [ "$style" = symlink ]; then
    # 宿主总线地址经软链别名给出：hostBusPaths 必须先 EvalSymlinks 成真实
    # socket 再遮蔽，否则 bwrap 报 "Can't mount on symlink destination"，
    # 整条 run 失败。沙箱里别名仍指向真实路径，遮蔽失效时 hostbus=reachable。
    ln -s "$ROOT/$name/hostbus" "$ROOT/$name/hostbus-alias"
    hostaddr="unix:path=$ROOT/$name/hostbus-alias"
  fi

  cat >"$ROOT/$name/checkapp" <<'EOS'
#!/bin/sh
out="$XDG_CONFIG_HOME/C1Demo/out.txt"
mkdir -p "$XDG_CONFIG_HOME/C1Demo"
{
  echo "base_sentinel=$(test -e "$AC_BASE/sentinel" && echo yes || echo no)"
  echo "base_entries=$(ls -A "$AC_BASE" 2>/dev/null | wc -l)"
  echo "downloads=$(test -d "$HOME/Downloads" && echo yes || echo no)"
  echo "dbus=$DBUS_SESSION_BUS_ADDRESS"
  echo "runtime=$XDG_RUNTIME_DIR"
  if dbus-send --bus="$HOST_BUS_ADDR" --print-reply --dest=org.freedesktop.DBus \
      /org/freedesktop/DBus org.freedesktop.DBus.NameHasOwner \
      string:org.freedesktop.DBus >/dev/null 2>&1; then
    echo "hostbus=reachable"
  else
    echo "hostbus=blocked"
  fi
  dbus-send --bus="$DBUS_SESSION_BUS_ADDRESS" --print-reply \
    --dest=org.freedesktop.DBus /org/freedesktop/DBus \
    org.freedesktop.DBus.NameHasOwner string:org.freedesktop.secrets 2>&1 | tail -1
} >"$out" 2>&1
echo "state" >>"$XDG_CONFIG_HOME/C1Demo/state"
exit 0
EOS
  chmod +x "$ROOT/$name/checkapp"
  cat >"$H/.local/share/applications/c1demo.desktop" <<EOS
[Desktop Entry]
Type=Application
Name=C1 Demo
Exec=$ROOT/$name/checkapp
Categories=Utility;
EOS
  printf 'sentinel\n' >"$base/sentinel"
  mkdir -p "$base/profiles.d"
  cat >"$base/profiles.d/c1demo.toml" <<EOS
version = 1
exec = "$ROOT/$name/checkapp"
paths = ["~/.config/C1Demo"]
isolate_dbus = true
isolate_runtime = true
cwd = "~"
probe_method = "manual"

[env]
AC_BASE = "$base"
HOST_BUS_ADDR = "$hostaddr"
EOS

  local envs=(HOME="$H" XDG_CONFIG_HOME="$H/.config" XDG_DATA_HOME="$H/.local/share"
              XDG_STATE_HOME="$H/.local/state" XDG_CACHE_HOME="$H/.cache"
              XDG_RUNTIME_DIR="$ROOT/$name/run" AC_BASE="$base"
              DBUS_SESSION_BUS_ADDRESS="$hostaddr")
  echo "== [$name] add（已有档案，不探测）"
  env "${envs[@]}" "$CLI" add c1demo main >/dev/null

  echo "== [$name] run"
  env "${envs[@]}" "$CLI" run c1demo main

  local inst="$base/instances/c1demo/main"
  local out; out=$(find "$inst/root" -path '*C1Demo/out.txt' -type f | head -1)
  [ -n "$out" ] || { echo "[$name] 沙箱输出没落进实例：$inst/root" >&2; find "$inst" -type f >&2; exit 10; }
  grep -q '^base_sentinel=no$' "$out" || { echo "[$name] AC_BASE 没被遮蔽：" >&2; cat "$out" >&2; exit 11; }
  grep -q '^base_entries=0$' "$out" || { echo "[$name] AC_BASE 里仍能看到条目：" >&2; cat "$out" >&2; exit 11; }
  grep -q '^downloads=yes$' "$out" || { echo "[$name] 公共目录不可见：" >&2; cat "$out" >&2; exit 12; }
  grep -q '^dbus=unix:path=' "$out" || { echo "[$name] 私有总线地址没注入：" >&2; cat "$out" >&2; exit 13; }
  grep -q '^hostbus=blocked$' "$out" || { echo "[$name] 应用仍能连上宿主总线（B5 遮蔽失效）：" >&2; cat "$out" >&2; exit 13; }
  grep -q 'boolean false' "$out" || { echo "[$name] secrets 服务被激活了（应 ServiceUnknown/无 owner）：" >&2; cat "$out" >&2; exit 13; }
  # 宿主上不得出现应用数据
  if [ -n "$(find "$H/.config/C1Demo" -mindepth 1 2>/dev/null)" ]; then
    echo "[$name] 宿主配置目录被写入：" >&2; find "$H/.config/C1Demo" -mindepth 1 >&2; exit 14
  fi
  # 长路径必须走短回退目录，且 Stop 后不留目录
  if [ "$name" = long ]; then
    grep -q 'appclone-bus-' "$inst/dbus/session.conf" || {
      echo "[$name] 长路径没有回退到短 socket 目录：" >&2; cat "$inst/dbus/session.conf" >&2; exit 15; }
    local left; left=$(find "$ROOT/$name/run" -maxdepth 1 -name 'appclone-bus-*' 2>/dev/null | wc -l)
    [ "$left" = 0 ] || { echo "[$name] 回退目录没清理：" >&2; find "$ROOT/$name/run" -maxdepth 1 -name 'appclone-bus-*' >&2; exit 15; }
  fi
  stop_host_bus
  echo "[$name] OK"
}

# 1) 正常短路径
run_case short "$ROOT/base-short"

# 2) AC_BASE 很长（socket 路径超 dbus-daemon 上限）→ 必须走短回退目录
LONG="$ROOT/$(printf 'deepdir-%.0s' $(seq 1 12))base"
run_case long "$LONG"

# 3) 宿主总线地址是软链别名 → 必须解析后遮蔽，run 不能因 "Can't mount on
#    symlink destination" 失败，且 hostbus 必须仍为 blocked。
run_case symlink "$ROOT/base-symlink" symlink

# 4) 私有总线起不来 + isolate_runtime：必须降级为共享宿主总线（宿主 bus 绑回
#    私有 runtime），而不是让应用拿着指向空目录的地址失去 DBus。
degrade_case() {
  local name=degrade
  local H="$ROOT/$name/home" base="$ROOT/base-degrade" RT="$ROOT/$name/run"
  mkdir -p "$H/Downloads" "$H/.config" "$H/.local/share/applications" \
           "$H/.local/state" "$H/.cache" "$RT" "$base/profiles.d" "$ROOT/$name/fakebin"
  chmod 700 "$RT"
  printf 'XDG_DOWNLOAD_DIR="$HOME/Downloads"\n' >"$H/.config/user-dirs.dirs"
  # 宿主会话总线：应用降级后应该连上它
  start_host_bus "$RT/bus" "$ROOT/$name/hostbus.addr" || {
    echo "[$name] 宿主总线没起来" >&2; exit 20; }
  local hostaddr; hostaddr=$(head -1 "$ROOT/$name/hostbus.addr")
  # 让 appclone 找不到真正的 dbus-daemon（模拟缺失）
  printf '#!/bin/sh\nexit 1\n' >"$ROOT/$name/fakebin/dbus-daemon"
  chmod +x "$ROOT/$name/fakebin/dbus-daemon"
  cat >"$ROOT/$name/checkapp" <<'EOS'
#!/bin/sh
out="$XDG_CONFIG_HOME/C1Demo/out.txt"
mkdir -p "$XDG_CONFIG_HOME/C1Demo"
{
  echo "dbus=$DBUS_SESSION_BUS_ADDRESS"
  dbus-send --bus="$DBUS_SESSION_BUS_ADDRESS" --print-reply \
    --dest=org.freedesktop.DBus /org/freedesktop/DBus \
    org.freedesktop.DBus.NameHasOwner string:org.freedesktop.DBus 2>&1 | tail -1
} >"$out" 2>&1
exit 0
EOS
  chmod +x "$ROOT/$name/checkapp"
  cat >"$H/.local/share/applications/c1demo.desktop" <<EOS
[Desktop Entry]
Type=Application
Name=C1 Demo
Exec=$ROOT/$name/checkapp
Categories=Utility;
EOS
  cat >"$base/profiles.d/c1demo.toml" <<EOS
version = 1
exec = "$ROOT/$name/checkapp"
paths = ["~/.config/C1Demo"]
isolate_dbus = true
isolate_runtime = true
cwd = "~"
probe_method = "manual"
EOS
  local envs=(HOME="$H" XDG_CONFIG_HOME="$H/.config" XDG_DATA_HOME="$H/.local/share"
              XDG_STATE_HOME="$H/.local/state" XDG_CACHE_HOME="$H/.cache"
              XDG_RUNTIME_DIR="$RT" AC_BASE="$base" DBUS_SESSION_BUS_ADDRESS="$hostaddr")
  echo "== [$name] add/run（dbus-daemon 不可用）"
  env "${envs[@]}" "$CLI" add c1demo main >/dev/null
  env "${envs[@]}" PATH="$ROOT/$name/fakebin:$PATH" "$CLI" run c1demo main 2>&1 | tail -1
  local out; out=$(find "$base/instances/c1demo/main/root" -path '*C1Demo/out.txt' -type f | head -1)
  [ -n "$out" ] || { echo "[$name] 没有输出" >&2; exit 21; }
  grep -q 'boolean true' "$out" || { echo "[$name] 降级后应用连不上宿主总线：" >&2; cat "$out" >&2; exit 21; }
  stop_host_bus
  echo "[$name] OK"
}
degrade_case

echo
echo "C1 冒烟全部通过"
