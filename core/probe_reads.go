package core

// 读证据会话状态（ADR-034 §3.3/§4/§5/§11）：把"预检 → FIFO → 每轮命令 →
// 停止 → 告警/三态"收在一个类型里，确认页横幅与档案的 read_trace 同源。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// readsWarnOrder 是 §3.3 的固定顺序。no_home_reads 之前为完整性告警，
// 其后的证据告警只描述观察结果，不改变 ReadTrace。
var readsWarnOrder = []string{
	"unavailable", "manual_finish", "preflight_failed", "strace_failed", "parse_error",
	"size_exceeded", "no_fd_annotation", "openat2_unavailable",
	"openat2_flags_hidden", "reader_stuck", "setuid_child",
	"no_home_reads", "tracee_unverified", "round_marker_dropped",
}

func isIntegrityWarn(key string) bool {
	for _, k := range readsWarnOrder {
		if k == "no_home_reads" {
			return false
		}
		if k == key {
			return true
		}
	}
	return false
}

// probeReadState 是一次探测的读追踪状态。reader 为 nil 表示没有可用的 FIFO
// （overlay 下仍可继续，读证据记 unavailable）。
type probeReadState struct {
	e   *Env
	opt ProbeOptions

	caps     ProbeCaps
	reader   *traceReader
	stderr   *probeStderr
	readOK   bool // 预检确认读列表可用（且未被 DisableReadTrace 关闭）
	needRead bool

	warns map[string]bool

	reads       []string
	flagsHidden []string
	stats       StraceStats
	execSeen    bool
	execs       []string
	execCalls   []ExecCall
	traceErr    error
	unclean     bool // 任一轮 ExitCode!=0（会话不干净，读证据不可信）

	// keyring 证据来自 sendmsg 扫描：与读路径是两套口径（方案 §6）。
	keyringHits    []string
	keyringDropped int
}

// newProbeReadState 做能力预检并启动读证据 FIFO；写入始终从 overlay upper 收集。
func (e *Env) newProbeReadState(ctx context.Context, traceDir string, opt ProbeOptions) (*probeReadState, error) {
	st := &probeReadState{e: e, opt: opt, warns: map[string]bool{}}
	if opt.DisableReadTrace {
		// 排障/基准对照：不碰 FIFO、不跑预检，写证据照常（overlay upper）。
		st.addWarn("unavailable")
		return st, nil
	}
	var caps ProbeCaps
	if e.ProbeCapsOverride != nil {
		// 单测注入：不碰宿主的 strace/bwrap。
		caps = *e.ProbeCapsOverride
	} else {
		var err error
		caps, err = e.ProbeCaps(ctx, traceDir)
		if err != nil {
			// bwrap 预检失败是硬错误，不能降级成"读不可用"。
			return nil, err
		}
	}
	st.caps = caps
	if !caps.Read || opt.DisableReadTrace {
		switch {
		case opt.DisableReadTrace:
			// 排障/基准对照：显式关闭。
			st.addWarn("unavailable")
		default:
			// overlay 路径写证据来自 upper，读失败只降级证据。
			st.addWarn("preflight_failed")
		}
	} else {
		st.readOK = true
		st.needRead = true
		if !caps.Y {
			st.addWarn("no_fd_annotation")
		}
		if !caps.Openat2 {
			st.addWarn("openat2_unavailable")
		}
	}
	if !st.readOK {
		// overlay 下没有读能力：写证据来自 upper，不需要 FIFO。
		return st, nil
	}
	if st.stderr == nil {
		st.stderr = newProbeStderr(opt.Stderr)
	}
	r, rerr := newTraceReaderOpts(traceDir, e.Resolver.Home, e.markProbeReaderLeaked, !st.readOK)
	if rerr != nil {

		st.readOK = false
		st.addWarn("unavailable")
		return st, nil
	}
	st.reader = r
	return st, nil
}

func (st *probeReadState) addWarn(key string) {
	if st == nil || key == "" || st.warns[key] {
		return
	}
	st.warns[key] = true
	if st.opt.OnReadsWarn != nil {
		st.opt.OnReadsWarn(key)
	}
}

// stderrWriter 是交给 runner 的 stderr：有采集器时走采集器（它再转发给用户），
// 否则原样交给调用方（可能为 nil）。
func (st *probeReadState) stderrWriter() io.Writer {
	if st != nil && st.stderr != nil {
		return st.stderr
	}
	if st != nil {
		return st.opt.Stderr
	}
	return nil
}

// readSyscalls 是读列表（§4.1）；openat2 只在预检确认可用时加入。
func (st *probeReadState) readSyscalls() []string {
	out := []string{"open", "openat"}
	if st.caps.Openat2 {
		out = append(out, "openat2")
	}
	return append(out, "execve", "execveat")
}

// keyringSyscalls 是 keyring 证据专用的额外系统调用。单独成函数、在
// command() 里统一拼接一次：它是**证据**采集，不是读路径采集，绝不能混进
// readSyscalls()（probeCapsReadList 的 golden 与读能力预检都依赖那份列表）。
func keyringSyscalls() []string {
	return []string{"sendmsg"}
}

// command 生成 overlay 会话命令；读能力不可用时直接运行 bwrap。
func (st *probeReadState) command(inner []string) []string {
	if st == nil || st.reader == nil {
		return inner
	}
	if !st.readOK {
		return inner
	}
	list := st.readSyscalls()
	// sendmsg 只用于 keyring 字面量扫描（方案 §6）。唯一拼接点，不改
	// readSyscalls()——probeCapsReadList 的 golden 依赖它保持原样。
	list = append(list, keyringSyscalls()...)
	return StraceCommandWith(StraceSpec{
		Binary:   st.e.stracePath(),
		LogPath:  st.reader.path(),
		Syscalls: list,
		Y:        st.caps.Y,
		V:        true,
		Tracee:   inner,
	})
}

// readsSoFar 供进度显示（噪声过滤前的去重读路径数）。
func (st *probeReadState) readsSoFar() int {
	if st == nil || st.reader == nil {
		return 0
	}
	return st.reader.readCount()
}

// markRound 在每轮 runner.Run 之前写轮次标记行；没有 reader 时 no-op。
func (st *probeReadState) markRound(n int) {
	if st == nil || st.reader == nil {
		return
	}
	st.reader.markRound(n)
}

// checkRound 在每轮结束后检查会话期 stderr 与退出码。strace 的 stderr 可能
// 在会话结束才被读完，所以 stop 之后还会再查一次。
func (st *probeReadState) checkRound(res SessionResult) {
	if st == nil {
		return
	}
	if res.ExitCode != 0 && !res.ManualFinish {
		st.unclean = true
	}
	if st.stderr == nil {
		return
	}
	if st.stderr.sawPtraceDenied() {
		st.addWarn("strace_failed")
	}
	if st.stderr.sawSetuid() {
		st.addWarn("setuid_child")
	}
}

// stop 停止 reader 并吸收结果。可重复调用（reader 的 stopOnce 保证只停一次）。
func (st *probeReadState) stop() {
	if st == nil || st.reader == nil {
		return
	}
	res, err := st.reader.stopAndResult()
	st.reads = res.Reads
	st.flagsHidden = res.FlagsHidden
	st.stats = res.Stats
	if st.stats.RoundMarkerDropped > 0 {
		st.addWarn("round_marker_dropped")
	}
	st.execSeen = res.ExecSeen
	st.execs = res.Execs
	st.execCalls = deepCopyExecCalls(res.ExecCalls)
	st.keyringHits = res.KeyringHits
	st.keyringDropped = res.KeyringTraceDropped
	if err != nil {
		st.traceErr = err
		st.addWarn("reader_stuck")
	}
	if st.stderr != nil {
		st.stderr.flush()
		if st.stderr.sawPtraceDenied() {
			st.addWarn("strace_failed")
		}
		if st.stderr.sawSetuid() {
			st.addWarn("setuid_child")
		}
	}
	st.checkExecSetuid()
}

// checkExecSetuid 检查观察到执行的目标文件是否带 setuid 位。strace 的 execve
// 目标在解析结果里；stat 失败不影响结论（只是没有这条告警）。
func (st *probeReadState) checkExecSetuid() {
	for _, p := range st.execs {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSetuid != 0 {
			st.addWarn("setuid_child")
			return
		}
	}
}

// finalize 补齐依赖计数/活性的告警（§3.3）。必须在 stop 之后、计算
// ReadTrace 之前调用。
func (st *probeReadState) finalize(writes int, activity bool) {
	if st == nil {
		return
	}
	if st.readOK {
		if st.stats.ParseErrors > 0 {
			st.addWarn("parse_error")
		}
		if st.stats.SizeExceeded {
			st.addWarn("size_exceeded")
		}
		if st.stats.NoFDAnnotation {
			st.addWarn("no_fd_annotation")
		}
		if st.stats.Openat2FlagsHidden {
			st.addWarn("openat2_flags_hidden")
		}
	}
	if st.unclean && activity {
		// 应用跑过但退出码非零：读证据可能截断，不能静默当 ok。
		st.addWarn("strace_failed")
	}
	if st.readOK && writes > 0 && !st.execSeen {
		st.addWarn("tracee_unverified")
	}
	if st.readOK && !st.hasIntegrity() && !st.unclean && len(st.reads) == 0 &&
		!st.warns["tracee_unverified"] {
		// tracee 未验证时"没读到"没有解释力：应用可能根本没被 exec，
		// 两条语义相反的告警不能同时出现（plan §1 决策表）。
		st.addWarn("no_home_reads")
	}
}

func (st *probeReadState) hasIntegrity() bool {
	for k := range st.warns {
		if isIntegrityWarn(k) {
			return true
		}
	}
	return false
}

// readTrace 推导四态（§3.3）。只看完整性告警；证据告警不改变结论。
func (st *probeReadState) readTrace() string {
	if st == nil || !st.readOK {
		return ReadTraceUnavailable
	}
	if st.warns["strace_failed"] {
		if len(st.reads) > 0 {
			return ReadTracePartial
		}
		return ReadTraceUnavailable
	}
	if st.hasIntegrity() {
		return ReadTracePartial
	}
	return ReadTraceOK
}

// warnsList 按固定顺序输出告警键。
func (st *probeReadState) warnsList() []string {
	if st == nil {
		return nil
	}
	var out []string
	for _, k := range readsWarnOrder {
		if st.warns[k] {
			out = append(out, k)
		}
	}
	return out
}

// homeReadsNoise 是噪声过滤前的读路径里命中 §7.2 清单的条数。
func (st *probeReadState) homeReadsNoise() int {
	if st == nil {
		return 0
	}
	n := 0
	for _, rel := range st.reads {
		abs := filepath.Join(st.e.Resolver.Home, filepath.FromSlash(rel))
		if st.e.Resolver.isReadNoise(abs, st.e.Base) {
			n++
		}
	}
	return n
}

// AppendStaticCandidates 把 L0 静态线索补成"不勾选、只提示"的候选（§4.5）。
// 只补不存在的 owner；cache 不生成行；Public/Shared/Keyring/System 保留
// Classify 的类别，但 Why 统一是 static_hint——"为什么出现这一行"始终是
// 静态线索，类别本身由 DisplayClass 表达。
func (r *PathResolver) AppendStaticCandidates(cands []Candidate, staticPaths []string) []Candidate {
	seen := map[string]bool{}
	for _, c := range cands {
		seen[realKey(c.Path)] = true
	}
	var add []Candidate
	for _, raw := range staticPaths {
		abs, err := r.Expand(raw)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		key := realKey(abs)
		if seen[key] {
			continue
		}
		cls, _, _, warn := r.Classify(abs)
		if cls == ClassCache {
			continue
		}
		c := Candidate{
			Path: abs, Class: cls, DisplayClass: cls,
			Checked: false, Reads: 0, Warning: warn, Why: "probe.why.static_hint",
		}
		seen[key] = true
		add = append(add, c)
	}
	sort.Slice(add, func(i, j int) bool { return add[i].Path < add[j].Path })
	return append(cands, add...)
}
