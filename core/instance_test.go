package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedApp 造一个最小可用的应用 + 档案 + 实例，供实例/入口相关测试复用。
func seedApp(t *testing.T, e *Env) (string, Profile) {
	t.Helper()
	app := "testapp"
	dataDir := filepath.Join(e.Resolver.Home, ".config", "TestApp")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(e.Resolver.Home, "testapp-bin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// 桌面入口的源信息也造一份：CreateInstance 会读它来决定 Icon/Name。
	desktop := filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications", "testapp.desktop")
	if err := os.MkdirAll(filepath.Dir(desktop), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "[Desktop Entry]\nType=Application\nName=Test App\nExec=" + execPath + " %U\nIcon=test-icon\nCategories=Utility;\n"
	if err := os.WriteFile(desktop, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Profile{
		App:         app,
		Version:     ProfileVersion,
		Exec:        execPath,
		Paths:       []string{"~/.config/TestApp"},
		ProbeMethod: "manual",
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	return app, p
}

func TestCreateInstanceTransaction(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	ctx := context.Background()

	in, err := e.CreateInstance(ctx, app, p, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if in.ID != "main" {
		t.Fatalf("默认 id 应为 main：%s", in.ID)
	}
	if in.DisplayName != "分身 1" {
		t.Fatalf("默认显示名应为「分身 1」，得到 %q", in.DisplayName)
	}
	// 实例侧源目录必须已存在：⑤ 的源不存在会让整条 bwrap 命令失败（§3.2 第 3 条）。
	src := filepath.Join(in.Root, strings.TrimPrefix(filepath.Join(e.Resolver.Home, ".config", "TestApp"), "/"))
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		t.Fatalf("实例侧源不存在：%s", src)
	}
	// 桌面入口必须写出来，且 Exec 指向 CLI 绝对路径。
	entry, err := os.ReadFile(e.LauncherPath(app, in.ID))
	if err != nil {
		t.Fatalf("入口未生成：%v", err)
	}
	for _, want := range []string{"Type=Application", "Name[zh_CN]=Test App（分身 分身 1）", "X-Appclone-Id=main", "%U"} {
		if !strings.Contains(string(entry), want) {
			t.Fatalf("入口缺少 %q：\n%s", want, entry)
		}
	}

	// 重复同名 id：必须以 EEXIST 判重（退出 5），不许静默覆盖（§4.2）。
	if _, err := e.CreateInstance(ctx, app, p, "main"); ExitCode(err) != CodeConflict {
		t.Fatalf("同名 id 应冲突（5），得到 %v", err)
	}
	// 再建一个：id 自动分配 inst2，显示名取最小未占用。
	in2, err := e.CreateInstance(ctx, app, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if in2.ID != "inst2" || in2.DisplayName != "分身 2" {
		t.Fatalf("第二个实例应为 inst2/分身 2，得到 %s/%s", in2.ID, in2.DisplayName)
	}
}

func TestRenameKeepsFileAndRejectsDuplicate(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	ctx := context.Background()
	in1, _ := e.CreateInstance(ctx, app, p, "a")
	_, _ = e.CreateInstance(ctx, app, p, "b")

	launcher := e.LauncherPath(app, in1.ID)
	if err := e.RenameInstance(app, "a", "工作号"); err != nil {
		t.Fatal(err)
	}
	// 重命名不换文件名：换文件名会往回收站堆过期入口（§4）。
	data, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatalf("重命名后入口文件不见了：%v", err)
	}
	if !strings.Contains(string(data), "工作号") {
		t.Fatalf("入口 Name 未更新：%s", data)
	}
	// 同应用内撞名必须拒绝（§4 显示名规则）。
	if err := e.RenameInstance(app, "b", "工作号"); ExitCode(err) != CodeConflict {
		t.Fatalf("撞名应冲突（5），得到 %v", err)
	}
}

func TestIsRunningAndDeleteGuard(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	ctx := context.Background()
	in, err := e.CreateInstance(ctx, app, p, "work")
	if err != nil {
		t.Fatal(err)
	}

	if running, err := e.IsRunning(app, "work"); err != nil || running {
		t.Fatalf("刚建出来不该在跑：%v %v", running, err)
	}
	// 拿住 run.lock 模拟"正在运行"：删除必须拒绝（退出 7），且不替用户杀进程。
	lock, err := e.HoldRunLock(app, "work")
	if err != nil {
		t.Fatal(err)
	}
	if running, _ := e.IsRunning(app, "work"); !running {
		t.Fatal("持有锁时应判定为运行中")
	}
	if err := e.DeleteInstance(ctx, app, "work"); ExitCode(err) != CodeState {
		t.Fatalf("运行中删除应被拒（7），得到 %v", err)
	}
	lock.Close()
	if err := e.DeleteInstance(ctx, app, "work"); err != nil {
		t.Fatalf("释放后应可删除：%v", err)
	}
	_ = in
}

func TestProfileRoundtripKeepsUnknownFields(t *testing.T) {
	e := testEnv(t)
	dir := filepath.Join(e.Base, "profiles.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `version = 1
exec = "/bin/true"
paths = ["~/.config/X"]
future_field = "must survive"

[future_table]
a = 1
`
	if err := os.WriteFile(filepath.Join(dir, "x.toml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadProfile("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Unknown) != 2 {
		t.Fatalf("未知字段应被保留 2 个，得到 %d：%v", len(p.Unknown), p.Unknown)
	}
	p.Paths = []string{"~/.config/X", "~/.local/share/X"}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "x.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"future_field", "future_table", ".local/share/X"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("重写后丢了 %q：\n%s", want, out)
		}
	}
}

func TestProfileVersionAheadIsRejected(t *testing.T) {
	e := testEnv(t)
	dir := filepath.Join(e.Base, "profiles.d")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "y.toml"), []byte("version = 9\nexec = \"/bin/true\"\npaths = []\n"), 0o600)
	_, err := e.LoadProfile("y")
	if ExitCode(err) != CodeInput || errKey(err) != "err.profile.version_ahead" {
		t.Fatalf("超前版本必须拒绝加载（2），得到 %v", err)
	}
}

func TestRunRefusesRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("只有 root 会话才需要这条回归")
	}
	e := testEnv(t)
	app, p := seedApp(t, e)
	if _, err := e.CreateInstance(context.Background(), app, p, "work"); err != nil {
		t.Fatal(err)
	}
	// root 下 --bind / / 是 rw，误操作能改宿主系统；而且 root 能跑通会掩盖
	// 非特权路径的问题（§3.1 末段）。
	if _, err := e.Run(context.Background(), app, "work", RunOptions{}); ExitCode(err) != CodeEnv {
		t.Fatalf("root 必须被拒（3），得到 %v", err)
	}
}

func TestTimeFormatIsUTC(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	fixed := time.Date(2026, 11, 7, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	e.Clock = func() time.Time { return fixed }
	in, err := e.CreateInstance(context.Background(), app, p, "work")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(in.Dir(), "instance.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "2026-11-07T04:00:00Z") {
		t.Fatalf("时间必须是 RFC3339 UTC：\n%s", raw)
	}
}
