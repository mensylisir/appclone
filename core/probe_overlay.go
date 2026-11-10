package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OverlayMounter 把"挂一个 overlay"抽象出来：真实现要 /dev/fuse，
// 而 CI/沙箱里没有。判定逻辑（argv、污染断言、归并）与它无关，必须能单独测。
type OverlayMounter interface {
	// Available 做 §3.2 的前置检查 + 冒烟（静态检查在容器里会假阳性）。
	Available() error
	Mount(lower, upper, work, merged string) (unmount func() error, err error)
}

// fuseOverlayMounter 是 OverlayMounter 的真实现：fuse-overlayfs + fusermount3。
type fuseOverlayMounter struct{}

// fuseSmoke 缓存"能不能真挂一次"的**成功**结论：静态检查在容器/权限受限时
// 假阳性，但每次探测都做一次冒烟挂载又太贵。
//
// 两个方向都要能翻转（§4.5"只能进不能出的状态等于卡死"）：
//   - 失败不缓存：用户装好组件、改好权限后，GUI 每 30 秒的自检要能变绿；
//   - 成功只缓存一个短 TTL：环境可能在自检之后退化（/tmp 被 remount ro、
//     AppArmor 重载、/dev/fuse 权限变化），否则界面一直显示"可用"而每次
//     探测都失败。TTL 内不重复挂载，过期后重验一次（一次空挂载而已）。
const fuseSmokeTTL = 60 * time.Second

var fuseSmoke struct {
	mu      sync.Mutex
	okUntil time.Time
}

func (fuseOverlayMounter) Available() error {
	fuseSmoke.mu.Lock()
	fresh := time.Now().Before(fuseSmoke.okUntil)
	fuseSmoke.mu.Unlock()
	if fresh {
		return nil
	}
	// 冒烟本身不在锁里做：它会 spawn fuse-overlayfs 并等挂载，持锁会把
	// 并发的 Available 全堵住。两个 goroutine 同时冒烟只是多挂一次。
	if err := fuseSmokeCheck(); err != nil {
		return err
	}
	fuseSmoke.mu.Lock()
	fuseSmoke.okUntil = time.Now().Add(fuseSmokeTTL)
	fuseSmoke.mu.Unlock()
	return nil
}

func fuseSmokeCheck() error {
	if _, err := exec.LookPath("fuse-overlayfs"); err != nil {
		return errEnv("err.probe.no_fuse_overlayfs", nil, err)
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err2 := exec.LookPath("fusermount"); err2 != nil {
			return errEnv("err.probe.no_fusermount", nil, err)
		}
	}
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		return errEnv("err.probe.no_dev_fuse", nil, err)
	} else {
		f.Close()
	}
	// 与探测同一个写层根（ADR-033）：在 /var/tmp 下冒烟必然失败，
	// 而用 MkdirTemp("") 又会跟着 $TMPDIR 漂移，结论与探测不一致。
	dir, err := os.MkdirTemp(DefaultTmpRoot(), "appclone-fusesmoke-")
	if err != nil {
		// 写层根建不出来不是"缺 /dev/fuse"：把它归进后端缺失会让 Probe 去
		// 降级 strace、让 doctor 叫用户装 fuse3，全错方向。
		return errEnv("err.probe.workroot_unavailable", map[string]any{"path": DefaultTmpRoot()}, err)
	}
	defer os.RemoveAll(dir)
	for _, sub := range []string{"lower", "upper", "work", "merged"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return errEnv("err.env.mkdir", map[string]any{"path": sub}, err)
		}
	}
	unmount, err := fuseOverlayMounter{}.Mount(
		filepath.Join(dir, "lower"), filepath.Join(dir, "upper"), filepath.Join(dir, "work"), filepath.Join(dir, "merged"))
	if err != nil {
		return err
	}
	return unmount()
}

func (fuseOverlayMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	bin, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return nil, errEnv("err.probe.no_fuse_overlayfs", nil, err)
	}
	// `-o auto_unmount`：期望 core 被 SIGKILL 时挂载点自动卸载，否则会变成
	// `Transport endpoint is not connected`、下次挂载 EBUSY（§3.2.3）。
	// **实测本机 fuse-overlayfs 1.18 + libfuse 3.18 会忽略它**（stderr 打印
	// "unknown argument ignored: auto_unmount"，SIGKILL 后 mountinfo 里仍有条目），
	// 所以不能把它当保证：保留参数作尽力而为，真正的兜底是下次探测前的
	// sweepStaleProbes（先 `-uz` 再判污染）。`-o noatime`：只读访问也会因 atime
	// 触发 copy-up，制造大量伪写入（§3.4）。
	// `-f` 前台运行：只有前台我们才拿得到进程句柄，退出时才能确定它真的没了。
	cmd := exec.Command(bin,
		"-f",
		"-o", "lowerdir="+lower,
		"-o", "upperdir="+upper,
		"-o", "workdir="+work,
		"-o", "auto_unmount",
		"-o", "noatime",
		merged)
	var errBuf syncBuffer
	cmd.Stderr = &errBuf
	// Pdeathsig 兜底：core 被 SIGKILL 时 fuse-overlayfs 一起走。
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return nil, errEnv("err.probe.mount_failed", map[string]any{"output": strings.TrimSpace(errBuf.String())}, err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mountLive(merged) {
			return func() error {
				err := umount(merged)
				_ = cmd.Process.Kill()
				select {
				case <-waited:
				case <-time.After(2 * time.Second):
				}
				return err
			}, nil
		}
		select {
		case <-waited:
			return nil, errEnv("err.probe.mount_failed", map[string]any{
				"output": strings.TrimSpace(errBuf.String()),
			}, nil)
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	// 等 fuse 进程真的退出，并补一次卸载：最后一次轮询与超时判定之间它可能
	// 刚好把挂载挂上。不处理的话，调用方会在"以为没挂载"的情况下删工作目录，
	// 穿过 merged 制造 whiteout。
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
	}
	if mounted(merged) {
		_ = umount(merged)
	}
	// 超时是产品判定，不是 fuse 的输出：前缀进消息表，{output} 只放外部
	// 命令自己的 stderr（可能是英文），不把中文塞进 en 界面。
	return nil, errEnv("err.probe.mount_timeout", map[string]any{
		"output": strings.TrimSpace(errBuf.String()),
	}, nil)
}

// umount 先试正常卸载，失败退 -uz（懒卸载）：正常卸载在 EBUSY 时只有这一条路。
func umount(merged string) error {
	for _, name := range []string{"fusermount3", "fusermount"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if out, err := exec.Command(p, "-u", merged).CombinedOutput(); err == nil {
			return nil
		} else if out2, err2 := exec.Command(p, "-uz", merged).CombinedOutput(); err2 != nil {
			return errEnv("err.probe.umount_failed", map[string]any{
				"output": strings.TrimSpace(string(out)) + " / " + strings.TrimSpace(string(out2)),
			}, err)
		}
		return nil
	}
	return errEnv("err.probe.no_fusermount", nil, nil)
}

// mounted 判挂载点是否已经生效（不是"目录存在"，而是它真的在 mountinfo 里）。
// mounted 只认 mountinfo，不做 os.Stat：fuse daemon 已死时 stat 返回
// ENOTCONN，会把仍在 mountinfo 里的挂载判成"没挂载"；而 mountinfo 对
// 空格等字符做八进制转义，路径必须解码后整串比对（mountInfoHas），否则
// HOME/AC_BASE 带空格时永远判不中。清理路径（stale 回收）用这个语义。
func mounted(path string) bool { return mountPointIn(path) }

// mountLive 要求挂载既在 mountinfo 里、又能访问（stat 成功）。挂载轮询必须
// 用它：fuse-overlayfs 挂上后立刻 crash 时 mountinfo 仍有条目，mounted 会
// 把死挂载判成成功，应用随后在数据目录上直接 ENOTCONN 退出。
func mountLive(path string) bool {
	if !mounted(path) {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// syncBuffer 是并发安全的 stderr 收集器（进程 stderr 在另一个 goroutine 里写）。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
