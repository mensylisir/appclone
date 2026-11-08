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

// fuseSmoke 缓存"能不能真挂一次"的结论：静态检查在容器/权限受限时假阳性，
// 但每次探测都做一次冒烟挂载又太贵。进程内只做一次。
var fuseSmoke struct {
	once sync.Once
	err  error
}

func (fuseOverlayMounter) Available() error {
	fuseSmoke.once.Do(func() { fuseSmoke.err = fuseSmokeCheck() })
	return fuseSmoke.err
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
	dir, err := os.MkdirTemp("", "appclone-fusesmoke-")
	if err != nil {
		return errEnv("err.probe.no_dev_fuse", nil, err)
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
		if mounted(merged) {
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
func mounted(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), " "+path+" ")
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
