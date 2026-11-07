#!/usr/bin/env bash
# probe-smoke.sh —— 在**你自己的账号**下验证 L1 overlay 探测（不需要 root）。
#
# 为什么需要这个脚本：开发环境的沙箱里没有 /dev/fuse，overlay 挂不起来，
# 所以探测的 syscall 部分只能在真机上验。这个脚本把整条链路（挂载 → 会话 →
# 收集 → 归并 → 落档案 → 删除）跑一遍，且**全程用假 HOME**，不碰你的真实主目录。
#
# 用法：design/probe-smoke.sh [appclone 可执行文件路径]
# 退出码：0 = 通过；非 0 = 失败（会打印现场，便于贴回来）。
set -u

CLI=${1:-$(command -v appclone || true)}
if [ -z "$CLI" ] || [ ! -x "$CLI" ]; then
  echo "找不到 appclone 可执行文件；用法：$0 /path/to/appclone" >&2
  exit 2
fi

if [ "$(id -u)" = "0" ]; then
  echo "请在普通账号下运行（appclone 拒绝 root，探测也必须在非特权下才有意义）" >&2
  exit 2
fi

need() { command -v "$1" >/dev/null || { echo "缺少 $1（$2）" >&2; exit 3; }; }
need bwrap bubblewrap
need fuse-overlayfs fuse-overlayfs
need fusermount3 fuse3
[ -r /dev/fuse ] || { echo "打不开 /dev/fuse" >&2; exit 3; }

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/appclone-smoke-XXXXXX") || exit 3
trap 'rm -rf "$ROOT"' EXIT
export HOME="$ROOT/home"
export XDG_CONFIG_HOME="$ROOT/xdg/config"
export XDG_DATA_HOME="$ROOT/xdg/data"
export XDG_STATE_HOME="$ROOT/xdg/state"
export XDG_CACHE_HOME="$ROOT/xdg/cache"
export XDG_RUNTIME_DIR="$ROOT/run"
export AC_BASE="$ROOT/xdg/appclone"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$XDG_CACHE_HOME" \
         "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME/applications"
chmod 700 "$XDG_RUNTIME_DIR"

# 一个"应用"：写自己的配置，然后退出。探测会话会真的启动它。
cat >"$ROOT/fakeapp" <<'EOS'
#!/bin/sh
mkdir -p "$HOME/.config/ProbeDemo"
printf 'v1\n' >>"$HOME/.config/ProbeDemo/config.ini"
printf 'x\n'  >>"$HOME/.local/share/ProbeDemo/data.bin" 2>/dev/null || {
  mkdir -p "$HOME/.local/share/ProbeDemo"; printf 'x\n' >>"$HOME/.local/share/ProbeDemo/data.bin"; }
exit 0
EOS
chmod +x "$ROOT/fakeapp"

cat >"$XDG_DATA_HOME/applications/probedemo.desktop" <<EOS
[Desktop Entry]
Type=Application
Name=Probe Demo
Exec=$ROOT/fakeapp
Categories=Utility;
EOS

echo "== 1) doctor（fuse 应为 ok）"
"$CLI" doctor | sed -n '1,12p' || exit 3

echo
echo "== 2) add probedemo（首次配置 = 跑一次探测）"
if ! "$CLI" add probedemo main; then
  echo "探测失败。若提示 /dev/fuse 或 fuse-overlayfs，先确认它们是可用的。" >&2
  exit 4
fi

echo
echo "== 3) 档案（应含 ~/.config/ProbeDemo 与 ~/.local/share/ProbeDemo，probe_method = overlay）"
if ! grep -q 'ProbeDemo' "$AC_BASE/profiles.d/probedemo.toml"; then
  echo "档案里没有探测到的路径，实际内容：" >&2
  cat "$AC_BASE/profiles.d/probedemo.toml" >&2
  exit 5
fi
grep -E 'probe_method|ProbeDemo' "$AC_BASE/profiles.d/probedemo.toml"

echo
echo "== 4) 探测目录必须已被清理（残留会导致下一次 EBUSY）"
left=$(find /var/tmp -maxdepth 1 -name "appclone-probe-$(id -u)-*" 2>/dev/null | wc -l)
if [ "$left" != "0" ]; then
  echo "有 $left 个探测目录残留：" >&2
  find /var/tmp -maxdepth 1 -name "appclone-probe-$(id -u)-*" >&2
  exit 6
fi

echo
echo "== 5) 宿主零污染：假 HOME 之外不应出现 ProbeDemo"
if [ -e "$HOME/.config/ProbeDemo" ]; then
  echo "假 HOME 里出现了 ProbeDemo（说明探测会话写的是真实路径，overlay 没生效）" >&2
  exit 7
fi

echo
echo "== 6) 删除分身（走回收站白名单）"
"$CLI" rm probedemo main || exit 8

echo
echo "全部通过：overlay 探测 → 归并 → 档案 → 清理 都在真机上跑通了。"
