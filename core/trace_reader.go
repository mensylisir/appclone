package core

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	return newTraceReaderOpts(dir, home, onStuck, false)
}

// newTraceReaderOpts 允许兜底路径把 reader 设成“exec 只作证据”：记录
// ExecCalls/ExecSeen/Execs，但不把 exec 目标追加到 Reads。
func newTraceReaderOpts(dir, home string, onStuck func(), execEvidenceOnly bool) (*traceReader, error) {
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
	r.res.keyringSeen = map[string]bool{}
	r.res.execEvidenceOnly = execEvidenceOnly
	go r.loop()
	return r, nil
}

func (r *traceReader) path() string { return r.fifo }

// markRound 在每轮 runner.Run 之前写入轮次标记行。reader 以 O_RDWR 持有
// FIFO，这里另开 O_WRONLY|O_NONBLOCK 一次 write；前导换行切断上一轮可能
// 留下的半行。写入失败是 best-effort：只计数并保持旧轮次，绝不阻塞 probe。
func (r *traceReader) markRound(n int) {
	if r == nil {
		return
	}
	fd, err := syscall.Open(r.fifo, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		r.dropRoundMarker()
		return
	}
	defer syscall.Close(fd)
	line := []byte(fmt.Sprintf("\n#appclone-round %d\n", n))
	if nw, werr := syscall.Write(fd, line); werr != nil || nw != len(line) {
		r.dropRoundMarker()
	}
}

func (r *traceReader) dropRoundMarker() {
	r.mu.Lock()
	r.res.Stats.RoundMarkerDropped++
	r.mu.Unlock()
}

// parseRoundMarker 识别 `#appclone-round n`（允许前后空白）。
func parseRoundMarker(line string) (int, bool) {
	line = strings.TrimSpace(line)
	const prefix = "#appclone-round "
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[len(prefix):]))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

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
		// Sleep(10ms) wakes 100 times per second even when the app is idle,
		// and holds a newly arrived burst until the next tick. Poll wakes as
		// soon as data arrives; its bounded timeout also observes stop without
		// closing a descriptor from another goroutine (fd reuse race).
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		_, perr := unix.Poll(fds, 250)
		if (perr != nil && perr != unix.EINTR) || fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			// A broken descriptor must not turn the recovery path into a spin.
			select {
			case <-r.stop:
			case <-time.After(250 * time.Millisecond):
			}
		}
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
	// 标记行必须在 100k 上限判断之前识别：超过上限后仍要能切换轮次。
	if n, ok := parseRoundMarker(line); ok {
		r.res.round = n
		return
	}
	if r.entries >= traceReadLimit {
		// 已到上限：继续解析但丢弃路径结果，保证 FIFO 被排空、strace 不阻塞。
		// exec/keyring 证据是**采集级**状态，必须带回 r.res：它们是上限之外
		// 的独立口径，不能每行重置（方案 §6 D3 / ADR-036）。
		//
		// 这里直接共享底层切片而不深拷贝：解析在 r.mu 内单线程进行，sink 的
		// append/淘汰都作用于同一组数组，解析完再整体赋回 r.res。此前每行
		// 深拷贝 256 条 argv，超限后（子进程反复 exec 时）会变成显著的 CPU
		// 与内存抖动。
		sink := StraceResult{
			Stats:               r.res.Stats,
			ExecCalls:           r.res.ExecCalls,
			ExecSeen:            r.res.ExecSeen,
			Execs:               r.res.Execs,
			execEvidenceOnly:    r.res.execEvidenceOnly,
			round:               r.res.round,
			keyringSeen:         r.res.keyringSeen,
			keyringScanned:      r.res.keyringScanned,
			KeyringHits:         r.res.KeyringHits,
			KeyringTraceDropped: r.res.KeyringTraceDropped,
		}
		parseStraceLineInto(line, r.home, &sink, map[string]bool{}, map[string]bool{})
		if len(sink.Reads)+len(sink.Writes) > 0 {
			r.sizeHit = true
		}
		r.res.Stats = sink.Stats
		r.res.ExecCalls = sink.ExecCalls
		r.res.ExecSeen = sink.ExecSeen
		r.res.Execs = sink.Execs
		r.res.execEvidenceOnly = sink.execEvidenceOnly
		r.res.round = sink.round
		r.res.keyringSeen = sink.keyringSeen
		r.res.keyringScanned = sink.keyringScanned
		r.res.KeyringHits = sink.KeyringHits
		r.res.KeyringTraceDropped = sink.KeyringTraceDropped
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
		ExecCalls:   deepCopyExecCalls(r.res.ExecCalls),
		FlagsHidden: append([]string{}, r.res.FlagsHidden...),
		Stats:       r.res.Stats,
		round:       r.res.round,
	}
	out.ExecSeen = r.res.ExecSeen
	out.execEvidenceOnly = r.res.execEvidenceOnly
	out.KeyringHits = append([]string{}, r.res.KeyringHits...)
	out.KeyringTraceDropped = r.res.KeyringTraceDropped
	sort.Strings(out.Reads)
	out.Stats.SizeExceeded = r.sizeHit
	return out
}

// readCount is the progress-only view: O(1), no snapshot allocation or sort.
func (r *traceReader) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.res.Reads)
}
