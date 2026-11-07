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
	counts := map[string]int{}
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
		if _, seen := counts[owner]; !seen {
			order = append(order, owner)
		}
		counts[owner]++
	}

	// 输出必须稳定：同一份 upper 两次归并结果不同，GUI 的分组会跳、
	// golden 断言也无从写起（§11 的同一纪律）。
	sort.Strings(order)
	cands := make([]Candidate, 0, len(order))
	for _, p := range order {
		class, checked, why, warn := r.Classify(p)
		cands = append(cands, Candidate{
			Path: p, Class: class, Checked: checked,
			Observed: counts[p], Why: why, Warning: warn,
		})
	}
	sort.Strings(whiteouts)
	return cands, whiteouts
}

// ownerOf 把一条具体条目归到"它属于哪个应用目录"。
//
// 规则是"XDG 根下取第一级，$HOME 下取第一级"，因为应用把自己的状态放在
// `<XDG 根>/<应用名>/…` 是绝大多数情况；更深的层级是实现细节，不该拆成
// 多个勾选项让用户逐个判断（复杂度不泄漏给用户）。
func (r *PathResolver) ownerOf(abs string) (string, bool) {
	for _, root := range []string{
		r.XDG["CONFIG_HOME"], r.XDG["DATA_HOME"], r.XDG["STATE_HOME"], r.XDG["CACHE_HOME"],
		filepath.Join(r.Home, ".var", "app"),
	} {
		if abs == root {
			return "", false // 纯父目录：不是候选
		}
		if rel, ok := relUnder(root, abs); ok {
			first, _, _ := strings.Cut(rel, string(filepath.Separator))
			return filepath.Join(root, first), true
		}
	}
	if rel, ok := relUnder(r.Home, abs); ok && rel != "" {
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		return filepath.Join(r.Home, first), true
	}
	return "", false // $HOME 本身或其祖先：丢弃
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
