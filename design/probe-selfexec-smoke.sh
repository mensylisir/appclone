#!/usr/bin/env bash
# ADR-036 发版前冒烟：真实 bwrap + strace 下 50 次迭代（100 轮）自重启捕获。
# E2E 单测每次 10 iteration × 2 轮；跑 5 次即 50 iteration。任一次 SKIP/FAIL
# 都按失败处理——环境缺失不能被当成通过。
set -euo pipefail
cd "$(dirname "$0")/.."

RUNS="${RUNS:-5}"
for i in $(seq 1 "$RUNS"); do
  echo "== 冒烟 $i/$RUNS =="
  out=$(go test ./core -run '^TestSelfRelaunchCaptureE2E$' -count=1 -v 2>&1) || {
    printf '%s\n' "$out"
    echo "FAIL: 第 $i 次冒烟失败" >&2
    exit 1
  }
  printf '%s\n' "$out"
  if ! grep -q -- '--- PASS: TestSelfRelaunchCaptureE2E' <<<"$out"; then
    echo "FAIL: 第 $i 次冒烟未真实执行（SKIP 或未运行）" >&2
    exit 1
  fi
done
echo "PASS: $((RUNS * 10)) 次迭代（$((RUNS * 20)) 轮）全部捕获"
