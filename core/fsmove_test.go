package core

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// 跨设备回退（EXDEV）没法在单测里真的制造出来，但回退路径真正危险的是
// copyTree 本身：它搬的是**可能属于用户的文件**，丢内容、跟着软链跑到外面、
// 或者把 whiteout 当数据，都是数据事故。这里直接测这些边界。
//
// 注意：本文件依赖包级变量 moveRename 做故障注入，**不要给这些用例加
// t.Parallel()**——并发用例会互相覆盖钩子，测试变成随机绿/红。
func TestCopyTreePreservesContentAndSkipsWhiteout(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "upper")
	dst := filepath.Join(root, "quarantine")

	if err := os.MkdirAll(filepath.Join(src, "Downloads"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("user file that must survive")
	if err := os.WriteFile(filepath.Join(src, "Downloads", "important.docx"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	// 软链必须原样保留（指向哪里都行），不能被解引用成内容拷贝——
	// 解引用会把链接目标的内容复制成普通文件，等于凭空造出一份用户数据。
	if err := os.Symlink("/home/user/target", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	// overlay whiteout 是字符设备 0,0：非特权下重建不了，也不承载用户内容。
	// 单测里用无法创建的设备节点替代——只要它不是普通文件/目录/软链就会被跳过。
	// 用 FIFO 制造"非普通文件"（真实环境的 whiteout 同理走 default 分支）。
	if err := unix.Mkfifo(filepath.Join(src, "fifo"), 0o600); err != nil {
		t.Skipf("当前环境不支持 mkfifo：%v", err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "Downloads", "important.docx"))
	if err != nil {
		t.Fatalf("用户文件必须被搬过去：%v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("内容被改动：%q", got)
	}
	link, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil {
		t.Fatalf("软链必须保留：%v", err)
	}
	if link != "/home/user/target" {
		t.Fatalf("软链目标被改动：%s", link)
	}
	if _, err := os.Lstat(filepath.Join(dst, "fifo")); !os.IsNotExist(err) {
		t.Fatal("FIFO/whiteout 这类非普通文件必须跳过，不能卡在打开上")
	}
	// 源目录在 copyTree 里不许被删：删源由 moveDir 在拷贝完整后负责。
	if _, err := os.Stat(filepath.Join(src, "Downloads", "important.docx")); err != nil {
		t.Fatalf("copyTree 不得删源：%v", err)
	}
}

func TestMoveDirRenameFallsBackToCopy(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveDir(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "x")); err != nil {
		t.Fatalf("目标里必须有内容：%v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("搬完后源必须消失：%v", err)
	}
}

// 跨设备回退（EXDEV）在生产上很常见（/home 独立分区），但单测无法真的造出
// 两个文件系统。替换 rename 钩子走真实回退路径：内容、软链、删源都要对。
func TestMoveDirCrossDeviceFallback(t *testing.T) {
	old := moveRename
	moveRename = func(_, _ string) error { return syscall.EXDEV }
	defer func() { moveRename = old }()

	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "f"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveDir(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a", "f"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("回退拷贝必须保留内容：%q %v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("拷贝完整后源应被删除")
	}
}

// 拷贝失败时**源必须原样还在**：搬的是可能属于用户的文件，宁可留下现场。
func TestMoveDirCopyFailureKeepsSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受权限位限制，无法制造读失败")
	}
	old := moveRename
	moveRename = func(_, _ string) error { return syscall.EXDEV }
	defer func() { moveRename = old }()

	root := t.TempDir()
	src := filepath.Join(root, "src")
	blocked := filepath.Join(src, "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	merr := moveDir(src, filepath.Join(root, "dst"))
	// 先把权限恢复再断言：000 目录连测试自己都进不去。
	if err := os.Chmod(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if merr == nil {
		t.Fatal("读不了源目录时必须失败")
	}
	if _, err := os.Stat(filepath.Join(src, "blocked", "f")); err != nil {
		t.Fatalf("失败后源必须完整保留：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dst")); !os.IsNotExist(err) {
		t.Fatalf("失败时应回收自己写出的半成品 dst：%v", err)
	}
}

// dst 已存在时直接失败：绝不先删一个我们没建过的目录。
//
// dst 故意用**空目录**：rename(src, 空 dst) 是会成功的（替换掉 dst），所以
// 去掉预检这条测试就会红；用非空 dst 的话 ENOTEMPTY 会让测试假绿。
func TestMoveDirRefusesExistingDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := moveDir(src, dst); err == nil {
		t.Fatal("dst 已存在（即使是空目录）时必须失败")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("预先存在的 dst 不能被替换/删除：%v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("源不能被删：%v", err)
	}
}

// 拷贝完成、删源失败（子目录 0555 且含文件）不是搬迁失败：完整副本必须
// 被交出去，而不是拿残缺的源再搬一次。非 root 才有权限语义；root 跳过。
func TestQuarantineDirKeepsCompleteCopyWhenSourceCleanupFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，复现不了删源失败")
	}
	e := probeEnv(t)
	src := filepath.Join(e.TmpRoot, fmt.Sprintf("appclone-probe-%d-locked", e.UID))
	locked := filepath.Join(src, "upper", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "user-file"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(e.TmpRoot, func(p string, _ os.DirEntry, _ error) error {
			return os.Chmod(p, 0o700)
		})
	})
	// 强制走跨设备分支，生产里 /tmp(tmpfs)→/var/tmp 就是这条；再让补权限
	// 那一步失败，模拟"连 chmod 都不允许"的源（root 拥有的子树等）。
	old := moveRename
	moveRename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	oldChmod := removeChmod
	removeChmod = func(string, os.FileMode) error { return syscall.EPERM }
	t.Cleanup(func() { moveRename = old; removeChmod = oldChmod })

	q, err := e.quarantineDir(src)
	if err != nil {
		t.Fatalf("拷贝已完成时不应报失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(q, "upper", "locked", "user-file")); err != nil {
		t.Fatalf("返回的路径应是完整副本：%v", err)
	}
}
