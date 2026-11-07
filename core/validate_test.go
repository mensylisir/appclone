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
