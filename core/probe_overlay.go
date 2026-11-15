package core

import (
	"errors"
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
	if err := sweepStaleSmoke(DefaultTmpRoot()); err != nil {
		return err
	}
	return fuseSmokeAt(DefaultTmpRoot(), fuseOverlayMounter{})
}

func fuseSmokeAt(root string, mounter OverlayMounter) (retErr error) {
	dir, err := os.MkdirTemp(root, "appclone-fusesmoke-")
	if err != nil {
		// 写层根建不出来不是"缺 /dev/fuse"，应提示修复目录权限，不能误导用户装包。
		return errEnv("err.probe.workroot_unavailable", map[string]any{"path": root}, err)
	}
	scene, err := openOverlayScene(dir)
	if err != nil {
		return err
	}
	defer scene.close()
	keep := false
	defer func() {
		if !keep {
			if err := scene.remove("."); err != nil {
				retErr = err
			}
		}
	}()
	for _, sub := range []string{"lower", "upper", "work", "merged"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return errEnv("err.env.mkdir", map[string]any{"path": sub}, err)
		}
	}
	unmount, err := mounter.Mount(
		filepath.Join(dir, "lower"), filepath.Join(dir, "upper"), filepath.Join(dir, "work"), filepath.Join(dir, "merged"))
	if err != nil {
		return err
	}
	if err := scene.checkIdle(); err != nil {
		keep = true
		return err
	}
	if err := unmount(); err != nil {
		keep = true
		return sceneError(dir, err)
	}
	return nil
}

// sweepStaleSmoke 清掉异常退出（panic/SIGKILL）留下的冒烟目录：它们只有
// 空目录骨架，但写层根是用户可见区。只碰本 uid 且 10 分钟没动过的——
// 另一个进程正在冒烟时删它的目录会把结论变成假失败。
func sweepStaleSmoke(root string) error {
	if _, err := checkedOrdinaryPath(root, true); err != nil {
		return err
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "appclone-fusesmoke-*"))
	for _, d := range dirs {
		scene, err := ownedOverlayScene(d)
		if err != nil {
			return err
		}
		if scene == nil {
			continue
		}
		if time.Since(scene.info.ModTime()) < 10*time.Minute {
			scene.close()
			continue
		}
		err = scene.detachStale()
		if err == nil {
			err = scene.remove(".")
		}
		scene.close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (fuseOverlayMounter) Mount(lower, upper, work, merged string) (func() error, error) {
	bin, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return nil, errEnv("err.probe.no_fuse_overlayfs", nil, err)
	}
	scene, err := openOverlayScene(filepath.Dir(merged))
	if err != nil {
		return nil, err
	}
	merged = filepath.Join(scene.path, filepath.Base(merged))
	transferred := false
	defer func() {
		if !transferred {
			scene.close()
		}
	}()
	if err := scene.checkUnmounted(); err != nil {
		return nil, err
	}
	starting, err := beginFuseStarting(scene)
	if err != nil {
		return nil, sceneError(scene.path, err)
	}
	// Foreground + a dedicated group keep mount helpers accountable. Do not use
	// auto_unmount: libfuse's background helper can setsid out of this group.
	cmd := exec.Command(bin,
		"-f",
		"-o", "lowerdir="+lower,
		"-o", "upperdir="+upper,
		"-o", "workdir="+work,
		"-o", "noatime",
		merged)
	var errBuf syncBuffer
	cmd.Stderr = &errBuf
	// Pdeathsig 兜底：core 被 SIGKILL 时 fuse-overlayfs 一起走。
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setsid: true}
	if err := cmd.Start(); err != nil {
		if cerr := starting.complete(); cerr != nil {
			return nil, sceneError(scene.path, cerr)
		}
		return nil, errEnv("err.probe.mount_failed", map[string]any{"output": strings.TrimSpace(errBuf.String())}, err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	// Direct-child exit does not prove fusermount has stopped. Never signal an
	// unconfirmed group; an unknown group instead leaves the persistent marker.
	reap := func() error {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
			return errEnv("err.probe.mount_unreaped", nil, nil)
		}
		if !sessionGroupGone(cmd.Process.Pid) {
			return errEnv("err.probe.mount_unreaped", nil, nil)
		}
		return nil
	}
	fail := func(cause error) (func() error, error) {
		if err := reap(); err != nil {
			return nil, sceneError(scene.path, err)
		}
		if err := starting.complete(); err != nil {
			return nil, sceneError(scene.path, err)
		}
		return nil, cause
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		table, err := scene.checkStructure()
		if err != nil {
			return fail(err)
		}
		select {
		case <-waited:
			return fail(errEnv("err.probe.mount_failed", map[string]any{"output": strings.TrimSpace(errBuf.String())}, nil))
		default:
		}
		if record := table[merged]; record != "" {
			current, err := scene.checkStructure()
			if err != nil || current[merged] != record {
				return fail(sceneError(scene.path, errors.New("mount changed while completing startup")))
			}
			select {
			case <-waited:
				return fail(sceneError(scene.path, errors.New("mount driver exited during startup")))
			default:
			}
			if err := starting.complete(); err != nil {
				_ = reap()
				return nil, sceneError(scene.path, err)
			}
			var once sync.Once
			var stopErr error
			transferred = true
			return func() error {
				once.Do(func() {
					defer scene.close()
					current, err := scene.checkStructure()
					if err != nil {
						stopErr = err
						return
					}
					if current[merged] != record {
						stopErr = sceneError(scene.path, errors.New("mount identity changed"))
						return
					}
					stopping, err := beginFuseStarting(scene)
					if err != nil {
						stopErr = sceneError(scene.path, err)
						return
					}
					unmountErr := umount(merged)
					reapErr := reap()
					if unmountErr != nil || reapErr != nil {
						stopErr = sceneError(scene.path, errors.Join(unmountErr, reapErr))
						return
					}
					if err := stopping.complete(); err != nil {
						stopErr = sceneError(scene.path, err)
					}
				})
				return stopErr
			}, nil
		}
		select {
		case <-waited:
			return fail(errEnv("err.probe.mount_failed", map[string]any{
				"output": strings.TrimSpace(errBuf.String()),
			}, nil))
		case <-time.After(50 * time.Millisecond):
		}
	}
	// 超时是产品判定，不是 fuse 的输出：前缀进消息表，{output} 只放外部
	// 命令自己的 stderr（可能是英文），不把中文塞进 en 界面。
	return fail(errEnv("err.probe.mount_timeout", map[string]any{
		"output": strings.TrimSpace(errBuf.String()),
	}, nil))
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
