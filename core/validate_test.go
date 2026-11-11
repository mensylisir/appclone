package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateProfileRejectsDangerousPaths(t *testing.T) {
	// 这一组要测 XDG 默认值下的判定（~/.config 就是受保护目录），
	// 所以单独造一个不覆盖 XDG 的 env——用 testEnv 的 XDG 覆盖会把
	// "~/.config 受保护"这条测成假通过。
	root := t.TempDir()
	r := newResolver(root+"/home", root+"/home", 1000, func(string) string { return "" })
	e := NewEnvAt(root+"/home/.local/share/appclone", r)
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	o := e.opts()
	base := func(paths ...string) Profile {
		return Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: paths}
	}
	cases := []struct {
		name  string
		paths []string
		key   string
	}{
		{"根目录", []string{"/"}, "err.profile.root_path"},
		{"家目录", []string{"~"}, "err.profile.home_path"},
		{"公共目录本身", []string{"~/Downloads"}, "err.profile.public_path"},
		{"公共目录之下", []string{"~/Downloads/work"}, "err.profile.under_public_path"},
		{"整体配置目录", []string{"~/.config"}, "err.profile.protected_path"},
		{"数据目录之下", []string{"~/.local/share/appclone/x"}, "err.profile.base_path"},
		{"相对路径", []string{"relative/path"}, "err.path.not_absolute"},
		{"带点点", []string{"~/.config/../x"}, "err.path.dotdot"},
		{"未知变量", []string{"$NOPE/x"}, "err.path.unknown_var"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateProfile(base(c.paths...), o)
			if ExitCode(err) != CodeInput {
				t.Fatalf("必须被拒（2），得到 %v", err)
			}
			if !strings.Contains(errKey(err), c.key) {
				t.Fatalf("错误键应为 %s，得到 %s", c.key, errKey(err))
			}
		})
	}
}

func TestValidateProfileResolvesSymlinkIntoPublicDir(t *testing.T) {
	root := t.TempDir()
	home := root + "/home"
	r := newResolver(home, home, 1000, func(string) string { return "" })
	e := NewEnvAt(home+"/.local/share/appclone", r)
	o := e.opts()
	// ~/.config/App 是指向 ~/Downloads 的软链：只用 Clean 的字符串判定会放行，
	// 而内核 bind 时会解析到公共目录，等于把 Downloads 隔离成实例私有。
	if err := os.MkdirAll(home+"/.config", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/Downloads", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home+"/Downloads", home+"/.config/App"); err != nil {
		t.Skip("文件系统不支持符号链接：", err)
	}
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/App"}}
	if err := ValidateProfile(p, o); errKey(err) != "err.profile.public_path" {
		t.Fatalf("软链指向公共目录必须被拒，得到 %v", err)
	}
}

func TestValidateProfileRejectsReservedEnvAndOverlap(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/App"},
		Env: map[string]string{"HOME": "/tmp"}}
	if err := ValidateProfile(p, o); errKey(err) != "err.profile.reserved_env" {
		t.Fatalf("保留键必须被拒，得到 %v", err)
	}

	p = Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/App"},
		ExtraBinds: []BindPair{{Src: "/opt/data", Dst: "~/.config/App"}}}
	if err := ValidateProfile(p, o); errKey(err) != "err.profile.extra_bind_overlap" {
		t.Fatalf("额外绑定与 paths 重叠必须被拒，得到 %v", err)
	}

	p = Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/App"},
		LockFiles: []string{"~/.config/App/x.sock"}, Sockets: []string{"~/.config/App/x.sock"}}
	if err := ValidateProfile(p, o); errKey(err) != "err.profile.lock_is_socket" {
		t.Fatalf("锁文件与 socket 同路径必须被拒，得到 %v", err)
	}
}

func TestNormalizePathsCollapsesToAncestor(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	got, err := normalizePaths([]string{
		"~/.config/App/Cache", "~/.config/App", "~/.config/App", "~/.local/share/App",
	}, o)
	if err != nil {
		t.Fatal(err)
	}
	// 父目录已覆盖子目录：保留子项只会多一条冗余 bind（§3.1 ⑤）。
	want := []string{o.Home + "/.config/App", o.Home + "/.local/share/App"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("归一结果错误：%v", got)
	}
}

func TestValidateCreatableAndRunnable(t *testing.T) {
	e := testEnv(t)
	if err := ValidateCreatable(Profile{App: "x"}); errKey(err) != "err.profile.empty" {
		t.Fatalf("空 paths 必须被拒，得到 %v", err)
	}

	dir := filepath.Join(e.Resolver.Home, ".config", "App")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(e.Resolver.Home, "app")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Profile{App: "x", Version: 1, Exec: execPath, Paths: []string{"~/.config/App"}}
	if err := e.ValidateRunnable(p); err != nil {
		t.Fatalf("应当可运行：%v", err)
	}
	// 半失效：只要有一条路径没了，整条 bwrap 命令就失败（§6.3 profile_stale）。
	p.Paths = append(p.Paths, "~/.config/Gone")
	if err := e.ValidateRunnable(p); errKey(err) != "err.profile.stale" {
		t.Fatalf("缺一条路径必须判 stale，得到 %v", err)
	}
}

func TestValidateAppKeyAndID(t *testing.T) {
	ok := []string{"wechat", "com.tencent.wechat", "a", "app_1-2"}
	for _, s := range ok {
		if err := ValidateAppKey(s); err != nil {
			t.Fatalf("%s 应当合法：%v", s, err)
		}
	}
	bad := []string{"", ".", "..", "WeChat", "app clone", "appclone-x", "-a", "a/b", "微信"}
	for _, s := range bad {
		if err := ValidateAppKey(s); err == nil {
			t.Fatalf("%q 应当被拒", s)
		}
	}
	if err := ValidateID("work"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Work", "work space", "..", "work/1", ""} {
		if err := ValidateID(s); err == nil {
			t.Fatalf("id %q 应当被拒", s)
		}
	}
}

func TestAllocDisplayNameSkipsGaps(t *testing.T) {
	// 删掉中间那个后，"数量+1" 会撞上已有的「分身 2」——GNOME 里两个入口同名。
	if got := AllocDisplayName([]string{"分身 1", "分身 3"}); got != "分身 2" {
		t.Fatalf("应取最小未占用：%s", got)
	}
	if got := AllocDisplayName(nil); got != "分身 1" {
		t.Fatalf("首个应是 分身 1：%s", got)
	}
}

func TestValidateDisplayName(t *testing.T) {
	if err := ValidateDisplayName("工作号", nil); err != nil {
		t.Fatal(err)
	}
	// NFC：é 的合成形式与分解形式必须视为同一个名字。
	if err := ValidateDisplayName("caf\u00e9", []string{"cafe\u0301"}); errKey(err) != "err.display_name.duplicate" {
		t.Fatalf("NFC 后同名应判重复，得到 %v", err)
	}
	if err := ValidateDisplayName(strings.Repeat("名", 33), nil); errKey(err) != "err.display_name.too_long" {
		t.Fatalf("超长应被拒，得到 %v", err)
	}
	if err := ValidateDisplayName("a\nb", nil); errKey(err) != "err.display_name.control_char" {
		t.Fatalf("换行应被拒，得到 %v", err)
	}
}

// 根目录这条边界：`relUnder("/", "/etc")` 曾经恒为 false（拼出 "//"），
// 于是"拒绝 `/`"只能靠调用方记得单独补一条 —— 漏一次就是全量放行。
func TestIsUnderRootParent(t *testing.T) {
	if !IsUnder("/", "/etc/passwd") {
		t.Fatal(`IsUnder("/", "/etc/passwd") 应为 true`)
	}
	if IsUnder("/etc", "/etcetera") {
		t.Fatal("前缀相同但不是子路径，不得判 true")
	}
	if rel, ok := relUnder("/", "/etc"); !ok || rel != "etc" {
		t.Fatalf(`relUnder("/", "/etc") = %q,%v，期望 "etc",true`, rel, ok)
	}
}

// ignored / shared_writes 里写"家目录的祖先"同样等于全部放行：
// `ignored = ["/home"]` 会让 verify 对任何越界写入都说通过。
func TestValidateProfileRejectsHomeAncestor(t *testing.T) {
	root := t.TempDir()
	home := "/home/appclone-test-user/u" // 刻意不存在的家目录，只验证祖先判定
	r := newResolver(home, home, 1000, func(string) string { return "" })
	e := NewEnvAt(filepath.Join(root, "base"), r)
	base := Profile{Exec: "/usr/bin/true", Paths: []string{"~/.config/App"}}
	for _, bad := range []string{"/", "/home", "/home/appclone-test-user"} {
		p := base
		p.Ignored = []string{bad}
		if err := ValidateProfile(p, e.BuildOptions()); ExitCode(err) != CodeInput {
			t.Fatalf("ignored=[%q]（家目录的祖先）应被拒（2），得到 %v", bad, err)
		}
	}
	// 正常的具体目录不受影响。
	p := base
	p.Ignored = []string{"~/.cache"}
	if err := ValidateProfile(p, e.BuildOptions()); err != nil {
		t.Fatalf("正常的忽略目录应合法，得到 %v", err)
	}
}

// 同一路径既隔离又共享：bwrap 里谁生效取决于 bind 顺序，档案必须自相矛盾才报错。
func TestValidateProfileRejectsIsolateAndShare(t *testing.T) {
	e := testEnv(t)
	_, p := seedApp(t, e)
	p.SharedWrites = []string{"~/.config/TestApp"}
	if err := ValidateProfile(p, e.BuildOptions()); ExitCode(err) != CodeInput {
		t.Fatalf("同时隔离与共享应被拒（2），得到 %v", err)
	}
	// 共享目录在隔离目录之下也算冲突（子项被父项隔离后共享就失效了）。
	p.SharedWrites = []string{"~/.config/TestApp/cache"}
	if err := ValidateProfile(p, e.BuildOptions()); ExitCode(err) != CodeInput {
		t.Fatalf("共享项落在隔离目录之下也应被拒（2），得到 %v", err)
	}
}

func TestValidateProfileReadTraceEnum(t *testing.T) {
	root := t.TempDir()
	r := newResolver(root+"/home", root+"/home", 1000, func(string) string { return "" })
	e := NewEnvAt(root+"/home/.local/share/appclone", r)
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/App"}}
	for _, v := range []string{"", "ok", "partial", "unavailable"} {
		p.ReadTrace = v
		if err := ValidateProfile(p, e.opts()); err != nil {
			t.Fatalf("%q 应合法：%v", v, err)
		}
	}
	p.ReadTrace = "complete"
	if err := ValidateProfile(p, e.opts()); errKey(err) != "err.profile.read_trace" {
		t.Fatalf("未知 read_trace 必须拒绝，得到 %v", err)
	}
}

func TestValidateCredentialPathsSubset(t *testing.T) {
	root := t.TempDir()
	r := newResolver(root+"/home", root+"/home", 1000, func(string) string { return "" })
	e := NewEnvAt(root+"/home/.local/share/appclone", r)
	o := e.opts()
	p := Profile{App: "x", Version: 1, Exec: "/bin/true", Paths: []string{"~/.config/App"}}

	p.CredentialPaths = []string{"~/.config/App"}
	if err := ValidateProfile(p, o); err != nil {
		t.Fatalf("credential == path 应合法：%v", err)
	}
	p.CredentialPaths = []string{"~/.config/App/credentials.json"}
	if err := ValidateProfile(p, o); err != nil {
		t.Fatalf("credential 位于 path 之下应合法：%v", err)
	}
	p.CredentialPaths = []string{"~/.config/Other"}
	if err := ValidateProfile(p, o); errKey(err) != "err.profile.credential_outside_paths" {
		t.Fatalf("paths 之外的 credential 必须拒绝，得到 %v", err)
	}
}

// 自定义 AC_BASE 是软链时，词法比较会漏掉"实例根位于被隔离目录内"；
// 必须同时按真实路径判定（否则 bwrap 自指）。
func TestCheckDangerousBaseSymlink(t *testing.T) {
	root := t.TempDir()
	realBase := filepath.Join(root, "real")
	if err := os.MkdirAll(realBase, 0o700); err != nil {
		t.Fatal(err)
	}
	linkBase := filepath.Join(root, "link")
	if err := os.Symlink(realBase, linkBase); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(realBase, "instances", "demo")
	o := BuildOptions{Home: filepath.Join(root, "home"), Base: linkBase}
	if err := checkDangerous(abs, o, "paths"); err == nil {
		t.Fatal("真实路径落在 AC_BASE 之下必须拒绝")
	}
}
