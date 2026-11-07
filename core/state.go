package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

	mu   sync.Mutex // 进程内先互斥，再抢 flock：GUI 的探测 goroutine 与列表刷新
	lock *os.File   // 在同一进程里各开一次会自锁（flock 是进程级的，同进程不互斥）
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
