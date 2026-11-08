#!/usr/bin/env bash
# guitest-gtk.sh —— GTK 侧回归（真实 GTK/libadwaita，不用合成输入）。
#
# 与 design/guitest.js 的分工：那个验 HTML 设计稿的交互语义（项数见其 EXPECTED），
# 这个验 **GTK 实现**的页面生命周期与回调路径：列表渲染 → 配置确认 →
# 创建实例 → 详情 → 返回，全程走产品自己的信号处理函数。
#
# 为什么不需要鼠标/键盘：`SwitchRow.SetActive` 会发 notify::active 进产品
# 登记的 onSwitch；确认按钮与测试里的 v.submit() 绑的是同一个处理函数。
# 这是"直调同一个回调"，不是合成点击——本机 X 会话禁止 XTEST 注入、
# broadway 在 push 后自崩，所以这是目前唯一能自动跑起来的 GTK 闸门。
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

# -tags gtk_e2e：测试文件带构建标签，默认 go test 不编译它（puregotk 在 init
# 阶段 dlopen GTK，没有 GTK 运行库的机器会在 init panic 而不是 skip）。
LOG="$WORK/gtk-test.log"
set +e
CGO_ENABLED=0 PATH="$WORK:$PATH" APPCLONE_GTK_TEST=1 \
  go test -tags gtk_e2e -count=1 -timeout 180s -v -run 'TestGtk|TestDetachedRunnerSpawn|TestDetachLogHelpers|TestDetachedTargetEnv' ./cmd/appclone-gui \
  2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
# `-run` 匹配不到任何测试时 `go test` 仍返回 0（输出只剩 "[no test files]"）：
# 测试被改名/移走会静默假绿，所以必须真的看到每一条 PASS。
for name in TestGtkScreensLifecycle TestGtkLandsOnClonesView TestGtkEmptyStateMapped \
            TestGtkSetupStates TestGtkErrorState TestGtkRetryableError \
            TestGtkRapidRepush TestGtkFatalBanner TestGtkEnglishStrings \
            TestGtkDetailShowsStopWhenRunning TestGtkForkTogglePersists \
            TestGtkForkDisabledWhenDegraded TestGtkAppIconFallback \
            TestGtkDialogResponses TestGtkCloseSwitchDialog \
            TestDetachedRunnerSpawn \
            TestDetachLogHelpers TestDetachedTargetEnv; do
  if ! grep -q -- "--- PASS: $name" "$LOG"; then
    echo "guitest-gtk: 没有看到 $name 的 PASS（测试被移走/改名/跳过？）" >&2
    exit 5
  fi
done
