package core

// ProbeCommand 决定探测时用什么命令启动应用。重探时如果旧档案与桌面入口
// 指向同一可执行文件且旧档案有 exec_args，优先用旧参数：这些参数可能是
// 上次从 execve 证据识别出的自重启参数（例如 --ozone-platform=x11），
// 用 .desktop 的原始参数探测会让应用再次自重启并撞上 PID 命名空间拆毁。
// 返回新切片，调用方修改不会污染旧档案。
func ProbeCommand(desktopExec string, desktopArgs []string, old Profile, hasOld bool) []string {
	args := desktopArgs
	if hasOld && sameExecPath(old.Exec, desktopExec) && len(old.ExecArgs) > 0 {
		args = old.ExecArgs
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, desktopExec)
	out = append(out, args...)
	return out
}
