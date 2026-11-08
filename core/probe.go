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
}

// ProbeResult 是一次探测的产出。
type ProbeResult struct {
	Profile     Profile     `json:"-"`
	Candidates  []Candidate `json:"candidates"`
	Whiteouts   []string    `json:"whiteouts"`
	Quarantined string      `json:"quarantined,omitempty"` // 非空 = 保留现场的目录
	ExitCode    int         `json:"session_exit_code"`
}

const probeTimeoutDefault = 30 * time.Minute

// reportProbeProgress 把"到目前为止合并后的条目数"交给调用方。
// 用候选数而不是原始 upper 条目数：白化文件与 XDG 根本身不是"会隔离的写入"，
// 拿它们计数会让界面数字比最终确认页多出一截。
func reportProbeProgress(opt ProbeOptions, e *Env, round, rounds int, entries []UpperEntry) {
	if opt.Progress == nil {
		return
	}
	cands, _ := e.Resolver.MergeUpper(entries)
	opt.Progress(ProbeProgress{
		Stage:   "round_done",
		Round:   round + 1,
		Rounds:  rounds,
		Entries: len(cands),
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
	if err := ValidateAppKey(app); err != nil {
		return ProbeResult{}, err
	}
	if len(opt.Command) == 0 {
		return ProbeResult{}, errInput("err.probe.no_command", map[string]any{"app": app})
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
	// 降级判据（§3.4）：overlay 挂不起来但有 strace，就走 strace；
	// 两者都没有才报"环境给不出结论"。降级必须显式告知：strace 只能看到
	// "打开过什么"，拿不到 overlay 那种零噪声的写入集。
	if err := mounter.Available(); err != nil {
		if _, serr := exec.LookPath("strace"); serr != nil {
			return ProbeResult{}, err
		}
		if opt.Stderr != nil {
			fmt.Fprintln(opt.Stderr, "appclone: 没有可用的 fuse-overlayfs，改用 strace 跟踪（结果置信度较低，且需要多开几次应用）")
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
	defer os.RemoveAll(work)
	runtimeDir := filepath.Join(work, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return ProbeResult{}, errEnv("err.env.mkdir", map[string]any{"path": runtimeDir}, err)
	}
	bus, stopBus := e.startProbeBus(filepath.Join(work, "dbus"))
	if stopBus != nil {
		defer stopBus()
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
	var entries []UpperEntry
	var last SessionResult
	for round := 0; round < rounds; round++ {
		logPath := filepath.Join(work, fmt.Sprintf("round-%d.log", round+1))
		inner, aerr := ProbeBwrapArgs(ProbeSpec{
			Home:        e.Resolver.Home,
			Base:        e.Base,
			RuntimeDir:  runtimeDir,
			RuntimeSock: probeRuntimeSocks(e.Resolver.XDG["RUNTIME_DIR"], runtimeDir),
			DBusAddress: bus,
			SharedWrite: e.probeSharedWrites(),
			LockBinds:   locks,
			Command:     opt.Command,
			NoOverlay:   true,
		})
		if aerr != nil {
			return ProbeResult{}, aerr
		}
		if opt.Stderr != nil && rounds > 1 {
			fmt.Fprintf(opt.Stderr, "appclone: 第 %d/%d 轮 —— 请打开应用用一下，然后把它关掉\n", round+1, rounds)
		}
		sctx, cancel := context.WithTimeout(ctx, timeout)
		res, runErr := runner.Run(sctx, StraceCommand(logPath, inner), opt.Stderr)
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
		if f, ferr := os.Open(logPath); ferr == nil {
			entries = append(entries, ParseStrace(f, e.Resolver.Home)...)
			f.Close()
		}
		reportProbeProgress(opt, e, round, rounds, entries)
	}
	cands, whiteouts := e.Resolver.MergeUpper(entries)
	p := e.Resolver.ProfileFromCandidates(app, "strace", cands)
	p.Exec = opt.Command[0]
	p.ExecArgs = append([]string{}, opt.Command[1:]...)
	p.Ignored = DefaultIgnored()
	p.LockFiles = e.lockHostPaths(locks)
	p.Unsupported = append(p.Unsupported, probeUnsupported(e)...)
	sort.Slice(p.Unsupported, func(i, j int) bool { return p.Unsupported[i].Path < p.Unsupported[j].Path })
	return ProbeResult{Profile: p, Candidates: cands, Whiteouts: whiteouts, ExitCode: last.ExitCode}, nil
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
	discard := func() {
		if !removed && !isMounted {
			removed = true
			os.RemoveAll(work)
		}
	}
	quarantine := func() (string, error) {
		// 与 discard 对称的硬护栏：挂载还在时改名会把挂载点一起搬走，
		// 退出时卸载目标就不对了。当前调用点都在卸载之后，这里防未来。
		if isMounted {
			return work, errEnv("err.probe.quarantine_mounted", map[string]any{"path": work}, nil)
		}
		q := filepath.Join(e.tmpRoot(), fmt.Sprintf("appclone-quarantine-%d-%s", e.UID,
			time.Now().UTC().Format("20060102T150405")+"-"+randSuffix()))
		if err := os.Rename(work, q); err != nil {
			// **绝不回退到 RemoveAll**：这里面可能就是用户刚存的文件（§3.2.1）。
			// 留着、把真实路径告诉用户，让人自己决定。
			return work, errEnv("err.probe.quarantine_failed", map[string]any{"path": work}, err)
		}
		removed = true
		return q, nil
	}

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

	bus, stopBus := e.startProbeBus(filepath.Join(work, "dbus"))
	if stopBus != nil {
		defer stopBus()
	}
	// teardown 是异常路径唯一允许的收尾顺序：停 dbus → 卸载 → 处置工作目录
	// （§3.2.3「先杀进程再卸载」）。preserve 表示"应用可能已经跑过、不能
	// 直接删"：卸载后先做一次污染检查——公共目录进了 upper 才 quarantine，
	// 只有应用自身配置/缓存写入时和成功路径一样删掉（否则每次取消都在
	// /var/tmp 攒一份用户不知道的垃圾）。读不出来时按"可能有内容"保留。
	teardown := func(preserve bool) (string, error) {
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
	defer lockCleanup()

	spec := ProbeSpec{
		Home:        e.Resolver.Home,
		Base:        e.Base,
		Merged:      merged,
		RuntimeDir:  filepath.Join(work, "runtime"),
		RuntimeSock: probeRuntimeSocks(e.Resolver.XDG["RUNTIME_DIR"], filepath.Join(work, "runtime")),
		DBusAddress: bus,
		PublicBinds: pubBinds,
		SharedWrite: e.probeSharedWrites(),
		LockBinds:   locks,
		Env:         probeGuardEnv(pubBinds),
		Command:     opt.Command,
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
		r, runErr := runner.Run(sctx, argv, opt.Stderr)
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
		// 中途读一次 upper 只为进度展示；读失败不影响主流程（结束时还有一次
		// 权威读取，错误会在那里上报）。
		if ents, present, rerr := readUpperTree(upper); rerr == nil && present {
			reportProbeProgress(opt, e, round, rounds, ents)
		}
	}

	// 会话结束 → 先停 dbus、再卸载（顺序反了会 EBUSY）。
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
	entries, present, readErr := readUpperTree(upper)
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
	if res.ExitCode != 0 && len(entries) == 0 {
		discard()
		return ProbeResult{}, newErr(CodeEnv, "err.probe.session_failed", map[string]any{
			"app": app, "code": res.ExitCode,
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

	cands, whiteouts := e.Resolver.MergeUpper(entries)
	p := e.Resolver.ProfileFromCandidates(app, "overlay", cands)
	// 档案必须自带启动命令：ValidateProfile 会因为它为空直接拒（§5），
	// 而探测是唯一知道"这次跑的是什么"的地方。
	p.Exec = opt.Command[0]
	p.ExecArgs = append([]string{}, opt.Command[1:]...)
	p.Ignored = DefaultIgnored()
	p.LockFiles = e.lockHostPaths(locks)
	p.Unsupported = append(p.Unsupported, probeUnsupported(e)...)
	sort.Slice(p.Unsupported, func(i, j int) bool { return p.Unsupported[i].Path < p.Unsupported[j].Path })

	discard()
	return ProbeResult{
		Profile:    p,
		Candidates: cands,
		Whiteouts:  whiteouts,
		ExitCode:   res.ExitCode,
	}, nil
}
