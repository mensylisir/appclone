package core

import (
	"bytes"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// traceReader 是探测会话的 FIFO 读端（ADR-034 §5）。
//
// 生命周期：每次探测一个 FIFO、一个 reader，跨所有轮次复用；只有全部轮次结束、
// 取消或硬错误才 stop。fd 由 reader 独占，stop 只置标志并等待，不在另一个
// goroutine close（避免 fd 号复用与读竞争）。
type traceReader struct {
	fifo string
	home string
	fd   int
	// onStuck 在 2 秒 join 超时时回调（Env 置进程级 reader_stuck 标记）。
	onStuck func()

	mu        sync.Mutex
	res       StraceResult
	readSeen  map[string]bool
	writeSeen map[string]bool
	entries   int
	sizeHit   bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

const traceReadLimit = 100000

// traceReaderJoinTimeout 是 stopAndResult 等待采集线程退出的上限。
// 变量而非常量：测试需要把 2 秒压到毫秒级，否则一条 reader_stuck 用例
// 就要让整个包慢两秒。
var traceReaderJoinTimeout = 2 * time.Second

func newTraceReader(dir, home string, onStuck func()) (*traceReader, error) {
	fifo := filepath.Join(dir, "reads.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return nil, errEnv("err.probe.trace_fifo", map[string]any{"path": fifo}, err)
	}
	fd, err := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errEnv("err.probe.trace_fifo", map[string]any{"path": fifo}, err)
	}
	r := &traceReader{
		fifo: fifo, home: filepath.Clean(home), fd: fd, onStuck: onStuck,
		readSeen: map[string]bool{}, writeSeen: map[string]bool{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go r.loop()
	return r, nil
}

func (r *traceReader) path() string { return r.fifo }

func (r *traceReader) loop() {
	defer close(r.done)
	defer syscall.Close(r.fd)
	buf := make([]byte, 64*1024)
	var residual []byte
	for {
		n, err := syscall.Read(r.fd, buf)
		if n > 0 {
			residual = append(residual, buf[:n]...)
			for {
				i := bytes.IndexByte(residual, '\n')
				if i < 0 {
					break
				}
				line := string(residual[:i])
				residual = residual[i+1:]
				r.consume(line)
			}
			if len(residual) > 1<<20 {
				// 单行超过 1MiB：不可能是一条正常 strace 行，丢弃并计数。
				r.mu.Lock()
				r.res.Stats.ParseErrors++
				r.mu.Unlock()
				residual = nil
			}
			continue
		}
		if err != nil && err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
			if r.stopped() {
				return
			}
			// 读错误不终止采集：记录后继续排空，避免 strace 写端阻塞。
			r.mu.Lock()
			r.res.Stats.ParseErrors++
			r.mu.Unlock()
		}
		if r.stopped() {
			// stop 后已把本轮读到的数据全部按行消费；n==0 说明写端已无数据。
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *traceReader) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *traceReader) consume(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries >= traceReadLimit {
		// 已到上限：继续解析但丢弃结果，保证 FIFO 被排空、strace 不阻塞。
		var sink StraceResult
		parseStraceLineInto(line, r.home, &sink, map[string]bool{}, map[string]bool{})
		if len(sink.Reads)+len(sink.Writes) > 0 {
			r.sizeHit = true
		}
		return
	}
	before := len(r.res.Reads) + len(r.res.Writes)
	parseStraceLineInto(line, r.home, &r.res, r.readSeen, r.writeSeen)
	added := len(r.res.Reads) + len(r.res.Writes) - before
	if added == 0 {
		return
	}
	// 上限按"新增条目"判定：恰好 100k 后再来重复行不算超限。
	if r.entries+added > traceReadLimit {
		r.sizeHit = true
	}
	r.entries += added
}

// stopAndResult 停止 reader 并返回聚合结果。2 秒内不退出按 reader_stuck
// 处理：保留 work（reader 仍持 fd），调用方不得删除现场。
func (r *traceReader) stopAndResult() (StraceResult, error) {
	r.stopOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
	case <-time.After(traceReaderJoinTimeout):
		if r.onStuck != nil {
			r.onStuck()
		}
		return r.snapshot(), errEnv("err.probe.reader_stuck", map[string]any{"fifo": r.fifo}, nil)
	}
	return r.snapshot(), nil
}

func (r *traceReader) snapshot() StraceResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := StraceResult{
		Reads:       append([]string{}, r.res.Reads...),
		Writes:      append([]UpperEntry{}, r.res.Writes...),
		Execs:       append([]string{}, r.res.Execs...),
		FlagsHidden: append([]string{}, r.res.FlagsHidden...),
		Stats:       r.res.Stats,
	}
	out.ExecSeen = r.res.ExecSeen
	sort.Strings(out.Reads)
	out.Stats.SizeExceeded = r.sizeHit
	return out
}
