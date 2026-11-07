package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProbeSpec 是"探测会话要 bind 什么"的全部输入（docs/discover.md §3.2.4）。
// 单独抽出来是为了让 argv 构造成为纯函数：这条命令一旦顺序写错，后果是
// "用户存进 ~/Downloads 的文件被删"或者"探测零写入"，都必须能断言。
type ProbeSpec struct {
	Home        string     // 有效 HOME（AC_HOME 或真实 HOME）
	RealHome    string     // 真实 HOME：公共目录的 bind 源取它
	Base        string     // AC_BASE（探测期要 bind 回去，否则工具自己的写入会进 upper）
	Merged      string     // overlay 的 merged 视图
	RuntimeDir  string     // 探测期私有 XDG_RUNTIME_DIR（实例目录下）
	RuntimeSock []BindPair // 宿主 socket（wayland/pulse/at-spi）：Src 在宿主，Dst 在探测私有 runtime 里
	DBusAddress string     // 私有会话总线地址；空 = 没有总线（降级）
	PublicDirs  []string   // 公共目录：必须在其后 bind 回真实目录
	SharedWrite []string   // 共享可写文件（输入法/书签），尽量 bind 回真实
	LockFiles   []string   // L0 预填的锁文件：bind 空文件盖住，避免转发到主实例
	Command     []string   // 应用命令（argv[0] 之后全部原样）
}

// ProbeBwrapArgs 产出探测会话的完整 argv（纯函数，golden 断言）。
//
// 顺序即语义，与 §3.2.4 逐条对应：
//
//	① --unshare-user/pid/ipc（探测期一律全套隔离，见 §3.2.2）
//	② --bind / /（全透传）
//	③ --dev-bind /dev/dev ④ --proc /proc（必须在其后：同目标后挂载覆盖先挂载）
//	⑤ --bind merged HOME（overlay 罩住家目录）
//	⑥ 公共目录 bind 回真实目录（**必须在 ⑤ 之后**，否则被 overlay 罩回去，
//	   用户在探测会话里保存的文件会落进 upper，探测结束删 upper 就是删用户数据）
//	⑦ AC_BASE 与其 instances/ 的 tmpfs 屏蔽（探测期不该写实例数据）
//	⑧ shared_writes（-try：不存在就跳过）
//	⑨ 锁文件（bind 空文件；只接受普通文件）
//	⑩ 私有 runtime + 宿主 socket 只读引入
//	⑪ --setenv XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS / HOME
//	⑫ --die-with-parent（兜底；主保证是 pgid kill）
//	⑬ -- 应用命令
func ProbeBwrapArgs(s ProbeSpec) ([]string, error) {
	if s.Home == "" || !filepath.IsAbs(s.Home) {
		return nil, errInternal("err.internal.probe_spec", map[string]any{"field": "home"}, nil)
	}
	if s.Merged == "" || !filepath.IsAbs(s.Merged) {
		return nil, errInternal("err.internal.probe_spec", map[string]any{"field": "merged"}, nil)
	}
	if len(s.Command) == 0 {
		return nil, errInput("err.probe.no_command", nil)
	}
	argv := []string{
		"bwrap",
		"--unshare-user", "--unshare-pid", "--unshare-ipc",
		"--bind", "/", "/",
		"--dev-bind", "/dev", "/dev",
		"--proc", "/proc",
		"--bind", s.Merged, s.Home,
	}
	// 公共目录：源是**宿主真实目录**，目标与有效 HOME 下的同名路径对齐。
	for _, pub := range s.PublicDirs {
		target := pub
		if rel, ok := relUnder(s.RealHome, pub); ok && rel != "" {
			target = filepath.Join(s.Home, rel)
		}
		src := pub
		if rel, ok := relUnder(s.RealHome, pub); ok && rel != "" {
			src = filepath.Join(s.RealHome, rel)
		}
		argv = append(argv, "--bind", src, target)
	}
	if s.Base != "" {
		argv = append(argv, "--bind", s.Base, s.Base)
		// 探测期不需要、也不该看见已有分身：把 instances/ 换成空 tmpfs，
		// 否则"探测会写进主实例目录"这件事没法从参数层面排除。
		argv = append(argv, "--tmpfs", filepath.Join(s.Base, "instances"))
	}
	for _, sw := range s.SharedWrite {
		argv = append(argv, "--bind-try", sw, sw)
	}
	for _, lf := range s.LockFiles {
		argv = append(argv, "--bind", lf, lf)
	}
	if s.RuntimeDir != "" {
		argv = append(argv, "--bind", s.RuntimeDir, s.RuntimeDir)
	}
	for _, sock := range s.RuntimeSock {
		argv = append(argv, "--ro-bind-try", sock.Src, sock.Dst)
	}
	if s.RuntimeDir != "" {
		argv = append(argv, "--setenv", "XDG_RUNTIME_DIR", s.RuntimeDir)
	}
	if s.DBusAddress != "" {
		argv = append(argv, "--setenv", "DBUS_SESSION_BUS_ADDRESS", s.DBusAddress)
	}
	argv = append(argv, "--setenv", "HOME", s.Home)
	argv = append(argv, "--die-with-parent")
	argv = append(argv, "--")
	return append(argv, s.Command...), nil
}

// OverlayMounter 把"挂一个 overlay" abstract 出来：真实现要 /dev/fuse，
// 而 CI/沙箱里没有。判定逻辑（argv、污染断言、归并）与它无关，必须能单独测。
type OverlayMounter interface {
	// Available 做 §3.2 的前置检查 + 冒烟（静态检查在容器里会假阳性）。
	Available() error
	Mount(lower, upper, work, merged string) (unmount func() error, err error)
}

// SessionRunner 跑"被探测的那一次启动"。
type SessionRunner interface {
	Run(ctx context.Context, argv []string, stderr io.Writer) (exitCode int, err error)
}

// ProbeOptions 是探测的全部可调项。
type ProbeOptions struct {
	Command  []string // 应用命令（Exec + 参数）
	LockFile []string // L0 预填的锁文件（可空）
	Stderr   io.Writer
	Mounter  OverlayMounter
	Runner   SessionRunner
	// Timeout 是整个会话的硬上限：用户开了应用就不管了的情况必须有尽头，
	// 否则探测会话会一直挂着 overlay 挂载点。0 = 用默认 30 分钟。
	Timeout time.Duration
}

// ProbeResult 是一次探测的产出。
type ProbeResult struct {
	Profile     Profile     `json:"-"`
	Candidates  []Candidate `json:"candidates"`
	Whiteouts   []string    `json:"whiteouts"`
	Quarantined string      `json:"quarantined,omitempty"` // 非空 = 保留现场的目录
	ExitCode    int         `json:"session_exit_code"`
}

const probeTimeoutDefault = 30 * time.Minute

// Probe 跑一次 overlay-diff 探测并产出档案（docs/discover.md §3 全节）。
//
// 顺序是契约，不能改：
//  1. 四道闸门（非 root、应用没在跑、overlay 可用、写层位置合法）；
//  2. 挂 overlay（写层在 /var/tmp，**不在 $HOME 里**，否则 ELOOP）；
//  3. 起会话（私有 runtime/dbus/锁文件，setid 独立会话）；
//  4. 会话结束 → 先杀进程组、再停 dbus、再卸载；
//  5. **然后**收集 upper、做污染检查（先删再查 = 断言永远为空）；
//  6. 干净 → 删 probe/；有污染 → 改名 quarantine-*、结果作废、报错。
func (e *Env) Probe(ctx context.Context, app string, opt ProbeOptions) (ProbeResult, error) {
	if err := CheckNotRoot(); err != nil {
		return ProbeResult{}, err
	}
	if err := ValidateAppKey(app); err != nil {
		return ProbeResult{}, err
	}
	if len(opt.Command) == 0 {
		return ProbeResult{}, errInput("err.probe.no_command", map[string]any{"app": app})
	}
	mounter := opt.Mounter
	if mounter == nil {
		mounter = fuseOverlayMounter{}
	}
	runner := opt.Runner
	if runner == nil {
		runner = setsidRunner{}
	}
	return e.probeSession(ctx, app, opt, mounter, runner)
}

// probeSession 是去掉"非 root"闸门之后的探测本体：root 检查与它无关，
// 而单测必然以 root 跑（沙箱里没有别的用户），所以拆开才能测到编排逻辑。
func (e *Env) probeSession(ctx context.Context, app string, opt ProbeOptions, mounter OverlayMounter, runner SessionRunner) (ProbeResult, error) {
	// 闸门 2：目标应用不能正在跑。主实例在跑时应用会把参数转发给它然后自己退出，
	// 探测结果是"零写入"——与"应用真的不写盘"无法区分（§3.2.2）。
	if running, err := e.AppRunning(filepath.Base(opt.Command[0])); err != nil {
		return ProbeResult{}, err
	} else if running {
		return ProbeResult{}, errState("err.probe.app_running", map[string]any{"exec": filepath.Base(opt.Command[0])})
	}
	// 闸门 3/4：overlay 可用 + 写层位置合法。
	if err := mounter.Available(); err != nil {
		return ProbeResult{}, err
	}

	work, err := e.newProbeWorkDir(app)
	if err != nil {
		return ProbeResult{}, err
	}
	// 从这一刻起，任何失败都要走清理；cleanup 自己判断"该删还是该留"。
	cleanup := func(keep bool) string {
		if keep {
			// 同一 /var/tmp 下改名保留（§3.2.1）。名字带 uid：多个用户的现场
			// 混在一个目录里，谁都不敢删。
			q := filepath.Join("/var/tmp",
				fmt.Sprintf("appclone-quarantine-%d-%s", e.UID, time.Now().UTC().Format("20060102T150405")))
			if err := os.Rename(work, q); err == nil {
				return q
			}
		}
		os.RemoveAll(work)
		return ""
	}
	upper := filepath.Join(work, "upper")
	merged := filepath.Join(work, "merged")
	runtimeDir := filepath.Join(work, "runtime")
	unmount, err := mounter.Mount(e.Resolver.Home, upper, filepath.Join(work, "work"), merged)
	if err != nil {
		cleanup(false)
		return ProbeResult{}, err
	}
	// 卸载必须在收集之前、在进程收完之后：先卸载才能保证 upper 不再被写，
	// 顺序反了会 EBUSY。
	defer func() {
		if unmount != nil {
			_ = unmount()
		}
	}()

	bus, stopBus := e.startProbeBus(filepath.Join(work, "dbus"))
	if stopBus != nil {
		defer stopBus()
	}
	spec := ProbeSpec{
		Home:        e.Resolver.Home,
		RealHome:    e.Resolver.RealHome,
		Base:        e.Base,
		Merged:      merged,
		RuntimeDir:  runtimeDir,
		RuntimeSock: probeRuntimeSocks(e.Resolver.XDG["RUNTIME_DIR"], runtimeDir),
		DBusAddress: bus,
		PublicDirs:  e.Resolver.PublicDirs(),
		SharedWrite: e.probeSharedWrites(),
		LockFiles:   opt.LockFile,
		Command:     opt.Command,
	}
	argv, err := ProbeBwrapArgs(spec)
	if err != nil {
		cleanup(false)
		return ProbeResult{}, err
	}

	timeout := opt.Timeout
	if timeout == 0 {
		timeout = probeTimeoutDefault
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exitCode, runErr := runner.Run(sctx, argv, opt.Stderr)
	// 卸载（defer 里那次是兜底），然后才收集。
	_ = unmount()
	unmount = nil

	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
		q := cleanup(false)
		_ = q
		return ProbeResult{}, newErr(CodeEnv, "err.probe.session", map[string]any{"app": app}, runErr)
	}

	// 收集：先读，再判，最后才谈删（§3.2.3 末条）。
	entries, present, err := readUpperTree(upper)
	if err != nil {
		cleanup(false)
		return ProbeResult{}, err
	}
	if !present {
		cleanup(false)
		return ProbeResult{}, errEnv("err.probe.no_upper", map[string]any{"path": upper}, nil)
	}
	contaminated := e.ProbeContaminated(entries, spec.PublicDirs)
	if len(contaminated) > 0 {
		// 公共目录出现在 upper 里 = 摘出没生效，用户文件可能已经落进去。
		// 结果整体作废，且**绝不删**：宁可让用户重跑，也不能带着"可能删过文件"
		// 的结果去建实例。
		q := cleanup(true)
		return ProbeResult{
			Quarantined: q,
			Whiteouts:   nil,
		}, errEnv("err.probe.contaminated", map[string]any{
			"paths": strings.Join(contaminated, "、"), "quarantine": q,
		}, nil)
	}

	cands, whiteouts := e.Resolver.MergeUpper(entries)
	p := e.Resolver.ProfileFromCandidates(app, "overlay", cands)
	p.Unsupported = append(p.Unsupported, probeUnsupported(spec)...)
	p.Ignored = DefaultIgnored()
	sort.Slice(p.Unsupported, func(i, j int) bool { return p.Unsupported[i].Path < p.Unsupported[j].Path })
	p.LockFiles = probeLockFiles(spec)

	cleanup(false)
	return ProbeResult{
		Profile:    p,
		Candidates: cands,
		Whiteouts:  whiteouts,
		ExitCode:   exitCode,
	}, nil
}

// ProbeContaminated 是 §3.2.1 的反向断言：公共目录的路径出现在 upper 里，
// 说明"摘出"没生效（或应用绕过了 bind）。**纯函数**，与删不删现场分开。
func (e *Env) ProbeContaminated(entries []UpperEntry, publicDirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, en := range entries {
		abs := filepath.Clean(filepath.Join(e.Resolver.Home, filepath.FromSlash(en.Path)))
		for _, pub := range publicDirs {
			if !IsUnder(pub, abs) {
				continue
			}
			if !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
			break
		}
	}
	sort.Strings(out)
	return out
}

// probeSharedWrites 是探测期要 bind 回真实的共享可写文件：隔离它们会让
// 输入法/书签在会话里失效，从而"探测不到写入"（§3.2.1 第三条）。
func (e *Env) probeSharedWrites() []string {
	var out []string
	for _, p := range []string{
		filepath.Join(e.Resolver.XDG["DATA_HOME"], "recently-used.xbel"),
		filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "gtk-3.0", "bookmarks"),
		filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "gtk-4.0", "bookmarks"),
		filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "fcitx5"),
	} {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// probeUnsupported 记录"文件路径判定不了"的东西：凭证在 DBus 服务里。
// 探测期看得到 keyring 文件的写入，但把它当普通配置隔离就是假隔离。
func probeUnsupported(s ProbeSpec) []Unsupported {
	var out []Unsupported
	for _, p := range []string{
		filepath.Join(s.RealHome, ".local", "share", "keyrings"),
	} {
		out = append(out, Unsupported{Kind: "keyring", Path: p, Reason: "probe.why.keyring"})
	}
	return out
}

// probeLockFiles 只保留**普通文件**：socket 走私有 runtime，bind 空文件盖住
// socket 会 ENOTSOCK（§6.2 的老事故）。
func probeLockFiles(s ProbeSpec) []string {
	var out []string
	for _, lf := range s.LockFiles {
		if st, err := os.Stat(lf); err == nil && st.Mode().IsRegular() {
			out = append(out, lf)
		}
	}
	return out
}

// newProbeWorkDir 建探测写层目录，并做三条"缺一不挂"的断言（§3.2）：
// upper 不能嵌在 lower 里、work 与 upper 同文件系统、三个目录都空。
func (e *Env) newProbeWorkDir(app string) (string, error) {
	root, err := os.MkdirTemp("/var/tmp", fmt.Sprintf("appclone-probe-%d-", e.UID))
	if err != nil {
		return "", errEnv("err.env.mkdir", map[string]any{"path": "/var/tmp"}, err)
	}
	for _, sub := range []string{"upper", "work", "merged", "runtime", "dbus"} {
		if err := os.Mkdir(filepath.Join(root, sub), 0o700); err != nil {
			os.RemoveAll(root)
			return "", errEnv("err.env.mkdir", map[string]any{"path": filepath.Join(root, sub)}, err)
		}
	}
	// ① upper 不能在 lower（有效 HOME）里：overlay 的 upper 落在自己的 lower 下会
	// 让视图递归，`ls merged/<更深层>` 直接 ELOOP，而且删不掉。
	up := Real(filepath.Join(root, "upper"))
	lower := Real(e.Resolver.Home)
	if IsUnder(lower, up) || up == lower {
		os.RemoveAll(root)
		return "", errEnv("err.probe.workdir_in_home", map[string]any{"path": up, "home": lower}, nil)
	}
	return root, nil
}

// AppRunning 判定"目标应用是否已有进程在跑"（§3.2.2）。
//
// 按 exe 的 basename 比对 /proc/*/exe：comm 只有 15 字符、会被截断（
// `org.telegram.desktop` 这类必然误判）。已经在本工具 mount namespace 里跑的
// 分身进程要排除，否则用户开着分身就永远无法探测主实例。
func (e *Env) AppRunning(name string) (bool, error) {
	if name == "" || name == "." || name == "/" {
		return false, nil
	}
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return false, nil // 读不到 namespace 就退化成"不排除"，不要因此挡住探测
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false, errEnv("err.env.read_dir", map[string]any{"path": "/proc"}, err)
	}
	for _, ent := range ents {
		if !ent.IsDir() || ent.Name()[0] < '0' || ent.Name()[0] > '9' {
			continue
		}
		pid := ent.Name()
		if pid == fmt.Sprint(os.Getpid()) {
			continue
		}
		ns, err := os.Readlink(filepath.Join("/proc", pid, "ns/mnt"))
		if err != nil || ns != self {
			continue // 名字空间不同：那是分身（或别的容器），不是主实例
		}
		exe, err := os.Readlink(filepath.Join("/proc", pid, "exe"))
		if err != nil {
			continue
		}
		if filepath.Base(exe) == name {
			return true, nil
		}
	}
	return false, nil
}

// probeRuntimeSocks 把宿主 socket 重新指向私有 runtime 里的同名路径：
// 探测期 XDG_RUNTIME_DIR 是空的私有目录，不逐个引入的话会话里没有图形界面，
// 应用直接起不来，探测结果就是"零写入"。
func probeRuntimeSocks(hostRuntime, probeRuntime string) []BindPair {
	socks := FilterExisting(RuntimeSocketCandidates(hostRuntime))
	for i := range socks {
		rel, ok := relUnder(hostRuntime, socks[i].Src)
		if !ok {
			continue
		}
		socks[i].Dst = filepath.Join(probeRuntime, rel)
	}
	return socks
}

// startProbeBus 起探测期的私有会话总线（与运行期同一份生命周期实现）。
func (e *Env) startProbeBus(dir string) (addr string, stop func()) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		return "", nil // 降级：没有 dbus-daemon 就不注入地址
	}
	sock := filepath.Join(dir, "bus")
	cmd := exec.Command(daemon, "--session", "--nofork", "--address=unix:path="+sock, "--print-address")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil
	}
	if err := cmd.Start(); err != nil {
		return "", nil
	}
	line, err := readFirstLine(stdout)
	if err != nil || line == "" {
		_ = cmd.Process.Kill()
		return "", nil
	}
	return line, func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.Remove(sock)
	}
}

// readFirstLine 读第一行（dbus-daemon 的 --print-address）。
func readFirstLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	return strings.TrimSpace(line), err
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
	// `-o auto_unmount`：core 被 SIGKILL 时内核帮忙卸载，否则挂载点变成
	// `Transport endpoint is not connected`，下次挂载直接 EBUSY（§3.2.3）。
	// `-f` 前台运行：只有前台我们才拿得到进程句柄，退出时才能确定它真的没了。
	cmd := exec.Command(bin,
		"-f",
		"-o", "lowerdir="+lower,
		"-o", "upperdir="+upper,
		"-o", "workdir="+work,
		"-o", "auto_unmount",
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
			// 进程已退出：挂载失败，把它自己的 stderr 交出去。
			return nil, errEnv("err.probe.mount_failed", map[string]any{
				"output": strings.TrimSpace(errBuf.String()),
			}, nil)
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	return nil, errEnv("err.probe.mount_failed", map[string]any{
		"output": "超时（5 秒内没看到挂载点）：" + strings.TrimSpace(errBuf.String()),
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

// mounted 判挂载点是否已经生效（能 stat 到且不是空目录的假象）。
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

// setsidRunner 起独立会话并跑会话命令；退出时对**整个进程组**发信号。
type setsidRunner struct{}

func (setsidRunner) Run(ctx context.Context, argv []string, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 0, errInput("err.probe.no_command", nil)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, errEnv("err.probe.start_failed", map[string]any{"exec": argv[0]}, err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		// setsid 失败或读早了会拿到调用者自己的 pgid，`kill(-pgid)` 会把终端
		// 整个会话杀掉（灾难级误杀）。断言不过只杀单进程。
		defer func() { _ = cmd.Process.Kill() }()
		return 0, errEnv("err.probe.setsid_failed", map[string]any{"exec": argv[0]}, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		terminateGroup(cmd.Process.Pid)
		<-done
	case err := <-done:
		terminateGroup(cmd.Process.Pid) // 收掉应用 fork 出去的后代
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		if err != nil {
			return 0, err
		}
	}
	return 0, nil
}
