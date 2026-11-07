package core

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PathResolver 是路径展开的唯一实现（§6.4）。CLI、GUI、探测、审计全部走它，
// 任何地方自己拼 "~/.config" 都是 bug：AC_HOME 一旦生效，手拼的路径指向的
// 是真实主目录，隔离测试会"看起来成功"。
type PathResolver struct {
	RealHome string            // 真实 $HOME
	Home     string            // 有效 HOME：AC_HOME 或 RealHome
	XDG      map[string]string // 已补默认值的 XDG 变量（键名不带 XDG_ 前缀的大写形式，如 "CONFIG_HOME"）
	UID      int
}

// xdgKeys 是允许在路径里出现的变量名。只认这些键是刻意的：
// 用 os.ExpandEnv 会把 $PATH、$RANDOM 之类也展开，档案里写一个 $FOO 就能
// 让同一份档案在两台机器上指向不同目录。
var xdgKeys = []string{"CONFIG_HOME", "DATA_HOME", "STATE_HOME", "CACHE_HOME", "RUNTIME_DIR"}

// NewPathResolver 从当前进程环境构造。AC_HOME 必须是绝对路径，否则忽略并
// 保持 RealHome（相对的"家目录"没有意义，只可能来自脚本手误）。
func NewPathResolver() (*PathResolver, error) {
	real, err := os.UserHomeDir()
	if err != nil {
		return nil, errEnv("err.env.no_home", nil, err)
	}
	home := real
	if h := os.Getenv("AC_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			// 静默忽略会让冒烟脚本"以为用了假 HOME"，实际跑的是真实主目录——
			// 那是最危险的一类假通过（§6.4）。
			return nil, errInput("err.env.ac_home_relative", map[string]any{"path": h})
		}
		home = filepath.Clean(h)
	}
	uid := os.Getuid()
	return newResolver(real, home, uid, os.Getenv), nil
}

// newResolver 供测试注入：不读全局环境，可并行。
func newResolver(realHome, home string, uid int, getenv func(string) string) *PathResolver {
	r := &PathResolver{RealHome: realHome, Home: home, UID: uid, XDG: map[string]string{}}
	for _, k := range xdgKeys {
		v := getenv("XDG_" + k)
		if v == "" || !filepath.IsAbs(v) {
			v = r.defaultXDG(k)
		}
		r.XDG[k] = filepath.Clean(v)
	}
	return r
}

func (r *PathResolver) defaultXDG(key string) string {
	switch key {
	case "CONFIG_HOME":
		return filepath.Join(r.Home, ".config")
	case "DATA_HOME":
		return filepath.Join(r.Home, ".local", "share")
	case "STATE_HOME":
		return filepath.Join(r.Home, ".local", "state")
	case "CACHE_HOME":
		return filepath.Join(r.Home, ".cache")
	case "RUNTIME_DIR":
		// 规范默认值就是 /run/user/<uid>；容器里常常没有，调用方自己判存在性。
		return filepath.Join("/run/user", strconv.Itoa(r.UID))
	}
	return ""
}

// Vars 返回路径展开时可用的变量表（不含 XDG_ 前缀）。
func (r *PathResolver) Vars() map[string]string {
	m := map[string]string{"HOME": r.Home}
	for _, k := range xdgKeys {
		m["XDG_"+k] = r.XDG[k]
	}
	return m
}

// Expand 把 ~ / $HOME / $XDG_* 展开成绝对路径，并做 Clean 与 ".." 拒绝。
// 不存在的路径也允许（ValidateRunnable 才管存在性）。
func (r *PathResolver) Expand(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", errInput("err.path.empty", nil)
	}
	orig := s
	switch {
	case s == "~":
		s = r.Home
	case strings.HasPrefix(s, "~/"):
		s = filepath.Join(r.Home, s[2:])
	}
	s = expandKnownVars(s, r.Vars())
	if strings.ContainsRune(s, '$') {
		return "", errInput("err.path.unknown_var", map[string]any{"path": orig})
	}
	if !filepath.IsAbs(s) {
		return "", errInput("err.path.not_absolute", map[string]any{"path": orig})
	}
	clean := filepath.Clean(s)
	if hasDotDot(orig) {
		return "", errInput("err.path.dotdot", map[string]any{"path": orig})
	}
	// 必须在这里解析软链：`~/.config/App` 可能是指向 `~/Downloads` 的软链，
	// 只用 Clean 的字符串做公共目录判定会放行，而内核 bind 时会解析到公共目录，
	// 于是 Downloads 被隔离成实例私有——直接踩第一原则（§5 paths、§6.4）。
	return Real(clean), nil
}

func hasDotDot(p string) bool {
	for _, seg := range strings.Split(p, string(filepath.Separator)) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// expandKnownVars 只替换变量表里有的键，其余原样保留（随后会被判为未知变量）。
func expandKnownVars(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i + 1
		braced := false
		if j < len(s) && s[j] == '{' {
			braced = true
			j++
		}
		start := j
		for j < len(s) && (isVarChar(s[j])) {
			j++
		}
		name := s[start:j]
		if braced {
			if j < len(s) && s[j] == '}' {
				j++
			} else {
				b.WriteString(s[i:])
				break
			}
		}
		if v, ok := vars[name]; ok && name != "" {
			b.WriteString(v)
			i = j
			continue
		}
		b.WriteString(s[i:j])
		i = j
	}
	return b.String()
}

func isVarChar(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// ToStoreForm 把绝对路径转成落盘形式：能用有效 HOME 无损还原就用 ~/...，
// 否则保留绝对路径（软链到 $HOME 之外、XDG 指向别处的情况）。§5 paths
func (r *PathResolver) ToStoreForm(abs string) string {
	if abs == r.Home {
		return "~"
	}
	if rel, ok := relUnder(r.Home, abs); ok {
		return "~/" + rel
	}
	return abs
}

// relUnder 判断 child 是否在 parent 之下，返回相对路径。
func relUnder(parent, child string) (string, bool) {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return "", true
	}
	if !strings.HasPrefix(child, parent+string(filepath.Separator)) {
		return "", false
	}
	return child[len(parent)+1:], true
}

// IsUnder 报告 child 是否等于 parent 或位于其下。所有"危险路径"判定都靠它，
// 用字符串前缀比较会把 "/home/ua" 当成 "/home/u" 的子路径。
func IsUnder(parent, child string) bool {
	_, ok := relUnder(parent, child)
	return ok
}

// looseRel 在**词法与真实**两种形态下各试一次 relUnder，返回命中的基准与相对路径。
//
// 存在的理由：软链是常态（§6.4）——`~/.config` 可能指向 `~/cfg`，家目录本身也可能
// 是挂载点软链。upper 里记录的是**真实**路径，而档案里写的是**词法**路径（`~/...`）。
// 只比其中一种，结论就是"认不出这是配置目录"，用户看到的是"预勾选项凭空消失"。
func looseRel(root, child string) (base, rel string, ok bool) {
	if rel, ok := relUnder(root, child); ok {
		return root, rel, true
	}
	rr, rc := Real(root), Real(child)
	if rr == root && rc == child {
		return "", "", false // 两边都没被解析过，上面的判定已覆盖全部可能
	}
	if rel, ok := relUnder(rr, rc); ok {
		return rr, rel, true
	}
	return "", "", false
}

// isUnderLoose 是 looseRel 的布尔版。
func isUnderLoose(root, child string) bool {
	_, _, ok := looseRel(root, child)
	return ok
}

// PublicDirs 返回公共目录清单（§5 与 discover.md §5 的同一份数据）。
// 优先读 user-dirs.dirs，缺失或值为 $HOME 本身时用 XDG 规范的英文默认名。
// 返回的路径**不保证存在**：判定的是"这是不是一个公共目录"，不是"它在不在"。
func (r *PathResolver) PublicDirs() []string {
	defaults := []string{"Desktop", "Documents", "Downloads", "Music", "Pictures", "Public", "Templates", "Videos"}
	configured := map[string]string{}
	f, err := os.Open(filepath.Join(r.XDG["CONFIG_HOME"], "user-dirs.dirs"))
	if err == nil {
		defer f.Close()
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
			v = expandKnownVars(v, r.Vars())
			if v == "" || strings.ContainsRune(v, '$') {
				continue
			}
			if !filepath.IsAbs(v) {
				v = filepath.Join(r.Home, v)
			}
			v = Real(filepath.Clean(v))
			if v == filepath.Clean(r.Home) {
				// 用户把某个公共目录指到了家目录本身：那不是公共目录，是家目录。
				continue
			}
			// 键名是 `XDG_DOWNLOAD_DIR`：先剥 `XDG_` 再剥 `_DIR`，得到
			// `DOWNLOAD`。只剥 `_DIR` 会留下 `XDG_DOWNLOAD`，下面按 `DOWNLOAD`
			// 查表永远查不到——中文系统上 `~/下载` 就再也不会被认成公共目录，
			// 而那正是"公共文件必须共享"这条第一原则的判据来源。
			configured[strings.TrimSuffix(strings.TrimPrefix(k, "XDG_"), "_DIR")] = v
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(defaults))
	for _, k := range []string{"DESKTOP", "DOCUMENTS", "DOWNLOAD", "MUSIC", "PICTURES", "PUBLICSHARE", "TEMPLATES", "VIDEOS"} {
		p := configured[k]
		if p == "" {
			name := map[string]string{
				"DESKTOP": "Desktop", "DOCUMENTS": "Documents", "DOWNLOAD": "Downloads",
				"MUSIC": "Music", "PICTURES": "Pictures", "PUBLICSHARE": "Public",
				"TEMPLATES": "Templates", "VIDEOS": "Videos",
			}[k]
			p = filepath.Join(r.Home, name)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Real 把路径解析成真实路径：对最长的已存在前缀做 EvalSymlinks，再接回不存在的尾段。
// 直接用 filepath.EvalSymlinks 要求整条路径都存在，而"应用还没建过这个目录"是常态。
func Real(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	// 逐级向上找到存在的祖先，解析它，再把剩下的尾段接回去。
	head, tail := p, ""
	for {
		parent := filepath.Dir(head)
		if parent == head { // 到根了
			return p
		}
		tail = filepath.Join(filepath.Base(head), tail)
		head = parent
		if resolved, err := filepath.EvalSymlinks(head); err == nil {
			return filepath.Join(resolved, tail)
		}
	}
}

// ProtectedDirs 是 paths 里一律不许出现的"整个命名空间"级路径（不含公共目录，
// 公共目录由 PublicDirs 单独给出，因为界面上要单独渲染那一组）。
func (r *PathResolver) ProtectedDirs(base string) []string {
	return []string{
		"/",
		r.Home,
		r.XDG["CONFIG_HOME"],
		r.XDG["DATA_HOME"],
		r.XDG["STATE_HOME"],
		base,
	}
}
