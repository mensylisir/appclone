#!/usr/bin/env bash
# probe-smoke.sh —— 在**你自己的账号**下验证 L1 overlay 探测（不需要 root）。
#
# 为什么需要这个脚本：开发沙箱里没有 /dev/fuse，overlay 挂不起来，所以探测的
# 挂载/会话部分只能在真机上验。这个脚本用假 HOME 走完整条链路：
#   挂载 → 会话 → 收集 upper → 归并 → 落档案 → 建实例 → 删除
# 并且专门验证三件最容易做错的事：
#   ① 公共目录摘出真的生效（往 ~/Downloads 存一个哨兵文件，探测后必须还在）
#   ② 探测结束后没有残留挂载点（残留会让下一次探测 EBUSY）
#   ③ 审计层跑完后 verify 真能读到证据并给出结论（而不是永远 unverified）
#
# 用法：design/probe-smoke.sh [appclone 可执行文件路径]
set -euo pipefail

CLI=${1:-$(command -v appclone || true)}
[ -n "$CLI" ] && [ -x "$CLI" ] || { echo "用法：$0 /path/to/appclone" >&2; exit 2; }
[ "$(id -u)" != "0" ] || { echo "请在普通账号下运行（appclone 拒绝 root）" >&2; exit 2; }

for c in bwrap fuse-overlayfs; do
  command -v "$c" >/dev/null || { echo "缺少 $c" >&2; exit 3; }
done
command -v fusermount3 >/dev/null || command -v fusermount >/dev/null || { echo "缺少 fusermount3" >&2; exit 3; }
[ -r /dev/fuse ] || { echo "打不开 /dev/fuse" >&2; exit 3; }

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/appclone-smoke-XXXXXX")
cleanup() { rm -rf "$ROOT"; }
trap cleanup EXIT

export HOME="$ROOT/home"
# XDG 根必须落在假 HOME 之内：探测的 overlay 只罩 $HOME，写在它外面的
# "XDG 目录"根本不会进 upper，归因结果必然是零写入（真实桌面上这些目录
# 默认就在 $HOME 下；把 fixture 放到外面只会验出一个假失败）。
export XDG_CONFIG_HOME="$HOME/.config"
export XDG_DATA_HOME="$HOME/.local/share"
export XDG_STATE_HOME="$HOME/.local/state"
export XDG_CACHE_HOME="$HOME/.cache"
export XDG_RUNTIME_DIR="$ROOT/run"
export AC_BASE="$ROOT/appclone-base"
mkdir -p "$HOME/Downloads" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME/applications" \
         "$XDG_STATE_HOME" "$XDG_CACHE_HOME" "$XDG_RUNTIME_DIR" "$AC_BASE"
chmod 700 "$XDG_RUNTIME_DIR"
# 与 core.DefaultTmpRoot 同一规则（ADR-033）：/tmp 可写就用它，否则退 runtime dir。
# 残留检查与审计断言必须跟着同一个根，否则会在错误的目录里"全绿"。
if [ -w /tmp ]; then WORKROOT=/tmp
elif [ -w "$XDG_RUNTIME_DIR" ]; then WORKROOT="$XDG_RUNTIME_DIR"
else WORKROOT=/tmp; fi

# 公共目录清单：探测不允许回退默认名，必须有一份可读的 user-dirs.dirs。
cat >"$XDG_CONFIG_HOME/user-dirs.dirs" <<'EOS'
XDG_DOWNLOAD_DIR="$HOME/Downloads"
EOS

# 一个"应用"：写自己的配置，同时在 Downloads 里存一个哨兵文件。
cat >"$ROOT/fakeapp" <<'EOS'
#!/bin/sh
mkdir -p "$XDG_CONFIG_HOME/ProbeDemo"
printf 'v1\n' >>"$XDG_CONFIG_HOME/ProbeDemo/config.ini"
# 审计要看到"范围内的写入"才会给 verified（空集按 §6.5 只算 unverified）：
# 缓存是 ignored 类别，正好是合法噪声。
mkdir -p "$XDG_CACHE_HOME/ProbeDemo"
printf 'noise\n' >>"$XDG_CACHE_HOME/ProbeDemo/cache.bin"
printf 'user file\n' >"$HOME/Downloads/probe-sentinel.txt"
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

echo "== 1) doctor"
out=$("$CLI" doctor) || { echo "doctor 自己失败了" >&2; exit 3; }
printf '%s\n' "$out" | sed -n '1,12p'
printf '%s\n' "$out" | grep -q '✓  fuse' || { echo "doctor 说 fuse 不可用，先解决它" >&2; exit 3; }

echo
echo "== 2) add probedemo（首次配置 = 跑一次探测）"
if ! "$CLI" add probedemo main; then
  echo "探测失败（见上面的错误码与原因）。" >&2
  exit 4
fi

echo
echo "== 3) 档案内容（应有 ProbeDemo、probe_method = overlay、exec 非空）"
prof="$AC_BASE/profiles.d/probedemo.toml"
grep -q 'ProbeDemo' "$prof" || { echo "档案里没有探测到的路径：" >&2; cat "$prof" >&2; exit 5; }
grep -q 'probe_method = "overlay"' "$prof" || { echo "probe_method 不是 overlay：" >&2; cat "$prof" >&2; exit 5; }
grep -q '^exec = ' "$prof" || { echo "档案缺 exec：探测产物过不了校验" >&2; exit 5; }
grep -E 'probe_method|^exec |ProbeDemo' "$prof"

echo
echo "== 4) 公共目录摘出是否真的生效（哨兵文件必须还在真实 Downloads 里）"
if [ ! -f "$HOME/Downloads/probe-sentinel.txt" ]; then
  echo "哨兵文件不见了：公共目录没有被 bind 回真实位置，探测会话的写入落进了 upper。" >&2
  echo "这些文件可能已被删除，立即停止并检查 /var/tmp/appclone-quarantine-*。" >&2
  exit 6
fi

echo
echo "== 5) 探测结束后不许有残留挂载点/写层"
left=$(find "$WORKROOT" -maxdepth 1 -name "appclone-probe-$(id -u)-*" 2>/dev/null | wc -l)
[ "$left" = "0" ] || { echo "有 $left 个残留目录：" >&2; find "$WORKROOT" -maxdepth 1 -name "appclone-probe-$(id -u)-*" >&2; exit 7; }
mountinfo=$(grep -c "appclone-probe-$(id -u)-" /proc/self/mountinfo || true)
[ "$mountinfo" = "0" ] || { echo "还有挂载点没卸载：" >&2; grep "appclone-probe-$(id -u)-" /proc/self/mountinfo >&2; exit 7; }

echo
echo "== 6) 实例建出来了，且数据目录在实例下"
"$CLI" list | sed -n '1,4p'
inst=$(find "$AC_BASE/instances/probedemo" -maxdepth 1 -mindepth 1 -type d | head -1)
[ -n "$inst" ] || { echo "没有建出实例目录" >&2; exit 8; }
[ -d "$inst/root" ] || { echo "实例里没有 root/（持久写层）" >&2; exit 8; }

echo
echo "== 7) run --audit：审计层 → 证据 → 结论"
# flag 必须写在位置参数之前：run 关了 flag 交错解析，`--audit` 放在
# probedemo/main 之后会被当成透传给应用的参数（§6 语法边界）。
"$CLI" run --audit probedemo main || exit 9
auditdir="$inst/audit"
[ -f "$auditdir/run.toml" ] || { echo "审计结束后应留下 run.toml（正常跑完的凭证）" >&2; exit 10; }
workdir="$WORKROOT/appclone/audit.$(id -u)/probedemo.main"
[ -d "$workdir/upper" ] || { echo "审计写层不在预期位置：$workdir/upper" >&2; exit 10; }

echo
echo "== 7b) 隔离语义：应用写入只在实例里，宿主不留数据"
# run 把档案 paths 绑到实例 root；应用写的就是自己的配置目录。
grep -rq '^v1$' "$inst/root" || { echo "实例里没有找到应用写入（$inst/root）" >&2; exit 13; }
# 宿主上的同名目录只是 bind 目标的空占位，不得含应用数据。
if [ -n "$(find "$HOME/.config/ProbeDemo" -mindepth 1 2>/dev/null)" ]; then
  echo "宿主 ~/.config/ProbeDemo 里出现了应用数据：" >&2
  find "$HOME/.config/ProbeDemo" -mindepth 1 >&2
  exit 13
fi

echo
echo "== 8) doctor --verify：判读审计证据"
if ! "$CLI" doctor --verify probedemo main; then
  echo "verify 退出码非 0：对照上面的结论判断是越界（8）还是摘出失效（4）。" >&2
  exit 11
fi
[ ! -d "$workdir/upper" ] || { echo "verified 之后写层应被消费掉" >&2; exit 11; }
[ -f "$auditdir/summary.toml" ] || { echo "verify 应留下 summary.toml" >&2; exit 11; }

echo
echo "== 9) 删除分身（走回收站白名单）"
"$CLI" rm probedemo main || exit 12

echo
echo "全部通过：探测 → 档案 → 实例 → 审计 → verify → 清理 都在真机上跑通了。"
