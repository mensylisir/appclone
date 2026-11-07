package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// recordingTrasher 记下调用参数并断言"确实被调用了"，替代真的 gio trash。
func recordingTrasher(e *Env) *[]string {
	var got []string
	e.Trasher = func(paths ...string) error {
		got = append(got, paths...)
		return nil
	}
	return &got
}

// 删除范围是"误删用户文件"的唯一防线：越界必须 5，且**不许**调用回收站。
func TestTrashRejectsPathsOutsideOwnDataDirs(t *testing.T) {
	e := testEnv(t)
	called := recordingTrasher(e)

	outside := []string{
		"/etc/passwd",
		filepath.Join(e.Resolver.XDG["DATA_HOME"], "some-app", "config.toml"),
		filepath.Join(e.Base, "profiles.toml"), // profiles.d 的兄弟，不是本工具的数据
		filepath.Join(e.LauncherDir(), "firefox.desktop"),
		filepath.Join(e.LauncherDir(), "appclone-.desktop"), // 不是本工具生成的命名
		"relative/path",
	}
	for _, p := range outside {
		if err := e.Trash(p); ExitCode(err) != CodeConflict {
			t.Fatalf("删除 %q 应被拒（5），得到 %v", p, err)
		}
	}
	if len(*called) != 0 {
		t.Fatalf("越界路径不得交给回收站，却收到了 %v", *called)
	}
}

func TestTrashAcceptsInstanceProfileAndLauncher(t *testing.T) {
	e := testEnv(t)
	called := recordingTrasher(e)
	app, p := seedApp(t, e)
	if _, err := e.CreateInstance(context.Background(), app, p, "work"); err != nil {
		t.Fatal(err)
	}
	ok := []string{
		e.InstancePath(app, "work"),
		e.ProfilePath(app),
		e.LauncherPath(app, "work"),
	}
	for _, p := range ok {
		if err := e.Trash(p); err != nil {
			t.Fatalf("%q 应可删除，得到 %v", p, err)
		}
	}
	if len(*called) != len(ok) {
		t.Fatalf("应逐个交给回收站（%d 个），实际 %v", len(ok), *called)
	}
}

// 已经不在的路径不是错误：并发两次删除不该给用户一条假故障。
func TestTrashSkipsMissingPath(t *testing.T) {
	e := testEnv(t)
	called := recordingTrasher(e)
	gone := filepath.Join(e.InstancePath("demo", "main"), "does-not-exist")
	if err := e.Trash(gone); err != nil {
		t.Fatalf("不存在的路径应静默跳过，得到 %v", err)
	}
	if len(*called) != 0 {
		t.Fatalf("不存在的东西不该交给回收站：%v", *called)
	}
}

// 回滚要连 MkdirAll 顺手建出的多级父目录一起收：留下来的空目录用户从没见过，
// appclone 也不再认得它（§4 写入白名单）。
func TestMkdirAllTrackedRollsBackWholeChain(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")
	made, err := mkdirAllTracked(target, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), target}
	if len(made) != len(want) {
		t.Fatalf("应记录 %d 级目录，得到 %v", len(want), made)
	}
	for i := range want {
		if made[i] != want[i] {
			t.Fatalf("记录顺序应为由外到内：want %v got %v", want, made)
		}
	}
	removeCreated(made)
	for _, p := range want {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%q 应已被回收", p)
		}
	}
}

// 已存在的那几级不算"本次新建"，不能记进回滚清单——否则失败回滚会把
// 用户原本就有的目录删掉。
func TestMkdirAllTrackedIgnoresExistingAncestors(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "a")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	made, err := mkdirAllTracked(filepath.Join(existing, "b"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 1 || made[0] != filepath.Join(existing, "b") {
		t.Fatalf("只应记录新建的那一级，得到 %v", made)
	}
	removeCreated(made)
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("原有目录不得被回收：%v", err)
	}
}
