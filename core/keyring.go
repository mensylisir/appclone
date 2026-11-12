package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// keyringPwBytes 是每个实例 keyring 密码的随机字节数（方案 §5.1）。32 字节
// 足够抵抗离线猜测，hex 后 64 字符，写进 keyring.pw（0600）。
const keyringPwBytes = 32

// keyringSession 是一个私有 keyring daemon 的生命周期，与 dbusSession 同构：
// 唯一的 Wait 由内部 goroutine 完成，Stop() 只发信号并等 Done，绝不重复 Wait。
type keyringSession struct {
	cmd  *exec.Cmd
	log  *boundedLog
	done chan struct{}
	// runDir 是 daemon 的 XDG_RUNTIME_DIR：Stop 时删掉它留下的 socket，
	// 否则下次 run 复用同一目录会撞上残留的 control/ssh socket。
	runDir string
	// cleanup 在进程退出后执行（探测期临时目录）；run 会话为 nil。
	cleanup func()
}

// Done 在 daemon 退出后关闭；nil session 返回已关闭的 channel。
func (s *keyringSession) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Stop 终止 daemon：SIGTERM → 2 秒 → SIGKILL → 等 Done。顺序与 dbusSession
// 一致，调用方不再自己发信号。
func (s *keyringSession) Stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(time.Second):
		}
	}
	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
	cleanupKeyringRuntime(s.runDir)
}

// cleanupKeyringRuntime 删掉 daemon 在 XDG_RUNTIME_DIR 下留下的 socket
// （gnome-keyring 的 control/ssh）。与 dbusSession.Stop 同构：目录会被下一次
// run 复用，残留 socket 可能让新 daemon 以为已有实例在跑或直接拒绝启动。
// 只删 socket 类型，绝不递归删除目录内容。
func cleanupKeyringRuntime(runDir string) {
	if runDir == "" {
		return
	}
	dir := filepath.Join(runDir, "keyring")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		p := filepath.Join(dir, ent.Name())
		if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(p)
		}
	}
	_ = os.Remove(dir) // 只在空时成功
}

// Log 返回 daemon 的有界输出（诊断用）。密码只走 stdin，不会出现在这里。
func (s *keyringSession) Log() string {
	if s == nil || s.log == nil {
		return ""
	}
	return s.log.String()
}

// boundedLog 是 stdout/stderr 的有界环形缓冲：daemon 可能持续输出，不排空会
// 把管道写满、把 daemon 卡死（禁止死等）；全量保留又会在长时间运行后吃光内存。
type boundedLog struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newBoundedLog(max int) *boundedLog { return &boundedLog{max: max} }

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte{}, b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *boundedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// keyringDir 是实例内 keyring 状态的根（方案 §4.2）。
func keyringDir(in Instance) string { return filepath.Join(in.Dir(), "keyring") }

// keyringDataDir 是实例自己的 keyring 文件目录，也是沙箱内两个 bind 目标的
// 共同来源（XDG_DATA_HOME/keyrings 与 XDG_CONFIG_HOME/keyrings）。
func keyringDataDir(in Instance) string {
	return filepath.Join(keyringDir(in), "data", "keyrings")
}

// EnsureKeyring 建实例的 keyring 目录与密码。规则（方案 §5.1）：
//   - 目录 0700；密码文件 0600，32 字节随机 hex，O_CREAT|O_EXCL；
//   - **密码一旦创建绝不删除、绝不重写**：重写等于让已有 keyring 文件永久打不开；
//   - 已有 keyring 文件但缺密码 → err.env.keyring_pw_missing（不猜、不重置）；
//   - 失败回滚只在"没有 keyring 文件"时回收本次新建的空目录。
func (e *Env) EnsureKeyring(in Instance) error {
	root := keyringDir(in)
	dirs := []string{
		keyringDataDir(in),
		filepath.Join(root, "config"),
		filepath.Join(root, "run"),
		filepath.Join(root, "home"),
	}
	var created []string
	rollback := func() {
		if dirHasEntries(keyringDataDir(in)) {
			return // 已有 keyring 文件：绝不回收（可能正是用户的数据）
		}
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i]) // 只删空目录
		}
	}
	for _, d := range dirs {
		if _, err := os.Lstat(d); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			rollback()
			return errEnv("err.env.mkdir", map[string]any{"path": d}, err)
		}
		made, err := mkdirAllTracked(d, 0o700)
		created = append(created, made...)
		if err != nil {
			// 并发创建（两个 run 同时起同一实例）会让 Mkdir 得到 EEXIST：
			// 目标已经是目录就算成功，不把它当成失败。
			if fi, serr := os.Lstat(d); serr == nil && fi.IsDir() {
				continue
			}
			rollback()
			return errEnv("err.env.mkdir", map[string]any{"path": d}, err)
		}
	}

	pwPath := filepath.Join(root, "keyring.pw")
	if fi, err := os.Lstat(pwPath); err == nil {
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
			return errEnv("err.env.keyring_pw_missing", map[string]any{"path": pwPath}, nil)
		}
		if fi.Size() > 0 {
			return nil
		}
		// 0 字节密码文件是"创建后、写入前被 SIGKILL"的残留。只有 keyring
		// 数据目录也为空时才敢重建（没有钥匙可丢）；有数据就按缺密码处理，
		// 绝不猜、绝不覆盖。
		if dirHasEntries(keyringDataDir(in)) {
			return errEnv("err.env.keyring_pw_missing", map[string]any{"path": pwPath}, nil)
		}
		if rerr := os.Remove(pwPath); rerr != nil {
			return errEnv("err.env.write", map[string]any{"path": pwPath}, rerr)
		}
	} else if !os.IsNotExist(err) {
		return errEnv("err.env.keyring_pw_missing", map[string]any{"path": pwPath}, err)
	}
	if dirHasEntries(keyringDataDir(in)) {
		// 有 keyring 文件却没有密码：任何"补一个"都是猜，猜错会让文件永久打不开。
		return errEnv("err.env.keyring_pw_missing", map[string]any{"path": pwPath}, nil)
	}

	pw, err := newKeyringPassword()
	if err != nil {
		rollback()
		return errEnv("err.env.write", map[string]any{"path": pwPath}, err)
	}
	f, err := os.OpenFile(pwPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil && os.IsExist(err) {
		// 并发创建：对方可能刚好在写。只接受非空文件；0 字节说明对方也
		// 死在写入前，删掉重试一次（此时数据目录必为空，重建是安全的）。
		if data, rerr := os.ReadFile(pwPath); rerr == nil && strings.TrimSpace(string(data)) != "" {
			return nil
		}
		_ = os.Remove(pwPath)
		f, err = os.OpenFile(pwPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		rollback()
		return errEnv("err.env.write", map[string]any{"path": pwPath}, err)
	}
	_, werr := f.WriteString(pw + "\n")
	cerr := f.Close()
	if werr != nil || cerr != nil {
		// 未写完的密码文件不是"已创建的密码"：删掉，避免留下一个永远解不开的实例。
		_ = os.Remove(pwPath)
		rollback()
		if werr != nil {
			return errEnv("err.env.write", map[string]any{"path": pwPath}, werr)
		}
		return errEnv("err.env.write", map[string]any{"path": pwPath}, cerr)
	}
	return nil
}

// newKeyringPassword 生成 32 字节随机密码的 hex 形态。
func newKeyringPassword() (string, error) {
	raw := make([]byte, keyringPwBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// probeKeyringRoot 找一个**不在 AC_BASE 下**的临时目录（方案 §6）：探测期
// AC_BASE 不被遮蔽，把临时 keyring 放进去会污染实例数据。先试系统临时目录，
// 再依次试 $XDG_RUNTIME_DIR 下的候选；全部不安全才放弃。
func (e *Env) probeKeyringRoot() (string, error) {
	base := e.BuildOptions().BaseReal
	safe := func(d string) bool {
		if base == "" {
			return true
		}
		r := Real(d)
		return r != base && !IsUnder(base, r)
	}
	if d, err := os.MkdirTemp("", "appclone-probe-keyring-"); err == nil {
		if safe(d) {
			return d, nil
		}
		_ = os.RemoveAll(d)
	}
	if rd := e.Resolver.XDG["RUNTIME_DIR"]; filepath.IsAbs(rd) && dirWritable(rd) {
		for i := 0; i < 2; i++ {
			d := filepath.Join(rd, fmt.Sprintf("appclone-probe-keyring-%d-%d", os.Getpid(), i))
			if !safe(d) {
				continue
			}
			if err := os.MkdirAll(d, 0o700); err != nil {
				continue
			}
			return d, nil
		}
	}
	return "", errEnv("err.env.mkdir", map[string]any{"path": "appclone-probe-keyring"}, nil)
}

// startProbeKeyring 为探测会话起临时 keyring daemon（方案 §6）。失败只告警、
// 探测继续：没有它，libsecret 应用会在 ServiceUnknown 处提前退出，读证据
// 可能不完整，但"探测失败"比"证据少一点"更糟。
func (e *Env) startProbeKeyring(busAddr string, opt ProbeOptions) *keyringSession {
	if busAddr == "" {
		return nil
	}
	cap := e.keyringCaps()
	if !cap.Available {
		warnProbeKeyring(opt, "缺少 gnome-keyring-daemon 或 dbus-send")
		return nil
	}
	root, err := e.probeKeyringRoot()
	if err != nil {
		warnProbeKeyring(opt, "没有安全的临时目录")
		return nil
	}
	pw, err := newKeyringPassword()
	if err != nil {
		_ = os.RemoveAll(root)
		warnProbeKeyring(opt, "无法生成临时密码")
		return nil
	}
	sess, err := startKeyringDaemon(cap.Daemon, cap.DbusSend, busAddr, root, pw)
	if err != nil {
		_ = os.RemoveAll(root)
		warnProbeKeyring(opt, "daemon 未就绪")
		return nil
	}
	sess.cleanup = func() { _ = os.RemoveAll(root) }
	return sess
}

func warnProbeKeyring(opt ProbeOptions, why string) {
	if opt.Stderr != nil {
		fmt.Fprintf(opt.Stderr, "appclone: 未启用探测期临时密钥环（%s），读证据可能不完整\n", why)
	}
}

// readKeyringPassword 读实例密码。空文件/读失败都按"缺密码"处理：拿着空密码
// 去起 daemon 只会得到一个必然失败的会话。
func readKeyringPassword(in Instance) (string, error) {
	pwPath := filepath.Join(keyringDir(in), "keyring.pw")
	data, err := os.ReadFile(pwPath)
	if err != nil {
		return "", errEnv("err.env.keyring_pw_missing", map[string]any{"path": pwPath}, err)
	}
	pw := strings.TrimSpace(string(data))
	if pw == "" {
		return "", errEnv("err.env.keyring_pw_missing", map[string]any{"path": pwPath}, nil)
	}
	return pw, nil
}

// KeyringCapability 是给 GUI 的能力查询入口：与 run 共用同一份结论与测试注入。
func (e *Env) KeyringCapability() KeyringCaps { return e.keyringCaps() }

// keyringCaps 是能力结论的唯一取数点：测试可注入，产品路径走固定目录解析。
func (e *Env) keyringCaps() KeyringCaps {
	if e != nil && e.KeyringCapsOverride != nil {
		return *e.KeyringCapsOverride
	}
	return KeyringCapability()
}

// checkKeyringCapability 是 run 与 CreateInstance 共用的能力检查（方案 §6）：
// 缺 daemon 或缺 dbus-send 都直接失败，不静默退回宿主 keyring。
func (e *Env) checkKeyringCapability() error {
	c := e.keyringCaps()
	if c.Daemon == "" {
		return errEnv("err.env.no_keyring_daemon", nil, nil)
	}
	if c.DbusSend == "" {
		return errEnv("err.env.no_dbus_send", nil, nil)
	}
	return nil
}

// shBin 返回启动 daemon 用的 POSIX shell 绝对路径。GUI 从 .desktop 启动时
// PATH 可能极简（只有 /usr/bin 甚至更少），不能依赖 exec.Command("sh") 的
// PATH 解析；/bin/sh 在主流发行版（含 NixOS）都存在。
func shBin() string {
	for _, p := range []string{"/bin/sh", "/usr/bin/sh"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "sh" // 极端环境退回 PATH 解析
}

// startKeyringDaemon 起一个私有 keyring daemon 并等到就绪。root 是它的状态根
// （run 传 <inst>/keyring，probe 传临时目录），布局与实例目录一致。
//
// 环境完全重建：不继承用户 PATH、DBUS_*、XDG_*。密码只走 stdin，不出现在
// argv、环境或日志里。stdout/stderr 由有界环形缓冲持续排空。
func startKeyringDaemon(daemon, dbusSend, busAddr, root, pw string) (*keyringSession, error) {
	if daemon == "" {
		return nil, errEnv("err.env.no_keyring_daemon", nil, nil)
	}
	if dbusSend == "" {
		return nil, errEnv("err.env.no_dbus_send", nil, nil)
	}
	for _, d := range []string{
		filepath.Join(root, "data", "keyrings"),
		filepath.Join(root, "config"),
		filepath.Join(root, "run"),
		filepath.Join(root, "home"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, errEnv("err.env.mkdir", map[string]any{"path": d}, err)
		}
	}
	cmd := exec.Command(shBin(), "-c", `umask 077; exec "$@"`, "sh",
		daemon, "--foreground", "--unlock", "--components=secrets")
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "home"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"DBUS_SESSION_BUS_ADDRESS=" + busAddr,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	// 与 dbusSession 同理：GUI 崩溃/收到 SIGTERM 时没有机会 Stop，让内核收掉。
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	log := newBoundedLog(8192)
	cmd.Stdout, cmd.Stderr = log, log
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errEnv("err.env.no_keyring_daemon", nil, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, errEnv("err.env.no_keyring_daemon", nil, err)
	}
	sess := &keyringSession{cmd: cmd, log: log, done: make(chan struct{}), runDir: filepath.Join(root, "run")}
	go func() {
		_ = cmd.Wait()
		close(sess.done)
	}()
	// 65 字节远小于管道缓冲，写入不会阻塞；daemon 已退出时得到 EPIPE。
	if _, werr := stdin.Write([]byte(pw + "\n")); werr != nil {
		sess.Stop()
		return nil, errEnv("err.env.keyring_not_ready", map[string]any{"reason": "stdin"}, werr)
	}
	_ = stdin.Close()
	if rerr := waitKeyringReady(dbusSend, busAddr); rerr != nil {
		sess.Stop()
		return nil, rerr
	}
	return sess, nil
}

// waitKeyringReady 是就绪三重（方案 §5）：NameHasOwner → ReadAlias →
// Unlock。每步 2 秒、按**返回内容**轮询：daemon 刚启动时先是没有总线名、
// 然后名字有了但集合还没解锁，任何"调用成功即通过"的写法都会把启动竞态
// 误报成密钥环损坏。
func waitKeyringReady(dbusSend, busAddr string) error {
	// 刚 Start 的 daemon 还没抢到总线名：必须轮询而不是一次判死
	// （一次 NameHasOwner 会把启动竞态误报成"密钥环不可用"）。
	_, ok := pollDbus(dbusSend, busAddr, 2*time.Second,
		func(out string) bool { return strings.Contains(out, "boolean true") },
		"org.freedesktop.DBus", "/org/freedesktop/DBus",
		"org.freedesktop.DBus.NameHasOwner", "string:org.freedesktop.secrets")
	if !ok {
		return errEnv("err.env.keyring_not_ready", map[string]any{"reason": "service"}, nil)
	}
	collection := ""
	if out, ok := pollDbus(dbusSend, busAddr, 2*time.Second,
		func(out string) bool { return lastObjectPath(out) != "" },
		"org.freedesktop.secrets", "/org/freedesktop/secrets",
		"org.freedesktop.Secret.Service.ReadAlias", "string:default"); ok {
		collection = lastObjectPath(out)
	}
	// `/` 是 ReadAlias 无默认别名时的哨兵值，不是集合对象：同样退回 login，
	// 否则 Unlock 会拿 `array:objpath:/` 去调用，永远等不到解锁数组。
	if collection == "" || collection == "/" {
		collection = "/org/freedesktop/secrets/collection/login"
	}
	// Unlock 是 **Service 对象**上的方法（不是集合对象上的），参数是
	// object path 数组：`array:objpath:<collection>`。写成 `objpath:` 会被
	// 当成错误签名，真实 dbus-send 直接返回 InvalidArgs/UnknownMethod。
	// 集合已存在但还没解锁时，调用会**成功返回**空数组 + prompt 路径，
	// 所以必须按内容轮询，等解锁数组真的非空。
	out, ok := pollDbus(dbusSend, busAddr, 2*time.Second, keyringUnlocked,
		"org.freedesktop.secrets", "/org/freedesktop/secrets",
		"org.freedesktop.Secret.Service.Unlock", "array:objpath:"+collection)
	if !ok {
		reason := "locked"
		if strings.TrimSpace(out) == "" {
			reason = "unlock" // 连调用都没成功（签名/方法名/服务错误）
		}
		return errEnv("err.env.keyring_not_ready", map[string]any{"reason": reason}, nil)
	}
	return nil
}

// keyringUnlocked 判 Unlock 的 dbus-send 回复：prompt 为 "/" 且解锁数组
// 非空 = 已解锁；数组为空或 prompt 非 "/" 都还是锁定态。
func keyringUnlocked(out string) bool {
	paths := objectPaths(out)
	return len(paths) >= 2 && paths[len(paths)-1] == "/"
}

// pollDbus 在 timeout 内反复调用，直到 ready 对**调用结果内容**成立或超时。
// 只看"调用成功"不够：NameHasOwner 在 daemon 还没抢到总线名时会成功返回
// `boolean false`——那正是启动竞态，必须继续等（每次调用自身有 2 秒上限）。
func pollDbus(bin, bus string, timeout time.Duration, ready func(string) bool, dest, path, method string, args ...string) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		out, ok := dbusSendCall(context.Background(), bin, bus, dest, path, method, args...)
		if ok && ready(out) {
			return out, true
		}
		if time.Now().After(deadline) {
			return out, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dbusSendCall 是一次带 2 秒上限的 dbus-send 调用。
func dbusSendCall(ctx context.Context, bin, bus, dest, path, method string, args ...string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	argv := []string{"--bus=" + bus, "--print-reply", "--dest=" + dest, path, method}
	argv = append(argv, args...)
	cmd := exec.CommandContext(cctx, bin, argv...)
	out, err := cmd.Output()
	return string(out), err == nil
}

// objectPaths 提取 dbus-send 输出里的全部 object path（按出现顺序）。
func objectPaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := objectPathOnLine(line); ok {
			paths = append(paths, p)
		}
	}
	return paths
}

// lastObjectPath 取最后一个 object path（ReadAlias 的返回值）。
func lastObjectPath(out string) string {
	paths := objectPaths(out)
	if len(paths) == 0 {
		return ""
	}
	return paths[len(paths)-1]
}

func objectPathOnLine(line string) (string, bool) {
	const prefix = "object path "
	i := strings.Index(line, prefix)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimSpace(line[i+len(prefix):])
	if len(rest) < 2 || rest[0] != '"' {
		return "", false
	}
	end := strings.LastIndexByte(rest, '"')
	if end <= 0 {
		return "", false
	}
	return rest[1:end], true
}
