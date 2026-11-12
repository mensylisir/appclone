package core

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// dbusSockMax 是私有总线 socket 路径的安全上限。AF_UNIX 的 sun_path 是 108
// 字节，但 dbus-daemon 自己更严：实测 99 字节可用、100 字节即报
// "Socket name too long"。这里再留一段余量，避免不同版本/客户端差异。
const dbusSockMax = 90

// dbusSession 是一个私有会话总线的生命周期。**唯一**的 Wait 由内部 goroutine
// 完成：调用方只通过 Done() 观察退出，Stop() 不会重复 Wait（重复 Wait 会
// 直接报 "wait: no child processes"，并把正常停止误判成错误）。
type dbusSession struct {
	cmd  *exec.Cmd
	addr string
	sock string
	// dir 非空表示 socket 用了短路径回退目录（见 busSocketPath），Stop 时一并
	// 删掉。它只放一个 socket 文件，删空目录即可，不做 RemoveAll。
	dir  string
	done chan struct{}
}

// Addr 是 --print-address 的原文（含 guid）。宿主侧工具（dbus-send、keyring
// daemon）必须用它；应用侧地址见 BuildOptions.DBusAppAddress。
func (s *dbusSession) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Sock 是 dbus-daemon 实际监听的 socket 路径（宿主侧）。正常就是
// <busDir>/bus；路径过长时是短回退目录里的 bus。BuildBwrapArgs 必须用它
// 作为 bind 源，不能自己拼 <inst>/dbus/bus。
func (s *dbusSession) Sock() string {
	if s == nil {
		return ""
	}
	return s.sock
}

// Done 在 daemon 退出后关闭。nil session 返回一个已关闭的 channel。
func (s *dbusSession) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Stop 终止 daemon：SIGTERM → 2 秒 → SIGKILL → 等 Done，然后删 socket。
// 调用方不再自己发信号，避免两处逻辑分叉。
func (s *dbusSession) Stop() {
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
	_ = os.Remove(s.sock)
	if s.dir != "" {
		_ = os.Remove(s.dir) // 只在空目录时成功：不碰任何意外内容
	}
}

// AppDBusAddress 是写进沙箱的私有总线地址：命名空间内的路径，按 D-Bus
// 地址规则转义。run 与 doctor 必须用同一个构造函数，否则打印与执行分叉。
func AppDBusAddress(runtimeDir string) string {
	return "unix:path=" + escapeDBusAddressValue(filepath.Join(runtimeDir, "bus"))
}

// unavailableBusAddress 是探测总线起不来时注入的地址：指向一个不存在的
// 私有路径。**绝不能留空**——ProbeBwrapArgs 只在地址非空时 setenv，空值
// 会让探测应用继承宿主 DBUS_SESSION_BUS_ADDRESS，反而连回宿主 keyring。
func unavailableBusAddress(busDir string) string {
	return "unix:path=" + escapeDBusAddressValue(filepath.Join(busDir, "bus.unavailable"))
}

// hostBusPaths 取宿主会话总线里**确实存在且是 socket** 的 unix 路径。
// 只解析地址不够：陈旧/畸形的 DBUS_SESSION_BUS_ADDRESS 很常见，把不存在的
// 路径交给 bwrap 会在宿主创建 0444 空文件（工具在声明范围外写用户目录），
// 目标是目录时整条启动失败。所以遮蔽清单必须在构造 argv 之前按真实文件过滤。
//
// 通过的路径再 EvalSymlinks：bwrap 拒绝在**最终分量是软链**的目标上挂载
// （"Can't mount on symlink destination"），而地址里出现软链别名很常见
// （用户自建、容器/发行版布局）。解析成真实 socket 后，沙箱里的软链仍指向
// 该真实路径，遮蔽照样生效。
//
// stat 与 bwrap 之间 socket 消失的窗口仍在：那会在宿主留下一个 0444 占位
// 文件。窗口极小、fail-closed、不影响安全，接受。EvalSymlinks 失败时保留
// 字面路径：它是真实 socket 时遮蔽照常生效，是软链时 bwrap 拒绝挂载、整条
// run 失败——失败方向是拒绝启动，不是放行。
func (e *Env) hostBusPaths() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range parseDBusUnixPaths(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode()&os.ModeSocket == 0 {
			continue
		}
		if real, rerr := filepath.EvalSymlinks(p); rerr == nil {
			p = real
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// parseDBusUnixPaths 解析 D-Bus 地址里的所有 unix:path= 条目。地址由 `;`
// 分隔，单个条目形如 `unix:path=/tmp/dbus-xxxx,guid=...`，值按 D-Bus 规则
// 百分号编码。`unix:abstract=` 不是文件路径，不参与遮蔽。
func parseDBusUnixPaths(addr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, entry := range strings.Split(addr, ";") {
		entry = strings.TrimSpace(entry)
		if !strings.HasPrefix(entry, "unix:") {
			continue
		}
		for _, kv := range strings.Split(strings.TrimPrefix(entry, "unix:"), ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k != "path" {
				continue
			}
			p, err := url.PathUnescape(v)
			if err != nil || !filepath.IsAbs(p) {
				continue
			}
			p = filepath.Clean(p)
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// startSessionBus 起一个私有会话总线。busDir 就是放 session.conf 与 bus 的
// 目录（不自动追加子目录）：run 传 <inst>/dbus，probe 传 <work>/dbus。
//
// 配置是自写的最小 busconfig：没有 <servicedir>/<standard_session_servicedirs/>，
// 因此请求 org.freedesktop.secrets 之类的名字不会激活宿主服务——这是
// 「私有总线」与「换个 socket 的宿主总线」的分界线（ADR-035）。
func (e *Env) startSessionBus(busDir string) (*dbusSession, error) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		return nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	if err := os.MkdirAll(busDir, 0o700); err != nil {
		return nil, errEnv("err.env.mkdir", map[string]any{"path": busDir}, err)
	}
	sock, sockDir, err := e.busSocketPath(busDir)
	if err != nil {
		return nil, err
	}
	// 上一轮被 kill 留下的 socket 文件会让 dbus-daemon 直接失败。
	_ = os.Remove(sock)
	conf := filepath.Join(busDir, "session.conf")
	if err := writeSessionConf(conf, sock); err != nil {
		dropSockDir(sockDir)
		return nil, err
	}

	cmd := exec.Command(daemon, "--config-file", conf, "--nofork", "--print-address")
	// GUI 崩溃或收到 SIGTERM 时没有机会跑 Stop：让内核在父进程消失时把
	// 私有总线也收掉。Pdeathsig 绑定创建它的线程，而 Go 从不销毁 OS 线程，
	// 所以不会出现"进程还活着、线程没了"的误杀。
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	// daemon 的环境也要重建：它会继承给被激活的服务（本配置没有 servicedir，
	// 但第二道防线不能省）。PATH 固定，不读用户 PATH。
	cmd.Env = sessionBusEnv(busDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		dropSockDir(sockDir)
		return nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	if err := cmd.Start(); err != nil {
		dropSockDir(sockDir)
		return nil, errEnv("err.env.no_dbus_daemon", nil, err)
	}
	line, rdone, rerr := readFirstLine(stdout, 2*time.Second)
	if rerr != nil || strings.TrimSpace(line) == "" {
		_ = cmd.Process.Kill()
		// 先关读端：假 daemon 若把 stdout 继承给了子进程，光 Kill 它不会
		// 让 ReadString 返回；关掉读端再等读 goroutine 退出，避免每次失败
		// 的总线启动都泄漏一个 goroutine/fd。
		_ = stdout.Close()
		select {
		case <-rdone:
		case <-time.After(time.Second):
		}
		_ = cmd.Wait()
		dropSockDir(sockDir)
		if errors.Is(rerr, errBusAddrTimeout) {
			return nil, errEnv("err.env.dbus_timeout", map[string]any{"s": 2}, rerr)
		}
		return nil, errEnv("err.env.no_dbus_daemon", nil, rerr)
	}
	_ = stdout.Close() // 地址已经拿到，daemon 之后不再往 stdout 写

	sess := &dbusSession{
		cmd:  cmd,
		addr: strings.TrimSpace(line),
		sock: sock,
		dir:  sockDir,
		done: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(sess.done)
	}()
	return sess, nil
}

// busSocketPath 决定私有总线 socket 的落点。正常就是 <busDir>/bus；但
// dbus-daemon 对 socket 路径有上限（实测 99 字节可用、100 字节报
// "Socket name too long"），AC_BASE/TMPDIR 很长时（测试临时目录、长用户名）
// 会直接起不来。此时退到短目录：优先 $XDG_RUNTIME_DIR（登录即清理），
// 否则 /tmp。目录名由 busDir 哈希决定：同一实例复用同一目录，不会随运行
// 次数堆积；每次启动前已删旧 socket。
//
// 返回的第二个值非空表示用了回退目录，Stop 时删掉。
func (e *Env) busSocketPath(busDir string) (sock, fallbackDir string, err error) {
	sock = filepath.Join(busDir, "bus")
	if len(sock) <= dbusSockMax {
		return sock, "", nil
	}
	sum := sha256.Sum256([]byte(busDir))
	name := fmt.Sprintf("appclone-bus-%x", sum[:6])
	var roots []string
	if rd := e.Resolver.XDG["RUNTIME_DIR"]; filepath.IsAbs(rd) && dirWritable(rd) {
		roots = append(roots, rd)
	}
	if tmp := os.TempDir(); dirWritable(tmp) {
		roots = append(roots, tmp)
	}
	for _, root := range roots {
		// /tmp 是全局可写目录：名字里带 uid，避免不同用户互相撞名。
		// 属主判据用**进程真实 uid**：e.UID 是路径语义上的 uid，测试里可能
		// 与进程不同（与 sweepStaleAudits 的判据一致）。
		uid := os.Getuid()
		dir := filepath.Join(root, fmt.Sprintf("%s-%d", name, uid))
		if len(filepath.Join(dir, "bus")) > dbusSockMax {
			continue
		}
		if err := ensureBusDir(dir, uid); err != nil {
			return "", "", err
		}
		return filepath.Join(dir, "bus"), dir, nil
	}
	return "", "", errEnv("err.env.dbus_path_too_long", map[string]any{"path": sock}, nil)
}

// ensureBusDir 建（或复用）回退目录。它落在 /tmp 这类全局可写目录里，
// 所以**不跟随软链**：先 Mkdir（对已存在的软链返回 EEXIST），再 Lstat
// 确认是真目录、属主是自己、权限不含 group/other。
func ensureBusDir(dir string, uid int) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return errEnv("err.env.mkdir", map[string]any{"path": dir}, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return errEnv("err.env.mkdir", map[string]any{"path": dir}, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() || !ok || int(st.Uid) != uid {
		return errEnv("err.env.bus_dir_unsafe", map[string]any{"path": dir}, nil)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return errEnv("err.env.chmod", map[string]any{"path": dir}, err)
		}
	}
	return nil
}

// dropSockDir 清掉启动失败时留下的空回退目录；正常目录（fallbackDir 为空）
// 不动，它属于调用方的工作目录。
func dropSockDir(dir string) {
	if dir != "" {
		_ = os.Remove(dir)
	}
}

// sessionBusEnv 是 dbus-daemon 的运行环境：HOME/XDG_RUNTIME_DIR 指到总线
// 目录，PATH 固定，语言固定。不继承用户 PATH 与任何 DBUS_* 变量。
func sessionBusEnv(busDir string) []string {
	return []string{
		"HOME=" + busDir,
		"XDG_RUNTIME_DIR=" + busDir,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
}

// writeSessionConf 写最小 busconfig。没有 servicedir 是有意为之：一旦有
// <standard_session_servicedirs/>，请求 org.freedesktop.secrets 会激活宿主
// keyring daemon（继承 dbus-daemon 的宿主环境），独立 keyring 直接失效。
func writeSessionConf(path, sock string) error {
	// 路径先按 D-Bus 地址规则转义，再按 XML 转义（两者都要：地址语法认 %XX，
	// XML 认 & < > "）。实测带空格的路径在 <listen> 里必须写成 %20。
	addr := "unix:path=" + escapeDBusAddressValue(sock)
	conf := fmt.Sprintf(`<busconfig>
  <type>session</type>
  <listen>%s</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow user="*"/>
    <allow own="*"/>
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow receive_sender="*" eavesdrop="true"/>
  </policy>
</busconfig>
`, xmlEscape(addr))
	if err := writeFileAtomic(path, []byte(conf), 0o600); err != nil {
		return errEnv("err.env.write", map[string]any{"path": path}, err)
	}
	return nil
}

// escapeDBusAddressValue 按 D-Bus 地址规则转义值：只有
// A-Z a-z 0-9 - _ / . 原样保留，其余字节（空格、中文、`,`、`;`、`%`、`*`）
// 写成 %XX。多转义几个字符是合法的：客户端会解码，少转义才会断地址。
func escapeDBusAddressValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '/', c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// errBusAddrTimeout 区分"daemon 超时不出地址"与"daemon 立刻退出/没输出"：
// 两者排障方向完全不同（等太久 vs 根本没起来）。
var errBusAddrTimeout = errors.New("dbus-daemon 在限定时间内没有输出地址")

// readFirstLine 读第一行（dbus-daemon 的 --print-address）。带超时：
// 它不输出时不能把整个会话挂在这里（AGENTS：禁止死等）。返回的 done 在读
// goroutine 退出后关闭，调用方超时后必须等它，避免读 goroutine 泄漏。
func readFirstLine(r io.Reader, timeout time.Duration) (string, <-chan struct{}, error) {
	type res struct {
		line string
		err  error
	}
	done := make(chan struct{})
	ch := make(chan res, 1)
	go func() {
		defer close(done)
		line, err := bufio.NewReader(r).ReadString('\n')
		ch <- res{strings.TrimSpace(line), err}
	}()
	select {
	case v := <-ch:
		return v.line, done, v.err
	case <-time.After(timeout):
		return "", done, errBusAddrTimeout
	}
}
