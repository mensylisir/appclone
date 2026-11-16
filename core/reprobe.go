package core

import "strings"

// DroppedIsolatedPaths 报告旧隔离范围里、新确认页已不再覆盖的路径。
//
// 用途只在「旧档案就地重新探测」这一个入口：重探会按本次候选重新定案 paths，
// 旧档案里的路径可能整条消失（本次没有观察到），也可能按公共/缓存规则改成
// 共享。产品不允许静默收窄隔离范围，所以差异必须逐条交给用户看。
//
// 覆盖判据只有一条：**新确认页上已勾选且能落进 paths 的候选**（checked &&
// CountsTowardGate）。未勾选、公共目录、缓存、keyring 候选都不算覆盖——否则
// 文案承诺「确认后不再隔离」，实际落盘却还会隔离（或反过来说了不算数）。
//
// 路径比较词法优先、真实路径兜底：候选等于旧路径或为其父目录即覆盖
// （app 与 app2 这类相邻目录不会误判）。无法展开的旧路径保守算 dropped：
// 宁可让用户多看一条，也不能把可能失去的隔离悄悄咽下。
func DroppedIsolatedPaths(r *PathResolver, old []string, cands []Candidate) []string {
	if r == nil || len(old) == 0 {
		return nil
	}
	var covers []string
	for _, c := range cands {
		if !CountsTowardGate(c, c.Checked) {
			continue
		}
		covers = append(covers, c.Path)
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range old {
		if strings.TrimSpace(o) == "" {
			continue
		}
		key := reprobePathKey(r, o)
		if seen[key] {
			continue
		}
		seen[key] = true
		if pathStillCovered(r, covers, o) {
			continue
		}
		out = append(out, o)
	}
	return out
}

// reprobePathKey 是去重键：先按词法展开成绝对路径，再按真实路径归一。
//
// 只按词法去重会漏掉软链 HOME（Silverblue 的 /var/home/u → /home/u）与软链
// `~/.config`：旧档案存 `~/...`，候选路径由探测管线解析成真实路径，同一个目录
// 会以两种写法进基线，范围变化告警于是把一条路径报两遍、count 也对不上。
// 输出仍保留调用方给的写法（档案形态），这个键只用于判重。
func reprobePathKey(r *PathResolver, p string) string {
	if abs, err := r.expandLexical(p); err == nil {
		return Real(abs)
	}
	return p
}

// pathStillCovered 判断新的隔离候选里是否还有一条等于 old 或是它的父目录。
func pathStillCovered(r *PathResolver, covers []string, old string) bool {
	oldLex, lexErr := r.expandLexical(old)
	if lexErr != nil {
		return false
	}
	for _, c := range covers {
		cLex, cerr := r.expandLexical(c)
		if cerr == nil && isUnderLoose(cLex, oldLex) {
			return true
		}
	}
	// 词法不命中再按真实路径比：软链 HOME 下词法与真实目录可能不同名。
	oldReal := Real(oldLex)
	for _, c := range covers {
		cLex, cerr := r.expandLexical(c)
		if cerr != nil {
			continue
		}
		if isUnderLoose(Real(cLex), oldReal) {
			return true
		}
	}
	return false
}

// BaselineIsolatedPaths 汇总「重探前确认页的旧隔离范围」，供 DroppedIsolatedPaths
// 做差集。旧档案的 paths 是决定性的：公共/缓存路径即使仍是候选也一定来自这里，
// 只按 checked 候选取基线会漏掉它们。
//
// checked 且能进门禁的候选一并并进来，防止档案 paths 缺失（旧档案损坏但已加载
// 内存）时把当前页刚勾选的路径误报成 dropped。
func BaselineIsolatedPaths(r *PathResolver, oldPaths []string, cands []Candidate) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(oldPaths)+len(cands))
	seen := map[string]bool{}
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		key := reprobePathKey(r, p)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, p)
	}
	for _, p := range oldPaths {
		add(p)
	}
	for _, c := range cands {
		if CountsTowardGate(c, c.Checked) {
			add(c.Path)
		}
	}
	return out
}
