package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDesktop(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseDesktopExec(t *testing.T) {
	cases := []struct {
		name     string
		exec     string
		wantExec string
		wantArgs []string
		wantCode string
	}{
		{"字段码剥离", "/usr/bin/app %U", "/usr/bin/app", nil, "%U"},
		{"带引号的参数", `sh -c "echo hi" %F`, "sh", []string{"-c", "echo hi"}, "%F"},
		{"env 前缀", "env FOO=bar /usr/bin/app --x", "/usr/bin/app", []string{"--x"}, ""},
		{"无字段码", "/usr/bin/app --flag", "/usr/bin/app", []string{"--flag"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exec, args, codes, err := parseExecLine(c.exec)
			if err != nil {
				t.Fatal(err)
			}
			if exec != c.wantExec || codes != c.wantCode || strings.Join(args, "|") != strings.Join(c.wantArgs, "|") {
				t.Fatalf("解析结果 %q %v %q", exec, args, codes)
			}
		})
	}
	if _, _, _, err := parseExecLine(`/usr/bin/app "unterminated`); err == nil {
		t.Fatal("引号不配对必须报错")
	}
}

func TestScanAppsFiltersAndNormalizes(t *testing.T) {
	root := t.TempDir()
	r := newResolver(root+"/home", root+"/home", 1000, func(k string) string {
		if k == "XDG_DATA_HOME" {
			return root + "/xdg/data"
		}
		return ""
	})
	e := NewEnvAt(root+"/xdg/appclone", r)
	dir := filepath.Join(root, "xdg", "data", "applications")
	e.AppDirList = []string{dir} // 只看临时目录：扫描真实 /usr/share 会让测试随机器漂移

	writeDesktop(t, dir, "Demo.desktop", "[Desktop Entry]\nType=Application\nName=Demo\nName[zh_CN]=演示\nExec=/bin/true %U\nIcon=demo\n")
	writeDesktop(t, dir, "hidden.desktop", "[Desktop Entry]\nType=Application\nName=H\nExec=/bin/true\nNoDisplay=true\n")
	writeDesktop(t, dir, "lib.desktop", "[Desktop Entry]\nType=Link\nName=L\n")
	writeDesktop(t, dir, "appclone-demo-main.desktop", "[Desktop Entry]\nType=Application\nName=Self\nExec=/bin/true\n")

	apps, errs := e.ScanApps(context.Background())
	if len(errs) != 0 {
		t.Fatalf("不应有解析错误：%v", errs)
	}
	// 只剩 demo：NoDisplay/Type=Link/appclone- 前缀都该被排除。
	if len(apps) != 1 {
		t.Fatalf("应只扫到 1 个应用，得到 %d：%v", len(apps), apps)
	}
	a := apps[0]
	if a.Key != "demo" {
		t.Fatalf("key 应归一为小写 demo，得到 %s", a.Key)
	}
	if a.FieldCodes != "%U" {
		t.Fatalf("字段码应保留给入口生成，得到 %q", a.FieldCodes)
	}
	if a.Name != "Demo" && a.Name != "演示" {
		t.Fatalf("名称应取 Name，得到 %q", a.Name)
	}
}

func TestSanitizeAppKey(t *testing.T) {
	// 真实机器上就有 "CC Switch.desktop"：直接拒绝会让这类应用从列表里消失。
	cases := map[string]string{
		"CC Switch":       "cc-switch",
		"WorkBuddy AI":    "workbuddy-ai",
		"com.tencent.qq":  "com.tencent.qq",
		"  Spaced  Name ": "spaced-name",
		"dot..dot":        "dot..dot",
	}
	for in, want := range cases {
		got, err := SanitizeAppKey(in)
		if err != nil {
			t.Fatalf("%q 应可归一化：%v", in, err)
		}
		if got != want {
			t.Fatalf("%q → %q，期望 %q", in, got, want)
		}
	}
	for _, bad := range []string{"---", "appclone-x", ""} {
		if _, err := SanitizeAppKey(bad); err == nil {
			t.Fatalf("%q 应被拒", bad)
		}
	}
}

func TestScanAppsRejectsBadKey(t *testing.T) {
	root := t.TempDir()
	r := newResolver(root+"/home", root+"/home", 1000, func(k string) string {
		if k == "XDG_DATA_HOME" {
			return root + "/xdg/data"
		}
		return ""
	})
	e := NewEnvAt(root+"/xdg/appclone", r)
	dir := filepath.Join(root, "xdg", "data", "applications")
	e.AppDirList = []string{dir}
	// 归一化后是空串：没有可用的 key，必须记 ScanError 而不是进列表（§4 <app> 规则）。
	writeDesktop(t, dir, "---.desktop", "[Desktop Entry]\nType=Application\nName=B\nExec=/bin/true\n")
	apps, errs := e.ScanApps(context.Background())
	if len(apps) != 0 || len(errs) != 1 {
		t.Fatalf("非法 key 应被拒并记一条 ScanError：apps=%v errs=%v", apps, errs)
	}
}

// 应用卸载之后，孤儿实例必须还能出现在列表里并被删除（§6 命令表）。
// 这条路径没有 .desktop 可扫，完全依赖"档案 ∪ 实例目录"的并集；
// 少一半就会出现"用户在列表里找不到、也删不掉"的实例。
func TestListAppsIncludesOrphans(t *testing.T) {
	e := testEnv(t)
	app, id := seedVerifyApp(t, e) // 建了档案 + 一个实例
	// 把 .desktop 删掉，模拟"应用被卸载"。
	desktop := filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications", app+".desktop")
	if err := os.Remove(desktop); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	apps, _ := e.ListApps(context.Background())
	var found *App
	for i := range apps {
		if apps[i].Key == app {
			found = &apps[i]
		}
	}
	if found == nil {
		t.Fatalf("应用卸载后，孤儿实例仍应出现在列表里：%+v", apps)
	}
	if !found.HasProfile || found.InstanceN != 1 {
		t.Fatalf("孤儿应带 hasProfile 与实例数：%+v", *found)
	}
	if found.Exec == "" {
		// Exec 从档案里回填：应用已被卸载，桌面项没了，只能靠档案。
		t.Fatalf("档案里有 exec 时列表应回填：%+v", *found)
	}
	// 删除路径不依赖 .desktop，必须仍然可用。
	if err := e.DeleteInstance(context.Background(), app, id); err != nil {
		t.Fatalf("孤儿实例必须能删：%v", err)
	}
}
