package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// retryMounter 模拟"应用只有在带上自重启参数后才写出可隔离路径"：
// 第一次 Mount 没有任何写入，第二次 Mount 才出现配置目录。
type retryMounter struct {
	*fakeMounter
	calls         int
	secondEntries []string
}

func (m *retryMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	m.calls++
	if m.calls == 1 {
		m.fakeMounter.entries = nil
	} else {
		m.fakeMounter.entries = m.secondEntries
	}
	return m.fakeMounter.Mount(lower, upper, work, merged)
}

func TestProbeRetryCommand(t *testing.T) {
	cmd := []string{"/opt/Qoder CN/qoder-cn"}
	cases := []struct {
		name string
		cmd  []string
		res  ProbeResult
		want []string
	}{
		{
			name: "已知增量触发重试",
			cmd:  cmd,
			res:  ProbeResult{SelfRelaunchArgs: []string{"--ozone-platform=x11"}},
			want: []string{"/opt/Qoder CN/qoder-cn", "--ozone-platform=x11"},
		},
		{
			name: "unknown 证据不驱动真实启动",
			cmd:  cmd,
			res: ProbeResult{
				SelfRelaunchArgs:    []string{"--ozone-platform=x11"},
				SelfRelaunchUnknown: true,
			},
			want: nil,
		},
		{
			name: "没有增量",
			cmd:  cmd,
			res:  ProbeResult{},
			want: nil,
		},
		{
			name: "原命令为空",
			cmd:  nil,
			res:  ProbeResult{SelfRelaunchArgs: []string{"--ozone-platform=x11"}},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProbeRetryCommand(c.cmd, c.res)
			if !slices.Equal(got, c.want) {
				t.Fatalf("ProbeRetryCommand=%q，期望 %q", got, c.want)
			}
			if len(got) > 0 && len(c.cmd) > 0 {
				// 返回必须是新切片：改它不能污染调用方的命令。
				got[len(got)-1] = "--mutated"
				if c.cmd[len(c.cmd)-1] == "--mutated" {
					t.Fatal("返回值与入参共享底层数组")
				}
			}
		})
	}
}

// TestProbeWithRetrySelfRelaunch 是真实产品路径的回归：第一次探测零写入
// （应用自重启后被杀），FinalizeProbe 返回 no_writes；ProbeWithRetry 用识别
// 到的增量再探一次，第二次应用正常写出配置，档案落盘并带上 exec_args。
func TestProbeWithRetrySelfRelaunch(t *testing.T) {
	e := probeEnv(t)
	q := "/opt/Qoder CN/qoder-cn"
	ozone := "--ozone-platform=x11"
	m := &retryMounter{
		fakeMounter:   &fakeMounter{},
		secondEntries: []string{".config/QoderCN/state"},
	}
	r := &execFifoRunner{q: q}
	var retried [][]string
	res, p, cands, prefilled, err := e.ProbeWithRetry(context.Background(), "qoder", ProbeOptions{
		Command: []string{q},
		Mounter: m,
		Runner:  r,
		Rounds:  2,
		OnRetry: func(args []string) { retried = append(retried, args) },
	}, ProbeFinalize{})
	if err != nil {
		t.Fatalf("重探后应成功，得到 %v", err)
	}
	if m.calls != 2 {
		t.Fatalf("应挂载两次（原探测 + 重探），得到 %d", m.calls)
	}
	if r.calls != 4 {
		t.Fatalf("两轮 × 两次探测应启动 4 次，得到 %d", r.calls)
	}
	if len(retried) != 1 || !slices.Equal(retried[0], []string{ozone}) {
		t.Fatalf("OnRetry 应只回调一次并带增量，得到 %q", retried)
	}
	if !slices.Equal(p.ExecArgs, []string{ozone}) {
		t.Fatalf("重探后的档案应带识别到的 exec_args，得到 %q", p.ExecArgs)
	}
	if len(p.Paths) != 1 || !strings.HasPrefix(p.Paths[0], "~/.config/QoderCN") {
		t.Fatalf("重探后的档案应隔离应用配置，得到 %q", p.Paths)
	}
	if res.SelfRelaunchArgs != nil {
		t.Fatalf("第二次探测不应再报告自重启增量，得到 %q", res.SelfRelaunchArgs)
	}
	if len(cands) != 1 || prefilled != 0 {
		t.Fatalf("候选/预填计数异常：%d/%d", len(cands), prefilled)
	}
}

// TestProbeWithRetryOverridesOldExecArgs 覆盖"旧档案已有 exec_args"的合并
// 顺序：重探识别出的增量必须覆盖旧值，而不是被 merge 层用旧档案写回去
// （ADR-036 审查 P2）。
func TestProbeWithRetryOverridesOldExecArgs(t *testing.T) {
	e := probeEnv(t)
	q := "/opt/Qoder CN/qoder-cn"
	ozone := "--ozone-platform=x11"
	m := &retryMounter{
		fakeMounter:   &fakeMounter{},
		secondEntries: []string{".config/QoderCN/state"},
	}
	r := &execFifoRunner{q: q}
	old := Profile{App: "qoder", Exec: q, ExecArgs: []string{"--old-flag"}}
	_, p, _, _, err := e.ProbeWithRetry(context.Background(), "qoder", ProbeOptions{
		Command: []string{q},
		Mounter: m,
		Runner:  r,
		Rounds:  2,
	}, ProbeFinalize{Existing: old, HasExisting: true})
	if err != nil {
		t.Fatalf("重探后应成功，得到 %v", err)
	}
	if !slices.Equal(p.ExecArgs, []string{ozone}) {
		t.Fatalf("重探识别出的 exec_args 应覆盖旧档案，得到 %q", p.ExecArgs)
	}
	for _, w := range p.Warnings {
		if w == "exec_args_preserved" {
			t.Fatal("有本次证据时不应告警 exec_args_preserved")
		}
	}
}

// TestProbeWithRetryNoEvidence 确保没有自重启证据时不会多启动一轮：
// 零写入错误原样返回，挂载只发生一次。
func TestProbeWithRetryNoEvidence(t *testing.T) {
	e := probeEnv(t)
	m := &retryMounter{fakeMounter: &fakeMounter{}}
	r := &countingRunner{}
	_, _, _, _, err := e.ProbeWithRetry(context.Background(), "plain", ProbeOptions{
		Command: []string{"/usr/bin/plainapp"},
		Mounter: m,
		Runner:  r,
		Rounds:  1,
	}, ProbeFinalize{})
	if err == nil {
		t.Fatal("零写入必须报错")
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.probe.no_writes" {
		t.Fatalf("应返回 no_writes，得到 %v", err)
	}
	if m.calls != 1 || r.calls != 1 {
		t.Fatalf("没有证据时不能重探，得到挂载 %d 次、启动 %d 次", m.calls, r.calls)
	}
}

// allowEmptyRunner 模拟 GUI 首次配置的降级路径：读证据不可用（strace 兜底），
// 第一次探测只看到自重启、没有任何写入；带上增量重探后才写出配置目录。
type allowEmptyRunner struct {
	q, home string
	calls   int
}

func (f *allowEmptyRunner) Run(_ context.Context, argv []string, _ io.Writer) (SessionResult, error) {
	f.calls++
	fifo := ""
	for i, a := range argv {
		if a == "-o" && i+1 < len(argv) {
			fifo = argv[i+1]
			break
		}
	}
	if fifo == "" {
		return SessionResult{ExitCode: 0, Started: true}, nil
	}
	lines := fmt.Sprintf(
		"1 execve(%q, [%q], 0x0) = 0\n1 execve(%q, [%q, %q], 0x0) = 0\n",
		f.q, f.q, f.q, f.q, "--ozone-platform=x11")
	if f.calls > 2 {
		lines += fmt.Sprintf("1 openat(AT_FDCWD, %q, O_WRONLY|O_CREAT, 0600) = 3\n",
			filepath.Join(f.home, ".config/QoderCN/state"))
	}
	fh, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		return SessionResult{}, err
	}
	if _, err := fh.WriteString(lines); err != nil {
		_ = fh.Close()
		return SessionResult{}, err
	}
	if err := fh.Close(); err != nil {
		return SessionResult{}, err
	}
	return SessionResult{ExitCode: 0, Started: true}, nil
}

// TestProbeWithRetryAllowEmpty 锁定 GUI 降级确认页的重探：读证据不可用时
// FinalizeProbe 返回 nil + 空 paths（AllowEmpty），有自重启证据就不该停在
// 空确认页，而应带上增量再探一次拿到真实写入。
func TestProbeWithRetryAllowEmpty(t *testing.T) {
	e := probeEnv(t)
	// 读能力不可用 → strace 兜底；exec 证据仍采集。
	e.ProbeCapsOverride = &ProbeCaps{Read: false, Write: true, Y: true}
	q := "/opt/Qoder CN/qoder-cn"
	r := &allowEmptyRunner{q: q, home: e.Resolver.Home}
	var retried [][]string
	res, p, _, _, err := e.ProbeWithRetry(context.Background(), "qoder", ProbeOptions{
		Command: []string{q},
		// fuse-overlayfs 缺失 → Probe 走 strace 兜底（读证据不可用）。
		Mounter:     &fakeMounter{failOn: "available"},
		Runner:      r,
		Rounds:      2,
		StaticPaths: []string{filepath.Join(e.Resolver.Home, ".config/QoderCN")},
		OnRetry:     func(args []string) { retried = append(retried, args) },
	}, ProbeFinalize{AllowEmpty: true})
	if err != nil {
		t.Fatalf("降级页有自重启证据时应重探成功，得到 %v", err)
	}
	if r.calls != 4 {
		t.Fatalf("应重探一次（2 轮 × 2 次），得到 %d", r.calls)
	}
	if len(retried) != 1 {
		t.Fatalf("OnRetry 应回调一次，得到 %q", retried)
	}
	if len(p.Paths) == 0 {
		t.Fatal("重探后应拿到真实写入路径，不能停在空确认页")
	}
	if !slices.Equal(p.ExecArgs, []string{"--ozone-platform=x11"}) {
		t.Fatalf("重探后应带 exec_args，得到 %q", p.ExecArgs)
	}
	if res.ReadTrace != ReadTraceUnavailable {
		t.Fatalf("本场景读证据应不可用，得到 %q", res.ReadTrace)
	}
}

type countingRunner struct{ calls int }

func (c *countingRunner) Run(_ context.Context, _ []string, _ io.Writer) (SessionResult, error) {
	c.calls++
	return SessionResult{ExitCode: 0, Started: true}, nil
}
