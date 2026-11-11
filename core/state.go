package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Env 是 core 的依赖注入点（§11）。所有读写磁盘的函数都是它的方法：
// 一构造就固定了路径，单测用 t.TempDir() 造一个即可，不需要 os.Setenv，
// 因此可以并行跑。
type Env struct {
	Base     string
	Resolver *PathResolver
	UID      int
	Clock    func() time.Time
	Trasher  func(paths ...string) error
	// CliPath 覆盖 CLI 二进制路径（测试注入）。空值才走 ResolveCLIPath 的三段解析：
	// 生成 .desktop 必须写绝对路径，裸 `appclone` 在图形会话里 PATH 不保证包含它。
	CliPath string
	// AppDirList 覆盖 .desktop 来源目录（测试注入）。空值走 AppDirs() 的三目录。
	AppDirList []string
	// Mounter 覆盖 overlay 挂载（测试注入）。空值走 fuse-overlayfs。
	// 审计与探测共用它：两处各写一个挂着实现，迟早只有一处被修。
	Mounter OverlayMounter
	// RunCmd 覆盖"跑一个只读外部命令拿输出"（测试注入）。空值走 exec。
	// 只有 L0 的 `flatpak info --show-permissions` 用它，所以抽成字段而不是多一层接口。
	RunCmd func(ctx context.Context, name string, args ...string) (string, error)
	// TmpRoot 是探测/审计写层的根（默认 $XDG_RUNTIME_DIR 或 /tmp，必须与
	// $HOME 分离；见 tmpRoot 的注释）。测试注入独立目录：否则用例之间会通过
	// 共享目录看到对方的残留写层，一个用例的 upper 让另一个用例的 verify
	// 读到"证据不完整"。
	TmpRoot string
	// ProbeCapsOverride 覆盖读/写追踪预检（测试注入）。空值走真实 ProbeCaps；
	// 单测不能依赖宿主是否装了 strace/bwrap，也不能每次等 5 秒超时。
	ProbeCapsOverride *ProbeCaps

	// probeReaderLeaked 是进程级"读追踪采集线程没停下"标记（ADR-034 §5.2）。
	// reader 仍持有 FIFO 读端时，再开一次探测会与旧 reader 抢同一份数据，
	// 现场也不再可解释；必须重启 GUI / 显式 reset 才能继续。
	probeReaderLeaked atomic.Bool

	mu sync.Mutex // 进程内先互斥，再抢 flock：GUI 的探测 goroutine 与列表刷新
	// 在同一进程里各开一次文件锁会自锁（flock 是进程级的，同进程不互斥），
	// 所以锁文件只在 OpenState 里局部持有，不挂在 Env 上。
}

// tmpRoot 是写层根目录。必须在 $HOME 之外（overlay 的 upper 嵌在 lower 里
// 会 ELOOP），**且必须在 fusermount3 的 AppArmor 白名单内**。
//
// 不能默认 /var/tmp：Ubuntu 26.04 给 /usr/bin/fusermount3 装了 AppArmor
// profile，只允许在 $HOME、/tmp、/run/user/<uid>、/mnt、/media 下挂 FUSE。
// 在 /var/tmp 下挂载会被 mount(2) 拒绝（"fusermount3: mount failed:
// Permission denied"），探测与审计直接失败。ADR-033。
//
// /tmp 优先于 $XDG_RUNTIME_DIR：runtime dir 一定是 tmpfs，探测会话里应用
// 写的 ~/.cache 可能上 GB，内存盘会把 ENOSPC 变成"探测半途失败且理由难懂"
// （§4.4）。/tmp 在部分发行版也是 tmpfs——doctor 会就这一点 warn，探测失败
// 时也会把 ENOSPC 归到写层根而不是应用本身。runtime dir 只在 /tmp 不可写
// 时兜底，它同样在白名单内。
func (e *Env) tmpRoot() string {
	if e.TmpRoot != "" {
		return e.TmpRoot
	}
	return DefaultTmpRoot()
}

// DefaultTmpRoot 是生产环境的写层根。doctor 的冒烟挂载与容量检查也必须
// 用它：冒烟在 /tmp 过、探测在别的根被拒，正是"doctor 说可用、探测报错"
// 的来源。
func DefaultTmpRoot() string {
	if dirWritable("/tmp") {
		return "/tmp"
	}
	if rd := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(rd) && dirWritable(rd) {
		return rd
	}
	return "/tmp"
}

// dirWritable 只判"当前用户能否在这个目录里建东西"，不往里写任何文件：
// tmpRoot 会被反复调用，留临时文件等于给清扫逻辑制造假残留。
func dirWritable(dir string) bool {
	return unix.Access(dir, unix.W_OK) == nil
}

// quarantineRoot 是保留现场（可能含用户文件）的目录。它不参与 FUSE 挂载，
// 所以可以放在持久的 /var/tmp：写层根在 tmpfs 上时，上层目录也不会随重启
// 丢掉证据。测试注入 TmpRoot 时跟随它，避免写真实 /var/tmp。
func (e *Env) quarantineRoot() string {
	if e.TmpRoot != "" {
		return e.TmpRoot
	}
	// /var/tmp 缺失/不可写（精简容器、只读根）时退回写层根：宁可只保留到
	// 重启，也不能让"保留用户文件"这一步直接失败、把探测/审计堵死。
	if dirWritable("/var/tmp") {
		return "/var/tmp"
	}
	return e.tmpRoot()
}

// NewEnv 从真实环境构造。
func NewEnv() (*Env, error) {
	r, err := NewPathResolver()
	if err != nil {
		return nil, err
	}
	base := os.Getenv("AC_BASE")
	if base == "" {
		base = filepath.Join(r.XDG["DATA_HOME"], "appclone")
	}
	if !filepath.IsAbs(base) {
		return nil, errInput("err.env.base_relative", map[string]any{"path": base})
	}
	return NewEnvAt(filepath.Clean(base), r), nil
}

// NewEnvAt 用给定根与 resolver 构造，供测试与 GUI 的多环境使用。
func NewEnvAt(base string, r *PathResolver) *Env {
	return &Env{
		Base:     filepath.Clean(base),
		Resolver: r,
		UID:      r.UID,
		Clock:    time.Now,
		Trasher:  Trash,
	}
}

// markProbeReaderLeaked 由 traceReader 在 2 秒 join 超时时调用。
func (e *Env) markProbeReaderLeaked() { e.probeReaderLeaked.Store(true) }

// probeReaderBlocked 报告是否还有上一次探测的 reader 没退出。
func (e *Env) probeReaderBlocked() bool { return e.probeReaderLeaked.Load() }

// resetProbeReaderLeak 只给显式 reset（测试/重启）用：真实泄漏无法通过
// 本进程"忘记"来修复，reader 仍持有 fd；生产路径不调用它。
func (e *Env) resetProbeReaderLeak() { e.probeReaderLeaked.Store(false) }

// AuditLayers 是 `run --audit` 才有的那一层（§6.5）：overlay 的 merged 视图
// 与"绑回真实"的公共目录。放在 BuildOptions 里而不是另写一个 argv 构造器，
// 是因为审计会话与运行期会话的其余部分（runtime/dbus/env/锁文件）必须完全一致——
// 两套构造器迟早分叉，而分叉的表现是"审计期能跑、平时跑不起来"。
type AuditLayers struct {
	Merged      string     // overlay 的 merged 视图，bind 到 HOME
	PublicBinds []BindPair // Src = 真实目录，Dst = HOME 下的目标
}

// BuildOptions 把 Env 摊平成 BuildBwrapArgs 的输入。运行时相关的项
// （RuntimeDir / RuntimeSocks / DBusAddress）由 run 自己填：它们依赖本次
// 运行的探测结果，不属于 Env。
func (e *Env) BuildOptions() BuildOptions {
	return BuildOptions{
		Resolver:   e.Resolver,
		RealHome:   e.Resolver.RealHome,
		Home:       e.Resolver.Home,
		UID:        e.UID,
		Base:       e.Base,
		RuntimeDir: e.Resolver.XDG["RUNTIME_DIR"],
		XDG:        e.Resolver.Vars(),
	}
}

// State 是全局写锁的持有者。作用域只覆盖"命名空间变更"
// （profiles.d/、instances/、appclone-*.desktop），不包括 ui.toml 与 cache/：
// 否则 GUI 一拖窗口就把 CLI 的 add 卡到超时报 6（§4.2）。
type State struct {
	env  *Env
	file *os.File
	once sync.Once
}

// OpenState 拿全局写锁。等锁超时 5 秒报 6——用户看到的是"另一个 appclone 正在操作"，
// 而不是一个永远不返回的命令。
func (e *Env) OpenState(ctx context.Context, timeout time.Duration) (*State, error) {
	if err := os.MkdirAll(e.Base, 0o700); err != nil {
		return nil, errEnv("err.env.mkdir", map[string]any{"path": e.Base}, err)
	}
	e.mu.Lock()
	path := filepath.Join(e.Base, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		e.mu.Unlock()
		return nil, errEnv("err.env.open_lock", map[string]any{"path": path}, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &State{env: e, file: f}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			f.Close()
			e.mu.Unlock()
			return nil, errEnv("err.env.flock", map[string]any{"path": path}, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			e.mu.Unlock()
			return nil, errBusy("err.lock.timeout", map[string]any{"seconds": int(timeout.Seconds())})
		}
		select {
		case <-ctx.Done():
			f.Close()
			e.mu.Unlock()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close 幂等：释放 flock、关闭文件、放开进程内互斥。
func (s *State) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.once.Do(func() {
		if s.file != nil {
			err = unix.Flock(int(s.file.Fd()), unix.LOCK_UN)
			if cerr := s.file.Close(); err == nil {
				err = cerr
			}
			s.file = nil
		}
		s.env.mu.Unlock()
	})
	return err
}

// writeFileAtomic 是所有落盘的唯一写法：同目录 tmp → Sync → Rename。
// 断电或崩溃只留一个 .tmp 文件，不会出现"半份档案"让下次解析失败（§4.2）。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errEnv("err.env.mkdir", map[string]any{"path": dir}, err)
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp.%d", filepath.Base(path), os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return errEnv("err.env.write", map[string]any{"path": tmp}, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return errEnv("err.env.write", map[string]any{"path": tmp}, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return errEnv("err.env.write", map[string]any{"path": tmp}, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return errEnv("err.env.write", map[string]any{"path": tmp}, err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return errEnv("err.env.chmod", map[string]any{"path": tmp}, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return errEnv("err.env.write", map[string]any{"path": path}, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
