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
	if os.Geteuid() == 0 {
		return RunResult{}, errEnv("err.env.root", nil, nil)
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
					return RunResult{}, ctx.Err()
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
	if err := e.ValidateRunnable(p); err != nil {
		return RunResult{}, err
	}
	if _, err := e.prepareBindTargets(p, in); err != nil {
		return RunResult{}, err
	}

	o := e.BuildOptions()
	o.RuntimeDir = e.runtimeDir(in)
	o.RuntimeSocks = FilterExisting(RuntimeSocketCandidates(e.Resolver.XDG["RUNTIME_DIR"]))

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
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Signaled = true
				res.Signal = ws.Signal()
				return res, nil
			}
			res.AppExitCode = ee.ExitCode()
			return res, nil
		}
		return res, errEnv("err.run.start_failed", map[string]any{"exec": p.Exec}, waitErr)
	}
	_ = e.TouchLastUsed(app, id)
	return res, nil
}

// runtimeDir 返回该实例的私有 XDG_RUNTIME_DIR。
// 即使档案没开 isolate_runtime 也返回实例内路径：调用方只在需要时才用它。
func (e *Env) runtimeDir(in Instance) string {
	return filepath.Join(in.Dir(), "runtime")
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
