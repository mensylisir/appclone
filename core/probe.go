package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProbeOptions 是探测的全部可调项。
type ProbeOptions struct {
	Command  []string  // 应用命令（Exec + 参数）
	LockFile []string  // L0 预填的锁文件（可空）
	Stderr   io.Writer // 会话的 stderr（GUI 无 stderr，可为 nil）
	Mounter  OverlayMounter
	Runner   SessionRunner
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
	return e.probeSession(ctx, app, opt, mounter, runner)
}

// probeRoundFailed 把"某一轮没跑起来"翻译成退出码。抽出来是因为它同时要
// 卸载、清理/保留现场，塞在循环里会让主流程读不出顺序。
func (e *Env) probeRoundFailed(app string, round, rounds int, runErr error, discard func()) (ProbeResult, error) {
	discard()
	if errors.Is(runErr, context.Canceled) {
		return ProbeResult{}, ErrCanceled
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		return ProbeResult{}, newErr(CodeProbe, "err.probe.timeout", map[string]any{
			"app": app, "round": round + 1, "rounds": rounds,
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
	removed := false
	discard := func() {
		if !removed {
			removed = true
			os.RemoveAll(work)
		}
	}
	quarantine := func() (string, error) {
		q := fmt.Sprintf("/var/tmp/appclone-quarantine-%d-%s", e.UID,
			time.Now().UTC().Format("20060102T150405")+"-"+randSuffix())
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
		discard()
		return ProbeResult{}, err
	}
	// 卸载顺序与"先杀进程再卸载"（§3.2.3）：这里注册的 defer 是兜底，
	// 正常路径在下面显式调用并检查错误。
	mounted := true
	defer func() {
		if mounted {
			_ = unmount()
		}
	}()

	bus, stopBus := e.startProbeBus(filepath.Join(work, "dbus"))
	if stopBus != nil {
		defer stopBus()
	}
	locks, lockCleanup, err := e.probeLockBinds(work, opt.LockFile)
	if err != nil {
		_ = unmount()
		discard()
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
		_ = unmount()
		discard()
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
			// 第一轮就取消/失败：没有可信证据，按原来的分派处理。
			return e.probeRoundFailed(app, round, rounds, runErr, discard)
		}
		if r.ExitCode == probeGuardFailed {
			// 正向断言失败：公共目录仍在 overlay 里。应用还没跑，删写层安全。
			discard()
			return ProbeResult{}, errProbe("err.probe.bind_verify_failed", map[string]any{
				"path": strings.Join(publicTargets(pubBinds), "、"),
			})
		}
		// 后续轮次沿用最后一轮的结果；前面轮次只要没报错就继续——
		// "第二轮才写 token" 正是要覆盖的情形。
		res = r
	}

	// 会话结束 → 先停 dbus、再卸载（顺序反了会 EBUSY）。
	if stopBus != nil {
		stopBus()
	}
	unmountErr := unmount()
	mounted = false
	if unmountErr != nil {
		// 挂载点还在：这时删工作目录会遍历用户家目录视图并制造 whiteout，
		// 所以**不删**，留着并报环境错误。
		return ProbeResult{}, unmountErr
	}
	// 收集：先读，再判，最后才谈删（§3.2.3 末条）。
	entries, present, readErr := readUpperTree(upper)
	if readErr != nil {
		discard()
		return ProbeResult{}, readErr
	}
	if !present {
		discard()
		return ProbeResult{}, errEnv("err.probe.no_upper", map[string]any{"path": upper}, nil)
	}
	// 会话内的正向断言失败：公共目录仍落在 overlay 里（bind 没生效）。
	// 这条比反向断言更早、也更硬——此时应用还没跑，所以可以安全删掉写层。
	if res.ExitCode == probeGuardFailed {
		discard()
		return ProbeResult{}, errProbe("err.probe.bind_verify_failed", map[string]any{
			"path": strings.Join(publicTargets(pubBinds), "、"),
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
			"paths": strings.Join(contaminated, "、"), "quarantine": q,
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
