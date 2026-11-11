package core

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// probeStderr 把会话 stderr 同时转给调用方并做有限扫描（ADR-034 §4.4）。
// 只认以 `strace:` 开头的行，避免把应用自己打印的 "ptrace"/"setuid" 当成
// strace 错误；最多保留 64KiB，不落盘。
type probeStderr struct {
	dst io.Writer

	mu      sync.Mutex
	pending []byte
	ptrace  bool
	setuid  bool
}

const probeStderrLimit = 64 * 1024

func newProbeStderr(dst io.Writer) *probeStderr {
	return &probeStderr{dst: dst}
}

func (w *probeStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	// strace 的 stderr 可能被拆成多次 write：必须保留残行，否则 `strace:`
	// 与关键词分在两次 Write 里时会漏判（ptrace 拒绝会被当成正常退出）。
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.scanLine(string(w.pending[:i]))
		w.pending = w.pending[i+1:]
	}
	if len(w.pending) > probeStderrLimit {
		w.pending = nil
	}
	w.mu.Unlock()
	if w.dst != nil {
		return w.dst.Write(p)
	}
	return len(p), nil
}

// flush 处理最后一条没有换行符的残行；会话结束后由调用方调用。
func (w *probeStderr) flush() {
	w.mu.Lock()
	if len(w.pending) > 0 {
		w.scanLine(string(w.pending))
		w.pending = nil
	}
	w.mu.Unlock()
}

// scanLine 只认以 `strace:` 开头的行，避免把应用自己打印的 "ptrace"/
// "setuid" 当成 strace 错误。调用方必须持有 w.mu。
func (w *probeStderr) scanLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "strace:") {
		return
	}
	low := strings.ToLower(line)
	if strings.Contains(low, "ptrace") || strings.Contains(low, "operation not permitted") {
		w.ptrace = true
	}
	if strings.Contains(low, "setuid") &&
		(strings.Contains(low, "exec") || strings.Contains(low, "ptrace") || strings.Contains(low, "attach")) {
		// 必须同时出现 strace 的追踪语境，避免应用自己打印
		// "strace: setuid ..." 这类行被误当成 setuid 子进程告警。
		w.setuid = true
	}
}

func (w *probeStderr) sawPtraceDenied() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ptrace
}

func (w *probeStderr) sawSetuid() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.setuid
}
