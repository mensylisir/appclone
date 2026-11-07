package core

import (
	"strings"
	"testing"
)

// 桌面入口是用户唯一直接接触的东西：少一个 `= `、`Exec` 少引号、`%U` 丢了，
// 表现都是"图标点了没反应"或"右键菜单里没有它"。这些字段逐条钉住，
// 因为它们不会被任何"运行成功"的测试覆盖到。
func TestRenderLauncherFields(t *testing.T) {
	e := testEnv(t)
	d := App{
		Key:        "demo",
		Name:       "Demo App",
		Icon:       "demo-icon",
		FieldCodes: "%U",
		Categories: "Utility;Network;",
		// Actions/MimeType 这类必须不被继承，否则分身会重复注册。
		StartupWMClass: "DemoApp",
	}
	in := Instance{App: "demo", ID: "work", DisplayName: "工作号"}

	raw, err := e.renderLauncher(in, d)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Version=1.0",
		"Terminal=false",
		// Exec 用绝对路径 + 保留字段码：文件管理器"用分身打开"依赖 %U。
		"Exec=" + e.CliPath + " run demo work %U",
		"TryExec=" + e.CliPath,
		"Icon=demo-icon",
		"Categories=Utility;Network;",
		"StartupWMClass=DemoApp",
		"X-Appclone-App=demo",
		"X-Appclone-Id=work",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("缺少字段 %q\n---\n%s", want, out)
		}
	}
	// 三行 Name：Name 是兜底，另外两行是本地化（只写 Name 会让中文桌面显示英文）。
	if !strings.Contains(out, "Name=Demo App") {
		t.Fatalf("Name 兜底行应有应用名：\n%s", out)
	}
	if !strings.Contains(out, "Name[zh_CN]=Demo App（分身 工作号）") {
		t.Fatalf("中文名没写对：\n%s", out)
	}
	if !strings.Contains(out, "Name[en]=Demo App (Clone · 工作号)") {
		t.Fatalf("英文名没写对：\n%s", out)
	}
	// 不能继承源入口的这些键（白名单式继承）。
	for _, forbidden := range []string{"Actions=", "MimeType=", "DBusActivatable=", "Exec=demo"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("不该出现 %q：\n%s", forbidden, out)
		}
	}
}

// 缺省值不能让文件变成非法：没有 Icon、没有 Categories、没有字段码时也要能起。
func TestRenderLauncherDefaults(t *testing.T) {
	e := testEnv(t)
	in := Instance{App: "demo", ID: "main", DisplayName: "分身 1"}
	out := string(mustRender(t, e, in, App{Key: "demo"}))

	if !strings.Contains(out, "Icon=application-x-executable\n") {
		t.Fatalf("没有图标时应回退到通用图标：\n%s", out)
	}
	if !strings.Contains(out, "Categories=Utility;\n") {
		t.Fatalf("没有分类时应回退到 Utility：\n%s", out)
	}
	// 源 Exec 没有字段码时**不许自己补** %U：补了应用会收到不认识的参数。
	if strings.Contains(out, "%U") {
		t.Fatalf("源没有字段码时不该补：\n%s", out)
	}
	if strings.Contains(out, "StartupWMClass=") {
		t.Fatalf("没有 StartupWMClass 时该整行省略（空值会让窗口分组错乱）：\n%s", out)
	}
}

// 纯净字符串写进 .desktop 是注入面：源 Exec 的 Name 里一个换行就能把
// 后续任意键注进去（比如 `X-Appclone-Id=`）。
func TestRenderLauncherEscapes(t *testing.T) {
	e := testEnv(t)
	in := Instance{App: "demo", ID: "main", DisplayName: "a\nb"}
	out := string(mustRender(t, e, in, App{Key: "demo", Name: "evil\nExec=/bin/sh"}))
	if strings.Contains(out, "evil\nExec=/bin/sh") {
		t.Fatalf("应用名里的换行必须被转义：\n%s", out)
	}
	if !strings.Contains(out, `evil\nExec=/bin/sh`) {
		t.Fatalf("转义形态应是字面量 \\n：\n%s", out)
	}
}

func mustRender(t *testing.T, e *Env, in Instance, d App) []byte {
	t.Helper()
	raw, err := e.renderLauncher(in, d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
