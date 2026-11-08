package core

// ProbeFinalize 是"探测结果落成档案"时需要的非路径基线。
//
// 优先级与 §5.5 一致：既有档案 > 内置模板。两者都可能为空（首次配置且无模板）。
type ProbeFinalize struct {
	Bundled    Profile
	HasBundled bool
	Existing   Profile
	// HasExisting 为真时既有档案优先：它记录的是用户上次确认过的行为配置。
	HasExisting bool
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
	p := res.Profile
	cands := res.Candidates
	prefilled := 0
	if opt.HasBundled {
		extra := e.Resolver.PrefillCandidates(opt.Bundled, cands)
		cands = append(cands, extra...)
		prefilled = len(extra)
	}

	method := p.ProbeMethod
	if prefilled > 0 && len(p.Paths) == 0 {
		method = "manual"
	}
	// 判定口径与确认按钮的门禁同一份：derived 就是"勾选后真正会进档案的东西"。
	derived := e.Resolver.ProfileFromCandidates(app, method, cands)
	if len(derived.Paths) == 0 {
		return Profile{}, cands, prefilled, NewProbeNoWrites(app, len(cands))
	}
	if prefilled > 0 {
		// 预填改变了路径集合：以候选集的派生结果为准（探测只给了没有预填前的路径）。
		p.Paths = derived.Paths
		p.SharedWrites = derived.SharedWrites
		p.ProbeMethod = method
	}

	base, hasBase := opt.Bundled, opt.HasBundled
	if opt.HasExisting {
		base, hasBase = opt.Existing, true
	}
	if hasBase {
		p = MergeNonPathFieldsWith(e.Resolver, p, base)
	}
	return p, cands, prefilled, nil
}
