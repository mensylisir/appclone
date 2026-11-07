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
	if h := os.Getenv("AC_HOME"); h != "" && filepath.IsAbs(h) {
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
	if clean != s && hasDotDot(orig) {
		return "", errInput("err.path.dotdot", map[string]any{"path": orig})
	}
	if hasDotDot(orig) {
		return "", errInput("err.path.dotdot", map[string]any{"path": orig})
	}
	return clean, nil
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
			v = filepath.Clean(v)
			if v == filepath.Clean(r.Home) {
				// 用户把某个公共目录指到了家目录本身：那不是公共目录，是家目录。
				continue
			}
			configured[strings.TrimSuffix(k, "_DIR")] = v
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
