#!/usr/bin/env bash
# guitest-gtk.sh —— GTK 侧回归（真实 GTK/libadwaita，不用合成输入）。
#
# 与 design/guitest.js 的分工：那个验 HTML 设计稿的交互语义（项数见其 EXPECTED），
# 这个验 **GTK 实现**的页面生命周期与回调路径：列表渲染 → 配置确认 →
# 创建实例 → 详情 → 返回，全程走产品自己的信号处理函数。
#
# 输入路径分两层：
#   - 不开 /dev/uinput 时（CI 的 GitHub runner 就没有），用例直调产品回调：
#     `SwitchRow.SetActive` 发 notify::active 进 onSwitch，确认按钮与
#     v.submit() 绑的是同一个处理函数。这验的是"接线"，不是合成点击。
#   - Wayland 会话且 /dev/uinput 可写时（无需 root；ydotool 同款机制），
#     额外跑 TestGtkRealInputUinput：真实触摸点切换器、真实按键输入搜索框，
#     事件走 libinput → GDK → 控件。窗口在用例期间全屏，结束即恢复。
#     X11/Xvfb 下不启用：事件进宿主 seat，可能落到别的应用上。
#   - 第三层：调用者先手动把桌面输入法切到中文，并额外给 APPCLONE_GTK_IME=1，
#     才跑 TestGtkRealInputChineseIME（拼音 → 空格提交候选 → 逐字退格）。
#     门禁不读取也不改写用户 IME 配置；没给该变量时按 SKIP 校验，给了则必须
#     PASS。显式请求真实输入但环境不满足时，闸门以退出码 5 失败，不把 SKIP
#     记成通过。
#
# 注意：这个闸门在当前桌面**真的开窗口**（uinput 用例还会全屏并抢焦点）。
# 不要在有用户操作的会话里循环重跑；无人值守时用 xvfb-run——未显式 opt-in 时
# 真实输入自动 SKIP，显式给出 APPCLONE_GTK_UINPUT=1/APPCLONE_GTK_IME=1 但环境
# 不满足时闸门以退出码 5 失败，不会把降级记成通过。
#
# 需要可用的显示与宿主 `bwrap`（缺任何一个都直接报错，不要让它跑 45 秒
# 超时后才发现环境不对）；在假 HOME 下运行（不碰真实主目录），并临时构建
# 一份 `appclone` CLI 放进 PATH——创建分身要生成指向 CLI 的桌面入口。
#
# 用法：design/guitest-gtk.sh [--keep]   （--keep 保留临时目录与假 HOME 便于排查）
set -euo pipefail
cd "$(dirname "$0")/.."

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

if [ -z "${DISPLAY:-}" ] && [ -z "${WAYLAND_DISPLAY:-}" ]; then
  echo "guitest-gtk: 需要可用的显示（DISPLAY / WAYLAND_DISPLAY）" >&2
  exit 3
fi
if ! command -v bwrap >/dev/null 2>&1; then
  echo "guitest-gtk: 找不到 bwrap；GUI 在致命态下会停用所有开关，闸门无法驱动" >&2
  exit 4
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/appclone-gtktest-XXXXXX")
cleanup() {
  if [ "$KEEP" = 1 ]; then
    echo "guitest-gtk: 保留临时目录 $WORK（含假 HOME：$WORK/testroot）" >&2
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# 假 HOME 放在 $WORK 下：默认随 trap 清理，--keep 时保留，失败现场才能查。
export APPCLONE_GTK_ROOT="$WORK/testroot"
mkdir -p "$APPCLONE_GTK_ROOT"

# 不引入 cgo；与产品发布形态一致（puregotk 不需要 C 工具链）。
CGO_ENABLED=0 go build -o "$WORK/appclone" ./cmd/appclone
# 真实启动/停止用例要 fork 真正的 runner（测试二进制自己会再跑测试，不能当 runner）。
CGO_ENABLED=0 go build -o "$WORK/appclone-gui" ./cmd/appclone-gui
export APPCLONE_GTK_GUI_BIN="$WORK/appclone-gui"

# 真实输入注入必须显式开启（APPCLONE_GTK_UINPUT=1），且当前是 Wayland 会话、
# /dev/uinput 可写：uinput 事件进入宿主 seat，X11/Xvfb/嵌套合成器里窗口可以
# 在自己的 server 里 active，事件却落到宿主的其他应用上。默认关闭，宁可 SKIP
# 也不冒险；CI 没有 /dev/uinput，该用例会 SKIP——门禁检查这一行，不允许它静默消失。
CALLER_UINPUT="${APPCLONE_GTK_UINPUT:-}"
# 中文输入法用例还要求调用者先把桌面输入法切到中文：IME 引擎与激活态属于
# 用户配置，门禁不改写它。未显式给出 APPCLONE_GTK_IME=1 时该用例 SKIP。
CALLER_IME="${APPCLONE_GTK_IME:-0}"
UINPUT=0
UINPUT_WHY=""
if [ "$CALLER_UINPUT" = 1 ]; then
  if [ -z "${WAYLAND_DISPLAY:-}" ]; then
    UINPUT_WHY="当前不是 Wayland 会话（X11/Xvfb 下注入可能落到宿主应用）"
  elif [ -n "${GDK_BACKEND:-}" ] && [ "${GDK_BACKEND#*wayland}" = "$GDK_BACKEND" ]; then
    # WAYLAND_DISPLAY 可能从宿主继承（例如在 Wayland 桌面里套 xvfb-run），
    # 显式 GDK_BACKEND 才是真正的后端选择。
    UINPUT_WHY="GDK_BACKEND=$GDK_BACKEND 强制了非 Wayland 后端"
  elif [ "${WAYLAND_DISPLAY#/}" = "$WAYLAND_DISPLAY" ] && [ -z "${XDG_RUNTIME_DIR:-}" ]; then
    UINPUT_WHY="XDG_RUNTIME_DIR 未设置，无法定位 Wayland socket"
  elif [ ! -w /dev/uinput ]; then
    UINPUT_WHY="/dev/uinput 不可写"
  else
    UINPUT=1
  fi
fi
# 显式请求与自动降级必须分开：调用者已经声明要跑真实输入，却因环境不满足
# 而全部 SKIP 时，退出码 0 会被验收脚本记成「闸门通过」。此时直接失败；
# 默认未 opt-in 的 SKIP 仍然放行（CI/Xvfb 不设置这些变量）。
if [ "$CALLER_UINPUT" = 1 ] && [ "$UINPUT" != 1 ]; then
  echo "guitest-gtk: 已请求真实注入（APPCLONE_GTK_UINPUT=1）但未启用：$UINPUT_WHY" >&2
  exit 5
fi
if [ "$CALLER_IME" = 1 ] && [ "$UINPUT" != 1 ]; then
  IME_WHY="$UINPUT_WHY"
  if [ "$CALLER_UINPUT" != 1 ]; then
    IME_WHY="未设置 APPCLONE_GTK_UINPUT=1（中文用例依赖真实注入）"
  fi
  echo "guitest-gtk: 已请求中文输入法用例（APPCLONE_GTK_IME=1），但真实注入未启用：$IME_WHY" >&2
  exit 5
fi
export APPCLONE_GTK_UINPUT="$UINPUT"

if [ "$UINPUT" = 1 ]; then
  # 用例会把 XDG_RUNTIME_DIR 指到假 HOME 下（隔离应用 socket），GDK 的 Wayland
  # 后端却按 XDG_RUNTIME_DIR 解析 WAYLAND_DISPLAY。这里先固化成绝对 socket
  # 路径，并强制 Wayland 后端：真实注入验的必须是产品主目标的原生路径，而不是
  # 依赖 XWayland 的转发。
  case "${WAYLAND_DISPLAY:-}" in
    /*) : ;;
    *) WAYLAND_DISPLAY="${XDG_RUNTIME_DIR:-}/$WAYLAND_DISPLAY" ;;
  esac
  export WAYLAND_DISPLAY GDK_BACKEND=wayland
fi

# -tags gtk_e2e：测试文件带构建标签，默认 go test 不编译它（puregotk 在 init
# 阶段 dlopen GTK，没有 GTK 运行库的机器会在 init panic 而不是 skip）。
LOG="$WORK/gtk-test.log"
set +e
# -skip：长稳用例（TestGtkSoakRenderList）与性能用例（TestGtkSearchPerformance、
# TestGtkDistinctIconBatches）会污染常规闸门——前者在残留 APPCLONE_GTK_SOAK_MIN
# 时静默跑成 30 分钟长稳，后两者只经 design/perf.sh 手动跑。
# TestGtkLegacyReprobeRealProbe 会真启动假应用跑完整探测会话（需要 bwrap +
# FUSE），只在手动冒烟时用 APPCLONE_GTK_REAL_PROBE=1 单独跑，不进 CI 闸门。
# 整闸门用 fatal-criticals：拆卸期对已销毁控件调用会产生 GTK critical，必须当场中止。
# 不用 fatal-warnings 跑整闸门——本机 GL 上下文创建失败会间歇打出 Gdk-WARNING，
# 那与产品无关却会 abort 无关用例；关窗用例另开一轮，严格告警可选，见下。
CGO_ENABLED=0 PATH="$WORK:$PATH" APPCLONE_GTK_TEST=1 G_DEBUG=fatal-criticals \
  go test -tags gtk_e2e -count=1 -timeout 240s -v \
  -run 'TestGtk|TestIconPureHelpers|TestKeyringRowState|TestSearchProjection|TestDetachedRunnerSpawn|TestDetachLogHelpers|TestDetachedTargetEnv|TestBuildProfileKeepsNonPathFieldsAndCredentials|TestBuildProfileKeyringExplicitAndRowSync|TestSaveProfileLeavesNoKeyringState|TestReadsBannerPartsStates|TestMergeWarnKeys|TestRetryActionReaderStuck' \
  -skip '^(TestGtkSoakRenderList|TestGtkSearchPerformance|TestGtkDistinctIconBatches|TestGtkLegacyReprobeRealProbe)$' ./cmd/appclone-gui \
  2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
# 关窗路径单跑一轮，独立 cairo 渲染与独立 GTK_ROOT。默认 fatal-criticals：
# 拆卸期对已销毁控件调用会产生 critical，这是必须当场中止的。
# fatal-warnings 在无 GPU 的 Xvfb runner 上会被宿主 GTK 的无害 warning 误伤
# （本地桌面 GL 警告同理），因此只在显式 APPCLONE_GTK_STRICT_WARNINGS=1 时启用；
# 输出单独落 CLOSELOG 并逐条校验 PASS，主轮的 PASS 不能替这一轮作证。
CLOSE_GDEBUG=fatal-criticals
if [ "${APPCLONE_GTK_STRICT_WARNINGS:-0}" = 1 ]; then
  CLOSE_GDEBUG=fatal-warnings
fi
CLOSELOG="$WORK/gtk-close.log"
if ! GSK_RENDERER=cairo G_DEBUG="$CLOSE_GDEBUG" CGO_ENABLED=0 PATH="$WORK:$PATH" APPCLONE_GTK_TEST=1 \
  APPCLONE_GTK_ROOT="$WORK/testroot-close" \
  go test -tags gtk_e2e -count=1 -timeout 120s -v \
  -run '^(TestGtkListRetireClose|TestGtkDetailOpenClose|TestGtkRenameDialogClose|TestGtkRenameConfirmedClose|TestGtkStopThenClose|TestGtkClosedIgnoresCalls|TestGtkSetupClose)$' \
  ./cmd/appclone-gui >"$CLOSELOG" 2>&1; then
  tail -n 40 "$CLOSELOG" >&2
  exit 6
fi
cat "$CLOSELOG" >>"$LOG"
for name in TestGtkListRetireClose TestGtkDetailOpenClose TestGtkRenameDialogClose \
            TestGtkRenameConfirmedClose TestGtkStopThenClose TestGtkClosedIgnoresCalls \
            TestGtkSetupClose; do
  if ! grep -q -- "--- PASS: $name" "$CLOSELOG"; then
    echo "guitest-gtk: 关窗子轮（G_DEBUG=$CLOSE_GDEBUG）没有看到 $name 的 PASS" >&2
    exit 6
  fi
done
# `-run` 匹配不到任何测试时 `go test` 仍返回 0（输出只剩 "[no test files]"）：
# 测试被改名/移走会静默假绿，所以必须真的看到每一条 PASS。
for name in TestGtkFinishProbeRound TestGtkScreensLifecycle TestGtkLandsOnClonesView TestGtkEmptyStateMapped \
            TestGtkSetupStates TestGtkErrorState TestGtkRetryableError \
            TestGtkRapidRepush TestGtkFatalBanner TestGtkEnglishStrings \
            TestGtkDetailShowsStopWhenRunning TestGtkForkTogglePersists \
            TestGtkForkDisabledWhenFuseUnavailable TestGtkAppIconFallback \
            TestGtkConfirmReuseEvidence TestGtkSharedCertificates \
            TestGtkProbeProgress \
            TestGtkSearchResults TestSearchProjection TestGtkListBatches \
            TestGtkListRetireClose TestGtkDetailOpenClose TestGtkSetupClose \
            TestGtkListEmptyBatchClearsFocusMarker TestGtkFocusTargets \
            TestGtkIconBounds TestGtkIconCallbackSlots TestIconPureHelpers \
            TestGtkDialogResponses TestGtkCloseSwitchDialog \
            TestGtkRenameDialogClose TestGtkRenameConfirmedClose TestGtkStopThenClose TestGtkClosedIgnoresCalls \
            TestKeyringRowState \
            TestDetachedRunnerSpawn \
            TestDetachLogHelpers TestDetachedTargetEnv \
            TestBuildProfileKeepsNonPathFieldsAndCredentials \
            TestBuildProfileKeyringExplicitAndRowSync TestSaveProfileLeavesNoKeyringState \
            TestReadsBannerPartsStates TestMergeWarnKeys \
            TestRetryActionReaderStuck \
            TestGtkLegacyReprobeBanner TestGtkLegacyReprobeCancel \
            TestGtkLegacyReprobeLanded TestGtkLegacyReprobeErrorPaths \
            TestGtkRealInputUinput \
            TestGtkRealInputStartStop \
            TestGtkRealInputChineseIME; do
  if { [ "$name" = TestGtkRealInputUinput ] || [ "$name" = TestGtkRealInputStartStop ]; } && [ "$UINPUT" != 1 ]; then
    continue
  fi
  if [ "$name" = TestGtkRealInputChineseIME ] && { [ "$UINPUT" != 1 ] || [ "$CALLER_IME" != 1 ]; }; then
    continue
  fi
  if ! grep -q -- "--- PASS: $name" "$LOG"; then
    echo "guitest-gtk: 没有看到 $name 的 PASS（测试被移走/改名/跳过？）" >&2
    exit 5
  fi
done
if [ "$UINPUT" != 1 ]; then
  for name in TestGtkRealInputUinput TestGtkRealInputStartStop; do
    if ! grep -q -- "--- SKIP: $name" "$LOG"; then
      echo "guitest-gtk: 真实注入未启用，但也没看到 $name 的 SKIP" >&2
      exit 5
    fi
  done
  if [ "$CALLER_UINPUT" = 0 ]; then
    echo "guitest-gtk: 调用者显式关闭真实注入（APPCLONE_GTK_UINPUT=0），用例 SKIP" >&2
  elif [ "$CALLER_UINPUT" = 1 ]; then
    echo "guitest-gtk: 已请求真实注入但未启用：$UINPUT_WHY，用例 SKIP" >&2
  else
    echo "guitest-gtk: 未设置 APPCLONE_GTK_UINPUT=1（真实注入默认关闭），用例 SKIP" >&2
  fi
fi
if [ "$UINPUT" != 1 ] || [ "$CALLER_IME" != 1 ]; then
  if ! grep -q -- "--- SKIP: TestGtkRealInputChineseIME" "$LOG"; then
    echo "guitest-gtk: 中文输入法用例未启用，但也没看到它的 SKIP" >&2
    exit 5
  fi
  if [ "$UINPUT" = 1 ] && [ "$CALLER_IME" != 1 ]; then
    echo "guitest-gtk: 未设置 APPCLONE_GTK_IME=1（中文输入法用例默认关闭，需先把桌面输入法切到中文），用例 SKIP" >&2
  fi
fi
