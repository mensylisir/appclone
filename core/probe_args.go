package core

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProbeSpec 是"探测会话要 bind 什么"的全部输入（docs/discover.md §3.2.4）。
// 单独抽出来是为了让 argv 构造成为纯函数：这条命令一旦顺序写错，后果是
// "用户存进 ~/Downloads 的文件被删"或者"探测零写入"，都必须能断言。
type ProbeSpec struct {
	Home         string            // 有效 HOME（AC_HOME 或真实 HOME）
	Base         string            // AC_BASE（探测期要 bind 回去，否则工具自己的写入会进 upper）
	Merged       string            // overlay 的 merged 视图
	RuntimeDir   string            // 探测期私有 XDG_RUNTIME_DIR
	RuntimeSock  []BindPair        // 宿主 socket：Src 在宿主，Dst 在探测私有 runtime 里
	DBusAddress  string            // 私有会话总线地址；产品路径总会注入（不可用时注入不存在的私有路径）
	HostBusPaths []string          // 宿主总线 unix socket（DBUS_SESSION_BUS_ADDRESS）；非空时用 /dev/null 遮蔽
	PublicBinds  []BindPair        // 公共目录：Src = 宿主真实目录，Dst = 有效 HOME 下的目标
	SharedWrite  []string          // 共享可写文件（输入法/书签），尽量 bind 回真实
	LockBinds    []BindPair        // 锁文件：Src = 探测期空文件，Dst = 宿主锁路径
	Env          map[string]string // 额外注入的环境变量（字典序输出）
	// NoOverlay 是 strace 降级模式：不罩 overlay（HOME 就是真实的），
	// 因此也没有"把公共目录绑回去"这回事——那句话的前提是 overlay 罩住了它们。
	NoOverlay bool
	// TraceDir 是会话 FIFO 所在目录；非空时在 --bind / / 之后用 --tmpfs 遮蔽，
	// 防止沙箱内看到宿主 strace 的 FIFO/日志（ADR-034）。
	TraceDir string
	Command  []string // 应用命令（argv[0] 之后全部原样）
	// AuditRoot 非空时在 Base 之后 tmpfs 遮蔽审计写层根（存在才由调用方填）。
	AuditRoot string
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
//	⑨ 锁文件（bind 探测期的空文件，盖住宿主上的真实锁）
//	⑩ 私有 runtime + 宿主 socket 只读引入
//	⑪ --setenv XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS / HOME
//	⑫ --die-with-parent（兜底；主保证是 pgid kill）
//	⑬ -- 应用命令
func ProbeBwrapArgs(s ProbeSpec) ([]string, error) {
	if s.Home == "" || !filepath.IsAbs(s.Home) {
		return nil, errInternal("err.internal.probe_spec", map[string]any{"field": "home"}, nil)
	}
	if !s.NoOverlay && (s.Merged == "" || !filepath.IsAbs(s.Merged)) {
		return nil, errInternal("err.internal.probe_spec", map[string]any{"field": "merged"}, nil)
	}
	if len(s.Command) == 0 {
		return nil, errInput("err.probe.no_command", nil)
	}
	argv := []string{
		"bwrap",
		"--unshare-user", "--unshare-pid", "--unshare-ipc",
		"--bind", "/", "/",
	}
	if s.TraceDir != "" {
		argv = append(argv, "--tmpfs", s.TraceDir)
	}
	argv = append(argv,
		"--dev-bind", "/dev", "/dev",
		"--proc", "/proc",
	)
	if !s.NoOverlay {
		argv = append(argv, "--bind", s.Merged, s.Home)
		for _, pb := range s.PublicBinds {
			argv = append(argv, "--bind", pb.Src, pb.Dst)
		}
	}
	// 宿主会话总线遮蔽：探测期必须让应用只看到私有总线（起不来时看到的是
	// 不存在的私有地址），否则 dbus-launch 放在 /tmp 的总线经 `--bind / /`
	// 仍然可连，读证据/keyring 证据会被宿主会话污染。位置必须在
	// --dev-bind/--proc 之后：宿主总线若在 /dev/shm，先遮蔽会被随后的
	// dev-bind 覆盖回去。HostBusPaths 已过滤为真实 socket（含软链解析），
	// 因此用 `--bind`；stat 与 bwrap 之间的极小竞态窗口由 fail-closed 接受。
	if s.DBusAddress != "" {
		for _, pth := range s.HostBusPaths {
			argv = append(argv, "--bind", "/dev/null", pth)
		}
	}
	if s.Base != "" {
		argv = append(argv, "--bind", s.Base, s.Base)
		// 探测期不需要、也不该看见已有分身：把 instances/ 换成空 tmpfs，
		// 否则"探测会写进主实例目录"这件事没法从参数层面排除。
		argv = append(argv, "--tmpfs", filepath.Join(s.Base, "instances"))
	}
	// 审计写层根对探测会话同样不可见：里面是其他分身尚未 verify 的证据副本。
	if s.AuditRoot != "" {
		argv = append(argv, "--tmpfs", s.AuditRoot)
	}
	for _, sw := range s.SharedWrite {
		argv = append(argv, "--bind-try", sw, sw)
	}
	for _, lb := range s.LockBinds {
		argv = append(argv, "--bind", lb.Src, lb.Dst)
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
	for _, k := range sortedKeys(s.Env) {
		argv = append(argv, "--setenv", k, s.Env[k])
	}
	argv = append(argv, "--die-with-parent")
	argv = append(argv, "--")

	// 正向断言（§3.2.1 要求，反向断言证明不了"bind 真的生效"）：
	// 在会话里先跑一段守卫脚本，比对每个公共目录与 $HOME 的**设备号**——
	// overlay 是 fuse 设备，bind 成功的真实目录在底层文件系统上，两者必然不同；
	// 如果相同，说明 bind 没生效、这个目录仍然落在 overlay 里。
	//
	// 为什么不用"写标记文件再回读"：那要往用户的 ~/Downloads 里写东西，
	// 中途被杀就留下一个残留文件。设备号比较不改任何用户数据。
	if len(s.PublicBinds) > 0 && !s.NoOverlay {
		argv = append(argv, "/bin/sh", "-c", probeBindGuard, "--")
	}
	return append(argv, s.Command...), nil
}

// probeBindGuard 是会话内的正向断言脚本。退出码 91 = bind 没生效。
const probeBindGuard = `IFS='
'
h=$(stat -c %d -- "$HOME" 2>/dev/null) || h=
[ -n "$h" ] || exec "$@"
for d in $AC_PROBE_PUBLICS; do
  [ -n "$d" ] || continue
  v=$(stat -c %d -- "$d" 2>/dev/null) || continue
  [ "$v" = "$h" ] && exit 91
done
exec "$@"
`

// probeGuardFailed 是守卫脚本约定的失败退出码（应用自己不会返回它：
// 真返回 91 时也只多报一次"bind 未生效"，比漏报安全）。
const probeGuardFailed = 91

// probeGuardEnv 把要校验的公共目录传给守卫脚本。用换行分隔而不是空格：
// `My Documents`、`下载` 这类名字带空格时，空格分隔会被 word splitting 拆碎。
func probeGuardEnv(binds []BindPair) map[string]string {
	if len(binds) == 0 {
		return nil
	}
	targets := make([]string, 0, len(binds))
	for _, b := range binds {
		targets = append(targets, b.Dst)
	}
	return map[string]string{"AC_PROBE_PUBLICS": strings.Join(targets, "\n")}
}

// publicTargets 给错误文案用（只列目标，不列真实源）。
func publicTargets(binds []BindPair) []string {
	out := make([]string, 0, len(binds))
	for _, b := range binds {
		out = append(out, b.Dst)
	}
	return out
}

// sortedKeys 让 --setenv 的输出稳定（map 迭代顺序随机，同一条命令两次不同）。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// probePublicBinds 解析并校验公共目录，产出"真实源 → 有效 HOME 目标"的映射。
//
// 三条硬约束（§3.2.1）：
//   - **不许回退到八个英文默认名**：`user-dirs.dirs` 读不到就禁用探测（3）。
//     回退在中文系统上会漏掉 `~/下载`，用户存的文件照样落进 upper 被删。
//   - 每项必须是有效 HOME 的**严格后代**：`/`、`/home`、HOME 本身，或 HOME 的祖先
//     一旦被 bind，会把整个 overlay 遮掉，探测变成在真实家目录里跑。
//   - 不存在的目录不 bind：`--bind` 的源不存在会让整条 bwrap 命令失败，
//     而"用户没建 Templates 目录"是最正常的情况。
func (e *Env) probePublicBinds() ([]BindPair, error) {
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "user-dirs.dirs")
	f, err := os.Open(cfg)
	if err != nil {
		return nil, errEnv("err.probe.no_user_dirs", map[string]any{"path": cfg}, err)
	}
	defer f.Close()

	configured := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		if !strings.HasSuffix(k, "_DIR") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		v = expandKnownVars(v, e.Resolver.Vars())
		if !filepath.IsAbs(v) || strings.ContainsRune(v, '$') {
			continue
		}
		configured[strings.TrimSuffix(strings.TrimPrefix(k, "XDG_"), "_DIR")] = filepath.Clean(v)
	}
	if err := sc.Err(); err != nil {
		return nil, errEnv("err.probe.no_user_dirs", map[string]any{"path": cfg}, err)
	}

	home := filepath.Clean(e.Resolver.Home)
	keys := []string{"DESKTOP", "DOCUMENTS", "DOWNLOAD", "MUSIC", "PICTURES", "PUBLICSHARE", "TEMPLATES", "VIDEOS"}
	var out []BindPair
	for _, k := range keys {
		pub, ok := configured[k]
		if !ok || pub == "" {
			continue // 这一项用户没配：不是错误，跳过
		}
		// HOME 可能是软链，user-dirs.dirs 里也可能写真实路径；用 looseRel
		// 两种形态都认，否则合法配置会被硬判成 bad_public_dir、整个探测被禁用。
		base, rel, under := looseRel(home, pub)
		if !under || rel == "" {
			// 指向家目录之外或就是家目录本身：禁用探测，不要"猜一个"。
			return nil, errInput("err.probe.bad_public_dir", map[string]any{"path": pub, "home": home})
		}
		if st, err := os.Stat(pub); err != nil || !st.IsDir() {
			continue // 目录还没建：没必要 bind，也不会有人往这里存文件
		}
		out = append(out, BindPair{Src: pub, Dst: filepath.Join(base, rel)})
	}
	return out, nil
}

// probeLockBinds 把 L0 预填的锁文件换成"探测期空文件 → 宿主锁路径"的绑定。
// 只处理普通文件：socket 走私有 runtime，bind 空文件盖住 socket 会 ENOTSOCK。
func (e *Env) probeLockBinds(work string, lockFiles []string) ([]BindPair, func(), error) {
	dir := filepath.Join(work, "locks")
	var out []BindPair
	for _, lf := range lockFiles {
		abs, err := e.Resolver.Expand(lf)
		if err != nil {
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, func() {}, errEnv("err.env.mkdir", map[string]any{"path": dir}, err)
		}
		empty := filepath.Join(dir, lockFileName(abs))
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			return nil, func() {}, errEnv("err.env.write", map[string]any{"path": empty}, err)
		}
		out = append(out, BindPair{Src: empty, Dst: abs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dst < out[j].Dst })
	return out, func() {}, nil
}

// lockHostPaths 收档案里的 lock_files：探测期盖住的是宿主路径（Dst）。
func (e *Env) lockHostPaths(binds []BindPair) []string {
	var out []string
	for _, b := range binds {
		out = append(out, e.Resolver.ToStoreForm(b.Dst))
	}
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
// 路径用档案的 store 形态（`~/…`）：候选派生（ProfileFromCandidates）与模板
// 写出的都是这种形态，混用绝对路径会让同一目录在合并时变成两条。
func probeUnsupported(e *Env) []Unsupported {
	return []Unsupported{{
		Kind: "keyring",
		// 与 normalizeKeyring 共用同一个取数点：用户设置了 XDG_DATA_HOME 时，
		// 写死 ~/.local/share/keyrings 会让同一份档案出现两条 keyring 条目。
		Path:   e.keyringUnsupportedPath(),
		Reason: "probe.why.keyring",
	}}
}
