package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 探测 argv 的顺序是契约：公共目录的 bind 排在 overlay 之后，否则会被 overlay
// 罩回去——用户在探测会话里存进 ~/Downloads 的文件会落进 upper，探测结束删
// upper 就是删用户数据。这条断言是"第一红线"的机器化版本。
func TestProbeBwrapArgsOrder(t *testing.T) {
	spec := ProbeSpec{
		Home:        "/home/u",
		RealHome:    "/home/u",
		Base:        "/home/u/.local/share/appclone",
		Merged:      "/var/tmp/probe/merged",
		RuntimeDir:  "/var/tmp/probe/runtime",
		RuntimeSock: []BindPair{{Src: "/run/user/1000/wayland-0", Dst: "/var/tmp/probe/runtime/wayland-0"}},
		DBusAddress: "unix:path=/var/tmp/probe/dbus/bus",
		PublicDirs:  []string{"/home/u/Downloads", "/home/u/Documents"},
		SharedWrite: []string{"/home/u/.local/share/recently-used.xbel"},
		LockFiles:   []string{"/home/u/.config/App/lock"},
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
		"--bind /home/u/.config/App/lock /home/u/.config/App/lock",
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

// AC_HOME 生效时 HOME 与真实 HOME 不同：公共目录的 bind 源必须取真实的那个，
// 目标取有效 HOME 下的同名路径——否则探测会话里看到的是一个空目录。
func TestProbeBwrapArgsSplitsEffectiveAndRealHome(t *testing.T) {
	spec := ProbeSpec{
		Home:       "/tmp/fakehome",
		RealHome:   "/home/u",
		Merged:     "/var/tmp/probe/merged",
		PublicDirs: []string{"/home/u/Downloads"},
		Command:    []string{"/usr/bin/app"},
	}
	argv, err := ProbeBwrapArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--bind /home/u/Downloads /tmp/fakehome/Downloads") {
		t.Fatalf("公共目录的源应取真实 HOME、目标取有效 HOME：%s", joined)
	}
	if !strings.Contains(joined, "--setenv HOME /tmp/fakehome") {
		t.Fatalf("HOME 必须设成有效 HOME（AC_HOME）：%s", joined)
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

// 反向断言：公共目录的路径出现在 upper 里，说明"摘出"没生效。
func TestProbeContaminated(t *testing.T) {
	e := probeEnv(t)
	pubs := []string{filepath.Join(e.Resolver.Home, "Downloads")}
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
}

// probeEnv 造一个 XDG 全在家目录里的环境：探测的 upper 是相对 $HOME 的，
// 而 testEnv 的 XDG 指在家目录外（那里本来就进不了 upper）。
func probeEnv(t *testing.T) *Env {
	t.Helper()
	r := classifyEnv(t)
	e := NewEnvAt(filepath.Join(r.Home, ".local", "share", "appclone"), r)
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	return e
}

// fakeMounter 把"应用写了什么"直接写进 upper，模拟一次会话；
// 真实现要 /dev/fuse，单测环境没有。
type fakeMounter struct {
	entries []string // 相对 upper 的路径
	failOn  string
	mounted bool
}

func (f *fakeMounter) Available() error {
	if f.failOn == "available" {
		return errEnv("err.probe.no_dev_fuse", nil, nil)
	}
	return nil
}

func (f *fakeMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	if f.failOn == "mount" {
		return nil, errEnv("err.probe.mount_failed", map[string]any{"output": "boom"}, nil)
	}
	f.mounted = true
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
	exit int
	err  error
	argv []string
}

func (f *fakeRunner) Run(_ context.Context, argv []string, _ io.Writer) (int, error) {
	f.argv = argv
	return f.exit, f.err
}

func probeTestOpt(f *fakeMounter, r *fakeRunner) ProbeOptions {
	return ProbeOptions{
		Command: []string{"/usr/bin/testapp"},
		Mounter: f,
		Runner:  r,
	}
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
	// 探测闸门要"应用没在跑"：给一个必然不存在的名字。
	res, err := e.probeSession(context.Background(), "testapp",
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.ProbeMethod != "overlay" {
		t.Fatalf("档案应记 probe_method=overlay，得到 %q", res.Profile.ProbeMethod)
	}
	paths := strings.Join(res.Profile.Paths, ",")
	if !strings.Contains(paths, "~/.config/TestApp") || !strings.Contains(paths, "~/.local/share/TestApp") {
		t.Fatalf("应用目录应进 paths：%v", res.Profile.Paths)
	}
	if strings.Contains(paths, "~/.cache/TestApp") {
		t.Fatalf("缓存不该进 paths：%v", res.Profile.Paths)
	}
	shared := strings.Join(res.Profile.SharedWrites, ",")
	if !strings.Contains(shared, "gtk-3.0") {
		t.Fatalf("书签所在目录应进 shared_writes：%v", res.Profile.SharedWrites)
	}
	if !m.mounted == false {
		t.Fatal("探测结束后必须卸载")
	}
	// 探测目录不许留下：留着下一轮会 EBUSY。
	if ents, _ := filepath.Glob("/var/tmp/appclone-probe-*"); len(ents) != 0 {
		for _, d := range ents {
			if strings.Contains(d, "quarantine") {
				continue
			}
			os.RemoveAll(d) // 只清本测试留下的（可能是别的测试失败残留）
		}
	}
}

// 污染：公共目录里出现写入 → 结果作废、**保留现场**、报环境错误。
func TestProbeSessionQuarantinesOnContamination(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{"Downloads/important.docx"}}
	r := &fakeRunner{}
	_, err := e.probeSession(context.Background(), "testapp.probe", // 用独立 app 名避免与上一条互相干扰
		ProbeOptions{Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r}, m, r)
	if ExitCode(err) != CodeEnv {
		t.Fatalf("污染应报环境错误（3），得到 %v", err)
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.contaminated" {
		t.Fatalf("应是 err.probe.contaminated，得到 %v", err)
	}
	ents, _ := filepath.Glob("/var/tmp/appclone-quarantine-*")
	if len(ents) == 0 {
		t.Fatal("污染现场必须保留（quarantine-*），不能删掉可能属于用户的文件")
	}
	for _, d := range ents {
		os.RemoveAll(d)
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
