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

func TestDeleteInstanceRejectsPathTraversal(t *testing.T) {
	e := testEnv(t)
	// app 会拼进删除路径：`../../x` 这种值必须在入口就被拒，不能进到 gio trash。
	if err := e.DeleteInstance(context.Background(), "../../etc", "main"); ExitCode(err) != CodeInput {
		t.Fatalf("非法 app key 必须拒绝（2），得到 %v", err)
	}
	if _, err := e.GetInstance("../x", "main"); ExitCode(err) != CodeInput {
		t.Fatalf("GetInstance 也要校验 app key，得到 %v", err)
	}
}

func TestValidateRunnableRequiresExtraBindSource(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	_ = app
	p.ExtraBinds = []BindPair{{Src: "/nonexistent/path", Dst: "/opt/x"}}
	if err := e.ValidateRunnable(p); errKey(err) != "err.profile.extra_bind_missing" {
		t.Fatalf("额外绑定源不存在必须拒绝，得到 %v", err)
	}
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

// 单体文件也是应用状态（`~/.config/mimeapps.list`、`~/.apprc`）。
// 实例里把它建成目录，bwrap 就是"目录 bind 到文件目标"，整条命令失败——
// 而用户看到的只是"应用起不来"。
func TestCreateInstanceHandlesSingleFilePath(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	if err := os.MkdirAll(filepath.Join(e.Resolver.Home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	hostFile := filepath.Join(e.Resolver.Home, ".config", "mimeapps.list")
	if err := os.WriteFile(hostFile, []byte("[Default Applications]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Paths = []string{"~/.config/mimeapps.list"}
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(in.Root, strings.TrimPrefix(hostFile, "/"))
	st, err := os.Stat(src)
	if err != nil {
		t.Fatalf("实例侧占位物不存在：%v", err)
	}
	if st.IsDir() {
		t.Fatal("文件路径在实例里被建成了目录：bwrap 会把目录 bind 到文件目标，整条命令失败")
	}
}

// ignored / shared_writes 是 verify 的白名单：写一个 `~` 或 `/` 进去，
// 任何越界写入都会被判通过。这是最危险的一类假通过，必须在档案校验就拒掉。
func TestValidateProfileRejectsBlanketWhitelist(t *testing.T) {
	e := testEnv(t)
	_, p := seedApp(t, e)
	for _, bad := range []string{"~", "/"} {
		p.Ignored = []string{bad}
		if err := ValidateProfile(p, e.BuildOptions()); ExitCode(err) != CodeInput {
			t.Fatalf("ignored=[%q] 应被拒（2），得到 %v", bad, err)
		}
		p.Ignored = nil
		p.SharedWrites = []string{bad}
		if err := ValidateProfile(p, e.BuildOptions()); ExitCode(err) != CodeInput {
			t.Fatalf("shared_writes=[%q] 应被拒（2），得到 %v", bad, err)
		}
		p.SharedWrites = nil
	}
	// 正常值不能被误伤：公共目录写进 shared_writes 是合法的（"应用就是会写 Downloads"）。
	p.SharedWrites = []string{"~/Downloads"}
	if err := ValidateProfile(p, e.BuildOptions()); err != nil {
		t.Fatalf("公共目录在 shared_writes 里应合法，得到 %v", err)
	}
}

// 档案里的 paths 在宿主上被删掉之后：建实例要能补建（探测刚跑完时目录本来就
// 不存在），而不是报"档案已失效"——否则用户卡在"重新配置 → 仍然失效"里。
func TestCreateInstanceRecreatesMissingPath(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	gone := filepath.Join(e.Resolver.Home, ".config", "Gone")
	if err := os.MkdirAll(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	p.Paths = []string{"~/.config/Gone"}
	// 先确认它存在时能建出一个实例。
	if _, err := e.CreateInstance(context.Background(), app, p, "one"); err != nil {
		t.Fatalf("第一次建实例失败：%v", err)
	}
	// 删掉宿主目录，再用同一个档案建第二个实例：必须补建而不是报 stale。
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateInstance(context.Background(), app, p, "two"); err != nil {
		t.Fatalf("目录缺失时应补建而不是报 stale：%v", err)
	}
	if st, err := os.Stat(gone); err != nil || !st.IsDir() {
		t.Fatalf("宿主目录应被补建：%v", err)
	}
	// ProfileState 也不该因为目录缺失就把档案判成失效。
	hasProfile, creatable, err := e.ProfileState(app)
	if err != nil {
		t.Fatal(err)
	}
	if !hasProfile || !creatable {
		t.Fatalf("档案可创建：hasProfile=%v creatable=%v", hasProfile, creatable)
	}
}

// 实例目录布局是跨版本契约（CLI 与 GUI 必须一致，§4）。这里逐项断言，
// 因为布局一旦漂移，旧实例会"看起来还在、实际数据丢了"。
func TestInstanceLayout(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"root", "locks", "runtime", "dbus", "audit"} {
		dir := filepath.Join(in.Dir(), sub)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Fatalf("实例里缺少 %s/：%v", sub, err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s/ 的权限应为 0700，得到 %o", sub, perm)
		}
	}
	// instance.toml 是元数据的唯一权威来源，必须只有自己可读。
	st, err := os.Stat(filepath.Join(in.Dir(), "instance.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("instance.toml 权限应为 0600，得到 %o", perm)
	}
	// run.lock 在第一次取锁时才建（只读命令不许在实例目录里造文件）。
	if _, err := os.Stat(filepath.Join(in.Dir(), "run.lock")); !os.IsNotExist(err) {
		t.Fatal("建实例时不该预建 run.lock")
	}
	f, err := e.HoldRunLock(app, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err = os.Stat(filepath.Join(in.Dir(), "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("run.lock 权限应为 0600，得到 %o", perm)
	}
}

// 入口里的 CLI 路径必须跟着当前二进制走：appclone 升级/搬家后 TryExec 指向旧路径，
// 表现是"图标点了没反应"。repair-desktop 做的就是这件事（§6 命令表）。
func TestWriteLauncherFollowsCliPath(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	path := e.LauncherPath(app, "main")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "TryExec="+e.CliPath) {
		t.Fatalf("入口应指向当前 CLI：\n%s", first)
	}
	// 换一个 CLI 路径后重写同一入口（repair-desktop 的动作）。
	e.CliPath = "/opt/appclone/appclone"
	d, err := e.LoadApp(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.WriteLauncher(in, d); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "TryExec=/opt/appclone/appclone") ||
		!strings.Contains(string(second), "Exec=/opt/appclone/appclone run ") {
		t.Fatalf("重写后入口应指向新 CLI：\n%s", second)
	}
	// 入口文件名只由 app/id 决定，不随 CLI 路径或显示名变化。
	if e.LauncherPath(app, "main") != path {
		t.Fatal("入口文件名不能变（改名不换文件）")
	}
}

// 实例侧占位若是软链，补建会跟着它写到宿主任意位置（越出 §4 的写入白名单）；
// 指向 FIFO 时 OpenFile 还会挂死。必须拒绝。
func TestPrepareBindTargetsRejectsSymlinkPlaceholder(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := e.Resolver.Expand(p.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	src, err := instanceChild(in.Root, abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-target")
	if err := os.Symlink(outside, src); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareBindTargets(p, in); ExitCode(err) != CodeInput {
		t.Fatalf("软链占位应被拒（2），得到 %v", err)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatal("拒绝之后不得在宿主任意位置创建文件")
	}
}

// 宿主路径在"建实例 → 运行"之间从目录变成文件（或反之）时，bwrap 会
// "目录 bind 到文件目标"整条失败；必须给一个指向重新配置的明确错误。
func TestPrepareBindTargetsRejectsTypeChange(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := e.Resolver.Expand(p.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	src, err := instanceChild(in.Root, abs)
	if err != nil {
		t.Fatal(err)
	}
	// 宿主仍是目录（seedApp 建的），实例侧被换成普通文件。
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, perr := e.prepareBindTargets(p, in)
	if ExitCode(perr) != CodeInput {
		t.Fatalf("形态变化应被拒（2），得到 %v", perr)
	}
	// 必须是"去重新配置"的键：裸的环境错误会让用户以为工具坏了，
	// 而真正要做的是把档案里的路径重新探测/勾选一次。
	if errKey(perr) != "err.profile.bind_target_type" {
		t.Fatalf("应指向重新配置的错误键，得到 %v", perr)
	}
}

// 父级软链也必须拒：O_NOFOLLOW 只保护最后一段，`~/.config` 被换成指向
// 实例 root 之外的软链时，MkdirAll/OpenFile 会跟着它跑出去。
func TestPrepareBindTargetsRejectsSymlinkAncestor(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstance(context.Background(), app, p, "main")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := e.Resolver.Expand(p.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	src, err := instanceChild(in.Root, abs)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	// 把实例里的 .config 整个换成指向 root 之外的软链。
	ancestor := filepath.Dir(src)
	if err := os.RemoveAll(ancestor); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ancestor); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareBindTargets(p, in); ExitCode(err) != CodeInput {
		t.Fatalf("父级软链应被拒（2），得到 %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, filepath.Base(src))); !os.IsNotExist(err) {
		t.Fatal("拒绝之后不得沿父软链在 root 之外创建目录")
	}
}
