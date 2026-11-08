package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// run.pid 是**停止信号的目标记录**，不是运行态判据（ADR-023/ADR-028）。
//
// 运行态永远由 run.lock 的 flock 回答；PID 文件一旦参与判定，崩溃后的残留
// 就会变成假阳性。这里只解决"该把 SIGTERM 发给谁"：内容写成 "pid starttime"，
// starttime 取 /proc/<pid>/stat 的字段 22，发送前必须与当前值一致，防止
// PID 复用后误杀无关进程。
func runPIDPath(instDir string) string { return filepath.Join(instDir, "run.pid") }

func writeRunPID(instDir string, pid int) error {
	start, err := procStartTime(pid)
	if err != nil {
		return errEnv("err.env.write_pid", map[string]any{"path": runPIDPath(instDir)}, err)
	}
	body := fmt.Sprintf("%d %d\n", pid, start)
	path := runPIDPath(instDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		_ = os.Remove(tmp)
		return errEnv("err.env.write_pid", map[string]any{"path": path}, err)
	}
	// rename 原子替换：stop 可能正好在读，不能让它看到半条记录。
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return errEnv("err.env.write_pid", map[string]any{"path": path}, err)
	}
	return nil
}

// clearRunPID 只删自己的记录；失败不改变 run 的退出语义（最坏情况是
// stop 读到一条 stale 记录，届时会被启动时刻校验挡下）。
func clearRunPID(instDir string) { _ = os.Remove(runPIDPath(instDir)) }

func readRunPID(instDir string) (int, uint64, error) {
	b, err := os.ReadFile(runPIDPath(instDir))
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("core: malformed run.pid")
	}
	pid, err := strconv.Atoi(f[0])
	if err != nil || pid <= 0 {
		return 0, 0, fmt.Errorf("core: malformed run.pid pid")
	}
	start, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("core: malformed run.pid starttime")
	}
	return pid, start, nil
}

// procStartTime 读进程启动时刻（/proc/<pid>/stat 字段 22，单位 jiffies）。
// comm 字段可能含空格与括号，所以从**最后一个** ')' 之后再切字段。
func procStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0, fmt.Errorf("core: malformed /proc stat")
	}
	fields := bytes.Fields(b[i+1:])
	// fields[0] 是字段 3（state）；starttime 是字段 22。
	if len(fields) < 20 {
		return 0, fmt.Errorf("core: short /proc stat")
	}
	return strconv.ParseUint(string(fields[19]), 10, 64)
}

// StopRun 停止一个正在运行的分身。
//
// 顺序是刻意的：先用 run.lock 证明"确实在跑"，再读 run.pid，再校验启动时刻，
// 最后才发信号——任何一步对不上都拒绝动手，绝不盲杀 PID。信号只发给 run
// 进程：它收到 SIGTERM 后对应用进程组 SIGTERM→3s→SIGKILL（ADR-020），
// 自己不直接碰应用进程。
func (e *Env) StopRun(ctx context.Context, app, id string) error {
	if _, err := e.GetInstance(app, id); err != nil {
		return err
	}
	instDir := e.InstancePath(app, id)
	lockPath := filepath.Join(instDir, "run.lock")

	// 与 IsRunning 同一条判据（不带 O_CREATE：stop 不该在实例目录里造文件）。
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			// 从没跑过。stop 不是只读命令，可以创建锁文件；在**持锁状态下**
			// 清掉崩溃残留的 pid 记录——先清后锁的话，可能误删刚启动的新 run。
			if lf, oerr := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600); oerr == nil {
				if uerr := unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB); uerr == nil {
					clearRunPID(instDir)
					_ = unix.Flock(int(lf.Fd()), unix.LOCK_UN)
				}
				_ = lf.Close()
			}
			return errState("err.instance.not_running", nil)
		}
		return errEnv("err.env.open_lock", map[string]any{"path": lockPath}, err)
	}
	ferr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if ferr == nil {
		// 锁拿得到 = 没在跑；顺手清掉崩溃残留的 pid 记录。
		// 删除必须在持锁状态下完成：先放锁再删，可能把刚启动的新 run
		// 写下的记录误删。
		clearRunPID(instDir)
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return errState("err.instance.not_running", nil)
	}
	_ = f.Close()
	if ferr != unix.EWOULDBLOCK && ferr != unix.EAGAIN {
		return errEnv("err.env.flock", map[string]any{"path": lockPath}, ferr)
	}

	pid, start, perr := readRunPID(instDir)
	if perr != nil {
		// 锁被占着：可能是 run 正在启动（还没写 pid）、旧版本启动、或 verify
		// 正持锁。我们无法确认目标，拒绝发送信号。
		return errState("err.instance.stop_no_pid", map[string]any{"app": app, "id": id})
	}
	cur, serr := procStartTime(pid)
	if serr != nil || cur != start {
		return errState("err.instance.stop_no_pid", map[string]any{"app": app, "id": id})
	}

	// pidfd 一旦打开就绑定到具体进程：即使 PID 在校验后被复用，信号也不会
	// 打到新进程上（check→kill 的 TOCTOU 由内核收口）。再读一次启动时刻，
	// 兜住"校验之后、pidfd 打开之前"被复用的情形。
	if fd, pfdErr := unix.PidfdOpen(pid, 0); pfdErr == nil {
		defer unix.Close(fd)
		if cur2, err2 := procStartTime(pid); err2 != nil || cur2 != start {
			return errState("err.instance.stop_no_pid", map[string]any{"app": app, "id": id})
		}
		if err := unix.PidfdSendSignal(fd, syscall.SIGTERM, nil, 0); err != nil {
			if errors.Is(err, unix.ESRCH) {
				// 已经退出，停止目标达成。这里不删 run.pid：我们没持锁，
				// 锁可能刚被新 run 拿到并写下新记录，删了会误伤它；残留
				// 由持有者收尾或下一次 stop 的持锁清理带走。
				return nil
			}
			return errEnv("err.instance.stop_signal", map[string]any{"app": app, "id": id}, err)
		}
	} else {
		// 旧内核/受限环境没有 pidfd：退回普通 kill，刚刚的启动时刻校验仍然有效。
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil // 同上：不持锁时不动 run.pid
			}
			return errEnv("err.instance.stop_signal", map[string]any{"app": app, "id": id}, err)
		}
	}

	// 结束判据有两条：① run 在 defer 里先删 run.pid 再放锁，记录消失
	// （或被新 run 换掉）即结束；② runner 在注册信号处理器之前就被默认
	// SIGTERM 打死时 defers 不会执行，记录会残留——此时"锁拿得到"就是
	// 唯一可靠的事实，顺手清掉残留记录。清记录一律在持锁状态下做。
	stopped := func() bool {
		pid2, start2, err := readRunPID(instDir)
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		// pid 相同但启动时刻不同 = 记录已被新 run 换掉（PID 复用）。
		if err == nil && (pid2 != pid || start2 != start) {
			return true
		}
		lf, oerr := os.OpenFile(lockPath, os.O_RDWR, 0o600)
		if oerr != nil {
			return os.IsNotExist(oerr)
		}
		defer lf.Close()
		if lerr := unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB); lerr == nil {
			clearRunPID(instDir)
			_ = unix.Flock(int(lf.Fd()), unix.LOCK_UN)
			return true
		}
		return false
	}
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ErrCanceled
		case <-deadline.C:
			return errState("err.instance.stop_timeout", map[string]any{"app": app, "id": id})
		case <-tick.C:
			if stopped() {
				return nil
			}
		}
	}
}
