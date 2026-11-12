package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// ProbeProgress 是探测的进度事件。GUI 靠它显示"已发现 N 个写入条目"，
// 而不是等全部结束才一次性画出来——十几秒到一分钟的空屏会被当成卡死。
//
// Entries 是**写入条目数**，不是次数（overlay 拿不到次数，§6.1 文案红线）。
type ProbeProgress struct {
	Stage   string // round_done | fallback（无 fuse-overlayfs，本次走 strace）
	Round   int
	Rounds  int
	Entries int
	Reads   int // 跨轮累计去重后的 HOME 读取路径数（噪声过滤前）
}

// ProbeOptions 是探测的全部可调项。
type ProbeOptions struct {
	Command  []string  // 应用命令（Exec + 参数）
	LockFile []string  // L0 预填的锁文件（可空）
	Stderr   io.Writer // 会话的 stderr（GUI 无 stderr，可为 nil）
	Mounter  OverlayMounter
	Runner   SessionRunner
	// Progress 可为 nil；调用发生在后台会话线程里，调用方自己负责切回 UI 线程。
	Progress func(ProbeProgress)
	// Timeout 是**每一轮**会话的硬上限：用户开了应用就不管了的情况必须有尽头。
	Timeout time.Duration
	// Rounds 是要跑几轮，默认 2（§4）。登录态、token 往往第二次启动才落盘：
	// 只跑一轮的典型结果就是"探到了配置目录、没探到凭据目录"。
	Rounds int
	// StaticPaths 是 L0 静态预填的路径（.desktop/AppArmor/Flatpak 元数据）。
	// 只补不存在的 owner，不升级/降级类别，也不占 20 条只读上限。
	StaticPaths []string
	// OnReadsWarn 在每条读追踪告警产生时立即回调（可在后台线程调用）。
	OnReadsWarn func(reason string)
	// DisableReadTrace 关闭读追踪（零值 = 开启）。只用于性能对照/排障，
	// 产品路径不设置它；关闭时 ReadTrace=unavailable 且不报 no_home_reads。
	DisableReadTrace bool
}

// ProbeResult 是一次探测的产出。
type ProbeResult struct {
	Profile     Profile     `json:"-"`
	Candidates  []Candidate `json:"candidates"`
	Whiteouts   []string    `json:"whiteouts"`
	Quarantined string      `json:"quarantined,omitempty"` // 非空 = 保留现场的目录
	ExitCode    int         `json:"session_exit_code"`

	Writes         int      `json:"writes"`
	HomeReads      int      `json:"home_reads"`
	HomeReadsNoise int      `json:"home_reads_noise"`
	Started        bool     `json:"started"`
	ExecSeen       bool     `json:"exec_seen"`
	Activity       bool     `json:"activity"`
	ReadsDropped   int      `json:"reads_dropped"`
	ParseErrors    int      `json:"parse_errors"`
	SizeExceeded   bool     `json:"size_exceeded"`
	ReadsHidden    int      `json:"reads_hidden"`
	ReadsWarns     []string `json:"reads_warns,omitempty"`
	ReadTrace      string   `json:"read_trace"`
	// KeyringEvidence 是 "" / linked / observed（方案 §6）。它只描述证据，
	// 默认值由 KeyringDefault 与能力检查共同决定。
	KeyringEvidence string `json:"keyring_evidence"`
	// KeyringTraceDropped 是 sendmsg 证据扫描被单独上限截断后丢弃的行数。
	// 非零表示"未观测到 keyring"可能是证据不全，而不是应用真的不用——
	// 必须可诊断，否则探测结论会静默降级。
	KeyringTraceDropped int `json:"keyring_trace_dropped,omitempty"`
}

const probeTimeoutDefault = 30 * time.Minute

// reportProbeProgress 把"到目前为止合并后的条目数"交给调用方。
// 用候选数而不是原始 upper 条目数：白化文件与 XDG 根本身不是"会隔离的写入"，
// 拿它们计数会让界面数字比最终确认页多出一截。
func reportProbeProgress(opt ProbeOptions, e *Env, round, rounds int, entries []UpperEntry, reads int) {
	if opt.Progress == nil {
		return
	}
	cands, _ := e.Resolver.MergeUpper(entries)
	opt.Progress(ProbeProgress{
		Stage:   "round_done",
		Round:   round + 1,
		Rounds:  rounds,
		Entries: len(cands),
		Reads:   reads,
	})
}

// Probe 跑一次 overlay-diff 探测并产出档案（docs/discover.md §3 全节）。
//
// 顺序是契约：闸门 → 扫残留 → 挂 overlay → 起会话 → **先收集、再判污染、
// 最后才删**（反过来这个断言永远为空）。
func (e *Env) Probe(ctx context.Context, app string, opt ProbeOptions) (ProbeResult, error) {
	// root 会话下 --bind / / 是 rw，误操作能改宿主；而且 root 下 bwrap 本来就能跑通，
	// "验证通过"会把非特权路径的问题全藏起来（§3.1 末段）。
	if err := CheckNotRoot(); err != nil {
		return ProbeResult{}, err
	}
	if e.probeReaderBlocked() {
		// 上一次探测的采集线程还活着（2 秒 join 超时）。它仍持有旧 FIFO，
		// 再开一次探测只会叠加泄漏；先让用户重跑/重启（§5.2）。
		return ProbeResult{}, errEnv("err.probe.reader_stuck", nil, nil)
	}
	if err := ValidateAppKey(app); err != nil {
		return ProbeResult{}, err
	}
	if len(opt.Command) == 0 {
		return ProbeResult{}, errInput("err.probe.no_command", map[string]any{"app": app})
	}
	// 审计写层根提前以 0700 建好：探测会话同样要 tmpfs 遮蔽它，且不能让
	// bwrap 在宿主父目录里自建一个默认权限的挂载点。
	if err := e.ensureAuditRoot(); err != nil {
		return ProbeResult{}, err
	}
	mounter := opt.Mounter
	if mounter == nil {
		mounter = e.Mounter
	}
	if mounter == nil {
		mounter = fuseOverlayMounter{}
	}
	runner := opt.Runner
	if runner == nil {
		runner = setsidRunner{}
	}
	// 降级判据（§3.4 + ADR-032）：**只有后端确实不存在**时，才在 strace 可用
	// 的前提下降级。fuse-overlayfs 在、但挂载被系统拒绝（权限/沙盒/超时）是
	// 可修复的环境问题：静默走 strace 会把权限问题藏起来，还会给用户一份低
	// 置信度档案。这类错误必须原样交给界面/CLI。
	if err := mounter.Available(); err != nil {
		if !ProbeBackendMissing(err) {
			return ProbeResult{}, err
		}
		if _, serr := exec.LookPath("strace"); serr != nil {
			// fuse 与 strace 都缺：报"读追踪不可用"的环境错误，而不是把 fuse
			// 的原始错误抛给界面——后者无法归类，用户也不知道该装哪个包。
			return ProbeResult{}, errEnv("err.probe.strace_unavailable", nil, serr)
		}
		if opt.Stderr != nil {
			fmt.Fprintln(opt.Stderr, "appclone: fuse-overlayfs 不可用，改用 strace 跟踪（结果置信度较低，且需要多开几次应用）")
		}
		// 降级事件由 probeStrace 在闸门之后发（见那里的注释）：还没确认能真的
		// 开跑就告诉界面"已降级"，紧接着又报 app_running，横幅会和错误文案打架。
		return e.probeStrace(ctx, app, opt, runner)
	}
	return e.probeSession(ctx, app, opt, mounter, runner)
}

// probeStrace 是 §3.4 的降级路径：不罩 overlay（HOME 就是真实的），
// 靠 strace 抓"带写入意图的 open/rename/unlink"。
//
// 与 overlay 路径的差别必须写在档案里（`probe_method = "strace"`），
// 界面要按低置信展示——两套结果看起来一样，但一套是"确定写过"、
// 一套是"可能写过"。
// ProbeBackendMissing 区分"探测后端根本不存在"与"后端存在但挂载失败"。
// 前者可以降级 strace；后者（Permission denied、EPERM、超时）是权限/沙盒
// 问题，自动降级只会掩盖它——ADR-032。
// ProbeErrorKey 取 core 错误键；不是 *Error 时返回空串。界面按它区分
// "组件缺失 / 写层不可写 / 挂载被拒"（三者的处置完全不同），不解析文案。
func ProbeErrorKey(err error) string {
	var ce *Error
	if !errors.As(err, &ce) {
		return ""
	}
	return ce.Key
}

func ProbeBackendMissing(err error) bool { return ProbeBackendKeyMissing(ProbeErrorKey(err)) }

// ProbeBackendKeyMissing 是上面那条判据的键形态：界面手里只有键（错误已经
// 不再携带底层 errno），但仍要判断"是组件缺失还是挂载被拒"。
func ProbeBackendKeyMissing(key string) bool {
	switch key {
	case "err.probe.no_fuse_overlayfs", "err.probe.no_fusermount", "err.probe.no_dev_fuse":
		return true
	}
	return false
}

// warnProbeBus 在探测总线起不来时告诉用户：fail-closed 后应用拿不到会话
// 总线，读证据可能不完整。不能静默——否则用户会以为"应用本来就不读配置"。
func warnProbeBus(opt ProbeOptions) {
	if opt.Stderr != nil {
		fmt.Fprintln(opt.Stderr, "appclone: 未能启动私有 DBus 总线；探测期间应用拿不到会话总线，读证据可能不完整")
	}
}

// startProbeBus 起探测私有总线与临时 keyring，返回应用侧地址与合并的停止
// 函数。总线失败 → 不存在的私有地址 + 告警（fail-closed，绝不继承宿主总线）；
// keyring 失败只告警，探测继续（方案 §6）。
func (e *Env) startProbeBus(work string, opt ProbeOptions) (string, func()) {
	busDir := filepath.Join(work, "dbus")
	sess, err := e.startSessionBus(busDir)
	if err != nil || sess == nil {
		warnProbeBus(opt)
		return unavailableBusAddress(busDir), nil
	}
	kr := e.startProbeKeyring(sess.Addr(), opt)
	stop := func() {
		if kr != nil {
			kr.Stop()
		}
		sess.Stop()
	}
	return sess.Addr(), stop
}

func (e *Env) probeStrace(ctx context.Context, app string, opt ProbeOptions, runner SessionRunner) (ProbeResult, error) {
	if running, err := e.AppRunning(opt.Command[0]); err != nil {
		return ProbeResult{}, err
	} else if running {
		return ProbeResult{}, errBusy("err.probe.app_running", map[string]any{"exec": filepath.Base(opt.Command[0])})
	}
	// 闸门过了、strace 会话确定要跑，才把"本次降级"告诉界面：GUI 会在探测页
	// 起一条常驻横幅，切到确认态也不消失。提前发就等于给一个不会发生的
	// 会话挂横幅（闸门失败时用户看到的应是 app_running，不是 fuse 提示）。
	if opt.Progress != nil {
		opt.Progress(ProbeProgress{Stage: "fallback"})
	}
	work, err := os.MkdirTemp(e.tmpRoot(), fmt.Sprintf("appclone-strace-%d-", e.UID))
	if err != nil {
		return ProbeResult{}, errEnv("err.env.mkdir", map[string]any{"path": e.tmpRoot()}, err)
	}
	keepWork := false
	var trace *probeReadState
	defer func() {
		// reader 必须先于 work 清理停止：它仍持有 FIFO fd，先删目录会让它
		// 读到已删除的 inode，join 超时还会把进程级泄漏标记置位（§5.2/§12）。
		if trace != nil {
			trace.stop()
			if trace.traceErr != nil {
				keepWork = true
			}
		}
		if !keepWork {
			os.RemoveAll(work)
		}
	}()
	runtimeDir := filepath.Join(work, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return ProbeResult{}, errEnv("err.env.mkdir", map[string]any{"path": runtimeDir}, err)
	}
	traceDir := filepath.Join(work, "trace")
	if err := os.MkdirAll(traceDir, 0o700); err != nil {
		return ProbeResult{}, errEnv("err.env.mkdir", map[string]any{"path": traceDir}, err)
	}
	bus, stopBus := e.startProbeBus(work, opt)
	if stopBus != nil {
		// 用闭包而不是 defer stopBus()：重跑会换 work 并重启总线，
		// defer 必须在返回时停**当前**那一个。
		defer func() {
			if stopBus != nil {
				stopBus()
			}
		}()
	}
	locks, _, err := e.probeLockBinds(work, opt.LockFile)
	if err != nil {
		return ProbeResult{}, err
	}
	rounds := opt.Rounds
	if rounds <= 0 {
		rounds = 2
	}
	timeout := opt.Timeout
	if timeout == 0 {
		timeout = probeTimeoutDefault
	}
	// 兜底的写证据完全来自 strace：FIFO/写列表不可用就没有可信结果，
	// 必须报环境错误而不是静默返回零写入（§11.2）。
	trace, err = e.newProbeReadState(ctx, traceDir, opt, true)
	if err != nil {
		return ProbeResult{}, err
	}
	if !trace.caps.Write {
		return ProbeResult{}, errEnv("err.probe.strace_unavailable", nil, nil)
	}
	var last SessionResult
	for round := 0; round < rounds; round++ {
		inner, aerr := ProbeBwrapArgs(ProbeSpec{
			Home:         e.Resolver.Home,
			Base:         Real(e.Base), // 软链 Base 不能作为 bwrap 挂载目标
			AuditRoot:    e.auditRoot(),
			RuntimeDir:   runtimeDir,
			RuntimeSock:  probeRuntimeSocks(e.Resolver.XDG["RUNTIME_DIR"], runtimeDir),
			DBusAddress:  bus,
			HostBusPaths: e.hostBusPaths(),
			SharedWrite:  e.probeSharedWrites(),
			LockBinds:    locks,
			TraceDir:     traceDir,
			Command:      opt.Command,
			NoOverlay:    true,
		})
		if aerr != nil {
			return ProbeResult{}, aerr
		}
		if opt.Stderr != nil && rounds > 1 {
			fmt.Fprintf(opt.Stderr, "appclone: 第 %d/%d 轮 —— 请打开应用用一下，然后把它关掉\n", round+1, rounds)
		}
		sctx, cancel := context.WithTimeout(ctx, timeout)
		res, runErr := runner.Run(sctx, trace.command(inner, true), trace.stderrWriter())
		cancel()
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				return ProbeResult{}, ErrCanceled
			}
			if errors.Is(runErr, context.DeadlineExceeded) {
				// strace 路径没有 overlay 现场可保留（work 是普通临时目录）：
				// 用单独一条不提"保留现场"、也没有 {quarantine} 的文案。
				return ProbeResult{}, newErr(CodeProbe, "err.probe.timeout_strace", map[string]any{
					"app": app, "round": round + 1, "rounds": rounds,
				}, runErr)
			}
			return ProbeResult{}, newErr(CodeEnv, "err.probe.session", map[string]any{
				"app": app, "message": runErr.Error(),
			}, runErr)
		}
		last = res
		trace.checkRound(res)
		reportProbeProgress(opt, e, round, rounds, trace.writesSoFar(), trace.readsSoFar())
	}
	trace.stop()
	if trace.traceErr != nil {
		// reader 卡住时保留现场（它仍持 FIFO fd）。
		keepWork = true
		return ProbeResult{}, trace.traceErr
	}
	entries := trace.writesSoFar()
	writes := e.countProbeWrites(entries)
	activity := last.Started || len(entries) > 0 || len(trace.reads) > 0 || trace.execSeen
	if last.ExitCode != 0 && !activity {
		return ProbeResult{}, newErr(CodeEnv, "err.probe.session_failed", map[string]any{
			"app": app, "code": last.ExitCode,
		}, nil)
	}
	if last.Started && !trace.execSeen && last.ExitCode != 0 && trace.readOK {
		return ProbeResult{}, newErr(CodeEnv, "err.probe.session_not_started", map[string]any{
			"code": last.ExitCode,
		}, nil)
	}
	trace.finalize(writes, activity)
	cands, whiteouts, readsHidden, credWarns := e.Resolver.MergeUpperWithReads(
		entries, trace.reads, trace.flagsHidden, e.Base, app)
	cands = e.Resolver.AppendStaticCandidates(cands, opt.StaticPaths)

	evidence := e.keyringEvidence(opt.Command[0], cands, trace.reads, trace.keyringHits)
	probe := Profile{
		App:       app,
		Exec:      opt.Command[0],
		ExecArgs:  append([]string{}, opt.Command[1:]...),
		Ignored:   DefaultIgnored(),
		LockFiles: e.lockHostPaths(locks),
	}
	// 默认策略（方案 §6）：只有 observed 且能力可用才默认开启。KeyringSet
	// 保持 false——这是"探测默认"，不是用户显式选择，合并时既有档案优先。
	if on, _ := e.keyringDefault(evidence); on {
		probe.IsolateKeyring = true
	}
	probe.Unsupported = append(probe.Unsupported, probeUnsupported(e)...)
	sort.Slice(probe.Unsupported, func(i, j int) bool { return probe.Unsupported[i].Path < probe.Unsupported[j].Path })
	p := e.Resolver.DeriveProfile(probe, nil, "strace", trace.readTrace(), cands)
	p.Warnings = append(p.Warnings, credWarns...)
	if _, warn := e.keyringDefault(evidence); warn != "" {
		p.Warnings = append(p.Warnings, warn)
	}
	return ProbeResult{
		Profile:             p,
		Candidates:          cands,
		Whiteouts:           whiteouts,
		ExitCode:            last.ExitCode,
		Writes:              writes,
		HomeReads:           len(trace.reads),
		HomeReadsNoise:      trace.homeReadsNoise(),
		Started:             last.Started,
		ExecSeen:            trace.execSeen,
		Activity:            activity,
		ReadsDropped:        trace.stats.ReadsDropped,
		ParseErrors:         trace.stats.ParseErrors,
		SizeExceeded:        trace.stats.SizeExceeded,
		ReadsHidden:         readsHidden,
		ReadsWarns:          trace.warnsList(),
		ReadTrace:           trace.readTrace(),
		KeyringEvidence:     evidence,
		KeyringTraceDropped: trace.keyringDropped,
	}, nil
}

// probeRoundFailed 把"某一轮没跑起来"翻译成退出码。工作目录的处置（停止
// 会话进程 → 卸载 → 删除/保留现场）由调用方用 teardown 完成：顺序错了会
// 穿过 merged 挂载点删到 upper 里的内容（见 discard 的注释）。
func (e *Env) probeRoundFailed(app string, round, rounds int, runErr error, quarantined string) (ProbeResult, error) {
	if errors.Is(runErr, context.Canceled) {
		// 多数取消没有现场（teardown 只在公共目录可能被写进 upper 时才保留）；
		// 一旦保留了，必须把路径告诉用户，否则文件被搬走却无处可寻。
		if quarantined != "" {
			return ProbeResult{}, newErr(CodeOK, "err.probe.canceled_kept", map[string]any{
				"quarantine": quarantined,
			}, ErrCanceled)
		}
		return ProbeResult{}, ErrCanceled
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		// 保留了现场就必须给出恢复路径，否则用户不知道该去哪里找探测期
		// 写下的内容（§3.2.1）；写层为空时 teardown 会安全删掉，此时不能
		// 再用"保留现场：{quarantine}"的文案（否则是悬空的冒号 + 假承诺）。
		if quarantined == "" {
			return ProbeResult{}, newErr(CodeProbe, "err.probe.timeout_no_scene", map[string]any{
				"app": app, "round": round + 1, "rounds": rounds,
			}, runErr)
		}
		return ProbeResult{}, newErr(CodeProbe, "err.probe.timeout", map[string]any{
			"app": app, "round": round + 1, "rounds": rounds, "quarantine": quarantined,
		}, runErr)
	}
	return ProbeResult{}, newErr(CodeEnv, "err.probe.session", map[string]any{
		"app": app, "message": runErr.Error(),
	}, runErr)
}

// probeSession 是去掉"非 root"闸门之后的探测本体：root 检查与它无关，
// 而单测必然以 root 跑（沙箱里没有别的用户），所以拆开才能测到编排逻辑。
func (e *Env) probeSession(ctx context.Context, app string, opt ProbeOptions, mounter OverlayMounter, runner SessionRunner) (ProbeResult, error) {
	// 闸门 1：目标应用不能正在跑。主实例在跑时应用会把参数转发给它然后自己退出，
	// 探测结果是"零写入"——与"应用真的不写盘"无法区分（§3.2.2）。
	if running, err := e.AppRunning(opt.Command[0]); err != nil {
		return ProbeResult{}, err
	} else if running {
		// 6 = 并发（"目标应用正在运行"），不是 7（实例在跑）。
		return ProbeResult{}, errBusy("err.probe.app_running", map[string]any{"exec": filepath.Base(opt.Command[0])})
	}
	// 闸门 2：overlay 可用（静态检查 + 冒烟，进程内缓存一次）。
	if err := mounter.Available(); err != nil {
		return ProbeResult{}, err
	}
	// 闸门 3：公共目录清单必须可信。读不到 user-dirs.dirs 时**不许回退默认名**：
	// 中文系统上那会漏掉 `~/下载`，用户存的文件照样落进 upper 被删（§3.2.1）。
	pubBinds, err := e.probePublicBinds()
	if err != nil {
		return ProbeResult{}, err
	}
	// 闸门 4：上一轮残留先扫掉（含仍挂载的），扫不干净就不跑（§3.2.3）。
	if err := e.sweepStaleProbes(); err != nil {
		return ProbeResult{}, err
	}

	work, err := e.newProbeWorkDir(app)
	if err != nil {
		return ProbeResult{}, err
	}
	// removed 标记"work 已经被处理过"，避免重复删/quarantine。
	// isMounted 是硬护栏：merged 还挂着时 os.RemoveAll(work) 会**穿过挂载点**
	// 遍历整个 $HOME 视图——upper 里探测期写下的文件会被 unlink 掉，而
	// removeall 只在最后才因 EBUSY 失败。任何路径都不许在这里越线。
	removed := false
	isMounted := false
	// reader 卡住时 work 是唯一还能找到现场的地方：删/搬之前必须知道。
	keepWork := false
	// stopTrace 在 trace 创建后赋值；discard/quarantine/teardown 都先调用它，
	// 保证"停 reader → 卸载 → 处置 work"的顺序在所有异常路径一致（§5.2）。
	stopTrace := func() {}
	discard := func() {
		stopTrace()
		if keepWork || removed || isMounted {
			return
		}
		removed = true
		os.RemoveAll(work)
	}
	quarantine := func() (string, error) {
		stopTrace()
		// 与 discard 对称的硬护栏：挂载还在时改名会把挂载点一起搬走，
		// 退出时卸载目标就不对了。当前调用点都在卸载之后，这里防未来。
		if isMounted {
			return work, errEnv("err.probe.quarantine_mounted", map[string]any{"path": work}, nil)
		}
		if keepWork {
			// reader 仍可能持 FIFO fd：现场不能搬，原路径就是用户能找到的位置。
			return work, nil
		}
		// 写层根可能在 tmpfs（/tmp、$XDG_RUNTIME_DIR），quarantine 优先落磁盘；
		// quarantineDir 负责 EXDEV 拷贝 + fsync，拷贝不动（子树不可读）时退回
		// 同 fs rename，任何失败都不删源（§3.2.1）。
		q, qerr := e.quarantineDir(work)
		if qerr != nil {
			// **绝不回退到 RemoveAll**：这里面可能就是用户刚存的文件（§3.2.1）。
			// 留着、把真实路径告诉用户，让人自己决定。
			return work, errEnv("err.probe.quarantine_failed", map[string]any{"path": work}, qerr)
		}
		removed = true
		return q, nil
	}

	traceDir := filepath.Join(work, "trace")
	if err := os.MkdirAll(traceDir, 0o700); err != nil {
		discard()
		return ProbeResult{}, errEnv("err.env.mkdir", map[string]any{"path": traceDir}, err)
	}
	// reader 必须在会话开始前就绪：strace 打开 FIFO 写端时没有读端会阻塞。
	// 预检也在挂 overlay 之前跑完——bwrap 硬错误不该先付出一次挂载。
	trace, err := e.newProbeReadState(ctx, traceDir, opt, false)
	if err != nil {
		discard()
		return ProbeResult{}, err
	}
	stopTrace = func() {
		trace.stop()
		if trace.traceErr != nil {
			// reader 仍持 FIFO fd：删/搬 work 会毁掉现场，也会让后续探测被
			// 进程级泄漏标记拒绝（§5.2）。
			keepWork = true
		}
	}
	// 任何错误路径都要停 reader（它持有 FIFO fd）；正常路径会在卸载前显式 stop。
	defer stopTrace()

	upper := filepath.Join(work, "upper")
	merged := filepath.Join(work, "merged")
	unmount, err := mounter.Mount(e.Resolver.Home, upper, filepath.Join(work, "work"), merged)
	if err != nil {
		// 极窄的竞态：Mount 已返回错误（典型是超时），但挂载点刚好在不该在
		// 的时刻生效了。这时删 work 会穿过 merged；宁可把目录留给下次
		// sweepStaleProbes（它会先卸载、再判污染）。
		if mounted(merged) {
			return ProbeResult{}, err
		}
		discard()
		return ProbeResult{}, err
	}
	// 卸载顺序与"先杀进程再卸载"（§3.2.3）：这里注册的 defer 是兜底，
	// 正常路径在下面显式调用并检查错误。
	isMounted = true
	defer func() {
		if isMounted {
			_ = unmount()
		}
	}()

	bus, stopBus := e.startProbeBus(work, opt)
	if stopBus != nil {
		// 用闭包而不是 defer stopBus()：重跑会换 work 并重启总线，
		// defer 必须在返回时停**当前**那一个。
		defer func() {
			if stopBus != nil {
				stopBus()
			}
		}()
	}
	// teardown 是异常路径唯一允许的收尾顺序：停 dbus → 卸载 → 处置工作目录
	// （§3.2.3「先杀进程再卸载」）。preserve 表示"应用可能已经跑过、不能
	// 直接删"：卸载后先做一次污染检查——公共目录进了 upper 才 quarantine，
	// 只有应用自身配置/缓存写入时和成功路径一样删掉（否则每次取消都在
	// 写层根攒一份用户不知道的垃圾）。读不出来时按"可能有内容"保留。
	teardown := func(preserve bool) (string, error) {
		stopTrace()
		if stopBus != nil {
			stopBus()
		}
		if isMounted {
			if uerr := unmount(); uerr != nil {
				// 挂载点没卸掉：什么都不删，留给启动前的残留扫描。
				return "", uerr
			}
			isMounted = false
		}
		if preserve {
			ents, present, rerr := readUpperTree(upper)
			switch {
			case rerr != nil:
				return quarantine()
			case !present || len(ents) == 0:
				discard()
				return "", nil
			case len(e.ProbeContaminated(ents, pubBinds)) > 0:
				return quarantine()
			default:
				discard()
				return "", nil
			}
		}
		discard()
		return "", nil
	}
	locks, lockCleanup, err := e.probeLockBinds(work, opt.LockFile)
	if err != nil {
		if _, terr := teardown(false); terr != nil {
			return ProbeResult{}, terr
		}
		return ProbeResult{}, err
	}
	// 重跑会为新 work 重新算锁绑定；defer 必须调用当前那一份。
	defer func() { lockCleanup() }()

	spec := ProbeSpec{
		Home:         e.Resolver.Home,
		Base:         Real(e.Base), // 软链 Base 不能作为 bwrap 挂载目标
		AuditRoot:    e.auditRoot(),
		Merged:       merged,
		RuntimeDir:   filepath.Join(work, "runtime"),
		RuntimeSock:  probeRuntimeSocks(e.Resolver.XDG["RUNTIME_DIR"], filepath.Join(work, "runtime")),
		DBusAddress:  bus,
		HostBusPaths: e.hostBusPaths(),
		PublicBinds:  pubBinds,
		SharedWrite:  e.probeSharedWrites(),
		LockBinds:    locks,
		Env:          probeGuardEnv(pubBinds),
		TraceDir:     traceDir,
		Command:      opt.Command,
	}
	argv, err := ProbeBwrapArgs(spec)
	if err != nil {
		if _, terr := teardown(false); terr != nil {
			return ProbeResult{}, terr
		}
		return ProbeResult{}, err
	}

	timeout := opt.Timeout
	if timeout == 0 {
		timeout = probeTimeoutDefault
	}
	rounds := opt.Rounds
	if rounds <= 0 {
		rounds = 2
	}
	var res SessionResult
	for round := 0; round < rounds; round++ {
		sctx, cancel := context.WithTimeout(ctx, timeout)
		if opt.Stderr != nil && rounds > 1 {
			fmt.Fprintf(opt.Stderr, "appclone: 第 %d/%d 轮 —— 请打开应用用一下，然后把它关掉\n", round+1, rounds)
		}
		r, runErr := runner.Run(sctx, trace.command(argv, false), trace.stderrWriter())
		cancel()
		if runErr != nil {
			// 没有可信证据。第 0 轮 Start 失败时应用还没 exec、写层安全；
			// 取消/超时与后续轮次的失败都可能已经有写入，必须先卸载再判断
			// 是否保留，绝不直接删写层。
			preserve := round > 0 || errors.Is(runErr, context.Canceled) ||
				errors.Is(runErr, context.DeadlineExceeded)
			q, terr := teardown(preserve)
			if terr != nil {
				return ProbeResult{}, terr
			}
			return e.probeRoundFailed(app, round, rounds, runErr, q)
		}
		if r.ExitCode == probeGuardFailed {
			// 正向断言失败：公共目录仍在 overlay 里。第 0 轮 guard 在 exec
			// 应用之前就退出了，写层里没有用户内容；后续轮次共用同一个 upper，
			// 前面轮次可能已经写进用户文件——那部分必须保留并告诉用户路径。
			q, terr := teardown(round > 0)
			if terr != nil {
				return ProbeResult{}, terr
			}
			if q != "" {
				return ProbeResult{}, errProbe("err.probe.bind_verify_failed_kept", map[string]any{
					"path": publicTargets(pubBinds), "quarantine": q,
				})
			}
			return ProbeResult{}, errProbe("err.probe.bind_verify_failed", map[string]any{
				"path": publicTargets(pubBinds),
			})
		}
		// 后续轮次沿用最后一轮的结果；前面轮次只要没报错就继续——
		// "第二轮才写 token" 正是要覆盖的情形。
		res = r
		trace.checkRound(r)
		// 中途读一次 upper 只为进度展示；读失败不影响主流程（结束时还有一次
		// 权威读取，错误会在那里上报）。
		if ents, present, rerr := readUpperTree(upper); rerr == nil && present {
			reportProbeProgress(opt, e, round, rounds, ents, trace.readsSoFar())
		}
	}

	// 会话结束 → 先停 reader、再停 dbus、最后卸载（§5：搬动/清理 work 前
	// 必须停 reader；顺序反了会与正在读的 FIFO 竞态）。
	trace.stop()
	if trace.traceErr != nil {
		if stopBus != nil {
			stopBus()
		}
		if isMounted {
			_ = unmount()
			isMounted = false
		}
		// 现场保留：reader 可能仍持 fd，删 work 会让它读到已删除的 inode。
		return ProbeResult{}, trace.traceErr
	}

	// 可疑失败重跑（§11.3）：只有读证据被尝试过、且 upper 里没有任何写入
	// （丢弃安全）时才做。去掉 strace、保留 overlay 重跑一次，拿到写证据即可；
	// 第一次已解析的读路径保留并合并，但重跑本身没有读追踪 → unavailable。
	rerun := false
	if trace.readOK && ctx.Err() == nil {
		if ents, present, rerr := readUpperTree(upper); rerr == nil && (!present || len(ents) == 0) {
			rerun = res.ExitCode != 0 ||
				(len(trace.reads) == 0 && trace.warns["strace_failed"])
		}
	}
	var entries []UpperEntry
	present := false
	var readErr error
	if rerun {
		if uerr := unmount(); uerr != nil {
			return ProbeResult{}, uerr
		}
		isMounted = false
		// 旧 work 即将删除：里面的 dbus socket 与锁绑定源都会消失。必须先停
		// 总线、清锁，再为新 work 重新创建；否则重跑会 bind 到不存在的源，
		// 失败又被当成"应用正常退出"，最终误报"没有观察到写入"（§11.3）。
		if stopBus != nil {
			stopBus()
			stopBus = nil
		}
		lockCleanup()
		lockCleanup = func() {}
		discard()
		work, err = e.newProbeWorkDir(app)
		if err != nil {
			return ProbeResult{}, err
		}
		removed = false
		bus, stopBus = e.startProbeBus(work, opt)
		locks, lockCleanup, err = e.probeLockBinds(work, opt.LockFile)
		if err != nil {
			if _, terr := teardown(false); terr != nil {
				return ProbeResult{}, terr
			}
			return ProbeResult{}, err
		}
		upper = filepath.Join(work, "upper")
		merged = filepath.Join(work, "merged")
		unmount2, merr := mounter.Mount(e.Resolver.Home, upper, filepath.Join(work, "work"), merged)
		if merr != nil {
			if mounted(merged) {
				return ProbeResult{}, merr
			}
			discard()
			return ProbeResult{}, merr
		}
		unmount = unmount2
		isMounted = true
		argv2, aerr := ProbeBwrapArgs(ProbeSpec{
			Home:         e.Resolver.Home,
			Base:         Real(e.Base), // 软链 Base 不能作为 bwrap 挂载目标
			AuditRoot:    e.auditRoot(),
			Merged:       merged,
			RuntimeDir:   filepath.Join(work, "runtime"),
			RuntimeSock:  probeRuntimeSocks(e.Resolver.XDG["RUNTIME_DIR"], filepath.Join(work, "runtime")),
			DBusAddress:  bus,
			HostBusPaths: e.hostBusPaths(),
			PublicBinds:  pubBinds,
			SharedWrite:  e.probeSharedWrites(),
			LockBinds:    locks,
			Env:          probeGuardEnv(pubBinds),
			Command:      opt.Command,
		})
		if aerr != nil {
			if _, terr := teardown(false); terr != nil {
				return ProbeResult{}, terr
			}
			return ProbeResult{}, aerr
		}
		rto := timeout
		if rto > 5*time.Minute {
			rto = 5 * time.Minute
		}
		sctx, cancel := context.WithTimeout(ctx, rto)
		var runErr error
		res, runErr = runner.Run(sctx, argv2, opt.Stderr)
		cancel()
		if runErr != nil {
			preserve := errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)
			q, terr := teardown(preserve)
			if terr != nil {
				return ProbeResult{}, terr
			}
			return e.probeRoundFailed(app, 0, 1, runErr, q)
		}
		if uerr := unmount(); uerr != nil {
			return ProbeResult{}, uerr
		}
		isMounted = false
		entries, present, readErr = readUpperTree(upper)
		// 重跑没有读追踪：三态明确降为 unavailable（不是 partial）。
		// 第一轮的 unclean 只描述"strace 会话退出码非零"，重跑是刻意不套
		// strace 的，不能再把它翻译成 strace_failed。
		trace.readOK = false
		trace.unclean = false
		trace.addWarn("unavailable")
	} else {
		if stopBus != nil {
			stopBus()
		}
		unmountErr := unmount()
		isMounted = false
		if unmountErr != nil {
			// 挂载点还在：这时删工作目录会遍历用户家目录视图并制造 whiteout，
			// 所以**不删**，留着并报环境错误。
			return ProbeResult{}, unmountErr
		}
		// 收集：先读，再判，最后才谈删（§3.2.3 末条）。
		entries, present, readErr = readUpperTree(upper)
	}
	if rerun && stopBus != nil {
		stopBus()
	}
	if readErr != nil {
		// 读不出来的东西不能顺手删：里面可能正是用户文件。与 teardown、
		// 审计层保持同一口径——读失败就保留现场。
		if _, qerr := quarantine(); qerr != nil {
			return ProbeResult{}, qerr
		}
		return ProbeResult{}, readErr
	}
	if !present {
		discard()
		return ProbeResult{}, errEnv("err.probe.no_upper", map[string]any{"path": upper}, nil)
	}
	// 会话内的正向断言失败：公共目录仍落在 overlay 里（bind 没生效）。
	// 单轮时应用还没跑，删写层安全；多轮时前面轮次可能已写入用户文件，
	// 必须保留现场（与循环内的同一判据保持对称）。
	if res.ExitCode == probeGuardFailed {
		if rounds > 1 {
			q, qerr := quarantine()
			if qerr != nil {
				return ProbeResult{}, qerr
			}
			return ProbeResult{Quarantined: q}, errProbe("err.probe.bind_verify_failed_kept", map[string]any{
				"path": publicTargets(pubBinds), "quarantine": q,
			})
		}
		discard()
		return ProbeResult{}, errProbe("err.probe.bind_verify_failed", map[string]any{
			"path": publicTargets(pubBinds),
		})
	}
	// 会话"跑起来了但立刻失败"（bwrap 建 namespace 失败、Exec 不存在）与
	// "应用正常退出但没写"必须分开：前者是环境/输入问题（3），后者才是 4。
	writes := e.countProbeWrites(entries)
	activity := res.Started || len(entries) > 0 || len(trace.reads) > 0 || trace.execSeen
	if res.ExitCode != 0 && !activity {
		discard()
		return ProbeResult{}, newErr(CodeEnv, "err.probe.session_failed", map[string]any{
			"app": app, "code": res.ExitCode,
		}, nil)
	}
	if res.Started && !trace.execSeen && res.ExitCode != 0 && trace.readOK {
		// 会话起来了、但没观察到应用执行：按"应用未启动/立即退出"报，
		// 不能把 bwrap 启动成功当成应用已运行（§11.3）。
		discard()
		return ProbeResult{}, newErr(CodeEnv, "err.probe.session_not_started", map[string]any{
			"code": res.ExitCode,
		}, nil)
	}

	contaminated := e.ProbeContaminated(entries, pubBinds)
	if len(contaminated) > 0 {
		// 公共目录（或 AC_BASE/shared_writes）出现在 upper 里 = bind 没生效，
		// 用户文件可能已经落进去。结果整体作废，且**绝不删**。
		q, qerr := quarantine()
		if qerr != nil {
			return ProbeResult{}, qerr
		}
		return ProbeResult{Quarantined: q}, errProbe("err.probe.contaminated", map[string]any{
			"paths": contaminated, "quarantine": q,
		})
	}

	trace.finalize(writes, activity)
	cands, whiteouts, readsHidden, credWarns := e.Resolver.MergeUpperWithReads(
		entries, trace.reads, trace.flagsHidden, e.Base, app)
	cands = e.Resolver.AppendStaticCandidates(cands, opt.StaticPaths)

	// 非路径字段先装进 probe，再经 DeriveProfile 落成档案：Probe 不再直接
	// 调 ProfileFromCandidates（§8.1 要求凭据派生与告警走同一条路径）。
	evidence := e.keyringEvidence(opt.Command[0], cands, trace.reads, trace.keyringHits)
	probe := Profile{
		App:       app,
		Exec:      opt.Command[0],
		ExecArgs:  append([]string{}, opt.Command[1:]...),
		Ignored:   DefaultIgnored(),
		LockFiles: e.lockHostPaths(locks),
	}
	// 默认策略（方案 §6）：observed 且能力可用才默认开启；KeyringSet=false
	// 表示这是探测默认，合并时既有档案的显式选择优先。
	if on, _ := e.keyringDefault(evidence); on {
		probe.IsolateKeyring = true
	}
	probe.Unsupported = append(probe.Unsupported, probeUnsupported(e)...)
	sort.Slice(probe.Unsupported, func(i, j int) bool { return probe.Unsupported[i].Path < probe.Unsupported[j].Path })
	p := e.Resolver.DeriveProfile(probe, nil, "overlay", trace.readTrace(), cands)
	p.Warnings = append(p.Warnings, credWarns...)
	if _, warn := e.keyringDefault(evidence); warn != "" {
		p.Warnings = append(p.Warnings, warn)
	}

	discard()
	return ProbeResult{
		Profile:             p,
		Candidates:          cands,
		Whiteouts:           whiteouts,
		ExitCode:            res.ExitCode,
		Writes:              writes,
		HomeReads:           len(trace.reads),
		HomeReadsNoise:      trace.homeReadsNoise(),
		Started:             res.Started,
		ExecSeen:            trace.execSeen,
		Activity:            activity,
		ReadsDropped:        trace.stats.ReadsDropped,
		ParseErrors:         trace.stats.ParseErrors,
		SizeExceeded:        trace.stats.SizeExceeded,
		ReadsHidden:         readsHidden,
		ReadsWarns:          trace.warnsList(),
		ReadTrace:           trace.readTrace(),
		KeyringEvidence:     evidence,
		KeyringTraceDropped: trace.keyringDropped,
	}, nil
}

// countProbeWrites 是 §3.2 的 Writes 口径：upper 条目逐条 → 丢弃 HOME 外 →
// Classify → 剔除 cache 与 whiteout → 按路径去重。
func (e *Env) countProbeWrites(entries []UpperEntry) int {
	seen := map[string]bool{}
	n := 0
	for _, ent := range entries {
		if ent.Kind == EntryWhiteout {
			continue
		}
		abs := filepath.Join(e.Resolver.Home, filepath.FromSlash(ent.Path))
		if !IsUnder(e.Resolver.Home, abs) {
			continue
		}
		if cls, _, _, _ := e.Resolver.Classify(abs); cls == ClassCache {
			continue
		}
		key := realKey(abs)
		if seen[key] {
			continue
		}
		seen[key] = true
		n++
	}
	return n
}
