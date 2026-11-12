package core

import "path/filepath"

// sameExecPath 判定两条 exec 路径是否同一文件。空字符串一律 false：没有
// 目标就谈不上“同路径”。先按 Clean 比较（快、不碰文件系统），再尝试
// Real（兼容 `/usr/bin/qoder` 软链与 `/opt/qoder/qoder` 真实路径）。
func sameExecPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	return Real(a) == Real(b)
}
