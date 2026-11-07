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
		if abs == root {
			return "", false // 纯父目录：不是候选
		}
		if rel, ok := relUnder(root, abs); ok {
			first, _, _ := strings.Cut(rel, string(filepath.Separator))
			owner := filepath.Join(root, first)
			if owner == r.Home {
				return "", false
			}
			return owner, true
		}
	}
	if rel, ok := relUnder(r.Home, abs); ok && rel != "" {
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		owner := filepath.Join(r.Home, first)
		// 祖先：`~/.local` 是 DATA_HOME/STATE_HOME 的祖先，`~/.var` 是 Flatpak 根的祖先。
		for _, root := range roots {
			if IsUnder(owner, root) {
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

// ProfileFromCandidates 把用户确认后的候选集翻译成档案（§5.4/§5.5）。
// 三个落点不许混：
//   - 勾选的 config/data → `paths`（真隔离）
//   - shared → `shared_writes`（不隔离，只用于界面提示与 verify 白名单）
//   - keyring → `unsupported`（**不进 shared_writes**：那是"预期共享"白名单，
//     放进去会让 `doctor --verify` 的对账口径失真）
//
// 未勾选的条目直接丢弃：它们不是"以后再说"，而是"这次不隔离"，写进档案只会
// 让下一次"重新配置"看见一堆用户已经从没勾过的项。
func (r *PathResolver) ProfileFromCandidates(app, method string, cands []Candidate) Profile {
	p := Profile{
		App:         app,
		Version:     ProfileVersion,
		ProbeMethod: method,
		Ignored:     DefaultIgnored(),
		Env:         map[string]string{},
	}
	for _, c := range cands {
		switch c.Class {
		case ClassShared:
			p.SharedWrites = append(p.SharedWrites, r.ToStoreForm(c.Path))
		case ClassKeyring:
			p.Unsupported = append(p.Unsupported, Unsupported{
				Kind: "keyring", Path: r.ToStoreForm(c.Path),
			})
		default:
			if !c.Checked {
				continue
			}
			p.Paths = append(p.Paths, r.ToStoreForm(c.Path))
		}
	}
	sort.Strings(p.Paths)
	sort.Strings(p.SharedWrites)
	return p
}
