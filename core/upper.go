package core

import (
	"path/filepath"
	"sort"
	"strings"
)

// EntryKind 是 overlay upper 层里一条的形态。
//
// whiteout 必须单列：它证明"应用删过这个路径"，不是"写过"。把删除证据当写入
// 候选会让用户在一个已经删掉的路径上勾"隔离"（§3.2.4 的 whiteout 三形态）。
type EntryKind int

const (
	EntryDir EntryKind = iota
	EntryFile
	EntryWhiteout
)

// UpperEntry 是 upper 层的一条。Path **相对 $HOME**：探测会话把 merger 挂在
// $HOME 上，所以 upper 里的相对路径天然就是"家目录里的什么位置"。
type UpperEntry struct {
	Path string // 如 ".config/DingTalk/config.json"
	Kind EntryKind
	Size int64
}

// MergeUpper 把 upper 条目归并成屏3 的候选集（docs/discover.md §3.2.4）。
//
// 归并做三件事，缺一件都会出现"用户勾了但漏隔离"或"勾了不该勾的"：
//  1. 取"最深的应用目录"或"单文件本身"——不能只收目录（`~/.apprc` 是顶层单文件），
//     也不能把 `~/.config` 这类纯父目录当候选（勾中它 = 隔离所有应用的配置）；
//  2. 剔除 XDG 根及其祖先；
//  3. 同一应用目录下的多条条目合并成一行，`Observed` 记条目数。
//
// 判"零写入"看的是**归并后的候选数**，不是 upper 条目数：一个应用可能写 300 个
// 文件全落在 `~/.cache` 里，那在语义上仍然是"没有值得隔离的写入"。
func (r *PathResolver) MergeUpper(entries []UpperEntry) ([]Candidate, []string) {
	counts := map[string]int{}         // 应用目录 → 非目录条目数
	dirSeen := map[string]bool{}       // 应用目录 → 至少出现过一个目录条目
	escalate := map[string]PathClass{} // 应用目录 → 必须升级成的类别（共享可写）
	extra := map[string]Candidate{}    // 单独成行的文件级候选（凭证）
	added := map[string]bool{}         // 已进入 order 的应用目录
	var order []string
	var whiteouts []string

	for _, en := range entries {
		if strings.TrimSpace(en.Path) == "" {
			continue
		}
		abs := filepath.Clean(filepath.Join(r.Home, filepath.FromSlash(en.Path)))
		// upper 里出现 $HOME 之外的路径只可能是探测期被改坏了：
		// 宁可丢掉这一条，也不要把 /etc/passwd 之类的候选摆到用户面前。
		if !IsUnder(r.Home, abs) {
			continue
		}
		if en.Kind == EntryWhiteout {
			whiteouts = append(whiteouts, abs)
			continue
		}
		owner, ok := r.ownerOf(abs)
		if !ok {
			continue
		}
		// 条目自身的类别要多看一步：应用目录里的 `bookmarks`、keyring 副本
		// 是**文件级硬排除**，它们被归并进目录后就再也看不出来了——
		// 直接把目录当 config 勾选，等于把书签/凭证关进分身（§5 优先级 2）。
		cls, _, _, _ := r.Classify(abs)
		switch cls {
		case ClassShared:
			// 共享可写压过 config 默认勾选：整个目录必须走 shared_writes，
			// 否则 bwrap 里没有单独的绑定点可以让它保持共享。
			escalate[owner] = ClassShared
		case ClassKeyring:
			if owner != abs {
				// 应用目录里的凭证副本：目录照旧隔离，但这个文件单独标 unsupported。
				c := Candidate{Path: abs, Class: ClassKeyring, Why: "probe.why.keyring"}
				extra[abs] = c
			}
		}
		if !added[owner] {
			added[owner] = true
			order = append(order, owner)
		}
		if en.Kind == EntryDir {
			dirSeen[owner] = true
			continue
		}
		counts[owner]++
	}

	// 输出必须稳定：同一份 upper 两次归并结果不同，GUI 的分组会跳、
	// golden 断言也无从写起（§11 的同一纪律）。
	sort.Strings(order)
	cands := make([]Candidate, 0, len(order)+len(extra))
	for _, p := range order {
		class, checked, why, warn := r.Classify(p)
		n := counts[p]
		if n == 0 && dirSeen[p] {
			// 只建了目录、没写出文件：仍是一次写入，但只有 1 条，
			// 不能把父目录自身也算成条目（否则"观测到 N 个写入条目"会偏大）。
			n = 1
		}
		if esc, ok := escalate[p]; ok {
			class, checked, why, warn = esc, false, "probe.why.shared", false
		}
		cands = append(cands, Candidate{
			Path: p, Class: class, Checked: checked,
			Observed: n, Why: why, Warning: warn,
		})
	}
	for _, c := range extra {
		c.Observed = counts[c.Path]
		if c.Observed == 0 {
			c.Observed = 1
		}
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Path < cands[j].Path })
	sort.Strings(whiteouts)
	return cands, whiteouts
}

// ownerOf 把一条具体条目归到"它属于哪个应用目录"。
//
// 规则是"XDG 根下取第一级，$HOME 下取第一级"，因为应用把自己的状态放在
// `<XDG 根>/<应用名>/…` 是绝大多数情况；更深的层级是实现细节，不该拆成
// 多个勾选项让用户逐个判断（复杂度不泄漏给用户）。
//
// 必须剔除两类"看起来像应用目录"的东西（§3.2.4）：
//   - XDG 根本身（`~/.config`）——勾中它等于隔离所有应用的配置；
//   - XDG 根的**祖先**（`~/.local`、`~/.var`）——upper 里它们只因下级有写入
//     而存在，勾中 `~/.var` 会把所有 Flatpak 应用的数据一起隔离。
func (r *PathResolver) ownerOf(abs string) (string, bool) {
	roots := r.xdgRoots()
	for _, root := range roots {
		// looseRel 同时认词法与真实形态：`~/.config` 软链到 `~/cfg` 时，
		// upper 里记录的是 `cfg/App/...`，只比词法根会匹配不上，整条候选消失。
		base, rel, ok := looseRel(root, abs)
		if !ok {
			continue
		}
		if rel == "" {
			return "", false // 纯父目录：不是候选
		}
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		owner := filepath.Join(base, first)
		if owner == r.Home || owner == Real(r.Home) {
			return "", false
		}
		return owner, true
	}
	if base, rel, ok := looseRel(r.Home, abs); ok && rel != "" {
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		owner := filepath.Join(base, first)
		// 祖先：`~/.local` 是 DATA_HOME/STATE_HOME 的祖先，`~/.var` 是 Flatpak 根的祖先。
		for _, root := range roots {
			if isUnderLoose(owner, root) {
				return "", false
			}
		}
		return owner, true
	}
	return "", false // $HOME 本身或其祖先：丢弃
}

// xdgRoots 是"应用目录都在它下面一级"的那几个根。
func (r *PathResolver) xdgRoots() []string {
	return []string{
		r.XDG["CONFIG_HOME"], r.XDG["DATA_HOME"], r.XDG["STATE_HOME"], r.XDG["CACHE_HOME"],
		filepath.Join(r.Home, ".var", "app"),
	}
}

// ---------------------------------------------------------------------------
// 读证据归并（ADR-034 §7）
// ---------------------------------------------------------------------------

// readAgg 是同一个 owner 下读路径的聚合。证据强度取后代最大值（§7.1）。
type readAgg struct {
	owner       string
	reads       int
	weak        bool
	strong      bool
	credPath    string // 实际观察到的最强凭据路径
	ownerStrong bool
}

// strongCredentialNames 是强证据的精确 basename（小写，§7.3）。
var strongCredentialNames = map[string]bool{
	"credentials":      true,
	"credentials.json": true,
	"logins.json":      true,
	"login data":       true,
	"key4.db":          true,
	"cookies":          true,
	"cookies.sqlite":   true,
	"token":            true,
	"token.json":       true,
}

// weakCredentialTokens 是弱证据的词元（相等匹配，不做子串，§7.3）。
var weakCredentialTokens = map[string]bool{
	"credential": true, "credentials": true,
	"token": true, "auth": true, "login": true, "oauth": true,
	"secret": true, "password": true, "passwd": true, "account": true,
	"keyring": true, "keychain": true, "cookie": true, "cookies": true,
	"session": true,
}

// credentialNameStrength 判定一个文件名/目录名像不像登录态。
// 强证据先判精确 basename；弱证据按非字母数字切词后精确比词元。
func credentialNameStrength(base string) CredentialStrength {
	b := strings.ToLower(strings.TrimSpace(base))
	if strongCredentialNames[b] {
		return CredentialStrong
	}
	for _, tok := range strings.FieldsFunc(b, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if weakCredentialTokens[tok] {
			return CredentialWeak
		}
	}
	return CredentialNone
}

// MergeUpperWithReads 在写入候选的基础上并入读证据（§7.1 固定顺序）：
// 文件级硬排除 → 噪声 → owner 归并 → owner 护栏 → 证据升级 → 排序/截断。
//
// reads 是相对 $HOME 的路径（strace 解析产物）。acBase 是 AC_BASE，
// 读它只是工具自己在工作，绝不能变成"用户应用要隔离的目录"。
func (r *PathResolver) MergeUpperWithReads(entries []UpperEntry, reads, flagsHidden []string, acBase, app string) ([]Candidate, []string, int, []string) {
	cands, whiteouts := r.MergeUpper(entries)
	for i := range cands {
		cands[i].DisplayClass = cands[i].Class
	}
	idx := map[string]int{}
	for i, c := range cands {
		idx[realKey(c.Path)] = i
	}
	lookup := func(p string) int {
		if i, ok := idx[realKey(p)]; ok {
			return i
		}
		return -1
	}
	hiddenFlags := map[string]bool{}
	for _, rel := range flagsHidden {
		abs := filepath.Clean(filepath.Join(r.Home, filepath.FromSlash(rel)))
		hiddenFlags[realKey(abs)] = true
	}

	aggs := map[string]*readAgg{}
	order := []string{}
	readSeen := map[string]bool{}
	warnSet := map[string]bool{}
	for _, rel := range reads {
		if strings.TrimSpace(rel) == "" {
			continue
		}
		var abs string
		if filepath.IsAbs(rel) {
			abs = filepath.Clean(rel)
		} else {
			abs = filepath.Clean(filepath.Join(r.Home, filepath.FromSlash(rel)))
		}
		if !IsUnder(r.Home, abs) {
			continue
		}
		if readSeen[realKey(abs)] {
			continue
		}
		readSeen[realKey(abs)] = true
		if acBase != "" && isUnderLoose(acBase, abs) {
			continue
		}
		cls, _, why, _ := r.Classify(abs)
		switch cls {
		case ClassKeyring:
			// keyring 是文件级硬排除：单独成 unsupported 行，不归并进 owner，
			// 也不因读证据变成可隔离目录（§7.1）。
			if i := lookup(abs); i >= 0 {
				cands[i].Reads++
				continue
			}
			c := Candidate{
				Path: abs, Class: ClassKeyring, DisplayClass: ClassKeyring,
				Checked: false, Reads: 1, Why: "probe.why.keyring",
			}
			cands = append(cands, c)
			idx[realKey(abs)] = len(cands) - 1
			continue
		case ClassPublic, ClassShared:
			// 公共/共享只生成"不勾选"的行，owner 类别不升级（§7.1）。
			owner, ok := r.ownerOf(abs)
			if !ok {
				continue
			}
			if i := lookup(owner); i >= 0 {
				cands[i].Reads++
				continue
			}
			c := Candidate{
				Path: owner, Class: cls, DisplayClass: cls,
				Checked: false, Reads: 1, Why: why,
			}
			cands = append(cands, c)
			idx[realKey(owner)] = len(cands) - 1
			continue
		}
		strength := credentialNameStrength(filepath.Base(abs))
		if hiddenFlags[realKey(abs)] {
			// openat2 flags 不可见：按读计入，但**不参与凭据升级**（§6.1）。
			strength = CredentialNone
		}
		if cls == ClassCache {
			// owner 是 config、文件本身落在 cache 段下（如
			// `~/.config/App/cache/token.json`）：这是可再生的缓存，不能当
			// 登录态证据写进 credential_paths，否则 fork 会因"lower 含凭据"
			// 拒绝整个 App 目录（§8.4）。
			if strength == CredentialStrong {
				warnSet["cache_credential"] = true
			}
			strength = CredentialNone
		}
		if r.isReadNoise(abs, acBase) && !(strength == CredentialStrong && !r.isSharedCredStore(abs)) {
			continue
		}
		owner, ok := r.ownerOf(abs)
		if !ok {
			continue
		}
		ownerCls, _, _, _ := r.Classify(owner)
		if ownerCls == ClassCache || ownerCls == ClassSystem {
			// cache 读不生成候选；system 只允许既有档案重建（§8.3），
			// 新探测不生成可勾选行（§9.3）。
			if ownerCls == ClassCache && strength == CredentialStrong {
				warnSet["cache_credential"] = true
			}
			continue
		}
		key := realKey(owner)
		a := aggs[key]
		if a == nil {
			a = &readAgg{
				owner:       owner,
				ownerStrong: credentialNameStrength(filepath.Base(owner)) == CredentialStrong,
			}
			aggs[key] = a
			order = append(order, key)
		}
		a.reads++
		switch strength {
		case CredentialStrong:
			a.strong = true
			if a.credPath == "" || len(abs) < len(a.credPath) {
				a.credPath = abs
			}
		case CredentialWeak:
			a.weak = true
		}
	}

	for _, key := range order {
		a := aggs[key]
		if a == nil {
			continue
		}
		if i := lookup(a.owner); i >= 0 {
			c := &cands[i]
			c.Reads += a.reads
			if c.Class == ClassCache {
				continue
			}
			if a.strong {
				c.Checked = true
				c.Warning = false
				if a.ownerStrong {
					c.Credential = CredentialStrong
					c.CredentialPath = a.owner
				} else {
					c.ContainsCredential = true
					c.CredentialPath = a.credPath
				}
			} else if a.weak && c.Credential == CredentialNone {
				c.Credential = CredentialWeak
			}
			c.DisplayClass = c.Class
			continue
		}
		cls, checked, why, warn := r.Classify(a.owner)
		c := Candidate{
			Path: a.owner, Class: cls, DisplayClass: cls,
			Checked: checked, Reads: a.reads, Why: why, Warning: warn,
		}
		if cls == ClassDotdir || cls == ClassUnknown {
			switch {
			case a.strong:
				c.Checked, c.Warning, c.Why = true, false, "probe.why.credential"
			case a.weak:
				c.Checked, c.Warning, c.Why = false, true, "probe.why.read_credential_weak"
			case strings.EqualFold(strings.TrimLeft(filepath.Base(a.owner), "."), app):
				c.Checked, c.Warning, c.Why = false, true, "probe.why.app_home"
			default:
				c.Checked, c.Warning, c.Why = false, true, "probe.why.readonly"
			}
		}
		if a.strong {
			if a.ownerStrong {
				c.Credential, c.CredentialPath = CredentialStrong, a.owner
			} else {
				c.ContainsCredential, c.CredentialPath = true, a.credPath
			}
		} else if a.weak {
			c.Credential = CredentialWeak
		}
		cands = append(cands, c)
		idx[realKey(a.owner)] = len(cands) - 1
	}

	// cache 写行锁定：展示但永远不进入 paths（§7.3、§8.4）。
	for i := range cands {
		if cands[i].Class == ClassCache && cands[i].Observed > 0 {
			cands[i].Locked = true
			cands[i].Checked = false
			cands[i].DisplayClass = ClassCache
		}
	}

	// 只读非凭据行全局最多 20 条；凭据行不占配额，但单独设 100 条上限，
	// 避免应用读几千个 token 文件时确认页被撑爆（§7.3）。
	var kept, credRows, plain []int
	for i := range cands {
		c := cands[i]
		switch {
		case c.Observed > 0 || c.Class == ClassCache || c.Reads == 0,
			c.Class == ClassPublic || c.Class == ClassShared ||
				c.Class == ClassKeyring || c.Class == ClassSystem || c.Locked:
			// 不可勾选行始终展示，不占只读配额。
			kept = append(kept, i)
		case c.Credential != CredentialNone || c.ContainsCredential:
			credRows = append(credRows, i)
		default:
			plain = append(plain, i)
		}
	}
	byReads := func(ids []int) {
		sort.Slice(ids, func(i, j int) bool {
			a, b := cands[ids[i]], cands[ids[j]]
			if a.Reads != b.Reads {
				return a.Reads > b.Reads
			}
			return a.Path < b.Path
		})
	}
	sort.Slice(kept, func(i, j int) bool { return cands[kept[i]].Path < cands[kept[j]].Path })
	byReads(credRows)
	byReads(plain)
	hidden := 0
	if len(plain) > 20 {
		hidden = len(plain) - 20
		plain = plain[:20]
	}
	if len(credRows) > 100 {
		warnSet["credential_rows_truncated"] = true
		credRows = credRows[:100]
	}
	out := make([]Candidate, 0, len(kept)+len(credRows)+len(plain))
	for _, group := range [][]int{kept, credRows, plain} {
		for _, i := range group {
			out = append(out, cands[i])
		}
	}
	sort.Strings(whiteouts)
	var warns []string
	for w := range warnSet {
		warns = append(warns, w)
	}
	return out, whiteouts, hidden, sortCredentialWarns(warns)
}

// realKey 是候选去重键：词法形态优先，存在软链时再取真实形态。
// 只对未命中的路径调用 Real，避免为每条读路径付一次 EvalSymlinks。
func realKey(abs string) string {
	clean := filepath.Clean(abs)
	if real := Real(clean); real != clean {
		return real
	}
	return clean
}

// isReadNoise 是 §7.2 的噪声清单。精确路径或前缀，不做 basename 通配。
func (r *PathResolver) isReadNoise(abs, acBase string) bool {
	home := r.Home
	// 工具自身读写 AC_BASE（实例根/档案目录）：这是探测机制的开销，
	// 不是应用的读取，不能计入"家目录读取"（§7.2）。
	if acBase != "" && isUnderLoose(acBase, abs) {
		return true
	}
	exact := []string{
		filepath.Join(home, ".Xauthority"),
		filepath.Join(home, ".ICEauthority"),
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".bash_history"),
		filepath.Join(home, ".zsh_history"),
		filepath.Join(home, ".config", "user-dirs.dirs"),
		filepath.Join(home, ".config", "mimeapps.list"),
		filepath.Join(r.XDG["DATA_HOME"], "recently-used.xbel"),
	}
	for _, p := range exact {
		if samePathLoose(p, abs) {
			return true
		}
	}
	prefixes := []string{
		filepath.Join(home, ".config", "dconf"),
		filepath.Join(home, ".config", "fontconfig"),
		filepath.Join(r.XDG["CACHE_HOME"], "fontconfig"),
		filepath.Join(home, ".fonts"),
		filepath.Join(r.XDG["DATA_HOME"], "fonts"),
		filepath.Join(r.XDG["DATA_HOME"], "icons"),
		filepath.Join(home, ".themes"),
		filepath.Join(home, ".icons"),
		filepath.Join(home, ".config", "gtk-2.0"),
		filepath.Join(home, ".config", "gtk-3.0"),
		filepath.Join(home, ".config", "gtk-4.0"),
		filepath.Join(r.XDG["DATA_HOME"], "themes"),
		filepath.Join(home, ".config", "autostart"),
		filepath.Join(r.XDG["DATA_HOME"], "applications"),
		filepath.Join(home, ".config", "ibus"),
		filepath.Join(home, ".config", "pulse"),
		filepath.Join(home, ".config", "pipewire"),
		filepath.Join(home, ".config", "gvfs"),
		filepath.Join(r.XDG["DATA_HOME"], "gvfs-metadata"),
		filepath.Join(home, ".pki"),
		filepath.Join(r.XDG["DATA_HOME"], "pki"),
		filepath.Join(r.XDG["DATA_HOME"], "keyrings"),
		filepath.Join(home, ".config", "enchant"),
		filepath.Join(r.XDG["DATA_HOME"], "enchant"),
		filepath.Join(home, ".config", "git"),
		filepath.Join(r.XDG["DATA_HOME"], "Trash"),
		filepath.Join(r.XDG["STATE_HOME"], "Trash"),
		filepath.Join(r.XDG["CACHE_HOME"], "thumbnails"),
		filepath.Join(home, ".thumbnails"),
		filepath.Join(r.XDG["DATA_HOME"], "flatpak"),
	}
	for _, p := range prefixes {
		if isUnderLoose(p, abs) {
			return true
		}
	}
	for _, root := range []string{filepath.Join(home, ".config"), r.XDG["DATA_HOME"]} {
		if _, rel, ok := looseRel(root, abs); ok && rel != "" {
			first, _, _ := strings.Cut(rel, string(filepath.Separator))
			low := strings.ToLower(first)
			if strings.HasPrefix(low, "gnome-") || strings.HasPrefix(low, "fcitx") {
				return true
			}
		}
	}
	if _, rel, ok := looseRel(filepath.Join(home, ".var", "app"), abs); ok && rel != "" {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 2 && strings.EqualFold(parts[1], "cache") {
			return true
		}
	}
	return false
}

// isSharedCredStore 判定共享凭据库：噪声例外不适用于这里（§7.1）。
func (r *PathResolver) isSharedCredStore(abs string) bool {
	for _, p := range []string{
		filepath.Join(r.Home, ".pki"),
		filepath.Join(r.XDG["DATA_HOME"], "pki"),
		filepath.Join(r.XDG["DATA_HOME"], "keyrings"),
	} {
		if isUnderLoose(p, abs) {
			return true
		}
	}
	return strings.HasSuffix(strings.ToLower(filepath.Base(abs)), ".keyring")
}

// credentialUpgradeable 判定候选能否展示/派生凭据证据。Public/Shared/
// Keyring/System 永不升级（ADR-034）；DisplayClass 也要看，因为既有档案里
// 这些类别可能以 ClassConfig 保留。
func credentialUpgradeable(c Candidate) bool {
	switch c.Class {
	case ClassCache, ClassPublic, ClassShared, ClassKeyring, ClassSystem:
		return false
	}
	switch c.DisplayClass {
	case ClassPublic, ClassShared, ClassKeyring, ClassSystem:
		return false
	}
	return true
}

// betterCredentialPath 在多个凭据路径里做确定性选择：更短（更接近 owner）
// 优先，同长度按字典序。map 遍历顺序不能泄漏到界面/档案里。
func betterCredentialPath(cur, next string) string {
	if cur == "" {
		return next
	}
	if len(next) != len(cur) {
		if len(next) < len(cur) {
			return next
		}
		return cur
	}
	if next < cur {
		return next
	}
	return cur
}

// sortCredentialWarns 按固定顺序去重（§7.1/§8.1）。
func sortCredentialWarns(in []string) []string {
	order := []string{
		"profile_conflict", "cache_path_dropped", "cache_credential",
		"credential_scope_unsupported", "credential_scope_narrowed",
		"credential_rows_truncated",
	}
	seen := map[string]bool{}
	for _, w := range in {
		seen[w] = true
	}
	var out []string
	for _, w := range order {
		if seen[w] {
			out = append(out, w)
			delete(seen, w)
		}
	}
	var rest []string
	for w := range seen {
		rest = append(rest, w)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// CandidatesFromProfile 是 §8.3 的"档案 → 屏3 行"：字段路由与展示分离。
// Paths 里每一项 Class 固定 Config（用户显式路径），DisplayClass 只用于 UI；
// cache 行 Locked，public 行默认不勾选。
func (r *PathResolver) CandidatesFromProfile(p Profile) []Candidate {
	cands, _ := r.CandidatesFromProfileWithWarns(p)
	return cands
}

// CandidatesFromProfileWithWarns 额外返回 §8.3 的告警（cache 路径/凭据、
// 同路径多类别冲突），由 DeriveProfile 并入 Profile.Warnings。
func (r *PathResolver) CandidatesFromProfileWithWarns(p Profile) ([]Candidate, []string) {
	warnSet := map[string]bool{}
	type agg struct {
		c       Candidate
		classes map[PathClass]bool
	}
	byKey := map[string]*agg{}
	var order []string
	add := func(c Candidate) {
		abs, err := r.Expand(c.Path)
		if err != nil {
			abs = filepath.Clean(c.Path)
		}
		c.Path = abs
		key := realKey(abs)
		if a, ok := byKey[key]; ok {
			a.classes[c.Class] = true
			switch {
			case c.Class == ClassConfig && a.c.Class != ClassConfig:
				// 用户显式 paths 优先，其余类别只留一个 Warning 徽标。
				a.c.Class, a.c.DisplayClass = ClassConfig, c.DisplayClass
				a.c.Checked, a.c.Why, a.c.Warning = c.Checked, c.Why, true
				a.c.Locked = false
			case c.Class == ClassKeyring && a.c.Class == ClassShared:
				// keyring 优先于 shared：同一路径不能留在 verify 的"预期共享"
				// 白名单里，否则对账口径会把凭证当成正常共享（ADR-034）。
				a.c.Class, a.c.DisplayClass = ClassKeyring, ClassKeyring
				a.c.Checked, a.c.Locked, a.c.Warning = false, false, true
				a.c.Why = c.Why
			case c.Class == ClassShared && a.c.Class == ClassKeyring:
				// 已按 keyring 定案，shared 只贡献 Warning。
				a.c.Warning = true
			default:
				a.c.Warning = true
			}
			if c.Reads > a.c.Reads {
				a.c.Reads = c.Reads
			}
			if c.Observed > a.c.Observed {
				a.c.Observed = c.Observed
			}
			if c.Credential == CredentialStrong {
				a.c.Credential = CredentialStrong
			} else if c.Credential == CredentialWeak && a.c.Credential == CredentialNone {
				a.c.Credential = CredentialWeak
			}
			a.c.ContainsCredential = a.c.ContainsCredential || c.ContainsCredential
			if c.CredentialPath != "" {
				a.c.CredentialPath = c.CredentialPath
			}
			if len(a.classes) > 1 {
				warnSet["profile_conflict"] = true
			}
			return
		}
		a := &agg{c: c, classes: map[PathClass]bool{c.Class: true}}
		byKey[key] = a
		order = append(order, key)
	}

	for _, raw := range p.Paths {
		abs, err := r.Expand(raw)
		if err != nil {
			continue
		}
		cls, _, why, warn := r.Classify(abs)
		c := Candidate{Path: abs, Class: ClassConfig, DisplayClass: cls, Checked: true, Why: why, Warning: warn}
		switch cls {
		case ClassCache:
			c.Class, c.DisplayClass = ClassCache, ClassCache
			c.Checked, c.Locked, c.Warning = false, true, true
			c.Why = "probe.why.cache"
			warnSet["cache_path_dropped"] = true
		case ClassPublic:
			c.Class, c.DisplayClass = ClassConfig, ClassPublic
			c.Checked, c.Warning, c.Why = false, true, "probe.why.public"
		}
		add(c)
	}
	for _, raw := range p.SharedWrites {
		abs, err := r.Expand(raw)
		if err != nil {
			continue
		}
		add(Candidate{
			Path: abs, Class: ClassShared, DisplayClass: ClassShared,
			Checked: false, Why: "probe.why.shared",
		})
	}
	for _, u := range p.Unsupported {
		if u.Kind != "keyring" || strings.TrimSpace(u.Path) == "" {
			continue
		}
		abs, err := r.Expand(u.Path)
		if err != nil {
			continue
		}
		why := u.Reason
		if why == "" {
			why = "probe.why.keyring"
		}
		add(Candidate{
			Path: abs, Class: ClassKeyring, DisplayClass: ClassKeyring,
			Checked: false, Why: why,
		})
	}

	// 凭据展示：候选等于凭据路径或位于其下 → strong；候选是凭据的父路径 →
	// ContainsCredential。真正的集合由 DeriveProfile 第 6 步并集决定（§8.3）。
	// Public/Shared/Keyring/System/cache 不升级、不改 Class（ADR-034）。
	for _, a := range byKey {
		if !credentialUpgradeable(a.c) {
			continue
		}
		var strong, contains string
		for _, raw := range p.CredentialPaths {
			cp, err := r.Expand(raw)
			if err != nil {
				continue
			}
			// 旧档案的凭据可能属于 cache/public/shared/keyring/system：
			// 这些范围一律不参与凭据展示（ADR-034 §8.4）。
			switch cls, _, _, _ := r.Classify(cp); cls {
			case ClassCache:
				warnSet["cache_credential"] = true
				continue
			case ClassPublic, ClassShared, ClassKeyring, ClassSystem:
				continue
			}
			switch {
			case samePathLoose(a.c.Path, cp), isUnderLoose(cp, a.c.Path):
				strong = betterCredentialPath(strong, cp)
			case isUnderLoose(a.c.Path, cp):
				contains = betterCredentialPath(contains, cp)
			}
		}
		if strong != "" {
			a.c.Credential, a.c.CredentialPath = CredentialStrong, strong
		} else if contains != "" {
			a.c.ContainsCredential, a.c.CredentialPath = true, contains
		}
		if a.c.Class == ClassCache {
			a.c.Credential, a.c.ContainsCredential = CredentialNone, false
			warnSet["cache_credential"] = true
		}
	}

	out := make([]Candidate, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key].c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	var warns []string
	for w := range warnSet {
		warns = append(warns, w)
	}
	return out, sortCredentialWarns(warns)
}

// ProfileFromCandidates 把用户确认后的候选集翻译成档案（§5.4/§5.5）。
// 三个落点不许混：
//   - 勾选的 config/data → `paths`（真隔离）
//   - shared → `shared_writes`（不隔离，只用于界面提示与 verify 白名单）
//   - keyring → `unsupported`（**不进 shared_writes**：那是"预期共享"白名单，
//     放进去会让 `doctor --verify` 的对账口径失真）
//
// 未勾选的条目直接丢弃：它们不是"以后再说"，而是"这次不隔离"，写进档案只会
// 让下一次"重新配置"看见一堆用户已经从没勾过的项。
// 未勾选的 config/data/cache 条目直接丢弃（"这次不隔离"），但 shared 与 keyring
// **不看勾选**：前者是"隔离会坏功能"的硬事实，后者是"根本隔离不了"的声明，
// 两者都不该因为用户没勾就消失（§5.4）。
func (r *PathResolver) ProfileFromCandidates(app, method string, cands []Candidate) Profile {
	p := Profile{
		App:         app,
		Version:     ProfileVersion,
		ProbeMethod: method,
		Ignored:     DefaultIgnored(),
		Env:         map[string]string{},
	}
	seen := map[string]bool{}
	put := func(dst *[]string, path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		*dst = append(*dst, path)
	}
	for _, c := range cands {
		// cache 永不进入 paths：即使调用方误把 Locked 候选标成 Checked，
		// 也不能生成"保存了但门禁永远禁用"的死路（方案 §8.4）。
		if c.Locked || c.Class == ClassCache {
			continue
		}
		store := r.ToStoreForm(c.Path)
		switch c.Class {
		case ClassShared:
			put(&p.SharedWrites, store)
		case ClassKeyring:
			p.Unsupported = append(p.Unsupported, Unsupported{
				Kind: "keyring", Path: store, Reason: "probe.why.keyring",
			})
		default:
			if !c.Checked {
				continue
			}
			put(&p.Paths, store)
		}
	}
	// 同一路径的落点优先级：paths > keyring > shared_writes。keyring 不能留在
	// shared_writes（verify 会把它当"预期共享"），显式 paths 也不能同时被声明
	// 为 unsupported。
	keyring := map[string]bool{}
	for _, u := range p.Unsupported {
		if abs, err := r.Expand(u.Path); err == nil {
			keyring[realKey(abs)] = true
		}
	}
	if len(keyring) > 0 {
		kept := p.SharedWrites[:0]
		for _, s := range p.SharedWrites {
			abs, err := r.Expand(s)
			if err != nil || !keyring[realKey(abs)] {
				kept = append(kept, s)
			}
		}
		p.SharedWrites = kept
	}
	if len(p.Paths) > 0 {
		explicit := map[string]bool{}
		for _, s := range p.Paths {
			if abs, err := r.Expand(s); err == nil {
				explicit[realKey(abs)] = true
			}
		}
		kept := p.Unsupported[:0]
		for _, u := range p.Unsupported {
			abs, err := r.Expand(u.Path)
			if err != nil || !explicit[realKey(abs)] {
				kept = append(kept, u)
			}
		}
		p.Unsupported = kept
	}
	sort.Strings(p.Paths)
	sort.Strings(p.SharedWrites)
	return p
}
