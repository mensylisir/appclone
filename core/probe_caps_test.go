package core

import (
	"slices"
	"testing"
)

func TestClassifyProbeFailurePriority(t *testing.T) {
	cases := []struct {
		stderr string
		want   string
	}{
		{"bwrap: Creating new namespace failed: Operation not permitted", "bwrap"},
		{"strace: ptrace(PTRACE_TRACEME): Operation not permitted", "ptrace"},
		{"strace: invalid system call 'openat2'", "expression"},
		{"something else", "unknown"},
	}
	for _, c := range cases {
		if got := classifyProbeFailure(c.stderr, nil); got != c.want {
			t.Fatalf("stderr %q → %s，want %s", c.stderr, got, c.want)
		}
	}
}

// 预检列表必须与会话列表逐项一致（顺序也一致）：两套列表各写一份时，
// 改了实际命令却忘了缓存键列表会命中旧能力结论（ADR-034 §4.2）。
func TestProbeCapsListsMatchSessionLists(t *testing.T) {
	st := &probeReadState{caps: ProbeCaps{Openat2: true, Y: true}}
	if got, want := st.readSyscalls(), probeCapsReadList(); !slices.Equal(got, want) {
		t.Fatalf("读列表不一致：session=%v caps=%v", got, want)
	}
	if got, want := st.writeSyscalls(), probeCapsWriteList(); !slices.Equal(got, want) {
		t.Fatalf("写列表不一致：session=%v caps=%v", got, want)
	}
}

// bwrap 被删除时 strace 输出 `Can't stat 'bwrap'`（没有 "bwrap:" 前缀），
// 必须仍归为 bwrap 硬错误。
func TestClassifyProbeFailureMissingBwrap(t *testing.T) {
	if got := classifyProbeFailure("strace: Can't stat 'bwrap': No such file or directory", nil); got != "bwrap" {
		t.Fatalf("缺 bwrap 应归 bwrap，得到 %q", got)
	}
}

// 能力为 false 的结果不能缓存：用户修好 ptrace/沙盒后应立即生效。
func TestCacheableCaps(t *testing.T) {
	cases := []struct {
		caps ProbeCaps
		want bool
	}{
		{ProbeCaps{Read: true, Write: true}, true},
		{ProbeCaps{Read: false, Write: true}, false},
		{ProbeCaps{Read: true, Write: false}, false},
	}
	for _, c := range cases {
		if got := cacheableCaps(c.caps); got != c.want {
			t.Fatalf("%+v: got %v want %v", c.caps, got, c.want)
		}
	}
}
