package core

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// RunOptions 是 run 的全部可调项（§6 命令表）。
type RunOptions struct {
	Args          []string // `--` 之后原样透传给应用的参数
	Audit         bool     // 开运行期审计层（§6.5，本轮未落地，仅留接口）
	Wait          bool     // 第二个 run 是否等待（默认不等待，立刻退 7）
	PropagateExit bool     // 原样返回应用退出码（脚本场景）
	Stdout        io.Writer
	Stderr        io.Writer
}

// RunResult 描述一次运行的结束方式。应用码与信号分开存：
// 调用方不该自己减 128 去反推信号（§6.3）。
type RunResult struct {
	AppExitCode int
	Signal      syscall.Signal
	Signaled    bool
}

// Run 在分身环境里启动应用，并常驻为父进程持有进程组与 run.lock（§3 末段）。
//
// 不 exec bwrap 的原因：--die-with-parent 只保证直接子进程，应用自己
// fork+setsid 出来的进程会逃出这条链路。所以 run 自己 setsid 起 bwrap、
// 常驻等待，退出时对**整个进程组**发 SIGTERM，3 秒后 SIGKILL。
func (e *Env) Run(ctx context.Context, app, id string, opt RunOptions) (RunResult, error) {
	// root 会话下 --bind / / 是 rw，误操作能改宿主系统；而且 root 下 bwrap
	// 本来就能跑通，"验证通过"会把非特权路径的问题全藏起来（§3.1 末段）。
	if err := CheckNotRoot(); err != nil {
		return RunResult{}, err
	}
	// AC_BASE 是指向不存在目标的软链时，`--tmpfs Real(Base)` 会落到软链本身，
	// bwrap 报 "Can't mount on symlink destination"——错误看起来像 bwrap 坏了。
	// 在这里给出可操作的错误（用户去修 AC_BASE，而不是猜 bwrap）。
	if e.Base != "" {
		if st, lerr := os.Lstat(e.Base); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
			if _, rerr := filepath.EvalSymlinks(e.Base); rerr != nil {
				return RunResult{}, errEnv("err.env.base_symlink", map[string]any{"path": e.Base}, rerr)
			}
		}
	}
	// 信号处理器必须在任何耗时步骤之前注册：否则 stop 在启动窗口内送达
	// SIGTERM 会走默认处置，core.Run 的 defer 全不执行——run.pid 残留，
	// 私有 dbus-daemon 变孤儿（ADR-028 对抗复审 P2-1）。信号先缓存，
	// pgid 确定后再整组终止；启动前收到就标记 stopping，不再拉起应用。
	stop := make(chan os.Signal, 1)
	unnotify := notifyRunSignals(stop)
	defer unnotify()
	var stopping atomic.Bool
	pgidCh := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
			stopping.Store(true)
			select {
			case pgid := <-pgidCh:
				terminateGroup(pgid)
			case <-done:
			}
		case <-done:
		}
	}()
	defer close(done)

	in, err := e.GetInstance(app, id)
	if err != nil {
		return RunResult{}, err
	}
	// 运行态判定用 LOCK_NB 立刻返回：桌面图标被点两次是常态，
	// 阻塞的后果是"点了没反应"再叠加一个永不返回的进程（§4.3）。
	lock, err := e.HoldRunLock(app, id)
	if err != nil {
		if opt.Wait {
			for {
				select {
				case <-ctx.Done():
					// 用户取消不是失败：CLI 把它映射成 0（§11 sentinel）。
					return RunResult{}, ErrCanceled
				case <-time.After(500 * time.Millisecond):
				}
				if lock, err = e.HoldRunLock(app, id); err == nil {
					break
				}
			}
		} else {
			return RunResult{}, err
		}
	}
	defer lock.Close()
	// run.pid 供 stop 定位本进程（ADR-028）：defer 顺序是"先删记录、再放锁"，
	// 这样 stop 看到记录消失时，锁一定还没释放，不会把新的 run 当成旧的杀。
	// 运行态判定仍只看 run.lock（ADR-023）。
	instDir := e.InstancePath(app, id)
	if err := writeRunPID(instDir, os.Getpid()); err != nil {
		// 拿不到 stop 的目标记录不影响应用启动（磁盘满/实例目录只读时，
		// 让应用照常跑比拦下来更重要）；明确告知本次不支持停止。
		if opt.Stderr != nil {
			_, _ = io.WriteString(opt.Stderr,
				"appclone: 无法记录运行进程，本次运行将无法用 stop 停止（启动继续）\n")
		}
	} else {
		defer clearRunPID(instDir)
	}

	p, err := e.LoadProfile(app)
	if err != nil {
		return RunResult{}, err
	}
	// 先补建、再校验存在性：宿主的 `paths` 目标如果被删掉了（应用重装、用户清理），
	// 补一个空目录就能继续跑——那正是 `add` 建实例时做的事。反过来先校验的话，
	// 用户会得到一个"档案已失效"的死胡同，而唯一的出路是重新探测（得到同一份档案）。
	if _, err := e.prepareBindTargets(p, in); err != nil {
		return RunResult{}, err
	}
	// cwd 不存在时回退到有效 HOME：应用升级删了自己的工作目录是偶然事件，
	// 不该让整个分身起不来；bwrap 的 --chdir 到不存在目录会直接失败（§3.1 ⑫）。
	if p.Cwd != "" {
		if abs, cerr := e.Resolver.Expand(p.Cwd); cerr == nil {
			if st, serr := os.Stat(abs); serr != nil || !st.IsDir() {
				p.Cwd = e.Resolver.Home
				if opt.Stderr != nil {
					_, _ = io.WriteString(opt.Stderr, "appclone: 工作目录不存在，已回退到家目录\n")
				}
			}
		}
	}

	// 起 bwrap 之前把"源必须存在"的项全部确认一遍：否则 bwrap 会在容器里失败，
	// 那个失败看起来就像"应用自己退出了"，会被映射成 1，与 appclone 的码混在一起（§6.3）。
	if err := e.ValidateRunnable(p); err != nil {
		return RunResult{}, err
	}

	// 克隆起步（§8.1）：fork 实例在 bwrap 之前把主实例数据挂成只读 lower，
	// bind 源换成挂载点；run 结束（含失败/信号）由 defer 卸载。
	var forkSources map[string]string
	if in.Fork {
		var forkCleanup func()
		forkSources, forkCleanup = e.forkMounts(p, in, func() bool {
			return stopping.Load() || ctx.Err() != nil
		}, func(msg string) {
			if opt.Stderr != nil {
				_, _ = io.WriteString(opt.Stderr, "appclone: "+msg+"\n")
			}
		})
		defer forkCleanup()
	}

	// RuntimeDir 保持宿主值（它是 bind 的**目标**，命名空间里应用看到的路径）；
	// 私有 runtime 的**源**是 <inst>/runtime，由 BuildBwrapArgs 自己拼。
	// 曾经把这里改成实例目录，结果 socket 的源与目标都是实例内的空目录，
	// wayland/pulse 全丢，而 bind 源又指向不存在的 <inst>/root/runtime。
	o := e.BuildOptions()
	o.ForkSources = forkSources
	o.RuntimeSocks = FilterExisting(RuntimeSocketCandidates(o.RuntimeDir))

	// 审计层（§6.5）：overlay 罩住 HOME + 把公共目录/AC_BASE 绑回真实。
	// 这三件事必须发生在实例 bind 之前，所以它走 BuildOptions 而不是另拼 argv。
	var audit *AuditSession
	started := false
	if opt.Audit {
		sess, layers, aerr := e.beginAudit(in, p, o.KeyringTargets)
		if aerr != nil {
			return RunResult{}, aerr
		}
		audit = sess
		o.Audit = layers
		// 兜底：没走到 finish 就退出。**应用是否启动过决定能不能删写层**：
		// Start 之后写层里就可能已有用户文件，只能卸载并保留（下一次 verify
		// 会判 audit_incomplete）；Start 之前删是安全的。
		defer func() {
			if audit != nil {
				audit.abandon(started)
			}
		}()
	}
	// 审计写层根以 0700 建好（bwrap 对不存在的 --tmpfs 目标会自建默认权限
	// 的挂载点）。根永远遮蔽：不能按"当前是否存在"取快照——构造参数到 bwrap
	// 启动之间可能有并发审计刚建出根。
	if err := e.ensureAuditRoot(); err != nil {
		return RunResult{}, err
	}
	o.AuditRoot = e.auditRoot()

	// 私有 DBus 总线：必须先起 daemon（要 socket 存在才能 bind），
	// 且 --nofork 不能少——fork 模式下 --print-address 的父进程会先退出（§6.6）。
	// isolate_keyring 隐含 isolate_dbus（ADR-035）：keyring daemon 与 libsecret
	// 都走私有总线。
	dbusSess, berr := e.startRunBus(p, in, &o, opt.Stderr)
	if berr != nil {
		return RunResult{}, berr
	}
	if dbusSess != nil {
		defer dbusSess.Stop()
	}

	// 独立 keyring daemon（ADR-035）：必须在私有总线就绪之后、应用启动之前。
	// 退出顺序由 defer LIFO 写死：keyring 先停、dbus 后停、socket 最后删。
	var keyringSess *keyringSession
	if p.IsolateKeyring {
		if err := e.checkKeyringCapability(); err != nil {
			return RunResult{}, err
		}
		if err := e.EnsureKeyring(in); err != nil {
			return RunResult{}, err
		}
		pw, err := readKeyringPassword(in)
		if err != nil {
			return RunResult{}, err
		}
		cap := e.keyringCaps()
		sess, kerr := startKeyringDaemon(cap.Daemon, cap.DbusSend, o.DBusAddress, keyringDir(in), pw)
		if kerr != nil {
			return RunResult{}, kerr
		}
		keyringSess = sess
		defer keyringSess.Stop()
	}

	// cwd 落在 AC_BASE 里（含软链解析）时回退 HOME：沙箱里 Base 已被 tmpfs
	// 遮蔽，chdir 必失败。BuildBwrapArgs 也做词法兜底，doctor 与 run 同判据。
	if o.BaseReal != "" && p.Cwd != "" {
		if abs, cerr := o.expand(p.Cwd); cerr == nil {
			if c := Real(abs); c == o.BaseReal || IsUnder(o.BaseReal, c) {
				p.Cwd = o.Home
			}
		}
	}

	argv, err := BwrapCommand(p, in, o)
	if err != nil {
		return RunResult{}, err
	}
	argv = append(argv, opt.Args...)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = opt.Stdout, opt.Stderr
	cmd.Stdin = nil
	// setsid：新会话 + 新进程组，进程组 kill 才有对象可杀。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// 启动前收到过停止信号：不为一次注定要杀的运行把应用拉起来。
	if stopping.Load() {
		return RunResult{}, ErrCanceled
	}
	if err := cmd.Start(); err != nil {
		return RunResult{}, errEnv("err.run.start_failed", map[string]any{"exec": p.Exec}, err)
	}
	started = true
	pgid := cmd.Process.Pid
	pgidCh <- pgid // 早到的 SIGTERM 在这里拿到目标，整组终止

	// E6：daemon 意外退出时终止应用。应用先退出或用户 stop 时不判 3——
	// watcher 先看 appDone/stopping，再看 Done，避免把正常停止误判成故障。
	appDone := make(chan struct{})
	// appExited 在 Wait 返回后、close(appDone) 前同步置位：watcher 用它区分
	// "应用先退出"与"daemon 先死、应用随后也退出"——后者仍是故障（退出 3），
	// 不能因为应用在 100ms 确认窗口内退出就放过。
	var appExited atomic.Bool
	var daemonErr atomic.Value
	watchDaemon := func(name string, done <-chan struct{}) {
		select {
		case <-done:
		case <-appDone:
			return
		}
		if appExited.Load() {
			return // 应用先退出：契约规定不算失败
		}
		if stopping.Load() {
			return
		}
		// 用户按 Ctrl-C 时 SIGINT 同时送达 daemon 与本进程，信号 goroutine
		// 可能还没被调度。给一个很短的确认窗口再判定，避免把用户主动停止
		// 记成"daemon 意外死亡"（退出 3）。应用在窗口内退出**不**改变结论：
		// daemon 先死就是故障。
		select {
		case <-appDone:
		case <-time.After(100 * time.Millisecond):
		}
		if stopping.Load() {
			return
		}
		daemonErr.Store(daemonDiedKeys[name])
		terminateGroup(pgid)
	}
	if keyringSess != nil {
		go watchDaemon("keyring_daemon", keyringSess.Done())
	}
	if dbusSess != nil {
		go watchDaemon("dbus_daemon", dbusSess.Done())
	}

	waitErr := cmd.Wait()
	appExited.Store(true)
	close(appDone)

	// 应用退出后，它 fork 出去的子进程仍可能活着：补一次组终止。
	terminateGroup(pgid)
	// 收掉早到信号仍留在缓冲里的目标：否则 Run 返回后信号 goroutine 才取到
	// pgid，会对一个已经收尸的进程组再发一遍 SIGTERM——PID 复用时就是误杀。
	select {
	case <-pgidCh:
	default:
	}

	res := RunResult{}
	// 审计会话在这里落账：写 run.toml、保留 upper（证据交给 verify 消费）。
	// 必须在应用退出之后写——中途崩掉就不该留下"正常跑完"的凭证。
	finishAudit := func(code int) error {
		if audit == nil {
			return nil
		}
		ferr := audit.finish(code)
		audit = nil // 置空让 defer 的 abort 不再执行
		return ferr
	}
	// daemon 死亡优先于应用退出码：应用是被我们 terminateGroup 掉的，
	// 它的 SIGTERM(143) 没有解释力，必须返回环境错误（退出码 3）。
	if key, _ := daemonErr.Load().(string); key != "" {
		if ferr := finishAudit(3); ferr != nil {
			return res, ferr
		}
		return res, errEnv(key, nil, nil)
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Signaled = true
				res.Signal = ws.Signal()
				if ferr := finishAudit(128 + int(ws.Signal())); ferr != nil {
					return res, ferr
				}
				return res, nil
			}
			res.AppExitCode = ee.ExitCode()
			if ferr := finishAudit(res.AppExitCode); ferr != nil {
				return res, ferr
			}
			return res, nil
		}
		return res, errEnv("err.run.start_failed", map[string]any{"exec": p.Exec}, waitErr)
	}
	if ferr := finishAudit(0); ferr != nil {
		return res, ferr
	}
	_ = e.TouchLastUsed(app, id)
	return res, nil
}

// startRunBus 按档案决定是否起私有总线，并处理两条不同的失败方向：
//   - keyring 开启时**禁止降级**（P1）：回落到宿主总线等于把宿主 keyring
//     交给应用，独立密钥环从根上失效；
//   - 仅 dbus 时降级为共享宿主总线，但必须明确告知（静默共享会让用户以为
//     "应用不支持分身"）。
func (e *Env) startRunBus(p Profile, in Instance, o *BuildOptions, warn io.Writer) (*dbusSession, error) {
	if !p.IsolateDBus && !p.IsolateKeyring {
		return nil, nil
	}
	sess, derr := e.startSessionBus(filepath.Join(in.Dir(), "dbus"))
	if derr != nil {
		if p.IsolateKeyring {
			return nil, newErr(CodeEnv, "err.env.keyring_requires_bus", nil, derr)
		}
		if warn != nil {
			_, _ = io.WriteString(warn, "appclone: 未启动私有 DBus 总线，将与主实例共用会话总线（可能无法双开）\n")
		}
		return nil, nil
	}
	o.DBusAddress = sess.Addr()
	// bind 源用会话给出的真实 socket：路径过长时它不在 <inst>/dbus 下。
	o.BusSock = sess.Sock()
	// 应用侧地址是命名空间里的路径：AC_BASE 被 tmpfs 遮蔽后，宿主
	// socket 路径在沙箱里不存在。
	o.DBusAppAddress = AppDBusAddress(o.RuntimeDir)
	return sess, nil
}

// daemonDiedKeys 把 watcher 的名字映射到错误键。用映射而不是拼字符串：
// i18n 门禁按字面量扫描错误键，动态拼接会漏检（也漏掉真正的缺文案）。
var daemonDiedKeys = map[string]string{
	"keyring_daemon": "err.env.keyring_daemon_died",
	"dbus_daemon":    "err.env.dbus_daemon_died",
}

// 私有总线生命周期见 core/dbus_session.go（startSessionBus / dbusSession）：
// run 与 probe 共用同一份实现，且配置里没有 servicedir（禁服务激活）。
