package core

import "os/exec"

// CheckBwrap 回答"PATH 里有没有 bwrap 可执行文件"。
//
// 它**不启动** bwrap：启动自检每开一次窗口都要跑，开一次沙箱太重；真正能不能
// 建 namespace 由 doctor / 探测会话回答。GUI 用它决定是否整页只读（§4.5）：
// 没有 bwrap 时任何操作最终都会失败，允许用户点开关只会让人在一屏点不动的
// 界面里迷路。
func CheckBwrap() error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return errEnv("err.env.bwrap_missing", nil, err)
	}
	return nil
}

// OverlayAvailable 报告探测能否走 overlay-diff。真实现会做一次空目录冒烟
// 挂载（结果进程内缓存），所以它回答的是"现在真的能挂吗"，不是"装了没"。
// GUI 据此判断 fuse 是否可用；是否降级另由 ProbeFallsBackToStrace 决定。
func (e *Env) OverlayAvailable() error {
	m := e.Mounter
	if m == nil {
		m = fuseOverlayMounter{}
	}
	return m.Available()
}

// ProbeFallsBackToStrace 回答"这次 overlay 不可用会不会自动降级到 strace"。
// 只有后端确实缺失（无 fuse-overlayfs/fusermount3、打不开 /dev/fuse）且
// strace 在 PATH 时才成立；挂载被拒（权限/沙盒）不降级，会直接把可操作的
// 错误交给用户（ADR-032）。GUI 用它在列表页挂降级横幅。
func ProbeFallsBackToStrace(fuseErr error) bool {
	if fuseErr == nil || !ProbeBackendMissing(fuseErr) {
		return false
	}
	_, err := exec.LookPath("strace")
	return err == nil
}
