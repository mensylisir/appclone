package main

// GUI 启动的分身与设置窗口解耦（ADR-028）。
//
// 点「启动」时不用 GUI 自己的进程跑 core.Run，而是以自身二进制 fork 一个
// 会话首进程（runner）。关掉设置窗口只会结束 GUI，runner 继续常驻为 bwrap
// 的父进程，应用照常运行。runner 是一次性进程，不监听、不提供 API——不是
// daemon，也不引入 IPC（ADR-004 不变）。
//
// 停止由 core.StopRun 完成：它先用 run.lock 证明实例在跑，再按 run.pid
// 向 runner 发 SIGTERM，走 core.Run 与 CLI 共用的整组终止链（ADR-020）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/mensylisir/appclone/core"
	"github.com/mensylisir/appclone/core/i18n"
)

const (
	detachAppEnv = "APPCLONE_GUI_DETACH_APP"
	detachIDEnv  = "APPCLONE_GUI_DETACH_ID"

	// runLogLimit 限制单次运行落盘的应用输出上限。Electron/Chromium 类应用
	// 会持续往 stderr 写，不设上限可能占满磁盘；超过上限后静默丢弃后续输出
	// （启动失败诊断都在前几行，保留尾部意义不大——这里保留的是头部）。
	runLogLimit = 1 << 20 // 1 MiB
)

// cappedWriter 把应用输出截断到 runLogLimit：写入超出部分直接丢弃，
// 但对调用方仍报告"写完了"，避免 exec 的 copier 报 ErrShortWrite。
// core.Run 的 stdout/stderr 由两个 copier 并发写，必须加锁。
type cappedWriter struct {
	mu   sync.Mutex
	w    io.Writer
	left int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	orig := len(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.left <= 0 {
		return orig, nil
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.w.Write(p)
	c.left -= int64(n)
	if err != nil {
		return n, err
	}
	return orig, nil
}

// detachedExecutable 是"自己的二进制"的取法；GTK 闸门换成脚本，
// 才能在不起真 runner 的情况下验证 fork 的 env / 会话 / 收尸。
var detachedExecutable = os.Executable

// detachedTarget 读 runner 标记。只在 fork 出来的子进程里为真；
// 正常启动的 GUI 永远走 GTK 初始化。
func detachedTarget() (string, string, bool) {
	app := os.Getenv(detachAppEnv)
	id := os.Getenv(detachIDEnv)
	if app == "" || id == "" {
		return "", "", false
	}
	return app, id, true
}

// runDetached 是 runner 的主体：只做 core.Run，绝不初始化 GTK。
// 错误写进继承来的 stderr（父进程把它接到实例目录的 run.log），退出码
// 沿用 core 的码表；GUI 侧目前只用"非零 + 2 秒内退出"给启动失败提示，
// 没有逐码分支。
func runDetached(appKey, id string) int {
	// 不把内部标记传给应用：bwrap 的参数集不会主动清环境，应用没有理由看见它。
	_ = os.Unsetenv(detachAppEnv)
	_ = os.Unsetenv(detachIDEnv)

	env, err := core.NewEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "appclone-gui:", i18n.T(i18n.Lang(), errorKey(err), errorArgs(err)))
		return core.ExitCode(err)
	}
	// 应用输出只留前 1 MiB：既够定位启动失败，又不让长跑应用写满磁盘。
	out := &cappedWriter{w: os.Stderr, left: runLogLimit}
	_, rerr := env.Run(context.Background(), appKey, id, core.RunOptions{
		Stdout: out,
		Stderr: out,
	})
	if rerr != nil {
		if errors.Is(rerr, core.ErrCanceled) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "appclone:", i18n.T(i18n.Lang(), errorKey(rerr), errorArgs(rerr)))
		return core.ExitCode(rerr)
	}
	return 0
}

// startDetached 起 runner 并立即返回。wait 由调用方在 goroutine 里收尸
// （不 Wait 会留下僵尸）。stdout/stderr 落到实例目录的 run.log：
// 应用已经跑起来之后 GUI 就不再关心输出，但启动失败时这是唯一线索。
func startDetached(env *core.Env, appKey, id string) (wait func() error, logPath string, logOffset int64, err error) {
	exe, err := detachedExecutable()
	if err != nil {
		return nil, "", 0, err
	}
	logPath = filepath.Join(env.InstancePath(appKey, id), "run.log")
	// 快速连点「启动」时两次打开同一个文件：第二次不能把赢家的诊断截掉，
	// 所以正常追加；只有上一轮留下的日志已经超过上限时才重整。
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, serr := os.Stat(logPath); serr == nil {
		logOffset = st.Size()
		if st.Size() > runLogLimit {
			flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			logOffset = 0
		}
	}
	lf, err := os.OpenFile(logPath, flags, 0o600)
	if err != nil {
		return nil, logPath, 0, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), detachAppEnv+"="+appKey, detachIDEnv+"="+id)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = lf, lf
	// Setsid：runner 脱离 GUI 的会话/进程组。GUI 被信号打死时它不会跟着死，
	// 也不会因为终端/窗口关闭收到 SIGHUP。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	serr := cmd.Start()
	_ = lf.Close()
	if serr != nil {
		return nil, logPath, 0, serr
	}
	return cmd.Wait, logPath, logOffset, nil
}

// lastLogLine 取 run.log 最后一条有内容的行，去掉内部的 "appclone:" 前缀。
// 只用于启动失败的 toast；应用跑起来之后的输出不再打扰用户。
//
// offset 是本次启动前的文件长度：追加模式下只读新写入的部分，否则上一次
// 运行的最后一行会被当成这次失败的原因。
//
// best-effort：cappedWriter 保留的是本次运行的前 1 MiB，若应用先灌满上限
// 再失败，失败原因本身已被丢弃，这里只剩保留区里的最后一行。
func lastLogLine(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// 应用可以往 stderr 灌任意多内容；只读尾部 8 KiB，避免启动失败时
	// 因为一条日志把整个文件读进内存。
	const tail = 8 << 10
	if st, serr := f.Stat(); serr == nil {
		start := st.Size() - tail
		if start < offset {
			start = offset
		}
		if start < 0 {
			start = 0
		}
		if start > 0 {
			if _, serr := f.Seek(start, 0); serr != nil {
				return ""
			}
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		s = strings.TrimPrefix(s, "appclone-gui: ")
		s = strings.TrimPrefix(s, "appclone: ")
		// GTK 的字符串 API 要求合法 UTF-8：8 KiB 截断可能切在多字节 rune
		// 中间，应用自己也可能吐非法字节；去掉控制字符后再进 toast。
		s = strings.ToValidUTF8(s, "")
		s = strings.Map(func(r rune) rune {
			if r == '\t' || (r >= 0x20 && r != 0x7f) {
				return r
			}
			return -1
		}, s)
		if s != "" {
			return s
		}
	}
	return ""
}
