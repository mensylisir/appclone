package core

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// BuildOptions 是 BuildBwrapArgs / ValidateProfile 的全部外部输入。
// 它存在的方式就是"纯函数"的保证：函数自己不读环境变量、不探文件系统，
// 存在性与权限由 PrepareBindTargets 前置完成（§3.2 第 2、3 条）。
type BuildOptions struct {
	Resolver     *PathResolver // 路径展开的唯一实现（§6.4）
	RealHome     string        // 真实 $HOME（宿主路径）
	Home         string        // 有效 HOME：AC_HOME 或 RealHome
	UID          int
	Base         string     // AC_BASE 绝对路径
	BaseReal     string     // AC_BASE 的真实路径（EvalSymlinks；空时退回 Base）
	RuntimeDir   string     // 宿主 XDG_RUNTIME_DIR
	RuntimeSocks []BindPair // 已探测存在的宿主 socket → 私有 runtime 内目标（只读）
	DBusAddress  string     // dbus-daemon --print-address 原文；空 = 不隔离
	// DBusAppAddress 是写进沙箱的地址（命名空间内路径，已按 D-Bus 规则转义）。
	// 空时退回 DBusAddress（探测会话不遮 AC_BASE，仍可用宿主原文）。
	DBusAppAddress string
	// BusSock 是 dbus-daemon 实际监听的 socket（宿主路径）。空时按实例目录
	// 约定拼 <inst>/dbus/bus；socket 路径过长时会退到短目录，必须用会话给出
	// 的真实路径（core/dbus_session.go 的 busSocketPath）。
	BusSock string
	// HostBusPaths 是宿主会话总线的 unix socket 路径（DBUS_SESSION_BUS_ADDRESS
	// 里所有 unix:path= 条目，已按 D-Bus 规则解码、去重）。私有总线在跑时
	// 逐个用 /dev/null 遮蔽：`--bind / /` 会让 /tmp/dbus-XXXX 之类的宿主总线
	// 在沙箱里仍可连，应用能绕过私有总线。
	HostBusPaths []string
	XDG          map[string]string
	// Audit 非空时在 ④ 与 ⑤ 之间插入审计层（只有 `run --audit` 会设置它）。
	Audit *AuditLayers
	// ForkSources 把 paths 的 bind 源从实例目录换成 overlay 挂载点
	// （`add --fork` 的实例，§8.1）。键是 normalizePaths 之后的宿主绝对路径；
	// 没有条目的路径仍绑定实例目录。BuildBwrapArgs 仍是纯函数：挂载由
	// core.Run 在调用前完成，这里只消费结果。
	ForkSources map[string]string
	// KeyringTargets 是本次实际要 bind 的 keyring 目标（由 BuildOptions 用
	// 文件系统现状解析：软链目标解析后与同组另一个目标指向同一目录时跳过，
	// 因为 bwrap 拒绝在软链目标上挂载）。空时退回 keyringTargets() 的两个
	// 原始目标，保持 BuildBwrapArgs 对纯函数测试的可用性。
	KeyringTargets []string
	// AuditRoot 是所有审计写层的根（<tmpRoot>/appclone/audit.<uid>）。run 与
	// 探测都无条件 tmpfs 遮蔽它：普通 run 的 `--bind / /` 同样能按真实路径
	// 读到/改掉其他分身尚未 verify 的 upper 副本。
	AuditRoot string
}

// BindPair 表示一次 bind 的源与目标。
type BindPair struct{ Src, Dst string }

// 保留环境变量由 core 产出，档案里出现即判非法（§3.1 ⑫）。
var reservedEnvKeys = map[string]bool{
	"HOME":                     true,
	"DBUS_SESSION_BUS_ADDRESS": true,
}

func isReservedEnv(k string) bool {
	return reservedEnvKeys[k] || strings.HasPrefix(k, "XDG_")
}

// ValidateProfile 校验档案本身是否自洽、是否触碰红线。
// 无写副作用（只读 user-dirs.dirs 取公共目录清单），因此可以被表驱动测试反复调用。
func ValidateProfile(p Profile, o BuildOptions) error {
	if p.Version > ProfileVersion {
		return errInput("err.profile.version_ahead", map[string]any{
			"app": p.App, "version": p.Version, "current": ProfileVersion,
		})
	}
	if strings.TrimSpace(p.Exec) == "" {
		return errInput("err.profile.no_exec", map[string]any{"app": p.App})
	}
	// HOME/真实 HOME/runtime/XDG 根落在 AC_BASE 里时，`--tmpfs Base` 会把
	// 它们自己盖掉（实例目录、keyring、私有总线全在 Base 下），运行必坏。
	if err := checkBaseContainers(o); err != nil {
		return err
	}
	if !filepath.IsAbs(p.Exec) {
		return errInput("err.profile.exec_relative", map[string]any{"exec": p.Exec})
	}
	// 字段码必须在解析成档案时剥掉：留着会让应用收到一个名叫 "%U" 的参数（§4.1 末条）。
	if strings.Contains(p.Exec, "%") {
		return errInput("err.profile.exec_field_code", map[string]any{"exec": p.Exec})
	}
	for k := range p.Env {
		if isReservedEnv(k) {
			return errInput("err.profile.reserved_env", map[string]any{"key": k, "app": p.App})
		}
	}

	paths, err := normalizePaths(p.Paths, o)
	if err != nil {
		return err
	}
	p.Paths = paths
	// keyring 目录不能进 paths：keyring 的隔离由独立 daemon 负责（ADR-035）。
	// 把宿主 keyring 文件当普通路径 bind 进分身，等于让两个实例共享同一份密钥，
	// 与 isolate_keyring 的语义直接冲突。
	for _, pp := range p.Paths {
		// 只拒两个 keyring 目录：paths 支持精确文件，`.keyring` 后缀不能单独
		// 判定（应用自己的 `~/.config/app/foo.keyring` 是合法文件）。
		if o.Resolver != nil && o.Resolver.isKeyringDir(pp) {
			return errInput("err.profile.keyring_in_paths", map[string]any{"path": pp})
		}
	}
	if err := ValidateReadTrace(p.ReadTrace); err != nil {
		return err
	}
	if err := ValidateCredentialPaths(p, o); err != nil {
		return err
	}

	// lock_files 与 sockets 不能是同一路径：一个 bind 空文件、一个靠私有 runtime，
	// 混着写会让实现只能二选一，另一条静默失效（§6.2 末尾的 ENOTSOCK 事故）。
	for _, lf := range p.LockFiles {
		abs, err := o.expand(lf)
		if err != nil {
			return err
		}
		if err := checkBaseContained(abs, o, "lock_files"); err != nil {
			return err
		}
		for _, sk := range p.Sockets {
			sabs, err := o.expand(sk)
			if err != nil {
				return err
			}
			if abs == sabs {
				return errInput("err.profile.lock_is_socket", map[string]any{"path": abs})
			}
		}
	}

	for _, b := range p.ExtraBinds {
		src, err := o.expand(b.Src)
		if err != nil {
			return err
		}
		dst, err := o.expand(b.Dst)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(src) {
			return errInput("err.profile.extra_bind_relative", map[string]any{"src": b.Src})
		}
		// 源同样要查：`--bind "$HOME" /mnt/home` 会让应用经 /mnt/home/...
		// 重新读到 AC_BASE（实例数据、keyring、私有总线 socket）。
		if err := checkBaseExposure(src, o, "extra_binds"); err != nil {
			return err
		}
		if err := checkDangerous(dst, o, "extra_binds"); err != nil {
			return err
		}
		for _, pp := range p.Paths {
			pabs, err := o.expand(pp)
			if err != nil {
				return err
			}
			if dst == pabs {
				// 同一目标被两条规则 bind：档案自相矛盾，谁在后谁生效，
				// 用户看到的隔离结果取决于实现顺序（§3.1 ⑦）。
				return errInput("err.profile.extra_bind_overlap", map[string]any{"path": dst})
			}
		}
	}

	// 独立 keyring 的两个 bind 目标不得与 AC_BASE 重叠：目标落在被 tmpfs
	// 遮蔽的范围内时，bind 会把实例数据重新暴露成"宿主 keyring 目录"。
	// 源在 Base 下是正常布局（<inst>/keyring/data/keyrings）。
	if p.IsolateKeyring {
		// 原始目标与解析后的目标都要查：双侧软链可以指向 Base 下一个**尚不存在**
		// 的目录，词法检查只看软链位置会放行，而 keyringBinds 最终 bind 的是
		// 解析后的真实路径（Real 解析不到末级时逐级回退，仍可能落在 Base 下）。
		checked := map[string]bool{}
		for _, tgt := range append(o.keyringTargets(), o.keyringBinds()...) {
			if checked[tgt] {
				continue
			}
			checked[tgt] = true
			if !filepath.IsAbs(tgt) {
				return errInternal("err.internal.probe_spec", map[string]any{"field": "xdg"}, nil)
			}
			if err := checkBaseExposure(tgt, o, "isolate_keyring"); err != nil {
				return err
			}
		}
	}

	// ignored 与 shared_writes 是 verify 的白名单：写一个 `~` 或 `/` 进去，
	// `doctor --verify` 就会对**任何**越界写入都说 verified——最危险的一类假通过。
	// 这两项过去只被读取、从没被校验（§5.5"非法值报错"）。
	//
	// 判定必须同时挡住**家目录本身与它的所有祖先**：只挡 `/` 和 `~` 的话，
	// `ignored = ["/home"]`（家目录的父目录）照样放行整个主目录。而且要比
	// 词法与真实两种形态——`Expand` 返回的是解析过软链的路径，HOME 本身是软链时
	// 纯词法比较连 `~` 都挡不住。
	home := ""
	if o.Home != "" {
		home = filepath.Clean(o.Home)
	}
	unsupportedPaths := make([]string, 0, len(p.Unsupported))
	for _, u := range p.Unsupported {
		if u.Path != "" {
			unsupportedPaths = append(unsupportedPaths, u.Path)
		}
	}
	for _, field := range []struct {
		name string
		list []string
	}{
		{"ignored", p.Ignored},
		{"shared_writes", p.SharedWrites},
		// unsupported 同样是 verify 的放行名单（§6.5 把它并进 allowed），
		// 所以它必须和上面两项接受同一套 blanket 校验；只挡前两项的话，
		// `unsupported = [{path="~"}]` 会让任何越界都判 verified。
		{"unsupported", unsupportedPaths},
	} {
		for _, raw := range field.list {
			abs, err := o.expand(raw)
			if err != nil {
				return err
			}
			// 根目录单独判：relUnder("/", x) 拼前缀会得到 "//"，把它漏掉。
			if abs == "/" {
				return errInput("err.profile.blanket_path", map[string]any{"path": raw, "field": field.name})
			}
			if home != "" && isUnderLoose(abs, home) {
				// abs 是 home 本身或它的祖先：等于"全部放行"。
				return errInput("err.profile.blanket_path", map[string]any{"path": raw, "field": field.name})
			}
			if o.RealHome != "" && isUnderLoose(abs, filepath.Clean(o.RealHome)) {
				return errInput("err.profile.blanket_path", map[string]any{"path": raw, "field": field.name})
			}
		}
	}

	// 同一路径不能既隔离又共享：bwrap 里两条 bind 的先后决定谁生效，
	// 结果取决于参数顺序 —— 那是"看起来设置过、实际不确定"的状态（§3.1 ⑦）。
	for _, pp := range p.Paths {
		pabs, err := o.expand(pp)
		if err != nil {
			return err
		}
		for _, sw := range p.SharedWrites {
			sabs, err := o.expand(sw)
			if err != nil {
				return err
			}
			if pabs == sabs || IsUnder(pabs, sabs) || IsUnder(sabs, pabs) {
				return errInput("err.profile.isolate_and_share", map[string]any{
					"path": pabs, "other": sabs,
				})
			}
		}
	}
	// shared_writes 的源会被 --bind-try 原样绑回真实位置：源是 Base 的祖先
	// （如 `$HOME/.local/share`）时，应用经共享项重新读到实例目录。
	for _, sw := range p.SharedWrites {
		sabs, err := o.expand(sw)
		if err != nil {
			return err
		}
		if err := checkBaseExposure(sabs, o, "shared_writes"); err != nil {
			return err
		}
	}
	return nil
}

// normalizePaths 展开、去重、把父子重叠归一到最上层项。
//
// "最上层"而不是"最具体"：`paths` 里的每一项都会被 bind 到实例目录的同名子路径，
// 父目录已经覆盖子目录，保留子项只会多一条冗余 bind；反过来若只留子项，
// 父目录的其余内容就留在了共享层——那才是真正的漏隔离。
// ValidateReadTrace 只接受四种取值；旧档案的 "" 表示未知。
func ValidateReadTrace(v string) error {
	switch v {
	case "", "ok", "partial", "unavailable":
		return nil
	}
	return errInput("err.profile.read_trace", map[string]any{"value": v})
}

// ValidateCredentialPaths 校验 credential_paths ⊆ paths（相等或位于其下）。
// 词法优先、Real 兜底（§8.4）：软链 HOME 下词法在 paths 内就接受，不因真实
// 路径在外而拒绝加载。
func ValidateCredentialPaths(p Profile, o BuildOptions) error {
	paths, err := normalizePaths(p.Paths, o)
	if err != nil {
		return err
	}
	var lexPaths []string
	if o.Resolver != nil {
		for _, raw := range p.Paths {
			if abs, lerr := o.Resolver.expandLexical(raw); lerr == nil {
				lexPaths = append(lexPaths, abs)
			}
		}
	}
	for _, raw := range p.CredentialPaths {
		if o.Resolver != nil {
			if lex, lerr := o.Resolver.expandLexical(raw); lerr == nil {
				if underAnyLexical(lexPaths, lex) {
					continue
				}
			}
		}
		abs, err := o.expand(raw)
		if err != nil {
			return err
		}
		ok := false
		for _, parent := range paths {
			if abs == parent || IsUnder(parent, abs) {
				ok = true
				break
			}
		}
		if !ok {
			return errInput("err.profile.credential_outside_paths", map[string]any{
				"credential": abs,
			})
		}
	}
	return nil
}

func underAnyLexical(paths []string, child string) bool {
	for _, p := range paths {
		if _, ok := relUnder(p, child); ok {
			return true
		}
	}
	return false
}

func normalizePaths(raw []string, o BuildOptions) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	abs := make([]string, 0, len(raw))
	for _, r := range raw {
		p, err := o.expand(r)
		if err != nil {
			return nil, err
		}
		if err := checkDangerous(p, o, "paths"); err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		abs = append(abs, p)
	}
	sort.Slice(abs, func(i, j int) bool { return pathLess(abs[i], abs[j]) })
	out := make([]string, 0, len(abs))
	for _, p := range abs {
		covered := false
		for _, kept := range out {
			if IsUnder(kept, p) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out, nil
}

// checkDangerous 是项目第一原则的代码化身：这些路径一旦进 paths，
// 用户就会在"两个分身各自一份下载目录"和"家目录被整体偷换"之间二选一。
func checkDangerous(abs string, o BuildOptions, field string) error {
	if !filepath.IsAbs(abs) {
		return errInput("err.path.not_absolute", map[string]any{"path": abs, "field": field})
	}
	if abs == "/" {
		return errInput("err.profile.root_path", map[string]any{"field": field})
	}
	// HOME 本身及其祖先：paths 里出现它等于把整个家目录（或包含家目录的树）
	// 隔离进实例。Expand 会把软链 HOME 解析成真实路径，所以词法与真实形态都
	// 必须判；o.RealHome 来自 os.UserHomeDir，也可能是词法值，同样要取 Real。
	home := filepath.Clean(o.Home)
	if home != "" {
		for _, h := range []string{home, Real(home)} {
			if abs == h || IsUnder(abs, h) {
				return errInput("err.profile.home_path", map[string]any{"field": field})
			}
		}
	}
	if o.RealHome != "" {
		for _, h := range []string{filepath.Clean(o.RealHome), Real(o.RealHome)} {
			if abs == h || IsUnder(abs, h) {
				return errInput("err.profile.home_path", map[string]any{"field": field})
			}
		}
	}
	// 公共目录之下的任何路径同样拒绝：`~/Downloads/work` 被隔离成实例私有，
	// 正是"两个分身各存一份下载"这种用户最不能接受的失误（§5 paths）。
	for _, pub := range o.publicDirs() {
		switch {
		case abs == pub:
			return errInput("err.profile.public_path", map[string]any{"path": pub, "field": field})
		case IsUnder(pub, abs):
			return errInput("err.profile.under_public_path", map[string]any{"path": abs, "public": pub, "field": field})
		case IsUnder(abs, pub):
			return errInput("err.profile.public_ancestor", map[string]any{"path": abs, "public": pub, "field": field})
		}
	}
	// AC_BASE 及其之下：把分身自己的运行时根写进 paths 会让 bind 自指
	// （instances/<app>/<id>/root 变成它自己的子目录）。paths 已经过 Expand
	// （真实形态），自定义 AC_BASE 若是软链，词法比较会漏判。
	//
	// 只拒「等于 / 之下」，**不拒祖先**：`--bind <inst>/root/<path> <path>`
	// 把目标替换成实例数据，Base 在沙箱里仍被 tmpfs 遮住；祖先形态是合法档案
	// （fork 的 lower/upper 边界用例依赖它）。真正会暴露 Base 的是绑定**源**，
	// 见 checkBaseExposure。
	if o.Base != "" {
		for _, base := range []string{o.Base, Real(o.Base)} {
			if abs == base || IsUnder(base, abs) {
				return errInput("err.profile.base_path", map[string]any{"path": abs, "field": field})
			}
		}
	}
	// 整个命名空间级目录（~/.config、~/.local/share、~/.local/state）**及其祖先**：
	// `~/.local` 是 DATA_HOME/STATE_HOME 的祖先，也是默认 AC_BASE 的祖先，
	// 隔离它会让"实例目录里的 bind 源"落在 bind 目标之内，形成自指。
	for _, pd := range o.protectedDirs() {
		if abs == pd || IsUnder(abs, pd) {
			return errInput("err.profile.protected_path", map[string]any{"path": abs, "field": field})
		}
	}
	return nil
}

// baseReal 返回 AC_BASE 的真实路径。BaseReal 未填时（测试或直接构造
// BuildOptions 的调用方）退回词法 Base；两者都空时返回 ""。
func (o BuildOptions) baseReal() string {
	if o.BaseReal != "" {
		return filepath.Clean(o.BaseReal)
	}
	if o.Base == "" {
		return ""
	}
	return filepath.Clean(o.Base)
}

// checkBaseExposure 拒绝能把 AC_BASE 重新带回命名空间的 bind **源**。
// 三向都拒：等于、祖先（`--bind "$HOME" /mnt/home` 会经 /mnt/home/...
// 读到实例目录）、子孙（直接指到 keyring/密码）。
//
// 只用于源，不用于 bind 目标：目标被替换成 bind 的内容，Base 仍被 tmpfs
// 遮住；对目标做祖先拒绝会误杀合法档案。
func checkBaseExposure(abs string, o BuildOptions, field string) error {
	base := o.baseReal()
	if base == "" {
		return nil
	}
	for _, cand := range []string{filepath.Clean(abs), Real(abs)} {
		if cand == base || IsUnder(cand, base) || IsUnder(base, cand) {
			return errInput("err.profile.base_exposure", map[string]any{
				"path": abs, "base": base, "field": field,
			})
		}
	}
	return nil
}

// checkBaseContained 只拒「等于 / 位于 Base 之下」：这类目标在沙箱里已被
// tmpfs 盖住，bind 会失败或写进虚空，必须在档案阶段就拒绝。
func checkBaseContained(abs string, o BuildOptions, field string) error {
	base := o.baseReal()
	if base == "" {
		return nil
	}
	for _, cand := range []string{filepath.Clean(abs), Real(abs)} {
		if cand == base || IsUnder(base, cand) {
			return errInput("err.profile.base_exposure", map[string]any{
				"path": abs, "base": base, "field": field,
			})
		}
	}
	return nil
}

// checkBaseContainers 拒绝把 HOME / 真实 HOME / runtime / 任一 XDG 根
// 放在 AC_BASE 里（等于或子孙）。它们是 Base 的**祖先**时是正常默认布局
// （AC_BASE=$HOME/.local/share/appclone），不能误拒。
func checkBaseContainers(o BuildOptions) error {
	base := o.baseReal()
	if base == "" {
		return nil
	}
	cands := []struct {
		field string
		path  string
	}{
		{"home", o.Home},
		{"real_home", o.RealHome},
		{"runtime", o.RuntimeDir},
	}
	for k, d := range o.XDG {
		cands = append(cands, struct {
			field string
			path  string
		}{"xdg_" + strings.ToLower(k), d})
	}
	for _, c := range cands {
		if c.path == "" {
			continue
		}
		for _, cand := range []string{filepath.Clean(c.path), Real(c.path)} {
			if cand == base || IsUnder(base, cand) {
				return errInput("err.profile.base_exposure", map[string]any{
					"path": c.path, "base": base, "field": c.field,
				})
			}
		}
	}
	return nil
}

// checkBaseVsLateBinds 实现方案 §3.1 的校验 2：AC_BASE 不得与 tmpfs 之后
// 才绑定的 runtime 目标/宿主 socket 重叠（任一方向）。
//
// 典型事故：AC_BASE=$XDG_RUNTIME_DIR/X11-unix/appclone。`--tmpfs Base` 之后
// 紧跟着 `--ro-bind-try $XDG_RUNTIME_DIR/X11-unix`（整个目录），Base 连同
// 实例数据、keyring、私有总线又被应用看见。宁可在档案阶段报 2，也不静默
// 过滤 socket——过滤会让某个分身的输入法/剪贴板悄悄失效，用户无从得知。
//
// needRuntime 为假时 BuildBwrapArgs 根本不生成这些 bind（不隔离 runtime/dbus
// 的普通档案），此时把 AC_BASE 放进 $XDG_RUNTIME_DIR 是合法的，不能误拒。
func (o BuildOptions) checkBaseVsLateBinds(needRuntime bool) error {
	base := o.baseReal()
	if base == "" || !needRuntime {
		return nil
	}
	var targets []string
	if o.RuntimeDir != "" {
		targets = append(targets, o.RuntimeDir, filepath.Join(o.RuntimeDir, "bus"))
	}
	for _, s := range o.RuntimeSocks {
		targets = append(targets, s.Src, s.Dst)
	}
	for _, t := range targets {
		if t == "" {
			continue
		}
		for _, cand := range []string{filepath.Clean(t), Real(t)} {
			if cand == base || IsUnder(cand, base) || IsUnder(base, cand) {
				return errInput("err.profile.base_exposure", map[string]any{
					"path": t, "base": base, "field": "runtime_socks",
				})
			}
		}
	}
	return nil
}

func (o BuildOptions) expand(s string) (string, error) {
	r := o.Resolver
	if r == nil {
		// 没有 resolver 就不可能正确展开：宁可直接失败，也不许退化成
		// "把 ~ 当成相对路径"然后被 filepath.IsAbs 拒绝却给出错误的键。
		return "", errInternal("err.internal.no_resolver", nil, nil)
	}
	return r.Expand(s)
}

// keyringTargets 是沙箱内两个 keyring 目录（XDG_DATA_HOME/keyrings 与
// XDG_CONFIG_HOME/keyrings），两个目标 bind 到同一来源。XDG 相同时去重，
// 避免同目标两次 bind（后一次覆盖前一次，语义不变但 argv 冗余）。
func (o BuildOptions) keyringTargets() []string {
	data := filepath.Join(o.XDG["XDG_DATA_HOME"], "keyrings")
	conf := filepath.Join(o.XDG["XDG_CONFIG_HOME"], "keyrings")
	if data == conf {
		return []string{data}
	}
	return []string{data, conf}
}

// keyringBinds 是本次实际要 bind 的 keyring 目标。调用方（BuildOptions）已
// 用文件系统现状剔除"软链到同组另一个目标"的路径——bwrap 不能在软链目标上
// 挂载（Can't mount on symlink destination），常见布局
// `~/.config/keyrings -> ~/.local/share/keyrings` 会让整次启动失败。
// 没有提供解析结果时退回原始目标：纯函数测试与离线命令构造仍可工作。
func (o BuildOptions) keyringBinds() []string {
	if len(o.KeyringTargets) > 0 {
		return o.KeyringTargets
	}
	return o.keyringTargets()
}

func (o BuildOptions) publicDirs() []string {
	if o.Resolver == nil {
		return nil
	}
	return o.Resolver.PublicDirs()
}

func (o BuildOptions) protectedDirs() []string {
	out := []string{}
	if o.Resolver != nil {
		// 必须 Real：paths 里的条目在 expand 时已经解析过软链，而这里给的是词法
		// 路径。`~/.config` 一旦是指向 `~/cfg` 的软链，`paths=["~/.config"]` 会被
		// 归一成 `~/cfg`，既不等也不在 `~/.config` 之下 —— 整个配置根被隔离，
		// 而检查看起来"做过了"。
		for _, d := range []string{
			o.Resolver.XDG["CONFIG_HOME"], o.Resolver.XDG["DATA_HOME"], o.Resolver.XDG["STATE_HOME"],
		} {
			out = append(out, Real(d))
		}
	}
	return out
}

// pathLess 给 paths 排序：深度升序（父先子后），同深度按字典序。
// 没有全序的排序，doctor 打出来的命令两次不一致，golden 断言也无从写起（§3.1 ⑤）。
func pathLess(a, b string) bool {
	da, db := strings.Count(a, string(filepath.Separator)), strings.Count(b, string(filepath.Separator))
	if da != db {
		return da < db
	}
	return a < b
}

// ValidateCreatable 是"能不能建分身"的门禁：paths 为空即拒。
// CLI 与 GUI 必须用同一个判定，否则命令行能造出界面明确挡住的空档案（§6 表格）。
func ValidateCreatable(p Profile) error {
	if len(p.Paths) == 0 {
		return errInput("err.profile.empty", map[string]any{"app": p.App})
	}
	return nil
}

// ValidateBindable 是"能不能建实例"的门禁：Exec 存在、extra_binds 源存在、paths 非空。
//
// **它不要求 paths 的源已经存在**。理由是一次具体的事故：探测刚跑完、应用目录还
// 没在宿主上落地（首次配置时它本来就不存在，写入只发生在 overlay 的 upper 里），
// 若这里要求存在，`add` 会在探测成功后立刻报"档案已失效"，而"重新探测"仍然得到
// 同一份结果——用户陷在一个永远出不去的循环里。目标由 prepareBindTargets 补建
// （§4 白名单里的唯一显式例外），存在性只有 `run` 才需要。
func (e *Env) ValidateBindable(p Profile) error {
	if err := ValidateCreatable(p); err != nil {
		return err
	}
	if st, err := os.Stat(p.Exec); err != nil || st.IsDir() {
		return errInput("err.profile.exec_missing", map[string]any{"app": p.App, "exec": p.Exec})
	}
	// extra_binds 的源必须存在：它用的是 --bind 不是 -try，缺失会让整条 bwrap 命令
	// 在容器里失败，而错误看起来像"应用自己退出"（§3.1 ⑦）。
	for _, b := range p.ExtraBinds {
		src, err := e.Resolver.Expand(b.Src)
		if err != nil {
			return err
		}
		if _, err := os.Stat(src); err != nil {
			return newErr(CodeInput, "err.profile.extra_bind_missing", map[string]any{
				"app": p.App, "path": src,
			}, err)
		}
	}
	return nil
}

// ValidateRunnable 是"这份档案现在还能不能用"的门禁：paths 里的源**必须全部存在**。
// 部分存在不算——BuildBwrapArgs 产出的是一条命令，缺一个源整条失败，
// 而且失败发生在容器里，用户看不懂（§6.3 profile_stale）。
//
// 只有 `run` 用它。建实例用 ValidateBindable：目录不存在是可以补建的，
// 而"现在就跑得起来"是另一件事。
func (e *Env) ValidateRunnable(p Profile) error {
	if err := e.ValidateBindable(p); err != nil {
		return err
	}
	for _, raw := range p.Paths {
		abs, err := e.Resolver.Expand(raw)
		if err != nil {
			return err
		}
		if _, err := os.Stat(abs); err != nil {
			return newErr(CodeInput, "err.profile.stale", map[string]any{
				"app": p.App, "path": abs,
			}, err)
		}
	}
	return nil
}

// ProfileState 是 GUI 列表页/开关分派的唯一依据（§6.1 ②）：
// hasProfile = 档案文件在；creatable = 存在 + 可创建 + 可运行。
// GUI 不许自己拼这两个判断——拼错一次就会把"档案失效"当成"可以复用"，
// 造出一个什么都不隔离、启动即失败的分身。
func (e *Env) ProfileState(app string) (hasProfile, creatable bool, err error) {
	p, err := e.LoadProfile(app)
	if err != nil {
		if errKey(err) == "err.profile.not_found" {
			return false, false, nil
		}
		// 档案在但解析不了（损坏 / 版本超前）：这是"档案失效"，不是"没有档案"。
		// 返回 false,false 会让 GUI 走「首次配置」并覆盖掉用户的档案；
		// 返回 hasProfile=true 才能让它走「重新配置」（§6.1 ②）。
		return true, false, nil
	}
	if err := ValidateProfile(p, e.BuildOptions()); err != nil {
		return true, false, nil
	}
	if err := ValidateCreatable(p); err != nil {
		return true, false, nil
	}
	// creatable 的语义是"现在能建出实例"，不是"现在就能跑"：paths 的源不存在时
	// 目标会由 prepareBindTargets 补建，不该因此把档案判成失效（那会让用户
	// 在"重新配置 → 仍然失效"之间打转）。
	if err := e.ValidateBindable(p); err != nil {
		return true, false, nil
	}
	return true, true, nil
}

func errKey(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Key
	}
	return ""
}

// appKeyRe / idRe 是路径组成部分的唯一权威校验（§4）。CLI 与 GUI 共用，
// 不许各自写正则——两套正则的差异最终表现为"GUI 能建、CLI 报错"。
var (
	appKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	idRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidateAppKey 校验应用 key（= 源 .desktop 文件名去后缀）。
func ValidateAppKey(s string) error {
	if !appKeyRe.MatchString(s) {
		return errInput("err.app.invalid_key", map[string]any{"app": s})
	}
	if s == "." || s == ".." || strings.HasPrefix(s, ".") {
		return errInput("err.app.invalid_key", map[string]any{"app": s})
	}
	// appclone- 前缀是分身入口的保留命名空间：扫应用时会按它排除自己生成的入口。
	if strings.HasPrefix(s, "appclone-") {
		return errInput("err.app.reserved_prefix", map[string]any{"app": s})
	}
	return nil
}

// SanitizeAppKey 把桌面文件名归一化成安全的应用 key。
//
// 存在的理由：真实系统里有 "CC Switch.desktop"、"WorkBuddy AI.desktop" 这种
// 带空格的文件名（本机 /usr/share/applications 就有两个）。严格按字符集拒绝，
// 用户会在列表里找不到这些应用，而且没有任何提示说明为什么。
// 规则：小写 → 非法字符换成 '-' → 合并连续 '-' → 去掉首尾的点和横线；
// 归一化后仍不合法（空、以 appclone- 开头等）才拒绝。
func SanitizeAppKey(name string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-.")
	// 截断到 64 字符（§4 的 <app> 上限）。
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-.")
	}
	if err := ValidateAppKey(out); err != nil {
		return "", err
	}
	return out, nil
}

// ValidateID 校验实例 id。id 会拼进删除路径，所以字符集必须硬约束。
func ValidateID(s string) error {
	if !idRe.MatchString(s) {
		return errInput("err.instance.invalid_id", map[string]any{"id": s})
	}
	return nil
}

// ValidateDisplayName 校验用户可见名（§4）：trim 后 1..32 rune、NFC、
// 禁控制字符与换行/制表符（换行会截断 .desktop 的 Name= 行）、同应用内唯一。
func ValidateDisplayName(s string, existing []string) error {
	n := norm.NFC.String(strings.TrimSpace(s))
	if n == "" {
		return errInput("err.display_name.empty", nil)
	}
	if utf8.RuneCountInString(n) > 32 {
		return errInput("err.display_name.too_long", map[string]any{"name": n, "max": 32})
	}
	for _, r := range n {
		if r == '\n' || r == '\t' || r == '\r' || unicode.IsControl(r) {
			return errInput("err.display_name.control_char", map[string]any{"name": s})
		}
	}
	for _, e := range existing {
		if norm.NFC.String(e) == n {
			return errConflict("err.display_name.duplicate", map[string]any{"name": n})
		}
	}
	return nil
}

// AllocID 按 main、inst2、inst3… 取第一个空闲（§4）。
// 省略 id 时用户说"再来一个分身"，不该被一个 EEXIST 打断。
func (e *Env) AllocID(app string) (string, error) {
	dir := filepath.Join(e.Base, "instances", app)
	for i := 1; i <= 10000; i++ {
		cand := "main"
		if i > 1 {
			cand = "inst" + strconv.Itoa(i)
		}
		if _, err := os.Stat(filepath.Join(dir, cand)); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return "", errInternal("err.instance.id_exhausted", map[string]any{"app": app}, nil)
}

// AllocDisplayName 取"第一个空闲的「分身 N」"，N 是最小未占用正整数。
// 不是"实例数 + 1"：删掉中间那个后，数量+1 会撞上已有的「分身 2」，
// GNOME 里两个入口同名、用户无法区分（§4）。
func AllocDisplayName(existing []string) string {
	used := map[string]bool{}
	for _, e := range existing {
		used[norm.NFC.String(strings.TrimSpace(e))] = true
	}
	for i := 1; ; i++ {
		cand := "分身 " + strconv.Itoa(i)
		if !used[cand] {
			return cand
		}
	}
}
