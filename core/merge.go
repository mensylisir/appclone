package core

import (
	"maps"
	"path/filepath"
	"slices"
)

// MergeNonPathFields 把 base 里"探测不会产生"的配置补进 p，返回合并后的档案。
// 没有 resolver 时按字面路径去重（供不关心路径形态的调用方与测试使用）；
// CLI / GUI 应使用 MergeNonPathFieldsWith，让 `~/x` 与 `/home/u/x` 视为同一条。
func MergeNonPathFields(p, base Profile) Profile {
	return MergeNonPathFieldsWith(nil, p, base)
}

// MergeNonPathFieldsWith 是 MergeNonPathFields 的正式实现。
//
// 只用于重新配置 / 重新探测：这类操作重新定案的是**路径**（Paths /
// SharedWrites / ProbeMethod / 探测新发现的 keyring 项），而 env、extra_binds、
// isolate_dbus、isolate_runtime、sockets、cwd、audit、ignored 与未知字段是
// 用户或维护者的显式配置。探测产物里这些字段是零值，直接落盘等于把它们
// 整片抹掉（§5.5：未知字段必须原样保留；audit=true 更不能被静默关掉）。
//
// 冲突时以 base 为准：探测没有资格替用户决定"要不要审计/要不要私有 runtime"。
// 例外是 exec：探测刚看过应用的真实启动命令，它给出值时比旧档案更可信。
// lock_files 也以 base 为准（非空时）：维护者校对过的单例线索比本次 L0 静态
// 线索完整，用探测结果整体替换会丢掉一条锁文件，单例检测随之退化。
func MergeNonPathFieldsWith(r *PathResolver, p, base Profile) Profile {
	if len(p.Exec) == 0 && base.Exec != "" {
		p.Exec = base.Exec
		p.ExecArgs = slices.Clone(base.ExecArgs)
	}
	if len(base.LockFiles) > 0 {
		p.LockFiles = slices.Clone(base.LockFiles)
	}
	p.IsolateDBus = base.IsolateDBus
	p.IsolateRuntime = base.IsolateRuntime
	p.Sockets = slices.Clone(base.Sockets)
	p.ExtraBinds = slices.Clone(base.ExtraBinds)
	p.Cwd = base.Cwd
	if base.Env != nil {
		p.Env = maps.Clone(base.Env)
	}
	p.Audit = base.Audit
	// 空表 != 没写：`ignored = []` 是用户"我就要隔离所有这些"的明确表达，
	// 不能被悄悄换回默认忽略表。只有 nil（没声明）才保持 p 的默认值。
	if base.Ignored != nil {
		p.Ignored = slices.Clone(base.Ignored)
	}
	p.Unknown = base.Unknown
	p.Unsupported = mergeUnsupported(r, base.Unsupported, p.Unsupported)
	return p
}

// mergeUnsupported 按 (kind, 规范化路径) 去重合并。base（显式声明）排在前面，
// 同一路径上维护者写的 kind/reason 压过探测产物。
func mergeUnsupported(r *PathResolver, base, probe []Unsupported) []Unsupported {
	seen := make(map[string]bool, len(base)+len(probe))
	out := make([]Unsupported, 0, len(base)+len(probe))
	put := func(u Unsupported) {
		k := u.Kind + "\x00" + normalizedPath(r, u.Path)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, u)
	}
	for _, u := range base {
		put(u)
	}
	for _, u := range probe {
		put(u)
	}
	return out
}

// normalizedPath 把 `~/x` 与 `/home/u/x` 归一到同一形态，去重键才认得它们是
// 同一条路径（档案里两种写法都合法，探测与模板历来各写一种）。
func normalizedPath(r *PathResolver, path string) string {
	if r != nil {
		if abs, err := r.Expand(path); err == nil {
			return filepath.Clean(abs)
		}
	}
	return filepath.Clean(path)
}
