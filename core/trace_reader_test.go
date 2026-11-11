package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTraceReaderReadsAndStops(t *testing.T) {
	dir := t.TempDir()
	r, err := newTraceReader(dir, "/home/u", nil)
	if err != nil {
		t.Fatalf("newTraceReader: %v", err)
	}
	w, err := os.OpenFile(r.path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := w.WriteString(`1234  openat(AT_FDCWD, "/home/u/.config/App/x", O_RDONLY) = 3
1234  openat(AT_FDCWD, "/home/u/.config/App/y", O_WRONLY|O_CREAT, 0666) = 4
`); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()
	// 残行：不完整的一行必须在 stop 时被丢弃，不能卡住 reader。
	w2, err := os.OpenFile(r.path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w2.WriteString(`1234  openat(AT_FDCWD, "/home/u/.config/App/partial`)
	w2.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.snapshot().Reads) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(res.Reads) != 1 || res.Reads[0] != ".config/App/x" {
		t.Fatalf("读路径错：%v", res.Reads)
	}
	if len(res.Writes) != 1 || res.Writes[0].Path != ".config/App/y" {
		t.Fatalf("写条目错：%v", res.Writes)
	}
}

func TestTraceReaderStopWithoutWriter(t *testing.T) {
	dir := t.TempDir()
	r, err := newTraceReader(dir, "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := r.stopAndResult(); err != nil {
		t.Fatalf("无写端也必须能停：%v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("无写端 stop 过慢：%v", d)
	}
	if !strings.HasSuffix(r.path(), filepath.Join("reads.fifo")) {
		t.Fatalf("FIFO 路径错：%s", r.path())
	}
}

func TestTraceReaderSizeLimitKeepsDraining(t *testing.T) {
	dir := t.TempDir()
	r, err := newTraceReader(dir, "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	line := `1234  openat(AT_FDCWD, "/home/u/.config/App/x", O_RDONLY) = 3` + "\n"
	w, err := os.OpenFile(r.path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 去重后只有 1 条路径；上限按"过滤后新增条目"计，因此这里只验证
	// 大量重复行不会让 reader 停止工作。
	for i := 0; i < 1000; i++ {
		if _, err := w.WriteString(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	w.Close()
	time.Sleep(50 * time.Millisecond)
	res, err := r.stopAndResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reads) != 1 {
		t.Fatalf("重复行应去重为 1 条，得到 %v", res.Reads)
	}
}

// 上限按"过滤后新增条目"计：第 100001 条唯一路径必须置 SizeExceeded，
// 且 reader 仍继续排空（strace 写端不能阻塞）。
func TestTraceReaderEntryLimit(t *testing.T) {
	r := &traceReader{
		fifo: "test", home: "/home/u",
		readSeen: map[string]bool{}, writeSeen: map[string]bool{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	for i := 0; i < traceReadLimit; i++ {
		r.consume(`1234  openat(AT_FDCWD, "/home/u/.config/App/f` + strconv.Itoa(i) + `", O_RDONLY) = 3`)
	}
	if r.sizeHit {
		t.Fatal("恰好 100000 条不应判超限")
	}
	r.consume(`1234  openat(AT_FDCWD, "/home/u/.config/App/overflow", O_RDONLY) = 3`)
	if !r.sizeHit {
		t.Fatal("第 100001 条必须置 SizeExceeded")
	}
	if got := r.snapshot(); !got.Stats.SizeExceeded {
		t.Fatal("snapshot 必须带上 SizeExceeded")
	}
}

// join 超时：采集线程卡住时 stopAndResult 必须回调 onStuck 并返回
// reader_stuck，而不是无限等待。
func TestTraceReaderStuck(t *testing.T) {
	old := traceReaderJoinTimeout
	traceReaderJoinTimeout = 20 * time.Millisecond
	defer func() { traceReaderJoinTimeout = old }()
	stuck := false
	r := &traceReader{
		fifo: "test", home: "/home/u",
		readSeen: map[string]bool{}, writeSeen: map[string]bool{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}), // 永不关闭 = 模拟采集线程卡住
		onStuck: func() { stuck = true },
	}
	if _, err := r.stopAndResult(); err == nil || errKey(err) != "err.probe.reader_stuck" {
		t.Fatalf("应返回 reader_stuck，得到 %v", err)
	}
	if !stuck {
		t.Fatal("超时必须回调 onStuck")
	}
}
