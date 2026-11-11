package core

import (
	"path/filepath"
	"slices"
	"sort"
)

// ProbeFinalize 是"探测结果落成档案"时需要的非路径基线。
//
// 优先级与 §5.5 一致：既有档案 > 内置模板。两者都可能为空（首次配置且无模板）。
type ProbeFinalize struct {
	Bundled    Profile
	HasBundled bool
	Existing   Profile
	// HasExisting 为真时既有档案优先：它记录的是用户上次确认过的行为配置。
	HasExisting bool
	// AllowEmpty 只由 GUI 首次配置的降级确认页设置：读证据不可用时允许
	// 先进入确认页，由用户勾选至少一条；CLI 永不设置（§11.4）。
	AllowEmpty bool
}

// FinalizeProbe 把一次探测结果落成可确认 / 可保存的档案。CLI 的 `add` 与 GUI
// 的确认页必须走同一个函数——两端各写一套，同一个应用迟早落出两份语义不同的
// 档案（§6.1 要求两个视图对同一探测结果给出一致结论）。
//
// 规则（每条都对应一条契约）：
//   - 内置模板只作**预填**（§4、§6）：模板路径补进候选，分类仍走 Classify；
//     模板里落在公共目录/凭证上的路径不会因为"模板写了"就被强行隔离。
//   - 本次一条路径都没跟踪到、结果全靠模板预填时，来源标 manual 而不是
//     overlay——这是一份模板档案，不是本机探测结果（§6.1 文案红线）。
//   - 零写入一律报 err.probe.no_writes，且判定看**会进档案的路径数**而不是
//     候选数：只有缓存/共享/凭证写入时不该生成一份空档案（§6.3）。
//   - 非路径字段的合并只经 MergeNonPathFieldsWith：env / extra_binds /
//     isolate_* / sockets / cwd / audit / ignored / 未知字段是显式配置，
//     探测产物里没有它们，不能顺手抹掉（§5.5）。
//
// 返回值里的 candidates 是合并预填后的候选集（调用方渲染确认页 / 计数要用），
// prefilled 是其中模板补进来的条数（0 = 没有模板或没有新增）。
//
// 前提：res.Profile 必须由同一份 res.Candidates 派生（core.Probe 保证这个
// 不变量）。调用方手工拼 ProbeResult 时两者若不一致，"零写入判定"与返回档案
// 会各说各话。零写入时返回的 Profile 是零值，调用方在 err != nil 时不得使用它。
func (e *Env) FinalizeProbe(app string, res ProbeResult, opt ProbeFinalize) (Profile, []Candidate, int, error) {
	cands := res.Candidates
	prefilled := 0
	if opt.HasBundled {
		extra := e.Resolver.PrefillCandidates(opt.Bundled, cands)
		cands = append(cands, extra...)
		prefilled = len(extra)
	}

	probe := res.Profile
	if probe.App == "" {
		probe.App = app
	}
	method := probe.ProbeMethod
	if prefilled > 0 && len(probe.Paths) == 0 {
		method = "manual"
	}
	var base *Profile
	switch {
	case opt.HasExisting:
		base = &opt.Existing
	case opt.HasBundled:
		base = &opt.Bundled
	}
	p := e.Resolver.DeriveProfile(probe, base, method, res.ReadTrace, cands)
	if len(p.Paths) == 0 {
		// AllowEmpty 是 GUI 首次配置在读证据不可用时的降级确认页：只有确实
		// 有"能进 paths"的行可勾选时才允许进入，否则页面是空的，用户会被
		// 卡死在一个无路可走的确认页上（§11.4）。
		if opt.AllowEmpty && res.ReadTrace == ReadTraceUnavailable && hasGateCandidate(cands) {
			return p, cands, prefilled, nil
		}
		return Profile{}, cands, prefilled, NewProbeNoWrites(app, len(cands))
	}
	return p, cands, prefilled, nil
}

// hasGateCandidate 报告候选里是否至少有一条能落进 paths 的行。
// cache/public/shared/keyring/system 都不可勾选，不能支撑降级确认页。
func hasGateCandidate(cands []Candidate) bool {
	for _, c := range cands {
		if candidateCanGate(c) {
			return true
		}
	}
	return false
}

// candidateCanGate 是"这条候选能否成为应用自身数据"的唯一判据。
// DisplayClass 必须一起看：既有档案里的公共路径 Class 是 Config、
// DisplayClass 是 Public，只看 Class 会把它当成可隔离项（§9.3）。
func candidateCanGate(c Candidate) bool {
	if c.Locked || c.DisplayClass == ClassPublic {
		return false
	}
	switch c.Class {
	case ClassCache, ClassPublic, ClassShared, ClassKeyring, ClassSystem:
		return false
	}
	return true
}

// CountsTowardGate 报告一条候选是否计入确认门禁：界面上的勾选状态为真，
// 且该行能进 paths。CLI/GUI/设计稿必须共用这一条判据，否则"公共路径点亮
// 确认按钮"的分叉会在某个视图里重新出现。
func CountsTowardGate(c Candidate, checked bool) bool {
	return checked && candidateCanGate(c)
}

// DeriveProfile 是"探测产物 + 非路径 base"落成档案的唯一入口（§8.1）。
//
// probe 是本次探测产物（新档案时由 Probe 预置，重探时是 res.Profile）；
// base == nil 表示没有既有档案/模板。**不能**用一个零值 Profile 代替 nil：
// 零值会让 MergeNonPathFieldsWith 无条件覆盖 IsolateDBus/Sockets/Unknown，
// 也会把 Ignored 的 nil 语义打回默认表。
func (r *PathResolver) DeriveProfile(probe Profile, base *Profile, method, readTrace string, cands []Candidate) Profile {
	effective := probe
	if base != nil {
		effective = MergeNonPathFieldsWith(r, probe, *base)
		// Ignored 三态：nil（没声明）/ 空表（显式"不忽略"）/ 表。合并函数在
		// base.Ignored==nil 时会保留 probe 的默认表，这里必须无条件以 base 为准。
		effective.Ignored = slices.Clone(base.Ignored)
		if effective.App == "" {
			effective.App = base.App
		}
	}

	p := r.ProfileFromCandidates(effective.App, method, cands)
	p = MergeNonPathFieldsWith(r, p, effective)
	p.Ignored = slices.Clone(effective.Ignored)
	p.ReadTrace = readTrace

	// 凭据继承与派生（§8.1 第 6 步，重探/取消勾选都走这一份并集）：
	// 旧凭据适配到新 paths，再并入本次 checked 且有强证据的候选。
	old := append([]string{}, effective.CredentialPaths...)
	if base != nil {
		old = append(old, base.CredentialPaths...)
	}
	adapted, dropped, narrowed := r.adaptCredentialPaths(old, p.Paths)
	p.CredentialDropped = append([]string{}, dropped...)
	// 本次探测的 merge 告警（cache 凭据、凭据行超限）是 run-scoped 结论，
	// 必须随档案一起交给展示层；只继承 probe，不继承 base——base 里的旧
	// 告警早已被重新探测取代，复制它会让过期提示复活。
	warns := append([]string{}, effective.Warnings...)
	if narrowed {
		warns = append(warns, "credential_scope_narrowed")
	}
	for _, d := range dropped {
		abs, err := r.expandLexical(d)
		if err != nil {
			warns = append(warns, "credential_scope_unsupported")
			continue
		}
		switch cls, _, _, _ := r.Classify(abs); cls {
		case ClassCache:
			warns = append(warns, "cache_credential")
		case ClassPublic, ClassShared, ClassKeyring, ClassSystem:
			// 公共/共享/keyring/system 本就不接受凭据范围：告诉用户"不支持"，
			// 而不是"范围收窄"（§8.4）。
			warns = append(warns, "credential_scope_unsupported")
		default:
			warns = append(warns, "credential_scope_narrowed")
		}
	}
	derived := r.credentialPaths(cands, p.Paths)
	all := append(adapted, derived...)

	// 最终不变量：credential_paths ⊆ paths（相等或位于其下）。
	for _, cp := range all {
		if r.credentialWithinPaths(cp, p.Paths) {
			p.CredentialPaths = append(p.CredentialPaths, cp)
		} else {
			warns = append(warns, "credential_scope_narrowed")
		}
	}
	p.CredentialPaths = r.normalizeCredentialPaths(p.CredentialPaths)
	p.Warnings = append(p.Warnings, sortCredentialWarns(warns)...)
	return p
}

// adaptCredentialPaths 把旧档案的 credential_paths 适配到新的 paths：
//   - 旧路径等于某个新 path 或位于其下 → 保留；
//   - 旧路径是父、新 paths 只有其子项 → 收窄为这些子项；
//   - 旧路径是 cache → 剔除（cache 凭据不受支持，§8.4）；
//   - 其它（已从 paths 移除）→ 丢弃。
func (r *PathResolver) adaptCredentialPaths(old, paths []string) ([]string, []string, bool) {
	var dropped []string
	pathAbs := make([]string, 0, len(paths))
	for _, raw := range paths {
		abs, err := r.expandLexical(raw)
		if err != nil {
			abs, err = r.Expand(raw)
		}
		if err == nil {
			pathAbs = append(pathAbs, abs)
		}
	}
	var out []string
	narrowed := false
	for _, raw := range old {
		abs, err := r.expandLexical(raw)
		if err != nil {
			abs, err = r.Expand(raw)
		}
		if err != nil {
			dropped = append(dropped, raw)
			continue
		}
		if !credentialClassAllowed(r, abs) {
			// cache/public/shared/keyring/system 的旧凭据一律剔除（ADR-034）。
			dropped = append(dropped, raw)
			continue
		}
		kept := false
		for _, pa := range pathAbs {
			if isUnderLoose(pa, abs) {
				out = append(out, r.ToStoreForm(abs))
				kept = true
				break
			}
			if isUnderLoose(abs, pa) && !samePathLoose(abs, pa) {
				// 旧父路径只包含新子路径：把父凭据收窄为子项，绝不留下
				// 不在 paths 下的父路径（§10.1）。
				if credentialClassAllowed(r, pa) {
					out = append(out, r.ToStoreForm(pa))
					kept, narrowed = true, true
				}
			}
		}
		if !kept {
			dropped = append(dropped, raw)
		}
	}
	if narrowed {
		// 收窄不算"丢弃"，但要让界面知道范围变了。
		return out, dropped, true
	}
	return out, dropped, false
}

// credentialClassAllowed 按 Classify 过滤旧凭据范围；DisplayClass 不参与。
func credentialClassAllowed(r *PathResolver, abs string) bool {
	switch cls, _, _, _ := r.Classify(abs); cls {
	case ClassCache, ClassPublic, ClassShared, ClassKeyring, ClassSystem:
		return false
	}
	return true
}

// credentialPaths 从候选派生凭据集合：仅 checked、非 Locked、有强证据。
// 弱证据只用于展示，永不进入 credential_paths（§7.3）。
func (r *PathResolver) credentialPaths(cands []Candidate, paths []string) []string {
	var out []string
	for _, c := range cands {
		if !c.Checked || c.Locked || c.Class == ClassCache {
			continue
		}
		if c.Credential != CredentialStrong && c.CredentialPath == "" {
			continue
		}
		cp := c.CredentialPath
		if cp == "" {
			cp = c.Path
		}
		if !credentialClassAllowed(r, cp) {
			// cache/public/shared/keyring/system 的路径不能进 credential_paths，
			// 即使候选来自旧档案或静态线索（§8.4）。
			continue
		}
		if r.credentialWithinPaths(cp, paths) {
			abs, err := r.expandLexical(cp)
			if err != nil {
				abs, err = r.Expand(cp)
			}
			if err == nil {
				out = append(out, r.ToStoreForm(abs))
			}
		}
	}
	return out
}

// credentialWithinPaths 判定凭据路径是否等于某个 path 或位于其下。
// **词法优先**（§8.4）：软链凭据只要词法在 paths 内就接受；词法不命中再
// 用真实形态兜底。
func (r *PathResolver) credentialWithinPaths(cp string, paths []string) bool {
	if lex, err := r.expandLexical(cp); err == nil {
		for _, raw := range paths {
			if p, err := r.expandLexical(raw); err == nil {
				if _, ok := relUnder(p, lex); ok {
					return true
				}
			}
		}
	}
	cabs, err := r.Expand(cp)
	if err != nil {
		return false
	}
	for _, raw := range paths {
		pabs, err := r.Expand(raw)
		if err != nil {
			continue
		}
		if isUnderLoose(pabs, cabs) {
			return true
		}
	}
	return false
}

// normalizeCredentialPaths 规范化、去重、排序；输出仍是 store 形态。
// 词法形态优先保留：软链凭据不能因为 Real 指向别处而被改写成宿主目标。
func (r *PathResolver) normalizeCredentialPaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		abs, err := r.expandLexical(raw)
		if err != nil {
			abs, err = r.Expand(raw)
		}
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		key := realKey(abs)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r.ToStoreForm(abs))
	}
	sort.Strings(out)
	return out
}
