package core

import (
	"os"
	"testing"
)

func writeTraceFIFO(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func newExecTestReader(t *testing.T) *traceReader {
	t.Helper()
	r, err := newTraceReader(t.TempDir(), "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTraceReaderExecCallsRound(t *testing.T) {
	r := newExecTestReader(t)
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", ["qoder"], 0x0) = 0`+"\n")
	r.markRound(1)
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", ["qoder", "--x"], 0x0) = 0`+"\n")
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExecCalls) != 2 {
		t.Fatalf("应保留两轮证据，得到 %#v", res.ExecCalls)
	}
	if res.ExecCalls[0].Round != 0 || res.ExecCalls[1].Round != 1 {
		t.Fatalf("轮次标记错：%#v", res.ExecCalls)
	}
	if res.Stats.RoundMarkerDropped != 0 {
		t.Fatalf("标记行不应失败：%d", res.Stats.RoundMarkerDropped)
	}
}

func TestTraceReaderRoundMarkerSplitRead(t *testing.T) {
	r := newExecTestReader(t)
	writeTraceFIFO(t, r.path(), "\n#appclone")
	writeTraceFIFO(t, r.path(), "-round 3\n")
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", ["qoder"], 0x0) = 0`+"\n")
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExecCalls) != 1 || res.ExecCalls[0].Round != 3 {
		t.Fatalf("拆成两次 Read 的标记行仍应识别：%#v", res.ExecCalls)
	}
}

func TestTraceReaderExecCallsAtPathLimit(t *testing.T) {
	r := newExecTestReader(t)
	r.mu.Lock()
	r.entries = traceReadLimit
	r.mu.Unlock()
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", ["qoder"], 0x0) = 0`+"\n")
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", NULL, 0x0) = 0`+"\n")
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExecCalls) != 1 || res.ExecCalls[0].Path != "/opt/app/qoder" {
		t.Fatalf("上限分支仍应保留 exec 证据：%#v", res.ExecCalls)
	}
	if res.Stats.ExecArgsSkipped != 1 {
		t.Fatalf("上限分支应回写 ExecArgsSkipped，得到 %d", res.Stats.ExecArgsSkipped)
	}
}

func TestTraceReaderRoundMarkerAfterLimit(t *testing.T) {
	r := newExecTestReader(t)
	r.mu.Lock()
	r.entries = traceReadLimit
	r.mu.Unlock()
	r.markRound(4)
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", ["qoder"], 0x0) = 0`+"\n")
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExecCalls) != 1 || res.ExecCalls[0].Round != 4 {
		t.Fatalf("超过上限后标记行仍要生效：%#v", res.ExecCalls)
	}
}

func TestTraceReaderSnapshotExecCallsIsDeepCopy(t *testing.T) {
	r := newExecTestReader(t)
	writeTraceFIFO(t, r.path(), `1 execve("/opt/app/qoder", ["qoder", "--x"], 0x0) = 0`+"\n")
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExecCalls) != 1 {
		t.Fatalf("snapshot 应携带 ExecCalls：%#v", res.ExecCalls)
	}
	res.ExecCalls[0].Args[1] = "mutated"
	r.mu.Lock()
	got := r.res.ExecCalls[0].Args[1]
	r.mu.Unlock()
	if got != "--x" {
		t.Fatalf("snapshot 必须深拷贝 Args，内部状态被改成 %q", got)
	}
}
