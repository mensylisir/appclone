//go:build linux

package core

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// notifyRunSignals 订阅会终止 run 的三个信号。SIGHUP 也要接：
// 用户关掉终端时默认行为是直接没，那就留下一个看不见的实例占着 run.lock。
//
// 返回的函数用于撤销订阅（run 返回前必须调；测试里同一进程反复 run 时尤其重要）。
func notifyRunSignals(ch chan<- os.Signal) func() {
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	return func() { signal.Stop(ch) }
}

// terminateGroup 对整个进程组发 SIGTERM，3 秒后 SIGKILL。
//
// 只杀直接子进程不够：应用自己 fork+setsid 出来的进程会逃出这条链路
// （bwrap 0.13.1 实测），而 --unshare-pid 保证 bwrap 一死，namespace 内的
// PID 1 就死，内核连带清空。
func terminateGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return // 组里已经没有进程
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
