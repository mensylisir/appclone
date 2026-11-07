package core

import (
	"fmt"
	"os"
	"path/filepath"
)

// AppRunning 判定"目标应用是否已有进程在跑"（§3.2.2）。
//
// 比对 /proc/*/exe 的真实路径与 basename（comm 只有 15 字符、会截断）。
// 已知局限（写进 discover.md）：解释器包装（`python3 x.py`）、AppImage、
// flatpak 的 exe 都在别处，会漏判——漏判的表现是零写入，用户会被提示
// "没有跟踪到任何写入"，不是静默错误。
func (e *Env) AppRunning(execPath string) (bool, error) {
	if execPath == "" || execPath == "." || execPath == "/" {
		return false, nil
	}
	want := filepath.Base(execPath)
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return false, nil // 读不到 namespace 就退化成"不排除"，不要因此挡住探测
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false, errEnv("err.env.read_dir", map[string]any{"path": "/proc"}, err)
	}
	selfPid := fmt.Sprint(os.Getpid())
	for _, ent := range ents {
		name := ent.Name()
		if !ent.IsDir() || name[0] < '0' || name[0] > '9' || name == selfPid {
			continue
		}
		ns, err := os.Readlink(filepath.Join("/proc", name, "ns/mnt"))
		if err != nil || ns != self {
			continue // 名字空间不同：那是分身（或别的容器），不是主实例
		}
		exe, err := os.Readlink(filepath.Join("/proc", name, "exe"))
		if err != nil {
			continue
		}
		if exe == execPath || filepath.Base(exe) == want {
			return true, nil
		}
	}
	return false, nil
}
