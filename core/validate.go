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
	RuntimeDir   string     // 宿主 XDG_RUNTIME_DIR
	RuntimeSocks []BindPair // 已探测存在的宿主 socket → 私有 runtime 内目标（只读）
	DBusAddress  string     // dbus-daemon --print-address 原文；空 = 不隔离
	XDG          map[string]string
	// Audit 非空时在 ④ 与 ⑤ 之间插入审计层（只有 `run --audit` 会设置它）。
	Audit *AuditLayers
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

	// lock_files 与 sockets 不能是同一路径：一个 bind 空文件、一个靠私有 runtime，
	// 混着写会让实现只能二选一，另一条静默失效（§6.2 末尾的 ENOTSOCK 事故）。
	for _, lf := range p.LockFiles {
		abs, err := o.expand(lf)
		if err != nil {
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
	for _, field := range []struct {
		name string
		list []string
	}{
		{"ignored", p.Ignored},
		{"shared_writes", p.SharedWrites},
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
	return nil
}

// normalizePaths 展开、去重、把父子重叠归一到最上层项。
//
// "最上层"而不是"最具体"：`paths` 里的每一项都会被 bind 到实例目录的同名子路径，
// 父目录已经覆盖子目录，保留子项只会多一条冗余 bind；反过来若只留子项，
// 父目录的其余内容就留在了共享层——那才是真正的漏隔离。
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
	if abs == filepath.Clean(o.Home) {
		return errInput("err.profile.home_path", map[string]any{"field": field})
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
	// （instances/<app>/<id>/root 变成它自己的子目录）。
	if o.Base != "" && IsUnder(o.Base, abs) {
		return errInput("err.profile.base_path", map[string]any{"path": abs, "field": field})
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

func (o BuildOptions) expand(s string) (string, error) {
	r := o.Resolver
	if r == nil {
		// 没有 resolver 就不可能正确展开：宁可直接失败，也不许退化成
		// "把 ~ 当成相对路径"然后被 filepath.IsAbs 拒绝却给出错误的键。
		return "", errInternal("err.internal.no_resolver", nil, nil)
	}
	return r.Expand(s)
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
