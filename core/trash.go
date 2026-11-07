package core

import (
	"os/exec"
)

// Trash 把路径移入系统回收站（§4 删除语义）。
//
// 绝不回退到 rm：实例目录里是聊天记录级别的用户数据，"回收站不可用"必须
// 以环境错误暴露，让用户自己决定下一步；静默 rm -rf 是不可逆的数据丢失。
func Trash(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	gio, err := exec.LookPath("gio")
	if err != nil {
		return errEnv("err.env.no_gio", nil, err)
	}
	args := append([]string{"trash", "--"}, paths...)
	out, err := exec.Command(gio, args...).CombinedOutput()
	if err != nil {
		return errEnv("err.env.trash_failed", map[string]any{"output": string(out)}, err)
	}
	return nil
}
