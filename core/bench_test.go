package core

// 性能基线：这些函数在每次启动/列表页刷新时都会走，回归里只断言正确性，
// 性能退化不会有任何信号。基准不设阈值（不同机器差异大），但数字要能复现：
//
//	go test -run '^$' -bench . -benchmem ./core/
//
// 结果记录在 docs/STATUS.md 的「性能基线」一行与 NEXT.md 本轮记录。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchEnv 与 testEnv 同构，只是接受 *testing.B（基准不能用 *testing.T 的
// Cleanup 语义，TempDir 的生命周期由 b 管理）。
func benchEnv(b *testing.B) *Env {
	b.Helper()
	root := b.TempDir()
	home := root + "/home"
	getenv := func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return root + "/xdg/config"
		case "XDG_DATA_HOME":
			return root + "/xdg/data"
		case "XDG_STATE_HOME":
			return root + "/xdg/state"
		case "XDG_CACHE_HOME":
			return root + "/xdg/cache"
		case "XDG_RUNTIME_DIR":
			return root + "/run"
		}
		return ""
	}
	r := newResolver(home, home, 1000, getenv)
	e := NewEnvAt(root+"/xdg/appclone", r)
	e.TmpRoot = filepath.Join(root, "tmp")
	if err := os.MkdirAll(e.TmpRoot, 0o700); err != nil {
		b.Fatal(err)
	}
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	// CreateInstance 会为实例生成桌面入口，必须能按 key 找到"已安装的应用"。
	appsDir := filepath.Join(root, "apps")
	if err := os.MkdirAll(appsDir, 0o700); err != nil {
		b.Fatal(err)
	}
	desktop := "[Desktop Entry]\nType=Application\nName=Bench\nExec=/usr/bin/true\n"
	if err := os.WriteFile(filepath.Join(appsDir, "bench.desktop"), []byte(desktop), 0o600); err != nil {
		b.Fatal(err)
	}
	e.AppDirList = []string{appsDir}
	return e
}

// benchProfile 模拟一个典型 Electron 应用：多个数据目录、少量共享写回、
// 一对额外 bind。规模取实测档案的中上水平（多数应用 2–5 条路径）。
func benchProfile() Profile {
	return Profile{
		App:     "bench",
		Version: ProfileVersion,
		Exec:    "/usr/bin/true",
		Paths: []string{
			"~/.config/Bench",
			"~/.local/share/Bench",
			"~/.cache/Bench",
			"~/.config/Bench-helper",
			"~/.local/state/Bench",
			"~/.config/Bench2",
		},
		SharedWrites: []string{"~/.config/gtk-3.0/bookmarks", "~/.local/share/recently-used.xbel"},
		ExtraBinds:   []BindPair{{Src: "/usr/share/fonts", Dst: "/usr/share/fonts"}},
		LockFiles:    []string{"~/.config/Bench/lock"},
		Env:          map[string]string{"GDK_BACKEND": "wayland", "LANG": "zh_CN.UTF-8"},
		Cwd:          "~",
		ProbeMethod:  "overlay",
	}
}

var benchArgsSink []string

// BenchmarkBuildBwrapArgs 是热路径里的纯计算：启动、doctor、审计都会调。
func BenchmarkBuildBwrapArgs(b *testing.B) {
	e := benchEnv(b)
	o := e.opts()
	o.RuntimeSocks = []BindPair{
		{Src: o.RuntimeDir + "/wayland-0", Dst: o.RuntimeDir + "/wayland-0"},
		{Src: o.RuntimeDir + "/pulse/native", Dst: o.RuntimeDir + "/pulse/native"},
	}
	o.DBusAddress = "unix:path=/run/user/1000/bus-private"
	p := benchProfile()
	in := Instance{App: "bench", ID: "main", Root: e.InstancePath("bench", "main") + "/root"}

	// 真实启动前 PrepareBindTargets 会把这些目录建出来；基准里补齐，
	// 否则 EvalSymlinks 走的是"路径不存在、逐级回退"的另一条分支。
	for _, rel := range p.Paths {
		abs := o.Home + strings.TrimPrefix(rel, "~")
		if err := os.MkdirAll(abs, 0o700); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		args, err := BuildBwrapArgs(p, in, o)
		if err != nil {
			b.Fatal(err)
		}
		benchArgsSink = args
	}
}

// BenchmarkLoadProfile 是列表页/详情页每次刷新都要付的 TOML 解析 + 校验成本。
func BenchmarkLoadProfile(b *testing.B) {
	e := benchEnv(b)
	if err := e.SaveProfile(benchProfile()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.LoadProfile("bench"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListInstances100 模拟重度用户：一个应用 100 个分身，详情页
// 每次刷新都要完整读一遍（实例目录 + instance.toml）。
func BenchmarkListInstances100(b *testing.B) {
	e := benchEnv(b)
	p := benchProfile()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := e.CreateInstance(ctx, "bench", p, fmt.Sprintf("id%03d", i)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list, err := e.ListInstances("bench")
		if err != nil {
			b.Fatal(err)
		}
		if len(list) != 100 {
			b.Fatalf("应读到 100 个实例，得到 %d", len(list))
		}
	}
}
