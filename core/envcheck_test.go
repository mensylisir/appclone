package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// CheckBwrap 是 GUI 致命横幅的唯一判据：找不到 bwrap 必须报 err.env.bwrap_missing，
// 而不是一个没有文案键的裸 exec 错误（界面会退化成"未知错误"）。
func TestCheckBwrapReportsMissingWithReason(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := CheckBwrap()
	if err == nil {
		t.Fatal("PATH 里没有 bwrap 时必须报错")
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.env.bwrap_missing" {
		t.Fatalf("应报 err.env.bwrap_missing，得到 %v", err)
	}
}

func TestCheckBwrapFindsBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if err := CheckBwrap(); err != nil {
		t.Fatalf("PATH 里有 bwrap 时不应报错：%v", err)
	}
}

// OverlayAvailable 必须走 Env 注入的 mounter（测试用它模拟 /dev/fuse 缺失），
// 不能自己去查真实系统——否则 GUI 的自检在单测里永远与探测定理不一致。
func TestOverlayAvailableUsesInjectedMounter(t *testing.T) {
	e := &Env{Mounter: &fakeMounter{}}
	if err := e.OverlayAvailable(); err != nil {
		t.Fatalf("可用时应返回 nil：%v", err)
	}
	e.Mounter = &fakeMounter{failOn: "available"}
	if err := e.OverlayAvailable(); err == nil {
		t.Fatal("fuse 不可用时必须报错（GUI 据此挂降级横幅）")
	}
}
