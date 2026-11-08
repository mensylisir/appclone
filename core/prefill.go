package core

// PrefillCandidates 把内置模板里的 paths 作为预填候选补进探测结果（§4：
// 「内置模板仅作首次生成的预填来源」）。分类仍走 Classify 这一张规则表：
// 模板里落在公共目录/共享可写/凭证上的路径会被分类接走，不会因为"模板写了"
// 就强行隔离。
//
// 只补探测没提到的绝对路径；返回新增的候选，调用方负责把它们并进候选集。
// 公共目录与凭证不在这里补：前者有独立的只读分组，后者要保留 kind/reason，
// 由档案里的 unsupported 原样继承，绕一圈候选反而会丢信息。
func (r *PathResolver) PrefillCandidates(tmpl Profile, existing []Candidate) []Candidate {
	seen := make(map[string]bool, len(existing))
	for _, c := range existing {
		seen[c.Path] = true
	}
	var out []Candidate
	for _, raw := range tmpl.Paths {
		abs, err := r.Expand(raw)
		if err != nil || seen[abs] {
			continue
		}
		class, checked, why, warn := r.Classify(abs)
		if class == ClassPublic || class == ClassKeyring {
			continue
		}
		out = append(out, Candidate{Path: abs, Class: class, Checked: checked, Why: why, Warning: warn})
		seen[abs] = true
	}
	return out
}
