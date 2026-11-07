package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L0 的召回必须诚实：模式匹配只认"已经存在的目录"，不存在的猜出来也没用
// （它会被 ValidateRunnable 判 stale，或者让 prepareBindTargets 建一堆空目录）。
func TestStaticHintsPattern(t *testing.T) {
	e := probeEnv(t)
	home := e.Resolver.Home
	for _, p := range []string{
		filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "testapp"),
		filepath.Join(e.Resolver.XDG["DATA_HOME"], "testapp"),
		filepath.Join(home, ".testapp"),
	} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	d := App{Key: "testapp", DesktopID: "testapp.desktop"}
	h := e.StaticHints(context.Background(), d)
	if len(h.Paths) != 3 {
		t.Fatalf("应命中三条已有目录，得到 %v", h.Paths)
	}
	if !contains(h.Sources, "pattern") {
		t.Fatalf("应记录命中来源，得到 %v", h.Sources)
	}
	// 重复调用结果必须一致（map/遍历顺序不该泄漏到输出）。
	if again := e.StaticHints(context.Background(), d); strings.Join(again.Paths, "|") != strings.Join(h.Paths, "|") {
		t.Fatalf("两次结果不一致：%v vs %v", h.Paths, again.Paths)
	}
}

// 锁文件是 L0 唯一被探测直接消费的产出：它能让"主实例在跑→零写入"不再发生。
func TestStaticHintsLockFiles(t *testing.T) {
	e := probeEnv(t)
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "testapp")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chromium 系的锁名；目录不是普通文件，不该被当成锁。
	if err := os.WriteFile(filepath.Join(cfg, "SingletonLock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := e.StaticHints(context.Background(), App{Key: "testapp", DesktopID: "testapp.desktop"})
	if len(h.LockFiles) != 1 || !strings.HasSuffix(h.LockFiles[0], "SingletonLock") {
		t.Fatalf("应只把普通文件当锁，得到 %v", h.LockFiles)
	}
}

// AppArmor 只认 `owner ... w` 里家目录下的具体路径：不带 owner 的是系统级
// 可写路径，带通配的落不成路径（交给 L1）。
func TestParseAppArmorProfile(t *testing.T) {
	home := "/home/u"
	profile := strings.Join([]string{
		"#include <tunables/global>",
		"/usr/bin/demo {",
		"  #include <abstractions/base>",
		"  owner @{HOME}/.config/demo/** rwk,",
		"  owner @{HOME}/.demo/state rw,",
		"  owner @{HOME}/.cache/demo/* rw,", // 通配：跳过
		"  /etc/demo.conf rw,",              // 没有 owner：系统级
		"  owner /var/lib/demo/** rw,",      // 不在家目录：跳过
		"  owner @{HOME}/.config/demo/x r,", // 只读：不该当写入候选
		"}",
	}, "\n")
	got := parseAppArmorProfile(strings.NewReader(profile), home)
	want := []string{
		filepath.Join(home, ".config", "demo"),
		filepath.Join(home, ".demo", "state"),
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("解析结果错：\n得到 %v\n期望 %v", got, want)
	}
}

// Flatpak 只认 `~` 开头的 persist 项：`home` / `xdg-download` 太宽，
// 落成候选会让用户勾出一个"隔离一切"的档案。
func TestStaticHintsFlatpak(t *testing.T) {
	e := probeEnv(t)
	appData := filepath.Join(e.Resolver.Home, ".var", "app", "org.demo.App")
	if err := os.MkdirAll(filepath.Join(appData, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.RunCmd = func(_ context.Context, name string, args ...string) (string, error) {
		if name != "flatpak" {
			t.Fatalf("不该调用 %s", name)
		}
		return "[Context]\nshared=network;\nfilesystems=home;xdg-download;~/.var/app/org.demo.App;\n", nil
	}
	h := e.StaticHints(context.Background(), App{
		Key: "org.demo.app", DesktopID: "org.demo.App.desktop", Exec: "/usr/bin/flatpak",
	})
	if !contains(h.Sources, "flatpak") {
		t.Fatalf("应记录 flatpak 来源，得到 %+v", h)
	}
	found := false
	for _, p := range h.Paths {
		if strings.HasSuffix(p, filepath.Join(".var", "app", "org.demo.App")) {
			found = true
		}
		if p == e.Resolver.Home {
			t.Fatalf("`home` 这种过宽的项不能变成候选：%v", h.Paths)
		}
	}
	if !found {
		t.Fatalf("应命中 ~/.var/app/<id>，得到 %v", h.Paths)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
