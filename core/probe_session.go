package core

import (
	"context"
	"fmt"
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
	ExitCode     int
	Signaled     bool
	Started      bool // cmd.Start 成功即 true，与读追踪能力无关
	Elapsed      time.Duration
	ManualFinish bool
	Unreaped     bool // caller must retain the mounted scene
	Process      SessionProcess
	Reaped       <-chan struct{}
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

// StartAwareSessionRunner calls started only after cmd.Start and group ownership
// checks succeed. An observer error aborts and reaps the session before return.
type StartAwareSessionRunner interface {
	RunWithStart(context.Context, []string, io.Writer, func(SessionProcess) error) (SessionResult, error)
}

type setsidRunner struct {
	grace, waitLimit time.Duration
	identify         func(int, int) (SessionProcess, error) // fault injection for ownership checks
}

func (r setsidRunner) Run(ctx context.Context, argv []string, stderr io.Writer) (SessionResult, error) {
	return r.RunWithStart(ctx, argv, stderr, nil)
}

func (r setsidRunner) RunWithStart(ctx context.Context, argv []string, stderr io.Writer, started func(SessionProcess) error) (SessionResult, error) {
	if len(argv) == 0 {
		return SessionResult{}, errInput("err.probe.no_command", nil)
	}
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}
	grace, limit := r.grace, r.waitLimit
	if grace == 0 {
		grace = 3 * time.Second
	}
	if limit == 0 {
		limit = 2 * time.Second
	}
	start := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.WaitDelay = limit // inherited stderr pipes must not make Wait unbounded
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return SessionResult{}, errEnv("err.probe.start_failed", map[string]any{"exec": argv[0]}, err)
	}
	done, reaped := make(chan error, 1), make(chan struct{})

	res := SessionResult{Started: true, Reaped: reaped}
	pgid, gerr := syscall.Getpgid(cmd.Process.Pid)
	identify := r.identify
	if identify == nil {
		identify = identifySessionProcess
	}
	process, perr := identify(cmd.Process.Pid, pgid)
	go func() { err := cmd.Wait(); close(reaped); done <- err }()
	// A very short process may already have exited. It cannot expose a Finish token.
	// Wait still owns reaping. Never signal a group whose ownership wasn't checked.
	if gerr != nil || pgid != cmd.Process.Pid || perr != nil {
		res.Unreaped = true // the direct child exiting does not prove descendants are gone
		if perr != nil {
			gerr = perr
		} else if gerr == nil {
			gerr = fmt.Errorf("process group %d does not match session leader %d", pgid, cmd.Process.Pid)
		}
		_ = cmd.Process.Kill()
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case <-reaped:
		case <-timer.C:
			res.Unreaped = true
		}
		res.Elapsed = time.Since(start)
		return res, errEnv("err.probe.setsid_failed", map[string]any{"exec": argv[0]}, gerr)
	}
	res.Process = process
	var observeErr error
	if started != nil {
		observeErr = started(process)
	}
	var waitErr error
	waited := false
	if observeErr == nil {
		select {
		case <-ctx.Done():
		case waitErr = <-done:
			waited = true
		}
	}
	// Stop all remaining descendants even when the direct process exited normally.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for !sessionGroupGone(pgid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !sessionGroupGone(pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	deadline = time.Now().Add(limit)
	for {
		if !waited {
			select {
			case waitErr = <-done:
				waited = true
			default:
			}
		}
		if waited && sessionGroupGone(pgid) {
			break
		}
		if time.Now().After(deadline) {
			res.Unreaped = true
			res.Elapsed = time.Since(start)
			return res, errEnv("err.probe.session_unreaped", nil, ctx.Err())
		}
		time.Sleep(20 * time.Millisecond)
	}
	res.Elapsed = time.Since(start)
	if observeErr != nil {
		return res, observeErr
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if ee, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signaled = true
			res.ExitCode = 128 + int(ws.Signal())
		}
		return res, nil
	}
	return res, waitErr
}
