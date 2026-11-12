package core

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fifoRunner 模拟 strace：把预置的日志行写进命令里的 `-o <fifo>`，再返回
// 预设的会话结果。这样 probeSession 的 FIFO/reader/解析链路是真实跑通的，
// 只有外部进程被替换。
type fifoRunner struct {
	lines []string
	res   SessionResult
	err   error
	step  func(call int) (SessionResult, error)
	argv  []string
	calls int
}

func (f *fifoRunner) Run(_ context.Context, argv []string, _ io.Writer) (SessionResult, error) {
	f.argv = argv
	f.calls++
	if path := argvLogPath(argv); path != "" && len(f.lines) > 0 {
		if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			for _, line := range f.lines {
				_, _ = w.WriteString(line + "\n")
			}
			w.Close()
		}
	}
	if f.step != nil {
		return f.step(f.calls)
	}
	return f.res, f.err
}

func argvLogPath(argv []string) string {
	for i, a := range argv {
		if a == "-o" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// 读证据接入后：凭据读路径必须派生 credential_paths，三态为 ok。
func TestProbeSessionReadEvidence(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fifoRunner{
		lines: []string{
			`1234  execve("/usr/bin/testapp", ["testapp"], 0x0) = 0`,
			`1234  openat(AT_FDCWD, "` + filepath.Join(e.Resolver.Home, ".config/TestApp/credentials.json") + `", O_RDONLY) = 3`,
		},
		res: SessionResult{Started: true},
	}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReadTrace != ReadTraceOK {
		t.Fatalf("无完整性告警应为 ok，得到 %q（warns %v）", res.ReadTrace, res.ReadsWarns)
	}
	if res.Writes == 0 || res.HomeReads != 1 || !res.ExecSeen || !res.Activity {
		t.Fatalf("计数错：writes=%d reads=%d exec=%v activity=%v", res.Writes, res.HomeReads, res.ExecSeen, res.Activity)
	}
	if len(res.Profile.CredentialPaths) != 1 ||
		!strings.Contains(res.Profile.CredentialPaths[0], "credentials.json") {
		t.Fatalf("凭据读路径应进 credential_paths：%v", res.Profile.CredentialPaths)
	}
	if !containsString(res.Profile.Paths, "~/.config/TestApp") {
		t.Fatalf("凭据应把 owner 升级进 paths：%v", res.Profile.Paths)
	}
	if got := argvLogPath(r.argv); got == "" {
		t.Fatal("读追踪开启时命令必须带 -o <fifo>")
	}
}

// DisableReadTrace：不套 strace、不建 FIFO，ReadTrace=unavailable，
// 且不得同时报 no_home_reads（没有读证据 ≠ 没有读到）。
func TestProbeSessionDisableReadTrace(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fakeRunner{}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r,
		DisableReadTrace: true,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReadTrace != ReadTraceUnavailable {
		t.Fatalf("关闭读追踪必须 unavailable，得到 %q", res.ReadTrace)
	}
	if !containsString(res.ReadsWarns, "unavailable") {
		t.Fatalf("应记 unavailable：%v", res.ReadsWarns)
	}
	if containsString(res.ReadsWarns, "no_home_reads") {
		t.Fatalf("unavailable 时不能报 no_home_reads：%v", res.ReadsWarns)
	}
	if len(r.argv) == 0 || r.argv[0] != "bwrap" {
		t.Fatalf("关闭读追踪不应套 strace：%v", r.argv)
	}
}

// 读能力预检失败：写证据继续，ReadTrace=unavailable + preflight_failed。
func TestProbeSessionPreflightFailed(t *testing.T) {
	e := probeEnv(t)
	e.ProbeCapsOverride = &ProbeCaps{Read: false, Write: true, Openat2: false, Y: true}
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fakeRunner{}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReadTrace != ReadTraceUnavailable || !containsString(res.ReadsWarns, "preflight_failed") {
		t.Fatalf("预检失败应 unavailable+preflight_failed：%q %v", res.ReadTrace, res.ReadsWarns)
	}
	if res.Writes == 0 {
		t.Fatalf("写证据应照常：%d", res.Writes)
	}
}

// 兜底路径写能力不可用：必须报环境错误，不能静默生成零写入档案。
func TestProbeStraceWriteUnavailable(t *testing.T) {
	e := probeEnv(t)
	e.ProbeCapsOverride = &ProbeCaps{Read: true, Write: false, Y: true}
	r := &fakeRunner{}
	_, err := e.probeStrace(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Runner: r,
	}, r)
	if errKey(err) != "err.probe.strace_unavailable" {
		t.Fatalf("写能力不可用应报 strace_unavailable，得到 %v", err)
	}
}

// StaticPaths 只补不存在的 owner，不勾选、不占配额、不进 paths。
func TestProbeStaticPathsCandidates(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fakeRunner{}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r,
		StaticPaths: []string{"~/.config/StaticApp", "~/.cache/StaticApp"},
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	var static *Candidate
	for i := range res.Candidates {
		if strings.Contains(res.Candidates[i].Path, "StaticApp") &&
			res.Candidates[i].Class != ClassCache {
			static = &res.Candidates[i]
		}
	}
	if static == nil || static.Checked || static.Reads != 0 || static.Why != "probe.why.static_hint" {
		t.Fatalf("静态候选形态错：%+v", static)
	}
	if containsString(res.Profile.Paths, "~/.config/StaticApp") {
		t.Fatal("静态线索不能自动进 paths")
	}
	for _, c := range res.Candidates {
		if c.Class == ClassCache && strings.Contains(c.Path, "StaticApp") {
			t.Fatal("cache 静态线索不应生成行")
		}
	}
}

// 有写入但没有观察到 exec：tracee_unverified 是证据告警，不改变 ok。
func TestProbeSessionTraceeUnverified(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fakeRunner{}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(res.ReadsWarns, "tracee_unverified") {
		t.Fatalf("应记 tracee_unverified：%v", res.ReadsWarns)
	}
	if containsString(res.ReadsWarns, "no_home_reads") {
		t.Fatalf("tracee_unverified 时不能同时报 no_home_reads：%v", res.ReadsWarns)
	}
	if res.ReadTrace != ReadTraceOK {
		t.Fatalf("证据告警不能把 ok 降级：%q", res.ReadTrace)
	}
}

// setuid 子进程：观察到带 setuid 位的 exec 目标 → partial。
func TestProbeSessionSetuidChild(t *testing.T) {
	e := probeEnv(t)
	bin := filepath.Join(t.TempDir(), "suid-helper")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o4755); err != nil {
		t.Skipf("无法设置 setuid 位：%v", err)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&os.ModeSetuid == 0 {
		t.Skip("文件系统不支持 setuid 位")
	}
	m := &fakeMounter{entries: []string{".config/TestApp/config.json"}}
	r := &fifoRunner{
		lines: []string{`1234  execve("` + bin + `", ["helper"], 0x0) = 0`},
		res:   SessionResult{Started: true},
	}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(res.ReadsWarns, "setuid_child") || res.ReadTrace != ReadTracePartial {
		t.Fatalf("setuid 子进程应 partial+setuid_child：%q %v", res.ReadTrace, res.ReadsWarns)
	}
}

// 可疑失败且 upper 无写入：丢弃第一次 overlay，无 strace 重跑一次；
// 第一次读路径保留，但重跑没有读追踪 → ReadTrace=unavailable。
func TestProbeSessionRerunsSuspiciousOverlay(t *testing.T) {
	e := probeEnv(t)
	m := &rerunMounter{}
	r := &fifoRunner{
		lines: []string{
			`1234  openat(AT_FDCWD, "` + filepath.Join(e.Resolver.Home, ".config/TestApp/settings.json") + `", O_RDONLY) = 3`,
		},
		step: func(call int) (SessionResult, error) {
			if call == 1 {
				return SessionResult{Started: true, ExitCode: 1}, nil
			}
			return SessionResult{Started: true}, nil
		},
	}
	res, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r, Rounds: 1,
	}, m, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 || m.mounts != 2 {
		t.Fatalf("应重跑一次：runner=%d mounts=%d", r.calls, m.mounts)
	}
	if res.ReadTrace != ReadTraceUnavailable {
		t.Fatalf("重跑没有读追踪应为 unavailable，得到 %q", res.ReadTrace)
	}
	if res.Writes == 0 {
		t.Fatal("重跑应拿到写证据")
	}
	if res.HomeReads != 1 {
		t.Fatalf("第一次的读路径必须保留：%d", res.HomeReads)
	}
}

// rerunMounter 第一次挂载为空，第二次挂载写入一条应用配置。
type rerunMounter struct {
	mounts int
}

func (m *rerunMounter) Available() error { return nil }

func (m *rerunMounter) Mount(_, upper, _, _ string) (func() error, error) {
	m.mounts++
	if m.mounts == 2 {
		p := filepath.Join(upper, ".config", "TestApp", "config.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			return nil, err
		}
	}
	return func() error { return nil }, nil
}

// 进程级 reader 泄漏标记：后续 Probe 直接拒绝。
func TestProbeRefusesAfterReaderLeak(t *testing.T) {
	e := probeEnv(t)
	e.markProbeReaderLeaked()
	defer e.resetProbeReaderLeak()
	_, err := e.Probe(context.Background(), "testapp", ProbeOptions{Command: []string{"/usr/bin/testapp"}})
	if errKey(err) != "err.probe.reader_stuck" {
		t.Fatalf("reader 泄漏后 Probe 应拒绝：%v", err)
	}
}

// 读追踪不可用时不报 tracee_unverified（ExecSeen 不可知）。
func TestProbeReadStateNoEvidenceWarnsWhenUnavailable(t *testing.T) {
	st := &probeReadState{e: probeEnv(t), warns: map[string]bool{}}
	st.addWarn("preflight_failed")
	st.finalize(3, true)
	if containsString(st.warnsList(), "tracee_unverified") || containsString(st.warnsList(), "no_home_reads") {
		t.Fatalf("读不可用时不能报证据告警：%v", st.warnsList())
	}
	if st.readTrace() != ReadTraceUnavailable {
		t.Fatalf("preflight_failed 应 unavailable：%q", st.readTrace())
	}
}

// 三态推导：strace_failed 有读数据 → partial，无读数据 → unavailable；
// 其余完整性告警 → partial。
func TestProbeReadStateTriState(t *testing.T) {
	cases := []struct {
		warns []string
		reads []string
		want  string
	}{
		{nil, nil, ReadTraceOK},
		{[]string{"parse_error"}, []string{"a"}, ReadTracePartial},
		{[]string{"strace_failed"}, []string{"a"}, ReadTracePartial},
		{[]string{"strace_failed"}, nil, ReadTraceUnavailable},
		{[]string{"no_home_reads"}, nil, ReadTraceOK},
	}
	for _, tc := range cases {
		st := &probeReadState{e: probeEnv(t), readOK: true, warns: map[string]bool{}, reads: tc.reads}
		for _, w := range tc.warns {
			st.warns[w] = true
		}
		if got := st.readTrace(); got != tc.want {
			t.Fatalf("%v reads=%v: got %q want %q", tc.warns, tc.reads, got, tc.want)
		}
	}
}

// tracee 未验证时不能再报 no_home_reads：两条语义相反的告警同时出现会让
// 确认页自相矛盾（plan §1 决策表）。
func TestProbeReadStateUnverifiedSuppressesNoHomeReads(t *testing.T) {
	st := &probeReadState{e: probeEnv(t), readOK: true, warns: map[string]bool{}}
	st.finalize(3, true)
	if !containsString(st.warnsList(), "tracee_unverified") {
		t.Fatalf("应记 tracee_unverified：%v", st.warnsList())
	}
	if containsString(st.warnsList(), "no_home_reads") {
		t.Fatalf("tracee_unverified 时不能同时报 no_home_reads：%v", st.warnsList())
	}
}

// 兜底路径读能力不可用：只报 unavailable，不叠加 openat2/-y 告警
// （用户应被引向"读追踪不可用"，而不是去查 strace 的 openat2 支持）。
func TestProbeReadStateFallbackReadUnavailableWarns(t *testing.T) {
	e := probeEnv(t)
	e.ProbeCapsOverride = &ProbeCaps{Read: false, Write: true, Openat2: false, Y: true}
	st, err := e.newProbeReadState(context.Background(), t.TempDir(), ProbeOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.stop()
	if got := st.warnsList(); len(got) != 1 || got[0] != "unavailable" {
		t.Fatalf("兜底读不可用只应报 unavailable：%v", got)
	}
	if st.readTrace() != ReadTraceUnavailable {
		t.Fatalf("应为 unavailable：%q", st.readTrace())
	}
}

// 异常路径必须先停 reader 再删 work：否则 reader 仍持 FIFO fd，join 超时
// 会把进程级泄漏标记置位，后续探测全被拒绝（§5.2/§12）。
func TestProbeSessionStopsReaderBeforeCleanup(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{}
	r := &fakeRunner{err: errEnv("err.probe.session", nil, nil)}
	_, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/nonexistent/zz-probe-test-app"}, Mounter: m, Runner: r,
	}, m, r)
	if err == nil {
		t.Fatal("runner 失败应返回错误")
	}
	if e.probeReaderBlocked() {
		t.Fatal("异常路径返回后 reader 必须已经停止")
	}
	left, _ := filepath.Glob(filepath.Join(e.TmpRoot, "appclone-probe-*"))
	if len(left) != 0 {
		t.Fatalf("reader 正常停止后工作目录应被清掉：%v", left)
	}
}

// 可疑失败重跑会删掉旧 work 并换新目录：锁绑定源与 DBus socket 必须在新
// work 下重建，否则重跑 bind 到不存在的源，失败又被当成"应用正常退出"。
func TestProbeSessionRerunRebuildsLockAndBus(t *testing.T) {
	e := probeEnv(t)
	lock := filepath.Join(e.Resolver.Home, ".config", "TestApp", "lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &rerunMounter{}
	var r *fifoRunner
	var lockSrc, lockDst, busSock string
	var lockMissing, busMissing bool
	r = &fifoRunner{
		lines: []string{
			`1234  openat(AT_FDCWD, "` + filepath.Join(e.Resolver.Home, ".config/TestApp/settings.json") + `", O_RDONLY) = 3`,
		},
		step: func(call int) (SessionResult, error) {
			if call == 1 {
				return SessionResult{Started: true, ExitCode: 1}, nil
			}
			for i := 0; i+2 < len(r.argv); i++ {
				switch r.argv[i] {
				case "--bind":
					if r.argv[i+2] == lock {
						lockSrc, lockDst = r.argv[i+1], r.argv[i+2]
						if _, err := os.Stat(lockSrc); err != nil {
							lockMissing = true
						}
					}
				case "--setenv":
					if r.argv[i+1] == "DBUS_SESSION_BUS_ADDRESS" {
						if p := strings.TrimPrefix(r.argv[i+2], "unix:path="); p != r.argv[i+2] {
							// --print-address 原文带 ",guid=..."，它不是路径的一部分。
							busSock = strings.SplitN(p, ",", 2)[0]
							if _, err := os.Stat(busSock); err != nil {
								busMissing = true
							}
						}
					}
				}
			}
			return SessionResult{Started: true}, nil
		},
	}
	if _, err := e.probeSession(context.Background(), "testapp", ProbeOptions{
		Command: []string{"/usr/bin/testapp"}, Mounter: m, Runner: r, Rounds: 1,
		LockFile: []string{"~/.config/TestApp/lock"},
	}, m, r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("应重跑一次：%d", r.calls)
	}
	if lockDst != lock || lockSrc == "" {
		t.Fatalf("重跑 argv 必须包含锁绑定：src=%q dst=%q", lockSrc, lockDst)
	}
	if lockMissing {
		t.Fatalf("锁绑定源必须在新 work 下存在：%s", lockSrc)
	}
	if busSock != "" && busMissing {
		t.Fatalf("DBus socket 必须在新 work 下存在：%s", busSock)
	}
}
