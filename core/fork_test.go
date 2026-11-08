package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mountCall struct {
	lower, upper, work, merged string
}

// recordingMounter 记录挂载参数，用来钉住"lower 必须是主实例数据、upper 必须
// 是实例 root"这条克隆语义。
type recordingMounter struct {
	calls       []mountCall
	available   error
	mountErr    error
	unmountErr  error
	unmountCall int
}

func (m *recordingMounter) Available() error { return m.available }

func (m *recordingMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	if m.mountErr != nil {
		return nil, m.mountErr
	}
	m.calls = append(m.calls, mountCall{lower, upper, work, merged})
	return func() error {
		m.unmountCall++
		return m.unmountErr
	}, nil
}

// fork 实例的元数据必须落盘并能读回：Fork 是数据层来源，丢了它就等于
// 下次 run 静默退化成"空分身"。
func TestCreateForkInstancePersists(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	if !in.Fork {
		t.Fatal("返回的实例应标记 fork")
	}
	got, err := e.GetInstance(app, "work")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fork {
		t.Fatalf("instance.toml 没有持久化 fork：%+v", got)
	}

	// 普通实例不能被带上 fork（默认零值）。
	plain, err := e.CreateInstance(context.Background(), app, p, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Fork {
		t.Fatal("未指定 --fork 的实例不应标记 fork")
	}
}

// forkMounts：lower=主实例真实目录、upper=实例 root 下的同名子路径，
// 返回的 bind 源是挂载点；cleanup 必须卸载并删掉临时目录。
func TestForkMountsBindsPrimaryData(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	primary := filepath.Join(e.Resolver.Home, ".config", "TestApp")
	if err := os.WriteFile(filepath.Join(primary, "state.db"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	fm := &recordingMounter{}
	e.Mounter = fm

	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	abs := primary
	upper, err := instanceChild(in.Root, abs)
	if err != nil {
		t.Fatal(err)
	}

	sources, cleanup := e.forkMounts(p, in, nil, func(string) {})
	if len(fm.calls) != 1 {
		t.Fatalf("应为主实例数据挂一次 overlay，得到 %d 次：%+v", len(fm.calls), fm.calls)
	}
	c := fm.calls[0]
	if c.lower != abs {
		t.Fatalf("lower 必须是主实例真实目录：want %q got %q", abs, c.lower)
	}
	if c.upper != upper {
		t.Fatalf("upper 必须是实例 root 下的持久层：want %q got %q", upper, c.upper)
	}
	if !strings.HasPrefix(c.merged, forkDir(in)) || !strings.HasPrefix(c.work, forkDir(in)) {
		t.Fatalf("挂载点/工作区必须在 <inst>/.fork 下：%+v", c)
	}
	merged, ok := sources[abs]
	if !ok || merged != c.merged {
		t.Fatalf("bind 源应指向挂载点：sources=%v merged=%q", sources, merged)
	}

	cleanup()
	if fm.unmountCall != 1 {
		t.Fatalf("cleanup 必须卸载，实际 %d 次", fm.unmountCall)
	}
	if _, err := os.Stat(forkDir(in)); !os.IsNotExist(err) {
		t.Fatalf("卸载成功后临时目录应删除，stat=%v", err)
	}
}

// 主实例目录为空时不做无意义的挂载；fuse 不可用时应整体降级并告警。
func TestForkMountsSkipsEmptyAndUnavailable(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	fm := &recordingMounter{}
	e.Mounter = fm
	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}

	// seedApp 只建了空目录。
	sources, cleanup := e.forkMounts(p, in, nil, func(string) {})
	cleanup()
	if len(sources) != 0 || len(fm.calls) != 0 {
		t.Fatalf("空目录不应挂 overlay：sources=%v calls=%v", sources, fm.calls)
	}

	// 有内容但 fuse 不可用：降级为普通实例目录，并告诉用户。
	if err := os.WriteFile(filepath.Join(e.Resolver.Home, ".config", "TestApp", "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fm.available = errEnv("err.probe.no_dev_fuse", nil, nil)
	var warns []string
	sources, cleanup = e.forkMounts(p, in, nil, func(msg string) { warns = append(warns, msg) })
	cleanup()
	if len(sources) != 0 {
		t.Fatalf("fuse 不可用时不应给出 bind 源：%v", sources)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "fuse-overlayfs") {
		t.Fatalf("应告警一次说明降级原因，得到 %v", warns)
	}
}

// mountinfo 对空格/制表符/换行/反斜杠做八进制转义。stale 挂载判断如果按
// 原样路径做子串匹配，AC_BASE 带空格（"My Data"）时会漏判，接着 RemoveAll
// 会穿过挂载点删主实例数据——这条断言就是那个边界的回归。
func TestMountInfoHasEscapedPaths(t *testing.T) {
	data := "" +
		"36 25 0:33 / /home/u/My\\040Data rw,relatime - fuse.overlayfs fuse-overlayfs rw\n" +
		"37 25 0:34 / /home/u/tab\\011dir rw,relatime - ext4 /dev/sda1 rw\n" +
		"38 25 0:35 / /home/u/back\\134slash rw,relatime - ext4 /dev/sda1 rw\n"
	for _, p := range []string{"/home/u/My Data", "/home/u/tab\tdir", `/home/u/back\slash`} {
		if !mountInfoHas(data, p) {
			t.Fatalf("转义后的挂载点必须能识别：%q", p)
		}
	}
	// 近似路径不能被误认成同一个挂载点。
	if mountInfoHas(data, "/home/u/My") || mountInfoHas(data, "/home/u/My Data/extra") {
		t.Fatal("只允许整路径相等，前缀/子路径不能算同一挂载点")
	}
}

// 非 fork 实例完全不碰 fuse，也不建 .fork 目录。
func TestForkMountsNoopForPlainInstance(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	fm := &recordingMounter{}
	e.Mounter = fm
	in, err := e.CreateInstance(context.Background(), app, p, "plain")
	if err != nil {
		t.Fatal(err)
	}
	sources, cleanup := e.forkMounts(p, in, nil, func(string) { t.Fatal("普通实例不该告警") })
	cleanup()
	if sources != nil || len(fm.calls) != 0 {
		t.Fatalf("普通实例不应挂载：sources=%v calls=%v", sources, fm.calls)
	}
	if _, err := os.Stat(forkDir(in)); !os.IsNotExist(err) {
		t.Fatalf("普通实例不该创建 .fork，stat=%v", err)
	}
}

// 软链是常态（/home → /var/home、~/.config → ~/cfg）：mountinfo 记的是解析后
// 路径，调用方拿的是词法路径。只比一种形态会让挂载轮询超时、stale 漏判。
func TestMountInfoHasResolvesSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "dir", "merged")
	data := "1 2 0:3 / " + want + " rw - ext4 /dev/sda1 rw\n"
	if !mountInfoHas(data, filepath.Join(link, "dir", "merged")) {
		t.Fatal("词法路径带软链时必须命中 mountinfo 的真实路径")
	}
}

// 内核只转义 ASCII 空白；U+3000/NBSP 会原样留在字段里。按 Unicode 空白
// 分词会把第 5 字段截断，stale 判断随即失效。
func TestMountInfoHasUnicodeSpaces(t *testing.T) {
	data := "1 2 0:3 / /home/u/ideo　space rw - ext4 /dev/sda1 rw\n"
	if !mountInfoHas(data, "/home/u/ideo　space") {
		t.Fatal("U+3000 不得被当成分隔符")
	}
}

// 兜底护栏：目录里只剩一个仍挂在 mountinfo 的 merged、且卸不掉时，
// resetForkBase 必须报错并保留 base，绝不能 RemoveAll 穿过去。
func TestResetForkBaseRefusesRemoveWhileMounted(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	base := forkDir(in)
	merged := filepath.Join(base, "deadbeef", "merged")
	if err := os.MkdirAll(merged, 0o700); err != nil {
		t.Fatal(err)
	}
	old := procMountInfo
	defer func() { procMountInfo = old }()
	fixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(fixture, []byte("1 2 0:3 / "+merged+" rw - fuse.overlayfs fuse-overlayfs rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procMountInfo = fixture
	if err := resetForkBase(base); err == nil {
		t.Fatal("挂载仍存在且卸不掉时必须拒绝删除")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("拒绝删除时 base 必须保留：%v", err)
	}

	// 现场消失后（mountinfo 为空）同样的调用才允许删。
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	procMountInfo = empty
	if err := resetForkBase(base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("无挂载时应删除 base，stat=%v", err)
	}
}

// Mount 已报错但挂载点刚好生效：不能 RemoveAll(wd)，否则会穿过 merged。
func TestForkMountsKeepsWorkDirWhenMountRaisedButPresent(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	primary := filepath.Join(e.Resolver.Home, ".config", "TestApp")
	if err := os.WriteFile(filepath.Join(primary, "state.db"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Mounter = &recordingMounter{mountErr: errEnv("err.probe.mount_timeout", nil, nil)}
	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(forkDir(in), forkHash(primary), "merged")
	if err := os.MkdirAll(merged, 0o700); err != nil {
		t.Fatal(err)
	}
	old := procMountInfo
	defer func() { procMountInfo = old }()
	fixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(fixture, []byte("1 2 0:3 / "+merged+" rw - fuse.overlayfs fuse-overlayfs rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procMountInfo = fixture

	sources, cleanup := e.forkMounts(p, in, nil, func(string) {})
	cleanup()
	if len(sources) != 0 {
		t.Fatalf("Mount 报错的路径不应成为 bind 源：%v", sources)
	}
	if _, err := os.Stat(merged); err != nil {
		t.Fatalf("挂载仍在时不得删工作目录：%v", err)
	}
}

// lower 把 upper 包在里面会让 overlay 递归；这类路径必须跳过而不是硬挂。
func TestForkMountsSkipsLowerContainingUpper(t *testing.T) {
	// 自定义 AC_BASE 放在 XDG 根之外，档案路径取它的父目录：这是合法档案
	// （不碰 XDG/公共目录），但 upper 会落在 lower 之内。
	root := t.TempDir()
	home := filepath.Join(root, "home")
	getenv := func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return filepath.Join(root, "xdg", "config")
		case "XDG_DATA_HOME":
			return filepath.Join(root, "xdg", "data")
		case "XDG_STATE_HOME":
			return filepath.Join(root, "xdg", "state")
		case "XDG_CACHE_HOME":
			return filepath.Join(root, "xdg", "cache")
		case "XDG_RUNTIME_DIR":
			return filepath.Join(root, "run")
		}
		return ""
	}
	e := NewEnvAt(filepath.Join(root, "data", "appclone"), newResolver(home, home, 1000, getenv))
	e.TmpRoot = filepath.Join(root, "tmp")
	if err := os.MkdirAll(e.TmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"

	app, p := seedApp(t, e)
	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(root, "data")
	p.Paths = []string{ancestor}
	fm := &recordingMounter{}
	e.Mounter = fm
	var warns []string
	sources, cleanup := e.forkMounts(p, in, nil, func(msg string) { warns = append(warns, msg) })
	cleanup()
	if len(sources) != 0 || len(fm.calls) != 0 {
		t.Fatalf("upper 在 lower 内时不应挂载：sources=%v calls=%v", sources, fm.calls)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "无法克隆起步") {
		t.Fatalf("应告警说明该路径不继承，得到 %v", warns)
	}
}

// 删除实例前必须先卸掉 .fork 的残留挂载：带着挂载搬进回收站等于把 lower
// 视图交出去。卸不掉就拒绝删除，实例目录保持原样。
func TestDeleteInstanceRefusesWhenForkMountStuck(t *testing.T) {
	e := testEnv(t)
	app, p := seedApp(t, e)
	in, err := e.CreateInstanceWith(context.Background(), app, p, "work", CreateOptions{Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(forkDir(in), "cafe0001", "merged")
	if err := os.MkdirAll(merged, 0o700); err != nil {
		t.Fatal(err)
	}
	old := procMountInfo
	defer func() { procMountInfo = old }()
	fixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(fixture, []byte("1 2 0:3 / "+merged+" rw - fuse.overlayfs fuse-overlayfs rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procMountInfo = fixture

	err = e.DeleteInstance(context.Background(), app, "work")
	if err == nil {
		t.Fatal("挂载卸不掉时必须拒绝删除")
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.instance.fork_cleanup" {
		t.Fatalf("应返回 fork_cleanup 错误，得到 %v", err)
	}
	if _, serr := os.Stat(e.InstancePath(app, "work")); serr != nil {
		t.Fatalf("拒绝删除时实例目录必须保留：%v", serr)
	}
}
