package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func seedInstance(t *testing.T, e *Env) string {
	t.Helper()
	app, p := seedApp(t, e)
	if _, err := e.CreateInstance(context.Background(), app, p, "main"); err != nil {
		t.Fatal(err)
	}
	return e.InstancePath(app, "main")
}

// run.pid 只记录"停止信号该发给谁"；没有锁时它必须被当作 stale 清掉，
// 绝不能让 stop 对一条残留记录动手（ADR-023/ADR-028）。
func TestStopRunNotRunningClearsStalePID(t *testing.T) {
	e := testEnv(t)
	instDir := seedInstance(t, e)
	if err := writeRunPID(instDir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRunPID(instDir); err != nil {
		t.Fatalf("run.pid 应可读：%v", err)
	}

	err := e.StopRun(context.Background(), "testapp", "main")
	if ExitCode(err) != CodeState {
		t.Fatalf("没有在跑时应报状态错误（7），得到 %v", err)
	}
	if _, serr := os.Stat(filepath.Join(instDir, "run.pid")); !os.IsNotExist(serr) {
		t.Fatalf("stale run.pid 应被清掉，stat=%v", serr)
	}
}

// 停止链路：锁被占 + pid/starttime 对得上 → SIGTERM 发给记录的进程；
// 记录消失（run 的 defer 顺序）即视为停止完成。
func TestStopRunSignalsRecordedProcess(t *testing.T) {
	e := testEnv(t)
	instDir := seedInstance(t, e)

	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := sleeper.Process.Pid

	lock, err := e.HoldRunLock("testapp", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunPID(instDir, pid); err != nil {
		t.Fatal(err)
	}
	// 模拟 run 的收尾 defer：先删 pid 记录，再放锁。
	go func() {
		_ = sleeper.Wait()
		clearRunPID(instDir)
		_ = lock.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := e.StopRun(ctx, "testapp", "main"); err != nil {
		t.Fatalf("停止应成功：%v", err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("目标进程应已退出，kill(0)=%v", err)
	}
}

// 锁被占但没有 pid 记录（run 正在启动、旧版本启动）：无法确认目标，
// 必须拒绝而不是盲发信号。
func TestStopRunLockHeldWithoutPID(t *testing.T) {
	e := testEnv(t)
	seedInstance(t, e)

	lock, err := e.HoldRunLock("testapp", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	err = e.StopRun(context.Background(), "testapp", "main")
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.instance.stop_no_pid" {
		t.Fatalf("没有 pid 记录时必须拒绝停止，得到 %v", err)
	}
}

// runner 在注册信号处理器前被默认 SIGTERM 打死时，defer 不执行：pid 记录
// 残留，但锁已释放。stop 必须把"锁拿得到"当作已停止，并顺手清掉残留。
func TestStopRunDetectsExitWithoutCleanup(t *testing.T) {
	e := testEnv(t)
	instDir := seedInstance(t, e)

	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	lock, err := e.HoldRunLock("testapp", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunPID(instDir, sleeper.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// 模拟"进程死了但收尾没跑"：只放锁，不删记录。
	go func() {
		_ = sleeper.Wait()
		_ = lock.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := e.StopRun(ctx, "testapp", "main"); err != nil {
		t.Fatalf("锁已释放时应判定为已停止：%v", err)
	}
	if _, serr := os.Stat(filepath.Join(instDir, "run.pid")); !os.IsNotExist(serr) {
		t.Fatalf("残留 run.pid 应在持锁状态下被清掉，stat=%v", serr)
	}
}

// PID 复用防线：pid 存在但启动时刻对不上时，绝不能发信号。
func TestStopRunRejectsStartTimeMismatch(t *testing.T) {
	e := testEnv(t)
	instDir := seedInstance(t, e)

	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleeper.Process.Kill()
		_, _ = sleeper.Process.Wait()
	}()

	lock, err := e.HoldRunLock("testapp", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// 故意写一个错误的启动时刻：starttime=0 不会与真实值相同。
	if err := os.WriteFile(runPIDPath(instDir),
		[]byte(fmt.Sprintf("%d 0\n", sleeper.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	err = e.StopRun(context.Background(), "testapp", "main")
	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.instance.stop_no_pid" {
		t.Fatalf("启动时刻不匹配时必须拒绝，得到 %v", err)
	}
	if err := syscall.Kill(sleeper.Process.Pid, 0); err != nil {
		t.Fatalf("拒绝时不许误杀：%v", err)
	}
}
