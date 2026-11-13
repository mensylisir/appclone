package core

import (
	"context"
	"errors"
	"io"
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

type countingRunner struct{ calls int }

func (c *countingRunner) Run(_ context.Context, _ []string, _ io.Writer) (SessionResult, error) {
	c.calls++
	return SessionResult{ExitCode: 0, Started: true}, nil
}
