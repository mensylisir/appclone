package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// probeEnv 造一个 XDG 全在家目录里、且带 user-dirs.dirs 的环境。
//
// 探测的 upper 是相对 $HOME 的，所以 XDG 必须在家目录内（testEnv 的 XDG 在
// 家目录外，那里本来就进不了 upper）；公共目录解析也不允许回退默认名，
// 所以必须有一份可读的 user-dirs.dirs。
func probeEnv(t *testing.T) *Env {
	t.Helper()
	r := classifyEnv(t)
	e := NewEnvAt(filepath.Join(r.Home, ".local", "share", "appclone"), r)
	e.TmpRoot = filepath.Join(filepath.Dir(r.Home), "tmp")
	if err := os.MkdirAll(e.TmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"

	if err := os.MkdirAll(r.XDG["CONFIG_HOME"], 0o700); err != nil {
		t.Fatal(err)
	}
	body := "XDG_DOWNLOAD_DIR=\"$HOME/Downloads\"\n"
	if err := os.WriteFile(filepath.Join(r.XDG["CONFIG_HOME"], "user-dirs.dirs"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.Home, "Downloads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.Home, ".local", "share"), 0o700); err != nil {
		t.Fatal(err)
	}
	return e
}

// 探测 argv 的顺序是契约：公共目录的 bind 排在 overlay 之后，否则会被 overlay
// 罩回去——用户在探测会话里存进 ~/Downloads 的文件会落进 upper，探测结束删
// upper 就是删用户数据。这条断言是"第一红线"的机器化版本。
func TestProbeBwrapArgsOrder(t *testing.T) {
	spec := ProbeSpec{
		Home:        "/home/u",
		Base:        "/home/u/.local/share/appclone",
		Merged:      "/var/tmp/probe/merged",
		RuntimeDir:  "/var/tmp/probe/runtime",
		RuntimeSock: []BindPair{{Src: "/run/user/1000/wayland-0", Dst: "/var/tmp/probe/runtime/wayland-0"}},
		DBusAddress: "unix:path=/var/tmp/probe/dbus/bus",
		PublicBinds: []BindPair{
			{Src: "/home/u/Downloads", Dst: "/home/u/Downloads"},
			{Src: "/home/u/Documents", Dst: "/home/u/Documents"},
		},
		SharedWrite: []string{"/home/u/.local/share/recently-used.xbel"},
		LockBinds:   []BindPair{{Src: "/var/tmp/probe/locks/abc", Dst: "/home/u/.config/App/lock"}},
		Command:     []string{"/usr/bin/app", "--flag"},
	}
	argv, err := ProbeBwrapArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	order := []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc",
		"--bind / /", "--dev-bind /dev /dev", "--proc /proc",
		"--bind /var/tmp/probe/merged /home/u",
		"--bind /home/u/Downloads /home/u/Downloads",
		"--bind /home/u/Documents /home/u/Documents",
		"--bind /home/u/.local/share/appclone /home/u/.local/share/appclone",
		"--tmpfs /home/u/.local/share/appclone/instances",
		"--bind-try /home/u/.local/share/recently-used.xbel /home/u/.local/share/recently-used.xbel",
		"--bind /var/tmp/probe/locks/abc /home/u/.config/App/lock",
		"--bind /var/tmp/probe/runtime /var/tmp/probe/runtime",
		"--ro-bind-try /run/user/1000/wayland-0 /var/tmp/probe/runtime/wayland-0",
		"--setenv XDG_RUNTIME_DIR /var/tmp/probe/runtime",
		"--setenv DBUS_SESSION_BUS_ADDRESS unix:path=/var/tmp/probe/dbus/bus",
		"--setenv HOME /home/u",
		"--die-with-parent",
		"-- /usr/bin/app --flag",
	}
	at := 0
	for _, want := range order {
		idx := strings.Index(joined[at:], want)
		if idx < 0 {
			t.Fatalf("argv 缺少或顺序不对：期望 %q 出现在 %q 之后\n实际：%s", want, joined[:at], joined)
		}
		at += idx + len(want)
	}
}

func TestProbeBwrapArgsRejectsEmptyCommand(t *testing.T) {
	if _, err := ProbeBwrapArgs(ProbeSpec{Home: "/home/u", Merged: "/m"}); ExitCode(err) != CodeInput {
		t.Fatalf("没有命令应报输入错误（2），得到 %v", err)
	}
	if _, err := ProbeBwrapArgs(ProbeSpec{Home: "relative", Merged: "/m", Command: []string{"x"}}); err == nil {
		t.Fatal("相对 HOME 必须拒绝")
	}
}

// 公共目录解析不许回退默认名（§3.2.1）：读不到 user-dirs.dirs 就禁用探测，
// 否则中文系统的 `~/下载` 漏出摘出清单，用户存的文件会落进 upper 被删。
func TestProbePublicBindsStrict(t *testing.T) {
	e := probeEnv(t)
	binds, err := e.probePublicBinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 || binds[0].Src != filepath.Join(e.Resolver.Home, "Downloads") {
		t.Fatalf("应只解析出 user-dirs.dirs 里配置的那一项，得到 %+v", binds)
	}
	// 目录不存在就不 bind：--bind 的源不存在会让整条命令失败。
	if err := os.RemoveAll(filepath.Join(e.Resolver.Home, "Downloads")); err != nil {
		t.Fatal(err)
	}
	if binds, err = e.probePublicBinds(); err != nil || len(binds) != 0 {
		t.Fatalf("不存在的公共目录不该 bind，得到 %+v / %v", binds, err)
	}
	// 缺 user-dirs.dirs → 报环境错误，不是静默回退。
	if err := os.Remove(filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "user-dirs.dirs")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.probePublicBinds(); ExitCode(err) != CodeEnv {
		t.Fatalf("缺 user-dirs.dirs 应报 3，得到 %v", err)
	}
}

// 公共目录被指到家目录之外/本身：禁用探测，不许"猜一个"（否则 bind 会把
// 整个 overlay 遮掉，探测变成在真实家目录里跑）。
func TestProbePublicBindsRejectsOutsideHome(t *testing.T) {
	e := probeEnv(t)
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "user-dirs.dirs")
	for _, bad := range []string{"\"$HOME\"", "\"/etc\""} {
		body := "XDG_DOWNLOAD_DIR=" + bad + "\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.probePublicBinds(); ExitCode(err) != CodeInput {
			t.Fatalf("%s 应被拒（2），得到 %v", bad, err)
		}
	}
}

// 反向断言：被 bind 回真实的路径（公共目录 / AC_BASE / shared）出现在 upper 里。
func TestProbeContaminated(t *testing.T) {
	e := probeEnv(t)
	pubs := []BindPair{{Src: filepath.Join(e.Resolver.Home, "Downloads"), Dst: filepath.Join(e.Resolver.Home, "Downloads")}}
	entries := []UpperEntry{
		{Path: "Downloads/important.docx", Kind: EntryFile},
		{Path: ".config/App/x", Kind: EntryFile},
	}
	got := e.ProbeContaminated(entries, pubs)
	if len(got) != 1 || !strings.HasSuffix(got[0], filepath.FromSlash("Downloads/important.docx")) {
		t.Fatalf("应只报公共目录里那条，得到 %v", got)
	}
	if len(e.ProbeContaminated(entries[1:], pubs)) != 0 {
		t.Fatal("非公共目录的写入不算污染")
	}
	// AC_BASE 出现在 upper 里同样是 bind 失效（工具自己的写入不该进隔离层）。
	baseEntry := UpperEntry{Path: filepath.ToSlash(strings.TrimPrefix(filepath.Join(e.Base, "profiles.d", "x.toml"), e.Resolver.Home+"/")), Kind: EntryFile}
	if len(e.ProbeContaminated([]UpperEntry{baseEntry}, pubs)) != 1 {
		t.Fatalf("AC_BASE 下的写入应算污染：%+v", baseEntry)
	}
}

// fakeMounter 把"应用写了什么"直接写进 upper，模拟一次会话；
// 真实现要 /dev/fuse，单测环境没有。
type fakeMounter struct {
	entries []string
	failOn  string
	// availableErr 精确控制 Available 的错误；failOn=="available" 固定返回
	// no_dev_fuse，这里用来模拟"binary 在、但挂载被拒"的权限失败。
	availableErr error
	mounted      bool
	// sentinel 在 merged（模拟的 $HOME 视图）里放一个文件。真实实现里它是
	// 挂载点；如果代码在挂载状态下 RemoveAll(work)，这个文件会先被删掉——
	// 用它来证明"先卸载、后处置目录"的顺序。
	sentinel string
}

func (f *fakeMounter) Available() error {
	if f.availableErr != nil {
		return f.availableErr
	}
	if f.failOn == "available" {
		return errEnv("err.probe.no_dev_fuse", nil, nil)
	}
	return nil
}

func (f *fakeMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	if f.failOn == "mount" {
		return nil, errEnv("err.probe.mount_failed", map[string]any{"output": "boom"}, nil)
	}
	if f.failOn == "umount" {
		f.mounted = true
		return func() error {
			return errEnv("err.probe.umount_failed", map[string]any{"output": "busy"}, nil)
		}, nil
	}
	f.mounted = true
	if f.sentinel != "" {
		if err := os.MkdirAll(merged, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(merged, f.sentinel), []byte("home"), 0o600); err != nil {
			return nil, err
		}
	}
	for _, rel := range f.entries {
		p := filepath.Join(upper, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			return nil, err
		}
	}
	return func() error { f.mounted = false; return nil }, nil
}

type fakeRunner struct {
	res   SessionResult
	err   error
	argv  []string
	calls int
	// step 按第几次调用返回不同结果（多轮场景）；为 nil 时用 res/err。
	step func(call int) (SessionResult, error)
}

func (f *fakeRunner) Run(_ context.Context, argv []string, _ io.Writer) (SessionResult, error) {
	f.argv = argv
	f.calls++
	if f.step != nil {
		return f.step(f.calls)
	}
	return f.res, f.err
}

// 正常一次探测：upper 里的写入 → 候选 → 档案；探测目录必须被清掉。
func TestProbeSessionBuildsProfile(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{
		".config/TestApp/config.json",
		".local/share/TestApp/db.dat",
		".cache/TestApp/blob",
		".config/gtk-3.0/bookmarks",
	}}
	r := &fakeRunner{}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app", "--flag"},
		Mounter: m, Runner: r,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.ProbeMethod != "overlay" {
		t.Fatalf("档案应记 probe_method=overlay，得到 %q", res.Profile.ProbeMethod)
	}
	// 档案必须自带启动命令：ValidateProfile 会因为它为空直接拒（§5）。
	if res.Profile.Exec != "/nonexistent/zz-probe-test-app" {
		t.Fatalf("Exec 必须回填，得到 %q", res.Profile.Exec)
	}
	if len(res.Profile.ExecArgs) != 1 || res.Profile.ExecArgs[0] != "--flag" {
		t.Fatalf("ExecArgs 必须回填，得到 %v", res.Profile.ExecArgs)
	}
	if err := ValidateProfile(res.Profile, e.BuildOptions()); err != nil {
		t.Fatalf("探测产物必须能过 ValidateProfile：%v", err)
	}
	paths := strings.Join(res.Profile.Paths, ",")
	if !strings.Contains(paths, "~/.config/TestApp") || !strings.Contains(paths, "~/.local/share/TestApp") {
		t.Fatalf("应用目录应进 paths：%v", res.Profile.Paths)
	}
	if strings.Contains(paths, "~/.cache/TestApp") {
		t.Fatalf("缓存不该进 paths：%v", res.Profile.Paths)
	}
	if shared := strings.Join(res.Profile.SharedWrites, ","); !strings.Contains(shared, "gtk-3.0") {
		t.Fatalf("书签所在目录应进 shared_writes：%v", res.Profile.SharedWrites)
	}
	if m.mounted {
		t.Fatal("探测结束后必须卸载")
	}
}

// 污染：公共目录里出现写入 → 结果作废（4）、**保留现场**、不删任何东西。
func TestProbeSessionQuarantinesOnContamination(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{"Downloads/important.docx"}}
	r := &fakeRunner{}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if ExitCode(err) != CodeProbe {
		t.Fatalf("污染是「环境给不出结论」（4），得到 %v", err)
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.contaminated" {
		t.Fatalf("应是 err.probe.contaminated，得到 %v", err)
	}
	// 现场必须还在（名字带 uid + 时间 + 随机尾巴），并且里面真有那个文件。
	ents, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-quarantine-*"))
	if len(ents) == 0 {
		t.Fatal("污染现场必须保留（quarantine-*），不能删掉可能属于用户的文件")
	}
	found := false
	for _, d := range ents {
		if _, err := os.Stat(filepath.Join(d, "upper", "Downloads", "important.docx")); err == nil {
			found = true
			os.RemoveAll(d) // 只清理本测试造出来的那个现场
		}
	}
	if !found {
		t.Fatal("quarantine 里应含被写进 upper 的用户文件")
	}
}

// 会话跑起来了但立刻失败（bwrap 建不了 namespace / Exec 不存在）≠ 零写入：
// 前者是环境问题（3），必须和"应用正常退出但没写"分开。
func TestProbeSessionDistinguishesFailedSession(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{}
	r := &fakeRunner{res: SessionResult{ExitCode: 1}}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if ExitCode(err) != CodeEnv {
		t.Fatalf("会话起不来应报 3，得到 %v", err)
	}
}

// 卸载失败时**不许**删工作目录：那时删会遍历用户家目录视图并制造 whiteout。
func TestProbeSessionKeepsWorkOnUmountFailure(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{failOn: "umount", entries: []string{".config/App/x"}}
	r := &fakeRunner{}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if err == nil {
		t.Fatal("卸载失败必须报错")
	}
	left, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-probe-*"))
	if len(left) == 0 {
		t.Fatal("卸载失败时工作目录必须保留（删了会碰用户家目录视图）")
	}
	for _, d := range left {
		os.RemoveAll(d)
	}
}

// 取消/超时发生在 overlay 还挂着的时候：必须先停会话、卸载，再处置工作目录。
//
// 反例是 probeRoundFailed 直接在挂载状态下 os.RemoveAll(work)——真实环境里
// RemoveAll 会穿过 merged 挂载点遍历整个 $HOME 视图，upper 里探测期写下的
// 文件（可能是用户刚保存的内容）会被 unlink，而 EBUSY 只在最后才报。
// 这里用 fakeMounter 在 merged 里放哨兵文件：它在挂载时被删掉，就说明
// 清理顺序错了。同时断言现场被 quarantine 而不是删除（§3.2.1）。
func TestProbeSessionCancelUnmountsBeforeCleanup(t *testing.T) {
	e := probeEnv(t)
	// 公共目录进 upper → 走 quarantine（只有这种情况会保留 merged）；
	// 哨兵文件用来证明清理发生在卸载之后。
	m := &fakeMounter{entries: []string{"Downloads/important.docx"}, sentinel: "user-file.txt"}
	r := &fakeRunner{err: context.Canceled}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("取消应返回 ErrCanceled，得到 %v", err)
	}
	if m.mounted {
		t.Fatal("取消后挂载点必须卸载")
	}
	dirs, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-quarantine-*"))
	found := false
	for _, d := range dirs {
		if _, statErr := os.Stat(filepath.Join(d, "merged", "user-file.txt")); statErr == nil {
			found = true
		}
		os.RemoveAll(d)
	}
	if !found {
		t.Fatal("取消后必须保留现场：merged 里的文件被提前删了（清理发生在卸载之前）")
	}
}

// strace 降级路径没有可保留的写层现场：超时必须用自己的文案，不能复用
// overlay 的 err.probe.timeout（那条带 {quarantine}，这里没有参数可填，
// 会直接给用户显示字面量）。
func TestProbeStraceTimeoutUsesOwnKey(t *testing.T) {
	e := probeEnv(t)
	r := &fakeRunner{err: context.DeadlineExceeded}
	_, err := e.probeStrace(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Runner: r}, r)
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.timeout_strace" {
		t.Fatalf("strace 超时应返回 err.probe.timeout_strace，得到 %v", err)
	}
}

// 取消时写层为空：没有现场可保留，不能留一个空 quarantine 目录。
func TestProbeSessionCancelEmptyUpperLeavesNoScene(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{}
	r := &fakeRunner{err: context.Canceled}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("取消应返回 ErrCanceled，得到 %v", err)
	}
	if dirs := quarantineDirs(t, e); len(dirs) != 0 {
		t.Fatalf("空写层不该留下 quarantine：%v", dirs)
	}
}

// 取消时 upper 里出现公共目录（bind 可能没生效）：必须保留现场，并把路径
// 包进仍可 errors.Is(ErrCanceled) 的错误里（否则用户不知道文件去哪了）。
func TestProbeSessionCancelContaminatedKeepsScene(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{"Downloads/important.docx"}}
	r := &fakeRunner{err: context.Canceled}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("取消必须仍能 errors.Is(ErrCanceled)，得到 %v", err)
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.canceled_kept" {
		t.Fatalf("污染取消应返回 err.probe.canceled_kept，得到 %v", err)
	}
	q, _ := ce.Args["quarantine"].(string)
	if q == "" {
		t.Fatal("错误里必须有恢复路径")
	}
	defer os.RemoveAll(q)
	if _, statErr := os.Stat(filepath.Join(q, "upper", "Downloads", "important.docx")); statErr != nil {
		t.Fatalf("quarantine 里应保留用户文件：%v", statErr)
	}
}

// 多轮场景：第 2 轮 guard 失败时，第 1 轮可能已写入公共目录——必须保留现场，
// 且错误文案要给出路径（不能再说"应用还没启动"）。
func TestProbeSessionMultiRoundGuardKeepsScene(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{"Downloads/important.docx"}}
	r := &fakeRunner{step: func(call int) (SessionResult, error) {
		if call == 1 {
			return SessionResult{}, nil
		}
		return SessionResult{ExitCode: probeGuardFailed}, nil
	}}
	_, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r, Rounds: 2,
	}, m, r)
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.bind_verify_failed_kept" {
		t.Fatalf("多轮 guard 失败应返回 kept 文案，得到 %v", err)
	}
	q, _ := ce.Args["quarantine"].(string)
	if q == "" {
		t.Fatal("错误里必须有恢复路径")
	}
	defer os.RemoveAll(q)
	if _, statErr := os.Stat(filepath.Join(q, "upper", "Downloads", "important.docx")); statErr != nil {
		t.Fatalf("quarantine 里应保留前几轮的写入：%v", statErr)
	}
}

// 多轮场景的普通启动错误：写层里只有应用自身配置（无公共目录污染）时，
// 与成功路径一样删掉写层——不能因为"轮次 > 0"就把纯配置写入也说成用户文件。
func TestProbeSessionMultiRoundStartErrorDiscardsCleanUpper(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/App/x"}}
	r := &fakeRunner{step: func(call int) (SessionResult, error) {
		if call == 1 {
			return SessionResult{}, nil
		}
		return SessionResult{}, errEnv("err.probe.start_failed", map[string]any{"exec": "x"}, nil)
	}}
	_, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r, Rounds: 2,
	}, m, r)
	if err == nil {
		t.Fatal("第 2 轮起不来必须报错")
	}
	if dirs := quarantineDirs(t, e); len(dirs) != 0 {
		t.Fatalf("无污染的多轮失败不该留 quarantine：%v", dirs)
	}
	if dirs, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-probe-*")); len(dirs) != 0 {
		t.Fatalf("工作目录应被清掉：%v", dirs)
	}
}

func quarantineDirs(t *testing.T, e *Env) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-quarantine-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// ADR-032：只有后端真正缺失才允许降级。fuse-overlayfs 在、但挂载被拒
// （权限/沙盒）时必须把错误交给用户，不能在 strace 可用时静默降级——那会
// 掩盖可修复的权限问题，还给出一份低置信度档案。
func TestProbeBackendMissingOnlyForAbsentBackend(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"err.probe.no_fuse_overlayfs", true},
		{"err.probe.no_fusermount", true},
		{"err.probe.no_dev_fuse", true},
		{"err.probe.mount_failed", false},
		{"err.probe.mount_timeout", false},
		{"err.probe.umount_failed", false},
	}
	for _, c := range cases {
		if got := ProbeBackendMissing(errEnv(c.key, nil, nil)); got != c.want {
			t.Fatalf("ProbeBackendMissing(%s)=%v want %v", c.key, got, c.want)
		}
	}
	if ProbeBackendMissing(errors.New("plain")) {
		t.Fatal("非 core.Error 不应被判成后端缺失")
	}
}

func TestProbeDoesNotDowngradeWhenMountIsDenied(t *testing.T) {
	e := probeEnv(t)
	// 放一个假 strace：旧逻辑看到它就会静默降级；新逻辑必须仍然返回
	// mount_failed，让界面告诉用户去修权限/换会话。
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "strace"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	denied := errEnv("err.probe.mount_failed", map[string]any{"output": "fusermount3: mount failed: Permission denied"}, nil)
	m := &fakeMounter{availableErr: denied}
	r := &fakeRunner{}
	var stderr strings.Builder
	_, err := e.Probe(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/bin/true"}, Mounter: m, Runner: r, Stderr: &stderr,
	})
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.mount_failed" {
		t.Fatalf("挂载被拒应原样返回 mount_failed，得到 %v", err)
	}
	if r.calls != 0 {
		t.Fatalf("挂载被拒时不应降级 strace，runner 被调用 %d 次", r.calls)
	}
	if strings.Contains(stderr.String(), "改用 strace") {
		t.Fatalf("不应向用户宣告降级：%q", stderr.String())
	}
}

// 四道闸门：overlay 不可用时不许硬试。
func TestProbeSessionGateOnMounter(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{failOn: "available"}
	r := &fakeRunner{}
	_, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if ExitCode(err) != CodeEnv {
		t.Fatalf("overlay 不可用应报 3，得到 %v", err)
	}
}

// 目标应用在跑：退出码是 6（并发），不是 7（实例在跑）。
func TestProbeSessionGateOnRunningApp(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{}
	r := &fakeRunner{}
	// 需要一个**别的进程**在跑目标可执行文件（自己的 pid 会被排除）。
	dir := t.TempDir()
	target := filepath.Join(dir, "zzappclone-probe-target")
	payload, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skipf("读不到 /bin/sleep：%v", err)
	}
	if err := os.WriteFile(target, payload, 0o700); err != nil {
		t.Skipf("写不了目标可执行文件：%v", err)
	}
	cmd := exec.Command(target, "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("起不了目标进程：%v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	if _, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{target}, Mounter: m, Runner: r}, m, r); ExitCode(err) != CodeBusy {
		t.Fatalf("应用在跑应报 6，得到 %v", err)
	}
}

// root 必须拒绝：探测会 bind 宿主目录，root 下的行为掩盖非特权问题。
func TestProbeRefusesRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("仅当以 root 跑时这条才有意义")
	}
	e := probeEnv(t)
	if _, err := e.Probe(context.Background(), "testapp", ProbeOptions{Command: []string{"/bin/true"}}); ExitCode(err) != CodeEnv {
		t.Fatalf("root 下探测应被拒（3），得到 %v", err)
	}
}

// 端到端：探测产物必须能直接建出实例，即使宿主上那个目录**还不存在**
// （首次配置就是这样：写入只发生在 overlay 的 upper 里）。
// 这条断言是上一轮 P0 的回归锁：缺 Exec 或要求源已存在，用户会卡在
// "探测成功 → 档案已失效 → 再探测 → 仍然失效"的循环里。
func TestProbeProfileCreatesInstanceForFreshApp(t *testing.T) {
	e := probeEnv(t)
	// 桌面项：Exec 指向真实存在的可执行文件。
	bin := filepath.Join(e.Resolver.Home, "freshapp-bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	appsDir := filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications")
	if err := os.MkdirAll(appsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	desktop := "[Desktop Entry]\nType=Application\nName=Fresh App\nExec=" + bin + "\nIcon=fresh\nCategories=Utility;\n"
	if err := os.WriteFile(filepath.Join(appsDir, "freshapp.desktop"), []byte(desktop), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &fakeMounter{entries: []string{".config/FreshApp/settings.ini"}}
	r := &fakeRunner{}
	res, err := e.probeSession(context.Background(), "freshapp",
		ProbeOptions{Command: []string{bin}, Mounter: m, Runner: r}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	// 宿主上确实还不存在（探测只写 upper）。
	hostDir := filepath.Join(e.Resolver.Home, ".config", "FreshApp")
	if _, err := os.Stat(hostDir); !os.IsNotExist(err) {
		t.Fatalf("这一步宿主目录不该已经存在：%v", err)
	}
	in, err := e.CreateInstance(context.Background(), "freshapp", res.Profile, "")
	if err != nil {
		t.Fatalf("探测产物应能直接建实例：%v", err)
	}
	if _, err := os.Stat(hostDir); err != nil {
		t.Fatalf("建实例时应补建宿主目录：%v", err)
	}
	if in.ID == "" {
		t.Fatal("实例 id 不能为空")
	}
}

// 有公共目录时，会话命令被包一层守卫脚本：`sh -c <guard> -- <app...>`。
// 守卫必须在 app 之前，且用 `--` 当 $0，否则 `exec "$@"` 会漏掉第一个参数。
func TestProbeBwrapArgsWrapsWithBindGuard(t *testing.T) {
	spec := ProbeSpec{
		Home:        "/home/u",
		Merged:      "/var/tmp/probe/merged",
		PublicBinds: []BindPair{{Src: "/home/u/Downloads", Dst: "/home/u/Downloads"}},
		Env:         probeGuardEnv([]BindPair{{Src: "/home/u/Downloads", Dst: "/home/u/Downloads"}}),
		Command:     []string{"/usr/bin/app", "--flag"},
	}
	argv, err := ProbeBwrapArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	idx := strings.Index(joined, "-- /bin/sh -c ")
	if idx < 0 {
		t.Fatalf("有公共目录时必须包守卫脚本：%s", joined)
	}
	if !strings.HasSuffix(joined, " -- /usr/bin/app --flag") {
		t.Fatalf("app 必须原样接在守卫之后：%s", joined)
	}
	// 目录通过环境变量传（空格安全的换行分隔），不能拼进脚本。
	if !strings.Contains(joined, "--setenv AC_PROBE_PUBLICS /home/u/Downloads") {
		t.Fatalf("公共目录应经环境变量传入：%s", joined)
	}
	// 没有公共目录时不包，保持最小惊讶。
	noPub, err := ProbeBwrapArgs(ProbeSpec{Home: "/home/u", Merged: "/m", Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(noPub, " "), "/bin/sh -c") {
		t.Fatalf("没有公共目录时不该包守卫：%v", noPub)
	}
}

// 守卫脚本本身的逻辑要能跑：这是"正向断言"唯一可离线验证的部分。
//   - 公共目录与 $HOME 同一设备（= 还在 overlay 上）→ 退出 91；
//   - 公共目录在另一个文件系统（= bind 生效）→ 放行，正常 exec 应用。
func TestProbeBindGuardScript(t *testing.T) {
	home := t.TempDir()
	inside := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(pub string) int {
		cmd := exec.Command("/bin/sh", "-c", probeBindGuard, "--", "/bin/true")
		cmd.Env = []string{"HOME=" + home, "AC_PROBE_PUBLICS=" + pub, "PATH=/usr/bin:/bin"}
		_ = cmd.Run()
		if cmd.ProcessState == nil {
			t.Fatalf("守卫生进程没有状态")
		}
		return cmd.ProcessState.ExitCode()
	}
	if code := run(inside); code != probeGuardFailed {
		t.Fatalf("同一设备（overlay 内）应报 %d，得到 %d", probeGuardFailed, code)
	}
	// /proc 一定在别的文件系统上，用它模拟"bind 生效的真实目录"。
	if code := run("/proc"); code != 0 {
		t.Fatalf("跨设备（bind 生效）应放行，得到 %d", code)
	}
}

// 一轮不够：登录态/token 往往第二次启动才落盘（§4）。默认跑两轮，
// 且两轮共用同一份 overlay 写层（第二轮能看到第一轮的写入，取并集）。
func TestProbeRunsTwoRoundsByDefault(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fakeRunner{}
	var stderr strings.Builder
	if _, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r,
		Stderr: &stderr,
	}, m, r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("默认应跑两轮，实际 %d 轮", r.calls)
	}
	// 每轮都要告诉用户"该做什么"，否则用户不知道要开两次。
	if strings.Count(stderr.String(), "轮") < 2 {
		t.Fatalf("两轮都应打印提示，实际：%q", stderr.String())
	}
	// 显式指定轮数时以它为准（排障时只跑一轮更快）。
	r2 := &fakeRunner{}
	if _, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r2, Rounds: 1,
	}, m, r2); err != nil {
		t.Fatal(err)
	}
	if r2.calls != 1 {
		t.Fatalf("显式 Rounds=1 时只该跑一轮，实际 %d", r2.calls)
	}
}

// 残留 upper 读不出来时是"看不见内容"，不是"空"：整棵删掉可能就是删用户
// 文件。与 sweepOneAudit 同口径——搬去 quarantine，源不再留着挡下一次探测。
func TestSweepStaleProbesQuarantinesUnreadableUpper(t *testing.T) {
	e := probeEnv(t)
	d := filepath.Join(e.TmpRoot, fmt.Sprintf("appclone-probe-%d-test", e.UID))
	upper := filepath.Join(d, "upper")
	secret := filepath.Join(upper, "secret")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "file"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 清扫再次回退时 000 现场会留在写层根：失败路径也要能清掉 TempDir，
		// 否则真正的失败原因会被 RemoveAll 的权限错误盖住。
		_ = filepath.WalkDir(e.TmpRoot, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})

	if err := e.sweepStaleProbes(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Fatal("读不出内容的残留应被搬走，不该留在写层根挡住下一次探测")
	}
	qs := quarantineDirs(t, e)
	if len(qs) != 1 {
		t.Fatalf("应留下 1 个 quarantine，得到 %v", qs)
	}
	if _, err := os.Stat(filepath.Join(qs[0], "upper", "secret")); err != nil {
		t.Fatalf("quarantine 里应保留原 upper 子树：%v", err)
	}
}

// 冒烟目录只清除本 uid、且已停手 10 分钟以上的：并发进程正在冒烟时删它
// 会把"能挂"变成假失败；panic/SIGKILL 留下的则不该在 /tmp 里攒着。
func TestSweepStaleSmokeKeepsFreshAndForeign(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "appclone-fusesmoke-old")
	fresh := filepath.Join(root, "appclone-fusesmoke-fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(filepath.Join(d, "merged"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	sweepStaleSmoke(root)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("一小时前的冒烟目录应被清掉")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("刚建的冒烟目录不能删（可能是并发进程正在用）：%v", err)
	}
}

// 清扫判断属主必须用进程真实 uid，而不是 Env.UID：Env.UID 在单测里被注入
// 成固定值，CI runner 的 uid 却不是 1000。用 e.UID 比较会把本进程刚留下的
// 残留全当成"别人的"跳过，残留永远清不掉（GitHub runner 上就是这样红的）。
func TestSweepOwnershipUsesProcessUID(t *testing.T) {
	e := probeEnv(t)
	e.UID = os.Getuid() + 1
	d := filepath.Join(e.TmpRoot, fmt.Sprintf("appclone-probe-%d-own", e.UID))
	upper := filepath.Join(d, "upper")
	if err := os.MkdirAll(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	// 非空 upper 才会走 quarantine 分支；空目录走 RemoveAll。两条都要按真实
	// 属主判定，错配时不能把本进程的残留当成别人的跳过。
	if err := os.WriteFile(filepath.Join(upper, "evidence"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.sweepStaleProbes(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Fatalf("本进程留下的残留应按真实 uid 判定属主并搬走：%v", err)
	}
	qs := quarantineDirs(t, e)
	if len(qs) != 1 {
		t.Fatalf("应留下 1 个 quarantine，得到 %v", qs)
	}
	if _, err := os.Stat(filepath.Join(qs[0], "upper", "evidence")); err != nil {
		t.Fatalf("quarantine 里应保留原 upper 文件：%v", err)
	}
}

// 负向边界：属主不是本进程的目录必须跳过，不能被当成残留处理。
func TestDirOwnedByRejectsForeignOwner(t *testing.T) {
	root := t.TempDir()
	if dirOwnedBy(root, os.Getuid()+1) {
		t.Fatal("属主判断不能把别人的目录当成自己的")
	}
	if !dirOwnedBy(root, os.Getuid()) {
		t.Fatal("本进程的目录应被认作自己的")
	}
}
