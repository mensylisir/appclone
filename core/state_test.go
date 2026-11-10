package core

import "testing"

// ADR-033：写层根不能在 /var/tmp —— Ubuntu 26.04 的 fusermount3 AppArmor
// profile 只允许在 $HOME、/tmp、/run/user/<uid>、/mnt、/media 下挂 FUSE。
func TestTmpRootStaysInsideMountAllowlist(t *testing.T) {
	e := &Env{}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	if got := e.tmpRoot(); got != "/tmp" {
		t.Fatalf("/tmp 可写时应优先用它（AppArmor 白名单内），得到 %q", got)
	}
	e.TmpRoot = "/injected"
	if got := e.tmpRoot(); got != "/injected" {
		t.Fatalf("测试注入必须优先，得到 %q", got)
	}
	if got := e.quarantineRoot(); got != "/injected" {
		t.Fatalf("注入 TmpRoot 时 quarantine 跟随它，得到 %q", got)
	}
	e.TmpRoot = ""
	want := "/var/tmp"
	if !dirWritable("/var/tmp") {
		want = "/tmp" // /var/tmp 不可写时按设计退回写层根
	}
	if got := e.quarantineRoot(); got != want {
		t.Fatalf("生产 quarantine 应放 %s，得到 %q", want, got)
	}
}
