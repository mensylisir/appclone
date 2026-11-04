#!/usr/bin/env bash
# appclone — 无 root 的 Linux「应用分身」工具
#
# 设计目标: 只隔离目标应用自身的配置/数据目录, 使两个实例的数据互不可见;
# 其余一切(下载、文档、桌面会话、输入法、通知、GPU/音频等)全部共享。
#
# 实现原理: mount namespace (bubblewrap) + 按路径 bind mount。
#   --bind / /                    → 整个文件系统原样透传
#   --bind <实例私有目录> <数据路径>  → 仅偷换应用自己的数据目录
#
# 依赖: bubblewrap(bwrap); 可选: dbus-run-session(私有会话总线),
#       strace(路径发现), gio(移入回收站), update-desktop-database(桌面入口)
set -euo pipefail

AC_BASE="${AC_BASE:-${XDG_DATA_HOME:-$HOME/.local/share}/appclone}"
AC_PROFILES="$AC_BASE/profiles.d"
AC_INSTANCES="$AC_BASE/instances"
AC_DESKTOP_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
AC_SELF="$(readlink -f "$0")"

die()  { printf 'appclone: 错误: %s\n' "$*" >&2; exit 1; }
info() { printf 'appclone: %s\n' "$*"; }

usage() {
  cat <<'EOF'
用法: appclone <命令> [参数]

  profile-new <app>            生成应用档案模板 (profiles.d/<app>.sh)
  discover  <app>              用 strace 跟踪应用, 列出它在 ~ 下写入的路径
  add       <app> <id>         创建分身实例并生成桌面入口
  run       <app> <id> [args]  在分身环境中启动应用
  list      [app]              列出已注册应用与实例
  doctor    <app> <id>         检查环境并打印将执行的 bwrap 命令
  rm        <app> <id>         将实例移入回收站并删除桌面入口
EOF
  exit 1
}

require_bwrap() { command -v bwrap >/dev/null 2>&1 || die "需要 bubblewrap (bwrap)，请先安装"; }
valid_id()      { [[ $1 =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "非法实例名: '$1' (仅限字母数字._-)"; }

# ---------------------------------------------------------------------------
# 应用档案 (per-app 路径知识库)
# 相当于事后补做 Android PMS「安装时分配数据目录」的工作:
# 它唯一需要人工维护的东西, 记录"这个应用把数据写到哪、怎么检测单例"。
# ---------------------------------------------------------------------------
load_profile() {
  local app=$1 f="$AC_PROFILES/$app.sh"
  [[ -f $f ]] || die "未找到应用档案: $f (先运行: appclone profile-new $app)"

  # 默认值
  AC_EXEC=""
  AC_PATHS=()
  AC_LOCKFILES=()
  AC_EXTRA_BINDS=()
  AC_EXTRA_ENV=()
  AC_ISOLATE_DBUS=0
  AC_ISOLATE_RUNTIME=0
  AC_CWD="$HOME"

  # shellcheck source=/dev/null
  source "$f"
  [[ -n $AC_EXEC ]] || die "档案 $f 未定义 AC_EXEC"
}

# ---------------------------------------------------------------------------
# bwrap 参数构造
# ---------------------------------------------------------------------------
build_bwrap_args() {
  local app=$1 id=$2
  local inst="$AC_INSTANCES/$app/$id"
  local p src dst i=0 e

  BWRAP_ARGS=(
    --dev-bind /dev /dev      # 共享真实设备 (GPU/音频/输入)
    --proc /proc
    --bind / /                # 整个系统原样透传 —— "其余全部可见"的关键
    --chdir "$AC_CWD"
    --die-with-parent
  )

  # 1) 数据目录偷换: 实例私有目录 bind 到应用真实数据路径上
  for p in "${AC_PATHS[@]}"; do
    dst="$inst/root$p"
    mkdir -p "$dst"
    mkdir -p "$p" 2>/dev/null || true   # 宿主侧挂载点必须存在
    BWRAP_ARGS+=(--bind "$dst" "$p")
  done

  # 2) 锁文件偷换: 用实例私有的空文件盖住宿主锁文件 (inode 不同, flock 不再互斥)
  if ((${#AC_LOCKFILES[@]})); then mkdir -p "$inst/locks"; fi
  for p in "${AC_LOCKFILES[@]}"; do
    src="$inst/locks/$i.lock"
    : >"$src"
    mkdir -p "$(dirname "$p")" 2>/dev/null || true
    BWRAP_ARGS+=(--bind "$src" "$p")
    i=$((i + 1))
  done

  # 3) 额外 bind (格式: "源|目标", 用于 /var、/opt 下的写死路径等)
  for e in "${AC_EXTRA_BINDS[@]}"; do
    BWRAP_ARGS+=(--bind "${e%%|*}" "${e##*|}")
  done

  # 4) 私有 XDG_RUNTIME_DIR (wayland/pulse/bus socket 仍 bind 进去, 桌面功能不丢)
  if [[ $AC_ISOLATE_RUNTIME == 1 ]]; then
    local rt="$inst/runtime" real_rt s
    real_rt="${XDG_RUNTIME_DIR:?需要 XDG_RUNTIME_DIR}"
    mkdir -p "$rt"
    for s in wayland-0 wayland-1 pulse pulse/native bus dconf user; do
      [[ -e "$real_rt/$s" ]] && BWRAP_ARGS+=(--bind "$real_rt/$s" "$rt/$s")
    done
    export XDG_RUNTIME_DIR="$rt"
  fi
}

# ---------------------------------------------------------------------------
# 桌面入口生成 (相当于 Android Launcher 上的第二个图标)
# ---------------------------------------------------------------------------
find_desktop() {
  local bin d f
  bin="$(basename "$AC_EXEC")"
  for d in "$HOME/.local/share/applications" /usr/share/applications \
           /var/lib/flatpak/exports/share/applications \
           "$HOME/.local/share/flatpak/exports/share/applications"; do
    [[ -d $d ]] || continue
    for f in "$d"/*.desktop; do
      [[ -f $f ]] || continue
      if grep -qE "^Exec=.*(^| )$bin( |$)|^Exec=.*/$bin( |$)" "$f" 2>/dev/null; then
        echo "$f"; return 0
      fi
    done
  done
  return 1
}

generate_desktop() {
  local app=$1 id=$2
  local name="$app" icon="$app" orig out f
  orig="$(find_desktop || true)"
  if [[ -n $orig ]]; then
    name="$(awk -F= '/^Name=/{print $2; exit}' "$orig")"
    icon="$(awk -F== '/^Icon=/{print $2; exit}' "$orig")"
  fi
  mkdir -p "$AC_DESKTOP_DIR"
  out="$AC_DESKTOP_DIR/appclone-$app-$id.desktop"
  cat >"$out" <<EOF
[Desktop Entry]
Type=Application
Name=$name (分身:$id)
Exec="$AC_SELF" run $app $id %U
Icon=$icon
Terminal=false
Categories=Utility;
StartupWMClass=$app
EOF
  command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$AC_DESKTOP_DIR" 2>/dev/null || true
  echo "$out"
}

# ---------------------------------------------------------------------------
# 子命令
# ---------------------------------------------------------------------------
cmd_profile_new() {
  local app=$1
  valid_id "$app"
  mkdir -p "$AC_PROFILES"
  local f="$AC_PROFILES/$app.sh"
  [[ -f $f ]] && die "档案已存在: $f"
  cat >"$f" <<'EOF'
# === appclone 应用档案 ===
# 应用的可执行文件 (必填)
AC_EXEC=""

# 需要隔离(重定向)的目录列表 —— 按 `appclone discover` 的结果填写。
# 路径中的 $HOME 会在档案加载时展开。
AC_PATHS=(
  # "$HOME/.config/Example"
  # "$HOME/.local/share/Example"
)

# 应用用于互斥的锁文件/socket 路径 (会被替换为实例私有的空文件)
AC_LOCKFILES=(
  # "$HOME/.example.lock"
)

# 额外的 bind 挂载, 格式 "源|目标" (用于 /var、/opt 下写死的路径)
AC_EXTRA_BINDS=(
  # "/var/lib/example|/var/lib/example"
)

# 是否给分身私有的 DBus 会话总线 (绕过 well-known name 单例检测)
# 副作用: 分身内无法使用依赖会话总线的托盘/通知等桌面集成, 按需开启
AC_ISOLATE_DBUS=0

# 是否给分身私有的 XDG_RUNTIME_DIR (wayland/pulse/bus socket 仍会共享进来)
AC_ISOLATE_RUNTIME=0

# 额外环境变量 ("VAR=value")
AC_EXTRA_ENV=(
  # "EXAMPLE_MODE=clone"
)

# 启动时的工作目录
AC_CWD="$HOME"
EOF
  info "已生成档案模板: $f"
  info "下一步: 1) 填写 AC_EXEC  2) appclone discover $app  3) 把写入路径填进 AC_PATHS"
}

cmd_discover() {
  local app=$1
  load_profile "$app"
  command -v strace >/dev/null 2>&1 || die "需要 strace，请先安装"
  local log; log="$(mktemp /tmp/appclone-strace.XXXXXX)"
  info "将以「主实例」方式跟踪: $AC_EXEC"
  info "在应用里把各类功能尽量用一遍(收发文件/截图/改设置), 然后退出。"
  warn_=""
  printf '%s\n' "  注意: 此过程应用本身未沙箱运行, 会正常写主实例数据。"
  strace -f -qq -e trace=%file -o "$log" -- "$AC_EXEC" || true
  # 只保留带写标志的 open 和 mkdir/rename/unlink 等行为中的 $HOME 绝对路径
  grep -E 'O_WRONLY|O_RDWR|O_CREAT|O_TRUNC|mkdir\(|rename\(|unlink\(|renameat|symlink\(' "$log" 2>/dev/null \
    | grep -oE '"[^"]+"' | tr -d '"' \
    | grep -v '^$' | sort -u | grep "^$HOME" | grep -vE "^$HOME/(\.cache/)" \
    || true
  info "────"
  info "把上面属于『应用自身状态』的路径(去掉随机文件名, 保留其父目录)填入档案 AC_PATHS:"
  info "  $AC_PROFILES/$app.sh"
  info "注意甄别: Downloads/图片/文档等公共目录出现在列表里属于正常共享行为, 不要隔离。"
  rm -f "$log"
}

cmd_add() {
  local app=$1 id=$2
  valid_id "$app"; valid_id "$id"
  require_bwrap; load_profile "$app"
  local inst="$AC_INSTANCES/$app/$id" p
  [[ -e $inst ]] && die "实例已存在: $app/$id"
  for p in "${AC_PATHS[@]}"; do mkdir -p "$inst/root$p"; done
  mkdir -p "$inst/locks" "$inst/runtime"
  local desk; desk="$(generate_desktop "$app" "$id")"
  info "已创建分身: $app/$id"
  info "实例数据目录: $inst/root"
  info "桌面入口: $desk"
  info "启动: appclone run $app $id   (或从应用列表里点「<app> (分身:$id)」)"
}

cmd_run() {
  local app=$1 id=$2; shift 2
  require_bwrap; load_profile "$app"
  local inst="$AC_INSTANCES/$app/$id"
  [[ -d $inst ]] || die "实例不存在: $app/$id (先运行: appclone add $app $id)"
  build_bwrap_args "$app" "$id"

  local kv
  for kv in "${AC_EXTRA_ENV[@]}"; do export "$kv"; done

  if [[ $AC_ISOLATE_DBUS == 1 ]] && command -v dbus-run-session >/dev/null 2>&1; then
    exec dbus-run-session -- bwrap "${BWRAP_ARGS[@]}" -- "$AC_EXEC" "$@"
  else
    exec bwrap "${BWRAP_ARGS[@]}" -- "$AC_EXEC" "$@"
  fi
}

cmd_list() {
  local app=${1:-} f inst
  printf '%-16s %-12s %s\n' "APP" "实例" "数据目录"
  for f in "$AC_PROFILES"/*.sh; do
    [[ -f $f ]] || continue
    local a; a="$(basename "$f" .sh)"
    [[ -n $app && $a != "$app" ]] && continue
    local found=0
    for inst in "$AC_INSTANCES/$a"/*; do
      [[ -d $inst ]] || continue
      printf '%-16s %-12s %s\n' "$a" "$(basename "$inst")" "$inst/root"
      found=1
    done
    ((found)) || printf '%-16s %-12s %s\n' "$a" "-" "(无实例)"
  done
}

cmd_doctor() {
  local app=$1 id=$2
  require_bwrap; load_profile "$app"
  [[ -d "$AC_INSTANCES/$app/$id" ]] || die "实例不存在: $app/$id"
  build_bwrap_args "$app" "$id"
  info "将执行的命令:"
  printf '  bwrap'; printf ' %q' "${BWRAP_ARGS[@]}"; printf ' -- %q' "$AC_EXEC"; printf '\n'
  info "如遇问题排查顺序: 1)档案路径是否齐(重跑 discover) 2)应用是否检测单例(开 AC_ISOLATE_DBUS / 填 AC_LOCKFILES) 3)是否写了 $HOME 外的路径(填 AC_EXTRA_BINDS)"
}

cmd_rm() {
  local app=$1 id=$2
  local inst="$AC_INSTANCES/$app/$id"
  [[ -d $inst ]] || die "实例不存在: $app/$id"
  local desk="$AC_DESKTOP_DIR/appclone-$app-$id.desktop"
  rm -f "$desk" 2>/dev/null || true
  if command -v gio >/dev/null 2>&1; then
    gio trash -- "$inst"
  else
    mkdir -p "$AC_BASE/.trash"
    mv -n "$inst" "$AC_BASE/.trash/$(date +%s)-$app-$id"
  fi
  info "已将实例 $app/$id 移入回收站, 并删除桌面入口"
}

# ---------------------------------------------------------------------------
main() {
  (($# >= 1)) || usage
  local cmd=$1; shift
  case $cmd in
    profile-new) (($# == 1)) || usage; cmd_profile_new "$1" ;;
    discover)     (($# == 1)) || usage; cmd_discover "$1" ;;
    add)          (($# == 2)) || usage; cmd_add "$1" "$2" ;;
    run)          (($# >= 2)) || usage; cmd_run "$@" ;;
    list)         (($# <= 1)) || usage; cmd_list "${1:-}" ;;
    doctor)       (($# == 2)) || usage; cmd_doctor "$1" "$2" ;;
    rm)           (($# == 2)) || usage; cmd_rm "$1" "$2" ;;
    *)            usage ;;
  esac
}
main "$@"
