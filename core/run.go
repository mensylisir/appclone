package core

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// RuntimeDir 保持宿主值（它是 bind 的**目标**，命名空间里应用看到的路径）；
	// 私有 runtime 的**源**是 <inst>/runtime，由 BuildBwrapArgs 自己拼。
	// 曾经把这里改成实例目录，结果 socket 的源与目标都是实例内的空目录，
	// wayland/pulse 全丢，而 bind 源又指向不存在的 <inst>/root/runtime。
	o := e.BuildOptions()
	o.RuntimeSocks = FilterExisting(RuntimeSocketCandidates(o.RuntimeDir))

	// 审计层（§6.5）：overlay 罩住 HOME + 把公共目录/AC_BASE 绑回真实。
	// 这三件事必须发生在实例 bind 之前，所以它走 BuildOptions 而不是另拼 argv。
	var audit *AuditSession
	if opt.Audit {
		sess, layers, aerr := e.beginAudit(in)
		if aerr != nil {
			return RunResult{}, aerr
		}
		audit = sess
		o.Audit = layers
		// 兜底：没走到 finish 就退出（启动失败、panic）时不清写层会更糟——
		// 但那时应用没跑，写层里不可能有用户文件，所以 abort 是安全的。
		defer func() {
			if audit != nil {
				audit.abort()
			}
		}()
	}

	// 私有 DBus 总线：必须先起 daemon（要 socket 存在才能 bind），
	// 且 --nofork 不能少——fork 模式下 --print-address 的父进程会先退出（§6.6）。
	var stopDBus func()
	if p.IsolateDBus {
		addr, stop, derr := e.startPrivateDBus(in)
		if derr != nil {
			// 降级为共享总线，但必须明确告知：静默共享会让用户以为"应用不支持分身"。
			if opt.Stderr != nil {
				_, _ = io.WriteString(opt.Stderr, "appclone: 未启动私有 DBus 总线，将与主实例共用会话总线（可能无法双开）\n")
			}
		} else {
			o.DBusAddress = addr
			stopDBus = stop
		}
	}
	if stopDBus != nil {
		defer stopDBus()
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
	if err := cmd.Start(); err != nil {
		return RunResult{}, errEnv("err.run.start_failed", map[string]any{"exec": p.Exec}, err)
	}
	pgid := cmd.Process.Pid

	// 信号 → 进程组。run 自身被杀时也要把应用带走，否则用户关掉终端
	// 会留下一个看不见的实例占着锁，下次 run 报"已在运行"。
	stop := make(chan os.Signal, 1)
	notifyRunSignals(stop)
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
			terminateGroup(pgid)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)

	// 应用退出后，它 fork 出去的子进程仍可能活着：补一次组终止。
	terminateGroup(pgid)

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

// startPrivateDBus 起一个与 bwrap 同生共死的会话总线（§6.6）。
func (e *Env) startPrivateDBus(in Instance) (addr string, stop func(), err error) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		return "", nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	sock := filepath.Join(in.Dir(), "dbus", "bus")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return "", nil, errEnv("err.env.mkdir", map[string]any{"path": sock}, err)
	}
	// 上一轮被 kill 留下的 socket 文件会让 dbus-daemon 直接失败。
	_ = os.Remove(sock)
	cmd := exec.Command(daemon, "--session", "--nofork", "--address=unix:path="+sock, "--print-address")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	if err := cmd.Start(); err != nil {
		return "", nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) == "" {
		_ = cmd.Process.Kill()
		return "", nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	stopFn := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.Remove(sock)
	}
	// 地址用 --print-address 的原文，不自己拼 unix:path=（地址格式是 dbus 的实现细节）。
	return strings.TrimSpace(line), stopFn, nil
}
