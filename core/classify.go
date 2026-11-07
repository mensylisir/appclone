package core

import (
	"path/filepath"
	"strings"
)

// PathClass 是 L2 语义分类的结果（docs/discover.md §5）。GUI 屏3 的分组、
// 默认勾选与档案字段的落点都由它决定——**不许在 GUI 里再算一遍**：两套判断
// 迟早会分叉，表现为"界面显示会隔离，实际没进 paths"。
type PathClass string

const (
	ClassConfig  PathClass = "config"  // XDG 配置/数据目录：默认隔离
	ClassData    PathClass = "data"    // 数据库/索引文件：默认隔离
	ClassCache   PathClass = "cache"   // 缓存：默认不隔离
	ClassPublic  PathClass = "public"  // 公共目录（Downloads 等）：强制排除
	ClassShared  PathClass = "shared"  // 共享可写文件：进 shared_writes，不隔离
	ClassKeyring PathClass = "keyring" // 凭证：进 unsupported，既不隔离也不共享
	ClassDotdir  PathClass = "dotdir"  // 写在 $HOME 的顶层点目录：灰区
	ClassSystem  PathClass = "system"  // /etc、/var、/opt 下：灰区
	ClassUnknown PathClass = "unknown" // 规则表没覆盖：灰区
)

// Candidate 是屏3 的一行（= 档案的一次预填建议）。
//
// Observed 是**写入条目数**，不是"命中次数"：overlay 只知道某个路径被写过或
// 删过，拿不到次数。文案红线见 docs/方案设计.md §6.1。
type Candidate struct {
	Path     string    `json:"path"`     // 绝对路径；展示用 ToStoreForm 转 ~ 形式
	Class    PathClass `json:"class"`    // 分组与 chip
	Checked  bool      `json:"checked"`  // 默认是否勾选；灰区一律 false
	Observed int       `json:"observed"` // 写入条目数
	Why      string    `json:"why"`      // i18n 键，由视图层翻译
	Warning  bool      `json:"warning"`  // 需要用户确认（灰区）
}

// isPublic 判定公共目录本身或其中任何路径（§5 规则 1，最高优先级）。
func (r *PathResolver) isPublic(abs string) bool {
	for _, pub := range r.PublicDirs() {
		if isUnderLoose(pub, abs) {
			return true
		}
	}
	return false
}

// isKeyring 判定凭证存储。keyring 不在文件路径上、而在 DBus 服务里，
// 但 `~/.local/share/keyrings/*.keyring` 这类落盘副本仍会被 overlay 看见；
// 把它当普通 config 隔离，分身里就会出现"另一个 keyring 文件"，
// 而真正的凭证查询走的是主实例的 secret service——那是彻底的假隔离（§5.4）。
func (r *PathResolver) isKeyring(abs string) bool {
	for _, base := range []string{
		filepath.Join(r.XDG["CONFIG_HOME"], "keyrings"),
		filepath.Join(r.XDG["DATA_HOME"], "keyrings"),
	} {
		if isUnderLoose(base, abs) {
			return true
		}
	}
	return strings.HasSuffix(filepath.Base(abs), ".keyring")
}

// sharedWriteDirs 是"隔离后功能立刻坏掉"的硬排除目录（§5 规则 2）。
// 这两处不是"建议不隔离"，是**必须**共享：
//   - 输入法：`~/.config/fcitx5` 被隔离 → 分身里输入法直接失效；
//   - GTK 书签：`bookmarks` 被隔离 → 文件选择器里书签全没了。
func (r *PathResolver) sharedWriteDirs() []string {
	return []string{
		filepath.Join(r.XDG["CONFIG_HOME"], "fcitx5"),
		filepath.Join(r.XDG["CONFIG_HOME"], "fcitx"),
		filepath.Join(r.XDG["CONFIG_HOME"], "ibus"),
		// GTK 的配置目录整体算共享：它的内容几乎全是"会话级共享状态"
		// （bookmarks、最近使用、打印设置），而应用自己的配置在 dconf 里，
		// 不在这个目录。整体隔离它换来的是"书签没了"。
		filepath.Join(r.XDG["CONFIG_HOME"], "gtk-3.0"),
		filepath.Join(r.XDG["CONFIG_HOME"], "gtk-4.0"),
	}
}

func (r *PathResolver) isSharedWrite(abs string) bool {
	for _, dir := range r.sharedWriteDirs() {
		if isUnderLoose(dir, abs) {
			return true
		}
	}
	base := filepath.Base(abs)
	// GTK 最近文件与书签：几行文本，两个实例互相追加是可接受代价，
	// 隔离开才是故障（§5.4）。
	//
	// `recently-used.xbel` 只认 XDG_DATA_HOME **顶层**那一个：按 basename
	// 任意匹配会把"应用自己数据目录里的同名文件"也算共享，归并时整个应用
	// 目录被升级成 shared_writes，私有配置跟着共享出去。
	if samePathLoose(abs, filepath.Join(r.XDG["DATA_HOME"], "recently-used.xbel")) {
		return true
	}
	if base == "bookmarks" {
		parent := filepath.Base(filepath.Dir(abs))
		if parent == "gtk-3.0" || parent == "gtk-4.0" {
			return true
		}
	}
	// 同一故障模式的其他桌面：KDE 文件对话框的书签（user-places.xbel）
	// 与 GTK2 的 ~/.gtk-bookmarks。隔离它们，用户同样会看到"书签全没了"。
	//
	// 三处精确路径都必须用 loose 比较：HOME 是软链、或 XDG_DATA_HOME 指向
	// 真实路径而 upper 里是词法路径时，单纯的字符串相等会判不出来，
	// 书签/最近文件被隔离——那正是这里要避免的故障。
	if samePathLoose(abs, filepath.Join(r.XDG["DATA_HOME"], "user-places.xbel")) {
		return true
	}
	if samePathLoose(abs, filepath.Join(r.Home, ".gtk-bookmarks")) {
		return true
	}
	return false
}

// samePathLoose 比较两个路径是否指向同一位置：词法相等，或解析软链后相等。
// 任一侧不存在时 Real 退回 Clean，词法比较仍然兜底。
func samePathLoose(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	return Real(a) == Real(b)
}

// hasCacheComponent 判定路径里是否出现 cache 段（大小写不敏感）。
// 用"段"而不是子串：`~/.config/App/cache.db` 与 `~/.cache/App` 都该命中，
// 但 `~/.config/App/CacheManager/settings.ini` 里的 "cache" 是名字的一部分，
// 那不是缓存——子串匹配会把整个 App 目录错判成缓存、默认不勾。
//
// 只看**主目录之内**的部分：绝对路径的祖先段与本判定无关，而 `$TMPDIR` 恰好
// 常常在 `~/.cache` 下（测试环境即如此），扫全路径会把整棵临时树判成缓存。
func (r *PathResolver) hasCacheComponent(abs string) bool {
	rel, ok := "", false
	if _, rel, ok = looseRel(r.Home, abs); !ok || rel == "" {
		rel = abs
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if strings.EqualFold(seg, "cache") || strings.EqualFold(seg, ".cache") {
			return true
		}
	}
	return false
}

// dataExts 是"这明显是应用数据"的文件扩展名（§5 规则 8，文件形态启发）。
var dataExts = map[string]bool{
	".db": true, ".sqlite": true, ".sqlite3": true, ".ldb": true,
	".mdb": true, ".realm": true, ".edb": true,
}

func hasDataExt(abs string) bool {
	return dataExts[strings.ToLower(filepath.Ext(abs))]
}

// Classify 把一条观测到的写入路径翻译成档案语义（§5 的有序规则表，
// 先命中先定，不许两条规则同时生效）：
//
//	1 公共目录（含其下任意路径）→ 强制排除，任何规则不得翻案
//	2 凭证 → unsupported
//	3 共享可写 → shared_writes（**压过** `~/.config/*` 的默认勾选）
//	4 cache 段 → 不隔离
//	5 XDG 配置/数据目录 → 默认勾选
//	6 系统路径 → 灰区
//	7 顶层点目录 → 灰区（设计稿把 `~/.dingtalk` 画成 config 且默认勾选，
//	  那是演示简化，照抄等于把用户自己放在那里的文件也关进分身）
//	8 数据库文件 → 默认勾选
func (r *PathResolver) Classify(abs string) (PathClass, bool, string, bool) {
	abs = filepath.Clean(abs)
	switch {
	case r.isPublic(abs):
		return ClassPublic, false, "probe.why.public", false
	case r.isKeyring(abs):
		return ClassKeyring, false, "probe.why.keyring", false
	case r.isSharedWrite(abs):
		return ClassShared, false, "probe.why.shared", false
	case r.hasCacheComponent(abs):
		return ClassCache, false, "probe.why.cache", false
	case r.underXdgAppDirs(abs):
		return ClassConfig, true, "probe.why.config", false
	case underAny(abs, "/etc", "/var", "/opt", "/usr", "/srv"):
		return ClassSystem, false, "probe.why.system", true
	case isTopLevelDotdir(r.Home, abs):
		return ClassDotdir, false, "probe.why.dotdir", true
	case hasDataExt(abs):
		return ClassData, true, "probe.why.data", false
	}
	return ClassUnknown, false, "probe.why.unknown", true
}

// underXdgAppDirs 判定是否落在"应用自己的 XDG 目录"里：
// `~/.config/*`、`~/.local/share/*`、`~/.local/state/*`、`~/.var/app/*`（Flatpak）。
// XDG 根本身不算——隔离整个 `~/.config` 就是隔离所有应用（§5）。
func (r *PathResolver) underXdgAppDirs(abs string) bool {
	for _, root := range []string{
		r.XDG["CONFIG_HOME"], r.XDG["DATA_HOME"], r.XDG["STATE_HOME"],
		filepath.Join(r.Home, ".var", "app"),
	} {
		if _, rel, ok := looseRel(root, abs); ok && rel != "" {
			return true
		}
	}
	return false
}

// isTopLevelDotdir 判定 `$HOME/.name` 这种写死在家目录顶层的条目。
// 它同时覆盖目录与单文件（`~/.apprc`）：overlay 里两者形态相同，
// 而"是不是应用自己的"这件事两者一样说不清。
func isTopLevelDotdir(home, abs string) bool {
	_, rel, ok := looseRel(home, abs)
	if !ok || rel == "" {
		return false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return strings.HasPrefix(first, ".") && first != "." && first != ".."
}

// underAny 判定 abs 是否落在任一前缀之下（含相等）。
func underAny(abs string, prefixes ...string) bool {
	for _, p := range prefixes {
		if isUnderLoose(p, abs) {
			return true
		}
	}
	return false
}
