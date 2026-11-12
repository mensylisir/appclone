package core

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// SessionRunner 跑"被探测的那一次启动"。返回的 error 只表示"没能跑起来"；
// 应用自己非零退出属于正常结果（AppExitCode）。
type SessionRunner interface {
	Run(ctx context.Context, argv []string, stderr io.Writer) (SessionResult, error)
}

// SessionResult 是一次会话的结束方式。
type SessionResult struct {
	ExitCode int
	Signaled bool
	Started  bool // cmd.Start 成功即 true，与读追踪能力无关
	Elapsed  time.Duration
}

// probeRuntimeSocks 把宿主 socket 重新指向私有 runtime 里的同名路径。
//
// 两件事都要做对：① 目标必须是**私有 runtime 里的路径**，只原地 bind 等于没
// bind（XDG_RUNTIME_DIR 已经指向私有目录）；② 宿主会话总线 `bus` 必须排除，
// 否则应用会绕过 --setenv DBUS_SESSION_BUS_ADDRESS 直接连回主实例，单例转发照旧。
func probeRuntimeSocks(hostRuntime, probeRuntime string) []BindPair {
	socks := FilterExisting(RuntimeSocketCandidates(hostRuntime))
	out := socks[:0]
	// 只排除**会话总线本身**（<runtime>/bus）。按 basename 过滤会把
	// at-spi/bus 一起丢掉：探测期应用的无障碍总线缺失，应用可能因此走不同
	// 代码路径，读证据的保真度下降。
	busSock := filepath.Join(hostRuntime, "bus")
	for _, s := range socks {
		if s.Src == busSock {
			continue
		}
		rel, ok := relUnder(hostRuntime, s.Src)
		if !ok {
			continue
		}
		out = append(out, BindPair{Src: s.Src, Dst: filepath.Join(probeRuntime, rel)})
	}
	return out
}

// 探测期的私有总线走 core/dbus_session.go 的 startSessionBus：与运行期
// 同一份实现（同样的最小 busconfig、同样的生命周期），避免两份实现分叉后
// 探测能跑、运行跑不起来。

// setsidRunner 起独立会话并跑会话命令；退出时对**整个进程组**发信号。
//
// `--die-with-parent` 只管直接子进程：应用自己 fork+setsid 出去的后代不继承
// death signal（bwrap 0.13.1 实测），所以主保证是 pgid kill，flag 只是兜底。
type setsidRunner struct{}

func (setsidRunner) Run(ctx context.Context, argv []string, stderr io.Writer) (SessionResult, error) {
	if len(argv) == 0 {
		return SessionResult{}, errInput("err.probe.no_command", nil)
	}
	if err := ctx.Err(); err != nil {
		// 准备阶段（起 dbus、等锁）期间被取消：不要再把应用拉起来闪一下，
		// 那既产生意外写入，也要多等一次 kill。
		return SessionResult{}, err
	}
	start := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return SessionResult{}, errEnv("err.probe.start_failed", map[string]any{"exec": argv[0]}, err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		// setsid 失败或读早了会拿到调用者自己的 pgid，`kill(-pgid)` 会把终端
		// 整个会话杀掉（灾难级误杀）。断言不过只杀单进程。
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return SessionResult{}, errEnv("err.probe.setsid_failed", map[string]any{"exec": argv[0]}, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		terminateGroup(cmd.Process.Pid)
		<-done
		return SessionResult{Started: true, Elapsed: time.Since(start)}, ctx.Err()
	case err := <-done:
		terminateGroup(cmd.Process.Pid) // 收掉应用 fork 出去的后代
		res := SessionResult{Started: true, Elapsed: time.Since(start)}
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Signaled = true
				res.ExitCode = 128 + int(ws.Signal())
				return res, nil
			}
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		if err != nil {
			return res, err
		}
		return res, nil
	}
}
