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

// OverlayAvailable 报告探测能否走 overlay-diff（不挂载、不写任何东西）。
// 不可用时探测会降级到 strace（§3.4），GUI 据此在列表页挂一条可「了解」
// 的横幅、在探测页挂一条常驻横幅——降级是事实，不是通知。
func (e *Env) OverlayAvailable() error {
	m := e.Mounter
	if m == nil {
		m = fuseOverlayMounter{}
	}
	return m.Available()
}
