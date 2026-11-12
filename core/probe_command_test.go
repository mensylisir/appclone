package core

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestProbeCommand(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "qoder")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "qoder-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	old := Profile{Exec: link, ExecArgs: []string{"--ozone-platform=x11"}}
	got := ProbeCommand(real, []string{"--desktop"}, old, true)
	if !slices.Equal(got, []string{real, "--ozone-platform=x11"}) {
		t.Fatalf("同 exec 应复用旧参数（含软链）：%q", got)
	}

	got = ProbeCommand(real, []string{"--desktop"}, Profile{Exec: "/opt/other", ExecArgs: []string{"--old"}}, true)
	if !slices.Equal(got, []string{real, "--desktop"}) {
		t.Fatalf("exec 变化应使用桌面参数：%q", got)
	}

	got = ProbeCommand(real, []string{"--desktop"}, old, false)
	if !slices.Equal(got, []string{real, "--desktop"}) {
		t.Fatalf("没有旧档案时应使用桌面参数：%q", got)
	}

	got = ProbeCommand(real, nil, Profile{Exec: real}, true)
	if !slices.Equal(got, []string{real}) {
		t.Fatalf("空参数应只返回 exec：%q", got)
	}

	// 返回切片独立：修改它不能污染旧档案。
	got = ProbeCommand(real, nil, old, true)
	got[1] = "--mutated"
	if old.ExecArgs[0] != "--ozone-platform=x11" {
		t.Fatalf("返回切片不能与旧档案共享底层数组：%q", old.ExecArgs)
	}
}
