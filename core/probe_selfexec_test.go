package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
)

// execFifoRunner 不启动真实 bwrap/strace：它从 argv 里找到 `-o <fifo>`，按调用
// 次数写入两轮 qoder 型 execve 证据。第 3 次（overlay 的 rerun）没有 -o，
// 只返回结果，不写 FIFO。
type execFifoRunner struct {
	q       string
	calls   int
	code    int
	unknown bool // 用 `= ?` 输出（调用发生但返回值未知）
}

func (f *execFifoRunner) Run(_ context.Context, argv []string, _ io.Writer) (SessionResult, error) {
	f.calls++
	fifo := ""
	for i, a := range argv {
		if a == "-o" && i+1 < len(argv) {
			fifo = argv[i+1]
			break
		}
	}
	if fifo != "" {
		ret := "= 0"
		if f.unknown {
			ret = "= ?"
		}
		lines := fmt.Sprintf(
			"1 execve(%q, [%q], 0x0) %s\n1 execve(%q, [%q, %q], 0x0) %s\n",
			f.q, f.q, ret, f.q, f.q, "--ozone-platform=x11", ret)
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
	}
	return SessionResult{ExitCode: f.code, Started: true}, nil
}

func TestProbeSessionSelfRelaunchArgs(t *testing.T) {
	e := probeEnv(t)
	q := "/opt/Qoder CN/qoder-cn"
	m := &fakeMounter{entries: []string{".config/QoderCN/state"}}
	r := &execFifoRunner{q: q}
	res, err := e.probeSession(context.Background(), "qoder", ProbeOptions{
		Command: []string{q}, Mounter: m, Runner: r, Rounds: 2,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Profile.ExecArgs, []string{"--ozone-platform=x11"}) {
		t.Fatalf("应自动预填 ozone 参数，得到 %q", res.Profile.ExecArgs)
	}
	if !res.Profile.execArgsFromEvidence {
		t.Fatal("自动预填必须标记证据来源")
	}
	if !slices.Equal(res.SelfRelaunchArgs, []string{"--ozone-platform=x11"}) {
		t.Fatalf("ProbeResult 应带出自重启增量，得到 %q", res.SelfRelaunchArgs)
	}
	if res.SelfRelaunchUnknown {
		t.Fatal("known 证据不应标 unknown")
	}
}

func TestProbeSessionSelfRelaunchAfterRerun(t *testing.T) {
	e := probeEnv(t)
	q := "/opt/Qoder CN/qoder-cn"
	// 没有 upper 写入 + 首轮非零退出 → 触发 rerun；rerun 不套 strace，
	// 检测必须继续使用首轮 execCalls。
	m := &fakeMounter{}
	r := &execFifoRunner{q: q, code: 1}
	res, err := e.probeSession(context.Background(), "qoder", ProbeOptions{
		Command: []string{q}, Mounter: m, Runner: r, Rounds: 2,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 3 {
		t.Fatalf("应发生一次 rerun（共 3 次 Run），得到 %d", r.calls)
	}
	if !slices.Equal(res.Profile.ExecArgs, []string{"--ozone-platform=x11"}) {
		t.Fatalf("rerun 后仍应保留首轮证据，得到 %q", res.Profile.ExecArgs)
	}
}

func TestProbeStraceSelfRelaunchArgs(t *testing.T) {
	e := probeEnv(t)
	// 兜底路径：读能力不可用，只有写能力；exec 证据仍要采集。
	e.ProbeCapsOverride = &ProbeCaps{Read: false, Write: true}
	q := "/opt/Qoder CN/qoder-cn"
	r := &execFifoRunner{q: q}
	res, err := e.probeStrace(context.Background(), "qoder", ProbeOptions{
		Command: []string{q}, Rounds: 2,
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Profile.ExecArgs, []string{"--ozone-platform=x11"}) {
		t.Fatalf("兜底路径应自动预填，得到 %q", res.Profile.ExecArgs)
	}
}

func TestProbeSessionSelfRelaunchUnknownDoesNotPrefill(t *testing.T) {
	e := probeEnv(t)
	q := "/opt/Qoder CN/qoder-cn"
	m := &fakeMounter{entries: []string{".config/QoderCN/state"}}
	r := &execFifoRunner{q: q, unknown: true}
	res, err := e.probeSession(context.Background(), "qoder", ProbeOptions{
		Command: []string{q}, Mounter: m, Runner: r, Rounds: 2,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	// `= ?` 证据只证明调用发生过，不能证明成功；落盘会污染档案、驱动重探
	// 又会真的多启动应用，所以只做 run-scoped 诊断（ADR-036 审查 P2）。
	if len(res.Profile.ExecArgs) != 0 || res.Profile.execArgsFromEvidence {
		t.Fatalf("unknown 证据不能预填 exec_args：%q", res.Profile.ExecArgs)
	}
	if !res.SelfRelaunchUnknown {
		t.Fatal("应标记 SelfRelaunchUnknown")
	}
	if !slices.Equal(res.SelfRelaunchArgs, []string{"--ozone-platform=x11"}) {
		t.Fatalf("诊断字段仍应带出增量，得到 %q", res.SelfRelaunchArgs)
	}
}

func TestProbeStraceSelfRelaunchUnknownDoesNotPrefill(t *testing.T) {
	e := probeEnv(t)
	e.ProbeCapsOverride = &ProbeCaps{Read: false, Write: true}
	q := "/opt/Qoder CN/qoder-cn"
	r := &execFifoRunner{q: q, unknown: true}
	res, err := e.probeStrace(context.Background(), "qoder", ProbeOptions{
		Command: []string{q}, Rounds: 2,
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Profile.ExecArgs) != 0 || res.Profile.execArgsFromEvidence {
		t.Fatalf("unknown 证据不能预填 exec_args：%q", res.Profile.ExecArgs)
	}
	if !res.SelfRelaunchUnknown {
		t.Fatal("应标记 SelfRelaunchUnknown")
	}
}

func TestProbeSessionNoReaderDoesNotPrefill(t *testing.T) {
	e := probeEnv(t)
	q := "/opt/Qoder CN/qoder-cn"
	m := &fakeMounter{entries: []string{".config/QoderCN/state"}}
	r := &execFifoRunner{q: q}
	res, err := e.probeSession(context.Background(), "qoder", ProbeOptions{
		Command: []string{q, "--desktop"}, Mounter: m, Runner: r,
		Rounds: 2, DisableReadTrace: true,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Profile.ExecArgs, []string{"--desktop"}) {
		t.Fatalf("没有读追踪时不能预填，得到 %q", res.Profile.ExecArgs)
	}
	if res.Profile.execArgsFromEvidence {
		t.Fatal("没有证据时不能标证据来源")
	}
}
