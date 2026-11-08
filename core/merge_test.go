package core

import (
	"path/filepath"
	"testing"
)

// 重新探测只重定路径：其它字段丢一个都是用户的显式配置被抹掉。
func TestMergeNonPathFieldsPreservesUserConfig(t *testing.T) {
	base := Profile{
		Exec:           "/opt/app",
		ExecArgs:       []string{"--flag"},
		Paths:          []string{"~/.config/Old"},
		LockFiles:      []string{"~/.config/App/lock"},
		Sockets:        []string{"~/.config/App/sock"},
		ExtraBinds:     []BindPair{{Src: "/a", Dst: "~/.b"}},
		IsolateDBus:    true,
		IsolateRuntime: true,
		Env:            map[string]string{"FOO": "bar"},
		Cwd:            "~",
		ProbeMethod:    "overlay",
		Audit:          true,
		Ignored:        []string{}, // 用户显式"全部隔离"
		Unknown:        nil,
		Unsupported:    []Unsupported{{Kind: "dbus", Path: "/run/x", Reason: "manual"}},
	}
	probe := Profile{
		Exec:        "",
		Paths:       []string{"~/.config/New"},
		ProbeMethod: "overlay",
		Ignored:     DefaultIgnored(),
		Env:         map[string]string{},
		Unsupported: []Unsupported{{Kind: "keyring", Path: "~/.local/share/keyrings"}},
	}
	got := MergeNonPathFields(probe, base)

	if len(got.Paths) != 1 || got.Paths[0] != "~/.config/New" {
		t.Fatalf("路径必须来自本次探测：%v", got.Paths)
	}
	if got.ProbeMethod != "overlay" {
		t.Fatalf("探测方式来自本次探测，得到 %q", got.ProbeMethod)
	}
	if got.Exec != "/opt/app" || len(got.ExecArgs) != 1 {
		t.Fatalf("探测没给出 exec 时必须保留旧档案的启动命令：%q %v", got.Exec, got.ExecArgs)
	}
	if !got.Audit || !got.IsolateDBus || !got.IsolateRuntime || got.Cwd != "~" {
		t.Fatalf("非路径开关必须保留：%+v", got)
	}
	if got.Env["FOO"] != "bar" {
		t.Fatalf("env 必须保留：%v", got.Env)
	}
	if got.Ignored == nil || len(got.Ignored) != 0 {
		t.Fatalf("ignored = [] 是显式声明，不能被换回默认表：%v", got.Ignored)
	}
	if len(got.ExtraBinds) != 1 || got.ExtraBinds[0].Src != "/a" {
		t.Fatalf("extra_binds 必须保留：%+v", got.ExtraBinds)
	}
	if len(got.Unsupported) != 2 {
		t.Fatalf("unsupported 必须合并去重（探测的 keyring + 旧档案的声明）：%+v", got.Unsupported)
	}
}

// 探测给出了 exec 时以探测为准（应用升级后旧路径可能已经不存在）。
func TestMergeNonPathFieldsPrefersFreshExec(t *testing.T) {
	base := Profile{Exec: "/old/app", ExecArgs: []string{"--old"}}
	probe := Profile{Exec: "/new/app", ExecArgs: []string{"--new"}}
	got := MergeNonPathFields(probe, base)
	if got.Exec != "/new/app" || len(got.ExecArgs) != 1 || got.ExecArgs[0] != "--new" {
		t.Fatalf("探测的 exec 应压过旧档案：%q %v", got.Exec, got.ExecArgs)
	}
}

// 没有声明 ignored（nil）时保持调用方给的默认值，而不是清空。
func TestMergeNonPathFieldsNilIgnoredKeepsDefault(t *testing.T) {
	probe := Profile{Ignored: DefaultIgnored()}
	got := MergeNonPathFields(probe, Profile{})
	if len(got.Ignored) == 0 {
		t.Fatal("base 没声明 ignored 时不应清空默认表")
	}
}

// lock_files 冲突时以既有档案为准：维护者校对过的单例线索比本次 L0 静态
// 线索完整，用探测结果整体替换会丢掉一条，单例检测随之退化。
func TestMergeNonPathFieldsKeepsBaseLockFiles(t *testing.T) {
	base := Profile{LockFiles: []string{"~/.config/App/lock-a", "~/.config/App/lock-b"}}
	probe := Profile{LockFiles: []string{"~/.config/App/lock-a"}}
	got := MergeNonPathFields(probe, base)
	if len(got.LockFiles) != 2 {
		t.Fatalf("既有档案的 lock_files 应整体保留，得到 %v", got.LockFiles)
	}
}

// `~/x` 与 `/home/u/x` 是同一路径的两种合法写法：unsupported 去重必须
// 认得它们，否则档案里会长期挂着两条重复条目（P3，复审发现）。
func TestMergeNonPathFieldsNormalizesUnsupportedPaths(t *testing.T) {
	e := testEnv(t)
	home := e.Resolver.Home
	base := Profile{Unsupported: []Unsupported{
		{Kind: "keyring", Path: "~/.local/share/keyrings", Reason: "probe.why.keyring"},
	}}
	probe := Profile{Unsupported: []Unsupported{
		{Kind: "keyring", Path: filepath.Join(home, ".local", "share", "keyrings"), Reason: "probe.why.keyring"},
		{Kind: "dbus", Path: "~/.config/App/secret", Reason: "manual"},
	}}
	got := MergeNonPathFieldsWith(e.Resolver, probe, base)
	if len(got.Unsupported) != 2 {
		t.Fatalf("同一 keyring 目录应去重，得到 %+v", got.Unsupported)
	}
	if got.Unsupported[0].Path != "~/.local/share/keyrings" {
		t.Fatalf("显式声明应压过探测产物（且保持 ~ 形态）：%+v", got.Unsupported[0])
	}
}
