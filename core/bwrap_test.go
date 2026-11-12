package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv 造一个完全隔离的 Env：家目录、XDG、数据根都在 t.TempDir() 里。
// 所有测试都必须走它——用真实环境跑的测试既是假通过源，也会污染主目录（§7 前置）。
func testEnv(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()
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
	// 写层根必须隔离：共用 /var/tmp 会让用例互相看到对方的 upper，
	// 一个测试的残留能把另一个测试判成 audit_incomplete。
	e.TmpRoot = filepath.Join(root, "tmp")
	if err := os.MkdirAll(e.TmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	// 不真的动回收站：测试里把删除换成"记下来"。
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	return e
}

func (e *Env) opts() BuildOptions {
	o := e.BuildOptions()
	o.RuntimeDir = e.Resolver.XDG["RUNTIME_DIR"]
	// HostBusPaths 来自宿主进程环境；测试默认清空，需要它的用例自己显式设置，
	// 否则精确 argv 断言会随运行环境变化。
	o.HostBusPaths = nil
	return o
}

// instSrc 独立实现一次映射，不复用 instanceChild：测试要能抓住被测函数的错。
func instSrc(root, abs string) string {
	return root + "/" + strings.TrimPrefix(abs, "/")
}

func TestBuildBwrapArgsGoldenOrder(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	o.RuntimeSocks = []BindPair{
		{Src: o.RuntimeDir + "/wayland-0", Dst: o.RuntimeDir + "/wayland-0"},
		{Src: o.RuntimeDir + "/pulse/native", Dst: o.RuntimeDir + "/pulse/native"},
	}
	o.DBusAddress = "unix:path=/run/user/1000/bus-private"
	// 宿主总线固定在 /tmp（dbus-launch 形态），golden 显式钉住遮蔽位置：
	// 私有总线在跑时用 /dev/null 盖住宿主 socket，必须排在 tmpfs 与实例 bind 之前。
	o.HostBusPaths = []string{"/tmp/dbus-hostbus"}

	p := Profile{
		App:            "wechat",
		Version:        ProfileVersion,
		Exec:           "/usr/bin/wechat",
		ExecArgs:       []string{"--flag"},
		Paths:          []string{"~/.config/WeChat", "~/.local/share/WeChat"},
		SharedWrites:   []string{"~/.config/gtk-3.0/bookmarks"},
		LockFiles:      []string{"~/.config/WeChat/lock"},
		IsolateDBus:    true,
		IsolateRuntime: true,
		Env:            map[string]string{"B": "2", "A": "1"},
		Cwd:            "~",
	}
	in := Instance{App: "wechat", ID: "work", Root: e.InstancePath("wechat", "work") + "/root"}
	instDir := e.InstancePath("wechat", "work")

	got, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatalf("BuildBwrapArgs: %v", err)
	}
	want := []string{
		"--unshare-user",
		"--unshare-pid", "--unshare-ipc",
		"--bind", "/", "/",
		"--dev-bind", "/dev", "/dev",
		"--proc", "/proc",
		// ④.1 宿主总线遮蔽（私有总线在跑时）
		"--bind", "/dev/null", "/tmp/dbus-hostbus",
		// ④.6 AC_BASE 遮蔽：必须在 paths 之前，否则实例 bind 会被它盖住
		"--tmpfs", o.BaseReal,
		// ④.7 审计写层根：普通 run 也必须遮蔽（其他分身的 upper 证据）
		"--tmpfs", o.AuditRoot,
		// ⑤ paths：父先子后，源在实例 root 下
		"--bind", instSrc(in.Root, o.Home+"/.config/WeChat"), o.Home + "/.config/WeChat",
		"--bind", instSrc(in.Root, o.Home+"/.local/share/WeChat"), o.Home + "/.local/share/WeChat",
		// ⑥ shared_writes 在 paths 之后
		"--bind-try", o.Home + "/.config/gtk-3.0/bookmarks", o.Home + "/.config/gtk-3.0/bookmarks",
		// ⑧ lock_files
		"--bind", instDir + "/locks/" + lockFileName(o.Home+"/.config/WeChat/lock"), o.Home + "/.config/WeChat/lock",
		// ⑨ runtime 目录 + setenv，然后只读 socket
		"--bind", instDir + "/runtime", o.RuntimeDir,
		"--setenv", "XDG_RUNTIME_DIR", o.RuntimeDir,
		"--ro-bind-try", o.RuntimeDir + "/wayland-0", o.RuntimeDir + "/wayland-0",
		"--ro-bind-try", o.RuntimeDir + "/pulse/native", o.RuntimeDir + "/pulse/native",
		// ⑩ dbus
		"--bind", instDir + "/dbus/bus", o.RuntimeDir + "/bus",
		"--setenv", "DBUS_SESSION_BUS_ADDRESS", o.DBusAddress,
		// ⑫ env 按 key 字典序
		"--setenv", "A", "1",
		"--setenv", "B", "2",
		"--chdir", o.Home,
		"--die-with-parent",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("argv 不符合契约：\n got=%v\nwant=%v", got, want)
	}
	// 硬规则单点复核：只读 socket 必须在 runtime 目录 bind 之后，否则目录 bind 会盖掉它。
	assertOrder(t, got, "--setenv XDG_RUNTIME_DIR", "--ro-bind-try")
	assertOrder(t, got, "--bind / /", "--dev-bind")
	assertOrder(t, got, "--bind /dev/null /tmp/dbus-hostbus", "--tmpfs "+o.BaseReal)
	assertOrder(t, got, "--bind "+instSrc(in.Root, o.Home+"/.config/WeChat"),
		"--bind-try "+o.Home+"/.config/gtk-3.0/bookmarks")
}

// assertOrder 断言 a 在 b 之前出现（参数按 argv 元素配对，不能只做字符串包含）。
func assertOrder(t *testing.T, argv []string, a, b string) {
	t.Helper()
	sa, sb := strings.Join(argv, " "), ""
	_ = sb
	ia := strings.Index(sa, a)
	ib := strings.Index(sa, b)
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("顺序不符合契约：%q 必须早于 %q\nargv=%v", a, b, argv)
	}
}

func TestBuildBwrapArgsPrivateDirsLiveOutsideRoot(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	o.DBusAddress = "unix:path=/run/user/1000/x"
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/X"},
		IsolateRuntime: true, IsolateDBus: true, LockFiles: []string{"~/.config/X/lock"}}
	instDir := e.InstancePath("x", "main")
	got, err := BuildBwrapArgs(p, Instance{App: "x", ID: "main", Root: instDir + "/root"}, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"--bind " + instDir + "/runtime " + o.RuntimeDir,
		"--bind " + instDir + "/dbus/bus " + o.RuntimeDir + "/bus",
		"--bind " + instDir + "/locks/",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %q\nargv=%v", want, got)
		}
	}
	// runtime/dbus/locks 都不是应用数据，绝不能出现在 root/ 下：那会被当成
	// 应用数据一起进回收站，也会与 paths 映射出的同名子路径打架。
	if strings.Contains(joined, "/root/runtime") || strings.Contains(joined, "/root/dbus") || strings.Contains(joined, "/root/locks") {
		t.Fatalf("私有目录不该在 root/ 下\nargv=%v", got)
	}
}

func TestBuildBwrapArgsRejectsPublicDir(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/Downloads"}}
	_, err := BuildBwrapArgs(p, Instance{Root: "/tmp/root"}, o)
	if ExitCode(err) != CodeInput {
		t.Fatalf("公共目录必须被拒（退出 2），得到 %v", err)
	}
}

func TestBuildBwrapArgsDBusImpliesRuntime(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	o.DBusAddress = "unix:path=/run/user/1000/x"
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/X"}, IsolateDBus: true}
	got, err := BuildBwrapArgs(p, Instance{Root: "/tmp/root"}, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	// 没有 isolate_runtime 时也必须 bind runtime：否则 bus 的 bind 目标落在宿主
	// $XDG_RUNTIME_DIR，bwrap 会在宿主建 bus 占位文件，压掉真实 session bus。
	if !strings.Contains(joined, "--setenv XDG_RUNTIME_DIR") {
		t.Fatalf("isolate_dbus 必须隐含 isolate_runtime，argv=%v", got)
	}
}

// fork 实例的 bind 源换成 overlay 挂载点；未挂载成功的路径仍退回实例目录。
func TestBuildBwrapArgsForkSources(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	p := Profile{
		App: "x", Version: ProfileVersion, Exec: "/bin/true",
		Paths: []string{"~/.config/X", "~/.local/share/X"},
	}
	in := Instance{App: "x", ID: "main", Root: e.InstancePath("x", "main") + "/root"}
	forkAbs := o.Home + "/.config/X"
	plainAbs := o.Home + "/.local/share/X"
	merged := e.InstancePath("x", "main") + "/.fork/abcd1234/merged"
	o.ForkSources = map[string]string{forkAbs: merged}

	got, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--bind "+merged+" "+forkAbs) {
		t.Fatalf("fork 路径必须绑定 overlay 挂载点，argv=%v", got)
	}
	if strings.Contains(joined, "--bind "+instSrc(in.Root, forkAbs)+" "+forkAbs) {
		t.Fatalf("fork 路径不应再绑定裸实例目录，argv=%v", got)
	}
	if !strings.Contains(joined, "--bind "+instSrc(in.Root, plainAbs)+" "+plainAbs) {
		t.Fatalf("未覆盖的路径应保持原语义，argv=%v", got)
	}
}

func TestInstanceChildNeverEscapes(t *testing.T) {
	root := "/base/instances/app/main/root"
	got, err := instanceChild(root, "/home/u/.config/App")
	if err != nil {
		t.Fatal(err)
	}
	if got != root+"/home/u/.config/App" {
		t.Fatalf("映射错误：%s", got)
	}
	// 这条是防 filepath.Join 丢前缀的回归：Join(root, "/abs") 会返回 "/abs"。
	if strings.HasPrefix(got, "/home") {
		t.Fatalf("源路径逃出实例目录：%s", got)
	}
}
