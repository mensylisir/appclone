#!/usr/bin/env bash
# soak.sh —— 长稳 / 内存稳定性证据（手动或发版前跑，不进 CI）。
#
# 两段：
#   1) GTK 渲染长稳：TestGtkSoakRenderList 反复整页重建 60 个应用的列表
#      （旧实现每轮泄漏 C 侧对象），预热后采样 RSS，增长超过
#      APPCLONE_GTK_SOAK_MAX_MB（默认 64 MiB）即失败。必须看到该用例的
#      `--- PASS`；SKIP 也算失败（分钟数给 0 时测试自己会 Skip，不能假绿）。
#   2) CLI 生命周期长稳：在隔离假 HOME 里循环 add → run → stop → rm。
#      就绪判据用 run.pid（stop 的目标记录），不是 run.lock——后者只建不删，
#      且"锁已建、pid 未写"的窗口里 stop 会按"正在启动"拒绝。每轮核对应用
#      写入落在实例根内、宿主假 HOME 无污染；结束后检查实例目录与
#      runner/bwrap 残留。
#
# 全程假 HOME / AC_BASE / XDG_RUNTIME_DIR，不碰真实主目录。$WORK 是 mktemp
# 出来的测试临时目录（不是用户数据目录），成功时自动清理，失败或 --keep
# 时保留现场。
#
# 用法：
#   design/soak.sh [分钟]          # 默认 30 分钟 GTK 段
#   SOAK_CYCLES=200 design/soak.sh 60
#   design/soak.sh --keep 10       # 保留现场便于排查
set -euo pipefail
cd "$(dirname "$0")/.."

MIN=30
KEEP=0
for a in "$@"; do
	case "$a" in
	--keep) KEEP=1 ;;
	'' | *[!0-9]*) echo "用法：design/soak.sh [--keep] [分钟]" >&2; exit 2 ;;
	*) MIN="$a" ;;
	esac
done
# 10# 归一化：08 按八进制会让 $((MIN + 5)) 直接报错，030 会被算成 24 分钟。
MIN=$((10#$MIN))
CYCLES="${SOAK_CYCLES:-100}"
case "$CYCLES" in
'' | *[!0-9]*) echo "soak: SOAK_CYCLES 必须是正整数" >&2; exit 2 ;;
esac
CYCLES=$((10#$CYCLES))
if [ "$MIN" -le 0 ] || [ "$CYCLES" -le 0 ]; then
	echo "soak: 分钟数与轮数必须是正整数" >&2
	exit 2
fi

if [ -z "${DISPLAY:-}" ] && [ -z "${WAYLAND_DISPLAY:-}" ]; then
	echo "soak: 需要可用的显示（DISPLAY / WAYLAND_DISPLAY）" >&2
	exit 3
fi
for tool in bwrap gio pgrep; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "soak: 找不到 $tool；CLI 生命周期段无法运行（bwrap=沙箱，gio=回收站，pgrep=残留检查）" >&2
		exit 4
	fi
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/appclone-soak-XXXXXX")
FAILED=0
cleanup() {
	rc=$?
	# 失败时保留现场：残留 runner 还在被删掉的 HOME 上跑会很难查。
	if [ "$KEEP" = 1 ] || [ "$FAILED" = 1 ] || [ "$rc" -ne 0 ]; then
		echo "soak: 保留临时目录 $WORK（含假 HOME，便于排查）" >&2
	else
		rm -rf "$WORK"
	fi
}
trap cleanup EXIT

CGO_ENABLED=0 go build -o "$WORK/appclone" ./cmd/appclone
export PATH="$WORK:$PATH"

echo "== GTK 渲染长稳：${MIN} 分钟 =="
set +e
APPCLONE_GTK_TEST=1 APPCLONE_GTK_ROOT="$WORK/gtkroot" \
	APPCLONE_GTK_SOAK_MIN="$MIN" \
	go test -tags gtk_e2e -count=1 -timeout "$((MIN + 5))m" -v \
	-run '^TestGtkSoakRenderList$' ./cmd/appclone-gui 2>&1 |
	tee "$WORK/gtk-soak.log" |
	grep -E '预热结束|进度 |长稳结束|--- (PASS|FAIL|SKIP)|FAIL:|panic|\[build failed\]'
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" -ne 0 ]; then
	echo "soak: GTK 长稳测试失败（退出码 $rc），完整日志 $WORK/gtk-soak.log" >&2
	FAILED=1
	exit "$rc"
fi
# go test 在 -run 匹配不到、或测试被 Skip 时同样返回 0：必须真的看到 PASS。
if ! grep -q -- '--- PASS: TestGtkSoakRenderList' "$WORK/gtk-soak.log"; then
	echo "soak: 没有看到 TestGtkSoakRenderList 的 PASS（SKIP/改名/未运行都算失败），完整日志 $WORK/gtk-soak.log" >&2
	FAILED=1
	exit 5
fi

echo "== CLI 生命周期长稳：${CYCLES} 轮 =="
H="$WORK/home"
A="$WORK/xdg/appclone"
XDG="$WORK/xdg"
mkdir -p "$H" "$XDG" "$A/profiles.d" "$WORK/run"
chmod 700 "$WORK/run"
export HOME="$H" AC_HOME="$H" XDG_DATA_HOME="$XDG" AC_BASE="$A"
export XDG_CONFIG_HOME="$H/.config" XDG_STATE_HOME="$H/.local/state"
export XDG_CACHE_HOME="$H/.cache" XDG_RUNTIME_DIR="$WORK/run"
mkdir -p "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_CACHE_HOME"

# 替身应用：只写自己的配置目录，然后睡足够久让 stop 有目标。
cat >"$H/fakeapp.sh" <<'EOF'
#!/bin/sh
mkdir -p "$HOME/.config/Soak"
echo run >>"$HOME/.config/Soak/state"
exec /bin/sleep 120
EOF
chmod 700 "$H/fakeapp.sh"
mkdir -p "$XDG/applications"
cat >"$XDG/applications/demo.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Demo
Exec=/usr/bin/true
EOF
cat >"$A/profiles.d/demo.toml" <<EOF
version = 1

exec = "$H/fakeapp.sh"

paths = [
  "~/.config/Soak",
]

probe_method = "manual"

ignored = []
EOF

# 有界等待进程退出（最多 $2 次 × 50ms）；禁止无上限 wait。
wait_gone() {
	local p=$1 n=0
	while kill -0 "$p" 2>/dev/null; do
		n=$((n + 1))
		if [ "$n" -ge "${2:-200}" ]; then
			return 1
		fi
		sleep 0.05
	done
	return 0
}

start=$(date +%s)
fails=0
ok_cycles=0
state_ok=0
host_leak=0
for i in $(seq 1 "$CYCLES"); do
	id="soak$i"
	clog="$WORK/cycle-$i.log"
	: >"$clog"
	if ! "$WORK/appclone" add demo "$id" >>"$clog" 2>&1; then
		echo "cycle $i: add 失败" >&2
		tail -5 "$clog" >&2 || true
		fails=$((fails + 1))
		continue
	fi
	"$WORK/appclone" run demo "$id" >>"$clog" 2>&1 &
	pid=$!
	inst="$A/instances/demo/$id"
	pidfile="$inst/run.pid"
	ready=0
	for _ in $(seq 1 100); do
		[ -s "$pidfile" ] && ready=1 && break
		sleep 0.05
	done
	if [ "$ready" != 1 ]; then
		echo "cycle $i: run 5 秒内没有写出 run.pid" >&2
		tail -5 "$clog" >&2 || true
		fails=$((fails + 1))
		src_fail=1
		# 没有 pid 记录就 stop 不了；禁止无界等待，有界收尾后再走公共 wait。
		if ! wait_gone "$pid" 200; then
			kill -TERM "$pid" 2>/dev/null || true
			wait_gone "$pid" 40 || kill -KILL "$pid" 2>/dev/null || true
		fi
	else
		src_fail=0
		# 写入隔离检查：应用只应写进实例根，宿主假 HOME 必须干净。
		istate="$inst/root$H/.config/Soak/state"
		state_ready=0
		for _ in $(seq 1 100); do
			[ -e "$istate" ] && state_ready=1 && break
			sleep 0.05
		done
		if [ "$state_ready" = 1 ] && [ ! -e "$H/.config/Soak/state" ]; then
			state_ok=$((state_ok + 1))
		else
			echo "cycle $i: 写入隔离不对（实例根 state=$state_ready，宿主 state=$([ -e "$H/.config/Soak/state" ] && echo 有 || echo 无)）" >&2
			[ -e "$H/.config/Soak/state" ] && host_leak=$((host_leak + 1))
			fails=$((fails + 1))
			src_fail=1
		fi
		# stop 对"锁已建、pid 未写"的窗口返回 7（正在启动）；重试几次兜住收尾竞态。
		stop_rc=0
		for _ in 1 2 3; do
			if "$WORK/appclone" stop demo "$id" >>"$clog" 2>&1; then
				stop_rc=0
				break
			else
				# 失败状态必须从 else 分支取：if 条件为假且无 else/elif 时，
				# 整个复合命令的退出状态是 0，`stop_rc=$?` 会被静默清成成功。
				stop_rc=$?
				sleep 0.05
			fi
		done
		if [ "$stop_rc" != 0 ]; then
			echo "cycle $i: stop 失败（退出码 $stop_rc）" >&2
			tail -5 "$clog" >&2 || true
			fails=$((fails + 1))
			src_fail=1
			# 禁止死等：有界等待后强制收尾（正常 stop 路径不会走到这里）。
			if ! wait_gone "$pid" 200; then
				kill -TERM "$pid" 2>/dev/null || true
				wait_gone "$pid" 40 || kill -KILL "$pid" 2>/dev/null || true
			fi
		fi
	fi
	wait "$pid" 2>/dev/null || true
	if "$WORK/appclone" rm demo "$id" >>"$clog" 2>&1; then
		:
	else
		# 同理从 else 取状态：`! cmd` 取反后 $? 恒为 0，会把失败退出码打成 0。
		rm_rc=$?
		echo "cycle $i: rm 失败（退出码 $rm_rc）" >&2
		tail -5 "$clog" >&2 || true
		fails=$((fails + 1))
		src_fail=1
	fi
	if [ "$src_fail" = 0 ]; then
		ok_cycles=$((ok_cycles + 1))
	fi
	if [ $((i % 10)) = 0 ]; then
		echo "cycle $i/$CYCLES（失败 $fails，隔离核对 $state_ok，用时 $(( $(date +%s) - start ))s）"
	fi
done

# 收尾核对：实例目录空了、没有残留 runner/bwrap。
# find 在实例父目录不存在（所有 add 都失败）时返回非零；pipefail 下会触发
# errexit，连汇总都打不出来，所以这里必须兜住。
left_inst=$(find "$A/instances/demo" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l || true)
left_run=$(pgrep -f "$WORK/appclone run" 2>/dev/null | wc -l || true)
left_bwrap=$(pgrep -f "$A/instances/demo/" 2>/dev/null | wc -l || true)
left_proc=$((left_run + left_bwrap))
echo "CLI 长稳结束：${CYCLES} 轮，成功 ${ok_cycles}，失败 ${fails}，隔离核对 ${state_ok}，宿主泄漏 ${host_leak}，剩余实例目录 ${left_inst}，残留进程 ${left_proc}"

if [ "$host_leak" != 0 ]; then
	echo "soak: 宿主假 HOME 出现应用写入，隔离被破坏" >&2
	FAILED=1
	exit 1
fi
# ok_cycles 要求整轮无失败，state_ok 只记隔离核对通过；stop/rm 失败会让
# state_ok 大于 ok_cycles，那已由 fails 覆盖，这里只抓反方向的不变式。
if [ "$state_ok" -lt "$ok_cycles" ]; then
	echo "soak: 无失败轮次里有 $((ok_cycles - state_ok)) 轮未确认写入落在实例根" >&2
	FAILED=1
	exit 1
fi
if [ "$left_inst" != 0 ] || [ "$left_proc" != 0 ] || [ "$fails" != 0 ]; then
	echo "soak: CLI 生命周期长稳未通过" >&2
	FAILED=1
	exit 1
fi
echo "soak: PASS"
